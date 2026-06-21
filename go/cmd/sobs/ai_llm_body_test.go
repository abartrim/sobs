package main

import (
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// llmRequestBody must serialize byte-identically to the app's httpx client (`json=payload`), which
// uses COMPACT separators (",", ":") and raw UTF-8 (ensure_ascii=False). The want value below is the
// exact httpx.Request(...).content captured from the parity image for the same payload — so the
// deterministic OpenAI-compatible mock can key a canned response on the request body, and the real
// upstream receives identical bytes from both runtimes.
func TestLLMRequestBodyMatchesHTTPX(t *testing.T) {
	req := llmRequest{
		model:     "m",
		messages:  []any{jsonenc.NewObject().Set("role", "user").Set("content", "café 日本語")},
		maxTokens: 1024,
	}
	want := `{"model":"m","messages":[{"role":"user","content":"café 日本語"}],"max_tokens":1024}`
	if got := string(llmRequestBody(req)); got != want {
		t.Errorf("llmRequestBody mismatch vs httpx:\n got: %s\nwant: %s", got, want)
	}
}
