package main

// SOBS MCP (Model Context Protocol) server module (port of mcp.py).
//
// Provides a set of read-only tool endpoints that allow Copilot (VS Code,
// GitHub Copilot Agent) and other MCP-compatible clients to query the SOBS
// observability data (OpenTelemetry logs, traces, and metrics tables) for
// diagnosis and troubleshooting.
//
// Transport: Streamable-HTTP / simple JSON-RPC 2.0 over HTTP POST.
//
// Authentication
// --------------
// All MCP endpoints require a valid MCP API key supplied in the
// "X-MCP-API-Key" request header. Keys are managed via the
// "/settings/mcp" settings page and stored in sobs_app_settings
// under the "mcp.api_keys" setting (JSON list of
// {id, key_hash, label, created_at} objects).
//
// Rate limiting
// -------------
// A simple in-process sliding-window counter limits each source IP to
// mcpRateLimitRequests requests per mcpRateLimitWindowSec seconds.
// Exceeding the limit returns HTTP 429.
//
// Available MCP tools
// -------------------
//   - list_services     – list all distinct service names
//   - query_otel_logs   – query the otel_logs table
//   - query_otel_traces – query the otel_traces table
//   - query_metrics     – query the v_otel_metrics_1m aggregated view
//   - query_metrics_raw – query raw metrics points (gauge / sum / histogram)
//   - get_metric_names  – list all distinct metric names
//   - get_anomaly_rules – list configured anomaly detection rules
//   - get_recent_errors – list recent error-level spans / log events

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/scrypt"
)

// ---------------------------------------------------------------------------
// Blueprint → routes registered in init() at the bottom of this file.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

const (
	mcpRateLimitRequests  = 60 // requests allowed per window
	mcpRateLimitWindowSec = 60 // sliding window size in seconds
)

// {ip: [(timestamp, ...), ...]}
var (
	mcpRateLimitStore = map[string][]time.Time{}
	mcpRateLimitLock  sync.Mutex
)

// mcpCheckRateLimit returns true if the request is allowed, false if it
// should be rate-limited.
func mcpCheckRateLimit(ip string) bool {
	now := time.Now()
	cutoff := now.Add(-mcpRateLimitWindowSec * time.Second)
	mcpRateLimitLock.Lock()
	defer mcpRateLimitLock.Unlock()
	timestamps := mcpRateLimitStore[ip]
	// Discard old entries outside the window.
	kept := timestamps[:0]
	for _, t := range timestamps {
		if !t.Before(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= mcpRateLimitRequests {
		mcpRateLimitStore[ip] = kept
		return false
	}
	mcpRateLimitStore[ip] = append(kept, now)
	return true
}

// ---------------------------------------------------------------------------
// Settings key
// ---------------------------------------------------------------------------

const (
	mcpApiKeysSetting = "mcp.api_keys"
	mcpEnabledSetting = "mcp.enabled"
	mcpApiKeyMax      = 20 // maximum number of concurrent keys
)

// ---------------------------------------------------------------------------
// MCP server identity – shared by GET probe and POST initialize handlers
// ---------------------------------------------------------------------------

const mcpProtocolVersion = "2024-11-05"

var mcpServerInfo = map[string]string{"name": "sobs-mcp", "version": "1.0"}
var mcpCapabilities = map[string]any{"tools": map[string]any{}}

// mcpMacKey returns a per-installation 32-byte key derived from
// SOBS_SECRET_KEY.
//
// The key is used as the scrypt salt so that token fingerprints are
// unique to this SOBS deployment.
func mcpMacKey() []byte {
	secret := os.Getenv("SOBS_SECRET_KEY")
	if secret == "" {
		secret = "sobs-dev-secret-key"
	}
	// blake2b with exactly 16-byte person tag (BLAKE2b requires person <= 16 bytes;
	// null-byte padding is used to reach the required length) to produce a
	// 32-byte sub-key for MCP token fingerprinting.
	// PORT-NOTE: golang.org/x/crypto/blake2b does not expose the BLAKE2b
	// "person" parameter; the person tag is used as the BLAKE2b MAC key
	// instead. Fingerprints therefore differ from the Python deployment and
	// existing MCP keys must be regenerated after migration.
	h, err := blake2b.New512([]byte("sobs-mcp-v1\x00\x00\x00\x00\x00"))
	if err != nil {
		logger.Error("mcpMacKey blake2b init failed", "error", err)
		return make([]byte, 32)
	}
	h.Write([]byte(secret))
	return h.Sum(nil)[:32]
}

// mcpHashKey returns a scrypt-derived hex fingerprint of the given raw API token.
//
// scrypt is a memory-hard KDF (NIST SP 800-132) appropriate for
// one-way token fingerprinting. MCP API tokens are generated with
// 32 random url-safe bytes (192+ bits of entropy). The per-
// installation salt (derived from SOBS_SECRET_KEY) ensures that
// stored fingerprints are unique to this deployment.
//
// Parameters chosen for sub-millisecond latency while still satisfying
// code-scanning policies for key derivation: n=1024 (2^10), r=8, p=1.
func mcpHashKey(rawToken string) string {
	salt := mcpMacKey()
	dk, err := scrypt.Key([]byte(rawToken), salt, 1024, 8, 1, 32)
	if err != nil {
		// PORT-NOTE: Python hashlib.scrypt would raise; with fixed valid
		// parameters this cannot happen. Return a non-matching sentinel.
		logger.Error("mcpHashKey scrypt failed", "error", err)
		return ""
	}
	return hex.EncodeToString(dk)
}

// loadMcpApiKeys loads the list of MCP API key descriptors from sobs_app_settings.
func loadMcpApiKeys(db *ChDbConnection) []map[string]any {
	raw := getAppSetting(db, mcpApiKeysSetting)
	if raw == "" {
		raw = "[]"
	}
	var keys []map[string]any
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return []map[string]any{}
	}
	if keys == nil {
		return []map[string]any{}
	}
	return keys
}

// saveMcpApiKeys persists the MCP API key descriptors to sobs_app_settings.
func saveMcpApiKeys(db *ChDbConnection, keys []map[string]any) {
	raw, err := json.Marshal(keys)
	if err != nil {
		logger.Warn("saveMcpApiKeys marshal failed", "error", err)
		return
	}
	setAppSetting(db, mcpApiKeysSetting, string(raw))
}

// mcpEnabled returns true when the MCP server is enabled.
func mcpEnabled(db *ChDbConnection) bool {
	v := getAppSetting(db, mcpEnabledSetting)
	if v == "" {
		v = "1"
	}
	return v == "1"
}

// ---------------------------------------------------------------------------
// Authentication helper
// ---------------------------------------------------------------------------

// authenticateMcpRequest returns true if the incoming request carries a valid
// MCP API key.
func authenticateMcpRequest(db *ChDbConnection, r *http.Request) bool {
	rawKey := strings.TrimSpace(r.Header.Get("X-MCP-API-Key"))
	if rawKey == "" {
		return false
	}
	keyHash := mcpHashKey(rawKey)
	keys := loadMcpApiKeys(db)
	for _, entry := range keys {
		stored, _ := entry["key_hash"].(string)
		if subtle.ConstantTimeCompare([]byte(stored), []byte(keyHash)) == 1 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// MCP protocol helpers
// ---------------------------------------------------------------------------

// mcpError writes a JSON-RPC 2.0 error response.
func mcpError(w http.ResponseWriter, code int, message string, reqId any) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      reqId,
		"error":   map[string]any{"code": code, "message": message},
	})
}

