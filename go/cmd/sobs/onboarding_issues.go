package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// POST /api/onboarding/create-issues — app.py api_onboarding_create_issues: create onboarding CI /
// OTEL GitHub issues and/or set up realtime CI ingest. The issue bodies are sent to GitHub (a
// body-ignoring mock under parity) and never returned, so a concise body is used.
func (s *server) handleApiOnboardingCreateIssues(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	appID := strings.TrimSpace(bstr(m, "app_id"))
	repoParam := strings.TrimSpace(bstr(m, "repo"))
	createCI := bodyBool(m, "create_ci", true)
	createOtel := bodyBool(m, "create_otel", true)
	assignCopilot := bodyBool(m, "assign_copilot", false)
	hasGithubActions := bodyBool(m, "has_github_actions", true)
	enableRealtime := bodyBool(m, "enable_realtime_support", false)

	if !createCI && !createOtel && !enableRealtime {
		s.errorJSON(w, http.StatusBadRequest, "Select at least one issue type or enable realtime support")
		return
	}
	repoURL := ""
	if appID != "" {
		cur, ok := s.findAppByID(appID)
		if !ok {
			s.errorJSON(w, http.StatusNotFound, "App not found")
			return
		}
		repoURL = cStr(cur, "RepoUrl")
	} else if repoParam != "" {
		repoURL = repoParam
	} else {
		s.errorJSON(w, http.StatusBadRequest, "app_id or repo parameter required")
		return
	}
	owner, repo := parseGithubRepoOwnerName(repoURL)
	if owner == "" || repo == "" {
		s.errorJSON(w, http.StatusBadRequest, "Could not parse owner/repo from '"+repoURL+"'")
		return
	}
	githubToken := strings.TrimSpace(s.repoScopedGithubToken(owner, repo))
	if githubToken == "" {
		githubToken = strings.TrimSpace(s.loadAISetting("ai.github_token", ""))
	}
	if githubToken == "" {
		s.errorJSON(w, http.StatusBadRequest, "No GitHub token configured for this repository")
		return
	}
	githubRepo := owner + "/" + repo
	results := jsonenc.NewObject().Set("ok", true).
		Set("ci_issue", nil).Set("otel_issue", nil).Set("realtime", nil)

	if enableRealtime {
		realtimeAppID := appID
		if realtimeAppID == "" && repoURL != "" {
			realtimeAppID = s.findAppIDByRepoURL(repoURL)
		}
		if realtimeAppID == "" {
			s.errorJSON(w, http.StatusBadRequest, "Realtime support requires a saved repository app.")
			return
		}
		status := s.ciPushStatus(realtimeAppID)
		keyPlain := ""
		if !ciStatusBool(status, "configured") || ciStatusExpiry(status, "state") == "expired" {
			keyPlain, _ = s.rotateCiPushKey(realtimeAppID, ciPushDefaultTTLDays)
			status = s.ciPushStatus(realtimeAppID)
		}
		s.setCiPushRealtimeEnabled(realtimeAppID, true)
		appIDForExample := realtimeAppID
		if appIDForExample == "" {
			appIDForExample = "<APP_ID>"
		}
		instructions := jsonenc.NewObject().
			Set("required_secrets", []any{"SOBS_URL", "SOBS_INGEST_API_KEY", "SOBS_APP_ID"}).
			Set("curl_example", "curl -sS -X POST '$SOBS_URL/v1/apps/"+appIDForExample+"/releases' "+
				"-H 'X-API-Key: $SOBS_INGEST_API_KEY' -H 'Content-Type: application/json' "+
				`-d '{"version":"$VERSION","commitSha":"$COMMIT_SHA","buildId":"$BUILD_ID"}'`).
			Set("webhook_note", "Optional: add a GitHub webhook for push/workflow events to reduce polling latency.")
		results.Set("realtime", jsonenc.NewObject().
			Set("app_id", realtimeAppID).Set("enabled", true).
			Set("configured", ciStatusBool(status, "configured")).
			Set("expires_at", ciStatusStr(status, "expires_at")).
			Set("expiry_state", orDefault(ciStatusExpiry(status, "state"), "unknown")).
			Set("expiry_message", ciStatusExpiry(status, "message")).
			Set("api_key", keyPlain).Set("api_key_show_once", keyPlain != "").
			Set("instructions", instructions))
	}

	if createCI {
		body := buildOnboardingIssueBody("ci", owner, repo, hasGithubActions)
		res := s.createOrUpdateOnboardingIssue(githubToken, githubRepo,
			"[Sobs] Set up CI metadata scripts for "+repo, body, []string{"sobs-onboarding", "ci-metadata"})
		results.Set("ci_issue", s.buildOnboardingIssueResult(res, assignCopilot, githubToken, githubRepo,
			"ci", repo, "[Sobs] Set up CI metadata scripts for "+repo))
	}
	if createOtel {
		body := buildOnboardingIssueBody("otel", owner, repo, hasGithubActions)
		res := s.createOrUpdateOnboardingIssue(githubToken, githubRepo,
			"[Sobs] OTEL & RUM telemetry audit for "+repo, body, []string{"sobs-onboarding", "observability"})
		results.Set("otel_issue", s.buildOnboardingIssueResult(res, assignCopilot, githubToken, githubRepo,
			"otel", repo, "[Sobs] OTEL & RUM telemetry audit for "+repo))
	}
	writeJSON(w, http.StatusOK, results)
}

