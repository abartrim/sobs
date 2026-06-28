package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
)

var limitRe = regexp.MustCompile(`(?i)\bLIMIT\b`)
var codePrefixRe = regexp.MustCompile(`^Code:\s*\d+\.\s*DB::Exception:\s*`)
var dbExcRe = regexp.MustCompile(`^DB::Exception:\s*`)

// publicDashboardQueryError mirrors app.py _public_dashboard_query_error: a concise, user-safe
// one-line message from a chdb exception.
func publicDashboardQueryError(err error) string {
	raw := strings.TrimSpace(err.Error())
	message := strings.TrimSpace(strings.SplitN(raw, "\n", 2)[0])
	// chdb query errors are wrapped as "chdb query: <chdb error>" (internal/store/chdb.go); the
	// Python oracle's exception has no such wrapper, so strip it before the Code:/DB::Exception:
	// cleaning runs — otherwise the prefix defeats codePrefixRe and we leak the raw chdb text.
	for strings.HasPrefix(message, "chdb query: ") {
		message = strings.TrimPrefix(message, "chdb query: ")
	}
	message = codePrefixRe.ReplaceAllString(message, "")
	message = dbExcRe.ReplaceAllString(message, "")
	message = strings.TrimSpace(strings.SplitN(message, ": while executing function", 2)[0])
	message = strings.TrimSpace(strings.SplitN(message, ". Stack trace", 2)[0])
	if message == "" {
		return "Query execution failed"
	}
	if (strings.Contains(raw, "NO_COMMON_TYPE") || strings.Contains(raw, "TYPE_MISMATCH")) &&
		!strings.Contains(message, "Check casts and column types.") {
		message += ". Check casts and column types."
	}
	// app.py truncates by CODEPOINT: `len(message) > 280` and `message[:277].rstrip()` operate on
	// Python str (Unicode), so a message containing multibyte chars (the echoed SQL) truncates at a
	// different byte offset than Go's old byte-slice. Mirror with rune length + rune slice + a
	// Unicode-aware right-strip (Python str.rstrip()).
	if r := []rune(message); len(r) > 280 {
		message = strings.TrimRightFunc(string(r[:277]), unicode.IsSpace) + "..."
	}
	return message
}

// chBaseType strips Nullable(...)/LowCardinality(...) wrappers from a ClickHouse type.
func chBaseType(t string) string {
	for {
		if inner, ok := unwrapType(t, "Nullable"); ok {
			t = inner
			continue
		}
		if inner, ok := unwrapType(t, "LowCardinality"); ok {
			t = inner
			continue
		}
		return t
	}
}

func unwrapType(t, wrapper string) (string, bool) {
	if strings.HasPrefix(t, wrapper+"(") && strings.HasSuffix(t, ")") {
		return t[len(wrapper)+1 : len(t)-1], true
	}
	return "", false
}

// chQueryValue converts a FORMAT JSON cell value to the jsonenc-encodable form the Python chdb
// driver + jsonify would produce: Int/UInt -> unquoted integer (json.Number), Float -> float64
// (Python repr), everything else as decoded (string/bool/null).
func chQueryValue(v any, chType string) any {
	if v == nil {
		return nil
	}
	base := chBaseType(chType)
	switch {
	case strings.HasPrefix(base, "Int") || strings.HasPrefix(base, "UInt"):
		switch x := v.(type) {
		case float64:
			return json.Number(strconv.FormatInt(int64(x), 10))
		case string: // 64-bit ints arrive as JSON strings
			return json.Number(strings.TrimSpace(x))
		}
	case strings.HasPrefix(base, "Float"):
		switch x := v.(type) {
		case float64:
			return x
		case string:
			if f, err := strconv.ParseFloat(x, 64); err == nil {
				return f
			}
		}
	case strings.HasPrefix(base, "DateTime") || strings.HasPrefix(base, "Date"):
		// The Python chdb driver returns Date/DateTime cells as datetime objects. They jsonify as
		// an RFC-822 http_date, but the output-masking redact masks them to "****" (a datetime is
		// an "unhandled type" in redact). chDateTime carries both behaviours: it Stringer-encodes
		// as the http_date (identical bytes for unmasked routes) and redacts to MASK.
		if str, ok := v.(string); ok {
			if d := flaskHTTPDate(str); d != "" {
				return chDateTime{d}
			}
		}
	}
	return v
}

