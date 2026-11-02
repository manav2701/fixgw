// Command fixsim is a FIX initiator for exercising the gateway: order flow,
// reject paths, and failure injection through a chaos proxy.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"

	"github.com/manav2701/fixgw/internal/auth"
	"github.com/manav2701/fixgw/internal/simclient"
)

func main() {
	var (
		scenario  = flag.String("scenario", "orders", "heartbeat | orders | gap | reconnect | authfail | all")
		addr      = flag.String("addr", "127.0.0.1:5001", "gateway address")
		sender    = flag.String("sender", "CLIENT1", "SenderCompID (this client)")
		target    = flag.String("target", "FIXGW", "TargetCompID (the gateway)")
		credsPath = flag.String("creds", "deploy/credentials.example.json", "credentials JSON; the entry for -sender is used")
		dict      = flag.String("dict", "spec/FIX44.xml", "FIX 4.4 data dictionary")
		n         = flag.Int("n", 10, "orders to send in the orders scenario")
		reset     = flag.Bool("reset", true, "send ResetSeqNumFlag=Y on logon")
	)
	flag.Parse()

	creds, err := auth.LoadFile(*credsPath)
	if err != nil {
		fatal(err)
	}
	cred, ok := creds[*sender]
	if !ok {
		fatal(fmt.Errorf("no credential for %q in %s", *sender, *credsPath))
	}
	host, port, err := net.SplitHostPort(*addr)
	if err != nil {
		fatal(err)
	}
	cfg := simclient.Config{
		Host: host, Port: port, SenderCompID: *sender, TargetCompID: *target,
		Username: cred.Username, Secret: cred.Secret, Password: cred.Password,
		DictPath: *dict, Reset: *reset,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	run := map[string]func() *simclient.Result{
		"heartbeat": func() *simclient.Result { return simclient.Heartbeat(ctx, cfg) },
		"orders":    func() *simclient.Result { return simclient.Orders(ctx, cfg, *n) },
		"gap":       func() *simclient.Result { return simclient.Gap(ctx, cfg) },
		"reconnect": func() *simclient.Result { return simclient.Reconnect(ctx, cfg) },
		"authfail":  func() *simclient.Result { return simclient.AuthFail(ctx, cfg) },
	}
	names := []string{*scenario}
	if *scenario == "all" {
		// reconnect needs a fresh gateway store, so it is not part of "all".
		names = []string{"authfail", "heartbeat", "orders", "gap"}
	}

	failed := false
	for _, name := range names {
		fn, ok := run[name]
		if !ok {
			fatal(fmt.Errorf("unknown scenario %q", name))
		}
		res := fn()
		verdict := "PASS"
		if !res.Pass {
			verdict, failed = "FAIL", true
		}
		fmt.Printf("== %s: %s\n", res.Name, verdict)
		for _, d := range res.Detail {
			fmt.Println("   " + d)
		}
	}
	if failed {
		os.Exit(1)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "fixsim:", err)
	os.Exit(1)
}
