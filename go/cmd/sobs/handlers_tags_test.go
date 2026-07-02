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

// handleApiGetTags is a three-way sub-router keyed on method + path arity (GET list, POST add,
// DELETE one key). The byte-parity corpus only ever hits the GET/empty-fixture shape; the POST,
// DELETE, validation, and error branches are corpus-unreachable.
// Oracle: app.py api_get_tags / api_add_tag / api_delete_tag.

func TestHandleApiGetTags_List(t *testing.T) {
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if len(params) != 2 || params[0] != "log" || params[1] != "42" {
			t.Fatalf("unexpected params: %v", params)
		}
		return storetest.Result([]string{"TagKey", "TagValue", "IsAuto"},
			[]any{"env", "prod", 0.0},
			[]any{"owner", "team-a", 1.0},
		), nil
	}}
	s := &server{db: fake}
	rec := httptest.NewRecorder()
	s.handleApiGetTags(rec, httptest.NewRequest(http.MethodGet, "/api/tags/log/42", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"key":"env"`) || !strings.Contains(body, `"is_auto":true`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestHandleApiGetTags_ListQueryError(t *testing.T) {
	fake := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}
	rec := httptest.NewRecorder()
	(&server{db: fake}).handleApiGetTags(rec, httptest.NewRequest(http.MethodGet, "/api/tags/log/42", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

func TestHandleApiGetTags_ListMissingRecordID(t *testing.T) {
	rec := httptest.NewRecorder()
	(&server{db: &storetest.FakeDB{}}).handleApiGetTags(rec, httptest.NewRequest(http.MethodGet, "/api/tags/log", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestHandleApiGetTags_Post(t *testing.T) {
	var inserted []map[string]any
	fake := &storetest.FakeDB{}
	fake.ExecuteFunc = func(string, ...any) (*store.Result, error) { return &store.Result{}, nil }
	s := &server{db: fake}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tags/log/42", strings.NewReader(`{"key":"env","value":"prod"}`))
	s.handleApiGetTags(rec, req)
	inserted = fake.Inserts[0].Rows
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(inserted) != 1 || inserted[0]["TagKey"] != "env" || inserted[0]["TagValue"] != "prod" {
		t.Fatalf("unexpected insert: %v", inserted)
	}
}

func TestHandleApiGetTags_PostMissingKey(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tags/log/42", strings.NewReader(`{"value":"prod"}`))
	(&server{db: &storetest.FakeDB{}}).handleApiGetTags(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestHandleApiGetTags_PostTooLong(t *testing.T) {
	rec := httptest.NewRecorder()
	longKey := strings.Repeat("k", 129)
	req := httptest.NewRequest(http.MethodPost, "/api/tags/log/42", strings.NewReader(`{"key":"`+longKey+`","value":"v"}`))
	(&server{db: &storetest.FakeDB{}}).handleApiGetTags(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestHandleApiGetTags_PostInsertError(t *testing.T) {
	fake := &storetest.FakeDB{InsertErr: errors.New("boom")}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tags/log/42", strings.NewReader(`{"key":"env","value":"prod"}`))
	(&server{db: fake}).handleApiGetTags(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleApiGetTags_Delete(t *testing.T) {
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "SELECT TagKey") {
			return storetest.Result([]string{"TagKey", "TagValue", "IsAuto"},
				[]any{"owner", "team-a", 0.0},
				[]any{"owner", "team-a", 0.0}, // duplicate (value, is_auto) -> deduped to one tombstone
				[]any{"owner", "team-b", 0.0},
			), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/tags/log/42/owner", nil)
	s.handleApiGetTags(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(fake.Inserts) != 1 || len(fake.Inserts[0].Rows) != 2 {
		t.Fatalf("want 2 deduped tombstones, got %v", fake.Inserts)
	}
}

func TestHandleApiGetTags_DeleteNotFound(t *testing.T) {
	fake := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) { return &store.Result{}, nil }}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/tags/log/42/owner", nil)
	(&server{db: fake}).handleApiGetTags(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

func TestHandleApiGetTags_DeleteInsertError(t *testing.T) {
	fake := &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) {
			return storetest.Result([]string{"TagKey", "TagValue", "IsAuto"}, []any{"owner", "team-a", 0.0}), nil
		},
		InsertErr: errors.New("boom"),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/tags/log/42/owner", nil)
	(&server{db: fake}).handleApiGetTags(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleApiGetTags_WrongMethod405(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/tags/log/42", nil)
	(&server{db: &storetest.FakeDB{}}).handleApiGetTags(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}
