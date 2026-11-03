package fixsession_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manav2701/fixgw/internal/bridge"
	"github.com/manav2701/fixgw/internal/fixmsg"
	"github.com/manav2701/fixgw/internal/oms"
	"github.com/manav2701/fixgw/internal/risk"
	"github.com/manav2701/fixgw/internal/simclient"
	"github.com/manav2701/fixgw/internal/venue"
)

// stubVenue is an exchange the test controls.
type stubVenue struct {
	mu      sync.Mutex
	state   venue.State
	placed  []venue.Request
	cancels int
}

func (s *stubVenue) Name() string { return "stub" }
func (s *stubVenue) Place(_ context.Context, r venue.Request) (venue.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.placed = append(s.placed, r)
	s.state = venue.State{VenueID: "V1", Status: venue.StatusOpen}
	return s.state, nil
}
func (s *stubVenue) Cancel(context.Context, venue.Ref) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels++
	s.state.Status = venue.StatusCanceled
	return nil
}
func (s *stubVenue) Query(context.Context, venue.Ref) (venue.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}
func (s *stubVenue) set(st venue.State) {
	s.mu.Lock()
	st.VenueID = "V1"
	s.state = st
	s.mu.Unlock()
}
func (s *stubVenue) counts() (placed, cancels int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.placed), s.cancels
}

func routedEnv(t *testing.T) (*env, *stubVenue, *bridge.Hub) {
	sv := &stubVenue{}
	hub := bridge.NewHub()
	e := newEnvWith(t, envOpts{
		store: "memory", risk: risk.DefaultConfig(), hub: hub,
		// ETH/USD: gateway tick 0.01, lot 0.0001. The stub venue is coarser:
		// tick 0.05, step 0.001.
		venues: []venue.Binding{{Venue: sv, Spec: venue.Spec{Symbol: "ETH/USD", VenueSymbol: "ETHUSDT", Tick: 5, Step: 100, MinQty: 100}}},
	})
	return e, sv, hub
}

func logon(t *testing.T, cfg simclient.Config) *simclient.Client {
	t.Helper()
	c, err := simclient.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	if err := c.WaitLogon(ctx(t), 8*time.Second); err != nil {
		t.Fatalf("logon: %v", err)
	}
	return c
}

func waitFor(t *testing.T, c *simclient.Client, what string, pred func(simclient.Report) bool) simclient.Report {
	t.Helper()
	var got simclient.Report
	err := simclient.WaitFor(ctx(t), 8*time.Second, func() bool {
		r, ok := c.Find(pred)
		got = r
		return ok
	})
	if err != nil {
		t.Fatalf("timed out waiting for %s", what)
	}
	return got
}

func execOf(id, et string) func(simclient.Report) bool {
	return func(r simclient.Report) bool {
		return r.MsgType == fixmsg.MsgExecutionReport && r.ClOrdID == id && r.ExecType == et
	}
}

func ethOrder(id, qty, px string) fixmsg.NewOrder {
	return fixmsg.NewOrder{ClOrdID: id, Symbol: "ETH/USD", Side: oms.Buy, Type: oms.Limit, TIF: oms.GTC, Qty: qty, Price: px}
}

