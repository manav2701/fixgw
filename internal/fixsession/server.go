package fixsession

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/quickfixgo/quickfix"
	filestore "github.com/quickfixgo/quickfix/store/file"

	"github.com/manav2701/fixgw/internal/audit"
	"github.com/manav2701/fixgw/internal/auth"
	"github.com/manav2701/fixgw/internal/oms"
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

	Logger *slog.Logger
}

// Server is a running FIX acceptor plus the state behind it.
type Server struct {
	Sessions *Registry
	Orders   *oms.Registry

	stopOnce sync.Once
	acceptor *quickfix.Acceptor
	audit    *audit.Logger
	sqlStore *SQLiteStoreFactory
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

	s := &Server{Sessions: NewRegistry(), Orders: oms.NewRegistry()}

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

	app := NewApp(auth.NewAuthenticator(cfg.Creds, cfg.MaxSkew), s.Orders, cfg.Instruments, s.Sessions, cfg.Logger)
	acc, err := quickfix.NewAcceptor(app, storeFactory, cfg.Settings, NewLogFactory(s.audit, cfg.Logger))
	if err != nil {
		s.Stop()
		return nil, err
	}
	s.acceptor = acc
	return s, nil
}

// Start begins accepting FIX connections.
func (s *Server) Start() error { return s.acceptor.Start() }

// Stop shuts the acceptor down and flushes the audit log and store. It is safe
// to call more than once.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		if s.acceptor != nil {
			s.acceptor.Stop()
		}
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

// StatusHandler serves /healthz, /api/sessions and /api/orders.
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
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
