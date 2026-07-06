package main

// coverage_handlers_pages_gaps_test.go — boundary-value tests for handlers_pages.go's
// input-clamping branches. The golden corpus only ever posts the default form values (hours=24,
// min_count=30, etc.), so the clamp branches (below-min, above-max, unparseable) were never
// exercised. These use the existing storetest.FakeDB seam (zero-value = empty result set for
// every query, matching the corpus's empty fixture) plus the real template directory, the same
// pattern handlers_incident_test.go already established.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store/storetest"
)

func newPageTestServer() *server {
	return &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{}}
}

func TestHandleSettingsTagsAuto_HoursClamp(t *testing.T) {
	cases := []struct {
		name      string
		hours     string
		wantValue string
	}{
		{"below min clamps to 1", "0", `value="1"`},
		{"negative clamps to 1", "-5", `value="1"`},
		{"above max clamps to 168", "999", `value="168"`},
		{"within range unchanged", "48", `value="48"`},
		{"non-numeric keeps default", "notanumber", `value="24"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newPageTestServer()
			form := strings.NewReader("hours=" + c.hours)
			req := httptest.NewRequest(http.MethodPost, "/settings/tags/auto", form)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			s.handleSettingsTagsAuto(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `name="hours" class="form-control form-control-sm" `+c.wantValue) {
				t.Errorf("hours=%q: response did not contain %s for the hours input", c.hours, c.wantValue)
			}
		})
	}
}

func TestHandleSettingsTagsAuto_MinCountClamp(t *testing.T) {
	cases := []struct {
		name      string
		minCount  string
		wantValue string
	}{
		{"below min clamps to 1", "0", `value="1"`},
		{"above max clamps to 5000", "999999", `value="5000"`},
		{"within range unchanged", "100", `value="100"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newPageTestServer()
			form := strings.NewReader("min_count=" + c.minCount)
			req := httptest.NewRequest(http.MethodPost, "/settings/tags/auto", form)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			s.handleSettingsTagsAuto(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `name="min_count" class="form-control form-control-sm" `+c.wantValue) {
				t.Errorf("min_count=%q: response did not contain %s for the min_count input", c.minCount, c.wantValue)
			}
		})
	}
}

func TestHandleSettingsTagsAuto_WrongMethod(t *testing.T) {
	s := newPageTestServer()
	req := httptest.NewRequest(http.MethodGet, "/settings/tags/auto", nil)
	rec := httptest.NewRecorder()
	s.handleSettingsTagsAuto(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

// TestHandleSettingsTagsAuto_RecordTypesDefault covers the "no auto_record_types selected ->
// fall back to the full default set" branch (handlers_pages.go:75-77).
func TestHandleSettingsTagsAuto_RecordTypesDefault(t *testing.T) {
	s := newPageTestServer()
	req := httptest.NewRequest(http.MethodPost, "/settings/tags/auto", strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleSettingsTagsAuto(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleSettingsTagsAuto_CreateEmpty covers the "create" action branch
// (handlers_pages.go:91-132), which the golden corpus's preview-only fixture never triggers:
// zero candidates -> no insert call, capSuffix stays ".", and a success flash redirect fires.
func TestHandleSettingsTagsAuto_CreateEmpty(t *testing.T) {
	fake := &storetest.FakeDB{}
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fake}
	form := strings.NewReader("action=create")
	req := httptest.NewRequest(http.MethodPost, "/settings/tags/auto", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleSettingsTagsAuto(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302 redirect; body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.Inserts) != 0 {
		t.Errorf("Inserts = %v, want none for zero candidates", fake.Inserts)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "open_panel=auto-tags") {
		t.Errorf("Location = %q, want it to contain open_panel=auto-tags", loc)
	}
}

// TestHandleMetricsRulesDashboardAuto_WrongMethod covers the sibling handler's 404 branch.
func TestHandleMetricsRulesDashboardAuto_WrongMethod(t *testing.T) {
	s := newPageTestServer()
	req := httptest.NewRequest(http.MethodGet, "/metrics/rules/dashboard/auto", nil)
	rec := httptest.NewRecorder()
	s.handleMetricsRulesDashboardAuto(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

// TestHandleMetricsRulesDashboardAuto_CreateNoMatches covers the "create with zero candidates"
// early-exit branch (handlers_pages.go:191-195), which the golden corpus's happy-path create
// case never triggers.
func TestHandleMetricsRulesDashboardAuto_CreateNoMatches(t *testing.T) {
	s := newPageTestServer()
	form := strings.NewReader("action=create")
	req := httptest.NewRequest(http.MethodPost, "/metrics/rules/dashboard/auto", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.handleMetricsRulesDashboardAuto(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302 redirect; body=%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "open_panel=auto-dashboard") {
		t.Errorf("Location = %q, want it to contain open_panel=auto-dashboard", loc)
	}
}
