package main

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Auth is a faithful port of app.py's two optional auth layers:
//
//   - require_api_key  -> ingest endpoints (/v1/* + /api/tags/*), gated by SOBS_API_KEY.
//   - require_basic_auth -> the Web UI + JSON API routes, gated by SOBS_BASIC_AUTH_* or
//     SOBS_EXTERNAL_AUTH_URL, with an optional same-origin CSRF check on writes.
//
// Both layers are NO-OPS when unconfigured, exactly as in Python (API_KEY=="" / auth mode
// "none"). The golden parity corpus is captured with everything unset, so an unconfigured
// server behaves byte-for-byte as before — this middleware only changes behaviour once an
// operator sets the documented env vars.
//
// require_api_key has a SECOND acceptance path beyond the static SOBS_API_KEY: a managed,
// DB-backed per-app CI-push key (Settings -> Repositories -> "rotate CI ingest key"). It is
// resolved from the route's app_id/release_id and validated against the per-app scrypt hash +
// expiry; see enforceAPIKey / resolveManagedCITargetAppID / isValidCiPushAPIKey below. It is
// inert (configured==false) until an operator rotates a per-app key, so it does not affect the
// unconfigured parity corpus — but once a key IS configured it gates that app's /v1 routes even
// when the static SOBS_API_KEY is unset, closing the open-POST hole.

// authConfig mirrors the app.py module-level auth globals (read once at process start).
type authConfig struct {
	apiKey      string
	basicUser   string
	basicPass   string
	externalURL string
	csrfCheck   bool
	behindTLS   bool
}

func loadAuthConfig() authConfig {
	behindTLS := envFlag("SOBS_BEHIND_TLS", false)
	return authConfig{
		apiKey:      os.Getenv("SOBS_API_KEY"),
		basicUser:   os.Getenv("SOBS_BASIC_AUTH_USERNAME"),
		basicPass:   os.Getenv("SOBS_BASIC_AUTH_PASSWORD"),
		externalURL: os.Getenv("SOBS_EXTERNAL_AUTH_URL"),
		behindTLS:   behindTLS,
		// app.py: CSRF_ORIGIN_CHECK = _env_flag("SOBS_CSRF_ORIGIN_CHECK", _BEHIND_TLS).
		csrfCheck: envFlag("SOBS_CSRF_ORIGIN_CHECK", behindTLS),
	}
}

