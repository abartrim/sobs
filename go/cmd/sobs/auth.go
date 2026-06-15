package main

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

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
// Not yet ported: the managed per-app CI-push key path inside require_api_key (DB-backed
// per-app keys). It is inert unless an operator configures a per-app key in Settings ->
// Repositories, and it does not affect parity or the documented SOBS_API_KEY behaviour.

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

// enforceAPIKey mirrors require_api_key's static-key path: when SOBS_API_KEY is set, the
// X-API-Key header must match it, else 401 jsonify({"error":"Unauthorized"}).
func (s *server) enforceAPIKey(w http.ResponseWriter, r *http.Request) bool {
	if s.auth.apiKey == "" {
		return false // no static key configured -> open (managed per-app keys not yet ported)
	}
	key := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if subtle.ConstantTimeCompare([]byte(key), []byte(s.auth.apiKey)) == 1 {
		return false
	}
	writeJSON(w, http.StatusUnauthorized, jsonenc.NewObject().Set("error", "Unauthorized"))
	return true
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

var externalAuthClient = &http.Client{Timeout: 5 * time.Second}

// checkExternalAuth mirrors app.py _check_external_auth: POST the Authorization header to
// {EXTERNAL_AUTH_URL}/internal/auth/validate; true only on HTTP 200.
func (s *server) checkExternalAuth(authorization string) bool {
	if s.auth.externalURL == "" {
		return false
	}
	endpoint := strings.TrimRight(s.auth.externalURL, "/") + "/internal/auth/validate"
	req, err := http.NewRequest(http.MethodPost, endpoint, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", authorization)
	resp, err := externalAuthClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
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
