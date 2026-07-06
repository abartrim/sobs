package main

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Coverage batch 14: cmd/sobs/query_introspect.go's DB-introspection helpers. None of these had a
// dedicated test file yet; queryAllowedTables/queryAllowedTableSet are package-level vars computed
// once from buildQueryAllowedTables(), so we test buildQueryAllowedTables() directly (re-invoking
// the pure builder function rather than mutating the frozen package var) to cover its
// SOBS_QUERY_ALLOWED_TABLES env-extension branch, which the default (unset) test environment never
// exercises.

func TestBuildQueryAllowedTables_EnvExtension(t *testing.T) {
	t.Run("no_env_matches_builtin", func(t *testing.T) {
		t.Setenv("SOBS_QUERY_ALLOWED_TABLES", "")
		got := buildQueryAllowedTables()
		if len(got) == 0 {
			t.Fatal("expected the builtin allowlist to be non-empty")
		}
	})

	t.Run("env_adds_new_safe_ident_lowercased", func(t *testing.T) {
		t.Setenv("SOBS_QUERY_ALLOWED_TABLES", "My_Custom_Table, other_tbl")
		got := buildQueryAllowedTables()
		found := map[string]bool{}
		for _, v := range got {
			if s, ok := v.(string); ok {
				found[s] = true
			}
		}
		if !found["my_custom_table"] {
			t.Errorf("expected env-supplied table lowercased in allowlist, got %v", got)
		}
		if !found["other_tbl"] {
			t.Errorf("expected second env-supplied table in allowlist, got %v", got)
		}
		// Result must stay sorted.
		names := make([]string, len(got))
		for i, v := range got {
			names[i], _ = v.(string)
		}
		if !sort.StringsAreSorted(names) {
			t.Errorf("expected sorted allowlist, got %v", names)
		}
	})

	t.Run("env_rejects_unsafe_identifiers", func(t *testing.T) {
		t.Setenv("SOBS_QUERY_ALLOWED_TABLES", "bad-name; DROP TABLE x,   , 9startsnumeric")
		got := buildQueryAllowedTables()
		for _, v := range got {
			s, _ := v.(string)
			if s == "bad-name; drop table x" || s == "9startsnumeric" {
				t.Errorf("unsafe identifier leaked into allowlist: %v", got)
			}
		}
	})

	t.Run("env_dedupes_against_builtin", func(t *testing.T) {
		// otel_logs is a builtin table; re-adding it via env must not duplicate it.
		t.Setenv("SOBS_QUERY_ALLOWED_TABLES", "otel_logs")
		got := buildQueryAllowedTables()
		count := 0
		for _, v := range got {
			if v == "otel_logs" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected exactly 1 occurrence of otel_logs, got %d", count)
		}
	})
}

func TestQueryTableNames(t *testing.T) {
	t.Run("success_orders_by_name", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if !strings.Contains(q, "system.tables") {
				t.Fatalf("unexpected query: %q", q)
			}
			if len(p) != 1 || p[0] != "default" {
				t.Fatalf("unexpected params: %v", p)
			}
			return storetest.Result([]string{"name"}, []any{"otel_logs"}, []any{"otel_traces"}), nil
		}}}
		got, err := s.queryTableNames("default")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 2 || got[0] != "otel_logs" || got[1] != "otel_traces" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("db_error_propagates", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		_, err := s.queryTableNames("default")
		if err == nil {
			t.Fatal("expected error to propagate")
		}
	})
}

func TestDescribeTableExtended(t *testing.T) {
	t.Run("success_maps_columns", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if !strings.Contains(q, "system.columns") {
				t.Fatalf("unexpected query: %q", q)
			}
			return storetest.Result(
				[]string{"name", "type", "default_kind", "comment", "is_in_primary_key", "is_in_sorting_key", "is_in_partition_key"},
				[]any{"id", "Nullable(String)", "", "an id", float64(1), float64(0), float64(0)},
			), nil
		}}}
		cols, err := s.describeTableExtended("otel_logs")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(cols) != 1 {
			t.Fatalf("expected 1 column, got %v", cols)
		}
	})

	t.Run("db_error_propagates", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		_, err := s.describeTableExtended("otel_logs")
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestAllowedTablesInfo(t *testing.T) {
	t.Run("queryTableNames_error_propagates", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		_, err := s.allowedTablesInfo()
		if err == nil {
			t.Fatal("expected error from queryTableNames failure")
		}
	})

	t.Run("describeTableExtended_error_propagates", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if strings.Contains(q, "system.tables") {
				return storetest.Result([]string{"name"}, []any{"otel_logs"}), nil
			}
			return nil, errors.New("describe boom")
		}}}
		_, err := s.allowedTablesInfo()
		if err == nil {
			t.Fatal("expected error from describeTableExtended failure")
		}
	})

	t.Run("only_existing_allowlisted_tables_returned", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if strings.Contains(q, "system.tables") {
				// otel_logs is allowlisted+exists; "not_a_real_table" exists but isn't allowlisted.
				return storetest.Result([]string{"name"}, []any{"otel_logs"}, []any{"not_a_real_table"}), nil
			}
			return storetest.Result([]string{"name", "type", "default_kind", "comment",
				"is_in_primary_key", "is_in_sorting_key", "is_in_partition_key"},
				[]any{"Timestamp", "DateTime64(9)", "", "", float64(0), float64(1), float64(0)}), nil
		}}}
		got, err := s.allowedTablesInfo()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected exactly 1 allowlisted+existing table, got %v", got)
		}
	})
}

