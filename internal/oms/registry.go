package oms

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrDuplicateClOrdID = errors.New("oms: duplicate ClOrdID")
	ErrUnknownOrder     = errors.New("oms: unknown order")
	ErrTooLate          = errors.New("oms: order already in a terminal state")
)

type key struct{ session, clOrdID string }

// Registry tracks live orders per session and enforces ClOrdID uniqueness.
// It is safe for concurrent use.
type Registry struct {
	mu     sync.Mutex
	orders map[key]*Order
	used   map[key]struct{} // every ClOrdID ever seen, including cancel requests
	nextID atomic.Uint64
	execID atomic.Uint64
	now    func() time.Time
}

func NewRegistry() *Registry {
	return &Registry{
		orders: make(map[key]*Order),
		used:   make(map[key]struct{}),
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// NextExecID returns a gateway-unique execution ID.
func (r *Registry) NextExecID() string {
	return fmt.Sprintf("X%010d", r.execID.Add(1))
}

// Accept registers a new order and returns the ExecutionReport acknowledging
// it (ExecType=New, OrdStatus=New). A reused ClOrdID is rejected.
func (r *Registry) Accept(o Order) (Execution, error) {
	k := key{o.Session, o.ClOrdID}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.used[k]; dup {
		return Execution{}, ErrDuplicateClOrdID
	}
	r.used[k] = struct{}{}

	now := r.now()
	o.OrderID = fmt.Sprintf("O%010d", r.nextID.Add(1))
	o.Status = StatusNew
	o.LeavesQty = o.Qty
	o.CreatedAt, o.UpdatedAt = now, now
	r.orders[k] = &o

	return Execution{
		ExecID:       r.NextExecID(),
		OrderID:      o.OrderID,
		ClOrdID:      o.ClOrdID,
		Symbol:       o.Symbol,
		Side:         o.Side,
		ExecType:     ExecNew,
		OrdStatus:    StatusNew,
		OrderQty:     o.Qty,
		Price:        o.Price,
		LeavesQty:    o.LeavesQty,
		TransactTime: now,
	}, nil
}

// Reject builds the ExecutionReport for an order that failed validation. The
// ClOrdID is consumed so a client cannot silently retry the same ID.
func (r *Registry) Reject(o Order, reason OrdRejReason, text string) Execution {
	r.mu.Lock()
	r.used[key{o.Session, o.ClOrdID}] = struct{}{}
	r.mu.Unlock()
	return Execution{
		ExecID:       r.NextExecID(),
		OrderID:      "NONE",
		ClOrdID:      o.ClOrdID,
		Symbol:       o.Symbol,
		Side:         o.Side,
		ExecType:     ExecRejected,
		OrdStatus:    StatusRejected,
		OrderQty:     o.Qty,
		Price:        o.Price,
		RejectReason: reason,
		Text:         text,
		TransactTime: r.now(),
	}
}

// Cancel cancels the order identified by origClOrdID. clOrdID is the ID of the
// cancel request itself and must be unique. On failure it returns a
// CancelReject describing why.
func (r *Registry) Cancel(session, origClOrdID, clOrdID string) (Execution, *CancelReject) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, dup := r.used[key{session, clOrdID}]; dup {
		return Execution{}, &CancelReject{
			OrderID: "NONE", ClOrdID: clOrdID, OrigClOrdID: origClOrdID,
			OrdStatus: StatusRejected, Reason: CxlDuplicateID, Text: "duplicate ClOrdID",
		}
	}
	r.used[key{session, clOrdID}] = struct{}{}

	o, ok := r.orders[key{session, origClOrdID}]
	if !ok {
		return Execution{}, &CancelReject{
			OrderID: "NONE", ClOrdID: clOrdID, OrigClOrdID: origClOrdID,
			OrdStatus: StatusRejected, Reason: CxlUnknownOrder, Text: "unknown order",
		}
	}
	if !CanTransition(o.Status, StatusCanceled) {
		return Execution{}, &CancelReject{
			OrderID: o.OrderID, ClOrdID: clOrdID, OrigClOrdID: origClOrdID,
			OrdStatus: o.Status, Reason: CxlTooLate, Text: "too late to cancel",
		}
	}

	now := r.now()
	o.Status, o.LeavesQty, o.UpdatedAt = StatusCanceled, 0, now
	return Execution{
		ExecID:       r.NextExecID(),
		OrderID:      o.OrderID,
		ClOrdID:      clOrdID,
		OrigClOrdID:  origClOrdID,
		Symbol:       o.Symbol,
		Side:         o.Side,
		ExecType:     ExecCanceled,
		OrdStatus:    StatusCanceled,
		OrderQty:     o.Qty,
		Price:        o.Price,
		CumQty:       o.CumQty,
		AvgPx:        o.AvgPx,
		TransactTime: now,
	}, nil
}

// Get returns a copy of the order, if present.
func (r *Registry) Get(session, clOrdID string) (Order, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.orders[key{session, clOrdID}]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

// Open returns copies of all non-terminal orders, for the blotter.
func (r *Registry) Open() []Order {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Order
	for _, o := range r.orders {
		if !o.Status.IsTerminal() {
			out = append(out, *o)
		}
	}
	return out
}
