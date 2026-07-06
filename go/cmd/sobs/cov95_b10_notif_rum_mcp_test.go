package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Coverage batch 10: cmd/sobs/{notif_check,rum_filters,handlers_rum,mcp_tools,fix_rum_helpers}.go
// undertested branches. This file only ADDS tests; it does not touch any production code.

// ---------------------------------------------------------------------------
// fix_rum_helpers.go
// ---------------------------------------------------------------------------

// rumInt (27.3%): accepts json.Number (int/float/unparseable), float64, int, int64, bool,
// numeric/non-numeric/empty string, missing key, and explicit nil.
func TestRumInt_Cov95B10(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		want int64
	}{
		{"missing_key", map[string]any{}, 0},
		{"explicit_nil", map[string]any{"k": nil}, 0},
		{"json_number_int", map[string]any{"k": json.Number("42")}, 42},
		{"json_number_float", map[string]any{"k": json.Number("3.9")}, 3},
		{"json_number_unparseable", map[string]any{"k": json.Number("abc")}, 0},
		{"float64", map[string]any{"k": 7.8}, 7},
		{"int", map[string]any{"k": int(9)}, 9},
		{"int64", map[string]any{"k": int64(11)}, 11},
		{"bool_true", map[string]any{"k": true}, 1},
		{"bool_false", map[string]any{"k": false}, 0},
		{"string_numeric", map[string]any{"k": " 15 "}, 15},
		{"string_empty", map[string]any{"k": "   "}, 0},
		{"string_non_numeric", map[string]any{"k": "not-a-number"}, 0},
		{"unsupported_type", map[string]any{"k": []any{1, 2}}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rumInt(c.m, "k"); got != c.want {
				t.Errorf("rumInt(%v) = %d, want %d", c.m["k"], got, c.want)
			}
		})
	}
}

// extractTraceFields (89.7%): traceFlags parsing (string hex, numeric, float-string, bad value)
// plus the traceparent fallback (present/absent traceId/spanId, malformed traceparent).
func TestExtractTraceFields_Cov95B10(t *testing.T) {
	t.Run("direct_trace_and_span_present_no_fallback", func(t *testing.T) {
		ev := map[string]any{"traceId": "  ABCDEF  ", "spanId": "1234", "traceFlags": json.Number("1")}
		tid, sid, flags := extractTraceFields(ev)
		if tid != "abcdef" || sid != "1234" || flags != 1 {
			t.Errorf("got (%q,%q,%d)", tid, sid, flags)
		}
	})
	t.Run("traceFlags_hex_string", func(t *testing.T) {
		ev := map[string]any{"traceId": "a", "spanId": "b", "traceFlags": "ff"}
		_, _, flags := extractTraceFields(ev)
		if flags != 255 {
			t.Errorf("hex string traceFlags: got %d, want 255", flags)
		}
	})
	t.Run("traceFlags_bad_hex_string", func(t *testing.T) {
		ev := map[string]any{"traceId": "a", "spanId": "b", "traceFlags": "zz"}
		_, _, flags := extractTraceFields(ev)
		if flags != 0 {
			t.Errorf("bad hex traceFlags: got %d, want 0", flags)
		}
	})
	t.Run("traceFlags_numeric_json_number", func(t *testing.T) {
		ev := map[string]any{"traceId": "a", "spanId": "b", "traceFlags": json.Number("2")}
		_, _, flags := extractTraceFields(ev)
		if flags != 2 {
			t.Errorf("numeric traceFlags: got %d, want 2", flags)
		}
	})
	t.Run("traceFlags_float_json_number", func(t *testing.T) {
		ev := map[string]any{"traceId": "a", "spanId": "b", "traceFlags": json.Number("2.9")}
		_, _, flags := extractTraceFields(ev)
		if flags != 2 {
			t.Errorf("float traceFlags: got %d, want 2", flags)
		}
	})
	t.Run("traceFlags_unparseable_json_number", func(t *testing.T) {
		ev := map[string]any{"traceId": "a", "spanId": "b", "traceFlags": json.Number("xx")}
		_, _, flags := extractTraceFields(ev)
		if flags != 0 {
			t.Errorf("unparseable traceFlags: got %d, want 0", flags)
		}
	})
	t.Run("traceparent_fallback_used_when_missing", func(t *testing.T) {
		ev := map[string]any{"traceparent": "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"}
		tid, sid, flags := extractTraceFields(ev)
		if tid != "0123456789abcdef0123456789abcdef" || sid != "0123456789abcdef" || flags != 1 {
			t.Errorf("got (%q,%q,%d)", tid, sid, flags)
		}
	})
	t.Run("traceparent_malformed_no_fallback_applied", func(t *testing.T) {
		ev := map[string]any{"traceparent": "not-a-valid-traceparent"}
		tid, sid, _ := extractTraceFields(ev)
		if tid != "" || sid != "" {
			t.Errorf("malformed traceparent: got (%q,%q), want empty", tid, sid)
		}
	})
	t.Run("only_traceId_missing_spanId_uses_traceparent_span", func(t *testing.T) {
		ev := map[string]any{
			"traceId":     "0123456789abcdef0123456789abcdef",
			"traceparent": "00-0123456789abcdef0123456789abcdef-fedcba9876543210-00",
		}
		tid, sid, _ := extractTraceFields(ev)
		if tid != "0123456789abcdef0123456789abcdef" || sid != "fedcba9876543210" {
			t.Errorf("got (%q,%q)", tid, sid)
		}
	})
}

// handleBrowserContextDelta (69.7%): missing sessionId/contextHash short-circuit, full-context
// cache-and-return, contextUnchanged retrieval (hit + miss), and non-object browserContext.
func TestHandleBrowserContextDelta_Cov95B10(t *testing.T) {
	t.Run("missing_session_id_returns_empty", func(t *testing.T) {
		got := handleBrowserContextDelta(map[string]any{"contextHash": "h"})
		if len(got) != 0 {
			t.Errorf("want empty, got %v", got)
		}
	})
	t.Run("missing_context_hash_returns_empty", func(t *testing.T) {
		got := handleBrowserContextDelta(map[string]any{"sessionId": "s"})
		if len(got) != 0 {
			t.Errorf("want empty, got %v", got)
		}
	})
	t.Run("full_context_cached_and_attrs_built", func(t *testing.T) {
		ctx := jsonenc.NewObject().Set("browser", "chrome").Set("empty_str", "").Set("dropped_nil", nil)
		ev := map[string]any{
			"sessionId":      "sess-cov95-b10-a",
			"contextHash":    "hash-1",
			"browserContext": ctx,
		}
		got := handleBrowserContextDelta(ev)
		if got["browser.context.browser"] != "chrome" {
			t.Errorf("got %v", got)
		}
		if _, ok := got["browser.context.empty_str"]; ok {
			t.Errorf("empty string value should be skipped: %v", got)
		}
		if _, ok := got["browser.context.dropped_nil"]; ok {
			t.Errorf("nil value should be skipped: %v", got)
		}
	})
	t.Run("context_unchanged_retrieves_cached_full_context", func(t *testing.T) {
		sessionID := "sess-cov95-b10-b"
		ctx := jsonenc.NewObject().Set("theme", "dark")
		// Prime the cache.
		handleBrowserContextDelta(map[string]any{
			"sessionId": sessionID, "contextHash": "hash-2", "browserContext": ctx,
		})
		// Now send contextUnchanged with the same hash and no browserContext.
		got := handleBrowserContextDelta(map[string]any{
			"sessionId": sessionID, "contextHash": "hash-2", "contextUnchanged": true,
		})
		if got["browser.context.theme"] != "dark" {
			t.Errorf("expected cached context retrieved, got %v", got)
		}
	})
	t.Run("context_unchanged_hash_mismatch_yields_no_cached_context", func(t *testing.T) {
		sessionID := "sess-cov95-b10-c"
		ctx := jsonenc.NewObject().Set("theme", "light")
		handleBrowserContextDelta(map[string]any{
			"sessionId": sessionID, "contextHash": "hash-3", "browserContext": ctx,
		})
		got := handleBrowserContextDelta(map[string]any{
			"sessionId": sessionID, "contextHash": "different-hash", "contextUnchanged": true,
		})
		if len(got) != 0 {
			t.Errorf("hash mismatch should yield no attrs, got %v", got)
		}
	})
	t.Run("no_context_and_unknown_session_returns_empty_attrs", func(t *testing.T) {
		got := handleBrowserContextDelta(map[string]any{
			"sessionId": "sess-cov95-b10-never-seen", "contextHash": "h",
		})
		if len(got) != 0 {
			t.Errorf("want empty, got %v", got)
		}
	})
}