// envFlag mirrors app.py _env_flag: truthy set is {1,true,yes,on}; unset -> default.
func envFlag(name string, def bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// mode mirrors app.py _auth_mode: none | basic | external | invalid.
func (a authConfig) mode() string {
	hasUser := a.basicUser != ""
	hasPass := a.basicPass != ""
	hasExternal := a.externalURL != ""
	// Configuration is exclusive: at most one auth type.
	if hasExternal && (hasUser || hasPass) {
		return "invalid"
	}
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

type authClass int

const (
	authPublic authClass = iota // no auth (health, static, MCP own-key, CORS preflight)
	authAPIKey                  // require_api_key  (ingest)
	authBasic                   // require_basic_auth (Web UI + JSON API)
)

// classifyAuth maps (method, path) to the auth layer app.py applies via its route decorators.
// Derived from the authoritative decorator map of app.py (see migration notes): the only basic
// route under /v1/ is GET /v1/rum/assets/<id>; everything else under /v1/ and all of /api/tags/
// is api-key; /mcp* carries its own X-MCP-API-Key auth so it is public to this layer.
func classifyAuth(method, path string) authClass {
	if method == http.MethodOptions {
		return authPublic // CORS preflight
	}
	switch path {
	case "/health", "/health/db", "/healthz", "/service-worker.js":
		return authPublic
	}
	if strings.HasPrefix(path, "/static/") {
		return authPublic
	}
	if path == "/mcp" || strings.HasPrefix(path, "/mcp/") {
		return authPublic // MCP endpoints enforce their own X-MCP-API-Key
	}
	if strings.HasPrefix(path, "/api/tags/") {
		return authAPIKey
	}
	if strings.HasPrefix(path, "/v1/") {
		// GET /v1/rum/assets/<id> is the one Web-UI (basic-auth) route under /v1/.
		if method == http.MethodGet && strings.HasPrefix(path, "/v1/rum/assets/") {
			return authBasic
		}
		return authAPIKey
	}
	return authBasic
}

// enforceAuth runs before the router. It returns true if it has fully written a blocking
// response (and the caller must stop), false to let the request proceed. Responses are written
// through the headerCapture wrapper so the same security headers apply as on any other route.
func (s *server) enforceAuth(w http.ResponseWriter, r *http.Request) bool {
	switch classifyAuth(r.Method, r.URL.Path) {
	case authAPIKey:
		return s.enforceAPIKey(w, r)
	case authBasic:
		return s.enforceUIAuth(w, r)
	default:
		return false
	}
}

// enforceAPIKey mirrors require_api_key. There are two acceptance paths, exactly as in Python:
//
//   - static_ok:  SOBS_API_KEY is set AND X-API-Key equals it.
//   - managed_ok: the route targets an app (resolved from app_id/release_id) that has a managed
//     per-app CI-push key configured, AND X-API-Key validates against its scrypt hash + expiry.
//
// The decision: when SOBS_API_KEY is set, 401 unless static_ok OR managed_ok; when it is unset,
// 401 only if the target app HAS a managed key configured and it does not validate. Open otherwise
// — preserving the unconfigured-server parity behaviour while closing the per-app open-POST hole.
func (s *server) enforceAPIKey(w http.ResponseWriter, r *http.Request) bool {
	key := strings.TrimSpace(r.Header.Get("X-API-Key"))
	staticKey := s.auth.apiKey
	staticOK := staticKey != "" && subtle.ConstantTimeCompare([]byte(key), []byte(staticKey)) == 1

	// Managed per-app key. app.py wraps this lookup in try/except so any DB failure leaves both
	// flags false (the request falls back to the static-key decision); a nil db is the unit-test
	// equivalent of that "lookup unavailable" state.
	managedConfigured := false
	managedOK := false
	if s.db != nil {
		if targetAppID := s.resolveManagedCITargetAppID(r.URL.Path); targetAppID != "" {
			if configured, _ := s.ciPushStatus(targetAppID)["configured"].(bool); configured {
				managedConfigured = true
				managedOK = s.isValidCiPushAPIKey(targetAppID, key)
			}
		}
	}

	if staticKey != "" {
		if !staticOK && !managedOK {
			writeJSON(w, http.StatusUnauthorized, jsonenc.NewObject().Set("error", "Unauthorized"))
			return true
		}
	} else if managedConfigured && !managedOK {
		writeJSON(w, http.StatusUnauthorized, jsonenc.NewObject().Set("error", "Unauthorized"))
		return true
	}
	return false
}

// resolveManagedCITargetAppID mirrors app.py _resolve_managed_ci_target_app_id over the route
// params require_api_key sees. Only /v1/apps/<app_id>[/releases] and
// /v1/releases/<release_id>[/artifacts[/meta]] expose an app_id/release_id; every other api-key
// route (ingest, /api/tags/<...>, the /v1/apps collection) resolves to "" and is never gated by a
// managed key — exactly as in Python, whose handlers take no app_id/release_id kwarg there. The
// segment is extracted the same way handlers_v1.go does, so the gate resolves the SAME app the
// handler will operate on.
func (s *server) resolveManagedCITargetAppID(path string) string {
	if rest, ok := strings.CutPrefix(path, "/v1/apps/"); ok {
		appID := rest
		if a, ok := strings.CutSuffix(rest, "/releases"); ok {
			appID = a
		}
		appID = strings.TrimSpace(appID)
		if appID == "" || strings.Contains(appID, "/") {
			return "" // not a single-segment <app_id> (Flask would not route it to require_api_key)
		}
		return appID
	}
	if rest, ok := strings.CutPrefix(path, "/v1/releases/"); ok {
		relID := rest
		if r, ok := strings.CutSuffix(rest, "/artifacts/meta"); ok {
			relID = r
		} else if r, ok := strings.CutSuffix(rest, "/artifacts"); ok {
			relID = r
		}
		relID = strings.TrimSpace(relID)
		if relID == "" || strings.Contains(relID, "/") {
			return ""
		}
		release, found := s.findReleaseByID(relID)
		if !found {
			return "" // _find_release_by_id miss -> "" (no managed gating; handler returns 404)
		}
		return strings.TrimSpace(cStr(release, "AppId"))
	}
	return ""
}

// isValidCiPushAPIKey mirrors app.py _is_valid_ci_push_api_key: a non-empty candidate, a configured
// per-app hash that is unexpired and scrypt-prefixed, and a constant-time match of the candidate's
// scrypt fingerprint against the stored hash.
func (s *server) isValidCiPushAPIKey(appID, providedKey string) bool {
	candidate := strings.TrimSpace(providedKey)
	if candidate == "" {
		return false
	}
	meta := s.ciPushStatus(appID)
	keyHash, _ := meta["hash"].(string)
	if keyHash == "" {
		return false
	}
	if expiry, ok := meta["expiry"].(map[string]any); ok {
		if state, _ := expiry["state"].(string); strings.ToLower(state) == "expired" {
			return false
		}
	}
	if !strings.HasPrefix(keyHash, ciPushHashPrefix) {
		return false
	}
	candidateHash := hashAPIKey(candidate)
	return subtle.ConstantTimeCompare([]byte(candidateHash), []byte(keyHash)) == 1
}

// enforceUIAuth mirrors require_basic_auth: invalid-config 500, optional same-origin CSRF check
// on writes, then none/basic/external acceptance with a WWW-Authenticate challenge on failure.
func (s *server) enforceUIAuth(w http.ResponseWriter, r *http.Request) bool {
	mode := s.auth.mode()
	if mode == "invalid" {
		writeJSON(w, http.StatusInternalServerError, jsonenc.NewObject().Set("error", "Server auth misconfiguration"))
		return true
	}
	if mode != "none" && s.auth.csrfCheck && isStateChangingMethod(r.Method) {
		if !sameOriginRequest(r) {
			writeJSON(w, http.StatusForbidden, jsonenc.NewObject().Set("error", "CSRF origin check failed"))
			return true
		}
	}
	if mode == "none" {
		return false
	}

	auth := r.Header.Get("Authorization")

	// Valid HTTP Basic credentials.
	if mode == "basic" && strings.HasPrefix(auth, "Basic ") {
		if decoded, err := base64.StdEncoding.DecodeString(auth[6:]); err == nil {
			user, pass, _ := strings.Cut(string(decoded), ":")
			userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.auth.basicUser)) == 1
			passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.auth.basicPass)) == 1
			if userOK && passOK {
				return false
			}
		}
	}

	// Bearer token validated by the external auth service, with a same-origin "session" cookie
	// fallback for browser requests that carry no explicit Authorization header.
	if mode == "external" {
		if !strings.HasPrefix(auth, "Bearer ") {
			if c, err := r.Cookie("session"); err == nil {
				v := c.Value
				if v != "" && !strings.ContainsAny(v, "\r\n") {
					auth = "Bearer " + v
				}
			}
		}
		if strings.HasPrefix(auth, "Bearer ") && s.checkExternalAuth(auth) {
			return false
		}
	}

	wwwAuth := `Bearer realm="SOBS"`
	if mode == "basic" {
		wwwAuth = `Basic realm="SOBS"`
	}
	hdr := w.Header()
	hdr.Set("WWW-Authenticate", wwwAuth)
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte("Unauthorized"))
	return true
}

