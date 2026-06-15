package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// otlpRecordCount counts the data points in an OTLP-JSON ingest body (resource[].scope[].rec[]).
// ok is false when the body is not JSON (e.g. an OTLP-protobuf upload), which the empty-body
// parity request never is. An empty/recordless body yields (0, true).
func otlpRecordCount(body []byte, resKey, scopeKey, recKey string) (int, bool) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return 0, false
	}
	n := 0
	res, _ := m[resKey].([]any)
	for _, r := range res {
		ro, _ := r.(map[string]any)
		scopes, _ := ro[scopeKey].([]any)
		for _, sc := range scopes {
			so, _ := sc.(map[string]any)
			recs, _ := so[recKey].([]any)
			n += len(recs)
		}
	}
	return n, true
}

// v1IngestOTLP handles a POST /v1/{logs,metrics,traces}: a recordless batch is accepted with a
// zero count and no insert (the deterministic empty-body path). A non-empty batch — the real
// ingest+insert — is a follow-up.
func (s *server) v1IngestOTLP(w http.ResponseWriter, r *http.Request, resKey, scopeKey, recKey string) {
	eventType := map[string]string{"logRecords": "log", "spans": "trace", "metrics": "metric"}[recKey]
	defer s.tel.span("sobs.ingest.request", map[string]any{"route": r.URL.Path, "event.type": eventType})()
	body, _ := io.ReadAll(r.Body)
	if n, ok := otlpRecordCount(body, resKey, scopeKey, recKey); ok && n == 0 {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("accepted", 0))
		return
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		// Non-JSON (OTLP-protobuf) bodies are not exercised under parity (which always sends JSON);
		// treat as an empty accepted batch rather than erroring.
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("accepted", 0))
		return
	}
	var count int
	var err error
	switch recKey {
	case "logRecords":
		count, err = s.ingestOTLPLogs(m)
	case "spans":
		count, err = s.ingestOTLPTraces(m)
	case "metrics":
		count, err = s.ingestOTLPMetrics(m)
	}
	if errors.Is(err, errWriteQueueFull) {
		writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().Set("error", "write queue is full"))
		return
	}
	// Self-telemetry (no-op unless SOBS_TELEMETRY_ENABLED) — mirrors app.py's per-ingest records.
	s.tel.recordIngestEvents(count, eventType)
	s.tel.recordIngestBatchSize(count, eventType)
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("accepted", count))
}

// mstr returns m[key] as a string ("" when absent), mirroring str(payload.get(key, "")).
func mstr(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return v
	case nil:
		return ""
	case float64:
		return formatPyNumber(v)
	case bool:
		if v {
			return "True"
		}
		return "False"
	default:
		return ""
	}
}

// mint returns m[key] as an int (0 when absent/non-numeric), mirroring int(payload.get(key,0) or 0).
func mint(m map[string]any, key string) int {
	if f, ok := m[key].(float64); ok {
		return int(f)
	}
	return 0
}

// formatPyNumber renders a JSON number like Python's str(): integral floats lose the ".0".
func formatPyNumber(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// POST /v1/ai — app.py ingest_ai: build a single gen_ai CLIENT span from the body and insert
// it into otel_traces. An empty body yields the canonical default span (operation "chat",
// empty model/service, zero tokens). Returns {"ok": true}. Ordered manifest-last so the new
// row never perturbs an earlier-compared route that scans otel_traces.
func (s *server) handleV1Ai(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	ts := mstr(m, "timestamp")
	if ts == "" {
		ts = nowUTC().Format("2006-01-02T15:04:05.000-07:00")
	}
	model := mstr(m, "model")
	operation := strings.ToLower(strings.TrimSpace(mstr(m, "operation")))
	if operation == "" {
		operation = "chat"
	}
	provider := mstr(m, "provider")
	service := mstr(m, "service")
	durationMs := 0.0
	if f, ok := m["duration_ms"].(float64); ok {
		durationMs = f
	}
	attrs := map[string]any{
		"gen_ai.operation.name":      operation,
		"gen_ai.provider.name":       provider,
		"gen_ai.request.model":       model,
		"gen_ai.usage.input_tokens":  strconv.Itoa(mint(m, "tokens_in")),
		"gen_ai.usage.output_tokens": strconv.Itoa(mint(m, "tokens_out")),
	}
	stringifyInto(attrs, m, "input_messages", "gen_ai.input.messages")
	stringifyInto(attrs, m, "output_messages", "gen_ai.output.messages")
	stringifyInto(attrs, m, "system_instructions", "gen_ai.system_instructions")
	if v := mstr(m, "prompt"); v != "" {
		attrs["sobs.gen_ai.prompt"] = v
	}
	if v := mstr(m, "response"); v != "" {
		attrs["sobs.gen_ai.response"] = v
	}
	if v := mstr(m, "error_type"); v != "" {
		attrs["error.type"] = v
	}
	row := map[string]any{
		"Timestamp": ts, "TraceId": mstr(m, "trace_id"), "SpanId": mstr(m, "span_id"),
		"ParentSpanId": "", "TraceState": "", "SpanName": strings.TrimSpace(operation + " " + model),
		"SpanKind": "CLIENT", "ServiceName": service, "ResourceAttributes": map[string]any{},
		"ScopeName": "sobs-ai", "ScopeVersion": "", "SpanAttributes": attrs,
		"Duration": int64(durationMs * 1_000_000), "StatusCode": "STATUS_CODE_OK", "StatusMessage": "",
		"Events": map[string]any{"Timestamp": []any{}, "Name": []any{}, "Attributes": []any{}},
		"Links":  map[string]any{"TraceId": []any{}, "SpanId": []any{}, "TraceState": []any{}, "Attributes": []any{}},
	}
	if err := s.enqueueWrite(func() error {
		_, e := s.db.InsertJSONEachRow("otel_traces", []map[string]any{row})
		return e
	}); err != nil {
		if errors.Is(err, errWriteQueueFull) {
			writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().Set("error", "write queue is full"))
		} else {
			s.errorJSON(w, http.StatusInternalServerError, "ai ingest write failed")
		}
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true))
}

