package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Coverage batch 14: cmd/sobs/notif_agent.go — no dedicated test file existed yet.
// (pickHighestSeverityEvent and truncateAttrObject are already covered by
// misc_helpers2_test.go, so they're intentionally not duplicated here.)

func TestNormalizeAgentTriggerState(t *testing.T) {
	cases := map[string]string{
		"outlier":   "critical",
		"OUTLIER":   "critical",
		"warning":   "warning",
		"critical":  "critical",
		" Warning ": "warning",
		"":          "normal",
		"unknown":   "normal",
		"ok":        "normal",
	}
	for in, want := range cases {
		if got := normalizeAgentTriggerState(in); got != want {
			t.Errorf("normalizeAgentTriggerState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAgentRuleTriggerStateMatches(t *testing.T) {
	cases := []struct {
		trigger, event string
		want           bool
	}{
		{"", "warning", true}, // empty -> "any" -> warning/critical match
		{"", "critical", true},
		{"", "normal", false},
		{"any", "warning", true},
		{"ANY", "critical", true},
		{"warning", "warning", true},
		{"warning", "critical", false},
		{"critical", "critical", true},
		{"critical", "warning", false},
		{" Critical ", "critical", true}, // trimmed + lowercased
	}
	for _, c := range cases {
		if got := agentRuleTriggerStateMatches(c.trigger, c.event); got != c.want {
			t.Errorf("agentRuleTriggerStateMatches(%q,%q) = %v, want %v", c.trigger, c.event, got, c.want)
		}
	}
}

func TestOrderedAgentEvents(t *testing.T) {
	o := newOrderedAgentEvents()
	if len(o.values()) != 0 {
		t.Fatal("expected empty initially")
	}
	e1 := jsonenc.NewObject().Set("state", "warning")
	e2 := jsonenc.NewObject().Set("state", "critical")
	o.set("r1", e1)
	o.set("r2", e2)
	// Re-setting r1 should keep its original position but update its value.
	e1Updated := jsonenc.NewObject().Set("state", "critical")
	o.set("r1", e1Updated)

	vals := o.values()
	if len(vals) != 2 {
		t.Fatalf("expected 2 events, got %d", len(vals))
	}
	if vals[0] != e1Updated {
		t.Error("expected r1's updated value to occupy its original (first) position")
	}
	if vals[1] != e2 {
		t.Error("expected r2 second")
	}
	if o.get("r1") != e1Updated {
		t.Error("get(r1) should return the updated value")
	}
	if o.get("missing") != nil {
		t.Error("get on missing id should return nil")
	}
}

func TestCollectAnomalyAgentEvents(t *testing.T) {
	t.Run("db_error_returns_empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		got := s.collectAnomalyAgentEvents()
		if len(got.values()) != 0 {
			t.Errorf("expected empty on db error, got %v", got.values())
		}
	})

	t.Run("no_rows_returns_empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return &store.Result{}, nil
		}}}
		got := s.collectAnomalyAgentEvents()
		if len(got.values()) != 0 {
			t.Errorf("expected empty for no rows, got %v", got.values())
		}
	})

	t.Run("matching_threshold_rule_emits_critical_event", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "v_derived_signals_anomaly"):
				return storetest.Result(
					[]string{"ServiceName", "SignalSource", "SignalName", "AttrFingerprint", "value", "SampleCount", "latest_time"},
					[]any{"checkout", "traces", "error_rate", "", float64(99), float64(10), "2026-01-01 00:00:00"},
				), nil
			case strings.Contains(q, "sobs_anomaly_rules"):
				return storetest.Result(
					[]string{"Id", "Name", "RuleType", "SignalSource", "SignalName", "ServiceName", "AttrFingerprint",
						"Comparator", "WarningThreshold", "CriticalThreshold", "SecondarySignalSource",
						"SecondarySignalName", "SecondaryComparator", "SecondaryWarningThreshold",
						"SecondaryCriticalThreshold", "MinSampleCount", "SeasonalBucketsJson"},
					[]any{"rule-crit", "High Error Rate", "threshold", "traces", "error_rate", "", "",
						"gt", float64(50), float64(90), "", "", "", float64(0), float64(0), float64(1), ""},
				), nil
			}
			return &store.Result{}, nil
		}}}
		got := s.collectAnomalyAgentEvents()
		vals := got.values()
		if len(vals) != 1 {
			t.Fatalf("expected 1 event, got %v", vals)
		}
		if objStrOr(vals[0], "state") != "critical" {
			t.Errorf("state = %v, want critical (outlier normalized)", vals[0])
		}
		if objStrOr(vals[0], "service") != "checkout" || objStrOr(vals[0], "signal") != "error_rate" {
			t.Errorf("got %v", vals[0])
		}
	})

	t.Run("no_matching_rule_produces_no_event", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "v_derived_signals_anomaly"):
				return storetest.Result(
					[]string{"ServiceName", "SignalSource", "SignalName", "AttrFingerprint", "value", "SampleCount", "latest_time"},
					[]any{"checkout", "traces", "error_rate", "", float64(1), float64(10), "2026-01-01 00:00:00"},
				), nil
			case strings.Contains(q, "sobs_anomaly_rules"):
				return &store.Result{}, nil // no rules configured -> effective_state stays "normal"
			}
			return &store.Result{}, nil
		}}}
		got := s.collectAnomalyAgentEvents()
		if len(got.values()) != 0 {
			t.Errorf("expected no events without a matching/triggering rule, got %v", got.values())
		}
	})
}

