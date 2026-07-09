package main

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// isFalsyAny mirrors Python truthiness for the `x or default` idiom (None/""/0/False -> falsy).
func isFalsyAny(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case json.Number:
		f, err := x.Float64()
		return err == nil && f == 0
	case int:
		return x == 0
	case float64:
		return x == 0
	default:
		return false
	}
}

// orStrDefault mirrors str(x or default).strip().
func orStrDefault(v any, present bool, def string) string {
	if !present || isFalsyAny(v) {
		return strings.TrimSpace(def)
	}
	return strings.TrimSpace(pyStr(v, true))
}

// compileBuilderSQL mirrors app.py _compile_builder_sql: build a SELECT from the builder spec's
// data block. Byte-exact SQL (the compiled query is returned verbatim in spec API responses).
func compileBuilderSQL(templateID string, data *jsonenc.Object) (string, string) {
	if templateID == "custom_echarts" {
		return "", "custom_echarts requires sql.mode='raw'"
	}
	gv := func(k string) (any, bool) { return data.Get(k) }
	svV, svOK := gv("source_view")
	sourceView := orStrDefault(svV, svOK, "v_derived_signals_anomaly")
	supported := map[string]bool{
		"v_derived_signals_anomaly": true, "v_otel_metrics_anomaly": true,
		"otel_metrics_gauge": true, "otel_metrics_sum": true, "otel_metrics_histogram": true,
		"otel_logs": true, "otel_traces": true, "sobs_error_resolutions": true,
	}
	if !supported[sourceView] {
		return "", "Unsupported source for builder mode"
	}
	get := func(k string) string { v, ok := gv(k); return pyStrOrStrip(v, ok) }
	service := get("service")
	signalSource := get("signal_source")
	signalName := get("signal_name")
	metricName := get("metric_name")
	attrFp := get("attr_fp")
	whV, whOK := gv("window_hours")
	windowHours := coercePositiveInt(whV, whOK, 6, 1, 168)
	limV, limOK := gv("limit")
	limit := coercePositiveInt(limV, limOK, 1000, 1, 2000)
	wh := strconv.Itoa(windowHours)

	defaultSourceLabel := func() string {
		switch sourceView {
		case "otel_logs":
			return "logs"
		case "otel_traces":
			return "traces"
		case "sobs_error_resolutions":
			return "errors"
		case "v_derived_signals_anomaly":
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
		switch sourceView {
		case "otel_logs":
			return "log_volume"
		case "otel_traces":
			return "trace_volume"
		case "sobs_error_resolutions":
			return "resolved_error_volume"
		}
		return "value"
	}

	// _scored_anomaly_tail is the shared scored->SELECT tail used by the count-per-minute sources.
	scoredTail := "  baseline_mean,\n" +
		"  greatest(0.0, baseline_mean - (3.0 * ifNull(baseline_stddev, 0.0))) AS baseline_lower,\n" +
		"  baseline_mean + (3.0 * ifNull(baseline_stddev, 0.0)) AS baseline_upper,\n" +
		"  if(\n" +
		"    abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) >= 3.0,\n" +
		"    'outlier',\n" +
		"    'normal'\n" +
		"  ) AS anomaly_state,\n" +
		"  abs(value - baseline_mean) / greatest(ifNull(baseline_stddev, 0.0), 1.0) AS anomaly_score\n" +
		"FROM scored"

	buildSeriesSQL := func() string {
		switch sourceView {
		case "v_derived_signals_anomaly":
			whereParts := []string{"time >= now() - INTERVAL " + wh + " HOUR"}
			if service != "" {
				whereParts = append(whereParts, "ServiceName = "+sqlLiteral(service))
			}
			if attrFp != "" {
				whereParts = append(whereParts, "AttrFingerprint = "+sqlLiteral(attrFp))
			}
			if signalSource != "" {
				whereParts = append(whereParts, "SignalSource = "+sqlLiteral(signalSource))
			}
			if signalName != "" {
				whereParts = append(whereParts, "SignalName = "+sqlLiteral(signalName))
			}
			return "SELECT\n  time,\n  value,\n  baseline_mean,\n  baseline_lower,\n  baseline_upper,\n" +
				"  anomaly_state,\n  anomaly_score\nFROM v_derived_signals_anomaly\nWHERE " +
				strings.Join(whereParts, " AND\n    ")
		case "v_otel_metrics_anomaly":
			whereParts := []string{"time >= now() - INTERVAL " + wh + " HOUR"}
			if service != "" {
				whereParts = append(whereParts, "ServiceName = "+sqlLiteral(service))
			}
			if metricName != "" {
				whereParts = append(whereParts, "MetricName = "+sqlLiteral(metricName))
			}
			if attrFp != "" {
				whereParts = append(whereParts, "AttrFingerprint = "+sqlLiteral(attrFp))
			}
			return "SELECT\n  time,\n  value,\n  baseline_mean,\n  baseline_lower,\n  baseline_upper,\n" +
				"  anomaly_state,\n  anomaly_score\nFROM v_otel_metrics_anomaly\nWHERE " +
				strings.Join(whereParts, " AND\n    ")
		case "otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram":
			valueExpr := "Value"
			if sourceView == "otel_metrics_histogram" {
				valueExpr = "if(Count = 0, 0.0, Sum / toFloat64(Count))"
			}
			whereParts := []string{"TimeUnixMs >= now() - INTERVAL " + wh + " HOUR"}
			if service != "" {
				whereParts = append(whereParts, "ServiceName = "+sqlLiteral(service))
			}
			if metricName != "" {
				whereParts = append(whereParts, "MetricName = "+sqlLiteral(metricName))
			}
			if attrFp != "" {
				whereParts = append(whereParts, "AttrFingerprint = "+sqlLiteral(attrFp))
			}
			return "WITH per_minute AS (\n  SELECT\n    toStartOfMinute(TimeUnixMs) AS time,\n" +
				"    avg(toFloat64(" + valueExpr + ")) AS value\n  FROM " + sourceView + "\n  WHERE " +
				strings.Join(whereParts, " AND\n    ") + "\n  GROUP BY time\n), scored AS (\n" +
				"  SELECT\n    time,\n    value,\n    avg(value) OVER (\n      ORDER BY time\n" +
				"      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n    ) AS baseline_mean,\n" +
				"    stddevPop(value) OVER (\n      ORDER BY time\n" +
				"      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n    ) AS baseline_stddev\n" +
				"  FROM per_minute\n)\nSELECT\n  time,\n  value,\n" + scoredTail
		case "otel_logs":
			whereParts := []string{"TimestampTime >= now() - INTERVAL " + wh + " HOUR"}
			if service != "" {
				whereParts = append(whereParts, "ServiceName = "+sqlLiteral(service))
			}
			return countPerMinuteSeries("toStartOfMinute(TimestampTime)", "otel_logs",
				strings.Join(whereParts, " AND\n    "), scoredTail)
		case "otel_traces":
			whereParts := []string{"TimestampTime >= now() - INTERVAL " + wh + " HOUR"}
			if service != "" {
				whereParts = append(whereParts, "ServiceName = "+sqlLiteral(service))
			}
			return countPerMinuteSeries("toStartOfMinute(TimestampTime)", "otel_traces",
				strings.Join(whereParts, " AND\n    "), scoredTail)
		}
		// sobs_error_resolutions
		whereClause := "ResolvedAt >= now() - INTERVAL " + wh + " HOUR"
		return countPerMinuteSeries("toStartOfMinute(ResolvedAt)", "sobs_error_resolutions",
			whereClause, scoredTail)
	}

	seriesSQL := buildSeriesSQL()

	switch templateID {
	case "derived_signal_overlay":
		return "WITH series AS (\n" + seriesSQL + "\n)\nSELECT\n  time,\n" +
			"  " + sqlLiteral(orDefault(service, "all")) + " AS service,\n" +
			"  " + sqlLiteral(defaultSourceLabel()) + " AS source,\n" +
			"  " + sqlLiteral(defaultSignalLabel()) + " AS signal,\n" +
			"  " + sqlLiteral(attrFp) + " AS attr_fp,\n" +
			"  value,\n  toUInt32(1) AS sample_count,\n  baseline_mean,\n  baseline_lower,\n" +
			"  baseline_upper,\n  anomaly_state,\n  anomaly_score\nFROM series\nORDER BY time\nLIMIT " +
			strconv.Itoa(limit), ""
	case "anomaly_overlay":
		return "WITH series AS (\n" + seriesSQL + "\n)\nSELECT\n  time,\n  value,\n  baseline_mean,\n" +
			"  baseline_lower,\n  baseline_upper,\n  anomaly_state\nFROM series\nORDER BY time\nLIMIT " +
			strconv.Itoa(limit), ""
	case "dual_axis_anomaly":
		return "WITH series AS (\n" + seriesSQL + "\n)\nSELECT\n  time,\n  value AS metric,\n" +
			"  anomaly_score\nFROM series\nORDER BY time\nLIMIT " + strconv.Itoa(limit), ""
	case "time_series_percentiles":
		return "WITH series AS (\n" + seriesSQL + "\n)\nSELECT\n  time,\n  value,\n" +
			"  baseline_upper AS p95,\n  greatest(baseline_upper, value) AS p99\nFROM series\n" +
			"ORDER BY time\nLIMIT " + strconv.Itoa(limit), ""
	case "heatmap":
		return "WITH series AS (\n" + seriesSQL + "\n)\nSELECT\n" +
			"  " + sqlLiteral(orDefault(service, "all")) + " AS x_category,\n" +
			"  toStartOfFiveMinutes(time) AS y_category,\n  avg(value) AS value\nFROM series\n" +
			"GROUP BY y_category\nORDER BY y_category\nLIMIT " + strconv.Itoa(limit), ""
	case "box_plot":
		return "WITH series AS (\n" + seriesSQL + "\n)\nSELECT\n" +
			"  " + sqlLiteral(defaultSignalLabel()) + " AS dimension,\n" +
			"  min(value) AS min,\n  quantile(0.25)(value) AS q1,\n  quantile(0.5)(value) AS median,\n" +
			"  quantile(0.75)(value) AS q3,\n  max(value) AS max\nFROM series", ""
	case "gauge_kpi":
		return "WITH series AS (\n" + seriesSQL + "\n)\n" +
			"SELECT round(100.0 * avg(if(anomaly_state = 'normal', 1.0, 0.0)), 2) AS value\nFROM series", ""
	}
	return "", "Builder mode does not support template: " + templateID
}

// countPerMinuteSeries builds the shared count()-per-minute scored CTE used by the
// otel_logs / otel_traces / sobs_error_resolutions sources.
func countPerMinuteSeries(timeExpr, fromTable, whereClause, scoredTail string) string {
	return "WITH per_minute AS (\n  SELECT\n    " + timeExpr + " AS time,\n    count() AS value\n" +
		"  FROM " + fromTable + "\n  WHERE " + whereClause + "\n  GROUP BY time\n), scored AS (\n" +
		"  SELECT\n    time,\n    toFloat64(value) AS value,\n    avg(toFloat64(value)) OVER (\n" +
		"      ORDER BY time\n      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n    ) AS baseline_mean,\n" +
		"    stddevPop(toFloat64(value)) OVER (\n      ORDER BY time\n" +
		"      ROWS BETWEEN 59 PRECEDING AND CURRENT ROW\n    ) AS baseline_stddev\n  FROM per_minute\n)\n" +
		"SELECT\n  time,\n  value,\n" + scoredTail
}
