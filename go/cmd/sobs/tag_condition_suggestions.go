package main

import (
	"sort"
	"strings"
)

// tag_condition_suggestions.go ports the five suggestion builders app.py dispatches to from
// /api/settings/tags/condition-suggestions (api_tag_rule_condition_suggestions, app.py:23305).
// Each is a positionCaseInsensitive ranked lookup over the otel/tag tables; on the empty fixture
// every one returns [] (the base corpus), so the seeded `tagsuggest` profile exercises the real
// branches. Faithful 1:1 with app.py — same SQL, same q/limit handling, same per-row trimming.

// suggestStrings runs a single-string-column suggestion query and collects the values, skipping
// rows that are blank after trimming. trimValue mirrors the per-builder difference in app.py:
// _tag_rule_value_suggestions._run appends the *stripped* value, while the tag/service builders
// append the raw value (both filter on `if str(row[0] or "").strip()`).
func (s *server) suggestStrings(sql string, trimValue bool, params ...any) []any {
	out := []any{}
	res, err := s.db.Execute(sql, params...)
	if err != nil || len(res.Columns) == 0 {
		return out
	}
	col := res.Columns[0]
	for _, m := range rowMaps(res) {
		raw := cStr(m, col)
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if trimValue {
			out = append(out, strings.TrimSpace(raw))
		} else {
			out = append(out, raw)
		}
	}
	return out
}

// tagRuleAttributeKeySuggestions ports app.py _tag_rule_attribute_key_suggestions (12591): union
// the cached attribute keys across every record type, rank by query match (startswith, then
// contains, then alphabetical), filter to keys containing q (when q is set), and cap at limit.
func (s *server) tagRuleAttributeKeySuggestions(queryText string, limit int) []any {
	keys := logAttrKeyCache.allKeysUnion(s)
	q := strings.ToLower(strings.TrimSpace(queryText))

	ranked := make([]string, 0, len(keys))
	for _, k := range keys {
		if k != "" {
			ranked = append(ranked, k)
		}
	}
	boolRank := func(b bool) int {
		if b {
			return 0
		}
		return 1
	}
	sort.Slice(ranked, func(i, j int) bool {
		li, lj := strings.ToLower(ranked[i]), strings.ToLower(ranked[j])
		si, sj := boolRank(q != "" && strings.HasPrefix(li, q)), boolRank(q != "" && strings.HasPrefix(lj, q))
		if si != sj {
			return si < sj
		}
		ci, cj := boolRank(q != "" && strings.Contains(li, q)), boolRank(q != "" && strings.Contains(lj, q))
		if ci != cj {
			return ci < cj
		}
		return li < lj
	})
	if q != "" {
		filtered := make([]string, 0, len(ranked))
		for _, k := range ranked {
			if strings.Contains(strings.ToLower(k), q) {
				filtered = append(filtered, k)
			}
		}
		ranked = filtered
	}
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]any, len(ranked))
	for i, k := range ranked {
		out[i] = k
	}
	return out
}

// tagRuleValueSuggestions ports app.py _tag_rule_value_suggestions (12610): per-field ranked
// distinct-value lookups over otel_logs/otel_traces/hyperdx_sessions. operator is reserved
// (app.py `del operator`) for future operator-specific ranking and currently unused.
func (s *server) tagRuleValueSuggestions(field, operator, queryText, attrKey string, limit int) []any {
	_ = operator
	fieldName := strings.ToLower(strings.TrimSpace(field))
	q := strings.ToLower(strings.TrimSpace(queryText))

	switch fieldName {
	case "service_name":
		return s.suggestStrings(
			"SELECT value FROM ("+
				"SELECT ServiceName AS value FROM otel_logs WHERE ServiceName != '' "+
				"UNION ALL "+
				"SELECT ServiceName AS value FROM otel_traces WHERE ServiceName != ''"+
				") "+
				"WHERE (? = '' OR positionCaseInsensitive(value, ?) > 0) "+
				"GROUP BY value ORDER BY count() DESC, value LIMIT ?",
			true, q, q, limit)
	case "severity":
		return s.suggestStrings(
			"SELECT SeverityText FROM otel_logs "+
				"WHERE SeverityText != '' AND (? = '' OR positionCaseInsensitive(SeverityText, ?) > 0) "+
				"GROUP BY SeverityText ORDER BY count() DESC, SeverityText LIMIT ?",
			true, q, q, limit)
	case "span_name":
		return s.suggestStrings(
			"SELECT SpanName FROM otel_traces "+
				"WHERE SpanName != '' AND (? = '' OR positionCaseInsensitive(SpanName, ?) > 0) "+
				"GROUP BY SpanName ORDER BY count() DESC, SpanName LIMIT ?",
			true, q, q, limit)
	case "event_type":
		return s.suggestStrings(
			"SELECT value FROM ("+
				"SELECT EventName AS value FROM otel_logs WHERE EventName != '' "+
				"UNION ALL "+
				"SELECT EventName AS value FROM hyperdx_sessions WHERE EventName != ''"+
				") "+
				"WHERE (? = '' OR positionCaseInsensitive(value, ?) > 0) "+
				"GROUP BY value ORDER BY count() DESC, value LIMIT ?",
			true, q, q, limit)
	case "body":
		return s.suggestStrings(
			"SELECT value FROM ("+
				"SELECT Body AS value FROM otel_logs WHERE Body != '' ORDER BY Timestamp DESC LIMIT 4000"+
				") "+
				"WHERE (? = '' OR positionCaseInsensitive(value, ?) > 0) "+
				"GROUP BY value ORDER BY count() DESC, value LIMIT ?",
			true, q, q, limit)
	case "attribute":
		key := strings.TrimSpace(attrKey)
		if key == "" {
			return []any{}
		}
		return s.suggestStrings(
			"SELECT value FROM ("+
				"SELECT LogAttributes[?] AS value FROM otel_logs WHERE LogAttributes[?] != '' "+
				"ORDER BY Timestamp DESC LIMIT 2500 "+
				"UNION ALL "+
				"SELECT SpanAttributes[?] AS value FROM otel_traces WHERE SpanAttributes[?] != '' "+
				"ORDER BY Timestamp DESC LIMIT 2500"+
				") "+
				"WHERE value != '' AND (? = '' OR positionCaseInsensitive(value, ?) > 0) "+
				"GROUP BY value ORDER BY count() DESC, value LIMIT ?",
			true, key, key, key, key, q, q, limit)
	}
	return []any{}
}

