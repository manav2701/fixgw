package risk

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/manav2701/fixgw/internal/oms"
)

var ins = oms.DefaultInstruments()

func btc(cp string, side oms.Side, px oms.Price, qty oms.Qty) oms.Order {
	return oms.Order{Counterparty: cp, Symbol: "BTC/USD", Side: side, Type: oms.Limit, TIF: oms.GTC, Price: px, Qty: qty}
}

func check(r *Risk, o oms.Order, ref oms.Price, open ...oms.Order) *Reject {
	return r.Check(Input{Order: o, Instruments: ins, RefPx: ref, Open: open})
}

func wantReject(t *testing.T, rj *Reject, substr string) {
	t.Helper()
	if rj == nil || !strings.Contains(rj.Text, substr) {
		t.Errorf("got %v, want a reject containing %q", rj, substr)
	}
}

func TestInstrumentConformance(t *testing.T) {
	r := New(Config{})
	if rj := check(r, btc("A", oms.Buy, 6500000, 10000), 0); rj != nil {
		t.Fatalf("conforming order rejected: %v", rj)
	}
	wantReject(t, check(r, btc("A", oms.Buy, 6500025, 10000), 0), "tick")
	wantReject(t, check(r, btc("A", oms.Buy, 6500000, 10050), 0), "lot")
	wantReject(t, check(r, btc("A", oms.Buy, 6500000, 0), 0), "minimum")
	wantReject(t, check(r, btc("A", oms.Buy, 6500000, 200_000_000), 0), "maximum")
	o := btc("A", oms.Buy, 6500000, 10000)
	o.Symbol = "NOPE/USD"
	if rj := check(r, o, 0); rj == nil || rj.Reason != oms.RejUnknownSymbol {
		t.Errorf("unknown symbol: %v", rj)
	}
}

func TestNotionalLimit(t *testing.T) {
	r := New(Config{Default: Limits{MaxOrderNotional: 1000}})
	// 0.01 BTC @ 65,000 = 650 USD: allowed. 0.02 BTC = 1,300 USD: not.
	if rj := check(r, btc("A", oms.Buy, 6500000, 10000), 0); rj != nil {
		t.Errorf("650 USD rejected: %v", rj)
	}
	wantReject(t, check(r, btc("A", oms.Buy, 6500000, 20000), 0), "notional")

	// Exactly at the limit passes; one tick over fails.
	if rj := check(r, btc("A", oms.Buy, 10_000_000, 10_000), 0); rj != nil { // 100,000.00 x 0.01 = 1,000
		t.Errorf("notional exactly at the limit rejected: %v", rj)
	}
	wantReject(t, check(r, btc("A", oms.Buy, 10_000_050, 10_000), 0), "notional")

	// Market orders are valued at the reference price; with none, the check is skipped.
	m := btc("A", oms.Buy, 0, 20000)
	m.Type = oms.Market
	wantReject(t, check(r, m, 6500000), "notional")
	if rj := check(r, m, 0); rj != nil {
		t.Errorf("market order with no reference: %v", rj)
	}
}

func TestCollar(t *testing.T) {
	r := New(Config{Default: Limits{CollarBps: 500}}) // 5%
	ref := oms.Price(6500000)
	if rj := check(r, btc("A", oms.Buy, 6800000, 100), ref); rj != nil { // +4.6%
		t.Errorf("inside collar rejected: %v", rj)
	}
	wantReject(t, check(r, btc("A", oms.Buy, 6900000, 100), ref), "collar")  // +6.2%
	wantReject(t, check(r, btc("A", oms.Sell, 6000000, 100), ref), "collar") // -7.7%
	if rj := check(r, btc("A", oms.Buy, 9000000, 100), 0); rj != nil {
		t.Errorf("no reference price should skip the collar: %v", rj)
	}
}

