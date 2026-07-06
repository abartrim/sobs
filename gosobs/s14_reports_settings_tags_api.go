package main

// Port of app.py lines 22534-24505:
//   - Metrics Anomaly API (GET /api/metrics/anomaly)
//   - Reports: saved filter configurations CRUD + import/export
//   - Static RUM script routes with content-hash ETags
//   - Settings hub page (GET /settings)
//   - Masking settings pages + APIs (/settings/masking, /api/settings/masking/*)
//   - Tag Rules pages (/settings/tags) + auto-tag generation + condition suggestions
//   - Record Tags API (/api/tags/...)
//   - Log field hints + SQL filter validation (/api/logs/field-hints, validate-filter)
//   - Regex validate helpers + 5 validate-regex endpoints (logs/errors/traces/metrics/rum)
//   - AI field hints + AI SQL filter validation (/api/ai/field-hints, validate-filter)
//   - SSE live tail (GET /tail) consuming the s06 subscriber registry
//
// PORT-NOTE: this section references many symbols owned by other sections via
// the deterministic naming rule (loadTagRules, softDeleteLatestRow,
// loadMaskingSettings, getAppSetting/setAppSetting, queryAllowedTables, etc.).
// They are assumed to exist; the reconcile phase fixes any signature drift.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func init() {
	registerRoute("GET", "/api/metrics/anomaly", requireBasicAuth(metricsAnomaly))

	registerRoute("GET", "/reports", requireBasicAuth(listReports))
	registerRoute("POST", "/reports/{report_id}/delete", requireBasicAuth(deleteReport))
	registerRoute("GET", "/api/reports", requireBasicAuth(apiListReports))
	registerRoute("POST", "/api/reports", requireBasicAuth(apiCreateReport))
	registerRoute("DELETE", "/api/reports/{report_id}", requireBasicAuth(apiDeleteReport))
	registerRoute("GET", "/api/reports/export", requireBasicAuth(apiExportReports))
	registerRoute("POST", "/api/reports/import", requireBasicAuth(apiImportReports))

	registerRoute("GET", "/static/rum.js", rumJs)
	registerRoute("GET", "/static/rum.js.map", rumJsMap)
	registerRoute("GET", "/static/rum.min.js", rumMinJs)
	registerRoute("GET", "/static/rum.min.js.map", rumMinJsMap)
	registerRoute("GET", "/static/rum.d.ts", rumDTs)

	registerRoute("GET", "/settings", requireBasicAuth(viewSettings))

	registerRoute("GET", "/settings/masking", requireBasicAuth(viewMaskingSettings))
	registerRoute("POST", "/settings/masking/keys", requireBasicAuth(addMaskingKey))
	registerRoute("POST", "/settings/masking/keys/delete", requireBasicAuth(deleteMaskingKey))
	registerRoute("POST", "/settings/masking/patterns", requireBasicAuth(addMaskingPattern))
	registerRoute("POST", "/settings/masking/patterns/delete", requireBasicAuth(deleteMaskingPattern))
	registerRoute("POST", "/settings/masking/output", requireBasicAuth(updateMaskingOutputSetting))
	registerRoute("POST", "/settings/masking/sql-output", requireBasicAuth(updateMaskingSqlOutputSetting))
	registerRoute("POST", "/api/settings/masking/preview", requireBasicAuth(apiMaskingPreview))
	registerRoute("GET", "/api/settings/masking/rules", requireBasicAuth(apiMaskingRules))

	registerRoute("GET", "/settings/tags", requireBasicAuth(viewTagRules))
	registerRoute("GET", "/api/settings/tags/condition-suggestions", requireBasicAuth(apiTagRuleConditionSuggestions))
	registerRoute("POST", "/settings/tags/auto", requireBasicAuth(autoTagRules))
	registerRoute("POST", "/settings/tags", requireBasicAuth(createTagRule))
	registerRoute("POST", "/settings/tags/{rule_id}/delete", requireBasicAuth(deleteTagRule))

	registerRoute("GET", "/api/tags/{record_type}/{record_id}", requireApiKey(apiGetTags))
	registerRoute("POST", "/api/tags/{record_type}/{record_id}", requireApiKey(apiAddTag))
	registerRoute("DELETE", "/api/tags/{record_type}/{record_id}/{tag_key}", requireApiKey(apiDeleteTag))

	registerRoute("GET", "/api/logs/field-hints", requireBasicAuth(apiLogsFieldHints))
	registerRoute("POST", "/api/logs/validate-filter", requireBasicAuth(apiLogsValidateFilter))
	registerRoute("POST", "/api/logs/validate-regex", requireBasicAuth(apiLogsValidateRegex))
	registerRoute("POST", "/api/errors/validate-regex", requireBasicAuth(apiErrorsValidateRegex))
	registerRoute("POST", "/api/traces/validate-regex", requireBasicAuth(apiTracesValidateRegex))
	registerRoute("POST", "/api/metrics/validate-regex", requireBasicAuth(apiMetricsValidateRegex))
	registerRoute("POST", "/api/rum/validate-regex", requireBasicAuth(apiRumValidateRegex))

	registerRoute("GET", "/api/ai/field-hints", requireBasicAuth(apiAiFieldHints))
	registerRoute("POST", "/api/ai/validate-filter", requireBasicAuth(apiAiValidateFilter))

	registerRoute("GET", "/tail", requireBasicAuth(tailStream))
}

// ---------------------------------------------------------------------------
// File-local helpers
// ---------------------------------------------------------------------------

// formList mirrors Quart form.getlist(key): returns all submitted values for a
// repeated form field (empty slice when absent).
func formList(r *http.Request, key string) []string {
	if r.Form == nil {
		return nil
	}
	return r.Form[key]
}

// toStringSlice coerces an arbitrary value (typically a []string or []any from a
// loaded-settings map) into a []string.
func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, rowString(item))
		}
		return out
	case nil:
		return nil
	default:
		return nil
	}
}

// containsStr reports whether s is present in list.
func containsStr(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// asBool coerces a settings value to bool.
func asBool(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// sortedSetKeys returns the keys of a set-style map, sorted ascending.
// PORT-NOTE: queryAllowedTables ports Python's _QUERY_ALLOWED_TABLES frozenset;
// assumed to be a map[string]struct{}. Reconcile adjusts if it is a []string.
func sortedSetKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Metrics Anomaly API  GET /api/metrics/anomaly
// ---------------------------------------------------------------------------

// metricsAnomaly returns per-minute anomaly detection data for a specific
// metric series.
func metricsAnomaly(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	service := strings.TrimSpace(query.Get("service"))
	metric := strings.TrimSpace(query.Get("metric"))
	if service == "" || metric == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "service and metric query parameters are required"})
		return
	}

	hours := 24
	if raw := strings.TrimSpace(query.Get("hours")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			hours = v
		}
	}
	if hours < 1 {
		hours = 1
	}
	if hours > 168 {
		hours = 168
	}

	attrFp := strings.TrimSpace(query.Get("attr_fp"))

	db := getDb()
	fpClause := ""
	params := []any{service, metric, hours}
	if attrFp != "" {
		fpClause = " AND AttrFingerprint = ?"
		params = append(params, attrFp)
	}
	result, err := db.Execute(
		"SELECT"+
			"  time,"+
			"  value,"+
			"  SampleCount AS sample_count,"+
			"  baseline_mean,"+
			"  baseline_stddev,"+
			"  baseline_lower,"+
			"  baseline_upper,"+
			"  anomaly_score,"+
			"  anomaly_state,"+
			"  MetricKind AS metric_kind,"+
			"  AttrFingerprint AS attr_fp"+
			" FROM v_otel_metrics_anomaly"+
			" WHERE ServiceName = ?"+
			"   AND MetricName = ?"+
			"   AND time >= now() - INTERVAL ? HOUR"+
			fpClause+
			" ORDER BY time"+
			" LIMIT 1440",
		params...,
	)
	if err != nil {
		logger.Error("metrics_anomaly query failed", "service", service, "metric", metric, "error", err)
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": publicDashboardQueryError(err)})
		return
	}

	rows := result.Fetchall()
	columns := []string{
		"time", "value", "sample_count", "baseline_mean", "baseline_stddev",
		"baseline_lower", "baseline_upper", "anomaly_score", "anomaly_state",
		"metric_kind", "attr_fp",
	}
	if len(rows) > 0 && len(result.Cols) > 0 {
		columns = result.Cols
	}

	safe := func(v any) any {
		// IEEE 754: NaN is the only value not equal to itself.
		if f, ok := v.(float64); ok && math.IsNaN(f) {
			return nil
		}
		return v
	}

	data := make([][]any, 0, len(rows))
	for _, row := range rows {
		rec := make([]any, 0, len(columns))
		for _, col := range columns {
			rec = append(rec, safe(row[col]))
		}
		data = append(data, rec)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"service": service, "metric": metric, "columns": columns, "rows": data})
}

// ---------------------------------------------------------------------------
// Reports – saved filter configurations
// ---------------------------------------------------------------------------

// reportPageTypes mirrors _REPORT_PAGE_TYPES.
var reportPageTypes = map[string]struct{}{
	"logs": {}, "traces": {}, "errors": {}, "metrics": {},
	"rum": {}, "ai": {}, "work_items": {}, "web_traffic": {},
}

func reportPageTypesSorted() []string { return sortedSetKeys(reportPageTypes) }

// parseReportFilters mirrors _parse_report_filters.
func parseReportFilters(rawFiltersJson any) map[string]any {
	s := rowString(rawFiltersJson)
	if s == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return map[string]any{}
	}
	if m, ok := parsed.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// getReports mirrors _get_reports. pageType == "" selects all reports.
