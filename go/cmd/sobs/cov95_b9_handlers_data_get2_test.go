package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// This file covers batch-9 undertested branches in cmd/sobs/handlers_data.go and
// cmd/sobs/handlers_get2.go — neither file had any dedicated test file before this one. Focus:
// DB-error branches, POST/validation branches, empty-vs-populated result shaping, and small pure
// helpers (werkzeugContentType). Oracle references are the doc comments on each handler.

// ---- handleApiChartTypes ------------------------------------------------------------------

func TestHandleApiChartTypes(t *testing.T) {
	t.Run("missing catalog file -> 404", func(t *testing.T) {
		s := &server{cfg: config{StaticDir: t.TempDir()}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/chart-types", nil)
		s.handleApiChartTypes(w, r)
		if w.Code != 404 {
			t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("present catalog file -> 200 with data", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "echarts-chart-types.json"), []byte(`{"bar":{}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &server{cfg: config{StaticDir: dir}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/chart-types", nil)
		s.handleApiChartTypes(w, r)
		if w.Code != 200 {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), `"bar"`) {
			t.Fatalf("unexpected body: %s", w.Body.String())
		}
	})

	t.Run("malformed catalog JSON -> 500", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "echarts-chart-types.json"), []byte(`{not json`), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &server{cfg: config{StaticDir: dir}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/chart-types", nil)
		s.handleApiChartTypes(w, r)
		if w.Code != 500 {
			t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
		}
	})
}

// ---- handleApiDashboardsList ---------------------------------------------------------------

func TestHandleApiDashboardsList(t *testing.T) {
	t.Run("db error -> error JSON", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		w := httptest.NewRecorder()
		s.handleApiDashboardsList(w, httptest.NewRequest("GET", "/api/dashboards/list", nil))
		if w.Code < 400 {
			t.Fatalf("want an error status, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("populated rows serialize", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"Id", "Name", "Description"},
				[]any{"d1", "Dash1", "desc1"}), nil
		}}}
		w := httptest.NewRecorder()
		s.handleApiDashboardsList(w, httptest.NewRequest("GET", "/api/dashboards/list", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "Dash1") {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})
}

// ---- handleApiReports (GET filter + POST create) ---------------------------------------------

func TestHandleApiReports_GetWithPageTypeFilter(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if !strings.Contains(q, "PageType = ?") || len(params) != 1 || params[0] != "errors" {
			t.Fatalf("unexpected query/params: %s %v", q, params)
		}
		return storetest.Result([]string{"Id", "Name", "Description", "PageType", "FiltersJson"},
			[]any{"r1", "Rep1", "d", "errors", "{}"}), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiReports(w, httptest.NewRequest("GET", "/api/reports?page_type=errors", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Rep1") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiReports_GetDbError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	s.handleApiReports(w, httptest.NewRequest("GET", "/api/reports", nil))
	if w.Code < 400 {
		t.Fatalf("want error status, got %d", w.Code)
	}
}

func TestHandleApiReports_PostValidation(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}

	t.Run("missing name -> 400", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/reports", strings.NewReader(`{"page_type":"errors"}`))
		s.handleApiReports(w, r)
		if w.Code != 400 {
			t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("invalid page_type -> 400", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/reports", strings.NewReader(`{"name":"n","page_type":"bogus"}`))
		s.handleApiReports(w, r)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "page_type must be one of") {
			t.Fatalf("want 400 with message, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("filters not an object -> 400", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/reports", strings.NewReader(`{"name":"n","page_type":"errors","filters":"not-an-object"}`))
		s.handleApiReports(w, r)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "filters must be an object") {
			t.Fatalf("want 400 with message, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("valid create -> 201", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/reports",
			strings.NewReader(`{"name":"My Report","page_type":"errors","description":"d","filters":{"a":1}}`))
		s.handleApiReports(w, r)
		if w.Code != 201 {
			t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "My Report") {
			t.Fatalf("unexpected body: %s", w.Body.String())
		}
	})

	t.Run("db insert error -> error status", func(t *testing.T) {
		sErr := &server{db: &storetest.FakeDB{InsertErr: errors.New("insert failed")}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/reports", strings.NewReader(`{"name":"n","page_type":"errors"}`))
		sErr.handleApiReports(w, r)
		if w.Code < 400 {
			t.Fatalf("want error status, got %d", w.Code)
		}
	})
}

// ---- handleApiAgentRuns (GET listing + POST dispatch) ------------------------------------------

func TestHandleApiAgentRuns_Get(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if !strings.Contains(q, "sobs_agent_runs") {
			t.Fatalf("unexpected query: %s", q)
		}
		return storetest.Result(
			[]string{"Id", "RuleId", "RuleName", "TriggerContext", "Status", "GuardDecision", "DlpResult",
				"Analysis", "Suggestion", "GithubIssueUrl", "ErrorMessage", "CreatedAt", "CompletedAt", "IsDismissed"},
			[]any{"run1", "rule1", "R1", "{}", "completed", "allowed", "skipped", "a", "s", "", "", "t1", "t2", float64(0)},
		), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiAgentRuns(w, httptest.NewRequest("GET", "/api/agent/runs?limit=10", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "run1") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiAgentRuns_GetDbError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	s.handleApiAgentRuns(w, httptest.NewRequest("GET", "/api/agent/runs", nil))
	if w.Code < 400 {
		t.Fatalf("want error status, got %d", w.Code)
	}
}

func TestHandleApiAgentRuns_PostDispatchesToTriggerAgentRun(t *testing.T) {
	// POST should delegate to handleTriggerAgentRun; a missing rule_id yields its 400.
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/agent/runs", strings.NewReader(`{}`))
	s.handleApiAgentRuns(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400 (delegated to handleTriggerAgentRun), got %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiEnrichmentLibraries -----------------------------------------------------------

func TestHandleApiEnrichmentLibraries_CveQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_cve_findings") {
			return nil, errors.New("boom")
		}
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiEnrichmentLibraries(w, httptest.NewRequest("GET", "/api/enrichment/libraries", nil))
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleApiEnrichmentLibraries_EmptyInventory(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiEnrichmentLibraries(w, httptest.NewRequest("GET", "/api/enrichment/libraries", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"libraries":[]`) {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiWorkItems (filters) -------------------------------------------------------------

func TestHandleApiWorkItems_WithFilters(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_github_work_items") && strings.Contains(q, "ServiceName = ?") {
			if len(params) != 1 || params[0] != "svc-a" {
				t.Fatalf("unexpected params: %v", params)
			}
			return storetest.Result([]string{"Id", "IssueUrl", "ServiceName"}, []any{"wi1", "", "svc-a"}), nil
		}
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiWorkItems(w, httptest.NewRequest("GET", "/api/work-items?service=svc-a&limit=9999", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "wi1") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiWorkItems_DbError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	s.handleApiWorkItems(w, httptest.NewRequest("GET", "/api/work-items", nil))
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiDmBackupList ---------------------------------------------------------------------

func TestHandleApiDmBackupList(t *testing.T) {
	t.Run("query error falls back to empty list", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("no such table")
		}}}
		w := httptest.NewRecorder()
		s.handleApiDmBackupList(w, httptest.NewRequest("GET", "/api/data-management/backup/list", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"backups":[]`) {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("populated backups serialize", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"name", "status", "start_time", "end_time", "num_files", "total_size", "error"},
				[]any{"b1", "BACKUP_CREATED", "t1", "t2", "3", "100", ""}), nil
		}}}
		w := httptest.NewRecorder()
		s.handleApiDmBackupList(w, httptest.NewRequest("GET", "/api/data-management/backup/list", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "b1") {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})
}

// ---- web traffic aggregations (browsers/os/timezones/languages/devices): DB-error + populated ----

func TestHandleApiWebTraffic_DbErrors(t *testing.T) {
	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	handlers := map[string]func(http.ResponseWriter, *http.Request){
		"browsers":  sErr.handleApiWebTrafficBrowsers,
		"os":        sErr.handleApiWebTrafficOS,
		"timezones": sErr.handleApiWebTrafficTimezones,
		"languages": sErr.handleApiWebTrafficLanguages,
		"devices":   sErr.handleApiWebTrafficDevices,
	}
	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h(w, httptest.NewRequest("GET", "/api/web-traffic/"+name, nil))
			if w.Code < 400 {
				t.Fatalf("%s: want error status, got %d", name, w.Code)
			}
		})
	}
}

func TestHandleApiWebTrafficBrowsers_Populated(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"browser", "version", "cnt"}, []any{"Chrome", "120", 5.0}), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiWebTrafficBrowsers(w, httptest.NewRequest("GET", "/api/web-traffic/browsers", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Chrome 120") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiWebTrafficBrowsers_BlankNameFallsBackToUnknown(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"browser", "version", "cnt"}, []any{"", "", 2.0}), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiWebTrafficBrowsers(w, httptest.NewRequest("GET", "/api/web-traffic/browsers", nil))
	if !strings.Contains(w.Body.String(), "Unknown") {
		t.Fatalf("want Unknown fallback, got: %s", w.Body.String())
	}
}

func TestHandleApiWebTrafficOS_Populated(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"os", "version", "cnt"}, []any{"macOS", "14", 3.0}), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiWebTrafficOS(w, httptest.NewRequest("GET", "/api/web-traffic/os", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "macOS 14") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiWebTrafficTimezones_Populated(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"tz", "cnt"}, []any{"UTC", 4.0}), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiWebTrafficTimezones(w, httptest.NewRequest("GET", "/api/web-traffic/timezones", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "UTC") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiWebTrafficLanguages_Populated(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"lang", "cnt"}, []any{"en-US", 6.0}), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiWebTrafficLanguages(w, httptest.NewRequest("GET", "/api/web-traffic/languages", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "en-US") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiWebTrafficDevices_Populated(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"device", "cnt"}, []any{"mobile", 7.0}), nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiWebTrafficDevices(w, httptest.NewRequest("GET", "/api/web-traffic/devices", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "mobile") {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

// ==================================== handlers_get2.go ========================================

// ---- handleApiEnrichmentGithubRepoHealth ------------------------------------------------------

func TestHandleApiEnrichmentGithubRepoHealth_DbError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_apps") {
			return nil, errors.New("boom")
		}
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiEnrichmentGithubRepoHealth(w, httptest.NewRequest("GET", "/api/enrichment/github/repo-health", nil))
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiEnrichmentGithubRepoHealth_EmptyOk(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiEnrichmentGithubRepoHealth(w, httptest.NewRequest("GET", "/api/enrichment/github/repo-health", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
	}
}

// ---- handleApiReportsExport --------------------------------------------------------------------

func TestHandleApiReportsExport(t *testing.T) {
	cols := []string{"Id", "Name", "Description", "PageType", "FiltersJson"}
	rows := []any{"id-1", "R1", "d", "errors", "{}"}
	rows2 := []any{"id-2", "R2", "d2", "logs", "{}"}

	t.Run("db error -> error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		w := httptest.NewRecorder()
		s.handleApiReportsExport(w, httptest.NewRequest("GET", "/api/reports/export", nil))
		if w.Code < 400 {
			t.Fatalf("want error status, got %d", w.Code)
		}
	})

	t.Run("no ids filter exports all", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(cols, rows, rows2), nil
		}}}
		w := httptest.NewRecorder()
		s.handleApiReportsExport(w, httptest.NewRequest("GET", "/api/reports/export", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "id-1") || !strings.Contains(w.Body.String(), "id-2") {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Disposition"); !strings.Contains(ct, "sobs_reports_export.json") {
			t.Fatalf("unexpected content-disposition: %s", ct)
		}
	})

	t.Run("ids filter restricts to matching rows", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result(cols, rows, rows2), nil
		}}}
		w := httptest.NewRecorder()
		s.handleApiReportsExport(w, httptest.NewRequest("GET", "/api/reports/export?ids=id-1,%20,id-missing", nil))
		if strings.Contains(w.Body.String(), "id-2") {
			t.Fatalf("id-2 should be filtered out: %s", w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "id-1") {
			t.Fatalf("id-1 should remain: %s", w.Body.String())
		}
	})
}

// ---- handleApiAiExport --------------------------------------------------------------------------

func TestHandleApiAiExport(t *testing.T) {
	t.Run("db error", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		w := httptest.NewRecorder()
		s.handleApiAiExport(w, httptest.NewRequest("GET", "/api/ai/export", nil))
		if w.Code < 400 {
			t.Fatalf("want error status, got %d", w.Code)
		}
	})

	t.Run("empty result -> empty jsonl body", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		w := httptest.NewRecorder()
		s.handleApiAiExport(w, httptest.NewRequest("GET", "/api/ai/export?service=svc-a&model=gpt-4o&operation=chat", nil))
		if w.Code != 200 {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
			t.Fatalf("unexpected content type: %s", ct)
		}
	})

	t.Run("format=json switches content-type/filename", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		w := httptest.NewRecorder()
		s.handleApiAiExport(w, httptest.NewRequest("GET", "/api/ai/export?format=json", nil))
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("unexpected content type: %s", ct)
		}
		if !strings.Contains(w.Header().Get("Content-Disposition"), "ai_training_data.json\"") {
			t.Fatalf("unexpected disposition: %s", w.Header().Get("Content-Disposition"))
		}
	})

	t.Run("populated span with parsable messages", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
			return storetest.Result(
				[]string{"Timestamp", "ServiceName", "TraceId", "Duration", "provider_name", "system", "req_model",
					"input_messages", "output_messages", "sobs_prompt", "sobs_response", "tokens_in", "tokens_out"},
				[]any{"2026-01-01 00:00:00", "svc-a", "tr1", 2_000_000.0, "openai", "", "gpt-4o",
					`[{"role":"user","content":"hi"}]`, `[{"role":"assistant","content":"hello"}]`, "", "", 3.0, 5.0},
			), nil
		}}}
		w := httptest.NewRecorder()
		s.handleApiAiExport(w, httptest.NewRequest("GET", "/api/ai/export?limit=10", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "svc-a") {
			t.Fatalf("unexpected response %d: %s", w.Code, w.Body.String())
		}
	})
}

// ---- handleApiOnboardingInspectRepo --------------------------------------------------------------

func TestHandleApiOnboardingInspectRepo_MissingParams(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiOnboardingInspectRepo(w, httptest.NewRequest("GET", "/api/onboarding/inspect-repo", nil))
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiOnboardingInspectRepo_AppNotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiOnboardingInspectRepo(w, httptest.NewRequest("GET", "/api/onboarding/inspect-repo?app_id=missing", nil))
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiOnboardingInspectRepo_AppDbError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	s.handleApiOnboardingInspectRepo(w, httptest.NewRequest("GET", "/api/onboarding/inspect-repo?app_id=a1", nil))
	if w.Code < 400 {
		t.Fatalf("want error status, got %d", w.Code)
	}
}

func TestHandleApiOnboardingInspectRepo_UnparseableRepo(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiOnboardingInspectRepo(w, httptest.NewRequest("GET", "/api/onboarding/inspect-repo?repo=not-a-url", nil))
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Could not parse owner/repo") {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleApiOnboardingInspectRepo_NoToken(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	s.handleApiOnboardingInspectRepo(w, httptest.NewRequest("GET", "/api/onboarding/inspect-repo?repo=acme/widgets", nil))
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "No GitHub token configured") || !strings.Contains(body, `"has_github_actions":false`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandleApiOnboardingInspectRepo_AppIDResolvesRepoURL(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_apps") {
			return storetest.Result([]string{"RepoUrl"}, []any{"https://github.com/acme/app1"}), nil
		}
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	s.handleApiOnboardingInspectRepo(w, httptest.NewRequest("GET", "/api/onboarding/inspect-repo?app_id=a1", nil))
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"owner":"acme"`) || !strings.Contains(w.Body.String(), `"repo":"app1"`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

// ---- handleV1RumAssetByID -----------------------------------------------------------------------

func TestHandleV1RumAssetByID(t *testing.T) {
	t.Run("wrong method -> not GET path", func(t *testing.T) {
		s := &server{cfg: config{DataDir: t.TempDir()}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/rum/assets/"+strings.Repeat("a", 32), nil)
		s.handleV1RumAssetByID(w, r)
		if w.Code == 200 {
			t.Fatalf("POST should not succeed like GET, got %d", w.Code)
		}
	})

	t.Run("invalid asset id -> 400", func(t *testing.T) {
		s := &server{cfg: config{DataDir: t.TempDir()}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/rum/assets/not-hex", nil)
		s.handleV1RumAssetByID(w, r)
		if w.Code != 400 {
			t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("missing asset -> 404", func(t *testing.T) {
		s := &server{cfg: config{DataDir: t.TempDir()}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/rum/assets/"+strings.Repeat("a", 32), nil)
		s.handleV1RumAssetByID(w, r)
		if w.Code != 404 {
			t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("present asset serves file with expected headers", func(t *testing.T) {
		dataDir := t.TempDir()
		assetsDir := filepath.Join(dataDir, "rum_assets")
		if err := os.MkdirAll(assetsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		id := strings.Repeat("b", 32)
		if err := os.WriteFile(filepath.Join(assetsDir, id+".meta.json"),
			[]byte(`{"storage_name":"stored.js","content_type":"application/javascript"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(assetsDir, "stored.js"), []byte("console.log(1)"), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &server{cfg: config{DataDir: dataDir}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/rum/assets/"+id, nil)
		s.handleV1RumAssetByID(w, r)
		if w.Code != 200 {
			t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/javascript; charset=utf-8" {
			t.Fatalf("unexpected content-type: %s", ct)
		}
		if w.Body.String() != "console.log(1)" {
			t.Fatalf("unexpected body: %s", w.Body.String())
		}
	})

	t.Run("malformed metadata json -> 500", func(t *testing.T) {
		dataDir := t.TempDir()
		assetsDir := filepath.Join(dataDir, "rum_assets")
		if err := os.MkdirAll(assetsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		id := strings.Repeat("c", 32)
		if err := os.WriteFile(filepath.Join(assetsDir, id+".meta.json"), []byte(`{not json`), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &server{cfg: config{DataDir: dataDir}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/rum/assets/"+id, nil)
		s.handleV1RumAssetByID(w, r)
		if w.Code != 500 {
			t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("unsafe storage_name -> 500", func(t *testing.T) {
		dataDir := t.TempDir()
		assetsDir := filepath.Join(dataDir, "rum_assets")
		if err := os.MkdirAll(assetsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		id := strings.Repeat("d", 32)
		if err := os.WriteFile(filepath.Join(assetsDir, id+".meta.json"),
			[]byte(`{"storage_name":"../evil.js"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &server{cfg: config{DataDir: dataDir}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/rum/assets/"+id, nil)
		s.handleV1RumAssetByID(w, r)
		if w.Code != 500 {
			t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("storage file missing on disk -> 404", func(t *testing.T) {
		dataDir := t.TempDir()
		assetsDir := filepath.Join(dataDir, "rum_assets")
		if err := os.MkdirAll(assetsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		id := strings.Repeat("e", 32)
		if err := os.WriteFile(filepath.Join(assetsDir, id+".meta.json"),
			[]byte(`{"storage_name":"gone.js"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &server{cfg: config{DataDir: dataDir}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/rum/assets/"+id, nil)
		s.handleV1RumAssetByID(w, r)
		if w.Code != 404 {
			t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("default content type when unset", func(t *testing.T) {
		dataDir := t.TempDir()
		assetsDir := filepath.Join(dataDir, "rum_assets")
		if err := os.MkdirAll(assetsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		id := strings.Repeat("f", 32)
		if err := os.WriteFile(filepath.Join(assetsDir, id+".meta.json"),
			[]byte(`{"storage_name":"blob.bin"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(assetsDir, "blob.bin"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &server{cfg: config{DataDir: dataDir}}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/rum/assets/"+id, nil)
		s.handleV1RumAssetByID(w, r)
		if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
			t.Fatalf("unexpected content-type: %s", ct)
		}
	})
}

// ---- werkzeugContentType ------------------------------------------------------------------------

func TestWerkzeugContentType(t *testing.T) {
	cases := map[string]string{
		"text/html":                "text/html; charset=utf-8",
		"text/plain":               "text/plain; charset=utf-8",
		"application/javascript":   "application/javascript; charset=utf-8",
		"application/xml":          "application/xml; charset=utf-8",
		"image/svg+xml":            "image/svg+xml; charset=utf-8",
		"application/octet-stream": "application/octet-stream",
		"image/png":                "image/png",
		"application/json":         "application/json",
	}
	for in, want := range cases {
		if got := werkzeugContentType(in); got != want {
			t.Errorf("werkzeugContentType(%q) = %q, want %q", in, got, want)
		}
	}
}
