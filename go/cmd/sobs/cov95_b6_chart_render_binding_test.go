package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b6_chart_render_binding_test.go — targeted unit tests for undertested branches in
// chart_render_binding.go (batch 6). Pure functions of columns/rows/bindings/jsonenc trees;
// exercised directly per the existing coverage_chart_binding_gaps_test.go / coverage_pure_*
// convention in this package. Checked against those files first to avoid duplicating existing
// coverage (formatDrilldownTime and normalizeCustomSeriesPointOrder already have dedicated tests
// elsewhere, so this file focuses on the remaining gaps).

// ---------------------------------------------------------------------------
// namedDatasetField
// ---------------------------------------------------------------------------

func TestCov95B6_NamedDatasetField(t *testing.T) {
	ds := jsonenc.NewObject().Set("rows", []any{[]any{1, 2}}).Set("records", []any{}).Set("columns", nil)
	if got := namedDatasetField(ds, "rows"); len(got) != 1 {
		t.Errorf("rows: got %v, want the non-empty list", got)
	}
	if got := namedDatasetField(ds, "records"); len(got) != 0 {
		t.Errorf("records (empty list stored): got %v, want []", got)
	}
	if got := namedDatasetField(ds, "columns"); len(got) != 0 {
		t.Errorf("columns (nil stored): got %v, want []", got)
	}
	if got := namedDatasetField(ds, "missing_key"); len(got) != 0 {
		t.Errorf("missing key: got %v, want []", got)
	}
}

// ---------------------------------------------------------------------------
// parseCustomJSONConfig
// ---------------------------------------------------------------------------

func TestCov95B6_ParseCustomJSONConfig(t *testing.T) {
	// nil visual -> {} (via getVisual returning nil, nil), ok=true
	if v, ok := parseCustomJSONConfig(nil, "custom_mapping_json"); !ok {
		t.Errorf("nil visual: ok = %v, want true", ok)
	} else if obj, isObj := v.(*jsonenc.Object); !isObj || obj.Len() != 0 {
		t.Errorf("nil visual: got %#v, want empty object", v)
	}

	// key absent -> {}
	visual := jsonenc.NewObject()
	if v, ok := parseCustomJSONConfig(visual, "custom_mapping_json"); !ok {
		t.Errorf("absent key: ok = %v, want true", ok)
	} else if obj, isObj := v.(*jsonenc.Object); !isObj || obj.Len() != 0 {
		t.Errorf("absent key: got %#v, want empty object", v)
	}

	// value already an *jsonenc.Object -> pass through
	inner := jsonenc.NewObject().Set("a", 1)
	visual2 := jsonenc.NewObject().Set("custom_mapping_json", inner)
	if v, ok := parseCustomJSONConfig(visual2, "custom_mapping_json"); !ok || v != any(inner) {
		t.Errorf("object passthrough: got (%#v, %v), want (inner, true)", v, ok)
	}

	// value already a []any -> pass through
	list := []any{1, 2, 3}
	visual3 := jsonenc.NewObject().Set("custom_mapping_json", list)
	if v, ok := parseCustomJSONConfig(visual3, "custom_mapping_json"); !ok {
		t.Errorf("list passthrough: ok = %v, want true", ok)
	} else if lv, isList := v.([]any); !isList || len(lv) != 3 {
		t.Errorf("list passthrough: got %#v", v)
	}

	// value is a blank string -> {}
	visual4 := jsonenc.NewObject().Set("custom_mapping_json", "   ")
	if v, ok := parseCustomJSONConfig(visual4, "custom_mapping_json"); !ok {
		t.Errorf("blank string: ok = %v, want true", ok)
	} else if obj, isObj := v.(*jsonenc.Object); !isObj || obj.Len() != 0 {
		t.Errorf("blank string: got %#v, want empty object", v)
	}

	// value is a valid JSON string -> parsed
	visual5 := jsonenc.NewObject().Set("custom_mapping_json", `{"x": 1}`)
	if v, ok := parseCustomJSONConfig(visual5, "custom_mapping_json"); !ok {
		t.Errorf("valid JSON string: ok = %v, want true", ok)
	} else if _, isObj := v.(*jsonenc.Object); !isObj {
		t.Errorf("valid JSON string: got %#v, want an object", v)
	}

	// value is an invalid JSON string -> ok=false
	visual6 := jsonenc.NewObject().Set("custom_mapping_json", `not-json{{{`)
	if _, ok := parseCustomJSONConfig(visual6, "custom_mapping_json"); ok {
		t.Error("invalid JSON string: ok = true, want false")
	}
}

// ---------------------------------------------------------------------------
// customSortRank / customSortKeyLess
// ---------------------------------------------------------------------------

