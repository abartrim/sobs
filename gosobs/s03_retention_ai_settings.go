package main

// Port of app.py lines 2325-3404: raw metrics retention (pinned window
// tables + copy worker + background loop), AI pricing helpers, AI settings
// load/save, GitHub token helpers, ISO datetime parsing, CI push API key
// helpers, and feature flags (template context processor).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/scrypt"
)

// ---------------------------------------------------------------------------
// Small coercion helpers (str()/int()/float() equivalents for Row values)
// ---------------------------------------------------------------------------

// coerceInt mirrors Python int(value or 0) for values coming from chdb rows.
func coerceInt(value any) int {
	switch v := value.(type) {
	case nil:
		return 0
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case uint64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
		if f, err := v.Float64(); err == nil {
			return int(f)
		}
		return 0
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return int(f)
		}
		return 0
	default:
		return 0
	}
}

// coerceFloat mirrors Python float(value) returning ok=false on TypeError/ValueError.
func coerceFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case float32:
		return float64(v), true
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// rowString mirrors str(value or "") for Row values.
func rowString(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%v", value)
}

// clipRunes mirrors Python string slicing s[:n] (code points, not bytes).
func clipRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// ---------------------------------------------------------------------------
// Raw metrics retention – baseline TTL + pinned window tables
// ---------------------------------------------------------------------------

func parsePositiveIntEnv(name, def, unit string) int {
	raw, ok := os.LookupEnv(name)
	if !ok {
		raw = def
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		// PORT-NOTE: Python raises ValueError at module import; the Go port
		// panics at package init (process fails to start in both cases).
		panic(fmt.Sprintf("%s must be a positive integer (%s)", name, unit))
	}
	return value
}

var (
	rawMetricsBaselineTtlHours = parsePositiveIntEnv("SOBS_RAW_METRICS_TTL_HOURS", "48", "hours")
	rawMetricsPinnedTtlDays    = parsePositiveIntEnv("SOBS_PINNED_METRICS_TTL_DAYS", "14", "days")
)

const (
	rawMetricsWindowMinutes = 5
	rawWindowCopyIntervalS  = 60
	rawWindowCopyMaxPerRun  = 10
)

var rawMetricTables = []string{"otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram"}
var pinnedMetricTables = []string{
	"otel_metrics_gauge_pinned",
	"otel_metrics_sum_pinned",
	"otel_metrics_histogram_pinned",
}

// PORT-NOTE: Python keeps the asyncio task handle in _RAW_WINDOW_COPY_TASK;
// the Go port starts rawWindowCopyLoop as a goroutine from main and needs no
// handle, so the global is omitted.

// ensureRawMetricsRetention applies baseline TTL to raw metric tables and
// pinned TTL to pinned tables.
func ensureRawMetricsRetention(db *ChDbConnection) {
	baselineHours := rawMetricsBaselineTtlHours
	pinnedDays := rawMetricsPinnedTtlDays
	statements := []string{
		fmt.Sprintf("ALTER TABLE otel_metrics_gauge MODIFY TTL TimeUnixMs + INTERVAL %d HOUR", baselineHours),
		fmt.Sprintf("ALTER TABLE otel_metrics_sum MODIFY TTL TimeUnixMs + INTERVAL %d HOUR", baselineHours),
		fmt.Sprintf("ALTER TABLE otel_metrics_histogram MODIFY TTL TimeUnixMs + INTERVAL %d HOUR", baselineHours),
		fmt.Sprintf("ALTER TABLE otel_metrics_gauge_pinned MODIFY TTL TimeUnixMs + INTERVAL %d DAY", pinnedDays),
		fmt.Sprintf("ALTER TABLE otel_metrics_sum_pinned MODIFY TTL TimeUnixMs + INTERVAL %d DAY", pinnedDays),
		fmt.Sprintf("ALTER TABLE otel_metrics_histogram_pinned MODIFY TTL TimeUnixMs + INTERVAL %d DAY", pinnedDays),
	}
	for _, stmt := range statements {
		if _, err := db.Execute(stmt); err != nil {
			logger.Debug(fmt.Sprintf("raw metrics retention alter skipped: %s", stmt), "error", err)
		}
	}
}

// registerRawWindow registers a raw preservation window around a signal.
// Returns the window Id.
func registerRawWindow(
	db *ChDbConnection,
	signalTs time.Time,
	signalType string,
	signalRef string,
	serviceName string,
	namespace string,
	nodeName string,
) string {
	windowStart := signalTs.Add(-rawMetricsWindowMinutes * time.Minute)
	windowEnd := signalTs.Add(rawMetricsWindowMinutes * time.Minute)

	dedupKey := strings.Join([]string{
		signalTs.Format("2006-01-02T15:04"),
		clipRunes(signalType, 64),
		clipRunes(signalRef, 128),
		clipRunes(serviceName, 64),
		clipRunes(namespace, 64),
		clipRunes(nodeName, 64),
	}, "|")
	digest := sha256.Sum256([]byte(dedupKey))
	windowId := hex.EncodeToString(digest[:])[:32]

	// Python: strftime("%Y-%m-%d %H:%M:%S.%f")[:-3] → millisecond precision.
	const tsFmt = "2006-01-02 15:04:05.000"
	insertRowsJsonEachRow(
		db,
		"sobs_raw_windows",
		[]Row{
			{
				"Id":          windowId,
				"SignalTs":    signalTs.Format(tsFmt),
				"WindowStart": windowStart.Format(tsFmt),
				"WindowEnd":   windowEnd.Format(tsFmt),
				"SignalType":  clipRunes(signalType, 64),
				"SignalRef":   clipRunes(signalRef, 256),
				"ServiceName": clipRunes(serviceName, 128),
				"Namespace":   clipRunes(namespace, 128),
				"NodeName":    clipRunes(nodeName, 128),
				"Version":     time.Now().UnixMilli(),
			},
		},
	)
	return windowId
}

