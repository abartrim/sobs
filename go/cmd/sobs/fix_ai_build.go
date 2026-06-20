package main

import (
	"regexp"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---------------------------------------------------------------------------
// AI dashboard-builder parity helpers (app.py vanna pipeline).
//
// These port the populated-data / AI-configured branches the empty golden
// corpus never exercises:
//   - _vanna_validate_and_execute_with_repair  (EXPLAIN preflight + bounded
//     auto-repair / LLM SQL-repair loop, real retry_count)
//   - _vanna_execute_named_queries(use_repair=True)  (named queries through the
//     allowlist + repair path — closing the table-allowlist BYPASS that the old
//     ai-build named-query call had)
//   - _infer_custom_mapping_from_option / _extract_chart_option_placeholders
//   - _vanna_repair_sql / _auto_repair_incomplete_cte_sql /
//     _repair_truncated_in_clause_literals / _vanna_explain_sql
//
// All of these only change behavior when AI is configured AND data exists, so
// the empty-corpus AI-off path stays byte-identical.
// ---------------------------------------------------------------------------

// vannaMaxRepairAttempts mirrors the max_attempts=3 loop bound in
// _vanna_validate_and_execute_with_repair.
const vannaMaxRepairAttempts = 3

// queryLLMMaxTokens mirrors app.py _QUERY_LLM_MAX_TOKENS.
const queryLLMMaxTokens = 8192

// vannaExplainSQL mirrors app.py _vanna_explain_sql: validate read-only/allowlist first
// (returns "SQL validation error: …"), then run EXPLAIN as a cheap parse/plan preflight.
// Returns "" on success, else the error message.
func (s *server) vannaExplainSQL(sql string) string {
	if vErr := validateSQL(sql); vErr != "" {
		return "SQL validation error: " + vErr
	}
	if _, err := s.db.Execute("EXPLAIN " + sql); err != nil {
		return err.Error()
	}
	return ""
}

// reVannaInTrailing mirrors app.py _repair_truncated_in_clause_literals' `\bIN\s*\(([^)]*)$`.
var reVannaInTrailing = regexp.MustCompile(`(?is)\bIN\s*\(([^)]*)$`)

// repairTruncatedInClauseLiterals mirrors app.py _repair_truncated_in_clause_literals: best-effort
// fix for a truncated trailing IN (...) literal list.
func repairTruncatedInClauseLiterals(sql string) string {
	text := sql
	loc := reVannaInTrailing.FindStringSubmatchIndex(text)
	if loc == nil {
		return text
	}
	// group(1) capture bounds.
	g1s, g1e := loc[2], loc[3]
	itemsRaw := text[g1s:g1e]
	if strings.TrimSpace(itemsRaw) == "" {
		return text
	}
	cleaned := []string{}
	for _, item := range strings.Split(itemsRaw, ",") {
		token := strings.TrimSpace(item)
		if token == "" {
			continue
		}
		if strings.Count(token, "'")%2 != 0 {
			break
		}
		cleaned = append(cleaned, token)
	}
	if len(cleaned) == 0 {
		return text
	}
	return text[:g1s] + strings.Join(cleaned, ",") + ")"
}

var (
	reVannaWithLeading = regexp.MustCompile(`(?is)^\s*with\b`)
	reVannaCTEAlias    = regexp.MustCompile(`(?is)^\s*with\s+([a-zA-Z_]\w*)\s+as\s*\(`)
	reVannaFinalSelect = regexp.MustCompile(`(?is)\)\s*select\b`)
)

// autoRepairIncompleteCTESQL mirrors app.py _auto_repair_incomplete_cte_sql: balance closing parens
// and append a final SELECT * FROM <cte> when missing for truncated WITH-CTE SQL.
func autoRepairIncompleteCTESQL(sql string) string {
	text := strings.TrimRight(strings.TrimSpace(sql), ";")
	if text == "" {
		return ""
	}
	if !reVannaWithLeading.MatchString(text) {
		return ""
	}
	text = repairTruncatedInClauseLiterals(text)
	if strings.Count(text, "'")%2 != 0 {
		return ""
	}
	cteMatch := reVannaCTEAlias.FindStringSubmatch(text)
	if cteMatch == nil {
		return ""
	}
	hasFinalSelect := reVannaFinalSelect.MatchString(text)
	openParens := strings.Count(text, "(")
	closeParens := strings.Count(text, ")")
	if hasFinalSelect && openParens <= closeParens {
		return ""
	}
	fixed := text
	if openParens > closeParens {
		fixed += strings.Repeat(")", openParens-closeParens)
	}
	if !reVannaFinalSelect.MatchString(fixed) {
		fixed += "\nSELECT * FROM " + cteMatch[1]
	}
	return fixed
}

// vannaRepairSQL mirrors app.py _vanna_repair_sql: ask the LLM to fix SQL after a failure. Returns
// (sql, error, stats) where error is "" on success and stats carries the repair call's LLM token
// usage (last_repair_stats in app.py). A minimal local replica of the ai_llm.go LLM-call path
// (callLLMChat) so this fix does not touch ai_llm.go.
func (s *server) vannaRepairSQL(endpoint, question, previousSQL, executionError string, attemptNumber int) (string, string, llmStats) {
	model := strings.TrimSpace(s.loadAISetting("ai.model", ""))
	if endpoint == "" || model == "" {
		return "", "AI endpoint not configured.", llmStats{}
	}
	systemPrompt := strings.Replace(querySQLSystemPrompt, "{schema}", s.getSchemaContext(), 1)
	userMessage := "Original question: " + question + "\n\n" +
		"Previous SQL (attempt " + itoa(attemptNumber) + "):\n" + previousSQL + "\n\n" +
		"Execution error:\n" + executionError + "\n\n" +
		"Rewrite the SQL so it is valid for this schema and still answers the question. " +
		"Return ONLY raw SQL."
	messages := []any{
		jsonenc.NewObject().Set("role", "system").Set("content", systemPrompt),
		jsonenc.NewObject().Set("role", "user").Set("content", userMessage),
	}
	raw, st, err := s.callLLMChat(llmRequest{
		endpoint:      endpoint,
		model:         model,
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: strings.TrimSpace(s.loadAISetting("ai.thinking_level", "off")),
		messages:      messages,
		maxTokens:     queryLLMMaxTokens,
	})
	if err != nil {
		return "", "LLM repair request failed: " + err.Error(), st
	}
	if strings.TrimSpace(raw) == "" {
		return "", "LLM did not return a repaired SQL statement.", st
	}
	sql := stripCodeFences(strings.TrimSpace(raw))
	if sql == "" {
		return "", "LLM returned an empty repaired SQL statement.", st
	}
	return sql, "", st
}

// validateAndExecuteVannaSQLWithRepair mirrors app.py _vanna_validate_and_execute_with_repair:
// EXPLAIN preflight -> _auto_repair_incomplete_cte_sql -> up to 3x _vanna_repair_sql, with the real
// retry_count and last_repair_stats. Returns (finalSQL, columns, rows, error, retryCount,
// lastRepairStats); on success error == "" and columns is non-nil.
func (s *server) validateAndExecuteVannaSQLWithRepair(endpoint, question, initialSQL string) (string, []any, []any, string, int, llmStats) {
	currentSQL := strings.TrimSpace(initialSQL)
	retryCount := 0
	lastRepairError := ""
	execError := ""
	var lastRepairStats llmStats

	explainError := s.vannaExplainSQL(currentSQL)
	if explainError != "" {
		autoRepaired := autoRepairIncompleteCTESQL(currentSQL)
		if autoRepaired != "" && autoRepaired != currentSQL {
			currentSQL = autoRepaired
			retryCount++
			explainError = s.vannaExplainSQL(currentSQL)
		}
		if explainError != "" {
			repairedSQL, repairErr, repairStats := s.vannaRepairSQL(endpoint, question, currentSQL, explainError, 0)
			lastRepairStats = repairStats
			if repairedSQL != "" && repairErr == "" {
				currentSQL = repairedSQL
				retryCount++
			} else {
				lastRepairError = repairErr
			}
		}
	}

	for attempt := 1; attempt <= vannaMaxRepairAttempts; attempt++ {
		var cols, rows []any
		cols, rows, execError = s.vannaRunQuery(currentSQL)
		if execError == "" {
			return currentSQL, cols, rows, "", retryCount, lastRepairStats
		}
		if attempt >= vannaMaxRepairAttempts {
			break
		}
		autoRepaired := autoRepairIncompleteCTESQL(currentSQL)
		if autoRepaired != "" && autoRepaired != currentSQL {
			currentSQL = autoRepaired
			retryCount++
			continue
		}
		errForRepair := execError
		if errForRepair == "" {
			errForRepair = "Unknown SQL execution error."
		}
		repairedSQL, repairErr, repairStats := s.vannaRepairSQL(endpoint, question, currentSQL, errForRepair, attempt)
		lastRepairStats = repairStats
		if repairedSQL != "" && repairErr == "" {
			currentSQL = repairedSQL
			retryCount++
			continue
		}
		lastRepairError = repairErr
		break
	}

	finalError := execError
	if finalError == "" {
		finalError = "Query execution failed"
	}
	if lastRepairError != "" {
		finalError = finalError + " | SQL repair error: " + lastRepairError
	}
	return currentSQL, nil, nil, finalError, retryCount, lastRepairStats
}

// rawChdbError recovers the underlying chdb exception text that Python's str(exc) yields, by
// stripping the Go store's "chdb query: " wrapper (internal/store/chdb.go) from err.Error(). Used
// where app.py surfaces the raw exception verbatim (e.g. _vanna_run_query's "Query execution error:
// {exc}"), as opposed to _public_dashboard_query_error which further sanitizes the message.
func rawChdbError(err error) string {
	msg := strings.TrimSpace(err.Error())
	for strings.HasPrefix(msg, "chdb query: ") {
		msg = strings.TrimPrefix(msg, "chdb query: ")
	}
	return msg
}

// vannaRunQuery mirrors app.py _vanna_run_query: validate read-only/allowlist, execute, apply the
// hard _QUERY_MAX_ROWS row cap. Returns (columns, rows, error); on success error == "" and columns
// is non-nil. On a chdb failure the error is "Query execution error: {exc}" — the RAW chdb exception
// (app.py _vanna_run_query), NOT _public_dashboard_query_error (that sanitizer is a different route's).
func (s *server) vannaRunQuery(sql string) ([]any, []any, string) {
	if vErr := validateSQL(sql); vErr != "" {
		return nil, nil, "SQL validation error: " + vErr
	}
	res, err := s.db.Execute(sql)
	if err != nil {
		return nil, nil, "Query execution error: " + rawChdbError(err)
	}
	columns, rows := serializeQueryResult(res)
	// Hard row cap (_QUERY_MAX_ROWS, default 1000) applied after execution.
	if cap := envInt("SOBS_QUERY_MAX_ROWS", 1000); cap >= 0 && len(rows) > cap {
		rows = rows[:cap]
	}
	return columns, rows, ""
}

// executeNamedQueriesValidatedAiBuild mirrors app.py _vanna_execute_named_queries(use_repair=True),
// the path ai_build_chart_spec uses. Unlike the shared executeNamedQueries (dashboard dry-run, which
// legitimately skips validation), every named query is routed through the allowlist + repair loop
// before execution — closing the table-allowlist BYPASS in ai-build. The output item shape matches
// executeNamedQueries' {name, purpose, sql, columns, rows, error} (plus retry_count, which
// _vanna_execute_named_queries also carries).
func (s *server) executeNamedQueriesValidatedAiBuild(endpoint, question string, named []any) []any {
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
		finalSQL, cols, rows, nqErr, retryCount, _ := s.validateAndExecuteVannaSQLWithRepair(endpoint, question, nqSQL)
		if cols == nil {
			cols = []any{}
		}
		if rows == nil {
			rows = []any{}
		}
		results = append(results, jsonenc.NewObject().
			Set("name", name).Set("purpose", purpose).Set("sql", finalSQL).
			Set("columns", cols).Set("rows", rows).Set("error", nqErr).
			Set("retry_count", retryCount))
	}
	return results
}

