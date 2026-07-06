package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Coverage batch 5: cmd/sobs/handlers_pages_logs_errors.go undertested branches. Oracle: the
// app.py dashboard query-helper family (_parse_limit/_parse_offset/_parse_sort, fromisoformat,
// _parse_trace_filter_values, _validate_re2_pattern, _prepare_re2_filter_patterns,
// _compute_log_stats, _fingerprint_log_message, _compute_advanced_log_analysis, and the small
// numeric/string coercion helpers).

// --- leftPad6 / parseLimitArg / parseOffsetArg / parseSortArg -----------------------------

func TestLeftPad6_Cov95B5(t *testing.T) {
	if got := leftPad6(5); got != "000005" {
		t.Fatalf("got %q", got)
	}
	if got := leftPad6(123456); got != "123456" {
		t.Fatalf("got %q", got)
	}
	if got := leftPad6(0); got != "000000" {
		t.Fatalf("got %q", got)
	}
}

func TestParseLimitArg_Cov95B5(t *testing.T) {
	t.Run("no_arg_uses_clamped_default", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x", nil)
		if got := parseLimitArg(r, 10000); got != 5000 {
			t.Fatalf("got %d, want clamp to 5000", got)
		}
	})
	t.Run("valid_value_clamped_to_range", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?limit=25", nil)
		if got := parseLimitArg(r, 100); got != 25 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("value_above_max_clamped", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?limit=999999", nil)
		if got := parseLimitArg(r, 100); got != 5000 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("value_below_min_clamped", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?limit=-5", nil)
		if got := parseLimitArg(r, 100); got != 1 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("invalid_value_uses_default_unclamped", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?limit=abc", nil)
		if got := parseLimitArg(r, 42); got != 42 {
			t.Fatalf("got %d", got)
		}
	})
}

func TestParseOffsetArg_Cov95B5(t *testing.T) {
	t.Run("no_arg_zero", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x", nil)
		if got := parseOffsetArg(r); got != 0 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("valid_positive", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?offset=15", nil)
		if got := parseOffsetArg(r); got != 15 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("negative_clamped_to_zero", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?offset=-5", nil)
		if got := parseOffsetArg(r); got != 0 {
			t.Fatalf("got %d", got)
		}
	})
	t.Run("invalid_value_zero", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?offset=nope", nil)
		if got := parseOffsetArg(r); got != 0 {
			t.Fatalf("got %d", got)
		}
	})
}

func TestParseSortArg_Cov95B5(t *testing.T) {
	allowed := map[string]string{"ts": "Timestamp", "svc": "ServiceName"}

	t.Run("defaults_when_absent", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x", nil)
		sortBy, sqlCol, sortDir := parseSortArg(r, allowed, "ts")
		if sortBy != "ts" || sqlCol != "Timestamp" || sortDir != "desc" {
			t.Fatalf("got (%q,%q,%q)", sortBy, sqlCol, sortDir)
		}
	})
	t.Run("explicit_valid_sort", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?sort_by=svc&sort_dir=asc", nil)
		sortBy, sqlCol, sortDir := parseSortArg(r, allowed, "ts")
		if sortBy != "svc" || sqlCol != "ServiceName" || sortDir != "asc" {
			t.Fatalf("got (%q,%q,%q)", sortBy, sqlCol, sortDir)
		}
	})
	t.Run("unknown_sort_by_falls_back_to_default", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?sort_by=bogus", nil)
		sortBy, sqlCol, _ := parseSortArg(r, allowed, "ts")
		if sortBy != "ts" || sqlCol != "Timestamp" {
			t.Fatalf("got (%q,%q)", sortBy, sqlCol)
		}
	})
	t.Run("invalid_sort_dir_falls_back_to_desc", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?sort_dir=sideways", nil)
		_, _, sortDir := parseSortArg(r, allowed, "ts")
		if sortDir != "desc" {
			t.Fatalf("got %q", sortDir)
		}
	})
}

