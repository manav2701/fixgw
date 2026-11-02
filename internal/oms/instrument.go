package oms

import (
	"fmt"
	"math"
	"math/bits"
	"sort"
)

// NormDecimals is the common scale notionals are normalised to, so exposure
// can be summed across instruments with different decimals.
const NormDecimals = 8

// Instrument describes a tradable symbol and its fixed-point conventions.
type Instrument struct {
	Symbol        string // FIX tag 55, e.g. "BTC/USD"
	PriceDecimals int
	QtyDecimals   int
	TickSize      Price // minimum price increment, in price units
	LotSize       Qty   // minimum quantity increment, in qty units
	MinQty        Qty
	MaxQty        Qty
}

// Validate checks the instrument's conventions are internally consistent.
func (i Instrument) Validate() error {
	switch {
	case i.Symbol == "":
		return fmt.Errorf("oms: instrument has no symbol")
	case i.PriceDecimals < 0 || i.QtyDecimals < 0 || i.PriceDecimals+i.QtyDecimals > NormDecimals:
		return fmt.Errorf("oms: %s: price+qty decimals must be within 0..%d", i.Symbol, NormDecimals)
	case i.TickSize <= 0 || i.LotSize <= 0:
		return fmt.Errorf("oms: %s: tick and lot size must be positive", i.Symbol)
	case i.MinQty < i.LotSize || i.MaxQty < i.MinQty:
		return fmt.Errorf("oms: %s: need LotSize <= MinQty <= MaxQty", i.Symbol)
	}
	return nil
}

// ParsePrice parses a FIX price string for this instrument.
func (i Instrument) ParsePrice(s string) (Price, error) {
	v, err := ParseDecimal(s, i.PriceDecimals)
	return Price(v), err
}

// ParseQty parses a FIX quantity string for this instrument.
func (i Instrument) ParseQty(s string) (Qty, error) {
	v, err := ParseDecimal(s, i.QtyDecimals)
	return Qty(v), err
}

func (i Instrument) FormatPrice(p Price) string { return FormatDecimal(int64(p), i.PriceDecimals) }
func (i Instrument) FormatQty(q Qty) string     { return FormatDecimal(int64(q), i.QtyDecimals) }

// Notional returns price x qty in quote currency, scaled by 10^NormDecimals.
// ok is false if the value does not fit in an int64.
func (i Instrument) Notional(p Price, q Qty) (n int64, ok bool) {
	if p < 0 || q < 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(uint64(p), uint64(q))
	if hi != 0 || lo > math.MaxInt64 {
		return 0, false
	}
	n = int64(lo)
	for s := NormDecimals - (i.PriceDecimals + i.QtyDecimals); s > 0; s-- {
		if n > math.MaxInt64/10 {
			return 0, false
		}
		n *= 10
	}
	return n, true
}

// Instruments is a symbol-keyed lookup table.
type Instruments map[string]Instrument

// DefaultInstruments returns the demo instrument set.
//
//	BTC/USD  tick 0.50    lot 0.0001   min 0.0001  max 100
//	ETH/USD  tick 0.01    lot 0.0001   min 0.0001  max 10000
//	SOL/USD  tick 0.010   lot 0.01     min 0.01    max 100000
func DefaultInstruments() Instruments {
	list := []Instrument{
		{Symbol: "BTC/USD", PriceDecimals: 2, QtyDecimals: 6, TickSize: 50, LotSize: 100, MinQty: 100, MaxQty: 100_000_000},
		{Symbol: "ETH/USD", PriceDecimals: 2, QtyDecimals: 5, TickSize: 1, LotSize: 10, MinQty: 10, MaxQty: 1_000_000_000},
		{Symbol: "SOL/USD", PriceDecimals: 3, QtyDecimals: 3, TickSize: 10, LotSize: 10, MinQty: 10, MaxQty: 100_000_000},
	}
	m := make(Instruments, len(list))
	for _, in := range list {
		m[in.Symbol] = in
	}
	return m
}

// Symbols returns the known symbols in sorted order.
func (m Instruments) Symbols() []string {
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
