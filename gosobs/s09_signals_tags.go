package main

// s09_signals_tags.go — port of app.py lines 11514-13309:
// derived signals / anomaly rules helpers, the centralised user SQL WHERE
// injection protection, and tag rules helpers.
//
// PORT-NOTE: rule-reason strings interpolate floats; Python float repr prints
// e.g. "5.0" where Go's %v prints "5". ruleReasonNumber mimics Python repr for
// the rounded values used in rule reasons; plain %v is used where Python
// interpolates row values directly.

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Derived Signals / Rules Helpers
// ---------------------------------------------------------------------------

var anomalySeverityRank = map[string]int{"normal": 0, "warning": 1, "outlier": 2}

const aiTracePromptSql = "coalesce(SpanAttributes['sobs.gen_ai.prompt'], " +
	"SpanAttributes['gen_ai.turn.summary.request'], " +
	"SpanAttributes['gen_ai.input.question'], " +
	"SpanAttributes['gen_ai.input.messages'])"

const aiTraceResponseSql = "coalesce(SpanAttributes['sobs.gen_ai.response'], " +
	"SpanAttributes['gen_ai.output.messages'])"

// Semantic convention-first condition: a span is an AI span if it carries any of the
// canonical GenAI semantic convention attributes (gen_ai.provider.name, gen_ai.operation.name)
// or the legacy gen_ai.system field used by older instrumentations.
const aiSpanCondition = "(SpanAttributes['gen_ai.provider.name'] != '' " +
	"OR SpanAttributes['gen_ai.system'] != '' " +
	"OR SpanAttributes['gen_ai.operation.name'] != '')"

// aiSqlReplacement is one (pattern, replacement) pair applied case-insensitively
// outside single-quoted SQL literals.
type aiSqlReplacement struct {
	re   *regexp.Regexp
	repl string
}

// replaceSqlOutsideSingleQuotes mirrors _replace_sql_outside_single_quotes:
// masks single-quoted literals (handling ” escapes), applies the regex
// replacements on the masked text, then restores the literals.
func replaceSqlOutsideSingleQuotes(sql string, replacements []aiSqlReplacement) string {
	placeholders := []string{}
	var maskedParts strings.Builder
	i := 0
	for i < len(sql) {
		ch := sql[i]
		if ch != '\'' {
			maskedParts.WriteByte(ch)
			i++
			continue
		}

		start := i
		i++
		for i < len(sql) {
			if sql[i] == '\'' {
				if i+1 < len(sql) && sql[i+1] == '\'' {
					i += 2
					continue
				}
				i++
				break
			}
			i++
		}

		literal := sql[start:i]
		token := fmt.Sprintf("__SQL_LITERAL_%d__", len(placeholders))
		placeholders = append(placeholders, literal)
		maskedParts.WriteString(token)
	}

	masked := maskedParts.String()
	for _, replacement := range replacements {
		masked = replacement.re.ReplaceAllString(masked, replacement.repl)
	}
	for idx, literal := range placeholders {
		masked = strings.ReplaceAll(masked, fmt.Sprintf("__SQL_LITERAL_%d__", idx), literal)
	}
	return masked
}

// aiSqlWhereReplacements mirrors the replacement list in _normalize_ai_sql_where.
// Patterns are compiled with (?i) to match Python's re.IGNORECASE.
var aiSqlWhereReplacements = []aiSqlReplacement{
	{regexp.MustCompile(`(?i)\bLogAttributes\s*\[`), "SpanAttributes["},
	{regexp.MustCompile(`(?i)SpanAttributes\s*\[\s*'prompt'\s*\]`), aiTracePromptSql},
	{regexp.MustCompile(`(?i)SpanAttributes\s*\[\s*'response'\s*\]`), aiTraceResponseSql},
	{regexp.MustCompile(`(?i)\bservice\b`), "ServiceName"},
	{regexp.MustCompile(`(?i)\bmodel\b`), "SpanAttributes['gen_ai.request.model']"},
	{regexp.MustCompile(`(?i)\bprovider\b`), "SpanAttributes['gen_ai.provider.name']"},
	{regexp.MustCompile(`(?i)\boperation\b`), "SpanAttributes['gen_ai.operation.name']"},
	{regexp.MustCompile(`(?i)\bprompt\b`), aiTracePromptSql},
	{regexp.MustCompile(`(?i)\bresponse\b`), aiTraceResponseSql},
	{regexp.MustCompile(`(?i)\btrace_id\b`), "TraceId"},
	{regexp.MustCompile(`(?i)\bspan_id\b`), "SpanId"},
	{regexp.MustCompile(`(?i)\bspan_name\b`), "SpanName"},
	{regexp.MustCompile(`(?i)\brow_type\b`), "if(SpanAttributes['gen_ai.request.model'] != '', 'llm', 'system')"},
	{regexp.MustCompile(`(?i)\bts\b`), "Timestamp"},
	{regexp.MustCompile(`(?i)\bstatus\b`), "StatusCode"},
	{regexp.MustCompile(`(?i)\berror_type\b`), "SpanAttributes['error.type']"},
	{regexp.MustCompile(`(?i)\btokens_in\b`), "toUInt64OrZero(SpanAttributes['gen_ai.usage.input_tokens'])"},
	{regexp.MustCompile(`(?i)\btokens_out\b`), "toUInt64OrZero(SpanAttributes['gen_ai.usage.output_tokens'])"},
	{regexp.MustCompile(`(?i)\bthinking_tokens\b`), "toUInt64OrZero(SpanAttributes['gen_ai.usage.thinking_tokens'])"},
	{regexp.MustCompile(`(?i)\bduration_ms\b`), "(Duration / 1000000.0)"},
}

// normalizeAiSqlWhere mirrors _normalize_ai_sql_where. The ValueError raised by
// _validate_user_sql_where becomes the returned error.
func normalizeAiSqlWhere(sqlWhere string) (string, error) {
	if err := validateUserSqlWhere(sqlWhere); err != nil {
		return "", err
	}
	safeSql := strings.ReplaceAll(sqlWhere, ";", "")
	return replaceSqlOutsideSingleQuotes(safeSql, aiSqlWhereReplacements), nil
}

// ---------------------------------------------------------------------------
// User SQL WHERE fragment – centralised injection protection
// ---------------------------------------------------------------------------

// Write / DDL keywords that must never appear in user-supplied WHERE filters.
var unsafeWherePatterns = regexp.MustCompile(
	`(?i)\b(insert|update|delete|drop|truncate|alter|create|replace|rename|attach|detach|` +
		`grant|revoke|system\s+stop|system\s+start|system\s+reload|kill|optimize|exchange)\b`,
)

// validateUserSqlWhere mirrors _validate_user_sql_where: returns an error if a
// user-supplied SQL WHERE fragment contains unsafe patterns.
//
// This is the centralised injection-protection layer for all filter-bar inputs
// across every page (logs, AI, traces, errors, RUM, metrics). It is applied
// before the normalised fragment is interpolated into any `WHERE {safe_sql}`
// clause.
//
// Blocked patterns:
//
//   - Write / DDL keywords: INSERT, UPDATE, DELETE, DROP, TRUNCATE, ALTER,
//     CREATE, REPLACE, RENAME, …
//
// Note: Set operations (UNION, INTERSECT, EXCEPT) are deliberately NOT blocked
// here because they are valid in dynamic dataset queries used by the NQL page
// and custom charts. The broader table-access control for the NL→SQL Query
// page is handled separately by ChdbSqlRunner. This function intentionally
// does NOT block SELECT itself, because valid ClickHouse WHERE conditions may
// contain correlated subqueries (e.g. EXISTS (SELECT 1 FROM … WHERE …)).
func validateUserSqlWhere(sqlWhere string) error {
	if unsafeWherePatterns.MatchString(sqlWhere) {
		return fmt.Errorf(
			"SQL filter contains a disallowed keyword. " +
				"Only comparison and logical expressions are permitted in filter fields.")
	}
	return nil
}

// listDerivedSignalDimensions mirrors _list_derived_signal_dimensions.
func listDerivedSignalDimensions(db *ChDbConnection) ([]string, []string, []string, error) {
	// ServiceName: query raw tables with LowCardinality indexes — avoids a full
	// 12-way UNION ALL scan through the derived-signals view.
	res, err := db.Execute(
		"SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName != ''" +
			" UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName != ''" +
			" UNION DISTINCT SELECT DISTINCT ServiceName FROM hyperdx_sessions WHERE ServiceName != ''" +
			" ORDER BY ServiceName")
	if err != nil {
		return nil, nil, nil, err
	}
	services := []string{}
	for _, row := range res.Fetchall() {
		services = append(services, rowString(row[res.Cols[0]]))
	}
	// SignalName / SignalSource are a static enumeration determined by the view
	// definition — no DB query needed.
	signals := []string{
		"log_volume",
		"error_volume",
		"error_ratio",
		"trace_volume",
		"trace_error_ratio",
		"latency_p95_ms",
		"exception_volume",
		"LCP",
		"FID",
		"CLS",
		"INP",
		"TTFB",
		"FCP",
	}
	sort.Strings(signals)
	sources := []string{"errors", "logs", "rum_vitals", "traces"}
	return services, signals, sources, nil
}

var autoRuleGtHints = []string{
	"error",
	"latency",
	"duration",
	"timeout",
	"p95",
	"p99",
	"failure",
	"fail",
	"retry",
}
var autoRuleLtHints = []string{"availability", "success", "throughput", "rps", "qps"}

const autoRuleCreateMax = 200
const autoDashboardCreateMax = 24
const autoTagRuleCreateMax = 200

// inferAutoRuleComparator mirrors _infer_auto_rule_comparator.
func inferAutoRuleComparator(signalName string) string {
	name := strings.ToLower(signalName)
	for _, token := range autoRuleLtHints {
		if strings.Contains(name, token) {
			return "lt"
		}
	}
	for _, token := range autoRuleGtHints {
		if strings.Contains(name, token) {
			return "gt"
		}
	}
	return "gt"
}