func TestCollectTagRuleAgentEvents(t *testing.T) {
	t.Run("no_tag_rules_short_circuits", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			// loadTagRulesCtx query returns empty -> collectTagRuleAgentEvents must return
			// before ever querying sobs_record_tags.
			if strings.Contains(q, "sobs_record_tags") {
				t.Fatal("should not query sobs_record_tags when there are no tag rules")
			}
			return &store.Result{}, nil
		}}}
		got := s.collectTagRuleAgentEvents(5)
		if len(got.values()) != 0 {
			t.Errorf("expected empty, got %v", got.values())
		}
	})

	t.Run("db_error_on_tag_count_query_returns_empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "sobs_tag_rules"):
				return storetest.Result(
					[]string{"Id", "Name", "RecordTypes", "MatchField", "MatchOperator", "MatchValue",
						"MatchAttrKey", "TagKey", "TagValue", "ConditionsJson"},
					[]any{"rule-1", "Rule 1", "", "", "", "", "", "env", "prod", ""},
				), nil
			case strings.Contains(q, "sobs_record_tags"):
				return nil, errors.New("boom")
			}
			return &store.Result{}, nil
		}}}
		got := s.collectTagRuleAgentEvents(5)
		if len(got.values()) != 0 {
			t.Errorf("expected empty on tag-count query error, got %v", got.values())
		}
	})

	t.Run("matching_tag_kv_emits_warning_event", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "sobs_tag_rules"):
				return storetest.Result(
					[]string{"Id", "Name", "RecordTypes", "MatchField", "MatchOperator", "MatchValue",
						"MatchAttrKey", "TagKey", "TagValue", "ConditionsJson"},
					[]any{"rule-1", "Rule 1", "", "", "", "", "", "env", "prod", ""},
				), nil
			case strings.Contains(q, "sobs_record_tags"):
				return storetest.Result([]string{"TagKey", "TagValue", "c"}, []any{"env", "prod", float64(3)}), nil
			}
			return &store.Result{}, nil
		}}}
		got := s.collectTagRuleAgentEvents(5)
		vals := got.values()
		if len(vals) != 1 {
			t.Fatalf("expected 1 event, got %v", vals)
		}
		if objStrOr(vals[0], "state") != "warning" {
			t.Errorf("state = %v", vals[0])
		}
		if objStrOr(vals[0], "tag_key") != "env" || objStrOr(vals[0], "tag_value") != "prod" {
			t.Errorf("got %v", vals[0])
		}
	})

	t.Run("unmatched_tag_kv_ignored", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "sobs_tag_rules"):
				return storetest.Result(
					[]string{"Id", "Name", "RecordTypes", "MatchField", "MatchOperator", "MatchValue",
						"MatchAttrKey", "TagKey", "TagValue", "ConditionsJson"},
					[]any{"rule-1", "Rule 1", "", "", "", "", "", "env", "prod", ""},
				), nil
			case strings.Contains(q, "sobs_record_tags"):
				return storetest.Result([]string{"TagKey", "TagValue", "c"}, []any{"other", "value", float64(1)}), nil
			}
			return &store.Result{}, nil
		}}}
		got := s.collectTagRuleAgentEvents(5)
		if len(got.values()) != 0 {
			t.Errorf("expected no events for an unmatched (tag_key,tag_value), got %v", got.values())
		}
	})
}

