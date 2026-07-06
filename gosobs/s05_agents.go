package main

// Port of app.py lines 5648-7514: agent rules helpers, agent runs helpers,
// GitHub work-item helpers, the agent flow, seed content, and the DB write
// worker (whose shared state lives in s02_db.go).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Agent rules helpers
// ---------------------------------------------------------------------------

var agentTriggerTypes = []string{"anomaly_rule", "tag_rule", "manual"}
var agentTriggerStates = []string{"warning", "critical", "any"}
var agentActions = []string{"analyze", "github_issue", "github_issue_copilot", "dlp_check"}

// agentUuid4 mirrors str(uuid.uuid4()).
// PORT-NOTE: no uuid dependency in go.mod; RFC 4122 v4 via crypto/rand.
func agentUuid4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable; fall back to time-based bytes.
		now := time.Now().UnixNano()
		for i := 0; i < 16; i++ {
			b[i] = byte(now >> ((i % 8) * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexStr := fmt.Sprintf("%x", b[:])
	return hexStr[0:8] + "-" + hexStr[8:12] + "-" + hexStr[12:16] + "-" + hexStr[16:20] + "-" + hexStr[20:32]
}

// agentUuid4Hex mirrors uuid.uuid4().hex.
func agentUuid4Hex() string {
	return strings.ReplaceAll(agentUuid4(), "-", "")
}

// agentTruthy mirrors Python truthiness for the JSON-ish values used in this
// section (None/""/0/False/empty containers are falsy).
func agentTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	case json.Number:
		f, err := v.Float64()
		return err != nil || f != 0
	case []any:
		return len(v) > 0
	case []string:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

// agentFirstTruthyStr mirrors str(a or b or ... or "").
func agentFirstTruthyStr(values ...any) string {
	for _, v := range values {
		if agentTruthy(v) {
			return rowString(v)
		}
	}
	return ""
}

// agentSettingsGet mirrors settings.get(key, default) for map[string]string.
func agentSettingsGet(settings map[string]string, key, def string) string {
	if v, ok := settings[key]; ok {
		return v
	}
	return def
}

// agentStringSlice coerces []string / []any to []string (str() each element).
func agentStringSlice(value any) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, rowString(item))
		}
		return out
	default:
		return nil
	}
}

// splitAgentActions mirrors [a.strip() for a in actions.split(",") if a.strip()].
func splitAgentActions(actions string) []string {
	out := []string{}
	for _, a := range strings.Split(actions, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

func agentRuleRowToDict(row Row) map[string]any {
	return map[string]any{
		"id":                 rowString(row["Id"]),
		"name":               rowString(row["Name"]),
		"description":        rowString(row["Description"]),
		"trigger_type":       rowString(row["TriggerType"]),
		"trigger_ref_id":     rowString(row["TriggerRefId"]),
		"trigger_state":      rowString(row["TriggerState"]),
		"actions":            splitAgentActions(rowString(row["Actions"])),
		"rate_limit_minutes": coerceInt(row["RateLimitMinutes"]),
		"is_enabled":         coerceInt(row["IsEnabled"]) != 0,
	}
}

func loadAgentRules(db *ChDbConnection) []map[string]any {
	res, err := db.Execute(
		"SELECT Id, Name, Description, TriggerType, TriggerRefId, TriggerState, " +
			"Actions, RateLimitMinutes, IsEnabled " +
			"FROM sobs_agent_rules FINAL WHERE IsDeleted=0 ORDER BY Name")
	if err != nil {
		// PORT-NOTE: Python propagated the exception; the Go port logs and
		// returns an empty list so route handlers render an empty state.
		logger.Warn(fmt.Sprintf("loadAgentRules query failed: %v", err))
		return []map[string]any{}
	}
	rows := res.Fetchall()
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, agentRuleRowToDict(row))
	}
	return out
}

func loadAgentRule(db *ChDbConnection, ruleId string) map[string]any {
	res, err := db.Execute(
		"SELECT Id, Name, Description, TriggerType, TriggerRefId, TriggerState, "+
			"Actions, RateLimitMinutes, IsEnabled "+
			"FROM sobs_agent_rules FINAL WHERE IsDeleted=0 AND Id=? LIMIT 1",
		ruleId)
	if err != nil {
		logger.Warn(fmt.Sprintf("loadAgentRule query failed: %v", err))
		return nil
	}
	row := res.Fetchone()
	if row == nil {
		return nil
	}
	return agentRuleRowToDict(row)
}

// ---------------------------------------------------------------------------
// Agent runs helpers
// ---------------------------------------------------------------------------

func loadAgentRuns(db *ChDbConnection, limit ...int) []map[string]any {
	limitValue := 50
	if len(limit) > 0 {
		limitValue = limit[0]
	}
	res, err := db.Execute(fmt.Sprintf(
		"SELECT Id, RuleId, RuleName, TriggerContext, Status, GuardDecision, DlpResult, "+
			"Analysis, Suggestion, GithubIssueUrl, ErrorMessage, CreatedAt, CompletedAt, IsDismissed "+
			"FROM sobs_agent_runs FINAL WHERE IsDeleted=0 ORDER BY CreatedAt DESC "+
			"LIMIT %d", limitValue))
	if err != nil {
		logger.Warn(fmt.Sprintf("loadAgentRuns query failed: %v", err))
		return []map[string]any{}
	}
	rows := res.Fetchall()
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"id":               rowString(row["Id"]),
			"rule_id":          rowString(row["RuleId"]),
			"rule_name":        rowString(row["RuleName"]),
			"trigger_context":  rowString(row["TriggerContext"]),
			"status":           rowString(row["Status"]),
			"guard_decision":   rowString(row["GuardDecision"]),
			"dlp_result":       rowString(row["DlpResult"]),
			"analysis":         rowString(row["Analysis"]),
			"suggestion":       rowString(row["Suggestion"]),
			"github_issue_url": rowString(row["GithubIssueUrl"]),
			"error_message":    rowString(row["ErrorMessage"]),
			"created_at":       rowString(row["CreatedAt"]),
			"completed_at":     rowString(row["CompletedAt"]),
			"is_dismissed":     coerceInt(row["IsDismissed"]) != 0,
		})
	}
	return out
}

// agentRuleLastRunTs returns the Unix timestamp of the most recent agent run
// for ruleId, or 0.
func agentRuleLastRunTs(db *ChDbConnection, ruleId string) float64 {
	res, err := db.Execute(
		"SELECT max(toUnixTimestamp64Milli(CreatedAt)) AS t "+
			"FROM sobs_agent_runs FINAL WHERE IsDeleted=0 AND RuleId=?",
		ruleId)
	if err != nil {
		logger.Warn(fmt.Sprintf("agentRuleLastRunTs query failed: %v", err))
		return 0.0
	}
	row := res.Fetchone()
	if row == nil {
		return 0.0
	}
	if t, ok := coerceFloat(row["t"]); ok && t != 0 {
		return t / 1000.0
	}
	return 0.0
}

// countGithubIssuesLastHour counts completed agent runs with a GitHub issue
// created in the last 60 minutes.
func countGithubIssuesLastHour(db *ChDbConnection) int {
	res, err := db.Execute(
		"SELECT count() AS c FROM sobs_agent_runs FINAL " +
			"WHERE IsDeleted=0 AND GithubIssueUrl != '' " +
			"AND CreatedAt >= now() - INTERVAL 1 HOUR")
	if err != nil {
		logger.Warn(fmt.Sprintf("countGithubIssuesLastHour query failed: %v", err))
		return 0
	}
	row := res.Fetchone()
	if row == nil {
		return 0
	}
	return coerceInt(row["c"])
}

func countCopilotAssignmentsLastHour(db *ChDbConnection) int {
	cutoffMs := max(int64(0), time.Now().UnixMilli()-3600*1000)
	res, err := db.Execute(
		"SELECT count() AS c FROM sobs_github_work_items FINAL "+
			"WHERE IsDeleted=0 AND CopilotAssignmentRequestedAt >= ? AND CopilotAssignmentRequestedAt > 0",
		cutoffMs)
	if err != nil {
		logger.Warn(fmt.Sprintf("countCopilotAssignmentsLastHour query failed: %v", err))
		return 0
	}
	row := res.Fetchone()
	if row == nil {
		return 0
	}
	return coerceInt(row["c"])
}

func countActiveCopilotAssignments(db *ChDbConnection) int {
	res, err := db.Execute(
		"SELECT count() AS c FROM sobs_github_work_items FINAL " +
			"WHERE IsDeleted=0 AND CopilotAssignmentStatus IN ('requested', 'active')")
	if err != nil {
		logger.Warn(fmt.Sprintf("countActiveCopilotAssignments query failed: %v", err))
		return 0
	}
	row := res.Fetchone()
	if row == nil {
		return 0
	}
	return coerceInt(row["c"])
}

// githubApiHeaders mirrors _github_api_headers.
// PORT-NOTE: Python keyword-only include_content_type/extra became positional.
func githubApiHeaders(githubToken string, includeContentType bool, extra map[string]string) map[string]string {
	headers := map[string]string{
		"Authorization":        "Bearer " + githubToken,
		"Accept":               "application/vnd.github+json",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	if includeContentType {
		headers["Content-Type"] = "application/json"
	}
	for k, v := range extra {
		headers[k] = v
	}
	return headers
}

func parseBoundedIntSetting(settings map[string]string, key string, def, minimum, maximum int) int {
	parsed := def
	raw := strings.TrimSpace(settings[key])
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			parsed = n
		} else {
			parsed = def
		}
	}
	return max(minimum, min(maximum, parsed))
}

func extractAgentTriggerFields(triggerContext map[string]any) map[string]any {
	var triggerContextParsed map[string]any
	switch extra := triggerContext["extra"].(type) {
	case string:
		if parsed, ok := safeJsonLoads(extra, map[string]any{}).(map[string]any); ok {
			triggerContextParsed = parsed
		}
	case map[string]any:
		triggerContextParsed = extra
	}
	if triggerContextParsed == nil {
		triggerContextParsed = map[string]any{}
	}

	serviceName := strings.TrimSpace(agentFirstTruthyStr(triggerContextParsed["service"], triggerContext["service"]))
	anomalyRuleId := strings.TrimSpace(agentFirstTruthyStr(triggerContext["trigger_ref_id"]))
	anomalyState := strings.TrimSpace(agentFirstTruthyStr(triggerContextParsed["state"], triggerContext["trigger_state"]))
	signalSource := strings.TrimSpace(agentFirstTruthyStr(triggerContextParsed["source"]))
	signalName := strings.TrimSpace(agentFirstTruthyStr(triggerContextParsed["signal"]))
	signalValue := 0.0
	if agentTruthy(triggerContextParsed["value"]) {
		if v, ok := coerceFloat(triggerContextParsed["value"]); ok {
			signalValue = v
		}
	}

	return map[string]any{
		"service_name":    serviceName,
		"anomaly_rule_id": anomalyRuleId,
		"anomaly_state":   anomalyState,
		"signal_source":   signalSource,
		"signal_name":     signalName,
		"signal_value":    signalValue,
		"extra":           triggerContextParsed,
	}
}

var issueMatchNonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

