package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b11_onboarding_issues_test.go — batch 11 targeted coverage for
// cmd/sobs/onboarding_issues.go: buildOnboardingIssueResult's error/copilot-assign/persist
// branches, createOrUpdateOnboardingIssue's create/reuse/update/leave-unchanged paths,
// githubGetIssueDetail's guard+error+success branches, githubIssueIsNewState's every boolean
// combination, updateGithubIssueRecord's guard/error/success branches, extractIssueAssigneeLogins,
// assignIssueToCopilot's guard/blocked/failed/success/ambiguous branches, persistOnboardingWorkItem's
// no-op-on-empty-url branch, findAppIDByRepoURL, and the small ciStatus*/mapInt helpers.

// ---- buildOnboardingIssueResult -------------------------------------------------------------

func TestBuildOnboardingIssueResult(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}

	t.Run("error result short-circuits", func(t *testing.T) {
		got := s.buildOnboardingIssueResult(map[string]any{"error": "boom"}, false, "tok", "acme/w", "ci", "w", "title")
		errMsg, _ := got.Get("error")
		if errMsg != "boom" {
			t.Errorf("want error passthrough, got %v", got)
		}
	})

	t.Run("created issue with no copilot request persists the work item", func(t *testing.T) {
		fdb := &storetest.FakeDB{}
		s2 := &server{db: fdb}
		res := map[string]any{
			"issue_url": "https://github.com/acme/w/issues/7", "issue_number": 7,
			"issue_title": "My Title", "issue_state": "open", "status": "created", "note": "Created a new onboarding issue.",
		}
		got := s2.buildOnboardingIssueResult(res, false, "tok", "acme/w", "ci", "w", "default title")
		url, _ := got.Get("url")
		num, _ := got.Get("number")
		cstatus, _ := got.Get("copilot_status")
		if url != "https://github.com/acme/w/issues/7" || num != 7 || cstatus != "not_requested" {
			t.Fatalf("unexpected result: %v", got)
		}
		if len(fdb.Inserts) != 1 || fdb.Inserts[0].Table != "sobs_github_work_items" {
			t.Fatalf("want a persisted work item, got inserts=%v", fdb.Inserts)
		}
	})

	t.Run("reused status does not persist a work item", func(t *testing.T) {
		fdb := &storetest.FakeDB{}
		s2 := &server{db: fdb}
		res := map[string]any{
			"issue_url": "https://github.com/acme/w/issues/8", "issue_number": 8,
			"issue_state": "open", "status": "reused", "note": "unchanged",
		}
		_ = s2.buildOnboardingIssueResult(res, false, "tok", "acme/w", "ci", "w", "default title")
		if len(fdb.Inserts) != 0 {
			t.Fatalf("want no persisted work item on reused status, got %v", fdb.Inserts)
		}
	})

	t.Run("assignCopilot true with a nonzero issue number requests copilot assignment", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		// githubRepoSupportsCopilot's GraphQL probe: no fixture -> 404 -> "blocked" outcome.
		s2 := &server{db: &storetest.FakeDB{}}
		res := map[string]any{
			"issue_url": "https://github.com/acme/w/issues/9", "issue_number": 9,
			"issue_state": "open", "status": "created", "note": "n",
		}
		got := s2.buildOnboardingIssueResult(res, true, "tok", "acme/w", "ci", "w", "default title")
		cstatus, _ := got.Get("copilot_status")
		if cstatus != "blocked" {
			t.Fatalf("want blocked (no copilot support fixture), got %v", got)
		}
	})

	t.Run("empty title/state fall back to defaults", func(t *testing.T) {
		fdb := &storetest.FakeDB{}
		s2 := &server{db: fdb}
		res := map[string]any{
			"issue_url": "https://github.com/acme/w/issues/10", "issue_number": 10,
			"status": "updated", "note": "n",
		}
		_ = s2.buildOnboardingIssueResult(res, false, "tok", "acme/w", "ci", "w", "the default title")
		if len(fdb.Inserts) != 1 {
			t.Fatalf("want a persisted work item, got %v", fdb.Inserts)
		}
		row := fdb.Inserts[0].Rows[0]
		if row["IssueTitle"] != "the default title" || row["IssueState"] != "open" {
			t.Errorf("want default title+state, got %v", row)
		}
	})
}

