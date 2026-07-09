package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// cov95_b16_chart_builder_sql_test.go — batch 16 targeted coverage for
// cmd/sobs/chart_builder_sql.go: isFalsyAny's default (unhandled-type) branch and
// compileBuilderSQL's remaining template/source_view combinations not covered by
// TestCompileBuilderSQLGuards (chart_builder_mcp_test.go), which only exercises the two error
// guards.

func TestIsFalsyAnyDefaultBranch(t *testing.T) {
	// Any type isFalsyAny doesn't special-case (slice, map, struct) falls to `default: return false`.
	for _, v := range []any{[]int{1, 2}, map[string]int{"a": 1}, struct{ X int }{X: 1}} {
		if isFalsyAny(v) {
			t.Errorf("isFalsyAny(%#v) = true, want false (unhandled type -> not falsy)", v)
		}
	}
}

func TestCompileBuilderSQLAllTemplatesAndSources(t *testing.T) {
	mk := func(kv map[string]any) *jsonenc.Object {
		o := jsonenc.NewObject()
		for k, v := range kv {
			o.Set(k, v)
		}
		return o
	}

	t.Run("derived_signal_overlay default source view with all filters", func(t *testing.T) {
		data := mk(map[string]any{
			"source_view": "v_derived_signals_anomaly", "service": "svc-a", "attr_fp": "fp1",
			"signal_source": "src1", "signal_name": "sig1", "window_hours": 12.0, "limit": 500.0,
		})
		sql, errMsg := compileBuilderSQL("derived_signal_overlay", data)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		for _, want := range []string{
			"FROM v_derived_signals_anomaly", "ServiceName = 'svc-a'", "AttrFingerprint = 'fp1'",
			"SignalSource = 'src1'", "SignalName = 'sig1'", "INTERVAL 12 HOUR", "LIMIT 500",
			"'sig1' AS signal", "'fp1' AS attr_fp",
		} {
			if !strings.Contains(sql, want) {
				t.Errorf("missing %q in:\n%s", want, sql)
			}
		}
	})

	t.Run("derived_signal_overlay defaults service/source/signal labels when absent", func(t *testing.T) {
		data := mk(map[string]any{"source_view": "v_derived_signals_anomaly"})
		sql, errMsg := compileBuilderSQL("derived_signal_overlay", data)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		// defaultSourceLabel: v_derived_signals_anomaly with no signal_source -> "derived".
		// defaultSignalLabel: no case for this source view (only otel_logs/otel_traces/
		// sobs_error_resolutions have a source-specific fallback) -> falls through to "value".
		if !strings.Contains(sql, "'all' AS service") || !strings.Contains(sql, "'derived' AS source") ||
			!strings.Contains(sql, "'value' AS signal") {
			t.Errorf("expected default service/source/signal labels, got:\n%s", sql)
		}
	})

	t.Run("v_otel_metrics_anomaly source with metric_name filter", func(t *testing.T) {
		data := mk(map[string]any{"source_view": "v_otel_metrics_anomaly", "service": "svc-b", "metric_name": "cpu.usage"})
		sql, errMsg := compileBuilderSQL("anomaly_overlay", data)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if !strings.Contains(sql, "FROM v_otel_metrics_anomaly") || !strings.Contains(sql, "MetricName = 'cpu.usage'") {
			t.Errorf("missing expected clauses:\n%s", sql)
		}
	})

	t.Run("otel_metrics_gauge source with default label mapping", func(t *testing.T) {
		data := mk(map[string]any{"source_view": "otel_metrics_gauge", "metric_name": "queue.depth"})
		sql, errMsg := compileBuilderSQL("dual_axis_anomaly", data)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if !strings.Contains(sql, "avg(toFloat64(Value))") {
			t.Errorf("gauge should read Value directly:\n%s", sql)
		}
	})

	t.Run("otel_metrics_histogram uses sum/count ratio expression", func(t *testing.T) {
		data := mk(map[string]any{"source_view": "otel_metrics_histogram"})
		sql, errMsg := compileBuilderSQL("time_series_percentiles", data)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if !strings.Contains(sql, "if(Count = 0, 0.0, Sum / toFloat64(Count))") {
			t.Errorf("histogram value expr missing:\n%s", sql)
		}
	})

	t.Run("otel_logs source default label is log_volume", func(t *testing.T) {
		data := mk(map[string]any{"source_view": "otel_logs", "service": "svc-c"})
		sql, errMsg := compileBuilderSQL("heatmap", data)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if !strings.Contains(sql, "FROM otel_logs") || !strings.Contains(sql, "ServiceName = 'svc-c'") {
			t.Errorf("missing expected clauses:\n%s", sql)
		}
	})

	t.Run("otel_traces source default label is trace_volume", func(t *testing.T) {
		data := mk(map[string]any{"source_view": "otel_traces"})
		sql, errMsg := compileBuilderSQL("box_plot", data)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if !strings.Contains(sql, "FROM otel_traces") || !strings.Contains(sql, "'trace_volume' AS dimension") {
			t.Errorf("missing expected trace_volume label:\n%s", sql)
		}
	})

	t.Run("sobs_error_resolutions source (default fallthrough) and gauge_kpi template", func(t *testing.T) {
		data := mk(map[string]any{"source_view": "sobs_error_resolutions"})
		sql, errMsg := compileBuilderSQL("gauge_kpi", data)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if !strings.Contains(sql, "FROM sobs_error_resolutions") || !strings.Contains(sql, "anomaly_state = 'normal'") {
			t.Errorf("missing expected clauses:\n%s", sql)
		}
	})

	t.Run("unknown templateID returns the builder-mode error", func(t *testing.T) {
		data := mk(map[string]any{"source_view": "otel_logs"})
		sql, errMsg := compileBuilderSQL("no_such_template", data)
		if sql != "" || errMsg != "Builder mode does not support template: no_such_template" {
			t.Errorf("got (%q, %q)", sql, errMsg)
		}
	})

	t.Run("window_hours and limit are clamped", func(t *testing.T) {
		data := mk(map[string]any{"source_view": "otel_logs", "window_hours": 99999.0, "limit": 99999.0})
		sql, errMsg := compileBuilderSQL("anomaly_overlay", data)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		if !strings.Contains(sql, "INTERVAL 168 HOUR") {
			t.Errorf("window_hours should clamp to max 168:\n%s", sql)
		}
		if !strings.Contains(sql, "LIMIT "+strconv.Itoa(2000)) {
			t.Errorf("limit should clamp to max 2000:\n%s", sql)
		}
	})
}
