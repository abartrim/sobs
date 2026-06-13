package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// serviceWorkerJS is app.py service_worker_js's fixed source string (extracted verbatim;
// it is a constant in the Python handler, not a static file).
//
//go:embed assets/service-worker.js
var serviceWorkerJS []byte

// maskingDefaultsJSON holds masking.py DEFAULT_SENSITIVE_KEYS/PATTERNS (extracted from the
// Python source of truth) — static security config, served by /api/settings/masking/rules.
//
//go:embed assets/masking_defaults.json
var maskingDefaultsJSON []byte

// signalLabelsJSON maps "source|signal" -> {label, description} (from app.py _SIGNAL_LABELS),
// powering the signal_label/signal_description Jinja globals.
//
//go:embed assets/signal_labels.json
var signalLabelsJSON []byte

// chartSpecTemplatesJSON is the static chart-spec template catalog (app.py CHART_TEMPLATES
// + _default_chart_spec), served by /api/dashboards/spec/templates.
//
//go:embed assets/chart_spec_templates.json
var chartSpecTemplatesJSON []byte

// GET /service-worker.js — fixed JS + push-notification headers (app.py:26461).
func (s *server) handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Service-Worker-Allowed", "/")
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(serviceWorkerJS)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(serviceWorkerJS)
}

// serveRumFile serves a file from StaticDir with an explicit content-type (the rum.* and
// service-worker routes set their own mimetype, unlike the ext-based default static
// handler). Cache-Control is the framework default; last-modified/expires are NOT emitted
// (non-reproducible, dropped by normalize). withEtag adds a content-hash ETag (sha256[:16],
// matching app.py _rum_etag) that IS compared.
func (s *server) serveRumFile(w http.ResponseWriter, r *http.Request, name, contentType string, withEtag bool, extra map[string]string) {
	data, err := os.ReadFile(filepath.Join(s.cfg.StaticDir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(staticMaxAge))
	if withEtag {
		sum := sha256.Sum256(data)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])[:16]+`"`)
	}
	for k, v := range extra {
		w.Header().Set(k, v)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *server) handleRumJS(w http.ResponseWriter, r *http.Request) {
	s.serveRumFile(w, r, "rum.js", "application/javascript; charset=utf-8", true,
		map[string]string{"X-SourceMap": "rum.js.map", "SourceMap": "rum.js.map"})
}

func (s *server) handleRumMinJS(w http.ResponseWriter, r *http.Request) {
	s.serveRumFile(w, r, "rum.min.js", "application/javascript; charset=utf-8", true, nil)
}

func (s *server) handleRumJSMap(w http.ResponseWriter, r *http.Request) {
	s.serveRumFile(w, r, "rum.js.map", "application/json", false, nil)
}

func (s *server) handleRumMinJSMap(w http.ResponseWriter, r *http.Request) {
	s.serveRumFile(w, r, "rum.min.js.map", "application/json", false, nil)
}

func (s *server) handleRumDTS(w http.ResponseWriter, r *http.Request) {
	// Note the doubled charset: Python sets mimetype "text/plain; charset=utf-8" and Quart
	// appends charset again. Reproduced exactly to match the golden.
	s.serveRumFile(w, r, "rum.d.ts", "text/plain; charset=utf-8; charset=utf-8", false, nil)
}

// methodNotAllowed405Body is Werkzeug/Quart's default 405 page (exact bytes).
const methodNotAllowed405Body = "<!doctype html>\n<html lang=en>\n<title>405 Method Not Allowed</title>\n" +
	"<h1>Method Not Allowed</h1>\n<p>The method is not allowed for the requested URL.</p>\n"

// v1 ingest endpoints are POST-only; a GET yields Quart's 405 with Allow: POST, OPTIONS.
// (The POST ingest branch lands with the OTLP work.)
func (s *server) handleV1IngestGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "POST, OPTIONS")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(methodNotAllowed405Body)))
	w.WriteHeader(http.StatusMethodNotAllowed)
	_, _ = w.Write([]byte(methodNotAllowed405Body))
}