// mcpResult writes a JSON-RPC 2.0 success response.
func mcpResult(w http.ResponseWriter, result any, reqId any) {
	jsonResponse(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": reqId, "result": result})
}

// ---------------------------------------------------------------------------
// MCP Tool definitions (schema + implementation)
// ---------------------------------------------------------------------------

// mcpTools is the full schema for every tool exported by this MCP server.
var mcpTools = []map[string]any{
	{
		"name": "list_services",
		"description": "List all distinct service names that have sent telemetry to SOBS. " +
			"Useful as a first step to discover what services are being observed.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []any{},
		},
	},
	{
		"name": "query_otel_logs",
		"description": "Query the otel_logs table.  Returns log records matching the given " +
			"filters.  Useful for diagnosing application errors, warning events, " +
			"and general operational log data.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"service": map[string]any{
					"type":        "string",
					"description": "Filter by ServiceName (exact match).",
				},
				"severity": map[string]any{
					"type":        "string",
					"description": "Filter by SeverityText, e.g. ERROR, WARN, INFO.",
				},
				"search": map[string]any{
					"type":        "string",
					"description": "Case-insensitive substring search applied to the Body field.",
				},
				"trace_id": map[string]any{
					"type":        "string",
					"description": "Filter by TraceId (exact match).",
				},
				"from_ts": map[string]any{
					"type": "string",
					"description": "Start of the time window as an ISO-8601 timestamp " +
						"(e.g. 2024-01-15T10:00:00Z).  Defaults to 1 hour ago.",
				},
				"to_ts": map[string]any{
					"type":        "string",
					"description": "End of the time window as an ISO-8601 timestamp.  Defaults to now.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of records to return (1–500, default 100).",
					"minimum":     1,
					"maximum":     500,
				},
			},
			"required": []any{},
		},
	},
	{
		"name": "query_otel_traces",
		"description": "Query the otel_traces table for distributed trace spans. " +
			"Useful for performance analysis, error tracing, and understanding " +
			"service dependencies.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"service": map[string]any{
					"type":        "string",
					"description": "Filter by ServiceName (exact match).",
				},
				"span_name": map[string]any{
					"type":        "string",
					"description": "Filter by SpanName (exact match).",
				},
				"trace_id": map[string]any{
					"type":        "string",
					"description": "Filter by TraceId (exact match).",
				},
				"status_code": map[string]any{
					"type":        "string",
					"description": "Filter by StatusCode, e.g. STATUS_CODE_ERROR.",
				},
				"from_ts": map[string]any{
					"type":        "string",
					"description": "Start of the time window (ISO-8601).  Defaults to 1 hour ago.",
				},
				"to_ts": map[string]any{
					"type":        "string",
					"description": "End of the time window (ISO-8601).  Defaults to now.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of records to return (1–500, default 100).",
					"minimum":     1,
					"maximum":     500,
				},
			},
			"required": []any{},
		},
	},
	{
		"name": "query_metrics",
		"description": "Query the v_otel_metrics_1m pre-aggregated 1-minute metrics view. " +
			"Returns average values and sample counts for the requested metric(s). " +
			"Useful for understanding service health and performance trends.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"service": map[string]any{
					"type":        "string",
					"description": "Filter by ServiceName (exact match).",
				},
				"metric_name": map[string]any{
					"type":        "string",
					"description": "Filter by MetricName (exact match).",
				},
				"metric_kind": map[string]any{
					"type":        "string",
					"description": "Filter by MetricKind: gauge, sum, or histogram.",
				},
				"from_ts": map[string]any{
					"type":        "string",
					"description": "Start of the time window (ISO-8601).  Defaults to 1 hour ago.",
				},
				"to_ts": map[string]any{
					"type":        "string",
					"description": "End of the time window (ISO-8601).  Defaults to now.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of records to return (1–1000, default 200).",
					"minimum":     1,
					"maximum":     1000,
				},
			},
			"required": []any{},
		},
	},
	{
		"name": "query_metrics_raw",
		"description": "Query raw metric data points from otel_metrics_gauge, otel_metrics_sum, " +
			"or otel_metrics_histogram tables. Useful when you need individual data " +
			"points rather than pre-aggregated 1-minute rollups.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"metric_kind": map[string]any{
					"type":        "string",
					"description": "The table to query: gauge, sum, or histogram.  Required.",
					"enum":        []any{"gauge", "sum", "histogram"},
				},
				"service": map[string]any{
					"type":        "string",
					"description": "Filter by ServiceName (exact match).",
				},
				"metric_name": map[string]any{
					"type":        "string",
					"description": "Filter by MetricName (exact match).",
				},
				"from_ts": map[string]any{
					"type":        "string",
					"description": "Start of the time window (ISO-8601).  Defaults to 1 hour ago.",
				},
				"to_ts": map[string]any{
					"type":        "string",
					"description": "End of the time window (ISO-8601).  Defaults to now.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of records to return (1–500, default 100).",
					"minimum":     1,
					"maximum":     500,
				},
			},
			"required": []any{"metric_kind"},
		},
	},
	{
		"name": "get_metric_names",
		"description": "Return a list of all distinct metric names currently stored in SOBS " +
			"along with the last seen timestamp and the service that reported them. " +
			"Useful for discovering which metrics are available to query.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"service": map[string]any{
					"type":        "string",
					"description": "Optional service name filter.",
				},
			},
			"required": []any{},
		},
	},
	{
		"name": "get_anomaly_rules",
		"description": "Return the list of configured anomaly detection rules in SOBS. " +
			"Useful for understanding which metrics are being monitored for " +
			"anomalies and what the configured thresholds are.",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []any{},
		},
	},
	{
		"name": "get_recent_errors",
		"description": "Return recent error-level log events and error-status trace spans. " +
			"Useful for quickly surfacing recent failures across all services.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"service": map[string]any{
					"type":        "string",
					"description": "Filter by ServiceName (exact match).",
				},
				"from_ts": map[string]any{
					"type":        "string",
					"description": "Start of the time window (ISO-8601).  Defaults to 1 hour ago.",
				},
				"to_ts": map[string]any{
					"type":        "string",
					"description": "End of the time window (ISO-8601).  Defaults to now.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum number of records to return (1–200, default 50).",
					"minimum":     1,
					"maximum":     200,
				},
			},
			"required": []any{},
		},
	},
}

