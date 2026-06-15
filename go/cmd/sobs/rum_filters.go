package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This file ports the request-param plumbing view_rum (app.py:17307) shares with the other
// data dashboards: limit/offset/sort parsing, the from_ts/to_ts time window, and the
// `include && !exclude` RE2 regex filter. The DEFAULT (no-param) /rum request — the only
// shape the parity corpus exercises (case get__rum sends a bare GET) — drives every parser
// to its empty/default result, so the WHERE clause is "" and the queries stay byte-identical
// to the prior hardcoded handler. The branches below only ever fire for param-bearing
// requests the golden never sends.

// parseLimitDefault mirrors app.py _parse_limit(default): clamp(1, int(?limit), 5000) with a
// try/except fallback to default on a bad value.
func parseLimitDefault(r *http.Request, def int) int {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return def
	}
	if n < 1 {
		n = 1
	}
	if n > 5000 {
		n = 5000
	}
	return n
}

// parseOffset mirrors app.py _parse_offset: max(0, int(?offset)) with a fallback to 0.
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
		return 0
	}
	return n
}

// sortOption pairs a URL param value with its SQL column. A slice (not a map) preserves the
// allowed-key set without affecting behavior — only membership + the mapped column matter.
type sortOption struct{ key, col string }

func lookupSort(allowed []sortOption, key string) (string, bool) {
	for _, o := range allowed {
		if o.key == key {
			return o.col, true
		}
	}
	return "", false
}

// parseSort mirrors app.py _parse_sort(allowed, default_col): validate ?sort_by against the
// allowed map (falling back to defaultBy) and ?sort_dir to asc/desc (default desc). Returns
// (sortBy, sqlCol, sortDir).
func parseSort(r *http.Request, allowed []sortOption, defaultBy string) (sortBy, sqlCol, sortDir string) {
	sortBy = r.URL.Query().Get("sort_by")
	if sortBy == "" {
		sortBy = defaultBy
	}
	sortDir = strings.ToLower(r.URL.Query().Get("sort_dir"))
	if sortDir == "" {
		sortDir = "desc"
	}
	col, ok := lookupSort(allowed, sortBy)
	if !ok {
		sortBy = defaultBy
		col, _ = lookupSort(allowed, defaultBy)
	}
	if sortDir != "asc" && sortDir != "desc" {
		sortDir = "desc"
	}
	return sortBy, col, sortDir
}

// orderDir mirrors `'ASC' if sort_dir == 'asc' else 'DESC'`.
func orderDir(sortDir string) string {
	if sortDir == "asc" {
		return "ASC"
	}
	return "DESC"
}

// parseTimeWindowArgs mirrors app.py _parse_time_window_args: normalize from_ts/to_ts (and
// optional window_s) to ClickHouse DateTime64 strings, returning ("", "", error) on a bad
// value or a non-increasing window. Empty inputs yield ("", "", "").
func parseTimeWindowArgs(r *http.Request) (fromTS, toTS, errMsg string) {
	const valueErr = "Invalid time value. Use ISO-8601, e.g. 2026-03-29T12:00:00Z"
	q := r.URL.Query()
	fromRaw := strings.TrimSpace(q.Get("from_ts"))
	toRaw := strings.TrimSpace(q.Get("to_ts"))
	windowRaw := strings.TrimSpace(q.Get("window_s"))

	if fromRaw != "" {
		fromTS = normalizeCHTimestamp(fromRaw)
	}
	if toRaw != "" {
		toTS = normalizeCHTimestamp(toRaw)
	}
	if fromTS != "" && toTS == "" && windowRaw != "" {
		ws, err := strconv.Atoi(windowRaw)
		if err != nil {
			return "", "", valueErr
		}
		if ws < 1 {
			ws = 1
		}
		fromDT, ok := parseCHTimestamp(fromTS)
		if !ok {
			return "", "", valueErr
		}
		toTS = fromDT.Add(time.Duration(ws) * time.Second).UTC().Format("2006-01-02 15:04:05.000000")
	}
	if fromTS != "" && toTS != "" {
		fromDT, fok := parseCHTimestamp(fromTS)
		toDT, tok := parseCHTimestamp(toTS)
		if !fok || !tok {
			return "", "", valueErr
		}
		if !toDT.After(fromDT) {
			return "", "", "Invalid time window: to_ts must be later than from_ts"
		}
	}
	return fromTS, toTS, ""
}

// parseCHTimestamp parses a "2006-01-02 15:04:05.000000" (or .Z / ISO) value back to a time,
// mirroring datetime.fromisoformat used in _parse_time_window_args's window-arithmetic and
// ordering checks.
func parseCHTimestamp(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	parse := strings.Replace(s, "Z", "+00:00", 1)
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, parse); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// timeWindowConditions mirrors app.py _time_window_conditions: build the parseDateTime64BestEffort
// fragments for a from/to window on a DateTime64 column.
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

// whereClause mirrors app.py _where_clause: "WHERE a AND b" or "" when empty.
func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(conds, " AND ")
}

// regexFilter holds the parsed include/exclude pattern lists from a `q` expression.
type regexFilter struct {
	include []string
	exclude []string
}