// checkExternalAuth mirrors app.py _check_external_auth: POST the Authorization header to
// {EXTERNAL_AUTH_URL}/internal/auth/validate; true only on HTTP 200.
//
// The dial goes through s.upstreamRequest so it honours the SOBS_UPSTREAM_FIXTURES mock under the
// parity harness — exactly as the Python oracle's _get_async_http_client() does (its httpx client is
// patched by the determinism shim's MockTransport). app.py reads the external auth service through
// the SAME shimmed client, so without this both sides would disagree: Python returns the canned 200
// (pass) while a raw net/http dial here would hit the (non-existent) host and fail to 401. When the
// fixtures dir is UNSET (real runtime) upstreamRequest falls back to a real http.Client, sending the
// Authorization header to the live auth service — matching Python's runtime behaviour. The 5s timeout
// app.py sets is a transport detail with no effect on either the mocked or the byte-compared result.
func (s *server) checkExternalAuth(authorization string) bool {
	if s.auth.externalURL == "" {
		return false
	}
	endpoint := strings.TrimRight(s.auth.externalURL, "/") + "/internal/auth/validate"
	resp, err := s.upstreamRequest(http.MethodPost, endpoint, nil, map[string]string{"Authorization": authorization})
	if err != nil {
		return false
	}
	return resp.Status == http.StatusOK
}

func isStateChangingMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// normalizeOrigin mirrors app.py _normalize_origin: "scheme://host[:port]" lowercased, or "".
func normalizeOrigin(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// sameOriginRequest mirrors app.py _same_origin_request.
func sameOriginRequest(r *http.Request) bool {
	origin := normalizeOrigin(r.Header.Get("Origin"))

	refererOrigin := ""
	if ref := strings.TrimSpace(r.Header.Get("Referer")); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
			refererOrigin = strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
		}
	}

	expectedHost := firstCSVLower(r.Header.Get("X-Forwarded-Host"))
	if expectedHost == "" {
		expectedHost = strings.ToLower(strings.TrimSpace(r.Host))
	}
	expectedScheme := firstCSVLower(r.Header.Get("X-Forwarded-Proto"))
	if expectedScheme == "" {
		if r.TLS != nil {
			expectedScheme = "https"
		} else {
			expectedScheme = "http"
		}
	}
	if expectedHost == "" {
		return false
	}
	expectedOrigin := expectedScheme + "://" + expectedHost
	return origin == expectedOrigin || refererOrigin == expectedOrigin
}

// firstCSVLower returns the first comma-separated token, trimmed and lowercased.
func firstCSVLower(v string) string {
	s := v
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}
