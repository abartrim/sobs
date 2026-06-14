// Package store is the Go counterpart to app.py's ChDbConnection (app.py:1876): a single
// persistent chdb session, serialized with a mutex, reused for the process lifetime.
//
// The SQL itself is reused VERBATIM from the Python app — chdb-go runs the same
// ClickHouse engine, so queries (FINAL, ReplacingMergeTree, JSONExtract, arrayMap,
// JSONEachRow inserts, …) port unchanged. The only Go-specific work is the connection
// wrapper, typed result scanning, and the async write-queue/batch worker
// (app.py:_queue_write / _insert_rows_json_each_row).
//
// The concrete chdb-go binding is wired in Phase 0 (see ../../CHDB_PIN.md) behind the
// `chdb` build tag so the rest of the module builds without the native library during
// early development.
package store

// Result mirrors ChDbResult: columns + materialized rows. Handlers gather data into
// ordered structures (NOT map[string]any) so JSON/template key order is controlled —
// see migration/JINJA_TO_GO_SPEC.md §3 and PARITY_STRATEGY.md §4.
type Result struct {
	Columns []string
	// Types holds the ClickHouse column type for each column (FORMAT JSON meta), parallel to
	// Columns. Used by query-result serializers that must distinguish Int/Float/String to match
	// the Python chdb driver + jsonify. Empty when no rows/meta.
	Types []string
	Rows  [][]any
}

// DB is the interface handlers depend on, so the chdb-go implementation (build-tagged)
// and a test fake can be swapped without touching handlers.
type DB interface {
	Execute(query string, params ...any) (*Result, error)
	InsertJSONEachRow(table string, rows []map[string]any) (int, error)
	Close() error
}
