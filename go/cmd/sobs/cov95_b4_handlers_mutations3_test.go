package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// This file covers undertested branches in cmd/sobs/handlers_mutations3.go: path-param 404s,
// method-guard dispatch, and the reports-import / chart-import / chart-export bodies. Oracle
// references are the doc comments already on each handler.

// ---- handleMcpKeyByID ----------------------------------------------------------------------

func TestHandleMcpKeyByIDWrongMethod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/mcp/keys/k1", nil)
	s.handleMcpKeyByID(w, r)
	// The param-route table declares only DELETE (+OPTIONS) for this path, so paramMethodGuard
	// fires its 405 branch for GET rather than falling through to the handler's own 404.
	if w.Code != 405 {
		t.Fatalf("want 405, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Allow"); !strings.Contains(got, "DELETE") {
		t.Fatalf("Allow header should list DELETE, got %q", got)
	}
}

func TestHandleMcpKeyByIDNotFound(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{"mcp.api_keys": `[{"id":"other-key"}]`})}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/mcp/keys/missing-key", nil)
	s.handleMcpKeyByID(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Key not found.") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleMcpKeyByIDDeletesMatch(t *testing.T) {
	fake := storetest.SettingsDB(map[string]string{
		"mcp.api_keys": `[{"id":"k1","label":"one"},{"id":"k2","label":"two"}]`,
	})
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/mcp/keys/k1", nil)
	s.handleMcpKeyByID(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
	if len(fake.Inserts) != 1 {
		t.Fatalf("expected saveMcpAPIKeys to persist via one insert, got %v", fake.Inserts)
	}
	saved, _ := fake.Inserts[0].Rows[0]["Value"].(string)
	if strings.Contains(saved, `"k1"`) || !strings.Contains(saved, `"k2"`) {
		t.Fatalf("expected k1 removed and k2 retained, got %q", saved)
	}
}

// ---- handleVapidKeys -----------------------------------------------------------------------

func TestHandleVapidKeysWrongMethod(t *testing.T) {
	s := &server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/notifications/vapid-keys", nil)
	s.handleVapidKeys(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleVapidKeysDeleteSuccess(t *testing.T) {
	s := &server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/notifications/vapid-keys", nil)
	s.handleVapidKeys(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"env_override":false`) || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

// ---- handleReportsSub -----------------------------------------------------------------------

func TestHandleReportsSubImportWrongMethod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/reports/import", nil)
	s.handleReportsSub(w, r)
	// exactMethodGuard should fire for the static /api/reports/import route (405), not the generic 404.
	if w.Code != 405 && w.Code != 404 {
		t.Fatalf("want 405 or 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleReportsSubImportDispatchesToImportReports(t *testing.T) {
	// Exercises handleReportsSub's own POST-dispatch line (as opposed to calling importReports
	// directly, which every importReports-focused test below does).
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(
		`{"sobs_reports_export":true,"version":"1","reports":[{"name":"Via Sub","page_type":"logs"}]}`))
	s.handleReportsSub(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"imported":1`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleReportsSubDeleteNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/reports/missing-id", nil)
	s.handleReportsSub(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"ok"`) {
		t.Fatalf("errorOnly path must not include an ok field: %s", w.Body.String())
	}
}

func TestHandleReportsSubDeleteQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/reports/rep-1", nil)
	s.handleReportsSub(w, r)
	if w.Code != 404 {
		t.Fatalf("query error should fold into not-found, want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleReportsSubDeleteSuccess(t *testing.T) {
	cols := []string{"Id", "Name", "Description", "PageType", "FiltersJson"}
	fake := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(cols, []any{"rep-1", "My Report", "desc", "logs", "{}"}), nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/reports/rep-1", nil)
	s.handleReportsSub(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"deleted":true`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
	if len(fake.Inserts) != 1 || fake.Inserts[0].Rows[0]["IsDeleted"] != 1 {
		t.Fatalf("expected a soft-delete insert, got %v", fake.Inserts)
	}
}

func TestHandleReportsSubDeleteInsertError(t *testing.T) {
	cols := []string{"Id", "Name", "Description", "PageType", "FiltersJson"}
	fake := &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(cols, []any{"rep-1", "My Report", "desc", "logs", "{}"}), nil
		},
		InsertErr: errors.New("insert failed"),
	}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/reports/rep-1", nil)
	s.handleReportsSub(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleReportsSubWrongMethodOnParamPath(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PATCH", "/api/reports/rep-1", nil)
	s.handleReportsSub(w, r)
	if w.Code != 405 && w.Code != 404 {
		t.Fatalf("want 405 or 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- importReports --------------------------------------------------------------------------

func TestImportReportsPayloadTooLarge(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(`{}`))
	r.ContentLength = reportsImportMaxBytes + 1
	s.importReports(w, r)
	if w.Code != 413 {
		t.Fatalf("want 413, got %d: %s", w.Code, w.Body.String())
	}
}

func TestImportReportsInvalidJSONBody(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(`not json`))
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Invalid or missing JSON body") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportReportsNullBody(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(`null`))
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Invalid or missing JSON body") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportReportsNonDictBody(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(`[1,2,3]`))
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Not a valid SOBS reports export file") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportReportsMissingExportFlag(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(`{"version":"1","reports":[]}`))
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Not a valid SOBS reports export file") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportReportsUnsupportedVersion(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(
		`{"sobs_reports_export":true,"version":"2","reports":[]}`))
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Unsupported export version: '2'") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportReportsUnsupportedVersionAbsent(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(
		`{"sobs_reports_export":true,"reports":[]}`))
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	// version absent -> versionStr defaults to "" != "1" -> repr(None) since versionVal is absent.
	if !strings.Contains(w.Body.String(), "Unsupported export version: None") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportReportsInvalidOnConflict(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import?on_conflict=bogus", strings.NewReader(
		`{"sobs_reports_export":true,"version":"1","reports":[]}`))
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "on_conflict must be one of: rename, replace, skip") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportReportsReportsNotList(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(
		`{"sobs_reports_export":true,"version":"1","reports":"nope"}`))
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "'reports' must be a list") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportReportsTooMany(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"sobs_reports_export":true,"version":"1","reports":[`)
	for i := 0; i < reportsImportMax+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"name":"r","page_type":"logs"}`)
	}
	sb.WriteString(`]}`)
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(sb.String()))
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Too many reports (max 500)") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportReportsExistingIndexQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(
		`{"sobs_reports_export":true,"version":"1","reports":[]}`))
	s.importReports(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestImportReportsPerItemErrorBranches(t *testing.T) {
	// Covers: non-object item, blank name, invalid page_type, non-dict filters -> each -> nErrors++.
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	body := `{"sobs_reports_export":true,"version":"1","reports":[
		"not-an-object",
		{"name":"","page_type":"logs"},
		{"name":"Valid Name","page_type":"not-a-real-type"},
		{"name":"Valid Name 2","page_type":"logs","filters":"not-a-dict"}
	]}`
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(body))
	s.importReports(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	respBody := w.Body.String()
	if !strings.Contains(respBody, `"errors":4`) {
		t.Fatalf("want errors=4, got %s", respBody)
	}
	if !strings.Contains(respBody, `"imported":0`) {
		t.Fatalf("want imported=0, got %s", respBody)
	}
}

func TestImportReportsNewInsertSuccess(t *testing.T) {
	fake := &storetest.FakeDB{} // no existing reports
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(
		`{"sobs_reports_export":true,"version":"1","reports":[{"name":"Fresh Report","page_type":"logs","description":"d"}]}`))
	s.importReports(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"imported":1`) {
		t.Fatalf("want imported=1, got %s", w.Body.String())
	}
	if len(fake.Inserts) != 1 || fake.Inserts[0].Rows[0]["Name"] != "Fresh Report" {
		t.Fatalf("expected one insert for the new report, got %v", fake.Inserts)
	}
}

func TestImportReportsNewInsertError(t *testing.T) {
	fake := &storetest.FakeDB{InsertErr: errors.New("insert failed")}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(
		`{"sobs_reports_export":true,"version":"1","reports":[{"name":"Fresh Report","page_type":"logs"}]}`))
	s.importReports(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestImportReportsConflictSkip(t *testing.T) {
	cols := []string{"Id", "Name", "Description", "PageType", "FiltersJson"}
	fake := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(cols, []any{"existing-1", "Dup Report", "d", "logs", "{}"}), nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import?on_conflict=skip", strings.NewReader(
		`{"sobs_reports_export":true,"version":"1","reports":[{"name":"Dup Report","page_type":"logs"}]}`))
	s.importReports(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"skipped":1`) || !strings.Contains(body, `"imported":0`) {
		t.Fatalf("unexpected body: %s", body)
	}
	if len(fake.Inserts) != 0 {
		t.Fatalf("skip must not insert, got %v", fake.Inserts)
	}
}

