package main

import (
	"encoding/json"
	"math"
	"testing"
)

// pyFloatStrict mirrors CPython's float(): coerce int/float/bool/numeric-string, RAISE on a
// non-numeric string / None / other type (it must NOT best-effort to 0), with CPython's exact
// ValueError/TypeError text (surfaced as the 400 body by _public_dashboard_query_error).
func TestPyFloatStrict(t *testing.T) {
	ok := []struct {
		name string
		in   any
		want float64
	}{
		{"int", 5, 5.0},
		{"int64", int64(7), 7.0},
		{"float64", 2.5, 2.5},
		{"bool true -> 1.0", true, 1.0},
		{"bool false -> 0.0", false, 0.0},
		{"string int", "42", 42.0},
		{"string float", "3.14", 3.14},
		{"string trimmed", "  6  ", 6.0},
		{"string scientific", "1e3", 1000.0},
		{"string negative", "-2.5", -2.5},
		{"json.Number", json.Number("8.5"), 8.5},
	}
	for _, c := range ok {
		got, err := pyFloatStrict(c.in)
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	// inf / nan are accepted by both float() and ParseFloat.
	if f, err := pyFloatStrict("inf"); err != nil || !math.IsInf(f, 1) {
		t.Errorf(`pyFloatStrict("inf") = (%v, %v), want (+Inf, nil)`, f, err)
	}
	if f, err := pyFloatStrict("nan"); err != nil || !math.IsNaN(f) {
		t.Errorf(`pyFloatStrict("nan") = (%v, %v), want (NaN, nil)`, f, err)
	}

	// Error cases RAISE with CPython's message text.
	errCases := []struct {
		name   string
		in     any
		wantTx string
	}{
		{"non-numeric string", "abc", "could not convert string to float: 'abc'"},
		{"empty string", "", "could not convert string to float: ''"},
		{"nil/None", nil, "float() argument must be a string or a real number, not 'NoneType'"},
	}
	for _, c := range errCases {
		_, err := pyFloatStrict(c.in)
		if err == nil {
			t.Errorf("%s: got nil error, want %q", c.name, c.wantTx)
			continue
		}
		if err.Error() != c.wantTx {
			t.Errorf("%s: error = %q, want %q", c.name, err.Error(), c.wantTx)
		}
	}

	// Any other (non string/number) type is a TypeError, never a silent 0.
	if _, err := pyFloatStrict([]int{1, 2}); err == nil {
		t.Error("slice: got nil error, want TypeError")
	}
}

func TestFloatStrConvErr(t *testing.T) {
	if got := floatStrConvErr("xyz").Error(); got != "could not convert string to float: 'xyz'" {
		t.Errorf("floatStrConvErr(xyz) = %q", got)
	}
}
