// Package fixsession is the gateway's FIX acceptor: authenticated logon, order
// entry on top of quickfix's session layer, a durable sequence store, live
// session state, and the raw-message audit hook.
package fixsession

import (
	"log/slog"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/manav2701/fixgw/internal/auth"
	"github.com/manav2701/fixgw/internal/engine"
	"github.com/manav2701/fixgw/internal/fixmsg"
	"github.com/manav2701/fixgw/internal/oms"
)

// FIX BusinessRejectReason(380) value for an unsupported message type.
const businessRejectUnsupportedMsgType = 3

// App implements quickfix.Application for the acceptor.
type App struct {
	authn       *auth.Authenticator
	orders      *oms.Registry
	engine      *engine.Engine
	instruments oms.Instruments
	sessions    *Registry
	log         *slog.Logger
	now         func() time.Time

	cancelOnDisconnect bool
	sids               sync.Map // session string -> quickfix.SessionID
}

// NewApp wires the acceptor application.
func NewApp(authn *auth.Authenticator, orders *oms.Registry, eng *engine.Engine, ins oms.Instruments,
	sessions *Registry, cancelOnDisconnect bool, log *slog.Logger) *App {
	return &App{
		authn: authn, orders: orders, engine: eng, instruments: ins, sessions: sessions,
		cancelOnDisconnect: cancelOnDisconnect, log: log,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (a *App) OnCreate(sid quickfix.SessionID) {
	a.sids.Store(sid.String(), sid)
	a.sessions.Created(sid.String())
}

func (a *App) OnLogon(sid quickfix.SessionID) {
	a.sessions.LoggedOn(sid.String())
	a.log.Info("logon", "session", sid.String())
}

func (a *App) OnLogout(sid quickfix.SessionID) {
	a.sessions.LoggedOut(sid.String())
	a.log.Info("logout", "session", sid.String())
	if a.cancelOnDisconnect {
		execs, err := a.engine.CancelSession(sid.String(), "cancel on disconnect")
		if err != nil {
			a.log.Error("cancel on disconnect", "session", sid.String(), "err", err)
			return
		}
		a.Dispatch(execs)
	}
}

func (a *App) ToAdmin(m *quickfix.Message, sid quickfix.SessionID) {
	a.sessions.Out(sid.String(), m)
}

func (a *App) ToApp(m *quickfix.Message, sid quickfix.SessionID) error {
	a.sessions.Out(sid.String(), m)
	return nil
}

// FromAdmin authenticates Logon messages. Every other admin message is just
// counted; quickfix handles heartbeats, test requests and resends itself.
func (a *App) FromAdmin(m *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	key := sid.String()
	a.sessions.In(key, m)
	if fixmsg.Str(&m.Header, fixmsg.TagMsgType) != fixmsg.MsgLogon {
		return nil
	}
	err := a.authn.Verify(auth.LogonFields{
		SenderCompID: fixmsg.Str(&m.Header, fixmsg.TagSenderCompID),
		TargetCompID: fixmsg.Str(&m.Header, fixmsg.TagTargetCompID),
		SendingTime:  fixmsg.Str(&m.Header, fixmsg.TagSendingTime),
		SeqNum:       fixmsg.Int(&m.Header, fixmsg.TagMsgSeqNum),
		Username:     fixmsg.Str(&m.Body, fixmsg.TagUsername),
		Password:     fixmsg.Str(&m.Body, fixmsg.TagPassword),
		Signature:    fixmsg.Str(&m.Body, fixmsg.TagRawData),
	}, a.now())
	if err != nil {
		a.sessions.AuthFailed(key)
		// The detail stays in our log; the client only learns that logon failed.
		a.log.Warn("logon rejected", "session", key, "reason", err.Error())
		return quickfix.RejectLogon{Text: "logon rejected"}
	}
	return nil
}

// FromApp routes application messages.
func (a *App) FromApp(m *quickfix.Message, sid quickfix.SessionID) quickfix.MessageRejectError {
	a.sessions.In(sid.String(), m)
	switch mt := fixmsg.Str(&m.Header, fixmsg.TagMsgType); mt {
	case fixmsg.MsgNewOrderSingle:
		a.onNewOrder(m, sid)
	case fixmsg.MsgOrderCancelReq:
		a.onCancel(m, sid)
	case fixmsg.MsgOrderCancelReplace:
		a.onReplace(m, sid)
	default:
		return quickfix.NewBusinessMessageRejectError(
			"unsupported message type "+mt, businessRejectUnsupportedMsgType, nil)
	}
	return nil
}

// orderFields are the values shared by NewOrderSingle and OrderCancelReplaceRequest.
type orderFields struct {
	order oms.Order
	ins   oms.Instrument
}

// parseOrder reads and validates the order fields of a 35=D or 35=G message.
// On failure it returns the reason and text for a reject.
func (a *App) parseOrder(m *quickfix.Message, sid quickfix.SessionID) (orderFields, oms.OrdRejReason, string, bool) {
	b := &m.Body
	o := oms.Order{
		Session:      sid.String(),
		Counterparty: sid.TargetCompID,
		ClOrdID:      fixmsg.Str(b, fixmsg.TagClOrdID),
		Account:      fixmsg.Str(b, fixmsg.TagAccount),
		Symbol:       fixmsg.Str(b, fixmsg.TagSymbol),
		Side:         firstByte[oms.Side](fixmsg.Str(b, fixmsg.TagSide)),
		Type:         firstByte[oms.OrdType](fixmsg.Str(b, fixmsg.TagOrdType)),
		TIF:          oms.Day,
	}
	if tif := fixmsg.Str(b, fixmsg.TagTimeInForce); tif != "" {
		o.TIF = firstByte[oms.TimeInForce](tif)
	}
	f := orderFields{order: o}

	ins, known := a.instruments[o.Symbol]
	if !known {
		return f, oms.RejUnknownSymbol, "unknown symbol", false
	}
	f.ins = ins
	qty, err := ins.ParseQty(fixmsg.Str(b, fixmsg.TagOrderQty))
	if err != nil || qty <= 0 {
		return f, oms.RejOther, "invalid OrderQty", false
	}
	f.order.Qty = qty

	switch {
	case !o.Side.Valid():
		return f, oms.RejOther, "invalid Side", false
	case !o.Type.Valid():
		return f, oms.RejUnsupportedOrder, "unsupported OrdType", false
	case !o.TIF.Valid():
		return f, oms.RejUnsupportedOrder, "unsupported TimeInForce", false
	}
	if o.Type == oms.Limit {
		px, err := ins.ParsePrice(fixmsg.Str(b, fixmsg.TagPrice))
		if err != nil || px <= 0 {
			return f, oms.RejOther, "invalid Price", false
		}
		f.order.Price = px
	}
	return f, 0, "", true
}

func (a *App) onNewOrder(m *quickfix.Message, sid quickfix.SessionID) {
	f, reason, text, ok := a.parseOrder(m, sid)
	if !ok {
		a.Dispatch([]oms.Execution{a.orders.Reject(f.order, reason, text)})
		return
	}
	execs, err := a.engine.NewOrder(f.order)
	if err != nil {
		a.log.Error("engine unavailable", "err", err)
		return
	}
	a.Dispatch(execs)
}

func (a *App) onCancel(m *quickfix.Message, sid quickfix.SessionID) {
	b := &m.Body
	execs, rej, err := a.engine.Cancel(sid.String(), fixmsg.Str(b, fixmsg.TagOrigClOrdID), fixmsg.Str(b, fixmsg.TagClOrdID))
	if err != nil {
		a.log.Error("engine unavailable", "err", err)
		return
	}
	if rej != nil {
		a.sendCancelReject(sid, *rej)
		return
	}
	a.Dispatch(execs)
}

func (a *App) onReplace(m *quickfix.Message, sid quickfix.SessionID) {
	b := &m.Body
	orig := fixmsg.Str(b, fixmsg.TagOrigClOrdID)
	f, _, text, ok := a.parseOrder(m, sid)
	if !ok {
		a.sendCancelReject(sid, oms.CancelReject{
			Session: sid.String(), ResponseTo: '2', OrderID: "NONE", ClOrdID: f.order.ClOrdID,
			OrigClOrdID: orig, OrdStatus: oms.StatusRejected, Reason: oms.CxlOther, Text: text,
		})
		return
	}
	execs, rej, err := a.engine.Replace(engine.ReplaceRequest{
		Session: sid.String(), OrigClOrdID: orig, ClOrdID: f.order.ClOrdID,
		Qty: f.order.Qty, Price: f.order.Price, Type: f.order.Type,
	})
	if err != nil {
		a.log.Error("engine unavailable", "err", err)
		return
	}
	if rej != nil {
		a.sendCancelReject(sid, *rej)
		return
	}
	a.Dispatch(execs)
}

// Dispatch sends each execution report to the session it is addressed to. One
// engine action can address several sessions (a trade reaches both sides).
func (a *App) Dispatch(execs []oms.Execution) {
	for _, ex := range execs {
		v, ok := a.sids.Load(ex.Session)
		if !ok {
			a.log.Warn("execution for unknown session", "session", ex.Session, "exec_id", ex.ExecID)
			continue
		}
		sid := v.(quickfix.SessionID)
		if err := quickfix.SendToTarget(fixmsg.ExecutionReport(a.instruments[ex.Symbol], ex), sid); err != nil {
			a.log.Error("send ExecutionReport", "session", ex.Session, "exec_id", ex.ExecID, "err", err)
		}
	}
}

func (a *App) sendCancelReject(sid quickfix.SessionID, r oms.CancelReject) {
	if err := quickfix.SendToTarget(fixmsg.OrderCancelReject(r), sid); err != nil {
		a.log.Error("send OrderCancelReject", "session", sid.String(), "err", err)
	}
}

// firstByte converts the first byte of a FIX single-character field to a typed
// enum, or 0 for an empty value.
func firstByte[T ~uint8](s string) T {
	if s == "" {
		return 0
	}
	return T(s[0])
}
