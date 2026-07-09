package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClassifyAuth(t *testing.T) {
	cases := []struct {
		method, path string
		want         authClass
	}{
		{"GET", "/health", authPublic},
		{"GET", "/health/db", authPublic},
		{"GET", "/healthz", authPublic},
		{"GET", "/service-worker.js", authPublic},
		{"GET", "/static/app.css", authPublic},
		{"OPTIONS", "/v1/logs", authPublic},    // CORS preflight
		{"POST", "/mcp", authPublic},           // own X-MCP-API-Key auth
		{"GET", "/mcp/tools", authPublic},      // discovery
		{"POST", "/v1/logs", authAPIKey},       // ingest
		{"POST", "/v1/traces", authAPIKey},     // ingest
		{"POST", "/v1/rum/assets", authAPIKey}, // asset upload
		{"POST", "/v1/rum/client-token", authAPIKey},
		{"GET", "/v1/apps", authAPIKey},
		{"GET", "/v1/releases/abc/artifacts", authAPIKey},
		{"GET", "/api/tags/log/123", authAPIKey},
		{"POST", "/api/tags/log/123", authAPIKey},
		{"GET", "/v1/rum/assets/abc123", authBasic}, // the one UI route under /v1/
		{"GET", "/", authBasic},
		{"GET", "/logs", authBasic},
		{"POST", "/settings/ai", authBasic},
		{"GET", "/api/reports", authBasic},
		{"POST", "/api/notifications/check", authBasic},
	}
	for _, c := range cases {
		if got := classifyAuth(c.method, c.path); got != c.want {
			t.Errorf("classifyAuth(%s %s) = %d, want %d", c.method, c.path, got, c.want)
		}
	}
}

func TestAuthMode(t *testing.T) {
	cases := []struct {
		a    authConfig
		want string
	}{
		{authConfig{}, "none"},
		{authConfig{basicUser: "u", basicPass: "p"}, "basic"},
		{authConfig{basicUser: "u"}, "invalid"},                          // user without pass
		{authConfig{basicPass: "p"}, "invalid"},                          // pass without user
		{authConfig{externalURL: "http://x"}, "external"},                // external only
		{authConfig{externalURL: "http://x", basicUser: "u"}, "invalid"}, // both
	}
	for i, c := range cases {
		if got := c.a.mode(); got != c.want {
			t.Errorf("case %d: mode() = %q, want %q", i, got, c.want)
		}
	}
}

