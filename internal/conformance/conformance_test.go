// Package conformance checks the gateway's order-state behaviour against the
// FIX 4.4 execution-report rules, as a table of named scenarios rather than as
// the implementation's own tests: each case states the reports a counterparty
// must see, as ExecType/OrdStatus pairs, and a set of invariants is applied to
// every report from every case.
package conformance

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/manav2701/fixgw/internal/engine"
	"github.com/manav2701/fixgw/internal/oms"
	"github.com/manav2701/fixgw/internal/risk"
)

const sym = "BTC/USD"

type rig struct {
	t    *testing.T
	eng  *engine.Engine
	seen map[string]bool // ExecIDs, for uniqueness
	all  []oms.Execution
	n    int
}

func newRig(t *testing.T) *rig {
	t.Helper()
	eng := engine.New(oms.NewRegistry(), oms.DefaultInstruments(),
		risk.New(risk.Config{Default: risk.Limits{MaxOrderNotional: 1e9, MaxGrossExposure: 1e12, MaxOpenOrders: 1000}}),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	eng.Start()
	t.Cleanup(eng.Stop)
	return &rig{t: t, eng: eng, seen: map[string]bool{}}
}

func (r *rig) record(execs []oms.Execution) []oms.Execution {
	r.all = append(r.all, execs...)
	return execs
}

func (r *rig) order(cp string, side oms.Side, typ oms.OrdType, tif oms.TimeInForce, px oms.Price, qty oms.Qty) oms.Order {
	r.n++
	return oms.Order{Session: cp, Counterparty: cp, ClOrdID: fmt.Sprintf("%s%d", cp, r.n), Symbol: sym,
		Side: side, Type: typ, TIF: tif, Qty: qty, Price: px}
}

func (r *rig) submit(o oms.Order) []oms.Execution {
	r.t.Helper()
	execs, err := r.eng.NewOrder(o)
	if err != nil {
		r.t.Fatal(err)
	}
	return r.record(execs)
}

func (r *rig) cancel(session, orig string) ([]oms.Execution, *oms.CancelReject) {
	r.t.Helper()
	r.n++
	execs, rej, err := r.eng.Cancel(session, orig, fmt.Sprintf("X%d", r.n))
	if err != nil {
		r.t.Fatal(err)
	}
	return r.record(execs), rej
}

func (r *rig) replace(session, orig string, qty oms.Qty, px oms.Price) ([]oms.Execution, *oms.CancelReject) {
	r.t.Helper()
	r.n++
	execs, rej, err := r.eng.Replace(engine.ReplaceRequest{Session: session, OrigClOrdID: orig,
		ClOrdID: fmt.Sprintf("R%d", r.n), Qty: qty, Price: px, Type: oms.Limit})
	if err != nil {
		r.t.Fatal(err)
	}
	return r.record(execs), rej
}

// seq renders reports as "session:ExecType/OrdStatus" pairs.
func seq(execs []oms.Execution) string {
	parts := make([]string, len(execs))
	for i, e := range execs {
		parts[i] = fmt.Sprintf("%s:%c/%c", e.Session, e.ExecType, e.OrdStatus)
	}
	return strings.Join(parts, " ")
}

const (
	px  = oms.Price(6_500_000) // 65000.00
	qty = oms.Qty(10_000)      // 0.01
)

// TestExecutionReportSequences is the scenario table.
func TestExecutionReportSequences(t *testing.T) {
	cases := []struct {
		name string // what the FIX rule says
		run  func(r *rig) []oms.Execution
		want string
	}{
		{"new order is acknowledged with ExecType=New, OrdStatus=New", func(r *rig) []oms.Execution {
			return r.submit(r.order("A", oms.Buy, oms.Limit, oms.GTC, px, qty))
		}, "A:0/0"},

		{"a crossing order reports New, then the trade for the taker, then the trade for the maker", func(r *rig) []oms.Execution {
			r.submit(r.order("A", oms.Sell, oms.Limit, oms.GTC, px, qty))
			return r.submit(r.order("B", oms.Buy, oms.Limit, oms.GTC, px, qty))
		}, "B:0/0 B:F/2 A:F/2"},

		{"a partial fill leaves both the maker and the taker partially filled and resting", func(r *rig) []oms.Execution {
			r.submit(r.order("A", oms.Sell, oms.Limit, oms.GTC, px, qty))
			return r.submit(r.order("B", oms.Buy, oms.Limit, oms.GTC, px, 4_000))
		}, "B:0/0 B:F/2 A:F/1"},

		{"an aggressor larger than the book fills what it can and rests the remainder", func(r *rig) []oms.Execution {
			r.submit(r.order("A", oms.Sell, oms.Limit, oms.GTC, px, 4_000))
			return r.submit(r.order("B", oms.Buy, oms.Limit, oms.GTC, px, qty))
		}, "B:0/0 B:F/1 A:F/2"},

		{"IOC: the unfilled remainder is cancelled, never rested", func(r *rig) []oms.Execution {
			r.submit(r.order("A", oms.Sell, oms.Limit, oms.GTC, px, 4_000))
			return r.submit(r.order("B", oms.Buy, oms.Limit, oms.IOC, px, qty))
		}, "B:0/0 B:F/1 A:F/2 B:4/4"},

		{"FOK that cannot be filled in full trades nothing and is cancelled", func(r *rig) []oms.Execution {
			r.submit(r.order("A", oms.Sell, oms.Limit, oms.GTC, px, 4_000))
			return r.submit(r.order("B", oms.Buy, oms.Limit, oms.FOK, px, qty))
		}, "B:0/0 B:4/4"},

		{"FOK that can be filled in full fills completely", func(r *rig) []oms.Execution {
			r.submit(r.order("A", oms.Sell, oms.Limit, oms.GTC, px, qty))
			return r.submit(r.order("B", oms.Buy, oms.Limit, oms.FOK, px, qty))
		}, "B:0/0 B:F/2 A:F/2"},

		{"a market order with no liquidity is cancelled", func(r *rig) []oms.Execution {
			return r.submit(r.order("B", oms.Buy, oms.Market, oms.IOC, 0, qty))
		}, "B:0/0 B:4/4"},

		{"non-crossing limits do not trade", func(r *rig) []oms.Execution {
			r.submit(r.order("A", oms.Sell, oms.Limit, oms.GTC, px+50, qty))
			return r.submit(r.order("B", oms.Buy, oms.Limit, oms.GTC, px, qty))
		}, "B:0/0"},

		{"cancelling a resting order reports ExecType=Canceled, OrdStatus=Canceled", func(r *rig) []oms.Execution {
			o := r.order("A", oms.Buy, oms.Limit, oms.GTC, px, qty)
			r.submit(o)
			ex, _ := r.cancel("A", o.ClOrdID)
			return ex
		}, "A:4/4"},

		{"replacing a resting order reports ExecType=Replaced and keeps OrdStatus=New", func(r *rig) []oms.Execution {
			o := r.order("A", oms.Buy, oms.Limit, oms.GTC, px, qty)
			r.submit(o)
			ex, _ := r.replace("A", o.ClOrdID, 5_000, px)
			return ex
		}, "A:5/0"},

		{"replacing to a crossing price reports Replaced, then the trades", func(r *rig) []oms.Execution {
			r.submit(r.order("B", oms.Sell, oms.Limit, oms.GTC, px+50, qty))
			o := r.order("A", oms.Buy, oms.Limit, oms.GTC, px, qty)
			r.submit(o)
			ex, _ := r.replace("A", o.ClOrdID, qty, px+50)
			return ex
		}, "A:5/0 A:F/2 B:F/2"},

		{"replacing a partially filled order keeps OrdStatus=PartiallyFilled", func(r *rig) []oms.Execution {
			o := r.order("A", oms.Sell, oms.Limit, oms.GTC, px, qty)
			r.submit(o)
			r.submit(r.order("B", oms.Buy, oms.Limit, oms.GTC, px, 4_000))
			ex, _ := r.replace("A", o.ClOrdID, 8_000, px)
			return ex
		}, "A:5/1"},

		{"a duplicate ClOrdID is rejected with OrdRejReason=6", func(r *rig) []oms.Execution {
			o := r.order("A", oms.Buy, oms.Limit, oms.GTC, px, qty)
			r.submit(o)
			return r.submit(o)
		}, "A:8/8"},

		{"an unknown symbol is rejected", func(r *rig) []oms.Execution {
			o := r.order("A", oms.Buy, oms.Limit, oms.GTC, px, qty)
			o.Symbol = "DOGE/USD"
			return r.submit(o)
		}, "A:8/8"},

		{"self-trade prevention cancels the incoming order and trades nothing", func(r *rig) []oms.Execution {
			r.submit(r.order("A", oms.Sell, oms.Limit, oms.GTC, px, qty))
			return r.submit(r.order("A", oms.Buy, oms.Limit, oms.GTC, px, qty))
		}, "A:0/0 A:4/4"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			got := seq(c.run(r))
			if got != c.want {
				t.Errorf("reports = %q, want %q", got, c.want)
			}
			checkInvariants(t, r.all)
		})
	}
}

