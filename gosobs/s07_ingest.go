// s07_ingest.go — port of app.py lines 9651-10742.
//
// OTLP ingest (POST /v1/logs, /v1/traces, /v1/metrics), RUM asset upload and
// download, RUM client tokens, RUM ingest (POST /v1/rum), AI transparency
// ingest (POST /v1/ai), direct error ingest (POST /v1/errors) and the
// App/Release/Artifact registry APIs (/v1/apps*, /v1/releases*), plus the
// error-source SQL and error item helpers used by the errors UI section.
//
// PORT-NOTE: symbols owned by other sections are referenced via the
// deterministic naming rule with the following assumed signatures:
//
//	parseOtlpRequest(w, r, msg) bool            — writes the 400 error response
//	                                               itself and returns false on failure
//	queueWrite(op func(*ChDbConnection) error, wait bool) error
//	protoLogsToEvents(msg) []LogEvent
//	protoTracesToEvents(msg) ([]SpanEvent, []ErrorEvent)
//	protoMetricsToEvents(msg) ([]TypedMetricEvent, error)
//	insertLogEvents(db, events) (int, error)
//	insertSpanEvents(db, events) (int, error)
//	insertErrorEvents(db, events) error
//	insertMetricEvents(db, events) (int, error)
//	verifyRumAssetSignature(r, body, method, path, contentType, assetType, assetName) (bool, string)
//	verifyRumClientAuth(r, events) (ok bool, status int, errMsg string)
//	requestOrigin(r) string / normalizeOrigin(v) string
//	rumClientTokenEncode(claims map[string]any) string
//	findAppById / findReleaseById (db, id) (Row, error)   — nil Row when missing
//	serializeAppRow / serializeReleaseRow / serializeArtifactRow (Row) map[string]any
//	loadTagRules(db) ([]map[string]any, error)
//	applyTagRules(db, recordType string, rows []Row, rules []map[string]any) error
//	stringifyAttrs(map[string]any) map[string]string
//	mapToDict(any) map[string]any
//	severityNumber(level string) int
//	maybeDemangleJsStack(string) string / remapRumConsoleStacks(map[string]any)
//	sanitizeRumAssetType / sanitizeRumAssetName / assetExtension / rumAssetMetaPath
//	appSlug(string) string, parseBool(any, bool) bool,
//	safeJsonDumps(any) string, safeJsonLoads(any, any) any,
//	errorId(ts, service, errType, message, traceId, spanId string) string
//	requireApiKey / requireBasicAuth (http.HandlerFunc) http.HandlerFunc
//
// PORT-NOTE: `wait = bool(app.config.get("TESTING", False))` is gated on the
// SOBS_TESTING env flag, matching the precedent set in s02_db.go.
// PORT-NOTE: where Python used json.dumps(..., ensure_ascii=False) the port
// uses safeJsonDumps (identical output for JSON-serialisable inputs except
// Python's ", "/": " separators; Go emits compact ","/":").
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

func init() {
	registerRoute("OPTIONS", "/v1/logs", ingestPreflight)
	registerRoute("OPTIONS", "/v1/traces", ingestPreflight)
	registerRoute("OPTIONS", "/v1/metrics", ingestPreflight)
	registerRoute("OPTIONS", "/v1/rum/assets", ingestPreflight)
	registerRoute("POST", "/v1/logs", requireApiKey(ingestLogs))
	registerRoute("POST", "/v1/rum/assets", requireApiKey(ingestRumAsset))
	registerRoute("GET", "/v1/rum/assets/{asset_id}", requireBasicAuth(rumAssetDownload))
	registerRoute("POST", "/v1/rum/client-token", requireApiKey(issueRumClientToken))
	registerRoute("POST", "/v1/traces", requireApiKey(ingestTraces))
	registerRoute("POST", "/v1/metrics", requireApiKey(ingestMetrics))
	registerRoute("POST", "/v1/rum", requireApiKey(ingestRum))
	registerRoute("POST", "/v1/ai", requireApiKey(ingestAi))
	registerRoute("POST", "/v1/errors", requireApiKey(ingestErrors))
	registerRoute("GET", "/v1/apps", requireApiKey(listApps))
	registerRoute("POST", "/v1/apps", requireApiKey(createAppRegistryEntry))
	registerRoute("GET", "/v1/apps/{app_id}", requireApiKey(getAppRegistryEntry))
	registerRoute("PATCH", "/v1/apps/{app_id}", requireApiKey(updateAppRegistryEntry))
	registerRoute("GET", "/v1/apps/{app_id}/releases", requireApiKey(listAppReleases))
	registerRoute("POST", "/v1/apps/{app_id}/releases", requireApiKey(createAppRelease))
	registerRoute("GET", "/v1/releases/{release_id}", requireApiKey(getRelease))
	registerRoute("GET", "/v1/releases/{release_id}/artifacts", requireApiKey(listReleaseArtifacts))
	registerRoute("POST", "/v1/releases/{release_id}/artifacts/meta", requireApiKey(createReleaseArtifactMeta))
}

// uuid4Hex mirrors uuid.uuid4().hex (32 lowercase hex chars, RFC 4122 v4).
func uuid4Hex() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// OTLP Ingest – Logs  POST /v1/logs
// ---------------------------------------------------------------------------

// ingestPreflight handles CORS preflight for the ingest endpoints
// (`return "", 204`; CORS headers are added by the security middleware).
func ingestPreflight(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func ingestLogs(w http.ResponseWriter, r *http.Request) {
	endSpan := telemetrySpan("sobs.ingest.request", map[string]any{"route": "/v1/logs", "event.type": "log"})
	defer endSpan()

	msg := &collogspb.ExportLogsServiceRequest{}
	if !parseOtlpRequest(w, r, msg) {
		return
	}
	endParse := telemetrySpan("sobs.ingest.parse", map[string]any{"event.type": "log", "parser": "otlp"})
	events := protoLogsToEvents(msg)
	endParse()

	wait := envFlag("SOBS_TESTING", false)
	err := queueWrite(func(db *ChDbConnection) error {
		_, ierr := insertLogEvents(db, events)
		return ierr
	}, wait)
	if err != nil {
		var queueFull *WriteQueueFullError
		if errors.As(err, &queueFull) {
			jsonError(w, "write queue is full", http.StatusServiceUnavailable)
			return
		}
		logger.Error("log ingest write failed", "error", err)
		jsonError(w, "log ingest write failed", http.StatusInternalServerError)
		return
	}
	for _, event := range events {
		sseBroadcast("logs", map[string]any{
			"source":   "logs",
			"ts":       event.ts,
			"level":    event.level,
			"service":  event.service,
			"body":     event.body,
			"trace_id": event.traceId,
		})
	}
	count := len(events)
	telemetryRecordIngestEvents(count, "log")
	telemetryRecordIngestBatchSize(count, "log")
	jsonResponse(w, http.StatusOK, map[string]any{"accepted": count})
}

var rumAssetIdRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

func ingestRumAsset(w http.ResponseWriter, r *http.Request) {
	assetTypeParam := r.URL.Query().Get("type")
	if assetTypeParam == "" {
		assetTypeParam = "asset"
	}
	assetNameParam := r.URL.Query().Get("name")
	if assetNameParam == "" {
		assetNameParam = "asset"
	}
	assetType := sanitizeRumAssetType(assetTypeParam)
	assetName := sanitizeRumAssetName(assetNameParam)
	contentTypeHeader := r.Header.Get("Content-Type")
	if contentTypeHeader == "" {
		contentTypeHeader = "application/octet-stream"
	}
	contentType := strings.TrimSpace(strings.SplitN(contentTypeHeader, ";", 2)[0])

	body, err := readAllRequestBody(r)
	if err != nil {
		// PORT-NOTE: Python's request.get_data failure would surface as a
		// framework 500; the Go port returns an explicit JSON 500.
		jsonError(w, "failed to read request body", http.StatusInternalServerError)
		return
	}

	if len(body) == 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "asset body is required"})
		return
	}
	maxBytes := rumAssetMaxBytes
	if maxBytes < 1024 {
		maxBytes = 1024
	}
	if len(body) > maxBytes {
		jsonResponse(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "asset exceeds max allowed size"})
		return
	}

	ok, verifyErr := verifyRumAssetSignature(r, body, r.Method, r.URL.Path, contentType, assetType, assetName)
	if !ok {
		if strings.Contains(verifyErr, "not configured") {
			jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": verifyErr})
			return
		}
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": verifyErr})
		return
	}

	assetId := uuid4Hex()
	ext := assetExtension(assetName, contentType)
	storageName := assetId + "." + ext
	assetPath := filepath.Join(rumAssetDir, storageName)
	metaPath := rumAssetMetaPath(assetId)

	if err := os.WriteFile(assetPath, body, 0o644); err != nil {
		logger.Error("failed to write rum asset", "error", err)
		jsonError(w, "failed to store asset", http.StatusInternalServerError)
		return
	}

	metadata := map[string]any{
		"id":            assetId,
		"type":          assetType,
		"original_name": assetName,
		"storage_name":  storageName,
		"content_type":  contentType,
		"size":          len(body),
		"uploaded_at":   nowIso(),
	}
	metaBytes, _ := json.Marshal(metadata)
	if err := os.WriteFile(metaPath, metaBytes, 0o644); err != nil {
		logger.Error("failed to write rum asset metadata", "error", err)
		jsonError(w, "failed to store asset metadata", http.StatusInternalServerError)
		return
	}

	jsonResponse(w, http.StatusCreated, map[string]any{
		"id":          assetId,
		"type":        assetType,
		"name":        assetName,
		"contentType": contentType,
		"size":        len(body),
		// PORT-NOTE: url_for("rum_asset_download", asset_id=...) → literal path.
		"url": "/v1/rum/assets/" + assetId,
	})
}

