package main

import (
	"errors"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Pure MCP / OTLP-list / RUM-asset / misc helpers — corpus-unreachable or only incidentally hit.
// Oracles: MCP where/time clause builders, urllib-style asset signature payload, httpx exception
// class naming, JSON coercion.

func TestMcpWhere(t *testing.T) {
	if got := mcpWhere(nil); got != "" {
		t.Errorf("empty: %q", got)
	}
	if got := mcpWhere([]string{"a = ?", "b = ?"}); got != "WHERE a = ? AND b = ?" {
		t.Errorf("got %q", got)
	}
}

func TestMcpAddEq(t *testing.T) {
	conds := []string{}
	params := []any{}
	mcpAddEq(&conds, &params, "x = ?", "v")
	mcpAddEq(&conds, &params, "y = ?", "") // empty value -> skipped
	if len(conds) != 1 || conds[0] != "x = ?" || len(params) != 1 || params[0] != "v" {
		t.Errorf("conds=%v params=%v", conds, params)
	}
}

func TestMcpTimeWhere(t *testing.T) {
	// explicit from+to -> two ? predicates + two params
	conds := []string{}
	params := []any{}
	mcpTimeWhere("Timestamp", "2026-01-01 00:00:00", "2026-01-02 00:00:00", &conds, &params)
	if len(conds) != 2 || len(params) != 2 {
		t.Errorf("from+to: conds=%v params=%v", conds, params)
	}
	// no from -> default now()-INTERVAL clause, no param for it
	conds = []string{}
	params = []any{}
	mcpTimeWhere("Timestamp", "", "", &conds, &params)
	if len(conds) != 1 || len(params) != 0 {
		t.Errorf("default: conds=%v params=%v", conds, params)
	}
}

func TestMcpParseTs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"2026-03-29T12:00:00Z", "2026-03-29 12:00:00"},
		{"2026-03-29T12:00:00", "2026-03-29 12:00:00"},
		{"not a ts", ""},
	}
	for _, c := range cases {
		if got := mcpParseTs(c.in); got != c.want {
			t.Errorf("mcpParseTs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMcpErrTypeName(t *testing.T) {
	if got := mcpErrTypeName(nil); got != "Error" {
		t.Errorf("nil: got %q, want Error", got)
	}
	if got := mcpErrTypeName(errors.New("boom")); got != "errorString" {
		t.Errorf("errors.New: got %q, want errorString", got)
	}
}

func TestOtlpFloatIntList(t *testing.T) {
	if got := otlpFloatList([]any{1, 2, 3}); len(got) != 3 {
		t.Errorf("floatList len %d, want 3", len(got))
	}
	if got := otlpFloatList("not a list"); len(got) != 0 {
		t.Errorf("floatList non-list len %d, want 0", len(got))
	}
	if got := otlpIntList([]any{1, 2}); len(got) != 2 {
		t.Errorf("intList len %d, want 2", len(got))
	}
	if got := otlpIntList(nil); len(got) != 0 {
		t.Errorf("intList nil len %d, want 0", len(got))
	}
}

func TestAssetExtension(t *testing.T) {
	cases := []struct{ name, ct, want string }{
		{"logo.PNG", "", "png"},               // from filename ext, lowercased
		{"data.json", "", "json"},             // from filename ext
		{"noext", "application/json", "json"}, // from content-type mapping
		{"noext", "image/jpeg", "jpg"},
		{"noext", "application/octet-stream", "bin"},
	}
	for _, c := range cases {
		if got := assetExtension(c.name, c.ct); got != c.want {
			t.Errorf("assetExtension(%q,%q) = %q, want %q", c.name, c.ct, got, c.want)
		}
	}
}

func TestRumAssetSignaturePayload(t *testing.T) {
	got := rumAssetSignaturePayload("post", "/v1/rum/assets", "1700000000", "abc", "Application/JSON", " Source-Map ", "app.js.map")
	want := "POST\n/v1/rum/assets\n1700000000\nabc\napplication/json\nsource-map\napp.js.map"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "i/o timeout" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return false }

func TestHttpxExceptionClassName(t *testing.T) {
	if got := httpxExceptionClassName(nil); got != "ConnectError" {
		t.Errorf("nil: got %q", got)
	}
	if got := httpxExceptionClassName(errors.New("refused")); got != "ConnectError" {
		t.Errorf("plain: got %q", got)
	}
	if got := httpxExceptionClassName(fakeTimeoutErr{}); got != "ConnectTimeout" {
		t.Errorf("timeout: got %q, want ConnectTimeout", got)
	}
}

func TestAsJSONObject(t *testing.T) {
	o := jsonenc.NewObject().Set("a", "1")
	if got := asJSONObject(o); got != o {
		t.Error("object passthrough")
	}
	if got := asJSONObject(`{"a":1}`); got == nil {
		t.Error("JSON string should parse to object")
	}
	if got := asJSONObject("not json"); got != nil {
		t.Error("non-JSON string -> nil")
	}
	if got := asJSONObject(5); got != nil {
		t.Error("non-string/non-object -> nil")
	}
}

func TestChatLabelFromFirstTurn(t *testing.T) {
	if got := chatLabelFromFirstTurn("  the question  ", "req"); got != "the question" {
		t.Errorf("question wins: %q", got)
	}
	if got := chatLabelFromFirstTurn("", "the request"); got != "the request" {
		t.Errorf("request fallback: %q", got)
	}
	if got := chatLabelFromFirstTurn("", ""); got != "New chat" {
		t.Errorf("default: %q", got)
	}
}