func getReports(db *ChDbConnection, pageType string) []map[string]any {
	var rows []Row
	if pageType != "" {
		res, err := db.Execute(
			"SELECT Id, Name, Description, PageType, FiltersJson "+
				"FROM sobs_reports FINAL WHERE IsDeleted = 0 AND PageType = ? ORDER BY Name",
			pageType,
		)
		if err == nil {
			rows = res.Fetchall()
		}
	} else {
		res, err := db.Execute(
			"SELECT Id, Name, Description, PageType, FiltersJson " +
				"FROM sobs_reports FINAL WHERE IsDeleted = 0 ORDER BY PageType, Name",
		)
		if err == nil {
			rows = res.Fetchall()
		}
	}
	out := make([]map[string]any, 0, len(rows))
	for _, rrow := range rows {
		out = append(out, map[string]any{
			"id":          rowString(rrow["Id"]),
			"name":        rowString(rrow["Name"]),
			"description": rowString(rrow["Description"]),
			"page_type":   rowString(rrow["PageType"]),
			"filters":     parseReportFilters(rrow["FiltersJson"]),
		})
	}
	return out
}

// getReport mirrors _get_report. Returns nil when not found.
func getReport(db *ChDbConnection, reportId string) map[string]any {
	res, err := db.Execute(
		"SELECT Id, Name, Description, PageType, FiltersJson FROM sobs_reports FINAL WHERE IsDeleted = 0 AND Id = ?",
		reportId,
	)
	if err != nil {
		return nil
	}
	row := res.Fetchone()
	if row == nil {
		return nil
	}
	return map[string]any{
		"id":          rowString(row["Id"]),
		"name":        rowString(row["Name"]),
		"description": rowString(row["Description"]),
		"page_type":   rowString(row["PageType"]),
		"filters":     parseReportFilters(row["FiltersJson"]),
	}
}

func listReports(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	reports := getReports(db, "")
	renderTemplate(w, r, "reports.html", map[string]any{"reports": reports})
}

func deleteReport(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	reportId := r.PathValue("report_id")
	report := getReport(db, reportId)
	if report == nil {
		flashMessage(w, r, "Report not found", "danger")
		http.Redirect(w, r, "/reports", http.StatusFound)
		return
	}
	version := time.Now().UnixMilli()
	filtersJson, _ := json.Marshal(report["filters"])
	_, _ = insertRowsJsonEachRow(db, "sobs_reports", []Row{
		{
			"Id":          reportId,
			"Name":        report["name"],
			"Description": report["description"],
			"PageType":    report["page_type"],
			"FiltersJson": string(filtersJson),
			"IsDeleted":   1,
			"Version":     version,
		},
	})
	flashMessage(w, r, fmt.Sprintf("Report '%s' deleted", rowString(report["name"])), "success")
	http.Redirect(w, r, "/reports", http.StatusFound)
}

func apiListReports(w http.ResponseWriter, r *http.Request) {
	pageType := strings.TrimSpace(r.URL.Query().Get("page_type"))
	db := getDb()
	reports := getReports(db, pageType)
	jsonResponse(w, http.StatusOK, reports)
}

func apiCreateReport(w http.ResponseWriter, r *http.Request) {
	body, _ := readJsonBody(r)
	name := strings.TrimSpace(rowString(body["name"]))
	description := strings.TrimSpace(rowString(body["description"]))
	pageType := strings.TrimSpace(rowString(body["page_type"]))
	filters, filtersOk := body["filters"].(map[string]any)
	if body["filters"] == nil {
		filters = map[string]any{}
		filtersOk = true
	}

	if name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	if _, ok := reportPageTypes[pageType]; !ok {
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("page_type must be one of: %s", strings.Join(reportPageTypesSorted(), ", ")),
		})
		return
	}
	if !filtersOk {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filters must be an object"})
		return
	}

	reportId := agentUuid4()
	version := time.Now().UnixMilli()
	db := getDb()
	filtersJson, _ := json.Marshal(filters)
	_, _ = insertRowsJsonEachRow(db, "sobs_reports", []Row{
		{
			"Id":          reportId,
			"Name":        name,
			"Description": description,
			"PageType":    pageType,
			"FiltersJson": string(filtersJson),
			"IsDeleted":   0,
			"Version":     version,
		},
	})
	jsonResponse(w, http.StatusCreated, map[string]any{
		"id": reportId, "name": name, "description": description, "page_type": pageType, "filters": filters,
	})
}

func apiDeleteReport(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	reportId := r.PathValue("report_id")
	report := getReport(db, reportId)
	if report == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	version := time.Now().UnixMilli()
	filtersJson, _ := json.Marshal(report["filters"])
	_, _ = insertRowsJsonEachRow(db, "sobs_reports", []Row{
		{
			"Id":          reportId,
			"Name":        report["name"],
			"Description": report["description"],
			"PageType":    report["page_type"],
			"FiltersJson": string(filtersJson),
			"IsDeleted":   1,
			"Version":     version,
		},
	})
	jsonResponse(w, http.StatusOK, map[string]any{"deleted": true})
}

// reportsExportVersion: export schema version for forward-compatibility.
const reportsExportVersion = "1"

// reportsImportMax: maximum number of reports importable in a single request.
const reportsImportMax = 500

// reportsImportMaxBytes: maximum raw request body size accepted by report import.
const reportsImportMaxBytes = 5 * 1024 * 1024

func apiExportReports(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	rawIds := strings.TrimSpace(r.URL.Query().Get("ids"))
	var reports []map[string]any
	if rawIds != "" {
		wanted := map[string]struct{}{}
		for _, s := range strings.Split(rawIds, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				wanted[s] = struct{}{}
			}
		}
		allReports := getReports(db, "")
		for _, rep := range allReports {
			if _, ok := wanted[rowString(rep["id"])]; ok {
				reports = append(reports, rep)
			}
		}
	} else {
		reports = getReports(db, "")
	}

	exportReports := make([]map[string]any, 0, len(reports))
	for _, rep := range reports {
		exportReports = append(exportReports, map[string]any{
			"id":          rep["id"],
			"name":        rep["name"],
			"description": rep["description"],
			"page_type":   rep["page_type"],
			"filters":     rep["filters"],
		})
	}
	payload := map[string]any{
		"sobs_reports_export": true,
		"version":             reportsExportVersion,
		"exported_at":         time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"reports":             exportReports,
	}
	jsonBytes, _ := json.MarshalIndent(payload, "", "  ")
	filename := "sobs_reports_export.json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(jsonBytes)
}

// reportImportTruthy mirrors Python truthiness for the export envelope flag.
func reportImportTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, _ := t.Float64()
		return f != 0
	case float64:
		return t != 0
	default:
		return true
	}
}

func reportPyRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return "'" + t + "'"
	default:
		return fmt.Sprintf("%v", t)
	}
}

func apiImportReports(w http.ResponseWriter, r *http.Request) {
	payloadTooLarge := func() {
		jsonResponse(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error": fmt.Sprintf("Import payload too large (max %d bytes)", reportsImportMaxBytes),
		})
	}

	if r.ContentLength > reportsImportMaxBytes {
		payloadTooLarge()
		return
	}

	contentType := r.Header.Get("Content-Type")
	onConflict := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("on_conflict")))

	var body map[string]any

	if strings.Contains(contentType, "multipart/form-data") || strings.Contains(contentType, "application/x-www-form-urlencoded") {
		if err := r.ParseMultipartForm(reportsImportMaxBytes); err != nil {
			_ = r.ParseForm()
		}
		if onConflict == "" {
			formVal := r.FormValue("on_conflict")
			if formVal == "" {
				formVal = "rename"
			}
			onConflict = strings.ToLower(strings.TrimSpace(formVal))
		}
		file, _, ferr := r.FormFile("file")
		if ferr != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "No file uploaded"})
			return
		}
		defer func() { _ = file.Close() }()
		rawBytes, rerr := io.ReadAll(io.LimitReader(file, reportsImportMaxBytes+1))
		if rerr == nil && int64(len(rawBytes)) > reportsImportMaxBytes {
			payloadTooLarge()
			return
		}
		if rerr != nil || json.Unmarshal(rawBytes, &body) != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "Invalid JSON file"})
			return
		}
	} else {
		parsed, err := readJsonBody(r)
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "Invalid or missing JSON body"})
			return
		}
		body = parsed
		if onConflict == "" {
			bodyVal := rowString(body["on_conflict"])
			if bodyVal == "" {
				bodyVal = "rename"
			}
			onConflict = strings.ToLower(strings.TrimSpace(bodyVal))
		}
	}

	// ── Validate envelope ──────────────────────────────────────────────────
	if !reportImportTruthy(body["sobs_reports_export"]) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "Not a valid SOBS reports export file"})
		return
	}
	if rowString(body["version"]) != reportsExportVersion {
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("Unsupported export version: %s", reportPyRepr(body["version"])),
		})
		return
	}
	if onConflict != "rename" && onConflict != "replace" && onConflict != "skip" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "on_conflict must be one of: rename, replace, skip"})
		return
	}

	incoming, listOk := body["reports"].([]any)
	if !listOk {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "'reports' must be a list"})
		return
	}
	if len(incoming) > reportsImportMax {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("Too many reports (max %d)", reportsImportMax)})
		return
	}

	// ── Build index of existing reports by (page_type, lower(name)) ────────
	db := getDb()
	existing := getReports(db, "")
	indexKey := func(pt, nm string) string { return pt + "\x00" + strings.ToLower(nm) }
	existingIndex := map[string]map[string]any{}
	for _, rep := range existing {
		existingIndex[indexKey(rowString(rep["page_type"]), rowString(rep["name"]))] = rep
	}

	nImported := 0
	nSkipped := 0
	nReplaced := 0
	nErrors := 0
	versionBase := time.Now().UnixMilli()

	for idx, rawItem := range incoming {
		item, ok := rawItem.(map[string]any)
		if !ok {
			nErrors++
			continue
		}
		name := strings.TrimSpace(rowString(item["name"]))
		description := strings.TrimSpace(rowString(item["description"]))
		pageType := strings.TrimSpace(rowString(item["page_type"]))
		filters, filtersOk := item["filters"].(map[string]any)
		if item["filters"] == nil {
			filters = map[string]any{}
			filtersOk = true
		}

		if name == "" {
			nErrors++
			continue
		}
		if _, valid := reportPageTypes[pageType]; !valid {
			nErrors++
			continue
		}
		if !filtersOk {
			nErrors++
			continue
		}

		conflictKey := indexKey(pageType, name)
		conflict := existingIndex[conflictKey]

		isReplace := false
		if conflict != nil {
			if onConflict == "skip" {
				nSkipped++
				continue
			} else if onConflict == "replace" {
				// Soft-delete the existing report.
				conflictFiltersJson, _ := json.Marshal(conflict["filters"])
				_, _ = insertRowsJsonEachRow(db, "sobs_reports", []Row{
					{
						"Id":          conflict["id"],
						"Name":        conflict["name"],
						"Description": conflict["description"],
						"PageType":    conflict["page_type"],
						"FiltersJson": string(conflictFiltersJson),
						"IsDeleted":   1,
						"Version":     versionBase + int64(idx)*2,
					},
				})
				nReplaced++
				isReplace = true
				delete(existingIndex, conflictKey)
			} else {
				// rename – find a unique name.
				candidate := fmt.Sprintf("%s (imported)", name)
				suffix := 2
				for {
					if _, exists := existingIndex[indexKey(pageType, candidate)]; !exists {
						break
					}
					candidate = fmt.Sprintf("%s (imported %d)", name, suffix)
					suffix++
				}
				name = candidate
			}
		}

		newId := agentUuid4()
		filtersJson, _ := json.Marshal(filters)
		_, _ = insertRowsJsonEachRow(db, "sobs_reports", []Row{
			{
				"Id":          newId,
				"Name":        name,
				"Description": description,
				"PageType":    pageType,
				"FiltersJson": string(filtersJson),
				"IsDeleted":   0,
				"Version":     versionBase + int64(idx)*2 + 1,
			},
		})
		// Track freshly-imported name to avoid collisions within the same batch.
		existingIndex[indexKey(pageType, name)] = map[string]any{"id": newId, "name": name, "page_type": pageType}
		if conflict != nil && isReplace {
			// Replacement inserts are counted as replaced, not imported.
			continue
		}
		nImported++
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"imported": nImported,
		"skipped":  nSkipped,
		"replaced": nReplaced,
		"errors":   nErrors,
	})
}

