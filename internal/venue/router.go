package venue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/manav2701/fixgw/internal/oms"
)

// Binding connects a gateway symbol to the venue that trades it.
type Binding struct {
	Venue Venue
	Spec  Spec
}

// Config assembles a Router.
type Config struct {
	Registry    *oms.Registry
	Instruments oms.Instruments
	Bindings    []Binding

	// Emit delivers execution reports to FIX sessions; EmitCancelReject delivers
	// OrderCancelRejects. Events receives venue and reconciliation events for the
	// dashboard. Any may be nil.
	Emit             func([]oms.Execution)
	EmitCancelReject func(oms.CancelReject)
	Events           func(kind string, data any)

	PollInterval   time.Duration // default 1s
	RequestTimeout time.Duration // default 10s
	Logger         *slog.Logger
}

// Router sends orders for routed symbols to their venue and turns the venue's
// answers into the same execution reports the internal engine would produce.
//
// Placement and cancellation are asynchronous, so the engine goroutine never
// waits on the network. A polling loop reconciles each live order against the
// venue and is the only source of fills.
type Router struct {
	cfg   Config
	runID string
	log   *slog.Logger
	by    map[string]Binding

	applyMu sync.Mutex // serialises apply

	mu   sync.Mutex
	live map[string]*liveOrder // by OrderID

	quit chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

type liveOrder struct {
	ref    Ref
	venue  Venue
	placed chan struct{} // closed once the placement attempt has finished
	ok     bool          // placement succeeded; valid after placed is closed
}

func NewRouter(cfg Config) *Router {
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	var id [4]byte
	rand.Read(id[:])
	r := &Router{
		cfg: cfg, runID: hex.EncodeToString(id[:]), log: cfg.Logger,
		by: map[string]Binding{}, live: map[string]*liveOrder{}, quit: make(chan struct{}),
	}
	for _, b := range cfg.Bindings {
		r.by[b.Spec.Symbol] = b
	}
	return r
}

// Start begins the reconciliation loop.
func (r *Router) Start() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		t := time.NewTicker(r.cfg.PollInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				r.Reconcile()
			case <-r.quit:
				return
			}
		}
	}()
}

// Stop ends the loop and waits for in-flight venue calls. Orders left open at
// the venue stay open; the gateway does not cancel them on shutdown.
func (r *Router) Stop() {
	r.once.Do(func() { close(r.quit) })
	r.wg.Wait()
}

// Handles reports whether a symbol is routed to a venue.
func (r *Router) Handles(symbol string) bool {
	_, ok := r.by[symbol]
	return ok
}

// VenueName returns the venue a symbol is routed to, or "".
func (r *Router) VenueName(symbol string) string {
	if b, ok := r.by[symbol]; ok {
		return b.Venue.Name()
	}
	return ""
}

// Validate checks an order against the venue's grid before it is accepted.
func (r *Router) Validate(o oms.Order) error {
	b, ok := r.by[o.Symbol]
	if !ok {
		return fmt.Errorf("symbol %s is not routed", o.Symbol)
	}
	return b.Spec.Check(o, r.cfg.Instruments[o.Symbol])
}

func (r *Router) event(kind string, data any) {
	if r.cfg.Events != nil {
		r.cfg.Events(kind, data)
	}
}

func (r *Router) emit(execs ...oms.Execution) {
	if r.cfg.Emit != nil && len(execs) > 0 {
		r.cfg.Emit(execs)
	}
}

func (r *Router) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), r.cfg.RequestTimeout)
}

// Submit places an already-accepted order at its venue. It returns immediately.
func (r *Router) Submit(o oms.Order) {
	b := r.by[o.Symbol]
	lo := &liveOrder{
		ref:    Ref{ClientID: fmt.Sprintf("fixgw%s-%s", r.runID, o.OrderID), Symbol: o.Symbol},
		venue:  b.Venue,
		placed: make(chan struct{}),
	}
	r.mu.Lock()
	r.live[o.OrderID] = lo
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx, cancel := r.ctx()
		defer cancel()
		st, err := b.Venue.Place(ctx, Request{
			ClientID: lo.ref.ClientID, Symbol: o.Symbol, Side: o.Side, Type: o.Type, TIF: o.TIF,
			Qty: o.Qty, Price: o.Price,
		})
		if err != nil {
			close(lo.placed)
			r.placeFailed(o, b.Venue, err)
			return
		}
		lo.ref.VenueID, lo.ok = st.VenueID, true
		close(lo.placed) // from here Cancel and Reconcile may use lo
		r.event("venue", map[string]any{"event": "placed", "venue": b.Venue.Name(), "order_id": o.OrderID,
			"cl_ord_id": o.ClOrdID, "venue_id": st.VenueID, "status": st.Status.String()})
		r.apply(o.OrderID, b.Venue.Name(), st)
	}()
}