func TestAgentRuleFromCtx(t *testing.T) {
	m := map[string]any{
		"id": "r1", "name": "Rule 1", "description": "desc",
		"trigger_type": "anomaly_rule", "trigger_ref_id": "ref-1", "trigger_state": "critical",
		"actions":            []any{"notify", "  ", "github_issue"},
		"rate_limit_minutes": float64(30),
		"is_enabled":         true,
	}
	rule := agentRuleFromCtx(m)
	if rule.id != "r1" || rule.name != "Rule 1" || rule.triggerType != "anomaly_rule" {
		t.Errorf("got %+v", rule)
	}
	if len(rule.actions) != 2 || rule.actions[0] != "notify" || rule.actions[1] != "github_issue" {
		t.Errorf("expected blank action trimmed out, got %v", rule.actions)
	}
	if rule.rateLimitMinutes != 30 {
		t.Errorf("rateLimitMinutes = %d", rule.rateLimitMinutes)
	}
	if !rule.isEnabled {
		t.Error("expected isEnabled true")
	}
	if !rule.hasAction("notify") || rule.hasAction("bogus") {
		t.Error("hasAction mismatch")
	}
}

func TestAgentRuleFromCtx_MissingActionsAndDisabled(t *testing.T) {
	m := map[string]any{"id": "r2", "is_enabled": false}
	rule := agentRuleFromCtx(m)
	if len(rule.actions) != 0 {
		t.Errorf("expected no actions, got %v", rule.actions)
	}
	if rule.isEnabled {
		t.Error("expected isEnabled false")
	}
}

func TestEvaluateAgentRuleTriggers_AIUnconfigured(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}} // no ai.endpoint_url/ai.model settings
	got := s.evaluateAgentRuleTriggers()
	if len(got) != 0 {
		t.Errorf("expected empty when AI is unconfigured, got %v", got)
	}
}

func TestEvaluateAgentRuleTriggers_AIConfiguredNoRules(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "FROM sobs_ai_settings"):
			return storetest.Result([]string{"Key", "Value"},
				[]any{"ai.endpoint_url", "https://llm.example"},
				[]any{"ai.model", "gpt-4o"},
			), nil
		case strings.Contains(q, "sobs_agent_rules"):
			return &store.Result{}, nil // no agent rules configured
		}
		return &store.Result{}, nil
	}}}
	got := s.evaluateAgentRuleTriggers()
	if len(got) != 0 {
		t.Errorf("expected empty with no configured agent rules, got %v", got)
	}
}