// ---------------------------------------------------------------------------
// Static RUM script
// ---------------------------------------------------------------------------

// rumEtag returns a hex ETag based on the file content (deterministic cache busting).
func rumEtag(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "0"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16]
}

// rumStaticDir resolves the static asset directory. Delegates to staticDir()
// (s00d_static.go) so it works whether run from gosobs/, the repo root, or
// alongside the executable — moduleDir() alone points at the binary's temp dir
// under `go run`.
func rumStaticDir() string { return staticDir() }

func rumJs(w http.ResponseWriter, r *http.Request) {
	staticDir := rumStaticDir()
	etag := rumEtag(filepath.Join(staticDir, "rum.js"))
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	w.Header().Set("X-SourceMap", "rum.js.map")
	w.Header().Set("SourceMap", "rum.js.map")
	http.ServeFile(w, r, filepath.Join(staticDir, "rum.js"))
}

func rumJsMap(w http.ResponseWriter, r *http.Request) {
	staticDir := rumStaticDir()
	mapPath := filepath.Join(staticDir, "rum.js.map")
	if st, err := os.Stat(mapPath); err != nil || st.IsDir() {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	http.ServeFile(w, r, mapPath)
}

func rumMinJs(w http.ResponseWriter, r *http.Request) {
	staticDir := rumStaticDir()
	etag := rumEtag(filepath.Join(staticDir, "rum.min.js"))
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, etag))
	http.ServeFile(w, r, filepath.Join(staticDir, "rum.min.js"))
}

func rumMinJsMap(w http.ResponseWriter, r *http.Request) {
	staticDir := rumStaticDir()
	w.Header().Set("Content-Type", "application/json")
	http.ServeFile(w, r, filepath.Join(staticDir, "rum.min.js.map"))
}

func rumDTs(w http.ResponseWriter, r *http.Request) {
	staticDir := rumStaticDir()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.ServeFile(w, r, filepath.Join(staticDir, "rum.d.ts"))
}

// ---------------------------------------------------------------------------
// Settings / Config  GET /settings
// ---------------------------------------------------------------------------

// viewSettings renders the settings/config hub page linking to tag rules,
// metrics rules, and other config.
func viewSettings(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	tagRules, _ := loadTagRules(db)
	anomalyRules, _ := loadAnomalyRules(db)
	agentRules := loadAgentRules(db)
	aiSettings := loadAllAiSettings(db)
	notificationChannels := loadNotificationChannels(db)
	notificationRules := loadNotificationRules(db)
	k8sSettings := loadK8sSettings(db)
	maskingSettings := loadMaskingSettings(db)
	backupEnabled := func() string {
		v := getAppSetting(db, "data_management.backup_enabled")
		if v == "" {
			return "0"
		}
		return v
	}() == "1"

	renderTemplate(w, r, "settings.html", map[string]any{
		"tag_rule_count":               len(tagRules),
		"anomaly_rule_count":           len(anomalyRules),
		"agent_rule_count":             len(agentRules),
		"ai_configured":                aiSettings["ai.endpoint_url"] != "" && aiSettings["ai.model"] != "",
		"notification_channel_count":   len(notificationChannels),
		"notification_rule_count":      len(notificationRules),
		"masking_custom_key_count":     len(toStringSlice(maskingSettings["custom_keys"])),
		"masking_custom_pattern_count": len(toStringSlice(maskingSettings["custom_patterns"])),
		"kubernetes_view_enabled":      k8sSettings["kubernetes.enabled"] == "1",
		"backup_enabled":               backupEnabled,
		"query_allowed_tables":         sortedStringSet(queryAllowedTables),
	})
}

// ---------------------------------------------------------------------------
// Masking settings pages / APIs
// ---------------------------------------------------------------------------

func viewMaskingSettings(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	settings := loadMaskingSettings(db)
	renderTemplate(w, r, "settings_masking.html", map[string]any{
		"custom_keys":                settings["custom_keys"],
		"custom_patterns":            settings["custom_patterns"],
		"default_keys":               settings["default_keys"],
		"default_patterns":           settings["default_patterns"],
		"effective_key_count":        len(toStringSlice(settings["effective_keys"])),
		"effective_pattern_count":    len(toStringSlice(settings["effective_patterns"])),
		"output_masking_enabled":     settings["output_masking_enabled"],
		"sql_output_masking_enabled": settings["sql_output_masking_enabled"],
	})
}

func addMaskingKey(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	db := getDb()
	key := maskingNormalizeSensitiveKey(r.FormValue("key"))
	settings := loadMaskingSettings(db)
	if key == "" {
		flashMessage(w, r, "Sensitive key name is required", "warning")
		http.Redirect(w, r, "/settings/masking", http.StatusFound)
		return
	}
	if containsStr(toStringSlice(settings["effective_keys"]), key) {
		flashMessage(w, r, fmt.Sprintf("Sensitive key '%s' is already active", key), "info")
		http.Redirect(w, r, "/settings/masking", http.StatusFound)
		return
	}
	customKeys := append(toStringSlice(settings["custom_keys"]), key)
	saveMaskingCustomKeys(db, customKeys)
	refreshMaskingRuntimeRules(db)
	flashMessage(w, r, fmt.Sprintf("Sensitive key '%s' added", key), "success")
	http.Redirect(w, r, "/settings/masking", http.StatusFound)
}

func deleteMaskingKey(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	db := getDb()
	key := maskingNormalizeSensitiveKey(r.FormValue("key"))
	settings := loadMaskingSettings(db)
	if !containsStr(toStringSlice(settings["custom_keys"]), key) {
		flashMessage(w, r, "Custom sensitive key not found", "warning")
		http.Redirect(w, r, "/settings/masking", http.StatusFound)
		return
	}
	customKeys := []string{}
	for _, item := range toStringSlice(settings["custom_keys"]) {
		if item != key {
			customKeys = append(customKeys, item)
		}
	}
	saveMaskingCustomKeys(db, customKeys)
	refreshMaskingRuntimeRules(db)
	flashMessage(w, r, fmt.Sprintf("Sensitive key '%s' removed", key), "success")
	http.Redirect(w, r, "/settings/masking", http.StatusFound)
}

func addMaskingPattern(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	db := getDb()
	rawPattern := r.FormValue("pattern")
	settings := loadMaskingSettings(db)
	pattern, err := validateCustomMaskingPatternForStorage(rawPattern)
	if err != nil {
		flashMessage(w, r, fmt.Sprintf("Invalid regex pattern: %s", err), "warning")
		http.Redirect(w, r, "/settings/masking", http.StatusFound)
		return
	}
	if containsStr(toStringSlice(settings["effective_patterns"]), pattern) {
		flashMessage(w, r, "That regex pattern is already active", "info")
		http.Redirect(w, r, "/settings/masking", http.StatusFound)
		return
	}
	customPatterns := append(toStringSlice(settings["custom_patterns"]), pattern)
	saveMaskingCustomPatterns(db, customPatterns)
	refreshMaskingRuntimeRules(db)
	flashMessage(w, r, "Custom masking pattern added", "success")
	http.Redirect(w, r, "/settings/masking", http.StatusFound)
}

func deleteMaskingPattern(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	db := getDb()
	rawPattern := r.FormValue("pattern")
	settings := loadMaskingSettings(db)
	pattern, err := validateCustomMaskingPatternForStorage(rawPattern)
	if err != nil {
		flashMessage(w, r, "Custom masking pattern not found", "warning")
		http.Redirect(w, r, "/settings/masking", http.StatusFound)
		return
	}
	if !containsStr(toStringSlice(settings["custom_patterns"]), pattern) {
		flashMessage(w, r, "Custom masking pattern not found", "warning")
		http.Redirect(w, r, "/settings/masking", http.StatusFound)
		return
	}
	customPatterns := []string{}
	for _, item := range toStringSlice(settings["custom_patterns"]) {
		if item != pattern {
			customPatterns = append(customPatterns, item)
		}
	}
	saveMaskingCustomPatterns(db, customPatterns)
	refreshMaskingRuntimeRules(db)
	flashMessage(w, r, "Custom masking pattern removed", "success")
	http.Redirect(w, r, "/settings/masking", http.StatusFound)
}