// windowCopyCounts returns copied source-table counts by window id.
func windowCopyCounts(db *ChDbConnection, windowIds []string) map[string]int {
	if len(windowIds) == 0 {
		return map[string]int{}
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(windowIds)), ",")
	params := make([]any, 0, len(windowIds))
	for _, id := range windowIds {
		params = append(params, id)
	}
	res, err := db.Execute(
		"SELECT WindowId, countDistinct(SourceTable) AS c "+
			"FROM sobs_raw_window_copy_state FINAL "+
			fmt.Sprintf("WHERE WindowId IN (%s) ", placeholders)+
			"GROUP BY WindowId",
		params...,
	)
	if err != nil {
		// PORT-NOTE: Python lets the exception propagate; callers wrap in try.
		logger.Debug("window copy counts query failed", "error", err)
		return map[string]int{}
	}
	out := map[string]int{}
	for _, r := range res.Fetchall() {
		out[rowString(r["WindowId"])] = coerceInt(r["c"])
	}
	return out
}

// listTraceOverlappingRawWindows returns retention windows that overlap a
// trace time range.
func listTraceOverlappingRawWindows(
	db *ChDbConnection,
	serviceNames []string,
	startTs string,
	endTs string,
	limit int,
) []map[string]any {
	whereParts := []string{
		"WindowEnd >= parseDateTime64BestEffort(?, 9)",
		"WindowStart <= parseDateTime64BestEffort(?, 9)",
	}
	params := []any{startTs, endTs}
	if len(serviceNames) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(serviceNames)), ",")
		whereParts = append(whereParts, fmt.Sprintf("(ServiceName = '' OR ServiceName IN (%s))", placeholders))
		for _, s := range serviceNames {
			params = append(params, s)
		}
	}
	whereSql := strings.Join(whereParts, " AND ")
	safeLimit := limit
	if safeLimit > 100 {
		safeLimit = 100
	}
	if safeLimit < 1 {
		safeLimit = 1
	}
	res, err := db.Execute(
		"SELECT Id, SignalType, SignalRef, ServiceName, Namespace, NodeName, WindowStart, WindowEnd "+
			"FROM sobs_raw_windows FINAL "+
			fmt.Sprintf("WHERE %s ", whereSql)+
			"ORDER BY WindowStart DESC "+
			"LIMIT ?",
		append(params, safeLimit)...,
	)
	if err != nil {
		logger.Debug("list trace overlapping raw windows failed", "error", err)
		return []map[string]any{}
	}
	rows := res.Fetchall()
	if len(rows) == 0 {
		return []map[string]any{}
	}

	expectedCount := len(rawMetricTables)
	windowIds := make([]string, 0, len(rows))
	for _, r := range rows {
		windowIds = append(windowIds, rowString(r["Id"]))
	}
	copiedCounts := windowCopyCounts(db, windowIds)

	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		windowId := rowString(r["Id"])
		copiedCount := copiedCounts[windowId]
		out = append(out, map[string]any{
			"id":             windowId,
			"signal_type":    rowString(r["SignalType"]),
			"signal_ref":     rowString(r["SignalRef"]),
			"service_name":   rowString(r["ServiceName"]),
			"namespace":      rowString(r["Namespace"]),
			"node_name":      rowString(r["NodeName"]),
			"window_start":   rowString(r["WindowStart"]),
			"window_end":     rowString(r["WindowEnd"]),
			"copied_count":   copiedCount,
			"expected_count": expectedCount,
			"copy_complete":  copiedCount >= expectedCount,
		})
	}
	return out
}

