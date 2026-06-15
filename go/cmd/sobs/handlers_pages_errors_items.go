package main

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---------------------------------------------------------------------------
// Error-item construction (ports of _build_error_item / _extract_structured_error_summary /
// _try_pretty_json_text / _compact_text and _load_work_item_links_for_ref_ids).
// Error items are map[string]any so the template's `err.count is defined` test stays false for
// non-grouped items (matching Python, which omits count/first_seen/last_seen there).
// ---------------------------------------------------------------------------

var reCompactSpace = regexp.MustCompile(`\s+`)

// compactText mirrors _compact_text(value, limit=220).
func compactText(value string, limit int) string {
	text := strings.TrimSpace(reCompactSpace.ReplaceAllString(value, " "))
	if len([]rune(text)) <= limit {
		return text
	}
	runes := []rune(text)
	cut := limit - 1
	if cut < 0 {
		cut = 0
	}
	return strings.TrimRight(string(runes[:cut]), " \t\n\r\f\v") + "..."
}

// tryPrettyJSONText mirrors _try_pretty_json_text(raw): (is_json, pretty). pretty is
// json.dumps(parsed, ensure_ascii=False, indent=2) preserving key order.
func tryPrettyJSONText(rawValue string) (bool, string) {
	raw := strings.TrimSpace(rawValue)
	if raw == "" {
		return false, ""
	}
	first := raw[:1]
	if first != "{" && first != "[" {
		return false, ""
	}
	parsed, err := parseJSONValue([]byte(raw))
	if err != nil {
		return false, ""
	}
	pretty, err := jsonDumpsIndent2NoEsc(parsed)
	if err != nil {
		return false, ""
	}
	return true, pretty
}

// errSummaryTextKeys/codeKeys/typeKeys mirror the keysets in _extract_structured_error_summary.
var (
	errSummaryTextKeys = map[string]bool{
		"message": true, "error": true, "error_message": true, "errormessage": true,
		"detail": true, "description": true, "reason": true, "body": true, "msg": true,
	}
	errSummaryCodeKeys = map[string]bool{
		"code": true, "status": true, "status_code": true, "error_code": true, "errorcode": true,
	}
	errSummaryTypeKeys = map[string]bool{
		"type": true, "error_type": true, "exception": true, "name": true,
	}
)

// firstScalar mirrors the nested _first_scalar(value, keyset, depth) helper.
func firstScalar(value any, keyset map[string]bool, depth int) string {
	if depth > 5 {
		return ""
	}
	switch v := value.(type) {
	case *jsonenc.Object:
		// Prefer direct matches before descending.
		for _, k := range v.Keys() {
			inner, _ := v.Get(k)
			if keyset[strings.ToLower(k)] && isJSONScalar(inner) {
				return strings.TrimSpace(jsonScalarToStr(inner))
			}
		}
		for _, k := range v.Keys() {
			inner, _ := v.Get(k)
			if found := firstScalar(inner, keyset, depth+1); found != "" {
				return found
			}
		}
		return ""
	case []any:
		for _, inner := range v {
			if found := firstScalar(inner, keyset, depth+1); found != "" {
				return found
			}
		}
		return ""
	default:
		if isJSONScalar(value) {
			return strings.TrimSpace(jsonScalarToStr(value))
		}
		return ""
	}
}

// isJSONScalar matches Python's isinstance(value, (str, int, float, bool)). parseJSONValue
// yields string / json.Number / bool for scalars (arrays and *jsonenc.Object are non-scalar).
func isJSONScalar(v any) bool {
	switch v.(type) {
	case string, bool, json.Number:
		return true
	default:
		return false
	}
}

// jsonScalarToStr mirrors Python str(value) for a parsed JSON scalar (bool→True/False;
// json.Number renders its literal text).
func jsonScalarToStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		return x.String()
	case nil:
		return ""
	default:
		return strings.Trim(string(jsonenc.Encode(v, jsonenc.Options{EnsureASCII: false})), `"`)
	}
}

