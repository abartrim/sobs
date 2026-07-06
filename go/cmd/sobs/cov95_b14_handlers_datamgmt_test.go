package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Coverage batch 14: cmd/sobs/handlers_datamgmt.go's fmtBytes (53.8%) had no direct unit test
// (only reached indirectly, and only on nil, via buildDBStats' unpopulated-parity path), and
// applyDMTTL (72%) was missing its metrics-hours non-numeric/non-positive branch and its
// per-table SQL-execution-error branch.

func TestFmtBytes(t *testing.T) {
	cases := []struct {
		in   []any
		want string
	}{
		{nil, "—"},
		{[]any{nil}, "—"},
		{[]any{float64(500)}, "500 B"},
		{[]any{float64(2048)}, "2.0 KB"},
		{[]any{float64(3 * 1024 * 1024)}, "3.0 MB"},
		{[]any{float64(5 * 1024 * 1024 * 1024)}, "5.0 GB"},
		{[]any{int(1500)}, "1.5 KB"},
		{[]any{int64(1500)}, "1.5 KB"},
		{[]any{"not-a-number"}, "—"}, // unsupported type falls to the default branch
	}
	for _, c := range cases {
		if got := fmtBytes(c.in); got != c.want {
			t.Errorf("fmtBytes(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildDBStats(t *testing.T) {
	t.Run("no_rows_leaves_nils", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			return &store.Result{}, nil
		}}}
		stats := s.buildDBStats()
		if v, _ := stats.Get("compressed_bytes"); v != nil {
			t.Errorf("expected nil compressed_bytes, got %v", v)
		}
		if v, _ := stats.Get("active_queries"); v != 0 {
			t.Errorf("expected 0 active_queries from an empty processes count, got %v", v)
		}
	})

	t.Run("overall_error_skips_but_tables_still_populate", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "GROUP BY table"):
				return storetest.Result([]string{"table", "comp", "uncomp", "rws"},
					[]any{"otel_logs", float64(100), float64(400), float64(10)}), nil
			case strings.Contains(q, "sum(data_compressed_bytes) AS comp"):
				return nil, errors.New("overall boom")
			case strings.Contains(q, "system.processes"):
				return storetest.Result([]string{"c"}, []any{float64(2)}), nil
			}
			return &store.Result{}, nil
		}}}
		stats := s.buildDBStats()
		if v, _ := stats.Get("compressed_bytes"); v != nil {
			t.Errorf("expected nil after overall query error, got %v", v)
		}
		tables, _ := stats.Get("tables")
		arr, _ := tables.([]any)
		if len(arr) != 1 {
			t.Fatalf("expected 1 table row despite overall error, got %v", tables)
		}
		if v, _ := stats.Get("active_queries"); v != 2 {
			t.Errorf("active_queries = %v, want 2", v)
		}
	})

	t.Run("compression_ratio_present_and_zero_branch", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "GROUP BY table"):
				return storetest.Result([]string{"table", "comp", "uncomp", "rws"},
					[]any{"otel_logs", float64(0), float64(0), float64(0)},
					[]any{"otel_traces", float64(200), float64(800), float64(5)},
				), nil
			case strings.Contains(q, "sum(data_compressed_bytes) AS comp"):
				return storetest.Result([]string{"comp", "uncomp", "rws"},
					[]any{float64(200), float64(800), float64(5)}), nil
			}
			return &store.Result{}, nil
		}}}
		stats := s.buildDBStats()
		if v, _ := stats.Get("compression_ratio"); v != 4.0 {
			t.Errorf("overall compression_ratio = %v, want 4.0", v)
		}
		tables, _ := stats.Get("tables")
		arr, _ := tables.([]any)
		if len(arr) != 2 {
			t.Fatalf("expected 2 table rows, got %v", tables)
		}
	})

	t.Run("tables_query_error_leaves_empty_tables", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
			if strings.Contains(q, "GROUP BY table") {
				return nil, errors.New("tables boom")
			}
			return &store.Result{}, nil
		}}}
		stats := s.buildDBStats()
		tables, _ := stats.Get("tables")
		arr, _ := tables.([]any)
		if len(arr) != 0 {
			t.Errorf("expected empty tables on query error, got %v", tables)
		}
	})
}

func TestRound2(t *testing.T) {
	if got := round2(2.0); got != 2.0 {
		t.Errorf("round2(2.0) = %v, want 2.0", got)
	}
	if got := round2(1.0 / 3.0); got != 0.33 {
		t.Errorf("round2(1/3) = %v, want 0.33", got)
	}
}

// applyDMTTL's day-table branches (positive/pyParseInt-error) are already exercised via
// handleSettingsDataManagement's ApplyTTLSuccess/ApplyTTLErrors tests; here we cover the two
// branches those miss: the metrics-hours error path (non-numeric AND non-positive both fold into
// the SAME message, unlike the day branch) and a genuine per-table SQL execution failure.

func TestApplyDMTTL_MetricsHoursNonNumeric(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	errs := s.applyDMTTL(map[string]string{"data_management.ttl_metrics_hours": "not-a-number"})
	if len(errs) != 1 || errs[0] != "metrics: TTL hours must be a positive integer" {
		t.Fatalf("got %v", errs)
	}
}

func TestApplyDMTTL_MetricsHoursNonPositive(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	errs := s.applyDMTTL(map[string]string{"data_management.ttl_metrics_hours": "0"})
	if len(errs) != 1 || errs[0] != "metrics: TTL hours must be a positive integer" {
		t.Fatalf("got %v", errs)
	}
}

func TestApplyDMTTL_MetricsHoursSQLError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if strings.Contains(q, "otel_metrics_gauge") {
			return nil, errors.New("alter boom")
		}
		return &store.Result{}, nil
	}}}
	errs := s.applyDMTTL(map[string]string{"data_management.ttl_metrics_hours": "48"})
	found := false
	for _, e := range errs {
		if strings.Contains(e, "otel_metrics_gauge: alter boom") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected otel_metrics_gauge SQL error surfaced, got %v", errs)
	}
}

func TestApplyDMTTL_DayTableSQLError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if strings.Contains(q, "otel_logs") {
			return nil, errors.New("day alter boom")
		}
		return &store.Result{}, nil
	}}}
	errs := s.applyDMTTL(map[string]string{"data_management.ttl_logs_days": "30"})
	found := false
	for _, e := range errs {
		if strings.Contains(e, "otel_logs: day alter boom") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected otel_logs SQL error surfaced, got %v", errs)
	}
}

func TestApplyDMTTL_EmptySettingsNoOp(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		t.Fatal("Execute should not be called when no TTL settings are present")
		return nil, nil
	}}}
	errs := s.applyDMTTL(map[string]string{})
	if len(errs) != 0 {
		t.Fatalf("expected no errors for empty settings, got %v", errs)
	}
}

func TestApplyDMTTL_DayTableNonPositive(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	errs := s.applyDMTTL(map[string]string{"data_management.ttl_traces_days": "0"})
	if len(errs) != 1 || !strings.Contains(errs[0], "otel_traces: TTL days must be a positive integer") {
		t.Fatalf("got %v", errs)
	}
}
