package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cov95_b15_handlers_static_test.go — batch 15 coverage for cmd/sobs/handlers_static.go:
//   serveRumFile (100)    85.7%
//   handleRumJSMap (129)  33.3%
//   handleV1IngestGet (164) 64.7%

func TestServeRumFile_MissingFileIs404(t *testing.T) {
	s := &server{cfg: config{StaticDir: t.TempDir()}}
	r := httptest.NewRequest(http.MethodGet, "/does-not-exist.js", nil)
	rec := httptest.NewRecorder()
	s.serveRumFile(rec, r, "does-not-exist.js", "application/javascript", true, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestServeRumFile_WithEtagAndExtraHeaders(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rum.js"), []byte("console.log('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{StaticDir: dir}}
	r := httptest.NewRequest(http.MethodGet, "/rum.js", nil)
	rec := httptest.NewRecorder()
	s.serveRumFile(rec, r, "rum.js", "application/javascript; charset=utf-8", true,
		map[string]string{"X-SourceMap": "rum.js.map"})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "console.log('hi')" {
		t.Errorf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("expected an ETag header when withEtag=true")
	}
	if rec.Header().Get("X-SourceMap") != "rum.js.map" {
		t.Errorf("expected extra header X-SourceMap, got %q", rec.Header().Get("X-SourceMap"))
	}
	if got := rec.Header().Get("Cache-Control"); got == "" {
		t.Error("expected a Cache-Control header")
	}
}

func TestServeRumFile_NoEtagWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rum.js.map"), []byte(`{"version":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{StaticDir: dir}}
	r := httptest.NewRequest(http.MethodGet, "/rum.js.map", nil)
	rec := httptest.NewRecorder()
	s.serveRumFile(rec, r, "rum.js.map", "application/json", false, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("ETag") != "" {
		t.Errorf("expected no ETag when withEtag=false, got %q", rec.Header().Get("ETag"))
	}
}

func TestHandleRumJSMap_MissingIsEmptyBody404(t *testing.T) {
	s := &server{cfg: config{StaticDir: t.TempDir()}}
	r := httptest.NewRequest(http.MethodGet, "/rum.js.map", nil)
	rec := httptest.NewRecorder()
	s.handleRumJSMap(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("expected an EMPTY 404 body (not the default page), got %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Length") != "0" {
		t.Errorf("Content-Length = %q, want 0", rec.Header().Get("Content-Length"))
	}
}

func TestHandleRumJSMap_PresentServesFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rum.js.map"), []byte(`{"version":3,"sources":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{StaticDir: dir}}
	r := httptest.NewRequest(http.MethodGet, "/rum.js.map", nil)
	rec := httptest.NewRecorder()
	s.handleRumJSMap(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Body.String() != `{"version":3,"sources":[]}` {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleV1IngestGet
// ---------------------------------------------------------------------------

func TestHandleV1IngestGet_OptionsReturns204(t *testing.T) {
	s := &server{}
	for _, path := range []string{"/v1/logs", "/v1/metrics", "/v1/traces", "/v1/rum/assets"} {
		r := httptest.NewRequest(http.MethodOptions, path, nil)
		rec := httptest.NewRecorder()
		s.handleV1IngestGet(rec, r)
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s OPTIONS status = %d, want 204", path, rec.Code)
		}
		if rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("%s OPTIONS Content-Type = %q", path, rec.Header().Get("Content-Type"))
		}
	}
}

func TestHandleV1IngestGet_UnsupportedMethodIs405(t *testing.T) {
	s := &server{}
	r := httptest.NewRequest(http.MethodGet, "/v1/logs", nil)
	rec := httptest.NewRecorder()
	s.handleV1IngestGet(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Allow") != "OPTIONS, POST" {
		t.Errorf("Allow = %q, want %q", rec.Header().Get("Allow"), "OPTIONS, POST")
	}
	if rec.Body.String() != methodNotAllowed405Body {
		t.Errorf("body = %q, want the Werkzeug 405 page", rec.Body.String())
	}
}

func TestHandleV1IngestGet_PostDispatchesPerPath(t *testing.T) {
	s := &server{}
	// Empty OTLP bodies for logs/metrics/traces route through v1IngestOTLP and succeed with 0
	// records (already covered by cov95_b8_handlers_v1_ingest_test.go's OTLP tests); this proves
	// handleV1IngestGet's own PATH SWITCH dispatches each of the three ingest paths without
	// panicking or falling through to the default NotFound branch.
	for _, path := range []string{"/v1/logs", "/v1/metrics", "/v1/traces"} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		s.handleV1IngestGet(rec, r)
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s: unexpectedly fell through to the default NotFound branch", path)
		}
	}

	// /v1/rum/assets dispatches to handleRumAssetUpload, whose real-upload path is gated on
	// SOBS_RUM_ASSET_SIGNING_KEY; unset (the default in this test), it short-circuits to a 503
	// once past the body-required check (a non-empty body is needed to reach that branch).
	t.Setenv("SOBS_RUM_ASSET_SIGNING_KEY", "")
	r := httptest.NewRequest(http.MethodPost, "/v1/rum/assets", strings.NewReader("some-asset-bytes"))
	rec := httptest.NewRecorder()
	s.handleV1IngestGet(rec, r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/v1/rum/assets status = %d, want 503 (signing key unset); body=%s", rec.Code, rec.Body.String())
	}
}