// autoRuleThresholds mirrors _auto_rule_thresholds: returns (warning, critical).
func autoRuleThresholds(comparator string, q05, q20, q50, q80, q95 float64) (float64, float64) {
	if comparator == "lt" {
		warning := q20
		critical := q05
		if critical > warning {
			critical = math.Min(warning, q50)
		}
		if critical == warning {
			if warning != 0 {
				critical = warning * 0.9
			} else {
				critical = -0.1
			}
		}
		return warning, critical
	}

	warning := q80
	critical := q95
	if critical < warning {
		critical = math.Max(warning, q50)
	}
	if critical == warning {
		if warning != 0 {
			critical = warning * 1.1
		} else {
			critical = 0.1
		}
	}
	return warning, critical
}

// formatAutoRuleName mirrors _format_auto_rule_name.
func formatAutoRuleName(source, signal, service, attrFp string) string {
	suffix := service
	if suffix == "" {
		suffix = "any"
	}
	if attrFp != "" {
		suffix = fmt.Sprintf("%s / %s", suffix, attrFp)
	}
	return fmt.Sprintf("Auto %s/%s [%s]", source, signal, suffix)
}

// anomalyRuleScopeKey is the (source, signal, service, attr_fp, rule_type)
// tuple used to dedupe auto-rule candidates against existing rules.
type anomalyRuleScopeKey struct {
	source, signal, service, attrFp, ruleType string
}

func existingAnomalyRuleScopes(rules []map[string]any) map[anomalyRuleScopeKey]bool {
	existing := map[anomalyRuleScopeKey]bool{}
	for _, rule := range rules {
		ruleType := rowString(rule["rule_type"])
		if ruleType == "" {
			ruleType = "threshold"
		}
		existing[anomalyRuleScopeKey{
			source:   rowString(rule["source"]),
			signal:   rowString(rule["signal"]),
			service:  rowString(rule["service"]),
			attrFp:   rowString(rule["attr_fp"]),
			ruleType: ruleType,
		}] = true
	}
	return existing
}

// buildAutoMetricRuleCandidates mirrors _build_auto_metric_rule_candidates.
func buildAutoMetricRuleCandidates(
	db *ChDbConnection,
	hours int,
	minPoints int,
	serviceFilter string,
	includeAttrFp bool,
) ([]map[string]any, map[string]int, error) {
	whereParts := []string{"time >= now() - INTERVAL ? HOUR"}
	params := []any{hours}
	if serviceFilter != "" {
		whereParts = append(whereParts, "ServiceName = ?")
		params = append(params, serviceFilter)
	}

	whereSql := " WHERE " + strings.Join(whereParts, " AND ")
	attrSelect := "''"
	attrGroup := ""
	if includeAttrFp {
		attrSelect = "AttrFingerprint"
		attrGroup = ", AttrFingerprint"
	}
	statsRes, err := db.Execute(
		"SELECT ServiceName, SignalSource, SignalName, "+
			attrSelect+" AS AttrFingerprint, "+
			"count() AS point_count, "+
			"quantile(0.05)(toFloat64(value)) AS q05, "+
			"quantile(0.20)(toFloat64(value)) AS q20, "+
			"quantile(0.50)(toFloat64(value)) AS q50, "+
			"quantile(0.80)(toFloat64(value)) AS q80, "+
			"quantile(0.95)(toFloat64(value)) AS q95 "+
			"FROM v_derived_signals_anomaly"+
			whereSql+
			" GROUP BY ServiceName, SignalSource, SignalName"+
			attrGroup+
			" HAVING point_count >= ?"+
			" ORDER BY point_count DESC",
		append(append([]any{}, params...), minPoints)...,
	)
	if err != nil {
		return nil, nil, err
	}
	statsRows := statsRes.Fetchall()

	activeRules, err := loadAnomalyRules(db)
	if err != nil {
		return nil, nil, err
	}
	existingSeries := existingAnomalyRuleScopes(activeRules)

	createdCandidates := []map[string]any{}
	skippedExisting := 0
	skippedInvalid := 0
	for _, row := range statsRows {
		service := rowString(row["ServiceName"])
		source := rowString(row["SignalSource"])
		signal := rowString(row["SignalName"])
		attrFp := rowString(row["AttrFingerprint"])
		key := anomalyRuleScopeKey{source: source, signal: signal, service: service, attrFp: attrFp, ruleType: "threshold"}
		if existingSeries[key] {
			skippedExisting++
			continue
		}

		pointCount := coerceInt(row["point_count"])
		q05, _ := coerceFloat(row["q05"])
		q20, _ := coerceFloat(row["q20"])
		q50, _ := coerceFloat(row["q50"])
		q80, _ := coerceFloat(row["q80"])
		q95, _ := coerceFloat(row["q95"])
		comparator := inferAutoRuleComparator(signal)
		warning, critical := autoRuleThresholds(comparator, q05, q20, q50, q80, q95)

		if comparator == "gt" && critical < warning {
			skippedInvalid++
			continue
		}
		if comparator == "lt" && critical > warning {
			skippedInvalid++
			continue
		}

		createdCandidates = append(createdCandidates, map[string]any{
			"name":               formatAutoRuleName(source, signal, service, attrFp),
			"rule_type":          "threshold",
			"source":             source,
			"signal":             signal,
			"service":            service,
			"attr_fp":            attrFp,
			"comparator":         comparator,
			"warning_threshold":  warning,
			"critical_threshold": critical,
			"min_sample_count":   3,
			"point_count":        pointCount,
		})
	}

	return createdCandidates, map[string]int{
		"examined": len(statsRows),
		"existing": skippedExisting,
		"invalid":  skippedInvalid,
	}, nil
}

// Supported seasonal strategies for auto-rule generation.
var seasonalStrategies = []string{"hour_of_day", "day_of_week"}

const seasonalMinBucketPoints = 3

// buildSeasonalBucketExpr returns a ClickHouse expression for the seasonal bucket key.
func buildSeasonalBucketExpr(strategy string) string {
	if strategy == "day_of_week" {
		return "toDayOfWeek(time)" // 1 (Mon) … 7 (Sun)
	}
	return "toHour(time)" // 0 … 23
}

// seasonalSeriesKey is the (source, signal, service, attr_fp) tuple keying
// per-series bucket data in buildSeasonalMetricRuleCandidates.
type seasonalSeriesKey struct {
	source, signal, service, attrFp string
}

// buildSeasonalMetricRuleCandidates mirrors _build_seasonal_metric_rule_candidates.
//
// Build auto-rule candidates using per-bucket (seasonal) thresholds.
//
// For each signal series that has enough data points over the lookback window,
// the function computes warning/critical thresholds independently for every
// hour-of-day (or day-of-week) bucket. The resulting candidate carries a
// `seasonal_buckets_json` payload that the evaluator uses at runtime to pick
// the threshold corresponding to the current time bucket.
func buildSeasonalMetricRuleCandidates(
	db *ChDbConnection,
	hours int,
	minPoints int,
	serviceFilter string,
	includeAttrFp bool,
	strategy string,
) ([]map[string]any, map[string]int, error) {
	validStrategy := false
	for _, s := range seasonalStrategies {
		if strategy == s {
			validStrategy = true
			break
		}
	}
	if !validStrategy {
		strategy = "hour_of_day"
	}
	bucketExpr := buildSeasonalBucketExpr(strategy)

	whereParts := []string{"time >= now() - INTERVAL ? HOUR"}
	params := []any{hours}
	if serviceFilter != "" {
		whereParts = append(whereParts, "ServiceName = ?")
		params = append(params, serviceFilter)
	}

	whereSql := " WHERE " + strings.Join(whereParts, " AND ")
	attrSelect := "''"
	attrGroup := ""
	if includeAttrFp {
		attrSelect = "AttrFingerprint"
		attrGroup = ", AttrFingerprint"
	}

	// Per-series totals for the min_points filter.
	seriesRes, err := db.Execute(
		"SELECT ServiceName, SignalSource, SignalName, "+
			attrSelect+" AS AttrFingerprint, "+
			"count() AS point_count, "+
			"quantile(0.05)(toFloat64(value)) AS q05, "+
			"quantile(0.20)(toFloat64(value)) AS q20, "+
			"quantile(0.50)(toFloat64(value)) AS q50, "+
			"quantile(0.80)(toFloat64(value)) AS q80, "+
			"quantile(0.95)(toFloat64(value)) AS q95 "+
			"FROM v_derived_signals_anomaly"+
			whereSql+
			" GROUP BY ServiceName, SignalSource, SignalName"+
			attrGroup+
			" HAVING point_count >= ?"+
			" ORDER BY point_count DESC",
		append(append([]any{}, params...), minPoints)...,
	)
	if err != nil {
		return nil, nil, err
	}
	seriesRows := seriesRes.Fetchall()

	// Only compute bucket stats for series that pass the min_points gate.
	// This avoids scanning and materializing buckets for sparse series that
	// can never become candidates in this run.
	eligibleSeriesSubquery := "SELECT ServiceName, SignalSource, SignalName, " +
		attrSelect + " AS AttrFingerprint " +
		"FROM v_derived_signals_anomaly" +
		whereSql +
		" GROUP BY ServiceName, SignalSource, SignalName" +
		attrGroup +
		" HAVING count() >= ?"

	// Per-series-per-bucket quantiles (requires minimum support per bucket).
	bucketParams := append(append([]any{}, params...), params...)
	bucketParams = append(bucketParams, minPoints, seasonalMinBucketPoints)
	bucketRes, err := db.Execute(
		"SELECT ServiceName, SignalSource, SignalName, "+
			attrSelect+" AS AttrFingerprint, "+
			bucketExpr+" AS bucket_key, "+
			"count() AS point_count, "+
			"quantile(0.05)(toFloat64(value)) AS q05, "+
			"quantile(0.20)(toFloat64(value)) AS q20, "+
			"quantile(0.50)(toFloat64(value)) AS q50, "+
			"quantile(0.80)(toFloat64(value)) AS q80, "+
			"quantile(0.95)(toFloat64(value)) AS q95 "+
			"FROM v_derived_signals_anomaly"+
			whereSql+
			" AND (ServiceName, SignalSource, SignalName, "+
			attrSelect+") IN ("+eligibleSeriesSubquery+")"+
			" GROUP BY ServiceName, SignalSource, SignalName"+
			attrGroup+
			", bucket_key"+
			" HAVING point_count >= ?"+
			" ORDER BY ServiceName, SignalSource, SignalName"+attrGroup+", bucket_key",
		bucketParams...,
	)
	if err != nil {
		return nil, nil, err
	}
	bucketRows := bucketRes.Fetchall()

	// Index bucket data by series key.
	bucketIndex := map[seasonalSeriesKey]map[string]map[string]float64{}
	for _, br := range bucketRows {
		bucketSeriesKey := seasonalSeriesKey{
			source:  rowString(br["SignalSource"]),
			signal:  rowString(br["SignalName"]),
			service: rowString(br["ServiceName"]),
			attrFp:  rowString(br["AttrFingerprint"]),
		}
		bk := strconv.Itoa(coerceInt(br["bucket_key"]))
		comparator := inferAutoRuleComparator(rowString(br["SignalName"]))
		q05, _ := coerceFloat(br["q05"])
		q20, _ := coerceFloat(br["q20"])
		q50, _ := coerceFloat(br["q50"])
		q80, _ := coerceFloat(br["q80"])
		q95, _ := coerceFloat(br["q95"])
		w, c := autoRuleThresholds(comparator, q05, q20, q50, q80, q95)
		if bucketIndex[bucketSeriesKey] == nil {
			bucketIndex[bucketSeriesKey] = map[string]map[string]float64{}
		}
		bucketIndex[bucketSeriesKey][bk] = map[string]float64{"warning": w, "critical": c}
	}

	activeRules, err := loadAnomalyRules(db)
	if err != nil {
		return nil, nil, err
	}
	existingSeries := existingAnomalyRuleScopes(activeRules)

	createdCandidates := []map[string]any{}
	skippedExisting := 0
	skippedInvalid := 0

	for _, row := range seriesRows {
		service := rowString(row["ServiceName"])
		source := rowString(row["SignalSource"])
		signal := rowString(row["SignalName"])
		attrFp := rowString(row["AttrFingerprint"])
		ruleScopeKey := anomalyRuleScopeKey{source: source, signal: signal, service: service, attrFp: attrFp, ruleType: "seasonal"}
		if existingSeries[ruleScopeKey] {
			skippedExisting++
			continue
		}

		pointCount := coerceInt(row["point_count"])
		q05, _ := coerceFloat(row["q05"])
		q20, _ := coerceFloat(row["q20"])
		q50, _ := coerceFloat(row["q50"])
		q80, _ := coerceFloat(row["q80"])
		q95, _ := coerceFloat(row["q95"])
		comparator := inferAutoRuleComparator(signal)
		warning, critical := autoRuleThresholds(comparator, q05, q20, q50, q80, q95)

		if comparator == "gt" && critical < warning {
			skippedInvalid++
			continue
		}
		if comparator == "lt" && critical > warning {
			skippedInvalid++
			continue
		}

		seriesBuckets := bucketIndex[seasonalSeriesKey{source: source, signal: signal, service: service, attrFp: attrFp}]
		if seriesBuckets == nil {
			seriesBuckets = map[string]map[string]float64{}
		}
		// PORT-NOTE: Python json.dumps preserves dict insertion order
		// ("strategy" then "buckets"); Go's json marshalling sorts map keys.
		// The payload shape is identical and the evaluator is key-order agnostic.
		seasonalBucketsJson := jsonDumpsNoEscape(map[string]any{"strategy": strategy, "buckets": seriesBuckets})

		createdCandidates = append(createdCandidates, map[string]any{
			"name":                  formatAutoRuleName(source, signal, service, attrFp),
			"rule_type":             "seasonal",
			"source":                source,
			"signal":                signal,
			"service":               service,
			"attr_fp":               attrFp,
			"comparator":            comparator,
			"warning_threshold":     warning,
			"critical_threshold":    critical,
			"min_sample_count":      3,
			"point_count":           pointCount,
			"seasonal_buckets_json": seasonalBucketsJson,
			"seasonal_bucket_count": len(seriesBuckets),
			"seasonal_strategy":     strategy,
		})
	}

	return createdCandidates, map[string]int{
		"examined": len(seriesRows),
		"existing": skippedExisting,
		"invalid":  skippedInvalid,
	}, nil
}

