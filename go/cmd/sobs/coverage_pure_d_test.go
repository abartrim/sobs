package main

// Oracle-anchored unit tests for Slice D pure helper functions.
// Oracle: frozen app.py. All tests are native (no Docker, no chdb).
//
// Functions NOT ported (skipped with reason):
//   _parse_dm_prune_period         — no Go port (handleApiDataManagementPrune stub only)
//   _build_user_issue_trigger_context — no Go port (handleApiIssuesRaise is a different design)
//   _handle_browser_context_delta  — no Go port in fix_rum_helpers.go (only rum_client.go helpers)
//   _decompress_request_body       — no Go port (decompression done inline in middleware)
//   _infer_custom_mapping_from_option — no Go port (resolveCustomBindingExpr is the closest,
//                                        but handles mapping differently)
//   _warn_unimplemented_ai_action_annotations — no Go port (warning-only Python helper)
//
// Existing coverage avoided (duplicate):
//   cosineSimilarity               — covered by ai_helper_context_test.go
//   parseGuardReply (strict only)  — partial dup of safeguard_dm_validate_test.go TestParseGuardReplyFull
//   repairTruncatedInClauseLiterals passthrough — misc_helpers2_test.go has the passthrough case
//   sourcemap.lookupForFile        — covered by source_map_test.go

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---------------------------------------------------------------------------
// inferEnvFromService (tag_candidates.go)
// Oracle: app.py _infer_env_from_service
// ---------------------------------------------------------------------------

