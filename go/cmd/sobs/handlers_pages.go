package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

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
	out, err := s.newEngine().Render(templateName, ctx)
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
	if !s.cfg.QueryPageEnabled {
		textStatus(w, http.StatusNotFound, "Query page is unavailable until AI and guard settings are configured.")
		return
	}
	s.renderPage(w, "query.html", "view_query", nil)
}

// GET /table-explorer — app.py view_table_explorer: 404 string when disabled (fixture).
func (s *server) handleViewTableExplorer(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.QueryPageEnabled {
		textStatus(w, http.StatusNotFound, "Table Explorer is unavailable until AI and guard settings are configured.")
		return
	}
	s.renderPage(w, "table_explorer.html", "view_table_explorer", nil)
}

// GET /dashboards — app.py list_dashboards: render custom_dashboards.html with the
// non-deleted dashboards (_get_dashboards).
func (s *server) handleListDashboards(w http.ResponseWriter, r *http.Request) {
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

// GET /logs — app.py view_logs. Empty otel_logs on the fixture (all lists/stats empty).
func (s *server) handleViewLogs(w http.ResponseWriter, r *http.Request) {
	services := s.distinctStrings("SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName!='' ORDER BY ServiceName")
	levels := s.distinctStrings("SELECT DISTINCT SeverityText FROM otel_logs ORDER BY SeverityText")
	eventNames := s.distinctStrings("SELECT DISTINCT EventName FROM otel_logs WHERE EventName!='' ORDER BY EventName")
	s.renderPage(w, "logs.html", "view_logs", map[string]any{
		"logs": []any{}, "total": 0, "limit": 200, "offset": 0, "q": "",
		"level": "", "selected_levels": []any{}, "service": "", "selected_services": []any{},
		"trace_id": "", "trace_ids_csv": "", "trace_ids_count": 0, "sql_where": "",
		"from_ts": "", "to_ts": "", "services": services, "levels": levels,
		"event_names": eventNames, "event_name": "", "selected_event_names": []any{},
		"error_msg": "", "sort_by": "Timestamp", "sort_dir": "desc",
		"run_advanced_analysis": false, "level_stats": map[string]any{},
		"service_stats": map[string]any{}, "tag_stats": []any{}, "advanced_analysis": nil,
		// epoch-0 formatted under determinism (stats not generated); display is "" so its
		// {% if %} block is skipped.
		"stats_generated_at_iso": "1969-12-31T17:00:00+00:00", "stats_generated_at_display": "", "stats_generated_age_s": 0,
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

// GET /errors — app.py view_errors. No error rows on the fixture -> empty lists/defaults.
func (s *server) handleViewErrors(w http.ResponseWriter, r *http.Request) {
	services := s.distinctStrings("SELECT DISTINCT ServiceName FROM (" + errorSourcesSQL + ") WHERE ServiceName != ''")
	s.renderPage(w, "errors.html", "view_errors", map[string]any{
		"errors": []any{}, "total": 0, "limit": 100, "offset": 0,
		"service": "", "selected_services": []any{}, "from_ts": "", "to_ts": "",
		"error_msg": "", "q": "", "resolved": "0", "services": services,
		"sort_by": "Timestamp", "sort_dir": "desc", "grouped_mode": false,
		"work_item_links": map[string]any{},
	})
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
	s.renderPage(w, "settings_agents.html", "view_agent_rules", map[string]any{
		"rules":          []any{},
		"runs":           []any{},
		"anomaly_rules":  s.loadAnomalyRulesCtx(),
		"tag_rules":      []any{},
		"trigger_types":  []any{"anomaly_rule", "tag_rule", "manual"},
		"trigger_states": []any{"warning", "critical", "any"},
		"agent_actions":  []any{"analyze", "github_issue", "github_issue_copilot", "dlp_check"},
	})
}

// GET /settings/kubernetes — app.py view_k8s_settings: render with k8s settings + flash.
func (s *server) handleViewK8sSettings(w http.ResponseWriter, r *http.Request) {
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
	if !s.cfg.KubernetesEnabled {
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
