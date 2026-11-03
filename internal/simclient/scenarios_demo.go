package simclient

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/manav2701/fixgw/internal/fixmsg"
	"github.com/manav2701/fixgw/internal/oms"
)

func fmtPx(p int64) string { return fmt.Sprintf("%d.%02d", p/100, p%100) }

// Demo keeps a live market on the gateway for the dashboard: counterparty A
// quotes three levels each side around a drifting mid, re-quoting every second,
// and B occasionally lifts or hits the touch. It returns when ctx ends, after
// maxRun (the client keeps every report in memory, so a long-lived process
// should restart its sessions periodically), or if either session drops.
func Demo(ctx context.Context, a, b Config, maxRun time.Duration) error {
	ca, err := start(ctx, a)
	if err != nil {
		return fmt.Errorf("maker: %w", err)
	}
	defer ca.Stop()
	cb, err := start(ctx, b)
	if err != nil {
		return fmt.Errorf("taker: %w", err)
	}
	defer cb.Stop()

	const (
		tick   = 50 // 0.50
		qty    = "0.020000"
		hitQty = "0.005000"
	)
	var (
		mid    int64 = 6_500_000
		n      int
		quotes []struct {
			id   string
			side oms.Side
		}
		runID = time.Now().UnixNano() % 1_000_000
	)
	id := func(p string) string { n++; return fmt.Sprintf("%s%d-%d", p, runID, n) }

	tickEvery := time.NewTicker(2 * time.Second)
	defer tickEvery.Stop()
	deadline := time.After(maxRun)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline:
			return nil
		case <-tickEvery.C:
		}
		if !ca.LoggedOn() || !cb.LoggedOn() {
			return fmt.Errorf("session dropped")
		}

		// Re-quote only when the mid moves: the gateway keeps every order in
		// memory, so a demo that runs for days should not churn needlessly.
		step := int64(rand.IntN(3)-1) * tick
		if step == 0 && len(quotes) > 0 {
			maybeHit(cb, id, mid, hitQty)
			continue
		}
		mid = max(mid+step, 6_000_000)

		for _, q := range quotes {
			ca.SendCancel(q.id, id("X"), symbol, q.side, qty)
		}
		quotes = quotes[:0]
		for _, off := range []int64{100, 200, 350} {
			for _, s := range []struct {
				side oms.Side
				px   int64
			}{{oms.Buy, mid - off}, {oms.Sell, mid + off}} {
				qid := id("Q")
				if ca.SendOrder(fixmsg.NewOrder{ClOrdID: qid, Symbol: symbol, Side: s.side, Type: oms.Limit, TIF: oms.GTC, Qty: qty, Price: fmtPx(s.px)}) == nil {
					quotes = append(quotes, struct {
						id   string
						side oms.Side
					}{qid, s.side})
				}
			}
		}

		maybeHit(cb, id, mid, hitQty)
	}
}

// maybeHit now and then has B take the touch with an IOC order.
func maybeHit(cb *Client, id func(string) string, mid int64, qty string) {
	if rand.IntN(3) != 0 {
		return
	}
	side, px := oms.Buy, mid+100
	if rand.IntN(2) == 0 {
		side, px = oms.Sell, mid-100
	}
	cb.SendOrder(fixmsg.NewOrder{ClOrdID: id("T"), Symbol: symbol, Side: side, Type: oms.Limit, TIF: oms.IOC, Qty: qty, Price: fmtPx(px)})
}
