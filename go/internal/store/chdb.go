package store

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	chdb "github.com/chdb-io/chdb-go/chdb"
)

// chdbStore is the production DB backed by chdb-go (purego/dlopen of libchdb at runtime;
// no cgo). It mirrors app.py's ChDbConnection: one persistent on-disk session opened on
// the shared sobs.chdb directory, queried with ClickHouse SQL verbatim.
//
// libchdb is located at runtime via CHDB_LIB_PATH (see go/CHDB_PIN.md). Build does NOT
// need the native library; only a running query does.
type chdbStore struct {
	sess *chdb.Session
}

// Open opens (or creates) the chdb session at dataDir/sobs.chdb — the SAME directory the
// Python app uses, enabling the shared-storage hard cutover.
func Open(dataDir string) (DB, error) {
	path := filepath.Join(dataDir, "sobs.chdb")
	target, err := chdbConnectTarget(path)
	if err != nil {
		return nil, err
	}
	sess, err := chdb.NewSession(target)
	if err != nil {
		return nil, fmt.Errorf("chdb open %s: %w", path, err)
	}
	applySessionSettings(sess)
	return &chdbStore{sess: sess}, nil
}

// applySessionSettings mirrors app.py ChDbConnection.__init__: cap query parallelism (max_threads,
// default 1 for the embedded single-process server) and enable spill-to-disk for large GROUP BY /
// ORDER BY so big queries don't OOM the container. These bound resource use and never change query
// results (the corpus uses stable ORDER BY), so parity is unaffected. Best-effort: if a SET fails,
// chdb keeps its defaults (the prior behavior) rather than failing startup.
func applySessionSettings(sess *chdb.Session) {
	maxThreads := envIntStore("SOBS_CHDB_MAX_THREADS", 1)
	spillGroupBy := envIntStore("SOBS_CHDB_SPILL_GROUP_BY_MB", 32) * 1024 * 1024
	spillSort := envIntStore("SOBS_CHDB_SPILL_SORT_MB", 32) * 1024 * 1024
	for _, stmt := range []string{
		fmt.Sprintf("SET max_threads = %d", maxThreads),
		fmt.Sprintf("SET max_bytes_before_external_group_by = %d", spillGroupBy),
		fmt.Sprintf("SET max_bytes_before_external_sort = %d", spillSort),
	} {
		_, _ = sess.Query(stmt, "")
	}
}

// chdbConnectTarget mirrors the config-file branch of app.py _build_chdb_connect_target: when
// SOBS_CLICKHOUSE_CONFIG_FILE points at a mounted ClickHouse config.xml (the encrypted-disk
// setup), pass it to chdb as a startup arg through the connection-string query params (chdb-go's
// connStr accepts the `path?key=value` form). Unset — the default, including the parity harness —
// yields the plain session path, byte-for-byte unchanged.
func chdbConnectTarget(path string) (string, error) {
	configFile := strings.TrimSpace(os.Getenv("SOBS_CLICKHOUSE_CONFIG_FILE"))
	if configFile != "" {
		if !filepath.IsAbs(configFile) {
			return "", fmt.Errorf("SOBS_CLICKHOUSE_CONFIG_FILE must be an absolute path to a mounted ClickHouse config.xml")
		}
		return path + "?config-file=" + quotePathSafeSlash(configFile), nil
	}

	// Low-memory defaults for embedded single-process chdb (app.py _build_chdb_connect_target's
	// else-branch); override via env for larger deployments. These are resource caps / threadpool
	// sizes — they bound RSS, never change query results, so the parity corpus is unaffected. The
	// Python oracle captures the corpus under these same defaults.
	maxServerMB := envIntStore("SOBS_CHDB_MAX_SERVER_MB", 768)
	markCacheMB := envIntStore("SOBS_CHDB_MARK_CACHE_MB", 64)
	uncompressedCacheMB := envIntStore("SOBS_CHDB_UNCOMPRESSED_CACHE_MB", 64)
	params := strings.Join([]string{
		"max_server_memory_usage=" + strconv.Itoa(maxServerMB*1024*1024),
		"mark_cache_size=" + strconv.Itoa(markCacheMB*1024*1024),
		"uncompressed_cache_size=" + strconv.Itoa(uncompressedCacheMB*1024*1024),
		"background_pool_size=2",
		"background_schedule_pool_size=16",
		"background_io_pool_size=2",
	}, "&")
	return path + "?" + params, nil
}

// envIntStore parses a bare integer env var, mirroring Python's int(os.environ.get(name, default))
// for the CHDB_* tuning vars: unset falls back to def, but a SET-but-malformed value is fatal at
// startup (Python raises ValueError at import and the process never boots).
func envIntStore(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("%s: invalid literal for int() with base 10: %q", name, v)
		}
		return n
	}
	return def
}

