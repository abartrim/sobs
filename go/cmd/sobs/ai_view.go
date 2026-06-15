package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// This file holds the helpers the /ai view (handleViewAi) needs, ported 1:1 from app.py.
// They are kept here (not handlers_pages.go) so the page handler edit stays minimal. The
// aiSpanCondition constant itself already lives in handlers_misc.go.

// ---------------------------------------------------------------------------
// SQL fragments (app.py _AI_TRACE_PROMPT_SQL / _AI_TRACE_RESPONSE_SQL).
// ---------------------------------------------------------------------------

const aiTracePromptSQL = "coalesce(SpanAttributes['sobs.gen_ai.prompt'], " +
	"SpanAttributes['gen_ai.turn.summary.request'], " +
	"SpanAttributes['gen_ai.input.question'], " +
	"SpanAttributes['gen_ai.input.messages'])"

const aiTraceResponseSQL = "coalesce(SpanAttributes['sobs.gen_ai.response'], " +
	"SpanAttributes['gen_ai.output.messages'])"

// ---------------------------------------------------------------------------
// Query-param parsing helpers (app.py _parse_limit / _parse_offset / _parse_sort /
// _parse_time_window_args / _time_window_conditions). These take the raw query values so
// callers stay net/http-native.
// ---------------------------------------------------------------------------

// parseLimitStr mirrors app.py _parse_limit(default): clamp to [1, 5000]; the literal default
// (not 1) on a parse failure. Takes the raw string value (vs the *http.Request-based parseLimit
// in query_filters.go).
func parseLimitStr(raw string, def int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return clampLimit(def)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return clampLimit(n)
}

func clampLimit(n int) int {
	if n > 5000 {
		n = 5000
	}
	if n < 1 {
		n = 1
	}
	return n
}

// parseOffsetStr mirrors app.py _parse_offset: max(0, int(offset)); 0 on parse failure. Takes
// the raw string value (vs the *http.Request-based parseOffset in query_filters.go).
func parseOffsetStr(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	if n < 0 {
		return 0
	}
	return n
}

// parseSortStr mirrors app.py _parse_sort(allowed, default_col). Returns (sortBy, sqlCol,
// sortDir). Takes the raw sort_by/sort_dir string values (vs the *http.Request-based parseSort
// in query_filters.go).
func parseSortStr(sortByRaw, sortDirRaw string, allowed map[string]string, defaultCol string) (string, string, string) {
	sortBy := sortByRaw
	if sortByRaw == "" {
		sortBy = defaultCol
	}
	sortDir := strings.ToLower(sortDirRaw)
	if sortDirRaw == "" {
		sortDir = "desc"
	}
	if _, ok := allowed[sortBy]; !ok {
		sortBy = defaultCol
	}
	if sortDir != "asc" && sortDir != "desc" {
		sortDir = "desc"
	}
	return sortBy, allowed[sortBy], sortDir
}

// parseTimeWindowArgsStrings mirrors app.py _parse_time_window_args. Returns (fromTS, toTS,
// errMsg). Takes the raw from/to/window string values (vs the *http.Request-based
// parseTimeWindowArgs in query_filters.go).
func parseTimeWindowArgsStrings(fromRaw, toRaw, windowRaw string) (string, string, string) {
	fromRaw = strings.TrimSpace(fromRaw)
	toRaw = strings.TrimSpace(toRaw)
	windowRaw = strings.TrimSpace(windowRaw)

	const invalidValue = "Invalid time value. Use ISO-8601, e.g. 2026-03-29T12:00:00Z"

	fromTS := ""
	toTS := ""
	if fromRaw != "" {
		fromTS = normalizeCHTimestamp(fromRaw)
	}
	if toRaw != "" {
		toTS = normalizeCHTimestamp(toRaw)
	}
	if fromTS != "" && toTS == "" && windowRaw != "" {
		windowS, err := strconv.Atoi(windowRaw)
		if err != nil {
			return "", "", invalidValue
		}
		if windowS < 1 {
			windowS = 1
		}
		fromDT, ok := parseISOFloor(fromTS)
		if !ok {
			return "", "", invalidValue
		}
		toTS = fromDT.Add(time.Duration(windowS) * time.Second).UTC().Format("2006-01-02 15:04:05.000000")
	}
	if fromTS != "" && toTS != "" {
		fromDT, okF := parseISOFloor(fromTS)
		toDT, okT := parseISOFloor(toTS)
		if !okF || !okT {
			return "", "", invalidValue
		}
		if !toDT.After(fromDT) {
			return "", "", "Invalid time window: to_ts must be later than from_ts"
		}
	}
	return fromTS, toTS, ""
}

