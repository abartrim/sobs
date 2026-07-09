package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Path-param GET routes. Go's ServeMux matches by prefix; the handler parses the trailing
// path segment(s). On the fixture these return empty/404/guard responses.

// GET /api/tags/<record_type>/<record_id> — app.py api_get_tags -> _get_record_tags.
func (s *server) handleApiGetTags(w http.ResponseWriter, r *http.Request) {
	// Two templates share this sub-router (/<rt>/<rid> GET+POST, /<rt>/<rid>/<tag_key> DELETE);
	// guard up front so a wrong method on either concrete shape gets the matching template's 405.
	if paramMethodGuard(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/tags/")
	seg := strings.Split(rest, "/")
	// DELETE /api/tags/<record_type>/<record_id>/<tag_key> — app.py api_delete_tag (soft-delete
	// every matching tag row, deduped by (value, is_auto)) -> {"ok": true}.
	if r.Method == http.MethodDelete && len(seg) == 3 && seg[2] != "" {
		res, err := s.db.Execute("SELECT TagKey, TagValue, IsAuto FROM sobs_record_tags FINAL "+
			"WHERE RecordType = ? AND RecordId = ? AND TagKey = ? AND IsDeleted = 0", seg[0], seg[1], seg[2])
		if err != nil || len(res.Rows) == 0 {
			errorOnly(w, http.StatusNotFound, "tag not found")
			return
		}
		seen := map[string]bool{}
		tombstones := []map[string]any{}
		version := fixedVersionMillis()
		for _, m := range rowMaps(res) {
			val := cStr(m, "TagValue")
			isAuto := cInt(m, "IsAuto")
			dk := val + "\x00" + strconv.Itoa(isAuto)
			if seen[dk] {
				continue
			}
			seen[dk] = true
			tombstones = append(tombstones, map[string]any{
				"RecordType": seg[0], "RecordId": seg[1], "TagKey": seg[2], "TagValue": val,
				"IsAuto": isAuto, "IsDeleted": 1, "Version": version,
			})
			version++
		}
		if _, err := s.db.InsertJSONEachRow("sobs_record_tags", tombstones); err != nil {
			s.dbError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true))
		return
	}
	// POST /api/tags/<record_type>/<record_id> — app.py api_add_tag: insert a manual tag -> {"ok": true}.
	if r.Method == http.MethodPost && len(seg) == 2 && seg[1] != "" {
		payload := bodyMap(r)
		key := bstr(payload, "key")
		value := bstr(payload, "value")
		if key == "" {
			errorOnly(w, http.StatusBadRequest, "key is required")
			return
		}
		if len(key) > 128 || len(value) > 512 {
			errorOnly(w, http.StatusBadRequest, "tag key or value too long")
			return
		}
		row := map[string]any{
			"RecordType": seg[0], "RecordId": seg[1], "TagKey": key, "TagValue": value,
			"IsAuto": 0, "IsDeleted": 0, "Version": fixedVersionMillis(),
		}
		if _, err := s.db.InsertJSONEachRow("sobs_record_tags", []map[string]any{row}); err != nil {
			s.dbError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, jsonenc.NewObject().Set("ok", true))
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) < 2 || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	res, err := s.db.Execute(
		"SELECT TagKey, TagValue, IsAuto FROM sobs_record_tags FINAL "+
			"WHERE RecordType = ? AND RecordId = ? AND IsDeleted = 0 ORDER BY TagKey", parts[0], parts[1])
	if err != nil {
		s.dbError(w, err)
		return
	}
	tags := []any{}
	for _, m := range rowMaps(res) {
		tags = append(tags, jsonenc.NewObject().
			Set("key", cStr(m, "TagKey")).
			Set("value", cStr(m, "TagValue")).
			Set("is_auto", cBool(m, "IsAuto")))
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("tags", tags))
}

