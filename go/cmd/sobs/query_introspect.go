package main

import (
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// queryAllowedTableSet is the membership view of queryAllowedTables (declared in
// handlers_pages.go as the sorted name list, mirroring sorted(_QUERY_ALLOWED_TABLES)) for
// O(1) allowlist checks. The SOBS_QUERY_ALLOWED_TABLES env extension is empty in parity.
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
