package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_handlers_health_test.go — batch 16 targeted coverage for
// cmd/sobs/handlers.go's handleHealthDB: the nil-db degraded branch, the query-error degraded
// branch, and the healthy-ok branch (all three status/body shapes).

func TestHandleHealthDBNilDB(t *testing.T) {
	s := &server{db: nil, wq: (*writeQueue)(nil)}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health/db", nil)
	s.handleHealthDB(w, r)
	if w.Code != 503 {
		t.Fatalf("want 503 for nil db, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["status"] != "degraded" || body["db"] != "error" {
		t.Errorf("unexpected body: %v", body)
	}
}

func TestHandleHealthDBQueryError(t *testing.T) {
	s := &server{
		db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return nil, assertErr("db down")
		}},
		wq: (*writeQueue)(nil),
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health/db", nil)
	s.handleHealthDB(w, r)
	if w.Code != 503 {
		t.Fatalf("want 503 on query error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleHealthDBOK(t *testing.T) {
	s := &server{
		db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"1"}, []any{int64(1)}), nil
		}},
		wq: (*writeQueue)(nil),
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/health/db", nil)
	s.handleHealthDB(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["status"] != "ok" || body["db"] != "ok" {
		t.Errorf("unexpected body: %v", body)
	}
	if _, ok := body["write_queue_depth"]; !ok {
		t.Errorf("missing write_queue_depth in body: %v", body)
	}
}