// runRawWindowCopyWorker copies raw metric rows that fall within registered
// windows into pinned tables.
//
// Reads at most rawWindowCopyMaxPerRun uncopied (window, table) pairs,
// performs INSERT INTO … SELECT …, and records completion in
// sobs_raw_window_copy_state. Idempotent: re-running is safe.
func runRawWindowCopyWorker(db *ChDbConnection) map[string]int {
	stats := map[string]int{"windows_attempted": 0, "copies_ok": 0, "copies_error": 0}

	res, err := db.Execute(
		"SELECT Id, WindowStart, WindowEnd, ServiceName, Namespace, NodeName " +
			"FROM sobs_raw_windows FINAL " +
			"ORDER BY WindowStart DESC " +
			fmt.Sprintf("LIMIT %d", rawWindowCopyMaxPerRun*20),
	)
	if err != nil {
		logger.Debug("raw window copy: failed to fetch windows", "error", err)
		return stats
	}
	windows := res.Fetchall()
	if len(windows) == 0 {
		return stats
	}

	copiesAttempted := 0
	for _, windowRow := range windows {
		if copiesAttempted >= rawWindowCopyMaxPerRun {
			break
		}

		windowId := rowString(windowRow["Id"])
		windowStart := rowString(windowRow["WindowStart"])
		windowEnd := rowString(windowRow["WindowEnd"])
		serviceName := rowString(windowRow["ServiceName"])
		namespace := rowString(windowRow["Namespace"])
		nodeName := rowString(windowRow["NodeName"])

		for i, rawTable := range rawMetricTables {
			pinnedTable := pinnedMetricTables[i]
			if copiesAttempted >= rawWindowCopyMaxPerRun {
				break
			}

			checkRes, err := db.Execute(
				"SELECT 1 FROM sobs_raw_window_copy_state FINAL WHERE WindowId=? AND SourceTable=? LIMIT 1",
				windowId, rawTable,
			)
			if err != nil {
				logger.Debug(fmt.Sprintf(
					"raw window copy: failed to check copy state for window=%s table=%s",
					windowId, rawTable), "error", err)
				continue
			}
			if checkRes.Fetchone() != nil {
				continue
			}

			stats["windows_attempted"]++

			whereClauses := []string{
				"TimeUnix >= parseDateTime64BestEffort(?, 9)",
				"TimeUnix <= parseDateTime64BestEffort(?, 9)",
			}
			params := []any{windowStart, windowEnd}

			if serviceName != "" {
				whereClauses = append(whereClauses, "ServiceName = ?")
				params = append(params, serviceName)
			}
			if namespace != "" {
				whereClauses = append(whereClauses, "Attributes['k8s.namespace.name'] = ?")
				params = append(params, namespace)
			}
			if nodeName != "" {
				whereClauses = append(whereClauses, "Attributes['k8s.node.name'] = ?")
				params = append(params, nodeName)
			}

			whereSql := strings.Join(whereClauses, " AND ")

			// Histogram has different columns from gauge/sum.
			var selectCols string
			switch rawTable {
			case "otel_metrics_histogram":
				selectCols = "TimeUnix, TimeUnixMs, ServiceName, MetricName, MetricDescription, " +
					"MetricUnit, Attributes, Count, Sum, BucketCounts, ExplicitBounds, " +
					"Flags, AggregationTemporality, AttrFingerprint"
			case "otel_metrics_sum":
				selectCols = "TimeUnix, TimeUnixMs, ServiceName, MetricName, MetricDescription, " +
					"MetricUnit, Attributes, Value, Flags, IsMonotonic, " +
					"AggregationTemporality, AttrFingerprint"
			default:
				selectCols = "TimeUnix, TimeUnixMs, ServiceName, MetricName, MetricDescription, " +
					"MetricUnit, Attributes, Value, Flags, AttrFingerprint"
			}

			doubledParams := append(append([]any{}, params...), params...)

			status, copyErr := func() (string, error) {
				countRes, err := db.Execute(
					fmt.Sprintf("SELECT count() AS cnt FROM %s WHERE %s", rawTable, whereSql),
					params...,
				)
				if err != nil {
					return "", err
				}
				matchedRows := 0
				if row := countRes.Fetchone(); row != nil {
					matchedRows = coerceInt(row["cnt"])
				}
				if matchedRows <= 0 {
					return "skip", nil
				}

				// Only copy rows that are not already present in the pinned table.
				missingRes, err := db.Execute(
					fmt.Sprintf("SELECT count() AS cnt FROM %s WHERE %s ", rawTable, whereSql)+
						"AND (ServiceName, MetricName, AttrFingerprint, TimeUnix) NOT IN ("+
						"SELECT ServiceName, MetricName, AttrFingerprint, TimeUnix "+
						fmt.Sprintf("FROM %s WHERE %s)", pinnedTable, whereSql),
					doubledParams...,
				)
				if err != nil {
					return "", err
				}
				missingRows := 0
				if row := missingRes.Fetchone(); row != nil {
					missingRows = coerceInt(row["cnt"])
				}
				if missingRows <= 0 {
					insertRowsJsonEachRow(
						db,
						"sobs_raw_window_copy_state",
						[]Row{
							{
								"WindowId":    windowId,
								"SourceTable": rawTable,
								"Version":     time.Now().UnixMilli(),
							},
						},
					)
					return "ok", nil
				}

				if _, err := db.Execute(
					fmt.Sprintf("INSERT INTO %s (%s) ", pinnedTable, selectCols)+
						fmt.Sprintf("SELECT %s FROM %s WHERE %s ", selectCols, rawTable, whereSql)+
						"AND (ServiceName, MetricName, AttrFingerprint, TimeUnix) NOT IN ("+
						"SELECT ServiceName, MetricName, AttrFingerprint, TimeUnix "+
						fmt.Sprintf("FROM %s WHERE %s)", pinnedTable, whereSql),
					doubledParams...,
				); err != nil {
					return "", err
				}
				insertRowsJsonEachRow(
					db,
					"sobs_raw_window_copy_state",
					[]Row{
						{
							"WindowId":    windowId,
							"SourceTable": rawTable,
							"Version":     time.Now().UnixMilli(),
						},
					},
				)
				return "ok", nil
			}()

			switch {
			case copyErr != nil:
				copiesAttempted++
				logger.Debug(fmt.Sprintf("raw window copy error: window=%s table=%s", windowId, rawTable), "error", copyErr)
				stats["copies_error"]++
			case status == "skip":
				continue
			default:
				copiesAttempted++
				stats["copies_ok"]++
			}
		}
	}

	return stats
}

// rawWindowCopyLoop is a background task: run the raw window copy worker
// every 60 seconds. Started as a goroutine from main.
func rawWindowCopyLoop() {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Debug("raw window copy loop error", "error", r)
				}
			}()
			db := getDb()
			stats := runRawWindowCopyWorker(db)
			if stats["copies_ok"] > 0 || stats["copies_error"] > 0 {
				logger.Info(fmt.Sprintf(
					"raw window copy: attempted=%d ok=%d errors=%d",
					stats["windows_attempted"], stats["copies_ok"], stats["copies_error"]))
			}
		}()
		time.Sleep(rawWindowCopyIntervalS * time.Second)
	}
}

// ---------------------------------------------------------------------------
// AI Settings helpers
// ---------------------------------------------------------------------------

var aiSettingKeys = []string{
	"ai.endpoint_url",
	"ai.model",
	"ai.thinking_level",
	"ai.api_key",
	"ai.endpoint_timeout_seconds",
	"ai.guard_endpoint_url",
	"ai.guard_model",
	"ai.guard_thinking_level",
	"ai.guard_timeout_seconds",
	"ai.dlp_endpoint_url",
	"ai.github_token",
	"ai.github_token_expires_at",
	"ai.github_token_last_validated_at",
	"ai.github_token_last_validation_status",
	"ai.github_token_last_validation_message",
	"ai.github_repo",
	"ai.agent_max_issues_per_hour",
	"ai.agent_max_assignments_per_hour",
	"ai.agent_max_active_assignments",
	"ai.github_copilot_base_branch",
	"ai.github_copilot_custom_instructions",
	"ai.system_prompt",
	"ai.model_pricing",
	"ai.model_pricing_confirmed",
}

var aiSensitiveSettingKeys = map[string]bool{"ai.api_key": true, "ai.github_token": true}

// Default per-model pricing in USD per 1M tokens. Keys are lowercase model names.
// Users can override or extend this table via Settings → AI Configuration.
// PORT-NOTE: Python relies on dict insertion order when substring-matching in
// _infer_ai_pricing_for_model; defaultAiPricingOrder preserves that order.
var defaultAiPricingOrder = []string{
	// OpenAI
	"gpt-4o",
	"gpt-4o-mini",
	"gpt-4-turbo",
	"gpt-4",
	"gpt-3.5-turbo",
	"o1",
	"o1-mini",
	"o3-mini",
	// Anthropic
	"claude-3-5-sonnet-20241022",
	"claude-3-5-sonnet",
	"claude-3-5-haiku",
	"claude-3-opus",
	"claude-3-sonnet",
	"claude-3-haiku",
	// Google
	"gemini-1.5-pro",
	"gemini-1.5-flash",
	"gemini-2.0-flash",
	// Meta / open source (inference cost estimate)
	"llama-3.1-70b",
	"llama-3.1-8b",
	// Mistral
	"mistral-large",
	"mistral-small",
}

