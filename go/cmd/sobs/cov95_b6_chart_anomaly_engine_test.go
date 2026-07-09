package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b6_chart_anomaly_engine_test.go — targeted unit tests for undertested branches in
// chart_anomaly_engine.go (batch 6). These are a pure port of app.py's anomaly rule-evaluation
// engine; exercised directly (no HTTP layer) per the existing coverage_* test convention in this
// package.

// ---------------------------------------------------------------------------
// pyFloatRepr
// ---------------------------------------------------------------------------

func TestCov95B6_PyFloatRepr(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want string
	}{
		{"nan", nan64(), "nan"},
		{"pos_inf", posInf64(), "inf"},
		{"neg_inf", negInf64(), "-inf"},
		{"integral", 450.0, "450.0"},
		{"fractional", 1.5, "1.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pyFloatRepr(c.in); got != c.want {
				t.Errorf("pyFloatRepr(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func nan64() float64 { var z float64; return z / z }
func posInf64() float64 {
	var z float64
	return 1 / z
}
func negInf64() float64 {
	var z float64
	return -1 / z
}

// ---------------------------------------------------------------------------
// ruleDefault
// ---------------------------------------------------------------------------

func TestCov95B6_RuleDefault(t *testing.T) {
	// key absent -> default
	if got := ruleDefault(map[string]any{}, "min_sample_count", 1); got != 1 {
		t.Errorf("absent key: got %v, want 1", got)
	}
	// key present but nil -> default (Python `rule.get(key) or default`-style nil handling)
	if got := ruleDefault(map[string]any{"min_sample_count": nil}, "min_sample_count", 1); got != 1 {
		t.Errorf("nil value: got %v, want 1", got)
	}
	// key present and non-nil -> the value itself
	if got := ruleDefault(map[string]any{"min_sample_count": 5}, "min_sample_count", 1); got != 5 {
		t.Errorf("present value: got %v, want 5", got)
	}
}

// ---------------------------------------------------------------------------
// evaluateCompositeRule (server method — DB-backed lookupSecondaryRuleRow fallback)
// ---------------------------------------------------------------------------

func TestCov95B6_EvaluateCompositeRule_PrimaryNil(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	rule := map[string]any{
		"name": "R1", "comparator": "gt", "warning_threshold": 100.0, "critical_threshold": 200.0,
		"secondary_source": "sec_src", "secondary_signal": "sec_sig",
	}
	row := map[string]any{"value": 10.0, "sample_count": 5}
	got := s.evaluateCompositeRule(rule, row, nil, nil, "source", "signal", "service", "attr_fp", "value", "sample_count", "time")
	if got != nil {
		t.Errorf("primary below threshold: got %v, want nil", got)
	}
}

func TestCov95B6_EvaluateCompositeRule_NoSecondaryConfigured(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	rule := map[string]any{
		"name": "R1", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
	}
	row := map[string]any{"value": 50.0, "sample_count": 5}
	got := s.evaluateCompositeRule(rule, row, nil, nil, "source", "signal", "service", "attr_fp", "value", "sample_count", "time")
	if got != nil {
		t.Errorf("no secondary_source/secondary_signal: got %v, want nil", got)
	}
}

func TestCov95B6_EvaluateCompositeRule_SecondaryFromLatestMap(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	rule := map[string]any{
		"name": "R1", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
		"secondary_source": "sec_src", "secondary_signal": "sec_sig",
		"secondary_comparator": "gt", "secondary_warning_threshold": 1.0, "secondary_critical_threshold": 2.0,
	}
	row := map[string]any{"value": 50.0, "sample_count": 5, "service": "web", "attr_fp": "fp1", "time": "t1", "signal": "sig1"}
	latest := map[[4]string]map[string]any{
		{"web", "fp1", "sec_src", "sec_sig"}: {"value": 3.0, "sample_count": 5},
	}
	got := s.evaluateCompositeRule(rule, row, latest, nil, "source", "signal", "service", "attr_fp", "value", "sample_count", "time")
	if got == nil {
		t.Fatal("want a combined match, got nil")
	}
	if got["rule_state"] != "outlier" {
		t.Errorf("rule_state = %v, want outlier (primary crit + secondary crit)", got["rule_state"])
	}
	if !strings.Contains(got["rule_reason"].(string), "primary sig1=50") {
		t.Errorf("rule_reason = %v, missing primary signal info", got["rule_reason"])
	}
}

func TestCov95B6_EvaluateCompositeRule_SecondaryFromTimedMap(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	rule := map[string]any{
		"name": "R1", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
		"secondary_source": "sec_src", "secondary_signal": "sec_sig",
		"secondary_comparator": "gt", "secondary_warning_threshold": 1.0, "secondary_critical_threshold": 2.0,
	}
	row := map[string]any{"value": 50.0, "sample_count": 5, "service": "web", "attr_fp": "fp1", "time": "t1", "signal": "sig1"}
	timed := map[[5]string]map[string]any{
		{"web", "fp1", "sec_src", "sec_sig", "t1"}: {"value": 3.0, "sample_count": 5},
	}
	got := s.evaluateCompositeRule(rule, row, nil, timed, "source", "signal", "service", "attr_fp", "value", "sample_count", "time")
	if got == nil {
		t.Fatal("want a combined match via the timed lookup, got nil")
	}
}

func TestCov95B6_EvaluateCompositeRule_SecondaryFromDBFallback(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if !strings.Contains(q, "v_derived_signals_anomaly") {
			t.Fatalf("unexpected query: %s", q)
		}
		return storetest.Result([]string{"time", "value", "SampleCount"}, []any{"t1", 3.0, 5.0}), nil
	}}}
	rule := map[string]any{
		"name": "R1", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
		"secondary_source": "sec_src", "secondary_signal": "sec_sig",
		"secondary_comparator": "gt", "secondary_warning_threshold": 1.0, "secondary_critical_threshold": 2.0,
	}
	row := map[string]any{"value": 50.0, "sample_count": 5, "service": "web", "attr_fp": "fp1", "time": "t1", "signal": "sig1"}
	got := s.evaluateCompositeRule(rule, row, nil, nil, "source", "signal", "service", "attr_fp", "value", "sample_count", "time")
	if got == nil {
		t.Fatal("want a combined match via the DB fallback, got nil")
	}
}