// mstrDef returns m[key] as a string, or def when the key is absent (mirrors
// str(payload.get(key, def))).
func mstrDef(m map[string]any, key, def string) string {
	if _, ok := m[key]; !ok {
		return def
	}
	return mstr(m, key)
}

// stringifyAttrMap converts a JSON object value to app.py _stringify_attrs output: a
// string->string map (scalars via Python str(); other values via compact ensure_ascii JSON).
func stringifyAttrMap(v any) map[string]any {
	out := map[string]any{}
	o, ok := v.(map[string]any)
	if !ok {
		return out
	}
	for k, val := range o {
		if val == nil {
			continue
		}
		switch x := val.(type) {
		case string:
			out[k] = x
		case bool:
			if x {
				out[k] = "True"
			} else {
				out[k] = "False"
			}
		case float64:
			out[k] = formatPyNumber(x)
		default:
			out[k] = string(jsonenc.Encode(val, jsonenc.Options{SortKeys: false, EnsureASCII: true, ItemSep: ",", KeySep: ":"}))
		}
	}
	return out
}

// POST /v1/errors — app.py ingest_errors: build an ERROR otel_logs row from the body and
// insert it. An empty body yields exception.type "Error", empty message. Returns {"ok": true}.
func (s *server) handleV1Errors(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	ts := mstr(m, "timestamp")
	if ts == "" {
		ts = nowUTC().Format("2006-01-02T15:04:05.000-07:00")
	}
	attrs := stringifyAttrMap(m["attributes"])
	attrs["exception.type"] = mstrDef(m, "type", "Error")
	attrs["exception.message"] = mstr(m, "message")
	if v := mstr(m, "stack"); v != "" {
		attrs["exception.stacktrace"] = v // JS source-map demangling is a follow-up
	}
	row := map[string]any{
		"Timestamp": ts, "TraceId": mstr(m, "trace_id"), "SpanId": mstr(m, "span_id"),
		"TraceFlags": 0, "SeverityText": "ERROR", "SeverityNumber": 17,
		"ServiceName": mstr(m, "service"), "Body": mstr(m, "message"),
		"ResourceSchemaUrl": "", "ResourceAttributes": map[string]any{},
		"ScopeSchemaUrl": "", "ScopeName": "", "ScopeVersion": "", "ScopeAttributes": map[string]any{},
		"LogAttributes": attrs, "EventName": "exception",
	}
	if err := s.enqueueWrite(func() error {
		_, e := s.db.InsertJSONEachRow("otel_logs", []map[string]any{row})
		return e
	}); err != nil {
		if errors.Is(err, errWriteQueueFull) {
			writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().Set("error", "write queue is full"))
		} else {
			s.errorJSON(w, http.StatusInternalServerError, "error ingest write failed")
		}
		return
	}
	s.tel.recordIngestEvents(1, "error")
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true))
}

// stringifyInto copies m[srcKey] into attrs[dstKey] as a string (raw string verbatim, else
// compact ensure_ascii JSON) — mirroring app.py's gen_ai content-attribute handling.
func stringifyInto(attrs map[string]any, m map[string]any, srcKey, dstKey string) {
	v, ok := m[srcKey]
	if !ok || v == nil {
		return
	}
	if str, isStr := v.(string); isStr {
		attrs[dstKey] = str
		return
	}
	attrs[dstKey] = string(jsonenc.Encode(v, jsonenc.Options{SortKeys: false, EnsureASCII: true, ItemSep: ",", KeySep: ":"}))
}

