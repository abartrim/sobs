package main

import (
	"testing"

	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_ingest_tag_rules_test.go — batch 16 targeted coverage for
// cmd/sobs/ingest_tag_rules.go: rowAttrs' empty-map-falls-through branch (an empty but present
// LogAttributes must NOT be returned; the code should fall through to SpanAttributes or {}), and
// applyTagRules' end-to-end write path (log + trace record-id selection, last-rule-wins per tag
// key, and the empty-rules/empty-rows no-op guards).

func TestRowAttrsEmptyMapFallsThrough(t *testing.T) {
	// An empty (but present) LogAttributes must not short-circuit; Python's `row.get(...) or {}`
	// treats an empty dict as falsy, so the fallback to SpanAttributes should still happen.
	sa := map[string]any{"b": "2"}
	got := rowAttrs(map[string]any{"LogAttributes": map[string]any{}, "SpanAttributes": sa})
	if got["b"] != "2" {
		t.Fatalf("want fallback to SpanAttributes when LogAttributes is empty, got %v", got)
	}

	// Both present but empty -> {}.
	got2 := rowAttrs(map[string]any{"LogAttributes": map[string]any{}, "SpanAttributes": map[string]any{}})
	if len(got2) != 0 {
		t.Fatalf("want empty map when both attrs maps are empty, got %v", got2)
	}
}

func TestApplyTagRulesNoopGuards(t *testing.T) {
	fdb := &storetest.FakeDB{}
	s := &server{db: fdb}

	// No rules -> no-op.
	s.applyTagRules("log", []map[string]any{{"ServiceName": "svc"}}, nil)
	if len(fdb.Inserts) != 0 {
		t.Fatalf("want no inserts with nil rules, got %v", fdb.Inserts)
	}

	// No rows -> no-op.
	s.applyTagRules("log", nil, []any{rule([]any{"log"}, nil, "service_name", "eq", "svc", "")})
	if len(fdb.Inserts) != 0 {
		t.Fatalf("want no inserts with no rows, got %v", fdb.Inserts)
	}
}

func TestApplyTagRulesLogRecord(t *testing.T) {
	fdb := &storetest.FakeDB{}
	s := &server{db: fdb}
	rules := []any{
		rule([]any{"log"}, nil, "service_name", "eq", "api", ""),
	}
	// Need tag_key/tag_value on the rule for the insert row.
	rules[0].(map[string]any)["tag_key"] = "env"
	rules[0].(map[string]any)["tag_value"] = "prod"

	rows := []map[string]any{
		{"ServiceName": "api", "Timestamp": "2024-01-01T00:00:00Z", "TraceId": "t1", "SpanId": "s1"},
		{"ServiceName": "other", "Timestamp": "2024-01-01T00:00:01Z", "TraceId": "t2", "SpanId": "s2"},
	}
	s.applyTagRules("log", rows, rules)
	if len(fdb.Inserts) != 1 {
		t.Fatalf("want exactly one insert call, got %d", len(fdb.Inserts))
	}
	ins := fdb.Inserts[0]
	if ins.Table != "sobs_record_tags" {
		t.Fatalf("table = %q, want sobs_record_tags", ins.Table)
	}
	if len(ins.Rows) != 1 {
		t.Fatalf("want 1 matching tag row (only the api-service log matches), got %d: %+v", len(ins.Rows), ins.Rows)
	}
	row := ins.Rows[0]
	wantRecordID := recordIDForLog("2024-01-01T00:00:00Z", "api", "t1", "s1")
	if row["RecordId"] != wantRecordID {
		t.Errorf("RecordId = %v, want %v", row["RecordId"], wantRecordID)
	}
	if row["TagKey"] != "env" || row["TagValue"] != "prod" || row["IsAuto"] != 1 {
		t.Errorf("unexpected tag row: %+v", row)
	}
}

func TestApplyTagRulesTraceRecordUsesSpanRecordID(t *testing.T) {
	fdb := &storetest.FakeDB{}
	s := &server{db: fdb}
	r := rule([]any{"trace"}, nil, "service_name", "eq", "api", "")
	r["tag_key"] = "kind"
	r["tag_value"] = "http"
	rows := []map[string]any{{"ServiceName": "api", "TraceId": "trace-x", "SpanId": "span-y"}}
	s.applyTagRules("trace", rows, []any{r})
	if len(fdb.Inserts) != 1 || len(fdb.Inserts[0].Rows) != 1 {
		t.Fatalf("want one insert with one row, got %+v", fdb.Inserts)
	}
	wantID := recordIDForSpan("trace-x", "span-y")
	if got := fdb.Inserts[0].Rows[0]["RecordId"]; got != wantID {
		t.Errorf("RecordId = %v, want span-derived %v", got, wantID)
	}
}

func TestApplyTagRulesLastMatchingRuleWinsPerKey(t *testing.T) {
	fdb := &storetest.FakeDB{}
	s := &server{db: fdb}
	r1 := rule([]any{"log"}, nil, "service_name", "eq", "api", "")
	r1["tag_key"], r1["tag_value"] = "env", "first"
	r2 := rule([]any{"log"}, nil, "service_name", "eq", "api", "")
	r2["tag_key"], r2["tag_value"] = "env", "second" // same tag_key, later rule -> wins
	rows := []map[string]any{{"ServiceName": "api"}}
	s.applyTagRules("log", rows, []any{r1, r2})
	if len(fdb.Inserts) != 1 || len(fdb.Inserts[0].Rows) != 1 {
		t.Fatalf("want exactly one merged tag row, got %+v", fdb.Inserts)
	}
	if got := fdb.Inserts[0].Rows[0]["TagValue"]; got != "second" {
		t.Errorf("TagValue = %v, want last-matching-rule value 'second'", got)
	}
}

func TestApplyTagRulesSkipsNonMapRuleEntries(t *testing.T) {
	fdb := &storetest.FakeDB{}
	s := &server{db: fdb}
	// A non-map entry in the rules slice must be skipped, not panic.
	s.applyTagRules("log", []map[string]any{{"ServiceName": "api"}}, []any{"not-a-rule-map"})
	if len(fdb.Inserts) != 0 {
		t.Fatalf("want no inserts, got %v", fdb.Inserts)
	}
}
