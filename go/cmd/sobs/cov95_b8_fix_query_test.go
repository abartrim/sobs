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

// cov95_b8_fix_query_test.go — batch 8 targeted coverage for cmd/sobs/fix_query.go: the
// records-carrying and field-typed named-query executors' skip/error branches, plus the LLM-backed
// generateNamedQueriesStats / generateChartSpecStats stats-capturing wrappers exercised end-to-end
// against a canned chat-completions mock (same pattern as handlers_tail_broadcast_test.go's
// TestCallLLMChatBroadcastsInternalGenAISpan).

// TestExecuteNamedQueriesWithRecordsSkipsAndErrors: a malformed entry (not an object) and one with
// an empty name/sql are skipped; a query error still yields an item, with all four "empty" fields
// consistently set (columns/rows/records all empty, error populated).
func TestExecuteNamedQueriesWithRecordsSkipsAndErrors(t *testing.T) {
	calls := 0
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		calls++
		if strings.Contains(query, "BADSQL") {
			return nil, errors.New("Code: 1. DB::Exception: nope")
		}
		return storetest.Result([]string{"a", "b"}, []any{1, "x"}), nil
	}}}
	named := []any{
		42, // not a *jsonenc.Object
		jsonenc.NewObject().Set("name", "").Set("sql", "SELECT 1"),
		jsonenc.NewObject().Set("name", "n").Set("sql", ""),
		jsonenc.NewObject().Set("name", "ok").Set("sql", "SELECT 1").Set("purpose", "p"),
		jsonenc.NewObject().Set("name", "bad").Set("sql", "BADSQL"),
	}

	results := s.executeNamedQueriesWithRecords(named, 5)
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2 (ok + bad; others skipped)", len(results))
	}
	if calls != 2 {
		t.Errorf("Execute calls = %d, want 2 (skipped entries never reach the DB)", calls)
	}

	okItem, _ := results[0].(*jsonenc.Object)
	if e, _ := okItem.Get("error"); e != "" {
		t.Errorf("ok item error = %v, want empty", e)
	}
	records, _ := okItem.Get("records")
	recArr, _ := records.([]any)
	if len(recArr) != 1 {
		t.Fatalf("ok item records len = %d, want 1", len(recArr))
	}
	rec, _ := recArr[0].(*jsonenc.Object)
	if v, ok := rec.Get("a"); !ok || v != 1 {
		t.Errorf(`record["a"] = %v (ok=%v), want 1`, v, ok)
	}

	badItem, _ := results[1].(*jsonenc.Object)
	errV, _ := badItem.Get("error")
	if errV == "" {
		t.Error("bad item should carry a non-empty error")
	}
	cols, _ := badItem.Get("columns")
	rows, _ := badItem.Get("rows")
	recs, _ := badItem.Get("records")
	if len(cols.([]any)) != 0 || len(rows.([]any)) != 0 || len(recs.([]any)) != 0 {
		t.Errorf("bad item should have empty columns/rows/records, got cols=%v rows=%v records=%v", cols, rows, recs)
	}
}

// TestExecuteNamedQueriesForQueryRunBranches: skip on empty name/sql, a SQL-validation failure
// (blocked table), a query execution error, and the success path capping rows at SOBS_QUERY_MAX_ROWS
// and inferring field types.
func TestExecuteNamedQueriesForQueryRunBranches(t *testing.T) {
	t.Setenv("SOBS_QUERY_MAX_ROWS", "1")
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		if strings.Contains(query, "EXECERR") {
			return nil, errors.New("boom")
		}
		return storetest.Result([]string{"a"}, []any{1}, []any{2}), nil
	}}}
	named := []any{
		jsonenc.NewObject().Set("name", "").Set("sql", "SELECT 1"),
		jsonenc.NewObject().Set("name", "n").Set("sql", ""),
		// A statement referencing a non-allowlisted table trips validateSQL's guard.
		jsonenc.NewObject().Set("name", "blocked").Set("sql", "SELECT * FROM some_random_table"),
		// otel_logs IS allowlisted, so this passes validateSQL and reaches Execute, which the fake
		// DB fails for any query mentioning "EXECERR" (a marker in the SQL text, not a table name).
		jsonenc.NewObject().Set("name", "err").Set("sql", "SELECT * FROM otel_logs /* EXECERR */"),
		jsonenc.NewObject().Set("name", "ok").Set("sql", "SELECT a FROM otel_logs"),
	}
	results := s.executeNamedQueriesForQueryRun(named)
	if len(results) != 3 {
		t.Fatalf("results len = %d, want 3 (blocked+err+ok; empty name/sql skipped)", len(results))
	}

	blocked, _ := results[0].(*jsonenc.Object)
	e, _ := blocked.Get("error")
	if !strings.Contains(e.(string), "SQL validation error") {
		t.Errorf("blocked item error = %v, want a SQL validation error", e)
	}

	errItem, _ := results[1].(*jsonenc.Object)
	e2, _ := errItem.Get("error")
	if !strings.Contains(e2.(string), "Query execution error") {
		t.Errorf("err item error = %v, want a Query execution error", e2)
	}

	okItem, _ := results[2].(*jsonenc.Object)
	rows, _ := okItem.Get("rows")
	if len(rows.([]any)) != 1 {
		t.Errorf("ok item rows len = %d, want 1 (capped by SOBS_QUERY_MAX_ROWS=1)", len(rows.([]any)))
	}
	ft, _ := okItem.Get("field_types")
	if len(ft.([]any)) == 0 {
		t.Error("ok item field_types should be populated")
	}
}

