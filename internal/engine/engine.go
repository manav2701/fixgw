package engine

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/manav2701/fixgw/internal/oms"
	"github.com/manav2701/fixgw/internal/risk"
)

// ErrStopped is returned when the engine is no longer running.
var ErrStopped = errors.New("engine: stopped")

// ReplaceRequest is a cancel/replace (FIX 35=G). Qty is the new total order
// quantity.
type ReplaceRequest struct {
	Session     string
	OrigClOrdID string
	ClOrdID     string
	Qty         oms.Qty
	Price       oms.Price
	Type        oms.OrdType
}

// Router sends orders for some symbols to an external venue instead of the
// internal book. Its methods must not block: they are called on the engine
// goroutine.
type Router interface {
	Handles(symbol string) bool
	// Validate checks an order against the venue's constraints before acceptance.
	Validate(o oms.Order) error
	// Submit hands an accepted order to the venue.
	Submit(o oms.Order)
	// CancelAtVenue cancels, best effort, an order the engine has closed locally.
	CancelAtVenue(orderID string)
}

// Engine serialises all order flow through one goroutine. Every public method
// hands a closure to that goroutine and waits, so books, the registry's
// mutations and risk decisions happen in a single total order. That order is
// what makes the book auditable and replayable; concurrency lives at the
// session layer, not here.
type Engine struct {
	reg   *oms.Registry
	ins   oms.Instruments
	risk  *risk.Risk
	log   *slog.Logger
	books map[string]*Book
	last  map[string]oms.Price // last trade price per symbol

	router Router // optional; set before Start

	cmds chan func()
	quit chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// New builds an engine. Call Start before use.
func New(reg *oms.Registry, ins oms.Instruments, rk *risk.Risk, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{
		reg: reg, ins: ins, risk: rk, log: log,
		books: make(map[string]*Book), last: make(map[string]oms.Price),
		cmds: make(chan func()), quit: make(chan struct{}),
	}
	for sym := range ins {
		e.books[sym] = NewBook(sym)
	}
	return e
}

// SetRouter routes the symbols r handles to an external venue. Call before Start.
func (e *Engine) SetRouter(r Router) { e.router = r }

func (e *Engine) routed(sym string) bool { return e.router != nil && e.router.Handles(sym) }

func (e *Engine) Start() {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			select {
			case f := <-e.cmds:
				f()
			case <-e.quit:
				return
			}
		}
	}()
}

// Stop ends the engine goroutine. It is safe to call more than once.
func (e *Engine) Stop() {
	e.once.Do(func() { close(e.quit) })
	e.wg.Wait()
}

func (e *Engine) do(f func()) error {
	done := make(chan struct{})
	select {
	case e.cmds <- func() { f(); close(done) }:
		<-done
		return nil
	case <-e.quit:
		return ErrStopped
	}
}

// NewOrder validates, risk-checks, acknowledges and matches an order, and
// returns every execution report the action produced, each addressed to its
// own session.
func (e *Engine) NewOrder(o oms.Order) (execs []oms.Execution, err error) {
	err = e.do(func() { execs = e.newOrder(o) })
	return
}

// Cancel cancels a resting order at the client's request.
func (e *Engine) Cancel(session, origClOrdID, clOrdID string) (execs []oms.Execution, rej *oms.CancelReject, err error) {
	err = e.do(func() {
		ex, r := e.reg.Cancel(session, origClOrdID, clOrdID)
		if r != nil {
			rej = r
			return
		}
		e.book(ex.Symbol).Cancel(ex.OrderID)
		if e.routed(ex.Symbol) {
			e.router.CancelAtVenue(ex.OrderID)
		}
		execs = []oms.Execution{ex}
	})
	return
}

// Replace amends a resting order. A price change or quantity increase loses
// time priority (and may trade immediately if it now crosses); a pure quantity
// decrease keeps its place in the queue.
func (e *Engine) Replace(req ReplaceRequest) (execs []oms.Execution, rej *oms.CancelReject, err error) {
	err = e.do(func() { execs, rej = e.replace(req) })
	return
}

// KillSwitch halts (or resumes) trading for scope: a counterparty CompID, or ""
// for everyone. With massCancel it also cancels every matching open order and
// returns the resulting reports.
func (e *Engine) KillSwitch(scope string, on, massCancel bool) (execs []oms.Execution, err error) {
	err = e.do(func() {
		e.risk.Halt(scope, on)
		e.log.Warn("kill switch", "scope", scope, "on", on, "mass_cancel", massCancel)
		if on && massCancel {
			execs = e.massCancel(func(o oms.Order) bool { return scope == "" || scope == "*" || o.Counterparty == scope }, "kill switch")
		}
	})
	return
}