// summaryToString mirrors the nested _to_summary(parsed) helper.
func summaryToString(parsed any) string {
	if arr, ok := parsed.([]any); ok {
		if len(arr) > 0 {
			parsed = arr[0]
		} else {
			parsed = jsonenc.NewObject()
		}
	}
	obj, ok := parsed.(*jsonenc.Object)
	if !ok {
		return ""
	}
	messageText := firstScalar(obj, errSummaryTextKeys, 0)
	codeText := firstScalar(obj, errSummaryCodeKeys, 0)
	typeText := firstScalar(obj, errSummaryTypeKeys, 0)

	if messageText != "" {
		summary := messageText
		var extras []string
		if typeText != "" && !strings.Contains(strings.ToLower(summary), strings.ToLower(typeText)) {
			extras = append(extras, typeText)
		}
		if codeText != "" && !strings.Contains(strings.ToLower(summary), strings.ToLower(codeText)) {
			extras = append(extras, "code "+codeText)
		}
		if len(extras) > 0 {
			summary = summary + " [" + strings.Join(extras, ", ") + "]"
		}
		return compactText(summary, 220)
	}
	if typeText != "" && codeText != "" {
		return compactText(typeText+" (code "+codeText+")", 220)
	}
	if typeText != "" {
		return compactText(typeText, 220)
	}
	if codeText != "" {
		return compactText("code "+codeText, 220)
	}
	return ""
}

// extractStructuredErrorSummary mirrors _extract_structured_error_summary: (summary, from_json).
func extractStructuredErrorSummary(message, rawBody string) (string, bool) {
	for _, candidate := range []string{message, rawBody} {
		raw := strings.TrimSpace(candidate)
		if raw == "" {
			continue
		}
		first := raw[:1]
		if first != "{" && first != "[" {
			continue
		}
		parsed, err := parseJSONValue([]byte(raw))
		if err != nil {
			continue
		}
		summary := summaryToString(parsed)
		if summary != "" {
			return summary, true
		}
		// json.dumps(parsed, ensure_ascii=False) with default separators.
		dumped := string(jsonenc.Encode(parsed, jsonenc.Options{EnsureASCII: false, ItemSep: ", ", KeySep: ": "}))
		return compactText(dumped, 220), true
	}
	basis := message
	if basis == "" {
		basis = rawBody
	}
	return compactText(basis, 220), false
}

// buildErrorItem mirrors _build_error_item(row): row is a rowMaps entry (LogAttributes as a
// JSON object/string). Returns the full error item dict.
func (s *server) buildErrorItem(row map[string]any) map[string]any {
	attrs := mapToDictStr(row["LogAttributes"])
	ts := cStr(row, "Timestamp")
	service := cStr(row, "ServiceName")
	errType := attrStr(attrs, "exception.type", "Error")
	body := cStr(row, "Body")
	message := attrStr(attrs, "exception.message", body)
	rawBody := body
	messageSummary, summaryFromJSON := extractStructuredErrorSummary(message, rawBody)
	messageIsJSON, messagePretty := tryPrettyJSONText(message)
	bodyIsJSON, bodyPretty := tryPrettyJSONText(rawBody)
	stack := s.srcMap.demangleStack(attrStr(attrs, "exception.stacktrace", ""))
	stackIsJSON, stackPretty := tryPrettyJSONText(stack)
	traceID := cStr(row, "TraceId")
	spanID := cStr(row, "SpanId")
	eid := errorIDFor(ts, service, errType, message, traceID, spanID)
	return map[string]any{
		"id":                   eid,
		"ts":                   ts,
		"service":              service,
		"err_type":             errType,
		"message":              message,
		"message_summary":      messageSummary,
		"summary_from_json":    summaryFromJSON,
		"message_is_json":      messageIsJSON,
		"message_pretty_json":  messagePretty,
		"raw_body":             rawBody,
		"raw_body_is_json":     bodyIsJSON,
		"raw_body_pretty_json": bodyPretty,
		"stack":                stack,
		"stack_is_json":        stackIsJSON,
		"stack_pretty_json":    stackPretty,
		"trace_id":             traceID,
		"span_id":              spanID,
		"url":                  attrs["url.full"],
		"error_source":         attrs["error.source"],
		"page_title":           attrs["browser.page.title"],
		"viewport":             attrs["browser.viewport"],
		"artifact_type":        attrs["artifact.type"],
		"artifact_id":          attrs["artifact.id"],
		"artifact_url":         attrs["artifact.url"],
		"replay_id":            attrs["replay.id"],
		"replay_url":           attrs["replay.url"],
	}
}

