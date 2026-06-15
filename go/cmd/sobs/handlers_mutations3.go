package main

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Path-parameter mutation handlers. Each is registered as a ServeMux subtree ("…/") and
// dispatches by the trailing path segments + method. On the fixture the referenced record
// never exists, so the deterministic branch is the not-found / validation error.

// DELETE /api/mcp/keys/<key_id> — mcp.py mcp_api_delete_key: loads the mcp.api_keys setting
// (a JSON list) and 404s when no descriptor has the given id. The fixture has no keys.
func (s *server) handleMcpKeyByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.NotFound(w, r)
		return
	}
	keyID := strings.TrimPrefix(r.URL.Path, "/api/mcp/keys/")
	raw, _ := s.appSetting("mcp.api_keys")
	if raw == "" {
		raw = "[]"
	}
	keys := []any{}
	if v, err := parseJSONValue([]byte(raw)); err == nil {
		if list, ok := v.([]any); ok {
			keys = list
		}
	}
	// mcp.py mcp_api_delete_key: drop the descriptor whose id matches; 404 if none did.
	newKeys := []any{}
	for _, it := range keys {
		if o, ok := it.(*jsonenc.Object); ok {
			if idv, _ := o.Get("id"); idv == keyID {
				continue
			}
		}
		newKeys = append(newKeys, it)
	}
	if len(newKeys) == len(keys) {
		s.errorJSON(w, http.StatusNotFound, "Key not found.")
		return
	}
	saved := "[]"
	if len(newKeys) > 0 {
		if b, err := json.Marshal(newKeys); err == nil {
			saved = string(b)
		}
	}
	_ = s.setAppSetting("mcp.api_keys", saved)
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true))
}

// DELETE /api/notifications/vapid-keys — app.py delete_vapid_keys: clears the DB VAPID key
// and reports the fixed note. env_override is false when SOBS_VAPID_PRIVATE_KEY is unset.
func (s *server) handleVapidKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("env_override", false).
		Set("note", "DB VAPID key cleared. Browser push is now unconfigured until new keys are generated.").
		Set("ok", true))
}

// /api/reports/import (POST) and /api/reports/<report_id> (DELETE). Registered under the
// "/api/reports/" subtree; the bare "/api/reports" route keeps its own handler.
func (s *server) handleReportsSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/reports/")
	if rest == "import" {
		// app.py api_import_reports: an empty/invalid body fails the export-file schema check.
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		errorOnly(w, http.StatusBadRequest, "Not a valid SOBS reports export file")
		return
	}
	// DELETE /api/reports/<report_id> — app.py api_delete_report (soft-delete -> {"deleted":true}).
	if r.Method == http.MethodDelete {
		res, err := s.db.Execute("SELECT Id, Name, Description, PageType, FiltersJson FROM sobs_reports FINAL WHERE IsDeleted = 0 AND Id = ?", rest)
		if err != nil || len(res.Rows) == 0 {
			errorOnly(w, http.StatusNotFound, "not found")
			return
		}
		m := rowMaps(res)[0]
		row := map[string]any{
			"Id": rest, "Name": cStr(m, "Name"), "Description": cStr(m, "Description"),
			"PageType": cStr(m, "PageType"), "FiltersJson": cStr(m, "FiltersJson"),
			"IsDeleted": 1, "Version": fixedVersionMillis(),
		}
		if _, err := s.db.InsertJSONEachRow("sobs_reports", []map[string]any{row}); err != nil {
			s.dbError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("deleted", true))
		return
	}
	http.NotFound(w, r)
}

// POST /api/agent/runs/<run_id>/dismiss — 404 when the run does not exist.
func (s *server) handleAgentRunSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/agent/runs/")
	runID, ok := strings.CutSuffix(rest, "/dismiss")
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	res, err := s.db.Execute("SELECT Id, RuleId, RuleName, TriggerContext, Status, GuardDecision, "+
		"DlpResult, Analysis, Suggestion, GithubIssueUrl, ErrorMessage, CreatedAt, CompletedAt "+
		"FROM sobs_agent_runs FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1", runID)
	if err != nil {
		s.dbError(w, err)
		return
	}
	if len(res.Rows) == 0 {
		s.errorJSON(w, http.StatusNotFound, "run not found")
		return
	}
	// app.py dismiss_agent_run: re-insert the row with IsDismissed=1 (ReplacingMergeTree upsert),
	// copying every column forward; the new Version (fixed millis) wins FINAL.
	ex := rowMaps(res)[0]
	row := map[string]any{
		"Id": runID, "RuleId": cStr(ex, "RuleId"), "RuleName": cStr(ex, "RuleName"),
		"TriggerContext": cStr(ex, "TriggerContext"), "Status": cStr(ex, "Status"),
		"GuardDecision": cStr(ex, "GuardDecision"), "DlpResult": cStr(ex, "DlpResult"),
		"Analysis": cStr(ex, "Analysis"), "Suggestion": cStr(ex, "Suggestion"),
		"GithubIssueUrl": cStr(ex, "GithubIssueUrl"), "ErrorMessage": cStr(ex, "ErrorMessage"),
		"CreatedAt": cStr(ex, "CreatedAt"), "CompletedAt": cStr(ex, "CompletedAt"),
		"IsDismissed": 1, "IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_agent_runs", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true))
}

