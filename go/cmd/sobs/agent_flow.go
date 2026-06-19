package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// agentRule is the subset of a sobs_agent_rules row the agent flow needs (mirrors _load_agent_rule).
type agentRule struct {
	id, name, description     string
	triggerType, triggerRefID string
	triggerState              string
	actions                   []string
	rateLimitMinutes          int
	isEnabled                 bool
}

// loadAgentRule mirrors app.py _load_agent_rule.
func (s *server) loadAgentRule(ruleID string) *agentRule {
	res, err := s.db.Execute(
		"SELECT Id, Name, Description, TriggerType, TriggerRefId, TriggerState, "+
			"Actions, RateLimitMinutes, IsEnabled FROM sobs_agent_rules FINAL "+
			"WHERE IsDeleted=0 AND Id=? LIMIT 1", ruleID)
	if err != nil || len(res.Rows) == 0 {
		return nil
	}
	m := rowMaps(res)[0]
	actions := []string{}
	for _, a := range strings.Split(cStr(m, "Actions"), ",") {
		if t := strings.TrimSpace(a); t != "" {
			actions = append(actions, t)
		}
	}
	return &agentRule{
		id: cStr(m, "Id"), name: cStr(m, "Name"), description: cStr(m, "Description"),
		triggerType: cStr(m, "TriggerType"), triggerRefID: cStr(m, "TriggerRefId"),
		triggerState: cStr(m, "TriggerState"), actions: actions,
		rateLimitMinutes: cInt(m, "RateLimitMinutes"), isEnabled: cBool(m, "IsEnabled"),
	}
}

// agentRuleLastRunTs mirrors _agent_rule_last_run_ts (Unix seconds of the most recent run, or 0).
func (s *server) agentRuleLastRunTs(ruleID string) float64 {
	res, err := s.db.Execute(
		"SELECT max(toUnixTimestamp64Milli(CreatedAt)) AS t FROM sobs_agent_runs FINAL "+
			"WHERE IsDeleted=0 AND RuleId=?", ruleID)
	if err == nil && len(res.Rows) > 0 {
		if t := cFloat(rowMaps(res)[0], "t"); t > 0 {
			return t / 1000.0
		}
	}
	return 0
}

func (a *agentRule) hasAction(name string) bool {
	for _, x := range a.actions {
		if x == name {
			return true
		}
	}
	return false
}

// agentTriggerContext is the manual-trigger context dict (insertion order matters for the
// json.dumps stored in TriggerContext).
func agentTriggerContext(rule *agentRule, extraContext string) *jsonenc.Object {
	return jsonenc.NewObject().
		Set("rule_name", rule.name).
		Set("trigger_state", "manual").
		Set("trigger_type", "manual").
		Set("trigger_ref_id", "").
		Set("extra", extraContext)
}

// agentOutcome mirrors the dict returned by _run_agent_rule_instance.
type agentOutcome struct {
	ok     bool
	runID  string
	result *jsonenc.Object
	err    string
}

// runAgentRuleInstance mirrors _run_agent_rule_instance: insert a pending run row, run the flow,
// and on error record a failed row.
func (s *server) runAgentRuleInstance(rule *agentRule, settings map[string]string, tctx *jsonenc.Object) agentOutcome {
	runID := newUUIDv4()
	nowTS := normalizeCHTimestampNow()
	tctxJSON := string(jsonenc.Encode(tctx, dumpsDefault))
	s.insertAgentRun(map[string]any{
		"Id": runID, "RuleId": rule.id, "RuleName": rule.name, "TriggerContext": tctxJSON,
		"Status": "pending", "GuardDecision": "", "DlpResult": "", "Analysis": "", "Suggestion": "",
		"GithubIssueUrl": "", "ErrorMessage": "", "CreatedAt": nowTS, "CompletedAt": nowTS,
		"IsDismissed": 0, "IsDeleted": 0, "Version": fixedVersionMillis(),
	})
	result, err := s.runAgentFlow(rule, settings, tctx, runID)
	if err != nil {
		s.insertAgentRun(map[string]any{
			"Id": runID, "RuleId": rule.id, "RuleName": rule.name, "TriggerContext": tctxJSON,
			"Status": "failed", "GuardDecision": "", "DlpResult": "", "Analysis": "", "Suggestion": "",
			"GithubIssueUrl": "", "ErrorMessage": err.Error(), "CreatedAt": nowTS,
			"CompletedAt": normalizeCHTimestampNow(), "IsDismissed": 0, "IsDeleted": 0,
			"Version": fixedVersionMillis(),
		})
		return agentOutcome{ok: false, runID: runID, err: err.Error()}
	}
	return agentOutcome{ok: true, runID: runID, result: result}
}

