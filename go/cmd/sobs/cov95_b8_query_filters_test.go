package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b8_query_filters_test.go — batch 8 targeted coverage for cmd/sobs/query_filters.go: limit/
// offset/sort clamping edge cases, the RE2-validation DB-error branch, the time-window bad-value
// paths, and the attr/placeholder helpers.

func reqWithQuery(t *testing.T, rawQuery string) *http.Request {
	t.Helper()
	return httptest.NewRequest(http.MethodGet, "/x?"+rawQuery, nil)
}

// parseLimit: non-numeric falls to default; over-5000 clamps down; below-1 clamps up; absent uses
// default.
func TestParseLimitClamping(t *testing.T) {
	if got := parseLimit(reqWithQuery(t, "limit=notanumber"), 50); got != 50 {
		t.Errorf("parseLimit(bad value) = %d, want default 50", got)
	}
	if got := parseLimit(reqWithQuery(t, "limit=999999"), 50); got != 5000 {
		t.Errorf("parseLimit(too big) = %d, want 5000", got)
	}
	if got := parseLimit(reqWithQuery(t, "limit=-5"), 50); got != 1 {
		t.Errorf("parseLimit(negative) = %d, want 1", got)
	}
	if got := parseLimit(reqWithQuery(t, ""), 50); got != 50 {
		t.Errorf("parseLimit(absent) = %d, want default 50", got)
	}
	if got := parseLimit(reqWithQuery(t, "limit=100"), 50); got != 100 {
		t.Errorf("parseLimit(100) = %d, want 100", got)
	}
}

// parseOffset: bad value and negative both fall to 0; a good value passes through.
func TestParseOffset(t *testing.T) {
	if got := parseOffset(reqWithQuery(t, "offset=nope")); got != 0 {
		t.Errorf("parseOffset(bad) = %d, want 0", got)
	}
	if got := parseOffset(reqWithQuery(t, "offset=-10")); got != 0 {
		t.Errorf("parseOffset(negative) = %d, want 0", got)
	}
	if got := parseOffset(reqWithQuery(t, "offset=25")); got != 25 {
		t.Errorf("parseOffset(25) = %d, want 25", got)
	}
	if got := parseOffset(reqWithQuery(t, "")); got != 0 {
		t.Errorf("parseOffset(absent) = %d, want 0", got)
	}
}

// parseSort: unknown sort_by falls back to defaultCol; invalid sort_dir falls back to desc; a valid
// pair passes through, including the asc branch.
func TestParseSort(t *testing.T) {
	allowed := map[string]string{"name": "Name", "ts": "Timestamp"}

	sortBy, sqlCol, dir := parseSort(reqWithQuery(t, "sort_by=bogus"), allowed, "ts")
	if sortBy != "ts" || sqlCol != "Timestamp" || dir != "desc" {
		t.Errorf("unknown sort_by: got (%q,%q,%q), want (ts,Timestamp,desc)", sortBy, sqlCol, dir)
	}

	sortBy, sqlCol, dir = parseSort(reqWithQuery(t, "sort_by=name&sort_dir=asc"), allowed, "ts")
	if sortBy != "name" || sqlCol != "Name" || dir != "asc" {
		t.Errorf("valid asc: got (%q,%q,%q), want (name,Name,asc)", sortBy, sqlCol, dir)
	}

	sortBy, sqlCol, dir = parseSort(reqWithQuery(t, "sort_by=name&sort_dir=sideways"), allowed, "ts")
	if dir != "desc" {
		t.Errorf("invalid sort_dir should fall back to desc, got %q", dir)
	}

	sortBy, sqlCol, dir = parseSort(reqWithQuery(t, ""), allowed, "ts")
	if sortBy != "ts" || sqlCol != "Timestamp" || dir != "desc" {
		t.Errorf("absent params: got (%q,%q,%q), want defaults", sortBy, sqlCol, dir)
	}
}

// orderClauseFor: asc vs. any-other-value (which is treated as desc).
func TestOrderClauseFor(t *testing.T) {
	if got := orderClauseFor("Name", "asc"); got != "ORDER BY Name ASC" {
		t.Errorf("orderClauseFor asc = %q", got)
	}
	if got := orderClauseFor("Name", "desc"); got != "ORDER BY Name DESC" {
		t.Errorf("orderClauseFor desc = %q", got)
	}
	if got := orderClauseFor("Name", ""); got != "ORDER BY Name DESC" {
		t.Errorf("orderClauseFor empty dir = %q, want DESC default", got)
	}
}