// chDateTime wraps a chdb Date/DateTime cell rendered as Flask's RFC-822 http_date. Its String()
// makes the jsonenc encoder's default case emit the http_date verbatim; the output-masking
// redact treats it as an unhandled type and masks it to "****" (matching Python's datetime).
type chDateTime struct{ s string }

func (d chDateTime) String() string { return d.s }

// flaskHTTPDate parses a chdb Date/DateTime string and renders Flask's http_date (UTC, RFC-822,
// no sub-second). Returns "" when unparseable.
func flaskHTTPDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	for _, layout := range drilldownTimeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format("Mon, 02 Jan 2006 15:04:05") + " GMT"
		}
	}
	// Date-only.
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC().Format("Mon, 02 Jan 2006 15:04:05") + " GMT"
	}
	return ""
}

// serializeQueryResult mirrors app.py's `columns = keys if rows else []` + row-as-list shaping.
func serializeQueryResult(res *store.Result) (columns []any, rows []any) {
	columns, rows = []any{}, []any{}
	if len(res.Rows) == 0 {
		return
	}
	for _, c := range res.Columns {
		columns = append(columns, c)
	}
	for _, row := range res.Rows {
		rec := []any{}
		for j, v := range row {
			t := ""
			if j < len(res.Types) {
				t = res.Types[j]
			}
			rec = append(rec, chQueryValue(v, t))
		}
		rows = append(rows, rec)
	}
	return
}

// serializeQueryDictRows mirrors the render handlers' shaping: columns = first row's keys (or
// [] when empty), rows = list-of-dicts (column name -> typed-serialized value).
func serializeQueryDictRows(res *store.Result) ([]any, []map[string]any) {
	if len(res.Rows) == 0 {
		return []any{}, []map[string]any{}
	}
	columns := make([]any, len(res.Columns))
	for i, c := range res.Columns {
		columns[i] = c
	}
	rows := make([]map[string]any, len(res.Rows))
	for i, row := range res.Rows {
		m := map[string]any{}
		for j, c := range res.Columns {
			t := ""
			if j < len(res.Types) {
				t = res.Types[j]
			}
			m[c] = chQueryValue(row[j], t)
		}
		rows[i] = m
	}
	return columns, rows
}

// injectLimit appends " LIMIT n" when the query has no LIMIT (mirrors the spec/query routes).
//
// Python is `query.rstrip(";") + f" LIMIT {n}"` (app.py:21781), and every call site pre-`.strip()`s
// the query, so the only structural difference is that Go also `TrimSpace`s leading whitespace here.
// For the pre-stripped inputs every caller actually passes, `TrimSpace`+`TrimRight(";")` is byte-
// identical to Python's `rstrip(";")`; it diverges only on a non-pre-stripped query with leading
// whitespace (which no caller produces) — both still block, no bypass. Left as a faithful superset.
func injectLimit(query string, n int) string {
	if limitRe.MatchString(query) {
		return query
	}
	return strings.TrimRight(strings.TrimSpace(query), ";") + " LIMIT " + strconv.Itoa(n)
}

// pythonTypeName maps a serialized cell value to the Python type name _infer_column_types reports
// (json.Number from Int columns -> "int", float64 -> "float", string -> "str", bool -> "bool").
func pythonTypeName(v any) string {
	switch x := v.(type) {
	case json.Number:
		s := x.String()
		if strings.ContainsAny(s, ".eE") {
			return "float"
		}
		return "int"
	case float64:
		return "float"
	case bool:
		return "bool"
	case string:
		return "str"
	default:
		return "str"
	}
}

// inferColumnTypes mirrors _infer_column_types: the type name of the first non-null value per
// column (or "null").
func inferColumnTypes(columns, rows []any) []any {
	out := []any{}
	for idx := range columns {
		detected := "null"
		for _, rowAny := range rows {
			row, ok := rowAny.([]any)
			if !ok || idx >= len(row) {
				continue
			}
			if row[idx] == nil {
				continue
			}
			detected = pythonTypeName(row[idx])
			break
		}
		out = append(out, detected)
	}
	return out
}

