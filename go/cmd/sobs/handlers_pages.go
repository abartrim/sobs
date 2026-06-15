package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/render"
)

// autoRulePreviewSummary builds the all-zero auto_summary for a preview render (the empty-form
// default). _AUTO_RULE_CREATE_MAX = 200.
func autoRulePreviewSummary() *jsonenc.Object {
	return jsonenc.NewObject().
		Set("action", "preview").Set("hours", 24).Set("min_points", 30).
		Set("service_filter", "").Set("include_attr_fp", false).
		Set("mode", "threshold").Set("seasonal_strategy", "hour_of_day").
		Set("examined", 0).Set("existing", 0).Set("invalid", 0).Set("candidates", 0).
		Set("create_cap", 200).Set("capped", false).Set("created", 0)
}

// POST /settings/tags/auto — app.py auto_tag_rules (preview). The tag-candidate builder scans
// records in the last `hours`; the fixture's records predate now()-24h, so 0 are examined and
// the empty all-zero preview renders on settings_tags.html with the auto-tags panel + flash.
func (s *server) handleSettingsTagsAuto(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()

	rawAction := r.PostFormValue("action")
	if rawAction == "" {
		rawAction = "preview"
	}
	action := strings.ToLower(strings.TrimSpace(rawAction))

	hours := 24
	if raw := r.PostFormValue("hours"); raw != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			hours = v
			if hours < 1 {
				hours = 1
			} else if hours > 168 {
				hours = 168
			}
		}
	}
	minCount := 30
	if raw := r.PostFormValue("min_count"); raw != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			minCount = v
			if minCount < 1 {
				minCount = 1
			} else if minCount > 5000 {
				minCount = 5000
			}
		}
	}
	serviceFilter := strings.TrimSpace(r.PostFormValue("service_filter"))

	selected := []string{}
	for _, rt := range r.PostForm["auto_record_types"] {
		if v := strings.ToLower(strings.TrimSpace(rt)); v != "" {
			selected = append(selected, v)
		}
	}
	if len(selected) == 0 {
		selected = []string{"log", "trace", "error", "ai", "rum"}
	}

	rules := s.loadTagRulesCtx()
	services := s.listTagCandidateServices()
	candidates, stats := s.buildAutoTagRuleCandidates(hours, minCount, serviceFilter, selected)

	const createMax = 200
	summary := jsonenc.NewObject().
		Set("action", action).Set("hours", hours).Set("min_count", minCount).
		Set("service_filter", serviceFilter).Set("record_types", strsToAny(selected)).
		Set("examined", stats["examined"]).Set("existing", stats["existing"]).
		Set("invalid", stats["invalid"]).Set("candidates", len(candidates)).
		Set("create_cap", createMax).Set("capped", len(candidates) > createMax).Set("created", 0)

	if action == "create" {
		limited := candidates
		if len(limited) > createMax {
			limited = limited[:createMax]
		}
		rowsToInsert := []map[string]any{}
		base := fixedVersionMillis()
		for idx, cv := range limited {
			c := cv.(map[string]any)
			rts := []string{}
			for _, t := range c["record_types"].([]any) {
				rts = append(rts, fmt.Sprintf("%v", t))
			}
			condList := []any{map[string]any{
				"match_field": c["match_field"], "match_operator": c["match_operator"],
				"match_value": c["match_value"], "match_attr_key": c["match_attr_key"],
			}}
			condJSON, _ := json.Marshal(condList)
			rowsToInsert = append(rowsToInsert, map[string]any{
				"Id": newUUIDv4(), "Name": c["name"], "RecordTypes": strings.Join(rts, ","),
				"MatchField": c["match_field"], "MatchOperator": c["match_operator"],
				"MatchValue": c["match_value"], "MatchAttrKey": c["match_attr_key"],
				"TagKey": c["tag_key"], "TagValue": c["tag_value"], "ConditionsJson": string(condJSON),
				"IsDeleted": 0, "Version": base + int64(idx),
			})
		}
		if len(rowsToInsert) > 0 {
			if _, err := s.insertRowsNormalized("sobs_tag_rules", rowsToInsert); err != nil {
				s.dbError(w, err)
				return
			}
		}
		skippedByCap := len(candidates) - len(limited)
		capSuffix := "."
		if skippedByCap > 0 {
			capSuffix = fmt.Sprintf(", skipped %d by max cap (%d).", skippedByCap, createMax)
		}
		flashRedirect(w, "success", fmt.Sprintf(
			"Auto tag rule generation complete: created %d rule(s), skipped %d existing, %d invalid%s",
			len(rowsToInsert), stats["existing"], stats["invalid"], capSuffix),
			"/settings/tags?open_panel=auto-tags")
		return
	}

	s.renderPageFlash(w, "settings_tags.html", "auto_tag_rules", "info",
		fmt.Sprintf("Auto-tag preview: %d candidate(s), %d existing skipped, %d invalid.",
			len(candidates), stats["existing"], stats["invalid"]),
		map[string]any{
			"rules": rules, "edit_rule": nil,
			"record_types":    []any{"log", "trace", "error", "ai", "rum", "all"},
			"match_fields":    []any{"service_name", "severity", "body", "span_name", "event_type", "attribute"},
			"match_operators": []any{"eq", "contains", "regex"},
			"services":        services,
			"auto_preview":    candidates, "auto_summary": summary, "auto_open_panel": "auto-tags",
		})
}

func mapStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// POST /metrics/rules/dashboard/auto — app.py auto_metrics_rules_dashboard (preview): one chart
// candidate per anomaly rule (deduped title, sorted by service/source/signal/title), rendered on
// metrics_rules.html with the auto-dashboard panel open.
func (s *server) handleMetricsRulesDashboardAuto(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	rules := s.loadAnomalyRulesCtx()
	candidates := []any{}
	titleCounts := map[string]int{}
	for _, ri := range rules {
		rule, _ := ri.(map[string]any)
		source, signal := mapStr(rule, "source"), mapStr(rule, "signal")
		if source == "" || signal == "" {
			continue
		}
		name := mapStr(rule, "name")
		base := name
		if base == "" {
			base = source + "/" + signal
		}
		idx := titleCounts[base]
		titleCounts[base]++
		title := base
		if idx > 0 {
			title = base + " (" + strconv.Itoa(idx+1) + ")"
		}
		candidates = append(candidates, map[string]any{
			"title": title, "rule_name": name, "rule_type": mapStr(rule, "rule_type"),
			"source": source, "signal": signal, "service": mapStr(rule, "service"), "attr_fp": mapStr(rule, "attr_fp"),
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].(map[string]any), candidates[j].(map[string]any)
		for _, k := range []string{"service", "source", "signal", "title"} {
			if mapStr(a, k) != mapStr(b, k) {
				return mapStr(a, k) < mapStr(b, k)
			}
		}
		return false
	})
	services, signals, sources := s.listDerivedSignalDimensions()
	summary := jsonenc.NewObject().
		Set("action", "preview").Set("hours", 24).Set("service_filter", "").
		Set("max_charts", 12).Set("create_cap", 24).Set("dashboard_name", "Auto Metric Rules Dashboard").
		Set("rules_total", len(rules)).Set("candidates", len(candidates)).
		Set("capped", false).Set("created", 0).Set("existing", 0)
	s.renderPageFlash(w, "metrics_rules.html", "auto_metrics_rules_dashboard", "info",
		"Auto-dashboard preview: "+strconv.Itoa(len(candidates))+" candidate chart(s) from "+strconv.Itoa(len(rules))+" rule(s).",
		map[string]any{
			"rules": rules, "services": services, "signals": signals, "sources": sources,
			"auto_preview": []any{}, "auto_summary": nil,
			"auto_dashboard_preview": candidates, "auto_dashboard_summary": summary, "auto_open_panel": "auto-dashboard",
		})
}

// POST /metrics/rules/auto — app.py auto_metrics_rules (default "preview" action). The candidate
// builder examines derived-signal series with >= min_points in the last `hours`; the fixture's
// data is far older than now()-24h, so 0 series are examined and no candidates are proposed. The
// page renders with the info flash + the auto-rules panel open.
func (s *server) handleMetricsRulesAutoPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()

	rawAction := r.PostFormValue("action")
	if rawAction == "" {
		rawAction = "preview"
	}
	action := strings.ToLower(strings.TrimSpace(rawAction))

	hours := 24
	if raw := r.PostFormValue("hours"); raw != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			hours = v
			if hours < 1 {
				hours = 1
			} else if hours > 168 {
				hours = 168
			}
		}
	}
	minPoints := 30
	if raw := r.PostFormValue("min_points"); raw != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			minPoints = v
			if minPoints < 1 {
				minPoints = 1
			} else if minPoints > 5000 {
				minPoints = 5000
			}
		}
	}
	serviceFilter := strings.TrimSpace(r.PostFormValue("service_filter"))
	includeAttrFp := false
	switch r.PostFormValue("include_attr_fp") {
	case "1", "true", "on", "yes":
		includeAttrFp = true
	}
	mode := strings.ToLower(strings.TrimSpace(orDefault(r.PostFormValue("mode"), "threshold")))
	if mode != "threshold" && mode != "seasonal" {
		mode = "threshold"
	}
	seasonalStrategy := strings.ToLower(strings.TrimSpace(orDefault(r.PostFormValue("seasonal_strategy"), "hour_of_day")))
	if seasonalStrategy != "hour_of_day" && seasonalStrategy != "day_of_week" {
		seasonalStrategy = "hour_of_day"
	}

	services, signals, sources := s.listDerivedSignalDimensions()
	existingRules := s.loadAnomalyRulesCtx()

	var candidates []any
	var stats map[string]int
	if mode == "seasonal" {
		candidates, stats = s.buildSeasonalMetricRuleCandidates(hours, minPoints, serviceFilter, includeAttrFp, seasonalStrategy)
	} else {
		candidates, stats = s.buildAutoMetricRuleCandidates(hours, minPoints, serviceFilter, includeAttrFp)
	}

	const createMax = 200
	summary := jsonenc.NewObject().
		Set("action", action).Set("hours", hours).Set("min_points", minPoints).
		Set("service_filter", serviceFilter).Set("include_attr_fp", includeAttrFp).
		Set("mode", mode).Set("seasonal_strategy", seasonalStrategy).
		Set("examined", stats["examined"]).Set("existing", stats["existing"]).
		Set("invalid", stats["invalid"]).Set("candidates", len(candidates)).
		Set("create_cap", createMax).Set("capped", len(candidates) > createMax).Set("created", 0)

	if action == "create" {
		limited := candidates
		if len(limited) > createMax {
			limited = limited[:createMax]
		}
		rowsToInsert := []map[string]any{}
		base := fixedVersionMillis()
		for idx, cv := range limited {
			c := cv.(map[string]any)
			sbj := ""
			if v, ok := c["seasonal_buckets_json"].(string); ok {
				sbj = v
			}
			rowsToInsert = append(rowsToInsert, map[string]any{
				"Id": newUUIDv4(), "Name": c["name"], "RuleType": c["rule_type"],
				"SignalSource": c["source"], "SignalName": c["signal"],
				"ServiceName": c["service"], "AttrFingerprint": c["attr_fp"],
				"Comparator":       c["comparator"],
				"WarningThreshold": c["warning_threshold"], "CriticalThreshold": c["critical_threshold"],
				"SecondarySignalSource": "", "SecondarySignalName": "", "SecondaryComparator": "gt",
				"SecondaryWarningThreshold": 0.0, "SecondaryCriticalThreshold": 0.0,
				"MinSampleCount": c["min_sample_count"], "SeasonalBucketsJson": sbj,
				"IsDeleted": 0, "Version": base + int64(idx),
			})
		}
		if len(rowsToInsert) > 0 {
			if _, err := s.insertRowsNormalized("sobs_anomaly_rules", rowsToInsert); err != nil {
				s.dbError(w, err)
				return
			}
		}
		skippedByCap := len(candidates) - len(limited)
		capSuffix := "."
		if skippedByCap > 0 {
			capSuffix = fmt.Sprintf(", skipped %d by max cap (%d).", skippedByCap, createMax)
		}
		flashRedirect(w, "success", fmt.Sprintf(
			"Auto rule generation complete: created %d rule(s), skipped %d existing, %d invalid%s",
			len(rowsToInsert), stats["existing"], stats["invalid"], capSuffix),
			"/metrics/rules?open_panel=auto-rules")
		return
	}

	s.renderPageFlash(w, "metrics_rules.html", "auto_metrics_rules", "info",
		fmt.Sprintf("Auto-rule preview: %d candidate(s), %d existing skipped, %d invalid.",
			len(candidates), stats["existing"], stats["invalid"]),
		map[string]any{
			"rules": existingRules, "services": services, "signals": signals, "sources": sources,
			"auto_preview": candidates, "auto_summary": summary,
			"auto_dashboard_preview": []any{}, "auto_dashboard_summary": nil, "auto_open_panel": "auto-rules",
		})
}