func TestCov95B6_EvaluateCompositeRule_SecondaryNilEverywhere(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}} // empty result -> lookupSecondaryRuleRow returns nil
	rule := map[string]any{
		"name": "R1", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
		"secondary_source": "sec_src", "secondary_signal": "sec_sig",
	}
	row := map[string]any{"value": 50.0, "sample_count": 5, "service": "web", "attr_fp": "fp1", "time": "t1", "signal": "sig1"}
	got := s.evaluateCompositeRule(rule, row, nil, nil, "source", "signal", "service", "attr_fp", "value", "sample_count", "time")
	if got != nil {
		t.Errorf("no secondary row anywhere: got %v, want nil", got)
	}
}

func TestCov95B6_EvaluateCompositeRule_SecondaryConditionNotMet(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	rule := map[string]any{
		"name": "R1", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
		"secondary_source": "sec_src", "secondary_signal": "sec_sig",
		"secondary_comparator": "gt", "secondary_warning_threshold": 100.0, "secondary_critical_threshold": 200.0,
	}
	row := map[string]any{"value": 50.0, "sample_count": 5, "service": "web", "attr_fp": "fp1", "time": "t1", "signal": "sig1"}
	latest := map[[4]string]map[string]any{
		{"web", "fp1", "sec_src", "sec_sig"}: {"value": 3.0, "sample_count": 5},
	}
	got := s.evaluateCompositeRule(rule, row, latest, nil, "source", "signal", "service", "attr_fp", "value", "sample_count", "time")
	if got != nil {
		t.Errorf("secondary below its own threshold: got %v, want nil", got)
	}
}