// ---------------------------------------------------------------------------
// Time window helpers
// ---------------------------------------------------------------------------

const mcpDefaultWindowHours = 1

// mcpParseTs normalises an ISO-8601 timestamp string for use in ClickHouse queries.
func mcpParseTs(value string) string {
	if value == "" {
		return ""
	}
	// Attempt to parse and re-serialise to a canonical form.
	if strings.HasSuffix(value, "Z") {
		value = value[:len(value)-1] + "+00:00"
	}
	// PORT-NOTE: Python datetime.fromisoformat accepts a broader grammar; the
	// common ISO-8601 layouts are tried here. Naive timestamps are interpreted
	// in the local timezone, mirroring `dt.astimezone(timezone.utc)` on a
	// naive datetime.
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999-07:00",
	} {
		if dt, err := time.Parse(layout, value); err == nil {
			return dt.UTC().Format("2006-01-02 15:04:05")
		}
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
		"2006-01-02",
	} {
		if dt, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return dt.UTC().Format("2006-01-02 15:04:05")
		}
	}
	return ""
}

// mcpBuildTimeWhere appends time-range conditions (and params) to the provided lists.
func mcpBuildTimeWhere(column, fromTs, toTs string, conditions *[]string, params *[]any) {
	// PORT-NOTE: Python used chdb `?` placeholders; the Go core Execute uses
	// `%s` placeholders. The substituted SQL text is identical.
	if fromTs != "" {
		*conditions = append(*conditions, fmt.Sprintf("%s >= %%s", column))
		*params = append(*params, fromTs)
	} else {
		*conditions = append(*conditions, fmt.Sprintf("%s >= now() - INTERVAL %d HOUR", column, mcpDefaultWindowHours))
	}
	if toTs != "" {
		*conditions = append(*conditions, fmt.Sprintf("%s <= %%s", column))
		*params = append(*params, toTs)
	}
}

func mcpClamp(value any, lo, hi, def int) int {
	if value == nil {
		return def
	}
	var n int
	switch v := value.(type) {
	case json.Number:
		if i, err := v.Int64(); err == nil {
			n = int(i)
		} else if f, err := v.Float64(); err == nil {
			n = int(f)
		} else {
			return def
		}
	case float64:
		n = int(v)
	case int:
		n = v
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return def
		}
		n = i
	default:
		return def
	}
	return max(lo, min(hi, n))
}

// mcpNormalizeMapValue returns a map for map-like chDB values across
// runtime/test representations.
func mcpNormalizeMapValue(raw any) map[string]any {
	if raw == nil {
		return map[string]any{}
	}
	switch v := raw.(type) {
	case map[string]any:
		return v
	case map[string]string:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = val
		}
		return out
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return map[string]any{}
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(text), &parsed); err == nil {
			return parsed
		}
		// PORT-NOTE: Python falls back to ast.literal_eval for Python-dict
		// repr strings. Approximated by rewriting Python literal syntax to
		// JSON and re-parsing; non-trivial literals fall through to {}.
		jsonish := strings.NewReplacer("'", `"`, "None", "null", "True", "true", "False", "false").Replace(text)
		parsed = nil
		if err := json.Unmarshal([]byte(jsonish), &parsed); err == nil {
			return parsed
		}
		return map[string]any{}
	}
	return map[string]any{}
}

