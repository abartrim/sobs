package main

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// s18: Query page, Table Explorer and Kubernetes health view.
//
// PORT-NOTE: This section calls a number of helpers owned by the (concurrent)
// s17 "vanna" section and by other already-ported sections. They are referenced
// here via the deterministic naming rule and assumed to exist with the
// signatures noted below; the reconcile phase aligns any mismatches.
//
//   queryPageEnabled(settings map[string]string) bool          (s03; nil ⇒ load)
//   kubernetesEnabled() bool                                   (s03)
//   loadAllAiSettings(db) map[string]string                    (s03)
//   getAppSetting/setAppSetting/delAppSetting(db, key[, value]) (shared)
//   normalizeThinkingLevel(string) string                      (s04)
//   summarizeQueryLlmStats(map[string]map[string]any) map[string]any (s04)
//   checkGuardModel(settings, input, ctx) (bool, string, map[string]any) (s04)
//   guardTelemetryAttrs(allowed bool, reason string, stats map[string]any) map[string]any (s17)
//   emitAiHelperLogEvent(eventName, chatId, turnId, page, model, guardModel,
//       thinkingLevel, body, severity string, attrs map[string]any)  (s17)
//       PORT-NOTE: Python _emit_ai_helper_log_event is keyword-only with
//       severity defaulting to "INFO"; the Go port is positional and callers
//       pass "INFO" explicitly where Python relied on the default.
//   newChdbSqlRunner(db) *ChdbSqlRunner                         (s17)
//       (*ChdbSqlRunner).getSchemaContext() string
//       (*ChdbSqlRunner).getAllowedTablesInfo() ([]map[string]any, error)
//       (*ChdbSqlRunner).getTableDetail(name) (map[string]any, error)
//   queryAllowedTables map[string]bool                         (s17)
//   queryFrame: replacement for pandas DataFrame, with fields
//       Columns []string and Rows [][]any (Rows == df.values.tolist()).  (s17)
//   vannaGenerateSql(question, schemaContext, settings, preferredChartType,
//       chartInstruction, thinkingLevel) (string, string, map[string]any) (s17)
//   vannaValidateAndExecuteWithRepair(db, question, schemaContext, initialSql,
//       settings, thinkingLevel) (string, *queryFrame, string, int, map[string]any) (s17)
//   vannaGenerateNamedQueries(question, schemaContext, baseSql, settings,
//       preferredChartType, chartInstruction, thinkingLevel)
//       ([]map[string]string, string, map[string]any)          (s17)
//   vannaExecuteNamedQueries(db, namedQueries, question, schemaContext,
//       settings, thinkingLevel, includeFieldTypes, useRepair bool)
//       []map[string]any                                       (s17)
//   vannaGenerateChartSpec(columns, sampleRows, question, settings,
//       preferredChartType, chartInstruction, namedDatasets, thinkingLevel)
//       (string, string, map[string]any)                       (s17)
//   vannaRefineChartSpec(currentSpec, columns, sampleRows, userInstruction,
//       settings, thinkingLevel) (string, string, map[string]any) (s17)
//   vannaExplainSql(db, sql) string                            (s17)
//   vannaRunQuery(db, sql) (*queryFrame, string)               (s17)
//   inferQueryFieldTypes(df *queryFrame) []map[string]string   (s17)
//   jsonSafeRows(rows [][]any) [][]any                         (s17)
// ---------------------------------------------------------------------------

func init() {
	registerRoute("GET", "/query", requireBasicAuth(viewQuery))
	registerRoute("POST", "/api/query/ask", requireBasicAuth(apiQueryAsk))
	registerRoute("POST", "/api/query/run", requireBasicAuth(apiQueryRun))
	registerRoute("POST", "/api/query/refine-chart", requireBasicAuth(apiQueryRefineChart))
	registerRoute("GET", "/api/query/schema", requireBasicAuth(apiQuerySchema))
	registerRoute("GET", "/table-explorer", requireBasicAuth(viewTableExplorer))
	registerRoute("GET", "/api/table-explorer/tables", requireBasicAuth(apiTableExplorerTables))
	registerRoute("GET", "/api/table-explorer/table/{name}", requireBasicAuth(apiTableExplorerTable))
	registerRoute("GET", "/api/chart-types", requireBasicAuth(apiChartTypes))
	registerRoute("GET", "/settings/kubernetes", requireBasicAuth(viewK8sSettings))
	registerRoute("POST", "/settings/kubernetes", requireBasicAuth(saveK8sSettings))
	registerRoute("GET", "/kubernetes", requireBasicAuth(viewKubernetes))
	registerRoute("GET", "/api/kubernetes/status", requireBasicAuth(apiKubernetesStatus))
}

// queryFrame is the s18 alias for the shared dataFrame query-result type.
type queryFrame = dataFrame

// qStat mirrors Python dict.get(key, default) for telemetry stat maps.
func qStat(stats map[string]any, key string, def any) any {
	if v, ok := stats[key]; ok && v != nil {
		return v
	}
	return def
}

// payloadStr mirrors str(payload.get(key) or "").strip().
func payloadStr(payload map[string]any, key string) string {
	return strings.TrimSpace(rowString(payload[key]))
}

// payloadThinking mirrors _normalize_thinking_level(str(payload.get(key) or "off")).
func payloadThinking(payload map[string]any, key string) string {
	v := rowString(payload[key])
	if v == "" {
		v = "off"
	}
	return normalizeThinkingLevel(v)
}

// ---------------------------------------------------------------------------
// Query page  GET /query   POST /api/query/ask
// ---------------------------------------------------------------------------

func viewQuery(w http.ResponseWriter, r *http.Request) {
	if !queryPageEnabled(nil) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Query page is unavailable until AI and guard settings are configured."))
		return
	}
	renderTemplate(w, r, "query.html", nil)
}

