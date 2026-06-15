package main

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
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

// nsToISO mirrors _ns_to_iso.
func nsToISO(ns int64) string {
	if ns <= 0 {
		return nowISO()
	}
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
		if err := s.enqueueWrite(func() error { _, e := s.insertRowsNormalized("otel_logs", rows); return e }); err != nil {
			return len(rows), err
		}
	}
	return len(rows), nil
}

func (s *server) ingestOTLPTraces(body map[string]any) (int, error) {
	rows := []map[string]any{}
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
				rows = append(rows, map[string]any{
					"Timestamp": nsToISO(startNs), "TraceId": "", "SpanId": "", "ParentSpanId": "",
					"TraceState": "", "SpanName": toStr(m["name"]),
					"SpanKind": orDefault(toStr(spanAttrs["span.kind"]), "INTERNAL"), "ServiceName": service,
					"ResourceAttributes": otlpStringifyAttrs(resAttrs), "ScopeName": "", "ScopeVersion": "",
					"SpanAttributes": otlpStringifyAttrs(merged),
					"Duration":       maxInt64(0, int64(durationMs*1_000_000)),
					"StatusCode":     traceStatusCode(status), "StatusMessage": toStr(spanAttrs["status.message"]),
				})
			}
		}
	}
	if len(rows) > 0 {
		if err := s.enqueueWrite(func() error { _, e := s.insertRowsNormalized("otel_traces", rows); return e }); err != nil {
			return len(rows), err
		}
	}
	return len(rows), nil
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
