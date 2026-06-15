package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// chatCompletionsURL mirrors app.py's endpoint normalization: endpoint_url.rstrip("/") with
// "/chat/completions" appended unless already present.
func chatCompletionsURL(endpoint string) string {
	base := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(base, "/chat/completions") {
		base += "/chat/completions"
	}
	return base
}

// llmStats holds the usage token counts a /chat/completions reply reports. elapsed_ms is always
// 0 in parity (frozen monotonic clock).
type llmStats struct{ prompt, completion, thinking int }

// llmRequest is the input to callLLMChat: the OpenAI-compatible /chat/completions request fields
// app.py _call_llm_endpoint sends. messages elements are *jsonenc.Object {role, content}.
type llmRequest struct {
	endpoint, model, apiKey, thinkingLevel string
	messages                               []any
	maxTokens                              int
}

// llmRequestBody builds the JSON request body (app.py _call_llm_endpoint payload +
// _llm_reasoning_payload). Built with jsonenc so nested *jsonenc.Object messages serialize.
func llmRequestBody(req llmRequest) []byte {
	maxTokens := req.maxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}
	payload := jsonenc.NewObject().
		Set("model", req.model).
		Set("messages", req.messages).
		Set("max_tokens", maxTokens)
	// _llm_reasoning_payload: include both common reasoning keys when thinking is on + supported.
	if level := normalizeThinkingLevel(req.thinkingLevel); level != "off" && modelSupportsThinking(req.model) {
		payload.Set("reasoning", jsonenc.NewObject().Set("effort", level)).Set("reasoning_effort", level)
	}
	return jsonenc.Encode(payload, dumpsDefault)
}

// llmRequestHeaders mirrors app.py _llm_request_headers.
func llmRequestHeaders(apiKey string) map[string]string {
	auth := "Bearer no-key"
	if apiKey != "" {
		auth = "Bearer " + apiKey
	}
	return map[string]string{"Content-Type": "application/json", "Authorization": auth}
}

// callLLMChat is the full app.py _call_llm_endpoint: POST {model, messages, max_tokens, reasoning}
// with the Authorization header and parse choices[0].message.content + usage. Under parity the
// request goes to the URL-keyed mock, which ignores the body, so the canned response — and the
// corpus — is unchanged; at runtime it is a real chat completion.
func (s *server) callLLMChat(req llmRequest) (string, llmStats, error) {
	resp, err := s.upstreamRequest("POST", chatCompletionsURL(req.endpoint), llmRequestBody(req), llmRequestHeaders(req.apiKey))
	if err != nil {
		return "", llmStats{}, err
	}
	if resp.Status >= 400 {
		return "", llmStats{}, fmt.Errorf("LLM endpoint returned HTTP %d", resp.Status)
	}
	obj, ok := resp.Body.(*jsonenc.Object)
	if !ok {
		return "", llmStats{}, nil
	}
	content := ""
	if cv, _ := obj.Get("choices"); cv != nil {
		if choices, _ := cv.([]any); len(choices) > 0 {
			if c0, _ := choices[0].(*jsonenc.Object); c0 != nil {
				if mv, _ := c0.Get("message"); mv != nil {
					if msg, _ := mv.(*jsonenc.Object); msg != nil {
						if cs, _ := msg.Get("content"); cs != nil {
							content, _ = cs.(string)
						}
					}
				}
			}
		}
	}
	st := llmStats{}
	if uv, _ := obj.Get("usage"); uv != nil {
		if usage, _ := uv.(*jsonenc.Object); usage != nil {
			st.prompt = jnInt(usage, "prompt_tokens")
			st.completion = jnInt(usage, "completion_tokens")
			st.thinking = jnInt(usage, "thinking_tokens")
		}
	}
	return content, st, nil
}

// callLLMEndpoint is the no-messages wrapper kept for call sites whose prompt construction is not
// yet ported: it builds a default request from the main ai.* settings (empty messages). Parity is
// unaffected (the mock ignores the body); at runtime these sites send an under-specified request
// until their messages are threaded through callLLMChat.
func (s *server) callLLMEndpoint(endpoint string) (string, llmStats, error) {
	return s.callLLMChat(llmRequest{
		endpoint:      endpoint,
		model:         strings.TrimSpace(s.loadAISetting("ai.model", "")),
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: strings.TrimSpace(s.loadAISetting("ai.thinking_level", "off")),
	})
}

