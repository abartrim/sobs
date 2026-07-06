// s06_sse_auth_util.go — port of app.py lines 7515-9650.
//
// Contents: SSE tail pub/sub, compression helpers, external auth +
// require_api_key / require_basic_auth middleware, utility helpers
// (_now_iso, _ns_to_iso, sourcemap demangling, parse helpers, GenAI message
// helpers, AI trace turn cards), the internal write-table allowlist plus
// _insert_rows_json_each_row, app/release registry helpers, OTLP proto →
// event conversion and insert helpers, and OTLP request body decompression /
// parsing.
package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// SSE tail pub/sub
// ---------------------------------------------------------------------------

// _sse_subscribers: set[asyncio.Queue] → set of buffered channels guarded by a
// mutex. The /tail endpoint subscribes via sseSubscribe/sseUnsubscribe.
var (
	sseSubscribersLock sync.Mutex
	sseSubscribers     = map[chan map[string]any]struct{}{}
)

var sseQueueMaxsize = envInt("SOBS_SSE_QUEUE_MAX", 200)

// sseSubscribe registers a new SSE subscriber queue (asyncio.Queue analogue).
func sseSubscribe() chan map[string]any {
	q := make(chan map[string]any, max(1, sseQueueMaxsize))
	sseSubscribersLock.Lock()
	sseSubscribers[q] = struct{}{}
	sseSubscribersLock.Unlock()
	return q
}

// sseUnsubscribe removes a subscriber queue (set.discard analogue).
func sseUnsubscribe(q chan map[string]any) {
	sseSubscribersLock.Lock()
	delete(sseSubscribers, q)
	sseSubscribersLock.Unlock()
}

// sseBroadcast mirrors _sse_broadcast: deliver an event to every active SSE
// subscriber (non-blocking, drops on full).
//
// PORT-NOTE: the pinned Core API signature is (event string, payload any)
// while Python takes a single event dict. payload carries the Python event
// dict; when event is non-empty and the dict has no "source" key, it is
// stored under "source". Non-dict payloads are wrapped as {"data": payload}.
func sseBroadcast(event string, payload any) {
	ev, ok := payload.(map[string]any)
	if !ok {
		ev = map[string]any{"data": payload}
	}
	if event != "" {
		if _, exists := ev["source"]; !exists {
			ev["source"] = event
		}
	}
	sseSubscribersLock.Lock()
	queues := make([]chan map[string]any, 0, len(sseSubscribers))
	for q := range sseSubscribers {
		queues = append(queues, q)
	}
	sseSubscribersLock.Unlock()
	for _, q := range queues {
		select {
		case q <- ev:
		default: // asyncio.QueueFull → drop
		}
	}
}

// ---------------------------------------------------------------------------
// Compression helpers
// ---------------------------------------------------------------------------

// compress compresses text and returns it as a base64-encoded string (chDB-safe).
func compress(text string) string {
	var buf bytes.Buffer
	zw, err := zlib.NewWriterLevel(&buf, 9)
	if err != nil {
		// Level 9 is always valid; this cannot happen.
		zw = zlib.NewWriter(&buf)
	}
	_, _ = zw.Write([]byte(text))
	_ = zw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// decompress decompresses a base64-encoded compressed value. Returns "" for
// nil/empty. PORT-NOTE: Python raises on corrupt data; Go returns an error.
func decompress(data any) (string, error) {
	var raw []byte
	switch v := data.(type) {
	case nil:
		return "", nil
	case string:
		if v == "" {
			return "", nil
		}
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return "", err
		}
		raw = decoded
	case []byte:
		if len(v) == 0 {
			return "", nil
		}
		raw = v
	default:
		return "", fmt.Errorf("decompress: unsupported value type %T", data)
	}
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(zr)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// compressJson mirrors compress_json.
func compressJson(obj any) string {
	return compress(jsonDumpsNoEscape(obj))
}

// decompressJson mirrors decompress_json: returns {} for nil.
func decompressJson(data any) (any, error) {
	if data == nil {
		return map[string]any{}, nil
	}
	text, err := decompress(data)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// jsonDumpsNoEscape mirrors json.dumps(obj, ensure_ascii=False): compact-ish
// JSON without HTML escaping (Go's Marshal escapes <,>,& by default).
func jsonDumpsNoEscape(obj any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// ---------------------------------------------------------------------------
// Auth decorator (optional API key)
// ---------------------------------------------------------------------------

// checkExternalAuth validates a Bearer token against the configured external
// auth service.
//
// Makes a POST to {EXTERNAL_AUTH_URL}/internal/auth/validate forwarding the
// Authorization header. Returns true only on an HTTP 200 reply.
func checkExternalAuth(authorization string) bool {
	if externalAuthUrl == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		strings.TrimRight(externalAuthUrl, "/")+"/internal/auth/validate", nil,
	)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", authorization)
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == 200
}

// authMode returns the auth mode: none, basic, external, or invalid.
func authMode() string {
	hasUser := basicAuthUsername != ""
	hasPass := basicAuthPassword != ""
	hasExternal := externalAuthUrl != ""

	// Configuration is exclusive: use at most one auth type.
	if hasExternal && (hasUser || hasPass) {
		return "invalid"
	}
	// Basic auth requires both username and password.
	if hasUser != hasPass {
		return "invalid"
	}
	if hasExternal {
		return "external"
	}
	if hasUser && hasPass {
		return "basic"
	}
	return "none"
}

// resolveManagedCiTargetAppId mirrors _resolve_managed_ci_target_app_id.
// kwargs carries the route path params (app_id / release_id).
func resolveManagedCiTargetAppId(db *ChDbConnection, kwargs map[string]any) (string, error) {
	appId := strings.TrimSpace(rowString(kwargs["app_id"]))
	if appId != "" {
		return appId, nil
	}

	releaseId := strings.TrimSpace(rowString(kwargs["release_id"]))
	if releaseId == "" {
		return "", nil
	}

	release, err := findReleaseById(db, releaseId)
	if err != nil {
		return "", err
	}
	if release == nil {
		return "", nil
	}
	return strings.TrimSpace(rowString(release["AppId"])), nil
}

// requireApiKey mirrors the require_api_key decorator.
//
// PORT-NOTE: the Python decorator inspects route kwargs for app_id/release_id;
// the Go port reads them from the request path values, which is equivalent for
// every decorated route.
func requireApiKey(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("X-API-Key"))
		staticOk := apiKey != "" && key == apiKey

		managedConfigured := false
		managedOk := false
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					managedConfigured = false
					managedOk = false
				}
			}()
			db := getDb()
			kwargs := map[string]any{
				"app_id":     r.PathValue("app_id"),
				"release_id": r.PathValue("release_id"),
			}
			targetAppId, err := resolveManagedCiTargetAppId(db, kwargs)
			if err != nil {
				return
			}
			if targetAppId != "" {
				managed := ciPushApiKeyStatus(db, targetAppId)
				configured, _ := managed["configured"].(bool)
				managedConfigured = configured
				if managedConfigured {
					managedOk = isValidCiPushApiKey(db, targetAppId, key)
				}
			}
		}()

		if apiKey != "" {
			if !staticOk && !managedOk {
				jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
				return
			}
		} else if managedConfigured && !managedOk {
			jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
			return
		}
		f(w, r)
	}
}

// ---------------------------------------------------------------------------
// RUM asset helpers
// ---------------------------------------------------------------------------

var (
	sanitizeRumAssetNameRe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)
	sanitizeRumAssetTypeRe = regexp.MustCompile(`[^a-z0-9._-]+`)
	assetExtensionRe       = regexp.MustCompile(`^\.[a-zA-Z0-9]{1,8}$`)
)

func sanitizeRumAssetName(value string) string {
	raw := filepath.Base(strings.TrimSpace(value))
	if raw == "" || raw == "." || raw == string(filepath.Separator) {
		return "asset"
	}
	cleaned := strings.Trim(sanitizeRumAssetNameRe.ReplaceAllString(raw, "-"), "-._")
	if cleaned == "" {
		return "asset"
	}
	return cleaned
}

func sanitizeRumAssetType(value string) string {
	raw := strings.ToLower(strings.TrimSpace(value))
	if raw == "" {
		return "asset"
	}
	cleaned := strings.Trim(sanitizeRumAssetTypeRe.ReplaceAllString(raw, "-"), "-._")
	if cleaned == "" {
		return "asset"
	}
	return cleaned
}

func assetExtension(assetName, contentType string) string {
	ext := filepath.Ext(assetName)
	if ext != "" && assetExtensionRe.MatchString(ext) {
		return strings.ToLower(strings.TrimPrefix(ext, "."))
	}
	mapping := map[string]string{
		"application/json":         "json",
		"application/octet-stream": "bin",
		"text/plain":               "txt",
		"image/png":                "png",
		"image/jpeg":               "jpg",
		"image/webp":               "webp",
		"video/webm":               "webm",
	}
	key := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if mapped, ok := mapping[key]; ok {
		return mapped
	}
	return "bin"
}

func rumAssetSignaturePayload(method, path, timestamp, bodySha256, contentType, assetType, assetName string) string {
	return strings.Join([]string{
		strings.ToUpper(method),
		path,
		timestamp,
		bodySha256,
		strings.ToLower(strings.TrimSpace(contentType)),
		strings.ToLower(strings.TrimSpace(assetType)),
		assetName,
	}, "\n")
}

func rumAssetSignature(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

// verifyRumAssetSignature mirrors _verify_rum_asset_signature; reads the
// signature headers from the request.
func verifyRumAssetSignature(r *http.Request, body []byte, method, path, contentType, assetType, assetName string) (bool, string) {
	if rumAssetSigningKey == "" {
		return false, "Asset upload signing key is not configured"
	}

	timestamp := strings.TrimSpace(r.Header.Get("X-SOBS-Asset-Timestamp"))
	signature := strings.ToLower(strings.TrimSpace(r.Header.Get("X-SOBS-Asset-Signature")))
	if timestamp == "" || signature == "" {
		return false, "Missing asset signature headers"
	}

	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false, "Invalid asset signature timestamp"
	}

	now := time.Now().Unix()
	diff := now - ts
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(max(1, rumAssetSignWindowSec)) {
		return false, "Asset signature timestamp outside allowed window"
	}

	bodySha := fmt.Sprintf("%x", sha256.Sum256(body))
	payload := rumAssetSignaturePayload(method, path, timestamp, bodySha, contentType, assetType, assetName)
	expected := rumAssetSignature(rumAssetSigningKey, payload)
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return false, "Invalid asset signature"
	}
	return true, ""
}

func rumB64UrlEncode(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

func rumB64UrlDecode(value string) ([]byte, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return []byte{}, nil
	}
	padLen := (4 - len(text)%4) % 4
	return base64.URLEncoding.DecodeString(text + strings.Repeat("=", padLen))
}

// ---------------------------------------------------------------------------
// Origin helpers
// ---------------------------------------------------------------------------

func normalizeOrigin(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
}

func requestOrigin(r *http.Request) string {
	origin := normalizeOrigin(r.Header.Get("Origin"))
	if origin != "" {
		return origin
	}
	referer := strings.TrimSpace(r.Header.Get("Referer"))
	parsed, err := url.Parse(referer)
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
	}
	return ""
}

func sameOriginRequest(r *http.Request) bool {
	origin := normalizeOrigin(r.Header.Get("Origin"))
	referer := r.Header.Get("Referer")
	refererOrigin := ""
	if referer != "" {
		parsed, err := url.Parse(strings.TrimSpace(referer))
		if err == nil && parsed.Scheme != "" && parsed.Host != "" {
			refererOrigin = strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
		}
	}

	forwardedHost := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("X-Forwarded-Host"), ",", 2)[0]))
	expectedHost := forwardedHost
	if expectedHost == "" {
		expectedHost = strings.ToLower(strings.TrimSpace(r.Host))
	}
	forwardedProto := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("X-Forwarded-Proto"), ",", 2)[0]))
	expectedScheme := forwardedProto
	if expectedScheme == "" {
		// request.scheme analogue.
		if r.TLS != nil {
			expectedScheme = "https"
		} else {
			expectedScheme = "http"
		}
	}
	expectedOrigin := ""
	if expectedHost != "" {
		expectedOrigin = expectedScheme + "://" + expectedHost
	}
	if expectedOrigin == "" {
		return false
	}
	return origin == expectedOrigin || refererOrigin == expectedOrigin
}

// ---------------------------------------------------------------------------
// RUM client auth tokens
// ---------------------------------------------------------------------------

func rumClientSign(payload string) string {
	mac := hmac.New(sha256.New, []byte(rumClientSigningKey))
	mac.Write([]byte(payload))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

// rumClientTokenEncode mirrors _rum_client_token_encode.
// PORT-NOTE: Go marshals map keys sorted while Python preserves insertion
// order; the signature covers the encoded payload, so verification still
// round-trips.
func rumClientTokenEncode(claims map[string]any) string {
	encodedPayload := rumB64UrlEncode([]byte(jsonDumpsNoEscape(claims)))
	signature := rumClientSign(encodedPayload)
	return encodedPayload + "." + signature
}

// rumClientTokenDecode mirrors _rum_client_token_decode: returns (claims, "")
// on success or (nil, error message).
func rumClientTokenDecode(token string) (map[string]any, string) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 2 {
		return nil, "Invalid RUM client token format"
	}
	payloadB64, signature := parts[0], strings.ToLower(parts[1])
	expected := rumClientSign(payloadB64)
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return nil, "Invalid RUM client token signature"
	}
	raw, err := rumB64UrlDecode(payloadB64)
	if err != nil {
		return nil, "Invalid RUM client token payload"
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil || claims == nil {
		return nil, "Invalid RUM client token payload"
	}
	return claims, ""
}

