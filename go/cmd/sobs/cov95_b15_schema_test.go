package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b15_schema_test.go — batch 15 coverage for cmd/sobs/schema.go:
//   ensureSchema (22)     28.6%
//   schemaPresent (36)    71.4%

func TestSchemaPresent(t *testing.T) {
	t.Run("present when count > 0", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if !strings.Contains(q, "system.tables") || !strings.Contains(q, "otel_logs") {
				t.Fatalf("unexpected query: %s", q)
			}
			return storetest.Result([]string{"c"}, []any{1.0}), nil
		}}}
		if !s.schemaPresent() {
			t.Error("expected schemaPresent() = true")
		}
	})

	t.Run("absent when count is 0", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			return storetest.Result([]string{"c"}, []any{0.0}), nil
		}}}
		if s.schemaPresent() {
			t.Error("expected schemaPresent() = false")
		}
	})

	t.Run("absent on query error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			return nil, errB15Boom
		}}}
		if s.schemaPresent() {
			t.Error("expected schemaPresent() = false on error")
		}
	})

	t.Run("absent on empty result rows", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if s.schemaPresent() {
			t.Error("expected schemaPresent() = false on no rows")
		}
	})
}

func TestEnsureSchema(t *testing.T) {
	t.Run("nil db is a strict no-op", func(t *testing.T) {
		s := &server{db: nil}
		s.ensureSchema() // must not panic
	})

	t.Run("schema already present -> no DDL executed", func(t *testing.T) {
		calls := 0
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			calls++
			if strings.Contains(q, "system.tables") {
				return storetest.Result([]string{"c"}, []any{1.0}), nil
			}
			t.Fatalf("unexpected DDL execution when schema already present: %s", q)
			return nil, nil
		}}}
		s.ensureSchema()
		if calls != 1 {
			t.Errorf("expected exactly 1 call (the presence check), got %d", calls)
		}
	})

	t.Run("schema absent -> applies every DDL statement", func(t *testing.T) {
		var executed []string
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "system.tables") {
				return &store.Result{}, nil // absent -> proceed to apply DDL
			}
			executed = append(executed, q)
			return &store.Result{}, nil
		}}}
		s.ensureSchema()
		if len(executed) == 0 {
			t.Fatal("expected at least one DDL statement to be executed")
		}
		// Every embedded schema.sql fragment (split on ';') should have been attempted.
		wantCount := len(splitSQLStatements(schemaSQL))
		if len(executed) != wantCount {
			t.Errorf("executed %d statements, want %d (the split count)", len(executed), wantCount)
		}
	})

	t.Run("a failing DDL statement does not abort the loop", func(t *testing.T) {
		var executed int
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "system.tables") {
				return &store.Result{}, nil
			}
			executed++
			if executed == 1 {
				return nil, errB15Boom
			}
			return &store.Result{}, nil
		}}}
		s.ensureSchema() // must not panic; must keep going past the first failure
		wantCount := len(splitSQLStatements(schemaSQL))
		if executed != wantCount {
			t.Errorf("executed %d statements despite one failing, want %d (loop must continue)", executed, wantCount)
		}
	})
}
