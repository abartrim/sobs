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

// Coverage batch 14: cmd/sobs/dm_prune.go — no dedicated test file existed yet. Covers
// requestIsJSON, parseDmPrunePeriod, dmColumnType, runDmPrune, and handleApiDataManagementPrune's
// branches (bad JSON, non-object payload, invalid period, lock contention, DB errors).

func TestRequestIsJSON(t *testing.T) {
	cases := []struct {
		ct   string
		want bool
	}{
		{"application/json", true},
		{"application/json; charset=utf-8", true},
		{"application/vnd.api+json", true},
		{"APPLICATION/JSON", true},
		{"  application/json  ", true}, // requestIsJSON trims the (';'-split) mimetype component
		{"text/plain", false},
		{"", false},
		{"application/xml", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/x", nil)
		if c.ct != "" {
			r.Header.Set("Content-Type", c.ct)
		}
		if got := requestIsJSON(r); got != c.want {
			t.Errorf("Content-Type %q: got %v, want %v", c.ct, got, c.want)
		}
	}
}

func TestParseDmPrunePeriod(t *testing.T) {
	obj := func(kv map[string]any) *jsonenc.Object {
		o := jsonenc.NewObject()
		for k, v := range kv {
			o.Set(k, v)
		}
		return o
	}

	t.Run("no_period_fields_ok", func(t *testing.T) {
		p, errMsg := parseDmPrunePeriod(jsonenc.NewObject())
		if errMsg != "" || p.present {
			t.Fatalf("got (%v,%q)", p, errMsg)
		}
	})

	t.Run("value_without_unit_errors", func(t *testing.T) {
		_, errMsg := parseDmPrunePeriod(obj(map[string]any{"prune_period_value": "5"}))
		if !strings.Contains(errMsg, "prune_period_unit is required") {
			t.Errorf("got %q", errMsg)
		}
	})

	t.Run("unit_without_value_errors", func(t *testing.T) {
		_, errMsg := parseDmPrunePeriod(obj(map[string]any{"prune_period_unit": "hours"}))
		if !strings.Contains(errMsg, "prune_period_value is required") {
			t.Errorf("got %q", errMsg)
		}
	})

	t.Run("invalid_unit_errors", func(t *testing.T) {
		_, errMsg := parseDmPrunePeriod(obj(map[string]any{"prune_period_value": "5", "prune_period_unit": "weeks"}))
		if !strings.Contains(errMsg, "must be 'hours' or 'days'") {
			t.Errorf("got %q", errMsg)
		}
	})

	t.Run("non_numeric_value_errors", func(t *testing.T) {
		_, errMsg := parseDmPrunePeriod(obj(map[string]any{"prune_period_value": "abc", "prune_period_unit": "days"}))
		if !strings.Contains(errMsg, "must be a positive integer") {
			t.Errorf("got %q", errMsg)
		}
	})

	t.Run("zero_or_negative_value_errors", func(t *testing.T) {
		_, errMsg := parseDmPrunePeriod(obj(map[string]any{"prune_period_value": "0", "prune_period_unit": "days"}))
		if !strings.Contains(errMsg, "must be a positive integer") {
			t.Errorf("got %q", errMsg)
		}
		_, errMsg2 := parseDmPrunePeriod(obj(map[string]any{"prune_period_value": "-1", "prune_period_unit": "days"}))
		if !strings.Contains(errMsg2, "must be a positive integer") {
			t.Errorf("got %q", errMsg2)
		}
	})

	t.Run("valid_hours", func(t *testing.T) {
		p, errMsg := parseDmPrunePeriod(obj(map[string]any{"prune_period_value": "12", "prune_period_unit": "HOURS"}))
		if errMsg != "" {
			t.Fatalf("unexpected error: %q", errMsg)
		}
		if !p.present || p.value != 12 || p.unit != "hours" {
			t.Errorf("got %+v", p)
		}
	})

	t.Run("valid_days_numeric_value_type", func(t *testing.T) {
		// raw_value can arrive as a JSON number, not just a string.
		p, errMsg := parseDmPrunePeriod(obj(map[string]any{"prune_period_value": 30, "prune_period_unit": "days"}))
		if errMsg != "" {
			t.Fatalf("unexpected error: %q", errMsg)
		}
		if !p.present || p.value != 30 || p.unit != "days" {
			t.Errorf("got %+v", p)
		}
	})

	t.Run("null_unit_present_stringifies_to_None", func(t *testing.T) {
		// prune_period_unit explicitly null (present) -> Python str(None) == "None" -> invalid unit.
		_, errMsg := parseDmPrunePeriod(obj(map[string]any{"prune_period_value": "5", "prune_period_unit": nil}))
		if !strings.Contains(errMsg, "must be 'hours' or 'days'") {
			t.Errorf("got %q", errMsg)
		}
	})

	t.Run("empty_string_value_with_unit_errors", func(t *testing.T) {
		_, errMsg := parseDmPrunePeriod(obj(map[string]any{"prune_period_value": "", "prune_period_unit": "days"}))
		if !strings.Contains(errMsg, "prune_period_value is required") {
			t.Errorf("got %q", errMsg)
		}
	})
}

func TestDmColumnType(t *testing.T) {
	t.Run("db_error_yields_false", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return nil, errors.New("boom")
		}}}
		typ, ok := s.dmColumnType("otel_metrics_gauge", "TimeUnixMs")
		if ok || typ != "" {
			t.Errorf("got (%q,%v)", typ, ok)
		}
	})

	t.Run("column_found_lowercased", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return storetest.Result([]string{"name", "type"},
				[]any{"TimeUnixMs", "  DateTime  "}), nil
		}}}
		typ, ok := s.dmColumnType("otel_metrics_gauge", "TimeUnixMs")
		if !ok || typ != "datetime" {
			t.Errorf("got (%q,%v)", typ, ok)
		}
	})

	t.Run("column_absent", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return storetest.Result([]string{"name", "type"}, []any{"OtherCol", "String"}), nil
		}}}
		typ, ok := s.dmColumnType("otel_metrics_gauge", "TimeUnixMs")
		if ok || typ != "" {
			t.Errorf("got (%q,%v)", typ, ok)
		}
	})
}

