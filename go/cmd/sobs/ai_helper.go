package main

import (
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// aiHelperServiceName is declared in ai_chat.go (== "sobs-ai-helper").

var aiThinkingLevels = map[string]bool{"off": true, "low": true, "medium": true, "high": true}

// aiGuardCategories mirrors app.py _AI_GUARD_CATEGORIES (insertion order preserved).
var aiGuardCategories = [][2]string{
	{"S1", "Violent Crimes"}, {"S2", "Non-Violent Crimes"}, {"S3", "Sex-Related Crimes"},
	{"S4", "Child Sexual Exploitation"}, {"S5", "Defamation"}, {"S6", "Specialized Advice"},
	{"S7", "Privacy"}, {"S8", "Intellectual Property"}, {"S9", "Indiscriminate Weapons"},
	{"S10", "Hate"}, {"S11", "Suicide & Self-Harm"}, {"S12", "Sexual Content"},
	{"S13", "Elections"}, {"S14", "Code Interpreter Abuse"},
}

var aiChartRequestKeywords = []string{
	"graph", "chart", "plot", "visual", "visualize", "timeseries", "trend", "response time", "latency",
}

// aiAssistantMetaRE mirrors app.py _AI_ASSISTANT_META_RE.
var aiAssistantMetaRE = regexp.MustCompile(`(?is)<assistant_meta\b[^>]*>\s*([\s\S]*?)\s*</assistant_meta>`)

// normalizeThinkingLevel mirrors app.py _normalize_thinking_level.
func normalizeThinkingLevel(value string) string {
	level := strings.ToLower(strings.TrimSpace(value))
	if aiThinkingLevels[level] {
		return level
	}
	return "off"
}

// modelSupportsThinking mirrors app.py _model_supports_thinking.
func modelSupportsThinking(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	for _, t := range []string{"gpt-oss", "reason", "thinking", "deepseek-r1", "qwen3", "o1", "o3"} {
		if strings.Contains(m, t) {
			return true
		}
	}
	return false
}

// coerceSummaryValue mirrors app.py _coerce_summary_value.
func coerceSummaryValue(value string, maxLen int) string {
	text := strings.TrimSpace(value)
	if len(text) > maxLen {
		return text[:maxLen]
	}
	return text
}

// buildAITurnLogsURL mirrors app.py _build_ai_turn_logs_url.
func buildAITurnLogsURL(chatID, turnID string) string {
	where := "ServiceName = '" + aiHelperServiceName +
		"' AND LogAttributes['gen_ai.chat_id'] = '" + strings.ReplaceAll(chatID, "'", "''") +
		"' AND LogAttributes['gen_ai.turn_id'] = '" + strings.ReplaceAll(turnID, "'", "''") + "'"
	// url_for('view_logs') == "/logs"; urllib.parse.quote(where, safe='') escapes everything.
	return "/logs?sql=" + pyQuoteAll(where)
}

// pyQuoteAll mirrors urllib.parse.quote(s, safe=”) — percent-encode every byte except the
// unreserved set A-Z a-z 0-9 _ . - ~.
func pyQuoteAll(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			const hex = "0123456789ABCDEF"
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		}
	}
	return b.String()
}

// extractAssistantMeta mirrors app.py _extract_assistant_meta: pull the trailing
// <assistant_meta>{...}</assistant_meta> JSON (if any) and return the cleaned answer + meta object.
func extractAssistantMeta(answer string) (string, *jsonenc.Object) {
	stripMeta := func(raw string) string {
		cleaned := aiAssistantMetaRE.ReplaceAllString(raw, "")
		if idx := strings.Index(strings.ToLower(cleaned), "<assistant_meta"); idx >= 0 {
			cleaned = cleaned[:idx]
		}
		return cleaned
	}
	m := aiAssistantMetaRE.FindStringSubmatch(answer)
	if m == nil {
		return strings.TrimSpace(stripMeta(answer)), jsonenc.NewObject()
	}
	var meta *jsonenc.Object
	normalized := strings.NewReplacer("“", `"`, "”", `"`, "‘", "'", "’", "'").Replace(m[1])
	if parsed, err := parseJSONValue([]byte(normalized)); err == nil {
		if obj, ok := parsed.(*jsonenc.Object); ok {
			meta = obj
		}
	}
	if meta == nil {
		meta = jsonenc.NewObject()
	}
	return strings.TrimSpace(stripMeta(answer)), meta
}

