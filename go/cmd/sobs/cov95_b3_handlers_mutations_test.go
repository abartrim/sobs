package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Batch 3: cmd/sobs/handlers_mutations.go — data-management backup/restore + query mutation
// handlers. Reuses the storetest.FakeDB seam (internal/store/storetest), matching the pattern
// in dm_s3_backup_test.go / cov95_b3_handlers_forms_test.go.

// ---- handleDmBackupGuard ----

func TestHandleDmBackupGuard_Disabled(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}} // dmBackupEnabled reads an unset setting -> disabled
	req := httptest.NewRequest(http.MethodPost, "/api/data-management/backup/run", nil)
	w := httptest.NewRecorder()
	s.handleDmBackupGuard(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Backup feature is disabled") {
		t.Fatalf("want disabled message, got %s", w.Body.String())
	}
}

func TestHandleDmBackupGuard_RestoreInvalidBackupName(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{"data_management.backup_enabled": "1"})}
	req := httptest.NewRequest(http.MethodPost, "/api/data-management/backup/restore",
		strings.NewReader(`{"backup_name":"bad name!"}`))
	w := httptest.NewRecorder()
	s.handleDmBackupGuard(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unsupported characters") {
		t.Fatalf("want unsupported-characters message, got %s", w.Body.String())
	}
}

func TestHandleDmBackupGuard_RestoreDelegatesToRunDmRestore(t *testing.T) {
	// Enabled, empty backup_name (skips the regex-name guard) -> runDmRestore's own
	// "backup_name is required" branch fires (ok=false surfaced as 200 {ok:false,...}).
	s := &server{db: storetest.SettingsDB(map[string]string{"data_management.backup_enabled": "1"})}
	req := httptest.NewRequest(http.MethodPost, "/api/data-management/backup/restore", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.handleDmBackupGuard(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":false`) || !strings.Contains(w.Body.String(), "backup_name is required") {
		t.Fatalf("want ok:false backup_name-required body, got %s", w.Body.String())
	}
}

func TestHandleDmBackupGuard_BackupTypeDefaultsToFull(t *testing.T) {
	// Enabled, unrecognized "type" value -> falls back to "full"; no S3 bucket configured ->
	// runDmBackup's "S3 bucket is not configured" branch (ok=false).
	s := &server{db: storetest.SettingsDB(map[string]string{"data_management.backup_enabled": "1"})}
	req := httptest.NewRequest(http.MethodPost, "/api/data-management/backup/run", strings.NewReader(`{"type":"bogus"}`))
	w := httptest.NewRecorder()
	s.handleDmBackupGuard(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "S3 bucket is not configured") {
		t.Fatalf("want S3-not-configured message, got %s", w.Body.String())
	}
}

// ---- runDmBackup ----

func TestRunDmBackup_NoS3Bucket(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	ok, msg := s.runDmBackup("full")
	if ok || msg != "S3 bucket is not configured" {
		t.Fatalf("got ok=%v msg=%q", ok, msg)
	}
}

func TestRunDmBackup_InvalidBucketValidationError(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{
		"data_management.s3_bucket": "bad bucket name!",
	})}
	ok, msg := s.runDmBackup("full")
	if ok || !strings.Contains(msg, "unsupported characters") {
		t.Fatalf("got ok=%v msg=%q", ok, msg)
	}
}

func TestRunDmBackup_FullSuccess(t *testing.T) {
	settings := map[string]string{"data_management.s3_bucket": "my-bucket"}
	var executed []string
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if len(params) == 1 {
			if key, ok := params[0].(string); ok {
				if v, present := settings[key]; present {
					return storetest.Result([]string{"Value"}, []any{v}), nil
				}
				return &store.Result{}, nil
			}
		}
		executed = append(executed, q)
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmBackup("full")
	if !ok {
		t.Fatalf("want success, got msg=%q", msg)
	}
	if !strings.Contains(msg, "started successfully") {
		t.Fatalf("want success message, got %q", msg)
	}
	found := false
	for _, q := range executed {
		if strings.HasPrefix(q, "BACKUP ALL TO S3(") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a BACKUP ALL TO S3(...) statement executed, got %v", executed)
	}
}