func TestEnvFlag(t *testing.T) {
	t.Setenv("SOBS_AUTH_FLAG_TEST", "")
	for _, v := range []string{"1", "true", "TRUE", "Yes", "on"} {
		t.Setenv("SOBS_AUTH_FLAG_TEST", v)
		if !envFlag("SOBS_AUTH_FLAG_TEST", false) {
			t.Errorf("envFlag(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"0", "false", "no", "off", "nonsense"} {
		t.Setenv("SOBS_AUTH_FLAG_TEST", v)
		if envFlag("SOBS_AUTH_FLAG_TEST", true) {
			t.Errorf("envFlag(%q) = true, want false", v)
		}
	}
}

func TestNormalizeOrigin(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"  https://A.com:8080 ": "https://a.com:8080",
		"http://Example.COM":    "http://example.com",
		"null":                  "",
		"not a url":             "",
	}
	for in, want := range cases {
		if got := normalizeOrigin(in); got != want {
			t.Errorf("normalizeOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameOriginRequest(t *testing.T) {
	mk := func(host, origin, referer string) *http.Request {
		r := httptest.NewRequest("POST", "http://"+host+"/x", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if referer != "" {
			r.Header.Set("Referer", referer)
		}
		return r
	}
	if !sameOriginRequest(mk("app.local", "http://app.local", "")) {
		t.Error("same Origin should pass")
	}
	if !sameOriginRequest(mk("app.local", "", "http://app.local/page")) {
		t.Error("same Referer should pass")
	}
	if sameOriginRequest(mk("app.local", "http://evil.com", "")) {
		t.Error("cross Origin should fail")
	}
}

// --- enforce* (no chdb needed: only s.auth is touched) ---

func newAuthServer(a authConfig) *server { return &server{auth: a} }

func TestEnforceUnconfiguredPassthrough(t *testing.T) {
	s := newAuthServer(authConfig{}) // nothing set -> all pass-through (parity invariant)
	for _, p := range []string{"/", "/logs", "/v1/logs", "/api/reports", "/health"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", p, nil)
		if s.enforceAuth(rec, req) {
			t.Errorf("unconfigured server blocked %s", p)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("unconfigured server wrote a body for %s", p)
		}
	}
}

func TestEnforceAPIKey(t *testing.T) {
	s := newAuthServer(authConfig{apiKey: "s3cr3t"})

	// Missing key -> 401.
	rec := httptest.NewRecorder()
	if !s.enforceAuth(rec, httptest.NewRequest("POST", "/v1/logs", nil)) {
		t.Fatal("expected block without key")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing key: code = %d, want 401", rec.Code)
	}

	// Wrong key -> 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/logs", nil)
	req.Header.Set("X-API-Key", "nope")
	if !s.enforceAuth(rec, req) || rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong key: blocked=%v code=%d", true, rec.Code)
	}

	// Correct key -> pass-through.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/logs", nil)
	req.Header.Set("X-API-Key", "s3cr3t")
	if s.enforceAuth(rec, req) {
		t.Error("correct key should pass")
	}

	// API key does not gate UI routes.
	rec = httptest.NewRecorder()
	if s.enforceAuth(rec, httptest.NewRequest("GET", "/logs", nil)) {
		t.Error("api-key config should not gate UI routes")
	}
}

func TestEnforceBasicAuth(t *testing.T) {
	s := newAuthServer(authConfig{basicUser: "admin", basicPass: "pw"})

	// No creds -> 401 + WWW-Authenticate.
	rec := httptest.NewRecorder()
	if !s.enforceAuth(rec, httptest.NewRequest("GET", "/logs", nil)) {
		t.Fatal("expected block without creds")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="SOBS"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}

	// Good creds -> pass.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/logs", nil)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:pw")))
	if s.enforceAuth(rec, req) {
		t.Error("good basic creds should pass")
	}

	// Ingest routes are not gated by basic config (no SOBS_API_KEY set).
	rec = httptest.NewRecorder()
	if s.enforceAuth(rec, httptest.NewRequest("POST", "/v1/logs", nil)) {
		t.Error("basic config should not gate ingest")
	}
}

func TestEnforceInvalidConfig(t *testing.T) {
	s := newAuthServer(authConfig{basicUser: "u"}) // user without pass -> invalid
	rec := httptest.NewRecorder()
	if !s.enforceAuth(rec, httptest.NewRequest("GET", "/logs", nil)) {
		t.Fatal("invalid config should block")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("code = %d, want 500", rec.Code)
	}
}

func TestEnforceCSRF(t *testing.T) {
	s := newAuthServer(authConfig{basicUser: "admin", basicPass: "pw", csrfCheck: true})
	creds := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:pw"))

	// Cross-origin write with valid creds -> 403 (CSRF checked before creds).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "http://app.local/settings/ai", nil)
	req.Host = "app.local"
	req.Header.Set("Origin", "http://evil.com")
	req.Header.Set("Authorization", creds)
	if !s.enforceAuth(rec, req) || rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin write: blocked code = %d, want 403", rec.Code)
	}

	// Same-origin write with valid creds -> pass.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "http://app.local/settings/ai", nil)
	req.Host = "app.local"
	req.Header.Set("Origin", "http://app.local")
	req.Header.Set("Authorization", creds)
	if s.enforceAuth(rec, req) {
		t.Error("same-origin write with creds should pass")
	}

	// GET is not subject to the CSRF check (still needs creds though).
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "http://app.local/logs", nil)
	req.Host = "app.local"
	req.Header.Set("Authorization", creds)
	if s.enforceAuth(rec, req) {
		t.Error("same-credential GET should pass")
	}
}
