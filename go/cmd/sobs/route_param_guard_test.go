package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestParamRouteFor checks the concrete-path -> template matcher: a placeholder segment matches
// any single non-empty segment, a literal segment must match exactly, and the segment count must
// be equal. Paths that match no template return ok=false (the caller keeps its own 404).
func TestParamRouteFor(t *testing.T) {
	cases := []struct {
		path      string
		wantTmpl  string // "" => no match expected
		wantAllow string
	}{
		// concrete instances of each kind of template
		{"/api/mcp/keys/abc123", "/api/mcp/keys/<key_id>", "DELETE, OPTIONS"},
		{"/api/reports/r-1", "/api/reports/<report_id>", "DELETE, OPTIONS"},
		{"/api/tags/log/42", "/api/tags/<record_type>/<record_id>", "GET, HEAD, OPTIONS, POST"},
		{"/api/tags/log/42/owner", "/api/tags/<record_type>/<record_id>/<tag_key>", "DELETE, OPTIONS"},
		{"/api/traces/span/s-9", "/api/traces/span/<span_id>", "GET, HEAD, OPTIONS"},
		{"/api/table-explorer/table/otel_logs", "/api/table-explorer/table/<name>", "GET, HEAD, OPTIONS"},
		{"/v1/apps/app-1", "/v1/apps/<app_id>", "GET, HEAD, OPTIONS, PATCH"},
		{"/v1/apps/app-1/releases", "/v1/apps/<app_id>/releases", "GET, HEAD, OPTIONS, POST"},
		{"/v1/releases/rel-1", "/v1/releases/<release_id>", "GET, HEAD, OPTIONS"},
		{"/v1/releases/rel-1/artifacts", "/v1/releases/<release_id>/artifacts", "GET, HEAD, OPTIONS"},
		{"/v1/releases/rel-1/artifacts/meta", "/v1/releases/<release_id>/artifacts/meta", "OPTIONS, POST"},
		{"/v1/rum/assets/a1", "/v1/rum/assets/<asset_id>", "GET, HEAD, OPTIONS"},
		{"/dashboards/d-1", "/dashboards/<dashboard_id>", "GET, HEAD, OPTIONS"},
		{"/dashboards/d-1/delete", "/dashboards/<dashboard_id>/delete", "OPTIONS, POST"},
		{"/dashboards/d-1/charts", "/dashboards/<dashboard_id>/charts", "OPTIONS, POST"},
		{"/dashboards/d-1/charts/c-2/clone", "/dashboards/<dashboard_id>/charts/<chart_id>/clone", "OPTIONS, POST"},
		{"/dashboards/d-1/charts/c-2/edit", "/dashboards/<dashboard_id>/charts/<chart_id>/edit", "OPTIONS, POST"},
		{"/dashboards/d-1/charts/c-2/delete", "/dashboards/<dashboard_id>/charts/<chart_id>/delete", "OPTIONS, POST"},
		{"/api/dashboards/d-1/charts/import", "/api/dashboards/<dashboard_id>/charts/import", "OPTIONS, POST"},
		{"/api/dashboards/d-1/charts/c-2/export", "/api/dashboards/<dashboard_id>/charts/<chart_id>/export", "GET, HEAD, OPTIONS"},
		{"/errors/e-1/resolve", "/errors/<string:error_id>/resolve", "OPTIONS, POST"},
		{"/settings/agents/a-1/delete", "/settings/agents/<rule_id>/delete", "OPTIONS, POST"},
		{"/settings/tags/t-1/delete", "/settings/tags/<rule_id>/delete", "OPTIONS, POST"},
		{"/metrics/rules/m-1/delete", "/metrics/rules/<rule_id>/delete", "OPTIONS, POST"},
		{"/settings/repositories/app-1", "/settings/repositories/<app_id>", "OPTIONS, POST"},
		{"/settings/repositories/app-1/delete", "/settings/repositories/<app_id>/delete", "OPTIONS, POST"},
		{"/settings/repositories/app-1/releases", "/settings/repositories/<app_id>/releases", "OPTIONS, POST"},
		{"/settings/repositories/app-1/realtime-mode", "/settings/repositories/<app_id>/realtime-mode", "OPTIONS, POST"},
		{"/settings/repositories/app-1/ci-ingest-key/rotate", "/settings/repositories/<app_id>/ci-ingest-key/rotate", "OPTIONS, POST"},
		{"/settings/repositories/app-1/ci-ingest-key/revoke", "/settings/repositories/<app_id>/ci-ingest-key/revoke", "OPTIONS, POST"},
		{"/settings/notifications/channels/c-1/delete", "/settings/notifications/channels/<channel_id>/delete", "OPTIONS, POST"},
		{"/settings/notifications/channels/c-1/toggle", "/settings/notifications/channels/<channel_id>/toggle", "OPTIONS, POST"},
		{"/settings/notifications/rules/r-1/delete", "/settings/notifications/rules/<rule_id>/delete", "OPTIONS, POST"},
		{"/settings/notifications/rules/r-1/toggle", "/settings/notifications/rules/<rule_id>/toggle", "OPTIONS, POST"},
		{"/api/notifications/channels/c-1/test", "/api/notifications/channels/<channel_id>/test", "OPTIONS, POST"},
		{"/api/enrichment/cve/findings/CVE-1/disposition", "/api/enrichment/cve/findings/<osv_id>/disposition", "OPTIONS, POST"},
		{"/api/agent/runs/run-1/dismiss", "/api/agent/runs/<run_id>/dismiss", "OPTIONS, POST"},
		{"/api/ai/helper/chats/chat-1", "/api/ai/helper/chats/<chat_id>", "GET, HEAD, OPTIONS"},

		// non-matches: wrong arity, empty trailing segment, unknown sub-action
		{"/api/mcp/keys", "", ""},  // bare collection route (separate handler)
		{"/api/mcp/keys/", "", ""}, // empty key id -> no match
		{"/api/reports/import", "/api/reports/<report_id>", "DELETE, OPTIONS"}, // shadowed by a static route, excluded by caller
		{"/settings/repositories/app-1/unknown-action", "", ""},
		{"/dashboards", "", ""},
		{"/errors/e-1", "", ""}, // missing /resolve
	}
	for _, c := range cases {
		pr, ok := paramRouteFor(c.path)
		if c.wantTmpl == "" {
			if ok {
				t.Errorf("paramRouteFor(%q) matched %q (allow %q); want no match", c.path, "<some>", pr.allow)
			}
			continue
		}
		if !ok {
			t.Errorf("paramRouteFor(%q) no match; want allow %q", c.path, c.wantAllow)
			continue
		}
		if pr.allow != c.wantAllow {
			t.Errorf("paramRouteFor(%q) allow = %q; want %q", c.path, pr.allow, c.wantAllow)
		}
	}
}

