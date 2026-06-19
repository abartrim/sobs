package main

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---------------------------------------------------------------------------
// Shared dashboard query helpers (ports of app.py filter/sort/window helpers).
// These mirror the Python functions byte-for-byte in behaviour so the SQL, and
// thus the result set and template output, are identical.
// ---------------------------------------------------------------------------

// queryGetList mirrors Quart's request.args.getlist(key): every value supplied for
// the repeated query param, in order.
func queryGetList(r *http.Request, key string) []string {
	return r.URL.Query()[key]
}

// pyISOFormatUTC mirrors datetime.isoformat() for a UTC-aware datetime: microseconds are
// emitted only when non-zero, and the offset renders as "+00:00".
func pyISOFormatUTC(dt time.Time) string {
	dt = dt.UTC()
	base := dt.Format("2006-01-02T15:04:05")
	if us := dt.Nanosecond() / 1000; us != 0 {
		base += "." + leftPad6(us)
	}
	return base + "+00:00"
}

// leftPad6 renders a microsecond value as a zero-padded 6-digit string.
func leftPad6(us int) string {
	s := strconv.Itoa(us)
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}

// parseLimitArg mirrors _parse_limit(default): clamp int(request.args["limit"]) to
// [1, 5000], falling back to default on a bad value.
func parseLimitArg(r *http.Request, def int) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return clampInt(def, 1, 5000)
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	return clampInt(n, 1, 5000)
}

// parseOffsetArg mirrors _parse_offset(): max(0, int(request.args["offset"])), 0 on error.
func parseOffsetArg(r *http.Request) int {
	raw := r.URL.Query().Get("offset")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	if n < 0 {
		return 0
	}
	return n
}

// parseSortArg mirrors _parse_sort(allowed, default_col): returns (sort_by, sql_col,
// sort_dir). allowed maps the URL-key → SQL-col; lookups are by key.
func parseSortArg(r *http.Request, allowed map[string]string, defaultCol string) (sortBy, sqlCol, sortDir string) {
	sortBy = r.URL.Query().Get("sort_by")
	if sortBy == "" {
		sortBy = defaultCol
	}
	sortDir = strings.ToLower(r.URL.Query().Get("sort_dir"))
	if sortDir == "" {
		sortDir = "desc"
	}
	if _, ok := allowed[sortBy]; !ok {
		sortBy = defaultCol
	}
	if sortDir != "asc" && sortDir != "desc" {
		sortDir = "desc"
	}
	return sortBy, allowed[sortBy], sortDir
}