// parseISOFloor parses a normalized CH timestamp ("2006-01-02 15:04:05.000000" or ISO) into a
// time.Time for comparison, mirroring datetime.fromisoformat over the normalized strings.
func parseISOFloor(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	parse := strings.Replace(s, "Z", "+00:00", 1)
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, parse); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// timeWindowConditions and whereClause are provided once in query_filters.go.

// ---------------------------------------------------------------------------
// User SQL WHERE normalization (app.py _normalize_ai_sql_where / _validate_user_sql_where /
// _replace_sql_outside_single_quotes).
// ---------------------------------------------------------------------------

var unsafeWherePatterns = regexp.MustCompile(`(?i)\b(insert|update|delete|drop|truncate|alter|create|replace|rename|attach|detach|grant|revoke|system\s+stop|system\s+start|system\s+reload|kill|optimize|exchange)\b`)

// errUnsafeSQLWhere is the user-readable message _validate_user_sql_where raises.
const errUnsafeSQLWhere = "SQL filter contains a disallowed keyword. Only comparison and logical expressions are permitted in filter fields."

// validateUserSQLWhere mirrors app.py _validate_user_sql_where: returns the error message when
// an unsafe pattern is present, "" otherwise.
func validateUserSQLWhere(sqlWhere string) string {
	if unsafeWherePatterns.MatchString(sqlWhere) {
		return errUnsafeSQLWhere
	}
	return ""
}

type sqlReplacement struct {
	re   *regexp.Regexp
	repl string
}

// aiSQLReplacements mirrors the ordered replacement list in app.py _normalize_ai_sql_where.
var aiSQLReplacements = []sqlReplacement{
	{regexp.MustCompile(`(?i)\bLogAttributes\s*\[`), "SpanAttributes["},
	{regexp.MustCompile(`(?i)SpanAttributes\s*\[\s*'prompt'\s*\]`), aiTracePromptSQL},
	{regexp.MustCompile(`(?i)SpanAttributes\s*\[\s*'response'\s*\]`), aiTraceResponseSQL},
	{regexp.MustCompile(`(?i)\bservice\b`), "ServiceName"},
	{regexp.MustCompile(`(?i)\bmodel\b`), "SpanAttributes['gen_ai.request.model']"},
	{regexp.MustCompile(`(?i)\bprovider\b`), "SpanAttributes['gen_ai.provider.name']"},
	{regexp.MustCompile(`(?i)\boperation\b`), "SpanAttributes['gen_ai.operation.name']"},
	{regexp.MustCompile(`(?i)\bprompt\b`), aiTracePromptSQL},
	{regexp.MustCompile(`(?i)\bresponse\b`), aiTraceResponseSQL},
	{regexp.MustCompile(`(?i)\btrace_id\b`), "TraceId"},
	{regexp.MustCompile(`(?i)\bspan_id\b`), "SpanId"},
	{regexp.MustCompile(`(?i)\bspan_name\b`), "SpanName"},
	{regexp.MustCompile(`(?i)\brow_type\b`), "if(SpanAttributes['gen_ai.request.model'] != '', 'llm', 'system')"},
	{regexp.MustCompile(`(?i)\bts\b`), "Timestamp"},
	{regexp.MustCompile(`(?i)\bstatus\b`), "StatusCode"},
	{regexp.MustCompile(`(?i)\berror_type\b`), "SpanAttributes['error.type']"},
	{regexp.MustCompile(`(?i)\btokens_in\b`), "toUInt64OrZero(SpanAttributes['gen_ai.usage.input_tokens'])"},
	{regexp.MustCompile(`(?i)\btokens_out\b`), "toUInt64OrZero(SpanAttributes['gen_ai.usage.output_tokens'])"},
	{regexp.MustCompile(`(?i)\bthinking_tokens\b`), "toUInt64OrZero(SpanAttributes['gen_ai.usage.thinking_tokens'])"},
	{regexp.MustCompile(`(?i)\bduration_ms\b`), "(Duration / 1000000.0)"},
}

// normalizeAiSQLWhere mirrors app.py _normalize_ai_sql_where. Returns (safeSQL, errMsg).
func normalizeAiSQLWhere(sqlWhere string) (string, string) {
	if msg := validateUserSQLWhere(sqlWhere); msg != "" {
		return "", msg
	}
	safe := strings.ReplaceAll(sqlWhere, ";", "")
	return replaceSQLOutsideSingleQuotes(safe, aiSQLReplacements), ""
}