func updateMaskingOutputSetting(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	db := getDb()
	enabled := false
	for _, value := range formList(r, "enabled") {
		if isTruthySetting(value, false) {
			enabled = true
			break
		}
	}
	flag := "0"
	if enabled {
		flag = "1"
	}
	setAppSetting(db, maskingOutputEnabledSetting, flag)
	msg := "Global output masking disabled across UI/JSON/notifications/GitHub issue payloads"
	if enabled {
		msg = "Global output masking enabled"
	}
	flashMessage(w, r, msg, "success")
	http.Redirect(w, r, "/settings/masking", http.StatusFound)
}

func updateMaskingSqlOutputSetting(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	db := getDb()
	// Browser submissions can send both hidden and checkbox values for the same
	// field name. Treat the toggle as enabled if any submitted value is truthy.
	enabled := false
	for _, value := range formList(r, "enabled") {
		if isTruthySetting(value, false) {
			enabled = true
			break
		}
	}
	flag := "0"
	if enabled {
		flag = "1"
	}
	setAppSetting(db, maskingSqlOutputEnabledSetting, flag)
	msg := "SQL output masking disabled for NLQ/chart endpoints"
	if enabled {
		msg = "SQL output masking enabled for NLQ/chart endpoints"
	}
	flashMessage(w, r, msg, "success")
	http.Redirect(w, r, "/settings/masking", http.StatusFound)
}

func apiMaskingPreview(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	value := payload["value"]
	var masked any
	switch value.(type) {
	case map[string]any, []any:
		masked = maskValueForOutput(value, getDb())
	default:
		masked = maskStringForOutput(value, getDb())
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "masked": masked})
}

func apiMaskingRules(w http.ResponseWriter, r *http.Request) {
	settings := loadMaskingSettings(getDb())
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":                         true,
		"keys":                       settings["effective_keys"],
		"patterns":                   settings["effective_patterns"],
		"custom_keys":                settings["custom_keys"],
		"custom_patterns":            settings["custom_patterns"],
		"output_masking_enabled":     settings["output_masking_enabled"],
		"sql_output_masking_enabled": settings["sql_output_masking_enabled"],
	})
}

// ---------------------------------------------------------------------------
// Tag Rules  GET/POST /settings/tags
// ---------------------------------------------------------------------------

func viewTagRules(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	openPanel := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("open_panel")))
	if openPanel != "auto-tags" {
		openPanel = ""
	}
	rules, _ := loadTagRules(db)
	editRuleId := strings.TrimSpace(r.URL.Query().Get("edit_rule"))
	var editRule map[string]any
	if editRuleId != "" {
		for _, rule := range rules {
			if rowString(rule["id"]) == editRuleId {
				editRule = rule
				break
			}
		}
		if editRule == nil {
			flashMessage(w, r, "Tag rule not found for editing", "warning")
		}
	}
	services, _ := listTagCandidateServices(db)
	renderTemplate(w, r, "settings_tags.html", map[string]any{
		"rules":           rules,
		"edit_rule":       editRule,
		"record_types":    tagRuleRecordTypes,
		"match_fields":    tagRuleFields,
		"match_operators": tagRuleOperators,
		"services":        services,
		"auto_preview":    []any{},
		"auto_summary":    nil,
		"auto_open_panel": openPanel,
	})
}

func apiTagRuleConditionSuggestions(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	query := r.URL.Query()
	scope := strings.ToLower(strings.TrimSpace(orDefault(query.Get("scope"), "tag_rule")))
	field := strings.ToLower(strings.TrimSpace(query.Get("field")))
	operator := strings.ToLower(strings.TrimSpace(orDefault(query.Get("operator"), "eq")))
	queryText := strings.TrimSpace(query.Get("q"))
	attrKey := strings.TrimSpace(query.Get("attr_key"))
	source := strings.ToLower(strings.TrimSpace(query.Get("source")))
	signal := strings.TrimSpace(query.Get("signal"))
	recordType := strings.ToLower(strings.TrimSpace(orDefault(query.Get("record_type"), "all")))
	tagKey := strings.TrimSpace(query.Get("tag_key"))
	target := strings.ToLower(strings.TrimSpace(orDefault(query.Get("target"), "value")))

	limit := 8
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			limit = v
		}
	}
	if limit < 3 {
		limit = 3
	}
	if limit > 20 {
		limit = 20
	}

	suggestions := []string{}
	if scope == "tag_rule" {
		if target == "attr_key" {
			suggestions, _ = tagRuleAttributeKeySuggestions(db, queryText, limit)
		} else {
			suggestions, _ = tagRuleValueSuggestions(db, field, operator, queryText, attrKey, limit)
		}
	} else {
		switch target {
		case "service":
			suggestions, _ = notificationConditionServiceSuggestions(db, queryText, limit, source, signal)
		case "tag_key":
			suggestions, _ = recordTagKeySuggestions(db, queryText, limit, recordType)
		case "tag_value":
			suggestions, _ = recordTagValueSuggestions(db, tagKey, queryText, limit, recordType)
		}
	}
	if suggestions == nil {
		suggestions = []string{}
	}

	maskedJsonResponse(w, http.StatusOK, map[string]any{
		"ok":          true,
		"scope":       scope,
		"field":       field,
		"operator":    operator,
		"target":      target,
		"suggestions": suggestions,
	})
}

// orDefault returns v when non-empty, otherwise def (mirrors Python `x or y`).
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// joinAnyList joins a list-valued any (coerced via toStringSlice) with commas.
func joinAnyList(v any) string {
	return strings.Join(toStringSlice(v), ",")
}

func autoTagRules(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	action := strings.ToLower(strings.TrimSpace(orDefault(r.FormValue("action"), "preview")))

	hours := 24
	if raw := strings.TrimSpace(r.FormValue("hours")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			hours = v
		}
	}
	if hours < 1 {
		hours = 1
	}
	if hours > 168 {
		hours = 168
	}

	minCount := 30
	if raw := strings.TrimSpace(r.FormValue("min_count")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			minCount = v
		}
	}
	if minCount < 1 {
		minCount = 1
	}
	if minCount > 5000 {
		minCount = 5000
	}

	serviceFilter := strings.TrimSpace(r.FormValue("service_filter"))
	selectedRecordTypes := []string{}
	for _, rt := range formList(r, "auto_record_types") {
		if strings.TrimSpace(rt) != "" {
			selectedRecordTypes = append(selectedRecordTypes, strings.ToLower(strings.TrimSpace(rt)))
		}
	}
	if len(selectedRecordTypes) == 0 {
		selectedRecordTypes = []string{"log", "trace", "error", "ai", "rum"}
	}

	db := getDb()
	rules, _ := loadTagRules(db)
	services, _ := listTagCandidateServices(db)

	candidates, stats, _ := buildAutoTagRuleCandidates(db, hours, minCount, serviceFilter, selectedRecordTypes)

	summary := map[string]any{
		"action":         action,
		"hours":          hours,
		"min_count":      minCount,
		"service_filter": serviceFilter,
		"record_types":   selectedRecordTypes,
		"examined":       stats["examined"],
		"existing":       stats["existing"],
		"invalid":        stats["invalid"],
		"candidates":     len(candidates),
		"create_cap":     autoTagRuleCreateMax,
		"capped":         len(candidates) > autoTagRuleCreateMax,
		"created":        0,
	}

	if action == "create" {
		end := len(candidates)
		if end > autoTagRuleCreateMax {
			end = autoTagRuleCreateMax
		}
		limitedCandidates := candidates[:end]
		version := time.Now().UnixMilli()
		rowsToInsert := []Row{}
		for idx, candidate := range limitedCandidates {
			conditionsJson, _ := json.Marshal([]map[string]any{
				{
					"match_field":    rowString(candidate["match_field"]),
					"match_operator": rowString(candidate["match_operator"]),
					"match_value":    rowString(candidate["match_value"]),
					"match_attr_key": rowString(candidate["match_attr_key"]),
				},
			})
			rowsToInsert = append(rowsToInsert, Row{
				"Id":             agentUuid4(),
				"Name":           rowString(candidate["name"]),
				"RecordTypes":    joinAnyList(candidate["record_types"]),
				"MatchField":     rowString(candidate["match_field"]),
				"MatchOperator":  rowString(candidate["match_operator"]),
				"MatchValue":     rowString(candidate["match_value"]),
				"MatchAttrKey":   rowString(candidate["match_attr_key"]),
				"TagKey":         rowString(candidate["tag_key"]),
				"TagValue":       rowString(candidate["tag_value"]),
				"ConditionsJson": string(conditionsJson),
				"IsDeleted":      0,
				"Version":        version + int64(idx),
			})
		}
		if len(rowsToInsert) > 0 {
			_, _ = insertRowsJsonEachRow(db, "sobs_tag_rules", rowsToInsert)
		}
		summary["created"] = len(rowsToInsert)
		skippedByCap := len(candidates) - len(limitedCandidates)
		if skippedByCap < 0 {
			skippedByCap = 0
		}
		capSuffix := "."
		if skippedByCap > 0 {
			capSuffix = fmt.Sprintf(", skipped %d by max cap (%d).", skippedByCap, autoTagRuleCreateMax)
		}
		flashMessage(w, r, fmt.Sprintf(
			"Auto tag rule generation complete: created %d rule(s), skipped %v existing, %v invalid%s",
			summary["created"], summary["existing"], summary["invalid"], capSuffix), "success")
		http.Redirect(w, r, "/settings/tags?open_panel=auto-tags", http.StatusFound)
		return
	}

	flashMessage(w, r, fmt.Sprintf(
		"Auto-tag preview: %v candidate(s), %v existing skipped, %v invalid.",
		summary["candidates"], summary["existing"], summary["invalid"]), "info")
	renderTemplate(w, r, "settings_tags.html", map[string]any{
		"rules":           rules,
		"record_types":    tagRuleRecordTypes,
		"match_fields":    tagRuleFields,
		"match_operators": tagRuleOperators,
		"services":        services,
		"auto_preview":    candidates,
		"auto_summary":    summary,
		"auto_open_panel": "auto-tags",
	})
}