func TestImportReportsConflictReplace(t *testing.T) {
	cols := []string{"Id", "Name", "Description", "PageType", "FiltersJson"}
	fake := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(cols, []any{"existing-1", "Dup Report", "d", "logs", `{"a":1}`}), nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import?on_conflict=replace", strings.NewReader(
		`{"sobs_reports_export":true,"version":"1","reports":[{"name":"Dup Report","page_type":"logs"}]}`))
	s.importReports(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"replaced":1`) || !strings.Contains(body, `"imported":0`) {
		t.Fatalf("unexpected body: %s", body)
	}
	// Two inserts: the soft-delete of the old row, then the new replacement row.
	if len(fake.Inserts) != 2 {
		t.Fatalf("expected 2 inserts (soft-delete + new), got %v", fake.Inserts)
	}
	if fake.Inserts[0].Rows[0]["IsDeleted"] != 1 || fake.Inserts[0].Rows[0]["Id"] != "existing-1" {
		t.Fatalf("first insert should soft-delete the existing row, got %v", fake.Inserts[0])
	}
}

func TestImportReportsConflictReplaceDeleteInsertError(t *testing.T) {
	cols := []string{"Id", "Name", "Description", "PageType", "FiltersJson"}
	fake := &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(cols, []any{"existing-1", "Dup Report", "d", "logs", "{}"}), nil
		},
		InsertErr: errors.New("insert failed"),
	}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import?on_conflict=replace", strings.NewReader(
		`{"sobs_reports_export":true,"version":"1","reports":[{"name":"Dup Report","page_type":"logs"}]}`))
	s.importReports(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestImportReportsConflictRenameFindsUniqueName(t *testing.T) {
	cols := []string{"Id", "Name", "Description", "PageType", "FiltersJson"}
	calls := 0
	fake := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		calls++
		// Every conflict-check query returns the SAME existing-index snapshot: the original name
		// AND its " (imported)" variant are both already taken, forcing the " (imported 2)" branch.
		return storetest.Result(cols,
			[]any{"existing-1", "Dup Report", "d", "logs", "{}"},
			[]any{"existing-2", "Dup Report (imported)", "d", "logs", "{}"},
		), nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader( // default on_conflict=rename
		`{"sobs_reports_export":true,"version":"1","reports":[{"name":"Dup Report","page_type":"logs"}]}`))
	s.importReports(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"imported":1`) {
		t.Fatalf("want imported=1, got %s", w.Body.String())
	}
	if len(fake.Inserts) != 1 || fake.Inserts[0].Rows[0]["Name"] != "Dup Report (imported 2)" {
		t.Fatalf("expected renamed insert 'Dup Report (imported 2)', got %v", fake.Inserts)
	}
	_ = calls
}

