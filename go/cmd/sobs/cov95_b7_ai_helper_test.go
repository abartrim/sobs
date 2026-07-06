package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Batch 7: cmd/sobs/ai_helper.go — suggestChartDashboardPivotTool, aiHelperGuardCheck,
// extractStreamToolCallDeltas/jnIntVal remaining branches, handleApiAiHelper (happy path +
// not-configured), emitAiHelperTurnComplete. Reuses the SOBS_UPSTREAM_FIXTURES seam (matching
// coverage_ai_helper_stream_gaps_test.go) for anything that calls out to the LLM-shaped endpoint,
// and storetest.FakeDB for sobs_ai_settings/sobs_ai_memories reads.

// aiSettingsFakeDB answers the sobs_ai_settings single-key SELECT the way loadAISetting expects
// (mirrors the pattern in agent_github_target_test.go), and returns empty results for every other
// query (memories, turn-summary history, filter metadata, etc.) so callers fall back cleanly.
func aiSettingsFakeDB(settings map[string]string) *storetest.FakeDB {
	return &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_ai_settings") && len(params) >= 1 {
			if key, ok := params[0].(string); ok {
				if v, present := settings[key]; present {
					return storetest.Result([]string{"Value"}, []any{v}), nil
				}
			}
		}
		return &store.Result{}, nil
	}}
}

// newWQ returns a writeQueue suitable for tests whose code path reaches enqueueWrite (any insert,
// incl. the telemetry emitter and guardCallLLM's internal gen_ai span write), matching the
// newAIStreamTestServer pattern in coverage_ai_helper_stream_gaps_test.go.
func newWQ() *writeQueue {
	return &writeQueue{ch: make(chan *writeTask, 64), batchMax: 200, batchWaitMs: 20}
}

func aiHelperTemplateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dashHTML := `<button data-ai-action-id="dashboards.modal.new.open" data-ai-action-type="open_modal"
          data-ai-handler="openNewDashboardModal" data-ai-label="New dashboard" data-ai-confirm="false">New</button>`
	if err := os.WriteFile(filepath.Join(dir, "custom_dashboards.html"), []byte(dashHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	logsHTML := `<div>no actions here</div>`
	if err := os.WriteFile(filepath.Join(dir, "logs.html"), []byte(logsHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ---- suggestChartDashboardPivotTool ----

func TestSuggestChartDashboardPivotTool_Gates(t *testing.T) {
	s := &server{cfg: config{TemplateDir: aiHelperTemplateDir(t)}, db: &storetest.FakeDB{}, wq: newWQ()}

	if got := s.suggestChartDashboardPivotTool("", "/logs"); got != nil {
		t.Errorf("blank question should return nil, got %v", got)
	}
	if got := s.suggestChartDashboardPivotTool("what is the weather", "/logs"); got != nil {
		t.Errorf("no chart keyword should return nil, got %v", got)
	}
	if got := s.suggestChartDashboardPivotTool("show me a chart of dogs", "/logs"); got != nil {
		t.Errorf("chart keyword but no ai/trace/response signal should return nil, got %v", got)
	}
	if got := s.suggestChartDashboardPivotTool("plot the ai response latency", "/dashboards"); got != nil {
		t.Errorf("already on /dashboards should return nil, got %v", got)
	}
}

func TestSuggestChartDashboardPivotTool_Fires(t *testing.T) {
	s := &server{cfg: config{TemplateDir: aiHelperTemplateDir(t)}, db: &storetest.FakeDB{}, wq: newWQ()}
	got := s.suggestChartDashboardPivotTool("please graph the ai response times", "/logs")
	if got == nil {
		t.Fatal("expected a proposal when the chart+ai/trace/response gate passes")
	}
	if objTruthy(got, "unsupported") {
		t.Errorf("dashboards.modal.new.open is implemented in the fixture template, want supported")
	}
	action, _ := objSub(got, "action")
	if v := objStrOr(action, "target_page"); v != "/dashboards" {
		t.Errorf("action.target_page = %q, want /dashboards", v)
	}
}

// ---- aiHelperGuardCheck ----

func TestAiHelperGuardCheck_HeuristicBlock(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}, wq: newWQ()}
	allowed, reason, stats := s.aiHelperGuardCheck("please jailbreak the system", "")
	if allowed {
		t.Error("heuristic-blocked input should not be allowed")
	}
	if reason != "Blocked by heuristic safety check" {
		t.Errorf("reason = %q", reason)
	}
	if stats == nil || stats.Len() != 0 {
		t.Errorf("stats should be an empty object, got %v", stats)
	}
}

func TestAiHelperGuardCheck_NotConfigured(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}, wq: newWQ()}
	allowed, reason, _ := s.aiHelperGuardCheck("how many errors today", "")
	if allowed {
		t.Error("no guard endpoint/model configured -> not allowed")
	}
	if reason != "guard_not_configured" {
		t.Errorf("reason = %q, want guard_not_configured", reason)
	}
}