// readAllRequestBody mirrors `await request.get_data(cache=False)`.
func readAllRequestBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}

func rumAssetDownload(w http.ResponseWriter, r *http.Request) {
	assetId := r.PathValue("asset_id")
	if !rumAssetIdRe.MatchString(assetId) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid asset id"})
		return
	}
	metaPath := rumAssetMetaPath(assetId)
	if _, err := os.Stat(metaPath); err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	metaBytes, err := os.ReadFile(metaPath)
	var metadata map[string]any
	if err == nil {
		err = json.Unmarshal(metaBytes, &metadata)
	}
	if err != nil || metadata == nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "asset metadata unavailable"})
		return
	}

	storageName := rowString(metadata["storage_name"])
	if storageName == "" || strings.Contains(storageName, "/") || strings.Contains(storageName, "\\") {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "invalid asset metadata"})
		return
	}

	filePath := filepath.Join(rumAssetDir, storageName)
	if _, err := os.Stat(filePath); err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}

	contentType := "application/octet-stream"
	if v, ok := metadata["content_type"]; ok {
		contentType = rowString(v)
	}
	// PORT-NOTE: send_from_directory(as_attachment=False) → http.ServeFile with
	// an explicit Content-Type (ServeFile honours a pre-set Content-Type).
	w.Header().Set("Content-Type", contentType)
	http.ServeFile(w, r, filePath)
}

func issueRumClientToken(w http.ResponseWriter, r *http.Request) {
	mode := strings.ToLower(strings.TrimSpace(rumClientAuthMode))
	if mode == "" || mode == "none" || mode == "off" || mode == "disabled" {
		jsonResponse(w, http.StatusOK, map[string]any{"enabled": false, "token": "", "error": "RUM client auth is disabled"})
		return
	}

	if mode != "origin" && mode != "origin-session" {
		jsonError(w, "Invalid SOBS_RUM_CLIENT_AUTH_MODE", http.StatusInternalServerError)
		return
	}

	if rumClientSigningKey == "" {
		jsonError(w, "RUM client signing key is not configured", http.StatusServiceUnavailable)
		return
	}

	payload, _ := readJsonBody(r)
	appNameRaw := payload["appName"]
	if appNameRaw == nil || rowString(appNameRaw) == "" {
		appNameRaw = payload["app"]
	}
	appName := strings.TrimSpace(rowString(appNameRaw))
	requestedOrigin := strings.TrimSpace(rowString(payload["origin"]))
	origin := normalizeOrigin(requestedOrigin)
	if origin == "" {
		origin = requestOrigin(r)
	}
	if origin == "" {
		jsonError(w, "origin is required", http.StatusBadRequest)
		return
	}

	ttlSec := rumClientTokenTtlSec
	if raw, ok := payload["ttlSec"]; ok {
		// PORT-NOTE: int(ttl_raw) — TypeError/ValueError falls back to default.
		switch v := raw.(type) {
		case json.Number:
			if n, err := v.Int64(); err == nil {
				ttlSec = int(n)
			} else if f, ferr := v.Float64(); ferr == nil {
				ttlSec = int(f)
			}
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				ttlSec = n
			}
		case float64:
			ttlSec = int(v)
		case int:
			ttlSec = v
		case bool:
			if v {
				ttlSec = 1
			} else {
				ttlSec = 0
			}
		}
	}
	if ttlSec > 24*60*60 {
		ttlSec = 24 * 60 * 60
	}
	if ttlSec < 30 {
		ttlSec = 30
	}

	now := int(time.Now().Unix())
	claims := map[string]any{
		"iss":    "sobs-rum",
		"app":    appName,
		"origin": origin,
		"iat":    now,
		"exp":    now + ttlSec,
		"jti":    uuid4Hex(),
	}
	token := rumClientTokenEncode(claims)
	jsonResponse(w, http.StatusOK, map[string]any{
		"enabled":   true,
		"token":     token,
		"expiresAt": claims["exp"],
		"origin":    origin,
		"app":       appName,
	})
}

// ---------------------------------------------------------------------------
// OTLP Ingest – Traces  POST /v1/traces
// ---------------------------------------------------------------------------

func ingestTraces(w http.ResponseWriter, r *http.Request) {
	endSpan := telemetrySpan("sobs.ingest.request", map[string]any{"route": "/v1/traces", "event.type": "trace"})
	defer endSpan()

	msg := &coltracepb.ExportTraceServiceRequest{}
	if !parseOtlpRequest(w, r, msg) {
		return
	}
	endParse := telemetrySpan("sobs.ingest.parse", map[string]any{"event.type": "trace", "parser": "otlp"})
	spanEvents, errorEvents := protoTracesToEvents(msg)
	endParse()

	wait := envFlag("SOBS_TESTING", false)

	op := func(db *ChDbConnection) error {
		if _, err := insertSpanEvents(db, spanEvents); err != nil {
			return err
		}
		return insertErrorEvents(db, errorEvents)
	}

	if err := queueWrite(op, wait); err != nil {
		var queueFull *WriteQueueFullError
		if errors.As(err, &queueFull) {
			jsonError(w, "write queue is full", http.StatusServiceUnavailable)
			return
		}
		logger.Error("trace ingest write failed", "error", err)
		jsonError(w, "trace ingest write failed", http.StatusInternalServerError)
		return
	}
	for _, event := range spanEvents {
		sseBroadcast("traces", map[string]any{
			"source":      "traces",
			"ts":          event.ts,
			"trace_id":    event.traceId,
			"span_id":     event.spanId,
			"name":        event.name,
			"service":     event.service,
			"duration_ms": event.durationMs,
			"status":      event.status,
		})
		// Also broadcast as an AI event when the span carries GenAI attributes
		var provider any = event.attrs["gen_ai.provider.name"]
		if provider == nil || rowString(provider) == "" {
			provider = event.attrs["gen_ai.system"]
			if provider == nil {
				provider = ""
			}
		}
		operationName := rowString(event.attrs["gen_ai.operation.name"])
		if rowString(provider) != "" || operationName != "" {
			sseBroadcast("ai", map[string]any{
				"source":      "ai",
				"ts":          event.ts,
				"trace_id":    event.traceId,
				"span_id":     event.spanId,
				"service":     event.service,
				"provider":    provider,
				"model":       rowString(event.attrs["gen_ai.request.model"]),
				"operation":   rowString(event.attrs["gen_ai.operation.name"]),
				"duration_ms": event.durationMs,
				"status":      event.status,
			})
		}
	}
	count := len(spanEvents)
	telemetryRecordIngestEvents(count, "trace")
	telemetryRecordIngestBatchSize(count, "trace")
	jsonResponse(w, http.StatusOK, map[string]any{"accepted": count})
}