// replaceSQLOutsideSingleQuotes mirrors app.py _replace_sql_outside_single_quotes: mask
// single-quoted literals (with doubled-quote escapes), apply the regex replacements to the
// masked text, then restore the literals.
func replaceSQLOutsideSingleQuotes(sql string, replacements []sqlReplacement) string {
	var placeholders []string
	var masked strings.Builder
	runes := []rune(sql)
	i := 0
	n := len(runes)
	for i < n {
		ch := runes[i]
		if ch != '\'' {
			masked.WriteRune(ch)
			i++
			continue
		}
		start := i
		i++
		for i < n {
			if runes[i] == '\'' {
				if i+1 < n && runes[i+1] == '\'' {
					i += 2
					continue
				}
				i++
				break
			}
			i++
		}
		literal := string(runes[start:i])
		token := "__SQL_LITERAL_" + strconv.Itoa(len(placeholders)) + "__"
		placeholders = append(placeholders, literal)
		masked.WriteString(token)
	}
	out := masked.String()
	for _, r := range replacements {
		out = r.re.ReplaceAllString(out, r.repl)
	}
	for idx, literal := range placeholders {
		out = strings.ReplaceAll(out, "__SQL_LITERAL_"+strconv.Itoa(idx)+"__", literal)
	}
	return out
}

// ---------------------------------------------------------------------------
// errorID (app.py _error_id): md5 of pipe-joined fields.
// ---------------------------------------------------------------------------

