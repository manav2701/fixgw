package venue

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/manav2701/fixgw/internal/oms"
)

func btc() (oms.Instruments, oms.Instrument) {
	ins := oms.DefaultInstruments()
	return ins, ins["BTC/USD"]
}

func TestBinanceSignedRequests(t *testing.T) {
	ins, _ := btc()
	var mu sync.Mutex
	var seen []*http.Request
	var bodies []url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the signature exactly as Binance does: HMAC over the query
		// string without the trailing signature parameter.
		raw := r.URL.RawQuery
		i := strings.LastIndex(raw, "&signature=")
		if i < 0 {
			http.Error(w, `{"code":-1102,"msg":"signature missing"}`, 400)
			return
		}
		m := hmac.New(sha256.New, []byte("shh"))
		m.Write([]byte(raw[:i]))
		if hex.EncodeToString(m.Sum(nil)) != raw[i+len("&signature="):] {
			http.Error(w, `{"code":-1022,"msg":"Signature for this request is not valid."}`, 400)
			return
		}
		mu.Lock()
		seen = append(seen, r)
		bodies = append(bodies, r.URL.Query())
		mu.Unlock()

		switch r.Method {
		case http.MethodPost:
			w.Write([]byte(`{"orderId":42,"status":"NEW","executedQty":"0.00000000","cummulativeQuoteQty":"0.00000000"}`))
		case http.MethodGet:
			w.Write([]byte(`{"orderId":42,"status":"PARTIALLY_FILLED","executedQty":"0.00400000","cummulativeQuoteQty":"260.00000000"}`))
		case http.MethodDelete:
			if r.URL.Query().Get("origClientOrderId") == "gone" {
				http.Error(w, `{"code":-2011,"msg":"Unknown order sent."}`, 400)
				return
			}
			w.Write([]byte(`{"orderId":42,"status":"CANCELED","executedQty":"0.00400000","cummulativeQuoteQty":"260.00000000"}`))
		}
	}))
	defer srv.Close()

	b := NewBinance(srv.URL, "key", "shh", ins, []Spec{{Symbol: "BTC/USD", VenueSymbol: "BTCUSDT"}})
	ctx := context.Background()

	st, err := b.Place(ctx, Request{ClientID: "cid-1", Symbol: "BTC/USD", Side: oms.Buy, Type: oms.Limit, TIF: oms.IOC, Qty: 10_000, Price: 6_500_000})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if st.VenueID != "42" || st.Status != StatusOpen {
		t.Errorf("place state = %+v", st)
	}
	q := bodies[0]
	for k, want := range map[string]string{
		"symbol": "BTCUSDT", "side": "BUY", "type": "LIMIT", "timeInForce": "IOC",
		"quantity": "0.010000", "price": "65000.00", "newClientOrderId": "cid-1",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if seen[0].Header.Get("X-MBX-APIKEY") != "key" {
		t.Error("API key header missing")
	}

	st, err = b.Query(ctx, Ref{ClientID: "cid-1", Symbol: "BTC/USD"})
	if err != nil {
		t.Fatal(err)
	}
	// 260 USDT for 0.004 BTC is 65000 per BTC.
	if st.Status != StatusPartial || st.CumQty != 4000 || st.AvgPx != 6_500_000 {
		t.Errorf("query state = %+v", st)
	}

	if err := b.Cancel(ctx, Ref{ClientID: "cid-1", Symbol: "BTC/USD"}); err != nil {
		t.Errorf("cancel: %v", err)
	}
	var ve *Error
	err = b.Cancel(ctx, Ref{ClientID: "gone", Symbol: "BTC/USD"})
	if e, ok := err.(*Error); !ok || e.Code != -2011 {
		t.Errorf("cancel of unknown order = %v, want *Error -2011 (got %T)", err, ve)
	}
}