// executeNamedQueries mirrors _execute_chart_spec_named_queries (include_records=False): run each
// {name, sql, purpose} with a LIMIT and collect {name, purpose, columns, rows, error}.
func (s *server) executeNamedQueries(named []any, defaultLimit int) []any {
	results := []any{}
	for _, nqAny := range named {
		nq, ok := nqAny.(*jsonenc.Object)
		if !ok {
			continue
		}
		nameV, nameOK := nq.Get("name")
		name := pyStrOrStrip(nameV, nameOK)
		sqlV, sqlOK := nq.Get("sql")
		nqSQL := pyStrOrStrip(sqlV, sqlOK)
		purposeV, purposeOK := nq.Get("purpose")
		purpose := pyStr2(purposeV, purposeOK)
		if name == "" || nqSQL == "" {
			continue
		}
		item := jsonenc.NewObject().Set("name", name).Set("purpose", purpose)
		res, err := s.db.Execute(injectLimit(nqSQL, defaultLimit))
		if err != nil {
			results = append(results, item.Set("columns", []any{}).Set("rows", []any{}).
				Set("error", publicDashboardQueryError(err)))
			continue
		}
		cols, rows := serializeQueryResult(res)
		results = append(results, item.Set("columns", cols).Set("rows", rows).Set("error", ""))
	}
	return results
}

// pyStr2 mirrors str(x or "") without the .strip() (used for `purpose`).
func pyStr2(v any, present bool) string {
	if !present || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyStr(v, present)
}

// handleApiDashboardsSpecDryRun — app.py dry_run_chart_spec_api: compile, execute (LIMIT 20),
// infer column types, run named queries, and return all of it.
func (s *server) handleApiDashboardsSpecDryRun(w http.ResponseWriter, r *http.Request) {
	tid, query, spec, errMsg := s.compileChartSpec(specFromBody(r))
	if errMsg != "" {
		errorOnly(w, http.StatusBadRequest, errMsg)
		return
	}
	res, err := s.db.Execute(injectLimit(query, 20))
	if err != nil {
		errorOnly(w, http.StatusBadRequest, publicDashboardQueryError(err))
		return
	}
	columns, rows := serializeQueryResult(res)
	var named []any
	if nqV, ok := spec.Get("named_queries"); ok {
		named, _ = nqV.([]any)
	}
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("template_id", tid).Set("query", query).Set("spec", spec).
		Set("columns", columns).Set("column_types", inferColumnTypes(columns, rows)).
		Set("rows", rows).Set("named_query_results", s.executeNamedQueries(named, 5)))
}

// md5Hex16Plus returns the full hex md5 of s (used for query trace ids).
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// cellKind classifies one serialized cell into the coarse Python type _infer_query_field_types keys
// on: "int", "float", "bool", "datetime", "json" (dict/list/tuple), or "str".
func cellKind(v any) string {
	switch x := v.(type) {
	case json.Number:
		if strings.ContainsAny(x.String(), ".eE") {
			return "float"
		}
		return "int"
	case float64:
		return "float"
	case bool:
		return "bool"
	case chDateTime:
		return "datetime"
	case map[string]any, []any:
		// chdb Map -> map, Array/Tuple -> list; pandas keeps these as object cells -> kind "json".
		return "json"
	default:
		return "str"
	}
}

