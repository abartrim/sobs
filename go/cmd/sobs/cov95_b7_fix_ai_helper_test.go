package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Batch 7: cmd/sobs/fix_ai_helper.go — the benign-question keyword gates (0% before this file),
// resolveGuardThinkingLevel (0%), stringifyAttrsMap (50%, missing the int64/default-JSON
// branches), guardCallLLM (63.3%: the retry-then-succeed and retry-then-fail-again branches),
// extractChatContentStats/extractFinishReason's not-an-object branches, guardStatsInt/
// guardTelemetryAttrs (0%). parseGuardReplyFull/parseOssSafeguardReply/isGptOssSafeguardModel are
// already thoroughly covered by safeguard_dm_validate_test.go, so this file does not touch them.

// ---- isBenignObservabilityQuestion / isBenignAIUsageQuestion / isBenignUINavigationRequest ----

func TestIsBenignObservabilityQuestion(t *testing.T) {
	if !isBenignObservabilityQuestion("show me the trace and span latency for checkout") {
		t.Error("two observability keywords should be benign")
	}
	if isBenignObservabilityQuestion("show me the trace") {
		t.Error("a single keyword hit should not be enough")
	}
	if isBenignObservabilityQuestion("help me exploit and steal data from traces and spans") {
		t.Error("a high-risk keyword should veto even with 2+ benign hits")
	}
	if isBenignObservabilityQuestion("what is the weather today") {
		t.Error("no benign keywords should not qualify")
	}
}

func TestIsBenignAIUsageQuestion(t *testing.T) {
	if !isBenignAIUsageQuestion("how many llm calls and tokens were used today") {
		t.Error("intent+analytics keyword combo should be benign")
	}
	if isBenignAIUsageQuestion("how many") {
		t.Error("intent keyword alone (no analytics keyword) should not qualify")
	}
	if isBenignAIUsageQuestion("model") {
		t.Error("analytics keyword alone (no intent keyword) should not qualify")
	}
	if isBenignAIUsageQuestion("show me how to exploit model tokens to steal data") {
		t.Error("high-risk keyword should veto")
	}
}

func TestIsBenignUINavigationRequest(t *testing.T) {
	if !isBenignUINavigationRequest("please navigate to the settings page") {
		t.Error("intent+surface keyword combo should be benign")
	}
	if isBenignUINavigationRequest("navigate") {
		t.Error("intent keyword alone should not qualify")
	}
	if isBenignUINavigationRequest("go to the exploit and steal the page") {
		t.Error("high-risk keyword should veto")
	}
}

// ---- resolveGuardThinkingLevel / resolveGuardMaxTokens ----

func TestResolveGuardThinkingLevel(t *testing.T) {
	if got := resolveGuardThinkingLevel("high", "gpt-4o"); got != "off" {
		t.Errorf("model without thinking support -> off, got %q", got)
	}
	if got := resolveGuardThinkingLevel("", "gpt-oss-safeguard-20b"); got != "low" {
		t.Errorf("thinking-capable model with no override -> low default, got %q", got)
	}
	if got := resolveGuardThinkingLevel("high", "gpt-oss-safeguard-20b"); got != "high" {
		t.Errorf("thinking-capable model with an explicit valid override -> that level, got %q", got)
	}
	if got := resolveGuardThinkingLevel("nonsense", "gpt-oss-safeguard-20b"); got != "off" {
		t.Errorf("invalid override string normalizes via normalizeThinkingLevel -> off, got %q", got)
	}
}

func TestResolveGuardMaxTokens(t *testing.T) {
	if got := resolveGuardMaxTokens("off"); got != 64 {
		t.Errorf("off -> 64, got %d", got)
	}
	if got := resolveGuardMaxTokens("low"); got != 256 {
		t.Errorf("non-off -> 256, got %d", got)
	}
}

// ---- stringifyAttrsMap ----