var defaultAiPricing = map[string]map[string]float64{
	"gpt-4o":                     {"in": 2.50, "out": 10.00},
	"gpt-4o-mini":                {"in": 0.15, "out": 0.60},
	"gpt-4-turbo":                {"in": 10.00, "out": 30.00},
	"gpt-4":                      {"in": 30.00, "out": 60.00},
	"gpt-3.5-turbo":              {"in": 0.50, "out": 1.50},
	"o1":                         {"in": 15.00, "out": 60.00},
	"o1-mini":                    {"in": 3.00, "out": 12.00},
	"o3-mini":                    {"in": 1.10, "out": 4.40},
	"claude-3-5-sonnet-20241022": {"in": 3.00, "out": 15.00},
	"claude-3-5-sonnet":          {"in": 3.00, "out": 15.00},
	"claude-3-5-haiku":           {"in": 0.80, "out": 4.00},
	"claude-3-opus":              {"in": 15.00, "out": 75.00},
	"claude-3-sonnet":            {"in": 3.00, "out": 15.00},
	"claude-3-haiku":             {"in": 0.25, "out": 1.25},
	"gemini-1.5-pro":             {"in": 1.25, "out": 5.00},
	"gemini-1.5-flash":           {"in": 0.075, "out": 0.30},
	"gemini-2.0-flash":           {"in": 0.10, "out": 0.40},
	"llama-3.1-70b":              {"in": 0.90, "out": 0.90},
	"llama-3.1-8b":               {"in": 0.20, "out": 0.20},
	"mistral-large":              {"in": 3.00, "out": 9.00},
	"mistral-small":              {"in": 0.20, "out": 0.60},
}

const aiPricingGenericDefaultKey = "gpt-4o"

type aiPricingInferenceRule struct {
	needles []string
	baseKey string
}

var aiPricingInferenceRules = []aiPricingInferenceRule{
	{[]string{"4o-mini"}, "gpt-4o-mini"},
	{[]string{"4o"}, "gpt-4o"},
	{[]string{"3.5"}, "gpt-3.5-turbo"},
	{[]string{"turbo"}, "gpt-4-turbo"},
	{[]string{"o3-mini"}, "o3-mini"},
	{[]string{"o1-mini"}, "o1-mini"},
	{[]string{"o1"}, "o1"},
	{[]string{"haiku"}, "claude-3-5-haiku"},
	{[]string{"sonnet"}, "claude-3-5-sonnet"},
	{[]string{"opus"}, "claude-3-opus"},
	{[]string{"claude"}, "claude-3-5-sonnet"},
	{[]string{"2.0-flash", "2-flash"}, "gemini-2.0-flash"},
	{[]string{"1.5-flash", "flash-lite", "flash"}, "gemini-1.5-flash"},
	{[]string{"1.5-pro", "pro"}, "gemini-1.5-pro"},
	{[]string{"gemini"}, "gemini-1.5-flash"},
	{[]string{"70b"}, "llama-3.1-70b"},
	{[]string{"8b"}, "llama-3.1-8b"},
	{[]string{"llama"}, "llama-3.1-8b"},
	{[]string{"large"}, "mistral-large"},
	{[]string{"small"}, "mistral-small"},
	{[]string{"mistral"}, "mistral-small"},
}

func normalizeAiModelName(model any) string {
	if model == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", model)))
}

func copyAiPricingEntry(prices map[string]float64) map[string]float64 {
	return map[string]float64{"in": prices["in"], "out": prices["out"]}
}

// coerceAiPricingEntry returns nil if prices is not a valid {in, out} mapping.
func coerceAiPricingEntry(prices any) map[string]float64 {
	m, ok := prices.(map[string]any)
	if !ok {
		return nil
	}
	inRaw, hasIn := m["in"]
	outRaw, hasOut := m["out"]
	if !hasIn || !hasOut {
		return nil
	}
	inVal, okIn := coerceFloat(inRaw)
	outVal, okOut := coerceFloat(outRaw)
	if !okIn || !okOut {
		return nil
	}
	return map[string]float64{"in": inVal, "out": outVal}
}

func loadSavedAiPricing(db *ChDbConnection) map[string]map[string]float64 {
	saved := map[string]map[string]float64{}
	raw := strings.TrimSpace(loadAiSetting(db, "ai.model_pricing", ""))
	if raw == "" {
		return saved
	}
	var userPricing any
	if err := json.Unmarshal([]byte(raw), &userPricing); err != nil {
		return saved
	}
	pricingMap, ok := userPricing.(map[string]any)
	if !ok {
		return saved
	}
	for modelKey, prices := range pricingMap {
		normalizedKey := normalizeAiModelName(modelKey)
		entry := coerceAiPricingEntry(prices)
		if normalizedKey != "" && entry != nil {
			saved[normalizedKey] = entry
		}
	}
	return saved
}

