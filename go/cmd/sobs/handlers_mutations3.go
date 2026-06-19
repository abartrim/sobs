package main

import (
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
		if paramMethodGuard(w, r) {
			return
		}
		http.NotFound(w, r)
		return
	}
	keyID := strings.TrimPrefix(r.URL.Path, "/api/mcp/keys/")
	keys := s.loadMcpAPIKeys()
	// mcp.py mcp_api_delete_key: new_keys = [k for k in keys if k.get("id") != key_id]; 404 if
	// the list is unchanged. loadMcpAPIKeys/saveMcpAPIKeys are the shared keystore round-trip so
	// create/auth/delete agree on the schema (id/label/key_hash/created_at/expires_at descriptors).
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
	s.saveMcpAPIKeys(newKeys)
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
		// /api/reports/import is a STATIC route (Werkzeug prefers it over the <report_id> rule),
		// so a wrong method yields the import route's own Allow, not the param route's.
		if r.Method != http.MethodPost {
			if exactMethodGuard(w, r, "/api/reports/import") {
				return
			}
			http.NotFound(w, r)
			return
		}
		s.importReports(w, r)
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
	if paramMethodGuard(w, r) {
		return
	}
	http.NotFound(w, r)
}

// Report-import constants mirror app.py: _REPORTS_EXPORT_VERSION / _REPORTS_IMPORT_MAX /
// _REPORTS_IMPORT_MAX_BYTES (reportPageTypes lives in mutation_helpers.go).
const (
	reportsExportVersion  = "1"
	reportsImportMax      = 500
	reportsImportMaxBytes = 5 * 1024 * 1024
)