// defaultAutoDashboardName mirrors _default_auto_dashboard_name.
func defaultAutoDashboardName(serviceFilter string) string {
	if serviceFilter != "" {
		return fmt.Sprintf("Auto Metric Rules - %s", serviceFilter)
	}
	return "Auto Metric Rules Dashboard"
}

var autoTagSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// autoTagSlug mirrors _auto_tag_slug (max_len defaults to 64).
func autoTagSlug(value, fallback string, maxLen ...int) string {
	limit := 64
	if len(maxLen) > 0 {
		limit = maxLen[0]
	}
	raw := strings.ToLower(strings.TrimSpace(value))
	slug := strings.Trim(autoTagSlugRe.ReplaceAllString(raw, "_"), "_")
	if slug == "" {
		slug = fallback
	}
	return clipRunes(slug, limit)
}

var (
	inferEnvProdRe    = regexp.MustCompile(`(^|[-_\.])(prod|production)($|[-_\.])`)
	inferEnvStagingRe = regexp.MustCompile(`(^|[-_\.])(stg|stage|staging)($|[-_\.])`)
	inferEnvDevRe     = regexp.MustCompile(`(^|[-_\.])(dev|development)($|[-_\.])`)
	inferEnvTestRe    = regexp.MustCompile(`(^|[-_\.])(qa|test|testing|uat)($|[-_\.])`)
)

// inferEnvFromService mirrors _infer_env_from_service.
func inferEnvFromService(serviceName string) string {
	name := strings.ToLower(strings.TrimSpace(serviceName))
	if name == "" {
		return ""
	}
	if inferEnvProdRe.MatchString(name) {
		return "production"
	}
	if inferEnvStagingRe.MatchString(name) {
		return "staging"
	}
	if inferEnvDevRe.MatchString(name) {
		return "development"
	}
	if inferEnvTestRe.MatchString(name) {
		return "test"
	}
	return ""
}

// listTagCandidateServices mirrors _list_tag_candidate_services.
func listTagCandidateServices(db *ChDbConnection) ([]string, error) {
	res, err := db.Execute(
		"SELECT DISTINCT ServiceName FROM (" +
			"  SELECT ServiceName FROM otel_logs " +
			"  UNION DISTINCT SELECT ServiceName FROM otel_traces " +
			"  UNION DISTINCT SELECT ServiceName FROM hyperdx_sessions" +
			") WHERE ServiceName != '' ORDER BY ServiceName")
	if err != nil {
		return nil, err
	}
	services := []string{}
	for _, row := range res.Fetchall() {
		services = append(services, rowString(row[res.Cols[0]]))
	}
	return services, nil
}

// tagRuleDedupeKey is the tuple used to dedupe auto tag-rule candidates against
// existing rules.
type tagRuleDedupeKey struct {
	recordTypes, matchField, matchOperator, matchValue, matchAttrKey, tagKey, tagValue string
}

