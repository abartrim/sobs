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
	s.mux.HandleFunc("/api/query/ask", s.handleApiQueryAsk)
	s.mux.HandleFunc("/api/query/run", s.handleApiQueryRun)
	s.mux.HandleFunc("/api/query/refine-chart", s.handleApiQueryRefineChart)
	s.mux.HandleFunc("/api/query/add-to-dashboard", s.handleApiQueryAddToDashboard)
	s.mux.HandleFunc("/api/ai/helper", s.handleApiAiHelper)
	s.mux.HandleFunc("/api/ai/helper/actions/execute", s.handleApiAiHelperExecute)
	s.mux.HandleFunc("/api/ai/helper/feedback", s.handleApiAiHelperFeedback)
	s.mux.HandleFunc("/api/dashboards/query", s.handleApiDashboardsQuery)
	s.mux.HandleFunc("/api/dashboards/render", s.handleApiDashboardsRender)
	s.mux.HandleFunc("/api/dashboards/spec/ai-build", s.handleApiDashboardsSpecAiBuild)
	s.mux.HandleFunc("/api/dashboards/spec/compile", s.handleApiDashboardsSpecCompile)
	s.mux.HandleFunc("/api/dashboards/spec/dry-run", s.handleApiDashboardsSpecDryRun)
	s.mux.HandleFunc("/api/dashboards/spec/render", s.handleApiDashboardsSpecRender)
	s.mux.HandleFunc("/api/dashboards/spec/validate", s.handleApiDashboardsSpecValidate)
	s.mux.HandleFunc("/api/notifications/subscribe", s.handleApiNotificationsSubscribe)
	s.mux.HandleFunc("/api/notifications/check", s.handleApiNotificationsCheck)
	s.mux.HandleFunc("/api/onboarding/create-issues", s.handleApiOnboardingCreateIssues)
	s.mux.HandleFunc("/api/onboarding/create-repo", s.handleApiOnboardingCreateRepo)
	s.mux.HandleFunc("/api/onboarding/import-repo", s.handleApiOnboardingImportRepo)
	s.mux.HandleFunc("/api/onboarding/list-repos", s.handleApiOnboardingListRepos)
	s.mux.HandleFunc("/api/issues/raise", s.handleApiIssuesRaise)
	s.mux.HandleFunc("/api/mcp/enabled", s.handleApiMcpEnabled)
	s.mux.HandleFunc("/api/settings/masking/preview", s.handleApiSettingsMaskingPreview)
	s.mux.HandleFunc("/api/data-management/prune", s.handleApiDataManagementPrune)
	s.mux.HandleFunc("/api/mcp/keys/", s.handleMcpKeyByID)
	s.mux.HandleFunc("/api/notifications/vapid-keys", s.handleVapidKeys)
	s.mux.HandleFunc("/api/reports/", s.handleReportsSub)
	s.mux.HandleFunc("/api/agent/runs/", s.handleAgentRunSub)
	s.mux.HandleFunc("/api/notifications/channels/", s.handleChannelSub)
	s.mux.HandleFunc("/api/enrichment/cve/findings/", s.handleCveDispositionSub)
	s.mux.HandleFunc("/api/dashboards/", s.handleDashboardSub)
	s.mux.HandleFunc("/errors/", s.handleErrorSub)
	s.mux.HandleFunc("/reports/", s.handleReportsFormSub)
	s.mux.HandleFunc("/settings/notifications/channels", s.handleNotifChannelsCreate)
	s.mux.HandleFunc("/settings/notifications/rules", s.handleNotifRulesCreate)
	s.mux.HandleFunc("/settings/masking/keys", s.handleMaskingKeysCreate)
	s.mux.HandleFunc("/settings/masking/patterns", s.handleMaskingPatternsCreate)
	s.mux.HandleFunc("/settings/masking/keys/delete", s.handleMaskingKeysDelete)
	s.mux.HandleFunc("/settings/masking/patterns/delete", s.handleMaskingPatternsDelete)
	s.mux.HandleFunc("/settings/data-management", s.handleSettingsDataManagement)
	s.mux.HandleFunc("/settings/masking/output", s.handleMaskingOutputSave)
	s.mux.HandleFunc("/settings/masking/sql-output", s.handleMaskingSqlOutputSave)
	s.mux.HandleFunc("/settings/agents/", s.handleSettingsAgentsSub)
	s.mux.HandleFunc("/settings/tags/", s.handleSettingsTagsSub)
	s.mux.HandleFunc("/settings/repositories/", s.handleSettingsRepositoriesSub)
	s.mux.HandleFunc("/settings/notifications/channels/", s.handleNotifChannelsSub)
	s.mux.HandleFunc("/settings/notifications/rules/", s.handleNotifRulesSub)
	s.mux.HandleFunc("/metrics/rules/", s.handleMetricsRulesSub)
	s.mux.HandleFunc("/dashboards/", s.handleDashboardsFormSub)
	s.mux.HandleFunc("/api/ai/helper/actions/manifest", s.handleApiAiHelperActionsManifest)
	s.mux.HandleFunc("/api/ai/helper/capabilities", s.handleApiAiHelperCapabilities)
	s.mux.HandleFunc("/api/ai/helper/chats/", s.handleApiAiHelperChatDetail)
	s.mux.HandleFunc("/api/onboarding/inspect-repo", s.handleApiOnboardingInspectRepo)
	s.mux.HandleFunc("/api/enrichment/github/repo-health", s.handleApiEnrichmentGithubRepoHealth)
	s.mux.HandleFunc("/api/reports/export", s.handleApiReportsExport)
	s.mux.HandleFunc("/api/ai/export", s.handleApiAiExport)
	s.mux.HandleFunc("/api/setup-wizard/steps", s.handleApiSetupWizardSteps)
	s.mux.HandleFunc("/v1/rum/assets/", s.handleV1RumAssetByID)
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
	s.mux.HandleFunc("/api/data-management/backup/run", s.handleDmBackupGuard)
	s.mux.HandleFunc("/api/data-management/restore", s.handleDmBackupGuard)
	s.mux.HandleFunc("/api/work-items", s.handleApiWorkItems)
	s.mux.HandleFunc("/api/enrichment/libraries", s.handleApiEnrichmentLibraries)
	s.mux.HandleFunc("/api/ai/span-attributes", s.handleApiAiSpanAttributes)
	s.mux.HandleFunc("/api/enrichment/cve/findings", s.handleApiCveFindings)
	s.mux.HandleFunc("/api/web-traffic/geo", s.handleApiWebTrafficGeo)
	s.mux.HandleFunc("/api/dashboards/spec/options", s.handleApiDashboardsSpecOptions)
	s.mux.HandleFunc("/api/dashboards/spec/templates", s.handleApiDashboardsSpecTemplates)
	s.mux.HandleFunc("/api/ai/field-hints", s.handleApiAiFieldHints)
	s.mux.HandleFunc("/api/logs/field-hints", s.handleApiLogsFieldHints)
	s.mux.HandleFunc("/api/metrics/anomaly", s.handleApiMetricsAnomaly)
	s.mux.HandleFunc("/api/ai/helper/chats", s.handleApiAiHelperChats)
	s.mux.HandleFunc("/api/ai/conversation", s.handleApiAiConversation)
	s.mux.HandleFunc("/api/settings/tags/condition-suggestions", s.handleApiTagRuleConditionSuggestions)
	s.mux.HandleFunc("/api/tags/", s.handleApiGetTags)
	s.mux.HandleFunc("/api/traces/span/", s.handleApiRawSpan)
	s.mux.HandleFunc("/api/table-explorer/table/", s.handleApiTableExplorerTable)
	s.mux.HandleFunc("/api/mcp/keys", s.handleApiMcpKeys)
	s.mux.HandleFunc("/api/logs/validate-regex", s.handleValidateRegex)
	s.mux.HandleFunc("/api/errors/validate-regex", s.handleValidateRegex)
	s.mux.HandleFunc("/api/traces/validate-regex", s.handleValidateRegex)
	s.mux.HandleFunc("/api/metrics/validate-regex", s.handleValidateRegex)
	s.mux.HandleFunc("/api/rum/validate-regex", s.handleValidateRegex)
	s.mux.HandleFunc("/api/logs/validate-filter", s.handleValidateFilter)
	s.mux.HandleFunc("/api/ai/validate-filter", s.handleValidateFilter)
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
	s.mux.HandleFunc("/v1/rum/client-token", s.handleV1RumClientToken)
	s.mux.HandleFunc("/v1/ai", s.handleV1Ai)
	s.mux.HandleFunc("/v1/errors", s.handleV1Errors)
	s.mux.HandleFunc("/v1/rum", s.handleV1Rum)
	s.mux.HandleFunc("/v1/apps", s.handleV1Apps)
	s.mux.HandleFunc("/v1/apps/", s.handleV1AppByID)
	s.mux.HandleFunc("/v1/releases/", s.handleV1ReleaseByID)
	s.mux.HandleFunc("/query", s.handleViewQuery)
	s.mux.HandleFunc("/table-explorer", s.handleViewTableExplorer)
	s.mux.HandleFunc("/kubernetes", s.handleViewKubernetes)
	s.mux.HandleFunc("/dashboards/new", s.handleNewDashboardForm)
	s.mux.HandleFunc("/settings/kubernetes", s.handleViewK8sSettings)
	s.mux.HandleFunc("/settings/repositories", s.handleViewSettingsRepositories)
	s.mux.HandleFunc("/settings/ai", s.handleViewAiSettings)
	s.mux.HandleFunc("/settings/tags", s.handleViewTagRules)
	s.mux.HandleFunc("/settings/mcp", s.handleMcpSettingsPage)
	s.mux.HandleFunc("/mcp", s.handleMcpEndpointGet)
	s.mux.HandleFunc("/mcp/tools", s.handleMcpListTools)
	s.mux.HandleFunc("/settings/enrichment", s.handleViewEnrichmentSettings)
	s.mux.HandleFunc("/settings/masking", s.handleViewMaskingSettings)
	s.mux.HandleFunc("/settings", s.handleViewSettings)
	s.mux.HandleFunc("/metrics/rules", s.handleViewMetricsRules)
	s.mux.HandleFunc("/settings/agents", s.handleViewAgentRules)
	s.mux.HandleFunc("/web-traffic", s.handleViewWebTraffic)
	s.mux.HandleFunc("/errors", s.handleViewErrors)
	s.mux.HandleFunc("/traces", s.handleViewTraces)
	s.mux.HandleFunc("/metrics", s.handleViewMetrics)
	s.mux.HandleFunc("/logs", s.handleViewLogs)
	s.mux.HandleFunc("/metrics/anomaly", s.handleViewMetricsAnomaly)
	s.mux.HandleFunc("/incident", s.handleViewIncident)
	s.mux.HandleFunc("/ai", s.handleViewAi)
	s.mux.HandleFunc("/work-items", s.handleViewWorkItemsPage)
	s.mux.HandleFunc("/enrichment/cve", s.handleViewEnrichmentCve)
	s.mux.HandleFunc("/{$}", s.handleSummary) // exact root "/" only (not a catch-all)
	s.mux.HandleFunc("/settings/notifications", s.handleViewNotifications)
	s.mux.HandleFunc("/dashboards", s.handleListDashboards)
	s.mux.HandleFunc("/reports", s.handleListReportsPage)

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
