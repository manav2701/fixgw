package venue

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/manav2701/fixgw/internal/oms"
)

// fakeVenue is a scriptable exchange.
type fakeVenue struct {
	mu        sync.Mutex
	placeErr  error
	cancelErr error
	state     State
	placed    []Request
	cancels   int
}

func (f *fakeVenue) Name() string { return "fake" }
func (f *fakeVenue) Place(_ context.Context, r Request) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.placeErr != nil {
		return State{}, f.placeErr
	}
	f.placed = append(f.placed, r)
	f.state.VenueID = "V1"
	return f.state, nil
}
func (f *fakeVenue) Cancel(context.Context, Ref) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	if f.cancelErr == nil {
		f.state.Status = StatusCanceled
	}
	return f.cancelErr
}
func (f *fakeVenue) Query(context.Context, Ref) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}
func (f *fakeVenue) set(s State) {
	f.mu.Lock()
	f.state = s
	f.mu.Unlock()
}

type sink struct {
	mu     sync.Mutex
	execs  []oms.Execution
	rejs   []oms.CancelReject
	events []map[string]any
}

func (s *sink) emit(e []oms.Execution) {
	s.mu.Lock()
	s.execs = append(s.execs, e...)
	s.mu.Unlock()
}
func (s *sink) rej(r oms.CancelReject) {
	s.mu.Lock()
	s.rejs = append(s.rejs, r)
	s.mu.Unlock()
}
func (s *sink) event(_ string, d any) {
	s.mu.Lock()
	s.events = append(s.events, d.(map[string]any))
	s.mu.Unlock()
}
func (s *sink) execTypes() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b []byte
	for _, e := range s.execs {
		b = append(b, byte(e.ExecType))
	}
	return string(b)
}
func (s *sink) hasEvent(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e["event"] == name {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type rig struct {
	t    *testing.T
	reg  *oms.Registry
	fv   *fakeVenue
	r    *Router
	out  *sink
	ins  oms.Instrument
	next int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ins := oms.DefaultInstruments()["BTC/USD"]
	fv := &fakeVenue{}
	reg := oms.NewRegistry()
	out := &sink{}
	r := NewRouter(Config{
		Registry:    reg,
		Instruments: oms.Instruments{"BTC/USD": ins},
		Bindings:    []Binding{{Venue: fv, Spec: Spec{Symbol: "BTC/USD", VenueSymbol: "BTCUSDT", Tick: 100, Step: 100, MinQty: 100}}},
		Emit:        out.emit, EmitCancelReject: out.rej, Events: out.event,
		PollInterval: time.Hour, // tests drive Reconcile by hand
	})
	t.Cleanup(r.Stop)
	return &rig{t: t, reg: reg, fv: fv, r: r, out: out, ins: ins}
}

// submit accepts an order the way the engine does, then hands it to the router.
func (g *rig) submit(side oms.Side, qty oms.Qty, px oms.Price) oms.Order {
	g.t.Helper()
	g.next++
	o := oms.Order{Session: "S", Counterparty: "C", ClOrdID: "c" + string(rune('0'+g.next)), Symbol: "BTC/USD",
		Side: side, Type: oms.Limit, TIF: oms.GTC, Qty: qty, Price: px}
	ack, err := g.reg.Accept(o)
	if err != nil {
		g.t.Fatal(err)
	}
	g.out.emit([]oms.Execution{ack})
	acc, _ := g.reg.ByID(ack.OrderID)
	g.r.Submit(acc)
	return acc
}

func TestSpecCheck(t *testing.T) {
	ins := oms.DefaultInstruments()["BTC/USD"]
	sp := Spec{Symbol: "BTC/USD", Tick: 100, Step: 1000, MinQty: 1000}
	ok := oms.Order{Type: oms.Limit, Price: 6_500_000, Qty: 2000}
	if err := sp.Check(ok, ins); err != nil {
		t.Fatalf("conforming order rejected: %v", err)
	}
	for name, o := range map[string]oms.Order{
		"off venue tick": {Type: oms.Limit, Price: 6_500_050, Qty: 2000},
		"off venue step": {Type: oms.Limit, Price: 6_500_000, Qty: 2500},
		"below minimum":  {Type: oms.Limit, Price: 6_500_000, Qty: 0},
	} {
		if err := sp.Check(o, ins); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	if err := sp.Check(oms.Order{Type: oms.Market, Qty: 2000}, ins); err != nil {
		t.Errorf("market order has no price to check: %v", err)
	}
}

func TestRouterPlaceAndFills(t *testing.T) {
	g := newRig(t)
	g.fv.set(State{Status: StatusOpen})
	o := g.submit(oms.Buy, 10_000, 6_500_000) // 0.01 BTC @ 65000.00

	eventually(t, "placement", func() bool { return g.out.hasEvent("placed") })
	if len(g.fv.placed) != 1 || g.fv.placed[0].ClientID == "" || g.fv.placed[0].Qty != 10_000 {
		t.Fatalf("venue request wrong: %+v", g.fv.placed)
	}

	// 0.004 filled at 65000.
	g.fv.set(State{VenueID: "V1", Status: StatusPartial, CumQty: 4000, AvgPx: 6_500_000})
	g.r.Reconcile()
	// Remaining 0.006 filled at 65100: the increment must be priced at 65100,
	// not at the 65060 average.
	g.fv.set(State{VenueID: "V1", Status: StatusFilled, CumQty: 10_000, AvgPx: 6_506_000})
	g.r.Reconcile()

	if got := g.out.execTypes(); got != "0FF" {
		t.Fatalf("exec types = %q, want 0FF", got)
	}
	f1, f2 := g.out.execs[1], g.out.execs[2]
	if f1.LastQty != 4000 || f1.LastPx != 6_500_000 || f1.OrdStatus != oms.StatusPartiallyFilled {
		t.Errorf("first fill = %+v", f1)
	}
	if f2.LastQty != 6000 || f2.LastPx != 6_510_000 || f2.OrdStatus != oms.StatusFilled {
		t.Errorf("second fill = %+v, want 0.006 @ 65100.00", f2)
	}
	cur, _ := g.reg.ByID(o.OrderID)
	if cur.Status != oms.StatusFilled || cur.AvgPx != 6_506_000 {
		t.Errorf("order = status %c avg %d", cur.Status, cur.AvgPx)
	}

	// Reconciling again changes nothing.
	g.r.Reconcile()
	if got := g.out.execTypes(); got != "0FF" {
		t.Errorf("repeat reconcile emitted more: %q", got)
	}
}

func TestRouterVenueRejectsPlacement(t *testing.T) {
	g := newRig(t)
	g.fv.placeErr = &Error{Code: -2010, Msg: "insufficient balance"}
	o := g.submit(oms.Buy, 10_000, 6_500_000)

	eventually(t, "close", func() bool { return g.out.execTypes() == "04" })
	cur, _ := g.reg.ByID(o.OrderID)
	if cur.Status != oms.StatusCanceled {
		t.Fatalf("status = %c", cur.Status)
	}
	if txt := g.out.execs[1].Text; txt == "" {
		t.Error("cancel should carry the venue's reason")
	}
}

func TestRouterClientCancel(t *testing.T) {
	g := newRig(t)
	g.fv.set(State{Status: StatusOpen})
	o := g.submit(oms.Buy, 10_000, 6_500_000)
	eventually(t, "placement", func() bool { return g.out.hasEvent("placed") })

	if !g.r.Owns("S", o.ClOrdID) {
		t.Fatal("router should own the routed order")
	}
	g.r.Cancel("S", o.ClOrdID, "x1")
	eventually(t, "cancel exec", func() bool { return g.out.execTypes() == "04" })
	if g.fv.cancels != 1 {
		t.Errorf("venue cancels = %d", g.fv.cancels)
	}
	if e := g.out.execs[1]; e.OrigClOrdID != o.ClOrdID || e.ClOrdID != "x1" {
		t.Errorf("cancel report not chained: %+v", e)
	}
}

func TestRouterCancelRefusedByVenue(t *testing.T) {
	g := newRig(t)
	g.fv.set(State{Status: StatusOpen})
	o := g.submit(oms.Buy, 10_000, 6_500_000)
	eventually(t, "placement", func() bool { return g.out.hasEvent("placed") })

	// The order filled at the venue just before our cancel arrived.
	g.fv.cancelErr = &Error{Code: -2011, Msg: "Unknown order sent."}
	g.fv.set(State{VenueID: "V1", Status: StatusFilled, CumQty: 10_000, AvgPx: 6_500_000})
	g.r.Cancel("S", o.ClOrdID, "x1")

	eventually(t, "cancel reject", func() bool {
		g.out.mu.Lock()
		defer g.out.mu.Unlock()
		return len(g.out.rejs) == 1
	})
	rej := g.out.rejs[0]
	if rej.Reason != oms.CxlTooLate || rej.OrdStatus != oms.StatusFilled {
		t.Errorf("reject = %+v, want too-late on a filled order", rej)
	}
	if got := g.out.execTypes(); got != "0F" {
		t.Errorf("the fill should still be reported: %q", got)
	}
}

func TestRouterReplaceIsRefused(t *testing.T) {
	g := newRig(t)
	g.fv.set(State{Status: StatusOpen})
	o := g.submit(oms.Buy, 10_000, 6_500_000)
	rj := g.r.RejectReplace("S", o.ClOrdID, "r1")
	if rj.ResponseTo != '2' || rj.Reason != oms.CxlOther {
		t.Errorf("reject = %+v", rj)
	}
	if !g.reg.Seen("S", "r1") {
		t.Error("the replace's ClOrdID should be consumed")
	}
}

func TestRouterBreakOnFillAfterLocalClose(t *testing.T) {
	g := newRig(t)
	g.fv.set(State{Status: StatusOpen})
	o := g.submit(oms.Buy, 10_000, 6_500_000)
	eventually(t, "placement", func() bool { return g.out.hasEvent("placed") })

	// A kill switch closes the order locally; the venue cancel fails and the
	// order fills before anyone notices.
	ex, err := g.reg.Close(o.OrderID, "kill switch")
	if err != nil {
		t.Fatal(err)
	}
	g.out.emit([]oms.Execution{ex})
	g.fv.set(State{VenueID: "V1", Status: StatusFilled, CumQty: 10_000, AvgPx: 6_500_000})
	g.r.Reconcile()

	if !g.out.hasEvent("break") {
		t.Fatal("expected a reconciliation break event")
	}
	if got := g.out.execTypes(); got != "04" {
		t.Errorf("no trade may be reported on a closed order: %q", got)
	}
}

func TestRouterCancelAtVenueAfterLocalClose(t *testing.T) {
	g := newRig(t)
	g.fv.set(State{Status: StatusOpen})
	o := g.submit(oms.Buy, 10_000, 6_500_000)
	eventually(t, "placement", func() bool { return g.out.hasEvent("placed") })

	g.reg.Close(o.OrderID, "kill switch")
	g.r.CancelAtVenue(o.OrderID)
	eventually(t, "venue cancel", func() bool {
		g.fv.mu.Lock()
		defer g.fv.mu.Unlock()
		return g.fv.cancels == 1
	})
}
