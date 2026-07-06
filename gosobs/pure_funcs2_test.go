package main

// Unit tests for more pure helpers ported from tests/test_app.py:
//   - extractAssistantMeta (TestAIMemory, s04_llm_guard.go)
//   - parseTagRuleConditionsJson (TestTagRules, s09_signals_tags.go)
//   - normalizeGenericUiActionToolCall (TestAIAssistantUIActions, s04_llm_guard.go)

import (
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// extractAssistantMeta
// ---------------------------------------------------------------------------

func TestExtractAssistantMetaSmartQuotesAndSpacing(t *testing.T) {
	answer := "Could you specify which type of telemetry you need? " +
		"<assistant_meta >{“turn_summary”:{“request”:“help me”,“action”:“ask clarification”," +
		"“result”:“requested more detail”},“memory_candidates”:[]}</assistant_meta>"
	cleaned, meta := extractAssistantMeta(answer)
	if strings.Contains(strings.ToLower(cleaned), "assistant_meta") {
		t.Errorf("meta tag leaked into cleaned: %q", cleaned)
	}
	if !strings.HasPrefix(cleaned, "Could you specify") {
		t.Errorf("cleaned = %q", cleaned)
	}
	summary, _ := meta["turn_summary"].(map[string]any)
	if summary == nil || rowString(summary["request"]) != "help me" {
		t.Errorf("turn_summary = %#v, want request 'help me'", meta["turn_summary"])
	}
}

func TestExtractAssistantMetaHtmlEscapedBlock(t *testing.T) {
	answer := "Which page are you referring to? " +
		`&lt;assistant_meta&gt;{"turn_summary":{"request":"navigate me to the airport page",` +
		`"action":"clarification asked","result":"asked which page"},` +
		`"memory_candidates":[]}&lt;/assistant_meta&gt;`
	cleaned, meta := extractAssistantMeta(answer)
	if strings.Contains(strings.ToLower(cleaned), "assistant_meta") {
		t.Errorf("meta tag leaked: %q", cleaned)
	}
	if cleaned != "Which page are you referring to?" {
		t.Errorf("cleaned = %q", cleaned)
	}
	summary, _ := meta["turn_summary"].(map[string]any)
	if summary == nil || rowString(summary["request"]) != "navigate me to the airport page" {
		t.Errorf("turn_summary = %#v", meta["turn_summary"])
	}
}

func TestExtractAssistantMetaStripsMalformedOpenTag(t *testing.T) {
	answer := `I will open the dashboard modal for you. <assistant_meta>{"turn_summary":{"request":"graph ai latency"}`
	cleaned, meta := extractAssistantMeta(answer)
	if strings.Contains(strings.ToLower(cleaned), "assistant_meta") {
		t.Errorf("meta tag leaked: %q", cleaned)
	}
	if cleaned != "I will open the dashboard modal for you." {
		t.Errorf("cleaned = %q", cleaned)
	}
	if len(meta) != 0 {
		t.Errorf("meta = %#v, want empty", meta)
	}
}

// ---------------------------------------------------------------------------
// parseTagRuleConditionsJson
// ---------------------------------------------------------------------------

func TestParseTagRuleConditionsJsonInvalidPayloads(t *testing.T) {
	for _, raw := range []string{"", "{bad-json", `{"match_field":"severity"}`} {
		if got := parseTagRuleConditionsJson(raw); len(got) != 0 {
			t.Errorf("parseTagRuleConditionsJson(%q) = %#v, want empty", raw, got)
		}
	}
}

func TestParseTagRuleConditionsJsonValid(t *testing.T) {
	got := parseTagRuleConditionsJson(`[{"match_field":"severity","match_operator":"eq","match_value":"ERROR"}]`)
	want := []map[string]string{
		{"match_field": "severity", "match_operator": "eq", "match_value": "ERROR", "match_attr_key": ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// ---------------------------------------------------------------------------
// normalizeGenericUiActionToolCall
// ---------------------------------------------------------------------------

func TestNormalizeActionAllowsCrossPageNav(t *testing.T) {
	normalized := normalizeGenericUiActionToolCall(map[string]any{
		"action_id":   "summary.nav.ai",
		"target_page": "/ai",
		"arguments":   map[string]any{},
		"notes":       "Navigate to AI page",
	}, "/")
	if normalized == nil {
		t.Fatal("normalized is nil")
	}
	if normalized["unsupported"] != false {
		t.Errorf("unsupported = %v, want false", normalized["unsupported"])
	}
	action, _ := normalized["action"].(map[string]any)
	if action == nil || action["type"] != "navigate" || action["target_page"] != "/ai" {
		t.Errorf("action = %#v, want navigate → /ai", normalized["action"])
	}
}

func TestNormalizeActionRejectsUnknownAiFilterFields(t *testing.T) {
	normalized := normalizeGenericUiActionToolCall(map[string]any{
		"action_id":   "ai.filter.apply",
		"target_page": "/ai",
		"arguments": map[string]any{
			"filters": map[string]any{"hours": "1", "chart": "response_time"},
			"submit":  true,
		},
		"notes": "Set time range to last hour and show AI model response times",
	}, "/ai")
	if normalized == nil {
		t.Fatal("normalized is nil")
	}
	if normalized["unsupported"] != true {
		t.Errorf("unsupported = %v, want true", normalized["unsupported"])
	}
	if normalized["requires_confirmation"] != false {
		t.Errorf("requires_confirmation = %v, want false", normalized["requires_confirmation"])
	}
	action, _ := normalized["action"].(map[string]any)
	if action == nil || action["type"] != "unsupported" {
		t.Errorf("action = %#v, want type unsupported", normalized["action"])
	}
}
