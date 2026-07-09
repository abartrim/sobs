package main

import (
	"regexp"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---------------------------------------------------------------------------
// validate-filter helpers (app.py api_logs_validate_filter / api_ai_validate_filter)
// ---------------------------------------------------------------------------

// filterTrailingOpRE mirrors app.py's \b(AND|OR|NOT|IN|LIKE|ILIKE)\s*$ (case-insensitive) warning.
var filterTrailingOpRE = regexp.MustCompile(`(?i)\b(AND|OR|NOT|IN|LIKE|ILIKE)\s*$`)

// filterIssue builds a {"level":..,"message":..} issue object (key order is irrelevant — the
// route serializes with QuartJSONify, which sorts keys, exactly like Python's jsonify).
func filterIssue(level, message string) *jsonenc.Object {
	return jsonenc.NewObject().Set("level", level).Set("message", message)
}

// errString is a trivial error wrapper so a validator's string message can flow through
// publicDashboardQueryError — mirroring app.py's `except Exception as exc:
// _public_dashboard_query_error(exc)` where exc is the ValueError raised by _validate_user_sql_where.
type errString string

func (e errString) Error() string { return string(e) }

// logsSQLReplacements mirrors the ordered token replacements in app.py api_logs_validate_filter
// (level->SeverityText, service->ServiceName, trace_id->TraceId, span_id->SpanId, ts->Timestamp,
// body->Body). Python applies these via plain re.sub over the WHOLE string (NOT quote-masked).
var logsSQLReplacements = []sqlReplacement{
	{regexp.MustCompile(`(?i)\blevel\b`), "SeverityText"},
	{regexp.MustCompile(`(?i)\bservice\b`), "ServiceName"},
	{regexp.MustCompile(`(?i)\btrace_id\b`), "TraceId"},
	{regexp.MustCompile(`(?i)\bspan_id\b`), "SpanId"},
	{regexp.MustCompile(`(?i)\bts\b`), "Timestamp"},
	{regexp.MustCompile(`(?i)\bbody\b`), "Body"},
}

// logsHasTagRE mirrors app.py's has_tag(...) recognizer.
var logsHasTagRE = regexp.MustCompile(`(?i)has_tag\s*\(\s*'((?:[^']|'')+)'\s*,\s*'((?:[^']|'')*)'\s*\)`)

// normalizeLogsSQLWhere mirrors the safe_sql construction in app.py api_logs_validate_filter:
// strip ';', apply the token replacements, then rewrite has_tag('k','v') into the
// sobs_record_tags membership subquery. validateUserSQLWhere must be checked by the caller first
// (Python calls _validate_user_sql_where at the top of the try block).
func normalizeLogsSQLWhere(sqlWhere string) string {
	safe := strings.ReplaceAll(sqlWhere, ";", "")
	for _, r := range logsSQLReplacements {
		safe = r.re.ReplaceAllString(safe, r.repl)
	}
	safe = logsHasTagRE.ReplaceAllStringFunc(safe, func(match string) string {
		m := logsHasTagRE.FindStringSubmatch(match)
		// app.py: tag_key/tag_val = group.replace("''","'").replace("'","''")
		tagKey := strings.ReplaceAll(strings.ReplaceAll(m[1], "''", "'"), "'", "''")
		tagVal := strings.ReplaceAll(strings.ReplaceAll(m[2], "''", "'"), "'", "''")
		return "MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)) IN (" +
			"SELECT RecordId FROM sobs_record_tags FINAL " +
			"WHERE TagKey='" + tagKey + "' AND TagValue='" + tagVal + "' " +
			"AND IsDeleted=0 AND RecordType='log')"
	})
	return safe
}

// ---------------------------------------------------------------------------
// refine-chart helpers (app.py api_query_refine_chart)
// ---------------------------------------------------------------------------

// bstrOr mirrors `str(payload.get(key) or default).strip()` — a non-empty string value is used,
// otherwise (missing / nil / "" / falsy) the default is substituted, then trimmed.
func bstrOr(m map[string]any, key, def string) string {
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok && s != "" {
			return strings.TrimSpace(s)
		}
	}
	return strings.TrimSpace(def)
}

