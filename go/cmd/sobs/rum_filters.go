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

// parseSortOptions mirrors app.py _parse_sort(allowed, default_col): validate ?sort_by against
// the allowed map (falling back to defaultBy) and ?sort_dir to asc/desc (default desc). Returns
// (sortBy, sqlCol, sortDir). Takes a []sortOption allow-list (vs the map[string]string form used
// by the canonical parseSort in query_filters.go).
func parseSortOptions(r *http.Request, allowed []sortOption, defaultBy string) (sortBy, sqlCol, sortDir string) {
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

// parseRumTimeWindowArgs mirrors app.py _parse_time_window_args: normalize from_ts/to_ts (and
// optional window_s) to ClickHouse DateTime64 strings, returning ("", "", error) on a bad
// value or a non-increasing window. Empty inputs yield ("", "", ""). Uses the package
// normalizeCHTimestamp(any) (first-T fallback) + the local parseCHTimestamp.
func parseRumTimeWindowArgs(r *http.Request) (fromTS, toTS, errMsg string) {
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

// regexFilter holds the parsed include/exclude pattern lists from a `q` expression.
type regexFilter struct {
	include []string
	exclude []string
}

// prepareRumRE2FilterPatterns mirrors app.py _prepare_re2_filter_patterns: parse the
// `include && !exclude` expression, then RE2-validate every pattern via the DB. Returns the
// pattern lists (as a regexFilter) and a non-empty error string on a parse/RE2 failure. Named
// distinctly from the []string-returning prepareRE2FilterPatterns in handlers_pages_logs_errors.go.
func (s *server) prepareRumRE2FilterPatterns(raw string) (regexFilter, string) {
	rf, parseErr := parseRumRegexFilterExpression(raw)
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

// parseRumRegexFilterExpression mirrors app.py _parse_regex_filter_expression but returns a
// regexFilter struct (vs the (include, exclude, err) form of the canonical
// parseRegexFilterExpression in query_filters.go).
func parseRumRegexFilterExpression(raw string) (regexFilter, string) {
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

// appendRumRegexExpressionClauses mirrors app.py _append_regex_expression_clauses: one
// match(col, ?) per include pattern and one NOT match(col, ?) per exclude pattern. Takes a
// regexFilter and returns the extended slices (vs the pointer-extend canonical
// appendRegexExpressionClauses in query_filters.go).
func appendRumRegexExpressionClauses(conds []string, params []any, column string, rf regexFilter) ([]string, []any) {
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