// checkInvariants applies the rules every execution report must satisfy.
func checkInvariants(t *testing.T, execs []oms.Execution) {
	t.Helper()
	ids := map[string]bool{}
	for _, e := range execs {
		id := fmt.Sprintf("%s %c/%c", e.ExecID, e.ExecType, e.OrdStatus)
		if ids[e.ExecID] {
			t.Errorf("%s: ExecID repeated", id)
		}
		ids[e.ExecID] = true

		switch e.OrdStatus {
		case oms.StatusNew, oms.StatusPartiallyFilled:
			if e.CumQty+e.LeavesQty != e.OrderQty {
				t.Errorf("%s: CumQty %d + LeavesQty %d != OrderQty %d", id, e.CumQty, e.LeavesQty, e.OrderQty)
			}
		case oms.StatusFilled:
			if e.LeavesQty != 0 || e.CumQty != e.OrderQty {
				t.Errorf("%s: filled but CumQty %d, LeavesQty %d, OrderQty %d", id, e.CumQty, e.LeavesQty, e.OrderQty)
			}
		case oms.StatusCanceled, oms.StatusRejected:
			if e.LeavesQty != 0 {
				t.Errorf("%s: terminal state with LeavesQty %d", id, e.LeavesQty)
			}
		}
		if e.OrdStatus == oms.StatusPartiallyFilled && (e.CumQty <= 0 || e.LeavesQty <= 0) {
			t.Errorf("%s: partial fill needs both CumQty and LeavesQty positive", id)
		}
		switch e.ExecType {
		case oms.ExecTrade:
			if e.LastQty <= 0 || e.LastPx <= 0 || e.LastQty > e.CumQty {
				t.Errorf("%s: bad trade fields LastQty=%d LastPx=%d CumQty=%d", id, e.LastQty, e.LastPx, e.CumQty)
			}
			if e.AvgPx <= 0 {
				t.Errorf("%s: trade without AvgPx", id)
			}
		case oms.ExecRejected:
			if e.OrderID != "NONE" || e.RejectReason == 0 && e.Text == "" {
				t.Errorf("%s: reject should carry OrderID NONE and a reason", id)
			}
		case oms.ExecNew:
			if e.CumQty != 0 || e.LeavesQty != e.OrderQty {
				t.Errorf("%s: New acknowledgement with fills", id)
			}
		}
		if e.ExecType != oms.ExecTrade && (e.LastQty != 0 || e.LastPx != 0) {
			t.Errorf("%s: LastQty/LastPx only belong on trades", id)
		}
	}
}