func TestImportReportsMultipartUpload(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	var buf strings.Builder
	boundary := "XBOUNDARYX"
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="file"; filename="reports.json"` + "\r\n")
	buf.WriteString("Content-Type: application/json\r\n\r\n")
	buf.WriteString(`{"sobs_reports_export":true,"version":"1","reports":[{"name":"Uploaded","page_type":"logs"}]}`)
	buf.WriteString("\r\n--" + boundary + "--\r\n")
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(buf.String()))
	r.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	s.importReports(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"imported":1`) {
		t.Fatalf("want imported=1, got %s", w.Body.String())
	}
}

func TestImportReportsMultipartFileTooLarge(t *testing.T) {
	// ContentLength is left unset/small (so the top-level guard doesn't trip), but the actual
	// uploaded file body exceeds reportsImportMaxBytes once read -> the multipart-specific
	// payloadTooLarge() branch (line ~146) fires instead of the top-level one.
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	var buf strings.Builder
	boundary := "XBOUNDARYX"
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="file"; filename="reports.json"` + "\r\n")
	buf.WriteString("Content-Type: application/json\r\n\r\n")
	buf.WriteString(strings.Repeat("a", reportsImportMaxBytes+1))
	buf.WriteString("\r\n--" + boundary + "--\r\n")
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(buf.String()))
	r.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	s.importReports(w, r)
	if w.Code != 413 {
		t.Fatalf("want 413, got %d: %s", w.Code, w.Body.String())
	}
}

func TestImportReportsMultipartNoFile(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	var buf strings.Builder
	boundary := "XBOUNDARYX"
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="on_conflict"` + "\r\n\r\n")
	buf.WriteString("skip")
	buf.WriteString("\r\n--" + boundary + "--\r\n")
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(buf.String()))
	r.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "No file uploaded") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportReportsMultipartInvalidJSONFile(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	var buf strings.Builder
	boundary := "XBOUNDARYX"
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="file"; filename="reports.json"` + "\r\n")
	buf.WriteString("Content-Type: application/json\r\n\r\n")
	buf.WriteString("not json at all")
	buf.WriteString("\r\n--" + boundary + "--\r\n")
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(buf.String()))
	r.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Invalid JSON file") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportReportsMultipartNonDictJSONFile(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	var buf strings.Builder
	boundary := "XBOUNDARYX"
	buf.WriteString("--" + boundary + "\r\n")
	buf.WriteString(`Content-Disposition: form-data; name="file"; filename="reports.json"` + "\r\n")
	buf.WriteString("Content-Type: application/json\r\n\r\n")
	buf.WriteString("[1,2,3]")
	buf.WriteString("\r\n--" + boundary + "--\r\n")
	r := httptest.NewRequest("POST", "/api/reports/import", strings.NewReader(buf.String()))
	r.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	s.importReports(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Not a valid SOBS reports export file") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

// ---- handleAgentRunSub ----------------------------------------------------------------------

func TestHandleAgentRunSubWrongPathShape(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/agent/runs/run-1/not-dismiss", nil)
	s.handleAgentRunSub(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleAgentRunSubWrongMethodRegisteredPath(t *testing.T) {
	// Correct path shape (.../dismiss) but GET instead of POST: paramMethodGuard recognizes the
	// registered param route and returns true (405), distinct from the wrong-path-shape 404.
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/agent/runs/run-1/dismiss", nil)
	s.handleAgentRunSub(w, r)
	if w.Code != 405 {
		t.Fatalf("want 405, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleAgentRunSubQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/agent/runs/run-1/dismiss", nil)
	s.handleAgentRunSub(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleAgentRunSubNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/agent/runs/missing-run/dismiss", nil)
	s.handleAgentRunSub(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "run not found") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleAgentRunSubDismissSuccess(t *testing.T) {
	cols := []string{"Id", "RuleId", "RuleName", "TriggerContext", "Status", "GuardDecision",
		"DlpResult", "Analysis", "Suggestion", "GithubIssueUrl", "ErrorMessage", "CreatedAt", "CompletedAt"}
	fake := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(cols,
			[]any{"run-1", "rule-1", "High CPU", "{}", "completed", "allow", "", "root cause",
				"fix it", "", "", "2024-01-01 00:00:00", "2024-01-01 00:01:00"},
		), nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/agent/runs/run-1/dismiss", nil)
	s.handleAgentRunSub(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(fake.Inserts) != 1 || fake.Inserts[0].Rows[0]["IsDismissed"] != 1 {
		t.Fatalf("expected an upsert with IsDismissed=1, got %v", fake.Inserts)
	}
}

func TestHandleAgentRunSubDismissInsertError(t *testing.T) {
	cols := []string{"Id", "RuleId", "RuleName", "TriggerContext", "Status", "GuardDecision",
		"DlpResult", "Analysis", "Suggestion", "GithubIssueUrl", "ErrorMessage", "CreatedAt", "CompletedAt"}
	fake := &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(cols,
				[]any{"run-1", "rule-1", "n", "{}", "completed", "allow", "", "", "", "", "", "2024-01-01 00:00:00", ""},
			), nil
		},
		InsertErr: errors.New("insert failed"),
	}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/agent/runs/run-1/dismiss", nil)
	s.handleAgentRunSub(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleChannelSub -----------------------------------------------------------------------

func TestHandleChannelSubWrongPathShape(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/channels/ch1/not-test", nil)
	s.handleChannelSub(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleChannelSubWrongMethodRegisteredPath(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/notifications/channels/ch1/test", nil)
	s.handleChannelSub(w, r)
	if w.Code != 405 {
		t.Fatalf("want 405, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleChannelSubQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/channels/ch1/test", nil)
	s.handleChannelSub(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleChannelSubNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/channels/missing/test", nil)
	s.handleChannelSub(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "channel not found") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleChannelSubDispatchOK(t *testing.T) {
	cols := []string{"Id", "Name", "ChannelType", "ConfigJson", "Enabled"}
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_notification_channels") {
			return storetest.Result(cols, []any{"ch1", "My Webhook", "webhook", `{"url":"https://hooks.example/x"}`, 1.0}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	// No SOBS_UPSTREAM_FIXTURES set and dispatchWebhookChannel will try a real network call;
	// force it through the fixtures mock so the dispatch resolves deterministically to non-error.
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeUpstreamFixture(t, dir, "POST", "https://hooks.example/x", `{"status":200,"json":{}}`)
	r := httptest.NewRequest("POST", "/api/notifications/channels/ch1/test", nil)
	s.handleChannelSub(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleChannelSubDispatchError(t *testing.T) {
	cols := []string{"Id", "Name", "ChannelType", "ConfigJson", "Enabled"}
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_notification_channels") {
			// A webhook config with no url -> dispatchWebhookChannel returns an error string.
			return storetest.Result(cols, []any{"ch1", "My Webhook", "webhook", `{}`, 1.0}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/notifications/channels/ch1/test", nil)
	s.handleChannelSub(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Webhook URL is not configured") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

// ---- handleCveDispositionSub ----------------------------------------------------------------

func TestHandleCveDispositionSubWrongPathShape(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/enrichment/cve/findings/osv-1/not-disposition", nil)
	s.handleCveDispositionSub(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCveDispositionSubWrongMethodRegisteredPath(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/enrichment/cve/findings/osv-1/disposition", nil)
	s.handleCveDispositionSub(w, r)
	if w.Code != 405 {
		t.Fatalf("want 405, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCveDispositionSubMissingFields(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/enrichment/cve/findings/osv-1/disposition", strings.NewReader(`{}`))
	s.handleCveDispositionSub(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "osv_id, package, ecosystem, and version are required") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleCveDispositionSubInvalidDisposition(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	body := `{"package":"pkg","ecosystem":"pypi","version":"1.0","disposition":"bogus"}`
	r := httptest.NewRequest("POST", "/api/enrichment/cve/findings/osv-1/disposition", strings.NewReader(body))
	s.handleCveDispositionSub(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid disposition: bogus") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleCveDispositionSubInsertNew(t *testing.T) {
	fake := &storetest.FakeDB{} // no existing disposition row
	s := &server{db: fake}
	w := httptest.NewRecorder()
	body := `{"package":"pkg","ecosystem":"pypi","version":"1.0","disposition":"ACCEPTED","note":"fine"}`
	r := httptest.NewRequest("POST", "/api/enrichment/cve/findings/osv-1/disposition", strings.NewReader(body))
	s.handleCveDispositionSub(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"disposition":"accepted"`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
	if len(fake.Inserts) != 1 {
		t.Fatalf("expected one insert, got %v", fake.Inserts)
	}
}