// refineChartSpecString mirrors app.py `current_spec = payload.get("chart_spec", "")` with the
// subsequent `if not current_spec` falsy-check. A string passes through verbatim (and is truthy
// when non-empty). A non-empty object/array is serialized to a JSON string for the refiner and is
// truthy. "", missing, nil, and empty containers are falsy. Returns (specString, truthy).
func refineChartSpecString(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, x != ""
	case map[string]any:
		if len(x) == 0 {
			return "", false
		}
		return string(jsonenc.Encode(toJSONObject(x), jsonDumpsDefault)), true
	case []any:
		if len(x) == 0 {
			return "", false
		}
		return string(jsonenc.Encode(x, jsonDumpsDefault)), true
	case bool:
		return "", x
	case float64:
		return "", x != 0
	default:
		return "", true
	}
}

// toJSONObject converts a json-decoded map[string]any into an insertion-ordered *jsonenc.Object so
// it serializes deterministically. (Order is irrelevant on the parity path — the LLM endpoint is
// unconfigured there — but keeps the serialized spec stable when AI is configured.)
func toJSONObject(m map[string]any) *jsonenc.Object {
	o := jsonenc.NewObject()
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			o.Set(k, toJSONObject(sub))
		} else {
			o.Set(k, v)
		}
	}
	return o
}

// vannaRefineChartSpecTL mirrors app.py _vanna_refine_chart_spec(..., thinking_level=thinking_level):
// validate the current spec, build the refinement system+user messages (spec + a data sample of up
// to 20 rows + the user instruction), ask the LLM, and return the re-serialized refined spec. The
// caller-supplied thinkingLevel (from the request body) is threaded into the LLM call instead of the
// stored ai.thinking_level setting. On the parity path the AI endpoint
// is the URL-keyed mock which ignores the body, so the canned response is unchanged.
func (s *server) vannaRefineChartSpecTL(endpoint, model, currentSpec, instruction string, columns, sampleRows []any, thinkingLevel string) (string, string) {
	if endpoint == "" || model == "" {
		return "", "AI endpoint not configured."
	}
	if _, err := parseJSONValue([]byte(currentSpec)); err != nil {
		return "", "Current chart spec is invalid JSON: " + err.Error()
	}
	rows := sampleRows
	if len(rows) > 20 {
		rows = rows[:20]
	}
	// app.py: json.dumps({"columns": columns, "rows": sample_rows[:20]}, ensure_ascii=False, default=str)
	// — default (spaced) separators ", " / ": ", NOT compact.
	sampleStr := string(jsonenc.Encode(jsonenc.NewObject().Set("columns", columns).Set("rows", rows), jsonDumpsDefault))
	userMsg := "Current ECharts spec structure:\n" + currentSpec +
		"\n\nData available (columns + up to 20 sample rows):\n" + sampleStr +
		"\n\nUser instruction: " + instruction +
		"\n\nPlease refine the chart spec to fulfill this request. Return only the updated JSON."
	messages := []any{
		jsonenc.NewObject().Set("role", "system").Set("content", s.buildChartRefinementPrompt()),
		jsonenc.NewObject().Set("role", "user").Set("content", userMsg),
	}
	baseReq := llmRequest{
		endpoint:      endpoint,
		model:         model,
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: thinkingLevel,
		maxTokens:     queryLLMMaxTokens, // app.py _vanna_refine_chart_spec: max_tokens=_QUERY_LLM_MAX_TOKENS (8192)
		messages:      messages,
	}
	content, _, err := s.callLLMChat(baseReq)
	if err != nil || content == "" {
		return "", "LLM did not return a refined chart spec."
	}
	parsed, _ := parseChartSpecJSON(content)
	if parsed != nil {
		return string(jsonenc.Encode(parsed, dumpsDefault)), ""
	}
	// app.py _vanna_refine_chart_spec: on parse failure call _repair_chart_spec_json_with_llm.
	// parseErr must match CPython JSONDecodeError format for the error message to be byte-identical.
	parseErr := chartSpecParseError(content)
	repairReq := baseReq
	repairReq.thinkingLevel = "off"
	repairReq.messages = []any{
		jsonenc.NewObject().Set("role", "system").Set("content", chartJSONRepairSystemPrompt),
		jsonenc.NewObject().Set("role", "user").Set("content",
			"The chart JSON below failed to parse. Repair it and return only valid JSON.\n\n"+
				"Parse error: "+parseErr+"\n\n"+
				"Malformed chart JSON:\n"+content),
	}
	repaired, _, rerr := s.callLLMChat(repairReq)
	if rerr != nil || strings.TrimSpace(repaired) == "" {
		// app.py repair returns None + "LLM JSON repair returned empty content." ->
		// _vanna_refine_chart_spec: "Refined chart spec JSON parse error: {parse_err}."
		// (no repair_error suffix since repaired_raw is falsy -> repair_error from stats,
		// which is empty on a normal empty-content return).
		return "", "Refined chart spec JSON parse error: " + parseErr + ". LLM JSON repair returned empty content."
	}
	rparsed, _ := parseChartSpecJSON(repaired)
	if rparsed == nil {
		repairErr := "LLM JSON repair output was still invalid: " + chartSpecParseError(repaired)
		return "", "Refined chart spec JSON parse error: " + parseErr + ". " + repairErr
	}
	return string(jsonenc.Encode(rparsed, dumpsDefault)), ""
}