func TestOpenOrderAndExposureCaps(t *testing.T) {
	r := New(Config{Default: Limits{MaxOpenOrders: 2, MaxGrossExposure: 1500}})
	open := func(px oms.Price, qty oms.Qty) oms.Order {
		o := btc("A", oms.Buy, px, qty)
		o.LeavesQty = qty
		return o
	}
	// One open order worth 650 USD; a new 650 USD order brings the total to 1,300.
	if rj := check(r, btc("A", oms.Buy, 6500000, 10000), 0, open(6500000, 10000)); rj != nil {
		t.Errorf("1,300 USD of 1,500 rejected: %v", rj)
	}
	wantReject(t, check(r, btc("A", oms.Buy, 6500000, 20000), 0, open(6500000, 10000)), "gross exposure")
	wantReject(t, check(r, btc("A", oms.Buy, 6500000, 100), 0, open(1, 100), open(1, 100)), "open order limit")

	// Exposure uses the open orders' remaining quantity, not their original size.
	partly := open(6500000, 100000)
	partly.LeavesQty = 10000
	if rj := check(r, btc("A", oms.Buy, 6500000, 10000), 0, partly); rj != nil {
		t.Errorf("partly filled order counted at original size: %v", rj)
	}
}

func TestPerCounterpartyLimits(t *testing.T) {
	r := New(Config{
		Default:  Limits{MaxOrderNotional: 1_000_000},
		Sessions: map[string]Limits{"TIGHT": {MaxOrderNotional: 100}},
	})
	o := btc("X", oms.Buy, 6500000, 10000)
	if rj := check(r, o, 0); rj != nil {
		t.Errorf("default limits: %v", rj)
	}
	o.Counterparty = "TIGHT"
	wantReject(t, check(r, o, 0), "notional")
}

func TestKillSwitchScopes(t *testing.T) {
	r := New(Config{})
	o := btc("A", oms.Buy, 6500000, 10000)
	r.Halt("A", true)
	wantReject(t, check(r, o, 0), "kill switch")
	o.Counterparty = "B"
	if rj := check(r, o, 0); rj != nil {
		t.Errorf("B halted by A's switch: %v", rj)
	}
	r.Halt("", true)
	wantReject(t, check(r, o, 0), "kill switch")
	r.Halt("", false)
	r.Halt("A", false)
	o.Counterparty = "A"
	if rj := check(r, o, 0); rj != nil {
		t.Errorf("after resume: %v", rj)
	}
}

func TestRateLimiterRefills(t *testing.T) {
	now := time.Unix(1000, 0)
	r := New(Config{Default: Limits{RatePerSec: 2, RateBurst: 3}})
	r.now = func() time.Time { return now }
	o := btc("A", oms.Buy, 6500000, 10000)

	for i := 0; i < 3; i++ {
		if rj := check(r, o, 0); rj != nil {
			t.Fatalf("burst order %d rejected: %v", i, rj)
		}
	}
	wantReject(t, check(r, o, 0), "rate limit")

	now = now.Add(500 * time.Millisecond) // 1 token back at 2/s
	if rj := check(r, o, 0); rj != nil {
		t.Errorf("order after refill rejected: %v", rj)
	}
	wantReject(t, check(r, o, 0), "rate limit")

	now = now.Add(time.Hour) // refill is capped at the burst size
	for i := 0; i < 3; i++ {
		if rj := check(r, o, 0); rj != nil {
			t.Fatalf("order %d after long idle rejected: %v", i, rj)
		}
	}
	wantReject(t, check(r, o, 0), "rate limit")
}

func TestRejectedOrdersStillConsumeTokens(t *testing.T) {
	now := time.Unix(1000, 0)
	r := New(Config{Default: Limits{RatePerSec: 0.001, RateBurst: 2}})
	r.now = func() time.Time { return now }
	bad := btc("A", oms.Buy, 6500025, 10000) // off tick
	wantReject(t, check(r, bad, 0), "tick")
	wantReject(t, check(r, bad, 0), "tick")
	wantReject(t, check(r, bad, 0), "rate limit")
}

func TestNotionalOverflowIsRejectedNotWrapped(t *testing.T) {
	if _, ok := ins["BTC/USD"].Notional(oms.Price(math.MaxInt64), 2); ok {
		t.Error("overflowing notional reported as ok")
	}
	r := New(Config{Default: Limits{MaxOrderNotional: 1000}})
	o := btc("A", oms.Buy, oms.Price(math.MaxInt64/50*50), 100_000_000)
	wantReject(t, check(r, o, 0), "notional")
}

func TestLoadFile(t *testing.T) {
	cfg, err := LoadFile("../../deploy/risk.json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Default.CollarBps != 500 || cfg.Sessions["DEMO"].MaxOrderNotional != 50000 {
		t.Errorf("unexpected config: %+v", cfg)
	}
}
