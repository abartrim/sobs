package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b8_handlers_v1_test.go — batch 8 targeted coverage for cmd/sobs/handlers_v1.go: the app/
// release/artifact registry handlers' branch coverage (not-found, conflict, validation, PATCH
// partial-update, and list/filter paths) plus the appSlug/parseBoolPy pure helpers' edge cases.

// appSlug: falls back when the slugified value is empty (all-symbol input), and truncates a long
// value to 80 chars.
func TestAppSlugFallbackAndTruncate(t *testing.T) {
	if got := appSlug("!!!", "fallback"); got != "fallback" {
		t.Errorf("appSlug(all symbols) = %q, want fallback", got)
	}
	if got := appSlug("", "fallback"); got != "fallback" {
		t.Errorf("appSlug(empty) = %q, want fallback", got)
	}
	long := strings.Repeat("a", 100)
	got := appSlug(long, "fallback")
	if len(got) != 80 {
		t.Errorf("appSlug(long) length = %d, want 80", len(got))
	}
	if got := appSlug("My App!!", "fallback"); got != "my-app" {
		t.Errorf("appSlug(normal) = %q, want my-app", got)
	}
}

// parseBoolPy: nil returns the default; a bool value passes through; recognized string forms in
// both cases; an unrecognized string falls back to the default.
func TestParseBoolPy(t *testing.T) {
	if got := parseBoolPy(nil, true); got != true {
		t.Errorf("parseBoolPy(nil, true) = %v, want true", got)
	}
	if got := parseBoolPy(nil, false); got != false {
		t.Errorf("parseBoolPy(nil, false) = %v, want false", got)
	}
	if got := parseBoolPy(false, true); got != false {
		t.Errorf("parseBoolPy(bool false) = %v, want false", got)
	}
	for _, s := range []string{"1", "true", "TRUE", "yes", "on"} {
		if got := parseBoolPy(s, false); got != true {
			t.Errorf("parseBoolPy(%q) = %v, want true", s, got)
		}
	}
	for _, s := range []string{"0", "false", "FALSE", "no", "off"} {
		if got := parseBoolPy(s, true); got != false {
			t.Errorf("parseBoolPy(%q) = %v, want false", s, got)
		}
	}
	if got := parseBoolPy("garbage", true); got != true {
		t.Errorf("parseBoolPy(unrecognized) = %v, want default true", got)
	}
}

// dbResultRow builds a *store.Result with one row from a map, matching the column shape rowMaps
// expects (columns list + a single positional row of the same values).
func dbResultRow(m map[string]any) *store.Result {
	cols := make([]string, 0, len(m))
	row := make([]any, 0, len(m))
	for k, v := range m {
		cols = append(cols, k)
		row = append(row, v)
	}
	return &store.Result{Columns: cols, Rows: [][]any{row}}
}

// TestHandleV1AppByIDNotFound: GET on an unknown app id returns 404 with the v1 error shape.
func TestHandleV1AppByIDNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/apps/missing-id", nil)
	rec := httptest.NewRecorder()
	s.handleV1AppByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("body = %s, want v1 error shape", rec.Body.String())
	}
}

// TestHandleV1AppByIDWrongMethod: an unsupported method (e.g. DELETE) on /v1/apps/<id> 404s.
func TestHandleV1AppByIDWrongMethod(t *testing.T) {
	// /v1/apps/<id> matches the paramMethodGuard's route template, so a disallowed method (DELETE)
	// is answered by the guard itself with 405 + Allow (Werkzeug's MethodNotAllowed shape) before
	// handleV1AppByID's own "else 404" fallback would ever run.
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodDelete, "/v1/apps/some-id", nil)
	rec := httptest.NewRecorder()
	s.handleV1AppByID(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE status = %d, want 405 (paramMethodGuard)", rec.Code)
	}
}

