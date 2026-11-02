package simclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/manav2701/fixgw/internal/fixmsg"
	"github.com/manav2701/fixgw/internal/oms"
)

// trades returns the trade reports (ExecType=F) for an order, oldest first.
func trades(c *Client, clOrdID string) []Report {
	var out []Report
	for _, r := range c.Reports() {
		if r.MsgType == fixmsg.MsgExecutionReport && r.ClOrdID == clOrdID && r.ExecType == "F" {
			out = append(out, r)
		}
	}
	return out
}

// waitTrades blocks until the order has at least n trade reports.
func waitTrades(ctx context.Context, c *Client, clOrdID string, n int) []Report {
	var got []Report
	WaitFor(ctx, timeout, func() bool {
		got = trades(c, clOrdID)
		return len(got) >= n
	})
	return got
}

func waitExec(ctx context.Context, c *Client, clOrdID, execType string) *Report {
	return waitReport(ctx, c, func(r Report) bool {
		return r.MsgType == fixmsg.MsgExecutionReport && r.ClOrdID == clOrdID && r.ExecType == execType
	})
}

// Trading runs two counterparties against each other: partial fills, trading
// at the resting order's price, self-trade prevention, fill-or-kill, and the
// cancel/replace chain.
func Trading(ctx context.Context, a, b Config) *Result {
	res := newResult("trading")
	ca, err := start(ctx, a)
	if err != nil {
		res.check(false, "client A: %v", err)
		return res
	}
	defer ca.Stop()
	defer ca.CleanUp(ctx)
	cb, err := start(ctx, b)
	if err != nil {
		res.check(false, "client B: %v", err)
		return res
	}
	defer cb.Stop()
	defer cb.CleanUp(ctx)

	p := fmt.Sprintf("T%d-", time.Now().UnixNano()%1_000_000)
	id := func(s string) string { return p + s }

	// 1-2. A rests a sell for 0.01; B buys 0.004 and takes part of it.
	ca.SendOrder(limitOrder(id("s1"), oms.Sell, "0.010000", "65000.00"))
	_, err = ca.WaitAck(ctx, timeout, id("s1"), oms.ExecNew)
	res.check(err == nil, "A rests Sell 0.01 @ 65000.00")

	cb.SendOrder(limitOrder(id("b1"), oms.Buy, "0.004000", "65000.00"))
	bt := waitTrades(ctx, cb, id("b1"), 1)
	at := waitTrades(ctx, ca, id("s1"), 1)
	res.check(len(bt) == 1 && bt[0].OrdStatus == "2" && bt[0].LastQty == "0.004000" && bt[0].LastPx == "65000.00" && bt[0].LeavesQty == "0.000000",
		"B Buy 0.004 fully filled (OrdStatus=2) at 65000.00")
	res.check(len(at) == 1 && at[0].OrdStatus == "1" && at[0].LastQty == "0.004000" && at[0].LeavesQty == "0.006000",
		"A's resting sell partially filled (OrdStatus=1), 0.006 left")

	// 3. B buys more than is left: fills 0.006, rests 0.004.
	cb.SendOrder(limitOrder(id("b2"), oms.Buy, "0.010000", "65000.00"))
	bt = waitTrades(ctx, cb, id("b2"), 1)
	at = waitTrades(ctx, ca, id("s1"), 2)
	res.check(len(bt) == 1 && bt[0].OrdStatus == "1" && bt[0].LastQty == "0.006000" && bt[0].LeavesQty == "0.004000",
		"B Buy 0.01 takes the remaining 0.006 and rests 0.004 (OrdStatus=1)")
	res.check(len(at) == 2 && at[1].OrdStatus == "2" && at[1].CumQty == "0.010000",
		"A's sell now fully filled (OrdStatus=2, CumQty=0.01)")

	// 4. A sells below B's resting bid and trades at B's price: price improvement.
	ca.SendOrder(limitOrder(id("s2"), oms.Sell, "0.004000", "64950.00"))
	at = waitTrades(ctx, ca, id("s2"), 1)
	bt = waitTrades(ctx, cb, id("b2"), 2)
	res.check(len(at) == 1 && at[0].LastPx == "65000.00" && at[0].AvgPx == "65000.00",
		"A sold at 64950.00 limit but executed at resting price 65000.00")
	res.check(len(bt) == 2 && bt[1].OrdStatus == "2" && bt[1].AvgPx == "65000.00",
		"B's resting bid filled (OrdStatus=2)")

	// 5. Self-trade prevention: A's sell would hit A's own bid.
	ca.SendOrder(limitOrder(id("st1"), oms.Buy, "0.001000", "64000.00"))
	ca.WaitAck(ctx, timeout, id("st1"), oms.ExecNew)
	ca.SendOrder(limitOrder(id("st2"), oms.Sell, "0.001000", "64000.00"))
	cx := waitExec(ctx, ca, id("st2"), "4")
	res.check(cx != nil && strings.Contains(cx.Text, "self-trade"), "A crossing its own bid is cancelled: self-trade prevented")
	res.check(len(trades(ca, id("st1"))) == 0 && len(trades(ca, id("st2"))) == 0, "no self-trade was reported")

	// 6. Fill-or-kill with nothing to fill against is cancelled without trading.
	fok := limitOrder(id("f1"), oms.Buy, "0.001000", "66000.00")
	fok.TIF = oms.FOK
	cb.SendOrder(fok)
	cx = waitExec(ctx, cb, id("f1"), "4")
	res.check(cx != nil && cx.OrdStatus == "4" && len(trades(cb, id("f1"))) == 0, "FOK with no liquidity is cancelled, no trade")

	// 7. Cancel/replace chain.
	ca.SendOrder(limitOrder(id("r1"), oms.Buy, "0.002000", "64000.00"))
	ca.WaitAck(ctx, timeout, id("r1"), oms.ExecNew)
	ca.SendReplace(id("r1"), limitOrder(id("r2"), oms.Buy, "0.001000", "64000.00"))
	rp := waitExec(ctx, ca, id("r2"), "5")
	res.check(rp != nil && rp.OrigClOrdID == id("r1") && rp.LeavesQty == "0.001000", "replace r1->r2 (qty 0.002 -> 0.001): ExecType=5, OrigClOrdID chained")
	ca.SendReplace(id("r2"), limitOrder(id("r3"), oms.Buy, "0.001000", "64500.00"))
	rp = waitExec(ctx, ca, id("r3"), "5")
	res.check(rp != nil && rp.OrigClOrdID == id("r2"), "replace r2->r3 (price 64000 -> 64500): ExecType=5")

	ca.SendCancel(id("r1"), id("c1"), symbol, oms.Buy, "0.001000")
	rj := waitReport(ctx, ca, func(r Report) bool { return r.MsgType == fixmsg.MsgOrderCancelReject && r.ClOrdID == id("c1") })
	res.check(rj != nil && rj.RejReason == "1", "cancel by superseded ClOrdID r1 -> 35=9, CxlRejReason=1")
	ca.SendCancel(id("r3"), id("c2"), symbol, oms.Buy, "0.001000")
	cx = waitExec(ctx, ca, id("c2"), "4")
	res.check(cx != nil && cx.OrigClOrdID == id("r3"), "cancel by the latest ClOrdID r3 succeeds")

	ca.SendReplace(id("r3"), limitOrder(id("r4"), oms.Buy, "0.001000", "64500.00"))
	rj = waitReport(ctx, ca, func(r Report) bool { return r.MsgType == fixmsg.MsgOrderCancelReject && r.ClOrdID == id("r4") })
	res.check(rj != nil && rj.RejReason == "0", "replace of a cancelled order -> 35=9, CxlRejReason=0 (too late)")
	return res
}