// apiQueryAsk is the natural-language → SQL → DataFrame endpoint.
//
// Accepts JSON {question, execute, chart} and returns ok/trace_id/turn_id/sql/
// columns/field_types/rows/retry_count/chart_spec/error.
func apiQueryAsk(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r) // force=True, silent=True → {} on bad input
	question := payloadStr(payload, "question")
	doExecute := true
	if v, ok := payload["execute"]; ok {
		doExecute = pyTruthy(v)
	}
	doChart := pyTruthy(payload["chart"])
	preferredChartType := payloadStr(payload, "preferred_chart_type")
	chartInstruction := payloadStr(payload, "chart_instruction")
	thinkingLevel := payloadThinking(payload, "thinking_level")

	if question == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "question is required"})
		return
	}

	db := getDb()
	settings := loadAllAiSettings(db)
	if !queryPageEnabled(settings) {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Query page is unavailable."})
		return
	}

	traceId := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("query|%s|%d", question, time.Now().UnixNano()))))
	turnId := clipRunes(traceId, 16)
	model := strings.TrimSpace(settings["ai.model"])
	guardModel := strings.TrimSpace(settings["ai.guard_model"])

	emitAiHelperLogEvent(
		"query.turn.start", traceId, turnId, "/query", model, guardModel, "off",
		question, "INFO",
		map[string]any{"gen_ai.input.question": question},
	)

	allowed, guardReason, guardStats := checkGuardModel(settings, question, "/query")
	emitAiHelperLogEvent(
		"query.guard.result", traceId, turnId, "/query", model, guardModel, "off",
		fmt.Sprintf("Guard verdict: %s", guardReason), "INFO",
		guardTelemetryAttrs(allowed, guardReason, guardStats),
	)
	if !allowed {
		jsonResponse(w, http.StatusForbidden, map[string]any{
			"ok":       false,
			"error":    fmt.Sprintf("Request blocked by safety guard: %s", guardReason),
			"trace_id": traceId,
			"turn_id":  turnId,
		})
		return
	}

	// Build schema context (synchronous; goroutine isolation handled by chDB lock).
	runner := newChdbSqlRunner(db)
	schemaContext := runner.getSchemaContext("default", 30)

	// Generate SQL
	sql, sqlErr, sqlStats := vannaGenerateSql(question, schemaContext, settings, preferredChartType, chartInstruction, thinkingLevel)
	sqlBody := sql
	if sqlBody == "" {
		sqlBody = sqlErr
	}
	emitAiHelperLogEvent(
		"query.sql.generated", traceId, turnId, "/query", model, guardModel, "off",
		sqlBody, "INFO",
		map[string]any{
			"gen_ai.operation.name":      "query_sql",
			"gen_ai.usage.input_tokens":  qStat(sqlStats, "prompt_tokens", 0),
			"gen_ai.usage.output_tokens": qStat(sqlStats, "completion_tokens", 0),
			"gen_ai.response.latency_ms": qStat(sqlStats, "elapsed_ms", 0),
			"sobs.gen_ai.prompt":         question,
			"sobs.gen_ai.response":       sql,
		},
	)
	if sqlErr != "" {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{
			"ok":        false,
			"error":     sqlErr,
			"trace_id":  traceId,
			"turn_id":   turnId,
			"sql":       "",
			"columns":   []string{},
			"rows":      [][]any{},
			"llm_stats": summarizeQueryLlmStats(map[string]map[string]any{"sql_generation": sqlStats}),
		})
		return
	}

	// Optionally execute
	var columns []string
	var fieldTypes []map[string]string
	var rows [][]any
	datasets := make([]map[string]any, 0)
	retryCount := 0
	execError := ""
	lastRepairStats := map[string]any{}
	namedStats := map[string]any{}
	chartStats := map[string]any{}
	if doExecute {
		execStarted := time.Now()
		var mainDf *queryFrame
		sql, mainDf, execError, retryCount, lastRepairStats = vannaValidateAndExecuteWithRepair(
			db, question, schemaContext, sql, settings, thinkingLevel,
		)
		execElapsedMs := int(time.Since(execStarted).Milliseconds())
		rowCount := 0
		if mainDf != nil {
			rowCount = len(mainDf.Rows)
		}
		sev := "INFO"
		if execError != "" {
			sev = "ERROR"
		}
		emitAiHelperLogEvent(
			"query.sql.executed", traceId, turnId, "/query", model, guardModel, "off",
			sql, sev,
			map[string]any{
				"gen_ai.operation.name":      "query_sql_execute",
				"sobs.query.exec.attempt":    max(1, retryCount+1),
				"sobs.query.exec.status":     execStatus(execError),
				"sobs.query.exec.row_count":  rowCount,
				"sobs.query.exec.error":      execError,
				"gen_ai.response.latency_ms": execElapsedMs,
				"sobs.gen_ai.prompt":         question,
				"sobs.gen_ai.response":       sql,
			},
		)

		if mainDf != nil && execError == "" {
			if len(mainDf.Rows) > 0 {
				columns = mainDf.Columns
				fieldTypes = inferQueryFieldTypes(*mainDf)
				rows = jsonSafeRows(dfValues(mainDf))
			}
			datasets = append(datasets, map[string]any{
				"name":        "main",
				"purpose":     "primary dataset",
				"sql":         sql,
				"columns":     columns,
				"field_types": fieldTypes,
				"rows":        rows,
				"error":       "",
			})
		}
	}

	// Optionally generate chart spec
	chartSpec := ""
	chartError := ""
	if doChart && execError == "" && len(columns) > 0 {
		var namedQueries []map[string]string
		namedQueries, _, namedStats = vannaGenerateNamedQueries(
			question, schemaContext, sql, settings, preferredChartType, chartInstruction, thinkingLevel,
		)
		emitAiHelperLogEvent(
			"query.sql.named_generated", traceId, turnId, "/query", model, guardModel, "off",
			jsonDumpsNoEscape(namedQueries), "INFO",
			map[string]any{
				"gen_ai.operation.name":      "query_sql_named",
				"gen_ai.usage.input_tokens":  qStat(namedStats, "prompt_tokens", 0),
				"gen_ai.usage.output_tokens": qStat(namedStats, "completion_tokens", 0),
				"gen_ai.response.latency_ms": qStat(namedStats, "elapsed_ms", 0),
			},
		)

		namedResults := vannaExecuteNamedQueries(
			db, namedQueries, question, schemaContext, settings, thinkingLevel, true, false,
		)
		for _, ds := range namedResults {
			datasets = append(datasets, normalizeNamedDataset(ds))
		}

		sample := buildSampleRows(columns, rows)
		chartSpec, chartError, chartStats = vannaGenerateChartSpec(
			columns, sample, question, settings, preferredChartType, chartInstruction, datasets, thinkingLevel,
		)
		emitAiHelperLogEvent(
			"query.chart.generated", traceId, turnId, "/query", model, guardModel, "off",
			chartBody(chartSpec, chartError), "INFO",
			map[string]any{
				"gen_ai.operation.name":      "query_chart",
				"gen_ai.usage.input_tokens":  qStat(chartStats, "prompt_tokens", 0),
				"gen_ai.usage.output_tokens": qStat(chartStats, "completion_tokens", 0),
				"gen_ai.response.latency_ms": qStat(chartStats, "elapsed_ms", 0),
			},
		)
	}

	emitAiHelperLogEvent(
		"query.turn.complete", traceId, turnId, "/query", model, guardModel, "off",
		"Query turn completed", "INFO",
		map[string]any{
			"gen_ai.input.question": question,
			"sobs.gen_ai.prompt":    question,
			"sobs.gen_ai.response":  sql,
			"gen_ai.operation.name": "query",
		},
	)

	finalError := execError
	if finalError == "" {
		finalError = chartError
	}
	jsonifyWithOptionalSqlOutputMask(w, map[string]any{
		"ok":          true,
		"trace_id":    traceId,
		"turn_id":     turnId,
		"sql":         sql,
		"columns":     columns,
		"field_types": fieldTypes,
		"rows":        rows,
		"retry_count": retryCount,
		"datasets":    datasets,
		"chart_spec":  chartSpec,
		"error":       finalError,
		"llm_stats": summarizeQueryLlmStats(map[string]map[string]any{
			"sql_generation":         sqlStats,
			"sql_repair":             lastRepairStats,
			"named_query_generation": namedStats,
			"chart_generation":       chartStats,
		}),
	})
}

// execStatus mirrors "ok" if not exec_error else "error".
func execStatus(execError string) string {
	if execError == "" {
		return "ok"
	}
	return "error"
}

// chartBody mirrors chart_spec if chart_spec else chart_error.
func chartBody(chartSpec, chartError string) string {
	if chartSpec != "" {
		return chartSpec
	}
	return chartError
}

// normalizeNamedDataset mirrors the per-dataset normalization used when
// appending named query results into the datasets list.
func normalizeNamedDataset(ds map[string]any) map[string]any {
	name := strings.TrimSpace(rowString(ds["name"]))
	if name == "" {
		name = "dataset"
	}
	return map[string]any{
		"name":        name,
		"purpose":     rowString(ds["purpose"]),
		"sql":         rowString(ds["sql"]),
		"columns":     orEmptyList(ds["columns"]),
		"field_types": orEmptyList(ds["field_types"]),
		"rows":        orEmptyList(ds["rows"]),
		"error":       rowString(ds["error"]),
	}
}

// orEmptyList mirrors `value or []` for dataset payload fields.
func orEmptyList(value any) any {
	if pyTruthy(value) {
		return value
	}
	return []any{}
}

// buildSampleRows mirrors [dict(zip(columns, r)) for r in rows[:20]].
func buildSampleRows(columns []string, rows [][]any) []map[string]any {
	limit := min(20, len(rows))
	sample := make([]map[string]any, 0, limit)
	for _, r := range rows[:limit] {
		m := map[string]any{}
		n := min(len(columns), len(r))
		for i := 0; i < n; i++ {
			m[columns[i]] = r[i]
		}
		sample = append(sample, m)
	}
	return sample
}