// TestHandleV1AppByIDCreateRelease covers POST /v1/apps/<id>/releases: app-not-found, missing
// version (400), and the success path (defaults for releasedAt/id).
func TestHandleV1AppByIDCreateRelease(t *testing.T) {
	// App not found.
	sMiss := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	req := httptest.NewRequest(http.MethodPost, "/v1/apps/app1/releases", strings.NewReader(`{"version":"1.0"}`))
	rec := httptest.NewRecorder()
	sMiss.handleV1AppByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("app-not-found status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}

	// App found, but version missing -> 400.
	sFound := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return dbResultRow(map[string]any{"Id": "app1", "IsDeleted": 0}), nil
	}}}
	req = httptest.NewRequest(http.MethodPost, "/v1/apps/app1/releases", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	sFound.handleV1AppByID(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing version status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}

	// App found, version present -> 201 created with server-generated id/releasedAt.
	req = httptest.NewRequest(http.MethodPost, "/v1/apps/app1/releases", strings.NewReader(`{"version":"1.2.3"}`))
	rec = httptest.NewRecorder()
	sFound.handleV1AppByID(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create release status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"version":"1.2.3"`) {
		t.Errorf("body = %s, want version 1.2.3", rec.Body.String())
	}
}

// TestHandleV1AppByIDPatch covers PATCH /v1/apps/<id>: not-found, missing-name (400), slug
// conflict (409), and the success path preserving existing metadata when the payload omits it.
func TestHandleV1AppByIDPatch(t *testing.T) {
	sMiss := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	req := httptest.NewRequest(http.MethodPatch, "/v1/apps/app1", strings.NewReader(`{"name":"x"}`))
	rec := httptest.NewRecorder()
	sMiss.handleV1AppByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("patch not-found status = %d, want 404", rec.Code)
	}

	// Found: build a FakeDB that answers findAppByID with a row, and the slug-conflict SELECT with
	// no rows for the "no conflict" cases, or a row for the "conflict" case.
	makeDB := func(conflict bool) *storetest.FakeDB {
		return &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
			if strings.Contains(query, "Slug=? AND IsDeleted=0 AND Id!=?") {
				if conflict {
					return dbResultRow(map[string]any{"Id": "other-app"}), nil
				}
				return &store.Result{}, nil
			}
			return dbResultRow(map[string]any{
				"Id": "app1", "Name": "Old Name", "Slug": "old-name", "MetadataJson": `{"k":"v"}`,
				"CreatedAt": "2026-01-01T00:00:00.000+00:00", "Enabled": 1,
			}), nil
		}}
	}

	// Missing name (payload explicitly empty string) -> 400.
	sNoName := &server{db: makeDB(false)}
	req = httptest.NewRequest(http.MethodPatch, "/v1/apps/app1", strings.NewReader(`{"name":""}`))
	rec = httptest.NewRecorder()
	sNoName.handleV1AppByID(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}

	// Slug conflict -> 409.
	sConflict := &server{db: makeDB(true)}
	req = httptest.NewRequest(http.MethodPatch, "/v1/apps/app1", strings.NewReader(`{"name":"New Name"}`))
	rec = httptest.NewRecorder()
	sConflict.handleV1AppByID(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("slug conflict status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}

	// Success: name changes, metadata omitted from payload -> preserves current metadata (re-dumped).
	sOK := &server{db: makeDB(false)}
	req = httptest.NewRequest(http.MethodPatch, "/v1/apps/app1", strings.NewReader(`{"name":"New Name"}`))
	rec = httptest.NewRecorder()
	sOK.handleV1AppByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch success status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"New Name"`) {
		t.Errorf("body = %s, want updated name", rec.Body.String())
	}
}

// TestPatchMetadataPreservesCurrentWhenAbsent directly exercises the patchMetadata helper's two
// branches: payload carries "metadata" vs. payload omits it (falls back to current's MetadataJson,
// itself re-dumped through safeJSONDumps since it's a raw string, not a parsed value).
func TestPatchMetadataBranches(t *testing.T) {
	s := &server{}
	current := map[string]any{"MetadataJson": `{"a":1}`}
	got := s.patchMetadata(map[string]any{"metadata": map[string]any{"b": 2}}, current)
	if got != `{"b":2}` {
		t.Errorf("patchMetadata(payload present) = %q, want {\"b\":2}", got)
	}
	got = s.patchMetadata(map[string]any{}, current)
	if got != `{"a":1}` {
		t.Errorf("patchMetadata(payload absent) = %q, want current re-dumped", got)
	}
}

