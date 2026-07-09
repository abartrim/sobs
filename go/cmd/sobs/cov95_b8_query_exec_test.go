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

// cov95_b8_query_exec_test.go — batch 8 targeted coverage for cmd/sobs/query_exec.go: the cell-type
// inference/formatting helpers' edge branches (unparseable dates, non-standard cell shapes) and the
// two simple query-execution handlers (handleApiDashboardsQuery / handleApiDashboardsSpecDryRun).

// flaskHTTPDate: empty/whitespace input returns ""; an unparseable string returns ""; a date-only
// value hits the date-only fallback branch; a full datetime with fractional seconds hits the
// primary layout branch.
func TestFlaskHTTPDate(t *testing.T) {
	if got := flaskHTTPDate("   "); got != "" {
		t.Errorf("flaskHTTPDate(blank) = %q, want empty", got)
	}
	if got := flaskHTTPDate("not-a-date-at-all"); got != "" {
		t.Errorf("flaskHTTPDate(garbage) = %q, want empty", got)
	}
	if got := flaskHTTPDate("2026-03-29"); got != "Sun, 29 Mar 2026 00:00:00 GMT" {
		t.Errorf("flaskHTTPDate(date-only) = %q", got)
	}
	if got := flaskHTTPDate("2026-03-29 12:00:00.123456"); got != "Sun, 29 Mar 2026 12:00:00 GMT" {
		t.Errorf("flaskHTTPDate(full) = %q", got)
	}
}

// pythonTypeName: the json.Number int-vs-float disambiguation (a Number containing "." or an
// exponent char is "float", else "int"), plus bool/string/default fallthrough.
func TestPythonTypeName(t *testing.T) {
	if got := pythonTypeName(json.Number("42")); got != "int" {
		t.Errorf("pythonTypeName(Number 42) = %q, want int", got)
	}
	if got := pythonTypeName(json.Number("3.14")); got != "float" {
		t.Errorf("pythonTypeName(Number 3.14) = %q, want float", got)
	}
	if got := pythonTypeName(json.Number("1e10")); got != "float" {
		t.Errorf("pythonTypeName(Number 1e10) = %q, want float", got)
	}
	if got := pythonTypeName(2.5); got != "float" {
		t.Errorf("pythonTypeName(float64) = %q, want float", got)
	}
	if got := pythonTypeName(true); got != "bool" {
		t.Errorf("pythonTypeName(bool) = %q, want bool", got)
	}
	if got := pythonTypeName("hi"); got != "str" {
		t.Errorf("pythonTypeName(string) = %q, want str", got)
	}
	if got := pythonTypeName(nil); got != "str" {
		t.Errorf("pythonTypeName(nil, default branch) = %q, want str", got)
	}
}

// inferColumnTypes: an all-null column reports "null"; a column whose first row is nil but a later
// row has a value still detects that later value's type (skip-nulls-until-first-value branch).
func TestInferColumnTypesSkipsLeadingNulls(t *testing.T) {
	columns := []any{"a", "b"}
	rows := []any{
		[]any{nil, nil},
		[]any{json.Number("5"), nil},
	}
	got := inferColumnTypes(columns, rows)
	if got[0] != "int" {
		t.Errorf("column a = %v, want int (detected from second row)", got[0])
	}
	if got[1] != "null" {
		t.Errorf("column b = %v, want null (never non-nil)", got[1])
	}
}

// inferColumnTypes: a malformed row (not a []any, or shorter than the column index) is skipped
// rather than panicking.
func TestInferColumnTypesSkipsMalformedRows(t *testing.T) {
	columns := []any{"a", "b"}
	rows := []any{
		"not a row",
		[]any{"only-one"},
		[]any{"x", "y"},
	}
	got := inferColumnTypes(columns, rows)
	if got[1] != "str" {
		t.Errorf("column b = %v, want str (detected from the well-formed row)", got[1])
	}
}

// chBaseType: nested Nullable(LowCardinality(...)) unwraps both layers.
func TestChBaseTypeNestedUnwrap(t *testing.T) {
	if got := chBaseType("Nullable(LowCardinality(String))"); got != "String" {
		t.Errorf("chBaseType(nested) = %q, want String", got)
	}
	if got := chBaseType("Int64"); got != "Int64" {
		t.Errorf("chBaseType(plain) = %q, want Int64", got)
	}
}

// chQueryValue: an Int64 arriving as a JSON string (64-bit overflow encoding) becomes a json.Number;
// a Float column with a bad string falls through unchanged; a DateTime column with an unparseable
// string value falls through unchanged (not wrapped in chDateTime); nil short-circuits.
func TestChQueryValueEdgeCases(t *testing.T) {
	if got := chQueryValue(nil, "Int64"); got != nil {
		t.Errorf("chQueryValue(nil) = %v, want nil", got)
	}
	if got := chQueryValue("123456789012345", "Int64"); got != json.Number("123456789012345") {
		t.Errorf("chQueryValue(int-as-string) = %#v, want json.Number", got)
	}
	if got := chQueryValue("not-a-float", "Float64"); got != "not-a-float" {
		t.Errorf("chQueryValue(bad float string) = %#v, want passthrough", got)
	}
	if got := chQueryValue("not-a-date", "DateTime"); got != "not-a-date" {
		t.Errorf("chQueryValue(bad datetime string) = %#v, want passthrough", got)
	}
	if got := chQueryValue(true, "String"); got != true {
		t.Errorf("chQueryValue(bool passthrough) = %#v, want true", got)
	}
	// A well-formed DateTime string DOES wrap into chDateTime.
	got := chQueryValue("2026-03-29 12:00:00", "DateTime")
	if _, ok := got.(chDateTime); !ok {
		t.Errorf("chQueryValue(valid datetime) = %#v, want chDateTime", got)
	}
}

