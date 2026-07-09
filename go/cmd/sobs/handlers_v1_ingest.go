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
	raw, _ := io.ReadAll(r.Body)
	// Inflate Content-Encoding: gzip/deflate before parsing (the OTel Collector's otlphttp
	// exporter compresses by default). A decompression failure — including the 32 MiB
	// decompression-bomb cap — mirrors app.py's JSON-path error: 400 "failed to read request
	// body". (The protobuf path's distinct "failed to parse protobuf body" message is C3's.)
	body, err := decompressRequestBody(raw, r.Header.Get("Content-Encoding"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("error", "failed to read request body"))
		return
	}
	var m map[string]any
	if isOTLPProtobuf(r) {
		// OTLP-protobuf wire format — the OTel Collector otlphttp exporter default and what most
		// SDKs send. Deserialize the proto and re-render as OTLP-JSON so it feeds the SAME row
		// builders the JSON branch uses. app.py's _parse_otlp_request branches on this same
		// Content-Type and converges both wire formats on one _proto_*_to_events path.
		parsed, perr := otlpProtoToMap(body, recKey)
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("error", "failed to parse protobuf body"))
			return
		}
		m = parsed
	} else {
		// app.py _parse_otlp_request (JSON branch): payload = json.loads(body) if body else {};
		// a json.loads error -> 400 "failed to read request body"; a non-object payload (array /
		// scalar) -> 400 "failed to parse json body". An empty body parses to {} (valid).
		if len(body) == 0 {
			m = map[string]any{}
		} else {
			var payload any
			if json.Unmarshal(body, &payload) != nil {
				writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("error", "failed to read request body"))
				return
			}
			obj, ok := payload.(map[string]any)
			if !ok {
				writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("error", "failed to parse json body"))
				return
			}
			m = obj
		}
		if n, _ := otlpRecordCount(body, resKey, scopeKey, recKey); n == 0 {
			writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("accepted", 0))
			return
		}
	}
	var count int
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