// importReports mirrors app.py api_import_reports: accept a previously-exported reports payload
// (JSON body OR multipart "file" upload), validate the envelope, and insert/replace/skip per the
// on_conflict policy. Returns {imported, skipped, replaced, errors}. On the empty corpus the test
// posts `{}`, which fails the envelope check -> {"error":"Not a valid SOBS reports export file"} 400.
func (s *server) importReports(w http.ResponseWriter, r *http.Request) {
	payloadTooLarge := func() {
		errorOnly(w, http.StatusRequestEntityTooLarge,
			"Import payload too large (max "+strconv.Itoa(reportsImportMaxBytes)+" bytes)")
	}
	// if (request.content_length or 0) > _REPORTS_IMPORT_MAX_BYTES.
	if r.ContentLength > reportsImportMaxBytes {
		payloadTooLarge()
		return
	}

	contentType := r.Header.Get("Content-Type")
	onConflict := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("on_conflict")))

	var bodyObj *jsonenc.Object
	if strings.Contains(contentType, "multipart/form-data") ||
		strings.Contains(contentType, "application/x-www-form-urlencoded") {
		// Parse the multipart form; on_conflict falls back to the form field then "rename".
		_ = r.ParseMultipartForm(reportsImportMaxBytes)
		if onConflict == "" {
			onConflict = strings.ToLower(strings.TrimSpace(orDefault(r.FormValue("on_conflict"), "rename")))
		}
		file, _, ferr := r.FormFile("file")
		if ferr != nil || file == nil {
			errorOnly(w, http.StatusBadRequest, "No file uploaded")
			return
		}
		defer file.Close()
		rawBytes, _ := io.ReadAll(io.LimitReader(file, reportsImportMaxBytes+1))
		if len(rawBytes) > reportsImportMaxBytes {
			payloadTooLarge()
			return
		}
		parsed, perr := parseJSONValue(rawBytes)
		if perr != nil {
			errorOnly(w, http.StatusBadRequest, "Invalid JSON file")
			return
		}
		o, ok := parsed.(*jsonenc.Object)
		if !ok {
			// Non-dict JSON (list/scalar) fails the envelope dict check below.
			errorOnly(w, http.StatusBadRequest, "Not a valid SOBS reports export file")
			return
		}
		bodyObj = o
	} else {
		// body = await request.get_json(silent=True); None -> 400.
		raw, _ := io.ReadAll(r.Body)
		parsed, perr := parseJSONValue(raw)
		if perr != nil {
			errorOnly(w, http.StatusBadRequest, "Invalid or missing JSON body")
			return
		}
		o, ok := parsed.(*jsonenc.Object)
		if !ok {
			// get_json returned a non-object (list/scalar/None handled above); not a dict ->
			// envelope check. A JSON null decodes to nil here -> treat as missing body.
			if parsed == nil {
				errorOnly(w, http.StatusBadRequest, "Invalid or missing JSON body")
				return
			}
			errorOnly(w, http.StatusBadRequest, "Not a valid SOBS reports export file")
			return
		}
		bodyObj = o
		if onConflict == "" {
			ocVal, ocOK := bodyObj.Get("on_conflict")
			onConflict = strings.ToLower(strings.TrimSpace(orDefaultVal(ocVal, ocOK, "rename")))
		}
	}

	// Envelope: must be a dict with a truthy sobs_reports_export flag.
	exportFlag, exportOK := bodyObj.Get("sobs_reports_export")
	if !isTruthyVal(exportFlag, exportOK) {
		errorOnly(w, http.StatusBadRequest, "Not a valid SOBS reports export file")
		return
	}
	versionVal, versionOK := bodyObj.Get("version")
	// str(body.get("version", "")) — absent defaults to "" (NOT None); present values str()-coerced.
	versionStr := ""
	if versionOK {
		versionStr = pyStr(versionVal, true)
	}
	if versionStr != reportsExportVersion {
		// f"Unsupported export version: {body.get('version')!r}" — Python repr (None when absent).
		errorOnly(w, http.StatusBadRequest, "Unsupported export version: "+pyRepr(versionVal, versionOK))
		return
	}
	if onConflict != "rename" && onConflict != "replace" && onConflict != "skip" {
		errorOnly(w, http.StatusBadRequest, "on_conflict must be one of: rename, replace, skip")
		return
	}

	incomingVal, _ := bodyObj.Get("reports")
	incoming, isList := incomingVal.([]any)
	if !isList {
		errorOnly(w, http.StatusBadRequest, "'reports' must be a list")
		return
	}
	if len(incoming) > reportsImportMax {
		errorOnly(w, http.StatusBadRequest, "Too many reports (max "+strconv.Itoa(reportsImportMax)+")")
		return
	}

	// existing_index: (page_type, lower(name)) -> {id, name, description, page_type, filters}.
	type existingReport struct {
		id, name, description, pageType, filtersJSON string
	}
	existingIndex := map[string]existingReport{}
	res, err := s.db.Execute("SELECT Id, Name, Description, PageType, FiltersJson " +
		"FROM sobs_reports FINAL WHERE IsDeleted = 0 ORDER BY PageType, Name")
	if err != nil {
		s.dbError(w, err)
		return
	}
	for _, m := range rowMaps(res) {
		er := existingReport{
			id: cStr(m, "Id"), name: cStr(m, "Name"), description: cStr(m, "Description"),
			pageType: cStr(m, "PageType"), filtersJSON: cStr(m, "FiltersJson"),
		}
		existingIndex[er.pageType+"\x00"+strings.ToLower(er.name)] = er
	}

	nImported, nSkipped, nReplaced, nErrors := 0, 0, 0, 0
	versionBase := fixedVersionMillis()

	for idx, itemAny := range incoming {
		item, ok := itemAny.(*jsonenc.Object)
		if !ok {
			nErrors++
			continue
		}
		nameV, nameOK := item.Get("name")
		name := strings.TrimSpace(orDefaultVal(nameV, nameOK, ""))
		descV, descOK := item.Get("description")
		description := strings.TrimSpace(orDefaultVal(descV, descOK, ""))
		ptV, ptOK := item.Get("page_type")
		pageType := strings.TrimSpace(orDefaultVal(ptV, ptOK, ""))
		// filters = item.get("filters") or {}; must be a dict.
		filtersV, filtersOK := item.Get("filters")
		var filters *jsonenc.Object
		if !isTruthyVal(filtersV, filtersOK) {
			filters = jsonenc.NewObject()
		} else if fo, isObj := filtersV.(*jsonenc.Object); isObj {
			filters = fo
		} else {
			nErrors++
			continue
		}

		if name == "" {
			nErrors++
			continue
		}
		if !reportPageTypes[pageType] {
			nErrors++
			continue
		}

		conflictKey := pageType + "\x00" + strings.ToLower(name)
		conflict, hasConflict := existingIndex[conflictKey]
		isReplace := false

		if hasConflict {
			switch onConflict {
			case "skip":
				nSkipped++
				continue
			case "replace":
				// Soft-delete the existing report (Version = base + idx*2).
				delRow := map[string]any{
					"Id": conflict.id, "Name": conflict.name, "Description": conflict.description,
					"PageType": conflict.pageType,
					// json.dumps(conflict["filters"], ensure_ascii=False) where conflict["filters"]
					// is _parse_report_filters(FiltersJson) — re-serialize the stored filters.
					"FiltersJson": jsonDumpsNoEsc(parseReportFiltersNative(conflict.filtersJSON)),
					"IsDeleted":   1, "Version": versionBase + int64(idx)*2,
				}
				if _, derr := s.db.InsertJSONEachRow("sobs_reports", []map[string]any{delRow}); derr != nil {
					s.dbError(w, derr)
					return
				}
				nReplaced++
				delete(existingIndex, conflictKey)
				isReplace = true
			default: // rename — find a unique " (imported)" name
				candidate := name + " (imported)"
				suffix := 2
				for {
					if _, exists := existingIndex[pageType+"\x00"+strings.ToLower(candidate)]; !exists {
						break
					}
					candidate = name + " (imported " + strconv.Itoa(suffix) + ")"
					suffix++
				}
				name = candidate
			}
		}

		newID := newUUIDv4()
		newRow := map[string]any{
			"Id": newID, "Name": name, "Description": description, "PageType": pageType,
			"FiltersJson": jsonDumpsNoEsc(filters), "IsDeleted": 0,
			"Version": versionBase + int64(idx)*2 + 1,
		}
		if _, ierr := s.db.InsertJSONEachRow("sobs_reports", []map[string]any{newRow}); ierr != nil {
			s.dbError(w, ierr)
			return
		}
		existingIndex[pageType+"\x00"+strings.ToLower(name)] = existingReport{
			id: newID, name: name, pageType: pageType,
		}
		if isReplace {
			// Replacement inserts are counted as replaced, not imported.
			continue
		}
		nImported++
	}

	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("imported", nImported).
		Set("skipped", nSkipped).
		Set("replaced", nReplaced).
		Set("errors", nErrors))
}

