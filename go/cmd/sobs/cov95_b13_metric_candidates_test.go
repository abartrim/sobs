package main

import (
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// ---- inferAutoRuleComparator -------------------------------------------------------------------

func TestInferAutoRuleComparator(t *testing.T) {
	cases := []struct{ signal, want string }{
		{"http_error_rate", "gt"},
		{"p99_latency", "gt"},
		{"request_timeout", "gt"},
		{"availability_pct", "lt"},
		{"success_ratio", "lt"},
		{"throughput", "lt"},
		{"unrelated_signal", "gt"}, // default
		{"", "gt"},
		{"AVAILABILITY_UPPER", "lt"}, // case-insensitive match
	}
	for _, c := range cases {
		if got := inferAutoRuleComparator(c.signal); got != c.want {
			t.Errorf("inferAutoRuleComparator(%q) = %q, want %q", c.signal, got, c.want)
		}
	}
}

// ---- formatAutoRuleName -------------------------------------------------------------------------

func TestFormatAutoRuleName(t *testing.T) {
	cases := []struct {
		source, signal, service, attrFp, want string
	}{
		{"traces", "latency", "checkout", "", "Auto traces/latency [checkout]"},
		{"traces", "latency", "", "", "Auto traces/latency [any]"},
		{"traces", "latency", "checkout", "fp123", "Auto traces/latency [checkout / fp123]"},
		{"traces", "latency", "", "fp123", "Auto traces/latency [any / fp123]"},
	}
	for _, c := range cases {
		if got := formatAutoRuleName(c.source, c.signal, c.service, c.attrFp); got != c.want {
			t.Errorf("formatAutoRuleName(%q,%q,%q,%q) = %q, want %q",
				c.source, c.signal, c.service, c.attrFp, got, c.want)
		}
	}
}

// ---- metricCandidateScope -----------------------------------------------------------------------

func TestMetricCandidateScope(t *testing.T) {
	t.Run("no service filter, no attr fp", func(t *testing.T) {
		where, attrSelect, attrGroup := metricCandidateScope(6, "", false)
		if where != " WHERE time >= now() - INTERVAL 6 HOUR" {
			t.Errorf("where = %q", where)
		}
		if attrSelect != "''" || attrGroup != "" {
			t.Errorf("attrSelect=%q attrGroup=%q", attrSelect, attrGroup)
		}
	})

	t.Run("with service filter and attr fp", func(t *testing.T) {
		where, attrSelect, attrGroup := metricCandidateScope(24, "checkout", true)
		want := " WHERE time >= now() - INTERVAL 24 HOUR AND ServiceName = 'checkout'"
		if where != want {
			t.Errorf("where = %q, want %q", where, want)
		}
		if attrSelect != "AttrFingerprint" || attrGroup != ", AttrFingerprint" {
			t.Errorf("attrSelect=%q attrGroup=%q", attrSelect, attrGroup)
		}
	})

	t.Run("service filter needing SQL-literal escaping", func(t *testing.T) {
		where, _, _ := metricCandidateScope(1, "o'brien", false)
		if where != ` WHERE time >= now() - INTERVAL 1 HOUR AND ServiceName = 'o''brien'` {
			t.Errorf("where = %q", where)
		}
	})
}

// ---- queryRows ------------------------------------------------------------------------------------

func TestQueryRows(t *testing.T) {
	t.Run("execute error yields nil", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			return nil, errString("boom")
		}}}
		if got := s.queryRows("SELECT 1"); got != nil {
			t.Errorf("want nil on error, got %+v", got)
		}
	})

	t.Run("success maps rows", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			return storetest.Result([]string{"ServiceName", "point_count"}, []any{"checkout", int64(5)}), nil
		}}}
		rows := s.queryRows("SELECT * FROM x")
		if len(rows) != 1 || rows[0]["ServiceName"] != "checkout" {
			t.Errorf("rows = %+v", rows)
		}
	})
}

// ---- loadAnomalyExistingSeries ------------------------------------------------------------------

func TestLoadAnomalyExistingSeries(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return storetest.Result(
			[]string{"Id", "Name", "RuleType", "SignalSource", "SignalName", "ServiceName", "AttrFingerprint",
				"Comparator", "WarningThreshold", "CriticalThreshold", "SecondarySignalSource", "SecondarySignalName",
				"SecondaryComparator", "SecondaryWarningThreshold", "SecondaryCriticalThreshold", "MinSampleCount",
				"SeasonalBucketsJson"},
			[]any{"1", "r1", "threshold", "traces", "latency", "checkout", "", "gt", 1.0, 2.0, "", "", "", 0.0, 0.0, 3, ""},
			// empty rule_type should default to "threshold"
			[]any{"2", "r2", "", "traces", "errors", "checkout", "", "gt", 1.0, 2.0, "", "", "", 0.0, 0.0, 3, ""},
		), nil
	}}}
	existing := s.loadAnomalyExistingSeries()
	if !existing[ruleKeyJoin("traces", "latency", "checkout", "", "threshold")] {
		t.Errorf("expected key present for r1: %#v", existing)
	}
	if !existing[ruleKeyJoin("traces", "errors", "checkout", "", "threshold")] {
		t.Errorf("expected key present for r2 defaulting rule_type: %#v", existing)
	}
	if len(existing) != 2 {
		t.Errorf("want 2 entries, got %d: %#v", len(existing), existing)
	}
}