// HTML page handlers. Pages render Jinja templates via the render engine; feature-gated
// pages return a plain 404 string when disabled (the fixture state for query/k8s pages).

// textStatus writes a plain text/html string body at the given status (Quart's behavior
// for a `(str, code)` handler return).
func textStatus(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// renderPage renders an HTML template with baseContext merged with extra context vars and
// writes it with Quart's text/html content type.
func (s *server) renderPage(w http.ResponseWriter, templateName, endpoint string, extra map[string]any) {
	ctx := s.baseContext(endpoint)
	for k, v := range extra {
		ctx[k] = v
	}
	s.renderInto(w, s.newEngine(), templateName, ctx)
}

// renderPageFlash renders a page with a single pre-seeded flash (category, message) consumed
// by get_flashed_messages — for handlers that flash() then render rather than redirect.
func (s *server) renderPageFlash(w http.ResponseWriter, templateName, endpoint, flashCategory, flashMessage string, extra map[string]any) {
	ctx := s.baseContext(endpoint)
	for k, v := range extra {
		ctx[k] = v
	}
	eng := s.newEngineFlash([]any{[]any{flashCategory, flashMessage}})
	// Consuming the flash empties the session, so Quart clears the session cookie and marks
	// the response Vary: Cookie (it read the request session).
	w.Header().Set("Set-Cookie", sessionCookieName+"=; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0"+sessionCookieAttrs())
	w.Header().Set("Vary", "Cookie")
	s.renderInto(w, eng, templateName, ctx)
}

func (s *server) renderInto(w http.ResponseWriter, eng *render.Engine, templateName string, ctx map[string]any) {
	out, err := eng.Render(templateName, ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body := []byte(out)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// GET /query — app.py view_query: 404 string when the query page is disabled (fixture).
func (s *server) handleViewQuery(w http.ResponseWriter, r *http.Request) {
	if !s.queryPageEnabled() {
		textStatus(w, http.StatusNotFound, "Query page is unavailable until AI and guard settings are configured.")
		return
	}
	s.renderPage(w, "query.html", "view_query", nil)
}

// GET /table-explorer — app.py view_table_explorer: 404 string when disabled (fixture).
func (s *server) handleViewTableExplorer(w http.ResponseWriter, r *http.Request) {
	if !s.queryPageEnabled() {
		textStatus(w, http.StatusNotFound, "Table Explorer is unavailable until AI and guard settings are configured.")
		return
	}
	s.renderPage(w, "table_explorer.html", "view_table_explorer", nil)
}

// GET /dashboards — app.py list_dashboards: render custom_dashboards.html with the
// non-deleted dashboards (_get_dashboards).
func (s *server) handleListDashboards(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// app.py create_dashboard: insert and redirect to the new dashboard's page (no flash).
		_ = r.ParseForm()
		name := strings.TrimSpace(r.PostFormValue("name"))
		if name == "" {
			flashRedirect(w, "warning", "Dashboard name is required", "/dashboards")
			return
		}
		id := newUUIDHex()
		row := map[string]any{
			"Id": id, "Name": name, "Description": strings.TrimSpace(r.PostFormValue("description")),
			"IsDeleted": 0, "Version": fixedVersionMillis(),
		}
		if _, err := s.db.InsertJSONEachRow("sobs_dashboards", []map[string]any{row}); err != nil {
			s.dbError(w, err)
			return
		}
		plainRedirect(w, "/dashboards/"+id)
		return
	}
	res, err := s.db.Execute(
		"SELECT Id, Name, Description FROM sobs_dashboards FINAL WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dashboards := []any{}
	for _, m := range rowMaps(res) {
		dashboards = append(dashboards, map[string]any{
			"id": cStr(m, "Id"), "name": cStr(m, "Name"), "description": cStr(m, "Description"),
		})
	}
	s.renderPage(w, "custom_dashboards.html", "list_dashboards", map[string]any{"dashboards": dashboards})
}

// GET /reports — app.py list_reports: render reports.html with all reports (_get_reports).
func (s *server) handleListReportsPage(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute("SELECT Id, Name, Description, PageType, FiltersJson " +
		"FROM sobs_reports FINAL WHERE IsDeleted = 0 ORDER BY PageType, Name")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reports := []any{}
	for _, m := range rowMaps(res) {
		reports = append(reports, map[string]any{
			"id": cStr(m, "Id"), "name": cStr(m, "Name"), "description": cStr(m, "Description"),
			"page_type": cStr(m, "PageType"), "filters": parseReportFiltersNative(cStr(m, "FiltersJson")),
		})
	}
	s.renderPage(w, "reports.html", "list_reports", map[string]any{"reports": reports})
}

// queryAllowedTables mirrors sorted(_QUERY_ALLOWED_TABLES) (app.py).
var queryAllowedTables = []any{
	"hyperdx_sessions", "otel_logs", "otel_metrics_1m_agg", "otel_metrics_gauge",
	"otel_metrics_gauge_pinned", "otel_metrics_histogram", "otel_metrics_histogram_pinned",
	"otel_metrics_sum", "otel_metrics_sum_pinned", "otel_traces", "sobs_anomaly_rules",
	"sobs_raw_windows", "v_derived_signals_1m", "v_derived_signals_anomaly",
	"v_otel_metrics_1m", "v_otel_metrics_anomaly", "v_otel_metrics_dedup",
	"v_otel_metrics_signal_context",
}

// GET /settings — app.py view_settings: the config hub (counts + flags).
func (s *server) handleViewSettings(w http.ResponseWriter, r *http.Request) {
	cnt := func(table string) int {
		return s.countRows("SELECT count() AS c FROM " + table + " FINAL WHERE IsDeleted=0")
	}
	aiURL, _ := s.appSetting("ai.endpoint_url")
	aiModel, _ := s.appSetting("ai.model")
	kEnabled, _ := s.appSetting("kubernetes.enabled")
	backup, _ := s.appSetting("data_management.backup_enabled")
	s.renderPage(w, "settings.html", "view_settings", map[string]any{
		"tag_rule_count":               cnt("sobs_tag_rules"),
		"anomaly_rule_count":           cnt("sobs_anomaly_rules"),
		"agent_rule_count":             cnt("sobs_agent_rules"),
		"ai_configured":                aiURL != "" && aiModel != "",
		"notification_channel_count":   cnt("sobs_notification_channels"),
		"notification_rule_count":      cnt("sobs_notification_rules"),
		"masking_custom_key_count":     len(s.loadJSONStringListSetting("masking.custom_keys")),
		"masking_custom_pattern_count": len(s.loadJSONStringListSetting("masking.custom_patterns")),
		"kubernetes_view_enabled":      kEnabled == "1",
		"backup_enabled":               backup == "1",
		"query_allowed_tables":         queryAllowedTables,
	})
}

// loadAnomalyRulesCtx mirrors _load_anomaly_rules (app.py): native maps for templates,
// with float thresholds (rendered Python-exact by the engine).
func (s *server) loadAnomalyRulesCtx() []any {
	res, err := s.db.Execute(
		"SELECT Id, Name, RuleType, SignalSource, SignalName, ServiceName, AttrFingerprint, Comparator, " +
			"WarningThreshold, CriticalThreshold, SecondarySignalSource, SecondarySignalName, " +
			"SecondaryComparator, SecondaryWarningThreshold, SecondaryCriticalThreshold, MinSampleCount, " +
			"SeasonalBucketsJson FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 ORDER BY Name")
	out := []any{}
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		ruleType := cStr(m, "RuleType")
		if ruleType == "" {
			ruleType = "threshold"
		}
		secCmp := cStr(m, "SecondaryComparator")
		if secCmp == "" {
			secCmp = "gt"
		}
		out = append(out, map[string]any{
			"id": cStr(m, "Id"), "name": cStr(m, "Name"), "rule_type": ruleType,
			"source": cStr(m, "SignalSource"), "signal": cStr(m, "SignalName"),
			"service": cStr(m, "ServiceName"), "attr_fp": cStr(m, "AttrFingerprint"),
			"comparator":                   cStr(m, "Comparator"),
			"warning_threshold":            cFloat(m, "WarningThreshold"),
			"critical_threshold":           cFloat(m, "CriticalThreshold"),
			"secondary_source":             cStr(m, "SecondarySignalSource"),
			"secondary_signal":             cStr(m, "SecondarySignalName"),
			"secondary_comparator":         secCmp,
			"secondary_warning_threshold":  cFloat(m, "SecondaryWarningThreshold"),
			"secondary_critical_threshold": cFloat(m, "SecondaryCriticalThreshold"),
			"min_sample_count":             cInt(m, "MinSampleCount"),
			"seasonal_buckets_json":        cStr(m, "SeasonalBucketsJson"),
		})
	}
	return out
}

// loadAgentRulesCtx mirrors app.py _load_agent_rules: active agent rules ordered by Name,
// shaped for settings_agents.html (actions split on commas, is_enabled as bool).
func (s *server) loadAgentRulesCtx() []any {
	res, err := s.db.Execute(
		"SELECT Id, Name, Description, TriggerType, TriggerRefId, TriggerState, " +
			"Actions, RateLimitMinutes, IsEnabled " +
			"FROM sobs_agent_rules FINAL WHERE IsDeleted=0 ORDER BY Name")
	out := []any{}
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		actions := []any{}
		for _, a := range strings.Split(cStr(m, "Actions"), ",") {
			if a = strings.TrimSpace(a); a != "" {
				actions = append(actions, a)
			}
		}
		out = append(out, map[string]any{
			"id": cStr(m, "Id"), "name": cStr(m, "Name"), "description": cStr(m, "Description"),
			"trigger_type": cStr(m, "TriggerType"), "trigger_ref_id": cStr(m, "TriggerRefId"),
			"trigger_state": cStr(m, "TriggerState"), "actions": actions,
			"rate_limit_minutes": cInt(m, "RateLimitMinutes"), "is_enabled": cInt(m, "IsEnabled") != 0,
		})
	}
	return out
}

// parseTagRuleConditions mirrors app.py _parse_tag_rule_conditions_json: best-effort decode of
// the ConditionsJson array into normalized {match_field, match_operator, match_value,
// match_attr_key} string maps (non-list/parse-failure -> empty).
func parseTagRuleConditions(raw string) []any {
	out := []any{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	v, err := parseJSONValue([]byte(raw))
	if err != nil {
		return out
	}
	list, ok := v.([]any)
	if !ok {
		return out
	}
	for _, it := range list {
		o, ok := it.(*jsonenc.Object)
		if !ok {
			continue
		}
		gs := func(k string) string {
			if val, has := o.Get(k); has {
				if str, ok := val.(string); ok {
					return str
				}
			}
			return ""
		}
		out = append(out, map[string]any{
			"match_field": gs("match_field"), "match_operator": gs("match_operator"),
			"match_value": gs("match_value"), "match_attr_key": gs("match_attr_key"),
		})
	}
	return out
}

// loadTagRulesCtx mirrors app.py _load_tag_rules: active tag rules ordered by Name, with the
// ConditionsJson decoded (and the pre-ConditionsJson MatchField backward-compat fallback).
func (s *server) loadTagRulesCtx() []any {
	res, err := s.db.Execute(
		"SELECT Id, Name, RecordTypes, MatchField, MatchOperator, MatchValue, " +
			"MatchAttrKey, TagKey, TagValue, ConditionsJson " +
			"FROM sobs_tag_rules FINAL WHERE IsDeleted = 0 ORDER BY Name")
	out := []any{}
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		conditions := parseTagRuleConditions(cStr(m, "ConditionsJson"))
		if len(conditions) == 0 && strings.TrimSpace(cStr(m, "MatchField")) != "" {
			op := cStr(m, "MatchOperator")
			if op == "" {
				op = "eq"
			}
			conditions = []any{map[string]any{
				"match_field": cStr(m, "MatchField"), "match_operator": op,
				"match_value": cStr(m, "MatchValue"), "match_attr_key": cStr(m, "MatchAttrKey"),
			}}
		}
		recordTypes := []any{}
		for _, t := range strings.Split(cStr(m, "RecordTypes"), ",") {
			if t = strings.TrimSpace(t); t != "" {
				recordTypes = append(recordTypes, t)
			}
		}
		out = append(out, map[string]any{
			"id": cStr(m, "Id"), "name": cStr(m, "Name"), "record_types": recordTypes,
			"match_field": cStr(m, "MatchField"), "match_operator": cStr(m, "MatchOperator"),
			"match_value": cStr(m, "MatchValue"), "match_attr_key": cStr(m, "MatchAttrKey"),
			"tag_key": cStr(m, "TagKey"), "tag_value": cStr(m, "TagValue"), "conditions": conditions,
		})
	}
	return out
}

// listDerivedSignalDimensions mirrors _list_derived_signal_dimensions.
func (s *server) listDerivedSignalDimensions() (services, signals, sources []any) {
	services = []any{}
	res, err := s.db.Execute(
		"SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName != ''" +
			" UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName != ''" +
			" UNION DISTINCT SELECT DISTINCT ServiceName FROM hyperdx_sessions WHERE ServiceName != ''" +
			" ORDER BY ServiceName")
	if err == nil {
		for _, m := range rowMaps(res) {
			services = append(services, cStr(m, "ServiceName"))
		}
	}
	sig := []string{"log_volume", "error_volume", "error_ratio", "trace_volume", "trace_error_ratio",
		"latency_p95_ms", "exception_volume", "LCP", "FID", "CLS", "INP", "TTFB", "FCP"}
	sort.Strings(sig)
	signals = strsToAny(sig)
	sources = []any{"errors", "logs", "rum_vitals", "traces"}
	return
}

// GET /metrics/rules — app.py view_metrics_rules.
func (s *server) handleViewMetricsRules(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.createMetricsRule(w, r)
		return
	}
	openPanel := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("open_panel")))
	if openPanel != "auto-rules" && openPanel != "auto-dashboard" {
		openPanel = ""
	}
	services, signals, sources := s.listDerivedSignalDimensions()
	s.renderPage(w, "metrics_rules.html", "view_metrics_rules", map[string]any{
		"rules":                  s.loadAnomalyRulesCtx(),
		"services":               services,
		"signals":                signals,
		"sources":                sources,
		"auto_preview":           []any{},
		"auto_summary":           nil,
		"auto_dashboard_preview": []any{},
		"auto_dashboard_summary": nil,
		"auto_open_panel":        openPanel,
	})
}

