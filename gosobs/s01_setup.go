package main

// Port of app.py lines 1-710: app setup, JSON safety coercion, masking
// settings cache + masked JSON glue, custom masking pattern validation,
// OTLP CORS helpers, security headers, env/base-path helpers, settings
// encryption (Fernet), BasePathMiddleware.

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fernet/fernet-go"
)

// coerceUndefinedForJson replaces Jinja Undefined sentinels with nil so JSON
// encoding can proceed. PORT-NOTE: Go data never contains Jinja Undefined;
// kept for structural fidelity (recursion depth guard preserved).
func coerceUndefinedForJson(value any, depth, maxDepth int) any {
	if depth > maxDepth {
		return value
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = coerceUndefinedForJson(item, depth+1, maxDepth)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = coerceUndefinedForJson(item, depth+1, maxDepth)
		}
		return out
	default:
		return value
	}
}

const (
	maskingCustomKeysSetting       = "masking.custom_keys"
	maskingCustomPatternsSetting   = "masking.custom_patterns"
	maskingOutputEnabledSetting    = "masking.output_enabled"
	maskingSqlOutputEnabledSetting = "masking.sql_output_enabled"
	maxCustomMaskingPatternLength  = 512
)

var sqlOutputMaskFieldNames = map[string]bool{
	"sql": true, "query": true, "sample_sql": true, "override_sql": true,
}

var (
	redosNestedQuantifierRe     = regexp.MustCompile(`\((?:[^()\\]|\\.)*[+*](?:[^()\\]|\\.)*\)\s*(?:[+*]|\{\d+,?\d*\})`)
	redosAmbiguousAlternationRe = regexp.MustCompile(`\((?:[^()\\]|\\.)*\|(?:[^()\\]|\\.)*\)\s*(?:[+*]|\{\d+,?\d*\})`)
)

var (
	maskingRulesRefreshLock    sync.Mutex
	maskingLastRulesSignature  *[2][]string
	maskingSettingsCacheLock   sync.Mutex
	maskingSettingsCacheLoaded bool
	maskingSettingsOutput      = true
	maskingSettingsSqlOutput   = true
)

func setMaskingSettingsCache(outputEnabled, sqlOutputEnabled *bool, loaded bool) {
	maskingSettingsCacheLock.Lock()
	defer maskingSettingsCacheLock.Unlock()
	if outputEnabled != nil {
		maskingSettingsOutput = *outputEnabled
	}
	if sqlOutputEnabled != nil {
		maskingSettingsSqlOutput = *sqlOutputEnabled
	}
	maskingSettingsCacheLoaded = loaded
}

func loadMaskingSettingsFlags(db *ChDbConnection) (bool, bool) {
	outputEnabled := isTruthySetting(getAppSetting(db, maskingOutputEnabledSetting), true)
	sqlOutputEnabled := isTruthySetting(getAppSetting(db, maskingSqlOutputEnabledSetting), true)
	return outputEnabled, sqlOutputEnabled
}

func getMaskingSettingsFlags(db *ChDbConnection) (bool, bool) {
	maskingSettingsCacheLock.Lock()
	if maskingSettingsCacheLoaded {
		o, s := maskingSettingsOutput, maskingSettingsSqlOutput
		maskingSettingsCacheLock.Unlock()
		return o, s
	}
	maskingSettingsCacheLock.Unlock()

	outputEnabled, sqlOutputEnabled := true, true
	func() {
		defer func() {
			if r := recover(); r != nil {
				outputEnabled, sqlOutputEnabled = true, true
			}
		}()
		resolved := db
		if resolved == nil {
			resolved = getDb()
		}
		outputEnabled, sqlOutputEnabled = loadMaskingSettingsFlags(resolved)
	}()

	setMaskingSettingsCache(&outputEnabled, &sqlOutputEnabled, true)
	return outputEnabled, sqlOutputEnabled
}

// maskJsonPayload masks observability payloads before sending them as JSON.
func maskJsonPayload(value any) any {
	return maskPayloadForOutputJson(value, nil, true)
}