// TestCov95B6_EvaluateCompositeRule_NoTimeKey covers the timeKey=="" branch (skips the timed
// lookup entirely, relying on latest/DB fallback), and the secVal/secSc fallback-to-"value"/
// "sample_count" keys (DB row shape uses those, not valueKey/sampleCountKey).
func TestCov95B6_EvaluateCompositeRule_NoTimeKey(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return storetest.Result([]string{"time", "value", "SampleCount"}, []any{"t1", 3.0, 5.0}), nil
	}}}
	rule := map[string]any{
		"name": "R1", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
		"secondary_source": "sec_src", "secondary_signal": "sec_sig",
		"secondary_comparator": "gt", "secondary_warning_threshold": 1.0, "secondary_critical_threshold": 2.0,
	}
	row := map[string]any{"value": 50.0, "sample_count": 5, "service": "web", "attr_fp": "fp1", "signal": "sig1"}
	got := s.evaluateCompositeRule(rule, row, nil, nil, "source", "signal", "service", "attr_fp", "value", "sample_count", "")
	if got == nil {
		t.Fatal("want a combined match with timeKey empty (DB fallback), got nil")
	}
}

// ---------------------------------------------------------------------------
// bucketNonEmpty / seasonalBucketFloat / parseSeasonalTime / isoWeekday
// (exercised indirectly through evaluateSeasonalRule, mirroring the existing test style)
// ---------------------------------------------------------------------------

func TestCov95B6_EvaluateSeasonalRule_EmptyBucketSkipsOverride(t *testing.T) {
	rule := map[string]any{
		"name": "Seasonal", "comparator": "gt", "warning_threshold": 100.0, "critical_threshold": 200.0,
		"seasonal_buckets_json": `{"strategy":"hour_of_day","buckets":{"10":{}}}`,
	}
	// hour 10 maps to an EMPTY bucket object -> falsy -> no override, is_seasonal stays false.
	// The un-overridden threshold (100 warn / 200 crit) is still evaluated, and 150 >= 100 still
	// triggers a warning -- just without the seasonal override applied.
	got := evaluateSeasonalRule(rule, 150.0, 10, "2024-01-15T10:30:00+00:00")
	if got == nil {
		t.Fatal("want the un-overridden warning threshold to trigger, got nil")
	}
	if got["rule_seasonal"] != false {
		t.Errorf("rule_seasonal = %v, want false (empty bucket skips override)", got["rule_seasonal"])
	}
	if got["rule_state"] != "warning" {
		t.Errorf("rule_state = %v, want warning", got["rule_state"])
	}
}

func TestCov95B6_EvaluateSeasonalRule_NonEmptyBucketOverridesAndTriggers(t *testing.T) {
	rule := map[string]any{
		"name": "Seasonal", "comparator": "gt", "warning_threshold": 100.0, "critical_threshold": 200.0,
		"seasonal_buckets_json": `{"strategy":"hour_of_day","buckets":{"10":{"warning":10,"critical":20}}}`,
	}
	got := evaluateSeasonalRule(rule, 15.0, 10, "2024-01-15T10:30:00+00:00")
	if got == nil {
		t.Fatal("want a match (overridden warning=10 < value=15), got nil")
	}
	if got["rule_seasonal"] != true {
		t.Errorf("rule_seasonal = %v, want true", got["rule_seasonal"])
	}
	if got["rule_state"] != "warning" {
		t.Errorf("rule_state = %v, want warning", got["rule_state"])
	}
}

func TestCov95B6_EvaluateSeasonalRule_DayOfWeekStrategy(t *testing.T) {
	rule := map[string]any{
		"name": "Seasonal", "comparator": "gt", "warning_threshold": 100.0, "critical_threshold": 200.0,
		// 2024-01-15 is a Monday -> isoWeekday = 1.
		"seasonal_buckets_json": `{"strategy":"day_of_week","buckets":{"1":{"warning":10,"critical":20}}}`,
	}
	got := evaluateSeasonalRule(rule, 15.0, 10, "2024-01-15T10:30:00+00:00")
	if got == nil {
		t.Fatal("want a match via day_of_week bucket key '1' (Monday), got nil")
	}
}