// pyFloat mirrors Python float(str): strips surrounding whitespace, accepts int/float/exponent
// forms. Returns ok=false on parse failure (Python would raise ValueError).
func pyFloat(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}

// createMetricsRule mirrors app.py create_metrics_rule (POST /metrics/rules): validate the
// form, insert one sobs_anomaly_rules row, flash success. Validation flash messages and order
// match app.py exactly so the warning branches stay byte-identical.
func (s *server) createMetricsRule(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	loc := "/metrics/rules"
	g := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	name, source, signal := g("name"), g("source"), g("signal")
	ruleType := strings.ToLower(orDefault(g("rule_type"), "threshold"))
	service, attrFp := g("service"), g("attr_fp")
	comparator := strings.ToLower(orDefault(g("comparator"), "gt"))
	secondarySource, secondarySignal := g("secondary_source"), g("secondary_signal")
	secondaryComparator := strings.ToLower(orDefault(g("secondary_comparator"), "gt"))

	if name == "" || source == "" || signal == "" {
		flashRedirect(w, "warning", "Rule name, source, and signal are required", loc)
		return
	}
	if ruleType != "threshold" && ruleType != "composite" {
		flashRedirect(w, "warning", "Rule type must be 'threshold' or 'composite'", loc)
		return
	}
	if comparator != "gt" && comparator != "lt" {
		flashRedirect(w, "warning", "Comparator must be 'gt' or 'lt'", loc)
		return
	}
	if secondaryComparator != "gt" && secondaryComparator != "lt" {
		flashRedirect(w, "warning", "Secondary comparator must be 'gt' or 'lt'", loc)
		return
	}
	// Python: float(... or "") -> empty thresholds raise; min_sample_count int(... or 1).
	warnTh, ok1 := pyFloat(r.PostFormValue("warning_threshold"))
	critTh, ok2 := pyFloat(r.PostFormValue("critical_threshold"))
	minSample := 1
	msRaw := strings.TrimSpace(r.PostFormValue("min_sample_count"))
	ok3 := true
	if msRaw != "" {
		if v, err := strconv.Atoi(msRaw); err == nil {
			minSample = v
		} else {
			ok3 = false
		}
	}
	if minSample < 1 {
		minSample = 1
	}
	secWarn, ok4 := 0.0, true
	if v := strings.TrimSpace(r.PostFormValue("secondary_warning_threshold")); v != "" {
		secWarn, ok4 = pyFloat(v)
	}
	secCrit, ok5 := 0.0, true
	if v := strings.TrimSpace(r.PostFormValue("secondary_critical_threshold")); v != "" {
		secCrit, ok5 = pyFloat(v)
	}
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		flashRedirect(w, "warning", "Thresholds must be numeric and sample count must be an integer", loc)
		return
	}
	if comparator == "gt" && critTh < warnTh {
		flashRedirect(w, "warning", "For 'gt' rules, critical threshold must be >= warning threshold", loc)
		return
	}
	if comparator == "lt" && critTh > warnTh {
		flashRedirect(w, "warning", "For 'lt' rules, critical threshold must be <= warning threshold", loc)
		return
	}
	if ruleType == "composite" {
		if secondarySource == "" || secondarySignal == "" {
			flashRedirect(w, "warning", "Composite rules require a secondary source and signal", loc)
			return
		}
		if secondaryComparator == "gt" && secCrit < secWarn {
			flashRedirect(w, "warning", "For secondary 'gt' rules, critical threshold must be >= warning threshold", loc)
			return
		}
		if secondaryComparator == "lt" && secCrit > secWarn {
			flashRedirect(w, "warning", "For secondary 'lt' rules, critical threshold must be <= warning threshold", loc)
			return
		}
	} else {
		secondarySource, secondarySignal, secondaryComparator = "", "", "gt"
		secWarn, secCrit = 0.0, 0.0
	}
	row := map[string]any{
		"Id": newUUIDv4(), "Name": name, "RuleType": ruleType,
		"SignalSource": source, "SignalName": signal, "ServiceName": service, "AttrFingerprint": attrFp,
		"Comparator": comparator, "WarningThreshold": warnTh, "CriticalThreshold": critTh,
		"SecondarySignalSource": secondarySource, "SecondarySignalName": secondarySignal,
		"SecondaryComparator": secondaryComparator, "SecondaryWarningThreshold": secWarn,
		"SecondaryCriticalThreshold": secCrit, "MinSampleCount": minSample,
		"IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_anomaly_rules", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	flashRedirect(w, "success", "Rule '"+name+"' created", loc)
}

// GET /settings/notifications — app.py view_notifications. Channels/rules/log empty on the
// fixture; metric_rules = the 4 seeded anomaly rules; the rest are constants. VAPID is
// unconfigured -> (nil, nil). signal_sources is an ORDERED object (tojson'd in template).
func (s *server) handleViewNotifications(w http.ResponseWriter, r *http.Request) {
	signalSources := jsonenc.NewObject().
		Set("logs", []any{"log_volume", "error_volume", "error_ratio"}).
		Set("traces", []any{"trace_volume", "trace_error_ratio", "latency_p95_ms"}).
		Set("errors", []any{"exception_volume"})
	s.renderPage(w, "settings_notifications.html", "view_notifications", map[string]any{
		"channels":            []any{},
		"rules":               []any{},
		"notification_log":    []any{},
		"channel_types":       []any{"webhook", "slack", "email", "browser_push"},
		"comparators":         []any{"gt", "lt", "gte", "lte", "eq"},
		"condition_types":     []any{"signal", "tag"},
		"severities":          []any{"warning", "critical"},
		"logic_operators":     []any{"any", "all"},
		"signal_sources":      signalSources,
		"tag_match_operators": []any{"eq", "contains", "regex"},
		"tag_record_types":    []any{"all", "log", "trace", "error", "ai", "rum"},
		"edit_rule":           nil,
		"vapid_public_key":    nil,
		"vapid_key_source":    nil,
		"metric_rules":        s.loadAnomalyRulesCtx(),
	})
}

// activePartRows mirrors _active_part_rows: row count of a table's active parts.
func (s *server) activePartRows(table string) int {
	return s.countRows("SELECT COALESCE(sum(rows), 0) AS c FROM system.parts " +
		"WHERE active = 1 AND database = currentDatabase() AND table = '" + table + "'")
}

// GET /enrichment/cve — app.py view_enrichment_cve. No CVE data on the fixture.
func (s *server) handleViewEnrichmentCve(w http.ResponseWriter, r *http.Request) {
	cveLastScan, _ := s.appSetting("enrichment.cve_last_scan")
	maxRel := 300
	if raw, ok := s.appSetting("enrichment.github_backfill_max_releases"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			if n < 1 {
				n = 1
			} else if n > 2000 {
				n = 2000
			}
			maxRel = n
		}
	}
	s.renderPage(w, "cve.html", "view_enrichment_cve", map[string]any{
		"cve_enabled":                  s.appSettingBool("enrichment.cve_enabled", true),
		"cve_last_scan":                cveLastScan,
		"github_backfill_max_releases": maxRel,
		"cve_last_backfill_attempted":  0, "cve_last_backfill_inserted": 0, "cve_last_backfill_cap": 0,
		"cve_findings": []any{}, "ecosystems": []any{}, "severities": []any{},
		"severity_filter": "", "ecosystem_filter": "",
		"selected_severities": []any{}, "selected_ecosystems": []any{},
		"package_filter": "", "show_all": false,
	})
}

