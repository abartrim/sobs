package main

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// This file ports app.py's AI-helper context assembly: the per-page action manifest + tools, the
// persistent-memory / chat-continuity / prior-turn-summary blocks, and the full default system
// prompt. All of it feeds the LLM request body + tools, which the parity mock ignores (it keys on
// the URL), so enriching it is parity-safe by construction; the DB loaders are read-only and
// best-effort (errors → empty), so they cannot change the route's response bytes.

const aiMemoryDimensions = 128        // _AI_MEMORY_DIMENSIONS
const aiMemorySemanticMinScore = 0.26 // _AI_MEMORY_SEMANTIC_MIN_SCORE
var embeddingTokenRE = regexp.MustCompile(`[a-z0-9_./:-]+`)

// tokenizeForEmbedding mirrors app.py _tokenize_for_embedding.
func tokenizeForEmbedding(text string) []string {
	return embeddingTokenRE.FindAllString(strings.ToLower(text), -1)
}

// textEmbedding mirrors app.py _text_embedding: a 128-dim hashed bag-of-tokens, L2-normalized. The
// bucket index is the full sha256 integer mod 128 (computed exactly via big.Int).
func textEmbedding(text string) []float64 {
	vec := make([]float64, aiMemoryDimensions)
	tokens := tokenizeForEmbedding(text)
	if len(tokens) == 0 {
		return vec
	}
	mod := big.NewInt(aiMemoryDimensions)
	for _, tok := range tokens {
		sum := sha256.Sum256([]byte(tok))
		idx := new(big.Int).Mod(new(big.Int).SetBytes(sum[:]), mod).Int64()
		vec[idx] += 1.0
	}
	norm := 0.0
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm <= 0 {
		return vec
	}
	for i := range vec {
		vec[i] /= norm
	}
	return vec
}

// cosineSimilarity mirrors app.py _cosine_similarity: a plain dot product over the shared prefix
// (the vectors are already unit-normalized).
func cosineSimilarity(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += a[i] * b[i]
	}
	return sum
}

// embeddingFromJSON mirrors app.py _embedding_from_json.
func embeddingFromJSON(raw string) []float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := parseJSONValue([]byte(raw))
	if err != nil {
		return nil
	}
	arr, ok := parsed.([]any)
	if !ok {
		return nil
	}
	out := make([]float64, 0, len(arr))
	for _, item := range arr {
		switch v := item.(type) {
		case json.Number:
			f, _ := v.Float64()
			out = append(out, f)
		case float64:
			out = append(out, v)
		default:
			out = append(out, 0)
		}
	}
	return out
}

// modelSupportsTools mirrors app.py _model_supports_tools.
func modelSupportsTools(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return false
	}
	for _, token := range []string{"instruct", "tool", "gpt", "qwen", "llama", "mistral"} {
		if strings.Contains(m, token) {
			return true
		}
	}
	return false
}

// aiActionPageTemplates mirrors app.py _AI_ACTION_PAGE_TEMPLATES.
var aiActionPageTemplates = map[string][]string{
	"/":                       {"summary.html"},
	"/summary":                {"summary.html"},
	"/logs":                   {"logs.html"},
	"/traces":                 {"traces.html"},
	"/metrics":                {"metrics.html"},
	"/metrics/anomaly":        {"metrics_anomaly.html"},
	"/metrics/rules":          {"metrics_rules.html"},
	"/errors":                 {"errors.html"},
	"/rum":                    {"rum.html"},
	"/ai":                     {"ai.html"},
	"/dashboards":             {"custom_dashboards.html"},
	"/dashboards/_detail":     {"custom_dashboard_view.html"},
	"/settings":               {"settings.html"},
	"/settings/ai":            {"settings_ai.html"},
	"/settings/agents":        {"settings_agents.html"},
	"/settings/notifications": {"settings_notifications.html"},
	"/settings/tags":          {"settings_tags.html"},
	"/settings/masking":       {"settings_masking.html"},
}

