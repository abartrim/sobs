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
		// AgentAction is onboarding_observability for the OTEL audit issue (app.py 33927:
		// issue_type="observability"), not onboarding_otel.
		results.Set("otel_issue", s.buildOnboardingIssueResult(res, assignCopilot, githubToken, githubRepo,
			"observability", repo, "[Sobs] OTEL & RUM telemetry audit for "+repo))
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
		// Onboarding assigns without base_branch / custom_instructions (app.py 33852, 33903).
		copilotStatus, copilotReason, copilotAt = s.assignIssueToCopilot(token, githubRepo, issueNumber, "", "")
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

// createOrUpdateOnboardingIssue mirrors _create_or_update_onboarding_issue: create the issue once;
// when an open issue with the same title already exists, fetch its detail and either UPDATE it (PATCH)
// while it is still in new/untouched state, or leave it unchanged ("reused").
func (s *server) createOrUpdateOnboardingIssue(token, githubRepo, title, body string, labels []string) map[string]any {
	titleNorm := strings.TrimSpace(title)
	var existing map[string]any
	for _, item := range s.fetchOpenGithubIssues(token, githubRepo) {
		if strings.TrimSpace(toStr(item["issue_title"])) == titleNorm {
			existing = item
			break
		}
	}

	if existing == nil {
		// Onboarding issues are created with mask_output_enabled=False (app.py 33063-33070).
		created := s.createGithubIssueRecord(token, githubRepo, title, body, labels, false)
		if _, isErr := created["error"]; isErr {
			return created
		}
		created["status"] = "created"
		created["note"] = "Created a new onboarding issue."
		return created
	}

	issueNumber := mapInt(existing, "issue_number")
	issueURL := toStr(existing["issue_url"])
	detail := s.githubGetIssueDetail(token, githubRepo, issueNumber)

	if detail != nil && githubIssueIsNewState(detail) {
		// app.py 33082-33095: the existing issue is still untouched, so PATCH it with the fresh
		// onboarding content. Onboarding updates pass mask_output_enabled=False (app.py 33089).
		updated := s.updateGithubIssueRecord(token, githubRepo, issueNumber, title, body, labels, false)
		if _, isErr := updated["error"]; isErr {
			return updated
		}
		updated["status"] = "updated"
		updated["note"] = "Updated the existing onboarding issue because it was still new."
		return updated
	}

	// app.py 33097-33105: not new state -> leave unchanged, preferring the detail payload's fields.
	existingState := orDefault(objGetStrAny(detail, "state"), orDefault(toStr(existing["issue_state"]), "open"))
	return map[string]any{
		"issue_url":    orDefault(objGetStrAny(detail, "html_url"), issueURL),
		"issue_number": issueNumber,
		"issue_title":  orDefault(objGetStrAny(detail, "title"), orDefault(toStr(existing["issue_title"]), title)),
		"issue_state":  existingState,
		"status":       "reused",
		"note":         "Existing onboarding issue is not in new state; left unchanged.",
	}
}

// githubGetIssueDetail mirrors _github_get_issue_detail: GET a single issue payload; nil on error.
func (s *server) githubGetIssueDetail(token, githubRepo string, issueNumber int) *jsonenc.Object {
	if token == "" || githubRepo == "" || issueNumber <= 0 {
		return nil
	}
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return nil
	}
	resp, err := s.upstreamRequest("GET",
		"https://api.github.com/repos/"+owner+"/"+repo+"/issues/"+itoaInt(issueNumber),
		nil, githubAPIHeaders(token, false, nil))
	if err != nil || resp.Status >= 300 {
		return nil
	}
	o, _ := resp.Body.(*jsonenc.Object)
	return o
}

// githubIssueIsNewState mirrors _github_issue_is_new_state: True when state=="open", comments==0,
// created_at non-empty and created_at == updated_at (i.e. the issue is still untouched).
func githubIssueIsNewState(o *jsonenc.Object) bool {
	if o == nil {
		return false
	}
	state := strings.ToLower(strings.TrimSpace(objGetStr(o, "state")))
	comments := 0
	if v, ok := o.Get("comments"); ok {
		comments = jnToInt(v)
	}
	createdAt := strings.TrimSpace(objGetStr(o, "created_at"))
	updatedAt := strings.TrimSpace(objGetStr(o, "updated_at"))
	return state == "open" && comments == 0 && createdAt != "" && createdAt == updatedAt
}

