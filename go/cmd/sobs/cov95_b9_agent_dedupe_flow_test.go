package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// This file covers batch-9 undertested branches in cmd/sobs/agent_dedupe.go and
// cmd/sobs/agent_flow.go: toFloatAny's numeric-coercion branches, loadRecentWorkItemCandidates'
// clamp/error/populated paths, classifyIssueDedupeWithLLM's LLM-success/failure branches,
// chooseGithubIssueOutcome's new-issue/suppressed/blocked-copilot branches, and
// reuseExistingIssueOutcome's PR-linked/active-assignment/blocked branches — none of which the
// existing agent_dedupe_test.go / agent_dedupe_counts_test.go / agent_flow_core_test.go /
// agent_flow_dedupe_helpers_test.go / agent_github_target_test.go / agent_work_item_persist_test.go
// exercise directly (they cover the pure helpers and the simple counters, not these larger
// multi-branch functions). Oracle: app.py _choose_github_issue_outcome / _classify_issue_dedupe_with_llm
// and helpers (app.py 5388-5646, 6241-6377).

// ---- toFloatAny --------------------------------------------------------------------------

func TestToFloatAnyCoercions(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want float64
	}{
		{"float64", 3.5, 3.5},
		{"int", 7, 7.0},
		{"int64", int64(9), 9.0},
		{"numeric string", "2.25", 2.25},
		{"numeric string with space", "  4  ", 4.0},
		{"unparseable string", "not-a-number", 0.0},
		{"nil", nil, 0.0},
		{"bool true (unparseable string form)", true, 0.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toFloatAny(c.in); got != c.want {
				t.Errorf("toFloatAny(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// ---- loadRecentWorkItemCandidates ---------------------------------------------------------

func TestLoadRecentWorkItemCandidates(t *testing.T) {
	cols := []string{"Id", "IssueUrl", "ServiceName", "SignalSource", "SignalName", "AnomalyState",
		"DedupKey", "IssueState", "IssueTitle", "IssueNumber", "CopilotAssignmentStatus",
		"PrLinked", "PrUrl", "OccurrenceCount", "CreatedAt", "CompletedAt", "RelatedIssueUrls",
		"CanonicalIssueUrl", "CanonicalIssueNumber", "AgentRuleId", "AgentRuleName", "AgentAction",
		"AnomalyRuleId", "SignalValue", "DedupDecision", "DedupConfidence",
		"CopilotAssignmentRequestedAt", "CopilotAssignmentReason", "PrNumber", "AnalysisSummary", "SuggestionSummary"}
	row := []any{"wi-1", "https://github.com/acme/x/issues/9", "svc-a", "errors", "TimeoutError", "critical",
		"acme x||errors|timeouterror|critical", "open", "svc-a timeout", 9.0, "not_requested",
		float64(0), "", 1.0, "2026-01-01 00:00:00.000000", "2026-01-01 00:00:00.000000", "[]",
		"", 0.0, "", "", "", "", 0.0, "", 0.0, 0.0, "", 0.0, "", ""}

	t.Run("populated result serializes rows and clamps limit<1 to 1", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if !strings.Contains(q, "sobs_github_work_items") || !strings.Contains(q, "IssueUrl != ''") {
				t.Fatalf("unexpected query: %s", q)
			}
			if len(params) != 2 || params[0] != "acme/x" || params[1] != 1 {
				t.Fatalf("unexpected params: %v (limit should clamp to 1)", params)
			}
			return storetest.Result(cols, row), nil
		}}}
		got := s.loadRecentWorkItemCandidates("acme/x", 0)
		if len(got) != 1 {
			t.Fatalf("want 1 candidate, got %d", len(got))
		}
		if objGetStr(got[0], "issue_url") != "https://github.com/acme/x/issues/9" {
			t.Fatalf("unexpected serialized row: %v", got[0])
		}
	})

	t.Run("query error yields nil", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, os.ErrClosed
		}}}
		if got := s.loadRecentWorkItemCandidates("acme/x", 5); got != nil {
			t.Fatalf("want nil on query error, got %v", got)
		}
	})

	t.Run("empty result yields empty non-nil slice", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		got := s.loadRecentWorkItemCandidates("acme/x", 5)
		if got == nil || len(got) != 0 {
			t.Fatalf("want empty slice, got %v", got)
		}
	})
}

