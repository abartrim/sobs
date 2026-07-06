package main

import (
	"net/http/httptest"
	"strconv"
	"testing"
)

// ---- posixBasename ------------------------------------------------------------------------------

func TestPosixBasename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a/b/c.js", "c.js"},
		{"c.js", "c.js"},
		{"", ""},
		{"a/b/", ""},
		{"/root", "root"},
		{`a\b\c.js`, `a\b\c.js`}, // backslash NOT treated as separator (POSIX semantics)
	}
	for _, c := range cases {
		if got := posixBasename(c.in); got != c.want {
			t.Errorf("posixBasename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- sanitizeRumAssetName -----------------------------------------------------------------------

func TestSanitizeRumAssetName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "asset"},
		{"  ", "asset"},
		{"path/to/my file!.png", "my-file-.png"},
		{"...", "asset"}, // strips to nothing after trimming "-._"
		{"valid_name-1.2.3", "valid_name-1.2.3"},
		{"  spaced.js  ", "spaced.js"},
	}
	for _, c := range cases {
		if got := sanitizeRumAssetName(c.in); got != c.want {
			t.Errorf("sanitizeRumAssetName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- sanitizeRumAssetType -----------------------------------------------------------------------

func TestSanitizeRumAssetType(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "asset"},
		{"  ", "asset"},
		{"Screenshot", "screenshot"},
		{"video/webm!!", "video-webm"},
		{"...", "asset"},
		{"  Trace_Blob  ", "trace_blob"},
	}
	for _, c := range cases {
		if got := sanitizeRumAssetType(c.in); got != c.want {
			t.Errorf("sanitizeRumAssetType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- assetExtension ------------------------------------------------------------------------------
// (assetExtension's straightforward cases are already covered by TestAssetExtension in
// mcp_rum_misc_helpers_test.go; this adds the remaining untested edge cases: webp/webm mappings,
// a content-type parameter suffix, and the "extension too long" / "bare dot" fallback branches.)

func TestAssetExtension_EdgeCases(t *testing.T) {
	cases := []struct{ name, contentType, want string }{
		{"noext", "image/webp", "webp"},
		{"noext", "video/webm", "webm"},
		{"noext", "text/plain; charset=utf-8", "txt"},         // content-type param stripped
		{"file.toolongextension", "application/json", "json"}, // ext too long -> falls to content-type map
		{"file.", "application/json", "json"},                 // bare dot doesn't match rumAssetExtRe
	}
	for _, c := range cases {
		if got := assetExtension(c.name, c.contentType); got != c.want {
			t.Errorf("assetExtension(%q,%q) = %q, want %q", c.name, c.contentType, got, c.want)
		}
	}
}

// ---- headerOr ------------------------------------------------------------------------------------

func TestHeaderOr(t *testing.T) {
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Present", "value")
	if got := headerOr(req, "X-Present", "def"); got != "value" {
		t.Errorf("headerOr(present) = %q", got)
	}
	if got := headerOr(req, "X-Missing", "def"); got != "def" {
		t.Errorf("headerOr(missing) = %q", got)
	}
	req.Header.Set("X-Empty", "")
	if got := headerOr(req, "X-Empty", "def"); got != "def" {
		t.Errorf("headerOr(empty) = %q, want default (empty counts as absent)", got)
	}
}

// ---- rumAssetQueryDefault ------------------------------------------------------------------------

func TestRumAssetQueryDefault(t *testing.T) {
	req := httptest.NewRequest("GET", "/x?type=screenshot&empty=", nil)
	if got := rumAssetQueryDefault(req, "type", "asset"); got != "screenshot" {
		t.Errorf("present param = %q", got)
	}
	if got := rumAssetQueryDefault(req, "missing", "asset"); got != "asset" {
		t.Errorf("missing param = %q, want default", got)
	}
	if got := rumAssetQueryDefault(req, "empty", "asset"); got != "" {
		t.Errorf("present-but-empty param = %q, want empty string (default only applies when absent)", got)
	}
}

// (rumAssetSignature and rumAssetSignaturePayload already have dedicated tests in
// remaining_pure_helpers_test.go / mcp_rum_misc_helpers_test.go respectively.)

// ---- intAbs64 --------------------------------------------------------------------------------------

func TestIntAbs64(t *testing.T) {
	if got := intAbs64(-5); got != 5 {
		t.Errorf("intAbs64(-5) = %d", got)
	}
	if got := intAbs64(5); got != 5 {
		t.Errorf("intAbs64(5) = %d", got)
	}
	if got := intAbs64(0); got != 0 {
		t.Errorf("intAbs64(0) = %d", got)
	}
}

// ---- verifyRumAssetSignature --------------------------------------------------------------------

func TestVerifyRumAssetSignature(t *testing.T) {
	t.Run("signing key unconfigured yields 503-style error", func(t *testing.T) {
		s := &server{rumAsset: rumAssetConfig{}}
		req := httptest.NewRequest("POST", "/v1/rum/assets", nil)
		ok, msg := s.verifyRumAssetSignature(req, []byte("body"), "POST", "/v1/rum/assets", "text/plain", "asset", "a.txt")
		if ok || msg != "Asset upload signing key is not configured" {
			t.Errorf("got (%v,%q)", ok, msg)
		}
	})

	t.Run("missing signature headers", func(t *testing.T) {
		s := &server{rumAsset: rumAssetConfig{signingKey: "k", signWindowSec: 300}}
		req := httptest.NewRequest("POST", "/v1/rum/assets", nil)
		ok, msg := s.verifyRumAssetSignature(req, []byte("body"), "POST", "/v1/rum/assets", "text/plain", "asset", "a.txt")
		if ok || msg != "Missing asset signature headers" {
			t.Errorf("got (%v,%q)", ok, msg)
		}
	})

	t.Run("invalid timestamp format", func(t *testing.T) {
		s := &server{rumAsset: rumAssetConfig{signingKey: "k", signWindowSec: 300}}
		req := httptest.NewRequest("POST", "/v1/rum/assets", nil)
		req.Header.Set("X-SOBS-Asset-Timestamp", "not-a-number")
		req.Header.Set("X-SOBS-Asset-Signature", "abc")
		ok, msg := s.verifyRumAssetSignature(req, []byte("body"), "POST", "/v1/rum/assets", "text/plain", "asset", "a.txt")
		if ok || msg != "Invalid asset signature timestamp" {
			t.Errorf("got (%v,%q)", ok, msg)
		}
	})

	t.Run("timestamp outside allowed window", func(t *testing.T) {
		s := &server{rumAsset: rumAssetConfig{signingKey: "k", signWindowSec: 10}}
		req := httptest.NewRequest("POST", "/v1/rum/assets", nil)
		oldTs := nowUTC().Unix() - 10000
		req.Header.Set("X-SOBS-Asset-Timestamp", itoa64(oldTs))
		req.Header.Set("X-SOBS-Asset-Signature", "abc")
		ok, msg := s.verifyRumAssetSignature(req, []byte("body"), "POST", "/v1/rum/assets", "text/plain", "asset", "a.txt")
		if ok || msg != "Asset signature timestamp outside allowed window" {
			t.Errorf("got (%v,%q)", ok, msg)
		}
	})

	t.Run("invalid signature rejected", func(t *testing.T) {
		s := &server{rumAsset: rumAssetConfig{signingKey: "k", signWindowSec: 300}}
		req := httptest.NewRequest("POST", "/v1/rum/assets", nil)
		ts := nowUTC().Unix()
		req.Header.Set("X-SOBS-Asset-Timestamp", itoa64(ts))
		req.Header.Set("X-SOBS-Asset-Signature", "0000000000000000000000000000000000000000000000000000000000000000")
		ok, msg := s.verifyRumAssetSignature(req, []byte("body"), "POST", "/v1/rum/assets", "text/plain", "asset", "a.txt")
		if ok || msg != "Invalid asset signature" {
			t.Errorf("got (%v,%q)", ok, msg)
		}
	})

	t.Run("valid signature accepted (window defaults to >=1 when configured 0)", func(t *testing.T) {
		s := &server{rumAsset: rumAssetConfig{signingKey: "topsecret", signWindowSec: 0}}
		req := httptest.NewRequest("POST", "/v1/rum/assets", nil)
		ts := nowUTC().Unix()
		body := []byte("hello world")
		bodySum := shaHexForTest(body)
		payload := rumAssetSignaturePayload("POST", "/v1/rum/assets", itoa64(ts), bodySum, "text/plain", "asset", "a.txt")
		sig := rumAssetSignature("topsecret", payload)
		req.Header.Set("X-SOBS-Asset-Timestamp", itoa64(ts))
		req.Header.Set("X-SOBS-Asset-Signature", sig)
		ok, msg := s.verifyRumAssetSignature(req, body, "POST", "/v1/rum/assets", "text/plain", "asset", "a.txt")
		if !ok || msg != "" {
			t.Errorf("got (%v,%q), want success", ok, msg)
		}
	})
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}

func shaHexForTest(b []byte) string {
	// Mirrors the sha256 hex computation done inside verifyRumAssetSignature, using the same
	// stdlib primitives so the test's expected signature matches exactly.
	sum := sha256Sum(b) // cve_scan.go's sha256Sum already returns a hex string of sha256(b)
	return sum
}