func isTruthySetting(raw string, def bool) bool {
	if raw == "" {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func normalizeJsRegexFlags(flagText string) string {
	out := ""
	for _, ch := range flagText {
		if strings.ContainsRune("gimsuy", ch) && !strings.ContainsRune(out, ch) {
			out += string(ch)
		}
	}
	return out
}

var inlineFlagsRe = regexp.MustCompile(`^\(\?([a-zA-Z]+)\)`)
var namedGroupRe = regexp.MustCompile(`\(\?P<[^>]+>`)

// validateCustomMaskingPatternForStorage ports the ReDoS / JS-compatibility
// safety checks for user-supplied masking patterns.
func validateCustomMaskingPatternForStorage(pattern any) (string, error) {
	normalized, err := maskingValidatePattern(pattern)
	if err != nil {
		return "", err
	}
	if len(normalized) > maxCustomMaskingPatternLength {
		return "", fmt.Errorf("Safety check failed: pattern is too long (max %d chars)", maxCustomMaskingPatternLength)
	}
	if strings.Contains(normalized, `\1`) || strings.Contains(normalized, `\2`) || strings.Contains(normalized, `\3`) {
		return "", fmt.Errorf("Safety check failed: backreferences are not allowed in custom masking patterns")
	}
	if redosNestedQuantifierRe.MatchString(normalized) {
		return "", fmt.Errorf("Safety check failed: pattern contains nested quantifiers and may cause catastrophic backtracking")
	}
	if redosAmbiguousAlternationRe.MatchString(normalized) {
		return "", fmt.Errorf("Safety check failed: pattern contains quantified alternation and may cause catastrophic backtracking")
	}

	jsPattern := normalized
	jsFlags := "g"
	if m := inlineFlagsRe.FindStringSubmatch(jsPattern); m != nil {
		jsFlags += normalizeJsRegexFlags(m[1])
		jsPattern = jsPattern[len(m[0]):]
	}
	jsFlags = normalizeJsRegexFlags(jsFlags)

	// Mirror the browser helper's Python-to-JS compatibility normalization.
	jsPattern = strings.ReplaceAll(jsPattern, `\A`, "^")
	jsPattern = strings.ReplaceAll(jsPattern, `\Z`, "$")
	jsPattern = namedGroupRe.ReplaceAllString(jsPattern, "(")

	if strings.Contains(jsPattern, "(?<=") || strings.Contains(jsPattern, "(?<!") {
		return "", fmt.Errorf("JavaScript compatibility check failed: lookbehind is not supported for screenshot DOM masking helper")
	}

	pyJsFlags := ""
	if strings.Contains(jsFlags, "i") {
		pyJsFlags += "i"
	}
	if strings.Contains(jsFlags, "m") {
		pyJsFlags += "m"
	}
	if strings.Contains(jsFlags, "s") {
		pyJsFlags += "s"
	}
	flagPrefix := ""
	if pyJsFlags != "" {
		flagPrefix = "(?" + pyJsFlags + ")"
	}
	jsCompiled, err := regexp.Compile(flagPrefix + jsPattern)
	if err != nil {
		return "", fmt.Errorf("JavaScript compatibility check failed: %v", err)
	}

	// Light smoke-test to fail fast on patterns that are extremely expensive
	// before persisting. PORT-NOTE: Go's RE2 has no catastrophic backtracking,
	// but the smoke test is preserved for behavioural fidelity.
	normalizedCompiled, err := regexp.Compile("(?s)" + normalized)
	if err != nil {
		return "", fmt.Errorf("Runtime smoke-test failed: %v", err)
	}
	samples := []string{
		strings.Repeat("a", 48) + "!",
		"customerRef=ZXCVBNM1234 email=ops@example.com",
		"Authorization: Bearer supersecrettoken123",
	}
	for _, sample := range samples {
		_ = normalizedCompiled.ReplaceAllString(sample, maskingMask)
		_ = jsCompiled.ReplaceAllString(sample, maskingMask)
	}

	return normalized, nil
}

func maskPayloadForOutputJson(value any, db *ChDbConnection, maskSqlFields bool) any {
	safeValue := coerceUndefinedForJson(value, 0, 12)
	if !isOutputMaskingEnabled(db) {
		return safeValue
	}
	switch v := safeValue.(type) {
	case map[string]any:
		masked := make(map[string]any, len(v))
		for key, item := range v {
			keyName := maskingNormalizeSensitiveKey(key)
			if maskingIsSensitiveKey(keyName) {
				masked[key] = maskingMask
				continue
			}
			if itemStr, isStr := item.(string); sqlOutputMaskFieldNames[keyName] && isStr && !maskSqlFields {
				masked[key] = itemStr
				continue
			}
			masked[key] = maskPayloadForOutputJson(item, db, maskSqlFields)
		}
		return masked
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = maskPayloadForOutputJson(item, db, maskSqlFields)
		}
		return out
	default:
		return maskValueForOutput(safeValue, db)
	}
}

func isOutputMaskingEnabled(db *ChDbConnection) bool {
	outputEnabled, _ := getMaskingSettingsFlags(db)
	return outputEnabled
}

func maskValueForOutput(value any, db *ChDbConnection) any {
	if !isOutputMaskingEnabled(db) {
		return value
	}
	return maskingMaskValue(value)
}

func maskStringForOutput(value any, db *ChDbConnection) string {
	if !isOutputMaskingEnabled(db) {
		if value == nil {
			return ""
		}
		return fmt.Sprintf("%v", value)
	}
	return maskingMaskString(value)
}

func isSqlOutputMaskingEnabled(db *ChDbConnection) bool {
	_, sqlOutputEnabled := getMaskingSettingsFlags(db)
	return sqlOutputEnabled
}

// jsonifyWithOptionalSqlOutputMask mirrors _jsonify_with_optional_sql_output_mask.
func jsonifyWithOptionalSqlOutputMask(w http.ResponseWriter, payload any) {
	jsonResponse(w, http.StatusOK, maskPayloadForOutputJson(payload, nil, isSqlOutputMaskingEnabled(nil)))
}

// ---------------------------------------------------------------------------
// Secure-context / CORS helpers
// ---------------------------------------------------------------------------

func requestIsSecureContext(r *http.Request) bool {
	if behindTls {
		return true
	}
	forwardedProto := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("X-Forwarded-Proto"), ",", 2)[0]))
	if forwardedProto == "https" {
		return true
	}
	return r.TLS != nil
}