// inferQueryFieldTypes mirrors app.py _infer_query_field_types: per column it reports the whole-column
// pandas dtype string and a display kind. The dtype is what pandas infers from the chdb DB-API values:
// all-int + no null -> int64; all-int WITH a null -> float64 (nullable-int promotion); any float ->
// float64; all-bool + no null -> bool; all-datetime -> datetime64[ns]; a pure-string column (with or
// without nulls) -> "str" (pandas 3.x's inferred string dtype); Map/Array/Tuple (object) cells and
// empty/all-null columns -> object. For an object dtype the kind is then refined from the first
// non-null cell (bool/int/float/dict-list-tuple/str), so Map/Array/Tuple columns report kind "json".
func inferQueryFieldTypes(columns, rows []any) []any {
	out := []any{}
	for idx, col := range columns {
		hasNull := false
		hasValue := false
		allInt := true
		allNumeric := true // int or float
		allBool := true
		allDatetime := true
		allStr := true // every non-null cell is a plain string (pandas 3.x "str" dtype)
		var firstNonNull any
		haveFirst := false
		for _, rowAny := range rows {
			row, ok := rowAny.([]any)
			if !ok || idx >= len(row) {
				continue
			}
			cell := row[idx]
			if cell == nil {
				hasNull = true
				continue
			}
			hasValue = true
			if !haveFirst {
				firstNonNull, haveFirst = cell, true
			}
			switch cellKind(cell) {
			case "int":
				allBool, allDatetime, allStr = false, false, false
			case "float":
				allInt, allBool, allDatetime, allStr = false, false, false, false
			case "bool":
				allInt, allNumeric, allDatetime, allStr = false, false, false, false
			case "datetime":
				allInt, allNumeric, allBool, allStr = false, false, false, false
			case "json": // chdb Map/Array/Tuple -> object cells -> pandas "object" dtype
				allInt, allNumeric, allBool, allDatetime, allStr = false, false, false, false, false
			default: // str -> pandas 3.x infers the "str" dtype (allStr stays true)
				allInt, allNumeric, allBool, allDatetime = false, false, false, false
			}
		}

		dtype := "object"
		switch {
		case !hasValue:
			dtype = "object"
		case allDatetime:
			dtype = "datetime64[ns]"
		case allInt && !hasNull:
			dtype = "int64"
		case allNumeric: // all-int-with-null (promoted) or any-float
			dtype = "float64"
		case allBool && !hasNull:
			dtype = "bool"
		case allStr: // pure-string column (with or without nulls) -> pandas 3.x "str" dtype
			dtype = "str"
		default:
			dtype = "object"
		}

		kind := pandasKindFromDtype(dtype, firstNonNull)
		out = append(out, jsonenc.NewObject().Set("name", pyStrAny(col)).Set("dtype", dtype).Set("kind", kind))
	}
	return out
}

// pandasKindFromDtype mirrors the kind-selection in _infer_query_field_types: derive the display kind
// from the dtype string, refining an "object" dtype from the first non-null sample.
func pandasKindFromDtype(dtype string, sample any) string {
	lower := strings.ToLower(dtype)
	switch {
	case strings.Contains(lower, "datetime"):
		return "datetime"
	case lower == "bool" || lower == "boolean":
		return "boolean"
	case strings.HasPrefix(lower, "int") || strings.HasPrefix(lower, "uint"):
		return "integer"
	case strings.HasPrefix(lower, "float") || strings.HasPrefix(lower, "double"):
		return "number"
	}
	// object dtype -> sample the first non-null value.
	switch cellKind(sample) {
	case "bool":
		return "boolean"
	case "int":
		return "integer"
	case "float":
		return "number"
	case "json":
		return "json"
	default:
		return "string"
	}
}

// zeroQueryLLMStats mirrors _summarize_query_llm_stats(named_query_generation={}, chart_generation={})
// for the no-chart path: every stage and the totals are zero.
func zeroQueryLLMStats() *jsonenc.Object {
	stage := func() *jsonenc.Object {
		return jsonenc.NewObject().Set("prompt_tokens", 0).Set("completion_tokens", 0).
			Set("thinking_tokens", 0).Set("elapsed_ms", 0)
	}
	return jsonenc.NewObject().Set("totals", stage()).
		Set("named_query_generation", stage()).Set("chart_generation", stage())
}

// queryExplainFailedLLMStats mirrors _summarize_query_llm_stats() called with NO stages on the
// api_query_run explain-failed branch: only the zeroed "totals" key (no per-stage entries).
func queryExplainFailedLLMStats() *jsonenc.Object {
	return jsonenc.NewObject().Set("totals", queryStageStats(llmStats{}))
}

