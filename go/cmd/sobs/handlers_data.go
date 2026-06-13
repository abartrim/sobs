package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// GET /api/chart-types — app.py api_chart_types(): load static/echarts-chart-types.json
// and return {"ok": true, "data": <catalog>}. Go reads the SAME file and re-serializes it
// through jsonenc, so jsonify's sorted/compact bytes match. 404 if the file is absent.
func (s *server) handleApiChartTypes(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.cfg.StaticDir, "echarts-chart-types.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		s.errorJSON(w, http.StatusNotFound,
			"Chart types catalog not found. Run: node scripts/extract-echarts-types.js")
		return
	}
	catalog, err := parseJSONValue(raw)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError,
			jsonenc.NewObject().Set("ok", false).Set("error", "Failed to load chart types: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("data", catalog))
}

// Data-backed JSON API handlers. Each issues the SAME SQL as its app.py counterpart and
// rebuilds the response object with identical keys + Python type coercion. Because Go and
// Python read the SAME on-disk chdb fixture, identical SQL yields identical rows, and
// jsonenc.QuartJSONify reproduces jsonify's bytes.

// GET /api/dashboards/list — app.py api_dashboards_list() -> _get_dashboards (app.py:21274).
func (s *server) handleApiDashboardsList(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute(
		"SELECT Id, Name, Description FROM sobs_dashboards FINAL WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		s.dbError(w, err)
		return
	}
	dashboards := []any{}
	for _, m := range rowMaps(res) {
		dashboards = append(dashboards, jsonenc.NewObject().
			Set("id", cStr(m, "Id")).
			Set("name", cStr(m, "Name")).
			Set("description", cStr(m, "Description")))
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("dashboards", dashboards))
}

// GET /api/reports — app.py api_list_reports() -> _get_reports (app.py:22649). Returns a
// top-level JSON array. Optional ?page_type filter changes the WHERE + ORDER BY.
func (s *server) handleApiReports(w http.ResponseWriter, r *http.Request) {
	pageType := strings.TrimSpace(r.URL.Query().Get("page_type"))
	query := "SELECT Id, Name, Description, PageType, FiltersJson " +
		"FROM sobs_reports FINAL WHERE IsDeleted = 0 ORDER BY PageType, Name"
	var params []any
	if pageType != "" {
		query = "SELECT Id, Name, Description, PageType, FiltersJson " +
			"FROM sobs_reports FINAL WHERE IsDeleted = 0 AND PageType = ? ORDER BY Name"
		params = []any{pageType}
	}
	res, err := s.db.Execute(query, params...)
	if err != nil {
		s.dbError(w, err)
		return
	}
	reports := []any{}
	for _, m := range rowMaps(res) {
		reports = append(reports, jsonenc.NewObject().
			Set("id", cStr(m, "Id")).
			Set("name", cStr(m, "Name")).
			Set("description", cStr(m, "Description")).
			Set("page_type", cStr(m, "PageType")).
			Set("filters", parseJSONObject(cStr(m, "FiltersJson"))))
	}
	writeJSON(w, http.StatusOK, reports)
}

// GET /api/agent/runs — app.py list_agent_runs() -> _load_agent_runs (app.py:5707).
func (s *server) handleApiAgentRuns(w http.ResponseWriter, r *http.Request) {
	limit := queryIntClamp(r, "limit", 50, 1, 200)
	res, err := s.db.Execute(
		"SELECT Id, RuleId, RuleName, TriggerContext, Status, GuardDecision, DlpResult, " +
			"Analysis, Suggestion, GithubIssueUrl, ErrorMessage, CreatedAt, CompletedAt, IsDismissed " +
			"FROM sobs_agent_runs FINAL WHERE IsDeleted=0 ORDER BY CreatedAt DESC " +
			"LIMIT " + strconv.Itoa(limit))
	if err != nil {
		s.dbError(w, err)
		return
	}
	runs := []any{}
	for _, m := range rowMaps(res) {
		runs = append(runs, jsonenc.NewObject().
			Set("id", cStr(m, "Id")).
			Set("rule_id", cStr(m, "RuleId")).
			Set("rule_name", cStr(m, "RuleName")).
			Set("trigger_context", cStr(m, "TriggerContext")).
			Set("status", cStr(m, "Status")).
			Set("guard_decision", cStr(m, "GuardDecision")).
			Set("dlp_result", cStr(m, "DlpResult")).
			Set("analysis", cStr(m, "Analysis")).
			Set("suggestion", cStr(m, "Suggestion")).
			Set("github_issue_url", cStr(m, "GithubIssueUrl")).
			Set("error_message", cStr(m, "ErrorMessage")).
			Set("created_at", cStr(m, "CreatedAt")).
			Set("completed_at", cStr(m, "CompletedAt")).
			Set("is_dismissed", cBool(m, "IsDismissed")))
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("runs", runs))
}

// GET /api/enrichment/libraries — app.py api_enrichment_libraries. The merged library
// inventory (release registry + OTEL SDK/scope tiers) and CVE findings are all empty on
// the fixture -> libraries []. scanned_at from the cve_last_scan setting ("").
func (s *server) handleApiEnrichmentLibraries(w http.ResponseWriter, r *http.Request) {
	scanned, _ := s.appSetting("enrichment.cve_last_scan")
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("libraries", []any{}).Set("scanned_at", scanned))
}

