package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// countCopilotAssignmentsLastHour / countActiveCopilotAssignments gate the copilot-assignment
// rate limits in _run_agent_flow; the corpus's analyze-only rule never requests a copilot
// assignment, so these counters are corpus-unreachable. Oracle: app.py
// _count_copilot_assignments_last_hour / _count_active_copilot_assignments.

func TestCountCopilotAssignmentsLastHour(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if !strings.Contains(q, "sobs_github_work_items") || !strings.Contains(q, "AS c") {
			t.Fatalf("unexpected query: %s", q)
		}
		if len(params) != 1 {
			t.Fatalf("unexpected params: %v", params)
		}
		return storetest.Result([]string{"c"}, []any{2.0}), nil
	}}}
	if got := s.countCopilotAssignmentsLastHour(); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
}

func TestCountActiveCopilotAssignments(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if !strings.Contains(q, "sobs_github_work_items") || !strings.Contains(q, "'requested', 'active'") {
			t.Fatalf("unexpected query: %s", q)
		}
		return storetest.Result([]string{"c"}, []any{1.0}), nil
	}}}
	if got := s.countActiveCopilotAssignments(); got != 1 {
		t.Fatalf("got %d, want 1", got)
	}
}

func TestCountRows_EmptyOrError(t *testing.T) {
	// Shared by both counters via s.countRowsParams/countRows: no rows -> 0.
	sEmpty := &server{db: &storetest.FakeDB{}}
	if got := sEmpty.countActiveCopilotAssignments(); got != 0 {
		t.Fatalf("empty result: got %d, want 0", got)
	}
	if got := sEmpty.countCopilotAssignmentsLastHour(); got != 0 {
		t.Fatalf("empty result: got %d, want 0", got)
	}
}