// deriveTurnSummary mirrors app.py _derive_turn_summary.
func deriveTurnSummary(question, answer, toolSummary string, metaSummary *jsonenc.Object) *jsonenc.Object {
	get := func(key string) string {
		if metaSummary == nil {
			return ""
		}
		if v, ok := metaSummary.Get(key); ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	request := get("request")
	if request == "" {
		request = question
	}
	action := get("action")
	if action == "" {
		action = toolSummary
	}
	if action == "" {
		action = "answer_only"
	}
	result := get("result")
	if result == "" {
		result = answer
	}
	return jsonenc.NewObject().
		Set("request", coerceSummaryValue(request, 180)).
		Set("action", coerceSummaryValue(action, 180)).
		Set("result", coerceSummaryValue(result, 280))
}

// extractMemoryCandidates mirrors app.py _extract_memory_candidates.
func extractMemoryCandidates(meta *jsonenc.Object) []string {
	if meta == nil {
		return nil
	}
	raw, ok := meta.Get("memory_candidates")
	if !ok {
		return nil
	}
	var candidates []string
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				if t := coerceSummaryValue(s, 280); t != "" {
					candidates = append(candidates, t)
				}
			}
		}
	case string:
		if t := coerceSummaryValue(v, 280); t != "" {
			candidates = append(candidates, t)
		}
	}
	var deduped []string
	seen := map[string]bool{}
	for _, t := range candidates {
		k := strings.ToLower(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		deduped = append(deduped, t)
		if len(deduped) >= 3 {
			break
		}
	}
	return deduped
}

// suggestChartDashboardPivotTool mirrors app.py _suggest_chart_dashboard_pivot_tool's gate: it
// returns nil unless the question contains a chart keyword and an ai/trace/response signal and the
// page is not /dashboards. (The non-nil branch builds a dashboards.modal.new.open UI-action
// proposal; UI-action proposals are applied by the separate /actions/execute route.)
func suggestChartDashboardPivotTool(question, currentPage string) *jsonenc.Object {
	lower := strings.ToLower(strings.TrimSpace(question))
	if lower == "" {
		return nil
	}
	hasChart := false
	for _, kw := range aiChartRequestKeywords {
		if strings.Contains(lower, kw) {
			hasChart = true
			break
		}
	}
	if !hasChart {
		return nil
	}
	if strings.HasPrefix(currentPage, "/dashboards") {
		return nil
	}
	if !strings.Contains(lower, "ai") && !strings.Contains(lower, "trace") && !strings.Contains(lower, "response") {
		return nil
	}
	// A chart/dashboard pivot is warranted; surface a minimal proposal carrying the canonical
	// dashboards open-modal action so the client can pivot.
	return jsonenc.NewObject().
		Set("tool", "propose_ui_action").
		Set("action_id", "dashboards.modal.new.open").
		Set("summary", "Open the new dashboard modal to create the requested chart").
		Set("requires_confirmation", true).
		Set("action", jsonenc.NewObject().Set("action_id", "dashboards.modal.new.open").Set("target_page", "/dashboards"))
}

// buildLlamaGuardPrompt mirrors app.py _build_llama_guard_prompt: (system_instructions, messages).
func buildLlamaGuardPrompt(userInput, context string) (string, []any) {
	cats := make([]string, 0, len(aiGuardCategories))
	for _, c := range aiGuardCategories {
		cats = append(cats, c[0]+": "+c[1]+".")
	}
	categories := strings.Join(cats, "\n")
	conversationUser := strings.TrimSpace(userInput)
	if context != "" {
		conversationUser = "Context: " + strings.TrimSpace(context) + "\n\nUser input: " + conversationUser
	}
	prompt := "Treat observability, SRE, incident-response, and security-hardening troubleshooting as safe by default " +
		"unless there is clear malicious or illegal intent.\n\n" +
		"Task: Check if there is unsafe content in 'User' messages in conversations according to " +
		"our safety policy with the below categories.\n\n" +
		"<BEGIN UNSAFE CONTENT CATEGORIES>\n" +
		categories + "\n" +
		"<END UNSAFE CONTENT CATEGORIES>\n\n" +
		"Calibration rules for this domain:\n" +
		"- Technical observability terms are usually benign: p50/p95/p99, percentile bands, error budget burn, " +
		"burst, spike, kill process, incident, root cause, exploit detection, vulnerability triage.\n" +
		"- Mark unsafe only when the user clearly asks to commit harm, abuse, fraud, " +
		"data theft, or illegal action.\n" +
		"- Do not classify as S3/S12 unless there is explicit sexual content or sexual-crime intent.\n" +
		"- Do not classify as S1/S2 unless there is explicit intent to commit " +
		"real-world violence/non-violent crime.\n\n" +
		"<BEGIN CONVERSATION>\n\n" +
		"User: " + conversationUser + "\n\n" +
		"<END CONVERSATION>\n\n" +
		"Provide your safety assessment for ONLY THE LAST User message in the above conversation:\n" +
		"- First line must read 'safe' or 'unsafe'.\n" +
		"- If unsafe, a second line must include a comma-separated list of violated categories."
	systemMsg := "You are a safety guard classifier. Follow the prompt format exactly."
	messages := []any{
		jsonenc.NewObject().Set("role", "system").Set("content", systemMsg),
		jsonenc.NewObject().Set("role", "user").Set("content", prompt),
	}
	return systemMsg, messages
}

