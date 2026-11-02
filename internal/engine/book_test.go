package engine

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/manav2701/fixgw/internal/oms"
)

func rest(t *testing.T, b *Book, id, key string, side oms.Side, px oms.Price, qty oms.Qty) {
	t.Helper()
	if err := b.Add(Entry{ID: id, Key: key, Side: side, Price: px, Leaves: qty}); err != nil {
		t.Fatal(err)
	}
	if err := b.Check(); err != nil {
		t.Fatal(err)
	}
}

func limit(id, key string, side oms.Side, px oms.Price, qty oms.Qty) Taker {
	return Taker{ID: id, Key: key, Side: side, Type: oms.Limit, Price: px, Qty: qty}
}

func fillsString(fs []Fill) string {
	s := ""
	for _, f := range fs {
		s += fmt.Sprintf("%s:%d@%d ", f.MakerID, f.Qty, f.Price)
	}
	return s
}

func TestPriceThenTimePriority(t *testing.T) {
	b := NewBook("BTC/USD")
	rest(t, b, "a1", "A", oms.Sell, 101, 5)
	rest(t, b, "a2", "B", oms.Sell, 100, 5) // better price, later
	rest(t, b, "a3", "C", oms.Sell, 100, 5) // same price as a2, later still

	res := b.Match(limit("t", "T", oms.Buy, 101, 12))
	want := "a2:5@100 a3:5@100 a1:2@101 "
	if got := fillsString(res.Fills); got != want {
		t.Errorf("fills = %q, want %q", got, want)
	}
	if res.Remaining != 0 {
		t.Errorf("remaining = %d", res.Remaining)
	}
	if err := b.Check(); err != nil {
		t.Fatal(err)
	}
	if e, ok := b.Get("a1"); !ok || e.Leaves != 3 {
		t.Errorf("a1 should rest with 3, got %+v ok=%v", e, ok)
	}
}

func TestTradesAtMakerPrice(t *testing.T) {
	b := NewBook("BTC/USD")
	rest(t, b, "bid", "A", oms.Buy, 105, 4)
	// An aggressive sell at 100 gets the better price 105 (price improvement).
	res := b.Match(limit("t", "T", oms.Sell, 100, 4))
	if len(res.Fills) != 1 || res.Fills[0].Price != 105 {
		t.Errorf("fills = %q, want one fill at 105", fillsString(res.Fills))
	}
}

func TestLimitDoesNotCrossBeyondItsPrice(t *testing.T) {
	b := NewBook("BTC/USD")
	rest(t, b, "a1", "A", oms.Sell, 100, 5)
	rest(t, b, "a2", "A", oms.Sell, 102, 5)
	res := b.Match(limit("t", "T", oms.Buy, 101, 10))
	if got := fillsString(res.Fills); got != "a1:5@100 " || res.Remaining != 5 {
		t.Errorf("fills=%q remaining=%d", got, res.Remaining)
	}
}

func TestMarketOrderWalksTheBook(t *testing.T) {
	b := NewBook("BTC/USD")
	rest(t, b, "a1", "A", oms.Sell, 100, 3)
	rest(t, b, "a2", "A", oms.Sell, 110, 3)
	res := b.Match(Taker{ID: "t", Key: "T", Side: oms.Buy, Type: oms.Market, Qty: 10})
	if got := fillsString(res.Fills); got != "a1:3@100 a2:3@110 " || res.Remaining != 4 {
		t.Errorf("fills=%q remaining=%d", got, res.Remaining)
	}
	if b.Len() != 0 {
		t.Errorf("book should be empty, has %d", b.Len())
	}
}

func TestSelfTradePrevention(t *testing.T) {
	b := NewBook("BTC/USD")
	rest(t, b, "mine", "ME", oms.Sell, 100, 5)
	rest(t, b, "other", "YOU", oms.Sell, 101, 5)

	res := b.Match(limit("t", "ME", oms.Buy, 101, 8))
	if !res.STP || len(res.Fills) != 0 || res.Remaining != 8 {
		t.Errorf("STP result = %+v", res)
	}
	// The resting order is untouched.
	if e, ok := b.Get("mine"); !ok || e.Leaves != 5 {
		t.Errorf("resting order disturbed: %+v", e)
	}

	// Trading with others first, then meeting your own order, stops there.
	b2 := NewBook("BTC/USD")
	rest(t, b2, "x", "YOU", oms.Sell, 100, 2)
	rest(t, b2, "mine", "ME", oms.Sell, 100, 5)
	res = b2.Match(limit("t", "ME", oms.Buy, 100, 8))
	if got := fillsString(res.Fills); got != "x:2@100 " || !res.STP || res.Remaining != 6 {
		t.Errorf("fills=%q stp=%v remaining=%d", got, res.STP, res.Remaining)
	}
}

func TestAvailableMatchesWhatMatchWouldDo(t *testing.T) {
	b := NewBook("BTC/USD")
	rest(t, b, "a1", "A", oms.Sell, 100, 3)
	rest(t, b, "a2", "ME", oms.Sell, 100, 3)
	rest(t, b, "a3", "A", oms.Sell, 100, 3)

	if got := b.Available(limit("t", "T", oms.Buy, 100, 9)); got != 9 {
		t.Errorf("Available = %d, want 9", got)
	}
	if got := b.Available(limit("t", "ME", oms.Buy, 100, 9)); got != 3 {
		t.Errorf("Available stops at own order: got %d, want 3", got)
	}
	if got := b.Available(limit("t", "T", oms.Buy, 99, 9)); got != 0 {
		t.Errorf("Available below the offer: got %d", got)
	}
	if b.Len() != 3 {
		t.Error("Available must not change the book")
	}
}

