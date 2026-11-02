// Package risk implements pre-trade controls: kill switch, rate limiting,
// instrument conformance, per-order notional, price collar, and per-counterparty
// open-order and gross-exposure caps.
//
// Limits are keyed by counterparty CompID with a default. Notional values are
// whole quote-currency units.
package risk

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/manav2701/fixgw/internal/oms"
)

// Limits are the caps applied to one counterparty.
type Limits struct {
	MaxOrderNotional int64   `json:"max_order_notional"` // per order, quote units
	MaxGrossExposure int64   `json:"max_gross_exposure"` // sum over open orders, quote units
	MaxOpenOrders    int     `json:"max_open_orders"`
	CollarBps        int64   `json:"collar_bps"`   // max distance of a limit price from the reference price
	RatePerSec       float64 `json:"rate_per_sec"` // sustained orders per second; 0 disables
	RateBurst        int     `json:"rate_burst"`
}

// Config holds default limits and per-counterparty overrides.
type Config struct {
	Default  Limits            `json:"default"`
	Sessions map[string]Limits `json:"sessions"`
}

// DefaultConfig returns conservative demo limits.
func DefaultConfig() Config {
	return Config{Default: Limits{
		MaxOrderNotional: 250_000,
		MaxGrossExposure: 2_000_000,
		MaxOpenOrders:    200,
		CollarBps:        500,
		RatePerSec:       50,
		RateBurst:        100,
	}}
}

// LoadFile reads a JSON risk configuration.
func LoadFile(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("risk: parse %s: %w", path, err)
	}
	return c, nil
}

func (c Config) limitsFor(counterparty string) Limits {
	if l, ok := c.Sessions[counterparty]; ok {
		return l
	}
	return c.Default
}

// Reject is a failed pre-trade check.
type Reject struct {
	Reason oms.OrdRejReason
	Text   string
}

func (r *Reject) Error() string { return r.Text }

// Input is everything a check needs. Open must be the counterparty's other
// open orders, excluding the order being checked (matters for replaces).
type Input struct {
	Order       oms.Order
	Instruments oms.Instruments
	RefPx       oms.Price // reference price for collar and market-order notional; 0 if unknown
	Open        []oms.Order
}

const scopeAll = "*"

type bucket struct {
	tokens float64
	last   time.Time
}

// Risk evaluates orders against the configured limits. It is safe for
// concurrent use.
type Risk struct {
	cfg Config
	now func() time.Time

	mu      sync.Mutex
	halted  map[string]bool
	buckets map[string]*bucket
}

func New(cfg Config) *Risk {
	return &Risk{cfg: cfg, now: time.Now, halted: make(map[string]bool), buckets: make(map[string]*bucket)}
}

// Halt turns the kill switch on or off. scope is a counterparty CompID, or ""
// (or "*") for everyone. While halted, new orders and replaces are rejected;
// cancels are still allowed so clients can reduce risk.
func (r *Risk) Halt(scope string, on bool) {
	if scope == "" {
		scope = scopeAll
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if on {
		r.halted[scope] = true
	} else {
		delete(r.halted, scope)
	}
}

// Halted reports whether new orders from the counterparty are currently blocked.
func (r *Risk) Halted(counterparty string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.halted[scopeAll] || r.halted[counterparty]
}

// allow takes one token from the counterparty's bucket.
func (r *Risk) allow(counterparty string, l Limits) bool {
	if l.RatePerSec <= 0 {
		return true
	}
	burst := float64(max(l.RateBurst, 1))
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[counterparty]
	if !ok {
		b = &bucket{tokens: burst, last: now}
		r.buckets[counterparty] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*l.RatePerSec)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func reject(reason oms.OrdRejReason, format string, a ...any) *Reject {
	return &Reject{Reason: reason, Text: fmt.Sprintf(format, a...)}
}

// Check runs the pre-trade checks in order and returns the first failure, or nil.
// Cheap, global checks come first; every attempt, including ones that fail
// later checks, consumes a rate-limit token so rejected orders are not free.
func (r *Risk) Check(in Input) *Reject {
	o := in.Order
	cp := o.Counterparty
	l := r.cfg.limitsFor(cp)

	if r.Halted(cp) {
		return reject(oms.RejBroker, "trading halted (kill switch)")
	}
	if !r.allow(cp, l) {
		return reject(oms.RejExceedsLimit, "rate limit exceeded")
	}

	ins, ok := in.Instruments[o.Symbol]
	if !ok {
		return reject(oms.RejUnknownSymbol, "unknown symbol")
	}
	qty := o.Qty
	switch {
	case qty < ins.MinQty:
		return reject(oms.RejExceedsLimit, "quantity below minimum %s", ins.FormatQty(ins.MinQty))
	case qty > ins.MaxQty:
		return reject(oms.RejExceedsLimit, "quantity above maximum %s", ins.FormatQty(ins.MaxQty))
	case qty%ins.LotSize != 0:
		return reject(oms.RejOther, "quantity not a multiple of lot size %s", ins.FormatQty(ins.LotSize))
	}
	if o.Type == oms.Limit && o.Price%ins.TickSize != 0 {
		return reject(oms.RejOther, "price not a multiple of tick size %s", ins.FormatPrice(ins.TickSize))
	}

	// Price used for notional: the limit price, or the reference for market orders.
	px := o.Price
	if o.Type == oms.Market {
		px = in.RefPx
	}
	if px > 0 && l.MaxOrderNotional > 0 {
		n, ok := ins.Notional(px, qty)
		if !ok || n > l.MaxOrderNotional*pow10(oms.NormDecimals) {
			return reject(oms.RejExceedsLimit, "order notional exceeds limit %d", l.MaxOrderNotional)
		}
	}

	if o.Type == oms.Limit && in.RefPx > 0 && l.CollarBps > 0 {
		dev := int64(o.Price) - int64(in.RefPx)
		if dev < 0 {
			dev = -dev
		}
		if dev*10_000/int64(in.RefPx) > l.CollarBps {
			return reject(oms.RejExceedsLimit, "price outside %d bps collar around %s",
				l.CollarBps, ins.FormatPrice(in.RefPx))
		}
	}

	if l.MaxOpenOrders > 0 && len(in.Open) >= l.MaxOpenOrders {
		return reject(oms.RejExceedsLimit, "open order limit %d reached", l.MaxOpenOrders)
	}
	if l.MaxGrossExposure > 0 && o.Type == oms.Limit {
		limit := l.MaxGrossExposure * pow10(oms.NormDecimals)
		total, ok := ins.Notional(o.Price, qty)
		for _, open := range in.Open {
			oi, known := in.Instruments[open.Symbol]
			if !known {
				continue
			}
			n, nok := oi.Notional(open.Price, open.LeavesQty)
			if !nok {
				ok = false
				break
			}
			total += n
			if total < 0 {
				ok = false
				break
			}
		}
		if !ok || total > limit {
			return reject(oms.RejExceedsLimit, "gross exposure limit %d exceeded", l.MaxGrossExposure)
		}
	}
	return nil
}

func pow10(n int) int64 {
	v := int64(1)
	for ; n > 0; n-- {
		v *= 10
	}
	return v
}