// apiQueryRun executes an existing SQL statement and optionally generates a chart.
func apiQueryRun(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r) // force=True, silent=True → {} on bad input
	sql := payloadStr(payload, "sql")
	question := payloadStr(payload, "question")
	doChart := pyTruthy(payload["chart"])
	preferredChartType := payloadStr(payload, "preferred_chart_type")
	chartInstruction := payloadStr(payload, "chart_instruction")
	thinkingLevel := payloadThinking(payload, "thinking_level")

	if sql == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "sql is required"})
		return
	}

	db := getDb()
	settings := loadAllAiSettings(db)
	if !queryPageEnabled(settings) {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Query page is unavailable."})
		return
	}

	traceId := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("query-run|%s|%d", sql, time.Now().UnixNano()))))
	turnId := clipRunes(traceId, 16)
	model := strings.TrimSpace(settings["ai.model"])
	guardModel := strings.TrimSpace(settings["ai.guard_model"])

	startBody := question
	if startBody == "" {
		startBody = sql
	}
	startQuestion := question
	if startQuestion == "" {
		startQuestion = "(manual SQL execution)"
	}
	emitAiHelperLogEvent(
		"query.turn.start", traceId, turnId, "/query", model, guardModel, "off",
		startBody, "INFO",
		map[string]any{"gen_ai.input.question": startQuestion},
	)

	execStarted := time.Now()
	// Pre-flight EXPLAIN to surface any parse/planning errors before execution.
	explainError := vannaExplainSql(db, sql)
	if explainError != "" {
		emitAiHelperLogEvent(
			"query.sql.explain_failed", traceId, turnId, "/query", model, guardModel, "off",
			explainError, "WARN",
			map[string]any{"gen_ai.operation.name": "query_sql_explain", "sobs.query.exec.error": explainError},
		)
		jsonResponse(w, http.StatusUnprocessableEntity, map[string]any{
			"ok":        false,
			"error":     explainError,
			"trace_id":  traceId,
			"turn_id":   turnId,
			"sql":       sql,
			"columns":   []string{},
			"rows":      [][]any{},
			"llm_stats": summarizeQueryLlmStats(map[string]map[string]any{}),
		})
		return
	}
	df, execError := vannaRunQuery(db, sql)
	execElapsedMs := int(time.Since(execStarted).Milliseconds())

	rowCount := 0
	var columns []string
	var fieldTypes []map[string]string
	var rows [][]any
	datasets := make([]map[string]any, 0)
	if df != nil {
		rowCount = len(df.Rows)
		if len(df.Rows) > 0 {
			columns = df.Columns
			fieldTypes = inferQueryFieldTypes(*df)
			rows = jsonSafeRows(dfValues(df))
		}
		datasets = append(datasets, map[string]any{
			"name":        "main",
			"purpose":     "primary dataset",
			"sql":         sql,
			"columns":     columns,
			"field_types": fieldTypes,
			"rows":        rows,
			"error":       "",
		})
	}

	sev := "INFO"
	if execError != "" {
		sev = "ERROR"
	}
	emitAiHelperLogEvent(
		"query.sql.executed", traceId, turnId, "/query", model, guardModel, "off",
		sql, sev,
		map[string]any{
			"gen_ai.operation.name":      "query_sql_execute",
			"sobs.query.exec.attempt":    1,
			"sobs.query.exec.status":     execStatus(execError),
			"sobs.query.exec.row_count":  rowCount,
			"sobs.query.exec.error":      execError,
			"gen_ai.response.latency_ms": execElapsedMs,
			"sobs.gen_ai.prompt":         question,
			"sobs.gen_ai.response":       sql,
		},
	)

	chartSpec := ""
	chartError := ""
	namedStats := map[string]any{}
	chartStats := map[string]any{}
	if doChart && execError == "" && len(columns) > 0 {
		guardInput := question
		if guardInput == "" {
			guardInput = fmt.Sprintf("Generate chart for SQL: %s", clipRunes(sql, 500))
		}
		allowed, guardReason, guardStats := checkGuardModel(settings, guardInput, "/query")
		emitAiHelperLogEvent(
			"query.guard.result", traceId, turnId, "/query", model, guardModel, "off",
			fmt.Sprintf("Guard verdict: %s", guardReason), "INFO",
			guardTelemetryAttrs(allowed, guardReason, guardStats),
		)
		if allowed {
			schemaContext := newChdbSqlRunner(db).getSchemaContext("default", 30)
			chartQuestion := question
			if chartQuestion == "" {
				chartQuestion = sql
			}
			var namedQueries []map[string]string
			namedQueries, _, namedStats = vannaGenerateNamedQueries(
				chartQuestion, schemaContext, sql, settings, preferredChartType, chartInstruction, thinkingLevel,
			)
			emitAiHelperLogEvent(
				"query.sql.named_generated", traceId, turnId, "/query", model, guardModel, "off",
				jsonDumpsNoEscape(namedQueries), "INFO",
				map[string]any{
					"gen_ai.operation.name":      "query_sql_named",
					"gen_ai.usage.input_tokens":  qStat(namedStats, "prompt_tokens", 0),
					"gen_ai.usage.output_tokens": qStat(namedStats, "completion_tokens", 0),
					"gen_ai.response.latency_ms": qStat(namedStats, "elapsed_ms", 0),
				},
			)

			namedResults := vannaExecuteNamedQueries(
				db, namedQueries, chartQuestion, schemaContext, settings, thinkingLevel, true, false,
			)
			for _, ds := range namedResults {
				datasets = append(datasets, normalizeNamedDataset(ds))
			}

			sample := buildSampleRows(columns, rows)
			chartSpec, chartError, chartStats = vannaGenerateChartSpec(
				columns, sample, question, settings, preferredChartType, chartInstruction, datasets, thinkingLevel,
			)
			emitAiHelperLogEvent(
				"query.chart.generated", traceId, turnId, "/query", model, guardModel, "off",
				chartBody(chartSpec, chartError), "INFO",
				map[string]any{
					"gen_ai.operation.name":      "query_chart",
					"gen_ai.usage.input_tokens":  qStat(chartStats, "prompt_tokens", 0),
					"gen_ai.usage.output_tokens": qStat(chartStats, "completion_tokens", 0),
					"gen_ai.response.latency_ms": qStat(chartStats, "elapsed_ms", 0),
				},
			)
		} else {
			chartError = fmt.Sprintf("Chart generation blocked by safety guard: %s", guardReason)
		}
	}

	finalError := execError
	if finalError == "" {
		finalError = chartError
	}
	sev2 := "INFO"
	if finalError != "" {
		sev2 = "ERROR"
	}
	emitAiHelperLogEvent(
		"query.turn.complete", traceId, turnId, "/query", model, guardModel, "off",
		"Query turn completed", sev2,
		map[string]any{
			"gen_ai.input.question": question,
			"sobs.gen_ai.prompt":    question,
			"sobs.gen_ai.response":  sql,
			"gen_ai.operation.name": "query",
		},
	)

	jsonifyWithOptionalSqlOutputMask(w, map[string]any{
		"ok":          true,
		"trace_id":    traceId,
		"turn_id":     turnId,
		"sql":         sql,
		"columns":     columns,
		"field_types": fieldTypes,
		"rows":        rows,
		"retry_count": 0,
		"datasets":    datasets,
		"chart_spec":  chartSpec,
		"error":       finalError,
		"llm_stats": summarizeQueryLlmStats(map[string]map[string]any{
			"named_query_generation": namedStats,
			"chart_generation":       chartStats,
		}),
	})
}

