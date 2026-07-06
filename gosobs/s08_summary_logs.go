package main

// Port of app.py lines 10743-11513:
//   - DB stats helper (_get_db_stats, _fmt_bytes)
//   - Web UI – Summary (GET /)
//   - Web UI – Logs (GET /logs) incl. _validate_re2_pattern and the regex
//     filter-expression helpers shared by other dashboard sections.

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

func init() {
	registerRoute("GET", "/", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(summary)(w, r)
	})
	// PORT-NOTE: decorator order mirrors Python (traced_view innermost,
	// require_basic_auth outermost).
	registerRoute("GET", "/logs", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(telemetryTracedView(
			"sobs.dashboard.query",
			map[string]any{"dashboard.name": "logs", "route": "/logs"},
		)(viewLogs))(w, r)
	})
}

// monotonicSeconds mirrors time.monotonic() for cache-expiry math
// (summaryStatsCache stores "expires_at" as float seconds).
var monotonicStart = time.Now()

func monotonicSeconds() float64 { return time.Since(monotonicStart).Seconds() }

// roundTo2 mirrors Python round(x, 2).
// PORT-NOTE: Python uses banker's rounding; math.Round rounds half away from
// zero. The difference only affects exact .xx5 midpoints of display ratios.
func roundTo2(x float64) float64 { return math.Round(x*100) / 100 }

// ---------------------------------------------------------------------------
// DB stats helper
// ---------------------------------------------------------------------------

// getDbStats returns a map of chDB/ClickHouse storage and activity metrics.
//
// Queries are read-only against system tables and do not lock OTEL ingestion.
// Returns a best-effort result; any unavailable metric defaults to nil.
func getDbStats(db *ChDbConnection) map[string]any {
	stats := map[string]any{
		"compressed_bytes":   nil,
		"uncompressed_bytes": nil,
		"compression_ratio":  nil,
		"total_rows":         nil,
		"active_queries":     nil,
		"tables":             []map[string]any{},
	}

	// Overall compressed / uncompressed size and row count across all active parts
	res, err := db.Execute(
		"SELECT " +
			"  sum(data_compressed_bytes)   AS comp, " +
			"  sum(data_uncompressed_bytes) AS uncomp, " +
			"  sum(rows)                    AS rws " +
			"FROM system.parts " +
			"WHERE active = 1 AND database = currentDatabase()",
	)
	if err != nil {
		logger.Debug("db_stats: system.parts query failed", "error", err)
	} else if row := res.Fetchone(); row != nil {
		comp := coerceInt(row["comp"])
		uncomp := coerceInt(row["uncomp"])
		stats["compressed_bytes"] = comp
		stats["uncompressed_bytes"] = uncomp
		stats["total_rows"] = coerceInt(row["rws"])
		if comp > 0 {
			stats["compression_ratio"] = roundTo2(float64(uncomp) / float64(comp))
		}
	}

	// Per-table breakdown (top tables by compressed size)
	res, err = db.Execute(
		"SELECT table, " +
			"  sum(data_compressed_bytes)   AS comp, " +
			"  sum(data_uncompressed_bytes) AS uncomp, " +
			"  sum(rows)                    AS rws " +
			"FROM system.parts " +
			"WHERE active = 1 AND database = currentDatabase() " +
			"GROUP BY table " +
			"ORDER BY comp DESC " +
			"LIMIT 10",
	)
	if err != nil {
		logger.Debug("db_stats: per-table system.parts query failed", "error", err)
	} else {
		tableStats := []map[string]any{}
		for _, r := range res.Fetchall() {
			comp := coerceInt(r["comp"])
			uncomp := coerceInt(r["uncomp"])
			var ratio any
			if comp > 0 {
				ratio = roundTo2(float64(uncomp) / float64(comp))
			}
			tableStats = append(tableStats, map[string]any{
				"table":              r["table"],
				"compressed_bytes":   comp,
				"uncompressed_bytes": uncomp,
				"rows":               coerceInt(r["rws"]),
				"compression_ratio":  ratio,
			})
		}
		stats["tables"] = tableStats
	}

	// Number of currently executing queries (activity indicator)
	res, err = db.Execute("SELECT COUNT(*) AS cnt FROM system.processes")
	if err != nil {
		logger.Debug("db_stats: system.processes query failed", "error", err)
	} else if row := res.Fetchone(); row != nil {
		stats["active_queries"] = coerceInt(row["cnt"])
	}

	return stats
}

// fmtBytes formats a byte count into a human-readable string.
// PORT-NOTE: Python signature is `int | None`; the Go port takes any so nil
// (None) and json.Number values from Row maps both work.
func fmtBytes(n any) string {
	if n == nil {
		return "—"
	}
	f, ok := coerceFloat(n)
	if !ok {
		return "—"
	}
	if f >= 1024*1024*1024 {
		return fmt.Sprintf("%.1f GB", f/(1024*1024*1024))
	}
	if f >= 1024*1024 {
		return fmt.Sprintf("%.1f MB", f/(1024*1024))
	}
	if f >= 1024 {
		return fmt.Sprintf("%.1f KB", f/1024)
	}
	return fmt.Sprintf("%d B", int64(f))
}

// ---------------------------------------------------------------------------
// Web UI – Summary
// ---------------------------------------------------------------------------

