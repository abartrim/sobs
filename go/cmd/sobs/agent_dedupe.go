package main

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// GitHub issue dedup/reuse subsystem — ports the populated-data branch of app.py
// _choose_github_issue_outcome (and its helpers) that the empty parity corpus never
// exercised. When prior work items + matching open issues exist, the agent flow classifies
// the proposed incident against them (LLM, with a deterministic local fallback) and reuses an
// existing issue instead of always opening a new one. See app.py 5388-5646, 6241-6377.

// Copilot/issue rate-limit defaults (app.py _AI_AGENT_MAX_*_DEFAULT). agentMaxIssuesDefault
// lives in agent_flow.go.
const (
	agentMaxAssignmentsPerHourDefault = 1 // _AI_AGENT_MAX_ASSIGNMENTS_PER_HOUR_DEFAULT
	agentMaxActiveAssignmentsDefault  = 1 // _AI_AGENT_MAX_ACTIVE_ASSIGNMENTS_DEFAULT
)

var (
	// _normalize_issue_match_text: collapse every run of non-[a-z0-9] (after lowercasing) to a space.
	issueMatchNonAlnumRE = regexp.MustCompile(`[^a-z0-9]+`)
	// _extract_first_json_object fence + first-object regexes (DOTALL).
	jsonFenceRE = regexp.MustCompile("(?s)^```(?:json)?\\s*|\\s*```$")
	jsonBraceRE = regexp.MustCompile(`(?s)\{.*\}`)
)

// triggerFields mirrors the dict from _extract_agent_trigger_fields (the keys the dedup path reads).
type triggerFields struct {
	serviceName   string
	anomalyRuleID string
	anomalyState  string
	signalSource  string
	signalName    string
	signalValue   float64
}

// dedupeCandidate mirrors a candidate dict built inside _choose_github_issue_outcome (a local
// work item merged with its still-open GitHub issue, or an open issue with no local row).
type dedupeCandidate struct {
	candidateID             string
	issueURL                string
	issueNumber             int
	issueTitle              string
	issueBody               string
	issueState              string
	serviceName             string
	signalSource            string
	signalName              string
	anomalyState            string
	dedupKey                string
	copilotAssignmentStatus string
	prLinked                bool
	prURL                   string
	assignees               []string
}

// normalizeIssueMatchText mirrors app.py _normalize_issue_match_text.
func normalizeIssueMatchText(value any) string {
	s := issueMatchNonAlnumRE.ReplaceAllString(strings.ToLower(toStr(value)), " ")
	return strings.Join(strings.Fields(s), " ")
}

// extractAgentTriggerFields mirrors app.py _extract_agent_trigger_fields. The trigger context's
// `extra` may be a nested object (user-observation flow) or a JSON string (manual flow).
func extractAgentTriggerFields(tctx *jsonenc.Object) triggerFields {
	extra := jsonenc.NewObject()
	if v, ok := tctx.Get("extra"); ok {
		switch x := v.(type) {
		case *jsonenc.Object:
			extra = x
		case string:
			if parsed, err := parseJSONValue([]byte(x)); err == nil {
				if o, ok := parsed.(*jsonenc.Object); ok {
					extra = o
				}
			}
		}
	}
	return triggerFields{
		serviceName:   strings.TrimSpace(orFirstNonEmpty(objGetStr(extra, "service"), objGetStr(tctx, "service"))),
		anomalyRuleID: strings.TrimSpace(objGetStr(tctx, "trigger_ref_id")),
		anomalyState:  strings.TrimSpace(orFirstNonEmpty(objGetStr(extra, "state"), objGetStr(tctx, "trigger_state"))),
		signalSource:  strings.TrimSpace(objGetStr(extra, "source")),
		signalName:    strings.TrimSpace(objGetStr(extra, "signal")),
		signalValue:   objGetFloat(extra, "value"),
	}
}

// buildGithubWorkItemDedupKey mirrors app.py _build_github_work_item_dedup_key.
func buildGithubWorkItemDedupKey(githubRepo string, tf triggerFields) string {
	parts := []string{
		normalizeIssueMatchText(githubRepo),
		normalizeIssueMatchText(tf.serviceName),
		normalizeIssueMatchText(tf.signalSource),
		normalizeIssueMatchText(tf.signalName),
		normalizeIssueMatchText(tf.anomalyState),
	}
	return strings.Trim(strings.Join(parts, "|"), "|")
}