// apiQueryRefineChart refines an existing chart spec based on user instruction.
func apiQueryRefineChart(w http.ResponseWriter, r *http.Request) {
	settings := loadAllAiSettings(getDb())
	if !queryPageEnabled(settings) {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Query page is unavailable."})
		return
	}

	payload, _ := readJsonBody(r)
	currentSpec := rowString(payload["chart_spec"])
	columns := payload["columns"]
	var rows []any
	if rs, ok := payload["rows"].([]any); ok {
		rows = rs
	}
	userInstruction := strings.TrimSpace(rowString(payload["instruction"]))
	thinkingLevel := payloadThinking(payload, "thinking_level")

	if currentSpec == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "No chart spec provided."})
		return
	}
	if userInstruction == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "No instruction provided."})
		return
	}

	// Use current row data as sample if available, otherwise empty list.
	// PORT-NOTE: Python passes raw rows[:20] (list of dicts) to the refiner; the
	// Go refiner expects []map[string]any, so non-dict row elements are skipped.
	sampleRows := []map[string]any{}
	if len(rows) > 0 {
		for _, rowItem := range rows[:min(20, len(rows))] {
			if m, ok := rowItem.(map[string]any); ok {
				sampleRows = append(sampleRows, m)
			}
		}
	}

	traceId := agentUuid4()
	turnId := agentUuid4()
	model := strings.TrimSpace(settings["ai.model"])

	emitAiHelperLogEvent(
		"query.turn.start", traceId, turnId, "/query", model, "", "off",
		fmt.Sprintf("Chart refinement requested: %s", userInstruction), "INFO",
		map[string]any{
			"gen_ai.operation.name":   "refine_chart",
			"sobs.gen_ai.instruction": userInstruction,
		},
	)

	chartSpec, chartError, chartStats := vannaRefineChartSpec(
		currentSpec, anyToStringList(columns), sampleRows, userInstruction, settings, thinkingLevel,
	)

	refineSev := "INFO"
	if chartError != "" {
		refineSev = "ERROR"
	}
	emitAiHelperLogEvent(
		"query.chart.refined", traceId, turnId, "/query", model, "", "off",
		chartBody(chartSpec, chartError), refineSev,
		map[string]any{
			"gen_ai.operation.name":      "refine_chart",
			"gen_ai.usage.input_tokens":  qStat(chartStats, "prompt_tokens", 0),
			"gen_ai.usage.output_tokens": qStat(chartStats, "completion_tokens", 0),
			"gen_ai.response.latency_ms": qStat(chartStats, "elapsed_ms", 0),
			"sobs.gen_ai.instruction":    userInstruction,
		},
	)

	emitAiHelperLogEvent(
		"query.turn.complete", traceId, turnId, "/query", model, "", "off",
		"Chart refinement completed", refineSev,
		map[string]any{"gen_ai.operation.name": "refine_chart"},
	)

	if chartError != "" {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{
			"ok":       false,
			"error":    chartError,
			"trace_id": traceId,
		})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":         true,
		"trace_id":   traceId,
		"chart_spec": chartSpec,
	})
}

// apiQuerySchema returns the schema context string used for LLM prompts.
func apiQuerySchema(w http.ResponseWriter, r *http.Request) {
	settings := loadAllAiSettings(getDb())
	if !queryPageEnabled(settings) {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Query page is unavailable."})
		return
	}
	db := getDb()
	runner := newChdbSqlRunner(db)
	schema := runner.getSchemaContext("default", 30)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "schema": schema})
}

// ---------------------------------------------------------------------------
// Table Explorer  GET /table-explorer
// API             GET /api/table-explorer/tables
//                 GET /api/table-explorer/table/<name>
// ---------------------------------------------------------------------------

func viewTableExplorer(w http.ResponseWriter, r *http.Request) {
	if !queryPageEnabled(nil) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Table Explorer is unavailable until AI and guard settings are configured."))
		return
	}
	renderTemplate(w, r, "table_explorer.html", nil)
}

func apiTableExplorerTables(w http.ResponseWriter, r *http.Request) {
	if !queryPageEnabled(nil) {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Table Explorer is unavailable."})
		return
	}
	db := getDb()
	runner := newChdbSqlRunner(db)
	tables := runner.getAllowedTablesInfo()
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "tables": tables})
}

func apiTableExplorerTable(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !queryPageEnabled(nil) {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Table Explorer is unavailable."})
		return
	}

	// Validate table is in the allowlist.
	if !queryAllowedTables[name] {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": fmt.Sprintf("Table '%s' is not accessible.", name)})
		return
	}

	db := getDb()
	runner := newChdbSqlRunner(db)
	detail := runner.getTableDetail(name)
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":      true,
		"table":   name,
		"columns": detail["columns"],
		"ddl":     detail["ddl"],
		"sample":  detail["sample"],
	})
}

// apiChartTypes returns the catalog of available ECharts chart types.
func apiChartTypes(w http.ResponseWriter, r *http.Request) {
	chartTypesPath := filepath.Join(moduleDir(), "static", "echarts-chart-types.json")
	if _, err := os.Stat(chartTypesPath); err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{
			"ok":    false,
			"error": "Chart types catalog not found. Run: node scripts/extract-echarts-types.js",
		})
		return
	}
	data, err := os.ReadFile(chartTypesPath)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("Failed to load chart types: %s", err.Error()),
		})
		return
	}
	var catalog any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&catalog); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("Failed to load chart types: %s", err.Error()),
		})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "data": catalog})
}

// ---------------------------------------------------------------------------
// Kubernetes Health View  GET /kubernetes
// Settings               GET/POST /settings/kubernetes
// API                    GET /api/kubernetes/status
// ---------------------------------------------------------------------------

var k8sSettingKeys = []string{"kubernetes.enabled"}

// loadK8sSettings loads Kubernetes health settings from sobs_app_settings.
func loadK8sSettings(db *ChDbConnection) map[string]string {
	result := map[string]string{}
	for _, key := range k8sSettingKeys {
		result[key] = ""
	}
	for _, key := range k8sSettingKeys {
		raw := getAppSetting(db, key)
		if raw != "" {
			result[key] = raw
		}
	}
	return result
}

// k8sSettingsFromForm extracts Kubernetes settings from a submitted form.
func k8sSettingsFromForm(form map[string]string) map[string]string {
	enabled := "0"
	if form["enabled"] == "1" {
		enabled = "1"
	}
	return map[string]string{"kubernetes.enabled": enabled}
}

var k8sPromExtraMetricNames = []string{"container_memory_working_set_bytes"}

const k8sOtelAttr = "k8s.node.name"

// coerceFloatOr0 mirrors float(value or 0).
func coerceFloatOr0(value any) float64 {
	f, _ := coerceFloat(value)
	return f
}

// detectK8sMetricFormat returns 'otel', 'prometheus', or 'none' based on which
// k8s metric names are present.
func detectK8sMetricFormat(db *ChDbConnection) string {
	if res, err := db.Execute(fmt.Sprintf("SELECT count() AS cnt FROM otel_metrics_gauge WHERE Attributes['%s'] != '' LIMIT 1", k8sOtelAttr)); err == nil {
		row := res.Fetchone()
		if coerceInt(qStat(row, "cnt", 0)) > 0 {
			return "otel"
		}
	}
	promMetricFilter := "MetricName LIKE 'kube_%'"
	if len(k8sPromExtraMetricNames) > 0 {
		names := make([]string, len(k8sPromExtraMetricNames))
		for i, n := range k8sPromExtraMetricNames {
			names[i] = "'" + n + "'"
		}
		promMetricFilter = fmt.Sprintf("(%s OR MetricName IN (%s))", promMetricFilter, strings.Join(names, ", "))
	}
	for _, table := range []string{"otel_metrics_gauge", "otel_metrics_sum"} {
		if res, err := db.Execute(fmt.Sprintf("SELECT count() AS cnt FROM %s WHERE %s LIMIT 1", table, promMetricFilter)); err == nil {
			row := res.Fetchone()
			if coerceInt(qStat(row, "cnt", 0)) > 0 {
				return "prometheus"
			}
		}
	}
	return "none"
}