// parseISOTimestamp: a totally invalid string returns ok=false; a naive (no-offset) timestamp parses
// as UTC; a "Z"-suffixed timestamp is accepted via the Z->+00:00 rewrite.
func TestParseISOTimestampInvalidAndNaive(t *testing.T) {
	if _, ok := parseISOTimestamp("not-a-timestamp-at-all"); ok {
		t.Error("parseISOTimestamp(garbage) should fail to parse")
	}
	tm, ok := parseISOTimestamp("2026-03-29T12:00:00")
	if !ok {
		t.Fatal("parseISOTimestamp(naive) should parse")
	}
	if tm.Hour() != 12 {
		t.Errorf("naive timestamp hour = %d, want 12", tm.Hour())
	}
	if _, ok := parseISOTimestamp("2026-03-29T12:00:00Z"); !ok {
		t.Error("parseISOTimestamp(Z-suffixed) should parse")
	}
	if _, ok := parseISOTimestamp("2026-03-29"); !ok {
		t.Error("parseISOTimestamp(date-only) should parse")
	}
}

// normalizeChTimestamp: an unparseable value falls back to the "T"->" " passthrough (the "hope
// ClickHouse accepts it" branch), while a valid ISO timestamp round-trips through UTC formatting.
func TestNormalizeChTimestampFallback(t *testing.T) {
	got := normalizeChTimestamp("garbage-with-a-T-in-it")
	want := "garbage-with-a- -in-it"
	if got != want {
		t.Errorf("normalizeChTimestamp(garbage) = %q, want %q (T->space passthrough)", got, want)
	}
	got2 := normalizeChTimestamp("2026-03-29T12:00:00Z")
	if got2 != "2026-03-29 12:00:00.000000" {
		t.Errorf("normalizeChTimestamp(valid) = %q", got2)
	}
}

// normalizeChTimestamp: empty input falls to the now() branch (just assert it looks like a
// timestamp, since exact time isn't deterministic here).
func TestNormalizeChTimestampEmptyUsesNow(t *testing.T) {
	got := normalizeChTimestamp("")
	if len(got) != len("2006-01-02 15:04:05.000000") {
		t.Errorf("normalizeChTimestamp(\"\") = %q, want now()-formatted string of that length", got)
	}
}

// parseTimeWindowArgs: bad from_ts/to_ts values, a bad window_s, an inverted window, and the
// window_s-derives-to_ts branch.
func TestParseTimeWindowArgsErrors(t *testing.T) {
	// A bad from_ts ALONE (no to_ts/window_s) does not error here: normalizeChTimestamp's
	// unparseable-value fallback just passes the raw string through unchanged, and the final
	// from/to cross-validation block only runs when BOTH are non-empty. The bad value only
	// surfaces as an error once paired with a to_ts, so the final parseISOTimestamp calls run.
	from, to, errMsg := parseTimeWindowArgs(reqWithQuery(t, "from_ts=not-a-date"))
	if errMsg != "" || from == "" || to != "" {
		t.Errorf("parseTimeWindowArgs(bad from_ts alone) = (%q,%q,%q), want passthrough from, empty to, no error", from, to, errMsg)
	}
	_, _, errMsg = parseTimeWindowArgs(reqWithQuery(t, "from_ts=not-a-date&to_ts=2026-01-01T00:00:00Z"))
	if errMsg == "" {
		t.Error("parseTimeWindowArgs(bad from_ts paired with a valid to_ts) should error")
	}

	from, to, errMsg = parseTimeWindowArgs(reqWithQuery(t, "from_ts=2026-01-01T00:00:00Z&to_ts=2025-01-01T00:00:00Z"))
	if errMsg == "" || from != "" || to != "" {
		t.Errorf("parseTimeWindowArgs(inverted window) should error with empty from/to, got from=%q to=%q err=%q", from, to, errMsg)
	}

	_, _, errMsg = parseTimeWindowArgs(reqWithQuery(t, "from_ts=2026-01-01T00:00:00Z&window_s=notanumber"))
	if errMsg == "" {
		t.Error("parseTimeWindowArgs(bad window_s) should error")
	}

	from, to, errMsg = parseTimeWindowArgs(reqWithQuery(t, "from_ts=2026-01-01T00:00:00Z&window_s=60"))
	if errMsg != "" {
		t.Fatalf("parseTimeWindowArgs(from_ts+window_s) unexpected error: %q", errMsg)
	}
	if from == "" || to == "" {
		t.Errorf("parseTimeWindowArgs(from_ts+window_s): from=%q to=%q, want both set", from, to)
	}

	// window_s below 1 clamps to 1 second (still succeeds, to > from).
	_, _, errMsg = parseTimeWindowArgs(reqWithQuery(t, "from_ts=2026-01-01T00:00:00Z&window_s=0"))
	if errMsg != "" {
		t.Errorf("parseTimeWindowArgs(window_s=0 clamped) unexpected error: %q", errMsg)
	}

	// Neither from_ts nor to_ts set: no error, both empty.
	from, to, errMsg = parseTimeWindowArgs(reqWithQuery(t, ""))
	if errMsg != "" || from != "" || to != "" {
		t.Errorf("parseTimeWindowArgs(no params) = (%q,%q,%q), want all empty", from, to, errMsg)
	}
}

