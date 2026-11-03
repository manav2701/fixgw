package oms

import "testing"

// FuzzDecimalRoundTrip: whatever ParseDecimal accepts must format back to a
// string that parses to the same value, and nothing may panic.
func FuzzDecimalRoundTrip(f *testing.F) {
	for _, s := range []string{"0", "1.5", "-0.01", "+7", ".5", "5.", "00012.3400", "9223372036854775807", "1e5", "", "-", ".", "1.2.3", "٣"} {
		f.Add(s, 2)
	}
	f.Fuzz(func(t *testing.T, s string, decimals int) {
		if decimals < 0 || decimals > 18 {
			return
		}
		v, err := ParseDecimal(s, decimals)
		if err != nil {
			return
		}
		back, err := ParseDecimal(FormatDecimal(v, decimals), decimals)
		if err != nil || back != v {
			t.Fatalf("ParseDecimal(%q,%d)=%d but its formatting parses to %d (err %v)", s, decimals, v, back, err)
		}
	})
}
