package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Batch 7: cmd/sobs/ai_view.go — parseLimitStr/clampLimit/parseOffsetStr/parseSortStr (no existing
// tests reference these at all), attrMap/safeAttrInt/safeDurationMS, truncRunes/firstNonEmptyAny/
// isEmptyArg's remaining branches, and getAiFilterMetadata (the one DB-backed function in this
// file, exercised via storetest.FakeDB). coverage_pure_{a,b,c,d}_test.go already cover
// parseTimeWindowArgsStrings, buildAiTraceTurnCards, genaiMessageContentToText, and
// normalizeGenaiMessagesForDisplay in depth, so this file does not duplicate those.

// ---- parseLimitStr / clampLimit ----

func TestParseLimitStr(t *testing.T) {
	if got := parseLimitStr("", 25); got != 25 {
		t.Errorf("blank -> default: got %d, want 25", got)
	}
	if got := parseLimitStr("  ", 25); got != 25 {
		t.Errorf("whitespace -> default: got %d, want 25", got)
	}
	if got := parseLimitStr("not-a-number", 25); got != 25 {
		t.Errorf("unparseable -> literal default (unclamped): got %d, want 25", got)
	}
	if got := parseLimitStr("10", 25); got != 10 {
		t.Errorf("valid value: got %d, want 10", got)
	}
	if got := parseLimitStr("100000", 25); got != 5000 {
		t.Errorf("over max clamps to 5000: got %d", got)
	}
	if got := parseLimitStr("-5", 25); got != 1 {
		t.Errorf("negative clamps to 1: got %d", got)
	}
	// Default itself is clamped when raw=="" and the given default is out of range.
	if got := parseLimitStr("", 999999); got != 5000 {
		t.Errorf("blank with an out-of-range default should still clamp: got %d", got)
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct {
		in, want int
	}{{0, 1}, {-100, 1}, {1, 1}, {5000, 5000}, {5001, 5000}, {2500, 2500}}
	for _, c := range cases {
		if got := clampLimit(c.in); got != c.want {
			t.Errorf("clampLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// ---- parseOffsetStr ----

func TestParseOffsetStr(t *testing.T) {
	if got := parseOffsetStr(""); got != 0 {
		t.Errorf("blank: got %d, want 0", got)
	}
	if got := parseOffsetStr("  "); got != 0 {
		t.Errorf("whitespace: got %d, want 0", got)
	}
	if got := parseOffsetStr("abc"); got != 0 {
		t.Errorf("unparseable: got %d, want 0", got)
	}
	if got := parseOffsetStr("-10"); got != 0 {
		t.Errorf("negative clamps to 0: got %d", got)
	}
	if got := parseOffsetStr("42"); got != 42 {
		t.Errorf("valid value: got %d, want 42", got)
	}
}

// ---- parseSortStr ----

func TestParseSortStr(t *testing.T) {
	allowed := map[string]string{"ts": "Timestamp", "service": "ServiceName"}

	sortBy, sqlCol, dir := parseSortStr("", "", allowed, "ts")
	if sortBy != "ts" || sqlCol != "Timestamp" || dir != "desc" {
		t.Errorf("defaults: got (%q,%q,%q)", sortBy, sqlCol, dir)
	}

	sortBy, sqlCol, dir = parseSortStr("service", "asc", allowed, "ts")
	if sortBy != "service" || sqlCol != "ServiceName" || dir != "asc" {
		t.Errorf("explicit valid: got (%q,%q,%q)", sortBy, sqlCol, dir)
	}

	// Unknown sort_by falls back to the default column (and its mapped SQL).
	sortBy, sqlCol, dir = parseSortStr("bogus_col", "asc", allowed, "ts")
	if sortBy != "ts" || sqlCol != "Timestamp" {
		t.Errorf("unknown sort_by should fall back to default: got (%q,%q)", sortBy, sqlCol)
	}

	// Invalid sort_dir (neither asc nor desc) normalizes to desc.
	_, _, dir = parseSortStr("ts", "sideways", allowed, "ts")
	if dir != "desc" {
		t.Errorf("invalid sort_dir should normalize to desc, got %q", dir)
	}

	// Case-insensitive sort_dir.
	_, _, dir = parseSortStr("ts", "ASC", allowed, "ts")
	if dir != "asc" {
		t.Errorf("sort_dir should lowercase: got %q", dir)
	}
}

// ---- attrMap / safeAttrInt / safeDurationMS ----

func TestAttrMap(t *testing.T) {
	if got := attrMap(map[string]any{"a": 1}); len(got) != 1 {
		t.Errorf("map[string]any passthrough: got %v", got)
	}
	if got := attrMap(`{"a":1}`); len(got) != 1 {
		t.Errorf("JSON string parse: got %v", got)
	}
	if got := attrMap(`not json`); len(got) != 0 {
		t.Errorf("bad JSON -> empty map: got %v", got)
	}
	if got := attrMap(42); len(got) != 0 {
		t.Errorf("unsupported kind -> empty map: got %v", got)
	}
}

func TestSafeAttrInt(t *testing.T) {
	if got := safeAttrInt(map[string]any{"tokens_in": "42"}, "tokens_in"); got != 42 {
		t.Errorf("string numeral: got %d, want 42", got)
	}
	if got := safeAttrInt(map[string]any{}, "missing"); got != 0 {
		t.Errorf("missing key defaults to 0: got %d", got)
	}
	if got := safeAttrInt(map[string]any{"x": "  "}, "x"); got != 0 {
		t.Errorf("blank value defaults to 0: got %d", got)
	}
	if got := safeAttrInt(map[string]any{"x": "not-a-number"}, "x"); got != 0 {
		t.Errorf("unparseable -> 0: got %d", got)
	}
	if got := safeAttrInt(map[string]any{"x": "NaN"}, "x"); got != 0 {
		t.Errorf("NaN -> 0: got %d", got)
	}
	if got := safeAttrInt(map[string]any{"x": "+Inf"}, "x"); got != 0 {
		t.Errorf("Inf -> 0: got %d", got)
	}
	if got := safeAttrInt(map[string]any{"x": "3.9"}, "x"); got != 3 {
		t.Errorf("float string truncates: got %d, want 3", got)
	}
}

func TestSafeDurationMS(t *testing.T) {
	if got := safeDurationMS("1500000"); got != 1.5 {
		t.Errorf("1.5e6 ns -> 1.5ms: got %v", got)
	}
	if got := safeDurationMS(""); got != 0.0 {
		t.Errorf("blank -> 0.0: got %v", got)
	}
	if got := safeDurationMS("nope"); got != 0.0 {
		t.Errorf("unparseable -> 0.0: got %v", got)
	}
	if got := safeDurationMS("NaN"); got != 0.0 {
		t.Errorf("NaN -> 0.0: got %v", got)
	}
	if got := safeDurationMS(nil); got != 0.0 {
		t.Errorf("nil -> 0.0 (toStr empty): got %v", got)
	}
}

// ---- truncRunes / firstNonEmptyAny / isEmptyArg ----

func TestTruncRunes(t *testing.T) {
	if got := truncRunes("hello", 10); got != "hello" {
		t.Errorf("short string unchanged: got %q", got)
	}
	if got := truncRunes("hello world", 5); got != "hello" {
		t.Errorf("truncate: got %q, want hello", got)
	}
	// Multibyte-safe: truncate by rune count, not byte count.
	if got := truncRunes("héllo world", 3); got != "hél" {
		t.Errorf("multibyte truncate: got %q, want hél", got)
	}
}

func TestFirstNonEmptyAny(t *testing.T) {
	if got := firstNonEmptyAny(nil, "", "second"); got != "second" {
		t.Errorf("skips nil/empty: got %v, want second", got)
	}
	if got := firstNonEmptyAny(nil, ""); got != nil {
		t.Errorf("all empty -> nil: got %v", got)
	}
	if got := firstNonEmptyAny(0, "x"); got != 0 {
		// 0 (int) is not nil and not an empty string, so it's returned as-is (not Python-falsy).
		t.Errorf("non-string non-nil first value wins: got %v, want 0", got)
	}
}

func TestIsEmptyArg_MapAndDefault(t *testing.T) {
	if !isEmptyArg(map[string]any{}) {
		t.Error("empty map should be empty")
	}
	if isEmptyArg(map[string]any{"a": 1}) {
		t.Error("non-empty map should not be empty")
	}
	if isEmptyArg(42) {
		t.Error("an int (not in None/''/[]/{}) should not be considered empty")
	}
}

// ---- getAiFilterMetadata ----

func filterMetadataDB(t *testing.T, byExpr map[string][]string, errExprs map[string]bool) *storetest.FakeDB {
	t.Helper()
	return &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		for expr, vals := range byExpr {
			if strings.Contains(q, "SELECT DISTINCT "+expr+" AS v") {
				if errExprs[expr] {
					return nil, assertErr("boom: " + expr)
				}
				rows := make([][]any, 0, len(vals))
				for _, v := range vals {
					rows = append(rows, []any{v})
				}
				return &store.Result{Columns: []string{"v"}, Rows: rows}, nil
			}
		}
		return &store.Result{}, nil
	}}
}

func TestGetAiFilterMetadata_HappyPath(t *testing.T) {
	s := &server{db: filterMetadataDB(t, map[string][]string{
		"ServiceName":   {"checkout", "billing", ""}, // blank filtered out
		"RequestModel":  {"gpt-4o"},
		"OperationName": {"chat"},
		"SpanName":      {"llm.call"},
	}, nil)}
	meta := s.getAiFilterMetadata("", "")
	if len(meta.services) != 2 {
		t.Errorf("services = %v, want 2 (blank filtered, deduped+sorted)", meta.services)
	}
	if meta.services[0] != "billing" || meta.services[1] != "checkout" {
		t.Errorf("services should be sorted: got %v", meta.services)
	}
	if len(meta.models) != 1 || meta.models[0] != "gpt-4o" {
		t.Errorf("models = %v", meta.models)
	}
	if len(meta.errors) != 0 {
		t.Errorf("errors = %v, want none", meta.errors)
	}
}

func TestGetAiFilterMetadata_PartialDBErrorsCollected(t *testing.T) {
	s := &server{db: filterMetadataDB(t, map[string][]string{
		"ServiceName":   {"checkout"},
		"RequestModel":  {"gpt-4o"},
		"OperationName": {"chat"},
		"SpanName":      {"llm.call"},
	}, map[string]bool{"RequestModel": true})}
	meta := s.getAiFilterMetadata("2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z")
	if len(meta.services) != 1 {
		t.Errorf("services should still succeed: %v", meta.services)
	}
	if len(meta.models) != 0 {
		t.Errorf("models should be empty on error: %v", meta.models)
	}
	foundModelsErr := false
	for _, e := range meta.errors {
		if strings.HasPrefix(e, "models=") {
			foundModelsErr = true
		}
	}
	if !foundModelsErr {
		t.Errorf("errors should include a models= entry, got %v", meta.errors)
	}
}

func TestAiFilterMetadataSampleRows_DefaultAndOverride(t *testing.T) {
	if got := aiFilterMetadataSampleRows(); got != 10000 {
		t.Errorf("default sample rows = %d, want 10000", got)
	}
	t.Setenv("SOBS_AI_FILTER_METADATA_SAMPLE_ROWS", "500")
	if got := aiFilterMetadataSampleRows(); got != 500 {
		t.Errorf("override sample rows = %d, want 500", got)
	}
}
