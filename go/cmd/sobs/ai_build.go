package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

var namedQueryNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// stripCodeFences removes a leading/trailing ``` fence (mirrors the re.sub pair in the vanna helpers).
func stripCodeFences(s string) string {
	s = reChartFenceOpen.ReplaceAllString(s, "")
	s = reChartFenceClose.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// POST /api/dashboards/spec/ai-build — app.py ai_build_chart_spec: generate a dashboard chart spec
// from a natural-language question via the vanna LLM pipeline (generate SQL -> execute -> named
// queries -> chart option), returning a custom_echarts spec.
func (s *server) handleApiDashboardsSpecAiBuild(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	question := strings.TrimSpace(bstr(m, "question"))
	if question == "" {
		s.errorJSON(w, http.StatusBadRequest, "question is required")
		return
	}
	endpoint := strings.TrimSpace(s.loadAISetting("ai.endpoint_url", ""))
	model := strings.TrimSpace(s.loadAISetting("ai.model", ""))
	if endpoint == "" || model == "" {
		writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().
			Set("ok", false).Set("error", "AI endpoint not configured. Visit Settings → AI Configuration."))
		return
	}

	sql, sqlErr, _ := s.generateSQLViaLLM(endpoint, question)
	if sqlErr != "" {
		writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().
			Set("ok", false).Set("error", "SQL generation failed: "+sqlErr))
		return
	}

	finalSQL, columns, rows, execErr, retryCount := s.validateAndExecuteVannaSQL(sql)
	if execErr != "" || columns == nil {
		writeJSON(w, http.StatusUnprocessableEntity, jsonenc.NewObject().
			Set("ok", false).Set("error", orDefault(execErr, "Generated SQL could not be executed.")).Set("sql", finalSQL))
		return
	}
	sql = finalSQL

	fieldTypes := inferQueryFieldTypes(columns, rows)
	datasets := []any{jsonenc.NewObject().
		Set("name", "main").Set("purpose", "primary dataset").Set("sql", sql).
		Set("columns", columns).Set("rows", rows)}

	// Optional named datasets for complex multi-dataset charts.
	namedQueryResults := []any{}
	if len(columns) > 0 {
		namedRaw := s.generateNamedQueries(endpoint, question, sql)
		namedQueryResults = s.executeNamedQueries(namedRaw, 1000)
		for _, nqv := range namedQueryResults {
			nq, ok := nqv.(*jsonenc.Object)
			if !ok || objGetStr(nq, "error") != "" {
				continue
			}
			datasets = append(datasets, jsonenc.NewObject().
				Set("name", objGetStr(nq, "name")).Set("purpose", objGetStr(nq, "purpose")).
				Set("sql", objGetStr(nq, "sql")).
				Set("columns", objGetOr(nq, "columns", []any{})).Set("rows", objGetOr(nq, "rows", []any{})))
		}
	}

	// Generate the ECharts option JSON via LLM (falling back to a safe template on failure).
	chartSpecJSON, chartError := "", ""
	customMappingJSON := "{}"
	if len(columns) > 0 {
		chartSpecJSON, chartError = s.generateChartSpec(endpoint, question, columns, rows, namedQueryResults)
		if chartSpecJSON != "" {
			customMappingJSON = "{}"
		} else {
			chartSpecJSON = buildFallbackCustomOptionJSON()
			customMappingJSON = string(jsonenc.Encode(
				jsonenc.NewObject().Set("points", jsonenc.NewObject().Set("from", "rows")), dumpsDefault))
			if chartError != "" {
				chartError = chartError + " Using fallback chart option template."
			} else {
				chartError = "Chart generation failed; using fallback chart option template."
			}
		}
	}

	namedQueries := []any{}
	for _, nqv := range namedQueryResults {
		nq, ok := nqv.(*jsonenc.Object)
		if !ok || objGetStr(nq, "error") != "" || objGetStr(nq, "name") == "" || objGetStr(nq, "sql") == "" {
			continue
		}
		namedQueries = append(namedQueries, jsonenc.NewObject().
			Set("name", objGetStr(nq, "name")).Set("sql", objGetStr(nq, "sql")).Set("purpose", objGetStr(nq, "purpose")))
	}

	spec := jsonenc.NewObject().
		Set("template_id", "custom_echarts").
		Set("sql", jsonenc.NewObject().Set("mode", "raw").Set("override_sql", sql)).
		Set("named_queries", namedQueries).
		Set("visual", jsonenc.NewObject().
			Set("custom_option_json", orDefault(chartSpecJSON, "{}")).
			Set("custom_mapping_json", customMappingJSON))

	_ = fieldTypes
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("spec", spec).Set("sql", sql).Set("retry_count", retryCount).
		Set("columns", columns).Set("named_queries", namedQueries).
		Set("named_query_results", namedQueryResults).Set("chart_error", chartError))
}

