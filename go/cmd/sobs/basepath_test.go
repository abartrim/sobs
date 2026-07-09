package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store/storetest"
)

func TestNormalizeBasePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"/", ""},
		{"   ", ""},
		{"sobs", "/sobs"},
		{"/sobs", "/sobs"},
		{"/sobs/", "/sobs"},
		{"//sobs//nested//", "/sobs/nested"},
		{"  /sobs  ", "/sobs"},
		// CWE-601: a leading "/\" is browser-protocol-relative just like "//" (CodeQL
		// go/bad-redirect-check) — reject it rather than let it flow into a redirect Location.
		{"/\\evil.com", ""},
		{"\\evil.com", ""},
		{"/\\\\evil.com", ""},
	}
	for _, c := range cases {
		if got := normalizeBasePath(c.in); got != c.want {
			t.Errorf("normalizeBasePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEffectiveBasePath(t *testing.T) {
	s := &server{cfg: config{BasePath: "/sobs"}}

	// X-Forwarded-Prefix, when present, overrides the static SOBS_BASE_PATH.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-Prefix", "/insights")
	if got := s.effectiveBasePath(r); got != "/insights" {
		t.Errorf("effectiveBasePath with header = %q, want /insights", got)
	}

	// No header -> falls back to the static config.
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := s.effectiveBasePath(r2); got != "/sobs" {
		t.Errorf("effectiveBasePath without header = %q, want /sobs", got)
	}

	// Header present but empty/root -> falls back too (normalizes to "").
	r3 := httptest.NewRequest(http.MethodGet, "/", nil)
	r3.Header.Set("X-Forwarded-Prefix", "/")
	if got := s.effectiveBasePath(r3); got != "/sobs" {
		t.Errorf("effectiveBasePath with root header = %q, want /sobs (fallback)", got)
	}
}

func TestApplyBasePath(t *testing.T) {
	s := &server{cfg: config{BasePath: "/sobs"}}

	cases := []struct {
		name      string
		path      string
		forwarded string // X-Forwarded-Prefix header, if any
		wantPath  string
	}{
		{
			name:     "no base path configured: path untouched",
			path:     "/logs",
			wantPath: "/logs",
		},
		{
			name:     "proxy kept prefix, exact match on root",
			path:     "/sobs",
			wantPath: "/",
		},
		{
			name:     "proxy kept prefix on a sub-path: stripped for routing",
			path:     "/sobs/static/app.css",
			wantPath: "/static/app.css",
		},
		{
			name:     "proxy already stripped the prefix: path passes through unchanged",
			path:     "/static/app.css",
			wantPath: "/static/app.css",
		},
		{
			name:      "X-Forwarded-Prefix overrides the static base for stripping too",
			path:      "/insights/static/app.css",
			forwarded: "/insights",
			wantPath:  "/static/app.css",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := s
			if c.name == "no base path configured: path untouched" {
				srv = &server{cfg: config{BasePath: ""}}
			}
			r := httptest.NewRequest(http.MethodGet, c.path, nil)
			if c.forwarded != "" {
				r.Header.Set("X-Forwarded-Prefix", c.forwarded)
			}
			got := srv.applyBasePath(r)
			if got.URL.Path != c.wantPath {
				t.Errorf("applyBasePath(%q).URL.Path = %q, want %q", c.path, got.URL.Path, c.wantPath)
			}
		})
	}
}

// TestRenderPage_ForwardedPrefixRewritesLinks exercises the full chain a reverse-proxy
// deployment depends on: renderPage -> newEngine(r) -> effectiveBasePath(r) -> url_for,
// mirroring app.py's test_forwarded_prefix_generates_prefixed_links. Regression test for the
// bug reported behind pab-admin's reverse proxy: static asset links weren't rewritten to the
// proxied URL because this whole chain read the request-independent SOBS_BASE_PATH only.
func TestRenderPage_ForwardedPrefixRewritesLinks(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Forwarded-Prefix", "/sobs")
	s.renderPage(w, r, "query.html", "view_query", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/sobs/logs"`) {
		t.Errorf("expected X-Forwarded-Prefix-aware nav link href=\"/sobs/logs\", got body without it:\n%s", body)
	}
	if !strings.Contains(body, `src="/sobs/static/bootstrap.bundle.min.js"`) {
		t.Errorf("expected X-Forwarded-Prefix-aware static asset link, got body without it:\n%s", body)
	}

	// Root mode (no header, no static config) must still emit unprefixed links — mirrors
	// app.py's test_root_mode_uses_root_relative_links.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	s.renderPage(w2, r2, "query.html", "view_query", nil)
	body2 := w2.Body.String()
	if !strings.Contains(body2, `href="/logs"`) {
		t.Errorf("expected unprefixed nav link href=\"/logs\" in root mode, got:\n%s", body2)
	}
	if strings.Contains(body2, "/sobs/") {
		t.Errorf("root-mode render leaked a /sobs/ prefixed link from the prior request:\n%s", body2)
	}
}
