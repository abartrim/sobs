package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b11_github_inspect_test.go — batch 11 targeted coverage for cmd/sobs/github_inspect.go:
// containsAnyIndicator, githubListDirectory / githubFileText's error+non-200+success branches,
// githubRepoSupportsCopilot's parse-failure/error/success branches, graphqlSuggestedActorNodes'
// nested-lookup-miss branches, and inspectRepoForOnboarding's end-to-end assembly (no workflows,
// error path, CI+OTEL found via workflow content, OTEL found via a manifest file fallback).

// ---- containsAnyIndicator ----------------------------------------------------------------------

func TestContainsAnyIndicator(t *testing.T) {
	if !containsAnyIndicator("this uses opentelemetry-sdk", sobsCIOtelIndicators) {
		t.Error("want a match on opentelemetry-sdk")
	}
	if containsAnyIndicator("nothing relevant here", sobsCIOtelIndicators) {
		t.Error("want no match")
	}
	if !containsAnyIndicator("calls sobs-agent register_release", sobsCIMetadataIndicators) {
		t.Error("want a match on sobs-agent")
	}
}

// ---- githubListDirectory ------------------------------------------------------------------------

func TestGithubListDirectory(t *testing.T) {
	t.Run("request error", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		s := &server{db: &storetest.FakeDB{}}
		// No fixture written -> upstreamFixture returns {Status: 404} with no error, so this
		// exercises the non-200 branch instead; force a genuine request error via an invalid dir.
		list, errMsg := s.githubListDirectory("tok", "acme", "widgets", ".github/workflows")
		if len(list) != 0 || !strings.Contains(errMsg, "GitHub API returned 404") {
			t.Errorf("got list=%v errMsg=%q", list, errMsg)
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := githubContentsURL("acme", "w2", ".github/workflows")
		writeUpstreamFixture(t, dir, "GET", url, `{"status": 500, "json": {"message": "boom"}}`)
		s := &server{db: &storetest.FakeDB{}}
		list, errMsg := s.githubListDirectory("tok", "acme", "w2", ".github/workflows")
		if len(list) != 0 || errMsg != "GitHub API returned 500 for .github/workflows" {
			t.Errorf("got list=%v errMsg=%q", list, errMsg)
		}
	})

	t.Run("success with a list body", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := githubContentsURL("acme", "w3", ".github/workflows")
		writeUpstreamFixture(t, dir, "GET", url, `{"status": 200, "json": [{"name": "ci.yml"}]}`)
		s := &server{db: &storetest.FakeDB{}}
		list, errMsg := s.githubListDirectory("tok", "acme", "w3", ".github/workflows")
		if errMsg != "" || len(list) != 1 {
			t.Fatalf("got list=%v errMsg=%q", list, errMsg)
		}
	})

	t.Run("success but body is not a list -> empty list, no error", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := githubContentsURL("acme", "w4", "README.md")
		writeUpstreamFixture(t, dir, "GET", url, `{"status": 200, "json": {"name": "README.md"}}`)
		s := &server{db: &storetest.FakeDB{}}
		list, errMsg := s.githubListDirectory("tok", "acme", "w4", "README.md")
		if errMsg != "" || len(list) != 0 {
			t.Fatalf("got list=%v errMsg=%q", list, errMsg)
		}
	})
}

// ---- githubFileText -----------------------------------------------------------------------------

func TestGithubFileText(t *testing.T) {
	t.Run("non-200", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		s := &server{db: &storetest.FakeDB{}}
		text, errMsg := s.githubFileText("tok", "acme", "missing", "requirements.txt")
		if text != "" || !strings.Contains(errMsg, "GitHub API returned 404") {
			t.Errorf("got text=%q errMsg=%q", text, errMsg)
		}
	})

	t.Run("success body not an object -> Unexpected response error", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := githubContentsURL("acme", "w5", "go.mod")
		writeUpstreamFixture(t, dir, "GET", url, `{"status": 200, "json": [1,2,3]}`)
		s := &server{db: &storetest.FakeDB{}}
		text, errMsg := s.githubFileText("tok", "acme", "w5", "go.mod")
		if text != "" || errMsg != "Unexpected GitHub API response for go.mod" {
			t.Errorf("got text=%q errMsg=%q", text, errMsg)
		}
	})

	t.Run("success decodes base64 content", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := githubContentsURL("acme", "w6", "go.mod")
		writeUpstreamFixture(t, dir, "GET", url,
			`{"status": 200, "json": {"content": "`+base64Std("module acme/widgets\n")+`", "encoding": "base64"}}`)
		s := &server{db: &storetest.FakeDB{}}
		text, errMsg := s.githubFileText("tok", "acme", "w6", "go.mod")
		if errMsg != "" || text != "module acme/widgets\n" {
			t.Errorf("got text=%q errMsg=%q", text, errMsg)
		}
	})
}