// reChartPlaceholder mirrors app.py _extract_chart_option_placeholders' `\{\{\s*([a-zA-Z0-9_:\-]+)\s*\}\}`.
var reChartPlaceholder = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_:\-]+)\s*\}\}`)

// extractChartOptionPlaceholders mirrors app.py _extract_chart_option_placeholders: the set of
// stripped {{name}} placeholders in the option JSON.
func extractChartOptionPlaceholders(optionJSON string) []string {
	if optionJSON == "" {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, m := range reChartPlaceholder.FindAllStringSubmatch(optionJSON, -1) {
		key := strings.TrimSpace(m[1])
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
	return out
}

var chartMappingReservedPrefixes = []string{"rows:", "records:", "columns:"}

// inferCustomMappingFromOption mirrors app.py _infer_custom_mapping_from_option: infer minimal
// custom_mapping_json entries for non-reserved placeholders used by the option JSON. Returns nil
// when no inference applies (caller emits "{}"). columns is the primary result column names.
func inferCustomMappingFromOption(optionJSON string, columns []any) *jsonenc.Object {
	placeholders := extractChartOptionPlaceholders(optionJSON)
	if len(placeholders) == 0 {
		return nil
	}
	reservedNames := map[string]bool{"rows": true, "records": true, "columns": true}
	inferred := jsonenc.NewObject()
	for _, placeholder := range placeholders {
		key := strings.TrimSpace(placeholder)
		if key == "" || reservedNames[key] {
			continue
		}
		skip := false
		for _, p := range chartMappingReservedPrefixes {
			if strings.HasPrefix(key, p) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		lowered := strings.ToLower(key)
		switch {
		case (lowered == "labels" || lowered == "categories" || lowered == "x" || lowered == "x_labels") && len(columns) > 0:
			inferred.Set(key, jsonenc.NewObject().Set("from", "column").Set("name", columns[0]))
		case (lowered == "values" || lowered == "y" || lowered == "y_values") && len(columns) > 1:
			inferred.Set(key, jsonenc.NewObject().Set("from", "column").Set("name", columns[1]))
		case lowered == "records_data" || lowered == "items" || lowered == "objects":
			inferred.Set(key, jsonenc.NewObject().Set("from", "records"))
		default:
			inferred.Set(key, jsonenc.NewObject().Set("from", "rows"))
		}
	}
	if inferred.Len() == 0 {
		return nil
	}
	return inferred
}
