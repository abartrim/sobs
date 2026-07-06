package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// ChdbSqlRunner – minimal Vanna-style chDB adapter
// ---------------------------------------------------------------------------

// dataFrame is the plain-Go replacement for the pandas DataFrames that the
// Python Vanna section returned. It carries an ordered column list plus the
// row data (each Row is a map keyed by column name). An empty dataFrame has
// nil/empty Columns and Rows.
type dataFrame struct {
	Columns []string
	Rows    []Row
}

// SQL statements that are safe to execute (read-only)
var safeSqlPrefixes = map[string]bool{
	"select":   true,
	"explain":  true,
	"show":     true,
	"describe": true,
	"desc":     true,
	"with":     true,
}

// Patterns that indicate write operations (blocked regardless of prefix)
var unsafeSqlPatterns = regexp.MustCompile(
	`(?i)\b(insert|update|delete|drop|truncate|alter|create|replace|rename|attach|detach|` +
		`grant|revoke|system\s+stop|system\s+start|system\s+reload|kill|optimize|exchange)\b`,
)

// ---------------------------------------------------------------------------
// Query page – table/view access allowlist
// ---------------------------------------------------------------------------

// Built-in set of table/view names that the Query page may SELECT from.
// The “system“ database is always permitted for metadata queries (SHOW,
// DESCRIBE, and SELECT from system.tables / system.columns).
// Operators can extend this set via the “SOBS_QUERY_ALLOWED_TABLES“
// environment variable (comma-separated additional names merged at startup).
var queryAllowedTablesBuiltin = map[string]bool{
	"otel_logs":                     true,
	"otel_traces":                   true,
	"hyperdx_sessions":              true,
	"otel_metrics_gauge":            true,
	"otel_metrics_gauge_pinned":     true,
	"otel_metrics_sum":              true,
	"otel_metrics_sum_pinned":       true,
	"otel_metrics_histogram":        true,
	"otel_metrics_histogram_pinned": true,
	"sobs_anomaly_rules":            true,
	"sobs_raw_windows":              true,
	"otel_metrics_1m_agg":           true,
	"v_derived_signals_1m":          true,
	"v_otel_metrics_1m":             true,
	"v_otel_metrics_signal_context": true,
	"v_otel_metrics_anomaly":        true,
	"v_otel_metrics_dedup":          true,
	"v_derived_signals_anomaly":     true,
}

// buildQueryAllowedTables returns the merged set of allowed table/view names
// for the Query page.
//
// Merges the built-in allowlist with any additional names supplied via the
// “SOBS_QUERY_ALLOWED_TABLES“ environment variable (comma-separated).
// Only names that match the safe identifier pattern “[a-zA-Z_][a-zA-Z0-9_]*“
// are accepted from the environment variable; malformed entries are silently
// skipped to prevent injection through the configuration surface.
func buildQueryAllowedTables() map[string]bool {
	// Start with a copy of the built-in set.
	merged := make(map[string]bool, len(queryAllowedTablesBuiltin))
	for k, v := range queryAllowedTablesBuiltin {
		merged[k] = v
	}
	extra := strings.TrimSpace(os.Getenv("SOBS_QUERY_ALLOWED_TABLES"))
	if extra == "" {
		return merged
	}
	safeIdent := regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	for _, n := range strings.Split(extra, ",") {
		n = strings.TrimSpace(n)
		if n != "" && safeIdent.MatchString(n) {
			merged[strings.ToLower(n)] = true
		}
	}
	return merged
}

var queryAllowedTables = buildQueryAllowedTables()

