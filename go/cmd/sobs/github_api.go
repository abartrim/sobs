package main

import "strings"

// _github_api_headers constants (app.py): the Copilot assignee login and the GraphQL feature
// preview flags the copilot-support probe must opt into.
const (
	githubCopilotAssignee         = "copilot-swe-agent[bot]"
	githubCopilotGraphqlFeatures  = "issues_copilot_assignment_api_support,coding_agent_model_selection"
	githubIssueDedupeCandidateMax = 10 // _GITHUB_ISSUE_DEDUPE_CANDIDATE_LIMIT
)

// githubCopilotSupportQuery mirrors the GraphQL document in _github_repo_supports_copilot_assignment:
// probe suggestedActors(CAN_BE_ASSIGNED) for the copilot SWE agent. Whitespace is irrelevant to
// GitHub's GraphQL parser; this is the Python string reassembled.
const githubCopilotSupportQuery = "query($owner:String!, $name:String!) {" +
	" repository(owner:$owner, name:$name) {" +
	"  suggestedActors(capabilities:[CAN_BE_ASSIGNED], first:100) {" +
	"   nodes {" +
	"    __typename " +
	"    login " +
	"    ... on Bot { id } " +
	"    ... on User { id }" +
	"   }" +
	"  }" +
	" }" +
	"}"

// githubAPIHeaders mirrors app.py _github_api_headers: the authenticated GitHub REST/GraphQL header
// set. include_content_type adds the JSON content type for write requests; extra merges in
// per-call headers (e.g. the GraphQL feature flags). Under parity these headers are never sent —
// upstreamFixture serves canned responses keyed only by URL — so threading them is parity-safe and
// only affects the real-runtime egress path.
func githubAPIHeaders(token string, includeContentType bool, extra map[string]string) map[string]string {
	h := map[string]string{
		"Authorization":        "Bearer " + token,
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	if includeContentType {
		h["Content-Type"] = "application/json"
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// githubRequestHeaders mirrors the token-or-public branch used by the import-repo / list-repos
// routes: with a token, the full authenticated header set; without one, the public header set
// GitHub still honours for unauthenticated public-repo reads (no Authorization).
func githubRequestHeaders(token string, includeContentType bool) map[string]string {
	if strings.TrimSpace(token) != "" {
		return githubAPIHeaders(token, includeContentType, nil)
	}
	h := map[string]string{
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	if includeContentType {
		h["Content-Type"] = "application/json"
	}
	return h
}

// labelsToAny lifts a []string into the []any GitHub issue label payloads use.
func labelsToAny(labels []string) []any {
	out := make([]any, 0, len(labels))
	for _, l := range labels {
		out = append(out, l)
	}
	return out
}
