package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// inferGenAIProvider mirrors app.py _infer_genai_provider: classify the provider from the endpoint
// host. An empty/relative host (no netloc) yields "openai-compatible", matching urllib.parse.
func inferGenAIProvider(endpointURL string) string {
	host := ""
	if u, err := url.Parse(strings.TrimSpace(endpointURL)); err == nil {
		host = strings.ToLower(u.Host)
	}
	if host == "" {
		return "openai-compatible"
	}
	switch {
	case strings.Contains(host, "openai"):
		return "openai"
	case strings.Contains(host, "anthropic"):
		return "anthropic"
	case strings.Contains(host, "groq"):
		return "groq"
	case strings.Contains(host, "google"), strings.Contains(host, "gemini"):
		return "google"
	case strings.Contains(host, "mistral"):
		return "mistral"
	case strings.Contains(host, "deepseek"):
		return "deepseek"
	case strings.Contains(host, "ollama"):
		return "ollama"
	default:
		return "openai-compatible"
	}
}

// emitInternalGenAISpan mirrors app.py _emit_internal_genai_span in full: it writes the internal
// AI-helper gen_ai CLIENT span as an otel_traces row (SpanName "chat <model>", the gen_ai.input/
// output.messages, system_instructions, token usage, error.type/message), applies the active tag
// rules to it (record_type "ai"), AND broadcasts the live /tail event. This is the producer the AI
// conversation / span-attributes / export / AI-page read surfaces consume for self-generated LLM
// calls. The write only happens once a real LLM reply (or error) flows through callLLMChat /
// streamLLMEndpoint at runtime; under parity the upstream mock serves the canned SSE/JSON so the
// reply is empty (memory-consolidation keep_new path) — the row write is gated identically to
// Python and never touches the empty-corpus / AI-off path. elapsed_ms is always 0 in the Go port
// (frozen clock), so Duration is 0 and the broadcast duration_ms is the int 0.
//
// outputMessages may be nil to mirror Python's `output_messages is not None` gate (the
// gen_ai.output.messages attr is then omitted). The trace/span ids are random per span
// (secrets.token_hex(16)/(8)); they are never parity-compared (the row is read back only at
// runtime, never in the empty corpus).
func (s *server) emitInternalGenAISpan(endpoint, model string, inputMessages, outputMessages []any, st llmStats, errorType, errorMessage string) {
	provider := inferGenAIProvider(endpoint)
	statusCode := "STATUS_CODE_OK"
	if errorType != "" {
		statusCode = "STATUS_CODE_ERROR"
	}
	ts := nowISO()

	spanAttrs := map[string]any{
		"gen_ai.operation.name":      "chat",
		"gen_ai.provider.name":       provider,
		"gen_ai.request.model":       model,
		"gen_ai.usage.input_tokens":  st.prompt,
		"gen_ai.usage.output_tokens": st.completion,
		"gen_ai.input.messages":      string(jsonenc.Encode(inputMessages, dumpsDefault)),
	}
	if outputMessages != nil {
		spanAttrs["gen_ai.output.messages"] = string(jsonenc.Encode(outputMessages, dumpsDefault))
	}
	if sys := joinSystemInstructions(inputMessages); sys != "" {
		spanAttrs["gen_ai.system_instructions"] = sys
	}
	if st.thinking > 0 {
		spanAttrs["sobs.gen_ai.usage.thinking_tokens"] = st.thinking
	}
	if errorType != "" {
		spanAttrs["error.type"] = errorType
		if errorMessage != "" {
			spanAttrs["error.message"] = errorMessage
		}
	}
	row := map[string]any{
		"Timestamp":          ts,
		"TraceId":            tokenHex(16),
		"SpanId":             tokenHex(8),
		"ParentSpanId":       "",
		"TraceState":         "",
		"SpanName":           strings.TrimSpace("chat " + model),
		"SpanKind":           "CLIENT",
		"ServiceName":        aiHelperServiceName,
		"ResourceAttributes": map[string]any{},
		"ScopeName":          "sobs-ai",
		"ScopeVersion":       "",
		"SpanAttributes":     stringifyAttrsMap(spanAttrs),
		"Duration":           0,
		"StatusCode":         statusCode,
		"StatusMessage":      errorMessage,
	}
	// Mirror _emit_internal_genai_span's _queue_write(_op): write the row then apply tag rules
	// (record_type "ai"). Best-effort — Python wraps both in try/except.
	_ = s.enqueueWrite(func() error {
		if _, e := s.insertRowsNormalized("otel_traces", []map[string]any{row}); e != nil {
			return e
		}
		if rules := s.loadTagRulesCtx(); len(rules) > 0 {
			s.applyTagRules("ai", []map[string]any{row}, rules)
		}
		return nil
	})

	s.sseBroadcast(jsonenc.NewObject().
		Set("source", "ai").
		Set("ts", ts).
		Set("service", aiHelperServiceName).
		Set("provider", provider).
		Set("model", model).
		Set("operation", "chat").
		Set("duration_ms", 0).
		Set("tokens_in", st.prompt).
		Set("tokens_out", st.completion).
		Set("error_type", errorType))
}

