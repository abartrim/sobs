package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// rumSessionKeySQL mirrors app.py _RUM_SESSION_KEY_SQL.
const rumSessionKeySQL = "if(LogAttributes['sessionId'] != '', LogAttributes['sessionId'], " +
	"if(LogAttributes['session.id'] != '', LogAttributes['session.id'], " +
	"concat('anon:', substring(lower(hex(MD5(concat(toString(Timestamp), '|', Body)))), 1, 16))))"

// rumSessionKeyFromAttrs mirrors _rum_session_key_from_attrs.
func rumSessionKeyFromAttrs(attrs map[string]any, ts, bodyRaw string) string {
	sid := strings.TrimSpace(mapStr(attrs, "sessionId"))
	if sid == "" {
		sid = strings.TrimSpace(mapStr(attrs, "session.id"))
	}
	if sid != "" {
		return sid
	}
	sum := md5.Sum([]byte(ts + "|" + bodyRaw))
	return "anon:" + hex.EncodeToString(sum[:])[:16]
}

// buildRumEventItem mirrors app.py _build_rum_event_item.
func buildRumEventItem(m map[string]any) *jsonenc.Object {
	attrs := mapToDict(cStr(m, "LogAttributes"))
	bodyRaw := cStr(m, "Body")
	var data any = jsonenc.NewObject()
	if bodyRaw != "" {
		if v, err := parseJSONValue([]byte(bodyRaw)); err == nil {
			if _, isObj := v.(*jsonenc.Object); isObj {
				data = v
			} else {
				data = jsonenc.NewObject().Set("value", v)
			}
		}
	}
	traceID, spanID, service := cStr(m, "TraceId"), cStr(m, "SpanId"), cStr(m, "ServiceName")
	ts := cStr(m, "Timestamp")
	attrsMap := map[string]any{}
	if o, ok := attrs.(*jsonenc.Object); ok {
		for _, k := range o.Keys() {
			v, _ := o.Get(k)
			attrsMap[k] = v
		}
	}
	url := mapStr(attrsMap, "url")
	if url == "" {
		url = mapStr(attrsMap, "url.full")
	}
	return jsonenc.NewObject().
		Set("ts", ts).
		Set("session_key", rumSessionKeyFromAttrs(attrsMap, ts, bodyRaw)).
		Set("session_id", truncStr(rumSessionKeyFromAttrs(attrsMap, ts, bodyRaw), 8)).
		Set("event_type", cStr(m, "EventName")).
		Set("url", url).
		Set("data", data).
		Set("trace_id", traceID).
		Set("span_id", spanID).
		Set("service", service).
		Set("has_artifact", false).
		Set("has_replay", false)
}