// jnInt reads an int from a parsed-JSON object (json.Number-aware).
func jnInt(o *jsonenc.Object, key string) int {
	v, _ := o.Get(key)
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
	case float64:
		return int(x)
	}
	return 0
}

// queryStageStats mirrors _query_llm_stage_stats.
func queryStageStats(st llmStats) *jsonenc.Object {
	return jsonenc.NewObject().Set("prompt_tokens", st.prompt).Set("completion_tokens", st.completion).
		Set("thinking_tokens", st.thinking).Set("elapsed_ms", 0)
}

var aiGuardBlockKeywords = []string{
	"ignore previous", "disregard", "jailbreak", "bypass", "forget instructions",
	"pretend you are", "act as",
}

// heuristicGuardCheck mirrors _heuristic_guard_check: False if an obvious injection keyword.
func heuristicGuardCheck(text string) bool {
	lower := strings.ToLower(text)
	for _, kw := range aiGuardBlockKeywords {
		if strings.Contains(lower, kw) {
			return false
		}
	}
	return true
}

// parseGuardReply mirrors _parse_guard_reply (llama-guard, strict): the verdict from the first line.
func parseGuardReply(reply string) string {
	lines := []string{}
	for _, ln := range strings.Split(reply, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	first := strings.ToUpper(lines[0])
	if first == "SAFE" || first == "ALLOWED" {
		return first
	}
	if first == "UNSAFE" || first == "BLOCKED" || strings.HasPrefix(first, "BLOCKED") {
		return "UNSAFE"
	}
	return ""
}

// checkGuardModel mirrors _check_guard_model: heuristic + guard LLM verdict. Returns allowed +
// reason + the guard call's usage stats.
func (s *server) checkGuardModel(userInput string) (bool, string, llmStats) {
	if !heuristicGuardCheck(userInput) {
		return false, "Blocked by heuristic safety check", llmStats{}
	}
	guardURL := strings.TrimSpace(s.loadAISetting("ai.guard_endpoint_url", ""))
	guardModel := strings.TrimSpace(s.loadAISetting("ai.guard_model", ""))
	if guardURL == "" || guardModel == "" {
		return false, "guard_not_configured", llmStats{}
	}
	_, messages := buildLlamaGuardPrompt(userInput, "")
	reply, st, err := s.callLLMChat(llmRequest{
		endpoint:      guardURL,
		model:         guardModel,
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: strings.TrimSpace(s.loadAISetting("ai.guard_thinking_level", "off")),
		messages:      messages,
	})
	if err != nil {
		return false, "guard_error", st
	}
	switch parseGuardReply(reply) {
	case "SAFE", "ALLOWED":
		return true, "allowed", st
	default:
		return false, "blocked", st
	}
}

// generateSQLViaLLM mirrors _vanna_generate_sql: build the NL->SQL system+user messages (system
// prompt with the live schema context, user question + allowlist hint), call the LLM, and strip
// any code fences. Under parity the mock ignores the body so the canned SQL is unchanged.
func (s *server) generateSQLViaLLM(endpoint, question string) (string, string, llmStats) {
	model := strings.TrimSpace(s.loadAISetting("ai.model", ""))
	systemPrompt := strings.Replace(querySQLSystemPrompt, "{schema}", s.getSchemaContext(), 1)
	hintParts := make([]string, 0, len(queryAllowedTables))
	for _, t := range queryAllowedTables {
		hintParts = append(hintParts, "- "+toStr(t))
	}
	userContent := question + "\n\n" +
		"Allowed queryable tables/views (must stay within this list):\n" + strings.Join(hintParts, "\n")
	messages := []any{
		jsonenc.NewObject().Set("role", "system").Set("content", systemPrompt),
		jsonenc.NewObject().Set("role", "user").Set("content", userContent),
	}
	raw, st, err := s.callLLMChat(llmRequest{
		endpoint:      endpoint,
		model:         model,
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: strings.TrimSpace(s.loadAISetting("ai.thinking_level", "off")),
		messages:      messages,
	})
	if err != nil || raw == "" {
		return "", "LLM did not return a response. Check AI settings.", st
	}
	sql := strings.TrimSpace(raw)
	if strings.HasPrefix(sql, "```") {
		sql = reChartFenceOpen.ReplaceAllString(sql, "")
		sql = reChartFenceClose.ReplaceAllString(sql, "")
		sql = strings.TrimSpace(sql)
	}
	if sql == "" {
		return "", "LLM returned an empty SQL statement.", st
	}
	return sql, "", st
}

var (
	reChartFenceOpen  = regexp.MustCompile("(?s)^```[a-zA-Z]*\n?")
	reChartFenceClose = regexp.MustCompile("(?s)\n?```$")
)

// normalizeChartSpecText mirrors app.py _normalize_chart_spec_text: strip a code fence and slice
// from the first "{" to the last "}".
func normalizeChartSpecText(raw string) string {
	spec := strings.TrimSpace(raw)
	if strings.HasPrefix(spec, "```") {
		spec = reChartFenceOpen.ReplaceAllString(spec, "")
		spec = reChartFenceClose.ReplaceAllString(spec, "")
		spec = strings.TrimSpace(spec)
	}
	first := strings.Index(spec, "{")
	last := strings.LastIndex(spec, "}")
	if first >= 0 && last > first {
		spec = strings.TrimSpace(spec[first : last+1])
	}
	return spec
}

// parseChartSpecJSON mirrors app.py _parse_chart_spec_json (the local parse; the LLM repair pass
// is only reached on malformed JSON, which the canned spec never is).
func parseChartSpecJSON(raw string) (*jsonenc.Object, string) {
	spec := normalizeChartSpecText(raw)
	if spec == "" {
		return nil, "empty chart spec"
	}
	parsed, err := parseJSONValue([]byte(spec))
	if err != nil {
		return nil, err.Error()
	}
	obj, ok := parsed.(*jsonenc.Object)
	if !ok {
		return nil, "top-level chart spec must be a JSON object"
	}
	return obj, ""
}

// vannaRefineChartSpec mirrors app.py _vanna_refine_chart_spec: validate the current spec, build
// the refinement system+user messages (spec + a data sample of up to 20 rows + the user
// instruction), ask the LLM, and return the re-serialized refined spec.
func (s *server) vannaRefineChartSpec(endpoint, model, currentSpec, instruction string, columns, sampleRows []any) (string, string) {
	if endpoint == "" || model == "" {
		return "", "AI endpoint not configured."
	}
	if _, err := parseJSONValue([]byte(currentSpec)); err != nil {
		return "", "Current chart spec is invalid JSON: " + err.Error()
	}
	rows := sampleRows
	if len(rows) > 20 {
		rows = rows[:20]
	}
	sampleOpts := jsonenc.Options{SortKeys: false, EnsureASCII: false, ItemSep: ",", KeySep: ":"}
	sampleStr := string(jsonenc.Encode(jsonenc.NewObject().Set("columns", columns).Set("rows", rows), sampleOpts))
	userMsg := "Current ECharts spec structure:\n" + currentSpec +
		"\n\nData available (columns + up to 20 sample rows):\n" + sampleStr +
		"\n\nUser instruction: " + instruction +
		"\n\nPlease refine the chart spec to fulfill this request. Return only the updated JSON."
	messages := []any{
		jsonenc.NewObject().Set("role", "system").Set("content", s.buildChartRefinementPrompt()),
		jsonenc.NewObject().Set("role", "user").Set("content", userMsg),
	}
	content, _, err := s.callLLMChat(llmRequest{
		endpoint:      endpoint,
		model:         model,
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: strings.TrimSpace(s.loadAISetting("ai.thinking_level", "off")),
		messages:      messages,
	})
	if err != nil || content == "" {
		return "", "LLM did not return a refined chart spec."
	}
	parsed, perr := parseChartSpecJSON(content)
	if parsed != nil {
		return string(jsonenc.Encode(parsed, dumpsDefault)), ""
	}
	return "", "Refined chart spec JSON parse error: " + perr
}