// buildAutoTagRuleCandidates mirrors _build_auto_tag_rule_candidates.
// recordTypes nil/empty falls back to all record types.
func buildAutoTagRuleCandidates(
	db *ChDbConnection,
	hours int,
	minCount int,
	serviceFilter string,
	recordTypes []string,
) ([]map[string]any, map[string]int, error) {
	allTypes := []string{"log", "trace", "error", "ai", "rum"}
	selected := map[string]bool{}
	source := recordTypes
	if len(source) == 0 {
		source = allTypes
	}
	for _, t := range source {
		for _, known := range allTypes {
			if t == known {
				selected[t] = true
			}
		}
	}
	if len(selected) == 0 {
		for _, t := range allTypes {
			selected[t] = true
		}
	}

	existingRules, err := loadTagRules(db)
	if err != nil {
		return nil, nil, err
	}
	existingKeys := map[tagRuleDedupeKey]bool{}
	for _, rule := range existingRules {
		types := []string{}
		if ruleTypes, ok := rule["record_types"].([]string); ok {
			for _, t := range ruleTypes {
				if strings.TrimSpace(t) != "" {
					types = append(types, strings.TrimSpace(t))
				}
			}
		}
		sort.Strings(types)
		existingKeys[tagRuleDedupeKey{
			recordTypes:   strings.Join(types, ","),
			matchField:    rowString(rule["match_field"]),
			matchOperator: rowString(rule["match_operator"]),
			matchValue:    rowString(rule["match_value"]),
			matchAttrKey:  rowString(rule["match_attr_key"]),
			tagKey:        rowString(rule["tag_key"]),
			tagValue:      rowString(rule["tag_value"]),
		}] = true
	}

	candidates := []map[string]any{}
	examined := 0
	skippedExisting := 0
	skippedInvalid := 0

	appendCandidate := func(recordType, name, matchField, matchOperator, matchValue, tagKey, tagValue string, pointCount int, matchAttrKey string) {
		if strings.TrimSpace(matchValue) == "" || strings.TrimSpace(tagKey) == "" || strings.TrimSpace(tagValue) == "" {
			skippedInvalid++
			return
		}
		ruleKey := tagRuleDedupeKey{
			recordTypes:   recordType,
			matchField:    matchField,
			matchOperator: matchOperator,
			matchValue:    matchValue,
			matchAttrKey:  matchAttrKey,
			tagKey:        tagKey,
			tagValue:      tagValue,
		}
		if existingKeys[ruleKey] {
			skippedExisting++
			return
		}
		candidates = append(candidates, map[string]any{
			"name":           name,
			"record_types":   []string{recordType},
			"match_field":    matchField,
			"match_operator": matchOperator,
			"match_value":    matchValue,
			"match_attr_key": matchAttrKey,
			"tag_key":        tagKey,
			"tag_value":      tagValue,
			"point_count":    pointCount,
		})
	}

	whereService := ""
	baseParams := []any{hours}
	if serviceFilter != "" {
		whereService = " AND ServiceName = ?"
		baseParams = append(baseParams, serviceFilter)
	}

	if selected["log"] {
		res, err := db.Execute(
			"SELECT ServiceName, count() AS c FROM otel_logs "+
				"WHERE Timestamp >= now() - INTERVAL ? HOUR AND ServiceName != ''"+
				whereService+" "+
				"GROUP BY ServiceName HAVING c >= ? ORDER BY c DESC",
			append(append([]any{}, baseParams...), minCount)...,
		)
		if err != nil {
			return nil, nil, err
		}
		rows := res.Fetchall()
		examined += len(rows)
		for _, row := range rows {
			service := rowString(row["ServiceName"])
			count := coerceInt(row["c"])
			inferredEnv := inferEnvFromService(service)
			if inferredEnv != "" {
				appendCandidate("log", fmt.Sprintf("log env=%s", inferredEnv),
					"service_name", "contains", service, "env", inferredEnv, count, "")
				continue
			}
			appendCandidate("log", fmt.Sprintf("log service=%s", service),
				"service_name", "eq", service, "service", service, count, "")
		}
	}

	if selected["trace"] {
		res, err := db.Execute(
			"SELECT ServiceName, count() AS c FROM otel_traces "+
				"WHERE Timestamp >= now() - INTERVAL ? HOUR AND ScopeName != 'sobs-ai' AND ServiceName != ''"+
				whereService+" "+
				"GROUP BY ServiceName HAVING c >= ? ORDER BY c DESC",
			append(append([]any{}, baseParams...), minCount)...,
		)
		if err != nil {
			return nil, nil, err
		}
		rows := res.Fetchall()
		examined += len(rows)
		for _, row := range rows {
			service := rowString(row["ServiceName"])
			count := coerceInt(row["c"])
			inferredEnv := inferEnvFromService(service)
			if inferredEnv != "" {
				appendCandidate("trace", fmt.Sprintf("trace env=%s", inferredEnv),
					"service_name", "contains", service, "env", inferredEnv, count, "")
				continue
			}
			appendCandidate("trace", fmt.Sprintf("trace service=%s", service),
				"service_name", "eq", service, "service", service, count, "")
		}
	}

	if selected["error"] {
		res, err := db.Execute(
			"SELECT coalesce(LogAttributes['exception.type'], '') AS ExceptionType, count() AS c "+
				"FROM otel_logs "+
				"WHERE Timestamp >= now() - INTERVAL ? HOUR "+
				"AND (EventName = 'exception' OR SeverityNumber >= 17 OR SeverityText IN ('ERROR','CRITICAL','FATAL'))"+
				whereService+" "+
				"GROUP BY ExceptionType HAVING c >= ? ORDER BY c DESC",
			append(append([]any{}, baseParams...), minCount)...,
		)
		if err != nil {
			return nil, nil, err
		}
		rows := res.Fetchall()
		examined += len(rows)
		for _, row := range rows {
			exceptionType := strings.TrimSpace(rowString(row["ExceptionType"]))
			if exceptionType == "" {
				skippedInvalid++
				continue
			}
			count := coerceInt(row["c"])
			appendCandidate("error", fmt.Sprintf("error type=%s", autoTagSlug(exceptionType, "error")),
				"attribute", "eq", exceptionType, "error_type", autoTagSlug(exceptionType, "error"),
				count, "exception.type")
		}
	}

	if selected["ai"] {
		res, err := db.Execute(
			"SELECT coalesce(SpanAttributes['gen_ai.provider.name'], '') AS Provider, count() AS c "+
				"FROM otel_traces "+
				"WHERE Timestamp >= now() - INTERVAL ? HOUR AND ScopeName = 'sobs-ai'"+
				whereService+" "+
				"GROUP BY Provider HAVING c >= ? ORDER BY c DESC",
			append(append([]any{}, baseParams...), minCount)...,
		)
		if err != nil {
			return nil, nil, err
		}
		rows := res.Fetchall()
		examined += len(rows)
		for _, row := range rows {
			provider := strings.TrimSpace(rowString(row["Provider"]))
			if provider == "" {
				skippedInvalid++
				continue
			}
			count := coerceInt(row["c"])
			appendCandidate("ai", fmt.Sprintf("ai provider=%s", autoTagSlug(provider, "provider")),
				"attribute", "eq", provider, "ai_provider", autoTagSlug(provider, "provider"),
				count, "gen_ai.provider.name")
		}
	}

	if selected["rum"] {
		res, err := db.Execute(
			"SELECT EventName, count() AS c FROM hyperdx_sessions "+
				"WHERE Timestamp >= now() - INTERVAL ? HOUR AND EventName != ''"+
				whereService+" "+
				"GROUP BY EventName HAVING c >= ? ORDER BY c DESC",
			append(append([]any{}, baseParams...), minCount)...,
		)
		if err != nil {
			return nil, nil, err
		}
		rows := res.Fetchall()
		examined += len(rows)
		for _, row := range rows {
			eventName := rowString(row["EventName"])
			count := coerceInt(row["c"])
			appendCandidate("rum", fmt.Sprintf("rum event=%s", autoTagSlug(eventName, "event")),
				"event_type", "eq", eventName, "rum_event", autoTagSlug(eventName, "event"), count, "")
		}
	}

	candidatePointCount := func(candidate map[string]any) int {
		return coerceInt(candidate["point_count"])
	}

	// Python: sort(key=(point_count, name), reverse=True) — descending tuple order.
	sort.SliceStable(candidates, func(i, j int) bool {
		ci, cj := candidatePointCount(candidates[i]), candidatePointCount(candidates[j])
		if ci != cj {
			return ci > cj
		}
		return rowString(candidates[i]["name"]) > rowString(candidates[j]["name"])
	})
	return candidates, map[string]int{
		"examined": examined,
		"existing": skippedExisting,
		"invalid":  skippedInvalid,
	}, nil
}

// buildAutoDashboardChartCandidates mirrors _build_auto_dashboard_chart_candidates.
func buildAutoDashboardChartCandidates(
	rules []map[string]any,
	serviceFilter string,
	hours int,
) []map[string]any {
	candidates := []map[string]any{}
	titleCounts := map[string]int{}
	for _, rule := range rules {
		source := strings.TrimSpace(rowString(rule["source"]))
		signal := strings.TrimSpace(rowString(rule["signal"]))
		if source == "" || signal == "" {
			continue
		}

		ruleService := strings.TrimSpace(rowString(rule["service"]))
		if serviceFilter != "" && ruleService != "" && ruleService != serviceFilter {
			continue
		}

		attrFp := strings.TrimSpace(rowString(rule["attr_fp"]))
		whereParts := []string{
			fmt.Sprintf("SignalSource = %s", sqlLiteral(source)),
			fmt.Sprintf("SignalName = %s", sqlLiteral(signal)),
			fmt.Sprintf("time >= now() - INTERVAL %d HOUR", hours),
		}
		if ruleService != "" {
			whereParts = append(whereParts, fmt.Sprintf("ServiceName = %s", sqlLiteral(ruleService)))
		}
		if attrFp != "" {
			whereParts = append(whereParts, fmt.Sprintf("AttrFingerprint = %s", sqlLiteral(attrFp)))
		}

		sql := "SELECT time, " +
			"ServiceName AS service, " +
			"SignalSource AS source, " +
			"SignalName AS signal, " +
			"AttrFingerprint AS attr_fp, " +
			"value, " +
			"SampleCount AS sample_count, " +
			"baseline_mean, " +
			"baseline_lower, " +
			"baseline_upper, " +
			"anomaly_state, " +
			"anomaly_score " +
			"FROM v_derived_signals_anomaly " +
			fmt.Sprintf("WHERE %s ", strings.Join(whereParts, " AND ")) +
			"ORDER BY time"

		baseTitle := strings.TrimSpace(rowString(rule["name"]))
		if baseTitle == "" {
			baseTitle = fmt.Sprintf("%s/%s", source, signal)
		}
		titleIndex := titleCounts[baseTitle]
		titleCounts[baseTitle] = titleIndex + 1
		title := baseTitle
		if titleIndex != 0 {
			title = fmt.Sprintf("%s (%d)", baseTitle, titleIndex+1)
		}

		ruleType := rowString(rule["rule_type"])
		if rule["rule_type"] == nil {
			ruleType = "threshold"
		}
		candidates = append(candidates, map[string]any{
			"title":      title,
			"rule_name":  rowString(rule["name"]),
			"rule_type":  ruleType,
			"source":     source,
			"signal":     signal,
			"service":    ruleService,
			"attr_fp":    attrFp,
			"chart_type": "derived_signal_overlay",
			"query":      sql,
		})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if sa, sb := rowString(a["service"]), rowString(b["service"]); sa != sb {
			return sa < sb
		}
		if sa, sb := rowString(a["source"]), rowString(b["source"]); sa != sb {
			return sa < sb
		}
		if sa, sb := rowString(a["signal"]), rowString(b["signal"]); sa != sb {
			return sa < sb
		}
		return rowString(a["title"]) < rowString(b["title"])
	})
	return candidates
}

// loadAnomalyRules mirrors _load_anomaly_rules.
func loadAnomalyRules(db *ChDbConnection) ([]map[string]any, error) {
	res, err := db.Execute(
		"SELECT Id, Name, RuleType, SignalSource, SignalName, ServiceName, AttrFingerprint, Comparator, " +
			"WarningThreshold, CriticalThreshold, SecondarySignalSource, SecondarySignalName, " +
			"SecondaryComparator, SecondaryWarningThreshold, SecondaryCriticalThreshold, MinSampleCount, " +
			"SeasonalBucketsJson " +
			"FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		return nil, err
	}
	rules := []map[string]any{}
	for _, row := range res.Fetchall() {
		ruleType := rowString(row["RuleType"])
		if ruleType == "" {
			ruleType = "threshold"
		}
		secondaryComparator := rowString(row["SecondaryComparator"])
		if secondaryComparator == "" {
			secondaryComparator = "gt"
		}
		warningThreshold, _ := coerceFloat(row["WarningThreshold"])
		criticalThreshold, _ := coerceFloat(row["CriticalThreshold"])
		secondaryWarning, _ := coerceFloat(row["SecondaryWarningThreshold"])
		secondaryCritical, _ := coerceFloat(row["SecondaryCriticalThreshold"])
		rules = append(rules, map[string]any{
			"id":                           rowString(row["Id"]),
			"name":                         rowString(row["Name"]),
			"rule_type":                    ruleType,
			"source":                       rowString(row["SignalSource"]),
			"signal":                       rowString(row["SignalName"]),
			"service":                      rowString(row["ServiceName"]),
			"attr_fp":                      rowString(row["AttrFingerprint"]),
			"comparator":                   rowString(row["Comparator"]),
			"warning_threshold":            warningThreshold,
			"critical_threshold":           criticalThreshold,
			"secondary_source":             rowString(row["SecondarySignalSource"]),
			"secondary_signal":             rowString(row["SecondarySignalName"]),
			"secondary_comparator":         secondaryComparator,
			"secondary_warning_threshold":  secondaryWarning,
			"secondary_critical_threshold": secondaryCritical,
			"min_sample_count":             coerceInt(row["MinSampleCount"]),
			"seasonal_buckets_json":        rowString(row["SeasonalBucketsJson"]),
		})
	}
	return rules, nil
}