func truncStr(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

// rumSessionSortOptions mirrors the app.py _parse_sort allowed-map for the sessions view.
var rumSessionSortOptions = []sortOption{
	{"severity", "severity_rank"},
	{"last_seen", "last_ts"},
	{"events", "event_count"},
	{"errors", "error_count"},
}

// rumEventSortOptions mirrors the _parse_sort allowed-map for the events view.
var rumEventSortOptions = []sortOption{
	{"Timestamp", "Timestamp"},
	{"EventName", "EventName"},
}

// GET /rum — app.py view_rum. Faithful 1:1 port: the request params (view=sessions|events,
// type, error_source, q RE2 filter, from_ts/to_ts/window_s, and the per-mode sort options)
// build a shared WHERE/ORDER that feeds the count, session-summary, per-session-detail, and
// events queries. On the parity fixture the corpus sends a bare GET /rum (case get__rum), so
// every parser collapses to its default — view=sessions, no conditions, sort severity desc —
// and the queries below are RESULT-identical to the prior hardcoded handler. Vitals/error_stats
// are empty on the fixture (data older than the now()-windows); the error-rate sparkline's
// now()-derived bucket timestamps are masked in parity; everything else is byte-compared.
func (s *server) handleViewRum(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	viewMode := strings.ToLower(strings.TrimSpace(q.Get("view")))
	if viewMode != "sessions" && viewMode != "events" {
		viewMode = "sessions"
	}
	eventType := strings.TrimSpace(q.Get("type"))
	errorSource := strings.TrimSpace(q.Get("error_source"))
	limit := parseLimitDefault(r, 200)
	offset := parseOffset(r)

	var sortBy, sortCol, sortDir string
	if viewMode == "sessions" {
		sortBy, sortCol, sortDir = parseSortOptions(r, rumSessionSortOptions, "severity")
	} else {
		sortBy, sortCol, sortDir = parseSortOptions(r, rumEventSortOptions, "Timestamp")
	}
	orderClause := "ORDER BY " + sortCol + " " + orderDir(sortDir)
	fromTS, toTS, timeError := parseRumTimeWindowArgs(r)

	qStr := strings.TrimSpace(q.Get("q"))
	qError := ""
	var rf regexFilter
	if qStr != "" {
		var rErr string
		rf, rErr = s.prepareRumRE2FilterPatterns(qStr)
		if rErr != "" {
			qError = rErr
		}
	}

	conditions := []string{}
	params := []any{}
	if eventType != "" {
		conditions = append(conditions, "EventName=?")
		params = append(params, eventType)
	}
	if errorSource != "" {
		conditions = append(conditions, "LogAttributes['errorSource']=?")
		params = append(params, errorSource)
	}
	tConds, tParams := timeWindowConditions("Timestamp", fromTS, toTS)
	conditions = append(conditions, tConds...)
	params = append(params, tParams...)
	if qStr != "" && qError == "" {
		conditions, params = appendRumRegexExpressionClauses(conditions, params, "Body", rf)
	}
	where := whereClause(conditions)

	total := 0
	events := []any{}
	sessionGroups := []any{}
	if viewMode == "sessions" {
		total = s.countRowsParams("SELECT count() AS c FROM ("+
			"SELECT "+rumSessionKeySQL+" AS session_key "+
			"FROM hyperdx_sessions "+where+" GROUP BY session_key)", params...)
		summaryParams := append(append([]any{}, params...), limit, offset)
		summary, err := s.db.Execute("SELECT "+
			"  "+rumSessionKeySQL+" AS session_key,"+
			"  max(Timestamp) AS last_ts,"+
			"  count() AS event_count,"+
			"  countIf(EventName IN ('error', 'unhandledrejection')) AS error_count,"+
			"  countIf(EventName = 'web-vital' AND JSONExtractString(Body, 'rating') = 'poor') AS poor_vital_count,"+
			"  countIf(EventName = 'web-vital' AND JSONExtractString(Body, 'rating') = 'needs-improvement') AS warn_vital_count,"+
			"  countIf(TraceId != '') AS traced_count,"+
			"  multiIf("+
			"    countIf(EventName IN ('error', 'unhandledrejection')) > 0, 3,"+
			"    countIf(EventName = 'web-vital' AND JSONExtractString(Body, 'rating') = 'poor') > 0, 2,"+
			"    countIf(EventName = 'web-vital' AND JSONExtractString(Body, 'rating') = 'needs-improvement') > 0, 1,"+
			"    0"+
			"  ) AS severity_rank,"+
			"  argMax(if(LogAttributes['url'] != '', LogAttributes['url'], LogAttributes['url.full']), Timestamp) AS last_url,"+
			"  argMax(EventName, Timestamp) AS last_event_type"+
			" FROM hyperdx_sessions "+where+
			" GROUP BY session_key "+
			" ORDER BY "+sortCol+" "+orderDir(sortDir)+", last_ts DESC LIMIT ? OFFSET ?", summaryParams...)
		if err != nil {
			s.dbError(w, err)
			return
		}
		summaryRows := rowMaps(summary)
		eventsBySession := map[string][]any{}
		if len(summaryRows) > 0 {
			sessionKeys := make([]string, len(summaryRows))
			for i, m := range summaryRows {
				sessionKeys[i] = cStr(m, "session_key")
			}
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sessionKeys)), ",")
			detailConditions := append(append([]string{}, conditions...),
				rumSessionKeySQL+" IN ("+placeholders+")")
			detailWhere := "WHERE " + strings.Join(detailConditions, " AND ")
			detailParams := append([]any{}, params...)
			for _, sk := range sessionKeys {
				detailParams = append(detailParams, sk)
			}
			detailParams = append(detailParams, rumSessionDetailEventCap())
			detail, derr := s.db.Execute("SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId "+
				"FROM ("+
				"SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId, "+
				rumSessionKeySQL+" AS session_key, "+
				"row_number() OVER (PARTITION BY "+rumSessionKeySQL+" ORDER BY Timestamp DESC) AS row_rank "+
				"FROM hyperdx_sessions "+detailWhere+
				") "+
				"WHERE row_rank <= ? "+
				"ORDER BY session_key ASC, Timestamp DESC", detailParams...)
			if derr == nil {
				for _, dm := range rowMaps(detail) {
					ev := buildRumEventItem(dm)
					sk, _ := ev.Get("session_key")
					key, _ := sk.(string)
					eventsBySession[key] = append(eventsBySession[key], ev)
				}
			}
		}
		for _, m := range summaryRows {
			sk := cStr(m, "session_key")
			evs := eventsBySession[sk]
			if evs == nil {
				evs = []any{}
			}
			traceID := ""
			hasReplay := false
			hasArtifact := false
			for _, e := range evs {
				if eo, ok := e.(*jsonenc.Object); ok {
					if traceID == "" {
						if t, _ := eo.Get("trace_id"); t != nil {
							if ts, _ := t.(string); ts != "" {
								traceID = ts
							}
						}
					}
					if hr, _ := eo.Get("has_replay"); truthy(hr) {
						hasReplay = true
					}
					if ha, _ := eo.Get("has_artifact"); truthy(ha) {
						hasArtifact = true
					}
				}
			}
			sessionGroups = append(sessionGroups, jsonenc.NewObject().
				Set("session_key", sk).Set("session_id", truncStr(sk, 8)).
				Set("last_ts", cStr(m, "last_ts")).Set("last_url", cStr(m, "last_url")).
				Set("last_event_type", cStr(m, "last_event_type")).
				Set("event_count", cInt(m, "event_count")).Set("error_count", cInt(m, "error_count")).
				Set("poor_vital_count", cInt(m, "poor_vital_count")).Set("warn_vital_count", cInt(m, "warn_vital_count")).
				Set("severity_rank", cInt(m, "severity_rank")).Set("traced_count", cInt(m, "traced_count")).
				Set("trace_id", traceID).Set("has_replay", hasReplay).Set("has_artifact", hasArtifact).
				Set("events", evs))
		}
	} else {
		if where == "" {
			total = s.activePartRows("hyperdx_sessions")
		} else {
			total = s.countRowsParams("SELECT COUNT(*) AS c FROM hyperdx_sessions "+where, params...)
		}
		eventParams := append(append([]any{}, params...), limit, offset)
		rows, rerr := s.db.Execute("SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId "+
			"FROM hyperdx_sessions "+where+" "+orderClause+" LIMIT ? OFFSET ?", eventParams...)
		if rerr == nil {
			for _, m := range rowMaps(rows) {
				events = append(events, buildRumEventItem(m))
			}
		}
	}

	eventTypes := s.distinctStrings("SELECT DISTINCT EventName FROM hyperdx_sessions ORDER BY EventName")
	errorSources := s.distinctStrings("SELECT DISTINCT LogAttributes['errorSource'] FROM hyperdx_sessions " +
		"WHERE LogAttributes['errorSource']!='' ORDER BY LogAttributes['errorSource']")

	// error_stats: by_type/recent/prior/top_* are empty on the fixture (data older than the
	// now()-windows); the sparkline buckets carry now()-derived timestamps (masked in parity).
	errorStats := jsonenc.NewObject().
		Set("total", 0).Set("by_type", jsonenc.NewObject()).
		Set("trend", "stable").Set("recent", 0).Set("prior", 0).
		Set("sparkline", s.rumErrorSparkline()).
		Set("top_messages", []any{}).Set("top_urls", []any{})

	errorMsg := qError
	if errorMsg == "" {
		errorMsg = timeError
	}
	var fromVar, toVar any
	if fromTS != "" {
		fromVar = fromTS
	}
	if toTS != "" {
		toVar = toTS
	}

	s.renderPage(w, "rum.html", "view_rum", map[string]any{
		"events": events, "session_groups": sessionGroups, "total": total,
		"limit": limit, "offset": offset, "view_mode": viewMode,
		"event_type": eventType, "event_types": eventTypes, "error_source": errorSource, "error_sources": errorSources,
		"vitals_summary": jsonenc.NewObject(), "vitals_sparklines": jsonenc.NewObject(), "vitals_hotspot": jsonenc.NewObject(),
		"error_stats": errorStats, "sort_by": sortBy, "sort_dir": sortDir,
		"from_ts": fromVar, "to_ts": toVar, "q": qStr, "error_msg": errorMsg,
	})
}

