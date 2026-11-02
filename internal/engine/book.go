// Package engine is the matching engine: a price-time-priority limit order book
// (Book) and the single-threaded orchestrator around it (Engine) that applies
// risk checks, records outcomes in the order registry and produces execution
// reports.
package engine

import (
	"container/list"
	"fmt"
	"sort"

	"github.com/manav2701/fixgw/internal/oms"
)

// Entry is a resting order in the book.
type Entry struct {
	ID     string // OrderID
	Key    string // self-trade-prevention key: orders with the same key never trade
	Side   oms.Side
	Price  oms.Price
	Leaves oms.Qty
}

// Taker is an incoming order to be matched.
type Taker struct {
	ID    string
	Key   string
	Side  oms.Side
	Type  oms.OrdType
	Price oms.Price // ignored for market orders
	Qty   oms.Qty
}

// Fill is one execution between a taker and a resting maker. It always happens
// at the maker's price.
type Fill struct {
	MakerID string
	Price   oms.Price
	Qty     oms.Qty
}

// MatchResult is the outcome of matching one taker.
type MatchResult struct {
	Fills     []Fill
	Remaining oms.Qty
	// STP is set when matching stopped because the taker met a resting order
	// with its own key. The taker's remainder must not rest or trade further.
	STP bool
}

type level struct {
	price  oms.Price
	qty    oms.Qty
	orders *list.List // of *Entry, oldest first
}

// side holds one half of the book. prices is kept ascending for both sides;
// the best price is the last element for bids and the first for asks.
type side struct {
	bids   bool
	levels map[oms.Price]*level
	prices []oms.Price
}

func newSide(bids bool) *side {
	return &side{bids: bids, levels: make(map[oms.Price]*level)}
}

func (s *side) best() (*level, bool) {
	if len(s.prices) == 0 {
		return nil, false
	}
	if s.bids {
		return s.levels[s.prices[len(s.prices)-1]], true
	}
	return s.levels[s.prices[0]], true
}

// ordered returns prices from best to worst.
func (s *side) ordered() []oms.Price {
	out := make([]oms.Price, len(s.prices))
	for i, p := range s.prices {
		if s.bids {
			out[len(out)-1-i] = p
		} else {
			out[i] = p
		}
	}
	return out
}

func (s *side) level(p oms.Price) *level {
	if l, ok := s.levels[p]; ok {
		return l
	}
	l := &level{price: p, orders: list.New()}
	s.levels[p] = l
	i := sort.Search(len(s.prices), func(i int) bool { return s.prices[i] >= p })
	s.prices = append(s.prices, 0)
	copy(s.prices[i+1:], s.prices[i:])
	s.prices[i] = p
	return l
}

func (s *side) remove(p oms.Price) {
	delete(s.levels, p)
	i := sort.Search(len(s.prices), func(i int) bool { return s.prices[i] >= p })
	if i < len(s.prices) && s.prices[i] == p {
		s.prices = append(s.prices[:i], s.prices[i+1:]...)
	}
}

type ref struct {
	elem *list.Element
	lvl  *level
	side *side
}

// Book is a limit order book for one symbol. It is not safe for concurrent
// use: the Engine confines each Book to its own goroutine, which is what keeps
// matching deterministic and replayable.
type Book struct {
	Symbol string
	bids   *side
	asks   *side
	idx    map[string]ref
}

func NewBook(symbol string) *Book {
	return &Book{Symbol: symbol, bids: newSide(true), asks: newSide(false), idx: make(map[string]ref)}
}

func (b *Book) sideOf(s oms.Side) *side {
	if s == oms.Buy {
		return b.bids
	}
	return b.asks
}

func (b *Book) opposite(s oms.Side) *side {
	if s == oms.Buy {
		return b.asks
	}
	return b.bids
}

// Len is the number of resting orders.
func (b *Book) Len() int { return len(b.idx) }