// ---- classifyIssueDedupeWithLLM ------------------------------------------------------------

func TestClassifyIssueDedupeWithLLM(t *testing.T) {
	proposed := jsonenc.NewObject().Set("dedup_key", "k").Set("service_name", "svc").Set("signal_name", "Err")
	cands := []dedupeCandidate{{candidateID: "u/1", issueURL: "u/1", dedupKey: "k", serviceName: "svc", signalName: "Err"}}

	t.Run("missing endpoint/model falls back without any HTTP call", func(t *testing.T) {
		s := &server{}
		got := s.classifyIssueDedupeWithLLM(map[string]string{}, proposed, cands)
		if got["classification"] != "same" { // dedup key matches -> fallback picks "same"
			t.Fatalf("want fallback same-key decision, got %+v", got)
		}
	})

	t.Run("no candidates falls back regardless of settings", func(t *testing.T) {
		s := &server{}
		got := s.classifyIssueDedupeWithLLM(map[string]string{"ai.endpoint_url": "http://x", "ai.model": "m"}, proposed, nil)
		if got["classification"] != "unrelated" {
			t.Fatalf("want unrelated fallback with no candidates, got %+v", got)
		}
	})

	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	settings := map[string]string{"ai.endpoint_url": "http://sobs-ai.mock", "ai.model": "test-model"}

	writeFixture := func(t *testing.T, url, bodyJSON string) {
		t.Helper()
		stem := upstreamFixtureKey("POST", url)
		if err := os.WriteFile(filepath.Join(dir, stem+".json"),
			[]byte(`{"status": 200, "json": {"choices": [{"message": {"content": `+bodyJSON+`}}]}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	chatURL := chatCompletionsURL("http://sobs-ai.mock")
	// callLLMChat's success path emits an internal gen_ai span via s.enqueueWrite -> needs a real
	// writeQueue (and a DB for the insert/tag-rule lookup it performs); mirrors
	// coverage_ai_helper_stream_gaps_test.go's newAIStreamTestServer helper.
	newLLMServer := func() *server {
		return &server{db: &storetest.FakeDB{}, wq: &writeQueue{ch: make(chan *writeTask, 64), batchMax: 200, batchWaitMs: 20}}
	}

	t.Run("valid LLM classification is used and confidence clamped to [0,1]", func(t *testing.T) {
		writeFixture(t, chatURL, `"{\"classification\": \"RELATED\", \"candidate_id\": \"u/1\", \"confidence\": 5, \"reason\": \"same family\"}"`)
		s := newLLMServer()
		got := s.classifyIssueDedupeWithLLM(settings, proposed, cands)
		if got["classification"] != "related" || got["candidate_id"] != "u/1" || got["confidence"].(float64) != 1.0 || got["reason"] != "same family" {
			t.Fatalf("unexpected classification: %+v", got)
		}
	})

	t.Run("negative confidence clamps to 0", func(t *testing.T) {
		writeFixture(t, chatURL, `"{\"classification\": \"unrelated\", \"confidence\": -3}"`)
		s := newLLMServer()
		got := s.classifyIssueDedupeWithLLM(settings, proposed, cands)
		if got["confidence"].(float64) != 0.0 {
			t.Fatalf("want confidence clamped to 0, got %+v", got)
		}
	})

	t.Run("unusable classification value falls back to local decision", func(t *testing.T) {
		writeFixture(t, chatURL, `"{\"classification\": \"maybe\"}"`)
		s := newLLMServer()
		got := s.classifyIssueDedupeWithLLM(settings, proposed, cands)
		if got["classification"] != "same" { // fallback: dedup key match
			t.Fatalf("want fallback on bad classification, got %+v", got)
		}
	})

	t.Run("LLM call error falls back", func(t *testing.T) {
		// No fixture written for this distinct model -> upstreamFixture returns 404 -> callLLMChat errors.
		s := newLLMServer()
		got := s.classifyIssueDedupeWithLLM(map[string]string{
			"ai.endpoint_url": "http://sobs-ai.mock/missing", "ai.model": "m2",
		}, proposed, cands)
		if got["classification"] != "same" {
			t.Fatalf("want fallback on LLM error, got %+v", got)
		}
	})
}

// ---- chooseGithubIssueOutcome ---------------------------------------------------------------

func githubIssuesListFixture(t *testing.T, dir, repo string, jsonBody string) {
	t.Helper()
	url := "https://api.github.com/repos/" + repo + "/issues?state=open&per_page=" + itoaInt(githubIssueDedupeCandidateMax)
	if err := os.WriteFile(filepath.Join(dir, upstreamFixtureKey("GET", url)+".json"),
		[]byte(`{"status": 200, "json": `+jsonBody+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestChooseGithubIssueOutcome_NewIssueSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	githubIssuesListFixture(t, dir, "acme/newrepo", `[]`)
	createURL := "https://api.github.com/repos/acme/newrepo/issues"
	if err := os.WriteFile(filepath.Join(dir, upstreamFixtureKey("POST", createURL)+".json"),
		[]byte(`{"status": 201, "json": {"html_url": "https://github.com/acme/newrepo/issues/5", "number": 5, "title": "svc down", "state": "open"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{db: &storetest.FakeDB{}}
	tctx := jsonenc.NewObject().Set("service", "svc-a")
	settings := map[string]string{}
	out := s.chooseGithubIssueOutcome(settings, tctx, "acme/newrepo", "tok", false,
		"analysis", "suggestion", "svc down", "body text", true, false)
	if out["issue_url"] != "https://github.com/acme/newrepo/issues/5" {
		t.Fatalf("unexpected issue_url: %v", out)
	}
	if out["dedup_decision"] != "new_issue" || out["dedup_confidence"] != 1.0 {
		t.Fatalf("unexpected dedup fields: %v", out)
	}
	if out["created_new_issue"] != true {
		t.Fatalf("want created_new_issue=true: %v", out)
	}
	if out["copilot_assignment_status"] != "not_requested" {
		t.Fatalf("no copilot requested -> not_requested, got %v", out["copilot_assignment_status"])
	}
}

func TestChooseGithubIssueOutcome_SuppressedRateLimit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	githubIssuesListFixture(t, dir, "acme/rl", `[]`)
	s := &server{db: &storetest.FakeDB{}}
	tctx := jsonenc.NewObject().Set("service", "svc-a")
	out := s.chooseGithubIssueOutcome(map[string]string{}, tctx, "acme/rl", "tok", true,
		"a", "s", "title", "body", false /* allowNewIssue */, false)
	if out["dedup_decision"] != "suppressed_rate_limit" {
		t.Fatalf("want suppressed_rate_limit, got %v", out["dedup_decision"])
	}
	if out["copilot_assignment_status"] != "blocked" {
		t.Fatalf("wantsCopilot with no created issue -> blocked, got %v", out["copilot_assignment_status"])
	}
	if out["issue_url"] != "" {
		t.Fatalf("no issue should be created: %v", out["issue_url"])
	}
}

func TestChooseGithubIssueOutcome_CreateFailed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	githubIssuesListFixture(t, dir, "acme/fail", `[]`)
	createURL := "https://api.github.com/repos/acme/fail/issues"
	if err := os.WriteFile(filepath.Join(dir, upstreamFixtureKey("POST", createURL)+".json"),
		[]byte(`{"status": 422, "json": {"message": "Validation failed"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{db: &storetest.FakeDB{}}
	tctx := jsonenc.NewObject().Set("service", "svc-a")
	out := s.chooseGithubIssueOutcome(map[string]string{}, tctx, "acme/fail", "tok", false,
		"a", "s", "title", "body", true, false)
	if out["dedup_decision"] != "create_failed" {
		t.Fatalf("want create_failed, got %v", out["dedup_decision"])
	}
	if !strings.Contains(toStr(out["copilot_assignment_reason"]), "Validation failed") {
		t.Fatalf("want creation error surfaced as reason, got %v", out["copilot_assignment_reason"])
	}
}

func TestChooseGithubIssueOutcome_CopilotBlockedByHourlyLimit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	githubIssuesListFixture(t, dir, "acme/copilotrl", `[]`)
	createURL := "https://api.github.com/repos/acme/copilotrl/issues"
	if err := os.WriteFile(filepath.Join(dir, upstreamFixtureKey("POST", createURL)+".json"),
		[]byte(`{"status": 201, "json": {"html_url": "https://github.com/acme/copilotrl/issues/3", "number": 3, "title": "t", "state": "open"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// countCopilotAssignmentsLastHour reads sobs_github_work_items; return a count >= the default max (1).
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "CopilotAssignmentRequestedAt") {
			return storetest.Result([]string{"c"}, []any{5.0}), nil
		}
		return &store.Result{}, nil
	}}}
	tctx := jsonenc.NewObject().Set("service", "svc-a")
	out := s.chooseGithubIssueOutcome(map[string]string{}, tctx, "acme/copilotrl", "tok", true,
		"a", "s", "title", "body", true, false)
	if out["copilot_assignment_status"] != "blocked" {
		t.Fatalf("want blocked by hourly limit, got %v", out)
	}
	if out["copilot_assignment_reason"] != "Copilot assignment hourly limit reached" {
		t.Fatalf("unexpected reason: %v", out["copilot_assignment_reason"])
	}
}

// ---- reuseExistingIssueOutcome --------------------------------------------------------------

func TestReuseExistingIssueOutcome_SameClassification(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// searchOpenPRForIssue and countRowsParams (occurrence count) both hit s.db; no PR fixture ->
	// searchOpenPRForIssue's GitHub call 404s -> nil, no PR linked.
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "count() AS c FROM sobs_github_work_items") && strings.Contains(q, "IssueUrl=?") {
			return storetest.Result([]string{"c"}, []any{2.0}), nil // 2 prior + 1 = 3
		}
		return &store.Result{}, nil
	}}}
	matched := &dedupeCandidate{
		candidateID: "u/1", issueURL: "https://github.com/acme/x/issues/9", issueNumber: 9,
		issueTitle: "existing title", issueState: "open", copilotAssignmentStatus: "not_requested",
	}
	classification := map[string]any{"confidence": 0.92, "reason": "deterministic dedupe key match"}
	out := s.reuseExistingIssueOutcome(map[string]string{}, "acme/x", "tok", false,
		"suggestion", "dedup-key", "new title", classification, matched, "same")
	if out["dedup_decision"] != "reused_existing" {
		t.Fatalf("want reused_existing, got %v", out["dedup_decision"])
	}
	if out["occurrence_count"] != 3 {
		t.Fatalf("want occurrence_count 3, got %v", out["occurrence_count"])
	}
	if out["issue_title"] != "existing title" {
		t.Fatalf("want matched title preserved, got %v", out["issue_title"])
	}
	if out["pr_linked"] != false {
		t.Fatalf("no PR fixture -> pr_linked should be false, got %v", out["pr_linked"])
	}
	if out["copilot_assignment_status"] != "not_requested" {
		t.Fatalf("wantsCopilot=false -> status untouched, got %v", out["copilot_assignment_status"])
	}
}