// ---------------------------------------------------------------------------
// OTLP Ingest – Metrics  POST /v1/metrics
// ---------------------------------------------------------------------------

func ingestMetrics(w http.ResponseWriter, r *http.Request) {
	endSpan := telemetrySpan("sobs.ingest.request", map[string]any{"route": "/v1/metrics", "event.type": "metric"})
	defer endSpan()

	msg := &colmetricspb.ExportMetricsServiceRequest{}
	if !parseOtlpRequest(w, r, msg) {
		return
	}
	endParse := telemetrySpan("sobs.ingest.parse", map[string]any{"event.type": "metric", "parser": "otlp"})
	events, convErr := protoMetricsToEvents(msg)
	endParse()
	if convErr != nil {
		logger.Error("failed to convert metrics protobuf to events", "error", convErr)
		jsonError(w, "failed to convert metrics protobuf to events", http.StatusInternalServerError)
		return
	}
	wait := envFlag("SOBS_TESTING", false)
	err := queueWrite(func(db *ChDbConnection) error {
		_, ierr := insertMetricEvents(db, events)
		return ierr
	}, wait)
	if err != nil {
		var queueFull *WriteQueueFullError
		if errors.As(err, &queueFull) {
			jsonError(w, "write queue is full", http.StatusServiceUnavailable)
			return
		}
		logger.Error("metric ingest write failed", "error", err)
		jsonError(w, "metric ingest write failed", http.StatusInternalServerError)
		return
	}
	count := len(events)
	telemetryRecordIngestEvents(count, "metric")
	telemetryRecordIngestBatchSize(count, "metric")
	jsonResponse(w, http.StatusOK, map[string]any{"accepted": count})
}

// ---------------------------------------------------------------------------
// RUM Ingest  POST /v1/rum
// ---------------------------------------------------------------------------

var traceparentRe = regexp.MustCompile(`^[0-9a-fA-F]{2}-([0-9a-fA-F]{32})-([0-9a-fA-F]{16})-([0-9a-fA-F]{2})$`)

func extractTraceFields(event map[string]any) (string, string, int) {
	traceId := strings.ToLower(strings.TrimSpace(rowString(event["traceId"])))
	spanId := strings.ToLower(strings.TrimSpace(rowString(event["spanId"])))
	traceFlags := 0

	if rawFlags, present := event["traceFlags"]; present && rawFlags != nil && strings.TrimSpace(rowString(rawFlags)) != "" {
		switch v := rawFlags.(type) {
		case string:
			// int(str(raw_flags), 16)
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 16, 64); err == nil {
				traceFlags = int(n)
			}
		default:
			// int(raw_flags); failures fall back to 0 like the except branch.
			traceFlags = coerceInt(v)
		}
	}

	if traceId != "" && spanId != "" {
		return traceId, spanId, traceFlags
	}

	traceparent := strings.TrimSpace(rowString(event["traceparent"]))
	match := traceparentRe.FindStringSubmatch(traceparent)
	if match == nil {
		return traceId, spanId, traceFlags
	}

	parsedTraceId := strings.ToLower(match[1])
	parsedSpanId := strings.ToLower(match[2])
	parsedFlags64, _ := strconv.ParseInt(match[3], 16, 64)
	parsedFlags := int(parsedFlags64)

	if parsedTraceId == "" {
		parsedTraceId = traceId
	}
	if parsedSpanId == "" {
		parsedSpanId = spanId
	}
	return parsedTraceId, parsedSpanId, parsedFlags
}

// RUM Browser Context Delta Posting Cache
// Session ID -> { contextHash: str, fullContext: dict }
var (
	rumBrowserContextCache     = map[string]map[string]any{}
	rumBrowserContextCacheLock sync.Mutex
)

const rumBrowserContextCacheMax = 10000 // Keep recent 10k sessions

// handleBrowserContextDelta handles delta posting for browser context.
//
// If event has full browserContext, cache it.
// If event has contextUnchanged flag, retrieve from cache.
// Returns the full browser context attributes to add to LogAttributes.
func handleBrowserContextDelta(event map[string]any) map[string]string {
	sessionId := rowString(event["sessionId"])
	browserContext := event["browserContext"]
	contextHash := rowString(event["contextHash"])
	contextUnchanged := false
	switch v := event["contextUnchanged"].(type) {
	case nil:
	case bool:
		contextUnchanged = v
	case string:
		contextUnchanged = v != ""
	case json.Number:
		f, _ := v.Float64()
		contextUnchanged = f != 0
	default:
		contextUnchanged = true
	}

	if sessionId == "" || contextHash == "" {
		return map[string]string{}
	}

	// browser_context truthiness (None/{}/"" are falsy)
	bcTruthy := false
	switch v := browserContext.(type) {
	case nil:
	case map[string]any:
		bcTruthy = len(v) > 0
	case string:
		bcTruthy = v != ""
	default:
		bcTruthy = true
	}

	rumBrowserContextCacheLock.Lock()
	// If we have full context, cache it
	if bcMap, isMap := browserContext.(map[string]any); isMap && bcTruthy {
		rumBrowserContextCache[sessionId] = map[string]any{
			"contextHash": contextHash,
			"fullContext": bcMap,
		}
		// Trim cache if too large
		if len(rumBrowserContextCache) > rumBrowserContextCacheMax {
			// Remove oldest (arbitrary first items)
			toRemove := len(rumBrowserContextCache) - rumBrowserContextCacheMax
			for key := range rumBrowserContextCache {
				if toRemove <= 0 {
					break
				}
				delete(rumBrowserContextCache, key)
				toRemove--
			}
		}
	}

	// Retrieve cached context if contextUnchanged or if we received just the hash
	if contextUnchanged || (!bcTruthy && contextHash != "") {
		cached := rumBrowserContextCache[sessionId]
		if cached != nil && rowString(cached["contextHash"]) == contextHash {
			if full, isMap := cached["fullContext"].(map[string]any); isMap {
				browserContext = full
			} else {
				browserContext = map[string]any{}
			}
		}
	}
	rumBrowserContextCacheLock.Unlock()

	// Convert browser context dict to LogAttributes string map
	// Prefix with "browser." to keep organized
	attrs := map[string]string{}
	if bcMap, isMap := browserContext.(map[string]any); isMap {
		for key, value := range bcMap {
			if value != nil && value != "" {
				attrs["browser.context."+key] = rowString(value)
			}
		}
	}

	return attrs
}