// GET /api/traces/span/<span_id> — app.py api_raw_span. 404 when no span (fixture).
func (s *server) handleApiRawSpan(w http.ResponseWriter, r *http.Request) {
	if paramMethodGuard(w, r) {
		return
	}
	spanID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/traces/span/"))
	if spanID == "" {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("error", "span_id is required"))
		return
	}
	// mapKeys/mapValues columns preserve the chdb Map INSERTION order for the attribute objects.
	// Reading the bare Map columns decodes into a Go map (order lost) which encoding/json then
	// sorts; the Python oracle's dict(_map_to_dict(...)) keeps the insertion order json.dumps
	// emits. The parallel ordered arrays reproduce it (see orderedMapFromKV in handlers_misc.go).
	sql := "SELECT Timestamp, TraceId, SpanId, ParentSpanId, TraceState, SpanName, SpanKind, " +
		"ServiceName, ScopeName, ScopeVersion, Duration, StatusCode, StatusMessage, " +
		"mapKeys(SpanAttributes) AS span_attr_keys, mapValues(SpanAttributes) AS span_attr_values, " +
		"mapKeys(ResourceAttributes) AS res_attr_keys, mapValues(ResourceAttributes) AS res_attr_values " +
		"FROM otel_traces WHERE SpanId=?"
	params := []any{spanID}
	if traceID := strings.TrimSpace(r.URL.Query().Get("trace_id")); traceID != "" {
		sql += " AND TraceId=?"
		params = append(params, traceID)
	}
	sql += " ORDER BY Timestamp DESC LIMIT 1"
	res, err := s.db.Execute(sql, params...)
	if err != nil {
		s.dbError(w, err)
		return
	}
	if len(res.Rows) == 0 {
		writeJSON(w, http.StatusNotFound, jsonenc.NewObject().Set("error", "span not found"))
		return
	}
	// Found-span serialization (app.py:15724-15764). Empty fixture never reaches here; this is the
	// populated-data path. attrs/resource_attributes are built as ordered jsonenc Objects so the
	// recursive output-masking descends into them (a bare Go map would be masked whole).
	row := rowMaps(res)[0]
	spanAttrs := orderedMapFromKV(row["span_attr_keys"], row["span_attr_values"])
	resourceAttrs := orderedMapFromKV(row["res_attr_keys"], row["res_attr_values"])
	durationNS := spanInt64(row["Duration"])

	buildPayload := func(attrs, resAttrs *jsonenc.Object) *jsonenc.Object {
		return jsonenc.NewObject().
			Set("timestamp", cStr(row, "Timestamp")).
			Set("trace_id", cStr(row, "TraceId")).
			Set("span_id", cStr(row, "SpanId")).
			Set("parent_span_id", cStr(row, "ParentSpanId")).
			Set("trace_state", cStr(row, "TraceState")).
			Set("name", cStr(row, "SpanName")).
			Set("kind", cStr(row, "SpanKind")).
			Set("service", cStr(row, "ServiceName")).
			Set("scope_name", cStr(row, "ScopeName")).
			Set("scope_version", cStr(row, "ScopeVersion")).
			Set("duration_ns", durationNS).
			Set("duration_ms", roundHalfEven(float64(durationNS)/1_000_000, 3)).
			Set("status_code", cStr(row, "StatusCode")).
			Set("status_message", cStr(row, "StatusMessage")).
			Set("attributes", attrs).
			Set("resource_attributes", resAttrs)
	}

	payload := buildPayload(spanAttrs, resourceAttrs)
	maskedPayload := s.maskValueForOutput(payload)
	raw, _ := jsonDumpsIndent2(maskedPayload)
	truncated := false
	if len(raw) > rawSpanMaxBytes {
		truncated = true
		// Truncate over-long attribute string values to 512 chars + "…" (app.py _ATTR_TRUNCATE).
		payload = buildPayload(truncateAttrObject(spanAttrs, 512), truncateAttrObject(resourceAttrs, 512))
		maskedPayload = s.maskValueForOutput(payload)
		raw, _ = jsonDumpsIndent2(maskedPayload)
	}
	// masked_jsonify masks again (idempotent) with mask_sql_fields=True, then sorts keys.
	writeJSON(w, http.StatusOK, s.maskPayloadForOutput(jsonenc.NewObject().
		Set("span", maskedPayload).Set("raw", raw).Set("truncated", truncated), true))
}

// rawSpanMaxBytes mirrors app.py _RAW_SPAN_MAX_BYTES (32 KB display cap on the pretty-printed span).
const rawSpanMaxBytes = 32 * 1024

// truncateAttrObject mirrors app.py's _ATTR_TRUNCATE pass: shorten over-long string values to the
// first n code points + "…"; non-string values pass through. Key order is preserved.
func truncateAttrObject(o *jsonenc.Object, n int) *jsonenc.Object {
	out := jsonenc.NewObject()
	for _, k := range o.Keys() {
		v, _ := o.Get(k)
		if s, ok := v.(string); ok {
			if r := []rune(s); len(r) > n {
				out.Set(k, string(r[:n])+"…")
				continue
			}
		}
		out.Set(k, v)
	}
	return out
}

// spanInt64 mirrors Python int(row[col]) for a chdb numeric cell (UInt64 arrives as a JSON string).
func spanInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
			return i
		}
	}
	return 0
}

// GET /api/table-explorer/table/<name> — app.py api_table_explorer_table: query-page guard,
// allowlist guard (403), then {columns, ddl, sample} for the single table.
func (s *server) handleApiTableExplorerTable(w http.ResponseWriter, r *http.Request) {
	if paramMethodGuard(w, r) {
		return
	}
	if !s.queryPageEnabled() {
		writeJSON(w, http.StatusNotFound,
			jsonenc.NewObject().Set("ok", false).Set("error", "Table Explorer is unavailable."))
		return
	}
	name := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/table-explorer/table/"))
	if !queryAllowedTableSet[name] {
		s.writeMaskedJSON(w, http.StatusForbidden,
			jsonenc.NewObject().Set("ok", false).Set("error", "Table '"+name+"' is not accessible."))
		return
	}
	cols, err := s.describeTableExtended(name)
	if err != nil {
		s.writeMaskedJSON(w, http.StatusInternalServerError,
			jsonenc.NewObject().Set("ok", false).Set("error", err.Error()))
		return
	}
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("table", name).
		Set("columns", cols).Set("ddl", s.getTableDDL(name)).Set("sample", s.getTableSample(name, 5)))
}