func TestCov95B6_EvaluateSeasonalRule_NonNumericBucketValueAborts(t *testing.T) {
	rule := map[string]any{
		"name": "Seasonal", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 6.0,
		"seasonal_buckets_json": `{"strategy":"hour_of_day","buckets":{"10":{"warning":"not-a-number"}}}`,
	}
	// float(bucket.get("warning", ...)) raises in Python -> is_seasonal stays False, override
	// aborted (warning_threshold untouched at 5.0). value=100 still triggers a normal critical.
	got := evaluateSeasonalRule(rule, 100.0, 10, "2024-01-15T10:30:00+00:00")
	if got == nil {
		t.Fatal("want the un-overridden threshold to still trigger, got nil")
	}
	if got["rule_seasonal"] != false {
		t.Errorf("rule_seasonal = %v, want false (aborted override)", got["rule_seasonal"])
	}
}

func TestCov95B6_EvaluateSeasonalRule_UnparseableTime(t *testing.T) {
	rule := map[string]any{
		"name": "Seasonal", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
		"seasonal_buckets_json": `{"strategy":"hour_of_day","buckets":{"10":{"warning":1,"critical":2}}}`,
	}
	// Unparseable time -> parseSeasonalTime fails -> no override applied, falls through to base
	// thresholds (still triggers since 50 >= 10).
	got := evaluateSeasonalRule(rule, 50.0, 10, "not-a-real-time")
	if got == nil {
		t.Fatal("want the un-overridden threshold to trigger, got nil")
	}
	if got["rule_seasonal"] != false {
		t.Errorf("rule_seasonal = %v, want false", got["rule_seasonal"])
	}
}

func TestCov95B6_EvaluateSeasonalRule_MissingBucketKey(t *testing.T) {
	rule := map[string]any{
		"name": "Seasonal", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
		"seasonal_buckets_json": `{"strategy":"hour_of_day","buckets":{"3":{"warning":1,"critical":2}}}`,
	}
	// hour 10 has no entry in buckets -> `buckets.get(bucket_key)` is None -> has=false, no override.
	got := evaluateSeasonalRule(rule, 50.0, 10, "2024-01-15T10:30:00+00:00")
	if got == nil {
		t.Fatal("want the un-overridden threshold to trigger, got nil")
	}
	if got["rule_seasonal"] != false {
		t.Errorf("rule_seasonal = %v, want false", got["rule_seasonal"])
	}
}

func TestCov95B6_EvaluateSeasonalRule_NoBucketsJSON(t *testing.T) {
	rule := map[string]any{
		"name": "Seasonal", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
	}
	got := evaluateSeasonalRule(rule, 50.0, 10, "2024-01-15T10:30:00+00:00")
	if got == nil {
		t.Fatal("want the base threshold to trigger with no seasonal_buckets_json, got nil")
	}
	if got["rule_seasonal"] != false {
		t.Errorf("rule_seasonal = %v, want false", got["rule_seasonal"])
	}
}

func TestCov95B6_EvaluateSeasonalRule_InvalidJSON(t *testing.T) {
	rule := map[string]any{
		"name": "Seasonal", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
		"seasonal_buckets_json": `not-json{{{`,
	}
	got := evaluateSeasonalRule(rule, 50.0, 10, "2024-01-15T10:30:00+00:00")
	if got == nil {
		t.Fatal("want the base threshold to trigger despite invalid JSON, got nil")
	}
}