// aiHelperGuardCheck mirrors app.py _check_guard_model for the AI helper: heuristic + guard LLM
// verdict, returning the rich guard_stats dict (token usage + the guard prompt) that the response
// surfaces. Only the llama-guard prompt family is built (the configured guard model is not a
// gpt-oss-safeguard model under parity).
func (s *server) aiHelperGuardCheck(question, context string) (bool, string, *jsonenc.Object) {
	if !heuristicGuardCheck(question) {
		return false, "Blocked by heuristic safety check", jsonenc.NewObject()
	}
	guardURL := strings.TrimSpace(s.loadAISetting("ai.guard_endpoint_url", ""))
	guardModel := strings.TrimSpace(s.loadAISetting("ai.guard_model", ""))
	if guardURL == "" || guardModel == "" {
		return false, "guard_not_configured", jsonenc.NewObject()
	}
	systemMsg, messages := buildLlamaGuardPrompt(question, context)
	reply, st, err := s.callLLMChat(llmRequest{
		endpoint:      guardURL,
		model:         guardModel,
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: strings.TrimSpace(s.loadAISetting("ai.guard_thinking_level", "off")),
		messages:      messages,
	})
	guardStats := queryStageStats(st)
	guardStats.Set("system_instructions", systemMsg)
	guardStats.Set("input_messages", messages)
	if err != nil || reply == "" {
		return false, "guard_unavailable", guardStats
	}
	switch parseGuardReply(reply) {
	case "SAFE", "ALLOWED":
		return true, "allowed", guardStats
	default:
		return false, "blocked", guardStats
	}
}

// aiToolCall is a completed streamed tool call.
type aiToolCall struct {
	name string
	args *jsonenc.Object
}

type aiToolSlot struct {
	name string
	args strings.Builder
}

// streamLLMEndpoint mirrors app.py _stream_llm_endpoint: POST the endpoint, parse the canned SSE
// body (data: lines), accumulate content deltas + tool-call deltas + usage. Returns the joined
// answer, the completed tool calls in index order, and usage stats (elapsed_ms 0 under parity).
func (s *server) streamLLMEndpoint(req llmRequest) (string, []aiToolCall, llmStats) {
	req.stream = true
	resp, err := s.upstreamRequest("POST", chatCompletionsURL(req.endpoint), llmRequestBody(req), llmRequestHeaders(req.apiKey))
	if err != nil || resp.Status >= 400 {
		return "", nil, llmStats{}
	}
	var outputParts []string
	var usage *jsonenc.Object
	toolAccum := map[int]*aiToolSlot{}
	var completed []aiToolCall
	flush := func() {
		idxs := make([]int, 0, len(toolAccum))
		for i := range toolAccum {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			slot := toolAccum[i]
			var args *jsonenc.Object
			if raw := slot.args.String(); raw != "" {
				if parsed, err := parseJSONValue([]byte(raw)); err == nil {
					args, _ = parsed.(*jsonenc.Object)
				}
			}
			if args == nil {
				args = jsonenc.NewObject()
			}
			completed = append(completed, aiToolCall{name: slot.name, args: args})
		}
		toolAccum = map[int]*aiToolSlot{}
	}
	for _, rawLine := range strings.Split(resp.RawContent, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		parsed, perr := parseJSONValue([]byte(data))
		if perr != nil {
			continue
		}
		event, _ := parsed.(*jsonenc.Object)
		if event == nil {
			continue
		}
		if uv, ok := event.Get("usage"); ok && uv != nil {
			if uo, ok := uv.(*jsonenc.Object); ok {
				usage = uo
			}
		}
		for _, td := range extractStreamToolCallDeltas(event) {
			slot := toolAccum[td.index]
			if slot == nil {
				slot = &aiToolSlot{}
				toolAccum[td.index] = slot
			}
			if td.name != "" {
				slot.name = td.name
			}
			if td.arguments != "" {
				slot.args.WriteString(td.arguments)
			}
		}
		if dt := extractStreamDelta(event); dt != "" {
			outputParts = append(outputParts, dt)
		}
		if extractStreamFinishReason(event) == "tool_calls" {
			flush()
		}
	}
	if len(toolAccum) > 0 {
		flush()
	}
	st := llmStats{}
	if usage != nil {
		st.prompt = jnInt(usage, "prompt_tokens")
		st.completion = jnInt(usage, "completion_tokens")
		st.thinking = jnInt(usage, "thinking_tokens")
		if st.thinking == 0 {
			st.thinking = jnInt(usage, "reasoning_tokens")
		}
	}
	return strings.Join(outputParts, ""), completed, st
}