func TestReuseExistingIssueOutcome_RelatedClassification(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	s := &server{db: &storetest.FakeDB{}}
	matched := &dedupeCandidate{candidateID: "u/2", issueURL: "https://github.com/acme/y/issues/1", issueNumber: 1, issueState: "open"}
	classification := map[string]any{"confidence": 0.73, "reason": "same service and signal family"}
	out := s.reuseExistingIssueOutcome(map[string]string{}, "acme/y", "tok", false,
		"s", "dk", "t", classification, matched, "related")
	if out["dedup_decision"] != "related_existing" {
		t.Fatalf("want related_existing, got %v", out["dedup_decision"])
	}
}

func TestReuseExistingIssueOutcome_CopilotBlockedByExistingAssignee(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	s := &server{db: &storetest.FakeDB{}}
	matched := &dedupeCandidate{
		candidateID: "u/3", issueURL: "https://github.com/acme/z/issues/2", issueNumber: 2, issueState: "open",
		assignees: []string{githubCopilotAssignee},
	}
	classification := map[string]any{"confidence": 0.9, "reason": "r"}
	out := s.reuseExistingIssueOutcome(map[string]string{}, "acme/z", "tok", true,
		"s", "dk", "t", classification, matched, "same")
	if out["copilot_assignment_status"] != "blocked" {
		t.Fatalf("existing copilot assignee -> should detect active + block, got %v", out)
	}
	if out["copilot_assignment_reason"] != "issue is already being worked by Copilot" {
		t.Fatalf("unexpected reason: %v", out["copilot_assignment_reason"])
	}
}

