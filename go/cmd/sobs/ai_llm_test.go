package main

import (
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

func mustParseObj(t *testing.T, body []byte) *jsonenc.Object {
	t.Helper()
	parsed, err := parseJSONValue(body)
	if err != nil {
		t.Fatalf("parse body: %v\n%s", err, body)
	}
	obj, ok := parsed.(*jsonenc.Object)
	if !ok {
		t.Fatalf("body is not an object: %s", body)
	}
	return obj
}

func TestLLMRequestHeaders(t *testing.T) {
	if got := llmRequestHeaders("sk-abc")["Authorization"]; got != "Bearer sk-abc" {
		t.Errorf("with key: %q", got)
	}
	if got := llmRequestHeaders("")["Authorization"]; got != "Bearer no-key" {
		t.Errorf("no key: %q", got)
	}
	if got := llmRequestHeaders("x")["Content-Type"]; got != "application/json" {
		t.Errorf("content-type: %q", got)
	}
}

func TestLLMRequestBody(t *testing.T) {
	msgs := []any{
		jsonenc.NewObject().Set("role", "system").Set("content", "be safe"),
		jsonenc.NewObject().Set("role", "user").Set("content", "hi"),
	}

	// Thinking model + level on -> both reasoning keys present.
	obj := mustParseObj(t, llmRequestBody(llmRequest{model: "o1", messages: msgs, maxTokens: 256, thinkingLevel: "high"}))
	if v, _ := obj.Get("model"); v != "o1" {
		t.Errorf("model = %v", v)
	}
	if got := jnInt(obj, "max_tokens"); got != 256 {
		t.Errorf("max_tokens = %d", got)
	}
	if v, _ := obj.Get("reasoning_effort"); v != "high" {
		t.Errorf("reasoning_effort = %v", v)
	}
	if _, ok := obj.Get("reasoning"); !ok {
		t.Error("reasoning object missing")
	}
	mv, _ := obj.Get("messages")
	if arr, _ := mv.([]any); len(arr) != 2 {
		t.Errorf("messages len = %d, want 2", len(arr))
	}

	// Thinking off -> no reasoning keys; max_tokens defaults to 1024.
	off := mustParseObj(t, llmRequestBody(llmRequest{model: "o1", messages: msgs, thinkingLevel: "off"}))
	if _, ok := off.Get("reasoning_effort"); ok {
		t.Error("thinking off should omit reasoning_effort")
	}
	if got := jnInt(off, "max_tokens"); got != 1024 {
		t.Errorf("default max_tokens = %d, want 1024", got)
	}

	// Non-thinking model -> no reasoning keys even when a level is requested.
	plain := mustParseObj(t, llmRequestBody(llmRequest{model: "gpt-4o", messages: msgs, thinkingLevel: "high"}))
	if _, ok := plain.Get("reasoning_effort"); ok {
		t.Error("non-thinking model should omit reasoning_effort")
	}
}
