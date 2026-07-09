package main

import (
	"encoding/json"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// RUM stringify/coerce helpers — pure ports of Python str()/bool()/json.dumps semantics on
// JSON-decoded values (numbers arrive as json.Number under UseNumber). Oracle: _stringify_attrs,
// str(x), str(x) if x else "", bool(x).

func TestRumStr(t *testing.T) {
	if got := rumStr(json.Number("5")); got != "5" {
		t.Errorf("json.Number: got %q, want 5", got)
	}
	if got := rumStr("hello"); got != "hello" {
		t.Errorf("string: got %q, want hello", got)
	}
}

func TestRumFloatStr(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{5.0, "5.0"}, // integral float keeps .0 (Python str(5.0))
		{0.0, "0.0"},
		{-2.0, "-2.0"},
		{3.14, "3.14"},
		{5.5, "5.5"},
	}
	for _, c := range cases {
		if got := rumFloatStr(c.in); got != c.want {
			t.Errorf("rumFloatStr(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRumStringifyAttrs(t *testing.T) {
	in := map[string]any{
		"s":    "x",
		"bt":   true,
		"bf":   false,
		"n":    json.Number("5"),
		"f":    5.0,
		"drop": nil, // None values are dropped
	}
	out := rumStringifyAttrs(in)
	want := map[string]string{"s": "x", "bt": "True", "bf": "False", "n": "5", "f": "5.0"}
	if len(out) != len(want) {
		t.Fatalf("len = %d, want %d (out=%v)", len(out), len(want), out)
	}
	for k, w := range want {
		if got, _ := out[k].(string); got != w {
			t.Errorf("key %q: got %q, want %q", k, got, w)
		}
	}
	if _, ok := out["drop"]; ok {
		t.Error("None value should be dropped")
	}
	// nil map -> empty (non-nil) map.
	if got := rumStringifyAttrs(nil); got == nil || len(got) != 0 {
		t.Errorf("nil input: got %v, want empty map", got)
	}
}

func TestRumStringifyContentAttr(t *testing.T) {
	if got := rumStringifyContentAttr("plain"); got != "plain" {
		t.Errorf("string passthrough: got %q", got)
	}
	if got := rumStringifyContentAttr(true); got != "true" { // json.dumps(True)
		t.Errorf("bool: got %q, want true", got)
	}
}

func TestRumStrOrEmpty(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"x", "x"},
		{true, "True"},
		{false, ""}, // falsy bool -> ""
		{json.Number("0"), ""},
		{json.Number("0.0"), ""},
		{json.Number("5"), "5"},
		{5.0, "5.0"}, // float64 nonzero -> rumFloatStr -> "5.0"
	}
	for _, c := range cases {
		if got := rumStrOrEmpty(c.in); got != c.want {
			t.Errorf("rumStrOrEmpty(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRumStrRaw(t *testing.T) {
	if got := rumStrRaw(nil); got != "" {
		t.Errorf("nil: got %q, want empty", got)
	}
	if got := rumStrRaw(json.Number("7")); got != "7" {
		t.Errorf("number: got %q, want 7", got)
	}
	if got := rumStrRaw("v"); got != "v" {
		t.Errorf("string: got %q, want v", got)
	}
}

func TestRumTruthy(t *testing.T) {
	truthy := []any{true, "x", json.Number("5"), 2.5, []any{1}, map[string]any{"k": 1}}
	for _, v := range truthy {
		if !rumTruthy(v) {
			t.Errorf("rumTruthy(%v) = false, want true", v)
		}
	}
	falsy := []any{nil, false, "", json.Number("0"), json.Number("0.0"), 0.0, []any{}, map[string]any{}}
	for _, v := range falsy {
		if rumTruthy(v) {
			t.Errorf("rumTruthy(%v) = true, want false", v)
		}
	}
}

func TestObjShallowMap(t *testing.T) {
	o := jsonenc.NewObject().Set("a", "1").Set("b", json.Number("2"))
	m := objShallowMap(o)
	if len(m) != 2 || m["a"] != "1" || m["b"] != json.Number("2") {
		t.Errorf("objShallowMap = %v", m)
	}
}
