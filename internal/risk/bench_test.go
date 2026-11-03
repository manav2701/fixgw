package risk

import (
	"testing"

	"github.com/manav2701/fixgw/internal/oms"
)

// BenchmarkCheck is one pre-trade check for a counterparty holding the default
// maximum of 200 open orders, which is the worst case the open-order limit allows.
func BenchmarkCheck(b *testing.B) {
	ins := oms.DefaultInstruments()
	r := New(Config{Default: Limits{MaxOrderNotional: 1e9, MaxGrossExposure: 1e12, MaxOpenOrders: 1000, CollarBps: 500}})
	open := make([]oms.Order, 200)
	for i := range open {
		open[i] = oms.Order{Symbol: "BTC/USD", Type: oms.Limit, Price: 6_400_000, LeavesQty: 1000, Qty: 1000}
	}
	in := Input{
		Order:       oms.Order{Counterparty: "A", Symbol: "BTC/USD", Side: oms.Buy, Type: oms.Limit, Price: 6_500_000, Qty: 1000},
		Instruments: ins, RefPx: 6_500_000, Open: open,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rj := r.Check(in); rj != nil {
			b.Fatal(rj)
		}
	}
}
