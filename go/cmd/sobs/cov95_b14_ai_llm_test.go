package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Coverage batch 14: cmd/sobs/ai_llm.go's remaining undertested branches — joinSystemInstructions,
// jnInt, normalizeChartSpecText, parseChartSpecJSON (pure functions with no prior direct test),
// plus callLLMChat/generateSQLViaLLM's error/empty-content branches (only the success path was
// exercised elsewhere, via cov95_b8_fix_query_test.go's generateNamedQueriesStats/
// generateChartSpecStats tests). llmTestServerB14/llmChatServerB14 are LOCAL copies (distinct
// names) of that file's llmTestServer/llmChatServer helper pattern — a live write queue + SSE
// broker is required because a non-empty reply drives emitInternalGenAISpan through s.wq/s.sse.

func llmTestServerB14() *server {
	return &server{
		db:  &storetest.FakeDB{},
		sse: newSSEBroker(),
		wq:  &writeQueue{ch: make(chan *writeTask, 64), batchMax: 200, batchWaitMs: 20},
	}
}

func llmChatServerB14(t *testing.T, status int, body string) string {
	t.Helper()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestJoinSystemInstructions(t *testing.T) {
	t.Run("no_system_messages", func(t *testing.T) {
		msgs := []any{jsonenc.NewObject().Set("role", "user").Set("content", "hi")}
		if got := joinSystemInstructions(msgs); got != "" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("single_system_message", func(t *testing.T) {
		msgs := []any{jsonenc.NewObject().Set("role", "System").Set("content", "be safe")}
		if got := joinSystemInstructions(msgs); got != "be safe" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("multiple_system_messages_joined", func(t *testing.T) {
		msgs := []any{
			jsonenc.NewObject().Set("role", "system").Set("content", "one"),
			jsonenc.NewObject().Set("role", "user").Set("content", "ignored"),
			jsonenc.NewObject().Set("role", "system").Set("content", "two"),
		}
		if got := joinSystemInstructions(msgs); got != "one\n\ntwo" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("non_object_message_skipped", func(t *testing.T) {
		msgs := []any{"not-an-object", jsonenc.NewObject().Set("role", "system").Set("content", "ok")}
		if got := joinSystemInstructions(msgs); got != "ok" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("nil_content_skipped", func(t *testing.T) {
		msgs := []any{jsonenc.NewObject().Set("role", "system").Set("content", nil)}
		if got := joinSystemInstructions(msgs); got != "" {
			t.Errorf("got %q", got)
		}
	})
}

func TestJnInt(t *testing.T) {
	o := jsonenc.NewObject().Set("a", json.Number("42")).Set("b", float64(7)).Set("c", "not-a-number").Set("d", json.Number("bad"))
	if got := jnInt(o, "a"); got != 42 {
		t.Errorf("json.Number = %d", got)
	}
	if got := jnInt(o, "b"); got != 7 {
		t.Errorf("float64 = %d", got)
	}
	if got := jnInt(o, "c"); got != 0 {
		t.Errorf("unsupported type = %d, want 0", got)
	}
	if got := jnInt(o, "d"); got != 0 {
		t.Errorf("bad json.Number = %d, want 0", got)
	}
	if got := jnInt(o, "missing"); got != 0 {
		t.Errorf("missing key = %d, want 0", got)
	}
}

func TestNormalizeChartSpecText(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{"prefix noise {\"a\":1} trailing noise", `{"a":1}`},
		{"no braces here", "no braces here"},
		{"  {\"a\":1}  ", `{"a":1}`},
	}
	for _, c := range cases {
		if got := normalizeChartSpecText(c.in); got != c.want {
			t.Errorf("normalizeChartSpecText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseChartSpecJSON(t *testing.T) {
	t.Run("empty_spec_errors", func(t *testing.T) {
		_, errMsg := parseChartSpecJSON("   ")
		if errMsg != "empty chart spec" {
			t.Errorf("got %q", errMsg)
		}
	})
	t.Run("malformed_json_errors", func(t *testing.T) {
		_, errMsg := parseChartSpecJSON(`{bad`)
		if errMsg == "" {
			t.Error("expected a parse error")
		}
	})
	t.Run("non_object_top_level_errors", func(t *testing.T) {
		_, errMsg := parseChartSpecJSON(`[1,2,3]`)
		if errMsg != "top-level chart spec must be a JSON object" {
			t.Errorf("got %q", errMsg)
		}
	})
	t.Run("valid_object_parses", func(t *testing.T) {
		obj, errMsg := parseChartSpecJSON("```json\n{\"chart_type\":\"line\"}\n```")
		if errMsg != "" {
			t.Fatalf("unexpected error: %q", errMsg)
		}
		if v, _ := obj.Get("chart_type"); v != "line" {
			t.Errorf("got %v", v)
		}
	})
}

func TestCallLLMChat_UpstreamHTTPError(t *testing.T) {
	s := llmTestServerB14()
	endpoint := llmChatServerB14(t, 500, `{"error":"boom"}`)
	_, _, err := s.callLLMChat(llmRequest{endpoint: endpoint, model: "gpt-4o", messages: []any{}})
	if err == nil {
		t.Fatal("expected an error for a non-2xx upstream response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("got %v", err)
	}
}

func TestCallLLMChat_NonObjectBody(t *testing.T) {
	s := llmTestServerB14()
	endpoint := llmChatServerB14(t, 200, `[1,2,3]`) // valid JSON but not an object
	content, st, err := s.callLLMChat(llmRequest{endpoint: endpoint, model: "gpt-4o", messages: []any{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "" || st.prompt != 0 {
		t.Errorf("got content=%q st=%+v", content, st)
	}
}

func TestCallLLMChat_EmptyContentSkipsSpanEmit(t *testing.T) {
	// A reply with an empty (or missing) content string must not attempt to emit the internal
	// gen_ai span — this exercises the `if strings.TrimSpace(content) != ""` false branch.
	s := &server{db: &storetest.FakeDB{}} // deliberately no sse/wq: would panic if the span-emit path ran
	endpoint := llmChatServerB14(t, 200, `{"choices":[{"message":{"content":""}}],"usage":{"prompt_tokens":1,"completion_tokens":0}}`)
	content, st, err := s.callLLMChat(llmRequest{endpoint: endpoint, model: "gpt-4o", messages: []any{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty content, got %q", content)
	}
	if st.prompt != 1 {
		t.Errorf("usage stats should still populate, got %+v", st)
	}
}

func TestCallLLMChat_SuccessEmitsSpan(t *testing.T) {
	s := llmTestServerB14()
	endpoint := llmChatServerB14(t, 200, `{"choices":[{"message":{"content":"hello there"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	content, st, err := s.callLLMChat(llmRequest{
		endpoint: endpoint, model: "gpt-4o",
		messages: []any{jsonenc.NewObject().Set("role", "user").Set("content", "hi")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "hello there" {
		t.Errorf("content = %q", content)
	}
	if st.prompt != 3 || st.completion != 2 {
		t.Errorf("got %+v", st)
	}
}

func TestGenerateSQLViaLLM_UpstreamError(t *testing.T) {
	s := llmTestServerB14()
	endpoint := llmChatServerB14(t, 500, `{}`)
	sql, errMsg, _ := s.generateSQLViaLLM(endpoint, "how many errors today?")
	if sql != "" {
		t.Errorf("expected empty sql, got %q", sql)
	}
	if errMsg != "LLM did not return a response. Check AI settings." {
		t.Errorf("got %q", errMsg)
	}
}

func TestGenerateSQLViaLLM_EmptySQLAfterFenceStrip(t *testing.T) {
	s := llmTestServerB14()
	endpoint := llmChatServerB14(t, 200, `{"choices":[{"message":{"content":"`+"```sql\\n```"+`"}}],"usage":{}}`)
	sql, errMsg, _ := s.generateSQLViaLLM(endpoint, "how many errors today?")
	if sql != "" {
		t.Errorf("expected empty sql, got %q", sql)
	}
	if errMsg != "LLM returned an empty SQL statement." {
		t.Errorf("got %q", errMsg)
	}
}

func TestGenerateSQLViaLLM_SuccessWithChartGuidance(t *testing.T) {
	s := llmTestServerB14()
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		capturedBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + "```sql\\nSELECT 1\\n```" + `"}}],"usage":{}}`))
	}))
	defer srv.Close()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", "")

	sql, errMsg, _ := s.generateSQLViaLLM(srv.URL, "count errors", "line", "make it a line chart")
	if errMsg != "" {
		t.Fatalf("unexpected error: %q", errMsg)
	}
	if sql != "SELECT 1" {
		t.Errorf("sql = %q", sql)
	}
	if !strings.Contains(capturedBody, "Preferred chart type: line") {
		t.Errorf("expected chart guidance embedded in the request body, got %s", capturedBody)
	}
	if !strings.Contains(capturedBody, "Chart instruction: make it a line chart") {
		t.Errorf("expected chart instruction embedded, got %s", capturedBody)
	}
}

func TestEmitInternalGenAISpan_ErrorBranch(t *testing.T) {
	s := llmTestServerB14()
	s.emitInternalGenAISpan("https://llm.example", "gpt-4o",
		[]any{jsonenc.NewObject().Set("role", "system").Set("content", "sys")},
		nil, llmStats{prompt: 1, completion: 0, thinking: 2}, "invalid_request", "bad input")
	// No panics + the write queue accepted the enqueued op is the primary assertion here (the
	// FakeDB accepts any insert), but also verify the queue actually drained without error by
	// giving the async writer a brief moment — enqueueWrite in TESTING/non-parity mode doesn't
	// wait, so just assert the call completed without panicking.
}

func TestChatCompletionsURL(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com/v1":           "https://api.openai.com/v1/chat/completions",
		"https://api.openai.com/v1/":          "https://api.openai.com/v1/chat/completions",
		"https://x.example/chat/completions":  "https://x.example/chat/completions",
		"https://x.example/chat/completions/": "https://x.example/chat/completions", // trailing "/" stripped first, already suffixed
	}
	for in, want := range cases {
		if got := chatCompletionsURL(in); got != want {
			t.Errorf("chatCompletionsURL(%q) = %q, want %q", in, got, want)
		}
	}
}