// insertAgentRun inserts a (possibly partial) sobs_agent_runs row, mirroring _update_run /
// _insert_rows_json_each_row — ClickHouse fills omitted columns with their defaults.
func (s *server) insertAgentRun(row map[string]any) {
	_, _ = s.insertRowsNormalized("sobs_agent_runs", []map[string]any{row})
}

// runAgentFlow mirrors _run_agent_flow. The analyze path (guard -> root-cause LLM -> record) is
// fully ported. The github-issue/DLP/dedup/copilot branch is only reachable when the rule requests
// a github action AND a github repo+token are resolved; for the parity analyze-only rule (and any
// rule without a configured repo/token) Python skips that branch, leaving github_issue_url empty.
func (s *server) runAgentFlow(rule *agentRule, settings map[string]string, tctx *jsonenc.Object, runID string) (*jsonenc.Object, error) {
	updateRun := func(updates map[string]any) {
		row := map[string]any{"Id": runID, "IsDeleted": 0, "Version": fixedVersionMillis()}
		for k, v := range updates {
			row[k] = v
		}
		s.insertAgentRun(row)
	}
	updateRun(map[string]any{"Status": "running"})

	endpointURL := strings.TrimSpace(settings["ai.endpoint_url"])
	model := strings.TrimSpace(settings["ai.model"])
	apiKey := strings.TrimSpace(settings["ai.api_key"])
	dlpURL := strings.TrimSpace(settings["ai.dlp_endpoint_url"])
	githubRepo, githubToken := s.resolveAgentGithubTarget(settings, tctx)

	// mask_output_enabled (default True) is derived from the trigger context's `extra` (a nested
	// object for the user-observation flow, a JSON string for the manual flow) and threaded into the
	// issue-creation masking — app.py 6794-6801.
	maskOutput := extractTriggerMaskOutput(tctx)

	contextSummary := s.buildAgentContextSummary(tctx)

	// 1. Guard model check.
	allowed, guardReason, _ := s.checkGuardModel(contextSummary)
	guardDecision := "allowed"
	if !allowed {
		guardDecision = "blocked: " + guardReason
		updateRun(map[string]any{
			"Status": "blocked_by_guard", "GuardDecision": guardDecision,
			"CompletedAt": normalizeCHTimestampNow(),
		})
		return jsonenc.NewObject().
			Set("status", "blocked_by_guard").Set("guard_decision", guardDecision), nil
	}

	// 2. LLM root-cause analysis.
	analysis, suggestion := "", ""
	if rule.hasAction("analyze") && endpointURL != "" && model != "" {
		systemPrompt := strings.TrimSpace(settings["ai.system_prompt"])
		if systemPrompt == "" {
			systemPrompt = agentRootCauseSystemPrompt
		}
		messages := []any{
			jsonenc.NewObject().Set("role", "system").Set("content", systemPrompt),
			jsonenc.NewObject().Set("role", "user").Set("content", contextSummary),
		}
		reply, _, err := s.callLLMChat(llmRequest{
			endpoint:      endpointURL,
			model:         model,
			apiKey:        apiKey,
			thinkingLevel: strings.TrimSpace(settings["ai.thinking_level"]),
			maxTokens:     512,
			messages:      messages,
		})
		if err != nil {
			return nil, err
		}
		analysis, suggestion = parseAgentAnalysis(reply)
	}

	// 3. Optional DLP check + GitHub issue creation. The issue branch runs only with a github
	// action AND a resolved repo+token. When a dlp_check action AND ai.dlp_endpoint_url are both
	// present (C7), the outgoing issue text is screened first: a flagged verdict completes the run
	// WITHOUT creating an issue; a clean (or fail-open "dlp_unavailable") verdict lets creation
	// proceed and leaves dlp_result "skipped" -> "clean". When creation proceeds,
	// chooseGithubIssueOutcome (H9) handles the full dedup/reuse + Copilot-assignment logic (the
	// parity fixture has no prior work items or open issues, so every issue is still a fresh
	// new_issue).
	dlpResult := "skipped"
	githubIssueURL := ""
	issueOutcome := map[string]any{}
	wantsIssue := rule.hasAction("github_issue") || rule.hasAction("github_issue_copilot")
	wantsCopilot := rule.hasAction("github_issue_copilot")
	if wantsIssue && githubToken != "" && githubRepo != "" {
		issueText := contextSummary + "\n\nAnalysis: " + analysis + "\n\nSuggestion: " + suggestion
		if rule.hasAction("dlp_check") && dlpURL != "" {
			dlpClean, dlpDetail := s.checkDLPEndpoint(dlpURL, issueText, apiKey)
			if dlpClean {
				dlpResult = "clean"
			} else {
				dlpResult = "flagged: " + dlpDetail
			}
			if !dlpClean {
				updateRun(map[string]any{
					"Status": "completed", "GuardDecision": guardDecision, "DlpResult": dlpResult,
					"Analysis": analysis, "Suggestion": suggestion,
					"CompletedAt": normalizeCHTimestampNow(),
				})
				return jsonenc.NewObject().
					Set("status", "completed").
					Set("dlp_result", dlpResult).
					Set("analysis", analysis).
					Set("suggestion", suggestion), nil
			}
		}
		triggerFields := extractAgentTriggerFields(tctx)
		issueTitle := buildAgentIssueTitle(rule, triggerFields)
		issueBody := buildAgentIssueBody(rule, tctx, triggerFields, contextSummary, analysis, suggestion)
		allowNewIssue := s.countGithubIssuesLastHour() <
			parseBoundedIntSetting(settings, "ai.agent_max_issues_per_hour", agentMaxIssuesDefault, 1, 20)
		issueOutcome = s.chooseGithubIssueOutcome(settings, tctx, githubRepo, githubToken, wantsCopilot,
			analysis, suggestion, issueTitle, issueBody, allowNewIssue, maskOutput)
		githubIssueURL = toStr(issueOutcome["issue_url"])
	}

	// Persist the GitHub issue decision as a work item (app.py 6939-6965): keyed by run_id, with the
	// real agent/anomaly/dedup/copilot/PR fields — _persist_github_work_item, not the onboarding one.
	if wantsIssue && (githubIssueURL != "" || len(issueOutcome) > 0) {
		agentAction := "github_issue"
		if wantsCopilot {
			agentAction = "github_issue_copilot"
		}
		canonicalURL := toStr(issueOutcome["canonical_issue_url"])
		if canonicalURL == "" {
			canonicalURL = githubIssueURL
		}
		s.persistGithubWorkItem(runID, rule, tctx, githubIssueURL, analysis, suggestion, agentAction,
			toStr(issueOutcome["issue_title"]), toStr(issueOutcome["issue_state"]),
			toStr(issueOutcome["dedup_key"]), orDefault(toStr(issueOutcome["dedup_decision"]), "new_issue"),
			toFloatAny(issueOutcome["dedup_confidence"]), canonicalURL,
			mapInt(issueOutcome, "canonical_issue_number"), anyToStringList(issueOutcome["related_issue_urls"]),
			firstNonZeroInt(mapInt(issueOutcome, "occurrence_count"), 1),
			mapInt(issueOutcome, "copilot_assignment_requested_at"),
			orDefault(toStr(issueOutcome["copilot_assignment_status"]), "not_requested"),
			toStr(issueOutcome["copilot_assignment_reason"]),
			mapBool(issueOutcome, "pr_linked"), mapInt(issueOutcome, "pr_number"),
			toStr(issueOutcome["pr_url"]))
	}

	updateRun(map[string]any{
		"Status": "completed", "GuardDecision": guardDecision, "DlpResult": dlpResult,
		"Analysis": analysis, "Suggestion": suggestion, "GithubIssueUrl": githubIssueURL,
		"CompletedAt": normalizeCHTimestampNow(),
	})
	emitAgentIssueDecisionSummary(runID, rule, tctx, issueOutcome, githubIssueURL, wantsIssue, wantsCopilot, githubRepo)
	return jsonenc.NewObject().
		Set("status", "completed").
		Set("guard_decision", guardDecision).
		Set("dlp_result", dlpResult).
		Set("analysis", analysis).
		Set("suggestion", suggestion).
		Set("github_issue_url", githubIssueURL).
		Set("dedup_decision", toStr(issueOutcome["dedup_decision"])).
		Set("issue_error", toStr(issueOutcome["issue_error"])).
		Set("copilot_assignment_status", toStr(issueOutcome["copilot_assignment_status"])).
		Set("copilot_assignment_reason", toStr(issueOutcome["copilot_assignment_reason"])), nil
}