// TestHandleV1AppByIDListReleases covers GET /v1/apps/<id>/releases: app-not-found and the
// success path serializing zero or more release rows.
func TestHandleV1AppByIDListReleases(t *testing.T) {
	sMiss := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/apps/app1/releases", nil)
	rec := httptest.NewRecorder()
	sMiss.handleV1AppByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("app-not-found status = %d, want 404", rec.Code)
	}

	sOK := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		if strings.Contains(query, "sobs_app_releases") && strings.Contains(query, "ORDER BY ReleasedAt") {
			return dbResultRow(map[string]any{
				"Id": "rel1", "AppId": "app1", "ReleaseVersion": "1.0", "MetadataJson": "",
			}), nil
		}
		return dbResultRow(map[string]any{"Id": "app1"}), nil
	}}}
	req = httptest.NewRequest(http.MethodGet, "/v1/apps/app1/releases", nil)
	rec = httptest.NewRecorder()
	sOK.handleV1AppByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list releases status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"version":"1.0"`) {
		t.Errorf("body = %s, want release entry", rec.Body.String())
	}
}

// TestHandleV1ReleaseByIDNotFoundAndWrongMethod exercises handleV1ReleaseByID's guard branches:
// unsupported method 405s (via paramMethodGuard's route template match); GET on an unknown
// release id 404s with the v1 error shape.
func TestHandleV1ReleaseByIDNotFoundAndWrongMethod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	req := httptest.NewRequest(http.MethodDelete, "/v1/releases/rel1", nil)
	rec := httptest.NewRecorder()
	s.handleV1ReleaseByID(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE status = %d, want 405 (paramMethodGuard)", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/releases/missing", nil)
	rec = httptest.NewRecorder()
	s.handleV1ReleaseByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get missing status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleV1ReleaseByIDCreateArtifactMeta covers POST /v1/releases/<id>/artifacts/meta:
// release-not-found, missing required fields (400), and the success path.
func TestHandleV1ReleaseByIDCreateArtifactMeta(t *testing.T) {
	sMiss := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	req := httptest.NewRequest(http.MethodPost, "/v1/releases/rel1/artifacts/meta",
		strings.NewReader(`{"artifactType":"binary","name":"a.bin"}`))
	rec := httptest.NewRecorder()
	sMiss.handleV1ReleaseByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("release-not-found status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}

	sFound := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return dbResultRow(map[string]any{"Id": "rel1"}), nil
	}}}
	req = httptest.NewRequest(http.MethodPost, "/v1/releases/rel1/artifacts/meta", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	sFound.handleV1ReleaseByID(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fields status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/releases/rel1/artifacts/meta",
		strings.NewReader(`{"artifactType":"binary","name":"a.bin"}`))
	rec = httptest.NewRecorder()
	sFound.handleV1ReleaseByID(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create artifact meta status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleV1ReleaseByIDListArtifactsAndGet covers GET /v1/releases/<id>/artifacts
// (release-not-found + success) and GET /v1/releases/<id> (release+artifacts envelope).
func TestHandleV1ReleaseByIDListArtifactsAndGet(t *testing.T) {
	sMiss := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/releases/rel1/artifacts", nil)
	rec := httptest.NewRecorder()
	sMiss.handleV1ReleaseByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("artifacts release-not-found status = %d, want 404", rec.Code)
	}

	sFound := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		if strings.Contains(query, "sobs_release_artifacts") {
			return dbResultRow(map[string]any{"Id": "art1", "ReleaseId": "rel1", "Name": "a.bin"}), nil
		}
		return dbResultRow(map[string]any{"Id": "rel1", "AppId": "app1"}), nil
	}}}
	req = httptest.NewRequest(http.MethodGet, "/v1/releases/rel1/artifacts", nil)
	rec = httptest.NewRecorder()
	sFound.handleV1ReleaseByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list artifacts status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"name":"a.bin"`) {
		t.Errorf("body = %s, want artifact entry", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/releases/rel1", nil)
	rec = httptest.NewRecorder()
	sFound.handleV1ReleaseByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("get release status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"release"`) || !strings.Contains(rec.Body.String(), `"artifacts"`) {
		t.Errorf("body = %s, want release+artifacts envelope", rec.Body.String())
	}
}

// TestArtifactsForReleaseDBError: a DB error returns an empty slice rather than propagating.
func TestArtifactsForReleaseDBError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return nil, http.ErrBodyNotAllowed // any error
	}}}
	got := s.artifactsForRelease("rel1")
	if len(got) != 0 {
		t.Errorf("artifactsForRelease(db error) = %v, want empty slice", got)
	}
}

// TestHandleV1AppsCreate covers POST /v1/apps: missing name (400), slug conflict (409), and the
// success path with a default slug derived from the name.
func TestHandleV1AppsCreate(t *testing.T) {
	sNoConflict := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	req := httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	sNoConflict.handleV1Apps(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing name status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}

	sConflict := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return dbResultRow(map[string]any{"Id": "existing"}), nil
	}}}
	req = httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{"name":"Widgets"}`))
	rec = httptest.NewRecorder()
	sConflict.handleV1Apps(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("slug conflict status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(`{"name":"Widgets"}`))
	rec = httptest.NewRecorder()
	sNoConflict.handleV1Apps(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"slug":"widgets"`) {
		t.Errorf("body = %s, want slug widgets", rec.Body.String())
	}
}

// TestHandleV1AppsListFilter covers GET /v1/apps with a "q" filter that matches on name OR slug
// (case-insensitively), and a DB error surfaced via dbError.
func TestHandleV1AppsListFilter(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return &store.Result{
			Columns: []string{"Id", "Name", "Slug"},
			Rows: [][]any{
				{"1", "Alpha Widgets", "alpha-widgets"},
				{"2", "Beta Gadgets", "beta-gadgets"},
			},
		}, nil
	}}}
	req := httptest.NewRequest(http.MethodGet, "/v1/apps?q=widg", nil)
	rec := httptest.NewRecorder()
	s.handleV1Apps(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Alpha Widgets") {
		t.Errorf("body = %s, want Alpha Widgets matched by q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Beta Gadgets") {
		t.Errorf("body = %s, should NOT contain Beta Gadgets (filtered out)", rec.Body.String())
	}

	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(query string, params ...any) (*store.Result, error) {
		return nil, http.ErrBodyNotAllowed
	}}}
	req = httptest.NewRequest(http.MethodGet, "/v1/apps", nil)
	rec = httptest.NewRecorder()
	sErr.handleV1Apps(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("db error should not produce a 200, got %d", rec.Code)
	}
}
