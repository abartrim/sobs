package main

import (
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---------------------------------------------------------------------------
// Error-item helpers unique to the errors view. The shared error-item family
// (_build_error_item / _extract_structured_error_summary / _try_pretty_json_text /
// _compact_text / _load_work_item_links_for_ref_ids and the JSON-scalar helpers)
// is provided once in handlers_incident.go; this file keeps only the funcs that
// are not also defined there: the narrow-error stub builder and the resolved-id set.
// ---------------------------------------------------------------------------

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

// loadWorkItemLinksForRefIDsObject mirrors _load_work_item_links_for_ref_ids(db, ref_ids) but
// returns an ordered *jsonenc.Object. The map[string]any form used by the page handlers lives in
// handlers_incident.go as loadWorkItemLinksForRefIDs; this ordered variant is retained for any
// caller that needs deterministic key order. (Currently unreferenced — kept for parity fidelity.)
func (s *server) loadWorkItemLinksForRefIDsObject(refIDs []string) *jsonenc.Object {
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