func normalizeIssueMatchText(value any) string {
	text := issueMatchNonAlnumRe.ReplaceAllString(strings.ToLower(rowString(value)), " ")
	return strings.Join(strings.Fields(text), " ")
}

func buildGithubWorkItemDedupKey(githubRepo string, triggerFields map[string]any) string {
	joined := strings.Join([]string{
		normalizeIssueMatchText(githubRepo),
		normalizeIssueMatchText(triggerFields["service_name"]),
		normalizeIssueMatchText(triggerFields["signal_source"]),
		normalizeIssueMatchText(triggerFields["signal_name"]),
		normalizeIssueMatchText(triggerFields["anomaly_state"]),
	}, "|")
	return strings.Trim(joined, "|")
}

func buildAgentIssueTitle(rule map[string]any, triggerFields map[string]any) string {
	serviceName := strings.TrimSpace(agentFirstTruthyStr(triggerFields["service_name"]))
	signalName := strings.TrimSpace(agentFirstTruthyStr(triggerFields["signal_name"]))
	signalSource := strings.TrimSpace(agentFirstTruthyStr(triggerFields["signal_source"]))
	anomalyState := strings.TrimSpace(agentFirstTruthyStr(triggerFields["anomaly_state"], "detected"))
	focus := serviceName
	if focus == "" {
		focus = agentFirstTruthyStr(rule["name"], "Agent Rule")
	}
	if signalSource != "" && signalName != "" {
		return fmt.Sprintf("[SOBS Agent] %s — %s/%s %s anomaly", focus, signalSource, signalName, anomalyState)
	}
	return fmt.Sprintf("[SOBS Agent] %s — %s state detected", focus, anomalyState)
}

// ---------------------------------------------------------------------------
// GitHub work-item helpers
// ---------------------------------------------------------------------------

// workItemTzSuffixRe mirrors re.search(r"[zZ]|[+\-]\d\d:?\d\d$", normalized).
var workItemTzSuffixRe = regexp.MustCompile(`[zZ]|[+\-]\d\d:?\d\d$`)

func serializeGithubWorkItemRow(row Row) map[string]any {
	relatedRaw := row["RelatedIssueUrls"]
	if relatedRaw == nil {
		relatedRaw = "[]"
	}
	relatedIssueUrls := []any{}
	if parsed, ok := safeJsonLoads(relatedRaw, []any{}).([]any); ok {
		relatedIssueUrls = parsed
	}

	toUtcIso := func(tsValue any) string {
		if t, ok := tsValue.(time.Time); ok {
			// PORT-NOTE: Go time.Time always carries a location, so the Python
			// naive-datetime ("assume UTC") branch collapses into .UTC().
			return t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		raw := strings.TrimSpace(rowString(tsValue))
		if raw == "" {
			return ""
		}
		normalized := strings.ReplaceAll(raw, " ", "T")
		if strings.HasSuffix(normalized, "Z") {
			normalized = normalized[:len(normalized)-1] + "+00:00"
		}
		if !workItemTzSuffixRe.MatchString(normalized) {
			normalized += "+00:00"
		}
		// PORT-NOTE: datetime.fromisoformat → fixed layout list (with/without
		// fractional seconds and colon-less offsets).
		var dt time.Time
		parsed := false
		for _, layout := range []string{
			"2006-01-02T15:04:05.999999999Z07:00",
			"2006-01-02T15:04:05.999999999-0700",
			"2006-01-02T15:04:05.999999999-07",
		} {
			if p, err := time.Parse(layout, normalized); err == nil {
				dt = p
				parsed = true
				break
			}
		}
		if !parsed {
			return raw
		}
		return dt.UTC().Format("2006-01-02T15:04:05.000Z")
	}

	signalValue, _ := coerceFloat(row["SignalValue"])
	dedupConfidence, _ := coerceFloat(row["DedupConfidence"])
	occurrenceCount := coerceInt(row["OccurrenceCount"])
	if occurrenceCount == 0 {
		occurrenceCount = 1
	}
	// Python: r.get("CopilotAssignmentStatus", "not_requested") — default only
	// applies when the key is missing entirely.
	copilotAssignmentStatus := "not_requested"
	if v, ok := row["CopilotAssignmentStatus"]; ok {
		copilotAssignmentStatus = rowString(v)
	}

	return map[string]any{
		"id":                              rowString(row["Id"]),
		"created_at":                      toUtcIso(row["CreatedAt"]),
		"completed_at":                    toUtcIso(row["CompletedAt"]),
		"agent_rule_id":                   rowString(row["AgentRuleId"]),
		"agent_rule_name":                 rowString(row["AgentRuleName"]),
		"agent_action":                    rowString(row["AgentAction"]),
		"service":                         rowString(row["ServiceName"]),
		"anomaly_rule_id":                 rowString(row["AnomalyRuleId"]),
		"anomaly_state":                   rowString(row["AnomalyState"]),
		"signal_source":                   rowString(row["SignalSource"]),
		"signal_name":                     rowString(row["SignalName"]),
		"signal_value":                    signalValue,
		"github_repo":                     rowString(row["GithubRepo"]),
		"dedup_key":                       rowString(row["DedupKey"]),
		"dedup_decision":                  rowString(row["DedupDecision"]),
		"dedup_confidence":                dedupConfidence,
		"issue_number":                    coerceInt(row["IssueNumber"]),
		"issue_url":                       rowString(row["IssueUrl"]),
		"canonical_issue_number":          coerceInt(row["CanonicalIssueNumber"]),
		"canonical_issue_url":             rowString(row["CanonicalIssueUrl"]),
		"related_issue_urls":              relatedIssueUrls,
		"occurrence_count":                occurrenceCount,
		"issue_state":                     rowString(row["IssueState"]),
		"issue_title":                     rowString(row["IssueTitle"]),
		"analysis_summary":                rowString(row["AnalysisSummary"]),
		"suggestion_summary":              rowString(row["SuggestionSummary"]),
		"copilot_assignment_requested_at": coerceInt(row["CopilotAssignmentRequestedAt"]),
		"copilot_assignment_status":       copilotAssignmentStatus,
		"copilot_assignment_reason":       rowString(row["CopilotAssignmentReason"]),
		"pr_linked":                       coerceInt(row["PrLinked"]) != 0,
		"pr_number":                       coerceInt(row["PrNumber"]),
		"pr_url":                          rowString(row["PrUrl"]),
	}
}

// fetchOpenGithubIssues mirrors _fetch_open_github_issues (limit defaults to
// _GITHUB_ISSUE_DEDUPE_CANDIDATE_LIMIT).
func fetchOpenGithubIssues(githubToken, githubRepo string, limit ...int) []map[string]any {
	limitValue := githubIssueDedupeCandidateLimit
	if len(limit) > 0 {
		limitValue = limit[0]
	}
	if githubToken == "" || githubRepo == "" {
		return []map[string]any{}
	}
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return []map[string]any{}
	}
	params := url.Values{}
	params.Set("state", "open")
	params.Set("per_page", strconv.Itoa(max(1, min(100, limitValue))))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?%s", owner, repo, params.Encode()), nil)
	if err != nil {
		logger.Warn(fmt.Sprintf("GitHub open issue fetch failed for %s/%s: %v", owner, repo, err))
		return []map[string]any{}
	}
	for k, v := range githubApiHeaders(githubToken, false, nil) {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		logger.Warn(fmt.Sprintf("GitHub open issue fetch failed for %s/%s: %v", owner, repo, err))
		return []map[string]any{}
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil || resp.StatusCode >= 400 {
		logger.Warn(fmt.Sprintf("GitHub open issue fetch failed for %s/%s: HTTP %d", owner, repo, resp.StatusCode))
		return []map[string]any{}
	}
	var payload any = []any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			logger.Warn(fmt.Sprintf("GitHub open issue fetch failed for %s/%s: %v", owner, repo, err))
			return []map[string]any{}
		}
	}
	payloadList, ok := payload.([]any)
	if !ok {
		return []map[string]any{}
	}

	issues := []map[string]any{}
	for _, rawItem := range payloadList {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if _, isPr := item["pull_request"].(map[string]any); isPr {
			continue
		}
		assignees := []string{}
		if list, ok := item["assignees"].([]any); ok {
			for _, a := range list {
				if m, ok := a.(map[string]any); ok {
					assignees = append(assignees, agentFirstTruthyStr(m["login"]))
				}
			}
		}
		issues = append(issues, map[string]any{
			"issue_number": coerceInt(item["number"]),
			"issue_url":    agentFirstTruthyStr(item["html_url"]),
			"issue_title":  agentFirstTruthyStr(item["title"]),
			"issue_body":   agentFirstTruthyStr(item["body"]),
			"issue_state":  agentFirstTruthyStr(item["state"], "open"),
			"assignees":    assignees,
		})
	}
	return issues
}

// searchOpenPrForIssue mirrors _search_open_pr_for_issue (nil = Python None).
func searchOpenPrForIssue(githubToken, githubRepo string, issueNumber int) map[string]any {
	if githubToken == "" || githubRepo == "" || issueNumber <= 0 {
		return nil
	}
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return nil
	}
	params := url.Values{}
	params.Set("q", fmt.Sprintf(`repo:%s/%s is:pr is:open "#%d" in:body`, owner, repo, issueNumber))
	params.Set("per_page", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/search/issues?"+params.Encode(), nil)
	if err != nil {
		return nil
	}
	for k, v := range githubApiHeaders(githubToken, false, nil) {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil || resp.StatusCode >= 400 {
		return nil
	}
	payload := map[string]any{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil
		}
	}
	items, _ := payload["items"].([]any)
	if len(items) == 0 {
		return nil
	}
	item, _ := items[0].(map[string]any)
	if len(item) == 0 {
		return nil
	}
	return map[string]any{
		"pr_number": coerceInt(item["number"]),
		"pr_url":    agentFirstTruthyStr(item["html_url"]),
	}
}

var issueRefFromUrlRe = regexp.MustCompile(`github\.com/([^/]+)/([^/]+)/issues/(\d+)`)

func parseIssueRefFromUrl(issueUrl string) (string, string, int) {
	match := issueRefFromUrlRe.FindStringSubmatch(issueUrl)
	if match == nil {
		return "", "", 0
	}
	n, _ := strconv.Atoi(match[3])
	return match[1], match[2], n
}

func deriveCopilotAssignmentStatus(currentStatus, issueState string, assignees []string, prLinked bool) (string, string) {
	normalizedCurrent := strings.ToLower(strings.TrimSpace(currentStatus))
	if normalizedCurrent == "" {
		normalizedCurrent = "not_requested"
	}
	normalizedState := strings.ToLower(strings.TrimSpace(issueState))
	copilotAssigned := false
	for _, item := range assignees {
		normalized := strings.ToLower(strings.TrimSpace(item))
		if normalized == strings.ToLower(githubCopilotAssignee) || normalized == "copilot-swe-agent" {
			copilotAssigned = true
		}
	}

	if normalizedState == "closed" {
		if normalizedCurrent == "requested" || normalizedCurrent == "active" {
			return "completed", "issue is closed"
		}
		return normalizedCurrent, ""
	}
	if prLinked && (normalizedCurrent == "not_requested" || normalizedCurrent == "blocked") {
		return "blocked", "linked pull request already exists"
	}
	if copilotAssigned {
		return "active", "Copilot is assigned on the issue"
	}
	if normalizedCurrent == "requested" || normalizedCurrent == "active" {
		return "requested", "Copilot assignment requested"
	}
	return normalizedCurrent, ""
}