// verifyRumClientAuth mirrors _verify_rum_client_auth → (ok, status, error).
func verifyRumClientAuth(r *http.Request, events []any) (bool, int, string) {
	mode := strings.ToLower(strings.TrimSpace(rumClientAuthMode))
	if mode == "" || mode == "none" || mode == "off" || mode == "disabled" {
		return true, 200, ""
	}

	if mode != "origin" && mode != "origin-session" {
		return false, 500, "Invalid SOBS_RUM_CLIENT_AUTH_MODE"
	}

	if rumClientSigningKey == "" {
		return false, 503, "RUM client signing key is not configured"
	}

	token := strings.TrimSpace(r.Header.Get("X-SOBS-RUM-Token"))
	if token == "" {
		for _, event := range events {
			if m, ok := event.(map[string]any); ok {
				token = strings.TrimSpace(rowString(m["clientAuthToken"]))
				if token != "" {
					break
				}
			}
		}
	}
	if token == "" {
		return false, 401, "Missing RUM client auth token"
	}

	claims, errMsg := rumClientTokenDecode(token)
	if claims == nil {
		return false, 401, errMsg
	}

	now := time.Now().Unix()
	expVal, hasExp := claims["exp"]
	exp := int64(0)
	if hasExp && expVal != nil {
		f, ok := coerceFloat(expVal)
		if !ok {
			if b, isBool := expVal.(bool); isBool {
				// Python int(True)=1, int(False)=0.
				if b {
					f = 1
				}
			} else {
				return false, 401, "Invalid RUM client token expiry"
			}
		}
		exp = int64(f)
	}
	if exp <= now {
		return false, 401, "RUM client token expired"
	}

	boundOrigin := normalizeOrigin(rowString(claims["origin"]))
	reqOrigin := requestOrigin(r)
	if boundOrigin == "" {
		return false, 401, "RUM client token missing origin binding"
	}
	if reqOrigin == "" {
		return false, 401, "Missing Origin/Referer for RUM client auth"
	}
	if reqOrigin != boundOrigin {
		return false, 401, "RUM client token origin mismatch"
	}

	boundApp := strings.TrimSpace(rowString(claims["app"]))
	if boundApp != "" {
		for _, event := range events {
			m, ok := event.(map[string]any)
			if !ok {
				continue
			}
			eventApp := strings.TrimSpace(rowString(m["appName"]))
			if eventApp != "" && eventApp != boundApp {
				return false, 401, "RUM client token app mismatch"
			}
		}
	}

	return true, 200, ""
}

func rumAssetMetaPath(assetId string) string {
	return filepath.Join(rumAssetDir, assetId+".meta.json")
}

// ---------------------------------------------------------------------------
// Auth decorator (optional Basic Auth for Web UI)
// ---------------------------------------------------------------------------

// requireBasicAuth mirrors the require_basic_auth decorator.
func requireBasicAuth(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mode := authMode()
		if mode == "invalid" {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "Server auth misconfiguration"})
			return
		}
		if mode != "none" && csrfOriginCheck {
			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
				if !sameOriginRequest(r) {
					jsonResponse(w, http.StatusForbidden, map[string]any{"error": "CSRF origin check failed"})
					return
				}
			}
		}
		if mode == "none" {
			f(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		// Accept valid HTTP Basic credentials when configured.
		if mode == "basic" && strings.HasPrefix(auth, "Basic ") {
			if decoded, err := base64.StdEncoding.Strict().DecodeString(auth[6:]); err == nil {
				username, password, _ := strings.Cut(string(decoded), ":")
				userOk := subtle.ConstantTimeCompare([]byte(username), []byte(basicAuthUsername)) == 1
				passOk := subtle.ConstantTimeCompare([]byte(password), []byte(basicAuthPassword)) == 1
				if userOk && passOk {
					f(w, r)
					return
				}
			}
		}
		// Accept a Bearer token validated by the external auth service.
		// Fall back to the `session` cookie for same-origin browser requests
		// that carry no explicit Authorization header.
		if mode == "external" {
			if !strings.HasPrefix(auth, "Bearer ") {
				if c, err := r.Cookie("session"); err == nil {
					sessionCookie := c.Value
					if sessionCookie != "" && !strings.Contains(sessionCookie, "\r") && !strings.Contains(sessionCookie, "\n") {
						auth = "Bearer " + sessionCookie
					}
				}
			}
			if strings.HasPrefix(auth, "Bearer ") && checkExternalAuth(auth) {
				f(w, r)
				return
			}
		}
		// Advertise the configured auth scheme.
		wwwAuth := `Bearer realm="SOBS"`
		if mode == "basic" {
			wwwAuth = `Basic realm="SOBS"`
		}
		w.Header().Set("WWW-Authenticate", wwwAuth)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Unauthorized"))
	}
}

// ---------------------------------------------------------------------------
// Utility helpers
// ---------------------------------------------------------------------------

// nowIso mirrors _now_iso: datetime.now(timezone.utc).isoformat(timespec="milliseconds").
func nowIso() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000") + "+00:00"
}

// nsToIso converts an OpenTelemetry nanosecond timestamp to ISO-8601.
func nsToIso(nanos int64) string {
	t := time.Unix(0, nanos).UTC()
	if t.Year() < 1 || t.Year() > 9999 {
		return nowIso()
	}
	return t.Format("2006-01-02T15:04:05.000") + "+00:00"
}

// ---------------------------------------------------------------------------
// JS stack demangling via source maps
// ---------------------------------------------------------------------------

var stackFrameRe = regexp.MustCompile(
	`^(?P<prefix>.*?)` +
		`(?P<url>https?://[^\s\)]+|/[^\s\):]+\.js(?:\?[^\s\)]*)?)` +
		`(?::(?P<line>\d+))` +
		`(?::(?P<col>\d+))` +
		`(?P<suffix>.*)$`,
)

// sourceMapSegment is one decoded mapping segment (all values 0-based).
type sourceMapSegment struct {
	genCol  int
	srcIdx  int
	srcLine int
	srcCol  int
	nameIdx int
}

// sourceMapIndex is a parsed source map (replaces the Python `sourcemap` lib).
type sourceMapIndex struct {
	sources []string
	names   []string
	lines   [][]sourceMapSegment
}

type sourceMapCacheEntry struct {
	mtime time.Time
	index *sourceMapIndex
}

var (
	sourceMapCacheLock sync.Mutex
	sourceMapCache     = map[string]sourceMapCacheEntry{}
)

const sourceMapB64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

var sourceMapB64Lookup = func() [256]int8 {
	var table [256]int8
	for i := range table {
		table[i] = -1
	}
	for i := 0; i < len(sourceMapB64Chars); i++ {
		table[sourceMapB64Chars[i]] = int8(i)
	}
	return table
}()

// decodeSourceMapVlq decodes one base64 VLQ value from s starting at pos.
// Returns (value, nextPos, ok).
func decodeSourceMapVlq(s string, pos int) (int, int, bool) {
	result := 0
	shift := 0
	for pos < len(s) {
		digit := sourceMapB64Lookup[s[pos]]
		if digit < 0 {
			return 0, pos, false
		}
		pos++
		result |= int(digit&31) << shift
		if digit&32 == 0 {
			value := result >> 1
			if result&1 != 0 {
				value = -value
			}
			return value, pos, true
		}
		shift += 5
	}
	return 0, pos, false
}

