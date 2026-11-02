package oms

import (
	"errors"
	"strconv"
	"strings"
)

var (
	ErrDecimalSyntax    = errors.New("oms: malformed decimal")
	ErrDecimalPrecision = errors.New("oms: too many decimal places")
)

// ParseDecimal converts a decimal string such as "101.25" to a fixed-point
// integer with the given number of decimal places, without using floats.
// Input with more significant fractional digits than decimals is rejected.
func ParseDecimal(s string, decimals int) (int64, error) {
	if decimals < 0 {
		return 0, ErrDecimalSyntax
	}
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg, s = true, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	intPart, frac, hasDot := strings.Cut(s, ".")
	if intPart == "" && (!hasDot || frac == "") {
		return 0, ErrDecimalSyntax
	}
	if !allDigits(intPart) || !allDigits(frac) {
		return 0, ErrDecimalSyntax
	}
	frac = strings.TrimRight(frac, "0")
	if len(frac) > decimals {
		return 0, ErrDecimalPrecision
	}
	if intPart == "" {
		intPart = "0"
	}
	v, err := strconv.ParseInt(intPart+frac+strings.Repeat("0", decimals-len(frac)), 10, 64)
	if err != nil {
		return 0, ErrDecimalSyntax
	}
	if neg {
		v = -v
	}
	return v, nil
}

// FormatDecimal renders a fixed-point integer as a decimal string with exactly
// the given number of decimal places.
func FormatDecimal(v int64, decimals int) string {
	neg := v < 0
	var digits string
	if neg {
		digits = strconv.FormatUint(uint64(-(v+1))+1, 10)
	} else {
		digits = strconv.FormatInt(v, 10)
	}
	if decimals > 0 {
		if len(digits) <= decimals {
			digits = strings.Repeat("0", decimals-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-decimals] + "." + digits[len(digits)-decimals:]
	}
	if neg {
		return "-" + digits
	}
	return digits
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