// placeFailed closes the order locally. The client has already seen the New
// acknowledgement, so the failure is reported as a cancel carrying the venue's
// reason rather than as a reject.
func (r *Router) placeFailed(o oms.Order, v Venue, err error) {
	var ve *Error
	text := "venue unavailable: " + err.Error()
	if errors.As(err, &ve) || errors.Is(err, ErrUnsupported) {
		text = "rejected by venue: " + err.Error()
	}
	r.log.Warn("venue placement failed", "venue", v.Name(), "order", o.OrderID, "err", err)
	r.event("venue", map[string]any{"event": "place_failed", "venue": v.Name(), "order_id": o.OrderID, "text": text})
	r.forget(o.OrderID)
	if ex, cerr := r.cfg.Registry.Close(o.OrderID, text); cerr == nil {
		r.emit(ex)
	}
}

func (r *Router) forget(orderID string) {
	r.mu.Lock()
	delete(r.live, orderID)
	r.mu.Unlock()
}

// apply brings the local order in line with the venue's state and emits the
// resulting reports. It is idempotent: applying the same state twice is a no-op.
func (r *Router) apply(orderID, venueName string, st State) {
	// Placement, the reconcile loop and cancel handling can all arrive here for
	// the same order; the read-compute-fill sequence below must not interleave.
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	reg := r.cfg.Registry
	cur, ok := reg.ByID(orderID)
	if !ok {
		return
	}
	ins := r.cfg.Instruments[cur.Symbol]

	if st.CumQty > cur.CumQty {
		if cur.Status.IsTerminal() {
			// The venue executed something the gateway already closed (a kill
			// switch cancelled locally while the order was still live). It cannot
			// be un-done; surface it so an operator can act.
			r.log.Error("reconciliation break: venue fill on a closed order",
				"order", orderID, "local_status", string(cur.Status), "venue_cum", st.CumQty, "local_cum", cur.CumQty)
			r.event("venue", map[string]any{"event": "break", "venue": venueName, "order_id": orderID,
				"text": fmt.Sprintf("venue executed %s but order is %c locally", ins.FormatQty(st.CumQty), cur.Status)})
			r.forget(orderID)
			return
		}
		dq := min(st.CumQty-cur.CumQty, cur.LeavesQty)
		// Price of this increment, from the change in total executed value.
		px := st.AvgPx
		if cur.CumQty > 0 && st.CumQty > cur.CumQty {
			total := int64(st.AvgPx)*int64(st.CumQty) - int64(cur.AvgPx)*int64(cur.CumQty)
			if inc := total / int64(st.CumQty-cur.CumQty); inc > 0 {
				px = oms.Price(inc)
			}
		}
		if px <= 0 {
			px = cur.Price
		}
		if ex, err := reg.Fill(orderID, dq, px); err != nil {
			r.log.Error("recording venue fill", "order", orderID, "err", err)
		} else {
			r.emit(ex)
			r.event("venue", map[string]any{"event": "fill", "venue": venueName, "order_id": orderID,
				"qty": ins.FormatQty(dq), "px": ins.FormatPrice(px)})
		}
	}

	switch st.Status {
	case StatusFilled:
		r.forget(orderID)
	case StatusCanceled, StatusRejected, StatusExpired:
		r.forget(orderID)
		text := "venue " + st.Status.String()
		if st.Text != "" {
			text += ": " + st.Text
		}
		if ex, err := reg.Close(orderID, text); err == nil {
			r.emit(ex)
		}
	}
}

// Reconcile polls the venue for every live order and applies what it finds.
func (r *Router) Reconcile() {
	type item struct {
		id string
		lo *liveOrder
	}
	r.mu.Lock()
	items := make([]item, 0, len(r.live))
	for id, lo := range r.live {
		items = append(items, item{id, lo})
	}
	r.mu.Unlock()

	for _, it := range items {
		select {
		case <-it.lo.placed:
		default:
			continue // placement still in flight
		}
		if !it.lo.ok {
			continue
		}
		ctx, cancel := r.ctx()
		st, err := it.lo.venue.Query(ctx, it.lo.ref)
		cancel()
		if err != nil {
			r.log.Warn("venue query failed", "venue", it.lo.venue.Name(), "order", it.id, "err", err)
			continue
		}
		r.apply(it.id, it.lo.venue.Name(), st)
	}
}