func createTagRule(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	editRuleId := strings.TrimSpace(r.FormValue("edit_rule_id"))
	redirectEndpoint := "/settings/tags"
	if editRuleId != "" {
		redirectEndpoint = "/settings/tags?edit_rule=" + editRuleId
	}
	name := strings.TrimSpace(r.FormValue("name"))
	recordTypesList := formList(r, "record_types")
	tagKey := strings.TrimSpace(r.FormValue("tag_key"))
	tagValue := strings.TrimSpace(r.FormValue("tag_value"))

	// --- Composite conditions ----------------------------------------------
	// The form may submit multiple conditions via parallel lists. When at least
	// two conditions are present the rule is "composite"; a single condition is
	// stored both as ConditionsJson and in the legacy Match* columns.
	condFields := formList(r, "condition_field")
	condOperators := formList(r, "condition_operator")
	condValues := formList(r, "condition_value")
	condAttrKeys := formList(r, "condition_attr_key")

	n := len(condFields)
	if len(condOperators) > n {
		n = len(condOperators)
	}
	if len(condValues) > n {
		n = len(condValues)
	}
	if len(condAttrKeys) > n {
		n = len(condAttrKeys)
	}

	getAt := func(lst []string, i int) string {
		if i < len(lst) {
			return strings.TrimSpace(lst[i])
		}
		return ""
	}

	conditions := []map[string]any{}
	for i := 0; i < n; i++ {
		f := strings.ToLower(getAt(condFields, i))
		op := strings.ToLower(getAt(condOperators, i))
		if op == "" {
			op = "eq"
		}
		val := getAt(condValues, i)
		attr := getAt(condAttrKeys, i)
		if f != "" {
			conditions = append(conditions, map[string]any{
				"match_field": f, "match_operator": op, "match_value": val, "match_attr_key": attr,
			})
		}
	}

	// Fall back to single-condition fields if no composite conditions supplied.
	if len(conditions) == 0 {
		matchField := strings.ToLower(strings.TrimSpace(r.FormValue("match_field")))
		matchOperator := strings.ToLower(strings.TrimSpace(orDefault(r.FormValue("match_operator"), "eq")))
		matchValue := strings.TrimSpace(r.FormValue("match_value"))
		matchAttrKey := strings.TrimSpace(r.FormValue("match_attr_key"))
		if matchField != "" {
			conditions = []map[string]any{
				{"match_field": matchField, "match_operator": matchOperator, "match_value": matchValue, "match_attr_key": matchAttrKey},
			}
		}
	}

	if name == "" || len(conditions) == 0 || tagKey == "" || tagValue == "" {
		flashMessage(w, r, "Name, at least one match condition, tag key, and tag value are required", "warning")
		http.Redirect(w, r, redirectEndpoint, http.StatusFound)
		return
	}

	for _, cond := range conditions {
		if !containsStr(tagRuleFields, rowString(cond["match_field"])) {
			flashMessage(w, r, fmt.Sprintf("Invalid match field: %s", rowString(cond["match_field"])), "warning")
			http.Redirect(w, r, redirectEndpoint, http.StatusFound)
			return
		}
		if !containsStr(tagRuleOperators, rowString(cond["match_operator"])) {
			flashMessage(w, r, fmt.Sprintf("Invalid match operator: %s", rowString(cond["match_operator"])), "warning")
			http.Redirect(w, r, redirectEndpoint, http.StatusFound)
			return
		}
		if rowString(cond["match_field"]) == "attribute" && rowString(cond["match_attr_key"]) == "" {
			flashMessage(w, r, "Attribute key is required when match field is 'attribute'", "warning")
			http.Redirect(w, r, redirectEndpoint, http.StatusFound)
			return
		}
		if rowString(cond["match_operator"]) == "regex" {
			if _, err := regexp.Compile(rowString(cond["match_value"])); err != nil {
				flashMessage(w, r, fmt.Sprintf("Invalid regex pattern: %s", err), "warning")
				http.Redirect(w, r, redirectEndpoint, http.StatusFound)
				return
			}
		}
	}

	// Normalise record types.
	chosen := []string{}
	for _, t := range recordTypesList {
		ts := strings.TrimSpace(t)
		if containsStr(tagRuleRecordTypes, ts) {
			chosen = append(chosen, ts)
		}
	}
	recordTypesStr := "all"
	if len(chosen) > 0 {
		recordTypesStr = strings.Join(chosen, ",")
	}

	// For the legacy single-condition columns use the first condition.
	primary := conditions[0]

	ruleId := agentUuid4()
	if editRuleId != "" {
		res, err := getDb().Execute(
			"SELECT Id FROM sobs_tag_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
			editRuleId,
		)
		var existingRow Row
		if err == nil {
			existingRow = res.Fetchone()
		}
		if existingRow == nil {
			flashMessage(w, r, "Tag rule not found for editing", "warning")
			http.Redirect(w, r, "/settings/tags", http.StatusFound)
			return
		}
		ruleId = rowString(existingRow["Id"])
	}

	conditionsJson, _ := json.Marshal(conditions)
	_, _ = insertRowsJsonEachRow(getDb(), "sobs_tag_rules", []Row{
		{
			"Id":             ruleId,
			"Name":           name,
			"RecordTypes":    recordTypesStr,
			"MatchField":     primary["match_field"],
			"MatchOperator":  primary["match_operator"],
			"MatchValue":     primary["match_value"],
			"MatchAttrKey":   primary["match_attr_key"],
			"TagKey":         tagKey,
			"TagValue":       tagValue,
			"ConditionsJson": string(conditionsJson),
			"IsDeleted":      0,
			"Version":        time.Now().UnixMilli(),
		},
	})
	verb := "created"
	if editRuleId != "" {
		verb = "updated"
	}
	flashMessage(w, r, fmt.Sprintf("Tag rule '%s' %s", name, verb), "success")
	http.Redirect(w, r, "/settings/tags", http.StatusFound)
}

func deleteTagRule(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	ruleId := r.PathValue("rule_id")

	deletedRow := func(row Row) Row {
		return Row{
			"Id":             ruleId,
			"Name":           rowString(row["Name"]),
			"RecordTypes":    "",
			"MatchField":     "",
			"MatchOperator":  "eq",
			"MatchValue":     "",
			"MatchAttrKey":   "",
			"TagKey":         "",
			"TagValue":       "",
			"ConditionsJson": "[]",
		}
	}

	softDeleteLatestRow(
		w, r, db,
		"SELECT Id, Name FROM sobs_tag_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
		[]any{ruleId},
		"sobs_tag_rules",
		deletedRow,
		"Tag rule not found",
		"Tag rule '{name}' deleted",
		"view_tag_rules",
		"warning",
		"success",
	)
}

// ---------------------------------------------------------------------------
// Record Tags API  GET/POST /api/tags/<record_type>/<record_id>
//                  DELETE /api/tags/<record_type>/<record_id>/<tag_key>
// ---------------------------------------------------------------------------

func apiGetTags(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	recordType := r.PathValue("record_type")
	recordId := r.PathValue("record_id")
	tags, _ := getRecordTags(db, recordType, recordId)
	jsonResponse(w, http.StatusOK, map[string]any{"tags": tags})
}

func apiAddTag(w http.ResponseWriter, r *http.Request) {
	recordType := r.PathValue("record_type")
	recordId := r.PathValue("record_id")
	payload, _ := readJsonBody(r)
	tagKey := strings.TrimSpace(rowString(payload["key"]))
	tagValue := strings.TrimSpace(rowString(payload["value"]))
	if tagKey == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "key is required"})
		return
	}
	if len([]rune(tagKey)) > 128 || len([]rune(tagValue)) > 512 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "tag key or value too long"})
		return
	}
	_, _ = insertRowsJsonEachRow(getDb(), "sobs_record_tags", []Row{
		{
			"RecordType": recordType,
			"RecordId":   recordId,
			"TagKey":     tagKey,
			"TagValue":   tagValue,
			"IsAuto":     0,
			"IsDeleted":  0,
			"Version":    time.Now().UnixMilli(),
		},
	})
	jsonResponse(w, http.StatusCreated, map[string]any{"ok": true})
}

func apiDeleteTag(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	recordType := r.PathValue("record_type")
	recordId := r.PathValue("record_id")
	tagKey := r.PathValue("tag_key")
	res, err := db.Execute(
		"SELECT TagKey, TagValue, IsAuto FROM sobs_record_tags FINAL "+
			"WHERE RecordType = ? AND RecordId = ? AND TagKey = ? AND IsDeleted = 0",
		recordType, recordId, tagKey,
	)
	var rows []Row
	if err == nil {
		rows = res.Fetchall()
	}
	if len(rows) == 0 {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "tag not found"})
		return
	}
	tombstones := []Row{}
	version := time.Now().UnixMilli()
	seenValues := map[string]struct{}{}
	for _, row := range rows {
		tagVal := rowString(row["TagValue"])
		isAuto := coerceInt(row["IsAuto"])
		dedupeKey := tagVal + "\x00" + strconv.Itoa(isAuto)
		if _, ok := seenValues[dedupeKey]; ok {
			continue
		}
		seenValues[dedupeKey] = struct{}{}
		tombstones = append(tombstones, Row{
			"RecordType": recordType,
			"RecordId":   recordId,
			"TagKey":     tagKey,
			"TagValue":   tagVal,
			"IsAuto":     isAuto,
			"IsDeleted":  1,
			"Version":    version,
		})
		version++
	}
	_, _ = insertRowsJsonEachRow(db, "sobs_record_tags", tombstones)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Log Field Hints API  GET /api/logs/field-hints