func TestCov95B6_CustomSortRank_ChDateTime(t *testing.T) {
	d := chDateTime{s: "Mon, 15 Jan 2024 10:30:45 GMT"}
	rank, f, s := customSortRank(d)
	if rank != 0 {
		t.Errorf("parseable chDateTime: rank = %d, want 0", rank)
	}
	if f == 0 {
		t.Errorf("parseable chDateTime: unix nanos = 0, want nonzero")
	}
	_ = s

	unparseable := chDateTime{s: "not-a-real-date"}
	rank2, _, str2 := customSortRank(unparseable)
	if rank2 != 3 || str2 != "not-a-real-date" {
		t.Errorf("unparseable chDateTime: got rank=%d str=%q, want rank=3 str=%q", rank2, str2, "not-a-real-date")
	}
}

func TestCov95B6_CustomSortRank_Numbers(t *testing.T) {
	if rank, f, _ := customSortRank(json.Number("3.5")); rank != 1 || f != 3.5 {
		t.Errorf("json.Number: got rank=%d f=%v, want rank=1 f=3.5", rank, f)
	}
	if rank, f, _ := customSortRank(2.5); rank != 1 || f != 2.5 {
		t.Errorf("float64: got rank=%d f=%v, want rank=1 f=2.5", rank, f)
	}
	if rank, f, _ := customSortRank(7); rank != 1 || f != 7 {
		t.Errorf("int: got rank=%d f=%v, want rank=1 f=7", rank, f)
	}
	if rank, f, _ := customSortRank(true); rank != 1 || f != 1 {
		t.Errorf("bool true: got rank=%d f=%v, want rank=1 f=1", rank, f)
	}
	if rank, f, _ := customSortRank(false); rank != 1 || f != 0 {
		t.Errorf("bool false: got rank=%d f=%v, want rank=1 f=0", rank, f)
	}
}

func TestCov95B6_CustomSortRank_Strings(t *testing.T) {
	// ISO-parseable string -> rank 0 (datetime).
	rank, _, _ := customSortRank("2024-01-15T10:30:45Z")
	if rank != 0 {
		t.Errorf("ISO string: rank = %d, want 0", rank)
	}
	// Numeric string is NOT float-parsed -> rank 2, lexicographic.
	rank2, _, s2 := customSortRank("10")
	if rank2 != 2 || s2 != "10" {
		t.Errorf(`numeric string "10": got rank=%d s=%q, want rank=2 s="10"`, rank2, s2)
	}
	// Non-numeric, non-date string -> rank 2 as well (text).
	rank3, _, s3 := customSortRank("hello")
	if rank3 != 2 || s3 != "hello" {
		t.Errorf(`plain string: got rank=%d s=%q, want rank=2 s="hello"`, rank3, s3)
	}
}

func TestCov95B6_CustomSortRank_Other(t *testing.T) {
	type weird struct{ X int }
	rank, _, s := customSortRank(weird{X: 5})
	if rank != 3 {
		t.Errorf("other type: rank = %d, want 3", rank)
	}
	if s == "" {
		t.Error("other type: str repr empty, want non-empty")
	}
}

func TestCov95B6_CustomSortKeyLess_CrossRank(t *testing.T) {
	// rank 1 (number) < rank 2 (text)
	if !customSortKeyLess(5, "hello") {
		t.Error("number should sort before text")
	}
	// rank 2 text vs text: lexicographic
	if !customSortKeyLess("10", "2") {
		t.Error(`"10" should sort before "2" lexicographically (numeric strings are not float-parsed)`)
	}
	// rank 3 vs rank 3: lexicographic on str(value)
	type weird struct{}
	if customSortKeyLess(weird{}, weird{}) {
		t.Error("equal rank-3 values should not be less-than each other")
	}
}

// ---------------------------------------------------------------------------
// resolveTemplateStringCustom
// ---------------------------------------------------------------------------

func TestCov95B6_ResolveTemplateStringCustom(t *testing.T) {
	record := map[string]any{"service": "web", "count": 42}
	got := resolveTemplateStringCustom("svc={{ service }} n={{count}} missing={{nope}}", record)
	want := "svc=web n=42 missing="
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// nil value in the record -> "".
	record2 := map[string]any{"x": nil}
	if got := resolveTemplateStringCustom("v={{x}}", record2); got != "v=" {
		t.Errorf("nil record value: got %q, want %q", got, "v=")
	}
	// nil record entirely -> every placeholder resolves empty (map lookup on nil map is safe).
	if got := resolveTemplateStringCustom("v={{x}}", nil); got != "v=" {
		t.Errorf("nil record: got %q, want %q", got, "v=")
	}
	// no placeholders -> unchanged.
	if got := resolveTemplateStringCustom("plain text", record); got != "plain text" {
		t.Errorf("no placeholders: got %q, want unchanged", got)
	}
}

// ---------------------------------------------------------------------------
// numListAt / min2 / minFloats / sprintfPlus0f
// ---------------------------------------------------------------------------

