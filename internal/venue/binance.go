package venue

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/manav2701/fixgw/internal/oms"
)

// BinanceTestnet is the Spot testnet REST endpoint.
const BinanceTestnet = "https://testnet.binance.vision"

// Binance is a Binance Spot adapter. It is signed with HMAC-SHA256 as the API
// requires: the signature covers the whole query string.
type Binance struct {
	BaseURL string
	Key     string
	Secret  string
	HTTP    *http.Client
	Now     func() time.Time

	specs map[string]Spec
	ins   oms.Instruments
}

// NewBinance builds an adapter for the given symbols.
func NewBinance(baseURL, key, secret string, ins oms.Instruments, specs []Spec) *Binance {
	b := &Binance{
		BaseURL: baseURL, Key: key, Secret: secret, ins: ins, specs: map[string]Spec{},
		HTTP: &http.Client{Timeout: 10 * time.Second}, Now: time.Now,
	}
	for _, s := range specs {
		b.specs[s.Symbol] = s
	}
	return b
}

func (b *Binance) Name() string { return "binance" }

func (b *Binance) sign(q url.Values) string {
	q.Set("timestamp", strconv.FormatInt(b.Now().UnixMilli(), 10))
	q.Set("recvWindow", "5000")
	enc := q.Encode()
	m := hmac.New(sha256.New, []byte(b.Secret))
	m.Write([]byte(enc))
	return enc + "&signature=" + hex.EncodeToString(m.Sum(nil))
}

type binanceOrder struct {
	OrderID     int64  `json:"orderId"`
	Status      string `json:"status"`
	ExecutedQty string `json:"executedQty"`
	QuoteQty    string `json:"cummulativeQuoteQty"` // sic: Binance's spelling
}

type binanceError struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func (b *Binance) do(ctx context.Context, method, path string, q url.Values) (binanceOrder, error) {
	req, err := http.NewRequestWithContext(ctx, method, b.BaseURL+path+"?"+b.sign(q), nil)
	if err != nil {
		return binanceOrder{}, err
	}
	req.Header.Set("X-MBX-APIKEY", b.Key)
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return binanceOrder{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return binanceOrder{}, err
	}
	if resp.StatusCode >= 400 {
		var e binanceError
		if json.Unmarshal(body, &e) == nil && e.Msg != "" {
			return binanceOrder{}, &Error{Code: e.Code, Msg: e.Msg}
		}
		return binanceOrder{}, fmt.Errorf("binance: HTTP %d", resp.StatusCode)
	}
	var o binanceOrder
	if err := json.Unmarshal(body, &o); err != nil {
		return binanceOrder{}, fmt.Errorf("binance: decode response: %w", err)
	}
	return o, nil
}

func (b *Binance) spec(symbol string) (Spec, oms.Instrument, error) {
	s, ok := b.specs[symbol]
	ins, iok := b.ins[symbol]
	if !ok || !iok {
		return Spec{}, oms.Instrument{}, fmt.Errorf("binance: no mapping for %s", symbol)
	}
	return s, ins, nil
}

func (b *Binance) Place(ctx context.Context, r Request) (State, error) {
	s, ins, err := b.spec(r.Symbol)
	if err != nil {
		return State{}, err
	}
	q := url.Values{}
	q.Set("symbol", s.VenueSymbol)
	q.Set("newClientOrderId", r.ClientID)
	q.Set("quantity", ins.FormatQty(r.Qty))
	q.Set("newOrderRespType", "RESULT")
	if r.Side == oms.Buy {
		q.Set("side", "BUY")
	} else {
		q.Set("side", "SELL")
	}
	switch r.Type {
	case oms.Market:
		q.Set("type", "MARKET")
	case oms.Limit:
		q.Set("type", "LIMIT")
		q.Set("price", ins.FormatPrice(r.Price))
		switch r.TIF {
		case oms.IOC:
			q.Set("timeInForce", "IOC")
		case oms.FOK:
			q.Set("timeInForce", "FOK")
		default:
			q.Set("timeInForce", "GTC")
		}
	default:
		return State{}, ErrUnsupported
	}
	o, err := b.do(ctx, http.MethodPost, "/api/v3/order", q)
	if err != nil {
		return State{}, err
	}
	return b.state(o, ins), nil
}

func (b *Binance) Cancel(ctx context.Context, ref Ref) error {
	s, _, err := b.spec(ref.Symbol)
	if err != nil {
		return err
	}
	q := url.Values{"symbol": {s.VenueSymbol}, "origClientOrderId": {ref.ClientID}}
	_, err = b.do(ctx, http.MethodDelete, "/api/v3/order", q)
	return err
}

func (b *Binance) Query(ctx context.Context, ref Ref) (State, error) {
	s, ins, err := b.spec(ref.Symbol)
	if err != nil {
		return State{}, err
	}
	q := url.Values{"symbol": {s.VenueSymbol}, "origClientOrderId": {ref.ClientID}}
	o, err := b.do(ctx, http.MethodGet, "/api/v3/order", q)
	if err != nil {
		return State{}, err
	}
	return b.state(o, ins), nil
}

// state converts Binance's order object to the neutral State. Binance statuses:
// NEW, PARTIALLY_FILLED, FILLED, CANCELED, PENDING_CANCEL, REJECTED, EXPIRED,
// EXPIRED_IN_MATCH.
func (b *Binance) state(o binanceOrder, ins oms.Instrument) State {
	st := State{VenueID: strconv.FormatInt(o.OrderID, 10)}
	switch o.Status {
	case "NEW", "PENDING_CANCEL", "PENDING_NEW":
		st.Status = StatusOpen
	case "PARTIALLY_FILLED":
		st.Status = StatusPartial
	case "FILLED":
		st.Status = StatusFilled
	case "CANCELED":
		st.Status = StatusCanceled
	case "REJECTED":
		st.Status = StatusRejected
	case "EXPIRED", "EXPIRED_IN_MATCH":
		st.Status = StatusExpired
	default:
		st.Status = StatusOpen
		st.Text = "unrecognised venue status " + o.Status
	}
	if q, err := parseTrunc(o.ExecutedQty, ins.QtyDecimals); err == nil {
		st.CumQty = oms.Qty(q)
	}
	if st.CumQty > 0 {
		st.AvgPx = avgPx(o.QuoteQty, o.ExecutedQty, ins)
	}
	return st
}