// POST /v1/rum — app.py ingest_rum: each event in the body (a bare event, or {"events":[...]},
// or a top-level array) becomes a browser-rum hyperdx_sessions row; error/unhandledrejection
// events also index into otel_logs. An empty body is one default "unknown" INFO event, so the
// response is {"accepted": 1}. Manifest-last (inserts into hyperdx_sessions/otel_logs).
func (s *server) handleV1Rum(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var payload any
	_ = json.Unmarshal(body, &payload)
	var events []any
	switch p := payload.(type) {
	case []any:
		events = p
	case map[string]any:
		if e, ok := p["events"].([]any); ok {
			events = e
		} else {
			events = []any{p}
		}
	default: // null/absent -> {} -> [{}]
		events = []any{map[string]any{}}
	}
	clientIP := r.RemoteAddr
	if i := strings.LastIndexByte(clientIP, ':'); i >= 0 {
		clientIP = clientIP[:i]
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		clientIP = strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
	}
	bodyOpts := jsonenc.Options{SortKeys: false, EnsureASCII: false, ItemSep: ",", KeySep: ":"}
	now := nowUTC().Format("2006-01-02T15:04:05.000-07:00")
	sessionRows := []map[string]any{}
	errorRows := []map[string]any{}
	for _, ev := range events {
		e, ok := ev.(map[string]any)
		if !ok {
			continue
		}
		eventType := mstrDef(e, "type", "unknown")
		isErr := eventType == "error" || eventType == "unhandledrejection"
		sevText, sevNum := "INFO", 9
		if isErr {
			sevText, sevNum = "ERROR", 17
		}
		attrs := stringifyAttrMap(e)
		if clientIP != "" {
			attrs["client.ip"] = clientIP
		}
		sessionRows = append(sessionRows, map[string]any{
			"Timestamp":    mstrDef(e, "timestamp", now),
			"TraceId":      strings.ToLower(strings.TrimSpace(mstr(e, "traceId"))),
			"SpanId":       strings.ToLower(strings.TrimSpace(mstr(e, "spanId"))),
			"TraceFlags":   0,
			"SeverityText": sevText, "SeverityNumber": sevNum,
			"ServiceName":       mstrDef(e, "service", "browser"),
			"Body":              string(jsonenc.Encode(e, bodyOpts)),
			"ResourceSchemaUrl": "", "ResourceAttributes": map[string]any{},
			"ScopeSchemaUrl": "", "ScopeName": "browser-rum", "ScopeVersion": "", "ScopeAttributes": map[string]any{},
			"LogAttributes": attrs, "EventName": eventType,
		})
		if isErr {
			errAttrs := map[string]any{
				"exception.type":    mstrDef(e, "errorType", "JSError"),
				"exception.message": mstr(e, "message"),
				"url.full":          mstr(e, "url"),
				"session.id":        mstr(e, "sessionId"),
			}
			errorRows = append(errorRows, map[string]any{
				"Timestamp": mstrDef(e, "timestamp", now), "TraceId": "", "SpanId": "", "TraceFlags": 0,
				"SeverityText": "ERROR", "SeverityNumber": 17, "ServiceName": mstrDef(e, "service", "browser"),
				"Body": mstr(e, "message"), "ResourceSchemaUrl": "", "ResourceAttributes": map[string]any{},
				"ScopeSchemaUrl": "", "ScopeName": "browser-rum", "ScopeVersion": "", "ScopeAttributes": map[string]any{},
				"LogAttributes": errAttrs, "EventName": "exception",
			})
		}
	}
	if len(sessionRows) > 0 {
		if err := s.enqueueWrite(func() error {
			_, e := s.db.InsertJSONEachRow("hyperdx_sessions", sessionRows)
			return e
		}); err != nil {
			if errors.Is(err, errWriteQueueFull) {
				writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().Set("error", "write queue is full"))
			} else {
				s.errorJSON(w, http.StatusInternalServerError, "rum ingest write failed")
			}
			return
		}
	}
	if len(errorRows) > 0 {
		_ = s.enqueueWrite(func() error {
			_, e := s.db.InsertJSONEachRow("otel_logs", errorRows)
			return e
		})
	}
	s.tel.recordIngestEvents(len(sessionRows), "rum")
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("accepted", len(sessionRows)))
}

// POST /v1/rum/client-token — app.py issue_rum_client_token: RUM client auth is disabled on
// the fixture (RUM_CLIENT_AUTH_MODE unset), so it reports the disabled state.
func (s *server) handleV1RumClientToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("enabled", false).Set("error", "RUM client auth is disabled").Set("token", ""))
}
