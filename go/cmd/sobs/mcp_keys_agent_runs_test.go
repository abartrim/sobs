package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// loadMcpAPIKeys / saveMcpAPIKeys / mcpSafeKeys round-trip the mcp.api_keys app setting; the
// settings page corpus profile only ever sees the empty-list default, so the populated-list and
// invalid-JSON branches are corpus-unreachable. Oracle: mcp.py _load_mcp_api_keys /
// _save_mcp_api_keys / mcp_settings_page.
func TestLoadMcpAPIKeys(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		unset bool
		want  int
	}{
		{"unset -> empty default", "", true, 0},
		{"empty string setting -> empty default", "", false, 0},
		{"invalid json -> empty", "{not json", false, 0},
		{"non-list json -> empty", `{"a":1}`, false, 0},
		{"valid list", `[{"id":"k1"},{"id":"k2"}]`, false, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{}
			if !tc.unset {
				settings["mcp.api_keys"] = tc.raw
			}
			s := &server{db: storetest.SettingsDB(settings)}
			got := s.loadMcpAPIKeys()
			if len(got) != tc.want {
				t.Fatalf("got %d keys, want %d: %v", len(got), tc.want, got)
			}
		})
	}
}

func TestSaveMcpAPIKeys(t *testing.T) {
	fake := &storetest.FakeDB{}
	(&server{db: fake}).saveMcpAPIKeys(nil)
	if len(fake.Inserts) != 1 || fake.Inserts[0].Rows[0]["Value"] != "[]" {
		t.Fatalf("empty save: want stored '[]', got %v", fake.Inserts)
	}

	fake2 := &storetest.FakeDB{}
	(&server{db: fake2}).saveMcpAPIKeys([]any{jsonenc.NewObject().Set("id", "k1")})
	saved, _ := fake2.Inserts[0].Rows[0]["Value"].(string)
	if !strings.Contains(saved, `"id"`) || !strings.Contains(saved, `"k1"`) {
		t.Fatalf("populated save: got %q", saved)
	}
}

func TestMcpSafeKeys(t *testing.T) {
	raw := `[{"id":"k1","label":"CI key","created_at":"2024-01-01","expires_at":"2025-01-01"},` +
		`{"id":"k2"},"not-an-object"]`
	s := &server{db: storetest.SettingsDB(map[string]string{"mcp.api_keys": raw})}
	got := s.mcpSafeKeys()
	if len(got) != 2 { // the bare string entry is skipped
		t.Fatalf("want 2 safe keys, got %d: %v", len(got), got)
	}
	k1 := got[0].(*jsonenc.Object)
	if v, _ := k1.Get("label"); v != "CI key" {
		t.Fatalf("k1 label: got %v", v)
	}
	if v, _ := k1.Get("expires_at"); v != "2025-01-01" {
		t.Fatalf("k1 expires_at: got %v", v)
	}
	k2 := got[1].(*jsonenc.Object)
	if v, _ := k2.Get("label"); v != "" {
		t.Fatalf("k2 label default: got %v", v)
	}
	if v, has := k2.Get("expires_at"); has && v != nil {
		t.Fatalf("k2 expires_at should be absent/nil, got %v (present=%v)", v, has)
	}
}

// loadAgentRunsCtx shapes sobs_agent_runs rows for the settings/agents page; the corpus's
// analyze-only fixture never has a run row, so the populated-list shaping is corpus-unreachable.
// Oracle: app.py _load_agent_runs (agent settings page).
func TestLoadAgentRunsCtx(t *testing.T) {
	cols := []string{"Id", "RuleId", "RuleName", "TriggerContext", "Status", "GuardDecision", "DlpResult",
		"Analysis", "Suggestion", "GithubIssueUrl", "ErrorMessage", "CreatedAt", "CompletedAt", "IsDismissed"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if !strings.Contains(q, "LIMIT 25") {
			t.Fatalf("unexpected query: %s", q)
		}
		return storetest.Result(cols,
			[]any{"run-1", "rule-1", "High CPU", "{}", "completed", "allow", "", "root cause",
				"fix it", "", "", "2024-01-01 00:00:00", "2024-01-01 00:01:00", 1.0},
		), nil
	}}}
	got := s.loadAgentRunsCtx(25)
	if len(got) != 1 {
		t.Fatalf("want 1 run, got %d: %v", len(got), got)
	}
	r := got[0].(map[string]any)
	if r["id"] != "run-1" || r["status"] != "completed" || r["is_dismissed"] != true {
		t.Fatalf("unexpected shape: %v", r)
	}

	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.loadAgentRunsCtx(25); len(got) != 0 {
		t.Fatalf("query error: want empty, got %v", got)
	}
}