// POST /api/agent/runs/<run_id>/dismiss — 404 when the run does not exist.
func (s *server) handleAgentRunSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/agent/runs/")
	runID, ok := strings.CutSuffix(rest, "/dismiss")
	if !ok || r.Method != http.MethodPost {
		if paramMethodGuard(w, r) {
			return
		}
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
		if paramMethodGuard(w, r) {
			return
		}
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
	if notificationChannelMaskOutputEnabled(cStr(m, "ConfigJson")) {
		summary = s.maskStringForOutput(summary)
	}
	testPayload := jsonenc.NewObject().
		Set("rule_name", "Test").Set("severity", "info").Set("conditions", []any{}).
		Set("summary", summary).
		Set("fired_at", nowUTC().Format("2006-01-02T15:04:05.000000-07:00"))
	if result := s.dispatchNotificationChannel(cStr(m, "ChannelType"), cStr(m, "ConfigJson"), testPayload); result == "ok" {
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
		if paramMethodGuard(w, r) {
			return
		}
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
	if paramMethodGuard(w, r) {
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
	// app.py: `if template_version != 1` — Python's `True == 1`, so a JSON `true` value passes
	// (as does 1 and 1.0); numEquals only covers numbers, so accept bool-true too.
	if !pythonEquals1(tv, tvOK) {
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
	if paramMethodGuard(w, r) {
		return
	}
	http.NotFound(w, r)
}
