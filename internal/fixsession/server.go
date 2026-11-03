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
	"github.com/manav2701/fixgw/internal/bridge"
	"github.com/manav2701/fixgw/internal/engine"
	"github.com/manav2701/fixgw/internal/oms"
	"github.com/manav2701/fixgw/internal/risk"
	"github.com/manav2701/fixgw/internal/venue"
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

	// Hub receives the dashboard event feed. Optional.
	Hub *bridge.Hub
	// Venues routes the listed symbols to external exchanges instead of the
	// internal book. Optional.
	Venues []venue.Binding
	// VenuePoll is how often live venue orders are reconciled (default 1s).
	VenuePoll time.Duration

	Logger *slog.Logger
}

// Server is a running FIX acceptor plus the state behind it.
type Server struct {
	Sessions *Registry
	Orders   *oms.Registry
	Engine   *engine.Engine
	Risk     *risk.Risk
	Router   *venue.Router // nil when no symbol is routed
	Hub      *bridge.Hub
	ins      oms.Instruments

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

	s := &Server{Sessions: NewRegistry(), Orders: oms.NewRegistry(), Risk: risk.New(cfg.Risk), adminToken: cfg.AdminToken,
		Hub: cfg.Hub, ins: cfg.Instruments}
	s.Engine = engine.New(s.Orders, cfg.Instruments, s.Risk, cfg.Logger)
	if len(cfg.Venues) > 0 {
		for _, b := range cfg.Venues {
			if _, ok := cfg.Instruments[b.Spec.Symbol]; !ok {
				return nil, fmt.Errorf("fixsession: venue route for unknown symbol %q", b.Spec.Symbol)
			}
		}
		s.Router = venue.NewRouter(venue.Config{
			Registry: s.Orders, Instruments: cfg.Instruments, Bindings: cfg.Venues,
			// s.app does not exist yet; the closures run only after it does.
			Emit:             func(e []oms.Execution) { s.app.Dispatch(e) },
			EmitCancelReject: func(r oms.CancelReject) { s.app.sendCancelRejectTo(r) },
			Events:           func(kind string, data any) { s.Hub.Publish(kind, data) },
			PollInterval:     cfg.VenuePoll, Logger: cfg.Logger,
		})
		s.Engine.SetRouter(s.Router)
	}

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
	s.app.hub, s.app.router = cfg.Hub, s.Router
	acc, err := quickfix.NewAcceptor(s.app, storeFactory, cfg.Settings, NewLogFactory(s.audit, cfg.Hub, cfg.Logger))
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
	if s.Router != nil {
		s.Router.Start()
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
	s.Hub.Publish(bridge.KindHalt, map[string]any{"scope": scope, "halted": on, "mass_cancel": massCancel, "orders_cancelled": len(execs)})
	return len(execs), nil
}

// Source adapts the server to the bridge's read-only view.
func (s *Server) Source() bridge.Source { return source{s} }

type source struct{ s *Server }

func (b source) Sessions() any { return b.s.Sessions.Snapshot() }
func (b source) OpenOrders() any {
	open := b.s.Orders.Open()
	out := make([]map[string]any, 0, len(open))
	for _, o := range open {
		out = append(out, orderView(b.s.ins[o.Symbol], o))
	}
	return out
}
func (b source) Symbols() []string { return b.s.ins.Symbols() }
func (b source) Halted() bool      { return b.s.Risk.Halted("") }

// bookView is a book snapshot plus the decimals needed to render its
// fixed-point prices and quantities.
type bookView struct {
	engine.Snapshot
	PriceDecimals int    `json:"price_decimals"`
	QtyDecimals   int    `json:"qty_decimals"`
	Routed        string `json:"routed,omitempty"` // set when the symbol trades at an external venue
}

func (b source) Book(symbol string, depth int) (any, bool, error) {
	snap, ok, err := b.s.Engine.Snapshot(symbol, depth)
	if err != nil || !ok {
		return nil, ok, err
	}
	ins := b.s.ins[symbol]
	v := bookView{Snapshot: snap, PriceDecimals: ins.PriceDecimals, QtyDecimals: ins.QtyDecimals}
	if b.s.Router != nil && b.s.Router.Handles(symbol) {
		v.Routed = b.s.Router.VenueName(symbol)
	}
	return v, true, nil
}

// orderView renders an open order for the dashboard.
func orderView(ins oms.Instrument, o oms.Order) map[string]any {
	side := "BUY"
	if o.Side == oms.Sell {
		side = "SELL"
	}
	return map[string]any{
		"session": o.Session, "order_id": o.OrderID, "cl_ord_id": o.ClOrdID, "symbol": o.Symbol, "side": side,
		"ord_status": string(rune(o.Status)), "qty": ins.FormatQty(o.Qty), "price": ins.FormatPrice(o.Price),
		"cum_qty": ins.FormatQty(o.CumQty), "leaves_qty": ins.FormatQty(o.LeavesQty), "avg_px": ins.FormatPrice(o.AvgPx),
	}
}

// Stop shuts the acceptor down and flushes the audit log and store. It is safe
// to call more than once.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		// quickfix's Acceptor.Stop blocks forever if the acceptor never started.
		if s.started.Load() {
			s.acceptor.Stop()
		}
		if s.Router != nil {
			s.Router.Stop()
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