// --- parseISOTime --------------------------------------------------------------------------

func TestParseISOTime_Cov95B5(t *testing.T) {
	t.Run("empty_returns_not_ok", func(t *testing.T) {
		_, ok := parseISOTime("  ")
		if ok {
			t.Fatal("expected ok=false for blank input")
		}
	})
	t.Run("date_only", func(t *testing.T) {
		_, ok := parseISOTime("2026-01-01")
		if !ok {
			t.Fatal("expected ok=true for date-only")
		}
	})
	t.Run("datetime_with_offset", func(t *testing.T) {
		_, ok := parseISOTime("2026-01-01T10:00:00-07:00")
		if !ok {
			t.Fatal("expected ok=true")
		}
	})
	t.Run("datetime_with_fractional_seconds", func(t *testing.T) {
		_, ok := parseISOTime("2026-01-01 10:00:00.123456")
		if !ok {
			t.Fatal("expected ok=true")
		}
	})
	t.Run("unparseable_returns_not_ok", func(t *testing.T) {
		_, ok := parseISOTime("not-a-date")
		if ok {
			t.Fatal("expected ok=false")
		}
	})
}

// --- parseTraceFilterValues -----------------------------------------------------------------

func TestParseTraceFilterValues_Cov95B5(t *testing.T) {
	t.Run("empty_returns_nil_and_empty_primary", func(t *testing.T) {
		parsed, primary := parseTraceFilterValues("", nil)
		if len(parsed) != 0 || primary != "" {
			t.Fatalf("got (%v,%q)", parsed, primary)
		}
	})

	t.Run("trace_id_prepended_before_trace_ids", func(t *testing.T) {
		parsed, primary := parseTraceFilterValues("ABC", []string{"def,ghi"})
		if primary != "abc" {
			t.Fatalf("expected primary=abc, got %q", primary)
		}
		if len(parsed) != 3 || parsed[0] != "abc" {
			t.Fatalf("got %v", parsed)
		}
	})

	t.Run("dedupes_case_insensitively", func(t *testing.T) {
		parsed, _ := parseTraceFilterValues("ABC", []string{"abc, ABC ,def"})
		count := 0
		for _, p := range parsed {
			if p == "abc" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("expected abc deduped once, got parsed=%v", parsed)
		}
	})

	t.Run("blank_parts_skipped", func(t *testing.T) {
		parsed, _ := parseTraceFilterValues("", []string{" , ,x"})
		if len(parsed) != 1 || parsed[0] != "x" {
			t.Fatalf("got %v", parsed)
		}
	})
}

// --- validateRE2Pattern / prepareRE2FilterPatterns -------------------------------------------

