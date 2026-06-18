package main

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// jsonDumpsNoEscBytes mirrors json.dumps(value, ensure_ascii=False) (compact, no HTML escaping) for
// non-scalar OTLP attribute values stored as strings.
func jsonDumpsNoEscBytes(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// OTLP-JSON ingest. The HTTP response is only {"accepted": <count>}, so the per-record row mapping
// is faithful but never byte-compared; the parity check verifies the count and a successful insert.
// (The attr-key tracking + tag-rule application side-effects of the Python inserters are deferred —
// they feed secondary indexes, not the response.)

// otlpAnyValue mirrors _proto_any_value_to_python: unwrap an OTLP AnyValue JSON object.
func otlpAnyValue(v any) any {
	o, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if sv, ok := o["stringValue"]; ok {
		return toStr(sv)
	}
	if iv, ok := o["intValue"]; ok {
		switch n := iv.(type) {
		case string:
			i, _ := strconv.ParseInt(n, 10, 64)
			return i
		case float64:
			return int64(n)
		}
		return int64(0)
	}
	if dv, ok := o["doubleValue"]; ok {
		if f, ok := dv.(float64); ok {
			return f
		}
	}
	if bv, ok := o["boolValue"]; ok {
		if b, ok := bv.(bool); ok {
			return b
		}
	}
	if av, ok := o["arrayValue"].(map[string]any); ok {
		vals, _ := av["values"].([]any)
		out := []any{}
		for _, e := range vals {
			out = append(out, otlpAnyValue(e))
		}
		return out
	}
	if kv, ok := o["kvlistValue"].(map[string]any); ok {
		vals, _ := kv["values"].([]any)
		return otlpKVList(vals)
	}
	return nil
}

// otlpKVList mirrors _proto_kvlist_to_dict: a list of {key, value} into an ordered-insensitive map.
func otlpKVList(attrs []any) map[string]any {
	out := map[string]any{}
	for _, a := range attrs {
		kv, ok := a.(map[string]any)
		if !ok {
			continue
		}
		out[toStr(kv["key"])] = otlpAnyValue(kv["value"])
	}
	return out
}

// otlpStringifyAttrs mirrors _stringify_attrs: any attr value -> string (scalars via str(), else JSON).
func otlpStringifyAttrs(values map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range values {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			if t {
				out[k] = "True"
			} else {
				out[k] = "False"
			}
		case int64:
			out[k] = strconv.FormatInt(t, 10)
		case float64:
			out[k] = formatPyNumber(t)
		default:
			out[k] = string(jsonDumpsNoEscBytes(v))
		}
	}
	return out
}

// nsToISO mirrors _ns_to_iso: convert the OTLP nanosecond timestamp to a millisecond ISO-8601
// string. ns == 0 yields the epoch (datetime.fromtimestamp(0)), matching Python — NOT now().
func nsToISO(ns int64) string {
	return time.Unix(0, ns).UTC().Format("2006-01-02T15:04:05.000-07:00")
}

func otlpInt(v any) int64 {
	switch n := v.(type) {
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	case float64:
		return int64(n)
	}
	return 0
}