// GET /work-items — app.py view_work_items. Empty work items on the fixture.
func (s *server) handleViewWorkItemsPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "work_items.html", "view_work_items", map[string]any{
		"items": []any{}, "total_items": 0, "services": []any{}, "rules": []any{},
		"service_filter": "", "rule_filter": "", "action_type_filter": "", "status_filter": "",
		"from_ts": "", "to_ts": "", "time_error": "",
	})
}

// GET /ai — app.py view_ai. Empty AI traces on the fixture.
func (s *server) handleViewAi(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "ai.html", "view_ai", map[string]any{
		"ai_items": []any{}, "total": 0, "limit": 50, "offset": 0,
		"service": "", "selected_services": []any{}, "model": "", "selected_models": []any{},
		"operation": "", "selected_operations": []any{}, "span_name": "", "selected_span_names": []any{},
		"row_type": "", "selected_row_types": []any{}, "sql_where": "", "view_mode": "flat",
		"services": []any{}, "models": []any{}, "operations": []any{}, "span_names": []any{},
		"trace_groups":    []any{},
		"total_tokens_in": 0, "total_tokens_out": 0, "total_calls": 0, "total_errors": 0,
		"error_msg": "", "sort_by": "Timestamp", "sort_dir": "desc", "from_ts": "", "to_ts": "",
		"ai_pricing_json":         mustParseJSON(savedAiPricingJSON),
		"ai_pricing_sources_json": mustParseJSON(aiPricingSourcesJSON),
	})
}

// mustParseJSON parses an embedded JSON asset (order-preserving), returning an empty
// object on error.
func mustParseJSON(raw []byte) any {
	v, err := parseJSONValue(raw)
	if err != nil {
		return jsonenc.NewObject()
	}
	return v
}

// GET /incident — app.py view_incident. No incident reference on a param-less request.
func (s *server) handleViewIncident(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "incident.html", "view_incident", map[string]any{
		"trace_id": "", "error_id": "", "rum_session": "", "rum_ts": "",
		"primary_error": nil, "primary_trace": nil, "primary_rum": nil,
		"service": "", "from_ts": "", "to_ts": "", "window_minutes": 30,
		"related_errors": []any{}, "related_log_count": 0, "related_span_count": 0,
		"related_rum_count": 0, "related_rum_sessions": 0, "related_rum_error_count": 0,
		"related_rum_events": []any{}, "raw_windows": []any{},
		"metrics_context": map[string]any{
			"source_mode": "none", "total_points": 0, "series": []any{},
			"match_mode": "none", "match_label": "no match", "match_dimensions": []any{},
		},
		"anomaly_state": nil, "work_item_links": map[string]any{}, "time_error": "",
		"error_msg": "No incident reference provided. Specify trace_id, error_id, or rum_session.",
	})
}

// GET /metrics/anomaly — app.py view_metrics_anomaly. Empty derived signals.
func (s *server) handleViewMetricsAnomaly(w http.ResponseWriter, r *http.Request) {
	services, signals, sources := s.listDerivedSignalDimensions()
	s.renderPage(w, "metrics_anomaly.html", "view_metrics_anomaly", map[string]any{
		"rows": []any{}, "total": 0, "service": "", "metric": "", "signal": "", "source": "",
		"attr_fp": "", "from_ts": "", "to_ts": "", "hours": 24, "error_msg": "",
		"point_state": "", "point_score": "", "related_target": "",
		"services": services, "signals": signals, "sources": sources,
	})
}

// GET /logs — app.py view_logs. Faithful port: q (RE2 regex) / level[] / service[] /
// event_name[] / trace_id(s) / sql_where / time-window filtering, sort/limit/offset, the
// otel_logs row list + total, level/service/tag stats, optional advanced analysis, and the
// stats-snapshot timestamps. On the empty fixture every list/stat is empty (the .items()
// stats use ordered Objects that render identically to the prior empty map), so byte-parity
// with the golden empty render is preserved. (/logs is excluded from byte-compare for
// ORDER BY tie order, but this still matches Python exactly.)
func (s *server) handleViewLogs(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var selectedLevels []string
	for _, v := range queryGetList(r, "level") {
		if t := strings.TrimSpace(v); t != "" {
			selectedLevels = append(selectedLevels, strings.ToUpper(t))
		}
	}
	var selectedServices []string
	for _, v := range queryGetList(r, "service") {
		if t := strings.TrimSpace(v); t != "" {
			selectedServices = append(selectedServices, t)
		}
	}
	traceID := strings.TrimSpace(r.URL.Query().Get("trace_id"))
	traceIDs, traceID := parseTraceFilterValues(traceID, queryGetList(r, "trace_ids"))
	traceIDsCSV := strings.Join(traceIDs, ",")
	traceIDsCount := len(traceIDs)
	var selectedEventNames []string
	for _, v := range queryGetList(r, "event_name") {
		if t := strings.TrimSpace(v); t != "" {
			selectedEventNames = append(selectedEventNames, t)
		}
	}
	eventName := "" // backward compatibility; use selected_event_names for filtering
	fromTs, toTs, timeError := parseTimeWindowArgs(r)
	sqlWhere := strings.TrimSpace(r.URL.Query().Get("sql"))
	runAdvancedAnalysis := strings.TrimSpace(r.URL.Query().Get("analyze")) == "1"
	limit := parseLimitArg(r, 200)
	offset := parseOffsetArg(r)
	sortBy, sortCol, sortDir := parseSortArg(r, map[string]string{
		"Timestamp": "Timestamp", "SeverityText": "SeverityText", "ServiceName": "ServiceName",
	}, "Timestamp")
	orderDir := "DESC"
	if sortDir == "asc" {
		orderDir = "ASC"
	}
	orderClause := "ORDER BY " + sortCol + " " + orderDir

	var rows []map[string]any
	logRows := []any{}
	total := 0
	errorMsg := ""
	var levelStats, serviceStats *jsonenc.Object
	levelStats = jsonenc.NewObject()
	serviceStats = jsonenc.NewObject()
	var advancedAnalysis any = nil
	statsGeneratedAtISO := ""
	statsGeneratedAtDisplay := ""
	statsGeneratedAgeS := 0
	where := ""
	var params []any
	var includePatterns, excludePatterns []string

	if timeError != "" {
		errorMsg = timeError
	}
	if q != "" {
		inc, exc, regexErr := s.prepareRE2FilterPatterns(q)
		includePatterns, excludePatterns = inc, exc
		if regexErr != "" {
			errorMsg = regexErr
		}
	}

	if errorMsg != "" {
		// keep error
	} else if sqlWhere != "" {
		if vErr := validateUserSQLWhere(sqlWhere); vErr != "" {
			errorMsg = "SQL error: " + vErr
		} else {
			safeSQL := translateLogsSQLWhere(sqlWhere)
			where = "WHERE " + safeSQL
			timeConds, timeParams := timeWindowConditions("Timestamp", fromTs, toTs)
			if len(timeConds) > 0 {
				where = where + " AND " + strings.Join(timeConds, " AND ")
				params = append(params, timeParams...)
			}
		}
	} else {
		var conditions []string
		params = nil
		if len(selectedLevels) > 0 {
			conditions = append(conditions, "SeverityText IN ("+placeholders(len(selectedLevels))+")")
			for _, v := range selectedLevels {
				params = append(params, v)
			}
		}
		if len(selectedServices) > 0 {
			conditions = append(conditions, "ServiceName IN ("+placeholders(len(selectedServices))+")")
			for _, v := range selectedServices {
				params = append(params, v)
			}
		}
		if len(selectedEventNames) > 0 {
			conditions = append(conditions, "EventName IN ("+placeholders(len(selectedEventNames))+")")
			for _, v := range selectedEventNames {
				params = append(params, v)
			}
		}
		if len(traceIDs) > 0 {
			conditions = append(conditions, "lower(TraceId) IN ("+placeholders(len(traceIDs))+")")
			for _, v := range traceIDs {
				params = append(params, v)
			}
		} else if traceID != "" {
			conditions = append(conditions, "lower(TraceId)=?")
			params = append(params, strings.ToLower(traceID))
		}
		appendTimeWindowFilter(&conditions, &params, "Timestamp", fromTs, toTs)
		where = whereClauseSQL(conditions)
	}

	if errorMsg == "" {
		queryWhere := where
		queryParams := append([]any{}, params...)
		if q != "" {
			var regexConditions []string
			appendRegexExpressionClauses(&regexConditions, &queryParams, "Body", includePatterns, excludePatterns)
			if len(regexConditions) > 0 {
				regexSQL := strings.Join(regexConditions, " AND ")
				if queryWhere != "" {
					queryWhere = queryWhere + " AND " + regexSQL
				} else {
					queryWhere = "WHERE " + regexSQL
				}
			}
		}

		selectBase := "SELECT Timestamp, SeverityText, ServiceName, Body, TraceId, SpanId FROM otel_logs " + queryWhere + " "

		var queryErr error
		if queryWhere == "" {
			total = s.activePartRows("otel_logs")
		} else {
			if res, err := s.db.Execute("SELECT COUNT(*) AS c FROM otel_logs "+queryWhere, queryParams...); err == nil {
				total = cInt(rowMaps(res)[0], "c")
			} else {
				queryErr = err
			}
		}
		if queryErr == nil {
			if res, err := s.db.Execute(selectBase+orderClause+" LIMIT ? OFFSET ?", append(append([]any{}, queryParams...), limit, offset)...); err == nil {
				rows = rowMaps(res)
			} else {
				queryErr = err
			}
		}
		if queryErr == nil {
			levelStats, serviceStats = s.computeLogStats(queryWhere, queryParams)
			if runAdvancedAnalysis {
				if res, err := s.db.Execute("SELECT SeverityText, ServiceName, Body, LogAttributes FROM otel_logs "+queryWhere, queryParams...); err == nil {
					advancedAnalysis = computeAdvancedLogAnalysis(rowMaps(res), levelStats, serviceStats)
				} else {
					queryErr = err
				}
			}
		}
		if queryErr == nil {
			generatedAt := nowUTC()
			snapshotAt := generatedAt
			if res, err := s.db.Execute("SELECT max(Timestamp) AS m FROM otel_logs "+queryWhere, queryParams...); err == nil {
				m := rowMaps(res)[0]
				if raw := m["m"]; raw != nil {
					if rawStr := cStr(m, "m"); rawStr != "" {
						if parsed, ok := parseISOTime(strings.ReplaceAll(rawStr, "Z", "+00:00")); ok {
							snapshotAt = parsed.UTC()
						}
					}
				}
			}
			statsGeneratedAtISO = pyISOFormatUTC(snapshotAt)
			statsGeneratedAtDisplay = snapshotAt.UTC().Format("2006-01-02 15:04:05") + " UTC"
			age := int(generatedAt.Sub(snapshotAt).Seconds())
			if age < 0 {
				age = 0
			}
			statsGeneratedAgeS = age
		}
		if queryErr != nil {
			if sqlWhere != "" {
				errorMsg = "SQL error: " + publicDashboardQueryError(queryErr)
			} else {
				errorMsg = "Query error: " + queryErr.Error()
			}
			rows = nil
			total = 0
			levelStats = jsonenc.NewObject()
			serviceStats = jsonenc.NewObject()
			advancedAnalysis = nil
		}
	}

	// Record IDs for visible rows so tags can be batch-fetched.
	var rowRecordIDs []string
	for _, rmap := range rows {
		rowRecordIDs = append(rowRecordIDs, recordIDForLog(
			cStr(rmap, "Timestamp"), cStr(rmap, "ServiceName"), cStr(rmap, "TraceId"), cStr(rmap, "SpanId")))
	}
	tagsByRecordID := map[string][]any{}
	type tagStatKey struct{ k, v string }
	tagStatsCount := map[tagStatKey]int{}
	var tagStatsOrder []tagStatKey
	if len(rowRecordIDs) > 0 {
		idParams := make([]any, len(rowRecordIDs))
		for i, id := range rowRecordIDs {
			idParams[i] = id
		}
		tagQuery := "SELECT RecordId, TagKey, TagValue, IsAuto FROM sobs_record_tags FINAL " +
			"WHERE RecordType='log' AND RecordId IN (" + placeholders(len(rowRecordIDs)) + ") AND IsDeleted=0 " +
			"ORDER BY RecordId, TagKey"
		if res, err := s.db.Execute(tagQuery, idParams...); err == nil {
			for _, tr := range rowMaps(res) {
				rid := cStr(tr, "RecordId")
				entry := map[string]any{
					"key": cStr(tr, "TagKey"), "value": cStr(tr, "TagValue"), "is_auto": cBool(tr, "IsAuto"),
				}
				tagsByRecordID[rid] = append(tagsByRecordID[rid], entry)
				sk := tagStatKey{cStr(tr, "TagKey"), cStr(tr, "TagValue")}
				if _, seen := tagStatsCount[sk]; !seen {
					tagStatsOrder = append(tagStatsOrder, sk)
				}
				tagStatsCount[sk]++
			}
		}
	}
	// tag_stats sorted by (-count, key, value).
	sortedKeys := append([]tagStatKey{}, tagStatsOrder...)
	sort.SliceStable(sortedKeys, func(i, j int) bool {
		ci, cj := tagStatsCount[sortedKeys[i]], tagStatsCount[sortedKeys[j]]
		if ci != cj {
			return ci > cj
		}
		if sortedKeys[i].k != sortedKeys[j].k {
			return sortedKeys[i].k < sortedKeys[j].k
		}
		return sortedKeys[i].v < sortedKeys[j].v
	})
	tagStats := []any{}
	for _, sk := range sortedKeys {
		tagStats = append(tagStats, map[string]any{"key": sk.k, "value": sk.v, "count": tagStatsCount[sk]})
	}

	for _, rmap := range rows {
		rid := recordIDForLog(cStr(rmap, "Timestamp"), cStr(rmap, "ServiceName"), cStr(rmap, "TraceId"), cStr(rmap, "SpanId"))
		tags := tagsByRecordID[rid]
		if tags == nil {
			tags = []any{}
		}
		logRows = append(logRows, map[string]any{
			"ts":        cStr(rmap, "Timestamp"),
			"level":     rmap["SeverityText"],
			"service":   rmap["ServiceName"],
			"body":      rmap["Body"],
			"trace_id":  rmap["TraceId"],
			"span_id":   rmap["SpanId"],
			"record_id": rid,
			"tags":      tags,
		})
	}

	services := s.distinctStrings("SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName!='' ORDER BY ServiceName")
	levels := s.distinctStrings("SELECT DISTINCT SeverityText FROM otel_logs ORDER BY SeverityText")
	eventNames := s.distinctStrings("SELECT DISTINCT EventName FROM otel_logs WHERE EventName!='' ORDER BY EventName")

	s.renderPage(w, "logs.html", "view_logs", map[string]any{
		"logs": logRows, "total": total, "limit": limit, "offset": offset, "q": q,
		"level": "", "selected_levels": toAnySlice(selectedLevels),
		"service": "", "selected_services": toAnySlice(selectedServices),
		"trace_id": traceID, "trace_ids_csv": traceIDsCSV, "trace_ids_count": traceIDsCount,
		"sql_where": sqlWhere, "from_ts": fromTs, "to_ts": toTs,
		"services": services, "levels": levels, "event_names": eventNames,
		"event_name": eventName, "selected_event_names": toAnySlice(selectedEventNames),
		"error_msg": errorMsg, "sort_by": sortBy, "sort_dir": sortDir,
		"run_advanced_analysis": runAdvancedAnalysis,
		"level_stats":           levelStats, "service_stats": serviceStats, "tag_stats": tagStats,
		"advanced_analysis":      advancedAnalysis,
		"stats_generated_at_iso": statsGeneratedAtISO, "stats_generated_at_display": statsGeneratedAtDisplay,
		"stats_generated_age_s": statsGeneratedAgeS,
	})
}

