package fixsession

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"
	filestore "github.com/quickfixgo/quickfix/store/file"

	"github.com/manav2701/fixgw/internal/audit"
	"github.com/manav2701/fixgw/internal/auth"
	"github.com/manav2701/fixgw/internal/engine"
	"github.com/manav2701/fixgw/internal/oms"
	"github.com/manav2701/fixgw/internal/risk"
)

// Config assembles a Server.
type Config struct {
	Settings    *quickfix.Settings
	Creds       auth.Store
	Instruments oms.Instruments // defaults to oms.DefaultInstruments()

	// StoreKind is "sqlite" (default), "file" or "memory".
	StoreKind string
	DBPath    string // sqlite only; default "store/fixgw.db"

	AuditPath string // empty disables the audit log
	MaxSkew   time.Duration

	Risk               risk.Config // zero value means risk.DefaultConfig()
	CancelOnDisconnect bool        // cancel a session's open orders when it logs out
	// AdminToken guards the mutating admin endpoints (kill switch). When empty
	// they are disabled.
	AdminToken string

	Logger *slog.Logger
}

// Server is a running FIX acceptor plus the state behind it.
type Server struct {
	Sessions *Registry
	Orders   *oms.Registry
	Engine   *engine.Engine
	Risk     *risk.Risk

	adminToken string
	app        *App
	stopOnce   sync.Once
	started    atomic.Bool
	acceptor   *quickfix.Acceptor
	audit      *audit.Logger
	sqlStore   *SQLiteStoreFactory
}

// NewServer builds, but does not start, the acceptor.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Settings == nil {
		return nil, errors.New("fixsession: Settings is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Instruments == nil {
		cfg.Instruments = oms.DefaultInstruments()
	}
	if cfg.MaxSkew == 0 {
		cfg.MaxSkew = 2 * time.Minute
	}

	for sym, in := range cfg.Instruments {
		if err := in.Validate(); err != nil {
			return nil, err
		}
		if in.Symbol != sym {
			return nil, fmt.Errorf("fixsession: instrument keyed %q but named %q", sym, in.Symbol)
		}
	}
	if cfg.Risk.Default == (risk.Limits{}) && len(cfg.Risk.Sessions) == 0 {
		cfg.Risk = risk.DefaultConfig()
	}

	s := &Server{Sessions: NewRegistry(), Orders: oms.NewRegistry(), Risk: risk.New(cfg.Risk), adminToken: cfg.AdminToken}
	s.Engine = engine.New(s.Orders, cfg.Instruments, s.Risk, cfg.Logger)

	var storeFactory quickfix.MessageStoreFactory
	switch cfg.StoreKind {
	case "", "sqlite":
		path := cfg.DBPath
		if path == "" {
			path = "store/fixgw.db"
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, err
		}
		f, err := OpenSQLiteStore(path)
		if err != nil {
			return nil, err
		}
		s.sqlStore, storeFactory = f, f
	case "file":
		storeFactory = filestore.NewStoreFactory(cfg.Settings)
	case "memory":
		storeFactory = quickfix.NewMemoryStoreFactory()
	default:
		return nil, fmt.Errorf("fixsession: unknown store %q", cfg.StoreKind)
	}

	if cfg.AuditPath != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.AuditPath), 0o750); err != nil {
			return nil, err
		}
		a, err := audit.Open(cfg.AuditPath, 50*time.Millisecond)
		if err != nil {
			s.closeStore()
			return nil, err
		}
		s.audit = a
	}

	s.app = NewApp(auth.NewAuthenticator(cfg.Creds, cfg.MaxSkew), s.Orders, s.Engine, cfg.Instruments,
		s.Sessions, cfg.CancelOnDisconnect, cfg.Logger)
	acc, err := quickfix.NewAcceptor(s.app, storeFactory, cfg.Settings, NewLogFactory(s.audit, cfg.Logger))
	if err != nil {
		s.Stop()
		return nil, err
	}
	s.acceptor = acc
	return s, nil
}

// Start starts the matching engine and begins accepting FIX connections.
func (s *Server) Start() error {
	s.Engine.Start()
	if err := s.acceptor.Start(); err != nil {
		s.Engine.Stop()
		return err
	}
	s.started.Store(true)
	return nil
}

// KillSwitch halts (on) or resumes (off) trading for a counterparty CompID, or
// everyone when scope is "". With massCancel, open orders are cancelled and
// their owners notified.
func (s *Server) KillSwitch(scope string, on, massCancel bool) (cancelled int, err error) {
	execs, err := s.Engine.KillSwitch(scope, on, massCancel)
	if err != nil {
		return 0, err
	}
	s.app.Dispatch(execs)
	return len(execs), nil
}

// Stop shuts the acceptor down and flushes the audit log and store. It is safe
// to call more than once.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		// quickfix's Acceptor.Stop blocks forever if the acceptor never started.
		if s.started.Load() {
			s.acceptor.Stop()
		}
		s.Engine.Stop()
		if s.audit != nil {
			s.audit.Close()
		}
		s.closeStore()
	})
}

func (s *Server) closeStore() {
	if s.sqlStore != nil {
		s.sqlStore.Close()
	}
}

// StatusHandler serves read-only status plus the token-guarded kill switch:
//
//	GET  /healthz
//	GET  /api/sessions
//	GET  /api/orders
//	GET  /api/book?symbol=BTC/USD&depth=10
//	POST /api/killswitch?scope=CLIENT1&on=true&mass_cancel=true   (Bearer token)
func (s *Server) StatusHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.Sessions.Snapshot())
	})
	mux.HandleFunc("/api/orders", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, s.Orders.Open())
	})
	mux.HandleFunc("/api/book", func(w http.ResponseWriter, r *http.Request) {
		depth := 10
		if d, err := strconv.Atoi(r.URL.Query().Get("depth")); err == nil && d > 0 && d <= 100 {
			depth = d
		}
		snap, ok, err := s.Engine.Snapshot(r.URL.Query().Get("symbol"), depth)
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		case !ok:
			http.Error(w, "unknown symbol", http.StatusNotFound)
		default:
			writeJSON(w, snap)
		}
	})
	mux.HandleFunc("/api/killswitch", s.handleKillSwitch)
	return mux
}

func (s *Server) handleKillSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.adminToken == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.adminToken)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	on, err := strconv.ParseBool(q.Get("on"))
	if err != nil {
		http.Error(w, "on=true|false required", http.StatusBadRequest)
		return
	}
	massCancel, _ := strconv.ParseBool(q.Get("mass_cancel"))
	n, err := s.KillSwitch(q.Get("scope"), on, massCancel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"scope": q.Get("scope"), "halted": on, "orders_cancelled": n})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
