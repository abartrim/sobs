package main

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// This file ports app.py's BasePathMiddleware (added in "Fix #14: add base-path aware URL
// generation and routing") — reverse-proxy base-path support was documented in README.md
// ("For reverse proxies, SOBS also honors X-Forwarded-Prefix for URL generation and prefixed
// routing") but never actually implemented in the Go rewrite: url_for/redirect targets used
// only the static SOBS_BASE_PATH env var (see render.go/fix_forms.go's old direct
// s.cfg.BasePath reads), and incoming requests were never stripped of a proxy-forwarded
// prefix at all. Caught testing behind pab-admin's reverse proxy: static asset links weren't
// rewritten to the proxied URL.

var repeatedSlashes = regexp.MustCompile(`/+`)

// normalizeBasePath mirrors app.py's _normalize_base_path: collapse repeated slashes, ensure a
// single leading slash and no trailing slash; "" and "/" both normalize to "" (no base path).
//
// This is the single chokepoint both effectiveBasePath (redirects, url_for-generated links) and
// applyBasePath (inbound routing) go through, so it's also where an X-Forwarded-Prefix value —
// attacker-controlled if SOBS is ever reached without a trusted proxy overwriting that header —
// must be rejected as an open-redirect vector (CWE-601) rather than passed through. Collapsing
// repeated slashes above already defuses the classic "//evil.com" case, but browsers also treat
// a leading "/\" as protocol-relative (e.g. "/\evil.com" parses like "//evil.com"), so that
// pattern needs an explicit reject too — CodeQL go/bad-redirect-check flagged exactly this gap.
func normalizeBasePath(v string) string {
	v = repeatedSlashes.ReplaceAllString(strings.TrimSpace(v), "/")
	if v == "" || v == "/" {
		return ""
	}
	if !strings.HasPrefix(v, "/") {
		v = "/" + v
	}
	// A leading-slash check alone is an incomplete redirect-target guard (CWE-601): browsers treat
	// both "//" and "/\" as protocol-relative absolute URLs. repeatedSlashes above already makes
	// "//" unreachable here, but check both together explicitly so this guard is self-evidently
	// complete rather than relying on that as an implicit invariant.
	if len(v) < 2 || v[0] != '/' || v[1] == '/' || v[1] == '\\' {
		return ""
	}
	v = strings.TrimSuffix(v, "/")
	if v == "" || v == "/" {
		return ""
	}
	return v
}

// effectiveBasePath resolves the base path to use for THIS request: an X-Forwarded-Prefix
// header (dynamic, set by a reverse proxy per-request) takes priority over the static
// SOBS_BASE_PATH the server started with — mirrors app.py's
// "effective_base = forwarded or BASE_PATH".
func (s *server) effectiveBasePath(r *http.Request) string {
	if fwd := normalizeBasePath(r.Header.Get("X-Forwarded-Prefix")); fwd != "" {
		return fwd
	}
	return s.cfg.BasePath // normalized once at startup in loadConfig
}

// applyBasePath mirrors app.py's BasePathMiddleware.__call__: if the incoming request still
// carries the base-path prefix (the proxy passed the raw path through unstripped), strip it
// so the app's own routes — which are only ever registered bare, e.g. "/static/" — match.
// Requests where the proxy already stripped the prefix before forwarding pass through
// unchanged: effectiveBasePath(r) (used for OUTBOUND link generation via url_for/redirects)
// reads X-Forwarded-Prefix directly, independent of what this does to r.URL.Path.
func (s *server) applyBasePath(r *http.Request) *http.Request {
	base := s.effectiveBasePath(r)
	if base == "" {
		return r
	}
	p := r.URL.Path
	var newPath string
	switch {
	case p == base:
		newPath = "/"
	case strings.HasPrefix(p, base+"/"):
		newPath = strings.TrimPrefix(p, base)
	default:
		return r // proxy already stripped the prefix; nothing to do for routing
	}
	// Same shallow-copy approach as stdlib's http.StripPrefix: clone Request and URL (not a
	// deep Clone) so concurrent handling of other requests never observes this mutation.
	r2 := new(http.Request)
	*r2 = *r
	r2.URL = new(url.URL)
	*r2.URL = *r.URL
	r2.URL.Path = newPath
	if rp := r.URL.RawPath; rp != "" {
		if rp == base {
			r2.URL.RawPath = "/"
		} else {
			r2.URL.RawPath = strings.TrimPrefix(rp, base)
		}
	}
	return r2
}