// loadRecentWorkItemCandidates mirrors app.py _load_recent_work_item_candidates: the most-recent
// non-deleted work items for this repo that already have an IssueUrl, serialized identically to
// the work-items API row so the candidate-merge reads the same keys as Python.
func (s *server) loadRecentWorkItemCandidates(githubRepo string, limit int) []*jsonenc.Object {
	if limit < 1 {
		limit = 1
	}
	res, err := s.db.Execute(
		"SELECT * FROM sobs_github_work_items FINAL "+
			"WHERE IsDeleted=0 AND GithubRepo=? AND IssueUrl != '' "+
			"ORDER BY CreatedAt DESC LIMIT ?", githubRepo, limit)
	if err != nil {
		return nil
	}
	out := []*jsonenc.Object{}
	for _, m := range rowMaps(res) {
		out = append(out, serializeGithubWorkItemRow(m))
	}
	return out
}

// fallbackIssueDedupeDecision mirrors app.py _fallback_issue_dedupe_decision: a deterministic local
// classification used when the LLM is unavailable or returns an unusable reply. A dedupe-key match
// is "same"@0.92; a same service+signal family is "related"@0.73; otherwise "unrelated".
func fallbackIssueDedupeDecision(proposed *jsonenc.Object, candidates []dedupeCandidate) map[string]any {
	proposedKey := objGetStr(proposed, "dedup_key")
	proposedService := normalizeIssueMatchText(objGetStr(proposed, "service_name"))
	proposedSignal := normalizeIssueMatchText(objGetStr(proposed, "signal_name"))
	for _, c := range candidates {
		if proposedKey != "" && c.dedupKey != "" && proposedKey == c.dedupKey {
			return map[string]any{
				"classification": "same", "candidate_id": c.candidateID,
				"confidence": 0.92, "reason": "deterministic dedupe key match",
			}
		}
	}
	for _, c := range candidates {
		if proposedService != "" && proposedService == normalizeIssueMatchText(c.serviceName) &&
			proposedSignal != "" && proposedSignal == normalizeIssueMatchText(c.signalName) {
			return map[string]any{
				"classification": "related", "candidate_id": c.candidateID,
				"confidence": 0.73, "reason": "same service and signal family",
			}
		}
	}
	return map[string]any{
		"classification": "unrelated", "candidate_id": "", "confidence": 0.0, "reason": "no strong local match",
	}
}

// extractFirstJSONObject mirrors app.py _extract_first_json_object: strip a ```json fence, parse the
// whole reply as a JSON object, else parse the first {...} span; returns an empty object on failure.
func extractFirstJSONObject(text string) *jsonenc.Object {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return jsonenc.NewObject()
	}
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimSpace(jsonFenceRE.ReplaceAllString(raw, ""))
	}
	if v, err := parseJSONValue([]byte(raw)); err == nil {
		if o, ok := v.(*jsonenc.Object); ok {
			return o
		}
	}
	m := jsonBraceRE.FindString(raw)
	if m == "" {
		return jsonenc.NewObject()
	}
	if v, err := parseJSONValue([]byte(m)); err == nil {
		if o, ok := v.(*jsonenc.Object); ok {
			return o
		}
	}
	return jsonenc.NewObject()
}

