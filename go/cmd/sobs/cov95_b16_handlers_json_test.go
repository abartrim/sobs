package main

import (
	"net/http/httptest"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_handlers_json_test.go — batch 16 targeted coverage for
// cmd/sobs/handlers_json.go's handleApiTableExplorerTables: the query-page-disabled 404 guard,
// the allowedTablesInfo error branch (500), and the enabled+empty-tables success branch.

func TestHandleApiTableExplorerTablesDisabled(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}} // no ai.endpoint_url/model -> queryPageEnabled() false
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/table-explorer/tables", nil)
	s.handleApiTableExplorerTables(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404 when query page disabled, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got == "" {
		t.Fatal("want a body")
	}
}

func TestHandleApiTableExplorerTablesQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		switch q {
		case "SELECT Value FROM sobs_ai_settings FINAL WHERE Key=? AND IsDeleted=0 LIMIT 1":
			return storetest.Result([]string{"Value"}, []any{"enabled-value"}), nil
		default:
			return nil, assertErr("boom")
		}
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/table-explorer/tables", nil)
	s.handleApiTableExplorerTables(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500 on query error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiTableExplorerTablesEnabledEmpty(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		switch q {
		case "SELECT Value FROM sobs_ai_settings FINAL WHERE Key=? AND IsDeleted=0 LIMIT 1":
			return storetest.Result([]string{"Value"}, []any{"enabled-value"}), nil
		case "SELECT name FROM system.tables WHERE database=? ORDER BY name":
			return &store.Result{}, nil // no tables exist -> empty allowlist intersection
		default:
			return &store.Result{}, nil
		}
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/table-explorer/tables", nil)
	s.handleApiTableExplorerTables(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got == "" {
		t.Fatal("want a body")
	}
}

// assertErr (a minimal error constructor for forcing a query failure branch) is defined in
// cov95_b7_ai_helper_context_test.go and reused here.