// isOTLPProtobuf reports whether the request carries an OTLP-protobuf body, mirroring app.py's
// `request.mimetype == "application/x-protobuf"` (Quart's mimetype is the Content-Type with any
// `; charset=…`/boundary parameters stripped, lowercased).
func isOTLPProtobuf(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct)) == "application/x-protobuf"
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
	if r.Method != http.MethodPost { // app.py methods=["POST"] — never write on a non-POST request
		http.NotFound(w, r)
		return
	}
	m := bodyMapNumber(r)
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
	// float(payload.get("duration_ms", 0) or 0): accept numeric strings too.
	durationMs := 0.0
	switch d := m["duration_ms"].(type) {
	case json.Number:
		durationMs, _ = strconv.ParseFloat(strings.TrimSpace(string(d)), 64)
	case float64:
		durationMs = d
	case string:
		durationMs, _ = strconv.ParseFloat(strings.TrimSpace(d), 64)
	}
	// _stringify_attrs(span_attrs): the int token counts stringify via str(int) -> "5".
	attrs := map[string]any{
		"gen_ai.operation.name":      operation,
		"gen_ai.provider.name":       provider,
		"gen_ai.request.model":       model,
		"gen_ai.usage.input_tokens":  strconv.FormatInt(rumInt(m, "tokens_in"), 10),
		"gen_ai.usage.output_tokens": strconv.FormatInt(rumInt(m, "tokens_out"), 10),
	}
	if v, ok := m["input_messages"]; ok && v != nil {
		attrs["gen_ai.input.messages"] = rumStringifyContentAttr(v)
	}
	if v, ok := m["output_messages"]; ok && v != nil {
		attrs["gen_ai.output.messages"] = rumStringifyContentAttr(v)
	}
	if v, ok := m["system_instructions"]; ok && v != nil {
		attrs["gen_ai.system_instructions"] = rumStringifyContentAttr(v)
	}
	if v := mstr(m, "prompt"); v != "" {
		attrs["sobs.gen_ai.prompt"] = v
	}
	if v := mstr(m, "response"); v != "" {
		attrs["sobs.gen_ai.response"] = v
	}
	if v := mstr(m, "error_type"); v != "" {
		attrs["error.type"] = v
	}
	// max(0, int(duration_ms * 1_000_000)) — negative durations clamp to 0.
	durationNs := int64(durationMs * 1_000_000)
	if durationNs < 0 {
		durationNs = 0
	}
	row := map[string]any{
		"Timestamp": ts, "TraceId": mstr(m, "trace_id"), "SpanId": mstr(m, "span_id"),
		"ParentSpanId": "", "TraceState": "", "SpanName": strings.TrimSpace(operation + " " + model),
		"SpanKind": "CLIENT", "ServiceName": service, "ResourceAttributes": map[string]any{},
		"ScopeName": "sobs-ai", "ScopeVersion": "", "SpanAttributes": attrs,
		"Duration": durationNs, "StatusCode": "STATUS_CODE_OK", "StatusMessage": "",
		"Events": map[string]any{"Timestamp": []any{}, "Name": []any{}, "Attributes": []any{}},
		"Links":  map[string]any{"TraceId": []any{}, "SpanId": []any{}, "TraceState": []any{}, "Attributes": []any{}},
	}
	if err := s.enqueueWrite(func() error {
		// app.py ingest_ai inserts via _insert_rows_json_each_row (normalizes the DateTime columns).
		_, e := s.insertRowsNormalized("otel_traces", []map[string]any{row})
		return e
	}); err != nil {
		if errors.Is(err, errWriteQueueFull) {
			writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().Set("error", "write queue is full"))
		} else {
			s.errorJSON(w, http.StatusInternalServerError, "ai ingest write failed")
		}
		return
	}
	// app.py ingest_ai: after the write, _sse_broadcast the gen_ai event to live /tail subscribers
	// (app.py:10228). tokens use the raw int payload values (not the stringified span attrs) and
	// duration is rounded to 1 decimal; field order mirrors app.py exactly.
	s.sseBroadcast(jsonenc.NewObject().
		Set("source", "ai").
		Set("ts", ts).
		Set("service", service).
		Set("provider", provider).
		Set("model", model).
		Set("operation", operation).
		Set("duration_ms", roundHalfEven(durationMs, 1)).
		Set("tokens_in", rumInt(m, "tokens_in")).
		Set("tokens_out", rumInt(m, "tokens_out")))
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