// classifyIssueDedupeWithLLM mirrors app.py _classify_issue_dedupe_with_llm: ask the configured LLM
// to classify the proposed incident against the candidate issues, falling back to the deterministic
// local decision when the endpoint/model is unset, there are no candidates, or the reply is unusable.
func (s *server) classifyIssueDedupeWithLLM(settings map[string]string, proposed *jsonenc.Object, candidates []dedupeCandidate) map[string]any {
	endpoint := strings.TrimSpace(settings["ai.endpoint_url"])
	model := strings.TrimSpace(settings["ai.model"])
	apiKey := strings.TrimSpace(settings["ai.api_key"])
	thinking := strings.TrimSpace(settings["ai.thinking_level"])
	if thinking == "" {
		thinking = "off"
	}
	if endpoint == "" || model == "" || len(candidates) == 0 {
		return fallbackIssueDedupeDecision(proposed, candidates)
	}

	limit := githubIssueDedupeCandidateMax
	if len(candidates) < limit {
		limit = len(candidates)
	}
	compact := make([]any, 0, limit)
	for _, c := range candidates[:limit] {
		compact = append(compact, jsonenc.NewObject().
			Set("candidate_id", c.candidateID).
			Set("issue_url", c.issueURL).
			Set("issue_title", c.issueTitle).
			Set("service_name", c.serviceName).
			Set("signal_source", c.signalSource).
			Set("signal_name", c.signalName).
			Set("anomaly_state", c.anomalyState).
			Set("dedup_key", c.dedupKey).
			Set("copilot_assignment_status", c.copilotAssignmentStatus).
			Set("has_open_pr", c.prLinked || c.prURL != "").
			Set("assignees", stringsToAny(c.assignees)))
	}
	prompt := jsonenc.NewObject().
		Set("task", "Classify whether the proposed observability incident matches any existing GitHub issue.").
		Set("return_json_only", true).
		Set("required_keys", []any{"classification", "candidate_id", "confidence", "reason"}).
		Set("allowed_classifications", []any{"same", "related", "unrelated"}).
		Set("proposed_incident", proposed).
		Set("candidates", compact)
	messages := []any{
		jsonenc.NewObject().Set("role", "system").Set("content",
			"You classify observability incidents against existing GitHub issues. "+
				"Return a single JSON object only. Prefer 'same' only for clear duplicates, "+
				"'related' for likely same fault family but materially different work, otherwise 'unrelated'."),
		jsonenc.NewObject().Set("role", "user").Set("content", string(jsonenc.Encode(prompt, dumpsDefault))),
	}
	reply, _, err := s.callLLMChat(llmRequest{
		endpoint: endpoint, model: model, apiKey: apiKey,
		thinkingLevel: thinking, maxTokens: 400, messages: messages,
	})
	if err != nil { // _call_llm_endpoint returns ('', {}) on failure -> unusable reply -> fallback.
		reply = ""
	}
	parsed := extractFirstJSONObject(reply)
	classification := strings.ToLower(strings.TrimSpace(objGetStr(parsed, "classification")))
	if classification != "same" && classification != "related" && classification != "unrelated" {
		return fallbackIssueDedupeDecision(proposed, candidates)
	}
	confidence := objGetFloat(parsed, "confidence")
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	return map[string]any{
		"classification": classification,
		"candidate_id":   strings.TrimSpace(objGetStr(parsed, "candidate_id")),
		"confidence":     confidence,
		"reason":         strings.TrimSpace(objGetStr(parsed, "reason")),
	}
}

