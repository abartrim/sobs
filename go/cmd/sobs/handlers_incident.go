package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Incident-correlation port of app.py view_incident's helper surface. The param-less branch
// (the only corpus-tested path) is rendered by handleViewIncident verbatim; everything here
// drives the has-reference branches the corpus never exercises, so it cannot regress parity.

// Incident tuning constants — app.py _INCIDENT_* module globals.
const (
	incidentMaxRelatedErrors     = 50
	incidentMaxRelatedRumEvents  = 20
	incidentWindowDefaultMinutes = 30
	incidentWindowMaxMinutes     = 180
)

// metricGroupDef mirrors one entry of app.py _METRIC_GROUP_DEFS (key, label, icon, patterns).
type metricGroupDef struct {
	key      string
	label    string
	icon     string
	patterns []string
}

// metricGroupDefs mirrors app.py _METRIC_GROUP_DEFS, in order.
var metricGroupDefs = []metricGroupDef{
	{"resource", "Resource Pressure", "bi-cpu", []string{
		"cpu", "memory", "mem_usage", "node.cpu", "node.memory", "system.cpu", "system.memory",
	}},
	{"io", "I/O & Storage", "bi-hdd", []string{
		"blkio", "fs_read", "fs_write", "disk", "network", "bandwidth",
	}},
	{"k8s", "Kubernetes State", "bi-layers", []string{
		"kube_pod", "kube_node", "kube_deploy", "pod_phase", "pod_status", "replica",
		"feature_enabled", "tasks_state",
	}},
	{"infra", "Infrastructure", "bi-server", []string{
		"apiserver", "etcd", "scheduler", "controller_manager",
	}},
}

var whitespaceRe = regexp.MustCompile(`\s+`)

// attrsToStringMap normalizes a ClickHouse Map(String,String) column value (which arrives
// from FORMAT JSON either as a JSON object -> map[string]any, or as a serialized JSON
// string) into a flat string map, mirroring _map_to_dict + str() coercion on access.
func attrsToStringMap(v any) map[string]string {
	out := map[string]string{}
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			out[k] = toStr(vv)
		}
	case *jsonenc.Object:
		for _, k := range x.Keys() {
			vv, _ := x.Get(k)
			out[k] = toStr(vv)
		}
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return out
		}
		var parsed map[string]any
		if json.Unmarshal([]byte(s), &parsed) == nil {
			for k, vv := range parsed {
				out[k] = toStr(vv)
			}
		}
	}
	return out
}

// attrGetDef mirrors `str(attrs.get(key, default))` for a normalized string-attr map. (Named
// distinctly from the variadic attrGet(any, ...string) in query_filters.go.)
func attrGetDef(attrs map[string]string, key, def string) string {
	if v, ok := attrs[key]; ok {
		return v
	}
	return def
}

// compactText mirrors app.py _compact_text(value, limit=220).
func compactText(value string, limit int) string {
	text := strings.TrimSpace(whitespaceRe.ReplaceAllString(value, " "))
	if len([]rune(text)) <= limit {
		return text
	}
	cut := limit - 1
	if cut < 0 {
		cut = 0
	}
	r := []rune(text)
	return strings.TrimRight(string(r[:cut]), " \t\n\r\f\v") + "..."
}

// tryPrettyJSONText mirrors app.py _try_pretty_json_text: (is_json, pretty) for an object/array.
func tryPrettyJSONText(rawValue string) (bool, string) {
	raw := strings.TrimSpace(rawValue)
	if raw == "" || (raw[0] != '{' && raw[0] != '[') {
		return false, ""
	}
	v, err := parseJSONValue([]byte(raw))
	if err != nil {
		return false, ""
	}
	pretty, err := jsonDumpsIndent2NoEsc(v)
	if err != nil {
		return false, ""
	}
	return true, pretty
}