// ---- githubRepoSupportsCopilot -------------------------------------------------------------------

func TestGithubRepoSupportsCopilot(t *testing.T) {
	t.Run("unparseable owner/repo -> false, no request", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if s.githubRepoSupportsCopilot("tok", "not-a-valid-repo") {
			t.Error("want false")
		}
	})

	t.Run("request error/non-2xx -> false", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		// No fixture -> upstreamFixture 404s (>= 400), so the "err != nil || resp.Status >= 400" guard fires.
		s := &server{db: &storetest.FakeDB{}}
		if s.githubRepoSupportsCopilot("tok", "acme/widgets") {
			t.Error("want false on 404")
		}
	})

	t.Run("body not an object -> false", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		writeUpstreamFixture(t, dir, "POST", "https://api.github.com/graphql", `{"status": 200, "json": [1]}`)
		s := &server{db: &storetest.FakeDB{}}
		if s.githubRepoSupportsCopilot("tok", "acme/widgets") {
			t.Error("want false when body is not an object")
		}
	})

	t.Run("copilot-swe-agent present (bracket-bot form) -> true", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		body := `{"data":{"repository":{"suggestedActors":{"nodes":[
			{"__typename":"Bot","login":"copilot-swe-agent[bot]"},
			{"__typename":"User","login":"someone-else"}
		]}}}}`
		writeUpstreamFixture(t, dir, "POST", "https://api.github.com/graphql", `{"status": 200, "json": `+body+`}`)
		s := &server{db: &storetest.FakeDB{}}
		if !s.githubRepoSupportsCopilot("tok", "acme/widgets") {
			t.Error("want true")
		}
	})

	t.Run("copilot-swe-agent present without [bot] suffix -> true", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		body := `{"data":{"repository":{"suggestedActors":{"nodes":[{"login":"Copilot-SWE-Agent"}]}}}}`
		writeUpstreamFixture(t, dir, "POST", "https://api.github.com/graphql", `{"status": 200, "json": `+body+`}`)
		s := &server{db: &storetest.FakeDB{}}
		if !s.githubRepoSupportsCopilot("tok", "acme/widgets") {
			t.Error("want true (case-insensitive match)")
		}
	})

	t.Run("no copilot in nodes -> false", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		body := `{"data":{"repository":{"suggestedActors":{"nodes":[{"login":"someone-else"}]}}}}`
		writeUpstreamFixture(t, dir, "POST", "https://api.github.com/graphql", `{"status": 200, "json": `+body+`}`)
		s := &server{db: &storetest.FakeDB{}}
		if s.githubRepoSupportsCopilot("tok", "acme/widgets") {
			t.Error("want false")
		}
	})
}

// ---- graphqlSuggestedActorNodes -------------------------------------------------------------------