// CancelSession cancels every open order on a session (cancel-on-disconnect).
func (e *Engine) CancelSession(session, text string) (execs []oms.Execution, err error) {
	err = e.do(func() {
		execs = e.massCancel(func(o oms.Order) bool { return o.Session == session }, text)
	})
	return
}

// Snapshot returns the top of a symbol's book.
func (e *Engine) Snapshot(symbol string, depth int) (snap Snapshot, ok bool, err error) {
	err = e.do(func() {
		b, found := e.books[symbol]
		if found {
			snap, ok = b.Snapshot(depth), true
		}
	})
	return
}

// LastTrade returns the last trade price for a symbol.
func (e *Engine) LastTrade(symbol string) (px oms.Price, ok bool, err error) {
	err = e.do(func() { px, ok = e.last[symbol] })
	return
}

// Verify checks every book's invariants.
func (e *Engine) Verify() (err error) {
	if derr := e.do(func() {
		for sym, b := range e.books {
			if cerr := b.Check(); cerr != nil {
				err = fmt.Errorf("%s: %w", sym, cerr)
				return
			}
		}
	}); derr != nil {
		return derr
	}
	return
}

// Everything below runs on the engine goroutine.

func (e *Engine) book(sym string) *Book { return e.books[sym] }

func stpKey(o oms.Order) string {
	if o.Account != "" {
		return o.Account
	}
	return o.Session
}

// refPrice is the price used for collars and market-order notional: the last
// trade, else the midpoint, else whichever side exists.
func (e *Engine) refPrice(sym string) oms.Price {
	if px, ok := e.last[sym]; ok {
		return px
	}
	b := e.books[sym]
	bid, hasBid := b.BestBid()
	ask, hasAsk := b.BestAsk()
	switch {
	case hasBid && hasAsk:
		return (bid + ask) / 2
	case hasBid:
		return bid
	case hasAsk:
		return ask
	}
	return 0
}

func (e *Engine) newOrder(o oms.Order) []oms.Execution {
	if _, ok := e.ins[o.Symbol]; !ok {
		return []oms.Execution{e.reg.Reject(o, oms.RejUnknownSymbol, "unknown symbol")}
	}
	if e.reg.Seen(o.Session, o.ClOrdID) {
		return []oms.Execution{e.reg.Reject(o, oms.RejDuplicateOrder, "duplicate ClOrdID")}
	}
	if rj := e.risk.Check(risk.Input{
		Order: o, Instruments: e.ins, RefPx: e.refPrice(o.Symbol),
		Open: e.reg.OpenForCounterparty(o.Counterparty),
	}); rj != nil {
		return []oms.Execution{e.reg.Reject(o, rj.Reason, rj.Text)}
	}

	routed := e.routed(o.Symbol)
	if routed {
		if verr := e.router.Validate(o); verr != nil {
			return []oms.Execution{e.reg.Reject(o, oms.RejOther, verr.Error())}
		}
	}

	ack, err := e.reg.Accept(o)
	if err != nil {
		return []oms.Execution{e.reg.Reject(o, oms.RejDuplicateOrder, "duplicate ClOrdID")}
	}
	execs := []oms.Execution{ack}
	accepted, _ := e.reg.ByID(ack.OrderID)
	if routed {
		e.router.Submit(accepted) // the venue's answers arrive asynchronously
		return execs
	}
	return e.matchAndRest(execs, accepted)
}

// matchAndRest trades the order's remaining quantity against the book and
// disposes of what is left according to type and time in force.
func (e *Engine) matchAndRest(execs []oms.Execution, ord oms.Order) []oms.Execution {
	b := e.book(ord.Symbol)
	taker := Taker{ID: ord.OrderID, Key: stpKey(ord), Side: ord.Side, Type: ord.Type, Price: ord.Price, Qty: ord.LeavesQty}

	if ord.TIF == oms.FOK && b.Available(taker) < taker.Qty {
		return e.closeOut(execs, ord.OrderID, "FOK order could not be filled in full")
	}

	res := b.Match(taker)
	for _, f := range res.Fills {
		tex, terr := e.reg.Fill(ord.OrderID, f.Qty, f.Price)
		mex, merr := e.reg.Fill(f.MakerID, f.Qty, f.Price)
		if terr != nil || merr != nil {
			e.fault(fmt.Errorf("recording fill %+v: taker: %v, maker: %v", f, terr, merr))
			return execs
		}
		e.last[ord.Symbol] = f.Price
		execs = append(execs, tex, mex)
	}
	if res.Remaining == 0 {
		return execs
	}

	switch {
	case res.STP:
		return e.closeOut(execs, ord.OrderID, "self-trade prevented")
	case ord.Type == oms.Market:
		return e.closeOut(execs, ord.OrderID, "market order remainder canceled: no liquidity")
	case ord.TIF == oms.IOC || ord.TIF == oms.FOK:
		return e.closeOut(execs, ord.OrderID, "IOC remainder canceled")
	}
	if err := b.Add(Entry{ID: ord.OrderID, Key: taker.Key, Side: ord.Side, Price: ord.Price, Leaves: res.Remaining}); err != nil {
		e.fault(err)
	}
	return execs
}

