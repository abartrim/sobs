package main

// coverage_chart_binding_gaps_test.go — direct unit tests for chart_render_binding.go's pure
// helper functions. The golden corpus only ever renders charts with well-formed, in-range,
// all-numeric fixture data, so the validation/error branches (unknown template, column-count
// caps, role-map errors, out-of-range indices) and several data-shape branches (non-numeric
// heatmap values, missing box-plot dimension, unrecognized anomaly state) were never reached.
// All of this is testable directly — no HTTP layer, no store — since these are plain functions
// of columns/rows/bindings.

import (
	"strings"
	"testing"
)

func TestRenderChartFromTemplate_UnknownTemplate(t *testing.T) {
	s := &server{}
	_, errMsg := s.renderChartFromTemplate("no_such_template", []any{"a"}, nil, nil)
	if !strings.Contains(errMsg, "Unknown template") {
		t.Errorf("errMsg = %q, want it to mention Unknown template", errMsg)
	}
}

func TestRenderChartFromTemplate_TooManyColumns(t *testing.T) {
	s := &server{}
	columns := []any{"x", "y", "value", "extra"}
	rows := []map[string]any{{"x": "a", "y": "b", "value": 1.0, "extra": "z"}}
	_, errMsg := s.renderChartFromTemplate("heatmap", columns, rows, nil)
	if !strings.Contains(errMsg, "accepts maximum") {
		t.Errorf("errMsg = %q, want it to mention the max-column cap", errMsg)
	}
}

func TestRenderChartFromTemplate_TooFewColumns(t *testing.T) {
	s := &server{}
	columns := []any{"x", "y"}
	rows := []map[string]any{{"x": "a", "y": "b"}}
	_, errMsg := s.renderChartFromTemplate("heatmap", columns, rows, nil)
	if !strings.Contains(errMsg, "requires at least") {
		t.Errorf("errMsg = %q, want it to mention the min-column requirement", errMsg)
	}
}

func TestRenderChartFromTemplate_NoRows(t *testing.T) {
	s := &server{}
	_, errMsg := s.renderChartFromTemplate("heatmap", []any{"x", "y", "value"}, nil, nil)
	if errMsg != "" {
		t.Errorf("errMsg = %q, want empty (no-data placeholder, not an error)", errMsg)
	}
}

// TestExtractBindings_OutOfRangeRoleIndexSkipped covers the bounds check that drops a role
// whose index falls outside the columns slice (extractBindings never crashes on it).
func TestExtractBindings_OutOfRangeRoleIndexSkipped(t *testing.T) {
	columns := []any{"a", "b"}
	rows := []map[string]any{{"a": 1, "b": 2}}
	bindings, err := extractBindings("heatmap", columns, rows, map[string]int{"x_category": 99})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := bindings["x_category"]; present {
		t.Errorf("bindings[x_category] should be absent for an out-of-range role index, got %v", bindings["x_category"])
	}
}

