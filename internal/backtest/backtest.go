// Package backtest drives the matching engine with a deterministic stream of
// orders and measures what happened.
//
// The engine is deterministic by design (ADR 0004): the same actions in the same
// order give the same fills. That makes two things possible here. A simulation
// can be recorded as an action log and replayed exactly, and a digest of every
// execution report proves two runs were identical.
package backtest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math/rand/v2"

	"github.com/manav2701/fixgw/internal/engine"
	"github.com/manav2701/fixgw/internal/oms"
	"github.com/manav2701/fixgw/internal/risk"
)

// Symbol is the instrument every backtest trades.
const Symbol = "BTC/USD"

// Action is one input to the engine.
type Action struct {
	Op          string // "new" or "cancel"
	Account     string
	ClOrdID     string
	OrigClOrdID string // cancel only
	Side        oms.Side
	Type        oms.OrdType
	TIF         oms.TimeInForce
	Price       oms.Price
	Qty         oms.Qty
}

// Account is one participant's result.
type Account struct {
	Position  int64 // signed, in qty units
	Cash      int64 // signed, in price x qty units
	Fills     int
	MaxAbsPos int64
}

// PnL marks the position to mid. The unit is 10^-(price+qty decimals) USD.
func (a Account) PnL(mid oms.Price) int64 { return a.Cash + a.Position*int64(mid) }

// Result summarises a run.
type Result struct {
	Actions   []Action
	Orders    int
	Trades    int // trade reports; each trade appears once per side
	Volume    oms.Qty
	Rejects   int
	CancelRej int
	Accounts  map[string]*Account
	Mid       oms.Price // final mid used for marking
	Digest    string    // hash of every execution report, in order
}

// Runner feeds actions to a private engine and accumulates results.
type Runner struct {
	eng *engine.Engine
	res *Result
	h   hash.Hash
}

