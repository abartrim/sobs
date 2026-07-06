package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b11_fix_ai_build_test.go — batch 11 targeted coverage for cmd/sobs/fix_ai_build.go's
// DB/LLM-backed functions (the pure regex helpers repairTruncatedInClauseLiterals /
// autoRepairIncompleteCTESQL / inferCustomMappingFromOption already have dedicated coverage
// elsewhere): vannaRepairSQL's config-guard/error/success branches, vannaRunQuery's
// validation/execution-error/success+row-cap branches, validateAndExecuteVannaSQLWithRepair's
// EXPLAIN-preflight/auto-repair/LLM-repair/retry-exhaustion paths, and
// executeNamedQueriesValidatedAiBuild's skip/aggregate branches. Reuses the llmChatServer /
// llmTestServer helpers already defined in cov95_b8_fix_query_test.go.

// ---- vannaRepairSQL --------------------------------------------------------------------------

func TestVannaRepairSQL(t *testing.T) {
	t.Run("no endpoint/model configured -> error, no HTTP call", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		sql, errMsg, stats := s.vannaRepairSQL("", "q", "SELECT 1", "bad syntax", 1)
		if sql != "" || errMsg != "AI endpoint not configured." || stats != (llmStats{}) {
			t.Errorf("unexpected: sql=%q err=%q stats=%+v", sql, errMsg, stats)
		}
	})

	t.Run("LLM call error surfaces a repair-request-failed message", func(t *testing.T) {
		// A non-empty endpoint with SOBS_UPSTREAM_FIXTURES unset routes to the real client, which
		// will fail fast against an unroutable host.
		s := &server{db: storetest.SettingsDB(map[string]string{"ai.model": "m"})}
		sql, errMsg, _ := s.vannaRepairSQL("http://127.0.0.1:1/nope", "q", "SELECT 1", "bad", 1)
		if sql != "" || !strings.Contains(errMsg, "LLM repair request failed") {
			t.Errorf("unexpected: sql=%q err=%q", sql, errMsg)
		}
	})

	t.Run("empty LLM content -> did not return message", func(t *testing.T) {
		endpoint := llmChatServer(t, "")
		s := &server{db: storetest.SettingsDB(map[string]string{"ai.model": "m"}),
			wq: &writeQueue{ch: make(chan *writeTask, 64), batchMax: 200, batchWaitMs: 20}}
		sql, errMsg, _ := s.vannaRepairSQL(endpoint, "q", "SELECT 1", "bad", 1)
		if sql != "" || errMsg != "LLM did not return a repaired SQL statement." {
			t.Errorf("unexpected: sql=%q err=%q", sql, errMsg)
		}
	})

	t.Run("fenced-empty content -> empty repaired statement message", func(t *testing.T) {
		endpoint := llmChatServer(t, "```sql\n\n```")
		s := &server{db: storetest.SettingsDB(map[string]string{"ai.model": "m"}),
			wq: &writeQueue{ch: make(chan *writeTask, 64), batchMax: 200, batchWaitMs: 20}}
		sql, errMsg, _ := s.vannaRepairSQL(endpoint, "q", "SELECT 1", "bad", 1)
		if sql != "" || errMsg != "LLM returned an empty repaired SQL statement." {
			t.Errorf("unexpected: sql=%q err=%q", sql, errMsg)
		}
	})

	t.Run("success strips code fences and returns the repaired SQL + stats", func(t *testing.T) {
		endpoint := llmChatServer(t, "```sql\nSELECT 2 FROM otel_logs\n```")
		s := &server{db: storetest.SettingsDB(map[string]string{"ai.model": "m"}),
			wq: &writeQueue{ch: make(chan *writeTask, 64), batchMax: 200, batchWaitMs: 20}}
		sql, errMsg, stats := s.vannaRepairSQL(endpoint, "how many?", "SELECT 1", "syntax error", 2)
		if errMsg != "" || sql != "SELECT 2 FROM otel_logs" {
			t.Fatalf("unexpected: sql=%q err=%q", sql, errMsg)
		}
		if stats.prompt != 5 || stats.completion != 7 {
			t.Errorf("unexpected stats: %+v", stats)
		}
	})
}

// ---- vannaRunQuery ---------------------------------------------------------------------------