// BestBid returns the highest resting bid price.
func (b *Book) BestBid() (oms.Price, bool) {
	if l, ok := b.bids.best(); ok {
		return l.price, true
	}
	return 0, false
}

// BestAsk returns the lowest resting ask price.
func (b *Book) BestAsk() (oms.Price, bool) {
	if l, ok := b.asks.best(); ok {
		return l.price, true
	}
	return 0, false
}

// Add rests an order at the back of its price level's queue.
func (b *Book) Add(e Entry) error {
	if e.Leaves <= 0 || !e.Side.Valid() {
		return fmt.Errorf("engine: cannot rest %s: leaves=%d side=%c", e.ID, e.Leaves, e.Side)
	}
	if _, dup := b.idx[e.ID]; dup {
		return fmt.Errorf("engine: order %s is already resting", e.ID)
	}
	s := b.sideOf(e.Side)
	l := s.level(e.Price)
	entry := e
	el := l.orders.PushBack(&entry)
	l.qty += e.Leaves
	b.idx[e.ID] = ref{elem: el, lvl: l, side: s}
	return nil
}

// Get returns a copy of a resting order.
func (b *Book) Get(id string) (Entry, bool) {
	r, ok := b.idx[id]
	if !ok {
		return Entry{}, false
	}
	return *r.elem.Value.(*Entry), true
}

// Cancel removes a resting order and returns it.
func (b *Book) Cancel(id string) (Entry, bool) {
	r, ok := b.idx[id]
	if !ok {
		return Entry{}, false
	}
	e := r.elem.Value.(*Entry)
	r.lvl.orders.Remove(r.elem)
	r.lvl.qty -= e.Leaves
	if r.lvl.orders.Len() == 0 {
		r.side.remove(r.lvl.price)
	}
	delete(b.idx, id)
	return *e, true
}

// Reduce lowers a resting order's remaining quantity in place, keeping its
// time priority. newLeaves must be positive and smaller than the current value.
func (b *Book) Reduce(id string, newLeaves oms.Qty) bool {
	r, ok := b.idx[id]
	if !ok {
		return false
	}
	e := r.elem.Value.(*Entry)
	if newLeaves <= 0 || newLeaves >= e.Leaves {
		return false
	}
	r.lvl.qty -= e.Leaves - newLeaves
	e.Leaves = newLeaves
	return true
}

func crosses(taker oms.Side, takerPx, makerPx oms.Price) bool {
	if taker == oms.Buy {
		return takerPx >= makerPx
	}
	return takerPx <= makerPx
}

func (t Taker) accepts(makerPx oms.Price) bool {
	return t.Type == oms.Market || crosses(t.Side, t.Price, makerPx)
}

// Match trades the taker against the opposite side in price-time priority and
// returns the fills. It does not rest the remainder; the caller decides what
// to do with it based on time in force.
func (b *Book) Match(t Taker) MatchResult {
	res := MatchResult{Remaining: t.Qty}
	opp := b.opposite(t.Side)

	for res.Remaining > 0 {
		lvl, ok := opp.best()
		if !ok || !t.accepts(lvl.price) {
			break
		}
		for el := lvl.orders.Front(); el != nil && res.Remaining > 0; {
			maker := el.Value.(*Entry)
			if maker.Key == t.Key {
				res.STP = true
				return res
			}
			q := min(res.Remaining, maker.Leaves)
			res.Fills = append(res.Fills, Fill{MakerID: maker.ID, Price: lvl.price, Qty: q})
			maker.Leaves -= q
			lvl.qty -= q
			res.Remaining -= q

			next := el.Next()
			if maker.Leaves == 0 {
				lvl.orders.Remove(el)
				delete(b.idx, maker.ID)
			}
			el = next
		}
		if lvl.orders.Len() == 0 {
			opp.remove(lvl.price)
		}
	}
	return res
}