// llmTestServer builds a server with a live write queue (needed because a non-empty LLM reply
// makes callLLMChat call emitInternalGenAISpan, which writes through s.enqueueWrite -> s.wq; a nil
// wq panics) and an SSE broker (emitInternalGenAISpan also broadcasts to /tail).
func llmTestServer() *server {
	return &server{
		db:  &storetest.FakeDB{},
		sse: newSSEBroker(),
		wq:  &writeQueue{ch: make(chan *writeTask, 64), batchMax: 200, batchWaitMs: 20},
	}
}

// llmChatServer starts an httptest server answering the OpenAI-compatible /chat/completions shape
// with the given content string, and returns its base URL as the "endpoint".
func llmChatServer(t *testing.T, content string) string {
	t.Helper()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", "")
	quoted, _ := json.Marshal(content)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(quoted) +
			`}}],"usage":{"prompt_tokens":5,"completion_tokens":7}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestGenerateNamedQueriesStatsSuccess drives generateNamedQueriesStats end-to-end against a canned
// LLM reply containing a fenced JSON datasets plan, verifying the SQL-prefix filter (a non-SELECT/
// WITH statement is dropped) and the base-SQL-identity filter (a dataset identical to the base SQL
// is dropped), while a valid distinct SELECT survives. NOTE: the function only considers the first
// 3 dataset entries (`if i >= 3 { break }`), so this reply intentionally uses exactly 3 — the
// 4-entries-get-capped behavior is covered separately by TestGenerateNamedQueriesStatsCapsAtThreeDatasets.
func TestGenerateNamedQueriesStatsSuccess(t *testing.T) {
	reply := "```json\n" + `{"datasets":[
		{"name":"notselect","sql":"DELETE FROM x","purpose":"bad sql"},
		{"name":"samesql","sql":"SELECT 1 AS a","purpose":"same as base"},
		{"name":"good","sql":"SELECT 2 AS b","purpose":"aux dataset"}
	]}` + "\n```"
	endpoint := llmChatServer(t, reply)
	s := llmTestServer()
	datasets, stats := s.generateNamedQueriesStats(endpoint, "how many?", "SELECT 1 AS a", "", "")
	if len(datasets) != 1 {
		t.Fatalf("datasets len = %d, want 1 (only the 'good' entry survives filtering): %#v", len(datasets), datasets)
	}
	ds, _ := datasets[0].(*jsonenc.Object)
	if name, _ := ds.Get("name"); name != "good" {
		t.Errorf("surviving dataset name = %v, want good", name)
	}
	if stats.prompt != 5 || stats.completion != 7 {
		t.Errorf("stats = %+v, want prompt=5 completion=7", stats)
	}
}

// TestGenerateNamedQueriesStatsInvalidNameFiltered covers the name-regex rejection branch (an
// uppercase/symbol-containing name fails namedQueryNameRE) in isolation, within the 3-entry cap.
func TestGenerateNamedQueriesStatsInvalidNameFiltered(t *testing.T) {
	reply := `{"datasets":[{"name":"Invalid-Name!","sql":"SELECT 1","purpose":"bad name"}]}`
	endpoint := llmChatServer(t, reply)
	s := llmTestServer()
	datasets, _ := s.generateNamedQueriesStats(endpoint, "q", "SELECT 0", "", "")
	if len(datasets) != 0 {
		t.Errorf("datasets = %#v, want empty (invalid name filtered)", datasets)
	}
}