func backfillGithubWorkItemLinks(db *ChDbConnection, settings map[string]string) error {
	startedAt := time.Now()
	scannedCount := 0
	updatedCount := 0
	skippedCount := 0
	errorCount := 0

	logSummary := func(reason string) {
		summary := map[string]any{
			"scanned":     scannedCount,
			"updated":     updatedCount,
			"skipped":     skippedCount,
			"errors":      errorCount,
			"duration_ms": int(time.Since(startedAt).Milliseconds()),
			"max_items":   githubWorkItemBackfillMaxItems,
		}
		if reason != "" {
			summary["reason"] = reason
		}
		logger.Info("github_work_item_backfill_summary " + safeJsonDumps(summary))
	}

	defaultToken := strings.TrimSpace(settings["ai.github_token"])
	if defaultToken == "" {
		logSummary("missing_default_token")
		return nil
	}

	res, err := db.Execute(
		"SELECT * FROM sobs_github_work_items FINAL "+
			"WHERE IsDeleted=0 AND IssueUrl != '' "+
			"AND (IssueState = '' OR IssueState = 'open' OR CopilotAssignmentStatus IN ('requested','active')) "+
			"ORDER BY CreatedAt DESC LIMIT ?",
		githubWorkItemBackfillMaxItems)
	if err != nil {
		return err
	}
	rows := res.Fetchall()
	scannedCount = len(rows)
	if len(rows) == 0 {
		logSummary("")
		return nil
	}

	updates := []Row{}

	for _, row := range rows {
		issueUrl := strings.TrimSpace(rowString(row["IssueUrl"]))
		if issueUrl == "" {
			skippedCount++
			continue
		}
		owner := ""
		repo := ""
		issueNumber := 0

		githubRepo := strings.TrimSpace(rowString(row["GithubRepo"]))
		if githubRepo != "" {
			owner, repo = parseGithubRepoOwnerName(githubRepo)
		}
		if owner == "" || repo == "" {
			owner, repo, issueNumber = parseIssueRefFromUrl(issueUrl)
		}
		if issueNumber <= 0 {
			issueNumber = coerceInt(row["IssueNumber"])
		}
		if owner == "" || repo == "" || issueNumber <= 0 {
			skippedCount++
			continue
		}

		scopedToken := loadRepoScopedGithubToken(db, owner, repo)
		githubToken := scopedToken
		if githubToken == "" {
			githubToken = defaultToken
		}
		if githubToken == "" {
			skippedCount++
			continue
		}

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", owner, repo, issueNumber), nil)
		if reqErr != nil {
			cancel()
			errorCount++
			skippedCount++
			continue
		}
		for k, v := range githubApiHeaders(githubToken, false, nil) {
			req.Header.Set(k, v)
		}
		resp, doErr := httpClient.Do(req)
		if doErr != nil {
			cancel()
			errorCount++
			skippedCount++
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		if readErr != nil || resp.StatusCode >= 400 {
			errorCount++
			skippedCount++
			continue
		}
		issuePayload := map[string]any{}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &issuePayload); err != nil {
				errorCount++
				skippedCount++
				continue
			}
		}

		issueState := agentFirstTruthyStr(issuePayload["state"], row["IssueState"])
		issueTitle := agentFirstTruthyStr(issuePayload["title"], row["IssueTitle"])
		assignees := []string{}
		if list, ok := issuePayload["assignees"].([]any); ok {
			for _, item := range list {
				if m, ok := item.(map[string]any); ok {
					assignees = append(assignees, agentFirstTruthyStr(m["login"]))
				}
			}
		}

		prInfo := searchOpenPrForIssue(githubToken, owner+"/"+repo, issueNumber)
		prUrl := ""
		prNumber := 0
		if prInfo != nil {
			prUrl = agentFirstTruthyStr(prInfo["pr_url"])
			prNumber = coerceInt(prInfo["pr_number"])
		}
		prLinked := prUrl != ""

		nextAssignmentStatus, nextAssignmentReason := deriveCopilotAssignmentStatus(
			rowString(row["CopilotAssignmentStatus"]),
			issueState,
			assignees,
			prLinked,
		)

		prLinkedInt := 0
		if prLinked {
			prLinkedInt = 1
		}
		changed := false
		if rowString(row["IssueState"]) != issueState {
			changed = true
		}
		if rowString(row["IssueTitle"]) != issueTitle {
			changed = true
		}
		if coerceInt(row["PrLinked"]) != prLinkedInt {
			changed = true
		}
		if coerceInt(row["PrNumber"]) != prNumber {
			changed = true
		}
		if rowString(row["PrUrl"]) != prUrl {
			changed = true
		}
		if rowString(row["CopilotAssignmentStatus"]) != nextAssignmentStatus {
			changed = true
		}
		if rowString(row["CopilotAssignmentReason"]) != nextAssignmentReason {
			changed = true
		}

		if !changed {
			skippedCount++
			continue
		}

		updated := Row{}
		for k, v := range row {
			updated[k] = v
		}
		updated["IssueState"] = issueState
		updated["IssueTitle"] = issueTitle
		updated["PrLinked"] = prLinkedInt
		updated["PrNumber"] = prNumber
		updated["PrUrl"] = prUrl
		updated["CopilotAssignmentStatus"] = nextAssignmentStatus
		updated["CopilotAssignmentReason"] = nextAssignmentReason
		updated["Version"] = time.Now().UnixMilli()
		updates = append(updates, updated)
	}

	if len(updates) > 0 {
		if _, err := insertRowsJsonEachRow(db, "sobs_github_work_items", updates); err != nil {
			return err
		}
		updatedCount = len(updates)
	}

	logSummary("")
	return nil
}

func emitAgentIssueDecisionSummary(
	runId string,
	rule map[string]any,
	triggerContext map[string]any,
	issueOutcome map[string]any,
	githubIssueUrl string,
	wantsIssue bool,
	wantsCopilotAssignment bool,
	githubRepo string,
) {
	if !wantsIssue {
		return
	}

	dedupConfidence, _ := coerceFloat(issueOutcome["dedup_confidence"])
	summary := map[string]any{
		"run_id":                    runId,
		"rule_id":                   agentFirstTruthyStr(rule["id"]),
		"rule_name":                 agentFirstTruthyStr(rule["name"]),
		"trigger_type":              agentFirstTruthyStr(triggerContext["trigger_type"]),
		"trigger_ref_id":            agentFirstTruthyStr(triggerContext["trigger_ref_id"]),
		"github_repo":               githubRepo,
		"issue_url":                 agentFirstTruthyStr(githubIssueUrl, issueOutcome["issue_url"]),
		"dedup_decision":            agentFirstTruthyStr(issueOutcome["dedup_decision"]),
		"dedup_confidence":          dedupConfidence,
		"copilot_requested":         wantsCopilotAssignment,
		"copilot_assignment_status": agentFirstTruthyStr(issueOutcome["copilot_assignment_status"]),
		"copilot_assignment_reason": agentFirstTruthyStr(issueOutcome["copilot_assignment_reason"]),
		"created_new_issue":         agentTruthy(issueOutcome["created_new_issue"]),
		"occurrence_count":          coerceInt(issueOutcome["occurrence_count"]),
	}
	logger.Info("agent_issue_decision_summary " + safeJsonDumps(summary))
}

// maybeBackfillGithubWorkItemLinks mirrors _maybe_backfill_github_work_item_links.
// PORT-NOTE: the Python globals are read/written without a lock from a single
// asyncio loop; the Go port mirrors that (benign race when called concurrently).
func maybeBackfillGithubWorkItemLinks(db *ChDbConnection, settings map[string]string) {
	now := float64(time.Now().UnixNano()) / 1e9
	if githubWorkItemBackfillRunning {
		return
	}
	if now-githubWorkItemBackfillLastTs < float64(githubWorkItemBackfillIntervalSec) {
		return
	}
	githubWorkItemBackfillRunning = true
	githubWorkItemBackfillLastTs = now
	defer func() { githubWorkItemBackfillRunning = false }()
	if err := backfillGithubWorkItemLinks(db, settings); err != nil {
		logger.Warn(fmt.Sprintf("GitHub work-item backfill failed: %v", err))
	}
}

// loadRecentWorkItemCandidates mirrors _load_recent_work_item_candidates
// (limit defaults to _GITHUB_ISSUE_DEDUPE_CANDIDATE_LIMIT).
func loadRecentWorkItemCandidates(db *ChDbConnection, githubRepo string, limit ...int) []map[string]any {
	limitValue := githubIssueDedupeCandidateLimit
	if len(limit) > 0 {
		limitValue = limit[0]
	}
	res, err := db.Execute(
		"SELECT * FROM sobs_github_work_items FINAL "+
			"WHERE IsDeleted=0 AND GithubRepo=? AND IssueUrl != '' "+
			"ORDER BY CreatedAt DESC LIMIT ?",
		githubRepo, max(1, limitValue))
	if err != nil {
		// PORT-NOTE: Python propagated the exception; the Go port logs and
		// returns an empty candidate list.
		logger.Warn(fmt.Sprintf("loadRecentWorkItemCandidates query failed: %v", err))
		return []map[string]any{}
	}
	rows := res.Fetchall()
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, serializeGithubWorkItemRow(r))
	}
	return out
}

func fallbackIssueDedupeDecision(proposed map[string]any, candidates []map[string]any) map[string]any {
	proposedKey := agentFirstTruthyStr(proposed["dedup_key"])
	proposedService := normalizeIssueMatchText(proposed["service_name"])
	proposedSignal := normalizeIssueMatchText(proposed["signal_name"])
	for _, candidate := range candidates {
		candidateKey := agentFirstTruthyStr(candidate["dedup_key"])
		if proposedKey != "" && candidateKey != "" && proposedKey == candidateKey {
			return map[string]any{
				"classification": "same",
				"candidate_id":   agentFirstTruthyStr(candidate["candidate_id"]),
				"confidence":     0.92,
				"reason":         "deterministic dedupe key match",
			}
		}
	}
	for _, candidate := range candidates {
		if proposedService != "" &&
			proposedService == normalizeIssueMatchText(candidate["service_name"]) &&
			proposedSignal != "" &&
			proposedSignal == normalizeIssueMatchText(candidate["signal_name"]) {
			return map[string]any{
				"classification": "related",
				"candidate_id":   agentFirstTruthyStr(candidate["candidate_id"]),
				"confidence":     0.73,
				"reason":         "same service and signal family",
			}
		}
	}
	return map[string]any{
		"classification": "unrelated",
		"candidate_id":   "",
		"confidence":     0.0,
		"reason":         "no strong local match",
	}
}

var (
	jsonCodeFenceRe   = regexp.MustCompile("(?s)^```(?:json)?\\s*|\\s*```$")
	jsonFirstObjectRe = regexp.MustCompile(`(?s)\{.*\}`)
)

