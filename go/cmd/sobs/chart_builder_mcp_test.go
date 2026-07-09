package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Pure chart-builder + MCP-arg helpers. Oracles: Python truthiness (isFalsyAny), `str(v) or default`
// trimming (orStrDefault), _compile_builder_sql guards, int(value) coercion/clamp for MCP args.

func TestIsFalsyAny(t *testing.T) {
	falsy := []any{nil, "", false, json.Number("0"), 0, 0.0}
	for _, v := range falsy {
		if !isFalsyAny(v) {
			t.Errorf("isFalsyAny(%v) = false, want true", v)
		}
	}
	truthy := []any{"x", true, json.Number("5"), 3, 2.5}
	for _, v := range truthy {
		if isFalsyAny(v) {
			t.Errorf("isFalsyAny(%v) = true, want false", v)
		}
	}
}

func TestOrStrDefault(t *testing.T) {
	cases := []struct {
		name    string
		v       any
		present bool
		def     string
		want    string
	}{
		{"absent -> trimmed default", nil, false, "  d  ", "d"},
		{"falsy empty -> default", "", true, "d", "d"},
		{"present string trimmed", "  y  ", true, "d", "y"},
		{"present number", json.Number("5"), true, "d", "5"},
	}
	for _, c := range cases {
		if got := orStrDefault(c.v, c.present, c.def); got != c.want {
			t.Errorf("%s: orStrDefault(%v,%v,%q) = %q, want %q", c.name, c.v, c.present, c.def, got, c.want)
		}
	}
}

func TestCountPerMinuteSeries(t *testing.T) {
	out := countPerMinuteSeries("toStartOfMinute(Timestamp)", "otel_logs", "ServiceName='x'", "scored.* FROM scored")
	for _, want := range []string{
		"WITH per_minute AS", "toStartOfMinute(Timestamp) AS time", "FROM otel_logs",
		"WHERE ServiceName='x'", "baseline_mean", "scored.* FROM scored",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("countPerMinuteSeries output missing %q:\n%s", want, out)
		}
	}
}

func TestCompileBuilderSQLGuards(t *testing.T) {
	if sql, errMsg := compileBuilderSQL("custom_echarts", jsonenc.NewObject()); sql != "" || errMsg != "custom_echarts requires sql.mode='raw'" {
		t.Errorf("custom_echarts: got (%q, %q)", sql, errMsg)
	}
	data := jsonenc.NewObject().Set("source_view", "bogus_view")
	if sql, errMsg := compileBuilderSQL("time_series_percentiles", data); sql != "" || errMsg != "Unsupported source for builder mode" {
		t.Errorf("unsupported source: got (%q, %q)", sql, errMsg)
	}
}

func TestMcpToInt(t *testing.T) {
	ok := []struct {
		in   any
		want int64
	}{
		{json.Number("5"), 5},
		{json.Number("5.9"), 5}, // int(float) truncates toward zero
		{float64(3.7), 3},
		{float64(-2.5), -2}, // trunc toward zero, not floor
		{7, 7},
	}
	for _, c := range ok {
		if n, valid := mcpToInt(c.in); !valid || n != c.want {
			t.Errorf("mcpToInt(%v) = (%d, %v), want (%d, true)", c.in, n, valid, c.want)
		}
	}
	for _, v := range []any{nil, math.NaN(), math.Inf(1)} {
		if n, valid := mcpToInt(v); valid {
			t.Errorf("mcpToInt(%v) = (%d, true), want (_, false)", v, n)
		}
	}
}

func TestMcpClampArg(t *testing.T) {
	o := jsonenc.NewObject().
		Set("mid", json.Number("50")).
		Set("hi", json.Number("200")).
		Set("lo", json.Number("0")).
		Set("bad", "abc")
	if got := mcpClampArg(o, "mid", 1, 100, 10); got != 50 {
		t.Errorf("in-range: got %d, want 50", got)
	}
	if got := mcpClampArg(o, "hi", 1, 100, 10); got != 100 {
		t.Errorf("above hi: got %d, want 100", got)
	}
	if got := mcpClampArg(o, "lo", 1, 100, 10); got != 1 {
		t.Errorf("below lo: got %d, want 1", got)
	}
	if got := mcpClampArg(o, "bad", 1, 100, 10); got != 10 {
		t.Errorf("non-int: got %d, want 10 (default)", got)
	}
	if got := mcpClampArg(o, "missing", 1, 100, 10); got != 10 {
		t.Errorf("missing: got %d, want 10 (default)", got)
	}
}
