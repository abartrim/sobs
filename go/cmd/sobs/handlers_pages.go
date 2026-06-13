package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
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