// cellKind: the map/[]any "json" branch and the chDateTime "datetime" branch.
func TestCellKindJSONAndDatetime(t *testing.T) {
	if got := cellKind(map[string]any{"a": 1}); got != "json" {
		t.Errorf("cellKind(map) = %q, want json", got)
	}
	if got := cellKind([]any{1, 2}); got != "json" {
		t.Errorf("cellKind([]any) = %q, want json", got)
	}
	if got := cellKind(chDateTime{"x"}); got != "datetime" {
		t.Errorf("cellKind(chDateTime) = %q, want datetime", got)
	}
	if got := cellKind(json.Number("1.5")); got != "float" {
		t.Errorf("cellKind(Number 1.5) = %q, want float", got)
	}
}

// pandasKindFromDtype: every dtype-string branch (datetime/bool/int/uint/float/double), plus the
// object-dtype sample-refinement fallback for each cellKind.
func TestPandasKindFromDtype(t *testing.T) {
	cases := []struct {
		dtype  string
		sample any
		want   string
	}{
		{"datetime64[ns]", nil, "datetime"},
		{"bool", nil, "boolean"},
		{"boolean", nil, "boolean"},
		{"int64", nil, "integer"},
		{"uint32", nil, "integer"},
		{"float64", nil, "number"},
		{"double", nil, "number"},
		{"object", true, "boolean"},
		{"object", json.Number("1"), "integer"},
		{"object", json.Number("1.5"), "number"},
		{"object", map[string]any{}, "json"},
		{"object", "hi", "string"},
		{"object", nil, "string"},
	}
	for _, c := range cases {
		if got := pandasKindFromDtype(c.dtype, c.sample); got != c.want {
			t.Errorf("pandasKindFromDtype(%q, %#v) = %q, want %q", c.dtype, c.sample, got, c.want)
		}
	}
}

// pyStr2: absent/nil -> "", a string passes through unchanged (no strip), and a non-string value
// falls through to pyStr.
func TestPyStr2(t *testing.T) {
	if got := pyStr2(nil, false); got != "" {
		t.Errorf("pyStr2(absent) = %q, want empty", got)
	}
	if got := pyStr2(nil, true); got != "" {
		t.Errorf("pyStr2(present nil) = %q, want empty", got)
	}
	if got := pyStr2("  padded  ", true); got != "  padded  " {
		t.Errorf("pyStr2(string) = %q, want unstripped passthrough", got)
	}
	if got := pyStr2(true, true); got != "True" {
		t.Errorf("pyStr2(bool) = %q, want True (via pyStr)", got)
	}
}

// chartSampleRecords: the `limit` cap is on the input INDEX (i >= limit breaks), not on the count of
// well-formed records produced, so a malformed entry within the first `limit` rows still "consumes"
// its slot; and any malformed entry is skipped rather than producing a garbage record.
func TestChartSampleRecordsCapsAndSkipsMalformed(t *testing.T) {
	columns := []any{"a", "b"}
	rows := []any{
		[]any{1, 2},
		"not a row", // index 1: malformed, skipped, but still counts against the index cap
		[]any{3, 4}, // index 2: beyond limit=2, never reached
		[]any{5, 6},
	}
	got := chartSampleRecords(columns, rows, 2)
	if len(got) != 1 {
		t.Fatalf("chartSampleRecords len = %d, want 1 (only index 0 survives before the index-2 cap)", len(got))
	}

	// A limit large enough to reach past the malformed entry DOES pick up the later well-formed rows.
	got = chartSampleRecords(columns, rows, 4)
	if len(got) != 3 {
		t.Fatalf("chartSampleRecords(limit=4) len = %d, want 3 (index 1 malformed skipped, 0/2/3 kept)", len(got))
	}
}

// severityFor: nil -> INFO, non-nil -> ERROR.
func TestSeverityFor(t *testing.T) {
	if got := severityFor(nil); got != "INFO" {
		t.Errorf("severityFor(nil) = %q, want INFO", got)
	}
	if got := severityFor(errors.New("boom")); got != "ERROR" {
		t.Errorf("severityFor(err) = %q, want ERROR", got)
	}
}

