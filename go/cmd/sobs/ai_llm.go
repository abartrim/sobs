package main

import (
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

// callLLMEndpoint mirrors app.py _call_llm_endpoint: POST the OpenAI-compatible endpoint and
// return choices[0].message.content. Under parity the response is the canned upstream fixture
// (keyed by the URL — each AI route's profile uses a distinct endpoint path).
func (s *server) callLLMEndpoint(endpoint string) (string, error) {
	resp, err := s.upstreamGet("POST", chatCompletionsURL(endpoint))
	if err != nil {
		return "", err
	}
	if resp.Status >= 400 {
		return "", fmt.Errorf("LLM endpoint returned HTTP %d", resp.Status)
	}
	obj, ok := resp.Body.(*jsonenc.Object)
	if !ok {
		return "", nil
	}
	cv, _ := obj.Get("choices")
	choices, _ := cv.([]any)
	if len(choices) == 0 {
		return "", nil
	}
	c0, _ := choices[0].(*jsonenc.Object)
	if c0 == nil {
		return "", nil
	}
	mv, _ := c0.Get("message")
	msg, _ := mv.(*jsonenc.Object)
	if msg == nil {
		return "", nil
	}
	content, _ := msg.Get("content")
	if str, ok := content.(string); ok {
		return str, nil
	}
	return "", nil
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
	content, err := s.callLLMEndpoint(endpoint)
	if err != nil || content == "" {
		return "", "LLM did not return a refined chart spec."
	}
	parsed, perr := parseChartSpecJSON(content)
	if parsed != nil {
		return string(jsonenc.Encode(parsed, dumpsDefault)), ""
	}
	return "", "Refined chart spec JSON parse error: " + perr
}