// sortedQueryAllowedTables returns the allowlist as a sorted slice (used for
// deterministic error messages and close-match suggestions).
func sortedQueryAllowedTables() []string {
	names := make([]string, 0, len(queryAllowedTables))
	for k := range queryAllowedTables {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// suggestAllowedTableNames returns close matches from the current query
// allowlist for a blocked table.
func suggestAllowedTableNames(blockedRef string, maxSuggestions int) []string {
	parts := strings.Split(strings.ToLower(blockedRef), ".")
	tableName := ""
	if len(parts) > 0 {
		tableName = parts[len(parts)-1]
	}
	if tableName == "" {
		return nil
	}
	return getCloseMatches(tableName, sortedQueryAllowedTables(), maxSuggestions, 0.45)
}

// Extracts CTE alias names.  Handles all three forms:
//
//	WITH alias AS (          – standard CTE
//	WITH RECURSIVE alias AS  – recursive CTE (ClickHouse extension)
//	, alias AS (             – additional CTE in the same WITH clause
//
// The comma variant omits the word-boundary (“\b“) because it is
// preceded by “)“ which is a non-word character.
// Identifiers are matched as “[a-zA-Z_]\w*“ (cannot start with a digit).
var sqlCteAliasRe = regexp.MustCompile(`(?i)(?:\bWITH\s+(?:RECURSIVE\s+)?|,\s*)([a-zA-Z_]\w*)\s+AS\s*\(`)

// Extracts the column/array expression that follows “ARRAY JOIN“ so it can
// be excluded from the table-reference allowlist check (ARRAY JOIN targets
// are array columns, not data-source tables).
var sqlArrayJoinRe = regexp.MustCompile(`(?i)\bARRAY\s+JOIN\s+((?:[a-zA-Z_]\w*\.)*[a-zA-Z_]\w*)`)

// Extracts table/view references that follow “FROM“ or any “JOIN“ keyword.
// Matches optional “database.“ qualifier (e.g. “default.otel_logs“).
// Does NOT match subqueries (“FROM (SELECT …)“), because “(“ is not “\w“.
// Identifiers use “[a-zA-Z_]\w*“ so numeric-leading tokens are never matched.
var sqlTableRefRe = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+((?:[a-zA-Z_]\w*\.)*[a-zA-Z_]\w*)`)

// ChdbSqlRunner is a Vanna-style chDB adapter for read-only SQL execution.
//
// This adapter:
//   - Validates SQL is read-only before execution (SELECT, EXPLAIN, SHOW, DESCRIBE, WITH).
//   - Restricts data access to the tables/views in queryAllowedTables (allowlist).
//   - Executes queries through the shared ChDbConnection so the chDB lock is respected.
//   - Returns results as dataFrame values.
//   - Provides schema introspection helpers for building LLM prompt context.
type ChdbSqlRunner struct {
	db *ChDbConnection
}

func newChdbSqlRunner(db *ChDbConnection) *ChdbSqlRunner {
	return &ChdbSqlRunner{db: db}
}

// ------------------------------------------------------------------
// SQL safety validation
// ------------------------------------------------------------------

// validateSql returns an error if sql is not a safe, read-only statement.
//
// Checks:
//  1. The first non-whitespace keyword must be in safeSqlPrefixes.
//  2. The statement must not contain write/DDL keywords.
//  3. All referenced tables/views must be in queryAllowedTables
//     (or in the “system“ database which is always permitted for metadata).
func validateSql(sql string) error {
	stripped := strings.TrimSpace(sql)
	if stripped == "" {
		return fmt.Errorf("SQL statement is empty.")
	}

	firstToken := strings.ToLower(strings.Fields(stripped)[0])
	if !safeSqlPrefixes[firstToken] {
		return fmt.Errorf(
			"Only read-only SQL is allowed (SELECT, EXPLAIN, SHOW, DESCRIBE, WITH). "+
				"Got: '%s'.",
			strings.ToUpper(firstToken),
		)
	}

	if unsafeSqlPatterns.MatchString(stripped) {
		return fmt.Errorf(
			"SQL statement contains a disallowed write or DDL keyword " +
				"(INSERT, UPDATE, DELETE, DROP, CREATE, TRUNCATE, …).",
		)
	}

	blocked := checkTableRefs(stripped)
	if blocked != "" {
		suggestions := suggestAllowedTableNames(blocked, 5)
		suggestionText := ""
		if len(suggestions) > 0 {
			suggestionText = fmt.Sprintf(" Closest allowed names: %s.", strings.Join(suggestions, ", "))
		}
		return fmt.Errorf(
			"Access to table or view '%s' is not permitted. "+
				"Only approved observability tables may be queried via the Query page. "+
				"Allowed tables: %s.%s"+
				" If this is a valid custom table/view, add it via "+
				"SOBS_QUERY_ALLOWED_TABLES.",
			blocked,
			strings.Join(sortedQueryAllowedTables(), ", "),
			suggestionText,
		)
	}
	return nil
}

// checkTableRefs returns the first disallowed table reference in sql, or empty
// string if all are OK.
//
// The check is allowlist-based:
//   - The “system“ database (e.g. “system.tables“) is always permitted.
//   - Table/view names listed in queryAllowedTables are permitted.
//   - CTE aliases (“WITH alias AS (...)“) are recognised and excluded so
//     that queries like “WITH t AS (SELECT …) SELECT * FROM t“ are not
//     incorrectly rejected.
//   - “ARRAY JOIN“ targets are array column expressions, not data-source
//     tables, so they are excluded from the check.
func checkTableRefs(sql string) string {
	// Step 1: Collect CTE alias names – they are not real tables.
	cteAliases := map[string]bool{}
	for _, m := range sqlCteAliasRe.FindAllStringSubmatch(sql, -1) {
		cteAliases[strings.ToLower(m[1])] = true
	}

	// Step 2: Collect ARRAY JOIN refs – these are column/array expressions, not tables.
	arrayJoinRefs := map[string]bool{}
	for _, m := range sqlArrayJoinRe.FindAllStringSubmatch(sql, -1) {
		arrayJoinRefs[strings.ToLower(m[1])] = true
	}

	// Step 3: Check each FROM/JOIN reference against the allowlist.
	for _, m := range sqlTableRefRe.FindAllStringSubmatch(sql, -1) {
		ref := m[1]
		refLower := strings.ToLower(ref)

		// Skip CTE aliases and ARRAY JOIN targets – not real data-source tables.
		if cteAliases[refLower] || arrayJoinRefs[refLower] {
			continue
		}

		parts := strings.Split(refLower, ".")
		dbName := "default"
		if len(parts) > 1 {
			dbName = parts[0]
		}
		tableName := parts[len(parts)-1]

		// The system database is read-only metadata – always permitted.
		if dbName == "system" {
			continue
		}

		// Only the `default` database is valid for observability tables.
		if dbName != "default" {
			return ref
		}

		// Check against the allowlist.
		if !queryAllowedTables[tableName] {
			return ref
		}
	}

	return ""
}

// ------------------------------------------------------------------
// Query execution
// ------------------------------------------------------------------

// runSql validates and executes sql, returning a dataFrame.
func (s *ChdbSqlRunner) runSql(sql string) (dataFrame, error) {
	if err := validateSql(sql); err != nil {
		return dataFrame{}, err
	}
	result, err := s.db.Execute(sql)
	if err != nil {
		return dataFrame{}, err
	}
	rows := result.Fetchall()
	if len(rows) == 0 {
		return dataFrame{}, nil
	}
	// Column order mirrors pandas: keys of the first row.
	columns := result.Cols
	if len(columns) == 0 {
		columns = rowKeys(rows[0])
	}
	return dataFrame{Columns: columns, Rows: rows}, nil
}

// ------------------------------------------------------------------
// Schema introspection
// ------------------------------------------------------------------

// getTables returns a list of table names in database.
func (s *ChdbSqlRunner) getTables(database string) []string {
	result, err := s.db.Execute("SELECT name FROM system.tables WHERE database=? ORDER BY name", database)
	if err != nil {
		return nil
	}
	rows := result.Fetchall()
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, fmt.Sprintf("%v", row[result.Cols[0]]))
	}
	return out
}

// describeTable returns column metadata for table as a dataFrame.
func (s *ChdbSqlRunner) describeTable(table, database string) dataFrame {
	cols := []string{"name", "type", "default_kind", "comment"}
	result, err := s.db.Execute(
		"SELECT name, type, default_kind, comment "+
			"FROM system.columns WHERE database=? AND table=? ORDER BY position",
		database, table,
	)
	if err != nil {
		return dataFrame{Columns: cols}
	}
	rows := result.Fetchall()
	if len(rows) == 0 {
		return dataFrame{Columns: cols}
	}
	return dataFrame{Columns: result.Cols, Rows: rows}
}

// describeTableExtended returns extended column metadata for table including
// key and nullability info.
//
// Each entry contains: name, type, is_nullable, is_primary_key,
// is_sorting_key, default_kind, comment.
func (s *ChdbSqlRunner) describeTableExtended(table string) []map[string]any {
	result, err := s.db.Execute(
		"SELECT name, type, default_kind, comment, "+
			"is_in_primary_key, is_in_sorting_key, is_in_partition_key "+
			"FROM system.columns WHERE database=? AND table=? ORDER BY position",
		"default", table,
	)
	if err != nil {
		return nil
	}
	rows := result.Fetchall()
	columns := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		typeStr := fmt.Sprintf("%v", orEmpty(r["type"]))
		columns = append(columns, map[string]any{
			"name":             fmt.Sprintf("%v", orEmpty(r["name"])),
			"type":             typeStr,
			"is_nullable":      strings.Contains(typeStr, "Nullable("),
			"is_primary_key":   truthyInt(r["is_in_primary_key"]),
			"is_sorting_key":   truthyInt(r["is_in_sorting_key"]),
			"is_partition_key": truthyInt(r["is_in_partition_key"]),
			"default_kind":     fmt.Sprintf("%v", orEmpty(r["default_kind"])),
			"comment":          fmt.Sprintf("%v", orEmpty(r["comment"])),
		})
	}
	return columns
}

// getTableDdl returns the DDL (CREATE TABLE statement) for table.
// Returns an empty string if the DDL cannot be retrieved.
func (s *ChdbSqlRunner) getTableDdl(table string) string {
	result, err := s.db.Execute(fmt.Sprintf("SHOW CREATE TABLE `%s`", table))
	if err != nil {
		return ""
	}
	rows := result.Fetchall()
	if len(rows) > 0 {
		return fmt.Sprintf("%v", rows[0][result.Cols[0]])
	}
	return ""
}

// getTableSample returns sample rows from table as
// {"columns": [...], "rows": [[...], ...]}. Only allowed tables are sampled.
// Returns empty columns/rows on error.
func (s *ChdbSqlRunner) getTableSample(table string, limit int) map[string]any {
	if !queryAllowedTables[table] {
		return map[string]any{"columns": []string{}, "rows": [][]any{}}
	}
	sql := fmt.Sprintf("SELECT * FROM `%s` LIMIT %d", table, limit)
	df, err := s.runSql(sql)
	if err != nil {
		return map[string]any{"columns": []string{}, "rows": [][]any{}}
	}
	cols := df.Columns
	rows := make([][]any, 0, len(df.Rows))
	for _, row := range df.Rows {
		vals := make([]any, 0, len(cols))
		for _, c := range cols {
			vals = append(vals, jsonSafeScalar(row[c]))
		}
		rows = append(rows, vals)
	}
	return map[string]any{"columns": cols, "rows": rows}
}

// getAllowedTablesInfo returns metadata for all allowed tables that exist in
// the default database. Each entry contains: name, column_count, columns list.
func (s *ChdbSqlRunner) getAllowedTablesInfo() []map[string]any {
	existing := map[string]bool{}
	for _, t := range s.getTables("default") {
		existing[t] = true
	}
	allowedExisting := []string{}
	for t := range queryAllowedTables {
		if existing[t] {
			allowedExisting = append(allowedExisting, t)
		}
	}
	sort.Strings(allowedExisting)
	result := make([]map[string]any, 0, len(allowedExisting))
	for _, table := range allowedExisting {
		cols := s.describeTableExtended(table)
		result = append(result, map[string]any{
			"name":         table,
			"column_count": len(cols),
			"columns":      cols,
		})
	}
	return result
}

// getTableDetail returns serialized table detail payload for a single allowed table.
func (s *ChdbSqlRunner) getTableDetail(table string) map[string]any {
	return map[string]any{
		"columns": s.describeTableExtended(table),
		"ddl":     s.getTableDdl(table),
		"sample":  s.getTableSample(table, 5),
	}
}

var reLowCardinality = regexp.MustCompile(`\bLowCardinality\((.+)\)$`)
var reNullableType = regexp.MustCompile(`\bNullable\((.+)\)$`)
var reDateTime64 = regexp.MustCompile(`\bDateTime64\(\d+\)`)

// compactClickhouseType returns a compact ClickHouse-oriented type string for prompts.
func compactClickhouseType(typeName string) string {
	compact := strings.TrimSpace(typeName)
	compact = reLowCardinality.ReplaceAllString(compact, "$1")
	compact = reNullableType.ReplaceAllString(compact, "$1?")
	compact = reDateTime64.ReplaceAllString(compact, "DateTime64")
	return compact
}

// schemaColumnTags returns concise semantic tags that help SQL generation.
func schemaColumnTags(columnName, typeName string) string {
	lowerName := strings.ToLower(columnName)
	lowerType := strings.ToLower(typeName)
	tags := []string{}

	if strings.Contains(lowerType, "date") || strings.Contains(lowerType, "time") {
		tags = append(tags, "ts")
	}
	idNames := map[string]bool{"id": true, "traceid": true, "spanid": true, "sessionid": true}
	if strings.HasSuffix(lowerName, "id") || idNames[lowerName] {
		tags = append(tags, "id")
	}
	for _, token := range []string{"count", "value", "duration", "latency", "score", "sum", "avg"} {
		if strings.Contains(lowerName, token) {
			tags = append(tags, "metric")
			break
		}
	}
	for _, token := range []string{"map", "array", "tuple", "json"} {
		if strings.Contains(lowerType, token) {
			tags = append(tags, "json")
			break
		}
	}
	if len(tags) == 0 {
		for _, token := range []string{"string", "enum", "bool"} {
			if strings.Contains(lowerType, token) {
				tags = append(tags, "dim")
				break
			}
		}
	}

	if len(tags) > 0 {
		return "[" + strings.Join(tags, ",") + "]"
	}
	return ""
}

// compactSchemaLine returns a compact one-line schema summary for a single table.
func (s *ChdbSqlRunner) compactSchemaLine(table, database string) string {
	df := s.describeTable(table, database)
	fields := []string{}
	for _, colRow := range df.Rows {
		colName := strings.TrimSpace(fmt.Sprintf("%v", orEmpty(colRow["name"])))
		if colName == "" {
			continue
		}
		compactType := compactClickhouseType(fmt.Sprintf("%v", orEmpty(colRow["type"])))
		tags := schemaColumnTags(colName, compactType)
		fields = append(fields, fmt.Sprintf("%s:%s%s", colName, compactType, tags))
	}
	return fmt.Sprintf("%s(%s)", table, strings.Join(fields, ", "))
}

func (s *ChdbSqlRunner) compactAttrKeyLine(recordType, label string, maxKeys int) string {
	keys, err := getCachedAttrKeys(s.db, recordType)
	if err != nil {
		logger.Error("getCachedAttrKeys failed", "record_type", recordType, "error", err)
		return ""
	}
	if len(keys) == 0 {
		return ""
	}
	shown := keys
	suffix := ""
	if len(keys) > maxKeys {
		shown = keys[:maxKeys]
		suffix = ", ..."
	}
	return fmt.Sprintf("%s: %s%s", label, strings.Join(shown, ", "), suffix)
}

// getSchemaContext builds a compact ClickHouse-focused schema string for LLM prompts.
//
// Only tables present in queryAllowedTables are included so the prompt stays
// aligned with runtime access control.
func (s *ChdbSqlRunner) getSchemaContext(database string, maxTables int) string {
	allTables := s.getTables(database)
	// Restrict schema context to the allowlist (defence-in-depth: the LLM
	// should not generate queries for tables it cannot see in the schema).
	tables := []string{}
	for _, t := range allTables {
		if queryAllowedTables[t] {
			tables = append(tables, t)
		}
	}
	if len(tables) > maxTables {
		tables = tables[:maxTables]
	}
	if len(tables) == 0 {
		return fmt.Sprintf("Database: %s\n(no tables found)", database)
	}

	lines := []string{fmt.Sprintf("Database: %s", database)}
	for _, table := range tables {
		lines = append(lines, s.compactSchemaLine(table, database))
	}

	lines = append(lines,
		"",
		"Signal terminology:",
		"sobs_anomaly_rules => metric/anomaly rule definitions (threshold/comparator config),",
		"not time-series values",
		"v_derived_signals_1m => derived 1-minute signal values used as rule inputs",
		"v_otel_metrics_1m => finalized 1-minute metric rollups for charts and trend queries",
		"otel_metrics_1m_agg => aggregate-state backing table for 1-minute metrics; query with ",
		"avgMerge(Value) and sumMerge(SampleCount) grouped by the dimension columns when using it directly",
		"v_derived_signals_anomaly and v_otel_metrics_anomaly => anomaly-scored signal/metric outputs",
		"",
		"Signal windows:",
		"sobs_raw_windows => raw-metric preservation windows registered around active signals "+
			"(for example errors/rules), with "+
			"WindowStart, WindowEnd, SignalType, SignalRef, ServiceName",
		"v_otel_metrics_signal_context => deduplicated raw+pinned "+
			"metric points that fall inside each signal window",
		"For deployment/release-window overlays, use sobs_raw_windows and filter "+
			"SignalType/SignalRef for deployment-like values when present.",
		"",
		"OTEL map access:",
		"otel_logs => LogAttributes['key'], ResourceAttributes['key'], ScopeAttributes['key']",
		"otel_traces => SpanAttributes['key'], ResourceAttributes['key']",
		"In this dataset, resource/scope keys are often also available in LogAttributes or SpanAttributes.",
	)

	attrLines := []string{
		s.compactAttrKeyLine("log", "Observed LogAttributes keys", 20),
		s.compactAttrKeyLine("span", "Observed SpanAttributes keys", 20),
		s.compactAttrKeyLine("resource", "Observed ResourceAttributes keys", 20),
		s.compactAttrKeyLine("scope", "Observed ScopeAttributes keys", 20),
	}
	filtered := []string{}
	for _, line := range attrLines {
		if line != "" {
			filtered = append(filtered, line)
		}
	}
	if len(filtered) > 0 {
		lines = append(lines, "Observed OTEL attribute keys:")
		lines = append(lines, filtered...)
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// Vanna Query Service – async helpers for NL → SQL → DataFrame
// ---------------------------------------------------------------------------

const querySqlSystemPrompt = `You are a ClickHouse SQL expert. Your job is to write correct, read-only ClickHouse SELECT queries based on natural-language questions.

Rules:
- Output ONLY raw SQL. No markdown, no backticks, no explanation.
- You MUST return a non-empty SQL query as your final answer.
- Use only SELECT statements (or WITH … SELECT). Never use INSERT, UPDATE,
    DELETE, DROP, CREATE, or any DDL.
- Use ONLY tables/views and columns that are present in the provided schema
    context and allowed-table list.
- Do NOT invent, guess, hallucinate, or rename tables, views, or fields.
- If the user's wording does not exactly match the schema, map it to the
    closest real table/column names from the provided schema.
- Terminology disambiguation:
  - ` + "`sobs_anomaly_rules`" + ` = metric/anomaly rule definitions (configuration rows).
    - ` + "`v_otel_metrics_1m`" + ` = finalized 1-minute metric rollups for trend/chart queries.
    - ` + "`otel_metrics_1m_agg`" + ` = aggregate-state backing table for those 1-minute metric rollups.
        If you query it directly, you MUST use ` + "`avgMerge(Value)`" + ` and ` + "`sumMerge(SampleCount)`" + ` and
        ` + "`GROUP BY ServiceName, MetricName, AttrFingerprint, MetricKind, MinuteBucket`" + ` (or a subset
        that still includes every selected non-aggregated column).
  - ` + "`v_derived_signals_1m`" + ` = derived signal time series before anomaly scoring.
  - ` + "`v_derived_signals_anomaly`" + ` and ` + "`v_otel_metrics_anomaly`" + ` = scored outputs with
      anomaly_state and anomaly_score.
  - ` + "`sobs_raw_windows`" + ` = signal windows that preserve raw metrics data around active
      signals; this is window metadata, not rule definitions.
- If asked about rule definitions, thresholds, comparators, or rule coverage,
    query ` + "`sobs_anomaly_rules`" + ` first.
- If asked about signal trends/values over time, prefer ` + "`v_derived_signals_1m`" + `
    unless anomaly state/score is explicitly requested.
- Prefer ` + "`v_otel_metrics_1m`" + ` over ` + "`otel_metrics_1m_agg`" + ` for normal charts unless the user
    explicitly wants aggregate-state internals or a query that benefits from direct ` + "`avgMerge`" + ` access.
- For signal, anomaly, alert, or incident-window questions, prefer
    ` + "`sobs_raw_windows`" + ` for window metadata and
    ` + "`v_otel_metrics_signal_context`" + ` for metrics that occurred inside those windows.
- For deployment/release correlation requests, treat deployment windows as a subset
    of signal windows in ` + "`sobs_raw_windows`" + ` (typically matched via SignalType/SignalRef
    text filters when explicit deployment tables are absent).
- For complex analytical, correlation, or chart-oriented questions with
    multiple metrics or transforms, prefer 2-4 compact, clearly named CTEs
    instead of one large SELECT.
- For simple questions, a single SELECT is preferred over unnecessary CTEs.
- When using multiple CTEs, keep each CTE focused on one step such as
    filtering, aggregation, enrichment, or final shaping.
- If you use CTEs (WITH ...), you MUST include a final SELECT statement after the CTE block.
- Ensure all parentheses and quotes are balanced before returning the SQL.
- The database name is "default". Always qualify table names as ` + "`default.<table>`" + ` or omit the database when unambiguous.
- Use ClickHouse-compatible syntax (e.g. toDate(), now(), formatDateTime(), arrayJoin(), etc.).
- ClickHouse JOIN safety: keep JOIN ON predicates equality-based whenever possible.
- For time-window overlap/non-equality correlation (e.g. t between WindowStart and WindowEnd),
    avoid non-equi predicates directly in JOIN ON. Prefer CROSS JOIN (or pre-aggregated equality keys)
    and apply the overlap predicates in WHERE.
- When the question asks for a chart or visualisation, still return only the SQL that produces the data.
- Limit results to at most 1000 rows unless the user explicitly asks for more (add LIMIT 1000 unless already present).

CTE pattern example (structure only):
WITH filtered AS (
    SELECT TimestampTime, ServiceName
    FROM default.otel_logs
    WHERE TimestampTime >= now() - INTERVAL 24 HOUR
), counts AS (
    SELECT ServiceName, count() AS error_count
    FROM filtered
    GROUP BY ServiceName
)
SELECT ServiceName, error_count
FROM counts
ORDER BY error_count DESC
LIMIT 20

Schema context:
{schema}
`

const queryChartSystemPrompt = `You are a data-visualisation expert. Given a ClickHouse SQL result set described as column names and sample rows, produce an Apache ECharts option object (JSON) that best visualises the data.

Guidelines:
- Output ONLY a valid JSON object — the value to assign to ` + "`chart.setOption(...)`" + `.
- You MUST return a non-empty final JSON object.
- Use Bootstrap 5 colours where possible (primary: #0d6efd, success: #198754, danger: #dc3545, warning: #ffc107, info: #0dcaf0).
- Choose the most appropriate chart type from the full ECharts library (bar, line, pie, scatter, heatmap, radar, funnel, gauge, candlestick, tree, treemap, sunburst, etc.).
- Titles, tooltips, legends, and axes should be concise and readable.
- Set ` + "`backgroundColor: 'transparent'`" + ` to inherit the page background.
- If the data is tabular with no obvious chart form, use a simple bar chart.
- If a preferred chart type is incompatible with available columns, choose the nearest compatible
    type and still return valid JSON.
- The JSON must be parseable by JSON.parse() with no trailing commas or comments.

Formatting and placeholder guidance:
- Prefer compact, deterministic ECharts option structures with explicit arrays/objects.
- If you use custom placeholders, only use ` + "`{{rows}}`" + `, ` + "`{{records}}`" + `, ` + "`{{columns}}`" + `, or named-dataset forms like
    ` + "`{{rows:nodes}}`" + ` / ` + "`{{rows:links}}`" + `.
- Do not emit pseudo-JSON, JavaScript functions, or template syntax beyond those placeholders.

Reference examples (for shape/style only):
Mapping JSON example:
{
    "points": {"from": "rows"},
    "labels": {"from": "column", "name": "service"},
    "values": {"from": "column", "name": "error_count"}
}

ECharts option JSON example:
{
    "backgroundColor": "transparent",
    "tooltip": {"trigger": "axis"},
    "xAxis": {"type": "category"},
    "yAxis": {"type": "value"},
    "series": [
        {
            "type": "bar",
            "data": "{{points}}"
        }
    ]
}
`

const queryChartJsonRepairSystemPrompt = `You repair malformed Apache ECharts option JSON.

Rules:
- Return ONLY a valid JSON object.
- Preserve the original visualization intent as closely as possible.
- Do not add markdown, comments, or code fences.
- Ensure the output is parseable by JSON.parse().
`

const queryLlmMaxTokens = 8192

var reFenceOpen = regexp.MustCompile("(?i)^```[a-zA-Z]*\n?")
var reFenceClose = regexp.MustCompile("(?i)\n?```$")

// normalizeChartSpecText extracts a likely JSON object from a raw chart-spec model reply.
func normalizeChartSpecText(specRaw string) string {
	spec := strings.TrimSpace(specRaw)
	if strings.HasPrefix(spec, "```") {
		spec = reFenceOpen.ReplaceAllString(spec, "")
		spec = reFenceClose.ReplaceAllString(spec, "")
	}
	spec = strings.TrimSpace(spec)

	firstObj := strings.Index(spec, "{")
	lastObj := strings.LastIndex(spec, "}")
	if firstObj >= 0 && lastObj > firstObj {
		spec = strings.TrimSpace(spec[firstObj : lastObj+1])
	}
	return spec
}

// PORT-NOTE: these best-effort JSON-comma-repair regexes are transcribed from
// the original (intentionally heavily escaped) Python patterns. The final
// missing-comma pass in Python used a zero-width lookahead, which RE2 does not
// support, so it is reimplemented here as a consuming capture group.
const jsonValueTokenPattern = `"(?:\\.|[^"\\])*"|true|false|null|-?\\d+(?:\\.\\d+)?(?:[eE][+-]?\\d+)?|\}|\]`

var jsonObjectMemberPattern = compileQuiet(`(?s)(` + jsonValueTokenPattern + `)(\\s+)(\"(?:\\\\.|[^\"\\\\])*\"\\s*:)`)
var jsonArrayItemPattern = compileQuiet(`(?s)(` + jsonValueTokenPattern + `)(\\s+)(\{|\[|\"(?:\\\\.|[^\"\\\\])*\"|true|false|null|-?\\d)`)
var reDoubleComma = regexp.MustCompile(`,\s*,+`)
var reMissingCommaBeforeKey = compileQuiet(`([}\]"0-9eE])\s*("(?:\\.|[^"\\])*"\s*:)`)

// insertMissingJsonCommas is a best-effort repair for missing commas between
// JSON values/items.
func insertMissingJsonCommas(text string) string {
	repaired := text
	if repaired == "" {
		return repaired
	}

	// Run a few stabilization passes because one insertion can unlock the next.
	for i := 0; i < 4; i++ {
		previous := repaired
		if jsonObjectMemberPattern != nil {
			repaired = jsonObjectMemberPattern.ReplaceAllString(repaired, "$1,$2$3")
		}
		if jsonArrayItemPattern != nil {
			repaired = jsonArrayItemPattern.ReplaceAllString(repaired, "$1,$2$3")
		}
		repaired = reDoubleComma.ReplaceAllString(repaired, ",")
		if reMissingCommaBeforeKey != nil {
			repaired = reMissingCommaBeforeKey.ReplaceAllString(repaired, "$1, $2")
		}
		if repaired == previous {
			break
		}
	}
	return repaired
}

var reLineComment = regexp.MustCompile(`//[^\n]*`)
var reBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
var reTrailingComma = regexp.MustCompile(`,\s*([}\]])`)

// parseChartSpecJson parses chart JSON with a lightweight local repair pass.
// Returns the parsed object (nil on failure) and an error description.
func parseChartSpecJson(specRaw string) (map[string]any, string) {
	spec := normalizeChartSpecText(specRaw)
	if spec == "" {
		return nil, "empty chart spec"
	}

	var parsed any
	if err := json.Unmarshal([]byte(spec), &parsed); err != nil {
		repaired := reLineComment.ReplaceAllString(spec, "")        // // line comments
		repaired = reBlockComment.ReplaceAllString(repaired, "")    // /* */ comments
		repaired = reTrailingComma.ReplaceAllString(repaired, "$1") // trailing commas
		repaired = insertMissingJsonCommas(repaired)
		repaired = strings.TrimSpace(repaired)
		if err2 := json.Unmarshal([]byte(repaired), &parsed); err2 != nil {
			return nil, err2.Error()
		}
	}

	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil, "top-level chart spec must be a JSON object"
	}
	return obj, ""
}

// ---------------------------------------------------------------------------
// Local helpers
// ---------------------------------------------------------------------------

// compileQuiet compiles a regexp, returning nil (instead of panicking) when the
// pattern is not valid under RE2.
func compileQuiet(pattern string) *regexp.Regexp {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return re
}

// rowKeys returns the keys of a Row. Order is non-deterministic; callers should
// prefer ChDbResult.Cols when column ordering matters.
func rowKeys(r Row) []string {
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	return keys
}

// orEmpty returns v unless it is nil, in which case it returns "".
func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// truthyInt mirrors Python's bool(row.get(...)) for the integer key-flag columns
// returned by system.columns.
func truthyInt(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int:
		return t != 0
	case int8:
		return t != 0
	case int16:
		return t != 0
	case int32:
		return t != 0
	case int64:
		return t != 0
	case uint:
		return t != 0
	case uint8:
		return t != 0
	case uint16:
		return t != 0
	case uint32:
		return t != 0
	case uint64:
		return t != 0
	case float32:
		return t != 0
	case float64:
		return t != 0
	case string:
		return t != "" && t != "0"
	default:
		return false
	}
}

// getCloseMatches is a faithful port of difflib.get_close_matches using the
// Ratcliff/Obershelp similarity ratio (SequenceMatcher.ratio). It returns up to
// n possibilities whose ratio against word is at least cutoff, ordered by
// descending similarity.
func getCloseMatches(word string, possibilities []string, n int, cutoff float64) []string {
	type scored struct {
		name  string
		ratio float64
		idx   int
	}
	results := []scored{}
	for i, p := range possibilities {
		r := sequenceRatio(word, p)
		if r >= cutoff {
			results = append(results, scored{name: p, ratio: r, idx: i})
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].ratio > results[j].ratio
	})
	if len(results) > n {
		results = results[:n]
	}
	out := make([]string, 0, len(results))
	for _, s := range results {
		out = append(out, s.name)
	}
	return out
}