// handleApiDashboardsQuery: an invalid query (empty, non-SELECT, denied keyword) short-circuits
// with 400 before touching the DB; a DB execution error surfaces the public error message; a
// successful query returns columns+rows.
func TestHandleApiDashboardsQuery(t *testing.T) {
	dbCalled := false
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		dbCalled = true
		return storetest.Result([]string{"a"}, []any{json.Number("1")}), nil
	}}}

	// Empty query.
	req := httptest.NewRequest(http.MethodPost, "/api/dashboards/query", strings.NewReader(`{"query":""}`))
	rec := httptest.NewRecorder()
	s.handleApiDashboardsQuery(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty query status = %d, want 400", rec.Code)
	}
	if dbCalled {
		t.Error("DB should not be called for an invalid query")
	}

	// DB execution error.
	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return nil, errors.New("Code: 62. DB::Exception: Syntax error")
	}}}
	req = httptest.NewRequest(http.MethodPost, "/api/dashboards/query", strings.NewReader(`{"query":"SELECT 1"}`))
	rec = httptest.NewRecorder()
	sErr.handleApiDashboardsQuery(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("db error status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Syntax error") {
		t.Errorf("db error body = %s, want public error message", rec.Body.String())
	}

	// Successful query.
	req = httptest.NewRequest(http.MethodPost, "/api/dashboards/query", strings.NewReader(`{"query":"SELECT 1 AS a"}`))
	rec = httptest.NewRecorder()
	s.handleApiDashboardsQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("success status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !dbCalled {
		t.Error("DB should have been called for a valid query")
	}
	if !strings.Contains(rec.Body.String(), `"columns"`) || !strings.Contains(rec.Body.String(), `"rows"`) {
		t.Errorf("success body missing columns/rows: %s", rec.Body.String())
	}
}

// handleApiDashboardsSpecDryRun: an invalid/empty spec body returns 400 from the compile stage; a
// query execution error after a valid compile returns 400 with the public error message.
func TestHandleApiDashboardsSpecDryRunErrors(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/dashboards/spec/dry-run", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.handleApiDashboardsSpecDryRun(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty spec status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}

	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return nil, errors.New("boom exec error")
	}}}
	body := `{"spec":{"template_id":"custom_echarts","sql":{"mode":"raw","override_sql":"SELECT 1"}}}`
	req = httptest.NewRequest(http.MethodPost, "/api/dashboards/spec/dry-run", strings.NewReader(body))
	rec = httptest.NewRecorder()
	sErr.handleApiDashboardsSpecDryRun(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("exec error status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "boom exec error") {
		t.Errorf("exec error body = %s, want public error message", rec.Body.String())
	}
}

// handleApiDashboardsSpecDryRun: the success path (compile+execute OK) also runs named_queries when
// present, exercising the named-query loop end to end through the real handler.
func TestHandleApiDashboardsSpecDryRunSuccessWithNamedQueries(t *testing.T) {
	calls := 0
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		calls++
		return storetest.Result([]string{"a"}, []any{json.Number("1")}), nil
	}}}
	body := `{"spec":{"template_id":"custom_echarts","sql":{"mode":"raw","override_sql":"SELECT 1 AS a"},` +
		`"named_queries":[{"name":"secondary","sql":"SELECT 2 AS a","purpose":"aux"}]}}`
	req := httptest.NewRequest(http.MethodPost, "/api/dashboards/spec/dry-run", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleApiDashboardsSpecDryRun(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if calls < 2 {
		t.Errorf("expected at least 2 Execute calls (primary + named query), got %d", calls)
	}
	if !strings.Contains(rec.Body.String(), "named_query_results") {
		t.Errorf("body missing named_query_results: %s", rec.Body.String())
	}
}

// executeNamedQueries: a malformed named-query entry (not a *jsonenc.Object) is skipped; one with
// an empty name or sql is skipped; a query execution error is captured per-item (not fatal to the
// whole batch).
func TestExecuteNamedQueriesSkipsAndCapturesErrors(t *testing.T) {
	calls := 0
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		calls++
		if strings.Contains(query, "BADSQL") {
			return nil, errors.New("Code: 1. DB::Exception: bad")
		}
		return storetest.Result([]string{"x"}, []any{json.Number("9")}), nil
	}}}
	named := []any{
		"not an object",
		jsonenc.NewObject().Set("name", "").Set("sql", "SELECT 1"),
		jsonenc.NewObject().Set("name", "n1").Set("sql", ""),
		jsonenc.NewObject().Set("name", "ok").Set("sql", "SELECT 1"),
		jsonenc.NewObject().Set("name", "err").Set("sql", "BADSQL"),
	}
	results := s.executeNamedQueries(named, 5)
	if len(results) != 2 {
		t.Fatalf("executeNamedQueries len = %d, want 2 (ok + err; others skipped)", len(results))
	}
	if calls != 2 {
		t.Errorf("Execute call count = %d, want 2 (skipped entries never reach the DB)", calls)
	}
	okItem, _ := results[0].(*jsonenc.Object)
	if e, _ := okItem.Get("error"); e != "" {
		t.Errorf("ok item error = %v, want empty", e)
	}
	errItem, _ := results[1].(*jsonenc.Object)
	if e, _ := errItem.Get("error"); e == "" {
		t.Error("err item should carry a non-empty error message")
	}
}