// joinSystemInstructions mirrors app.py's
// "\n\n".join(content for role==system messages) used for gen_ai.system_instructions.
func joinSystemInstructions(messages []any) string {
	var parts []string
	for _, m := range messages {
		mo, ok := m.(*jsonenc.Object)
		if !ok {
			continue
		}
		roleV, _ := mo.Get("role")
		role, _ := roleV.(string)
		if strings.ToLower(strings.TrimSpace(role)) != "system" {
			continue
		}
		if cv, ok := mo.Get("content"); ok && cv != nil {
			parts = append(parts, toStr(cv))
		}
	}
	return strings.Join(parts, "\n\n")
}

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
	stream                                 bool
	tools                                  []any
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
	// _stream_llm_endpoint payload: stream + usage; tools when the model supports them.
	if req.stream {
		payload.Set("stream", true).Set("stream_options", jsonenc.NewObject().Set("include_usage", true))
	}
	if len(req.tools) > 0 {
		payload.Set("tools", req.tools)
	}
	// httpx (the app's HTTP client) serializes `json=payload` COMPACT (separators (",", ":"), raw
	// UTF-8) — NOT json.dumps' spaced default. The request body must be byte-identical to Python's so
	// the deterministic OpenAI-compatible mock can key a canned response on it (and so the real
	// upstream receives the same bytes). dumpsDefault (", "/": ") would silently differ.
	return jsonenc.Encode(payload, jsonenc.Compact)
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
	// app.py _call_llm_endpoint emits the internal gen_ai span — writing the otel_traces CLIENT
	// row + applying tag rules + the /tail _sse_broadcast — once a non-empty reply arrives
	// (app.py:4662). Go's callLLMChat does not implement the empty-content retry sub-paths, so
	// mirror the dominant success branch only.
	if strings.TrimSpace(content) != "" {
		outputMessages := []any{jsonenc.NewObject().Set("role", "assistant").Set("content", content)}
		s.emitInternalGenAISpan(req.endpoint, req.model, req.messages, outputMessages, st, "", "")
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

// checkGuardModel mirrors _check_guard_model(settings, user_input) (default context ""): it now
// delegates to the faithful aiHelperGuardCheck so the agent-flow and NL→SQL guard call sites get the
// same oss-safeguard/llama-guard selection, guard-specific thinking/max_tokens, empty-content retry,
// category parsing, benign overrides, and rich reasons. The third return is kept as llmStats for
// signature compatibility with the existing callers (which discard it); the rich guard_stats dict is
// produced internally and surfaced only on the AI-helper route.
func (s *server) checkGuardModel(userInput, context string) (bool, string, llmStats) {
	// context mirrors app.py _check_guard_model(settings, user_input, context): it wraps the guard's
	// user turn as "Context: <ctx>\n\nUser input: <userInput>". Passing "" (as before) dropped that
	// wrapper, so Go sent a different guard prompt than Python on the /query routes.
	allowed, reason, _ := s.aiHelperGuardCheck(userInput, context)
	return allowed, reason, llmStats{}
}

// generateSQLViaLLM mirrors _vanna_generate_sql: build the NL->SQL system+user messages (system
// prompt with the live schema context, user question + allowlist hint, plus the chart-generation
// guidance when a preferred chart type / chart instruction is supplied), call the LLM, and strip
// any code fences. Under parity the mock ignores the body so the canned SQL is unchanged.
//
// chartOpts is the optional (preferredChartType, chartInstruction) pair _vanna_generate_sql takes;
// existing callers that pass neither keep the bare NL→SQL behavior. When preferredChartType names a
// catalog chart type, its dataStructure type/example are appended as desired-shape guidance, exactly
// as Python does (app.py:29494-29521).
func (s *server) generateSQLViaLLM(endpoint, question string, chartOpts ...string) (string, string, llmStats) {
	preferredChartType, chartInstruction := "", ""
	if len(chartOpts) > 0 {
		preferredChartType = strings.TrimSpace(chartOpts[0])
	}
	if len(chartOpts) > 1 {
		chartInstruction = strings.TrimSpace(chartOpts[1])
	}
	model := strings.TrimSpace(s.loadAISetting("ai.model", ""))
	systemPrompt := strings.Replace(querySQLSystemPrompt, "{schema}", s.getSchemaContext(), 1)
	hintParts := make([]string, 0, len(queryAllowedTables))
	for _, t := range queryAllowedTables {
		hintParts = append(hintParts, "- "+toStr(t))
	}
	userContent := question + "\n\n" +
		"Allowed queryable tables/views (must stay within this list):\n" + strings.Join(hintParts, "\n")

	var chartGuidance []string
	if preferredChartType != "" {
		chartGuidance = append(chartGuidance, "Preferred chart type: "+preferredChartType)
	}
	if chartInstruction != "" {
		chartGuidance = append(chartGuidance, "Chart instruction: "+chartInstruction)
	}
	if preferredChartType != "" {
		if cat := s.loadChartTypesCatalog(); cat != nil {
			if ctv, ok := cat.Get("chartTypes"); ok {
				if ct, _ := ctv.(*jsonenc.Object); ct != nil {
					if iv, _ := ct.Get(preferredChartType); iv != nil {
						if info, _ := iv.(*jsonenc.Object); info != nil {
							if dv, _ := info.Get("dataStructure"); dv != nil {
								if ds, _ := dv.(*jsonenc.Object); ds != nil {
									if dsType := strings.TrimSpace(objStrOr(ds, "type")); dsType != "" {
										chartGuidance = append(chartGuidance, "Desired chart data shape: "+dsType)
									}
									if dsExample := strings.TrimSpace(objStrOr(ds, "example")); dsExample != "" {
										chartGuidance = append(chartGuidance, "Desired chart data example: "+dsExample)
									}
								}
							}
						}
					}
				}
			}
		}
	}
	if len(chartGuidance) > 0 {
		lines := make([]string, len(chartGuidance))
		for i, g := range chartGuidance {
			lines[i] = "- " + g
		}
		userContent = userContent + "\n\n" +
			"Chart generation guidance (shape SQL output to fit this):\n" + strings.Join(lines, "\n")
	}

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