// checkDLPEndpoint mirrors app.py _check_dlp_endpoint: POST {"text": text} to an optional DLP
// endpoint and parse a flagged / pii_detected / blocked verdict. Returns (clean, detail). An empty
// dlpURL is (true, "skipped"). Any failure — network error, HTTP >= 400 (resp.raise_for_status),
// or a non-object JSON body (resp.json().get raising) — fails OPEN as (true, "dlp_unavailable"), so
// a broken/unreachable DLP service never blocks issue creation, byte-identical to Python. The
// upstream client is shared with the GitHub/OSV/LLM calls (parity-mocked, URL-keyed).
func (s *server) checkDLPEndpoint(dlpURL, text, apiKey string) (bool, string) {
	if dlpURL == "" {
		return true, "skipped"
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	reqBody := jsonenc.Encode(jsonenc.NewObject().Set("text", text), dumpsDefault)
	resp, err := s.upstreamRequest("POST", dlpURL, reqBody, headers)
	if err != nil || resp.Status >= 400 {
		return true, "dlp_unavailable"
	}
	body, ok := resp.Body.(*jsonenc.Object)
	if !ok {
		return true, "dlp_unavailable"
	}
	flagged := objTruthy(body, "flagged") || objTruthy(body, "pii_detected") || objTruthy(body, "blocked")
	// detail = str(body.get("detail") or body.get("reason") or ("flagged" if flagged else "clean"))
	detail := "clean"
	if flagged {
		detail = "flagged"
	}
	if dv, present := body.Get("detail"); isTruthyVal(dv, present) {
		detail = pyStr(dv, present)
	} else if rv, present := body.Get("reason"); isTruthyVal(rv, present) {
		detail = pyStr(rv, present)
	}
	return !flagged, detail
}

// buildAgentIssueTitle mirrors _build_agent_issue_title (the title is sent to GitHub, never returned).
func buildAgentIssueTitle(rule *agentRule, tf triggerFields) string {
	anomalyState := strings.TrimSpace(tf.anomalyState)
	if anomalyState == "" {
		anomalyState = "detected"
	}
	focus := strings.TrimSpace(tf.serviceName)
	if focus == "" {
		focus = rule.name
		if focus == "" {
			focus = "Agent Rule"
		}
	}
	signalSource := strings.TrimSpace(tf.signalSource)
	signalName := strings.TrimSpace(tf.signalName)
	if signalSource != "" && signalName != "" {
		return "[SOBS Agent] " + focus + " — " + signalSource + "/" + signalName + " " + anomalyState + " anomaly"
	}
	return "[SOBS Agent] " + focus + " — " + anomalyState + " state detected"
}

// buildAgentIssueBody mirrors the markdown body assembled in _run_agent_flow (sent to GitHub, never
// returned). Masking of the title/body is applied later in createGithubIssueRecord, gated on the
// trigger-derived mask_output_enabled threaded through chooseGithubIssueOutcome.
func buildAgentIssueBody(rule *agentRule, tctx *jsonenc.Object, tf triggerFields, contextSummary, analysis, suggestion string) string {
	additionalContext := strings.TrimSpace(extractTriggerAdditionalContext(tctx))
	additionalSection := ""
	if additionalContext != "" {
		additionalSection = "\n### Additional Context\n" + additionalContext + "\n"
	}
	ruleName := rule.name
	if ruleName == "" {
		ruleName = "Agent Rule"
	}
	return "## SOBS Automated Agent Report\n\n" +
		"**Rule:** " + ruleName + "  \n" +
		"**Trigger state:** " + objGetStr(tctx, "trigger_state") + "  \n" +
		"**Service:** " + tf.serviceName + "  \n" +
		"**Signal:** " + tf.signalSource + "/" + tf.signalName + "  \n\n" +
		"### Telemetry Context\n```\n" + contextSummary + "\n```\n\n" +
		"### Root Cause Analysis\n" + analysis + "\n\n" +
		"### Suggested Fix\n" + suggestion + "\n" +
		additionalSection + "\n" +
		"---\n*Generated automatically by [SOBS](https://github.com/abartrim/sobs). " +
		"Please review before acting.*"
}

// extractTriggerAdditionalContext reads extra.additional_context from a trigger context whose `extra`
// is either a nested object (user-observation flow) or a JSON string (manual flow).
func extractTriggerAdditionalContext(tctx *jsonenc.Object) string {
	v, ok := tctx.Get("extra")
	if !ok {
		return ""
	}
	switch x := v.(type) {
	case *jsonenc.Object:
		return objGetStr(x, "additional_context")
	case string:
		if parsed, err := parseJSONValue([]byte(x)); err == nil {
			if o, ok := parsed.(*jsonenc.Object); ok {
				return objGetStr(o, "additional_context")
			}
		}
	}
	return ""
}

const agentMaxIssuesDefault = 5

// countGithubIssuesLastHour mirrors _count_github_issues_last_hour.
func (s *server) countGithubIssuesLastHour() int {
	return s.countRows("SELECT count() FROM sobs_agent_runs FINAL WHERE IsDeleted=0 AND GithubIssueUrl != '' " +
		"AND CreatedAt >= now() - INTERVAL 1 HOUR")
}

// handleTriggerAgentRun mirrors app.py trigger_agent_run (POST /api/agent/runs): validate rule_id,
// load the rule, gate on AI config, rate-limit, then run the agent flow.
func (s *server) handleTriggerAgentRun(w http.ResponseWriter, r *http.Request) {
	body := bodyMap(r)
	ruleID := strings.TrimSpace(bstr(body, "rule_id"))
	extraContext := strings.TrimSpace(bstr(body, "extra_context"))
	if ruleID == "" {
		s.errorJSON(w, http.StatusBadRequest, "rule_id is required")
		return
	}
	rule := s.loadAgentRule(ruleID)
	if rule == nil {
		s.errorJSON(w, http.StatusNotFound, "agent rule not found")
		return
	}
	// app.py trigger_agent_run uses _load_all_ai_settings(db) — the full settings surface, so the
	// flow sees system_prompt / thinking_level / dlp / agent max-* / copilot knobs.
	settings := s.loadAllAISettings()
	if settings["ai.endpoint_url"] == "" || settings["ai.model"] == "" {
		s.errorJSON(w, http.StatusServiceUnavailable, "AI endpoint not configured. Visit Settings → AI Configuration.")
		return
	}
	rateLimitMinutes := rule.rateLimitMinutes
	lastRunTs := s.agentRuleLastRunTs(ruleID)
	elapsedMinutes := (float64(nowUTC().Unix()) - lastRunTs) / 60.0
	if elapsedMinutes < float64(rateLimitMinutes) && lastRunTs > 0 {
		s.errorJSON(w, http.StatusTooManyRequests, fmt.Sprintf(
			"Rate limit: this rule ran %.0fm ago (limit: every %dm)", elapsedMinutes, rateLimitMinutes))
		return
	}
	tctx := agentTriggerContext(rule, extraContext)
	outcome := s.runAgentRuleInstance(rule, settings, tctx)
	if !outcome.ok {
		writeJSON(w, http.StatusInternalServerError, jsonenc.NewObject().
			Set("ok", false).Set("error", outcome.err).Set("run_id", outcome.runID))
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("run_id", outcome.runID).Set("result", outcome.result))
}

// buildUserIssueTriggerContext mirrors app.py _build_user_issue_trigger_context: derive the signal
// fields + extra context from the raise-issue payload. The derived service/signal/state feed the
// dedup key and issue title, so they must match Python exactly.
func buildUserIssueTriggerContext(sourcePage string, body map[string]any) *jsonenc.Object {
	source := strings.ToLower(strings.TrimSpace(sourcePage))
	if source != "errors" && source != "traces" && source != "incident" {
		source = "errors"
	}
	service := strings.TrimSpace(bstr(body, "service"))
	traceID := strings.TrimSpace(bstr(body, "trace_id"))
	spanID := strings.TrimSpace(bstr(body, "span_id"))
	errorID := strings.TrimSpace(bstr(body, "error_id"))
	errType := strings.TrimSpace(bstr(body, "err_type"))
	spanName := strings.TrimSpace(bstr(body, "span_name"))
	status := strings.TrimSpace(bstr(body, "status"))
	message := strings.TrimSpace(bstr(body, "message"))
	stack := strings.TrimSpace(bstr(body, "stack"))

	var signalSource, signalName, anomalyState, triggerRefID string
	var signalValue float64
	switch source {
	case "traces":
		signalSource = "traces"
		signalName = orFirstNonEmpty(spanName, "trace_span")
		if strings.Contains(strings.ToUpper(status), "ERROR") {
			anomalyState = "critical"
		} else {
			anomalyState = "warning"
		}
		signalValue = parseFloatDefault(bstr(body, "duration_ms"), 0.0)
		triggerRefID = orFirstNonEmpty(traceID, spanID)
	case "incident":
		signalSource = "incident"
		signalName = orFirstNonEmpty(errType, orFirstNonEmpty(spanName, "incident_packet"))
		if errorID != "" || strings.Contains(strings.ToUpper(status), "ERROR") {
			anomalyState = "critical"
		} else {
			anomalyState = "warning"
		}
		signalValue = parseFloatDefault(bstr(body, "duration_ms"), 1.0)
		triggerRefID = orFirstNonEmpty(errorID, orFirstNonEmpty(traceID, spanID))
	default: // errors
		signalSource = "errors"
		signalName = orFirstNonEmpty(errType, "exception")
		anomalyState = "critical"
		signalValue = 1.0
		triggerRefID = errorID
	}

	extra := jsonenc.NewObject().
		Set("initiated_by", "user").
		Set("source_page", source).
		Set("source", signalSource).
		Set("signal", signalName).
		Set("state", anomalyState).
		Set("value", signalValue).
		Set("service", service).
		Set("trace_id", traceID).
		Set("span_id", spanID).
		Set("error_id", errorID).
		Set("err_type", errType).
		Set("message", truncRunes(message, 1200)).
		Set("stack", truncRunes(stack, 3000)).
		Set("url", strings.TrimSpace(bstr(body, "url"))).
		Set("timestamp", strings.TrimSpace(bstr(body, "timestamp"))).
		Set("additional_context", truncRunes(strings.TrimSpace(bstr(body, "additional_context")), 2000))

	return jsonenc.NewObject().
		Set("rule_name", "User Raised Issue ("+source+")").
		Set("trigger_state", anomalyState).
		Set("trigger_type", "manual").
		Set("trigger_ref_id", triggerRefID).
		Set("service", service).
		Set("extra", extra)
}

// parseFloatDefault mirrors `float(value or default)` for the (parity-invisible) duration_ms field;
// a blank/unparseable value yields the default. JSON-number payloads reach bstr as text under the
// parity harness; the trace/incident branches are untested in the corpus.
func parseFloatDefault(raw string, def float64) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}
	return def
}