// handleApiQueryRun — app.py api_query_run (no-chart path): execute a user SQL statement and
// return the results + telemetry trace ids. (The do_chart branch needs the LLM mock — follow-up.)
func (s *server) handleApiQueryRun(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	sql := strings.TrimSpace(bstr(m, "sql"))
	question := strings.TrimSpace(bstr(m, "question"))
	doChart := bodyBool(m, "chart", false)
	// app.py api_query_run threads preferred_chart_type + chart_instruction into the named-query and
	// chart-spec LLM prompts (do_chart branch); thread them here too.
	preferredChartType := strings.TrimSpace(bstr(m, "preferred_chart_type"))
	chartInstruction := strings.TrimSpace(bstr(m, "chart_instruction"))
	if sql == "" {
		s.errorJSON(w, http.StatusBadRequest, "sql is required")
		return
	}
	if !s.queryPageEnabled() {
		s.writeMaskedJSON(w, http.StatusNotFound,
			jsonenc.NewObject().Set("ok", false).Set("error", "Query page is unavailable."))
		return
	}
	model := strings.TrimSpace(s.loadAISetting("ai.model", ""))
	guardModel := strings.TrimSpace(s.loadAISetting("ai.guard_model", ""))
	endpoint := strings.TrimSpace(s.loadAISetting("ai.endpoint_url", ""))
	traceID := md5Hex("query-run|" + sql + "|" + fakeTimeNs())
	turnID := traceID[:16]
	startBody := question
	if startBody == "" {
		startBody = sql
	}
	startQ := question
	if startQ == "" {
		startQ = "(manual SQL execution)"
	}
	s.emitAiHelperLogEvent("query.turn.start", traceID, turnID, "/query", model, guardModel, "off",
		startBody, "INFO", map[string]string{"gen_ai.input.question": startQ})

	// Pre-flight EXPLAIN to surface any parse/planning errors before execution (app.py
	// _vanna_explain_sql): validate_sql first (the C5 security gate that restricts user-submitted SQL
	// to the approved table set), then `EXPLAIN {sql}`. A blocked statement OR a planning error is
	// rejected with the explain-failed payload (422) before any real execution. validateSQL returns a
	// "SQL validation error: …" message; the EXPLAIN error is the RAW chdb exception string (str(exc))
	// — neither is run through publicDashboardQueryError, matching Python.
	if explainErr := s.vannaExplainSQL(sql); explainErr != "" {
		s.emitAiHelperLogEvent("query.sql.explain_failed", traceID, turnID, "/query", model, guardModel, "off",
			explainErr, "WARN", map[string]string{
				"gen_ai.operation.name": "query_sql_explain", "sobs.query.exec.error": explainErr,
			})
		writeJSON(w, http.StatusUnprocessableEntity, jsonenc.NewObject().
			Set("ok", false).Set("error", explainErr).Set("trace_id", traceID).Set("turn_id", turnID).
			Set("sql", sql).Set("columns", []any{}).Set("rows", []any{}).
			Set("llm_stats", queryExplainFailedLLMStats()))
		return
	}

	res, execErr := s.db.Execute(sql)
	var columns, rows, fieldTypes []any
	rowCount := 0
	if execErr == nil {
		columns, rows = serializeQueryResult(res)
		// Hard row cap (app.py _vanna_run_query): truncate to SOBS_QUERY_MAX_ROWS (default 1000)
		// after execution, regardless of what the LLM generated.
		if cap := envInt("SOBS_QUERY_MAX_ROWS", 1000); cap >= 0 && len(rows) > cap {
			rows = rows[:cap]
		}
		rowCount = len(rows)
		fieldTypes = inferQueryFieldTypes(columns, rows)
	} else {
		columns, rows, fieldTypes = []any{}, []any{}, []any{}
	}
	execStatus, execErrStr := "ok", ""
	if execErr != nil {
		execStatus, execErrStr = "error", publicDashboardQueryError(execErr)
	}
	s.emitAiHelperLogEvent("query.sql.executed", traceID, turnID, "/query", model, guardModel, "off",
		sql, severityFor(execErr), map[string]string{
			"gen_ai.operation.name": "query_sql_execute", "sobs.query.exec.attempt": "1",
			"sobs.query.exec.status": execStatus, "sobs.query.exec.row_count": strconv.Itoa(rowCount),
			"sobs.query.exec.error": execErrStr, "gen_ai.response.latency_ms": "0",
			"sobs.gen_ai.prompt": question, "sobs.gen_ai.response": sql,
		})

	// app.py only appends the primary "main" dataset when df is not None (i.e. no exec error). On an
	// exec error _vanna_run_query returns (None, error) so datasets stays empty.
	datasets := []any{}
	if execErr == nil {
		datasets = append(datasets, jsonenc.NewObject().
			Set("name", "main").Set("purpose", "primary dataset").Set("sql", sql).
			Set("columns", columns).Set("field_types", fieldTypes).Set("rows", rows).Set("error", ""))
	}

	// do_chart branch (app.py:30629-30765): when chart:true, the primary query succeeded, and it has
	// columns, guard the chart request, generate + execute named datasets, generate the chart spec, and
	// emit the named/chart telemetry. The parity corpus never sets chart:true, so this is runtime-only;
	// it reuses the read-only LLM/guard/chart helpers and never alters the no-chart success bytes.
	chartSpec, chartError := "", ""
	var namedStats, chartStats llmStats
	if doChart && execErr == nil && len(columns) > 0 {
		guardInput := question
		if guardInput == "" {
			guardInput = "Generate chart for SQL: " + truncRunes(sql, 500)
		}
		allowed, guardReason, guardStats := s.aiHelperGuardCheck(guardInput, "/query")
		s.emitAiHelperLogEvent("query.guard.result", traceID, turnID, "/query", model, guardModel, "off",
			"Guard verdict: "+guardReason, "INFO", guardTelemetryAttrs(allowed, guardReason, guardStats))
		if allowed {
			var namedQueries []any
			namedQueries, namedStats = s.generateNamedQueriesStats(endpoint, firstNonEmpty(question, sql), sql, preferredChartType, chartInstruction)
			s.emitAiHelperLogEvent("query.sql.named_generated", traceID, turnID, "/query", model, guardModel, "off",
				jsonDumpsNoEsc(namedQueries), "INFO", map[string]string{
					"gen_ai.operation.name":      "query_sql_named",
					"gen_ai.usage.input_tokens":  strconv.Itoa(namedStats.prompt),
					"gen_ai.usage.output_tokens": strconv.Itoa(namedStats.completion),
					"gen_ai.response.latency_ms": "0",
				})

			for _, ds := range s.executeNamedQueriesForQueryRun(namedQueries) {
				dso, ok := ds.(*jsonenc.Object)
				if !ok {
					continue
				}
				name := pyStr2OrDefault(objGetStr(dso, "name"), "dataset")
				datasets = append(datasets, jsonenc.NewObject().
					Set("name", name).Set("purpose", objGetStr(dso, "purpose")).
					Set("sql", objGetStr(dso, "sql")).
					Set("columns", objGetOr(dso, "columns", []any{})).
					Set("field_types", objGetOr(dso, "field_types", []any{})).
					Set("rows", objGetOr(dso, "rows", []any{})).
					Set("error", objGetStr(dso, "error")))
			}

			// sample = [dict(zip(columns, r)) for r in rows[:20]]
			sample := chartSampleRecords(columns, rows, 20)
			chartSpec, chartError, chartStats = s.generateChartSpecStats(endpoint, question, columns, sample, datasets, preferredChartType, chartInstruction)
			chartBody := chartSpec
			if chartBody == "" {
				chartBody = chartError
			}
			s.emitAiHelperLogEvent("query.chart.generated", traceID, turnID, "/query", model, guardModel, "off",
				chartBody, "INFO", map[string]string{
					"gen_ai.operation.name":      "query_chart",
					"gen_ai.usage.input_tokens":  strconv.Itoa(chartStats.prompt),
					"gen_ai.usage.output_tokens": strconv.Itoa(chartStats.completion),
					"gen_ai.response.latency_ms": "0",
				})
		} else {
			chartError = "Chart generation blocked by safety guard: " + guardReason
		}
	}

	finalError := execErrStr
	if finalError == "" {
		finalError = chartError
	}
	s.emitAiHelperLogEvent("query.turn.complete", traceID, turnID, "/query", model, guardModel, "off",
		"Query turn completed", severityForStr(finalError), map[string]string{
			"gen_ai.input.question": question, "sobs.gen_ai.prompt": question,
			"sobs.gen_ai.response": sql, "gen_ai.operation.name": "query",
		})

	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("trace_id", traceID).Set("turn_id", turnID).Set("sql", sql).
		Set("columns", columns).Set("field_types", fieldTypes).Set("rows", rows).
		Set("retry_count", 0).Set("datasets", datasets).Set("chart_spec", chartSpec).
		Set("error", finalError).Set("llm_stats", queryRunLLMStats(namedStats, chartStats)))
}