// updateGithubIssueRecord mirrors _update_github_issue_record: PATCH an existing issue (title/body and
// optionally labels) and return normalized metadata. maskOutput scrubs title/body before the outbound
// request (the onboarding caller passes False); the parity mock ignores the body either way.
func (s *server) updateGithubIssueRecord(token, githubRepo string, issueNumber int, title, body string, labels []string, maskOutput bool) map[string]any {
	if token == "" || githubRepo == "" || issueNumber <= 0 {
		return map[string]any{}
	}
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return map[string]any{}
	}
	outTitle, outBody := title, body
	if maskOutput {
		outTitle = s.maskStringForOutput(title)
		outBody = s.maskStringForOutput(body)
	}
	payload := jsonenc.NewObject().Set("title", outTitle).Set("body", outBody)
	// app.py 33014: labels is added only when not None. The onboarding caller always supplies labels.
	if labels != nil {
		payload.Set("labels", labelsToAny(labels))
	}
	resp, err := s.upstreamRequest("PATCH",
		"https://api.github.com/repos/"+owner+"/"+repo+"/issues/"+itoaInt(issueNumber),
		jsonenc.Encode(payload, dumpsDefault), githubAPIHeaders(token, true, nil))
	if err != nil || resp.Status >= 300 {
		detail := ""
		if o, ok := resp.Body.(*jsonenc.Object); ok {
			detail = strings.TrimSpace(objGetStr(o, "message"))
		}
		if detail == "" {
			detail = "request failed"
		}
		return map[string]any{"error": "GitHub issue update failed: " + detail}
	}
	o, _ := resp.Body.(*jsonenc.Object)
	if o == nil {
		o = jsonenc.NewObject()
	}
	num := issueNumber
	if v, ok := o.Get("number"); ok {
		if n := jnToInt(v); n != 0 {
			num = n
		}
	}
	respTitle := objGetStr(o, "title")
	if respTitle == "" {
		respTitle = title
	}
	state := objGetStr(o, "state")
	if state == "" {
		state = "open"
	}
	return map[string]any{
		"issue_url": objGetStr(o, "html_url"), "issue_number": num,
		"issue_title": respTitle, "issue_state": state,
	}
}

// objGetStrAny reads a string field from an optional *jsonenc.Object (empty when nil/absent).
func objGetStrAny(o *jsonenc.Object, key string) string {
	if o == nil {
		return ""
	}
	return objGetStr(o, key)
}

// createGithubIssueRecord mirrors _create_github_issue_record: POST a new issue, returning its URL.
// maskOutput selects whether the title/body are scrubbed before leaving for GitHub: the agent flow
// threads the trigger-derived mask_output_enabled (default True), while the onboarding wizard passes
// False (app.py 33069). Masking only affects the outbound bytes (ignored by the parity mock).
func (s *server) createGithubIssueRecord(token, githubRepo, title, body string, labels []string, maskOutput bool) map[string]any {
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return map[string]any{}
	}
	// Mirror _create_github_issue_record: mask the title/body only when mask_output_enabled, and
	// default the labels when the caller supplies none.
	issueLabels := labels
	if len(issueLabels) == 0 {
		issueLabels = []string{"sobs-agent", "automated"}
	}
	outTitle, outBody := title, body
	if maskOutput {
		outTitle = s.maskStringForOutput(title)
		outBody = s.maskStringForOutput(body)
	}
	payload := jsonenc.NewObject().
		Set("title", outTitle).
		Set("body", outBody).
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

// assignIssueToCopilot mirrors _assign_issue_to_copilot in full: validate inputs, probe the repo for
// Copilot-assignment support, POST the assignee + agent_assignment (base_branch / custom_instructions
// optional), then inspect the response assignees. Returns (status, reason, requested_at_ms) — the
// timestamp is int(time.time()*1000) on every success AND HTTP-error path, matching the limiter
// (which compares against CopilotAssignmentRequestedAt in ms).
func (s *server) assignIssueToCopilot(token, githubRepo string, issueNumber int, baseBranch, customInstructions string) (string, string, int) {
	if token == "" || githubRepo == "" || issueNumber <= 0 {
		return "blocked", "missing GitHub token, repo, or issue number", 0
	}
	if !s.githubRepoSupportsCopilot(token, githubRepo) {
		return "blocked", "Copilot cloud agent is not enabled for the target repository", 0
	}
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return "blocked", "invalid GitHub repository target", 0
	}

	agentAssignment := jsonenc.NewObject().Set("target_repo", owner+"/"+repo)
	if baseBranch != "" {
		agentAssignment.Set("base_branch", baseBranch)
	}
	if customInstructions != "" {
		agentAssignment.Set("custom_instructions", truncRunes(customInstructions, 4000))
	}
	payload := jsonenc.NewObject().
		Set("assignees", []any{githubCopilotAssignee}).
		Set("agent_assignment", agentAssignment)

	requestedAt := int(nowUTC().UnixMilli())
	resp, err := s.upstreamRequest("POST",
		"https://api.github.com/repos/"+owner+"/"+repo+"/issues/"+itoaInt(issueNumber)+"/assignees",
		jsonenc.Encode(payload, dumpsDefault), githubAPIHeaders(token, true, nil))
	if err != nil {
		return "failed", err.Error(), requestedAt
	}
	if resp.Status >= 300 {
		// httpx raise_for_status -> detail = exc.response.text[:500].
		detail := truncRunes(resp.RawContent, 500)
		if detail == "" {
			detail = "Copilot issue assignment failed"
		}
		return "failed", detail, requestedAt
	}
	assignees := map[string]bool{}
	if o, ok := resp.Body.(*jsonenc.Object); ok {
		for _, login := range extractIssueAssigneeLogins(o) {
			assignees[strings.ToLower(strings.TrimSpace(login))] = true
		}
	}
	if !assignees[strings.ToLower(githubCopilotAssignee)] && !assignees[githubCopilotSWEAgentLogin] {
		return "requested", "Copilot assignment request accepted; GitHub assignee visibility may lag briefly", requestedAt
	}
	return "requested", "Copilot assignment requested", requestedAt
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
