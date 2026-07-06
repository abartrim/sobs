package main

import (
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_handlers_pages_errors_items_test.go — batch 16 targeted coverage for
// cmd/sobs/handlers_pages_errors_items.go: buildErrorStubFromNarrow's ErrorType-default and
// ErrorId-derivation branches (both the pre-set-id and the computed-id path), a JSON-body summary
// case, and getResolvedErrorIDs' query-error / populated-rows branches.

func TestBuildErrorStubFromNarrowDefaults(t *testing.T) {
	row := map[string]any{
		"Timestamp":    "2024-01-01T00:00:00Z",
		"ServiceName":  "svc-a",
		"TraceId":      "trace-1",
		"SpanId":       "span-1",
		"ErrorMessage": "boom",
		// ErrorType and Body and ErrorId intentionally absent.
	}
	got := buildErrorStubFromNarrow(row, false)
	if got["err_type"] != "Error" {
		t.Errorf("want default err_type Error, got %v", got["err_type"])
	}
	wantID := errorIDFor("2024-01-01T00:00:00Z", "svc-a", "Error", "boom", "trace-1", "span-1")
	if got["id"] != wantID {
		t.Errorf("id = %v, want computed %v", got["id"], wantID)
	}
	if got["resolved"] != false {
		t.Errorf("resolved = %v, want false", got["resolved"])
	}
	if got["service"] != "svc-a" || got["message"] != "boom" {
		t.Errorf("unexpected row fields: %v", got)
	}
}

func TestBuildErrorStubFromNarrowExplicitID(t *testing.T) {
	row := map[string]any{
		"ErrorId":      "preset-id-123",
		"ErrorType":    "TimeoutError",
		"ErrorMessage": "took too long",
	}
	got := buildErrorStubFromNarrow(row, true)
	if got["id"] != "preset-id-123" {
		t.Errorf("id = %v, want preset-id-123 (should not recompute when ErrorId is set)", got["id"])
	}
	if got["err_type"] != "TimeoutError" {
		t.Errorf("err_type = %v, want TimeoutError", got["err_type"])
	}
	if got["resolved"] != true {
		t.Errorf("resolved = %v, want true", got["resolved"])
	}
}

func TestBuildErrorStubFromNarrowJSONMessageSummary(t *testing.T) {
	row := map[string]any{
		"ErrorMessage": `{"error": "bad request", "code": 400}`,
	}
	got := buildErrorStubFromNarrow(row, false)
	if got["summary_from_json"] != true {
		t.Errorf("want summary_from_json=true for a JSON message body, got %v", got["summary_from_json"])
	}
	if got["message_summary"] == "" {
		t.Errorf("want a non-empty message_summary")
	}
}

func TestGetResolvedErrorIDsQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, assertErr("boom")
	}}}
	got := s.getResolvedErrorIDs()
	if len(got) != 0 {
		t.Errorf("want empty map on query error, got %v", got)
	}
}

func TestGetResolvedErrorIDsPopulated(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return storetest.Result([]string{"ErrorId"}, []any{"err-1"}, []any{"err-2"}), nil
	}}}
	got := s.getResolvedErrorIDs()
	if !got["err-1"] || !got["err-2"] || len(got) != 2 {
		t.Errorf("got %v, want both err-1 and err-2 present", got)
	}
}
