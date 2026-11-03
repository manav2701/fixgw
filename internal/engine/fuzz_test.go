package engine

import (
	"fmt"
	"testing"

	"github.com/manav2701/fixgw/internal/oms"
)

// FuzzBook drives a Book with an arbitrary sequence of add, cancel, reduce and
// match operations decoded from the input and checks, after every step, the
// book's structural invariants and that quantity is conserved: what went in
// equals what rests, what traded and what was cancelled.
func FuzzBook(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20})
	f.Add([]byte{3, 3, 3, 3, 1, 1, 1, 1, 2, 2, 2, 2})
	f.Fuzz(func(t *testing.T, data []byte) {
		b := NewBook(sym)
		var (
			in, traded, cancelled oms.Qty
			next                  int
			ids                   []string
		)
		for i := 0; i+3 < len(data); i += 4 {
			op, side, px, qty := data[i]%4, oms.Buy, oms.Price(6_500_000+int64(data[i+1]%8)*50), oms.Qty(100*(1+int64(data[i+2]%20)))
			if data[i+1]&0x80 != 0 {
				side = oms.Sell
			}
			key := fmt.Sprint("k", data[i+3]%3)
			switch op {
			case 0, 1: // add or aggressive order
				next++
				id := fmt.Sprint("o", next)
				res := b.Match(Taker{ID: id, Key: key, Side: side, Type: oms.Limit, Price: px, Qty: qty})
				for _, fl := range res.Fills {
					traded += fl.Qty
				}
				in += qty
				if res.Remaining > 0 && !res.STP {
					if err := b.Add(Entry{ID: id, Key: key, Side: side, Price: px, Leaves: res.Remaining}); err != nil {
						t.Fatal(err)
					}
					ids = append(ids, id)
				} else {
					cancelled += res.Remaining
				}
			case 2: // cancel something
				if len(ids) > 0 {
					id := ids[int(data[i+3])%len(ids)]
					if e, ok := b.Cancel(id); ok {
						cancelled += e.Leaves
					}
				}
			case 3: // reduce something
				if len(ids) > 0 {
					id := ids[int(data[i+3])%len(ids)]
					if e, ok := b.Get(id); ok && e.Leaves > 100 {
						b.Reduce(id, e.Leaves-100)
						cancelled += 100
					}
				}
			}
			if err := b.Check(); err != nil {
				t.Fatalf("step %d: %v", i/4, err)
			}
			var resting oms.Qty
			for _, e := range b.Resting() {
				resting += e.Leaves
			}
			// Each trade consumes its quantity from both the taker and a maker, so
			// it leaves the book twice but entered it once as the taker's size.
			if in != resting+2*traded+cancelled {
				t.Fatalf("step %d: quantity not conserved: in=%d resting=%d traded=%d cancelled=%d", i/4, in, resting, traded, cancelled)
			}
		}
	})
}