// errorIDHash mirrors app.py _error_id: md5 of the pipe-joined identity fields.
func errorIDHash(ts, service, errType, message, traceID, spanID string) string {
	raw := strings.Join([]string{ts, service, errType, message, traceID, spanID}, "|")
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// firstScalarFromJSON mirrors the nested _first_scalar in _extract_structured_error_summary.
// Dicts are *jsonenc.Object (parseJSONValue) so iteration follows Python's INSERTION order:
// both the direct-match and the descend passes are order-faithful (a plain Go map would make
// the descend pass — first non-empty wins — nondeterministic when several keys could match).
func firstScalarFromJSON(value any, keyset map[string]bool, depth int) string {
	if depth > 5 {
		return ""
	}
	switch x := value.(type) {
	case *jsonenc.Object:
		// Prefer direct key matches before descending, exactly as Python does.
		for _, k := range x.Keys() {
			inner, _ := x.Get(k)
			if keyset[strings.ToLower(k)] && isJSONScalar(inner) {
				return strings.TrimSpace(jsonScalarToStr(inner))
			}
		}
		for _, k := range x.Keys() {
			inner, _ := x.Get(k)
			if found := firstScalarFromJSON(inner, keyset, depth+1); found != "" {
				return found
			}
		}
		return ""
	case map[string]any:
		// Defensive: a plain map can still arrive from other callers. Python iterates dicts
		// in insertion order; Go map order is unordered, so this branch is best-effort.
		for k, inner := range x {
			if keyset[strings.ToLower(k)] && isJSONScalar(inner) {
				return strings.TrimSpace(jsonScalarToStr(inner))
			}
		}
		for _, inner := range x {
			if found := firstScalarFromJSON(inner, keyset, depth+1); found != "" {
				return found
			}
		}
		return ""
	case []any:
		for _, inner := range x {
			if found := firstScalarFromJSON(inner, keyset, depth+1); found != "" {
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

func isJSONScalar(v any) bool {
	switch v.(type) {
	case string, bool, json.Number, float64:
		return true
	}
	return false
}

// jsonScalarToStr renders a JSON scalar the way Python str() would (json.Number preserves the
// integer/float literal; bool -> True/False).
func jsonScalarToStr(v any) string {
	switch x := v.(type) {
	case json.Number:
		return x.String()
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		return pyFloatRepr(x)
	}
	return fmt.Sprintf("%v", v)
}

// summaryFromParsed mirrors the nested _to_summary in _extract_structured_error_summary.
func summaryFromParsed(parsed any) string {
	if arr, ok := parsed.([]any); ok {
		if len(arr) > 0 {
			parsed = arr[0]
		} else {
			parsed = jsonenc.NewObject()
		}
	}
	// _to_summary only proceeds for a dict; parseJSONValue gives *jsonenc.Object.
	switch parsed.(type) {
	case *jsonenc.Object:
	default:
		return ""
	}
	obj := parsed
	textKeys := map[string]bool{
		"message": true, "error": true, "error_message": true, "errormessage": true,
		"detail": true, "description": true, "reason": true, "body": true, "msg": true,
	}
	codeKeys := map[string]bool{"code": true, "status": true, "status_code": true, "error_code": true, "errorcode": true}
	typeKeys := map[string]bool{"type": true, "error_type": true, "exception": true, "name": true}

	messageText := firstScalarFromJSON(obj, textKeys, 0)
	codeText := firstScalarFromJSON(obj, codeKeys, 0)
	typeText := firstScalarFromJSON(obj, typeKeys, 0)

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

// extractStructuredErrorSummary mirrors app.py _extract_structured_error_summary:
// (summary, summary_from_json).
func extractStructuredErrorSummary(message, rawBody string) (string, bool) {
	for _, candidate := range []string{message, rawBody} {
		raw := strings.TrimSpace(candidate)
		if raw == "" {
			continue
		}
		if raw[0] != '{' && raw[0] != '[' {
			continue
		}
		// Order-preserving parse: dicts become *jsonenc.Object so both the summary descend
		// pass and the json.dumps fallback keep Python's INSERTION order (a key-sorting
		// decode would reorder the fallback dump — app.py line ~10647).
		parsed, err := parseJSONValue([]byte(raw))
		if err != nil {
			continue
		}
		summary := summaryFromParsed(parsed)
		if summary != "" {
			return summary, true
		}
		// json.dumps(parsed, ensure_ascii=False): default (spaced) separators ", "/": ",
		// insertion order, raw UTF-8.
		dumped := string(jsonenc.Encode(parsed, jsonenc.Options{
			SortKeys: false, EnsureASCII: false, ItemSep: ", ", KeySep: ": ",
		}))
		return compactText(dumped, 220), true
	}
	basis := message
	if basis == "" {
		basis = rawBody
	}
	return compactText(basis, 220), false
}

// buildErrorItem mirrors app.py _build_error_item: an error-row map from ERROR_SOURCES_SQL
// (`SELECT *`) into the dashboard error dict. Stack demangling uses the source-map (identity
// unless SOBS_SOURCE_MAP_ENABLE).
func (s *server) buildErrorItem(m map[string]any) map[string]any {
	attrs := attrsToStringMap(m["LogAttributes"])
	ts := cStr(m, "Timestamp")
	service := cStr(m, "ServiceName")
	errType := attrGetDef(attrs, "exception.type", "Error")
	message := attrGetDef(attrs, "exception.message", cStr(m, "Body"))
	rawBody := cStr(m, "Body")
	messageSummary, summaryFromJSON := extractStructuredErrorSummary(message, rawBody)
	messageIsJSON, messagePretty := tryPrettyJSONText(message)
	bodyIsJSON, bodyPretty := tryPrettyJSONText(rawBody)
	stack := s.srcMap.demangleStack(attrGetDef(attrs, "exception.stacktrace", ""))
	stackIsJSON, stackPretty := tryPrettyJSONText(stack)
	traceID := cStr(m, "TraceId")
	spanID := cStr(m, "SpanId")
	eid := errorIDHash(ts, service, errType, message, traceID, spanID)
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
		"url":                  attrGetDef(attrs, "url.full", ""),
		"error_source":         attrGetDef(attrs, "error.source", ""),
		"page_title":           attrGetDef(attrs, "browser.page.title", ""),
		"viewport":             attrGetDef(attrs, "browser.viewport", ""),
		"artifact_type":        attrGetDef(attrs, "artifact.type", ""),
		"artifact_id":          attrGetDef(attrs, "artifact.id", ""),
		"artifact_url":         attrGetDef(attrs, "artifact.url", ""),
		"replay_id":            attrGetDef(attrs, "replay.id", ""),
		"replay_url":           attrGetDef(attrs, "replay.url", ""),
	}
}

// resolvedErrorIDs mirrors app.py _get_resolved_error_ids.
func (s *server) resolvedErrorIDs() map[string]bool {
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

// parseTimeWindowArgsQuery mirrors app.py _parse_time_window_args: (from_ts, to_ts, time_error).
// Takes a url.Values map (vs the *http.Request-based parseTimeWindowArgs in query_filters.go).
func parseTimeWindowArgsQuery(q map[string][]string) (string, string, string) {
	getArg := func(k string) string {
		if vs, ok := q[k]; ok && len(vs) > 0 {
			return strings.TrimSpace(vs[0])
		}
		return ""
	}
	fromRaw := getArg("from_ts")
	toRaw := getArg("to_ts")
	windowRaw := getArg("window_s")

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
			return "", "", "Invalid time value. Use ISO-8601, e.g. 2026-03-29T12:00:00Z"
		}
		if windowS < 1 {
			windowS = 1
		}
		fromDT, ok := parseISOLocalNaive(fromTS)
		if !ok {
			return "", "", "Invalid time value. Use ISO-8601, e.g. 2026-03-29T12:00:00Z"
		}
		toTS = normalizeCHTimestamp(fromDT.Add(time.Duration(windowS) * time.Second))
	}
	if fromTS != "" && toTS != "" {
		fromDT, ok1 := parseISOLocalNaive(fromTS)
		toDT, ok2 := parseISOLocalNaive(toTS)
		if !ok1 || !ok2 {
			return "", "", "Invalid time value. Use ISO-8601, e.g. 2026-03-29T12:00:00Z"
		}
		if !toDT.After(fromDT) {
			return "", "", "Invalid time window: to_ts must be later than from_ts"
		}
	}
	return fromTS, toTS, ""
}

// parseISOLocalNaive parses a normalized ClickHouse timestamp ("2006-01-02 15:04:05.000000")
// as a naive UTC time — mirrors datetime.fromisoformat on _normalize_ch_timestamp output.
func parseISOLocalNaive(s string) (time.Time, bool) {
	s = strings.Replace(strings.TrimSpace(s), " ", "T", 1)
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02T15:04:05-07:00",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// tsStrToEpochMs mirrors app.py _ts_str_to_epoch_ms.
func tsStrToEpochMs(ts string) float64 {
	ts = strings.TrimSpace(ts)
	if idx := strings.Index(ts, "."); idx >= 0 {
		base := ts[:idx]
		frac := ts[idx+1:]
		if len(frac) > 6 {
			frac = frac[:6]
		}
		for len(frac) < 6 {
			frac += "0"
		}
		ts = base + "." + frac
	}
	t, ok := parseISOLocalNaive(ts)
	if !ok {
		return 0.0
	}
	return float64(t.UnixNano()) / 1e6
}

// uniqStrings mirrors the _uniq helper in _fetch_trace_metric_context.
func uniqStrings(values []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		if v != "" && !seen[v] {
			out = append(out, v)
			seen[v] = true
		}
	}
	return out
}

// serviceFamilies mirrors the _service_families helper in _fetch_trace_metric_context.
func serviceFamilies(values []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, svc := range values {
		candidate := svc
		if idx := strings.LastIndex(svc, "-"); idx >= 0 {
			candidate = svc[:idx]
		}
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && !seen[candidate] {
			out = append(out, candidate)
			seen[candidate] = true
		}
	}
	return out
}

// windowCopyCounts mirrors app.py _window_copy_counts.
func (s *server) windowCopyCounts(windowIDs []string) map[string]int {
	out := map[string]int{}
	if len(windowIDs) == 0 {
		return out
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(windowIDs)), ",")
	params := make([]any, len(windowIDs))
	for i, id := range windowIDs {
		params[i] = id
	}
	res, err := s.db.Execute(
		"SELECT WindowId, countDistinct(SourceTable) AS c "+
			"FROM sobs_raw_window_copy_state FINAL "+
			"WHERE WindowId IN ("+placeholders+") "+
			"GROUP BY WindowId", params...)
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		out[cStr(m, "WindowId")] = cInt(m, "c")
	}
	return out
}

// listTraceOverlappingRawWindows mirrors app.py _list_trace_overlapping_raw_windows.
func (s *server) listTraceOverlappingRawWindows(serviceNames []string, startTS, endTS string, limit int) []any {
	whereParts := []string{
		"WindowEnd >= parseDateTime64BestEffort(?, 9)",
		"WindowStart <= parseDateTime64BestEffort(?, 9)",
	}
	params := []any{startTS, endTS}
	if len(serviceNames) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(serviceNames)), ",")
		whereParts = append(whereParts, "(ServiceName = '' OR ServiceName IN ("+placeholders+"))")
		for _, sn := range serviceNames {
			params = append(params, sn)
		}
	}
	whereSQL := strings.Join(whereParts, " AND ")
	clamped := limit
	if clamped < 1 {
		clamped = 1
	} else if clamped > 100 {
		clamped = 100
	}
	params = append(params, clamped)
	res, err := s.db.Execute(
		"SELECT Id, SignalType, SignalRef, ServiceName, Namespace, NodeName, WindowStart, WindowEnd "+
			"FROM sobs_raw_windows FINAL "+
			"WHERE "+whereSQL+" "+
			"ORDER BY WindowStart DESC "+
			"LIMIT ?", params...)
	if err != nil {
		return []any{}
	}
	rows := rowMaps(res)
	if len(rows) == 0 {
		return []any{}
	}
	expectedCount := len(rawMetricTables)
	windowIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		windowIDs = append(windowIDs, cStr(r, "Id"))
	}
	copiedCounts := s.windowCopyCounts(windowIDs)
	out := []any{}
	for _, r := range rows {
		windowID := cStr(r, "Id")
		copiedCount := copiedCounts[windowID]
		out = append(out, map[string]any{
			"id":             windowID,
			"signal_type":    cStr(r, "SignalType"),
			"signal_ref":     cStr(r, "SignalRef"),
			"service_name":   cStr(r, "ServiceName"),
			"namespace":      cStr(r, "Namespace"),
			"node_name":      cStr(r, "NodeName"),
			"window_start":   cStr(r, "WindowStart"),
			"window_end":     cStr(r, "WindowEnd"),
			"copied_count":   copiedCount,
			"expected_count": expectedCount,
			"copy_complete":  copiedCount >= expectedCount,
		})
	}
	return out
}

