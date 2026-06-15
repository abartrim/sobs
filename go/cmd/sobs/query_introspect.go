package main

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// safeQueryTableIdent mirrors app.py's identifier guard for env-supplied table names.
var safeQueryTableIdent = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// queryAllowedTables mirrors sorted(_QUERY_ALLOWED_TABLES) — the builtin allowlist merged with the
// comma-separated SOBS_QUERY_ALLOWED_TABLES env extension (each entry trimmed, safe-ident-checked on
// its original case, then lowercased), de-duplicated and re-sorted. Ports _build_query_allowed_tables
// (app.py). The env is empty under parity, so this equals queryAllowedTablesBuiltin there.
var queryAllowedTables = buildQueryAllowedTables()

func buildQueryAllowedTables() []any {
	set := map[string]bool{}
	names := make([]string, 0, len(queryAllowedTablesBuiltin))
	add := func(n string) {
		if n == "" || set[n] {
			return
		}
		set[n] = true
		names = append(names, n)
	}
	for _, t := range queryAllowedTablesBuiltin {
		if s, ok := t.(string); ok {
			add(s)
		}
	}
	if extra := strings.TrimSpace(os.Getenv("SOBS_QUERY_ALLOWED_TABLES")); extra != "" {
		for _, part := range strings.Split(extra, ",") {
			p := strings.TrimSpace(part)
			if p != "" && safeQueryTableIdent.MatchString(p) {
				add(strings.ToLower(p))
			}
		}
	}
	sort.Strings(names)
	out := make([]any, len(names))
	for i, n := range names {
		out[i] = n
	}
	return out
}

// queryAllowedTableSet is the membership view of queryAllowedTables for O(1) allowlist checks.
var queryAllowedTableSet = func() map[string]bool {
	m := make(map[string]bool, len(queryAllowedTables))
	for _, t := range queryAllowedTables {
		if s, ok := t.(string); ok {
			m[s] = true
		}
	}
	return m
}()

// queryTableNames mirrors ChdbSqlRunner.get_tables: the table names in *database*, ordered.
func (s *server) queryTableNames(database string) ([]string, error) {
	res, err := s.db.Execute("SELECT name FROM system.tables WHERE database=? ORDER BY name", database)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(res.Rows))
	for _, m := range rowMaps(res) {
		names = append(names, cStr(m, "name"))
	}
	return names, nil
}

// describeTableExtended mirrors ChdbSqlRunner.describe_table_extended: per-column metadata
// (name, type, nullability, key membership, default_kind, comment) from system.columns.
func (s *server) describeTableExtended(table string) ([]any, error) {
	res, err := s.db.Execute(
		"SELECT name, type, default_kind, comment, "+
			"is_in_primary_key, is_in_sorting_key, is_in_partition_key "+
			"FROM system.columns WHERE database=? AND table=? ORDER BY position",
		"default", table)
	if err != nil {
		return nil, err
	}
	cols := []any{}
	for _, m := range rowMaps(res) {
		typeStr := cStr(m, "type")
		cols = append(cols, jsonenc.NewObject().
			Set("name", cStr(m, "name")).
			Set("type", typeStr).
			Set("is_nullable", strings.Contains(typeStr, "Nullable(")).
			Set("is_primary_key", cBool(m, "is_in_primary_key")).
			Set("is_sorting_key", cBool(m, "is_in_sorting_key")).
			Set("is_partition_key", cBool(m, "is_in_partition_key")).
			Set("default_kind", cStr(m, "default_kind")).
			Set("comment", cStr(m, "comment")))
	}
	return cols, nil
}

// allowedTablesInfo mirrors ChdbSqlRunner.get_allowed_tables_info: for every allowlisted table
// that exists, its {name, column_count, columns}. Iteration order = sorted(allowlist ∩ existing).
func (s *server) allowedTablesInfo() ([]any, error) {
	existing, err := s.queryTableNames("default")
	if err != nil {
		return nil, err
	}
	existingSet := make(map[string]bool, len(existing))
	for _, t := range existing {
		existingSet[t] = true
	}
	// queryAllowedTables is already sorted, so filtering it by existence yields
	// sorted(allowlist ∩ existing) directly (matching Python's generator + sorted()).
	allowed := []string{}
	for _, tAny := range queryAllowedTables {
		t, _ := tAny.(string)
		if existingSet[t] {
			allowed = append(allowed, t)
		}
	}
	out := []any{}
	for _, table := range allowed {
		cols, err := s.describeTableExtended(table)
		if err != nil {
			return nil, err
		}
		out = append(out, jsonenc.NewObject().
			Set("name", table).
			Set("column_count", len(cols)).
			Set("columns", cols))
	}
	return out, nil
}

// ---- schema-context (LLM prompt) introspection: ChdbSqlRunner.get_schema_context ----

var (
	reLowCardinality = regexp.MustCompile(`\bLowCardinality\((.+)\)$`)
	reNullableType   = regexp.MustCompile(`\bNullable\((.+)\)$`)
	reDateTime64Prec = regexp.MustCompile(`\bDateTime64\(\d+\)`)
)