func (e *Engine) closeOut(execs []oms.Execution, orderID, text string) []oms.Execution {
	ex, err := e.reg.Close(orderID, text)
	if err != nil {
		e.fault(fmt.Errorf("closing %s: %w", orderID, err))
		return execs
	}
	return append(execs, ex)
}

// otherOpen is the counterparty's open orders excluding o.
func (e *Engine) otherOpen(o oms.Order) []oms.Order {
	all := e.reg.OpenForCounterparty(o.Counterparty)
	out := all[:0]
	for _, x := range all {
		if x.OrderID != o.OrderID {
			out = append(out, x)
		}
	}
	return out
}

func (e *Engine) replace(req ReplaceRequest) ([]oms.Execution, *oms.CancelReject) {
	rejectWith := func(st oms.OrdStatus, orderID string, reason oms.CxlRejReason, text string) *oms.CancelReject {
		return &oms.CancelReject{
			Session: req.Session, ResponseTo: '2', OrderID: orderID, ClOrdID: req.ClOrdID,
			OrigClOrdID: req.OrigClOrdID, OrdStatus: st, Reason: reason, Text: text,
		}
	}

	cur, ok := e.reg.Get(req.Session, req.OrigClOrdID)
	if ok && e.routed(cur.Symbol) {
		e.reg.Reject(oms.Order{Session: req.Session, ClOrdID: req.ClOrdID}, oms.RejUnsupportedOrder, "replace not supported on routed instruments")
		return nil, rejectWith(cur.Status, cur.OrderID, oms.CxlOther, "replace not supported on routed instruments")
	}
	if ok && !cur.Status.IsTerminal() {
		if req.Type != cur.Type {
			e.reg.Reject(oms.Order{Session: req.Session, ClOrdID: req.ClOrdID}, oms.RejOther, "")
			return nil, rejectWith(cur.Status, cur.OrderID, oms.CxlOther, "OrdType cannot be changed")
		}
		// Risk-check the order as it would look after the replace, against the
		// counterparty's other open orders.
		cand := cur
		cand.Qty, cand.LeavesQty = req.Qty, req.Qty-cur.CumQty
		if cur.Type == oms.Limit {
			cand.Price = req.Price
		}
		if rj := e.risk.Check(risk.Input{
			Order: cand, Instruments: e.ins, RefPx: e.refPrice(cur.Symbol),
			Open: e.otherOpen(cur),
		}); rj != nil {
			e.reg.Reject(oms.Order{Session: req.Session, ClOrdID: req.ClOrdID}, rj.Reason, rj.Text)
			return nil, rejectWith(cur.Status, cur.OrderID, oms.CxlOther, rj.Text)
		}
	}

	updated, ex, rej := e.reg.Replace(req.Session, req.OrigClOrdID, req.ClOrdID, req.Qty, req.Price)
	if rej != nil {
		return nil, rej
	}
	execs := []oms.Execution{ex}

	b := e.book(updated.Symbol)
	entry, resting := b.Get(updated.OrderID)
	samePrice := resting && updated.Type == oms.Limit && entry.Price == updated.Price
	switch {
	case samePrice && updated.LeavesQty < entry.Leaves:
		b.Reduce(updated.OrderID, updated.LeavesQty) // keeps priority
	case samePrice && updated.LeavesQty == entry.Leaves:
		// nothing changed in the book
	default:
		b.Cancel(updated.OrderID) // loses priority; re-enter as a fresh aggressor
		execs = e.matchAndRest(execs, updated)
	}
	return execs, nil
}

func (e *Engine) massCancel(pred func(oms.Order) bool, text string) []oms.Execution {
	var execs []oms.Execution
	for _, o := range e.reg.OpenWhere(pred) {
		e.book(o.Symbol).Cancel(o.OrderID)
		execs = e.closeOut(execs, o.OrderID, text)
		if e.routed(o.Symbol) {
			e.router.CancelAtVenue(o.OrderID)
		}
	}
	return execs
}

// fault handles a broken invariant. Continuing to trade on inconsistent state
// is worse than stopping, so it engages the global kill switch.
func (e *Engine) fault(err error) {
	e.log.Error("matching engine fault: halting all trading", "err", err)
	e.risk.Halt("", true)
}
