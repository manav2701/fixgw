package venue

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/manav2701/fixgw/internal/oms"
)

// FileConfig is the JSON routing configuration. API credentials are never read
// from the file: it names the environment variables that hold them.
type FileConfig struct {
	Binance *VenueEndpoint `json:"binance,omitempty"`
	Deribit *VenueEndpoint `json:"deribit,omitempty"`
	Routes  []Route        `json:"routes"`
}

// VenueEndpoint locates one venue and its credentials.
type VenueEndpoint struct {
	BaseURL   string `json:"base_url"`
	KeyEnv    string `json:"key_env"`
	SecretEnv string `json:"secret_env"`
}

// Route sends one gateway symbol to one venue. Tick, Step and MinQty are
// decimal strings in the gateway symbol's units.
type Route struct {
	Symbol      string `json:"symbol"`
	Venue       string `json:"venue"` // "binance" or "deribit"
	VenueSymbol string `json:"venue_symbol"`
	Tick        string `json:"tick"`
	Step        string `json:"step"`
	MinQty      string `json:"min_qty"`
	ContractUSD int64  `json:"contract_usd,omitempty"` // deribit only
}

// LoadFile reads a routing file and builds the venue bindings.
func LoadFile(path string, ins oms.Instruments) ([]Binding, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c FileConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("venue: parse %s: %w", path, err)
	}
	return Build(c, ins, os.Getenv)
}

// Build turns a FileConfig into bindings. getenv supplies credentials.
func Build(c FileConfig, ins oms.Instruments, getenv func(string) string) ([]Binding, error) {
	var (
		specs   = map[string][]Spec{}
		markets []DeribitMarket
	)
	for _, r := range c.Routes {
		in, ok := ins[r.Symbol]
		if !ok {
			return nil, fmt.Errorf("venue: route for unknown symbol %q", r.Symbol)
		}
		tick, err1 := in.ParsePrice(r.Tick)
		step, err2 := in.ParseQty(r.Step)
		minQty, err3 := in.ParseQty(r.MinQty)
		if err1 != nil || err2 != nil || err3 != nil || tick <= 0 || step <= 0 {
			return nil, fmt.Errorf("venue: route %s: tick, step and min_qty must be positive decimals within the instrument's precision", r.Symbol)
		}
		if r.VenueSymbol == "" {
			return nil, fmt.Errorf("venue: route %s: venue_symbol is required", r.Symbol)
		}
		sp := Spec{Symbol: r.Symbol, VenueSymbol: r.VenueSymbol, Tick: tick, Step: step, MinQty: minQty}
		specs[r.Venue] = append(specs[r.Venue], sp)
		if r.Venue == "deribit" {
			markets = append(markets, DeribitMarket{Spec: sp, ContractUSD: r.ContractUSD})
		}
	}

	creds := func(name string, e *VenueEndpoint, def string) (base, key, secret string, err error) {
		if e == nil {
			return "", "", "", fmt.Errorf("venue: routes use %s but its section is missing", name)
		}
		base = e.BaseURL
		if base == "" {
			base = def
		}
		key, secret = getenv(e.KeyEnv), getenv(e.SecretEnv)
		if key == "" || secret == "" {
			return "", "", "", fmt.Errorf("venue: %s credentials not set (env %s / %s)", name, e.KeyEnv, e.SecretEnv)
		}
		return base, key, secret, nil
	}

	var out []Binding
	for name, list := range specs {
		var v Venue
		switch name {
		case "binance":
			base, key, secret, err := creds(name, c.Binance, BinanceTestnet)
			if err != nil {
				return nil, err
			}
			v = NewBinance(base, key, secret, ins, list)
		case "deribit":
			base, key, secret, err := creds(name, c.Deribit, DeribitTestnet)
			if err != nil {
				return nil, err
			}
			v = NewDeribit(base, key, secret, ins, markets)
		default:
			return nil, fmt.Errorf("venue: unknown venue %q", name)
		}
		for _, sp := range list {
			out = append(out, Binding{Venue: v, Spec: sp})
		}
	}
	return out, nil
}