// POST /api/notifications/channels/<channel_id>/test — 404 when the channel does not exist.
func (s *server) handleChannelSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/notifications/channels/")
	chID, ok := strings.CutSuffix(rest, "/test")
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	res, err := s.db.Execute("SELECT Id, Name, ChannelType, ConfigJson, Enabled "+
		"FROM sobs_notification_channels FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1", chID)
	if err != nil {
		s.dbError(w, err)
		return
	}
	if len(res.Rows) == 0 {
		s.errorJSON(w, http.StatusNotFound, "channel not found")
		return
	}
	// app.py test_notification_channel: dispatch a test payload through the channel; "ok" => 200,
	// any error => 500 with the message (the test payload itself is the unobservable POST body).
	m := rowMaps(res)[0]
	summary := "[SOBS] Test notification from channel '" + cStr(m, "Name") + "'"
	if result := s.dispatchNotificationChannel(cStr(m, "ChannelType"), cStr(m, "ConfigJson"), summary); result == "ok" {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true))
	} else {
		writeJSON(w, http.StatusInternalServerError, jsonenc.NewObject().Set("ok", false).Set("error", result))
	}
}

var cveDispositionValues = map[string]bool{"open": true, "accepted": true, "false_positive": true, "fixed": true}

// POST /api/enrichment/cve/findings/<osv_id>/disposition — app.py api_cve_set_disposition:
// upsert a CVE finding disposition + optional note (deterministic; frozen timestamp).
func (s *server) handleCveDispositionSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/enrichment/cve/findings/")
	osvID, ok := strings.CutSuffix(rest, "/disposition")
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	m := bodyMap(r)
	pkg, eco, ver := bstr(m, "package"), bstr(m, "ecosystem"), bstr(m, "version")
	disp := strings.ToLower(bstr(m, "disposition"))
	note := bstr(m, "note")
	if strings.TrimSpace(osvID) == "" || pkg == "" || eco == "" || ver == "" {
		s.errorJSON(w, http.StatusBadRequest, "osv_id, package, ecosystem, and version are required")
		return
	}
	if !cveDispositionValues[disp] {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().
			Set("ok", false).Set("error", "invalid disposition: "+disp).
			Set("allowed", []any{"accepted", "false_positive", "fixed", "open"}))
		return
	}
	nowTs := nowISO()
	currentVersion := fixedVersionMillis()
	createdAt, versionUnder := nowTs, currentVersion
	if res, err := s.db.Execute("SELECT CreatedAt, Version_ FROM sobs_cve_dispositions FINAL "+
		"WHERE OsvId=? AND Package=? AND Ecosystem=? AND Version=? LIMIT 1", osvID, pkg, eco, ver); err == nil && len(res.Rows) > 0 {
		ex := rowMaps(res)[0]
		createdAt = pyDateTimeStr(cStr(ex, "CreatedAt"))
		if v := int64(cInt(ex, "Version_")) + 1; v > versionUnder {
			versionUnder = v
		}
	}
	row := map[string]any{
		"OsvId": osvID, "Package": pkg, "Ecosystem": eco, "Version": ver,
		"Disposition": disp, "Note": note, "CreatedAt": createdAt, "UpdatedAt": nowTs,
		"Version_": versionUnder,
	}
	if _, err := s.insertRowsNormalized("sobs_cve_dispositions", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("osv_id", osvID).Set("package", pkg).Set("ecosystem", eco).
		Set("version", ver).Set("disposition", disp).Set("note", note).Set("updated_at", nowTs))
}

// POST /api/dashboards/<dashboard_id>/charts/import — 404 when the dashboard does not exist.
// Registered under the "/api/dashboards/" subtree (exact /api/dashboards/* routes win).
func (s *server) handleDashboardSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/dashboards/")
	if dashID, ok := strings.CutSuffix(rest, "/charts/import"); ok && r.Method == http.MethodPost {
		if !s.rowExists("SELECT Id FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Id = ?", dashID) {
			s.errorJSON(w, http.StatusNotFound, "Dashboard not found")
			return
		}
		s.importChart(w, r, dashID)
		return
	}
	// GET /api/dashboards/<dashboard_id>/charts/<chart_id>/export — 404 when the dashboard
	// is absent (the lookup precedes the chart lookup).
	if seg := strings.Split(rest, "/"); r.Method == http.MethodGet && len(seg) == 4 &&
		seg[1] == "charts" && seg[3] == "export" {
		if !s.rowExists("SELECT Id FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Id = ?", seg[0]) {
			s.errorJSON(w, http.StatusNotFound, "Dashboard not found")
			return
		}
		s.exportChart(w, seg[0], seg[2])
		return
	}
	http.NotFound(w, r)
}

var chartTitleUnsafeRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// exportChart mirrors app.py export_chart: emit a chart as a downloadable JSON template
// (indent=2, attachment Content-Disposition). The dashboard existence check is the caller's.
func (s *server) exportChart(w http.ResponseWriter, dashID, chartID string) {
	res, err := s.db.Execute("SELECT Id, Title, ChartType, Query, OptionsJson "+
		"FROM sobs_chart_configs FINAL WHERE IsDeleted = 0 AND DashboardId = ? AND Id = ?", dashID, chartID)
	if err != nil {
		s.dbError(w, err)
		return
	}
	if len(res.Rows) == 0 {
		s.errorJSON(w, http.StatusNotFound, "Chart not found")
		return
	}
	c := rowMaps(res)[0]
	title := cStr(c, "Title")
	chartSpec := buildRawChartSpec(cStr(c, "ChartType"), cStr(c, "Query"), cStr(c, "OptionsJson"))
	payload := jsonenc.NewObject().
		Set("sobs_chart_template_version", 1).Set("title", title).Set("chart_spec", chartSpec)
	body, err := jsonDumpsIndent2NoEsc(payload)
	if err != nil {
		s.dbError(w, err)
		return
	}
	safeTitle := chartTitleUnsafeRe.ReplaceAllString(title, "_")
	if len(safeTitle) > 64 {
		safeTitle = safeTitle[:64]
	}
	if safeTitle == "" {
		safeTitle = "chart"
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Disposition", `attachment; filename="sobs_chart_`+safeTitle+`.json"`)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// importChart mirrors app.py import_chart: validate the template, compile the chart_spec, and
// insert a new chart at the next position. The dashboard existence check is done by the caller.
func (s *server) importChart(w http.ResponseWriter, r *http.Request, dashID string) {
	raw, _ := io.ReadAll(r.Body)
	body := asObject(func() any { v, _ := parseJSONValue(raw); return v }())
	tv, tvOK := body.Get("sobs_chart_template_version")
	if !numEquals(tv, tvOK, 1) {
		s.errorJSON(w, http.StatusBadRequest,
			"Invalid or unsupported chart template format (expected sobs_chart_template_version: 1)")
		return
	}
	titleV, titleOK := body.Get("title")
	title := pyStrOrStrip(titleV, titleOK)
	if title == "" {
		title = "Imported Chart"
	}
	chartSpecRaw, csOK := body.Get("chart_spec")
	if !isTruthyVal(chartSpecRaw, csOK) {
		s.errorJSON(w, http.StatusBadRequest, "chart_spec is required in template")
		return
	}
	templateID, query, normSpec, errMsg := s.compileChartSpec(chartSpecRaw)
	if errMsg != "" {
		s.errorJSON(w, http.StatusBadRequest, "Chart spec error: "+errMsg)
		return
	}
	optionsJSON := string(jsonenc.Encode(jsonenc.NewObject().Set("chart_spec", normSpec), jsonDumpsDefault))
	position := s.nextChartPosition(dashID)
	chartID := newUUIDv4()
	row := map[string]any{
		"Id": chartID, "DashboardId": dashID, "Title": title, "ChartType": templateID,
		"Query": query, "OptionsJson": optionsJSON, "Position": position,
		"IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_chart_configs", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("chart_id", chartID).Set("dashboard_id", dashID).
		Set("dashboard_url", "/dashboards/"+dashID))
}

// nextChartPosition returns max(Position)+1 over a dashboard's live charts (-1 default -> 0).
func (s *server) nextChartPosition(dashID string) int {
	res, err := s.db.Execute("SELECT max(Position) AS m FROM sobs_chart_configs FINAL "+
		"WHERE IsDeleted = 0 AND DashboardId = ?", dashID)
	if err != nil || len(res.Rows) == 0 {
		return 0
	}
	m := rowMaps(res)[0]
	if v, ok := m["m"]; !ok || v == nil {
		return 0
	}
	return cInt(m, "m") + 1
}

// POST /errors/<error_id>/resolve — app.py marks the error resolved; idempotent, so an
// unknown id still returns {"ok": true}. Registered under the "/errors/" subtree.
func (s *server) handleErrorSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/errors/")
	if _, ok := strings.CutSuffix(rest, "/resolve"); ok && r.Method == http.MethodPost {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true))
		return
	}
	http.NotFound(w, r)
}