// GET /metrics — app.py view_metrics. Empty derived signals on the fixture.
func (s *server) handleViewMetrics(w http.ResponseWriter, r *http.Request) {
	services, signals, sources := s.listDerivedSignalDimensions()
	s.renderPage(w, "metrics.html", "view_metrics", map[string]any{
		"rows": []any{}, "total": 0, "limit": 100, "offset": 0,
		"service": "", "selected_services": []any{}, "signal": "", "selected_signals": []any{},
		"source": "", "selected_sources": []any{}, "attr_fp": "", "q": "",
		"from_ts": "", "to_ts": "", "hours": 24, "error_msg": "",
		"services": services, "signals": signals, "sources": sources,
		"sort_by": "last_time", "sort_dir": "desc",
	})
}

// GET /traces — app.py view_traces. Empty otel_traces on the fixture.
func (s *server) handleViewTraces(w http.ResponseWriter, r *http.Request) {
	services := s.distinctStrings("SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName != '' ORDER BY ServiceName")
	s.renderPage(w, "traces.html", "view_traces", map[string]any{
		"spans": []any{}, "total": 0, "limit": 100, "offset": 0,
		"service": "", "selected_services": []any{}, "trace_id": "",
		"from_ts": "", "to_ts": "", "error_msg": "", "q": "", "services": services,
		"sort_by": "Timestamp", "sort_dir": "desc", "trace_detail": nil,
		"work_item_links": map[string]any{},
	})
}