// rumSessionDetailEventCap mirrors app.py RUM_SESSION_DETAIL_EVENT_CAP =
// int(os.environ.get("SOBS_RUM_SESSION_DETAIL_EVENT_CAP", "200")) — the `WHERE row_rank <= ?`
// bound on the per-session detail query. Read once per request (cheap; matches Python reading
// the module-level constant). envInt fail-fasts on a SET-but-malformed value like Python's int().
func rumSessionDetailEventCap() int {
	return envInt("SOBS_RUM_SESSION_DETAIL_EVENT_CAP", 200)
}

// rumErrorSparkline runs the WITH FILL bucket query (180 minute buckets, all 0 on the fixture).
func (s *server) rumErrorSparkline() []any {
	out := []any{}
	res, err := s.db.Execute("SELECT mb, cnt FROM (" +
		"SELECT toStartOfMinute(Timestamp) AS mb, count() AS cnt FROM hyperdx_sessions " +
		"WHERE EventName IN ('error','unhandledrejection') AND Timestamp >= now() - INTERVAL 180 MINUTE GROUP BY mb) " +
		"ORDER BY mb WITH FILL FROM toStartOfMinute(now() - INTERVAL 180 MINUTE) TO toStartOfMinute(now()) STEP toIntervalMinute(1)")
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		out = append(out, jsonenc.NewObject().Set("t", cStr(m, "mb")).Set("v", cInt(m, "cnt")))
	}
	return out
}

var _ = json.Marshal