// parseSourceMapJson parses a source map document into a lookup index.
// PORT-NOTE: replaces the optional Python `sourcemap` library with a minimal
// VLQ mappings decoder (sources/names/mappings only; index maps with
// "sections" are not supported and yield a parse error → lookup miss).
func parseSourceMapJson(text string) (*sourceMapIndex, error) {
	var doc struct {
		Sources    []string `json:"sources"`
		SourceRoot string   `json:"sourceRoot"`
		Names      []string `json:"names"`
		Mappings   string   `json:"mappings"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, err
	}
	if doc.Mappings == "" {
		return nil, errors.New("source map has no mappings")
	}
	sources := make([]string, len(doc.Sources))
	root := doc.SourceRoot
	if root != "" && !strings.HasSuffix(root, "/") {
		root += "/"
	}
	for i, src := range doc.Sources {
		sources[i] = root + src
	}

	idx := &sourceMapIndex{sources: sources, names: doc.Names}
	srcIdx, srcLine, srcCol, nameIdx := 0, 0, 0, 0
	for _, lineStr := range strings.Split(doc.Mappings, ";") {
		genCol := 0
		var segments []sourceMapSegment
		for _, segStr := range strings.Split(lineStr, ",") {
			if segStr == "" {
				continue
			}
			pos := 0
			deltas := make([]int, 0, 5)
			for pos < len(segStr) {
				value, next, ok := decodeSourceMapVlq(segStr, pos)
				if !ok {
					return nil, errors.New("invalid VLQ in source map mappings")
				}
				deltas = append(deltas, value)
				pos = next
			}
			if len(deltas) == 0 {
				continue
			}
			genCol += deltas[0]
			seg := sourceMapSegment{genCol: genCol, srcIdx: -1, nameIdx: -1}
			if len(deltas) >= 4 {
				srcIdx += deltas[1]
				srcLine += deltas[2]
				srcCol += deltas[3]
				seg.srcIdx = srcIdx
				seg.srcLine = srcLine
				seg.srcCol = srcCol
			}
			if len(deltas) >= 5 {
				nameIdx += deltas[4]
				seg.nameIdx = nameIdx
			}
			segments = append(segments, seg)
		}
		idx.lines = append(idx.lines, segments)
	}
	return idx, nil
}

// lookup finds the mapping segment covering (line, col) (0-based), like the
// Python sourcemap library's index.lookup.
func (idx *sourceMapIndex) lookup(line, col int) *sourceMapSegment {
	if line < 0 || line >= len(idx.lines) {
		return nil
	}
	segments := idx.lines[line]
	if len(segments) == 0 {
		return nil
	}
	// Rightmost segment with genCol <= col.
	pos := sort.Search(len(segments), func(i int) bool { return segments[i].genCol > col })
	if pos == 0 {
		return nil
	}
	seg := segments[pos-1]
	return &seg
}

// sourcemapLookupForFile mirrors _sourcemap_lookup_for_file. Returns
// (src, line, col, name, ok); ok=false replaces the Python None.
func sourcemapLookupForFile(jsUrl string, line, col int) (string, int, int, string, bool) {
	if !sourceMapEnable || sourceMapDir == "" {
		return "", 0, 0, "", false
	}
	if info, err := os.Stat(sourceMapDir); err != nil || !info.IsDir() {
		return "", 0, 0, "", false
	}

	parsed, err := url.Parse(jsUrl)
	if err != nil {
		return "", 0, 0, "", false
	}
	relPath := strings.TrimLeft(parsed.Path, "/")
	basename := filepath.Base(parsed.Path)
	if basename == "." || basename == "/" {
		basename = ""
	}
	var candidates []string
	if relPath != "" {
		candidates = append(candidates, filepath.Join(sourceMapDir, relPath+".map"))
	}
	if basename != "" {
		candidates = append(candidates, filepath.Join(sourceMapDir, basename+".map"))
		if strings.HasSuffix(basename, ".min.js") {
			candidates = append(candidates, filepath.Join(sourceMapDir, strings.ReplaceAll(basename, ".min.js", ".js.map")))
		}
		if strings.HasSuffix(basename, ".js") {
			candidates = append(candidates, filepath.Join(sourceMapDir, basename[:len(basename)-3]+".js.map"))
		}
	}

	mapPath := ""
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			mapPath = candidate
			break
		}
	}
	if mapPath == "" {
		return "", 0, 0, "", false
	}

	info, err := os.Stat(mapPath)
	if err != nil {
		return "", 0, 0, "", false
	}
	mtime := info.ModTime()

	sourceMapCacheLock.Lock()
	entry, cached := sourceMapCache[mapPath]
	sourceMapCacheLock.Unlock()
	var index *sourceMapIndex
	if cached && entry.mtime.Equal(mtime) {
		index = entry.index
	} else {
		raw, err := os.ReadFile(mapPath)
		if err != nil {
			return "", 0, 0, "", false
		}
		index, err = parseSourceMapJson(string(raw))
		if err != nil {
			return "", 0, 0, "", false
		}
		sourceMapCacheLock.Lock()
		sourceMapCache[mapPath] = sourceMapCacheEntry{mtime: mtime, index: index}
		sourceMapCacheLock.Unlock()
	}

	token := index.lookup(max(0, line-1), max(0, col-1))
	if token == nil {
		return "", 0, 0, "", false
	}

	src := ""
	if token.srcIdx >= 0 && token.srcIdx < len(index.sources) {
		src = index.sources[token.srcIdx]
	}
	name := ""
	if token.nameIdx >= 0 && token.nameIdx < len(index.names) {
		name = index.names[token.nameIdx]
	}
	return src, token.srcLine + 1, token.srcCol + 1, name, true
}

// maybeDemangleJsStack mirrors _maybe_demangle_js_stack.
func maybeDemangleJsStack(stackText string) string {
	text := stackText
	if text == "" || !sourceMapEnable {
		return text
	}

	var mappedLines []string
	for _, rawLine := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		match := stackFrameRe.FindStringSubmatch(rawLine)
		if match == nil {
			mappedLines = append(mappedLines, rawLine)
			continue
		}
		groups := map[string]string{}
		for i, name := range stackFrameRe.SubexpNames() {
			if name != "" {
				groups[name] = match[i]
			}
		}

		jsUrl := groups["url"]
		line, lineErr := strconv.Atoi(groups["line"])
		col, colErr := strconv.Atoi(groups["col"])
		if lineErr != nil || colErr != nil {
			mappedLines = append(mappedLines, rawLine)
			continue
		}

		src, srcLine, srcCol, name, ok := sourcemapLookupForFile(jsUrl, line, col)
		if !ok {
			mappedLines = append(mappedLines, rawLine)
			continue
		}

		mappedTarget := fmt.Sprintf("%s:%d:%d", jsUrl, line, col)
		if src != "" {
			mappedTarget = fmt.Sprintf("%s:%d:%d", src, srcLine, srcCol)
		}
		if name != "" {
			mappedTarget = fmt.Sprintf("%s (%s)", name, mappedTarget)
		}
		mappedLines = append(mappedLines, fmt.Sprintf("%s[mapped] %s%s", groups["prefix"], mappedTarget, groups["suffix"]))
	}

	return strings.Join(mappedLines, "\n")
}

// remapRumConsoleStacks mirrors _remap_rum_console_stacks (mutates event).
func remapRumConsoleStacks(event map[string]any) {
	breadcrumbs, ok := event["breadcrumbs"].(map[string]any)
	if !ok {
		return
	}
	consoleEntries, ok := breadcrumbs["console"].([]any)
	if !ok {
		return
	}
	for _, entryAny := range consoleEntries {
		entry, ok := entryAny.(map[string]any)
		if !ok {
			continue
		}
		stack := rowString(entry["stack"])
		if stack != "" {
			entry["stack"] = maybeDemangleJsStack(stack)
		}
	}
}

// pyTruthy mirrors Python truthiness for the dynamic values handled in this
// section (None/""/0/empty containers are falsy; everything else truthy).
func pyTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	case []map[string]any:
		return len(v) > 0
	case []string:
		return len(v) > 0
	default:
		if f, ok := coerceFloat(v); ok {
			return f != 0
		}
		return true
	}
}

// parseLimit mirrors _parse_limit(default=200).
func parseLimit(r *http.Request, def ...int) int {
	defaultLimit := 200
	if len(def) > 0 {
		defaultLimit = def[0]
	}
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return max(1, min(defaultLimit, 5000))
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return defaultLimit
	}
	return max(1, min(value, 5000))
}

// parseOffset mirrors _parse_offset.
func parseOffset(r *http.Request) int {
	raw := r.URL.Query().Get("offset")
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	return max(0, value)
}

// parseSort parses and validates sort_by / sort_dir query params.
//
// *allowed* maps URL param values to SQL column names.
// Returns (sortBy, sqlCol, sortDir) where sortDir is 'asc' or 'desc'.
func parseSort(r *http.Request, allowed map[string]string, defaultColOpt ...string) (string, string, string) {
	defaultCol := "Timestamp"
	if len(defaultColOpt) > 0 {
		defaultCol = defaultColOpt[0]
	}
	sortBy := r.URL.Query().Get("sort_by")
	if sortBy == "" {
		sortBy = defaultCol
	}
	sortDir := strings.ToLower(r.URL.Query().Get("sort_dir"))
	if sortDir == "" {
		sortDir = "desc"
	}
	if _, ok := allowed[sortBy]; !ok {
		sortBy = defaultCol
	}
	if sortDir != "asc" && sortDir != "desc" {
		sortDir = "desc"
	}
	return sortBy, allowed[sortBy], sortDir
}

// isoTimestampLayouts approximates datetime.fromisoformat for the formats the
// app produces/accepts (used by normalizeChTimestamp / parseTimeWindowArgs).
var isoTimestampLayouts = []string{
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseIsoTimestamp mirrors datetime.fromisoformat(raw.replace("Z", "+00:00")).
// PORT-NOTE: covers the ISO-8601 shapes used by the app, not every form
// Python 3.11 fromisoformat accepts.
func parseIsoTimestamp(raw string) (time.Time, error) {
	text := strings.TrimSpace(raw)
	for _, layout := range isoTimestampLayouts {
		if t, err := time.Parse(layout, text); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid isoformat string: %q", raw)
}

// parseTimeWindowArgs parses from_ts/to_ts query params and optional window_s.
func parseTimeWindowArgs(r *http.Request) (string, string, string) {
	const invalidValueMsg = "Invalid time value. Use ISO-8601, e.g. 2026-03-29T12:00:00Z"
	fromTsRaw := strings.TrimSpace(r.URL.Query().Get("from_ts"))
	toTsRaw := strings.TrimSpace(r.URL.Query().Get("to_ts"))
	windowSRaw := strings.TrimSpace(r.URL.Query().Get("window_s"))

	fromTs := ""
	toTs := ""
	if fromTsRaw != "" {
		fromTs = normalizeChTimestamp(fromTsRaw)
	}
	if toTsRaw != "" {
		toTs = normalizeChTimestamp(toTsRaw)
	}
	if fromTs != "" && toTs == "" && windowSRaw != "" {
		windowS, err := strconv.Atoi(windowSRaw)
		if err != nil {
			return "", "", invalidValueMsg
		}
		windowS = max(1, windowS)
		fromDt, err := parseIsoTimestamp(fromTs)
		if err != nil {
			return "", "", invalidValueMsg
		}
		toTs = normalizeChTimestamp(fromDt.Add(time.Duration(windowS) * time.Second))
	}
	if fromTs != "" && toTs != "" {
		fromDt, err1 := parseIsoTimestamp(fromTs)
		toDt, err2 := parseIsoTimestamp(toTs)
		if err1 != nil || err2 != nil {
			return "", "", invalidValueMsg
		}
		if !toDt.After(fromDt) {
			return "", "", "Invalid time window: to_ts must be later than from_ts"
		}
	}
	return fromTs, toTs, ""
}

// timeWindowConditions builds time-window WHERE fragments for ClickHouse
// DateTime64 columns.
// PORT-NOTE: params is []any (not []string) to match the query-param slices
// used by the Go query builders.
func timeWindowConditions(column, fromTs, toTs string) ([]string, []any) {
	conditions := []string{}
	params := []any{}
	if fromTs != "" {
		conditions = append(conditions, fmt.Sprintf("%s >= parseDateTime64BestEffort(?, 9)", column))
		params = append(params, fromTs)
	}
	if toTs != "" {
		conditions = append(conditions, fmt.Sprintf("%s < parseDateTime64BestEffort(?, 9)", column))
		params = append(params, toTs)
	}
	return conditions, params
}

// rumSessionKeySql mirrors _RUM_SESSION_KEY_SQL.
const rumSessionKeySql = "if(LogAttributes['sessionId'] != '', LogAttributes['sessionId'], " +
	"if(LogAttributes['session.id'] != '', LogAttributes['session.id'], " +
	"concat('anon:', substring(lower(hex(MD5(concat(toString(Timestamp), '|', Body)))), 1, 16))))"

// rumSessionKeyFromAttrs mirrors _rum_session_key_from_attrs.
func rumSessionKeyFromAttrs(attrs map[string]any, ts, bodyRaw string) string {
	v, ok := attrs["sessionId"]
	if !ok {
		v = attrs["session.id"]
	}
	sessionId := strings.TrimSpace(rowString(v))
	if sessionId != "" {
		return sessionId
	}
	sum := md5.Sum([]byte(ts + "|" + bodyRaw))
	return "anon:" + fmt.Sprintf("%x", sum)[:16]
}

// buildRumEventItem mirrors _build_rum_event_item.
func buildRumEventItem(row Row) map[string]any {
	attrs := mapToDict(row["LogAttributes"])
	bodyRaw := rowString(row["Body"])
	var bodyData any = map[string]any{}
	if bodyRaw != "" {
		var parsed any
		if err := json.Unmarshal([]byte(bodyRaw), &parsed); err == nil {
			bodyData = parsed
		} else {
			bodyData = map[string]any{}
		}
	}

	data, isDict := bodyData.(map[string]any)
	if !isDict {
		data = map[string]any{"value": bodyData}
	}
	traceId := ""
	if v, present := row["TraceId"]; present {
		traceId = rowString(v)
	} else {
		traceId = rowString(data["traceId"])
	}
	spanId := ""
	if v, present := row["SpanId"]; present {
		spanId = rowString(v)
	} else {
		spanId = rowString(data["spanId"])
	}
	service := ""
	if v, present := row["ServiceName"]; present {
		service = rowString(v)
	} else {
		service = rowString(data["service"])
	}
	if traceId != "" && !pyTruthy(data["traceId"]) {
		data["traceId"] = traceId
	}
	if spanId != "" && !pyTruthy(data["spanId"]) {
		data["spanId"] = spanId
	}

	ts := rowString(row["Timestamp"])
	sessionKey := rumSessionKeyFromAttrs(attrs, ts, bodyRaw)
	artifact := map[string]any{}
	if m, ok := data["artifact"].(map[string]any); ok {
		artifact = m
	}
	replay := map[string]any{}
	if m, ok := data["replay"].(map[string]any); ok {
		replay = m
	}
	urlVal, ok := attrs["url"]
	if !ok {
		urlVal = attrs["url.full"]
	}
	return map[string]any{
		"ts":           ts,
		"session_key":  sessionKey,
		"session_id":   clipRunes(sessionKey, 8),
		"event_type":   rowString(row["EventName"]),
		"url":          rowString(urlVal),
		"data":         data,
		"trace_id":     traceId,
		"span_id":      spanId,
		"service":      service,
		"has_artifact": pyTruthy(artifact["url"]) || pyTruthy(artifact["id"]),
		"has_replay":   pyTruthy(replay["url"]) || pyTruthy(replay["id"]),
	}
}

// hex mirrors _hex: convert bytes or hex string to hex string.
// PORT-NOTE: this file must not import encoding/hex (the package-level name
// is shadowed by that import in files that use it).
func pyHexStr(b any) string {
	switch v := b.(type) {
	case []byte:
		return fmt.Sprintf("%x", v)
	default:
		if !pyTruthy(b) {
			return ""
		}
		return rowString(b)
	}
}

// pyScalarStr mirrors Python str(value) for scalar attribute values.
// PORT-NOTE: floats keep a trailing ".0" like Python's str(float).
func pyScalarStr(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case json.Number:
		return v.String()
	case float64, float32:
		f, _ := coerceFloat(v)
		s := strconv.FormatFloat(f, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") && !strings.Contains(s, "Inf") && !strings.Contains(s, "NaN") {
			s += ".0"
		}
		return s
	default:
		return fmt.Sprintf("%v", v)
	}
}

// stringifyAttrs converts arbitrary attribute values to a string map suitable
// for OTel Map columns.
func stringifyAttrs(values map[string]any) map[string]string {
	out := map[string]string{}
	if len(values) == 0 {
		return out
	}
	for key, value := range values {
		if value == nil {
			continue
		}
		switch value.(type) {
		case string, bool, int, int32, int64, uint64, float32, float64, json.Number:
			out[key] = pyScalarStr(value)
		default:
			out[key] = jsonDumpsNoEscape(value)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// GenAI message helpers
// ---------------------------------------------------------------------------

// genaiToolCallsToText mirrors _genai_tool_calls_to_text.
func genaiToolCallsToText(toolCallsValue any) string {
	items, ok := toolCallsValue.([]any)
	if !ok {
		return ""
	}
	var chunks []string
	for _, itemAny := range items {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		function := map[string]any{}
		if m, ok := item["function"].(map[string]any); ok {
			function = m
		}
		name := strings.TrimSpace(rowString(item["name"]))
		if name == "" {
			name = strings.TrimSpace(rowString(function["name"]))
		}
		arguments := item["arguments"]
		if !pyTruthy(arguments) { // arguments in (None, "", [], {})
			// PORT-NOTE: Python's membership test only swaps for None/""/[]/{};
			// other falsy values (0, False) cannot appear in tool-call args.
			arguments = function["arguments"]
		}
		label := "tool_call"
		if name != "" {
			label = "tool_call:" + name
		}
		switch a := arguments.(type) {
		case map[string]any:
			if len(a) > 0 {
				chunks = append(chunks, label+" "+jsonDumpsNoEscape(a))
			} else {
				chunks = append(chunks, label+" {}")
			}
		case []any:
			if len(a) > 0 {
				chunks = append(chunks, label+" "+jsonDumpsNoEscape(a))
			} else {
				chunks = append(chunks, label+" []")
			}
		case nil:
			chunks = append(chunks, label)
		case string:
			if a != "" {
				chunks = append(chunks, label+" "+a)
			} else {
				chunks = append(chunks, label)
			}
		default:
			chunks = append(chunks, label+" "+pyScalarStr(arguments))
		}
	}
	return strings.TrimSpace(strings.Join(chunks, "\n"))
}

// genaiMessageContentToText mirrors _genai_message_content_to_text.
func genaiMessageContentToText(message map[string]any) string {
	content := message["content"]
	if s, ok := content.(string); ok {
		return s
	}
	if list, ok := content.([]any); ok {
		var parts []string
		for _, partAny := range list {
			if part, ok := partAny.(map[string]any); ok {
				parts = append(parts, rowString(part["text"]))
			} else {
				parts = append(parts, rowString(partAny))
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	if content != nil && content != "" {
		return pyScalarStr(content)
	}

	if partsValue, ok := message["parts"].([]any); ok {
		var chunks []string
		for _, partAny := range partsValue {
			if s, ok := partAny.(string); ok {
				if s != "" {
					chunks = append(chunks, s)
				}
				continue
			}
			part, ok := partAny.(map[string]any)
			if !ok {
				continue
			}
			partType := strings.ToLower(strings.TrimSpace(rowString(part["type"])))
			if partType == "text" || partType == "reasoning" {
				text := part["content"]
				if !pyTruthy(text) {
					text = part["text"]
				}
				if pyTruthy(text) {
					chunks = append(chunks, rowString(text))
				}
				continue
			}
			if partType == "tool_call" || partType == "server_tool_call" {
				rendered := genaiToolCallsToText([]any{partAny})
				if rendered != "" {
					chunks = append(chunks, rendered)
				}
				continue
			}
			if partType == "tool_call_response" || partType == "server_tool_call_response" {
				response := part["response"]
				if pyTruthy(response) {
					chunks = append(chunks, rowString(response))
				} else {
					chunks = append(chunks, partType)
				}
				continue
			}
			partContent := part["content"]
			if pyTruthy(partContent) {
				chunks = append(chunks, rowString(partContent))
				continue
			}
			chunks = append(chunks, jsonDumpsNoEscape(part))
		}
		renderedParts := strings.TrimSpace(strings.Join(chunks, "\n"))
		if renderedParts != "" {
			return renderedParts
		}
	}

	toolCallsText := genaiToolCallsToText(message["tool_calls"])
	if toolCallsText != "" {
		return toolCallsText
	}

	if functionCall, ok := message["function_call"].(map[string]any); ok {
		functionText := genaiToolCallsToText([]any{map[string]any{"function": functionCall}})
		if functionText != "" {
			return functionText
		}
	}

	return ""
}

// genaiMessageReasoningToText extracts model reasoning/thinking text when
// providers expose it separately.
func genaiMessageReasoningToText(message map[string]any) string {
	coerceReasoningText := func(value any) string {
		switch v := value.(type) {
		case nil:
			return ""
		case string:
			return strings.TrimSpace(v)
		case []any:
			var chunks []string
			for _, itemAny := range v {
				if s, ok := itemAny.(string); ok {
					if text := strings.TrimSpace(s); text != "" {
						chunks = append(chunks, text)
					}
					continue
				}
				if item, ok := itemAny.(map[string]any); ok {
					text := rowString(item["text"])
					if !pyTruthy(item["text"]) {
						text = rowString(item["content"])
					}
					if text = strings.TrimSpace(text); text != "" {
						chunks = append(chunks, text)
					}
					continue
				}
				if text := strings.TrimSpace(rowString(itemAny)); text != "" {
					chunks = append(chunks, text)
				}
			}
			return strings.TrimSpace(strings.Join(chunks, "\n"))
		case map[string]any:
			direct := rowString(v["text"])
			if !pyTruthy(v["text"]) {
				direct = rowString(v["content"])
			}
			if direct = strings.TrimSpace(direct); direct != "" {
				return direct
			}
			return jsonDumpsNoEscape(v)
		default:
			if v == "" {
				return ""
			}
			return strings.TrimSpace(pyScalarStr(v))
		}
	}

	// Common provider fields.
	for _, key := range []string{"reasoning_content", "reasoning", "thinking"} {
		if text := coerceReasoningText(message[key]); text != "" {
			return text
		}
	}

	// Semconv-style parts with explicit reasoning type.
	if partsValue, ok := message["parts"].([]any); ok {
		var reasoningChunks []string
		for _, partAny := range partsValue {
			part, ok := partAny.(map[string]any)
			if !ok {
				continue
			}
			if strings.ToLower(strings.TrimSpace(rowString(part["type"]))) != "reasoning" {
				continue
			}
			source := part["content"]
			if !pyTruthy(source) {
				source = part["text"]
			}
			if text := coerceReasoningText(source); text != "" {
				reasoningChunks = append(reasoningChunks, text)
			}
		}
		if len(reasoningChunks) > 0 {
			return strings.TrimSpace(strings.Join(reasoningChunks, "\n"))
		}
	}

	return ""
}

// parseGenaiMessagesJson mirrors _parse_genai_messages_json.
// Returns (list, true) for parseable input or ([], true) when JSON is valid
// but not list-shaped; (nil, false) replaces the Python None (invalid JSON).
func parseGenaiMessagesJson(messagesStr string) ([]any, bool) {
	if messagesStr == "" {
		return []any{}, true
	}
	var parsed any
	if err := json.Unmarshal([]byte(messagesStr), &parsed); err != nil {
		return nil, false
	}
	if list, ok := parsed.([]any); ok {
		return list, true
	}
	if obj, ok := parsed.(map[string]any); ok {
		for _, key := range []string{"messages", "input_messages", "output_messages", "items"} {
			if nested, ok := obj[key].([]any); ok {
				return nested, true
			}
		}
	}
	return []any{}, true
}

// extractMessagesText extracts readable text from gen_ai.input.messages or
// gen_ai.output.messages JSON.
//
// Accepts either a JSON array of message objects (OTel GenAI convention) or a
// plain string and returns a human-readable representation for UI display.
func extractMessagesText(messagesStr string) string {
	if messagesStr == "" {
		return ""
	}

	messages, ok := parseGenaiMessagesJson(messagesStr)
	if !ok {
		return messagesStr
	}
	var parts []string
	for _, msgAny := range messages {
		if msg, ok := msgAny.(map[string]any); ok {
			role := rowString(msg["role"])
			content := genaiMessageContentToText(msg)
			if content != "" {
				if role != "" {
					parts = append(parts, fmt.Sprintf("[%s] %s", role, content))
				} else {
					parts = append(parts, content)
				}
			}
		} else if s, ok := msgAny.(string); ok {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// normalizeGenaiMessagesForDisplay normalizes GenAI message payloads into
// role/content objects for UI rendering.
func normalizeGenaiMessagesForDisplay(messages any) []map[string]any {
	var list []any
	switch v := messages.(type) {
	case []any:
		list = v
	case []map[string]any:
		list = make([]any, 0, len(v))
		for _, m := range v {
			list = append(list, m)
		}
	default:
		return []map[string]any{}
	}

	roleLabels := map[string]string{
		"system":    "system instruction",
		"user":      "user",
		"assistant": "assistant",
		"tool":      "tool",
	}

	normalized := []map[string]any{}
	for _, messageAny := range list {
		if message, ok := messageAny.(map[string]any); ok {
			msg := make(map[string]any, len(message))
			for k, v := range message {
				msg[k] = v
			}
			role := strings.ToLower(strings.TrimSpace(rowString(msg["role"])))
			if role != "" {
				msg["role"] = role
				label, ok := roleLabels[role]
				if !ok {
					label = role
				}
				msg["role_label"] = label
			}
			content := genaiMessageContentToText(msg)
			reasoning := genaiMessageReasoningToText(msg)
			if content != "" {
				msg["content"] = content
			}
			if reasoning != "" {
				msg["thinking_content"] = reasoning
			}
			if msg["content"] == nil {
				msg["content"] = ""
			}
			normalized = append(normalized, msg)
			continue
		}

		if s, ok := messageAny.(string); ok {
			normalized = append(normalized, map[string]any{"role": "", "content": s})
			continue
		}

		normalized = append(normalized, map[string]any{"role": "", "content": jsonDumpsNoEscape(messageAny)})
	}

	return normalized
}

var dedupeWhitespaceRe = regexp.MustCompile(`\s+`)

// normalizeForDedupe mirrors _normalize_for_dedupe.
func normalizeForDedupe(value any) string {
	text := strings.ToLower(strings.TrimSpace(rowString(value)))
	if text == "" {
		return ""
	}
	return dedupeWhitespaceRe.ReplaceAllString(text, " ")
}

// dedupeSystemInputMessages mirrors _dedupe_system_input_messages.
func dedupeSystemInputMessages(inputMessages []map[string]any, systemInstructions string) ([]map[string]any, int) {
	canonicalSystem := normalizeForDedupe(systemInstructions)
	if canonicalSystem == "" {
		return inputMessages, 0
	}

	filteredMessages := []map[string]any{}
	duplicateCount := 0
	for _, msg := range inputMessages {
		role := strings.ToLower(strings.TrimSpace(rowString(msg["role"])))
		if role == "system" {
			content := normalizeForDedupe(msg["content"])
			if content != "" && content == canonicalSystem {
				duplicateCount++
				continue
			}
		}
		filteredMessages = append(filteredMessages, msg)
	}
	return filteredMessages, duplicateCount
}

// stringAttrTruthy mirrors _string_attr_truthy.
func stringAttrTruthy(value any) bool {
	switch strings.ToLower(strings.TrimSpace(rowString(value))) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}

// firstMessageContent mirrors _first_message_content.
func firstMessageContent(messages []map[string]any, roles ...string) string {
	targetRoles := map[string]bool{}
	for _, role := range roles {
		targetRoles[strings.ToLower(strings.TrimSpace(role))] = true
	}
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(rowString(message["role"])))
		if !targetRoles[role] {
			continue
		}
		content := strings.TrimSpace(rowString(message["content"]))
		if content != "" {
			return content
		}
	}
	return ""
}

// summarizeAiToolAction mirrors _summarize_ai_tool_action.
func summarizeAiToolAction(rawAction string) string {
	text := strings.TrimSpace(rawAction)
	if text == "" {
		return ""
	}
	var parsedAny any
	if err := json.Unmarshal([]byte(text), &parsedAny); err != nil {
		return clipRunes(text, 180)
	}
	parsed, ok := parsedAny.(map[string]any)
	if !ok {
		return clipRunes(text, 180)
	}
	actionType := strings.TrimSpace(rowString(parsed["type"]))
	sqlWhere := strings.TrimSpace(rowString(parsed["sql_where"]))
	targetPage := strings.TrimSpace(rowString(parsed["target_page"]))
	if sqlWhere != "" {
		label := actionType
		if label == "" {
			label = "action"
		}
		return clipRunes(fmt.Sprintf("%s: %s", label, sqlWhere), 180)
	}
	if targetPage != "" {
		label := actionType
		if label == "" {
			label = "action"
		}
		return clipRunes(fmt.Sprintf("%s -> %s", label, targetPage), 180)
	}
	return clipRunes(actionType, 180)
}

// buildAiTraceTurnCards mirrors _build_ai_trace_turn_cards.
func buildAiTraceTurnCards(spans []map[string]any) []map[string]any {
	messagesOf := func(v any) []map[string]any {
		switch m := v.(type) {
		case []map[string]any:
			return m
		case []any:
			out := make([]map[string]any, 0, len(m))
			for _, itemAny := range m {
				if item, ok := itemAny.(map[string]any); ok {
					out = append(out, item)
				}
			}
			return out
		default:
			return []map[string]any{}
		}
	}

	turns := map[string]map[string]any{}
	for _, item := range spans {
		turnId := strings.TrimSpace(rowString(item["turn_id"]))
		if turnId == "" {
			continue
		}
		turn, exists := turns[turnId]
		if !exists { // setdefault
			turn = map[string]any{
				"turn_id":           turnId,
				"chat_id":           strings.TrimSpace(rowString(item["chat_id"])),
				"model":             strings.TrimSpace(rowString(item["model"])),
				"provider":          strings.TrimSpace(rowString(item["provider"])),
				"status":            "in_progress",
				"user_message":      "",
				"assistant_message": "",
				"request_summary":   "",
				"action_summary":    "",
				"result_summary":    "",
				"guard_allowed":     nil,
				"guard_reason":      "",
				"tools":             []map[string]any{},
				"tool_count":        0,
				"tokens_in":         0,
				"tokens_out":        0,
				"thinking_tokens":   0,
				"duration_ms":       0.0,
				"started_at":        rowString(item["ts"]),
				"completed_at":      "",
				"event_names":       []string{},
				"trace_id":          strings.TrimSpace(rowString(item["trace_id"])),
			}
			turns[turnId] = turn
		}

		eventName := strings.TrimSpace(rowString(item["event_name"]))
		if eventName != "" {
			names := turn["event_names"].([]string)
			seen := false
			for _, existing := range names {
				if existing == eventName {
					seen = true
					break
				}
			}
			if !seen {
				turn["event_names"] = append(names, eventName)
			}
		}

		if rowString(turn["model"]) == "" {
			turn["model"] = strings.TrimSpace(rowString(item["model"]))
		}
		if rowString(turn["provider"]) == "" {
			turn["provider"] = strings.TrimSpace(rowString(item["provider"]))
		}
		if rowString(turn["chat_id"]) == "" {
			turn["chat_id"] = strings.TrimSpace(rowString(item["chat_id"]))
		}
		if rowString(turn["trace_id"]) == "" {
			turn["trace_id"] = strings.TrimSpace(rowString(item["trace_id"]))
		}

		ts := rowString(item["ts"])
		if ts != "" && (rowString(turn["started_at"]) == "" || ts < rowString(turn["started_at"])) {
			turn["started_at"] = ts
		}
		if ts != "" && (rowString(turn["completed_at"]) == "" || ts > rowString(turn["completed_at"])) {
			turn["completed_at"] = ts
		}

		turn["tokens_in"] = turn["tokens_in"].(int) + coerceInt(item["tokens_in"])
		turn["tokens_out"] = turn["tokens_out"].(int) + coerceInt(item["tokens_out"])
		turn["thinking_tokens"] = turn["thinking_tokens"].(int) + coerceInt(item["thinking_tokens"])
		prevDuration, _ := coerceFloat(turn["duration_ms"])
		itemDuration, _ := coerceFloat(item["duration_ms"])
		turn["duration_ms"] = math.Round((prevDuration+itemDuration)*10) / 10

		userCandidate := strings.TrimSpace(rowString(item["input_question"]))
		if userCandidate == "" {
			userCandidate = firstMessageContent(messagesOf(item["input_messages"]), "user")
		}
		if userCandidate == "" {
			userCandidate = strings.TrimSpace(rowString(item["prompt"]))
		}
		if userCandidate != "" && rowString(turn["user_message"]) == "" {
			turn["user_message"] = userCandidate
		}

		assistantCandidate := firstMessageContent(messagesOf(item["output_messages"]), "assistant")
		if assistantCandidate == "" {
			assistantCandidate = strings.TrimSpace(rowString(item["response"]))
		}
		if assistantCandidate != "" && (eventName == "turn.complete" || rowString(turn["assistant_message"]) == "") {
			turn["assistant_message"] = assistantCandidate
		}

		requestSummary := strings.TrimSpace(rowString(item["turn_summary_request"]))
		actionSummary := strings.TrimSpace(rowString(item["turn_summary_action"]))
		resultSummary := strings.TrimSpace(rowString(item["turn_summary_result"]))
		if requestSummary != "" && rowString(turn["request_summary"]) == "" {
			turn["request_summary"] = requestSummary
		}
		if actionSummary != "" && rowString(turn["action_summary"]) == "" {
			turn["action_summary"] = actionSummary
		}
		if resultSummary != "" && rowString(turn["result_summary"]) == "" {
			turn["result_summary"] = resultSummary
		}

		switch {
		case eventName == "guard.result":
			turn["guard_allowed"] = stringAttrTruthy(item["guard_allowed"])
			turn["guard_reason"] = strings.TrimSpace(rowString(item["guard_reason"]))
		case eventName == "turn.blocked":
			turn["status"] = "blocked"
			reason := rowString(item["guard_reason"])
			if !pyTruthy(item["guard_reason"]) {
				reason = rowString(item["error_message"])
			}
			turn["guard_reason"] = strings.TrimSpace(reason)
		case eventName == "turn.error":
			turn["status"] = "failed"
		case eventName == "turn.cancelled":
			turn["status"] = "cancelled"
		case eventName == "turn.complete" && rowString(turn["status"]) == "in_progress":
			turn["status"] = "completed"
		}

		if eventName == "tool.proposed" || eventName == "tool.executed" {
			toolName := strings.TrimSpace(rowString(item["tool_name"]))
			if toolName == "" {
				toolName = "propose_ui_action"
			}
			toolStatus := strings.TrimSpace(rowString(item["tool_status"]))
			if toolStatus == "" {
				if eventName == "tool.executed" {
					toolStatus = "executed"
				} else {
					toolStatus = "proposed"
				}
			}
			toolSummary := strings.TrimSpace(rowString(item["tool_summary"]))
			if toolSummary == "" {
				toolSummary = summarizeAiToolAction(rowString(item["tool_action"]))
			}
			toolKey := [4]string{
				strings.TrimSpace(rowString(item["tool_action_id"])),
				toolName,
				toolStatus,
				toolSummary,
			}
			tools := turn["tools"].([]map[string]any)
			existingKeys := map[[4]string]bool{}
			for _, existing := range tools {
				existingKeys[[4]string{
					strings.TrimSpace(rowString(existing["action_id"])),
					strings.TrimSpace(rowString(existing["name"])),
					strings.TrimSpace(rowString(existing["status"])),
					strings.TrimSpace(rowString(existing["summary"])),
				}] = true
			}
			if !existingKeys[toolKey] {
				turn["tools"] = append(tools, map[string]any{
					"name":      toolName,
					"status":    toolStatus,
					"summary":   toolSummary,
					"action_id": strings.TrimSpace(rowString(item["tool_action_id"])),
				})
			}
		}
	}

	turnCards := make([]map[string]any, 0, len(turns))
	for _, turn := range turns {
		turnCards = append(turnCards, turn)
	}
	sort.SliceStable(turnCards, func(i, j int) bool {
		si, sj := rowString(turnCards[i]["started_at"]), rowString(turnCards[j]["started_at"])
		if si != sj {
			return si < sj
		}
		return rowString(turnCards[i]["turn_id"]) < rowString(turnCards[j]["turn_id"])
	})
	for index, turn := range turnCards {
		turn["index"] = index + 1
		turn["tool_count"] = len(turn["tools"].([]map[string]any))
		if strings.TrimSpace(rowString(turn["request_summary"])) == "" {
			turn["request_summary"] = strings.TrimSpace(rowString(turn["user_message"]))
		}
		if strings.TrimSpace(rowString(turn["result_summary"])) == "" {
			turn["result_summary"] = strings.TrimSpace(rowString(turn["assistant_message"]))
		}
	}
	return turnCards
}

// mapToDict performs a best-effort conversion of ClickHouse Map values to maps.
// PORT-NOTE: the Python ast.literal_eval fallback (Python dict reprs) is not
// replicated; chDB JSONEachRow output always yields JSON-compatible values.
func mapToDict(value any) map[string]any {
	switch v := value.(type) {
	case map[string]any:
		return v
	case map[string]string:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = item
		}
		return out
	case nil:
		return map[string]any{}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return map[string]any{}
		}
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			if m, ok := parsed.(map[string]any); ok {
				return m
			}
			return map[string]any{}
		}
		return map[string]any{}
	default:
		return map[string]any{}
	}
}

// severityNumber mirrors _severity_number.
func severityNumber(level string) int {
	switch strings.ToUpper(level) {
	case "TRACE":
		return 1
	case "DEBUG":
		return 5
	case "INFO":
		return 9
	case "WARN", "WARNING":
		return 13
	case "ERROR":
		return 17
	case "CRITICAL", "FATAL":
		return 21
	case "METRIC":
		return 9
	}
	return 9
}

// traceStatusCode mirrors _trace_status_code.
func traceStatusCode(status string) string {
	switch strings.ToUpper(status) {
	case "ERROR":
		return "STATUS_CODE_ERROR"
	case "OK":
		return "STATUS_CODE_OK"
	}
	return "STATUS_CODE_UNSET"
}

// errorId mirrors _error_id.
func errorId(ts, service, errType, message, traceId, spanId string) string {
	raw := strings.Join([]string{ts, service, errType, message, traceId, spanId}, "|")
	return fmt.Sprintf("%x", md5.Sum([]byte(raw)))
}

// errorIdSqlExpr returns the shared SQL expression for stable ErrorId derivation.
func errorIdSqlExpr() string {
	return "lower(hex(MD5(concat(" +
		"toString(Timestamp), '|', ServiceName, '|', " +
		"if(mapContains(LogAttributes, 'exception.type'), LogAttributes['exception.type'], 'Error'), '|', " +
		"if(mapContains(LogAttributes, 'exception.message'), LogAttributes['exception.message'], Body), '|', " +
		"TraceId, '|', SpanId" +
		"))))"
}

// PORT-NOTE: the write worker (_run_write_batch/_write_worker_main/
// _ensure_write_worker/_queue_write/_write_queue_depth, app.py 7414-7481) is
// ported in s05_agents.go (runWriteBatch/writeWorkerMain/ensureWriteWorker/
// queueWrite/writeQueueDepth); the queue/thread plumbing lives in s02_db.go.
// Only the write-table allowlist and _insert_rows_json_each_row are here.

// ---------------------------------------------------------------------------
// Internal write-table allowlist
// ---------------------------------------------------------------------------

// writableTables is the complete set of table names that SOBS may write to via
// insertRowsJsonEachRow. This prevents inadvertent writes to unintended tables
// if the tableName argument were ever derived from an unexpected source, and
// makes the write surface explicit and auditable.
var writableTables = map[string]bool{
	// OTEL/observability ingest tables
	"otel_logs":                     true,
	"otel_traces":                   true,
	"otel_metrics_gauge":            true,
	"otel_metrics_sum":              true,
	"otel_metrics_histogram":        true,
	"otel_metrics_gauge_pinned":     true,
	"otel_metrics_sum_pinned":       true,
	"otel_metrics_histogram_pinned": true,
	"hyperdx_sessions":              true,
	// SOBS internal state tables
	"sobs_ai_memories":           true,
	"sobs_ai_settings":           true,
	"sobs_agent_rules":           true,
	"sobs_agent_runs":            true,
	"sobs_anomaly_rules":         true,
	"sobs_app_releases":          true,
	"sobs_app_settings":          true,
	"sobs_apps":                  true,
	"sobs_chart_configs":         true,
	"sobs_cve_dispositions":      true,
	"sobs_cve_findings":          true,
	"sobs_dashboards":            true,
	"sobs_github_work_items":     true,
	"sobs_log_attr_keys":         true,
	"sobs_notification_channels": true,
	"sobs_notification_log":      true,
	"sobs_notification_rules":    true,
	"sobs_raw_window_copy_state": true,
	"sobs_raw_windows":           true,
	"sobs_record_tags":           true,
	"sobs_release_artifacts":     true,
	"sobs_reports":               true,
	"sobs_tag_rules":             true,
}

// insertRowsJsonEachRowDtKeys mirrors the dt_keys set in
// _insert_rows_json_each_row.
var insertRowsJsonEachRowDtKeys = []string{
	"Timestamp",
	"TimeUnix",
	"UpdatedAt",
	"CreatedAt",
	"CompletedAt",
	"ReleasedAt",
	"UploadedAt",
	"ScannedAt",
}

// insertRowsJsonEachRow mirrors _insert_rows_json_each_row.
func insertRowsJsonEachRow(db *ChDbConnection, tableName string, rows []Row) (int, error) {
	if !writableTables[tableName] {
		return 0, fmt.Errorf(
			"Attempt to write to unregistered table '%s'. "+
				"Only tables in _WRITABLE_TABLES may be written via _insert_rows_json_each_row.",
			tableName,
		)
	}
	if len(rows) == 0 {
		return 0, nil
	}
	normalizedRows := make([]Row, 0, len(rows))
	for _, row := range rows {
		item := make(Row, len(row)) // dict(row) shallow copy
		for k, v := range row {
			item[k] = v
		}
		for _, key := range insertRowsJsonEachRowDtKeys {
			if _, present := item[key]; present {
				item[key] = normalizeChTimestamp(item[key])
			}
		}
		if events, ok := item["Events"].(map[string]any); ok {
			if rawTs, present := events["Timestamp"]; present {
				switch list := rawTs.(type) {
				case []any:
					normalized := make([]any, len(list))
					for i, v := range list {
						normalized[i] = normalizeChTimestamp(v)
					}
					events["Timestamp"] = normalized
				case []string:
					normalized := make([]any, len(list))
					for i, v := range list {
						normalized[i] = normalizeChTimestamp(v)
					}
					events["Timestamp"] = normalized
				}
			}
		}
		normalizedRows = append(normalizedRows, item)
	}
	var payload strings.Builder
	for i, row := range normalizedRows {
		if i > 0 {
			payload.WriteByte('\n')
		}
		payload.WriteString(jsonDumpsNoEscape(row))
	}
	endSpan := telemetrySpan("sobs.storage.write", map[string]any{
		"storage.engine": "chdb", "table": tableName, "row.count": len(normalizedRows),
	})
	defer endSpan()
	if _, err := db.Execute(fmt.Sprintf("INSERT INTO %s FORMAT JSONEachRow\n", tableName) + payload.String()); err != nil {
		return 0, err
	}
	return len(normalizedRows), nil
}

// normalizeChTimestamp converts common timestamp forms to ClickHouse
// DateTime64-compatible strings.
func normalizeChTimestamp(value any) string {
	var dt time.Time
	if t, ok := value.(time.Time); ok {
		dt = t.UTC()
	} else {
		raw := strings.TrimSpace(rowString(value))
		if raw == "" {
			dt = time.Now().UTC()
		} else {
			parsed, err := parseIsoTimestamp(strings.Replace(raw, "Z", "+00:00", 1))
			if err != nil {
				// Last resort: preserve value and hope ClickHouse parser accepts it.
				return strings.ReplaceAll(raw, "T", " ")
			}
			dt = parsed.UTC()
		}
	}
	return dt.Format("2006-01-02 15:04:05.000000")
}

// safeJsonDumps mirrors _safe_json_dumps.
func safeJsonDumps(value any) string {
	switch v := value.(type) {
	case nil:
		return "{}"
	case string:
		stripped := strings.TrimSpace(v)
		if stripped == "" {
			return "{}"
		}
		var parsed any
		if err := json.Unmarshal([]byte(stripped), &parsed); err != nil {
			return "{}"
		}
		return jsonDumpsNoEscape(parsed)
	case map[string]any, []any, []map[string]any, []string:
		return jsonDumpsNoEscape(v)
	}
	return "{}"
}

// safeJsonLoads mirrors _safe_json_loads: returns the parsed value only when
// its shape matches the default's (dict default → dict, list default → list).
func safeJsonLoads(value any, def any) any {
	raw := strings.TrimSpace(rowString(value))
	if raw == "" {
		return def
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return def
	}
	switch def.(type) {
	case map[string]any:
		if m, ok := parsed.(map[string]any); ok {
			return m
		}
	case []any:
		if l, ok := parsed.([]any); ok {
			return l
		}
	}
	return def
}

var appSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// appSlug mirrors _app_slug(value, fallback="app").
func appSlug(value string, fallbackOpt ...string) string {
	fallback := "app"
	if len(fallbackOpt) > 0 {
		fallback = fallbackOpt[0]
	}
	slug := strings.Trim(appSlugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(value)), "-"), "-")
	if slug == "" {
		slug = fallback
	}
	return clipRunes(slug, 80)
}

// findAppById mirrors _find_app_by_id (nil Row when missing).
func findAppById(db *ChDbConnection, appId string) (Row, error) {
	res, err := db.Execute(
		"SELECT * FROM sobs_apps FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1",
		appId,
	)
	if err != nil {
		return nil, err
	}
	return res.Fetchone(), nil
}

// findAppIdByRepoUrl mirrors _find_app_id_by_repo_url.
func findAppIdByRepoUrl(db *ChDbConnection, repoUrl string) (string, error) {
	normalizedInput := strings.TrimSpace(repoUrl)
	if normalizedInput == "" {
		return "", nil
	}
	inputOwner, inputRepo := parseGithubRepoOwnerName(normalizedInput)
	if inputOwner == "" || inputRepo == "" {
		return "", nil
	}

	res, err := db.Execute("SELECT Id, RepoUrl FROM sobs_apps FINAL WHERE IsDeleted=0")
	if err != nil {
		return "", err
	}
	for _, row := range res.Fetchall() {
		owner, repo := parseGithubRepoOwnerName(rowString(row["RepoUrl"]))
		if strings.EqualFold(owner, inputOwner) && strings.EqualFold(repo, inputRepo) {
			return rowString(row["Id"]), nil
		}
	}
	return "", nil
}

// findReleaseById mirrors _find_release_by_id (nil Row when missing).
func findReleaseById(db *ChDbConnection, releaseId string) (Row, error) {
	res, err := db.Execute(
		"SELECT * FROM sobs_app_releases FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1",
		releaseId,
	)
	if err != nil {
		return nil, err
	}
	return res.Fetchone(), nil
}

// serializeAppRow mirrors _serialize_app_row.
func serializeAppRow(row Row) map[string]any {
	enabledVal, present := row["Enabled"]
	if !present {
		enabledVal = 1
	}
	return map[string]any{
		"id":                 rowString(row["Id"]),
		"name":               rowString(row["Name"]),
		"slug":               rowString(row["Slug"]),
		"ownerTeam":          rowString(row["OwnerTeam"]),
		"repoUrl":            rowString(row["RepoUrl"]),
		"defaultEnvironment": rowString(row["DefaultEnvironment"]),
		"enabled":            coerceInt(enabledVal) != 0,
		"metadata":           safeJsonLoads(row["MetadataJson"], map[string]any{}),
		"createdAt":          rowString(row["CreatedAt"]),
		"updatedAt":          rowString(row["UpdatedAt"]),
	}
}

// serializeReleaseRow mirrors _serialize_release_row.
func serializeReleaseRow(row Row) map[string]any {
	return map[string]any{
		"id":          rowString(row["Id"]),
		"appId":       rowString(row["AppId"]),
		"version":     rowString(row["ReleaseVersion"]),
		"commitSha":   rowString(row["CommitSha"]),
		"buildId":     rowString(row["BuildId"]),
		"environment": rowString(row["Environment"]),
		"releasedAt":  rowString(row["ReleasedAt"]),
		"metadata":    safeJsonLoads(row["MetadataJson"], map[string]any{}),
	}
}

// serializeArtifactRow mirrors _serialize_artifact_row.
func serializeArtifactRow(row Row) map[string]any {
	return map[string]any{
		"id":             rowString(row["Id"]),
		"releaseId":      rowString(row["ReleaseId"]),
		"artifactType":   rowString(row["ArtifactType"]),
		"name":           rowString(row["Name"]),
		"contentType":    rowString(row["ContentType"]),
		"size":           coerceInt(row["Size"]),
		"storageRef":     rowString(row["StorageRef"]),
		"checksumSha256": rowString(row["ChecksumSha256"]),
		"platform":       rowString(row["Platform"]),
		"architecture":   rowString(row["Architecture"]),
		"metadata":       safeJsonLoads(row["MetadataJson"], map[string]any{}),
		"uploadedAt":     rowString(row["UploadedAt"]),
	}
}

// seedAppReleaseRegistryFromEnv mirrors _seed_app_release_registry_from_env.
// PORT-NOTE: s02 calls this without consuming a return value; DB errors are
// logged and abort the seed instead of propagating (Python would raise out of
// _ensure_post_schema_state).
func seedAppReleaseRegistryFromEnv(db *ChDbConnection) {
	seedRaw := readEnvOrFile(appRegistrySeedJsonEnv, appRegistrySeedJsonFileEnv)
	if seedRaw == "" {
		return
	}

	var parsed any
	if err := json.Unmarshal([]byte(seedRaw), &parsed); err != nil {
		logger.Warn(fmt.Sprintf("Failed to parse app registry seed JSON: %s", err))
		return
	}

	var appsAny any
	switch v := parsed.(type) {
	case map[string]any:
		appsAny = v["apps"]
		if appsAny == nil {
			appsAny = []any{}
		}
	case []any:
		appsAny = v
	default:
		logger.Warn("Ignoring app registry seed: expected object with 'apps' or an array")
		return
	}

	apps, ok := appsAny.([]any)
	if !ok {
		logger.Warn("Ignoring app registry seed: 'apps' must be an array")
		return
	}

	nowVersion := time.Now().UnixMilli()
	appRows := []Row{}
	releaseRows := []Row{}
	artifactRows := []Row{}

	for _, appAny := range apps {
		appItem, ok := appAny.(map[string]any)
		if !ok {
			continue
		}
		name := strings.TrimSpace(rowString(appItem["name"]))
		if name == "" {
			continue
		}

		slugInput := strings.TrimSpace(rowString(appItem["slug"]))
		if slugInput == "" {
			slugInput = name
		}
		slug := appSlug(slugInput)
		res, err := db.Execute(
			"SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 LIMIT 1",
			slug,
		)
		if err != nil {
			logger.Error("app registry seed lookup failed", "error", err)
			return
		}
		existing := res.Fetchone()
		appId := strings.TrimSpace(rowString(appItem["id"]))
		if appId == "" {
			if existing != nil {
				appId = rowString(existing[res.Cols[0]])
			} else {
				appId = uuid4Hex()
			}
		}

		enabledVal, present := appItem["enabled"]
		if !present {
			enabledVal = true
		}
		enabled := 0
		if parseBool(enabledVal, true) {
			enabled = 1
		}
		appRows = append(appRows, Row{
			"Id":                 appId,
			"Name":               name,
			"Slug":               slug,
			"OwnerTeam":          strings.TrimSpace(rowString(appItem["ownerTeam"])),
			"RepoUrl":            strings.TrimSpace(rowString(appItem["repoUrl"])),
			"DefaultEnvironment": strings.TrimSpace(rowString(appItem["defaultEnvironment"])),
			"Enabled":            enabled,
			"MetadataJson":       safeJsonDumps(metadataOrEmpty(appItem["metadata"])),
			"IsDeleted":          0,
			"Version":            nowVersion,
			"CreatedAt":          nowIso(),
			"UpdatedAt":          nowIso(),
		})

		releases, ok := appItem["releases"].([]any)
		if !ok {
			continue
		}
		for _, relAny := range releases {
			rel, ok := relAny.(map[string]any)
			if !ok {
				continue
			}
			relVersion := strings.TrimSpace(rowString(rel["version"]))
			if relVersion == "" {
				continue
			}

			relRes, err := db.Execute(
				"SELECT Id FROM sobs_app_releases FINAL "+
					"WHERE AppId=? AND ReleaseVersion=? AND CommitSha=? AND Environment=? AND IsDeleted=0 LIMIT 1",
				appId,
				relVersion,
				strings.TrimSpace(rowString(rel["commitSha"])),
				strings.TrimSpace(rowString(rel["environment"])),
			)
			if err != nil {
				logger.Error("app registry seed release lookup failed", "error", err)
				return
			}
			existingRel := relRes.Fetchone()
			relId := strings.TrimSpace(rowString(rel["id"]))
			if relId == "" {
				if existingRel != nil {
					relId = rowString(existingRel[relRes.Cols[0]])
				} else {
					relId = uuid4Hex()
				}
			}

			releasedAt := strings.TrimSpace(rowString(rel["releasedAt"]))
			if releasedAt == "" {
				releasedAt = nowIso()
			}
			releaseRows = append(releaseRows, Row{
				"Id":             relId,
				"AppId":          appId,
				"ReleaseVersion": relVersion,
				"CommitSha":      strings.TrimSpace(rowString(rel["commitSha"])),
				"BuildId":        strings.TrimSpace(rowString(rel["buildId"])),
				"Environment":    strings.TrimSpace(rowString(rel["environment"])),
				"ReleasedAt":     releasedAt,
				"MetadataJson":   safeJsonDumps(metadataOrEmpty(rel["metadata"])),
				"IsDeleted":      0,
				"Version":        nowVersion,
			})

			artifacts, ok := rel["artifacts"].([]any)
			if !ok {
				continue
			}
			for _, artAny := range artifacts {
				art, ok := artAny.(map[string]any)
				if !ok {
					continue
				}
				artifactType := strings.TrimSpace(rowString(art["artifactType"]))
				artifactName := strings.TrimSpace(rowString(art["name"]))
				if artifactType == "" || artifactName == "" {
					continue
				}

				artId := strings.TrimSpace(rowString(art["id"]))
				if artId == "" {
					artId = uuid4Hex()
				}
				uploadedAt := strings.TrimSpace(rowString(art["uploadedAt"]))
				if uploadedAt == "" {
					uploadedAt = nowIso()
				}
				artifactRows = append(artifactRows, Row{
					"Id":             artId,
					"ReleaseId":      relId,
					"ArtifactType":   artifactType,
					"Name":           artifactName,
					"ContentType":    strings.TrimSpace(rowString(art["contentType"])),
					"Size":           coerceInt(art["size"]),
					"StorageRef":     strings.TrimSpace(rowString(art["storageRef"])),
					"ChecksumSha256": strings.TrimSpace(rowString(art["checksumSha256"])),
					"Platform":       strings.TrimSpace(rowString(art["platform"])),
					"Architecture":   strings.TrimSpace(rowString(art["architecture"])),
					"MetadataJson":   safeJsonDumps(metadataOrEmpty(art["metadata"])),
					"UploadedAt":     uploadedAt,
					"IsDeleted":      0,
					"Version":        nowVersion,
				})
			}
		}
	}

	if _, err := insertRowsJsonEachRow(db, "sobs_apps", appRows); err != nil {
		logger.Error("app registry seed insert failed", "error", err)
		return
	}
	if _, err := insertRowsJsonEachRow(db, "sobs_app_releases", releaseRows); err != nil {
		logger.Error("app registry seed insert failed", "error", err)
		return
	}
	if _, err := insertRowsJsonEachRow(db, "sobs_release_artifacts", artifactRows); err != nil {
		logger.Error("app registry seed insert failed", "error", err)
	}
}

// metadataOrEmpty mirrors item.get("metadata", {}).
func metadataOrEmpty(value any) any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

// attrListToDict converts an OTLP attribute list [{key, value}] to a plain map.
func attrListToDict(attrList []any) map[string]any {
	out := map[string]any{}
	for _, itemAny := range attrList {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		key := rowString(item["key"])
		valObj, _ := item["value"].(map[string]any)
		// OTLP uses typed value wrappers
		for _, vtype := range []string{"stringValue", "intValue", "doubleValue", "boolValue", "bytesValue"} {
			if v, present := valObj[vtype]; present {
				out[key] = v
				break
			}
		}
	}
	return out
}

// protoAnyValueToPython converts an OTLP AnyValue proto object to a plain value.
func protoAnyValueToPython(val *commonpb.AnyValue) any {
	if val == nil {
		return nil
	}
	switch v := val.Value.(type) {
	case *commonpb.AnyValue_StringValue:
		return v.StringValue
	case *commonpb.AnyValue_IntValue:
		return v.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return v.DoubleValue
	case *commonpb.AnyValue_BoolValue:
		return v.BoolValue
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(v.BytesValue)
	case *commonpb.AnyValue_ArrayValue:
		out := make([]any, 0, len(v.ArrayValue.GetValues()))
		for _, item := range v.ArrayValue.GetValues() {
			out = append(out, protoAnyValueToPython(item))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		out := map[string]any{}
		for _, kv := range v.KvlistValue.GetValues() {
			out[kv.GetKey()] = protoAnyValueToPython(kv.GetValue())
		}
		return out
	}
	return nil
}

// protoKvlistToDict mirrors _proto_kvlist_to_dict.
func protoKvlistToDict(attributes []*commonpb.KeyValue) map[string]any {
	out := map[string]any{}
	for _, kv := range attributes {
		out[kv.GetKey()] = protoAnyValueToPython(kv.GetValue())
	}
	return out
}

// LogEvent mirrors the LogEvent dataclass.
type LogEvent struct {
	ts            string
	level         string
	service       string
	body          string
	attrs         map[string]any
	resourceAttrs map[string]any
	scopeAttrs    map[string]any
	traceId       string
	spanId        string
}

// SpanEvent mirrors the SpanEvent dataclass.
type SpanEvent struct {
	ts            string
	traceId       string
	spanId        string
	parentSpanId  string
	name          string
	service       string
	durationMs    float64
	status        string
	attrs         map[string]any
	resourceAttrs map[string]any
	scopeAttrs    map[string]any
}

// ErrorEvent mirrors the ErrorEvent dataclass.
type ErrorEvent struct {
	ts      string
	service string
	errType string
	message string
	stack   string
	attrs   map[string]any
	traceId string
	spanId  string
}

// MetricEvent mirrors the MetricEvent dataclass.
type MetricEvent struct {
	ts      string
	service string
	name    string
	attrs   map[string]any
}

// fingerprintSkipPrefixes: attribute key prefixes excluded from the metric
// series fingerprint (high-cardinality resource attributes that do not
// differentiate metric series).
var fingerprintSkipPrefixes = []string{"telemetry.", "process.", "os.", "runtime."}

// attrFingerprint computes a stable, low-cardinality fingerprint of data-point
// attributes.
//
// Excludes high-cardinality resource/runtime attribute prefixes and limits to
// the first 8 sorted key=value pairs to keep cardinality manageable.
// PORT-NOTE: scalar values are rendered with Python str() semantics
// (True/False, trailing .0) for cross-runtime fingerprint parity; nested
// containers use JSON instead of Python repr.
func attrFingerprint(attrs map[string]any) string {
	pairs := make([]string, 0, len(attrs))
	for k, v := range attrs {
		skip := false
		for _, p := range fingerprintSkipPrefixes {
			if strings.HasPrefix(k, p) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		switch v.(type) {
		case map[string]any, []any:
			pairs = append(pairs, k+"="+jsonDumpsNoEscape(v))
		case nil:
			pairs = append(pairs, k+"=None")
		default:
			pairs = append(pairs, k+"="+pyScalarStr(v))
		}
	}
	sort.Strings(pairs)
	if len(pairs) > 8 {
		pairs = pairs[:8]
	}
	// MD5 is used here for non-cryptographic cardinality reduction only
	// (16-hex fingerprint).
	sum := md5.Sum([]byte(strings.Join(pairs, "|")))
	return fmt.Sprintf("%x", sum)[:16]
}

// TypedMetricEvent is a single OTEL metric data point with type information
// and value extracted.
type TypedMetricEvent struct {
	ts                     string
	service                string
	metricName             string
	metricDescription      string
	metricUnit             string
	metricKind             string // 'gauge', 'sum', or 'histogram'
	value                  float64
	attrs                  map[string]any // data-point-level attributes
	attrFp                 string         // stable fingerprint for series identity
	isMonotonic            int
	aggregationTemporality int
	histogramCount         int
	histogramSum           float64
	histogramBuckets       []uint64
	histogramBounds        []float64
}

// mergeAttrMaps mirrors {**resource_attrs, **scope_attrs, **record_attrs}.
func mergeAttrMaps(maps ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// bytesHex mirrors `b.hex() if b else ""` for proto byte IDs.
func bytesHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return fmt.Sprintf("%x", b)
}

// protoLogsToEvents mirrors _proto_logs_to_events.
func protoLogsToEvents(msg *collogspb.ExportLogsServiceRequest) []LogEvent {
	events := []LogEvent{}
	for _, resourceLog := range msg.GetResourceLogs() {
		resourceAttrs := protoKvlistToDict(resourceLog.GetResource().GetAttributes())
		service := rowString(resourceAttrs["service.name"])
		for _, scopeLog := range resourceLog.GetScopeLogs() {
			scopeAttrs := protoKvlistToDict(scopeLog.GetScope().GetAttributes())
			for _, record := range scopeLog.GetLogRecords() {
				recordAttrs := protoKvlistToDict(record.GetAttributes())
				mergedAttrs := mergeAttrMaps(resourceAttrs, scopeAttrs, recordAttrs)
				bodyVal := protoAnyValueToPython(record.GetBody())
				bodyStr, isStr := bodyVal.(string)
				if !isStr {
					bodyStr = jsonDumpsNoEscape(bodyVal)
				}
				level := record.GetSeverityText()
				if level == "" {
					level = "INFO"
				}
				events = append(events, LogEvent{
					ts:            nsToIso(int64(record.GetTimeUnixNano())),
					level:         strings.ToUpper(level),
					service:       service,
					body:          bodyStr,
					attrs:         mergedAttrs,
					resourceAttrs: resourceAttrs,
					scopeAttrs:    scopeAttrs,
					traceId:       bytesHex(record.GetTraceId()),
					spanId:        bytesHex(record.GetSpanId()),
				})
			}
		}
	}
	return events
}

// protoTracesToEvents mirrors _proto_traces_to_events.
func protoTracesToEvents(msg *coltracepb.ExportTraceServiceRequest) ([]SpanEvent, []ErrorEvent) {
	spanEvents := []SpanEvent{}
	errorEvents := []ErrorEvent{}
	for _, resourceSpan := range msg.GetResourceSpans() {
		resourceAttrs := protoKvlistToDict(resourceSpan.GetResource().GetAttributes())
		service := rowString(resourceAttrs["service.name"])
		for _, scopeSpan := range resourceSpan.GetScopeSpans() {
			scopeAttrs := protoKvlistToDict(scopeSpan.GetScope().GetAttributes())
			for _, span := range scopeSpan.GetSpans() {
				startNs := int64(span.GetStartTimeUnixNano())
				endNs := int64(span.GetEndTimeUnixNano())
				durationMs := 0.0
				if endNs > startNs {
					durationMs = float64(endNs-startNs) / 1_000_000
				}
				status := "UNSET"
				switch int(span.GetStatus().GetCode()) {
				case 1:
					status = "OK"
				case 2:
					status = "ERROR"
				}
				spanAttrs := protoKvlistToDict(span.GetAttributes())
				mergedAttrs := mergeAttrMaps(resourceAttrs, scopeAttrs, spanAttrs)
				spanEvent := SpanEvent{
					ts:            nsToIso(startNs),
					traceId:       bytesHex(span.GetTraceId()),
					spanId:        bytesHex(span.GetSpanId()),
					parentSpanId:  bytesHex(span.GetParentSpanId()),
					name:          span.GetName(),
					service:       service,
					durationMs:    durationMs,
					status:        status,
					attrs:         mergedAttrs,
					resourceAttrs: resourceAttrs,
					scopeAttrs:    scopeAttrs,
				}
				spanEvents = append(spanEvents, spanEvent)
				if strings.Contains(strings.ToUpper(status), "ERROR") {
					errType := "SpanError"
					if v, present := spanAttrs["exception.type"]; present {
						errType = rowString(v)
					}
					var messageVal any = span.GetName()
					if v, present := spanAttrs["exception.message"]; present {
						messageVal = v
					} else if v, present := spanAttrs["error.message"]; present {
						messageVal = v
					}
					errorEvents = append(errorEvents, ErrorEvent{
						ts:      spanEvent.ts,
						service: service,
						errType: errType,
						message: rowString(messageVal),
						stack:   rowString(spanAttrs["exception.stacktrace"]),
						attrs:   mergedAttrs,
						traceId: spanEvent.traceId,
						spanId:  spanEvent.spanId,
					})
				}
			}
		}
	}
	return spanEvents, errorEvents
}

// protoMetricsToEvents parses an OTLP ExportMetricsServiceRequest into typed
// data-point events.
//
// Supports gauge, sum, and histogram metric types with actual numeric values.
// PORT-NOTE: the error return is always nil (pinned by the s07 ingest
// contract); the Python function cannot fail either.
func protoMetricsToEvents(msg *colmetricspb.ExportMetricsServiceRequest) ([]TypedMetricEvent, error) {
	events := []TypedMetricEvent{}
	numberValue := func(dp *metricspb.NumberDataPoint) float64 {
		switch v := dp.GetValue().(type) {
		case *metricspb.NumberDataPoint_AsInt:
			return float64(v.AsInt)
		case *metricspb.NumberDataPoint_AsDouble:
			return v.AsDouble
		}
		return 0
	}
	for _, resourceMetric := range msg.GetResourceMetrics() {
		resourceAttrs := protoKvlistToDict(resourceMetric.GetResource().GetAttributes())
		service := rowString(resourceAttrs["service.name"])
		if _, present := resourceAttrs["service.name"]; !present {
			service = "metrics"
		}
		for _, scopeMetric := range resourceMetric.GetScopeMetrics() {
			for _, metric := range scopeMetric.GetMetrics() {
				name := metric.GetName()
				desc := metric.GetDescription()
				unit := metric.GetUnit()

				switch data := metric.GetData().(type) {
				case *metricspb.Metric_Gauge:
					for _, dp := range data.Gauge.GetDataPoints() {
						dpAttrs := protoKvlistToDict(dp.GetAttributes())
						value := numberValue(dp)
						ts := nowIso()
						if dp.GetTimeUnixNano() != 0 {
							ts = nsToIso(int64(dp.GetTimeUnixNano()))
						}
						events = append(events, TypedMetricEvent{
							ts:                ts,
							service:           service,
							metricName:        name,
							metricDescription: desc,
							metricUnit:        unit,
							metricKind:        "gauge",
							value:             value,
							attrs:             dpAttrs,
							attrFp:            attrFingerprint(dpAttrs),
						})
					}

				case *metricspb.Metric_Sum:
					for _, dp := range data.Sum.GetDataPoints() {
						dpAttrs := protoKvlistToDict(dp.GetAttributes())
						value := numberValue(dp)
						ts := nowIso()
						if dp.GetTimeUnixNano() != 0 {
							ts = nsToIso(int64(dp.GetTimeUnixNano()))
						}
						isMonotonic := 0
						if data.Sum.GetIsMonotonic() {
							isMonotonic = 1
						}
						events = append(events, TypedMetricEvent{
							ts:                     ts,
							service:                service,
							metricName:             name,
							metricDescription:      desc,
							metricUnit:             unit,
							metricKind:             "sum",
							value:                  value,
							attrs:                  dpAttrs,
							attrFp:                 attrFingerprint(dpAttrs),
							isMonotonic:            isMonotonic,
							aggregationTemporality: int(data.Sum.GetAggregationTemporality()),
						})
					}

				case *metricspb.Metric_Histogram:
					for _, dp := range data.Histogram.GetDataPoints() {
						dpAttrs := protoKvlistToDict(dp.GetAttributes())
						count := int(dp.GetCount())
						histSum := dp.GetSum()
						meanVal := 0.0
						if count > 0 {
							meanVal = histSum / float64(count)
						}
						ts := nowIso()
						if dp.GetTimeUnixNano() != 0 {
							ts = nsToIso(int64(dp.GetTimeUnixNano()))
						}
						events = append(events, TypedMetricEvent{
							ts:                     ts,
							service:                service,
							metricName:             name,
							metricDescription:      desc,
							metricUnit:             unit,
							metricKind:             "histogram",
							value:                  meanVal,
							attrs:                  dpAttrs,
							attrFp:                 attrFingerprint(dpAttrs),
							aggregationTemporality: int(data.Histogram.GetAggregationTemporality()),
							histogramCount:         count,
							histogramSum:           histSum,
							histogramBuckets:       append([]uint64{}, dp.GetBucketCounts()...),
							histogramBounds:        append([]float64{}, dp.GetExplicitBounds()...),
						})
					}

				default:
					// Unsupported metric type (exponential histogram, summary):
					// fall back to a minimal gauge-like entry at current time.
					events = append(events, TypedMetricEvent{
						ts:                nowIso(),
						service:           service,
						metricName:        name,
						metricDescription: desc,
						metricUnit:        unit,
						metricKind:        "gauge",
						value:             0.0,
						attrs:             map[string]any{},
						attrFp:            attrFingerprint(map[string]any{}),
					})
				}
			}
		}
	}

	return events, nil
}

// applyTagRulesBestEffort mirrors the shared try/except around auto-tagging in
// the insert helpers (`except Exception: app.logger.exception(...)`).
// PORT-NOTE: loadTagRules/applyTagRules are owned by the tag-rules section;
// signatures assumed per the deterministic naming rule.
func applyTagRulesBestEffort(db *ChDbConnection, recordKind string, rows []Row, what string) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Error(fmt.Sprintf("auto-tag application failed for %s", what), "error", rec)
		}
	}()
	rules, err := loadTagRules(db)
	if err != nil {
		logger.Error(fmt.Sprintf("auto-tag application failed for %s", what), "error", err)
		return
	}
	if len(rules) > 0 {
		if err := applyTagRules(db, recordKind, rows, rules); err != nil {
			logger.Error(fmt.Sprintf("auto-tag application failed for %s", what), "error", err)
		}
	}
}

// insertLogEvents mirrors _insert_log_events.
func insertLogEvents(db *ChDbConnection, events []LogEvent) (int, error) {
	rows := make([]Row, 0, len(events))
	for _, event := range events {
		rows = append(rows, Row{
			"Timestamp":          event.ts,
			"TraceId":            event.traceId,
			"SpanId":             event.spanId,
			"TraceFlags":         0,
			"SeverityText":       event.level,
			"SeverityNumber":     severityNumber(event.level),
			"ServiceName":        event.service,
			"Body":               event.body,
			"ResourceSchemaUrl":  "",
			"ResourceAttributes": stringifyAttrs(event.resourceAttrs),
			"ScopeSchemaUrl":     "",
			"ScopeName":          "",
			"ScopeVersion":       "",
			"ScopeAttributes":    stringifyAttrs(event.scopeAttrs),
			"LogAttributes":      stringifyAttrs(event.attrs),
			"EventName":          rowString(event.attrs["event.name"]),
		})
	}
	count, err := insertRowsJsonEachRow(db, "otel_logs", rows)
	if err != nil {
		return 0, err
	}
	rememberLogAttrKeys(db, extractLogAttrMaps(rows))
	rememberAttrKeys(db, extractAttrMaps(rows, "ResourceAttributes"), "resource")
	rememberAttrKeys(db, extractAttrMaps(rows, "ScopeAttributes"), "scope")
	applyTagRulesBestEffort(db, "log", rows, "logs")
	return count, nil
}

// insertSpanEvents mirrors _insert_span_events.
func insertSpanEvents(db *ChDbConnection, spanEvents []SpanEvent) (int, error) {
	rows := make([]Row, 0, len(spanEvents))
	for _, event := range spanEvents {
		spanKind, present := event.attrs["span.kind"]
		if !present {
			spanKind = "INTERNAL"
		}
		rows = append(rows, Row{
			"Timestamp":          event.ts,
			"TraceId":            event.traceId,
			"SpanId":             event.spanId,
			"ParentSpanId":       event.parentSpanId,
			"TraceState":         "",
			"SpanName":           event.name,
			"SpanKind":           spanKind,
			"ServiceName":        event.service,
			"ResourceAttributes": stringifyAttrs(event.resourceAttrs),
			"ScopeName":          "",
			"ScopeVersion":       "",
			"SpanAttributes":     stringifyAttrs(event.attrs),
			"Duration":           max(0, int64(event.durationMs*1_000_000)),
			"StatusCode":         traceStatusCode(event.status),
			"StatusMessage":      rowString(event.attrs["status.message"]),
			"Events":             map[string]any{"Timestamp": []any{}, "Name": []any{}, "Attributes": []any{}},
			"Links":              map[string]any{"TraceId": []any{}, "SpanId": []any{}, "TraceState": []any{}, "Attributes": []any{}},
		})
	}
	count, err := insertRowsJsonEachRow(db, "otel_traces", rows)
	if err != nil {
		return 0, err
	}
	rememberAttrKeys(db, extractAttrMaps(rows, "SpanAttributes"), "span")
	rememberAttrKeys(db, extractAttrMaps(rows, "ResourceAttributes"), "resource")
	applyTagRulesBestEffort(db, "trace", rows, "traces")
	return count, nil
}

// insertErrorEvents mirrors _insert_error_events.
func insertErrorEvents(db *ChDbConnection, errorEvents []ErrorEvent) error {
	rows := make([]Row, 0, len(errorEvents))
	for _, event := range errorEvents {
		attrs := stringifyAttrs(event.attrs)
		attrs["exception.type"] = event.errType
		attrs["exception.message"] = event.message
		if event.stack != "" {
			attrs["exception.stacktrace"] = event.stack
		}
		rows = append(rows, Row{
			"Timestamp":          event.ts,
			"TraceId":            event.traceId,
			"SpanId":             event.spanId,
			"TraceFlags":         0,
			"SeverityText":       "ERROR",
			"SeverityNumber":     severityNumber("ERROR"),
			"ServiceName":        event.service,
			"Body":               event.message,
			"ResourceSchemaUrl":  "",
			"ResourceAttributes": map[string]string{},
			"ScopeSchemaUrl":     "",
			"ScopeName":          "",
			"ScopeVersion":       "",
			"ScopeAttributes":    map[string]string{},
			"LogAttributes":      attrs,
			"EventName":          "exception",
		})
	}
	if _, err := insertRowsJsonEachRow(db, "otel_logs", rows); err != nil {
		return err
	}
	rememberLogAttrKeys(db, extractLogAttrMaps(rows))
	applyTagRulesBestEffort(db, "error", rows, "errors")
	return nil
}

// insertMetricEvents inserts typed OTEL metric data points into the
// appropriate metric tables.
func insertMetricEvents(db *ChDbConnection, events []TypedMetricEvent) (int, error) {
	return insertTypedMetricEvents(db, events)
}

// insertTypedMetricEvents routes typed metric events to their respective OTEL
// metric tables.
func insertTypedMetricEvents(db *ChDbConnection, events []TypedMetricEvent) (int, error) {
	gaugeRows := []Row{}
	sumRows := []Row{}
	histogramRows := []Row{}

	for _, ev := range events {
		base := Row{
			"TimeUnix":          ev.ts,
			"ServiceName":       ev.service,
			"MetricName":        ev.metricName,
			"MetricDescription": ev.metricDescription,
			"MetricUnit":        ev.metricUnit,
			"Attributes":        stringifyAttrs(ev.attrs),
			"Value":             ev.value,
			"Flags":             0,
			"AttrFingerprint":   ev.attrFp,
		}
		switch ev.metricKind {
		case "gauge":
			gaugeRows = append(gaugeRows, base)
		case "sum":
			row := make(Row, len(base)+2)
			for k, v := range base {
				row[k] = v
			}
			row["IsMonotonic"] = ev.isMonotonic
			row["AggregationTemporality"] = ev.aggregationTemporality
			sumRows = append(sumRows, row)
		case "histogram":
			row := make(Row, len(base)+5)
			for k, v := range base {
				if k == "Value" {
					continue
				}
				row[k] = v
			}
			row["Count"] = ev.histogramCount
			row["Sum"] = ev.histogramSum
			buckets := ev.histogramBuckets
			if buckets == nil {
				buckets = []uint64{}
			}
			bounds := ev.histogramBounds
			if bounds == nil {
				bounds = []float64{}
			}
			row["BucketCounts"] = buckets
			row["ExplicitBounds"] = bounds
			row["AggregationTemporality"] = ev.aggregationTemporality
			histogramRows = append(histogramRows, row)
		}
	}

	inserted := 0
	if len(gaugeRows) > 0 {
		count, err := insertRowsJsonEachRow(db, "otel_metrics_gauge", gaugeRows)
		if err != nil {
			return inserted, err
		}
		inserted += count
	}
	if len(sumRows) > 0 {
		count, err := insertRowsJsonEachRow(db, "otel_metrics_sum", sumRows)
		if err != nil {
			return inserted, err
		}
		inserted += count
	}
	if len(histogramRows) > 0 {
		count, err := insertRowsJsonEachRow(db, "otel_metrics_histogram", histogramRows)
		if err != nil {
			return inserted, err
		}
		inserted += count
	}
	return inserted, nil
}

// ---------------------------------------------------------------------------
// OTLP request body decompression / parsing
// ---------------------------------------------------------------------------

const protobufContentType = "application/x-protobuf"

// maxDecompressedBodyBytes: maximum number of bytes allowed after
// decompression (32 MiB). Prevents zip-bomb / decompression bomb DoS where a
// tiny compressed payload expands to an unbounded amount of memory.
const maxDecompressedBodyBytes = 32 * 1024 * 1024

// decompressWithLimit incrementally decompresses raw and enforces
// maxDecompressedBodyBytes, raising an error as soon as the cap is exceeded.
// PORT-NOTE: Python selects the container via zlib wbits; the Go port takes a
// format name ("gzip", "zlib", "deflate-raw") instead.
func decompressWithLimit(raw []byte, format string) ([]byte, error) {
	var zr io.ReadCloser
	switch format {
	case "gzip":
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		zr = gz
	case "zlib":
		zlr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		zr = zlr
	case "deflate-raw":
		zr = flate.NewReader(bytes.NewReader(raw))
	default:
		return nil, fmt.Errorf("unsupported decompression format %q", format)
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(io.LimitReader(zr, maxDecompressedBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxDecompressedBodyBytes {
		return nil, fmt.Errorf("decompressed body exceeds %d bytes", maxDecompressedBodyBytes)
	}
	return out, nil
}

// decompressRequestBody decompresses a request body according to its
// Content-Encoding.
//
// The OpenTelemetry Collector's otlphttp exporter can send gzip-compressed
// payloads (Content-Encoding: gzip); the HTTP server does not auto-decompress
// request bodies, so we handle it explicitly here.
//
// Per RFC 9110, Content-Encoding may contain multiple comma-separated values
// applied in order (e.g. "gzip, deflate"). We apply decodings in reverse
// order (outermost first).
//
// Supported individual encodings: gzip, deflate. Unrecognised encodings are
// passed through so that a downstream parse error surfaces a meaningful
// message.
//
// Returns an error if the decompressed body exceeds maxDecompressedBodyBytes
// to guard against decompression bombs.
func decompressRequestBody(raw []byte, contentEncoding string) ([]byte, error) {
	var encodings []string
	for _, e := range strings.Split(contentEncoding, ",") {
		if trimmed := strings.ToLower(strings.TrimSpace(e)); trimmed != "" {
			encodings = append(encodings, trimmed)
		}
	}
	data := raw
	for i := len(encodings) - 1; i >= 0; i-- {
		switch encodings[i] {
		case "gzip":
			decoded, err := decompressWithLimit(data, "gzip")
			if err != nil {
				return nil, err
			}
			data = decoded
		case "deflate":
			// Some senders use raw deflate (no zlib wrapper). Accept both.
			decoded, err := decompressWithLimit(data, "zlib")
			if err != nil {
				decoded, err = decompressWithLimit(data, "deflate-raw")
				if err != nil {
					return nil, err
				}
			}
			data = decoded
		default:
			if len(data) > maxDecompressedBodyBytes {
				return nil, fmt.Errorf("decompressed body exceeds %d bytes", maxDecompressedBodyBytes)
			}
		}
	}
	return data, nil
}

// parseOtlpRequest parses an OTLP HTTP request body into msg.
//
// Returns true on success; on failure it writes the 400 error response and
// returns false (replacing the Python (msg, error_response) tuple — pinned by
// the s07 ingest contract).
//
//   - Content-Type: application/x-protobuf → deserialise with proto.Unmarshal.
//   - Any other content-type (including application/json) → parse JSON and map
//     into the same protobuf message via protobuf JSON mapping.
//
// Both paths transparently handle Content-Encoding: gzip and
// Content-Encoding: deflate request bodies, which the OpenTelemetry Collector
// otlphttp exporter may send when compression is enabled.
func parseOtlpRequest(w http.ResponseWriter, r *http.Request, msg proto.Message) bool {
	mimetype := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	contentEncoding := r.Header.Get("Content-Encoding")
	if mimetype == protobufContentType {
		logger.Debug(fmt.Sprintf("OTLP ingest: parse_path=protobuf endpoint=%s", r.URL.Path))
		raw, err := io.ReadAll(r.Body)
		var body []byte
		if err == nil {
			body, err = decompressRequestBody(raw, contentEncoding)
		}
		if err == nil {
			err = proto.Unmarshal(body, msg)
		}
		if err != nil {
			logger.Warn(fmt.Sprintf("OTLP protobuf parse error [%s]: %s", r.URL.Path, err))
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "failed to parse protobuf body"})
			return false
		}
		return true
	}
	logger.Debug(fmt.Sprintf("OTLP ingest: parse_path=json endpoint=%s", r.URL.Path))
	raw, err := io.ReadAll(r.Body)
	var body []byte
	if err == nil {
		body, err = decompressRequestBody(raw, contentEncoding)
	}
	var payload any = map[string]any{}
	if err == nil && len(body) > 0 {
		err = json.Unmarshal(body, &payload)
	}
	if err != nil {
		logger.Warn(fmt.Sprintf("OTLP json body read/decompress error [%s]: %s", r.URL.Path, err))
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return false
	}
	// Per OTLP spec, JSON ExportMetricsServiceRequest/ExportLogsServiceRequest/
	// ExportTraceServiceRequest must have a top-level object (dict) with
	// resource_metrics/resource_logs/resource_spans keys. Arrays and
	// primitives are invalid and must return 400.
	if _, isObject := payload.(map[string]any); !isObject {
		logger.Warn(fmt.Sprintf("OTLP json parse error [%s]: top-level value is not an object", r.URL.Path))
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "failed to parse json body"})
		return false
	}
	if len(body) > 0 {
		if err := protojson.Unmarshal(body, msg); err != nil {
			logger.Warn(fmt.Sprintf("OTLP json parse error [%s]: %s", r.URL.Path, err))
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "failed to parse json body"})
			return false
		}
	}
	return true
}