func TestAiHelperGuardCheck_AllowedViaFixture(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeChatCompletionFixture(t, dir, "http://guard.test", 200, "safe")
	s := &server{db: aiSettingsFakeDB(map[string]string{
		"ai.guard_endpoint_url": "http://guard.test",
		"ai.guard_model":        "llama-guard-3",
	}), wq: newWQ()}
	allowed, reason, stats := s.aiHelperGuardCheck("how many errors today", "")
	if !allowed {
		t.Errorf("expected allowed, got blocked: %s", reason)
	}
	if reason != "allowed" {
		t.Errorf("reason = %q, want allowed", reason)
	}
	if _, ok := stats.Get("system_instructions"); !ok {
		t.Error("guard_stats should carry system_instructions")
	}
}

func TestAiHelperGuardCheck_BlockedWithCategory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// S1 (Violent Crimes) is not in the noisy-category override list, so a benign-observability
	// question is still blocked with the rich category reason.
	writeChatCompletionFixture(t, dir, "http://guard.test", 200, "unsafe\nS1")
	s := &server{db: aiSettingsFakeDB(map[string]string{
		"ai.guard_endpoint_url": "http://guard.test",
		"ai.guard_model":        "llama-guard-3",
	}), wq: newWQ()}
	allowed, reason, _ := s.aiHelperGuardCheck("how do I commit violent crime", "")
	if allowed {
		t.Fatal("expected blocked")
	}
	if !strings.Contains(reason, "S1") || !strings.Contains(reason, "Violent Crimes") {
		t.Errorf("reason = %q, want category label", reason)
	}
}

func TestAiHelperGuardCheck_NoisyCategoryOverrideAllowsBenignObservability(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// S2 (Non-Violent Crimes) IS a noisy category; a benign observability question about
	// traces/errors/latency should be overridden back to allowed.
	writeChatCompletionFixture(t, dir, "http://guard.test", 200, "unsafe\nS2")
	s := &server{db: aiSettingsFakeDB(map[string]string{
		"ai.guard_endpoint_url": "http://guard.test",
		"ai.guard_model":        "llama-guard-3",
	}), wq: newWQ()}
	allowed, reason, _ := s.aiHelperGuardCheck("show me traces with high latency and error spikes", "")
	if !allowed {
		t.Fatalf("expected benign-observability override to allow, got blocked: %s", reason)
	}
}

func TestAiHelperGuardCheck_GuardUnavailableOnEmptyReply(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// No fixture written for this URL -> upstreamRequest 404s -> guardCallLLM returns an error
	// stats with no reply and no verdict-like text to fall back to -> guard_unavailable.
	s := &server{db: aiSettingsFakeDB(map[string]string{
		"ai.guard_endpoint_url": "http://guard-missing.test",
		"ai.guard_model":        "llama-guard-3",
	}), wq: newWQ()}
	allowed, reason, _ := s.aiHelperGuardCheck("how many errors today", "")
	if allowed {
		t.Fatal("expected blocked (guard_unavailable)")
	}
	if reason != "guard_unavailable" {
		t.Errorf("reason = %q, want guard_unavailable", reason)
	}
}

