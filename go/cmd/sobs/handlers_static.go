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

// dashboardViewTemplatesJSON is the chart-template catalog in the shape app.py
// view_custom_dashboard builds (CHART_TEMPLATES sorted by id, each with id/name/description/
// icon/query_shape/sample_sql/drilldown/default_spec) — embedded into the dashboard view page.
//
//go:embed assets/dashboard_view_templates.json
var dashboardViewTemplatesJSON []byte

// field-hints static config (operators/keywords/functions/snippets/fields) — the
// query-derived parts (attr_keys/tag_keys/tag_values) are computed in the handler.
//
//go:embed assets/logs_field_hints_static.json
var logsFieldHintsStaticJSON []byte

//go:embed assets/ai_field_hints_static.json
var aiFieldHintsStaticJSON []byte

// errorSourcesSQL is app.py ERROR_SOURCES_SQL (the otel_logs ∪ hyperdx_sessions error
// subquery) used by the summary/errors pages.
//
//go:embed assets/error_sources.sql
var errorSourcesSQL string

// mcpToolsJSON is the static MCP tools list response (mcp.py MCP_TOOLS via jsonify).
//
//go:embed assets/mcp_tools.json
var mcpToolsJSON []byte

// AI settings page static data: the 24 ai.* setting keys (all default ""), and the
// pricing catalogs (default/saved/sources) — extracted from app.py for /settings/ai.
//
//go:embed assets/ai_setting_keys.json
var aiSettingKeysJSON []byte

//go:embed assets/default_ai_pricing.json
var defaultAiPricingJSON []byte

//go:embed assets/saved_ai_pricing.json
var savedAiPricingJSON []byte

//go:embed assets/ai_pricing_sources.json
var aiPricingSourcesJSON []byte

// errorIDExpr is app.py _error_id_sql_expr() — the stable ErrorId MD5 expression.
const errorIDExpr = "lower(hex(MD5(concat(toString(Timestamp), '|', ServiceName, '|', " +
	"if(mapContains(LogAttributes, 'exception.type'), LogAttributes['exception.type'], 'Error'), '|', " +
	"if(mapContains(LogAttributes, 'exception.message'), LogAttributes['exception.message'], Body), '|', " +
	"TraceId, '|', SpanId))))"

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
	// app.py rum_js_map: if the .map is absent, return ("", 404) — an EMPTY body, not Werkzeug's
	// default 404 page. serveRumFile's os.ReadFile-miss falls through to http.NotFound (the
	// default page), so guard the existence check here to emit the empty-body 404 Python does.
	if _, err := os.Stat(filepath.Join(s.cfg.StaticDir, "rum.js.map")); err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNotFound)
		return
	}
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

// v1 ingest endpoints are POST-only; a GET yields Quart's 405. Werkzeug sorts the allowed
// methods alphabetically, so the Allow header is "OPTIONS, POST" (not "POST, OPTIONS").
//
// These four paths (/v1/{logs,traces,metrics}, /v1/rum/assets) are app.py's ingest_preflight
// routes: they declare methods=["OPTIONS"] explicitly, so OPTIONS is dispatched to the view
// (not auto-answered by Werkzeug) and the view returns ("", 204). route_guard.go keeps these in
// explicitOptionsRoutes so OPTIONS reaches this handler; reproduce the 204 here. (CORS headers
// are layered on by the OTLP after_request hook regardless.)
func (s *server) handleV1IngestGet(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodPost {
		switch r.URL.Path {
		case "/v1/logs":
			s.v1IngestOTLP(w, r, "resourceLogs", "scopeLogs", "logRecords")
		case "/v1/metrics":
			s.v1IngestOTLP(w, r, "resourceMetrics", "scopeMetrics", "metrics")
		case "/v1/traces":
			s.v1IngestOTLP(w, r, "resourceSpans", "scopeSpans", "spans")
		case "/v1/rum/assets":
			// ingest_rum_asset. Real upload path is gated on SOBS_RUM_ASSET_SIGNING_KEY being set;
			// when unset (the corpus fixture) it short-circuits to the same 503
			// "Asset upload signing key is not configured" byte-for-byte.
			s.handleRumAssetUpload(w, r)
		default:
			// Unreachable: this handler is registered only for the four paths above.
			http.NotFound(w, r)
		}
		return
	}
	w.Header().Set("Allow", "OPTIONS, POST")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(methodNotAllowed405Body)))
	w.WriteHeader(http.StatusMethodNotAllowed)
	_, _ = w.Write([]byte(methodNotAllowed405Body))
}
