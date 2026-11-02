package simclient

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/manav2701/fixgw/internal/chaos"
	"github.com/manav2701/fixgw/internal/fixmsg"
	"github.com/manav2701/fixgw/internal/oms"
)

// Result is the outcome of one scenario.
type Result struct {
	Name   string
	Pass   bool
	Detail []string
}

func (r *Result) logf(format string, a ...any) {
	r.Detail = append(r.Detail, fmt.Sprintf(format, a...))
}

func (r *Result) check(ok bool, format string, a ...any) {
	status := "ok  "
	if !ok {
		status, r.Pass = "FAIL", false
	}
	r.Detail = append(r.Detail, status+" "+fmt.Sprintf(format, a...))
}

func newResult(name string) *Result { return &Result{Name: name, Pass: true} }

const (
	symbol  = "BTC/USD"
	timeout = 8 * time.Second
)

// order builds a resting limit order well away from the other side, so one
// client's buys and sells never cross each other: buys at 64,000, sells at 66,000.
func order(id string, side oms.Side) fixmsg.NewOrder {
	px := "64000.00"
	if side == oms.Sell {
		px = "66000.00"
	}
	return limitOrder(id, side, "0.010000", px)
}

func limitOrder(id string, side oms.Side, qty, px string) fixmsg.NewOrder {
	return fixmsg.NewOrder{ClOrdID: id, Symbol: symbol, Side: side, Type: oms.Limit, TIF: oms.GTC, Qty: qty, Price: px}
}

