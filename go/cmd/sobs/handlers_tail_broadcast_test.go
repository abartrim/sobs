package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// These tests cover the /tail broadcast CONTENT, which the byte-parity harness cannot reach: the
// golden corpus ingests nothing, so app.py never fires _sse_broadcast and /tail emits only its
// opening frame. They drive each ingest/AI path that app.py broadcasts on (app.py:9681/9855/9871/
// 10228/3599) and assert the live subscriber receives the matching event.

// newTailTestServer builds a server with a live /tail broker and a write queue that has NO worker
// draining it, so enqueued ingest writes never run (no chdb needed) while the post-write /tail
// broadcast still fires. cfg.Parity is false (the default), so enqueueWrite returns as soon as the
// task is queued — exactly the runtime path on which app.py broadcasts.
func newTailTestServer() *server {
	return &server{
		sse: newSSEBroker(),
		wq:  &writeQueue{ch: make(chan *writeTask, 64), batchMax: 200, batchWaitMs: 20},
	}
}

// recvTailEvent reads one broadcast frame from the subscriber within a short timeout, returning the
// raw frame (to verify the source/service substrings sseEventMatches relies on) and the parsed,
// insertion-ordered object (for field + key-order assertions).
func recvTailEvent(t *testing.T, ch chan string) (string, *jsonenc.Object) {
	t.Helper()
	select {
	case raw := <-ch:
		parsed, err := parseJSONValue([]byte(raw))
		if err != nil {
			t.Fatalf("tail event is not valid JSON: %v\n%s", err, raw)
		}
		obj, ok := parsed.(*jsonenc.Object)
		if !ok {
			t.Fatalf("tail event is not an object: %s", raw)
		}
		return raw, obj
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a /tail broadcast")
		return "", nil
	}
}

func tailStr(t *testing.T, o *jsonenc.Object, key string) string {
	t.Helper()
	v, ok := o.Get(key)
	if !ok {
		t.Fatalf("tail event missing %q (keys: %v)", key, o.Keys())
	}
	s, _ := v.(string)
	return s
}

func tailNum(t *testing.T, o *jsonenc.Object, key string) string {
	t.Helper()
	v, ok := o.Get(key)
	if !ok {
		t.Fatalf("tail event missing %q (keys: %v)", key, o.Keys())
	}
	if n, ok := v.(json.Number); ok {
		return n.String()
	}
	return fmt.Sprintf("%v", v)
}

func assertKeyOrder(t *testing.T, o *jsonenc.Object, want ...string) {
	t.Helper()
	if got := strings.Join(o.Keys(), ","); got != strings.Join(want, ",") {
		t.Errorf("key order = %v, want %v", o.Keys(), want)
	}
}

func otlpAttr(key, val string) map[string]any {
	return map[string]any{"key": key, "value": map[string]any{"stringValue": val}}
}

func TestTailBroadcastLogs(t *testing.T) {
	s := newTailTestServer()
	ch := s.sse.subscribe()
	defer s.sse.unsubscribe(ch)

	body := map[string]any{"resourceLogs": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{otlpAttr("service.name", "checkout")}},
		"scopeLogs": []any{map[string]any{"logRecords": []any{map[string]any{
			"timeUnixNano": "1700000000000000000",
			"severityText": "warn",
			"body":         map[string]any{"stringValue": "disk almost full"},
		}}}},
	}}}

	n, err := s.ingestOTLPLogs(body)
	if err != nil || n != 1 {
		t.Fatalf("ingestOTLPLogs n=%d err=%v", n, err)
	}

	raw, ev := recvTailEvent(t, ch)
	if !strings.Contains(raw, `"source": "logs"`) {
		t.Errorf("raw frame missing the source token sseEventMatches needs: %s", raw)
	}
	if got := tailStr(t, ev, "source"); got != "logs" {
		t.Errorf("source = %q, want logs", got)
	}
	if got := tailStr(t, ev, "level"); got != "WARN" {
		t.Errorf("level = %q, want WARN (uppercased severityText)", got)
	}
	if got := tailStr(t, ev, "service"); got != "checkout" {
		t.Errorf("service = %q, want checkout", got)
	}
	if got := tailStr(t, ev, "body"); got != "disk almost full" {
		t.Errorf("body = %q", got)
	}
	assertKeyOrder(t, ev, "source", "ts", "level", "service", "body", "trace_id")
}