// mcpArgStr mirrors `(args.get(key) or "")` for string tool arguments.
func mcpArgStr(args map[string]any, key string) string {
	if v, ok := args[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// mcpRowStr returns a row value as a string (toString() columns).
func mcpRowStr(row Row, col string) string {
	if v, ok := row[col]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprintf("%v", v)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Tool implementations
// ---------------------------------------------------------------------------

func mcpToolListServices(db *ChDbConnection, _ map[string]any) (map[string]any, error) {
	res, err := db.Execute(
		"SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName != '' " +
			"UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName != '' " +
			"UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_metrics_gauge WHERE ServiceName != '' " +
			"ORDER BY ServiceName")
	if err != nil {
		return nil, err
	}
	services := []any{}
	for _, row := range res.Fetchall() {
		services = append(services, row["ServiceName"])
	}
	return map[string]any{"services": services}, nil
}

func mcpToolQueryOtelLogs(db *ChDbConnection, args map[string]any) (map[string]any, error) {
	service := strings.TrimSpace(mcpArgStr(args, "service"))
	severity := strings.ToUpper(strings.TrimSpace(mcpArgStr(args, "severity")))
	search := strings.TrimSpace(mcpArgStr(args, "search"))
	traceId := strings.TrimSpace(mcpArgStr(args, "trace_id"))
	fromTs := mcpParseTs(mcpArgStr(args, "from_ts"))
	toTs := mcpParseTs(mcpArgStr(args, "to_ts"))
	limit := mcpClamp(args["limit"], 1, 500, 100)

	var conditions []string
	var params []any

	mcpBuildTimeWhere("Timestamp", fromTs, toTs, &conditions, &params)

	if service != "" {
		conditions = append(conditions, "ServiceName = %s")
		params = append(params, service)
	}
	if severity != "" {
		conditions = append(conditions, "SeverityText = %s")
		params = append(params, severity)
	}
	if traceId != "" {
		conditions = append(conditions, "TraceId = %s")
		params = append(params, traceId)
	}
	if search != "" {
		conditions = append(conditions, "Body ILIKE %s")
		params = append(params, "%"+search+"%")
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	sql := fmt.Sprintf(
		"SELECT toString(Timestamp) AS ts, ServiceName, SeverityText, "+
			"Body, TraceId, SpanId, LogAttributes "+
			"FROM otel_logs %s "+
			"ORDER BY Timestamp DESC LIMIT %d", where, limit)
	res, err := db.Execute(sql, params...)
	if err != nil {
		return nil, err
	}
	result := []any{}
	for _, row := range res.Fetchall() {
		result = append(result, map[string]any{
			"ts":         row["ts"],
			"service":    row["ServiceName"],
			"severity":   row["SeverityText"],
			"body":       row["Body"],
			"trace_id":   row["TraceId"],
			"span_id":    row["SpanId"],
			"attributes": mcpNormalizeMapValue(row["LogAttributes"]),
		})
	}
	return map[string]any{"count": len(result), "rows": result}, nil
}

func mcpToolQueryOtelTraces(db *ChDbConnection, args map[string]any) (map[string]any, error) {
	service := strings.TrimSpace(mcpArgStr(args, "service"))
	spanName := strings.TrimSpace(mcpArgStr(args, "span_name"))
	traceId := strings.TrimSpace(mcpArgStr(args, "trace_id"))
	statusCode := strings.TrimSpace(mcpArgStr(args, "status_code"))
	fromTs := mcpParseTs(mcpArgStr(args, "from_ts"))
	toTs := mcpParseTs(mcpArgStr(args, "to_ts"))
	limit := mcpClamp(args["limit"], 1, 500, 100)

	var conditions []string
	var params []any

	mcpBuildTimeWhere("Timestamp", fromTs, toTs, &conditions, &params)

	if service != "" {
		conditions = append(conditions, "ServiceName = %s")
		params = append(params, service)
	}
	if spanName != "" {
		conditions = append(conditions, "SpanName = %s")
		params = append(params, spanName)
	}
	if traceId != "" {
		conditions = append(conditions, "TraceId = %s")
		params = append(params, traceId)
	}
	if statusCode != "" {
		conditions = append(conditions, "StatusCode = %s")
		params = append(params, statusCode)
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	sql := fmt.Sprintf(
		"SELECT toString(Timestamp) AS ts, ServiceName, TraceId, SpanId, "+
			"SpanName, SpanKind, StatusCode, StatusMessage, "+
			"toUInt64(Duration / 1000000) AS duration_ms "+
			"FROM otel_traces %s "+
			"ORDER BY Timestamp DESC LIMIT %d", where, limit)
	res, err := db.Execute(sql, params...)
	if err != nil {
		return nil, err
	}
	result := []any{}
	for _, row := range res.Fetchall() {
		result = append(result, map[string]any{
			"ts":             row["ts"],
			"service":        row["ServiceName"],
			"trace_id":       row["TraceId"],
			"span_id":        row["SpanId"],
			"span_name":      row["SpanName"],
			"span_kind":      row["SpanKind"],
			"status_code":    row["StatusCode"],
			"status_message": row["StatusMessage"],
			"duration_ms":    row["duration_ms"],
		})
	}
	return map[string]any{"count": len(result), "rows": result}, nil
}

func mcpToolQueryMetrics(db *ChDbConnection, args map[string]any) (map[string]any, error) {
	service := strings.TrimSpace(mcpArgStr(args, "service"))
	metricName := strings.TrimSpace(mcpArgStr(args, "metric_name"))
	metricKind := strings.ToLower(strings.TrimSpace(mcpArgStr(args, "metric_kind")))
	fromTs := mcpParseTs(mcpArgStr(args, "from_ts"))
	toTs := mcpParseTs(mcpArgStr(args, "to_ts"))
	limit := mcpClamp(args["limit"], 1, 1000, 200)

	var conditions []string
	var params []any

	mcpBuildTimeWhere("MinuteBucket", fromTs, toTs, &conditions, &params)

	if service != "" {
		conditions = append(conditions, "ServiceName = %s")
		params = append(params, service)
	}
	if metricName != "" {
		conditions = append(conditions, "MetricName = %s")
		params = append(params, metricName)
	}
	if metricKind == "gauge" || metricKind == "sum" || metricKind == "histogram" {
		conditions = append(conditions, "MetricKind = %s")
		params = append(params, metricKind)
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	sql := fmt.Sprintf(
		"SELECT toString(MinuteBucket) AS ts, ServiceName, MetricName, "+
			"MetricKind, Value, SampleCount "+
			"FROM v_otel_metrics_1m %s "+
			"ORDER BY MinuteBucket DESC LIMIT %d", where, limit)
	res, err := db.Execute(sql, params...)
	if err != nil {
		return nil, err
	}
	result := []any{}
	for _, row := range res.Fetchall() {
		result = append(result, map[string]any{
			"ts":           row["ts"],
			"service":      row["ServiceName"],
			"metric_name":  row["MetricName"],
			"metric_kind":  row["MetricKind"],
			"value":        row["Value"],
			"sample_count": row["SampleCount"],
		})
	}
	return map[string]any{"count": len(result), "rows": result}, nil
}

var mcpRawMetricTables = map[string]string{
	"gauge":     "otel_metrics_gauge",
	"sum":       "otel_metrics_sum",
	"histogram": "otel_metrics_histogram",
}

func mcpToolQueryMetricsRaw(db *ChDbConnection, args map[string]any) (map[string]any, error) {
	metricKind := strings.ToLower(strings.TrimSpace(mcpArgStr(args, "metric_kind")))
	table, ok := mcpRawMetricTables[metricKind]
	if !ok {
		return map[string]any{"error": "metric_kind must be one of: gauge, sum, histogram"}, nil
	}

	service := strings.TrimSpace(mcpArgStr(args, "service"))
	metricName := strings.TrimSpace(mcpArgStr(args, "metric_name"))
	fromTs := mcpParseTs(mcpArgStr(args, "from_ts"))
	toTs := mcpParseTs(mcpArgStr(args, "to_ts"))
	limit := mcpClamp(args["limit"], 1, 500, 100)

	var conditions []string
	var params []any

	mcpBuildTimeWhere("TimeUnix", fromTs, toTs, &conditions, &params)

	if service != "" {
		conditions = append(conditions, "ServiceName = %s")
		params = append(params, service)
	}
	if metricName != "" {
		conditions = append(conditions, "MetricName = %s")
		params = append(params, metricName)
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	result := []any{}
	if metricKind == "histogram" {
		sql := fmt.Sprintf(
			"SELECT toString(TimeUnix) AS ts, ServiceName, MetricName, "+
				"MetricUnit, Attributes, Count, Sum "+
				"FROM %s %s "+
				"ORDER BY TimeUnix DESC LIMIT %d", table, where, limit)
		res, err := db.Execute(sql, params...)
		if err != nil {
			return nil, err
		}
		for _, row := range res.Fetchall() {
			result = append(result, map[string]any{
				"ts":          row["ts"],
				"service":     row["ServiceName"],
				"metric_name": row["MetricName"],
				"metric_unit": row["MetricUnit"],
				"attributes":  mcpNormalizeMapValue(row["Attributes"]),
				"count":       row["Count"],
				"sum":         row["Sum"],
			})
		}
	} else {
		sql := fmt.Sprintf(
			"SELECT toString(TimeUnix) AS ts, ServiceName, MetricName, "+
				"MetricUnit, Attributes, Value "+
				"FROM %s %s "+
				"ORDER BY TimeUnix DESC LIMIT %d", table, where, limit)
		res, err := db.Execute(sql, params...)
		if err != nil {
			return nil, err
		}
		for _, row := range res.Fetchall() {
			result = append(result, map[string]any{
				"ts":          row["ts"],
				"service":     row["ServiceName"],
				"metric_name": row["MetricName"],
				"metric_unit": row["MetricUnit"],
				"attributes":  mcpNormalizeMapValue(row["Attributes"]),
				"value":       row["Value"],
			})
		}
	}
	return map[string]any{"count": len(result), "rows": result}, nil
}

func mcpToolGetMetricNames(db *ChDbConnection, args map[string]any) (map[string]any, error) {
	service := strings.TrimSpace(mcpArgStr(args, "service"))
	var conditions []string
	var params []any
	if service != "" {
		conditions = append(conditions, "ServiceName = %s")
		params = append(params, service)
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}
	sql := "SELECT MetricName, ServiceName, max(toString(TimeUnixMs)) AS last_seen " +
		fmt.Sprintf("FROM otel_metrics_gauge %s ", where) +
		"GROUP BY MetricName, ServiceName " +
		"UNION ALL " +
		"SELECT MetricName, ServiceName, max(toString(TimeUnixMs)) AS last_seen " +
		fmt.Sprintf("FROM otel_metrics_sum %s ", where) +
		"GROUP BY MetricName, ServiceName " +
		"UNION ALL " +
		"SELECT MetricName, ServiceName, max(toString(TimeUnixMs)) AS last_seen " +
		fmt.Sprintf("FROM otel_metrics_histogram %s ", where) +
		"GROUP BY MetricName, ServiceName " +
		"ORDER BY MetricName, ServiceName"
	// Each UNION branch uses the same WHERE clause with one param per branch.
	var allParams []any
	if len(params) > 0 {
		for i := 0; i < 3; i++ {
			allParams = append(allParams, params...)
		}
	}
	res, err := db.Execute(sql, allParams...)
	if err != nil {
		return nil, err
	}
	result := []any{}
	for _, row := range res.Fetchall() {
		result = append(result, map[string]any{
			"metric_name": row["MetricName"],
			"service":     row["ServiceName"],
			"last_seen":   row["last_seen"],
		})
	}
	return map[string]any{"count": len(result), "metrics": result}, nil
}

func mcpToolGetAnomalyRules(db *ChDbConnection, _ map[string]any) (map[string]any, error) {
	sql := "SELECT Id, Name, RuleType, SignalSource, SignalName, ServiceName, " +
		"Comparator, WarningThreshold, CriticalThreshold " +
		"FROM sobs_anomaly_rules FINAL " +
		"WHERE IsDeleted = 0 " +
		"ORDER BY SignalSource, SignalName"
	res, err := db.Execute(sql)
	if err != nil {
		return nil, err
	}
	result := []any{}
	for _, row := range res.Fetchall() {
		result = append(result, map[string]any{
			"id":                 row["Id"],
			"name":               row["Name"],
			"rule_type":          row["RuleType"],
			"signal_source":      row["SignalSource"],
			"signal_name":        row["SignalName"],
			"service":            row["ServiceName"],
			"comparator":         row["Comparator"],
			"warning_threshold":  row["WarningThreshold"],
			"critical_threshold": row["CriticalThreshold"],
		})
	}
	return map[string]any{"count": len(result), "rules": result}, nil
}

func mcpToolGetRecentErrors(db *ChDbConnection, args map[string]any) (map[string]any, error) {
	service := strings.TrimSpace(mcpArgStr(args, "service"))
	fromTs := mcpParseTs(mcpArgStr(args, "from_ts"))
	toTs := mcpParseTs(mcpArgStr(args, "to_ts"))
	limit := mcpClamp(args["limit"], 1, 200, 50)

	var logConditions []string
	var logParams []any
	mcpBuildTimeWhere("Timestamp", fromTs, toTs, &logConditions, &logParams)
	logConditions = append(logConditions, "SeverityText IN ('ERROR', 'FATAL', 'CRITICAL')")
	if service != "" {
		logConditions = append(logConditions, "ServiceName = %s")
		logParams = append(logParams, service)
	}
	logWhere := "WHERE " + strings.Join(logConditions, " AND ")

	var traceConditions []string
	var traceParams []any
	mcpBuildTimeWhere("Timestamp", fromTs, toTs, &traceConditions, &traceParams)
	traceConditions = append(traceConditions, "StatusCode = 'STATUS_CODE_ERROR'")
	if service != "" {
		traceConditions = append(traceConditions, "ServiceName = %s")
		traceParams = append(traceParams, service)
	}
	traceWhere := "WHERE " + strings.Join(traceConditions, " AND ")

	half := limit / 2
	if half == 0 {
		half = 1
	}
	logSql := fmt.Sprintf(
		"SELECT toString(Timestamp) AS ts, ServiceName, 'log' AS source, "+
			"SeverityText AS level_or_status, Body AS message, TraceId "+
			"FROM otel_logs %s "+
			"ORDER BY Timestamp DESC LIMIT %d", logWhere, half)
	traceSql := fmt.Sprintf(
		"SELECT toString(Timestamp) AS ts, ServiceName, 'trace' AS source, "+
			"StatusCode AS level_or_status, SpanName AS message, TraceId "+
			"FROM otel_traces %s "+
			"ORDER BY Timestamp DESC LIMIT %d", traceWhere, half)

	logRes, err := db.Execute(logSql, logParams...)
	if err != nil {
		return nil, err
	}
	traceRes, err := db.Execute(traceSql, traceParams...)
	if err != nil {
		return nil, err
	}

	rowToDict := func(row Row) map[string]any {
		return map[string]any{
			"ts":              row["ts"],
			"service":         row["ServiceName"],
			"source":          row["source"],
			"level_or_status": row["level_or_status"],
			"message":         row["message"],
			"trace_id":        row["TraceId"],
		}
	}

	result := []map[string]any{}
	for _, r := range logRes.Fetchall() {
		result = append(result, rowToDict(r))
	}
	for _, r := range traceRes.Fetchall() {
		result = append(result, rowToDict(r))
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, _ := result[i]["ts"].(string)
		b, _ := result[j]["ts"].(string)
		return a > b
	})
	return map[string]any{"count": len(result), "errors": result}, nil
}

// ---------------------------------------------------------------------------
// Tool dispatch table
// ---------------------------------------------------------------------------

var mcpToolHandlers = map[string]func(*ChDbConnection, map[string]any) (map[string]any, error){
	"list_services":     mcpToolListServices,
	"query_otel_logs":   mcpToolQueryOtelLogs,
	"query_otel_traces": mcpToolQueryOtelTraces,
	"query_metrics":     mcpToolQueryMetrics,
	"query_metrics_raw": mcpToolQueryMetricsRaw,
	"get_metric_names":  mcpToolGetMetricNames,
	"get_anomaly_rules": mcpToolGetAnomalyRules,
	"get_recent_errors": mcpToolGetRecentErrors,
}

// ---------------------------------------------------------------------------
// MCP HTTP endpoints
// ---------------------------------------------------------------------------

// mcpListTools returns the list of MCP tools this server exposes (no auth required).
func mcpListTools(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{
		"jsonrpc": "2.0",
		"id":      nil,
		"result": map[string]any{
			"tools": mcpTools,
		},
	})
}

// mcpEndpointGet handles GET /mcp – MCP transport compatibility probe.
//
// Per the MCP Streamable HTTP transport specification, clients (including
// VS Code) may send a GET request to the endpoint before establishing
// a session. Returning 405 here breaks those clients even when the
// POST endpoint works correctly.
//
// This handler returns a lightweight 200 OK response with the server
// capability descriptor so that clients can discover the server without
// starting a full JSON-RPC session. No authentication is required.
func mcpEndpointGet(w http.ResponseWriter, _ *http.Request) {
	db := getDb()
	if !mcpEnabled(db) {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": -32001, "message": "MCP server is disabled."},
		})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    mcpCapabilities,
		"serverInfo":      mcpServerInfo,
	})
}

// mcpEndpoint is the main MCP JSON-RPC 2.0 endpoint.
//
// Accepts initialize, ping, notifications/*, tools/list,
// and tools/call method calls.
//
// Authentication
// --------------
// Set "X-MCP-API-Key: <key>" in the request header.
// initialize, ping, and notifications/* do not require a key.
//
// Rate limiting
// -------------
// Each IP is limited to 60 requests per minute.
func mcpEndpoint(w http.ResponseWriter, r *http.Request) {
	// Rate limiting.
	clientIp := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
	if clientIp == "" {
		// PORT-NOTE: Quart's request.remote_addr is the bare IP; Go's
		// RemoteAddr includes the port, so it is stripped here.
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			clientIp = host
		} else {
			clientIp = r.RemoteAddr
		}
	}
	if clientIp == "" {
		clientIp = "unknown"
	}
	if !mcpCheckRateLimit(clientIp) {
		jsonResponse(w, http.StatusTooManyRequests, map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": -32000, "message": "Rate limit exceeded. Try again later."},
		})
		return
	}

	db := getDb()

	// Require MCP to be enabled.
	if !mcpEnabled(db) {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": -32001, "message": "MCP server is disabled."},
		})
		return
	}

	// Parse the JSON-RPC body.
	body, err := readJsonBody(r)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": -32700, "message": "Parse error"},
		})
		return
	}

	reqId := body["id"]
	method, _ := body["method"].(string)

	// The "initialize" method is used by MCP clients to negotiate capabilities.
	// It does NOT require an API key so that clients can discover the server.
	if method == "initialize" {
		jsonResponse(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      reqId,
			"result": map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities":    mcpCapabilities,
				"serverInfo":      mcpServerInfo,
			},
		})
		return
	}

	// MCP notifications are fire-and-forget messages sent by the client (e.g.
	// "notifications/initialized" after the handshake, "notifications/cancelled"
	// when cancelling an in-flight request). Per the MCP Streamable HTTP transport
	// spec, the server MUST respond with HTTP 202 Accepted and an empty body – it
	// must NOT return a JSON-RPC error, because notifications have no "id" and
	// the client does not read the response body. Returning an error here breaks
	// mainstream MCP clients (VS Code, etc.) at startup.
	if strings.HasPrefix(method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// The "ping" utility method can be sent by either party to check liveness.
	// It requires no API key and returns an empty result object.
	if method == "ping" {
		jsonResponse(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": reqId, "result": map[string]any{}})
		return
	}

	// All other methods require authentication.
	if !authenticateMcpRequest(db, r) {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{
			"jsonrpc": "2.0",
			"id":      reqId,
			"error":   map[string]any{"code": -32002, "message": "Unauthorized: missing or invalid X-MCP-API-Key header."},
		})
		return
	}

	if method == "tools/list" {
		jsonResponse(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      reqId,
			"result":  map[string]any{"tools": mcpTools},
		})
		return
	}

	if method == "tools/call" {
		params, _ := body["params"].(map[string]any)
		toolName, _ := params["name"].(string)
		toolArgs, ok := params["arguments"].(map[string]any)
		if !ok {
			toolArgs = map[string]any{}
		}

		handler, found := mcpToolHandlers[toolName]
		if !found {
			names := make([]string, 0, len(mcpToolHandlers))
			for name := range mcpToolHandlers {
				names = append(names, "'"+name+"'")
			}
			sort.Strings(names)
			jsonResponse(w, http.StatusNotFound, map[string]any{
				"jsonrpc": "2.0",
				"id":      reqId,
				"error": map[string]any{
					"code":    -32601,
					"message": fmt.Sprintf("Unknown tool: '%s'. Available: [%s]", toolName, strings.Join(names, ", ")),
				},
			})
			return
		}

		toolResult, err := handler(db, toolArgs)
		if err != nil {
			logger.Error(fmt.Sprintf("MCP tool '%s' raised an error", toolName), "error", err)
			// PORT-NOTE: Python exposes the exception class name; Go exposes
			// the concrete error type name instead.
			jsonResponse(w, http.StatusInternalServerError, map[string]any{
				"jsonrpc": "2.0",
				"id":      reqId,
				"error":   map[string]any{"code": -32603, "message": fmt.Sprintf("Internal error: %T", err)},
			})
			return
		}
		// Apply the same output masking used across SOBS UI routes so that
		// PII / secrets in log bodies, span names, and attributes are
		// redacted before they leave the server.
		masked := maskValueForOutput(toolResult, db)
		text, err := json.Marshal(masked)
		if err != nil {
			// PORT-NOTE: Python json.dumps(default=str) cannot fail here;
			// Go marshal errors map to the same internal-error response.
			logger.Error(fmt.Sprintf("MCP tool '%s' raised an error", toolName), "error", err)
			jsonResponse(w, http.StatusInternalServerError, map[string]any{
				"jsonrpc": "2.0",
				"id":      reqId,
				"error":   map[string]any{"code": -32603, "message": fmt.Sprintf("Internal error: %T", err)},
			})
			return
		}

		jsonResponse(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0",
			"id":      reqId,
			"result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": string(text)}},
				"isError": false,
			},
		})
		return
	}

	// Unknown method.
	jsonResponse(w, http.StatusNotFound, map[string]any{
		"jsonrpc": "2.0",
		"id":      reqId,
		"error":   map[string]any{"code": -32601, "message": fmt.Sprintf("Method not found: '%s'", method)},
	})
}

