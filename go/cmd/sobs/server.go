package main

import (
	"log"
	"net/http"
	"strings"

	"github.com/sobs/sobs/internal/store"
)

// server is the root http.Handler. It owns the router and the response middleware that
// reproduces Python's @app.after_request _apply_security_headers (app.py:458). Getting
// this middleware byte-exact is a Phase-1/2 prerequisite: every response carries these
// headers, in this order, so parity_check.py compares them on every route.
type server struct {
	cfg config
	mux *http.ServeMux
	db  store.DB
}

func newServer(cfg config) *server {
	s := &server{cfg: cfg, mux: http.NewServeMux()}
	// Open the shared chdb session. Tolerate failure so non-DB routes still serve (and
	// so a missing libchdb only breaks data routes, not the whole server).
	if db, err := store.Open(cfg.DataDir); err != nil {
		log.Printf("warning: chdb open failed (%v) — data routes will error", err)
	} else {
		s.db = db
	}
	s.routes()
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &headerCapture{ResponseWriter: w, req: r, cfg: s.cfg}
	s.mux.ServeHTTP(rec, r)
}

func (s *server) routes() {
	// Health endpoint used by parity_check.py to detect readiness. Not a Python route;
	// excluded from the corpus comparison (it has its own manifest entry only if you
	// also add /healthz to the Python app — otherwise keep it out of routes.yaml).
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Phase 1: first real parity route. app.py: health() -> jsonify({...}).
	s.mux.HandleFunc("/health", s.handleHealth)

	// Phase 3: first data-backed route. app.py: health_db() -> SELECT 1 + status JSON.
	s.mux.HandleFunc("/health/db", s.handleHealthDB)

	// Phase 3: JSON API guard routes (feature-disabled returns in the parity state).
	s.mux.HandleFunc("/api/query/schema", s.handleApiQuerySchema)
	s.mux.HandleFunc("/api/table-explorer/tables", s.handleApiTableExplorerTables)
	s.mux.HandleFunc("/api/kubernetes/status", s.handleApiKubernetesStatus)
	s.mux.HandleFunc("/api/notifications/vapid-public-key", s.handleApiVapidPublicKey)

	// Phase 3: data-backed JSON list/aggregation routes (read the shared chdb fixture).
	s.mux.HandleFunc("/api/dashboards/list", s.handleApiDashboardsList)
	s.mux.HandleFunc("/api/reports", s.handleApiReports)
	s.mux.HandleFunc("/api/agent/runs", s.handleApiAgentRuns)
	s.mux.HandleFunc("/api/web-traffic/browsers", s.handleApiWebTrafficBrowsers)
	s.mux.HandleFunc("/api/web-traffic/os", s.handleApiWebTrafficOS)
	s.mux.HandleFunc("/api/web-traffic/timezones", s.handleApiWebTrafficTimezones)
	s.mux.HandleFunc("/api/web-traffic/languages", s.handleApiWebTrafficLanguages)
	s.mux.HandleFunc("/api/web-traffic/devices", s.handleApiWebTrafficDevices)
	s.mux.HandleFunc("/api/chart-types", s.handleApiChartTypes)
	s.mux.HandleFunc("/api/data-management/backup/list", s.handleApiDmBackupList)
	s.mux.HandleFunc("/api/ai/span-attributes", s.handleApiAiSpanAttributes)
	s.mux.HandleFunc("/api/enrichment/cve/findings", s.handleApiCveFindings)
	s.mux.HandleFunc("/api/web-traffic/geo", s.handleApiWebTrafficGeo)
	s.mux.HandleFunc("/api/dashboards/spec/options", s.handleApiDashboardsSpecOptions)
	s.mux.HandleFunc("/api/mcp/keys", s.handleApiMcpKeys)
	s.mux.HandleFunc("/api/settings/masking/rules", s.handleApiMaskingRules)

	// Dedicated static assets (explicit mimetypes / content-hash ETags) + service worker.
	s.mux.HandleFunc("/service-worker.js", s.handleServiceWorker)
	s.mux.HandleFunc("/static/rum.js", s.handleRumJS)
	s.mux.HandleFunc("/static/rum.min.js", s.handleRumMinJS)
	s.mux.HandleFunc("/static/rum.js.map", s.handleRumJSMap)
	s.mux.HandleFunc("/static/rum.min.js.map", s.handleRumMinJSMap)
	s.mux.HandleFunc("/static/rum.d.ts", s.handleRumDTS)
	// v1 ingest endpoints: GET -> 405 (POST ingest lands with OTLP).
	s.mux.HandleFunc("/v1/logs", s.handleV1IngestGet)
	s.mux.HandleFunc("/v1/metrics", s.handleV1IngestGet)
	s.mux.HandleFunc("/v1/traces", s.handleV1IngestGet)
	s.mux.HandleFunc("/v1/rum/assets", s.handleV1IngestGet)
	s.mux.HandleFunc("/v1/apps", s.handleV1Apps)
	s.mux.HandleFunc("/query", s.handleViewQuery)
	s.mux.HandleFunc("/table-explorer", s.handleViewTableExplorer)
	s.mux.HandleFunc("/kubernetes", s.handleViewKubernetes)
	s.mux.HandleFunc("/dashboards/new", s.handleNewDashboardForm)
	s.mux.HandleFunc("/dashboards", s.handleListDashboards)

	// Static assets — served byte-for-byte from static/ (Quart's default static endpoint).
	s.mux.HandleFunc("/static/", s.handleStatic)

	// Phase 2: all *_help pages (template engine), generated from the registry.
	for _, h := range helpRoutes {
		s.mux.HandleFunc(h.Path, s.handleHelpPage(h.Endpoint, h.Template))
	}

	// TODO (Phase 1+): register real handlers here, one per app.py @app.route.
	//   s.mux.HandleFunc("/", s.handleSummary)
	//   s.mux.HandleFunc("/api/...", s.handleX)
	// Static serving (Phase 1) — byte-identical to the committed static/ tree, with the
	// explicit ETag/X-SourceMap routes for rum.js* (see AUDIT.md §8).
}

