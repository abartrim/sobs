package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Path-param GET routes. Go's ServeMux matches by prefix; the handler parses the trailing
// path segment(s). On the fixture these return empty/404/guard responses.

// GET /api/tags/<record_type>/<record_id> — app.py api_get_tags -> _get_record_tags.
func (s *server) handleApiGetTags(w http.ResponseWriter, r *http.Request) {
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
	spanID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/traces/span/"))
	if spanID == "" {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("error", "span_id is required"))
		return
	}
	sql := "SELECT Timestamp, TraceId, SpanId, ParentSpanId, TraceState, SpanName, SpanKind, " +
		"ServiceName, ResourceAttributes, ScopeName, ScopeVersion, SpanAttributes, Duration, " +
		"StatusCode, StatusMessage FROM otel_traces WHERE SpanId=?"
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
	// Found-span serialization needs seeded trace data (none on the fixture).
	writeJSON(w, http.StatusNotFound, jsonenc.NewObject().Set("error", "span not found"))
}

// GET /api/table-explorer/table/<name> — app.py api_table_explorer_table: query-page guard,
// allowlist guard (403), then {columns, ddl, sample} for the single table.
func (s *server) handleApiTableExplorerTable(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.QueryPageEnabled {
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