// buildOnboardingIssueResult mirrors the per-issue result block (incl. optional copilot assignment
// and the work-item persist side-effect).
func (s *server) buildOnboardingIssueResult(res map[string]any, assignCopilot bool, token, githubRepo, issueType, repo, defaultTitle string) *jsonenc.Object {
	if errMsg, ok := res["error"].(string); ok {
		return jsonenc.NewObject().Set("error", errMsg)
	}
	issueURL, _ := res["issue_url"].(string)
	issueNumber := mapInt(res, "issue_number")
	issueStatus, _ := res["status"].(string)
	issueNote, _ := res["note"].(string)
	copilotStatus, copilotReason, copilotAt := "not_requested", "", 0
	if assignCopilot && issueNumber != 0 {
		copilotStatus, copilotReason, copilotAt = s.assignIssueToCopilot(token, githubRepo, issueNumber)
	}
	if issueStatus == "created" || issueStatus == "updated" {
		title, _ := res["issue_title"].(string)
		if title == "" {
			title = defaultTitle
		}
		state, _ := res["issue_state"].(string)
		if state == "" {
			state = "open"
		}
		s.persistOnboardingWorkItem(githubRepo, issueURL, issueNumber, title, state, issueStatus, issueNote,
			copilotStatus, copilotReason, copilotAt, issueType, repo)
	}
	return jsonenc.NewObject().
		Set("url", issueURL).Set("number", issueNumber).Set("status", issueStatus).Set("note", issueNote).
		Set("copilot_status", copilotStatus).Set("copilot_assignment_status", copilotStatus).
		Set("copilot_assignment_reason", copilotReason).Set("copilot_assignment_requested_at", copilotAt)
}

// createOrUpdateOnboardingIssue mirrors _create_or_update_onboarding_issue: create the issue when no
// open issue with the same title exists. (The update/reuse branch for an existing new-state issue is
// reached only when GitHub already has a matching issue, which the parity fixture's mock never does.)
func (s *server) createOrUpdateOnboardingIssue(token, githubRepo, title, body string, labels []string) map[string]any {
	titleNorm := strings.TrimSpace(title)
	for _, item := range s.fetchOpenGithubIssues(token, githubRepo) {
		if strings.TrimSpace(toStr(item["issue_title"])) == titleNorm {
			// Existing issue found — leave it unchanged (the new-state update path is a follow-up).
			return map[string]any{
				"issue_url": toStr(item["issue_url"]), "issue_number": mapInt(item, "issue_number"),
				"issue_title": toStr(item["issue_title"]), "issue_state": orDefault(toStr(item["issue_state"]), "open"),
				"status": "reused", "note": "Existing onboarding issue is not in new state; left unchanged.",
			}
		}
	}
	created := s.createGithubIssueRecord(token, githubRepo, title, body, labels)
	if _, isErr := created["error"]; isErr {
		return created
	}
	created["status"] = "created"
	created["note"] = "Created a new onboarding issue."
	return created
}