// ---- buildAutoMetricRuleCandidates ----------------------------------------------------------------

func statsRow(service, source, signal, attrFP string, count int, q05, q20, q50, q80, q95 float64) []any {
	return []any{service, source, signal, attrFP, int64(count), q05, q20, q50, q80, q95}
}

func statsCols() []string {
	return []string{"ServiceName", "SignalSource", "SignalName", "AttrFingerprint", "point_count", "q05", "q20", "q50", "q80", "q95"}
}

func TestBuildAutoMetricRuleCandidates(t *testing.T) {
	t.Run("produces valid gt/lt candidates; self-correcting thresholds make 'invalid' unreachable", func(t *testing.T) {
		// autoRuleThresholds self-corrects before buildAutoMetricRuleCandidates's invalid check
		// ever runs: for "gt" it forces critical = max(warning, q50) whenever the raw q95 <
		// q80, which by construction is always >= warning; for "lt" it forces critical =
		// min(warning, q50) whenever raw q05 > q20, always <= warning. So comparator=="gt" &&
		// critical<warning (or the lt mirror) can never be true for any real quantile input —
		// the "payments" row below (q95=1 < q80=2) is corrected to critical=max(2,q50=5)=5, a
		// VALID candidate, not an invalid one.
		calls := 0
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			calls++
			// First call: stats rows. Second call: existing anomaly rules (empty).
			if calls == 1 {
				return storetest.Result(statsCols(),
					statsRow("checkout", "traces", "error_rate", "", 100, 1, 2, 5, 8, 9),   // gt, q95>=q80: no correction needed
					statsRow("checkout", "traces", "availability", "", 100, 1, 2, 5, 8, 9), // lt, q05<=q20: no correction needed
					statsRow("payments", "traces", "error_rate", "", 100, 9, 8, 5, 2, 1),   // gt, q95<q80: corrected to critical=max(q80,q50)=5
				), nil
			}
			return &store.Result{}, nil // no existing rules
		}}}
		candidates, stats := s.buildAutoMetricRuleCandidates(6, 10, "", false)
		if stats["examined"] != 3 {
			t.Fatalf("examined = %d, want 3", stats["examined"])
		}
		if stats["invalid"] != 0 {
			t.Errorf("invalid = %d, want 0 (self-correction makes all 3 rows valid)", stats["invalid"])
		}
		if len(candidates) != 3 {
			t.Fatalf("want 3 valid candidates, got %d: %+v", len(candidates), candidates)
		}
	})

	t.Run("skips a candidate already covered by an existing threshold rule", func(t *testing.T) {
		calls := 0
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			calls++
			if calls == 1 {
				return storetest.Result(statsCols(), statsRow("checkout", "traces", "error_rate", "", 100, 1, 2, 5, 8, 9)), nil
			}
			return storetest.Result(
				[]string{"Id", "Name", "RuleType", "SignalSource", "SignalName", "ServiceName", "AttrFingerprint",
					"Comparator", "WarningThreshold", "CriticalThreshold", "SecondarySignalSource", "SecondarySignalName",
					"SecondaryComparator", "SecondaryWarningThreshold", "SecondaryCriticalThreshold", "MinSampleCount",
					"SeasonalBucketsJson"},
				[]any{"1", "r1", "threshold", "traces", "error_rate", "checkout", "", "gt", 1.0, 2.0, "", "", "", 0.0, 0.0, 3, ""},
			), nil
		}}}
		candidates, stats := s.buildAutoMetricRuleCandidates(6, 10, "", false)
		if stats["existing"] != 1 {
			t.Errorf("existing = %d, want 1", stats["existing"])
		}
		if len(candidates) != 0 {
			t.Errorf("want 0 candidates (skipped as existing), got %+v", candidates)
		}
	})
}

// ---- buildSeasonalMetricRuleCandidates -------------------------------------------------------------