// handleApiIssuesRaise mirrors app.py raise_issue_from_user_observation: gate on AI config, then run
// the agent flow (analyze + github_issue + dlp_check) for a synthetic user-observation rule.
func (s *server) handleApiIssuesRaise(w http.ResponseWriter, r *http.Request) {
	body := bodyMap(r)
	sourcePage := strings.ToLower(strings.TrimSpace(orDefault(bstr(body, "source_page"), "errors")))
	assignCopilot := bodyBool(body, "assign_copilot", false)
	// app.py raise_issue_from_user_observation uses _load_all_ai_settings(db) — the full surface.
	settings := s.loadAllAISettings()
	if strings.TrimSpace(settings["ai.endpoint_url"]) == "" || strings.TrimSpace(settings["ai.model"]) == "" {
		writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().
			Set("ok", false).Set("error", "AI endpoint not configured. Visit Settings -> AI Configuration."))
		return
	}
	tctx := buildUserIssueTriggerContext(sourcePage, body)
	if ev, ok := tctx.Get("extra"); ok {
		if eo, ok := ev.(*jsonenc.Object); ok {
			eo.Set("mask_output", bodyBool(body, "mask_output", true))
		}
	}
	githubRepo, githubToken := s.resolveAgentGithubTarget(settings, tctx)
	if githubRepo == "" || githubToken == "" {
		writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().Set("ok", false).
			Set("error", "GitHub repo/token not configured for issue creation. Visit Settings -> AI Configuration."))
		return
	}
	actions := []string{"analyze", "github_issue", "dlp_check"}
	if assignCopilot {
		actions = append(actions, "github_issue_copilot")
	}
	rule := &agentRule{id: "user-observation-" + sourcePage, name: "User Raised Issue (" + sourcePage + ")",
		actions: actions, rateLimitMinutes: 0}
	outcome := s.runAgentRuleInstance(rule, settings, tctx)
	if !outcome.ok {
		writeJSON(w, http.StatusInternalServerError, jsonenc.NewObject().
			Set("ok", false).Set("error", orDefault(outcome.err, "agent flow failed")).Set("run_id", outcome.runID))
		return
	}
	res := outcome.result
	issueURL := objGetStr(res, "github_issue_url")
	dedupDecision := objGetStr(res, "dedup_decision")
	issueError := strings.TrimSpace(objGetStr(res, "issue_error"))
	if owner, repo, num := parseIssueRefFromURL(issueURL); owner == "" || repo == "" || num <= 0 {
		issueError = orDefault(issueError, "Agent returned an invalid issue URL")
		dedupDecision = "create_failed"
		issueURL = ""
	}
	if issueURL == "" && dedupDecision == "create_failed" {
		writeJSON(w, http.StatusBadGateway, jsonenc.NewObject().Set("ok", false).
			Set("error", orDefault(issueError, "GitHub issue creation failed. Check repository settings and token scopes.")).
			Set("run_id", outcome.runID).Set("source", "user").Set("source_page", sourcePage))
		return
	}
	if issueURL == "" && dedupDecision == "suppressed_rate_limit" {
		writeJSON(w, http.StatusTooManyRequests, jsonenc.NewObject().Set("ok", false).
			Set("error", "GitHub issue creation suppressed by hourly limit. Try again later.").
			Set("run_id", outcome.runID).Set("source", "user").Set("source_page", sourcePage))
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("run_id", outcome.runID).Set("source", "user").Set("source_page", sourcePage).
		Set("issue_url", issueURL).Set("dedup_decision", dedupDecision).
		Set("copilot_assignment_status", objGetStr(res, "copilot_assignment_status")).
		Set("copilot_assignment_reason", objGetStr(res, "copilot_assignment_reason")).
		Set("status", objGetStr(res, "status")))
}

