package main

import (
	"errors"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// The ai_helper chat-context loaders read otel_logs turn.summary events; they run inside the
// buildAIHelperContext assembly, a path the byte-parity corpus reaches only on the seeded AI
// profiles and never on its error/empty branches. Drive them directly through the store.DB seam.
// Oracle: app.py _load_recent_chat_turns / _load_recent_turn_summaries.

var turnCols = []string{"Timestamp", "request", "action", "result", "turn_id"}

func TestLoadRecentChatTurns(t *testing.T) {
	// Blank chatID → guard returns nil without querying.
	if got := (&server{db: &storetest.FakeDB{}}).loadRecentChatTurns("   ", 5); got != nil {
		t.Fatalf("blank chatID: want nil, got %v", got)
	}

	// limit<1 is normalised to 1 (assert via the bound query param), and an all-empty row is
	// skipped while a populated one is kept.
	var gotLimit any
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, params ...any) (*store.Result, error) {
		if len(params) == 3 {
			if params[0] != aiHelperServiceName || params[1] != "chat-1" {
				t.Fatalf("unexpected params: %v", params)
			}
			gotLimit = params[2]
		}
		return storetest.Result(turnCols,
			[]any{"2023-01-01 00:00:00", "find errors", "ran query", "3 rows", "t1"},
			[]any{"2023-01-01 00:00:01", "", "", "", "t2"}, // all-empty → skipped
		), nil
	}}}
	got := s.loadRecentChatTurns("chat-1", 0)
	if gotLimit != 1 {
		t.Fatalf("limit normalisation: want bound param 1, got %v", gotLimit)
	}
	if len(got) != 1 || got[0].turnID != "t1" || got[0].request != "find errors" || got[0].result != "3 rows" {
		t.Fatalf("unexpected turns: %+v", got)
	}

	// Query error → nil.
	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.loadRecentChatTurns("chat-1", 5); got != nil {
		t.Fatalf("query error: want nil, got %v", got)
	}
}

func TestLoadRecentTurnSummaries(t *testing.T) {
	// A candidate identical to the query scores cosine 1.0 (>= 0.2 threshold) and is kept; a row
	// with empty request+result is skipped before scoring.
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(turnCols,
			[]any{"2023-01-01 00:00:00", "database migration plan", "drafted steps", "database migration plan", "keep"},
			[]any{"2023-01-01 00:00:01", "", "noop", "", "skip"}, // req=="" && result=="" → skipped
		), nil
	}}}
	got := s.loadRecentTurnSummaries("chat-1", "database migration plan", 4)
	if len(got) != 1 || got[0].turnID != "keep" {
		t.Fatalf("expected only the query-matching turn, got %+v", got)
	}

	// Query error → nil.
	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.loadRecentTurnSummaries("chat-1", "q", 4); got != nil {
		t.Fatalf("query error: want nil, got %v", got)
	}
}
