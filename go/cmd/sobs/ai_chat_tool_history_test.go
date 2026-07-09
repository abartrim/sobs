package main

import (
	"errors"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// loadChatToolHistory groups tool.proposed/tool.executed log rows per turn, dedupes by action id
// (falling back to an anon-<ts> key), upgrades status on the matching tool.executed row, and
// sorts each turn's entries by timestamp. None of this is corpus-reachable: the AI-helper
// profiles never emit a tool.executed row for the same action twice, and the empty/anon/skip
// branches never fire on the fixture data. Oracle: app.py _load_chat_tool_history.
func TestLoadChatToolHistory(t *testing.T) {
	cols := []string{"Timestamp", "EventName", "turn_id", "action_id", "summary", "action_json", "action_status", "requires_confirmation"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, params ...any) (*store.Result, error) {
		if len(params) != 2 || params[0] != aiHelperServiceName || params[1] != "chat-1" {
			t.Fatalf("unexpected params: %v", params)
		}
		return storetest.Result(cols,
			[]any{"2024-01-01 00:00:01", "tool.proposed", "t1", "a1", "Run query", `{"tool":"run_query"}`, "", "true"},
			[]any{"2024-01-01 00:00:02", "tool.executed", "t1", "a1", "Run query", "", "", ""}, // upgrades a1 -> executed
			[]any{"2024-01-01 00:00:00", "tool.proposed", "t1", "a2", "Delete row", "", "unsupported", ""},
			[]any{"2024-01-01 00:00:05", "tool.proposed", "", "a3", "Skipped (no turn)", "", "", ""}, // blank turn_id -> dropped
			[]any{"2024-01-01 00:00:06", "tool.proposed", "t2", "", "Anon action", "", "", "1"},      // blank action_id -> anon-<ts>
		), nil
	}}}

	got := s.loadChatToolHistory("chat-1")
	if len(got) != 2 {
		t.Fatalf("want 2 turns, got %d: %v", len(got), got)
	}

	t1 := got["t1"]
	if len(t1) != 2 {
		t.Fatalf("t1: want 2 actions, got %d: %v", len(t1), t1)
	}
	a2, a1 := t1[0].(*jsonenc.Object), t1[1].(*jsonenc.Object) // sorted by ts: a2 (00:00:00) before a1 (00:00:01)
	if v, _ := a2.Get("action_id"); v != "a2" {
		t.Fatalf("t1[0]: want a2 first (earlier ts), got %v", v)
	}
	if v, _ := a2.Get("status_label"); v != "Not available in this page action manifest" {
		t.Fatalf("a2 status_label: got %v", v)
	}
	if v, _ := a1.Get("status"); v != "executed" {
		t.Fatalf("a1 should be upgraded to executed, got %v", v)
	}
	if v, _ := a1.Get("status_label"); v != "Executed" {
		t.Fatalf("a1 status_label: got %v", v)
	}
	action, _ := a1.Get("action")
	if ao, ok := action.(*jsonenc.Object); !ok {
		t.Fatalf("a1 action should be the parsed object, got %v", action)
	} else if v, _ := ao.Get("tool"); v != "run_query" {
		t.Fatalf("a1 action.tool: got %v", v)
	}

	t2 := got["t2"]
	if len(t2) != 1 {
		t.Fatalf("t2: want 1 action, got %d: %v", len(t2), t2)
	}
	anon := t2[0].(*jsonenc.Object)
	if v, _ := anon.Get("action_id"); v != "anon-2024-01-01 00:00:06" {
		t.Fatalf("anon action_id: got %v", v)
	}
	if v, _ := anon.Get("status_label"); v != "Awaiting confirmation" {
		t.Fatalf("anon status_label: got %v", v)
	}

	// Query error -> empty (non-nil) map.
	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.loadChatToolHistory("chat-1"); len(got) != 0 {
		t.Fatalf("query error: want empty map, got %v", got)
	}
}