// GET /errors — app.py view_errors. Faithful port of the ERROR_SOURCES (otel_logs ∪
// hyperdx_sessions) error feed: service[]/q(RE2)/resolved/time-window/sort filters, the
// grouped (best-effort dedup) and non-grouped (resolved SQL + hydrate, or simple) paths,
// resolution flags, error rows + total, and work_item_links. On the empty fixture every list
// is empty and work_item_links is an empty ordered Object (renders like the prior empty map),
// so byte-parity with the golden empty render holds.
func (s *server) handleViewErrors(w http.ResponseWriter, r *http.Request) {
	errorIDSQL := errorIDExpr
	const groupedTraceChunkSize = 200
	const hydrateKeyChunkSize = 200

	var selectedServices []string
	for _, v := range queryGetList(r, "service") {
		if t := strings.TrimSpace(v); t != "" {
			selectedServices = append(selectedServices, t)
		}
	}
	service := ""
	if len(selectedServices) > 0 {
		service = selectedServices[0]
	}
	groupBy := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_by")))
	groupedMode := strings.TrimSpace(r.URL.Query().Get("grouped")) == "1" ||
		groupBy == "group" || groupBy == "message" || groupBy == "fingerprint" || groupBy == "signature"
	fromTs, toTs, timeError := parseTimeWindowArgs(r)
	// request.args.get("resolved", "0"): default "0" only when the key is ABSENT; a
	// present-but-empty value (?resolved=) yields "" after strip.
	resolvedRaw := "0"
	if vals, ok := r.URL.Query()["resolved"]; ok {
		if len(vals) > 0 {
			resolvedRaw = vals[0]
		} else {
			resolvedRaw = ""
		}
	}
	resolved := strings.TrimSpace(resolvedRaw)
	limit := parseLimitArg(r, 100)
	offset := parseOffsetArg(r)

	var sortBy, sortCol, sortDir string
	if groupedMode {
		sortBy, sortCol, sortDir = parseSortArg(r, map[string]string{
			"count": "Count", "last_seen": "LastSeen", "ServiceName": "RepServiceName", "Timestamp": "LastSeen",
		}, "count")
	} else {
		sortBy, sortCol, sortDir = parseSortArg(r, map[string]string{
			"Timestamp": "Timestamp", "ServiceName": "ServiceName",
		}, "Timestamp")
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var includePatterns, excludePatterns []string
	errorMsg := timeError
	if q != "" && errorMsg == "" {
		inc, exc, regexErr := s.prepareRE2FilterPatterns(q)
		includePatterns, excludePatterns = inc, exc
		if regexErr != "" {
			errorMsg = regexErr
		}
	}
	var resolvedIDs map[string]bool
	if resolved != "0" && resolved != "1" {
		resolvedIDs = s.getResolvedErrorIDs()
	}

	var whereParts []string
	var whereParams []any
	if len(selectedServices) > 0 {
		whereParts = append(whereParts, "ServiceName IN ("+placeholders(len(selectedServices))+")")
		for _, v := range selectedServices {
			whereParams = append(whereParams, v)
		}
	}
	appendTimeWindowFilter(&whereParts, &whereParams, "Timestamp", fromTs, toTs)
	if q != "" && errorMsg == "" {
		appendRegexExpressionClauses(&whereParts, &whereParams, "Body", includePatterns, excludePatterns)
	}
	whereSQL := whereClauseSQL(whereParts)

	errors := []any{}
	total := 0

	if groupedMode {
		probeLimit := limit * 100
		if probeLimit < 2000 {
			probeLimit = 2000
		}
		if probeLimit > 10000 {
			probeLimit = 10000
		}
		groupedWhereSQL := whereSQL
		if resolved == "1" {
			cond := errorIDSQL + " IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)"
			groupedWhereSQL = combineWhere(groupedWhereSQL, cond)
		} else if resolved == "0" {
			cond := errorIDSQL + " NOT IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)"
			groupedWhereSQL = combineWhere(groupedWhereSQL, cond)
		}

		groupedProbeSQL := "SELECT " +
			"Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes, " +
			"substring(replaceRegexpAll(lower(ServiceName), '\\s+', ' '), 1, 220) AS GroupService, " +
			"substring(replaceRegexpAll(lower(if(LogAttributes['exception.type'] != '', LogAttributes['exception.type'], 'Error')), '\\s+', ' '), 1, 220) AS GroupType, " +
			"substring(replaceRegexpAll(lower(if(LogAttributes['exception.message'] != '', LogAttributes['exception.message'], Body)), '\\s+', ' '), 1, 220) AS GroupMessage " +
			"FROM (" + errorSourcesSQL + ") " + groupedWhereSQL + " " +
			"ORDER BY Timestamp DESC LIMIT ?"
		groupedAggregateSQL := "SELECT " +
			"GroupService, GroupType, GroupMessage, " +
			"count() AS Count, min(Timestamp) AS FirstSeen, max(Timestamp) AS LastSeen, " +
			"argMax(Timestamp, Timestamp) AS RepTimestamp, argMax(ServiceName, Timestamp) AS RepServiceName, " +
			"argMax(TraceId, Timestamp) AS RepTraceId, argMax(SpanId, Timestamp) AS RepSpanId, " +
			"argMax(Body, Timestamp) AS RepBody, argMax(LogAttributes, Timestamp) AS RepLogAttributes, " +
			"groupUniqArray(64)(TraceId) AS TraceIds " +
			"FROM (" + groupedProbeSQL + ") " +
			"GROUP BY GroupService, GroupType, GroupMessage"

		if res, err := s.db.Execute("SELECT COUNT(*) AS c FROM ("+groupedAggregateSQL+")", append(append([]any{}, whereParams...), probeLimit)...); err == nil {
			total = cInt(rowMaps(res)[0], "c")
		}
		sortDirection := "DESC"
		if sortDir == "asc" {
			sortDirection = "ASC"
		}
		pageSQL := groupedAggregateSQL + " ORDER BY " + sortCol + " " + sortDirection + " LIMIT ? OFFSET ?"
		var groupRows []map[string]any
		if res, err := s.db.Execute(pageSQL, append(append([]any{}, whereParams...), probeLimit, limit, offset)...); err == nil {
			groupRows = rowMaps(res)
		}

		type groupTuple struct{ service, gtype, message string }
		var visibleGroupTuples []groupTuple
		itemGroupTuples := map[int]groupTuple{}
		for _, row := range groupRows {
			gt := groupTuple{cStr(row, "GroupService"), cStr(row, "GroupType"), cStr(row, "GroupMessage")}
			item := s.buildErrorItem(map[string]any{
				"Timestamp": row["RepTimestamp"], "ServiceName": row["RepServiceName"],
				"TraceId": row["RepTraceId"], "SpanId": row["RepSpanId"],
				"Body": row["RepBody"], "LogAttributes": row["RepLogAttributes"],
			})
			if resolved == "1" {
				item["resolved"] = true
			} else if resolved == "0" {
				item["resolved"] = false
			} else {
				item["resolved"] = resolvedIDs[item["id"].(string)]
			}
			item["count"] = cInt(row, "Count")
			fs := cStr(row, "FirstSeen")
			if fs == "" {
				fs = item["ts"].(string)
			}
			ls := cStr(row, "LastSeen")
			if ls == "" {
				ls = item["ts"].(string)
			}
			item["first_seen"] = fs
			item["last_seen"] = ls
			itemGroupTuples[len(errors)] = gt
			visibleGroupTuples = append(visibleGroupTuples, gt)
			errors = append(errors, item)
		}

		if len(errors) > 0 {
			var uniqueGroupTuples []groupTuple
			seen := map[groupTuple]bool{}
			for _, gt := range visibleGroupTuples {
				if seen[gt] {
					continue
				}
				seen[gt] = true
				uniqueGroupTuples = append(uniqueGroupTuples, gt)
			}
			traceIDsByGroup := map[groupTuple][]string{}
			for chunkStart := 0; chunkStart < len(uniqueGroupTuples); chunkStart += groupedTraceChunkSize {
				end := chunkStart + groupedTraceChunkSize
				if end > len(uniqueGroupTuples) {
					end = len(uniqueGroupTuples)
				}
				groupChunk := uniqueGroupTuples[chunkStart:end]
				chunkParams := append([]any{}, whereParams...)
				chunkParams = append(chunkParams, probeLimit)
				tuplePlaceholders := strings.TrimSuffix(strings.Repeat("(?, ?, ?), ", len(groupChunk)), ", ")
				for _, gt := range groupChunk {
					chunkParams = append(chunkParams, gt.service, gt.gtype, gt.message)
				}
				groupedTraceSQL := "SELECT GroupService, GroupType, GroupMessage, " +
					"arrayStringConcat(groupUniqArray(64)(TraceId), ',') AS TraceIdsCsv " +
					"FROM (" + groupedProbeSQL + ") " +
					"WHERE (GroupService, GroupType, GroupMessage) IN (" + tuplePlaceholders + ") " +
					"GROUP BY GroupService, GroupType, GroupMessage"
				if res, err := s.db.Execute(groupedTraceSQL, chunkParams...); err == nil {
					for _, row := range rowMaps(res) {
						gt := groupTuple{cStr(row, "GroupService"), cStr(row, "GroupType"), cStr(row, "GroupMessage")}
						var vals []string
						for _, v := range strings.Split(cStr(row, "TraceIdsCsv"), ",") {
							if t := strings.TrimSpace(v); t != "" {
								vals = append(vals, t)
							}
						}
						traceIDsByGroup[gt] = vals
					}
				}
			}
			for i, item := range errors {
				m := item.(map[string]any)
				gt := itemGroupTuples[i]
				traceValues := append([]string{}, traceIDsByGroup[gt]...)
				primaryTrace := strings.TrimSpace(cStr(m, "trace_id"))
				if primaryTrace != "" && !containsStr(traceValues, primaryTrace) {
					traceValues = append([]string{primaryTrace}, traceValues...)
				}
				if len(traceValues) > 0 {
					m["trace_ids"] = toAnySlice(traceValues)
					m["trace_ids_csv"] = strings.Join(traceValues, ",")
				}
			}
		}
	} else {
		orderDir := "DESC"
		if sortDir == "asc" {
			orderDir = "ASC"
		}
		orderClause := "ORDER BY " + sortCol + " " + orderDir
		sourceSQL := "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes " +
			"FROM (" + errorSourcesSQL + ") " + whereSQL + " " + orderClause + " LIMIT ? OFFSET ?"
		useResolvedSQLPath := resolved == "0" || resolved == "1"
		if useResolvedSQLPath {
			pocWhereSQL := whereSQL
			pocWhereParams := append([]any{}, whereParams...)
			if resolved == "1" {
				cond := errorIDSQL + " IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)"
				pocWhereSQL = combineWhere(pocWhereSQL, cond)
			} else if resolved == "0" {
				cond := errorIDSQL + " NOT IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)"
				pocWhereSQL = combineWhere(pocWhereSQL, cond)
			}
			narrowSourceSQL := "SELECT Timestamp, ServiceName, TraceId, SpanId, " + errorIDSQL + " AS ErrorId " +
				"FROM (" + errorSourcesSQL + ") " + pocWhereSQL + " " + orderClause + " LIMIT ? OFFSET ?"

			if res, err := s.db.Execute("SELECT COUNT(*) AS c FROM ("+errorSourcesSQL+") "+pocWhereSQL, pocWhereParams...); err == nil {
				total = cInt(rowMaps(res)[0], "c")
			}
			var pageRows []map[string]any
			if res, err := s.db.Execute(narrowSourceSQL, append(append([]any{}, pocWhereParams...), limit, offset)...); err == nil {
				pageRows = rowMaps(res)
			}
			detailsByID := map[string]map[string]any{}
			if len(pageRows) > 0 {
				type detailKey struct{ ts, service, trace, span string }
				var detailKeyTuples []detailKey
				seenDetail := map[detailKey]bool{}
				for _, row := range pageRows {
					dk := detailKey{cStr(row, "Timestamp"), cStr(row, "ServiceName"), cStr(row, "TraceId"), cStr(row, "SpanId")}
					if seenDetail[dk] {
						continue
					}
					seenDetail[dk] = true
					detailKeyTuples = append(detailKeyTuples, dk)
				}
				for chunkStart := 0; chunkStart < len(detailKeyTuples); chunkStart += hydrateKeyChunkSize {
					end := chunkStart + hydrateKeyChunkSize
					if end > len(detailKeyTuples) {
						end = len(detailKeyTuples)
					}
					detailChunk := detailKeyTuples[chunkStart:end]
					var detailParams []any
					tuplePlaceholders := strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?), ", len(detailChunk)), ", ")
					for _, dk := range detailChunk {
						detailParams = append(detailParams, dk.ts, dk.service, dk.trace, dk.span)
					}
					detailSQL := "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes " +
						"FROM (" + errorSourcesSQL + ") " +
						"WHERE (Timestamp, ServiceName, TraceId, SpanId) IN (" + tuplePlaceholders + ")"
					if res, err := s.db.Execute(detailSQL, detailParams...); err == nil {
						for _, drow := range rowMaps(res) {
							detailItem := s.buildErrorItem(drow)
							detailsByID[detailItem["id"].(string)] = detailItem
						}
					}
				}
			}
			for _, row := range pageRows {
				rowID := cStr(row, "ErrorId")
				var resolvedFlag bool
				if resolved == "1" {
					resolvedFlag = true
				} else if resolved == "0" {
					resolvedFlag = false
				} else {
					resolvedFlag = resolvedIDs[rowID]
				}
				item := buildErrorStubFromNarrow(row, resolvedFlag)
				if detailItem, ok := detailsByID[item["id"].(string)]; ok {
					detailItem["resolved"] = resolvedFlag
					item = detailItem
				}
				errors = append(errors, item)
			}
		} else {
			if res, err := s.db.Execute("SELECT COUNT(*) AS c FROM ("+errorSourcesSQL+") "+whereSQL, whereParams...); err == nil {
				total = cInt(rowMaps(res)[0], "c")
			}
			if res, err := s.db.Execute(sourceSQL, append(append([]any{}, whereParams...), limit, offset)...); err == nil {
				for _, row := range rowMaps(res) {
					item := s.buildErrorItem(row)
					item["resolved"] = resolvedIDs[item["id"].(string)]
					errors = append(errors, item)
				}
			}
		}
	}

	services := s.distinctStrings("SELECT DISTINCT ServiceName FROM (" + errorSourcesSQL + ") WHERE ServiceName!='' ORDER BY ServiceName")

	refIDs := make([]string, 0, len(errors))
	for _, e := range errors {
		refIDs = append(refIDs, e.(map[string]any)["id"].(string))
	}
	workItemLinks := s.loadWorkItemLinksForRefIDs(refIDs)

	s.renderPage(w, "errors.html", "view_errors", map[string]any{
		"errors": errors, "total": total, "limit": limit, "offset": offset,
		"service": service, "selected_services": toAnySlice(selectedServices),
		"from_ts": fromTs, "to_ts": toTs, "error_msg": errorMsg, "q": q, "resolved": resolved,
		"services": services, "sort_by": sortBy, "sort_dir": sortDir, "grouped_mode": groupedMode,
		"work_item_links": workItemLinks,
	})
}

// combineWhere appends a condition to an existing WHERE fragment (or opens one), mirroring
// the Python `f"{w} AND {cond}" if w else f"WHERE {cond}"` idiom.
func combineWhere(where, cond string) string {
	if where != "" {
		return where + " AND " + cond
	}
	return "WHERE " + cond
}

