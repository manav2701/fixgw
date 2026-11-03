package fixsession_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/manav2701/fixgw/internal/audit"
	"github.com/manav2701/fixgw/internal/auth"
	"github.com/manav2701/fixgw/internal/bridge"
	"github.com/manav2701/fixgw/internal/fixmsg"
	"github.com/manav2701/fixgw/internal/fixsession"
	"github.com/manav2701/fixgw/internal/oms"
	"github.com/manav2701/fixgw/internal/risk"
	"github.com/manav2701/fixgw/internal/simclient"
	"github.com/manav2701/fixgw/internal/venue"
)

const (
	dictPath   = "../../spec/FIX44.xml"
	adminToken = "test-admin-token"
)

var creds = auth.Store{
	"CLIENT1": {Username: "client1-key", Secret: "client1-secret", Password: "client1-pass"},
	"CLIENT2": {Username: "client2-key", Secret: "client2-secret", Password: "client2-pass"},
}

type env struct {
	srv       *fixsession.Server
	cfg       simclient.Config // CLIENT1
	cfg2      simclient.Config // CLIENT2
	auditPath string
}

type envOpts struct {
	store  string
	risk   risk.Config
	cod    bool
	venues []venue.Binding
	hub    *bridge.Hub
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func newEnv(t *testing.T, storeKind string) *env {
	return newEnvWith(t, envOpts{store: storeKind})
}

func newEnvWith(t *testing.T, o envOpts) *env {
	t.Helper()
	port := freePort(t)
	settings, err := quickfix.ParseSettings(strings.NewReader(fmt.Sprintf(`
[DEFAULT]
ConnectionType=acceptor
SocketAcceptAddress=127.0.0.1
SocketAcceptPort=%s
BeginString=FIX.4.4
SenderCompID=FIXGW
StartTime=00:00:00
EndTime=00:00:00
HeartBtInt=30
DataDictionary=%s
[SESSION]
TargetCompID=CLIENT1
[SESSION]
TargetCompID=CLIENT2
`, port, filepath.ToSlash(dictPath))))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	e := &env{auditPath: filepath.Join(dir, "audit.jsonl")}
	e.srv, err = fixsession.NewServer(fixsession.Config{
		Settings: settings, Creds: creds, StoreKind: o.store,
		DBPath: filepath.Join(dir, "fix.db"), AuditPath: e.auditPath,
		Risk: o.risk, CancelOnDisconnect: o.cod, AdminToken: adminToken,
		Hub: o.hub, Venues: o.venues, VenuePoll: 20 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.srv.Stop)
	mk := func(id string) simclient.Config {
		c := creds[id]
		return simclient.Config{
			Host: "127.0.0.1", Port: port, SenderCompID: id, TargetCompID: "FIXGW",
			Username: c.Username, Secret: c.Secret, Password: c.Password, DictPath: dictPath,
		}
	}
	e.cfg, e.cfg2 = mk("CLIENT1"), mk("CLIENT2")
	return e
}

func report(t *testing.T, r *simclient.Result) {
	t.Helper()
	for _, d := range r.Detail {
		t.Log(d)
	}
	if !r.Pass {
		t.Fatalf("scenario %s failed", r.Name)
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestOrderLifecycle(t *testing.T) {
	e := newEnv(t, "memory")
	report(t, simclient.Orders(ctx(t), e.cfg, 6))
}

func TestHeartbeat(t *testing.T) {
	e := newEnv(t, "memory")
	report(t, simclient.Heartbeat(ctx(t), e.cfg))
}

func TestBadSignatureNeverLogsOn(t *testing.T) {
	e := newEnv(t, "memory")
	report(t, simclient.AuthFail(ctx(t), e.cfg))
	var failures uint64
	for _, s := range e.srv.Sessions.Snapshot() {
		failures += s.AuthFailures
	}
	if failures == 0 {
		t.Error("gateway did not record the authentication failure")
	}
}

// The headline resilience test: two orders vanish on the wire, the gateway
// detects the sequence gap, and the client replays them with PossDupFlag=Y.
func TestSequenceGapRecovery(t *testing.T) {
	e := newEnv(t, "sqlite")
	report(t, simclient.Gap(ctx(t), e.cfg))
}

func TestReconnectAfterSeveredConnection(t *testing.T) {
	e := newEnv(t, "sqlite")
	report(t, simclient.Reconnect(ctx(t), e.cfg))
}

// Two counterparties trade against each other over FIX: partial fills, price
// improvement, self-trade prevention, FOK, and the cancel/replace chain.
func TestTradingBetweenTwoCounterparties(t *testing.T) {
	e := newEnvWith(t, envOpts{store: "memory", risk: risk.DefaultConfig()})
	report(t, simclient.Trading(ctx(t), e.cfg, e.cfg2))
	if err := e.srv.Engine.Verify(); err != nil {
		t.Fatalf("book invariants after trading: %v", err)
	}
}

func TestRiskControlsOverFIX(t *testing.T) {
	e := newEnvWith(t, envOpts{store: "memory", risk: risk.DefaultConfig()})
	report(t, simclient.Risk(ctx(t), e.cfg))
}

func TestRateLimitOverFIX(t *testing.T) {
	cfg := risk.Config{Default: risk.Limits{RatePerSec: 0.001, RateBurst: 5}}
	e := newEnvWith(t, envOpts{store: "memory", risk: cfg})
	c, err := simclient.New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	if err := c.WaitLogon(ctx(t), 8*time.Second); err != nil {
		t.Fatal(err)
	}

	const n = 12
	for i := 0; i < n; i++ {
		c.SendOrder(fixmsg.NewOrder{ClOrdID: fmt.Sprintf("RL-%d", i), Symbol: "BTC/USD", Side: oms.Buy,
			Type: oms.Limit, TIF: oms.GTC, Qty: "0.010000", Price: "64000.00"})
	}
	if err := simclient.WaitFor(ctx(t), 8*time.Second, func() bool { return len(c.Reports()) >= n }); err != nil {
		t.Fatalf("got %d of %d reports", len(c.Reports()), n)
	}
	var accepted, limited int
	for _, r := range c.Reports() {
		switch {
		case r.ExecType == "0":
			accepted++
		case r.ExecType == "8" && strings.Contains(r.Text, "rate limit"):
			limited++
		}
	}
	if accepted != 5 || limited != 7 {
		t.Errorf("accepted=%d rate-limited=%d, want 5 and 7 with burst 5", accepted, limited)
	}
}

func TestKillSwitchOverHTTP(t *testing.T) {
	e := newEnvWith(t, envOpts{store: "memory", risk: risk.DefaultConfig()})
	web := httptest.NewServer(e.srv.StatusHandler())
	defer web.Close()

	post := func(path, token string) int {
		req, _ := http.NewRequest(http.MethodPost, web.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	c, err := simclient.New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	if err := c.WaitLogon(ctx(t), 8*time.Second); err != nil {
		t.Fatal(err)
	}
	bid := func(id string) fixmsg.NewOrder {
		return fixmsg.NewOrder{ClOrdID: id, Symbol: "BTC/USD", Side: oms.Buy, Type: oms.Limit, TIF: oms.GTC,
			Qty: "0.010000", Price: "64000.00"}
	}
	c.SendOrder(bid("KS-1"))
	c.SendOrder(bid("KS-2"))
	if _, err := c.WaitAck(ctx(t), 8*time.Second, "KS-2", oms.ExecNew); err != nil {
		t.Fatal(err)
	}

	// The endpoint refuses callers without the token.
	if code := post("/api/killswitch?on=true&mass_cancel=true", ""); code != http.StatusForbidden {
		t.Errorf("no token: status %d", code)
	}
	if code := post("/api/killswitch?on=true&mass_cancel=true", "wrong"); code != http.StatusForbidden {
		t.Errorf("wrong token: status %d", code)
	}
	if n := len(e.srv.Orders.Open()); n != 2 {
		t.Fatalf("unauthorised calls must not cancel anything, open=%d", n)
	}

	// Engage it for everyone with mass cancel.
	if code := post("/api/killswitch?on=true&mass_cancel=true", adminToken); code != http.StatusOK {
		t.Fatalf("kill switch: status %d", code)
	}
	for _, id := range []string{"KS-1", "KS-2"} {
		r, err := c.WaitAck(ctx(t), 8*time.Second, id, oms.ExecCanceled)
		if err != nil || r.Text != "kill switch" {
			t.Errorf("%s: cancel report %+v, err %v", id, r, err)
		}
	}
	c.SendOrder(bid("KS-3"))
	if err := simclient.WaitFor(ctx(t), 8*time.Second, func() bool {
		r, ok := c.Find(func(r simclient.Report) bool { return r.ClOrdID == "KS-3" && r.ExecType == "8" })
		return ok && strings.Contains(r.Text, "kill switch")
	}); err != nil {
		t.Error("new order during a halt was not rejected with a kill switch reason")
	}

	// Resume.
	if code := post("/api/killswitch?on=false", adminToken); code != http.StatusOK {
		t.Fatalf("resume: status %d", code)
	}
	c.SendOrder(bid("KS-4"))
	if _, err := c.WaitAck(ctx(t), 8*time.Second, "KS-4", oms.ExecNew); err != nil {
		t.Errorf("order after resume not accepted: %v", err)
	}
}

func TestKillSwitchDisabledWithoutToken(t *testing.T) {
	// quickfix keeps a process-global session registry, so this server uses its
	// own session ID (see mustSettings) rather than the one newEnv creates.
	srv, err := fixsession.NewServer(fixsession.Config{
		Settings: mustSettings(t), Creds: creds, StoreKind: "memory",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), // no AdminToken
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Stop()
	web := httptest.NewServer(srv.StatusHandler())
	defer web.Close()
	req, _ := http.NewRequest(http.MethodPost, web.URL+"/api/killswitch?on=true", nil)
	req.Header.Set("Authorization", "Bearer ")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403 when no admin token is configured", resp.StatusCode)
	}
}

func mustSettings(t *testing.T) *quickfix.Settings {
	t.Helper()
	s, err := quickfix.ParseSettings(strings.NewReader(fmt.Sprintf(`
[DEFAULT]
ConnectionType=acceptor
SocketAcceptPort=%s
BeginString=FIX.4.4
SenderCompID=FIXGW
StartTime=00:00:00
EndTime=00:00:00
HeartBtInt=30
[SESSION]
TargetCompID=NOTOKEN
`, freePort(t))))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCancelOnDisconnect(t *testing.T) {
	e := newEnvWith(t, envOpts{store: "memory", risk: risk.DefaultConfig(), cod: true})
	c, err := simclient.New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if err := c.WaitLogon(ctx(t), 8*time.Second); err != nil {
		t.Fatal(err)
	}
	c.SendOrder(fixmsg.NewOrder{ClOrdID: "COD-1", Symbol: "BTC/USD", Side: oms.Buy, Type: oms.Limit, TIF: oms.GTC,
		Qty: "0.010000", Price: "64000.00"})
	if _, err := c.WaitAck(ctx(t), 8*time.Second, "COD-1", oms.ExecNew); err != nil {
		t.Fatal(err)
	}
	if len(e.srv.Orders.Open()) != 1 {
		t.Fatal("expected one resting order")
	}
	c.Stop() // drops the connection
	if err := simclient.WaitFor(ctx(t), 8*time.Second, func() bool { return len(e.srv.Orders.Open()) == 0 }); err != nil {
		t.Error("open order survived the disconnect")
	}
	if snap, _, _ := e.srv.Engine.Snapshot("BTC/USD", 5); len(snap.Bids) != 0 {
		t.Errorf("book still holds the disconnected client's order: %+v", snap)
	}
}

func TestAuditTrailIsCompleteAndRedacted(t *testing.T) {
	e := newEnv(t, "memory")
	report(t, simclient.Orders(ctx(t), e.cfg, 3))
	e.srv.Stop() // flushes the audit log

	n, err := audit.Verify(e.auditPath)
	if err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	if n < 10 {
		t.Errorf("only %d audit entries for a full order session", n)
	}
	raw, err := readFile(e.auditPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"client1-pass", "client1-secret"} {
		if strings.Contains(raw, secret) {
			t.Errorf("audit log leaks %q", secret)
		}
	}
	if !strings.Contains(raw, "35=A") || !strings.Contains(raw, "35=D") || !strings.Contains(raw, "35=8") {
		t.Error("audit log missing Logon, NewOrderSingle or ExecutionReport")
	}
}