func TestRunDmBackup_IncrementalWithBaseBackup(t *testing.T) {
	settings := map[string]string{"data_management.s3_bucket": "my-bucket"}
	var executed []string
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if len(params) == 1 {
			if key, ok := params[0].(string); ok {
				if v, present := settings[key]; present {
					return storetest.Result([]string{"Value"}, []any{v}), nil
				}
				return &store.Result{}, nil
			}
		}
		if strings.Contains(q, "system.backups") {
			cols := []string{"name", "status", "start_time", "end_time", "num_files", "total_size", "error"}
			return storetest.Result(cols,
				[]any{"sobs-full-20240101T000000Z", "BACKUP_COMPLETE", nil, nil, nil, nil, nil},
			), nil
		}
		executed = append(executed, q)
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmBackup("incremental")
	if !ok {
		t.Fatalf("want success, got msg=%q", msg)
	}
	found := false
	for _, q := range executed {
		if strings.Contains(q, "BASE_BACKUP") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a BASE_BACKUP clause, got %v", executed)
	}
}

func TestRunDmBackup_IncrementalBaseBackupBuildError(t *testing.T) {
	// The most recent completed backup has an unsafe name -> buildS3BackupDest fails for it,
	// surfaced as the overall runDmBackup error.
	settings := map[string]string{"data_management.s3_bucket": "my-bucket"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if len(params) == 1 {
			if key, ok := params[0].(string); ok {
				if v, present := settings[key]; present {
					return storetest.Result([]string{"Value"}, []any{v}), nil
				}
				return &store.Result{}, nil
			}
		}
		if strings.Contains(q, "system.backups") {
			cols := []string{"name", "status", "start_time", "end_time", "num_files", "total_size", "error"}
			return storetest.Result(cols,
				[]any{"sobs-bad name!", "BACKUP_COMPLETE", nil, nil, nil, nil, nil},
			), nil
		}
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmBackup("incremental")
	if ok || !strings.Contains(msg, "unsupported characters") {
		t.Fatalf("got ok=%v msg=%q", ok, msg)
	}
}

func TestRunDmBackup_EncryptionEnabledMissingPassword(t *testing.T) {
	settings := map[string]string{
		"data_management.s3_bucket":         "my-bucket",
		"data_management.s3_encrypt_backup": "1",
	}
	s := &server{db: storetest.SettingsDB(settings)}
	ok, msg := s.runDmBackup("full")
	if ok || msg != "Backup encryption is enabled but no encryption password is configured" {
		t.Fatalf("got ok=%v msg=%q", ok, msg)
	}
}

func TestRunDmBackup_EncryptionEnabledWithPassword(t *testing.T) {
	settings := map[string]string{
		"data_management.s3_bucket":                  "my-bucket",
		"data_management.s3_encrypt_backup":          "1",
		"data_management.backup_encryption_password": "hunter2",
	}
	var executed []string
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if len(params) == 1 {
			if key, ok := params[0].(string); ok {
				if v, present := settings[key]; present {
					return storetest.Result([]string{"Value"}, []any{v}), nil
				}
				return &store.Result{}, nil
			}
		}
		executed = append(executed, q)
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmBackup("full")
	if !ok {
		t.Fatalf("want success, got msg=%q", msg)
	}
	found := false
	for _, q := range executed {
		if strings.Contains(q, "SETTINGS compression_method='lz4'") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want encryption SETTINGS clause, got %v", executed)
	}
}

func TestRunDmBackup_BackupExecuteError(t *testing.T) {
	settings := map[string]string{"data_management.s3_bucket": "my-bucket"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if len(params) == 1 {
			if key, ok := params[0].(string); ok {
				if v, present := settings[key]; present {
					return storetest.Result([]string{"Value"}, []any{v}), nil
				}
				return &store.Result{}, nil
			}
		}
		if strings.HasPrefix(q, "BACKUP ALL TO") {
			return nil, errors.New("chdb boom")
		}
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmBackup("full")
	if ok || msg != "chdb boom" {
		t.Fatalf("got ok=%v msg=%q", ok, msg)
	}
}

// ---- runDmRestore ----

func TestRunDmRestore_EmptyBackupName(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	ok, msg := s.runDmRestore("")
	if ok || msg != "backup_name is required" {
		t.Fatalf("got ok=%v msg=%q", ok, msg)
	}
}

func TestRunDmRestore_NoS3Bucket(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	ok, msg := s.runDmRestore("sobs-full-1")
	if ok || msg != "S3 bucket is not configured" {
		t.Fatalf("got ok=%v msg=%q", ok, msg)
	}
}

func TestRunDmRestore_DestBuildError(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{"data_management.s3_bucket": "bad bucket!"})}
	ok, msg := s.runDmRestore("sobs-full-1")
	if ok || !strings.Contains(msg, "unsupported characters") {
		t.Fatalf("got ok=%v msg=%q", ok, msg)
	}
}