func TestVenueRoutedOrderLifecycleOverFIX(t *testing.T) {
	e, sv, hub := routedEnv(t)
	feed := hub.Subscribe(4096)
	defer feed.Close()
	c := logon(t, e.cfg)

	// The order is acknowledged immediately and reaches the venue.
	c.SendOrder(ethOrder("V-1", "0.100", "3000.00"))
	waitFor(t, c, "ack", execOf("V-1", "0"))
	if err := simclient.WaitFor(ctx(t), 5*time.Second, func() bool { p, _ := sv.counts(); return p == 1 }); err != nil {
		t.Fatal("order never reached the venue")
	}
	if p := sv.placed[0]; p.Symbol != "ETH/USD" || p.Qty != 10_000 || p.Price != 300_000 {
		t.Errorf("venue request = %+v", p)
	}

	// The venue fills part of it; the client gets a trade report from the poll loop.
	sv.set(venue.State{Status: venue.StatusPartial, CumQty: 4_000, AvgPx: 300_000})
	tr := waitFor(t, c, "partial fill", execOf("V-1", "F"))
	if tr.LastQty != "0.04000" || tr.LastPx != "3000.00" || tr.OrdStatus != "1" || tr.LeavesQty != "0.06000" {
		t.Errorf("partial fill report = %+v", tr)
	}

	// A replace is refused, not approximated.
	c.SendReplace("V-1", ethOrder("V-2", "0.200", "3000.00"))
	rj := waitFor(t, c, "replace reject", func(r simclient.Report) bool {
		return r.MsgType == fixmsg.MsgOrderCancelReject && r.ClOrdID == "V-2"
	})
	if rj.RejReason != "99" {
		t.Errorf("replace reject reason = %q", rj.RejReason)
	}

	// Cancel goes to the venue and is confirmed once it accepts.
	c.SendCancel("V-1", "V-3", "ETH/USD", oms.Buy, "0.100")
	cx := waitFor(t, c, "cancel", execOf("V-3", "4"))
	if cx.OrigClOrdID != "V-1" || cx.CumQty != "0.04000" {
		t.Errorf("cancel report = %+v", cx)
	}
	if _, n := sv.counts(); n != 1 {
		t.Errorf("venue cancels = %d", n)
	}

	// Everything above reached the dashboard feed, and no secret did.
	kinds := map[string]int{}
	var wire strings.Builder
	for len(feed.C()) > 0 {
		b := <-feed.C()
		var ev bridge.Event
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatal(err)
		}
		kinds[ev.Kind]++
		if ev.Kind == bridge.KindWire {
			wire.Write(b)
		}
	}
	for _, k := range []string{bridge.KindExec, bridge.KindWire, bridge.KindSession, bridge.KindVenue} {
		if kinds[k] == 0 {
			t.Errorf("no %q events on the feed: %v", k, kinds)
		}
	}
	if strings.Contains(wire.String(), "client1-pass") || strings.Contains(wire.String(), "554=client1") {
		t.Error("password leaked onto the dashboard tape")
	}
	if !strings.Contains(wire.String(), "554=***") {
		t.Error("expected the password field to appear redacted on the tape")
	}
}

func TestVenueGridMismatchIsRejectedBeforeAcceptance(t *testing.T) {
	e, sv, _ := routedEnv(t)
	c := logon(t, e.cfg)

	// Valid for the gateway (tick 0.01) but not the venue (tick 0.05).
	c.SendOrder(ethOrder("G-1", "0.100", "3000.01"))
	rj := waitFor(t, c, "reject", execOf("G-1", "8"))
	if !strings.Contains(rj.Text, "venue tick") {
		t.Errorf("reject text = %q", rj.Text)
	}
	if p, _ := sv.counts(); p != 0 {
		t.Error("a rejected order must never reach the venue")
	}
}

func TestKillSwitchCancelsRoutedOrdersAtTheVenue(t *testing.T) {
	e, sv, _ := routedEnv(t)
	c := logon(t, e.cfg)

	c.SendOrder(ethOrder("K-1", "0.100", "3000.00"))
	waitFor(t, c, "ack", execOf("K-1", "0"))
	if err := simclient.WaitFor(ctx(t), 5*time.Second, func() bool { p, _ := sv.counts(); return p == 1 }); err != nil {
		t.Fatal("order never reached the venue")
	}

	if n, err := e.srv.KillSwitch("", true, true); err != nil || n != 1 {
		t.Fatalf("kill switch: n=%d err=%v", n, err)
	}
	cx := waitFor(t, c, "kill switch cancel", execOf("K-1", "4"))
	if !strings.Contains(cx.Text, "kill switch") {
		t.Errorf("cancel text = %q", cx.Text)
	}
	if err := simclient.WaitFor(ctx(t), 5*time.Second, func() bool { _, n := sv.counts(); return n == 1 }); err != nil {
		t.Fatal("the order is still live at the venue after the kill switch")
	}
}

func TestUnroutedSymbolsStillMatchInternally(t *testing.T) {
	e, _, _ := routedEnv(t)
	a, b := logon(t, e.cfg), logon(t, e.cfg2)

	a.SendOrder(fixmsg.NewOrder{ClOrdID: "I-1", Symbol: "BTC/USD", Side: oms.Sell, Type: oms.Limit, TIF: oms.GTC, Qty: "0.010000", Price: "65000.00"})
	waitFor(t, a, "ack", execOf("I-1", "0"))
	b.SendOrder(fixmsg.NewOrder{ClOrdID: "I-2", Symbol: "BTC/USD", Side: oms.Buy, Type: oms.Limit, TIF: oms.GTC, Qty: "0.010000", Price: "65000.00"})
	tr := waitFor(t, b, "trade", execOf("I-2", "F"))
	if tr.OrdStatus != "2" || tr.LastPx != "65000.00" {
		t.Errorf("trade = %+v", tr)
	}
}