var aiActionTagRE = regexp.MustCompile(`(?i)<[^>]*\bdata-ai-action-id\s*=\s*['"][^'"]+['"][^>]*>`)
var aiActionAttrRE = regexp.MustCompile(`(?s)([A-Za-z_:][A-Za-z0-9_:\-.]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// parseBoolAttr mirrors the inner _parse_bool_attr of _helper_action_manifest_for_page.
func parseBoolAttr(value string, def bool) bool {
	text := strings.ToLower(strings.TrimSpace(value))
	if text == "" {
		return def
	}
	switch text {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// aiActionTagAttrs mirrors the inner _tag_attrs: lowercase attribute name → value, preferring the
// double-quoted capture, else the single-quoted one.
func aiActionTagAttrs(tagHTML string) map[string]string {
	attrs := map[string]string{}
	for _, m := range aiActionAttrRE.FindAllStringSubmatch(tagHTML, -1) {
		name := strings.ToLower(m[1])
		val := m[2]
		if val == "" {
			val = m[3]
		}
		attrs[name] = val
	}
	return attrs
}

// helperActionManifestForPage mirrors app.py _helper_action_manifest_for_page: parse the page's
// template(s) for data-ai-action-id annotations into a sorted action manifest. Missing templates
// are skipped (→ empty), exactly as the Python OSError branch does.
func (s *server) helperActionManifestForPage(page string) []*jsonenc.Object {
	normalized := strings.TrimSpace(page)
	if normalized == "" {
		normalized = "/logs"
	}
	templates := aiActionPageTemplates[normalized]
	if len(templates) == 0 && strings.HasPrefix(normalized, "/dashboards/") {
		templates = aiActionPageTemplates["/dashboards/_detail"]
	}
	if len(templates) == 0 {
		return nil
	}
	actionsByID := map[string]*jsonenc.Object{}
	for _, tmplName := range templates {
		raw, err := os.ReadFile(filepath.Join(s.cfg.TemplateDir, tmplName))
		if err != nil {
			continue
		}
		for _, tagHTML := range aiActionTagRE.FindAllString(string(raw), -1) {
			attrs := aiActionTagAttrs(tagHTML)
			actionID := strings.TrimSpace(attrs["data-ai-action-id"])
			if actionID == "" {
				continue
			}
			actionType := strings.ToLower(strings.TrimSpace(attrs["data-ai-action-type"]))
			if actionType == "" {
				continue
			}
			handler := strings.TrimSpace(attrs["data-ai-handler"])
			risk := strings.ToLower(strings.TrimSpace(attrs["data-ai-risk"]))
			if risk != "low" && risk != "medium" && risk != "high" {
				risk = "medium"
			}
			label := attrs["data-ai-label"]
			if label == "" {
				label = actionID
			}
			var arguments *jsonenc.Object
			if argStr := strings.TrimSpace(attrs["data-ai-args"]); argStr != "" {
				if parsed, err := parseJSONValue([]byte(argStr)); err == nil {
					arguments, _ = parsed.(*jsonenc.Object)
				}
			}
			if arguments == nil {
				arguments = jsonenc.NewObject()
			}
			actionsByID[actionID] = jsonenc.NewObject().
				Set("action_id", actionID).
				Set("action_type", actionType).
				Set("label", label).
				Set("risk", risk).
				Set("requires_confirmation", parseBoolAttr(attrs["data-ai-confirm"], true)).
				Set("implemented", handler != "").
				Set("handler", handler).
				Set("arguments", arguments).
				Set("role", attrs["data-ai-action-role"])
		}
	}
	ids := make([]string, 0, len(actionsByID))
	for id := range actionsByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	manifest := make([]*jsonenc.Object, 0, len(ids))
	for _, id := range ids {
		manifest = append(manifest, actionsByID[id])
	}
	return manifest
}

// helperToolsForPage mirrors app.py _helper_tools_for_page: the generic propose_ui_action tool
// when the page has at least one implemented action.
func (s *server) helperToolsForPage(page string) []any {
	manifest := s.helperActionManifestForPage(page)
	if len(manifest) == 0 {
		return nil
	}
	anyImplemented := false
	for _, action := range manifest {
		if v, ok := action.Get("implemented"); ok {
			if b, ok := v.(bool); ok && b {
				anyImplemented = true
				break
			}
		}
	}
	if !anyImplemented {
		return nil
	}
	return []any{aiHelperGenericUIActionTool()}
}

// aiHelperGenericUIActionTool mirrors app.py _AI_HELPER_GENERIC_UI_ACTION_TOOL.
func aiHelperGenericUIActionTool() *jsonenc.Object {
	props := jsonenc.NewObject().
		Set("action_id", jsonenc.NewObject().Set("type", "string").
			Set("description", "Stable action identifier from the page action manifest.")).
		Set("target_page", jsonenc.NewObject().Set("type", "string").
			Set("description", "Optional target page path. Defaults to current page.")).
		Set("arguments", jsonenc.NewObject().Set("type", "object").
			Set("description", "Action arguments for the selected action_id.")).
		Set("notes", jsonenc.NewObject().Set("type", "string").
			Set("description", "Short plain-language summary of the intended action."))
	parameters := jsonenc.NewObject().
		Set("type", "object").
		Set("properties", props).
		Set("required", []any{"action_id"}).
		Set("additionalProperties", false)
	return jsonenc.NewObject().
		Set("type", "function").
		Set("function", jsonenc.NewObject().
			Set("name", "propose_ui_action").
			Set("description", "Propose a UI action using a server-approved action_id and validated arguments. "+
				"Use only action_ids listed as available for this page.").
			Set("parameters", parameters))
}

// manifestJSON renders an action manifest the way app.py does (json.dumps default separators,
// ensure_ascii=False) for splicing into the system prompt.
func manifestJSON(manifest []*jsonenc.Object) string {
	arr := make([]any, len(manifest))
	for i, m := range manifest {
		arr[i] = m
	}
	return string(jsonenc.Encode(arr, dumpsDefault))
}

type chatMemory struct {
	id           string
	text         string
	embedding    []float64
	sourceTurnID string
}

type memoryMatch struct {
	id           string
	text         string
	score        float64
	sourceTurnID string
}

type turnSummary struct {
	turnID  string
	request string
	action  string
	result  string
}

// loadChatMemories mirrors app.py _load_chat_memories (best-effort read).
func (s *server) loadChatMemories(chatID string) []chatMemory {
	res, err := s.db.Execute(
		"SELECT Id, MemoryText, EmbeddingJson, SourceTurnId, UpdatedAt "+
			"FROM sobs_ai_memories FINAL WHERE ChatId=? AND IsDeleted=0 ORDER BY UpdatedAt DESC LIMIT 200",
		chatID)
	if err != nil {
		return nil
	}
	var out []chatMemory
	for _, m := range rowMaps(res) {
		out = append(out, chatMemory{
			id:           cStr(m, "Id"),
			text:         strings.TrimSpace(cStr(m, "MemoryText")),
			embedding:    embeddingFromJSON(cStr(m, "EmbeddingJson")),
			sourceTurnID: cStr(m, "SourceTurnId"),
		})
	}
	return out
}

// semanticMemoryMatches mirrors app.py _semantic_memory_matches.
func semanticMemoryMatches(memories []chatMemory, queryText string, maxResults int) []memoryMatch {
	queryEmb := textEmbedding(queryText)
	var scored []memoryMatch
	for _, item := range memories {
		emb := item.embedding
		if len(emb) == 0 {
			emb = textEmbedding(item.text)
		}
		score := cosineSimilarity(queryEmb, emb)
		if score < aiMemorySemanticMinScore {
			continue
		}
		// _semantic_memory_matches stores round(score, 4) and sorts on that rounded value, so two
		// scores differing only past 4 decimals keep stable (DB) order.
		scored = append(scored, memoryMatch{id: item.id, text: item.text, score: roundHalfEven(score, 4), sourceTurnID: item.sourceTurnID})
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	if len(scored) > maxResults {
		scored = scored[:maxResults]
	}
	return scored
}

// loadRecentChatTurns mirrors app.py _load_recent_chat_turns (most-recent turn summaries).
func (s *server) loadRecentChatTurns(chatID string, limit int) []turnSummary {
	if strings.TrimSpace(chatID) == "" {
		return nil
	}
	if limit < 1 {
		limit = 1
	}
	res, err := s.db.Execute(
		"SELECT Timestamp, LogAttributes['gen_ai.turn.summary.request'] AS request, "+
			"LogAttributes['gen_ai.turn.summary.action'] AS action, "+
			"LogAttributes['gen_ai.turn.summary.result'] AS result, "+
			"LogAttributes['gen_ai.turn_id'] AS turn_id "+
			"FROM otel_logs "+
			"WHERE ServiceName=? AND EventName='turn.summary' AND LogAttributes['gen_ai.chat_id']=? "+
			"ORDER BY Timestamp DESC LIMIT ?",
		aiHelperServiceName, chatID, limit)
	if err != nil {
		return nil
	}
	var out []turnSummary
	for _, m := range rowMaps(res) {
		req := strings.TrimSpace(cStr(m, "request"))
		act := strings.TrimSpace(cStr(m, "action"))
		result := strings.TrimSpace(cStr(m, "result"))
		if req == "" && act == "" && result == "" {
			continue
		}
		out = append(out, turnSummary{turnID: cStr(m, "turn_id"), request: req, action: act, result: result})
	}
	return out
}

// loadRecentTurnSummaries mirrors app.py _load_recent_turn_summaries: pull turn.summary events and
// rank by semantic similarity to the query (min 0.2), keeping the top `limit`.
func (s *server) loadRecentTurnSummaries(chatID, query string, limit int) []turnSummary {
	res, err := s.db.Execute(
		"SELECT Timestamp, LogAttributes['gen_ai.turn.summary.request'] AS request, "+
			"LogAttributes['gen_ai.turn.summary.action'] AS action, "+
			"LogAttributes['gen_ai.turn.summary.result'] AS result, "+
			"LogAttributes['gen_ai.turn_id'] AS turn_id "+
			"FROM otel_logs WHERE ServiceName=? AND EventName='turn.summary' AND LogAttributes['gen_ai.chat_id']=? "+
			"ORDER BY Timestamp DESC LIMIT 100",
		aiHelperServiceName, chatID)
	if err != nil {
		return nil
	}
	queryEmb := textEmbedding(query)
	type scoredTurn struct {
		t     turnSummary
		score float64
	}
	var scored []scoredTurn
	for _, m := range rowMaps(res) {
		req := strings.TrimSpace(cStr(m, "request"))
		act := strings.TrimSpace(cStr(m, "action"))
		result := strings.TrimSpace(cStr(m, "result"))
		if req == "" && result == "" {
			continue
		}
		candidate := strings.TrimSpace(req + " " + act + " " + result)
		score := cosineSimilarity(queryEmb, textEmbedding(candidate))
		if score < 0.2 {
			continue
		}
		scored = append(scored, scoredTurn{
			t: turnSummary{
				turnID:  cStr(m, "turn_id"),
				request: coerceSummaryValue(req, 180),
				action:  coerceSummaryValue(act, 180),
				result:  coerceSummaryValue(result, 220),
			},
			score: score,
		})
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	if len(scored) > limit {
		scored = scored[:limit]
	}
	out := make([]turnSummary, 0, len(scored))
	for _, st := range scored {
		out = append(out, st.t)
	}
	return out
}

// buildAIHelperContext assembles the (system_prompt, user_content, tools) triple exactly as the
// app.py ai_helper route does: the override-or-default system prompt with the per-page + dashboard
// action manifests spliced in, followed by the persistent-memory / chat-continuity / prior-summary
// blocks; the page-context user content; and the per-page tools when the model supports them.
func (s *server) buildAIHelperContext(question, page, chatID, model string, contextData map[string]any) (string, string, []any) {
	actionManifestJSON := manifestJSON(s.helperActionManifestForPage(page))
	dashboardManifestJSON := manifestJSON(s.helperActionManifestForPage("/dashboards"))

	relevant := semanticMemoryMatches(s.loadChatMemories(chatID), question, 5)
	recentTurns := s.loadRecentChatTurns(chatID, 8)
	recentHistory := s.loadRecentTurnSummaries(chatID, question, 4)

	var memoryLines []string
	for _, item := range relevant {
		if t := strings.TrimSpace(item.text); t != "" {
			memoryLines = append(memoryLines, "- "+t)
		}
	}
	var continuityLines []string
	for _, item := range recentTurns {
		continuityLines = append(continuityLines, "- request="+item.request+"; action="+item.action+"; result="+item.result)
	}
	var historyLines []string
	for _, item := range recentHistory {
		historyLines = append(historyLines, "- request="+item.request+"; action="+item.action+"; result="+item.result)
	}

	systemPrompt := strings.TrimSpace(s.loadAISetting("ai.system_prompt", ""))
	if systemPrompt == "" {
		systemPrompt = aiHelperDefaultSystemPrompt +
			"Page action manifest: " + actionManifestJSON +
			"\nCross-page dashboard actions (/dashboards): " + dashboardManifestJSON
	}
	if block := strings.Join(memoryLines, "\n"); block != "" {
		systemPrompt += "\n\nRelevant persistent memories:\n" + block
	}
	if block := strings.Join(continuityLines, "\n"); block != "" {
		systemPrompt += "\n\nCurrent chat continuity (recent turns):\n" + block
	}
	if block := strings.Join(historyLines, "\n"); block != "" {
		systemPrompt += "\n\nSemantically relevant prior turn summaries:\n" + block
	}

	var tools []any
	if modelSupportsTools(model) {
		tools = s.helperToolsForPage(page)
	}
	return systemPrompt, aiHelperUserContent(question, page, contextData), tools
}

// aiHelperUserContent mirrors app.py's user_content assembly: "Current page: <page>" plus each
// truthy context_data key:value pair, then the question. Keys are sorted for deterministic output
// (the JSON body the request decodes from is an unordered map).
func aiHelperUserContent(question, page string, contextData map[string]any) string {
	var lines []string
	if page != "" {
		lines = append(lines, "Current page: "+page)
	}
	if len(contextData) > 0 {
		keys := make([]string, 0, len(contextData))
		for k := range contextData {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := contextData[k]
			if isTruthyVal(v, true) {
				lines = append(lines, k+": "+pyStrAny(v))
			}
		}
	}
	contextStr := strings.Join(lines, "\n")
	if contextStr != "" {
		return contextStr + "\n\nQuestion: " + question
	}
	return question
}