func extractFirstJsonObject(text string) map[string]any {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return map[string]any{}
	}
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimSpace(jsonCodeFenceRe.ReplaceAllString(raw, ""))
	}
	if parsed, ok := safeJsonLoads(raw, map[string]any{}).(map[string]any); ok && len(parsed) > 0 {
		return parsed
	}
	// PORT-NOTE: Python returns the first parse result even when it is an empty
	// dict; the empty-dict and no-match outcomes are identical, so falling
	// through to the regex search is behavior-preserving.
	match := jsonFirstObjectRe.FindString(raw)
	if match == "" {
		return map[string]any{}
	}
	if parsed, ok := safeJsonLoads(match, map[string]any{}).(map[string]any); ok {
		return parsed
	}
	return map[string]any{}
}

func classifyIssueDedupeWithLlm(
	settings map[string]string,
	proposed map[string]any,
	candidates []map[string]any,
) map[string]any {
	endpointUrl := strings.TrimSpace(settings["ai.endpoint_url"])
	model := strings.TrimSpace(settings["ai.model"])
	apiKey := strings.TrimSpace(settings["ai.api_key"])
	thinkingLevel := strings.TrimSpace(agentSettingsGet(settings, "ai.thinking_level", "off"))
	if thinkingLevel == "" {
		thinkingLevel = "off"
	}
	if endpointUrl == "" || model == "" || len(candidates) == 0 {
		return fallbackIssueDedupeDecision(proposed, candidates)
	}

	limited := candidates
	if len(limited) > githubIssueDedupeCandidateLimit {
		limited = limited[:githubIssueDedupeCandidateLimit]
	}
	compactCandidates := make([]map[string]any, 0, len(limited))
	for _, item := range limited {
		assignees := item["assignees"]
		if assignees == nil {
			assignees = []any{}
		}
		compactCandidates = append(compactCandidates, map[string]any{
			"candidate_id":              agentFirstTruthyStr(item["candidate_id"]),
			"issue_url":                 agentFirstTruthyStr(item["issue_url"]),
			"issue_title":               agentFirstTruthyStr(item["issue_title"]),
			"service_name":              agentFirstTruthyStr(item["service_name"]),
			"signal_source":             agentFirstTruthyStr(item["signal_source"]),
			"signal_name":               agentFirstTruthyStr(item["signal_name"]),
			"anomaly_state":             agentFirstTruthyStr(item["anomaly_state"]),
			"dedup_key":                 agentFirstTruthyStr(item["dedup_key"]),
			"copilot_assignment_status": agentFirstTruthyStr(item["copilot_assignment_status"]),
			"has_open_pr":               agentTruthy(item["pr_linked"]) || agentTruthy(item["pr_url"]),
			"assignees":                 assignees,
		})
	}
	prompt := map[string]any{
		"task":                    "Classify whether the proposed observability incident matches any existing GitHub issue.",
		"return_json_only":        true,
		"required_keys":           []string{"classification", "candidate_id", "confidence", "reason"},
		"allowed_classifications": []string{"same", "related", "unrelated"},
		"proposed_incident":       proposed,
		"candidates":              compactCandidates,
	}
	promptJson, _ := json.Marshal(prompt)
	replyText, _ := callLlmEndpoint(
		endpointUrl,
		model,
		apiKey,
		[]map[string]any{
			{
				"role": "system",
				"content": "You classify observability incidents against existing GitHub issues. " +
					"Return a single JSON object only. Prefer 'same' only for clear duplicates, " +
					"'related' for likely same fault family but materially different work, otherwise 'unrelated'.",
			},
			{"role": "user", "content": string(promptJson)},
		},
		thinkingLevel,
		400,
		25,
		"",
	)
	parsed := extractFirstJsonObject(replyText)
	classification := strings.ToLower(strings.TrimSpace(agentFirstTruthyStr(parsed["classification"])))
	if classification != "same" && classification != "related" && classification != "unrelated" {
		return fallbackIssueDedupeDecision(proposed, candidates)
	}
	candidateId := strings.TrimSpace(agentFirstTruthyStr(parsed["candidate_id"]))
	confidence := 0.0
	if v, ok := coerceFloat(parsed["confidence"]); ok {
		confidence = v
	}
	return map[string]any{
		"classification": classification,
		"candidate_id":   candidateId,
		"confidence":     max(0.0, min(1.0, confidence)),
		"reason":         strings.TrimSpace(agentFirstTruthyStr(parsed["reason"])),
	}
}

// createGithubIssueRecord mirrors _create_github_issue_record.
// PORT-NOTE: keyword-only mask_output_enabled became positional; labels=nil
// means the Python default ["sobs-agent", "automated"].
func createGithubIssueRecord(
	githubToken, githubRepo, title, bodyMd string,
	labels []string,
	maskOutputEnabled bool,
) map[string]any {
	if githubToken == "" || githubRepo == "" {
		return map[string]any{}
	}
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		parts := []string{}
		for _, p := range strings.Split(strings.Trim(githubRepo, "/"), "/") {
			if p != "" {
				parts = append(parts, p)
			}
		}
		if len(parts) >= 2 {
			owner, repo = parts[len(parts)-2], parts[len(parts)-1]
		}
	}
	if owner == "" || repo == "" {
		return map[string]any{}
	}
	issueTitle := title
	issueBody := bodyMd
	if maskOutputEnabled {
		issueTitle = maskStringForOutput(title, nil)
		issueBody = maskStringForOutput(bodyMd, nil)
	}
	if labels == nil {
		labels = []string{"sobs-agent", "automated"}
	}
	issuePayload := map[string]any{
		"title":  issueTitle,
		"body":   issueBody,
		"labels": labels,
	}
	payloadJson, err := json.Marshal(issuePayload)
	if err != nil {
		logger.Warn(fmt.Sprintf("GitHub issue creation failed: %v", err))
		return map[string]any{"error": fmt.Sprintf("GitHub issue creation failed: %v", err)}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/issues", owner, repo),
		strings.NewReader(string(payloadJson)))
	if err != nil {
		logger.Warn(fmt.Sprintf("GitHub issue creation failed: %v", err))
		return map[string]any{"error": fmt.Sprintf("GitHub issue creation failed: %v", err)}
	}
	for k, v := range githubApiHeaders(githubToken, true, nil) {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		logger.Warn(fmt.Sprintf("GitHub issue creation failed: %v", err))
		return map[string]any{"error": fmt.Sprintf("GitHub issue creation failed: %v", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		// httpx.HTTPStatusError branch: prefer the API "message" field.
		detail := ""
		errPayload := map[string]any{}
		if readErr == nil && len(body) > 0 {
			if jsonErr := json.Unmarshal(body, &errPayload); jsonErr == nil {
				detail = strings.TrimSpace(agentFirstTruthyStr(errPayload["message"]))
			}
		}
		if detail == "" {
			// PORT-NOTE: Python falls back to str(exc); the Go port reports the
			// HTTP status line.
			detail = fmt.Sprintf("HTTP %d for %s", resp.StatusCode, req.URL)
		}
		logger.Warn("GitHub issue creation failed: " + detail)
		return map[string]any{"error": "GitHub issue creation failed: " + detail}
	}
	if readErr != nil {
		logger.Warn(fmt.Sprintf("GitHub issue creation failed: %v", readErr))
		return map[string]any{"error": fmt.Sprintf("GitHub issue creation failed: %v", readErr)}
	}
	result := map[string]any{}
	if err := json.Unmarshal(body, &result); err != nil {
		logger.Warn(fmt.Sprintf("GitHub issue creation failed: %v", err))
		return map[string]any{"error": fmt.Sprintf("GitHub issue creation failed: %v", err)}
	}
	return map[string]any{
		"issue_url":    agentFirstTruthyStr(result["html_url"]),
		"issue_number": coerceInt(result["number"]),
		"issue_title":  agentFirstTruthyStr(result["title"], title),
		"issue_state":  agentFirstTruthyStr(result["state"], "open"),
	}
}

// persistGithubWorkItem persists a GitHub issue decision as a work item for
// tracking and cross-linking.
// PORT-NOTE: Python keyword-only arguments became positional (declaration order).
func persistGithubWorkItem(
	db *ChDbConnection,
	runId string,
	rule map[string]any,
	triggerContext map[string]any,
	githubIssueUrl string,
	analysis string,
	suggestion string,
	agentAction string,
	issueTitle string,
	issueState string,
	dedupKey string,
	dedupDecision string,
	dedupConfidence float64,
	canonicalIssueUrl string,
	canonicalIssueNumber int,
	relatedIssueUrls []string,
	occurrenceCount int,
	copilotAssignmentRequestedAt int,
	copilotAssignmentStatus string,
	copilotAssignmentReason string,
	prLinked bool,
	prNumber int,
	prUrl string,
) {
	nowTs := normalizeChTimestamp(time.Now().UTC())
	issueNumber := 0
	parts := strings.Split(strings.TrimRight(githubIssueUrl, "/"), "/")
	if len(parts) > 0 {
		if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil && parts[len(parts)-1] != "" {
			issueNumber = n
		}
	}

	triggerFields := extractAgentTriggerFields(triggerContext)
	serviceName := agentFirstTruthyStr(triggerFields["service_name"])
	anomalyRuleId := agentFirstTruthyStr(triggerFields["anomaly_rule_id"])
	anomalyState := agentFirstTruthyStr(triggerFields["anomaly_state"])
	signalSource := agentFirstTruthyStr(triggerFields["signal_source"])
	signalName := agentFirstTruthyStr(triggerFields["signal_name"])
	signalValue := 0.0
	if v, ok := coerceFloat(triggerFields["signal_value"]); ok && agentTruthy(triggerFields["signal_value"]) {
		signalValue = v
	}

	githubRepo := ""
	issueSourceUrl := canonicalIssueUrl
	if issueSourceUrl == "" {
		issueSourceUrl = githubIssueUrl
	}
	if urlParts := strings.Split(issueSourceUrl, "/"); len(urlParts) >= 4 {
		githubRepo = urlParts[len(urlParts)-4] + "/" + urlParts[len(urlParts)-3]
	}

	canonicalNumber := canonicalIssueNumber
	if canonicalNumber == 0 {
		canonicalNumber = issueNumber
	}
	resolvedIssueUrl := githubIssueUrl
	if resolvedIssueUrl == "" {
		resolvedIssueUrl = canonicalIssueUrl
	}
	canonicalUrl := canonicalIssueUrl
	if canonicalUrl == "" {
		canonicalUrl = resolvedIssueUrl
	}
	if dedupDecision == "" {
		dedupDecision = "new_issue"
	}
	if copilotAssignmentStatus == "" {
		copilotAssignmentStatus = "not_requested"
	}
	if relatedIssueUrls == nil {
		relatedIssueUrls = []string{}
	}
	if occurrenceCount == 0 {
		occurrenceCount = 1
	}
	prLinkedInt := 0
	if prLinked {
		prLinkedInt = 1
	}

	workItem := Row{
		"Id":                           runId,
		"CreatedAt":                    nowTs,
		"CompletedAt":                  nowTs,
		"AgentRunId":                   runId,
		"AgentRuleId":                  rowString(rule["id"]),
		"AgentRuleName":                rowString(rule["name"]),
		"AgentAction":                  agentAction,
		"ServiceName":                  serviceName,
		"AnomalyRuleId":                anomalyRuleId,
		"AnomalyState":                 anomalyState,
		"SignalSource":                 signalSource,
		"SignalName":                   signalName,
		"SignalValue":                  signalValue,
		"GithubRepo":                   githubRepo,
		"DedupKey":                     dedupKey,
		"DedupDecision":                dedupDecision,
		"DedupConfidence":              dedupConfidence,
		"IssueNumber":                  issueNumber,
		"IssueUrl":                     resolvedIssueUrl,
		"CanonicalIssueNumber":         canonicalNumber,
		"CanonicalIssueUrl":            canonicalUrl,
		"RelatedIssueUrls":             safeJsonDumps(relatedIssueUrls),
		"OccurrenceCount":              max(1, occurrenceCount),
		"IssueState":                   issueState,
		"IssueTitle":                   issueTitle,
		"AnalysisSummary":              clipRunes(analysis, 500),
		"SuggestionSummary":            clipRunes(suggestion, 500),
		"CopilotAssignmentRequestedAt": copilotAssignmentRequestedAt,
		"CopilotAssignmentStatus":      copilotAssignmentStatus,
		"CopilotAssignmentReason":      copilotAssignmentReason,
		"PrLinked":                     prLinkedInt,
		"PrNumber":                     prNumber,
		"PrUrl":                        prUrl,
		"IsDeleted":                    0,
		"Version":                      time.Now().UnixMilli(),
	}

	if _, err := insertRowsJsonEachRow(db, "sobs_github_work_items", []Row{workItem}); err != nil {
		logger.Warn(fmt.Sprintf("Failed to persist work item: %v", err))
		return
	}
	invalidateWorkItemsCache()
}

// persistOnboardingWorkItem persists onboarding-created GitHub issues to the
// Work Items table.
// PORT-NOTE: Python keyword-only arguments became positional (declaration order).
func persistOnboardingWorkItem(
	db *ChDbConnection,
	githubRepo string,
	issueUrl string,
	issueNumber int,
	issueTitle string,
	issueState string,
	dedupDecision string,
	note string,
	copilotAssignmentStatus string,
	copilotAssignmentReason string,
	copilotAssignmentRequestedAt int,
	issueType string,
) {
	if issueUrl == "" {
		return
	}

	nowTs := normalizeChTimestamp(time.Now().UTC())
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		owner, repo, _ = parseIssueRefFromUrl(issueUrl)
	}
	githubRepoValue := githubRepo
	if owner != "" && repo != "" {
		githubRepoValue = owner + "/" + repo
	}

	dedupConfidence := 0.0
	if dedupDecision == "reused" {
		dedupConfidence = 1.0
	}
	if dedupDecision == "" {
		dedupDecision = "new_issue"
	}
	if issueState == "" {
		issueState = "open"
	}
	if copilotAssignmentStatus == "" {
		copilotAssignmentStatus = "not_requested"
	}

	workItem := Row{
		"Id":                           agentUuid4Hex(),
		"CreatedAt":                    nowTs,
		"CompletedAt":                  nowTs,
		"AgentRunId":                   "",
		"AgentRuleId":                  "",
		"AgentRuleName":                "Onboarding Wizard",
		"AgentAction":                  "onboarding_" + issueType,
		"ServiceName":                  repo,
		"AnomalyRuleId":                "",
		"AnomalyState":                 "",
		"SignalSource":                 "",
		"SignalName":                   "",
		"SignalValue":                  0.0,
		"GithubRepo":                   githubRepoValue,
		"DedupKey":                     "",
		"DedupDecision":                dedupDecision,
		"DedupConfidence":              dedupConfidence,
		"IssueNumber":                  issueNumber,
		"IssueUrl":                     issueUrl,
		"CanonicalIssueNumber":         issueNumber,
		"CanonicalIssueUrl":            issueUrl,
		"RelatedIssueUrls":             "[]",
		"OccurrenceCount":              1,
		"IssueState":                   issueState,
		"IssueTitle":                   issueTitle,
		"AnalysisSummary":              "Sobs onboarding wizard issue.",
		"SuggestionSummary":            note,
		"CopilotAssignmentRequestedAt": copilotAssignmentRequestedAt,
		"CopilotAssignmentStatus":      copilotAssignmentStatus,
		"CopilotAssignmentReason":      copilotAssignmentReason,
		"PrLinked":                     0,
		"PrNumber":                     0,
		"PrUrl":                        "",
		"IsDeleted":                    0,
		"Version":                      time.Now().UnixMilli(),
	}
	if _, err := insertRowsJsonEachRow(db, "sobs_github_work_items", []Row{workItem}); err != nil {
		logger.Warn(fmt.Sprintf("Failed to persist onboarding work item: %v", err))
		return
	}
	invalidateWorkItemsCache()
}

