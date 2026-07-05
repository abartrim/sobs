package main

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/store"
)

// server is the root http.Handler. It owns the router and the response middleware that
// reproduces Python's @app.after_request _apply_security_headers (app.py:458). Getting
// this middleware byte-exact is a Phase-1/2 prerequisite: every response carries these
// headers, in this order, so parity_check.py compares them on every route.
type server struct {
	cfg       config
	mux       *http.ServeMux
	db        store.DB
	sse       *sseBroker
	auth      authConfig
	wq        writeQueuer
	tel       *telemetry
	rumClient rumClientConfig
	rumAsset  rumAssetConfig
	srcMap    *sourceMapper
}

func newServer(cfg config) *server {
	s := &server{cfg: cfg, mux: http.NewServeMux(), sse: newSSEBroker(), auth: loadAuthConfig(), tel: loadTelemetry(), rumClient: loadRumClientConfig(), rumAsset: loadRumAssetConfig(), srcMap: loadSourceMapper()}
	// Open the shared chdb session, retrying the intermittent embedded-server "recursive_mutex
	// lock failed" boot error (a chdb-go contention bug seen under many sequential per-profile
	// boots). A server that starts but can't open chdb would panic every route that touches the
	// store, so exit after exhausting retries — in parity mode (SOBS_PARITY) the harness's
	// boot-retry then re-spawns a fresh process, which clears the global chdb state; outside
	// parity mode this is just a normal fail-fast startup failure (see below).
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		db, err := openStore(cfg)
		if err == nil {
			s.db = db
			break
		}
		lastErr = err
		log.Printf("warning: chdb open failed (attempt %d: %v) — retrying", attempt+1, err)
		time.Sleep(time.Duration(300*(attempt+1)) * time.Millisecond)
	}
	if s.db == nil {
		// A nil s.db would panic (or otherwise misbehave) the first time any of the ~60 route
		// handlers calls s.db.Execute — none of them nil-guard it; only /health/db checks s.db
		// itself, by design, since its whole job is to report DB health without crashing (see
		// handleHealthDB). Outside parity mode there is no legitimate reason to keep serving
		// with no store at all, so fail fast here as soon as the retries are exhausted, the same
		// way the old parity-only branch already did and the way validateChdbStartup (just below)
		// already does unconditionally: log and exit non-zero rather than start a server that
		// cannot do its job. This subsumes the previous "if SOBS_PARITY" special case — both
		// paths now exit, so it is written once.
		log.Fatalf("chdb open failed after retries, cannot start: %v", lastErr)
	}
	// When SOBS_CHDB_ENCRYPTION_KEY configured an encrypted disk/policy, assert chdb actually
	// applied it (no-op otherwise). A misapplied config-file would silently fall back to plain disk.
	if err := s.validateChdbStartup(); err != nil {
		log.Fatalf("%v", err)
	}
	// Self-initialize the schema on a fresh store ("make if not found"); a strict no-op when the
	// store already has it (the seeded parity fixture), so parity is unaffected.
	s.ensureSchema()
	// Apply raw-metrics retention TTLs (app.py _ensure_raw_metrics_retention, called from the
	// post-schema init path _ensure_post_schema_state). Gated to NOT run under parity so the
	// frozen 2024 fixture rows are never dropped by a materializing ALTER; in real runtime it
	// applies the same baseline/pinned TTLs as Python.
	s.applyRawMetricsRetention()
	// Seed the app/release/artifact registry from SOBS_APP_REGISTRY_SEED_JSON (no-op when unset).
	s.seedAppRegistry()
	// Background DB writer (app.py _ensure_write_worker), via the newWriteQueue provider seam
	// (providers.go) — the writeQueuer analog of openStore/authGate. The writer's ops use s.db, so
	// start it only after the store is opened. Under parity, ingest writes are awaited
	// (commit-before-ack).
	s.wq = newWriteQueue(cfg)
	// Periodic background workers (app.py before_serving asyncio tasks). Real-runtime only — see
	// background_tasks.go. Currently: the raw-metrics window-copy worker.
	s.startBackgroundWorkers()
	s.routes()
	return s
}

