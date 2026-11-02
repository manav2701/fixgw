package engine

import (
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"testing"

	"github.com/manav2701/fixgw/internal/oms"
	"github.com/manav2701/fixgw/internal/risk"
)

const sym = "BTC/USD"

type harness struct {
	t   *testing.T
	eng *Engine
	reg *oms.Registry
	rk  *risk.Risk
	n   int
}

func newHarness(t *testing.T, cfg risk.Config) *harness {
	t.Helper()
	reg := oms.NewRegistry()
	rk := risk.New(cfg)
	eng := New(reg, oms.DefaultInstruments(), rk, slog.New(slog.NewTextHandler(io.Discard, nil)))
	eng.Start()
	t.Cleanup(eng.Stop)
	return &harness{t: t, eng: eng, reg: reg, rk: rk}
}

// open config: nothing to get in the way of matching tests.
func openRisk() risk.Config {
	return risk.Config{Default: risk.Limits{MaxOrderNotional: 1e9, MaxGrossExposure: 1e12, MaxOpenOrders: 100000, CollarBps: 0}}
}

// Prices are in cents (2 decimals), quantities in 1e-6 BTC. Tick is 50, lot 100.
func (h *harness) order(cp string, side oms.Side, px oms.Price, qty oms.Qty, tif oms.TimeInForce) oms.Order {
	h.n++
	typ := oms.Limit
	if px == 0 {
		typ = oms.Market
	}
	return oms.Order{
		Session: "S-" + cp, Counterparty: cp, ClOrdID: fmt.Sprintf("%s-%d", cp, h.n),
		Symbol: sym, Side: side, Type: typ, TIF: tif, Qty: qty, Price: px,
	}
}

func (h *harness) submit(o oms.Order) []oms.Execution {
	h.t.Helper()
	execs, err := h.eng.NewOrder(o)
	if err != nil {
		h.t.Fatal(err)
	}
	return execs
}

func kinds(execs []oms.Execution) string {
	s := ""
	for _, e := range execs {
		s += fmt.Sprintf("%s:%c/%c ", e.Session, e.ExecType, e.OrdStatus)
	}
	return s
}

func TestCrossProducesReportsForBothSides(t *testing.T) {
	h := newHarness(t, openRisk())
	h.submit(h.order("A", oms.Sell, 6500000, 10000, oms.GTC))
	execs := h.submit(h.order("B", oms.Buy, 6500000, 4000, oms.GTC))

	if got, want := kinds(execs), "S-B:0/0 S-B:F/2 S-A:F/1 "; got != want {
		t.Fatalf("reports = %q, want %q", got, want)
	}
	taker, maker := execs[1], execs[2]
	if taker.LastQty != 4000 || taker.LastPx != 6500000 || taker.LeavesQty != 0 || taker.CumQty != 4000 {
		t.Errorf("taker fill: %+v", taker)
	}
	if maker.LastQty != 4000 || maker.LeavesQty != 6000 || maker.CumQty != 4000 || maker.AvgPx != 6500000 {
		t.Errorf("maker fill: %+v", maker)
	}
}

func TestPriceImprovementAndAveragePrice(t *testing.T) {
	h := newHarness(t, openRisk())
	h.submit(h.order("A", oms.Sell, 6500000, 1000, oms.GTC))
	h.submit(h.order("A", oms.Sell, 6501000, 1000, oms.GTC))
	execs := h.submit(h.order("B", oms.Buy, 6505000, 2000, oms.GTC)) // limit above both offers

	last := execs[len(execs)-1]
	_ = last
	var taker []oms.Execution
	for _, e := range execs {
		if e.Session == "S-B" && e.ExecType == oms.ExecTrade {
			taker = append(taker, e)
		}
	}
	if len(taker) != 2 || taker[0].LastPx != 6500000 || taker[1].LastPx != 6501000 {
		t.Fatalf("expected fills at maker prices, got %+v", taker)
	}
	if taker[1].AvgPx != 6500500 {
		t.Errorf("AvgPx = %d, want 6500500", taker[1].AvgPx)
	}
}

