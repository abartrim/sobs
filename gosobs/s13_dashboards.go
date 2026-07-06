package main

// Port of app.py lines 19228-22533:
//   Custom Dashboards (Template-driven eCharts)
//   - CHART_TEMPLATES catalog + chart spec compile/normalize/validate helpers
//   - Builder-mode SQL compilation, eCharts option rendering + drilldown metadata
//   - Custom ECharts (raw SQL + mapping JSON + option JSON) rendering
//   - Dashboard CRUD pages, chart CRUD/clone/export/import
//   - Spec APIs: templates/options/compile/dry-run/validate/render, AI build
//   - Help-route registry and the shared soft-delete helper
//
// PORT-NOTE: file-local helpers are prefixed "chart" to avoid clashes with
// other sections' generic coercion helpers.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func init() {
	registerRoute("GET", "/api/dashboards/list", requireBasicAuth(apiDashboardsList))
	registerRoute("POST", "/api/query/add-to-dashboard", requireBasicAuth(apiQueryAddToDashboard))
	registerRoute("GET", "/dashboards", requireBasicAuth(listDashboards))
	registerRoute("GET", "/dashboards/new", requireBasicAuth(newDashboardForm))
	registerRoute("POST", "/dashboards", requireBasicAuth(createDashboard))
	registerRoute("GET", "/dashboards/{dashboard_id}", requireBasicAuth(viewCustomDashboard))
	registerRoute("POST", "/dashboards/{dashboard_id}/delete", requireBasicAuth(deleteDashboard))
	registerRoute("POST", "/dashboards/{dashboard_id}/charts", requireBasicAuth(addChart))
	registerRoute("POST", "/dashboards/{dashboard_id}/charts/{chart_id}/edit", requireBasicAuth(editChart))
	registerRoute("POST", "/dashboards/{dashboard_id}/charts/{chart_id}/clone", requireBasicAuth(cloneChart))
	registerRoute("POST", "/dashboards/{dashboard_id}/charts/{chart_id}/delete", requireBasicAuth(removeChart))
	registerRoute("POST", "/api/dashboards/query", requireBasicAuth(executeChartQuery))
	registerRoute("GET", "/api/dashboards/spec/templates", requireBasicAuth(listChartSpecTemplates))
	registerRoute("GET", "/api/dashboards/spec/options", requireBasicAuth(chartSpecOptionsApi))
	registerRoute("POST", "/api/dashboards/spec/compile", requireBasicAuth(compileChartSpecApi))
	registerRoute("POST", "/api/dashboards/spec/dry-run", requireBasicAuth(dryRunChartSpecApi))
	registerRoute("POST", "/api/dashboards/spec/validate", requireBasicAuth(validateChartSpecApi))
	registerRoute("POST", "/api/dashboards/spec/render", requireBasicAuth(renderChartSpecApi))
	registerRoute("POST", "/api/dashboards/render", requireBasicAuth(renderChart))
	registerRoute("POST", "/api/dashboards/spec/ai-build", requireBasicAuth(aiBuildChartSpec))
	registerRoute("GET", "/api/dashboards/{dashboard_id}/charts/{chart_id}/export", requireBasicAuth(exportChart))
	registerRoute("POST", "/api/dashboards/{dashboard_id}/charts/import", requireBasicAuth(importChart))

	for _, entry := range helpRouteRegistry {
		registerHelpRoute(entry[0], entry[1], entry[2])
	}
}

// ---------------------------------------------------------------------------
// Custom Dashboards (Template-driven eCharts)
// ---------------------------------------------------------------------------

// chartTemplates mirrors CHART_TEMPLATES: define structure, column roles, and
// eCharts rendering.
var chartTemplates = map[string]map[string]any{
	"time_series_percentiles": {
		"id":          "time_series_percentiles",
		"name":        "Time Series with Normal Range",
		"description": "Show metric with percentile bands for anomaly detection",
		"icon":        "bi-graph-up",
		"query_shape": "Columns: time, value, p95, p99",
		"sample_sql": "SELECT\n" +
			"  toStartOfMinute(Timestamp) AS time,\n" +
			"  avg(Duration) AS value,\n" +
			"  quantile(0.95)(Duration) AS p95,\n" +
			"  quantile(0.99)(Duration) AS p99\n" +
			"FROM otel_traces\n" +
			"GROUP BY time\n" +
			"ORDER BY time",
		"drilldown": map[string]any{
			"target":         "traces",
			"label":          "Open source traces",
			"bucket_seconds": 60,
			"time_axis":      "x",
		},
		"min_columns":  4,
		"max_columns":  4,
		"column_roles": map[string]any{"time": 0, "value": 1, "p95": 2, "p99": 3},
		"echarts_option_template": map[string]any{
			"tooltip": map[string]any{"trigger": "axis"},
			"legend":  map[string]any{"data": []any{"Metric", "p95 Band", "p99 Band"}, "bottom": 0},
			"xAxis":   map[string]any{"type": "time", "data": "{{time}}"},
			"yAxis":   map[string]any{"type": "value"},
			"grid":    map[string]any{"left": "3%", "right": "4%", "bottom": "15%", "containLabel": true},
			"series": []any{
				map[string]any{
					"name":      "Metric",
					"type":      "line",
					"data":      "{{value}}",
					"lineStyle": map[string]any{"color": "#0d6efd"},
					"symbol":    "none",
				},
				map[string]any{
					"name":      "p95 Band",
					"type":      "line",
					"data":      "{{p95}}",
					"lineStyle": map[string]any{"type": "dashed", "color": "#ffc107"},
					"symbol":    "none",
				},
				map[string]any{
					"name":      "p99 Band",
					"type":      "line",
					"data":      "{{p99}}",
					"lineStyle": map[string]any{"type": "dashed", "color": "#dc3545"},
					"symbol":    "none",
					"areaStyle": map[string]any{"color": "rgba(220, 53, 69, 0.1)"},
				},
			},
		},
	},
	"heatmap": {
		"id":          "heatmap",
		"name":        "Heatmap",
		"description": "2D heatmap for correlating errors across dimensions",
		"icon":        "bi-fire",
		"query_shape": "Columns: x category, y time bucket, numeric value",
		"sample_sql": "SELECT\n" +
			"  ServiceName AS x_category,\n" +
			"  toStartOfFiveMinutes(Timestamp) AS y_category,\n" +
			"  round(100.0 * countIf(StatusCode = 'STATUS_CODE_ERROR') / count(), 2) AS value\n" +
			"FROM otel_traces\n" +
			"GROUP BY ServiceName, y_category\n" +
			"ORDER BY ServiceName, y_category",
		"drilldown": map[string]any{
			"target":         "traces",
			"label":          "Open source traces",
			"bucket_seconds": 300,
			"time_axis":      "y",
			"service_axis":   "x",
		},
		"min_columns":  3,
		"max_columns":  3,
		"column_roles": map[string]any{"x_category": 0, "y_category": 1, "value": 2},
		"echarts_option_template": map[string]any{
			"tooltip": map[string]any{"trigger": "item", "formatter": "{b}: {c}"},
			"xAxis":   map[string]any{"type": "category", "data": "{{x_unique_values}}"},
			"yAxis":   map[string]any{"type": "category", "data": "{{y_unique_values}}"},
			"visualMap": map[string]any{
				"min":     "{{value_min}}",
				"max":     "{{value_max}}",
				"inRange": map[string]any{"color": []any{"#ebedf0", "#c6e48b", "#7bc96f", "#239a3b", "#196127"}},
				"text":    []any{"High", "Low"},
				"bottom":  0,
			},
			"grid": map[string]any{"left": "15%", "right": "10%", "bottom": "15%", "top": "10%", "containLabel": true},
			"series": []any{
				map[string]any{
					"type":     "heatmap",
					"data":     "{{heatmap_data}}",
					"emphasis": map[string]any{"itemStyle": map[string]any{"borderColor": "#fff", "borderWidth": 2}},
				},
			},
		},
	},
	"box_plot": {
		"id":          "box_plot",
		"name":        "Distribution Box Plot",
		"description": "Show distribution, quartiles, and outliers",
		"icon":        "bi-boxes",
		"query_shape": "Columns: dimension, min, q1, median, q3, max",
		"sample_sql": "SELECT\n" +
			"  HTTPMethod AS dimension,\n" +
			"  min(Duration) AS min,\n" +
			"  quantile(0.25)(Duration) AS q1,\n" +
			"  quantile(0.5)(Duration) AS median,\n" +
			"  quantile(0.75)(Duration) AS q3,\n" +
			"  max(Duration) AS max\n" +
			"FROM otel_traces\n" +
			"GROUP BY HTTPMethod\n" +
			"ORDER BY median DESC",
		"drilldown": map[string]any{
			"target": "traces",
			"label":  "Open traces view",
		},
		"min_columns":  6,
		"max_columns":  6,
		"column_roles": map[string]any{"dimension": 0, "min": 1, "q1": 2, "median": 3, "q3": 4, "max": 5},
		"echarts_option_template": map[string]any{
			"tooltip": map[string]any{"trigger": "item"},
			"xAxis":   map[string]any{"type": "category", "data": "{{dimension_values}}", "nameGap": 30},
			"yAxis":   map[string]any{"type": "value", "name": "Value"},
			"grid":    map[string]any{"left": "10%", "right": "10%", "bottom": "15%", "containLabel": true},
			"series": []any{
				map[string]any{
					"type":      "boxplot",
					"data":      "{{boxplot_data}}",
					"itemStyle": map[string]any{"color": "#0d6efd", "borderColor": "#0d6efd"},
				},
			},
		},
	},
	"dual_axis_anomaly": {
		"id":          "dual_axis_anomaly",
		"name":        "Metric + Anomaly Score",
		"description": "Compare metric vs anomaly detection signal on dual axes",
		"icon":        "bi-graph-up-arrow",
		"query_shape": "Columns: time, metric, anomaly_score",
		"sample_sql": "SELECT\n" +
			"  time,\n" +
			"  value AS metric,\n" +
			"  anomaly_score\n" +
			"FROM v_otel_metrics_anomaly\n" +
			"WHERE ServiceName = 'my-service'\n" +
			"  AND MetricName = 'my.metric'\n" +
			"  AND time >= now() - INTERVAL 1 HOUR\n" +
			"ORDER BY time",
		"drilldown": map[string]any{
			"target":         "logs",
			"label":          "Open source logs",
			"bucket_seconds": 60,
			"time_axis":      "x",
			"extra":          map[string]any{"analyze": "1", "stats": "1"},
		},
		"min_columns":  3,
		"max_columns":  3,
		"column_roles": map[string]any{"time": 0, "metric": 1, "anomaly_score": 2},
		"echarts_option_template": map[string]any{
			"tooltip": map[string]any{"trigger": "axis"},
			"legend":  map[string]any{"data": []any{"Metric", "Anomaly Score"}, "bottom": 0},
			"xAxis":   map[string]any{"type": "time", "data": "{{time}}"},
			"yAxis": []any{
				map[string]any{
					"type":     "value",
					"name":     "Metric",
					"position": "left",
					"axisLine": map[string]any{"lineStyle": map[string]any{"color": "#0d6efd"}},
				},
				map[string]any{
					"type":     "value",
					"name":     "Anomaly Score",
					"position": "right",
					"axisLine": map[string]any{"lineStyle": map[string]any{"color": "#dc3545"}},
				},
			},
			"grid": map[string]any{"left": "3%", "right": "4%", "bottom": "15%", "containLabel": true},
			"series": []any{
				map[string]any{
					"name":       "Metric",
					"type":       "line",
					"data":       "{{metric}}",
					"yAxisIndex": 0,
					"lineStyle":  map[string]any{"color": "#0d6efd"},
					"symbol":     "none",
				},
				map[string]any{
					"name":       "Anomaly Score",
					"type":       "bar",
					"data":       "{{anomaly_score}}",
					"yAxisIndex": 1,
					"itemStyle":  map[string]any{"color": "rgba(220, 53, 69, 0.5)"},
				},
			},
		},
	},
	"anomaly_overlay": {
		"id":          "anomaly_overlay",
		"name":        "Anomaly Overlay",
		"description": "Metric with baseline band and per-point anomaly state markers (normal/warning/outlier)",
		"icon":        "bi-activity",
		"query_shape": "Columns: time, value, baseline_mean, baseline_lower, baseline_upper, anomaly_state",
		"sample_sql": "SELECT\n" +
			"  time,\n" +
			"  value,\n" +
			"  baseline_mean,\n" +
			"  baseline_lower,\n" +
			"  baseline_upper,\n" +
			"  anomaly_state\n" +
			"FROM v_otel_metrics_anomaly\n" +
			"WHERE ServiceName = 'my-service'\n" +
			"  AND MetricName = 'my.metric'\n" +
			"  AND time >= now() - INTERVAL 6 HOUR\n" +
			"ORDER BY time",
		"drilldown": map[string]any{
			"target":         "metrics",
			"label":          "Open anomaly details",
			"bucket_seconds": 60,
			"time_axis":      "x",
		},
		"min_columns": 6,
		"max_columns": 6,
		"column_roles": map[string]any{
			"time":           0,
			"value":          1,
			"baseline_mean":  2,
			"baseline_lower": 3,
			"baseline_upper": 4,
			"anomaly_state":  5,
		},
		"echarts_option_template": map[string]any{
			"tooltip": map[string]any{"trigger": "axis"},
			"legend":  map[string]any{"data": []any{"Value", "Baseline", "Normal Band"}, "bottom": 0},
			"xAxis":   map[string]any{"type": "time", "data": "{{time}}"},
			"yAxis":   map[string]any{"type": "value"},
			"grid":    map[string]any{"left": "3%", "right": "4%", "bottom": "15%", "containLabel": true},
			"series": []any{
				map[string]any{
					"name":      "Normal Band",
					"type":      "line",
					"data":      "{{baseline_upper}}",
					"lineStyle": map[string]any{"opacity": 0},
					"areaStyle": map[string]any{"color": "rgba(13, 110, 253, 0.08)"},
					"symbol":    "none",
					"stack":     "band",
				},
				map[string]any{
					"name":      "Baseline",
					"type":      "line",
					"data":      "{{baseline_mean}}",
					"lineStyle": map[string]any{"type": "dashed", "color": "#6c757d"},
					"symbol":    "none",
				},
				map[string]any{
					"name":       "Value",
					"type":       "line",
					"data":       "{{value}}",
					"lineStyle":  map[string]any{"color": "#0d6efd"},
					"symbol":     "circle",
					"symbolSize": "{{anomaly_symbol_size}}",
					"itemStyle":  map[string]any{"color": "{{anomaly_point_color}}"},
				},
			},
		},
	},
	"derived_signal_overlay": {
		"id":          "derived_signal_overlay",
		"name":        "Derived Signal Overlay",
		"description": "At-a-glance signal health view with recent focus, anomaly windows, and status summary",
		"icon":        "bi-soundwave",
		"query_shape": "Columns: time, service, source, signal, attr_fp, value, sample_count, baseline_mean, " +
			"baseline_lower, baseline_upper, anomaly_state, anomaly_score",
		"sample_sql": "SELECT\n" +
			"  time,\n" +
			"  ServiceName AS service,\n" +
			"  SignalSource AS source,\n" +
			"  SignalName AS signal,\n" +
			"  AttrFingerprint AS attr_fp,\n" +
			"  value,\n" +
			"  SampleCount AS sample_count,\n" +
			"  baseline_mean,\n" +
			"  baseline_lower,\n" +
			"  baseline_upper,\n" +
			"  anomaly_state,\n" +
			"  anomaly_score\n" +
			"FROM v_derived_signals_anomaly\n" +
			"WHERE ServiceName = 'trace-svc-0'\n" +
			"  AND SignalSource = 'traces'\n" +
			"  AND SignalName = 'latency_p95_ms'\n" +
			"  AND time >= now() - INTERVAL 6 HOUR\n" +
			"ORDER BY time",
		"drilldown": map[string]any{
			"target":         "metrics",
			"label":          "Open signal details",
			"bucket_seconds": 60,
			"time_axis":      "x",
		},
		"min_columns": 12,
		"max_columns": 16,
		"column_roles": map[string]any{
			"time":            0,
			"service":         1,
			"source":          2,
			"signal":          3,
			"attr_fp":         4,
			"value":           5,
			"sample_count":    6,
			"baseline_mean":   7,
			"baseline_lower":  8,
			"baseline_upper":  9,
			"anomaly_state":   10,
			"anomaly_score":   11,
			"rule_state":      12,
			"rule_name":       13,
			"rule_reason":     14,
			"effective_state": 15,
		},
		"echarts_option_template": map[string]any{
			"title": map[string]any{
				"left":         8,
				"top":          2,
				"text":         "",
				"subtext":      "{{signal_summary}}",
				"textStyle":    map[string]any{"fontSize": 11, "color": "#adb5bd"},
				"subtextStyle": map[string]any{"fontSize": 11, "color": "#9ca3af"},
			},
			"tooltip": map[string]any{"trigger": "axis"},
			"legend":  map[string]any{"data": []any{"Value", "Baseline", "Expected Band"}, "bottom": 0},
			"xAxis":   map[string]any{"type": "time", "axisLabel": map[string]any{"hideOverlap": true}},
			"yAxis": map[string]any{
				"type":          "value",
				"name":          "{{y_axis_name}}",
				"nameTextStyle": map[string]any{"color": "#9ca3af", "fontSize": 11},
				"min":           "{{value_axis_min}}",
				"max":           "{{value_axis_max}}",
			},
			"dataZoom": []any{
				map[string]any{"type": "inside", "xAxisIndex": 0, "filterMode": "none", "start": "{{zoom_start_pct}}", "end": 100},
			},
			"visualMap": map[string]any{
				"show":        false,
				"dimension":   2,
				"seriesIndex": 3,
				"pieces": []any{
					map[string]any{"value": 2, "color": "#dc3545"},
					map[string]any{"value": 1, "color": "#ffc107"},
					map[string]any{"value": 0, "color": "#20c997"},
				},
			},
			"grid": map[string]any{"left": "3%", "right": "4%", "bottom": "15%", "containLabel": true},
			"series": []any{
				map[string]any{
					"name":      "Band Lower",
					"type":      "line",
					"data":      "{{baseline_lower_points}}",
					"lineStyle": map[string]any{"opacity": 0},
					"symbol":    "none",
					"stack":     "expected_band",
				},
				map[string]any{
					"name":      "Expected Band",
					"type":      "line",
					"data":      "{{baseline_upper_points}}",
					"lineStyle": map[string]any{"opacity": 0},
					"areaStyle": map[string]any{"color": "rgba(13, 110, 253, 0.12)"},
					"symbol":    "none",
					"stack":     "expected_band",
				},
				map[string]any{
					"name":      "Baseline",
					"type":      "line",
					"data":      "{{baseline_mean_points}}",
					"lineStyle": map[string]any{"type": "dashed", "color": "#6c757d"},
					"symbol":    "none",
				},
				map[string]any{
					"name":         "Value",
					"type":         "line",
					"smooth":       true,
					"data":         "{{value_points}}",
					"encode":       map[string]any{"x": 0, "y": 1},
					"lineStyle":    map[string]any{"width": 2, "color": "#20c997"},
					"symbol":       "circle",
					"symbolSize":   4,
					"itemStyle":    map[string]any{"color": "#20c997"},
					"connectNulls": true,
					"markArea":     map[string]any{"silent": true, "label": map[string]any{"show": false}, "data": "{{anomaly_mark_areas}}"},
				},
				map[string]any{
					"name":       "Warnings",
					"type":       "scatter",
					"data":       "{{warning_points}}",
					"symbolSize": 8,
					"itemStyle":  map[string]any{"color": "#ffc107"},
					"encode":     map[string]any{"x": 0, "y": 1},
				},
				map[string]any{
					"name":       "Outliers",
					"type":       "scatter",
					"data":       "{{outlier_points}}",
					"symbolSize": 10,
					"itemStyle":  map[string]any{"color": "#dc3545"},
					"encode":     map[string]any{"x": 0, "y": 1},
				},
			},
		},
	},
	"gauge_kpi": {
		"id":          "gauge_kpi",
		"name":        "KPI Gauge",
		"description": "Single-value gauge for KPI monitoring (SLA %, uptime %)",
		"icon":        "bi-speedometer",
		"query_shape": "Columns: single numeric value",
		"sample_sql": "SELECT\n" +
			"  round(100.0 * countIf(StatusCode = 'STATUS_CODE_OK') / count(), 2) AS value\n" +
			"FROM otel_traces\n" +
			"WHERE Timestamp > now() - interval 1 hour",
		"drilldown": map[string]any{
			"target": "traces",
			"label":  "Open source traces",
		},
		"min_columns":  1,
		"max_columns":  1,
		"column_roles": map[string]any{"value": 0},
		"echarts_option_template": map[string]any{
			"series": []any{
				map[string]any{
					"type":     "gauge",
					"progress": map[string]any{"itemStyle": map[string]any{"color": "#0d6efd"}},
					"axisLine": map[string]any{
						"lineStyle": map[string]any{
							"color": []any{[]any{0.3, "#dc3545"}, []any{0.7, "#ffc107"}, []any{1, "#28a745"}},
							"width": 30,
						},
					},
					"splitLine": map[string]any{"distance": 8},
					"axisTick":  map[string]any{"distance": 8},
					"axisLabel": map[string]any{"color": "#adb5bd"},
					"detail":    map[string]any{"valueAnimation": true, "formatter": "{value}%", "color": "#adb5bd"},
					"data":      []any{map[string]any{"value": "{{value_first}}", "name": "Current"}},
					"min":       0,
					"max":       100,
				},
			},
		},
	},
	"custom_echarts": {
		"id":           "custom_echarts",
		"name":         "Custom ECharts",
		"description":  "Bring your own SQL, mapping JSON, and raw ECharts option JSON.",
		"icon":         "bi-code-slash",
		"query_shape":  "Any SELECT result set",
		"sample_sql":   "SELECT toDateTime('2024-01-01 00:00:00') AS time, 1 AS value",
		"min_columns":  0,
		"column_roles": map[string]any{},
		"echarts_option_template": map[string]any{
			"tooltip": map[string]any{"trigger": "axis"},
			"xAxis":   map[string]any{"type": "time"},
			"yAxis":   map[string]any{"type": "value"},
			"series": []any{
				map[string]any{
					"name":       "Value",
					"type":       "line",
					"data":       "{{points}}",
					"showSymbol": false,
					"smooth":     true,
				},
			},
		},
	},
}

