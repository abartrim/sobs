package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// loadAgentRule / agentRuleLastRunTs / hasAction / insertAgentRun / buildAgentIssueBody /
// countGithubIssuesLastHour are agent-flow building blocks the corpus's single analyze-only rule
// never exercises beyond the happy path. Oracle: app.py _load_agent_rule /
// _agent_rule_last_run_ts / _run_agent_rule_instance / _count_github_issues_last_hour.

func TestLoadAgentRule(t *testing.T) {
	cols := []string{"Id", "Name", "Description", "TriggerType", "TriggerRefId", "TriggerState",
		"Actions", "RateLimitMinutes", "IsEnabled"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, params ...any) (*store.Result, error) {
		if len(params) != 1 || params[0] != "rule-1" {
			t.Fatalf("unexpected params: %v", params)
		}
		return storetest.Result(cols,
			[]any{"rule-1", "High CPU", "desc", "anomaly", "sig-1", "outlier", "github_issue, , analyze", 15.0, float64(1)},
		), nil
	}}}
	got := s.loadAgentRule("rule-1")
	if got == nil {
		t.Fatal("want a rule, got nil")
	}
	if got.name != "High CPU" || got.rateLimitMinutes != 15 || !got.isEnabled {
		t.Fatalf("unexpected rule: %+v", got)
	}
	if len(got.actions) != 2 || got.actions[0] != "github_issue" || got.actions[1] != "analyze" {
		t.Fatalf("actions should drop blanks: %v", got.actions)
	}
	if !got.hasAction("analyze") || got.hasAction("nope") {
		t.Fatalf("hasAction wrong: %v", got.actions)
	}

	// Not found / query error -> nil.
	sEmpty := &server{db: &storetest.FakeDB{}}
	if got := sEmpty.loadAgentRule("missing"); got != nil {
		t.Fatalf("not found: want nil, got %+v", got)
	}
	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.loadAgentRule("rule-1"); got != nil {
		t.Fatalf("query error: want nil, got %+v", got)
	}
}

func TestAgentRuleLastRunTs(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, params ...any) (*store.Result, error) {
		if len(params) != 1 || params[0] != "rule-1" {
			t.Fatalf("unexpected params: %v", params)
		}
		return storetest.Result([]string{"t"}, []any{5_000.0}), nil // 5000ms -> 5.0s
	}}}
	if got := s.agentRuleLastRunTs("rule-1"); got != 5.0 {
		t.Fatalf("got %v, want 5.0", got)
	}

	// No prior runs (t <= 0, e.g. NULL from max() over no rows) -> 0.
	sNone := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result([]string{"t"}, []any{0.0}), nil
	}}}
	if got := sNone.agentRuleLastRunTs("rule-1"); got != 0 {
		t.Fatalf("got %v, want 0", got)
	}

	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.agentRuleLastRunTs("rule-1"); got != 0 {
		t.Fatalf("query error: got %v, want 0", got)
	}
}

func TestInsertAgentRun(t *testing.T) {
	fake := &storetest.FakeDB{}
	(&server{db: fake}).insertAgentRun(map[string]any{"Id": "run-1", "Status": "pending"})
	if len(fake.Inserts) != 1 || fake.Inserts[0].Table != "sobs_agent_runs" {
		t.Fatalf("unexpected inserts: %v", fake.Inserts)
	}
	if fake.Inserts[0].Rows[0]["Id"] != "run-1" {
		t.Fatalf("unexpected row: %v", fake.Inserts[0].Rows[0])
	}
}

func TestCountGithubIssuesLastHour(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if !strings.Contains(q, "sobs_agent_runs") || !strings.Contains(q, "GithubIssueUrl != ''") ||
			!strings.Contains(q, "AS c") {
			t.Fatalf("unexpected query: %s", q)
		}
		return storetest.Result([]string{"c"}, []any{3.0}), nil
	}}}
	if got := s.countGithubIssuesLastHour(); got != 3 {
		t.Fatalf("got %d, want 3", got)
	}
}

func TestBuildAgentIssueBody(t *testing.T) {
	rule := &agentRule{name: "High CPU"}
	tctx := jsonenc.NewObject().Set("trigger_state", "firing").
		Set("extra", jsonenc.NewObject().Set("additional_context", "please check the deploy"))
	tf := triggerFields{serviceName: "svc-a", signalSource: "cpu", signalName: "usage"}

	body := buildAgentIssueBody(rule, tctx, tf, "ctx summary", "root cause here", "suggested fix here")
	for _, want := range []string{
		"**Rule:** High CPU",
		"**Trigger state:** firing",
		"**Service:** svc-a",
		"**Signal:** cpu/usage",
		"### Telemetry Context\n```\nctx summary\n```",
		"### Root Cause Analysis\nroot cause here",
		"### Suggested Fix\nsuggested fix here",
		"### Additional Context\nplease check the deploy",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q; got:\n%s", want, body)
		}
	}

	// Blank rule name falls back to "Agent Rule"; no extra -> no Additional Context section.
	blank := buildAgentIssueBody(&agentRule{}, jsonenc.NewObject(), tf, "c", "a", "s")
	if !strings.Contains(blank, "**Rule:** Agent Rule") {
		t.Fatalf("want default rule name, got:\n%s", blank)
	}
	if strings.Contains(blank, "### Additional Context") {
		t.Fatalf("unexpected Additional Context section:\n%s", blank)
	}
}