func TestCov95B6_NumListAt(t *testing.T) {
	bindings := map[string]any{"time": []any{1, 2, 3}, "other": "not-a-list"}
	if got := numListAt(bindings, "time"); len(got) != 3 {
		t.Errorf("time: got %v, want length 3", got)
	}
	if got := numListAt(bindings, "other"); got != nil {
		t.Errorf("non-list value: got %v, want nil", got)
	}
	if got := numListAt(bindings, "missing"); got != nil {
		t.Errorf("missing key: got %v, want nil", got)
	}
}

func TestCov95B6_Min2(t *testing.T) {
	if got := min2(3, 5); got != 3 {
		t.Errorf("min2(3,5) = %d, want 3", got)
	}
	if got := min2(5, 3); got != 3 {
		t.Errorf("min2(5,3) = %d, want 3", got)
	}
	if got := min2(4, 4); got != 4 {
		t.Errorf("min2(4,4) = %d, want 4", got)
	}
}

func TestCov95B6_MinFloats(t *testing.T) {
	if got := minFloats([]float64{3.5, 1.2, 9.9, -2.0}); got != -2.0 {
		t.Errorf("minFloats: got %v, want -2.0", got)
	}
	if got := minFloats([]float64{42.0}); got != 42.0 {
		t.Errorf("minFloats single element: got %v, want 42.0", got)
	}
}

func TestCov95B6_SprintfPlus0f(t *testing.T) {
	if got := sprintfPlus0f(5.0); got != "+5" {
		t.Errorf("sprintfPlus0f(5.0) = %q, want %q", got, "+5")
	}
	if got := sprintfPlus0f(0.0); got != "+0" {
		t.Errorf("sprintfPlus0f(0.0) = %q, want %q", got, "+0")
	}
	if got := sprintfPlus0f(-5.0); got != "-5" {
		t.Errorf("sprintfPlus0f(-5.0) = %q, want %q", got, "-5")
	}
}

// ---------------------------------------------------------------------------
// extractDerivedSignalBindings — the "ratio" (non-delta) branch, and missing-list early return
// ---------------------------------------------------------------------------

func TestCov95B6_ExtractDerivedSignalBindings_RatioBranch(t *testing.T) {
	bindings := map[string]any{
		"signal":          []any{"error_ratio"},
		"time":            []any{"t1", "t2"},
		"value":           []any{0.5, 0.9},
		"baseline_mean":   []any{0.4, 0.4},
		"baseline_lower":  []any{0.1, 0.1},
		"baseline_upper":  []any{0.8, 0.8},
		"effective_state": []any{"normal", "outlier"},
	}
	err := extractDerivedSignalBindings("derived_signal_overlay", bindings)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bindings["value_axis_min"] != 0 || bindings["value_axis_max"] != 1 {
		t.Errorf("ratio signal: value_axis_min/max = %v/%v, want 0/1", bindings["value_axis_min"], bindings["value_axis_max"])
	}
	if bindings["y_axis_name"] != "Value" {
		t.Errorf("ratio (non-delta) branch: y_axis_name = %v, want Value (unchanged from default)", bindings["y_axis_name"])
	}
	vp, ok := bindings["value_points"].([]any)
	if !ok || len(vp) != 2 {
		t.Fatalf("value_points = %#v, want 2 points", bindings["value_points"])
	}
	// baseline_lower is clamped to >= 0 in the ratio branch.
	blp, _ := bindings["baseline_lower_points"].([]any)
	if len(blp) != 2 {
		t.Fatalf("baseline_lower_points = %#v, want 2 points", bindings["baseline_lower_points"])
	}
}

func TestCov95B6_ExtractDerivedSignalBindings_VolumeTokenSetsAxisMin(t *testing.T) {
	// A "count"-token signal sets value_axis_min=0 initially, but since it is NOT a ratio signal
	// the delta branch runs afterward and recomputes value_axis_min/max from the plotted delta-%
	// bounds (mirrors app.py: the volume/count pre-check result is overwritten by the later delta
	// axis computation for any non-ratio signal). This test pins that actual sequencing.
	bindings := map[string]any{
		"signal":         []any{"request_count"},
		"time":           []any{"t1"},
		"value":          []any{5.0},
		"baseline_mean":  []any{5.0},
		"baseline_lower": []any{1.0},
		"baseline_upper": []any{9.0},
	}
	if err := extractDerivedSignalBindings("derived_signal_overlay", bindings); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := bindings["value_axis_min"].(float64); !ok {
		t.Errorf("count token (delta branch): value_axis_min = %#v, want a recomputed float64", bindings["value_axis_min"])
	}
	if _, ok := bindings["value_axis_max"].(float64); !ok {
		t.Errorf("count token (delta branch): value_axis_max = %#v, want a recomputed float64", bindings["value_axis_max"])
	}
}