// ---------------------------------------------------------------------------
// Agent flow
// ---------------------------------------------------------------------------

// buildAgentContextSummary builds a plain-text summary of current
// observability state for the LLM.
func buildAgentContextSummary(db *ChDbConnection, triggerContext map[string]any) string {
	lines := []string{}
	lines = append(lines, "=== SOBS Observability Context ===")

	ruleName := "unknown rule"
	if v, ok := triggerContext["rule_name"]; ok {
		ruleName = rowString(v)
	}
	triggerState := rowString(triggerContext["trigger_state"])
	lines = append(lines, fmt.Sprintf("Triggered by: %s (%s)", ruleName, triggerState))

	// Additional context from trigger (user-provided or automated)
	extra := triggerContext["extra"]
	extraDict := map[string]any{}
	if m, ok := extra.(map[string]any); ok {
		extraDict = m
	} else if agentTruthy(extra) {
		if parsed, ok := safeJsonLoads(rowString(extra), map[string]any{}).(map[string]any); ok {
			extraDict = parsed
		}
	}

	additionalContext := strings.TrimSpace(agentFirstTruthyStr(extraDict["additional_context"]))
	if additionalContext != "" {
		lines = append(lines, "\nUser-provided context: "+additionalContext)
	}

	// Event frequency / noise analysis — only when we have enough scope (service + err_type).
	// Without both, the counts would represent "all errors for this service" which is too
	// broad to be a meaningful noise indicator and can mislead the LLM.
	service := strings.TrimSpace(agentFirstTruthyStr(extraDict["service"], triggerContext["service"]))
	errType := strings.TrimSpace(agentFirstTruthyStr(extraDict["err_type"]))
	if service != "" && errType != "" {
		// Single query with countIf for both windows to halve DB round-trips.
		res, err := db.Execute(
			"SELECT "+
				"  countIf(Timestamp >= now() - INTERVAL 1 HOUR) AS c_1h, "+
				"  count() AS c_24h "+
				"FROM otel_logs "+
				"WHERE Timestamp >= now() - INTERVAL 24 HOUR "+
				"  AND SeverityText IN ('ERROR','FATAL') "+
				"  AND ServiceName = ? "+
				"  AND LogAttributes['exception.type'] = ?",
			service, errType)
		if err == nil {
			freqRow := res.Fetchone()
			count1h := 0
			count24h := 0
			if freqRow != nil {
				count1h = coerceInt(freqRow["c_1h"])
				count24h = coerceInt(freqRow["c_24h"])
			}
			lines = append(lines, fmt.Sprintf("\nEvent frequency (%s / %s):", service, errType))
			lines = append(lines, fmt.Sprintf("  Last 1h:  %d occurrence(s)", count1h))
			lines = append(lines, fmt.Sprintf("  Last 24h: %d occurrence(s)", count24h))
			if count1h <= 1 && count24h <= 2 {
				lines = append(lines, "  Noise indicator: LOW recurrence — may be an isolated event")
			} else if count1h >= 10 || count24h >= 50 {
				lines = append(lines, "  Noise indicator: HIGH recurrence — persistent or systemic pattern")
			} else {
				lines = append(lines, "  Noise indicator: MODERATE recurrence — monitor for escalation")
			}
		}
	}

	// Recent errors (broader context across all services)
	if res, err := db.Execute(
		"SELECT ServiceName, ExceptionType, count() AS c " +
			"FROM otel_logs FINAL " +
			"WHERE Timestamp >= now() - INTERVAL 1 HOUR AND SeverityText IN ('ERROR','FATAL') " +
			"GROUP BY ServiceName, ExceptionType ORDER BY c DESC LIMIT 5"); err == nil {
		errRows := res.Fetchall()
		if len(errRows) > 0 {
			lines = append(lines, "\nRecent errors (last 1h, all services):")
			for _, r := range errRows {
				lines = append(lines, fmt.Sprintf("  %s | %s x%s", rowString(r["ServiceName"]), rowString(r["ExceptionType"]), rowString(r["c"])))
			}
		}
	}

	// Recent anomaly states
	if res, err := db.Execute(
		"SELECT ServiceName, Name AS Signal, anomaly_state " +
			"FROM v_derived_signals_anomaly " +
			"WHERE anomaly_state != 'normal' " +
			"AND time >= now() - INTERVAL 2 HOUR " +
			"LIMIT 5"); err == nil {
		anomRows := res.Fetchall()
		if len(anomRows) > 0 {
			lines = append(lines, "\nActive anomalies:")
			for _, r := range anomRows {
				lines = append(lines, fmt.Sprintf("  %s | %s → %s", rowString(r["ServiceName"]), rowString(r["Signal"]), rowString(r["anomaly_state"])))
			}
		}
	}

	// Remaining extra fields (exclude already-rendered keys)
	renderedExtraKeys := map[string]bool{"additional_context": true, "mask_output": true, "initiated_by": true}
	if len(extraDict) > 0 {
		remaining := map[string]any{}
		for k, v := range extraDict {
			if !renderedExtraKeys[k] {
				remaining[k] = v
			}
		}
		if len(remaining) > 0 {
			// PORT-NOTE: Python interpolates the dict repr; the Go port renders
			// JSON (stable key order) instead of Go map syntax.
			lines = append(lines, "\nTrigger details: "+safeJsonDumps(remaining))
		}
	} else if agentTruthy(extra) {
		lines = append(lines, "\nAdditional context: "+rowString(extra))
	}

	return strings.Join(lines, "\n")
}

func extractTriggerServiceName(triggerContext map[string]any) string {
	service := strings.TrimSpace(agentFirstTruthyStr(triggerContext["service"]))
	if service != "" {
		return service
	}

	extraRaw := triggerContext["extra"]
	var extra map[string]any
	if m, ok := extraRaw.(map[string]any); ok {
		extra = m
	} else if parsed, ok := safeJsonLoads(rowString(extraRaw), map[string]any{}).(map[string]any); ok {
		extra = parsed
	}

	if extra != nil {
		for _, key := range []string{"service", "service_name", "ServiceName"} {
			value := strings.TrimSpace(agentFirstTruthyStr(extra[key]))
			if value != "" {
				return value
			}
		}
	}
	return ""
}