// ---------------------------------------------------------------------------
// Tag rules helpers
// ---------------------------------------------------------------------------

var tagRuleFields = []string{"service_name", "severity", "body", "span_name", "event_type", "attribute"}
var tagRuleOperators = []string{"eq", "contains", "regex"}
var tagRuleRecordTypes = []string{"log", "trace", "error", "ai", "rum", "all"}

// recordIdForLog computes a stable record ID for a log/rum/error event.
func recordIdForLog(ts, service, traceId, spanId string) string {
	key := fmt.Sprintf("%s|%s|%s|%s", service, ts, traceId, spanId)
	sum := md5.Sum([]byte(key))
	return hex.EncodeToString(sum[:])
}

// recordIdForSpan computes a stable record ID for a trace span.
func recordIdForSpan(traceId, spanId string) string {
	key := fmt.Sprintf("%s|%s", traceId, spanId)
	sum := md5.Sum([]byte(key))
	return hex.EncodeToString(sum[:])
}

// parseTagRuleConditionsJson mirrors _parse_tag_rule_conditions_json:
// best-effort decode for ConditionsJson with safe fallback semantics.
func parseTagRuleConditionsJson(raw any) []map[string]string {
	text := strings.TrimSpace(rowString(raw))
	if text == "" {
		return []map[string]string{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return []map[string]string{}
	}
	items, ok := parsed.([]any)
	if !ok {
		return []map[string]string{}
	}

	normalized := []map[string]string{}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		normalized = append(normalized, map[string]string{
			"match_field":    rowString(m["match_field"]),
			"match_operator": rowString(m["match_operator"]),
			"match_value":    rowString(m["match_value"]),
			"match_attr_key": rowString(m["match_attr_key"]),
		})
	}
	return normalized
}

// loadTagRules mirrors _load_tag_rules: load all active tag rules.
func loadTagRules(db *ChDbConnection) ([]map[string]any, error) {
	res, err := db.Execute(
		"SELECT Id, Name, RecordTypes, MatchField, MatchOperator, MatchValue, " +
			"MatchAttrKey, TagKey, TagValue, ConditionsJson " +
			"FROM sobs_tag_rules FINAL WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		return nil, err
	}
	loaded := []map[string]any{}
	for _, row := range res.Fetchall() {
		conditions := parseTagRuleConditionsJson(row["ConditionsJson"])
		// Backward compatibility for pre-ConditionsJson rules.
		if len(conditions) == 0 && strings.TrimSpace(rowString(row["MatchField"])) != "" {
			matchOperator := rowString(row["MatchOperator"])
			if matchOperator == "" {
				matchOperator = "eq"
			}
			conditions = []map[string]string{
				{
					"match_field":    rowString(row["MatchField"]),
					"match_operator": matchOperator,
					"match_value":    rowString(row["MatchValue"]),
					"match_attr_key": rowString(row["MatchAttrKey"]),
				},
			}
		}

		recordTypes := []string{}
		for _, t := range strings.Split(rowString(row["RecordTypes"]), ",") {
			if strings.TrimSpace(t) != "" {
				recordTypes = append(recordTypes, strings.TrimSpace(t))
			}
		}

		loaded = append(loaded, map[string]any{
			"id":             rowString(row["Id"]),
			"name":           rowString(row["Name"]),
			"record_types":   recordTypes,
			"match_field":    rowString(row["MatchField"]),
			"match_operator": rowString(row["MatchOperator"]),
			"match_value":    rowString(row["MatchValue"]),
			"match_attr_key": rowString(row["MatchAttrKey"]),
			"tag_key":        rowString(row["TagKey"]),
			"tag_value":      rowString(row["TagValue"]),
			"conditions":     conditions,
		})
	}
	return loaded, nil
}

// matchTagRule mirrors _match_tag_rule: returns true if the tag rule matches
// the given record fields.
//
// For composite rules (non-empty "conditions" list), ALL conditions must
// match. For simple rules the single match_field/match_operator/match_value
// triple is evaluated as before.
func matchTagRule(
	rule map[string]any,
	recordType string,
	service string,
	severity string,
	body string,
	attrs map[string]any,
	spanName string,
	eventType string,
) bool {
	ruleTypes, _ := rule["record_types"].([]string)
	if len(ruleTypes) > 0 {
		hasAll := false
		hasType := false
		for _, t := range ruleTypes {
			if t == "all" {
				hasAll = true
			}
			if t == recordType {
				hasType = true
			}
		}
		if !hasAll && !hasType {
			return false
		}
	}

	conditions, _ := rule["conditions"].([]map[string]string)
	if len(conditions) > 0 {
		// Composite rule – every condition in the list must match.
		for _, cond := range conditions {
			if !matchSingleCondition(cond, service, severity, body, attrs, spanName, eventType) {
				return false
			}
		}
		return true
	}

	// Simple (legacy) rule – evaluate the single condition stored directly on
	// the rule dict.
	return matchSingleCondition(
		map[string]string{
			"match_field":    rowString(rule["match_field"]),
			"match_operator": rowString(rule["match_operator"]),
			"match_value":    rowString(rule["match_value"]),
			"match_attr_key": rowString(rule["match_attr_key"]),
		},
		service,
		severity,
		body,
		attrs,
		spanName,
		eventType,
	)
}

// matchSingleCondition mirrors _match_single_condition: evaluate a single
// condition dict against the record fields.
func matchSingleCondition(
	cond map[string]string,
	service string,
	severity string,
	body string,
	attrs map[string]any,
	spanName string,
	eventType string,
) bool {
	field := cond["match_field"]
	var value string
	switch field {
	case "service_name":
		value = service
	case "severity":
		value = severity
	case "body":
		value = body
	case "span_name":
		value = spanName
	case "event_type":
		value = eventType
	case "attribute":
		if attrs != nil {
			if v, ok := attrs[cond["match_attr_key"]]; ok {
				value = rowString(v)
			}
		}
	default:
		value = ""
	}

	operator := cond["match_operator"]
	matchValue := cond["match_value"]
	switch operator {
	case "eq":
		return value == matchValue
	case "contains":
		return strings.Contains(strings.ToLower(value), strings.ToLower(matchValue))
	case "regex":
		re, err := regexp.Compile(matchValue)
		if err != nil {
			// PORT-NOTE: Python catches re.error; Go RE2 rejects some
			// Python-only constructs (lookbehind etc.) — both yield false.
			return false
		}
		return re.MatchString(value)
	}
	return false
}

// tagRuleAttributeKeySuggestions mirrors _tag_rule_attribute_key_suggestions.
func tagRuleAttributeKeySuggestions(db *ChDbConnection, queryText string, limit int) ([]string, error) {
	keys := map[string]bool{}
	for _, recordType := range attrKeyRecordTypes {
		cached, err := getCachedAttrKeys(db, recordType)
		if err != nil {
			return nil, err
		}
		for _, k := range cached {
			keys[k] = true
		}
	}

	q := strings.ToLower(strings.TrimSpace(queryText))
	ranked := []string{}
	for k := range keys {
		if k != "" {
			ranked = append(ranked, k)
		}
	}
	// Python: sorted(key=(startswith-rank, contains-rank, k.lower())).
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		aLower, bLower := strings.ToLower(a), strings.ToLower(b)
		aPrefix, bPrefix := 1, 1
		if q != "" && strings.HasPrefix(aLower, q) {
			aPrefix = 0
		}
		if q != "" && strings.HasPrefix(bLower, q) {
			bPrefix = 0
		}
		if aPrefix != bPrefix {
			return aPrefix < bPrefix
		}
		aContains, bContains := 1, 1
		if q != "" && strings.Contains(aLower, q) {
			aContains = 0
		}
		if q != "" && strings.Contains(bLower, q) {
			bContains = 0
		}
		if aContains != bContains {
			return aContains < bContains
		}
		return aLower < bLower
	})
	if q != "" {
		filtered := []string{}
		for _, k := range ranked {
			if strings.Contains(strings.ToLower(k), q) {
				filtered = append(filtered, k)
			}
		}
		ranked = filtered
	}
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked, nil
}