func start(ctx context.Context, cfg Config) (*Client, error) {
	c, err := New(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	if err := c.WaitLogon(ctx, timeout); err != nil {
		c.Stop()
		return nil, fmt.Errorf("logon: %w", err)
	}
	return c, nil
}

// Heartbeat logs on and checks the session stays up across several heartbeat
// intervals.
func Heartbeat(ctx context.Context, cfg Config) *Result {
	res := newResult("heartbeat")
	cfg.HeartBtInt = 1
	c, err := start(ctx, cfg)
	if err != nil {
		res.check(false, "%v", err)
		return res
	}
	defer c.Stop()
	res.check(true, "logged on")
	time.Sleep(3500 * time.Millisecond)
	st := c.Stats()
	res.check(c.LoggedOn(), "still logged on after 3.5s at HeartBtInt=1")
	res.check(st.Heartbeats >= 2, "received %d heartbeats from the gateway", st.Heartbeats)
	return res
}

// Orders exercises the order lifecycle: acknowledge, cancel, and the reject paths.
func Orders(ctx context.Context, cfg Config, n int) *Result {
	res := newResult("orders")
	c, err := start(ctx, cfg)
	if err != nil {
		res.check(false, "%v", err)
		return res
	}
	defer c.Stop()
	defer c.CleanUp(ctx)

	prefix := fmt.Sprintf("O%d-", time.Now().UnixNano()%1_000_000)
	for i := 0; i < n; i++ {
		side := oms.Buy
		if i%2 == 1 {
			side = oms.Sell
		}
		if err := c.SendOrder(order(fmt.Sprintf("%s%d", prefix, i), side)); err != nil {
			res.check(false, "send order %d: %v", i, err)
			return res
		}
	}
	acked := 0
	for i := 0; i < n; i++ {
		r, err := c.WaitAck(ctx, timeout, fmt.Sprintf("%s%d", prefix, i), oms.ExecNew)
		if err == nil && r.OrdStatus == string(rune(oms.StatusNew)) {
			acked++
		}
	}
	res.check(acked == n, "%d/%d orders acknowledged (ExecType=0, OrdStatus=0)", acked, n)

	// Cancel the first order.
	c.SendCancel(prefix+"0", prefix+"C0", symbol, oms.Buy, "0.010000")
	r, err := c.WaitAck(ctx, timeout, prefix+"C0", oms.ExecCanceled)
	res.check(err == nil && r.OrigClOrdID == prefix+"0" && r.OrdStatus == "4",
		"cancel acknowledged (ExecType=4, OrdStatus=4, OrigClOrdID chained)")

	// Cancelling it again is too late: OrderCancelReject, CxlRejReason=0.
	c.SendCancel(prefix+"0", prefix+"C1", symbol, oms.Buy, "0.010000")
	rej := waitReport(ctx, c, func(r Report) bool { return r.MsgType == fixmsg.MsgOrderCancelReject && r.ClOrdID == prefix+"C1" })
	res.check(rej != nil && rej.RejReason == "0", "cancel of canceled order -> 35=9 CxlRejReason=0")

	// Unknown order: CxlRejReason=1.
	c.SendCancel("NO-SUCH-ORDER", prefix+"C2", symbol, oms.Buy, "0.010000")
	rej = waitReport(ctx, c, func(r Report) bool { return r.MsgType == fixmsg.MsgOrderCancelReject && r.ClOrdID == prefix+"C2" })
	res.check(rej != nil && rej.RejReason == "1", "cancel of unknown order -> 35=9 CxlRejReason=1")

	// Duplicate ClOrdID is rejected, not silently accepted.
	c.SendOrder(order(prefix+"1", oms.Buy))
	dup := waitReport(ctx, c, func(r Report) bool {
		return r.MsgType == fixmsg.MsgExecutionReport && r.ClOrdID == prefix+"1" && r.ExecType == "8"
	})
	res.check(dup != nil && dup.RejReason == "6", "duplicate ClOrdID -> ExecType=8, OrdRejReason=6")

	// Unknown symbol.
	bad := order(prefix+"X", oms.Buy)
	bad.Symbol = "DOGE/USD"
	c.SendOrder(bad)
	unk := waitReport(ctx, c, func(r Report) bool {
		return r.MsgType == fixmsg.MsgExecutionReport && r.ClOrdID == prefix+"X" && r.ExecType == "8"
	})
	res.check(unk != nil && unk.RejReason == "1", "unknown symbol -> ExecType=8, OrdRejReason=1")

	// Price with more decimals than the instrument allows.
	prec := order(prefix+"P", oms.Buy)
	prec.Price = "65000.123"
	c.SendOrder(prec)
	pr := waitReport(ctx, c, func(r Report) bool {
		return r.MsgType == fixmsg.MsgExecutionReport && r.ClOrdID == prefix+"P" && r.ExecType == "8"
	})
	res.check(pr != nil, "excess price precision rejected")
	return res
}

func waitReport(ctx context.Context, c *Client, pred func(Report) bool) *Report {
	var got Report
	if err := WaitFor(ctx, timeout, func() bool {
		r, ok := c.Find(pred)
		got = r
		return ok
	}); err != nil {
		return nil
	}
	return &got
}

// Gap creates a real sequence-number gap on the wire and checks recovery.
// A chaos proxy swallows two NewOrderSingle messages; the client believes it
// sent them. The next order arrives with a higher MsgSeqNum than the gateway
// expects, the gateway sends a ResendRequest, and the client replays the
// missing messages with PossDupFlag=Y.
func Gap(ctx context.Context, cfg Config) *Result {
	res := newResult("gap")
	proxy, err := chaos.Listen("127.0.0.1:0", net.JoinHostPort(cfg.Host, cfg.Port))
	if err != nil {
		res.check(false, "proxy: %v", err)
		return res
	}
	defer proxy.Close()
	_, port, _ := net.SplitHostPort(proxy.Addr())
	cfg.Host, cfg.Port = "127.0.0.1", port

	c, err := start(ctx, cfg)
	if err != nil {
		res.check(false, "%v", err)
		return res
	}
	defer c.Stop()
	defer c.CleanUp(ctx)
	res.check(true, "logged on through chaos proxy")

	p := fmt.Sprintf("G%d-", time.Now().UnixNano()%1_000_000)
	ids := []string{p + "1", p + "2", p + "3", p + "4", p + "5", p + "6"}

	for _, id := range ids[:2] {
		c.SendOrder(order(id, oms.Buy))
	}
	for _, id := range ids[:2] {
		if _, err := c.WaitAck(ctx, timeout, id, oms.ExecNew); err != nil {
			res.check(false, "baseline order %s not acknowledged", id)
			return res
		}
	}
	res.check(true, "2 baseline orders acknowledged")

	proxy.DropClientMessages(fixmsg.MsgNewOrderSingle, 2)
	c.SendOrder(order(ids[2], oms.Buy))
	c.SendOrder(order(ids[3], oms.Buy))
	if err := WaitFor(ctx, timeout, func() bool { return proxy.Dropped() == 2 }); err != nil {
		res.check(false, "proxy did not swallow both orders")
		return res
	}
	res.check(true, "proxy swallowed 2 orders (%s, %s); gateway has not seen them", ids[2], ids[3])

	// The next message exposes the gap.
	c.SendOrder(order(ids[4], oms.Buy))
	c.SendOrder(order(ids[5], oms.Buy))

	missing := 0
	for _, id := range ids[2:] {
		if _, err := c.WaitAck(ctx, timeout, id, oms.ExecNew); err == nil {
			missing++
		}
	}
	st := c.Stats()
	res.check(missing == 4, "all 4 post-gap orders acknowledged (%d/4), including the 2 that were swallowed", missing)
	res.check(st.ResendRequests >= 1, "gateway sent ResendRequest (35=2): %d", st.ResendRequests)
	res.check(st.PossDupResent >= 2, "client replayed application messages with PossDupFlag=Y: %d", st.PossDupResent)
	res.logf("admin messages in the range were gap-filled with SequenceReset (35=4): %d", st.SequenceResets)
	res.check(c.LoggedOn(), "session still logged on")
	return res
}

// Reconnect severs the TCP connection mid-session and checks the client logs
// back on and carries on. It does not set ResetSeqNumFlag, so the session
// resumes its sequence numbers; against a gateway with leftover state, start
// the gateway with a fresh store.
func Reconnect(ctx context.Context, cfg Config) *Result {
	res := newResult("reconnect")
	cfg.Reset = false
	proxy, err := chaos.Listen("127.0.0.1:0", net.JoinHostPort(cfg.Host, cfg.Port))
	if err != nil {
		res.check(false, "proxy: %v", err)
		return res
	}
	defer proxy.Close()
	_, port, _ := net.SplitHostPort(proxy.Addr())
	cfg.Host, cfg.Port = "127.0.0.1", port

	c, err := start(ctx, cfg)
	if err != nil {
		res.check(false, "%v", err)
		return res
	}
	defer c.Stop()
	defer c.CleanUp(ctx)

	p := fmt.Sprintf("R%d-", time.Now().UnixNano()%1_000_000)
	c.SendOrder(order(p+"1", oms.Buy))
	_, err = c.WaitAck(ctx, timeout, p+"1", oms.ExecNew)
	res.check(err == nil, "order acknowledged before the cut")

	proxy.KillConnections()
	res.check(WaitFor(ctx, timeout, func() bool { return !c.LoggedOn() }) == nil, "connection severed, session logged out")
	res.check(WaitFor(ctx, 15*time.Second, c.LoggedOn) == nil, "client reconnected and logged on again")

	c.SendOrder(order(p+"2", oms.Sell))
	_, err = c.WaitAck(ctx, timeout, p+"2", oms.ExecNew)
	res.check(err == nil, "order acknowledged after reconnect")
	res.check(c.Stats().Logons >= 2, "logons observed: %d", c.Stats().Logons)
	return res
}

// AuthFail checks that a wrong secret never gets a session established.
func AuthFail(ctx context.Context, cfg Config) *Result {
	res := newResult("authfail")
	cfg.Secret = "definitely-the-wrong-secret"
	c, err := New(cfg)
	if err != nil {
		res.check(false, "%v", err)
		return res
	}
	if err := c.Start(); err != nil {
		res.check(false, "%v", err)
		return res
	}
	defer c.Stop()
	err = c.WaitLogon(ctx, 3*time.Second)
	res.check(err == ErrTimeout, "logon with a bad signature never succeeds")
	return res
}