// Owns reports whether the order a session refers to is routed to a venue.
func (r *Router) Owns(session, clOrdID string) bool {
	o, ok := r.cfg.Registry.Get(session, clOrdID)
	return ok && r.Handles(o.Symbol)
}

// Cancel handles a client's cancel request for a routed order. It returns
// immediately; the outcome arrives through Emit or EmitCancelReject.
func (r *Router) Cancel(session, origClOrdID, clOrdID string) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		reg := r.cfg.Registry
		reject := func(rej *oms.CancelReject) {
			if r.cfg.EmitCancelReject != nil {
				r.cfg.EmitCancelReject(*rej)
			}
		}
		o, ok := reg.Get(session, origClOrdID)
		if !ok || o.Status.IsTerminal() {
			// The registry produces the right reject (unknown order, too late).
			if _, rej := reg.Cancel(session, origClOrdID, clOrdID); rej != nil {
				reject(rej)
			}
			return
		}

		r.mu.Lock()
		lo := r.live[o.OrderID]
		r.mu.Unlock()
		if lo != nil {
			<-lo.placed // never race the placement
			if lo.ok {
				ctx, cancel := r.ctx()
				err := lo.venue.Cancel(ctx, lo.ref)
				cancel()
				var ve *Error
				if err != nil && !errors.As(err, &ve) {
					// Transport failure: we do not know whether it was cancelled.
					r.refuse(session, o, origClOrdID, clOrdID, oms.CxlOther, "venue unavailable, cancel not confirmed")
					return
				}
				if err != nil {
					// The venue said no, usually because the order just filled or
					// was cancelled. Bring local state up to date, then answer.
					ctx, cancel := r.ctx()
					if st, qerr := lo.venue.Query(ctx, lo.ref); qerr == nil {
						r.apply(o.OrderID, lo.venue.Name(), st)
					}
					cancel()
					r.refuse(session, o, origClOrdID, clOrdID, oms.CxlTooLate, "venue refused cancel: "+ve.Msg)
					return
				}
			}
		}
		ex, rej := reg.Cancel(session, origClOrdID, clOrdID)
		if rej != nil {
			reject(rej)
			return
		}
		r.forget(o.OrderID)
		r.emit(ex)
	}()
}

// refuse sends an OrderCancelReject, consuming the request's ClOrdID.
func (r *Router) refuse(session string, o oms.Order, orig, clOrdID string, reason oms.CxlRejReason, text string) {
	r.cfg.Registry.Reject(oms.Order{Session: session, ClOrdID: clOrdID}, oms.RejOther, text)
	cur, _ := r.cfg.Registry.ByID(o.OrderID)
	if r.cfg.EmitCancelReject != nil {
		r.cfg.EmitCancelReject(oms.CancelReject{
			Session: session, ResponseTo: '1', OrderID: o.OrderID, ClOrdID: clOrdID,
			OrigClOrdID: orig, OrdStatus: cur.Status, Reason: reason, Text: text,
		})
	}
}

// RejectReplace answers a replace request for a routed order. Venues differ too
// much in amend semantics to emulate them faithfully, so this is refused rather
// than approximated.
func (r *Router) RejectReplace(session, origClOrdID, clOrdID string) oms.CancelReject {
	r.cfg.Registry.Reject(oms.Order{Session: session, ClOrdID: clOrdID}, oms.RejUnsupportedOrder, "replace not supported on routed instruments")
	o, _ := r.cfg.Registry.Get(session, origClOrdID)
	return oms.CancelReject{
		Session: session, ResponseTo: '2', OrderID: o.OrderID, ClOrdID: clOrdID, OrigClOrdID: origClOrdID,
		OrdStatus: o.Status, Reason: oms.CxlOther, Text: "replace not supported on routed instruments; cancel and re-enter",
	}
}

// CancelAtVenue cancels, best effort, a routed order the gateway has already
// closed locally (kill switch, cancel-on-disconnect). It returns immediately.
func (r *Router) CancelAtVenue(orderID string) {
	r.mu.Lock()
	lo := r.live[orderID]
	r.mu.Unlock()
	if lo == nil {
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		<-lo.placed
		if !lo.ok {
			return
		}
		ctx, cancel := r.ctx()
		defer cancel()
		if err := lo.venue.Cancel(ctx, lo.ref); err != nil {
			r.log.Error("venue cancel after local close failed", "order", orderID, "err", err)
			r.event("venue", map[string]any{"event": "cancel_failed", "venue": lo.venue.Name(), "order_id": orderID, "text": err.Error()})
			return
		}
		// Keep the order live for one more poll so a fill that raced the cancel
		// is noticed and reported as a break.
	}()
}