// POST /v1/errors — app.py ingest_errors: build an ERROR otel_logs row from the body and
// insert it. An empty body yields exception.type "Error", empty message. Returns {"ok": true}.
func (s *server) handleV1Errors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { // app.py methods=["POST"] — never write on a non-POST request
		http.NotFound(w, r)
		return
	}
	m := bodyMapNumber(r)
	ts := mstr(m, "timestamp")
	if ts == "" {
		ts = nowUTC().Format("2006-01-02T15:04:05.000-07:00")
	}
	attrsIn, _ := m["attributes"].(map[string]any)
	attrs := rumStringifyAttrs(attrsIn)
	attrs["exception.type"] = mstrDef(m, "type", "Error")
	attrs["exception.message"] = mstr(m, "message")
	if v := mstr(m, "stack"); v != "" {
		attrs["exception.stacktrace"] = s.srcMap.demangleStack(v) // identity unless SOBS_SOURCE_MAP_ENABLE
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
		// app.py ingest_error inserts via _insert_rows_json_each_row (normalizes DateTime columns).
		if _, e := s.insertRowsNormalized("otel_logs", []map[string]any{row}); e != nil {
			return e
		}
		// Side-effects mirroring app.py ingest_error's _op: track discovered log attr keys and
		// apply active tag rules (record_type "error") to the inserted row.
		s.rememberLogAttrKeys(extractLogAttrMaps([]map[string]any{row}))
		if rules := s.loadTagRulesCtx(); len(rules) > 0 {
			s.applyTagRules("error", []map[string]any{row}, rules)
		}
		return nil
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
	// Parse with key order AND int/float literals preserved (decodeOrdered/UseNumber), matching
	// Python json.loads — so the per-event json.dumps(event, ensure_ascii=False) Body keeps the
	// original key order and number forms. Objects become *jsonenc.Object; arrays []any.
	var payload any
	if len(body) > 0 {
		payload, _ = parseJSONValue(body)
	}
	var events []any
	switch p := payload.(type) {
	case []any:
		events = p
	case *jsonenc.Object:
		if e, ok := p.Get("events"); ok {
			if arr, isArr := e.([]any); isArr {
				events = arr
			} else {
				events = []any{p}
			}
		} else {
			events = []any{p}
		}
	default: // null/absent -> {} -> [{}]
		events = []any{jsonenc.NewObject()}
	}
	// Optional origin-bound RUM client auth (no-op unless SOBS_RUM_CLIENT_AUTH_MODE is configured).
	if ok, status, msg := s.verifyRumClientAuth(events, r); !ok {
		writeJSON(w, status, jsonenc.NewObject().Set("error", msg))
		return
	}
	// app.py: X-Forwarded-For (first hop), else X-Real-IP, else remote_addr.
	clientIP := strings.TrimSpace(strings.SplitN(r.Header.Get("X-Forwarded-For"), ",", 2)[0])
	if clientIP == "" {
		clientIP = strings.TrimSpace(r.Header.Get("X-Real-IP"))
	}
	if clientIP == "" {
		clientIP = r.RemoteAddr
		if i := strings.LastIndexByte(clientIP, ':'); i >= 0 {
			clientIP = clientIP[:i]
		}
	}
	now := nowUTC().Format("2006-01-02T15:04:05.000-07:00")
	sessionRows := []map[string]any{}
	errorRows := []map[string]any{}
	for _, ev := range events {
		eo, ok := ev.(*jsonenc.Object)
		if !ok {
			continue
		}
		// app.py: event = dict(event); event.pop("clientAuthToken", None) — strip the auth token
		// before the event is serialized into Body / LogAttributes. Rebuild a new ordered object
		// (preserving key order) without that key.
		ebody := jsonenc.NewObject()
		for _, k := range eo.Keys() {
			if k == "clientAuthToken" {
				continue
			}
			v, _ := eo.Get(k)
			ebody.Set(k, v)
		}
		// JS source-map demangling (no-op unless SOBS_SOURCE_MAP_ENABLE); mutates the event before
		// the body is serialized, mirroring app.py ingest_rum.
		if st := rumStrRaw(objGet(ebody, "stack")); st != "" {
			ebody.Set("stack", s.srcMap.demangleStack(st))
		}
		s.srcMap.remapRumConsoleStacksObj(ebody)
		// Shallow map of the top-level fields (nested values stay *jsonenc.Object so json.dumps of
		// a nested object serializes correctly) for the field-reading / attr helpers.
		e := objShallowMap(ebody)
		// app.py: ts = event.get("timestamp", now) — NOT str()-wrapped (unlike service/type), so a
		// present non-string timestamp (bool/number/null) passes through raw into the row and chdb
		// coerces it on insert; only an absent key falls back to now. Stringifying it (mstrDef) makes
		// e.g. False/1.5 become "False"/"1.5", which chdb then rejects as a DateTime -> 500.
		var ts any = now
		if v, ok := e["timestamp"]; ok {
			ts = v
		}
		sessionID := rumStrRaw(e["sessionId"])
		eventType := mstrDef(e, "type", "unknown")
		url := rumStrRaw(e["url"])
		traceID, spanID, traceFlags := extractTraceFields(e)
		isErr := eventType == "error" || eventType == "unhandledrejection"
		sevText, sevNum := "INFO", 9
		if isErr {
			sevText, sevNum = "ERROR", 17
		}
		attrs := rumStringifyAttrs(e)
		// Browser context delta posting (compress redundant context) — browser.context.<key> attrs.
		for k, v := range handleBrowserContextDelta(e) {
			attrs[k] = v
		}
		if clientIP != "" {
			attrs["client.ip"] = clientIP
		}
		sessionRows = append(sessionRows, map[string]any{
			"Timestamp":    ts,
			"TraceId":      traceID,
			"SpanId":       spanID,
			"TraceFlags":   traceFlags,
			"SeverityText": sevText, "SeverityNumber": sevNum,
			"ServiceName":       mstrDef(e, "service", "browser"),
			"Body":              string(jsonenc.Encode(ebody, jsonenc.Compact)),
			"ResourceSchemaUrl": "", "ResourceAttributes": map[string]any{},
			"ScopeSchemaUrl": "", "ScopeName": "browser-rum", "ScopeVersion": "", "ScopeAttributes": map[string]any{},
			"LogAttributes": attrs, "EventName": eventType,
		})
		if isErr {
			errAttrs := map[string]any{
				"exception.type":    mstrDef(e, "errorType", "JSError"),
				"exception.message": rumStrRaw(e["message"]),
				"url.full":          url,
				"session.id":        sessionID,
			}
			if st := rumStrRaw(e["stack"]); st != "" {
				errAttrs["exception.stacktrace"] = st
			}
			if src := rumStrRaw(e["errorSource"]); src != "" {
				errAttrs["error.source"] = src
			}
			if page, ok := e["page"].(*jsonenc.Object); ok {
				if t := rumStrRaw(objGet(page, "title")); t != "" {
					errAttrs["browser.page.title"] = t
				}
				if vp := rumStrRaw(objGet(page, "viewport")); vp != "" {
					errAttrs["browser.viewport"] = vp
				}
			}
			if artifact, ok := e["artifact"].(*jsonenc.Object); ok {
				if v := rumStrRaw(objGet(artifact, "type")); v != "" {
					errAttrs["artifact.type"] = v
				}
				if v := rumStrRaw(objGet(artifact, "id")); v != "" {
					errAttrs["artifact.id"] = v
				}
				if v := rumStrRaw(objGet(artifact, "url")); v != "" {
					errAttrs["artifact.url"] = v
				}
			}
			if replay, ok := e["replay"].(*jsonenc.Object); ok {
				if v := rumStrRaw(objGet(replay, "id")); v != "" {
					errAttrs["replay.id"] = v
				}
				if v := rumStrRaw(objGet(replay, "url")); v != "" {
					errAttrs["replay.url"] = v
				}
			}
			errorRows = append(errorRows, map[string]any{
				"Timestamp": ts, "TraceId": traceID, "SpanId": spanID, "TraceFlags": traceFlags,
				"SeverityText": "ERROR", "SeverityNumber": 17, "ServiceName": "rum",
				"Body": rumStrRaw(e["message"]), "ResourceSchemaUrl": "", "ResourceAttributes": map[string]any{},
				"ScopeSchemaUrl": "", "ScopeName": "browser-rum", "ScopeVersion": "", "ScopeAttributes": map[string]any{},
				"LogAttributes": errAttrs, "EventName": "exception",
			})
		}
	}
	// app.py _op: both inserts + remember + tag-rules run in ONE queued write; any failure -> 500.
	if err := s.enqueueWrite(func() error {
		if len(sessionRows) > 0 {
			// app.py ingest_rum inserts via _insert_rows_json_each_row, which normalizes the
			// DateTime columns (Timestamp) — a raw user value like false/1.5/"" must be coerced the
			// same way, so go through insertRowsNormalized rather than the store method directly.
			if _, e := s.insertRowsNormalized("hyperdx_sessions", sessionRows); e != nil {
				return e
			}
		}
		if len(errorRows) > 0 {
			if _, e := s.insertRowsNormalized("otel_logs", errorRows); e != nil {
				return e
			}
		}
		// app.py: _remember_log_attr_keys(db, _extract_log_attr_maps(error_rows), record_type="log").
		s.rememberLogAttrKeys(extractLogAttrMaps(errorRows))
		// app.py applies active tag rules: "rum" to session rows, "error" to error rows.
		if rules := s.loadTagRulesCtx(); len(rules) > 0 {
			s.applyTagRules("rum", sessionRows, rules)
			if len(errorRows) > 0 {
				s.applyTagRules("error", errorRows, rules)
			}
		}
		return nil
	}); err != nil {
		if errors.Is(err, errWriteQueueFull) {
			writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().Set("error", "write queue is full"))
		} else {
			// app.py _json_error -> {"error": msg} with NO "ok" field (errorJSON would add ok:false).
			writeJSON(w, http.StatusInternalServerError, jsonenc.NewObject().Set("error", "rum ingest write failed"))
		}
		return
	}
	count := len(sessionRows)
	s.tel.recordIngestEvents(count, "rum")
	s.tel.recordIngestBatchSize(count, "rum")
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("accepted", count))
}

