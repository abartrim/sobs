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

// buildOnboardingIssueBody dispatches to the two full markdown-body ports below. It mirrors the
// call sites in app.py's api_onboarding_create_issues (33829, 33880), which call
// _build_ci_metadata_issue_body / _build_otel_audit_issue_body directly.
func buildOnboardingIssueBody(kind, owner, repo string, hasGithubActions bool) string {
	if kind == "otel" {
		return buildOtelAuditIssueBody(owner, repo)
	}
	return buildCIMetadataIssueBody(owner, repo, hasGithubActions)
}

// buildCIMetadataIssueBody mirrors _build_ci_metadata_issue_body (app.py:33108) byte-for-byte: the
// full "Sobs CI Metadata Setup" markdown issue body, including the CI-secrets table, curl examples,
// and step-by-step setup checklist. Only the CI-provider callout section varies with
// hasGithubActions; everything else is invariant template text.
func buildCIMetadataIssueBody(owner, repo string, hasGithubActions bool) string {
	ciSection := "\n## CI Provider\n\n" +
		"No GitHub Actions workflows were detected. The steps below are provider-agnostic and can\n" +
		"be adapted for Jenkins, CircleCI, GitLab CI, Buildkite, or other CI systems.\n"
	if hasGithubActions {
		ciSection = "\n## CI Provider\n\n" +
			"This repository uses **GitHub Actions**. Use polling mode first, then optionally add\n" +
			"realtime push once security approval for outbound CI calls is in place.\n"
	}

	var b strings.Builder
	b.WriteString("# Sobs CI Metadata Setup\n\n")
	b.WriteString("This issue defines how `" + owner + "/" + repo + "` should integrate with Sobs CI metadata.\n\n")
	b.WriteString("Sobs supports two modes:\n\n")
	b.WriteString("1. **Polling mode (default)**\n")
	b.WriteString("     - No CI workflow edits required.\n")
	b.WriteString("    - Sobs reads GitHub run/check state and uses conditional requests\n")
	b.WriteString("      (`ETag`/`If-None-Match`) to keep polling efficient.\n")
	b.WriteString("     - Best starting point when CI outbound calls require security approval.\n\n")
	b.WriteString("2. **Realtime push mode (optional)**\n")
	b.WriteString("     - CI posts release metadata directly to Sobs with a Sobs API key.\n")
	b.WriteString("     - Faster and deterministic release visibility.\n")
	b.WriteString("     - Optional GitHub webhook can be added for faster refresh triggers.\n\n")
	b.WriteString("> Keep polling mode available as fallback even if realtime push is enabled.\n\n")
	b.WriteString(ciSection)
	b.WriteString("\n\n---\n\n")
	b.WriteString("## Step 1 - Baseline repository setup in Sobs\n\n")
	b.WriteString("- Verify repository URL in **Settings -> Repositories**\n")
	b.WriteString("- Verify GitHub token is valid for read operations\n")
	b.WriteString("- Verify token expiry tracking is configured\n\n")
	b.WriteString("---\n\n")
	b.WriteString("## Step 2 - Polling mode (no CI changes)\n\n")
	b.WriteString("No workflow updates are required for this step.\n\n")
	b.WriteString("- Confirm Sobs can read workflow/check state for this repo\n")
	b.WriteString("- Confirm Sobs conditional polling is enabled and stable\n")
	b.WriteString("- Confirm CVE/release views continue to populate\n\n")
	b.WriteString("---\n\n")
	b.WriteString("## Step 3 - Register a release (optional realtime push mode)\n\n")
	b.WriteString("If CI outbound integration is approved, add these CI secrets:\n\n")
	b.WriteString("| Secret | Description |\n")
	b.WriteString("|--------|-------------|\n")
	b.WriteString("| `SOBS_URL` | Base URL of your Sobs instance (for example `https://sobs.internal`) |\n")
	b.WriteString("| `SOBS_INGEST_API_KEY` | Sobs ingest API key from Settings -> Repositories |\n")
	b.WriteString("| `SOBS_APP_ID` | Application ID from Settings -> Repositories |\n\n")
	b.WriteString("Use this push call in CI:\n\n")
	b.WriteString("```bash\n")
	b.WriteString(`curl -sS -X POST "${SOBS_URL}/v1/apps/${SOBS_APP_ID}/releases" \` + "\n")
	b.WriteString(`        -H "X-API-Key: ${SOBS_INGEST_API_KEY}" \` + "\n")
	b.WriteString(`        -H "Content-Type: application/json" \` + "\n")
	b.WriteString("        -d '{\n")
	b.WriteString(`                "version":    "${VERSION}",` + "\n")
	b.WriteString(`                "commitSha":  "${COMMIT_SHA}",` + "\n")
	b.WriteString(`                "buildId":    "${BUILD_ID}",` + "\n")
	b.WriteString(`                "environment": "production"` + "\n")
	b.WriteString("        }'\n")
	b.WriteString("```\n\n")
	b.WriteString("Best practice requirements for release identity:\n\n")
	b.WriteString("- Use a release `version` that exactly matches deployed runtime identity (for example image tag or Git tag).\n")
	b.WriteString("- Keep `commitSha` and `buildId` immutable per published release.\n")
	b.WriteString("- Propagate the same release identifier into OTEL `service.version` so Sobs can\n")
	b.WriteString("    correlate CVEs to observed runtime activity.\n")
	b.WriteString("- For containerized workloads, include image digest/tag in release metadata where available.\n\n")
	b.WriteString("---\n\n")
	b.WriteString("## Step 4 - Upload dependency lockfile metadata\n\n")
	b.WriteString("Lockfile metadata improves release-scoped CVE enrichment. Best practice is to\n")
	b.WriteString("extract resolved dependency snapshots from the built container image for each\n")
	b.WriteString("target architecture (for example linux/amd64 and linux/arm64), then register\n")
	b.WriteString("each snapshot with provenance fields (size/checksum/storageRef/platform/architecture):\n\n")
	b.WriteString("For GitHub Actions, prefer a visible artifact directory/path for dependency\n")
	b.WriteString("snapshots (for example `sobs-release/pip-freeze-linux-amd64.txt`). Hidden\n")
	b.WriteString("directories such as `.sobs-release/` are excluded by `actions/upload-artifact`\n")
	b.WriteString("unless `include-hidden-files: true` is set explicitly.\n\n")
	b.WriteString("```bash\n")
	b.WriteString(`curl -sS -X POST "${SOBS_URL}/v1/releases/${RELEASE_ID}/artifacts/meta" \` + "\n")
	b.WriteString(`        -H "X-API-Key: ${SOBS_INGEST_API_KEY}" \` + "\n")
	b.WriteString(`        -H "Content-Type: application/json" \` + "\n")
	b.WriteString("        -d '{\n")
	b.WriteString(`                "artifactType": "dependencies-lockfile",` + "\n")
	b.WriteString(`                                "name": "pip-freeze-linux-amd64",` + "\n")
	b.WriteString(`                                "contentType": "application/json",` + "\n")
	b.WriteString(`                                "size": ${LOCKFILE_SIZE},` + "\n")
	b.WriteString(`                                "storageRef": "ci://artifacts/pip-freeze-linux-amd64.txt",` + "\n")
	b.WriteString(`                                "checksumSha256": "${LOCKFILE_SHA256}",` + "\n")
	b.WriteString(`                                "platform": "linux",` + "\n")
	b.WriteString(`                                "architecture": "amd64",` + "\n")
	b.WriteString(`                                "metadata": {` + "\n")
	b.WriteString(`                                    "dependencies": ${RESOLVED_DEPS_JSON}` + "\n")
	b.WriteString("                                }\n")
	b.WriteString("        }'\n")
	b.WriteString("```\n\n")
	b.WriteString("Repeat per architecture (for example `pip-freeze-linux-arm64`) to ensure CVE\n")
	b.WriteString("tracking reflects what is actually shipped for each target platform.\n\n")
	b.WriteString("Dependency capture requirements:\n\n")
	b.WriteString("- Derive snapshots from the built/published container image, not from a host-only\n")
	b.WriteString("    resolver run.\n")
	b.WriteString("- Track per-arch snapshots independently for multi-arch releases.\n")
	b.WriteString("- Fail CI early if any expected dependency snapshot file is missing or empty\n")
	b.WriteString("    before artifact upload and metadata registration.\n")
	b.WriteString("- Verify the dependency snapshot artifact upload succeeds before release/artifact\n")
	b.WriteString("    registration continues.\n")
	b.WriteString("- Include provenance fields (`storageRef`, `checksumSha256`, `size`, `platform`,\n")
	b.WriteString("  `architecture`) on every dependency artifact.\n\n")
	b.WriteString("---\n\n")
	b.WriteString("## Step 5 - Upload JS source maps (web front-end only)\n\n")
	b.WriteString("Source maps let Sobs resolve minified stack traces to original source locations:\n\n")
	b.WriteString("```bash\n")
	b.WriteString(`curl -sS -X POST "${SOBS_URL}/v1/releases/${RELEASE_ID}/artifacts/meta" \` + "\n")
	b.WriteString(`    -H "X-API-Key: ${SOBS_INGEST_API_KEY}" \` + "\n")
	b.WriteString(`    -H "Content-Type: application/json" \` + "\n")
	b.WriteString("    -d '{\n")
	b.WriteString(`        "artifactType": "js_sourcemap",` + "\n")
	b.WriteString(`        "name": "app.min.js.map",` + "\n")
	b.WriteString(`        "contentType": "application/json",` + "\n")
	b.WriteString(`        "size": ${SOURCEMAP_SIZE},` + "\n")
	b.WriteString(`        "checksumSha256": "${SOURCEMAP_SHA256}",` + "\n")
	b.WriteString(`        "storageRef": "ci://artifacts/app.min.js.map"` + "\n")
	b.WriteString("    }'\n")
	b.WriteString("```\n\n")
	b.WriteString("Source map capture requirements:\n\n")
	b.WriteString("- Register maps from the same build outputs that were deployed.\n")
	b.WriteString("- Include `size` and `checksumSha256` for provenance and troubleshooting.\n\n")
	b.WriteString("---\n\n")
	b.WriteString("## Step 6 - Optional webhook acceleration\n\n")
	b.WriteString("If repository admins approve webhook setup, add a GitHub webhook to Sobs for push/workflow events.\n\n")
	b.WriteString("- This is optional and should not block onboarding.\n")
	b.WriteString("- Admin/webhook-write permissions are usually required.\n")
	b.WriteString("- Keep polling mode enabled as fallback.\n\n")
	b.WriteString("---\n\n")
	b.WriteString("## Step 7 - Trigger a CVE scan (optional)\n\n")
	b.WriteString("```bash\n")
	b.WriteString(`curl -sS -X POST "${SOBS_URL}/api/enrichment/cve/scan" \` + "\n")
	b.WriteString(`        -H "X-API-Key: ${SOBS_INGEST_API_KEY}" \` + "\n")
	b.WriteString(`        -H "Content-Type: application/json" \` + "\n")
	b.WriteString("        -d '{}'\n")
	b.WriteString("```\n\n")
	b.WriteString("---\n\n")
	b.WriteString("## Step 8 - OTEL-linked CVE impact triage\n\n")
	b.WriteString("Use CVE results together with OTEL/log evidence to separate:\n\n")
	b.WriteString("- **Confirmed impact candidates**: vulnerable package/version appears in release\n")
	b.WriteString("    metadata and related services show active OTEL/log usage for that runtime.\n")
	b.WriteString("- **Latent exposure**: vulnerable package/version exists in release metadata but no\n")
	b.WriteString("    current OTEL/log evidence of active usage.\n\n")
	b.WriteString("This lets teams prioritize \"must patch now\" findings while still tracking latent risk.\n\n")
	b.WriteString("Recommended correlation keys:\n\n")
	b.WriteString("- `service.name`\n")
	b.WriteString("- `service.version` (must match the registered release version)\n")
	b.WriteString("- `deployment.environment`\n")
	b.WriteString("- release metadata (`version`, `commitSha`, `buildId`, image tag/digest)\n\n")
	b.WriteString("---\n\n")
	b.WriteString("## Manual verification checklist\n\n")
	b.WriteString("- Confirm first pushed release appears in Sobs\n")
	b.WriteString("- Confirm lockfile artifact metadata is visible for each architecture\n")
	b.WriteString("- Confirm dependency snapshot artifacts upload successfully from non-hidden CI paths\n")
	b.WriteString("- Confirm dependency artifacts include provenance fields (size/checksum/storageRef/platform/architecture)\n")
	b.WriteString("- Confirm release version matches OTEL `service.version`\n")
	b.WriteString("- Confirm CVE findings reflect the container-derived dependency snapshots\n")
	b.WriteString("- Confirm CVE review distinguishes confirmed impact candidates vs latent exposure\n")
	b.WriteString("- Confirm polling-only fallback works if CI push or webhook path is blocked\n\n")
	b.WriteString("---\n\n")
	b.WriteString("*This issue was created automatically by the Sobs Onboarding Wizard for repository `" +
		owner + "/" + repo + "`.*\n")
	return b.String()
}

// buildOtelAuditIssueBody mirrors _build_otel_audit_issue_body (app.py:33333) byte-for-byte: the
// full "OTEL & RUM Telemetry Audit" markdown issue body, including the per-section audit checklist
// and code samples (OTEL SDK setup, RUM snippet).
func buildOtelAuditIssueBody(owner, repo string) string {
	var b strings.Builder
	b.WriteString("# OTEL & RUM Telemetry Audit\n\n")
	b.WriteString("This issue requests a comprehensive audit of the `" + owner + "/" + repo + "` repository to identify\n")
	b.WriteString("gaps in observability coverage and add best-practice OpenTelemetry (OTEL) instrumentation,\n")
	b.WriteString("Real User Monitoring (RUM), and AI telemetry.\n\n")
	b.WriteString("---\n\n")
	b.WriteString("## Audit Checklist\n\n")
	b.WriteString("### 1. Core OTEL SDK Setup\n\n")
	b.WriteString("- [ ] Install and configure the OTEL SDK for the primary language(s) used in this repository\n")
	b.WriteString("- [ ] Set up a `TracerProvider` with OTLP export pointing to Sobs (`<SOBS_URL>:4317`)\n")
	b.WriteString("- [ ] Set up a `LoggerProvider` (or bridge) so structured application logs flow through OTEL\n")
	b.WriteString("- [ ] Set up a `MeterProvider` for custom metrics (request counts, error rates, latency histograms)\n")
	b.WriteString("- [ ] Ensure `service.name`, `service.version`, and `deployment.environment` resource attributes\n")
	b.WriteString("      are set\n\n")
	b.WriteString("**Example (Python):**\n")
	b.WriteString("```python\n")
	b.WriteString("from opentelemetry import trace\n")
	b.WriteString("from opentelemetry.sdk.trace import TracerProvider\n")
	b.WriteString("from opentelemetry.sdk.trace.export import BatchSpanProcessor\n")
	b.WriteString("from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter\n\n")
	b.WriteString("provider = TracerProvider(\n")
	b.WriteString(`    resource=Resource({"service.name": "my-service", "service.version": "1.0.0"})` + "\n")
	b.WriteString(")\n")
	b.WriteString("provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=\"http://sobs:4317\")))\n")
	b.WriteString("trace.set_tracer_provider(provider)\n")
	b.WriteString("```\n\n")
	b.WriteString("---\n\n")
	b.WriteString("### 2. Web Front-End — RUM Snippet (if applicable)\n\n")
	b.WriteString("If this repository contains a web front-end (HTML, React, Vue, Angular, etc.):\n\n")
	b.WriteString("- [ ] Add the Sobs RUM snippet to the `<head>` of every page (or the root layout component)\n")
	b.WriteString("- [ ] Configure RUM to capture **console logs**, **JavaScript stack traces**, **navigation\n")
	b.WriteString("      breadcrumbs**, **Web Vitals** (LCP, CLS, INP, TTFB, FCP), **screenshots** (on error),\n")
	b.WriteString("      and **session replays**\n")
	b.WriteString("- [ ] Set `service`, `environment`, and `release` attributes in the RUM config\n\n")
	b.WriteString("**Sobs RUM snippet:**\n")
	b.WriteString("```html\n")
	b.WriteString("<script>\n")
	b.WriteString("  window.SobsRumConfig = {\n")
	b.WriteString("    endpoint: '<SOBS_URL>/rum',\n")
	b.WriteString("    service:  'my-frontend',\n")
	b.WriteString("    env:      'production',\n")
	b.WriteString("    release:  '{{ APP_VERSION }}',\n")
	b.WriteString("    captureConsole: true,\n")
	b.WriteString("    captureErrors:  true,\n")
	b.WriteString("    captureReplays: true,\n")
	b.WriteString("    captureScreenshots: true\n")
	b.WriteString("  };\n")
	b.WriteString("</script>\n")
	b.WriteString("<script src=\"<SOBS_URL>/static/rum.min.js\"></script>\n")
	b.WriteString("```\n\n")
	b.WriteString("---\n\n")
	b.WriteString("### 3. AI / LLM Workloads (if applicable)\n\n")
	b.WriteString("If this repository makes LLM API calls (OpenAI, Anthropic, Azure OpenAI, etc.):\n\n")
	b.WriteString("- [ ] Use `opentelemetry-instrumentation-openai` (or equivalent) to auto-instrument LLM calls\n")
	b.WriteString("- [ ] Emit OTEL `gen_ai.*` semantic-convention attributes on every LLM span:\n")
	b.WriteString("      `gen_ai.system`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`,\n")
	b.WriteString("      `gen_ai.usage.output_tokens`\n")
	b.WriteString("- [ ] Propagate trace context into LLM calls so the Sobs AI page can correlate prompts with\n")
	b.WriteString("      application traces\n")
	b.WriteString("- [ ] Record prompt templates and response hashes (not full content) as span attributes for\n")
	b.WriteString("      traceability\n")
	b.WriteString("- [ ] Ensure no PII / secrets are emitted in span attributes\n\n")
	b.WriteString("---\n\n")
	b.WriteString("### 4. Infrastructure & Web Logs (if applicable)\n\n")
	b.WriteString("For infrastructure services (proxies, gateways, databases, queues):\n\n")
	b.WriteString("- [ ] Add OTEL log bridge or structured JSON logging shipped via OTLP to Sobs\n")
	b.WriteString("- [ ] Include `http.method`, `http.route`, `http.status_code`, `net.peer.ip` attributes\n")
	b.WriteString("      for HTTP services\n")
	b.WriteString("- [ ] For databases: include `db.system`, `db.statement` (redacted), `db.name` span attributes\n")
	b.WriteString("- [ ] For message queues: include `messaging.system`, `messaging.destination` span attributes\n\n")
	b.WriteString("---\n\n")
	b.WriteString("### 5. Error & Exception Capture\n\n")
	b.WriteString("- [ ] Call `span.record_exception(exc)` and `span.set_status(StatusCode.ERROR)` in all\n")
	b.WriteString("      exception handlers\n")
	b.WriteString("- [ ] Ensure unhandled exceptions are captured and forwarded to the Sobs errors endpoint\n")
	b.WriteString("- [ ] Add a global uncaught-exception handler that emits a final error span before process exit\n\n")
	b.WriteString("---\n\n")
	b.WriteString("### 6. Telemetry Verification\n\n")
	b.WriteString("After implementing the above:\n\n")
	b.WriteString("- [ ] Verify traces appear on the Sobs **Traces** page\n")
	b.WriteString("- [ ] Verify logs appear on the Sobs **Logs** page\n")
	b.WriteString("- [ ] Verify metrics appear on the Sobs **Metrics** page\n")
	b.WriteString("- [ ] Verify RUM events appear on the Sobs **RUM** page (if web front-end added)\n")
	b.WriteString("- [ ] Verify AI calls appear on the Sobs **AI** page (if LLM workload added)\n")
	b.WriteString("- [ ] Run the CVE scan and verify findings appear on the Sobs **CVE** page\n\n")
	b.WriteString("---\n\n")
	b.WriteString("## What remains manual\n\n")
	b.WriteString("- Reviewing each checklist item and confirming it applies to this repository's technology stack\n")
	b.WriteString("- Testing that telemetry flows correctly end-to-end\n")
	b.WriteString("- Removing any accidentally captured PII or secrets from span attributes\n\n")
	b.WriteString("---\n\n")
	b.WriteString("*This issue was created automatically by the Sobs Onboarding Wizard for repository `" +
		owner + "/" + repo + "`.*\n")
	return b.String()
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
