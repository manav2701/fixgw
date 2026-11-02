package oms

import "testing"

func TestParseDecimal(t *testing.T) {
	tests := []struct {
		in       string
		decimals int
		want     int64
		err      error
	}{
		{"101.25", 2, 10125, nil},
		{"101", 2, 10100, nil},
		{"101.2", 2, 10120, nil},
		{"0.000001", 6, 1, nil},
		{".5", 2, 50, nil},
		{"1.500", 2, 150, nil}, // trailing zeros beyond precision are harmless
		{"-3.5", 1, -35, nil},
		{"+7", 0, 7, nil},
		{"101.255", 2, 0, ErrDecimalPrecision},
		{"1e5", 2, 0, ErrDecimalSyntax},
		{"", 2, 0, ErrDecimalSyntax},
		{".", 2, 0, ErrDecimalSyntax},
		{"1.2.3", 2, 0, ErrDecimalSyntax},
		{"abc", 2, 0, ErrDecimalSyntax},
		{"99999999999999999999", 2, 0, ErrDecimalSyntax},
	}
	for _, tc := range tests {
		got, err := ParseDecimal(tc.in, tc.decimals)
		if err != tc.err || got != tc.want {
			t.Errorf("ParseDecimal(%q,%d) = %d, %v; want %d, %v", tc.in, tc.decimals, got, err, tc.want, tc.err)
		}
	}
}

func TestFormatDecimalRoundTrip(t *testing.T) {
	for _, v := range []int64{0, 1, 9, 10, 99, 100, 10125, -1, -10125} {
		for _, d := range []int{0, 1, 2, 6} {
			s := FormatDecimal(v, d)
			back, err := ParseDecimal(s, d)
			if err != nil || back != v {
				t.Errorf("round trip v=%d d=%d: %q -> %d, %v", v, d, s, back, err)
			}
		}
	}
	if got := FormatDecimal(5, 2); got != "0.05" {
		t.Errorf("FormatDecimal(5,2) = %q", got)
	}
}

func TestStateMachine(t *testing.T) {
	if !CanTransition(StatusNew, StatusCanceled) {
		t.Error("New -> Canceled must be allowed")
	}
	if CanTransition(StatusFilled, StatusCanceled) {
		t.Error("Filled -> Canceled must not be allowed")
	}
	if CanTransition(StatusCanceled, StatusNew) {
		t.Error("Canceled -> New must not be allowed")
	}
	for _, s := range []OrdStatus{StatusFilled, StatusCanceled, StatusRejected, StatusExpired} {
		if !s.IsTerminal() {
			t.Errorf("%c should be terminal", s)
		}
	}
}

func newOrder(session, id string) Order {
	return Order{Session: session, ClOrdID: id, Symbol: "BTC/USD", Side: Buy, Type: Limit, TIF: GTC, Qty: 1000, Price: 5000000}
}

func TestAcceptAndDuplicate(t *testing.T) {
	r := NewRegistry()
	ex, err := r.Accept(newOrder("s1", "A1"))
	if err != nil {
		t.Fatal(err)
	}
	if ex.ExecType != ExecNew || ex.OrdStatus != StatusNew || ex.LeavesQty != 1000 {
		t.Errorf("unexpected ack: %+v", ex)
	}
	if _, err := r.Accept(newOrder("s1", "A1")); err != ErrDuplicateClOrdID {
		t.Errorf("duplicate ClOrdID: got %v", err)
	}
	// Same ClOrdID on another session is a different order.
	if _, err := r.Accept(newOrder("s2", "A1")); err != nil {
		t.Errorf("same ClOrdID on another session: %v", err)
	}
}

func TestFillsAndAveragePrice(t *testing.T) {
	r := NewRegistry()
	ack, _ := r.Accept(newOrder("s1", "A1")) // Qty 1000
	id := ack.OrderID

	ex, err := r.Fill(id, 400, 5000000)
	if err != nil || ex.ExecType != ExecTrade || ex.OrdStatus != StatusPartiallyFilled || ex.LeavesQty != 600 || ex.CumQty != 400 {
		t.Fatalf("first fill: %+v %v", ex, err)
	}
	ex, err = r.Fill(id, 600, 5000100)
	if err != nil || ex.OrdStatus != StatusFilled || ex.LeavesQty != 0 || ex.CumQty != 1000 {
		t.Fatalf("second fill: %+v %v", ex, err)
	}
	if want := Price(5000060); ex.AvgPx != want { // (400*5000000 + 600*5000100) / 1000
		t.Errorf("AvgPx = %d, want %d", ex.AvgPx, want)
	}
	if ex.Session != "s1" || ex.LastQty != 600 || ex.LastPx != 5000100 {
		t.Errorf("addressing or last-fill fields wrong: %+v", ex)
	}
	if _, err := r.Fill(id, 1, 5000000); err == nil {
		t.Error("filling a filled order must fail")
	}
	if _, _, rej := r.Replace("s1", "A1", "A2", 2000, 5000000); rej == nil || rej.Reason != CxlTooLate {
		t.Errorf("replace of filled order: %+v", rej)
	}
}