var queryDenyPattern = regexp.MustCompile(
	`(?i)\b(INSERT|UPDATE|DELETE|DROP|ALTER|CREATE|TRUNCATE|REPLACE|RENAME|ATTACH|DETACH|GRANT|REVOKE)\b`,
)

// validateChartQuery returns an error message if the query is not a safe
// SELECT, otherwise "" (mirrors _validate_chart_query returning None).
func validateChartQuery(query string) string {
	stripped := strings.TrimSpace(query)
	if stripped == "" {
		return "Query cannot be empty"
	}
	upper := strings.ToUpper(stripped)
	if !(strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH")) {
		return "Only SELECT queries are allowed"
	}
	if queryDenyPattern.MatchString(stripped) {
		return "Query contains a disallowed keyword"
	}
	return ""
}

func sqlLiteral(value any) string {
	return "'" + strings.ReplaceAll(chartPyStr(value), "'", "''") + "'"
}

func coercePositiveInt(raw any, defaultValue, minValue, maxValue int) int {
	// PORT-NOTE: mirrors int(str(raw)); fractional / non-numeric inputs fall
	// back to the default exactly like Python's ValueError path.
	parsed, err := strconv.Atoi(chartPyStr(raw))
	if err != nil {
		return defaultValue
	}
	if parsed < minValue {
		return minValue
	}
	if parsed > maxValue {
		return maxValue
	}
	return parsed
}

// defaultChartSpec mirrors _default_chart_spec (Python default arg
// template_id="derived_signal_overlay"; Go callers pass it explicitly).
func defaultChartSpec(templateId string) map[string]any {
	if templateId == "custom_echarts" {
		return map[string]any{
			"template_id": templateId,
			"sql": map[string]any{
				"mode":         "raw",
				"override_sql": "SELECT toDateTime('2024-01-01 00:00:00') AS time, 1 AS value",
			},
			"data": map[string]any{
				"source_view":   "v_derived_signals_anomaly",
				"service":       "",
				"signal_source": "traces",
				"signal_name":   "trace_volume",
				"metric_name":   "",
				"attr_fp":       "",
				"window_hours":  6,
				"limit":         1000,
			},
			"visual": map[string]any{
				"zoom_inside":    true,
				"zoom_slider":    false,
				"zoom_start_pct": 0,
				"zoom_end_pct":   100,
				"legend_show":    true,
				"smooth_line":    true,
				"value_color":    "",
				"role_map":       map[string]any{},
				// PORT-NOTE: literal strings match Python json.dumps output
				// (including ", "/": " separators).
				"custom_mapping_json": `{"points": {"from": "rows"}}`,
				"custom_option_json": `{"tooltip": {"trigger": "axis"}, "xAxis": {"type": "time"}, ` +
					`"yAxis": {"type": "value"}, "series": [{"name": "Value", "type": "line", ` +
					`"data": "{{points}}", "showSymbol": false, "smooth": true}]}`,
			},
		}
	}

	return map[string]any{
		"template_id": templateId,
		"sql":         map[string]any{"mode": "builder", "override_sql": ""},
		"data": map[string]any{
			"source_view":   "v_derived_signals_anomaly",
			"service":       "",
			"signal_source": "traces",
			"signal_name":   "trace_volume",
			"metric_name":   "",
			"attr_fp":       "",
			"window_hours":  6,
			"limit":         1000,
		},
		"visual": map[string]any{
			"zoom_inside":    true,
			"zoom_slider":    false,
			"zoom_start_pct": 0,
			"zoom_end_pct":   100,
			"legend_show":    true,
			"smooth_line":    true,
			"value_color":    "",
			"role_map":       map[string]any{},
		},
	}
}

// buildRawChartSpec mirrors _build_raw_chart_spec.
func buildRawChartSpec(templateId, query string, optionsJson string) map[string]any {
	if optionsJson != "" {
		var parsed any
		dec := json.NewDecoder(strings.NewReader(optionsJson))
		dec.UseNumber()
		if err := dec.Decode(&parsed); err == nil {
			if parsedMap, ok := parsed.(map[string]any); ok {
				if specCandidate, ok := parsedMap["chart_spec"].(map[string]any); ok {
					if normalized, err := normalizeChartSpec(specCandidate); err == nil {
						return normalized
					}
				}
			}
		}
	}

	spec := defaultChartSpec(templateId)
	spec["template_id"] = templateId
	spec["sql"] = map[string]any{"mode": "raw", "override_sql": query}
	return spec
}

var chartNamedQueryNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// normalizeChartSpec mirrors _normalize_chart_spec (ValueError -> error).
func normalizeChartSpec(specRaw any) (map[string]any, error) {
	base := defaultChartSpec("derived_signal_overlay")
	raw, _ := specRaw.(map[string]any)
	if raw == nil {
		raw = map[string]any{}
	}

	templateId := rowString(raw["template_id"])
	if templateId == "" {
		templateId = rowString(base["template_id"])
	}
	if templateId == "" {
		templateId = "time_series_percentiles"
	}
	templateId = strings.TrimSpace(templateId)
	if _, ok := chartTemplates[templateId]; !ok {
		return nil, fmt.Errorf("Unknown template: %s", templateId)
	}

	normalized := defaultChartSpec(templateId)
	normalized["template_id"] = templateId

	sqlRaw, _ := raw["sql"].(map[string]any)
	// PORT-NOTE: mirrors str(sql_raw.get("mode")) — a missing key yields the
	// Python string "None" ("none" after lower) and therefore an error.
	var modeVal any
	var overrideVal any
	if sqlRaw != nil {
		modeVal = sqlRaw["mode"]
		overrideVal = sqlRaw["override_sql"]
	}
	sqlMode := strings.ToLower(strings.TrimSpace(chartPyStr(modeVal)))
	if sqlMode != "builder" && sqlMode != "raw" {
		return nil, fmt.Errorf("sql.mode must be 'builder' or 'raw'")
	}
	normalized["sql"] = map[string]any{
		"mode":         sqlMode,
		"override_sql": chartPyStr(overrideVal),
	}

	dataRaw, _ := raw["data"].(map[string]any)
	if normalizedData, ok := normalized["data"].(map[string]any); ok {
		mergedData := map[string]any{}
		for k, v := range normalizedData {
			mergedData[k] = v
		}
		for k, v := range dataRaw {
			mergedData[k] = v
		}
		normalized["data"] = mergedData
	}

	visualRaw, _ := raw["visual"].(map[string]any)
	mergedVisual := map[string]any{}
	if normalizedVisual, ok := normalized["visual"].(map[string]any); ok {
		for k, v := range normalizedVisual {
			mergedVisual[k] = v
		}
	}
	for k, v := range visualRaw {
		mergedVisual[k] = v
	}

	roleMap := map[string]any{}
	if roleMapRaw, ok := mergedVisual["role_map"].(map[string]any); ok {
		for role, colName := range roleMapRaw {
			roleName := strings.TrimSpace(role)
			mapped := strings.TrimSpace(chartPyStr(colName))
			if roleName != "" && mapped != "" {
				roleMap[roleName] = mapped
			}
		}
	}
	mergedVisual["role_map"] = roleMap
	normalized["visual"] = mergedVisual

	// Named queries: additional SQL datasets referenced via {{rows:name}} etc. in eCharts JSON.
	namedQueries := []any{}
	if namedQueriesRaw, ok := raw["named_queries"].([]any); ok {
		for _, item := range namedQueriesRaw {
			itemMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(rowString(itemMap["name"])))
			sqlText := strings.TrimRight(strings.TrimSpace(rowString(itemMap["sql"])), ";")
			purpose := strings.TrimSpace(rowString(itemMap["purpose"]))
			if name == "" || !chartNamedQueryNameRe.MatchString(name) {
				continue
			}
			if sqlText == "" {
				continue
			}
			namedQueries = append(namedQueries, map[string]any{"name": name, "sql": sqlText, "purpose": purpose})
		}
	}
	normalized["named_queries"] = namedQueries

	return normalized, nil
}