// chooseGithubIssueOutcome mirrors app.py _choose_github_issue_outcome in full: load local + open
// candidates, classify the proposed incident, and either REUSE a matched issue (incrementing the
// occurrence count, detecting an existing PR/assignees, and routing Copilot via the reuse path) or
// open a fresh issue. The returned map carries every field _run_agent_flow / _persist_github_work_item read.
func (s *server) chooseGithubIssueOutcome(settings map[string]string, tctx *jsonenc.Object,
	githubRepo, githubToken string, wantsCopilot bool,
	analysis, suggestion, issueTitle, issueBody string, allowNewIssue bool) map[string]any {

	tf := extractAgentTriggerFields(tctx)
	dedupKey := buildGithubWorkItemDedupKey(githubRepo, tf)
	localCandidates := s.loadRecentWorkItemCandidates(githubRepo, githubIssueDedupeCandidateMax)
	openIssues := s.fetchOpenGithubIssues(githubToken, githubRepo)
	openByURL := map[string]map[string]any{}
	for _, oi := range openIssues {
		openByURL[toStr(oi["issue_url"])] = oi
	}

	var candidates []dedupeCandidate
	seen := map[string]bool{}
	for _, li := range localCandidates {
		issueURL := objGetStr(li, "issue_url")
		if issueURL == "" || seen[issueURL] {
			continue
		}
		oi, ok := openByURL[issueURL]
		if !ok {
			continue
		}
		state := toStr(oi["issue_state"])
		if state == "" {
			state = objGetStr(li, "issue_state")
		}
		if state == "" {
			state = "open"
		}
		candidates = append(candidates, dedupeCandidate{
			candidateID:             issueURL,
			issueURL:                issueURL,
			issueNumber:             firstNonZeroInt(mapInt(oi, "issue_number"), objGetInt(li, "issue_number")),
			issueTitle:              orFirstNonEmpty(toStr(oi["issue_title"]), objGetStr(li, "issue_title")),
			issueBody:               toStr(oi["issue_body"]),
			issueState:              state,
			serviceName:             objGetStr(li, "service"),
			signalSource:            objGetStr(li, "signal_source"),
			signalName:              objGetStr(li, "signal_name"),
			anomalyState:            objGetStr(li, "anomaly_state"),
			dedupKey:                objGetStr(li, "dedup_key"),
			copilotAssignmentStatus: objGetStr(li, "copilot_assignment_status"),
			prLinked:                objGetBool(li, "pr_linked"),
			prURL:                   objGetStr(li, "pr_url"),
			assignees:               anyToStringList(oi["assignees"]),
		})
		seen[issueURL] = true
	}
	for _, oi := range openIssues {
		issueURL := toStr(oi["issue_url"])
		if issueURL == "" || seen[issueURL] {
			continue
		}
		candidates = append(candidates, dedupeCandidate{
			candidateID: issueURL,
			issueURL:    issueURL,
			issueNumber: mapInt(oi, "issue_number"),
			issueTitle:  toStr(oi["issue_title"]),
			issueBody:   toStr(oi["issue_body"]),
			issueState:  orDefault(toStr(oi["issue_state"]), "open"),
			assignees:   anyToStringList(oi["assignees"]),
		})
		seen[issueURL] = true
	}

	proposed := jsonenc.NewObject().
		Set("github_repo", githubRepo).
		Set("service_name", tf.serviceName).
		Set("signal_source", tf.signalSource).
		Set("signal_name", tf.signalName).
		Set("anomaly_state", tf.anomalyState).
		Set("dedup_key", dedupKey).
		Set("issue_title", issueTitle).
		Set("analysis_summary", truncRunes(analysis, 300)).
		Set("suggestion_summary", truncRunes(suggestion, 300))

	classification := s.classifyIssueDedupeWithLLM(settings, proposed, candidates)
	classificationName := orDefault(toStr(classification["classification"]), "unrelated")
	candidateID := toStr(classification["candidate_id"])
	var matched *dedupeCandidate
	for i := range candidates {
		if candidates[i].candidateID == candidateID {
			matched = &candidates[i]
			break
		}
	}

	if (classificationName == "same" || classificationName == "related") && matched != nil {
		return s.reuseExistingIssueOutcome(settings, githubRepo, githubToken, wantsCopilot,
			suggestion, dedupKey, issueTitle, classification, matched, classificationName)
	}

	// New-issue branch (app.py 5555-5646).
	created := map[string]any{}
	if allowNewIssue {
		created = s.createGithubIssueRecord(githubToken, githubRepo, issueTitle, issueBody, []string{"sobs-agent", "automated"})
	}
	creationError := toStr(created["error"])
	dedupDecision := "create_failed"
	dedupConfidence := 0.0
	assignmentReason := orDefault(creationError, "GitHub issue creation failed")
	if toStr(created["issue_url"]) != "" {
		dedupDecision = "new_issue"
		dedupConfidence = 1.0
		assignmentReason = ""
	} else if !allowNewIssue {
		dedupDecision = "suppressed_rate_limit"
		dedupConfidence = 0.0
		assignmentReason = "GitHub issue creation suppressed by hourly limit"
	}

	outcome := map[string]any{
		"issue_url":                       toStr(created["issue_url"]),
		"issue_number":                    mapInt(created, "issue_number"),
		"issue_title":                     orDefault(toStr(created["issue_title"]), issueTitle),
		"issue_state":                     newIssueState(created),
		"dedup_key":                       dedupKey,
		"dedup_decision":                  dedupDecision,
		"dedup_confidence":                dedupConfidence,
		"canonical_issue_url":             toStr(created["issue_url"]),
		"canonical_issue_number":          mapInt(created, "issue_number"),
		"related_issue_urls":              []any{},
		"occurrence_count":                1,
		"pr_linked":                       false,
		"pr_number":                       0,
		"pr_url":                          "",
		"copilot_assignment_status":       "not_requested",
		"copilot_assignment_reason":       assignmentReason,
		"copilot_assignment_requested_at": 0,
		"created_new_issue":               toStr(created["issue_url"]) != "",
		"issue_error":                     creationError,
	}
	if len(created) == 0 {
		if wantsCopilot {
			outcome["copilot_assignment_status"] = "blocked"
		} else {
			outcome["copilot_assignment_status"] = "not_requested"
		}
		if dedupDecision == "create_failed" {
			outcome["copilot_assignment_reason"] = assignmentReason
		}
		return outcome
	}
	if wantsCopilot {
		maxPerHour := parseBoundedIntSetting(settings, "ai.agent_max_assignments_per_hour", agentMaxAssignmentsPerHourDefault, 1, 20)
		maxActive := parseBoundedIntSetting(settings, "ai.agent_max_active_assignments", agentMaxActiveAssignmentsDefault, 1, 10)
		if s.countCopilotAssignmentsLastHour() >= maxPerHour {
			outcome["copilot_assignment_status"] = "blocked"
			outcome["copilot_assignment_reason"] = "Copilot assignment hourly limit reached"
			return outcome
		}
		if s.countActiveCopilotAssignments() >= maxActive {
			outcome["copilot_assignment_status"] = "blocked"
			outcome["copilot_assignment_reason"] = "active Copilot assignment limit reached"
			return outcome
		}
		status, reason, requestedAt := s.assignIssueToCopilot(githubToken, githubRepo, mapInt(created, "issue_number"))
		outcome["copilot_assignment_status"] = status
		outcome["copilot_assignment_reason"] = reason
		outcome["copilot_assignment_requested_at"] = requestedAt
	}
	return outcome
}