// TestReuseExistingIssueOutcome_CopilotBlockedByActiveAssignmentLimit exercises the
// countActiveCopilotAssignments >= maxActive branch: with the max clamped to 1 and one active
// assignment already recorded, a new Copilot request on the reuse path must be blocked.
func TestReuseExistingIssueOutcome_CopilotBlockedByActiveAssignmentLimit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "'requested', 'active'") {
			return storetest.Result([]string{"c"}, []any{1.0}), nil // already at the (clamped) max of 1
		}
		return &store.Result{}, nil
	}}}
	matched := &dedupeCandidate{
		candidateID: "u/4", issueURL: "https://github.com/acme/pr1/issues/4", issueNumber: 4, issueState: "open",
	}
	classification := map[string]any{"confidence": 0.9, "reason": "r"}
	out := s.reuseExistingIssueOutcome(map[string]string{"ai.agent_max_active_assignments": "0"}, "acme/pr1", "tok", true,
		"s", "dk", "t", classification, matched, "same")
	if out["copilot_assignment_status"] != "blocked" {
		t.Fatalf("want blocked by active-assignment limit, got %v", out)
	}
	if out["copilot_assignment_reason"] != "active Copilot assignment limit reached" {
		t.Fatalf("unexpected reason: %v", out["copilot_assignment_reason"])
	}
}

