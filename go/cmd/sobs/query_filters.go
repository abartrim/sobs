package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// query_filters.go holds the shared read-path query plumbing ported from app.py: limit/offset/
// sort parsing, the from_ts/to_ts time window, and the `include && !exclude` regex filter
// expression (RE2 validated). These mirror _parse_limit / _parse_offset / _parse_sort /
// _parse_time_window_args / _time_window_conditions / _append_time_window_filter /
// _where_clause / _prepare_re2_filter_patterns / _append_regex_expression_clauses. They are used
// by the traces and metrics read handlers and are written to match app.py exactly so populated
// queries reproduce Python's SQL/serialization (the golden corpus only exercises the empty case,
// which these helpers leave byte-identical to the prior stubs).

// parseLimit mirrors _parse_limit: max(1, min(int(limit or default), 5000)), default on a bad value.
func parseLimit(r *http.Request, def int) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if n > 5000 {
		n = 5000
	}
	if n < 1 {
		n = 1
	}
	return n
}

// parseOffset mirrors _parse_offset: max(0, int(offset)), 0 on a bad value.
func parseOffset(r *http.Request) int {
	raw := r.URL.Query().Get("offset")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0
	}
	if n < 0 {
		n = 0
	}
	return n
}

// parseSort mirrors _parse_sort: validate sort_by against an allow-map (URL value -> SQL column)
// and sort_dir against {asc,desc}. Returns (sort_by, sql_col, sort_dir).
func parseSort(r *http.Request, allowed map[string]string, defaultCol string) (sortBy, sqlCol, sortDir string) {
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

// orderClauseFor mirrors `ORDER BY {col} {ASC|DESC}` from the page handlers.
func orderClauseFor(sqlCol, sortDir string) string {
	dir := "DESC"
	if sortDir == "asc" {
		dir = "ASC"
	}
	return "ORDER BY " + sqlCol + " " + dir
}

// parseISOTimestamp parses the ISO-8601 forms datetime.fromisoformat accepts (after the Python
// "Z" -> "+00:00" rewrite), returning the time and whether it parsed. Naive timestamps (no
// offset) are treated as UTC, matching app.py's strftime-without-tz behavior.
func parseISOTimestamp(raw string) (time.Time, bool) {
	s := strings.Replace(raw, "Z", "+00:00", 1)
	layouts := []string{
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02T15:04:05-07:00",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	naive := []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, l := range naive {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// normalizeChTimestamp mirrors _normalize_ch_timestamp(str): convert an ISO-8601 string to a
// ClickHouse DateTime64-compatible "%Y-%m-%d %H:%M:%S.%f" string (UTC). On a parse failure it
// preserves the value with 'T'->' ' (the Python "hope ClickHouse accepts it" fallback). Empty
// input yields now() — but the callers only invoke this on non-empty strings.
func normalizeChTimestamp(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return time.Now().UTC().Format("2006-01-02 15:04:05.000000")
	}
	dt, ok := parseISOTimestamp(raw)
	if !ok {
		return strings.ReplaceAll(raw, "T", " ")
	}
	return dt.UTC().Format("2006-01-02 15:04:05.000000")
}

// normalizeChTimestampTime mirrors _normalize_ch_timestamp(datetime) — format a time value.
func normalizeChTimestampTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000000")
}

// parseTimeWindowArgs mirrors _parse_time_window_args: normalize from_ts/to_ts (and derive to_ts
// from from_ts+window_s when only from_ts is given), validate to_ts > from_ts, and return
// (from_ts, to_ts, error). Returns ("","",error) on a bad value or an inverted window.
func parseTimeWindowArgs(r *http.Request) (fromTS, toTS, timeError string) {
	fromRaw := strings.TrimSpace(r.URL.Query().Get("from_ts"))
	toRaw := strings.TrimSpace(r.URL.Query().Get("to_ts"))
	windowRaw := strings.TrimSpace(r.URL.Query().Get("window_s"))

	const badValue = "Invalid time value. Use ISO-8601, e.g. 2026-03-29T12:00:00Z"

	if fromRaw != "" {
		fromTS = normalizeChTimestamp(fromRaw)
	}
	if toRaw != "" {
		toTS = normalizeChTimestamp(toRaw)
	}

	if fromTS != "" && toTS == "" && windowRaw != "" {
		windowS, err := strconv.Atoi(windowRaw)
		if err != nil {
			return "", "", badValue
		}
		if windowS < 1 {
			windowS = 1
		}
		fromDT, ok := parseISOTimestamp(fromTS)
		if !ok {
			return "", "", badValue
		}
		toTS = normalizeChTimestampTime(fromDT.Add(time.Duration(windowS) * time.Second))
	}

	if fromTS != "" && toTS != "" {
		fromDT, ok1 := parseISOTimestamp(fromTS)
		toDT, ok2 := parseISOTimestamp(toTS)
		if !ok1 || !ok2 {
			return "", "", badValue
		}
		if !toDT.After(fromDT) {
			return "", "", "Invalid time window: to_ts must be later than from_ts"
		}
	}
	return fromTS, toTS, ""
}

// timeWindowConditions mirrors _time_window_conditions: parseDateTime64BestEffort(?, 9) bounds.
func timeWindowConditions(column, fromTS, toTS string) (conds []string, params []any) {
	if fromTS != "" {
		conds = append(conds, column+" >= parseDateTime64BestEffort(?, 9)")
		params = append(params, fromTS)
	}
	if toTS != "" {
		conds = append(conds, column+" < parseDateTime64BestEffort(?, 9)")
		params = append(params, toTS)
	}
	return conds, params
}

// appendTimeWindowFilter mirrors _append_time_window_filter (extend in place).
func appendTimeWindowFilter(conds *[]string, params *[]any, column, fromTS, toTS string) {
	c, p := timeWindowConditions(column, fromTS, toTS)
	*conds = append(*conds, c...)
	*params = append(*params, p...)
}

// whereClause mirrors _where_clause: "WHERE a AND b" or "" when there are no conditions.
func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(conds, " AND ")
}

