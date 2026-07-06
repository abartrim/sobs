package main

// coverage_ai_helper_stream_gaps_test.go — fault-injection / data-variety tests for
// streamLLMEndpoint and streamLLMEndpointWithCallbacks. The golden corpus only ever replays a
// fixed, always-200, always-well-formed set of AI conversation fixtures, so the upstream-error
// early return, tool-call-delta accumulation, and callback-forwarding branches were never
// exercised at the unit level. These use the SAME SOBS_UPSTREAM_FIXTURES seam the corpus harness
// itself relies on for the AI surface (a canned JSON file keyed by request URL) — no live network,
// no new dependency.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store/storetest"
)

func newAIStreamTestServer() *server {
	return &server{
		db: &storetest.FakeDB{},
		wq: &writeQueue{ch: make(chan *writeTask, 64), batchMax: 200, batchWaitMs: 20},
	}
}

// writeLLMFixture writes a canned upstream response for the chat-completions URL the given
// llmRequest resolves to (fixture files are keyed by request URL, matching the parity harness).
func writeLLMFixture(t *testing.T, dir string, endpoint string, status int, content string) {
	t.Helper()
	url := chatCompletionsURL(endpoint)
	stem := upstreamFixtureKey("POST", url)
	raw, err := json.Marshal(map[string]any{"status": status, "content": content})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStreamLLMEndpoint_UpstreamError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir) // no fixture written -> upstreamFixture returns 404
	s := newAIStreamTestServer()
	req := llmRequest{endpoint: "http://mock-llm.test", model: "test-model", messages: []any{}}
	content, calls, stats := s.streamLLMEndpoint(req)
	if content != "" || calls != nil || stats != (llmStats{}) {
		t.Errorf("upstream error: content=%q calls=%v stats=%v, want all zero values", content, calls, stats)
	}
}

func TestStreamLLMEndpoint_ContentAndUsage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	sse := "data: " + `{"choices":[{"index":0,"delta":{"content":"Hello "}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{"content":"world"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"reasoning_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"
	writeLLMFixture(t, dir, "http://mock-llm.test", 200, sse)
	s := newAIStreamTestServer()
	req := llmRequest{endpoint: "http://mock-llm.test", model: "test-model", messages: []any{}}
	content, calls, stats := s.streamLLMEndpoint(req)
	if content != "Hello world" {
		t.Errorf("content = %q, want %q", content, "Hello world")
	}
	if len(calls) != 0 {
		t.Errorf("calls = %v, want none", calls)
	}
	if stats.prompt != 10 || stats.completion != 5 || stats.thinking != 2 {
		t.Errorf("stats = %+v, want prompt=10 completion=5 thinking=2 (reasoning_tokens fallback)", stats)
	}
}

func TestStreamLLMEndpoint_ToolCallAccumulation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// Arguments arrive split across two deltas for the same tool-call index, then a
	// finish_reason of "tool_calls" flushes the accumulated call.
	sse := "data: " + `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"search","arguments":"{\"q\":"}}]}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"cats\"}"}}]}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	writeLLMFixture(t, dir, "http://mock-llm.test", 200, sse)
	s := newAIStreamTestServer()
	req := llmRequest{endpoint: "http://mock-llm.test", model: "test-model", messages: []any{}}
	content, calls, _ := s.streamLLMEndpoint(req)
	if content != "" {
		t.Errorf("content = %q, want empty (tool-call-only stream)", content)
	}
	if len(calls) != 1 || calls[0].name != "search" {
		t.Fatalf("calls = %+v, want one 'search' call", calls)
	}
	q, _ := calls[0].args.Get("q")
	if q != "cats" {
		t.Errorf("tool call args[q] = %v, want %q", q, "cats")
	}
}

// TestStreamLLMEndpoint_UnflushedToolCallAtStreamEnd covers the "tool call accumulated but the
// stream ends (via [DONE]) without a tool_calls finish_reason" flush branch
// (streamLLMEndpoint's trailing `if len(toolAccum) > 0 { flush() }`).
func TestStreamLLMEndpoint_UnflushedToolCallAtStreamEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	sse := "data: " + `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"lookup","arguments":"{}"}}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	writeLLMFixture(t, dir, "http://mock-llm.test", 200, sse)
	s := newAIStreamTestServer()
	req := llmRequest{endpoint: "http://mock-llm.test", model: "test-model", messages: []any{}}
	_, calls, _ := s.streamLLMEndpoint(req)
	if len(calls) != 1 || calls[0].name != "lookup" {
		t.Fatalf("calls = %+v, want one unflushed 'lookup' call surfaced at stream end", calls)
	}
}

func TestStreamLLMEndpointWithCallbacks_Forwarding(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	sse := "data: " + `{"choices":[{"index":0,"delta":{"content":"chunk1"}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"act","arguments":"{}"}}]}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{"content":"chunk2"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	writeLLMFixture(t, dir, "http://mock-llm.test", 200, sse)
	s := newAIStreamTestServer()
	req := llmRequest{endpoint: "http://mock-llm.test", model: "test-model", messages: []any{}}

	var deltas []string
	var toolNames []string
	stats := s.streamLLMEndpointWithCallbacks(req,
		func(d string) { deltas = append(deltas, d) },
		func(tc aiToolCall) { toolNames = append(toolNames, tc.name) },
	)
	if strings.Join(deltas, "") != "chunk1chunk2" {
		t.Errorf("deltas = %v, want [chunk1 chunk2]", deltas)
	}
	if len(toolNames) != 1 || toolNames[0] != "act" {
		t.Errorf("toolNames = %v, want [act]", toolNames)
	}
	_ = stats
}

func TestStreamLLMEndpointWithCallbacks_UpstreamError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	s := newAIStreamTestServer()
	req := llmRequest{endpoint: "http://mock-llm.test", model: "test-model", messages: []any{}}
	called := false
	stats := s.streamLLMEndpointWithCallbacks(req, func(string) { called = true }, nil)
	if called {
		t.Error("onDelta should not be called on an upstream error")
	}
	if stats != (llmStats{}) {
		t.Errorf("stats = %+v, want zero value", stats)
	}
}

// TestCoerceLLMContent covers the []any content-list coercion branch (string items + {"text":...}
// object items), which the corpus's fixed conversation fixtures never happen to send.
func TestCoerceLLMContent_ListForm(t *testing.T) {
	items := []any{
		"plain ",
		mustJSONObject(t, `{"text":"from-object"}`),
		mustJSONObject(t, `{"other":"ignored"}`),
	}
	got := coerceLLMContent(items)
	if got != "plain from-object" {
		t.Errorf("coerceLLMContent(list) = %q, want %q", got, "plain from-object")
	}
}

func mustJSONObject(t *testing.T, raw string) any {
	t.Helper()
	v, err := parseJSONValue([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v
}