func TestRunDmRestore_ExecuteError(t *testing.T) {
	settings := map[string]string{"data_management.s3_bucket": "my-bucket"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if len(params) == 1 {
			if key, ok := params[0].(string); ok {
				if v, present := settings[key]; present {
					return storetest.Result([]string{"Value"}, []any{v}), nil
				}
				return &store.Result{}, nil
			}
		}
		if strings.HasPrefix(q, "RESTORE ALL FROM") {
			return nil, errors.New("restore boom")
		}
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmRestore("sobs-full-1")
	if ok || msg != "restore boom" {
		t.Fatalf("got ok=%v msg=%q", ok, msg)
	}
}

func TestRunDmRestore_Success(t *testing.T) {
	settings := map[string]string{"data_management.s3_bucket": "my-bucket"}
	var executed []string
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if len(params) == 1 {
			if key, ok := params[0].(string); ok {
				if v, present := settings[key]; present {
					return storetest.Result([]string{"Value"}, []any{v}), nil
				}
				return &store.Result{}, nil
			}
		}
		executed = append(executed, q)
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmRestore("sobs-full-1")
	if !ok || !strings.Contains(msg, "started successfully") {
		t.Fatalf("got ok=%v msg=%q", ok, msg)
	}
	found := false
	for _, q := range executed {
		if strings.HasPrefix(q, "RESTORE ALL FROM S3(") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want RESTORE ALL FROM S3(...) executed, got %v", executed)
	}
}

// ---- handleApiQueryAddToDashboard ----

func TestHandleApiQueryAddToDashboard_MissingDashboardID(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/query/add-to-dashboard", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.handleApiQueryAddToDashboard(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "dashboard_id is required") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleApiQueryAddToDashboard_MissingSQL(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/query/add-to-dashboard",
		strings.NewReader(`{"dashboard_id":"d1"}`))
	w := httptest.NewRecorder()
	s.handleApiQueryAddToDashboard(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "sql is required") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleApiQueryAddToDashboard_MissingChartSpec(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/query/add-to-dashboard",
		strings.NewReader(`{"dashboard_id":"d1","sql":"SELECT 1"}`))
	w := httptest.NewRecorder()
	s.handleApiQueryAddToDashboard(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "chart_spec is required") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleApiQueryAddToDashboard_DashboardNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/query/add-to-dashboard",
		strings.NewReader(`{"dashboard_id":"missing","sql":"SELECT 1","chart_spec":{"a":1}}`))
	w := httptest.NewRecorder()
	s.handleApiQueryAddToDashboard(w, req)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Dashboard not found") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleApiQueryAddToDashboard_DashboardQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("boom") },
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/query/add-to-dashboard",
		strings.NewReader(`{"dashboard_id":"d1","sql":"SELECT 1","chart_spec":{"a":1}}`))
	w := httptest.NewRecorder()
	s.handleApiQueryAddToDashboard(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiQueryAddToDashboard_ChartSpecStringInvalidJSON(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name", "Description"}, []any{"d1", "Dash", "desc"}), nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/query/add-to-dashboard",
		strings.NewReader(`{"dashboard_id":"d1","sql":"SELECT 1","chart_spec":"not-json"}`))
	w := httptest.NewRecorder()
	s.handleApiQueryAddToDashboard(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "chart_spec must be valid JSON") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleApiQueryAddToDashboard_ChartSpecStringNotObject(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name", "Description"}, []any{"d1", "Dash", "desc"}), nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/query/add-to-dashboard",
		strings.NewReader(`{"dashboard_id":"d1","sql":"SELECT 1","chart_spec":"[1,2,3]"}`))
	w := httptest.NewRecorder()
	s.handleApiQueryAddToDashboard(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "chart_spec must be a JSON object") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleApiQueryAddToDashboard_ChartSpecNotObjectDirect(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name", "Description"}, []any{"d1", "Dash", "desc"}), nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/query/add-to-dashboard",
		strings.NewReader(`{"dashboard_id":"d1","sql":"SELECT 1","chart_spec":[1,2,3]}`))
	w := httptest.NewRecorder()
	s.handleApiQueryAddToDashboard(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "chart_spec must be a JSON object") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleApiQueryAddToDashboard_InsertError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_dashboards") {
				return storetest.Result([]string{"Id", "Name", "Description"}, []any{"d1", "Dash", "desc"}), nil
			}
			return &store.Result{}, nil
		},
		InsertErr: errors.New("boom"),
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/query/add-to-dashboard",
		strings.NewReader(`{"dashboard_id":"d1","sql":"SELECT 1","chart_spec":{"foo":"bar"}}`))
	w := httptest.NewRecorder()
	s.handleApiQueryAddToDashboard(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiQueryAddToDashboard_SuccessWithDefaultTitle(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_dashboards") {
				return storetest.Result([]string{"Id", "Name", "Description"}, []any{"d1", "Dash", "desc"}), nil
			}
			return &store.Result{}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/query/add-to-dashboard",
		strings.NewReader(`{"dashboard_id":"d1","sql":"SELECT 1","chart_spec":{"foo":"bar"}}`))
	w := httptest.NewRecorder()
	s.handleApiQueryAddToDashboard(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"dashboard_name":"Dash"`) {
		t.Fatalf("want dashboard_name in response, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"dashboard_url":"/dashboards/d1"`) {
		t.Fatalf("want dashboard_url in response, got %s", w.Body.String())
	}
}