// tagRuleValueSuggestions mirrors _tag_rule_value_suggestions.
func tagRuleValueSuggestions(
	db *ChDbConnection,
	field string,
	operator string,
	queryText string,
	attrKey string,
	limit int,
) ([]string, error) {
	_ = operator // Reserved for future operator-specific ranking.

	fieldName := strings.ToLower(strings.TrimSpace(field))
	q := strings.ToLower(strings.TrimSpace(queryText))

	run := func(sql string, params []any) ([]string, error) {
		res, err := db.Execute(sql, params...)
		if err != nil {
			return nil, err
		}
		out := []string{}
		for _, row := range res.Fetchall() {
			v := strings.TrimSpace(rowString(row[res.Cols[0]]))
			if v == "" {
				continue
			}
			out = append(out, v)
		}
		return out, nil
	}

	switch fieldName {
	case "service_name":
		return run(
			"SELECT value FROM ("+
				"SELECT ServiceName AS value FROM otel_logs WHERE ServiceName != '' "+
				"UNION ALL "+
				"SELECT ServiceName AS value FROM otel_traces WHERE ServiceName != ''"+
				") "+
				"WHERE (? = '' OR positionCaseInsensitive(value, ?) > 0) "+
				"GROUP BY value ORDER BY count() DESC, value LIMIT ?",
			[]any{q, q, limit},
		)
	case "severity":
		return run(
			"SELECT SeverityText FROM otel_logs "+
				"WHERE SeverityText != '' AND (? = '' OR positionCaseInsensitive(SeverityText, ?) > 0) "+
				"GROUP BY SeverityText ORDER BY count() DESC, SeverityText LIMIT ?",
			[]any{q, q, limit},
		)
	case "span_name":
		return run(
			"SELECT SpanName FROM otel_traces "+
				"WHERE SpanName != '' AND (? = '' OR positionCaseInsensitive(SpanName, ?) > 0) "+
				"GROUP BY SpanName ORDER BY count() DESC, SpanName LIMIT ?",
			[]any{q, q, limit},
		)
	case "event_type":
		return run(
			"SELECT value FROM ("+
				"SELECT EventName AS value FROM otel_logs WHERE EventName != '' "+
				"UNION ALL "+
				"SELECT EventName AS value FROM hyperdx_sessions WHERE EventName != ''"+
				") "+
				"WHERE (? = '' OR positionCaseInsensitive(value, ?) > 0) "+
				"GROUP BY value ORDER BY count() DESC, value LIMIT ?",
			[]any{q, q, limit},
		)
	case "body":
		return run(
			"SELECT value FROM ("+
				"SELECT Body AS value FROM otel_logs WHERE Body != '' ORDER BY Timestamp DESC LIMIT 4000"+
				") "+
				"WHERE (? = '' OR positionCaseInsensitive(value, ?) > 0) "+
				"GROUP BY value ORDER BY count() DESC, value LIMIT ?",
			[]any{q, q, limit},
		)
	case "attribute":
		key := strings.TrimSpace(attrKey)
		if key == "" {
			return []string{}, nil
		}
		return run(
			"SELECT value FROM ("+
				"SELECT LogAttributes[?] AS value FROM otel_logs WHERE LogAttributes[?] != '' "+
				"ORDER BY Timestamp DESC LIMIT 2500 "+
				"UNION ALL "+
				"SELECT SpanAttributes[?] AS value FROM otel_traces WHERE SpanAttributes[?] != '' "+
				"ORDER BY Timestamp DESC LIMIT 2500"+
				") "+
				"WHERE value != '' AND (? = '' OR positionCaseInsensitive(value, ?) > 0) "+
				"GROUP BY value ORDER BY count() DESC, value LIMIT ?",
			[]any{key, key, key, key, q, q, limit},
		)
	}

	return []string{}, nil
}

