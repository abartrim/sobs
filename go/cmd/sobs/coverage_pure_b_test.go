package main

// Oracle-anchored unit tests for slice-B target functions.
//
// Skipped (not purely unit-testable or already well-covered):
//   _shutdown_db_resources     → not pure (global state / threading)
//   _write_worker_main         → not pure (blocking queue loop)
//   _generate (SSE)            → async generator, not unit-testable
//   _dispatch_browser_push_channel → already covered by TestDispatchChannelConfigErrors
//   _verify_rum_client_auth    → already covered by TestRumClientVerify*
//   _maybe_demangle_js_stack   → already covered by TestDemangleStack*
//   _geo_lookup_batch          → already covered by TestGeoLookupBatch*
//   _decrypt_secret_value      → already covered by TestSettingsEncryptionGating
//   _to_utc_iso                → already covered by TestWorkItemToUTCISO
//   _read_file_or_env          → already covered by TestReadFileOrEnv
//   _chat_label_from_first_turn → already covered by TestChatLabelFromFirstTurn
//   _action_meta_for_id        → already covered by TestActionMetaForPageAndID
//   _error_group_key           → no Go port (Python dead code, never called)
//   _mask_channel_config       → no Go port (Python dead code, never called)
//   _merge_root_path           → no Go port (Go handles base-path in config, not as middleware)
//   _normalized_values (local) → Go port is k8sNormalizedValues which takes *http.Request

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---------------------------------------------------------------------------
// parseTagRuleConditions — mirrors _parse_tag_rule_conditions_json
// ---------------------------------------------------------------------------