// headerCapture wraps the ResponseWriter to apply the security headers via setdefault
// semantics (only set if the handler didn't already set them) — matching Quart's
// response.headers.setdefault(...). The order below MUST match app.py:458.
type headerCapture struct {
	http.ResponseWriter
	req         *http.Request
	cfg         config
	wroteHeader bool
}

func (h *headerCapture) WriteHeader(code int) {
	if !h.wroteHeader {
		h.applySecurityHeaders()
		h.wroteHeader = true
	}
	h.ResponseWriter.WriteHeader(code)
}

func (h *headerCapture) Write(b []byte) (int, error) {
	if !h.wroteHeader {
		h.applySecurityHeaders()
		h.wroteHeader = true
	}
	return h.ResponseWriter.Write(b)
}

func (h *headerCapture) applySecurityHeaders() {
	hdr := h.ResponseWriter.Header()
	setDefault(hdr, "X-Content-Type-Options", "nosniff")
	setDefault(hdr, "X-Frame-Options", "SAMEORIGIN")
	setDefault(hdr, "Referrer-Policy", "strict-origin-when-cross-origin")
	setDefault(hdr, "Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	setDefault(hdr, "Content-Security-Policy", "frame-ancestors 'self'; object-src 'none'; base-uri 'self'")
	// HSTS only in a secure context (Python: _request_is_secure_context). In parity the
	// test client is not secure, so HSTS is absent — match that.
	if isSecure(h.req) {
		setDefault(hdr, "Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}
	// OTLP CORS for /v1/* (app.py:_path_needs_otlp_cors) — TODO Phase 3.
	_ = strings.HasPrefix
}

func setDefault(h http.Header, k, v string) {
	if h.Get(k) == "" {
		h.Set(k, v)
	}
}

func isSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