func TestTailBroadcastTracesSpanAndAI(t *testing.T) {
	s := newTailTestServer()
	ch := s.sse.subscribe()
	defer s.sse.unsubscribe(ch)

	body := map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{otlpAttr("service.name", "agent")}},
		"scopeSpans": []any{map[string]any{"spans": []any{map[string]any{
			"name":              "chat gpt-4o",
			"startTimeUnixNano": "1700000000000000000",
			"endTimeUnixNano":   "1700000000500000000", // +500ms
			"status":            map[string]any{"code": float64(1)},
			"attributes": []any{
				otlpAttr("gen_ai.provider.name", "openai"),
				otlpAttr("gen_ai.operation.name", "chat"),
				otlpAttr("gen_ai.request.model", "gpt-4o"),
			},
		}}}},
	}}}

	n, err := s.ingestOTLPTraces(body)
	if err != nil || n != 1 {
		t.Fatalf("ingestOTLPTraces n=%d err=%v", n, err)
	}

	// app.py broadcasts the span event first, then the GenAI-derived "ai" event.
	_, span := recvTailEvent(t, ch)
	if got := tailStr(t, span, "source"); got != "traces" {
		t.Fatalf("first event source = %q, want traces", got)
	}
	if got := tailStr(t, span, "name"); got != "chat gpt-4o" {
		t.Errorf("name = %q", got)
	}
	if got := tailStr(t, span, "service"); got != "agent" {
		t.Errorf("service = %q", got)
	}
	if got := tailStr(t, span, "status"); got != "OK" {
		t.Errorf("status = %q, want OK", got)
	}
	if got := tailNum(t, span, "duration_ms"); got != "500.0" {
		t.Errorf("duration_ms = %q, want 500.0", got)
	}
	assertKeyOrder(t, span, "source", "ts", "trace_id", "span_id", "name", "service", "duration_ms", "status")

	_, ai := recvTailEvent(t, ch)
	if got := tailStr(t, ai, "source"); got != "ai" {
		t.Fatalf("second event source = %q, want ai", got)
	}
	if got := tailStr(t, ai, "provider"); got != "openai" {
		t.Errorf("provider = %q", got)
	}
	if got := tailStr(t, ai, "model"); got != "gpt-4o" {
		t.Errorf("model = %q", got)
	}
	if got := tailStr(t, ai, "operation"); got != "chat" {
		t.Errorf("operation = %q", got)
	}
	assertKeyOrder(t, ai, "source", "ts", "trace_id", "span_id", "service", "provider", "model", "operation", "duration_ms", "status")
}

func TestTailBroadcastV1Ai(t *testing.T) {
	s := newTailTestServer()
	ch := s.sse.subscribe()
	defer s.sse.unsubscribe(ch)

	payload := `{"timestamp":"2024-01-02T03:04:05.000+00:00","model":"claude-3","operation":"Chat",` +
		`"provider":"anthropic","service":"assistant","duration_ms":12.34,"tokens_in":11,"tokens_out":22}`
	req := httptest.NewRequest(http.MethodPost, "/v1/ai", strings.NewReader(payload))
	rec := httptest.NewRecorder()
	s.handleV1Ai(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handleV1Ai status = %d, body = %s", rec.Code, rec.Body.String())
	}

	_, ev := recvTailEvent(t, ch)
	if got := tailStr(t, ev, "source"); got != "ai" {
		t.Errorf("source = %q, want ai", got)
	}
	if got := tailStr(t, ev, "service"); got != "assistant" {
		t.Errorf("service = %q", got)
	}
	if got := tailStr(t, ev, "provider"); got != "anthropic" {
		t.Errorf("provider = %q", got)
	}
	if got := tailStr(t, ev, "model"); got != "claude-3" {
		t.Errorf("model = %q", got)
	}
	if got := tailStr(t, ev, "operation"); got != "chat" {
		t.Errorf("operation = %q, want chat (lowercased)", got)
	}
	if got := tailNum(t, ev, "duration_ms"); got != "12.3" {
		t.Errorf("duration_ms = %q, want 12.3 (round to 1 dp)", got)
	}
	if got := tailNum(t, ev, "tokens_in"); got != "11" {
		t.Errorf("tokens_in = %q", got)
	}
	if got := tailNum(t, ev, "tokens_out"); got != "22" {
		t.Errorf("tokens_out = %q", got)
	}
	assertKeyOrder(t, ev, "source", "ts", "service", "provider", "model", "operation", "duration_ms", "tokens_in", "tokens_out")
}