func TestHandleApiQueryAddToDashboard_SuccessWithExplicitTitleAndCompileError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			if strings.Contains(q, "sobs_dashboards") {
				return storetest.Result([]string{"Id", "Name", "Description"}, []any{"d1", "Dash", "desc"}), nil
			}
			return &store.Result{}, nil
		},
	}}
	// custom_echarts template requires custom_option_json to be valid JSON; here it embeds an
	// arbitrary chart_spec object which compileChartSpec accepts as the raw custom option, so this
	// documents the success path with an explicit title rather than forcing a synthetic compile error
	// (compileChartSpec's custom_echarts branch is lenient about the option payload shape).
	req := httptest.NewRequest(http.MethodPost, "/api/query/add-to-dashboard",
		strings.NewReader(`{"dashboard_id":"d1","title":"My Chart","sql":"SELECT 1","chart_spec":{"foo":"bar"}}`))
	w := httptest.NewRecorder()
	s.handleApiQueryAddToDashboard(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"chart_id"`) {
		t.Fatalf("want chart_id in response, got %s", w.Body.String())
	}
}

// ---- handleValidateFilter (residual branches: probe query error + AI variant) ----

func TestHandleValidateFilter_EmptySQL(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/logs/validate-filter", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.handleValidateFilter(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"normalized":""`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleValidateFilter_UnclosedParenAndQuote(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/logs/validate-filter",
		strings.NewReader(`{"sql":"(status = 'ok"}`))
	w := httptest.NewRecorder()
	s.handleValidateFilter(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Unclosed single quote") || !strings.Contains(body, "Unclosed '('") {
		t.Fatalf("want unclosed paren+quote issues, got %s", body)
	}
}

func TestHandleValidateFilter_UnexpectedCloseParen(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/logs/validate-filter",
		strings.NewReader(`{"sql":"status = 1)"}`))
	w := httptest.NewRecorder()
	s.handleValidateFilter(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Unexpected ')'") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleValidateFilter_TrailingOperator(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/logs/validate-filter",
		strings.NewReader(`{"sql":"status = 1 AND"}`))
	w := httptest.NewRecorder()
	s.handleValidateFilter(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ends with an operator") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleValidateFilter_LogsUnsafeKeyword(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/logs/validate-filter",
		strings.NewReader(`{"sql":"1=1; DROP TABLE x"}`))
	w := httptest.NewRecorder()
	s.handleValidateFilter(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleValidateFilter_LogsProbeQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("bad column") },
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/logs/validate-filter",
		strings.NewReader(`{"sql":"status = 1"}`))
	w := httptest.NewRecorder()
	s.handleValidateFilter(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleValidateFilter_LogsSuccess(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/logs/validate-filter",
		strings.NewReader(`{"sql":"status = 1"}`))
	w := httptest.NewRecorder()
	s.handleValidateFilter(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleValidateFilter_AIUnsafeKeyword(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/ai/validate-filter",
		strings.NewReader(`{"sql":"1=1; DROP TABLE x"}`))
	w := httptest.NewRecorder()
	s.handleValidateFilter(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleValidateFilter_AIProbeQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return nil, errors.New("bad column") },
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/ai/validate-filter",
		strings.NewReader(`{"sql":"model = 'gpt'"}`))
	w := httptest.NewRecorder()
	s.handleValidateFilter(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

func TestHandleValidateFilter_AISuccess(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	req := httptest.NewRequest(http.MethodPost, "/api/ai/validate-filter",
		strings.NewReader(`{"sql":"model = 'gpt'"}`))
	w := httptest.NewRecorder()
	s.handleValidateFilter(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
}

// ---- sqlQuoteLiteral (pure helper, tiny branch) ----

func TestSqlQuoteLiteral_EscapesSingleQuote(t *testing.T) {
	if got := sqlQuoteLiteral("it's ok"); got != "'it''s ok'" {
		t.Fatalf("got %q", got)
	}
}
