package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// This file closes a gap left by the sibling cov95_b4_handlers_mutations3_test.go: it only
// exercised handleDashboardSub's "dashboard not found" branches for both the chart-import and
// chart-export sub-paths. The "dashboard exists" dispatch lines (the calls into s.importChart /
// s.exportChart once s.rowExists confirms the dashboard) were still uncovered.

func TestHandleDashboardSubImportDispatchesWhenDashboardExists(t *testing.T) {
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_dashboards") {
			return storetest.Result([]string{"Id"}, []any{"dash-1"}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	// A body missing sobs_chart_template_version=1 exercises importChart's own validation error,
	// proving control reached s.importChart (rather than short-circuiting on "Dashboard not found").
	r := httptest.NewRequest("POST", "/api/dashboards/dash-1/charts/import", strings.NewReader(`{}`))
	s.handleDashboardSub(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400 from importChart's own validation, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Invalid or unsupported chart template format") {
		t.Fatalf("expected importChart's error to surface (dispatch reached), got %s", w.Body.String())
	}
}

func TestHandleDashboardSubExportDispatchesWhenDashboardExists(t *testing.T) {
	dashSeen := false
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_dashboards") {
			dashSeen = true
			return storetest.Result([]string{"Id"}, []any{"dash-1"}), nil
		}
		// exportChart's own chart lookup: no matching chart -> its own 404 ("Chart not found"),
		// proving control reached s.exportChart (rather than "Dashboard not found").
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/dashboards/dash-1/charts/chart-1/export", nil)
	s.handleDashboardSub(w, r)
	if !dashSeen {
		t.Fatal("expected the dashboard-existence query to run")
	}
	if w.Code != 404 {
		t.Fatalf("want 404 from exportChart's own chart lookup, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Chart not found") {
		t.Fatalf("expected exportChart's own error (dispatch reached), got %s", w.Body.String())
	}
}
