package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// loadNotificationRules / loadNotificationLog shape the settings-page listings; the corpus's
// empty-fixture profile never seeds a rule or a fired-notification row, so the populated-shape
// and query-error branches are corpus-unreachable.
// Oracle: app.py _load_notification_rules / _load_notification_log.
func TestLoadNotificationRules(t *testing.T) {
	cols := []string{"Id", "Name", "Enabled", "LogicOperator", "ConditionsJson", "ChannelIds",
		"Severity", "CooldownSeconds", "LastFiredAt"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, _ ...any) (*store.Result, error) {
		return storetest.Result(cols,
			[]any{"r1", "CPU rule", 1.0, "", `[{"type":"signal","source":"cpu","signal":"usage"}]`,
				" c1 , ,c2", "", 0.0, ""}, // blank logic/severity default; blank channel entries dropped
		), nil
	}}}
	got := s.loadNotificationRules()
	if len(got) != 1 {
		t.Fatalf("want 1 rule, got %d: %v", len(got), got)
	}
	r := got[0].(map[string]any)
	if r["enabled"] != true || r["logic_operator"] != "any" || r["severity"] != "warning" {
		t.Fatalf("defaults wrong: %v", r)
	}
	if ids := r["channel_ids"].([]any); len(ids) != 2 || ids[0] != "c1" || ids[1] != "c2" {
		t.Fatalf("channel_ids wrong: %v", ids)
	}
	if conds := r["conditions"].([]any); len(conds) != 1 {
		t.Fatalf("conditions should parse the JSON array, got %v", conds)
	}

	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.loadNotificationRules(); len(got) != 0 {
		t.Fatalf("query error: want empty, got %v", got)
	}
}

func TestLoadNotificationLog(t *testing.T) {
	cols := []string{"Id", "RuleId", "RuleName", "ChannelId", "ChannelName", "FiredAt", "Status", "ErrorMessage", "Summary"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if !strings.Contains(q, "LIMIT ?") || len(params) != 1 || params[0] != 10 {
			t.Fatalf("unexpected query/params: %s %v", q, params)
		}
		return storetest.Result(cols,
			[]any{"log-1", "r1", "CPU rule", "c1", "Slack", "2024-01-01 00:00:00", "sent", "", "CPU over threshold"},
		), nil
	}}}
	got := s.loadNotificationLog(10)
	if len(got) != 1 || got[0].(map[string]any)["status"] != "sent" {
		t.Fatalf("unexpected result: %v", got)
	}

	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.loadNotificationLog(10); len(got) != 0 {
		t.Fatalf("query error: want empty, got %v", got)
	}
}

// seedDashboardIfMissing is the auto-dashboard boot-seed guard: reuse an existing dashboard by
// name, or insert one. The corpus never has an existing "Auto Metric Rules" dashboard to reuse,
// so the reuse and insert-error branches are corpus-unreachable.
// Oracle: app.py _seed_dashboard_if_missing.
func TestSeedDashboardIfMissing(t *testing.T) {
	t.Run("existing dashboard reused", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, params ...any) (*store.Result, error) {
			if params[0] != "Auto Metric Rules Dashboard" {
				t.Fatalf("unexpected params: %v", params)
			}
			return storetest.Result([]string{"Id"}, []any{"dash-existing"}), nil
		}}}
		id, err := s.seedDashboardIfMissing("Auto Metric Rules Dashboard", "desc")
		if err != nil || id != "dash-existing" {
			t.Fatalf("got id=%q err=%v", id, err)
		}
	})

	t.Run("missing dashboard is created", func(t *testing.T) {
		fake := &storetest.FakeDB{}
		id, err := (&server{db: fake}).seedDashboardIfMissing("New Dash", "desc")
		if err != nil || id == "" {
			t.Fatalf("got id=%q err=%v", id, err)
		}
		if len(fake.Inserts) != 1 || fake.Inserts[0].Rows[0]["Name"] != "New Dash" {
			t.Fatalf("unexpected insert: %v", fake.Inserts)
		}
	})

	t.Run("select error propagates", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		if _, err := s.seedDashboardIfMissing("X", "d"); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("insert error propagates", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{InsertErr: errors.New("boom")}}
		if _, err := s.seedDashboardIfMissing("X", "d"); err == nil {
			t.Fatal("want an error")
		}
	})
}

// loadAllAISettings shapes the full ai.* settings surface; the empty fixture never sets a value,
// so both the DB-populated and env-override precedence paths are corpus-unreachable.
// Oracle: app.py _load_all_ai_settings.
func TestLoadAllAISettings(t *testing.T) {
	cols := []string{"Key", "Value"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, _ ...any) (*store.Result, error) {
		return storetest.Result(cols,
			[]any{"ai.model", "gpt-x"},
			[]any{"ai.api_key", "sk-plain"},           // sensitive key, no enc prefix -> passes through
			[]any{"not.a.tracked.key", "should-drop"}, // outside aiSettingKeys -> ignored
		), nil
	}}}
	t.Setenv("SOBS_AI_ENDPOINT_URL", "https://from-env.example")
	got := s.loadAllAISettings()

	if got["ai.model"] != "gpt-x" {
		t.Fatalf("db value: got %q", got["ai.model"])
	}
	if got["ai.api_key"] != "sk-plain" {
		t.Fatalf("sensitive db value: got %q", got["ai.api_key"])
	}
	if _, tracked := got["not.a.tracked.key"]; tracked {
		t.Fatalf("untracked key leaked into result: %v", got)
	}
	if got["ai.endpoint_url"] != "https://from-env.example" {
		t.Fatalf("env override: got %q, want the env value since DB left it blank", got["ai.endpoint_url"])
	}
	if got["ai.system_prompt"] != "" {
		t.Fatalf("unset, no-override key should default to empty: got %q", got["ai.system_prompt"])
	}
	if len(got) != len(aiSettingKeys) {
		t.Fatalf("result should cover exactly the tracked key set, got %d keys, want %d", len(got), len(aiSettingKeys))
	}
}
