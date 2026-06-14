package main

import (
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

const aiHelperServiceName = "sobs-ai-helper"

var (
	aiAssistantMetaRe = regexp.MustCompile(`(?is)<assistant_meta\b[^>]*>\s*([\s\S]*?)\s*</assistant_meta>`)
	// the &lt;…&gt;-escaped variant.
	aiAssistantMetaEscapedRe = regexp.MustCompile(
		`(?is)&lt;\s*assistant_meta\b(?:[\s\S]*?)&gt;\s*([\s\S]*?)\s*&lt;\s*/assistant_meta\s*&gt;`)
)

// extractAssistantMetaText mirrors the TEXT result of app.py _extract_assistant_meta: strip any
// <assistant_meta> blocks (raw or escaped), cut at any dangling open tag, and trim. (chat_detail
// discards the parsed meta dict, so only the cleaned text is needed.)
func extractAssistantMetaText(text string) string {
	cleaned := aiAssistantMetaRe.ReplaceAllString(text, "")
	cleaned = aiAssistantMetaEscapedRe.ReplaceAllString(cleaned, "")
	lower := strings.ToLower(cleaned)
	cut := -1
	if i := strings.Index(lower, "<assistant_meta"); i >= 0 {
		cut = i
	}
	if i := strings.Index(lower, "&lt;assistant_meta"); i >= 0 && (cut < 0 || i < cut) {
		cut = i
	}
	if cut >= 0 {
		cleaned = cleaned[:cut]
	}
	return strings.TrimSpace(cleaned)
}

// toolStatusLabel mirrors app.py _tool_status_label.
func toolStatusLabel(status string, requiresConfirmation bool) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "executed":
		return "Executed"
	case "unsupported":
		return "Not available in this page action manifest"
	}
	if requiresConfirmation {
		return "Awaiting confirmation"
	}
	return "Queued"
}

// loadChatToolHistory mirrors app.py _load_chat_tool_history: the proposed/executed tool actions
// per turn for a chat. Empty when the chat has no tool events.
func (s *server) loadChatToolHistory(chatID string) map[string][]any {
	res, err := s.db.Execute(
		"SELECT Timestamp, EventName, LogAttributes['gen_ai.turn_id'] AS turn_id, "+
			"LogAttributes['sobs.ai.action_id'] AS action_id, "+
			"LogAttributes['sobs.ai.tool.summary'] AS summary, "+
			"LogAttributes['sobs.ai.tool.action'] AS action_json, "+
			"LogAttributes['sobs.ai.action.status'] AS action_status, "+
			"LogAttributes['sobs.ai.action.requires_confirmation'] AS requires_confirmation "+
			"FROM otel_logs WHERE ServiceName=? AND EventName IN ('tool.proposed', 'tool.executed') "+
			"AND LogAttributes['gen_ai.chat_id']=? ORDER BY Timestamp ASC LIMIT 500",
		aiHelperServiceName, chatID)
	if err != nil {
		return map[string][]any{}
	}
	// grouped[turn_id][action_id] -> entry
	grouped := map[string]map[string]*jsonenc.Object{}
	order := map[string][]string{} // preserve first-seen action order per turn
	confirm := map[string]map[string]bool{}
	for _, m := range rowMaps(res) {
		turnID := strings.TrimSpace(cStr(m, "turn_id"))
		if turnID == "" {
			continue
		}
		actionID := strings.TrimSpace(cStr(m, "action_id"))
		if actionID == "" {
			actionID = "anon-" + cStr(m, "Timestamp")
		}
		if grouped[turnID] == nil {
			grouped[turnID] = map[string]*jsonenc.Object{}
			confirm[turnID] = map[string]bool{}
		}
		entry := grouped[turnID][actionID]
		if entry == nil {
			var actionPayload any = jsonenc.NewObject()
			if raw := strings.TrimSpace(cStr(m, "action_json")); raw != "" {
				if parsed, err := parseJSONValue([]byte(raw)); err == nil {
					if _, ok := parsed.(*jsonenc.Object); ok {
						actionPayload = parsed
					}
				}
			}
			status := strings.ToLower(strings.TrimSpace(cStr(m, "action_status")))
			if status == "" {
				status = "proposed"
			}
			rc := truthyStr(cStr(m, "requires_confirmation"))
			entry = jsonenc.NewObject().
				Set("kind", "tool").Set("turn_id", turnID).Set("action_id", actionID).
				Set("summary", strings.TrimSpace(cStr(m, "summary"))).
				Set("action", actionPayload).Set("status", status).
				Set("requires_confirmation", rc).Set("ts", cStr(m, "Timestamp"))
			grouped[turnID][actionID] = entry
			order[turnID] = append(order[turnID], actionID)
			confirm[turnID][actionID] = rc
		}
		if cStr(m, "EventName") == "tool.executed" {
			entry.Set("status", "executed")
		}
	}
	out := map[string][]any{}
	for turnID, ids := range order {
		items := make([]*jsonenc.Object, 0, len(ids))
		for _, id := range ids {
			items = append(items, grouped[turnID][id])
		}
		sort.SliceStable(items, func(i, j int) bool {
			return objStrOr(items[i], "ts") < objStrOr(items[j], "ts")
		})
		list := make([]any, 0, len(items))
		for _, it := range items {
			statusV, _ := it.Get("status")
			st, _ := statusV.(string)
			it.Set("status_label", toolStatusLabel(st, confirm[turnID][objStrOr(it, "action_id")]))
			list = append(list, it)
		}
		out[turnID] = list
	}
	return out
}