// reuseExistingIssueOutcome mirrors the reuse branch of _choose_github_issue_outcome (app.py
// 5477-5553): increment the occurrence count, detect an existing linked PR / Copilot assignees,
// and route a Copilot assignment via the reuse-path rate limiters.
func (s *server) reuseExistingIssueOutcome(settings map[string]string, githubRepo, githubToken string,
	wantsCopilot bool, suggestion, dedupKey, issueTitle string, classification map[string]any,
	matched *dedupeCandidate, classificationName string) map[string]any {

	issueURL := matched.issueURL
	issueNumber := matched.issueNumber
	prInfo := s.searchOpenPRForIssue(githubToken, githubRepo, issueNumber)
	assignmentStatus := orDefault(matched.copilotAssignmentStatus, "not_requested")
	for _, a := range matched.assignees {
		la := strings.ToLower(a)
		if la == strings.ToLower(githubCopilotAssignee) || la == githubCopilotSWEAgentLogin {
			assignmentStatus = "active"
			break
		}
	}
	occurrenceCount := s.countRowsParams(
		"SELECT count() AS c FROM sobs_github_work_items FINAL WHERE IsDeleted=0 AND IssueUrl=?", issueURL) + 1

	prLinked := prInfo != nil && objGetStr(prInfo, "pr_url") != ""
	prNumber := 0
	prURL := ""
	if prInfo != nil {
		prNumber = objGetInt(prInfo, "pr_number")
		prURL = objGetStr(prInfo, "pr_url")
	}

	dedupDecision := "related_existing"
	if classificationName == "same" {
		dedupDecision = "reused_existing"
	}
	outcome := map[string]any{
		"issue_url":                       issueURL,
		"issue_number":                    issueNumber,
		"issue_title":                     orDefault(matched.issueTitle, issueTitle),
		"issue_state":                     orDefault(matched.issueState, "open"),
		"dedup_key":                       dedupKey,
		"dedup_decision":                  dedupDecision,
		"dedup_confidence":                toFloatAny(classification["confidence"]),
		"canonical_issue_url":             issueURL,
		"canonical_issue_number":          issueNumber,
		"related_issue_urls":              []any{issueURL},
		"occurrence_count":                occurrenceCount,
		"pr_linked":                       prLinked,
		"pr_number":                       prNumber,
		"pr_url":                          prURL,
		"copilot_assignment_status":       assignmentStatus,
		"copilot_assignment_reason":       toStr(classification["reason"]),
		"copilot_assignment_requested_at": 0,
		"created_new_issue":               false,
	}
	if !wantsCopilot {
		return outcome
	}
	maxPerHour := parseBoundedIntSetting(settings, "ai.agent_max_assignments_per_hour", agentMaxAssignmentsPerHourDefault, 1, 20)
	maxActive := parseBoundedIntSetting(settings, "ai.agent_max_active_assignments", agentMaxActiveAssignmentsDefault, 1, 10)
	switch {
	case prLinked:
		outcome["copilot_assignment_status"] = "blocked"
		outcome["copilot_assignment_reason"] = "existing linked pull request already covers this issue"
	case assignmentStatus == "requested" || assignmentStatus == "active":
		outcome["copilot_assignment_status"] = "blocked"
		outcome["copilot_assignment_reason"] = "issue is already being worked by Copilot"
	case s.countCopilotAssignmentsLastHour() >= maxPerHour:
		outcome["copilot_assignment_status"] = "blocked"
		outcome["copilot_assignment_reason"] = "Copilot assignment hourly limit reached"
	case s.countActiveCopilotAssignments() >= maxActive:
		outcome["copilot_assignment_status"] = "blocked"
		outcome["copilot_assignment_reason"] = "active Copilot assignment limit reached"
	default:
		status, reason, requestedAt := s.assignIssueToCopilot(githubToken, githubRepo, issueNumber)
		outcome["copilot_assignment_status"] = status
		outcome["copilot_assignment_reason"] = reason
		outcome["copilot_assignment_requested_at"] = requestedAt
	}
	return outcome
}

