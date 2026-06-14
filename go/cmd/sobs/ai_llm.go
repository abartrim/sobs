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

// callLLMEndpoint mirrors app.py _call_llm_endpoint: POST the OpenAI-compatible endpoint and
// return choices[0].message.content + usage stats. Under parity the response is the canned
// upstream fixture (keyed by the URL — each AI route's profile uses a distinct endpoint path).
func (s *server) callLLMEndpoint(endpoint string) (string, llmStats, error) {
	resp, err := s.upstreamGet("POST", chatCompletionsURL(endpoint))
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
	reply, st, err := s.callLLMEndpoint(guardURL)
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

// generateSQLViaLLM mirrors _vanna_generate_sql: the LLM-generated SQL (fences stripped).
func (s *server) generateSQLViaLLM(endpoint string) (string, string, llmStats) {
	raw, st, err := s.callLLMEndpoint(endpoint)
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

// vannaRefineChartSpec mirrors app.py _vanna_refine_chart_spec: validate the current spec, ask
// the LLM (canned), and return the re-serialized refined spec.
func (s *server) vannaRefineChartSpec(endpoint, model, currentSpec string) (string, string) {
	if endpoint == "" || model == "" {
		return "", "AI endpoint not configured."
	}
	if _, err := parseJSONValue([]byte(currentSpec)); err != nil {
		return "", "Current chart spec is invalid JSON: " + err.Error()
	}
	content, _, err := s.callLLMEndpoint(endpoint)
	if err != nil || content == "" {
		return "", "LLM did not return a refined chart spec."
	}
	parsed, perr := parseChartSpecJSON(content)
	if parsed != nil {
		return string(jsonenc.Encode(parsed, dumpsDefault)), ""
	}
	return "", "Refined chart spec JSON parse error: " + perr
}