// compileBuilderSql mirrors _compile_builder_sql.
func compileBuilderSql(templateId string, data map[string]any) (string, error) {
	if templateId == "custom_echarts" {
		return "", fmt.Errorf("custom_echarts requires sql.mode='raw'")
	}
	if data == nil {
		data = map[string]any{}
	}

	sourceView := strings.TrimSpace(rowString(data["source_view"]))
	if sourceView == "" {
		sourceView = "v_derived_signals_anomaly"
	}
	supportedSources := map[string]bool{
		"v_derived_signals_anomaly": true,
		"v_otel_metrics_anomaly":    true,
		"otel_metrics_gauge":        true,
		"otel_metrics_sum":          true,
		"otel_metrics_histogram":    true,
		"otel_logs":                 true,
		"otel_traces":               true,
		"sobs_error_resolutions":    true,
	}
	if !supportedSources[sourceView] {
		return "", fmt.Errorf("Unsupported source for builder mode")
	}

	service := strings.TrimSpace(rowString(data["service"]))
	signalSource := strings.TrimSpace(rowString(data["signal_source"]))
	signalName := strings.TrimSpace(rowString(data["signal_name"]))
	metricName := strings.TrimSpace(rowString(data["metric_name"]))
	attrFp := strings.TrimSpace(rowString(data["attr_fp"]))
	windowHours := coercePositiveInt(data["window_hours"], 6, 1, 168)
	limit := coercePositiveInt(data["limit"], 1000, 1, 2000)

	defaultSourceLabel := func() string {
		if sourceView == "otel_logs" {
			return "logs"
		}
		if sourceView == "otel_traces" {
			return "traces"
		}
		if sourceView == "sobs_error_resolutions" {
			return "errors"
		}
		if sourceView == "v_derived_signals_anomaly" {
			if signalSource != "" {
				return signalSource
			}
			return "derived"
		}
		return "metrics"
	}

	defaultSignalLabel := func() string {
		if signalName != "" {
			return signalName
		}
		if metricName != "" {
			return metricName
		}
		if sourceView == "otel_logs" {
			return "log_volume"
		}
		if sourceView == "otel_traces" {
			return "trace_volume"
		}
		if sourceView == "sobs_error_resolutions" {
			return "resolved_error_volume"
		}
		return "value"
	}

	// scoredCountSeriesSql builds the shared per-minute count + rolling
	// baseline CTE used by the otel_logs / otel_traces / sobs_error_resolutions
	// branches of _build_series_sql.
	scoredCountSeriesSql := func(timeExpr, tableName, whereClause string) string {
		return "WITH per_minute AS (\n" +
			"  SELECT\n" +
			"    toStartOfMinute(" + timeExpr + ") AS time,\n" +
			"    count() AS value\n" +
			"  FROM " + tableName + "\n" +
			"  WHERE " + whereClause + "\n" +
			"  GROUP BY time\n" +
			"), scored AS (\n" +
			"  SELECT\n" +
			"    time,\n" +
			"    toFloat64(value) AS value,\n" +
			"    avg(toFloat64(value)) OVER (\n" +
			"      ORDER BY time\n" +
			"      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n" +
			"    ) AS baseline_mean,\n" +
			"    stddevPop(toFloat64(value)) OVER (\n" +
			"      ORDER BY time\n" +
			"      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n" +
			"    ) AS baseline_stddev\n" +
			"  FROM per_minute\n" +
			")\n" +
			"SELECT\n" +
			"  time,\n" +
			"  value,\n" +
			"  baseline_mean,\n" +
			"  greatest(0.0, baseline_mean - (3.0 * ifNull(baseline_stddev, 0.0))) AS baseline_lower,\n" +
			"  baseline_mean + (3.0 * ifNull(baseline_stddev, 0.0)) AS baseline_upper,\n" +
			"  if(\n" +
			"    abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) >= 3.0,\n" +
			"    'outlier',\n" +
			"    'normal'\n" +
			"  ) AS anomaly_state,\n" +
			"  abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) AS anomaly_score\n" +
			"FROM scored"
	}

	buildSeriesSql := func() string {
		if sourceView == "v_derived_signals_anomaly" {
			whereParts := []string{fmt.Sprintf("time >= now() - INTERVAL %d HOUR", windowHours)}
			if service != "" {
				whereParts = append(whereParts, fmt.Sprintf("ServiceName = %s", sqlLiteral(service)))
			}
			if attrFp != "" {
				whereParts = append(whereParts, fmt.Sprintf("AttrFingerprint = %s", sqlLiteral(attrFp)))
			}
			if signalSource != "" {
				whereParts = append(whereParts, fmt.Sprintf("SignalSource = %s", sqlLiteral(signalSource)))
			}
			if signalName != "" {
				whereParts = append(whereParts, fmt.Sprintf("SignalName = %s", sqlLiteral(signalName)))
			}
			whereClause := strings.Join(whereParts, " AND\n    ")
			return "SELECT\n" +
				"  time,\n" +
				"  value,\n" +
				"  baseline_mean,\n" +
				"  baseline_lower,\n" +
				"  baseline_upper,\n" +
				"  anomaly_state,\n" +
				"  anomaly_score\n" +
				"FROM v_derived_signals_anomaly\n" +
				"WHERE " + whereClause
		}

		if sourceView == "v_otel_metrics_anomaly" {
			whereParts := []string{fmt.Sprintf("time >= now() - INTERVAL %d HOUR", windowHours)}
			if service != "" {
				whereParts = append(whereParts, fmt.Sprintf("ServiceName = %s", sqlLiteral(service)))
			}
			if metricName != "" {
				whereParts = append(whereParts, fmt.Sprintf("MetricName = %s", sqlLiteral(metricName)))
			}
			if attrFp != "" {
				whereParts = append(whereParts, fmt.Sprintf("AttrFingerprint = %s", sqlLiteral(attrFp)))
			}
			whereClause := strings.Join(whereParts, " AND\n    ")
			return "SELECT\n" +
				"  time,\n" +
				"  value,\n" +
				"  baseline_mean,\n" +
				"  baseline_lower,\n" +
				"  baseline_upper,\n" +
				"  anomaly_state,\n" +
				"  anomaly_score\n" +
				"FROM v_otel_metrics_anomaly\n" +
				"WHERE " + whereClause
		}

		if sourceView == "otel_metrics_gauge" || sourceView == "otel_metrics_sum" || sourceView == "otel_metrics_histogram" {
			valueExpr := "Value"
			if sourceView == "otel_metrics_histogram" {
				valueExpr = "if(Count = 0, 0.0, Sum / toFloat64(Count))"
			}
			whereParts := []string{fmt.Sprintf("TimeUnixMs >= now() - INTERVAL %d HOUR", windowHours)}
			if service != "" {
				whereParts = append(whereParts, fmt.Sprintf("ServiceName = %s", sqlLiteral(service)))
			}
			if metricName != "" {
				whereParts = append(whereParts, fmt.Sprintf("MetricName = %s", sqlLiteral(metricName)))
			}
			if attrFp != "" {
				whereParts = append(whereParts, fmt.Sprintf("AttrFingerprint = %s", sqlLiteral(attrFp)))
			}
			whereClause := strings.Join(whereParts, " AND\n    ")
			return "WITH per_minute AS (\n" +
				"  SELECT\n" +
				"    toStartOfMinute(TimeUnixMs) AS time,\n" +
				"    avg(toFloat64(" + valueExpr + ")) AS value\n" +
				"  FROM " + sourceView + "\n" +
				"  WHERE " + whereClause + "\n" +
				"  GROUP BY time\n" +
				"), scored AS (\n" +
				"  SELECT\n" +
				"    time,\n" +
				"    value,\n" +
				"    avg(value) OVER (\n" +
				"      ORDER BY time\n" +
				"      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n" +
				"    ) AS baseline_mean,\n" +
				"    stddevPop(value) OVER (\n" +
				"      ORDER BY time\n" +
				"      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n" +
				"    ) AS baseline_stddev\n" +
				"  FROM per_minute\n" +
				")\n" +
				"SELECT\n" +
				"  time,\n" +
				"  value,\n" +
				"  baseline_mean,\n" +
				"  greatest(0.0, baseline_mean - (3.0 * ifNull(baseline_stddev, 0.0))) AS baseline_lower,\n" +
				"  baseline_mean + (3.0 * ifNull(baseline_stddev, 0.0)) AS baseline_upper,\n" +
				"  if(\n" +
				"    abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) >= 3.0,\n" +
				"    'outlier',\n" +
				"    'normal'\n" +
				"  ) AS anomaly_state,\n" +
				"  abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) AS anomaly_score\n" +
				"FROM scored"
		}

		if sourceView == "otel_logs" {
			whereParts := []string{fmt.Sprintf("TimestampTime >= now() - INTERVAL %d HOUR", windowHours)}
			if service != "" {
				whereParts = append(whereParts, fmt.Sprintf("ServiceName = %s", sqlLiteral(service)))
			}
			whereClause := strings.Join(whereParts, " AND\n    ")
			return scoredCountSeriesSql("TimestampTime", "otel_logs", whereClause)
		}

		if sourceView == "otel_traces" {
			whereParts := []string{fmt.Sprintf("TimestampTime >= now() - INTERVAL %d HOUR", windowHours)}
			if service != "" {
				whereParts = append(whereParts, fmt.Sprintf("ServiceName = %s", sqlLiteral(service)))
			}
			whereClause := strings.Join(whereParts, " AND\n    ")
			return scoredCountSeriesSql("TimestampTime", "otel_traces", whereClause)
		}

		whereClause := fmt.Sprintf("ResolvedAt >= now() - INTERVAL %d HOUR", windowHours)
		return scoredCountSeriesSql("ResolvedAt", "sobs_error_resolutions", whereClause)
	}

	seriesSql := buildSeriesSql()

	serviceOrAll := service
	if serviceOrAll == "" {
		serviceOrAll = "all"
	}

	switch templateId {
	case "derived_signal_overlay":
		return "WITH series AS (\n" +
			seriesSql + "\n" +
			")\n" +
			"SELECT\n" +
			"  time,\n" +
			fmt.Sprintf("  %s AS service,\n", sqlLiteral(serviceOrAll)) +
			fmt.Sprintf("  %s AS source,\n", sqlLiteral(defaultSourceLabel())) +
			fmt.Sprintf("  %s AS signal,\n", sqlLiteral(defaultSignalLabel())) +
			fmt.Sprintf("  %s AS attr_fp,\n", sqlLiteral(attrFp)) +
			"  value,\n" +
			"  toUInt32(1) AS sample_count,\n" +
			"  baseline_mean,\n" +
			"  baseline_lower,\n" +
			"  baseline_upper,\n" +
			"  anomaly_state,\n" +
			"  anomaly_score\n" +
			"FROM series\n" +
			"ORDER BY time\n" +
			fmt.Sprintf("LIMIT %d", limit), nil
	case "anomaly_overlay":
		return "WITH series AS (\n" +
			seriesSql + "\n" +
			")\n" +
			"SELECT\n" +
			"  time,\n" +
			"  value,\n" +
			"  baseline_mean,\n" +
			"  baseline_lower,\n" +
			"  baseline_upper,\n" +
			"  anomaly_state\n" +
			"FROM series\n" +
			"ORDER BY time\n" +
			fmt.Sprintf("LIMIT %d", limit), nil
	case "dual_axis_anomaly":
		return "WITH series AS (\n" +
			seriesSql + "\n" +
			")\n" +
			"SELECT\n" +
			"  time,\n" +
			"  value AS metric,\n" +
			"  anomaly_score\n" +
			"FROM series\n" +
			"ORDER BY time\n" +
			fmt.Sprintf("LIMIT %d", limit), nil
	case "time_series_percentiles":
		return "WITH series AS (\n" +
			seriesSql + "\n" +
			")\n" +
			"SELECT\n" +
			"  time,\n" +
			"  value,\n" +
			"  baseline_upper AS p95,\n" +
			"  greatest(baseline_upper, value) AS p99\n" +
			"FROM series\n" +
			"ORDER BY time\n" +
			fmt.Sprintf("LIMIT %d", limit), nil
	case "heatmap":
		return "WITH series AS (\n" +
			seriesSql + "\n" +
			")\n" +
			"SELECT\n" +
			fmt.Sprintf("  %s AS x_category,\n", sqlLiteral(serviceOrAll)) +
			"  toStartOfFiveMinutes(time) AS y_category,\n" +
			"  avg(value) AS value\n" +
			"FROM series\n" +
			"GROUP BY y_category\n" +
			"ORDER BY y_category\n" +
			fmt.Sprintf("LIMIT %d", limit), nil
	case "box_plot":
		return "WITH series AS (\n" +
			seriesSql + "\n" +
			")\n" +
			"SELECT\n" +
			fmt.Sprintf("  %s AS dimension,\n", sqlLiteral(defaultSignalLabel())) +
			"  min(value) AS min,\n" +
			"  quantile(0.25)(value) AS q1,\n" +
			"  quantile(0.5)(value) AS median,\n" +
			"  quantile(0.75)(value) AS q3,\n" +
			"  max(value) AS max\n" +
			"FROM series", nil
	case "gauge_kpi":
		return "WITH series AS (\n" +
			seriesSql + "\n" +
			")\n" +
			"SELECT round(100.0 * avg(if(anomaly_state = 'normal', 1.0, 0.0)), 2) AS value\n" +
			"FROM series", nil
	}

	return "", fmt.Errorf("Builder mode does not support template: %s", templateId)
}

// compileChartSpec mirrors _compile_chart_spec.
func compileChartSpec(specRaw any) (string, string, map[string]any, error) {
	spec, err := normalizeChartSpec(specRaw)
	if err != nil {
		return "", "", nil, err
	}

	templateId := strings.TrimSpace(rowString(spec["template_id"]))
	if templateId == "" {
		templateId = "time_series_percentiles"
	}

	sqlBlock, _ := spec["sql"].(map[string]any)
	sqlMode := "builder"
	if sqlBlock != nil {
		sqlMode = strings.ToLower(strings.TrimSpace(rowString(sqlBlock["mode"])))
	}

	var query string
	if sqlMode == "raw" {
		if sqlBlock != nil {
			query = strings.TrimSpace(rowString(sqlBlock["override_sql"]))
		}
	} else {
		if templateId == "custom_echarts" {
			return "", "", nil, fmt.Errorf("custom_echarts requires sql.mode='raw'")
		}
		data, _ := spec["data"].(map[string]any)
		query, err = compileBuilderSql(templateId, data)
		if err != nil {
			return "", "", nil, err
		}
	}

	if errMsg := validateChartQuery(query); errMsg != "" {
		return "", "", nil, fmt.Errorf("%s", errMsg)
	}

	// Validate named queries SQL (read-only check only; execution not required here)
	if namedQueries, ok := spec["named_queries"].([]any); ok {
		for _, nq := range namedQueries {
			nqMap, ok := nq.(map[string]any)
			if !ok {
				continue
			}
			nqSql := strings.TrimSpace(rowString(nqMap["sql"]))
			nqName := strings.TrimSpace(rowString(nqMap["name"]))
			if nqSql != "" {
				if nqErr := validateChartQuery(nqSql); nqErr != "" {
					return "", "", nil, fmt.Errorf("Named query '%s': %s", nqName, nqErr)
				}
			}
		}
	}

	return templateId, query, spec, nil
}

// resolveTemplateRoleIndices mirrors _resolve_template_role_indices.
func resolveTemplateRoleIndices(
	templateId string,
	template map[string]any,
	columns []string,
	spec map[string]any,
) (map[string]int, error) {
	rawRoles, _ := template["column_roles"].(map[string]any)
	roleIndices := map[string]int{}
	for role, idxRaw := range rawRoles {
		if f, ok := chartNumeric(idxRaw); ok {
			roleIndices[role] = int(f)
		}
	}

	if len(spec) == 0 {
		return roleIndices, nil
	}

	visual, _ := spec["visual"].(map[string]any)
	var roleMapRaw map[string]any
	if visual != nil {
		roleMapRaw, _ = visual["role_map"].(map[string]any)
	}
	if roleMapRaw == nil {
		return roleIndices, nil
	}

	colIndexByName := map[string]int{}
	for idx, name := range columns {
		colIndexByName[name] = idx
	}
	// PORT-NOTE: Python keeps the LAST duplicate in col_index_by_name (dict
	// comprehension overwrite); mirror that by overwriting above.
	lowerNameToIndex := map[string]int{}
	for idx, name := range columns {
		lower := strings.ToLower(name)
		if _, ok := lowerNameToIndex[lower]; !ok {
			lowerNameToIndex[lower] = idx
		}
	}

	for role, mappedCol := range roleMapRaw {
		roleName := strings.TrimSpace(role)
		colName := strings.TrimSpace(chartPyStr(mappedCol))
		if roleName == "" || colName == "" {
			continue
		}
		if _, ok := roleIndices[roleName]; !ok {
			return nil, fmt.Errorf("Unknown role '%s' for template %s", roleName, templateId)
		}

		if idx, ok := colIndexByName[colName]; ok {
			roleIndices[roleName] = idx
			continue
		}

		lowered := strings.ToLower(colName)
		if idx, ok := lowerNameToIndex[lowered]; ok {
			roleIndices[roleName] = idx
			continue
		}

		return nil, fmt.Errorf("Role '%s' maps to unknown column '%s'", roleName, colName)
	}

	return roleIndices, nil
}

// parseBool mirrors _parse_bool (shared with other sections).
func parseBool(value any, defaultValue bool) bool {
	if b, ok := value.(bool); ok {
		return b
	}
	if value == nil {
		return defaultValue
	}
	raw := strings.ToLower(strings.TrimSpace(chartPyStr(value)))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return defaultValue
}

// applyChartSpecVisualOverrides mirrors _apply_chart_spec_visual_overrides.
func applyChartSpecVisualOverrides(templateId string, option map[string]any, spec map[string]any) map[string]any {
	if templateId == "custom_echarts" {
		return option
	}

	visual, _ := spec["visual"].(map[string]any)
	if visual == nil {
		return option
	}

	legendShow := parseBool(visual["legend_show"], true)
	if legend, ok := option["legend"].(map[string]any); ok {
		legend["show"] = legendShow
	}

	zoomInside := parseBool(visual["zoom_inside"], true)
	zoomSlider := parseBool(visual["zoom_slider"], false)
	dataZoom, _ := option["dataZoom"].([]any)
	if dataZoom == nil {
		dataZoom = []any{}
	}
	zoomStart := coercePositiveInt(visual["zoom_start_pct"], 0, 0, 100)
	zoomEnd := coercePositiveInt(visual["zoom_end_pct"], 100, 0, 100)
	maxZoomEnd := zoomEnd
	if zoomStart > maxZoomEnd {
		maxZoomEnd = zoomStart
	}
	nextDataZoom := []any{}
	if zoomInside {
		nextDataZoom = append(nextDataZoom, map[string]any{
			"type":       "inside",
			"xAxisIndex": 0,
			"filterMode": "none",
			"start":      zoomStart,
			"end":        maxZoomEnd,
		})
	}
	if zoomSlider {
		nextDataZoom = append(nextDataZoom, map[string]any{
			"type":        "slider",
			"xAxisIndex":  0,
			"start":       zoomStart,
			"end":         maxZoomEnd,
			"height":      16,
			"bottom":      30,
			"borderColor": "#495057",
			"fillerColor": "rgba(13, 110, 253, 0.20)",
			"handleStyle": map[string]any{"color": "#0d6efd"},
		})
	}
	if len(nextDataZoom) > 0 {
		option["dataZoom"] = nextDataZoom
	} else {
		option["dataZoom"] = dataZoom
	}

	smoothLine := parseBool(visual["smooth_line"], true)
	valueColor := strings.TrimSpace(rowString(visual["value_color"]))
	if series, ok := option["series"].([]any); ok {
		for _, sRaw := range series {
			s, ok := sRaw.(map[string]any)
			if !ok {
				continue
			}
			name := ""
			if nv, ok := s["name"]; ok {
				name = chartPyStr(nv)
			}
			if name != "Value" {
				continue
			}
			if tv, ok := s["type"]; ok && chartPyStr(tv) == "line" {
				s["smooth"] = smoothLine
			}
			if valueColor != "" {
				lineStyle := map[string]any{}
				itemStyle := map[string]any{}
				if existingLineStyle, ok := s["lineStyle"].(map[string]any); ok {
					for key, val := range existingLineStyle {
						lineStyle[key] = val
					}
				}
				if existingItemStyle, ok := s["itemStyle"].(map[string]any); ok {
					for key, val := range existingItemStyle {
						itemStyle[key] = val
					}
				}
				lineStyle["color"] = valueColor
				itemStyle["color"] = valueColor
				s["lineStyle"] = lineStyle
				s["itemStyle"] = itemStyle
			}
		}
	}

	// Template guard for future template-specific visual overrides.
	_ = templateId
	return option
}

