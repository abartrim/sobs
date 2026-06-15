package main

import (
	"reflect"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

func TestGithubAPIHeaders(t *testing.T) {
	got := githubAPIHeaders("ghp_tok", false, nil)
	want := map[string]string{
		"Authorization":        "Bearer ghp_tok",
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("base headers = %v, want %v", got, want)
	}

	got = githubAPIHeaders("ghp_tok", true, map[string]string{"GraphQL-Features": githubCopilotGraphqlFeatures})
	if got["Content-Type"] != "application/json" {
		t.Fatalf("include_content_type did not set Content-Type: %v", got)
	}
	if got["GraphQL-Features"] != githubCopilotGraphqlFeatures {
		t.Fatalf("extra header not merged: %v", got)
	}
	if got["Authorization"] != "Bearer ghp_tok" {
		t.Fatalf("Authorization lost when merging extras: %v", got)
	}
}

func TestGithubRequestHeadersTokenOrPublic(t *testing.T) {
	// With a token: full authenticated set.
	auth := githubRequestHeaders("ghp_tok", false)
	if auth["Authorization"] != "Bearer ghp_tok" {
		t.Fatalf("token branch missing Authorization: %v", auth)
	}
	// Without a token: public set, no Authorization (mirrors the import-repo / list-repos fallback).
	pub := githubRequestHeaders("   ", false)
	if _, ok := pub["Authorization"]; ok {
		t.Fatalf("public branch must not send Authorization: %v", pub)
	}
	if pub["Accept"] != "application/vnd.github+json" || pub["X-GitHub-Api-Version"] != "2022-11-28" {
		t.Fatalf("public branch headers wrong: %v", pub)
	}
}

func TestLabelsToAny(t *testing.T) {
	got := labelsToAny([]string{"sobs-onboarding", "ci-metadata"})
	want := []any{"sobs-onboarding", "ci-metadata"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("labelsToAny = %v, want %v", got, want)
	}
	if got := labelsToAny(nil); len(got) != 0 {
		t.Fatalf("labelsToAny(nil) = %v, want empty", got)
	}
}

// TestRequestBodyShapes guards the JSON payloads the runtime egress now sends, mirroring the
// Python httpx `json=` dicts (default separators). The body is not part of the parity fixture key,
// so these are runtime-only and parity-safe, but the structure must stay faithful to app.py.
func TestRequestBodyShapes(t *testing.T) {
	// OSV /v1/query — {package:{name,ecosystem}, version}.
	osv := jsonenc.NewObject().
		Set("package", jsonenc.NewObject().Set("name", "flask").Set("ecosystem", "PyPI")).
		Set("version", "2.0.1")
	if got := string(jsonenc.Encode(osv, dumpsDefault)); got != `{"package": {"name": "flask", "ecosystem": "PyPI"}, "version": "2.0.1"}` {
		t.Fatalf("OSV body = %s", got)
	}

	// GitHub create-issue — {title, body, labels}.
	issue := jsonenc.NewObject().
		Set("title", "T").Set("body", "B").Set("labels", labelsToAny([]string{"a", "b"}))
	if got := string(jsonenc.Encode(issue, dumpsDefault)); got != `{"title": "T", "body": "B", "labels": ["a", "b"]}` {
		t.Fatalf("issue body = %s", got)
	}

	// GraphQL copilot probe — {query, variables:{owner,name}}.
	gql := jsonenc.NewObject().
		Set("query", githubCopilotSupportQuery).
		Set("variables", jsonenc.NewObject().Set("owner", "acme").Set("name", "checkout"))
	gqlBytes := string(jsonenc.Encode(gql, dumpsDefault))
	wantPrefix := `{"query": "query($owner:String!, $name:String!) {`
	if len(gqlBytes) < len(wantPrefix) || gqlBytes[:len(wantPrefix)] != wantPrefix {
		t.Fatalf("graphql body prefix = %s", gqlBytes)
	}
	if want := `, "variables": {"owner": "acme", "name": "checkout"}}`; gqlBytes[len(gqlBytes)-len(want):] != want {
		t.Fatalf("graphql body suffix = %s", gqlBytes)
	}

	// Copilot assignment — {assignees, agent_assignment:{target_repo}}.
	assign := jsonenc.NewObject().
		Set("assignees", []any{githubCopilotAssignee}).
		Set("agent_assignment", jsonenc.NewObject().Set("target_repo", "acme/checkout"))
	if got := string(jsonenc.Encode(assign, dumpsDefault)); got != `{"assignees": ["copilot-swe-agent[bot]"], "agent_assignment": {"target_repo": "acme/checkout"}}` {
		t.Fatalf("assign body = %s", got)
	}
}