// fetchK8sFromOtel builds Kubernetes status from OTEL metric tables only.
func fetchK8sFromOtel(db *ChDbConnection, query map[string]any) map[string]any {
	if query == nil {
		query = map[string]any{}
	}

	toIntBounded := func(value any, def, lo, hi int) int {
		parsed, err := strconv.Atoi(strings.TrimSpace(rowString(value)))
		if err != nil {
			return def
		}
		return max(lo, min(hi, parsed))
	}

	countQuery := func(sql string, params []any) int {
		res, err := db.Execute(sql, params)
		if err != nil {
			return 0
		}
		row := res.Fetchone()
		if row == nil {
			return 0
		}
		return coerceInt(qStat(row, "cnt", 0))
	}

	normalizedValues := func(raw any) []string {
		switch v := raw.(type) {
		case nil:
			return []string{}
		case []string:
			out := []string{}
			for _, s := range v {
				if t := strings.TrimSpace(s); t != "" {
					out = append(out, t)
				}
			}
			return out
		case []any:
			out := []string{}
			for _, item := range v {
				if t := strings.TrimSpace(rowString(item)); t != "" {
					out = append(out, t)
				}
			}
			return out
		default:
			t := strings.TrimSpace(rowString(raw))
			if t != "" {
				return []string{t}
			}
			return []string{}
		}
	}

	appendOrEquals := func(conditions []string, params []any, fieldSql string, values []string) ([]string, []any) {
		if len(values) == 0 {
			return conditions, params
		}
		ph := make([]string, len(values))
		for i := range values {
			ph[i] = fieldSql + " = ?"
		}
		conditions = append(conditions, "("+strings.Join(ph, " OR ")+")")
		for _, v := range values {
			params = append(params, v)
		}
		return conditions, params
	}

	nameFilter := strings.TrimSpace(rowString(query["name"]))
	namespaceFilter := strings.TrimSpace(rowString(query["namespace"]))
	namespaceValues := normalizedValues(query["namespace_values"])
	if len(namespaceValues) == 0 && namespaceFilter != "" {
		namespaceValues = []string{namespaceFilter}
	}
	nodeValues := normalizedValues(query["node_values"])
	deploymentValues := normalizedValues(query["deployment_values"])
	podValues := normalizedValues(query["pod_values"])

	tableDefaults := map[string]string{
		"nodes":       "name",
		"deployments": "namespace",
		"pods":        "namespace",
	}
	sortColumns := map[string]map[string]string{
		"nodes": {
			"name":    "name",
			"status":  "status",
			"version": "version",
			"created": "last_seen",
		},
		"deployments": {
			"namespace": "namespace",
			"name":      "name",
			"desired":   "desired",
			"ready":     "ready",
			"available": "available",
			"created":   "last_seen",
		},
		"pods": {
			"namespace": "namespace",
			"name":      "name",
			"phase":     "phase",
			"ready":     "ready_signal",
			"restarts":  "restarts",
			"node":      "node",
			"created":   "last_seen",
		},
	}

	tableOpts := map[string]map[string]any{}
	for _, table := range []string{"nodes", "deployments", "pods"} {
		defaultSort := tableDefaults[table]
		reqSort := strings.TrimSpace(rowString(query[table+"_sort"]))
		sortKey := defaultSort
		if _, ok := sortColumns[table][reqSort]; ok {
			sortKey = reqSort
		}
		reqDir := strings.ToLower(strings.TrimSpace(rowString(query[table+"_dir"])))
		sortDir := "asc"
		if reqDir == "desc" {
			sortDir = "desc"
		}
		page := toIntBounded(query[table+"_page"], 1, 1, 1000000)
		pageSize := toIntBounded(query[table+"_page_size"], 25, 1, 200)
		tableOpts[table] = map[string]any{
			"sort_key":  sortKey,
			"sort_col":  sortColumns[table][sortKey],
			"sort_dir":  sortDir,
			"page":      page,
			"page_size": pageSize,
			"offset":    (page - 1) * pageSize,
		}
	}

	// Per-table accessors mirroring table_opts[table][...].
	optStr := func(table, key string) string { return rowString(tableOpts[table][key]) }
	optInt := func(table, key string) int { return coerceInt(tableOpts[table][key]) }

	metaNodes := map[string]any{"total": 0}
	metaDeployments := map[string]any{"total": 0}
	metaPods := map[string]any{"total": 0}
	for k, v := range tableOpts["nodes"] {
		metaNodes[k] = v
	}
	for k, v := range tableOpts["deployments"] {
		metaDeployments[k] = v
	}
	for k, v := range tableOpts["pods"] {
		metaPods[k] = v
	}

	summary := map[string]any{
		"nodes_total":           0,
		"nodes_ready":           0,
		"nodes_cpu_avg":         0.0,
		"nodes_mem_used_avg":    0.0,
		"pods_total":            0,
		"pods_running":          0,
		"pods_failed":           0,
		"pods_cpu_total":        0.0,
		"pods_mem_used_total":   0.0,
		"deployments_total":     0,
		"deployments_unhealthy": 0,
		"namespaces_total":      0,
	}

	result := map[string]any{
		"pods":        []any{},
		"deployments": []any{},
		"nodes":       []any{},
		"namespaces":  []any{},
		"meta": map[string]any{
			"nodes":       metaNodes,
			"deployments": metaDeployments,
			"pods":        metaPods,
		},
		"summary": summary,
		"error":   "",
		"source":  "otel",
	}
	errorsList := []string{}

	metricFormat := detectK8sMetricFormat(db)
	if metricFormat != "none" {
		result["source"] = metricFormat
	} else {
		result["source"] = "otel"
	}

	// --- Nodes ---
	if metricFormat == "prometheus" {
		// kube-state-metrics Prometheus format
		if err := func() error {
			nodeConditions := []string{
				"Attributes['node'] != ''",
				"MetricName IN ('kube_node_status_condition', 'kube_node_status_allocatable', 'kube_node_info')",
			}
			nodeParams := []any{}
			nodeConditions, nodeParams = appendOrEquals(nodeConditions, nodeParams, "Attributes['node']", nodeValues)
			if nameFilter != "" {
				nodeConditions = append(nodeConditions, "positionCaseInsensitive(Attributes['node'], ?) > 0")
				nodeParams = append(nodeParams, nameFilter)
			}

			nodeBaseSql := fmt.Sprintf(`
                SELECT
                    Attributes['node'] AS name,
                    maxIf(Value, MetricName = 'kube_node_status_condition'
                          AND Attributes['condition'] = 'Ready'
                          AND Attributes['status'] = 'true') AS ready_signal,
                    if(maxIf(Value, MetricName = 'kube_node_status_condition'
                             AND Attributes['condition'] = 'Ready'
                             AND Attributes['status'] = 'true') > 0,
                       'Ready', 'NotReady') AS status,
                    0.0 AS cpu_usage,
                    maxIf(Value, MetricName = 'kube_node_status_allocatable'
                          AND Attributes['resource'] = 'memory') AS mem_used,
                    anyIf(Attributes['kubelet_version'], MetricName = 'kube_node_info') AS version,
                    max(TimeUnix) AS last_seen
                FROM otel_metrics_gauge
                WHERE %s
                GROUP BY name
            `, strings.Join(nodeConditions, " AND "))
			statsRes, err := db.Execute(fmt.Sprintf(`
                SELECT
                    count() AS total,
                    countIf(ready_signal > 0) AS ready,
                    avg(cpu_usage) AS cpu_avg,
                    avg(mem_used) AS mem_avg
                FROM (%s)
                `, nodeBaseSql), nodeParams)
			if err != nil {
				return err
			}
			if nodeStats := statsRes.Fetchone(); nodeStats != nil {
				metaNodes["total"] = coerceInt(qStat(nodeStats, "total", 0))
				summary["nodes_total"] = coerceInt(qStat(nodeStats, "total", 0))
				summary["nodes_ready"] = coerceInt(qStat(nodeStats, "ready", 0))
				summary["nodes_cpu_avg"] = coerceFloatOr0(qStat(nodeStats, "cpu_avg", 0))
				summary["nodes_mem_used_avg"] = coerceFloatOr0(qStat(nodeStats, "mem_avg", 0))
			}
			nodeSql := fmt.Sprintf("SELECT * FROM (%s) ORDER BY %s %s LIMIT ? OFFSET ?",
				nodeBaseSql, optStr("nodes", "sort_col"), strings.ToUpper(optStr("nodes", "sort_dir")))
			rowsRes, err := db.Execute(nodeSql, append(append([]any{}, nodeParams...), optInt("nodes", "page_size"), optInt("nodes", "offset")))
			if err != nil {
				return err
			}
			nodeList := []any{}
			for _, row := range rowsRes.Fetchall() {
				status := "NotReady"
				if coerceFloatOr0(row["ready_signal"]) > 0 {
					status = "Ready"
				}
				nodeList = append(nodeList, map[string]any{
					"name":      rowString(row["name"]),
					"status":    status,
					"version":   rowString(row["version"]),
					"cpu_usage": coerceFloatOr0(row["cpu_usage"]),
					"mem_used":  coerceFloatOr0(row["mem_used"]),
					"created":   rowString(row["last_seen"]),
				})
			}
			result["nodes"] = nodeList
			return nil
		}(); err != nil {
			errorsList = append(errorsList, "nodes: "+err.Error())
		}
	} else {
		if err := func() error {
			nodeConditions := []string{"Attributes['k8s.node.name'] != ''"}
			nodeParams := []any{}
			nodeConditions, nodeParams = appendOrEquals(nodeConditions, nodeParams, "Attributes['k8s.node.name']", nodeValues)
			if nameFilter != "" {
				nodeConditions = append(nodeConditions, "positionCaseInsensitive(Attributes['k8s.node.name'], ?) > 0")
				nodeParams = append(nodeParams, nameFilter)
			}

			nodeBaseSql := fmt.Sprintf(`
                SELECT
                    Attributes['k8s.node.name'] AS name,
                    maxIf(Value, MetricName = 'k8s.node.condition_ready') AS ready_signal,
                    if(maxIf(Value, MetricName = 'k8s.node.condition_ready') > 0, 'Ready', 'NotReady') AS status,
                    maxIf(Value, MetricName = 'k8s.node.cpu.usage') AS cpu_usage,
                    maxIf(Value, MetricName = 'k8s.node.memory.usage') AS mem_used,
                    any(Attributes['k8s.kubelet.version']) AS version,
                    max(TimeUnix) AS last_seen
                FROM otel_metrics_gauge
                WHERE %s
                GROUP BY name
            `, strings.Join(nodeConditions, " AND "))
			statsRes, err := db.Execute(fmt.Sprintf(`
                SELECT
                    count() AS total,
                    countIf(ready_signal > 0) AS ready,
                    avg(cpu_usage) AS cpu_avg,
                    avg(mem_used) AS mem_avg
                FROM (%s)
                `, nodeBaseSql), nodeParams)
			if err != nil {
				return err
			}
			if nodeStats := statsRes.Fetchone(); nodeStats != nil {
				metaNodes["total"] = coerceInt(qStat(nodeStats, "total", 0))
				summary["nodes_total"] = coerceInt(qStat(nodeStats, "total", 0))
				summary["nodes_ready"] = coerceInt(qStat(nodeStats, "ready", 0))
				summary["nodes_cpu_avg"] = coerceFloatOr0(qStat(nodeStats, "cpu_avg", 0))
				summary["nodes_mem_used_avg"] = coerceFloatOr0(qStat(nodeStats, "mem_avg", 0))
			}
			nodeSql := fmt.Sprintf("SELECT * FROM (%s) ORDER BY %s %s LIMIT ? OFFSET ?",
				nodeBaseSql, optStr("nodes", "sort_col"), strings.ToUpper(optStr("nodes", "sort_dir")))
			rowsRes, err := db.Execute(nodeSql, append(append([]any{}, nodeParams...), optInt("nodes", "page_size"), optInt("nodes", "offset")))
			if err != nil {
				return err
			}
			nodeList := []any{}
			for _, row := range rowsRes.Fetchall() {
				status := "NotReady"
				if coerceFloatOr0(row["ready_signal"]) > 0 {
					status = "Ready"
				}
				nodeList = append(nodeList, map[string]any{
					"name":      rowString(row["name"]),
					"status":    status,
					"version":   rowString(row["version"]),
					"cpu_usage": coerceFloatOr0(row["cpu_usage"]),
					"mem_used":  coerceFloatOr0(row["mem_used"]),
					"created":   rowString(row["last_seen"]),
				})
			}
			result["nodes"] = nodeList
			return nil
		}(); err != nil {
			errorsList = append(errorsList, "nodes: "+err.Error())
		}
	}

	// --- Pods ---
	if metricFormat == "prometheus" {
		// kube-state-metrics + cAdvisor Prometheus format
		if err := func() error {
			podConditions := []string{
				"Attributes['pod'] != ''",
				"MetricName IN ('kube_pod_status_phase', 'kube_pod_status_ready'," +
					" 'container_memory_working_set_bytes', 'kube_pod_container_status_restarts_total'," +
					" 'kube_pod_info')",
			}
			podParams := []any{}
			podConditions, podParams = appendOrEquals(podConditions, podParams, "Attributes['namespace']", namespaceValues)
			podConditions, podParams = appendOrEquals(podConditions, podParams, "Attributes['pod']", podValues)
			if nameFilter != "" {
				podConditions = append(podConditions, "positionCaseInsensitive(Attributes['pod'], ?) > 0")
				podParams = append(podParams, nameFilter)
			}

			podBaseSql := fmt.Sprintf(`
                SELECT
                    Attributes['namespace'] AS namespace,
                    Attributes['pod'] AS name,
                    anyIf(Attributes['phase'], MetricName = 'kube_pod_status_phase'
                          AND Value > 0) AS phase,
                    maxIf(Value, MetricName = 'kube_pod_status_ready'
                          AND Attributes['condition'] = 'true') AS ready_signal,
                    0.0 AS cpu_usage,
                      sumIf(Value, MetricName = 'container_memory_working_set_bytes'
                          AND Attributes['container'] != 'POD') AS mem_used,
                    toInt64(maxIf(Value, MetricName = 'kube_pod_container_status_restarts_total'))
                        AS restarts,
                    anyIf(Attributes['node'], MetricName = 'kube_pod_info') AS node,
                    max(TimeUnix) AS last_seen
                FROM otel_metrics_gauge
                WHERE %s
                GROUP BY namespace, name
            `, strings.Join(podConditions, " AND "))

			// Also check otel_metrics_sum for restart counter (some exporters store _total there)
			podSumConditions := []string{
				"Attributes['pod'] != ''",
				"MetricName = 'kube_pod_container_status_restarts_total'",
			}
			podSumParams := []any{}
			podSumConditions, podSumParams = appendOrEquals(podSumConditions, podSumParams, "Attributes['namespace']", namespaceValues)
			podSumConditions, podSumParams = appendOrEquals(podSumConditions, podSumParams, "Attributes['pod']", podValues)
			if nameFilter != "" {
				podSumConditions = append(podSumConditions, "positionCaseInsensitive(Attributes['pod'], ?) > 0")
				podSumParams = append(podSumParams, nameFilter)
			}

			podSumSql := fmt.Sprintf(`
                SELECT
                    Attributes['namespace'] AS namespace,
                    Attributes['pod'] AS name,
                    '' AS phase,
                    0.0 AS ready_signal,
                    0.0 AS cpu_usage,
                    0.0 AS mem_used,
                    toInt64(max(Value)) AS restarts,
                    '' AS node,
                    max(TimeUnix) AS last_seen
                FROM otel_metrics_sum
                WHERE %s
                GROUP BY namespace, name
            `, strings.Join(podSumConditions, " AND "))

			podMergedSql := fmt.Sprintf(`
                SELECT
                    namespace,
                    name,
                    anyIf(phase, phase != '') AS phase,
                    max(ready_signal) AS ready_signal,
                    max(cpu_usage) AS cpu_usage,
                    max(mem_used) AS mem_used,
                    max(restarts) AS restarts,
                    anyIf(node, node != '') AS node,
                    max(last_seen) AS last_seen
                FROM (%s UNION ALL %s)
                GROUP BY namespace, name
            `, podBaseSql, podSumSql)
			podMergedParams := append(append([]any{}, podParams...), podSumParams...)

			statsRes, err := db.Execute(fmt.Sprintf(`
                SELECT
                    count() AS total,
                    countIf(phase = 'Running') AS running,
                    countIf(phase = 'Failed') AS failed,
                    sum(cpu_usage) AS cpu_total,
                    sum(mem_used) AS mem_total
                FROM (%s)
                `, podMergedSql), podMergedParams)
			if err != nil {
				return err
			}
			if podStats := statsRes.Fetchone(); podStats != nil {
				metaPods["total"] = coerceInt(qStat(podStats, "total", 0))
				summary["pods_total"] = coerceInt(qStat(podStats, "total", 0))
				summary["pods_running"] = coerceInt(qStat(podStats, "running", 0))
				summary["pods_failed"] = coerceInt(qStat(podStats, "failed", 0))
				summary["pods_cpu_total"] = coerceFloatOr0(qStat(podStats, "cpu_total", 0))
				summary["pods_mem_used_total"] = coerceFloatOr0(qStat(podStats, "mem_total", 0))
			}
			podSql := fmt.Sprintf("SELECT * FROM (%s) ORDER BY %s %s LIMIT ? OFFSET ?",
				podMergedSql, optStr("pods", "sort_col"), strings.ToUpper(optStr("pods", "sort_dir")))
			rowsRes, err := db.Execute(podSql, append(append([]any{}, podMergedParams...), optInt("pods", "page_size"), optInt("pods", "offset")))
			if err != nil {
				return err
			}
			result["pods"] = buildPodList(rowsRes.Fetchall())
			return nil
		}(); err != nil {
			errorsList = append(errorsList, "pods: "+err.Error())
		}
	} else {
		if err := func() error {
			podConditions := []string{"Attributes['k8s.pod.name'] != ''"}
			podParams := []any{}
			podConditions, podParams = appendOrEquals(podConditions, podParams, "Attributes['k8s.namespace.name']", namespaceValues)
			podConditions, podParams = appendOrEquals(podConditions, podParams, "Attributes['k8s.pod.name']", podValues)
			if nameFilter != "" {
				podConditions = append(podConditions, "positionCaseInsensitive(Attributes['k8s.pod.name'], ?) > 0")
				podParams = append(podParams, nameFilter)
			}

			podBaseSql := fmt.Sprintf(`
                SELECT
                    Attributes['k8s.namespace.name'] AS namespace,
                    Attributes['k8s.pod.name'] AS name,
                    any(Attributes['k8s.pod.phase']) AS phase,
                    maxIf(Value, MetricName = 'k8s.pod.status_ready') AS ready_signal,
                    maxIf(Value, MetricName = 'k8s.pod.cpu.usage') AS cpu_usage,
                    maxIf(Value, MetricName = 'k8s.pod.memory.usage') AS mem_used,
                    maxIf(toInt64(Value), MetricName = 'k8s.container.restart_count') AS restarts,
                    any(Attributes['k8s.node.name']) AS node,
                    max(TimeUnix) AS last_seen
                FROM otel_metrics_gauge
                WHERE %s
                GROUP BY namespace, name
            `, strings.Join(podConditions, " AND "))
			statsRes, err := db.Execute(fmt.Sprintf(`
                SELECT
                    count() AS total,
                    countIf(phase = 'Running') AS running,
                    countIf(phase = 'Failed') AS failed,
                    sum(cpu_usage) AS cpu_total,
                    sum(mem_used) AS mem_total
                FROM (%s)
                `, podBaseSql), podParams)
			if err != nil {
				return err
			}
			if podStats := statsRes.Fetchone(); podStats != nil {
				metaPods["total"] = coerceInt(qStat(podStats, "total", 0))
				summary["pods_total"] = coerceInt(qStat(podStats, "total", 0))
				summary["pods_running"] = coerceInt(qStat(podStats, "running", 0))
				summary["pods_failed"] = coerceInt(qStat(podStats, "failed", 0))
				summary["pods_cpu_total"] = coerceFloatOr0(qStat(podStats, "cpu_total", 0))
				summary["pods_mem_used_total"] = coerceFloatOr0(qStat(podStats, "mem_total", 0))
			}
			podSql := fmt.Sprintf("SELECT * FROM (%s) ORDER BY %s %s LIMIT ? OFFSET ?",
				podBaseSql, optStr("pods", "sort_col"), strings.ToUpper(optStr("pods", "sort_dir")))
			rowsRes, err := db.Execute(podSql, append(append([]any{}, podParams...), optInt("pods", "page_size"), optInt("pods", "offset")))
			if err != nil {
				return err
			}
			result["pods"] = buildPodList(rowsRes.Fetchall())
			return nil
		}(); err != nil {
			errorsList = append(errorsList, "pods: "+err.Error())
		}
	}

	// --- Deployments ---
	if metricFormat == "prometheus" {
		// kube-state-metrics Prometheus format
		if err := func() error {
			deployConditions := []string{
				"Attributes['deployment'] != ''",
				"MetricName IN ('kube_deployment_spec_replicas'," +
					" 'kube_deployment_status_replicas_ready'," +
					" 'kube_deployment_status_replicas_available'," +
					" 'kube_deployment_status_replicas_updated'," +
					" 'kube_deployment_status_replicas')",
			}
			deployParams := []any{}
			deployConditions, deployParams = appendOrEquals(deployConditions, deployParams, "Attributes['namespace']", namespaceValues)
			deployConditions, deployParams = appendOrEquals(deployConditions, deployParams, "Attributes['deployment']", deploymentValues)
			if nameFilter != "" {
				deployConditions = append(deployConditions, "positionCaseInsensitive(Attributes['deployment'], ?) > 0")
				deployParams = append(deployParams, nameFilter)
			}

			deployBaseSql := fmt.Sprintf(`
                SELECT
                    Attributes['namespace'] AS namespace,
                    Attributes['deployment'] AS name,
                    toInt64(maxIf(Value, MetricName = 'kube_deployment_spec_replicas'))
                        AS desired,
                    toInt64(maxIf(Value, MetricName = 'kube_deployment_status_replicas_ready'))
                        AS ready,
                    toInt64(maxIf(Value, MetricName = 'kube_deployment_status_replicas_available'))
                        AS available,
                    toInt64(maxIf(Value, MetricName = 'kube_deployment_status_replicas_updated'))
                        AS updated,
                    max(TimeUnix) AS last_seen
                FROM otel_metrics_gauge
                WHERE %s
                GROUP BY namespace, name
            `, strings.Join(deployConditions, " AND "))
			deployTotal := countQuery(fmt.Sprintf("SELECT count(*) AS cnt FROM (%s)", deployBaseSql), deployParams)
			metaDeployments["total"] = deployTotal
			summary["deployments_total"] = deployTotal
			summary["deployments_unhealthy"] = countQuery(
				fmt.Sprintf("SELECT count(*) AS cnt FROM (%s) WHERE ready < desired", deployBaseSql), deployParams)
			deploySql := fmt.Sprintf("SELECT * FROM (%s) ORDER BY %s %s LIMIT ? OFFSET ?",
				deployBaseSql, optStr("deployments", "sort_col"), strings.ToUpper(optStr("deployments", "sort_dir")))
			rowsRes, err := db.Execute(deploySql, append(append([]any{}, deployParams...), optInt("deployments", "page_size"), optInt("deployments", "offset")))
			if err != nil {
				return err
			}
			result["deployments"] = buildDeploymentList(rowsRes.Fetchall())
			return nil
		}(); err != nil {
			errorsList = append(errorsList, "deployments: "+err.Error())
		}
	} else {
		if err := func() error {
			deployConditions := []string{"Attributes['k8s.deployment.name'] != ''"}
			deployParams := []any{}
			deployConditions, deployParams = appendOrEquals(deployConditions, deployParams, "Attributes['k8s.namespace.name']", namespaceValues)
			deployConditions, deployParams = appendOrEquals(deployConditions, deployParams, "Attributes['k8s.deployment.name']", deploymentValues)
			if nameFilter != "" {
				deployConditions = append(deployConditions, "positionCaseInsensitive(Attributes['k8s.deployment.name'], ?) > 0")
				deployParams = append(deployParams, nameFilter)
			}

			deployBaseSql := fmt.Sprintf(`
                SELECT
                    Attributes['k8s.namespace.name'] AS namespace,
                    Attributes['k8s.deployment.name'] AS name,
                    maxIf(toInt64(Value), MetricName = 'k8s.deployment.desired') AS desired,
                    maxIf(toInt64(Value), MetricName = 'k8s.deployment.ready') AS ready,
                    maxIf(toInt64(Value), MetricName = 'k8s.deployment.available') AS available,
                    maxIf(toInt64(Value), MetricName = 'k8s.deployment.updated') AS updated,
                    max(TimeUnix) AS last_seen
                FROM otel_metrics_gauge
                WHERE %s
                GROUP BY namespace, name
            `, strings.Join(deployConditions, " AND "))
			deployTotal := countQuery(fmt.Sprintf("SELECT count(*) AS cnt FROM (%s)", deployBaseSql), deployParams)
			metaDeployments["total"] = deployTotal
			summary["deployments_total"] = deployTotal
			summary["deployments_unhealthy"] = countQuery(
				fmt.Sprintf("SELECT count(*) AS cnt FROM (%s) WHERE ready < desired", deployBaseSql), deployParams)
			deploySql := fmt.Sprintf("SELECT * FROM (%s) ORDER BY %s %s LIMIT ? OFFSET ?",
				deployBaseSql, optStr("deployments", "sort_col"), strings.ToUpper(optStr("deployments", "sort_dir")))
			rowsRes, err := db.Execute(deploySql, append(append([]any{}, deployParams...), optInt("deployments", "page_size"), optInt("deployments", "offset")))
			if err != nil {
				return err
			}
			result["deployments"] = buildDeploymentList(rowsRes.Fetchall())
			return nil
		}(); err != nil {
			errorsList = append(errorsList, "deployments: "+err.Error())
		}
	}
	// --- Namespaces ---
	if metricFormat == "prometheus" {
		// kube-state-metrics Prometheus format
		if err := func() error {
			rowsRes, err := db.Execute(`
                SELECT
                    Attributes['namespace'] AS name,
                    anyIf(Attributes['phase'], Value > 0) AS status,
                    max(TimeUnix) AS last_seen
                FROM otel_metrics_gauge
                WHERE Attributes['namespace'] != ''
                AND MetricName = 'kube_namespace_status_phase'
                GROUP BY name
                ORDER BY name
                `)
			if err != nil {
				return err
			}
			nsList := []any{}
			for _, row := range rowsRes.Fetchall() {
				nsList = append(nsList, map[string]any{
					"name":    rowString(row["name"]),
					"status":  strOrDefault(row["status"], "Unknown"),
					"created": rowString(row["last_seen"]),
				})
			}
			result["namespaces"] = nsList
			summary["namespaces_total"] = len(nsList)
			return nil
		}(); err != nil {
			errorsList = append(errorsList, "namespaces: "+err.Error())
		}
	} else {
		if err := func() error {
			rowsRes, err := db.Execute(`
                SELECT
                    Attributes['k8s.namespace.name'] AS name,
                    max(TimeUnix) AS last_seen
                FROM otel_metrics_gauge
                WHERE Attributes['k8s.namespace.name'] != ''
                GROUP BY name
                ORDER BY name
                `)
			if err != nil {
				return err
			}
			nsList := []any{}
			for _, row := range rowsRes.Fetchall() {
				nsList = append(nsList, map[string]any{
					"name":    rowString(row["name"]),
					"status":  "Active",
					"created": rowString(row["last_seen"]),
				})
			}
			result["namespaces"] = nsList
			summary["namespaces_total"] = len(nsList)
			return nil
		}(); err != nil {
			errorsList = append(errorsList, "namespaces: "+err.Error())
		}
	}

	if len(errorsList) > 0 {
		result["error"] = strings.Join(errorsList, "; ")
	} else if !pyTruthy(result["pods"]) && !pyTruthy(result["deployments"]) && !pyTruthy(result["nodes"]) && !pyTruthy(result["namespaces"]) {
		result["error"] = "No Kubernetes data found yet. Deploy OTEL collectors (kubeletstats/k8s_cluster) or" +
			" configure an OTEL Prometheus receiver scraping kube-state-metrics and cAdvisor."
	}

	return result
}