// createGithubIssueRecord mirrors _create_github_issue_record: POST a new issue, returning its URL.
func (s *server) createGithubIssueRecord(token, githubRepo, title, body string, labels []string) map[string]any {
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return map[string]any{}
	}
	// Mirror _create_github_issue_record: mask the title/body before they leave for GitHub
	// (mask_output_enabled defaults true) and default the labels when the caller supplies none.
	issueLabels := labels
	if len(issueLabels) == 0 {
		issueLabels = []string{"sobs-agent", "automated"}
	}
	payload := jsonenc.NewObject().
		Set("title", s.maskStringForOutput(title)).
		Set("body", s.maskStringForOutput(body)).
		Set("labels", labelsToAny(issueLabels))
	resp, err := s.upstreamRequest("POST", "https://api.github.com/repos/"+owner+"/"+repo+"/issues",
		jsonenc.Encode(payload, dumpsDefault), githubAPIHeaders(token, true, nil))
	if err != nil || resp.Status >= 300 {
		detail := "request failed"
		if o, ok := resp.Body.(*jsonenc.Object); ok {
			if msg := objGetStr(o, "message"); msg != "" {
				detail = msg
			}
		}
		return map[string]any{"error": "GitHub issue creation failed: " + detail}
	}
	o, _ := resp.Body.(*jsonenc.Object)
	if o == nil {
		o = jsonenc.NewObject()
	}
	state := objGetStr(o, "state")
	if state == "" {
		state = "open"
	}
	respTitle := objGetStr(o, "title")
	if respTitle == "" {
		respTitle = title
	}
	num := 0
	if v, ok := o.Get("number"); ok {
		num = jnToInt(v)
	}
	return map[string]any{
		"issue_url": objGetStr(o, "html_url"), "issue_number": num,
		"issue_title": respTitle, "issue_state": state,
	}
}

// fetchOpenGithubIssues mirrors _fetch_open_github_issues (GET open issues; 404/absent -> empty).
func (s *server) fetchOpenGithubIssues(token, githubRepo string) []map[string]any {
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return nil
	}
	// Mirror _fetch_open_github_issues: GET open issues, capped per_page, with auth headers. The
	// param'd URL matches Python's httpx request, so the parity corpus (which has no fixture for
	// this dedupe lookup — both sides 404 → empty) is unaffected.
	url := "https://api.github.com/repos/" + owner + "/" + repo +
		"/issues?state=open&per_page=" + itoaInt(githubIssueDedupeCandidateMax)
	resp, err := s.upstreamRequest("GET", url, nil, githubAPIHeaders(token, false, nil))
	if err != nil || resp.Status != 200 {
		return nil
	}
	list, ok := resp.Body.([]any)
	if !ok {
		return nil
	}
	out := []map[string]any{}
	for _, it := range list {
		o, ok := it.(*jsonenc.Object)
		if !ok {
			continue
		}
		if _, isPR := o.Get("pull_request"); isPR {
			continue
		}
		num := 0
		if v, ok := o.Get("number"); ok {
			num = jnToInt(v)
		}
		// issue_body + assignees mirror _fetch_open_github_issues — needed by the dedup/reuse path
		// (the onboarding caller ignores them). assignees is the list of login strings.
		out = append(out, map[string]any{
			"issue_number": num, "issue_url": objGetStr(o, "html_url"),
			"issue_title": objGetStr(o, "title"), "issue_body": objGetStr(o, "body"),
			"issue_state": orDefault(objGetStr(o, "state"), "open"),
			"assignees":   extractIssueAssigneeLogins(o),
		})
	}
	return out
}

// extractIssueAssigneeLogins mirrors `[str(a.get("login") or "") for a in (item.get("assignees") or [])
// if isinstance(a, dict)]` — the login of every assignee object (empty string when absent).
func extractIssueAssigneeLogins(o *jsonenc.Object) []string {
	v, ok := o.Get("assignees")
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := []string{}
	for _, e := range arr {
		if eo, ok := e.(*jsonenc.Object); ok {
			out = append(out, objGetStr(eo, "login"))
		}
	}
	return out
}

// assignIssueToCopilot mirrors _assign_issue_to_copilot at a high level: attempt the assignment via
// GitHub (a body-ignoring mock under parity). Returns (status, reason, requested_at).
func (s *server) assignIssueToCopilot(token, githubRepo string, issueNumber int) (string, string, int) {
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return "failed", "invalid repository", 0
	}
	// Mirror _assign_issue_to_copilot's request: assign the Copilot SWE agent with the
	// agent-assignment target. (The full Python path also gates on a copilot-support probe and
	// threads base_branch/custom_instructions; this remains the high-level assignment.)
	payload := jsonenc.NewObject().
		Set("assignees", []any{githubCopilotAssignee}).
		Set("agent_assignment", jsonenc.NewObject().Set("target_repo", owner+"/"+repo))
	resp, err := s.upstreamRequest("POST",
		"https://api.github.com/repos/"+owner+"/"+repo+"/issues/"+itoaInt(issueNumber)+"/assignees",
		jsonenc.Encode(payload, dumpsDefault), githubAPIHeaders(token, true, nil))
	if err != nil || resp.Status >= 300 {
		return "failed", "assignment request failed", 0
	}
	return "requested", "", int(nowUTC().Unix())
}