func errorID(ts, service, errType, message, traceID, spanID string) string {
	raw := strings.Join([]string{ts, service, errType, message, traceID, spanID}, "|")
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Attribute helpers operating on a parsed SpanAttributes map (mapToDict result). The chdb
// Map(String,String) column arrives as a JSON object of strings, so attrStr mirrors
// str(attrs.get(key, default)).
// ---------------------------------------------------------------------------

func attrMap(v any) map[string]any {
	if m, ok := mapToDict(v).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func attrStr(attrs map[string]any, key string) string {
	if v, ok := attrs[key]; ok {
		return toStr(v)
	}
	return ""
}

// attrStrDef mirrors str(attrs.get(key, default)).
func attrStrDef(attrs map[string]any, key, def string) string {
	if v, ok := attrs[key]; ok {
		return toStr(v)
	}
	return def
}

// safeAttrInt mirrors app.py view_ai._safe_attr_int: float(str(raw or 0)) -> int, 0 on bad/NaN/inf.
func safeAttrInt(attrs map[string]any, key string) int {
	raw, ok := attrs[key]
	if !ok {
		raw = "0"
	}
	s := toStr(raw)
	if strings.TrimSpace(s) == "" {
		s = "0"
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return int(f)
}

// safeDurationMS mirrors app.py view_ai._safe_duration_ms: round(ns/1e6, 1), 0.0 on bad/NaN/inf.
func safeDurationMS(durationNS any) float64 {
	s := toStr(durationNS)
	if strings.TrimSpace(s) == "" {
		s = "0"
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0.0
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0.0
	}
	return roundHalfEven(f/1_000_000, 1)
}

// ---------------------------------------------------------------------------
// GenAI message text extraction / normalization (app.py _genai_* / _extract_messages_text /
// _normalize_genai_messages_for_display / _dedupe_system_input_messages).
// ---------------------------------------------------------------------------

// parseGenaiMessagesJSON mirrors app.py _parse_genai_messages_json: returns the message list,
// or nil (ok=false) on a JSON decode error. ([] is a valid non-nil empty list.)
func parseGenaiMessagesJSON(messagesStr string) ([]any, bool) {
	if messagesStr == "" {
		return []any{}, true
	}
	var parsed any
	if err := json.Unmarshal([]byte(messagesStr), &parsed); err != nil {
		return nil, false
	}
	switch p := parsed.(type) {
	case []any:
		return p, true
	case map[string]any:
		for _, key := range []string{"messages", "input_messages", "output_messages", "items"} {
			if nested, ok := p[key]; ok {
				if list, ok := nested.([]any); ok {
					return list, true
				}
			}
		}
	}
	return []any{}, true
}

// genaiToolCallsToText mirrors app.py _genai_tool_calls_to_text.
func genaiToolCallsToText(toolCallsValue any) string {
	list, ok := toolCallsValue.([]any)
	if !ok {
		return ""
	}
	var chunks []string
	for _, itemAny := range list {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		function, _ := item["function"].(map[string]any)
		if function == nil {
			function = map[string]any{}
		}
		name := strings.TrimSpace(firstNonEmptyStr(item["name"], function["name"]))
		arguments := item["arguments"]
		if isEmptyArg(arguments) {
			arguments = function["arguments"]
		}
		label := "tool_call"
		if name != "" {
			label = "tool_call:" + name
		}
		switch a := arguments.(type) {
		case map[string]any:
			if len(a) > 0 {
				chunks = append(chunks, label+" "+jsonDumpsNoEsc(a))
			} else {
				chunks = append(chunks, label)
			}
		case []any:
			if len(a) > 0 {
				chunks = append(chunks, label+" "+jsonDumpsNoEsc(a))
			} else {
				chunks = append(chunks, label)
			}
		default:
			if !isNoneOrEmptyStr(arguments) {
				chunks = append(chunks, label+" "+toStr(arguments))
			} else {
				chunks = append(chunks, label)
			}
		}
	}
	return strings.TrimSpace(strings.Join(chunks, "\n"))
}

// genaiMessageContentToText mirrors app.py _genai_message_content_to_text.
func genaiMessageContentToText(message map[string]any) string {
	content := message["content"]
	switch c := content.(type) {
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, part := range c {
			if pm, ok := part.(map[string]any); ok {
				parts = append(parts, toStr(pm["text"]))
			} else {
				parts = append(parts, toStr(part))
			}
		}
		return strings.TrimSpace(strings.Join(parts, " "))
	}
	if !isNoneOrEmptyStr(content) {
		return toStr(content)
	}

	if partsValue, ok := message["parts"].([]any); ok {
		var chunks []string
		for _, partAny := range partsValue {
			if ps, ok := partAny.(string); ok {
				if ps != "" {
					chunks = append(chunks, ps)
				}
				continue
			}
			part, ok := partAny.(map[string]any)
			if !ok {
				continue
			}
			partType := strings.ToLower(strings.TrimSpace(toStr(part["type"])))
			switch partType {
			case "text", "reasoning":
				text := firstNonEmptyStr(part["content"], part["text"])
				if text != "" {
					chunks = append(chunks, text)
				}
				continue
			case "tool_call", "server_tool_call":
				rendered := genaiToolCallsToText([]any{part})
				if rendered != "" {
					chunks = append(chunks, rendered)
				}
				continue
			case "tool_call_response", "server_tool_call_response":
				if resp := part["response"]; !isNoneOrEmptyStr(resp) {
					chunks = append(chunks, toStr(resp))
				} else {
					chunks = append(chunks, partType)
				}
				continue
			}
			if pc := part["content"]; !isNoneOrEmptyStr(pc) {
				chunks = append(chunks, toStr(pc))
				continue
			}
			chunks = append(chunks, jsonDumpsNoEsc(part))
		}
		rendered := strings.TrimSpace(strings.Join(chunks, "\n"))
		if rendered != "" {
			return rendered
		}
	}

	if toolCallsText := genaiToolCallsToText(message["tool_calls"]); toolCallsText != "" {
		return toolCallsText
	}

	if fc, ok := message["function_call"].(map[string]any); ok {
		functionText := genaiToolCallsToText([]any{map[string]any{"function": fc}})
		if functionText != "" {
			return functionText
		}
	}

	return ""
}

// genaiMessageReasoningToText mirrors app.py _genai_message_reasoning_to_text.
func genaiMessageReasoningToText(message map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoning", "thinking"} {
		if text := coerceReasoningText(message[key]); text != "" {
			return text
		}
	}
	if partsValue, ok := message["parts"].([]any); ok {
		var reasoningChunks []string
		for _, partAny := range partsValue {
			part, ok := partAny.(map[string]any)
			if !ok {
				continue
			}
			if strings.ToLower(strings.TrimSpace(toStr(part["type"]))) != "reasoning" {
				continue
			}
			text := coerceReasoningText(firstNonEmptyAny(part["content"], part["text"]))
			if text != "" {
				reasoningChunks = append(reasoningChunks, text)
			}
		}
		if len(reasoningChunks) > 0 {
			return strings.TrimSpace(strings.Join(reasoningChunks, "\n"))
		}
	}
	return ""
}

func coerceReasoningText(value any) string {
	if isNoneOrEmptyStr(value) {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var chunks []string
		for _, itemAny := range v {
			switch item := itemAny.(type) {
			case string:
				if text := strings.TrimSpace(item); text != "" {
					chunks = append(chunks, text)
				}
			case map[string]any:
				text := strings.TrimSpace(firstNonEmptyStr(item["text"], item["content"]))
				if text != "" {
					chunks = append(chunks, text)
				}
			default:
				if text := strings.TrimSpace(toStr(itemAny)); text != "" {
					chunks = append(chunks, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(chunks, "\n"))
	case map[string]any:
		direct := strings.TrimSpace(firstNonEmptyStr(v["text"], v["content"]))
		if direct != "" {
			return direct
		}
		return jsonDumpsNoEsc(v)
	}
	return strings.TrimSpace(toStr(value))
}

// extractMessagesText mirrors app.py _extract_messages_text.
func extractMessagesText(messagesStr string) string {
	if messagesStr == "" {
		return ""
	}
	messages, ok := parseGenaiMessagesJSON(messagesStr)
	if !ok {
		return messagesStr
	}
	var parts []string
	for _, msgAny := range messages {
		switch msg := msgAny.(type) {
		case map[string]any:
			role := toStr(msg["role"])
			content := genaiMessageContentToText(msg)
			if content != "" {
				if role != "" {
					parts = append(parts, "["+role+"] "+content)
				} else {
					parts = append(parts, content)
				}
			}
		case string:
			parts = append(parts, msg)
		}
	}
	return strings.Join(parts, "\n")
}

var genaiRoleLabels = map[string]string{
	"system":    "system instruction",
	"user":      "user",
	"assistant": "assistant",
	"tool":      "tool",
}

// normalizeGenaiMessagesForDisplay mirrors app.py _normalize_genai_messages_for_display.
// Returns a slice of ordered objects so template iteration/key order matches Python dicts.
func normalizeGenaiMessagesForDisplay(messages []any) []any {
	normalized := []any{}
	for _, messageAny := range messages {
		switch message := messageAny.(type) {
		case map[string]any:
			msg := orderedFromMap(message)
			role := strings.ToLower(strings.TrimSpace(toStr(message["role"])))
			if role != "" {
				msg.Set("role", role)
				label, ok := genaiRoleLabels[role]
				if !ok {
					label = role
				}
				msg.Set("role_label", label)
			}
			content := genaiMessageContentToText(message)
			reasoning := genaiMessageReasoningToText(message)
			if content != "" {
				msg.Set("content", content)
			}
			if reasoning != "" {
				msg.Set("thinking_content", reasoning)
			}
			if v, ok := msg.Get("content"); !ok || v == nil {
				msg.Set("content", "")
			}
			normalized = append(normalized, msg)
		case string:
			normalized = append(normalized, jsonenc.NewObject().Set("role", "").Set("content", message))
		default:
			normalized = append(normalized, jsonenc.NewObject().Set("role", "").Set("content", jsonDumpsNoEsc(messageAny)))
		}
	}
	return normalized
}

var dedupeWhitespaceRe = regexp.MustCompile(`\s+`)

// normalizeForDedupe mirrors app.py _normalize_for_dedupe.
func normalizeForDedupe(value any) string {
	text := strings.ToLower(strings.TrimSpace(toStr(value)))
	if text == "" {
		return ""
	}
	return dedupeWhitespaceRe.ReplaceAllString(text, " ")
}

// dedupeSystemInputMessages mirrors app.py _dedupe_system_input_messages.
func dedupeSystemInputMessages(inputMessages []any, systemInstructions string) ([]any, int) {
	canonical := normalizeForDedupe(systemInstructions)
	if canonical == "" {
		return inputMessages, 0
	}
	filtered := []any{}
	duplicateCount := 0
	for _, msgAny := range inputMessages {
		if msg, ok := msgAny.(*jsonenc.Object); ok {
			roleVal, _ := msg.Get("role")
			role := strings.ToLower(strings.TrimSpace(toStr(roleVal)))
			if role == "system" {
				contentVal, _ := msg.Get("content")
				content := normalizeForDedupe(contentVal)
				if content != "" && content == canonical {
					duplicateCount++
					continue
				}
			}
		}
		filtered = append(filtered, msgAny)
	}
	return filtered, duplicateCount
}

// ---------------------------------------------------------------------------
// Trace-turn card builder (app.py _build_ai_trace_turn_cards / supporting helpers).
// ---------------------------------------------------------------------------

func stringAttrTruthy(value any) bool {
	switch strings.ToLower(strings.TrimSpace(toStr(value))) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}

// firstMessageContentForRoles mirrors app.py _first_message_content.
func firstMessageContentForRoles(messages []any, roles ...string) string {
	target := map[string]bool{}
	for _, r := range roles {
		target[strings.ToLower(strings.TrimSpace(r))] = true
	}
	for _, msgAny := range messages {
		msg, ok := msgAny.(*jsonenc.Object)
		if !ok {
			continue
		}
		roleVal, _ := msg.Get("role")
		role := strings.ToLower(strings.TrimSpace(toStr(roleVal)))
		if !target[role] {
			continue
		}
		contentVal, _ := msg.Get("content")
		content := strings.TrimSpace(toStr(contentVal))
		if content != "" {
			return content
		}
	}
	return ""
}

// summarizeAiToolAction mirrors app.py _summarize_ai_tool_action.
func summarizeAiToolAction(rawAction string) string {
	text := strings.TrimSpace(rawAction)
	if text == "" {
		return ""
	}
	var parsedAny any
	if err := json.Unmarshal([]byte(text), &parsedAny); err != nil {
		return truncRunes(text, 180)
	}
	parsed, ok := parsedAny.(map[string]any)
	if !ok {
		return truncRunes(text, 180)
	}
	actionType := strings.TrimSpace(toStr(parsed["type"]))
	sqlWhere := strings.TrimSpace(toStr(parsed["sql_where"]))
	targetPage := strings.TrimSpace(toStr(parsed["target_page"]))
	if sqlWhere != "" {
		label := actionType
		if label == "" {
			label = "action"
		}
		return truncRunes(label+": "+sqlWhere, 180)
	}
	if targetPage != "" {
		label := actionType
		if label == "" {
			label = "action"
		}
		return truncRunes(label+" -> "+targetPage, 180)
	}
	return truncRunes(actionType, 180)
}

// aiItem is the in-flight representation of a flat ai_items entry, carrying the typed values the
// turn-card builder reads plus the ordered object the template render consumes.
type aiItem struct {
	obj            *jsonenc.Object
	ts             string
	service        string
	model          string
	provider       string
	operation      string
	chatID         string
	turnID         string
	traceID        string
	eventName      string
	tokensIn       int
	tokensOut      int
	thinkingTokens int
	durationMS     float64
	inputQuestion  string
	prompt         string
	response       string
	inputMessages  []any
	outputMessages []any
	turnSummaryReq string
	turnSummaryAct string
	turnSummaryRes string
	guardAllowed   any
	guardReason    string
	errorType      string
	errorMessage   string
	toolName       string
	toolStatus     string
	toolSummary    string
	toolAction     string
	toolActionID   string
}

// buildAiTraceTurnCards mirrors app.py _build_ai_trace_turn_cards.
func buildAiTraceTurnCards(spans []*aiItem) []any {
	type tool struct {
		name, status, summary, actionID string
	}
	type turn struct {
		turnID         string
		chatID         string
		model          string
		provider       string
		traceID        string
		status         string
		userMessage    string
		assistantMsg   string
		requestSummary string
		actionSummary  string
		resultSummary  string
		guardAllowed   any
		guardReason    string
		tools          []tool
		tokensIn       int
		tokensOut      int
		thinkingTokens int
		durationMS     float64
		startedAt      string
		completedAt    string
		eventNames     []string
	}
	turns := map[string]*turn{}
	order := []string{}

	for _, item := range spans {
		turnID := strings.TrimSpace(item.turnID)
		if turnID == "" {
			continue
		}
		t, ok := turns[turnID]
		if !ok {
			t = &turn{
				turnID:    turnID,
				chatID:    strings.TrimSpace(item.chatID),
				model:     strings.TrimSpace(item.model),
				provider:  strings.TrimSpace(item.provider),
				status:    "in_progress",
				startedAt: item.ts,
				traceID:   strings.TrimSpace(item.traceID),
			}
			turns[turnID] = t
			order = append(order, turnID)
		}

		eventName := strings.TrimSpace(item.eventName)
		if eventName != "" && !containsStr(t.eventNames, eventName) {
			t.eventNames = append(t.eventNames, eventName)
		}

		if t.model == "" {
			t.model = strings.TrimSpace(item.model)
		}
		if t.provider == "" {
			t.provider = strings.TrimSpace(item.provider)
		}
		if t.chatID == "" {
			t.chatID = strings.TrimSpace(item.chatID)
		}
		if t.traceID == "" {
			t.traceID = strings.TrimSpace(item.traceID)
		}

		ts := item.ts
		if ts != "" && (t.startedAt == "" || ts < t.startedAt) {
			t.startedAt = ts
		}
		if ts != "" && (t.completedAt == "" || ts > t.completedAt) {
			t.completedAt = ts
		}

		t.tokensIn += item.tokensIn
		t.tokensOut += item.tokensOut
		t.thinkingTokens += item.thinkingTokens
		t.durationMS = roundHalfEven(t.durationMS+item.durationMS, 1)

		userCandidate := strings.TrimSpace(item.inputQuestion)
		if userCandidate == "" {
			userCandidate = firstMessageContentForRoles(item.inputMessages, "user")
		}
		if userCandidate == "" {
			userCandidate = strings.TrimSpace(item.prompt)
		}
		if userCandidate != "" && t.userMessage == "" {
			t.userMessage = userCandidate
		}

		assistantCandidate := firstMessageContentForRoles(item.outputMessages, "assistant")
		if assistantCandidate == "" {
			assistantCandidate = strings.TrimSpace(item.response)
		}
		if assistantCandidate != "" && (eventName == "turn.complete" || t.assistantMsg == "") {
			t.assistantMsg = assistantCandidate
		}

		if rs := strings.TrimSpace(item.turnSummaryReq); rs != "" && t.requestSummary == "" {
			t.requestSummary = rs
		}
		if as := strings.TrimSpace(item.turnSummaryAct); as != "" && t.actionSummary == "" {
			t.actionSummary = as
		}
		if rs := strings.TrimSpace(item.turnSummaryRes); rs != "" && t.resultSummary == "" {
			t.resultSummary = rs
		}

		switch eventName {
		case "guard.result":
			t.guardAllowed = stringAttrTruthy(item.guardAllowed)
			t.guardReason = strings.TrimSpace(item.guardReason)
		case "turn.blocked":
			t.status = "blocked"
			reason := strings.TrimSpace(item.guardReason)
			if reason == "" {
				reason = strings.TrimSpace(item.errorMessage)
			}
			t.guardReason = reason
		case "turn.error":
			t.status = "failed"
		case "turn.cancelled":
			t.status = "cancelled"
		case "turn.complete":
			if t.status == "in_progress" {
				t.status = "completed"
			}
		}

		if eventName == "tool.proposed" || eventName == "tool.executed" {
			toolName := strings.TrimSpace(item.toolName)
			if toolName == "" {
				toolName = "propose_ui_action"
			}
			toolStatus := strings.TrimSpace(item.toolStatus)
			if toolStatus == "" {
				if eventName == "tool.executed" {
					toolStatus = "executed"
				} else {
					toolStatus = "proposed"
				}
			}
			toolSummary := strings.TrimSpace(item.toolSummary)
			if toolSummary == "" {
				toolSummary = summarizeAiToolAction(strings.TrimSpace(item.toolAction))
			}
			actionID := strings.TrimSpace(item.toolActionID)
			seen := false
			for _, ex := range t.tools {
				if strings.TrimSpace(ex.actionID) == actionID &&
					strings.TrimSpace(ex.name) == toolName &&
					strings.TrimSpace(ex.status) == toolStatus &&
					strings.TrimSpace(ex.summary) == toolSummary {
					seen = true
					break
				}
			}
			if !seen {
				t.tools = append(t.tools, tool{name: toolName, status: toolStatus, summary: toolSummary, actionID: actionID})
			}
		}
	}

	// sorted(turns.values(), key=(started_at, turn_id))
	sortedTurns := make([]*turn, 0, len(order))
	for _, id := range order {
		sortedTurns = append(sortedTurns, turns[id])
	}
	sort.SliceStable(sortedTurns, func(i, j int) bool {
		ai, aj := sortedTurns[i], sortedTurns[j]
		if ai.startedAt != aj.startedAt {
			return ai.startedAt < aj.startedAt
		}
		return ai.turnID < aj.turnID
	})

	out := []any{}
	for index, t := range sortedTurns {
		requestSummary := t.requestSummary
		if strings.TrimSpace(requestSummary) == "" {
			requestSummary = strings.TrimSpace(t.userMessage)
		}
		resultSummary := t.resultSummary
		if strings.TrimSpace(resultSummary) == "" {
			resultSummary = strings.TrimSpace(t.assistantMsg)
		}
		toolsList := []any{}
		for _, tl := range t.tools {
			toolsList = append(toolsList, jsonenc.NewObject().
				Set("name", tl.name).
				Set("status", tl.status).
				Set("summary", tl.summary).
				Set("action_id", tl.actionID))
		}
		eventNames := []any{}
		for _, e := range t.eventNames {
			eventNames = append(eventNames, e)
		}
		obj := jsonenc.NewObject().
			Set("turn_id", t.turnID).
			Set("chat_id", t.chatID).
			Set("model", t.model).
			Set("provider", t.provider).
			Set("status", t.status).
			Set("user_message", t.userMessage).
			Set("assistant_message", t.assistantMsg).
			Set("request_summary", requestSummary).
			Set("action_summary", t.actionSummary).
			Set("result_summary", resultSummary).
			Set("guard_allowed", t.guardAllowed).
			Set("guard_reason", t.guardReason).
			Set("tools", toolsList).
			Set("tool_count", len(t.tools)).
			Set("tokens_in", t.tokensIn).
			Set("tokens_out", t.tokensOut).
			Set("thinking_tokens", t.thinkingTokens).
			Set("duration_ms", t.durationMS).
			Set("started_at", t.startedAt).
			Set("completed_at", t.completedAt).
			Set("event_names", eventNames).
			Set("trace_id", t.traceID).
			Set("index", index+1)
		out = append(out, obj)
	}
	return out
}

// ---------------------------------------------------------------------------
// AI filter metadata (app.py _get_ai_filter_metadata). The per-process cache is intentionally
// NOT ported (it is invisible at the byte level and the parity harness boots fresh).
// ---------------------------------------------------------------------------

// aiFilterMetadata holds the four facet lists plus any error strings.
type aiFilterMetadata struct {
	services   []any
	models     []any
	operations []any
	spanNames  []any
	errors     []string
}

func aiFilterMetadataSampleRows() int {
	return envInt("SOBS_AI_FILTER_METADATA_SAMPLE_ROWS", 10000)
}

// getAiFilterMetadata mirrors app.py _get_ai_filter_metadata.
func (s *server) getAiFilterMetadata(fromTS, toTS string) aiFilterMetadata {
	meta := aiFilterMetadata{
		services:   []any{},
		models:     []any{},
		operations: []any{},
		spanNames:  []any{},
	}

	timeConds, timeParams := timeWindowConditions("Timestamp", fromTS, toTS)
	baseConds := []string{aiSpanCondition}
	baseConds = append(baseConds, timeConds...)
	baseWhere := strings.Join(baseConds, " AND ")
	sourceSQL := "SELECT Timestamp, ServiceName, SpanName, " +
		"SpanAttributes['gen_ai.request.model'] AS RequestModel, " +
		"SpanAttributes['gen_ai.operation.name'] AS OperationName " +
		"FROM otel_traces " +
		"WHERE " + baseWhere + " " +
		"ORDER BY Timestamp DESC LIMIT ?"
	sourceParams := append(append([]any{}, timeParams...), aiFilterMetadataSampleRows())

	fetch := func(selectExpr, extraWhere string) ([]any, error) {
		whereSuffix := ""
		if extraWhere != "" {
			whereSuffix = "WHERE " + extraWhere
		}
		res, err := s.db.Execute(
			"SELECT DISTINCT "+selectExpr+" AS v FROM ("+sourceSQL+") recent_ai "+whereSuffix,
			sourceParams...)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		var values []string
		for _, m := range rowMaps(res) {
			v := cStr(m, "v")
			if strings.TrimSpace(v) == "" {
				continue
			}
			if !seen[v] {
				seen[v] = true
				values = append(values, v)
			}
		}
		sort.Strings(values)
		out := make([]any, len(values))
		for i, v := range values {
			out[i] = v
		}
		return out, nil
	}

	if vals, err := fetch("ServiceName", "ServiceName != ''"); err != nil {
		meta.errors = append(meta.errors, "services="+publicDashboardQueryError(err))
	} else {
		meta.services = vals
	}
	if vals, err := fetch("RequestModel", "RequestModel != ''"); err != nil {
		meta.errors = append(meta.errors, "models="+publicDashboardQueryError(err))
	} else {
		meta.models = vals
	}
	if vals, err := fetch("OperationName", "OperationName != ''"); err != nil {
		meta.errors = append(meta.errors, "operations="+publicDashboardQueryError(err))
	} else {
		meta.operations = vals
	}
	if vals, err := fetch("SpanName", "SpanName != ''"); err != nil {
		meta.errors = append(meta.errors, "span_names="+publicDashboardQueryError(err))
	} else {
		meta.spanNames = vals
	}
	return meta
}

// ---------------------------------------------------------------------------
// Small shared helpers.
// ---------------------------------------------------------------------------

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// firstNonEmptyStr mirrors Python `str(a or b or "")` for the message helpers.
func firstNonEmptyStr(values ...any) string {
	for _, v := range values {
		s := toStr(v)
		if s != "" {
			return s
		}
	}
	return ""
}

func firstNonEmptyAny(values ...any) any {
	for _, v := range values {
		if !isNoneOrEmptyStr(v) {
			return v
		}
	}
	return nil
}

// isNoneOrEmptyStr mirrors Python `value in (None, "")`.
func isNoneOrEmptyStr(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return s == ""
	}
	return false
}

// isEmptyArg mirrors Python `arguments in (None, "", [], {})`.
func isEmptyArg(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// jsonDumpsNoEsc mirrors json.dumps(value, ensure_ascii=False) (compact, spaced separators).
func jsonDumpsNoEsc(v any) string {
	return string(jsonenc.Encode(toEncodable(v), dumpsDefault))
}

// orderedFromMap builds an ordered Object from a parsed JSON object. Go's json.Unmarshal loses
// key order, so keys are emitted sorted; this only affects messages re-serialized to JSON for
// display (a degenerate path), not the parity empty-case render.
func orderedFromMap(m map[string]any) *jsonenc.Object {
	obj := jsonenc.NewObject()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		obj.Set(k, toEncodable(m[k]))
	}
	return obj
}
