package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// cov95_b16_static_test.go — batch 16 targeted coverage for cmd/sobs/static.go's handleStatic:
// the success path (Content-Type/Content-Length/Cache-Control headers + body), the 404-missing
// branch, the 404-directory branch, and the path-traversal-rejected branch.

func TestHandleStaticServesFileWithHeaders(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log('hi');"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{StaticDir: dir}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/static/app.js", nil)
	s.handleStatic(w, r)

	if w.Code != 200 {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := w.Header().Get("Content-Length"); got != strconv.Itoa(len("console.log('hi');")) {
		t.Errorf("Content-Length = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=43200" {
		t.Errorf("Cache-Control = %q", got)
	}
	if w.Body.String() != "console.log('hi');" {
		t.Errorf("body = %q", w.Body.String())
	}
}

func TestHandleStaticMissingFile(t *testing.T) {
	s := &server{cfg: config{StaticDir: t.TempDir()}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/static/does-not-exist.css", nil)
	s.handleStatic(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleStaticDirectoryRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{StaticDir: dir}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/static/subdir", nil)
	s.handleStatic(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404 for a directory path, got %d", w.Code)
	}
}

func TestHandleStaticPathTraversalContained(t *testing.T) {
	dir := t.TempDir()
	// Write a secret file OUTSIDE the static root, then try to traverse to it. filepath.Clean("/"+
	// rel) normalizes ".." segments before joining, so the escape must be neutralized (typically
	// landing on a 404, never serving content from outside StaticDir).
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{StaticDir: dir}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/static/../../../../etc/passwd", nil)
	s.handleStatic(w, r)
	if w.Code == 200 {
		t.Fatalf("path traversal must not succeed, got 200 body=%q", w.Body.String())
	}
}

func TestHandleStaticUnknownExtensionOmitsContentType(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.xyz"), []byte("blob"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{cfg: config{StaticDir: dir}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/static/data.xyz", nil)
	s.handleStatic(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "" {
		t.Errorf("want no explicit Content-Type for an unmapped extension, got %q", got)
	}
}