var issueRefRE = regexp.MustCompile(`github\.com/([^/]+)/([^/]+)/issues/(\d+)`)

// parseIssueRefFromURL mirrors _parse_issue_ref_from_url.
func parseIssueRefFromURL(issueURL string) (string, string, int) {
	m := issueRefRE.FindStringSubmatch(issueURL)
	if m == nil {
		return "", "", 0
	}
	n, _ := strconv.Atoi(m[3])
	return m[1], m[2], n
}

// normalizeCHTimestampNow mirrors _normalize_ch_timestamp(datetime.now(timezone.utc)).
func normalizeCHTimestampNow() string { return nowUTC().Format("2006-01-02 15:04:05.000000") }

// buildAgentContextSummary mirrors _build_agent_context_summary: a plain-text snapshot of current
// observability state fed to the guard + analysis LLM calls.
func (s *server) buildAgentContextSummary(tctx *jsonenc.Object) string {
	lines := []string{"=== SOBS Observability Context ==="}
	ruleName := objGetStr(tctx, "rule_name")
	if ruleName == "" {
		ruleName = "unknown rule"
	}
	lines = append(lines, "Triggered by: "+ruleName+" ("+objGetStr(tctx, "trigger_state")+")")

	extraRaw := objGetStr(tctx, "extra")
	extra := map[string]string{}
	if extraRaw != "" {
		if parsed, err := parseJSONValue([]byte(extraRaw)); err == nil {
			if o, ok := parsed.(*jsonenc.Object); ok {
				for _, k := range o.Keys() {
					extra[k] = objGetStr(o, k)
				}
			}
		}
	}
	if ac := strings.TrimSpace(extra["additional_context"]); ac != "" {
		lines = append(lines, "\nUser-provided context: "+ac)
	}

	// Recent errors (last 1h, all services).
	if res, err := s.db.Execute(
		"SELECT ServiceName, ExceptionType, count() AS c FROM otel_logs FINAL " +
			"WHERE Timestamp >= now() - INTERVAL 1 HOUR AND SeverityText IN ('ERROR','FATAL') " +
			"GROUP BY ServiceName, ExceptionType ORDER BY c DESC LIMIT 5"); err == nil {
		rows := rowMaps(res)
		if len(rows) > 0 {
			lines = append(lines, "\nRecent errors (last 1h, all services):")
			for _, r := range rows {
				lines = append(lines, "  "+cStr(r, "ServiceName")+" | "+cStr(r, "ExceptionType")+" x"+cStr(r, "c"))
			}
		}
	}

	// Active anomalies (last 2h).
	if res, err := s.db.Execute(
		"SELECT ServiceName, Name AS Signal, anomaly_state FROM v_derived_signals_anomaly " +
			"WHERE anomaly_state != 'normal' AND time >= now() - INTERVAL 2 HOUR LIMIT 5"); err == nil {
		rows := rowMaps(res)
		if len(rows) > 0 {
			lines = append(lines, "\nActive anomalies:")
			for _, r := range rows {
				lines = append(lines, "  "+cStr(r, "ServiceName")+" | "+cStr(r, "Signal")+" → "+cStr(r, "anomaly_state"))
			}
		}
	}

	if extraRaw != "" && len(extra) == 0 {
		lines = append(lines, "\nAdditional context: "+extraRaw)
	}
	return strings.Join(lines, "\n")
}

