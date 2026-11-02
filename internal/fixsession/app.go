// Package fixsession is the gateway's FIX acceptor: authenticated logon, order
// entry on top of quickfix's session layer, a durable sequence store, live
// session state, and the raw-message audit hook.
package fixsession

import (
	"errors"
	"log/slog"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/manav2701/fixgw/internal/auth"
	"github.com/manav2701/fixgw/internal/fixmsg"
	"github.com/manav2701/fixgw/internal/oms"
)

// FIX BusinessRejectReason(380) value for an unsupported message type.
const businessRejectUnsupportedMsgType = 3

// App implements quickfix.Application for the acceptor.
type App struct {
	authn       *auth.Authenticator
	orders      *oms.Registry
	instruments oms.Instruments
	sessions    *Registry
	log         *slog.Logger
	now         func() time.Time
}

// NewApp wires the acceptor application.
func NewApp(authn *auth.Authenticator, orders *oms.Registry, ins oms.Instruments, sessions *Registry, log *slog.Logger) *App {
	return &App{
		authn: authn, orders: orders, instruments: ins, sessions: sessions, log: log,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (a *App) OnCreate(sid quickfix.SessionID) {
	a.sessions.Created(sid.String())
}

func (a *App) OnLogon(sid quickfix.SessionID) {
	a.sessions.LoggedOn(sid.String())
	a.log.Info("logon", "session", sid.String())
}

func (a *App) OnLogout(sid quickfix.SessionID) {
	a.sessions.LoggedOut(sid.String())
	a.log.Info("logout", "session", sid.String())
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
		return nil
	case fixmsg.MsgOrderCancelReq:
		a.onCancel(m, sid)
		return nil
	default:
		return quickfix.NewBusinessMessageRejectError(
			"unsupported message type "+mt, businessRejectUnsupportedMsgType, nil)
	}
}

func (a *App) onNewOrder(m *quickfix.Message, sid quickfix.SessionID) {
	b := &m.Body
	o := oms.Order{
		Session: sid.String(),
		ClOrdID: fixmsg.Str(b, fixmsg.TagClOrdID),
		Account: fixmsg.Str(b, fixmsg.TagAccount),
		Symbol:  fixmsg.Str(b, fixmsg.TagSymbol),
		Side:    firstByte[oms.Side](fixmsg.Str(b, fixmsg.TagSide)),
		Type:    firstByte[oms.OrdType](fixmsg.Str(b, fixmsg.TagOrdType)),
		TIF:     oms.Day,
	}
	if tif := fixmsg.Str(b, fixmsg.TagTimeInForce); tif != "" {
		o.TIF = firstByte[oms.TimeInForce](tif)
	}

	ins, known := a.instruments[o.Symbol]
	if !known {
		a.send(sid, ins, a.orders.Reject(o, oms.RejUnknownSymbol, "unknown symbol"))
		return
	}
	reject := func(reason oms.OrdRejReason, text string) {
		a.send(sid, ins, a.orders.Reject(o, reason, text))
	}

	qty, err := ins.ParseQty(fixmsg.Str(b, fixmsg.TagOrderQty))
	if err != nil || qty <= 0 {
		reject(oms.RejOther, "invalid OrderQty")
		return
	}
	o.Qty = qty

	switch {
	case !o.Side.Valid():
		reject(oms.RejOther, "invalid Side")
		return
	case !o.Type.Valid():
		reject(oms.RejUnsupportedOrder, "unsupported OrdType")
		return
	case !o.TIF.Valid():
		reject(oms.RejUnsupportedOrder, "unsupported TimeInForce")
		return
	}
	if o.Type == oms.Limit {
		px, err := ins.ParsePrice(fixmsg.Str(b, fixmsg.TagPrice))
		if err != nil || px <= 0 {
			reject(oms.RejOther, "invalid Price")
			return
		}
		o.Price = px
	}

	ex, err := a.orders.Accept(o)
	if errors.Is(err, oms.ErrDuplicateClOrdID) {
		reject(oms.RejDuplicateOrder, "duplicate ClOrdID")
		return
	}
	a.send(sid, ins, ex)
}

func (a *App) onCancel(m *quickfix.Message, sid quickfix.SessionID) {
	b := &m.Body
	ex, rej := a.orders.Cancel(sid.String(), fixmsg.Str(b, fixmsg.TagOrigClOrdID), fixmsg.Str(b, fixmsg.TagClOrdID))
	if rej != nil {
		if err := quickfix.SendToTarget(fixmsg.OrderCancelReject(*rej), sid); err != nil {
			a.log.Error("send OrderCancelReject", "session", sid.String(), "err", err)
		}
		return
	}
	a.send(sid, a.instruments[ex.Symbol], ex)
}

func (a *App) send(sid quickfix.SessionID, ins oms.Instrument, ex oms.Execution) {
	if err := quickfix.SendToTarget(fixmsg.ExecutionReport(ins, ex), sid); err != nil {
		a.log.Error("send ExecutionReport", "session", sid.String(), "err", err)
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