func TestStringifyAttrsMap_AllValueKinds(t *testing.T) {
	got := stringifyAttrsMap(map[string]any{
		"skip_nil":   nil,
		"str":        "hello",
		"bool_true":  true,
		"bool_false": false,
		"int_val":    int(7),
		"int64_val":  int64(9),
		"float_val":  3.5,
		"list_val":   []any{"a", "b"},
	})
	if _, ok := got["skip_nil"]; ok {
		t.Error("nil values should be omitted")
	}
	if got["str"] != "hello" {
		t.Errorf("str = %v", got["str"])
	}
	if got["bool_true"] != "True" || got["bool_false"] != "False" {
		t.Errorf("bools = %v / %v", got["bool_true"], got["bool_false"])
	}
	if got["int_val"] != "7" {
		t.Errorf("int = %v", got["int_val"])
	}
	if got["int64_val"] != "9" {
		t.Errorf("int64 = %v", got["int64_val"])
	}
	if _, ok := got["float_val"].(string); !ok {
		t.Errorf("float should stringify, got %T %v", got["float_val"], got["float_val"])
	}
	if _, ok := got["list_val"].(string); !ok {
		t.Errorf("default branch (list) should JSON-stringify, got %T %v", got["list_val"], got["list_val"])
	} else if !strings.Contains(got["list_val"].(string), "a") {
		t.Errorf("list JSON should contain its elements, got %v", got["list_val"])
	}
}

// ---- guardStatsInt ----

func TestGuardStatsInt(t *testing.T) {
	if got := guardStatsInt(nil, "x"); got != 0 {
		t.Errorf("nil object -> 0, got %d", got)
	}
	o := jsonenc.NewObject().
		Set("i", 5).
		Set("i64", int64(6)).
		Set("f", 7.0).
		Set("jn", mustJSONNumber(t, "8")).
		Set("bad", "not-a-number")
	if got := guardStatsInt(o, "missing"); got != 0 {
		t.Errorf("missing key -> 0, got %d", got)
	}
	if got := guardStatsInt(o, "i"); got != 5 {
		t.Errorf("int -> %d, want 5", got)
	}
	if got := guardStatsInt(o, "i64"); got != 6 {
		t.Errorf("int64 -> %d, want 6", got)
	}
	if got := guardStatsInt(o, "f"); got != 7 {
		t.Errorf("float64 -> %d, want 7", got)
	}
	if got := guardStatsInt(o, "jn"); got != 8 {
		t.Errorf("json.Number -> %d, want 8", got)
	}
	if got := guardStatsInt(o, "bad"); got != 0 {
		t.Errorf("unsupported kind -> 0, got %d", got)
	}
}

// ---- guardTelemetryAttrs ----

func TestGuardTelemetryAttrs_NilStats(t *testing.T) {
	got := guardTelemetryAttrs(true, "allowed", nil)
	if got["gen_ai.guard.allowed"] != "True" {
		t.Errorf("allowed = %v", got["gen_ai.guard.allowed"])
	}
	if got["gen_ai.usage.input_tokens"] != "0" {
		t.Errorf("input_tokens should default to 0 with nil stats, got %v", got["gen_ai.usage.input_tokens"])
	}
	if _, ok := got["gen_ai.system_instructions"]; ok {
		t.Error("nil stats should not add system_instructions")
	}
}

func TestGuardTelemetryAttrs_WithSystemInstructionsAndMessages(t *testing.T) {
	stats := jsonenc.NewObject().
		Set("prompt_tokens", 10).
		Set("completion_tokens", 3).
		Set("elapsed_ms", 42).
		Set("system_instructions", "be safe").
		Set("input_messages", []any{jsonenc.NewObject().Set("role", "user").Set("content", "hi")})
	got := guardTelemetryAttrs(false, "blocked", stats)
	if got["gen_ai.guard.allowed"] != "False" {
		t.Errorf("allowed = %v", got["gen_ai.guard.allowed"])
	}
	if got["gen_ai.usage.input_tokens"] != "10" || got["gen_ai.usage.output_tokens"] != "3" {
		t.Errorf("tokens = %v / %v", got["gen_ai.usage.input_tokens"], got["gen_ai.usage.output_tokens"])
	}
	if got["gen_ai.system_instructions"] != "be safe" {
		t.Errorf("system_instructions = %v", got["gen_ai.system_instructions"])
	}
	if !strings.Contains(got["gen_ai.input.messages"], "user") {
		t.Errorf("input.messages should be JSON-encoded, got %v", got["gen_ai.input.messages"])
	}
}

func TestGuardTelemetryAttrs_InputMessagesAlreadyString(t *testing.T) {
	stats := jsonenc.NewObject().Set("input_messages", `[{"role":"user"}]`)
	got := guardTelemetryAttrs(true, "allowed", stats)
	if got["gen_ai.input.messages"] != `[{"role":"user"}]` {
		t.Errorf("string input_messages should pass through as-is, got %v", got["gen_ai.input.messages"])
	}
}