// TestGenerateNamedQueriesStatsEmptyAndBadJSON covers the early-return branches: empty LLM content,
// and content that isn't valid/expected JSON (missing braces / not an object / datasets not an
// array) all yield an empty dataset list without panicking.
func TestGenerateNamedQueriesStatsEmptyAndBadJSON(t *testing.T) {
	s := llmTestServer()

	endpoint := llmChatServer(t, "")
	ds, _ := s.generateNamedQueriesStats(endpoint, "q", "SELECT 1", "", "")
	if len(ds) != 0 {
		t.Errorf("empty content: datasets = %v, want empty", ds)
	}

	endpoint2 := llmChatServer(t, "no braces here at all")
	ds2, _ := s.generateNamedQueriesStats(endpoint2, "q", "SELECT 1", "", "")
	if len(ds2) != 0 {
		t.Errorf("no-JSON content: datasets = %v, want empty", ds2)
	}

	endpoint3 := llmChatServer(t, `["not", "an", "object"]`)
	ds3, _ := s.generateNamedQueriesStats(endpoint3, "q", "SELECT 1", "", "")
	if len(ds3) != 0 {
		t.Errorf("array-not-object content: datasets = %v, want empty", ds3)
	}

	endpoint4 := llmChatServer(t, `{"datasets":"not an array"}`)
	ds4, _ := s.generateNamedQueriesStats(endpoint4, "q", "SELECT 1", "", "")
	if len(ds4) != 0 {
		t.Errorf("datasets-not-array content: datasets = %v, want empty", ds4)
	}
}

// TestGenerateNamedQueriesStatsCapsAtThreeDatasets: only the first 3 dataset entries are
// considered, even when the LLM returns more.
func TestGenerateNamedQueriesStatsCapsAtThreeDatasets(t *testing.T) {
	reply := `{"datasets":[
		{"name":"d1","sql":"SELECT 1","purpose":"p1"},
		{"name":"d2","sql":"SELECT 2","purpose":"p2"},
		{"name":"d3","sql":"SELECT 3","purpose":"p3"},
		{"name":"d4","sql":"SELECT 4","purpose":"p4"}
	]}`
	endpoint := llmChatServer(t, reply)
	s := llmTestServer()
	ds, _ := s.generateNamedQueriesStats(endpoint, "q", "SELECT 0", "bar", "make it a bar chart")
	if len(ds) != 3 {
		t.Fatalf("datasets len = %d, want 3 (capped), got %#v", len(ds), ds)
	}
}

// TestGenerateChartSpecStatsSuccess drives the happy path (valid chart JSON on the first reply, no
// repair needed) including the named-dataset condensation branch.
func TestGenerateChartSpecStatsSuccess(t *testing.T) {
	endpoint := llmChatServer(t, `{"xAxis":{},"series":[{"type":"line"}]}`)
	s := llmTestServer()
	named := []any{jsonenc.NewObject().Set("name", "aux").Set("purpose", "p").
		Set("columns", []any{"a"}).Set("rows", []any{[]any{1}})}
	spec, chartErr, stats := s.generateChartSpecStats(endpoint, "chart it", []any{"a"}, []any{
		jsonenc.NewObject().Set("a", 1),
	}, named, "bar", "make it pretty")
	if chartErr != "" {
		t.Fatalf("unexpected chartErr: %q", chartErr)
	}
	if !strings.Contains(spec, "series") {
		t.Errorf("spec = %q, want it to contain series", spec)
	}
	if stats.prompt != 5 {
		t.Errorf("stats.prompt = %d, want 5", stats.prompt)
	}
}

// TestGenerateChartSpecStatsEmptyReply: an empty LLM reply short-circuits with the "did not return a
// chart spec" error.
func TestGenerateChartSpecStatsEmptyReply(t *testing.T) {
	endpoint := llmChatServer(t, "")
	s := llmTestServer()
	spec, chartErr, _ := s.generateChartSpecStats(endpoint, "q", []any{"a"}, []any{}, nil, "", "")
	if spec != "" || chartErr != "LLM did not return a chart spec." {
		t.Errorf("spec=%q chartErr=%q, want empty spec + the did-not-return message", spec, chartErr)
	}
}

// TestGenerateChartSpecStatsEmptyObject: a syntactically valid but EMPTY chart spec object
// ("{}") is treated as an error, not a success.
func TestGenerateChartSpecStatsEmptyObject(t *testing.T) {
	endpoint := llmChatServer(t, "{}")
	s := llmTestServer()
	spec, chartErr, _ := s.generateChartSpecStats(endpoint, "q", []any{"a"}, []any{}, nil, "", "")
	if spec != "" || chartErr != "LLM returned an empty chart spec object." {
		t.Errorf("spec=%q chartErr=%q, want the empty-object message", spec, chartErr)
	}
}

// queryRunLLMStats: sums the two stages into totals and keys both stage names.
func TestQueryRunLLMStats(t *testing.T) {
	out := queryRunLLMStats(llmStats{prompt: 10, completion: 20, thinking: 1}, llmStats{prompt: 5, completion: 6, thinking: 0})
	totals, _ := out.Get("totals")
	to, _ := totals.(*jsonenc.Object)
	pt, _ := to.Get("prompt_tokens")
	if pt != 15 {
		t.Errorf("totals.prompt_tokens = %v, want 15", pt)
	}
	if _, ok := out.Get("named_query_generation"); !ok {
		t.Error("missing named_query_generation key")
	}
	if _, ok := out.Get("chart_generation"); !ok {
		t.Error("missing chart_generation key")
	}
}
