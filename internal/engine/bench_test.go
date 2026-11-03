package engine

import (
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/manav2701/fixgw/internal/oms"
	"github.com/manav2701/fixgw/internal/risk"
)

func benchEngine(b *testing.B) (*Engine, *oms.Registry) {
	b.Helper()
	reg := oms.NewRegistry()
	rk := risk.New(risk.Config{Default: risk.Limits{MaxOrderNotional: 1e9, MaxGrossExposure: 1e15, MaxOpenOrders: 1 << 30}})
	e := New(reg, oms.DefaultInstruments(), rk, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.Start()
	b.Cleanup(e.Stop)
	return e, reg
}

func benchOrder(i int, cp string, side oms.Side, px oms.Price, qty oms.Qty) oms.Order {
	return oms.Order{
		Session: "S-" + cp, Counterparty: cp, ClOrdID: fmt.Sprintf("%s-%d", cp, i),
		Symbol: sym, Side: side, Type: oms.Limit, TIF: oms.GTC, Qty: qty, Price: px,
	}
}

// BenchmarkBookAddCancel measures the pure data structure: no registry, risk or
// goroutine hand-off.
func BenchmarkBookAddCancel(b *testing.B) {
	bk := NewBook(sym)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		id := fmt.Sprint(i)
		px := oms.Price(6_000_000 + int64(i%200)*50)
		bk.Add(Entry{ID: id, Key: "k", Side: oms.Buy, Price: px, Leaves: 1000})
		bk.Cancel(id)
	}
}

// BenchmarkBookMatch measures matching a taker against a deep book.
func BenchmarkBookMatch(b *testing.B) {
	bk := NewBook(sym)
	for i := 0; i < 5000; i++ {
		bk.Add(Entry{ID: fmt.Sprintf("m%d", i), Key: "maker", Side: oms.Sell, Price: oms.Price(6_500_000 + int64(i%100)*50), Leaves: 1_000_000_000})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := bk.Match(Taker{ID: "t", Key: "taker", Side: oms.Buy, Type: oms.Limit, Price: 6_600_000, Qty: 100})
		if len(res.Fills) != 1 {
			b.Fatal("expected one fill")
		}
	}
}

// BenchmarkEngineRest is the full order path for an order that rests: hand-off to
// the engine goroutine, risk, registry and book.
func BenchmarkEngineRest(b *testing.B) {
	e, _ := benchEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		px := oms.Price(6_000_000 - int64(i%500)*50)
		// Spread over many counterparties: the per-counterparty open-order limit
		// is what bounds the cost of the pre-trade check in production.
		if _, err := e.NewOrder(benchOrder(i, fmt.Sprintf("C%d", i%500), oms.Buy, px, 1000)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEngineCross measures the full path when every second order trades.
func BenchmarkEngineCross(b *testing.B) {
	e, _ := benchEngine(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		side, cp := oms.Buy, "A"
		if i%2 == 1 {
			side, cp = oms.Sell, "B"
		}
		if _, err := e.NewOrder(benchOrder(i, cp, side, 6_500_000, 1000)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEngineRestWithBacklog shows how the cost of one order grows with the
// number of orders the gateway already holds.
func BenchmarkEngineRestWithBacklog(b *testing.B) {
	for _, backlog := range []int{1_000, 20_000} {
		b.Run(fmt.Sprintf("open=%d", backlog), func(b *testing.B) {
			e, _ := benchEngine(b)
			for i := 0; i < backlog; i++ {
				e.NewOrder(benchOrder(i, fmt.Sprintf("C%d", i%500), oms.Buy, oms.Price(5_000_000-int64(i%500)*50), 1000))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				e.NewOrder(benchOrder(backlog+i, fmt.Sprintf("C%d", i%500), oms.Buy, 5_000_000, 1000))
			}
		})
	}
}