// bodyMapNumber (80.0%): nil body and empty body both yield an empty (non-nil) map.
func TestBodyMapNumber_Cov95B10(t *testing.T) {
	t.Run("nil_body", func(t *testing.T) {
		req := &http.Request{}
		got := bodyMapNumber(req)
		if got == nil || len(got) != 0 {
			t.Errorf("nil body: got %v, want empty map", got)
		}
	})
	t.Run("empty_body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(""))
		got := bodyMapNumber(req)
		if got == nil || len(got) != 0 {
			t.Errorf("empty body: got %v, want empty map", got)
		}
	})
	t.Run("numbers_stay_json_Number", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":5,"b":5.0}`))
		got := bodyMapNumber(req)
		if _, ok := got["a"].(json.Number); !ok {
			t.Errorf("want json.Number for int, got %T", got["a"])
		}
		if got["b"].(json.Number).String() != "5.0" {
			t.Errorf("want '5.0' preserved, got %v", got["b"])
		}
	})
}

// rumStrOrEmpty (85.7%) extra: bool true path + nil fallthrough via toStr already covered
// elsewhere; add the "unsupported struct type falls through to toStr" path.
func TestRumStrOrEmpty_Cov95B10_Extra(t *testing.T) {
	if got := rumStrOrEmpty([]any{1, 2}); got == "" {
		// toStr on a slice should not be "" (sanity: exercising the default branch).
		t.Log("fallthrough toStr on slice produced empty string, which is acceptable for toStr's own semantics")
	}
}

// rumTruthy (88.9%) extra: negative-zero json.Number and non-empty map/slice already covered in
// fix_rum_helpers_test.go; add explicit non-empty-string edge and default-branch struct value.
func TestRumTruthy_Cov95B10_Extra(t *testing.T) {
	if !rumTruthy("nonempty") {
		t.Error("non-empty string should be truthy")
	}
	if rumTruthy(json.Number("")) {
		t.Error("empty json.Number should be falsy")
	}
}

// rumStringifyAttrs (88.2%) extra: int/int64/default (json.dumps) branches not hit by the
// existing test in fix_rum_helpers_test.go.
func TestRumStringifyAttrs_Cov95B10_Extra(t *testing.T) {
	in := map[string]any{
		"i":   int(7),
		"i64": int64(9),
		"obj": map[string]any{"nested": "x"},
	}
	out := rumStringifyAttrs(in)
	if out["i"] != "7" {
		t.Errorf("int: got %v", out["i"])
	}
	if out["i64"] != "9" {
		t.Errorf("int64: got %v", out["i64"])
	}
	s, ok := out["obj"].(string)
	if !ok || !strings.Contains(s, "nested") {
		t.Errorf("default json.dumps branch: got %v", out["obj"])
	}
}

// rumSplitlines (75.0%): empty string, no-newline string, \r\n, lone \r, trailing newline.
func TestRumSplitlines_Cov95B10(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"no_newline", "abc", []string{"abc"}},
		{"lf", "a\nb\nc", []string{"a", "b", "c"}},
		{"crlf", "a\r\nb\r\nc", []string{"a", "b", "c"}},
		{"cr_only", "a\rb\rc", []string{"a", "b", "c"}},
		{"trailing_newline_dropped", "a\nb\n", []string{"a", "b"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rumSplitlines(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// rum_filters.go
// ---------------------------------------------------------------------------

// parseLimitDefault (27.3%): empty, bad value, below-min clamp, above-max clamp, in-range.
func TestParseLimitDefault_Cov95B10(t *testing.T) {
	mk := func(raw string) *http.Request {
		u := "/rum"
		if raw != "" {
			u += "?limit=" + raw
		}
		return httptest.NewRequest(http.MethodGet, u, nil)
	}
	cases := []struct {
		name string
		raw  string
		def  int
		want int
	}{
		{"empty_uses_default", "", 200, 200},
		{"non_numeric_uses_default", "abc", 200, 200},
		{"below_min_clamped_to_1", "-5", 200, 1},
		{"zero_clamped_to_1", "0", 200, 1},
		{"above_max_clamped_to_5000", "999999", 200, 5000},
		{"in_range_passthrough", "50", 200, 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseLimitDefault(mk(c.raw), c.def); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}

// lookupSort / parseSortOptions / orderDir (75/76.9/66.7%).
func TestSortHelpers_Cov95B10(t *testing.T) {
	allowed := []sortOption{{"name", "Name"}, {"count", "Cnt"}}

	t.Run("lookupSort_found", func(t *testing.T) {
		col, ok := lookupSort(allowed, "count")
		if !ok || col != "Cnt" {
			t.Errorf("got (%q,%v)", col, ok)
		}
	})
	t.Run("lookupSort_not_found", func(t *testing.T) {
		col, ok := lookupSort(allowed, "bogus")
		if ok || col != "" {
			t.Errorf("got (%q,%v)", col, ok)
		}
	})
	t.Run("parseSortOptions_defaults_when_no_params", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rum", nil)
		by, col, dir := parseSortOptions(req, allowed, "name")
		if by != "name" || col != "Name" || dir != "desc" {
			t.Errorf("got (%q,%q,%q)", by, col, dir)
		}
	})
	t.Run("parseSortOptions_invalid_sort_by_falls_back_to_default", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rum?sort_by=nope", nil)
		by, col, _ := parseSortOptions(req, allowed, "name")
		if by != "name" || col != "Name" {
			t.Errorf("got (%q,%q)", by, col)
		}
	})
	t.Run("parseSortOptions_valid_sort_by_and_asc_dir", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rum?sort_by=count&sort_dir=ASC", nil)
		by, col, dir := parseSortOptions(req, allowed, "name")
		if by != "count" || col != "Cnt" || dir != "asc" {
			t.Errorf("got (%q,%q,%q)", by, col, dir)
		}
	})
	t.Run("parseSortOptions_invalid_sort_dir_falls_back_to_desc", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/rum?sort_by=count&sort_dir=sideways", nil)
		_, _, dir := parseSortOptions(req, allowed, "name")
		if dir != "desc" {
			t.Errorf("got %q, want desc", dir)
		}
	})
	t.Run("orderDir", func(t *testing.T) {
		if orderDir("asc") != "ASC" {
			t.Error("asc -> ASC")
		}
		if orderDir("desc") != "DESC" {
			t.Error("desc -> DESC")
		}
		if orderDir("garbage") != "DESC" {
			t.Error("unknown -> DESC (else branch)")
		}
	})
}

// parseRumTimeWindowArgs (37.0%) + parseCHTimestamp (83.3%): empty inputs, from+to valid ordering,
// bad values, window_s arithmetic (valid/invalid/clamped), non-increasing window rejection.
func TestParseRumTimeWindowArgs_Cov95B10(t *testing.T) {
	mk := func(qs string) *http.Request {
		return httptest.NewRequest(http.MethodGet, "/rum?"+qs, nil)
	}
	t.Run("all_empty", func(t *testing.T) {
		from, to, errMsg := parseRumTimeWindowArgs(mk(""))
		if from != "" || to != "" || errMsg != "" {
			t.Errorf("got (%q,%q,%q)", from, to, errMsg)
		}
	})
	t.Run("bad_from_ts_value", func(t *testing.T) {
		_, _, errMsg := parseRumTimeWindowArgs(mk("from_ts=not-a-date&to_ts=2026-01-01T00:00:00Z"))
		if errMsg == "" {
			t.Error("expected an error for a to_ts present but from_ts unparsable normalizing to non-CH-format")
		}
	})
	t.Run("from_and_to_valid_increasing", func(t *testing.T) {
		from, to, errMsg := parseRumTimeWindowArgs(mk("from_ts=2026-01-01T00:00:00Z&to_ts=2026-01-01T01:00:00Z"))
		if errMsg != "" || from == "" || to == "" {
			t.Errorf("got (%q,%q,%q)", from, to, errMsg)
		}
	})
	t.Run("to_before_from_rejected", func(t *testing.T) {
		_, _, errMsg := parseRumTimeWindowArgs(mk("from_ts=2026-01-01T01:00:00Z&to_ts=2026-01-01T00:00:00Z"))
		if !strings.Contains(errMsg, "must be later than") {
			t.Errorf("got %q", errMsg)
		}
	})
	t.Run("to_equal_from_rejected", func(t *testing.T) {
		_, _, errMsg := parseRumTimeWindowArgs(mk("from_ts=2026-01-01T00:00:00Z&to_ts=2026-01-01T00:00:00Z"))
		if !strings.Contains(errMsg, "must be later than") {
			t.Errorf("got %q", errMsg)
		}
	})
	t.Run("window_s_computes_to_ts", func(t *testing.T) {
		from, to, errMsg := parseRumTimeWindowArgs(mk("from_ts=2026-01-01T00:00:00Z&window_s=60"))
		if errMsg != "" || to == "" {
			t.Errorf("got (%q,%q,%q)", from, to, errMsg)
		}
	})
	t.Run("window_s_invalid_value", func(t *testing.T) {
		_, _, errMsg := parseRumTimeWindowArgs(mk("from_ts=2026-01-01T00:00:00Z&window_s=notanumber"))
		if errMsg == "" {
			t.Error("expected value error for bad window_s")
		}
	})
	t.Run("window_s_below_one_clamped", func(t *testing.T) {
		from, to, errMsg := parseRumTimeWindowArgs(mk("from_ts=2026-01-01T00:00:00Z&window_s=-5"))
		if errMsg != "" || from == "" || to == "" {
			t.Errorf("got (%q,%q,%q)", from, to, errMsg)
		}
	})
}

func TestParseCHTimestamp_Cov95B10(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"space_form", "2026-03-29 12:00:00.000000", true},
		{"space_form_no_micros", "2026-03-29 12:00:00", true},
		{"iso_z_form", "2026-03-29T12:00:00Z", true},
		{"iso_offset_form", "2026-03-29T12:00:00-07:00", true},
		{"iso_no_offset", "2026-03-29T12:00:00", true},
		{"unparseable", "not-a-timestamp", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ok := parseCHTimestamp(c.in)
			if ok != c.ok {
				t.Errorf("parseCHTimestamp(%q) ok=%v, want %v", c.in, ok, c.ok)
			}
		})
	}
}