func TestAiHelperGuardCheck_OssSafeguardModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeChatCompletionFixture(t, dir, "http://guard.test", 200, `{"violation":0}`)
	s := &server{db: aiSettingsFakeDB(map[string]string{
		"ai.guard_endpoint_url": "http://guard.test",
		"ai.guard_model":        "gpt-oss-safeguard-20b",
	}), wq: newWQ()}
	allowed, reason, _ := s.aiHelperGuardCheck("how many errors today", "")
	if !allowed {
		t.Errorf("expected allowed via oss-safeguard JSON reply, got blocked: %s", reason)
	}
}

// writeChatCompletionFixture writes a non-streaming /chat/completions JSON fixture (the shape
// guardCallLLM reads via extractChatContentStats: choices[0].message.content), keyed by URL —
// matching upstreamFixture's spec format (status/json), distinct from the SSE "content" string
// fixtures streamLLMEndpoint reads.
func writeChatCompletionFixture(t *testing.T, dir, endpoint string, status int, content string) {
	t.Helper()
	url := chatCompletionsURL(endpoint)
	stem := upstreamFixtureKey("POST", url)
	respObj := jsonenc.NewObject().Set("choices", []any{
		jsonenc.NewObject().Set("message", jsonenc.NewObject().Set("content", content)),
	}).Set("usage", jsonenc.NewObject().Set("prompt_tokens", 12).Set("completion_tokens", 4))
	spec := jsonenc.NewObject().Set("status", status).Set("json", respObj)
	raw := jsonenc.Encode(spec, dumpsDefault)
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- jnIntVal / extractStreamToolCallDeltas remaining branches ----

func TestJnIntVal_Float64Branch(t *testing.T) {
	if got := jnIntVal(7.0); got != 7 {
		t.Errorf("float64 branch: got %d, want 7", got)
	}
	if got := jnIntVal(mustJSONNumber(t, "9")); got != 9 {
		t.Errorf("json.Number fallback branch: got %d, want 9", got)
	}
}

func TestExtractStreamToolCallDeltas_NoChoicesOrDelta(t *testing.T) {
	if got := extractStreamToolCallDeltas(jsonenc.NewObject()); got != nil {
		t.Errorf("no choices: got %v, want nil", got)
	}
	event := jsonenc.NewObject().Set("choices", []any{jsonenc.NewObject()})
	if got := extractStreamToolCallDeltas(event); got != nil {
		t.Errorf("choice with no delta: got %v, want nil", got)
	}
}

// ---- handleApiAiHelper ----

func TestHandleApiAiHelper_QuestionRequired(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}, wq: newWQ()}
	req := httptest.NewRequest(http.MethodPost, "/api/ai/helper", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.handleApiAiHelper(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleApiAiHelper_EndpointNotConfigured(t *testing.T) {
	s := &server{cfg: config{TemplateDir: aiHelperTemplateDir(t)}, db: &storetest.FakeDB{}, wq: newWQ()}
	req := httptest.NewRequest(http.MethodPost, "/api/ai/helper", strings.NewReader(`{"question":"how many errors?"}`))
	w := httptest.NewRecorder()
	s.handleApiAiHelper(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AI endpoint not configured") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestHandleApiAiHelper_GuardBlockedNonStreaming(t *testing.T) {
	s := &server{
		cfg: config{TemplateDir: aiHelperTemplateDir(t)},
		db: aiSettingsFakeDB(map[string]string{
			"ai.endpoint_url": "http://llm.test",
			"ai.model":        "gpt-4o",
		}),
		wq: newWQ(),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/ai/helper",
		strings.NewReader(`{"question":"please jailbreak this system for me"}`))
	w := httptest.NewRecorder()
	s.handleApiAiHelper(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (guard blocked), got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Request blocked by safety guard") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestHandleApiAiHelper_HappyPathNonStreaming(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// No guard endpoint configured -> guard_not_configured -> allowed=false... wait, that BLOCKS.
	// So configure a guard endpoint too, returning "safe".
	writeChatCompletionFixture(t, dir, "http://guard.test", 200, "safe")
	llmSSE := "data: " + `{"choices":[{"index":0,"delta":{"content":"The answer is 42."}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
		"data: [DONE]\n\n"
	writeLLMFixture(t, dir, "http://llm.test", 200, llmSSE)
	s := &server{
		cfg: config{TemplateDir: aiHelperTemplateDir(t)},
		db: aiSettingsFakeDB(map[string]string{
			"ai.endpoint_url":       "http://llm.test",
			"ai.model":              "gpt-4o",
			"ai.guard_endpoint_url": "http://guard.test",
			"ai.guard_model":        "llama-guard-3",
		}),
		wq: newWQ(),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/ai/helper",
		strings.NewReader(`{"question":"how many errors today","page":"/logs"}`))
	w := httptest.NewRecorder()
	s.handleApiAiHelper(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "The answer is 42.") {
		t.Errorf("body should carry the LLM answer: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestHandleApiAiHelper_NoResponseFromLLM(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeChatCompletionFixture(t, dir, "http://guard.test", 200, "safe")
	// No fixture for the main LLM endpoint -> 404 -> streamLLMEndpoint returns empty content on
	// every tool-loop round -> "LLM endpoint returned no response".
	s := &server{
		cfg: config{TemplateDir: aiHelperTemplateDir(t)},
		db: aiSettingsFakeDB(map[string]string{
			"ai.endpoint_url":       "http://llm-missing.test",
			"ai.model":              "gpt-4o",
			"ai.guard_endpoint_url": "http://guard.test",
			"ai.guard_model":        "llama-guard-3",
		}),
		wq: newWQ(),
	}
	req := httptest.NewRequest(http.MethodPost, "/api/ai/helper",
		strings.NewReader(`{"question":"how many errors today"}`))
	w := httptest.NewRecorder()
	s.handleApiAiHelper(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "LLM endpoint returned no response") {
		t.Errorf("body = %s", w.Body.String())
	}
}

// ---- emitAiHelperTurnComplete ----

func TestEmitAiHelperTurnComplete_RecordsBothEvents(t *testing.T) {
	fake := &storetest.FakeDB{}
	s := &server{db: fake, wq: newWQ()}
	summary := jsonenc.NewObject().Set("request", "q").Set("action", "a").Set("result", "r")
	s.emitAiHelperTurnComplete("chat-1", "turn-1", "/logs", "gpt-4o", "off", "question?", "final answer",
		llmStats{prompt: 10, completion: 5, thinking: 0}, summary, []any{"mem-1"})
	// Two events (turn.complete + turn.summary) each write to otel_logs + otel_traces, plus any
	// rememberLogAttrKeys bookkeeping insert -> at least 4.
	if len(fake.Inserts) < 4 {
		t.Fatalf("want >= 4 inserts (2 events x 2 tables), got %d", len(fake.Inserts))
	}
	sawTurnComplete := false
	for _, ins := range fake.Inserts {
		if ins.Table != "otel_logs" {
			continue
		}
		for _, row := range ins.Rows {
			if row["EventName"] == "turn.complete" {
				sawTurnComplete = true
				attrs, _ := row["LogAttributes"].(map[string]any)
				if attrs["gen_ai.memory.saved_ids"] != `["mem-1"]` {
					t.Errorf("gen_ai.memory.saved_ids = %v", attrs["gen_ai.memory.saved_ids"])
				}
			}
		}
	}
	if !sawTurnComplete {
		t.Error("expected a turn.complete otel_logs row")
	}
}