// validateAndExecuteVannaSQL mirrors _vanna_validate_and_execute_with_repair for a valid SQL:
// validate read-only/allowlist (app.py _vanna_run_query -> validate_sql), then execute and return
// columns/rows. A blocked statement yields the "SQL validation error: …" error the route surfaces
// as a 422. (The EXPLAIN-fail -> AI repair retry loop is a follow-up; the parity canned SQL is
// valid, so retry_count stays 0.)
func (s *server) validateAndExecuteVannaSQL(sql string) (string, []any, []any, string, int) {
	if vErr := validateSQL(sql); vErr != "" {
		return sql, nil, nil, "SQL validation error: " + vErr, 0
	}
	res, err := s.db.Execute(sql)
	if err != nil {
		return sql, nil, nil, publicDashboardQueryError(err), 0
	}
	columns, rows := serializeQueryResult(res)
	return sql, columns, rows, "", 0
}

// generateNamedQueries mirrors _vanna_generate_named_queries: parse the LLM reply as
// {"datasets":[{name,sql,purpose}]}. A non-JSON reply yields no named queries.
func (s *server) generateNamedQueries(endpoint, question, baseSQL string) []any {
	userMsg := "Question: " + question + "\n\n" +
		"Preferred chart type: \nChart instruction: \n\n" +
		"Primary SQL:\n" + baseSQL + "\n\n" +
		"Schema context:\n" + s.getSchemaContext() + "\n\n" +
		"If one dataset is sufficient, return an empty datasets array. " +
		"For network/flow charts (graph/sankey/chord), prefer separate nodes and links datasets."
	messages := []any{
		jsonenc.NewObject().Set("role", "system").Set("content", namedQueriesSystemPrompt),
		jsonenc.NewObject().Set("role", "user").Set("content", userMsg),
	}
	raw, _, err := s.callLLMChat(llmRequest{
		endpoint:      endpoint,
		model:         strings.TrimSpace(s.loadAISetting("ai.model", "")),
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: strings.TrimSpace(s.loadAISetting("ai.thinking_level", "off")),
		messages:      messages,
	})
	if err != nil || raw == "" {
		return []any{}
	}
	planText := stripCodeFences(strings.TrimSpace(raw))
	if first, last := strings.Index(planText, "{"), strings.LastIndex(planText, "}"); first >= 0 && last > first {
		planText = strings.TrimSpace(planText[first : last+1])
	}
	parsed, perr := parseJSONValue([]byte(planText))
	if perr != nil {
		return []any{}
	}
	obj, ok := parsed.(*jsonenc.Object)
	if !ok {
		return []any{}
	}
	dsV, _ := obj.Get("datasets")
	rawDS, ok := dsV.([]any)
	if !ok {
		return []any{}
	}
	out := []any{}
	baseNorm := strings.TrimRight(strings.TrimSpace(baseSQL), ";")
	for i, item := range rawDS {
		if i >= 3 {
			break
		}
		o, ok := item.(*jsonenc.Object)
		if !ok {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(objGetStr(o, "name")))
		nqSQL := strings.TrimRight(strings.TrimSpace(objGetStr(o, "sql")), ";")
		purpose := strings.TrimSpace(objGetStr(o, "purpose"))
		if name == "" || !namedQueryNameRE.MatchString(name) {
			continue
		}
		up := strings.TrimLeft(strings.ToUpper(nqSQL), " ")
		if !strings.HasPrefix(up, "SELECT") && !strings.HasPrefix(up, "WITH") {
			continue
		}
		if nqSQL == baseNorm {
			continue
		}
		out = append(out, jsonenc.NewObject().Set("name", name).Set("sql", nqSQL).Set("purpose", purpose))
	}
	return out
}