// Risk checks that each pre-trade control rejects with a specific reason.
func Risk(ctx context.Context, cfg Config) *Result {
	res := newResult("risk")
	c, err := start(ctx, cfg)
	if err != nil {
		res.check(false, "%v", err)
		return res
	}
	defer c.Stop()
	defer c.CleanUp(ctx)

	p := fmt.Sprintf("K%d-", time.Now().UnixNano()%1_000_000)
	// A resting offer gives the collar a reference price when nothing has traded yet.
	c.SendOrder(limitOrder(p+"ref", oms.Sell, "0.010000", "66000.00"))
	c.WaitAck(ctx, timeout, p+"ref", oms.ExecNew)

	expect := func(name, qty, px, want string) {
		id := p + name
		c.SendOrder(limitOrder(id, oms.Buy, qty, px))
		r := waitExec(ctx, c, id, "8")
		res.check(r != nil && strings.Contains(r.Text, want), "%s -> rejected (%q)", name, textOf(r))
	}
	expect("off-tick", "0.010000", "64000.25", "tick")
	expect("off-lot", "0.000150", "64000.00", "lot")
	expect("below-min", "0.000050", "64000.00", "minimum")
	expect("over-notional", "10.000000", "64000.00", "notional")
	expect("fat-finger", "0.010000", "80000.00", "collar")

	ok := p + "fine"
	c.SendOrder(limitOrder(ok, oms.Buy, "0.010000", "64000.00"))
	_, err = c.WaitAck(ctx, timeout, ok, oms.ExecNew)
	res.check(err == nil, "a conforming order is still accepted")
	return res
}

func textOf(r *Report) string {
	if r == nil {
		return "no reject received"
	}
	return r.Text
}
