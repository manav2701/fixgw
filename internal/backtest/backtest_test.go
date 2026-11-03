package backtest

import (
	"bytes"
	"testing"
)

func small() Params {
	p := DefaultParams()
	p.Steps = 300
	return p
}

func TestSameSeedGivesIdenticalRuns(t *testing.T) {
	a, err := Simulate(small())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Simulate(small())
	if a.Digest != b.Digest {
		t.Fatalf("same seed, different digests:\n%s\n%s", a.Digest, b.Digest)
	}
	if a.Trades == 0 || a.Orders < 1000 {
		t.Fatalf("run too quiet to mean anything: %d orders, %d trade reports", a.Orders, a.Trades)
	}
}

func TestDifferentSeedsDiffer(t *testing.T) {
	p := small()
	a, _ := Simulate(p)
	p.Seed = 2
	b, _ := Simulate(p)
	if a.Digest == b.Digest {
		t.Fatal("different seeds produced the same run")
	}
}

// The recorded action log, replayed on a fresh engine, must reproduce every
// execution report exactly. This is the property that makes the log an audit
// and debugging tool.
func TestReplayReproducesTheLiveRun(t *testing.T) {
	live, err := Simulate(small())
	if err != nil {
		t.Fatal(err)
	}
	again, err := Replay(live.Actions, live.Mid)
	if err != nil {
		t.Fatal(err)
	}
	if again.Digest != live.Digest {
		t.Fatal("replay diverged from the live run")
	}
	if again.Trades != live.Trades || again.Volume != live.Volume {
		t.Errorf("replay: %d trades %d volume, live: %d trades %d volume", again.Trades, again.Volume, live.Trades, live.Volume)
	}
}

func TestReplayThroughCSVIsExact(t *testing.T) {
	live, _ := Simulate(small())
	var buf bytes.Buffer
	if err := WriteCSV(&buf, live.Actions); err != nil {
		t.Fatal(err)
	}
	actions, err := ReadCSV(&buf)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Replay(actions, live.Mid)
	if err != nil {
		t.Fatal(err)
	}
	if again.Digest != live.Digest {
		t.Fatal("CSV round trip changed the outcome")
	}
}

// Trading is zero-sum: every unit one account buys another sells, and every
// unit of cash one pays another receives.
func TestLedgersBalance(t *testing.T) {
	res, err := Simulate(small())
	if err != nil {
		t.Fatal(err)
	}
	var pos, cash, pnl int64
	for _, a := range res.Accounts {
		pos += a.Position
		cash += a.Cash
		pnl += a.PnL(res.Mid)
	}
	if pos != 0 || cash != 0 || pnl != 0 {
		t.Fatalf("not zero-sum: position %d, cash %d, pnl %d", pos, cash, pnl)
	}
	mm := res.Accounts["MM"]
	if mm == nil || mm.Fills == 0 {
		t.Fatal("market maker never traded")
	}
	// The inventory lean should keep the maker well inside the cap most of the
	// time; an unbounded position would mean the skew is broken.
	if mm.MaxAbsPos > 10*DefaultParams().MMInventoryCap {
		t.Errorf("maker inventory reached %d, cap is %d", mm.MaxAbsPos, DefaultParams().MMInventoryCap)
	}
}

func TestReadCSVRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"no header":  "new,A,1,,1,2,1,100,100\n",
		"bad number": "op,account,clordid,orig_clordid,side,type,tif,price,qty\nnew,A,1,,1,2,1,xx,100\n",
		"short row":  "op,account,clordid,orig_clordid,side,type,tif,price,qty\nnew,A\n",
	} {
		if _, err := ReadCSV(bytes.NewBufferString(in)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