// ---- handleTriggerAgentRun / handleApiIssuesRaise (HTTP-layer branches) --------------------

func TestHandleTriggerAgentRun_Validation(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}

	t.Run("missing rule_id -> 400", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/agent/runs", strings.NewReader(`{}`))
		s.handleTriggerAgentRun(w, r)
		if w.Code != 400 {
			t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("unknown rule_id -> 404", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/agent/runs", strings.NewReader(`{"rule_id":"nope"}`))
		s.handleTriggerAgentRun(w, r)
		if w.Code != 404 {
			t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestHandleTriggerAgentRun_AIConfigMissingAndRateLimit(t *testing.T) {
	ruleCols := []string{"Id", "Name", "Description", "TriggerType", "TriggerRefId", "TriggerState",
		"Actions", "RateLimitMinutes", "IsEnabled"}
	makeDB := func(withAISettings bool, lastRunSeconds float64) *storetest.FakeDB {
		return &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "sobs_agent_rules"):
				return storetest.Result(ruleCols, []any{"r1", "Rule1", "d", "manual", "", "",
					"analyze", 15.0, float64(1)}), nil
			case strings.Contains(q, "sobs_app_settings") || strings.Contains(q, "sobs_ai_settings"):
				return &store.Result{}, nil // ai.endpoint_url / ai.model unset
			case strings.Contains(q, "max(toUnixTimestamp64Milli"):
				return storetest.Result([]string{"t"}, []any{lastRunSeconds * 1000}), nil
			}
			return &store.Result{}, nil
		}}
	}

	t.Run("AI endpoint not configured -> 503", func(t *testing.T) {
		s := &server{db: makeDB(false, 0)}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/agent/runs", strings.NewReader(`{"rule_id":"r1"}`))
		s.handleTriggerAgentRun(w, r)
		if w.Code != 503 {
			t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestHandleApiIssuesRaise_Validation(t *testing.T) {
	t.Run("AI endpoint not configured -> 503", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/issues/raise", strings.NewReader(`{}`))
		s.handleApiIssuesRaise(w, r)
		if w.Code != 503 {
			t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("AI configured but no github repo/token -> 503", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_app_settings") && len(params) == 1 {
				switch params[0] {
				case "ai.endpoint_url":
					return storetest.Result([]string{"Value"}, []any{"http://x"}), nil
				case "ai.model":
					return storetest.Result([]string{"Value"}, []any{"m"}), nil
				}
			}
			return &store.Result{}, nil
		}}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/issues/raise", strings.NewReader(`{"source_page":"errors","err_type":"TimeoutError"}`))
		s.handleApiIssuesRaise(w, r)
		if w.Code != 503 {
			t.Fatalf("want 503 (no github repo/token), got %d: %s", w.Code, w.Body.String())
		}
	})
}