func TestCov95B6_ExtractDerivedSignalBindings_MissingRequiredList(t *testing.T) {
	bindings := map[string]any{"signal": []any{"foo"}, "time": []any{"t1"}}
	// baseline_mean/lower/upper/value missing entirely -> early return, no error, bindings mostly
	// left at their pre-set defaults.
	if err := extractDerivedSignalBindings("derived_signal_overlay", bindings); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, has := bindings["value_points"]; has {
		t.Errorf("value_points should be absent on early return, got %v", bindings["value_points"])
	}
}

func TestCov95B6_ExtractDerivedSignalBindings_NoSignalBinding(t *testing.T) {
	// bindings["signal"] absent entirely -> signalName stays "" -> delta branch (not ratio).
	bindings := map[string]any{
		"time":           []any{"t1"},
		"value":          []any{20.0},
		"baseline_mean":  []any{10.0},
		"baseline_lower": []any{5.0},
		"baseline_upper": []any{15.0},
	}
	if err := extractDerivedSignalBindings("derived_signal_overlay", bindings); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bindings["y_axis_name"] != "Delta %" {
		t.Errorf("y_axis_name = %v, want Delta %% (delta branch)", bindings["y_axis_name"])
	}
}

func TestCov95B6_ExtractDerivedSignalBindings_ZeroBaselineDeltaGuard(t *testing.T) {
	// baseline_mean ~ 0 -> the delta-branch "near-zero" guard sets all plot values to 0 for that
	// point, avoiding a division by ~0.
	bindings := map[string]any{
		"time":           []any{"t1"},
		"value":          []any{5.0},
		"baseline_mean":  []any{0.0},
		"baseline_lower": []any{0.0},
		"baseline_upper": []any{0.0},
	}
	if err := extractDerivedSignalBindings("derived_signal_overlay", bindings); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	vp, ok := bindings["value_points"].([]any)
	if !ok || len(vp) != 1 {
		t.Fatalf("value_points = %#v, want 1 point", bindings["value_points"])
	}
	point := vp[0].([]any)
	if point[1] != 0.0 {
		t.Errorf("near-zero baseline: plotted value = %v, want 0", point[1])
	}
}

func TestCov95B6_ExtractDerivedSignalBindings_NonNumericErrorsPropagate(t *testing.T) {
	// Ratio-branch non-numeric cells raise (float() semantics) at each of value/baseline_mean/
	// baseline_lower/baseline_upper — exercise the ratio-branch error paths not covered by the
	// existing delta-branch test in coverage_chart_binding_gaps_test.go.
	base := func() map[string]any {
		return map[string]any{
			"signal":         []any{"error_ratio"},
			"time":           []any{"t1"},
			"value":          []any{0.5},
			"baseline_mean":  []any{0.4},
			"baseline_lower": []any{0.1},
			"baseline_upper": []any{0.8},
		}
	}
	cases := []string{"value", "baseline_mean", "baseline_lower", "baseline_upper"}
	for _, key := range cases {
		b := base()
		b[key] = []any{"not-a-number"}
		if err := extractDerivedSignalBindings("derived_signal_overlay", b); err == nil {
			t.Errorf("bad %s cell in ratio branch: want an error, got nil", key)
		}
	}
}

func TestCov95B6_ExtractDerivedSignalBindings_LatestValueBaselineErrors(t *testing.T) {
	// The final "latestValue"/"latestBaseline" float() calls (used for signal_summary) can also
	// raise on a non-numeric last element.
	bindings := map[string]any{
		"time":           []any{"t1"},
		"value":          []any{"not-a-number"},
		"baseline_mean":  []any{10.0},
		"baseline_lower": []any{5.0},
		"baseline_upper": []any{15.0},
	}
	if err := extractDerivedSignalBindings("derived_signal_overlay", bindings); err == nil {
		t.Error("bad latest value: want an error, got nil")
	}

	bindings2 := map[string]any{
		"time":           []any{"t1"},
		"value":          []any{20.0},
		"baseline_mean":  []any{"not-a-number"},
		"baseline_lower": []any{5.0},
		"baseline_upper": []any{15.0},
	}
	if err := extractDerivedSignalBindings("derived_signal_overlay", bindings2); err == nil {
		t.Error("bad latest baseline: want an error, got nil")
	}
}

// ---------------------------------------------------------------------------
// attachDrilldownMetadata — heatmap branch + time-templates nil-guard
// ---------------------------------------------------------------------------

func TestCov95B6_AttachDrilldownMetadata_NilDrilldown(t *testing.T) {
	opt := jsonenc.NewObject().Set("series", []any{})
	// Should be a no-op, not panic.
	attachDrilldownMetadata("heatmap", nil, map[string]any{}, opt)
}

func TestCov95B6_AttachDrilldownMetadata_TimeTemplateNoTimeBinding(t *testing.T) {
	drilldown := jsonenc.NewObject().Set("bucket_seconds", 60)
	opt := jsonenc.NewObject().Set("series", []any{})
	// bindings has no "time" key -> early return, no panic.
	attachDrilldownMetadata("time_series_percentiles", drilldown, map[string]any{}, opt)
}

