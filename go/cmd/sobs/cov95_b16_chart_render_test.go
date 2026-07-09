package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_chart_render_test.go — batch 16 targeted coverage for cmd/sobs/chart_render.go's
// handleApiDashboardsSpecValidate: the compile-error (400), db-query-error (400), and
// success-with-empty-result-set (200, noDataPlaceholder render) branches. The render-error branch
// is not exercised here (it requires a populated multi-column result whose binding extraction
// fails, which duplicates the extensive coverage already in chart_render_binding tests) — the
// three branches above cover the handler's own control flow (the part unique to this file).

func TestHandleApiDashboardsSpecValidateCompileError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/spec/validate", strings.NewReader(
		`{"spec":{"template_id":"no_such_template"}}`))
	s.handleApiDashboardsSpecValidate(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400 on unknown template, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"valid":false`) {
		t.Errorf("want valid:false in body, got %s", w.Body.String())
	}
}

func TestHandleApiDashboardsSpecValidateQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, assertErr("boom")
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/spec/validate", strings.NewReader(
		`{"spec":{"template_id":"gauge_kpi","sql":{"mode":"raw","override_sql":"SELECT 1 AS value"}}}`))
	s.handleApiDashboardsSpecValidate(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400 on query error, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"valid":false`) {
		t.Errorf("want valid:false in body, got %s", w.Body.String())
	}
}

func TestHandleApiDashboardsSpecValidateSuccessEmptyResult(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return &store.Result{Columns: []string{"value"}}, nil // 0 rows -> noDataPlaceholder, no error
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/spec/validate", strings.NewReader(
		`{"spec":{"template_id":"gauge_kpi","sql":{"mode":"raw","override_sql":"SELECT 1 AS value"}}}`))
	s.handleApiDashboardsSpecValidate(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"valid":true`) {
		t.Errorf("want valid:true in body, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"row_count":0`) {
		t.Errorf("want row_count:0, got %s", w.Body.String())
	}
}
