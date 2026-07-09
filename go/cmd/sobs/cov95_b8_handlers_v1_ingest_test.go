package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b8_handlers_v1_ingest_test.go — batch 8 targeted coverage for
// cmd/sobs/handlers_v1_ingest.go: the OTLP-JSON/protobuf content-type sniffing, the mstr/
// otlpRecordCount pure helpers' edge cases, and the /v1/ai, /v1/errors, /v1/rum, and
// /v1/rum/client-token handler branches not already covered by the /tail-broadcast tests.

// newV1IngestTestServer builds a minimal server whose write queue drains synchronously enough for
// these tests: cfg.Parity=true makes enqueueWrite BLOCK until the op runs (see writeQueue.enqueue),
// so assertions after the handler call observe the completed insert deterministically.
func newV1IngestTestServer(db *storetest.FakeDB) *server {
	return &server{
		cfg: config{Parity: true},
		db:  db,
		sse: newSSEBroker(),
		wq:  newDefaultWriteQueue(),
	}
}

// isOTLPProtobuf: a charset parameter is stripped before comparison; case-insensitive; a JSON
// content type (or absent header) is false.
func TestIsOTLPProtobuf(t *testing.T) {
	mk := func(ct string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/logs", nil)
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		return r
	}
	if !isOTLPProtobuf(mk("application/x-protobuf; charset=utf-8")) {
		t.Error("should strip the charset parameter and match")
	}
	if !isOTLPProtobuf(mk("APPLICATION/X-PROTOBUF")) {
		t.Error("should match case-insensitively")
	}
	if isOTLPProtobuf(mk("application/json")) {
		t.Error("application/json should not match")
	}
	if isOTLPProtobuf(mk("")) {
		t.Error("absent Content-Type should not match")
	}
}

// mstr: nil -> "", bool True/False -> Python-style strings, float64 -> formatted number, an
// unsupported type (e.g. a slice) falls to the default "" branch.
func TestMstr(t *testing.T) {
	m := map[string]any{
		"n": nil, "t": true, "f": false, "num": float64(42), "frac": 3.5, "arr": []any{1, 2},
	}
	if got := mstr(m, "n"); got != "" {
		t.Errorf("mstr(nil) = %q, want empty", got)
	}
	if got := mstr(m, "missing"); got != "" {
		t.Errorf("mstr(missing key) = %q, want empty", got)
	}
	if got := mstr(m, "t"); got != "True" {
		t.Errorf("mstr(true) = %q, want True", got)
	}
	if got := mstr(m, "f"); got != "False" {
		t.Errorf("mstr(false) = %q, want False", got)
	}
	if got := mstr(m, "num"); got != "42" {
		t.Errorf("mstr(42.0) = %q, want 42 (no trailing .0)", got)
	}
	if got := mstr(m, "frac"); got != "3.5" {
		t.Errorf("mstr(3.5) = %q, want 3.5", got)
	}
	if got := mstr(m, "arr"); got != "" {
		t.Errorf("mstr(unsupported type) = %q, want empty (default branch)", got)
	}
}

