// Package venue translates gateway orders to external exchanges and back.
//
// A Venue is a thin adapter around one exchange's REST API. Everything that is
// not exchange-specific (symbol mapping, tick and lot conformance, turning
// venue state changes into FIX execution reports, reconciliation) lives in the
// Router, so adding a venue means implementing four methods.
package venue

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/manav2701/fixgw/internal/oms"
)

// Status is a venue-neutral order state.
type Status int

const (
	StatusOpen Status = iota // resting or accepted, no fills yet
	StatusPartial
	StatusFilled
	StatusCanceled
	StatusRejected
	StatusExpired
)

func (s Status) String() string {
	return [...]string{"open", "partial", "filled", "canceled", "rejected", "expired"}[s]
}

// Terminal reports whether no further change is expected.
func (s Status) Terminal() bool { return s >= StatusFilled }

// Request is an order in gateway units, already mapped and validated.
type Request struct {
	ClientID string // unique per order; sent to the venue as its client order ID
	Symbol   string // gateway symbol, e.g. "BTC/USD"
	Side     oms.Side
	Type     oms.OrdType
	TIF      oms.TimeInForce
	Qty      oms.Qty
	Price    oms.Price
}

// State is a venue's view of one order, converted to gateway units.
type State struct {
	VenueID string
	Status  Status
	CumQty  oms.Qty   // total executed so far
	AvgPx   oms.Price // volume-weighted price of the executed part; 0 if none
	Text    string    // venue's reason, for rejects
}

// Ref identifies an order that has already been placed.
type Ref struct {
	ClientID string
	VenueID  string
	Symbol   string
}

// Error is a venue's rejection of a request. It is distinct from a transport
// failure: a *Error means the venue answered "no".
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("venue error %d: %s", e.Code, e.Msg) }

// ErrUnsupported is returned for order shapes a venue cannot express.
var ErrUnsupported = errors.New("venue: unsupported order")

// Venue is one exchange.
type Venue interface {
	Name() string
	// Place submits an order. The returned State reflects the immediate response.
	Place(ctx context.Context, r Request) (State, error)
	// Cancel cancels a placed order. A *Error means the venue refused.
	Cancel(ctx context.Context, ref Ref) error
	// Query returns the order's current state.
	Query(ctx context.Context, ref Ref) (State, error)
}

// Spec maps one gateway symbol to a venue symbol and states the venue's own
// price and quantity grid, which can be coarser than the gateway's.
type Spec struct {
	Symbol      string    // gateway symbol
	VenueSymbol string    // e.g. "BTCUSDT"
	Tick        oms.Price // venue price increment, in gateway price units
	Step        oms.Qty   // venue quantity increment, in gateway qty units
	MinQty      oms.Qty
}

// Check reports why the order does not fit the venue's grid, or nil. Orders are
// rejected rather than silently rounded: changing a client's price or size
// behind their back is worse than telling them.
func (s Spec) Check(o oms.Order, ins oms.Instrument) error {
	if o.Type == oms.Limit && s.Tick > 0 && o.Price%s.Tick != 0 {
		return fmt.Errorf("price not a multiple of venue tick %s", ins.FormatPrice(s.Tick))
	}
	if s.Step > 0 && o.Qty%s.Step != 0 {
		return fmt.Errorf("quantity not a multiple of venue step %s", ins.FormatQty(s.Step))
	}
	if o.Qty < s.MinQty {
		return fmt.Errorf("quantity below venue minimum %s", ins.FormatQty(s.MinQty))
	}
	return nil
}

// parseTrunc parses a non-negative decimal string into units of 10^-decimals,
// truncating digits beyond that precision. Venues report quantities with more
// precision than the gateway keeps; truncating never overstates a fill.
func parseTrunc(s string, decimals int) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > decimals {
		frac = frac[:decimals]
	}
	return oms.ParseDecimal(whole+"."+frac+strings.Repeat("0", decimals-len(frac)), decimals)
}

// avgPx converts a venue's executed quote amount and executed base quantity,
// both decimal strings, into the gateway's price units: quote / base, rounded
// half up.
func avgPx(quote, base string, ins oms.Instrument) oms.Price {
	const scale = 8 // Binance reports 8 decimals
	q, err1 := parseTrunc(quote, scale)
	b, err2 := parseTrunc(base, scale)
	if err1 != nil || err2 != nil || b <= 0 {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(q), pow10(ins.PriceDecimals))
	den := big.NewInt(b)
	num.Add(num, new(big.Int).Rsh(den, 1))
	num.Quo(num, den)
	if !num.IsInt64() {
		return 0
	}
	return oms.Price(num.Int64())
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}
