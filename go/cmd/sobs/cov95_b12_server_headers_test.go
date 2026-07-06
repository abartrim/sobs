package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// cov95_b12_server_headers_test.go — coverage-gate batch 12 targeted coverage for
// cmd/sobs/server.go's response-middleware plumbing that no existing test drove directly:
// headerCapture.Write / headerCapture.Flush (both apply-once-then-delegate paths), the
// appendVaryHeader token-dedup helper, and isSecure's X-Forwarded-Proto / behindTLS branches.

func TestHeaderCaptureWriteAppliesSecurityHeadersOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/health", nil)
	h := &headerCapture{ResponseWriter: rec, req: req, cfg: config{}}

	n, err := h.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != 5 {
		t.Fatalf("Write returned n=%d, want 5", n)
	}
	if !h.wroteHeader {
		t.Fatalf("wroteHeader = false after Write, want true")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff (security headers not applied)", got)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body = %q, want hello", rec.Body.String())
	}

	// A second Write must not re-apply the headers (already-set values are left alone via
	// setdefault semantics; wroteHeader guards a redundant call).
	rec.Header().Set("X-Frame-Options", "CUSTOM")
	if _, err := h.Write([]byte(" world")); err != nil {
		t.Fatalf("second Write returned error: %v", err)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "CUSTOM" {
		t.Fatalf("second Write overwrote X-Frame-Options: got %q, want CUSTOM (untouched)", got)
	}
}

func TestHeaderCaptureFlushAppliesSecurityHeadersOnceThenDelegates(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/tail", nil)
	h := &headerCapture{ResponseWriter: rec, req: req, cfg: config{}}

	h.Flush() // rec implements http.Flusher; must not panic and must apply headers
	if !h.wroteHeader {
		t.Fatalf("wroteHeader = false after Flush, want true")
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
		t.Fatalf("Referrer-Policy = %q, want strict-origin-when-cross-origin", got)
	}
	if !rec.Flushed {
		t.Fatalf("underlying ResponseRecorder.Flushed = false, want true (Flush must delegate)")
	}

	// A second Flush must not re-apply security headers (wroteHeader already true).
	rec.Header().Set("Permissions-Policy", "CUSTOM")
	h.Flush()
	if got := rec.Header().Get("Permissions-Policy"); got != "CUSTOM" {
		t.Fatalf("second Flush overwrote Permissions-Policy: got %q, want CUSTOM (untouched)", got)
	}
}

// nonFlusherWriter wraps httptest.ResponseRecorder but hides the http.Flusher method, so
// headerCapture.Flush's `if f, ok := ...` type-assertion false branch is exercised too.
type nonFlusherWriter struct{ http.ResponseWriter }

func TestHeaderCaptureFlushOnNonFlusherIsANoop(t *testing.T) {
	rec := httptest.NewRecorder()
	wrapped := &nonFlusherWriter{ResponseWriter: rec}
	req := httptest.NewRequest("GET", "/tail", nil)
	h := &headerCapture{ResponseWriter: wrapped, req: req, cfg: config{}}

	h.Flush() // must not panic even though wrapped is not an http.Flusher
	if !h.wroteHeader {
		t.Fatalf("wroteHeader = false after Flush, want true")
	}
	if rec.Flushed {
		t.Fatalf("underlying recorder Flushed = true, want false (wrapper hides Flusher)")
	}
}

// --- appendVaryHeader ----------------------------------------------------------------------

func TestAppendVaryHeaderOnEmpty(t *testing.T) {
	h := http.Header{}
	appendVaryHeader(h, "Origin")
	if got := h.Get("Vary"); got != "Origin" {
		t.Fatalf("Vary = %q, want Origin", got)
	}
}

func TestAppendVaryHeaderAppendsNewToken(t *testing.T) {
	h := http.Header{}
	h.Set("Vary", "Accept-Encoding")
	appendVaryHeader(h, "Origin")
	if got := h.Get("Vary"); got != "Accept-Encoding, Origin" {
		t.Fatalf("Vary = %q, want Accept-Encoding, Origin", got)
	}
}

func TestAppendVaryHeaderDedupesCaseInsensitive(t *testing.T) {
	h := http.Header{}
	h.Set("Vary", "origin, Accept-Encoding")
	appendVaryHeader(h, "Origin")
	if got := h.Get("Vary"); got != "origin, Accept-Encoding" {
		t.Fatalf("Vary = %q, want unchanged (case-insensitive dedup)", got)
	}
}

func TestAppendVaryHeaderSkipsBlankTokens(t *testing.T) {
	h := http.Header{}
	h.Set("Vary", "Origin, , Accept-Encoding")
	appendVaryHeader(h, "X-Custom")
	if got := h.Get("Vary"); got != "Origin, Accept-Encoding, X-Custom" {
		t.Fatalf("Vary = %q, want Origin, Accept-Encoding, X-Custom (blanks dropped)", got)
	}
}

// --- isSecure --------------------------------------------------------------------------------

func TestIsSecureViaXForwardedProto(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	if !isSecure(req) {
		t.Fatal("X-Forwarded-Proto: https should be secure")
	}
}

func TestIsSecureViaXForwardedProtoFirstOfCommaList(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Forwarded-Proto", "https,http")
	if !isSecure(req) {
		t.Fatal("X-Forwarded-Proto: https,http should be secure (first token wins)")
	}
	req2 := httptest.NewRequest("GET", "/x", nil)
	req2.Header.Set("X-Forwarded-Proto", "http,https")
	if isSecure(req2) {
		t.Fatal("X-Forwarded-Proto: http,https should NOT be secure (first token is http)")
	}
}

func TestIsSecureFalseWithNoTLSNoHeaderNoBehindTLS(t *testing.T) {
	if behindTLS {
		t.Skip("SOBS_BEHIND_TLS is set in this environment; the plain-HTTP case can't be isolated")
	}
	req := httptest.NewRequest("GET", "/x", nil)
	if isSecure(req) {
		t.Fatal("plain HTTP request with no forwarded-proto header should not be secure")
	}
}

func TestIsSecureCaseInsensitiveAndTrimmed(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Forwarded-Proto", "  HTTPS  ")
	if !isSecure(req) {
		t.Fatal("X-Forwarded-Proto: '  HTTPS  ' should be secure (case-insensitive, trimmed)")
	}
}