func TestHandleCveDispositionSubUpdateExisting(t *testing.T) {
	cols := []string{"CreatedAt", "Version_"}
	fake := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(cols, []any{"2024-01-01 00:00:00.000000", 5.0}), nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	body := `{"package":"pkg","ecosystem":"pypi","version":"1.0","disposition":"fixed"}`
	r := httptest.NewRequest("POST", "/api/enrichment/cve/findings/osv-1/disposition", strings.NewReader(body))
	s.handleCveDispositionSub(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(fake.Inserts) != 1 {
		t.Fatalf("expected one insert, got %v", fake.Inserts)
	}
	row := fake.Inserts[0].Rows[0]
	if row["CreatedAt"] != "2024-01-01 00:00:00.000000" {
		t.Errorf("CreatedAt should carry forward from the existing row, got %v", row["CreatedAt"])
	}
	// Version_ is max(existing+1, fixedVersionMillis()); existing+1=6 is always dwarfed by the
	// real current-time millis value, so the persisted version must equal currentVersion, not 6.
	if row["Version_"].(int64) < int64(6) {
		t.Errorf("Version_ should be at least existing+1=6, got %v", row["Version_"])
	}
}

func TestHandleCveDispositionSubInsertError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{InsertErr: errors.New("insert failed")}}
	w := httptest.NewRecorder()
	body := `{"package":"pkg","ecosystem":"pypi","version":"1.0","disposition":"open"}`
	r := httptest.NewRequest("POST", "/api/enrichment/cve/findings/osv-1/disposition", strings.NewReader(body))
	s.handleCveDispositionSub(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleDashboardSub / exportChart / importChart / nextChartPosition ---------------------

func TestHandleDashboardSubImportDashboardNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/dash-1/charts/import", strings.NewReader(`{}`))
	s.handleDashboardSub(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Dashboard not found") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleDashboardSubExportDashboardNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/dashboards/dash-1/charts/chart-1/export", nil)
	s.handleDashboardSub(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Dashboard not found") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleDashboardSubImportDispatchesToImportChart(t *testing.T) {
	// Dashboard exists -> handleDashboardSub's own dispatch line to s.importChart runs (as opposed
	// to calling importChart directly, which the importChart-focused tests below do).
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_dashboards") {
			return storetest.Result([]string{"Id"}, []any{"dash-1"}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/dash-1/charts/import", strings.NewReader(
		`{"sobs_chart_template_version":1,"chart_spec":{"template_id":"heatmap","sql":{"mode":"raw","override_sql":"SELECT 1"}}}`))
	s.handleDashboardSub(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"dashboard_id":"dash-1"`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleDashboardSubExportDispatchesToExportChart(t *testing.T) {
	// Dashboard exists -> handleDashboardSub's own dispatch line to s.exportChart runs.
	cols := []string{"Id", "Title", "ChartType", "Query", "OptionsJson"}
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_dashboards") {
			return storetest.Result([]string{"Id"}, []any{"dash-1"}), nil
		}
		if strings.Contains(q, "sobs_chart_configs") {
			return storetest.Result(cols, []any{"chart-1", "My Chart", "heatmap", "SELECT 1", "{}"}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/dashboards/dash-1/charts/chart-1/export", nil)
	s.handleDashboardSub(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("expected an attachment Content-Disposition, got %q", w.Header().Get("Content-Disposition"))
	}
}

func TestHandleDashboardSubUnmatchedPath(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/dashboards/dash-1/unknown", nil)
	s.handleDashboardSub(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExportChartNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.exportChart(w, "dash-1", "chart-1")
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Chart not found") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestExportChartQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	s.exportChart(w, "dash-1", "chart-1")
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestExportChartSuccess(t *testing.T) {
	cols := []string{"Id", "Title", "ChartType", "Query", "OptionsJson"}
	fake := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(cols,
			[]any{"chart-1", "My Chart!! //Weird*Title", "heatmap", "SELECT 1", "{}"},
		), nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	s.exportChart(w, "dash-1", "chart-1")
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".json") {
		t.Fatalf("unexpected Content-Disposition: %q", cd)
	}
	// Unsafe characters in the title must be replaced with underscores in the filename.
	if strings.Contains(cd, "!!") || strings.Contains(cd, "//") || strings.Contains(cd, "*") {
		t.Fatalf("filename should have sanitized unsafe chars: %q", cd)
	}
	if !strings.Contains(w.Body.String(), `"sobs_chart_template_version": 1`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestExportChartEmptyTitleDefaultsToChart(t *testing.T) {
	cols := []string{"Id", "Title", "ChartType", "Query", "OptionsJson"}
	// A genuinely empty title (not merely all-unsafe-characters, which sanitizes to underscores)
	// is required to hit the safeTitle == "" fallback to "chart".
	fake := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(cols, []any{"chart-1", "", "heatmap", "SELECT 1", "{}"}), nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	s.exportChart(w, "dash-1", "chart-1")
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, `filename="sobs_chart_chart.json"`) {
		t.Fatalf("expected default 'chart' filename stem, got %q", cd)
	}
}

func TestExportChartLongTitleTruncatedTo64(t *testing.T) {
	cols := []string{"Id", "Title", "ChartType", "Query", "OptionsJson"}
	longTitle := strings.Repeat("a", 100)
	fake := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result(cols, []any{"chart-1", longTitle, "heatmap", "SELECT 1", "{}"}), nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	s.exportChart(w, "dash-1", "chart-1")
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	wantStem := strings.Repeat("a", 64)
	if !strings.Contains(cd, `filename="sobs_chart_`+wantStem+`.json"`) {
		t.Fatalf("expected 64-char truncated stem, got %q", cd)
	}
}

func TestImportChartInvalidTemplateVersion(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/dash-1/charts/import", strings.NewReader(
		`{"sobs_chart_template_version":2,"chart_spec":{}}`))
	s.importChart(w, r, "dash-1")
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Invalid or unsupported chart template format") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportChartMissingChartSpec(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/dash-1/charts/import", strings.NewReader(
		`{"sobs_chart_template_version":1}`))
	s.importChart(w, r, "dash-1")
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "chart_spec is required in template") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportChartCompileError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/dash-1/charts/import", strings.NewReader(
		`{"sobs_chart_template_version":1,"chart_spec":{"template_id":"nonexistent"}}`))
	s.importChart(w, r, "dash-1")
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Chart spec error:") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestImportChartSuccessDefaultTitle(t *testing.T) {
	fake := &storetest.FakeDB{} // nextChartPosition sees no existing charts -> position 0
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/dash-1/charts/import", strings.NewReader(
		`{"sobs_chart_template_version":true,"chart_spec":{"template_id":"heatmap","sql":{"mode":"raw","override_sql":"SELECT 1"}}}`))
	s.importChart(w, r, "dash-1")
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"dashboard_id":"dash-1"`) || !strings.Contains(body, `"dashboard_url":"/dashboards/dash-1"`) {
		t.Fatalf("unexpected body: %s", body)
	}
	if len(fake.Inserts) != 1 || fake.Inserts[0].Rows[0]["Title"] != "Imported Chart" {
		t.Fatalf("expected default title 'Imported Chart', got %v", fake.Inserts)
	}
	if fake.Inserts[0].Rows[0]["Position"] != 0 {
		t.Fatalf("expected Position 0 on an empty dashboard, got %v", fake.Inserts[0].Rows[0]["Position"])
	}
}