func loadConfirmedAiPricingModels(db *ChDbConnection) map[string]bool {
	raw := strings.TrimSpace(loadAiSetting(db, "ai.model_pricing_confirmed", ""))
	if raw == "" {
		return map[string]bool{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return map[string]bool{}
	}
	list, ok := parsed.([]any)
	if !ok {
		return map[string]bool{}
	}
	confirmed := map[string]bool{}
	for _, model := range list {
		if modelKey := normalizeAiModelName(model); modelKey != "" {
			confirmed[modelKey] = true
		}
	}
	return confirmed
}

func inferAiPricingForModel(model string) map[string]float64 {
	normalized := normalizeAiModelName(model)
	if normalized == "" {
		return copyAiPricingEntry(defaultAiPricing[aiPricingGenericDefaultKey])
	}
	if prices, ok := defaultAiPricing[normalized]; ok {
		return copyAiPricingEntry(prices)
	}
	for _, knownKey := range defaultAiPricingOrder {
		if strings.Contains(knownKey, normalized) || strings.Contains(normalized, knownKey) {
			return copyAiPricingEntry(defaultAiPricing[knownKey])
		}
	}
	for _, rule := range aiPricingInferenceRules {
		for _, needle := range rule.needles {
			if strings.Contains(normalized, needle) {
				return copyAiPricingEntry(defaultAiPricing[rule.baseKey])
			}
		}
	}
	return copyAiPricingEntry(defaultAiPricing[aiPricingGenericDefaultKey])
}

func loadObservedAiModels(db *ChDbConnection, limit int) []string {
	safeLimit := limit
	if safeLimit > 500 {
		safeLimit = 500
	}
	if safeLimit < 1 {
		safeLimit = 1
	}
	res, err := db.Execute(
		"SELECT DISTINCT SpanAttributes['gen_ai.request.model'] AS model " +
			"FROM otel_traces " +
			fmt.Sprintf("WHERE %s AND SpanAttributes['gen_ai.request.model'] != '' ", aiSpanCondition) +
			fmt.Sprintf("ORDER BY model LIMIT %d", safeLimit),
	)
	if err != nil {
		return []string{}
	}
	normalizedModels := []string{}
	seen := map[string]bool{}
	for _, row := range res.Fetchall() {
		modelKey := normalizeAiModelName(row["model"])
		if modelKey != "" && !seen[modelKey] {
			seen[modelKey] = true
			normalizedModels = append(normalizedModels, modelKey)
		}
	}
	return normalizedModels
}

func loadAiPricingWithSources(db *ChDbConnection) (map[string]map[string]float64, map[string]string) {
	merged := map[string]map[string]float64{}
	sources := map[string]string{}
	for modelKey, prices := range defaultAiPricing {
		merged[modelKey] = copyAiPricingEntry(prices)
		sources[modelKey] = "default"
	}

	for _, modelKey := range loadObservedAiModels(db, 200) {
		if _, ok := merged[modelKey]; !ok {
			merged[modelKey] = inferAiPricingForModel(modelKey)
			sources[modelKey] = "inferred"
		}
	}

	confirmedModels := loadConfirmedAiPricingModels(db)
	for modelKey, prices := range loadSavedAiPricing(db) {
		merged[modelKey] = prices
		if sources[modelKey] == "inferred" {
			if confirmedModels[modelKey] {
				sources[modelKey] = "confirmed"
			}
		} else if _, ok := sources[modelKey]; !ok {
			sources[modelKey] = "custom"
		}
	}

	return merged, sources
}

// loadAiPricing returns merged model pricing including defaults, observed
// models, and user overrides.
func loadAiPricing(db *ChDbConnection) map[string]map[string]float64 {
	merged, _ := loadAiPricingWithSources(db)
	return merged
}

var aiEnvOverrides = map[string][2]string{
	"ai.endpoint_url":             {"SOBS_AI_ENDPOINT_URL", "SOBS_AI_ENDPOINT_URL_FILE"},
	"ai.model":                    {"SOBS_AI_MODEL", "SOBS_AI_MODEL_FILE"},
	"ai.thinking_level":           {"SOBS_AI_THINKING_LEVEL", "SOBS_AI_THINKING_LEVEL_FILE"},
	"ai.api_key":                  {"SOBS_AI_API_KEY", "SOBS_AI_API_KEY_FILE"},
	"ai.endpoint_timeout_seconds": {"SOBS_AI_ENDPOINT_TIMEOUT_SECONDS", "SOBS_AI_ENDPOINT_TIMEOUT_SECONDS_FILE"},
	"ai.guard_endpoint_url":       {"SOBS_AI_GUARD_ENDPOINT_URL", "SOBS_AI_GUARD_ENDPOINT_URL_FILE"},
	"ai.guard_model":              {"SOBS_AI_GUARD_MODEL", "SOBS_AI_GUARD_MODEL_FILE"},
	"ai.guard_thinking_level":     {"SOBS_AI_GUARD_THINKING_LEVEL", "SOBS_AI_GUARD_THINKING_LEVEL_FILE"},
	"ai.guard_timeout_seconds":    {"SOBS_AI_GUARD_TIMEOUT_SECONDS", "SOBS_AI_GUARD_TIMEOUT_SECONDS_FILE"},
	"ai.dlp_endpoint_url":         {"SOBS_AI_DLP_ENDPOINT_URL", "SOBS_AI_DLP_ENDPOINT_URL_FILE"},
}

func isSensitiveAiSettingKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	return aiSensitiveSettingKeys[normalized] || strings.HasPrefix(normalized, "ai.github_token.repo.")
}

func githubRepoTokenKey(owner, repo string) string {
	return fmt.Sprintf("ai.github_token.repo.%s/%s",
		strings.ToLower(strings.TrimSpace(owner)),
		strings.ToLower(strings.TrimSpace(repo)))
}

func loadRepoScopedGithubToken(db *ChDbConnection, owner, repo string) string {
	if owner == "" || repo == "" {
		return ""
	}
	return strings.TrimSpace(loadAiSetting(db, githubRepoTokenKey(owner, repo), ""))
}

func saveRepoScopedGithubToken(db *ChDbConnection, owner, repo, token string) {
	if owner == "" || repo == "" || strings.TrimSpace(token) == "" {
		return
	}
	saveAiSetting(db, githubRepoTokenKey(owner, repo), strings.TrimSpace(token))
}

const (
	aiAgentMaxIssuesDefault             = 5
	aiAgentMaxAssignmentsPerHourDefault = 1
	aiAgentMaxActiveAssignmentsDefault  = 1
	githubCopilotAssignee               = "copilot-swe-agent[bot]"
	githubCopilotGraphqlFeatures        = "issues_copilot_assignment_api_support,coding_agent_model_selection"
	githubIssueDedupeCandidateLimit     = 10
	githubWorkItemBackfillIntervalSec   = 300
	githubWorkItemBackfillMaxItems      = 25
	githubTokenExpiryWarningDays        = 14
	ciPushAppKeyPrefix                  = "ai.ci_push.app."
	ciPushApiKeyDefaultTtlDays          = 30
	ciPushApiKeyMinTtlDays              = 1
	ciPushApiKeyMaxTtlDays              = 365
)

var (
	githubWorkItemBackfillLastTs  float64 = 0.0
	githubWorkItemBackfillRunning         = false
)