// buildPodList mirrors the per-row pod dict construction shared by the
// Prometheus and OTEL pod query branches.
func buildPodList(rows []Row) []any {
	out := []any{}
	for _, row := range rows {
		out = append(out, map[string]any{
			"namespace": strOrDefault(row["namespace"], "default"),
			"name":      rowString(row["name"]),
			"phase":     strOrDefault(row["phase"], "Unknown"),
			"ready":     coerceFloatOr0(row["ready_signal"]) > 0,
			"cpu_usage": coerceFloatOr0(row["cpu_usage"]),
			"mem_used":  coerceFloatOr0(row["mem_used"]),
			"restarts":  coerceInt(row["restarts"]),
			"node":      rowString(row["node"]),
			"created":   rowString(row["last_seen"]),
		})
	}
	return out
}

// buildDeploymentList mirrors the per-row deployment dict construction shared
// by the Prometheus and OTEL deployment query branches.
func buildDeploymentList(rows []Row) []any {
	out := []any{}
	for _, row := range rows {
		out = append(out, map[string]any{
			"namespace": strOrDefault(row["namespace"], "default"),
			"name":      rowString(row["name"]),
			"desired":   coerceInt(row["desired"]),
			"ready":     coerceInt(row["ready"]),
			"available": coerceInt(row["available"]),
			"updated":   coerceInt(row["updated"]),
			"created":   rowString(row["last_seen"]),
		})
	}
	return out
}