func TestRunDmPrune_OptimizeOnlySuccess(t *testing.T) {
	var executed []string
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		executed = append(executed, q)
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmPrune(dmPrunePeriod{})
	if !ok {
		t.Fatalf("expected success, got %q", msg)
	}
	if !strings.Contains(msg, "6 tables processed") {
		t.Errorf("got %q", msg)
	}
	if strings.Contains(msg, "custom period") {
		t.Errorf("optimize-only run should not mention a custom period, got %q", msg)
	}
	for _, table := range dmAllPruneTables() {
		found := false
		for _, q := range executed {
			if strings.Contains(q, "OPTIMIZE TABLE "+table+" FINAL") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected OPTIMIZE TABLE %s FINAL to run", table)
		}
	}
}

func TestRunDmPrune_OptimizeError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if strings.Contains(q, "OPTIMIZE TABLE otel_logs") {
			return nil, errors.New("optimize boom")
		}
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmPrune(dmPrunePeriod{})
	if ok {
		t.Fatal("expected failure")
	}
	if !strings.Contains(msg, "Prune completed with errors") || !strings.Contains(msg, "optimize boom") {
		t.Errorf("got %q", msg)
	}
}

func TestRunDmPrune_CustomPeriod_TTLDeleteSuccess(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmPrune(dmPrunePeriod{present: true, value: 7, unit: "days"})
	if !ok {
		t.Fatalf("expected success, got %q", msg)
	}
	if !strings.Contains(msg, "custom period: 7 days") {
		t.Errorf("got %q", msg)
	}
}

func TestRunDmPrune_CustomPeriod_TTLDeleteError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if strings.Contains(q, "ALTER TABLE otel_logs DELETE") {
			return nil, errors.New("delete boom")
		}
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmPrune(dmPrunePeriod{present: true, value: 1, unit: "hours"})
	if ok {
		t.Fatal("expected failure")
	}
	if !strings.Contains(msg, "otel_logs: delete boom") {
		t.Errorf("got %q", msg)
	}
}

// TestRunDmPrune_MetricTables_DetectedDateTime_PrimarySucceeds covers the useMsExpr=false branch:
// dmColumnType reports the column IS a DateTime (already converted), so the plain (non-ms) DELETE
// expression is tried first and, on success, no fallback runs.
func TestRunDmPrune_MetricTables_DetectedDateTime_PrimarySucceeds(t *testing.T) {
	var plainRan, msRan bool
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if strings.Contains(q, "DESCRIBE TABLE") {
			return storetest.Result([]string{"name", "type"}, []any{"TimeUnixMs", "DateTime"}), nil
		}
		if strings.Contains(q, "ALTER TABLE otel_metrics_gauge DELETE WHERE TimeUnixMs <") {
			plainRan = true
			return &store.Result{}, nil
		}
		if strings.Contains(q, "ALTER TABLE otel_metrics_gauge DELETE WHERE toDateTime") {
			msRan = true
			return &store.Result{}, nil
		}
		return &store.Result{}, nil
	}}}
	ok, _ := s.runDmPrune(dmPrunePeriod{present: true, value: 2, unit: "days"})
	if !ok {
		t.Fatal("expected success")
	}
	if !plainRan {
		t.Error("expected the plain (DateTime-typed) DELETE expression to run first")
	}
	if msRan {
		t.Error("fallback ms-expr should not run when the primary succeeds")
	}
}