// inferColumnTypes mirrors _infer_column_types.
// PORT-NOTE: emits Python type names ("str", "int", "float", "bool", "list",
// "dict", "null") inferred from JSON-decoded values; json.Number text decides
// int vs float exactly like Python's json module.
func inferColumnTypes(columns []string, rows []any) []string {
	inferred := []string{}
	for idx := range columns {
		detected := "null"
		for _, rowRaw := range rows {
			row, ok := rowRaw.([]any)
			if !ok || idx >= len(row) {
				continue
			}
			value := row[idx]
			if value == nil {
				continue
			}
			detected = chartPyTypeName(value)
			break
		}
		inferred = append(inferred, detected)
	}
	return inferred
}

func chartPyTypeName(value any) string {
	switch t := value.(type) {
	case string:
		return "str"
	case bool:
		return "bool"
	case json.Number:
		if strings.ContainsAny(t.String(), ".eE") {
			return "float"
		}
		return "int"
	case float64:
		return "float"
	case int, int64:
		return "int"
	case []any:
		return "list"
	case map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", value)
}

var chartErrCodePrefixRe = regexp.MustCompile(`^Code:\s*\d+\.\s*DB::Exception:\s*`)
var chartErrDbExcPrefixRe = regexp.MustCompile(`^DB::Exception:\s*`)

// publicDashboardQueryError extracts a concise, user-safe error message from a
// database error (mirrors _public_dashboard_query_error).
func publicDashboardQueryError(exc error) string {
	raw := ""
	if exc != nil {
		raw = strings.TrimSpace(exc.Error())
	}
	message := ""
	if raw != "" {
		message = strings.TrimSpace(strings.SplitN(raw, "\n", 2)[0])
	}
	message = chartErrCodePrefixRe.ReplaceAllString(message, "")
	message = chartErrDbExcPrefixRe.ReplaceAllString(message, "")
	message = strings.TrimSpace(strings.SplitN(message, ": while executing function", 2)[0])
	message = strings.TrimSpace(strings.SplitN(message, ". Stack trace", 2)[0])
	if message == "" {
		return "Query execution failed"
	}
	if (strings.Contains(raw, "NO_COMMON_TYPE") || strings.Contains(raw, "TYPE_MISMATCH")) &&
		!strings.Contains(message, "Check casts and column types.") {
		message = message + ". Check casts and column types."
	}
	if len([]rune(message)) > 280 {
		message = strings.TrimRightFunc(clipRunes(message, 277), unicode.IsSpace) + "..."
	}
	return message
}

// deepSubstitute recursively substitutes {{key}} placeholders in a JSON object
// (mirrors _deep_substitute).
// PORT-NOTE: when a string contains multiple placeholders Python picks the
// first matching binding in dict insertion order; Go map iteration order is
// random. All template strings carry a single placeholder, so this is benign.
func deepSubstitute(obj any, bindings map[string]any) any {
	switch v := obj.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = deepSubstitute(val, bindings)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = deepSubstitute(item, bindings)
		}
		return out
	case string:
		// Replace {{key}} with binding value
		for key, value := range bindings {
			placeholder := "{{" + key + "}}"
			if strings.Contains(v, placeholder) {
				if value != nil {
					return value
				}
				return v
			}
		}
		return v
	}
	return obj
}

// ---------------------------------------------------------------------------
// File-local scalar/collection helpers (prefixed "chart" to avoid clashes).
// ---------------------------------------------------------------------------

// chartPyStr mirrors Python str(value): None->"None", bool->"True"/"False".
// PORT-NOTE: float repr approximates Python's; integral floats render "N.0".
func chartPyStr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return "False"
	case json.Number:
		return v.String()
	case float64:
		return chartPyFloatRepr(v)
	case float32:
		return chartPyFloatRepr(float64(v))
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	}
	return fmt.Sprintf("%v", value)
}

func chartPyFloatRepr(f float64) string {
	if math.IsInf(f, 1) {
		return "inf"
	}
	if math.IsInf(f, -1) {
		return "-inf"
	}
	if math.IsNaN(f) {
		return "nan"
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEnN") {
		s += ".0"
	}
	return s
}

// chartNumeric mirrors isinstance(x, (int, float)) returning the float value.
// PORT-NOTE: Python bool is an int subclass, so bools count as numeric.
func chartNumeric(value any) (float64, bool) {
	switch v := value.(type) {
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// chartFloat mirrors Python float(value).
// PORT-NOTE: Python float() raises on invalid input; the port returns 0.0.
func chartFloat(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f
	case bool:
		if v {
			return 1
		}
		return 0
	}
	return 0
}

// chartRound2 mirrors round(x, 2).
// PORT-NOTE: Python round uses banker's rounding; RoundToEven mirrors it.
func chartRound2(x float64) float64 {
	return math.RoundToEven(x*100) / 100
}

// chartTitle mirrors str.title() (capitalize each word, lowercase the rest).
func chartTitle(s string) string {
	var b strings.Builder
	prevAlpha := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			if prevAlpha {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(unicode.ToUpper(r))
			}
			prevAlpha = true
		} else {
			b.WriteRune(r)
			prevAlpha = false
		}
	}
	return b.String()
}

// chartEq mirrors Python == between query-result scalars.
// PORT-NOTE: numeric values compare by float; everything else by str().
func chartEq(a, b any) bool {
	af, aok := chartNumeric(a)
	bf, bok := chartNumeric(b)
	if aok && bok {
		return af == bf
	}
	return chartPyStr(a) == chartPyStr(b)
}