func TestCov95B6_AttachDrilldownMetadata_Heatmap(t *testing.T) {
	drilldown := jsonenc.NewObject().Set("bucket_seconds", 60)
	dataPoint := []any{0, 1, 5.0}
	series := jsonenc.NewObject().Set("data", []any{dataPoint})
	opt := jsonenc.NewObject().Set("series", []any{series})
	bindings := map[string]any{
		"x_unique_values": []any{"svcA", "svcB"},
		"y_unique_values": []any{"2024-01-01", "2024-01-02"},
	}
	attachDrilldownMetadata("heatmap", drilldown, bindings, opt)
	dataV, _ := series.Get("data")
	data := dataV.([]any)
	if len(data) != 1 {
		t.Fatalf("want 1 data point, got %d", len(data))
	}
	wrapped, ok := data[0].(*jsonenc.Object)
	if !ok {
		t.Fatalf("data point not wrapped in an object: %#v", data[0])
	}
	ddV, _ := wrapped.Get("drilldown")
	dd, ok := ddV.(*jsonenc.Object)
	if !ok {
		t.Fatalf("drilldown missing/wrong type: %#v", ddV)
	}
	svcV, _ := dd.Get("service")
	if svcV != "svcA" {
		t.Errorf("service = %v, want svcA (x index 0)", svcV)
	}
}

func TestCov95B6_AttachDrilldownMetadata_HeatmapMalformedPoint(t *testing.T) {
	drilldown := jsonenc.NewObject().Set("bucket_seconds", 60)
	// A data point that is not a >=3-element list -> passed through unchanged.
	series := jsonenc.NewObject().Set("data", []any{"not-a-point", []any{1}})
	opt := jsonenc.NewObject().Set("series", []any{series})
	bindings := map[string]any{
		"x_unique_values": []any{"a"},
		"y_unique_values": []any{"b"},
	}
	attachDrilldownMetadata("heatmap", drilldown, bindings, opt)
	dataV, _ := series.Get("data")
	data := dataV.([]any)
	if data[0] != "not-a-point" {
		t.Errorf("malformed point should pass through unchanged, got %#v", data[0])
	}
}

func TestCov95B6_AttachDrilldownMetadata_HeatmapNoUniqueValues(t *testing.T) {
	drilldown := jsonenc.NewObject().Set("bucket_seconds", 60)
	series := jsonenc.NewObject().Set("data", []any{[]any{0, 0, 1.0}})
	opt := jsonenc.NewObject().Set("series", []any{series})
	// bindings missing x_unique_values/y_unique_values entirely -> early return.
	attachDrilldownMetadata("heatmap", drilldown, map[string]any{}, opt)
	dataV, _ := series.Get("data")
	data := dataV.([]any)
	if _, isObj := data[0].(*jsonenc.Object); isObj {
		t.Error("without unique-value bindings, data should be left unwrapped")
	}
}

func TestCov95B6_AttachDrilldownMetadata_TimeSeries_NonValueSeries(t *testing.T) {
	drilldown := jsonenc.NewObject().Set("bucket_seconds", 30)
	otherSeries := jsonenc.NewObject().Set("name", "P95").Set("data", []any{1.0, 2.0})
	opt := jsonenc.NewObject().Set("series", []any{otherSeries})
	bindings := map[string]any{
		"time":          []any{"t1", "t2"},
		"anomaly_state": []any{"normal", "outlier"},
		"anomaly_score": []any{0.1, 0.9},
	}
	attachDrilldownMetadata("anomaly_overlay", drilldown, bindings, opt)
	dataV, _ := otherSeries.Get("data")
	data := dataV.([]any)
	pt, ok := data[0].(*jsonenc.Object)
	if !ok {
		t.Fatalf("expected wrapped point, got %#v", data[0])
	}
	dd, _ := pt.Get("drilldown")
	ddObj := dd.(*jsonenc.Object)
	if _, has := ddObj.Get("_anomaly_state"); has {
		t.Error("non-Value series should not get _anomaly_state metadata")
	}
}