// TestRunDmPrune_MetricTables_PrimaryFailsFallbackSucceeds covers the fallback-succeeds branch.
func TestRunDmPrune_MetricTables_PrimaryFailsFallbackSucceeds(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if strings.Contains(q, "DESCRIBE TABLE") {
			return &store.Result{}, nil // dmColumnType fails -> ok=false -> useMsExpr=true -> ms primary
		}
		if strings.Contains(q, "ALTER TABLE otel_metrics_gauge DELETE WHERE toDateTime") {
			return nil, errors.New("ms primary boom")
		}
		if strings.Contains(q, "ALTER TABLE otel_metrics_gauge DELETE WHERE TimeUnixMs <") {
			return &store.Result{}, nil // plain fallback succeeds
		}
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmPrune(dmPrunePeriod{present: true, value: 3, unit: "hours"})
	if !ok {
		t.Fatalf("expected overall success (fallback succeeded), got %q", msg)
	}
}

// TestRunDmPrune_MetricTables_BothFail covers the "fallback after" error-aggregation message.
func TestRunDmPrune_MetricTables_BothFail(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if strings.Contains(q, "DESCRIBE TABLE") {
			return &store.Result{}, nil
		}
		if strings.Contains(q, "ALTER TABLE otel_metrics_gauge DELETE") {
			return nil, errors.New("both fail")
		}
		return &store.Result{}, nil
	}}}
	ok, msg := s.runDmPrune(dmPrunePeriod{present: true, value: 3, unit: "hours"})
	if ok {
		t.Fatal("expected failure")
	}
	if !strings.Contains(msg, "otel_metrics_gauge:") || !strings.Contains(msg, "fallback after:") {
		t.Errorf("got %q", msg)
	}
}

// --- handleApiDataManagementPrune (HTTP-level) ---

func TestHandleApiDataManagementPrune_MalformedJSON(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/data-management/prune", strings.NewReader(`{bad`))
	r.Header.Set("Content-Type", "application/json")
	s.handleApiDataManagementPrune(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid JSON") {
		t.Errorf("got %s", w.Body.String())
	}
}

func TestHandleApiDataManagementPrune_NonObjectPayload(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/data-management/prune", strings.NewReader(`[1,2,3]`))
	r.Header.Set("Content-Type", "application/json")
	s.handleApiDataManagementPrune(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "must be a JSON object") {
		t.Errorf("got %s", w.Body.String())
	}
}

func TestHandleApiDataManagementPrune_InvalidPeriod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/data-management/prune",
		strings.NewReader(`{"prune_period_value":"5"}`))
	r.Header.Set("Content-Type", "application/json")
	s.handleApiDataManagementPrune(w, r)
	if w.Code != 400 {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "prune_period_unit is required") {
		t.Errorf("got %s", w.Body.String())
	}
}

func TestHandleApiDataManagementPrune_NoBodyNoContentType(t *testing.T) {
	// silent=True + non-JSON content-type -> payload defaults to {} -> optimize-only success.
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/data-management/prune", nil)
	s.handleApiDataManagementPrune(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("got %s", w.Body.String())
	}
}

func TestHandleApiDataManagementPrune_LockContention(t *testing.T) {
	dmPruneMu.Lock()
	defer dmPruneMu.Unlock()
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/data-management/prune", nil)
	s.handleApiDataManagementPrune(w, r)
	if w.Code != 409 {
		t.Fatalf("want 409 when the prune lock is held, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "already in progress") {
		t.Errorf("got %s", w.Body.String())
	}
}

func TestHandleApiDataManagementPrune_SuccessWithCustomPeriod(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/data-management/prune",
		strings.NewReader(`{"prune_period_value":"1","prune_period_unit":"days"}`))
	r.Header.Set("Content-Type", "application/json")
	s.handleApiDataManagementPrune(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "custom period: 1 days") {
		t.Errorf("got %s", w.Body.String())
	}
}