func mergeAttrs(maps ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

var fingerprintSkipPrefixes = []string{"telemetry.", "process.", "os.", "runtime."}

// attrFingerprint mirrors _attr_fingerprint (sorted k=v, skip-prefixes, first 8, md5[:16]).
func attrFingerprint(attrs map[string]any) string {
	pairs := []string{}
	for k, v := range attrs {
		skip := false
		for _, p := range fingerprintSkipPrefixes {
			if strings.HasPrefix(k, p) {
				skip = true
				break
			}
		}
		if !skip {
			pairs = append(pairs, k+"="+toStr(v))
		}
	}
	sort.Strings(pairs)
	if len(pairs) > 8 {
		pairs = pairs[:8]
	}
	return md5Hex(strings.Join(pairs, "|"))[:16]
}

func traceStatusCode(status string) string {
	switch strings.ToUpper(status) {
	case "ERROR":
		return "STATUS_CODE_ERROR"
	case "OK":
		return "STATUS_CODE_OK"
	default:
		return "STATUS_CODE_UNSET"
	}
}

// ---- per-schema ingest ----------------------------------------------------------------

func (s *server) ingestOTLPLogs(body map[string]any) (int, error) {
	rows := []map[string]any{}
	resList, _ := body["resourceLogs"].([]any)
	for _, r := range resList {
		ro, _ := r.(map[string]any)
		resAttrs := otlpResourceAttrs(ro)
		service := toStr(resAttrs["service.name"])
		scopes, _ := ro["scopeLogs"].([]any)
		for _, sc := range scopes {
			so, _ := sc.(map[string]any)
			scopeAttrs := otlpScopeAttrs(so)
			recs, _ := so["logRecords"].([]any)
			for _, rec := range recs {
				m, _ := rec.(map[string]any)
				recAttrs := otlpKVList(asList(m["attributes"]))
				level := strings.ToUpper(orDefault(toStr(m["severityText"]), "INFO"))
				bodyVal := otlpAnyValue(m["body"])
				bodyStr, ok := bodyVal.(string)
				if !ok {
					bodyStr = string(jsonDumpsNoEscBytes(bodyVal))
				}
				rows = append(rows, map[string]any{
					"Timestamp": nsToISO(otlpInt(m["timeUnixNano"])), "TraceId": "", "SpanId": "",
					"TraceFlags": 0, "SeverityText": level, "SeverityNumber": severityNumber(level),
					"ServiceName": service, "Body": bodyStr, "ResourceSchemaUrl": "",
					"ResourceAttributes": otlpStringifyAttrs(resAttrs), "ScopeSchemaUrl": "",
					"ScopeName": "", "ScopeVersion": "", "ScopeAttributes": otlpStringifyAttrs(scopeAttrs),
					"LogAttributes": otlpStringifyAttrs(mergeAttrs(resAttrs, scopeAttrs, recAttrs)),
					"EventName":     toStr(recAttrs["event.name"]),
				})
			}
		}
	}
	if len(rows) > 0 {
		if err := s.enqueueWrite(func() error {
			if _, e := s.insertRowsNormalized("otel_logs", rows); e != nil {
				return e
			}
			// Side-effects mirroring app.py _insert_log_events: track discovered attr keys per
			// record_type and apply active tag rules to the inserted rows.
			s.rememberLogAttrKeys(extractLogAttrMaps(rows))
			s.rememberAttrKeys(extractAttrMaps(rows, "ResourceAttributes"), "resource")
			s.rememberAttrKeys(extractAttrMaps(rows, "ScopeAttributes"), "scope")
			if rules := s.loadTagRulesCtx(); len(rules) > 0 {
				s.applyTagRules("log", rows, rules)
			}
			return nil
		}); err != nil {
			return len(rows), err
		}
		// app.py ingest_logs: after the write succeeds, _sse_broadcast one event per log to the
		// live /tail subscribers (app.py:9681). Fields mirror the LogEvent payload exactly
		// (source/ts/level/service/body/trace_id, in that order).
		for _, row := range rows {
			s.sseBroadcast(jsonenc.NewObject().
				Set("source", "logs").
				Set("ts", row["Timestamp"]).
				Set("level", row["SeverityText"]).
				Set("service", row["ServiceName"]).
				Set("body", row["Body"]).
				Set("trace_id", row["TraceId"]))
		}
	}
	return len(rows), nil
}

func (s *server) ingestOTLPTraces(body map[string]any) (int, error) {
	rows := []map[string]any{}
	errorRows := []map[string]any{}
	// tailEvents accumulates the /tail SSE payloads (one "traces" event per span, plus a "ai"
	// event for GenAI spans) so they can be broadcast after the write succeeds, mirroring the
	// broadcast loop in app.py ingest_traces (app.py:9855/9871).
	var tailEvents []*jsonenc.Object
	resList, _ := body["resourceSpans"].([]any)
	for _, r := range resList {
		ro, _ := r.(map[string]any)
		resAttrs := otlpResourceAttrs(ro)
		service := toStr(resAttrs["service.name"])
		scopes, _ := ro["scopeSpans"].([]any)
		for _, sc := range scopes {
			so, _ := sc.(map[string]any)
			scopeAttrs := otlpScopeAttrs(so)
			spans, _ := so["spans"].([]any)
			for _, sp := range spans {
				m, _ := sp.(map[string]any)
				startNs := otlpInt(m["startTimeUnixNano"])
				endNs := otlpInt(m["endTimeUnixNano"])
				durationMs := 0.0
				if endNs > startNs {
					durationMs = float64(endNs-startNs) / 1_000_000
				}
				statusCode := 0
				if st, ok := m["status"].(map[string]any); ok {
					statusCode = int(otlpInt(st["code"]))
				}
				status := "UNSET"
				if statusCode == 1 {
					status = "OK"
				} else if statusCode == 2 {
					status = "ERROR"
				}
				spanAttrs := otlpKVList(asList(m["attributes"]))
				merged := mergeAttrs(resAttrs, scopeAttrs, spanAttrs)
				ts := nsToISO(startNs)
				spanName := toStr(m["name"])
				rows = append(rows, map[string]any{
					"Timestamp": ts, "TraceId": "", "SpanId": "", "ParentSpanId": "",
					"TraceState": "", "SpanName": spanName,
					"SpanKind": orDefault(toStr(spanAttrs["span.kind"]), "INTERNAL"), "ServiceName": service,
					"ResourceAttributes": otlpStringifyAttrs(resAttrs), "ScopeName": "", "ScopeVersion": "",
					"SpanAttributes": otlpStringifyAttrs(merged),
					"Duration":       maxInt64(0, int64(durationMs*1_000_000)),
					"StatusCode":     traceStatusCode(status), "StatusMessage": toStr(spanAttrs["status.message"]),
				})
				// app.py renders duration_ms as an int 0 when end<=start (the `else 0` branch) and a
				// float otherwise; mirror that so the /tail payload matches.
				var durVal any = durationMs
				if durationMs == 0 {
					durVal = 0
				}
				tailEvents = append(tailEvents, jsonenc.NewObject().
					Set("source", "traces").Set("ts", ts).Set("trace_id", "").Set("span_id", "").
					Set("name", spanName).Set("service", service).
					Set("duration_ms", durVal).Set("status", status))
				// app.py also broadcasts an "ai" event when the span carries GenAI attributes
				// (provider = gen_ai.provider.name or gen_ai.system); attrs are the merged map.
				provider := toStr(merged["gen_ai.provider.name"])
				if provider == "" {
					provider = toStr(merged["gen_ai.system"])
				}
				operationName := toStr(merged["gen_ai.operation.name"])
				if provider != "" || operationName != "" {
					tailEvents = append(tailEvents, jsonenc.NewObject().
						Set("source", "ai").Set("ts", ts).Set("trace_id", "").Set("span_id", "").
						Set("service", service).Set("provider", provider).
						Set("model", toStr(merged["gen_ai.request.model"])).
						Set("operation", operationName).
						Set("duration_ms", durVal).Set("status", status))
				}
				// ERROR-status spans become synthetic otel_logs exception rows, mirroring
				// app.py _proto_traces_to_events -> _insert_error_events.
				if strings.Contains(strings.ToUpper(status), "ERROR") {
					errType := spanAttrStr(spanAttrs, "exception.type", "SpanError")
					// message = span_attrs.get("exception.message", span_attrs.get("error.message", span.name))
					var message string
					if v, ok := spanAttrs["exception.message"]; ok {
						message = toStr(v)
					} else {
						message = spanAttrStr(spanAttrs, "error.message", spanName)
					}
					stack := spanAttrStr(spanAttrs, "exception.stacktrace", "")
					errAttrs := otlpStringifyAttrs(merged)
					errAttrs["exception.type"] = errType
					errAttrs["exception.message"] = message
					if stack != "" {
						errAttrs["exception.stacktrace"] = stack
					}
					errorRows = append(errorRows, map[string]any{
						"Timestamp": ts, "TraceId": "", "SpanId": "", "TraceFlags": 0,
						"SeverityText": "ERROR", "SeverityNumber": severityNumber("ERROR"),
						"ServiceName": service, "Body": message, "ResourceSchemaUrl": "",
						"ResourceAttributes": map[string]any{}, "ScopeSchemaUrl": "",
						"ScopeName": "", "ScopeVersion": "", "ScopeAttributes": map[string]any{},
						"LogAttributes": errAttrs, "EventName": "exception",
					})
				}
			}
		}
	}
	if len(rows) > 0 {
		if err := s.enqueueWrite(func() error {
			if _, e := s.insertRowsNormalized("otel_traces", rows); e != nil {
				return e
			}
			// Side-effects mirroring app.py _insert_span_events.
			s.rememberAttrKeys(extractAttrMaps(rows, "SpanAttributes"), "span")
			s.rememberAttrKeys(extractAttrMaps(rows, "ResourceAttributes"), "resource")
			if rules := s.loadTagRulesCtx(); len(rules) > 0 {
				s.applyTagRules("trace", rows, rules)
			}
			return nil
		}); err != nil {
			return len(rows), err
		}
	}
	// The synthetic exception rows are written via their own queued op, mirroring app.py
	// ingest_traces' separate _insert_error_events call (which also tracks attr keys + tags).
	if len(errorRows) > 0 {
		_ = s.enqueueWrite(func() error {
			if _, e := s.insertRowsNormalized("otel_logs", errorRows); e != nil {
				return e
			}
			s.rememberLogAttrKeys(extractLogAttrMaps(errorRows))
			if rules := s.loadTagRulesCtx(); len(rules) > 0 {
				s.applyTagRules("error", errorRows, rules)
			}
			return nil
		})
	}
	// app.py ingest_traces broadcasts the per-span (and GenAI-derived "ai") events to live /tail
	// subscribers after the write. A failed span write returns above, so reaching here means the
	// span rows were durably queued.
	for _, ev := range tailEvents {
		s.sseBroadcast(ev)
	}
	return len(rows), nil
}

// spanAttrStr mirrors str(span_attrs.get(key, default)) for the exception-derivation logic:
// stringify the span attribute via the same Python str() semantics (toStr), defaulting if absent.
// Note the span attrs here are the *parsed* AnyValue map (otlpKVList), matching app.py which reads
// span_attrs (the per-span dict) — NOT the already-stringified Map column.
func spanAttrStr(attrs map[string]any, key, def string) string {
	if v, ok := attrs[key]; ok && v != nil {
		return toStr(v)
	}
	return def
}

func (s *server) ingestOTLPMetrics(body map[string]any) (int, error) {
	gauge, sum, hist := []map[string]any{}, []map[string]any{}, []map[string]any{}
	resList, _ := body["resourceMetrics"].([]any)
	for _, r := range resList {
		ro, _ := r.(map[string]any)
		resAttrs := otlpResourceAttrs(ro)
		service := orDefault(toStr(resAttrs["service.name"]), "metrics")
		scopes, _ := ro["scopeMetrics"].([]any)
		for _, sc := range scopes {
			so, _ := sc.(map[string]any)
			metrics, _ := so["metrics"].([]any)
			for _, mt := range metrics {
				m, _ := mt.(map[string]any)
				name, desc, unit := toStr(m["name"]), toStr(m["description"]), toStr(m["unit"])
				base := func(dp map[string]any, value float64) map[string]any {
					dpAttrs := otlpKVList(asList(dp["attributes"]))
					return map[string]any{
						"TimeUnix": nsToISO(otlpInt(dp["timeUnixNano"])), "ServiceName": service,
						"MetricName": name, "MetricDescription": desc, "MetricUnit": unit,
						"Attributes": otlpStringifyAttrs(dpAttrs), "Value": value, "Flags": 0,
						"AttrFingerprint": attrFingerprint(dpAttrs),
					}
				}
				if g, ok := m["gauge"].(map[string]any); ok {
					for _, d := range asList(g["dataPoints"]) {
						dp, _ := d.(map[string]any)
						gauge = append(gauge, base(dp, otlpDPValue(dp)))
					}
				} else if sm, ok := m["sum"].(map[string]any); ok {
					mono := 0
					if b, _ := sm["isMonotonic"].(bool); b {
						mono = 1
					}
					temp := int(otlpInt(sm["aggregationTemporality"]))
					for _, d := range asList(sm["dataPoints"]) {
						dp, _ := d.(map[string]any)
						row := base(dp, otlpDPValue(dp))
						row["IsMonotonic"], row["AggregationTemporality"] = mono, temp
						sum = append(sum, row)
					}
				} else if h, ok := m["histogram"].(map[string]any); ok {
					temp := int(otlpInt(h["aggregationTemporality"]))
					for _, d := range asList(h["dataPoints"]) {
						dp, _ := d.(map[string]any)
						row := base(dp, 0)
						delete(row, "Value")
						row["Count"] = otlpInt(dp["count"])
						row["Sum"] = otlpFloat(dp["sum"])
						row["BucketCounts"] = otlpIntList(dp["bucketCounts"])
						row["ExplicitBounds"] = otlpFloatList(dp["explicitBounds"])
						row["AggregationTemporality"] = temp
						hist = append(hist, row)
					}
				}
			}
		}
	}
	total := len(gauge) + len(sum) + len(hist)
	if total > 0 {
		// One queued op for all three metric tables, mirroring app.py's single _insert_metric_events.
		err := s.enqueueWrite(func() error {
			if len(gauge) > 0 {
				if _, e := s.insertRowsNormalized("otel_metrics_gauge", gauge); e != nil {
					return e
				}
			}
			if len(sum) > 0 {
				if _, e := s.insertRowsNormalized("otel_metrics_sum", sum); e != nil {
					return e
				}
			}
			if len(hist) > 0 {
				if _, e := s.insertRowsNormalized("otel_metrics_histogram", hist); e != nil {
					return e
				}
			}
			return nil
		})
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// ---- small shared helpers -------------------------------------------------------------

func otlpResourceAttrs(ro map[string]any) map[string]any {
	if res, ok := ro["resource"].(map[string]any); ok {
		return otlpKVList(asList(res["attributes"]))
	}
	return map[string]any{}
}

func otlpScopeAttrs(so map[string]any) map[string]any {
	if sc, ok := so["scope"].(map[string]any); ok {
		return otlpKVList(asList(sc["attributes"]))
	}
	return map[string]any{}
}

func otlpDPValue(dp map[string]any) float64 {
	if v, ok := dp["asInt"]; ok {
		return float64(otlpInt(v))
	}
	return otlpFloat(dp["asDouble"])
}

func otlpFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func otlpIntList(v any) []any {
	out := []any{}
	for _, e := range asList(v) {
		out = append(out, otlpInt(e))
	}
	return out
}

func otlpFloatList(v any) []any {
	out := []any{}
	for _, e := range asList(v) {
		out = append(out, otlpFloat(e))
	}
	return out
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