func chartMinFloat(values []float64) float64 {
	m := values[0]
	for _, v := range values[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func chartMaxFloat(values []float64) float64 {
	m := values[0]
	for _, v := range values[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

// chartMinMaxNumeric returns the smallest/largest numeric value (original type
// preserved) and whether any numeric value was present.
func chartMinMaxNumeric(values []any) (any, any, bool) {
	var minV, maxV any
	var minF, maxF float64
	found := false
	for _, v := range values {
		f, ok := chartNumeric(v)
		if !ok {
			continue
		}
		if !found {
			minV, maxV, minF, maxF, found = v, v, f, f, true
			continue
		}
		if f < minF {
			minF, minV = f, v
		}
		if f > maxF {
			maxF, maxV = f, v
		}
	}
	return minV, maxV, found
}

// chartSortedUnique mirrors sorted(set(values)).
// PORT-NOTE: dedup keys on (type, str); mixed-type ordering differs from
// Python which would raise TypeError — query columns here are homogeneous.
func chartSortedUnique(values []any) []any {
	seen := map[string]bool{}
	uniq := []any{}
	for _, v := range values {
		key := chartPyTypeName(v) + "\x00" + chartPyStr(v)
		if !seen[key] {
			seen[key] = true
			uniq = append(uniq, v)
		}
	}
	sort.SliceStable(uniq, func(i, j int) bool {
		fi, oi := chartNumeric(uniq[i])
		fj, oj := chartNumeric(uniq[j])
		if oi && oj {
			return fi < fj
		}
		return chartPyStr(uniq[i]) < chartPyStr(uniq[j])
	})
	return uniq
}

// chartRowGet mirrors row[col_idx] for list rows / row.get(columns[col_idx])
// for dict rows.
func chartRowGet(row any, idx int, columns []string) any {
	switch r := row.(type) {
	case []any:
		if idx >= 0 && idx < len(r) {
			return r[idx]
		}
		return nil
	case Row:
		if idx >= 0 && idx < len(columns) {
			return r[columns[idx]]
		}
		return nil
	}
	return nil
}

// chartDeepCopy mirrors copy.deepcopy for JSON-like data.
func chartDeepCopy(obj any) any {
	switch v := obj.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = chartDeepCopy(val)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = chartDeepCopy(item)
		}
		return out
	}
	return obj
}

// extractBindings mirrors _extract_bindings: build data bindings from query
// results based on column roles.
func extractBindings(template map[string]any, columns []string, rows []any, roleIndices map[string]int) map[string]any {
	bindings := map[string]any{}

	type roleIdx struct {
		role string
		idx  int
	}
	var roles []roleIdx
	if roleIndices != nil {
		for role, idx := range roleIndices {
			roles = append(roles, roleIdx{role, idx})
		}
	} else if cr, ok := template["column_roles"].(map[string]any); ok {
		for role, raw := range cr {
			idx := 0
			if f, ok := chartNumeric(raw); ok {
				idx = int(f)
			}
			roles = append(roles, roleIdx{role, idx})
		}
	}

	for _, ri := range roles {
		if ri.idx < len(columns) {
			values := make([]any, len(rows))
			for i, row := range rows {
				values[i] = chartRowGet(row, ri.idx, columns)
			}
			bindings[ri.role] = values
		}
	}

	// For heatmap: extract unique X and Y, build matrix.
	_, hasX := bindings["x_category"]
	_, hasY := bindings["y_category"]
	_, hasVal := bindings["value"]
	if hasX && hasY && hasVal {
		xVals, xok := bindings["x_category"].([]any)
		yVals, yok := bindings["y_category"].([]any)
		vVals, vok := bindings["value"].([]any)
		if xok && yok && vok {
			xUnique := chartSortedUnique(xVals)
			yUnique := chartSortedUnique(yVals)
			bindings["x_unique_values"] = xUnique
			bindings["y_unique_values"] = yUnique

			zipLen := min(len(xVals), len(yVals), len(vVals))
			heatmapData := []any{}
			for i, xVal := range xUnique {
				for j, yVal := range yUnique {
					for k := 0; k < zipLen; k++ {
						if chartEq(xVals[k], xVal) && chartEq(yVals[k], yVal) {
							heatmapData = append(heatmapData, []any{i, j, vVals[k]})
							break
						}
					}
				}
			}
			bindings["heatmap_data"] = heatmapData
			minV, maxV, found := chartMinMaxNumeric(vVals)
			if found {
				bindings["value_min"] = minV
				bindings["value_max"] = maxV
			} else {
				bindings["value_min"] = 0
				bindings["value_max"] = 1
			}
		}
	}

	// For box plot: build [min, q1, median, q3, max] array.
	_, hasMin := bindings["min"]
	_, hasMax := bindings["max"]
	if hasMin && hasMax {
		minVals, ok1 := bindings["min"].([]any)
		q1Vals, ok2 := bindings["q1"].([]any)
		medVals, ok3 := bindings["median"].([]any)
		q3Vals, ok4 := bindings["q3"].([]any)
		maxVals, ok5 := bindings["max"].([]any)
		if ok1 && ok2 && ok3 && ok4 && ok5 {
			n := min(len(minVals), len(q1Vals), len(medVals), len(q3Vals), len(maxVals))
			boxplotData := []any{}
			for i := 0; i < n; i++ {
				boxplotData = append(boxplotData, []any{minVals[i], q1Vals[i], medVals[i], q3Vals[i], maxVals[i]})
			}
			bindings["boxplot_data"] = boxplotData
			if dim, ok := bindings["dimension"]; ok {
				bindings["dimension_values"] = dim
			} else {
				bindings["dimension_values"] = []any{}
			}
		}
	}

	// For gauge: get first value.
	if vList, ok := bindings["value"].([]any); ok && len(vList) > 0 {
		bindings["value_first"] = vList[0]
	}

	// For anomaly overlays: build per-point symbol sizes and colors.
	var stateBinding any
	if v, ok := bindings["effective_state"]; ok {
		stateBinding = v
	} else {
		stateBinding = bindings["anomaly_state"]
	}
	if states, ok := stateBinding.([]any); ok {
		stateColors := map[string]string{"outlier": "#dc3545", "warning": "#ffc107", "normal": "#0d6efd"}
		stateSizes := map[string]int{"outlier": 10, "warning": 7, "normal": 4}
		pointColor := make([]any, len(states))
		symbolSize := make([]any, len(states))
		for i, s := range states {
			key := chartPyStr(s)
			if c, ok := stateColors[key]; ok {
				pointColor[i] = c
			} else {
				pointColor[i] = "#0d6efd"
			}
			if z, ok := stateSizes[key]; ok {
				symbolSize[i] = z
			} else {
				symbolSize[i] = 4
			}
		}
		bindings["anomaly_point_color"] = pointColor
		bindings["anomaly_symbol_size"] = symbolSize
	}

	// Derived signal overlays: choose chart style by signal semantics.
	if chartPyStr(template["id"]) == "derived_signal_overlay" {
		bindings["value_axis_min"] = "dataMin"
		bindings["value_axis_max"] = "dataMax"
		bindings["zoom_start_pct"] = 0
		bindings["signal_summary"] = ""
		bindings["y_axis_name"] = "Value"

		signalName := ""
		if sb, ok := bindings["signal"].([]any); ok && len(sb) > 0 {
			signalName = strings.ToLower(chartPyStr(sb[0]))
		}

		if strings.Contains(signalName, "ratio") {
			bindings["value_axis_min"] = 0
			bindings["value_axis_max"] = 1
		} else {
			for _, token := range []string{"volume", "count", "latency", "duration", "p95", "p99"} {
				if strings.Contains(signalName, token) {
					bindings["value_axis_min"] = 0
					break
				}
			}
		}

		timeValues, tOk := bindings["time"].([]any)
		valueValues, vOk := bindings["value"].([]any)
		baselineMeanValues, bmOk := bindings["baseline_mean"].([]any)
		baselineLowerValues, blOk := bindings["baseline_lower"].([]any)
		baselineUpperValues, buOk := bindings["baseline_upper"].([]any)
		var effectiveStates any
		if v, ok := bindings["effective_state"]; ok {
			effectiveStates = v
		} else {
			effectiveStates = bindings["anomaly_state"]
		}

		if tOk && vOk && bmOk && blOk && buOk {
			stateToRank := map[string]int{"normal": 0, "warning": 1, "outlier": 2}
			var rankSeries []int
			if es, ok := effectiveStates.([]any); ok {
				rankSeries = make([]int, len(es))
				for i, s := range es {
					if r, ok := stateToRank[chartPyStr(s)]; ok {
						rankSeries[i] = r
					}
				}
			}
			if len(rankSeries) == 0 {
				rankSeries = make([]int, len(valueValues))
			}

			useDeltaMode := !strings.Contains(signalName, "ratio")
			var plotValues, plotBaseline, plotLower, plotUpper []float64
			if useDeltaMode {
				bindings["y_axis_name"] = "Delta %"
				m := min(len(valueValues), len(baselineMeanValues), len(baselineLowerValues), len(baselineUpperValues))
				for idx := 0; idx < m; idx++ {
					base := chartFloat(baselineMeanValues[idx])
					val := chartFloat(valueValues[idx])
					low := chartFloat(baselineLowerValues[idx])
					up := chartFloat(baselineUpperValues[idx])
					if math.Abs(base) < 1e-9 {
						plotValues = append(plotValues, 0.0)
						plotBaseline = append(plotBaseline, 0.0)
						plotLower = append(plotLower, 0.0)
						plotUpper = append(plotUpper, 0.0)
					} else {
						denom := math.Abs(base)
						plotValues = append(plotValues, ((val-base)/denom)*100.0)
						plotBaseline = append(plotBaseline, 0.0)
						plotLower = append(plotLower, ((low-base)/denom)*100.0)
						plotUpper = append(plotUpper, ((up-base)/denom)*100.0)
					}
				}
				if len(plotValues) > 0 {
					minBound := chartMinFloat(append(append([]float64{}, plotLower...), plotValues...))
					maxBound := chartMaxFloat(append(append([]float64{}, plotUpper...), plotValues...))
					span := math.Max(5.0, (maxBound-minBound)*0.15)
					bindings["value_axis_min"] = chartRound2(minBound - span)
					bindings["value_axis_max"] = chartRound2(maxBound + span)
				}
			} else {
				plotValues = make([]float64, len(valueValues))
				for i, v := range valueValues {
					plotValues[i] = chartFloat(v)
				}
				plotBaseline = make([]float64, len(baselineMeanValues))
				for i, v := range baselineMeanValues {
					plotBaseline[i] = chartFloat(v)
				}
				plotLower = make([]float64, len(baselineLowerValues))
				for i, v := range baselineLowerValues {
					plotLower[i] = math.Max(0.0, chartFloat(v))
				}
				plotUpper = make([]float64, len(baselineUpperValues))
				for i, v := range baselineUpperValues {
					plotUpper[i] = chartFloat(v)
				}
			}

			valuePoints := []any{}
			for idx := 0; idx < min(len(timeValues), len(plotValues)); idx++ {
				rank := 0
				if idx < len(rankSeries) {
					rank = rankSeries[idx]
				}
				valuePoints = append(valuePoints, []any{timeValues[idx], plotValues[idx], rank})
			}
			baselineMeanPoints := []any{}
			for idx := 0; idx < min(len(timeValues), len(plotBaseline)); idx++ {
				baselineMeanPoints = append(baselineMeanPoints, []any{timeValues[idx], plotBaseline[idx]})
			}
			baselineLowerPoints := []any{}
			for idx := 0; idx < min(len(timeValues), len(plotLower)); idx++ {
				baselineLowerPoints = append(baselineLowerPoints, []any{timeValues[idx], plotLower[idx]})
			}
			baselineUpperPoints := []any{}
			for idx := 0; idx < min(len(timeValues), len(plotUpper), len(plotLower)); idx++ {
				baselineUpperPoints = append(baselineUpperPoints, []any{timeValues[idx], math.Max(0.0, plotUpper[idx]-plotLower[idx])})
			}

			warningPoints := []any{}
			outlierPoints := []any{}
			for _, ptRaw := range valuePoints {
				pt, _ := ptRaw.([]any)
				if len(pt) >= 3 {
					rank, _ := pt[2].(int)
					if rank == 1 {
						warningPoints = append(warningPoints, []any{pt[0], pt[1]})
					} else if rank == 2 {
						outlierPoints = append(outlierPoints, []any{pt[0], pt[1]})
					}
				}
			}

			markAreas := []any{}
			if es, ok := effectiveStates.([]any); ok && len(timeValues) > 0 {
				i := 0
				lim := min(len(es), len(timeValues))
				for i < lim {
					state := chartPyStr(es[i])
					if state == "normal" {
						i++
						continue
					}
					startIdx := i
					for i+1 < len(es) && chartPyStr(es[i+1]) == state {
						i++
					}
					endIdx := i
					shade := "rgba(220, 53, 69, 0.15)"
					if state == "warning" {
						shade = "rgba(255, 193, 7, 0.15)"
					}
					markAreas = append(markAreas, []any{
						map[string]any{
							"name":      chartTitle(state),
							"itemStyle": map[string]any{"color": shade},
							"xAxis":     timeValues[startIdx],
						},
						map[string]any{"xAxis": timeValues[endIdx]},
					})
					i++
				}
			}

			latestValue := 0.0
			if len(valueValues) > 0 {
				latestValue = chartFloat(valueValues[len(valueValues)-1])
			}
			latestBaseline := 0.0
			if len(baselineMeanValues) > 0 {
				latestBaseline = chartFloat(baselineMeanValues[len(baselineMeanValues)-1])
			}
			deltaPct := 0.0
			if math.Abs(latestBaseline) > 1e-9 {
				deltaPct = ((latestValue - latestBaseline) / math.Abs(latestBaseline)) * 100.0
			}
			warningCount := len(warningPoints)
			outlierCount := len(outlierPoints)
			bindings["signal_summary"] = fmt.Sprintf(
				"now %.1f | baseline %.1f | Δ %+.0f%% | warn %d | outlier %d",
				latestValue, latestBaseline, deltaPct, warningCount, outlierCount,
			)

			bindings["value_points"] = valuePoints
			bindings["baseline_mean_points"] = baselineMeanPoints
			bindings["baseline_lower_points"] = baselineLowerPoints
			bindings["baseline_upper_points"] = baselineUpperPoints
			bindings["anomaly_mark_areas"] = markAreas
			bindings["warning_points"] = warningPoints
			bindings["outlier_points"] = outlierPoints
		}
	}

	return bindings
}

// formatDrilldownTime mirrors _format_drilldown_time: canonical ISO-8601 UTC.
func formatDrilldownTime(value any) string {
	if t, ok := value.(time.Time); ok {
		return t.UTC().Format("2006-01-02T15:04:05Z")
	}
	raw := strings.TrimSpace(rowString(value))
	if raw == "" {
		return ""
	}
	dt, err := parseIsoTimestamp(strings.ReplaceAll(raw, "Z", "+00:00"))
	if err != nil {
		dt, err = parseIsoTimestamp(normalizeChTimestamp(raw))
		if err != nil {
			return raw
		}
	}
	return dt.UTC().Format("2006-01-02T15:04:05Z")
}

// chartAsRow returns a Row view of dict-shaped rows, else nil.
func chartAsRow(v any) Row {
	switch r := v.(type) {
	case Row:
		return r
	}
	return nil
}

// chartPickAt mirrors `list[idx] if isinstance(list, list) and idx < len(list)
// else default`.
func chartPickAt(list []any, idx int, def any) any {
	if list != nil && idx >= 0 && idx < len(list) {
		return list[idx]
	}
	return def
}

// attachDrilldownMetadata mirrors _attach_drilldown_metadata.
func attachDrilldownMetadata(template map[string]any, bindings map[string]any, option map[string]any) map[string]any {
	drilldown, ok := template["drilldown"].(map[string]any)
	if !ok {
		return option
	}
	series, ok := option["series"].([]any)
	if !ok {
		return option
	}

	templateId := chartPyStr(template["id"])
	bucketSeconds := drilldown["bucket_seconds"]

	timeTemplates := map[string]bool{
		"time_series_percentiles": true,
		"dual_axis_anomaly":       true,
		"anomaly_overlay":         true,
		"derived_signal_overlay":  true,
	}
	if timeTemplates[templateId] {
		timeValues, ok := bindings["time"].([]any)
		if ok {
			isAnomalyTemplate := templateId == "anomaly_overlay" || templateId == "derived_signal_overlay"
			isDerived := templateId == "derived_signal_overlay"
			getList := func(key string) []any { l, _ := bindings[key].([]any); return l }

			var anomalyStates, anomalyScores []any
			if isAnomalyTemplate {
				anomalyStates = getList("anomaly_state")
				anomalyScores = getList("anomaly_score")
			}
			var ruleStates, ruleNames, ruleReasons, effectiveStates, services, sources, signals, attrFps []any
			if isDerived {
				ruleStates = getList("rule_state")
				ruleNames = getList("rule_name")
				ruleReasons = getList("rule_reason")
				effectiveStates = getList("effective_state")
				services = getList("service")
				sources = getList("source")
				signals = getList("signal")
				attrFps = getList("attr_fp")
			}

			for _, seRaw := range series {
				seriesEntry, ok := seRaw.(map[string]any)
				if !ok {
					continue
				}
				data, ok := seriesEntry["data"].([]any)
				if !ok || len(data) != len(timeValues) {
					continue
				}
				isValueSeries := isAnomalyTemplate && chartPyStr(seriesEntry["name"]) == "Value"
				newData := make([]any, len(data))
				for idx, value := range data {
					dd := map[string]any{
						"from_ts":  formatDrilldownTime(timeValues[idx]),
						"window_s": bucketSeconds,
					}
					if isValueSeries {
						dd["_anomaly_state"] = chartPickAt(anomalyStates, idx, "normal")
						dd["_anomaly_score"] = chartPickAt(anomalyScores, idx, 0)
						if isDerived {
							dd["_rule_state"] = chartPickAt(ruleStates, idx, "normal")
							dd["_rule_name"] = chartPickAt(ruleNames, idx, "")
							dd["_rule_reason"] = chartPickAt(ruleReasons, idx, "")
							dd["_effective_state"] = chartPickAt(effectiveStates, idx, "normal")
							dd["service"] = chartPickAt(services, idx, "")
							dd["source"] = chartPickAt(sources, idx, "")
							dd["signal"] = chartPickAt(signals, idx, "")
							dd["attr_fp"] = chartPickAt(attrFps, idx, "")
						}
					}
					newData[idx] = map[string]any{"value": value, "drilldown": dd}
				}
				seriesEntry["data"] = newData
			}
		}
		return option
	}

	if templateId == "heatmap" && len(series) > 0 {
		xUnique, xok := bindings["x_unique_values"].([]any)
		yUnique, yok := bindings["y_unique_values"].([]any)
		firstSeries, fok := series[0].(map[string]any)
		if fok && xok && yok {
			if data, ok := firstSeries["data"].([]any); ok {
				drilldownData := []any{}
				for _, itemRaw := range data {
					item, isL := itemRaw.([]any)
					if !isL || len(item) < 3 {
						drilldownData = append(drilldownData, itemRaw)
						continue
					}
					xIdx := int(chartFloat(item[0]))
					yIdx := int(chartFloat(item[1]))
					var fromValue any = ""
					if yIdx >= 0 && yIdx < len(yUnique) {
						fromValue = yUnique[yIdx]
					}
					var serviceValue any = ""
					if xIdx >= 0 && xIdx < len(xUnique) {
						serviceValue = xUnique[xIdx]
					}
					drilldownData = append(drilldownData, map[string]any{
						"value": item,
						"drilldown": map[string]any{
							"from_ts":  formatDrilldownTime(fromValue),
							"window_s": bucketSeconds,
							"service":  serviceValue,
						},
					})
				}
				firstSeries["data"] = drilldownData
			}
		}
		return option
	}

	return option
}

// prepareTemplateRows mirrors _prepare_template_rows.
func prepareTemplateRows(templateId string, columns []string, rows []any, roleIndices map[string]int) ([]string, []any) {
	if templateId != "derived_signal_overlay" {
		return columns, rows
	}

	requiredColumns := []string{
		"time", "service", "source", "signal", "attr_fp", "value",
		"sample_count", "baseline_mean", "baseline_lower", "baseline_upper",
		"anomaly_state", "anomaly_score",
	}
	if len(columns) < len(requiredColumns) {
		return columns, rows
	}

	colForRole := func(role string, fallbackIdx int) string {
		idx := fallbackIdx
		if roleIndices != nil {
			if v, ok := roleIndices[role]; ok {
				idx = v
			}
		}
		if idx >= 0 && idx < len(columns) {
			return columns[idx]
		}
		return columns[fallbackIdx]
	}

	roleColumns := map[string]string{
		"time":           colForRole("time", 0),
		"service":        colForRole("service", 1),
		"source":         colForRole("source", 2),
		"signal":         colForRole("signal", 3),
		"attr_fp":        colForRole("attr_fp", 4),
		"value":          colForRole("value", 5),
		"sample_count":   colForRole("sample_count", 6),
		"baseline_mean":  colForRole("baseline_mean", 7),
		"baseline_lower": colForRole("baseline_lower", 8),
		"baseline_upper": colForRole("baseline_upper", 9),
		"anomaly_state":  colForRole("anomaly_state", 10),
		"anomaly_score":  colForRole("anomaly_score", 11),
	}

	normalizedRows := make([]Row, 0, len(rows))
	for _, rawRow := range rows {
		rr := chartAsRow(rawRow)
		normalizedRows = append(normalizedRows, Row{
			"time":           rr[roleColumns["time"]],
			"service":        rr[roleColumns["service"]],
			"source":         rr[roleColumns["source"]],
			"signal":         rr[roleColumns["signal"]],
			"attr_fp":        rr[roleColumns["attr_fp"]],
			"value":          rr[roleColumns["value"]],
			"sample_count":   rr[roleColumns["sample_count"]],
			"baseline_mean":  rr[roleColumns["baseline_mean"]],
			"baseline_lower": rr[roleColumns["baseline_lower"]],
			"baseline_upper": rr[roleColumns["baseline_upper"]],
			"anomaly_state":  rr[roleColumns["anomaly_state"]],
			"anomaly_score":  rr[roleColumns["anomaly_score"]],
		})
	}

	rules, _ := loadAnomalyRules(getDb())
	annotateRowsWithRules(
		normalizedRows, rules,
		"source", "signal", "service", "attr_fp", "value", "sample_count", "time",
	)

	preparedColumns := append(append([]string{}, requiredColumns...), "rule_state", "rule_name", "rule_reason", "effective_state")
	preparedRows := make([]any, len(normalizedRows))
	for i, row := range normalizedRows {
		pr := Row{}
		for _, column := range preparedColumns {
			if v, ok := row[column]; ok {
				pr[column] = v
			} else {
				pr[column] = ""
			}
		}
		preparedRows[i] = pr
	}
	return preparedColumns, preparedRows
}

// renderChartFromTemplate mirrors _render_chart_from_template (ValueError ->
// error). Renders chart option by substituting query results into a template.
func renderChartFromTemplate(
	templateId string,
	columns []string,
	rows []any,
	spec map[string]any,
	namedDatasets map[string]map[string]any,
) (map[string]any, error) {
	template, ok := chartTemplates[templateId]
	if !ok {
		return nil, fmt.Errorf("Unknown template: %s", templateId)
	}

	if templateId == "custom_echarts" {
		return renderCustomEcharts(template, columns, rows, spec, namedDatasets)
	}

	if len(rows) == 0 {
		return map[string]any{
			"backgroundColor": "transparent",
			"textStyle":       map[string]any{"color": "#adb5bd"},
			"title": map[string]any{
				"text":      "No data for selected query/time window",
				"left":      "center",
				"top":       "middle",
				"textStyle": map[string]any{"color": "#6c757d", "fontSize": 13, "fontWeight": 500},
			},
			"series": []any{},
			"xAxis":  map[string]any{"show": false},
			"yAxis":  map[string]any{"show": false},
		}, nil
	}

	minCols := 0
	if f, ok := chartNumeric(template["min_columns"]); ok {
		minCols = int(f)
	}
	maxCols := 0
	hasMax := false
	if f, ok := chartNumeric(template["max_columns"]); ok {
		maxCols = int(f)
		hasMax = true
	}
	if len(columns) < minCols {
		return nil, fmt.Errorf("Template %s requires at least %d columns, got %d", templateId, minCols, len(columns))
	}
	if hasMax && maxCols != 0 && len(columns) > maxCols {
		return nil, fmt.Errorf("Template %s accepts maximum %d columns, got %d", templateId, maxCols, len(columns))
	}

	roleIndices, err := resolveTemplateRoleIndices(templateId, template, columns, spec)
	if err != nil {
		return nil, err
	}

	if len(rows) > 0 && chartAsRow(rows[0]) != nil {
		columns, rows = prepareTemplateRows(templateId, columns, rows, roleIndices)
	}

	bindings := extractBindings(template, columns, rows, roleIndices)

	substituted := deepSubstitute(template["echarts_option_template"], bindings)
	option, isMap := substituted.(map[string]any)
	if !isMap {
		// PORT-NOTE: a dict echarts_option_template always substitutes to a dict.
		return nil, fmt.Errorf("rendered chart option is not an object")
	}
	option = attachDrilldownMetadata(template, bindings, option)
	if _, ok := option["backgroundColor"]; !ok {
		option["backgroundColor"] = "transparent"
	}
	if _, ok := option["textStyle"]; !ok {
		option["textStyle"] = map[string]any{"color": "#adb5bd"}
	}
	return option, nil
}

func parseCustomJsonConfig(raw any, fieldName string) (any, error) {
	switch raw.(type) {
	case map[string]any, []any:
		return raw, nil
	}
	if raw == nil {
		return map[string]any{}, nil
	}
	text := strings.TrimSpace(chartPyStr(raw))
	if text == "" {
		return map[string]any{}, nil
	}
	var parsed any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("%s must be valid JSON", fieldName)
	}
	return parsed, nil
}

func resolveCustomBindingExpr(expr any, columns []string, records []map[string]any, rows []any) (any, error) {
	if s, ok := expr.(string); ok {
		key := strings.TrimSpace(s)
		if key == "" {
			return nil, nil
		}
		if key == "columns" {
			return columns, nil
		}
		if key == "rows" {
			return rows, nil
		}
		if key == "records" {
			return records, nil
		}
		out := make([]any, len(records))
		for i, record := range records {
			out[i] = record[key]
		}
		return out, nil
	}

	exprMap, ok := expr.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("custom_mapping_json values must be strings or objects")
	}

	fromVal := exprMap["from"]
	fromStr := ""
	if fromVal != nil {
		fromStr = chartPyStr(fromVal)
	}
	if fromStr == "" {
		fromStr = "column"
	}
	mode := strings.ToLower(strings.TrimSpace(fromStr))
	switch mode {
	case "columns":
		return columns, nil
	case "rows":
		return rows, nil
	case "records":
		return records, nil
	case "literal":
		return exprMap["value"], nil
	case "column":
		name := ""
		if nameVal := exprMap["name"]; nameVal != nil {
			name = chartPyStr(nameVal)
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("custom_mapping_json column mapping requires a non-empty 'name'")
		}
		out := make([]any, len(records))
		for i, record := range records {
			out[i] = record[name]
		}
		return out, nil
	}
	return nil, fmt.Errorf("Unsupported custom mapping mode: %s", mode)
}

var customTemplateVarRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

func resolveTemplateString(value string, record map[string]any) string {
	return customTemplateVarRe.ReplaceAllStringFunc(value, func(match string) string {
		sub := customTemplateVarRe.FindStringSubmatch(match)
		key := strings.TrimSpace(sub[1])
		resolved, ok := record[key]
		if !ok || resolved == nil {
			return ""
		}
		return chartPyStr(resolved)
	})
}

func buildCustomDrilldown(mapping map[string]any, records []map[string]any) map[string]any {
	drilldownRaw, ok := mapping["_drilldown"].(map[string]any)
	if !ok {
		return nil
	}

	target := ""
	if pyTruthy(drilldownRaw["target"]) {
		target = chartPyStr(drilldownRaw["target"])
	}
	target = strings.TrimSpace(target)
	switch target {
	case "logs", "metrics", "traces", "errors":
	default:
		return nil
	}

	firstRecord := map[string]any{}
	if len(records) > 0 {
		firstRecord = records[0]
	}
	label := ""
	if pyTruthy(drilldownRaw["label"]) {
		label = chartPyStr(drilldownRaw["label"])
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "Open Source View"
	}

	extra := map[string]any{}
	if extraRaw, ok := drilldownRaw["extra"].(map[string]any); ok {
		for k, v := range extraRaw {
			key := strings.TrimSpace(k)
			if key == "" {
				continue
			}
			if vs, ok := v.(string); ok {
				extra[key] = resolveTemplateString(vs, firstRecord)
			} else {
				extra[key] = v
			}
		}
	}

	out := map[string]any{"target": target, "label": label}
	for _, optionalKey := range []string{"bucket_seconds", "time_axis", "service_axis"} {
		if val, ok := drilldownRaw[optionalKey]; ok {
			out[optionalKey] = val
		}
	}
	if len(extra) > 0 {
		out["extra"] = extra
	}
	return out
}

// normalizeCustomSeriesPointOrder ensures deterministic ordering for tuple-like
// series points in custom ECharts.
func normalizeCustomSeriesPointOrder(option map[string]any) {
	series, ok := option["series"].([]any)
	if !ok {
		return
	}
	for _, entryAny := range series {
		entry, ok := entryAny.(map[string]any)
		if !ok {
			continue
		}
		data, ok := entry["data"].([]any)
		if !ok || len(data) < 2 {
			continue
		}
		allPoints := true
		for _, point := range data {
			pl, ok := point.([]any)
			if !ok || len(pl) < 2 {
				allPoints = false
				break
			}
		}
		if !allPoints {
			continue
		}
		sort.SliceStable(data, func(i, j int) bool {
			pi := data[i].([]any)
			pj := data[j].([]any)
			return chartSortKeyLess(pi[0], pj[0])
		})
	}
}

// chartSortKey mirrors _normalize_custom_series_point_order._to_sort_key.
func chartSortKey(value any) (int, time.Time, float64, string) {
	if t, ok := value.(time.Time); ok {
		return 0, t, 0, ""
	}
	if f, ok := chartNumeric(value); ok {
		return 1, time.Time{}, f, ""
	}
	if s, ok := value.(string); ok {
		text := strings.TrimSpace(s)
		if t := parseIsoDatetime(strings.Replace(text, "Z", "+00:00", 1)); t != nil {
			return 0, *t, 0, ""
		}
		return 2, time.Time{}, 0, text
	}
	return 3, time.Time{}, 0, chartPyStr(value)
}

func chartSortKeyLess(a, b any) bool {
	ga, ta, fa, sa := chartSortKey(a)
	gb, tb, fb, sb := chartSortKey(b)
	if ga != gb {
		return ga < gb
	}
	switch ga {
	case 0:
		return ta.Before(tb)
	case 1:
		return fa < fb
	default:
		return sa < sb
	}
}

func renderCustomEcharts(
	template map[string]any,
	columns []string,
	rows []any,
	spec map[string]any,
	namedDatasets map[string]map[string]any,
) (map[string]any, error) {
	visualDict := map[string]any{}
	if spec != nil {
		if v, ok := spec["visual"].(map[string]any); ok {
			visualDict = v
		}
	}

	mappingRaw, err := parseCustomJsonConfig(visualDict["custom_mapping_json"], "visual.custom_mapping_json")
	if err != nil {
		return nil, err
	}
	mapping, ok := mappingRaw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("visual.custom_mapping_json must be a JSON object")
	}

	var optionTemplate any
	optionRawCfg := visualDict["custom_option_json"]
	isEmptyStr := false
	if s, ok := optionRawCfg.(string); ok && strings.TrimSpace(s) == "" {
		isEmptyStr = true
	}
	if optionRawCfg == nil || isEmptyStr {
		ot := template["echarts_option_template"]
		if ot == nil {
			ot = map[string]any{}
		}
		optionTemplate = chartDeepCopy(ot)
	} else {
		optionTemplate, err = parseCustomJsonConfig(optionRawCfg, "visual.custom_option_json")
		if err != nil {
			return nil, err
		}
	}
	optionTemplateMap, ok := optionTemplate.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("visual.custom_option_json must be a JSON object")
	}

	records := []map[string]any{}
	for _, rowAny := range rows {
		if rowMap := chartAsRow(rowAny); rowMap != nil {
			rec := map[string]any{}
			for _, k := range columns {
				rec[k] = rowMap[k]
			}
			records = append(records, rec)
			continue
		}
		if rowList, ok := rowAny.([]any); ok {
			rec := map[string]any{}
			for idx, col := range columns {
				if idx < len(rowList) {
					rec[col] = rowList[idx]
				} else {
					rec[col] = nil
				}
			}
			records = append(records, rec)
		}
	}

	rows2d := make([]any, len(records))
	for i, record := range records {
		rowVals := make([]any, len(columns))
		for j, col := range columns {
			rowVals[j] = record[col]
		}
		rows2d[i] = rowVals
	}

	bindings := map[string]any{
		"columns": columns,
		"records": records,
		"rows":    rows2d,
	}
	for key, expr := range mapping {
		bindingKey := strings.TrimSpace(key)
		if bindingKey == "" {
			continue
		}
		if strings.HasPrefix(bindingKey, "_") {
			continue
		}
		val, err := resolveCustomBindingExpr(expr, columns, records, rows2d)
		if err != nil {
			return nil, err
		}
		bindings[bindingKey] = val
	}

	// Expose named dataset results as {{rows:name}}, {{records:name}}, {{columns:name}}
	for dsName, dsData := range namedDatasets {
		if dsData == nil {
			continue
		}
		dsColumns := dsData["columns"]
		if !pyTruthy(dsColumns) {
			dsColumns = []any{}
		}
		dsRecords := dsData["records"]
		if !pyTruthy(dsRecords) {
			dsRecords = []any{}
		}
		dsRows := dsData["rows"]
		if !pyTruthy(dsRows) {
			dsRows = []any{}
		}
		bindings["rows:"+dsName] = dsRows
		bindings["records:"+dsName] = dsRecords
		bindings["columns:"+dsName] = dsColumns
	}

	substituted := deepSubstitute(optionTemplateMap, bindings)
	option, ok := substituted.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Custom ECharts option must resolve to a JSON object")
	}

	if _, ok := option["backgroundColor"]; !ok {
		option["backgroundColor"] = "transparent"
	}
	if _, ok := option["textStyle"]; !ok {
		option["textStyle"] = map[string]any{"color": "#adb5bd"}
	}

	normalizeCustomSeriesPointOrder(option)

	drilldown := buildCustomDrilldown(mapping, records)
	if drilldown != nil {
		option["_customDrilldown"] = drilldown
	}
	return option, nil
}

func getDashboards(db *ChDbConnection) []map[string]any {
	out := []map[string]any{}
	res, err := db.Execute(
		"SELECT Id, Name, Description FROM sobs_dashboards FINAL WHERE IsDeleted = 0 ORDER BY Name",
	)
	if err != nil {
		return out
	}
	for _, r := range res.Fetchall() {
		out = append(out, map[string]any{
			"id":          rowString(r["Id"]),
			"name":        rowString(r["Name"]),
			"description": rowString(r["Description"]),
		})
	}
	return out
}

func getDashboard(db *ChDbConnection, dashboardId string) map[string]any {
	res, err := db.Execute(
		"SELECT Id, Name, Description FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Id = ?",
		dashboardId,
	)
	if err != nil {
		return nil
	}
	row := res.Fetchone()
	if row == nil {
		return nil
	}
	return map[string]any{
		"id":          rowString(row["Id"]),
		"name":        rowString(row["Name"]),
		"description": rowString(row["Description"]),
	}
}

func getCharts(db *ChDbConnection, dashboardId string) []map[string]any {
	charts := []map[string]any{}
	res, err := db.Execute(
		"SELECT Id, Title, ChartType, Query, OptionsJson, Position "+
			"FROM sobs_chart_configs FINAL WHERE IsDeleted = 0 AND DashboardId = ? "+
			"ORDER BY Position, Id",
		dashboardId,
	)
	if err != nil {
		return charts
	}
	for _, r := range res.Fetchall() {
		chartType := rowString(r["ChartType"])
		query := rowString(r["Query"])
		optionsJson := rowString(r["OptionsJson"])
		chartSpec := buildRawChartSpec(chartType, query, optionsJson)
		optionsJson = jsonDumpsNoEscape(map[string]any{"chart_spec": chartSpec})

		charts = append(charts, map[string]any{
			"id":           rowString(r["Id"]),
			"title":        rowString(r["Title"]),
			"chart_type":   chartType,
			"query":        query,
			"options_json": optionsJson,
			"position":     coerceInt(r["Position"]),
			"chart_spec":   chartSpec,
		})
	}
	return charts
}

// nextChartPosition mirrors max((c["position"] for c in charts), default=-1) + 1.
func nextChartPosition(charts []map[string]any) int {
	position := -1
	for _, c := range charts {
		if p := coerceInt(c["position"]); p > position {
			position = p
		}
	}
	return position + 1
}

func apiDashboardsList(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	dashboards := getDashboards(db)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "dashboards": dashboards})
}

func apiQueryAddToDashboard(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)

	dashboardId := strings.TrimSpace(rowString(payload["dashboard_id"]))
	title := strings.TrimSpace(rowString(payload["title"]))
	sql := strings.TrimSpace(rowString(payload["sql"]))
	chartSpecRaw := payload["chart_spec"]

	if dashboardId == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "dashboard_id is required"})
		return
	}
	if sql == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "sql is required"})
		return
	}
	if !pyTruthy(chartSpecRaw) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "chart_spec is required"})
		return
	}

	db := getDb()
	dashboard := getDashboard(db, dashboardId)
	if dashboard == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Dashboard not found"})
		return
	}

	if title == "" {
		title = "Query Chart"
	}

	var chartOption any
	if s, ok := chartSpecRaw.(string); ok {
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&chartOption); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("chart_spec must be valid JSON: %s", err)})
			return
		}
	} else {
		chartOption = chartSpecRaw
	}
	chartOptionMap, ok := chartOption.(map[string]any)
	if !ok {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "chart_spec must be a JSON object"})
		return
	}

	specRaw := map[string]any{
		"template_id": "custom_echarts",
		"sql":         map[string]any{"mode": "raw", "override_sql": sql},
		"visual": map[string]any{
			"custom_option_json":  jsonDumpsNoEscape(chartOptionMap),
			"custom_mapping_json": "{}",
		},
	}
	templateId, query, normalizedSpec, err := compileChartSpec(specRaw)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("Chart spec error: %s", err)})
		return
	}

	optionsJson := jsonDumpsNoEscape(map[string]any{"chart_spec": normalizedSpec})
	existing := getCharts(db, dashboardId)
	position := nextChartPosition(existing)

	chartId := agentUuid4()
	version := time.Now().UnixMilli()
	_, _ = insertRowsJsonEachRow(db, "sobs_chart_configs", []Row{
		{
			"Id":          chartId,
			"DashboardId": dashboardId,
			"Title":       title,
			"ChartType":   templateId,
			"Query":       query,
			"OptionsJson": optionsJson,
			"Position":    position,
			"IsDeleted":   0,
			"Version":     version,
		},
	})

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":             true,
		"chart_id":       chartId,
		"dashboard_id":   dashboardId,
		"dashboard_name": dashboard["name"],
		"dashboard_url":  "/dashboards/" + dashboardId,
	})
}

// sortedChartTemplateIds mirrors sorted(CHART_TEMPLATES.items()) key ordering.
func sortedChartTemplateIds() []string {
	ids := make([]string, 0, len(chartTemplates))
	for id := range chartTemplates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func listDashboards(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	dashboards := getDashboards(db)
	renderTemplate(w, r, "custom_dashboards.html", map[string]any{"dashboards": dashboards})
}

func newDashboardForm(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, r, "custom_dashboards.html", map[string]any{"dashboards": []any{}, "show_new_form": true})
}

func createDashboard(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	description := strings.TrimSpace(r.FormValue("description"))
	if name == "" {
		flashMessage(w, r, "Dashboard name is required", "warning")
		http.Redirect(w, r, "/dashboards", http.StatusFound)
		return
	}
	dashboardId := agentUuid4()
	version := time.Now().UnixMilli()
	db := getDb()
	_, _ = insertRowsJsonEachRow(db, "sobs_dashboards", []Row{
		{"Id": dashboardId, "Name": name, "Description": description, "IsDeleted": 0, "Version": version},
	})
	http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
}

func viewCustomDashboard(w http.ResponseWriter, r *http.Request) {
	dashboardId := r.PathValue("dashboard_id")
	db := getDb()
	dashboard := getDashboard(db, dashboardId)
	if dashboard == nil {
		flashMessage(w, r, "Dashboard not found", "danger")
		http.Redirect(w, r, "/dashboards", http.StatusFound)
		return
	}
	charts := getCharts(db, dashboardId)
	// Convert chart_type to template metadata for frontend
	templates := []map[string]any{}
	for _, tid := range sortedChartTemplateIds() {
		t := chartTemplates[tid]
		queryShape := t["query_shape"]
		if queryShape == nil {
			queryShape = ""
		}
		sampleSql := t["sample_sql"]
		if sampleSql == nil {
			sampleSql = ""
		}
		templates = append(templates, map[string]any{
			"id":           tid,
			"name":         t["name"],
			"description":  t["description"],
			"icon":         t["icon"],
			"query_shape":  queryShape,
			"sample_sql":   sampleSql,
			"drilldown":    t["drilldown"],
			"default_spec": defaultChartSpec(tid),
		})
	}
	renderTemplate(w, r, "custom_dashboard_view.html", map[string]any{
		"dashboard": dashboard,
		"charts":    charts,
		"templates": templates,
	})
}

// registerHelpRoute mirrors _register_help_route: registers a basic-auth-guarded
// GET route that renders a static help template.
func registerHelpRoute(path, endpoint, templateName string) {
	tn := templateName
	registerRoute("GET", path, requireBasicAuth(func(w http.ResponseWriter, r *http.Request) {
		renderTemplate(w, r, tn, map[string]any{})
	}))
}

var helpRouteRegistry = [][3]string{
	{"/dashboards/help/chart-editor", "chart_editor_help", "chart_editor_help.html"},
	{"/metrics/help/rules", "metrics_rules_help", "metrics_rules_help.html"},
	{"/metrics/help/rules/auto", "auto_metrics_rules_help", "auto_metrics_rules_help.html"},
	{"/kubernetes/help", "kubernetes_help", "kubernetes_help.html"},
	{"/settings/help/data-management", "data_management_help", "data_management_help.html"},
	{"/settings/help", "settings_help", "settings_help.html"},
	{"/settings/help/masking", "masking_help", "masking_help.html"},
	{"/settings/help/ai", "settings_ai_help", "settings_ai_help.html"},
	{"/settings/help/agents", "settings_agents_help", "settings_agents_help.html"},
	{"/settings/help/notifications", "settings_notifications_help", "settings_notifications_help.html"},
	{"/settings/help/tags", "settings_tags_help", "settings_tags_help.html"},
	{"/settings/help/enrichment", "settings_enrichment_help", "settings_enrichment_help.html"},
	{"/settings/help/repositories", "settings_repositories_help", "settings_repositories_help.html"},
	{"/settings/help/kubernetes", "settings_kubernetes_help", "kubernetes_help.html"},
	{"/web-traffic/help", "web_traffic_help", "web_traffic_help.html"},
	{"/errors/help", "errors_help", "errors_help.html"},
	{"/table-explorer/help", "table_explorer_help", "table_explorer_help.html"},
	{"/setup/help/playbooks", "setup_playbooks_help", "setup_playbooks_help.html"},
	{"/logs/help", "logs_help", "logs_help.html"},
	{"/traces/help", "traces_help", "traces_help.html"},
	{"/rum/help", "rum_help", "rum_help.html"},
	{"/ai/help", "ai_help", "ai_help.html"},
	{"/cve/help", "cve_help", "cve_help.html"},
	{"/metrics/help", "metrics_help", "metrics_help.html"},
	{"/metrics/help/anomaly", "metrics_anomaly_help", "metrics_anomaly_help.html"},
	{"/query/help", "query_help", "query_help.html"},
	{"/reports/help", "reports_help", "reports_help.html"},
	{"/summary/help", "summary_help", "summary_help.html"},
	{"/work-items/help", "work_items_help", "work_items_help.html"},
	{"/incident/help", "incident_help", "incident_help.html"},
}