func TestReduceKeepsPriorityCancelAndReAddLosesIt(t *testing.T) {
	b := NewBook("BTC/USD")
	rest(t, b, "first", "A", oms.Buy, 100, 10)
	rest(t, b, "second", "B", oms.Buy, 100, 10)

	if !b.Reduce("first", 4) {
		t.Fatal("reduce failed")
	}
	if b.Reduce("first", 4) || b.Reduce("first", 9) || b.Reduce("nope", 1) {
		t.Error("reduce must refuse non-decreasing or unknown")
	}
	res := b.Match(limit("t", "T", oms.Sell, 100, 6))
	if got := fillsString(res.Fills); got != "first:4@100 second:2@100 " {
		t.Errorf("reduced order should keep first place: %q", got)
	}

	// Cancel and re-add moves to the back of the queue.
	b = NewBook("BTC/USD")
	rest(t, b, "first", "A", oms.Buy, 100, 5)
	rest(t, b, "second", "B", oms.Buy, 100, 5)
	e, _ := b.Cancel("first")
	rest(t, b, "first", "A", oms.Buy, 100, e.Leaves)
	res = b.Match(limit("t", "T", oms.Sell, 100, 5))
	if got := fillsString(res.Fills); got != "second:5@100 " {
		t.Errorf("re-added order should queue behind: %q", got)
	}
}

func TestCancelRemovesEmptyLevels(t *testing.T) {
	b := NewBook("BTC/USD")
	rest(t, b, "a", "A", oms.Buy, 100, 1)
	rest(t, b, "b", "A", oms.Buy, 99, 1)
	if _, ok := b.Cancel("a"); !ok {
		t.Fatal("cancel failed")
	}
	if px, _ := b.BestBid(); px != 99 {
		t.Errorf("best bid = %d", px)
	}
	if _, ok := b.Cancel("a"); ok {
		t.Error("double cancel should fail")
	}
	if err := b.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotDepthAndOrder(t *testing.T) {
	b := NewBook("BTC/USD")
	for i, px := range []oms.Price{98, 100, 99} {
		rest(t, b, fmt.Sprintf("b%d", i), "A", oms.Buy, px, 1)
	}
	for i, px := range []oms.Price{103, 101, 102} {
		rest(t, b, fmt.Sprintf("s%d", i), "B", oms.Sell, px, oms.Qty(i+1))
	}
	s := b.Snapshot(2)
	if len(s.Bids) != 2 || s.Bids[0].Price != 100 || s.Bids[1].Price != 99 {
		t.Errorf("bids = %+v", s.Bids)
	}
	if len(s.Asks) != 2 || s.Asks[0].Price != 101 || s.Asks[1].Price != 102 {
		t.Errorf("asks = %+v", s.Asks)
	}
}

// Random operations against a model: after every step the book's invariants hold
// and quantity is conserved. Each unit traded consumes one unit from the taker
// and one from a maker, so
//
//	submitted = 2*traded + cancelled + resting.
func TestRandomOperationsKeepInvariants(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		rng := rand.New(rand.NewSource(seed))
		b := NewBook("BTC/USD")
		var submitted, traded, cancelled oms.Qty
		var resting []string
		next := 0

		for step := 0; step < 2000; step++ {
			switch op := rng.Intn(10); {
			case op < 6: // new order
				next++
				id := fmt.Sprintf("o%d", next)
				side := oms.Buy
				if rng.Intn(2) == 0 {
					side = oms.Sell
				}
				qty := oms.Qty(rng.Intn(20) + 1)
				tk := Taker{ID: id, Key: fmt.Sprintf("k%d", rng.Intn(4)), Side: side, Type: oms.Limit,
					Price: oms.Price(95 + rng.Intn(11)), Qty: qty}
				if rng.Intn(6) == 0 {
					tk.Type = oms.Market
				}
				submitted += qty
				res := b.Match(tk)
				for _, f := range res.Fills {
					traded += f.Qty
				}
				switch {
				case res.Remaining == 0:
				case res.STP || tk.Type == oms.Market || rng.Intn(5) == 0: // cancel remainder
					cancelled += res.Remaining
				default:
					if err := b.Add(Entry{ID: id, Key: tk.Key, Side: side, Price: tk.Price, Leaves: res.Remaining}); err != nil {
						t.Fatal(err)
					}
					resting = append(resting, id)
				}
			case op < 8 && len(resting) > 0: // cancel
				i := rng.Intn(len(resting))
				if e, ok := b.Cancel(resting[i]); ok {
					cancelled += e.Leaves
				}
				resting = append(resting[:i], resting[i+1:]...)
			case len(resting) > 0: // reduce
				id := resting[rng.Intn(len(resting))]
				if e, ok := b.Get(id); ok && e.Leaves > 1 {
					nl := oms.Qty(rng.Intn(int(e.Leaves)-1) + 1)
					cancelled += e.Leaves - nl
					b.Reduce(id, nl)
				}
			}
			if err := b.Check(); err != nil {
				t.Fatalf("seed %d step %d: %v", seed, step, err)
			}
			var onBook oms.Qty
			for _, e := range b.Resting() {
				onBook += e.Leaves
			}
			if submitted != 2*traded+cancelled+onBook {
				t.Fatalf("seed %d step %d: submitted %d != 2*%d + %d + %d", seed, step, submitted, traded, cancelled, onBook)
			}
		}
	}
}