func TestTimeInForce(t *testing.T) {
	h := newHarness(t, openRisk())
	h.submit(h.order("A", oms.Sell, 6500000, 1000, oms.GTC))

	// IOC: fills what it can, cancels the rest.
	execs := h.submit(h.order("B", oms.Buy, 6500000, 3000, oms.IOC))
	if got, want := kinds(execs), "S-B:0/0 S-B:F/1 S-A:F/2 S-B:4/4 "; got != want {
		t.Errorf("IOC reports = %q, want %q", got, want)
	}

	// FOK that cannot fully fill must not trade at all.
	h.submit(h.order("A", oms.Sell, 6500000, 1000, oms.GTC))
	execs = h.submit(h.order("B", oms.Buy, 6500000, 3000, oms.FOK))
	if got, want := kinds(execs), "S-B:0/0 S-B:4/4 "; got != want {
		t.Errorf("unfillable FOK reports = %q, want %q", got, want)
	}
	if s, _, _ := h.eng.Snapshot(sym, 5); len(s.Asks) != 1 || s.Asks[0].Qty != 1000 {
		t.Errorf("FOK disturbed the book: %+v", s.Asks)
	}

	// FOK that can fill in full trades.
	execs = h.submit(h.order("B", oms.Buy, 6500000, 1000, oms.FOK))
	if got, want := kinds(execs), "S-B:0/0 S-B:F/2 S-A:F/2 "; got != want {
		t.Errorf("fillable FOK reports = %q, want %q", got, want)
	}

	// Market order into an empty book is acknowledged then cancelled.
	execs = h.submit(h.order("B", oms.Buy, 0, 1000, oms.IOC))
	if got, want := kinds(execs), "S-B:0/0 S-B:4/4 "; got != want {
		t.Errorf("market into empty book = %q, want %q", got, want)
	}
}

func TestSelfTradePreventionCancelsTheNewOrder(t *testing.T) {
	h := newHarness(t, openRisk())
	h.submit(h.order("A", oms.Sell, 6500000, 1000, oms.GTC))
	execs := h.submit(h.order("A", oms.Buy, 6500000, 1000, oms.GTC))
	if got, want := kinds(execs), "S-A:0/0 S-A:4/4 "; got != want {
		t.Fatalf("reports = %q, want %q", got, want)
	}
	if execs[1].Text != "self-trade prevented" {
		t.Errorf("text = %q", execs[1].Text)
	}
	if s, _, _ := h.eng.Snapshot(sym, 5); len(s.Asks) != 1 || len(s.Bids) != 0 {
		t.Errorf("book after STP: %+v", s)
	}
}

func TestReplaceSemantics(t *testing.T) {
	h := newHarness(t, openRisk())
	first := h.order("A", oms.Buy, 6500000, 5000, oms.GTC)
	h.submit(first)
	second := h.order("B", oms.Buy, 6500000, 5000, oms.GTC)
	h.submit(second)

	// Quantity decrease keeps priority.
	execs, rej, err := h.eng.Replace(ReplaceRequest{Session: first.Session, OrigClOrdID: first.ClOrdID, ClOrdID: "A-r1",
		Qty: 3000, Price: 6500000, Type: oms.Limit})
	if err != nil || rej != nil {
		t.Fatalf("replace: %v %+v", err, rej)
	}
	if len(execs) != 1 || execs[0].ExecType != oms.ExecReplaced || execs[0].OrigClOrdID != first.ClOrdID || execs[0].LeavesQty != 3000 {
		t.Fatalf("replace report: %+v", execs)
	}
	fills := h.submit(h.order("C", oms.Sell, 6500000, 3000, oms.GTC))
	if fills[2].Session != "S-A" { // taker ack, taker fill, then maker
		t.Fatalf("reduced order should fill first, got %q", kinds(fills))
	}

	// The old ClOrdID is gone; the chain continues from the new one.
	if _, rej, _ := h.eng.Cancel(first.Session, first.ClOrdID, "A-c0"); rej == nil || rej.Reason != oms.CxlUnknownOrder {
		t.Errorf("cancel by superseded ClOrdID: %+v", rej)
	}

	// Price change loses priority and can cross immediately.
	h.submit(h.order("D", oms.Sell, 6501000, 2000, oms.GTC))
	execs, rej, _ = h.eng.Replace(ReplaceRequest{Session: second.Session, OrigClOrdID: second.ClOrdID, ClOrdID: "B-r1",
		Qty: 5000, Price: 6501000, Type: oms.Limit})
	if rej != nil {
		t.Fatalf("replace: %+v", rej)
	}
	if got, want := kinds(execs), "S-B:5/0 S-B:F/1 S-D:F/2 "; got != want {
		t.Errorf("replace-through-market = %q, want %q", got, want)
	}

	// Replace of a terminal order, qty at or below cumqty, and duplicate IDs are rejected.
	if _, rej, _ := h.eng.Replace(ReplaceRequest{Session: "S-D", OrigClOrdID: "D-4", ClOrdID: "D-r1", Qty: 1000, Price: 6501000, Type: oms.Limit}); rej == nil || rej.Reason != oms.CxlTooLate || rej.ResponseTo != '2' {
		t.Errorf("replace of filled order: %+v", rej)
	}
	if _, rej, _ := h.eng.Replace(ReplaceRequest{Session: "S-B", OrigClOrdID: "B-r1", ClOrdID: "B-r2", Qty: 2000, Price: 6501000, Type: oms.Limit}); rej == nil || rej.Reason != oms.CxlOther {
		t.Errorf("replace to qty <= cumqty: %+v", rej)
	}
	if _, rej, _ := h.eng.Replace(ReplaceRequest{Session: "S-B", OrigClOrdID: "B-r1", ClOrdID: "B-r1", Qty: 9000, Price: 6501000, Type: oms.Limit}); rej == nil || rej.Reason != oms.CxlDuplicateID {
		t.Errorf("replace with reused ClOrdID: %+v", rej)
	}
}