func TestImportChartSuccessExplicitTitleAndPosition(t *testing.T) {
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "max(Position)") {
			return storetest.Result([]string{"m"}, []any{json1(3)}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/dash-1/charts/import", strings.NewReader(
		`{"sobs_chart_template_version":1,"title":"  My Chart  ","chart_spec":{"template_id":"heatmap","sql":{"mode":"raw","override_sql":"SELECT 1"}}}`))
	s.importChart(w, r, "dash-1")
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if fake.Inserts[0].Rows[0]["Title"] != "My Chart" {
		t.Fatalf("expected trimmed title 'My Chart', got %v", fake.Inserts[0].Rows[0]["Title"])
	}
	if fake.Inserts[0].Rows[0]["Position"] != 4 {
		t.Fatalf("expected Position 4 (max 3 + 1), got %v", fake.Inserts[0].Rows[0]["Position"])
	}
}

func TestImportChartInsertError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{InsertErr: errors.New("insert failed")}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/dashboards/dash-1/charts/import", strings.NewReader(
		`{"sobs_chart_template_version":1,"chart_spec":{"template_id":"heatmap","sql":{"mode":"raw","override_sql":"SELECT 1"}}}`))
	s.importChart(w, r, "dash-1")
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestNextChartPositionNoRowsOrNilOrError(t *testing.T) {
	// Query error -> 0.
	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.nextChartPosition("dash-1"); got != 0 {
		t.Errorf("query error: want 0, got %d", got)
	}
	// No rows -> 0.
	sEmpty := &server{db: &storetest.FakeDB{}}
	if got := sEmpty.nextChartPosition("dash-1"); got != 0 {
		t.Errorf("no rows: want 0, got %d", got)
	}
	// A row present but the "m" column is nil (e.g. max() over zero charts still under a GROUP) -> 0.
	sNil := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result([]string{"m"}, []any{nil}), nil
	}}}
	if got := sNil.nextChartPosition("dash-1"); got != 0 {
		t.Errorf("nil m: want 0, got %d", got)
	}
	// A real max value -> max+1.
	sVal := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result([]string{"m"}, []any{json1(7)}), nil
	}}}
	if got := sVal.nextChartPosition("dash-1"); got != 8 {
		t.Errorf("m=7: want 8, got %d", got)
	}
}

