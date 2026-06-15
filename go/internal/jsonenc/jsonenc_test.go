package jsonenc

import "testing"

// TestPyFloatRepr pins PyFloatRepr to CPython's repr(float)/json.dumps(float) output.
// The values cover the scientific-notation boundary (decpt <= -4 || decpt > 16), the band
// where Go's strconv 'g' wrongly switched to exponent (1e6..1e16), whole-number ".0",
// negative zero, and small fractions. Each "want" is literally repr(v) in CPython 3.
func TestPyFloatRepr(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.0"},
		{1, "1.0"},
		{1.5, "1.5"},
		{250.0, "250.0"},
		{0.1, "0.1"},
		{0.001, "0.001"},
		{0.0001, "0.0001"},
		{0.00001, "1e-05"},
		{1000000.0, "1000000.0"},     // Go 'g' gave "1e+06"
		{1500000.0, "1500000.0"},     // Go 'g' gave "1.5e+06"
		{123456789.0, "123456789.0"}, // Go 'g' gave "1.23456789e+08"
		{1e15, "1000000000000000.0"},
		{1e16, "1e+16"},
		{1e17, "1e+17"},
		{9999999999999998.0, "9999999999999998.0"},
		{2500000.5, "2500000.5"},
		{3.14159, "3.14159"},
		{-2.5, "-2.5"},
		{1e-7, "1e-07"},
		{1e20, "1e+20"},
		{6.022e23, "6.022e+23"},
		{9.109e-31, "9.109e-31"},
		{1234567890123456.0, "1234567890123456.0"},
		{100.0, "100.0"},
		{-0.0001, "-0.0001"},
		{0.3333333333333333, "0.3333333333333333"},
	}
	for _, c := range cases {
		if got := PyFloatRepr(c.in); got != c.want {
			t.Errorf("PyFloatRepr(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	// Negative zero keeps its sign, like repr(-0.0).
	if got := PyFloatRepr(negZero()); got != "-0.0" {
		t.Errorf("PyFloatRepr(-0.0) = %q, want %q", got, "-0.0")
	}
}

func negZero() float64 {
	z := 0.0
	return -z
}