var aiThinkingLevels = []string{"off", "low", "medium", "high"}

var aiGuardBlockKeywords = map[string]bool{
	"ignore previous":     true,
	"disregard":           true,
	"jailbreak":           true,
	"bypass":              true,
	"forget instructions": true,
	"pretend you are":     true,
	"act as":              true,
}

var aiGuardNoisyCategories = map[string]bool{"S1": true, "S2": true, "S6": true, "S8": true, "S14": true}

var aiGuardCategories = map[string]string{
	"S1":  "Violent Crimes",
	"S2":  "Non-Violent Crimes",
	"S3":  "Sex-Related Crimes",
	"S4":  "Child Sexual Exploitation",
	"S5":  "Defamation",
	"S6":  "Specialized Advice",
	"S7":  "Privacy",
	"S8":  "Intellectual Property",
	"S9":  "Indiscriminate Weapons",
	"S10": "Hate",
	"S11": "Suicide & Self-Harm",
	"S12": "Sexual Content",
	"S13": "Elections",
	"S14": "Code Interpreter Abuse",
}

var aiObservabilityBenignKeywords = map[string]bool{
	"trace": true, "traces": true, "span": true, "spans": true,
	"latency": true, "duration": true, "slow": true, "p95": true, "p99": true,
	"error": true, "errors": true, "logs": true, "metrics": true,
	"service": true, "services": true, "query": true, "sql": true,
	"dashboard": true, "anomaly": true, "alert": true, "alerts": true,
	"root cause": true, "window": true, "windows": true, "burst": true,
	"spike": true, "spikes": true, "noisy": true,
	"deployment": true, "deployments": true,
}

var aiObservabilityHighRiskKeywords = map[string]bool{
	"exploit": true, "exfiltrate": true, "steal": true, "fraud": true,
	"malware": true, "ransomware": true, "ddos": true, "phishing": true,
	"evade": true, "weapon": true, "illegal": true, "break into": true,
	"unauthorized": true,
}

var aiUsageQueryIntentKeywords = map[string]bool{
	"list": true, "show": true, "count": true, "how many": true,
	"what": true, "which": true, "summarize": true,
}

var aiUsageAnalyticsKeywords = map[string]bool{
	"model": true, "models": true, "gpt": true, "llm": true,
	"calls": true, "call": true, "requests": true, "request": true,
	"usage": true, "token": true, "tokens": true, "cost": true,
	"latency": true,
}

var aiNavigationIntentKeywords = map[string]bool{
	"navigate": true, "go to": true, "open": true,
	"take me to": true, "bring me to": true, "switch to": true,
}

var aiNavigationSurfaceKeywords = map[string]bool{
	"page": true, "screen": true, "view": true, "tab": true,
	"section": true, "modal": true, "panel": true,
}

var aiChartRequestKeywords = map[string]bool{
	"graph": true, "chart": true, "plot": true, "visual": true,
	"visualize": true, "timeseries": true, "trend": true,
	"response time": true, "latency": true,
}

func loadAiSetting(db *ChDbConnection, key, def string) string {
	res, err := db.Execute(
		"SELECT Value FROM sobs_ai_settings FINAL WHERE Key=? AND IsDeleted=0 LIMIT 1",
		key,
	)
	if err != nil {
		// PORT-NOTE: Python propagates DB errors here; the Go port logs and
		// falls back to env/default (signature pinned to return string only).
		logger.Debug("loadAiSetting query failed", "key", key, "error", err)
	} else if row := res.Fetchone(); row != nil {
		rawValue := rowString(row["Value"])
		value := rawValue
		if isSensitiveAiSettingKey(key) {
			value = decryptSecretValue(rawValue)
		}
		if value != "" {
			return value
		}
	}

	if names, ok := aiEnvOverrides[key]; ok && names[0] != "" {
		if envFallback := readFileOrEnv(names[0], names[1]); envFallback != "" {
			return envFallback
		}
	}

	return def
}

func saveAiSetting(db *ChDbConnection, key, value string) {
	version := time.Now().UnixMilli()
	storedValue := value
	if isSensitiveAiSettingKey(key) {
		storedValue = encryptSecretValue(value)
	}
	insertRowsJsonEachRow(
		db,
		"sobs_ai_settings",
		[]Row{{"Key": key, "Value": storedValue, "IsDeleted": 0, "Version": version}},
	)
}

func loadAllAiSettings(db *ChDbConnection) map[string]string {
	result := make(map[string]string, len(aiSettingKeys))
	for _, k := range aiSettingKeys {
		result[k] = ""
	}
	res, err := db.Execute("SELECT Key, Value FROM sobs_ai_settings FINAL WHERE IsDeleted=0")
	if err != nil {
		logger.Debug("loadAllAiSettings query failed", "error", err)
	} else {
		for _, row := range res.Fetchall() {
			k := rowString(row["Key"])
			if _, ok := result[k]; ok {
				rawValue := rowString(row["Value"])
				if isSensitiveAiSettingKey(k) {
					result[k] = decryptSecretValue(rawValue)
				} else {
					result[k] = rawValue
				}
			}
		}
	}

	// Precedence: DB value first, then file-backed env, then direct env.
	for key, names := range aiEnvOverrides {
		if result[key] != "" {
			continue
		}
		if envFallback := readFileOrEnv(names[0], names[1]); envFallback != "" {
			result[key] = envFallback
		}
	}

	return result
}

// parseIsoDatetime mirrors _parse_iso_datetime: returns nil on empty/invalid
// input, otherwise the parsed time normalized to UTC.
func parseIsoDatetime(value string) *time.Time {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return nil
	}
	if strings.HasSuffix(raw, "Z") {
		raw = raw[:len(raw)-1] + "+00:00"
	}
	// PORT-NOTE: datetime.fromisoformat accepts more variants; these layouts
	// cover the formats produced/stored by the app (date, T/space separator,
	// optional fraction, optional offset).
	layouts := []string{
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04Z07:00",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			// Naive layouts parse as UTC (≅ replace(tzinfo=utc)); aware ones
			// convert (≅ astimezone(utc)).
			utc := parsed.UTC()
			return &utc
		}
	}
	return nil
}

var githubTokenExpiryDateOnlyRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func normalizeGithubTokenExpiryInput(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ""
	}
	// Support either date input (YYYY-MM-DD) or full ISO timestamp.
	if githubTokenExpiryDateOnlyRe.MatchString(raw) {
		return raw + "T23:59:59+00:00"
	}
	parsed := parseIsoDatetime(raw)
	if parsed == nil {
		return ""
	}
	return pyIsoFormat(*parsed)
}

func githubTokenExpiryDateInputValue(value string) string {
	parsed := parseIsoDatetime(value)
	if parsed == nil {
		return ""
	}
	return parsed.Format("2006-01-02")
}

// githubTokenExpiryStatus mirrors _github_token_expiry_status. warningDays is
// optional (Python keyword default _GITHUB_TOKEN_EXPIRY_WARNING_DAYS).
func githubTokenExpiryStatus(expiresAt string, warningDays ...int) map[string]any {
	warning := githubTokenExpiryWarningDays
	if len(warningDays) > 0 {
		warning = warningDays[0]
	}
	parsed := parseIsoDatetime(expiresAt)
	if parsed == nil {
		return map[string]any{
			"state":          "unknown",
			"expires_at":     "",
			"days_remaining": nil,
			"message":        "Token expiry date not set",
		}
	}

	nowUtc := time.Now().UTC()
	secondsRemaining := int(parsed.Sub(nowUtc).Seconds())
	// Python floor division (rounds toward negative infinity).
	daysRemaining := secondsRemaining / 86400
	if secondsRemaining%86400 != 0 && secondsRemaining < 0 {
		daysRemaining--
	}

	if secondsRemaining < 0 {
		return map[string]any{
			"state":          "expired",
			"expires_at":     pyIsoFormat(*parsed),
			"days_remaining": daysRemaining,
			"message":        fmt.Sprintf("Token expired on %s", parsed.Format("2006-01-02")),
		}
	}
	if daysRemaining <= warning {
		return map[string]any{
			"state":          "warning",
			"expires_at":     pyIsoFormat(*parsed),
			"days_remaining": daysRemaining,
			"message":        fmt.Sprintf("Token expires in %d day(s)", daysRemaining),
		}
	}
	return map[string]any{
		"state":          "healthy",
		"expires_at":     pyIsoFormat(*parsed),
		"days_remaining": daysRemaining,
		"message":        fmt.Sprintf("Token healthy (%d day(s) remaining)", daysRemaining),
	}
}

// normalizeTtlDays mirrors _normalize_ttl_days. defaultDays is optional
// (Python keyword default _CI_PUSH_API_KEY_DEFAULT_TTL_DAYS).
func normalizeTtlDays(value any, defaultDays ...int) int {
	def := ciPushApiKeyDefaultTtlDays
	if len(defaultDays) > 0 {
		def = defaultDays[0]
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(fmt.Sprintf("%v", value)))
	if err != nil {
		parsed = def
	}
	if parsed < ciPushApiKeyMinTtlDays {
		return ciPushApiKeyMinTtlDays
	}
	if parsed > ciPushApiKeyMaxTtlDays {
		return ciPushApiKeyMaxTtlDays
	}
	return parsed
}

func ciPushExpiryIsoFromDays(ttlDays int) string {
	expires := time.Now().UTC().Add(time.Duration(ttlDays) * 24 * time.Hour)
	expires = time.Date(expires.Year(), expires.Month(), expires.Day(), 23, 59, 59, 0, time.UTC)
	return pyIsoFormat(expires)
}

const ciPushHashPrefix = "scrypt:v1:"

// ciPushHashKey returns a per-installation key for CI push API-key fingerprinting.
func ciPushHashKey() []byte {
	secret := envDefault("SOBS_SECRET_KEY", "sobs-dev-secret-key")
	// PORT-NOTE: Python uses hashlib.blake2b(person=b"sobs-ci-hash-v1");
	// x/crypto/blake2b does not expose the personalization parameter, so the
	// Go port uses keyed BLAKE2b-256 with the same string as key. Derived
	// fingerprints therefore differ from a Python-written database.
	h, err := blake2b.New256([]byte("sobs-ci-hash-v1"))
	if err != nil {
		digest := sha256.Sum256([]byte(secret))
		return digest[:]
	}
	h.Write([]byte(secret))
	return h.Sum(nil)
}

// hashApiKey returns a keyed, memory-hard fingerprint for CI push API keys.
func hashApiKey(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ""
	}
	salt := ciPushHashKey()
	digest, err := scrypt.Key([]byte(raw), salt, 1024, 8, 1, 32)
	if err != nil {
		logger.Warn("hashApiKey scrypt failed", "error", err)
		return ""
	}
	return ciPushHashPrefix + hex.EncodeToString(digest)
}

func generateCiPushApiKey() string {
	// secrets.token_urlsafe(24) → 24 random bytes, URL-safe base64, no padding.
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "sobs_ci_" + base64.RawURLEncoding.EncodeToString(b)
}

func ciPushSettingKey(appId, leaf string) string {
	return fmt.Sprintf("%s%s.%s", ciPushAppKeyPrefix, strings.ToLower(strings.TrimSpace(appId)), leaf)
}

func ciPushApiKeyStatus(db *ChDbConnection, appId string) map[string]any {
	targetAppId := strings.TrimSpace(appId)
	if targetAppId == "" {
		return map[string]any{
			"app_id":           "",
			"configured":       false,
			"expires_at":       "",
			"rotated_at":       "",
			"hash":             "",
			"realtime_enabled": false,
			"expiry": map[string]any{
				"state":          "missing",
				"expires_at":     "",
				"days_remaining": nil,
				"message":        "CI push API key not configured",
			},
		}
	}

	keyHash := strings.TrimSpace(loadAiSetting(db, ciPushSettingKey(targetAppId, "hash"), ""))
	expiresAt := strings.TrimSpace(loadAiSetting(db, ciPushSettingKey(targetAppId, "expires_at"), ""))
	rotatedAt := strings.TrimSpace(loadAiSetting(db, ciPushSettingKey(targetAppId, "rotated_at"), ""))
	realtimeRaw := strings.ToLower(strings.TrimSpace(loadAiSetting(db, ciPushSettingKey(targetAppId, "realtime_enabled"), "false")))
	realtimeEnabled := realtimeRaw == "1" || realtimeRaw == "true" || realtimeRaw == "yes"

	expiryStatus := githubTokenExpiryStatus(expiresAt)
	if keyHash == "" {
		expiryStatus = map[string]any{
			"state":          "missing",
			"expires_at":     "",
			"days_remaining": nil,
			"message":        "CI push API key not configured",
		}
	}

	return map[string]any{
		"app_id":           targetAppId,
		"configured":       keyHash != "",
		"expires_at":       expiresAt,
		"rotated_at":       rotatedAt,
		"hash":             keyHash,
		"realtime_enabled": realtimeEnabled,
		"expiry":           expiryStatus,
	}
}