// groupMetricSeries mirrors app.py _group_metric_series.
func groupMetricSeries(series []any) []any {
	buckets := map[string][]any{}
	for _, d := range metricGroupDefs {
		buckets[d.key] = []any{}
	}
	other := []any{}
	for _, sAny := range series {
		s, _ := sAny.(map[string]any)
		m := strings.ToLower(toStr(s["metric"]))
		placed := false
		for _, def := range metricGroupDefs {
			for _, p := range def.patterns {
				if strings.Contains(m, p) {
					buckets[def.key] = append(buckets[def.key], sAny)
					placed = true
					break
				}
			}
			if placed {
				break
			}
		}
		if !placed {
			other = append(other, sAny)
		}
	}
	result := []any{}
	for _, def := range metricGroupDefs {
		if len(buckets[def.key]) > 0 {
			result = append(result, map[string]any{
				"label": def.label, "icon": def.icon, "key": def.key, "metrics": buckets[def.key],
			})
		}
	}
	if len(other) > 0 {
		result = append(result, map[string]any{
			"label": "Other", "icon": "bi-graph-up", "key": "other", "metrics": other,
		})
	}
	return result
}

// computeHealthChips mirrors app.py _compute_health_chips.
func computeHealthChips(series []any) []any {
	chips := []any{}
	for _, sAny := range series {
		s, _ := sAny.(map[string]any)
		m := strings.ToLower(toStr(s["metric"]))
		avg, _ := fStr(s["avg"])
		maxV, _ := fStr(s["max"])
		switch {
		case strings.Contains(m, "cpu") && (strings.Contains(m, "utiliz") || strings.Contains(m, "usage")):
			level := "ok"
			if avg > 80 {
				level = "crit"
			} else if avg > 60 {
				level = "warn"
			}
			chips = append(chips, map[string]any{
				"label": "CPU", "value": fmt.Sprintf("%.1f%%", avg), "level": level, "icon": "bi-cpu",
			})
		case strings.Contains(m, "memory_failures") || strings.Contains(m, "mem_failures"):
			level := "ok"
			if maxV > 1000 {
				level = "crit"
			} else if maxV > 0 {
				level = "warn"
			}
			chips = append(chips, map[string]any{
				"label": "Mem Faults", "value": strconv.Itoa(int(maxV)), "level": level, "icon": "bi-exclamation-triangle",
			})
		case strings.Contains(m, "memory") && strings.Contains(m, "usage") && !strings.Contains(m, "failures"):
			gb := avg / math.Pow(1024, 3)
			var valStr string
			if gb >= 0.1 {
				valStr = fmt.Sprintf("%.1fGB", gb)
			} else {
				valStr = fmt.Sprintf("%.0fMB", avg/1048576)
			}
			chips = append(chips, map[string]any{
				"label": "Memory", "value": valStr, "level": "ok", "icon": "bi-memory",
			})
		case strings.Contains(m, "pod_status_phase") || strings.Contains(m, "pod_phase"):
			level := "crit"
			if avg >= 0.9 {
				level = "ok"
			} else if avg >= 0.5 {
				level = "warn"
			}
			chips = append(chips, map[string]any{
				"label": "Pod Phase", "value": fmt.Sprintf("%.2f", avg), "level": level, "icon": "bi-layers",
			})
		case strings.Contains(m, "tasks_state"):
			level := "ok"
			if maxV > 0 {
				level = "crit"
			}
			chips = append(chips, map[string]any{
				"label": "Container Tasks", "value": strconv.Itoa(int(maxV)), "level": level, "icon": "bi-box",
			})
		}
		if len(chips) >= 6 {
			break
		}
	}
	return chips
}

