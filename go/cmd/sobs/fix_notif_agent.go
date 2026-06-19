package main

import (
	"log"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// aiSettingKeys mirrors app.py _AI_SETTING_KEYS (insertion order preserved; only membership matters
// for _load_all_ai_settings since the result is keyed by name).
var aiSettingKeys = []string{
	"ai.endpoint_url",
	"ai.model",
	"ai.thinking_level",
	"ai.api_key",
	"ai.endpoint_timeout_seconds",
	"ai.guard_endpoint_url",
	"ai.guard_model",
	"ai.guard_thinking_level",
	"ai.guard_timeout_seconds",
	"ai.dlp_endpoint_url",
	"ai.github_token",
	"ai.github_token_expires_at",
	"ai.github_token_last_validated_at",
	"ai.github_token_last_validation_status",
	"ai.github_token_last_validation_message",
	"ai.github_repo",
	"ai.agent_max_issues_per_hour",
	"ai.agent_max_assignments_per_hour",
	"ai.agent_max_active_assignments",
	"ai.github_copilot_base_branch",
	"ai.github_copilot_custom_instructions",
	"ai.system_prompt",
	"ai.model_pricing",
	"ai.model_pricing_confirmed",
}

// loadAllAISettings mirrors app.py _load_all_ai_settings: read every sobs_ai_settings row, keep only
// the _AI_SETTING_KEYS (decrypting sensitive ones), defaulting any missing key to "", then apply the
// env-override precedence (DB value first, then file-backed env, then direct env). Used by the agent
// trigger / manual-trigger / raise-issue paths so they expose the full settings surface (DLP, agent
// max-* limits, copilot base-branch/custom-instructions, system_prompt, thinking_level), matching
// the oracle. On the empty/unconfigured parity fixture every value resolves to "", identical to the
// single-key loads it replaces for the gating checks.
func (s *server) loadAllAISettings() map[string]string {
	result := make(map[string]string, len(aiSettingKeys))
	keySet := make(map[string]bool, len(aiSettingKeys))
	for _, k := range aiSettingKeys {
		result[k] = ""
		keySet[k] = true
	}
	if s.db != nil {
		if res, err := s.db.Execute("SELECT Key, Value FROM sobs_ai_settings FINAL WHERE IsDeleted=0"); err == nil {
			for _, m := range rowMaps(res) {
				k := cStr(m, "Key")
				if !keySet[k] {
					continue
				}
				raw := cStr(m, "Value")
				if isSensitiveAISettingKey(k) {
					raw = s.decryptSecretValue(raw)
				}
				result[k] = raw
			}
		}
	}
	// Precedence: DB value first, then file-backed env, then direct env.
	for key, envName := range aiEnvOverrides {
		if result[key] != "" {
			continue
		}
		if v := readFileOrEnv(envName, aiEnvFileOverrides[key]); v != "" {
			result[key] = v
		}
	}
	return result
}

// persistGithubWorkItem mirrors app.py _persist_github_work_item: record a GitHub issue *decision*
// (run-keyed, with the real agent/anomaly/dedup/copilot/PR fields) as a work item. This is the
// agent-flow persist path (distinct from the onboarding-wizard persist), so the work item carries
// AgentRunId=run_id and the trigger-derived telemetry, not the onboarding placeholder values. The
// Python _invalidate_work_items_cache() call is a no-op here (the Go port keeps no such cache).
func (s *server) persistGithubWorkItem(runID string, rule *agentRule, tctx *jsonenc.Object,
	githubIssueURL, analysis, suggestion, agentAction, issueTitle, issueState, dedupKey, dedupDecision string,
	dedupConfidence float64, canonicalIssueURL string, canonicalIssueNumber int, relatedIssueURLs []string,
	occurrenceCount, copilotAssignmentRequestedAt int, copilotAssignmentStatus, copilotAssignmentReason string,
	prLinked bool, prNumber int, prURL string) {

	now := normalizeCHTimestampNow()

	// issue_number from the trailing path segment of github_issue_url (when numeric).
	issueNumber := 0
	if parts := strings.Split(strings.TrimRight(githubIssueURL, "/"), "/"); len(parts) > 0 {
		last := parts[len(parts)-1]
		if last != "" {
			if n, err := strconv.Atoi(last); err == nil {
				issueNumber = n
			}
		}
	}

	tf := extractAgentTriggerFields(tctx)

	// github_repo derived from the canonical (or fired) issue URL: f"{parts[-4]}/{parts[-3]}".
	githubRepo := ""
	issueSourceURL := canonicalIssueURL
	if issueSourceURL == "" {
		issueSourceURL = githubIssueURL
	}
	if parts := strings.Split(issueSourceURL, "/"); len(parts) >= 4 {
		githubRepo = parts[len(parts)-4] + "/" + parts[len(parts)-3]
	}

	canonicalNumber := canonicalIssueNumber
	if canonicalNumber == 0 {
		canonicalNumber = issueNumber
	}
	resolvedIssueURL := githubIssueURL
	if resolvedIssueURL == "" {
		resolvedIssueURL = canonicalIssueURL
	}
	canonicalURLOut := canonicalIssueURL
	if canonicalURLOut == "" {
		canonicalURLOut = resolvedIssueURL
	}

	if dedupDecision == "" {
		dedupDecision = "new_issue"
	}
	// OccurrenceCount = max(1, int(occurrence_count or 1)).
	if occurrenceCount < 1 {
		occurrenceCount = 1
	}

	// RelatedIssueUrls = _safe_json_dumps(related_issue_urls or []): json.dumps(list, ensure_ascii=False).
	related := relatedIssueURLs
	if related == nil {
		related = []string{}
	}
	relatedJSON := string(jsonenc.Encode(stringsToAny(related), dumpsDefault))

	_, _ = s.insertRowsNormalized("sobs_github_work_items", []map[string]any{{
		"Id": runID, "CreatedAt": now, "CompletedAt": now,
		"AgentRunId": runID, "AgentRuleId": rule.id, "AgentRuleName": rule.name,
		"AgentAction": agentAction, "ServiceName": tf.serviceName,
		"AnomalyRuleId": tf.anomalyRuleID, "AnomalyState": tf.anomalyState,
		"SignalSource": tf.signalSource, "SignalName": tf.signalName, "SignalValue": tf.signalValue,
		"GithubRepo": githubRepo, "DedupKey": dedupKey, "DedupDecision": dedupDecision,
		"DedupConfidence": dedupConfidence, "IssueNumber": issueNumber, "IssueUrl": resolvedIssueURL,
		"CanonicalIssueNumber": canonicalNumber, "CanonicalIssueUrl": canonicalURLOut,
		"RelatedIssueUrls": relatedJSON, "OccurrenceCount": occurrenceCount,
		"IssueState": issueState, "IssueTitle": issueTitle,
		"AnalysisSummary": truncRunes(analysis, 500), "SuggestionSummary": truncRunes(suggestion, 500),
		"CopilotAssignmentRequestedAt": copilotAssignmentRequestedAt,
		"CopilotAssignmentStatus":      copilotAssignmentStatus, "CopilotAssignmentReason": copilotAssignmentReason,
		"PrLinked": boolToInt(prLinked), "PrNumber": prNumber, "PrUrl": prURL,
		"IsDeleted": 0, "Version": fixedVersionMillis(),
	}})
	// app.py calls _invalidate_work_items_cache() here; the Go port keeps no work-items page/filter
	// cache (every work-items request reads sobs_github_work_items fresh), so there is nothing to
	// invalidate — the post-write read already reflects this row, byte-identical to the oracle.
}

// emitAgentIssueDecisionSummary mirrors app.py _emit_agent_issue_decision_summary: a structured
// info log of the issue decision (no-op when the rule did not request an issue).
func emitAgentIssueDecisionSummary(runID string, rule *agentRule, tctx *jsonenc.Object,
	issueOutcome map[string]any, githubIssueURL string, wantsIssue, wantsCopilot bool, githubRepo string) {
	if !wantsIssue {
		return
	}
	issueURL := githubIssueURL
	if issueURL == "" {
		issueURL = toStr(issueOutcome["issue_url"])
	}
	summary := jsonenc.NewObject().
		Set("run_id", runID).
		Set("rule_id", rule.id).
		Set("rule_name", rule.name).
		Set("trigger_type", objGetStr(tctx, "trigger_type")).
		Set("trigger_ref_id", objGetStr(tctx, "trigger_ref_id")).
		Set("github_repo", githubRepo).
		Set("issue_url", issueURL).
		Set("dedup_decision", toStr(issueOutcome["dedup_decision"])).
		Set("dedup_confidence", toFloatAny(issueOutcome["dedup_confidence"])).
		Set("copilot_requested", wantsCopilot).
		Set("copilot_assignment_status", toStr(issueOutcome["copilot_assignment_status"])).
		Set("copilot_assignment_reason", toStr(issueOutcome["copilot_assignment_reason"])).
		Set("created_new_issue", mapBool(issueOutcome, "created_new_issue")).
		Set("occurrence_count", mapInt(issueOutcome, "occurrence_count"))
	log.Printf("agent_issue_decision_summary %s", string(jsonenc.Encode(summary, dumpsDefault)))
}

// mapBool reads a bool from an outcome map (the dedup outcome stores Go bools).
func mapBool(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// copilotAssignmentParams mirrors the base_branch + custom_instructions construction shared by both
// _choose_github_issue_outcome assignment branches (app.py 5538-5548 / 5631-5641): base_branch from
// ai.github_copilot_base_branch; custom_instructions from ai.github_copilot_custom_instructions, with
// the (truncated) suggested fix appended when a suggestion is present.
func copilotAssignmentParams(settings map[string]string, suggestion string) (baseBranch, customInstructions string) {
	baseBranch = strings.TrimSpace(settings["ai.github_copilot_base_branch"])
	customInstructions = strings.TrimSpace(settings["ai.github_copilot_custom_instructions"])
	if suggestion != "" {
		prefix := ""
		if customInstructions != "" {
			prefix = customInstructions + "\n\n"
		}
		customInstructions = prefix + "Use this suggested fix guidance when relevant:\n" + truncRunes(suggestion, 1500)
	}
	return baseBranch, customInstructions
}

// extractTriggerMaskOutput mirrors the mask_output_enabled derivation in app.py _run_agent_flow
// (6794-6801): default True; when `extra` is a nested object, _parse_bool(extra["mask_output"], True);
// when `extra` is a (non-empty) JSON string, parse it and apply the same. A missing/blank key keeps
// the default True, so the masking behaviour is unchanged unless the caller explicitly sets it.
func extractTriggerMaskOutput(tctx *jsonenc.Object) bool {
	v, ok := tctx.Get("extra")
	if !ok {
		return true
	}
	switch x := v.(type) {
	case *jsonenc.Object:
		mv, present := x.Get("mask_output")
		if !present {
			return true
		}
		return parseBoolPy(mv, true)
	case string:
		if strings.TrimSpace(x) == "" {
			return true
		}
		if parsed, err := parseJSONValue([]byte(x)); err == nil {
			if o, ok := parsed.(*jsonenc.Object); ok {
				mv, present := o.Get("mask_output")
				if !present {
					return true
				}
				return parseBoolPy(mv, true)
			}
		}
	}
	return true
}