// Returns available otel_logs field names (with user-friendly aliases),
// sample values for enum-like fields, and active tag keys for the log type.
// Used by the SQL filter autocomplete on the Logs page.
// ---------------------------------------------------------------------------
func apiLogsFieldHints(w http.ResponseWriter, r *http.Request) {
	db := getDb()

	fields := []any{
		map[string]any{"name": "level", "column": "SeverityText", "type": "string", "values": []any{}},
		map[string]any{"name": "service", "column": "ServiceName", "type": "string", "values": []any{}},
		map[string]any{"name": "body", "column": "Body", "type": "string", "values": []any{}},
		map[string]any{"name": "trace_id", "column": "TraceId", "type": "string", "values": []any{}},
		map[string]any{"name": "span_id", "column": "SpanId", "type": "string", "values": []any{}},
		map[string]any{"name": "ts", "column": "Timestamp", "type": "datetime", "values": []any{}},
		map[string]any{"name": "EventName", "column": "EventName", "type": "string", "values": []any{}},
		map[string]any{"name": "ScopeName", "column": "ScopeName", "type": "string", "values": []any{}},
	}

	attrKeys, _ := getCachedLogAttrKeys(db, "log")

	// Active tag keys for logs (used in has_tag() suggestions)
	tagKeys := []string{}
	tagValues := map[string][]string{}
	tagKeyRes, tagKeyErr := db.Execute(
		"SELECT DISTINCT TagKey FROM sobs_record_tags FINAL " +
			"WHERE RecordType='log' AND IsDeleted=0 ORDER BY TagKey LIMIT 100")
	if tagKeyErr == nil {
		for _, row := range tagKeyRes.Fetchall() {
			tagKeys = append(tagKeys, rowString(row["TagKey"]))
		}
		// For each tag key, also fetch distinct values (cap at 20)
		for _, tk := range tagKeys {
			vals := []string{}
			valRes, valErr := db.Execute(
				"SELECT DISTINCT TagValue FROM sobs_record_tags FINAL "+
					"WHERE RecordType='log' AND TagKey=? AND IsDeleted=0 ORDER BY TagValue LIMIT 20",
				tk)
			if valErr != nil {
				tagKeys = []string{}
				tagValues = map[string][]string{}
				break
			}
			for _, vr := range valRes.Fetchall() {
				vals = append(vals, rowString(vr["TagValue"]))
			}
			tagValues[tk] = vals
		}
	}

	operators := []string{"=", "!=", "LIKE", "NOT LIKE", "ILIKE", "NOT ILIKE", "IN", "NOT IN", ">", "<", ">=", "<="}
	keywords := []string{"AND", "OR", "NOT", "IS NULL", "IS NOT NULL", "TRUE", "FALSE", "NULL"}
	functions := []any{
		map[string]any{"name": "has_tag", "signature": "has_tag('key','value')", "kind": "tag"},
		map[string]any{"name": "match", "signature": "match(body, 'regex')", "kind": "string"},
		map[string]any{"name": "positionCaseInsensitive", "signature": "positionCaseInsensitive(body, 'needle')", "kind": "string"},
		map[string]any{"name": "startsWith", "signature": "startsWith(service, 'api')", "kind": "string"},
		map[string]any{"name": "endsWith", "signature": "endsWith(service, 'worker')", "kind": "string"},
		map[string]any{"name": "lower", "signature": "lower(service)", "kind": "string"},
		map[string]any{"name": "upper", "signature": "upper(level)", "kind": "string"},
		map[string]any{"name": "toString", "signature": "toString(ts)", "kind": "cast"},
		map[string]any{"name": "toDateTime", "signature": "toDateTime('2026-03-30 12:00:00')", "kind": "datetime"},
	}
	snippets := []any{
		map[string]any{"label": "level='ERROR'", "insert": "level='ERROR'", "kind": "predicate"},
		map[string]any{"label": "service IN ('api','worker')", "insert": "service IN ('api','worker')", "kind": "predicate"},
		map[string]any{"label": "has_tag('env','prod')", "insert": "has_tag('env','prod')", "kind": "predicate"},
		map[string]any{"label": "match(body, 'timeout')", "insert": "match(body, 'timeout')", "kind": "predicate"},
		map[string]any{
			"label":  "ts >= toDateTime('2026-03-30 00:00:00')",
			"insert": "ts >= toDateTime('2026-03-30 00:00:00')",
			"kind":   "predicate",
		},
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"fields":     fields,
		"attr_keys":  attrKeys,
		"tag_keys":   tagKeys,
		"tag_values": tagValues,
		"operators":  operators,
		"keywords":   keywords,
		"functions":  functions,
		"snippets":   snippets,
	})
}

// filterTrailingOperatorRe matches a filter ending with an operator/keyword.
var filterTrailingOperatorRe = regexp.MustCompile(`(?i)\b(AND|OR|NOT|IN|LIKE|ILIKE)\s*$`)

// Validate a SQL WHERE fragment used by /logs?sql=... and return actionable feedback.
func apiLogsValidateFilter(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	sqlWhere := strings.TrimSpace(rowString(payload["sql"]))
	if sqlWhere == "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "normalized": "", "issues": []any{}})
		return
	}

	issues := []any{}

	// Lightweight structural checks for instant, helpful feedback.
	quoteOpen := false
	parenDepth := 0
	runes := []rune(sqlWhere)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		if ch == '\'' {
			if i+1 < len(runes) && runes[i+1] == '\'' {
				i++
				continue
			}
			quoteOpen = !quoteOpen
		} else if !quoteOpen {
			if ch == '(' {
				parenDepth++
			} else if ch == ')' {
				parenDepth--
				if parenDepth < 0 {
					issues = append(issues, map[string]any{"level": "error", "message": "Unexpected ')' in filter."})
					break
				}
			}
		}
	}

	if quoteOpen {
		issues = append(issues, map[string]any{"level": "error", "message": "Unclosed single quote in filter."})
	}
	if parenDepth > 0 {
		issues = append(issues, map[string]any{"level": "error", "message": "Unclosed '(' in filter."})
	}
	if filterTrailingOperatorRe.MatchString(sqlWhere) {
		issues = append(issues, map[string]any{"level": "warning", "message": "Filter ends with an operator or keyword."})
	}

	if err := validateUserSqlWhere(sqlWhere); err != nil {
		issues = append(issues, map[string]any{"level": "error", "message": publicDashboardQueryError(err)})
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "normalized": "", "issues": issues})
		return
	}
	safeSql := strings.ReplaceAll(sqlWhere, ";", "")
	for _, sub := range logsSqlAliasSubs {
		safeSql = sub.re.ReplaceAllString(safeSql, sub.repl)
	}
	safeSql = logsHasTagRe.ReplaceAllStringFunc(safeSql, translateLogsHasTag)

	db := getDb()
	// Existence probe is much cheaper than aggregate count() for live typing validation.
	if _, err := db.Execute("SELECT 1 FROM otel_logs WHERE " + safeSql + " LIMIT 1"); err != nil {
		issues = append(issues, map[string]any{"level": "error", "message": publicDashboardQueryError(err)})
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "normalized": "", "issues": issues})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "normalized": safeSql, "issues": issues})
}

// ---------------------------------------------------------------------------
// Regex Validate API helpers
// ---------------------------------------------------------------------------
const (
	regexSampleMaxLen           = 200
	regexScopeMaxLen            = 200
	regexValidateRecentHours    = 24
	regexValidateCandidateLimit = 2000
)

// truncateSample truncates a regex sample match to a displayable length.
func truncateSample(sample any) any {
	if s, ok := sample.(string); ok && len([]rune(s)) > regexSampleMaxLen {
		r := []rune(s)
		return string(r[:regexSampleMaxLen-3]) + "..."
	}
	return sample
}

// regexScopeText reads a bounded text value from regex validation scope payload.
func regexScopeText(scope map[string]any, key string, maxLen int) string {
	raw := strings.TrimSpace(rowString(scope[key]))
	if raw == "" {
		return ""
	}
	r := []rune(raw)
	if len(r) > maxLen {
		return string(r[:maxLen])
	}
	return raw
}

// regexScopeTimeConditions uses the requested time window when valid; otherwise
// defaults to a recent bounded window.
func regexScopeTimeConditions(scope map[string]any, column string) ([]string, []any) {
	fromTs := ""
	toTs := ""

	fromRaw := regexScopeText(scope, "from_ts", 64)
	toRaw := regexScopeText(scope, "to_ts", 64)
	if fromRaw != "" {
		fromTs = normalizeChTimestamp(fromRaw)
	}
	if toRaw != "" {
		toTs = normalizeChTimestamp(toRaw)
	}

	conditions, params := timeWindowConditions(column, fromTs, toTs)
	if len(conditions) == 0 {
		return []string{column + " >= now() - INTERVAL ? HOUR"}, []any{regexValidateRecentHours}
	}
	return conditions, params
}

func parseAndValidateRegexExpressionForApi(db *ChDbConnection, expression string) ([]string, []string, string) {
	includePatterns, excludePatterns, regexError := prepareRe2FilterPatterns(db, expression)
	if regexError != "" {
		return []string{}, []string{}, strings.Replace(regexError, "Regex error: ", "", 1)
	}
	return includePatterns, excludePatterns, ""
}

// regexBestEffortSample returns a bounded sample match by probing only recent
// candidate rows.
func regexBestEffortSample(db *ChDbConnection, fromSql, sampleColumn, orderColumn string, includePatterns, excludePatterns []string, whereParts []string, whereParams []any) (any, error) {
	whereSql := ""
	if len(whereParts) > 0 {
		whereSql = "WHERE " + strings.Join(whereParts, " AND ")
	}
	regexConditions := []string{}
	regexParams := []any{}
	appendRegexExpressionClauses(&regexConditions, &regexParams, "sample_value", includePatterns, excludePatterns)
	regexWhereSql := ""
	if len(regexConditions) > 0 {
		regexWhereSql = "WHERE " + strings.Join(regexConditions, " AND ")
	}
	sql := "SELECT sample_value FROM (" +
		fmt.Sprintf("SELECT %s AS sample_value FROM %s ", sampleColumn, fromSql) +
		fmt.Sprintf("%s ORDER BY %s DESC LIMIT ?", whereSql, orderColumn) +
		") " +
		regexWhereSql + " LIMIT 1"
	params := append(append(append([]any{}, whereParams...), regexValidateCandidateLimit), regexParams...)
	res, err := db.Execute(sql, params...)
	if err != nil {
		return nil, err
	}
	row := res.Fetchone()
	if row == nil {
		return truncateSample(nil), nil
	}
	return truncateSample(row["sample_value"]), nil
}