var otlpCorsAllowedOrigins = func() []string {
	raw := os.Getenv("SOBS_OTLP_CORS_ALLOWED_ORIGINS")
	if raw == "" {
		raw = "http://localhost:*,https://localhost:*,http://127.0.0.1:*,https://127.0.0.1:*"
	}
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}()

// Exact paths that are OTLP/RUM ingest endpoints exposed to browsers.
// CORS is applied only to these paths, NOT to management API routes like
// /v1/apps or /v1/releases which are not intended for browser cross-origin use.
var otlpCorsIngestPaths = map[string]bool{
	"/v1/logs":             true,
	"/v1/traces":           true,
	"/v1/metrics":          true,
	"/v1/rum":              true,
	"/v1/rum/assets":       true,
	"/v1/rum/client-token": true,
	"/v1/errors":           true,
	"/v1/ai":               true,
}

// Default ports per scheme – used to normalise origins for matching.
var schemeDefaultPorts = map[string]int{"http": 80, "https": 443}

// fnmatchSimple ports fnmatch.fnmatch for the * and ? wildcards used in
// origin patterns (no [seq] support needed by the defaults, but included).
func fnmatchSimple(name, pattern string) bool {
	var sb strings.Builder
	sb.WriteString("(?s)^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteString(".")
		default:
			sb.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return false
	}
	return re.MatchString(name)
}

func originAllowedForOtlp(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	netloc := strings.ToLower(parsed.Host)
	if (scheme != "http" && scheme != "https") || netloc == "" {
		return false
	}

	withPort := scheme + "://" + netloc
	// Include the port-stripped form only when the origin carries no explicit
	// port or uses the scheme default. Non-default ports must match explicitly.
	candidates := []string{withPort}
	portStr := parsed.Port()
	var parsedPort *int
	if portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return false
		}
		parsedPort = &p
	}
	if parsedPort == nil || *parsedPort == schemeDefaultPorts[scheme] {
		withoutPort := withPort
		if host != "" {
			withoutPort = scheme + "://" + host
		}
		if withoutPort != withPort {
			candidates = append(candidates, withoutPort)
		}
	}
	for _, pattern := range otlpCorsAllowedOrigins {
		p := strings.ToLower(pattern)
		for _, c := range candidates {
			if fnmatchSimple(c, p) {
				return true
			}
		}
	}
	return false
}

// pathNeedsOtlpCors returns true if path is an OTLP/RUM ingest endpoint that
// may receive browser cross-origin requests.
func pathNeedsOtlpCors(path string) bool {
	if otlpCorsIngestPaths[path] {
		return true
	}
	// Dynamic sub-paths under /v1/rum/assets/ (individual asset downloads).
	return strings.HasPrefix(path, "/v1/rum/assets/")
}

