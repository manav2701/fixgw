// Command fixbt backtests a market maker against synthetic order flow on the
// gateway's matching engine, or replays a recorded action log.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/manav2701/fixgw/internal/backtest"
	"github.com/manav2701/fixgw/internal/oms"
)

func main() {
	p := backtest.DefaultParams()
	var (
		replay = flag.String("replay", "", "replay an action log (CSV) instead of simulating")
		dump   = flag.String("dump", "", "write the action log of the run to this CSV file")
	)
	flag.Uint64Var(&p.Seed, "seed", p.Seed, "random seed")
	flag.IntVar(&p.Steps, "steps", p.Steps, "simulation steps")
	flag.IntVar(&p.HalfSpreadTick, "spread", p.HalfSpreadTick, "market maker half-spread, in ticks")
	flag.IntVar(&p.NoisePerStep, "noise", p.NoisePerStep, "noise-trader orders per step")
	flag.Parse()

	start := time.Now()
	var (
		res *backtest.Result
		err error
	)
	if *replay != "" {
		res, err = replayFile(*replay, p)
	} else {
		res, err = backtest.Simulate(p)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixbt:", err)
		os.Exit(1)
	}
	elapsed := time.Since(start)

	if *dump != "" {
		if err := writeLog(*dump, res.Actions); err != nil {
			fmt.Fprintln(os.Stderr, "fixbt:", err)
			os.Exit(1)
		}
	}
	print(res, elapsed)
}

func replayFile(path string, p backtest.Params) (*backtest.Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	actions, err := backtest.ReadCSV(f)
	if err != nil {
		return nil, err
	}
	return backtest.Replay(actions, p.MidStart)
}

func writeLog(path string, actions []backtest.Action) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return backtest.WriteCSV(f, actions)
}

// usd renders a P&L in 10^-8 USD units (BTC/USD: price 2 decimals + qty 6).
func usd(v int64) string { return fmt.Sprintf("%.2f", float64(v)/1e8) }

func print(r *backtest.Result, elapsed time.Duration) {
	fmt.Printf("actions %d   orders %d   trade reports %d   volume %s BTC   rejects %d   cancel rejects %d\n",
		len(r.Actions), r.Orders, r.Trades, oms.FormatDecimal(int64(r.Volume), 6), r.Rejects, r.CancelRej)
	fmt.Printf("replayed in %s (%.0f actions/s)\n\n", elapsed.Round(time.Millisecond), float64(len(r.Actions))/elapsed.Seconds())

	names := make([]string, 0, len(r.Accounts))
	for n := range r.Accounts {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("%-6s %8s %14s %14s %14s\n", "acct", "fills", "position BTC", "max |pos| BTC", "P&L USD")
	var total int64
	for _, n := range names {
		a := r.Accounts[n]
		total += a.PnL(r.Mid)
		fmt.Printf("%-6s %8d %14s %14s %14s\n", n, a.Fills,
			oms.FormatDecimal(a.Position, 6), oms.FormatDecimal(a.MaxAbsPos, 6), usd(a.PnL(r.Mid)))
	}
	fmt.Printf("%-6s %8s %14s %14s %14s   (zero-sum check)\n\n", "total", "", "", "", usd(total))
	fmt.Println("digest", r.Digest)
}