// quotePathSafeSlash mirrors Python urllib.parse.quote(value, safe="/"): keep unreserved chars
// (A-Za-z0-9 and -._~) plus '/', percent-encode the rest with uppercase hex.
func quotePathSafeSlash(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '/' ||
			(c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' || c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// Close persists and closes the session. NOTE: Close, never Cleanup — Cleanup does
// os.RemoveAll on the data directory (see go/CHDB_PIN.md).
func (s *chdbStore) Close() error {
	s.sess.Close()
	return nil
}

// chJSON is ClickHouse's FORMAT JSON envelope.
type chJSON struct {
	Meta []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"meta"`
	Data []map[string]any `json:"data"`
}

// Execute runs a query and returns columns + rows. Parameters are substituted by the
// caller for now (chdb-go has no server-side bind); query construction stays identical to
// the Python side, which also builds SQL with ? placeholders filled positionally.
func (s *chdbStore) Execute(query string, params ...any) (*Result, error) {
	sql, err := inlineParams(query, params)
	if err != nil {
		return nil, err
	}
	res, err := s.sess.Query(sql, "JSON")
	if err != nil {
		return nil, fmt.Errorf("chdb query: %w", err)
	}
	if res != nil {
		if e := res.Error(); e != nil {
			return nil, fmt.Errorf("chdb query: %w", e)
		}
	}
	out := res.String()
	if out == "" {
		return &Result{}, nil
	}
	var env chJSON
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return nil, fmt.Errorf("chdb decode JSON: %w (got %.200q)", err, out)
	}
	cols := make([]string, len(env.Meta))
	types := make([]string, len(env.Meta))
	for i, m := range env.Meta {
		cols[i] = m.Name
		types[i] = m.Type
	}
	rows := make([][]any, len(env.Data))
	for i, d := range env.Data {
		row := make([]any, len(cols))
		for j, c := range cols {
			row[j] = d[c]
		}
		rows[i] = row
	}
	return &Result{Columns: cols, Types: types, Rows: rows}, nil
}

// InsertJSONEachRow inserts rows via ClickHouse JSONEachRow, mirroring app.py
// _insert_rows_json_each_row: one JSON object per line after "INSERT INTO t FORMAT
// JSONEachRow". HTML escaping is disabled so stored strings keep <, >, & verbatim
// (Go's json HTML-escapes by default). Key order is irrelevant — JSONEachRow maps by
// column name. Callers must pre-normalize DateTime columns to ClickHouse strings, exactly
// as the Python helper does before insert.
// writableTables mirrors app.py _WRITABLE_TABLES: the only tables the app inserts into. Writing
// any other table is rejected (defense-in-depth, matching the Python helper's ValueError).
var writableTables = map[string]bool{
	"otel_logs": true, "otel_traces": true, "otel_metrics_gauge": true,
	"otel_metrics_sum": true, "otel_metrics_histogram": true, "otel_metrics_gauge_pinned": true,
	"otel_metrics_sum_pinned": true, "otel_metrics_histogram_pinned": true, "hyperdx_sessions": true,
	"sobs_ai_memories": true, "sobs_ai_settings": true, "sobs_agent_rules": true,
	"sobs_agent_runs": true, "sobs_anomaly_rules": true, "sobs_app_releases": true,
	"sobs_app_settings": true, "sobs_apps": true, "sobs_chart_configs": true,
	"sobs_cve_dispositions": true, "sobs_cve_findings": true, "sobs_dashboards": true,
	"sobs_github_work_items": true, "sobs_log_attr_keys": true, "sobs_notification_channels": true,
	"sobs_notification_log": true, "sobs_notification_rules": true, "sobs_raw_window_copy_state": true,
	"sobs_raw_windows": true, "sobs_record_tags": true, "sobs_release_artifacts": true,
	"sobs_reports": true, "sobs_tag_rules": true,
}

func (s *chdbStore) InsertJSONEachRow(table string, rows []map[string]any) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	if !writableTables[table] {
		return 0, fmt.Errorf("attempt to write to unregistered table %q", table)
	}
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" FORMAT JSONEachRow\n")
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil { // Encode appends '\n' per row
			return 0, fmt.Errorf("encode row for %s: %w", table, err)
		}
	}
	res, err := s.sess.Query(b.String(), "")
	if err != nil {
		return 0, fmt.Errorf("chdb insert into %s: %w", table, err)
	}
	if res != nil {
		if e := res.Error(); e != nil {
			return 0, fmt.Errorf("chdb insert into %s: %w", table, e)
		}
	}
	return len(rows), nil
}