// ---------------------------------------------------------------------------
// Settings API endpoints (key management)
// ---------------------------------------------------------------------------

// mcpApiListKeys lists MCP API key descriptors (hashes are not exposed; only metadata).
func mcpApiListKeys(w http.ResponseWriter, r *http.Request) {
	requireBasicAuth(func(w http.ResponseWriter, _ *http.Request) {
		db := getDb()
		keys := loadMcpApiKeys(db)
		// Return metadata only – never expose raw keys or hashes.
		safe := []any{}
		for _, k := range keys {
			safe = append(safe, map[string]any{
				"id":         mcpKeyField(k, "id"),
				"label":      mcpKeyField(k, "label"),
				"created_at": mcpKeyField(k, "created_at"),
				"expires_at": k["expires_at"],
			})
		}
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "keys": safe})
	})(w, r)
}

// mcpKeyField mirrors `k.get(field, "")` for key descriptor maps.
func mcpKeyField(k map[string]any, field string) any {
	if v, ok := k[field]; ok && v != nil {
		return v
	}
	return ""
}

// mcpApiCreateKey generates a new MCP API key.
func mcpApiCreateKey(w http.ResponseWriter, r *http.Request) {
	requireBasicAuth(func(w http.ResponseWriter, r *http.Request) {
		db := getDb()
		keys := loadMcpApiKeys(db)
		if len(keys) >= mcpApiKeyMax {
			jsonResponse(w, http.StatusBadRequest, map[string]any{
				"ok":    false,
				"error": fmt.Sprintf("Maximum of %d keys reached.", mcpApiKeyMax),
			})
			return
		}

		body, _ := readJsonBody(r) // silent=True → ignore parse errors, use {}
		label := ""
		if v, ok := body["label"]; ok && v != nil {
			label = fmt.Sprintf("%v", v)
		}
		label = strings.TrimSpace(label)
		if runes := []rune(label); len(runes) > 128 {
			label = string(runes[:128])
		}
		if label == "" {
			label = "API Key"
		}
		expiresAt := body["expires_at"] // Optional ISO 8601 expiry date
		createdAt := strings.Replace(pyIsoFormat(time.Now().UTC()), "+00:00", "Z", 1)

		rawKey := "smcp_" + mcpTokenUrlsafe(32)
		keyId := mcpTokenHex(8)
		keys = append(keys, map[string]any{
			"id":         keyId,
			"label":      label,
			"key_hash":   mcpHashKey(rawKey),
			"created_at": createdAt,
			"expires_at": expiresAt,
		})
		saveMcpApiKeys(db, keys)
		jsonResponse(w, http.StatusOK, map[string]any{
			"ok":         true,
			"id":         keyId,
			"key":        rawKey,
			"label":      label,
			"created_at": createdAt,
			"expires_at": expiresAt,
		})
	})(w, r)
}