// resolveAgentGithubTarget resolves (repo, token) for agent GitHub issue creation.
//
// Priority:
//  1. Repo inferred from trigger service mapped via sobs_apps Name/Slug/RepoUrl
//     with repo-scoped token when configured.
//  2. Global ai.github_repo + per-repo token for that repo if present.
//  3. Global ai.github_token fallback.
func resolveAgentGithubTarget(
	db *ChDbConnection,
	settings map[string]string,
	triggerContext map[string]any,
) (string, string) {
	defaultRepo := strings.TrimSpace(settings["ai.github_repo"])
	defaultToken := strings.TrimSpace(settings["ai.github_token"])

	serviceName := extractTriggerServiceName(triggerContext)
	if serviceName != "" {
		res, err := db.Execute(
			"SELECT RepoUrl FROM sobs_apps FINAL "+
				"WHERE IsDeleted=0 AND Enabled=1 AND RepoUrl != '' "+
				"AND (lower(Name)=lower(?) OR lower(Slug)=lower(?)) "+
				"ORDER BY UpdatedAt DESC LIMIT 1",
			serviceName, serviceName)
		if err != nil {
			// PORT-NOTE: Python propagated the exception; the Go port logs and
			// falls back to the global settings.
			logger.Warn(fmt.Sprintf("resolveAgentGithubTarget query failed: %v", err))
		} else if row := res.Fetchone(); row != nil {
			owner, repo := parseGithubRepoOwnerName(rowString(row["RepoUrl"]))
			if owner != "" && repo != "" {
				scopedToken := loadRepoScopedGithubToken(db, owner, repo)
				token := scopedToken
				if token == "" {
					token = defaultToken
				}
				return owner + "/" + repo, token
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
			scopedToken := loadRepoScopedGithubToken(db, owner, repo)
			token := scopedToken
			if token == "" {
				token = defaultToken
			}
			return owner + "/" + repo, token
		}
		return defaultRepo, defaultToken
	}

	return "", defaultToken
}

// runAgentFlow executes the full agent flow for a given rule. Updates
// sobs_agent_runs in place.
func runAgentFlow(
	db *ChDbConnection,
	rule map[string]any,
	settings map[string]string,
	triggerContext map[string]any,
	runId string,
) (map[string]any, error) {
	updateRun := func(updates map[string]any) error {
		version := time.Now().UnixMilli()
		row := Row{"Id": runId, "IsDeleted": 0, "Version": version}
		for k, v := range updates {
			row[k] = v
		}
		_, err := insertRowsJsonEachRow(db, "sobs_agent_runs", []Row{row})
		return err
	}

	if err := updateRun(map[string]any{"Status": "running"}); err != nil {
		return nil, err
	}

	endpointUrl := strings.TrimSpace(settings["ai.endpoint_url"])
	model := strings.TrimSpace(agentSettingsGet(settings, "ai.model", "gpt-4o-mini"))
	apiKey := strings.TrimSpace(settings["ai.api_key"])
	dlpUrl := strings.TrimSpace(settings["ai.dlp_endpoint_url"])
	githubRepo, githubToken := resolveAgentGithubTarget(db, settings, triggerContext)
	actions := map[string]bool{}
	for _, action := range agentStringSlice(rule["actions"]) {
		actions[action] = true
	}
	maskOutputEnabled := true
	extraRaw := triggerContext["extra"]
	if m, ok := extraRaw.(map[string]any); ok {
		maskOutputEnabled = parseBool(m["mask_output"], true)
	} else if agentTruthy(extraRaw) {
		if parsedExtra, ok := safeJsonLoads(rowString(extraRaw), map[string]any{}).(map[string]any); ok {
			maskOutputEnabled = parseBool(parsedExtra["mask_output"], true)
		}
	}
	maxIssues := aiAgentMaxIssuesDefault
	if raw := strings.TrimSpace(settings["ai.agent_max_issues_per_hour"]); raw == "" {
		maxIssues = max(1, min(20, aiAgentMaxIssuesDefault))
	} else if n, err := strconv.Atoi(raw); err == nil {
		maxIssues = max(1, min(20, n))
	}

	contextSummary := buildAgentContextSummary(db, triggerContext)

	// 1. Guard model check
	allowed, guardReason, _ := checkGuardModel(settings, contextSummary, "")
	guardDecision := "allowed"
	if !allowed {
		guardDecision = "blocked: " + guardReason
		if err := updateRun(map[string]any{
			"Status":        "blocked_by_guard",
			"GuardDecision": guardDecision,
			"CompletedAt":   normalizeChTimestamp(time.Now().UTC()),
		}); err != nil {
			return nil, err
		}
		return map[string]any{"status": "blocked_by_guard", "guard_decision": guardDecision}, nil
	}

	// 2. LLM root-cause analysis
	analysis := ""
	suggestion := ""
	if actions["analyze"] && endpointUrl != "" && model != "" {
		systemPrompt := strings.TrimSpace(settings["ai.system_prompt"])
		if systemPrompt == "" {
			systemPrompt = "You are an expert SRE and observability engineer. " +
				"Analyse the provided telemetry context and provide a concise root cause analysis " +
				"and a specific, actionable suggested fix. " +
				"Before concluding, assess whether this event is NOISE (transient, self-resolving, " +
				"e.g. a single reconnection attempt that succeeded, a brief timeout that did not recur) " +
				"or IMPACT (persistent fault, exhausted retries, service degradation, user-facing error). " +
				"If the event frequency is low (≤2 occurrences) and there are no active anomalies or related " +
				"errors, note that this may be noise and recommend monitoring rather than immediate escalation. " +
				"Format your response as:\n" +
				"NOISE_OR_IMPACT: <NOISE|IMPACT|UNCERTAIN>\n" +
				"ROOT CAUSE: <text>\n" +
				"SUGGESTED FIX: <text>"
		}
		messages := []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": contextSummary},
		}
		reply, _ := callLlmEndpoint(endpointUrl, model, apiKey, messages, "off", 512, 30, "")
		if strings.Contains(reply, "SUGGESTED FIX:") {
			parts := strings.SplitN(reply, "SUGGESTED FIX:", 2)
			analysis = strings.TrimSpace(strings.ReplaceAll(parts[0], "ROOT CAUSE:", ""))
			suggestion = strings.TrimSpace(parts[1])
		} else {
			analysis = strings.TrimSpace(reply)
		}
		// Strip the NOISE_OR_IMPACT classification line from analysis so it doesn't
		// appear as raw header text in the generated GitHub issue.
		if strings.HasPrefix(analysis, "NOISE_OR_IMPACT:") {
			firstNewline := strings.Index(analysis, "\n")
			if firstNewline != -1 {
				analysis = strings.TrimSpace(analysis[firstNewline:])
			} else {
				analysis = ""
			}
		}
	}

	// 3. Optional DLP check before GitHub issue creation
	dlpResult := "skipped"
	githubIssueUrl := ""

	wantsIssue := actions["github_issue"] || actions["github_issue_copilot"]
	wantsCopilotAssignment := actions["github_issue_copilot"]
	issueOutcome := map[string]any{}

	if wantsIssue && githubToken != "" && githubRepo != "" {
		issueText := fmt.Sprintf("%s\n\nAnalysis: %s\n\nSuggestion: %s", contextSummary, analysis, suggestion)

		if actions["dlp_check"] && dlpUrl != "" {
			dlpClean, dlpDetail := checkDlpEndpoint(dlpUrl, issueText, apiKey)
			if dlpClean {
				dlpResult = "clean"
			} else {
				dlpResult = "flagged: " + dlpDetail
			}
			if !dlpClean {
				if err := updateRun(map[string]any{
					"Status":        "completed",
					"GuardDecision": guardDecision,
					"DlpResult":     dlpResult,
					"Analysis":      analysis,
					"Suggestion":    suggestion,
					"CompletedAt":   normalizeChTimestamp(time.Now().UTC()),
				}); err != nil {
					return nil, err
				}
				return map[string]any{
					"status":     "completed",
					"dlp_result": dlpResult,
					"analysis":   analysis,
					"suggestion": suggestion,
				}, nil
			}
		}

		issuesThisHour := countGithubIssuesLastHour(db)
		allowNewIssue := issuesThisHour < maxIssues
		triggerFields := extractAgentTriggerFields(triggerContext)
		issueTitle := buildAgentIssueTitle(rule, triggerFields)

		// Include user-provided additional context in the issue body when present.
		extraRaw := triggerContext["extra"]
		extraForBody := map[string]any{}
		if m, ok := extraRaw.(map[string]any); ok {
			extraForBody = m
		} else if agentTruthy(extraRaw) {
			if parsed, ok := safeJsonLoads(rowString(extraRaw), map[string]any{}).(map[string]any); ok {
				extraForBody = parsed
			}
		}
		additionalContext := strings.TrimSpace(agentFirstTruthyStr(extraForBody["additional_context"]))
		additionalContextSection := ""
		if additionalContext != "" {
			additionalContextSection = "\n### Additional Context\n" + additionalContext + "\n"
		}

		issueBody := fmt.Sprintf(
			"## SOBS Automated Agent Report\n\n"+
				"**Rule:** %s  \n"+
				"**Trigger state:** %s  \n"+
				"**Service:** %s  \n"+
				"**Signal:** %s/%s  \n\n"+
				"### Telemetry Context\n```\n%s\n```\n\n"+
				"### Root Cause Analysis\n%s\n\n"+
				"### Suggested Fix\n%s\n"+
				"%s\n"+
				"---\n*Generated automatically by [SOBS](https://github.com/abartrim/sobs). "+
				"Please review before acting.*",
			agentRuleNameOrDefault(rule),
			rowString(triggerContext["trigger_state"]),
			rowString(triggerFields["service_name"]),
			rowString(triggerFields["signal_source"]),
			rowString(triggerFields["signal_name"]),
			contextSummary,
			analysis,
			suggestion,
			additionalContextSection,
		)
		issueOutcome = chooseGithubIssueOutcome(
			db,
			settings,
			rule,
			triggerContext,
			githubRepo,
			githubToken,
			wantsCopilotAssignment,
			analysis,
			suggestion,
			issueTitle,
			issueBody,
			allowNewIssue,
			maskOutputEnabled,
		)
		if issueOutcome == nil {
			issueOutcome = map[string]any{}
		}
		githubIssueUrl = agentFirstTruthyStr(issueOutcome["issue_url"])
	}

	completedTs := normalizeChTimestamp(time.Now().UTC())

	if wantsIssue && (githubIssueUrl != "" || len(issueOutcome) > 0) {
		agentAction := "github_issue"
		if wantsCopilotAssignment {
			agentAction = "github_issue_copilot"
		}
		dedupConfidence := 0.0
		if v, ok := coerceFloat(issueOutcome["dedup_confidence"]); ok {
			dedupConfidence = v
		}
		persistGithubWorkItem(
			db,
			runId,
			rule,
			triggerContext,
			githubIssueUrl,
			analysis,
			suggestion,
			agentAction,
			agentFirstTruthyStr(issueOutcome["issue_title"]),
			agentFirstTruthyStr(issueOutcome["issue_state"]),
			agentFirstTruthyStr(issueOutcome["dedup_key"]),
			agentFirstTruthyStr(issueOutcome["dedup_decision"], "new_issue"),
			dedupConfidence,
			agentFirstTruthyStr(issueOutcome["canonical_issue_url"], githubIssueUrl),
			coerceInt(issueOutcome["canonical_issue_number"]),
			agentStringSliceOrEmpty(issueOutcome["related_issue_urls"]),
			coerceIntOrDefault(issueOutcome["occurrence_count"], 1),
			coerceInt(issueOutcome["copilot_assignment_requested_at"]),
			agentFirstTruthyStr(issueOutcome["copilot_assignment_status"], "not_requested"),
			agentFirstTruthyStr(issueOutcome["copilot_assignment_reason"]),
			agentTruthy(issueOutcome["pr_linked"]),
			coerceInt(issueOutcome["pr_number"]),
			agentFirstTruthyStr(issueOutcome["pr_url"]),
		)
	}

	if err := updateRun(map[string]any{
		"Status":         "completed",
		"GuardDecision":  guardDecision,
		"DlpResult":      dlpResult,
		"Analysis":       analysis,
		"Suggestion":     suggestion,
		"GithubIssueUrl": githubIssueUrl,
		"CompletedAt":    completedTs,
	}); err != nil {
		return nil, err
	}
	emitAgentIssueDecisionSummary(
		runId,
		rule,
		triggerContext,
		issueOutcome,
		githubIssueUrl,
		wantsIssue,
		wantsCopilotAssignment,
		githubRepo,
	)
	return map[string]any{
		"status":                    "completed",
		"guard_decision":            guardDecision,
		"dlp_result":                dlpResult,
		"analysis":                  analysis,
		"suggestion":                suggestion,
		"github_issue_url":          githubIssueUrl,
		"dedup_decision":            agentFirstTruthyStr(issueOutcome["dedup_decision"]),
		"issue_error":               agentFirstTruthyStr(issueOutcome["issue_error"]),
		"copilot_assignment_status": agentFirstTruthyStr(issueOutcome["copilot_assignment_status"]),
		"copilot_assignment_reason": agentFirstTruthyStr(issueOutcome["copilot_assignment_reason"]),
	}, nil
}