func TestFillRejectsOverfill(t *testing.T) {
	r := NewRegistry()
	ack, _ := r.Accept(newOrder("s1", "A1"))
	if _, err := r.Fill(ack.OrderID, 1001, 5000000); err != ErrBadFill {
		t.Errorf("overfill: %v", err)
	}
	if _, err := r.Fill(ack.OrderID, 0, 5000000); err != ErrBadFill {
		t.Errorf("zero fill: %v", err)
	}
	if _, err := r.Fill("nope", 1, 1); err != ErrUnknownOrder {
		t.Errorf("unknown order: %v", err)
	}
}

func TestReplaceChainsClOrdID(t *testing.T) {
	r := NewRegistry()
	r.Accept(newOrder("s1", "A1"))
	r.Fill("O0000000001", 300, 5000000)

	o, ex, rej := r.Replace("s1", "A1", "A2", 800, 5100000)
	if rej != nil {
		t.Fatalf("replace rejected: %+v", rej)
	}
	if o.ClOrdID != "A2" || o.OrigClOrdID != "A1" || o.Qty != 800 || o.LeavesQty != 500 || o.Price != 5100000 {
		t.Errorf("replaced order: %+v", o)
	}
	if ex.ExecType != ExecReplaced || ex.OrdStatus != StatusPartiallyFilled || ex.CumQty != 300 || ex.LeavesQty != 500 {
		t.Errorf("replace report: %+v", ex)
	}
	if _, ok := r.Get("s1", "A1"); ok {
		t.Error("superseded ClOrdID must no longer resolve")
	}
	if _, ok := r.Get("s1", "A2"); !ok {
		t.Error("new ClOrdID must resolve")
	}
	if !r.Seen("s1", "A1") {
		t.Error("superseded ClOrdID must stay burned")
	}
	if _, _, rej := r.Replace("s1", "A2", "A3", 300, 5100000); rej == nil || rej.Reason != CxlOther {
		t.Errorf("replace to CumQty must be rejected: %+v", rej)
	}
	if _, _, rej := r.Replace("s1", "A1", "A4", 900, 5100000); rej == nil || rej.Reason != CxlUnknownOrder || rej.ResponseTo != '2' {
		t.Errorf("replace via old ID: %+v", rej)
	}
}

func TestCancelPaths(t *testing.T) {
	r := NewRegistry()
	if _, err := r.Accept(newOrder("s1", "A1")); err != nil {
		t.Fatal(err)
	}

	ex, rej := r.Cancel("s1", "A1", "C1")
	if rej != nil {
		t.Fatalf("cancel rejected: %+v", rej)
	}
	if ex.ExecType != ExecCanceled || ex.OrdStatus != StatusCanceled || ex.OrigClOrdID != "A1" || ex.ClOrdID != "C1" {
		t.Errorf("unexpected cancel ack: %+v", ex)
	}

	// Cancel again: terminal order -> too late.
	if _, rej := r.Cancel("s1", "A1", "C2"); rej == nil || rej.Reason != CxlTooLate || rej.OrdStatus != StatusCanceled {
		t.Errorf("cancel of canceled order: %+v", rej)
	}
	// Unknown order.
	if _, rej := r.Cancel("s1", "NOPE", "C3"); rej == nil || rej.Reason != CxlUnknownOrder {
		t.Errorf("cancel of unknown order: %+v", rej)
	}
	// Reused cancel ClOrdID.
	if _, rej := r.Cancel("s1", "A1", "C1"); rej == nil || rej.Reason != CxlDuplicateID {
		t.Errorf("duplicate cancel ClOrdID: %+v", rej)
	}
	if len(r.Open()) != 0 {
		t.Error("no open orders expected")
	}
}