func TestValidateRE2Pattern_Cov95B5(t *testing.T) {
	t.Run("empty_pattern_is_valid", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			t.Fatal("should not query chDB for an empty pattern")
			return nil, nil
		}}}
		if got := s.validateRE2Pattern("  "); got != "" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("db_accepts_pattern_no_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return &store.Result{}, nil
		}}}
		if got := s.validateRE2Pattern("foo.*bar"); got != "" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("db_rejects_pattern_message_trimmed_at_marker", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("Some RE2 error: while executing function match: bad")
		}}}
		got := s.validateRE2Pattern("(bad")
		if !strings.HasPrefix(got, "Regex error: Some RE2 error") {
			t.Fatalf("got %q", got)
		}
		if strings.Contains(got, "while executing function") {
			t.Fatalf("expected trailing marker trimmed, got %q", got)
		}
	})

	t.Run("db_rejects_pattern_no_marker_full_message_kept", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("plain failure, no marker here")
		}}}
		got := s.validateRE2Pattern("x")
		if got != "Regex error: plain failure, no marker here" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestPrepareRE2FilterPatterns_Cov95B5(t *testing.T) {
	t.Run("empty_raw_returns_empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		inc, exc, errMsg := s.prepareRE2FilterPatterns("")
		if len(inc) != 0 || len(exc) != 0 || errMsg != "" {
			t.Fatalf("got (%v,%v,%q)", inc, exc, errMsg)
		}
	})

	t.Run("parse_error_propagates", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			t.Fatal("should not reach chDB validation when parse fails")
			return nil, nil
		}}}
		_, _, errMsg := s.prepareRE2FilterPatterns("&&")
		if errMsg == "" {
			t.Fatal("expected a parse error for invalid '&&' expression")
		}
	})

	t.Run("valid_include_and_exclude_pass_re2_validation", func(t *testing.T) {
		var probed []string
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if len(p) == 1 {
				probed = append(probed, p[0].(string))
			}
			return &store.Result{}, nil
		}}}
		inc, exc, errMsg := s.prepareRE2FilterPatterns("foo && !bar")
		if errMsg != "" {
			t.Fatalf("unexpected error: %q", errMsg)
		}
		if len(inc) != 1 || inc[0] != "foo" || len(exc) != 1 || exc[0] != "bar" {
			t.Fatalf("got inc=%v exc=%v", inc, exc)
		}
		if len(probed) != 2 {
			t.Fatalf("expected both patterns RE2-probed, got %v", probed)
		}
	})

	t.Run("re2_validation_failure_short_circuits", func(t *testing.T) {
		calls := 0
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			calls++
			return nil, errors.New("bad pattern")
		}}}
		_, _, errMsg := s.prepareRE2FilterPatterns("foo && bar")
		if errMsg == "" {
			t.Fatal("expected RE2 error")
		}
		if calls != 1 {
			t.Fatalf("expected short-circuit after first failing probe, got %d calls", calls)
		}
	})
}

// --- computeLogStats ------------------------------------------------------------------------

func TestComputeLogStats_Cov95B5(t *testing.T) {
	t.Run("empty_severity_becomes_unknown", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if strings.Contains(q, "GROUP BY SeverityText") {
				return storetest.Result([]string{"SeverityText", "cnt"}, []any{"", float64(4)}), nil
			}
			return &store.Result{}, nil
		}}}
		levelStats, _ := s.computeLogStats("", nil)
		v, ok := levelStats.Get("UNKNOWN")
		if !ok || v != 4 {
			t.Fatalf("got %v (ok=%v)", v, ok)
		}
	})

	t.Run("query_error_leaves_stats_empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		levelStats, serviceStats := s.computeLogStats("WHERE 1=1", nil)
		if levelStats.Len() != 0 || serviceStats.Len() != 0 {
			t.Fatalf("expected empty stats on error")
		}
	})

	t.Run("where_clause_present_uses_and_condition", func(t *testing.T) {
		var serviceQuery string
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if strings.Contains(q, "ServiceName!=''") && strings.Contains(q, "GROUP BY ServiceName") {
				serviceQuery = q
			}
			return &store.Result{}, nil
		}}}
		s.computeLogStats("WHERE Foo=1", []any{})
		if !strings.Contains(serviceQuery, "AND ServiceName!=''") {
			t.Fatalf("expected AND-joined condition, got %q", serviceQuery)
		}
	})

	t.Run("service_stats_populated", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if strings.Contains(q, "GROUP BY ServiceName") {
				return storetest.Result([]string{"ServiceName", "cnt"}, []any{"svc-a", float64(9)}), nil
			}
			return &store.Result{}, nil
		}}}
		_, serviceStats := s.computeLogStats("", nil)
		v, _ := serviceStats.Get("svc-a")
		if v != 9 {
			t.Fatalf("got %v", v)
		}
	})
}

// --- fingerprintLogMessage ------------------------------------------------------------------