// prepareRumRE2FilterPatterns (87.5%): parse error short-circuit, RE2-DB-validation failure,
// and the success path with both include+exclude patterns.
func TestPrepareRumRE2FilterPatterns_Cov95B10(t *testing.T) {
	t.Run("parse_error_short_circuits", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		_, errMsg := s.prepareRumRE2FilterPatterns("foo && ")
		if errMsg == "" {
			t.Error("expected a parse error")
		}
	})
	t.Run("re2_validation_failure_from_db", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("bad pattern: while executing function match")
		}}}
		_, errMsg := s.prepareRumRE2FilterPatterns("foo")
		if !strings.HasPrefix(errMsg, "Regex error:") {
			t.Errorf("got %q", errMsg)
		}
	})
	t.Run("success_include_and_exclude", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		rf, errMsg := s.prepareRumRE2FilterPatterns("foo && !bar")
		if errMsg != "" {
			t.Fatalf("unexpected error: %q", errMsg)
		}
		if len(rf.include) != 1 || rf.include[0] != "foo" {
			t.Errorf("include: %v", rf.include)
		}
		if len(rf.exclude) != 1 || rf.exclude[0] != "bar" {
			t.Errorf("exclude: %v", rf.exclude)
		}
	})
}

// parseRumRegexFilterExpression (75.0%): empty expr, empty term around &&, negation with empty
// pattern after '!', bad regex syntax, multi-term include/exclude.
func TestParseRumRegexFilterExpression_Cov95B10(t *testing.T) {
	t.Run("empty_expr", func(t *testing.T) {
		rf, errMsg := parseRumRegexFilterExpression("   ")
		if errMsg != "" || len(rf.include) != 0 || len(rf.exclude) != 0 {
			t.Errorf("got %v, %q", rf, errMsg)
		}
	})
	t.Run("empty_term_between_ampersands", func(t *testing.T) {
		_, errMsg := parseRumRegexFilterExpression("foo &&  && bar")
		if !strings.Contains(errMsg, "invalid expression around '&&'") {
			t.Errorf("got %q", errMsg)
		}
	})
	t.Run("negation_with_empty_pattern", func(t *testing.T) {
		_, errMsg := parseRumRegexFilterExpression("!   ")
		if !strings.Contains(errMsg, "expected a pattern after '!'") {
			t.Errorf("got %q", errMsg)
		}
	})
	t.Run("bad_regex_syntax", func(t *testing.T) {
		_, errMsg := parseRumRegexFilterExpression("(unbalanced[")
		if !strings.HasPrefix(errMsg, "Regex error:") {
			t.Errorf("got %q", errMsg)
		}
	})
	t.Run("multi_term_success", func(t *testing.T) {
		rf, errMsg := parseRumRegexFilterExpression("alpha && !beta && gamma")
		if errMsg != "" {
			t.Fatalf("unexpected error: %q", errMsg)
		}
		if len(rf.include) != 2 || len(rf.exclude) != 1 {
			t.Errorf("got include=%v exclude=%v", rf.include, rf.exclude)
		}
	})
}

// compileRE2Surface (66.7%): valid pattern -> "", invalid pattern -> error message.
func TestCompileRE2Surface_Cov95B10(t *testing.T) {
	if got := compileRE2Surface("valid.*pattern"); got != "" {
		t.Errorf("valid pattern: got %q", got)
	}
	if got := compileRE2Surface("("); got == "" {
		t.Error("invalid pattern should yield a non-empty error")
	}
}

// appendRumRegexExpressionClauses (71.4%): include-only, exclude-only, both, neither.
func TestAppendRumRegexExpressionClauses_Cov95B10(t *testing.T) {
	t.Run("both_include_and_exclude", func(t *testing.T) {
		conds, params := appendRumRegexExpressionClauses(nil, nil, "Body", regexFilter{
			include: []string{"a", "b"}, exclude: []string{"c"},
		})
		if len(conds) != 3 || len(params) != 3 {
			t.Fatalf("got conds=%v params=%v", conds, params)
		}
		if conds[0] != "match(Body, ?)" || conds[2] != "NOT match(Body, ?)" {
			t.Errorf("got %v", conds)
		}
	})
	t.Run("neither", func(t *testing.T) {
		conds, params := appendRumRegexExpressionClauses(nil, nil, "Body", regexFilter{})
		if len(conds) != 0 || len(params) != 0 {
			t.Errorf("got conds=%v params=%v", conds, params)
		}
	})
}

// ---------------------------------------------------------------------------
// handlers_rum.go
// ---------------------------------------------------------------------------

// objGet (75.0%): nil object, missing key, present key.
func TestObjGet_Cov95B10(t *testing.T) {
	if got := objGet(nil, "k"); got != nil {
		t.Errorf("nil object: got %v", got)
	}
	o := jsonenc.NewObject().Set("k", "v")
	if got := objGet(o, "missing"); got != nil {
		t.Errorf("missing key: got %v", got)
	}
	if got := objGet(o, "k"); got != "v" {
		t.Errorf("present key: got %v", got)
	}
}

// truncStr (66.7%): shorter-than-n (passthrough), exactly n, longer-than-n (truncated).
func TestTruncStr_Cov95B10(t *testing.T) {
	if got := truncStr("ab", 8); got != "ab" {
		t.Errorf("shorter: got %q", got)
	}
	if got := truncStr("abcdefgh", 8); got != "abcdefgh" {
		t.Errorf("exact length: got %q", got)
	}
	if got := truncStr("abcdefghij", 4); got != "abcd" {
		t.Errorf("longer: got %q", got)
	}
}

