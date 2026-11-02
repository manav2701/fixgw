package fixsession_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/manav2701/fixgw/internal/audit"
	"github.com/manav2701/fixgw/internal/auth"
	"github.com/manav2701/fixgw/internal/fixsession"
	"github.com/manav2701/fixgw/internal/simclient"
)

const dictPath = "../../spec/FIX44.xml"

var creds = auth.Store{
	"CLIENT1": {Username: "client1-key", Secret: "client1-secret", Password: "client1-pass"},
}

type env struct {
	srv       *fixsession.Server
	cfg       simclient.Config
	auditPath string
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
`, port, filepath.ToSlash(dictPath))))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	e := &env{auditPath: filepath.Join(dir, "audit.jsonl")}
	e.srv, err = fixsession.NewServer(fixsession.Config{
		Settings: settings, Creds: creds, StoreKind: storeKind,
		DBPath: filepath.Join(dir, "fix.db"), AuditPath: e.auditPath,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.srv.Stop)
	e.cfg = simclient.Config{
		Host: "127.0.0.1", Port: port, SenderCompID: "CLIENT1", TargetCompID: "FIXGW",
		Username: "client1-key", Secret: "client1-secret", Password: "client1-pass", DictPath: dictPath,
	}
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
