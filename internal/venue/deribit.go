package venue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/manav2701/fixgw/internal/oms"
)

// DeribitTestnet is the Deribit test environment's REST endpoint.
const DeribitTestnet = "https://test.deribit.com"

// DeribitMarket describes one Deribit instrument. Deribit's perpetuals are
// inverse contracts sized in USD, so the gateway quantity (base currency) is
// converted: amount = qty x price, rounded to ContractUSD.
type DeribitMarket struct {
	Spec
	ContractUSD int64 // amount granularity in USD, e.g. 10 for BTC-PERPETUAL
}

// Deribit is a Deribit adapter using the JSON-RPC-over-HTTP interface with
// client-credentials authentication.
type Deribit struct {
	BaseURL  string
	ClientID string
	Secret   string
	HTTP     *http.Client
	Now      func() time.Time

	markets map[string]DeribitMarket
	ins     oms.Instruments

	mu     sync.Mutex
	token  string
	expiry time.Time
	sent   map[string]sentOrder // by ClientID
}

// sentOrder remembers what was sent so venue-side USD amounts can be converted
// back to a gateway quantity.
type sentOrder struct {
	qty    oms.Qty
	amount int64
}

func NewDeribit(baseURL, clientID, secret string, ins oms.Instruments, markets []DeribitMarket) *Deribit {
	d := &Deribit{
		BaseURL: baseURL, ClientID: clientID, Secret: secret, ins: ins,
		HTTP: &http.Client{Timeout: 10 * time.Second}, Now: time.Now,
		markets: map[string]DeribitMarket{}, sent: map[string]sentOrder{},
	}
	for _, m := range markets {
		d.markets[m.Symbol] = m
	}
	return d
}

func (d *Deribit) Name() string { return "deribit" }

type deribitResp struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type deribitOrder struct {
	OrderID      string      `json:"order_id"`
	OrderState   string      `json:"order_state"`
	FilledAmount json.Number `json:"filled_amount"`
	AveragePrice json.Number `json:"average_price"`
	Amount       json.Number `json:"amount"`
}

func (d *Deribit) call(ctx context.Context, path string, q url.Values, auth bool) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.BaseURL+"/api/v2/"+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if auth {
		tok, err := d.accessToken(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var r deribitResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("deribit: HTTP %d: undecodable response", resp.StatusCode)
	}
	if r.Error != nil {
		return nil, &Error{Code: r.Error.Code, Msg: r.Error.Message}
	}
	return r.Result, nil
}

func (d *Deribit) accessToken(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.token != "" && d.Now().Before(d.expiry) {
		return d.token, nil
	}
	raw, err := d.call(ctx, "public/auth", url.Values{
		"grant_type": {"client_credentials"}, "client_id": {d.ClientID}, "client_secret": {d.Secret},
	}, false)
	if err != nil {
		return "", err
	}
	var a struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.AccessToken == "" {
		return "", fmt.Errorf("deribit: bad auth response")
	}
	d.token = a.AccessToken
	// Refresh a minute early.
	d.expiry = d.Now().Add(time.Duration(a.ExpiresIn)*time.Second - time.Minute)
	return d.token, nil
}

func (d *Deribit) market(symbol string) (DeribitMarket, oms.Instrument, error) {
	m, ok := d.markets[symbol]
	ins, iok := d.ins[symbol]
	if !ok || !iok {
		return DeribitMarket{}, oms.Instrument{}, fmt.Errorf("deribit: no mapping for %s", symbol)
	}
	return m, ins, nil
}