// recordTagKeySuggestions ports app.py _record_tag_key_suggestions (12703): ranked distinct tag
// keys from sobs_record_tags FINAL, optionally constrained to a record_type.
func (s *server) recordTagKeySuggestions(queryText string, limit int, recordType string) []any {
	q := strings.ToLower(strings.TrimSpace(queryText))
	rt := strings.ToLower(strings.TrimSpace(recordType))
	if rt == "" {
		rt = "all"
	}
	where := "IsDeleted = 0"
	params := []any{}
	if rt != "all" {
		where += " AND RecordType = ?"
		params = append(params, rt)
	}
	params = append(params, q, q, limit)
	sql := "SELECT TagKey FROM sobs_record_tags FINAL " +
		"WHERE " + where + " " +
		"AND (? = '' OR positionCaseInsensitive(TagKey, ?) > 0) " +
		"GROUP BY TagKey ORDER BY count() DESC, TagKey LIMIT ?"
	return s.suggestStrings(sql, false, params...)
}

// recordTagValueSuggestions ports app.py _record_tag_value_suggestions (12727): ranked distinct
// tag values for a given tag key from sobs_record_tags FINAL, optionally by record_type.
func (s *server) recordTagValueSuggestions(tagKey, queryText string, limit int, recordType string) []any {
	key := strings.TrimSpace(tagKey)
	if key == "" {
		return []any{}
	}
	q := strings.ToLower(strings.TrimSpace(queryText))
	rt := strings.ToLower(strings.TrimSpace(recordType))
	if rt == "" {
		rt = "all"
	}
	where := "IsDeleted = 0 AND TagKey = ?"
	params := []any{key}
	if rt != "all" {
		where += " AND RecordType = ?"
		params = append(params, rt)
	}
	params = append(params, q, q, limit)
	sql := "SELECT TagValue FROM sobs_record_tags FINAL " +
		"WHERE " + where + " " +
		"AND (? = '' OR positionCaseInsensitive(TagValue, ?) > 0) " +
		"GROUP BY TagValue ORDER BY count() DESC, TagValue LIMIT ?"
	return s.suggestStrings(sql, false, params...)
}

// notificationConditionServiceSuggestions ports app.py _notification_condition_service_suggestions
// (12755): ranked distinct service names from v_derived_signals_1m, optionally filtered by signal
// source and signal name.
func (s *server) notificationConditionServiceSuggestions(queryText string, limit int, source, signal string) []any {
	q := strings.ToLower(strings.TrimSpace(queryText))
	src := strings.ToLower(strings.TrimSpace(source))
	sig := strings.TrimSpace(signal)
	sql := "SELECT ServiceName FROM v_derived_signals_1m " +
		"WHERE ServiceName != '' " +
		"AND (? = '' OR SignalSource = ?) " +
		"AND (? = '' OR SignalName = ?) " +
		"AND (? = '' OR positionCaseInsensitive(ServiceName, ?) > 0) " +
		"GROUP BY ServiceName ORDER BY count() DESC, ServiceName LIMIT ?"
	return s.suggestStrings(sql, false, src, src, sig, sig, q, q, limit)
}
