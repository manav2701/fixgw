# ADR 0001: Fixed-point integers for price and quantity

## Status
Accepted

## Context
Order prices and quantities arrive as decimal strings in FIX tags 44 and 38.
`float64` cannot represent most decimals exactly, so sums, comparisons and
price-level map keys drift. A matching engine keyed on a float price would put
`0.1 + 0.2` and `0.3` in different levels.

## Decision
`oms.Price` and `oms.Qty` are `int64` counts of the smallest unit for the
instrument (`10^-PriceDecimals`, `10^-QtyDecimals`). Parsing and formatting go
through `oms.ParseDecimal` / `oms.FormatDecimal`, which never touch floats and
reject input with more precision than the instrument allows. Tick and lot
conformance is a modulo check against `Instrument.TickSize` / `LotSize`.

## Consequences
- Exact arithmetic, safe map keys, cheap comparison.
- Every boundary (FIX in, FIX out, venue adapters) must convert explicitly.
- `int64` range is ample: 8 decimals of precision still allows ~9.2e10 units.