func (d *Deribit) Place(ctx context.Context, r Request) (State, error) {
	m, ins, err := d.market(r.Symbol)
	if err != nil {
		return State{}, err
	}
	if r.Type != oms.Limit {
		// Converting a base quantity to USD contracts needs a price.
		return State{}, fmt.Errorf("%w: deribit adapter takes limit orders only", ErrUnsupported)
	}
	// amount = qty x price in USD, rounded to the contract size.
	usd := new(big.Int).Mul(big.NewInt(int64(r.Qty)), big.NewInt(int64(r.Price)))
	usd.Quo(usd, pow10(ins.QtyDecimals+ins.PriceDecimals))
	step := max(m.ContractUSD, 1)
	amount := usd.Int64() / step * step
	if amount < step {
		return State{}, fmt.Errorf("%w: order value below one deribit contract", ErrUnsupported)
	}

	q := url.Values{
		"instrument_name": {m.VenueSymbol},
		"amount":          {fmt.Sprint(amount)},
		"type":            {"limit"},
		"price":           {ins.FormatPrice(r.Price)},
		"label":           {r.ClientID},
	}
	switch r.TIF {
	case oms.IOC:
		q.Set("time_in_force", "immediate_or_cancel")
	case oms.FOK:
		q.Set("time_in_force", "fill_or_kill")
	default:
		q.Set("time_in_force", "good_til_cancelled")
	}
	method := "private/buy"
	if r.Side == oms.Sell {
		method = "private/sell"
	}
	d.mu.Lock()
	d.sent[r.ClientID] = sentOrder{qty: r.Qty, amount: amount}
	d.mu.Unlock()

	raw, err := d.call(ctx, method, q, true)
	if err != nil {
		return State{}, err
	}
	var res struct {
		Order deribitOrder `json:"order"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return State{}, fmt.Errorf("deribit: decode order: %w", err)
	}
	return d.state(res.Order, r.ClientID, ins), nil
}

func (d *Deribit) Cancel(ctx context.Context, ref Ref) error {
	_, err := d.call(ctx, "private/cancel", url.Values{"order_id": {ref.VenueID}}, true)
	return err
}

func (d *Deribit) Query(ctx context.Context, ref Ref) (State, error) {
	_, ins, err := d.market(ref.Symbol)
	if err != nil {
		return State{}, err
	}
	raw, err := d.call(ctx, "private/get_order_state", url.Values{"order_id": {ref.VenueID}}, true)
	if err != nil {
		return State{}, err
	}
	var o deribitOrder
	if err := json.Unmarshal(raw, &o); err != nil {
		return State{}, fmt.Errorf("deribit: decode order: %w", err)
	}
	return d.state(o, ref.ClientID, ins), nil
}

// state converts a Deribit order. order_state is one of open, filled, rejected,
// cancelled, untriggered, archive. Deribit reports executed size in USD, so the
// gateway quantity is the same fraction of the original order.
func (d *Deribit) state(o deribitOrder, clientID string, ins oms.Instrument) State {
	st := State{VenueID: o.OrderID}
	d.mu.Lock()
	sent := d.sent[clientID]
	d.mu.Unlock()

	filled, _ := o.FilledAmount.Float64()
	switch {
	case sent.amount > 0 && int64(filled) >= sent.amount:
		st.CumQty = sent.qty
	case sent.amount > 0 && filled > 0:
		cum := new(big.Int).Mul(big.NewInt(int64(sent.qty)), big.NewInt(int64(filled)))
		cum.Quo(cum, big.NewInt(sent.amount))
		st.CumQty = oms.Qty(cum.Int64())
	}
	if st.CumQty > 0 {
		if px, err := parseTrunc(o.AveragePrice.String(), ins.PriceDecimals); err == nil {
			st.AvgPx = oms.Price(px)
		}
	}

	switch o.OrderState {
	case "open", "untriggered":
		st.Status = StatusOpen
		if st.CumQty > 0 {
			st.Status = StatusPartial
		}
	case "filled":
		st.Status = StatusFilled
	case "cancelled", "archive":
		st.Status = StatusCanceled
	case "rejected":
		st.Status = StatusRejected
	default:
		st.Status = StatusOpen
		st.Text = "unrecognised venue status " + o.OrderState
	}
	return st
}