func TestCov95B6_EvaluateSeasonalRule_NilTimeValue(t *testing.T) {
	rule := map[string]any{
		"name": "Seasonal", "comparator": "gt", "warning_threshold": 5.0, "critical_threshold": 10.0,
		"seasonal_buckets_json": `{"strategy":"hour_of_day","buckets":{"10":{"warning":1,"critical":2}}}`,
	}
	got := evaluateSeasonalRule(rule, 50.0, 10, nil)
	if got == nil {
		t.Fatal("want the base threshold to trigger with nil timeValue, got nil")
	}
}

func TestCov95B6_IsoWeekday_SundayWraps(t *testing.T) {
	// 2024-01-14 is a Sunday.
	sun, ok := parseSeasonalTime("2024-01-14T00:00:00+00:00")
	if !ok {
		t.Fatal("parseSeasonalTime failed to parse a valid RFC3339 time")
	}
	if got := isoWeekday(sun); got != 7 {
		t.Errorf("isoWeekday(Sunday) = %d, want 7", got)
	}
	mon := sun.Add(24 * time.Hour)
	if got := isoWeekday(mon); got != 1 {
		t.Errorf("isoWeekday(Monday) = %d, want 1", got)
	}
}

func TestCov95B6_ParseSeasonalTime_Formats(t *testing.T) {
	cases := []string{
		"2024-01-15T10:30:00+00:00",
		"2024-01-15T10:30:00+05:30",
		"2024-01-15 10:30:00",
		"2024-01-15T10:30:00.123456",
	}
	for _, in := range cases {
		if _, ok := parseSeasonalTime(in); !ok {
			t.Errorf("parseSeasonalTime(%q) failed, want success", in)
		}
	}
	if _, ok := parseSeasonalTime("garbage"); ok {
		t.Error("parseSeasonalTime(garbage) succeeded, want failure")
	}
}

// ---------------------------------------------------------------------------
// prepareTemplateRows (server method) — via s.annotateRowsWithRules + s.loadAnomalyRulesCtx
// ---------------------------------------------------------------------------

func TestCov95B6_PrepareTemplateRows_TooFewColumns(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	columns := []any{"time", "value"}
	rows := []map[string]any{{"time": "t1", "value": 1.0}}
	gotCols, gotRows, errMsg := s.prepareTemplateRows(columns, rows, map[string]int{})
	if errMsg != "" {
		t.Errorf("errMsg = %q, want empty (pass-through on too-few columns)", errMsg)
	}
	if len(gotCols) != len(columns) || len(gotRows) != len(rows) {
		t.Errorf("expected pass-through of columns/rows unchanged, got %v / %v", gotCols, gotRows)
	}
}

func TestCov95B6_PrepareTemplateRows_AnnotatesAndProjects(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_anomaly_rules") {
			// One threshold rule matching source/signal "src"/"sig".
			return storetest.Result(
				[]string{"Id", "Name", "RuleType", "SignalSource", "SignalName", "ServiceName", "AttrFingerprint",
					"Comparator", "WarningThreshold", "CriticalThreshold", "SecondarySignalSource", "SecondarySignalName",
					"SecondaryComparator", "SecondaryWarningThreshold", "SecondaryCriticalThreshold", "MinSampleCount", "SeasonalBucketsJson"},
				[]any{"r1", "HighValue", "threshold", "src", "sig", "", "", "gt", 10.0, 20.0, "", "", "gt", 0.0, 0.0, 1.0, ""},
			), nil
		}
		return &store.Result{}, nil
	}}}
	columns := []any{"time", "service", "source", "signal", "attr_fp", "value", "sample_count",
		"baseline_mean", "baseline_lower", "baseline_upper", "anomaly_state", "anomaly_score"}
	rows := []map[string]any{
		{"time": "t1", "service": "web", "source": "src", "signal": "sig", "attr_fp": "fp1",
			"value": 25.0, "sample_count": 5, "baseline_mean": 10.0, "baseline_lower": 5.0,
			"baseline_upper": 15.0, "anomaly_state": "normal", "anomaly_score": 0.0},
	}
	roleIndices := map[string]int{}
	for i, c := range columns {
		roleIndices[toStr(c)] = i
	}
	preparedCols, preparedRows, errMsg := s.prepareTemplateRows(columns, rows, roleIndices)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if len(preparedRows) != 1 {
		t.Fatalf("want 1 prepared row, got %d", len(preparedRows))
	}
	pr := preparedRows[0]
	if pr["rule_state"] != "outlier" {
		t.Errorf("rule_state = %v, want outlier (value 25 >= critical 20)", pr["rule_state"])
	}
	if pr["effective_state"] != "outlier" {
		t.Errorf("effective_state = %v, want outlier", pr["effective_state"])
	}
	// preparedCols must include the 4 rule columns appended to the required set.
	foundRuleState := false
	for _, c := range preparedCols {
		if toStr(c) == "rule_state" {
			foundRuleState = true
		}
	}
	if !foundRuleState {
		t.Errorf("preparedCols missing rule_state: %v", preparedCols)
	}
}