// agentRuleNameOrDefault mirrors rule.get("name", "Agent Rule") (default only
// when the key is missing).
func agentRuleNameOrDefault(rule map[string]any) string {
	if v, ok := rule["name"]; ok {
		return rowString(v)
	}
	return "Agent Rule"
}

// agentStringSliceOrEmpty mirrors list(value or []) for str lists.
func agentStringSliceOrEmpty(value any) []string {
	if out := agentStringSlice(value); out != nil {
		return out
	}
	return []string{}
}

// coerceIntOrDefault mirrors int(value or default).
func coerceIntOrDefault(value any, def int) int {
	if !agentTruthy(value) {
		return def
	}
	return coerceInt(value)
}

// ---------------------------------------------------------------------------
// Notification schema migration + seed content
// ---------------------------------------------------------------------------

// ensureNotificationSchema runs additive migrations to ensure notification
// tables have all expected columns.
func ensureNotificationSchema(db *ChDbConnection) {
	migrationStatements := []string{
		"ALTER TABLE sobs_notification_channels ADD COLUMN IF NOT EXISTS " + "Enabled UInt8 DEFAULT 1",
	}
	for _, statement := range migrationStatements {
		if _, err := db.Execute(statement); err != nil {
			// table may not exist yet (will be created by CREATE IF NOT EXISTS in SCHEMA)
			logger.Debug(fmt.Sprintf("notification schema migration skipped: %v", err))
		}
	}
}

func seedRuleIfMissing(db *ChDbConnection, rule Row) {
	res, err := db.Execute(
		"SELECT 1 FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 AND Name = ? LIMIT 1",
		rowString(rule["Name"]))
	if err != nil {
		logger.Warn(fmt.Sprintf("seedRuleIfMissing query failed: %v", err))
		return
	}
	if res.Fetchone() != nil {
		return
	}
	if _, err := insertRowsJsonEachRow(db, "sobs_anomaly_rules", []Row{rule}); err != nil {
		logger.Warn(fmt.Sprintf("seedRuleIfMissing insert failed: %v", err))
	}
}

func seedDashboardIfMissing(db *ChDbConnection, dashboardName, description string) string {
	res, err := db.Execute(
		"SELECT Id FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Name = ? LIMIT 1",
		dashboardName)
	if err != nil {
		logger.Warn(fmt.Sprintf("seedDashboardIfMissing query failed: %v", err))
		return ""
	}
	if existing := res.Fetchone(); existing != nil {
		return rowString(existing["Id"])
	}

	dashboardId := agentUuid4()
	if _, err := insertRowsJsonEachRow(db, "sobs_dashboards", []Row{
		{
			"Id":          dashboardId,
			"Name":        dashboardName,
			"Description": description,
			"IsDeleted":   0,
			"Version":     time.Now().UnixMilli(),
		},
	}); err != nil {
		logger.Warn(fmt.Sprintf("seedDashboardIfMissing insert failed: %v", err))
	}
	return dashboardId
}

func seedChartIfMissing(db *ChDbConnection, dashboardId, title, chartType, query string, position int) {
	res, err := db.Execute(
		"SELECT 1 FROM sobs_chart_configs FINAL WHERE IsDeleted = 0 AND DashboardId = ? AND Title = ? LIMIT 1",
		dashboardId, title)
	if err != nil {
		logger.Warn(fmt.Sprintf("seedChartIfMissing query failed: %v", err))
		return
	}
	if res.Fetchone() != nil {
		return
	}
	optionsJson, _ := json.Marshal(map[string]any{"chart_spec": buildRawChartSpec(chartType, query, "")})
	if _, err := insertRowsJsonEachRow(db, "sobs_chart_configs", []Row{
		{
			"Id":          agentUuid4(),
			"DashboardId": dashboardId,
			"Title":       title,
			"ChartType":   chartType,
			"Query":       query,
			"OptionsJson": string(optionsJson),
			"Position":    position,
			"IsDeleted":   0,
			"Version":     time.Now().UnixMilli(),
		},
	}); err != nil {
		logger.Warn(fmt.Sprintf("seedChartIfMissing insert failed: %v", err))
	}
}

func upsertSeedChart(db *ChDbConnection, dashboardId, title, chartType, query string, position int) {
	res, err := db.Execute(
		"SELECT Id, ChartType, Query, OptionsJson, Position "+
			"FROM sobs_chart_configs FINAL "+
			"WHERE IsDeleted = 0 AND DashboardId = ? AND Title = ? LIMIT 1",
		dashboardId, title)
	if err != nil {
		logger.Warn(fmt.Sprintf("upsertSeedChart query failed: %v", err))
		return
	}
	existing := res.Fetchone()
	if existing == nil {
		seedChartIfMissing(db, dashboardId, title, chartType, query, position)
		return
	}

	if rowString(existing["ChartType"]) == chartType &&
		rowString(existing["Query"]) == query &&
		coerceInt(existing["Position"]) == position {
		return
	}

	optionsJson, _ := json.Marshal(map[string]any{
		"chart_spec": buildRawChartSpec(chartType, query, rowString(existing["OptionsJson"])),
	})
	if _, err := insertRowsJsonEachRow(db, "sobs_chart_configs", []Row{
		{
			"Id":          rowString(existing["Id"]),
			"DashboardId": dashboardId,
			"Title":       title,
			"ChartType":   chartType,
			"Query":       query,
			"OptionsJson": string(optionsJson),
			"Position":    position,
			"IsDeleted":   0,
			"Version":     time.Now().UnixMilli(),
		},
	}); err != nil {
		logger.Warn(fmt.Sprintf("upsertSeedChart insert failed: %v", err))
	}
}

func softDeleteSeedChartByTitle(db *ChDbConnection, dashboardId, title string) {
	res, err := db.Execute(
		"SELECT Id, ChartType, Query, OptionsJson, Position "+
			"FROM sobs_chart_configs FINAL "+
			"WHERE IsDeleted = 0 AND DashboardId = ? AND Title = ? LIMIT 1",
		dashboardId, title)
	if err != nil {
		logger.Warn(fmt.Sprintf("softDeleteSeedChartByTitle query failed: %v", err))
		return
	}
	row := res.Fetchone()
	if row == nil {
		return
	}
	if _, err := insertRowsJsonEachRow(db, "sobs_chart_configs", []Row{
		{
			"Id":          rowString(row["Id"]),
			"DashboardId": dashboardId,
			"Title":       title,
			"ChartType":   rowString(row["ChartType"]),
			"Query":       rowString(row["Query"]),
			"OptionsJson": rowString(row["OptionsJson"]),
			"Position":    coerceInt(row["Position"]),
			"IsDeleted":   1,
			"Version":     time.Now().UnixMilli(),
		},
	}); err != nil {
		logger.Warn(fmt.Sprintf("softDeleteSeedChartByTitle insert failed: %v", err))
	}
}

