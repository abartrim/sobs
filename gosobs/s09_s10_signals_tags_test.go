package main

// Unit tests for signal label helpers (port of TestMetricsAnomalyDetection
// label cases) and tag-rule matching (port of TestTagRules).

import "testing"

func TestSignalLabelKnownSignals(t *testing.T) {
	cases := []struct {
		source, signal, want string
	}{
		{"logs", "log_volume", "Log Volume"},
		{"logs", "error_volume", "Error Volume"},
		{"logs", "error_ratio", "Error Ratio"},
		{"traces", "trace_volume", "Trace Volume"},
		{"traces", "trace_error_ratio", "Trace Error Ratio"},
		{"traces", "latency_p95_ms", "Latency p95"},
		{"errors", "exception_volume", "Exception Volume"},
		{"rum_vitals", "LCP", "Largest Contentful Paint"},
		{"rum_vitals", "INP", "Interaction to Next Paint"},
		{"rum_vitals", "CLS", "Cumulative Layout Shift"},
		{"rum_vitals", "TTFB", "Time to First Byte"},
		{"rum_vitals", "FCP", "First Contentful Paint"},
		{"rum_vitals", "FID", "First Input Delay"},
	}
	for _, c := range cases {
		if got := signalLabel(c.source, c.signal); got != c.want {
			t.Errorf("signalLabel(%q, %q) = %q, want %q", c.source, c.signal, got, c.want)
		}
	}
}

func TestSignalLabelUnknownSignalFallsBack(t *testing.T) {
	if got := signalLabel("unknown_source", "my_custom_signal"); got != "My Custom Signal" {
		t.Errorf("got %q, want My Custom Signal", got)
	}
}

func TestSignalLabelUnknownSourceKnownSignal(t *testing.T) {
	// "LCP" under a different source must not cross-match by signal name alone.
	if got := signalLabel("other_source", "LCP"); got != "Lcp" {
		t.Errorf("got %q, want Lcp", got)
	}
}

func TestSignalDescriptionKnownSignal(t *testing.T) {
	if desc := signalDescription("logs", "log_volume"); desc == "" {
		t.Error("expected non-empty description")
	}
}

func TestSignalDescriptionUnknownSignalReturnsEmpty(t *testing.T) {
	if desc := signalDescription("unknown_source", "unknown_signal"); desc != "" {
		t.Errorf("got %q, want empty", desc)
	}
}

func TestSourceLabelKnownSources(t *testing.T) {
	cases := map[string]string{
		"logs":       "Logs",
		"traces":     "Traces",
		"errors":     "Errors",
		"rum_vitals": "RUM Vitals",
		"metrics":    "Metrics",
	}
	for source, want := range cases {
		if got := sourceLabel(source); got != want {
			t.Errorf("sourceLabel(%q) = %q, want %q", source, got, want)
		}
	}
}

func TestSourceLabelUnknownSourceFallsBack(t *testing.T) {
	if got := sourceLabel("custom_data_source"); got != "Custom Data Source" {
		t.Errorf("got %q, want Custom Data Source", got)
	}
}

// ---------------------------------------------------------------------------
// Record IDs
// ---------------------------------------------------------------------------

func TestRecordIdForLogStable(t *testing.T) {
	rid1 := recordIdForLog("2026-01-01T00:00:00", "svc", "traceid", "spanid")
	rid2 := recordIdForLog("2026-01-01T00:00:00", "svc", "traceid", "spanid")
	if rid1 != rid2 {
		t.Errorf("not stable: %q != %q", rid1, rid2)
	}
	if len(rid1) != 32 {
		t.Errorf("len = %d, want 32", len(rid1))
	}
}

func TestRecordIdForSpanStable(t *testing.T) {
	rid1 := recordIdForSpan("traceid", "spanid")
	rid2 := recordIdForSpan("traceid", "spanid")
	if rid1 != rid2 {
		t.Errorf("not stable: %q != %q", rid1, rid2)
	}
	if len(rid1) != 32 {
		t.Errorf("len = %d, want 32", len(rid1))
	}
}

func TestRecordIdForLogDiffersByFields(t *testing.T) {
	rid1 := recordIdForLog("2026-01-01T00:00:00", "svc-a", "t1", "s1")
	rid2 := recordIdForLog("2026-01-01T00:00:00", "svc-b", "t1", "s1")
	if rid1 == rid2 {
		t.Error("expected different IDs for different service")
	}
}

// ---------------------------------------------------------------------------
// Tag rule matching
// ---------------------------------------------------------------------------