// recordTagKeySuggestions mirrors _record_tag_key_suggestions
// (record_type defaults to "all" in Python).
func recordTagKeySuggestions(db *ChDbConnection, queryText string, limit int, recordType string) ([]string, error) {
	q := strings.ToLower(strings.TrimSpace(queryText))
	rt := strings.ToLower(strings.TrimSpace(recordType))
	if rt == "" {
		rt = "all"
	}
	where := []string{"IsDeleted = 0"}
	params := []any{}
	if rt != "" && rt != "all" {
		where = append(where, "RecordType = ?")
		params = append(params, rt)
	}
	params = append(params, q, q, limit)
	res, err := db.Execute(
		"SELECT TagKey FROM sobs_record_tags FINAL "+
			fmt.Sprintf("WHERE %s ", strings.Join(where, " AND "))+
			"AND (? = '' OR positionCaseInsensitive(TagKey, ?) > 0) "+
			"GROUP BY TagKey ORDER BY count() DESC, TagKey LIMIT ?",
		params...,
	)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, row := range res.Fetchall() {
		v := rowString(row[res.Cols[0]])
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// recordTagValueSuggestions mirrors _record_tag_value_suggestions
// (record_type defaults to "all" in Python).
func recordTagValueSuggestions(db *ChDbConnection, tagKey, queryText string, limit int, recordType string) ([]string, error) {
	key := strings.TrimSpace(tagKey)
	if key == "" {
		return []string{}, nil
	}
	q := strings.ToLower(strings.TrimSpace(queryText))
	rt := strings.ToLower(strings.TrimSpace(recordType))
	if rt == "" {
		rt = "all"
	}
	where := []string{"IsDeleted = 0", "TagKey = ?"}
	params := []any{key}
	if rt != "" && rt != "all" {
		where = append(where, "RecordType = ?")
		params = append(params, rt)
	}
	params = append(params, q, q, limit)
	res, err := db.Execute(
		"SELECT TagValue FROM sobs_record_tags FINAL "+
			fmt.Sprintf("WHERE %s ", strings.Join(where, " AND "))+
			"AND (? = '' OR positionCaseInsensitive(TagValue, ?) > 0) "+
			"GROUP BY TagValue ORDER BY count() DESC, TagValue LIMIT ?",
		params...,
	)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, row := range res.Fetchall() {
		v := rowString(row[res.Cols[0]])
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// notificationConditionServiceSuggestions mirrors _notification_condition_service_suggestions.
func notificationConditionServiceSuggestions(
	db *ChDbConnection,
	queryText string,
	limit int,
	source string,
	signal string,
) ([]string, error) {
	q := strings.ToLower(strings.TrimSpace(queryText))
	src := strings.ToLower(strings.TrimSpace(source))
	sig := strings.TrimSpace(signal)
	res, err := db.Execute(
		"SELECT ServiceName FROM v_derived_signals_1m "+
			"WHERE ServiceName != '' "+
			"AND (? = '' OR SignalSource = ?) "+
			"AND (? = '' OR SignalName = ?) "+
			"AND (? = '' OR positionCaseInsensitive(ServiceName, ?) > 0) "+
			"GROUP BY ServiceName ORDER BY count() DESC, ServiceName LIMIT ?",
		src, src, sig, sig, q, q, limit,
	)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, row := range res.Fetchall() {
		v := rowString(row[res.Cols[0]])
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// tagRuleAttrs coerces a row attribute payload (LogAttributes/SpanAttributes)
// into a map[string]any, mirroring `isinstance(attrs, dict)` checks for the
// concrete map shapes used by the ingest code.
func tagRuleAttrs(value any) map[string]any {
	switch v := value.(type) {
	case map[string]any:
		return v
	case map[string]string:
		out := make(map[string]any, len(v))
		for k, s := range v {
			out[k] = s
		}
		return out
	}
	return nil
}

// applyTagRules mirrors _apply_tag_rules: apply tag rules to ingested rows and
// write matching tags to sobs_record_tags.
func applyTagRules(db *ChDbConnection, recordType string, rowsData []Row, rules []map[string]any) error {
	if len(rules) == 0 || len(rowsData) == 0 {
		return nil
	}
	endSpan := telemetrySpan("sobs.rules.evaluate", map[string]any{
		"rule.count": len(rules), "event.count": len(rowsData),
	})
	defer endSpan()
	tagRows := []Row{}
	version := time.Now().UnixMilli()
	for _, row := range rowsData {
		service := rowString(row["ServiceName"])
		severity := rowString(row["SeverityText"])
		body := rowString(row["Body"])
		// Python: attrs = row.get("LogAttributes") or row.get("SpanAttributes") or {}
		attrs := tagRuleAttrs(row["LogAttributes"])
		if len(attrs) == 0 {
			attrs = tagRuleAttrs(row["SpanAttributes"])
		}
		if attrs == nil {
			attrs = map[string]any{}
		}
		spanName := rowString(row["SpanName"])
		eventType := rowString(row["EventName"])
		traceId := rowString(row["TraceId"])
		spanId := rowString(row["SpanId"])
		ts := rowString(row["Timestamp"])

		var recordId string
		if recordType == "trace" || recordType == "ai" {
			recordId = recordIdForSpan(traceId, spanId)
		} else {
			recordId = recordIdForLog(ts, service, traceId, spanId)
		}

		// Keep one value per tag key per record. If multiple rules match the same
		// key, last matching rule wins (deterministic by rule order).
		matchedByKey := map[string]string{}
		matchedKeyOrder := []string{}
		for _, rule := range rules {
			if matchTagRule(rule, recordType, service, severity, body, attrs, spanName, eventType) {
				tagKey := rowString(rule["tag_key"])
				if _, seen := matchedByKey[tagKey]; !seen {
					matchedKeyOrder = append(matchedKeyOrder, tagKey)
				}
				matchedByKey[tagKey] = rowString(rule["tag_value"])
			}
		}
		for _, tagKey := range matchedKeyOrder {
			tagRows = append(tagRows, Row{
				"RecordType": recordType,
				"RecordId":   recordId,
				"TagKey":     tagKey,
				"TagValue":   matchedByKey[tagKey],
				"IsAuto":     1,
				"IsDeleted":  0,
				"Version":    version,
			})
			version++
		}
	}
	if len(tagRows) > 0 {
		if _, err := insertRowsJsonEachRow(db, "sobs_record_tags", tagRows); err != nil {
			return err
		}
	}
	return nil
}

// getRecordTags mirrors _get_record_tags: return all active tags for a given record.
func getRecordTags(db *ChDbConnection, recordType, recordId string) ([]map[string]any, error) {
	res, err := db.Execute(
		"SELECT TagKey, TagValue, IsAuto "+
			"FROM sobs_record_tags FINAL "+
			"WHERE RecordType = ? AND RecordId = ? AND IsDeleted = 0 "+
			"ORDER BY TagKey",
		recordType, recordId,
	)
	if err != nil {
		return nil, err
	}
	tags := []map[string]any{}
	for _, row := range res.Fetchall() {
		tags = append(tags, map[string]any{
			"key":     rowString(row["TagKey"]),
			"value":   rowString(row["TagValue"]),
			"is_auto": coerceInt(row["IsAuto"]) != 0,
		})
	}
	return tags, nil
}

// getServiceTags mirrors _get_service_tags: return distinct tag values applied
// to a service's records in the last N hours (hours defaults to 24).
func getServiceTags(db *ChDbConnection, recordType, service string, hoursOpt ...int) []string {
	hours := 24
	if len(hoursOpt) > 0 {
		hours = hoursOpt[0]
	}
	res, err := db.Execute(
		"SELECT DISTINCT concat(rt.TagKey, ':', rt.TagValue) AS tag "+
			"FROM sobs_record_tags rt FINAL "+
			"WHERE rt.RecordType = ? AND rt.IsDeleted = 0 "+
			"AND rt.RecordId IN ("+
			"  SELECT MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)) "+
			"  FROM otel_logs "+
			"  WHERE ServiceName = ? AND Timestamp >= now() - INTERVAL ? HOUR "+
			") "+
			"ORDER BY tag",
		recordType, service, hours,
	)
	if err != nil {
		return []string{}
	}
	tags := []string{}
	for _, r := range res.Fetchall() {
		tags = append(tags, rowString(r["tag"]))
	}
	return tags
}

// getDefTagsForService mirrors _get_def_tags_for_service: return distinct
// auto-tags for a service from all record types (last 24 h).
func getDefTagsForService(db *ChDbConnection, service string) []string {
	res, err := db.Execute(
		"SELECT DISTINCT concat(TagKey,'=',TagValue) AS tag "+
			"FROM sobs_record_tags FINAL "+
			"WHERE IsDeleted = 0 "+
			"AND RecordId IN ("+
			"  SELECT MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)) "+
			"  FROM otel_logs WHERE ServiceName = ? AND Timestamp >= now() - INTERVAL 24 HOUR"+
			") ORDER BY tag",
		service,
	)
	if err != nil {
		return []string{}
	}
	tags := []string{}
	for _, r := range res.Fetchall() {
		tags = append(tags, rowString(r["tag"]))
	}
	return tags
}

// getSignalHealthByService mirrors _get_signal_health_by_service: return worst
// effective_state per service for derived signals in the last `hours` hours
// (hours defaults to 24).
func getSignalHealthByService(db *ChDbConnection, hoursOpt ...int) []map[string]any {
	hours := 24
	if len(hoursOpt) > 0 {
		hours = hoursOpt[0]
	}
	res, err := db.Execute(
		"SELECT ServiceName, SignalSource, SignalName, AttrFingerprint, "+
			"argMax(value, time) AS value, argMax(SampleCount, time) AS SampleCount "+
			"FROM v_derived_signals_anomaly "+
			"WHERE time >= now() - INTERVAL ? HOUR "+
			"GROUP BY ServiceName, SignalSource, SignalName, AttrFingerprint",
		hours,
	)
	if err != nil {
		return []map[string]any{}
	}
	rows := res.Fetchall()
	if len(rows) == 0 {
		return []map[string]any{}
	}
	dicts := make([]Row, 0, len(rows))
	for _, r := range rows {
		d := Row{}
		for k, v := range r {
			d[k] = v
		}
		dicts = append(dicts, d)
	}
	rules, err := loadAnomalyRules(db)
	if err != nil {
		// PORT-NOTE: Python would raise here (500). With a single return value
		// we degrade to an empty result like the surrounding try/except.
		logger.Debug("signal health: failed to load anomaly rules", "error", err)
		return []map[string]any{}
	}
	annotateRowsWithRules(
		dicts,
		rules,
		"SignalSource",
		"SignalName",
		"ServiceName",
		"AttrFingerprint",
		"value",
		"SampleCount",
		"",
	)
	serviceWorst := map[string]int{}
	serviceCount := map[string]int{}
	for _, row := range dicts {
		svc := rowString(row["ServiceName"])
		rank := anomalySeverityRank[rowString(row["effective_state"])]
		if existing, ok := serviceWorst[svc]; !ok || rank > existing {
			serviceWorst[svc] = rank
		}
		serviceCount[svc]++
	}
	rankToState := map[int]string{}
	for k, v := range anomalySeverityRank {
		rankToState[v] = k
	}
	out := []map[string]any{}
	for svc := range serviceWorst {
		worstState, ok := rankToState[serviceWorst[svc]]
		if !ok {
			worstState = "normal"
		}
		out = append(out, map[string]any{
			"service":      svc,
			"worst_state":  worstState,
			"signal_count": serviceCount[svc],
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri := anomalySeverityRank[rowString(out[i]["worst_state"])]
		rj := anomalySeverityRank[rowString(out[j]["worst_state"])]
		if ri != rj {
			return ri > rj
		}
		return rowString(out[i]["service"]) < rowString(out[j]["service"])
	})
	return out
}

// ruleMatchesSeries mirrors _rule_matches_series.
func ruleMatchesSeries(rule map[string]any, source, signal, service, attrFp string) bool {
	if rowString(rule["source"]) != source {
		return false
	}
	if rowString(rule["signal"]) != signal {
		return false
	}
	ruleService := rowString(rule["service"])
	if ruleService != "" && ruleService != service {
		return false
	}
	ruleAttrFp := rowString(rule["attr_fp"])
	if ruleAttrFp != "" && ruleAttrFp != attrFp {
		return false
	}
	return true
}

// ruleReasonNumber renders a float like Python's repr (5.0 → "5.0").
func ruleReasonNumber(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.ContainsAny(s, ".eE") && !strings.Contains(s, "inf") && !strings.Contains(s, "NaN") {
		s += ".0"
	}
	return s
}

// evaluateThresholdCondition mirrors _evaluate_threshold_condition; returns
// nil where Python returns None.
func evaluateThresholdCondition(
	name string,
	comparator string,
	warningThreshold any,
	criticalThreshold any,
	value any,
	sampleCount any,
	minSampleCount any,
) map[string]any {
	valueNum, ok := coerceFloat(value)
	if !ok {
		return nil
	}
	sampleCountFloat, ok := coerceFloat(sampleCount)
	if !ok {
		return nil
	}
	sampleCountNum := int(sampleCountFloat)

	minSamples := coerceInt(minSampleCount)
	if sampleCountNum < minSamples {
		return nil
	}

	warning, _ := coerceFloat(warningThreshold)
	critical, _ := coerceFloat(criticalThreshold)

	state := "normal"
	var triggeredThreshold *float64
	if comparator == "gt" {
		if valueNum >= critical {
			state = "outlier"
			triggeredThreshold = &critical
		} else if valueNum >= warning {
			state = "warning"
			triggeredThreshold = &warning
		}
	} else if comparator == "lt" {
		if valueNum <= critical {
			state = "outlier"
			triggeredThreshold = &critical
		} else if valueNum <= warning {
			state = "warning"
			triggeredThreshold = &warning
		}
	}

	if state == "normal" || triggeredThreshold == nil {
		return nil
	}

	operator := "<="
	if comparator == "gt" {
		operator = ">="
	}
	// PORT-NOTE: Python round() uses banker's rounding; math.Round rounds
	// half away from zero. Difference only affects the display string.
	rounded := math.Round(valueNum*10000) / 10000
	return map[string]any{
		"rule_state": state,
		"rule_reason": fmt.Sprintf("%s: value %s %s %s",
			name, ruleReasonNumber(rounded), operator, ruleReasonNumber(*triggeredThreshold)),
	}
}

// evaluateThresholdRule mirrors _evaluate_threshold_rule.
func evaluateThresholdRule(rule map[string]any, value any, sampleCount any) map[string]any {
	evaluation := evaluateThresholdCondition(
		rowString(rule["name"]),
		thresholdComparator(rule, "comparator"),
		rule["warning_threshold"],
		rule["critical_threshold"],
		value,
		sampleCount,
		ruleMinSampleCount(rule),
	)
	if evaluation == nil {
		return nil
	}
	result := map[string]any{
		"rule_id":   rowString(rule["id"]),
		"rule_name": rowString(rule["name"]),
	}
	for k, v := range evaluation {
		result[k] = v
	}
	return result
}

// thresholdComparator mirrors str(rule.get(key, "gt")).
func thresholdComparator(rule map[string]any, key string) string {
	if v, ok := rule[key]; ok {
		return rowString(v)
	}
	return "gt"
}

// ruleMinSampleCount mirrors rule.get("min_sample_count", 1).
func ruleMinSampleCount(rule map[string]any) any {
	if v, ok := rule["min_sample_count"]; ok {
		return v
	}
	return 1
}

// evaluateSeasonalRule mirrors _evaluate_seasonal_rule.
//
// Evaluate a *seasonal* rule against *value* using per-bucket thresholds.
//
// The bucket key is derived from *timeValue* according to the strategy stored
// in `seasonal_buckets_json`. When no matching bucket is found, the rule
// falls back to the global warning_threshold / critical_threshold so that
// evaluation never silently skips a data point.
func evaluateSeasonalRule(rule map[string]any, value any, sampleCount any, timeValue any) map[string]any {
	bucketsJson := rowString(rule["seasonal_buckets_json"])
	warningThreshold, ok := coerceFloat(rule["warning_threshold"])
	if !ok {
		warningThreshold = 0.0
	}
	criticalThreshold, ok := coerceFloat(rule["critical_threshold"])
	if !ok {
		criticalThreshold = 0.0
	}
	isSeasonal := false

	if bucketsJson != "" {
		var bucketsData map[string]any
		if err := json.Unmarshal([]byte(bucketsJson), &bucketsData); err == nil && bucketsData != nil {
			strategy := "hour_of_day"
			if v, exists := bucketsData["strategy"]; exists {
				strategy = rowString(v)
			}
			buckets, _ := bucketsData["buckets"].(map[string]any)
			if len(buckets) > 0 && timeValue != nil {
				timeStr := strings.TrimSpace(rowString(timeValue))
				// Backend timestamps are UTC; treat naive values as UTC and
				// normalize offset-aware values to UTC before bucket lookup.
				if dt, err := parseIsoTimestamp(strings.ReplaceAll(timeStr, " ", "T")); err == nil {
					dt = dt.UTC()
					var bucketKey string
					if strategy == "day_of_week" {
						bucketKey = strconv.Itoa((int(dt.Weekday())+6)%7 + 1) // 1 (Mon) … 7 (Sun)
					} else {
						bucketKey = strconv.Itoa(dt.Hour()) // 0 … 23
					}
					if bucket, okB := buckets[bucketKey].(map[string]any); okB && len(bucket) > 0 {
						wNum, wOk := warningThreshold, true
						if raw, exists := bucket["warning"]; exists {
							wNum, wOk = coerceFloat(raw)
						}
						cNum, cOk := criticalThreshold, true
						if raw, exists := bucket["critical"]; exists {
							cNum, cOk = coerceFloat(raw)
						}
						if wOk && cOk {
							warningThreshold = wNum
							criticalThreshold = cNum
							isSeasonal = true
						}
					}
				}
			}
		}
	}

	evaluation := evaluateThresholdCondition(
		rowString(rule["name"]),
		thresholdComparator(rule, "comparator"),
		warningThreshold,
		criticalThreshold,
		value,
		sampleCount,
		ruleMinSampleCount(rule),
	)
	if evaluation == nil {
		return nil
	}
	result := map[string]any{
		"rule_id":       rowString(rule["id"]),
		"rule_name":     rowString(rule["name"]),
		"rule_seasonal": isSeasonal,
	}
	for k, v := range evaluation {
		result[k] = v
	}
	return result
}

// seriesRuleKey is the (service, attr_fp, source, signal) lookup tuple used by
// composite rule evaluation.
type seriesRuleKey struct {
	service, attrFp, source, signal string
}

// seriesRuleTimedKey extends seriesRuleKey with a time component.
type seriesRuleTimedKey struct {
	service, attrFp, source, signal, time string
}

// buildSeriesRuleLookups mirrors _build_series_rule_lookups
// (timeKey "" means Python's time_key=None).
func buildSeriesRuleLookups(
	rows []Row,
	sourceKey string,
	signalKey string,
	serviceKey string,
	attrFpKey string,
	timeKey string,
) (map[seriesRuleKey]Row, map[seriesRuleTimedKey]Row) {
	latestLookup := map[seriesRuleKey]Row{}
	timedLookup := map[seriesRuleTimedKey]Row{}
	for _, row := range rows {
		baseKey := seriesRuleKey{
			service: rowString(row[serviceKey]),
			attrFp:  rowString(row[attrFpKey]),
			source:  rowString(row[sourceKey]),
			signal:  rowString(row[signalKey]),
		}
		latestLookup[baseKey] = row
		if timeKey != "" {
			timedLookup[seriesRuleTimedKey{
				service: baseKey.service,
				attrFp:  baseKey.attrFp,
				source:  baseKey.source,
				signal:  baseKey.signal,
				time:    rowString(row[timeKey]),
			}] = row
		}
	}
	return latestLookup, timedLookup
}

// combineRuleStates mirrors _combine_rule_states: max() over (rank, state)
// tuples — ties on rank break on the state string, like Python tuple compare.
func combineRuleStates(states ...string) string {
	bestRank := 0
	bestState := ""
	first := true
	for _, state := range states {
		rank := anomalySeverityRank[state]
		if first || rank > bestRank || (rank == bestRank && state > bestState) {
			bestRank = rank
			bestState = state
			first = false
		}
	}
	return bestState
}

// lookupSecondaryRuleRow mirrors _lookup_secondary_rule_row.
// PORT-NOTE: Python lets db errors propagate; here a query error is logged at
// debug and treated as "no row" (nil), matching the None-handling path.
func lookupSecondaryRuleRow(service, attrFp, secondarySource, secondarySignal, timeValue string) Row {
	db := getDb()
	attrFilter := "AttrFingerprint = ?"
	params := []any{service, secondarySource, secondarySignal, attrFp}
	if timeValue != "" {
		res, err := db.Execute(
			"SELECT time, value, SampleCount FROM v_derived_signals_anomaly "+
				"WHERE ServiceName = ? AND SignalSource = ? AND SignalName = ? AND "+
				attrFilter+" AND time = ? ORDER BY time DESC LIMIT 1",
			append(append([]any{}, params...), timeValue)...,
		)
		if err != nil {
			logger.Debug("lookup secondary rule row failed", "error", err)
			return nil
		}
		if row := res.Fetchone(); row != nil {
			return Row{"time": row["time"], "value": row["value"], "sample_count": row["SampleCount"]}
		}
	}
	res, err := db.Execute(
		"SELECT time, value, SampleCount FROM v_derived_signals_anomaly "+
			"WHERE ServiceName = ? AND SignalSource = ? AND SignalName = ? AND "+
			attrFilter+" ORDER BY time DESC LIMIT 1",
		params...,
	)
	if err != nil {
		logger.Debug("lookup secondary rule row failed", "error", err)
		return nil
	}
	row := res.Fetchone()
	if row == nil {
		return nil
	}
	return Row{"time": row["time"], "value": row["value"], "sample_count": row["SampleCount"]}
}

// evaluateCompositeRule mirrors _evaluate_composite_rule
// (timeKey "" means Python's time_key=None).
func evaluateCompositeRule(
	rule map[string]any,
	row Row,
	latestLookup map[seriesRuleKey]Row,
	timedLookup map[seriesRuleTimedKey]Row,
	sourceKey string,
	signalKey string,
	serviceKey string,
	attrFpKey string,
	valueKey string,
	sampleCountKey string,
	timeKey string,
) map[string]any {
	_ = sourceKey // accepted for signature parity with the Python keyword args
	primary := evaluateThresholdCondition(
		fmt.Sprintf("%s primary", rowString(rule["name"])),
		thresholdComparator(rule, "comparator"),
		rule["warning_threshold"],
		rule["critical_threshold"],
		row[valueKey],
		row[sampleCountKey],
		ruleMinSampleCount(rule),
	)
	if primary == nil {
		return nil
	}

	secondarySource := rowString(rule["secondary_source"])
	secondarySignal := rowString(rule["secondary_signal"])
	if secondarySource == "" || secondarySignal == "" {
		return nil
	}

	service := rowString(row[serviceKey])
	attrFp := rowString(row[attrFpKey])
	timeValue := ""
	if timeKey != "" {
		timeValue = rowString(row[timeKey])
	}
	var secondaryRow Row
	if timeKey != "" {
		secondaryRow = timedLookup[seriesRuleTimedKey{
			service: service, attrFp: attrFp, source: secondarySource, signal: secondarySignal, time: timeValue,
		}]
	}
	if secondaryRow == nil {
		secondaryRow = latestLookup[seriesRuleKey{
			service: service, attrFp: attrFp, source: secondarySource, signal: secondarySignal,
		}]
	}
	if secondaryRow == nil {
		secondaryRow = lookupSecondaryRuleRow(
			service,
			attrFp,
			secondarySource,
			secondarySignal,
			timeValue,
		)
	}
	if secondaryRow == nil {
		return nil
	}

	secondaryValue, exists := secondaryRow[valueKey]
	if !exists {
		secondaryValue = secondaryRow["value"]
	}
	secondarySampleCount, exists := secondaryRow[sampleCountKey]
	if !exists {
		secondarySampleCount = secondaryRow["sample_count"]
	}
	secondary := evaluateThresholdCondition(
		fmt.Sprintf("%s secondary", rowString(rule["name"])),
		thresholdComparator(rule, "secondary_comparator"),
		rule["secondary_warning_threshold"],
		rule["secondary_critical_threshold"],
		secondaryValue,
		secondarySampleCount,
		ruleMinSampleCount(rule),
	)
	if secondary == nil {
		return nil
	}

	primaryState := rowString(primary["rule_state"])
	secondaryState := rowString(secondary["rule_state"])
	combinedState := combineRuleStates(primaryState, secondaryState)
	return map[string]any{
		"rule_id":    rowString(rule["id"]),
		"rule_name":  rowString(rule["name"]),
		"rule_state": combinedState,
		"rule_reason": fmt.Sprintf("%s: primary %s=%v and secondary %s=%v triggered",
			rowString(rule["name"]), rowString(row[signalKey]), row[valueKey],
			secondarySignal, secondaryValue),
	}
}

// annotateRowsWithRules mirrors _annotate_rows_with_rules
// (timeKey "" means Python's time_key=None). Mutates rows in place.
func annotateRowsWithRules(
	rows []Row,
	rules []map[string]any,
	sourceKey string,
	signalKey string,
	serviceKey string,
	attrFpKey string,
	valueKey string,
	sampleCountKey string,
	timeKey string,
) {
	latestLookup, timedLookup := buildSeriesRuleLookups(
		rows,
		sourceKey,
		signalKey,
		serviceKey,
		attrFpKey,
		timeKey,
	)
	ruleTypePrecedence := map[string]int{
		"seasonal":  3,
		"composite": 2,
		"threshold": 1,
	}
	type evalRank struct {
		severity int
		typeRank int
		ruleName string
	}
	rankGreater := func(a, b evalRank) bool {
		if a.severity != b.severity {
			return a.severity > b.severity
		}
		if a.typeRank != b.typeRank {
			return a.typeRank > b.typeRank
		}
		return a.ruleName > b.ruleName
	}
	for _, row := range rows {
		row["rule_name"] = ""
		row["rule_state"] = "normal"
		row["rule_reason"] = ""
		row["rule_seasonal"] = false
		anomalyState := "normal"
		if v, ok := row["anomaly_state"]; ok {
			anomalyState = rowString(v)
		}
		row["effective_state"] = anomalyState
		var bestMatch map[string]any
		bestRank := evalRank{severity: -1, typeRank: -1, ruleName: ""}
		rowSource := rowString(row[sourceKey])
		rowSignal := rowString(row[signalKey])
		rowService := rowString(row[serviceKey])
		rowAttrFp := rowString(row[attrFpKey])
		for _, rule := range rules {
			if !ruleMatchesSeries(rule, rowSource, rowSignal, rowService, rowAttrFp) {
				continue
			}
			ruleType := rowString(rule["rule_type"])
			if rule["rule_type"] == nil {
				ruleType = "threshold"
			}
			var evaluation map[string]any
			if ruleType == "composite" {
				evaluation = evaluateCompositeRule(
					rule,
					row,
					latestLookup,
					timedLookup,
					sourceKey,
					signalKey,
					serviceKey,
					attrFpKey,
					valueKey,
					sampleCountKey,
					timeKey,
				)
			} else if ruleType == "seasonal" {
				var timeValue any
				if timeKey != "" {
					timeValue = row[timeKey]
				}
				evaluation = evaluateSeasonalRule(rule, row[valueKey], row[sampleCountKey], timeValue)
			} else {
				evaluation = evaluateThresholdRule(rule, row[valueKey], row[sampleCountKey])
			}
			if evaluation == nil {
				continue
			}
			severity := anomalySeverityRank[rowString(evaluation["rule_state"])]
			typeRank := ruleTypePrecedence[ruleType]
			// Deterministic tie-breaker when multiple rules fire with equal
			// severity: prefer richer rule types (seasonal > composite > threshold),
			// then lexical rule name for stable behavior.
			rank := evalRank{severity: severity, typeRank: typeRank, ruleName: rowString(evaluation["rule_name"])}
			if rankGreater(rank, bestRank) {
				bestMatch = evaluation
				bestRank = rank
			}
		}
		if bestMatch != nil {
			for k, v := range bestMatch {
				row[k] = v
			}
		}
		finalAnomalyState := "normal"
		if v, ok := row["anomaly_state"]; ok {
			finalAnomalyState = rowString(v)
		}
		row["effective_state"] = combineRuleStates(
			finalAnomalyState,
			rowString(row["rule_state"]),
		)
	}
}
