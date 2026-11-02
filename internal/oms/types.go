// Package oms holds the order-management domain: order and execution types,
// the order state machine, and an in-memory order registry.
//
// Prices and quantities are fixed-point integers (see docs/adr/0001).
package oms

import "time"

// Price is a count of 10^-PriceDecimals units for the instrument.
type Price int64

// Qty is a count of 10^-QtyDecimals units for the instrument.
type Qty int64

// Side is FIX tag 54.
type Side byte

const (
	Buy  Side = '1'
	Sell Side = '2'
)

func (s Side) Valid() bool { return s == Buy || s == Sell }

// OrdType is FIX tag 40.
type OrdType byte

const (
	Market OrdType = '1'
	Limit  OrdType = '2'
)

func (t OrdType) Valid() bool { return t == Market || t == Limit }

// TimeInForce is FIX tag 59.
type TimeInForce byte

const (
	Day TimeInForce = '0'
	GTC TimeInForce = '1'
	IOC TimeInForce = '3'
	FOK TimeInForce = '4'
)

func (t TimeInForce) Valid() bool {
	return t == Day || t == GTC || t == IOC || t == FOK
}

// OrdStatus is FIX tag 39: the state of the order.
type OrdStatus byte

const (
	StatusNew             OrdStatus = '0'
	StatusPartiallyFilled OrdStatus = '1'
	StatusFilled          OrdStatus = '2'
	StatusCanceled        OrdStatus = '4'
	StatusPendingCancel   OrdStatus = '6'
	StatusRejected        OrdStatus = '8'
	StatusPendingNew      OrdStatus = 'A'
	StatusExpired         OrdStatus = 'C'
	StatusPendingReplace  OrdStatus = 'E'
)

// ExecType is FIX tag 150: the event that produced an execution report.
// It is distinct from OrdStatus: ExecType describes what just happened,
// OrdStatus describes where the order now stands.
type ExecType byte

const (
	ExecNew      ExecType = '0'
	ExecCanceled ExecType = '4'
	ExecReplaced ExecType = '5'
	ExecRejected ExecType = '8'
	ExecExpired  ExecType = 'C'
	ExecTrade    ExecType = 'F'
)

// OrdRejReason is FIX tag 103.
type OrdRejReason int

const (
	RejBroker           OrdRejReason = 0
	RejUnknownSymbol    OrdRejReason = 1
	RejExchangeClosed   OrdRejReason = 2
	RejExceedsLimit     OrdRejReason = 3
	RejDuplicateOrder   OrdRejReason = 6
	RejUnsupportedOrder OrdRejReason = 11
	RejOther            OrdRejReason = 99
)

// CxlRejReason is FIX tag 102.
type CxlRejReason int

const (
	CxlTooLate        CxlRejReason = 0
	CxlUnknownOrder   CxlRejReason = 1
	CxlAlreadyPending CxlRejReason = 3
	CxlDuplicateID    CxlRejReason = 6
	CxlOther          CxlRejReason = 99
)

// Order is the gateway's view of a client order.
type Order struct {
	OrderID     string // gateway-assigned (FIX tag 37)
	ClOrdID     string // client-assigned (FIX tag 11)
	OrigClOrdID string // set on replaces (FIX tag 41)
	Session     string // owning FIX session
	// Counterparty is the client's CompID. Risk limits are keyed by it.
	Counterparty string
	Account      string
	Symbol       string
	Side         Side
	Type         OrdType
	TIF          TimeInForce
	Qty          Qty
	Price        Price // zero for market orders
	CumQty       Qty
	LeavesQty    Qty
	AvgPx        Price
	Status       OrdStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time

	notional int64 // sum of fill price x fill qty, for AvgPx
}

// Execution is one execution report (FIX 35=8) to be sent to the client.
type Execution struct {
	Session      string // session this report is addressed to
	ExecID       string
	OrderID      string
	ClOrdID      string
	OrigClOrdID  string
	Symbol       string
	Side         Side
	ExecType     ExecType
	OrdStatus    OrdStatus
	OrderQty     Qty
	Price        Price
	LastQty      Qty
	LastPx       Price
	CumQty       Qty
	LeavesQty    Qty
	AvgPx        Price
	RejectReason OrdRejReason
	Text         string
	TransactTime time.Time
}

// CancelReject is one OrderCancelReject (FIX 35=9) to be sent to the client.
type CancelReject struct {
	Session     string
	ResponseTo  byte // CxlRejResponseTo(434): '1' cancel request, '2' replace request
	OrderID     string
	ClOrdID     string
	OrigClOrdID string
	OrdStatus   OrdStatus
	Reason      CxlRejReason
	Text        string
}