func TestBuildSeasonalMetricRuleCandidates(t *testing.T) {
	bucketCols := []string{"ServiceName", "SignalSource", "SignalName", "AttrFingerprint", "bucket_key",
		"point_count", "q05", "q20", "q50", "q80", "q95"}

	t.Run("hour_of_day strategy default and bucket aggregation", func(t *testing.T) {
		calls := 0
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			calls++
			switch calls {
			case 1: // series stats
				return storetest.Result(statsCols(), statsRow("checkout", "traces", "error_rate", "", 100, 1, 2, 5, 8, 9)), nil
			case 2: // bucket rows
				return storetest.Result(bucketCols,
					[]any{"checkout", "traces", "error_rate", "", int64(3), int64(10), 1.0, 2.0, 5.0, 8.0, 9.0},
					[]any{"checkout", "traces", "error_rate", "", int64(5), int64(10), 1.0, 2.0, 5.0, 8.0, 9.0},
				), nil
			default: // existing rules (empty)
				return &store.Result{}, nil
			}
		}}}
		candidates, stats := s.buildSeasonalMetricRuleCandidates(6, 10, "", false, "bogus_strategy")
		if stats["examined"] != 1 {
			t.Errorf("examined = %d, want 1", stats["examined"])
		}
		if len(candidates) != 1 {
			t.Fatalf("want 1 candidate, got %d: %+v", len(candidates), candidates)
		}
		cand := candidates[0].(map[string]any)
		if cand["seasonal_strategy"] != "hour_of_day" {
			t.Errorf("seasonal_strategy = %v, want hour_of_day (bogus input defaults)", cand["seasonal_strategy"])
		}
		if cand["seasonal_bucket_count"] != 2 {
			t.Errorf("seasonal_bucket_count = %v, want 2", cand["seasonal_bucket_count"])
		}
	})

	t.Run("day_of_week strategy and no matching buckets", func(t *testing.T) {
		calls := 0
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			calls++
			switch calls {
			case 1:
				return storetest.Result(statsCols(), statsRow("checkout", "traces", "error_rate", "", 100, 1, 2, 5, 8, 9)), nil
			case 2:
				return &store.Result{}, nil // no bucket rows at all
			default:
				return &store.Result{}, nil
			}
		}}}
		candidates, _ := s.buildSeasonalMetricRuleCandidates(6, 10, "", false, "day_of_week")
		if len(candidates) != 1 {
			t.Fatalf("want 1 candidate, got %d", len(candidates))
		}
		cand := candidates[0].(map[string]any)
		if cand["seasonal_bucket_count"] != 0 {
			t.Errorf("seasonal_bucket_count = %v, want 0 (no buckets found)", cand["seasonal_bucket_count"])
		}
		if cand["seasonal_strategy"] != "day_of_week" {
			t.Errorf("seasonal_strategy = %v", cand["seasonal_strategy"])
		}
	})

	t.Run("skips an existing-seasonal candidate; self-correction leaves the other valid", func(t *testing.T) {
		// As in TestBuildAutoMetricRuleCandidates, autoRuleThresholds' self-correction (critical
		// = max(warning, q50) whenever raw q95 < q80) means the "checkout" row's raw q95(1) <
		// q80(2) is corrected to critical=max(2,5)=5, a VALID candidate rather than an invalid
		// one. Only the "payments" row is excluded here, via the pre-existing seasonal rule.
		calls := 0
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			calls++
			switch calls {
			case 1:
				return storetest.Result(statsCols(),
					statsRow("checkout", "traces", "error_rate", "", 100, 9, 8, 5, 2, 1), // gt, q95<q80: corrected to critical=max(q80,q50)=5, valid
					statsRow("payments", "traces", "error_rate", "", 100, 1, 2, 5, 8, 9), // valid, but blocked by existing seasonal rule
				), nil
			case 2:
				return &store.Result{}, nil
			default:
				return storetest.Result(
					[]string{"Id", "Name", "RuleType", "SignalSource", "SignalName", "ServiceName", "AttrFingerprint",
						"Comparator", "WarningThreshold", "CriticalThreshold", "SecondarySignalSource", "SecondarySignalName",
						"SecondaryComparator", "SecondaryWarningThreshold", "SecondaryCriticalThreshold", "MinSampleCount",
						"SeasonalBucketsJson"},
					[]any{"1", "r1", "seasonal", "traces", "error_rate", "payments", "", "gt", 1.0, 2.0, "", "", "", 0.0, 0.0, 3, ""},
				), nil
			}
		}}}
		candidates, stats := s.buildSeasonalMetricRuleCandidates(6, 10, "", false, "hour_of_day")
		if stats["invalid"] != 0 {
			t.Errorf("invalid = %d, want 0 (self-correction makes the checkout row valid)", stats["invalid"])
		}
		if stats["existing"] != 1 {
			t.Errorf("existing = %d, want 1", stats["existing"])
		}
		if len(candidates) != 1 {
			t.Fatalf("want 1 surviving candidate, got %+v", candidates)
		}
		cand, _ := candidates[0].(map[string]any)
		if cand["service"] != "checkout" || cand["critical_threshold"] != 5.0 {
			t.Errorf("surviving candidate = %+v, want checkout/critical=5", cand)
		}
	})
}
