package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_regex_validate_test.go — batch 16 targeted coverage for cmd/sobs/regex_validate.go:
// handleValidateRegex's empty-pattern / invalid-expression / probe-error / matched-sample
// branches, regexBestEffortSample's query-error and no-match branches, truncateRegexSample's
// truncation + non-string-passthrough branches, and regexScopeText's absent/blank/over-length
// branches.

func TestHandleValidateRegexEmptyPattern(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/logs/validate-regex", strings.NewReader(`{"pattern":""}`))
	s.handleValidateRegex(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), `"sample":null`) {
		t.Errorf("unexpected body: %s", w.Body.String())
	}
}

func TestHandleValidateRegexInvalidExpression(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/logs/validate-regex", strings.NewReader(`{"pattern":"(["}`))
	s.handleValidateRegex(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200 (errors are reported in-body), got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Errorf("want ok:false in body, got %s", w.Body.String())
	}
}

func TestHandleValidateRegexProbeErrorDegradesToNilSample(t *testing.T) {
	// Let the RE2 validation query ("SELECT match('', ?)") succeed so the expression is accepted,
	// but fail the actual sample-probe SELECT — that's the branch that degrades to {ok:true,
	// sample:null} rather than surfacing the error.
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.HasPrefix(q, "SELECT match(") {
			return &store.Result{}, nil
		}
		return nil, assertErr("boom")
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/logs/validate-regex", strings.NewReader(`{"pattern":"boom"}`))
	s.handleValidateRegex(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) || !strings.Contains(w.Body.String(), `"sample":null`) {
		t.Errorf("want ok:true, sample:null on probe error, got %s", w.Body.String())
	}
}

func TestHandleValidateRegexMatchedSample(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return storetest.Result([]string{"sample_value"}, []any{"a matching boom sample"}), nil
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/traces/validate-regex", strings.NewReader(
		`{"pattern":"boom","scope":{"service":"svc-a","trace_id":"t1"}}`))
	s.handleValidateRegex(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "a matching boom sample") {
		t.Errorf("want the sample echoed back, got %s", w.Body.String())
	}
}

func TestHandleValidateRegexScopeMissingDefaultsToEmptyMap(t *testing.T) {
	// scope absent from the body -> m["scope"] type-asserts to nil, and the handler defaults it to
	// an empty map rather than panicking on a nil map read.
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/rum/validate-regex", strings.NewReader(`{"pattern":"x"}`))
	s.handleValidateRegex(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRegexBestEffortSampleQueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, assertErr("db down")
	}}}
	_, err := s.regexBestEffortSample("otel_logs", "Body", "Timestamp", nil, nil, nil, nil)
	if err == nil {
		t.Fatal("want the query error propagated")
	}
}

func TestRegexBestEffortSampleNoRows(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	got, err := s.regexBestEffortSample("otel_logs", "Body", "Timestamp", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("want nil sample for no rows, got %v", got)
	}
}

func TestTruncateRegexSampleVariants(t *testing.T) {
	t.Run("nil passes through", func(t *testing.T) {
		if got := truncateRegexSample(nil); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("non-string type returns nil", func(t *testing.T) {
		if got := truncateRegexSample(42); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
	t.Run("short string unchanged", func(t *testing.T) {
		if got := truncateRegexSample("short"); got != "short" {
			t.Errorf("got %v, want short", got)
		}
	})
	t.Run("over-long string truncated with ellipsis", func(t *testing.T) {
		long := strings.Repeat("x", regexSampleMaxLen+50)
		got, ok := truncateRegexSample(long).(string)
		if !ok {
			t.Fatalf("want string result")
		}
		if len([]rune(got)) != regexSampleMaxLen {
			t.Errorf("truncated length = %d, want %d", len([]rune(got)), regexSampleMaxLen)
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("want ellipsis suffix, got %q", got)
		}
	})
}

func TestRegexScopeTextVariants(t *testing.T) {
	t.Run("absent key returns empty", func(t *testing.T) {
		if got := regexScopeText(map[string]any{}, "service", 200); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("blank value returns empty", func(t *testing.T) {
		if got := regexScopeText(map[string]any{"service": "   "}, "service", 200); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("non-string value returns empty", func(t *testing.T) {
		if got := regexScopeText(map[string]any{"service": 5}, "service", 200); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("value truncated to maxLen", func(t *testing.T) {
		if got := regexScopeText(map[string]any{"service": "abcdef"}, "service", 3); got != "abc" {
			t.Errorf("got %q, want abc", got)
		}
	})
	t.Run("value within bounds trimmed", func(t *testing.T) {
		if got := regexScopeText(map[string]any{"service": "  svc  "}, "service", 200); got != "svc" {
			t.Errorf("got %q, want svc", got)
		}
	})
}
