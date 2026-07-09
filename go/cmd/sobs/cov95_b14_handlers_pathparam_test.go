package main

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Coverage batch 14: cmd/sobs/handlers_pathparam.go. spanInt64 (28.6%) had no direct unit test —
// only the string-branch (via the DB's typical UInt64-as-string JSON encoding) was reached
// through handleApiRawSpan's populated-row path; the float64/json.Number/default branches were
// never exercised. handleApiTableExplorerTable had no coverage at all (78.6% via inference from
// other codepaths only). handleApiRawSpan's found-span serialization / truncation path was
// likewise untested (the parity corpus only ever hits the 404 branch).

func TestSpanInt64(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{float64(42), 42},
		{json.Number("123"), 123},
		{json.Number("not-a-number"), 0}, // Int64() fails -> falls through to the trailing 0
		{"456", 456},
		{"  789  ", 789},
		{"not-a-number", 0},
		{nil, 0},
		{true, 0}, // unrecognized type -> default 0
	}
	for _, c := range cases {
		if got := spanInt64(c.in); got != c.want {
			t.Errorf("spanInt64(%#v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestHandleApiRawSpan_MissingSpanID(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/traces/span/", nil)
	s.handleApiRawSpan(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiRawSpan_DBError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/traces/span/abc123", nil)
	s.handleApiRawSpan(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiRawSpan_NotFound(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/traces/span/abc123", nil)
	s.handleApiRawSpan(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "span not found") {
		t.Errorf("got %s", w.Body.String())
	}
}

func TestHandleApiRawSpan_TraceIDFilterAppendsCondition(t *testing.T) {
	var capturedSQL string
	var capturedParams []any
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		capturedSQL = q
		capturedParams = p
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/traces/span/abc123?trace_id=trace-xyz", nil)
	s.handleApiRawSpan(w, r)
	if !strings.Contains(capturedSQL, "AND TraceId=?") {
		t.Errorf("expected TraceId filter in SQL, got %q", capturedSQL)
	}
	if len(capturedParams) != 2 || capturedParams[1] != "trace-xyz" {
		t.Errorf("expected trace_id param appended, got %v", capturedParams)
	}
}

func TestHandleApiRawSpan_FoundSpanSerialization(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		return storetest.Result(
			[]string{"Timestamp", "TraceId", "SpanId", "ParentSpanId", "TraceState", "SpanName",
				"SpanKind", "ServiceName", "ScopeName", "ScopeVersion", "Duration", "StatusCode",
				"StatusMessage", "span_attr_keys", "span_attr_values", "res_attr_keys", "res_attr_values"},
			[]any{
				"2026-01-01 00:00:00.000000", "trace-1", "span-1", "", "", "GET /x",
				"SERVER", "svc-a", "otel-scope", "1.0", float64(1_500_000), "STATUS_CODE_OK",
				"", []any{"http.method"}, []any{"GET"}, []any{"service.name"}, []any{"svc-a"},
			},
		), nil
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/traces/span/span-1", nil)
	s.handleApiRawSpan(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"trace_id":"trace-1"`) {
		t.Errorf("expected trace_id in body, got %s", body)
	}
	if !strings.Contains(body, `"duration_ns":1500000`) {
		t.Errorf("expected duration_ns=1500000, got %s", body)
	}
	if !strings.Contains(body, `"duration_ms":1.5`) {
		t.Errorf("expected duration_ms=1.5, got %s", body)
	}
	if !strings.Contains(body, `"truncated":false`) {
		t.Errorf("expected truncated=false for a small span, got %s", body)
	}
	if !strings.Contains(body, `"http.method":"GET"`) {
		t.Errorf("expected span attributes rendered, got %s", body)
	}
}

func TestHandleApiRawSpan_TruncatesOverlongAttributes(t *testing.T) {
	// rawSpanMaxBytes is 32KB on the pretty-printed span; a single attribute value must exceed
	// that (with headroom for JSON escaping/quoting/indentation) to trip the truncation branch.
	longVal := strings.Repeat("y", 40000)
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		return storetest.Result(
			[]string{"Timestamp", "TraceId", "SpanId", "ParentSpanId", "TraceState", "SpanName",
				"SpanKind", "ServiceName", "ScopeName", "ScopeVersion", "Duration", "StatusCode",
				"StatusMessage", "span_attr_keys", "span_attr_values", "res_attr_keys", "res_attr_values"},
			[]any{
				"2026-01-01 00:00:00.000000", "trace-2", "span-2", "", "", "GET /y",
				"SERVER", "svc-b", "otel-scope", "1.0", float64(1000), "STATUS_CODE_OK",
				"", []any{"big.attr"}, []any{longVal}, []any{}, []any{},
			},
		), nil
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/traces/span/span-2", nil)
	s.handleApiRawSpan(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"truncated":true`) {
		t.Errorf("expected truncated=true for an overlong raw payload, got a body of len %d", len(body))
	}
}

func TestHandleApiTableExplorerTable_QueryPageDisabled(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}} // no ai.endpoint_url/ai.model settings -> queryPageEnabled() false
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/table-explorer/table/otel_logs", nil)
	s.handleApiTableExplorerTable(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404 when query page disabled, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Table Explorer is unavailable") {
		t.Errorf("got %s", w.Body.String())
	}
}

func aiEnabledSettingsDB(extra map[string]string, execFn func(q string, p ...any) (*store.Result, error)) *storetest.FakeDB {
	settings := map[string]string{"ai.endpoint_url": "https://llm.example", "ai.model": "gpt-4o"}
	for k, v := range extra {
		settings[k] = v
	}
	return &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_ai_settings") {
			if len(p) > 0 {
				if key, ok := p[0].(string); ok {
					if v, present := settings[key]; present {
						return storetest.Result([]string{"Value"}, []any{v}), nil
					}
				}
			}
			return &store.Result{}, nil
		}
		if execFn != nil {
			return execFn(q, p...)
		}
		return &store.Result{}, nil
	}}
}