func seedExampleMetricsContent(db *ChDbConnection) {
	version := time.Now().UnixMilli()
	exampleRules := []Row{
		{
			"Id":                         agentUuid4(),
			"Name":                       "Trace latency elevated",
			"RuleType":                   "threshold",
			"SignalSource":               "traces",
			"SignalName":                 "latency_p95_ms",
			"ServiceName":                "trace-svc-0",
			"AttrFingerprint":            "",
			"Comparator":                 "gt",
			"WarningThreshold":           250.0,
			"CriticalThreshold":          450.0,
			"SecondarySignalSource":      "",
			"SecondarySignalName":        "",
			"SecondaryComparator":        "gt",
			"SecondaryWarningThreshold":  0.0,
			"SecondaryCriticalThreshold": 0.0,
			"MinSampleCount":             5,
			"IsDeleted":                  0,
			"Version":                    version,
		},
		{
			"Id":                         agentUuid4(),
			"Name":                       "Trace error ratio elevated",
			"RuleType":                   "threshold",
			"SignalSource":               "traces",
			"SignalName":                 "trace_error_ratio",
			"ServiceName":                "trace-svc-0",
			"AttrFingerprint":            "",
			"Comparator":                 "gt",
			"WarningThreshold":           0.04,
			"CriticalThreshold":          0.08,
			"SecondarySignalSource":      "",
			"SecondarySignalName":        "",
			"SecondaryComparator":        "gt",
			"SecondaryWarningThreshold":  0.0,
			"SecondaryCriticalThreshold": 0.0,
			"MinSampleCount":             5,
			"IsDeleted":                  0,
			"Version":                    version,
		},
		{
			"Id":                         agentUuid4(),
			"Name":                       "Exception volume elevated",
			"RuleType":                   "threshold",
			"SignalSource":               "errors",
			"SignalName":                 "exception_volume",
			"ServiceName":                "err-svc-0",
			"AttrFingerprint":            "",
			"Comparator":                 "gt",
			"WarningThreshold":           1.0,
			"CriticalThreshold":          3.0,
			"SecondarySignalSource":      "",
			"SecondarySignalName":        "",
			"SecondaryComparator":        "gt",
			"SecondaryWarningThreshold":  0.0,
			"SecondaryCriticalThreshold": 0.0,
			"MinSampleCount":             1,
			"IsDeleted":                  0,
			"Version":                    version,
		},
		{
			"Id":                         agentUuid4(),
			"Name":                       "Composite trace distress",
			"RuleType":                   "composite",
			"SignalSource":               "traces",
			"SignalName":                 "latency_p95_ms",
			"ServiceName":                "trace-svc-0",
			"AttrFingerprint":            "",
			"Comparator":                 "gt",
			"WarningThreshold":           250.0,
			"CriticalThreshold":          450.0,
			"SecondarySignalSource":      "traces",
			"SecondarySignalName":        "trace_error_ratio",
			"SecondaryComparator":        "gt",
			"SecondaryWarningThreshold":  0.04,
			"SecondaryCriticalThreshold": 0.08,
			"MinSampleCount":             5,
			"IsDeleted":                  0,
			"Version":                    version,
		},
	}
	for _, rule := range exampleRules {
		seedRuleIfMissing(db, rule)
	}

	dashboardId := seedDashboardIfMissing(
		db,
		"Example Derived Signals",
		"Seeded dashboard for load_example-derived log, trace, and error anomaly signals.",
	)
	charts := []struct {
		title     string
		chartType string
		query     string
	}{
		{
			"Trace volume",
			"derived_signal_overlay",
			"SELECT\n" +
				"  time,\n" +
				"  ServiceName AS service,\n" +
				"  SignalSource AS source,\n" +
				"  SignalName AS signal,\n" +
				"  AttrFingerprint AS attr_fp,\n" +
				"  value,\n" +
				"  SampleCount AS sample_count,\n" +
				"  baseline_mean,\n" +
				"  baseline_lower,\n" +
				"  baseline_upper,\n" +
				"  anomaly_state,\n" +
				"  anomaly_score\n" +
				"FROM v_derived_signals_anomaly\n" +
				"WHERE ServiceName = (\n" +
				"  SELECT ServiceName\n" +
				"  FROM v_derived_signals_anomaly\n" +
				"  WHERE SignalSource = 'traces' AND SignalName = 'trace_volume'\n" +
				"  ORDER BY time DESC\n" +
				"  LIMIT 1\n" +
				")\n" +
				"  AND SignalSource = 'traces'\n" +
				"  AND SignalName = 'trace_volume'\n" +
				"  AND time >= now() - INTERVAL 6 HOUR\n" +
				"ORDER BY time",
		},
		{
			"Trace error ratio",
			"derived_signal_overlay",
			"SELECT\n" +
				"  time,\n" +
				"  ServiceName AS service,\n" +
				"  SignalSource AS source,\n" +
				"  SignalName AS signal,\n" +
				"  AttrFingerprint AS attr_fp,\n" +
				"  value,\n" +
				"  SampleCount AS sample_count,\n" +
				"  baseline_mean,\n" +
				"  baseline_lower,\n" +
				"  baseline_upper,\n" +
				"  anomaly_state,\n" +
				"  anomaly_score\n" +
				"FROM v_derived_signals_anomaly\n" +
				"WHERE ServiceName = (\n" +
				"  SELECT ServiceName\n" +
				"  FROM v_derived_signals_anomaly\n" +
				"  WHERE SignalSource = 'traces' AND SignalName = 'trace_error_ratio'\n" +
				"  ORDER BY time DESC\n" +
				"  LIMIT 1\n" +
				")\n" +
				"  AND SignalSource = 'traces'\n" +
				"  AND SignalName = 'trace_error_ratio'\n" +
				"  AND time >= now() - INTERVAL 6 HOUR\n" +
				"ORDER BY time",
		},
		{
			"Load log volume",
			"derived_signal_overlay",
			"SELECT\n" +
				"  time,\n" +
				"  ServiceName AS service,\n" +
				"  SignalSource AS source,\n" +
				"  SignalName AS signal,\n" +
				"  AttrFingerprint AS attr_fp,\n" +
				"  value,\n" +
				"  SampleCount AS sample_count,\n" +
				"  baseline_mean,\n" +
				"  baseline_lower,\n" +
				"  baseline_upper,\n" +
				"  anomaly_state,\n" +
				"  anomaly_score\n" +
				"FROM v_derived_signals_anomaly\n" +
				"WHERE ServiceName = (\n" +
				"  SELECT ServiceName\n" +
				"  FROM v_derived_signals_anomaly\n" +
				"  WHERE SignalSource = 'logs' AND SignalName = 'log_volume'\n" +
				"  ORDER BY time DESC\n" +
				"  LIMIT 1\n" +
				")\n" +
				"  AND SignalSource = 'logs'\n" +
				"  AND SignalName = 'log_volume'\n" +
				"  AND time >= now() - INTERVAL 6 HOUR\n" +
				"ORDER BY time",
		},
		{
			"Exception volume",
			"derived_signal_overlay",
			"SELECT\n" +
				"  time,\n" +
				"  ServiceName AS service,\n" +
				"  SignalSource AS source,\n" +
				"  SignalName AS signal,\n" +
				"  AttrFingerprint AS attr_fp,\n" +
				"  value,\n" +
				"  SampleCount AS sample_count,\n" +
				"  baseline_mean,\n" +
				"  baseline_lower,\n" +
				"  baseline_upper,\n" +
				"  anomaly_state,\n" +
				"  anomaly_score\n" +
				"FROM v_derived_signals_anomaly\n" +
				"WHERE ServiceName = (\n" +
				"  SELECT ServiceName\n" +
				"  FROM v_derived_signals_anomaly\n" +
				"  WHERE SignalSource = 'errors' AND SignalName = 'exception_volume'\n" +
				"  ORDER BY time DESC\n" +
				"  LIMIT 1\n" +
				")\n" +
				"  AND SignalSource = 'errors'\n" +
				"  AND SignalName = 'exception_volume'\n" +
				"  AND time >= now() - INTERVAL 6 HOUR\n" +
				"ORDER BY time",
		},
	}
	for position, chart := range charts {
		upsertSeedChart(db, dashboardId, chart.title, chart.chartType, chart.query, position)
	}
	softDeleteSeedChartByTitle(db, dashboardId, "Trace latency")
}

// cwvRules mirrors _CWV_RULES: (name, signal, comparator, warn, crit).
var cwvRules = []struct {
	name       string
	signal     string
	comparator string
	warn       float64
	crit       float64
}{
	{"CWV LCP", "LCP", "gt", 2500.0, 4000.0},
	{"CWV INP", "INP", "gt", 200.0, 500.0},
	{"CWV CLS", "CLS", "gt", 0.1, 0.25},
	{"CWV TTFB", "TTFB", "gt", 800.0, 1800.0},
	{"CWV FCP", "FCP", "gt", 1800.0, 3000.0},
	{"CWV FID", "FID", "gt", 100.0, 300.0},
}

// seedCwvAnomalyRules seeds default Core Web Vitals threshold rules into
// sobs_anomaly_rules.
func seedCwvAnomalyRules(db *ChDbConnection) {
	version := time.Now().UnixMilli()
	for _, r := range cwvRules {
		seedRuleIfMissing(db, Row{
			"Id":                         agentUuid4(),
			"Name":                       r.name,
			"RuleType":                   "threshold",
			"SignalSource":               "rum_vitals",
			"SignalName":                 r.signal,
			"ServiceName":                "",
			"AttrFingerprint":            "",
			"Comparator":                 r.comparator,
			"WarningThreshold":           r.warn,
			"CriticalThreshold":          r.crit,
			"SecondarySignalSource":      "",
			"SecondarySignalName":        "",
			"SecondaryComparator":        "gt",
			"SecondaryWarningThreshold":  0.0,
			"SecondaryCriticalThreshold": 0.0,
			"MinSampleCount":             5,
			"IsDeleted":                  0,
			"Version":                    version,
		})
	}
}

// ---------------------------------------------------------------------------
// DB write worker (shared state — writeQueue/writeThread/etc. — in s02_db.go)
// ---------------------------------------------------------------------------

func runWriteBatch(tasks []*writeTask) {
	db := getDb()
	for _, task := range tasks {
		func() {
			// PORT-NOTE: except Exception around task.op also covers panics so a
			// bad op cannot kill the writer goroutine.
			defer func() {
				if r := recover(); r != nil {
					task.err = fmt.Errorf("write op panic: %v", r)
				}
			}()
			if err := task.op(db); err != nil {
				task.err = err
			}
		}()
	}
	db.Commit()
	for _, task := range tasks {
		if task.done != nil {
			close(task.done)
		}
	}
}

// writeWorkerMain is the sobs-db-writer goroutine body. The queue and the
// liveness channel are captured as arguments (under writeWorkerLock in
// ensureWriteWorker) so shutdownDbResources can reset the globals safely.
func writeWorkerMain(queue chan *writeTask, threadDone chan struct{}) {
	defer close(threadDone)
	for {
		first := <-queue
		if first == writeStop {
			return
		}
		batch := []*writeTask{first}
		deadline := time.Now().Add(time.Duration(max(1, writeBatchWaitMs)) * time.Millisecond)
	collect:
		for len(batch) < max(1, writeBatchMax) {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			select {
			case queued := <-queue:
				if queued == writeStop {
					runWriteBatch(batch)
					return
				}
				batch = append(batch, queued)
			case <-time.After(remaining): // queue.Empty
				break collect
			}
		}
		runWriteBatch(batch)
	}
}

// ensureWriteWorker mirrors _ensure_write_worker.
// PORT-NOTE: Python's unlocked is_alive() fast path is dropped; the check
// always runs under writeWorkerLock (cheap, race-free).
func ensureWriteWorker() {
	writeWorkerLock.Lock()
	defer writeWorkerLock.Unlock()
	if writeQueue == nil {
		writeQueue = make(chan *writeTask, max(1, writeQueueMax))
	}
	alive := false
	if writeThread != nil {
		select {
		case <-writeThread: // closed → worker exited
		default:
			alive = true
		}
	}
	if !alive {
		writeThread = make(chan struct{})
		go writeWorkerMain(writeQueue, writeThread)
	}
}

// queueWrite mirrors _queue_write.
// PORT-NOTE: raise WriteQueueFullError / task.error → returned error.
func queueWrite(op func(*ChDbConnection) error, wait bool) error {
	ensureWriteWorker()
	var done chan struct{}
	if wait {
		done = make(chan struct{})
	}
	task := &writeTask{op: op, done: done}
	writeWorkerLock.Lock()
	queue := writeQueue
	writeWorkerLock.Unlock()
	if queue == nil {
		// assert _write_queue is not None
		panic("write queue is nil")
	}
	select { // _write_queue.put(task, timeout=1)
	case queue <- task:
	case <-time.After(time.Second):
		return &WriteQueueFullError{Message: "write queue is full"}
	}
	if done != nil {
		// Intentionally best-effort wait: embedded chDB runs in single-process mode
		// and sustained bursts can delay writer completion. We avoid surfacing a hard
		// timeout to clients here to prevent avoidable 5xx responses under backpressure.
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
		if task.err != nil {
			return task.err
		}
	}
	return nil
}

func writeQueueDepth() int {
	// PORT-NOTE: like Python's unsynchronized qsize() read.
	queue := writeQueue
	if queue == nil {
		return 0
	}
	return len(queue)
}

// _shutdown_db_resources (app.py 7484-7511) and the atexit registration are
// ported as shutdownDbResources in s02_db.go (shared state owner) — not
// redefined here.
