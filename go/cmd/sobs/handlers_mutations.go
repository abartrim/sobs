package main

import (
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Mutation handlers (POST/DELETE/PATCH). The parity manifest sends the simplest request
// that yields a DETERMINISTIC response (no generated uuid/timestamp/cookie) — usually an
// empty body hitting a validation/no-op branch.

// handleValidateRegex (POST /api/{logs,errors,traces,metrics,rum}/validate-regex) is defined
// in regex_validate.go — it validates the regex expression and probes a best-effort sample.

// handleApiQueryAsk is defined in query_exec.go (guard + NL→SQL + execute).

// handleApiQueryRun is defined in query_exec.go (full SQL exec + telemetry).

// POST /api/query/refine-chart — app.py api_query_refine_chart: refine an ECharts spec via one
// LLM call (canned in parity). trace_id/turn_id are uuids (masked).
func (s *server) handleApiQueryRefineChart(w http.ResponseWriter, r *http.Request) {
	if !s.queryPageEnabled() {
		s.writeMaskedJSON(w, http.StatusNotFound,
			jsonenc.NewObject().Set("ok", false).Set("error", "Query page is unavailable."))
		return
	}
	m := bodyMap(r)
	// app.py: current_spec = payload.get("chart_spec", "") with a falsy-check — a string passes
	// through verbatim, a non-empty object/dict passes (serialized to JSON for the refiner), and
	// "" / missing / an empty object/dict are falsy -> "No chart spec provided.".
	currentSpec, specTruthy := refineChartSpecString(m["chart_spec"])
	instruction := strings.TrimSpace(bstr(m, "instruction"))
	// _normalize_thinking_level(str(payload.get("thinking_level") or "off")) — read from the body,
	// NOT the stored ai.thinking_level setting.
	thinkingLevel := normalizeThinkingLevel(bstrOr(m, "thinking_level", "off"))
	if !specTruthy {
		s.writeMaskedJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", "No chart spec provided."))
		return
	}
	if instruction == "" {
		s.writeMaskedJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", "No instruction provided."))
		return
	}
	traceID := newUUIDv4()
	turnID := newUUIDv4()
	model := strings.TrimSpace(s.loadAISetting("ai.model", ""))
	endpoint := strings.TrimSpace(s.loadAISetting("ai.endpoint_url", ""))
	instrAttrs := map[string]string{"gen_ai.operation.name": "refine_chart", "sobs.gen_ai.instruction": instruction}
	s.emitAiHelperLogEvent("query.turn.start", traceID, turnID, "/query", model, "", "off",
		"Chart refinement requested: "+instruction, "INFO", instrAttrs)
	columns, _ := m["columns"].([]any)
	rows, _ := m["rows"].([]any)
	chartSpec, chartErr := s.vannaRefineChartSpecTL(endpoint, model, currentSpec, instruction, columns, rows, thinkingLevel)
	sev := "INFO"
	emitBody := chartSpec
	if chartErr != "" {
		sev, emitBody = "ERROR", chartErr
	}
	s.emitAiHelperLogEvent("query.chart.refined", traceID, turnID, "/query", model, "", "off", emitBody, sev, instrAttrs)
	s.emitAiHelperLogEvent("query.turn.complete", traceID, turnID, "/query", model, "", "off", "", sev,
		map[string]string{"gen_ai.operation.name": "refine_chart"})
	if chartErr != "" {
		s.writeMaskedJSON(w, http.StatusInternalServerError,
			jsonenc.NewObject().Set("ok", false).Set("error", chartErr).Set("trace_id", traceID))
		return
	}
	s.writeMaskedJSON(w, http.StatusOK,
		jsonenc.NewObject().Set("ok", true).Set("trace_id", traceID).Set("chart_spec", chartSpec))
}

// POST /api/query/add-to-dashboard — app.py api_query_add_to_dashboard: persist query SQL + a
// custom eCharts JSON as a custom_echarts chart on a dashboard. Deterministic (not query-gated).
func (s *server) handleApiQueryAddToDashboard(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := asObject(func() any { v, _ := parseJSONValue(raw); return v }())
	get := func(k string) string { v, ok := body.Get(k); return pyStrOrStrip(v, ok) }
	dashID, title, sql := get("dashboard_id"), get("title"), get("sql")
	chartSpecRaw, csOK := body.Get("chart_spec")
	if dashID == "" {
		s.errorJSON(w, http.StatusBadRequest, "dashboard_id is required")
		return
	}
	if sql == "" {
		s.errorJSON(w, http.StatusBadRequest, "sql is required")
		return
	}
	if !isTruthyVal(chartSpecRaw, csOK) {
		s.errorJSON(w, http.StatusBadRequest, "chart_spec is required")
		return
	}
	dres, err := s.db.Execute("SELECT Id, Name, Description FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Id = ?", dashID)
	if err != nil {
		s.dbError(w, err)
		return
	}
	if len(dres.Rows) == 0 {
		s.errorJSON(w, http.StatusNotFound, "Dashboard not found")
		return
	}
	dashName := cStr(rowMaps(dres)[0], "Name")
	if title == "" {
		title = "Query Chart"
	}
	// chart_option = json.loads(chart_spec) if it's a string, else the value itself.
	chartOption := chartSpecRaw
	if str, ok := chartSpecRaw.(string); ok {
		v, perr := parseJSONValue([]byte(str))
		if perr != nil {
			s.errorJSON(w, http.StatusBadRequest, "chart_spec must be valid JSON: "+perr.Error())
			return
		}
		chartOption = v
	}
	if _, ok := chartOption.(*jsonenc.Object); !ok {
		s.errorJSON(w, http.StatusBadRequest, "chart_spec must be a JSON object")
		return
	}
	specRaw := jsonenc.NewObject().
		Set("template_id", "custom_echarts").
		Set("sql", jsonenc.NewObject().Set("mode", "raw").Set("override_sql", sql)).
		Set("visual", jsonenc.NewObject().
			Set("custom_option_json", string(jsonenc.Encode(chartOption, jsonDumpsDefault))).
			Set("custom_mapping_json", "{}"))
	templateID, query, normSpec, errMsg := s.compileChartSpec(specRaw)
	if errMsg != "" {
		s.errorJSON(w, http.StatusBadRequest, "Chart spec error: "+errMsg)
		return
	}
	optionsJSON := string(jsonenc.Encode(jsonenc.NewObject().Set("chart_spec", normSpec), jsonDumpsDefault))
	chartID := newUUIDv4()
	row := map[string]any{
		"Id": chartID, "DashboardId": dashID, "Title": title, "ChartType": templateID,
		"Query": query, "OptionsJson": optionsJSON, "Position": s.nextChartPosition(dashID),
		"IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_chart_configs", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("chart_id", chartID).Set("dashboard_id", dashID).
		Set("dashboard_name", dashName).Set("dashboard_url", "/dashboards/"+dashID))
}

// POST /api/data-management/backup/run and /restore — app.py: 403 when the backup feature
// is disabled (default), else run the ClickHouse BACKUP/RESTORE. Without a configured S3 bucket
// both short-circuit to a deterministic message (the actual BACKUP ALL TO S3 needs real S3).
func (s *server) handleDmBackupGuard(w http.ResponseWriter, r *http.Request) {
	// app.py _dm_backup_enabled: (_get_app_setting(...) or "0") == "1" — strictly "1", NOT the
	// general {1,true,yes,on} truthiness appSettingBool accepts.
	if !s.dmBackupEnabled() {
		writeJSON(w, http.StatusForbidden,
			jsonenc.NewObject().Set("ok", false).Set("message", "Backup feature is disabled"))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/restore") {
		body := bodyMap(r)
		backupName := strings.TrimSpace(bstr(body, "backup_name"))
		if backupName != "" && !dmBackupNameRE.MatchString(backupName) {
			writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().
				Set("ok", false).Set("message", "backup_name contains unsupported characters"))
			return
		}
		ok, msg := s.runDmRestore(backupName)
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", ok).Set("message", msg))
		return
	}
	backupType := strings.ToLower(bstr(bodyMap(r), "type"))
	if backupType != "full" && backupType != "incremental" {
		backupType = "full"
	}
	ok, msg := s.runDmBackup(backupType)
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", ok).Set("message", msg))
}

var dmBackupNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)

// dmS3Bucket returns the configured S3 bucket (empty if unset).
func (s *server) dmS3Bucket() string {
	v, _ := s.appSetting("data_management.s3_bucket")
	return strings.TrimSpace(v)
}

// runDmBackup mirrors app.py _run_dm_backup: a missing S3 bucket short-circuits; otherwise a
// ClickHouse BACKUP ALL TO S3(...) is issued (reached only when S3 is configured), with an
// optional incremental BASE_BACKUP clause and an optional encryption SETTINGS clause.
func (s *server) runDmBackup(backupType string) (bool, string) {
	if s.dmS3Bucket() == "" {
		return false, "S3 bucket is not configured"
	}
	backupName := "sobs-" + backupType + "-" + nowUTC().Format("20060102T150405Z")
	dest, err := s.buildS3BackupDest(backupName)
	if err != nil {
		return false, err.Error()
	}

	// Incremental: BASE_BACKUP <dest of most recent completed sobs-* backup>.
	baseSQL := ""
	if backupType == "incremental" {
		for _, b := range s.listDmBackups() {
			if b.status == "BACKUP_COMPLETE" && strings.HasPrefix(b.name, "sobs-") {
				baseDest, berr := s.buildS3BackupDest(b.name)
				if berr != nil {
					return false, berr.Error()
				}
				baseSQL = ", BASE_BACKUP " + baseDest
				break
			}
		}
	}

	// Encryption: SETTINGS compression_method='lz4', encryption_password=<quoted>.
	encryptClause := ""
	if v, _ := s.appSetting("data_management.s3_encrypt_backup"); strings.TrimSpace(v) == "1" {
		encPassword := strings.TrimSpace(s.dmSettingValue("data_management.backup_encryption_password"))
		if encPassword == "" {
			return false, "Backup encryption is enabled but no encryption password is configured"
		}
		encryptClause = " SETTINGS compression_method='lz4', encryption_password=" + sqlQuoteLiteral(encPassword)
	}

	if _, err := s.db.Execute("BACKUP ALL TO " + dest + baseSQL + encryptClause); err != nil {
		return false, err.Error()
	}
	return true, "Backup '" + backupName + "' started successfully"
}