// ---------------------------------------------------------------------------
// iStr
// ---------------------------------------------------------------------------

func TestCov95B6_IStr(t *testing.T) {
	if n, ok := iStr(10.0); !ok || n != 10 {
		t.Errorf("iStr(10.0) = (%d, %v), want (10, true) [integral float]", n, ok)
	}
	if _, ok := iStr(10.5); ok {
		t.Error("iStr(10.5) succeeded, want failure (fractional float)")
	}
	if n, ok := iStr("42"); !ok || n != 42 {
		t.Errorf(`iStr("42") = (%d, %v), want (42, true)`, n, ok)
	}
	if _, ok := iStr("5.0"); ok {
		t.Error(`iStr("5.0") succeeded, want failure (non-integer string)`)
	}
	if _, ok := iStr("not-a-number"); ok {
		t.Error("iStr(garbage string) succeeded, want failure")
	}
}

// ---------------------------------------------------------------------------
// evaluateThresholdCondition — lt comparator branch (not yet covered) + min-sample-count gate
// ---------------------------------------------------------------------------

func TestCov95B6_EvaluateThresholdCondition_LtComparator(t *testing.T) {
	got := evaluateThresholdCondition("LowSignal", "lt", 10.0, 5.0, 3.0, 10, 1)
	if got == nil {
		t.Fatal("want a match (value 3 <= critical 5), got nil")
	}
	if got["rule_state"] != "outlier" {
		t.Errorf("rule_state = %v, want outlier", got["rule_state"])
	}
	if !strings.Contains(got["rule_reason"].(string), "<=") {
		t.Errorf("rule_reason = %v, want it to use <=", got["rule_reason"])
	}
}

func TestCov95B6_EvaluateThresholdCondition_LtWarning(t *testing.T) {
	got := evaluateThresholdCondition("LowSignal", "lt", 10.0, 5.0, 8.0, 10, 1)
	if got == nil {
		t.Fatal("want a warning match (value 8 <= warning 10, but > critical 5), got nil")
	}
	if got["rule_state"] != "warning" {
		t.Errorf("rule_state = %v, want warning", got["rule_state"])
	}
}

func TestCov95B6_EvaluateThresholdCondition_BelowMinSampleCount(t *testing.T) {
	got := evaluateThresholdCondition("R", "gt", 10.0, 20.0, 50.0, 2, 5)
	if got != nil {
		t.Errorf("sample_count 2 < min_sample_count 5: got %v, want nil", got)
	}
}

func TestCov95B6_EvaluateThresholdCondition_BadValue(t *testing.T) {
	if got := evaluateThresholdCondition("R", "gt", 10.0, 20.0, "not-a-number", 10, 1); got != nil {
		t.Errorf("non-numeric value: got %v, want nil", got)
	}
	if got := evaluateThresholdCondition("R", "gt", 10.0, 20.0, 50.0, "not-a-number", 1); got != nil {
		t.Errorf("non-numeric sample_count: got %v, want nil", got)
	}
}