// buildErrorStubFromNarrow mirrors the nested _build_error_stub_from_narrow(row, resolved).
func buildErrorStubFromNarrow(row map[string]any, resolved bool) map[string]any {
	ts := cStr(row, "Timestamp")
	serviceName := cStr(row, "ServiceName")
	traceID := cStr(row, "TraceId")
	spanID := cStr(row, "SpanId")
	errType := cStr(row, "ErrorType")
	if errType == "" {
		errType = "Error"
	}
	message := cStr(row, "ErrorMessage")
	rawBody := cStr(row, "Body")
	messageSummary, summaryFromJSON := extractStructuredErrorSummary(message, rawBody)
	itemID := cStr(row, "ErrorId")
	if itemID == "" {
		itemID = errorIDFor(ts, serviceName, errType, message, traceID, spanID)
	}
	return map[string]any{
		"id":                   itemID,
		"ts":                   ts,
		"service":              serviceName,
		"err_type":             errType,
		"message":              message,
		"message_summary":      messageSummary,
		"summary_from_json":    summaryFromJSON,
		"message_is_json":      false,
		"message_pretty_json":  "",
		"raw_body":             rawBody,
		"raw_body_is_json":     false,
		"raw_body_pretty_json": "",
		"stack":                "",
		"stack_is_json":        false,
		"stack_pretty_json":    "",
		"trace_id":             traceID,
		"span_id":              spanID,
		"url":                  "",
		"error_source":         "",
		"page_title":           "",
		"viewport":             "",
		"artifact_type":        "",
		"artifact_id":          "",
		"artifact_url":         "",
		"replay_id":            "",
		"replay_url":           "",
		"resolved":             resolved,
	}
}

// attrStr mirrors `str(attrs.get(key, default))` for a string→string attr map.
func attrStr(attrs map[string]string, key, def string) string {
	if v, ok := attrs[key]; ok {
		return v
	}
	return def
}

// getResolvedErrorIDs mirrors _get_resolved_error_ids(db).
func (s *server) getResolvedErrorIDs() map[string]bool {
	out := map[string]bool{}
	res, err := s.db.Execute("SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId")
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		out[cStr(m, "ErrorId")] = true
	}
	return out
}

// loadWorkItemLinksForRefIDs mirrors _load_work_item_links_for_ref_ids(db, ref_ids). Returns an
// ordered object {ref_id: {issue_url, issue_number, issue_state}} so template .get() works.
func (s *server) loadWorkItemLinksForRefIDs(refIDs []string) *jsonenc.Object {
	result := jsonenc.NewObject()
	refSet := map[string]bool{}
	var uniqueRefs []string
	for _, r := range refIDs {
		if r != "" && !refSet[r] {
			refSet[r] = true
			uniqueRefs = append(uniqueRefs, r)
		}
	}
	if len(uniqueRefs) == 0 {
		return result
	}
	placeholders := make([]string, len(uniqueRefs))
	params := make([]any, len(uniqueRefs))
	for i, r := range uniqueRefs {
		placeholders[i] = "?"
		params[i] = r
	}
	query := "SELECT AnomalyRuleId, IssueUrl, CanonicalIssueUrl, IssueNumber, IssueState " +
		"FROM sobs_github_work_items FINAL " +
		"WHERE IsDeleted=0 AND IssueUrl != '' AND AnomalyRuleId IN (" + strings.Join(placeholders, ", ") + ") " +
		"ORDER BY CreatedAt DESC"
	res, err := s.db.Execute(query, params...)
	if err != nil {
		return result
	}
	for _, m := range rowMaps(res) {
		ref := cStr(m, "AnomalyRuleId")
		if !refSet[ref] {
			continue
		}
		if _, present := result.Get(ref); present {
			continue
		}
		issueURL := cStr(m, "IssueUrl")
		if issueURL == "" {
			issueURL = cStr(m, "CanonicalIssueUrl")
		}
		result.Set(ref, jsonenc.NewObject().
			Set("issue_url", issueURL).
			Set("issue_number", cInt(m, "IssueNumber")).
			Set("issue_state", cStr(m, "IssueState")))
	}
	return result
}