// appendRegexExpressionClauses: both include and exclude patterns append their own SQL fragment and
// parameter.
func TestAppendRegexExpressionClauses(t *testing.T) {
	var conds []string
	var params []any
	appendRegexExpressionClauses(&conds, &params, "Body", []string{"foo"}, []string{"bar"})
	if len(conds) != 2 || len(params) != 2 {
		t.Fatalf("conds=%v params=%v, want 2 of each", conds, params)
	}
	if conds[0] != "match(Body, ?)" {
		t.Errorf("include clause = %q", conds[0])
	}
	if conds[1] != "NOT match(Body, ?)" {
		t.Errorf("exclude clause = %q", conds[1])
	}
	if params[0] != "foo" || params[1] != "bar" {
		t.Errorf("params = %v", params)
	}
}

// placeholders: n<=0 returns empty string; n>0 returns the comma-joined "?" list.
func TestPlaceholders(t *testing.T) {
	if got := placeholders(0); got != "" {
		t.Errorf("placeholders(0) = %q, want empty", got)
	}
	if got := placeholders(-3); got != "" {
		t.Errorf("placeholders(-3) = %q, want empty", got)
	}
	if got := placeholders(1); got != "?" {
		t.Errorf("placeholders(1) = %q, want ?", got)
	}
	if got := placeholders(3); got != "?,?,?" {
		t.Errorf("placeholders(3) = %q, want ?,?,?", got)
	}
}

// attrGet: returns the first present key among several candidates, stringifying non-string values;
// a non-map input (or no keys present) yields "".
func TestAttrGet(t *testing.T) {
	m := map[string]any{"b": 42}
	if got := attrGet(m, "a", "b", "c"); got != "42" {
		t.Errorf("attrGet fallback to second key = %q, want 42", got)
	}
	if got := attrGet(m, "missing"); got != "" {
		t.Errorf("attrGet(missing key) = %q, want empty", got)
	}
	if got := attrGet("not a map", "a"); got != "" {
		t.Errorf("attrGet(non-map) = %q, want empty", got)
	}
}