// metricMatchAttempt mirrors one ranked-match attempt in _fetch_trace_metric_context.
type metricMatchAttempt struct {
	mode       string
	label      string
	clauses    []string
	params     []any
	dimensions []any
}

// attrClause mirrors the _attr_clause helper in _fetch_trace_metric_context.
func attrClause(primaryKey, legacyKey string, values []string) (string, []any) {
	if len(values) == 0 {
		return "", nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
	params := make([]any, 0, len(values)*2)
	for _, v := range values {
		params = append(params, v)
	}
	clause := "Attributes['" + primaryKey + "'] IN (" + placeholders + ")"
	if legacyKey != "" && legacyKey != primaryKey {
		clause = "(" + clause + " OR Attributes['" + legacyKey + "'] IN (" + placeholders + "))"
		for _, v := range values {
			params = append(params, v)
		}
	}
	return clause, params
}

// fetchTraceMetricContext mirrors app.py _fetch_trace_metric_context with ranked matching and
// raw/pinned source-mode detection.
func (s *server) fetchTraceMetricContext(serviceNames []string, startTS, endTS string, windowIDs []string,
	limitMetrics int, namespaceValues, podValues, nodeValues, deploymentValues []string) map[string]any {

	startMsNorm := int64(tsStrToEpochMs(startTS))
	endMsNorm := int64(tsStrToEpochMs(endTS))
	var queryStartTS, queryEndTS string
	if endMsNorm > startMsNorm && startMsNorm > 0 {
		queryStartTS = time.UnixMilli(startMsNorm).UTC().Format("2006-01-02 15:04:05.000000")
		queryEndTS = time.UnixMilli(endMsNorm).UTC().Format("2006-01-02 15:04:05.000000")
	} else {
		queryStartTS = startTS
		queryEndTS = endTS
	}

	query := func(extraClauses []string, extraParams []any) map[string]any {
		for _, mode := range []string{"utc", "default"} {
			var startClause, endClause string
			if mode == "utc" {
				startClause = "TimeUnix >= parseDateTime64BestEffort(?, 9, 'UTC')"
				endClause = "TimeUnix <= parseDateTime64BestEffort(?, 9, 'UTC')"
			} else {
				startClause = "TimeUnix >= parseDateTime64BestEffort(?, 9)"
				endClause = "TimeUnix <= parseDateTime64BestEffort(?, 9)"
			}
			whereParts := append([]string{startClause, endClause}, extraClauses...)
			params := append([]any{queryStartTS, queryEndTS}, extraParams...)
			whereSQL := strings.Join(whereParts, " AND ")
			dedupSubquery := "SELECT ServiceName, MetricName, AttrFingerprint, TimeUnix, " +
				"argMin(Value, SourceRank) AS Value, min(SourceRank) AS DedupRank " +
				"FROM v_otel_metrics_dedup WHERE " + whereSQL + " " +
				"GROUP BY ServiceName, MetricName, AttrFingerprint, TimeUnix"

			statsRes, err := s.db.Execute(
				"SELECT count() AS c, min(DedupRank) AS min_rank, max(DedupRank) AS max_rank "+
					"FROM ("+dedupSubquery+") AS dedup", params...)
			if err != nil || len(statsRes.Rows) == 0 {
				continue
			}
			statsRow := rowMaps(statsRes)[0]
			totalPoints := cInt(statsRow, "c")
			if totalPoints <= 0 {
				continue
			}
			minRank := cInt(statsRow, "min_rank")
			maxRank := cInt(statsRow, "max_rank")
			sourceMode := "mixed"
			if minRank == 0 && maxRank == 0 {
				sourceMode = "raw"
			} else if minRank == 1 && maxRank == 1 {
				sourceMode = "pinned"
			}

			limClamp := limitMetrics
			if limClamp < 1 {
				limClamp = 1
			} else if limClamp > 50 {
				limClamp = 50
			}
			rowsRes, rerr := s.db.Execute(
				"SELECT ServiceName, MetricName, count() AS points, "+
					"round(avg(Value), 4) AS avg_value, "+
					"round(min(Value), 4) AS min_value, "+
					"round(max(Value), 4) AS max_value "+
					"FROM ("+dedupSubquery+") AS dedup "+
					"GROUP BY ServiceName, MetricName "+
					"ORDER BY points DESC, MetricName ASC "+
					"LIMIT ?", append(append([]any{}, params...), limClamp)...)
			if rerr != nil {
				continue
			}
			series := []any{}
			for _, r := range rowMaps(rowsRes) {
				series = append(series, map[string]any{
					"service": cStr(r, "ServiceName"),
					"metric":  cStr(r, "MetricName"),
					"points":  cInt(r, "points"),
					"avg":     cFloat(r, "avg_value"),
					"min":     cFloat(r, "min_value"),
					"max":     cFloat(r, "max_value"),
				})
			}
			return map[string]any{
				"source_mode":     sourceMode,
				"total_points":    totalPoints,
				"series":          series,
				"time_parse_mode": mode,
			}
		}
		return map[string]any{
			"source_mode": "none", "total_points": 0, "series": []any{}, "time_parse_mode": "none",
		}
	}

	queryTimeseries := func(extraClauses []string, extraParams []any, topMetricNames []string, timeParseMode string) map[string]any {
		const numBuckets = 24
		if len(topMetricNames) == 0 {
			return map[string]any{"ticks_ms": []any{}, "by_metric": map[string]any{}}
		}
		startMsInt := int64(tsStrToEpochMs(startTS))
		endMsInt := int64(tsStrToEpochMs(endTS))
		if endMsInt <= startMsInt {
			return map[string]any{"ticks_ms": []any{}, "by_metric": map[string]any{}}
		}
		durationMs := endMsInt - startMsInt
		bucketMs := durationMs / numBuckets
		if bucketMs < 1 {
			bucketMs = 1
		}
		ticksMs := make([]any, numBuckets)
		for i := 0; i < numBuckets; i++ {
			ticksMs[i] = int64(float64(startMsInt) + (float64(i)+0.5)*float64(bucketMs))
		}
		metricPhs := strings.TrimSuffix(strings.Repeat("?,", len(topMetricNames)), ",")
		parseModes := []string{"utc", "default"}
		if timeParseMode == "default" {
			parseModes = []string{"default", "utc"}
		}
		byMetric := map[string]any{}
		for _, mn := range topMetricNames {
			arr := make([]any, numBuckets)
			byMetric[mn] = arr
		}
		for _, mode := range parseModes {
			var startClause, endClause string
			if mode == "utc" {
				startClause = "TimeUnix >= parseDateTime64BestEffort(?, 9, 'UTC')"
				endClause = "TimeUnix <= parseDateTime64BestEffort(?, 9, 'UTC')"
			} else {
				startClause = "TimeUnix >= parseDateTime64BestEffort(?, 9)"
				endClause = "TimeUnix <= parseDateTime64BestEffort(?, 9)"
			}
			tsWhereParts := append([]string{startClause, endClause,
				"MetricName IN (" + metricPhs + ")"}, extraClauses...)
			tsWhereSQL := strings.Join(tsWhereParts, " AND ")
			tsParams := append([]any{queryStartTS, queryEndTS}, toAnySlice(topMetricNames)...)
			tsParams = append(tsParams, extraParams...)
			tsDedup := "SELECT MetricName, TimeUnix, argMin(Value, SourceRank) AS Value " +
				"FROM v_otel_metrics_dedup WHERE " + tsWhereSQL + " " +
				"GROUP BY MetricName, TimeUnix, AttrFingerprint"
			tsRes, err := s.db.Execute(
				"SELECT MetricName, "+
					"intDiv(toUnixTimestamp64Milli(TimeUnix) - "+strconv.FormatInt(startMsInt, 10)+", "+strconv.FormatInt(bucketMs, 10)+") AS BucketIdx, "+
					"round(avg(Value), 6) AS AvgVal "+
					"FROM ("+tsDedup+") AS src "+
					"WHERE BucketIdx >= 0 AND BucketIdx < "+strconv.Itoa(numBuckets)+" "+
					"GROUP BY MetricName, BucketIdx "+
					"ORDER BY MetricName, BucketIdx", tsParams...)
			if err != nil {
				continue
			}
			tsRows := rowMaps(tsRes)
			if len(tsRows) == 0 {
				continue
			}
			for _, r := range tsRows {
				mname := cStr(r, "MetricName")
				idx := cInt(r, "BucketIdx")
				if arr, ok := byMetric[mname].([]any); ok && idx >= 0 && idx < numBuckets {
					arr[idx] = cFloat(r, "AvgVal")
				}
			}
			break
		}
		return map[string]any{"ticks_ms": ticksMs, "by_metric": byMetric}
	}

	_ = windowIDs // kept for API compatibility; raw SQL path intentionally ignores this
	traceServices := uniqStrings(serviceNames)
	traceNamespaces := uniqStrings(namespaceValues)
	tracePods := uniqStrings(podValues)
	traceNodes := uniqStrings(nodeValues)
	traceDeployments := uniqStrings(deploymentValues)
	families := serviceFamilies(traceServices)

	nsClause, nsParams := attrClause("k8s.namespace.name", "namespace", traceNamespaces)
	podClause, podParams := attrClause("k8s.pod.name", "pod", tracePods)
	nodeClause, nodeParams := attrClause("k8s.node.name", "node", traceNodes)
	deployClause, deployParams := attrClause("k8s.deployment.name", "deployment", traceDeployments)

	var attempts []metricMatchAttempt
	if nsClause != "" && podClause != "" {
		attempts = append(attempts, metricMatchAttempt{"pod_exact", "pod + namespace",
			[]string{nsClause, podClause}, append(append([]any{}, nsParams...), podParams...),
			[]any{"namespace", "pod"}})
	}
	if nsClause != "" && nodeClause != "" {
		attempts = append(attempts, metricMatchAttempt{"node_namespace", "node + namespace",
			[]string{nsClause, nodeClause}, append(append([]any{}, nsParams...), nodeParams...),
			[]any{"namespace", "node"}})
	}
	if nsClause != "" && deployClause != "" {
		attempts = append(attempts, metricMatchAttempt{"deployment_namespace", "deployment + namespace",
			[]string{nsClause, deployClause}, append(append([]any{}, nsParams...), deployParams...),
			[]any{"namespace", "deployment"}})
	}
	if len(traceServices) > 0 {
		svcPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(traceServices)), ",")
		attempts = append(attempts, metricMatchAttempt{"service_exact", "service exact",
			[]string{"ServiceName IN (" + svcPlaceholders + ")"}, toAnySlice(traceServices),
			[]any{"service"}})
	}
	if len(families) > 0 {
		famPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(families)), ",")
		familyClause := "(ServiceName IN (" + famPlaceholders + ") OR " +
			"Attributes['service.name'] IN (" + famPlaceholders + ") OR " +
			"Attributes['service'] IN (" + famPlaceholders + "))"
		famParams := append(append(append([]any{}, toAnySlice(families)...), toAnySlice(families)...), toAnySlice(families)...)
		attempts = append(attempts, metricMatchAttempt{"service_family", "service family",
			[]string{familyClause}, famParams, []any{"service_family"}})
	}
	attempts = append(attempts, metricMatchAttempt{"time_window_only", "time window only",
		[]string{}, []any{}, []any{"time_window"}})

	for _, attempt := range attempts {
		ctx := query(attempt.clauses, attempt.params)
		if tp, _ := ctx["total_points"].(int); tp > 0 {
			ctx["match_mode"] = attempt.mode
			ctx["match_label"] = attempt.label
			ctx["match_dimensions"] = attempt.dimensions
			rawSeries, _ := ctx["series"].([]any)
			// top_names = first six series metric names
			topNames := []string{}
			for i, sAny := range rawSeries {
				if i >= 6 {
					break
				}
				sm, _ := sAny.(map[string]any)
				topNames = append(topNames, toStr(sm["metric"]))
			}
			// Ensure CPU is included if available.
			cpuMetric := ""
			for _, sAny := range rawSeries {
				sm, _ := sAny.(map[string]any)
				mn := toStr(sm["metric"])
				if strings.Contains(strings.ToLower(mn), "cpu") {
					cpuMetric = mn
					break
				}
			}
			finalTopNames := topNames
			if cpuMetric != "" && !containsStr(topNames, cpuMetric) {
				finalTopNames = append([]string{cpuMetric}, topNames...)
			} else if cpuMetric != "" {
				filtered := []string{}
				for _, m := range topNames {
					if m != cpuMetric {
						filtered = append(filtered, m)
					}
				}
				finalTopNames = append([]string{cpuMetric}, filtered...)
			}
			timeParseMode := "utc"
			if v, ok := ctx["time_parse_mode"].(string); ok && v != "" {
				timeParseMode = v
			}
			ctx["timeseries"] = queryTimeseries(attempt.clauses, attempt.params, finalTopNames, timeParseMode)
			ctx["metric_groups"] = groupMetricSeries(rawSeries)
			healthChips := computeHealthChips(rawSeries)
			ctx["health_chips"] = healthChips
			var headerChip any = nil
			for _, c := range healthChips {
				cm, _ := c.(map[string]any)
				if strings.Contains(toStr(cm["label"]), "CPU") {
					headerChip = c
					break
				}
			}
			ctx["header_chip"] = headerChip
			return ctx
		}
	}

	return map[string]any{
		"source_mode": "none", "total_points": 0, "series": []any{},
		"match_mode": "none", "match_label": "no match", "match_dimensions": []any{},
	}
}

