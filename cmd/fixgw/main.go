// Command fixgw runs the FIX 4.4 order-entry gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/quickfixgo/quickfix"

	"github.com/manav2701/fixgw/internal/auth"
	"github.com/manav2701/fixgw/internal/bridge"
	"github.com/manav2701/fixgw/internal/fixsession"
	"github.com/manav2701/fixgw/internal/oms"
	"github.com/manav2701/fixgw/internal/risk"
	"github.com/manav2701/fixgw/internal/venue"
	webui "github.com/manav2701/fixgw/web"
)

func main() {
	var (
		cfgPath   = flag.String("config", "deploy/fixgw.cfg", "quickfix settings file")
		credsPath = flag.String("creds", "deploy/credentials.example.json", "counterparty credentials (JSON)")
		storeKind = flag.String("store", "sqlite", "message store: sqlite, file or memory")
		dbPath    = flag.String("db", "store/fixgw.db", "sqlite database path")
		auditPath = flag.String("audit", "logs/audit.jsonl", "audit log path (empty to disable)")
		httpAddr  = flag.String("http", "127.0.0.1:8080", "status HTTP listen address (empty to disable)")
		riskPath  = flag.String("risk", "deploy/risk.json", "risk limits (JSON); empty uses built-in defaults")
		cod       = flag.Bool("cancel-on-disconnect", false, "cancel a session's open orders when it logs out")
		venuesCfg = flag.String("venues", "", "venue routing (JSON); empty keeps every symbol on the internal book")
		verbose   = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	opts := options{
		cfgPath: *cfgPath, credsPath: *credsPath, storeKind: *storeKind, dbPath: *dbPath,
		auditPath: *auditPath, httpAddr: *httpAddr, riskPath: *riskPath, cancelOnDisconnect: *cod, venuesPath: *venuesCfg,
		// The kill-switch endpoint stays disabled unless a token is provided.
		adminToken: os.Getenv("FIXGW_ADMIN_TOKEN"),
	}
	if err := run(log, opts); err != nil {
		log.Error("fixgw failed", "err", err)
		os.Exit(1)
	}
}

type options struct {
	cfgPath, credsPath, storeKind, dbPath, auditPath, httpAddr, riskPath, adminToken, venuesPath string
	cancelOnDisconnect                                                                           bool
}

func run(log *slog.Logger, o options) error {
	cfgPath, credsPath, storeKind, dbPath, auditPath, httpAddr := o.cfgPath, o.credsPath, o.storeKind, o.dbPath, o.auditPath, o.httpAddr
	f, err := os.Open(cfgPath)
	if err != nil {
		return err
	}
	settings, err := quickfix.ParseSettings(f)
	f.Close()
	if err != nil {
		return err
	}
	creds, err := auth.LoadFile(credsPath)
	if err != nil {
		return err
	}

	var riskCfg risk.Config
	if o.riskPath != "" {
		if riskCfg, err = risk.LoadFile(o.riskPath); err != nil {
			return err
		}
	}

	var bindings []venue.Binding
	if o.venuesPath != "" {
		if bindings, err = venue.LoadFile(o.venuesPath, oms.DefaultInstruments()); err != nil {
			return err
		}
		for _, b := range bindings {
			log.Info("routing to venue", "symbol", b.Spec.Symbol, "venue", b.Venue.Name(), "venue_symbol", b.Spec.VenueSymbol)
		}
	}

	hub := bridge.NewHub()
	srv, err := fixsession.NewServer(fixsession.Config{
		Settings: settings, Creds: creds,
		StoreKind: storeKind, DBPath: dbPath, AuditPath: auditPath,
		Risk: riskCfg, CancelOnDisconnect: o.cancelOnDisconnect, AdminToken: o.adminToken,
		Hub: hub, Venues: bindings,
		Logger: log,
	})
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		srv.Stop()
		return err
	}
	log.Info("fixgw started", "config", cfgPath, "store", storeKind, "audit", auditPath)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var web *http.Server
	if httpAddr != "" {
		mux := http.NewServeMux()
		status := srv.StatusHandler()
		mux.Handle("/healthz", status)
		mux.Handle("/api/", status)
		mux.Handle("/ws", bridge.Handler(hub, srv.Source()))
		mux.Handle("/", webui.Handler())
		go bridge.RunBooks(ctx, hub, srv.Source(), 250*time.Millisecond)
		web = &http.Server{Addr: httpAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := web.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("status server", "err", err)
			}
		}()
		log.Info("dashboard and status endpoint", "addr", httpAddr)
	}

	<-ctx.Done()
	log.Info("shutting down")

	if web != nil {
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		web.Shutdown(shutdown)
		cancel()
	}
	srv.Stop()
	return nil
}