func ingestRum(w http.ResponseWriter, r *http.Request) {
	// request.get_json(force=True, silent=True): the payload may be a JSON
	// object or a JSON array, so decode into `any` (numbers as json.Number).
	var payload any
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		if err := dec.Decode(&payload); err != nil {
			payload = nil
		}
		_ = r.Body.Close()
	}
	if payload == nil {
		payload = map[string]any{}
	}
	var events []any
	switch p := payload.(type) {
	case []any:
		events = p
	case map[string]any:
		if v, present := p["events"]; present {
			// PORT-NOTE: a non-list "events" value would raise in Python (500);
			// the Go port treats it as an empty batch.
			events, _ = v.([]any)
		} else {
			events = []any{p}
		}
	default:
		// PORT-NOTE: scalar JSON payloads would raise AttributeError in Python.
		events = []any{}
	}

	// Extract client IP from proxy-forwarded or direct headers
	clientIp := strings.TrimSpace(strings.SplitN(r.Header.Get("X-Forwarded-For"), ",", 2)[0])
	if clientIp == "" {
		clientIp = strings.TrimSpace(r.Header.Get("X-Real-IP"))
	}
	if clientIp == "" {
		// PORT-NOTE: request.remote_addr → host part of r.RemoteAddr.
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			clientIp = host
		} else {
			clientIp = r.RemoteAddr
		}
	}

	ok, statusCode, authErr := verifyRumClientAuth(r, events)
	if !ok {
		jsonResponse(w, statusCode, map[string]any{"error": authErr})
		return
	}

	sessionRows := []Row{}
	errorRows := []Row{}
	for _, rawEvent := range events {
		src, isMap := rawEvent.(map[string]any)
		if !isMap {
			continue
		}
		event := make(map[string]any, len(src))
		for k, v := range src {
			event[k] = v
		}
		delete(event, "clientAuthToken")
		if rowString(event["stack"]) != "" {
			event["stack"] = maybeDemangleJsStack(rowString(event["stack"]))
		}
		remapRumConsoleStacks(event)
		ts, present := event["timestamp"]
		if !present {
			ts = nowIso()
		}
		sessionId := rowString(event["sessionId"])
		eventType := "unknown"
		if v, present := event["type"]; present {
			eventType = rowString(v)
		}
		url := rowString(event["url"])
		traceId, spanId, traceFlags := extractTraceFields(event)
		attrs := stringifyAttrs(event)

		// Handle browser context delta posting (compress redundant context)
		browserContextAttrs := handleBrowserContextDelta(event)
		for k, v := range browserContextAttrs {
			attrs[k] = v
		}

		if clientIp != "" {
			attrs["client.ip"] = clientIp
		}
		severityText := "INFO"
		if eventType == "error" || eventType == "unhandledrejection" {
			severityText = "ERROR"
		}
		serviceName := "browser"
		if v, present := event["service"]; present {
			serviceName = rowString(v)
		}
		sessionRows = append(sessionRows, Row{
			"Timestamp":          ts,
			"TraceId":            traceId,
			"SpanId":             spanId,
			"TraceFlags":         traceFlags,
			"SeverityText":       severityText,
			"SeverityNumber":     severityNumber(severityText),
			"ServiceName":        serviceName,
			"Body":               safeJsonDumps(event),
			"ResourceSchemaUrl":  "",
			"ResourceAttributes": map[string]any{},
			"ScopeSchemaUrl":     "",
			"ScopeName":          "browser-rum",
			"ScopeVersion":       "",
			"ScopeAttributes":    map[string]any{},
			"LogAttributes":      attrs,
			"EventName":          eventType,
		})

		// Also index browser exceptions into otel_logs for unified error views.
		if eventType == "error" || eventType == "unhandledrejection" {
			errType := "JSError"
			if v, present := event["errorType"]; present {
				errType = rowString(v)
			}
			errAttrs := map[string]string{
				"exception.type":    errType,
				"exception.message": rowString(event["message"]),
				"url.full":          url,
				"session.id":        sessionId,
			}
			if rowString(event["stack"]) != "" {
				errAttrs["exception.stacktrace"] = rowString(event["stack"])
			}
			if rowString(event["errorSource"]) != "" {
				errAttrs["error.source"] = rowString(event["errorSource"])
			}
			page, _ := event["page"].(map[string]any)
			if page != nil {
				if rowString(page["title"]) != "" {
					errAttrs["browser.page.title"] = rowString(page["title"])
				}
				if rowString(page["viewport"]) != "" {
					errAttrs["browser.viewport"] = rowString(page["viewport"])
				}
			}
			artifact, _ := event["artifact"].(map[string]any)
			if artifact != nil {
				if rowString(artifact["type"]) != "" {
					errAttrs["artifact.type"] = rowString(artifact["type"])
				}
				if rowString(artifact["id"]) != "" {
					errAttrs["artifact.id"] = rowString(artifact["id"])
				}
				if rowString(artifact["url"]) != "" {
					errAttrs["artifact.url"] = rowString(artifact["url"])
				}
			}
			replay, _ := event["replay"].(map[string]any)
			if replay != nil {
				if rowString(replay["id"]) != "" {
					errAttrs["replay.id"] = rowString(replay["id"])
				}
				if rowString(replay["url"]) != "" {
					errAttrs["replay.url"] = rowString(replay["url"])
				}
			}
			errorRows = append(errorRows, Row{
				"Timestamp":          ts,
				"TraceId":            traceId,
				"SpanId":             spanId,
				"TraceFlags":         traceFlags,
				"SeverityText":       "ERROR",
				"SeverityNumber":     severityNumber("ERROR"),
				"ServiceName":        "rum",
				"Body":               rowString(event["message"]),
				"ResourceSchemaUrl":  "",
				"ResourceAttributes": map[string]any{},
				"ScopeSchemaUrl":     "",
				"ScopeName":          "browser-rum",
				"ScopeVersion":       "",
				"ScopeAttributes":    map[string]any{},
				"LogAttributes":      errAttrs,
				"EventName":          "exception",
			})
		}
	}
	wait := envFlag("SOBS_TESTING", false)

	op := func(db *ChDbConnection) error {
		if _, err := insertRowsJsonEachRow(db, "hyperdx_sessions", sessionRows); err != nil {
			return err
		}
		if _, err := insertRowsJsonEachRow(db, "otel_logs", errorRows); err != nil {
			return err
		}
		rememberLogAttrKeys(db, extractLogAttrMaps(errorRows), "log")
		// try/except: auto-tag application is best-effort.
		rules, err := loadTagRules(db)
		if err != nil {
			logger.Error("auto-tag application failed for rum", "error", err)
			return nil
		}
		if len(rules) > 0 {
			if err := applyTagRules(db, "rum", sessionRows, rules); err != nil {
				logger.Error("auto-tag application failed for rum", "error", err)
				return nil
			}
			if len(errorRows) > 0 {
				if err := applyTagRules(db, "error", errorRows, rules); err != nil {
					logger.Error("auto-tag application failed for rum", "error", err)
				}
			}
		}
		return nil
	}

	if err := queueWrite(op, wait); err != nil {
		var queueFull *WriteQueueFullError
		if errors.As(err, &queueFull) {
			jsonError(w, "write queue is full", http.StatusServiceUnavailable)
			return
		}
		logger.Error("rum ingest write failed", "error", err)
		jsonError(w, "rum ingest write failed", http.StatusInternalServerError)
		return
	}
	count := len(sessionRows)
	telemetryRecordIngestEvents(count, "rum")
	telemetryRecordIngestBatchSize(count, "rum")
	jsonResponse(w, http.StatusOK, map[string]any{"accepted": count})
}

// jsonTruthy mirrors Python truthiness for decoded JSON payload values
// (None/False/0/""/{} /[] are falsy).
func jsonTruthy(v any) bool {
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
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	default:
		return true
	}
}