func TestParseTagRuleConditions(t *testing.T) {
	// Oracle: _parse_tag_rule_conditions_json

	t.Run("empty string", func(t *testing.T) {
		if got := parseTagRuleConditions(""); len(got) != 0 {
			t.Errorf("empty string: want [], got %v", got)
		}
	})

	t.Run("whitespace only", func(t *testing.T) {
		if got := parseTagRuleConditions("   "); len(got) != 0 {
			t.Errorf("whitespace only: want [], got %v", got)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		if got := parseTagRuleConditions("not json"); len(got) != 0 {
			t.Errorf("invalid json: want [], got %v", got)
		}
	})

	t.Run("JSON object (not list)", func(t *testing.T) {
		if got := parseTagRuleConditions(`{}`); len(got) != 0 {
			t.Errorf("object input: want [], got %v", got)
		}
	})

	t.Run("single condition", func(t *testing.T) {
		raw := `[{"match_field":"service","match_operator":"eq","match_value":"web","match_attr_key":""}]`
		got := parseTagRuleConditions(raw)
		if len(got) != 1 {
			t.Fatalf("single condition: want len 1, got %d: %v", len(got), got)
		}
		m, ok := got[0].(map[string]any)
		if !ok {
			t.Fatalf("item is not map[string]any: %T", got[0])
		}
		if m["match_field"] != "service" {
			t.Errorf("match_field: got %q, want service", m["match_field"])
		}
		if m["match_operator"] != "eq" {
			t.Errorf("match_operator: got %q, want eq", m["match_operator"])
		}
		if m["match_value"] != "web" {
			t.Errorf("match_value: got %q, want web", m["match_value"])
		}
		if m["match_attr_key"] != "" {
			t.Errorf("match_attr_key: got %q, want empty", m["match_attr_key"])
		}
	})

	t.Run("non-dict items skipped", func(t *testing.T) {
		// Oracle: non-dict entries (like "not_a_dict") are skipped.
		raw := `[{"match_field":"attr","match_operator":"contains","match_value":"error","match_attr_key":"k1"},"not_a_dict"]`
		got := parseTagRuleConditions(raw)
		if len(got) != 1 {
			t.Errorf("mixed list: want len 1, got %d", len(got))
		}
	})

	t.Run("null field values coerced to empty string", func(t *testing.T) {
		// Oracle: str(item.get("match_field","") or "") -> null coerces to ""
		raw := `[{"match_field":null,"match_operator":null,"match_value":null,"match_attr_key":null}]`
		got := parseTagRuleConditions(raw)
		if len(got) != 1 {
			t.Fatalf("null fields: want len 1, got %d", len(got))
		}
		m := got[0].(map[string]any)
		for _, k := range []string{"match_field", "match_operator", "match_value", "match_attr_key"} {
			if m[k] != "" {
				t.Errorf("null %s should coerce to empty string, got %q", k, m[k])
			}
		}
	})

	t.Run("multiple conditions", func(t *testing.T) {
		raw := `[{"match_field":"a","match_operator":"eq","match_value":"1","match_attr_key":""},{"match_field":"b","match_operator":"regex","match_value":"x+","match_attr_key":"my_attr"}]`
		got := parseTagRuleConditions(raw)
		if len(got) != 2 {
			t.Fatalf("two conditions: want 2, got %d", len(got))
		}
		m0 := got[0].(map[string]any)
		m1 := got[1].(map[string]any)
		if m0["match_field"] != "a" {
			t.Errorf("cond0 match_field: got %q", m0["match_field"])
		}
		if m1["match_attr_key"] != "my_attr" {
			t.Errorf("cond1 match_attr_key: got %q", m1["match_attr_key"])
		}
	})
}

// ---------------------------------------------------------------------------
// extractMemoryCandidates — mirrors _extract_memory_candidates
// ---------------------------------------------------------------------------

func objWith(kv ...any) *jsonenc.Object {
	o := jsonenc.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func TestExtractMemoryCandidates(t *testing.T) {
	// Oracle: _extract_memory_candidates

	t.Run("nil object", func(t *testing.T) {
		if got := extractMemoryCandidates(nil); got != nil {
			t.Errorf("nil: want nil, got %v", got)
		}
	})

	t.Run("missing key", func(t *testing.T) {
		meta := jsonenc.NewObject().Set("other", "value")
		if got := extractMemoryCandidates(meta); got != nil {
			t.Errorf("missing key: want nil, got %v", got)
		}
	})

	t.Run("nil value", func(t *testing.T) {
		meta := jsonenc.NewObject().Set("memory_candidates", nil)
		// raw is nil, not a list or string
		if got := extractMemoryCandidates(meta); len(got) != 0 {
			t.Errorf("nil value: want [], got %v", got)
		}
	})

	t.Run("single string value", func(t *testing.T) {
		meta := jsonenc.NewObject().Set("memory_candidates", "remember this fact")
		got := extractMemoryCandidates(meta)
		if len(got) != 1 || got[0] != "remember this fact" {
			t.Errorf("single string: got %v", got)
		}
	})

	t.Run("list of strings", func(t *testing.T) {
		meta := jsonenc.NewObject().Set("memory_candidates", []any{"item1", "item2", "item3"})
		got := extractMemoryCandidates(meta)
		if len(got) != 3 {
			t.Fatalf("three items: want 3, got %d: %v", len(got), got)
		}
		if got[0] != "item1" || got[1] != "item2" || got[2] != "item3" {
			t.Errorf("list items: %v", got)
		}
	})

	t.Run("list capped at 3", func(t *testing.T) {
		// Oracle: deduped capped at 3 items.
		meta := jsonenc.NewObject().Set("memory_candidates", []any{"a", "b", "c", "d", "e"})
		got := extractMemoryCandidates(meta)
		if len(got) != 3 {
			t.Errorf("cap at 3: got %d items %v", len(got), got)
		}
		if got[0] != "a" || got[1] != "b" || got[2] != "c" {
			t.Errorf("cap at 3: wrong items %v", got)
		}
	})

	t.Run("case-insensitive dedup", func(t *testing.T) {
		// Oracle: deduplication is case-insensitive ("ITEM1" deduped against "item1").
		meta := jsonenc.NewObject().Set("memory_candidates", []any{"item1", "ITEM1", "item2"})
		got := extractMemoryCandidates(meta)
		if len(got) != 2 {
			t.Errorf("case dedup: want 2, got %d: %v", len(got), got)
		}
		if got[0] != "item1" || got[1] != "item2" {
			t.Errorf("case dedup: wrong items %v", got)
		}
	})

	t.Run("empty strings filtered", func(t *testing.T) {
		meta := jsonenc.NewObject().Set("memory_candidates", []any{"item1", "", "item2"})
		got := extractMemoryCandidates(meta)
		if len(got) != 2 {
			t.Errorf("empty filtered: want 2, got %d: %v", len(got), got)
		}
	})

	t.Run("truncated at 280 chars", func(t *testing.T) {
		// Oracle: _coerce_summary_value(item, 280) truncates at 280 characters.
		long := strings.Repeat("a", 300)
		meta := jsonenc.NewObject().Set("memory_candidates", []any{long})
		got := extractMemoryCandidates(meta)
		if len(got) != 1 || len(got[0]) != 280 {
			t.Errorf("truncate: want len 280, got %d", len(got[0]))
		}
	})
}

// ---------------------------------------------------------------------------
// genaiToolCallsToText — mirrors _genai_tool_calls_to_text
// ---------------------------------------------------------------------------

func TestGenaiToolCallsToText(t *testing.T) {
	// Oracle: _genai_tool_calls_to_text

	t.Run("not a list returns empty", func(t *testing.T) {
		if got := genaiToolCallsToText(nil); got != "" {
			t.Errorf("nil: got %q", got)
		}
		if got := genaiToolCallsToText("not a list"); got != "" {
			t.Errorf("string: got %q", got)
		}
		if got := genaiToolCallsToText(42); got != "" {
			t.Errorf("int: got %q", got)
		}
	})

	t.Run("empty list returns empty", func(t *testing.T) {
		if got := genaiToolCallsToText([]any{}); got != "" {
			t.Errorf("empty list: got %q", got)
		}
	})

	t.Run("non-dict items skipped", func(t *testing.T) {
		// Oracle: non-dict items in list are skipped (continue).
		if got := genaiToolCallsToText([]any{"str", 42, nil}); got != "" {
			t.Errorf("all non-dict: got %q", got)
		}
	})

	t.Run("named tool with dict arguments", func(t *testing.T) {
		// Oracle: tool_call:search {"query": "hello"}
		item := map[string]any{"name": "search", "arguments": map[string]any{"query": "hello"}}
		got := genaiToolCallsToText([]any{item})
		if !strings.HasPrefix(got, "tool_call:search {") {
			t.Errorf("dict args: got %q", got)
		}
		if !strings.Contains(got, `"query"`) {
			t.Errorf("dict args missing key: %q", got)
		}
	})

	t.Run("no name produces tool_call label", func(t *testing.T) {
		// Oracle: empty/nil name -> label "tool_call"
		item := map[string]any{"name": "", "arguments": nil}
		got := genaiToolCallsToText([]any{item})
		if got != "tool_call" {
			t.Errorf("no name: got %q, want tool_call", got)
		}
	})

	t.Run("function fallback name", func(t *testing.T) {
		// Oracle: item["function"]["name"] is used when item["name"] is absent.
		item := map[string]any{"function": map[string]any{"name": "fn", "arguments": []any{1, 2}}}
		got := genaiToolCallsToText([]any{item})
		if !strings.HasPrefix(got, "tool_call:fn ") {
			t.Errorf("function name: got %q", got)
		}
	})

	t.Run("empty dict arguments", func(t *testing.T) {
		// Oracle: empty dict {} is falsy in Python -> check function.arguments ->
		// function.arguments also absent -> label only.
		item := map[string]any{"name": "e", "arguments": map[string]any{}}
		got := genaiToolCallsToText([]any{item})
		if got != "tool_call:e" {
			t.Errorf("empty dict: got %q, want tool_call:e", got)
		}
	})

	t.Run("empty list arguments", func(t *testing.T) {
		// Oracle: empty list [] is also falsy -> check function.arguments -> nil -> label only.
		item := map[string]any{"name": "e", "arguments": []any{}}
		got := genaiToolCallsToText([]any{item})
		if got != "tool_call:e" {
			t.Errorf("empty list: got %q, want tool_call:e", got)
		}
	})

	t.Run("raw string arguments", func(t *testing.T) {
		// Oracle: tool_call:tool2 raw text
		item := map[string]any{"name": "tool2", "arguments": "raw text"}
		got := genaiToolCallsToText([]any{item})
		if got != "tool_call:tool2 raw text" {
			t.Errorf("raw string args: got %q", got)
		}
	})

	t.Run("multiple tools joined with newline", func(t *testing.T) {
		// Oracle: joined with \n, stripped.
		items := []any{
			map[string]any{"name": "tool", "arguments": ""},
			map[string]any{"name": "tool2", "arguments": "raw text"},
		}
		got := genaiToolCallsToText(items)
		if got != "tool_call:tool\ntool_call:tool2 raw text" {
			t.Errorf("two tools: got %q", got)
		}
	})

	t.Run("trimmed name", func(t *testing.T) {
		// Oracle: name is stripped: "  spaced  " -> "spaced"
		item := map[string]any{"name": "  spaced  ", "arguments": nil}
		got := genaiToolCallsToText([]any{item})
		if got != "tool_call:spaced" {
			t.Errorf("trimmed name: got %q", got)
		}
	})

	t.Run("unicode arguments not escaped", func(t *testing.T) {
		// Oracle: json.dumps(arguments, ensure_ascii=False) -> unicode preserved
		item := map[string]any{"name": "x", "arguments": map[string]any{"k": "日本語"}}
		got := genaiToolCallsToText([]any{item})
		if !strings.Contains(got, "日本語") {
			t.Errorf("unicode not preserved: %q", got)
		}
	})

	t.Run("empty string argument is label only", func(t *testing.T) {
		// Oracle: arguments="" is in (None,"") -> label only
		item := map[string]any{"name": "tool", "arguments": ""}
		got := genaiToolCallsToText([]any{item})
		if got != "tool_call:tool" {
			t.Errorf("empty string arg: got %q, want tool_call:tool", got)
		}
	})
}

// ---------------------------------------------------------------------------
// parseTimeWindowArgsStrings — mirrors _parse_time_window_args
// ---------------------------------------------------------------------------

func TestParseTimeWindowArgsStrings(t *testing.T) {
	// Oracle: _parse_time_window_args (rewritten as a pure-string fn in Go)

	t.Run("all empty", func(t *testing.T) {
		from, to, err := parseTimeWindowArgsStrings("", "", "")
		if from != "" || to != "" || err != "" {
			t.Errorf("all empty: got (%q,%q,%q)", from, to, err)
		}
	})

	t.Run("from only", func(t *testing.T) {
		// Oracle: from_ts normalized, to_ts and error empty.
		from, to, errMsg := parseTimeWindowArgsStrings("2026-03-29T12:00:00Z", "", "")
		if from != "2026-03-29 12:00:00.000000" {
			t.Errorf("from only: from=%q", from)
		}
		if to != "" || errMsg != "" {
			t.Errorf("from only: to=%q err=%q", to, errMsg)
		}
	})

	t.Run("from and to", func(t *testing.T) {
		from, to, errMsg := parseTimeWindowArgsStrings("2026-03-29T12:00:00Z", "2026-03-29T13:00:00Z", "")
		if from != "2026-03-29 12:00:00.000000" {
			t.Errorf("from: got %q", from)
		}
		if to != "2026-03-29 13:00:00.000000" {
			t.Errorf("to: got %q", to)
		}
		if errMsg != "" {
			t.Errorf("err: got %q", errMsg)
		}
	})

	t.Run("from + window_s derives to", func(t *testing.T) {
		// Oracle: from_ts + window_s=3600 -> to_ts = from + 1 hour
		from, to, errMsg := parseTimeWindowArgsStrings("2026-03-29T12:00:00Z", "", "3600")
		if from != "2026-03-29 12:00:00.000000" {
			t.Errorf("window from: got %q", from)
		}
		if to != "2026-03-29 13:00:00.000000" {
			t.Errorf("window to: got %q", to)
		}
		if errMsg != "" {
			t.Errorf("window err: got %q", errMsg)
		}
	})

	t.Run("to <= from is an error", func(t *testing.T) {
		// Oracle: to_dt <= from_dt -> error message.
		_, _, errMsg := parseTimeWindowArgsStrings("2026-03-29T12:00:00Z", "2026-03-29T11:00:00Z", "")
		if errMsg != "Invalid time window: to_ts must be later than from_ts" {
			t.Errorf("invalid window: got %q", errMsg)
		}
	})

	t.Run("window_s clamped to 1", func(t *testing.T) {
		// Oracle: max(1, int(window_s_raw)) -> 0 becomes 1
		from, to, errMsg := parseTimeWindowArgsStrings("2026-03-29T12:00:00Z", "", "0")
		if errMsg != "" {
			t.Fatalf("window 0: unexpected error %q", errMsg)
		}
		// window_s 0 gets clamped to 1 second
		if to == from {
			t.Errorf("window 0 should produce different to_ts")
		}
	})

	t.Run("invalid window_s string", func(t *testing.T) {
		// Oracle: int("abc") raises ValueError -> error message
		_, _, errMsg := parseTimeWindowArgsStrings("2026-03-29T12:00:00Z", "", "abc")
		if !strings.Contains(errMsg, "Invalid time value") {
			t.Errorf("non-int window_s: got %q", errMsg)
		}
	})

	t.Run("window_s ignored when to is already set", func(t *testing.T) {
		// Oracle: if from_ts and not to_ts and window_s_raw -> only applies when to is empty.
		from, to, errMsg := parseTimeWindowArgsStrings("2026-03-29T12:00:00Z", "2026-03-29T13:00:00Z", "99")
		if errMsg != "" {
			t.Errorf("to set: unexpected error %q", errMsg)
		}
		// to is already set, window_s ignored; to should remain the explicitly given value
		if to != "2026-03-29 13:00:00.000000" {
			t.Errorf("explicit to unchanged: got %q", to)
		}
		_ = from
	})
}

// ---------------------------------------------------------------------------
// resolveTemplateRoleIndices — mirrors _resolve_template_role_indices
// ---------------------------------------------------------------------------

func TestResolveTemplateRoleIndices(t *testing.T) {
	// Oracle: _resolve_template_role_indices

	t.Run("no spec returns template roles", func(t *testing.T) {
		meta := chartTemplateColMeta{roles: map[string]int{"x": 0, "y": 1}}
		got, errMsg := resolveTemplateRoleIndices("t1", meta, []any{"a", "b", "c"}, nil)
		if errMsg != "" {
			t.Fatalf("no spec: err %q", errMsg)
		}
		if got["x"] != 0 || got["y"] != 1 {
			t.Errorf("no spec: got %v", got)
		}
	})

	t.Run("no column_roles returns empty", func(t *testing.T) {
		meta := chartTemplateColMeta{roles: map[string]int{}}
		got, errMsg := resolveTemplateRoleIndices("t1", meta, []any{"a", "b"}, nil)
		if errMsg != "" {
			t.Fatalf("empty roles: err %q", errMsg)
		}
		if len(got) != 0 {
			t.Errorf("empty roles: got %v", got)
		}
	})

	t.Run("spec with exact column name match", func(t *testing.T) {
		// Oracle: col_name in col_index_by_name -> override role index
		meta := chartTemplateColMeta{roles: map[string]int{"x": 0, "y": 1}}
		spec := jsonenc.NewObject().Set("visual", jsonenc.NewObject().Set("role_map", jsonenc.NewObject().Set("x", "b")))
		got, errMsg := resolveTemplateRoleIndices("t1", meta, []any{"a", "b", "c"}, spec)
		if errMsg != "" {
			t.Fatalf("exact match: err %q", errMsg)
		}
		// x should now point to index 1 (column "b"), y unchanged at 1
		if got["x"] != 1 {
			t.Errorf("exact match: x=%d, want 1", got["x"])
		}
	})

	t.Run("spec with case-insensitive column match", func(t *testing.T) {
		// Oracle: lowered col_name in lower_name_to_index -> override role index
		meta := chartTemplateColMeta{roles: map[string]int{"x": 0, "y": 1}}
		spec := jsonenc.NewObject().Set("visual", jsonenc.NewObject().Set("role_map", jsonenc.NewObject().Set("x", "A")))
		// columns are lower case, role_map uses upper "A" -> case-insensitive
		got, errMsg := resolveTemplateRoleIndices("t1", meta, []any{"a", "b", "c"}, spec)
		if errMsg != "" {
			t.Fatalf("case-insensitive: err %q", errMsg)
		}
		if got["x"] != 0 {
			t.Errorf("case-insensitive: x=%d, want 0", got["x"])
		}
	})

	t.Run("unknown role in role_map returns error", func(t *testing.T) {
		// Oracle: role_name not in role_indices -> raise ValueError
		meta := chartTemplateColMeta{roles: map[string]int{"x": 0}}
		spec := jsonenc.NewObject().Set("visual", jsonenc.NewObject().Set("role_map", jsonenc.NewObject().Set("z", "a")))
		_, errMsg := resolveTemplateRoleIndices("t1", meta, []any{"a"}, spec)
		if !strings.Contains(errMsg, "Unknown role") || !strings.Contains(errMsg, "z") {
			t.Errorf("unknown role: err %q", errMsg)
		}
	})

	t.Run("unknown column in role_map returns error", func(t *testing.T) {
		// Oracle: col_name not found -> raise ValueError
		meta := chartTemplateColMeta{roles: map[string]int{"x": 0}}
		spec := jsonenc.NewObject().Set("visual", jsonenc.NewObject().Set("role_map", jsonenc.NewObject().Set("x", "NOTFOUND")))
		_, errMsg := resolveTemplateRoleIndices("t1", meta, []any{"a"}, spec)
		if !strings.Contains(errMsg, "unknown column") || !strings.Contains(errMsg, "NOTFOUND") {
			t.Errorf("unknown column: err %q", errMsg)
		}
	})

	t.Run("empty role or col names are skipped", func(t *testing.T) {
		// Oracle: not role_name or not col_name -> continue
		meta := chartTemplateColMeta{roles: map[string]int{"x": 0}}
		spec := jsonenc.NewObject().Set("visual", jsonenc.NewObject().Set("role_map",
			jsonenc.NewObject().Set("", "a").Set("x", "")))
		got, errMsg := resolveTemplateRoleIndices("t1", meta, []any{"a"}, spec)
		if errMsg != "" {
			t.Fatalf("skip empty: err %q", errMsg)
		}
		// x unchanged at 0 because col name "" is skipped
		if got["x"] != 0 {
			t.Errorf("skip empty: x=%d, want 0", got["x"])
		}
	})

	t.Run("spec missing visual key returns template roles", func(t *testing.T) {
		meta := chartTemplateColMeta{roles: map[string]int{"x": 0, "y": 2}}
		spec := jsonenc.NewObject().Set("other_key", "val")
		got, errMsg := resolveTemplateRoleIndices("t1", meta, []any{"a", "b", "c"}, spec)
		if errMsg != "" {
			t.Fatalf("missing visual: err %q", errMsg)
		}
		if got["x"] != 0 || got["y"] != 2 {
			t.Errorf("missing visual: got %v", got)
		}
	})

	t.Run("spec visual missing role_map returns template roles", func(t *testing.T) {
		meta := chartTemplateColMeta{roles: map[string]int{"x": 0}}
		spec := jsonenc.NewObject().Set("visual", jsonenc.NewObject().Set("other", "v"))
		got, errMsg := resolveTemplateRoleIndices("t1", meta, []any{"a"}, spec)
		if errMsg != "" {
			t.Fatalf("missing role_map: err %q", errMsg)
		}
		if got["x"] != 0 {
			t.Errorf("missing role_map: x=%d, want 0", got["x"])
		}
	})
}

// ---------------------------------------------------------------------------
// normalizeNotificationCondition — tag type branch + range clamping
// (adds coverage beyond the minimal TestNormalizeNotificationCondition)
// ---------------------------------------------------------------------------

func TestNormalizeNotificationConditionTagBranch(t *testing.T) {
	// Oracle: _normalize_notification_condition — tag branch

	t.Run("tag type with valid fields", func(t *testing.T) {
		o := jsonenc.NewObject().
			Set("type", "tag").
			Set("record_type", "log").
			Set("tag_key", "env").
			Set("tag_match_operator", "eq").
			Set("tag_value", "prod").
			Set("comparator", "gt").
			Set("threshold", 1.0).
			Set("window_minutes", 5)
		got := normalizeNotificationCondition(o)
		if got == nil {
			t.Fatal("tag type: want non-nil")
		}
		if got["type"] != "tag" {
			t.Errorf("type: got %v, want tag", got["type"])
		}
		if got["record_type"] != "log" {
			t.Errorf("record_type: got %v, want log", got["record_type"])
		}
		if got["tag_key"] != "env" {
			t.Errorf("tag_key: got %v, want env", got["tag_key"])
		}
		if got["tag_value"] != "prod" {
			t.Errorf("tag_value: got %v, want prod", got["tag_value"])
		}
	})

	t.Run("invalid record_type falls back to all", func(t *testing.T) {
		// Oracle: record_type not in _NOTIFICATION_TAG_RECORD_TYPES -> "all"
		o := jsonenc.NewObject().Set("type", "tag").Set("record_type", "INVALID")
		got := normalizeNotificationCondition(o)
		if got["record_type"] != "all" {
			t.Errorf("invalid record_type: got %v, want all", got["record_type"])
		}
	})

	t.Run("invalid tag_match_operator falls back to eq", func(t *testing.T) {
		// Oracle: tag_match_operator not in {eq,contains,regex} -> "eq"
		o := jsonenc.NewObject().Set("type", "tag").Set("tag_match_operator", "INVALID")
		got := normalizeNotificationCondition(o)
		if got["tag_match_operator"] != "eq" {
			t.Errorf("invalid operator: got %v, want eq", got["tag_match_operator"])
		}
	})

	t.Run("invalid comparator falls back to gt", func(t *testing.T) {
		// Oracle: comparator not in {gt,lt,gte,lte,eq} -> "gt"
		o := jsonenc.NewObject().Set("type", "tag").Set("comparator", "INVALID")
		got := normalizeNotificationCondition(o)
		if got["comparator"] != "gt" {
			t.Errorf("invalid comparator: got %v, want gt", got["comparator"])
		}
	})

	t.Run("window_minutes zero is falsy so defaults to 5", func(t *testing.T) {
		// Oracle: max(1, min(60, int(raw.get("window_minutes") or 5)))
		// 0 is falsy in Python -> "0 or 5" -> 5 -> clamped -> 5
		o := jsonenc.NewObject().Set("type", "tag").Set("window_minutes", 0)
		got := normalizeNotificationCondition(o)
		if got["window_minutes"] != 5 {
			t.Errorf("zero window_minutes: got %v, want 5 (falsy -> default 5)", got["window_minutes"])
		}
	})

	t.Run("window_minutes clamped to 60", func(t *testing.T) {
		// Oracle: max(1, min(60, int(...))) -> 100 => 60
		o := jsonenc.NewObject().Set("type", "tag").Set("window_minutes", 100)
		got := normalizeNotificationCondition(o)
		if got["window_minutes"] != 60 {
			t.Errorf("clamp to 60: got %v", got["window_minutes"])
		}
	})

	t.Run("signal type with full fields", func(t *testing.T) {
		// Oracle: default condition_type is signal
		o := jsonenc.NewObject().
			Set("type", "signal").
			Set("source", "web").
			Set("signal", "error_rate").
			Set("service", "api").
			Set("comparator", "gt").
			Set("threshold", 5.0).
			Set("window_minutes", 10)
		got := normalizeNotificationCondition(o)
		if got == nil {
			t.Fatal("signal type: nil")
		}
		if got["type"] != "signal" {
			t.Errorf("type: got %v", got["type"])
		}
		if got["source"] != "web" {
			t.Errorf("source: got %v", got["source"])
		}
		if got["signal"] != "error_rate" {
			t.Errorf("signal: got %v", got["signal"])
		}
	})

	t.Run("empty object produces signal defaults", func(t *testing.T) {
		// Oracle: {} -> signal type with all defaults
		o := jsonenc.NewObject()
		got := normalizeNotificationCondition(o)
		if got == nil {
			t.Fatal("empty obj: nil")
		}
		if got["type"] != "signal" {
			t.Errorf("empty type: got %v, want signal", got["type"])
		}
		if got["comparator"] != "gt" {
			t.Errorf("empty comparator: got %v, want gt", got["comparator"])
		}
		if got["window_minutes"] != 5 {
			t.Errorf("empty window_minutes: got %v, want 5", got["window_minutes"])
		}
	})

	t.Run("non-numeric threshold falls back to 0", func(t *testing.T) {
		// Oracle: float("not_a_number") raises ValueError -> 0.0
		o := jsonenc.NewObject().Set("threshold", "not_a_number")
		got := normalizeNotificationCondition(o)
		if got["threshold"] != 0.0 {
			t.Errorf("bad threshold: got %v, want 0.0", got["threshold"])
		}
	})
}