// pyStr2OrDefault mirrors `str(ds.get(k) or default)`.
func pyStr2OrDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// chartSampleRecords mirrors `[dict(zip(columns, r)) for r in rows[:20]]`: up to 20 rows as ordered
// {column: value} objects (column order preserved, like Python dict(zip(...))).
func chartSampleRecords(columns, rows []any, limit int) []any {
	out := []any{}
	for i, rowAny := range rows {
		if i >= limit {
			break
		}
		row, ok := rowAny.([]any)
		if !ok {
			continue
		}
		rec := jsonenc.NewObject()
		for j, c := range columns {
			if j >= len(row) {
				break
			}
			rec.Set(pyStrAny(c), row[j])
		}
		out = append(out, rec)
	}
	return out
}

func severityFor(err error) string {
	if err != nil {
		return "ERROR"
	}
	return "INFO"
}

// severityForStr mirrors app.py's "INFO" if not <error_string> else "ERROR" — the ask/vanna paths
// key severity on the error STRING (which now includes validation errors), not an error object.
func severityForStr(errStr string) string {
	if errStr != "" {
		return "ERROR"
	}
	return "INFO"
}

// handleApiQueryAsk — app.py api_query_ask (no-chart path): guard the question, generate SQL via
// the LLM (canned), execute it, and return the results + llm_stats.
func (s *server) handleApiQueryAsk(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	question := strings.TrimSpace(bstr(m, "question"))
	if question == "" {
		s.errorJSON(w, http.StatusBadRequest, "question is required")
		return
	}
	if !s.queryPageEnabled() {
		s.writeMaskedJSON(w, http.StatusNotFound,
			jsonenc.NewObject().Set("ok", false).Set("error", "Query page is unavailable."))
		return
	}
	traceID := md5Hex("query|" + question + "|" + fakeTimeNs())
	turnID := traceID[:16]
	model := strings.TrimSpace(s.loadAISetting("ai.model", ""))
	guardModel := strings.TrimSpace(s.loadAISetting("ai.guard_model", ""))
	endpoint := strings.TrimSpace(s.loadAISetting("ai.endpoint_url", ""))
	s.emitAiHelperLogEvent("query.turn.start", traceID, turnID, "/query", model, guardModel, "off",
		question, "INFO", map[string]string{"gen_ai.input.question": question})

	// Call aiHelperGuardCheck directly (not checkGuardModel, which discards the stats) so the
	// query.guard.result event carries the same _guard_telemetry_attrs Python emits — the guard
	// verdict/reason, token usage, latency, system instructions, and input messages. Those gen_ai.*
	// LogAttributes are then remembered (rememberLogAttrKeys), so the SQL-gen schema context's
	// "Observed LogAttributes keys" line matches Python byte-for-byte (app.py:30254-30265).
	allowed, reason, guardStats := s.aiHelperGuardCheck(question, "/query")
	s.emitAiHelperLogEvent("query.guard.result", traceID, turnID, "/query", model, guardModel, "off",
		"Guard verdict: "+reason, "INFO", guardTelemetryAttrs(allowed, reason, guardStats))
	if !allowed {
		s.writeMaskedJSON(w, http.StatusForbidden, jsonenc.NewObject().
			Set("ok", false).Set("error", "Request blocked by safety guard: "+reason).
			Set("trace_id", traceID).Set("turn_id", turnID))
		return
	}

	sql, sqlErr, sqlStats := s.generateSQLViaLLM(endpoint, question)
	emitSQLBody := sql
	if sqlErr != "" {
		emitSQLBody = sqlErr
	}
	s.emitAiHelperLogEvent("query.sql.generated", traceID, turnID, "/query", model, guardModel, "off",
		emitSQLBody, "INFO", map[string]string{"gen_ai.operation.name": "query_sql", "sobs.gen_ai.response": sql})
	if sqlErr != "" {
		s.writeMaskedJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().
			Set("ok", false).Set("error", sqlErr).Set("trace_id", traceID).Set("turn_id", turnID).
			Set("sql", "").Set("columns", []any{}).Set("rows", []any{}).
			Set("llm_stats", queryAskGenOnlyLLMStats(sqlStats)))
		return
	}

	// Validate + execute LLM-generated SQL with the bounded EXPLAIN/auto-CTE/AI-repair loop
	// (app.py _vanna_validate_and_execute_with_repair). On the canned parity success path the SQL is
	// valid so EXPLAIN passes and the loop returns on the first run; when the SQL is invalid this
	// drives the full repair-to-failure loop (retry_count + last_repair_stats reflect it). do_execute
	// defaults True; api_query_ask only runs this block when execute is truthy.
	columns, rows, fieldTypes := []any{}, []any{}, []any{}
	execErrStr := ""
	retryCount := 0
	mainDataframe := false
	var lastRepairStats llmStats
	if bodyBool(m, "execute", true) {
		finalSQL, cols, rws, execErr, rc, repairStats := s.validateAndExecuteVannaSQLWithRepair(endpoint, question, sql)
		sql = finalSQL
		execErrStr = execErr
		retryCount = rc
		lastRepairStats = repairStats
		// app.py: main_df is None on failure -> cols is nil here. A successful (even empty) result
		// returns a non-nil cols slice, which is the "main_df is not None" condition.
		mainDataframe = cols != nil
		if mainDataframe && execErr == "" {
			columns, rows = cols, rws
			fieldTypes = inferQueryFieldTypes(columns, rows)
		}
	}
	s.emitAiHelperLogEvent("query.sql.executed", traceID, turnID, "/query", model, guardModel, "off",
		sql, severityForStr(execErrStr), map[string]string{
			"gen_ai.operation.name": "query_sql_execute", "sobs.query.exec.attempt": itoa(maxInt(1, retryCount+1)),
			"sobs.query.exec.error": execErrStr, "sobs.gen_ai.response": sql,
		})
	datasets := []any{}
	// app.py: appends the main dataset when main_df is not None and not exec_error (columns/rows are
	// empty when the df itself is empty, but the dataset entry is still present).
	if mainDataframe && execErrStr == "" {
		datasets = append(datasets, jsonenc.NewObject().
			Set("name", "main").Set("purpose", "primary dataset").Set("sql", sql).
			Set("columns", columns).Set("field_types", fieldTypes).Set("rows", rows).Set("error", ""))
	}
	s.emitAiHelperLogEvent("query.turn.complete", traceID, turnID, "/query", model, guardModel, "off",
		"Query turn completed", severityForStr(execErrStr), map[string]string{
			"gen_ai.input.question": question, "gen_ai.operation.name": "query",
		})

	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("trace_id", traceID).Set("turn_id", turnID).Set("sql", sql).
		Set("columns", columns).Set("field_types", fieldTypes).Set("rows", rows).
		Set("retry_count", retryCount).Set("datasets", datasets).Set("chart_spec", "").
		Set("error", execErrStr).Set("llm_stats", queryAskLLMStats(sqlStats, lastRepairStats)))
}