func TestCov95B6_AttachDrilldownMetadata_DerivedSignalOverlay_ValueSeries(t *testing.T) {
	drilldown := jsonenc.NewObject().Set("bucket_seconds", 30)
	valueSeries := jsonenc.NewObject().Set("name", "Value").Set("data", []any{10.0, 20.0})
	opt := jsonenc.NewObject().Set("series", []any{valueSeries})
	bindings := map[string]any{
		"time":            []any{"t1", "t2"},
		"anomaly_state":   []any{"normal", "outlier"},
		"anomaly_score":   []any{0.1, 0.9},
		"effective_state": []any{"normal", "outlier"},
		"rule_state":      []any{"normal", "outlier"},
		"rule_name":       []any{"", "HighCPU"},
		"rule_reason":     []any{"", "value too high"},
		"service":         []any{"web", "web"},
		"source":          []any{"metrics", "metrics"},
		"signal":          []any{"cpu", "cpu"},
		"attr_fp":         []any{"fp1", "fp1"},
	}
	attachDrilldownMetadata("derived_signal_overlay", drilldown, bindings, opt)
	dataV, _ := valueSeries.Get("data")
	data := dataV.([]any)
	pt1 := data[1].(*jsonenc.Object)
	ddV, _ := pt1.Get("drilldown")
	dd := ddV.(*jsonenc.Object)
	ruleNameV, _ := dd.Get("_rule_name")
	if ruleNameV != "HighCPU" {
		t.Errorf("_rule_name = %v, want HighCPU (attachDerivedDrilldownFields wired in)", ruleNameV)
	}
	svcV, _ := dd.Get("service")
	if svcV != "web" {
		t.Errorf("service = %v, want web", svcV)
	}
	effV, _ := dd.Get("_effective_state")
	if effV != "outlier" {
		t.Errorf("_effective_state = %v, want outlier", effV)
	}
}

func TestCov95B6_AttachDerivedDrilldownFields_MissingBindingsUseDefaults(t *testing.T) {
	dd := jsonenc.NewObject()
	// bindings missing every optional key -> each falls back to its default.
	attachDerivedDrilldownFields(dd, map[string]any{}, 0)
	cases := map[string]string{
		"_rule_state": "normal", "_rule_name": "", "_rule_reason": "", "_effective_state": "normal",
		"service": "", "source": "", "signal": "", "attr_fp": "",
	}
	for k, want := range cases {
		got, _ := dd.Get(k)
		if got != want {
			t.Errorf("%s = %v, want %q (default)", k, got, want)
		}
	}
}

func TestCov95B6_AttachDerivedDrilldownFields_IndexOutOfRangeUsesDefault(t *testing.T) {
	dd := jsonenc.NewObject()
	bindings := map[string]any{"rule_state": []any{"outlier"}}
	// idx=5 is out of range for the length-1 rule_state list -> falls back to the default.
	attachDerivedDrilldownFields(dd, bindings, 5)
	got, _ := dd.Get("_rule_state")
	if got != "normal" {
		t.Errorf("_rule_state = %v, want normal (out-of-range index default)", got)
	}
}

// ---------------------------------------------------------------------------
// renderChartFromTemplateWithNamed — the derived_signal_overlay prepareTemplateRows error path
// and custom_echarts delegation
// ---------------------------------------------------------------------------

func TestCov95B6_RenderChartFromTemplateWithNamed_CustomEchartsDelegation(t *testing.T) {
	s := &server{}
	// custom_echarts with an invalid custom_mapping_json -> error surfaces from renderCustomEcharts.
	spec := jsonenc.NewObject().Set("visual", jsonenc.NewObject().Set("custom_mapping_json", "not-json{{{"))
	_, errMsg := s.renderChartFromTemplateWithNamed("custom_echarts", []any{"a"}, []map[string]any{{"a": 1}}, spec, nil)
	if !strings.Contains(errMsg, "custom_mapping_json must be valid JSON") {
		t.Errorf("errMsg = %q, want the custom_mapping_json validation error", errMsg)
	}
}

func TestCov95B6_RenderChartFromTemplateWithNamed_RoleMapError(t *testing.T) {
	s := &server{}
	spec := jsonenc.NewObject().Set("visual", jsonenc.NewObject().Set("role_map",
		jsonenc.NewObject().Set("not_a_real_role", "x")))
	columns := []any{"x", "y", "value"}
	rows := []map[string]any{{"x": "a", "y": "b", "value": 1.0}}
	_, errMsg := s.renderChartFromTemplateWithNamed("heatmap", columns, rows, spec, nil)
	if !strings.Contains(errMsg, "Unknown role") {
		t.Errorf("errMsg = %q, want it to mention the unknown role", errMsg)
	}
}

// ---------------------------------------------------------------------------
// parseBool3
// ---------------------------------------------------------------------------

func TestCov95B6_ParseBool3(t *testing.T) {
	if got := parseBool3(nil, false, true); got != true {
		t.Errorf("not present: got %v, want default true", got)
	}
	if got := parseBool3(nil, true, true); got != true {
		t.Errorf("present but nil: got %v, want default true", got)
	}
	if got := parseBool3(true, true, false); got != true {
		t.Errorf("native bool true: got %v, want true", got)
	}
	if got := parseBool3(false, true, true); got != false {
		t.Errorf("native bool false: got %v, want false", got)
	}
	truthy := []string{"1", "true", "yes", "on", "TRUE", " On "}
	for _, s := range truthy {
		if got := parseBool3(s, true, false); got != true {
			t.Errorf("parseBool3(%q) = %v, want true", s, got)
		}
	}
	falsy := []string{"0", "false", "no", "off", "FALSE"}
	for _, s := range falsy {
		if got := parseBool3(s, true, true); got != false {
			t.Errorf("parseBool3(%q) = %v, want false", s, got)
		}
	}
	// unrecognized string -> falls back to default.
	if got := parseBool3("maybe", true, true); got != true {
		t.Errorf(`parseBool3("maybe") = %v, want default true`, got)
	}
}