// payloadGet mirrors Python dict.get(key, default) for JSON payload maps.
func payloadGet(payload map[string]any, key string, def any) any {
	if v, ok := payload[key]; ok {
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// AI Transparency  POST /v1/ai
// ---------------------------------------------------------------------------

func ingestAi(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	ts, present := payload["timestamp"]
	if !present {
		ts = nowIso()
	}
	model := rowString(payload["model"])
	// Canonicalize operation: default to "chat", normalise case/whitespace
	operation := rowString(payload["operation"])
	if operation == "" {
		operation = "chat"
	}
	operation = strings.TrimSpace(strings.ToLower(operation))
	// PORT-NOTE: float(payload.get("duration_ms", 0) or 0) raises (→ 500) for
	// non-numeric strings in Python; the port falls back to 0.
	durationMs, _ := coerceFloat(payload["duration_ms"])
	provider := rowString(payload["provider"])
	service := rowString(payload["service"])
	spanName := strings.TrimSpace(operation + " " + model)
	spanAttrs := map[string]any{
		"gen_ai.operation.name":      operation,
		"gen_ai.provider.name":       provider,
		"gen_ai.request.model":       model,
		"gen_ai.usage.input_tokens":  coerceInt(payload["tokens_in"]),
		"gen_ai.usage.output_tokens": coerceInt(payload["tokens_out"]),
	}
	// Standard OTel GenAI content attributes (primary)
	if raw, ok := payload["input_messages"]; ok && raw != nil {
		if s, isStr := raw.(string); isStr {
			spanAttrs["gen_ai.input.messages"] = s
		} else {
			spanAttrs["gen_ai.input.messages"] = safeJsonDumps(raw)
		}
	}
	if raw, ok := payload["output_messages"]; ok && raw != nil {
		if s, isStr := raw.(string); isStr {
			spanAttrs["gen_ai.output.messages"] = s
		} else {
			spanAttrs["gen_ai.output.messages"] = safeJsonDumps(raw)
		}
	}
	if raw, ok := payload["system_instructions"]; ok && raw != nil {
		if s, isStr := raw.(string); isStr {
			spanAttrs["gen_ai.system_instructions"] = s
		} else {
			spanAttrs["gen_ai.system_instructions"] = safeJsonDumps(raw)
		}
	}
	// Legacy sobs fields (kept for backward-compat / UI fallback)
	if jsonTruthy(payload["prompt"]) {
		spanAttrs["sobs.gen_ai.prompt"] = rowString(payload["prompt"])
	}
	if jsonTruthy(payload["response"]) {
		spanAttrs["sobs.gen_ai.response"] = rowString(payload["response"])
	}
	if jsonTruthy(payload["error_type"]) {
		spanAttrs["error.type"] = rowString(payload["error_type"])
	}
	duration := int64(durationMs * 1_000_000)
	if duration < 0 {
		duration = 0
	}
	row := Row{
		"Timestamp":          ts,
		"TraceId":            rowString(payload["trace_id"]),
		"SpanId":             rowString(payload["span_id"]),
		"ParentSpanId":       "",
		"TraceState":         "",
		"SpanName":           spanName,
		"SpanKind":           "CLIENT",
		"ServiceName":        service,
		"ResourceAttributes": map[string]any{},
		"ScopeName":          "sobs-ai",
		"ScopeVersion":       "",
		"SpanAttributes":     stringifyAttrs(spanAttrs),
		"Duration":           duration,
		"StatusCode":         "STATUS_CODE_OK",
		"StatusMessage":      "",
		"Events":             map[string]any{"Timestamp": []any{}, "Name": []any{}, "Attributes": []any{}},
		"Links":              map[string]any{"TraceId": []any{}, "SpanId": []any{}, "TraceState": []any{}, "Attributes": []any{}},
	}
	wait := envFlag("SOBS_TESTING", false)

	op := func(db *ChDbConnection) error {
		if _, err := insertRowsJsonEachRow(db, "otel_traces", []Row{row}); err != nil {
			return err
		}
		// try/except: auto-tag application is best-effort.
		rules, err := loadTagRules(db)
		if err != nil {
			logger.Error("auto-tag application failed for ai", "error", err)
			return nil
		}
		if len(rules) > 0 {
			if err := applyTagRules(db, "ai", []Row{row}, rules); err != nil {
				logger.Error("auto-tag application failed for ai", "error", err)
			}
		}
		return nil
	}

	if err := queueWrite(op, wait); err != nil {
		var queueFull *WriteQueueFullError
		if errors.As(err, &queueFull) {
			jsonError(w, "write queue is full", http.StatusServiceUnavailable)
			return
		}
		logger.Error("ai ingest write failed", "error", err)
		jsonError(w, "ai ingest write failed", http.StatusInternalServerError)
		return
	}
	sseBroadcast("ai", map[string]any{
		"source":    "ai",
		"ts":        ts,
		"service":   service,
		"provider":  provider,
		"model":     model,
		"operation": operation,
		// PORT-NOTE: Python round() uses banker's rounding; math.Round rounds
		// half away from zero (differs only on exact .x5 values).
		"duration_ms": math.Round(durationMs*10) / 10,
		"tokens_in":   spanAttrs["gen_ai.usage.input_tokens"],
		"tokens_out":  spanAttrs["gen_ai.usage.output_tokens"],
	})
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Error ingest  POST /v1/errors  (direct error submission)
// ---------------------------------------------------------------------------

func ingestErrors(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	ts, present := payload["timestamp"]
	if !present {
		ts = nowIso()
	}
	// PORT-NOTE: a non-dict "attributes" value would raise in Python (500);
	// the Go port treats it as an empty map.
	attrsSrc, _ := payload["attributes"].(map[string]any)
	if attrsSrc == nil {
		attrsSrc = map[string]any{}
	}
	attrs := stringifyAttrs(attrsSrc)
	errType := "Error"
	if v, ok := payload["type"]; ok {
		errType = rowString(v)
	}
	attrs["exception.type"] = errType
	attrs["exception.message"] = rowString(payload["message"])
	if jsonTruthy(payload["stack"]) {
		attrs["exception.stacktrace"] = maybeDemangleJsStack(rowString(payload["stack"]))
	}
	row := Row{
		"Timestamp":          ts,
		"TraceId":            rowString(payload["trace_id"]),
		"SpanId":             rowString(payload["span_id"]),
		"TraceFlags":         0,
		"SeverityText":       "ERROR",
		"SeverityNumber":     severityNumber("ERROR"),
		"ServiceName":        rowString(payload["service"]),
		"Body":               rowString(payload["message"]),
		"ResourceSchemaUrl":  "",
		"ResourceAttributes": map[string]any{},
		"ScopeSchemaUrl":     "",
		"ScopeName":          "",
		"ScopeVersion":       "",
		"ScopeAttributes":    map[string]any{},
		"LogAttributes":      attrs,
		"EventName":          "exception",
	}
	wait := envFlag("SOBS_TESTING", false)

	op := func(db *ChDbConnection) error {
		if _, err := insertRowsJsonEachRow(db, "otel_logs", []Row{row}); err != nil {
			return err
		}
		rememberLogAttrKeys(db, extractLogAttrMaps([]Row{row}), "log")
		// try/except: auto-tag application is best-effort.
		rules, err := loadTagRules(db)
		if err != nil {
			logger.Error("auto-tag application failed for direct errors", "error", err)
			return nil
		}
		if len(rules) > 0 {
			if err := applyTagRules(db, "error", []Row{row}, rules); err != nil {
				logger.Error("auto-tag application failed for direct errors", "error", err)
			}
		}
		return nil
	}

	if err := queueWrite(op, wait); err != nil {
		var queueFull *WriteQueueFullError
		if errors.As(err, &queueFull) {
			jsonError(w, "write queue is full", http.StatusServiceUnavailable)
			return
		}
		logger.Error("error ingest write failed", "error", err)
		jsonError(w, "error ingest write failed", http.StatusInternalServerError)
		return
	}
	telemetryRecordIngestEvents(1, "error")
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// App / Release / Artifact Registry APIs (Phase 1 scaffolding)
// ---------------------------------------------------------------------------
// PORT-NOTE: in Python, unexpected DB errors in these handlers propagate to a
// framework 500 page; the Go port logs and returns a JSON 500 instead.

func listApps(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	res, err := db.Execute("SELECT * FROM sobs_apps FINAL WHERE IsDeleted=0 ORDER BY Name ASC")
	if err != nil {
		logger.Error("list apps query failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	apps := []map[string]any{}
	for _, row := range res.Fetchall() {
		apps = append(apps, serializeAppRow(row))
	}
	if q != "" {
		filtered := []map[string]any{}
		for _, item := range apps {
			if strings.Contains(strings.ToLower(rowString(item["name"])), q) ||
				strings.Contains(strings.ToLower(rowString(item["slug"])), q) {
				filtered = append(filtered, item)
			}
		}
		apps = filtered
	}
	jsonResponse(w, http.StatusOK, apps)
}

func createAppRegistryEntry(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	payload, _ := readJsonBody(r)
	name := strings.TrimSpace(rowString(payload["name"]))
	if name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}

	slugBasis := strings.TrimSpace(rowString(payload["slug"]))
	if slugBasis == "" {
		slugBasis = name
	}
	slug := appSlug(slugBasis)
	existingRes, err := db.Execute(
		"SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 LIMIT 1",
		slug)
	if err != nil {
		logger.Error("create app slug lookup failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if existingRes.Fetchone() != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": "app slug already exists"})
		return
	}

	version := time.Now().UnixMilli()
	appId := strings.TrimSpace(rowString(payload["id"]))
	if appId == "" {
		appId = uuid4Hex()
	}
	enabled := 0
	if parseBool(payloadGet(payload, "enabled", true), true) {
		enabled = 1
	}
	row := Row{
		"Id":                 appId,
		"Name":               name,
		"Slug":               slug,
		"OwnerTeam":          strings.TrimSpace(rowString(payload["ownerTeam"])),
		"RepoUrl":            strings.TrimSpace(rowString(payload["repoUrl"])),
		"DefaultEnvironment": strings.TrimSpace(rowString(payload["defaultEnvironment"])),
		"Enabled":            enabled,
		"MetadataJson":       safeJsonDumps(payloadGet(payload, "metadata", map[string]any{})),
		"IsDeleted":          0,
		"Version":            version,
		"CreatedAt":          nowIso(),
		"UpdatedAt":          nowIso(),
	}
	if _, err := insertRowsJsonEachRow(db, "sobs_apps", []Row{row}); err != nil {
		logger.Error("create app insert failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusCreated, serializeAppRow(row))
}

func getAppRegistryEntry(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	appId := r.PathValue("app_id")
	row, err := findAppById(db, appId)
	if err != nil {
		logger.Error("get app lookup failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if row == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	jsonResponse(w, http.StatusOK, serializeAppRow(row))
}

func updateAppRegistryEntry(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	appId := r.PathValue("app_id")
	current, err := findAppById(db, appId)
	if err != nil {
		logger.Error("update app lookup failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if current == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}

	payload, _ := readJsonBody(r)
	name := strings.TrimSpace(rowString(payloadGet(payload, "name", current["Name"])))
	if name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}

	slugBasis := strings.TrimSpace(rowString(payloadGet(payload, "slug", current["Slug"])))
	if slugBasis == "" {
		slugBasis = name
	}
	slug := appSlug(slugBasis)
	conflictRes, err := db.Execute(
		"SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 AND Id!=? LIMIT 1",
		slug, appId)
	if err != nil {
		logger.Error("update app slug lookup failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if conflictRes.Fetchone() != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": "app slug already exists"})
		return
	}

	version := time.Now().UnixMilli()
	enabledDefault := 1
	if v, ok := current["Enabled"]; ok {
		enabledDefault = coerceInt(v)
	}
	enabled := 0
	if parseBool(payloadGet(payload, "enabled", enabledDefault), true) {
		enabled = 1
	}
	createdAt := rowString(current["CreatedAt"])
	if createdAt == "" {
		createdAt = nowIso()
	}
	row := Row{
		"Id":                 appId,
		"Name":               name,
		"Slug":               slug,
		"OwnerTeam":          strings.TrimSpace(rowString(payloadGet(payload, "ownerTeam", current["OwnerTeam"]))),
		"RepoUrl":            strings.TrimSpace(rowString(payloadGet(payload, "repoUrl", current["RepoUrl"]))),
		"DefaultEnvironment": strings.TrimSpace(rowString(payloadGet(payload, "defaultEnvironment", current["DefaultEnvironment"]))),
		"Enabled":            enabled,
		"MetadataJson": safeJsonDumps(
			payloadGet(payload, "metadata", safeJsonLoads(rowString(current["MetadataJson"]), map[string]any{}))),
		"IsDeleted": 0,
		"Version":   version,
		"CreatedAt": createdAt,
		"UpdatedAt": nowIso(),
	}
	if _, err := insertRowsJsonEachRow(db, "sobs_apps", []Row{row}); err != nil {
		logger.Error("update app insert failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, serializeAppRow(row))
}

func listAppReleases(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	appId := r.PathValue("app_id")
	appRow, err := findAppById(db, appId)
	if err != nil {
		logger.Error("list app releases lookup failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if appRow == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "app not found"})
		return
	}
	res, err := db.Execute(
		"SELECT * FROM sobs_app_releases FINAL WHERE AppId=? AND IsDeleted=0 ORDER BY ReleasedAt DESC",
		appId)
	if err != nil {
		logger.Error("list app releases query failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	rows := []map[string]any{}
	for _, row := range res.Fetchall() {
		rows = append(rows, serializeReleaseRow(row))
	}
	jsonResponse(w, http.StatusOK, rows)
}

func createAppRelease(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	appId := r.PathValue("app_id")
	appRow, err := findAppById(db, appId)
	if err != nil {
		logger.Error("create app release lookup failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if appRow == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "app not found"})
		return
	}

	payload, _ := readJsonBody(r)
	releaseVersion := strings.TrimSpace(rowString(payload["version"]))
	if releaseVersion == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "version is required"})
		return
	}

	version := time.Now().UnixMilli()
	releaseId := strings.TrimSpace(rowString(payload["id"]))
	if releaseId == "" {
		releaseId = uuid4Hex()
	}
	releasedAt := strings.TrimSpace(rowString(payload["releasedAt"]))
	if releasedAt == "" {
		releasedAt = nowIso()
	}
	row := Row{
		"Id":             releaseId,
		"AppId":          appId,
		"ReleaseVersion": releaseVersion,
		"CommitSha":      strings.TrimSpace(rowString(payload["commitSha"])),
		"BuildId":        strings.TrimSpace(rowString(payload["buildId"])),
		"Environment":    strings.TrimSpace(rowString(payload["environment"])),
		"ReleasedAt":     releasedAt,
		"MetadataJson":   safeJsonDumps(payloadGet(payload, "metadata", map[string]any{})),
		"IsDeleted":      0,
		"Version":        version,
	}
	if _, err := insertRowsJsonEachRow(db, "sobs_app_releases", []Row{row}); err != nil {
		logger.Error("create app release insert failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusCreated, serializeReleaseRow(row))
}

func getRelease(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	releaseId := r.PathValue("release_id")
	row, err := findReleaseById(db, releaseId)
	if err != nil {
		logger.Error("get release lookup failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if row == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}

	release := serializeReleaseRow(row)
	res, err := db.Execute(
		"SELECT * FROM sobs_release_artifacts FINAL WHERE ReleaseId=? AND IsDeleted=0 ORDER BY UploadedAt DESC",
		releaseId)
	if err != nil {
		logger.Error("get release artifacts query failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	artifacts := []map[string]any{}
	for _, artifactRow := range res.Fetchall() {
		artifacts = append(artifacts, serializeArtifactRow(artifactRow))
	}
	jsonResponse(w, http.StatusOK, map[string]any{"release": release, "artifacts": artifacts})
}

func listReleaseArtifacts(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	releaseId := r.PathValue("release_id")
	row, err := findReleaseById(db, releaseId)
	if err != nil {
		logger.Error("list release artifacts lookup failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if row == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "release not found"})
		return
	}
	res, err := db.Execute(
		"SELECT * FROM sobs_release_artifacts FINAL WHERE ReleaseId=? AND IsDeleted=0 ORDER BY UploadedAt DESC",
		releaseId)
	if err != nil {
		logger.Error("list release artifacts query failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	artifacts := []map[string]any{}
	for _, artifactRow := range res.Fetchall() {
		artifacts = append(artifacts, serializeArtifactRow(artifactRow))
	}
	jsonResponse(w, http.StatusOK, artifacts)
}

func createReleaseArtifactMeta(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	releaseId := r.PathValue("release_id")
	release, err := findReleaseById(db, releaseId)
	if err != nil {
		logger.Error("create release artifact lookup failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if release == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "release not found"})
		return
	}

	payload, _ := readJsonBody(r)
	artifactType := strings.TrimSpace(rowString(payload["artifactType"]))
	name := strings.TrimSpace(rowString(payload["name"]))
	if artifactType == "" || name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "artifactType and name are required"})
		return
	}

	version := time.Now().UnixMilli()
	artifactId := strings.TrimSpace(rowString(payload["id"]))
	if artifactId == "" {
		artifactId = uuid4Hex()
	}
	uploadedAt := strings.TrimSpace(rowString(payload["uploadedAt"]))
	if uploadedAt == "" {
		uploadedAt = nowIso()
	}
	row := Row{
		"Id":             artifactId,
		"ReleaseId":      releaseId,
		"ArtifactType":   artifactType,
		"Name":           name,
		"ContentType":    strings.TrimSpace(rowString(payload["contentType"])),
		"Size":           coerceInt(payload["size"]),
		"StorageRef":     strings.TrimSpace(rowString(payload["storageRef"])),
		"ChecksumSha256": strings.TrimSpace(rowString(payload["checksumSha256"])),
		"Platform":       strings.TrimSpace(rowString(payload["platform"])),
		"Architecture":   strings.TrimSpace(rowString(payload["architecture"])),
		"MetadataJson":   safeJsonDumps(payloadGet(payload, "metadata", map[string]any{})),
		"UploadedAt":     uploadedAt,
		"IsDeleted":      0,
		"Version":        version,
	}
	if _, err := insertRowsJsonEachRow(db, "sobs_release_artifacts", []Row{row}); err != nil {
		logger.Error("create release artifact insert failed", "error", err)
		jsonError(w, "internal server error", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusCreated, serializeArtifactRow(row))
}

// errorSourcesSql is the shared subquery selecting error-like records from
// both otel_logs and hyperdx_sessions (ERROR_SOURCES_SQL in Python).
const errorSourcesSql = `
SELECT
    Timestamp,
    ServiceName,
    TraceId,
    SpanId,
    toValidUTF8(Body) AS Body,
    mapApply((k, v) -> (toValidUTF8(k), toValidUTF8(v)), LogAttributes) AS LogAttributes
FROM otel_logs
WHERE EventName = 'exception'
   OR SeverityNumber >= 17
   OR SeverityText IN ('ERROR', 'CRITICAL', 'FATAL')
   OR LogAttributes['exception.type'] != ''
UNION ALL
SELECT
    Timestamp,
    ServiceName,
    TraceId,
    SpanId,
    toValidUTF8(Body) AS Body,
    mapApply((k, v) -> (toValidUTF8(k), toValidUTF8(v)), LogAttributes) AS LogAttributes
FROM hyperdx_sessions
WHERE EventName IN ('error', 'unhandledrejection', 'exception')
   OR SeverityNumber >= 17
   OR SeverityText IN ('ERROR', 'CRITICAL', 'FATAL')
   OR LogAttributes['exception.type'] != ''
`

// compactText mirrors _compact_text (Python default limit is 220).
func compactText(value string, limit int) string {
	text := strings.Join(strings.Fields(value), " ")
	if len([]rune(text)) <= limit {
		return text
	}
	clip := limit - 1
	if clip < 0 {
		clip = 0
	}
	return strings.TrimRightFunc(clipRunes(text, clip), unicode.IsSpace) + "..."
}

type prettyJsonResult struct {
	ok     bool
	pretty string
}

var (
	prettyJsonCache     = map[string]prettyJsonResult{}
	prettyJsonCacheLock sync.Mutex
)

const prettyJsonCacheMax = 4096

// tryPrettyJsonText mirrors _try_pretty_json_text: returns (true, indented
// JSON) when the value parses as a JSON object/array, else (false, "").
//
// PORT-NOTE: Python uses functools.lru_cache(maxsize=4096); the port keeps a
// bounded map and evicts arbitrary entries when full (no LRU ordering).
// PORT-NOTE: Go's encoder sorts object keys; Python preserves insertion order.
func tryPrettyJsonText(rawValue string) (bool, string) {
	prettyJsonCacheLock.Lock()
	if cached, hit := prettyJsonCache[rawValue]; hit {
		prettyJsonCacheLock.Unlock()
		return cached.ok, cached.pretty
	}
	prettyJsonCacheLock.Unlock()

	ok, pretty := computePrettyJsonText(rawValue)

	prettyJsonCacheLock.Lock()
	if len(prettyJsonCache) >= prettyJsonCacheMax {
		toRemove := len(prettyJsonCache) - prettyJsonCacheMax + 1
		for key := range prettyJsonCache {
			if toRemove <= 0 {
				break
			}
			delete(prettyJsonCache, key)
			toRemove--
		}
	}
	prettyJsonCache[rawValue] = prettyJsonResult{ok: ok, pretty: pretty}
	prettyJsonCacheLock.Unlock()
	return ok, pretty
}

func computePrettyJsonText(rawValue string) (bool, string) {
	raw := strings.TrimSpace(rawValue)
	if raw == "" || (raw[0] != '{' && raw[0] != '[') {
		return false, ""
	}
	parsed, parseErr := decodeStrictJson(raw)
	if parseErr != nil {
		return false, ""
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(parsed); err != nil {
		return false, ""
	}
	// Encode appends a trailing newline; json.dumps does not.
	return true, strings.TrimRight(buf.String(), "\n")
}

// decodeStrictJson mirrors json.loads: numbers stay precise (json.Number) and
// trailing non-whitespace content is rejected.
func decodeStrictJson(raw string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing JSON content")
	}
	return parsed, nil
}

// extractStructuredErrorSummary mirrors _extract_structured_error_summary:
// pulls a human-readable summary out of JSON-shaped error messages/bodies.
// Returns (summary, true) when the summary came from parsed JSON.
func extractStructuredErrorSummary(message string, rawBody string) (string, bool) {
	textKeys := map[string]bool{
		"message":       true,
		"error":         true,
		"error_message": true,
		"errormessage":  true,
		"detail":        true,
		"description":   true,
		"reason":        true,
		"body":          true,
		"msg":           true,
	}
	codeKeys := map[string]bool{"code": true, "status": true, "status_code": true, "error_code": true, "errorcode": true}
	typeKeys := map[string]bool{"type": true, "error_type": true, "exception": true, "name": true}

	// isinstance(value, (str, int, float, bool)) for decoded JSON values.
	isScalar := func(value any) bool {
		switch value.(type) {
		case string, bool, json.Number, float64, int:
			return true
		default:
			return false
		}
	}
	// str(value).strip() — bools render as True/False like Python.
	scalarText := func(value any) string {
		if b, isBool := value.(bool); isBool {
			if b {
				return "True"
			}
			return "False"
		}
		return strings.TrimSpace(rowString(value))
	}

	var firstScalar func(value any, keyset map[string]bool, depth int) string
	firstScalar = func(value any, keyset map[string]bool, depth int) string {
		if depth > 5 {
			return ""
		}
		switch v := value.(type) {
		case map[string]any:
			// Prefer direct matches before descending.
			// PORT-NOTE: Go map iteration order is randomized; when several
			// keys qualify, Python picks the first in insertion order while
			// the port picks an arbitrary one.
			for key, inner := range v {
				if keyset[strings.ToLower(key)] && isScalar(inner) {
					return scalarText(inner)
				}
			}
			for _, inner := range v {
				if found := firstScalar(inner, keyset, depth+1); found != "" {
					return found
				}
			}
			return ""
		case []any:
			for _, inner := range v {
				if found := firstScalar(inner, keyset, depth+1); found != "" {
					return found
				}
			}
			return ""
		default:
			if isScalar(value) {
				return scalarText(value)
			}
			return ""
		}
	}

	toSummary := func(parsed any) string {
		if list, isList := parsed.([]any); isList {
			if len(list) > 0 {
				parsed = list[0]
			} else {
				parsed = map[string]any{}
			}
		}
		obj, isMap := parsed.(map[string]any)
		if !isMap {
			return ""
		}

		messageText := firstScalar(obj, textKeys, 0)
		codeText := firstScalar(obj, codeKeys, 0)
		typeText := firstScalar(obj, typeKeys, 0)

		if messageText != "" {
			summary := messageText
			extras := []string{}
			if typeText != "" && !strings.Contains(strings.ToLower(summary), strings.ToLower(typeText)) {
				extras = append(extras, typeText)
			}
			if codeText != "" && !strings.Contains(strings.ToLower(summary), strings.ToLower(codeText)) {
				extras = append(extras, "code "+codeText)
			}
			if len(extras) > 0 {
				summary = summary + " [" + strings.Join(extras, ", ") + "]"
			}
			return compactText(summary, 220)
		}
		if typeText != "" && codeText != "" {
			return compactText(typeText+" (code "+codeText+")", 220)
		}
		if typeText != "" {
			return compactText(typeText, 220)
		}
		if codeText != "" {
			return compactText("code "+codeText, 220)
		}
		return ""
	}

	for _, candidate := range []string{message, rawBody} {
		raw := strings.TrimSpace(candidate)
		if raw == "" {
			continue
		}
		if raw[0] != '{' && raw[0] != '[' {
			continue
		}
		parsed, parseErr := decodeStrictJson(raw)
		if parseErr != nil {
			continue
		}
		if summary := toSummary(parsed); summary != "" {
			return summary, true
		}
		return compactText(safeJsonDumps(parsed), 220), true
	}

	basis := message
	if basis == "" {
		basis = rawBody
	}
	return compactText(basis, 220), false
}

// buildErrorItem mirrors _build_error_item: converts an error-source row into
// the dict consumed by the errors UI/templates.
func buildErrorItem(row Row) map[string]any {
	attrs := mapToDict(row["LogAttributes"])
	ts := rowString(row["Timestamp"])
	service := rowString(row["ServiceName"])
	errType := "Error"
	if v, ok := attrs["exception.type"]; ok {
		errType = rowString(v)
	}
	message := rowString(row["Body"])
	if v, ok := attrs["exception.message"]; ok {
		message = rowString(v)
	}
	rawBody := rowString(row["Body"])
	messageSummary, summaryFromJson := extractStructuredErrorSummary(message, rawBody)
	messageIsJson, messagePrettyJson := tryPrettyJsonText(message)
	bodyIsJson, bodyPrettyJson := tryPrettyJsonText(rawBody)
	stack := maybeDemangleJsStack(rowString(attrs["exception.stacktrace"]))
	stackIsJson, stackPrettyJson := tryPrettyJsonText(stack)
	traceId := rowString(row["TraceId"])
	spanId := rowString(row["SpanId"])
	eid := errorId(ts, service, errType, message, traceId, spanId)
	return map[string]any{
		"id":                   eid,
		"ts":                   ts,
		"service":              service,
		"err_type":             errType,
		"message":              message,
		"message_summary":      messageSummary,
		"summary_from_json":    summaryFromJson,
		"message_is_json":      messageIsJson,
		"message_pretty_json":  messagePrettyJson,
		"raw_body":             rawBody,
		"raw_body_is_json":     bodyIsJson,
		"raw_body_pretty_json": bodyPrettyJson,
		"stack":                stack,
		"stack_is_json":        stackIsJson,
		"stack_pretty_json":    stackPrettyJson,
		"trace_id":             traceId,
		"span_id":              spanId,
		"url":                  rowString(attrs["url.full"]),
		"error_source":         rowString(attrs["error.source"]),
		"page_title":           rowString(attrs["browser.page.title"]),
		"viewport":             rowString(attrs["browser.viewport"]),
		"artifact_type":        rowString(attrs["artifact.type"]),
		"artifact_id":          rowString(attrs["artifact.id"]),
		"artifact_url":         rowString(attrs["artifact.url"]),
		"replay_id":            rowString(attrs["replay.id"]),
		"replay_url":           rowString(attrs["replay.url"]),
	}
}

// errorGroupKey returns a stable grouping key used to fan out grouped error links.
// PORT-NOTE: Python returns a 3-tuple (service, err_type, message); the port
// returns a comparable [3]string usable as a map key.
func errorGroupKey(item map[string]any) [3]string {
	service := strings.ToLower(strings.Join(strings.Fields(rowString(item["service"])), " "))
	errType := strings.ToLower(strings.Join(strings.Fields(rowString(item["err_type"])), " "))
	messageBasis := rowString(item["message_summary"])
	if messageBasis == "" {
		messageBasis = rowString(item["message"])
	}
	message := clipRunes(strings.ToLower(strings.Join(strings.Fields(messageBasis), " ")), 220)
	return [3]string{service, errType, message}
}

// parseTraceFilterValues returns normalized unique trace IDs from trace_id and
// trace_ids query params (parsed list, primary).
func parseTraceFilterValues(traceId string, rawTraceIds []string) ([]string, string) {
	iterParts := func(value string) []string {
		parts := []string{}
		for _, p := range strings.Split(value, ",") {
			if t := strings.TrimSpace(p); t != "" {
				parts = append(parts, t)
			}
		}
		return parts
	}
	contains := func(list []string, v string) bool {
		for _, item := range list {
			if item == v {
				return true
			}
		}
		return false
	}

	parsed := []string{}
	for _, rawValue := range rawTraceIds {
		for _, part := range iterParts(rawValue) {
			norm := strings.ToLower(part)
			if norm != "" && !contains(parsed, norm) {
				parsed = append(parsed, norm)
			}
		}
	}

	// Each trace_id part is inserted at position 0, mirroring Python's
	// parsed.insert(0, norm) (so the last part of trace_id becomes primary).
	for _, part := range iterParts(traceId) {
		norm := strings.ToLower(part)
		if norm != "" && !contains(parsed, norm) {
			parsed = append([]string{norm}, parsed...)
		}
	}

	primary := ""
	if len(parsed) > 0 {
		primary = parsed[0]
	}
	return parsed, primary
}

// getResolvedErrorIds mirrors _get_resolved_error_ids (set[str] → map[string]bool).
// PORT-NOTE: Python lets DB errors propagate to a framework 500; the port logs
// and returns an empty set.
func getResolvedErrorIds(db *ChDbConnection) map[string]bool {
	resolved := map[string]bool{}
	res, err := db.Execute("SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId")
	if err != nil {
		logger.Error("resolved error ids query failed", "error", err)
		return resolved
	}
	for _, row := range res.Fetchall() {
		// str(r[0]) — first (only) selected column.
		resolved[rowString(row[res.Cols[0]])] = true
	}
	return resolved
}

// activePartRows mirrors _active_part_rows: total rows across active parts of
// the given table.
// PORT-NOTE: Python lets DB errors propagate; the port logs and returns 0.
func activePartRows(db *ChDbConnection, tableName string) int {
	res, err := db.Execute(
		"SELECT COALESCE(sum(rows), 0) AS c "+
			"FROM system.parts "+
			"WHERE active = 1 AND database = currentDatabase() AND table = ?",
		tableName)
	if err != nil {
		logger.Error("active part rows query failed", "error", err)
		return 0
	}
	row := res.Fetchone()
	if row == nil {
		return 0
	}
	return coerceInt(row["c"])
}
