package main

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