// ---------------------------------------------------------------------------
// data-management backup helpers (app.py _dm_backup_enabled / _list_dm_backups /
// _validate_dm_backup_name / _validate_dm_s3_settings)
// ---------------------------------------------------------------------------

// DM S3 validation regexes (app.py:31815-31819). The backup-name regex reuses the existing
// dmBackupNameRE (handlers_mutations.go), which is byte-identical to app.py's _DM_BACKUP_NAME_RE.
var (
	dmS3EndpointRE   = regexp.MustCompile(`^[A-Za-z0-9:/._-]+$`)
	dmS3PrefixRE     = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)
	dmAWSRegionRE    = regexp.MustCompile(`^[a-z0-9-]*$`)
	dmAWSAccessKeyRE = regexp.MustCompile(`^[A-Za-z0-9]*$`)
	dmAWSSecretKeyRE = regexp.MustCompile(`^[A-Za-z0-9/+=]*$`)
)

// requireDmSafeValue mirrors app.py _require_dm_safe_value: a non-empty value that does not
// fullmatch the pattern raises "<field> contains unsupported characters".
func requireDmSafeValue(fieldName, value string, pattern *regexp.Regexp) error {
	if value != "" && !pattern.MatchString(value) {
		return errString(fieldName + " contains unsupported characters")
	}
	return nil
}

// validateDmBackupName mirrors app.py _validate_dm_backup_name (regex == _DM_BACKUP_NAME_RE).
func validateDmBackupName(backupName string) error {
	if !dmBackupNameRE.MatchString(backupName) {
		return errString("backup_name contains unsupported characters")
	}
	return nil
}

// dmBackupEnabled mirrors app.py _dm_backup_enabled: (_get_app_setting(...) or "0") == "1".
// Strictly "1" — NOT the {1,true,yes,on} truthiness of appSettingBool.
func (s *server) dmBackupEnabled() bool {
	v, _ := s.appSetting("data_management.backup_enabled")
	return strings.TrimSpace(v) == "1"
}

// dmBackupRow is the subset of system.backups needed for the incremental BASE_BACKUP lookup.
type dmBackupRow struct {
	name   string
	status string
}

// listDmBackups mirrors app.py _list_dm_backups: read system.backups ordered most-recent-first;
// any query error falls back to an empty list (the Python try/except). Only name+status are
// surfaced (the only fields the incremental base-backup selection consults).
func (s *server) listDmBackups() []dmBackupRow {
	res, err := s.db.Execute(
		"SELECT name, status, start_time, end_time, num_files, total_size, error " +
			"FROM system.backups ORDER BY start_time DESC LIMIT 100")
	if err != nil {
		return nil
	}
	out := make([]dmBackupRow, 0, len(res.Rows))
	for _, m := range rowMaps(res) {
		out = append(out, dmBackupRow{
			name:   cStrDef(m, "name", ""),
			status: cStrDef(m, "status", ""),
		})
	}
	return out
}