// mcpTokenUrlsafe mirrors secrets.token_urlsafe(n).
func mcpTokenUrlsafe(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// mcpTokenHex mirrors secrets.token_hex(n).
func mcpTokenHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// mcpApiDeleteKey revokes (deletes) an MCP API key by its ID.
func mcpApiDeleteKey(w http.ResponseWriter, r *http.Request) {
	keyId := r.PathValue("key_id")
	requireBasicAuth(func(w http.ResponseWriter, _ *http.Request) {
		db := getDb()
		keys := loadMcpApiKeys(db)
		newKeys := []map[string]any{}
		for _, k := range keys {
			id, _ := k["id"].(string)
			if id != keyId {
				newKeys = append(newKeys, k)
			}
		}
		if len(newKeys) == len(keys) {
			jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Key not found."})
			return
		}
		saveMcpApiKeys(db, newKeys)
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
	})(w, r)
}

// mcpApiSetEnabled enables or disables the MCP server.
func mcpApiSetEnabled(w http.ResponseWriter, r *http.Request) {
	requireBasicAuth(func(w http.ResponseWriter, r *http.Request) {
		db := getDb()
		body, _ := readJsonBody(r) // silent=True → ignore parse errors, use {}
		enabled := true
		if v, ok := body["enabled"]; ok {
			enabled = mcpTruthy(v)
		}
		val := "0"
		if enabled {
			val = "1"
		}
		setAppSetting(db, mcpEnabledSetting, val)
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "enabled": enabled})
	})(w, r)
}

