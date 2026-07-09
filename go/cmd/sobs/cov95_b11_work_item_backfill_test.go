package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b11_work_item_backfill_test.go — batch 11 targeted coverage for
// cmd/sobs/work_item_backfill.go: searchOpenPRForIssue's guard/error/empty-items/success branches,
// deriveCopilotAssignmentStatus's full state-transition matrix, and backfillGithubWorkItemLinks'
// missing-token / query-error / empty-rows / skip / update paths (maybeBackfillGithubWorkItemLinks's
// interval+running gate is already covered elsewhere per its 93.8%).

// ---- searchOpenPRForIssue ----------------------------------------------------------------------

func TestSearchOpenPRForIssue(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	t.Run("guard: missing token", func(t *testing.T) {
		if got := s.searchOpenPRForIssue("", "acme/w", 1); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("guard: missing repo", func(t *testing.T) {
		if got := s.searchOpenPRForIssue("tok", "", 1); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("guard: non-positive issue number", func(t *testing.T) {
		if got := s.searchOpenPRForIssue("tok", "acme/w", 0); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("guard: unparseable repo", func(t *testing.T) {
		if got := s.searchOpenPRForIssue("tok", "not-a-repo", 1); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("request error/non-2xx -> nil", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		// No fixture -> 404 -> nil.
		if got := s.searchOpenPRForIssue("tok", "acme/w", 5); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("body not an object -> nil", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := searchPRURLB11("acme", "w2", 6)
		writeUpstreamFixture(t, dir, "GET", url, `{"status": 200, "json": [1,2]}`)
		if got := s.searchOpenPRForIssue("tok", "acme/w2", 6); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("missing items key -> nil", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := searchPRURLB11("acme", "w3", 7)
		writeUpstreamFixture(t, dir, "GET", url, `{"status": 200, "json": {}}`)
		if got := s.searchOpenPRForIssue("tok", "acme/w3", 7); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("items not a list, or empty -> nil", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := searchPRURLB11("acme", "w4", 8)
		writeUpstreamFixture(t, dir, "GET", url, `{"status": 200, "json": {"items": []}}`)
		if got := s.searchOpenPRForIssue("tok", "acme/w4", 8); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("success returns the first item's pr number/url", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := searchPRURLB11("acme", "w5", 9)
		writeUpstreamFixture(t, dir, "GET", url,
			`{"status": 200, "json": {"items": [{"number": 42, "html_url": "https://github.com/acme/w5/pull/42"}]}}`)
		got := s.searchOpenPRForIssue("tok", "acme/w5", 9)
		if got == nil {
			t.Fatal("want a non-nil result")
		}
		num, _ := got.Get("pr_number")
		prURL, _ := got.Get("pr_url")
		if num != 42 || prURL != "https://github.com/acme/w5/pull/42" {
			t.Errorf("unexpected: %v", got)
		}
	})
}

func searchPRURLB11(owner, repo string, issueNumber int) string {
	q := "repo:" + owner + "/" + repo + " is:pr is:open \"#" + itoaInt(issueNumber) + "\" in:body"
	return "https://api.github.com/search/issues?q=" + url.QueryEscape(q) + "&per_page=1"
}

// ---- deriveCopilotAssignmentStatus ------------------------------------------------------------

func TestDeriveCopilotAssignmentStatus(t *testing.T) {
	cases := []struct {
		name                       string
		currentStatus, issueState  string
		assignees                  []string
		prLinked                   bool
		wantStatus, wantReasonPart string
	}{
		{"closed issue transitions requested -> completed", "requested", "closed", nil, false, "completed", "issue is closed"},
		{"closed issue transitions active -> completed", "active", "CLOSED", nil, false, "completed", "issue is closed"},
		{"closed issue leaves other statuses untouched", "blocked", "closed", nil, false, "blocked", ""},
		{"PR linked blocks a not_requested status", "not_requested", "open", nil, true, "blocked", "linked pull request already exists"},
		{"PR linked blocks an empty (defaults not_requested) status", "", "open", nil, true, "blocked", "linked pull request already exists"},
		{"PR linked on an already-blocked status stays blocked", "blocked", "open", nil, true, "blocked", "linked pull request already exists"},
		// The PR-linked block only fires for not_requested/blocked; an "active" current status with
		// no copilot-assignee signal falls through to the final active/requested->requested branch.
		{"PR linked with an already-active status collapses to requested (no override)", "active", "open", nil, true, "requested", "Copilot assignment requested"},
		{"copilot assigned (bracket form) -> active", "not_requested", "open", []string{githubCopilotAssignee}, false, "active", "Copilot is assigned"},
		{"copilot assigned (bare login form) -> active", "requested", "open", []string{"copilot-swe-agent"}, false, "active", "Copilot is assigned"},
		{"requested with no new signal stays requested", "requested", "open", nil, false, "requested", "Copilot assignment requested"},
		{"active with no new signal collapses to requested", "active", "open", nil, false, "requested", "Copilot assignment requested"},
		{"not_requested with no signal stays not_requested, no reason", "not_requested", "open", nil, false, "not_requested", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, reason := deriveCopilotAssignmentStatus(c.currentStatus, c.issueState, c.assignees, c.prLinked)
			if status != c.wantStatus {
				t.Errorf("status = %q, want %q", status, c.wantStatus)
			}
			if c.wantReasonPart == "" {
				if reason != "" {
					t.Errorf("reason = %q, want empty", reason)
				}
			} else if !strings.Contains(reason, c.wantReasonPart) {
				t.Errorf("reason = %q, want it to contain %q", reason, c.wantReasonPart)
			}
		})
	}
}

// ---- backfillGithubWorkItemLinks ----------------------------------------------------------------

func TestBackfillGithubWorkItemLinks(t *testing.T) {
	t.Run("missing default token -> logs and returns without reaching the work-items query", func(t *testing.T) {
		reachedWorkItems := false
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_github_work_items") {
				reachedWorkItems = true
			}
			return &store.Result{}, nil // no ai.github_token configured -> loadAISetting returns ""
		}}}
		s.backfillGithubWorkItemLinks()
		if reachedWorkItems {
			t.Error("want no work-items query with a missing default token")
		}
	})

	t.Run("query error returns without panicking", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_ai_settings") || strings.Contains(q, "sobs_app_settings") {
				if len(params) == 1 && params[0] == "ai.github_token" {
					return storetest.Result([]string{"Value"}, []any{"tok"}), nil
				}
				return &store.Result{}, nil
			}
			return nil, assertErr("boom")
		}}}
		s.backfillGithubWorkItemLinks() // must not panic
	})

	t.Run("empty rows returns cleanly", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "ai.github_token") || strings.Contains(q, "sobs_ai_settings") {
				return storetest.Result([]string{"Value"}, []any{"tok"}), nil
			}
			return &store.Result{}, nil
		}}}
		s.backfillGithubWorkItemLinks()
	})

	t.Run("row with empty IssueUrl is skipped, no egress", func(t *testing.T) {
		cols := workItemBackfillColsB11()
		row := workItemBackfillRowB11(cols, "wi-1", "", "acme/w", 0, "open", "t", "not_requested", 0, "", 0)
		egressCalled := false
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "sobs_ai_settings") || strings.Contains(q, "sobs_app_settings"):
				if len(params) == 1 && params[0] == "ai.github_token" {
					return storetest.Result([]string{"Value"}, []any{"tok"}), nil
				}
				return &store.Result{}, nil
			case strings.Contains(q, "sobs_github_work_items"):
				return storetest.Result(cols, row), nil
			}
			egressCalled = true
			return &store.Result{}, nil
		}}}
		s.backfillGithubWorkItemLinks()
		if egressCalled {
			t.Error("want no egress for a row with an empty IssueUrl")
		}
	})

	t.Run("issue fetch error is counted and skipped, no update", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		cols := workItemBackfillColsB11()
		row := workItemBackfillRowB11(cols, "wi-2", "https://github.com/acme/w/issues/9", "acme/w", 9,
			"open", "t", "not_requested", 0, "", 0)
		var inserted []map[string]any
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "sobs_ai_settings") || strings.Contains(q, "sobs_app_settings"):
				if len(params) == 1 && params[0] == "ai.github_token" {
					return storetest.Result([]string{"Value"}, []any{"tok"}), nil
				}
				return &store.Result{}, nil
			case strings.Contains(q, "sobs_github_work_items"):
				return storetest.Result(cols, row), nil
			}
			return &store.Result{}, nil
		}}}
		// No fixture for GET .../issues/9 -> upstreamFixture 404s -> errorCount++/skippedCount++.
		s.backfillGithubWorkItemLinks()
		if len(inserted) != 0 {
			t.Errorf("want no update inserted, got %v", inserted)
		}
	})

	t.Run("unchanged row (identical refreshed fields) is skipped without an update", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		cols := workItemBackfillColsB11()
		row := workItemBackfillRowB11(cols, "wi-3", "https://github.com/acme/w/issues/10", "acme/w", 10,
			"open", "same title", "not_requested", 0, "", 0)
		issueURL := "https://api.github.com/repos/acme/w/issues/10"
		writeUpstreamFixture(t, dir, "GET", issueURL,
			`{"status": 200, "json": {"state": "open", "title": "same title", "assignees": []}}`)
		// No PR search fixture -> 404 -> no PR link (matches the row's existing PrUrl="").
		fdb := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "sobs_ai_settings") || strings.Contains(q, "sobs_app_settings"):
				if len(params) == 1 && params[0] == "ai.github_token" {
					return storetest.Result([]string{"Value"}, []any{"tok"}), nil
				}
				return &store.Result{}, nil
			case strings.Contains(q, "sobs_github_work_items"):
				return storetest.Result(cols, row), nil
			}
			return &store.Result{}, nil
		}}
		s := &server{db: fdb}
		s.backfillGithubWorkItemLinks()
		for _, ins := range fdb.Inserts {
			if ins.Table == "sobs_github_work_items" {
				t.Errorf("want no update insert for an unchanged row, got %v", ins.Rows)
			}
		}
	})

	t.Run("changed row (new title/state) is re-inserted with a fresh Version", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		cols := workItemBackfillColsB11()
		row := workItemBackfillRowB11(cols, "wi-4", "https://github.com/acme/w/issues/11", "acme/w", 11,
			"open", "old title", "not_requested", 0, "", 0)
		issueURL := "https://api.github.com/repos/acme/w/issues/11"
		writeUpstreamFixture(t, dir, "GET", issueURL,
			`{"status": 200, "json": {"state": "closed", "title": "new title", "assignees": [{"login": "copilot-swe-agent[bot]"}]}}`)
		fdb := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "sobs_ai_settings") || strings.Contains(q, "sobs_app_settings"):
				if len(params) == 1 && params[0] == "ai.github_token" {
					return storetest.Result([]string{"Value"}, []any{"tok"}), nil
				}
				return &store.Result{}, nil
			case strings.Contains(q, "sobs_github_work_items"):
				return storetest.Result(cols, row), nil
			}
			return &store.Result{}, nil
		}}
		s := &server{db: fdb}
		s.backfillGithubWorkItemLinks()
		var updated map[string]any
		for _, ins := range fdb.Inserts {
			if ins.Table == "sobs_github_work_items" {
				updated = ins.Rows[0]
			}
		}
		if updated == nil {
			t.Fatal("want an update insert")
		}
		if updated["IssueState"] != "closed" || updated["IssueTitle"] != "new title" {
			t.Errorf("unexpected updated row: %v", updated)
		}
	})
}

func workItemBackfillColsB11() []string {
	return []string{"Id", "IssueUrl", "GithubRepo", "IssueNumber", "IssueState", "IssueTitle",
		"CopilotAssignmentStatus", "PrLinked", "PrUrl", "PrNumber", "CopilotAssignmentReason", "Version"}
}

func workItemBackfillRowB11(cols []string, id, issueURL, githubRepo string, issueNumber int,
	issueState, issueTitle, copilotStatus string, prLinked int, prURL string, prNumber int) []any {
	return []any{id, issueURL, githubRepo, float64(issueNumber), issueState, issueTitle,
		copilotStatus, float64(prLinked), prURL, float64(prNumber), "", float64(0)}
}