func TestHandleApiTableExplorerTable_NotAllowlisted(t *testing.T) {
	s := &server{db: aiEnabledSettingsDB(nil, nil)}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/table-explorer/table/not_a_real_table", nil)
	s.handleApiTableExplorerTable(w, r)
	if w.Code != 403 {
		t.Fatalf("want 403 for a non-allowlisted table, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiTableExplorerTable_DescribeError(t *testing.T) {
	s := &server{db: aiEnabledSettingsDB(nil, func(q string, p ...any) (*store.Result, error) {
		if strings.Contains(q, "system.columns") {
			return nil, errors.New("describe boom")
		}
		return &store.Result{}, nil
	})}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/table-explorer/table/otel_logs", nil)
	s.handleApiTableExplorerTable(w, r)
	if w.Code != 500 {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "describe boom") {
		t.Errorf("got %s", w.Body.String())
	}
}

func TestHandleApiTableExplorerTable_Success(t *testing.T) {
	s := &server{db: aiEnabledSettingsDB(nil, func(q string, p ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "system.columns"):
			return storetest.Result([]string{"name", "type", "default_kind", "comment",
				"is_in_primary_key", "is_in_sorting_key", "is_in_partition_key"},
				[]any{"Timestamp", "DateTime64(9)", "", "", float64(0), float64(1), float64(0)}), nil
		case strings.Contains(q, "SHOW CREATE TABLE"):
			return storetest.Result([]string{"statement"}, []any{"CREATE TABLE otel_logs (...)"}), nil
		case strings.Contains(q, "SELECT * FROM"):
			return storetest.Result([]string{"Timestamp"}, []any{"2026-01-01 00:00:00"}), nil
		}
		return &store.Result{}, nil
	})}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/table-explorer/table/otel_logs", nil)
	s.handleApiTableExplorerTable(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"table":"otel_logs"`) {
		t.Errorf("expected table name echoed, got %s", body)
	}
	if !strings.Contains(body, "CREATE TABLE otel_logs") {
		t.Errorf("expected ddl populated, got %s", body)
	}
}