// runDmRestore mirrors app.py _run_dm_restore.
func (s *server) runDmRestore(backupName string) (bool, string) {
	if backupName == "" {
		return false, "backup_name is required"
	}
	if s.dmS3Bucket() == "" {
		return false, "S3 bucket is not configured"
	}
	dest, err := s.buildS3BackupDest(backupName)
	if err != nil {
		return false, err.Error()
	}
	if _, err := s.db.Execute("RESTORE ALL FROM " + dest); err != nil {
		return false, err.Error()
	}
	return true, "Restore from '" + backupName + "' started successfully"
}

// buildS3BackupDest mirrors app.py _build_s3_backup_dest (the S3(...) destination clause). It first
// validates the backup name and every S3 field (the _validate_dm_backup_name + _validate_dm_s3_settings
// regex guards) and returns an error ("<field> contains unsupported characters") on a violation — the
// Python ValueError path, surfaced to callers as {ok:false, message:<field> contains ...}.
func (s *server) buildS3BackupDest(backupName string) (string, error) {
	bucket := strings.TrimRight(s.dmS3Bucket(), "/")
	prefix := strings.Trim(strings.TrimSpace(s.appSettingOr("data_management.s3_path_prefix")), "/")
	region := strings.TrimSpace(s.appSettingOr("data_management.s3_region"))
	accessKey := strings.TrimSpace(s.appSettingOr("data_management.s3_access_key_id"))
	secretKey := strings.TrimSpace(s.dmSettingValue("data_management.s3_secret_access_key"))

	if err := validateDmBackupName(backupName); err != nil {
		return "", err
	}
	if err := requireDmSafeValue("s3_bucket", bucket, dmS3EndpointRE); err != nil {
		return "", err
	}
	if err := requireDmSafeValue("s3_path_prefix", prefix, dmS3PrefixRE); err != nil {
		return "", err
	}
	if err := requireDmSafeValue("s3_region", region, dmAWSRegionRE); err != nil {
		return "", err
	}
	if err := requireDmSafeValue("s3_access_key_id", accessKey, dmAWSAccessKeyRE); err != nil {
		return "", err
	}
	if err := requireDmSafeValue("s3_secret_access_key", secretKey, dmAWSSecretKeyRE); err != nil {
		return "", err
	}

	path := bucket + "/" + backupName
	if prefix != "" {
		path = bucket + "/" + prefix + "/" + backupName
	}
	endpoint := path
	if !strings.HasPrefix(path, "http") {
		if region != "" {
			endpoint = "https://s3." + region + ".amazonaws.com/" + path
		} else {
			endpoint = "https://s3.amazonaws.com/" + path
		}
	}
	if accessKey != "" && secretKey != "" {
		return "S3(" + sqlQuoteLiteral(endpoint) + ", " + sqlQuoteLiteral(accessKey) + ", " + sqlQuoteLiteral(secretKey) + ")", nil
	}
	return "S3(" + sqlQuoteLiteral(endpoint) + ")", nil
}