// softDeleteRedirectPath resolves the Flask endpoint names passed to
// softDeleteLatestRow into literal URL paths.
func softDeleteRedirectPath(endpoint string) string {
	switch endpoint {
	case "view_metrics_rules":
		return "/metrics/rules"
	case "view_tag_rules":
		return "/settings/tags"
	case "view_agent_rules":
		return "/settings/agents"
	case "list_dashboards":
		return "/dashboards"
	case "view_notifications":
		return "/settings/notifications"
	}
	return "/" + strings.ReplaceAll(endpoint, "_", "-")
}

// softDeleteLatestRow mirrors _soft_delete_latest_row: load the latest row,
// emit a tombstone (IsDeleted=1) and redirect with a flash message.
func softDeleteLatestRow(
	w http.ResponseWriter,
	r *http.Request,
	db *ChDbConnection,
	selectSql string,
	selectParams []any,
	tableName string,
	buildDeletedRow func(Row) Row,
	notFoundMessage string,
	successMessage string,
	redirectEndpoint string,
	notFoundCategory string,
	successCategory string,
) {
	redirectPath := softDeleteRedirectPath(redirectEndpoint)
	var row Row
	if res, err := db.Execute(selectSql, selectParams...); err == nil {
		row = res.Fetchone()
	}
	if row == nil {
		flashMessage(w, r, notFoundMessage, notFoundCategory)
		http.Redirect(w, r, redirectPath, http.StatusFound)
		return
	}

	payload := buildDeletedRow(row)
	payload["IsDeleted"] = 1
	payload["Version"] = time.Now().UnixMilli()
	_, _ = insertRowsJsonEachRow(db, tableName, []Row{payload})

	flashMessage(w, r, strings.ReplaceAll(successMessage, "{name}", rowString(row["Name"])), successCategory)
	http.Redirect(w, r, redirectPath, http.StatusFound)
}

func deleteDashboard(w http.ResponseWriter, r *http.Request) {
	dashboardId := r.PathValue("dashboard_id")
	db := getDb()
	dashboard := getDashboard(db, dashboardId)
	if dashboard == nil {
		flashMessage(w, r, "Dashboard not found", "danger")
		http.Redirect(w, r, "/dashboards", http.StatusFound)
		return
	}
	version := time.Now().UnixMilli()
	// Soft-delete dashboard
	_, _ = insertRowsJsonEachRow(db, "sobs_dashboards", []Row{
		{
			"Id":          dashboardId,
			"Name":        dashboard["name"],
			"Description": dashboard["description"],
			"IsDeleted":   1,
			"Version":     version,
		},
	})
	// Soft-delete all charts in this dashboard
	charts := getCharts(db, dashboardId)
	if len(charts) > 0 {
		tombstones := make([]Row, 0, len(charts))
		for _, c := range charts {
			tombstones = append(tombstones, Row{
				"Id":          c["id"],
				"DashboardId": dashboardId,
				"Title":       c["title"],
				"ChartType":   c["chart_type"],
				"Query":       c["query"],
				"OptionsJson": c["options_json"],
				"Position":    c["position"],
				"IsDeleted":   1,
				"Version":     version,
			})
		}
		_, _ = insertRowsJsonEachRow(db, "sobs_chart_configs", tombstones)
	}
	flashMessage(w, r, fmt.Sprintf("Dashboard '%s' deleted", rowString(dashboard["name"])), "success")
	http.Redirect(w, r, "/dashboards", http.StatusFound)
}

func addChart(w http.ResponseWriter, r *http.Request) {
	dashboardId := r.PathValue("dashboard_id")
	db := getDb()
	dashboard := getDashboard(db, dashboardId)
	if dashboard == nil {
		flashMessage(w, r, "Dashboard not found", "danger")
		http.Redirect(w, r, "/dashboards", http.StatusFound)
		return
	}
	_ = r.ParseForm()
	title, templateId, query, optionsJson, err := parseChartFormSubmission(r)
	if err != nil {
		flashMessage(w, r, err.Error(), "warning")
		http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
		return
	}
	existing := getCharts(db, dashboardId)
	position := nextChartPosition(existing)
	chartId := agentUuid4()
	version := time.Now().UnixMilli()
	_, _ = insertRowsJsonEachRow(db, "sobs_chart_configs", []Row{
		{
			"Id":          chartId,
			"DashboardId": dashboardId,
			"Title":       title,
			"ChartType":   templateId,
			"Query":       query,
			"OptionsJson": optionsJson,
			"Position":    position,
			"IsDeleted":   0,
			"Version":     version,
		},
	})
	http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
}

// parseChartFormSubmission mirrors _parse_chart_form_submission.
func parseChartFormSubmission(r *http.Request) (string, string, string, string, error) {
	title := strings.TrimSpace(r.FormValue("title"))
	chartSpecJson := strings.TrimSpace(r.FormValue("chart_spec_json"))

	if title == "" {
		return "", "", "", "", fmt.Errorf("Chart title is required")
	}
	if chartSpecJson == "" {
		return "", "", "", "", fmt.Errorf("Chart spec is required")
	}

	var specRaw any
	dec := json.NewDecoder(strings.NewReader(chartSpecJson))
	dec.UseNumber()
	if err := dec.Decode(&specRaw); err != nil {
		return "", "", "", "", fmt.Errorf("Chart spec error: %s", err)
	}
	templateId, query, normalizedSpec, err := compileChartSpec(specRaw)
	if err != nil {
		return "", "", "", "", fmt.Errorf("Chart spec error: %s", err)
	}

	optionsJson := jsonDumpsNoEscape(map[string]any{"chart_spec": normalizedSpec})
	return title, templateId, query, optionsJson, nil
}

func editChart(w http.ResponseWriter, r *http.Request) {
	dashboardId := r.PathValue("dashboard_id")
	chartId := r.PathValue("chart_id")
	db := getDb()
	dashboard := getDashboard(db, dashboardId)
	if dashboard == nil {
		flashMessage(w, r, "Dashboard not found", "danger")
		http.Redirect(w, r, "/dashboards", http.StatusFound)
		return
	}

	charts := getCharts(db, dashboardId)
	var chart map[string]any
	for _, c := range charts {
		if rowString(c["id"]) == chartId {
			chart = c
			break
		}
	}
	if chart == nil {
		flashMessage(w, r, "Chart not found", "warning")
		http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
		return
	}

	_ = r.ParseForm()
	title, templateId, query, optionsJson, err := parseChartFormSubmission(r)
	if err != nil {
		flashMessage(w, r, err.Error(), "warning")
		http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
		return
	}

	version := time.Now().UnixMilli()
	_, _ = insertRowsJsonEachRow(db, "sobs_chart_configs", []Row{
		{
			"Id":          chartId,
			"DashboardId": dashboardId,
			"Title":       title,
			"ChartType":   templateId,
			"Query":       query,
			"OptionsJson": optionsJson,
			"Position":    chart["position"],
			"IsDeleted":   0,
			"Version":     version,
		},
	})
	http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
}

func cloneChart(w http.ResponseWriter, r *http.Request) {
	dashboardId := r.PathValue("dashboard_id")
	chartId := r.PathValue("chart_id")
	db := getDb()
	dashboard := getDashboard(db, dashboardId)
	if dashboard == nil {
		flashMessage(w, r, "Dashboard not found", "danger")
		http.Redirect(w, r, "/dashboards", http.StatusFound)
		return
	}

	charts := getCharts(db, dashboardId)
	var sourceChart map[string]any
	for _, c := range charts {
		if rowString(c["id"]) == chartId {
			sourceChart = c
			break
		}
	}
	if sourceChart == nil {
		flashMessage(w, r, "Chart not found", "warning")
		http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
		return
	}

	_ = r.ParseForm()
	title, templateId, query, optionsJson, err := parseChartFormSubmission(r)
	if err != nil {
		flashMessage(w, r, err.Error(), "warning")
		http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
		return
	}

	position := nextChartPosition(charts)
	version := time.Now().UnixMilli()
	_, _ = insertRowsJsonEachRow(db, "sobs_chart_configs", []Row{
		{
			"Id":          agentUuid4(),
			"DashboardId": dashboardId,
			"Title":       title,
			"ChartType":   templateId,
			"Query":       query,
			"OptionsJson": optionsJson,
			"Position":    position,
			"IsDeleted":   0,
			"Version":     version,
		},
	})
	http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
}

func removeChart(w http.ResponseWriter, r *http.Request) {
	dashboardId := r.PathValue("dashboard_id")
	chartId := r.PathValue("chart_id")
	db := getDb()
	dashboard := getDashboard(db, dashboardId)
	if dashboard == nil {
		flashMessage(w, r, "Dashboard not found", "danger")
		http.Redirect(w, r, "/dashboards", http.StatusFound)
		return
	}
	charts := getCharts(db, dashboardId)
	var chart map[string]any
	for _, c := range charts {
		if rowString(c["id"]) == chartId {
			chart = c
			break
		}
	}
	if chart == nil {
		flashMessage(w, r, "Chart not found", "warning")
		http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
		return
	}
	version := time.Now().UnixMilli()
	_, _ = insertRowsJsonEachRow(db, "sobs_chart_configs", []Row{
		{
			"Id":          chartId,
			"DashboardId": dashboardId,
			"Title":       chart["title"],
			"ChartType":   chart["chart_type"],
			"Query":       chart["query"],
			"OptionsJson": chart["options_json"],
			"Position":    chart["position"],
			"IsDeleted":   1,
			"Version":     version,
		},
	})
	http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
}

var chartLimitRe = regexp.MustCompile(`(?i)\bLIMIT\b`)

func executeChartQuery(w http.ResponseWriter, r *http.Request) {
	body, _ := readJsonBody(r)
	query := strings.TrimSpace(rowString(body["query"]))
	if !pyTruthy(body["query"]) {
		query = ""
	}
	if errMsg := validateChartQuery(query); errMsg != "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": errMsg})
		return
	}
	// Inject a row limit to prevent runaway queries
	if !chartLimitRe.MatchString(query) {
		query = strings.TrimRight(query, ";") + " LIMIT 1000"
	}
	db := getDb()
	result, err := db.Execute(query)
	if err != nil {
		logger.Error("Chart query execution failed", "query", query, "error", err)
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": publicDashboardQueryError(err)})
		return
	}
	rows := result.Fetchall()
	columns := []string{}
	if len(rows) > 0 {
		columns = result.Cols
	}
	data := make([]any, 0, len(rows))
	for _, row := range rows {
		vals := make([]any, len(columns))
		for i, col := range columns {
			vals[i] = row[col]
		}
		data = append(data, vals)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"columns": columns, "rows": data})
}

func listChartSpecTemplates(w http.ResponseWriter, r *http.Request) {
	templates := []map[string]any{}
	for _, tid := range sortedChartTemplateIds() {
		t := chartTemplates[tid]
		queryShape := t["query_shape"]
		if queryShape == nil {
			queryShape = ""
		}
		sampleSql := t["sample_sql"]
		if sampleSql == nil {
			sampleSql = ""
		}
		minColumns := t["min_columns"]
		if minColumns == nil {
			minColumns = 0
		}
		columnRoles := t["column_roles"]
		if columnRoles == nil {
			columnRoles = map[string]any{}
		}
		templates = append(templates, map[string]any{
			"id":           tid,
			"name":         t["name"],
			"description":  t["description"],
			"query_shape":  queryShape,
			"sample_sql":   sampleSql,
			"default_spec": defaultChartSpec(tid),
			"min_columns":  minColumns,
			"max_columns":  t["max_columns"],
			"column_roles": columnRoles,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"templates": templates})
}

func chartSpecOptionsApi(w http.ResponseWriter, r *http.Request) {
	sourceView := strings.TrimSpace(r.URL.Query().Get("source_view"))
	if sourceView == "" {
		sourceView = "v_derived_signals_anomaly"
	}
	signalSource := strings.TrimSpace(r.URL.Query().Get("signal_source"))
	limit := coercePositiveInt(r.URL.Query().Get("limit"), 100, 1, 500)

	supportedSources := map[string]bool{
		"v_derived_signals_anomaly": true,
		"v_otel_metrics_anomaly":    true,
		"otel_metrics_gauge":        true,
		"otel_metrics_sum":          true,
		"otel_metrics_histogram":    true,
		"otel_logs":                 true,
		"otel_traces":               true,
		"sobs_error_resolutions":    true,
	}
	if !supportedSources[sourceView] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "Unsupported source for options"})
		return
	}

	db := getDb()

	distinctValues := func(query string) []string {
		values := []string{}
		res, err := db.Execute(query)
		if err != nil {
			return values
		}
		for _, row := range res.Fetchall() {
			val := ""
			if pyTruthy(row["v"]) {
				val = rowString(row["v"])
			}
			val = strings.TrimSpace(val)
			if val != "" {
				values = append(values, val)
			}
		}
		return values
	}

	services := []string{}
	signals := []string{}
	metrics := []string{}

	limitStr := strconv.Itoa(limit)
	switch {
	case sourceView == "v_derived_signals_anomaly":
		services = distinctValues(
			"SELECT DISTINCT ServiceName AS v " +
				"FROM v_derived_signals_anomaly " +
				"WHERE time >= now() - INTERVAL 24 HOUR " +
				"ORDER BY v " +
				"LIMIT " + limitStr)
		signalsQuery := "SELECT DISTINCT SignalName AS v " +
			"FROM v_derived_signals_anomaly " +
			"WHERE time >= now() - INTERVAL 24 HOUR"
		if signalSource != "" {
			signalsQuery += " AND SignalSource = " + sqlLiteral(signalSource)
		}
		signalsQuery += " ORDER BY v LIMIT " + limitStr
		signals = distinctValues(signalsQuery)
	case sourceView == "otel_logs" || sourceView == "otel_traces":
		services = distinctValues(
			"SELECT DISTINCT ServiceName AS v " + "FROM " + sourceView + " " + "ORDER BY v " + "LIMIT " + limitStr)
		if sourceView == "otel_logs" {
			signals = []string{"log_volume"}
		} else {
			signals = []string{"trace_volume"}
		}
	case sourceView == "sobs_error_resolutions":
		signals = []string{"resolved_error_volume"}
	case sourceView == "v_otel_metrics_anomaly" || sourceView == "otel_metrics_gauge" ||
		sourceView == "otel_metrics_sum" || sourceView == "otel_metrics_histogram":
		services = distinctValues(
			"SELECT DISTINCT ServiceName AS v " + "FROM " + sourceView + " " + "ORDER BY v " + "LIMIT " + limitStr)
		metrics = distinctValues(
			"SELECT DISTINCT MetricName AS v " + "FROM " + sourceView + " " + "ORDER BY v " + "LIMIT " + limitStr)
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"source_view": sourceView,
		"services":    services,
		"signals":     signals,
		"metrics":     metrics,
	})
}