// ---- extractChatContentStats / extractFinishReason not-an-object branches ----

func TestExtractChatContentStats_NilAndMalformed(t *testing.T) {
	content, st := extractChatContentStats(nil)
	if content != "" || st != (llmStats{}) {
		t.Errorf("nil object: got %q %+v", content, st)
	}
	// choices present but not objects.
	obj := jsonenc.NewObject().Set("choices", []any{"not-an-object"})
	content, st = extractChatContentStats(obj)
	if content != "" || st != (llmStats{}) {
		t.Errorf("non-object choice: got %q %+v", content, st)
	}
}

func TestExtractChatContentStats_ReasoningTokensFallback(t *testing.T) {
	// jnInt only recognizes json.Number/float64 (the shapes a real JSON decode produces), so use
	// float64 literals here rather than plain Go ints.
	obj := jsonenc.NewObject().
		Set("choices", []any{jsonenc.NewObject().Set("message", jsonenc.NewObject().Set("content", "hi"))}).
		Set("usage", jsonenc.NewObject().Set("prompt_tokens", 1.0).Set("completion_tokens", 2.0).Set("reasoning_tokens", 4.0))
	content, st := extractChatContentStats(obj)
	if content != "hi" {
		t.Errorf("content = %q", content)
	}
	if st.thinking != 4 {
		t.Errorf("thinking should fall back to reasoning_tokens, got %d", st.thinking)
	}
}

func TestExtractFinishReason_NotAnObjectOrNonString(t *testing.T) {
	obj := jsonenc.NewObject().Set("choices", []any{"nope"})
	if got := extractFinishReason(obj); got != "" {
		t.Errorf("non-object choice: got %q", got)
	}
	obj2 := jsonenc.NewObject().Set("choices", []any{jsonenc.NewObject().Set("finish_reason", 5)})
	if got := extractFinishReason(obj2); got != "" {
		t.Errorf("non-string finish_reason: got %q", got)
	}
}

// ---- guardCallLLM ----

func guardTestServer(db *storetest.FakeDB) *server {
	return &server{db: db, wq: newWQ()}
}

func TestGuardCallLLM_NoEndpointOrModel(t *testing.T) {
	s := guardTestServer(&storetest.FakeDB{})
	got := s.guardCallLLM(llmRequest{}, "")
	if got.reply != "" || got.stats == nil || got.stats.Len() != 0 {
		t.Errorf("blank endpoint/model should short-circuit, got %+v", got)
	}
}

func TestGuardCallLLM_NetworkError(t *testing.T) {
	// No SOBS_UPSTREAM_FIXTURES set, and a non-routable URL -> upstreamRequest returns an error.
	s := guardTestServer(&storetest.FakeDB{})
	got := s.guardCallLLM(llmRequest{endpoint: "http://127.0.0.1:1/nope", model: "m"}, "")
	if got.reply != "" {
		t.Errorf("network error should yield an empty reply, got %q", got.reply)
	}
	if _, ok := got.stats.Get("error"); !ok {
		t.Error("network error should set the stats error key")
	}
}

func TestGuardCallLLM_HTTPErrorStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeRawFixture(t, dir, "http://guard.test", 500, "boom detail")
	s := guardTestServer(&storetest.FakeDB{})
	got := s.guardCallLLM(llmRequest{endpoint: "http://guard.test", model: "m"}, "")
	if got.reply != "" {
		t.Errorf("HTTP 500 should yield an empty reply, got %q", got.reply)
	}
	errV, ok := got.stats.Get("error")
	if !ok || !strings.Contains(toStr(errV), "HTTP 500") {
		t.Errorf("error should mention HTTP 500, got %v", errV)
	}
}

func TestGuardCallLLM_EmptyContentThenRetrySucceeds(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// Body-keyed fixtures: the first call (original messages) returns empty content; the retry
	// call (messages + the injected retry-instruction user turn) returns real content. Both are
	// POSTs to the same URL, so they must be distinguished by request body.
	msgs := []any{jsonenc.NewObject().Set("role", "user").Set("content", "check this")}
	req := llmRequest{endpoint: "http://guard.test", model: "m", messages: msgs, maxTokens: 64}
	writeBodyKeyedFixture(t, dir, req, 200, "")
	retryMessages := append(append([]any{}, msgs...),
		jsonenc.NewObject().Set("role", "user").Set("content",
			"Your previous reply had empty message.content. Return a NON-EMPTY final answer now, content only, no reasoning trace."))
	retryReq := llmRequest{endpoint: "http://guard.test", model: "m", thinkingLevel: "off", maxTokens: 64, messages: retryMessages}
	writeBodyKeyedFixture(t, dir, retryReq, 200, "safe")

	s := guardTestServer(&storetest.FakeDB{})
	got := s.guardCallLLM(req, "")
	if got.reply != "safe" {
		t.Fatalf("expected the retry's content to win, got reply=%q stats=%v", got.reply, got.stats)
	}
}