// regexValidateScope coerces the "scope" payload key into a dict (asyncio
// isinstance(scope, dict) guard).
func regexValidateScope(payload map[string]any) map[string]any {
	if scope, ok := payload["scope"].(map[string]any); ok {
		return scope
	}
	return map[string]any{}
}

// ---------------------------------------------------------------------------
// Logs Regex Validate API  POST /api/logs/validate-regex
// Used by the regex autocomplete / IntelliSense on the Logs filter panel.
// ---------------------------------------------------------------------------
// Validate a regex pattern used by /logs?q=... and return a sample match.
func apiLogsValidateRegex(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	pattern := strings.TrimSpace(rowString(payload["pattern"]))
	scope := regexValidateScope(payload)
	if pattern == "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": nil})
		return
	}

	db := getDb()
	includePatterns, excludePatterns, expressionError := parseAndValidateRegexExpressionForApi(db, pattern)
	if expressionError != "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": expressionError, "sample": nil})
		return
	}

	// Attempt a cheap LIMIT 1 probe to surface a real sample match.
	whereParts := []string{}
	whereParams := []any{}

	service := regexScopeText(scope, "service", regexScopeMaxLen)
	level := regexScopeText(scope, "level", regexScopeMaxLen)
	traceId := regexScopeText(scope, "trace_id", 64)

	if service != "" {
		whereParts = append(whereParts, "ServiceName = ?")
		whereParams = append(whereParams, service)
	}
	if level != "" {
		whereParts = append(whereParts, "SeverityText = ?")
		whereParams = append(whereParams, level)
	}
	if traceId != "" {
		whereParts = append(whereParts, "TraceId = ?")
		whereParams = append(whereParams, traceId)
	}

	timeParts, timeParams := regexScopeTimeConditions(scope, "Timestamp")
	whereParts = append(whereParts, timeParts...)
	whereParams = append(whereParams, timeParams...)

	sample, err := regexBestEffortSample(db, "otel_logs", "Body", "Timestamp",
		includePatterns, excludePatterns, whereParts, whereParams)
	if err != nil {
		maskedJsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": nil})
		return
	}
	maskedJsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": sample})
}

// ---------------------------------------------------------------------------
// Errors Regex Validate API  POST /api/errors/validate-regex
// Used by the regex autocomplete / IntelliSense on the Errors filter panel.
// ---------------------------------------------------------------------------
// Validate a regex pattern used by /errors?q=... and return a sample match.
func apiErrorsValidateRegex(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	pattern := strings.TrimSpace(rowString(payload["pattern"]))
	scope := regexValidateScope(payload)
	if pattern == "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": nil})
		return
	}

	db := getDb()
	includePatterns, excludePatterns, expressionError := parseAndValidateRegexExpressionForApi(db, pattern)
	if expressionError != "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": expressionError, "sample": nil})
		return
	}

	whereParts := []string{}
	whereParams := []any{}

	service := regexScopeText(scope, "service", regexScopeMaxLen)
	if service != "" {
		whereParts = append(whereParts, "ServiceName = ?")
		whereParams = append(whereParams, service)
	}

	timeParts, timeParams := regexScopeTimeConditions(scope, "Timestamp")
	whereParts = append(whereParts, timeParts...)
	whereParams = append(whereParams, timeParams...)

	sample, err := regexBestEffortSample(db, "("+errorSourcesSql+")", "Body", "Timestamp",
		includePatterns, excludePatterns, whereParts, whereParams)
	if err != nil {
		maskedJsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": nil})
		return
	}
	maskedJsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": sample})
}

// ---------------------------------------------------------------------------
// Traces Regex Validate API  POST /api/traces/validate-regex
// Used by the regex autocomplete / IntelliSense on the Traces filter panel.
// ---------------------------------------------------------------------------
// Validate a regex pattern used by /traces?q=... and return a sample match.
func apiTracesValidateRegex(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	pattern := strings.TrimSpace(rowString(payload["pattern"]))
	scope := regexValidateScope(payload)
	if pattern == "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": nil})
		return
	}

	db := getDb()
	includePatterns, excludePatterns, expressionError := parseAndValidateRegexExpressionForApi(db, pattern)
	if expressionError != "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": expressionError, "sample": nil})
		return
	}

	whereParts := []string{}
	whereParams := []any{}

	service := regexScopeText(scope, "service", regexScopeMaxLen)
	traceId := regexScopeText(scope, "trace_id", 64)
	if service != "" {
		whereParts = append(whereParts, "ServiceName = ?")
		whereParams = append(whereParams, service)
	}
	if traceId != "" {
		whereParts = append(whereParts, "TraceId = ?")
		whereParams = append(whereParams, traceId)
	}

	timeParts, timeParams := regexScopeTimeConditions(scope, "Timestamp")
	whereParts = append(whereParts, timeParts...)
	whereParams = append(whereParams, timeParams...)

	sample, err := regexBestEffortSample(db, "otel_traces", "SpanName", "Timestamp",
		includePatterns, excludePatterns, whereParts, whereParams)
	if err != nil {
		maskedJsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": nil})
		return
	}
	maskedJsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": sample})
}

// ---------------------------------------------------------------------------
// Metrics Regex Validate API  POST /api/metrics/validate-regex
// Used by the regex autocomplete / IntelliSense on the Metrics filter panel.
// ---------------------------------------------------------------------------
// Validate a regex pattern used by /metrics?q=... and return a sample match.
func apiMetricsValidateRegex(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	pattern := strings.TrimSpace(rowString(payload["pattern"]))
	scope := regexValidateScope(payload)
	if pattern == "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": nil})
		return
	}

	db := getDb()
	includePatterns, excludePatterns, expressionError := parseAndValidateRegexExpressionForApi(db, pattern)
	if expressionError != "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": expressionError, "sample": nil})
		return
	}

	whereParts := []string{}
	whereParams := []any{}

	service := regexScopeText(scope, "service", regexScopeMaxLen)
	source := regexScopeText(scope, "source", regexScopeMaxLen)
	signal := regexScopeText(scope, "signal", regexScopeMaxLen)
	attrFp := regexScopeText(scope, "attr_fp", 64)
	if service != "" {
		whereParts = append(whereParts, "ServiceName = ?")
		whereParams = append(whereParams, service)
	}
	if source != "" {
		whereParts = append(whereParts, "SignalSource = ?")
		whereParams = append(whereParams, source)
	}
	if signal != "" {
		whereParts = append(whereParts, "SignalName = ?")
		whereParams = append(whereParams, signal)
	}
	if attrFp != "" {
		whereParts = append(whereParts, "AttrFingerprint = ?")
		whereParams = append(whereParams, attrFp)
	}

	timeParts, timeParams := regexScopeTimeConditions(scope, "time")
	whereParts = append(whereParts, timeParts...)
	whereParams = append(whereParams, timeParams...)

	sample, err := regexBestEffortSample(db, "v_derived_signals_anomaly", "SignalName", "time",
		includePatterns, excludePatterns, whereParts, whereParams)
	if err != nil {
		maskedJsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": nil})
		return
	}
	maskedJsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": sample})
}

// ---------------------------------------------------------------------------
// RUM Regex Validate API  POST /api/rum/validate-regex
// Used by the regex autocomplete / IntelliSense on the RUM filter panel.
// ---------------------------------------------------------------------------
// Validate a regex pattern used by /rum?q=... and return a sample match.
func apiRumValidateRegex(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	pattern := strings.TrimSpace(rowString(payload["pattern"]))
	scope := regexValidateScope(payload)
	if pattern == "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": nil})
		return
	}

	db := getDb()
	includePatterns, excludePatterns, expressionError := parseAndValidateRegexExpressionForApi(db, pattern)
	if expressionError != "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "error": expressionError, "sample": nil})
		return
	}

	whereParts := []string{}
	whereParams := []any{}

	eventType := regexScopeText(scope, "type", regexScopeMaxLen)
	errorSource := regexScopeText(scope, "error_source", regexScopeMaxLen)
	if eventType != "" {
		whereParts = append(whereParts, "EventName = ?")
		whereParams = append(whereParams, eventType)
	}
	if errorSource != "" {
		whereParts = append(whereParts, "LogAttributes['errorSource'] = ?")
		whereParams = append(whereParams, errorSource)
	}

	timeParts, timeParams := regexScopeTimeConditions(scope, "Timestamp")
	whereParts = append(whereParts, timeParts...)
	whereParams = append(whereParams, timeParams...)

	sample, err := regexBestEffortSample(db, "hyperdx_sessions", "Body", "Timestamp",
		includePatterns, excludePatterns, whereParts, whereParams)
	if err != nil {
		maskedJsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": nil})
		return
	}
	maskedJsonResponse(w, http.StatusOK, map[string]any{"ok": true, "sample": sample})
}