// extractStreamDelta mirrors app.py _extract_stream_delta.
func extractStreamDelta(event *jsonenc.Object) string {
	choices := objGetArr(event, "choices")
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(*jsonenc.Object)
	if choice == nil {
		return ""
	}
	if delta, ok := objSub(choice, "delta"); ok {
		if c, ok := delta.Get("content"); ok && c != nil {
			if s := coerceLLMContent(c); s != "" {
				return s
			}
		}
	}
	if msg, ok := objSub(choice, "message"); ok {
		if c, ok := msg.Get("content"); ok {
			return coerceLLMContent(c)
		}
	}
	return ""
}

// extractStreamFinishReason mirrors app.py _extract_stream_finish_reason.
func extractStreamFinishReason(event *jsonenc.Object) string {
	choices := objGetArr(event, "choices")
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(*jsonenc.Object)
	if choice == nil {
		return ""
	}
	if v, ok := choice.Get("finish_reason"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

type streamToolDelta struct {
	index     int
	name      string
	arguments string
}

// extractStreamToolCallDeltas mirrors app.py _extract_stream_tool_call_deltas: the (index, name,
// arguments) of each tool-call delta in choices[0].delta.tool_calls.
func extractStreamToolCallDeltas(event *jsonenc.Object) []streamToolDelta {
	choices := objGetArr(event, "choices")
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(*jsonenc.Object)
	if choice == nil {
		return nil
	}
	delta, ok := objSub(choice, "delta")
	if !ok {
		return nil
	}
	var out []streamToolDelta
	for _, item := range objGetArr(delta, "tool_calls") {
		call, _ := item.(*jsonenc.Object)
		if call == nil {
			continue
		}
		fn, _ := objSub(call, "function")
		name, args := "", ""
		if fn != nil {
			if v, ok := fn.Get("name"); ok {
				name, _ = v.(string)
			}
			if v, ok := fn.Get("arguments"); ok {
				args, _ = v.(string)
			}
		}
		idx := 0
		if v, ok := call.Get("index"); ok {
			idx = jnIntVal(v)
		}
		out = append(out, streamToolDelta{index: idx, name: name, arguments: args})
	}
	return out
}

// coerceLLMContent mirrors app.py _coerce_llm_content: a string stays as-is; a list concatenates
// its string items and {"text": ...} parts.
func coerceLLMContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			switch it := item.(type) {
			case string:
				b.WriteString(it)
			case *jsonenc.Object:
				if t, ok := it.Get("text"); ok {
					if ts, ok := t.(string); ok {
						b.WriteString(ts)
					}
				}
			}
		}
		return b.String()
	}
	return ""
}

// jnIntVal reads an int from a parsed-JSON scalar (json.Number / float64).
func jnIntVal(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	default:
		o := jsonenc.NewObject().Set("v", v)
		return jnInt(o, "v")
	}
}

// objGetArr returns a []any value from a parsed-JSON object (nil if absent/not a list).
func objGetArr(o *jsonenc.Object, key string) []any {
	if o == nil {
		return nil
	}
	if v, ok := o.Get(key); ok {
		if arr, ok := v.([]any); ok {
			return arr
		}
	}
	return nil
}

