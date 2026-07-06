package main

import (
	"encoding/json"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// cov95_b15_fix_mcp_helpers_test.go — batch 15 coverage for cmd/sobs/fix_mcp_helpers.go:
//   mcpClampArg (21)  92.9%
//   mcpToInt (53)     66.7%
//
// chart_builder_mcp_test.go (an earlier batch) already covers the json.Number int/float,
// float64, plain int, nil/NaN/+Inf, and the basic mcpClampArg in-range/hi/lo/non-int/missing
// cases. This file fills the remaining branches: mcpToInt's int64/bool/string forms and
// mcpToInt's -Inf case, plus mcpClampArg's int32-overflow guard (both signs) and the exact
// lo/hi boundary values.

func TestMcpToInt_RemainingBranches(t *testing.T) {
	cases := []struct {
		name   string
		v      any
		wantN  int64
		wantOk bool
	}{
		{"int64 passthrough", int64(123456789), 123456789, true},
		{"bool true -> 1", true, 1, true},
		{"bool false -> 0", false, 0, true},
		{"string numeric", "100", 100, true},
		{"string numeric with surrounding whitespace", "  55  ", 55, true},
		{"string negative", "-7", -7, true},
		{"string float -> ValueError -> false (no float fallback)", "3.9", 0, false},
		{"string non-numeric -> false", "abc", 0, false},
		{"string empty -> false", "", 0, false},
		{"json.Number negative Inf-like garbage -> false", json.Number("-Infinity"), 0, false},
		{"map -> false (TypeError)", map[string]any{"a": 1}, 0, false},
		{"slice -> false (TypeError)", []any{1, 2}, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, ok := mcpToInt(c.v)
			if ok != c.wantOk || (ok && n != c.wantN) {
				t.Errorf("mcpToInt(%#v) = (%d, %v), want (%d, %v)", c.v, n, ok, c.wantN, c.wantOk)
			}
		})
	}
}

func TestMcpClampArg_OverflowAndBoundaries(t *testing.T) {
	build := func(kv ...any) *jsonenc.Object {
		o := jsonenc.NewObject()
		for i := 0; i < len(kv); i += 2 {
			o.Set(kv[i].(string), kv[i+1])
		}
		return o
	}

	cases := []struct {
		name        string
		o           *jsonenc.Object
		key         string
		lo, hi, def int
		want        int
	}{
		{"above int32 max -> default (not clamped to hi)",
			build("limit", json.Number("99999999999")), "limit", 1, 100, 25, 25},
		{"below int32 min -> default (not clamped to lo)",
			build("limit", json.Number("-99999999999")), "limit", 1, 100, 25, 25},
		{"exactly lo boundary passes through unclamped",
			build("limit", json.Number("1")), "limit", 1, 100, 25, 1},
		{"exactly hi boundary passes through unclamped",
			build("limit", json.Number("100")), "limit", 1, 100, 25, 100},
		{"bool true coerces to 1 then clamps within range",
			build("limit", true), "limit", 1, 100, 25, 1},
		{"string form is honored (not just json.Number)",
			build("limit", "42"), "limit", 1, 100, 25, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mcpClampArg(c.o, c.key, c.lo, c.hi, c.def); got != c.want {
				t.Errorf("mcpClampArg(key=%q lo=%d hi=%d def=%d) = %d, want %d",
					c.key, c.lo, c.hi, c.def, got, c.want)
			}
		})
	}
}