// otlpRecordCount: a non-JSON body reports ok=false; a JSON body with malformed nested shapes
// (non-array resource/scope/rec lists) still counts 0 without panicking.
func TestOtlpRecordCountMalformedNesting(t *testing.T) {
	if _, ok := otlpRecordCount([]byte("not json"), "resourceLogs", "scopeLogs", "logRecords"); ok {
		t.Error("otlpRecordCount(non-JSON) should report ok=false")
	}
	n, ok := otlpRecordCount([]byte(`{"resourceLogs":"not-an-array"}`), "resourceLogs", "scopeLogs", "logRecords")
	if !ok || n != 0 {
		t.Errorf("otlpRecordCount(malformed resource list) = (%d,%v), want (0,true)", n, ok)
	}
	n, ok = otlpRecordCount([]byte(`{"resourceLogs":[{"scopeLogs":"nope"}]}`), "resourceLogs", "scopeLogs", "logRecords")
	if !ok || n != 0 {
		t.Errorf("otlpRecordCount(malformed scope list) = (%d,%v), want (0,true)", n, ok)
	}
	n, ok = otlpRecordCount([]byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{},{}]}]}]}`), "resourceLogs", "scopeLogs", "logRecords")
	if !ok || n != 2 {
		t.Errorf("otlpRecordCount(well-formed, 2 recs) = (%d,%v), want (2,true)", n, ok)
	}
}

// v1IngestOTLP: an unparseable JSON body returns 400 "failed to read request body"; a non-object
// JSON payload (a bare array) returns 400 "failed to parse json body"; a protobuf-content-typed
// body that fails to parse returns 400 "failed to parse protobuf body"; a recordless JSON batch is
// accepted with accepted:0 and never reaches the insert path.
func TestV1IngestOTLPBadBodies(t *testing.T) {
	s := newV1IngestTestServer(&storetest.FakeDB{})

	req := httptest.NewRequest(http.MethodPost, "/v1/logs", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	s.v1IngestOTLP(rec, req, "resourceLogs", "scopeLogs", "logRecords")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "failed to read request body") {
		t.Fatalf("bad JSON: status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/logs", strings.NewReader("[1,2,3]"))
	rec = httptest.NewRecorder()
	s.v1IngestOTLP(rec, req, "resourceLogs", "scopeLogs", "logRecords")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "failed to parse json body") {
		t.Fatalf("non-object JSON: status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/logs", strings.NewReader("not valid protobuf bytes"))
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec = httptest.NewRecorder()
	s.v1IngestOTLP(rec, req, "resourceLogs", "scopeLogs", "logRecords")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "failed to parse protobuf body") {
		t.Fatalf("bad protobuf: status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/logs", strings.NewReader(`{"resourceLogs":[]}`))
	rec = httptest.NewRecorder()
	s.v1IngestOTLP(rec, req, "resourceLogs", "scopeLogs", "logRecords")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"accepted":0`) {
		t.Fatalf("recordless batch: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// v1IngestOTLP: a well-formed metrics batch dispatches through ingestOTLPMetrics and reports the
// accepted count (exercising the recKey switch's "metrics" branch, which the trace/log tail tests
// don't reach via this entry point).
func TestV1IngestOTLPMetricsDispatch(t *testing.T) {
	s := newV1IngestTestServer(&storetest.FakeDB{})
	body := `{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"cpu","gauge":{"dataPoints":[{"asDouble":1.5}]}}]}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.v1IngestOTLP(rec, req, "resourceMetrics", "scopeMetrics", "metrics")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"accepted":1`) {
		t.Fatalf("metrics dispatch: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// handleV1Ai: wrong method 404s; a duration_ms passed as a numeric STRING is parsed (the
// json.Number/string branch other than plain float64); a negative duration clamps to 0 ns.
func TestHandleV1AiMethodAndDurationParsing(t *testing.T) {
	s := newV1IngestTestServer(&storetest.FakeDB{})
	req := httptest.NewRequest(http.MethodGet, "/v1/ai", nil)
	rec := httptest.NewRecorder()
	s.handleV1Ai(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /v1/ai status = %d, want 404", rec.Code)
	}

	var lastRow map[string]any
	sIns := newV1IngestTestServer(&storetest.FakeDB{ExecuteFunc: nil})
	sIns.db = &storetest.FakeDB{}
	req = httptest.NewRequest(http.MethodPost, "/v1/ai", strings.NewReader(`{"duration_ms":"-5"}`))
	rec = httptest.NewRecorder()
	sIns.handleV1Ai(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("negative-string duration: status=%d body=%s", rec.Code, rec.Body.String())
	}
	fdb := sIns.db.(*storetest.FakeDB)
	if len(fdb.Inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(fdb.Inserts))
	}
	lastRow = fdb.Inserts[0].Rows[0]
	if lastRow["Duration"] != int64(0) {
		t.Errorf("Duration = %v, want 0 (negative clamps)", lastRow["Duration"])
	}
}

// handleV1Ai: input_messages/output_messages/system_instructions/prompt/response/error_type attrs
// are only set when present — exercise the "present" branches together.
func TestHandleV1AiOptionalAttrs(t *testing.T) {
	s := newV1IngestTestServer(&storetest.FakeDB{})
	body := `{"input_messages":[{"role":"user","content":"hi"}],"output_messages":[{"role":"assistant","content":"there"}],` +
		`"system_instructions":"be nice","prompt":"p","response":"r","error_type":"TimeoutError"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/ai", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleV1Ai(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	fdb := s.db.(*storetest.FakeDB)
	if len(fdb.Inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(fdb.Inserts))
	}
	attrs, ok := fdb.Inserts[0].Rows[0]["SpanAttributes"].(map[string]any)
	if !ok {
		t.Fatalf("SpanAttributes not a map: %#v", fdb.Inserts[0].Rows[0]["SpanAttributes"])
	}
	for _, k := range []string{"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions",
		"sobs.gen_ai.prompt", "sobs.gen_ai.response", "error.type"} {
		if _, present := attrs[k]; !present {
			t.Errorf("SpanAttributes missing %q", k)
		}
	}
}

// handleV1Errors: wrong method 404s; a stack trace attribute is only set when present; type
// defaults to "Error" when absent.
func TestHandleV1ErrorsBranches(t *testing.T) {
	s := newV1IngestTestServer(&storetest.FakeDB{})
	req := httptest.NewRequest(http.MethodGet, "/v1/errors", nil)
	rec := httptest.NewRecorder()
	s.handleV1Errors(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /v1/errors status = %d, want 404", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/errors", strings.NewReader(`{"message":"boom"}`))
	rec = httptest.NewRecorder()
	s.handleV1Errors(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	fdb := s.db.(*storetest.FakeDB)
	row := fdb.Inserts[0].Rows[0]
	attrs, _ := row["LogAttributes"].(map[string]any)
	if attrs["exception.type"] != "Error" {
		t.Errorf("exception.type default = %v, want Error", attrs["exception.type"])
	}
	if _, present := attrs["exception.stacktrace"]; present {
		t.Error("exception.stacktrace should be absent when stack is empty")
	}

	s2 := newV1IngestTestServer(&storetest.FakeDB{})
	req = httptest.NewRequest(http.MethodPost, "/v1/errors", strings.NewReader(`{"type":"TypeError","stack":"at foo()"}`))
	rec = httptest.NewRecorder()
	s2.handleV1Errors(rec, req)
	fdb2 := s2.db.(*storetest.FakeDB)
	row2 := fdb2.Inserts[0].Rows[0]
	attrs2, _ := row2["LogAttributes"].(map[string]any)
	if attrs2["exception.type"] != "TypeError" {
		t.Errorf("exception.type = %v, want TypeError", attrs2["exception.type"])
	}
	if attrs2["exception.stacktrace"] != "at foo()" {
		t.Errorf("exception.stacktrace = %v, want 'at foo()'", attrs2["exception.stacktrace"])
	}
}

// handleV1Rum: wrong method 404s; a top-level bare array of events is accepted (the []any payload
// branch); a null/absent body defaults to one synthetic empty event; the clientAuthToken field is
// stripped from the serialized Body.
func TestHandleV1RumPayloadShapes(t *testing.T) {
	s := newV1IngestTestServer(&storetest.FakeDB{})
	req := httptest.NewRequest(http.MethodGet, "/v1/rum", nil)
	rec := httptest.NewRecorder()
	s.handleV1Rum(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /v1/rum status = %d, want 404", rec.Code)
	}

	// Bare top-level array.
	sArr := newV1IngestTestServer(&storetest.FakeDB{})
	req = httptest.NewRequest(http.MethodPost, "/v1/rum", strings.NewReader(`[{"type":"pageview"},{"type":"click"}]`))
	rec = httptest.NewRecorder()
	sArr.handleV1Rum(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"accepted":2`) {
		t.Fatalf("bare array: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Empty body -> one default event.
	sEmpty := newV1IngestTestServer(&storetest.FakeDB{})
	req = httptest.NewRequest(http.MethodPost, "/v1/rum", strings.NewReader(``))
	rec = httptest.NewRecorder()
	sEmpty.handleV1Rum(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"accepted":1`) {
		t.Fatalf("empty body: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// clientAuthToken stripped from the stored Body.
	sTok := newV1IngestTestServer(&storetest.FakeDB{})
	req = httptest.NewRequest(http.MethodPost, "/v1/rum", strings.NewReader(`{"type":"pageview","clientAuthToken":"secret123"}`))
	rec = httptest.NewRecorder()
	sTok.handleV1Rum(rec, req)
	fdb := sTok.db.(*storetest.FakeDB)
	if len(fdb.Inserts) == 0 {
		t.Fatal("expected a hyperdx_sessions insert")
	}
	bodyStr, _ := fdb.Inserts[0].Rows[0]["Body"].(string)
	if strings.Contains(bodyStr, "secret123") {
		t.Errorf("stored Body leaked clientAuthToken: %s", bodyStr)
	}
}

// handleV1Rum: an error/unhandledrejection event also produces an otel_logs error row with the
// nested page/artifact/replay attribute extraction branches.
func TestHandleV1RumErrorEventNestedAttrs(t *testing.T) {
	s := newV1IngestTestServer(&storetest.FakeDB{})
	body := `{"type":"error","errorType":"ReferenceError","message":"x is not defined","url":"https://example.com/a",
		"stack":"at foo (bar.js:1:1)","errorSource":"window.onerror",
		"page":{"title":"Home","viewport":"1920x1080"},
		"artifact":{"type":"build","id":"art1","url":"https://cdn/a"},
		"replay":{"id":"rep1","url":"https://cdn/replay"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/rum", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleV1Rum(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	fdb := s.db.(*storetest.FakeDB)
	var errorInsert *storetest.Insert
	for i := range fdb.Inserts {
		if fdb.Inserts[i].Table == "otel_logs" {
			errorInsert = &fdb.Inserts[i]
		}
	}
	if errorInsert == nil {
		t.Fatal("expected an otel_logs insert for the error event")
	}
	attrs, _ := errorInsert.Rows[0]["LogAttributes"].(map[string]any)
	for _, k := range []string{"exception.type", "exception.message", "url.full", "session.id",
		"exception.stacktrace", "error.source", "browser.page.title", "browser.viewport",
		"artifact.type", "artifact.id", "artifact.url", "replay.id", "replay.url"} {
		if _, present := attrs[k]; !present {
			t.Errorf("error row LogAttributes missing %q: %#v", k, attrs)
		}
	}
}

// handleV1RumClientToken: wrong method 404s; the disabled-mode default response (no signing key
// configured, default "none" mode).
func TestHandleV1RumClientTokenDisabled(t *testing.T) {
	s := &server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/rum/client-token", nil)
	rec := httptest.NewRecorder()
	s.handleV1RumClientToken(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/rum/client-token", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	s.handleV1RumClientToken(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("disabled mode: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// handleV1RumClientToken: an invalid configured mode -> 500; a configured mode with no signing key
// -> 503; a valid mode+key with a missing origin (and no Origin header) -> 400; a valid mode+key
// with an explicit numeric-string ttlSec and origin succeeds, clamping an out-of-range ttl.
func TestHandleV1RumClientTokenModesAndTTL(t *testing.T) {
	sInvalid := &server{rumClient: rumClientConfig{mode: "bogus-mode"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/rum/client-token", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	sInvalid.handleV1RumClientToken(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("invalid mode status = %d, want 500, body=%s", rec.Code, rec.Body.String())
	}

	sNoKey := &server{rumClient: rumClientConfig{mode: "origin"}}
	req = httptest.NewRequest(http.MethodPost, "/v1/rum/client-token", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	sNoKey.handleV1RumClientToken(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no signing key status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}

	sOK := &server{rumClient: rumClientConfig{mode: "origin", signingKey: "s3cr3t", ttlSec: 900}}
	req = httptest.NewRequest(http.MethodPost, "/v1/rum/client-token", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	sOK.handleV1RumClientToken(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing origin status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}

	// ttlSec as a numeric string, exceeding the max clamp (86400).
	req = httptest.NewRequest(http.MethodPost, "/v1/rum/client-token",
		strings.NewReader(`{"origin":"https://example.com","ttlSec":"999999"}`))
	rec = httptest.NewRecorder()
	sOK.handleV1RumClientToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid request status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Errorf("body = %s, want enabled:true", rec.Body.String())
	}

	// ttlSec below the min clamp (30), passed as a plain number.
	req = httptest.NewRequest(http.MethodPost, "/v1/rum/client-token",
		strings.NewReader(`{"origin":"https://example.com","ttlSec":5}`))
	rec = httptest.NewRecorder()
	sOK.handleV1RumClientToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("low ttl request status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}