// NewRunner starts an engine with risk limits wide enough not to interfere.
func NewRunner() *Runner {
	eng := engine.New(oms.NewRegistry(), oms.DefaultInstruments(),
		risk.New(risk.Config{Default: risk.Limits{MaxOrderNotional: 1e12, MaxGrossExposure: 1e15, MaxOpenOrders: 1 << 30}}),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	eng.Start()
	return &Runner{eng: eng, res: &Result{Accounts: map[string]*Account{}}, h: sha256.New()}
}

// Close stops the engine.
func (r *Runner) Close() { r.eng.Stop() }

func (r *Runner) account(name string) *Account {
	a := r.res.Accounts[name]
	if a == nil {
		a = &Account{}
		r.res.Accounts[name] = a
	}
	return a
}

// Apply runs one action and returns the reports it produced.
func (r *Runner) Apply(a Action) ([]oms.Execution, error) {
	r.res.Actions = append(r.res.Actions, a)
	var execs []oms.Execution
	switch a.Op {
	case "new":
		r.res.Orders++
		var err error
		execs, err = r.eng.NewOrder(oms.Order{
			Session: a.Account, Counterparty: a.Account, ClOrdID: a.ClOrdID, Symbol: Symbol,
			Side: a.Side, Type: a.Type, TIF: a.TIF, Qty: a.Qty, Price: a.Price,
		})
		if err != nil {
			return nil, err
		}
	case "cancel":
		ex, rej, err := r.eng.Cancel(a.Account, a.OrigClOrdID, a.ClOrdID)
		if err != nil {
			return nil, err
		}
		if rej != nil {
			r.res.CancelRej++
			fmt.Fprintf(r.h, "cxlrej|%s|%s|%d\n", rej.Session, rej.ClOrdID, rej.Reason)
		}
		execs = ex
	default:
		return nil, fmt.Errorf("backtest: unknown op %q", a.Op)
	}
	for _, e := range execs {
		r.record(e)
	}
	return execs, nil
}

// record folds one report into the digest and the account ledgers. Wall-clock
// timestamps are left out of the digest: they are the one non-deterministic field.
func (r *Runner) record(e oms.Execution) {
	fmt.Fprintf(r.h, "%s|%s|%s|%s|%c|%c|%d|%d|%d|%d|%d|%d|%s\n",
		e.Session, e.ExecID, e.OrderID, e.ClOrdID, e.ExecType, e.OrdStatus,
		e.LastQty, e.LastPx, e.CumQty, e.LeavesQty, e.AvgPx, e.RejectReason, e.Text)
	switch e.ExecType {
	case oms.ExecRejected:
		r.res.Rejects++
	case oms.ExecTrade:
		r.res.Trades++
		acc := r.account(e.Session)
		acc.Fills++
		sign := int64(1)
		if e.Side == oms.Sell {
			sign = -1
		}
		acc.Position += sign * int64(e.LastQty)
		acc.Cash -= sign * int64(e.LastQty) * int64(e.LastPx)
		acc.MaxAbsPos = max(acc.MaxAbsPos, abs(acc.Position))
		if e.Side == oms.Buy { // count volume once per trade
			r.res.Volume += e.LastQty
		}
	}
}

// Finish marks accounts at mid and returns the result.
func (r *Runner) Finish(mid oms.Price) *Result {
	r.res.Mid = mid
	r.res.Digest = hex.EncodeToString(r.h.Sum(nil))
	return r.res
}

// Replay runs a recorded action log from a fresh engine.
func Replay(actions []Action, mid oms.Price) (*Result, error) {
	r := NewRunner()
	defer r.Close()
	for _, a := range actions {
		if _, err := r.Apply(a); err != nil {
			return nil, err
		}
	}
	return r.Finish(mid), nil
}

// Params configure the synthetic market.
type Params struct {
	Seed           uint64
	Steps          int
	MidStart       oms.Price // multiple of Tick
	HalfSpreadTick int       // market maker quotes this many ticks either side of mid
	MMQty          oms.Qty
	MMInventoryCap int64 // position at which the maker's quotes are skewed by a full spread
	NoisePerStep   int
	NoiseTraders   int
}

// DefaultParams is a calm market with one maker and four noise traders.
func DefaultParams() Params {
	return Params{Seed: 1, Steps: 2000, MidStart: 6_500_000, HalfSpreadTick: 2, MMQty: 20_000,
		MMInventoryCap: 200_000, NoisePerStep: 3, NoiseTraders: 4}
}

const tick = oms.Price(50) // BTC/USD tick

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Simulate runs a market maker against random order flow. The maker re-quotes
// every step around a random-walking mid, leaning its prices against its own
// inventory; noise traders send a mix of marketable and resting orders.
func Simulate(p Params) (*Result, error) {
	rng := rand.New(rand.NewPCG(p.Seed, p.Seed^0x9e3779b97f4a7c15))
	r := NewRunner()
	defer r.Close()

	var (
		mid    = p.MidStart
		n      int
		quotes []string   // the maker's live ClOrdIDs
		rest   []struct { // resting noise orders and the step they expire
			acct, id string
			expire   int
		}
	)
	id := func(prefix string) string { n++; return fmt.Sprintf("%s%d", prefix, n) }
	apply := func(a Action) error { _, err := r.Apply(a); return err }

	for step := 0; step < p.Steps; step++ {
		mid += oms.Price(rng.IntN(3)-1) * tick
		mid = max(mid, 100*tick)

		for _, q := range quotes {
			if err := apply(Action{Op: "cancel", Account: "MM", ClOrdID: id("MX"), OrigClOrdID: q}); err != nil {
				return nil, err
			}
		}
		quotes = quotes[:0]

		// Lean against inventory: long means quote lower to shed it.
		inv := int64(0)
		if a := r.res.Accounts["MM"]; a != nil {
			inv = a.Position
		}
		lean := oms.Price(-inv * int64(p.HalfSpreadTick) * 2 / max(p.MMInventoryCap, 1))
		bid := mid + (lean-oms.Price(p.HalfSpreadTick))*tick
		ask := mid + (lean+oms.Price(p.HalfSpreadTick))*tick
		for _, s := range []struct {
			side oms.Side
			px   oms.Price
		}{{oms.Buy, bid}, {oms.Sell, ask}} {
			qid := id("MQ")
			if err := apply(Action{Op: "new", Account: "MM", ClOrdID: qid, Side: s.side, Type: oms.Limit, TIF: oms.GTC, Price: s.px, Qty: p.MMQty}); err != nil {
				return nil, err
			}
			quotes = append(quotes, qid)
		}

		for i := 0; i < p.NoisePerStep; i++ {
			acct := fmt.Sprintf("N%d", 1+rng.IntN(p.NoiseTraders))
			side := oms.Buy
			if rng.IntN(2) == 0 {
				side = oms.Sell
			}
			qty := oms.Qty(100 * (1 + rng.IntN(50)))
			switch {
			case rng.IntN(100) < 15:
				if err := apply(Action{Op: "new", Account: acct, ClOrdID: id("NM"), Side: side, Type: oms.Market, TIF: oms.IOC, Qty: qty}); err != nil {
					return nil, err
				}
			default:
				off := oms.Price(rng.IntN(7)-3) * tick // -3..+3 ticks into or away from the book
				px := mid + off
				if side == oms.Sell {
					px = mid - off
				}
				tif := oms.IOC
				if rng.IntN(2) == 0 {
					tif = oms.GTC
				}
				nid := id("NL")
				if err := apply(Action{Op: "new", Account: acct, ClOrdID: nid, Side: side, Type: oms.Limit, TIF: tif, Price: px, Qty: qty}); err != nil {
					return nil, err
				}
				if tif == oms.GTC {
					rest = append(rest, struct {
						acct, id string
						expire   int
					}{acct, nid, step + 3})
				}
			}
		}
		// Expire resting noise orders so the book does not fill with stale flow.
		keep := rest[:0]
		for _, o := range rest {
			if o.expire <= step {
				if err := apply(Action{Op: "cancel", Account: o.acct, ClOrdID: id("NX"), OrigClOrdID: o.id}); err != nil {
					return nil, err
				}
				continue
			}
			keep = append(keep, o)
		}
		rest = keep
	}
	return r.Finish(mid), nil
}