func TestVannaRunQuery(t *testing.T) {
	t.Run("validation error short-circuits before Execute", func(t *testing.T) {
		called := false
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			called = true
			return &store.Result{}, nil
		}}}
		cols, rows, errMsg := s.vannaRunQuery("SELECT * FROM some_random_disallowed_table")
		if cols != nil || rows != nil || !strings.Contains(errMsg, "SQL validation error") {
			t.Errorf("unexpected: cols=%v rows=%v err=%q", cols, rows, errMsg)
		}
		if called {
			t.Error("Execute should not have been called")
		}
	})

	t.Run("execution error surfaces the raw chdb message", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("chdb query: Code: 60. DB::Exception: Table doesn't exist")
		}}}
		cols, rows, errMsg := s.vannaRunQuery("SELECT * FROM otel_logs")
		if cols != nil || rows != nil {
			t.Errorf("want nil cols/rows on error, got %v %v", cols, rows)
		}
		if errMsg != "Query execution error: Code: 60. DB::Exception: Table doesn't exist" {
			t.Errorf("want the chdb-wrapper-stripped message, got %q", errMsg)
		}
	})

	t.Run("success caps rows at SOBS_QUERY_MAX_ROWS", func(t *testing.T) {
		t.Setenv("SOBS_QUERY_MAX_ROWS", "1")
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"a"}, []any{1}, []any{2}, []any{3}), nil
		}}}
		cols, rows, errMsg := s.vannaRunQuery("SELECT a FROM otel_logs")
		if errMsg != "" {
			t.Fatalf("unexpected error: %q", errMsg)
		}
		if len(cols) != 1 || len(rows) != 1 {
			t.Errorf("want 1 col + 1 row (capped), got cols=%v rows=%v", cols, rows)
		}
	})
}

// ---- validateAndExecuteVannaSQLWithRepair -----------------------------------------------------

func TestValidateAndExecuteVannaSQLWithRepair(t *testing.T) {
	t.Run("EXPLAIN succeeds and query succeeds on first attempt -> no retries", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			return storetest.Result([]string{"a"}, []any{1}), nil
		}}}
		sql, cols, rows, errMsg, retries, _ := s.validateAndExecuteVannaSQLWithRepair("", "q", "SELECT a FROM otel_logs")
		if errMsg != "" || retries != 0 || sql != "SELECT a FROM otel_logs" {
			t.Fatalf("unexpected: sql=%q err=%q retries=%d", sql, errMsg, retries)
		}
		if cols == nil || rows == nil {
			t.Error("want non-nil cols/rows on success")
		}
	})

	t.Run("EXPLAIN fails, auto-repair via CTE-fixup succeeds, then query succeeds", func(t *testing.T) {
		calls := 0
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			calls++
			if strings.HasPrefix(q, "EXPLAIN") {
				// The first EXPLAIN (of the truncated CTE) fails; the second (post auto-repair)
				// succeeds, matching a real chdb parse error on truncated SQL then success.
				if calls == 1 {
					return nil, errors.New("parse error: unexpected end of input")
				}
				return &store.Result{}, nil
			}
			return storetest.Result([]string{"x"}, []any{1}), nil
		}}}
		// Missing a final SELECT for a WITH-CTE -> autoRepairIncompleteCTESQL appends one.
		truncated := "WITH t AS (SELECT 1 FROM otel_logs"
		sql, _, _, errMsg, retries, _ := s.validateAndExecuteVannaSQLWithRepair("", "q", truncated)
		if errMsg != "" {
			t.Fatalf("unexpected error: %q", errMsg)
		}
		if retries != 1 {
			t.Errorf("want 1 retry (the auto-repair), got %d", retries)
		}
		if !strings.Contains(sql, "SELECT * FROM t") {
			t.Errorf("want the auto-repaired SQL, got %q", sql)
		}
	})

	t.Run("EXPLAIN fails and cannot be auto-repaired; no endpoint -> repair unavailable, exec still attempted and fails", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			if strings.HasPrefix(q, "EXPLAIN") {
				return nil, errors.New("syntax error near SELECT")
			}
			return nil, errors.New("syntax error near SELECT")
		}}}
		// A plain SELECT (not a WITH-CTE) can't be auto-repaired by autoRepairIncompleteCTESQL.
		sql, cols, rows, errMsg, _, _ := s.validateAndExecuteVannaSQLWithRepair("", "q", "SELECT FROM otel_logs")
		if cols != nil || rows != nil {
			t.Errorf("want nil cols/rows on failure, got %v %v", cols, rows)
		}
		if !strings.Contains(errMsg, "Query execution error") {
			t.Errorf("want a final execution error, got %q", errMsg)
		}
		_ = sql
	})

	t.Run("query fails all 3 attempts with no successful repair -> final error includes repair error", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		s := &server{
			db: storetest.SettingsDB(map[string]string{"ai.model": "m"}),
			wq: &writeQueue{ch: make(chan *writeTask, 64), batchMax: 200, batchWaitMs: 20},
		}
		// Swap in a DB whose EXPLAIN always succeeds (skip the preflight-repair branch) but whose
		// real Execute always fails, and whose ai.model setting comes from the settings map above
		// combined with the same FakeDB's ExecuteFunc for query execution.
		fdb := s.db.(*storetest.FakeDB)
		fdb.ExecuteFunc = func(q string, params ...any) (*store.Result, error) {
			if strings.HasPrefix(q, "EXPLAIN") {
				return &store.Result{}, nil
			}
			if strings.Contains(q, "sobs_ai_settings") {
				if len(params) == 1 && params[0] == "ai.model" {
					return storetest.Result([]string{"Value"}, []any{"m"}), nil
				}
				return &store.Result{}, nil
			}
			return nil, errors.New("Code: 1. DB::Exception: always fails")
		}
		endpoint := "http://sobs-ai.mock/repair-fails"
		// No fixture written for the repair LLM call -> upstreamFixture 404s -> callLLMChat errors
		// -> vannaRepairSQL returns a non-empty repair error each attempt -> retries exhausted.
		sql, cols, rows, errMsg, retries, _ := s.validateAndExecuteVannaSQLWithRepair(endpoint, "q", "SELECT a FROM otel_logs")
		if cols != nil || rows != nil {
			t.Errorf("want nil cols/rows, got %v %v", cols, rows)
		}
		if !strings.Contains(errMsg, "Query execution error") || !strings.Contains(errMsg, "SQL repair error") {
			t.Errorf("want a combined exec+repair error, got %q", errMsg)
		}
		if retries != 0 {
			t.Errorf("want 0 retries (repair never succeeds so currentSQL never changes), got %d", retries)
		}
		_ = sql
	})
}