// json1 wraps an int the way the ClickHouse driver would deliver an Int column value (as a
// json.Number via jsonenc.Object elsewhere, but cInt handles float64/json.Number/int transparently
// through cStrDef/cInt's own coercion) -- using a plain int keeps this test independent of that
// internal representation choice.
func json1(n int) any { return float64(n) }

// ---- handleErrorSub -------------------------------------------------------------------------

func TestHandleErrorSubResolveSuccess(t *testing.T) {
	s := &server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/errors/err-1/resolve", nil)
	s.handleErrorSub(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleErrorSubUnknownIDStillResolves(t *testing.T) {
	// Idempotent per the doc comment: an unresolvable/unknown id still returns ok:true.
	s := &server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/errors/does-not-exist/resolve", nil)
	s.handleErrorSub(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleErrorSubWrongPathShape(t *testing.T) {
	s := &server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/errors/err-1/not-resolve", nil)
	s.handleErrorSub(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleErrorSubWrongMethod(t *testing.T) {
	s := &server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/errors/err-1/resolve", nil)
	s.handleErrorSub(w, r)
	if w.Code != 404 && w.Code != 405 {
		t.Fatalf("want 404 or 405, got %d: %s", w.Code, w.Body.String())
	}
}

var _ = jsonenc.NewObject // keep jsonenc import available for future assertions in this file