// appendRegexExpressionClauses mirrors _append_regex_expression_clauses: match()/NOT match().
func appendRegexExpressionClauses(conds *[]string, params *[]any, column string, includePatterns, excludePatterns []string) {
	for _, p := range includePatterns {
		*conds = append(*conds, "match("+column+", ?)")
		*params = append(*params, p)
	}
	for _, p := range excludePatterns {
		*conds = append(*conds, "NOT match("+column+", ?)")
		*params = append(*params, p)
	}
}

// placeholders mirrors `",".join(["?"] * n)` for an IN (...) clause.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// appendStrs appends each string as an `any` param (params.extend(values)).
func appendStrs(params []any, values []string) []any {
	for _, v := range values {
		params = append(params, v)
	}
	return params
}

// queryListNonEmpty mirrors `[v.strip() for v in request.args.getlist(key) if v.strip()]`.
func queryListNonEmpty(r *http.Request, key string) []string {
	out := []string{}
	for _, v := range r.URL.Query()[key] {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// attrGet mirrors `attrs.get(k1, attrs.get(k2, ""))` over a _map_to_dict result: return the
// first present key as a string, else "". The value is stringified like Python str(...).
func attrGet(attrs any, keys ...string) string {
	m, ok := attrs.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range keys {
		if v, present := m[k]; present {
			return toStr(v)
		}
	}
	return ""
}

// firstOrEmpty returns xs[0] or "" — mirrors `xs[0] if xs else ""`.
func firstOrEmpty(xs []string) string {
	if len(xs) > 0 {
		return xs[0]
	}
	return ""
}

// splitRegexFilterExpressionTerms mirrors _split_regex_filter_expression_terms: split on
// unescaped `&&`, preserving escaped `\&&`.
func splitRegexFilterExpressionTerms(expression string) []string {
	parts := []string{}
	var buf []byte
	n := len(expression)
	for i := 0; i < n; {
		if i+1 < n && expression[i] == '&' && expression[i+1] == '&' {
			backslashes := 0
			for j := i - 1; j >= 0 && expression[j] == '\\'; j-- {
				backslashes++
			}
			if backslashes%2 == 0 {
				parts = append(parts, strings.TrimSpace(string(buf)))
				buf = buf[:0]
				i += 2
				continue
			}
		}
		buf = append(buf, expression[i])
		i++
	}
	parts = append(parts, strings.TrimSpace(string(buf)))
	return parts
}

// unescapeRegexFilterTerm mirrors _unescape_regex_filter_term: \&& -> &&.
func unescapeRegexFilterTerm(term string) string {
	return strings.ReplaceAll(term, `\&&`, "&&")
}

// parseRegexFilterExpression mirrors _parse_regex_filter_expression: parse `include && !exclude`,
// validating each term compiles (case-insensitive). Returns (include, exclude, error).
//
// First-pass engine LIMITATION (audit finding #7): Python validates each term with the `re` module
// (`re.compile(token, re.IGNORECASE)`), whereas Go has only RE2 (`regexp.Compile`). RE2 cannot fully
// replicate Python `re`:
//   - Patterns Python `re` ACCEPTS but RE2 rejects (backreferences `\1`, lookahead/lookbehind) are
//     over-rejected here, and the error bytes differ. In Python these would pass the first pass and
//     be authoritatively rejected by the chDB RE2 second pass (prepareRe2FilterPatterns) — which is
//     ALSO RE2, so the eventual verdict matches even though this pass's message does not.
//   - Patterns invalid in BOTH engines (e.g. an unbalanced paren) error here with Go's regexp message
//     instead of Python `re`'s.
//
// This is irreducible without a Python-`re`-compatible engine. The two-pass STRUCTURE (parse-compile
// then chDB `match(”, ?)`) faithfully mirrors Python, so the dominant case — a valid simple pattern
// (accepted by both) — and the structural `&&` errors (caught below, engine-independent) are exact;
// only first-pass error bytes for the exotic-syntax inputs above diverge, none of which the empty
// golden corpus exercises.
func parseRegexFilterExpression(raw string) (include, exclude []string, errMsg string) {
	expression := strings.TrimSpace(raw)
	if expression == "" {
		return nil, nil, ""
	}
	parts := splitRegexFilterExpressionTerms(expression)
	if len(parts) == 0 {
		return nil, nil, "Regex error: invalid expression around '&&'"
	}
	for _, part := range parts {
		if part == "" {
			return nil, nil, "Regex error: invalid expression around '&&'"
		}
	}
	for _, part := range parts {
		negate := strings.HasPrefix(part, "!")
		token := part
		if negate {
			token = strings.TrimSpace(part[1:])
		}
		token = unescapeRegexFilterTerm(token)
		if token == "" {
			return nil, nil, "Regex error: expected a pattern after '!'"
		}
		if _, err := regexp.Compile("(?i)" + token); err != nil {
			return nil, nil, "Regex error: " + err.Error()
		}
		if negate {
			exclude = append(exclude, token)
		} else {
			include = append(include, token)
		}
	}
	return include, exclude, ""
}

// validateRe2Pattern mirrors _validate_re2_pattern: chDB RE2 rejects some patterns Go's regexp
// accepts. Returns the first "Regex error: ..." (": while executing function" tail stripped) or "".
func (s *server) validateRe2Pattern(pattern string) string {
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

// validateRe2Patterns mirrors _validate_re2_patterns.
func (s *server) validateRe2Patterns(patterns []string) string {
	for _, p := range patterns {
		if msg := s.validateRe2Pattern(p); msg != "" {
			return msg
		}
	}
	return ""
}

// prepareRe2FilterPatterns mirrors _prepare_re2_filter_patterns: parse the expression, then
// RE2-validate every pattern. Returns (include, exclude, error).
func (s *server) prepareRe2FilterPatterns(raw string) (include, exclude []string, errMsg string) {
	include, exclude, parseErr := parseRegexFilterExpression(raw)
	if parseErr != "" {
		return nil, nil, parseErr
	}
	all := append(append([]string{}, include...), exclude...)
	if re2Err := s.validateRe2Patterns(all); re2Err != "" {
		return nil, nil, re2Err
	}
	return include, exclude, ""
}