// queryAskLLMStats mirrors _summarize_query_llm_stats(sql_generation, sql_repair,
// named_query_generation, chart_generation): the sql-generation stage + the sql-repair stage
// (last_repair_stats — non-zero whenever the repair loop dialed the LLM, zero on the no-repair
// success path) + two zero stages (no named-query / chart on this route) + totals (sum of all four).
func queryAskLLMStats(sqlStats, repairStats llmStats) *jsonenc.Object {
	totals := llmStats{
		prompt:     sqlStats.prompt + repairStats.prompt,
		completion: sqlStats.completion + repairStats.completion,
		thinking:   sqlStats.thinking + repairStats.thinking,
	}
	return jsonenc.NewObject().
		Set("sql_generation", queryStageStats(sqlStats)).
		Set("sql_repair", queryStageStats(repairStats)).
		Set("named_query_generation", queryStageStats(llmStats{})).
		Set("chart_generation", queryStageStats(llmStats{})).
		Set("totals", queryStageStats(totals))
}

// queryAskGenOnlyLLMStats mirrors the sqlErr early-return's _summarize_query_llm_stats(
// sql_generation=sql_stats): ONLY the sql_generation stage + totals (= sql_generation), with no
// sql_repair / named_query_generation / chart_generation keys (Python passes only that one kwarg).
func queryAskGenOnlyLLMStats(sqlStats llmStats) *jsonenc.Object {
	return jsonenc.NewObject().
		Set("totals", queryStageStats(sqlStats)).
		Set("sql_generation", queryStageStats(sqlStats))
}

// handleApiDashboardsQuery — app.py execute_chart_query: validate + execute a SELECT and return
// {columns, rows}. Not query-page gated.
func (s *server) handleApiDashboardsQuery(w http.ResponseWriter, r *http.Request) {
	query := bstr(bodyMap(r), "query")
	if e := validateChartQuery(query); e != "" {
		errorOnly(w, http.StatusBadRequest, e)
		return
	}
	res, err := s.db.Execute(injectLimit(query, 1000))
	if err != nil {
		errorOnly(w, http.StatusBadRequest, publicDashboardQueryError(err))
		return
	}
	columns, rows := serializeQueryResult(res)
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("columns", columns).Set("rows", rows))
}