func compileChartSpecApi(w http.ResponseWriter, r *http.Request) {
	body, _ := readJsonBody(r)
	spec := body["spec"]
	templateId, query, normalizedSpec, err := compileChartSpec(spec)
	if err != nil {
		// PORT-NOTE: Python distinguishes ValueError (str(ve)) from generic
		// exceptions (_public_dashboard_query_error); both return HTTP 400.
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	jsonifyWithOptionalSqlOutputMask(w, map[string]any{"template_id": templateId, "query": query, "spec": normalizedSpec})
}

// executeChartSpecNamedQueries mirrors _execute_chart_spec_named_queries.
func executeChartSpecNamedQueries(db *ChDbConnection, namedQueries any, defaultLimit int, includeRecords bool) []map[string]any {
	results := []map[string]any{}
	nqList, ok := namedQueries.([]any)
	if !ok {
		return results
	}
	for _, nqAny := range nqList {
		nq, ok := nqAny.(map[string]any)
		if !ok {
			continue
		}
		nqName := ""
		if pyTruthy(nq["name"]) {
			nqName = rowString(nq["name"])
		}
		nqName = strings.TrimSpace(nqName)
		nqSql := ""
		if pyTruthy(nq["sql"]) {
			nqSql = rowString(nq["sql"])
		}
		nqSql = strings.TrimSpace(nqSql)
		if nqName == "" || nqSql == "" {
			continue
		}
		purpose := ""
		if pyTruthy(nq["purpose"]) {
			purpose = rowString(nq["purpose"])
		}
		nqRun := nqSql
		if !chartLimitRe.MatchString(nqSql) {
			nqRun = nqSql + " LIMIT " + strconv.Itoa(defaultLimit)
		}
		nqResult, err := db.Execute(nqRun)
		if err != nil {
			item := map[string]any{
				"name":    nqName,
				"purpose": purpose,
				"columns": []string{},
				"rows":    []any{},
				"error":   publicDashboardQueryError(err),
			}
			if includeRecords {
				item["records"] = []any{}
			}
			results = append(results, item)
			continue
		}
		nqRows := nqResult.Fetchall()
		nqColumns := []string{}
		if len(nqRows) > 0 {
			nqColumns = nqResult.Cols
		}
		nqData := make([]any, 0, len(nqRows))
		for _, row := range nqRows {
			vals := make([]any, len(nqColumns))
			for i, col := range nqColumns {
				vals[i] = row[col]
			}
			nqData = append(nqData, vals)
		}
		item := map[string]any{
			"name":    nqName,
			"purpose": purpose,
			"columns": nqColumns,
			"rows":    nqData,
			"error":   "",
		}
		if includeRecords {
			records := make([]any, 0, len(nqRows))
			for _, row := range nqRows {
				rec := map[string]any{}
				for k, v := range row {
					rec[k] = v
				}
				records = append(records, rec)
			}
			item["records"] = records
		}
		results = append(results, item)
	}
	return results
}

func dryRunChartSpecApi(w http.ResponseWriter, r *http.Request) {
	body, _ := readJsonBody(r)
	spec := body["spec"]
	templateId, query, normalizedSpec, err := compileChartSpec(spec)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	runQuery := query
	if !chartLimitRe.MatchString(runQuery) {
		runQuery = strings.TrimRight(runQuery, ";") + " LIMIT 20"
	}
	db := getDb()
	result, execErr := db.Execute(runQuery)
	if execErr != nil {
		logger.Error("Chart spec dry-run failed", "error", execErr)
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": publicDashboardQueryError(execErr)})
		return
	}
	rows := result.Fetchall()
	columns := []string{}
	if len(rows) > 0 {
		columns = result.Cols
	}
	data := make([]any, 0, len(rows))
	for _, row := range rows {
		vals := make([]any, len(columns))
		for i, col := range columns {
			vals[i] = row[col]
		}
		data = append(data, vals)
	}
	columnTypes := inferColumnTypes(columns, data)

	namedQueryResults := executeChartSpecNamedQueries(db, normalizedSpec["named_queries"], 5, false)

	jsonifyWithOptionalSqlOutputMask(w, map[string]any{
		"template_id":         templateId,
		"query":               query,
		"spec":                normalizedSpec,
		"columns":             columns,
		"column_types":        columnTypes,
		"rows":                data,
		"named_query_results": namedQueryResults,
	})
}

func validateChartSpecApi(w http.ResponseWriter, r *http.Request) {
	body, _ := readJsonBody(r)
	spec := body["spec"]
	templateId, query, normalizedSpec, err := compileChartSpec(spec)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"valid": false, "error": err.Error()})
		return
	}

	db := getDb()
	runQuery := query
	if !chartLimitRe.MatchString(runQuery) {
		runQuery = strings.TrimRight(runQuery, ";") + " LIMIT 200"
	}
	result, execErr := db.Execute(runQuery)
	if execErr != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"valid": false, "error": publicDashboardQueryError(execErr)})
		return
	}
	rawRows := result.Fetchall()
	columns := []string{}
	if len(rawRows) > 0 {
		columns = result.Cols
	}
	data := make([]any, 0, len(rawRows))
	for _, row := range rawRows {
		rec := map[string]any{}
		for k, v := range row {
			rec[k] = v
		}
		data = append(data, rec)
	}
	if _, rerr := renderChartFromTemplate(templateId, columns, data, normalizedSpec, nil); rerr != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"valid": false, "error": publicDashboardQueryError(rerr)})
		return
	}

	jsonifyWithOptionalSqlOutputMask(w, map[string]any{
		"valid":       true,
		"template_id": templateId,
		"query":       query,
		"spec":        normalizedSpec,
		"columns":     columns,
		"row_count":   len(data),
	})
}

func renderChart(w http.ResponseWriter, r *http.Request) {
	body, _ := readJsonBody(r)
	query := ""
	if pyTruthy(body["query"]) {
		query = rowString(body["query"])
	}
	query = strings.TrimSpace(query)
	templateId := "time_series_percentiles"
	if pyTruthy(body["template_id"]) {
		templateId = rowString(body["template_id"])
	}
	templateId = strings.TrimSpace(templateId)

	if errMsg := validateChartQuery(query); errMsg != "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": errMsg})
		return
	}
	if _, ok := chartTemplates[templateId]; !ok {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "Unknown template: " + templateId})
		return
	}

	// Inject a row limit to prevent runaway queries
	if !chartLimitRe.MatchString(query) {
		query = strings.TrimRight(query, ";") + " LIMIT 1000"
	}

	db := getDb()
	result, execErr := db.Execute(query)
	if execErr != nil {
		logger.Error("Chart render failed", "template", templateId, "query", query, "error", execErr)
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": publicDashboardQueryError(execErr)})
		return
	}
	rawRows := result.Fetchall()
	columns := []string{}
	if len(rawRows) > 0 {
		columns = result.Cols
	}
	data := make([]any, 0, len(rawRows))
	for _, row := range rawRows {
		rec := map[string]any{}
		for k, v := range row {
			rec[k] = v
		}
		data = append(data, rec)
	}

	option, rerr := renderChartFromTemplate(templateId, columns, data, nil, nil)
	if rerr != nil {
		// Template column mismatch (Python ValueError → str(ve)).
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": rerr.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"option": option})
}

func renderChartSpecApi(w http.ResponseWriter, r *http.Request) {
	body, _ := readJsonBody(r)
	spec := body["spec"]
	templateId, query, normalizedSpec, err := compileChartSpec(spec)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	db := getDb()
	runQuery := query
	if !chartLimitRe.MatchString(runQuery) {
		runQuery = strings.TrimRight(runQuery, ";") + " LIMIT 1000"
	}
	result, execErr := db.Execute(runQuery)
	if execErr != nil {
		logger.Error("Chart spec render failed", "error", execErr)
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": publicDashboardQueryError(execErr)})
		return
	}
	rawRows := result.Fetchall()
	columns := []string{}
	if len(rawRows) > 0 {
		columns = result.Cols
	}
	data := make([]any, 0, len(rawRows))
	for _, row := range rawRows {
		rec := map[string]any{}
		for k, v := range row {
			rec[k] = v
		}
		data = append(data, rec)
	}

	// Execute named queries and collect datasets
	namedDatasets := map[string]map[string]any{}
	namedQueryResults := executeChartSpecNamedQueries(db, normalizedSpec["named_queries"], 1000, true)
	for _, nq := range namedQueryResults {
		nqName := ""
		if pyTruthy(nq["name"]) {
			nqName = rowString(nq["name"])
		}
		nqName = strings.TrimSpace(nqName)
		if nqName == "" {
			continue
		}
		if pyTruthy(nq["error"]) {
			logger.Warn("Named query failed during render", "name", nqName, "error", nq["error"])
		}
		nqColumns := nq["columns"]
		if !pyTruthy(nqColumns) {
			nqColumns = []any{}
		}
		nqRecords := nq["records"]
		if !pyTruthy(nqRecords) {
			nqRecords = []any{}
		}
		nqRows := nq["rows"]
		if !pyTruthy(nqRows) {
			nqRows = []any{}
		}
		namedDatasets[nqName] = map[string]any{
			"columns": nqColumns,
			"records": nqRecords,
			"rows":    nqRows,
		}
	}

	option, rerr := renderChartFromTemplate(templateId, columns, data, normalizedSpec, namedDatasets)
	if rerr != nil {
		logger.Error("Chart spec render failed", "error", rerr)
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": publicDashboardQueryError(rerr)})
		return
	}
	option = applyChartSpecVisualOverrides(templateId, option, normalizedSpec)

	jsonifyWithOptionalSqlOutputMask(w, map[string]any{
		"template_id": templateId,
		"query":       query,
		"spec":        normalizedSpec,
		"option":      option,
	})
}

// chartStrOr mirrors str(value or "").
func chartStrOr(v any) string {
	if pyTruthy(v) {
		return rowString(v)
	}
	return ""
}

func aiBuildChartSpec(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	question := strings.TrimSpace(chartStrOr(payload["question"]))
	preferredChartType := strings.TrimSpace(chartStrOr(payload["preferred_chart_type"]))
	chartInstruction := strings.TrimSpace(chartStrOr(payload["chart_instruction"]))
	thinkingLevelRaw := chartStrOr(payload["thinking_level"])
	if thinkingLevelRaw == "" {
		thinkingLevelRaw = "off"
	}
	thinkingLevel := normalizeThinkingLevel(thinkingLevelRaw)

	if question == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "question is required"})
		return
	}

	db := getDb()
	settings := loadAllAiSettings(db)
	endpointUrl := strings.TrimSpace(settings["ai.endpoint_url"])
	model := strings.TrimSpace(settings["ai.model"])
	if endpointUrl == "" || model == "" {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "AI endpoint not configured. Visit Settings → AI Configuration."})
		return
	}

	// Build schema context
	runner := newChdbSqlRunner(db)
	schemaContext := runner.getSchemaContext("default", 30)

	// Generate primary SQL
	sql, sqlErr, _ := vannaGenerateSql(question, schemaContext, settings, preferredChartType, chartInstruction, thinkingLevel)
	if sqlErr != "" {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "SQL generation failed: " + sqlErr})
		return
	}

	// Validate/execute primary SQL and auto-repair if needed.
	sql, primaryDf, primaryError, sqlRetryCount, _ := vannaValidateAndExecuteWithRepair(db, question, schemaContext, sql, settings, thinkingLevel)
	if primaryError != "" || primaryDf == nil {
		errMsg := primaryError
		if errMsg == "" {
			errMsg = "Generated SQL could not be executed."
		}
		jsonResponse(w, 422, map[string]any{"ok": false, "error": errMsg, "sql": sql})
		return
	}

	// Primary query data for chart generation context.
	columns := primaryDf.Columns
	rows := [][]any{}
	if len(primaryDf.Rows) > 0 {
		rows = jsonSafeRows(dfValues(primaryDf))
	}
	datasets := []map[string]any{
		{
			"name":    "main",
			"purpose": "primary dataset",
			"sql":     sql,
			"columns": columns,
			"rows":    rows,
		},
	}

	// Optionally generate named queries for complex multi-dataset charts
	namedQueryResults := []map[string]any{}
	if len(columns) > 0 {
		namedQueriesRaw, _, _ := vannaGenerateNamedQueries(question, schemaContext, sql, settings, preferredChartType, chartInstruction, thinkingLevel)
		namedQueryResults = vannaExecuteNamedQueries(db, namedQueriesRaw, question, schemaContext, settings, thinkingLevel, true, false)
		for _, nq := range namedQueryResults {
			if !pyTruthy(nq["error"]) {
				datasets = append(datasets, map[string]any{
					"name":    chartStrOr(nq["name"]),
					"purpose": chartStrOr(nq["purpose"]),
					"sql":     chartStrOr(nq["sql"]),
					"columns": orEmptyList(nq["columns"]),
					"rows":    orEmptyList(nq["rows"]),
				})
			}
		}
	}

	// Generate eCharts option JSON via LLM
	chartSpecJson := ""
	chartError := ""
	customMappingJson := "{}"
	if len(columns) > 0 {
		sample := buildSampleRows(columns, rows)
		chartSpecJson, chartError, _ = vannaGenerateChartSpec(columns, sample, question, settings, preferredChartType, chartInstruction, datasets, thinkingLevel)
		if chartSpecJson != "" {
			inferredMapping := inferCustomMappingFromOption(chartSpecJson, columns)
			if pyTruthy(inferredMapping) {
				customMappingJson = jsonDumpsNoEscape(inferredMapping)
			} else {
				customMappingJson = "{}"
			}
		} else {
			// Ensure the UI always gets a usable option JSON even when chart generation fails.
			chartSpecJson = buildFallbackCustomOptionJson()
			customMappingJson = jsonDumpsNoEscape(map[string]any{"points": map[string]any{"from": "rows"}})
			if chartError != "" {
				chartError = chartError + " Using fallback chart option template."
			} else {
				chartError = "Chart generation failed; using fallback chart option template."
			}
		}
	}

	namedQueries := []map[string]string{}
	for _, nq := range namedQueryResults {
		if !pyTruthy(nq["error"]) && pyTruthy(nq["name"]) && pyTruthy(nq["sql"]) {
			namedQueries = append(namedQueries, map[string]string{
				"name":    chartStrOr(nq["name"]),
				"sql":     chartStrOr(nq["sql"]),
				"purpose": chartStrOr(nq["purpose"]),
			})
		}
	}

	customOptionJson := chartSpecJson
	if customOptionJson == "" {
		customOptionJson = "{}"
	}
	spec := map[string]any{
		"template_id":   "custom_echarts",
		"sql":           map[string]any{"mode": "raw", "override_sql": sql},
		"named_queries": namedQueries,
		"visual": map[string]any{
			"custom_option_json":  customOptionJson,
			"custom_mapping_json": customMappingJson,
		},
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"spec":                spec,
		"sql":                 sql,
		"retry_count":         sqlRetryCount,
		"columns":             columns,
		"named_queries":       namedQueries,
		"named_query_results": namedQueryResults,
		"chart_error":         chartError,
	})
}

var chartFilenameRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func exportChart(w http.ResponseWriter, r *http.Request) {
	dashboardId := r.PathValue("dashboard_id")
	chartId := r.PathValue("chart_id")
	db := getDb()
	dashboard := getDashboard(db, dashboardId)
	if dashboard == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Dashboard not found"})
		return
	}

	charts := getCharts(db, dashboardId)
	var chart map[string]any
	for _, c := range charts {
		if rowString(c["id"]) == chartId {
			chart = c
			break
		}
	}
	if chart == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Chart not found"})
		return
	}

	templatePayload := map[string]any{
		"sobs_chart_template_version": 1,
		"title":                       chart["title"],
		"chart_spec":                  chart["chart_spec"],
	}

	safeTitle := chartFilenameRe.ReplaceAllString(rowString(chart["title"]), "_")
	if runes := []rune(safeTitle); len(runes) > 64 {
		safeTitle = string(runes[:64])
	}
	if safeTitle == "" {
		safeTitle = "chart"
	}
	filename := "sobs_chart_" + safeTitle + ".json"

	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(templatePayload)
	body := strings.TrimSuffix(sb.String(), "\n")

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	_, _ = w.Write([]byte(body))
}

func importChart(w http.ResponseWriter, r *http.Request) {
	dashboardId := r.PathValue("dashboard_id")
	db := getDb()
	dashboard := getDashboard(db, dashboardId)
	if dashboard == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Dashboard not found"})
		return
	}

	payload, _ := readJsonBody(r)

	templateVersion := payload["sobs_chart_template_version"]
	versionOk := false
	if f, ok := chartNumeric(templateVersion); ok && f == 1 {
		versionOk = true
	}
	if !versionOk {
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "Invalid or unsupported chart template format (expected sobs_chart_template_version: 1)",
		})
		return
	}

	title := strings.TrimSpace(chartStrOr(payload["title"]))
	if title == "" {
		title = "Imported Chart"
	}

	chartSpecRaw := payload["chart_spec"]
	if !pyTruthy(chartSpecRaw) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "chart_spec is required in template"})
		return
	}

	templateId, query, normalizedSpec, err := compileChartSpec(chartSpecRaw)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("Chart spec error: %s", err)})
		return
	}

	optionsJson := jsonDumpsNoEscape(map[string]any{"chart_spec": normalizedSpec})
	existing := getCharts(db, dashboardId)
	position := nextChartPosition(existing)

	chartIdNew := agentUuid4()
	version := time.Now().UnixMilli()
	_, _ = insertRowsJsonEachRow(db, "sobs_chart_configs", []Row{
		{
			"Id":          chartIdNew,
			"DashboardId": dashboardId,
			"Title":       title,
			"ChartType":   templateId,
			"Query":       query,
			"OptionsJson": optionsJson,
			"Position":    position,
			"IsDeleted":   0,
			"Version":     version,
		},
	})

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":            true,
		"chart_id":      chartIdNew,
		"dashboard_id":  dashboardId,
		"dashboard_url": "/dashboards/" + dashboardId,
	})
}