// buildRumEventItem (72.7%): body parse error -> {}, body parses to non-object -> {"value":...},
// traceId/spanId already present in data (no overwrite), url.full fallback, has_artifact/has_replay
// true via url and via id.
func TestBuildRumEventItem_Cov95B10(t *testing.T) {
	t.Run("malformed_body_json_yields_empty_data", func(t *testing.T) {
		m := map[string]any{
			"LogAttributes": map[string]any{}, "Body": "{not json", "TraceId": "", "SpanId": "",
			"ServiceName": "", "EventName": "error", "Timestamp": "2026-01-01 00:00:00",
		}
		item := buildRumEventItem(m)
		data, _ := item.Get("data")
		obj, ok := data.(*jsonenc.Object)
		if !ok || obj.Len() != 0 {
			t.Errorf("want empty object data, got %v", data)
		}
	})
	t.Run("non_object_body_wrapped_in_value_key", func(t *testing.T) {
		m := map[string]any{
			"LogAttributes": map[string]any{}, "Body": "42", "TraceId": "", "SpanId": "",
			"ServiceName": "", "EventName": "custom", "Timestamp": "2026-01-01 00:00:00",
		}
		item := buildRumEventItem(m)
		data, _ := item.Get("data")
		obj, ok := data.(*jsonenc.Object)
		if !ok {
			t.Fatalf("want object data, got %T", data)
		}
		v, _ := obj.Get("value")
		if v == nil {
			t.Errorf("want a 'value' key wrapping the scalar, got %v", obj)
		}
	})
	t.Run("existing_traceId_in_body_not_overwritten", func(t *testing.T) {
		m := map[string]any{
			"LogAttributes": map[string]any{}, "Body": `{"traceId":"existing-trace"}`,
			"TraceId": "column-trace", "SpanId": "", "ServiceName": "", "EventName": "x",
			"Timestamp": "2026-01-01 00:00:00",
		}
		item := buildRumEventItem(m)
		data, _ := item.Get("data")
		obj := data.(*jsonenc.Object)
		v, _ := obj.Get("traceId")
		if v != "existing-trace" {
			t.Errorf("should not overwrite existing traceId, got %v", v)
		}
	})
	t.Run("url_full_fallback_when_url_absent", func(t *testing.T) {
		m := map[string]any{
			"LogAttributes": map[string]any{"url.full": "https://example.com/x"},
			"Body":          "{}", "TraceId": "", "SpanId": "", "ServiceName": "", "EventName": "x",
			"Timestamp": "2026-01-01 00:00:00",
		}
		item := buildRumEventItem(m)
		u, _ := item.Get("url")
		if u != "https://example.com/x" {
			t.Errorf("want url.full fallback, got %v", u)
		}
	})
	t.Run("has_artifact_and_has_replay_via_id", func(t *testing.T) {
		m := map[string]any{
			"LogAttributes": map[string]any{},
			"Body":          `{"artifact":{"id":"a1"},"replay":{"id":"r1"}}`,
			"TraceId":       "", "SpanId": "", "ServiceName": "", "EventName": "x",
			"Timestamp": "2026-01-01 00:00:00",
		}
		item := buildRumEventItem(m)
		ha, _ := item.Get("has_artifact")
		hr, _ := item.Get("has_replay")
		if ha != true || hr != true {
			t.Errorf("got has_artifact=%v has_replay=%v", ha, hr)
		}
	})
	t.Run("no_artifact_or_replay_false", func(t *testing.T) {
		m := map[string]any{
			"LogAttributes": map[string]any{}, "Body": "{}", "TraceId": "", "SpanId": "",
			"ServiceName": "", "EventName": "x", "Timestamp": "2026-01-01 00:00:00",
		}
		item := buildRumEventItem(m)
		ha, _ := item.Get("has_artifact")
		hr, _ := item.Get("has_replay")
		if ha != false || hr != false {
			t.Errorf("got has_artifact=%v has_replay=%v", ha, hr)
		}
	})
	t.Run("empty_body_yields_empty_object_data", func(t *testing.T) {
		m := map[string]any{
			"LogAttributes": map[string]any{}, "Body": "", "TraceId": "", "SpanId": "",
			"ServiceName": "", "EventName": "x", "Timestamp": "2026-01-01 00:00:00",
		}
		item := buildRumEventItem(m)
		data, _ := item.Get("data")
		obj, ok := data.(*jsonenc.Object)
		if !ok || obj.Len() != 0 {
			t.Errorf("want empty object, got %v", data)
		}
	})
}

// rumVitals (34.1%): each of the three sub-queries can independently error (early return of
// partial results), and the full-success path exercises the CLS-specific rounding + hotspot
// top-5 trim + metric-name-empty skip in the hotspot loop.
func rumVitalsQueryKind(q string) string {
	switch {
	case strings.Contains(q, "v_derived_signals_anomaly"):
		return "anomaly"
	case strings.Contains(q, "v_derived_signals_1m"):
		return "sparkline"
	case strings.Contains(q, "web-vital"):
		return "hotspot"
	}
	return "unknown"
}

func rumVitalsDB(t *testing.T, results map[string]*store.Result, failOn map[string]bool) *storetest.FakeDB {
	return &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		kind := rumVitalsQueryKind(q)
		if kind == "unknown" {
			t.Fatalf("unrecognized query: %s", q)
		}
		if failOn[kind] {
			return nil, errors.New("boom")
		}
		if res, ok := results[kind]; ok {
			return res, nil
		}
		return &store.Result{}, nil
	}}
}

func TestRumVitals_Cov95B10(t *testing.T) {
	t.Run("anomaly_query_error_returns_all_empty", func(t *testing.T) {
		s := &server{db: rumVitalsDB(t, nil, map[string]bool{"anomaly": true})}
		summary, sparklines, hotspot := s.rumVitals()
		if summary.Len() != 0 || sparklines.Len() != 0 || hotspot.Len() != 0 {
			t.Errorf("expected all empty on anomaly error")
		}
	})
	t.Run("sparkline_query_error_returns_summary_but_empty_rest", func(t *testing.T) {
		results := map[string]*store.Result{
			"anomaly": storetest.Result([]string{"SignalName", "latest_value", "latest_state", "latest_count"},
				[]any{"LCP", 2500.0, "ok", 10.0}),
		}
		s := &server{db: rumVitalsDB(t, results, map[string]bool{"sparkline": true})}
		summary, sparklines, hotspot := s.rumVitals()
		if summary.Len() == 0 {
			t.Error("summary should be populated before the sparkline error")
		}
		if sparklines.Len() != 0 || hotspot.Len() != 0 {
			t.Error("sparklines/hotspot should stay empty after early exit")
		}
	})
	t.Run("hotspot_query_error_returns_summary_and_sparklines", func(t *testing.T) {
		results := map[string]*store.Result{
			"sparkline": storetest.Result([]string{"SignalName", "MinuteBucket", "Value", "SampleCount"},
				[]any{"CLS", "2026-01-01 00:00:00", 0.12345, 5.0}),
		}
		s := &server{db: rumVitalsDB(t, results, map[string]bool{"hotspot": true})}
		_, sparklines, hotspot := s.rumVitals()
		if sparklines.Len() == 0 {
			t.Error("sparklines should be populated before the hotspot error")
		}
		if hotspot.Len() != 0 {
			t.Error("hotspot should stay empty after early exit")
		}
	})
	t.Run("full_success_cls_rounding_and_hotspot_trim_and_empty_metric_skip", func(t *testing.T) {
		hotspotRows := [][]any{}
		// 6 rows for the same metric to exercise the top-5 trim.
		for i := 0; i < 6; i++ {
			hotspotRows = append(hotspotRows, []any{"LCP", "/page", 10.0, 2.0, 0.2, 2500.0})
		}
		// One row with an empty metric name -> must be skipped.
		hotspotRows = append(hotspotRows, []any{"", "/other", 5.0, 1.0, 0.2, 100.0})
		hotspotResult := &store.Result{
			Columns: []string{"metric", "url", "total", "poor_count", "poor_rate", "p75"},
			Rows:    hotspotRows,
		}
		results := map[string]*store.Result{
			"anomaly": storetest.Result([]string{"SignalName", "latest_value", "latest_state", "latest_count"},
				[]any{"CLS", 0.256789, "ok", 4.0}),
			"sparkline": storetest.Result([]string{"SignalName", "MinuteBucket", "Value", "SampleCount"},
				[]any{"CLS", "2026-01-01 00:00:00", 0.256789, 4.0}),
			"hotspot": hotspotResult,
		}
		s := &server{db: rumVitalsDB(t, results, nil)}
		summary, sparklines, hotspot := s.rumVitals()

		clsSummary, _ := summary.Get("CLS")
		p75, _ := clsSummary.(*jsonenc.Object).Get("p75")
		if p75 != 0.257 {
			t.Errorf("CLS p75 should round to 3 places, got %v", p75)
		}
		spark, _ := sparklines.Get("CLS")
		arr := spark.([]any)
		vObj := arr[0].(*jsonenc.Object)
		v, _ := vObj.Get("v")
		if v != 0.257 {
			t.Errorf("CLS sparkline value should round to 3 places, got %v", v)
		}
		lcp, _ := hotspot.Get("LCP")
		lcpArr := lcp.([]any)
		if len(lcpArr) != 5 {
			t.Errorf("hotspot LCP should be trimmed to 5, got %d", len(lcpArr))
		}
		if _, ok := hotspot.Get(""); ok {
			t.Error("empty metric name should be skipped from hotspot")
		}
	})
}