// TestOrderStatusNeverRegresses replays a long random session and checks that
// each order only ever moves along the FIX state machine: never out of a
// terminal state, never from PartiallyFilled back to New, and CumQty only grows.
func TestOrderStatusNeverRegresses(t *testing.T) {
	r := newRig(t)
	type last struct {
		st  oms.OrdStatus
		cum oms.Qty
	}
	seen := map[string]last{}
	rank := map[oms.OrdStatus]int{oms.StatusNew: 0, oms.StatusPartiallyFilled: 1, oms.StatusFilled: 2, oms.StatusCanceled: 2, oms.StatusRejected: 2}

	var open []oms.Order
	seed := uint64(12345)
	next := func(n uint64) uint64 { seed = seed*6364136223846793005 + 1442695040888963407; return (seed >> 33) % n }
	for i := 0; i < 3000; i++ {
		cp := string(rune('A' + next(3)))
		switch next(5) {
		case 0, 1, 2:
			side := oms.Buy
			if next(2) == 0 {
				side = oms.Sell
			}
			o := r.order(cp, side, oms.Limit, oms.GTC, px+oms.Price(next(6))*50, oms.Qty(100*(1+next(30))))
			r.submit(o)
			open = append(open, o)
		case 3:
			if len(open) > 0 {
				o := open[next(uint64(len(open)))]
				r.cancel(o.Session, o.ClOrdID)
			}
		case 4:
			if len(open) > 0 {
				o := open[next(uint64(len(open)))]
				r.replace(o.Session, o.ClOrdID, oms.Qty(100*(1+next(30))), px+oms.Price(next(6))*50)
			}
		}
	}
	checkInvariants(t, r.all)
	for _, e := range r.all {
		prev, ok := seen[e.OrderID]
		if e.OrderID == "NONE" {
			continue
		}
		if ok {
			if rank[e.OrdStatus] < rank[prev.st] || (rank[prev.st] == 2 && e.ExecType == oms.ExecTrade) {
				t.Fatalf("order %s moved %c -> %c", e.OrderID, prev.st, e.OrdStatus)
			}
			if e.CumQty < prev.cum {
				t.Fatalf("order %s CumQty fell %d -> %d", e.OrderID, prev.cum, e.CumQty)
			}
		}
		seen[e.OrderID] = last{e.OrdStatus, e.CumQty}
	}
	if len(seen) < 100 {
		t.Fatalf("only %d orders exercised", len(seen))
	}
}