// otlpCorsAllowMethods returns allowed methods for CORS preflight.
func otlpCorsAllowMethods(path string) string {
	if strings.HasPrefix(path, "/v1/rum/assets/") {
		return "GET, HEAD, OPTIONS"
	}
	return "POST, OPTIONS"
}

func appendVaryHeader(h http.Header, value string) {
	existing := h.Get("Vary")
	if existing == "" {
		h.Set("Vary", value)
		return
	}
	var parts []string
	lowerSeen := map[string]bool{}
	for _, p := range strings.Split(existing, ",") {
		if s := strings.TrimSpace(p); s != "" {
			parts = append(parts, s)
			lowerSeen[strings.ToLower(s)] = true
		}
	}
	// Vary tokens are case-insensitive; compare lower-case to avoid dupes.
	if !lowerSeen[strings.ToLower(value)] {
		h.Set("Vary", strings.Join(append(parts, value), ", "))
	}
}

func headerSetDefault(h http.Header, key, value string) {
	if h.Get(key) == "" {
		h.Set(key, value)
	}
}

// applySecurityHeaders ports the @app.after_request hook as middleware that
// sets headers before the wrapped handler writes them.
func applySecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		headerSetDefault(h, "X-Content-Type-Options", "nosniff")
		headerSetDefault(h, "X-Frame-Options", "SAMEORIGIN")
		headerSetDefault(h, "Referrer-Policy", "strict-origin-when-cross-origin")
		headerSetDefault(h, "Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		headerSetDefault(h, "Content-Security-Policy", "frame-ancestors 'self'; object-src 'none'; base-uri 'self'")
		if requestIsSecureContext(r) {
			headerSetDefault(h, "Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		if pathNeedsOtlpCors(r.URL.Path) {
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			if origin != "" && originAllowedForOtlp(origin) {
				h.Set("Access-Control-Allow-Origin", origin)
				appendVaryHeader(h, "Origin")
				headerSetDefault(h, "Access-Control-Allow-Credentials", "true")
				headerSetDefault(h, "Access-Control-Allow-Methods", otlpCorsAllowMethods(r.URL.Path))
				headerSetDefault(h, "Access-Control-Allow-Headers",
					"Content-Type, Authorization, X-API-Key, "+
						"X-SOBS-RUM-Client, X-SOBS-RUM-Signature, X-SOBS-RUM-Timestamp, "+
						"X-SOBS-Asset-Timestamp, X-SOBS-Asset-Signature")
				headerSetDefault(h, "Access-Control-Max-Age", "600")
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Env / base-path helpers
// ---------------------------------------------------------------------------

func envFlag(name string, def bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

var multiSlashRe = regexp.MustCompile(`/+`)

// normalizeBasePath normalizes base path values to ” or '/segment[/sub]'.
func normalizeBasePath(value string) string {
	if value == "" {
		return ""
	}
	normalized := multiSlashRe.ReplaceAllString(strings.TrimSpace(value), "/")
	if normalized == "" || normalized == "/" {
		return ""
	}
	if !strings.HasPrefix(normalized, "/") {
		normalized = "/" + normalized
	}
	normalized = strings.TrimRight(normalized, "/")
	if normalized == "/" {
		return ""
	}
	return normalized
}

// mergeScriptName appends base path to SCRIPT_NAME once.
func mergeScriptName(scriptName, basePath string) string {
	if scriptName == "" {
		return scriptName
	}
	if strings.HasSuffix(scriptName, basePath) {
		return scriptName
	}
	return strings.TrimRight(scriptName, "/") + basePath
}

// ---------------------------------------------------------------------------
// Module-level config (Python module top-level)
// ---------------------------------------------------------------------------

var (
	basePath              = normalizeBasePath(os.Getenv("SOBS_BASE_PATH"))
	secretKey             = envDefault("SOBS_SECRET_KEY", "sobs-dev-secret-key")
	sessionCookieName     = envDefault("SOBS_SESSION_COOKIE_NAME", "sobs_session")
	enableFirstRunTour    = envFlag("SOBS_ENABLE_FIRST_RUN_TOUR", true)
	behindTls             = envFlag("SOBS_BEHIND_TLS", false)
	sessionCookieSameSite = func() string {
		v := strings.ToLower(strings.TrimSpace(envDefault("SOBS_SESSION_COOKIE_SAMESITE", "Lax")))
		switch v {
		case "lax", "strict", "none":
			return strings.ToUpper(v[:1]) + v[1:]
		}
		return "Lax"
	}()
)

func envDefault(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

const (
	settingsEncryptionPrefix     = "enc:v1:"
	settingsEncryptionKeyEnv     = "SOBS_SETTINGS_ENCRYPTION_KEY"
	settingsEncryptionKeyFileEnv = "SOBS_SETTINGS_ENCRYPTION_KEY_FILE"
)

func readEnvOrFile(envVar, fileEnvVar string) string {
	value := strings.TrimSpace(os.Getenv(envVar))
	if value != "" {
		return value
	}
	if fileEnvVar == "" {
		return ""
	}
	filePath := strings.TrimSpace(os.Getenv(fileEnvVar))
	if filePath == "" {
		return ""
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		logger.Warn("Failed to read env from file", "env", envVar, "file", filePath, "error", err)
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func readFileOrEnv(envVar, fileEnvVar string) string {
	if fileEnvVar != "" {
		filePath := strings.TrimSpace(os.Getenv(fileEnvVar))
		if filePath != "" {
			raw, err := os.ReadFile(filePath)
			if err != nil {
				logger.Warn("Failed to read env from file", "env", envVar, "file", filePath, "error", err)
			} else if fileValue := strings.TrimSpace(string(raw)); fileValue != "" {
				return fileValue
			}
		}
	}
	return strings.TrimSpace(os.Getenv(envVar))
}

func loadSettingsEncryptionSecret() string {
	return readEnvOrFile(settingsEncryptionKeyEnv, settingsEncryptionKeyFileEnv)
}

var settingsEncryptionSecret = loadSettingsEncryptionSecret()

func settingsFernetKey() (*fernet.Key, error) {
	digest := sha256.Sum256([]byte(settingsEncryptionSecret))
	encoded := base64.URLEncoding.EncodeToString(digest[:])
	return fernet.DecodeKey(encoded)
}

func encryptSecretValue(value string) string {
	if value == "" || settingsEncryptionSecret == "" {
		return value
	}
	if strings.HasPrefix(value, settingsEncryptionPrefix) {
		return value
	}
	key, err := settingsFernetKey()
	if err != nil {
		logger.Warn("Failed to encrypt secret setting", "error", err)
		return value
	}
	token, err := fernet.EncryptAndSign([]byte(value), key)
	if err != nil {
		logger.Warn("Failed to encrypt secret setting", "error", err)
		return value
	}
	return settingsEncryptionPrefix + string(token)
}

func decryptSecretValue(value string) string {
	if value == "" {
		return value
	}
	if !strings.HasPrefix(value, settingsEncryptionPrefix) {
		return value
	}
	if settingsEncryptionSecret == "" {
		logger.Warn("Encrypted setting found but no decryption key is configured")
		return ""
	}
	token := value[len(settingsEncryptionPrefix):]
	key, err := settingsFernetKey()
	if err != nil {
		logger.Warn("Failed to decrypt secret setting", "error", err)
		return ""
	}
	msg := fernet.VerifyAndDecrypt([]byte(token), 0, []*fernet.Key{key})
	if msg == nil {
		logger.Warn("Failed to decrypt setting value: invalid encryption key")
		return ""
	}
	return string(msg)
}

// ---------------------------------------------------------------------------
// BasePathMiddleware — deployment behind a path prefix / proxy prefix headers.
// ---------------------------------------------------------------------------

func mergeRootPath(rootPath, base string) string {
	if base == "" {
		return rootPath
	}
	if strings.HasSuffix(rootPath, base) {
		return rootPath
	}
	if rootPath == "" {
		return base
	}
	return strings.TrimRight(rootPath, "/") + base
}

// basePathMiddleware strips/normalizes a configured or proxy-forwarded path
// prefix so route patterns match, mirroring the ASGI BasePathMiddleware.
func basePathMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded := normalizeBasePath(r.Header.Get("X-Forwarded-Prefix"))
		effectiveBase := forwarded
		if effectiveBase == "" {
			effectiveBase = basePath
		}
		if effectiveBase != "" {
			pathInfo := r.URL.Path
			if pathInfo == "" {
				pathInfo = "/"
			}
			if strings.HasPrefix(pathInfo, effectiveBase+"/") || pathInfo == effectiveBase {
				// Prefix present: strip it before routing (Quart strips
				// root_path from the path internally).
				stripped := strings.TrimPrefix(pathInfo, effectiveBase)
				if stripped == "" {
					stripped = "/"
				}
				r2 := r.Clone(r.Context())
				r2.URL.Path = stripped
				next.ServeHTTP(w, r2)
				return
			}
			// Proxy already stripped the prefix: route as-is.
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Data dirs and global config vars
// ---------------------------------------------------------------------------

var (
	dataDir     = envDefault("SOBS_DATA_DIR", filepath.Join(moduleDir(), "data"))
	dbPath      = filepath.Join(dataDir, "sobs.chdb")
	rumAssetDir = filepath.Join(dataDir, "rum_assets")

	apiKey            = os.Getenv("SOBS_API_KEY")             // empty = no auth required
	basicAuthUsername = os.Getenv("SOBS_BASIC_AUTH_USERNAME") // empty = no basic auth
	basicAuthPassword = os.Getenv("SOBS_BASIC_AUTH_PASSWORD")
	externalAuthUrl   = os.Getenv("SOBS_EXTERNAL_AUTH_URL") // empty = disabled

	rumAssetSigningKey    = os.Getenv("SOBS_RUM_ASSET_SIGNING_KEY")
	rumAssetSignWindowSec = envInt("SOBS_RUM_ASSET_SIGN_WINDOW_SEC", 300)
	rumAssetMaxBytes      = envInt("SOBS_RUM_ASSET_MAX_BYTES", 8*1024*1024)
	rumClientAuthMode     = strings.ToLower(strings.TrimSpace(envDefault("SOBS_RUM_CLIENT_AUTH_MODE", "none")))
	rumClientSigningKey   = os.Getenv("SOBS_RUM_CLIENT_SIGNING_KEY")
	rumClientTokenTtlSec  = envInt("SOBS_RUM_CLIENT_TOKEN_TTL_SEC", 900)
	csrfOriginCheck       = envFlag("SOBS_CSRF_ORIGIN_CHECK", behindTls)
	sourceMapDir          = strings.TrimSpace(os.Getenv("SOBS_SOURCE_MAP_DIR"))
	sourceMapEnable       = envFlag("SOBS_SOURCE_MAP_ENABLE", false)
	buildVersion          = strings.TrimSpace(os.Getenv("SOBS_BUILD_VERSION"))
)

const (
	appRegistrySeedJsonEnv     = "SOBS_APP_REGISTRY_SEED_JSON"
	appRegistrySeedJsonFileEnv = "SOBS_APP_REGISTRY_SEED_JSON_FILE"
	chdbConfigFileEnv          = "SOBS_CLICKHOUSE_CONFIG_FILE"
	chdbExpectDiskEnv          = "SOBS_CHDB_EXPECT_DISK"
	chdbExpectPolicyEnv        = "SOBS_CHDB_EXPECT_STORAGE_POLICY"
	chdbMaxServerMbEnv         = "SOBS_CHDB_MAX_SERVER_MB"
	chdbMarkCacheMbEnv         = "SOBS_CHDB_MARK_CACHE_MB"
	chdbUncompressedCacheMbEnv = "SOBS_CHDB_UNCOMPRESSED_CACHE_MB"
	chdbMaxThreadsEnv          = "SOBS_CHDB_MAX_THREADS"
	chdbSpillGroupByMbEnv      = "SOBS_CHDB_SPILL_GROUP_BY_MB"
	chdbSpillSortMbEnv         = "SOBS_CHDB_SPILL_SORT_MB"
)

func moduleDir() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Dir(exe)
	}
	wd, _ := os.Getwd()
	return wd
}

func envInt(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		// Python int() raises at import; the Go port falls back to default.
		logger.Warn("invalid integer env var", "name", name, "value", raw)
		return def
	}
	return n
}

func ensureDataDirs() {
	_ = os.MkdirAll(dataDir, 0o755)
	_ = os.MkdirAll(rumAssetDir, 0o755)
}

// startupHooks mirrors @app.before_serving _startup_async_http_client.
func startupHooks() {
	warnUnimplementedAiActionAnnotations()
	telemetryConfigureTelemetry()
}

// shutdownHooks mirrors @app.after_serving (background loops cancel via
// context in main; DB resources shut down here).
func shutdownHooks() {
	shutdownDbResources()
}

// maybeAwait has no Go equivalent (no awaitables); synchronous call sites
// simply use the value. Kept as identity for fidelity at translated sites.
func maybeAwait(value any) any { return value }

var _ = time.Now // keep time import if optimizations remove other uses