// mcpTruthy mirrors Python bool(value) for JSON-decoded values.
func mcpTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, err := t.Float64()
		return err != nil || f != 0
	case float64:
		return t != 0
	case int:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	default:
		return true
	}
}

// ---------------------------------------------------------------------------
// Settings UI page
// ---------------------------------------------------------------------------

// mcpSettingsPage renders the MCP API key management settings page.
func mcpSettingsPage(w http.ResponseWriter, r *http.Request) {
	requireBasicAuth(func(w http.ResponseWriter, r *http.Request) {
		db := getDb()
		keys := loadMcpApiKeys(db)
		enabled := mcpEnabled(db)
		safeKeys := []map[string]any{}
		for _, k := range keys {
			safeKeys = append(safeKeys, map[string]any{
				"id":         mcpKeyField(k, "id"),
				"label":      mcpKeyField(k, "label"),
				"created_at": mcpKeyField(k, "created_at"),
				"expires_at": k["expires_at"],
			})
		}
		nowIsoVal := pyIsoFormat(time.Now().UTC())
		renderTemplate(w, r, "settings_mcp.html", map[string]any{
			"mcp_keys":    safeKeys,
			"mcp_enabled": enabled,
			"now_iso":     nowIsoVal,
		})
	})(w, r)
}

// ---------------------------------------------------------------------------
// Route registration (mcp_bp blueprint)
// ---------------------------------------------------------------------------

func init() {
	registerRoute("GET", "/mcp/tools", mcpListTools)
	registerRoute("GET", "/mcp", mcpEndpointGet)
	registerRoute("POST", "/mcp", mcpEndpoint)
	registerRoute("GET", "/api/mcp/keys", mcpApiListKeys)
	registerRoute("POST", "/api/mcp/keys", mcpApiCreateKey)
	registerRoute("DELETE", "/api/mcp/keys/{key_id}", mcpApiDeleteKey)
	registerRoute("POST", "/api/mcp/enabled", mcpApiSetEnabled)
	registerRoute("GET", "/settings/mcp", mcpSettingsPage)
}
