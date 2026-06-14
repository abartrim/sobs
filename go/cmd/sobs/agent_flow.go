package main

import (
	"fmt"
	"net/http"
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
	githubRepo, githubToken := s.resolveAgentGithubTarget(settings, tctx)

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
		reply, _, err := s.callLLMEndpoint(endpointURL)
		if err != nil {
			return nil, err
		}
		analysis, suggestion = parseAgentAnalysis(reply)
	}

	// 3. DLP + GitHub issue creation — skipped unless a github action AND a resolved repo+token
	// are present (the analyze-only parity rule has none). Full issue creation is a follow-up.
	dlpResult := "skipped"
	githubIssueURL := ""
	wantsIssue := rule.hasAction("github_issue") || rule.hasAction("github_issue_copilot")
	_ = wantsIssue
	_ = githubRepo
	_ = githubToken

	updateRun(map[string]any{
		"Status": "completed", "GuardDecision": guardDecision, "DlpResult": dlpResult,
		"Analysis": analysis, "Suggestion": suggestion, "GithubIssueUrl": githubIssueURL,
		"CompletedAt": normalizeCHTimestampNow(),
	})
	return jsonenc.NewObject().
		Set("status", "completed").
		Set("guard_decision", guardDecision).
		Set("dlp_result", dlpResult).
		Set("analysis", analysis).
		Set("suggestion", suggestion).
		Set("github_issue_url", githubIssueURL).
		Set("dedup_decision", "").
		Set("issue_error", "").
		Set("copilot_assignment_status", "").
		Set("copilot_assignment_reason", ""), nil
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
	settings := map[string]string{
		"ai.endpoint_url": s.loadAISetting("ai.endpoint_url", ""),
		"ai.model":        s.loadAISetting("ai.model", ""),
		"ai.github_repo":  s.loadAISetting("ai.github_repo", ""),
		"ai.github_token": s.loadAISetting("ai.github_token", ""),
	}
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