// loadWorkItemLinksForRefIDs mirrors app.py _load_work_item_links_for_ref_ids.
func (s *server) loadWorkItemLinksForRefIDs(refIDs []string) map[string]any {
	refSet := []string{}
	seen := map[string]bool{}
	for _, r := range refIDs {
		if r != "" && !seen[r] {
			refSet = append(refSet, r)
			seen[r] = true
		}
	}
	if len(refSet) == 0 {
		return map[string]any{}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(refSet)), ", ")
	params := make([]any, len(refSet))
	for i, r := range refSet {
		params[i] = r
	}
	res, err := s.db.Execute(
		"SELECT AnomalyRuleId, IssueUrl, CanonicalIssueUrl, IssueNumber, IssueState "+
			"FROM sobs_github_work_items FINAL "+
			"WHERE IsDeleted=0 AND IssueUrl != '' AND AnomalyRuleId IN ("+placeholders+") "+
			"ORDER BY CreatedAt DESC", params...)
	if err != nil {
		return map[string]any{}
	}
	result := map[string]any{}
	for _, m := range rowMaps(res) {
		ref := cStr(m, "AnomalyRuleId")
		if seen[ref] {
			if _, exists := result[ref]; !exists {
				issueURL := cStr(m, "IssueUrl")
				if issueURL == "" {
					issueURL = cStr(m, "CanonicalIssueUrl")
				}
				result[ref] = map[string]any{
					"issue_url":    issueURL,
					"issue_number": cInt(m, "IssueNumber"),
					"issue_state":  cStr(m, "IssueState"),
				}
			}
		}
	}
	return result
}