// strOrDefault mirrors str(value or default): returns default when the string
// form of value is empty.
func strOrDefault(value any, def string) string {
	s := rowString(value)
	if s == "" {
		return def
	}
	return s
}

// viewK8sSettings renders the Kubernetes health view settings page.
func viewK8sSettings(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	settings := loadK8sSettings(db)
	flashMsg := r.URL.Query().Get("msg")
	flashType := r.URL.Query().Get("msg_type")
	if flashType == "" {
		flashType = "success"
	}
	renderTemplate(w, r, "settings_kubernetes.html", map[string]any{
		"k8s_settings": settings,
		"flash_msg":    flashMsg,
		"flash_type":   flashType,
	})
}

// saveK8sSettings persists Kubernetes health view settings.
func saveK8sSettings(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := map[string]string{}
	for key := range r.PostForm {
		form[key] = r.PostForm.Get(key)
	}
	newSettings := k8sSettingsFromForm(form)
	db := getDb()
	for key, value := range newSettings {
		if value != "" {
			setAppSetting(db, key, value)
		} else {
			delAppSetting(db, key)
		}
	}
	http.Redirect(w, r, "/settings/kubernetes?msg=Settings+saved&msg_type=success", http.StatusFound)
}

// viewKubernetes renders the Kubernetes health dashboard page.
func viewKubernetes(w http.ResponseWriter, r *http.Request) {
	if !kubernetesEnabled() {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("Kubernetes health view is disabled. Enable it in Settings → Kubernetes."))
		return
	}
	renderTemplate(w, r, "kubernetes.html", nil)
}