func TestRiskRejectsComeBackAsExecutionReports(t *testing.T) {
	cfg := risk.Config{Default: risk.Limits{MaxOrderNotional: 1000, MaxGrossExposure: 1e12, MaxOpenOrders: 2, CollarBps: 500}}
	h := newHarness(t, cfg)

	cases := []struct {
		name string
		o    oms.Order
		text string
	}{
		{"off tick", h.order("A", oms.Buy, 6500025, 1000, oms.GTC), "tick"},
		{"off lot", h.order("A", oms.Buy, 6500000, 1050, oms.GTC), "lot"},
		{"below min", h.order("A", oms.Buy, 6500000, 0, oms.GTC), "minimum"},
		{"notional", h.order("A", oms.Buy, 6500000, 1_000_000, oms.GTC), "notional"},
	}
	for _, tc := range cases {
		execs := h.submit(tc.o)
		if len(execs) != 1 || execs[0].ExecType != oms.ExecRejected || execs[0].OrdStatus != oms.StatusRejected {
			t.Errorf("%s: %+v", tc.name, execs)
			continue
		}
		if !contains(execs[0].Text, tc.text) {
			t.Errorf("%s: text %q lacks %q", tc.name, execs[0].Text, tc.text)
		}
	}

	// Collar needs a reference price: make a trade, then stray from it.
	h.submit(h.order("B", oms.Sell, 6500000, 100, oms.GTC))
	h.submit(h.order("C", oms.Buy, 6500000, 100, oms.GTC))
	execs := h.submit(h.order("A", oms.Buy, 8000000, 100, oms.GTC))
	if execs[0].ExecType != oms.ExecRejected || !contains(execs[0].Text, "collar") {
		t.Errorf("collar: %+v", execs)
	}

	// Open-order cap: two resting orders allowed, third rejected.
	h.submit(h.order("D", oms.Buy, 6490000, 100, oms.GTC))
	h.submit(h.order("D", oms.Buy, 6490000, 100, oms.GTC))
	execs = h.submit(h.order("D", oms.Buy, 6490000, 100, oms.GTC))
	if execs[0].ExecType != oms.ExecRejected || !contains(execs[0].Text, "open order limit") {
		t.Errorf("open order cap: %+v", execs)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestKillSwitch(t *testing.T) {
	h := newHarness(t, openRisk())
	h.submit(h.order("A", oms.Buy, 6490000, 1000, oms.GTC))
	h.submit(h.order("A", oms.Buy, 6480000, 1000, oms.GTC))
	h.submit(h.order("B", oms.Sell, 6510000, 1000, oms.GTC))

	// Scoped to A: only A's orders are cancelled, only A is blocked.
	execs, err := h.eng.KillSwitch("A", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kinds(execs), "S-A:4/4 S-A:4/4 "; got != want {
		t.Errorf("mass cancel = %q, want %q", got, want)
	}
	if execs[0].Text != "kill switch" {
		t.Errorf("text = %q", execs[0].Text)
	}
	blocked := h.submit(h.order("A", oms.Buy, 6490000, 1000, oms.GTC))
	if blocked[0].ExecType != oms.ExecRejected || !contains(blocked[0].Text, "kill switch") {
		t.Errorf("A should be halted: %+v", blocked)
	}
	if ok := h.submit(h.order("B", oms.Sell, 6520000, 1000, oms.GTC)); ok[0].ExecType != oms.ExecNew {
		t.Errorf("B should still trade: %+v", ok)
	}

	// Cancels still work while halted.
	h.eng.KillSwitch("", true, false)
	b := h.order("B", oms.Sell, 6530000, 1000, oms.GTC)
	if ex := h.submit(b); ex[0].ExecType != oms.ExecRejected {
		t.Fatalf("global halt should reject new orders: %+v", ex)
	}
	if _, rej, _ := h.eng.Cancel("S-B", "B-3", "B-cx"); rej != nil {
		t.Errorf("cancel during halt should be allowed: %+v", rej)
	}

	// Resume.
	h.eng.KillSwitch("", false, false)
	h.eng.KillSwitch("A", false, false)
	if ex := h.submit(h.order("A", oms.Buy, 6490000, 1000, oms.GTC)); ex[0].ExecType != oms.ExecNew {
		t.Errorf("after resume: %+v", ex)
	}
}

func TestCancelSession(t *testing.T) {
	h := newHarness(t, openRisk())
	h.submit(h.order("A", oms.Buy, 6490000, 1000, oms.GTC))
	h.submit(h.order("B", oms.Buy, 6480000, 1000, oms.GTC))
	execs, err := h.eng.CancelSession("S-A", "cancel on disconnect")
	if err != nil || len(execs) != 1 || execs[0].Session != "S-A" {
		t.Fatalf("CancelSession: %v %+v", err, execs)
	}
	if s, _, _ := h.eng.Snapshot(sym, 5); len(s.Bids) != 1 || s.Bids[0].Price != 6480000 {
		t.Errorf("book = %+v", s)
	}
}

func TestRateLimit(t *testing.T) {
	cfg := risk.Config{Default: risk.Limits{RatePerSec: 0.001, RateBurst: 3}}
	h := newHarness(t, cfg)
	var rejected int
	for i := 0; i < 8; i++ {
		ex := h.submit(h.order("A", oms.Buy, 6490000, 1000, oms.GTC))
		if ex[0].ExecType == oms.ExecRejected && contains(ex[0].Text, "rate limit") {
			rejected++
		}
	}
	if rejected != 5 {
		t.Errorf("rejected %d of 8 with burst 3, want 5", rejected)
	}
	// Another counterparty has its own bucket.
	if ex := h.submit(h.order("B", oms.Buy, 6490000, 1000, oms.GTC)); ex[0].ExecType != oms.ExecNew {
		t.Errorf("B throttled by A's traffic: %+v", ex)
	}
}

func TestUnknownSymbolAndDuplicate(t *testing.T) {
	h := newHarness(t, openRisk())
	o := h.order("A", oms.Buy, 6490000, 1000, oms.GTC)
	o.Symbol = "DOGE/USD"
	if ex := h.submit(o); ex[0].RejectReason != oms.RejUnknownSymbol {
		t.Errorf("unknown symbol: %+v", ex)
	}
	good := h.order("A", oms.Buy, 6490000, 1000, oms.GTC)
	h.submit(good)
	if ex := h.submit(good); ex[0].RejectReason != oms.RejDuplicateOrder {
		t.Errorf("duplicate: %+v", ex)
	}
}

// Thousands of random orders from several counterparties, with cancels and
// replaces mixed in. Afterwards every invariant a venue must hold is checked.
func TestRandomFlowInvariants(t *testing.T) {
	h := newHarness(t, openRisk())
	rng := rand.New(rand.NewSource(42))
	cps := []string{"A", "B", "C", "D"}
	tifs := []oms.TimeInForce{oms.GTC, oms.GTC, oms.GTC, oms.IOC, oms.FOK, oms.Day}

	var all []oms.Execution
	var clids []struct{ session, id string }
	for i := 0; i < 6000; i++ {
		cp := cps[rng.Intn(len(cps))]
		switch op := rng.Intn(10); {
		case op < 7:
			side := oms.Buy
			if rng.Intn(2) == 0 {
				side = oms.Sell
			}
			px := oms.Price(6490000 + 50*rng.Intn(41)) // 4,900.00 .. +20.00 in ticks around 64,900
			if rng.Intn(8) == 0 {
				px = 0 // market
			}
			o := h.order(cp, side, px, oms.Qty(100*(rng.Intn(30)+1)), tifs[rng.Intn(len(tifs))])
			if rng.Intn(5) == 0 {
				o.Account = "acct-" + cp // account-level STP
			}
			all = append(all, h.submit(o)...)
			clids = append(clids, struct{ session, id string }{o.Session, o.ClOrdID})
		case op < 9 && len(clids) > 0:
			c := clids[rng.Intn(len(clids))]
			ex, _, err := h.eng.Cancel(c.session, c.id, fmt.Sprintf("cx-%d", i))
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, ex...)
		case len(clids) > 0:
			c := clids[rng.Intn(len(clids))]
			if cur, ok := h.reg.Get(c.session, c.id); ok && !cur.Status.IsTerminal() && cur.Type == oms.Limit {
				newID := fmt.Sprintf("rp-%d", i)
				ex, rej, err := h.eng.Replace(ReplaceRequest{Session: c.session, OrigClOrdID: c.id, ClOrdID: newID,
					Qty: cur.CumQty + oms.Qty(100*(rng.Intn(20)+1)), Price: oms.Price(6490000 + 50*rng.Intn(41)), Type: oms.Limit})
				if err != nil {
					t.Fatal(err)
				}
				if rej == nil {
					all = append(all, ex...)
					clids = append(clids, struct{ session, id string }{c.session, newID})
				}
			}
		}
		if i%500 == 0 {
			if err := h.eng.Verify(); err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
	}
	if err := h.eng.Verify(); err != nil {
		t.Fatal(err)
	}

	// 1. Every trade has two sides: total bought equals total sold, per fill pairing.
	var bought, sold oms.Qty
	for _, e := range all {
		if e.ExecType != oms.ExecTrade {
			continue
		}
		if e.Side == oms.Buy {
			bought += e.LastQty
		} else {
			sold += e.LastQty
		}
	}
	if bought != sold || bought == 0 {
		t.Errorf("bought %d != sold %d (or no trades happened)", bought, sold)
	}

	// 2. Each order's quantity is fully accounted for, and its state is coherent.
	book, _, _ := h.eng.Snapshot(sym, 1000)
	var restingBook oms.Qty
	for _, l := range book.Bids {
		restingBook += l.Qty
	}
	for _, l := range book.Asks {
		restingBook += l.Qty
	}
	var restingOrders oms.Qty
	for _, o := range h.reg.All() {
		switch o.Status {
		case oms.StatusNew, oms.StatusPartiallyFilled:
			restingOrders += o.LeavesQty
			if o.LeavesQty == 0 || o.CumQty+o.LeavesQty != o.Qty {
				t.Errorf("open order %s inconsistent: %+v", o.OrderID, o)
			}
		case oms.StatusFilled:
			if o.CumQty != o.Qty || o.LeavesQty != 0 {
				t.Errorf("filled order %s inconsistent: %+v", o.OrderID, o)
			}
		case oms.StatusCanceled:
			if o.LeavesQty != 0 || o.CumQty > o.Qty {
				t.Errorf("cancelled order %s inconsistent: %+v", o.OrderID, o)
			}
		default:
			t.Errorf("order %s in unexpected state %c", o.OrderID, o.Status)
		}
	}
	// 3. What the registry says is open is exactly what the book holds.
	if restingOrders != restingBook {
		t.Errorf("registry has %d open, book has %d", restingOrders, restingBook)
	}
	if len(h.reg.Open()) != func() int {
		n := 0
		for _, l := range append(book.Bids, book.Asks...) {
			n += l.Orders
		}
		return n
	}() {
		t.Errorf("open order count disagrees with book")
	}
}