func isValidCiPushApiKey(db *ChDbConnection, appId, providedKey string) bool {
	candidate := strings.TrimSpace(providedKey)
	if candidate == "" {
		return false
	}

	meta := ciPushApiKeyStatus(db, appId)
	keyHash := rowString(meta["hash"])
	if keyHash == "" {
		return false
	}

	expiryState := ""
	if expiry, ok := meta["expiry"].(map[string]any); ok {
		expiryState = strings.ToLower(rowString(expiry["state"]))
	}
	if expiryState == "expired" {
		return false
	}

	if !strings.HasPrefix(keyHash, ciPushHashPrefix) {
		return false
	}

	candidateHash := hashApiKey(candidate)
	// hmac.compare_digest equivalent.
	return len(candidateHash) == len(keyHash) &&
		subtle.ConstantTimeCompare([]byte(candidateHash), []byte(keyHash)) == 1
}

func setCiPushRealtimeEnabled(db *ChDbConnection, appId string, enabled bool) {
	targetAppId := strings.TrimSpace(appId)
	if targetAppId == "" {
		return
	}
	value := "false"
	if enabled {
		value = "true"
	}
	saveAiSetting(db, ciPushSettingKey(targetAppId, "realtime_enabled"), value)
}

func rotateCiPushApiKey(db *ChDbConnection, appId string, ttlDays int) (string, string) {
	targetAppId := strings.TrimSpace(appId)
	if targetAppId == "" {
		return "", ""
	}
	normalizedTtl := normalizeTtlDays(ttlDays)
	plain := generateCiPushApiKey()
	expiresAt := ciPushExpiryIsoFromDays(normalizedTtl)
	saveAiSetting(db, ciPushSettingKey(targetAppId, "hash"), hashApiKey(plain))
	saveAiSetting(db, ciPushSettingKey(targetAppId, "expires_at"), expiresAt)
	saveAiSetting(db, ciPushSettingKey(targetAppId, "rotated_at"), nowIso())
	return plain, expiresAt
}

func revokeCiPushApiKey(db *ChDbConnection, appId string) {
	targetAppId := strings.TrimSpace(appId)
	if targetAppId == "" {
		return
	}
	saveAiSetting(db, ciPushSettingKey(targetAppId, "hash"), "")
	saveAiSetting(db, ciPushSettingKey(targetAppId, "expires_at"), "")
	saveAiSetting(db, ciPushSettingKey(targetAppId, "rotated_at"), nowIso())
}

// validateGithubToken checks a GitHub token against the rate_limit endpoint.
// Returns (status, message).
func validateGithubToken(githubToken string) (string, string) {
	token := strings.TrimSpace(githubToken)
	if token == "" {
		return "missing", "No token configured"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/rate_limit", nil)
	if err != nil {
		return "error", fmt.Sprintf("Validation request failed: %T", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		// PORT-NOTE: Python reports exc.__class__.__name__; Go reports the
		// error's concrete type via %T.
		return "error", fmt.Sprintf("Validation request failed: %T", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case 200:
		return "valid", "Token is valid"
	case 401:
		return "invalid", "Token rejected (401 Unauthorized)"
	case 403:
		return "error", "GitHub returned 403 (forbidden or rate-limited)"
	default:
		return "error", fmt.Sprintf("GitHub returned HTTP %d", resp.StatusCode)
	}
}

// queryPageEnabled: the Query page is available when an AI model and endpoint
// are configured. Pass nil to load settings from the DB (Python default None).
func queryPageEnabled(settings map[string]string) bool {
	if settings == nil {
		db := getDb()
		settings = loadAllAiSettings(db)
	}
	return strings.TrimSpace(settings["ai.endpoint_url"]) != "" && strings.TrimSpace(settings["ai.model"]) != ""
}

// kubernetesEnabled returns true when the Kubernetes health view is enabled
// in settings.
func kubernetesEnabled() (enabled bool) {
	defer func() {
		if r := recover(); r != nil {
			enabled = false
		}
	}()
	db := getDb()
	value := getAppSetting(db, "kubernetes.enabled")
	return value == "1"
}

const mobileBreakpointMax = "575.98px"

// injectFeatureFlags ports the @app.context_processor; renderTemplate (s00)
// merges its result into every template context.
func injectFeatureFlags() map[string]any {
	var result map[string]any
	func() {
		defer func() {
			if r := recover(); r != nil {
				result = nil
			}
		}()
		// Per-issue masking override is only effective when global masking is OFF.
		raiseIssueMaskToggleEffective := !isOutputMaskingEnabled(nil)
		result = map[string]any{
			"query_enabled":                     queryPageEnabled(nil),
			"kubernetes_enabled":                kubernetesEnabled(),
			"raise_issue_mask_toggle_effective": raiseIssueMaskToggleEffective,
			"mobile_breakpoint_max":             mobileBreakpointMax,
			"sobs_version":                      sobsVersionLabel(),
		}
	}()
	if result == nil {
		return map[string]any{
			"query_enabled":                     false,
			"kubernetes_enabled":                false,
			"raise_issue_mask_toggle_effective": false,
			"mobile_breakpoint_max":             mobileBreakpointMax,
			"sobs_version":                      sobsVersionLabel(),
		}
	}
	return result
}

// sobsVersionLabel mirrors `BUILD_VERSION or "dev"`.
func sobsVersionLabel() string {
	if buildVersion != "" {
		return buildVersion
	}
	return "dev"
}