// ---- createOrUpdateOnboardingIssue -----------------------------------------------------------

func TestCreateOrUpdateOnboardingIssue(t *testing.T) {
	t.Run("no existing open issue -> create", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		githubIssuesListFixtureB11(t, dir, "acme/new1", `[]`)
		createURL := "https://api.github.com/repos/acme/new1/issues"
		writeUpstreamFixture(t, dir, "POST", createURL,
			`{"status": 201, "json": {"html_url": "https://github.com/acme/new1/issues/1", "number": 1, "title": "T", "state": "open"}}`)
		s := &server{db: &storetest.FakeDB{}}
		got := s.createOrUpdateOnboardingIssue("tok", "acme/new1", "T", "body", []string{"a"})
		if got["status"] != "created" || got["note"] != "Created a new onboarding issue." {
			t.Fatalf("unexpected: %v", got)
		}
	})

	t.Run("existing issue in new state -> update", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		githubIssuesListFixtureB11(t, dir, "acme/upd1", `[{"number": 3, "html_url": "https://github.com/acme/upd1/issues/3", "title": "T", "state": "open"}]`)
		detailURL := "https://api.github.com/repos/acme/upd1/issues/3"
		writeUpstreamFixture(t, dir, "GET", detailURL,
			`{"status": 200, "json": {"state": "open", "comments": 0, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}}`)
		writeUpstreamFixture(t, dir, "PATCH", detailURL,
			`{"status": 200, "json": {"html_url": "https://github.com/acme/upd1/issues/3", "number": 3, "title": "T", "state": "open"}}`)
		s := &server{db: &storetest.FakeDB{}}
		got := s.createOrUpdateOnboardingIssue("tok", "acme/upd1", "T", "body", []string{"a"})
		if got["status"] != "updated" {
			t.Fatalf("unexpected: %v", got)
		}
	})

	t.Run("existing issue not in new state -> reused, prefers detail payload", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		githubIssuesListFixtureB11(t, dir, "acme/reuse1", `[{"number": 4, "html_url": "https://github.com/acme/reuse1/issues/4", "title": "T", "state": "open"}]`)
		detailURL := "https://api.github.com/repos/acme/reuse1/issues/4"
		writeUpstreamFixture(t, dir, "GET", detailURL,
			`{"status": 200, "json": {"state": "open", "comments": 3, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z", "html_url": "https://github.com/acme/reuse1/issues/4", "title": "Detail Title"}}`)
		s := &server{db: &storetest.FakeDB{}}
		got := s.createOrUpdateOnboardingIssue("tok", "acme/reuse1", "T", "body", []string{"a"})
		if got["status"] != "reused" || got["issue_title"] != "Detail Title" {
			t.Fatalf("unexpected: %v", got)
		}
	})

	t.Run("create fails -> error passthrough", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		githubIssuesListFixtureB11(t, dir, "acme/fail1", `[]`)
		createURL := "https://api.github.com/repos/acme/fail1/issues"
		writeUpstreamFixture(t, dir, "POST", createURL, `{"status": 422, "json": {"message": "nope"}}`)
		s := &server{db: &storetest.FakeDB{}}
		got := s.createOrUpdateOnboardingIssue("tok", "acme/fail1", "T", "body", []string{"a"})
		if _, ok := got["error"]; !ok {
			t.Fatalf("want an error, got %v", got)
		}
	})

	t.Run("update fails -> error passthrough", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		githubIssuesListFixtureB11(t, dir, "acme/updfail", `[{"number": 5, "html_url": "https://github.com/acme/updfail/issues/5", "title": "T", "state": "open"}]`)
		detailURL := "https://api.github.com/repos/acme/updfail/issues/5"
		writeUpstreamFixture(t, dir, "GET", detailURL,
			`{"status": 200, "json": {"state": "open", "comments": 0, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}}`)
		writeUpstreamFixture(t, dir, "PATCH", detailURL, `{"status": 500, "json": {"message": "server error"}}`)
		s := &server{db: &storetest.FakeDB{}}
		got := s.createOrUpdateOnboardingIssue("tok", "acme/updfail", "T", "body", []string{"a"})
		if _, ok := got["error"]; !ok {
			t.Fatalf("want an error, got %v", got)
		}
	})
}