// prepareRE2FilterPatterns mirrors app.py _prepare_re2_filter_patterns: parse the
// `include && !exclude` expression, then RE2-validate every pattern via the DB. Returns the
// pattern lists and a non-empty error string on a parse/RE2 failure.
func (s *server) prepareRE2FilterPatterns(raw string) (regexFilter, string) {
	rf, parseErr := parseRegexFilterExpression(raw)
	if parseErr != "" {
		return regexFilter{}, parseErr
	}
	patterns := append(append([]string{}, rf.include...), rf.exclude...)
	for _, p := range patterns {
		if e := s.validateRE2Pattern(p); e != "" {
			return regexFilter{}, e
		}
	}
	return rf, ""
}

// parseRegexFilterExpression mirrors app.py _parse_regex_filter_expression: split on
// unescaped `&&`, treat a leading `!` as exclude, unescape `\&&`, and compile each pattern
// (Go's regexp is RE2, the same engine ClickHouse match() uses) to surface a syntax error.
func parseRegexFilterExpression(raw string) (regexFilter, string) {
	expr := strings.TrimSpace(raw)
	if expr == "" {
		return regexFilter{}, ""
	}
	parts := splitRegexFilterExpressionTerms(expr)
	if len(parts) == 0 {
		return regexFilter{}, "Regex error: invalid expression around '&&'"
	}
	for _, p := range parts {
		if p == "" {
			return regexFilter{}, "Regex error: invalid expression around '&&'"
		}
	}
	var rf regexFilter
	for _, part := range parts {
		negate := strings.HasPrefix(part, "!")
		token := part
		if negate {
			token = strings.TrimSpace(part[1:])
		}
		token = unescapeRegexFilterTerm(token)
		if token == "" {
			return regexFilter{}, "Regex error: expected a pattern after '!'"
		}
		if msg := compileRE2Surface(token); msg != "" {
			return regexFilter{}, msg
		}
		if negate {
			rf.exclude = append(rf.exclude, token)
		} else {
			rf.include = append(rf.include, token)
		}
	}
	return rf, ""
}

// splitRegexFilterExpressionTerms mirrors app.py _split_regex_filter_expression_terms: split
// on `&&` only when an even number of backslashes precedes it (so `\&&` stays literal).
func splitRegexFilterExpressionTerms(expression string) []string {
	var parts []string
	var buf strings.Builder
	n := len(expression)
	for i := 0; i < n; i++ {
		if i+1 < n && expression[i] == '&' && expression[i+1] == '&' {
			backslashes := 0
			for j := i - 1; j >= 0 && expression[j] == '\\'; j-- {
				backslashes++
			}
			if backslashes%2 == 0 {
				parts = append(parts, strings.TrimSpace(buf.String()))
				buf.Reset()
				i++ // skip the second '&'; loop's i++ skips the first
				continue
			}
		}
		buf.WriteByte(expression[i])
	}
	parts = append(parts, strings.TrimSpace(buf.String()))
	return parts
}

// unescapeRegexFilterTerm mirrors app.py _unescape_regex_filter_term: `\&&` -> `&&`.
func unescapeRegexFilterTerm(term string) string {
	return strings.ReplaceAll(term, `\&&`, "&&")
}

// compileRE2Surface mirrors app.py's `re.compile(token, re.IGNORECASE)` syntax check that
// precedes the DB-side RE2 validation: it returns a "Regex error: ..." message when the
// pattern fails to compile, else "". Python uses the stdlib `re` engine here (Go's regexp is
// RE2 — the same engine ClickHouse uses), so the exact error TEXT for a malformed pattern can
// differ; the boundary it guards (only param-bearing /rum requests with a `q=`) is never hit
// by the parity corpus, so this only governs real-use error reporting, not golden bytes.
func compileRE2Surface(token string) string {
	if _, err := regexp.Compile("(?i)" + token); err != nil {
		return "Regex error: " + err.Error()
	}
	return ""
}

// validateRE2Pattern mirrors app.py _validate_re2_pattern: a blank pattern is fine; otherwise
// probe ClickHouse's RE2 via `SELECT match(”, ?)`, trimming the "...: while executing
// function" tail off any error the same way Python does.
func (s *server) validateRE2Pattern(pattern string) string {
	value := strings.TrimSpace(pattern)
	if value == "" {
		return ""
	}
	_, err := s.db.Execute("SELECT match('', ?)", value)
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if idx := strings.Index(msg, ": while executing function"); idx >= 0 {
		msg = strings.TrimSpace(msg[:idx])
	}
	return "Regex error: " + msg
}

// appendRegexExpressionClauses mirrors app.py _append_regex_expression_clauses: one
// match(col, ?) per include pattern and one NOT match(col, ?) per exclude pattern.
func appendRegexExpressionClauses(conds []string, params []any, column string, rf regexFilter) ([]string, []any) {
	for _, p := range rf.include {
		conds = append(conds, "match("+column+", ?)")
		params = append(params, p)
	}
	for _, p := range rf.exclude {
		conds = append(conds, "NOT match("+column+", ?)")
		params = append(params, p)
	}
	return conds, params
}