// TestParamMethodGuard checks the three guard outcomes: an allowed method passes through
// (returns false, writes nothing), a disallowed method yields Werkzeug's 405 + sorted Allow +
// the default error body, and OPTIONS yields an empty 200 + Allow.
func TestParamMethodGuard(t *testing.T) {
	// allowed method -> guard is a no-op
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tags/log/42", nil)
	if paramMethodGuard(rec, req) {
		t.Fatalf("guard handled an allowed method (POST on /api/tags/<rt>/<rid>)")
	}
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("guard wrote a response for an allowed method: code=%d bodylen=%d", rec.Code, rec.Body.Len())
	}

	// disallowed method -> 405 with the union Allow header and the error page body
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/tags/log/42", nil)
	if !paramMethodGuard(rec, req) {
		t.Fatalf("guard did not handle a disallowed method (DELETE on /api/tags/<rt>/<rid>)")
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("405 expected, got %d", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD, OPTIONS, POST" {
		t.Fatalf("Allow = %q; want %q", got, "GET, HEAD, OPTIONS, POST")
	}
	if rec.Body.String() != methodNotAllowed405Body {
		t.Fatalf("405 body mismatch")
	}

	// OPTIONS -> empty 200 + Allow
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodOptions, "/v1/apps/app-1/releases", nil)
	if !paramMethodGuard(rec, req) {
		t.Fatalf("guard did not handle OPTIONS")
	}
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("OPTIONS expected empty 200, got code=%d bodylen=%d", rec.Code, rec.Body.Len())
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD, OPTIONS, POST" {
		t.Fatalf("OPTIONS Allow = %q; want %q", got, "GET, HEAD, OPTIONS, POST")
	}

	// unmatched path -> guard returns false, writes nothing
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/mcp/keys", nil)
	if paramMethodGuard(rec, req) {
		t.Fatalf("guard handled an unmatched path")
	}
}