func githubIssuesListFixtureB11(t *testing.T, dir, repo, jsonBody string) {
	t.Helper()
	url := "https://api.github.com/repos/" + repo + "/issues?state=open&per_page=" + itoaInt(githubIssueDedupeCandidateMax)
	writeUpstreamFixture(t, dir, "GET", url, `{"status": 200, "json": `+jsonBody+`}`)
}

// ---- githubGetIssueDetail ----------------------------------------------------------------------

func TestGithubGetIssueDetail(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	t.Run("guard: missing token", func(t *testing.T) {
		if got := s.githubGetIssueDetail("", "acme/w", 1); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("guard: missing repo", func(t *testing.T) {
		if got := s.githubGetIssueDetail("tok", "", 1); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("guard: non-positive issue number", func(t *testing.T) {
		if got := s.githubGetIssueDetail("tok", "acme/w", 0); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("guard: unparseable repo", func(t *testing.T) {
		if got := s.githubGetIssueDetail("tok", "not-a-repo", 1); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("error/non-2xx -> nil", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		// No fixture -> 404 (>= 300) -> nil.
		if got := s.githubGetIssueDetail("tok", "acme/w", 5); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("success returns the parsed object", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := "https://api.github.com/repos/acme/w/issues/6"
		writeUpstreamFixture(t, dir, "GET", url, `{"status": 200, "json": {"state": "open", "number": 6}}`)
		got := s.githubGetIssueDetail("tok", "acme/w", 6)
		if got == nil {
			t.Fatal("want a non-nil object")
		}
		if v, _ := got.Get("state"); v != "open" {
			t.Errorf("unexpected detail: %v", got)
		}
	})
}

// ---- githubIssueIsNewState --------------------------------------------------------------------

func TestGithubIssueIsNewState(t *testing.T) {
	t.Run("nil -> false", func(t *testing.T) {
		if githubIssueIsNewState(nil) {
			t.Error("want false")
		}
	})
	t.Run("all conditions met -> true", func(t *testing.T) {
		o := jsonenc.NewObject().Set("state", "OPEN").Set("comments", 0).
			Set("created_at", "2026-01-01T00:00:00Z").Set("updated_at", "2026-01-01T00:00:00Z")
		if !githubIssueIsNewState(o) {
			t.Error("want true")
		}
	})
	t.Run("closed state -> false", func(t *testing.T) {
		o := jsonenc.NewObject().Set("state", "closed").Set("comments", 0).
			Set("created_at", "t").Set("updated_at", "t")
		if githubIssueIsNewState(o) {
			t.Error("want false")
		}
	})
	t.Run("has comments -> false", func(t *testing.T) {
		o := jsonenc.NewObject().Set("state", "open").Set("comments", 2.0).
			Set("created_at", "t").Set("updated_at", "t")
		if githubIssueIsNewState(o) {
			t.Error("want false")
		}
	})
	t.Run("empty created_at -> false", func(t *testing.T) {
		o := jsonenc.NewObject().Set("state", "open").Set("comments", 0).
			Set("created_at", "").Set("updated_at", "")
		if githubIssueIsNewState(o) {
			t.Error("want false")
		}
	})
	t.Run("created != updated -> false", func(t *testing.T) {
		o := jsonenc.NewObject().Set("state", "open").Set("comments", 0).
			Set("created_at", "t1").Set("updated_at", "t2")
		if githubIssueIsNewState(o) {
			t.Error("want false")
		}
	})
}

// ---- updateGithubIssueRecord --------------------------------------------------------------------

func TestUpdateGithubIssueRecord(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	t.Run("guards return empty map", func(t *testing.T) {
		if got := s.updateGithubIssueRecord("", "acme/w", 1, "t", "b", nil, false); len(got) != 0 {
			t.Errorf("want empty, got %v", got)
		}
		if got := s.updateGithubIssueRecord("tok", "", 1, "t", "b", nil, false); len(got) != 0 {
			t.Errorf("want empty, got %v", got)
		}
		if got := s.updateGithubIssueRecord("tok", "acme/w", 0, "t", "b", nil, false); len(got) != 0 {
			t.Errorf("want empty, got %v", got)
		}
		if got := s.updateGithubIssueRecord("tok", "not-a-repo", 1, "t", "b", nil, false); len(got) != 0 {
			t.Errorf("want empty, got %v", got)
		}
	})

	t.Run("error response surfaces the message detail", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := "https://api.github.com/repos/acme/w/issues/2"
		writeUpstreamFixture(t, dir, "PATCH", url, `{"status": 422, "json": {"message": "validation failed"}}`)
		got := s.updateGithubIssueRecord("tok", "acme/w", 2, "t", "b", []string{"x"}, false)
		errMsg, _ := got["error"].(string)
		if !strings.Contains(errMsg, "validation failed") {
			t.Errorf("unexpected: %v", got)
		}
	})

	t.Run("error response with no message detail falls back to 'request failed'", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := "https://api.github.com/repos/acme/w2/issues/3"
		writeUpstreamFixture(t, dir, "PATCH", url, `{"status": 500, "json": {}}`)
		got := s.updateGithubIssueRecord("tok", "acme/w2", 3, "t", "b", nil, false)
		errMsg, _ := got["error"].(string)
		if !strings.Contains(errMsg, "request failed") {
			t.Errorf("unexpected: %v", got)
		}
	})

	t.Run("success with masking applied, empty response falls back to request fields", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := "https://api.github.com/repos/acme/w3/issues/4"
		writeUpstreamFixture(t, dir, "PATCH", url, `{"status": 200, "json": {}}`)
		got := s.updateGithubIssueRecord("tok", "acme/w3", 4, "My Title", "My Body", []string{"x"}, true)
		if got["issue_number"] != 4 || got["issue_title"] != "My Title" || got["issue_state"] != "open" {
			t.Errorf("unexpected: %v", got)
		}
	})

	t.Run("success with response payload overrides", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := "https://api.github.com/repos/acme/w4/issues/5"
		writeUpstreamFixture(t, dir, "PATCH", url,
			`{"status": 200, "json": {"number": 9, "title": "Resp Title", "state": "closed", "html_url": "https://github.com/acme/w4/issues/9"}}`)
		got := s.updateGithubIssueRecord("tok", "acme/w4", 5, "t", "b", nil, false)
		if got["issue_number"] != 9 || got["issue_title"] != "Resp Title" || got["issue_state"] != "closed" {
			t.Errorf("unexpected: %v", got)
		}
	})
}

// ---- extractIssueAssigneeLogins ------------------------------------------------------------------

func TestExtractIssueAssigneeLogins(t *testing.T) {
	t.Run("missing assignees -> nil", func(t *testing.T) {
		if got := extractIssueAssigneeLogins(jsonenc.NewObject()); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("assignees not a list -> nil", func(t *testing.T) {
		o := jsonenc.NewObject().Set("assignees", "nope")
		if got := extractIssueAssigneeLogins(o); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("mixed list extracts logins, skips non-objects", func(t *testing.T) {
		o := jsonenc.NewObject().Set("assignees", []any{
			jsonenc.NewObject().Set("login", "alice"), "not-an-object", jsonenc.NewObject().Set("login", "bob"),
		})
		got := extractIssueAssigneeLogins(o)
		if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
			t.Errorf("unexpected: %v", got)
		}
	})
}

// ---- assignIssueToCopilot -----------------------------------------------------------------------

func TestAssignIssueToCopilot(t *testing.T) {
	t.Run("guard: missing token/repo/issue number", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		status, reason, at := s.assignIssueToCopilot("", "acme/w", 1, "", "")
		if status != "blocked" || at != 0 || !strings.Contains(reason, "missing GitHub token") {
			t.Errorf("unexpected: %v %v %v", status, reason, at)
		}
	})

	t.Run("copilot support probe fails -> blocked", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		// No fixture for the copilot GraphQL probe -> 404 -> false.
		s := &server{db: &storetest.FakeDB{}}
		status, reason, _ := s.assignIssueToCopilot("tok", "acme/w", 5, "", "")
		if status != "blocked" || !strings.Contains(reason, "Copilot cloud agent is not enabled") {
			t.Errorf("unexpected: %v %v", status, reason)
		}
	})

	t.Run("invalid repo target after copilot support (unreachable in practice, still guarded)", func(t *testing.T) {
		// githubRepoSupportsCopilot itself parses owner/repo and would already return false for an
		// unparseable target, so this exercises the guard defensively via a direct call shape.
		s := &server{db: &storetest.FakeDB{}}
		status, reason, at := s.assignIssueToCopilot("tok", "not-a-repo", 1, "", "")
		if status != "blocked" || at != 0 {
			t.Errorf("unexpected: %v %v %v", status, reason, at)
		}
	})

	t.Run("assignment request fails (>=300) -> failed with truncated detail", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		writeUpstreamFixture(t, dir, "POST", "https://api.github.com/graphql",
			`{"status": 200, "json": {"data":{"repository":{"suggestedActors":{"nodes":[{"login":"copilot-swe-agent[bot]"}]}}}}}`)
		assignURL := "https://api.github.com/repos/acme/w/issues/7/assignees"
		writeUpstreamFixture(t, dir, "POST", assignURL, `{"status": 422, "content": "Validation failed: bad assignee"}`)
		s := &server{db: &storetest.FakeDB{}}
		status, reason, at := s.assignIssueToCopilot("tok", "acme/w", 7, "", "")
		if status != "failed" || reason != "Validation failed: bad assignee" || at == 0 {
			t.Errorf("unexpected: %v %v %v", status, reason, at)
		}
	})

	t.Run("assignment request fails with empty content -> generic message", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		writeUpstreamFixture(t, dir, "POST", "https://api.github.com/graphql",
			`{"status": 200, "json": {"data":{"repository":{"suggestedActors":{"nodes":[{"login":"copilot-swe-agent[bot]"}]}}}}}`)
		assignURL := "https://api.github.com/repos/acme/w2/issues/8/assignees"
		writeUpstreamFixture(t, dir, "POST", assignURL, `{"status": 500, "json": {}}`)
		s := &server{db: &storetest.FakeDB{}}
		status, reason, _ := s.assignIssueToCopilot("tok", "acme/w2", 8, "", "")
		if status != "failed" || reason != "Copilot issue assignment failed" {
			t.Errorf("unexpected: %v %v", status, reason)
		}
	})

	t.Run("success with the assignee visible in response -> requested", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		writeUpstreamFixture(t, dir, "POST", "https://api.github.com/graphql",
			`{"status": 200, "json": {"data":{"repository":{"suggestedActors":{"nodes":[{"login":"copilot-swe-agent[bot]"}]}}}}}`)
		assignURL := "https://api.github.com/repos/acme/w3/issues/9/assignees"
		writeUpstreamFixture(t, dir, "POST", assignURL,
			`{"status": 200, "json": {"assignees": [{"login": "copilot-swe-agent[bot]"}]}}`)
		s := &server{db: &storetest.FakeDB{}}
		status, reason, at := s.assignIssueToCopilot("tok", "acme/w3", 9, "main", "custom instructions")
		if status != "requested" || reason != "Copilot assignment requested" || at == 0 {
			t.Errorf("unexpected: %v %v %v", status, reason, at)
		}
	})

	t.Run("success but assignee not yet visible -> requested with a lag caveat", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		writeUpstreamFixture(t, dir, "POST", "https://api.github.com/graphql",
			`{"status": 200, "json": {"data":{"repository":{"suggestedActors":{"nodes":[{"login":"copilot-swe-agent[bot]"}]}}}}}`)
		assignURL := "https://api.github.com/repos/acme/w4/issues/10/assignees"
		writeUpstreamFixture(t, dir, "POST", assignURL, `{"status": 200, "json": {"assignees": []}}`)
		s := &server{db: &storetest.FakeDB{}}
		status, reason, _ := s.assignIssueToCopilot("tok", "acme/w4", 10, "", "")
		if status != "requested" || !strings.Contains(reason, "visibility may lag briefly") {
			t.Errorf("unexpected: %v %v", status, reason)
		}
	})
}

// ---- persistOnboardingWorkItem -------------------------------------------------------------------

func TestPersistOnboardingWorkItem(t *testing.T) {
	t.Run("empty issue URL is a no-op", func(t *testing.T) {
		fdb := &storetest.FakeDB{}
		s := &server{db: fdb}
		s.persistOnboardingWorkItem("acme/w", "", 0, "t", "open", "new_issue", "n", "not_requested", "", 0, "ci", "w")
		if len(fdb.Inserts) != 0 {
			t.Fatalf("want no insert, got %v", fdb.Inserts)
		}
	})

	t.Run("reused decision sets full dedup confidence", func(t *testing.T) {
		fdb := &storetest.FakeDB{}
		s := &server{db: fdb}
		s.persistOnboardingWorkItem("acme/w", "https://github.com/acme/w/issues/1", 1, "t", "open",
			"reused", "n", "requested", "r", 1234, "ci", "w")
		if len(fdb.Inserts) != 1 {
			t.Fatalf("want 1 insert, got %v", fdb.Inserts)
		}
		row := fdb.Inserts[0].Rows[0]
		if row["DedupConfidence"] != 1.0 || row["DedupDecision"] != "reused" {
			t.Errorf("unexpected row: %v", row)
		}
	})

	t.Run("empty dedup decision defaults to new_issue", func(t *testing.T) {
		fdb := &storetest.FakeDB{}
		s := &server{db: fdb}
		s.persistOnboardingWorkItem("acme/w", "https://github.com/acme/w/issues/2", 2, "t", "", "",
			"n", "", "", 0, "observability", "w")
		row := fdb.Inserts[0].Rows[0]
		if row["DedupDecision"] != "new_issue" || row["IssueState"] != "open" || row["CopilotAssignmentStatus"] != "not_requested" {
			t.Errorf("unexpected row: %v", row)
		}
		if row["AgentAction"] != "onboarding_observability" {
			t.Errorf("unexpected AgentAction: %v", row["AgentAction"])
		}
	})
}

// ---- findAppIDByRepoURL --------------------------------------------------------------------------

func TestFindAppIDByRepoURL(t *testing.T) {
	t.Run("empty repo URL -> empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.findAppIDByRepoURL(""); got != "" {
			t.Errorf("want empty, got %q", got)
		}
	})
	t.Run("unparseable repo URL -> empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.findAppIDByRepoURL("not-a-url"); got != "" {
			t.Errorf("want empty, got %q", got)
		}
	})
	t.Run("query error -> empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, assertErr("boom") // shared helper from cov95_b7_ai_helper_context_test.go
		}}}
		if got := s.findAppIDByRepoURL("https://github.com/acme/w"); got != "" {
			t.Errorf("want empty, got %q", got)
		}
	})
	t.Run("case-insensitive match found", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_apps") {
				return storetest.Result([]string{"Id", "RepoUrl"}, []any{"app-9", "https://github.com/ACME/Widgets"}), nil
			}
			return &store.Result{}, nil
		}}}
		if got := s.findAppIDByRepoURL("https://github.com/acme/widgets"); got != "app-9" {
			t.Errorf("want app-9, got %q", got)
		}
	})
	t.Run("no match found -> empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_apps") {
				return storetest.Result([]string{"Id", "RepoUrl"}, []any{"app-1", "https://github.com/other/repo"}), nil
			}
			return &store.Result{}, nil
		}}}
		if got := s.findAppIDByRepoURL("https://github.com/acme/widgets"); got != "" {
			t.Errorf("want empty, got %q", got)
		}
	})
}

// ---- ciStatus*/mapInt small helpers ----------------------------------------------------------

func TestCiStatusHelpersAndMapInt(t *testing.T) {
	status := map[string]any{
		"configured": true,
		"expiry":     map[string]any{"state": "expired", "message": "msg"},
	}
	if !ciStatusBool(status, "configured") {
		t.Error("want true")
	}
	if ciStatusBool(status, "missing") {
		t.Error("want false for missing key")
	}
	if ciStatusExpiry(status, "state") != "expired" {
		t.Error("want expired")
	}
	if ciStatusExpiry(map[string]any{}, "state") != "" {
		t.Error("want empty when no expiry map")
	}
	// toStr(true) mirrors Python's str(True) -> "True" (mutation_helpers.go's bool branch).
	if ciStatusStr(status, "configured") != "True" {
		t.Errorf("got %q", ciStatusStr(status, "configured"))
	}
	if mapInt(map[string]any{"n": 5}, "n") != 5 {
		t.Error("want int passthrough")
	}
	if mapInt(map[string]any{"n": 5.0}, "n") != 5 {
		t.Error("want float64 coercion")
	}
	if mapInt(map[string]any{"n": "5"}, "n") != 0 {
		t.Error("want 0 for unsupported type")
	}
}