// ---------------------------------------------------------------------------
// applyChartSpecVisualOverrides — additional branches (custom_echarts short-circuit, nil visual,
// zoom slider, non-object option)
// ---------------------------------------------------------------------------

func TestCov95B6_ApplyChartSpecVisualOverrides_CustomEchartsShortCircuit(t *testing.T) {
	opt := jsonenc.NewObject()
	spec := jsonenc.NewObject().Set("visual", jsonenc.NewObject().Set("legend_show", false))
	got := applyChartSpecVisualOverrides("custom_echarts", opt, spec)
	if got != any(opt) {
		t.Error("custom_echarts should short-circuit and return option unchanged")
	}
}

func TestCov95B6_ApplyChartSpecVisualOverrides_NonObjectOption(t *testing.T) {
	got := applyChartSpecVisualOverrides("heatmap", "not-an-object", nil)
	if got != "not-an-object" {
		t.Errorf("non-object option should pass through unchanged, got %#v", got)
	}
}

func TestCov95B6_ApplyChartSpecVisualOverrides_NilSpec(t *testing.T) {
	opt := jsonenc.NewObject()
	got := applyChartSpecVisualOverrides("heatmap", opt, nil)
	if got != any(opt) {
		t.Error("nil spec should return option unchanged")
	}
}

func TestCov95B6_ApplyChartSpecVisualOverrides_NoVisualKey(t *testing.T) {
	opt := jsonenc.NewObject()
	spec := jsonenc.NewObject() // no "visual" key at all
	got := applyChartSpecVisualOverrides("heatmap", opt, spec)
	if got != any(opt) {
		t.Error("spec without a visual object should return option unchanged")
	}
}

func TestCov95B6_ApplyChartSpecVisualOverrides_ZoomSliderAndValueColor(t *testing.T) {
	series := jsonenc.NewObject().Set("name", "Value").Set("type", "line")
	opt := jsonenc.NewObject().Set("series", []any{series})
	visual := jsonenc.NewObject().
		Set("legend_show", false).
		Set("zoom_inside", false).
		Set("zoom_slider", true).
		Set("zoom_start_pct", 10).
		Set("zoom_end_pct", 90).
		Set("smooth_line", false).
		Set("value_color", "#ff0000")
	spec := jsonenc.NewObject().Set("visual", visual)
	got := applyChartSpecVisualOverrides("heatmap", opt, spec)
	oo := got.(*jsonenc.Object)
	dzV, _ := oo.Get("dataZoom")
	dz := dzV.([]any)
	if len(dz) != 1 {
		t.Fatalf("want exactly the slider entry (inside disabled), got %d entries", len(dz))
	}
	slider := dz[0].(*jsonenc.Object)
	startV, _ := slider.Get("start")
	if startV != 10 {
		t.Errorf("zoom slider start = %v, want 10", startV)
	}
	so := series
	smoothV, _ := so.Get("smooth")
	if smoothV != false {
		t.Errorf("smooth = %v, want false", smoothV)
	}
	lsV, _ := so.Get("lineStyle")
	ls := lsV.(*jsonenc.Object)
	colorV, _ := ls.Get("color")
	if colorV != "#ff0000" {
		t.Errorf("lineStyle.color = %v, want #ff0000", colorV)
	}
}

func TestCov95B6_ApplyChartSpecVisualOverrides_NoZoomKeepsExistingDataZoom(t *testing.T) {
	existing := []any{jsonenc.NewObject().Set("type", "inside")}
	opt := jsonenc.NewObject().Set("dataZoom", existing)
	visual := jsonenc.NewObject().Set("zoom_inside", false).Set("zoom_slider", false)
	spec := jsonenc.NewObject().Set("visual", visual)
	got := applyChartSpecVisualOverrides("heatmap", opt, spec)
	oo := got.(*jsonenc.Object)
	dzV, _ := oo.Get("dataZoom")
	dz := dzV.([]any)
	if len(dz) != 1 {
		t.Errorf("both zoom types disabled: want existing dataZoom preserved, got %#v", dz)
	}
}

func TestCov95B6_ApplyChartSpecVisualOverrides_NonLineSeriesNoSmooth(t *testing.T) {
	series := jsonenc.NewObject().Set("name", "Value").Set("type", "bar")
	opt := jsonenc.NewObject().Set("series", []any{series})
	visual := jsonenc.NewObject().Set("smooth_line", true)
	spec := jsonenc.NewObject().Set("visual", visual)
	applyChartSpecVisualOverrides("heatmap", opt, spec)
	if _, has := series.Get("smooth"); has {
		t.Error("a non-line series should not get a smooth key set")
	}
}