// Available returns how much of the taker's quantity could be filled right now,
// without changing the book. It stops at a self-trade, as Match would.
func (b *Book) Available(t Taker) oms.Qty {
	opp := b.opposite(t.Side)
	var total oms.Qty
	for _, p := range opp.ordered() {
		if !t.accepts(p) {
			break
		}
		for el := opp.levels[p].orders.Front(); el != nil; el = el.Next() {
			maker := el.Value.(*Entry)
			if maker.Key == t.Key {
				return total
			}
			total += maker.Leaves
			if total >= t.Qty {
				return t.Qty
			}
		}
	}
	return total
}

// Level is one aggregated price level.
type Level struct {
	Price  oms.Price `json:"price"`
	Qty    oms.Qty   `json:"qty"`
	Orders int       `json:"orders"`
}

// Snapshot is the top of the book.
type Snapshot struct {
	Symbol string  `json:"symbol"`
	Bids   []Level `json:"bids"` // best first
	Asks   []Level `json:"asks"` // best first
}

// Snapshot returns up to depth levels per side.
func (b *Book) Snapshot(depth int) Snapshot {
	snap := Snapshot{Symbol: b.Symbol, Bids: []Level{}, Asks: []Level{}}
	collect := func(s *side) []Level {
		out := []Level{}
		for _, p := range s.ordered() {
			if len(out) == depth {
				break
			}
			l := s.levels[p]
			out = append(out, Level{Price: p, Qty: l.qty, Orders: l.orders.Len()})
		}
		return out
	}
	snap.Bids, snap.Asks = collect(b.bids), collect(b.asks)
	return snap
}

// Resting returns every resting order, for tests and reconciliation.
func (b *Book) Resting() []Entry {
	out := make([]Entry, 0, len(b.idx))
	for _, r := range b.idx {
		out = append(out, *r.elem.Value.(*Entry))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Check verifies the book's structural invariants. It is meant for tests and
// for the engine's fault detection, not the hot path.
func (b *Book) Check() error {
	total := 0
	for _, s := range []*side{b.bids, b.asks} {
		if len(s.prices) != len(s.levels) {
			return fmt.Errorf("side bids=%v: %d prices but %d levels", s.bids, len(s.prices), len(s.levels))
		}
		for i, p := range s.prices {
			if i > 0 && s.prices[i-1] >= p {
				return fmt.Errorf("side bids=%v: prices not strictly ascending", s.bids)
			}
			l, ok := s.levels[p]
			if !ok || l.price != p {
				return fmt.Errorf("side bids=%v: level %d missing or mislabeled", s.bids, p)
			}
			if l.orders.Len() == 0 {
				return fmt.Errorf("side bids=%v: empty level %d left in book", s.bids, p)
			}
			var sum oms.Qty
			for el := l.orders.Front(); el != nil; el = el.Next() {
				e := el.Value.(*Entry)
				if e.Leaves <= 0 {
					return fmt.Errorf("order %s rests with leaves %d", e.ID, e.Leaves)
				}
				if e.Price != p {
					return fmt.Errorf("order %s priced %d sits in level %d", e.ID, e.Price, p)
				}
				if (e.Side == oms.Buy) != s.bids {
					return fmt.Errorf("order %s on the wrong side", e.ID)
				}
				if r, ok := b.idx[e.ID]; !ok || r.elem != el {
					return fmt.Errorf("order %s missing from index", e.ID)
				}
				sum += e.Leaves
				total++
			}
			if sum != l.qty {
				return fmt.Errorf("level %d: qty %d but orders sum to %d", p, l.qty, sum)
			}
		}
	}
	if total != len(b.idx) {
		return fmt.Errorf("index has %d entries but book holds %d orders", len(b.idx), total)
	}
	bid, hasBid := b.BestBid()
	ask, hasAsk := b.BestAsk()
	if hasBid && hasAsk && bid >= ask {
		return fmt.Errorf("crossed book: bid %d >= ask %d", bid, ask)
	}
	return nil
}