// sqlQuoteLiteral mirrors app.py _sql_quote_literal.
func sqlQuoteLiteral(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

// appSettingOr returns the app-setting value or empty string.
func (s *server) appSettingOr(key string) string {
	v, _ := s.appSetting(key)
	return v
}

// POST /api/{logs,ai}/validate-filter — app.py api_logs_validate_filter (app.py:23782) /
// api_ai_validate_filter (app.py:24405): validate a SQL WHERE fragment and return actionable
// feedback. The logs-vs-ai variant is distinguished by the request path (the two route
// registrations are /api/logs/validate-filter and /api/ai/validate-filter). The empty-sql case
// is preserved byte-for-byte ({"ok":true,"normalized":"","issues":[]}).
func (s *server) handleValidateFilter(w http.ResponseWriter, r *http.Request) {
	isAI := strings.Contains(r.URL.Path, "/ai/")
	sqlWhere := strings.TrimSpace(bstr(bodyMap(r), "sql"))
	if sqlWhere == "" {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().
			Set("ok", true).Set("normalized", "").Set("issues", []any{}))
		return
	}

	issues := []any{}

	// Lightweight structural checks (quote balance with '' escape, paren depth) — rune indexing
	// to mirror Python's codepoint-based string iteration.
	runes := []rune(sqlWhere)
	quoteOpen := false
	parenDepth := 0
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
					issues = append(issues, filterIssue("error", "Unexpected ')' in filter."))
					break
				}
			}
		}
	}
	if quoteOpen {
		issues = append(issues, filterIssue("error", "Unclosed single quote in filter."))
	}
	if parenDepth > 0 {
		issues = append(issues, filterIssue("error", "Unclosed '(' in filter."))
	}
	if filterTrailingOpRE.MatchString(sqlWhere) {
		issues = append(issues, filterIssue("warning", "Filter ends with an operator or keyword."))
	}

	var safeSQL string
	var probeErr error
	if isAI {
		safe, msg := normalizeAiSQLWhere(sqlWhere)
		if msg != "" {
			// _validate_user_sql_where raised (disallowed keyword) -> the except branch.
			probeErr = errString(msg)
		} else {
			safeSQL = safe
			_, probeErr = s.db.Execute(
				"SELECT 1 FROM otel_traces WHERE (" + safeSQL + ") AND " + aiSpanCondition + " LIMIT 1")
		}
	} else {
		if msg := validateUserSQLWhere(sqlWhere); msg != "" {
			probeErr = errString(msg)
		} else {
			safeSQL = normalizeLogsSQLWhere(sqlWhere)
			_, probeErr = s.db.Execute("SELECT 1 FROM otel_logs WHERE " + safeSQL + " LIMIT 1")
		}
	}

	if probeErr != nil {
		issues = append(issues, filterIssue("error", publicDashboardQueryError(probeErr)))
		writeJSON(w, http.StatusOK, jsonenc.NewObject().
			Set("ok", false).Set("normalized", "").Set("issues", issues))
		return
	}

	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("normalized", safeSQL).Set("issues", issues))
}