// ---------------------------------------------------------------------------
// renderChartFromTemplateWithNamed — full success paths (deepSubstitute + attachDrilldownMetadata
// + default backgroundColor/textStyle injection), for a simple template and for
// derived_signal_overlay (which additionally routes through s.prepareTemplateRows).
// ---------------------------------------------------------------------------

func TestCov95B6_RenderChartFromTemplateWithNamed_SimpleTemplateSuccess(t *testing.T) {
	s := &server{}
	columns := []any{"time", "value", "p95", "p99"}
	rows := []map[string]any{
		{"time": "2024-01-15T10:00:00Z", "value": 1.0, "p95": 2.0, "p99": 3.0},
		{"time": "2024-01-15T10:01:00Z", "value": 1.5, "p95": 2.5, "p99": 3.5},
	}
	option, errMsg := s.renderChartFromTemplateWithNamed("time_series_percentiles", columns, rows, nil, nil)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	opt, ok := option.(*jsonenc.Object)
	if !ok {
		t.Fatalf("option is not an object: %#v", option)
	}
	bg, has := opt.Get("backgroundColor")
	if !has || bg != "transparent" {
		t.Errorf("backgroundColor = %v (has=%v), want transparent", bg, has)
	}
	if _, has := opt.Get("textStyle"); !has {
		t.Error("textStyle default should be injected")
	}
	seriesV, _ := opt.Get("series")
	series, ok := seriesV.([]any)
	if !ok || len(series) == 0 {
		t.Fatal("expected a non-empty series list")
	}
}

func TestCov95B6_RenderChartFromTemplateWithNamed_ExtractBindingsErrorPropagates(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	// derived_signal_overlay needs 12-16 columns; supply a non-numeric baseline_mean cell so
	// extractBindings (via extractDerivedSignalBindings) returns an error that must be surfaced
	// through publicDashboardQueryError.
	columns := []any{"time", "service", "source", "signal", "attr_fp", "value", "sample_count",
		"baseline_mean", "baseline_lower", "baseline_upper", "anomaly_state", "anomaly_score"}
	rows := []map[string]any{
		{"time": "2024-01-15T10:00:00Z", "service": "web", "source": "src", "signal": "sig", "attr_fp": "fp1",
			"value": 1.0, "sample_count": 5, "baseline_mean": "not-a-number", "baseline_lower": 0.5,
			"baseline_upper": 1.5, "anomaly_state": "normal", "anomaly_score": 0.0},
	}
	_, errMsg := s.renderChartFromTemplateWithNamed("derived_signal_overlay", columns, rows, nil, nil)
	if errMsg == "" {
		t.Fatal("want an error from the non-numeric baseline_mean cell, got none")
	}
}

func TestCov95B6_RenderChartFromTemplateWithNamed_DerivedSignalOverlaySuccess(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	columns := []any{"time", "service", "source", "signal", "attr_fp", "value", "sample_count",
		"baseline_mean", "baseline_lower", "baseline_upper", "anomaly_state", "anomaly_score"}
	rows := []map[string]any{
		{"time": "2024-01-15T10:00:00Z", "service": "web", "source": "src", "signal": "sig", "attr_fp": "fp1",
			"value": 12.0, "sample_count": 5, "baseline_mean": 10.0, "baseline_lower": 8.0,
			"baseline_upper": 14.0, "anomaly_state": "normal", "anomaly_score": 0.0},
		{"time": "2024-01-15T10:01:00Z", "service": "web", "source": "src", "signal": "sig", "attr_fp": "fp1",
			"value": 30.0, "sample_count": 5, "baseline_mean": 10.0, "baseline_lower": 8.0,
			"baseline_upper": 14.0, "anomaly_state": "outlier", "anomaly_score": 1.0},
	}
	option, errMsg := s.renderChartFromTemplateWithNamed("derived_signal_overlay", columns, rows, nil, nil)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	opt, ok := option.(*jsonenc.Object)
	if !ok {
		t.Fatalf("option is not an object: %#v", option)
	}
	seriesV, _ := opt.Get("series")
	series := seriesV.([]any)
	var valueSeries *jsonenc.Object
	for _, se := range series {
		seo := se.(*jsonenc.Object)
		if nameV, _ := seo.Get("name"); nameV == "Value" {
			valueSeries = seo
			break
		}
	}
	if valueSeries == nil {
		t.Fatal("expected a 'Value' series in the rendered option")
	}
	dataV, _ := valueSeries.Get("data")
	data := dataV.([]any)
	if len(data) != 2 {
		t.Fatalf("want 2 data points, got %d", len(data))
	}
	pt := data[1].(*jsonenc.Object)
	ddV, _ := pt.Get("drilldown")
	dd := ddV.(*jsonenc.Object)
	if stateV, _ := dd.Get("_anomaly_state"); stateV != "outlier" {
		t.Errorf("_anomaly_state = %v, want outlier", stateV)
	}
}