// resolveAgentGithubTarget mirrors _resolve_agent_github_target: resolve (repo, token) for agent
// GitHub issue creation. For the analyze-only parity rule no service maps to an app and no global
// repo is set, so it returns ("", "").
func (s *server) resolveAgentGithubTarget(settings map[string]string, tctx *jsonenc.Object) (string, string) {
	defaultRepo := strings.TrimSpace(settings["ai.github_repo"])
	defaultToken := strings.TrimSpace(settings["ai.github_token"])

	serviceName := extractTriggerServiceName(tctx)
	if serviceName != "" {
		if res, err := s.db.Execute(
			"SELECT RepoUrl FROM sobs_apps FINAL WHERE IsDeleted=0 AND Enabled=1 AND RepoUrl != '' "+
				"AND (lower(Name)=lower(?) OR lower(Slug)=lower(?)) ORDER BY UpdatedAt DESC LIMIT 1",
			serviceName, serviceName); err == nil && len(res.Rows) > 0 {
			owner, repo := parseGithubRepoOwnerName(cStr(rowMaps(res)[0], "RepoUrl"))
			if owner != "" && repo != "" {
				scoped := s.repoScopedGithubToken(owner, repo)
				if scoped == "" {
					scoped = defaultToken
				}
				return owner + "/" + repo, scoped
			}
		}
	}
	if defaultRepo != "" {
		owner, repo := parseGithubRepoOwnerName(defaultRepo)
		if owner == "" || repo == "" {
			parts := []string{}
			for _, p := range strings.Split(strings.Trim(defaultRepo, "/"), "/") {
				if p != "" {
					parts = append(parts, p)
				}
			}
			if len(parts) >= 2 {
				owner, repo = parts[len(parts)-2], parts[len(parts)-1]
			}
		}
		if owner != "" && repo != "" {
			scoped := s.repoScopedGithubToken(owner, repo)
			if scoped == "" {
				scoped = defaultToken
			}
			return owner + "/" + repo, scoped
		}
		return defaultRepo, defaultToken
	}
	return "", defaultToken
}