// apiKubernetesStatus returns current Kubernetes health data from OTEL tables.
func apiKubernetesStatus(w http.ResponseWriter, r *http.Request) {
	if !kubernetesEnabled() {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Kubernetes health view is disabled."})
		return
	}

	args := r.URL.Query()
	qInt := func(name string, def, lo, hi int) int {
		raw := strings.TrimSpace(args.Get(name))
		if raw == "" {
			raw = strconv.Itoa(def)
		}
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			parsed = def
		}
		return max(lo, min(hi, parsed))
	}
	qList := func(name string) []string {
		out := []string{}
		for _, v := range args[name] {
			if t := strings.TrimSpace(v); t != "" {
				out = append(out, t)
			}
		}
		return out
	}

	queryOpts := map[string]any{
		"namespace":             strings.TrimSpace(args.Get("namespace")),
		"namespace_values":      qList("namespace"),
		"node_values":           qList("node"),
		"deployment_values":     qList("deployment"),
		"pod_values":            qList("pod"),
		"name":                  strings.TrimSpace(args.Get("name")),
		"nodes_sort":            strings.TrimSpace(args.Get("nodes_sort")),
		"nodes_dir":             strings.ToLower(strings.TrimSpace(args.Get("nodes_dir"))),
		"nodes_page":            qInt("nodes_page", 1, 1, 1000000),
		"nodes_page_size":       qInt("nodes_page_size", 25, 1, 200),
		"deployments_sort":      strings.TrimSpace(args.Get("deployments_sort")),
		"deployments_dir":       strings.ToLower(strings.TrimSpace(args.Get("deployments_dir"))),
		"deployments_page":      qInt("deployments_page", 1, 1, 1000000),
		"deployments_page_size": qInt("deployments_page_size", 25, 1, 200),
		"pods_sort":             strings.TrimSpace(args.Get("pods_sort")),
		"pods_dir":              strings.ToLower(strings.TrimSpace(args.Get("pods_dir"))),
		"pods_page":             qInt("pods_page", 1, 1, 1000000),
		"pods_page_size":        qInt("pods_page_size", 25, 1, 200),
	}

	db := getDb()
	data := fetchK8sFromOtel(db, queryOpts)
	data["ok"] = true
	jsonResponse(w, http.StatusOK, data)
}