// ---- executeNamedQueriesValidatedAiBuild -------------------------------------------------------

func TestExecuteNamedQueriesValidatedAiBuild(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"a"}, []any{1}), nil
	}}}
	named := []any{
		42, // not an object -> skipped
		jsonenc.NewObject().Set("name", "").Set("sql", "SELECT 1"), // empty name -> skipped
		jsonenc.NewObject().Set("name", "n").Set("sql", ""),        // empty sql -> skipped
		jsonenc.NewObject().Set("name", "ok").Set("sql", "SELECT a FROM otel_logs").Set("purpose", "p"),
	}
	results := s.executeNamedQueriesValidatedAiBuild("", "q", named)
	if len(results) != 1 {
		t.Fatalf("want 1 result (others skipped), got %d: %v", len(results), results)
	}
	item, _ := results[0].(*jsonenc.Object)
	name, _ := item.Get("name")
	errV, _ := item.Get("error")
	cols, _ := item.Get("columns")
	if name != "ok" || errV != "" {
		t.Fatalf("unexpected item: %v", item)
	}
	if colArr, ok := cols.([]any); !ok || len(colArr) != 1 {
		t.Errorf("want 1 column, got %v", cols)
	}
}

func TestExecuteNamedQueriesValidatedAiBuild_NilColsRowsBecomeEmpty(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	named := []any{jsonenc.NewObject().Set("name", "bad").Set("sql", "SELECT a FROM otel_logs")}
	results := s.executeNamedQueriesValidatedAiBuild("", "q", named)
	item, _ := results[0].(*jsonenc.Object)
	cols, _ := item.Get("columns")
	rows, _ := item.Get("rows")
	errV, _ := item.Get("error")
	if errV == "" {
		t.Error("want a non-empty error")
	}
	if colArr, ok := cols.([]any); !ok || len(colArr) != 0 {
		t.Errorf("want empty (non-nil) columns, got %v", cols)
	}
	if rowArr, ok := rows.([]any); !ok || len(rowArr) != 0 {
		t.Errorf("want empty (non-nil) rows, got %v", rows)
	}
}