// GET / — app.py summary: the overview/home page.
func (s *server) handleSummary(w http.ResponseWriter, r *http.Request) {
	unresolved := errorIDExpr + " NOT IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)"
	stats := map[string]any{
		"logs":         s.activePartRows("otel_logs"),
		"spans":        s.activePartRows("otel_traces"),
		"rum":          s.activePartRows("hyperdx_sessions"),
		"ai":           s.countRows("SELECT COUNT(*) AS c FROM otel_traces WHERE " + aiSpanCondition),
		"errors_total": s.countRows("SELECT count() AS c FROM (" + errorSourcesSQL + ")"),
		"errors":       s.countRows("SELECT count() AS c FROM (" + errorSourcesSQL + ") WHERE " + unresolved),
		"services": s.distinctStrings("SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName!='' " +
			"UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName!='' " +
			"UNION DISTINCT SELECT DISTINCT ServiceName FROM hyperdx_sessions WHERE ServiceName!=''"),
	}
	// recent_errors / recent_logs: empty on the fixture (no error/log rows).
	recentErrors := []any{}
	recentLogs := []any{}
	if res, err := s.db.Execute("SELECT Timestamp, SeverityText, ServiceName, Body FROM otel_logs ORDER BY Timestamp DESC LIMIT 10"); err == nil {
		for _, m := range rowMaps(res) {
			recentLogs = append(recentLogs, map[string]any{
				"ts": cStr(m, "Timestamp"), "level": cStr(m, "SeverityText"),
				"service": cStr(m, "ServiceName"), "body": cStr(m, "Body")})
		}
	}
	rumSummary := []any{}
	if res, err := s.db.Execute("SELECT EventName, COUNT(*) as cnt FROM hyperdx_sessions GROUP BY EventName ORDER BY cnt DESC"); err == nil {
		for _, m := range rowMaps(res) {
			rumSummary = append(rumSummary, []any{cStr(m, "EventName"), cInt(m, "cnt")})
		}
	}
	cveLastScan, _ := s.appSetting("enrichment.cve_last_scan")
	s.renderPage(w, "summary.html", "summary", map[string]any{
		"stats":         stats,
		"recent_errors": recentErrors,
		"recent_logs":   recentLogs,
		"rum_summary":   rumSummary,
		"ai_summary":    []any{},
		"signal_health": []any{},
		"cve_overview": map[string]any{
			"enabled": s.appSettingBool("enrichment.cve_enabled", true), "last_scan": cveLastScan,
			"total": 0, "critical": 0, "high": 0, "medium": 0, "low": 0,
		},
	})
}

// GET /web-traffic — app.py view_web_traffic.
func (s *server) handleViewWebTraffic(w http.ResponseWriter, r *http.Request) {
	total := s.activePartRows("hyperdx_sessions")
	topUrls := []any{}
	if res, err := s.db.Execute("SELECT LogAttributes['url'] AS url, COUNT(*) AS cnt " +
		"FROM hyperdx_sessions  GROUP BY url HAVING url != '' ORDER BY cnt DESC LIMIT 20"); err == nil {
		for _, m := range rowMaps(res) {
			topUrls = append(topUrls, []any{cStr(m, "url"), cInt(m, "cnt")})
		}
	}
	eventTypes := []any{}
	if res, err := s.db.Execute("SELECT EventName, COUNT(*) AS cnt FROM hyperdx_sessions  " +
		"GROUP BY EventName ORDER BY cnt DESC LIMIT 20"); err == nil {
		for _, m := range rowMaps(res) {
			eventTypes = append(eventTypes, []any{cStr(m, "EventName"), cInt(m, "cnt")})
		}
	}
	s.renderPage(w, "web_traffic.html", "view_web_traffic", map[string]any{
		"total": total, "top_urls": topUrls, "event_types": eventTypes,
		"from_ts": "", "to_ts": "", "error_msg": "",
		"geo_enabled": s.appSettingBool("enrichment.geo_enabled", true),
	})
}

// GET /settings/agents — app.py view_agent_rules. rules/runs/tag_rules are empty on the
// fixture; anomaly_rules has the 4 seeded rules; the trigger/action lists are constants.
func (s *server) handleViewAgentRules(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.createAgentRule(w, r)
		return
	}
	s.renderPage(w, "settings_agents.html", "view_agent_rules", map[string]any{
		"rules":          s.loadAgentRulesCtx(),
		"runs":           []any{},
		"anomaly_rules":  s.loadAnomalyRulesCtx(),
		"tag_rules":      s.loadTagRulesCtx(),
		"trigger_types":  []any{"anomaly_rule", "tag_rule", "manual"},
		"trigger_states": []any{"warning", "critical", "any"},
		"agent_actions":  []any{"analyze", "github_issue", "github_issue_copilot", "dlp_check"},
	})
}

var tagRuleFields = map[string]bool{"service_name": true, "severity": true, "body": true, "span_name": true, "event_type": true, "attribute": true}
var tagRuleOperators = map[string]bool{"eq": true, "contains": true, "regex": true}
var tagRuleRecordTypes = map[string]bool{"log": true, "trace": true, "error": true, "ai": true, "rum": true, "all": true}

// createTagRule mirrors app.py create_tag_rule (POST /settings/tags) create path.
func (s *server) createTagRule(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	loc := "/settings/tags"
	name := strings.TrimSpace(r.PostFormValue("name"))
	tagKey := strings.TrimSpace(r.PostFormValue("tag_key"))
	tagValue := strings.TrimSpace(r.PostFormValue("tag_value"))
	at := func(xs []string, i int) string {
		if i < len(xs) {
			return strings.TrimSpace(xs[i])
		}
		return ""
	}
	cf, co, cv, ca := r.PostForm["condition_field"], r.PostForm["condition_operator"], r.PostForm["condition_value"], r.PostForm["condition_attr_key"]
	n := 0
	for _, xs := range [][]string{cf, co, cv, ca} {
		if len(xs) > n {
			n = len(xs)
		}
	}
	type cond struct{ field, op, val, attr string }
	conditions := []cond{}
	for i := 0; i < n; i++ {
		f := strings.ToLower(at(cf, i))
		if f == "" {
			continue
		}
		conditions = append(conditions, cond{f, orDefault(strings.ToLower(at(co, i)), "eq"), at(cv, i), at(ca, i)})
	}
	if len(conditions) == 0 {
		f := strings.ToLower(strings.TrimSpace(r.PostFormValue("match_field")))
		if f != "" {
			conditions = []cond{{f, orDefault(strings.ToLower(strings.TrimSpace(r.PostFormValue("match_operator"))), "eq"),
				strings.TrimSpace(r.PostFormValue("match_value")), strings.TrimSpace(r.PostFormValue("match_attr_key"))}}
		}
	}
	if name == "" || len(conditions) == 0 || tagKey == "" || tagValue == "" {
		flashRedirect(w, "warning", "Name, at least one match condition, tag key, and tag value are required", loc)
		return
	}
	for _, c := range conditions {
		if !tagRuleFields[c.field] {
			flashRedirect(w, "warning", "Invalid match field: "+c.field, loc)
			return
		}
		if !tagRuleOperators[c.op] {
			flashRedirect(w, "warning", "Invalid match operator: "+c.op, loc)
			return
		}
		if c.field == "attribute" && c.attr == "" {
			flashRedirect(w, "warning", "Attribute key is required when match field is 'attribute'", loc)
			return
		}
		if c.op == "regex" {
			if _, err := regexp.Compile(c.val); err != nil {
				flashRedirect(w, "warning", "Invalid regex pattern: "+err.Error(), loc)
				return
			}
		}
	}
	chosen := []string{}
	for _, t := range r.PostForm["record_types"] {
		t = strings.TrimSpace(t)
		if tagRuleRecordTypes[t] {
			chosen = append(chosen, t)
		}
	}
	recordTypesStr := "all"
	if len(chosen) > 0 {
		recordTypesStr = strings.Join(chosen, ",")
	}
	condList := make([]any, len(conditions))
	for i, c := range conditions {
		condList[i] = map[string]any{"match_field": c.field, "match_operator": c.op, "match_value": c.val, "match_attr_key": c.attr}
	}
	condJSON, _ := json.Marshal(condList)
	p := conditions[0]
	row := map[string]any{
		"Id": newUUIDHex(), "Name": name, "RecordTypes": recordTypesStr,
		"MatchField": p.field, "MatchOperator": p.op, "MatchValue": p.val, "MatchAttrKey": p.attr,
		"TagKey": tagKey, "TagValue": tagValue, "ConditionsJson": string(condJSON),
		"IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.db.InsertJSONEachRow("sobs_tag_rules", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	flashRedirect(w, "success", fmt.Sprintf("Tag rule '%s' created", name), loc)
}

var agentTriggerTypes = map[string]bool{"anomaly_rule": true, "tag_rule": true, "manual": true}
var agentTriggerStates = map[string]bool{"warning": true, "critical": true, "any": true}
var agentActions = map[string]bool{"analyze": true, "github_issue": true, "github_issue_copilot": true, "dlp_check": true}

// createAgentRule mirrors app.py create_agent_rule (POST /settings/agents): validate, insert,
// flash + redirect. The inserted uuid id is not in the response.
func (s *server) createAgentRule(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	loc := "/settings/agents"
	name := strings.TrimSpace(r.PostFormValue("name"))
	triggerType := orDefault(strings.ToLower(strings.TrimSpace(r.PostFormValue("trigger_type"))), "manual")
	triggerState := orDefault(strings.ToLower(strings.TrimSpace(r.PostFormValue("trigger_state"))), "any")
	rateLimit := 60
	if v, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("rate_limit_minutes"))); err == nil {
		rateLimit = clampInt(v, 1, 10080)
	}
	if name == "" {
		flashRedirect(w, "warning", "Rule name is required", loc)
		return
	}
	if !agentTriggerTypes[triggerType] {
		flashRedirect(w, "warning", "Invalid trigger type: "+triggerType, loc)
		return
	}
	if !agentTriggerStates[triggerState] {
		flashRedirect(w, "warning", "Invalid trigger state: "+triggerState, loc)
		return
	}
	valid := []string{}
	for _, a := range r.PostForm["actions"] {
		if agentActions[a] {
			valid = append(valid, a)
		}
	}
	if len(valid) == 0 {
		valid = []string{"analyze"}
	}
	row := map[string]any{
		"Id": newUUIDHex(), "Name": name, "Description": strings.TrimSpace(r.PostFormValue("description")),
		"TriggerType": triggerType, "TriggerRefId": strings.TrimSpace(r.PostFormValue("trigger_ref_id")),
		"TriggerState": triggerState, "Actions": strings.Join(valid, ","), "RateLimitMinutes": rateLimit,
		"IsEnabled": 1, "IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.db.InsertJSONEachRow("sobs_agent_rules", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	flashRedirect(w, "success", fmt.Sprintf("Agent rule '%s' created", name), loc)
}

// GET /settings/mcp — mcp.py mcp_settings_page. now_iso is the frozen determinism clock.
func (s *server) handleMcpSettingsPage(w http.ResponseWriter, r *http.Request) {
	enabled := true
	if v, ok := s.appSetting("mcp.enabled"); ok {
		enabled = v == "1"
	}
	s.renderPage(w, "settings_mcp.html", "mcp.mcp_settings_page", map[string]any{
		"mcp_keys": []any{}, "mcp_enabled": enabled, "now_iso": "2024-01-02T03:04:05+00:00",
	})
}

// GET /settings/tags — app.py view_tag_rules. Empty tag rules; services from telemetry.
func (s *server) handleViewTagRules(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.createTagRule(w, r)
		return
	}
	services := s.distinctStrings("SELECT DISTINCT ServiceName FROM (" +
		"  SELECT ServiceName FROM otel_logs " +
		"  UNION DISTINCT SELECT ServiceName FROM otel_traces " +
		"  UNION DISTINCT SELECT ServiceName FROM hyperdx_sessions)")
	s.renderPage(w, "settings_tags.html", "view_tag_rules", map[string]any{
		"rules": s.loadTagRulesCtx(), "edit_rule": nil,
		"record_types":    []any{"log", "trace", "error", "ai", "rum", "all"},
		"match_fields":    []any{"service_name", "severity", "body", "span_name", "event_type", "attribute"},
		"match_operators": []any{"eq", "contains", "regex"},
		"services":        services,
		"auto_preview":    []any{}, "auto_summary": nil, "auto_open_panel": "",
	})
}