func TestCompactSchemaLine(t *testing.T) {
	t.Run("db_error_yields_error_placeholder", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		got := s.compactSchemaLine("otel_logs")
		if !strings.Contains(got, "describe_error:boom") {
			t.Errorf("got %q", got)
		}
	})

	t.Run("blank_column_names_skipped", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return storetest.Result([]string{"name", "type", "default_kind", "comment"},
				[]any{"", "String", "", ""},
				[]any{"ServiceName", "LowCardinality(String)", "", ""},
			), nil
		}}}
		got := s.compactSchemaLine("otel_logs")
		if strings.Contains(got, "(:") {
			t.Errorf("blank column should have been skipped, got %q", got)
		}
		if !strings.Contains(got, "ServiceName:String[dim]") {
			t.Errorf("expected compacted LowCardinality + dim tag, got %q", got)
		}
	})
}

func TestGetSchemaContext_NoTables(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		return &store.Result{}, nil // system.tables empty -> no allowlisted tables found
	}}}
	got := s.getSchemaContext()
	if !strings.Contains(got, "(no tables found)") {
		t.Errorf("got %q", got)
	}
}

func TestGetSchemaContext_WithTables(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if strings.Contains(q, "system.tables") {
			return storetest.Result([]string{"name"}, []any{"otel_logs"}), nil
		}
		return storetest.Result([]string{"name", "type", "default_kind", "comment"},
			[]any{"Timestamp", "DateTime64(9)", "", ""}), nil
	}}}
	got := s.getSchemaContext()
	if !strings.HasPrefix(got, "Database: default") {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(got, "otel_logs(Timestamp:DateTime64[ts])") {
		t.Errorf("expected the schema line for otel_logs, got %q", got)
	}
	if !strings.Contains(got, "Signal terminology:") {
		t.Errorf("expected the static block appended, got %q", got)
	}
}

func TestGetTableDDL(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if !strings.Contains(q, "SHOW CREATE TABLE") {
				t.Fatalf("unexpected query: %q", q)
			}
			return storetest.Result([]string{"statement"}, []any{"CREATE TABLE otel_logs (...)"}), nil
		}}}
		got := s.getTableDDL("otel_logs")
		if got != "CREATE TABLE otel_logs (...)" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("db_error_yields_empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		if got := s.getTableDDL("otel_logs"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("empty_result_yields_empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return &store.Result{}, nil
		}}}
		if got := s.getTableDDL("otel_logs"); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestGetTableSample(t *testing.T) {
	t.Run("not_allowlisted_yields_empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			t.Fatal("Execute should not be called for a non-allowlisted table")
			return nil, nil
		}}}
		got := s.getTableSample("not_allowlisted_xyz", 5)
		cols, _ := got.Get("columns")
		if arr, _ := cols.([]any); len(arr) != 0 {
			t.Errorf("expected empty columns, got %v", cols)
		}
	})

	t.Run("db_error_yields_empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		got := s.getTableSample("otel_logs", 5)
		rows, _ := got.Get("rows")
		if arr, _ := rows.([]any); len(arr) != 0 {
			t.Errorf("expected empty rows on error, got %v", rows)
		}
	})

	t.Run("success_returns_columns_and_rows", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if !strings.Contains(q, "LIMIT 5") {
				t.Fatalf("expected LIMIT 5 in query, got %q", q)
			}
			return storetest.Result([]string{"Timestamp"}, []any{"2026-01-01 00:00:00"}), nil
		}}}
		got := s.getTableSample("otel_logs", 5)
		cols, _ := got.Get("columns")
		if arr, _ := cols.([]any); len(arr) != 1 {
			t.Fatalf("expected 1 column, got %v", cols)
		}
	})
}
