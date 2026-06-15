package main

import (
	"regexp"
	"strings"
)

// ingest_tag_rules.go ports app.py's ingest-time auto-tag application: _apply_tag_rules
// (app.py:12777), _match_tag_rule (12506), _match_single_condition (12551) and the record-id
// helpers _record_id_for_log (12429) / _record_id_for_span (12435). Every inserter loads the
// active tag rules (via loadTagRulesCtx, the port of _load_tag_rules) and writes one auto-tag row
// (IsAuto=1) per matching rule into sobs_record_tags, keyed by the record id.
//
// The rule dicts are exactly what loadTagRulesCtx() produces: map[string]any with string fields
// (match_field/match_operator/match_value/match_attr_key/tag_key/tag_value), record_types as
// []any of strings, and conditions as []any of map[string]any.

// recordIDForLog (mirroring _record_id_for_log) is defined in handlers_pages_logs_errors.go and
// reused here.

// recordIDForSpan mirrors _record_id_for_span: md5("{trace_id}|{span_id}").
func recordIDForSpan(traceID, spanID string) string {
	return md5Hex(traceID + "|" + spanID)
}

// matchSingleCondition mirrors _match_single_condition.
func matchSingleCondition(cond map[string]any, service, severity, body string, attrs map[string]any, spanName, eventType string) bool {
	field := mapStr(cond, "match_field")
	var value string
	switch field {
	case "service_name":
		value = service
	case "severity":
		value = severity
	case "body":
		value = body
	case "span_name":
		value = spanName
	case "event_type":
		value = eventType
	case "attribute":
		if attrs != nil {
			value = toStr(attrs[mapStr(cond, "match_attr_key")])
		}
	default:
		value = ""
	}

	operator := mapStr(cond, "match_operator")
	matchValue := mapStr(cond, "match_value")
	switch operator {
	case "eq":
		return value == matchValue
	case "contains":
		return strings.Contains(strings.ToLower(value), strings.ToLower(matchValue))
	case "regex":
		// Python re.search(match_value, value); invalid pattern -> False.
		re, err := regexp.Compile(matchValue)
		if err != nil {
			return false
		}
		return re.MatchString(value)
	}
	return false
}

// matchTagRule mirrors _match_tag_rule: record-type gate, then either all-of composite conditions
// or the single legacy match_field/operator/value triple.
func matchTagRule(rule map[string]any, recordType, service, severity, body string, attrs map[string]any, spanName, eventType string) bool {
	ruleTypes := mapStrList(rule["record_types"])
	if len(ruleTypes) > 0 && !sliceContains(ruleTypes, "all") && !sliceContains(ruleTypes, recordType) {
		return false
	}

	conditions := asMapList(rule["conditions"])
	if len(conditions) > 0 {
		for _, cond := range conditions {
			if !matchSingleCondition(cond, service, severity, body, attrs, spanName, eventType) {
				return false
			}
		}
		return true
	}

	return matchSingleCondition(map[string]any{
		"match_field":    mapStr(rule, "match_field"),
		"match_operator": mapStr(rule, "match_operator"),
		"match_value":    mapStr(rule, "match_value"),
		"match_attr_key": mapStr(rule, "match_attr_key"),
	}, service, severity, body, attrs, spanName, eventType)
}

// applyTagRules mirrors _apply_tag_rules: for each ingested row evaluate every rule and write one
// auto-tag (IsAuto=1) row per matching tag_key (last matching rule wins per key, deterministic by
// rule order). record_type in ("trace","ai") uses the span record id, otherwise the log record id.
// Called from inside the ingest write op; the insert is via insertRowsNormalized like Python.
func (s *server) applyTagRules(recordType string, rowsData []map[string]any, rules []any) {
	if len(rules) == 0 || len(rowsData) == 0 {
		return
	}
	tagRows := []map[string]any{}
	version := fixedVersionMillis()
	for _, row := range rowsData {
		service := cStr(row, "ServiceName")
		severity := cStr(row, "SeverityText")
		body := cStr(row, "Body")
		attrs := rowAttrs(row)
		spanName := cStr(row, "SpanName")
		eventType := cStr(row, "EventName")
		traceID := cStr(row, "TraceId")
		spanID := cStr(row, "SpanId")
		ts := cStr(row, "Timestamp")

		var recordID string
		if recordType == "trace" || recordType == "ai" {
			recordID = recordIDForSpan(traceID, spanID)
		} else {
			recordID = recordIDForLog(ts, service, traceID, spanID)
		}

		// Keep one value per tag key per record; last matching rule wins. Preserve rule order so
		// the surviving value is deterministic (matches Python's dict-overwrite-by-iteration).
		matchedKeys := []string{}
		matchedByKey := map[string]string{}
		for _, ru := range rules {
			rule, ok := ru.(map[string]any)
			if !ok {
				continue
			}
			if matchTagRule(rule, recordType, service, severity, body, attrs, spanName, eventType) {
				tagKey := mapStr(rule, "tag_key")
				if _, seen := matchedByKey[tagKey]; !seen {
					matchedKeys = append(matchedKeys, tagKey)
				}
				matchedByKey[tagKey] = mapStr(rule, "tag_value")
			}
		}
		for _, tagKey := range matchedKeys {
			tagRows = append(tagRows, map[string]any{
				"RecordType": recordType,
				"RecordId":   recordID,
				"TagKey":     tagKey,
				"TagValue":   matchedByKey[tagKey],
				"IsAuto":     1,
				"IsDeleted":  0,
				"Version":    version,
			})
			version++
		}
	}
	if len(tagRows) > 0 {
		_, _ = s.insertRowsNormalized("sobs_record_tags", tagRows)
	}
}

// rowAttrs mirrors `row.get("LogAttributes") or row.get("SpanAttributes") or {}` (must be a map).
func rowAttrs(row map[string]any) map[string]any {
	if m, ok := row["LogAttributes"].(map[string]any); ok && len(m) > 0 {
		return m
	}
	if m, ok := row["SpanAttributes"].(map[string]any); ok && len(m) > 0 {
		return m
	}
	// Python falls back to {} when both are absent/empty/non-dict.
	if m, ok := row["LogAttributes"].(map[string]any); ok {
		return m
	}
	if m, ok := row["SpanAttributes"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// mapStr (string field from a map[string]any) is defined in handlers_pages.go and reused here.

// mapStrList coerces an []any of strings to []string (loadTagRulesCtx record_types form).
func mapStrList(v any) []string {
	out := []string{}
	if list, ok := v.([]any); ok {
		for _, e := range list {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// asMapList coerces an []any of map[string]any to []map[string]any (loadTagRulesCtx conditions).
func asMapList(v any) []map[string]any {
	out := []map[string]any{}
	if list, ok := v.([]any); ok {
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

func sliceContains(ss []string, target string) bool {
	for _, s := range ss {
		if s == target {
			return true
		}
	}
	return false
}