func TestInferGenAIProvider(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com/v1":                 "openai",
		"https://api.anthropic.com":                 "anthropic",
		"https://api.groq.com/openai/v1":            "groq",
		"https://generativelanguage.googleapis.com": "google",
		"https://gemini.example.com":                "google",
		"https://api.mistral.ai/v1":                 "mistral",
		"https://api.deepseek.com":                  "deepseek",
		"http://ollama.local:11434/v1":              "ollama",
		"https://my-llm.internal:8000/v1":           "openai-compatible",
		"":                                          "openai-compatible",
	}
	for endpoint, want := range cases {
		if got := inferGenAIProvider(endpoint); got != want {
			t.Errorf("inferGenAIProvider(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

// TestCallLLMChatBroadcastsInternalGenAISpan proves the internal AI-helper path (app.py:3599) is
// wired end-to-end: a real LLM call through callLLMChat broadcasts the gen_ai event to /tail.
func TestCallLLMChatBroadcastsInternalGenAISpan(t *testing.T) {
	t.Setenv("SOBS_UPSTREAM_FIXTURES", "") // force the real HTTP path, not the fixture mock
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hello there"}}],` +
			`"usage":{"prompt_tokens":7,"completion_tokens":3}}`))
	}))
	defer upstream.Close()

	s := newTailTestServer()
	ch := s.sse.subscribe()
	defer s.sse.unsubscribe(ch)

	content, _, err := s.callLLMChat(llmRequest{endpoint: upstream.URL, model: "gpt-4o-mini"})
	if err != nil || content != "hello there" {
		t.Fatalf("callLLMChat content=%q err=%v", content, err)
	}

	_, ev := recvTailEvent(t, ch)
	if got := tailStr(t, ev, "source"); got != "ai" {
		t.Errorf("source = %q, want ai", got)
	}
	if got := tailStr(t, ev, "service"); got != aiHelperServiceName {
		t.Errorf("service = %q, want %q", got, aiHelperServiceName)
	}
	// httptest serves on 127.0.0.1, which matches no known provider host.
	if got := tailStr(t, ev, "provider"); got != "openai-compatible" {
		t.Errorf("provider = %q, want openai-compatible", got)
	}
	if got := tailStr(t, ev, "model"); got != "gpt-4o-mini" {
		t.Errorf("model = %q", got)
	}
	if got := tailStr(t, ev, "operation"); got != "chat" {
		t.Errorf("operation = %q", got)
	}
	if got := tailNum(t, ev, "tokens_in"); got != "7" {
		t.Errorf("tokens_in = %q", got)
	}
	if got := tailNum(t, ev, "tokens_out"); got != "3" {
		t.Errorf("tokens_out = %q", got)
	}
	if got := tailStr(t, ev, "error_type"); got != "" {
		t.Errorf("error_type = %q, want empty (success path)", got)
	}
	assertKeyOrder(t, ev, "source", "ts", "service", "provider", "model", "operation",
		"duration_ms", "tokens_in", "tokens_out", "error_type")
}
