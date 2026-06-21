package main

import (
	"sort"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// fix_query.go — divergence fixes for the Query page execution path. Holds the EXPLAIN preflight,
// the records-carrying named-query executor (CHARTS/MUT2 contract), the do_chart LLM-stage wrappers
// that capture llm_stats, and the observed-OTEL-attr-key accessor for the schema context. These are
// all populated-data / configured-feature / chart paths the empty golden corpus never exercises, so
// they leave the byte-tested empty-success path unchanged.

// NOTE: vannaExplainSQL (app.py _vanna_explain_sql) already exists in fix_ai_build.go and is reused
// by handleApiQueryRun's EXPLAIN preflight — not re-declared here.

// executeNamedQueriesWithRecords mirrors app.py _execute_chart_spec_named_queries(include_records=True)
// (app.py:22213): run each {name, sql, purpose} with a LIMIT and collect
// {name, purpose, columns, rows, error, records}. `records` is one *jsonenc.Object per row (column ->
// typed value), like Python's `[dict(row) for row in nq_rows]`. The MUT2 agent's
// renderChartFromTemplateWithNamed consumes the `records` key. This is a sibling of executeNamedQueries
// (the existing include_records=False form) — the existing one is intentionally left unchanged.
func (s *server) executeNamedQueriesWithRecords(named []any, defaultLimit int) []any {
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
			results = append(results, item.
				Set("columns", []any{}).Set("rows", []any{}).
				Set("error", publicDashboardQueryError(err)).Set("records", []any{}))
			continue
		}
		cols, rows := serializeQueryResult(res)
		_, dictRows := serializeQueryDictRows(res)
		records := make([]any, len(dictRows))
		for i, m := range dictRows {
			// Preserve column order in each record dict (Python dict(row) keeps cursor order).
			rec := jsonenc.NewObject()
			for _, c := range res.Columns {
				rec.Set(c, m[c])
			}
			records[i] = rec
		}
		results = append(results, item.
			Set("columns", cols).Set("rows", rows).Set("error", "").Set("records", records))
	}
	return results
}