// GET /settings/ai — app.py view_ai_settings. Empty AI settings -> all keys "".
// aiSaveSkipKeys are the ai.* settings NOT written by the simple form loop in app.py
// save_ai_settings (token-validation bookkeeping + the pricing fields handled separately).
var aiSaveSkipKeys = map[string]bool{
	"ai.github_token_expires_at": true, "ai.github_token_last_validated_at": true,
	"ai.github_token_last_validation_status": true, "ai.github_token_last_validation_message": true,
	"ai.model_pricing": true, "ai.model_pricing_confirmed": true,
}

func (s *server) handleViewAiSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// app.py save_ai_settings: write each ai.* key from its prefix-stripped form field
		// (empty form -> ""), then flash success. Pricing/token-bookkeeping keys are skipped.
		_ = r.ParseForm()
		var aiKeys []string
		_ = json.Unmarshal(aiSettingKeysJSON, &aiKeys)
		for _, k := range aiKeys {
			if aiSaveSkipKeys[k] {
				continue
			}
			_ = s.setAppSetting(k, strings.TrimSpace(r.PostFormValue(strings.TrimPrefix(k, "ai."))))
		}
		flashRedirect(w, "success", "AI settings saved", "/settings/ai")
		return
	}
	var keys []string
	_ = json.Unmarshal(aiSettingKeysJSON, &keys)
	settings := map[string]any{}
	for _, k := range keys {
		settings[k] = ""
	}
	defPricing, _ := parseJSONValue(defaultAiPricingJSON)
	savedPricing, _ := parseJSONValue(savedAiPricingJSON)
	sources, _ := parseJSONValue(aiPricingSourcesJSON)
	s.renderPage(w, "settings_ai.html", "view_ai_settings", map[string]any{
		"settings":                  settings,
		"anomaly_rules":             s.loadAnomalyRulesCtx(),
		"tag_rules":                 []any{},
		"github_token_expires_date": "",
		"github_token_expiry_status": map[string]any{
			"state": "unknown", "expires_at": "", "days_remaining": nil, "message": "Token expiry date not set"},
		"github_token_validation_status": map[string]any{
			"status": "", "message": "", "last_validated_at": ""},
		"default_ai_pricing":          defPricing,
		"saved_ai_pricing":            savedPricing,
		"ai_pricing_sources":          sources,
		"confirmed_ai_pricing_models": []any{},
	})
}

// GET /settings/repositories — app.py view_settings_repositories. Empty apps/AI settings.
// createSettingsRepository mirrors app.py create_settings_repository (POST /settings/repositories
// create path). The github-token/repo-token/agent-repo saves are gated behind set_* flags
// (absent in the create test). The inserted app's uuid id is not in the response.
func (s *server) createSettingsRepository(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	loc := "/settings/repositories"
	name := strings.TrimSpace(r.PostFormValue("name"))
	repoURL, owner, repo := resolveGithubRepoFields(
		strings.TrimSpace(r.PostFormValue("repo_url")),
		strings.TrimSpace(r.PostFormValue("repo_owner")),
		strings.TrimSpace(r.PostFormValue("repo_name")))
	if name == "" || repoURL == "" {
		flashRedirect(w, "warning", "App name and repository are required", loc)
		return
	}
	slugSrc := strings.TrimSpace(r.PostFormValue("slug"))
	if slugSrc == "" {
		slugSrc = name
	}
	slug := appSlug(slugSrc, "app")
	if s.rowExists("SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 LIMIT 1", slug) {
		flashRedirect(w, "warning", "App slug already exists", loc)
		return
	}
	row := map[string]any{
		"Id": newUUIDHex(), "Name": name, "Slug": slug, "OwnerTeam": "", "RepoUrl": repoURL,
		"DefaultEnvironment": strings.TrimSpace(r.PostFormValue("default_environment")),
		"Enabled":            1, "MetadataJson": "{}", "IsDeleted": 0, "Version": fixedVersionMillis(),
		"CreatedAt": nowISO(), "UpdatedAt": nowISO(),
	}
	if _, err := s.insertRowsNormalized("sobs_apps", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	githubToken := strings.TrimSpace(r.PostFormValue("github_token"))
	if r.PostFormValue("set_github_token") != "" && githubToken != "" {
		s.saveAISetting("ai.github_token", githubToken)
		s.saveAISetting("ai.github_token_expires_at", strings.TrimSpace(r.PostFormValue("github_token_expires_at")))
	}
	if r.PostFormValue("set_repo_token") != "" && githubToken != "" && owner != "" && repo != "" {
		s.saveAISetting(githubRepoTokenKey(owner, repo), githubToken)
	}
	if r.PostFormValue("set_agent_repo") != "" && owner != "" && repo != "" {
		s.saveAISetting("ai.github_repo", owner+"/"+repo)
	}
	flashRedirect(w, "success", "Repository added", loc)
}

func (s *server) handleViewSettingsRepositories(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.createSettingsRepository(w, r)
		return
	}
	apps := s.buildRepositoriesApps()
	realtimeEnabled, realtimeConfigured := false, false
	for _, a := range apps {
		if m, ok := a.(map[string]any); ok {
			if cps, ok := m["ci_push_status"].(map[string]any); ok {
				if truthy(cps["realtime_enabled"]) {
					realtimeEnabled = true
				}
				if truthy(cps["configured"]) {
					realtimeConfigured = true
				}
			}
		}
	}
	expiresAt := strings.TrimSpace(s.loadAISetting("ai.github_token_expires_at", ""))
	s.renderPage(w, "settings_repositories.html", "view_settings_repositories", map[string]any{
		"apps":                       apps,
		"github_token_configured":    strings.TrimSpace(s.loadAISetting("ai.github_token", "")) != "",
		"default_agent_repo":         strings.TrimSpace(s.loadAISetting("ai.github_repo", "")),
		"github_token_expires_date":  githubTokenExpiryDateInputValue(expiresAt),
		"github_token_expiry_status": githubTokenExpiryStatus(expiresAt, 14),
		"github_token_validation_status": map[string]any{
			"status":            strings.TrimSpace(s.loadAISetting("ai.github_token_last_validation_status", "")),
			"message":           strings.TrimSpace(s.loadAISetting("ai.github_token_last_validation_message", "")),
			"last_validated_at": strings.TrimSpace(s.loadAISetting("ai.github_token_last_validated_at", "")),
		},
		"github_token_expiry_warning_days": 14,
		"realtime_seed": map[string]any{
			"enabled": realtimeEnabled, "configured": realtimeConfigured, "expires_at": "",
			"expiry_message": "Per-repository CI ingest keys are managed from each repository row.",
			"api_key":        "", "api_key_show_once": false},
		"ci_push_default_ttl_days": 30,
		"ci_push_max_ttl_days":     365,
	})
}

// GET /settings/kubernetes — app.py view_k8s_settings: render with k8s settings + flash.
func (s *server) handleViewK8sSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// app.py save_k8s_settings: _k8s_settings_from_form -> {"kubernetes.enabled": "1"/"0"},
		// then a plain query-param redirect (no flash).
		_ = r.ParseForm()
		enabled := "0"
		if r.PostFormValue("enabled") == "1" {
			enabled = "1"
		}
		_ = s.setAppSetting("kubernetes.enabled", enabled)
		plainRedirect(w, "/settings/kubernetes?msg=Settings+saved&msg_type=success")
		return
	}
	val, _ := s.appSetting("kubernetes.enabled")
	msgType := r.URL.Query().Get("msg_type")
	if msgType == "" {
		msgType = "success"
	}
	s.renderPage(w, "settings_kubernetes.html", "view_k8s_settings", map[string]any{
		"k8s_settings": map[string]any{"kubernetes.enabled": val},
		"flash_msg":    r.URL.Query().Get("msg"),
		"flash_type":   msgType,
	})
}

// GET /settings/enrichment — app.py view_enrichment_settings: geo/cve flags + backfill.
func (s *server) handleViewEnrichmentSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// app.py save_enrichment_settings: geo/cve checkboxes (absent -> "false") + backfill
		// max-releases (empty -> default 300), then flash success.
		_ = r.ParseForm()
		geo := "false"
		if r.PostFormValue("geo_enabled") != "" {
			geo = "true"
		}
		cve := "false"
		if r.PostFormValue("cve_enabled") != "" {
			cve = "true"
		}
		maxRel := strings.TrimSpace(r.PostFormValue("github_backfill_max_releases"))
		if maxRel == "" {
			maxRel = "300"
		}
		_ = s.setAppSetting("enrichment.geo_enabled", geo)
		_ = s.setAppSetting("enrichment.cve_enabled", cve)
		_ = s.setAppSetting("enrichment.github_backfill_max_releases", maxRel)
		flashRedirect(w, "success", "Enrichment settings saved", "/settings/enrichment")
		return
	}
	cveLastScan, _ := s.appSetting("enrichment.cve_last_scan")
	maxRel := 300
	if raw, ok := s.appSetting("enrichment.github_backfill_max_releases"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			if n < 1 {
				n = 1
			} else if n > 2000 {
				n = 2000
			}
			maxRel = n
		}
	}
	s.renderPage(w, "settings_enrichment.html", "view_enrichment_settings", map[string]any{
		"geo_enabled":                        s.appSettingBool("enrichment.geo_enabled", true),
		"cve_enabled":                        s.appSettingBool("enrichment.cve_enabled", true),
		"cve_last_scan":                      cveLastScan,
		"github_backfill_max_releases":       maxRel,
		"github_backfill_min_releases":       1,
		"github_backfill_max_releases_limit": 2000,
	})
}

// GET /settings/masking — app.py view_masking_settings: render the masking config page.
func (s *server) handleViewMaskingSettings(w http.ResponseWriter, r *http.Request) {
	var def struct {
		Keys     []string `json:"keys"`
		Patterns []string `json:"patterns"`
	}
	_ = json.Unmarshal(maskingDefaultsJSON, &def)
	customKeys := s.loadJSONStringListSetting("masking.custom_keys")
	customPatterns := s.loadJSONStringListSetting("masking.custom_patterns")
	defaultKeys := append([]string{}, def.Keys...)
	sort.Strings(defaultKeys)
	keySet := map[string]bool{}
	for _, k := range def.Keys {
		keySet[k] = true
	}
	for _, k := range customKeys {
		keySet[k] = true
	}
	s.renderPage(w, "settings_masking.html", "view_masking_settings", map[string]any{
		"custom_keys":                strsToAny(customKeys),
		"custom_patterns":            strsToAny(customPatterns),
		"default_keys":               strsToAny(defaultKeys),
		"default_patterns":           strsToAny(def.Patterns),
		"effective_key_count":        len(keySet),
		"effective_pattern_count":    len(def.Patterns) + len(customPatterns),
		"output_masking_enabled":     s.appSettingBool("masking.output_enabled", true),
		"sql_output_masking_enabled": s.appSettingBool("masking.sql_output_enabled", true),
	})
}

// GET /kubernetes — app.py view_kubernetes: 404 string when k8s is disabled (fixture).
func (s *server) handleViewKubernetes(w http.ResponseWriter, r *http.Request) {
	if !s.kubernetesEnabled() {
		textStatus(w, http.StatusNotFound, "Kubernetes health view is disabled. Enable it in Settings → Kubernetes.")
		return
	}
	s.renderPage(w, "kubernetes.html", "view_kubernetes", nil)
}

// GET /dashboards/new — app.py new_dashboard_form: render custom_dashboards.html with an
// empty dashboards list and the new-form flag.
func (s *server) handleNewDashboardForm(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "custom_dashboards.html", "new_dashboard_form", map[string]any{
		"dashboards":    []any{},
		"show_new_form": true,
	})
}