// splitRegexFilterExpressionTerms + parseRegexFilterExpression: empty input, a bare "!", escaped
// "&&" inside a term, and a genuinely invalid regex term.
func TestParseRegexFilterExpressionEdgeCases(t *testing.T) {
	inc, exc, errMsg := parseRegexFilterExpression("")
	if errMsg != "" || inc != nil || exc != nil {
		t.Errorf("parseRegexFilterExpression(\"\") = (%v,%v,%q), want all empty/nil", inc, exc, errMsg)
	}

	_, _, errMsg = parseRegexFilterExpression("!")
	if errMsg == "" {
		t.Error(`parseRegexFilterExpression("!") should error (empty pattern after !)`)
	}

	_, _, errMsg = parseRegexFilterExpression("foo && ")
	if errMsg == "" {
		t.Error("parseRegexFilterExpression trailing && with empty term should error")
	}

	// Escaped \&& inside a single term should NOT split, and should unescape back to literal &&.
	inc, exc, errMsg = parseRegexFilterExpression(`a\&&b`)
	if errMsg != "" {
		t.Fatalf("parseRegexFilterExpression(escaped &&) unexpected error: %q", errMsg)
	}
	if len(inc) != 1 || inc[0] != "a&&b" {
		t.Errorf("parseRegexFilterExpression(escaped &&): inc=%v, want [\"a&&b\"]", inc)
	}
	_ = exc

	// Unbalanced parens is invalid in Go's regexp too -> error surfaced.
	_, _, errMsg = parseRegexFilterExpression("(unbalanced")
	if errMsg == "" {
		t.Error("parseRegexFilterExpression(unbalanced paren) should error")
	}

	// A negated valid pattern lands in exclude.
	inc, exc, errMsg = parseRegexFilterExpression("!foo")
	if errMsg != "" || len(exc) != 1 || exc[0] != "foo" || len(inc) != 0 {
		t.Errorf("parseRegexFilterExpression(!foo) = inc=%v exc=%v err=%q", inc, exc, errMsg)
	}
}

// validateRe2Pattern: empty pattern short-circuits to "" (no DB call); a DB error is surfaced as a
// "Regex error: ..." message with the ": while executing function" tail stripped; a successful
// match() call returns "".
func TestValidateRe2Pattern(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	if got := s.validateRe2Pattern("   "); got != "" {
		t.Errorf("validateRe2Pattern(blank) = %q, want empty (no DB call)", got)
	}

	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return nil, errors.New("Code: 427. DB::Exception: bad regex: while executing function match")
	}}}
	got := sErr.validateRe2Pattern("(bad")
	want := "Regex error: Code: 427. DB::Exception: bad regex"
	if got != want {
		t.Errorf("validateRe2Pattern(db error) = %q, want %q", got, want)
	}

	sOK := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	if got := sOK.validateRe2Pattern("good"); got != "" {
		t.Errorf("validateRe2Pattern(ok) = %q, want empty", got)
	}
}

// validateRe2Patterns: returns the first failing pattern's message, short-circuiting the rest.
func TestValidateRe2Patterns(t *testing.T) {
	calls := 0
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		calls++
		if params[0] == "bad" {
			return nil, errors.New("boom")
		}
		return &store.Result{}, nil
	}}}
	got := s.validateRe2Patterns([]string{"good1", "bad", "good2"})
	if got != "Regex error: boom" {
		t.Errorf("validateRe2Patterns = %q, want Regex error: boom", got)
	}
	if calls != 2 {
		t.Errorf("validateRe2Patterns should short-circuit after the failing pattern, got %d calls", calls)
	}

	sAllGood := &server{db: &storetest.FakeDB{}}
	if got := sAllGood.validateRe2Patterns([]string{"a", "b"}); got != "" {
		t.Errorf("validateRe2Patterns(all good) = %q, want empty", got)
	}
}

// prepareRe2FilterPatterns: a parse-stage error short-circuits before any DB call; a valid
// expression that RE2-fails surfaces the RE2 error; a fully valid expression returns the split
// include/exclude lists.
func TestPrepareRe2FilterPatterns(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		t.Fatal("DB should not be called when the parse stage already failed")
		return nil, nil
	}}}
	_, _, errMsg := s.prepareRe2FilterPatterns("!")
	if errMsg == "" {
		t.Error("prepareRe2FilterPatterns(parse error) should error without calling the DB")
	}

	sRe2Fail := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return nil, errors.New("re2 rejects this")
	}}}
	_, _, errMsg = sRe2Fail.prepareRe2FilterPatterns("foo")
	if errMsg == "" {
		t.Error("prepareRe2FilterPatterns(re2 failure) should surface the RE2 error")
	}

	sOK := &server{db: &storetest.FakeDB{}}
	inc, exc, errMsg := sOK.prepareRe2FilterPatterns("foo && !bar")
	if errMsg != "" {
		t.Fatalf("prepareRe2FilterPatterns(valid) unexpected error: %q", errMsg)
	}
	if len(inc) != 1 || inc[0] != "foo" || len(exc) != 1 || exc[0] != "bar" {
		t.Errorf("prepareRe2FilterPatterns(valid) = inc=%v exc=%v", inc, exc)
	}
}