// parseISOTime mirrors datetime.fromisoformat. Returns the parsed time (offset-aware values
// retain their offset; naive values are treated as the local zero offset, fine for the
// strictly-later comparison) and ok.
func parseISOTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	candidates := []string{
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04-07:00",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range candidates {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// whereClauseSQL mirrors _where_clause(conditions).
func whereClauseSQL(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(conds, " AND ")
}

// parseTraceFilterValues mirrors _parse_trace_filter_values(trace_id, trace_ids).
func parseTraceFilterValues(traceID string, rawTraceIDs []string) (parsed []string, primary string) {
	iterParts := func(value string) []string {
		var out []string
		for _, p := range strings.Split(value, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	contains := func(s []string, v string) bool {
		for _, x := range s {
			if x == v {
				return true
			}
		}
		return false
	}
	for _, rawValue := range rawTraceIDs {
		for _, part := range iterParts(rawValue) {
			norm := strings.ToLower(part)
			if norm != "" && !contains(parsed, norm) {
				parsed = append(parsed, norm)
			}
		}
	}
	for _, part := range iterParts(traceID) {
		norm := strings.ToLower(part)
		if norm != "" && !contains(parsed, norm) {
			parsed = append([]string{norm}, parsed...)
		}
	}
	if len(parsed) > 0 {
		primary = parsed[0]
	}
	return parsed, primary
}

// recordIDForLog mirrors _record_id_for_log(ts, service, trace_id, span_id).
func recordIDForLog(ts, service, traceID, spanID string) string {
	key := service + "|" + ts + "|" + traceID + "|" + spanID
	sum := md5.Sum([]byte(key))
	return hex.EncodeToString(sum[:])
}

// errorIDFor mirrors _error_id(ts, service, err_type, message, trace_id, span_id).
func errorIDFor(ts, service, errType, message, traceID, spanID string) string {
	raw := strings.Join([]string{ts, service, errType, message, traceID, spanID}, "|")
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// RE2 regex-filter helpers (port of the _*_regex_filter_* family).
// chDB's match() uses RE2; Go's regexp is also RE2, so validation matches.
// The pure-parse helpers (splitRegexFilterExpressionTerms / parseRegexFilterExpression /
// appendRegexExpressionClauses) live once in query_filters.go; only the DB-probing methods
// (which the page handlers call as s.prepareRE2FilterPatterns) are kept here.
// ---------------------------------------------------------------------------

// validateRE2Pattern mirrors _validate_re2_pattern: probe `SELECT match(”, ?)` against chDB.
// Returns "" when valid, else a "Regex error: ..." message.
func (s *server) validateRE2Pattern(pattern string) string {
	value := strings.TrimSpace(pattern)
	if value == "" {
		return ""
	}
	if _, err := s.db.Execute("SELECT match('', ?)", value); err != nil {
		msg := strings.TrimSpace(err.Error())
		if idx := strings.Index(msg, ": while executing function"); idx >= 0 {
			msg = strings.TrimSpace(msg[:idx])
		}
		return "Regex error: " + msg
	}
	return ""
}

// prepareRE2FilterPatterns mirrors _prepare_re2_filter_patterns.
func (s *server) prepareRE2FilterPatterns(raw string) (include, exclude []string, errMsg string) {
	include, exclude, parseErr := parseRegexFilterExpression(raw)
	if parseErr != "" {
		return nil, nil, parseErr
	}
	all := append(append([]string{}, include...), exclude...)
	for _, pat := range all {
		if re2Err := s.validateRE2Pattern(pat); re2Err != "" {
			return nil, nil, re2Err
		}
	}
	return include, exclude, ""
}

// ---------------------------------------------------------------------------
// User SQL WHERE token substitution (logs SQL-search path). The unsafe-keyword guard
// (validateUserSQLWhere + unsafeWherePatterns) lives once in ai_view.go.
// ---------------------------------------------------------------------------

var (
	reWordLevel   = regexp.MustCompile(`(?i)\blevel\b`)
	reWordService = regexp.MustCompile(`(?i)\bservice\b`)
	reWordTraceID = regexp.MustCompile(`(?i)\btrace_id\b`)
	reWordSpanID  = regexp.MustCompile(`(?i)\bspan_id\b`)
	reWordTs      = regexp.MustCompile(`(?i)\bts\b`)
	reWordBody    = regexp.MustCompile(`(?i)\bbody\b`)
	reHasTag      = regexp.MustCompile(`(?i)has_tag\s*\(\s*'((?:[^']|'')+)'\s*,\s*'((?:[^']|'')*)'\s*\)`)
)

// translateLogsSQLWhere mirrors the logs SQL-search token substitution + has_tag() rewrite.
func translateLogsSQLWhere(sqlWhere string) string {
	safe := strings.ReplaceAll(sqlWhere, ";", "")
	safe = reWordLevel.ReplaceAllString(safe, "SeverityText")
	safe = reWordService.ReplaceAllString(safe, "ServiceName")
	safe = reWordTraceID.ReplaceAllString(safe, "TraceId")
	safe = reWordSpanID.ReplaceAllString(safe, "SpanId")
	safe = reWordTs.ReplaceAllString(safe, "Timestamp")
	safe = reWordBody.ReplaceAllString(safe, "Body")
	safe = reHasTag.ReplaceAllStringFunc(safe, func(m string) string {
		groups := reHasTag.FindStringSubmatch(m)
		tagKey := sqlEscapeSingle(groups[1])
		tagVal := sqlEscapeSingle(groups[2])
		return "MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)) IN (" +
			"SELECT RecordId FROM sobs_record_tags FINAL " +
			"WHERE TagKey='" + tagKey + "' AND TagValue='" + tagVal + "' " +
			"AND IsDeleted=0 AND RecordType='log')"
	})
	return safe
}

// sqlEscapeSingle mirrors `.replace("”", "'").replace("'", "”")` for has_tag args.
func sqlEscapeSingle(s string) string {
	s = strings.ReplaceAll(s, "''", "'")
	return strings.ReplaceAll(s, "'", "''")
}

// ---------------------------------------------------------------------------
// Log stats + advanced analysis (ports of _compute_log_stats / _compute_advanced_log_analysis).
// ---------------------------------------------------------------------------

// computeLogStats mirrors _compute_log_stats: returns ordered (level_stats, service_stats).
func (s *server) computeLogStats(where string, params []any) (*jsonenc.Object, *jsonenc.Object) {
	levelStats := jsonenc.NewObject()
	levelQuery := "SELECT SeverityText, COUNT(*) AS cnt FROM otel_logs " + where +
		" GROUP BY SeverityText ORDER BY cnt DESC"
	if res, err := s.db.Execute(levelQuery, params...); err == nil {
		for _, m := range rowMaps(res) {
			key := cStr(m, "SeverityText")
			if key == "" {
				key = "UNKNOWN"
			}
			// COUNT() arrives as a float64 here; Python keeps it an int, so the template
			// renders "3" not "3.0". Coerce to int to match.
			levelStats.Set(key, cInt(m, "cnt"))
		}
	}

	serviceStats := jsonenc.NewObject()
	svcCond := "WHERE ServiceName!=''"
	if where != "" {
		svcCond = "AND ServiceName!=''"
	}
	serviceQuery := "SELECT ServiceName, COUNT(*) AS cnt FROM otel_logs " + where + " " + svcCond +
		" GROUP BY ServiceName ORDER BY cnt DESC LIMIT 10"
	if res, err := s.db.Execute(serviceQuery, params...); err == nil {
		for _, m := range rowMaps(res) {
			serviceStats.Set(cStr(m, "ServiceName"), cInt(m, "cnt"))
		}
	}
	return levelStats, serviceStats
}

var (
	reFpUUID   = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reFpHex0x  = regexp.MustCompile(`\b0x[0-9a-f]+\b`)
	reFpHash   = regexp.MustCompile(`\b[0-9a-f]{16,}\b`)
	reFpNum4   = regexp.MustCompile(`\b\d{4,}\b`)
	reFpNum    = regexp.MustCompile(`\b\d+\b`)
	reFpSingle = regexp.MustCompile(`'[^']*'`)
	reFpDouble = regexp.MustCompile(`"[^"]*"`)
	reFpSpace  = regexp.MustCompile(`\s+`)
	reFamily   = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*(?:Error|Exception|Timeout|Refused|Unavailable|Failure))\b`)
	reKeyword  = regexp.MustCompile(`[a-z][a-z0-9_\-]{2,}`)
)

// fingerprintLogMessage mirrors _fingerprint_log_message.
func fingerprintLogMessage(message string) string {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return "(empty message)"
	}
	normalized = reFpUUID.ReplaceAllString(normalized, "<uuid>")
	normalized = reFpHex0x.ReplaceAllString(normalized, "<hex>")
	normalized = reFpHash.ReplaceAllString(normalized, "<hash>")
	normalized = reFpNum4.ReplaceAllString(normalized, "<num>")
	normalized = reFpNum.ReplaceAllString(normalized, "<n>")
	normalized = reFpSingle.ReplaceAllString(normalized, "'<text>'")
	normalized = reFpDouble.ReplaceAllString(normalized, `"<text>"`)
	normalized = strings.TrimSpace(reFpSpace.ReplaceAllString(normalized, " "))
	if len(normalized) > 160 {
		normalized = normalized[:160]
	}
	return normalized
}

// counterEntry tracks a counted token with first-seen order, mirroring collections.Counter.
type counterEntry struct {
	key   string
	count int
}

// orderedCounter mimics collections.Counter: increments preserve first-seen order, and
// most_common(n) sorts by count desc, ties broken by first-seen (Python 3.7+ Counter).
type orderedCounter struct {
	idx     map[string]int
	entries []counterEntry
}

func newOrderedCounter() *orderedCounter {
	return &orderedCounter{idx: map[string]int{}}
}

func (c *orderedCounter) add(key string, n int) {
	if i, ok := c.idx[key]; ok {
		c.entries[i].count += n
		return
	}
	c.idx[key] = len(c.entries)
	c.entries = append(c.entries, counterEntry{key: key, count: n})
}

func (c *orderedCounter) get(key string) int {
	if i, ok := c.idx[key]; ok {
		return c.entries[i].count
	}
	return 0
}

func (c *orderedCounter) mostCommon(n int) []counterEntry {
	sorted := make([]counterEntry, len(c.entries))
	copy(sorted, c.entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].count > sorted[j].count
	})
	if n >= 0 && n < len(sorted) {
		sorted = sorted[:n]
	}
	return sorted
}

// computeAdvancedLogAnalysis mirrors _compute_advanced_log_analysis. analysisRows are the raw
// SELECT SeverityText, ServiceName, Body, LogAttributes rows; levelStats/serviceStats are the
// ordered stats objects.
func computeAdvancedLogAnalysis(analysisRows []map[string]any, levelStats, serviceStats *jsonenc.Object) *jsonenc.Object {
	var messages []string
	for _, row := range analysisRows {
		body := cStr(row, "Body")
		if body != "" {
			messages = append(messages, body)
		}
	}
	if len(messages) == 0 {
		return jsonenc.NewObject().
			Set("top_patterns", []any{}).
			Set("top_keywords", []any{}).
			Set("error_families", []any{}).
			Set("hints", []any{})
	}

	fingerprintCounts := newOrderedCounter()
	for _, msg := range messages {
		fingerprintCounts.add(fingerprintLogMessage(msg), 1)
	}
	mostCommonPatterns := fingerprintCounts.mostCommon(8)
	topPatterns := []any{}
	for _, e := range mostCommonPatterns {
		topPatterns = append(topPatterns, jsonenc.NewObject().Set("pattern", e.key).Set("count", e.count))
	}

	familyCounts := newOrderedCounter()
	// Prefer structured exception types, then fall back to message parsing.
	for _, row := range analysisRows {
		attrs := mapToDictStr(row["LogAttributes"])
		excType := strings.TrimSpace(attrs["exception.type"])
		if excType != "" {
			familyCounts.add(excType, 1)
		}
	}
	for _, msg := range messages {
		// Python iterates set(findall(...)) — set order is arbitrary but counts are
		// order-insensitive and most_common ties fall back to first-seen, so add unique
		// families in first-occurrence order within the message.
		added := map[string]bool{}
		for _, m := range reFamily.FindAllStringSubmatch(msg, -1) {
			fam := m[1]
			if added[fam] {
				continue
			}
			added[fam] = true
			familyCounts.add(fam, 1)
		}
	}
	errorFamilies := []any{}
	for _, e := range familyCounts.mostCommon(8) {
		errorFamilies = append(errorFamilies, jsonenc.NewObject().Set("family", e.key).Set("count", e.count))
	}

	stopWords := map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "from": true, "into": true,
		"this": true, "that": true, "http": true, "https": true, "failed": true,
		"error": true, "warn": true, "info": true, "debug": true, "trace": true, "service": true,
	}
	keywordCounts := newOrderedCounter()
	for _, msg := range messages {
		for _, token := range reKeyword.FindAllString(strings.ToLower(msg), -1) {
			if !stopWords[token] {
				keywordCounts.add(token, 1)
			}
		}
	}
	topKeywords := []any{}
	for _, e := range keywordCounts.mostCommon(10) {
		topKeywords = append(topKeywords, jsonenc.NewObject().Set("keyword", e.key).Set("count", e.count))
	}

	hints := []any{}
	total := len(analysisRows)
	if total < 1 {
		total = 1
	}
	severe := 0
	severeSet := map[string]bool{"ERROR": true, "FATAL": true, "CRITICAL": true, "ALERT": true, "EMERGENCY": true}
	for _, level := range levelStats.Keys() {
		if severeSet[strings.ToUpper(level)] {
			v, _ := levelStats.Get(level)
			severe += toIntVal(v)
		}
	}
	severeRatio := float64(severe) / float64(total)
	if severeRatio >= 0.25 {
		hints = append(hints, "High severe-log ratio ("+pyPercent0(severeRatio)+
			"); prioritize stabilizing error paths before scaling traffic.")
	}
	if len(mostCommonPatterns) > 0 && mostCommonPatterns[0].count >= 3 {
		topCount := mostCommonPatterns[0].count
		hints = append(hints, "Most frequent message pattern repeats "+strconv.Itoa(topCount)+
			" times; consider deduplication/sampling and shared remediation guidance.")
	}
	timeoutHits := keywordCounts.get("timeout") + keywordCounts.get("timed")
	if timeoutHits >= 3 {
		hints = append(hints, "Timeout-related logs are common; review dependency latency, retry budgets, and circuit breakers.")
	}
	if serviceStats.Len() > 0 {
		topService := serviceStats.Keys()[0]
		v, _ := serviceStats.Get(topService)
		topServiceCount := toIntVal(v)
		if float64(topServiceCount)/float64(total) >= 0.6 {
			hints = append(hints, "Most events come from "+topService+
				"; investigate service-level hotspots and noisy call paths.")
		}
	}

	return jsonenc.NewObject().
		Set("top_patterns", topPatterns).
		Set("top_keywords", topKeywords).
		Set("error_families", errorFamilies).
		Set("hints", hints)
}

// pyPercent0 mirrors Python's f"{ratio:.0%}": multiply by 100, round-half-even, append "%".
func pyPercent0(ratio float64) string {
	return strconv.Itoa(int(pyRoundHalfEven(ratio*100))) + "%"
}

// pyRoundHalfEven mirrors Python's banker's rounding to the nearest integer.
func pyRoundHalfEven(v float64) float64 {
	floor := float64(int64(v))
	diff := v - floor
	if diff < 0.5 {
		return floor
	}
	if diff > 0.5 {
		return floor + 1
	}
	if int64(floor)%2 == 0 {
		return floor
	}
	return floor + 1
}

func toIntVal(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return int(n)
	default:
		return 0
	}
}

// mapToDictStr mirrors _map_to_dict for a chDB Map column, returning a string→string view of
// scalar entries (the only access pattern the error/log helpers use).
func mapToDictStr(v any) map[string]string {
	out := map[string]string{}
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			out[k] = scalarStr(val)
		}
	case *jsonenc.Object:
		for _, k := range x.Keys() {
			val, _ := x.Get(k)
			out[k] = scalarStr(val)
		}
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return out
		}
		if parsed, err := parseJSONValue([]byte(s)); err == nil {
			if obj, ok := parsed.(*jsonenc.Object); ok {
				for _, k := range obj.Keys() {
					val, _ := obj.Get(k)
					out[k] = scalarStr(val)
				}
			}
		}
	}
	return out
}

func scalarStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case bool:
		if x {
			return "True"
		}
		return "False"
	default:
		return strings.Trim(string(jsonenc.Encode(v, jsonenc.Options{EnsureASCII: false})), `"`)
	}
}
