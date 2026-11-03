package backtest

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"

	"github.com/manav2701/fixgw/internal/oms"
)

var header = []string{"op", "account", "clordid", "orig_clordid", "side", "type", "tif", "price", "qty"}

// WriteCSV writes an action log. Prices and quantities are the fixed-point
// integers the engine uses (BTC/USD: price in cents, quantity in 1e-6 BTC), so
// a log replays bit-for-bit.
func WriteCSV(w io.Writer, actions []Action) error {
	cw := csv.NewWriter(w)
	cw.Write(header)
	for _, a := range actions {
		cw.Write([]string{
			a.Op, a.Account, a.ClOrdID, a.OrigClOrdID, string(rune(a.Side)), string(rune(a.Type)), string(rune(a.TIF)),
			strconv.FormatInt(int64(a.Price), 10), strconv.FormatInt(int64(a.Qty), 10),
		})
	}
	cw.Flush()
	return cw.Error()
}

// ReadCSV parses an action log written by WriteCSV.
func ReadCSV(r io.Reader) ([]Action, error) {
	rows, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || len(rows[0]) != len(header) || rows[0][0] != header[0] {
		return nil, fmt.Errorf("backtest: missing or wrong CSV header, want %v", header)
	}
	out := make([]Action, 0, len(rows)-1)
	for i, row := range rows[1:] {
		if len(row) != len(header) {
			return nil, fmt.Errorf("backtest: line %d: %d fields, want %d", i+2, len(row), len(header))
		}
		px, err1 := strconv.ParseInt(row[7], 10, 64)
		qty, err2 := strconv.ParseInt(row[8], 10, 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("backtest: line %d: bad price or qty", i+2)
		}
		a := Action{Op: row[0], Account: row[1], ClOrdID: row[2], OrigClOrdID: row[3], Price: oms.Price(px), Qty: oms.Qty(qty)}
		if row[4] != "" {
			a.Side = oms.Side(row[4][0])
		}
		if row[5] != "" {
			a.Type = oms.OrdType(row[5][0])
		}
		if row[6] != "" {
			a.TIF = oms.TimeInForce(row[6][0])
		}
		out = append(out, a)
	}
	return out, nil
}
