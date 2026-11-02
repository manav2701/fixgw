package oms

import "sort"

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

// Instruments is a symbol-keyed lookup table.
type Instruments map[string]Instrument

// DefaultInstruments returns the demo instrument set.
func DefaultInstruments() Instruments {
	list := []Instrument{
		{Symbol: "BTC/USD", PriceDecimals: 2, QtyDecimals: 6, TickSize: 1, LotSize: 1, MinQty: 1, MaxQty: 100_000_000},
		{Symbol: "ETH/USD", PriceDecimals: 2, QtyDecimals: 5, TickSize: 1, LotSize: 1, MinQty: 1, MaxQty: 1_000_000_000},
		{Symbol: "SOL/USD", PriceDecimals: 3, QtyDecimals: 3, TickSize: 1, LotSize: 1, MinQty: 1, MaxQty: 10_000_000_000},
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