func summary(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	errorIdSql := errorIdSqlExpr()
	unresolvedCondition := fmt.Sprintf(
		"%s NOT IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)", errorIdSql)

	// PORT-NOTE: Python lets DB exceptions propagate to a 500 page; the Go
	// port logs and returns 500 explicitly at each unguarded query.
	recentErrors := []map[string]any{}
	recentRes, err := db.Execute(
		"SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes " +
			fmt.Sprintf("FROM (%s) ", errorSourcesSql) +
			"WHERE Timestamp >= now() - INTERVAL 48 HOUR " +
			fmt.Sprintf("AND %s ", unresolvedCondition) +
			"ORDER BY Timestamp DESC " +
			"LIMIT 5",
	)
	if err != nil {
		logger.Error("summary: recent errors query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	for _, row := range recentRes.Fetchall() {
		item := buildErrorItem(row)
		recentErrors = append(recentErrors, map[string]any{
			"id":       item["id"],
			"ts":       item["ts"],
			"service":  item["service"],
			"err_type": item["err_type"],
			"message":  item["message"],
		})
	}

	nowMono := monotonicSeconds()
	var cachedStats map[string]any
	summaryStatsCacheLock.Lock()
	if exp, _ := summaryStatsCache["expires_at"].(float64); exp > nowMono {
		cachedStats, _ = summaryStatsCache["data"].(map[string]any)
	}
	summaryStatsCacheLock.Unlock()
	if len(cachedStats) == 0 {
		errorsTotalRes, err := db.Execute(fmt.Sprintf("SELECT count() AS cnt FROM (%s)", errorSourcesSql))
		if err != nil {
			logger.Error("summary: errors_total query failed", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		errorsTotal := errorsTotalRes.Fetchone()
		unresolvedRes, err := db.Execute(fmt.Sprintf(
			"SELECT count() AS cnt FROM (%s) WHERE %s", errorSourcesSql, unresolvedCondition))
		if err != nil {
			logger.Error("summary: unresolved errors query failed", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		unresolvedTotalRow := unresolvedRes.Fetchone()

		aiRes, err := db.Execute("SELECT COUNT(*) FROM otel_traces " + fmt.Sprintf("WHERE %s", aiSpanCondition))
		if err != nil {
			logger.Error("summary: ai span count query failed", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		aiCount := 0
		if row := aiRes.Fetchone(); row != nil && len(aiRes.Cols) > 0 {
			aiCount = coerceInt(row[aiRes.Cols[0]])
		}

		servicesRes, err := db.Execute(
			"SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName!='' " +
				"UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName!='' " +
				"UNION DISTINCT SELECT DISTINCT ServiceName FROM hyperdx_sessions WHERE ServiceName!=''",
		)
		if err != nil {
			logger.Error("summary: services query failed", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		services := []string{}
		for _, sr := range servicesRes.Fetchall() {
			services = append(services, rowString(sr[servicesRes.Cols[0]]))
		}

		errorsTotalCnt := 0
		if errorsTotal != nil {
			errorsTotalCnt = coerceInt(errorsTotal["cnt"])
		}
		unresolvedCnt := 0
		if unresolvedTotalRow != nil {
			unresolvedCnt = coerceInt(unresolvedTotalRow["cnt"])
		}

		cachedStats = map[string]any{
			"logs":         activePartRows(db, "otel_logs"),
			"spans":        activePartRows(db, "otel_traces"),
			"rum":          activePartRows(db, "hyperdx_sessions"),
			"ai":           aiCount,
			"errors_total": errorsTotalCnt,
			"errors":       unresolvedCnt,
			"services":     services,
		}
		summaryStatsCacheLock.Lock()
		summaryStatsCache["expires_at"] = nowMono + float64(summaryStatsCacheTtlSec)
		summaryStatsCache["data"] = cachedStats
		summaryStatsCacheLock.Unlock()
	}
	stats := map[string]any{}
	for k, v := range cachedStats {
		stats[k] = v
	}

	// Recent logs (last 10)
	recentLogs := []map[string]any{}
	logsRes, err := db.Execute(
		"SELECT Timestamp, SeverityText, ServiceName, Body FROM otel_logs ORDER BY Timestamp DESC LIMIT 10")
	if err != nil {
		logger.Error("summary: recent logs query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	for _, lr := range logsRes.Fetchall() {
		recentLogs = append(recentLogs, map[string]any{
			"ts":      rowString(lr["Timestamp"]),
			"level":   lr["SeverityText"],
			"service": lr["ServiceName"],
			"body":    lr["Body"],
		})
	}
	// RUM summary – page views last 24h
	rumRes, err := db.Execute(
		"SELECT EventName, COUNT(*) as cnt FROM hyperdx_sessions GROUP BY EventName ORDER BY cnt DESC")
	if err != nil {
		logger.Error("summary: rum summary query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// PORT-NOTE: summary.html indexes these rows positionally (row[0], row[1], …),
	// matching Python's tuple DB rows. The Go Fetchall() yields maps (no integer
	// index), so emit ordered tuples here.
	rumSummary := rowsAsTuples(rumRes)
	// AI summary
	aiSummaryRes, err := db.Execute(
		"SELECT SpanAttributes['gen_ai.request.model'] AS model, " +
			"COUNT(*) cnt, " +
			"SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.input_tokens'])) ti, " +
			"SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.output_tokens'])) to_ " +
			"FROM otel_traces " +
			fmt.Sprintf("WHERE %s ", aiSpanCondition) +
			"GROUP BY model",
	)
	if err != nil {
		logger.Error("summary: ai summary query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	aiSummary := rowsAsTuples(aiSummaryRes)

	// CVE summary for Summary page security panel.
	cveEnabledRaw := getAppSetting(db, cveEnabledSetting)
	if cveEnabledRaw == "" {
		cveEnabledRaw = "true"
	}
	cveEnabledLower := strings.ToLower(cveEnabledRaw)
	cveEnabled := cveEnabledLower == "1" || cveEnabledLower == "true" || cveEnabledLower == "yes"
	cveLastScan := getAppSetting(db, cveLastScanSetting)
	cveOverview := map[string]any{
		"enabled":   cveEnabled,
		"last_scan": cveLastScan,
		"total":     0,
		"critical":  0,
		"high":      0,
		"medium":    0,
		"low":       0,
	}
	if cveEnabled {
		cveRes, err := db.Execute(
			"SELECT Severity, COUNT(*) AS cnt FROM sobs_cve_findings FINAL GROUP BY Severity")
		if err != nil {
			logger.Error("summary cve overview query failed", "error", err)
		} else {
			total := 0
			for _, row := range cveRes.Fetchall() {
				sev := strings.ToUpper(rowString(row["Severity"]))
				cnt := coerceInt(row["cnt"])
				total += cnt
				switch sev {
				case "CRITICAL":
					cveOverview["critical"] = cveOverview["critical"].(int) + cnt
				case "HIGH":
					cveOverview["high"] = cveOverview["high"].(int) + cnt
				case "MEDIUM":
					cveOverview["medium"] = cveOverview["medium"].(int) + cnt
				case "LOW":
					cveOverview["low"] = cveOverview["low"].(int) + cnt
				}
			}
			cveOverview["total"] = total
		}
	}

	renderTemplate(w, r, "summary.html", map[string]any{
		"stats":         stats,
		"recent_errors": recentErrors,
		"recent_logs":   recentLogs,
		"rum_summary":   rumSummary,
		"ai_summary":    aiSummary,
		"signal_health": getSignalHealthByService(db),
		"cve_overview":  cveOverview,
	})
}

// rowsAsTuples converts a query result into ordered positional tuples
// ([][]any, columns in SELECT order) for templates that index rows by position
// (row[0], row[1], …) — mirroring Python's tuple DB rows.
func rowsAsTuples(res *ChDbResult) [][]any {
	out := make([][]any, 0, len(res.Rows))
	for _, row := range res.Fetchall() {
		tuple := make([]any, len(res.Cols))
		for i, col := range res.Cols {
			tuple[i] = row[col]
		}
		out = append(out, tuple)
	}
	return out
}

// computeLogStats returns (level_stats, service_stats) counts for the given
// WHERE clause.
// PORT-NOTE: Python returns insertion-ordered dicts (ORDER BY cnt DESC); Go
// maps are unordered, so the count-descending service key order is returned
// separately (used by computeAdvancedLogAnalysis to find the top service).
func computeLogStats(db *ChDbConnection, whereClauseSql string, params []any) (map[string]any, map[string]any, []string, error) {
	levelQuery := "SELECT SeverityText, COUNT(*) AS cnt " +
		fmt.Sprintf("FROM otel_logs %s ", whereClauseSql) +
		"GROUP BY SeverityText ORDER BY cnt DESC"
	levelRes, err := db.Execute(levelQuery, params...)
	if err != nil {
		return nil, nil, nil, err
	}
	levelStats := map[string]any{}
	for _, r := range levelRes.Fetchall() {
		key := rowString(r["SeverityText"])
		if key == "" {
			key = "UNKNOWN"
		}
		levelStats[key] = coerceInt(r["cnt"])
	}

	svcCond := "WHERE ServiceName!=''"
	if whereClauseSql != "" {
		svcCond = "AND ServiceName!=''"
	}
	serviceQuery := "SELECT ServiceName, COUNT(*) AS cnt " +
		fmt.Sprintf("FROM otel_logs %s %s ", whereClauseSql, svcCond) +
		"GROUP BY ServiceName ORDER BY cnt DESC LIMIT 10"
	serviceRes, err := db.Execute(serviceQuery, params...)
	if err != nil {
		return nil, nil, nil, err
	}
	serviceStats := map[string]any{}
	serviceOrder := []string{}
	for _, r := range serviceRes.Fetchall() {
		key := rowString(r["ServiceName"])
		if _, seen := serviceStats[key]; !seen {
			serviceOrder = append(serviceOrder, key)
		}
		serviceStats[key] = coerceInt(r["cnt"])
	}
	return levelStats, serviceStats, serviceOrder, nil
}

var fingerprintLogPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<uuid>"},
	{regexp.MustCompile(`\b0x[0-9a-f]+\b`), "<hex>"},
	{regexp.MustCompile(`\b[0-9a-f]{16,}\b`), "<hash>"},
	{regexp.MustCompile(`\b\d{4,}\b`), "<num>"},
	{regexp.MustCompile(`\b\d+\b`), "<n>"},
}

var (
	fingerprintSingleQuoteRe = regexp.MustCompile(`'[^']*'`)
	fingerprintDoubleQuoteRe = regexp.MustCompile(`"[^"]*"`)
	fingerprintWhitespaceRe  = regexp.MustCompile(`\s+`)
)

// fingerprintLogMessage normalizes dynamic values so repeating message
// patterns can be grouped.
func fingerprintLogMessage(message string) string {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return "(empty message)"
	}

	for _, p := range fingerprintLogPatterns {
		normalized = p.re.ReplaceAllString(normalized, p.repl)
	}

	normalized = fingerprintSingleQuoteRe.ReplaceAllString(normalized, "'<text>'")
	normalized = fingerprintDoubleQuoteRe.ReplaceAllString(normalized, `"<text>"`)
	normalized = strings.TrimSpace(fingerprintWhitespaceRe.ReplaceAllString(normalized, " "))
	return clipRunes(normalized, 160)
}

// logsCounter mirrors collections.Counter for the advanced log analysis:
// most_common sorts by count descending with insertion order on ties.
type logsCounterEntry struct {
	Key   string
	Count int
}

type logsCounter struct {
	counts map[string]int
	order  []string
}

func newLogsCounter() *logsCounter { return &logsCounter{counts: map[string]int{}} }

func (c *logsCounter) add(key string, n int) {
	if _, ok := c.counts[key]; !ok {
		c.order = append(c.order, key)
	}
	c.counts[key] += n
}

func (c *logsCounter) get(key string) int { return c.counts[key] }

func (c *logsCounter) mostCommon(n int) []logsCounterEntry {
	entries := make([]logsCounterEntry, 0, len(c.order))
	for _, k := range c.order {
		entries = append(entries, logsCounterEntry{Key: k, Count: c.counts[k]})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Count > entries[j].Count })
	if n >= 0 && len(entries) > n {
		entries = entries[:n]
	}
	return entries
}

var logsErrorFamilyRe = regexp.MustCompile(
	`\b([A-Za-z_][A-Za-z0-9_]*(?:Error|Exception|Timeout|Refused|Unavailable|Failure))\b`)

var logsKeywordRe = regexp.MustCompile(`[a-z][a-z0-9_\-]{2,}`)

var logsAnalysisStopWords = map[string]bool{
	"the":     true,
	"and":     true,
	"for":     true,
	"with":    true,
	"from":    true,
	"into":    true,
	"this":    true,
	"that":    true,
	"http":    true,
	"https":   true,
	"failed":  true,
	"error":   true,
	"warn":    true,
	"info":    true,
	"debug":   true,
	"trace":   true,
	"service": true,
}

var logsSevereLevels = map[string]bool{
	"ERROR": true, "FATAL": true, "CRITICAL": true, "ALERT": true, "EMERGENCY": true,
}

// computeAdvancedLogAnalysis computes message intelligence for manual
// advanced analysis runs.
// PORT-NOTE: serviceOrder carries the count-descending key order of
// serviceStats (Python relied on dict insertion order via next(iter(...))).
func computeAdvancedLogAnalysis(rows []Row, levelStats map[string]any, serviceStats map[string]any, serviceOrder []string) map[string]any {
	messages := []string{}
	for _, row := range rows {
		if msg := rowString(row["Body"]); msg != "" {
			messages = append(messages, msg)
		}
	}
	if len(messages) == 0 {
		return map[string]any{
			"top_patterns":   []map[string]any{},
			"top_keywords":   []map[string]any{},
			"error_families": []map[string]any{},
			"hints":          []string{},
		}
	}

	fingerprintCounts := newLogsCounter()
	for _, msg := range messages {
		fingerprintCounts.add(fingerprintLogMessage(msg), 1)
	}
	mostCommonPatterns := fingerprintCounts.mostCommon(8)
	topPatterns := []map[string]any{}
	for _, e := range mostCommonPatterns {
		topPatterns = append(topPatterns, map[string]any{"pattern": e.Key, "count": e.Count})
	}

	familyCounts := newLogsCounter()

	// Prefer structured exception types when available, then fall back to message parsing.
	for _, row := range rows {
		attrs := mapToDict(row["LogAttributes"])
		excType := strings.TrimSpace(rowString(attrs["exception.type"]))
		if excType != "" {
			familyCounts.add(excType, 1)
		}
	}

	for _, msg := range messages {
		seen := map[string]bool{}
		for _, m := range logsErrorFamilyRe.FindAllStringSubmatch(msg, -1) {
			seen[m[1]] = true
		}
		for family := range seen {
			familyCounts.add(family, 1)
		}
	}
	errorFamilies := []map[string]any{}
	for _, e := range familyCounts.mostCommon(8) {
		errorFamilies = append(errorFamilies, map[string]any{"family": e.Key, "count": e.Count})
	}

	keywordCounts := newLogsCounter()
	for _, msg := range messages {
		for _, token := range logsKeywordRe.FindAllString(strings.ToLower(msg), -1) {
			if !logsAnalysisStopWords[token] {
				keywordCounts.add(token, 1)
			}
		}
	}
	topKeywords := []map[string]any{}
	for _, e := range keywordCounts.mostCommon(10) {
		topKeywords = append(topKeywords, map[string]any{"keyword": e.Key, "count": e.Count})
	}

	hints := []string{}
	total := len(rows)
	if total < 1 {
		total = 1
	}
	severe := 0
	for level, count := range levelStats {
		if logsSevereLevels[strings.ToUpper(level)] {
			severe += coerceInt(count)
		}
	}
	severeRatio := float64(severe) / float64(total)
	if severeRatio >= 0.25 {
		hints = append(hints, fmt.Sprintf(
			"High severe-log ratio (%.0f%%); prioritize stabilizing error paths before scaling traffic.",
			severeRatio*100,
		))
	}

	if len(mostCommonPatterns) > 0 && mostCommonPatterns[0].Count >= 3 {
		topCount := mostCommonPatterns[0].Count
		hints = append(hints, fmt.Sprintf(
			"Most frequent message pattern repeats %d times; consider deduplication/sampling and shared remediation guidance.",
			topCount,
		))
	}

	timeoutHits := keywordCounts.get("timeout") + keywordCounts.get("timed")
	if timeoutHits >= 3 {
		hints = append(hints, "Timeout-related logs are common; review dependency latency, retry budgets, and circuit breakers.")
	}

	if len(serviceOrder) > 0 {
		topService := serviceOrder[0]
		topServiceCount := coerceInt(serviceStats[topService])
		if float64(topServiceCount)/float64(total) >= 0.6 {
			hints = append(hints, fmt.Sprintf(
				"Most events come from %s; investigate service-level hotspots and noisy call paths.",
				topService,
			))
		}
	}

	return map[string]any{
		"top_patterns":   topPatterns,
		"top_keywords":   topKeywords,
		"error_families": errorFamilies,
		"hints":          hints,
	}
}

// ---------------------------------------------------------------------------
// Web UI – Logs
// ---------------------------------------------------------------------------

// validateRe2Pattern returns an error message ("" when valid — Python None).
func validateRe2Pattern(db *ChDbConnection, pattern string) string {
	value := strings.TrimSpace(pattern)
	if value == "" {
		return ""
	}
	// chDB uses RE2 for match(), which is stricter than Python's re.
	if _, err := db.Execute("SELECT match('', ?)", value); err != nil {
		msg := strings.TrimSpace(err.Error())
		if idx := strings.Index(msg, ": while executing function"); idx >= 0 {
			msg = strings.TrimSpace(msg[:idx])
		}
		return fmt.Sprintf("Regex error: %s", msg)
	}
	return ""
}

// splitRegexFilterExpressionTerms splits expression by unescaped && while
// preserving escaped literal \&& tokens.
func splitRegexFilterExpressionTerms(expression string) []string {
	parts := []string{}
	var buf []byte
	i := 0
	n := len(expression)
	for i < n {
		if i+1 < n && expression[i] == '&' && expression[i+1] == '&' {
			backslashes := 0
			j := i - 1
			for j >= 0 && expression[j] == '\\' {
				backslashes++
				j--
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

// unescapeRegexFilterTerm interprets \&& as literal && within a regex term.
func unescapeRegexFilterTerm(term string) string {
	return strings.ReplaceAll(term, `\&&`, "&&")
}

// parseRegexFilterExpression parses `include && !exclude` style regex
// expressions from filter inputs. Returns (include, exclude, errorMsg) where
// errorMsg == "" mirrors Python's None.
func parseRegexFilterExpression(raw string) ([]string, []string, string) {
	expression := strings.TrimSpace(raw)
	if expression == "" {
		return []string{}, []string{}, ""
	}

	parts := splitRegexFilterExpressionTerms(expression)
	anyEmpty := len(parts) == 0
	for _, part := range parts {
		if part == "" {
			anyEmpty = true
			break
		}
	}
	if anyEmpty {
		return []string{}, []string{}, "Regex error: invalid expression around '&&'"
	}

	includePatterns := []string{}
	excludePatterns := []string{}
	for _, part := range parts {
		negate := strings.HasPrefix(part, "!")
		token := part
		if negate {
			token = strings.TrimSpace(part[1:])
		}
		token = unescapeRegexFilterTerm(token)
		if token == "" {
			return []string{}, []string{}, "Regex error: expected a pattern after '!'"
		}
		// PORT-NOTE: Python validates with re.compile(token, re.IGNORECASE);
		// Go validates with RE2 ("(?i)" prefix). RE2 is stricter, but
		// patterns are subsequently RE2-validated against chDB anyway. The
		// error message text differs from Python's re.error text.
		if _, err := regexp.Compile("(?i)" + token); err != nil {
			return []string{}, []string{}, fmt.Sprintf("Regex error: %v", err)
		}
		if negate {
			excludePatterns = append(excludePatterns, token)
		} else {
			includePatterns = append(includePatterns, token)
		}
	}

	return includePatterns, excludePatterns, ""
}

func validateRe2Patterns(db *ChDbConnection, patterns []string) string {
	for _, pattern := range patterns {
		if re2Error := validateRe2Pattern(db, pattern); re2Error != "" {
			return re2Error
		}
	}
	return ""
}

// prepareRe2FilterPatterns parses and RE2-validates regex filters intended
// for SQL match() clauses.
//
// This helper is for the RE2 DB path only. It does not affect Python-only
// regex behavior or client-side JavaScript regex handling.
func prepareRe2FilterPatterns(db *ChDbConnection, raw string) ([]string, []string, string) {
	includePatterns, excludePatterns, parseError := parseRegexFilterExpression(raw)
	if parseError != "" {
		return []string{}, []string{}, parseError
	}
	allPatterns := append(append([]string{}, includePatterns...), excludePatterns...)
	if re2Error := validateRe2Patterns(db, allPatterns); re2Error != "" {
		return []string{}, []string{}, re2Error
	}
	return includePatterns, excludePatterns, ""
}

// appendTimeWindowFilter mirrors the Python in-place list mutation via slice
// pointers.
func appendTimeWindowFilter(conditions *[]string, params *[]any, column, fromTs, toTs string) {
	timeConditions, timeParams := timeWindowConditions(column, fromTs, toTs)
	*conditions = append(*conditions, timeConditions...)
	*params = append(*params, timeParams...)
}

func whereClause(conditions []string) string {
	if len(conditions) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(conditions, " AND ")
}

func appendRegexExpressionClauses(conditions *[]string, params *[]any, column string, includePatterns, excludePatterns []string) {
	for _, pattern := range includePatterns {
		*conditions = append(*conditions, fmt.Sprintf("match(%s, ?)", column))
		*params = append(*params, pattern)
	}
	for _, pattern := range excludePatterns {
		*conditions = append(*conditions, fmt.Sprintf("NOT match(%s, ?)", column))
		*params = append(*params, pattern)
	}
}

// Friendly column aliases accepted in raw SQL WHERE input on /logs.
var logsSqlAliasSubs = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`(?i)\blevel\b`), "SeverityText"},
	{regexp.MustCompile(`(?i)\bservice\b`), "ServiceName"},
	{regexp.MustCompile(`(?i)\btrace_id\b`), "TraceId"},
	{regexp.MustCompile(`(?i)\bspan_id\b`), "SpanId"},
	{regexp.MustCompile(`(?i)\bts\b`), "Timestamp"},
	{regexp.MustCompile(`(?i)\bbody\b`), "Body"},
}

// has_tag('key', 'value') in raw SQL WHERE input. Supports SQL-escaped quotes
// inside key/value (e.g. O”Reilly).
var logsHasTagRe = regexp.MustCompile(`(?i)has_tag\s*\(\s*'((?:[^']|'')+)'\s*,\s*'((?:[^']|'')*)'\s*\)`)

// translateLogsHasTag rewrites a has_tag(...) match to a correlated subquery
// (Python: local _translate_has_tag inside view_logs).
func translateLogsHasTag(match string) string {
	groups := logsHasTagRe.FindStringSubmatch(match)
	tagKey := strings.ReplaceAll(strings.ReplaceAll(groups[1], "''", "'"), "'", "''")
	tagVal := strings.ReplaceAll(strings.ReplaceAll(groups[2], "''", "'"), "'", "''")
	return "MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)) IN (" +
		"SELECT RecordId FROM sobs_record_tags FINAL " +
		fmt.Sprintf("WHERE TagKey='%s' AND TagValue='%s' ", tagKey, tagVal) +
		"AND IsDeleted=0 AND RecordType='log')"
}

// parseLogsSnapshotTimestamp parses a chDB max(Timestamp) value.
// PORT-NOTE: Python uses datetime.fromisoformat(str(raw).replace("Z",
// "+00:00")); Go tries the equivalent layouts. Naive values are treated as
// UTC, matching the Python replace(tzinfo=timezone.utc) branch.
func parseLogsSnapshotTimestamp(raw string) (time.Time, bool) {
	s := strings.TrimSpace(strings.ReplaceAll(raw, "Z", "+00:00"))
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func viewLogs(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	query := r.URL.Query()
	q := strings.TrimSpace(query.Get("q"))
	selectedLevels := []string{}
	for _, levelVal := range query["level"] {
		if v := strings.TrimSpace(levelVal); v != "" {
			selectedLevels = append(selectedLevels, strings.ToUpper(v))
		}
	}
	selectedServices := []string{}
	for _, svc := range query["service"] {
		if v := strings.TrimSpace(svc); v != "" {
			selectedServices = append(selectedServices, v)
		}
	}
	traceId := strings.TrimSpace(query.Get("trace_id"))
	traceIds, traceId := parseTraceFilterValues(traceId, query["trace_ids"])
	traceIdsCsv := strings.Join(traceIds, ",")
	traceIdsCount := len(traceIds)
	selectedEventNames := []string{}
	for _, evt := range query["event_name"] {
		if v := strings.TrimSpace(evt); v != "" {
			selectedEventNames = append(selectedEventNames, v)
		}
	}
	eventName := "" // Keep empty for backward compatibility; use selected_event_names for filtering
	fromTs, toTs, timeError := parseTimeWindowArgs(r)
	sqlWhere := strings.TrimSpace(query.Get("sql"))
	runAdvancedAnalysis := strings.TrimSpace(query.Get("analyze")) == "1"
	limit := parseLimit(r, 200)
	offset := parseOffset(r)
	sortBy, sortCol, sortDir := parseSort(r,
		map[string]string{"Timestamp": "Timestamp", "SeverityText": "SeverityText", "ServiceName": "ServiceName"},
		"Timestamp",
	)
	orderDir := "DESC"
	if sortDir == "asc" {
		orderDir = "ASC"
	}
	orderClause := fmt.Sprintf("ORDER BY %s %s", sortCol, orderDir)

	rows := []Row{}
	logRows := []map[string]any{}
	total := 0
	errorMsg := ""
	levelStats := map[string]any{}
	serviceStats := map[string]any{}
	serviceOrder := []string{}
	var advancedAnalysis map[string]any
	statsGeneratedAtIso := ""
	statsGeneratedAtDisplay := ""
	statsGeneratedAgeS := 0
	where := ""
	params := []any{}
	includePatterns := []string{}
	excludePatterns := []string{}

	if timeError != "" {
		errorMsg = timeError
	}

	if q != "" {
		var regexError string
		includePatterns, excludePatterns, regexError = prepareRe2FilterPatterns(db, q)
		if regexError != "" {
			errorMsg = regexError
		}
	}

	if errorMsg != "" {
		// pass
	} else if sqlWhere != "" {
		// Allow raw WHERE clause (SQL search)
		if err := validateUserSqlWhere(sqlWhere); err != nil {
			errorMsg = fmt.Sprintf("SQL error: %s", publicDashboardQueryError(err))
		} else {
			safeSql := strings.ReplaceAll(sqlWhere, ";", "")
			for _, sub := range logsSqlAliasSubs {
				safeSql = sub.re.ReplaceAllString(safeSql, sub.repl)
			}
			// Translate has_tag('key', 'value') to a correlated subquery.
			safeSql = logsHasTagRe.ReplaceAllStringFunc(safeSql, translateLogsHasTag)
			where = fmt.Sprintf("WHERE %s", safeSql)
			timeConditions, timeParams := timeWindowConditions("Timestamp", fromTs, toTs)
			if len(timeConditions) > 0 {
				where = fmt.Sprintf("%s AND ", where) + strings.Join(timeConditions, " AND ")
				params = append(params, timeParams...)
			}
		}
	} else {
		conditions := []string{}
		params = []any{}
		if len(selectedLevels) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selectedLevels)), ",")
			conditions = append(conditions, fmt.Sprintf("SeverityText IN (%s)", placeholders))
			for _, v := range selectedLevels {
				params = append(params, v)
			}
		}
		if len(selectedServices) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selectedServices)), ",")
			conditions = append(conditions, fmt.Sprintf("ServiceName IN (%s)", placeholders))
			for _, v := range selectedServices {
				params = append(params, v)
			}
		}
		if len(selectedEventNames) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selectedEventNames)), ",")
			conditions = append(conditions, fmt.Sprintf("EventName IN (%s)", placeholders))
			for _, v := range selectedEventNames {
				params = append(params, v)
			}
		}
		if len(traceIds) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(traceIds)), ",")
			conditions = append(conditions, fmt.Sprintf("lower(TraceId) IN (%s)", placeholders))
			for _, v := range traceIds {
				params = append(params, v)
			}
		} else if traceId != "" {
			conditions = append(conditions, "lower(TraceId)=?")
			params = append(params, strings.ToLower(traceId))
		}
		appendTimeWindowFilter(&conditions, &params, "Timestamp", fromTs, toTs)
		where = whereClause(conditions)
	}

	if errorMsg == "" {
		// PORT-NOTE: this closure mirrors the Python try/except around the
		// query block; any error resets results below.
		queryErr := func() error {
			queryWhere := where
			queryParams := append([]any{}, params...)
			if q != "" {
				regexConditions := []string{}
				appendRegexExpressionClauses(&regexConditions, &queryParams, "Body", includePatterns, excludePatterns)
				if len(regexConditions) > 0 {
					regexSql := strings.Join(regexConditions, " AND ")
					if queryWhere != "" {
						queryWhere = fmt.Sprintf("%s AND %s", queryWhere, regexSql)
					} else {
						queryWhere = fmt.Sprintf("WHERE %s", regexSql)
					}
				}
			}

			selectBase := "SELECT Timestamp, SeverityText, ServiceName, Body, TraceId, SpanId " +
				fmt.Sprintf("FROM otel_logs %s ", queryWhere)

			if queryWhere == "" {
				total = activePartRows(db, "otel_logs")
			} else {
				totalRes, err := db.Execute(fmt.Sprintf("SELECT COUNT(*) FROM otel_logs %s", queryWhere), queryParams...)
				if err != nil {
					return err
				}
				if row := totalRes.Fetchone(); row != nil && len(totalRes.Cols) > 0 {
					total = coerceInt(row[totalRes.Cols[0]])
				}
			}
			rowsRes, err := db.Execute(
				fmt.Sprintf("%s%s LIMIT ? OFFSET ?", selectBase, orderClause),
				append(append([]any{}, queryParams...), limit, offset)...,
			)
			if err != nil {
				return err
			}
			rows = rowsRes.Fetchall()
			levelStats, serviceStats, serviceOrder, err = computeLogStats(db, queryWhere, queryParams)
			if err != nil {
				return err
			}
			if runAdvancedAnalysis {
				analysisRes, err := db.Execute(
					fmt.Sprintf("SELECT SeverityText, ServiceName, Body, LogAttributes FROM otel_logs %s", queryWhere),
					queryParams...,
				)
				if err != nil {
					return err
				}
				advancedAnalysis = computeAdvancedLogAnalysis(analysisRes.Fetchall(), levelStats, serviceStats, serviceOrder)
			}

			generatedAt := time.Now().UTC()
			snapshotRes, err := db.Execute(fmt.Sprintf("SELECT max(Timestamp) FROM otel_logs %s", queryWhere), queryParams...)
			if err != nil {
				return err
			}
			snapshotAt := generatedAt
			if snapRow := snapshotRes.Fetchone(); snapRow != nil && len(snapshotRes.Cols) > 0 {
				if raw := snapRow[snapshotRes.Cols[0]]; raw != nil {
					parsed, ok := parseLogsSnapshotTimestamp(rowString(raw))
					if !ok {
						// PORT-NOTE: Python datetime.fromisoformat raises here,
						// landing in the except branch; error text differs.
						return fmt.Errorf("invalid snapshot timestamp: %s", rowString(raw))
					}
					snapshotAt = parsed
				}
			}

			statsGeneratedAtIso = pyIsoFormat(snapshotAt)
			statsGeneratedAtDisplay = snapshotAt.Format("2006-01-02 15:04:05 UTC")
			ageS := int(generatedAt.Sub(snapshotAt).Seconds())
			if ageS < 0 {
				ageS = 0
			}
			statsGeneratedAgeS = ageS
			return nil
		}()
		if queryErr != nil {
			if sqlWhere != "" {
				errorMsg = fmt.Sprintf("SQL error: %s", publicDashboardQueryError(queryErr))
			} else {
				errorMsg = fmt.Sprintf("Query error: %v", queryErr)
			}
			rows = []Row{}
			total = 0
			levelStats = map[string]any{}
			serviceStats = map[string]any{}
			serviceOrder = []string{}
			advancedAnalysis = nil
		}
	}

	// Compute record IDs for visible rows so tags can be batch-fetched
	rowRecordIds := []any{}
	for _, rr := range rows {
		rowRecordIds = append(rowRecordIds, recordIdForLog(
			rowString(rr["Timestamp"]), rowString(rr["ServiceName"]),
			rowString(rr["TraceId"]), rowString(rr["SpanId"])))
	}
	// Batch-fetch tags for all visible rows in one query
	tagsByRecordId := map[string][]map[string]any{}
	type logsTagStatKey struct{ key, value string }
	tagStatsCount := map[logsTagStatKey]int{}
	tagStatsOrder := []logsTagStatKey{}
	if len(rowRecordIds) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(rowRecordIds)), ",")
		tagRes, err := db.Execute(
			"SELECT RecordId, TagKey, TagValue, IsAuto "+
				"FROM sobs_record_tags FINAL "+
				fmt.Sprintf("WHERE RecordType='log' AND RecordId IN (%s) AND IsDeleted=0 ", placeholders)+
				"ORDER BY RecordId, TagKey",
			rowRecordIds...,
		)
		if err != nil {
			// Tags are supplementary; ignore failures
			logger.Debug("view_logs: tag batch fetch failed", "error", err)
		} else {
			for _, tr := range tagRes.Fetchall() {
				rid := rowString(tr["RecordId"])
				entry := map[string]any{
					"key":     rowString(tr["TagKey"]),
					"value":   rowString(tr["TagValue"]),
					"is_auto": coerceInt(tr["IsAuto"]) != 0,
				}
				tagsByRecordId[rid] = append(tagsByRecordId[rid], entry)
				statsKey := logsTagStatKey{key: rowString(tr["TagKey"]), value: rowString(tr["TagValue"])}
				if _, seen := tagStatsCount[statsKey]; !seen {
					tagStatsOrder = append(tagStatsOrder, statsKey)
				}
				tagStatsCount[statsKey]++
			}
		}
	}

	// Sorted by (-count, key, value) like the Python sorted(...) call.
	sort.SliceStable(tagStatsOrder, func(i, j int) bool {
		a, b := tagStatsOrder[i], tagStatsOrder[j]
		if tagStatsCount[a] != tagStatsCount[b] {
			return tagStatsCount[a] > tagStatsCount[b]
		}
		if a.key != b.key {
			return a.key < b.key
		}
		return a.value < b.value
	})
	tagStats := []map[string]any{}
	for _, k := range tagStatsOrder {
		tagStats = append(tagStats, map[string]any{"key": k.key, "value": k.value, "count": tagStatsCount[k]})
	}

	for _, rr := range rows {
		body := rr["Body"]
		rid := recordIdForLog(
			rowString(rr["Timestamp"]), rowString(rr["ServiceName"]),
			rowString(rr["TraceId"]), rowString(rr["SpanId"]))
		tags := tagsByRecordId[rid]
		if tags == nil {
			tags = []map[string]any{}
		}
		logRows = append(logRows, map[string]any{
			"ts":        rowString(rr["Timestamp"]),
			"level":     rr["SeverityText"],
			"service":   rr["ServiceName"],
			"body":      body,
			"trace_id":  rr["TraceId"],
			"span_id":   rr["SpanId"],
			"record_id": rid,
			"tags":      tags,
		})
	}

	// PORT-NOTE: Python lets these lookup-query exceptions propagate to a 500
	// page; the Go port logs and returns 500 explicitly.
	servicesRes, err := db.Execute(
		"SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName!='' ORDER BY ServiceName")
	if err != nil {
		logger.Error("view_logs: services query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	services := []string{}
	for _, row := range servicesRes.Fetchall() {
		services = append(services, rowString(row[servicesRes.Cols[0]]))
	}
	levelsRes, err := db.Execute("SELECT DISTINCT SeverityText FROM otel_logs ORDER BY SeverityText")
	if err != nil {
		logger.Error("view_logs: levels query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	levels := []string{}
	for _, row := range levelsRes.Fetchall() {
		levels = append(levels, rowString(row[levelsRes.Cols[0]]))
	}
	eventNamesRes, err := db.Execute(
		"SELECT DISTINCT EventName FROM otel_logs WHERE EventName!='' ORDER BY EventName")
	if err != nil {
		logger.Error("view_logs: event names query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	eventNames := []string{}
	for _, row := range eventNamesRes.Fetchall() {
		eventNames = append(eventNames, rowString(row[eventNamesRes.Cols[0]]))
	}

	renderTemplate(w, r, "logs.html", map[string]any{
		"logs":                       logRows,
		"total":                      total,
		"limit":                      limit,
		"offset":                     offset,
		"q":                          q,
		"level":                      "", // Keep empty for backward compatibility; use selected_levels for filtering
		"selected_levels":            selectedLevels,
		"service":                    "", // Keep empty for backward compatibility; use selected_services for filtering
		"selected_services":          selectedServices,
		"trace_id":                   traceId,
		"trace_ids_csv":              traceIdsCsv,
		"trace_ids_count":            traceIdsCount,
		"sql_where":                  sqlWhere,
		"from_ts":                    fromTs,
		"to_ts":                      toTs,
		"services":                   services,
		"levels":                     levels,
		"event_names":                eventNames,
		"event_name":                 eventName,
		"selected_event_names":       selectedEventNames,
		"error_msg":                  errorMsg,
		"sort_by":                    sortBy,
		"sort_dir":                   sortDir,
		"run_advanced_analysis":      runAdvancedAnalysis,
		"level_stats":                levelStats,
		"service_stats":              serviceStats,
		"tag_stats":                  tagStats,
		"advanced_analysis":          advancedAnalysis,
		"stats_generated_at_iso":     statsGeneratedAtIso,
		"stats_generated_at_display": statsGeneratedAtDisplay,
		"stats_generated_age_s":      statsGeneratedAgeS,
	})
}