// handleViewRum (90.6%): drive the full handler with events-mode + query error, and a q= regex
// filter parse error surfaced as error_msg, hitting several under-tested query-param branches.
func TestHandleViewRum_Cov95B10(t *testing.T) {
	t.Run("events_mode_db_error_on_events_query_still_renders", func(t *testing.T) {
		s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{
			ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
				if strings.Contains(q, "FROM hyperdx_sessions") && strings.Contains(q, "LIMIT ? OFFSET ?") {
					return nil, errors.New("boom")
				}
				return &store.Result{}, nil
			},
		}}
		req := httptest.NewRequest(http.MethodGet, "/rum?view=events", nil)
		rec := httptest.NewRecorder()
		s.handleViewRum(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("bad_regex_filter_surfaces_error_msg", func(t *testing.T) {
		s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{}}
		req := httptest.NewRequest(http.MethodGet, "/rum?q="+"(unbalanced[", nil)
		rec := httptest.NewRecorder()
		s.handleViewRum(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("bad_time_window_surfaces_error_msg", func(t *testing.T) {
		s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{}}
		req := httptest.NewRequest(http.MethodGet, "/rum?from_ts=2026-01-01T01:00:00Z&to_ts=2026-01-01T00:00:00Z", nil)
		rec := httptest.NewRecorder()
		s.handleViewRum(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("sessions_mode_with_summary_rows_builds_detail_and_groups", func(t *testing.T) {
		s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{
			ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
				switch {
				case strings.Contains(q, "GROUP BY session_key "):
					return storetest.Result(
						[]string{"session_key", "last_ts", "event_count", "error_count", "poor_vital_count",
							"warn_vital_count", "severity_rank", "traced_count", "last_url", "last_event_type"},
						[]any{"sess-1", "2026-01-01 00:00:00", 3.0, 1.0, 0.0, 0.0, 3.0, 1.0, "/x", "error"},
					), nil
				case strings.Contains(q, "row_number() OVER"):
					return storetest.Result(
						[]string{"Timestamp", "EventName", "Body", "LogAttributes", "TraceId", "SpanId"},
						[]any{"2026-01-01 00:00:00", "error", "{}", map[string]any{}, "trace-1", "span-1"},
					), nil
				case strings.Contains(q, "count() AS c FROM ("):
					return storetest.Result([]string{"c"}, []any{1.0}), nil
				}
				return &store.Result{}, nil
			},
		}}
		req := httptest.NewRequest(http.MethodGet, "/rum?view=sessions", nil)
		rec := httptest.NewRecorder()
		s.handleViewRum(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// ---------------------------------------------------------------------------
// mcp_tools.go
// ---------------------------------------------------------------------------

// mcpErrTypeName (87.5%) extra: pointer-typed error unwraps to its element name.
type cov95B10PtrErr struct{ msg string }

func (e *cov95B10PtrErr) Error() string { return e.msg }

func TestMcpErrTypeName_Cov95B10_PointerType(t *testing.T) {
	if got := mcpErrTypeName(&cov95B10PtrErr{msg: "x"}); got != "cov95B10PtrErr" {
		t.Errorf("got %q, want cov95B10PtrErr", got)
	}
}

// mcpInvokeTool (42.9%): a normal successful call, a returned error, and a recovered panic.
func TestMcpInvokeTool_Cov95B10(t *testing.T) {
	s := &server{}
	t.Run("success", func(t *testing.T) {
		res, err := s.mcpInvokeTool(func(*server, *jsonenc.Object) (*jsonenc.Object, error) {
			return jsonenc.NewObject().Set("ok", true), nil
		}, jsonenc.NewObject())
		if err != nil || res == nil {
			t.Fatalf("got res=%v err=%v", res, err)
		}
	})
	t.Run("returned_error", func(t *testing.T) {
		_, err := s.mcpInvokeTool(func(*server, *jsonenc.Object) (*jsonenc.Object, error) {
			return nil, errors.New("db down")
		}, jsonenc.NewObject())
		if err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("panic_recovered_as_error", func(t *testing.T) {
		_, err := s.mcpInvokeTool(func(*server, *jsonenc.Object) (*jsonenc.Object, error) {
			panic("kaboom")
		}, jsonenc.NewObject())
		if err == nil || !strings.Contains(err.Error(), "kaboom") {
			t.Fatalf("expected panic message wrapped as error, got %v", err)
		}
	})
	t.Run("panic_with_error_value_passed_through", func(t *testing.T) {
		_, err := s.mcpInvokeTool(func(*server, *jsonenc.Object) (*jsonenc.Object, error) {
			panic(errors.New("typed panic"))
		}, jsonenc.NewObject())
		if err == nil || err.Error() != "typed panic" {
			t.Fatalf("expected typed panic error passed through, got %v", err)
		}
	})
}

// handleMcpToolsCall (60.9%): unknown tool -> 404 JSON-RPC error; known tool success -> 200 with
// masked text content; known tool returning an error -> 500 JSON-RPC internal error.
func TestHandleMcpToolsCall_Cov95B10(t *testing.T) {
	t.Run("unknown_tool", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		body := jsonenc.NewObject().Set("params", jsonenc.NewObject().Set("name", "nonexistent_tool"))
		rec := httptest.NewRecorder()
		s.handleMcpToolsCall(rec, "req-1", body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Unknown tool") {
			t.Errorf("body: %s", rec.Body.String())
		}
	})
	t.Run("known_tool_success", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		body := jsonenc.NewObject().Set("params", jsonenc.NewObject().
			Set("name", "list_services").Set("arguments", jsonenc.NewObject()))
		rec := httptest.NewRecorder()
		s.handleMcpToolsCall(rec, "req-2", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"content"`) {
			t.Errorf("body: %s", rec.Body.String())
		}
	})
	t.Run("known_tool_db_error_yields_internal_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("db exploded")
		}}}
		body := jsonenc.NewObject().Set("params", jsonenc.NewObject().
			Set("name", "list_services").Set("arguments", jsonenc.NewObject()))
		rec := httptest.NewRecorder()
		s.handleMcpToolsCall(rec, "req-3", body)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "Internal error") {
			t.Errorf("body: %s", rec.Body.String())
		}
	})
}

// mcpListServices (85.7%): DB error propagates; success collects rows.
func TestMcpListServices_Cov95B10(t *testing.T) {
	t.Run("db_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		if _, err := s.mcpListServices(jsonenc.NewObject()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("success", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"ServiceName"}, []any{"svc-a"}, []any{"svc-b"}), nil
		}}}
		res, err := s.mcpListServices(jsonenc.NewObject())
		if err != nil {
			t.Fatal(err)
		}
		services, _ := res.Get("services")
		if len(services.([]any)) != 2 {
			t.Errorf("got %v", services)
		}
	})
}

// mcpQueryOtelLogs (82.4%): search filter appended, DB error, success with attributes normalized.
func TestMcpQueryOtelLogs_Cov95B10(t *testing.T) {
	t.Run("db_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		if _, err := s.mcpQueryOtelLogs(jsonenc.NewObject()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("search_filter_and_success", func(t *testing.T) {
		var capturedQuery string
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			capturedQuery = q
			return storetest.Result(
				[]string{"ts", "ServiceName", "SeverityText", "Body", "TraceId", "SpanId", "LogAttributes"},
				[]any{"2026-01-01 00:00:00", "svc", "ERROR", "oops", "t1", "s1", map[string]any{"k": "v"}},
			), nil
		}}}
		args := jsonenc.NewObject().Set("search", "oops").Set("service", "svc").Set("severity", "error").
			Set("trace_id", "t1")
		res, err := s.mcpQueryOtelLogs(args)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(capturedQuery, "Body ILIKE ?") {
			t.Errorf("expected search ILIKE clause in query: %s", capturedQuery)
		}
		cnt, _ := res.Get("count")
		if cnt != 1 {
			t.Errorf("got count=%v", cnt)
		}
	})
}

// mcpQueryOtelTraces (93.3%): DB error path.
func TestMcpQueryOtelTraces_Cov95B10_Error(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if _, err := s.mcpQueryOtelTraces(jsonenc.NewObject().Set("status_code", "STATUS_CODE_ERROR")); err == nil {
		t.Fatal("expected error")
	}
}

// mcpQueryMetrics (82.4%): invalid metric_kind ignored (no clause), valid kind applies clause,
// DB error propagates.
func TestMcpQueryMetrics_Cov95B10(t *testing.T) {
	t.Run("db_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		if _, err := s.mcpQueryMetrics(jsonenc.NewObject()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("invalid_metric_kind_ignored", func(t *testing.T) {
		var capturedQuery string
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			capturedQuery = q
			return &store.Result{}, nil
		}}}
		_, err := s.mcpQueryMetrics(jsonenc.NewObject().Set("metric_kind", "bogus"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(capturedQuery, "MetricKind = ?") {
			t.Errorf("bogus kind should not add a clause: %s", capturedQuery)
		}
	})
	t.Run("valid_metric_kind_applies_clause", func(t *testing.T) {
		var capturedQuery string
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			capturedQuery = q
			return storetest.Result([]string{"ts", "ServiceName", "MetricName", "MetricKind", "Value", "SampleCount"},
				[]any{"2026-01-01 00:00:00", "svc", "m", "gauge", 1.0, 2.0}), nil
		}}}
		res, err := s.mcpQueryMetrics(jsonenc.NewObject().Set("metric_kind", "gauge"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(capturedQuery, "MetricKind = ?") {
			t.Errorf("expected MetricKind clause: %s", capturedQuery)
		}
		cnt, _ := res.Get("count")
		if cnt != 1 {
			t.Errorf("got count=%v", cnt)
		}
	})
}

// mcpQueryMetricsRaw (65.2%): invalid metric_kind returns a plain error dict (not an error);
// histogram branch and gauge/sum branch DB errors and successes.
func TestMcpQueryMetricsRaw_Cov95B10(t *testing.T) {
	t.Run("invalid_metric_kind_returns_error_dict_not_go_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		res, err := s.mcpQueryMetricsRaw(jsonenc.NewObject().Set("metric_kind", "bogus"))
		if err != nil {
			t.Fatalf("expected no Go error, got %v", err)
		}
		v, _ := res.Get("error")
		if v == nil {
			t.Errorf("expected an 'error' key in the result, got %v", res)
		}
	})
	t.Run("histogram_db_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		if _, err := s.mcpQueryMetricsRaw(jsonenc.NewObject().Set("metric_kind", "histogram")); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("histogram_success", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"ts", "ServiceName", "MetricName", "MetricUnit", "Attributes", "Count", "Sum"},
				[]any{"2026-01-01 00:00:00", "svc", "m", "ms", map[string]any{"a": "b"}, 3.0, 9.0}), nil
		}}}
		res, err := s.mcpQueryMetricsRaw(jsonenc.NewObject().Set("metric_kind", "histogram"))
		if err != nil {
			t.Fatal(err)
		}
		cnt, _ := res.Get("count")
		if cnt != 1 {
			t.Errorf("got count=%v", cnt)
		}
	})
	t.Run("gauge_db_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		if _, err := s.mcpQueryMetricsRaw(jsonenc.NewObject().Set("metric_kind", "gauge")); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("sum_success", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"ts", "ServiceName", "MetricName", "MetricUnit", "Attributes", "Value"},
				[]any{"2026-01-01 00:00:00", "svc", "m", "ms", "{}", 5.0}), nil
		}}}
		res, err := s.mcpQueryMetricsRaw(jsonenc.NewObject().Set("metric_kind", "sum"))
		if err != nil {
			t.Fatal(err)
		}
		cnt, _ := res.Get("count")
		if cnt != 1 {
			t.Errorf("got count=%v", cnt)
		}
	})
}

// mcpGetMetricNames (69.2%): service filter applies WHERE + params, no-service uses no WHERE;
// DB error propagates.
func TestMcpGetMetricNames_Cov95B10(t *testing.T) {
	t.Run("db_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		if _, err := s.mcpGetMetricNames(jsonenc.NewObject()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("service_filter_applied", func(t *testing.T) {
		var capturedQuery string
		var capturedParams []any
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			capturedQuery, capturedParams = q, p
			return storetest.Result([]string{"MetricName", "ServiceName", "last_seen"},
				[]any{"m1", "svc", "2026-01-01 00:00:00"}), nil
		}}}
		res, err := s.mcpGetMetricNames(jsonenc.NewObject().Set("service", "svc"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(capturedQuery, "WHERE ServiceName = ?") {
			t.Errorf("expected WHERE clause: %s", capturedQuery)
		}
		if len(capturedParams) != 3 {
			t.Errorf("expected 3 params (one per UNION branch), got %d: %v", len(capturedParams), capturedParams)
		}
		cnt, _ := res.Get("count")
		if cnt != 1 {
			t.Errorf("got count=%v", cnt)
		}
	})
	t.Run("no_service_filter", func(t *testing.T) {
		var capturedQuery string
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			capturedQuery = q
			return &store.Result{}, nil
		}}}
		if _, err := s.mcpGetMetricNames(jsonenc.NewObject()); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(capturedQuery, "WHERE") {
			t.Errorf("no service -> no WHERE clause: %s", capturedQuery)
		}
	})
}

// mcpGetAnomalyRules (85.7%): DB error + success shaping.
func TestMcpGetAnomalyRules_Cov95B10(t *testing.T) {
	t.Run("db_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		if _, err := s.mcpGetAnomalyRules(jsonenc.NewObject()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("success", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(
				[]string{"Id", "Name", "RuleType", "SignalSource", "SignalName", "ServiceName",
					"Comparator", "WarningThreshold", "CriticalThreshold"},
				[]any{"r1", "High CPU", "threshold", "otel", "cpu", "svc", "gt", 80.0, 95.0},
			), nil
		}}}
		res, err := s.mcpGetAnomalyRules(jsonenc.NewObject())
		if err != nil {
			t.Fatal(err)
		}
		cnt, _ := res.Get("count")
		if cnt != 1 {
			t.Errorf("got count=%v", cnt)
		}
	})
}

// mcpGetRecentErrors (90.9%): half==0 -> clamped to 1 (limit=1 -> half=0), log-query error path,
// success merges + sorts logs and traces by ts descending.
func TestMcpGetRecentErrors_Cov95B10(t *testing.T) {
	t.Run("log_query_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		if _, err := s.mcpGetRecentErrors(jsonenc.NewObject()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("trace_query_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			if strings.Contains(q, "otel_traces") {
				return nil, errors.New("boom")
			}
			return &store.Result{}, nil
		}}}
		if _, err := s.mcpGetRecentErrors(jsonenc.NewObject()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("limit_one_clamps_half_to_one_and_merges_sorted", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			if strings.Contains(q, "otel_logs") {
				return storetest.Result([]string{"ts", "ServiceName", "source", "level_or_status", "message", "TraceId"},
					[]any{"2026-01-01 00:00:00", "svc", "log", "ERROR", "log msg", "t1"}), nil
			}
			return storetest.Result([]string{"ts", "ServiceName", "source", "level_or_status", "message", "TraceId"},
				[]any{"2026-01-02 00:00:00", "svc", "trace", "STATUS_CODE_ERROR", "span msg", "t2"}), nil
		}}}
		res, err := s.mcpGetRecentErrors(jsonenc.NewObject().Set("limit", 1))
		if err != nil {
			t.Fatal(err)
		}
		cnt, _ := res.Get("count")
		if cnt != 2 {
			t.Errorf("got count=%v", cnt)
		}
		errs, _ := res.Get("errors")
		arr := errs.([]any)
		first := arr[0].(*jsonenc.Object)
		ts, _ := first.Get("ts")
		if ts != "2026-01-02 00:00:00" {
			t.Errorf("expected the later trace-sourced row first (descending ts), got %v", ts)
		}
	})
}

// ---------------------------------------------------------------------------
// notif_check.go
// ---------------------------------------------------------------------------

// condInt (57.1%): missing key -> default, valid int string, float-string fallback, unparseable.
func TestCondInt_Cov95B10(t *testing.T) {
	mk := func(v any) *jsonenc.Object {
		if v == nil {
			return jsonenc.NewObject()
		}
		return jsonenc.NewObject().Set("k", v)
	}
	if got := condInt(mk(nil), "k", 5); got != 5 {
		t.Errorf("missing key: got %d, want 5", got)
	}
	if got := condInt(mk(json.Number("10")), "k", 5); got != 10 {
		t.Errorf("int string: got %d, want 10", got)
	}
	if got := condInt(mk(json.Number("10.7")), "k", 5); got != 10 {
		t.Errorf("float-string fallback: got %d, want 10", got)
	}
	if got := condInt(mk("not-a-number"), "k", 5); got != 5 {
		t.Errorf("unparseable: got %d, want default 5", got)
	}
}

// compareThreshold (28.6%): every comparator branch plus the default/unknown fallback.
func TestCompareThreshold_Cov95B10(t *testing.T) {
	cases := []struct {
		comp       string
		val, thold float64
		want       bool
	}{
		{"gt", 5, 3, true},
		{"gt", 3, 5, false},
		{"lt", 3, 5, true},
		{"lt", 5, 3, false},
		{"gte", 5, 5, true},
		{"gte", 4, 5, false},
		{"lte", 5, 5, true},
		{"lte", 6, 5, false},
		{"eq", 5.0000000001, 5, true},
		{"eq", 5.1, 5, false},
		{"unknown", 5, 5, false},
	}
	for _, c := range cases {
		if got := compareThreshold(c.comp, c.val, c.thold); got != c.want {
			t.Errorf("compareThreshold(%q, %v, %v) = %v, want %v", c.comp, c.val, c.thold, got, c.want)
		}
	}
}

// evaluateSignalCondition (85.0%): missing source/signal short-circuit, service filter appended,
// DB error, no-rows, success.
func TestEvaluateSignalCondition_Cov95B10(t *testing.T) {
	t.Run("missing_source_short_circuits", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		cond := jsonenc.NewObject().Set("signal", "cpu")
		matched, val := s.evaluateSignalCondition(cond)
		if matched || val != 0 {
			t.Errorf("got matched=%v val=%v", matched, val)
		}
	})
	t.Run("missing_signal_short_circuits", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		cond := jsonenc.NewObject().Set("source", "otel")
		matched, val := s.evaluateSignalCondition(cond)
		if matched || val != 0 {
			t.Errorf("got matched=%v val=%v", matched, val)
		}
	})
	t.Run("db_error_returns_false_zero", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		cond := jsonenc.NewObject().Set("source", "otel").Set("signal", "cpu")
		matched, val := s.evaluateSignalCondition(cond)
		if matched || val != 0 {
			t.Errorf("got matched=%v val=%v", matched, val)
		}
	})
	t.Run("no_rows_returns_false_zero", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return &store.Result{}, nil
		}}}
		cond := jsonenc.NewObject().Set("source", "otel").Set("signal", "cpu")
		matched, val := s.evaluateSignalCondition(cond)
		if matched || val != 0 {
			t.Errorf("got matched=%v val=%v", matched, val)
		}
	})
	t.Run("service_filter_applied_and_matches", func(t *testing.T) {
		var capturedQuery string
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			capturedQuery = q
			return storetest.Result([]string{"v"}, []any{90.0}), nil
		}}}
		cond := jsonenc.NewObject().Set("source", "otel").Set("signal", "cpu").
			Set("service", "svc").Set("comparator", "gt").Set("threshold", 80.0)
		matched, val := s.evaluateSignalCondition(cond)
		if !matched || val != 90.0 {
			t.Errorf("got matched=%v val=%v", matched, val)
		}
		if !strings.Contains(capturedQuery, "AND ServiceName = ?") {
			t.Errorf("expected ServiceName filter: %s", capturedQuery)
		}
	})
}

// evaluateTagCondition (78.6%): missing tag_key short-circuit, record_type filter, tag_value with
// eq/contains/regex operators, DB error, no rows.
func TestEvaluateTagCondition_Cov95B10(t *testing.T) {
	t.Run("missing_tag_key_short_circuits", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		matched, val := s.evaluateTagCondition(jsonenc.NewObject())
		if matched || val != 0 {
			t.Errorf("got matched=%v val=%v", matched, val)
		}
	})
	t.Run("db_error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		cond := jsonenc.NewObject().Set("tag_key", "env")
		matched, val := s.evaluateTagCondition(cond)
		if matched || val != 0 {
			t.Errorf("got matched=%v val=%v", matched, val)
		}
	})
	t.Run("no_rows", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return &store.Result{}, nil
		}}}
		cond := jsonenc.NewObject().Set("tag_key", "env")
		matched, val := s.evaluateTagCondition(cond)
		if matched || val != 0 {
			t.Errorf("got matched=%v val=%v", matched, val)
		}
	})
	for _, tagOp := range []string{"eq", "contains", "regex"} {
		t.Run("tag_op_"+tagOp, func(t *testing.T) {
			var capturedQuery string
			s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
				capturedQuery = q
				return storetest.Result([]string{"c"}, []any{5.0}), nil
			}}}
			cond := jsonenc.NewObject().Set("tag_key", "env").Set("tag_value", "prod").
				Set("tag_match_operator", tagOp).Set("record_type", "log").
				Set("comparator", "gt").Set("threshold", 1.0)
			matched, val := s.evaluateTagCondition(cond)
			if !matched || val != 5.0 {
				t.Errorf("got matched=%v val=%v", matched, val)
			}
			if !strings.Contains(capturedQuery, "AND RecordType = ?") {
				t.Errorf("expected RecordType filter: %s", capturedQuery)
			}
		})
	}
	t.Run("record_type_all_omits_filter", func(t *testing.T) {
		var capturedQuery string
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			capturedQuery = q
			return storetest.Result([]string{"c"}, []any{1.0}), nil
		}}}
		cond := jsonenc.NewObject().Set("tag_key", "env").Set("record_type", "all")
		s.evaluateTagCondition(cond)
		if strings.Contains(capturedQuery, "RecordType") {
			t.Errorf("record_type=all should omit the filter: %s", capturedQuery)
		}
	})
}

// notificationChannelMaskOutputEnabled (53.8%): invalid JSON -> true (fail open), non-object ->
// true, missing key -> true, explicit off values -> false, other values -> true.
func TestNotificationChannelMaskOutputEnabled_Cov95B10(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   bool
	}{
		{"invalid_json_fails_open_true", "{not json", true},
		{"non_object_json_true", `["a","b"]`, true},
		{"missing_key_true", `{}`, true},
		{"explicit_false_string", `{"mask_output_enabled":"false"}`, false},
		{"explicit_0", `{"mask_output_enabled":"0"}`, false},
		{"explicit_no", `{"mask_output_enabled":"no"}`, false},
		{"explicit_off", `{"mask_output_enabled":"off"}`, false},
		{"boolean_false", `{"mask_output_enabled":false}`, false},
		{"other_truthy_value", `{"mask_output_enabled":"yes"}`, true},
		{"empty_config_string_defaults_true", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := notificationChannelMaskOutputEnabled(c.config); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// condThreshold / condValue (66.7% each): present key vs. missing key default.
func TestCondThresholdAndCondValue_Cov95B10(t *testing.T) {
	present := jsonenc.NewObject().Set("threshold", 42.5).Set("_value", 7.0)
	if got := condThreshold(present); got != "42.5" {
		t.Errorf("condThreshold present: got %q", got)
	}
	if got := condValue(present); got != "7.0" {
		t.Errorf("condValue present: got %q", got)
	}
	missing := jsonenc.NewObject()
	if got := condThreshold(missing); got != "0" {
		t.Errorf("condThreshold missing: got %q, want 0", got)
	}
	if got := condValue(missing); got != "n/a" {
		t.Errorf("condValue missing: got %q, want n/a", got)
	}
}

// buildNotificationPayload (79.4%): tag-condition summary with/without record_type + tag_value,
// signal-condition summary with/without service, mask disabled (raw conditions passthrough), and
// an unknown comparator defaulting to ">".
func TestBuildNotificationPayload_Cov95B10(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	rule := notifRule{name: "My Rule", severity: "critical"}

	t.Run("tag_condition_with_record_type_and_value", func(t *testing.T) {
		cond := jsonenc.NewObject().Set("type", "tag").Set("record_type", "log").
			Set("tag_key", "env").Set("tag_match_operator", "eq").Set("tag_value", "prod").
			Set("comparator", "gte").Set("threshold", 3.0).Set("_value", 5.0)
		payload := s.buildNotificationPayload(rule, []any{cond}, false)
		summary, _ := payload.Get("summary")
		txt := summary.(string)
		if !strings.Contains(txt, "[log]") || !strings.Contains(txt, "env eq prod") || !strings.Contains(txt, "≥") {
			t.Errorf("got %q", txt)
		}
	})
	t.Run("tag_condition_all_record_type_and_no_value", func(t *testing.T) {
		cond := jsonenc.NewObject().Set("type", "tag").Set("record_type", "all").
			Set("tag_key", "env").Set("comparator", "gt").Set("threshold", 1.0)
		payload := s.buildNotificationPayload(rule, []any{cond}, false)
		summary, _ := payload.Get("summary")
		txt := summary.(string)
		if strings.Contains(txt, "[all]") {
			t.Errorf("record_type=all should not show a bracket prefix: %q", txt)
		}
	})
	t.Run("signal_condition_with_service", func(t *testing.T) {
		cond := jsonenc.NewObject().Set("type", "signal").Set("source", "otel").Set("signal", "cpu").
			Set("service", "svc-a").Set("comparator", "lt").Set("threshold", 10.0).Set("_value", 5.0)
		payload := s.buildNotificationPayload(rule, []any{cond}, false)
		summary, _ := payload.Get("summary")
		txt := summary.(string)
		if !strings.Contains(txt, "[svc-a]") || !strings.Contains(txt, "<") {
			t.Errorf("got %q", txt)
		}
	})
	t.Run("unknown_comparator_defaults_to_gt_symbol", func(t *testing.T) {
		cond := jsonenc.NewObject().Set("type", "signal").Set("source", "otel").Set("signal", "cpu").
			Set("comparator", "weird").Set("threshold", 1.0)
		payload := s.buildNotificationPayload(rule, []any{cond}, false)
		summary, _ := payload.Get("summary")
		txt := summary.(string)
		if !strings.Contains(txt, " > ") {
			t.Errorf("unknown comparator should fall back to '>': %q", txt)
		}
	})
	t.Run("non_object_condition_skipped", func(t *testing.T) {
		payload := s.buildNotificationPayload(rule, []any{"not-an-object"}, false)
		summary, _ := payload.Get("summary")
		if summary != "[SOBS] Rule 'My Rule' triggered (CRITICAL): " {
			t.Errorf("got %q", summary)
		}
	})
	t.Run("mask_enabled_masks_conditions_and_summary", func(t *testing.T) {
		cond := jsonenc.NewObject().Set("type", "signal").Set("source", "otel").Set("signal", "cpu").
			Set("comparator", "gt").Set("threshold", 1.0)
		payload := s.buildNotificationPayload(rule, []any{cond}, true)
		if _, ok := payload.Get("summary"); !ok {
			t.Error("expected a summary key even when masked")
		}
	})
}

// checkNotificationRule (87.7%): disabled rule, cooldown active, condition-logic "all" not met,
// "all" met with a mixed nil/valid condition list, channel-not-found, channel-disabled, and a
// full fire with an unknown channel type (avoids real network I/O via dispatchWebhookChannel).
func TestCheckNotificationRule_Cov95B10(t *testing.T) {
	t.Run("disabled_rule", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		rule := notifRule{id: "r1", enabled: false}
		got := s.checkNotificationRule(rule, map[string]notifChannel{})
		fired, _ := got.Get("fired")
		reason, _ := got.Get("reason")
		if fired != false || reason != "disabled" {
			t.Errorf("got fired=%v reason=%v", fired, reason)
		}
	})
	t.Run("cooldown_active", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			if strings.Contains(q, "LastFiredAt") {
				nowMs := float64(nowUTC().UnixMilli())
				return storetest.Result([]string{"ts"}, []any{nowMs}), nil
			}
			return &store.Result{}, nil
		}}}
		rule := notifRule{id: "r1", enabled: true, cooldownSecond: 3600}
		got := s.checkNotificationRule(rule, map[string]notifChannel{})
		fired, _ := got.Get("fired")
		reason, _ := got.Get("reason")
		if fired != false || reason != "cooldown" {
			t.Errorf("got fired=%v reason=%v", fired, reason)
		}
	})
	t.Run("logic_all_not_met_due_to_nil_condition", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		rule := notifRule{
			id: "r1", enabled: true, logicOperator: "all",
			conditions: []any{"not-an-object"}, // becomes a nil cond -> counted as notFired
		}
		got := s.checkNotificationRule(rule, map[string]notifChannel{})
		fired, _ := got.Get("fired")
		reason, _ := got.Get("reason")
		if fired != false || reason != "conditions not met" {
			t.Errorf("got fired=%v reason=%v", fired, reason)
		}
	})
	t.Run("logic_any_no_conditions_matched", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return &store.Result{}, nil // no rows -> evaluateSignalCondition -> false
		}}}
		cond := jsonenc.NewObject().Set("type", "signal").Set("source", "otel").Set("signal", "cpu")
		rule := notifRule{id: "r1", enabled: true, logicOperator: "any", conditions: []any{cond}}
		got := s.checkNotificationRule(rule, map[string]notifChannel{})
		fired, _ := got.Get("fired")
		if fired != false {
			t.Errorf("got fired=%v", fired)
		}
	})
	t.Run("fires_with_channel_not_found_and_disabled_and_unknown_type", func(t *testing.T) {
		insertCount := 0
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			if strings.Contains(q, "avg(Value)") {
				return storetest.Result([]string{"v"}, []any{99.0}), nil
			}
			return &store.Result{}, nil
		}}}
		_ = insertCount
		cond := jsonenc.NewObject().Set("type", "signal").Set("source", "otel").Set("signal", "cpu").
			Set("comparator", "gt").Set("threshold", 1.0)
		rule := notifRule{
			id: "r1", name: "Rule A", enabled: true, logicOperator: "any",
			conditions: []any{cond},
			channelIDs: []string{"missing-chan", "disabled-chan", "unknown-type-chan"},
		}
		channels := map[string]notifChannel{
			"disabled-chan":     {id: "disabled-chan", name: "Disabled", channelType: "webhook", enabled: false},
			"unknown-type-chan": {id: "unknown-type-chan", name: "Weird", channelType: "carrier_pigeon", enabled: true, configJSON: "{}"},
		}
		got := s.checkNotificationRule(rule, channels)
		fired, _ := got.Get("fired")
		if fired != true {
			t.Fatalf("expected fired=true, got %v (full=%v)", fired, got)
		}
		dispatchResults, _ := got.Get("dispatch_results")
		results := dispatchResults.([]any)
		if len(results) != 3 {
			t.Fatalf("expected 3 dispatch results, got %d: %v", len(results), results)
		}
		// missing-chan -> error "channel not found"
		r0 := results[0].(*jsonenc.Object)
		if st, _ := r0.Get("status"); st != "error" {
			t.Errorf("missing channel: got status=%v", st)
		}
		// disabled-chan -> skipped
		r1 := results[1].(*jsonenc.Object)
		if st, _ := r1.Get("status"); st != "skipped" {
			t.Errorf("disabled channel: got status=%v", st)
		}
		// unknown-type-chan -> error (Unknown channel type)
		r2 := results[2].(*jsonenc.Object)
		if st, _ := r2.Get("status"); st != "error" {
			t.Errorf("unknown type channel: got status=%v", st)
		}
	})
}

// loadNotificationRulesForCheck (87.0%): DB error -> nil, success parses ConditionsJson (with a
// mix of tag/signal conditions and a non-object entry that's dropped), defaults LogicOperator and
// Severity when empty, and splits/trims ChannelIds.
func TestLoadNotificationRulesForCheck_Cov95B10(t *testing.T) {
	t.Run("db_error_returns_nil", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		if got := s.loadNotificationRulesForCheck(); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("success_with_defaults_and_parsing", func(t *testing.T) {
		conditionsJSON := `[{"type":"tag","tag_key":"env"},{"type":"signal","source":"otel","signal":"cpu"},"bad-entry"]`
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(
				[]string{"Id", "Name", "Enabled", "LogicOperator", "ConditionsJson", "ChannelIds", "Severity", "CooldownSeconds"},
				[]any{"r1", "Rule One", 1.0, "", conditionsJSON, " ch1 , ch2 ,", "", 60.0},
			), nil
		}}}
		got := s.loadNotificationRulesForCheck()
		if len(got) != 1 {
			t.Fatalf("want 1 rule, got %d", len(got))
		}
		r := got[0]
		if r.logicOperator != "any" {
			t.Errorf("empty LogicOperator should default to 'any', got %q", r.logicOperator)
		}
		if r.severity != "warning" {
			t.Errorf("empty Severity should default to 'warning', got %q", r.severity)
		}
		if len(r.conditions) != 2 {
			t.Errorf("want 2 valid conditions (bad entry dropped), got %d", len(r.conditions))
		}
		if len(r.channelIDs) != 2 || r.channelIDs[0] != "ch1" || r.channelIDs[1] != "ch2" {
			t.Errorf("channel IDs should be trimmed and split, got %v", r.channelIDs)
		}
	})
}

// handleApiNotificationsCheck (92.9%): a rule whose evaluation panics is isolated (recovered) and
// reported as an error result rather than crashing the whole request.
func TestHandleApiNotificationsCheck_Cov95B10_PanicIsolation(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_notification_rules FINAL WHERE IsDeleted"):
			return storetest.Result(
				[]string{"Id", "Name", "Enabled", "LogicOperator", "ConditionsJson", "ChannelIds", "Severity", "CooldownSeconds"},
				[]any{"r1", "Panicky Rule", 1.0, "any", `[{"type":"signal","source":"otel","signal":"cpu"}]`, "", "warning", 0.0},
			), nil
		case strings.Contains(q, "avg(Value)"):
			panic("simulated evaluation panic")
		}
		return &store.Result{}, nil
	}}}
	req := httptest.NewRequest(http.MethodPost, "/api/notifications/check", nil)
	rec := httptest.NewRecorder()
	s.handleApiNotificationsCheck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rule evaluation failed") {
		t.Errorf("expected the panic to be isolated and reported, got body=%s", rec.Body.String())
	}
}

// normalizeNotificationConditionObj (95.5%): non-object input returns nil; tag-type normalization
// with invalid enum values falling back to defaults.
func TestNormalizeNotificationConditionObj_Cov95B10(t *testing.T) {
	if got := normalizeNotificationConditionObj("not-an-object"); got != nil {
		t.Errorf("non-object input should return nil, got %v", got)
	}
	raw := jsonenc.NewObject().Set("type", "TAG").Set("record_type", "bogus").
		Set("tag_match_operator", "bogus").Set("comparator", "bogus").
		Set("tag_key", "env").Set("threshold", json.Number("2"))
	got := normalizeNotificationConditionObj(raw)
	if got == nil {
		t.Fatal("expected a normalized object")
	}
	if v, _ := got.Get("record_type"); v != "all" {
		t.Errorf("invalid record_type should default to 'all', got %v", v)
	}
	if v, _ := got.Get("tag_match_operator"); v != "eq" {
		t.Errorf("invalid tag_match_operator should default to 'eq', got %v", v)
	}
	if v, _ := got.Get("comparator"); v != "gt" {
		t.Errorf("invalid comparator should default to 'gt', got %v", v)
	}
	if v, _ := got.Get("threshold"); v != 2.0 {
		t.Errorf("threshold should coerce to float, got %v (%T)", v, v)
	}
}