// generateChartSpec mirrors _vanna_generate_chart_spec: ask the LLM for an ECharts option JSON, with
// a local + LLM repair pass. Returns (option_json, error); a non-JSON reply yields ("", parse error).
func (s *server) generateChartSpec(endpoint, question string, columns, sampleRows, namedDatasets []any) (string, string) {
	rows := sampleRows
	if len(rows) > 20 {
		rows = rows[:20]
	}
	compact := jsonenc.Options{SortKeys: false, EnsureASCII: false, ItemSep: ",", KeySep: ":"}
	sampleStr := string(jsonenc.Encode(jsonenc.NewObject().Set("columns", columns).Set("rows", rows), compact))
	namedStr := ""
	if len(namedDatasets) > 0 {
		condensed := []any{}
		for _, dsv := range namedDatasets {
			ds, ok := dsv.(*jsonenc.Object)
			if !ok {
				continue
			}
			dr := objGetArr(ds, "rows")
			if len(dr) > 20 {
				dr = dr[:20]
			}
			condensed = append(condensed, jsonenc.NewObject().
				Set("name", objGetStr(ds, "name")).Set("purpose", objGetStr(ds, "purpose")).
				Set("columns", objGetOr(ds, "columns", []any{})).Set("rows", dr))
		}
		if len(condensed) > 0 {
			namedStr = "\n\nNamed datasets (use when multi-dataset chart structures are needed):\n" +
				string(jsonenc.Encode(condensed, compact))
		}
	}
	userMsg := "Original question: " + question + "\n\n" +
		"Result set (columns + up to 20 sample rows):\n" + sampleStr + "\n\n" +
		namedStr + "Produce an ECharts option JSON object for this data."
	chatReq := llmRequest{
		endpoint:      endpoint,
		model:         strings.TrimSpace(s.loadAISetting("ai.model", "")),
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: strings.TrimSpace(s.loadAISetting("ai.thinking_level", "off")),
		messages: []any{
			jsonenc.NewObject().Set("role", "system").Set("content", chartSystemPrompt),
			jsonenc.NewObject().Set("role", "user").Set("content", userMsg),
		},
	}
	raw, _, err := s.callLLMChat(chatReq)
	if err != nil || strings.TrimSpace(raw) == "" {
		return "", "LLM did not return a chart spec."
	}
	parsed, _ := parseChartSpecJSON(raw)
	if parsed != nil {
		if len(parsed.Keys()) == 0 {
			return "", "LLM returned an empty chart spec object."
		}
		return string(jsonenc.Encode(parsed, dumpsDefault)), ""
	}
	parseErr := chartSpecParseError(raw)
	repairReq := chatReq
	repairReq.messages = []any{
		jsonenc.NewObject().Set("role", "system").Set("content", "You repair malformed JSON. Return ONLY corrected, valid JSON — no markdown, no commentary."),
		jsonenc.NewObject().Set("role", "user").Set("content", "The following should be a JSON ECharts option but failed to parse ("+parseErr+"). Return ONLY the corrected JSON:\n"+raw),
	}
	repaired, _, rerr := s.callLLMChat(repairReq)
	if rerr != nil || strings.TrimSpace(repaired) == "" {
		return "", "Chart spec JSON parse error: " + parseErr + ". LLM JSON repair returned empty content."
	}
	rparsed, _ := parseChartSpecJSON(repaired)
	if rparsed == nil {
		return "", "Chart spec JSON parse error: " + parseErr + ". LLM JSON repair output was still invalid: " + chartSpecParseError(repaired)
	}
	if len(rparsed.Keys()) == 0 {
		return "", "LLM JSON repair returned an empty chart spec object."
	}
	return string(jsonenc.Encode(rparsed, dumpsDefault)), ""
}

// chartSpecParseError returns the SAME message CPython's json.loads emits when the chart reply is
// not JSON (the route surfaces it in chart_error). _normalize_chart_spec_text leaves a non-brace
// reply unchanged, so json.loads fails at the first non-whitespace char with "Expecting value".
func chartSpecParseError(raw string) string {
	spec := normalizeChartSpecText(raw)
	if spec == "" {
		return "empty chart spec"
	}
	return pyExpectingValueError(spec)
}

// pyExpectingValueError mirrors CPython json.JSONDecodeError("Expecting value", doc, pos) for a
// document whose first non-whitespace character is not a valid JSON value start.
func pyExpectingValueError(s string) string {
	pos := 0
	for pos < len(s) {
		if c := s[pos]; c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			pos++
		} else {
			break
		}
	}
	line, lastNL := 1, -1
	for i := 0; i < pos; i++ {
		if s[i] == '\n' {
			line++
			lastNL = i
		}
	}
	return fmt.Sprintf("Expecting value: line %d column %d (char %d)", line, pos-lastNL, pos)
}

// buildFallbackCustomOptionJSON mirrors _build_fallback_custom_option_json.
func buildFallbackCustomOptionJSON() string {
	opt := jsonenc.NewObject().
		Set("backgroundColor", "transparent").
		Set("tooltip", jsonenc.NewObject().Set("trigger", "axis")).
		Set("xAxis", jsonenc.NewObject().Set("type", "category")).
		Set("yAxis", jsonenc.NewObject().Set("type", "value")).
		Set("series", []any{jsonenc.NewObject().
			Set("name", "Value").Set("type", "line").Set("data", "{{points}}").
			Set("showSymbol", false).Set("smooth", true)})
	return string(jsonenc.Encode(opt, dumpsDefault))
}

func objGetOr(o *jsonenc.Object, key string, def any) any {
	if v, ok := o.Get(key); ok {
		return v
	}
	return def
}
