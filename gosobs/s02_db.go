package main

// Port of app.py lines 1876-2324 (+ _shutdown_db_resources, lines 7484-7511):
// chDB connection wrapper, query result compat types, DB init/schema helpers,
// write-queue plumbing types, work-items/errors/summary/AI-filter caches, and
// the log attribute key caches.
//
// PORT-NOTE: the chdb-go v1.11.0 Session API (NewSession(path),
// Query(sql, "JSONEachRow") returning a result with Buf()/Error()/Free(),
// Close()) is used here; the module cache was not readable from this sandbox,
// so signatures come from the published v1.11.0 sources — verify at reconcile.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chdb-io/chdb-go/chdb"
)

// ---------------------------------------------------------------------------
// chDB connect target / startup validation
// ---------------------------------------------------------------------------

// quoteUrlPath mirrors urllib.parse.quote(value, safe="/").
func quoteUrlPath(value string) string {
	var sb strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' || c == '~' || c == '/' {
			sb.WriteByte(c)
		} else {
			sb.WriteString(fmt.Sprintf("%%%02X", c))
		}
	}
	return sb.String()
}

// buildChdbConnectTarget builds the chDB connect target, optionally adding
// startup args via query params.
func buildChdbConnectTarget(path string) (string, error) {
	configFile := strings.TrimSpace(os.Getenv(chdbConfigFileEnv))
	if configFile != "" {
		if !filepath.IsAbs(configFile) {
			return "", fmt.Errorf("%s must be an absolute path to a mounted ClickHouse config.xml", chdbConfigFileEnv)
		}
		encoded := quoteUrlPath(configFile)
		return fmt.Sprintf("%s?config-file=%s", path, encoded), nil
	}

	// Apply low-memory defaults; override via env vars for larger deployments.
	// Important: use the plain directory path with query params, not a file: URL.
	// For directory-backed chDB stores, file:/... opens a different logical DB
	// than the plain path on this runtime.
	maxServerMb := envInt(chdbMaxServerMbEnv, 768)
	markCacheMb := envInt(chdbMarkCacheMbEnv, 64)
	// ClickHouse defaults uncompressed_cache_size to max(128MB, RAM*1%), which
	// exhausts a 160MB cap before any query runs. Default to 4MB for embedded use.
	uncompressedCacheMb := envInt(chdbUncompressedCacheMbEnv, 64)
	// Reduce background thread-pool sizes for an embedded single-process
	// deployment; defaults (16 / 128 / 16) inflate RSS at init time.
	// PORT-NOTE: params built in Python dict insertion order (urlencode), not
	// the alphabetical order url.Values.Encode would produce.
	params := fmt.Sprintf(
		"max_server_memory_usage=%d&mark_cache_size=%d&uncompressed_cache_size=%d"+
			"&background_pool_size=%d&background_schedule_pool_size=%d&background_io_pool_size=%d",
		maxServerMb*1024*1024,
		markCacheMb*1024*1024,
		uncompressedCacheMb*1024*1024,
		2,
		16,
		2,
	)
	return fmt.Sprintf("%s?%s", path, params), nil
}

func validateChdbStartupConfiguration(conn *ChDbConnection) error {
	expectedDisk := strings.TrimSpace(os.Getenv(chdbExpectDiskEnv))
	expectedPolicy := strings.TrimSpace(os.Getenv(chdbExpectPolicyEnv))
	if expectedDisk == "" && expectedPolicy == "" {
		return nil
	}

	disksRes, err := conn.Execute("SELECT name FROM system.disks")
	if err != nil {
		return err
	}
	policiesRes, err := conn.Execute("SELECT DISTINCT policy_name FROM system.storage_policies")
	if err != nil {
		return err
	}

	diskNames := map[string]bool{}
	for _, row := range disksRes.Fetchall() {
		diskNames[fmt.Sprintf("%v", row[disksRes.Cols[0]])] = true
	}
	policyNames := map[string]bool{}
	for _, row := range policiesRes.Fetchall() {
		policyNames[fmt.Sprintf("%v", row[policiesRes.Cols[0]])] = true
	}

	var missing []string
	if expectedDisk != "" && !diskNames[expectedDisk] {
		missing = append(missing, fmt.Sprintf("disk '%s'", expectedDisk))
	}
	if expectedPolicy != "" && !policyNames[expectedPolicy] {
		missing = append(missing, fmt.Sprintf("storage policy '%s'", expectedPolicy))
	}
	if len(missing) > 0 {
		sortedDisks := sortedStringSet(diskNames)
		sortedPolicies := sortedStringSet(policyNames)
		return fmt.Errorf(
			"chDB started but expected storage configuration was not applied; "+
				"missing %s. "+
				"This usually means the config-file startup argument was ignored or invalid. "+
				"Current disks=%v policies=%v",
			strings.Join(missing, ", "), sortedDisks, sortedPolicies,
		)
	}
	return nil
}

func sortedStringSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// Row / ChDbResult / ChDbConnection (pinned Core API)
// ---------------------------------------------------------------------------

// Row replaces RowCompat (dict + integer-index access). Index access at
// Python call sites (row[0]) translates to row[result.Cols[0]].
type Row = map[string]any

// ChDbResult is the pre-materialised query result; data is fetched while the
// connection lock is held (mirrors the Python ChDbResult cursor compat).
type ChDbResult struct {
	Cols []string
	Rows []Row
	idx  int
}

// Fetchone mirrors ChDbResult.fetchone; returns nil when exhausted.
func (r *ChDbResult) Fetchone() Row {
	if r.idx >= len(r.Rows) {
		return nil
	}
	row := r.Rows[r.idx]
	r.idx++
	return row
}

// Fetchall mirrors ChDbResult.fetchall; returns the remaining rows.
func (r *ChDbResult) Fetchall() []Row {
	rows := r.Rows[r.idx:]
	r.idx = len(r.Rows)
	return rows
}

// ChDbConnection is the thread-safe global chDB connection wrapper.
type ChDbConnection struct {
	session *chdb.Session
	lock    sync.Mutex
	closed  bool
}

// newChDbConnection mirrors ChDbConnection.__init__.
func newChDbConnection(path string) (*ChDbConnection, error) {
	connectTarget, err := buildChdbConnectTarget(path)
	if err != nil {
		return nil, err
	}
	logger.Info(fmt.Sprintf("chDB connect target: %s", connectTarget))
	session, err := chdb.NewSession(connectTarget)
	if err != nil {
		return nil, err
	}
	c := &ChDbConnection{session: session}

	// Apply session-level memory settings for low-memory embedded operation.
	// max_threads reduces per-query parallelism; the spill settings allow
	// GROUP BY / ORDER BY to overflow to disk rather than OOM the container.
	maxThreads := envInt(chdbMaxThreadsEnv, 1)
	spillGbMb := envInt(chdbSpillGroupByMbEnv, 32)
	spillSortMb := envInt(chdbSpillSortMbEnv, 32)
	for _, stmt := range []string{
		fmt.Sprintf("SET max_threads = %d", maxThreads),
		fmt.Sprintf("SET max_bytes_before_external_group_by = %d", spillGbMb*1024*1024),
		fmt.Sprintf("SET max_bytes_before_external_sort = %d", spillSortMb*1024*1024),
	} {
		res, qerr := session.Query(stmt, "JSONEachRow")
		if qerr == nil && res != nil {
			qerr = res.Error()
			res.Free()
		}
		if qerr != nil {
			logger.Warn("chDB: failed to apply session memory settings", "error", qerr)
			break
		}
	}

	if err := validateChdbStartupConfiguration(c); err != nil {
		c.session.Close()
		c.closed = true
		return nil, err
	}
	return c, nil
}

// Execute mirrors ChDbConnection.execute: substitutes %s / %(name)s (and ?)
// placeholders with SQL-quoted values, runs the query under the connection
// lock with one retry on transient errors, and materialises the result.
func (c *ChDbConnection) Execute(query string, params ...any) (*ChDbResult, error) {
	queryName := classifyChdbQueryName(query)
	finalQuery := query
	if len(params) > 0 {
		var err error
		finalQuery, err = interpolateSqlParams(query, params)
		if err != nil {
			return nil, err
		}
	}
	if queryName != "" {
		endSpan := telemetrySpan(
			"sobs.storage.query",
			map[string]any{"storage.engine": "chdb", "query.name": queryName},
		)
		defer endSpan()
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		res, err := c.runQueryLocked(finalQuery)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if attempt == 0 {
			logger.Warn("chDB: transient query error (will retry)", "error", err)
			time.Sleep(50 * time.Millisecond)
		}
	}
	return nil, lastErr
}

