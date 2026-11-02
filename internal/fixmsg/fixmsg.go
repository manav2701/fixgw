// Package fixmsg holds FIX tag numbers and builders for the messages the gateway
// and its test client exchange. It works on quickfix's generic Message so no
// code generation is needed.
package fixmsg

import (
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/manav2701/fixgw/internal/oms"
)

const (
	TagAccount          quickfix.Tag = 1
	TagAvgPx            quickfix.Tag = 6
	TagClOrdID          quickfix.Tag = 11
	TagCumQty           quickfix.Tag = 14
	TagExecID           quickfix.Tag = 17
	TagHandlInst        quickfix.Tag = 21
	TagLastPx           quickfix.Tag = 31
	TagLastQty          quickfix.Tag = 32
	TagMsgSeqNum        quickfix.Tag = 34
	TagMsgType          quickfix.Tag = 35
	TagOrderID          quickfix.Tag = 37
	TagOrderQty         quickfix.Tag = 38
	TagOrdStatus        quickfix.Tag = 39
	TagOrdType          quickfix.Tag = 40
	TagOrigClOrdID      quickfix.Tag = 41
	TagPossDupFlag      quickfix.Tag = 43
	TagPrice            quickfix.Tag = 44
	TagSenderCompID     quickfix.Tag = 49
	TagSendingTime      quickfix.Tag = 52
	TagSide             quickfix.Tag = 54
	TagSymbol           quickfix.Tag = 55
	TagTargetCompID     quickfix.Tag = 56
	TagText             quickfix.Tag = 58
	TagTimeInForce      quickfix.Tag = 59
	TagTransactTime     quickfix.Tag = 60
	TagRawDataLength    quickfix.Tag = 95
	TagRawData          quickfix.Tag = 96
	TagCxlRejReason     quickfix.Tag = 102
	TagOrdRejReason     quickfix.Tag = 103
	TagResetSeqNumFlag  quickfix.Tag = 141
	TagExecType         quickfix.Tag = 150
	TagLeavesQty        quickfix.Tag = 151
	TagCxlRejResponseTo quickfix.Tag = 434
	TagUsername         quickfix.Tag = 553
	TagPassword         quickfix.Tag = 554
)

// Message types used by the gateway.
const (
	MsgHeartbeat          = "0"
	MsgTestRequest        = "1"
	MsgResendRequest      = "2"
	MsgReject             = "3"
	MsgSequenceReset      = "4"
	MsgLogout             = "5"
	MsgExecutionReport    = "8"
	MsgOrderCancelReject  = "9"
	MsgLogon              = "A"
	MsgNewOrderSingle     = "D"
	MsgOrderCancelReq     = "F"
	MsgOrderCancelReplace = "G"
)

// Getter is satisfied by *quickfix.Header, *quickfix.Body and *quickfix.Trailer.
type Getter interface {
	GetString(tag quickfix.Tag) (string, quickfix.MessageRejectError)
	GetInt(tag quickfix.Tag) (int, quickfix.MessageRejectError)
}

// Str returns a string field, or "" if it is absent.
func Str(g Getter, tag quickfix.Tag) string {
	s, err := g.GetString(tag)
	if err != nil {
		return ""
	}
	return s
}

// Int returns an integer field, or 0 if it is absent.
func Int(g Getter, tag quickfix.Tag) int {
	n, err := g.GetInt(tag)
	if err != nil {
		return 0
	}
	return n
}

// UTC formats t as a FIX UTCTimestamp with millisecond precision.
func UTC(t time.Time) string { return t.UTC().Format("20060102-15:04:05.000") }

func ch(b byte) string { return string(rune(b)) }

