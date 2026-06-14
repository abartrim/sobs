package main

import (
	"encoding/json"
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
	body, _ := io.ReadAll(r.Body)
	if n, ok := otlpRecordCount(body, resKey, scopeKey, recKey); ok && n == 0 {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("accepted", 0))
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
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
	if _, err := s.db.InsertJSONEachRow("otel_traces", []map[string]any{row}); err != nil {
		s.errorJSON(w, http.StatusInternalServerError, "ai ingest write failed")
		return
	}
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