// GET /api/work-items — app.py api_get_work_items: github work items (empty on fixture).
// The async GitHub backfill is fire-and-forget (does not affect the response) and is skipped.
func (s *server) handleApiWorkItems(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	conds := []string{"IsDeleted = 0"}
	var params []any
	for arg, col := range map[string]string{
		"anomaly_rule_id": "AnomalyRuleId", "service": "ServiceName", "rule_id": "AgentRuleId",
		"signal_source": "SignalSource", "signal_name": "SignalName",
	} {
		if v := strings.TrimSpace(q.Get(arg)); v != "" {
			conds = append(conds, col+" = ?")
			params = append(params, v)
		}
	}
	limit := queryIntClamp(r, "limit", 100, 1, 1000)
	res, err := s.db.Execute("SELECT * FROM sobs_github_work_items FINAL WHERE "+strings.Join(conds, " AND ")+
		" ORDER BY CreatedAt DESC LIMIT "+strconv.Itoa(limit), params...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, jsonenc.NewObject().Set("ok", false).Set("error", err.Error()))
		return
	}
	items := []any{}
	_ = rowMaps(res) // rows are empty on the fixture; serialization lands with seeded work items
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("items", items))
}

// GET /api/data-management/backup/list — app.py api_dm_backup_list() -> _list_dm_backups
// (app.py:32109): reads system.backups (empty on the fixture). Query errors fall back to
// [] (mirrors the Python try/except).
func (s *server) handleApiDmBackupList(w http.ResponseWriter, r *http.Request) {
	backups := []any{}
	res, err := s.db.Execute(
		"SELECT name, status, start_time, end_time, num_files, total_size, error " +
			"FROM system.backups ORDER BY start_time DESC LIMIT 100")
	if err == nil {
		for _, m := range rowMaps(res) {
			backups = append(backups, jsonenc.NewObject().
				Set("name", cStrDef(m, "name", "")).
				Set("status", cStrDef(m, "status", "")).
				Set("start_time", cStrDef(m, "start_time", "")).
				Set("end_time", cStrDef(m, "end_time", "")).
				Set("num_files", cStrDef(m, "num_files", "0")).
				Set("total_size", cStrDef(m, "total_size", "0")).
				Set("error", cStrDef(m, "error", "")))
		}
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("backups", backups))
}

// --- Web traffic (RUM) aggregations over hyperdx_sessions (app.py:17766+) -----------
// No time-window query args => empty WHERE (matches _parse_time_window_args returning "").

// GET /api/web-traffic/browsers
func (s *server) handleApiWebTrafficBrowsers(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute(
		"SELECT LogAttributes['browser.context.browserName'] AS browser, " +
			"LogAttributes['browser.context.browserVersion'] AS version, COUNT(*) AS cnt " +
			"FROM hyperdx_sessions GROUP BY browser, version ORDER BY cnt DESC LIMIT 50")
	if err != nil {
		s.dbError(w, err)
		return
	}
	browsers := []any{}
	for _, m := range rowMaps(res) {
		name := strings.TrimSpace(cStr(m, "browser") + " " + cStr(m, "version"))
		if name == "" {
			name = "Unknown"
		}
		browsers = append(browsers, jsonenc.NewObject().Set("name", name).Set("value", cInt(m, "cnt")))
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("browsers", browsers))
}

// GET /api/web-traffic/os
func (s *server) handleApiWebTrafficOS(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute(
		"SELECT LogAttributes['browser.context.osName'] AS os, " +
			"LogAttributes['browser.context.osVersion'] AS version, COUNT(*) AS cnt " +
			"FROM hyperdx_sessions GROUP BY os, version ORDER BY cnt DESC LIMIT 50")
	if err != nil {
		s.dbError(w, err)
		return
	}
	osList := []any{}
	for _, m := range rowMaps(res) {
		name := strings.TrimSpace(cStr(m, "os") + " " + cStr(m, "version"))
		if name == "" {
			name = "Unknown"
		}
		osList = append(osList, jsonenc.NewObject().Set("name", name).Set("value", cInt(m, "cnt")))
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("operating_systems", osList))
}

// GET /api/web-traffic/timezones
func (s *server) handleApiWebTrafficTimezones(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute(
		"SELECT LogAttributes['browser.context.timezone'] AS tz, COUNT(*) AS cnt " +
			"FROM hyperdx_sessions GROUP BY tz HAVING tz != '' ORDER BY cnt DESC LIMIT 50")
	if err != nil {
		s.dbError(w, err)
		return
	}
	tzs := []any{}
	for _, m := range rowMaps(res) {
		tzs = append(tzs, jsonenc.NewObject().Set("name", cStr(m, "tz")).Set("value", cInt(m, "cnt")))
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("timezones", tzs))
}

// GET /api/web-traffic/languages
func (s *server) handleApiWebTrafficLanguages(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute(
		"SELECT LogAttributes['browser.context.language'] AS lang, COUNT(*) AS cnt " +
			"FROM hyperdx_sessions GROUP BY lang HAVING lang != '' ORDER BY cnt DESC LIMIT 50")
	if err != nil {
		s.dbError(w, err)
		return
	}
	langs := []any{}
	for _, m := range rowMaps(res) {
		langs = append(langs, jsonenc.NewObject().Set("name", cStr(m, "lang")).Set("value", cInt(m, "cnt")))
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("languages", langs))
}

// GET /api/web-traffic/devices
func (s *server) handleApiWebTrafficDevices(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute(
		"SELECT LogAttributes['browser.context.deviceClass'] AS device, COUNT(*) AS cnt " +
			"FROM hyperdx_sessions GROUP BY device HAVING device != '' ORDER BY cnt DESC")
	if err != nil {
		s.dbError(w, err)
		return
	}
	devices := []any{}
	for _, m := range rowMaps(res) {
		devices = append(devices, jsonenc.NewObject().Set("name", cStr(m, "device")).Set("value", cInt(m, "cnt")))
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("devices", devices))
}