func TestEvaluateAgentRuleTriggers_RateLimitedRuleSkipped(t *testing.T) {
	recentRunMillis := float64(nowUTC().UnixMilli()) // "just ran" -> well within any rate limit window
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "FROM sobs_ai_settings"):
			return storetest.Result([]string{"Key", "Value"},
				[]any{"ai.endpoint_url", "https://llm.example"},
				[]any{"ai.model", "gpt-4o"},
			), nil
		case strings.Contains(q, "v_derived_signals_anomaly"):
			return storetest.Result(
				[]string{"ServiceName", "SignalSource", "SignalName", "AttrFingerprint", "value", "SampleCount", "latest_time"},
				[]any{"checkout", "traces", "error_rate", "", float64(99), float64(10), "2026-01-01 00:00:00"},
			), nil
		case strings.Contains(q, "FROM sobs_anomaly_rules"):
			return storetest.Result(
				[]string{"Id", "Name", "RuleType", "SignalSource", "SignalName", "ServiceName", "AttrFingerprint",
					"Comparator", "WarningThreshold", "CriticalThreshold", "SecondarySignalSource",
					"SecondarySignalName", "SecondaryComparator", "SecondaryWarningThreshold",
					"SecondaryCriticalThreshold", "MinSampleCount", "SeasonalBucketsJson"},
				[]any{"rule-crit", "High Error Rate", "threshold", "traces", "error_rate", "", "",
					"gt", float64(50), float64(90), "", "", "", float64(0), float64(0), float64(1), ""},
			), nil
		case strings.Contains(q, "FROM sobs_agent_rules"):
			return storetest.Result(
				[]string{"Id", "Name", "Description", "TriggerType", "TriggerRefId", "TriggerState",
					"Actions", "RateLimitMinutes", "IsEnabled"},
				[]any{"agent-1", "Notify on High Error Rate", "", "anomaly_rule", "rule-crit", "any",
					"notify", float64(60), float64(1)},
			), nil
		case strings.Contains(q, "sobs_agent_runs"):
			return storetest.Result([]string{"t"}, []any{recentRunMillis}), nil
		case strings.Contains(q, "sobs_tag_rules"):
			return &store.Result{}, nil
		}
		return &store.Result{}, nil
	}}}
	got := s.evaluateAgentRuleTriggers()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 (rate-limited) result, got %v", got)
	}
	res, _ := got[0].(*jsonenc.Object)
	if objStrOr(res, "status") != "skipped_rate_limited" {
		t.Errorf("status = %v, want skipped_rate_limited", res)
	}
	if objStrOr(res, "rule_id") != "agent-1" {
		t.Errorf("rule_id = %v", res)
	}
}

func TestEvaluateAgentRuleTriggers_DisabledRuleSkipped(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "FROM sobs_ai_settings"):
			return storetest.Result([]string{"Key", "Value"},
				[]any{"ai.endpoint_url", "https://llm.example"},
				[]any{"ai.model", "gpt-4o"},
			), nil
		case strings.Contains(q, "FROM sobs_agent_rules"):
			return storetest.Result(
				[]string{"Id", "Name", "Description", "TriggerType", "TriggerRefId", "TriggerState",
					"Actions", "RateLimitMinutes", "IsEnabled"},
				[]any{"agent-2", "Disabled Rule", "", "anomaly_rule", "", "any", "", float64(0), float64(0)},
			), nil
		}
		return &store.Result{}, nil
	}}}
	got := s.evaluateAgentRuleTriggers()
	if len(got) != 0 {
		t.Errorf("expected disabled rule to be skipped entirely, got %v", got)
	}
}

func TestEvaluateAgentRuleTriggers_UnknownTriggerTypeSkipped(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "FROM sobs_ai_settings"):
			return storetest.Result([]string{"Key", "Value"},
				[]any{"ai.endpoint_url", "https://llm.example"},
				[]any{"ai.model", "gpt-4o"},
			), nil
		case strings.Contains(q, "FROM sobs_agent_rules"):
			return storetest.Result(
				[]string{"Id", "Name", "Description", "TriggerType", "TriggerRefId", "TriggerState",
					"Actions", "RateLimitMinutes", "IsEnabled"},
				[]any{"agent-3", "Weird Trigger", "", "some_unknown_type", "", "any", "", float64(0), float64(1)},
			), nil
		}
		return &store.Result{}, nil
	}}}
	got := s.evaluateAgentRuleTriggers()
	if len(got) != 0 {
		t.Errorf("expected an unrecognized trigger_type to be skipped, got %v", got)
	}
}