func TestFingerprintLogMessage_Cov95B5(t *testing.T) {
	t.Run("empty_message", func(t *testing.T) {
		if got := fingerprintLogMessage("   "); got != "(empty message)" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("uuid_replaced", func(t *testing.T) {
		got := fingerprintLogMessage("id 123e4567-e89b-12d3-a456-426614174000 failed")
		if !strings.Contains(got, "<uuid>") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("hex_and_hash_and_numbers_and_quotes", func(t *testing.T) {
		got := fingerprintLogMessage(`retry 0xdeadbeef count 12345 val 'secret' and "other"`)
		if !strings.Contains(got, "<hex>") || !strings.Contains(got, "<num>") || !strings.Contains(got, "'<text>'") || !strings.Contains(got, `"<text>"`) {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("small_numbers_become_n", func(t *testing.T) {
		got := fingerprintLogMessage("retry 5 times")
		if !strings.Contains(got, "<n>") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("truncated_at_160_chars", func(t *testing.T) {
		got := fingerprintLogMessage(strings.Repeat("word ", 100))
		if len(got) > 160 {
			t.Fatalf("expected <=160 chars, got %d", len(got))
		}
	})
}

// --- computeAdvancedLogAnalysis ---------------------------------------------------------------

func TestComputeAdvancedLogAnalysis_Cov95B5(t *testing.T) {
	t.Run("no_messages_returns_empty_shape", func(t *testing.T) {
		got := computeAdvancedLogAnalysis(nil, jsonenc.NewObject(), jsonenc.NewObject())
		tp, _ := got.Get("top_patterns")
		if arr, ok := tp.([]any); !ok || len(arr) != 0 {
			t.Fatalf("got %v", tp)
		}
	})

	t.Run("populated_rows_produce_patterns_keywords_families_hints", func(t *testing.T) {
		rows := []map[string]any{
			{"Body": "connection TimeoutError occurred", "LogAttributes": map[string]any{"exception.type": "TimeoutError"}},
			{"Body": "connection TimeoutError occurred", "LogAttributes": map[string]any{"exception.type": "TimeoutError"}},
			{"Body": "connection TimeoutError occurred", "LogAttributes": map[string]any{}},
			{"Body": "unrelated info message", "LogAttributes": map[string]any{}},
		}
		levelStats := jsonenc.NewObject().Set("ERROR", 3).Set("INFO", 1)
		serviceStats := jsonenc.NewObject().Set("svc-a", 4)
		got := computeAdvancedLogAnalysis(rows, levelStats, serviceStats)

		tp, _ := got.Get("top_patterns")
		patterns, _ := tp.([]any)
		if len(patterns) == 0 {
			t.Fatal("expected at least one pattern")
		}
		first := patterns[0].(*jsonenc.Object)
		cnt, _ := first.Get("count")
		if cnt != 3 {
			t.Fatalf("expected top pattern count=3, got %v", cnt)
		}

		ef, _ := got.Get("error_families")
		families, _ := ef.([]any)
		if len(families) == 0 {
			t.Fatal("expected TimeoutError family detected")
		}

		hintsV, _ := got.Get("hints")
		hints, _ := hintsV.([]any)
		if len(hints) == 0 {
			t.Fatal("expected at least one hint (high severe ratio + repeat pattern + hot service)")
		}
		joined := ""
		for _, h := range hints {
			joined += h.(string) + "\n"
		}
		if !strings.Contains(joined, "severe-log ratio") {
			t.Fatalf("expected severe-ratio hint, got %s", joined)
		}
		if !strings.Contains(joined, "repeats") {
			t.Fatalf("expected repeat-pattern hint, got %s", joined)
		}
		if !strings.Contains(joined, "svc-a") {
			t.Fatalf("expected hot-service hint, got %s", joined)
		}
	})

	t.Run("timeout_keyword_hint", func(t *testing.T) {
		rows := []map[string]any{
			{"Body": "operation timeout after retry", "LogAttributes": map[string]any{}},
			{"Body": "operation timed out again", "LogAttributes": map[string]any{}},
			{"Body": "another timeout seen here", "LogAttributes": map[string]any{}},
		}
		got := computeAdvancedLogAnalysis(rows, jsonenc.NewObject(), jsonenc.NewObject())
		hintsV, _ := got.Get("hints")
		hints, _ := hintsV.([]any)
		joined := ""
		for _, h := range hints {
			joined += h.(string) + "\n"
		}
		if !strings.Contains(joined, "Timeout-related") {
			t.Fatalf("expected timeout hint, got %s", joined)
		}
	})

	t.Run("family_regex_dedupes_within_message", func(t *testing.T) {
		rows := []map[string]any{
			{"Body": "TimeoutError and TimeoutError again", "LogAttributes": map[string]any{}},
		}
		got := computeAdvancedLogAnalysis(rows, jsonenc.NewObject(), jsonenc.NewObject())
		ef, _ := got.Get("error_families")
		families, _ := ef.([]any)
		if len(families) != 1 {
			t.Fatalf("expected 1 deduped family, got %v", families)
		}
		fam := families[0].(*jsonenc.Object)
		cnt, _ := fam.Get("count")
		if cnt != 1 {
			t.Fatalf("expected count 1 (deduped per-message), got %v", cnt)
		}
	})
}

// --- pyRoundHalfEven / toIntVal / mapToDictStr / scalarStr -------------------------------------

func TestPyRoundHalfEven_Cov95B5(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{2.5, 2.0}, // banker's rounding: ties to even
		{3.5, 4.0},
		{2.4, 2.0},
		{2.6, 3.0},
		{-0.0, 0.0},
	}
	for _, c := range cases {
		if got := pyRoundHalfEven(c.in); got != c.want {
			t.Errorf("pyRoundHalfEven(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestToIntVal_Cov95B5(t *testing.T) {
	if got := toIntVal(5); got != 5 {
		t.Errorf("int: got %v", got)
	}
	if got := toIntVal(float64(7.9)); got != 7 {
		t.Errorf("float64: got %v", got)
	}
	if got := toIntVal("42"); got != 42 {
		t.Errorf("string: got %v", got)
	}
	if got := toIntVal("not-a-number"); got != 0 {
		t.Errorf("bad string: got %v", got)
	}
	if got := toIntVal(true); got != 0 {
		t.Errorf("default branch: got %v", got)
	}
}

func TestMapToDictStr_Cov95B5(t *testing.T) {
	t.Run("map_string_any", func(t *testing.T) {
		got := mapToDictStr(map[string]any{"a": "b", "n": true})
		if got["a"] != "b" || got["n"] != "True" {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("jsonenc_object", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("k", "v")
		got := mapToDictStr(obj)
		if got["k"] != "v" {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("empty_string_returns_empty", func(t *testing.T) {
		got := mapToDictStr("   ")
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("valid_json_string_object", func(t *testing.T) {
		got := mapToDictStr(`{"x":"y"}`)
		if got["x"] != "y" {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("json_string_non_object_returns_empty", func(t *testing.T) {
		got := mapToDictStr(`[1,2,3]`)
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("malformed_json_string_returns_empty", func(t *testing.T) {
		got := mapToDictStr(`{bad`)
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("unrecognized_type_returns_empty", func(t *testing.T) {
		got := mapToDictStr(42)
		if len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
}

func TestScalarStr_Cov95B5(t *testing.T) {
	if got := scalarStr("hi"); got != "hi" {
		t.Errorf("string: got %q", got)
	}
	if got := scalarStr(nil); got != "" {
		t.Errorf("nil: got %q", got)
	}
	if got := scalarStr(true); got != "True" {
		t.Errorf("true: got %q", got)
	}
	if got := scalarStr(false); got != "False" {
		t.Errorf("false: got %q", got)
	}
	if got := scalarStr(float64(5)); got != "5.0" {
		t.Errorf("default(number): got %q", got)
	}
}
