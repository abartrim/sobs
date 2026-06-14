package main

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// mcpToolsJSON (the embedded MCP_TOOLS catalog) is declared in handlers_static.go.

const mcpDefaultWindowHours = 24

// mcpToolHandlers mirrors mcp.py _TOOL_HANDLERS.
var mcpToolHandlers = map[string]func(*server, *jsonenc.Object) *jsonenc.Object{
	"list_services":     (*server).mcpListServices,
	"query_otel_logs":   (*server).mcpQueryOtelLogs,
	"query_otel_traces": (*server).mcpQueryOtelTraces,
	"query_metrics":     (*server).mcpQueryMetrics,
	"query_metrics_raw": (*server).mcpQueryMetricsRaw,
	"get_metric_names":  (*server).mcpGetMetricNames,
	"get_anomaly_rules": (*server).mcpGetAnomalyRules,
	"get_recent_errors": (*server).mcpGetRecentErrors,
}

// handleMcpToolsCall mirrors the tools/call branch of mcp_endpoint.
func (s *server) handleMcpToolsCall(w http.ResponseWriter, reqID any, body *jsonenc.Object) {
	params := asObject(func() any { v, _ := body.Get("params"); return v }())
	toolName := strings.TrimSpace(objGetStr(params, "name"))
	args := asObject(func() any { v, _ := params.Get("arguments"); return v }())
	handler, ok := mcpToolHandlers[toolName]
	if !ok {
		names := make([]string, 0, len(mcpToolHandlers))
		for n := range mcpToolHandlers {
			names = append(names, n)
		}
		sort.Strings(names)
		avail := "['" + strings.Join(names, "', '") + "']"
		writeJSON(w, http.StatusNotFound, jsonenc.NewObject().Set("jsonrpc", "2.0").Set("id", reqID).
			Set("error", jsonenc.NewObject().Set("code", -32601).
				Set("message", "Unknown tool: '"+toolName+"'. Available: "+avail)))
		return
	}
	result := handler(s, args)
	masked := s.maskValueForOutput(result)
	text := string(jsonenc.Encode(masked, dumpsDefault))
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("jsonrpc", "2.0").Set("id", reqID).
		Set("result", jsonenc.NewObject().
			Set("content", []any{jsonenc.NewObject().Set("type", "text").Set("text", text)}).
			Set("isError", false)))
}

// ---- helpers (mirror mcp.py _parse_ts / _clamp / _build_time_where / _normalize_map_value) ----

func mcpParseTs(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.HasSuffix(v, "Z") {
		v = v[:len(v)-1] + "+00:00"
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999-07:00", "2006-01-02T15:04:05-07:00", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC().Format("2006-01-02 15:04:05")
		}
	}
	return ""
}