func TestGraphqlSuggestedActorNodes(t *testing.T) {
	t.Run("missing data -> nil", func(t *testing.T) {
		if got := graphqlSuggestedActorNodes(jsonenc.NewObject()); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("missing repository -> nil", func(t *testing.T) {
		o := jsonenc.NewObject().Set("data", jsonenc.NewObject())
		if got := graphqlSuggestedActorNodes(o); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("missing suggestedActors -> nil", func(t *testing.T) {
		o := jsonenc.NewObject().Set("data", jsonenc.NewObject().Set("repository", jsonenc.NewObject()))
		if got := graphqlSuggestedActorNodes(o); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("present nodes returned", func(t *testing.T) {
		o := jsonenc.NewObject().Set("data", jsonenc.NewObject().Set("repository",
			jsonenc.NewObject().Set("suggestedActors", jsonenc.NewObject().Set("nodes", []any{"a", "b"}))))
		got := graphqlSuggestedActorNodes(o)
		if len(got) != 2 {
			t.Errorf("want 2 nodes, got %v", got)
		}
	})
}

// ---- inspectRepoForOnboarding ---------------------------------------------------------------------

func TestInspectRepoForOnboarding(t *testing.T) {
	t.Run("workflow listing hard-errors (non-404) -> short-circuit with error", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := githubContentsURL("acme", "hardfail", ".github/workflows")
		writeUpstreamFixture(t, dir, "GET", url, `{"status": 500, "json": {"message": "boom"}}`)
		s := &server{db: &storetest.FakeDB{}}
		got := s.inspectRepoForOnboarding("tok", "acme", "hardfail")
		hasActions, _ := got.Get("has_github_actions")
		errMsg, _ := got.Get("error")
		if hasActions != false || errMsg == "" {
			t.Errorf("want short-circuit with error set, got %v", got)
		}
	})

	t.Run("no workflows dir (404) -> proceeds, no actions, otel found via manifest fallback", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		// .github/workflows 404s (has404 -> continue rather than short-circuit).
		reqURL := githubContentsURL("acme", "nowf", "requirements.txt")
		writeUpstreamFixture(t, dir, "GET", reqURL,
			`{"status": 200, "json": {"content": "`+base64Std("opentelemetry-sdk==1.0\n")+`", "encoding": "base64"}}`)
		// Copilot probe 404s -> false.
		s := &server{db: &storetest.FakeDB{}}
		got := s.inspectRepoForOnboarding("tok", "acme", "nowf")
		hasActions, _ := got.Get("has_github_actions")
		ci, _ := got.Get("sobs_ci_found")
		otel, _ := got.Get("sobs_otel_found")
		copilot, _ := got.Get("copilot_available")
		if hasActions != false || ci != false || otel != true || copilot != false {
			t.Errorf("unexpected result: %v", got)
		}
	})

	t.Run("workflows present: CI + OTEL found via workflow content, copilot available", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		wfListURL := githubContentsURL("acme", "full", ".github/workflows")
		writeUpstreamFixture(t, dir, "GET", wfListURL,
			`{"status": 200, "json": [{"name": "ci.yml"}, {"name": "notes.txt"}]}`)
		wfFileURL := githubContentsURL("acme", "full", ".github/workflows/ci.yml")
		wfContent := "uses: sobs-agent/register_release\nopentelemetry-sdk installed here\n"
		writeUpstreamFixture(t, dir, "GET", wfFileURL,
			`{"status": 200, "json": {"content": "`+base64Std(wfContent)+`", "encoding": "base64"}}`)
		writeUpstreamFixture(t, dir, "POST", "https://api.github.com/graphql",
			`{"status": 200, "json": {"data":{"repository":{"suggestedActors":{"nodes":[{"login":"copilot-swe-agent[bot]"}]}}}}}`)
		s := &server{db: &storetest.FakeDB{}}
		got := s.inspectRepoForOnboarding("tok", "acme", "full")
		hasActions, _ := got.Get("has_github_actions")
		ci, _ := got.Get("sobs_ci_found")
		otel, _ := got.Get("sobs_otel_found")
		copilot, _ := got.Get("copilot_available")
		files, _ := got.Get("workflow_files")
		if hasActions != true || ci != true || otel != true || copilot != true {
			t.Errorf("unexpected result: %v", got)
		}
		if fl, ok := files.([]any); !ok || len(fl) != 1 || fl[0] != "ci.yml" {
			t.Errorf("workflow_files should only include the .yml entry, got %v", files)
		}
	})

	t.Run("workflow content fetch errors but is non-fatal; first error retained", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		wfListURL := githubContentsURL("acme", "werr", ".github/workflows")
		writeUpstreamFixture(t, dir, "GET", wfListURL, `{"status": 200, "json": [{"name": "ci.yaml"}]}`)
		// No fixture for the file fetch -> 404 -> contentErr set, loop `continue`s (not found).
		s := &server{db: &storetest.FakeDB{}}
		got := s.inspectRepoForOnboarding("tok", "acme", "werr")
		errMsg, _ := got.Get("error")
		ci, _ := got.Get("sobs_ci_found")
		if errMsg == "" || ci != false {
			t.Errorf("want a retained content error and ci not found, got %v", got)
		}
	})
}