// countCopilotAssignmentsLastHour mirrors app.py _count_copilot_assignments_last_hour.
func (s *server) countCopilotAssignmentsLastHour() int {
	cutoffMs := nowUTC().UnixMilli() - 3600*1000
	if cutoffMs < 0 {
		cutoffMs = 0
	}
	return s.countRowsParams(
		"SELECT count() AS c FROM sobs_github_work_items FINAL "+
			"WHERE IsDeleted=0 AND CopilotAssignmentRequestedAt >= ? AND CopilotAssignmentRequestedAt > 0",
		cutoffMs)
}

// countActiveCopilotAssignments mirrors app.py _count_active_copilot_assignments.
func (s *server) countActiveCopilotAssignments() int {
	return s.countRows(
		"SELECT count() AS c FROM sobs_github_work_items FINAL " +
			"WHERE IsDeleted=0 AND CopilotAssignmentStatus IN ('requested', 'active')")
}

// parseBoundedIntSetting mirrors app.py _parse_bounded_int_setting.
func parseBoundedIntSetting(settings map[string]string, key string, def, minimum, maximum int) int {
	parsed := def
	if raw := strings.TrimSpace(settings[key]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			parsed = n
		} else {
			parsed = def
		}
	}
	if parsed < minimum {
		parsed = minimum
	}
	if parsed > maximum {
		parsed = maximum
	}
	return parsed
}

// newIssueState mirrors `str(created.get("issue_state") or ("open" if created else ""))`.
func newIssueState(created map[string]any) string {
	if st := toStr(created["issue_state"]); st != "" {
		return st
	}
	if len(created) > 0 {
		return "open"
	}
	return ""
}

// objGetFloat reads a float from a parsed-JSON object, mirroring Python's float() coercion.
func objGetFloat(o *jsonenc.Object, key string) float64 {
	v, ok := o.Get(key)
	if !ok {
		return 0
	}
	return toFloatAny(v)
}

// objGetBool reads a bool from a jsonenc object (the serialized work-item row stores Go bools).
func objGetBool(o *jsonenc.Object, key string) bool {
	if v, ok := o.Get(key); ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// toFloatAny coerces a JSON scalar to float64 (json.Number / float64 / int / numeric string).
func toFloatAny(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	if f, err := strconv.ParseFloat(strings.TrimSpace(toStr(v)), 64); err == nil {
		return f
	}
	return 0
}

// firstNonZeroInt returns a if non-zero, else b — the `int(a or b or 0)` idiom for ints.
func firstNonZeroInt(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// anyToStringList coerces a candidate's `assignees` value into []string.
func anyToStringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, toStr(e))
		}
		return out
	}
	return nil
}