// match wraps matchTagRule with the trailing spanName/eventType args (the
// Python signature defaults them to "").
func match(rule map[string]any, recordType, service, severity, body string, attrs map[string]any) bool {
	return matchTagRule(rule, recordType, service, severity, body, attrs, "", "")
}

func simpleRule(field, op, value, attrKey string, recordTypes ...string) map[string]any {
	return map[string]any{
		"record_types":   recordTypes,
		"match_field":    field,
		"match_operator": op,
		"match_value":    value,
		"match_attr_key": attrKey,
	}
}

func TestMatchTagRuleEqSeverity(t *testing.T) {
	rule := simpleRule("severity", "eq", "ERROR", "", "log")
	if !match(rule, "log", "svc", "ERROR", "body", map[string]any{}) {
		t.Error("ERROR should match")
	}
	if match(rule, "log", "svc", "WARN", "body", map[string]any{}) {
		t.Error("WARN should not match")
	}
}

func TestMatchTagRuleContainsBody(t *testing.T) {
	rule := simpleRule("body", "contains", "timeout", "", "all")
	if !match(rule, "log", "svc", "ERROR", "connection timeout error", map[string]any{}) {
		t.Error("body with timeout should match")
	}
	if match(rule, "log", "svc", "ERROR", "success", map[string]any{}) {
		t.Error("body without timeout should not match")
	}
}

func TestMatchTagRuleRegex(t *testing.T) {
	rule := simpleRule("service_name", "regex", "^prod-", "", "all")
	if !match(rule, "log", "prod-api", "", "", map[string]any{}) {
		t.Error("prod-api should match")
	}
	if match(rule, "log", "staging-api", "", "", map[string]any{}) {
		t.Error("staging-api should not match")
	}
}

func TestMatchTagRuleAttribute(t *testing.T) {
	rule := simpleRule("attribute", "eq", "500", "http.status_code", "trace")
	if !match(rule, "trace", "svc", "", "", map[string]any{"http.status_code": "500"}) {
		t.Error("status 500 should match")
	}
	if match(rule, "trace", "svc", "", "", map[string]any{"http.status_code": "200"}) {
		t.Error("status 200 should not match")
	}
}

func TestMatchTagRuleWrongRecordType(t *testing.T) {
	rule := simpleRule("severity", "eq", "ERROR", "", "trace")
	if match(rule, "log", "svc", "ERROR", "", map[string]any{}) {
		t.Error("log should not match a trace-only rule")
	}
	if !match(rule, "trace", "svc", "ERROR", "", map[string]any{}) {
		t.Error("trace should match")
	}
}

func TestMatchTagRuleInvalidRegexReturnsFalse(t *testing.T) {
	rule := simpleRule("body", "regex", "[invalid", "", "all")
	if match(rule, "log", "svc", "ERROR", "any body", map[string]any{}) {
		t.Error("invalid regex must return false, not panic")
	}
}

func TestMatchTagRuleCompositeAllConditionsMatch(t *testing.T) {
	rule := simpleRule("severity", "eq", "ERROR", "", "all")
	rule["conditions"] = []map[string]string{
		{"match_field": "severity", "match_operator": "eq", "match_value": "ERROR", "match_attr_key": ""},
		{"match_field": "body", "match_operator": "contains", "match_value": "timeout", "match_attr_key": ""},
	}
	if !match(rule, "log", "svc", "ERROR", "connection timeout error", map[string]any{}) {
		t.Error("both conditions match → true")
	}
	// Legacy field would match (severity=ERROR) but body condition fails → false,
	// proving composite conditions take precedence.
	if match(rule, "log", "svc", "ERROR", "success message", map[string]any{}) {
		t.Error("failing body condition → false")
	}
}

func TestMatchTagRuleCompositeWithAttributeCondition(t *testing.T) {
	rule := simpleRule("severity", "eq", "ERROR", "", "all")
	rule["conditions"] = []map[string]string{
		{"match_field": "severity", "match_operator": "eq", "match_value": "ERROR", "match_attr_key": ""},
		{"match_field": "attribute", "match_operator": "eq", "match_value": "500", "match_attr_key": "http.status_code"},
	}
	if !match(rule, "log", "svc", "ERROR", "", map[string]any{"http.status_code": "500"}) {
		t.Error("status 500 → true")
	}
	if match(rule, "log", "svc", "ERROR", "", map[string]any{"http.status_code": "200"}) {
		t.Error("status 200 → false")
	}
}