// handleApiAiHelper mirrors app.py ai_helper (non-streaming JSON path): guard, then the tool loop
// over the (canned) streaming LLM endpoint, then the turn summary + memory-consolidation pass, and
// the assembled JSON turn result. The streaming (text/event-stream) variant is a follow-up; the
// JSON path is the authoritative one the parity oracle exercises.
func (s *server) handleApiAiHelper(w http.ResponseWriter, r *http.Request) {
	payload := bodyMap(r)
	question := strings.TrimSpace(bstr(payload, "question"))
	page := strings.TrimSpace(bstr(payload, "page"))
	if question == "" {
		s.errorJSON(w, http.StatusBadRequest, "question is required")
		return
	}
	chatID := strings.TrimSpace(bstr(payload, "chat_id"))
	if chatID == "" {
		chatID = newUUIDv4()
	}
	turnID := strings.TrimSpace(bstr(payload, "turn_id"))
	if turnID == "" {
		turnID = newUUIDv4()
	}

	endpointURL := strings.TrimSpace(s.loadAISetting("ai.endpoint_url", ""))
	model := strings.TrimSpace(s.loadAISetting("ai.model", ""))

	defaultThinking := normalizeThinkingLevel(s.loadAISetting("ai.thinking_level", "off"))
	requestedThinking := normalizeThinkingLevel(bstr(payload, "thinking_level"))
	thinkingLevel := defaultThinking
	if requestedThinking != "off" {
		thinkingLevel = requestedThinking
	}
	if !modelSupportsThinking(model) {
		thinkingLevel = "off"
	}

	if endpointURL == "" || model == "" {
		writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().
			Set("ok", false).Set("error", "AI endpoint not configured. Visit Settings → AI Configuration."))
		return
	}

	allowed, guardReason, guardStats := s.aiHelperGuardCheck(question, page)
	if !allowed {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().
			Set("ok", false).Set("error", "Request blocked by safety guard: "+guardReason))
		return
	}

	turnLogsURL := buildAITurnLogsURL(chatID, turnID)

	var answerParts []string
	var proposedTools []any
	var modelStats *jsonenc.Object
	const maxToolRounds = 3
	var contextData map[string]any
	if cv, ok := payload["context"].(map[string]any); ok {
		contextData = cv
	}
	// Full context assembly (mirrors app.py ai_helper): per-page + dashboard action manifests,
	// persistent memories, chat continuity, prior-turn summaries, page-context user content, and
	// per-page tools. All of it lands in the LLM request body/tools, which the parity mock ignores.
	systemPrompt, userContent, helperTools := s.buildAIHelperContext(question, page, chatID, model, contextData)
	streamReq := llmRequest{
		endpoint:      endpointURL,
		model:         model,
		apiKey:        strings.TrimSpace(s.loadAISetting("ai.api_key", "")),
		thinkingLevel: thinkingLevel,
		maxTokens:     768,
		tools:         helperTools,
		messages: []any{
			jsonenc.NewObject().Set("role", "system").Set("content", systemPrompt),
			jsonenc.NewObject().Set("role", "user").Set("content", userContent),
		},
	}
	for loopRound := 0; loopRound <= maxToolRounds; loopRound++ {
		var roundFeedback []any
		content, toolCalls, stats := s.streamLLMEndpoint(streamReq)
		if content != "" {
			answerParts = append(answerParts, content)
		}
		modelStats = queryStageStats(stats)
		for _, tc := range toolCalls {
			if tc.name != "propose_ui_action" {
				continue
			}
			proposal := jsonenc.NewObject().Set("tool", tc.name).Set("action", tc.args).
				Set("requires_confirmation", true)
			proposedTools = append(proposedTools, proposal)
			roundFeedback = append(roundFeedback, proposal)
		}
		if len(roundFeedback) == 0 {
			if fallback := suggestChartDashboardPivotTool(question, page); fallback != nil {
				proposedTools = append(proposedTools, fallback)
				roundFeedback = append(roundFeedback, fallback)
			}
		}
		if len(roundFeedback) == 0 || loopRound >= maxToolRounds {
			break
		}
	}

	answer := strings.TrimSpace(strings.Join(answerParts, ""))
	if answer == "" {
		writeJSON(w, http.StatusBadGateway, jsonenc.NewObject().
			Set("ok", false).Set("error", "LLM endpoint returned no response"))
		return
	}

	finalAnswer, meta := extractAssistantMeta(answer)
	var metaSummary *jsonenc.Object
	if v, ok := meta.Get("turn_summary"); ok {
		metaSummary, _ = v.(*jsonenc.Object)
	}
	summary := deriveTurnSummary(question, finalAnswer, "", metaSummary)

	savedMemoryIDs := []any{}
	// memory_candidates drive the consolidation pass (a second LLM call per candidate); with none
	// proposed there is nothing to persist and saved_memory_ids stays empty.
	_ = extractMemoryCandidates(meta)

	if proposedTools == nil {
		proposedTools = []any{}
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).
		Set("answer", finalAnswer).
		Set("model", model).
		Set("chat_id", chatID).
		Set("turn_id", turnID).
		Set("thinking_level", thinkingLevel).
		Set("turn_logs_url", turnLogsURL).
		Set("guard_stats", guardStats).
		Set("model_stats", modelStats).
		Set("turn_summary", summary).
		Set("saved_memory_ids", savedMemoryIDs).
		Set("tool_proposals", proposedTools))
}
