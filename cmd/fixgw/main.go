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
	"github.com/manav2701/fixgw/internal/fixsession"
)

func main() {
	var (
		cfgPath   = flag.String("config", "deploy/fixgw.cfg", "quickfix settings file")
		credsPath = flag.String("creds", "deploy/credentials.example.json", "counterparty credentials (JSON)")
		storeKind = flag.String("store", "sqlite", "message store: sqlite, file or memory")
		dbPath    = flag.String("db", "store/fixgw.db", "sqlite database path")
		auditPath = flag.String("audit", "logs/audit.jsonl", "audit log path (empty to disable)")
		httpAddr  = flag.String("http", "127.0.0.1:8080", "status HTTP listen address (empty to disable)")
		verbose   = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := run(log, *cfgPath, *credsPath, *storeKind, *dbPath, *auditPath, *httpAddr); err != nil {
		log.Error("fixgw failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, cfgPath, credsPath, storeKind, dbPath, auditPath, httpAddr string) error {
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

	srv, err := fixsession.NewServer(fixsession.Config{
		Settings: settings, Creds: creds,
		StoreKind: storeKind, DBPath: dbPath, AuditPath: auditPath,
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

	var web *http.Server
	if httpAddr != "" {
		web = &http.Server{Addr: httpAddr, Handler: srv.StatusHandler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := web.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("status server", "err", err)
			}
		}()
		log.Info("status endpoint", "addr", httpAddr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
