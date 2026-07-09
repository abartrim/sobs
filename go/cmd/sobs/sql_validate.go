package main

import (
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// ChdbSqlRunner SQL safety validation — faithful port of app.py:
//   _SAFE_SQL_PREFIXES, _UNSAFE_SQL_PATTERNS, ChdbSqlRunner.validate_sql,
//   ChdbSqlRunner._check_table_refs, _suggest_allowed_table_names.
//
// The Query page (and every NL→SQL / vanna execution path) must restrict
// user-submitted and LLM-generated SQL to read-only statements over the
// approved table allowlist. validateSQL reproduces the Python ValueError
// message bytes exactly (the callers wrap it as "SQL validation error: …").
// ---------------------------------------------------------------------------

// safeSQLPrefixes mirrors _SAFE_SQL_PREFIXES — the read-only statement leaders.
var safeSQLPrefixes = map[string]bool{
	"select": true, "explain": true, "show": true, "describe": true, "desc": true, "with": true,
}

// unsafeSQLPatterns mirrors _UNSAFE_SQL_PATTERNS — write/DDL keywords blocked regardless of prefix.
var unsafeSQLPatterns = regexp.MustCompile(
	`(?i)\b(insert|update|delete|drop|truncate|alter|create|replace|rename|attach|detach|` +
		`grant|revoke|system\s+stop|system\s+start|system\s+reload|kill|optimize|exchange)\b`)

// sqlCTEAliasRE mirrors _SQL_CTE_ALIAS_RE: CTE alias names (WITH alias AS (, WITH RECURSIVE alias
// AS (, , alias AS (). \w is ASCII in RE2 — table/CTE identifiers here are ASCII, matching Python's
// effective behavior for this input domain.
var sqlCTEAliasRE = regexp.MustCompile(`(?i)(?:\bWITH\s+(?:RECURSIVE\s+)?|,\s*)([a-zA-Z_]\w*)\s+AS\s*\(`)

// sqlArrayJoinRE mirrors _SQL_ARRAY_JOIN_RE: the column/array expression after ARRAY JOIN.
var sqlArrayJoinRE = regexp.MustCompile(`(?i)\bARRAY\s+JOIN\s+((?:[a-zA-Z_]\w*\.)*[a-zA-Z_]\w*)`)

// sqlTableRefRE mirrors _SQL_TABLE_REF_RE: table/view references after FROM or JOIN, with an
// optional database. qualifier.
var sqlTableRefRE = regexp.MustCompile(`(?i)\b(?:FROM|JOIN)\s+((?:[a-zA-Z_]\w*\.)*[a-zA-Z_]\w*)`)

// validateSQL mirrors ChdbSqlRunner.validate_sql. It returns "" when *sql* is a safe, read-only
// statement over the allowlist; otherwise it returns the EXACT message Python's ValueError carries
// (the callers prepend "SQL validation error: " to match _vanna_explain_sql / _vanna_run_query).
func validateSQL(sql string) string {
	stripped := strings.TrimSpace(sql)
	if stripped == "" {
		return "SQL statement is empty."
	}

	// stripped.split()[0] — first whitespace-delimited token (str.split() collapses runs).
	firstToken := strings.ToLower(strings.Fields(stripped)[0])
	if !safeSQLPrefixes[firstToken] {
		return "Only read-only SQL is allowed (SELECT, EXPLAIN, SHOW, DESCRIBE, WITH). " +
			"Got: '" + strings.ToUpper(firstToken) + "'."
	}

	if unsafeSQLPatterns.MatchString(stripped) {
		return "SQL statement contains a disallowed write or DDL keyword " +
			"(INSERT, UPDATE, DELETE, DROP, CREATE, TRUNCATE, …)."
	}

	blocked := checkTableRefs(stripped)
	if blocked != "" {
		suggestions := suggestAllowedTableNames(blocked, 5)
		suggestionText := ""
		if len(suggestions) > 0 {
			suggestionText = " Closest allowed names: " + strings.Join(suggestions, ", ") + "."
		}
		return "Access to table or view '" + blocked + "' is not permitted. " +
			"Only approved observability tables may be queried via the Query page. " +
			"Allowed tables: " + strings.Join(sortedAllowedTableNames(), ", ") + "." +
			suggestionText +
			" If this is a valid custom table/view, add it via " +
			"SOBS_QUERY_ALLOWED_TABLES."
	}
	return ""
}

// checkTableRefs mirrors ChdbSqlRunner._check_table_refs: the first disallowed FROM/JOIN table
// reference in *sql*, or "" if every reference is permitted. CTE aliases and ARRAY JOIN targets are
// excluded; the `system` database is always allowed; only `default` tables are checked against the
// allowlist.
func checkTableRefs(sql string) string {
	cteAliases := lowerCaptureSet(sqlCTEAliasRE, sql)
	arrayJoinRefs := lowerCaptureSet(sqlArrayJoinRE, sql)

	for _, m := range sqlTableRefRE.FindAllStringSubmatch(sql, -1) {
		ref := m[1]
		refLower := strings.ToLower(ref)
		if cteAliases[refLower] || arrayJoinRefs[refLower] {
			continue
		}
		parts := strings.Split(refLower, ".")
		dbName := "default"
		if len(parts) > 1 {
			dbName = parts[0]
		}
		tableName := parts[len(parts)-1]
		if dbName == "system" {
			continue
		}
		if dbName != "default" {
			return ref
		}
		if !queryAllowedTableSet[tableName] {
			return ref
		}
	}
	return ""
}

// lowerCaptureSet collects group(1) of every match of re in s, lowercased — mirrors the Python
// set-comprehensions over finditer().
func lowerCaptureSet(re *regexp.Regexp, s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(s, -1) {
		out[strings.ToLower(m[1])] = true
	}
	return out
}

// sortedAllowedTableNames mirrors sorted(_QUERY_ALLOWED_TABLES) as a []string. queryAllowedTables is
// already sorted ascending (buildQueryAllowedTables), so this just narrows []any -> []string.
func sortedAllowedTableNames() []string {
	out := make([]string, 0, len(queryAllowedTables))
	for _, t := range queryAllowedTables {
		if s, ok := t.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// suggestAllowedTableNames mirrors _suggest_allowed_table_names: difflib close matches from the
// current allowlist for the table component of a blocked reference.
func suggestAllowedTableNames(blockedRef string, maxSuggestions int) []string {
	parts := strings.Split(strings.ToLower(blockedRef), ".")
	tableName := parts[len(parts)-1]
	if tableName == "" {
		return []string{}
	}
	return getCloseMatches(tableName, sortedAllowedTableNames(), maxSuggestions, 0.45)
}

// ---------------------------------------------------------------------------
// difflib port — get_close_matches + SequenceMatcher.ratio (isjunk=None,
// autojunk irrelevant for these short identifiers). Reproduces the exact set
// and ORDER Python returns so the "Closest allowed names: …" bytes match.
// ---------------------------------------------------------------------------

// getCloseMatches mirrors difflib.get_close_matches(word, possibilities, n, cutoff). The real_quick/
// quick ratios are successively looser upper bounds on ratio(), so `ratio >= cutoff` alone is the
// equivalent admission test. heapq.nlargest(n, (ratio, x) tuples) == sort by (ratio, x) descending,
// then take n.
func getCloseMatches(word string, possibilities []string, n int, cutoff float64) []string {
	type cand struct {
		ratio float64
		x     string
	}
	wordRunes := []rune(word)
	var result []cand
	for _, x := range possibilities {
		r := sequenceRatio([]rune(x), wordRunes) // a = possibility, b = word
		if r >= cutoff {
			result = append(result, cand{r, x})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ratio != result[j].ratio {
			return result[i].ratio > result[j].ratio
		}
		return result[i].x > result[j].x
	})
	if n >= 0 && len(result) > n {
		result = result[:n]
	}
	out := make([]string, 0, len(result))
	for _, c := range result {
		out = append(out, c.x)
	}
	return out
}

// sequenceRatio mirrors SequenceMatcher(None, a, b).ratio(): 2*M/(len(a)+len(b)) where M is the
// total size of the matching blocks (Ratcliff/Obershelp). b is the seq2 over which b2j is built.
func sequenceRatio(a, b []rune) float64 {
	b2j := map[rune][]int{}
	for j, c := range b {
		b2j[c] = append(b2j[c], j)
	}
	total := len(a) + len(b)
	if total == 0 {
		return 1.0
	}
	matches := matchingBlocksTotal(a, b, b2j)
	return 2.0 * float64(matches) / float64(total)
}

// matchingBlocksTotal mirrors the sum of triple sizes from SequenceMatcher.get_matching_blocks: the
// recursive longest-match decomposition. (Only the total is needed for ratio().)
func matchingBlocksTotal(a, b []rune, b2j map[rune][]int) int {
	type box struct{ alo, ahi, blo, bhi int }
	queue := []box{{0, len(a), 0, len(b)}}
	total := 0
	for len(queue) > 0 {
		q := queue[len(queue)-1] // queue.pop() — LIFO, matching CPython
		queue = queue[:len(queue)-1]
		i, j, k := findLongestMatch(a, b, b2j, q.alo, q.ahi, q.blo, q.bhi)
		if k > 0 {
			total += k
			if q.alo < i && q.blo < j {
				queue = append(queue, box{q.alo, i, q.blo, j})
			}
			if i+k < q.ahi && j+k < q.bhi {
				queue = append(queue, box{i + k, q.ahi, j + k, q.bhi})
			}
		}
	}
	return total
}

// findLongestMatch mirrors SequenceMatcher.find_longest_match with no junk: the longest matching
// block of a[alo:ahi] vs b[blo:bhi], preferring the earliest such block.
func findLongestMatch(a, b []rune, b2j map[rune][]int, alo, ahi, blo, bhi int) (int, int, int) {
	besti, bestj, bestsize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range b2j[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}
	for besti > alo && bestj > blo && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}
	return besti, bestj, bestsize
}
