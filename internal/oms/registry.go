package oms

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrDuplicateClOrdID = errors.New("oms: duplicate ClOrdID")
	ErrUnknownOrder     = errors.New("oms: unknown order")
	ErrTooLate          = errors.New("oms: order already in a terminal state")
	ErrBadFill          = errors.New("oms: fill quantity exceeds leaves")
)

type key struct{ session, clOrdID string }

// Registry is the system of record for orders: it enforces ClOrdID uniqueness
// per session, assigns order and execution IDs, and applies every state change.
// All mutation goes through its methods and happens under one mutex, so
// snapshots (Open, Get) are always consistent. Matching decisions are made by
// the engine; the registry only records their outcome.
type Registry struct {
	mu     sync.Mutex
	orders map[key]*Order    // by (session, current ClOrdID)
	byID   map[string]*Order // by OrderID
	used   map[key]struct{}  // every ClOrdID ever seen, including cancel and replace requests
	nextID atomic.Uint64
	execID atomic.Uint64
	now    func() time.Time
}

func NewRegistry() *Registry {
	return &Registry{
		orders: make(map[key]*Order),
		byID:   make(map[string]*Order),
		used:   make(map[key]struct{}),
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// NextExecID returns a gateway-unique execution ID.
func (r *Registry) NextExecID() string {
	return fmt.Sprintf("X%010d", r.execID.Add(1))
}

// exec builds an execution report from the order's current state.
func (r *Registry) exec(o *Order, et ExecType) Execution {
	return Execution{
		Session:      o.Session,
		ExecID:       r.NextExecID(),
		OrderID:      o.OrderID,
		ClOrdID:      o.ClOrdID,
		Symbol:       o.Symbol,
		Side:         o.Side,
		ExecType:     et,
		OrdStatus:    o.Status,
		OrderQty:     o.Qty,
		Price:        o.Price,
		CumQty:       o.CumQty,
		LeavesQty:    o.LeavesQty,
		AvgPx:        o.AvgPx,
		TransactTime: o.UpdatedAt,
	}
}

// Seen reports whether the ClOrdID has already been used on the session.
func (r *Registry) Seen(session, clOrdID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, dup := r.used[key{session, clOrdID}]
	return dup
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
	r.byID[o.OrderID] = &o
	return r.exec(&o, ExecNew), nil
}

// Reject builds the ExecutionReport for an order that failed validation. The
// ClOrdID is consumed so a client cannot silently retry the same ID.
func (r *Registry) Reject(o Order, reason OrdRejReason, text string) Execution {
	r.mu.Lock()
	r.used[key{o.Session, o.ClOrdID}] = struct{}{}
	r.mu.Unlock()
	return Execution{
		Session:      o.Session,
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

// Fill applies an execution of qty at px to the order and returns the trade
// report addressed to the order's session.
func (r *Registry) Fill(orderID string, qty Qty, px Price) (Execution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.byID[orderID]
	if !ok {
		return Execution{}, ErrUnknownOrder
	}
	if qty <= 0 || qty > o.LeavesQty {
		return Execution{}, ErrBadFill
	}
	next := StatusPartiallyFilled
	if qty == o.LeavesQty {
		next = StatusFilled
	}
	if !CanTransition(o.Status, next) {
		return Execution{}, ErrTooLate
	}
	o.CumQty += qty
	o.LeavesQty -= qty
	o.notional += int64(px) * int64(qty)
	o.AvgPx = Price((o.notional + int64(o.CumQty)/2) / int64(o.CumQty)) // round half up
	o.Status, o.UpdatedAt = next, r.now()

	ex := r.exec(o, ExecTrade)
	ex.LastQty, ex.LastPx = qty, px
	return ex, nil
}

// Close cancels whatever remains of an order on the gateway's initiative: an
// IOC remainder, a self-trade prevention, a kill switch, a disconnect.
func (r *Registry) Close(orderID, text string) (Execution, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.byID[orderID]
	if !ok {
		return Execution{}, ErrUnknownOrder
	}
	if !CanTransition(o.Status, StatusCanceled) {
		return Execution{}, ErrTooLate
	}
	o.Status, o.LeavesQty, o.UpdatedAt = StatusCanceled, 0, r.now()
	ex := r.exec(o, ExecCanceled)
	ex.Text = text
	return ex, nil
}

// Cancel cancels the order identified by origClOrdID on a client's request.
// clOrdID is the ID of the cancel request itself and must be unique. On
// failure it returns a CancelReject describing why.
func (r *Registry) Cancel(session, origClOrdID, clOrdID string) (Execution, *CancelReject) {
	r.mu.Lock()
	defer r.mu.Unlock()

	reject := func(orderID string, st OrdStatus, reason CxlRejReason, text string) *CancelReject {
		return &CancelReject{
			Session: session, ResponseTo: '1', OrderID: orderID, ClOrdID: clOrdID,
			OrigClOrdID: origClOrdID, OrdStatus: st, Reason: reason, Text: text,
		}
	}
	if _, dup := r.used[key{session, clOrdID}]; dup {
		return Execution{}, reject("NONE", StatusRejected, CxlDuplicateID, "duplicate ClOrdID")
	}
	r.used[key{session, clOrdID}] = struct{}{}

	o, ok := r.orders[key{session, origClOrdID}]
	if !ok {
		return Execution{}, reject("NONE", StatusRejected, CxlUnknownOrder, "unknown order")
	}
	if !CanTransition(o.Status, StatusCanceled) {
		return Execution{}, reject(o.OrderID, o.Status, CxlTooLate, "too late to cancel")
	}
	o.Status, o.LeavesQty, o.UpdatedAt = StatusCanceled, 0, r.now()
	ex := r.exec(o, ExecCanceled)
	ex.ClOrdID, ex.OrigClOrdID = clOrdID, origClOrdID
	return ex, nil
}

// Replace applies a cancel/replace to the order identified by origClOrdID. The
// order is re-indexed under newClOrdID, so later requests must reference the
// new ID (FIX ClOrdID chaining). newQty is the new total order quantity.
// Price is only applied to limit orders.
func (r *Registry) Replace(session, origClOrdID, newClOrdID string, newQty Qty, newPx Price) (Order, Execution, *CancelReject) {
	r.mu.Lock()
	defer r.mu.Unlock()

	reject := func(orderID string, st OrdStatus, reason CxlRejReason, text string) (Order, Execution, *CancelReject) {
		return Order{}, Execution{}, &CancelReject{
			Session: session, ResponseTo: '2', OrderID: orderID, ClOrdID: newClOrdID,
			OrigClOrdID: origClOrdID, OrdStatus: st, Reason: reason, Text: text,
		}
	}
	if _, dup := r.used[key{session, newClOrdID}]; dup {
		return reject("NONE", StatusRejected, CxlDuplicateID, "duplicate ClOrdID")
	}
	r.used[key{session, newClOrdID}] = struct{}{}

	o, ok := r.orders[key{session, origClOrdID}]
	if !ok {
		return reject("NONE", StatusRejected, CxlUnknownOrder, "unknown order")
	}
	if o.Status.IsTerminal() {
		return reject(o.OrderID, o.Status, CxlTooLate, "too late to replace")
	}
	if newQty <= o.CumQty {
		return reject(o.OrderID, o.Status, CxlOther, "OrderQty must exceed CumQty")
	}

	delete(r.orders, key{session, origClOrdID})
	o.OrigClOrdID, o.ClOrdID = origClOrdID, newClOrdID
	o.Qty, o.LeavesQty = newQty, newQty-o.CumQty
	if o.Type == Limit {
		o.Price = newPx
	}
	o.UpdatedAt = r.now()
	r.orders[key{session, newClOrdID}] = o

	ex := r.exec(o, ExecReplaced)
	ex.OrigClOrdID = origClOrdID
	return *o, ex, nil
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

// ByID returns a copy of the order with the given OrderID.
func (r *Registry) ByID(orderID string) (Order, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	o, ok := r.byID[orderID]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

// Open returns copies of all non-terminal orders sorted by OrderID.
func (r *Registry) Open() []Order { return r.openWhere(nil) }

// OpenFor returns copies of the session's non-terminal orders.
func (r *Registry) OpenFor(session string) []Order {
	return r.openWhere(func(o *Order) bool { return o.Session == session })
}

// OpenWhere returns copies of the non-terminal orders matching pred.
func (r *Registry) OpenWhere(pred func(Order) bool) []Order {
	return r.openWhere(func(o *Order) bool { return pred(*o) })
}

func (r *Registry) openWhere(pred func(*Order) bool) []Order {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Order
	for _, o := range r.byID {
		if !o.Status.IsTerminal() && (pred == nil || pred(o)) {
			out = append(out, *o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID < out[j].OrderID })
	return out
}

// All returns copies of every order ever accepted, sorted by OrderID.
func (r *Registry) All() []Order {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Order, 0, len(r.byID))
	for _, o := range r.byID {
		out = append(out, *o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OrderID < out[j].OrderID })
	return out
}