// getCachedAttrKeys mirrors app.py _get_cached_attr_keys (app.py:2187): prime the shared log-attr-key
// cache once, then return the sorted distinct keys for *recordType*. Read-only; reuses the same
// logAttrKeyCache the ingest writers populate.
func (s *server) getCachedAttrKeys(recordType string) []string {
	c := logAttrKeyCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prime(s)
	set := c.byType[recordType]
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// compactAttrKeyLine mirrors ChdbSqlRunner._compact_attr_key_line (app.py:29150): "<label>: k1, k2, …"
// over up to maxKeys observed attribute keys (", ..." suffix when more exist), or "" when none.
func (s *server) compactAttrKeyLine(recordType, label string, maxKeys int) string {
	keys := s.getCachedAttrKeys(recordType)
	if len(keys) == 0 {
		return ""
	}
	shown := keys
	suffix := ""
	if len(keys) > maxKeys {
		shown = keys[:maxKeys]
		suffix = ", ..."
	}
	return label + ": " + strings.Join(shown, ", ") + suffix
}

// generateNamedQueriesStats mirrors generateNamedQueries (ai_build.go _vanna_generate_named_queries)
// but ALSO returns the LLM usage stats so api_query_run's do_chart branch can populate
// named_query_generation in llm_stats. Logic is a faithful copy of generateNamedQueries; the only
// difference is the captured stats return. (generateNamedQueries lives in ai_build.go, owned by
// another agent, so this stats variant is kept here rather than modifying it.)
func (s *server) generateNamedQueriesStats(endpoint, question, baseSQL, preferredChartType, chartInstruction string) ([]any, llmStats) {
	preferred := preferredChartType
	if preferred == "" {
		preferred = "auto"
	}
	userMsg := "Question: " + question + "\n\n" +
		"Preferred chart type: " + preferred + "\nChart instruction: " + chartInstruction + "\n\n" +
		"Primary SQL:\n" + baseSQL + "\n\n" +
		"Schema context:\n" + s.getSchemaContext() + "\n\n" +
		"If one dataset is sufficient, return an empty datasets array. " +
		"For network/flow charts (graph/sankey/chord), prefer separate nodes and links datasets."
	messages := []any{
		jsonenc.NewObject().Set("role", "system").Set("content", namedQueriesSystemPrompt),
		jsonenc.NewObject().Set("role", "user").Set("content", userMsg),
	}
	raw, stats, err := s.callLLMChat(llmRequest{
		endpoint:      endpoint,
		model:         strings.TrimSpace(s.loadAISetting("ai.model", "")),
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: strings.TrimSpace(s.loadAISetting("ai.thinking_level", "off")),
		maxTokens:     queryLLMMaxTokens, // app.py _vanna_generate_named_queries: max_tokens=_QUERY_LLM_MAX_TOKENS
		messages:      messages,
	})
	if err != nil || raw == "" {
		return []any{}, stats
	}
	planText := stripCodeFences(strings.TrimSpace(raw))
	if first, last := strings.Index(planText, "{"), strings.LastIndex(planText, "}"); first >= 0 && last > first {
		planText = strings.TrimSpace(planText[first : last+1])
	}
	parsed, perr := parseJSONValue([]byte(planText))
	if perr != nil {
		return []any{}, stats
	}
	obj, ok := parsed.(*jsonenc.Object)
	if !ok {
		return []any{}, stats
	}
	dsV, _ := obj.Get("datasets")
	rawDS, ok := dsV.([]any)
	if !ok {
		return []any{}, stats
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
	return out, stats
}

// executeNamedQueriesForQueryRun mirrors app.py _vanna_execute_named_queries(use_repair=False,
// include_field_types=True) (app.py:22156) — the form api_query_run's do_chart branch uses. Each named
// query is run through _vanna_run_query (validateSQL guard + execute + SOBS_QUERY_MAX_ROWS cap); the
// item carries {name, purpose, sql, columns, rows, error, retry_count, field_types}.
func (s *server) executeNamedQueriesForQueryRun(named []any) []any {
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
		columns, rows, fieldTypes := []any{}, []any{}, []any{}
		nqErr := ""
		if vErr := validateSQL(nqSQL); vErr != "" {
			nqErr = "SQL validation error: " + vErr
		} else if res, execErr := s.db.Execute(nqSQL); execErr != nil {
			nqErr = "Query execution error: " + execErr.Error()
		} else {
			columns, rows = serializeQueryResult(res)
			if cap := envInt("SOBS_QUERY_MAX_ROWS", 1000); cap >= 0 && len(rows) > cap {
				rows = rows[:cap]
			}
			fieldTypes = inferQueryFieldTypes(columns, rows)
		}
		results = append(results, jsonenc.NewObject().
			Set("name", name).Set("purpose", purpose).Set("sql", nqSQL).
			Set("columns", columns).Set("rows", rows).Set("error", nqErr).
			Set("retry_count", 0).Set("field_types", fieldTypes))
	}
	return results
}

// generateChartSpecStats mirrors generateChartSpec (ai_build.go _vanna_generate_chart_spec) but ALSO
// returns the LLM usage stats so api_query_run's do_chart branch can populate chart_generation in
// llm_stats. The body is a faithful copy of generateChartSpec; only the captured stats differ.
func (s *server) generateChartSpecStats(endpoint, question string, columns, sampleRows, namedDatasets []any, preferredChartType, chartInstruction string) (string, string, llmStats) {
	rows := sampleRows
	if len(rows) > 20 {
		rows = rows[:20]
	}
	// app.py json.dumps(..., ensure_ascii=False) — default (spaced) separators; sampleRows is already
	// records (chartSampleRecords) built by the caller.
	sampleStr := string(jsonenc.Encode(jsonenc.NewObject().Set("columns", columns).Set("rows", rows), jsonDumpsDefault))
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
				string(jsonenc.Encode(condensed, jsonDumpsDefault))
		}
	}
	var prefLines []string
	if preferredChartType != "" {
		prefLines = append(prefLines, "Preferred chart type: "+preferredChartType)
	}
	if chartInstruction != "" {
		prefLines = append(prefLines, "Chart instruction: "+chartInstruction)
	}
	preferenceBlock := ""
	if len(prefLines) > 0 {
		preferenceBlock = "\n\nChart preferences:\n" + strings.Join(prefLines, "\n")
	}
	userMsg := "Original question: " + question + "\n\n" +
		"Result set (columns + up to 20 sample rows):\n" + sampleStr + "\n\n" +
		namedStr + preferenceBlock + "Produce an ECharts option JSON object for this data."
	chatReq := llmRequest{
		endpoint:      endpoint,
		model:         strings.TrimSpace(s.loadAISetting("ai.model", "")),
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: strings.TrimSpace(s.loadAISetting("ai.thinking_level", "off")),
		maxTokens:     queryLLMMaxTokens, // app.py _vanna_generate_chart_spec: max_tokens=_QUERY_LLM_MAX_TOKENS
		messages: []any{
			jsonenc.NewObject().Set("role", "system").Set("content", chartSystemPrompt),
			jsonenc.NewObject().Set("role", "user").Set("content", userMsg),
		},
	}
	raw, stats, err := s.callLLMChat(chatReq)
	if err != nil || strings.TrimSpace(raw) == "" {
		return "", "LLM did not return a chart spec.", stats
	}
	parsed, _ := parseChartSpecJSON(raw)
	if parsed != nil {
		if len(parsed.Keys()) == 0 {
			return "", "LLM returned an empty chart spec object.", stats
		}
		return string(jsonenc.Encode(parsed, dumpsDefault)), "", stats
	}
	parseErr := chartSpecParseError(raw)
	// app.py _repair_chart_spec_json_with_llm: ECharts-repair system prompt, raw failed reply + parse
	// error in the user message, thinking off, max_tokens _QUERY_LLM_MAX_TOKENS (inherited).
	repairReq := chatReq
	repairReq.thinkingLevel = "off"
	repairReq.messages = []any{
		jsonenc.NewObject().Set("role", "system").Set("content", chartJSONRepairSystemPrompt),
		jsonenc.NewObject().Set("role", "user").Set("content",
			"The chart JSON below failed to parse. Repair it and return only valid JSON.\n\n"+
				"Parse error: "+parseErr+"\n\n"+
				"Malformed chart JSON:\n"+raw),
	}
	repaired, _, rerr := s.callLLMChat(repairReq)
	if rerr != nil || strings.TrimSpace(repaired) == "" {
		return "", "Chart spec JSON parse error: " + parseErr + ". LLM JSON repair returned empty content.", stats
	}
	rparsed, _ := parseChartSpecJSON(repaired)
	if rparsed == nil {
		return "", "Chart spec JSON parse error: " + parseErr + ". LLM JSON repair output was still invalid: " + chartSpecParseError(repaired), stats
	}
	if len(rparsed.Keys()) == 0 {
		return "", "LLM JSON repair returned an empty chart spec object.", stats
	}
	return string(jsonenc.Encode(rparsed, dumpsDefault)), "", stats
}

// queryRunLLMStats mirrors _summarize_query_llm_stats(named_query_generation=…, chart_generation=…)
// for api_query_run: the two stages plus the totals (their sum). Used by the do_chart success path;
// the no-chart path uses zeroQueryLLMStats (both stages empty -> all zero, but with the per-stage keys
// present, matching Python passing the kwargs explicitly).
func queryRunLLMStats(namedStats, chartStats llmStats) *jsonenc.Object {
	named := queryStageStats(namedStats)
	chart := queryStageStats(chartStats)
	totals := queryStageStats(llmStats{
		prompt:     namedStats.prompt + chartStats.prompt,
		completion: namedStats.completion + chartStats.completion,
		thinking:   namedStats.thinking + chartStats.thinking,
	})
	return jsonenc.NewObject().Set("totals", totals).
		Set("named_query_generation", named).Set("chart_generation", chart)
}
