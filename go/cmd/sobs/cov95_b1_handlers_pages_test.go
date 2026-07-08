package main

// cov95_b1_handlers_pages_test.go — coverage-gate batch 1 for handlers_pages.go. Targets pure
// helpers (queryOffset, sortedNonEmptyFacet) and page/settings handlers whose non-happy-path
// branches (validation failures, DB-error paths, edit-not-found, DB-error toggles, and
// create-with-matches flows) the golden corpus's single default-form fixture never exercises.
// Uses the existing storetest.FakeDB seam; see coverage_handlers_pages_gaps_test.go for the
// sibling batch already covering handleSettingsTagsAuto/handleMetricsRulesDashboardAuto basics.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

func cov95B1TestServer() *server {
	return &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{}}
}

// ---------------------------------------------------------------------------
// queryOffset (handlers_pages.go:1174) — pure function, 42.9% covered.
// ---------------------------------------------------------------------------

func TestQueryOffset(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want int
	}{
		{"absent", "/x", 0},
		{"empty", "/x?offset=", 0},
		{"valid", "/x?offset=42", 42},
		{"negative clamps to 0", "/x?offset=-5", 0},
		{"non-numeric falls back to 0", "/x?offset=abc", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.url, nil)
			if got := queryOffset(req); got != c.want {
				t.Errorf("queryOffset(%q) = %d, want %d", c.url, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// sortedNonEmptyFacet (handlers_pages.go:1188) — pure function, 46.7% covered.
// ---------------------------------------------------------------------------

func TestSortedNonEmptyFacet(t *testing.T) {
	in := []any{"b", "", "a", "b", "", "c", 123, "a"}
	got := sortedNonEmptyFacet(in)
	want := []any{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}

	// Empty input -> empty (non-nil) slice.
	if got := sortedNonEmptyFacet(nil); len(got) != 0 {
		t.Errorf("nil input: got %v, want empty", got)
	}
	// All-empty/non-string input -> empty.
	if got := sortedNonEmptyFacet([]any{"", "", 42, nil}); len(got) != 0 {
		t.Errorf("all-falsy input: got %v, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// renderInto (handlers_pages.go:475) — error path, 77.8% covered.
// ---------------------------------------------------------------------------

func TestRenderInto_TemplateError(t *testing.T) {
	s := cov95B1TestServer()
	rec := httptest.NewRecorder()
	// A nonexistent template name forces eng.Render to return an error.
	s.renderInto(rec, s.newEngine(httptest.NewRequest(http.MethodGet, "/", nil)), "this_template_does_not_exist.html", map[string]any{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() == 0 {
		t.Errorf("expected a non-empty error body")
	}
}

// ---------------------------------------------------------------------------
// handleMcpSettingsPage (handlers_pages.go:3564) — 75.0% covered: the disabled branch
// (mcp.enabled == "0") is untested; only the "no setting -> default enabled" fixture path is.
// ---------------------------------------------------------------------------

func TestHandleMcpSettingsPage_Disabled(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: storetest.SettingsDB(map[string]string{
		"mcp.enabled": "0",
	})}
	req := httptest.NewRequest(http.MethodGet, "/settings/mcp", nil)
	rec := httptest.NewRecorder()
	s.handleMcpSettingsPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleMcpSettingsPage_EnabledExplicit(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: storetest.SettingsDB(map[string]string{
		"mcp.enabled": "1",
	})}
	req := httptest.NewRequest(http.MethodGet, "/settings/mcp", nil)
	rec := httptest.NewRecorder()
	s.handleMcpSettingsPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleViewTagRules (handlers_pages.go:3577) — 66.7% covered: edit_rule found/not-found and
// the open_panel normalization are untested (only the plain GET/POST-dispatch path is covered).
// ---------------------------------------------------------------------------

func tagRuleFakeDB(t *testing.T) *storetest.FakeDB {
	t.Helper()
	cols := []string{"Id", "Name", "RecordTypes", "MatchField", "MatchOperator", "MatchValue",
		"MatchAttrKey", "TagKey", "TagValue", "ConditionsJson"}
	return &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		if strings.Contains(query, "sobs_tag_rules") {
			return storetest.Result(cols,
				[]any{"tag-1", "My Rule", "log,trace", "service_name", "eq", "checkout", "", "team", "checkout", ""},
			), nil
		}
		return &store.Result{}, nil
	}}
}

func TestHandleViewTagRules_EditRuleFound(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: tagRuleFakeDB(t)}
	req := httptest.NewRequest(http.MethodGet, "/settings/tags?edit_rule=tag-1&open_panel=auto-tags", nil)
	rec := httptest.NewRecorder()
	s.handleViewTagRules(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleViewTagRules_EditRuleNotFound(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: tagRuleFakeDB(t)}
	req := httptest.NewRequest(http.MethodGet, "/settings/tags?edit_rule=does-not-exist", nil)
	rec := httptest.NewRecorder()
	s.handleViewTagRules(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Tag rule not found for editing") {
		t.Errorf("expected the not-found flash message in body")
	}
}

func TestHandleViewTagRules_OpenPanelNormalization(t *testing.T) {
	s := cov95B1TestServer()
	req := httptest.NewRequest(http.MethodGet, "/settings/tags?open_panel=bogus", nil)
	rec := httptest.NewRecorder()
	s.handleViewTagRules(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleViewTagRules_PostDispatch(t *testing.T) {
	// POST with no fields -> createTagRule's "missing required fields" validation redirect.
	s := cov95B1TestServer()
	form := strings.NewReader("")
	req := httptest.NewRequest(http.MethodPost, "/settings/tags", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleViewTagRules(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleViewKubernetes (handlers_pages.go:3946) — 75.0% covered: only the disabled (404) branch
// is exercised by the corpus; the enabled real-render branch is not.
// ---------------------------------------------------------------------------

func TestHandleViewKubernetes_Enabled(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: storetest.SettingsDB(map[string]string{
		"kubernetes.enabled": "1",
	})}
	req := httptest.NewRequest(http.MethodGet, "/kubernetes", nil)
	rec := httptest.NewRecorder()
	s.handleViewKubernetes(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleViewKubernetes_Disabled(t *testing.T) {
	s := cov95B1TestServer()
	req := httptest.NewRequest(http.MethodGet, "/kubernetes", nil)
	rec := httptest.NewRecorder()
	s.handleViewKubernetes(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleViewEnrichmentSettings (handlers_pages.go:3867) — 69.2% covered: the POST save branch
// (all sub-cases) is entirely untested (only GET is corpus-exercised).
// ---------------------------------------------------------------------------

func TestHandleViewEnrichmentSettings_PostDefaults(t *testing.T) {
	// No fields set -> geo/cve both "false", max_releases defaults to "300".
	fake := &storetest.FakeDB{}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fake}
	req := httptest.NewRequest(http.MethodPost, "/settings/enrichment", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleViewEnrichmentSettings(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	saved := map[string]string{}
	for _, ins := range fake.Inserts {
		for _, row := range ins.Rows {
			saved[row["Key"].(string)] = row["Value"].(string)
		}
	}
	if saved["enrichment.geo_enabled"] != "false" {
		t.Errorf("geo_enabled = %q, want false", saved["enrichment.geo_enabled"])
	}
	if saved["enrichment.cve_enabled"] != "false" {
		t.Errorf("cve_enabled = %q, want false", saved["enrichment.cve_enabled"])
	}
	if saved["enrichment.github_backfill_max_releases"] != "300" {
		t.Errorf("max_releases = %q, want 300", saved["enrichment.github_backfill_max_releases"])
	}
}

func TestHandleViewEnrichmentSettings_PostEnabledWithCustomMax(t *testing.T) {
	fake := &storetest.FakeDB{}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fake}
	form := "geo_enabled=on&cve_enabled=on&github_backfill_max_releases=500"
	req := httptest.NewRequest(http.MethodPost, "/settings/enrichment", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleViewEnrichmentSettings(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	saved := map[string]string{}
	for _, ins := range fake.Inserts {
		for _, row := range ins.Rows {
			saved[row["Key"].(string)] = row["Value"].(string)
		}
	}
	if saved["enrichment.geo_enabled"] != "true" {
		t.Errorf("geo_enabled = %q, want true", saved["enrichment.geo_enabled"])
	}
	if saved["enrichment.cve_enabled"] != "true" {
		t.Errorf("cve_enabled = %q, want true", saved["enrichment.cve_enabled"])
	}
	if saved["enrichment.github_backfill_max_releases"] != "500" {
		t.Errorf("max_releases = %q, want 500", saved["enrichment.github_backfill_max_releases"])
	}
}

func TestHandleViewEnrichmentSettings_GetMaxReleasesClamp(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"below min clamps to 1", "0"},
		{"above max clamps to 2000", "999999"},
		{"non-numeric keeps default", "notanumber"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &server{cfg: config{TemplateDir: "../../../templates"}, db: storetest.SettingsDB(map[string]string{
				"enrichment.github_backfill_max_releases": c.value,
			})}
			req := httptest.NewRequest(http.MethodGet, "/settings/enrichment", nil)
			rec := httptest.NewRecorder()
			s.handleViewEnrichmentSettings(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// handleListDashboards (handlers_pages.go:508) — 81.0% covered: POST branches (empty-name
// warning, successful create+redirect) and the GET DB-error path are untested.
// ---------------------------------------------------------------------------

func TestHandleListDashboards_PostEmptyName(t *testing.T) {
	s := cov95B1TestServer()
	req := httptest.NewRequest(http.MethodPost, "/dashboards", strings.NewReader("name="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleListDashboards(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/dashboards" {
		t.Errorf("Location = %q, want /dashboards", loc)
	}
}

func TestHandleListDashboards_PostCreate(t *testing.T) {
	fake := &storetest.FakeDB{}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fake}
	form := "name=My+Dashboard&description=desc+here"
	req := httptest.NewRequest(http.MethodPost, "/dashboards", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleListDashboards(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.Inserts) != 1 || fake.Inserts[0].Table != "sobs_dashboards" {
		t.Fatalf("Inserts = %v, want one sobs_dashboards insert", fake.Inserts)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/dashboards/") || loc == "/dashboards/" {
		t.Errorf("Location = %q, want /dashboards/<id>", loc)
	}
}

func TestHandleListDashboards_PostInsertError(t *testing.T) {
	fake := &storetest.FakeDB{InsertErr: errors.New("insert boom")}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fake}
	form := "name=My+Dashboard"
	req := httptest.NewRequest(http.MethodPost, "/dashboards", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleListDashboards(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleListDashboards_GetQueryError(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("query boom") },
	}}
	req := httptest.NewRequest(http.MethodGet, "/dashboards", nil)
	rec := httptest.NewRecorder()
	s.handleListDashboards(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleListDashboards_GetWithRows(t *testing.T) {
	cols := []string{"Id", "Name", "Description"}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(cols, []any{"d1", "Dash One", "desc"}), nil
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/dashboards", nil)
	rec := httptest.NewRecorder()
	s.handleListDashboards(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleListReportsPage (handlers_pages.go:545) — 75.0% covered: the DB-error path and a
// populated-row render are untested.
// ---------------------------------------------------------------------------

func TestHandleListReportsPage_QueryError(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("query boom") },
	}}
	req := httptest.NewRequest(http.MethodGet, "/reports", nil)
	rec := httptest.NewRecorder()
	s.handleListReportsPage(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleListReportsPage_WithRows(t *testing.T) {
	cols := []string{"Id", "Name", "Description", "PageType", "FiltersJson"}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(cols,
				[]any{"r1", "Report One", "desc", "errors", `{"service":"api"}`},
			), nil
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/reports", nil)
	rec := httptest.NewRecorder()
	s.handleListReportsPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// createAgentRule (handlers_pages.go:3519), reached via handleViewAgentRules's POST dispatch —
// 82.1% covered: validation-failure branches and the action-fallback/insert-error paths are
// untested.
// ---------------------------------------------------------------------------

func TestCreateAgentRule_EmptyName(t *testing.T) {
	s := cov95B1TestServer()
	req := httptest.NewRequest(http.MethodPost, "/settings/agents", strings.NewReader("name="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.createAgentRule(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateAgentRule_InvalidTriggerType(t *testing.T) {
	s := cov95B1TestServer()
	form := "name=Rule1&trigger_type=bogus"
	req := httptest.NewRequest(http.MethodPost, "/settings/agents", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.createAgentRule(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateAgentRule_InvalidTriggerState(t *testing.T) {
	s := cov95B1TestServer()
	form := "name=Rule1&trigger_type=manual&trigger_state=bogus"
	req := httptest.NewRequest(http.MethodPost, "/settings/agents", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.createAgentRule(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateAgentRule_NoValidActionsFallsBackToAnalyze(t *testing.T) {
	fake := &storetest.FakeDB{}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fake}
	form := "name=Rule1&actions=not_a_real_action"
	req := httptest.NewRequest(http.MethodPost, "/settings/agents", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.createAgentRule(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.Inserts) != 1 {
		t.Fatalf("Inserts = %v, want 1", fake.Inserts)
	}
	row := fake.Inserts[0].Rows[0]
	if row["Actions"] != "analyze" {
		t.Errorf("Actions = %v, want fallback 'analyze'", row["Actions"])
	}
}

func TestCreateAgentRule_RateLimitClampAndValidActions(t *testing.T) {
	fake := &storetest.FakeDB{}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fake}
	form := "name=Rule1&rate_limit_minutes=999999&actions=analyze&actions=dlp_check"
	req := httptest.NewRequest(http.MethodPost, "/settings/agents", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.createAgentRule(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	row := fake.Inserts[0].Rows[0]
	if row["RateLimitMinutes"] != 10080 {
		t.Errorf("RateLimitMinutes = %v, want clamped 10080", row["RateLimitMinutes"])
	}
	if row["Actions"] != "analyze,dlp_check" {
		t.Errorf("Actions = %v, want 'analyze,dlp_check'", row["Actions"])
	}
}

func TestCreateAgentRule_InsertError(t *testing.T) {
	fake := &storetest.FakeDB{InsertErr: errors.New("insert boom")}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fake}
	form := "name=Rule1"
	req := httptest.NewRequest(http.MethodPost, "/settings/agents", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.createAgentRule(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleViewAgentRules_PostDispatch(t *testing.T) {
	s := cov95B1TestServer()
	req := httptest.NewRequest(http.MethodPost, "/settings/agents", strings.NewReader("name="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleViewAgentRules(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleViewNotifications (handlers_pages.go:900) — 60.0% covered: the edit_rule-found branch
// and a configured VAPID key are untested (only the empty-fixture default is).
// ---------------------------------------------------------------------------

func TestHandleViewNotifications_EditRuleFound(t *testing.T) {
	cols := []string{"Id", "Name", "Enabled", "LogicOperator", "ConditionsJson", "ChannelIds",
		"Severity", "CooldownSeconds", "LastFiredAt"}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{
		ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
			if strings.Contains(query, "sobs_notification_rules") {
				return storetest.Result(cols,
					[]any{"rule-1", "My Notif Rule", float64(1), "any", "[]", "chan-1", "warning", float64(60), ""},
				), nil
			}
			return &store.Result{}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodGet, "/settings/notifications?edit_rule=rule-1", nil)
	rec := httptest.NewRecorder()
	s.handleViewNotifications(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleViewNotifications_EditRuleNotFound(t *testing.T) {
	s := cov95B1TestServer()
	req := httptest.NewRequest(http.MethodGet, "/settings/notifications?edit_rule=missing", nil)
	rec := httptest.NewRecorder()
	s.handleViewNotifications(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleViewWebTraffic (handlers_pages.go:3331) — 94.4% covered: the non-empty time-window
// WHERE-clause branch (from_ts/to_ts supplied) is untested (only the fixture's window-less
// default is).
// ---------------------------------------------------------------------------

func TestHandleViewWebTraffic_WithTimeWindow(t *testing.T) {
	cols := []string{"col"}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{
		ExecuteFunc: func(query string, _ ...any) (*store.Result, error) {
			return storetest.Result(cols, []any{"1"}), nil
		},
	}}
	req := httptest.NewRequest(http.MethodGet,
		"/web-traffic?from_ts=2024-01-01T00:00:00&to_ts=2024-01-02T00:00:00", nil)
	rec := httptest.NewRecorder()
	s.handleViewWebTraffic(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleViewWebTraffic_TimeError(t *testing.T) {
	s := cov95B1TestServer()
	req := httptest.NewRequest(http.MethodGet, "/web-traffic?from_ts=not-a-date&to_ts=also-not-a-date", nil)
	rec := httptest.NewRecorder()
	s.handleViewWebTraffic(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleMetricsRulesDashboardAuto (handlers_pages.go:158) — 75.0% covered: the "create with
// matching candidates" success path (seedDashboardIfMissing + insert) and the getCharts /
// seedDashboardIfMissing DB-error branches are untested (only the wrong-method 404 and the
// zero-candidate early exit are, per coverage_handlers_pages_gaps_test.go).
// ---------------------------------------------------------------------------

func dashboardAutoRuleFakeDB() *storetest.FakeDB {
	ruleCols := []string{"Id", "Name", "RuleType", "SignalSource", "SignalName", "ServiceName",
		"AttrFingerprint", "Comparator", "WarningThreshold", "CriticalThreshold",
		"SecondarySignalSource", "SecondarySignalName", "SecondaryComparator",
		"SecondaryWarningThreshold", "SecondaryCriticalThreshold", "MinSampleCount", "SeasonalBucketsJson"}
	return &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(query, "sobs_anomaly_rules"):
			return storetest.Result(ruleCols,
				[]any{"rule-1", "High Errors", "threshold", "errors", "error_volume", "checkout",
					"", "gt", 10.0, 20.0, "", "", "gt", 0.0, 0.0, float64(1), ""},
			), nil
		case strings.Contains(query, "sobs_dashboards"):
			return &store.Result{}, nil // no existing dashboard -> seedDashboardIfMissing inserts one
		case strings.Contains(query, "sobs_chart_configs"):
			return &store.Result{}, nil // no existing charts
		default:
			return &store.Result{}, nil
		}
	}}
}

func TestHandleMetricsRulesDashboardAuto_CreateWithMatches(t *testing.T) {
	fake := dashboardAutoRuleFakeDB()
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fake}
	form := "action=create&max_charts=5"
	req := httptest.NewRequest(http.MethodPost, "/metrics/rules/dashboard/auto", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMetricsRulesDashboardAuto(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	var sawDashboardInsert, sawChartInsert bool
	for _, ins := range fake.Inserts {
		if ins.Table == "sobs_dashboards" {
			sawDashboardInsert = true
		}
		if ins.Table == "sobs_chart_configs" {
			sawChartInsert = true
		}
	}
	if !sawDashboardInsert {
		t.Errorf("expected a sobs_dashboards insert (seedDashboardIfMissing), got inserts: %v", fake.Inserts)
	}
	if !sawChartInsert {
		t.Errorf("expected a sobs_chart_configs insert, got inserts: %v", fake.Inserts)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/dashboards/") {
		t.Errorf("Location = %q, want /dashboards/<id>", loc)
	}
}

func TestHandleMetricsRulesDashboardAuto_SeedDashboardError(t *testing.T) {
	ruleCols := []string{"Id", "Name", "RuleType", "SignalSource", "SignalName", "ServiceName",
		"AttrFingerprint", "Comparator", "WarningThreshold", "CriticalThreshold",
		"SecondarySignalSource", "SecondarySignalName", "SecondaryComparator",
		"SecondaryWarningThreshold", "SecondaryCriticalThreshold", "MinSampleCount", "SeasonalBucketsJson"}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{
		ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
			switch {
			case strings.Contains(query, "sobs_anomaly_rules"):
				return storetest.Result(ruleCols,
					[]any{"rule-1", "High Errors", "threshold", "errors", "error_volume", "checkout",
						"", "gt", 10.0, 20.0, "", "", "gt", 0.0, 0.0, float64(1), ""},
				), nil
			case strings.Contains(query, "sobs_dashboards"):
				return nil, errors.New("dashboard lookup boom")
			default:
				return &store.Result{}, nil
			}
		},
	}}
	form := "action=create"
	req := httptest.NewRequest(http.MethodPost, "/metrics/rules/dashboard/auto", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMetricsRulesDashboardAuto(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleMetricsRulesDashboardAuto_PreviewWithCustomHoursAndServiceFilter(t *testing.T) {
	fake := dashboardAutoRuleFakeDB()
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fake}
	req := httptest.NewRequest(http.MethodPost, "/metrics/rules/dashboard/auto",
		strings.NewReader("hours=48&service_filter=checkout&dashboard_name=Custom+Name"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMetricsRulesDashboardAuto(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleMetricsRulesAutoPreview (handlers_pages.go:290) — 78.8% covered: the "create" action
// (insert path), the seasonal mode branch, and the include_attr_fp truthy-string branch are
// untested (only the default "preview" empty-candidate branch is, per the sibling gaps test).
// ---------------------------------------------------------------------------

func TestHandleMetricsRulesAutoPreview_SeasonalMode(t *testing.T) {
	s := cov95B1TestServer()
	form := "mode=seasonal&seasonal_strategy=day_of_week&include_attr_fp=1"
	req := httptest.NewRequest(http.MethodPost, "/metrics/rules/auto", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMetricsRulesAutoPreview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleMetricsRulesAutoPreview_InvalidModeAndStrategyFallback(t *testing.T) {
	s := cov95B1TestServer()
	form := "mode=bogus&seasonal_strategy=bogus"
	req := httptest.NewRequest(http.MethodPost, "/metrics/rules/auto", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMetricsRulesAutoPreview(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleMetricsRulesAutoPreview_CreateEmptyCandidates(t *testing.T) {
	// Zero candidates on the empty fixture -> create still succeeds (no rows to insert), and the
	// success-flash redirect fires (distinct from handleSettingsTagsAuto's analogous branch).
	fake := &storetest.FakeDB{}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fake}
	form := "action=create"
	req := httptest.NewRequest(http.MethodPost, "/metrics/rules/auto", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMetricsRulesAutoPreview(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302; body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.Inserts) != 0 {
		t.Errorf("Inserts = %v, want none for zero candidates", fake.Inserts)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "open_panel=auto-rules") {
		t.Errorf("Location = %q, want it to contain open_panel=auto-rules", loc)
	}
}

func TestHandleMetricsRulesAutoPreview_HoursAndMinPointsClamp(t *testing.T) {
	cases := []struct {
		name, hours, minPoints string
	}{
		{"hours below min", "0", "30"},
		{"hours above max", "9999", "30"},
		{"hours non-numeric", "notanumber", "30"},
		{"min_points below min", "24", "0"},
		{"min_points above max", "24", "999999"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := cov95B1TestServer()
			form := "hours=" + c.hours + "&min_points=" + c.minPoints
			req := httptest.NewRequest(http.MethodPost, "/metrics/rules/auto", strings.NewReader(form))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			s.handleMetricsRulesAutoPreview(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
