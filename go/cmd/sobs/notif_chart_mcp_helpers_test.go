package main

import (
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Final complex-but-pure helpers: notification-condition normalization/parse, chart-option
// placeholder extraction, MCP map coercion. Oracles: _normalize_notification_condition,
// _infer/_extract placeholder scan ({{key}}), MCP _normalize_map_value.

func TestParseNotificationConditionsJSON(t *testing.T) {
	if got := parseNotificationConditionsJSON(""); len(got) != 0 {
		t.Errorf("empty -> %v, want []", got)
	}
	if got := parseNotificationConditionsJSON("not json"); len(got) != 0 {
		t.Errorf("bad json -> %v, want []", got)
	}
	if got := parseNotificationConditionsJSON(`{"not":"a list"}`); len(got) != 0 {
		t.Errorf("non-list -> %v, want []", got)
	}
	if got := parseNotificationConditionsJSON(`[{"type":"signal","threshold":5}]`); len(got) != 1 {
		t.Errorf("one valid condition -> len %d, want 1", len(got))
	}
	// a non-object list entry normalizes to nil and is skipped
	if got := parseNotificationConditionsJSON(`["junk", {"type":"signal"}]`); len(got) != 1 {
		t.Errorf("mixed list -> len %d, want 1", len(got))
	}
}

func TestNormalizeNotificationCondition(t *testing.T) {
	if got := normalizeNotificationCondition("not an object"); got != nil {
		t.Errorf("non-object -> %v, want nil", got)
	}
	o := jsonenc.NewObject().Set("type", "signal").Set("threshold", jsonenc.NewObject())
	c := normalizeNotificationCondition(o)
	if c == nil {
		t.Fatal("valid object -> nil, want non-nil map")
	}
	if len(c) == 0 {
		t.Error("normalized condition should be a populated map")
	}
}

func TestExtractChartOptionPlaceholders(t *testing.T) {
	if got := extractChartOptionPlaceholders(""); got != nil {
		t.Errorf("empty -> %v, want nil", got)
	}
	got := extractChartOptionPlaceholders(`{"a":"{{rows}}","b":"{{ cols }}","c":"{{rows}}"}`)
	if len(got) != 2 || got[0] != "rows" || got[1] != "cols" {
		t.Errorf("got %v, want [rows cols] (deduped, ordered, whitespace-trimmed)", got)
	}
}

func TestMcpNormalizeMap(t *testing.T) {
	// *jsonenc.Object passthrough
	o := jsonenc.NewObject().Set("a", 1)
	if got := mcpNormalizeMap(o); got != o {
		t.Error("object passthrough")
	}
	// Go map -> object with same keys
	if got := mcpNormalizeMap(map[string]any{"k": "v"}); got == nil {
		t.Error("map should convert")
	} else if v, _ := got.Get("k"); v != "v" {
		t.Errorf("map key: got %v, want v", v)
	}
	// JSON string -> object
	if got := mcpNormalizeMap(`{"a":1}`); got == nil {
		t.Error("JSON string should parse")
	} else if v, _ := got.Get("a"); v == nil {
		t.Error("parsed object missing key a")
	}
	// non-parseable / wrong type -> empty (non-nil) object
	for _, raw := range []any{"not json", nil, 5} {
		got := mcpNormalizeMap(raw)
		if got == nil || len(got.Keys()) != 0 {
			t.Errorf("mcpNormalizeMap(%v) = %v, want empty object", raw, got)
		}
	}
}