// sequenceRatio computes SequenceMatcher.ratio(): 2*M/T where M is the total
// number of matched characters (via recursive longest-matching-block) and T is
// the combined length of both strings.
func sequenceRatio(a, b string) float64 {
	ra := []rune(a)
	rb := []rune(b)
	total := len(ra) + len(rb)
	if total == 0 {
		return 1.0
	}
	matches := matchingBlocksTotal(ra, rb, 0, len(ra), 0, len(rb))
	return 2.0 * float64(matches) / float64(total)
}

// matchingBlocksTotal returns the total matched length for a[alo:ahi] vs
// b[blo:bhi] using the Ratcliff/Obershelp recursive decomposition.
func matchingBlocksTotal(a, b []rune, alo, ahi, blo, bhi int) int {
	besti, bestj, bestSize := findLongestMatch(a, b, alo, ahi, blo, bhi)
	if bestSize == 0 {
		return 0
	}
	total := bestSize
	if alo < besti && blo < bestj {
		total += matchingBlocksTotal(a, b, alo, besti, blo, bestj)
	}
	if besti+bestSize < ahi && bestj+bestSize < bhi {
		total += matchingBlocksTotal(a, b, besti+bestSize, ahi, bestj+bestSize, bhi)
	}
	return total
}

// findLongestMatch finds the longest matching block of a[alo:ahi] and
// b[blo:bhi], mirroring difflib.SequenceMatcher.find_longest_match without junk
// handling.
func findLongestMatch(a, b []rune, alo, ahi, blo, bhi int) (int, int, int) {
	besti, bestj, bestSize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for j := blo; j < bhi; j++ {
			if a[i] != b[j] {
				continue
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestSize {
				besti, bestj, bestSize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}
	return besti, bestj, bestSize
}

// settingStr returns settings[key] trimmed of surrounding whitespace.
func settingStr(settings map[string]string, key string) string {
	return strings.TrimSpace(settings[key])
}

// statsError returns the trimmed "error" field of an LLM stats map.
func statsError(stats map[string]any) string {
	if stats == nil {
		return ""
	}
	if v, ok := stats["error"]; ok && v != nil {
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
	return ""
}

// stripSqlFences removes leading/trailing markdown code fences from a model reply.
func stripSqlFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = reFenceOpen.ReplaceAllString(s, "")
		s = reFenceClose.ReplaceAllString(s, "")
	}
	return strings.TrimSpace(s)
}

// repairChartSpecJsonWithLlm asks the LLM for a strict JSON repair when local
// parsing fails.
func repairChartSpecJsonWithLlm(specRaw, parseError string, settings map[string]string) (map[string]any, string, map[string]any) {
	endpointUrl := settingStr(settings, "ai.endpoint_url")
	model := settingStr(settings, "ai.model")
	apiKey := settingStr(settings, "ai.api_key")
	if endpointUrl == "" || model == "" {
		return nil, "AI endpoint not configured.", map[string]any{}
	}

	userMessage := "The chart JSON below failed to parse. Repair it and return only valid JSON.\n\n" +
		fmt.Sprintf("Parse error: %s\n\n", parseError) +
		fmt.Sprintf("Malformed chart JSON:\n%s", specRaw)
	messages := []map[string]any{
		{"role": "system", "content": queryChartJsonRepairSystemPrompt},
		{"role": "user", "content": userMessage},
	}
	repairedRaw, repairStats := callLlmEndpoint(endpointUrl, model, apiKey, messages, "off", queryLlmMaxTokens, resolveEndpointTimeoutSeconds(settings), "")
	if repairedRaw == "" {
		if detail := statsError(repairStats); detail != "" {
			return nil, fmt.Sprintf("LLM JSON repair failed: %s", detail), repairStats
		}
		return nil, "LLM JSON repair returned empty content.", repairStats
	}

	parsed, parseErr := parseChartSpecJson(repairedRaw)
	if parsed == nil {
		return nil, fmt.Sprintf("LLM JSON repair output was still invalid: %s", parseErr), repairStats
	}
	return parsed, "", repairStats
}

// vannaGenerateSql asks the configured LLM to generate SQL for question.
// Returns (sql, error, stats) where error is empty on success.
func vannaGenerateSql(question, schemaContext string, settings map[string]string, preferredChartType, chartInstruction, thinkingLevel string) (string, string, map[string]any) {
	endpointUrl := settingStr(settings, "ai.endpoint_url")
	model := settingStr(settings, "ai.model")
	apiKey := settingStr(settings, "ai.api_key")

	if endpointUrl == "" || model == "" {
		return "", "AI endpoint not configured. Visit Settings → AI Configuration.", map[string]any{}
	}

	systemPrompt := strings.Replace(querySqlSystemPrompt, "{schema}", schemaContext, 1)
	allowlistParts := []string{}
	for _, name := range sortedQueryAllowedTables() {
		allowlistParts = append(allowlistParts, "- "+name)
	}
	allowlistHint := strings.Join(allowlistParts, "\n")
	userContent := fmt.Sprintf("%s\n\nAllowed queryable tables/views (must stay within this list):\n%s", question, allowlistHint)
	chartGuidance := []string{}
	if preferredChartType != "" {
		chartGuidance = append(chartGuidance, fmt.Sprintf("Preferred chart type: %s", preferredChartType))
	}
	if chartInstruction != "" {
		chartGuidance = append(chartGuidance, fmt.Sprintf("Chart instruction: %s", chartInstruction))
	}

	if preferredChartType != "" {
		catalog := loadChartTypesCatalog()
		var chartInfo map[string]any
		if ct, ok := catalog["chartTypes"].(map[string]any); ok {
			if ci, ok := ct[preferredChartType].(map[string]any); ok {
				chartInfo = ci
			}
		}
		if chartInfo != nil {
			if ds, ok := chartInfo["dataStructure"].(map[string]any); ok {
				dsType := strings.TrimSpace(fmt.Sprintf("%v", orEmpty(ds["type"])))
				dsExample := strings.TrimSpace(fmt.Sprintf("%v", orEmpty(ds["example"])))
				if dsType != "" {
					chartGuidance = append(chartGuidance, fmt.Sprintf("Desired chart data shape: %s", dsType))
				}
				if dsExample != "" {
					chartGuidance = append(chartGuidance, fmt.Sprintf("Desired chart data example: %s", dsExample))
				}
			}
		}
	}

	if len(chartGuidance) > 0 {
		guidanceLines := []string{}
		for _, line := range chartGuidance {
			guidanceLines = append(guidanceLines, "- "+line)
		}
		userContent = userContent + "\n\nChart generation guidance (shape SQL output to fit this):\n" + strings.Join(guidanceLines, "\n")
	}

	messages := []map[string]any{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": userContent},
	}

	endpointTimeout := resolveEndpointTimeoutSeconds(settings)
	sqlRaw, stats := callLlmEndpoint(endpointUrl, model, apiKey, messages, thinkingLevel, queryLlmMaxTokens, endpointTimeout, "")
	if sqlRaw == "" {
		if detail := statsError(stats); detail != "" {
			return "", fmt.Sprintf("LLM request failed: %s", detail), stats
		}
		return "", "LLM did not return a response. Check AI settings.", stats
	}

	// Strip markdown fences if the model included them despite instructions.
	sql := stripSqlFences(sqlRaw)
	if sql == "" {
		return "", "LLM returned an empty SQL statement.", stats
	}
	return sql, "", stats
}

var reNamedDatasetName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// vannaGenerateNamedQueries asks the LLM for optional named dataset SQL queries
// for complex charts. Returns (datasets, error, stats) where datasets is a list
// of {"name": str, "sql": str, "purpose": str}.
func vannaGenerateNamedQueries(question, schemaContext, baseSql string, settings map[string]string, preferredChartType, chartInstruction, thinkingLevel string) ([]map[string]string, string, map[string]any) {
	endpointUrl := settingStr(settings, "ai.endpoint_url")
	model := settingStr(settings, "ai.model")
	apiKey := settingStr(settings, "ai.api_key")

	if endpointUrl == "" || model == "" {
		return nil, "AI endpoint not configured.", map[string]any{}
	}

	preferred := preferredChartType
	if preferred == "" {
		preferred = "auto"
	}
	instruction := chartInstruction
	systemPrompt := "You are a ClickHouse SQL planner for chart datasets. " +
		"Return ONLY valid JSON with the shape: " +
		`{"datasets":[{"name":"...","sql":"SELECT ...","purpose":"..."}]}. ` +
		"Rules: use only read-only SELECT/WITH queries; keep at most 3 datasets; " +
		"names should be short snake_case identifiers; no markdown."
	userMessage := fmt.Sprintf("Question: %s\n\n", question) +
		fmt.Sprintf("Preferred chart type: %s\n", preferred) +
		fmt.Sprintf("Chart instruction: %s\n\n", instruction) +
		fmt.Sprintf("Primary SQL:\n%s\n\n", baseSql) +
		fmt.Sprintf("Schema context:\n%s\n\n", schemaContext) +
		"If one dataset is sufficient, return an empty datasets array. " +
		"For network/flow charts (graph/sankey/chord), prefer separate nodes and links datasets."
	messages := []map[string]any{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": userMessage},
	}

	endpointTimeout := resolveEndpointTimeoutSeconds(settings)
	planRaw, stats := callLlmEndpoint(endpointUrl, model, apiKey, messages, thinkingLevel, queryLlmMaxTokens, endpointTimeout, "")
	if planRaw == "" {
		return nil, statsError(stats), stats
	}

	planText := stripSqlFences(planRaw)
	firstObj := strings.Index(planText, "{")
	lastObj := strings.LastIndex(planText, "}")
	if firstObj >= 0 && lastObj > firstObj {
		planText = strings.TrimSpace(planText[firstObj : lastObj+1])
	}

	var parsed any
	if err := json.Unmarshal([]byte(planText), &parsed); err != nil {
		return nil, "", stats
	}

	parsedMap, ok := parsed.(map[string]any)
	if !ok {
		return nil, "", stats
	}
	rawDatasets, ok := parsedMap["datasets"].([]any)
	if !ok {
		return nil, "", stats
	}

	datasets := []map[string]string{}
	baseSqlNorm := strings.TrimRight(strings.TrimSpace(baseSql), ";")
	for i, item := range rawDatasets {
		if i >= 3 {
			break
		}
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", orEmpty(itemMap["name"]))))
		sql := strings.TrimRight(strings.TrimSpace(fmt.Sprintf("%v", orEmpty(itemMap["sql"]))), ";")
		purpose := strings.TrimSpace(fmt.Sprintf("%v", orEmpty(itemMap["purpose"])))
		if name == "" || !reNamedDatasetName.MatchString(name) {
			continue
		}
		upperSql := strings.TrimLeft(strings.ToUpper(sql), " \t\n\r")
		if !(strings.HasPrefix(upperSql, "SELECT") || strings.HasPrefix(upperSql, "WITH")) {
			continue
		}
		if sql == baseSqlNorm {
			continue
		}
		datasets = append(datasets, map[string]string{"name": name, "sql": sql, "purpose": purpose})
	}

	return datasets, "", stats
}

// vannaRepairSql asks the LLM to fix SQL after an execution failure.
// Returns (sql, error, stats) where error is empty on success.
func vannaRepairSql(question, schemaContext, previousSql, executionError string, settings map[string]string, attemptNumber int, thinkingLevel string) (string, string, map[string]any) {
	endpointUrl := settingStr(settings, "ai.endpoint_url")
	model := settingStr(settings, "ai.model")
	apiKey := settingStr(settings, "ai.api_key")

	if endpointUrl == "" || model == "" {
		return "", "AI endpoint not configured.", map[string]any{}
	}

	systemPrompt := strings.Replace(querySqlSystemPrompt, "{schema}", schemaContext, 1)
	userMessage := fmt.Sprintf("Original question: %s\n\n", question) +
		fmt.Sprintf("Previous SQL (attempt %d):\n%s\n\n", attemptNumber, previousSql) +
		fmt.Sprintf("Execution error:\n%s\n\n", executionError) +
		"Rewrite the SQL so it is valid for this schema and still answers the question. " +
		"Return ONLY raw SQL."
	messages := []map[string]any{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": userMessage},
	}

	endpointTimeout := resolveEndpointTimeoutSeconds(settings)
	sqlRaw, stats := callLlmEndpoint(endpointUrl, model, apiKey, messages, thinkingLevel, queryLlmMaxTokens, endpointTimeout,
		"Return ONLY complete executable ClickHouse SQL. No reasoning, no markdown, no commentary.")
	if sqlRaw == "" {
		if detail := statsError(stats); detail != "" {
			return "", fmt.Sprintf("LLM repair request failed: %s", detail), stats
		}
		return "", "LLM did not return a repaired SQL statement.", stats
	}

	sql := stripSqlFences(sqlRaw)
	if sql == "" {
		return "", "LLM returned an empty repaired SQL statement.", stats
	}
	return sql, "", stats
}

var reInClauseTail = regexp.MustCompile(`(?is)\bIN\s*\(([^)]*)$`)

// repairTruncatedInClauseLiterals is a best-effort fix for a truncated trailing
// “IN (...)“ literal list.
//
// Example input tail:
//
//	WHERE ServiceName IN ('load-svc-0','load-svc-
//
// becomes:
//
//	WHERE ServiceName IN ('load-svc-0')
func repairTruncatedInClauseLiterals(sql string) string {
	text := sql
	loc := reInClauseTail.FindStringSubmatchIndex(text)
	if loc == nil {
		return text
	}
	// loc[2]:loc[3] is group 1.
	group1Start := loc[2]
	itemsRaw := text[loc[2]:loc[3]]
	if strings.TrimSpace(itemsRaw) == "" {
		return text
	}

	cleanedItems := []string{}
	for _, item := range strings.Split(itemsRaw, ",") {
		token := strings.TrimSpace(item)
		if token == "" {
			continue
		}
		if strings.Count(token, "'")%2 != 0 {
			break
		}
		cleanedItems = append(cleanedItems, token)
	}

	if len(cleanedItems) == 0 {
		return text
	}

	return text[:group1Start] + strings.Join(cleanedItems, ",") + ")"
}

var reWithPrefix = regexp.MustCompile(`(?i)^\s*with\b`)
var reWithCte = regexp.MustCompile(`(?i)^\s*with\s+([a-zA-Z_]\w*)\s+as\s*\(`)
var reFinalSelect = regexp.MustCompile(`(?is)\)\s*select\b`)

// autoRepairIncompleteCteSql is a best-effort local fix for truncated CTE SQL.
//
// This handles common model truncation output like:
//
//	WITH t AS (SELECT ... GROUP BY ... HAVING ...
//	WITH t AS (SELECT ... WHERE name IN ('a','b','c-
//
// by balancing closing parentheses and appending a final “SELECT * FROM t“
// when missing.
func autoRepairIncompleteCteSql(sql string) string {
	text := strings.TrimRight(strings.TrimSpace(sql), ";")
	if text == "" {
		return ""
	}

	if !reWithPrefix.MatchString(text) {
		return ""
	}

	text = repairTruncatedInClauseLiterals(text)
	if strings.Count(text, "'")%2 != 0 {
		return ""
	}

	cteMatch := reWithCte.FindStringSubmatch(text)
	if cteMatch == nil {
		return ""
	}

	hasFinalSelect := reFinalSelect.MatchString(text)
	openParens := strings.Count(text, "(")
	closeParens := strings.Count(text, ")")

	if hasFinalSelect && openParens <= closeParens {
		return ""
	}

	fixed := text
	if openParens > closeParens {
		fixed += strings.Repeat(")", openParens-closeParens)
	}

	if !reFinalSelect.MatchString(fixed) {
		fixed += fmt.Sprintf("\nSELECT * FROM %s", cteMatch[1])
	}

	return fixed
}

// jsonDumps marshals v to a compact JSON string without HTML escaping,
// mirroring Python's json.dumps(..., ensure_ascii=False). Returns "" on error.
func jsonDumps(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(buf.String(), "\n")
}

// vannaGenerateChartSpec asks the LLM to produce an ECharts option JSON for the
// result set. Returns (json_spec, error, stats) where json_spec is the raw JSON string.
func vannaGenerateChartSpec(columns []string, sampleRows []map[string]any, question string, settings map[string]string, preferredChartType, chartInstruction string, namedDatasets []map[string]any, thinkingLevel string) (string, string, map[string]any) {
	endpointUrl := settingStr(settings, "ai.endpoint_url")
	model := settingStr(settings, "ai.model")
	apiKey := settingStr(settings, "ai.api_key")

	if endpointUrl == "" || model == "" {
		return "", "AI endpoint not configured.", map[string]any{}
	}

	sampleStr := jsonDumps(map[string]any{"columns": columns, "rows": firstN(sampleRows, 20)})
	namedDatasetsStr := ""
	if len(namedDatasets) > 0 {
		condensed := []map[string]any{}
		for _, ds := range namedDatasets {
			if ds == nil {
				continue
			}
			rows, _ := ds["rows"].([]any)
			condensed = append(condensed, map[string]any{
				"name":    orEmpty(ds["name"]),
				"purpose": orEmpty(ds["purpose"]),
				"columns": dsOrEmptyList(ds["columns"]),
				"rows":    firstNAny(rows, 20),
			})
		}
		if len(condensed) > 0 {
			namedDatasetsStr = "\n\nNamed datasets (use when multi-dataset chart structures are needed):\n" +
				jsonDumps(condensed)
		}
	}
	preferenceLines := []string{}
	if preferredChartType != "" {
		preferenceLines = append(preferenceLines, fmt.Sprintf("Preferred chart type: %s", preferredChartType))
	}
	if chartInstruction != "" {
		preferenceLines = append(preferenceLines, fmt.Sprintf("Chart instruction: %s", chartInstruction))
	}
	preferenceBlock := strings.Join(preferenceLines, "\n")
	if preferenceBlock != "" {
		preferenceBlock = fmt.Sprintf("\n\nChart preferences:\n%s", preferenceBlock)
	}

	userMessage := fmt.Sprintf("Original question: %s\n\n", question) +
		fmt.Sprintf("Result set (columns + up to 20 sample rows):\n%s\n\n", sampleStr) +
		namedDatasetsStr +
		preferenceBlock +
		"Produce an ECharts option JSON object for this data."
	messages := []map[string]any{
		{"role": "system", "content": queryChartSystemPrompt},
		{"role": "user", "content": userMessage},
	}

	endpointTimeout := resolveEndpointTimeoutSeconds(settings)
	specRaw, stats := callLlmEndpoint(endpointUrl, model, apiKey, messages, thinkingLevel, queryLlmMaxTokens, endpointTimeout, "")
	if specRaw == "" {
		if detail := statsError(stats); detail != "" {
			return "", fmt.Sprintf("LLM chart request failed: %s", detail), stats
		}
		return "", "LLM did not return a chart spec.", stats
	}

	parsed, parseErr := parseChartSpecJson(specRaw)
	if parsed != nil {
		if len(parsed) == 0 {
			return "", "LLM returned an empty chart spec object.", stats
		}
		return jsonDumps(parsed), "", stats
	}

	repairedParsed, repairError, repairStats := repairChartSpecJsonWithLlm(specRaw, parseErr, settings)
	if repairedParsed == nil {
		if repairError != "" {
			return "", fmt.Sprintf("Chart spec JSON parse error: %s. %s", parseErr, repairError), stats
		}
		return "", fmt.Sprintf("Chart spec JSON parse error: %s", parseErr), stats
	}

	if len(repairedParsed) == 0 {
		return "", "LLM JSON repair returned an empty chart spec object.", stats
	}

	mergedStats := map[string]any{}
	for k, v := range stats {
		mergedStats[k] = v
	}
	mergedStats["chart_json_repair"] = 1
	if len(repairStats) > 0 {
		mergedStats["chart_json_repair_stats"] = repairStats
	}
	return jsonDumps(repairedParsed), "", mergedStats
}

// firstN returns up to n elements of a slice of maps.
func firstN(s []map[string]any, n int) []map[string]any {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// firstNAny returns up to n elements of an []any slice (nil-safe).
func firstNAny(s []any, n int) []any {
	if s == nil {
		return []any{}
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}

// dsOrEmptyList returns v if it is a list, otherwise an empty list (mirrors
// ds.get("columns", [])).
func dsOrEmptyList(v any) any {
	if v == nil {
		return []any{}
	}
	return v
}

var reChartPlaceholder = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_:\-]+)\s*\}\}`)

// extractChartOptionPlaceholders finds custom placeholders used in an ECharts
// option JSON string.
func extractChartOptionPlaceholders(optionJson string) map[string]bool {
	out := map[string]bool{}
	if optionJson == "" {
		return out
	}
	for _, m := range reChartPlaceholder.FindAllStringSubmatch(optionJson, -1) {
		name := strings.TrimSpace(m[1])
		if name != "" {
			out[name] = true
		}
	}
	return out
}

// inferCustomMappingFromOption infers minimal custom_mapping_json entries for
// placeholders used by option JSON.
func inferCustomMappingFromOption(optionJson string, columns []string) map[string]any {
	placeholders := extractChartOptionPlaceholders(optionJson)
	if len(placeholders) == 0 {
		return map[string]any{}
	}

	reservedPrefixes := []string{"rows:", "records:", "columns:"}
	reservedNames := map[string]bool{"rows": true, "records": true, "columns": true}
	inferred := map[string]any{}
	for placeholder := range placeholders {
		key := strings.TrimSpace(placeholder)
		if key == "" || reservedNames[key] || hasAnyPrefix(key, reservedPrefixes) {
			continue
		}

		lowered := strings.ToLower(key)
		switch {
		case (lowered == "labels" || lowered == "categories" || lowered == "x" || lowered == "x_labels") && len(columns) > 0:
			inferred[key] = map[string]any{"from": "column", "name": columns[0]}
		case (lowered == "values" || lowered == "y" || lowered == "y_values") && len(columns) > 1:
			inferred[key] = map[string]any{"from": "column", "name": columns[1]}
		case lowered == "records_data" || lowered == "items" || lowered == "objects":
			inferred[key] = map[string]any{"from": "records"}
		default:
			inferred[key] = map[string]any{"from": "rows"}
		}
	}

	return inferred
}

// hasAnyPrefix reports whether s starts with any of the given prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// buildFallbackCustomOptionJson returns a safe fallback custom ECharts option
// JSON for AI builder.
func buildFallbackCustomOptionJson() string {
	fallbackOption := map[string]any{
		"backgroundColor": "transparent",
		"tooltip":         map[string]any{"trigger": "axis"},
		"xAxis":           map[string]any{"type": "category"},
		"yAxis":           map[string]any{"type": "value"},
		"series": []any{
			map[string]any{
				"name":       "Value",
				"type":       "line",
				"data":       "{{points}}",
				"showSymbol": false,
				"smooth":     true,
			},
		},
	}
	return jsonDumps(fallbackOption)
}

// loadChartTypesCatalog loads the ECharts chart types catalog from the JSON
// file. Returns the full catalog or empty map if file not found.
//
// PORT-NOTE: Python resolved the path relative to __file__ (the app source
// directory). Go has no __file__; we probe a couple of plausible locations
// relative to the working directory instead.
func loadChartTypesCatalog() map[string]any {
	candidates := []string{
		filepath.Join("static", "echarts-chart-types.json"),
		filepath.Join("..", "static", "echarts-chart-types.json"),
	}
	for _, catalogPath := range candidates {
		data, err := os.ReadFile(catalogPath)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) == nil {
			return m
		}
	}
	return map[string]any{}
}

// buildChartRefinementPrompt builds the chart refinement system prompt with the
// dynamic chart type catalog. Includes comprehensive chart type descriptions and
// data requirements.
//
// PORT-NOTE: Python iterated catalog["chartTypes"] in JSON insertion order. Go
// maps are unordered, so chart type entries are emitted in sorted-key order for
// determinism; only the ordering of catalog lines differs from Python.
func buildChartRefinementPrompt() string {
	catalog := loadChartTypesCatalog()
	chartCatalogSection := ""

	if chartTypes, ok := catalog["chartTypes"].(map[string]any); ok && len(chartTypes) > 0 {
		chartCatalogSection = "\nAvailable Chart Types and Data Requirements:\n"
		keys := make([]string, 0, len(chartTypes))
		for k := range chartTypes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, chartType := range keys {
			info, _ := chartTypes[chartType].(map[string]any)
			name := chartType
			if v, ok := info["name"]; ok && v != nil {
				name = fmt.Sprintf("%v", v)
			}
			ds, _ := info["dataStructure"].(map[string]any)
			chartCatalogSection += fmt.Sprintf("\n**%s** (%s)\n", name, chartType)
			chartCatalogSection += fmt.Sprintf("  Description: %v\n", orEmpty(info["description"]))
			chartCatalogSection += fmt.Sprintf("  Data Structure: %v\n", orEmpty(ds["type"]))
			chartCatalogSection += fmt.Sprintf("  Example: %v\n", orEmpty(ds["example"]))
			chartCatalogSection += fmt.Sprintf("  Best For: %v\n", orEmpty(info["goodFor"]))
		}
	}

	basePrompt := "You are an expert in Apache ECharts data visualization. " +
		"The user will ask you to modify or refine an existing chart spec based on the available data.\n\n" +
		"Your primary task: Fulfill the user's request, even if it requires changing the chart type.\n" +
		chartCatalogSection + "\n" +
		"Data-Aware Chart Transformation:\n" +
		"1. If the user requests a chart type different from current, intelligently restructure the data:\n" +
		"   - For pie/gauge: Select top values or aggregate by category\n" +
		"   - For scatter: Use first two numeric columns as x,y\n" +
		"   - For heatmap: Pivot or aggregate data into matrix form\n" +
		"   - For radar: Use all numeric columns as dimensions\n" +
		"   - For hierarchical (tree, treemap, sunburst): Organize data with parent-child structure\n" +
		"2. Always maintain data accuracy during transformation\n" +
		"3. The data object contains 'columns' (field names) and 'rows' (actual data)\n\n" +
		"Guidelines:\n" +
		"- Update chart.type to the requested chart type\n" +
		"- Restructure series.data if needed for the new chart type\n" +
		"- Change xAxis, yAxis, or other coordinate systems based on new chart type\n" +
		"- Update colors, gridlines, legends, tooltips, animations per user request\n" +
		"- Use Bootstrap 5 colors (primary: #0d6efd, success: #198754, danger: #dc3545, etc.) unless specified\n" +
		"- Set backgroundColor: 'transparent'\n" +
		"- Return ONLY valid JSON—no markdown, no explanations\n" +
		"- The result must be parseable by JSON.parse()\n"

	return basePrompt
}

var queryChartRefinementSystemPrompt = buildChartRefinementPrompt()

// vannaRefineChartSpec asks the LLM to refine an existing ECharts spec based on
// user instruction. Returns (json_spec, error, stats) where json_spec is the
// refined JSON string.
func vannaRefineChartSpec(currentSpec string, columns []string, sampleRows []map[string]any, userInstruction string, settings map[string]string, thinkingLevel string) (string, string, map[string]any) {
	endpointUrl := settingStr(settings, "ai.endpoint_url")
	model := settingStr(settings, "ai.model")
	apiKey := settingStr(settings, "ai.api_key")

	if endpointUrl == "" || model == "" {
		return "", "AI endpoint not configured.", map[string]any{}
	}

	// Validate current spec is valid JSON
	var tmp any
	if err := json.Unmarshal([]byte(currentSpec), &tmp); err != nil {
		return "", fmt.Sprintf("Current chart spec is invalid JSON: %s", err.Error()), map[string]any{}
	}

	sampleStr := jsonDumps(map[string]any{"columns": columns, "rows": firstN(sampleRows, 20)})
	userMessage := fmt.Sprintf("Current ECharts spec structure:\n%s\n\n", currentSpec) +
		fmt.Sprintf("Data available (columns + up to 20 sample rows):\n%s\n\n", sampleStr) +
		fmt.Sprintf("User instruction: %s\n\n", userInstruction) +
		"Please refine the chart spec to fulfill this request. Return only the updated JSON."
	messages := []map[string]any{
		{"role": "system", "content": queryChartRefinementSystemPrompt},
		{"role": "user", "content": userMessage},
	}

	endpointTimeout := resolveEndpointTimeoutSeconds(settings)
	specRaw, stats := callLlmEndpoint(endpointUrl, model, apiKey, messages, thinkingLevel, queryLlmMaxTokens, endpointTimeout, "")
	if specRaw == "" {
		if detail := statsError(stats); detail != "" {
			return "", fmt.Sprintf("LLM chart refinement failed: %s", detail), stats
		}
		return "", "LLM did not return a refined chart spec.", stats
	}

	parsed, parseErr := parseChartSpecJson(specRaw)
	if parsed != nil {
		return jsonDumps(parsed), "", stats
	}

	repairedParsed, repairError, repairStats := repairChartSpecJsonWithLlm(specRaw, parseErr, settings)
	if repairedParsed == nil {
		if repairError != "" {
			return "", fmt.Sprintf("Refined chart spec JSON parse error: %s. %s", parseErr, repairError), stats
		}
		return "", fmt.Sprintf("Refined chart spec JSON parse error: %s", parseErr), stats
	}

	mergedStats := map[string]any{}
	for k, v := range stats {
		mergedStats[k] = v
	}
	mergedStats["chart_json_repair"] = 1
	if len(repairStats) > 0 {
		mergedStats["chart_json_repair_stats"] = repairStats
	}
	return jsonDumps(repairedParsed), "", mergedStats
}

// queryMaxRows is the hard row cap applied to query results
// (SOBS_QUERY_MAX_ROWS, default 1000).
var queryMaxRows = func() int {
	if v := strings.TrimSpace(os.Getenv("SOBS_QUERY_MAX_ROWS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 1000
}()

// inferQueryFieldTypes infers display-friendly field type metadata from a query
// dataFrame.
//
// PORT-NOTE: Python derived the type from the pandas column dtype, falling back
// to sampling the first non-null cell. Without pandas dtypes here, the kind and
// a representative dtype string are derived purely from the first non-null cell.
func inferQueryFieldTypes(df dataFrame) []map[string]string {
	fieldTypes := []map[string]string{}
	for _, col := range df.Columns {
		var sample any
		for _, row := range df.Rows {
			if v := row[col]; v != nil {
				sample = v
				break
			}
		}
		dtypeName, kind := inferDtypeKind(sample)
		fieldTypes = append(fieldTypes, map[string]string{"name": col, "dtype": dtypeName, "kind": kind})
	}
	return fieldTypes
}

// inferDtypeKind returns a representative pandas-like dtype name and a
// display-friendly kind for a sample cell value.
func inferDtypeKind(sample any) (string, string) {
	switch sample.(type) {
	case nil:
		return "object", "string"
	case time.Time:
		return "datetime64[ns]", "datetime"
	case bool:
		return "bool", "boolean"
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "int64", "integer"
	case float32, float64:
		return "float64", "number"
	case map[string]any, []any:
		return "object", "json"
	default:
		return "object", "string"
	}
}

// jsonSafeScalar converts non-finite float values to nil for strict JSON responses.
func jsonSafeScalar(value any) any {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
	case float32:
		f := float64(v)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil
		}
	}
	return value
}

// jsonSafeRows normalizes a 2D row matrix to JSON-safe scalars.
func jsonSafeRows(rows [][]any) [][]any {
	out := make([][]any, 0, len(rows))
	for _, row := range rows {
		newRow := make([]any, 0, len(row))
		for _, cell := range row {
			newRow = append(newRow, jsonSafeScalar(cell))
		}
		out = append(out, newRow)
	}
	return out
}

// vannaExplainSql runs EXPLAIN on sql to validate syntax/planning without
// touching data. Returns an empty string on success, or the error message on
// failure.
//
// This is a cheap pre-flight check: chDB parses and plans the query without
// scanning any data, so it catches typos, unknown columns/tables, and invalid
// function calls before a real execution attempt.
func vannaExplainSql(db *ChDbConnection, sql string) string {
	// Validate read-only first (reuse existing guard).
	if err := validateSql(sql); err != nil {
		return fmt.Sprintf("SQL validation error: %s", err.Error())
	}

	// Execute EXPLAIN directly on the connection — skip the DataFrame
	// conversion in run_sql because EXPLAIN rows are plain tuples, not dicts.
	result, err := db.Execute(fmt.Sprintf("EXPLAIN %s", sql))
	if err != nil {
		return err.Error()
	}
	result.Fetchall()
	return ""
}

// vannaRunQuery synchronously validates and executes sql using a ChdbSqlRunner.
//
// Applies a hard row cap (queryMaxRows, default 1000) by truncating the
// resulting dataFrame to prevent memory exhaustion regardless of what the LLM
// generated.
//
// Returns (dataframe, error) – on success error is empty, on failure dataframe
// is nil.
func vannaRunQuery(db *ChDbConnection, sql string) (*dataFrame, string) {
	runner := newChdbSqlRunner(db)
	// Distinguish validation errors from execution errors to mirror the Python
	// ValueError vs generic Exception handling.
	if err := validateSql(sql); err != nil {
		return nil, fmt.Sprintf("SQL validation error: %s", err.Error())
	}
	df, err := runner.runSql(sql)
	if err != nil {
		return nil, fmt.Sprintf("Query execution error: %s", err.Error())
	}
	// Hard row cap applied after execution to avoid memory issues.
	if len(df.Rows) > queryMaxRows {
		df.Rows = df.Rows[:queryMaxRows]
	}
	return &df, ""
}

// dfValues mirrors pandas DataFrame.values.tolist(): a row matrix ordered by
// the frame's columns.
func dfValues(df *dataFrame) [][]any {
	if df == nil {
		return [][]any{}
	}
	matrix := make([][]any, 0, len(df.Rows))
	for _, row := range df.Rows {
		cells := make([]any, 0, len(df.Columns))
		for _, col := range df.Columns {
			cells = append(cells, row[col])
		}
		matrix = append(matrix, cells)
	}
	return matrix
}

// vannaValidateAndExecuteWithRepair validates/executes SQL with an EXPLAIN
// preflight plus bounded AI repair retries. Mirrors
// _vanna_validate_and_execute_with_repair: returns
// (finalSql, dataframe, error, retryCount, lastRepairStats).
func vannaValidateAndExecuteWithRepair(
	db *ChDbConnection,
	question, schemaContext, initialSql string,
	settings map[string]string,
	thinkingLevel string,
) (string, *dataFrame, string, int, map[string]any) {
	const maxAttempts = 3
	currentSql := strings.TrimSpace(initialSql)
	retryCount := 0
	lastRepairError := ""
	lastRepairStats := map[string]any{}
	execError := ""

	explainError := vannaExplainSql(db, currentSql)
	if explainError != "" {
		autoRepaired := autoRepairIncompleteCteSql(currentSql)
		if autoRepaired != "" && autoRepaired != currentSql {
			currentSql = autoRepaired
			retryCount++
			explainError = vannaExplainSql(db, currentSql)
		}
		if explainError != "" {
			repairedSql, repairError, repairStats := vannaRepairSql(
				question, schemaContext, currentSql, explainError, settings, 0, thinkingLevel,
			)
			lastRepairStats = repairStats
			if repairedSql != "" && repairError == "" {
				currentSql = repairedSql
				retryCount++
			} else {
				lastRepairError = repairError
			}
		}
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		df, runErr := vannaRunQuery(db, currentSql)
		execError = runErr
		if df != nil && execError == "" {
			return currentSql, df, "", retryCount, lastRepairStats
		}
		if attempt >= maxAttempts {
			break
		}
		autoRepaired := autoRepairIncompleteCteSql(currentSql)
		if autoRepaired != "" && autoRepaired != currentSql {
			currentSql = autoRepaired
			retryCount++
			continue
		}
		repairInput := execError
		if repairInput == "" {
			repairInput = "Unknown SQL execution error."
		}
		repairedSql, repairError, repairStats := vannaRepairSql(
			question, schemaContext, currentSql, repairInput, settings, attempt, thinkingLevel,
		)
		lastRepairStats = repairStats
		if repairedSql != "" && repairError == "" {
			currentSql = repairedSql
			retryCount++
			continue
		}
		lastRepairError = repairError
		break
	}

	finalError := execError
	if finalError == "" {
		finalError = "Query execution failed"
	}
	if lastRepairError != "" {
		finalError = fmt.Sprintf("%s | SQL repair error: %s", finalError, lastRepairError)
	}
	return currentSql, nil, finalError, retryCount, lastRepairStats
}

// vannaExecuteNamedQueries executes optional named dataset queries and returns
// normalized per-dataset results. Mirrors _vanna_execute_named_queries.
func vannaExecuteNamedQueries(
	db *ChDbConnection,
	namedQueries []map[string]string,
	question, schemaContext string,
	settings map[string]string,
	thinkingLevel string,
	includeFieldTypes, useRepair bool,
) []map[string]any {
	results := []map[string]any{}
	for _, nq := range namedQueries {
		nqSql := strings.TrimSpace(nq["sql"])
		nqName := strings.TrimSpace(nq["name"])
		nqPurpose := nq["purpose"]
		if nqSql == "" || nqName == "" {
			continue
		}

		nqFinalSql := nqSql
		nqError := ""
		nqRetryCount := 0
		var nqDf *dataFrame

		if useRepair {
			nqFinalSql, nqDf, nqError, nqRetryCount, _ = vannaValidateAndExecuteWithRepair(
				db, question, schemaContext, nqSql, settings, thinkingLevel,
			)
		} else {
			nqDf, nqError = vannaRunQuery(db, nqSql)
		}

		nqColumns := []string{}
		nqRows := [][]any{}
		if nqDf != nil {
			nqColumns = nqDf.Columns
			if len(nqDf.Rows) > 0 {
				nqRows = jsonSafeRows(dfValues(nqDf))
			}
		}
		item := map[string]any{
			"name":        nqName,
			"purpose":     nqPurpose,
			"sql":         nqFinalSql,
			"columns":     nqColumns,
			"rows":        nqRows,
			"error":       nqError,
			"retry_count": nqRetryCount,
		}
		if includeFieldTypes {
			if nqDf != nil && len(nqDf.Rows) > 0 {
				item["field_types"] = inferQueryFieldTypes(*nqDf)
			} else {
				item["field_types"] = []map[string]string{}
			}
		}
		results = append(results, item)
	}
	return results
}