func (c *ChDbConnection) runQueryLocked(query string) (*ChDbResult, error) {
	c.lock.Lock()
	defer c.lock.Unlock()
	res, err := c.session.Query(query, "JSONEachRow")
	if err != nil {
		return nil, err
	}
	if res == nil {
		return &ChDbResult{}, nil
	}
	defer res.Free()
	if rerr := res.Error(); rerr != nil {
		return nil, rerr
	}
	cols, rows, err := parseJsonEachRow(res.Buf())
	if err != nil {
		return nil, err
	}
	return &ChDbResult{Cols: cols, Rows: rows}, nil
}

// Executescript mirrors ChDbConnection.executescript: naive split on ';'
// (exactly like the Python `script.split(";")`), strip, skip empties, and
// execute each statement sequentially while holding the lock once.
func (c *ChDbConnection) Executescript(script string) error {
	var statements []string
	for _, s := range strings.Split(script, ";") {
		if t := strings.TrimSpace(s); t != "" {
			statements = append(statements, t)
		}
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	for _, stmt := range statements {
		res, err := c.session.Query(stmt, "JSONEachRow")
		if err != nil {
			return err
		}
		if res != nil {
			rerr := res.Error()
			res.Free()
			if rerr != nil {
				return rerr
			}
		}
	}
	return nil
}

// Commit is a no-op: ClickHouse auto-commits.
func (c *ChDbConnection) Commit() {}

// Close mirrors ChDbConnection.close.
func (c *ChDbConnection) Close() {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.closed {
		return
	}
	c.session.Close()
	c.closed = true
}

// parseJsonEachRow parses chDB JSONEachRow output into ordered columns
// (taken from the first row's key order) and Row maps.
// PORT-NOTE: numbers decode as json.Number (UseNumber) to avoid float64
// precision loss on UInt64 nanosecond timestamps; ClickHouse additionally
// quotes (U)Int64 as strings in JSON formats by default
// (output_format_json_quote_64bit_integers=1) — callers coerce as needed.
func parseJsonEachRow(buf []byte) ([]string, []Row, error) {
	var cols []string
	var rows []Row
	for _, line := range bytes.Split(buf, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if cols == nil {
			keys, err := orderedJsonObjectKeys(line)
			if err != nil {
				return nil, nil, err
			}
			cols = keys
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		row := Row{}
		if err := dec.Decode(&row); err != nil {
			return nil, nil, err
		}
		rows = append(rows, row)
	}
	return cols, rows, nil
}

// orderedJsonObjectKeys returns the top-level keys of a JSON object in
// document order (Go maps would lose the column order needed for Cols).
func orderedJsonObjectKeys(line []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("JSONEachRow line is not an object")
	}
	var keys []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := keyTok.(string)
		keys = append(keys, key)
		if err := skipJsonValue(dec); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func skipJsonValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok || (d != '{' && d != '[') {
		return nil
	}
	for dec.More() {
		if d == '{' {
			if _, err := dec.Token(); err != nil { // key
				return err
			}
		}
		if err := skipJsonValue(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token() // closing delim
	return err
}

// ---------------------------------------------------------------------------
// SQL parameter quoting / placeholder substitution
// ---------------------------------------------------------------------------

var sqlStringQuoteReplacer = strings.NewReplacer(
	`\`, `\\`,
	`'`, `\'`,
	"\x00", `\0`,
	"\n", `\n`,
	"\r", `\r`,
	"\t", `\t`,
)

// quoteSqlValue renders a Go value as a safely quoted ClickHouse SQL literal
// (mirrors the chdb DBAPI escape used by cursor.execute(query, params)).
func quoteSqlValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "NULL"
	case bool:
		if v {
			return "1"
		}
		return "0"
	case int:
		return strconv.Itoa(v)
	case int8:
		return strconv.FormatInt(int64(v), 10)
	case int16:
		return strconv.FormatInt(int64(v), 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint:
		return strconv.FormatUint(uint64(v), 10)
	case uint8:
		return strconv.FormatUint(uint64(v), 10)
	case uint16:
		return strconv.FormatUint(uint64(v), 10)
	case uint32:
		return strconv.FormatUint(uint64(v), 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case json.Number:
		return v.String()
	case time.Time:
		return "'" + v.UTC().Format("2006-01-02 15:04:05") + "'"
	case []byte:
		return "'" + sqlStringQuoteReplacer.Replace(string(v)) + "'"
	case string:
		return "'" + sqlStringQuoteReplacer.Replace(v) + "'"
	default:
		return "'" + sqlStringQuoteReplacer.Replace(fmt.Sprintf("%v", v)) + "'"
	}
}

// interpolateSqlParams substitutes placeholders into the query text.
// Positional: %s and ? (the Python chdb DBAPI accepts both at our call
// sites); named: %(name)s with a single map[string]any param; %% → %.
// PORT-NOTE: like the Python driver, placeholders inside string literals are
// not special-cased.
func interpolateSqlParams(query string, params []any) (string, error) {
	if len(params) == 1 {
		if named, ok := params[0].(map[string]any); ok {
			return interpolateNamedSqlParams(query, named)
		}
		if list, ok := params[0].([]any); ok {
			params = list
		}
	}
	var sb strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c == '%' && i+1 < len(query) {
			switch query[i+1] {
			case '%':
				sb.WriteByte('%')
				i++
				continue
			case 's':
				if n >= len(params) {
					return "", fmt.Errorf("not enough parameters for SQL query")
				}
				sb.WriteString(quoteSqlValue(params[n]))
				n++
				i++
				continue
			}
		}
		if c == '?' {
			if n >= len(params) {
				return "", fmt.Errorf("not enough parameters for SQL query")
			}
			sb.WriteString(quoteSqlValue(params[n]))
			n++
			continue
		}
		sb.WriteByte(c)
	}
	return sb.String(), nil
}

func interpolateNamedSqlParams(query string, named map[string]any) (string, error) {
	var sb strings.Builder
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c == '%' && i+1 < len(query) {
			switch query[i+1] {
			case '%':
				sb.WriteByte('%')
				i++
				continue
			case '(':
				end := strings.Index(query[i+2:], ")s")
				if end < 0 {
					return "", fmt.Errorf("unterminated named SQL placeholder")
				}
				name := query[i+2 : i+2+end]
				value, ok := named[name]
				if !ok {
					return "", fmt.Errorf("missing named SQL parameter: %s", name)
				}
				sb.WriteString(quoteSqlValue(value))
				i += 2 + end + 1 // skip "%(name)s"
				continue
			}
		}
		sb.WriteByte(c)
	}
	return sb.String(), nil
}

// ---------------------------------------------------------------------------
// Query classification
// ---------------------------------------------------------------------------

func classifyChdbQueryName(query string) string {
	rawQuery := strings.TrimLeft(query, " \t\n\r\v\f")
	if rawQuery == "" {
		return ""
	}
	queryName := strings.ToUpper(strings.Fields(rawQuery)[0])
	switch queryName {
	case "SELECT", "WITH", "SHOW", "DESCRIBE", "EXPLAIN":
		return queryName
	}
	return ""
}

// ---------------------------------------------------------------------------
// Module-level DB / cache state (app.py lines 2053-2083)
// ---------------------------------------------------------------------------

var (
	globalDb    *ChDbConnection
	dbInitLock  sync.Mutex
	schemaReady bool

	// Write queue: queue.Queue → buffered channel; the writer thread → a
	// goroutine. writeThread is the Thread.is_alive/join analogue: non-nil
	// while a worker has been started, closed by the worker goroutine on exit.
	writeQueue      chan *writeTask
	writeThread     chan struct{}
	writeWorkerLock sync.Mutex

	logAttrKeysLock        sync.Mutex
	logAttrKeysCacheLoaded bool
)

var attrKeyRecordTypes = []string{"log", "span", "resource", "scope"}

var logAttrKeysByRecordType = func() map[string]map[string]bool {
	m := make(map[string]map[string]bool, len(attrKeyRecordTypes))
	for _, recordType := range attrKeyRecordTypes {
		m[recordType] = map[string]bool{}
	}
	return m
}()

// PORT-NOTE: the Python work-items page cache key is a 8-tuple; the Go port
// keys by a deterministic string built by the work-items section.
var (
	workItemsCacheLock   sync.Mutex
	workItemsPageCache   = map[string]map[string]any{}
	workItemsFilterCache = map[string]any{"expires_at": 0.0, "services": []any{}, "rules": []any{}}

	errorsCacheLock     sync.Mutex
	errorsServicesCache = map[string]any{"expires_at": 0.0, "services": []any{}}

	summaryStatsCacheLock sync.Mutex
	summaryStatsCache     = map[string]any{"expires_at": 0.0, "data": map[string]any{}}

	aiFilterMetadataCacheLock sync.Mutex
	aiFilterMetadataCache     = map[[2]string]map[string]any{}
)

var (
	writeQueueMax               = envInt("SOBS_WRITE_QUEUE_MAX", 5000)
	writeBatchMax               = envInt("SOBS_WRITE_BATCH_MAX", 200)
	writeBatchWaitMs            = envInt("SOBS_WRITE_BATCH_WAIT_MS", 20)
	logAttrKeysMax              = envInt("SOBS_LOG_ATTR_KEYS_MAX", 20000)
	workItemsPageCacheTtlSec    = envInt("SOBS_WORK_ITEMS_PAGE_CACHE_TTL_SEC", 10)
	workItemsFilterCacheTtlSec  = envInt("SOBS_WORK_ITEMS_FILTER_CACHE_TTL_SEC", 30)
	errorsServicesCacheTtlSec   = envInt("SOBS_ERRORS_SERVICES_CACHE_TTL_SEC", 30)
	summaryStatsCacheTtlSec     = envInt("SOBS_SUMMARY_STATS_CACHE_TTL_SEC", 60)
	rumSessionDetailEventCap    = envInt("SOBS_RUM_SESSION_DETAIL_EVENT_CAP", 200)
	aiFilterMetadataCacheTtlSec = envInt("SOBS_AI_FILTER_METADATA_CACHE_TTL_SEC", 20)
	aiFilterMetadataSampleRows  = envInt("SOBS_AI_FILTER_METADATA_SAMPLE_ROWS", 10000)
)

// writeTask mirrors the _WriteTask dataclass.
type writeTask struct {
	op   func(*ChDbConnection) error
	done chan struct{} // threading.Event; closed by the writer when finished
	err  error         // set by the writer before closing done
}

// writeStop is the _WRITE_STOP sentinel.
var writeStop = &writeTask{}

func invalidateWorkItemsCache() {
	workItemsCacheLock.Lock()
	defer workItemsCacheLock.Unlock()
	clear(workItemsPageCache)
	workItemsFilterCache["expires_at"] = 0.0
	workItemsFilterCache["services"] = []any{}
	workItemsFilterCache["rules"] = []any{}
}

// WriteQueueFullError is raised when ingest cannot enqueue a write within
// timeout (Python: class WriteQueueFullError(RuntimeError)).
type WriteQueueFullError struct {
	Message string
}

func (e *WriteQueueFullError) Error() string { return e.Message }

// _json_error (app.py line 2108) is jsonError in s00_core.go — not redefined.

// ---------------------------------------------------------------------------
// getDb / initDb / ensureDbSchema
// ---------------------------------------------------------------------------

// getDb mirrors get_db. PORT-NOTE: the pinned Core API has no error return;
// connection/schema failures panic (Python raised; callers that swallowed
// exceptions use recover, e.g. getMaskingSettingsFlags in s01).
func getDb() *ChDbConnection {
	dbInitLock.Lock()
	defer dbInitLock.Unlock()
	if globalDb == nil {
		db, err := newChDbConnection(dbPath)
		if err != nil {
			panic(err)
		}
		globalDb = db
	}
	if !schemaReady {
		if err := globalDb.Executescript(schemaSql); err != nil {
			panic(err)
		}
		if err := ensurePostSchemaState(globalDb); err != nil {
			panic(err)
		}
		schemaReady = true
	}
	return globalDb
}

// initDb (re-)initialises the global DB connection and applies the schema.
func initDb() error {
	dbInitLock.Lock()
	defer dbInitLock.Unlock()
	db, err := newChDbConnection(dbPath)
	if err != nil {
		return err
	}
	globalDb = db
	if err := globalDb.Executescript(schemaSql); err != nil {
		return err
	}
	if err := ensurePostSchemaState(globalDb); err != nil {
		return err
	}
	schemaReady = true
	return nil
}

// ensureDbSchema creates the schema if tables are missing (fallback for
// fresh DB directories).
func ensureDbSchema() error {
	dbInitLock.Lock()
	defer dbInitLock.Unlock()
	if schemaReady {
		return nil
	}
	if globalDb == nil {
		db, err := newChDbConnection(dbPath)
		if err != nil {
			return err
		}
		globalDb = db
	}
	var hasLogs Row
	if res, err := globalDb.Execute(
		"SELECT 1 FROM system.tables WHERE database='default' AND name='otel_logs'",
	); err == nil {
		hasLogs = res.Fetchone()
	} // except Exception: has_logs = None
	if hasLogs == nil {
		if err := globalDb.Executescript(schemaSql); err != nil {
			return err
		}
	}
	if err := ensurePostSchemaState(globalDb); err != nil {
		return err
	}
	schemaReady = true
	return nil
}

// ensurePostSchemaState mirrors _ensure_post_schema_state.
// PORT-NOTE: functions owned by other sections are called as bare statements
// (error plumbing, if any, is settled at reconcile).
func ensurePostSchemaState(db *ChDbConnection) error {
	if err := ensureAnomalyRuleSchema(db); err != nil {
		return err
	}
	ensureNotificationSchema(db)
	if err := ensureAiMemorySchema(db); err != nil {
		return err
	}
	if err := ensureGithubWorkItemSchema(db); err != nil {
		return err
	}
	if err := ensureTagRuleSchema(db); err != nil {
		return err
	}
	ensureRawMetricsRetention(db)
	if err := primeLogAttrKeyCache(db); err != nil {
		return err
	}
	seedAppReleaseRegistryFromEnv(db)
	seedCwvAnomalyRules(db)
	// PORT-NOTE: app.config["TESTING"] has no Quart app object here; the Go
	// port gates the example-content seed on SOBS_TESTING instead.
	if !envFlag("SOBS_TESTING", false) {
		seedExampleMetricsContent(db)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Log attribute key caches
// ---------------------------------------------------------------------------

func loadLogAttrKeysFromDb(db *ChDbConnection, recordType string) (map[string]bool, error) {
	res, err := db.Execute(
		"SELECT DISTINCT AttrKey FROM sobs_log_attr_keys FINAL "+
			"WHERE RecordType=? AND IsDeleted=0 ORDER BY AttrKey",
		recordType,
	)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range res.Fetchall() {
		value := fmt.Sprintf("%v", r[res.Cols[0]])
		if strings.TrimSpace(value) != "" {
			out[value] = true
		}
	}
	return out, nil
}

func primeLogAttrKeyCache(db *ChDbConnection) error {
	logAttrKeysLock.Lock()
	defer logAttrKeysLock.Unlock()
	if logAttrKeysCacheLoaded {
		return nil
	}
	for _, recordType := range attrKeyRecordTypes {
		keys, err := loadLogAttrKeysFromDb(db, recordType)
		if err != nil {
			return err
		}
		logAttrKeysByRecordType[recordType] = keys
	}
	logAttrKeysCacheLoaded = true
	return nil
}

func getCachedAttrKeys(db *ChDbConnection, recordType string) ([]string, error) {
	if err := primeLogAttrKeyCache(db); err != nil {
		return nil, err
	}
	logAttrKeysLock.Lock()
	keys := sortedStringSet(logAttrKeysByRecordType[recordType])
	logAttrKeysLock.Unlock()
	return keys, nil
}

// getCachedLogAttrKeys mirrors _get_cached_log_attr_keys(db, record_type="log").
func getCachedLogAttrKeys(db *ChDbConnection, recordType ...string) ([]string, error) {
	rt := "log"
	if len(recordType) > 0 {
		rt = recordType[0]
	}
	return getCachedAttrKeys(db, rt)
}

func rememberAttrKeys(db *ChDbConnection, attrsMaps []map[string]any, recordType string) {
	if len(attrsMaps) == 0 {
		return
	}
	if err := primeLogAttrKeyCache(db); err != nil {
		// PORT-NOTE: Python would propagate; ingest paths treat this as
		// best-effort, so the Go port logs and skips.
		logger.Warn("failed to prime log attribute key cache", "error", err)
		return
	}

	logAttrKeysLock.Lock()
	defer logAttrKeysLock.Unlock()
	existing := logAttrKeysByRecordType[recordType]
	if existing == nil { // setdefault(record_type, set())
		existing = map[string]bool{}
		logAttrKeysByRecordType[recordType] = existing
	}
	if len(existing) >= logAttrKeysMax {
		return
	}

	candidates := map[string]bool{}
	for _, attrs := range attrsMaps {
		if attrs == nil {
			continue
		}
		for rawKey := range attrs {
			key := strings.TrimSpace(rawKey)
			if key == "" || existing[key] || candidates[key] {
				continue
			}
			if len(existing)+len(candidates) >= logAttrKeysMax {
				break
			}
			candidates[key] = true
		}
	}

	if len(candidates) == 0 {
		return
	}

	version := time.Now().UnixMilli()
	sortedCandidates := sortedStringSet(candidates)
	rows := make([]Row, 0, len(sortedCandidates))
	for idx, key := range sortedCandidates {
		rows = append(rows, Row{
			"RecordType": recordType,
			"AttrKey":    key,
			"IsDeleted":  0,
			"Version":    version + int64(idx),
		})
	}
	if _, err := insertRowsJsonEachRow(db, "sobs_log_attr_keys", rows); err != nil {
		logger.Error("failed to persist discovered log attribute keys", "error", err)
		return
	}
	for key := range candidates {
		existing[key] = true
	}
}

// rememberLogAttrKeys mirrors _remember_log_attr_keys(db, maps, record_type="log").
func rememberLogAttrKeys(db *ChDbConnection, attrsMaps []map[string]any, recordType ...string) {
	rt := "log"
	if len(recordType) > 0 {
		rt = recordType[0]
	}
	rememberAttrKeys(db, attrsMaps, rt)
}

func extractAttrMaps(rows []Row, attrField string) []map[string]any {
	maps := []map[string]any{}
	for _, row := range rows {
		rawAttrs, present := row[attrField]
		if !present {
			// row.get(attr_field, {}) default is itself a dict → appended.
			maps = append(maps, map[string]any{})
			continue
		}
		switch attrs := rawAttrs.(type) {
		case map[string]any:
			maps = append(maps, attrs)
		case map[string]string:
			converted := make(map[string]any, len(attrs))
			for k, v := range attrs {
				converted[k] = v
			}
			maps = append(maps, converted)
		}
	}
	return maps
}

func extractLogAttrMaps(rows []Row) []map[string]any {
	return extractAttrMaps(rows, "LogAttributes")
}

// ---------------------------------------------------------------------------
// Idempotent column migrations
// ---------------------------------------------------------------------------

func ensureAnomalyRuleSchema(db *ChDbConnection) error {
	migrationStatements := []string{
		"ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS " +
			"RuleType LowCardinality(String) DEFAULT 'threshold'",
		"ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS " +
			"SecondarySignalSource LowCardinality(String) DEFAULT ''",
		"ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS " +
			"SecondarySignalName LowCardinality(String) DEFAULT ''",
		"ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS " +
			"SecondaryComparator LowCardinality(String) DEFAULT 'gt'",
		"ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS SecondaryWarningThreshold Float64 DEFAULT 0",
		"ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS SecondaryCriticalThreshold Float64 DEFAULT 0",
		"ALTER TABLE sobs_anomaly_rules ADD COLUMN IF NOT EXISTS SeasonalBucketsJson String DEFAULT ''",
	}
	for _, statement := range migrationStatements {
		if _, err := db.Execute(statement); err != nil {
			return err
		}
	}
	return nil
}

func ensureAiMemorySchema(db *ChDbConnection) error {
	migrationStatements := []string{
		"ALTER TABLE sobs_ai_memories ADD COLUMN IF NOT EXISTS EmbeddingJson String DEFAULT ''",
		"ALTER TABLE sobs_ai_memories ADD COLUMN IF NOT EXISTS SourceTurnId String DEFAULT ''",
		"ALTER TABLE sobs_ai_memories ADD COLUMN IF NOT EXISTS UpdatedAt DateTime64(3) DEFAULT now64(3)",
	}
	for _, statement := range migrationStatements {
		if _, err := db.Execute(statement); err != nil {
			return err
		}
	}
	return nil
}

func ensureGithubWorkItemSchema(db *ChDbConnection) error {
	migrationStatements := []string{
		"ALTER TABLE sobs_github_work_items ADD COLUMN IF NOT EXISTS DedupKey String DEFAULT ''",
		"ALTER TABLE sobs_github_work_items ADD COLUMN IF NOT EXISTS " +
			"DedupDecision LowCardinality(String) DEFAULT 'new_issue'",
		"ALTER TABLE sobs_github_work_items ADD COLUMN IF NOT EXISTS DedupConfidence Float64 DEFAULT 0",
		"ALTER TABLE sobs_github_work_items ADD COLUMN IF NOT EXISTS CanonicalIssueNumber UInt32 DEFAULT 0",
		"ALTER TABLE sobs_github_work_items ADD COLUMN IF NOT EXISTS CanonicalIssueUrl String DEFAULT ''",
		"ALTER TABLE sobs_github_work_items ADD COLUMN IF NOT EXISTS RelatedIssueUrls String DEFAULT '[]'",
		"ALTER TABLE sobs_github_work_items ADD COLUMN IF NOT EXISTS OccurrenceCount UInt32 DEFAULT 1",
		"ALTER TABLE sobs_github_work_items ADD COLUMN IF NOT EXISTS " +
			"IssueState LowCardinality(String) DEFAULT ''",
		"ALTER TABLE sobs_github_work_items ADD COLUMN IF NOT EXISTS CopilotAssignmentRequestedAt UInt64 DEFAULT 0",
		"ALTER TABLE sobs_github_work_items ADD COLUMN IF NOT EXISTS " +
			"CopilotAssignmentStatus LowCardinality(String) DEFAULT 'not_requested'",
		"ALTER TABLE sobs_github_work_items ADD COLUMN IF NOT EXISTS CopilotAssignmentReason String DEFAULT ''",
	}
	for _, statement := range migrationStatements {
		if _, err := db.Execute(statement); err != nil {
			return err
		}
	}
	return nil
}

func ensureTagRuleSchema(db *ChDbConnection) error {
	migrationStatements := []string{
		"ALTER TABLE sobs_tag_rules ADD COLUMN IF NOT EXISTS ConditionsJson String DEFAULT ''",
	}
	for _, statement := range migrationStatements {
		if _, err := db.Execute(statement); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Shutdown (app.py _shutdown_db_resources, lines 7484-7511)
// ---------------------------------------------------------------------------

func shutdownDbResources() {
	var threadToJoin chan struct{}
	writeWorkerLock.Lock()
	if writeQueue != nil && writeThread != nil {
		alive := true
		select {
		case <-writeThread:
			alive = false
		default:
		}
		if alive {
			select { // _write_queue.put(_WRITE_STOP, timeout=1); queue.Full → pass
			case writeQueue <- writeStop:
			case <-time.After(time.Second):
			}
			threadToJoin = writeThread
		}
	}
	writeWorkerLock.Unlock()

	if threadToJoin != nil { // thread_to_join.join(timeout=5)
		select {
		case <-threadToJoin:
		case <-time.After(5 * time.Second):
		}
	}

	writeWorkerLock.Lock()
	writeThread = nil
	writeQueue = nil
	writeWorkerLock.Unlock()

	dbInitLock.Lock()
	if globalDb != nil {
		globalDb.Close() // try/except pass — Close never panics
	}
	globalDb = nil
	schemaReady = false
	dbInitLock.Unlock()
}

// PORT-NOTE: atexit.register(_shutdown_db_resources) → shutdownHooks() in
// s01_setup.go calls shutdownDbResources on server shutdown.
