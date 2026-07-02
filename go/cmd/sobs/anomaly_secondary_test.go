package main

import (
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// deriveTurnSummary mirrors app.py _derive_turn_summary: meta_summary fields win over the
// question/tool-summary/answer fallbacks, and a blank action falls all the way to "answer_only".
// None of the fallback combinations are corpus-reachable (the AI-helper profiles always emit a
// populated meta_summary). Oracle: app.py _derive_turn_summary (app.py:3749).
func TestDeriveTurnSummary(t *testing.T) {
	// No meta -> everything falls back to the raw args; blank tool summary -> "answer_only".
	got := deriveTurnSummary("what broke?", "nothing, all clear", "", nil)
	if v, _ := got.Get("request"); v != "what broke?" {
		t.Fatalf("request: got %v", v)
	}
	if v, _ := got.Get("action"); v != "answer_only" {
		t.Fatalf("action: got %v", v)
	}
	if v, _ := got.Get("result"); v != "nothing, all clear" {
		t.Fatalf("result: got %v", v)
	}

	// Non-blank tool summary wins over "answer_only" when meta has no action.
	got2 := deriveTurnSummary("q", "a", "ran a query", nil)
	if v, _ := got2.Get("action"); v != "ran a query" {
		t.Fatalf("action fallback to tool summary: got %v", v)
	}

	// meta_summary fields win over every fallback when present.
	meta := jsonenc.NewObject().Set("request", "meta request").Set("action", "meta action").Set("result", "meta result")
	got3 := deriveTurnSummary("q", "a", "tool", meta)
	if v, _ := got3.Get("request"); v != "meta request" {
		t.Fatalf("meta request should win: got %v", v)
	}
	if v, _ := got3.Get("action"); v != "meta action" {
		t.Fatalf("meta action should win: got %v", v)
	}
	if v, _ := got3.Get("result"); v != "meta result" {
		t.Fatalf("meta result should win: got %v", v)
	}

	// A non-string meta field is treated as absent (falls back), and a present-but-empty meta
	// field also falls back (mirrors Python's `summary.get(...) or question`).
	metaEmpty := jsonenc.NewObject().Set("request", "").Set("action", 42)
	got4 := deriveTurnSummary("q4", "a4", "tool4", metaEmpty)
	if v, _ := got4.Get("request"); v != "q4" {
		t.Fatalf("empty meta.request should fall back to question: got %v", v)
	}
	if v, _ := got4.Get("action"); v != "tool4" {
		t.Fatalf("non-string meta.action should fall back to tool summary: got %v", v)
	}
}

// lookupSecondaryRuleRow tries a time-scoped lookup first (when a timeValue is given), falling
// back to the latest row; the composite-rule evaluator that calls it only ever hits the "found
// latest, no timeValue" shape on the fixture data. Oracle: app.py _lookup_secondary_rule_row.
func TestLookupSecondaryRuleRow(t *testing.T) {
	cols := []string{"time", "value", "SampleCount"}

	t.Run("scoped hit", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, params ...any) (*store.Result, error) {
			if len(params) == 5 {
				return storetest.Result(cols, []any{"2024-01-01 00:00:00", "12.5", 3.0}), nil
			}
			t.Fatalf("unscoped query should not run when the scoped lookup hits: params=%v", params)
			return nil, nil
		}}}
		got := s.lookupSecondaryRuleRow("svc", "fp", "cpu", "usage", "2024-01-01 00:00:00")
		if got == nil || got["value"] != "12.5" || got["sample_count"] != 3 {
			t.Fatalf("unexpected row: %v", got)
		}
	})

	t.Run("scoped miss falls back to latest", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, params ...any) (*store.Result, error) {
			if len(params) == 5 {
				return &store.Result{}, nil // scoped miss
			}
			return storetest.Result(cols, []any{"2024-01-01 00:05:00", "8.0", 2.0}), nil
		}}}
		got := s.lookupSecondaryRuleRow("svc", "fp", "cpu", "usage", "2024-01-01 00:00:00")
		if got == nil || got["value"] != "8.0" {
			t.Fatalf("unexpected fallback row: %v", got)
		}
	})

	t.Run("no timeValue goes straight to latest", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, params ...any) (*store.Result, error) {
			if len(params) != 4 {
				t.Fatalf("expected the unscoped 4-param query, got %v", params)
			}
			return storetest.Result(cols, []any{"2024-01-01 00:05:00", "1.0", 1.0}), nil
		}}}
		if got := s.lookupSecondaryRuleRow("svc", "fp", "cpu", "usage", ""); got == nil {
			t.Fatalf("want a row, got nil")
		}
	})

	t.Run("nothing found -> nil", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.lookupSecondaryRuleRow("svc", "fp", "cpu", "usage", ""); got != nil {
			t.Fatalf("want nil, got %v", got)
		}
	})
}