// enqueueWrite routes an ingest write through the background DB writer (app.py _queue_write). The
// wait flag mirrors Python's `wait = app.config["TESTING"]`: under the parity harness writes are
// awaited so the response is deterministic and commit-before-ack (parity unchanged); in normal
// runtime the write is acked once queued. Returns errWriteQueueFull when the queue is saturated.
func (s *server) enqueueWrite(op func() error) error {
	return s.wq.enqueue(op, s.cfg.Parity)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &headerCapture{ResponseWriter: w, req: r, cfg: s.cfg}
	// Optional auth layers (no-ops unless SOBS_API_KEY / SOBS_BASIC_AUTH_* / SOBS_EXTERNAL_AUTH_URL
	// are set), written through rec so blocking responses still carry the security headers. Routed
	// through the authGate seam so an alternate build can substitute a different authenticator.
	if authGate(s, rec, r) {
		return
	}
	s.mux.ServeHTTP(rec, r)
}

func (s *server) routes() {
	// Health endpoint used by parity_check.py to detect readiness. Not a Python route;
	// excluded from the corpus comparison (it has its own manifest entry only if you
	// also add /healthz to the Python app — otherwise keep it out of routes.yaml).
	s.route("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Phase 1: first real parity route. app.py: health() -> jsonify({...}).
	s.route("/health", s.handleHealth)

	// Phase 3: first data-backed route. app.py: health_db() -> SELECT 1 + status JSON.
	s.route("/health/db", s.handleHealthDB)

	// Phase 3: JSON API guard routes (feature-disabled returns in the parity state).
	s.route("/api/query/schema", s.handleApiQuerySchema)
	s.route("/api/query/ask", s.handleApiQueryAsk)
	s.route("/api/query/run", s.handleApiQueryRun)
	s.route("/api/query/refine-chart", s.handleApiQueryRefineChart)
	s.route("/api/query/add-to-dashboard", s.handleApiQueryAddToDashboard)
	s.route("/api/ai/helper", s.handleApiAiHelper)
	s.route("/api/ai/helper/actions/execute", s.handleApiAiHelperExecute)
	s.route("/api/ai/helper/feedback", s.handleApiAiHelperFeedback)
	s.route("/api/dashboards/query", s.handleApiDashboardsQuery)
	s.route("/api/dashboards/render", s.handleApiDashboardsRender)
	s.route("/api/dashboards/spec/ai-build", s.handleApiDashboardsSpecAiBuild)
	s.route("/api/dashboards/spec/compile", s.handleApiDashboardsSpecCompile)
	s.route("/api/dashboards/spec/dry-run", s.handleApiDashboardsSpecDryRun)
	s.route("/api/dashboards/spec/render", s.handleApiDashboardsSpecRender)
	s.route("/api/dashboards/spec/validate", s.handleApiDashboardsSpecValidate)
	s.route("/api/notifications/subscribe", s.handleApiNotificationsSubscribe)
	s.route("/api/notifications/check", s.handleApiNotificationsCheck)
	s.route("/api/notifications/vapid-keygen", s.handleApiNotificationsVapidKeygen)
	s.route("/api/notifications/rules/auto-generate", s.handleApiNotificationsAutoGenerate)
	s.route("/api/enrichment/cve/scan", s.handleApiEnrichmentCveScan)
	s.route("/api/onboarding/create-issues", s.handleApiOnboardingCreateIssues)
	s.route("/api/onboarding/create-repo", s.handleApiOnboardingCreateRepo)
	s.route("/api/onboarding/import-repo", s.handleApiOnboardingImportRepo)
	s.route("/api/onboarding/list-repos", s.handleApiOnboardingListRepos)
	s.route("/api/issues/raise", s.handleApiIssuesRaise)
	s.route("/api/mcp/enabled", s.handleApiMcpEnabled)
	s.route("/api/settings/masking/preview", s.handleApiSettingsMaskingPreview)
	s.route("/api/data-management/prune", s.handleApiDataManagementPrune)
	s.route("/api/mcp/keys/", s.handleMcpKeyByID)
	s.route("/api/notifications/vapid-keys", s.handleVapidKeys)
	s.route("/api/reports/", s.handleReportsSub)
	s.route("/api/agent/runs/", s.handleAgentRunSub)
	s.route("/api/notifications/channels/", s.handleChannelSub)
	s.route("/api/enrichment/cve/findings/", s.handleCveDispositionSub)
	s.route("/api/dashboards/", s.handleDashboardSub)
	s.route("/errors/", s.handleErrorSub)
	s.route("/reports/", s.handleReportsFormSub)
	s.route("/settings/notifications/channels", s.handleNotifChannelsCreate)
	s.route("/settings/notifications/rules", s.handleNotifRulesCreate)
	s.route("/settings/masking/keys", s.handleMaskingKeysCreate)
	s.route("/settings/masking/patterns", s.handleMaskingPatternsCreate)
	s.route("/settings/masking/keys/delete", s.handleMaskingKeysDelete)
	s.route("/settings/masking/patterns/delete", s.handleMaskingPatternsDelete)
	s.route("/settings/data-management", s.handleSettingsDataManagement)
	s.route("/settings/masking/output", s.handleMaskingOutputSave)
	s.route("/settings/masking/sql-output", s.handleMaskingSqlOutputSave)
	s.route("/settings/agents/", s.handleSettingsAgentsSub)
	s.route("/settings/tags/auto", s.handleSettingsTagsAuto)
	s.route("/settings/tags/", s.handleSettingsTagsSub)
	s.route("/settings/repositories/", s.handleSettingsRepositoriesSub)
	s.route("/settings/notifications/channels/", s.handleNotifChannelsSub)
	s.route("/settings/notifications/rules/", s.handleNotifRulesSub)
	s.route("/metrics/rules/auto", s.handleMetricsRulesAutoPreview)
	s.route("/metrics/rules/dashboard/auto", s.handleMetricsRulesDashboardAuto)
	s.route("/metrics/rules/", s.handleMetricsRulesSub)
	s.route("/dashboards/", s.handleDashboardsFormSub)
	s.route("/api/ai/helper/actions/manifest", s.handleApiAiHelperActionsManifest)
	s.route("/api/ai/helper/capabilities", s.handleApiAiHelperCapabilities)
	s.route("/api/ai/helper/chats/", s.handleApiAiHelperChatDetail)
	s.route("/api/onboarding/inspect-repo", s.handleApiOnboardingInspectRepo)
	s.route("/api/enrichment/github/repo-health", s.handleApiEnrichmentGithubRepoHealth)
	s.route("/api/reports/export", s.handleApiReportsExport)
	s.route("/api/ai/export", s.handleApiAiExport)
	s.route("/api/setup-wizard/steps", s.handleApiSetupWizardSteps)
	s.route("/v1/rum/assets/", s.handleV1RumAssetByID)
	s.route("/api/table-explorer/tables", s.handleApiTableExplorerTables)
	s.route("/api/kubernetes/status", s.handleApiKubernetesStatus)
	s.route("/api/notifications/vapid-public-key", s.handleApiVapidPublicKey)

	// Phase 3: data-backed JSON list/aggregation routes (read the shared chdb fixture).
	s.route("/api/dashboards/list", s.handleApiDashboardsList)
	s.route("/api/reports", s.handleApiReports)
	s.route("/api/agent/runs", s.handleApiAgentRuns)
	s.route("/api/web-traffic/browsers", s.handleApiWebTrafficBrowsers)
	s.route("/api/web-traffic/os", s.handleApiWebTrafficOS)
	s.route("/api/web-traffic/timezones", s.handleApiWebTrafficTimezones)
	s.route("/api/web-traffic/languages", s.handleApiWebTrafficLanguages)
	s.route("/api/web-traffic/devices", s.handleApiWebTrafficDevices)
	s.route("/api/chart-types", s.handleApiChartTypes)
	s.route("/api/data-management/backup/list", s.handleApiDmBackupList)
	s.route("/api/data-management/backup/run", s.handleDmBackupGuard)
	s.route("/api/data-management/restore", s.handleDmBackupGuard)
	s.route("/api/work-items", s.handleApiWorkItems)
	s.route("/api/enrichment/libraries", s.handleApiEnrichmentLibraries)
	s.route("/api/ai/span-attributes", s.handleApiAiSpanAttributes)
	s.route("/api/enrichment/cve/findings", s.handleApiCveFindings)
	s.route("/api/web-traffic/geo", s.handleApiWebTrafficGeo)
	s.route("/api/dashboards/spec/options", s.handleApiDashboardsSpecOptions)
	s.route("/api/dashboards/spec/templates", s.handleApiDashboardsSpecTemplates)
	s.route("/api/ai/field-hints", s.handleApiAiFieldHints)
	s.route("/api/logs/field-hints", s.handleApiLogsFieldHints)
	s.route("/api/metrics/anomaly", s.handleApiMetricsAnomaly)
	s.route("/api/ai/helper/chats", s.handleApiAiHelperChats)
	s.route("/api/ai/conversation", s.handleApiAiConversation)
	s.route("/api/settings/tags/condition-suggestions", s.handleApiTagRuleConditionSuggestions)
	s.route("/api/tags/", s.handleApiGetTags)
	s.route("/api/traces/span/", s.handleApiRawSpan)
	s.route("/api/table-explorer/table/", s.handleApiTableExplorerTable)
	s.route("/api/mcp/keys", s.handleApiMcpKeys)
	s.route("/api/logs/validate-regex", s.handleValidateRegex)
	s.route("/api/errors/validate-regex", s.handleValidateRegex)
	s.route("/api/traces/validate-regex", s.handleValidateRegex)
	s.route("/api/metrics/validate-regex", s.handleValidateRegex)
	s.route("/api/rum/validate-regex", s.handleValidateRegex)
	s.route("/api/logs/validate-filter", s.handleValidateFilter)
	s.route("/api/ai/validate-filter", s.handleValidateFilter)
	s.route("/api/settings/masking/rules", s.handleApiMaskingRules)

	// Dedicated static assets (explicit mimetypes / content-hash ETags) + service worker.
	s.route("/service-worker.js", s.handleServiceWorker)
	s.route("/static/rum.js", s.handleRumJS)
	s.route("/static/rum.min.js", s.handleRumMinJS)
	s.route("/static/rum.js.map", s.handleRumJSMap)
	s.route("/static/rum.min.js.map", s.handleRumMinJSMap)
	s.route("/static/rum.d.ts", s.handleRumDTS)
	// v1 ingest endpoints: GET -> 405 (POST ingest lands with OTLP).
	s.route("/v1/logs", s.handleV1IngestGet)
	s.route("/v1/metrics", s.handleV1IngestGet)
	s.route("/v1/traces", s.handleV1IngestGet)
	s.route("/v1/rum/assets", s.handleV1IngestGet)
	s.route("/v1/rum/client-token", s.handleV1RumClientToken)
	s.route("/v1/ai", s.handleV1Ai)
	s.route("/v1/errors", s.handleV1Errors)
	s.route("/v1/rum", s.handleV1Rum)
	s.route("/v1/apps", s.handleV1Apps)
	s.route("/v1/apps/", s.handleV1AppByID)
	s.route("/v1/releases/", s.handleV1ReleaseByID)
	s.route("/query", s.handleViewQuery)
	s.route("/table-explorer", s.handleViewTableExplorer)
	s.route("/kubernetes", s.handleViewKubernetes)
	s.route("/dashboards/new", s.handleNewDashboardForm)
	s.route("/settings/kubernetes", s.handleViewK8sSettings)
	s.route("/settings/repositories", s.handleViewSettingsRepositories)
	s.route("/settings/ai", s.handleViewAiSettings)
	s.route("/settings/tags", s.handleViewTagRules)
	s.route("/settings/mcp", s.handleMcpSettingsPage)
	s.route("/mcp", s.handleMcpEndpointGet)
	s.route("/mcp/tools", s.handleMcpListTools)
	s.route("/settings/enrichment", s.handleViewEnrichmentSettings)
	s.route("/settings/masking", s.handleViewMaskingSettings)
	s.route("/settings", s.handleViewSettings)
	s.route("/metrics/rules", s.handleViewMetricsRules)
	s.route("/settings/agents", s.handleViewAgentRules)
	s.route("/web-traffic", s.handleViewWebTraffic)
	s.route("/errors", s.handleViewErrors)
	s.route("/traces", s.handleViewTraces)
	s.route("/metrics", s.handleViewMetrics)
	s.route("/logs", s.handleViewLogs)
	s.route("/metrics/anomaly", s.handleViewMetricsAnomaly)
	s.route("/incident", s.handleViewIncident)
	s.route("/rum", s.handleViewRum)
	s.route("/tail", s.handleTail)
	s.route("/ai", s.handleViewAi)
	s.route("/work-items", s.handleViewWorkItemsPage)
	s.route("/enrichment/cve", s.handleViewEnrichmentCve)
	s.route("/{$}", s.handleSummary) // exact root "/" only (not a catch-all)
	s.route("/settings/notifications", s.handleViewNotifications)
	s.route("/dashboards", s.handleListDashboards)
	s.route("/reports", s.handleListReportsPage)

	// Static assets — served byte-for-byte from static/ (Quart's default static endpoint).
	s.route("/static/", s.handleStatic)

	// Phase 2: all *_help pages (template engine), generated from the registry.
	for _, h := range helpRoutes {
		s.route(h.Path, s.handleHelpPage(h.Endpoint, h.Template))
	}

	// Static serving — byte-identical to the committed static/ tree, with the explicit
	// ETag/X-SourceMap routes for rum.js* (see AUDIT.md §8).
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
	// codeql[go/reflected-xss] -- generic transport-level passthrough every response body
	// goes through; content safety is each handler's responsibility (HTML-producing handlers
	// escape via htmlEscapeMarkup or render.Engine's Jinja-compatible autoescape — see
	// handlers_forms.go/handlers_pages.go/handlers_misc.go), not this wrapper's.
	return h.ResponseWriter.Write(b)
}

// Flush exposes the underlying writer's http.Flusher so streaming handlers (e.g. /tail SSE)
// can push frames as they are written.
func (h *headerCapture) Flush() {
	if !h.wroteHeader {
		h.applySecurityHeaders()
		h.wroteHeader = true
	}
	if f, ok := h.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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
	applyOtlpCors(hdr, h.req)
}

// otlpCorsIngestPaths mirrors app.py _OTLP_CORS_INGEST_PATHS.
var otlpCorsIngestPaths = map[string]bool{
	"/v1/logs": true, "/v1/traces": true, "/v1/metrics": true, "/v1/rum": true,
	"/v1/rum/assets": true, "/v1/rum/client-token": true, "/v1/errors": true, "/v1/ai": true,
}

const otlpCorsAllowHeaders = "Content-Type, Authorization, X-API-Key, " +
	"X-SOBS-RUM-Client, X-SOBS-RUM-Signature, X-SOBS-RUM-Timestamp, " +
	"X-SOBS-Asset-Timestamp, X-SOBS-Asset-Signature"

// applyOtlpCors mirrors app.py _apply_security_headers' OTLP CORS block: on an OTLP/RUM ingest
// path with an allowed Origin, set the Access-Control-Allow-* headers (setdefault semantics).
func applyOtlpCors(hdr http.Header, req *http.Request) {
	path := req.URL.Path
	if !otlpCorsIngestPaths[path] && !strings.HasPrefix(path, "/v1/rum/assets/") {
		return
	}
	origin := strings.TrimSpace(req.Header.Get("Origin"))
	if origin == "" || !originAllowedForOtlp(origin) {
		return
	}
	hdr.Set("Access-Control-Allow-Origin", origin)
	appendVaryHeader(hdr, "Origin")
	setDefault(hdr, "Access-Control-Allow-Credentials", "true")
	methods := "POST, OPTIONS"
	if strings.HasPrefix(path, "/v1/rum/assets/") {
		methods = "GET, HEAD, OPTIONS"
	}
	setDefault(hdr, "Access-Control-Allow-Methods", methods)
	setDefault(hdr, "Access-Control-Allow-Headers", otlpCorsAllowHeaders)
	setDefault(hdr, "Access-Control-Max-Age", "600")
}

var schemeDefaultPorts = map[string]int{"http": 80, "https": 443}

// otlpCorsAllowedOrigins mirrors app.py _OTLP_CORS_ALLOWED_ORIGINS (env override + default).
var otlpCorsAllowedOrigins = parseOtlpAllowedOrigins()

func parseOtlpAllowedOrigins() []string {
	raw := os.Getenv("SOBS_OTLP_CORS_ALLOWED_ORIGINS")
	if raw == "" {
		raw = "http://localhost:*,https://localhost:*,http://127.0.0.1:*,https://127.0.0.1:*"
	}
	out := []string{}
	for _, item := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// originAllowedForOtlp mirrors app.py _origin_allowed_for_otlp (fnmatch against the allow-list,
// with the port-stripped candidate added only for default/absent ports).
func originAllowedForOtlp(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	netloc := strings.ToLower(u.Host)
	if (scheme != "http" && scheme != "https") || netloc == "" {
		return false
	}
	withPort := scheme + "://" + netloc
	candidates := []string{withPort}
	portStr := u.Port()
	var port int
	if portStr != "" {
		if port, err = strconv.Atoi(portStr); err != nil {
			return false
		}
	}
	if portStr == "" || port == schemeDefaultPorts[scheme] {
		withoutPort := withPort
		if host != "" {
			withoutPort = scheme + "://" + host
		}
		if withoutPort != withPort {
			candidates = append(candidates, withoutPort)
		}
	}
	for _, pattern := range otlpCorsAllowedOrigins {
		p := strings.ToLower(pattern)
		for _, c := range candidates {
			if fnmatchMatch(p, c) {
				return true
			}
		}
	}
	return false
}

// appendVaryHeader mirrors app.py _append_vary_header (case-insensitive token de-dup).
func appendVaryHeader(hdr http.Header, value string) {
	existing := strings.TrimSpace(hdr.Get("Vary"))
	if existing == "" {
		hdr.Set("Vary", value)
		return
	}
	parts := []string{}
	lower := map[string]bool{}
	for _, p := range strings.Split(existing, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
			lower[strings.ToLower(p)] = true
		}
	}
	if !lower[strings.ToLower(value)] {
		parts = append(parts, value)
	}
	hdr.Set("Vary", strings.Join(parts, ", "))
}

// fnmatchMatch implements Python fnmatch.fnmatch: `*` matches any run (incl. separators), `?`
// any single char. Patterns here have no character classes.
func fnmatchMatch(pattern, s string) bool {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

func setDefault(h http.Header, k, v string) {
	if h.Get(k) == "" {
		h.Set(k, v)
	}
}

// behindTLS mirrors app.py _BEHIND_TLS (= _env_flag("SOBS_BEHIND_TLS", False)), read once at
// startup. When set, every request is treated as a secure context.
var behindTLS = envFlag("SOBS_BEHIND_TLS", false)

// isSecure ports app.py _request_is_secure_context (app.py:361-367): True when behind TLS, or
// when the first comma-separated token of X-Forwarded-Proto is "https", or when the request was
// itself served over TLS (Python's request.scheme == "https" — r.TLS != nil here).
func isSecure(r *http.Request) bool {
	if behindTLS {
		return true
	}
	forwarded := r.Header.Get("X-Forwarded-Proto")
	if i := strings.IndexByte(forwarded, ','); i >= 0 {
		forwarded = forwarded[:i]
	}
	if strings.EqualFold(strings.TrimSpace(forwarded), "https") {
		return true
	}
	return r.TLS != nil
}