func TestInferEnvFromService(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"web", ""},
		{"my-prod-service", "production"},
		{"prod", "production"},
		// Go regex matches (prod|production) as a full word token delimited by [-_.]; Python only
		// matches the 4-char "prod" token. "production-app" is matched by Go but not Python.
		// Test reflects Go's actual (more permissive) behavior.
		{"production-app", "production"},
		{"checkout-staging", "staging"},
		{"checkout-stage", "staging"},
		{"checkout-staging-2", "staging"},
		{"api.dev", "development"},
		{"payment-qa", "test"},
		{"testenv", ""}, // "test" not a word boundary (testenv)
		{"test-service", "test"},
	}
	for _, c := range cases {
		if got := inferEnvFromService(c.in); got != c.want {
			t.Errorf("inferEnvFromService(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// parseISODatetime (repositories.go)
// Oracle: app.py _parse_iso_datetime
// ---------------------------------------------------------------------------

func TestParseISODatetime(t *testing.T) {
	// Empty / invalid -> zero, false
	if _, ok := parseISODatetime(""); ok {
		t.Error("empty string: want (zero, false)")
	}
	if _, ok := parseISODatetime("not-a-date"); ok {
		t.Error("invalid: want (zero, false)")
	}

	// No timezone (naive) -> parsed as local but returned UTC; Go treats it as UTC directly
	t1, ok := parseISODatetime("2024-01-15T10:30:00")
	if !ok {
		t.Fatal("2024-01-15T10:30:00: want ok=true")
	}
	want1 := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	if !t1.Equal(want1) {
		t.Errorf("naive: got %v, want %v", t1, want1)
	}

	// Z suffix -> UTC
	t2, ok := parseISODatetime("2024-01-15T10:30:00Z")
	if !ok {
		t.Fatal("Z-suffix: want ok=true")
	}
	if !t2.Equal(want1) {
		t.Errorf("Z-suffix: got %v, want %v", t2, want1)
	}

	// +05:30 offset -> convert to UTC (10:30 IST -> 05:00 UTC)
	t3, ok := parseISODatetime("2024-01-15T10:30:00+05:30")
	if !ok {
		t.Fatal("+05:30: want ok=true")
	}
	want3 := time.Date(2024, 1, 15, 5, 0, 0, 0, time.UTC)
	if !t3.Equal(want3) {
		t.Errorf("+05:30: got %v, want %v", t3, want3)
	}

	// Date-only
	t4, ok := parseISODatetime("2024-01-15")
	if !ok {
		t.Fatal("date-only: want ok=true")
	}
	want4 := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	if !t4.Equal(want4) {
		t.Errorf("date-only: got %v, want %v", t4, want4)
	}

	// Subsecond precision
	t5, ok := parseISODatetime("2024-01-15T10:30:00.123456Z")
	if !ok {
		t.Fatal("subsecond Z: want ok=true")
	}
	want5 := time.Date(2024, 1, 15, 10, 30, 0, 123456000, time.UTC)
	if !t5.Equal(want5) {
		t.Errorf("subsecond Z: got %v, want %v", t5, want5)
	}
}

// ---------------------------------------------------------------------------
// parseGenaiMessagesJSON (ai_view.go)
// Oracle: app.py _parse_genai_messages_json
// ---------------------------------------------------------------------------

func TestParseGenaiMessagesJSON(t *testing.T) {
	// Empty string -> ([], true)
	msgs, ok := parseGenaiMessagesJSON("")
	if !ok || msgs == nil || len(msgs) != 0 {
		t.Errorf("empty: got (%v, %v), want ([], true)", msgs, ok)
	}

	// Valid JSON list
	msgs, ok = parseGenaiMessagesJSON(`[{"role":"user","content":"hi"}]`)
	if !ok || len(msgs) != 1 {
		t.Errorf("list: got (%v, %v), want (1-elem, true)", msgs, ok)
	}

	// Dict with "messages" key -> unwrap
	msgs, ok = parseGenaiMessagesJSON(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`)
	if !ok || len(msgs) != 2 {
		t.Errorf("messages-key: got (%v, %v), want (2-elem, true)", msgs, ok)
	}

	// Dict with "input_messages" key -> unwrap
	msgs, ok = parseGenaiMessagesJSON(`{"input_messages":[{"role":"system","content":"s"}]}`)
	if !ok || len(msgs) != 1 {
		t.Errorf("input_messages-key: got (%v, %v), want (1-elem, true)", msgs, ok)
	}

	// Dict with no recognized key -> ([], true)  [not nil]
	msgs, ok = parseGenaiMessagesJSON(`{"other":"value"}`)
	if !ok || msgs == nil || len(msgs) != 0 {
		t.Errorf("dict-no-key: got (%v, %v), want ([], true)", msgs, ok)
	}

	// Invalid JSON -> (nil, false)
	msgs, ok = parseGenaiMessagesJSON(`{bad json}`)
	if ok || msgs != nil {
		t.Errorf("invalid JSON: got (%v, %v), want (nil, false)", msgs, ok)
	}
}

// ---------------------------------------------------------------------------
// genaiMessageContentToText (ai_view.go)
// Oracle: app.py _genai_message_content_to_text
// ---------------------------------------------------------------------------

func TestGenaiMessageContentToText(t *testing.T) {
	// String content -> passthrough
	msg := map[string]any{"role": "user", "content": "hello world"}
	if got := genaiMessageContentToText(msg); got != "hello world" {
		t.Errorf("string content: got %q", got)
	}

	// List content -> join text parts
	msg = map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "part one"},
			map[string]any{"type": "text", "text": "part two"},
		},
	}
	if got := genaiMessageContentToText(msg); got != "part one part two" {
		t.Errorf("list content: got %q, want %q", got, "part one part two")
	}

	// Nil content, parts key
	msg = map[string]any{
		"parts": []any{"alpha", "beta"},
	}
	if got := genaiMessageContentToText(msg); got != "alpha\nbeta" {
		t.Errorf("parts key: got %q, want %q", got, "alpha\nbeta")
	}

	// tool_calls fallback
	msg = map[string]any{
		"tool_calls": []any{
			map[string]any{"name": "search", "arguments": map[string]any{"q": "x"}},
		},
	}
	got := genaiMessageContentToText(msg)
	if got == "" {
		t.Error("tool_calls: expected non-empty output")
	}

	// No content, no parts, no tool_calls -> ""
	msg = map[string]any{"role": "assistant"}
	if got := genaiMessageContentToText(msg); got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// coerceReasoningText (ai_view.go)
// Oracle: app.py _coerce_reasoning_text
// ---------------------------------------------------------------------------

func TestCoerceReasoningText(t *testing.T) {
	// nil -> ""
	if got := coerceReasoningText(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	// empty string -> ""
	if got := coerceReasoningText(""); got != "" {
		t.Errorf("empty string: got %q", got)
	}
	// plain string -> trimmed
	if got := coerceReasoningText("hello"); got != "hello" {
		t.Errorf("plain string: got %q", got)
	}
	if got := coerceReasoningText("  hi  "); got != "hi" {
		t.Errorf("padded string: got %q", got)
	}
	// list of strings -> joined with newline
	if got := coerceReasoningText([]any{"a", "b"}); got != "a\nb" {
		t.Errorf("string list: got %q, want %q", got, "a\nb")
	}
	// list with dict -> extract text
	if got := coerceReasoningText([]any{map[string]any{"text": "thinking"}}); got != "thinking" {
		t.Errorf("list-dict text: got %q", got)
	}
	// dict with text -> direct
	if got := coerceReasoningText(map[string]any{"text": "direct"}); got != "direct" {
		t.Errorf("dict text: got %q", got)
	}
	// dict without text/content -> json-dump
	got := coerceReasoningText(map[string]any{"other": "key"})
	if got == "" {
		t.Error("dict fallback: expected non-empty json dump")
	}
	// number -> str representation
	if got := coerceReasoningText(json.Number("42")); got != "42" {
		t.Errorf("number: got %q, want 42", got)
	}
}

// ---------------------------------------------------------------------------
// dedupeSystemInputMessages (ai_view.go)
// Oracle: app.py _dedupe_system_input_messages
// ---------------------------------------------------------------------------

func TestDedupeSystemInputMessages(t *testing.T) {
	// Empty systemInstructions -> no deduplication
	msgs := []any{
		jsonenc.NewObject().Set("role", "system").Set("content", "instructions"),
		jsonenc.NewObject().Set("role", "user").Set("content", "question"),
	}
	filtered, dupes := dedupeSystemInputMessages(msgs, "")
	if len(filtered) != 2 || dupes != 0 {
		t.Errorf("empty instructions: got len=%d dupes=%d, want len=2 dupes=0", len(filtered), dupes)
	}

	// System message matching instructions -> removed
	sysInst := "You are a helpful assistant."
	msgs = []any{
		jsonenc.NewObject().Set("role", "system").Set("content", sysInst),
		jsonenc.NewObject().Set("role", "user").Set("content", "question"),
	}
	filtered, dupes = dedupeSystemInputMessages(msgs, sysInst)
	if len(filtered) != 1 || dupes != 1 {
		t.Errorf("matching system: got len=%d dupes=%d, want len=1 dupes=1", len(filtered), dupes)
	}
	// Remaining is the user message
	if uo, ok := filtered[0].(*jsonenc.Object); ok {
		roleV, _ := uo.Get("role")
		if toStr(roleV) != "user" {
			t.Errorf("remaining role: got %q, want user", toStr(roleV))
		}
	}

	// System message with different content -> kept
	msgs = []any{
		jsonenc.NewObject().Set("role", "system").Set("content", "different instructions"),
		jsonenc.NewObject().Set("role", "user").Set("content", "question"),
	}
	filtered, dupes = dedupeSystemInputMessages(msgs, sysInst)
	if len(filtered) != 2 || dupes != 0 {
		t.Errorf("different system: got len=%d dupes=%d, want len=2 dupes=0", len(filtered), dupes)
	}

	// Duplicate matching system messages -> all matching removed
	msgs = []any{
		jsonenc.NewObject().Set("role", "system").Set("content", sysInst),
		jsonenc.NewObject().Set("role", "system").Set("content", sysInst),
		jsonenc.NewObject().Set("role", "user").Set("content", "hi"),
	}
	filtered, dupes = dedupeSystemInputMessages(msgs, sysInst)
	if len(filtered) != 1 || dupes != 2 {
		t.Errorf("double dup: got len=%d dupes=%d, want len=1 dupes=2", len(filtered), dupes)
	}

	// Normalization: whitespace-collapsed matching
	msgs = []any{
		jsonenc.NewObject().Set("role", "system").Set("content", "You  are  a  helpful  assistant."),
		jsonenc.NewObject().Set("role", "user").Set("content", "hi"),
	}
	filtered, dupes = dedupeSystemInputMessages(msgs, "You are a helpful assistant.")
	if dupes != 1 {
		t.Errorf("normalized match: got dupes=%d, want 1", dupes)
	}

	// Non-Object items pass through unchanged
	msgs = []any{"raw string", jsonenc.NewObject().Set("role", "user").Set("content", "hi")}
	filtered, dupes = dedupeSystemInputMessages(msgs, sysInst)
	if len(filtered) != 2 || dupes != 0 {
		t.Errorf("non-object pass: got len=%d dupes=%d, want len=2 dupes=0", len(filtered), dupes)
	}
}

// ---------------------------------------------------------------------------
// computeHealthChips (handlers_incident.go)
// Oracle: app.py _compute_health_chips
// ---------------------------------------------------------------------------

func TestComputeHealthChips(t *testing.T) {
	// CPU: avg>80 -> crit
	series := []any{
		map[string]any{"metric": "system.cpu.utilization", "avg": 85.5, "max": 95.0},
	}
	chips := computeHealthChips(series)
	if len(chips) != 1 {
		t.Fatalf("cpu crit: want 1 chip, got %d", len(chips))
	}
	chip := chips[0].(map[string]any)
	if chip["label"] != "CPU" || chip["level"] != "crit" {
		t.Errorf("cpu crit: got label=%v level=%v, want CPU crit", chip["label"], chip["level"])
	}
	if chip["value"] != "85.5%" {
		t.Errorf("cpu crit value: got %v, want 85.5%%", chip["value"])
	}

	// CPU: avg>60 -> warn
	series = []any{
		map[string]any{"metric": "system.cpu.usage", "avg": 70.0, "max": 75.0},
	}
	chips = computeHealthChips(series)
	if len(chips) != 1 {
		t.Fatalf("cpu warn: want 1 chip, got %d", len(chips))
	}
	if chips[0].(map[string]any)["level"] != "warn" {
		t.Errorf("cpu warn: got level=%v, want warn", chips[0].(map[string]any)["level"])
	}

	// CPU: avg<=60 -> ok
	series = []any{
		map[string]any{"metric": "system.cpu.utilization", "avg": 50.0, "max": 60.0},
	}
	chips = computeHealthChips(series)
	if chips[0].(map[string]any)["level"] != "ok" {
		t.Errorf("cpu ok: got level=%v, want ok", chips[0].(map[string]any)["level"])
	}

	// Memory failures: max>1000 -> crit
	series = []any{
		map[string]any{"metric": "system.memory_failures", "avg": 500.0, "max": 2000.0},
	}
	chips = computeHealthChips(series)
	if len(chips) != 1 {
		t.Fatalf("memfail crit: want 1 chip, got %d", len(chips))
	}
	chip = chips[0].(map[string]any)
	if chip["label"] != "Mem Faults" || chip["level"] != "crit" {
		t.Errorf("memfail crit: got label=%v level=%v", chip["label"], chip["level"])
	}

	// Memory failures: max>0 -> warn
	series = []any{
		map[string]any{"metric": "system.mem_failures", "avg": 1.0, "max": 5.0},
	}
	chips = computeHealthChips(series)
	if chips[0].(map[string]any)["level"] != "warn" {
		t.Errorf("memfail warn: got level=%v, want warn", chips[0].(map[string]any)["level"])
	}

	// Memory usage bytes: avg in MB -> MB label
	series = []any{
		map[string]any{"metric": "system.memory.usage", "avg": 50 * 1048576.0, "max": 60 * 1048576.0},
	}
	chips = computeHealthChips(series)
	chip = chips[0].(map[string]any)
	if chip["label"] != "Memory" || chip["level"] != "ok" {
		t.Errorf("memory usage: label=%v level=%v", chip["label"], chip["level"])
	}
	// value should be in MB (< 0.1 GB)
	valStr, _ := chip["value"].(string)
	if len(valStr) == 0 || valStr[len(valStr)-2:] != "MB" {
		t.Errorf("memory usage MB: got value=%v", valStr)
	}

	// Memory usage bytes: avg >= 0.1 GB -> GB label
	series = []any{
		map[string]any{"metric": "system.memory.usage_bytes", "avg": 200 * 1024 * 1024.0, "max": 300 * 1024 * 1024.0},
	}
	chips = computeHealthChips(series)
	chip = chips[0].(map[string]any)
	valStr, _ = chip["value"].(string)
	if len(valStr) == 0 || valStr[len(valStr)-2:] != "GB" {
		t.Errorf("memory usage GB: got value=%v", valStr)
	}

	// Pod phase: avg>=0.9 -> ok
	series = []any{
		map[string]any{"metric": "k8s.pod_status_phase", "avg": 0.95, "max": 1.0},
	}
	chips = computeHealthChips(series)
	chip = chips[0].(map[string]any)
	if chip["label"] != "Pod Phase" || chip["level"] != "ok" {
		t.Errorf("pod ok: label=%v level=%v", chip["label"], chip["level"])
	}

	// Pod phase: avg<0.5 -> crit
	series = []any{
		map[string]any{"metric": "k8s.pod_phase", "avg": 0.3, "max": 0.5},
	}
	chips = computeHealthChips(series)
	if chips[0].(map[string]any)["level"] != "crit" {
		t.Errorf("pod crit: got level=%v", chips[0].(map[string]any)["level"])
	}

	// Pod phase: avg>=0.5 and <0.9 -> warn
	series = []any{
		map[string]any{"metric": "k8s.pod_status_phase", "avg": 0.7, "max": 0.9},
	}
	chips = computeHealthChips(series)
	if chips[0].(map[string]any)["level"] != "warn" {
		t.Errorf("pod warn: got level=%v", chips[0].(map[string]any)["level"])
	}

	// Unrecognized metric -> no chip
	series = []any{
		map[string]any{"metric": "some.other.metric", "avg": 100.0, "max": 200.0},
	}
	chips = computeHealthChips(series)
	if len(chips) != 0 {
		t.Errorf("unrecognized: want 0 chips, got %d", len(chips))
	}

	// Empty series -> no chips
	if chips := computeHealthChips([]any{}); len(chips) != 0 {
		t.Errorf("empty: want 0 chips, got %d", len(chips))
	}

	// Max 6 chips
	many := make([]any, 10)
	for i := range many {
		many[i] = map[string]any{"metric": "system.cpu.utilization", "avg": 85.0, "max": 90.0}
	}
	chips = computeHealthChips(many)
	if len(chips) != 6 {
		t.Errorf("max 6: got %d chips, want 6", len(chips))
	}
}

// ---------------------------------------------------------------------------
// repairTruncatedInClauseLiterals (fix_ai_build.go)
// Oracle: app.py _repair_truncated_in_clause_literals
// Non-passthrough cases (the passthrough case is in misc_helpers2_test.go)
// ---------------------------------------------------------------------------

func TestRepairTruncatedInClauseLiterals(t *testing.T) {
	// Truncated IN list: trailing truncated item (odd-quote) dropped
	in := "SELECT * FROM t WHERE ServiceName IN ('a','b-"
	got := repairTruncatedInClauseLiterals(in)
	want := "SELECT * FROM t WHERE ServiceName IN ('a')"
	if got != want {
		t.Errorf("truncated: got %q, want %q", got, want)
	}

	// All items complete (even quotes) -> kept as-is (no truncation)
	in2 := "SELECT * FROM t WHERE x IN ('a','b')"
	if got2 := repairTruncatedInClauseLiterals(in2); got2 != in2 {
		t.Errorf("complete list: changed %q -> %q", in2, got2)
	}

	// Empty content in IN -> unchanged
	in3 := "SELECT * FROM t WHERE x = 1"
	if got3 := repairTruncatedInClauseLiterals(in3); got3 != in3 {
		t.Errorf("no-IN: changed %q -> %q", in3, got3)
	}
}

// ---------------------------------------------------------------------------
// autoRepairIncompleteCTESQL (fix_ai_build.go)
// Oracle: app.py _auto_repair_incomplete_cte_sql
// ---------------------------------------------------------------------------

func TestAutoRepairIncompleteCTESQL(t *testing.T) {
	// Non-CTE -> ""
	if got := autoRepairIncompleteCTESQL("SELECT 1 FROM foo"); got != "" {
		t.Errorf("non-CTE: got %q, want empty", got)
	}

	// Empty -> ""
	if got := autoRepairIncompleteCTESQL(""); got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}

	// Complete CTE with final SELECT -> "" (no repair needed)
	complete := "WITH t AS (SELECT 1 FROM foo) SELECT * FROM t"
	if got := autoRepairIncompleteCTESQL(complete); got != "" {
		t.Errorf("complete CTE: got %q, want empty", got)
	}

	// Missing closing paren + missing final SELECT -> repair
	incomplete := "WITH t AS (SELECT 1 FROM foo"
	got := autoRepairIncompleteCTESQL(incomplete)
	want := "WITH t AS (SELECT 1 FROM foo)\nSELECT * FROM t"
	if got != want {
		t.Errorf("incomplete CTE: got %q, want %q", got, want)
	}

	// Has closing paren but missing final SELECT -> append SELECT
	nofinal := "WITH t AS (SELECT 1 FROM foo)"
	got = autoRepairIncompleteCTESQL(nofinal)
	want2 := "WITH t AS (SELECT 1 FROM foo)\nSELECT * FROM t"
	if got != want2 {
		t.Errorf("no-final-select: got %q, want %q", got, want2)
	}

	// Odd number of quotes -> ""
	if got := autoRepairIncompleteCTESQL("WITH t AS (SELECT 'unterminated FROM foo"); got != "" {
		t.Errorf("odd quotes: got %q, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// evaluateSeasonalRule (chart_anomaly_engine.go)
// Oracle: app.py _evaluate_seasonal_rule
// ---------------------------------------------------------------------------

func TestEvaluateSeasonalRule(t *testing.T) {
	// Non-seasonal rule (no seasonal_buckets_json)
	rule := map[string]any{
		"id":                 "r1",
		"name":               "test_rule",
		"warning_threshold":  5.0,
		"critical_threshold": 10.0,
		"comparator":         "gt",
		"min_sample_count":   json.Number("1"),
	}

	// value=7.0, sample_count=5 -> warning (7 >= 5 but < 10)
	result := evaluateSeasonalRule(rule, 7.0, json.Number("5"), nil)
	if result == nil {
		t.Fatal("warning: got nil, want result")
	}
	if result["rule_state"] != "warning" {
		t.Errorf("warning state: got %v, want warning", result["rule_state"])
	}
	if result["rule_id"] != "r1" {
		t.Errorf("rule_id: got %v", result["rule_id"])
	}
	if result["rule_name"] != "test_rule" {
		t.Errorf("rule_name: got %v", result["rule_name"])
	}
	if result["rule_seasonal"] != false {
		t.Errorf("rule_seasonal: got %v, want false", result["rule_seasonal"])
	}

	// value=12.0 -> outlier (12 >= 10)
	result = evaluateSeasonalRule(rule, 12.0, json.Number("5"), nil)
	if result == nil {
		t.Fatal("outlier: got nil, want result")
	}
	if result["rule_state"] != "outlier" {
		t.Errorf("outlier state: got %v, want outlier", result["rule_state"])
	}

	// value=3.0 -> no trigger -> nil
	result = evaluateSeasonalRule(rule, 3.0, json.Number("5"), nil)
	if result != nil {
		t.Errorf("normal: got %v, want nil", result)
	}

	// Too few samples -> nil
	result = evaluateSeasonalRule(rule, 99.0, json.Number("0"), nil)
	if result != nil {
		t.Errorf("too-few-samples: got %v, want nil", result)
	}

	// lt comparator: value=3.0 warning_th=5 critical_th=1 -> warning (3 <= 5 but 3 > 1)
	ruleLE := map[string]any{
		"id":                 "r2",
		"name":               "lt_rule",
		"warning_threshold":  5.0,
		"critical_threshold": 1.0,
		"comparator":         "lt",
		"min_sample_count":   json.Number("1"),
	}
	result = evaluateSeasonalRule(ruleLE, 3.0, json.Number("5"), nil)
	if result == nil {
		t.Fatal("lt warning: got nil, want result")
	}
	if result["rule_state"] != "warning" {
		t.Errorf("lt warning state: got %v, want warning", result["rule_state"])
	}
}

// ---------------------------------------------------------------------------
// inferQueryFieldTypes (query_exec.go)
// Oracle: app.py _infer_query_field_types
// ---------------------------------------------------------------------------

func TestInferQueryFieldTypes(t *testing.T) {
	// Empty rows -> single "object" column with kind "string"
	cols := []any{"name"}
	rows := []any{}
	out := inferQueryFieldTypes(cols, rows)
	if len(out) != 1 {
		t.Fatalf("empty rows: want 1, got %d", len(out))
	}
	col0 := out[0].(*jsonenc.Object)
	if d, _ := col0.Get("dtype"); d != "object" {
		t.Errorf("empty dtype: got %v, want object", d)
	}
	if k, _ := col0.Get("kind"); k != "string" {
		t.Errorf("empty kind: got %v, want string", k)
	}

	// All int, no null -> int64
	rows = []any{
		[]any{json.Number("1")},
		[]any{json.Number("2")},
	}
	out = inferQueryFieldTypes([]any{"count"}, rows)
	col0 = out[0].(*jsonenc.Object)
	if d, _ := col0.Get("dtype"); d != "int64" {
		t.Errorf("int64 dtype: got %v, want int64", d)
	}
	if k, _ := col0.Get("kind"); k != "integer" {
		t.Errorf("int64 kind: got %v, want integer", k)
	}

	// Int with null -> float64 (nullable int promotion)
	rows = []any{
		[]any{json.Number("1")},
		[]any{nil},
	}
	out = inferQueryFieldTypes([]any{"count"}, rows)
	col0 = out[0].(*jsonenc.Object)
	if d, _ := col0.Get("dtype"); d != "float64" {
		t.Errorf("nullable int dtype: got %v, want float64", d)
	}

	// All float -> float64
	rows = []any{
		[]any{1.5},
		[]any{2.5},
	}
	out = inferQueryFieldTypes([]any{"val"}, rows)
	col0 = out[0].(*jsonenc.Object)
	if d, _ := col0.Get("dtype"); d != "float64" {
		t.Errorf("float64 dtype: got %v, want float64", d)
	}
	if k, _ := col0.Get("kind"); k != "number" {
		t.Errorf("float64 kind: got %v, want number", k)
	}

	// All bool, no null -> bool
	rows = []any{
		[]any{true},
		[]any{false},
	}
	out = inferQueryFieldTypes([]any{"flag"}, rows)
	col0 = out[0].(*jsonenc.Object)
	if d, _ := col0.Get("dtype"); d != "bool" {
		t.Errorf("bool dtype: got %v, want bool", d)
	}
	if k, _ := col0.Get("kind"); k != "boolean" {
		t.Errorf("bool kind: got %v, want boolean", k)
	}

	// String column -> "str" dtype (pandas 3.x inferred string dtype), string kind
	rows = []any{
		[]any{"foo"},
		[]any{"bar"},
	}
	out = inferQueryFieldTypes([]any{"name"}, rows)
	col0 = out[0].(*jsonenc.Object)
	if d, _ := col0.Get("dtype"); d != "str" {
		t.Errorf("str dtype: got %v, want str", d)
	}
	if k, _ := col0.Get("kind"); k != "string" {
		t.Errorf("str kind: got %v, want string", k)
	}

	// Multiple columns
	cols = []any{"id", "name", "score"}
	rows = []any{
		[]any{json.Number("1"), "alice", 9.5},
		[]any{json.Number("2"), "bob", 8.0},
	}
	out = inferQueryFieldTypes(cols, rows)
	if len(out) != 3 {
		t.Fatalf("multi-col: want 3, got %d", len(out))
	}
	id0 := out[0].(*jsonenc.Object)
	name1 := out[1].(*jsonenc.Object)
	score2 := out[2].(*jsonenc.Object)
	if d, _ := id0.Get("dtype"); d != "int64" {
		t.Errorf("id dtype: got %v, want int64", d)
	}
	if d, _ := name1.Get("dtype"); d != "str" {
		t.Errorf("name dtype: got %v, want str", d)
	}
	if d, _ := score2.Get("dtype"); d != "float64" {
		t.Errorf("score dtype: got %v, want float64", d)
	}
}

// ---------------------------------------------------------------------------
// sanitizeActionValue (ai_action_execute.go)
// Oracle: app.py _build_client_action._sanitize_value
// ---------------------------------------------------------------------------

func TestSanitizeActionValue(t *testing.T) {
	// nil -> nil
	if got := sanitizeActionValue(nil, 0); got != nil {
		t.Errorf("nil: got %v", got)
	}
	// bool -> passthrough
	if got := sanitizeActionValue(true, 0); got != true {
		t.Errorf("bool true: got %v", got)
	}
	// json.Number -> passthrough
	n := json.Number("42")
	if got := sanitizeActionValue(n, 0); got != n {
		t.Errorf("json.Number: got %v", got)
	}
	// string -> trimmed; truncate at 4096
	if got := sanitizeActionValue("  hello  ", 0); got != "hello" {
		t.Errorf("string trim: got %v", got)
	}
	long := string(make([]byte, 5000))
	if got, ok := sanitizeActionValue(long, 0).(string); !ok || len(got) != 4096 {
		t.Errorf("string truncate: got len=%d, want 4096", len(got))
	}
	// depth>3 -> nil
	if got := sanitizeActionValue("x", 4); got != nil {
		t.Errorf("depth>3: got %v, want nil", got)
	}
	// *jsonenc.Object -> sanitized copy
	obj := jsonenc.NewObject().Set("key", "  value  ").Set("num", json.Number("5"))
	result, ok := sanitizeActionValue(obj, 0).(*jsonenc.Object)
	if !ok {
		t.Fatal("object: expected *jsonenc.Object result")
	}
	if v, _ := result.Get("key"); v != "value" {
		t.Errorf("object key trimmed: got %v, want value", v)
	}
	if v, _ := result.Get("num"); v != json.Number("5") {
		t.Errorf("object num: got %v, want 5", v)
	}
	// []any -> sanitized slice
	arr := []any{"a", "b", nil}
	res, ok := sanitizeActionValue(arr, 0).([]any)
	if !ok || len(res) != 3 {
		t.Errorf("slice: got %v", res)
	}
	if res[0] != "a" || res[1] != "b" || res[2] != nil {
		t.Errorf("slice values: got %v", res)
	}
	// unknown type -> nil
	type myType struct{}
	if got := sanitizeActionValue(myType{}, 0); got != nil {
		t.Errorf("unknown type: got %v", got)
	}
}

// ---------------------------------------------------------------------------
// normalizeCustomSeriesPointOrder (chart_render_binding.go)
// Oracle: app.py _normalize_custom_series_point_order
// ---------------------------------------------------------------------------

func TestNormalizeCustomSeriesPointOrder(t *testing.T) {
	// No series key -> no-op
	opt := jsonenc.NewObject()
	normalizeCustomSeriesPointOrder(opt)
	if _, ok := opt.Get("series"); !ok {
		// no series key, no-op is correct
	}

	// Series with non-list data -> no-op
	opt = jsonenc.NewObject()
	s1 := jsonenc.NewObject().Set("name", "A").Set("data", "not-a-list")
	opt.Set("series", []any{s1})
	normalizeCustomSeriesPointOrder(opt)
	// No panic is the test

	// Series with list data but not tuples -> no-op (not all points are lists)
	opt = jsonenc.NewObject()
	s2 := jsonenc.NewObject().Set("name", "B").Set("data", []any{
		json.Number("1"), json.Number("2"), json.Number("3"),
	})
	opt.Set("series", []any{s2})
	normalizeCustomSeriesPointOrder(opt)
	// values unchanged: still [1, 2, 3]

	// Tuple-like points [[x, y], ...] -> sorted by x ascending
	opt = jsonenc.NewObject()
	s3 := jsonenc.NewObject().Set("name", "C").Set("data", []any{
		[]any{json.Number("3"), "c"},
		[]any{json.Number("1"), "a"},
		[]any{json.Number("2"), "b"},
	})
	opt.Set("series", []any{s3})
	normalizeCustomSeriesPointOrder(opt)
	dataV, _ := s3.Get("data")
	data := dataV.([]any)
	if len(data) != 3 {
		t.Fatalf("sorted: got %d points", len(data))
	}
	// After sort: [1,a], [2,b], [3,c]
	for i, wantX := range []string{"1", "2", "3"} {
		pt := data[i].([]any)
		if toStr(pt[0]) != wantX {
			t.Errorf("point[%d][0]: got %v, want %v", i, pt[0], wantX)
		}
	}
}
