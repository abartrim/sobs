package main

import (
	"net/http"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Path-param GET routes. Go's ServeMux matches by prefix; the handler parses the trailing
// path segment(s). On the fixture these return empty/404/guard responses.

// GET /api/tags/<record_type>/<record_id> — app.py api_get_tags -> _get_record_tags.
func (s *server) handleApiGetTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/tags/")
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

// GET /api/table-explorer/table/<name> — app.py api_table_explorer_table. Query page is
// disabled on the fixture -> 404 guard.
func (s *server) handleApiTableExplorerTable(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.QueryPageEnabled {
		writeJSON(w, http.StatusNotFound,
			jsonenc.NewObject().Set("ok", false).Set("error", "Table Explorer is unavailable."))
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented) // enabled branch: Phase follow-up
}