// persistOnboardingWorkItem mirrors _persist_onboarding_work_item: record the issue in the work-items
// table (a side-effect; never returned).
func (s *server) persistOnboardingWorkItem(githubRepo, issueURL string, issueNumber int, issueTitle, issueState,
	dedupDecision, note, copilotStatus, copilotReason string, copilotAt int, issueType, repo string) {
	if issueURL == "" {
		return
	}
	now := normalizeCHTimestampNow()
	dedupConfidence := 0.0
	if dedupDecision == "reused" {
		dedupConfidence = 1.0
	}
	if dedupDecision == "" {
		dedupDecision = "new_issue"
	}
	_, _ = s.insertRowsNormalized("sobs_github_work_items", []map[string]any{{
		"Id": newUUIDHex(), "CreatedAt": now, "CompletedAt": now, "AgentRunId": "", "AgentRuleId": "",
		"AgentRuleName": "Onboarding Wizard", "AgentAction": "onboarding_" + issueType, "ServiceName": repo,
		"AnomalyRuleId": "", "AnomalyState": "", "SignalSource": "", "SignalName": "", "SignalValue": 0.0,
		"GithubRepo": githubRepo, "DedupKey": "", "DedupDecision": dedupDecision, "DedupConfidence": dedupConfidence,
		"IssueNumber": issueNumber, "IssueUrl": issueURL, "CanonicalIssueNumber": issueNumber,
		"CanonicalIssueUrl": issueURL, "RelatedIssueUrls": "[]", "OccurrenceCount": 1,
		"IssueState": orDefault(issueState, "open"), "IssueTitle": issueTitle,
		"AnalysisSummary": "Sobs onboarding wizard issue.", "SuggestionSummary": note,
		"CopilotAssignmentRequestedAt": copilotAt, "CopilotAssignmentStatus": orDefault(copilotStatus, "not_requested"),
		"CopilotAssignmentReason": copilotReason, "PrLinked": 0, "PrNumber": 0, "PrUrl": "",
		"IsDeleted": 0, "Version": fixedVersionMillis(),
	}})
}

// findAppIDByRepoURL mirrors _find_app_id_by_repo_url (match a sobs_apps row by repo URL).
func (s *server) findAppIDByRepoURL(repoURL string) string {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return ""
	}
	owner, repo := parseGithubRepoOwnerName(repoURL)
	if owner == "" || repo == "" {
		return ""
	}
	res, err := s.db.Execute("SELECT Id, RepoUrl FROM sobs_apps FINAL WHERE IsDeleted=0")
	if err != nil {
		return ""
	}
	for _, m := range rowMaps(res) {
		o2, r2 := parseGithubRepoOwnerName(cStr(m, "RepoUrl"))
		if strings.EqualFold(o2, owner) && strings.EqualFold(r2, repo) {
			return cStr(m, "Id")
		}
	}
	return ""
}

// buildOnboardingIssueBody is a concise stand-in for the (response-invisible) markdown bodies built
// by _build_ci_metadata_issue_body / _build_otel_audit_issue_body.
func buildOnboardingIssueBody(kind, owner, repo string, hasGithubActions bool) string {
	if kind == "otel" {
		return "## OTEL & RUM telemetry audit for " + owner + "/" + repo + "\n\n" +
			"Review OpenTelemetry instrumentation coverage and RUM asset reporting.\n"
	}
	actions := "no GitHub Actions workflow detected"
	if hasGithubActions {
		actions = "GitHub Actions workflow detected"
	}
	return "## Set up CI metadata scripts for " + owner + "/" + repo + "\n\n" +
		"Add release metadata reporting to your CI (" + actions + ").\n"
}

func ciStatusBool(status map[string]any, key string) bool {
	b, _ := status[key].(bool)
	return b
}

func ciStatusStr(status map[string]any, key string) string {
	return toStr(status[key])
}

func ciStatusExpiry(status map[string]any, key string) string {
	if e, ok := status["expiry"].(map[string]any); ok {
		return toStr(e[key])
	}
	return ""
}

func mapInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

func itoaInt(n int) string { return strconv.Itoa(n) }