func mcpClamp(v string, lo, hi, def int) int {
	if strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func mcpTimeWhere(column, fromTs, toTs string, conds *[]string, params *[]any) {
	if fromTs != "" {
		*conds = append(*conds, column+" >= ?")
		*params = append(*params, fromTs)
	} else {
		*conds = append(*conds, column+" >= now() - INTERVAL "+strconv.Itoa(mcpDefaultWindowHours)+" HOUR")
	}
	if toTs != "" {
		*conds = append(*conds, column+" <= ?")
		*params = append(*params, toTs)
	}
}

func mcpWhere(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(conds, " AND ")
}

// ---- tool handlers ----

func (s *server) mcpListServices(_ *jsonenc.Object) *jsonenc.Object {
	services := []any{}
	if res, err := s.db.Execute("SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName != '' " +
		"UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName != '' " +
		"UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_metrics_gauge WHERE ServiceName != '' " +
		"ORDER BY ServiceName"); err == nil {
		for _, m := range rowMaps(res) {
			services = append(services, cStr(m, "ServiceName"))
		}
	}
	return jsonenc.NewObject().Set("services", services)
}

func (s *server) mcpQueryOtelLogs(args *jsonenc.Object) *jsonenc.Object {
	conds := []string{}
	params := []any{}
	mcpTimeWhere("Timestamp", mcpParseTs(objGetStr(args, "from_ts")), mcpParseTs(objGetStr(args, "to_ts")), &conds, &params)
	mcpAddEq(&conds, &params, "ServiceName = ?", strings.TrimSpace(objGetStr(args, "service")))
	mcpAddEq(&conds, &params, "SeverityText = ?", strings.ToUpper(strings.TrimSpace(objGetStr(args, "severity"))))
	mcpAddEq(&conds, &params, "TraceId = ?", strings.TrimSpace(objGetStr(args, "trace_id")))
	if search := strings.TrimSpace(objGetStr(args, "search")); search != "" {
		conds = append(conds, "Body ILIKE ?")
		params = append(params, "%"+search+"%")
	}
	limit := mcpClamp(objGetStr(args, "limit"), 1, 500, 100)
	rows := []any{}
	if res, err := s.db.Execute("SELECT toString(Timestamp) AS ts, ServiceName, SeverityText, Body, TraceId, SpanId, LogAttributes "+
		"FROM otel_logs "+mcpWhere(conds)+" ORDER BY Timestamp DESC LIMIT "+strconv.Itoa(limit), params...); err == nil {
		for _, m := range rowMaps(res) {
			rows = append(rows, jsonenc.NewObject().Set("ts", cStr(m, "ts")).Set("service", cStr(m, "ServiceName")).
				Set("severity", cStr(m, "SeverityText")).Set("body", cStr(m, "Body")).
				Set("trace_id", cStr(m, "TraceId")).Set("span_id", cStr(m, "SpanId")).
				Set("attributes", mcpNormalizeMap(m["LogAttributes"])))
		}
	}
	return jsonenc.NewObject().Set("count", len(rows)).Set("rows", rows)
}

func (s *server) mcpQueryOtelTraces(args *jsonenc.Object) *jsonenc.Object {
	conds := []string{}
	params := []any{}
	mcpTimeWhere("Timestamp", mcpParseTs(objGetStr(args, "from_ts")), mcpParseTs(objGetStr(args, "to_ts")), &conds, &params)
	mcpAddEq(&conds, &params, "ServiceName = ?", strings.TrimSpace(objGetStr(args, "service")))
	mcpAddEq(&conds, &params, "SpanName = ?", strings.TrimSpace(objGetStr(args, "span_name")))
	mcpAddEq(&conds, &params, "TraceId = ?", strings.TrimSpace(objGetStr(args, "trace_id")))
	mcpAddEq(&conds, &params, "StatusCode = ?", strings.TrimSpace(objGetStr(args, "status_code")))
	limit := mcpClamp(objGetStr(args, "limit"), 1, 500, 100)
	rows := []any{}
	if res, err := s.db.Execute("SELECT toString(Timestamp) AS ts, ServiceName, TraceId, SpanId, SpanName, SpanKind, "+
		"StatusCode, StatusMessage, toUInt64(Duration / 1000000) AS duration_ms FROM otel_traces "+mcpWhere(conds)+
		" ORDER BY Timestamp DESC LIMIT "+strconv.Itoa(limit), params...); err == nil {
		for _, m := range rowMaps(res) {
			rows = append(rows, jsonenc.NewObject().Set("ts", cStr(m, "ts")).Set("service", cStr(m, "ServiceName")).
				Set("trace_id", cStr(m, "TraceId")).Set("span_id", cStr(m, "SpanId")).Set("span_name", cStr(m, "SpanName")).
				Set("span_kind", cStr(m, "SpanKind")).Set("status_code", cStr(m, "StatusCode")).
				Set("status_message", cStr(m, "StatusMessage")).Set("duration_ms", cInt(m, "duration_ms")))
		}
	}
	return jsonenc.NewObject().Set("count", len(rows)).Set("rows", rows)
}

func (s *server) mcpQueryMetrics(args *jsonenc.Object) *jsonenc.Object {
	conds := []string{}
	params := []any{}
	mcpTimeWhere("MinuteBucket", mcpParseTs(objGetStr(args, "from_ts")), mcpParseTs(objGetStr(args, "to_ts")), &conds, &params)
	mcpAddEq(&conds, &params, "ServiceName = ?", strings.TrimSpace(objGetStr(args, "service")))
	mcpAddEq(&conds, &params, "MetricName = ?", strings.TrimSpace(objGetStr(args, "metric_name")))
	kind := strings.ToLower(strings.TrimSpace(objGetStr(args, "metric_kind")))
	if kind == "gauge" || kind == "sum" || kind == "histogram" {
		conds = append(conds, "MetricKind = ?")
		params = append(params, kind)
	}
	limit := mcpClamp(objGetStr(args, "limit"), 1, 1000, 200)
	rows := []any{}
	if res, err := s.db.Execute("SELECT toString(MinuteBucket) AS ts, ServiceName, MetricName, MetricKind, Value, SampleCount "+
		"FROM v_otel_metrics_1m "+mcpWhere(conds)+" ORDER BY MinuteBucket DESC LIMIT "+strconv.Itoa(limit), params...); err == nil {
		for _, m := range rowMaps(res) {
			rows = append(rows, jsonenc.NewObject().Set("ts", cStr(m, "ts")).Set("service", cStr(m, "ServiceName")).
				Set("metric_name", cStr(m, "MetricName")).Set("metric_kind", cStr(m, "MetricKind")).
				Set("value", cFloat(m, "Value")).Set("sample_count", cInt(m, "SampleCount")))
		}
	}
	return jsonenc.NewObject().Set("count", len(rows)).Set("rows", rows)
}

var mcpRawMetricTables = map[string]string{"gauge": "otel_metrics_gauge", "sum": "otel_metrics_sum", "histogram": "otel_metrics_histogram"}

func (s *server) mcpQueryMetricsRaw(args *jsonenc.Object) *jsonenc.Object {
	kind := strings.ToLower(strings.TrimSpace(objGetStr(args, "metric_kind")))
	table, ok := mcpRawMetricTables[kind]
	if !ok {
		return jsonenc.NewObject().Set("error", "metric_kind must be one of: gauge, sum, histogram")
	}
	conds := []string{}
	params := []any{}
	mcpTimeWhere("TimeUnix", mcpParseTs(objGetStr(args, "from_ts")), mcpParseTs(objGetStr(args, "to_ts")), &conds, &params)
	mcpAddEq(&conds, &params, "ServiceName = ?", strings.TrimSpace(objGetStr(args, "service")))
	mcpAddEq(&conds, &params, "MetricName = ?", strings.TrimSpace(objGetStr(args, "metric_name")))
	limit := mcpClamp(objGetStr(args, "limit"), 1, 500, 100)
	rows := []any{}
	if kind == "histogram" {
		if res, err := s.db.Execute("SELECT toString(TimeUnix) AS ts, ServiceName, MetricName, MetricUnit, Attributes, Count, Sum "+
			"FROM "+table+" "+mcpWhere(conds)+" ORDER BY TimeUnix DESC LIMIT "+strconv.Itoa(limit), params...); err == nil {
			for _, m := range rowMaps(res) {
				rows = append(rows, jsonenc.NewObject().Set("ts", cStr(m, "ts")).Set("service", cStr(m, "ServiceName")).
					Set("metric_name", cStr(m, "MetricName")).Set("metric_unit", cStr(m, "MetricUnit")).
					Set("attributes", mcpNormalizeMap(m["Attributes"])).Set("count", cInt(m, "Count")).Set("sum", cFloat(m, "Sum")))
			}
		}
	} else {
		if res, err := s.db.Execute("SELECT toString(TimeUnix) AS ts, ServiceName, MetricName, MetricUnit, Attributes, Value "+
			"FROM "+table+" "+mcpWhere(conds)+" ORDER BY TimeUnix DESC LIMIT "+strconv.Itoa(limit), params...); err == nil {
			for _, m := range rowMaps(res) {
				rows = append(rows, jsonenc.NewObject().Set("ts", cStr(m, "ts")).Set("service", cStr(m, "ServiceName")).
					Set("metric_name", cStr(m, "MetricName")).Set("metric_unit", cStr(m, "MetricUnit")).
					Set("attributes", mcpNormalizeMap(m["Attributes"])).Set("value", cFloat(m, "Value")))
			}
		}
	}
	return jsonenc.NewObject().Set("count", len(rows)).Set("rows", rows)
}

func (s *server) mcpGetMetricNames(args *jsonenc.Object) *jsonenc.Object {
	where := ""
	params := []any{}
	if service := strings.TrimSpace(objGetStr(args, "service")); service != "" {
		where = "WHERE ServiceName = ?"
		params = []any{service, service, service}
	}
	sql := "SELECT MetricName, ServiceName, max(toString(TimeUnixMs)) AS last_seen FROM otel_metrics_gauge " + where + " GROUP BY MetricName, ServiceName " +
		"UNION ALL SELECT MetricName, ServiceName, max(toString(TimeUnixMs)) AS last_seen FROM otel_metrics_sum " + where + " GROUP BY MetricName, ServiceName " +
		"UNION ALL SELECT MetricName, ServiceName, max(toString(TimeUnixMs)) AS last_seen FROM otel_metrics_histogram " + where + " GROUP BY MetricName, ServiceName " +
		"ORDER BY MetricName, ServiceName"
	metrics := []any{}
	if res, err := s.db.Execute(sql, params...); err == nil {
		for _, m := range rowMaps(res) {
			metrics = append(metrics, jsonenc.NewObject().Set("metric_name", cStr(m, "MetricName")).
				Set("service", cStr(m, "ServiceName")).Set("last_seen", cStr(m, "last_seen")))
		}
	}
	return jsonenc.NewObject().Set("count", len(metrics)).Set("metrics", metrics)
}

func (s *server) mcpGetAnomalyRules(_ *jsonenc.Object) *jsonenc.Object {
	rules := []any{}
	if res, err := s.db.Execute("SELECT Id, Name, RuleType, SignalSource, SignalName, ServiceName, Comparator, " +
		"WarningThreshold, CriticalThreshold FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 ORDER BY SignalSource, SignalName"); err == nil {
		for _, m := range rowMaps(res) {
			rules = append(rules, jsonenc.NewObject().Set("id", cStr(m, "Id")).Set("name", cStr(m, "Name")).
				Set("rule_type", cStr(m, "RuleType")).Set("signal_source", cStr(m, "SignalSource")).
				Set("signal_name", cStr(m, "SignalName")).Set("service", cStr(m, "ServiceName")).
				Set("comparator", cStr(m, "Comparator")).Set("warning_threshold", cFloat(m, "WarningThreshold")).
				Set("critical_threshold", cFloat(m, "CriticalThreshold")))
		}
	}
	return jsonenc.NewObject().Set("count", len(rules)).Set("rules", rules)
}

func (s *server) mcpGetRecentErrors(args *jsonenc.Object) *jsonenc.Object {
	service := strings.TrimSpace(objGetStr(args, "service"))
	fromTs, toTs := mcpParseTs(objGetStr(args, "from_ts")), mcpParseTs(objGetStr(args, "to_ts"))
	limit := mcpClamp(objGetStr(args, "limit"), 1, 200, 50)
	half := limit / 2
	if half == 0 {
		half = 1
	}
	logConds := []string{}
	logParams := []any{}
	mcpTimeWhere("Timestamp", fromTs, toTs, &logConds, &logParams)
	logConds = append(logConds, "SeverityText IN ('ERROR', 'FATAL', 'CRITICAL')")
	mcpAddEq(&logConds, &logParams, "ServiceName = ?", service)
	traceConds := []string{}
	traceParams := []any{}
	mcpTimeWhere("Timestamp", fromTs, toTs, &traceConds, &traceParams)
	traceConds = append(traceConds, "StatusCode = 'STATUS_CODE_ERROR'")
	mcpAddEq(&traceConds, &traceParams, "ServiceName = ?", service)
	out := []map[string]any{}
	if res, err := s.db.Execute("SELECT toString(Timestamp) AS ts, ServiceName, 'log' AS source, SeverityText AS level_or_status, "+
		"Body AS message, TraceId FROM otel_logs WHERE "+strings.Join(logConds, " AND ")+" ORDER BY Timestamp DESC LIMIT "+strconv.Itoa(half), logParams...); err == nil {
		for _, m := range rowMaps(res) {
			out = append(out, m)
		}
	}
	if res, err := s.db.Execute("SELECT toString(Timestamp) AS ts, ServiceName, 'trace' AS source, StatusCode AS level_or_status, "+
		"SpanName AS message, TraceId FROM otel_traces WHERE "+strings.Join(traceConds, " AND ")+" ORDER BY Timestamp DESC LIMIT "+strconv.Itoa(half), traceParams...); err == nil {
		for _, m := range rowMaps(res) {
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return cStr(out[i], "ts") > cStr(out[j], "ts") })
	errs := []any{}
	for _, m := range out {
		errs = append(errs, jsonenc.NewObject().Set("ts", cStr(m, "ts")).Set("service", cStr(m, "ServiceName")).
			Set("source", cStr(m, "source")).Set("level_or_status", cStr(m, "level_or_status")).
			Set("message", cStr(m, "message")).Set("trace_id", cStr(m, "TraceId")))
	}
	return jsonenc.NewObject().Set("count", len(errs)).Set("errors", errs)
}

func mcpAddEq(conds *[]string, params *[]any, clause, value string) {
	if value != "" {
		*conds = append(*conds, clause)
		*params = append(*params, value)
	}
}

// mcpNormalizeMap mirrors _normalize_map_value for a chdb Map cell.
func mcpNormalizeMap(raw any) *jsonenc.Object {
	switch v := raw.(type) {
	case *jsonenc.Object:
		return v
	case map[string]any:
		o := jsonenc.NewObject()
		for k, val := range v {
			o.Set(k, val)
		}
		return o
	case string:
		if parsed, err := parseJSONValue([]byte(v)); err == nil {
			if o, ok := parsed.(*jsonenc.Object); ok {
				return o
			}
		}
	}
	return jsonenc.NewObject()
}