// compactClickhouseType mirrors ChdbSqlRunner._compact_clickhouse_type: strip LowCardinality,
// render Nullable(T) as T?, and drop the DateTime64 precision. Subs are applied in order.
func compactClickhouseType(typeName string) string {
	compact := strings.TrimSpace(typeName)
	compact = reLowCardinality.ReplaceAllString(compact, "$1")
	compact = reNullableType.ReplaceAllString(compact, "$1?")
	compact = reDateTime64Prec.ReplaceAllString(compact, "DateTime64")
	return compact
}

// schemaColumnTags mirrors ChdbSqlRunner._schema_column_tags: concise semantic tags from the
// column name and its (already-compacted) type.
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
	if containsAny(lowerName, "count", "value", "duration", "latency", "score", "sum", "avg") {
		tags = append(tags, "metric")
	}
	if containsAny(lowerType, "map", "array", "tuple", "json") {
		tags = append(tags, "json")
	}
	if len(tags) == 0 && containsAny(lowerType, "string", "enum", "bool") {
		tags = append(tags, "dim")
	}
	if len(tags) == 0 {
		return ""
	}
	return "[" + strings.Join(tags, ",") + "]"
}

func containsAny(s string, tokens ...string) bool {
	for _, t := range tokens {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

// compactSchemaLine mirrors ChdbSqlRunner._compact_schema_line: a one-line summary
// `table(col:type[tags], ...)`.
func (s *server) compactSchemaLine(table string) string {
	res, err := s.db.Execute(
		"SELECT name, type, default_kind, comment FROM system.columns "+
			"WHERE database=? AND table=? ORDER BY position", "default", table)
	if err != nil {
		return table + "(describe_error:" + err.Error() + ")"
	}
	fields := []string{}
	for _, m := range rowMaps(res) {
		colName := strings.TrimSpace(cStr(m, "name"))
		if colName == "" {
			continue
		}
		compactType := compactClickhouseType(cStr(m, "type"))
		fields = append(fields, colName+":"+compactType+schemaColumnTags(colName, compactType))
	}
	return table + "(" + strings.Join(fields, ", ") + ")"
}

// schemaContextStaticBlock is the verbatim signal-terminology / window / OTEL-map suffix from
// ChdbSqlRunner.get_schema_context (adjacent Python string literals are pre-joined here; note
// the trailing space on the otel_metrics_1m_agg line).
var schemaContextStaticBlock = []string{
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
	"sobs_raw_windows => raw-metric preservation windows registered around active signals " +
		"(for example errors/rules), with " +
		"WindowStart, WindowEnd, SignalType, SignalRef, ServiceName",
	"v_otel_metrics_signal_context => deduplicated raw+pinned " +
		"metric points that fall inside each signal window",
	"For deployment/release-window overlays, use sobs_raw_windows and filter " +
		"SignalType/SignalRef for deployment-like values when present.",
	"",
	"OTEL map access:",
	"otel_logs => LogAttributes['key'], ResourceAttributes['key'], ScopeAttributes['key']",
	"otel_traces => SpanAttributes['key'], ResourceAttributes['key']",
	"In this dataset, resource/scope keys are often also available in LogAttributes or SpanAttributes.",
}

// getSchemaContext mirrors ChdbSqlRunner.get_schema_context: a compact schema string for LLM
// prompts. The observed-attr-key lines require otel data (none on the parity fixture, so they
// are empty there — matching Python, which appends nothing when the caches are empty).
func (s *server) getSchemaContext() string {
	const database = "default"
	allTables, _ := s.queryTableNames(database)
	tables := []string{}
	for _, t := range allTables {
		if queryAllowedTableSet[t] {
			tables = append(tables, t)
			if len(tables) >= 30 { // max_tables
				break
			}
		}
	}
	if len(tables) == 0 {
		return "Database: " + database + "\n(no tables found)"
	}
	lines := []string{"Database: " + database}
	for _, t := range tables {
		lines = append(lines, s.compactSchemaLine(t))
	}
	lines = append(lines, schemaContextStaticBlock...)
	return strings.Join(lines, "\n")
}

// getTableDDL mirrors ChdbSqlRunner.get_table_ddl: the CREATE statement via SHOW CREATE TABLE;
// "" on any error.
func (s *server) getTableDDL(table string) string {
	res, err := s.db.Execute("SHOW CREATE TABLE `" + table + "`")
	if err != nil || len(res.Rows) == 0 || len(res.Columns) == 0 {
		return ""
	}
	return cStr(rowMaps(res)[0], res.Columns[0])
}

// getTableSample mirrors ChdbSqlRunner.get_table_sample: SELECT * LIMIT n as {columns, rows}
// (list-of-lists). Non-allowlisted / error / empty -> empty columns+rows. serializeQueryResult
// already yields empty slices for a 0-row result, matching run_sql's empty-DataFrame shaping.
func (s *server) getTableSample(table string, limit int) *jsonenc.Object {
	empty := jsonenc.NewObject().Set("columns", []any{}).Set("rows", []any{})
	if !queryAllowedTableSet[table] {
		return empty
	}
	res, err := s.db.Execute("SELECT * FROM `" + table + "` LIMIT " + strconv.Itoa(limit))
	if err != nil {
		return empty
	}
	columns, rows := serializeQueryResult(res)
	return jsonenc.NewObject().Set("columns", columns).Set("rows", rows)
}