// TestExtractBindings_Heatmap_NonNumericValues covers the "no numeric values found" fallback
// (value_min=0, value_max=1) as well as the min/max tracking branches when values ARE numeric.
func TestExtractBindings_Heatmap_NonNumericValues(t *testing.T) {
	columns := []any{"x", "y", "value"}
	rows := []map[string]any{
		{"x": "a", "y": "p", "value": "not-a-number"},
		{"x": "b", "y": "q", "value": "also-not-a-number"},
	}
	roleIdx := map[string]int{"x_category": 0, "y_category": 1, "value": 2}
	bindings, err := extractBindings("heatmap", columns, rows, roleIdx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bindings["value_min"] != 0 || bindings["value_max"] != 1 {
		t.Errorf("all-non-numeric values: value_min/max = %v/%v, want 0/1", bindings["value_min"], bindings["value_max"])
	}
}

func TestExtractBindings_Heatmap_NumericRange(t *testing.T) {
	columns := []any{"x", "y", "value"}
	rows := []map[string]any{
		{"x": "a", "y": "p", "value": 5.0},
		{"x": "b", "y": "q", "value": 1.0},
		{"x": "c", "y": "r", "value": 9.0},
	}
	roleIdx := map[string]int{"x_category": 0, "y_category": 1, "value": 2}
	bindings, err := extractBindings("heatmap", columns, rows, roleIdx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bindings["value_min"] != 1.0 || bindings["value_max"] != 9.0 {
		t.Errorf("value_min/max = %v/%v, want 1/9", bindings["value_min"], bindings["value_max"])
	}
}

// TestExtractBindings_BoxPlot_NoDimension covers the "no 'dimension' binding" fallback to an
// empty dimension_values list.
func TestExtractBindings_BoxPlot_NoDimension(t *testing.T) {
	columns := []any{"min", "q1", "median", "q3", "max"}
	rows := []map[string]any{{"min": 1.0, "q1": 2.0, "median": 3.0, "q3": 4.0, "max": 5.0}}
	roleIdx := map[string]int{"min": 0, "q1": 1, "median": 2, "q3": 3, "max": 4}
	bindings, err := extractBindings("box_plot", columns, rows, roleIdx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dv, ok := bindings["dimension_values"].([]any)
	if !ok || len(dv) != 0 {
		t.Errorf("dimension_values = %#v, want an empty []any", bindings["dimension_values"])
	}
	bp, ok := bindings["boxplot_data"].([]any)
	if !ok || len(bp) != 1 {
		t.Fatalf("boxplot_data = %#v, want 1 row", bindings["boxplot_data"])
	}
}

// TestExtractBindings_AnomalyState_UnknownFallsBack covers the "unrecognized state" default
// color/size branch (distinct from the known outlier/warning/normal states).
func TestExtractBindings_AnomalyState_UnknownFallsBack(t *testing.T) {
	columns := []any{"time", "value", "baseline_mean", "baseline_lower", "baseline_upper", "anomaly_state"}
	rows := []map[string]any{
		{"time": "t1", "value": 1.0, "baseline_mean": 1.0, "baseline_lower": 0.5, "baseline_upper": 1.5, "anomaly_state": "weird_unknown_state"},
	}
	roleIdx := map[string]int{"time": 0, "value": 1, "baseline_mean": 2, "baseline_lower": 3, "baseline_upper": 4, "anomaly_state": 5}
	bindings, err := extractBindings("anomaly_overlay", columns, rows, roleIdx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	colors, _ := bindings["anomaly_point_color"].([]any)
	sizes, _ := bindings["anomaly_symbol_size"].([]any)
	if len(colors) != 1 || colors[0] != "#0d6efd" {
		t.Errorf("anomaly_point_color = %#v, want default #0d6efd", colors)
	}
	if len(sizes) != 1 || sizes[0] != 4 {
		t.Errorf("anomaly_symbol_size = %#v, want default 4", sizes)
	}
}

// TestExtractBindings_DerivedSignalOverlay_ErrorPropagates covers the extractBindings ->
// extractDerivedSignalBindings error-propagation branch: a non-numeric baseline cell makes
// Python's float() raise, which must surface as an error here too (not silently coerce to 0).
func TestExtractBindings_DerivedSignalOverlay_ErrorPropagates(t *testing.T) {
	columns := []any{"time", "value", "baseline_mean", "baseline_lower", "baseline_upper"}
	rows := []map[string]any{
		{"time": "t1", "value": 1.0, "baseline_mean": "not-a-number", "baseline_lower": 0.5, "baseline_upper": 1.5},
	}
	roleIdx := map[string]int{"time": 0, "value": 1, "baseline_mean": 2, "baseline_lower": 3, "baseline_upper": 4}
	_, err := extractBindings("derived_signal_overlay", columns, rows, roleIdx)
	if err == nil {
		t.Fatal("want an error for a non-numeric baseline_mean cell, got nil")
	}
}

// TestSortedUniqueAny covers the numeric-vs-string sort-precedence branches: numbers sort before
// strings, and within a type ties break by numeric or lexicographic order.
func TestSortedUniqueAny(t *testing.T) {
	in := []any{"b", 3.0, "a", 1.0, 3.0, 2.0}
	got := sortedUniqueAny(in)
	want := []any{1.0, 2.0, 3.0, "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("sortedUniqueAny(%v) = %v, want %v", in, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sortedUniqueAny(%v)[%d] = %v, want %v", in, i, got[i], want[i])
		}
	}
}

func TestAnyEqual_Numeric(t *testing.T) {
	if !anyEqual(1.0, 1) {
		t.Errorf("anyEqual(1.0, 1) = false, want true (numeric cross-type equality)")
	}
	if anyEqual(1.0, 2) {
		t.Errorf("anyEqual(1.0, 2) = true, want false")
	}
	if !anyEqual("x", "x") {
		t.Errorf(`anyEqual("x", "x") = false, want true`)
	}
}