// truthyStr mirrors `str(x).strip().lower() in {"1","true","yes","on"}`.
func truthyStr(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// GET /api/ai/helper/chats/<chat_id> — app.py ai_helper_chat_detail: the turn.complete log rows
// for a chat, serialized as user/assistant messages interleaved with tool actions.
func (s *server) handleApiAiHelperChatDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	chatID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/ai/helper/chats/"))
	if chatID == "" {
		s.writeMaskedJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", "chat_id is required"))
		return
	}
	res, err := s.db.Execute(
		"SELECT Timestamp, LogAttributes['gen_ai.turn_id'] AS turn_id, "+
			"LogAttributes['gen_ai.input.question'] AS input_question, "+
			"LogAttributes['gen_ai.turn.summary.request'] AS request, "+
			"LogAttributes['gen_ai.output.messages'] AS output_messages "+
			"FROM otel_logs WHERE ServiceName=? AND EventName='turn.complete' "+
			"AND LogAttributes['gen_ai.chat_id']=? ORDER BY Timestamp ASC LIMIT 300",
		aiHelperServiceName, chatID)
	if err != nil {
		s.dbError(w, err)
		return
	}
	toolsByTurn := s.loadChatToolHistory(chatID)
	messages := []any{}
	for _, m := range rowMaps(res) {
		ts := cStr(m, "Timestamp")
		turnID := cStr(m, "turn_id")
		requestText := strings.TrimSpace(cStr(m, "input_question"))
		if requestText != "" {
			messages = append(messages, jsonenc.NewObject().
				Set("kind", "message").Set("role", "user").Set("text", requestText).
				Set("ts", ts).Set("turn_id", turnID))
		}
		assistantText := ""
		if raw := cStr(m, "output_messages"); raw != "" {
			if parsed, err := parseJSONValue([]byte(raw)); err == nil {
				if list, ok := parsed.([]any); ok {
					parts := []string{}
					for _, it := range list {
						if o, ok := it.(*jsonenc.Object); ok {
							if content := strings.TrimSpace(objStrOr(o, "content")); content != "" {
								parts = append(parts, content)
							}
						}
					}
					assistantText = strings.TrimSpace(strings.Join(parts, "\n\n"))
				}
			}
		}
		if assistantText != "" {
			assistantText = extractAssistantMetaText(assistantText)
		}
		if assistantText != "" {
			messages = append(messages, jsonenc.NewObject().
				Set("kind", "message").Set("role", "assistant").Set("text", assistantText).
				Set("ts", ts).Set("turn_id", turnID).Set("question", requestText))
		}
		messages = append(messages, toolsByTurn[turnID]...)
	}
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("chat_id", chatID).Set("messages", messages))
}