// POST /v1/rum/client-token — app.py issue_rum_client_token: when RUM_CLIENT_AUTH_MODE is unset
// (the fixture default) it reports the disabled state; when configured it mints an origin-bound,
// HMAC-signed, TTL'd client token.
func (s *server) handleV1RumClientToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	mode := s.rumClient.mode
	switch mode {
	case "", "none", "off", "disabled":
		writeJSON(w, http.StatusOK, jsonenc.NewObject().
			Set("enabled", false).Set("error", "RUM client auth is disabled").Set("token", ""))
		return
	case "origin", "origin-session":
	default:
		writeJSON(w, http.StatusInternalServerError, jsonenc.NewObject().Set("error", "Invalid SOBS_RUM_CLIENT_AUTH_MODE"))
		return
	}
	if s.rumClient.signingKey == "" {
		writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().Set("error", "RUM client signing key is not configured"))
		return
	}

	m := bodyMapNumber(r)
	appName := strings.TrimSpace(orDefault(toStr(m["appName"]), toStr(m["app"])))
	origin := normalizeOrigin(toStr(m["origin"]))
	if origin == "" {
		origin = requestOrigin(r)
	}
	if origin == "" {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("error", "origin is required"))
		return
	}
	// ttl_raw = payload.get("ttlSec", default); int(ttl_raw) — accepts ints, floats, AND numeric
	// strings ("3600"); a TypeError/ValueError falls back to the configured default.
	// ttlSec is parsed as int64 first since the payload's ttlSec is untrusted client input and
	// could be an arbitrarily large numeric string; the bound check below (literal 30..86400)
	// runs before any narrowing to int, so an out-of-range value can't be silently truncated.
	ttlSec64 := int64(s.rumClient.ttlSec)
	if v, ok := m["ttlSec"]; ok {
		switch t := v.(type) {
		case float64:
			ttlSec64 = int64(t)
		case json.Number:
			if i, err := strconv.ParseInt(strings.TrimSpace(string(t)), 10, 64); err == nil {
				ttlSec64 = i
			} else if f, err := strconv.ParseFloat(strings.TrimSpace(string(t)), 64); err == nil {
				ttlSec64 = int64(f)
			}
		case string:
			if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
				ttlSec64 = i
			}
		}
	}
	var ttlSec int
	switch {
	case ttlSec64 < 30:
		ttlSec = 30
	case ttlSec64 > 24*60*60:
		ttlSec = 24 * 60 * 60
	default:
		ttlSec = int(ttlSec64)
	}

	now := nowUTC().Unix()
	claims := rumClaims{Iss: "sobs-rum", App: appName, Origin: origin, Iat: now, Exp: now + int64(ttlSec), Jti: rumJTI()}
	token := s.rumClientTokenEncode(claims)
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("enabled", true).Set("token", token).Set("expiresAt", claims.Exp).Set("origin", origin).Set("app", appName))
}