// ExecutionReport builds a 35=8 message. ins supplies decimal formatting for
// prices and quantities.
func ExecutionReport(ins oms.Instrument, ex oms.Execution) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgExecutionReport)
	b := &m.Body
	b.SetString(TagOrderID, ex.OrderID)
	b.SetString(TagExecID, ex.ExecID)
	b.SetString(TagExecType, ch(byte(ex.ExecType)))
	b.SetString(TagOrdStatus, ch(byte(ex.OrdStatus)))
	b.SetString(TagSymbol, ex.Symbol)
	b.SetString(TagSide, ch(byte(ex.Side)))
	b.SetString(TagLeavesQty, ins.FormatQty(ex.LeavesQty))
	b.SetString(TagCumQty, ins.FormatQty(ex.CumQty))
	b.SetString(TagAvgPx, ins.FormatPrice(ex.AvgPx))
	if ex.ClOrdID != "" {
		b.SetString(TagClOrdID, ex.ClOrdID)
	}
	if ex.OrigClOrdID != "" {
		b.SetString(TagOrigClOrdID, ex.OrigClOrdID)
	}
	if ex.OrderQty != 0 {
		b.SetString(TagOrderQty, ins.FormatQty(ex.OrderQty))
	}
	if ex.Price != 0 {
		b.SetString(TagPrice, ins.FormatPrice(ex.Price))
	}
	if ex.ExecType == oms.ExecTrade {
		b.SetString(TagLastQty, ins.FormatQty(ex.LastQty))
		b.SetString(TagLastPx, ins.FormatPrice(ex.LastPx))
	}
	if ex.ExecType == oms.ExecRejected {
		b.SetInt(TagOrdRejReason, int(ex.RejectReason))
	}
	if ex.Text != "" {
		b.SetString(TagText, ex.Text)
	}
	b.SetString(TagTransactTime, UTC(ex.TransactTime))
	return m
}

// OrderCancelReject builds a 35=9 message.
func OrderCancelReject(r oms.CancelReject) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgOrderCancelReject)
	b := &m.Body
	b.SetString(TagOrderID, r.OrderID)
	b.SetString(TagClOrdID, r.ClOrdID)
	b.SetString(TagOrigClOrdID, r.OrigClOrdID)
	b.SetString(TagOrdStatus, ch(byte(r.OrdStatus)))
	resp := r.ResponseTo
	if resp == 0 {
		resp = '1'
	}
	b.SetString(TagCxlRejResponseTo, ch(resp)) // '1' cancel request, '2' replace request
	b.SetInt(TagCxlRejReason, int(r.Reason))
	if r.Text != "" {
		b.SetString(TagText, r.Text)
	}
	return m
}

// NewOrder carries the client-side fields of a 35=D message. Qty and Price are
// decimal strings exactly as they go on the wire.
type NewOrder struct {
	ClOrdID string
	Account string
	Symbol  string
	Side    oms.Side
	Type    oms.OrdType
	TIF     oms.TimeInForce
	Qty     string
	Price   string
}

// NewOrderSingle builds a 35=D message.
func NewOrderSingle(o NewOrder, now time.Time) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgNewOrderSingle)
	b := &m.Body
	b.SetString(TagClOrdID, o.ClOrdID)
	if o.Account != "" {
		b.SetString(TagAccount, o.Account)
	}
	b.SetString(TagHandlInst, "1")
	b.SetString(TagSymbol, o.Symbol)
	b.SetString(TagSide, ch(byte(o.Side)))
	b.SetString(TagTransactTime, UTC(now))
	b.SetString(TagOrderQty, o.Qty)
	b.SetString(TagOrdType, ch(byte(o.Type)))
	if o.Type == oms.Limit {
		b.SetString(TagPrice, o.Price)
	}
	b.SetString(TagTimeInForce, ch(byte(o.TIF)))
	return m
}

// OrderCancelReplaceRequest builds a 35=G message. o.ClOrdID is the new ID;
// qty and price are the new total quantity and limit price.
func OrderCancelReplaceRequest(origClOrdID string, o NewOrder, now time.Time) *quickfix.Message {
	m := NewOrderSingle(o, now)
	m.Header.SetString(TagMsgType, MsgOrderCancelReplace)
	m.Body.SetString(TagOrigClOrdID, origClOrdID)
	return m
}

// OrderCancelRequest builds a 35=F message.
func OrderCancelRequest(origClOrdID, clOrdID, symbol string, side oms.Side, qty string, now time.Time) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgOrderCancelReq)
	b := &m.Body
	b.SetString(TagOrigClOrdID, origClOrdID)
	b.SetString(TagClOrdID, clOrdID)
	b.SetString(TagSymbol, symbol)
	b.SetString(TagSide, ch(byte(side)))
	b.SetString(TagTransactTime, UTC(now))
	b.SetString(TagOrderQty, qty)
	return m
}