func TestBinanceBadSecretIsRejected(t *testing.T) {
	ins, _ := btc()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"code":-1022,"msg":"Signature for this request is not valid."}`, 400)
	}))
	defer srv.Close()
	b := NewBinance(srv.URL, "k", "s", ins, []Spec{{Symbol: "BTC/USD", VenueSymbol: "BTCUSDT"}})
	_, err := b.Place(context.Background(), Request{Symbol: "BTC/USD", Side: oms.Sell, Type: oms.Limit, Qty: 10_000, Price: 6_500_000})
	if e, ok := err.(*Error); !ok || e.Code != -1022 {
		t.Fatalf("err = %v", err)
	}
}

func TestBinanceStatusMapping(t *testing.T) {
	_, ins := btc()
	b := &Binance{}
	for in, want := range map[string]Status{
		"NEW": StatusOpen, "PARTIALLY_FILLED": StatusPartial, "FILLED": StatusFilled,
		"CANCELED": StatusCanceled, "REJECTED": StatusRejected, "EXPIRED": StatusExpired,
		"EXPIRED_IN_MATCH": StatusExpired, "PENDING_CANCEL": StatusOpen,
	} {
		if got := b.state(binanceOrder{Status: in}, ins).Status; got != want {
			t.Errorf("%s -> %v, want %v", in, got, want)
		}
	}
	// More precision than the gateway keeps is truncated, never rounded up.
	st := b.state(binanceOrder{Status: "FILLED", ExecutedQty: "0.01000099", QuoteQty: "650.00"}, ins)
	if st.CumQty != 10_000 {
		t.Errorf("CumQty = %d, want 10000 (truncated)", st.CumQty)
	}
}

func TestAvgPxRoundsHalfUp(t *testing.T) {
	_, ins := btc()
	// 100.01 / 3 = 33.33666... -> 33.34 at two decimals.
	if got := avgPx("100.01", "3", ins); got != 3334 {
		t.Errorf("avgPx = %d, want 3334", got)
	}
	if got := avgPx("1", "0", ins); got != 0 {
		t.Errorf("zero base should give 0, got %d", got)
	}
}

func TestDeribitFlow(t *testing.T) {
	ins, _ := btc()
	var mu sync.Mutex
	auths := 0
	var last url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/public/auth":
			auths++
			if r.URL.Query().Get("client_secret") != "sec" {
				w.Write([]byte(`{"error":{"code":13004,"message":"invalid_credentials"}}`))
				return
			}
			w.Write([]byte(`{"result":{"access_token":"tok","expires_in":900}}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.Write([]byte(`{"error":{"code":13009,"message":"unauthorized"}}`))
			return
		}
		last = r.URL.Query()
		switch r.URL.Path {
		case "/api/v2/private/buy":
			w.Write([]byte(`{"result":{"order":{"order_id":"ETH-1","order_state":"open","filled_amount":0,"average_price":0,"amount":650}}}`))
		case "/api/v2/private/get_order_state":
			w.Write([]byte(`{"result":{"order_id":"ETH-1","order_state":"open","filled_amount":260,"average_price":65000.5,"amount":650}}`))
		case "/api/v2/private/cancel":
			w.Write([]byte(`{"result":{"order_id":"ETH-1","order_state":"cancelled"}}`))
		}
	}))
	defer srv.Close()

	d := NewDeribit(srv.URL, "id", "sec", ins, []DeribitMarket{{
		Spec: Spec{Symbol: "BTC/USD", VenueSymbol: "BTC-PERPETUAL"}, ContractUSD: 10,
	}})
	ctx := context.Background()

	// 0.01 BTC at 65000 is 650 USD of contracts.
	st, err := d.Place(ctx, Request{ClientID: "c1", Symbol: "BTC/USD", Side: oms.Buy, Type: oms.Limit, TIF: oms.GTC, Qty: 10_000, Price: 6_500_000})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if st.VenueID != "ETH-1" || st.Status != StatusOpen {
		t.Errorf("state = %+v", st)
	}
	if last.Get("amount") != "650" || last.Get("instrument_name") != "BTC-PERPETUAL" || last.Get("price") != "65000.00" ||
		last.Get("time_in_force") != "good_til_cancelled" || last.Get("label") != "c1" {
		t.Errorf("buy params = %v", last)
	}

	// 260 of 650 USD filled is 40%, which is 0.004 BTC.
	st, err = d.Query(ctx, Ref{ClientID: "c1", VenueID: "ETH-1", Symbol: "BTC/USD"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != StatusPartial || st.CumQty != 4000 || st.AvgPx != 6_500_050 {
		t.Errorf("query = %+v, want partial 0.004 @ 65000.50", st)
	}

	if err := d.Cancel(ctx, Ref{VenueID: "ETH-1"}); err != nil {
		t.Errorf("cancel: %v", err)
	}
	if auths != 1 {
		t.Errorf("token fetched %d times, want 1 (cached)", auths)
	}
}

func TestDeribitRefusesWhatItCannotExpress(t *testing.T) {
	ins, _ := btc()
	d := NewDeribit("http://unused", "id", "sec", ins, []DeribitMarket{{
		Spec: Spec{Symbol: "BTC/USD", VenueSymbol: "BTC-PERPETUAL"}, ContractUSD: 10,
	}})
	ctx := context.Background()
	if _, err := d.Place(ctx, Request{Symbol: "BTC/USD", Side: oms.Buy, Type: oms.Market, Qty: 10_000}); err == nil {
		t.Error("market order should be unsupported")
	}
	// 0.0001 BTC at 65000 is 6.5 USD, below the 10 USD contract size.
	if _, err := d.Place(ctx, Request{Symbol: "BTC/USD", Side: oms.Buy, Type: oms.Limit, Qty: 100, Price: 6_500_000}); err == nil {
		t.Error("sub-contract order should be refused")
	}
}

func TestExampleVenuesFileIsValid(t *testing.T) {
	t.Setenv("BINANCE_TESTNET_KEY", "k")
	t.Setenv("BINANCE_TESTNET_SECRET", "s")
	bs, err := LoadFile("../../deploy/venues.example.json", oms.DefaultInstruments())
	if err != nil || len(bs) != 1 || bs[0].Spec.Symbol != "ETH/USD" {
		t.Fatalf("bindings = %+v, err = %v", bs, err)
	}
}

func TestBuildFromConfig(t *testing.T) {
	ins := oms.DefaultInstruments()
	cfg := FileConfig{
		Binance: &VenueEndpoint{KeyEnv: "K", SecretEnv: "S"},
		Routes:  []Route{{Symbol: "ETH/USD", Venue: "binance", VenueSymbol: "ETHUSDT", Tick: "0.01", Step: "0.0001", MinQty: "0.0001"}},
	}
	env := func(k string) string { return map[string]string{"K": "key", "S": "secret"}[k] }
	bs, err := Build(cfg, ins, env)
	if err != nil || len(bs) != 1 || bs[0].Spec.Tick != 1 || bs[0].Spec.Step != 10 || bs[0].Venue.Name() != "binance" {
		t.Fatalf("bindings = %+v, err = %v", bs, err)
	}
	if _, err := Build(cfg, ins, func(string) string { return "" }); err == nil {
		t.Error("missing credentials should be an error")
	}
	bad := cfg
	bad.Routes = []Route{{Symbol: "NOPE/USD", Venue: "binance", VenueSymbol: "X", Tick: "1", Step: "1", MinQty: "1"}}
	if _, err := Build(bad, ins, env); err == nil {
		t.Error("unknown symbol should be an error")
	}
}