func TestGuardCallLLM_EmptyContentRetryAlsoEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	msgs := []any{jsonenc.NewObject().Set("role", "user").Set("content", "check this")}
	req := llmRequest{endpoint: "http://guard.test", model: "m", messages: msgs, maxTokens: 64}
	writeBodyKeyedFixture(t, dir, req, 200, "")
	retryMessages := append(append([]any{}, msgs...),
		jsonenc.NewObject().Set("role", "user").Set("content",
			"Your previous reply had empty message.content. Return a NON-EMPTY final answer now, content only, no reasoning trace."))
	retryReq := llmRequest{endpoint: "http://guard.test", model: "m", thinkingLevel: "off", maxTokens: 64, messages: retryMessages}
	writeBodyKeyedFixture(t, dir, retryReq, 200, "") // retry also empty

	s := guardTestServer(&storetest.FakeDB{})
	got := s.guardCallLLM(req, "")
	if got.reply != "" {
		t.Fatalf("both empty -> empty reply, got %q", got.reply)
	}
	errV, ok := got.stats.Get("error")
	if !ok || !strings.Contains(toStr(errV), "empty content after retry") {
		t.Errorf("error should mention empty content after retry, got %v", errV)
	}
	if _, ok := got.stats.Get("retry_max_tokens"); !ok {
		t.Error("stats should carry retry_max_tokens")
	}
}

func TestGuardCallLLM_RetryTransportFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	msgs := []any{jsonenc.NewObject().Set("role", "user").Set("content", "check this")}
	req := llmRequest{endpoint: "http://guard.test", model: "m", messages: msgs, maxTokens: 64}
	writeBodyKeyedFixture(t, dir, req, 200, "") // original call: empty content
	// No fixture at all for the retry body -> 404 (>=400) -> the retry-POST-itself-failed branch.
	s := guardTestServer(&storetest.FakeDB{})
	got := s.guardCallLLM(req, "")
	if got.reply != "" {
		t.Fatalf("expected empty reply on retry failure, got %q", got.reply)
	}
	errV, ok := got.stats.Get("error")
	if !ok || !strings.Contains(toStr(errV), "HTTP 404") {
		t.Errorf("error should mention HTTP 404 for the failed retry POST, got %v", errV)
	}
}

// writeRawFixture writes a URL-keyed fixture with a raw "content" string body (used to simulate a
// non-JSON HTTP error body), mirroring writeLLMFixture's status/content shape but at an arbitrary
// status code.
func writeRawFixture(t *testing.T, dir, endpoint string, status int, content string) {
	t.Helper()
	url := chatCompletionsURL(endpoint)
	stem := upstreamFixtureKey("POST", url)
	spec := jsonenc.NewObject().Set("status", status).Set("content", content)
	raw := jsonenc.Encode(spec, dumpsDefault)
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeBodyKeyedFixture writes a fixture keyed by the exact request body llmRequestBody(req)
// produces (upstreamFixtureKeyBody), so two POSTs to the same URL with different bodies (the
// original guard call vs its empty-content retry) resolve to distinct canned responses.
func writeBodyKeyedFixture(t *testing.T, dir string, req llmRequest, status int, content string) {
	t.Helper()
	url := chatCompletionsURL(req.endpoint)
	body := llmRequestBody(req)
	stem := upstreamFixtureKeyBody("POST", url, body)
	respObj := jsonenc.NewObject().Set("choices", []any{
		jsonenc.NewObject().Set("message", jsonenc.NewObject().Set("content", content)),
	}).Set("usage", jsonenc.NewObject().Set("prompt_tokens", 5).Set("completion_tokens", 2))
	spec := jsonenc.NewObject().Set("status", status).Set("json", respObj)
	raw := jsonenc.Encode(spec, dumpsDefault)
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