// ---------------------------------------------------------------------------
// AI Field Hints API  GET /api/ai/field-hints
// Used by SQL filter autocomplete on the AI Transparency page.
// ---------------------------------------------------------------------------
func apiAiFieldHints(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	baseWhere := aiSpanCondition

	fields := []any{
		map[string]any{"name": "service", "column": "ServiceName", "type": "string", "values": []any{}},
		map[string]any{"name": "model", "column": "SpanAttributes['gen_ai.request.model']", "type": "string", "values": []any{}},
		map[string]any{"name": "provider", "column": "SpanAttributes['gen_ai.provider.name']", "type": "string", "values": []any{}},
		map[string]any{"name": "operation", "column": "SpanAttributes['gen_ai.operation.name']", "type": "string", "values": []any{}},
		map[string]any{"name": "prompt", "column": aiTracePromptSql, "type": "string", "values": []any{}},
		map[string]any{"name": "response", "column": aiTraceResponseSql, "type": "string", "values": []any{}},
		map[string]any{"name": "span_name", "column": "SpanName", "type": "string", "values": []any{}},
		map[string]any{
			"name":   "row_type",
			"column": "if(SpanAttributes['gen_ai.request.model'] != '', 'llm', 'system')",
			"type":   "string",
			"values": []any{"llm", "system"},
		},
		map[string]any{"name": "trace_id", "column": "TraceId", "type": "string", "values": []any{}},
		map[string]any{"name": "span_id", "column": "SpanId", "type": "string", "values": []any{}},
		map[string]any{"name": "ts", "column": "Timestamp", "type": "datetime", "values": []any{}},
		map[string]any{"name": "status", "column": "StatusCode", "type": "string", "values": []any{}},
		map[string]any{"name": "error_type", "column": "SpanAttributes['error.type']", "type": "string", "values": []any{}},
		map[string]any{"name": "tokens_in", "column": "toUInt64OrZero(SpanAttributes['gen_ai.usage.input_tokens'])", "type": "number", "values": []any{}},
		map[string]any{"name": "tokens_out", "column": "toUInt64OrZero(SpanAttributes['gen_ai.usage.output_tokens'])", "type": "number", "values": []any{}},
		map[string]any{"name": "thinking_tokens", "column": "toUInt64OrZero(SpanAttributes['gen_ai.usage.thinking_tokens'])", "type": "number", "values": []any{}},
		map[string]any{"name": "duration_ms", "column": "(Duration / 1000000.0)", "type": "number", "values": []any{}},
	}

	firstColStrings := func(res *ChDbResult) []string {
		out := []string{}
		if len(res.Cols) == 0 {
			return out
		}
		col := res.Cols[0]
		for _, row := range res.Fetchall() {
			out = append(out, rowString(row[col]))
		}
		return out
	}

	var services, models, providers, operations, spanNames, statusCodes, errorTypes []string
	var qErr error
	run := func(query string) []string {
		if qErr != nil {
			return nil
		}
		res, err := db.Execute(query)
		if err != nil {
			qErr = err
			return nil
		}
		return firstColStrings(res)
	}

	services = run(fmt.Sprintf("SELECT DISTINCT ServiceName FROM otel_traces WHERE %s "+
		"AND ServiceName != '' ORDER BY ServiceName LIMIT 40", baseWhere))
	models = run(fmt.Sprintf("SELECT DISTINCT SpanAttributes['gen_ai.request.model'] FROM otel_traces WHERE %s "+
		"AND SpanAttributes['gen_ai.request.model'] != '' "+
		"ORDER BY SpanAttributes['gen_ai.request.model'] LIMIT 40", baseWhere))
	providers = run(fmt.Sprintf("SELECT DISTINCT coalesce(SpanAttributes['gen_ai.provider.name'], SpanAttributes['gen_ai.system']) "+
		"FROM otel_traces WHERE %s "+
		"ORDER BY coalesce(SpanAttributes['gen_ai.provider.name'], SpanAttributes['gen_ai.system']) LIMIT 40", baseWhere))
	operations = run(fmt.Sprintf("SELECT DISTINCT SpanAttributes['gen_ai.operation.name'] FROM otel_traces WHERE %s "+
		"AND SpanAttributes['gen_ai.operation.name'] != '' "+
		"ORDER BY SpanAttributes['gen_ai.operation.name'] LIMIT 40", baseWhere))
	spanNames = run(fmt.Sprintf("SELECT DISTINCT SpanName FROM otel_traces WHERE %s "+
		"AND SpanName != '' ORDER BY SpanName LIMIT 60", baseWhere))
	statusCodes = run(fmt.Sprintf("SELECT DISTINCT StatusCode FROM otel_traces WHERE %s "+
		"AND StatusCode != '' ORDER BY StatusCode LIMIT 20", baseWhere))
	errorTypes = run(fmt.Sprintf("SELECT DISTINCT SpanAttributes['error.type'] FROM otel_traces WHERE %s "+
		"AND SpanAttributes['error.type'] != '' ORDER BY SpanAttributes['error.type'] LIMIT 40", baseWhere))

	if qErr != nil {
		services = []string{}
		models = []string{}
		providers = []string{}
		operations = []string{}
		spanNames = []string{}
		statusCodes = []string{}
		errorTypes = []string{}
	}

	valuesByField := map[string][]string{
		"service":    services,
		"model":      models,
		"provider":   providers,
		"operation":  operations,
		"span_name":  spanNames,
		"status":     statusCodes,
		"error_type": errorTypes,
	}
	for _, f := range fields {
		fld := f.(map[string]any)
		if vals, ok := valuesByField[fld["name"].(string)]; ok {
			fld["values"] = vals
		}
	}

	operators := []string{"=", "!=", "LIKE", "NOT LIKE", "ILIKE", "NOT ILIKE", "IN", "NOT IN", ">", "<", ">=", "<="}
	keywords := []string{"AND", "OR", "NOT", "IS NULL", "IS NOT NULL", "TRUE", "FALSE", "NULL"}
	functions := []any{
		map[string]any{"name": "match", "signature": "match(model, 'gpt')", "kind": "string"},
		map[string]any{"name": "startsWith", "signature": "startsWith(span_name, 'ai.tool')", "kind": "string"},
		map[string]any{"name": "endsWith", "signature": "endsWith(provider, 'cloud')", "kind": "string"},
		map[string]any{"name": "lower", "signature": "lower(model)", "kind": "string"},
		map[string]any{"name": "upper", "signature": "upper(operation)", "kind": "string"},
		map[string]any{"name": "toDateTime", "signature": "toDateTime('2026-03-30 12:00:00')", "kind": "datetime"},
	}
	snippets := []any{
		map[string]any{"label": "row_type='llm'", "insert": "row_type='llm'", "kind": "predicate"},
		map[string]any{"label": "row_type='system'", "insert": "row_type='system'", "kind": "predicate"},
		map[string]any{"label": "span_name='ai.tool.executed'", "insert": "span_name='ai.tool.executed'", "kind": "predicate"},
		map[string]any{"label": "prompt ILIKE '%graph%'", "insert": "prompt ILIKE '%graph%'", "kind": "predicate"},
		map[string]any{"label": "response ILIKE '%chart%'", "insert": "response ILIKE '%chart%'", "kind": "predicate"},
		map[string]any{"label": "tokens_out > 1000", "insert": "tokens_out > 1000", "kind": "predicate"},
		map[string]any{"label": "error_type != ''", "insert": "error_type != ''", "kind": "predicate"},
		map[string]any{
			"label":  "ts >= toDateTime('2026-03-30 00:00:00')",
			"insert": "ts >= toDateTime('2026-03-30 00:00:00')",
			"kind":   "predicate",
		},
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"fields":    fields,
		"operators": operators,
		"keywords":  keywords,
		"functions": functions,
		"snippets":  snippets,
	})
}

// Validate a SQL WHERE fragment used by /ai?sql=... and return actionable feedback.
func apiAiValidateFilter(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	sqlWhere := strings.TrimSpace(rowString(payload["sql"]))
	if sqlWhere == "" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "normalized": "", "issues": []any{}})
		return
	}

	issues := []any{}

	quoteOpen := false
	parenDepth := 0
	runes := []rune(sqlWhere)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		if ch == '\'' {
			if i+1 < len(runes) && runes[i+1] == '\'' {
				i++
				continue
			}
			quoteOpen = !quoteOpen
		} else if !quoteOpen {
			if ch == '(' {
				parenDepth++
			} else if ch == ')' {
				parenDepth--
				if parenDepth < 0 {
					issues = append(issues, map[string]any{"level": "error", "message": "Unexpected ')' in filter."})
					break
				}
			}
		}
	}

	if quoteOpen {
		issues = append(issues, map[string]any{"level": "error", "message": "Unclosed single quote in filter."})
	}
	if parenDepth > 0 {
		issues = append(issues, map[string]any{"level": "error", "message": "Unclosed '(' in filter."})
	}
	if filterTrailingOperatorRe.MatchString(sqlWhere) {
		issues = append(issues, map[string]any{"level": "warning", "message": "Filter ends with an operator or keyword."})
	}

	safeSql, normErr := normalizeAiSqlWhere(sqlWhere)
	if normErr == nil {
		db := getDb()
		_, normErr = db.Execute(
			"SELECT 1 FROM otel_traces " + fmt.Sprintf("WHERE (%s) ", safeSql) + fmt.Sprintf("AND %s ", aiSpanCondition) + "LIMIT 1")
	}
	if normErr != nil {
		issues = append(issues, map[string]any{"level": "error", "message": publicDashboardQueryError(normErr)})
		jsonResponse(w, http.StatusOK, map[string]any{"ok": false, "normalized": "", "issues": issues})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "normalized": safeSql, "issues": issues})
}

// ---------------------------------------------------------------------------
// SSE live tail  GET /tail
// ---------------------------------------------------------------------------
// Live-tail logs and traces as a Server-Sent Events stream.
//
// Query parameters:
//   - source: logs, traces, or all (default: all)
//   - service: optional service name filter (exact match)
//
// SSE event format:
//
//	data: {"source": "logs", "ts": "...", "level": "INFO", "service": "...", "body": "..."}
//
// Example usage:
//
//	curl -N http://localhost:44317/tail
//	curl -N "http://localhost:44317/tail?source=logs&service=myapp"
func tailStream(w http.ResponseWriter, r *http.Request) {
	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	if source == "" {
		source = "all"
	}
	serviceFilter := strings.TrimSpace(r.URL.Query().Get("service"))

	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonError(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	q := sseSubscribe()
	defer sseUnsubscribe(q)

	io.WriteString(w, "retry: 5000\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-q:
			if source != "all" && rowString(event["source"]) != source {
				continue
			}
			if serviceFilter != "" && rowString(event["service"]) != serviceFilter {
				continue
			}
			io.WriteString(w, "data: "+jsonDumpsNoEscape(event)+"\n\n")
			flusher.Flush()
		case <-time.After(15 * time.Second):
			io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
