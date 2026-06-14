package main

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// _inspect_repo_for_onboarding indicator sets (app.py): substrings that, in a workflow or
// manifest file, flag Sobs CI metadata / OpenTelemetry presence.
var sobsCIMetadataIndicators = []string{
	"sobs", "sobs-agent", "register_release", "release_artifacts",
	"sobs_release", "sobs/api/apps", "/api/releases",
}
var sobsCIOtelIndicators = []string{
	"opentelemetry", "otlp", "otel", "opentelemetry-sdk", "opentelemetry-api",
}

func containsAnyIndicator(lower string, indicators []string) bool {
	for _, ind := range indicators {
		if strings.Contains(lower, ind) {
			return true
		}
	}
	return false
}

// githubContentsURL mirrors the Contents API URL with urllib.parse.quote(path, safe="/"). The
// inspected paths (".github/workflows", "requirements.txt", …) contain no characters quote
// would escape, so this is the literal path.
func githubContentsURL(owner, repo, path string) string {
	return "https://api.github.com/repos/" + owner + "/" + repo + "/contents/" + path
}

// githubListDirectory mirrors app.py _github_list_directory.
func (s *server) githubListDirectory(owner, repo, path string) ([]any, string) {
	resp, err := s.upstreamGet("GET", githubContentsURL(owner, repo, path))
	if err != nil {
		return []any{}, "GitHub API request failed for " + path + ": " + err.Error()
	}
	if resp.Status != 200 {
		return []any{}, fmt.Sprintf("GitHub API returned %d for %s", resp.Status, path)
	}
	if list, ok := resp.Body.([]any); ok {
		return list, ""
	}
	return []any{}, ""
}

// decodeGithubContents mirrors app.py _decode_github_contents_payload (base64 content only).
func decodeGithubContents(payload *jsonenc.Object) []byte {
	cv, _ := payload.Get("content")
	content, ok := cv.(string)
	if !ok {
		return nil
	}
	ev, _ := payload.Get("encoding")
	enc, _ := ev.(string)
	if strings.ToLower(enc) != "base64" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content, "\n", ""))
	if err != nil {
		return nil
	}
	return b
}

// githubFileText mirrors app.py _github_file_text.
func (s *server) githubFileText(owner, repo, path string) (string, string) {
	resp, err := s.upstreamGet("GET", githubContentsURL(owner, repo, path))
	if err != nil {
		return "", "GitHub API request failed for " + path + ": " + err.Error()
	}
	if resp.Status != 200 {
		return "", fmt.Sprintf("GitHub API returned %d for %s", resp.Status, path)
	}
	obj, ok := resp.Body.(*jsonenc.Object)
	if !ok {
		return "", "Unexpected GitHub API response for " + path
	}
	return string(decodeGithubContents(obj)), ""
}

// githubRepoSupportsCopilot mirrors app.py _github_repo_supports_copilot_assignment: a GraphQL
// probe of suggestedActors for the copilot SWE agent.
func (s *server) githubRepoSupportsCopilot(ownerRepo string) bool {
	owner, repo := parseGithubRepoOwnerName(ownerRepo)
	if owner == "" || repo == "" {
		return false
	}
	resp, err := s.upstreamGet("POST", "https://api.github.com/graphql")
	if err != nil || resp.Status >= 400 {
		return false
	}
	obj, ok := resp.Body.(*jsonenc.Object)
	if !ok {
		return false
	}
	for _, n := range graphqlSuggestedActorNodes(obj) {
		node, ok := n.(*jsonenc.Object)
		if !ok {
			continue
		}
		login := strings.ToLower(strings.TrimSpace(objStrOr(node, "login")))
		if login == "copilot-swe-agent" || login == "copilot-swe-agent[bot]" {
			return true
		}
	}
	return false
}

func graphqlSuggestedActorNodes(obj *jsonenc.Object) []any {
	d, ok := objSub(obj, "data")
	if !ok {
		return nil
	}
	r, ok := objSub(d, "repository")
	if !ok {
		return nil
	}
	sa, ok := objSub(r, "suggestedActors")
	if !ok {
		return nil
	}
	nv, _ := sa.Get("nodes")
	nodes, _ := nv.([]any)
	return nodes
}

// has404 mirrors the Python `" 404 " in f" {msg} "` membership test.
func has404(msg string) bool { return strings.Contains(" "+msg+" ", " 404 ") }

// inspectRepoForOnboarding mirrors app.py _inspect_repo_for_onboarding.
func (s *server) inspectRepoForOnboarding(owner, repo string) *jsonenc.Object {
	mk := func(hasActions, ci, otel, copilot bool, files []any, errMsg string) *jsonenc.Object {
		return jsonenc.NewObject().
			Set("has_github_actions", hasActions).Set("sobs_ci_found", ci).
			Set("sobs_otel_found", otel).Set("copilot_available", copilot).
			Set("workflow_files", files).Set("error", errMsg)
	}
	entries, wfErr := s.githubListDirectory(owner, repo, ".github/workflows")
	if wfErr != "" && !has404(wfErr) {
		return mk(false, false, false, false, []any{}, wfErr)
	}
	workflowFiles := []any{}
	for _, e := range entries {
		eo, ok := e.(*jsonenc.Object)
		if !ok {
			continue
		}
		name := objStrOr(eo, "name")
		if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
			workflowFiles = append(workflowFiles, name)
		}
	}
	hasGithubActions := len(workflowFiles) > 0
	sobsCIFound, sobsOtelFound, inspectError := false, false, ""
	capped := workflowFiles
	if len(capped) > 10 {
		capped = capped[:10]
	}
	for _, fnAny := range capped {
		fn, _ := fnAny.(string)
		content, contentErr := s.githubFileText(owner, repo, ".github/workflows/"+fn)
		if contentErr != "" && inspectError == "" {
			inspectError = contentErr
			continue
		}
		lower := strings.ToLower(content)
		if !sobsCIFound && containsAnyIndicator(lower, sobsCIMetadataIndicators) {
			sobsCIFound = true
		}
		if !sobsOtelFound && containsAnyIndicator(lower, sobsCIOtelIndicators) {
			sobsOtelFound = true
		}
		if sobsCIFound && sobsOtelFound {
			break
		}
	}
	if !sobsOtelFound {
		for _, cp := range []string{"requirements.txt", "package.json", "go.mod", "pom.xml", "build.gradle"} {
			content, contentErr := s.githubFileText(owner, repo, cp)
			if contentErr != "" && !has404(contentErr) && inspectError == "" {
				inspectError = contentErr
			}
			if content != "" && containsAnyIndicator(strings.ToLower(content), sobsCIOtelIndicators) {
				sobsOtelFound = true
				break
			}
		}
	}
	copilotAvailable := s.githubRepoSupportsCopilot(owner + "/" + repo)
	return mk(hasGithubActions, sobsCIFound, sobsOtelFound, copilotAvailable, workflowFiles, inspectError)
}