// extractTriggerServiceName mirrors _extract_trigger_service_name.
func extractTriggerServiceName(tctx *jsonenc.Object) string {
	if v := strings.TrimSpace(objGetStr(tctx, "service")); v != "" {
		return v
	}
	extraRaw := objGetStr(tctx, "extra")
	if extraRaw == "" {
		return ""
	}
	parsed, err := parseJSONValue([]byte(extraRaw))
	if err != nil {
		return ""
	}
	o, ok := parsed.(*jsonenc.Object)
	if !ok {
		return ""
	}
	for _, key := range []string{"service", "service_name", "ServiceName"} {
		if v := strings.TrimSpace(objGetStr(o, key)); v != "" {
			return v
		}
	}
	return ""
}

// parseAgentAnalysis mirrors the reply parsing in _run_agent_flow's analyze step.
func parseAgentAnalysis(reply string) (analysis, suggestion string) {
	if strings.Contains(reply, "SUGGESTED FIX:") {
		parts := strings.SplitN(reply, "SUGGESTED FIX:", 2)
		analysis = strings.TrimSpace(strings.ReplaceAll(parts[0], "ROOT CAUSE:", ""))
		suggestion = strings.TrimSpace(parts[1])
	} else {
		analysis = strings.TrimSpace(reply)
	}
	if strings.HasPrefix(analysis, "NOISE_OR_IMPACT:") {
		if nl := strings.Index(analysis, "\n"); nl != -1 {
			analysis = strings.TrimSpace(analysis[nl:])
		} else {
			analysis = ""
		}
	}
	return analysis, suggestion
}
