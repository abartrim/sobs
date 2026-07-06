package main

// s10_metrics_errors_traces.go — port of app.py lines 13310-15679:
// signal label registry, Web UI Metrics (GET /metrics), Metrics Rules pages
// (incl. auto rules + auto dashboard), Metrics Anomaly details page, Web UI
// Errors (incl. _load_work_item_links_for_ref_ids and the error resolve
// route), Web UI Traces, and metric series grouping / health chip helpers.

import (
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func init() {
	registerRoute("GET", "/metrics", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(telemetryTracedView(
			"sobs.dashboard.query",
			map[string]any{"dashboard.name": "metrics", "route": "/metrics"},
		)(viewMetrics))(w, r)
	})
	registerRoute("GET", "/metrics/rules", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(viewMetricsRules)(w, r)
	})
	registerRoute("POST", "/metrics/rules", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(createMetricsRule)(w, r)
	})
	registerRoute("POST", "/metrics/rules/auto", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(autoMetricsRules)(w, r)
	})
	registerRoute("POST", "/metrics/rules/dashboard/auto", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(autoMetricsRulesDashboard)(w, r)
	})
	registerRoute("POST", "/metrics/rules/{rule_id}/delete", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(deleteMetricsRule)(w, r)
	})
	registerRoute("GET", "/metrics/anomaly", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(viewMetricsAnomaly)(w, r)
	})
	registerRoute("GET", "/errors", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(telemetryTracedView(
			"sobs.dashboard.query",
			map[string]any{"dashboard.name": "errors", "route": "/errors"},
		)(viewErrors))(w, r)
	})
	registerRoute("POST", "/errors/{error_id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(resolveError)(w, r)
	})
	registerRoute("GET", "/traces", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(telemetryTracedView(
			"sobs.dashboard.query",
			map[string]any{"dashboard.name": "traces", "route": "/traces"},
		)(viewTraces))(w, r)
	})
	// Template globals (signal_label/signal_description/source_label) and the
	// `mask` filter are registered by s00's gonja setup (initTemplateEngine).
}

// ---------------------------------------------------------------------------
// Signal label registry – human-friendly names for derived signal identifiers
// ---------------------------------------------------------------------------

// Mapping of (source, signal_name) → {label, description}.
// Used by templates and API responses to show readable names alongside raw IDs.
var signalLabels = map[[2]string]map[string]string{
	// Logs-derived signals
	{"logs", "log_volume"}: {
		"label":       "Log Volume",
		"description": "Log lines ingested per minute",
	},
	{"logs", "error_volume"}: {
		"label":       "Error Volume",
		"description": "Error-level log lines per minute",
	},
	{"logs", "error_ratio"}: {
		"label":       "Error Ratio",
		"description": "Fraction of log lines that are errors",
	},
	// Traces-derived signals
	{"traces", "trace_volume"}: {
		"label":       "Trace Volume",
		"description": "Completed spans per minute",
	},
	{"traces", "trace_error_ratio"}: {
		"label":       "Trace Error Ratio",
		"description": "Fraction of spans with an error status",
	},
	{"traces", "latency_p95_ms"}: {
		"label":       "Latency p95",
		"description": "95th-percentile span duration (ms)",
	},
	// Errors-derived signals
	{"errors", "exception_volume"}: {
		"label":       "Exception Volume",
		"description": "Exception events per minute",
	},
	// RUM Web Vitals
	{"rum_vitals", "LCP"}: {
		"label":       "Largest Contentful Paint",
		"description": "Core Web Vital: LCP (ms) – measures loading performance",
	},
	{"rum_vitals", "INP"}: {
		"label":       "Interaction to Next Paint",
		"description": "Core Web Vital: INP (ms) – measures interactivity",
	},
	{"rum_vitals", "CLS"}: {
		"label":       "Cumulative Layout Shift",
		"description": "Core Web Vital: CLS (unitless) – measures visual stability",
	},
	{"rum_vitals", "TTFB"}: {
		"label":       "Time to First Byte",
		"description": "Core Web Vital: TTFB (ms) – measures server response time",
	},
	{"rum_vitals", "FCP"}: {
		"label":       "First Contentful Paint",
		"description": "Core Web Vital: FCP (ms) – measures perceived load speed",
	},
	{"rum_vitals", "FID"}: {
		"label":       "First Input Delay",
		"description": "Core Web Vital: FID (ms) – measures input responsiveness",
	},
}

// Human-friendly labels for signal sources.
var sourceLabels = map[string]string{
	"logs":       "Logs",
	"traces":     "Traces",
	"errors":     "Errors",
	"rum_vitals": "RUM Vitals",
	"metrics":    "Metrics",
}

// pyTitle mirrors Python str.title(): capitalises the first letter of every
// alpha run and lowercases the rest.
func pyTitle(s string) string {
	var b strings.Builder
	prevAlpha := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			if prevAlpha {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(unicode.ToUpper(r))
			}
			prevAlpha = true
		} else {
			b.WriteRune(r)
			prevAlpha = false
		}
	}
	return b.String()
}

// signalLabel returns a human-friendly label for a (source, signal) pair.
//
// Falls back to a title-cased version of *signal* when the pair is not
// registered.
func signalLabel(source string, signal string) string {
	if entry, ok := signalLabels[[2]string{source, signal}]; ok {
		return entry["label"]
	}
	// Capitalise underscored names as a best-effort fallback.
	return pyTitle(strings.ReplaceAll(signal, "_", " "))
}

// signalDescription returns a short description for a (source, signal) pair, or empty string.
func signalDescription(source string, signal string) string {
	if entry, ok := signalLabels[[2]string{source, signal}]; ok {
		return entry["description"]
	}
	return ""
}

// sourceLabel returns a human-friendly label for a signal source.
//
// Falls back to the raw *source* identifier when not registered.
func sourceLabel(source string) string {
	if label, ok := sourceLabels[source]; ok {
		return label
	}
	return pyTitle(strings.ReplaceAll(source, "_", " "))
}

// ---------------------------------------------------------------------------
// Web UI – Metrics (derived signal index)
// ---------------------------------------------------------------------------
func viewMetrics(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	query := r.URL.Query()
	selectedServices := []string{}
	for _, svc := range query["service"] {
		if v := strings.TrimSpace(svc); v != "" {
			selectedServices = append(selectedServices, v)
		}
	}
	selectedSignals := []string{}
	for _, sig := range query["signal"] {
		if v := strings.TrimSpace(sig); v != "" {
			selectedSignals = append(selectedSignals, v)
		}
	}
	selectedSources := []string{}
	for _, src := range query["source"] {
		if v := strings.TrimSpace(src); v != "" {
			selectedSources = append(selectedSources, v)
		}
	}
	service := ""
	if len(selectedServices) > 0 {
		service = selectedServices[0]
	}
	signal := ""
	if len(selectedSignals) > 0 {
		signal = selectedSignals[0]
	}
	source := ""
	if len(selectedSources) > 0 {
		source = selectedSources[0]
	}
	attrFp := strings.TrimSpace(query.Get("attr_fp"))
	q := strings.TrimSpace(query.Get("q"))
	fromTs, toTs, timeError := parseTimeWindowArgs(r)
	limit := parseLimit(r, 100)
	offset := parseOffset(r)
	sortBy, sortCol, sortDir := parseSort(r,
		map[string]string{
			"last_time":          "last_time",
			"service":            "service",
			"source":             "source",
			"signal":             "signal",
			"last_value":         "last_value",
			"last_anomaly_score": "last_anomaly_score",
			"last_anomaly_state": "last_anomaly_state",
			"last_sample_count":  "last_sample_count",
			"point_count":        "point_count",
		},
		"last_time",
	)
	orderDir := "DESC"
	if sortDir == "asc" {
		orderDir = "ASC"
	}
	orderClause := fmt.Sprintf("ORDER BY %s %s", sortCol, orderDir)

	hours := 24
	if raw := query.Get("hours"); raw != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			hours = max(1, min(168, v))
		}
	}

	whereParts := []string{}
	params := []any{}
	if len(selectedServices) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selectedServices)), ",")
		whereParts = append(whereParts, fmt.Sprintf("ServiceName IN (%s)", placeholders))
		for _, v := range selectedServices {
			params = append(params, v)
		}
	}
	if len(selectedSignals) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selectedSignals)), ",")
		whereParts = append(whereParts, fmt.Sprintf("SignalName IN (%s)", placeholders))
		for _, v := range selectedSignals {
			params = append(params, v)
		}
	}
	if len(selectedSources) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selectedSources)), ",")
		whereParts = append(whereParts, fmt.Sprintf("SignalSource IN (%s)", placeholders))
		for _, v := range selectedSources {
			params = append(params, v)
		}
	}
	if attrFp != "" {
		whereParts = append(whereParts, "AttrFingerprint = ?")
		params = append(params, attrFp)
	}

	if timeError == "" {
		appendTimeWindowFilter(&whereParts, &params, "time", fromTs, toTs)
	}

	hourClause := ""
	if fromTs == "" && toTs == "" {
		hourClause = "time >= now() - INTERVAL ? HOUR"
	}

	rows := []map[string]any{}
	total := 0
	errorMsg := timeError
	includePatterns := []string{}
	excludePatterns := []string{}
	if q != "" && errorMsg == "" {
		var regexError string
		includePatterns, excludePatterns, regexError = prepareRe2FilterPatterns(db, q)
		if regexError != "" {
			errorMsg = regexError
		} else {
			appendRegexExpressionClauses(&whereParts, &params, "SignalName", includePatterns, excludePatterns)
		}
	}

	if hourClause != "" {
		params = append(params, hours)
	}

	whereClauseSql := ""
	if len(whereParts) > 0 {
		whereClauseSql = " " + whereClause(whereParts)
	}
	if hourClause != "" {
		if whereClauseSql != "" {
			whereClauseSql = fmt.Sprintf("%s AND %s", whereClauseSql, hourClause)
		} else {
			whereClauseSql = fmt.Sprintf(" WHERE %s", hourClause)
		}
	}

	if errorMsg == "" {
		queryErr := func() error {
			groupedSql := "SELECT" +
				"  ServiceName AS service," +
				"  SignalSource AS source," +
				"  SignalName AS signal," +
				"  AttrFingerprint AS attr_fp," +
				"  max(time) AS last_time," +
				"  argMax(value, time) AS last_value," +
				"  argMax(anomaly_score, time) AS last_anomaly_score," +
				"  argMax(anomaly_state, time) AS last_anomaly_state," +
				"  argMax(SampleCount, time) AS last_sample_count," +
				"  count() AS point_count" +
				" FROM v_derived_signals_anomaly" +
				whereClauseSql +
				" GROUP BY ServiceName, SignalSource, SignalName, AttrFingerprint"

			totalRes, err := db.Execute(fmt.Sprintf("SELECT COUNT(*) FROM (%s)", groupedSql), params...)
			if err != nil {
				return err
			}
			if row := totalRes.Fetchone(); row != nil && len(totalRes.Cols) > 0 {
				total = coerceInt(row[totalRes.Cols[0]])
			}
			fetchedRes, err := db.Execute(
				fmt.Sprintf("SELECT * FROM (%s) %s LIMIT ? OFFSET ?", groupedSql, orderClause),
				append(append([]any{}, params...), limit, offset)...,
			)
			if err != nil {
				return err
			}
			for _, row := range fetchedRes.Fetchall() {
				rows = append(rows, map[string]any{
					"service":            rowString(row["service"]),
					"source":             rowString(row["source"]),
					"signal":             rowString(row["signal"]),
					"attr_fp":            rowString(row["attr_fp"]),
					"last_time":          rowString(row["last_time"]),
					"last_value":         row["last_value"],
					"last_anomaly_score": row["last_anomaly_score"],
					"last_anomaly_state": rowString(row["last_anomaly_state"]),
					"last_sample_count":  row["last_sample_count"],
					"point_count":        row["point_count"],
					"rule_name":          "",
				})
			}
			return nil
		}()
		if queryErr != nil {
			logger.Error("metrics index query failed", "error", queryErr)
			errorMsg = publicDashboardQueryError(queryErr)
		}
	}

	// PORT-NOTE: _annotate_rows_with_rules keyword args passed positionally in
	// declaration order (source/signal/service/attr_fp/value/sample_count/time keys).
	anomalyRules, err := loadAnomalyRules(db)
	if err != nil {
		logger.Error("loadAnomalyRules failed", "error", err)
	}
	annotateRowsWithRules(
		rows,
		anomalyRules,
		"source",
		"signal",
		"service",
		"attr_fp",
		"last_value",
		"last_sample_count",
		"last_time",
	)

	services, signals, sources, err := listDerivedSignalDimensions(db)
	if err != nil {
		logger.Error("listDerivedSignalDimensions failed", "error", err)
	}

	renderTemplate(w, r, "metrics.html", map[string]any{
		"rows":              rows,
		"total":             total,
		"limit":             limit,
		"offset":            offset,
		"service":           service,
		"selected_services": selectedServices,
		"signal":            signal,
		"selected_signals":  selectedSignals,
		"source":            source,
		"selected_sources":  selectedSources,
		"attr_fp":           attrFp,
		"q":                 q,
		"from_ts":           fromTs,
		"to_ts":             toTs,
		"hours":             hours,
		"error_msg":         errorMsg,
		"services":          services,
		"signals":           signals,
		"sources":           sources,
		"sort_by":           sortBy,
		"sort_dir":          sortDir,
	})
}

// ---------------------------------------------------------------------------
// Web UI – Metrics Rules
// ---------------------------------------------------------------------------
func viewMetricsRules(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	openPanel := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("open_panel")))
	if openPanel != "auto-rules" && openPanel != "auto-dashboard" {
		openPanel = ""
	}
	services, signals, sources, err := listDerivedSignalDimensions(db)
	if err != nil {
		logger.Error("listDerivedSignalDimensions failed", "error", err)
	}
	rules, err := loadAnomalyRules(db)
	if err != nil {
		logger.Error("loadAnomalyRules failed", "error", err)
	}
	renderTemplate(w, r, "metrics_rules.html", map[string]any{
		"rules":                  rules,
		"services":               services,
		"signals":                signals,
		"sources":                sources,
		"auto_preview":           []map[string]any{},
		"auto_summary":           nil,
		"auto_dashboard_preview": []map[string]any{},
		"auto_dashboard_summary": nil,
		"auto_open_panel":        openPanel,
	})
}

func createMetricsRule(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	// PORT-NOTE: Python's (form.get(x) or default).strip().lower() — the default
	// applies only when the raw value is empty, before strip.
	ruleTypeRaw := r.FormValue("rule_type")
	if ruleTypeRaw == "" {
		ruleTypeRaw = "threshold"
	}
	ruleType := strings.ToLower(strings.TrimSpace(ruleTypeRaw))
	source := strings.TrimSpace(r.FormValue("source"))
	signal := strings.TrimSpace(r.FormValue("signal"))
	service := strings.TrimSpace(r.FormValue("service"))
	attrFp := strings.TrimSpace(r.FormValue("attr_fp"))
	comparatorRaw := r.FormValue("comparator")
	if comparatorRaw == "" {
		comparatorRaw = "gt"
	}
	comparator := strings.ToLower(strings.TrimSpace(comparatorRaw))
	secondarySource := strings.TrimSpace(r.FormValue("secondary_source"))
	secondarySignal := strings.TrimSpace(r.FormValue("secondary_signal"))
	secondaryComparatorRaw := r.FormValue("secondary_comparator")
	if secondaryComparatorRaw == "" {
		secondaryComparatorRaw = "gt"
	}
	secondaryComparator := strings.ToLower(strings.TrimSpace(secondaryComparatorRaw))

	redirectToRules := func() {
		http.Redirect(w, r, "/metrics/rules", http.StatusFound)
	}

	if name == "" || source == "" || signal == "" {
		flashMessage(w, r, "Rule name, source, and signal are required", "warning")
		redirectToRules()
		return
	}

	if ruleType != "threshold" && ruleType != "composite" {
		flashMessage(w, r, "Rule type must be 'threshold' or 'composite'", "warning")
		redirectToRules()
		return
	}

	if comparator != "gt" && comparator != "lt" {
		flashMessage(w, r, "Comparator must be 'gt' or 'lt'", "warning")
		redirectToRules()
		return
	}
	if secondaryComparator != "gt" && secondaryComparator != "lt" {
		flashMessage(w, r, "Secondary comparator must be 'gt' or 'lt'", "warning")
		redirectToRules()
		return
	}

	// PORT-NOTE: mirrors Python float()/int() ValueError handling — any parse
	// failure flashes one combined warning.
	parseFailed := false
	parseFloatForm := func(field string, def string) float64 {
		raw := r.FormValue(field)
		if raw == "" {
			raw = def
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			parseFailed = true
		}
		return v
	}
	warningThreshold := parseFloatForm("warning_threshold", "")
	criticalThreshold := parseFloatForm("critical_threshold", "")
	minSampleCount := 1
	if raw := r.FormValue("min_sample_count"); raw != "" {
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			parseFailed = true
		}
		minSampleCount = max(1, v)
	}
	secondaryWarningThreshold := parseFloatForm("secondary_warning_threshold", "0")
	secondaryCriticalThreshold := parseFloatForm("secondary_critical_threshold", "0")
	if parseFailed {
		flashMessage(w, r, "Thresholds must be numeric and sample count must be an integer", "warning")
		redirectToRules()
		return
	}

	if comparator == "gt" && criticalThreshold < warningThreshold {
		flashMessage(w, r, "For 'gt' rules, critical threshold must be >= warning threshold", "warning")
		redirectToRules()
		return
	}
	if comparator == "lt" && criticalThreshold > warningThreshold {
		flashMessage(w, r, "For 'lt' rules, critical threshold must be <= warning threshold", "warning")
		redirectToRules()
		return
	}
	if ruleType == "composite" {
		if secondarySource == "" || secondarySignal == "" {
			flashMessage(w, r, "Composite rules require a secondary source and signal", "warning")
			redirectToRules()
			return
		}
		if secondaryComparator == "gt" && secondaryCriticalThreshold < secondaryWarningThreshold {
			flashMessage(w, r, "For secondary 'gt' rules, critical threshold must be >= warning threshold", "warning")
			redirectToRules()
			return
		}
		if secondaryComparator == "lt" && secondaryCriticalThreshold > secondaryWarningThreshold {
			flashMessage(w, r, "For secondary 'lt' rules, critical threshold must be <= warning threshold", "warning")
			redirectToRules()
			return
		}
	} else {
		secondarySource = ""
		secondarySignal = ""
		secondaryComparator = "gt"
		secondaryWarningThreshold = 0.0
		secondaryCriticalThreshold = 0.0
	}

	ruleId := agentUuid4()
	version := time.Now().UnixMilli()
	if _, err := insertRowsJsonEachRow(
		getDb(),
		"sobs_anomaly_rules",
		[]Row{
			{
				"Id":                         ruleId,
				"Name":                       name,
				"RuleType":                   ruleType,
				"SignalSource":               source,
				"SignalName":                 signal,
				"ServiceName":                service,
				"AttrFingerprint":            attrFp,
				"Comparator":                 comparator,
				"WarningThreshold":           warningThreshold,
				"CriticalThreshold":          criticalThreshold,
				"SecondarySignalSource":      secondarySource,
				"SecondarySignalName":        secondarySignal,
				"SecondaryComparator":        secondaryComparator,
				"SecondaryWarningThreshold":  secondaryWarningThreshold,
				"SecondaryCriticalThreshold": secondaryCriticalThreshold,
				"MinSampleCount":             minSampleCount,
				"IsDeleted":                  0,
				"Version":                    version,
			},
		},
	); err != nil {
		// PORT-NOTE: Python lets _insert_rows_json_each_row raise → 500.
		logger.Error("create metrics rule insert failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	flashMessage(w, r, fmt.Sprintf("Rule '%s' created", name), "success")
	redirectToRules()
}

func autoMetricsRules(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	actionRaw := r.FormValue("action")
	if actionRaw == "" {
		actionRaw = "preview"
	}
	action := strings.ToLower(strings.TrimSpace(actionRaw))
	hours := 24
	if raw := r.FormValue("hours"); raw != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			hours = max(1, min(168, v))
		}
	}
	minPoints := 30
	if raw := r.FormValue("min_points"); raw != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			minPoints = max(1, min(5000, v))
		}
	}

	serviceFilter := strings.TrimSpace(r.FormValue("service_filter"))
	includeAttrFpRaw := r.FormValue("include_attr_fp")
	includeAttrFp := includeAttrFpRaw == "1" || includeAttrFpRaw == "true" || includeAttrFpRaw == "on" || includeAttrFpRaw == "yes"
	modeRaw := r.FormValue("mode")
	if modeRaw == "" {
		modeRaw = "threshold"
	}
	mode := strings.ToLower(strings.TrimSpace(modeRaw))
	if mode != "threshold" && mode != "seasonal" {
		mode = "threshold"
	}
	seasonalStrategyRaw := r.FormValue("seasonal_strategy")
	if seasonalStrategyRaw == "" {
		seasonalStrategyRaw = "hour_of_day"
	}
	seasonalStrategy := strings.ToLower(strings.TrimSpace(seasonalStrategyRaw))
	// PORT-NOTE: assumes _SEASONAL_STRATEGIES ports as []string seasonalStrategies.
	if !slices.Contains(seasonalStrategies, seasonalStrategy) {
		seasonalStrategy = "hour_of_day"
	}

	db := getDb()
	services, signals, sources, err := listDerivedSignalDimensions(db)
	if err != nil {
		logger.Error("listDerivedSignalDimensions failed", "error", err)
	}
	existingRules, err := loadAnomalyRules(db)
	if err != nil {
		logger.Error("loadAnomalyRules failed", "error", err)
	}

	var candidates []map[string]any
	var stats map[string]int
	if mode == "seasonal" {
		candidates, stats, err = buildSeasonalMetricRuleCandidates(
			db,
			hours,
			minPoints,
			serviceFilter,
			includeAttrFp,
			seasonalStrategy,
		)
	} else {
		candidates, stats, err = buildAutoMetricRuleCandidates(
			db,
			hours,
			minPoints,
			serviceFilter,
			includeAttrFp,
		)
	}
	if err != nil {
		logger.Error("building metric rule candidates failed", "error", err)
	}

	summary := map[string]any{
		"action":            action,
		"hours":             hours,
		"min_points":        minPoints,
		"service_filter":    serviceFilter,
		"include_attr_fp":   includeAttrFp,
		"mode":              mode,
		"seasonal_strategy": seasonalStrategy,
		"examined":          stats["examined"],
		"existing":          stats["existing"],
		"invalid":           stats["invalid"],
		"candidates":        len(candidates),
		"create_cap":        autoRuleCreateMax,
		"capped":            len(candidates) > autoRuleCreateMax,
		"created":           0,
	}

	if action == "create" {
		limitedCandidates := candidates
		if len(limitedCandidates) > autoRuleCreateMax {
			limitedCandidates = limitedCandidates[:autoRuleCreateMax]
		}
		nowVersion := time.Now().UnixMilli()
		rowsToInsert := []Row{}
		for idx, candidate := range limitedCandidates {
			ruleType := "threshold"
			if v, ok := candidate["rule_type"]; ok {
				ruleType = rowString(v)
			}
			warningThreshold, _ := coerceFloat(candidate["warning_threshold"])
			criticalThreshold, _ := coerceFloat(candidate["critical_threshold"])
			rowsToInsert = append(rowsToInsert, Row{
				"Id":                         agentUuid4(),
				"Name":                       rowString(candidate["name"]),
				"RuleType":                   ruleType,
				"SignalSource":               rowString(candidate["source"]),
				"SignalName":                 rowString(candidate["signal"]),
				"ServiceName":                rowString(candidate["service"]),
				"AttrFingerprint":            rowString(candidate["attr_fp"]),
				"Comparator":                 rowString(candidate["comparator"]),
				"WarningThreshold":           warningThreshold,
				"CriticalThreshold":          criticalThreshold,
				"SecondarySignalSource":      "",
				"SecondarySignalName":        "",
				"SecondaryComparator":        "gt",
				"SecondaryWarningThreshold":  0.0,
				"SecondaryCriticalThreshold": 0.0,
				"MinSampleCount":             coerceInt(candidate["min_sample_count"]),
				"SeasonalBucketsJson":        rowString(candidate["seasonal_buckets_json"]),
				"IsDeleted":                  0,
				"Version":                    nowVersion + int64(idx),
			})
		}

		if len(rowsToInsert) > 0 {
			if _, err := insertRowsJsonEachRow(db, "sobs_anomaly_rules", rowsToInsert); err != nil {
				logger.Error("auto metrics rules insert failed", "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
		}
		summary["created"] = len(rowsToInsert)
		skippedByCap := max(0, len(candidates)-len(limitedCandidates))
		capSuffix := "."
		if skippedByCap > 0 {
			capSuffix = fmt.Sprintf(", skipped %d by max cap (%d).", skippedByCap, autoRuleCreateMax)
		}
		flashMessage(w, r,
			fmt.Sprintf(
				"Auto rule generation complete: created %d rule(s), skipped %d existing, %d invalid%s",
				summary["created"], summary["existing"], summary["invalid"], capSuffix,
			),
			"success",
		)
		http.Redirect(w, r, "/metrics/rules?open_panel=auto-rules", http.StatusFound)
		return
	}

	flashMessage(w, r,
		fmt.Sprintf(
			"Auto-rule preview: %d candidate(s), %d existing skipped, %d invalid.",
			summary["candidates"], summary["existing"], summary["invalid"],
		),
		"info",
	)
	renderTemplate(w, r, "metrics_rules.html", map[string]any{
		"rules":                  existingRules,
		"services":               services,
		"signals":                signals,
		"sources":                sources,
		"auto_preview":           candidates,
		"auto_summary":           summary,
		"auto_dashboard_preview": []map[string]any{},
		"auto_dashboard_summary": nil,
		"auto_open_panel":        "auto-rules",
	})
}

func autoMetricsRulesDashboard(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	actionRaw := r.FormValue("action")
	if actionRaw == "" {
		actionRaw = "preview"
	}
	action := strings.ToLower(strings.TrimSpace(actionRaw))
	serviceFilter := strings.TrimSpace(r.FormValue("service_filter"))
	hours := coercePositiveInt(r.FormValue("hours"), 24, 1, 168)
	maxCharts := coercePositiveInt(
		r.FormValue("max_charts"),
		12,
		1,
		autoDashboardCreateMax,
	)
	dashboardName := strings.TrimSpace(r.FormValue("dashboard_name"))
	if dashboardName == "" {
		dashboardName = defaultAutoDashboardName(serviceFilter)
	}

	db := getDb()
	services, signals, sources, err := listDerivedSignalDimensions(db)
	if err != nil {
		logger.Error("listDerivedSignalDimensions failed", "error", err)
	}
	rules, err := loadAnomalyRules(db)
	if err != nil {
		logger.Error("loadAnomalyRules failed", "error", err)
	}
	candidates := buildAutoDashboardChartCandidates(
		rules,
		serviceFilter,
		hours,
	)
	cappedCandidates := candidates
	if len(cappedCandidates) > maxCharts {
		cappedCandidates = cappedCandidates[:maxCharts]
	}

	summary := map[string]any{
		"action":         action,
		"hours":          hours,
		"service_filter": serviceFilter,
		"max_charts":     maxCharts,
		"create_cap":     autoDashboardCreateMax,
		"dashboard_name": dashboardName,
		"rules_total":    len(rules),
		"candidates":     len(candidates),
		"capped":         len(candidates) > maxCharts,
		"created":        0,
		"existing":       0,
	}

	if action == "create" {
		if len(cappedCandidates) == 0 {
			flashMessage(w, r, "No matching rules found for dashboard generation", "warning")
			http.Redirect(w, r, "/metrics/rules?open_panel=auto-dashboard", http.StatusFound)
			return
		}

		scope := serviceFilter
		if serviceFilter == "" {
			scope = "all services"
		}
		dashboardDescription := fmt.Sprintf(
			"Auto-generated from active metric rules. window=%dh, scope=%s.",
			hours, scope,
		)
		dashboardId := seedDashboardIfMissing(db, dashboardName, dashboardDescription)

		existingCharts := getCharts(db, dashboardId)
		existingTitles := map[string]bool{}
		nextPosition := -1
		for _, chart := range existingCharts {
			existingTitles[rowString(chart["title"])] = true
			if pos := coerceInt(chart["position"]); pos > nextPosition {
				nextPosition = pos
			}
		}
		nextPosition++
		nextVersion := time.Now().UnixMilli()
		rowsToInsert := []Row{}
		existingCount := 0

		for idx, candidate := range cappedCandidates {
			title := rowString(candidate["title"])
			if existingTitles[title] {
				existingCount++
				continue
			}
			query := rowString(candidate["query"])
			chartType := rowString(candidate["chart_type"])
			rowsToInsert = append(rowsToInsert, Row{
				"Id":          agentUuid4(),
				"DashboardId": dashboardId,
				"Title":       title,
				"ChartType":   chartType,
				"Query":       query,
				"OptionsJson": jsonDumpsNoEscape(
					map[string]any{"chart_spec": buildRawChartSpec(chartType, query, "")},
				),
				"Position":  nextPosition + idx,
				"IsDeleted": 0,
				"Version":   nextVersion + int64(idx),
			})
			existingTitles[title] = true
		}
		summary["existing"] = existingCount

		if len(rowsToInsert) > 0 {
			if _, err := insertRowsJsonEachRow(db, "sobs_chart_configs", rowsToInsert); err != nil {
				logger.Error("auto dashboard chart insert failed", "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
		}
		summary["created"] = len(rowsToInsert)

		skippedByMax := max(0, len(candidates)-len(cappedCandidates))
		capNote := ""
		if skippedByMax > 0 {
			capNote = fmt.Sprintf(", skipped %d by selected max (%d)", skippedByMax, maxCharts)
		}
		flashMessage(w, r,
			fmt.Sprintf(
				"Auto dashboard ready: created %d chart(s), skipped %d existing%s.",
				summary["created"], summary["existing"], capNote,
			),
			"success",
		)
		http.Redirect(w, r, "/dashboards/"+dashboardId, http.StatusFound)
		return
	}

	flashMessage(w, r,
		fmt.Sprintf(
			"Auto-dashboard preview: %d candidate chart(s) from %d rule(s).",
			summary["candidates"], summary["rules_total"],
		),
		"info",
	)
	renderTemplate(w, r, "metrics_rules.html", map[string]any{
		"rules":                  rules,
		"services":               services,
		"signals":                signals,
		"sources":                sources,
		"auto_preview":           []map[string]any{},
		"auto_summary":           nil,
		"auto_dashboard_preview": candidates,
		"auto_dashboard_summary": summary,
		"auto_open_panel":        "auto-dashboard",
	})
}

func deleteMetricsRule(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	ruleId := r.PathValue("rule_id")

	deletedRow := func(row Row) Row {
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
		secondaryWarningThreshold, _ := coerceFloat(row["SecondaryWarningThreshold"])
		secondaryCriticalThreshold, _ := coerceFloat(row["SecondaryCriticalThreshold"])
		return Row{
			"Id":                         rowString(row["Id"]),
			"Name":                       rowString(row["Name"]),
			"RuleType":                   ruleType,
			"SignalSource":               rowString(row["SignalSource"]),
			"SignalName":                 rowString(row["SignalName"]),
			"ServiceName":                rowString(row["ServiceName"]),
			"AttrFingerprint":            rowString(row["AttrFingerprint"]),
			"Comparator":                 rowString(row["Comparator"]),
			"WarningThreshold":           warningThreshold,
			"CriticalThreshold":          criticalThreshold,
			"SecondarySignalSource":      rowString(row["SecondarySignalSource"]),
			"SecondarySignalName":        rowString(row["SecondarySignalName"]),
			"SecondaryComparator":        secondaryComparator,
			"SecondaryWarningThreshold":  secondaryWarningThreshold,
			"SecondaryCriticalThreshold": secondaryCriticalThreshold,
			"MinSampleCount":             coerceInt(row["MinSampleCount"]),
		}
	}

	// PORT-NOTE: _soft_delete_latest_row is owned by another section; the
	// keyword-only args (incl. category defaults "warning"/"success") are
	// passed positionally in declaration order, with (w, r) prepended for
	// flash/redirect handling.
	softDeleteLatestRow(
		w, r, db,
		"SELECT Id, Name, RuleType, SignalSource, SignalName, ServiceName, AttrFingerprint, Comparator, "+
			"WarningThreshold, CriticalThreshold, SecondarySignalSource, SecondarySignalName, "+
			"SecondaryComparator, SecondaryWarningThreshold, SecondaryCriticalThreshold, MinSampleCount "+
			"FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 AND Id = ?",
		[]any{ruleId},
		"sobs_anomaly_rules",
		deletedRow,
		"Rule not found",
		"Rule '{name}' deleted",
		"view_metrics_rules",
		"warning",
		"success",
	)
}

// ---------------------------------------------------------------------------
// Web UI – Metrics Anomaly Details
// ---------------------------------------------------------------------------
func viewMetricsAnomaly(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	query := r.URL.Query()
	service := strings.TrimSpace(query.Get("service"))
	metric := strings.TrimSpace(query.Get("metric"))
	signal := strings.TrimSpace(query.Get("signal"))
	source := strings.TrimSpace(query.Get("source"))
	attrFp := strings.TrimSpace(query.Get("attr_fp"))
	fromTs, toTs, timeError := parseTimeWindowArgs(r)

	// Optional metadata passed from chart click for point-level context.
	pointState := strings.TrimSpace(query.Get("_anomaly_state"))
	pointScore := strings.TrimSpace(query.Get("_anomaly_score"))

	hours := 24
	if raw := query.Get("hours"); raw != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			hours = max(1, min(168, v))
		}
	}

	whereParts := []string{}
	params := []any{}
	if service != "" {
		whereParts = append(whereParts, "ServiceName = ?")
		params = append(params, service)
	}
	if metric != "" {
		whereParts = append(whereParts, "MetricName = ?")
		params = append(params, metric)
	}
	if signal != "" {
		whereParts = append(whereParts, "SignalName = ?")
		params = append(params, signal)
	}
	if source != "" {
		whereParts = append(whereParts, "SignalSource = ?")
		params = append(params, source)
	}
	if attrFp != "" {
		whereParts = append(whereParts, "AttrFingerprint = ?")
		params = append(params, attrFp)
	}

	if timeError == "" {
		timeConditions, timeParams := timeWindowConditions("time", fromTs, toTs)
		whereParts = append(whereParts, timeConditions...)
		params = append(params, timeParams...)
	}

	// Fallback to hour-based window only when explicit time window is not provided.
	hourClause := ""
	if fromTs == "" && toTs == "" {
		hourClause = "time >= now() - INTERVAL ? HOUR"
		params = append(params, hours)
	}

	whereClauseSql := ""
	if len(whereParts) > 0 {
		whereClauseSql = " WHERE " + strings.Join(whereParts, " AND ")
	}
	if hourClause != "" {
		if whereClauseSql != "" {
			whereClauseSql = fmt.Sprintf("%s AND %s", whereClauseSql, hourClause)
		} else {
			whereClauseSql = fmt.Sprintf(" WHERE %s", hourClause)
		}
	}

	rows := []map[string]any{}
	errorMsg := timeError
	relatedTarget := ""
	if source == "logs" || source == "traces" || source == "errors" {
		relatedTarget = source
	}
	activeRules, err := loadAnomalyRules(db)
	if err != nil {
		logger.Error("loadAnomalyRules failed", "error", err)
	}
	useOtelMetricsView := metric != "" && signal == "" && source == ""
	if errorMsg == "" {
		// Keep existing metric drilldown behavior and support derived signals.
		selectHead := "SELECT" +
			"  time," +
			"  ServiceName," +
			"  SignalName AS Name," +
			"  SignalSource AS Kind," +
			"  AttrFingerprint," +
			"  value," +
			"  SampleCount," +
			"  baseline_mean," +
			"  baseline_stddev," +
			"  baseline_lower," +
			"  baseline_upper," +
			"  anomaly_score," +
			"  anomaly_state" +
			" FROM v_derived_signals_anomaly"
		if useOtelMetricsView {
			selectHead = "SELECT" +
				"  time," +
				"  ServiceName," +
				"  MetricName AS Name," +
				"  MetricKind AS Kind," +
				"  AttrFingerprint," +
				"  value," +
				"  SampleCount," +
				"  baseline_mean," +
				"  baseline_stddev," +
				"  baseline_lower," +
				"  baseline_upper," +
				"  anomaly_score," +
				"  anomaly_state" +
				" FROM v_otel_metrics_anomaly"
		}
		result, err := db.Execute(
			selectHead+whereClauseSql+" ORDER BY time DESC"+" LIMIT 500",
			params...,
		)
		if err != nil {
			logger.Error("metrics anomaly detail query failed", "error", err)
			errorMsg = publicDashboardQueryError(err)
		} else {
			for _, row := range result.Fetchall() {
				relatedTargetVal := rowString(row["Kind"])
				if useOtelMetricsView {
					relatedTargetVal = ""
				}
				rows = append(rows, map[string]any{
					"time":            rowString(row["time"]),
					"service":         rowString(row["ServiceName"]),
					"metric":          rowString(row["Name"]),
					"metric_kind":     rowString(row["Kind"]),
					"related_target":  relatedTargetVal,
					"attr_fp":         rowString(row["AttrFingerprint"]),
					"value":           row["value"],
					"sample_count":    row["SampleCount"],
					"baseline_mean":   row["baseline_mean"],
					"baseline_stddev": row["baseline_stddev"],
					"baseline_lower":  row["baseline_lower"],
					"baseline_upper":  row["baseline_upper"],
					"anomaly_score":   row["anomaly_score"],
					"anomaly_state":   rowString(row["anomaly_state"]),
				})
			}
		}
	}

	if !useOtelMetricsView {
		annotateRowsWithRules(
			rows,
			activeRules,
			"related_target",
			"metric",
			"service",
			"attr_fp",
			"value",
			"sample_count",
			"time",
		)
	}

	services, signals, sources, err := listDerivedSignalDimensions(db)
	if err != nil {
		logger.Error("listDerivedSignalDimensions failed", "error", err)
	}

	renderTemplate(w, r, "metrics_anomaly.html", map[string]any{
		"rows":           rows,
		"total":          len(rows),
		"service":        service,
		"metric":         metric,
		"signal":         signal,
		"source":         source,
		"attr_fp":        attrFp,
		"from_ts":        fromTs,
		"to_ts":          toTs,
		"hours":          hours,
		"error_msg":      errorMsg,
		"point_state":    pointState,
		"point_score":    pointScore,
		"related_target": relatedTarget,
		"services":       services,
		"signals":        signals,
		"sources":        sources,
	})
}

// ---------------------------------------------------------------------------
// Web UI – Errors
// ---------------------------------------------------------------------------

// loadWorkItemLinksForRefIds returns {trigger_ref_id: {issue_url, issue_number,
// issue_state}} for already-raised issues.
//
// trigger_ref_id is stored as AnomalyRuleId (populated from
// trigger_context["trigger_ref_id"] which is error_id for errors-page raises
// and trace_id for traces-page raises).
func loadWorkItemLinksForRefIds(db *ChDbConnection, refIds []string) map[string]map[string]any {
	refSet := map[string]bool{}
	refList := []string{}
	for _, raw := range refIds {
		ref := rowString(raw)
		if ref != "" && !refSet[ref] {
			refSet[ref] = true
			refList = append(refList, ref)
		}
	}
	if len(refList) == 0 {
		return map[string]map[string]any{}
	}
	ph := make([]string, len(refList))
	for i := range ph {
		ph[i] = "?"
	}
	placeholders := strings.Join(ph, ", ")
	params := make([]any, 0, len(refList))
	for _, ref := range refList {
		params = append(params, ref)
	}
	res, err := db.Execute(
		"SELECT AnomalyRuleId, IssueUrl, CanonicalIssueUrl, IssueNumber, IssueState "+
			"FROM sobs_github_work_items FINAL "+
			fmt.Sprintf("WHERE IsDeleted=0 AND IssueUrl != '' AND AnomalyRuleId IN (%s) ", placeholders)+
			"ORDER BY CreatedAt DESC",
		params...,
	)
	if err != nil {
		// PORT-NOTE: Python lets this raise; the Go port logs and returns empty.
		logger.Error("load work item links failed", "error", err)
		return map[string]map[string]any{}
	}
	result := map[string]map[string]any{}
	for _, row := range res.Fetchall() {
		ref := rowString(row["AnomalyRuleId"])
		if refSet[ref] {
			if _, seen := result[ref]; !seen {
				issueUrl := rowString(row["IssueUrl"])
				if issueUrl == "" {
					issueUrl = rowString(row["CanonicalIssueUrl"])
				}
				result[ref] = map[string]any{
					"issue_url":    issueUrl,
					"issue_number": coerceInt(row["IssueNumber"]),
					"issue_state":  rowString(row["IssueState"]),
				}
			}
		}
	}
	return result
}

func viewErrors(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	query := r.URL.Query()
	errorIdSql := errorIdSqlExpr()
	groupedTraceChunkSize := 200
	hydrateKeyChunkSize := 200

	// serverError mirrors an unhandled Python exception → HTTP 500.
	serverError := func(context string, err error) {
		logger.Error(context, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}

	buildErrorStubFromNarrow := func(row Row, resolvedFlag bool) map[string]any {
		ts := rowString(row["Timestamp"])
		serviceName := rowString(row["ServiceName"])
		traceId := rowString(row["TraceId"])
		spanId := rowString(row["SpanId"])
		errType := rowString(row["ErrorType"])
		if errType == "" {
			errType = "Error"
		}
		message := rowString(row["ErrorMessage"])
		rawBody := rowString(row["Body"])
		messageSummary, summaryFromJson := extractStructuredErrorSummary(message, rawBody)
		itemId := rowString(row["ErrorId"])
		if itemId == "" {
			itemId = errorId(ts, serviceName, errType, message, traceId, spanId)
		}
		return map[string]any{
			"id":                   itemId,
			"ts":                   ts,
			"service":              serviceName,
			"err_type":             errType,
			"message":              message,
			"message_summary":      messageSummary,
			"summary_from_json":    summaryFromJson,
			"message_is_json":      false,
			"message_pretty_json":  "",
			"raw_body":             rawBody,
			"raw_body_is_json":     false,
			"raw_body_pretty_json": "",
			"stack":                "",
			"stack_is_json":        false,
			"stack_pretty_json":    "",
			"trace_id":             traceId,
			"span_id":              spanId,
			"url":                  "",
			"error_source":         "",
			"page_title":           "",
			"viewport":             "",
			"artifact_type":        "",
			"artifact_id":          "",
			"artifact_url":         "",
			"replay_id":            "",
			"replay_url":           "",
			"resolved":             resolvedFlag,
		}
	}

	selectedServices := []string{}
	for _, svc := range query["service"] {
		if v := strings.TrimSpace(svc); v != "" {
			selectedServices = append(selectedServices, v)
		}
	}
	service := ""
	if len(selectedServices) > 0 {
		service = selectedServices[0]
	}
	groupBy := strings.ToLower(strings.TrimSpace(query.Get("group_by")))
	groupedMode := strings.TrimSpace(query.Get("grouped")) == "1" ||
		groupBy == "group" || groupBy == "message" || groupBy == "fingerprint" || groupBy == "signature"
	fromTs, toTs, timeError := parseTimeWindowArgs(r)
	resolved := query.Get("resolved")
	if !query.Has("resolved") {
		resolved = "0"
	}
	resolved = strings.TrimSpace(resolved)
	limit := parseLimit(r, 100)
	offset := parseOffset(r)
	var sortBy, sortCol, sortDir string
	if groupedMode {
		sortBy, sortCol, sortDir = parseSort(r,
			map[string]string{
				"count":       "Count",
				"last_seen":   "LastSeen",
				"ServiceName": "RepServiceName",
				// Legacy alias retained for backwards compatibility.
				"Timestamp": "LastSeen",
			},
			"count",
		)
	} else {
		sortBy, sortCol, sortDir = parseSort(r,
			map[string]string{"Timestamp": "Timestamp", "ServiceName": "ServiceName"},
			"Timestamp",
		)
	}
	q := strings.TrimSpace(query.Get("q"))
	includePatterns := []string{}
	excludePatterns := []string{}
	errorMsg := timeError
	if q != "" && errorMsg == "" {
		var regexError string
		includePatterns, excludePatterns, regexError = prepareRe2FilterPatterns(db, q)
		if regexError != "" {
			errorMsg = regexError
		}
	}
	resolvedIds := map[string]bool{}
	if resolved != "0" && resolved != "1" {
		resolvedIds = getResolvedErrorIds(db)
	}
	whereParts := []string{}
	whereParams := []any{}
	if len(selectedServices) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selectedServices)), ",")
		whereParts = append(whereParts, fmt.Sprintf("ServiceName IN (%s)", placeholders))
		for _, v := range selectedServices {
			whereParams = append(whereParams, v)
		}
	}
	appendTimeWindowFilter(&whereParts, &whereParams, "Timestamp", fromTs, toTs)
	if q != "" && errorMsg == "" {
		appendRegexExpressionClauses(&whereParts, &whereParams, "Body", includePatterns, excludePatterns)
	}
	whereSql := whereClause(whereParts)

	var errorItems []map[string]any
	total := 0
	if groupedMode {
		// Best-effort deduplication: probe recent raw events, then aggregate in SQL.
		probeLimit := max(2000, min(10000, limit*100))
		groupedWhereSql := whereSql
		if resolved == "1" {
			resolvedCondition := fmt.Sprintf("%s IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)", errorIdSql)
			if groupedWhereSql != "" {
				groupedWhereSql = fmt.Sprintf("%s AND %s", groupedWhereSql, resolvedCondition)
			} else {
				groupedWhereSql = fmt.Sprintf("WHERE %s", resolvedCondition)
			}
		} else if resolved == "0" {
			resolvedCondition := fmt.Sprintf("%s NOT IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)", errorIdSql)
			if groupedWhereSql != "" {
				groupedWhereSql = fmt.Sprintf("%s AND %s", groupedWhereSql, resolvedCondition)
			} else {
				groupedWhereSql = fmt.Sprintf("WHERE %s", resolvedCondition)
			}
		}

		groupedProbeSql := "SELECT " +
			"Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes, " +
			"substring(replaceRegexpAll(lower(ServiceName), '\\s+', ' '), 1, 220) AS GroupService, " +
			"substring(" +
			"replaceRegexpAll(" +
			"lower(if(LogAttributes['exception.type'] != '', LogAttributes['exception.type'], 'Error')), " +
			"'\\s+', ' '" +
			"), 1, 220" +
			") AS GroupType, " +
			"substring(" +
			"replaceRegexpAll(" +
			"lower(if(LogAttributes['exception.message'] != '', LogAttributes['exception.message'], Body)), " +
			"'\\s+', ' '" +
			"), 1, 220" +
			") AS GroupMessage " +
			fmt.Sprintf("FROM (%s) %s ", errorSourcesSql, groupedWhereSql) +
			"ORDER BY Timestamp DESC LIMIT ?"
		groupedAggregateSql := "SELECT " +
			"GroupService, GroupType, GroupMessage, " +
			"count() AS Count, " +
			"min(Timestamp) AS FirstSeen, " +
			"max(Timestamp) AS LastSeen, " +
			"argMax(Timestamp, Timestamp) AS RepTimestamp, " +
			"argMax(ServiceName, Timestamp) AS RepServiceName, " +
			"argMax(TraceId, Timestamp) AS RepTraceId, " +
			"argMax(SpanId, Timestamp) AS RepSpanId, " +
			"argMax(Body, Timestamp) AS RepBody, " +
			"argMax(LogAttributes, Timestamp) AS RepLogAttributes, " +
			"groupUniqArray(64)(TraceId) AS TraceIds " +
			fmt.Sprintf("FROM (%s) ", groupedProbeSql) +
			"GROUP BY GroupService, GroupType, GroupMessage"

		totalRes, err := db.Execute(
			fmt.Sprintf("SELECT COUNT(*) FROM (%s)", groupedAggregateSql),
			append(append([]any{}, whereParams...), probeLimit)...,
		)
		if err != nil {
			serverError("errors grouped count query failed", err)
			return
		}
		if row := totalRes.Fetchone(); row != nil && len(totalRes.Cols) > 0 {
			total = coerceInt(row[totalRes.Cols[0]])
		}
		sortDirection := "DESC"
		if sortDir == "asc" {
			sortDirection = "ASC"
		}
		pageSql := fmt.Sprintf("%s ORDER BY %s %s LIMIT ? OFFSET ?", groupedAggregateSql, sortCol, sortDirection)
		groupRowsRes, err := db.Execute(
			pageSql,
			append(append([]any{}, whereParams...), probeLimit, limit, offset)...,
		)
		if err != nil {
			serverError("errors grouped page query failed", err)
			return
		}
		groupRows := groupRowsRes.Fetchall()
		errorItems = []map[string]any{}
		visibleGroupTuples := [][3]string{}
		for _, row := range groupRows {
			groupTuple := [3]string{
				rowString(row["GroupService"]),
				rowString(row["GroupType"]),
				rowString(row["GroupMessage"]),
			}
			item := buildErrorItem(Row{
				"Timestamp":     row["RepTimestamp"],
				"ServiceName":   row["RepServiceName"],
				"TraceId":       row["RepTraceId"],
				"SpanId":        row["RepSpanId"],
				"Body":          row["RepBody"],
				"LogAttributes": row["RepLogAttributes"],
			})
			if resolved == "1" {
				item["resolved"] = true
			} else if resolved == "0" {
				item["resolved"] = false
			} else {
				item["resolved"] = resolvedIds[rowString(item["id"])]
			}
			item["count"] = coerceInt(row["Count"])
			firstSeen := rowString(row["FirstSeen"])
			if firstSeen == "" {
				firstSeen = rowString(item["ts"])
			}
			item["first_seen"] = firstSeen
			lastSeen := rowString(row["LastSeen"])
			if lastSeen == "" {
				lastSeen = rowString(item["ts"])
			}
			item["last_seen"] = lastSeen
			item["group_tuple"] = groupTuple
			visibleGroupTuples = append(visibleGroupTuples, groupTuple)
			errorItems = append(errorItems, item)
		}

		if len(errorItems) > 0 {
			uniqueGroupTuples := [][3]string{}
			seenGroupTuples := map[[3]string]bool{}
			for _, groupTuple := range visibleGroupTuples {
				if seenGroupTuples[groupTuple] {
					continue
				}
				seenGroupTuples[groupTuple] = true
				uniqueGroupTuples = append(uniqueGroupTuples, groupTuple)
			}

			traceGroupParams := append(append([]any{}, whereParams...), probeLimit)
			traceIdsByGroup := map[[3]string][]string{}
			for chunkStart := 0; chunkStart < len(uniqueGroupTuples); chunkStart += groupedTraceChunkSize {
				chunkEnd := min(chunkStart+groupedTraceChunkSize, len(uniqueGroupTuples))
				groupChunk := uniqueGroupTuples[chunkStart:chunkEnd]
				chunkParams := append([]any{}, traceGroupParams...)
				placeholderParts := make([]string, len(groupChunk))
				for i := range placeholderParts {
					placeholderParts[i] = "(?, ?, ?)"
				}
				traceGroupPlaceholders := strings.Join(placeholderParts, ", ")
				for _, groupTuple := range groupChunk {
					chunkParams = append(chunkParams, groupTuple[0], groupTuple[1], groupTuple[2])
				}

				groupedTraceSql := "SELECT GroupService, GroupType, GroupMessage, " +
					"arrayStringConcat(groupUniqArray(64)(TraceId), ',') AS TraceIdsCsv " +
					fmt.Sprintf("FROM (%s) ", groupedProbeSql) +
					fmt.Sprintf("WHERE (GroupService, GroupType, GroupMessage) IN (%s) ", traceGroupPlaceholders) +
					"GROUP BY GroupService, GroupType, GroupMessage"
				traceRes, err := db.Execute(groupedTraceSql, chunkParams...)
				if err != nil {
					serverError("errors grouped trace ids query failed", err)
					return
				}
				for _, row := range traceRes.Fetchall() {
					groupTuple := [3]string{
						rowString(row["GroupService"]),
						rowString(row["GroupType"]),
						rowString(row["GroupMessage"]),
					}
					traceIds := []string{}
					for _, value := range strings.Split(rowString(row["TraceIdsCsv"]), ",") {
						if v := strings.TrimSpace(pyHexStr(value)); v != "" {
							traceIds = append(traceIds, v)
						}
					}
					traceIdsByGroup[groupTuple] = traceIds
				}
			}

			for _, item := range errorItems {
				groupTuple := [3]string{"", "", ""}
				if gt, ok := item["group_tuple"].([3]string); ok {
					groupTuple = gt
				}
				delete(item, "group_tuple")
				traceValues := append([]string{}, traceIdsByGroup[groupTuple]...)
				primaryTrace := strings.TrimSpace(rowString(item["trace_id"]))
				if primaryTrace != "" && !slices.Contains(traceValues, primaryTrace) {
					traceValues = append([]string{primaryTrace}, traceValues...)
				}
				if len(traceValues) > 0 {
					item["trace_ids"] = traceValues
					item["trace_ids_csv"] = strings.Join(traceValues, ",")
				}
			}
		}
	} else {
		orderDir := "DESC"
		if sortDir == "asc" {
			orderDir = "ASC"
		}
		orderClause := fmt.Sprintf("ORDER BY %s %s", sortCol, orderDir)
		sourceSql := "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes " +
			fmt.Sprintf("FROM (%s) %s ", errorSourcesSql, whereSql) +
			fmt.Sprintf("%s LIMIT ? OFFSET ?", orderClause)
		useResolvedSqlPath := resolved == "0" || resolved == "1"
		if useResolvedSqlPath {
			errorIdExpr := errorIdSql
			pocWhereSql := whereSql
			pocWhereParams := append([]any{}, whereParams...)
			if resolved == "1" {
				resolvedCondition := fmt.Sprintf("%s IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)", errorIdExpr)
				if pocWhereSql != "" {
					pocWhereSql = fmt.Sprintf("%s AND %s", pocWhereSql, resolvedCondition)
				} else {
					pocWhereSql = fmt.Sprintf("WHERE %s", resolvedCondition)
				}
			} else if resolved == "0" {
				resolvedCondition := fmt.Sprintf("%s NOT IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)", errorIdExpr)
				if pocWhereSql != "" {
					pocWhereSql = fmt.Sprintf("%s AND %s", pocWhereSql, resolvedCondition)
				} else {
					pocWhereSql = fmt.Sprintf("WHERE %s", resolvedCondition)
				}
			}
			narrowSourceSql := "SELECT " +
				"Timestamp, ServiceName, TraceId, SpanId, " +
				fmt.Sprintf("%s AS ErrorId ", errorIdExpr) +
				fmt.Sprintf("FROM (%s) %s ", errorSourcesSql, pocWhereSql) +
				fmt.Sprintf("%s LIMIT ? OFFSET ?", orderClause)

			countSql := fmt.Sprintf("SELECT COUNT(*) FROM (%s) %s", errorSourcesSql, pocWhereSql)
			totalRes, err := db.Execute(countSql, pocWhereParams...)
			if err != nil {
				serverError("errors count query failed", err)
				return
			}
			if row := totalRes.Fetchone(); row != nil && len(totalRes.Cols) > 0 {
				total = coerceInt(row[totalRes.Cols[0]])
			}
			pageRes, err := db.Execute(narrowSourceSql, append(append([]any{}, pocWhereParams...), limit, offset)...)
			if err != nil {
				serverError("errors narrow page query failed", err)
				return
			}
			pageRows := pageRes.Fetchall()
			detailsById := map[string]map[string]any{}
			if len(pageRows) > 0 {
				detailKeyTuples := [][4]any{}
				seenDetailKeys := map[string]bool{}
				for _, row := range pageRows {
					detailKey := [4]any{
						row["Timestamp"],
						row["ServiceName"],
						row["TraceId"],
						row["SpanId"],
					}
					// PORT-NOTE: Python dedupes on the raw tuple; the Go port
					// uses a printable composite key for the seen-set.
					seenKey := fmt.Sprintf("%v\x00%v\x00%v\x00%v", detailKey[0], detailKey[1], detailKey[2], detailKey[3])
					if seenDetailKeys[seenKey] {
						continue
					}
					seenDetailKeys[seenKey] = true
					detailKeyTuples = append(detailKeyTuples, detailKey)
				}
				for chunkStart := 0; chunkStart < len(detailKeyTuples); chunkStart += hydrateKeyChunkSize {
					chunkEnd := min(chunkStart+hydrateKeyChunkSize, len(detailKeyTuples))
					detailChunk := detailKeyTuples[chunkStart:chunkEnd]
					detailParams := []any{}
					placeholderParts := make([]string, len(detailChunk))
					for i := range placeholderParts {
						placeholderParts[i] = "(?, ?, ?, ?)"
					}
					tuplePlaceholders := strings.Join(placeholderParts, ", ")
					for _, key := range detailChunk {
						detailParams = append(detailParams, key[0], key[1], key[2], key[3])
					}
					detailSql := "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes " +
						fmt.Sprintf("FROM (%s) ", errorSourcesSql) +
						fmt.Sprintf("WHERE (Timestamp, ServiceName, TraceId, SpanId) IN (%s)", tuplePlaceholders)
					detailRes, err := db.Execute(detailSql, detailParams...)
					if err != nil {
						serverError("errors detail hydrate query failed", err)
						return
					}
					for _, drow := range detailRes.Fetchall() {
						detailItem := buildErrorItem(drow)
						detailsById[rowString(detailItem["id"])] = detailItem
					}
				}
			}
			errorItems = []map[string]any{}
			for _, row := range pageRows {
				rowId := rowString(row["ErrorId"])
				var resolvedFlag bool
				if resolved == "1" {
					resolvedFlag = true
				} else if resolved == "0" {
					resolvedFlag = false
				} else {
					resolvedFlag = resolvedIds[rowId]
				}
				item := buildErrorStubFromNarrow(row, resolvedFlag)
				if detailItem, ok := detailsById[rowString(item["id"])]; ok {
					detailItem["resolved"] = resolvedFlag
					item = detailItem
				}
				errorItems = append(errorItems, item)
			}
		} else {
			totalRes, err := db.Execute(
				fmt.Sprintf("SELECT COUNT(*) FROM (%s) %s", errorSourcesSql, whereSql),
				whereParams...,
			)
			if err != nil {
				serverError("errors count query failed", err)
				return
			}
			if row := totalRes.Fetchone(); row != nil && len(totalRes.Cols) > 0 {
				total = coerceInt(row[totalRes.Cols[0]])
			}
			rowsRes, err := db.Execute(sourceSql, append(append([]any{}, whereParams...), limit, offset)...)
			if err != nil {
				serverError("errors page query failed", err)
				return
			}
			errorItems = []map[string]any{}
			for _, row := range rowsRes.Fetchall() {
				item := buildErrorItem(row)
				item["resolved"] = resolvedIds[rowString(item["id"])]
				errorItems = append(errorItems, item)
			}
		}
	}

	nowSec := float64(time.Now().UnixNano()) / 1e9
	services := []string{}
	errorsCacheLock.Lock()
	if expiresAt, _ := coerceFloat(errorsServicesCache["expires_at"]); expiresAt > nowSec {
		switch cached := errorsServicesCache["services"].(type) {
		case []string:
			services = append([]string{}, cached...)
		case []any:
			for _, s := range cached {
				services = append(services, rowString(s))
			}
		}
	}
	errorsCacheLock.Unlock()

	if len(services) == 0 {
		svcRes, err := db.Execute(
			"SELECT DISTINCT ServiceName FROM (" +
				errorSourcesSql +
				") WHERE ServiceName!='' ORDER BY ServiceName",
		)
		if err != nil {
			serverError("errors services query failed", err)
			return
		}
		for _, row := range svcRes.Fetchall() {
			services = append(services, rowString(row["ServiceName"]))
		}
		errorsCacheLock.Lock()
		errorsServicesCache["services"] = append([]string{}, services...)
		errorsServicesCache["expires_at"] = nowSec + float64(max(1, errorsServicesCacheTtlSec))
		errorsCacheLock.Unlock()
	}

	refIds := make([]string, 0, len(errorItems))
	for _, e := range errorItems {
		refIds = append(refIds, rowString(e["id"]))
	}
	workItemLinks := loadWorkItemLinksForRefIds(db, refIds)

	renderTemplate(w, r, "errors.html", map[string]any{
		"errors":            errorItems,
		"total":             total,
		"limit":             limit,
		"offset":            offset,
		"service":           service,
		"selected_services": selectedServices,
		"from_ts":           fromTs,
		"to_ts":             toTs,
		"error_msg":         errorMsg,
		"q":                 q,
		"resolved":          resolved,
		"services":          services,
		"sort_by":           sortBy,
		"sort_dir":          sortDir,
		"grouped_mode":      groupedMode,
		"work_item_links":   workItemLinks,
	})
}

func resolveError(w http.ResponseWriter, r *http.Request) {
	errorIdValue := r.PathValue("error_id")
	op := func(db *ChDbConnection) error {
		_, err := db.Execute("INSERT INTO sobs_error_resolutions(ErrorId) VALUES(?)", errorIdValue)
		return err
	}
	if err := queueWrite(op, true); err != nil {
		logger.Error("resolve error write failed", "error", err)
		jsonError(w, "resolve error write failed", 500)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Web UI – Traces (GET /traces) and span-tree / timeline helpers
// (port of app.py 14623-14842, 15309-15678)
// ---------------------------------------------------------------------------

const (
	traceDetailHardCap           = 5000
	traceDetailDefaultLimit      = 200
	traceDetailMaxLimit          = 1000
	traceDetailCollapseThreshold = 300
)

func roundTo3(x float64) float64 { return math.Round(x*1000) / 1000 }

// buildSpanTree returns spans ordered depth-first with depth/has_children fields.
func buildSpanTree(spans []map[string]any) []map[string]any {
	byId := map[string]map[string]any{}
	for _, s := range spans {
		byId[rowString(s["span_id"])] = s
	}
	children := map[string][]map[string]any{}
	roots := []map[string]any{}
	for _, span := range spans {
		pid := rowString(span["parent_span_id"])
		if pid != "" {
			if _, ok := byId[pid]; ok {
				children[pid] = append(children[pid], span)
				continue
			}
		}
		roots = append(roots, span)
	}
	tsLess := func(a, b map[string]any) bool { return rowString(a["ts"]) < rowString(b["ts"]) }
	for _, clist := range children {
		sort.SliceStable(clist, func(i, j int) bool { return tsLess(clist[i], clist[j]) })
	}
	sort.SliceStable(roots, func(i, j int) bool { return tsLess(roots[i], roots[j]) })
	result := []map[string]any{}
	type frame struct {
		span  map[string]any
		depth int
	}
	stack := []frame{}
	for i := len(roots) - 1; i >= 0; i-- {
		stack = append(stack, frame{roots[i], 0})
	}
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		sid := rowString(f.span["span_id"])
		_, hasChildren := children[sid]
		out := map[string]any{}
		for k, v := range f.span {
			out[k] = v
		}
		out["depth"] = f.depth
		out["has_children"] = hasChildren
		result = append(result, out)
		kids := children[sid]
		for i := len(kids) - 1; i >= 0; i-- {
			stack = append(stack, frame{kids[i], f.depth + 1})
		}
	}
	return result
}

// mergeSpanIntervals merges span start/end intervals sorted by start time.
func mergeSpanIntervals(spans []map[string]any) [][2]float64 {
	if len(spans) == 0 {
		return [][2]float64{}
	}
	intervals := make([][2]float64, 0, len(spans))
	for _, span := range spans {
		startMs, _ := coerceFloat(span["start_ms"])
		dur, _ := coerceFloat(span["duration_ms"])
		if dur < 0 {
			dur = 0
		}
		intervals = append(intervals, [2]float64{startMs, startMs + dur})
	}
	sort.SliceStable(intervals, func(i, j int) bool { return intervals[i][0] < intervals[j][0] })
	merged := [][2]float64{}
	for _, iv := range intervals {
		if len(merged) == 0 || iv[0] > merged[len(merged)-1][1] {
			merged = append(merged, iv)
		} else if iv[1] > merged[len(merged)-1][1] {
			merged[len(merged)-1][1] = iv[1]
		}
	}
	return merged
}

// computeActiveTimelineMs returns merged active time across span intervals.
func computeActiveTimelineMs(spans []map[string]any) float64 {
	total := 0.0
	for _, iv := range mergeSpanIntervals(spans) {
		if d := iv[1] - iv[0]; d > 0 {
			total += d
		}
	}
	return total
}

// sliceSpanTreeWithAncestors returns a paged span-tree slice plus ancestors.
func sliceSpanTreeWithAncestors(fullSpanTree []map[string]any, offset, limit int) ([]map[string]any, int, int) {
	if len(fullSpanTree) == 0 {
		return []map[string]any{}, 0, 0
	}
	total := len(fullSpanTree)
	pageStart := offset
	if pageStart < 0 {
		pageStart = 0
	}
	if pageStart > total {
		pageStart = total
	}
	step := limit
	if step < 1 {
		step = 1
	}
	pageEnd := pageStart + step
	if pageEnd > total {
		pageEnd = total
	}
	pageRows := fullSpanTree[pageStart:pageEnd]
	if len(pageRows) == 0 {
		return []map[string]any{}, pageEnd, 0
	}
	byId := map[string]map[string]any{}
	for _, row := range fullSpanTree {
		byId[rowString(row["span_id"])] = row
	}
	included := map[string]bool{}
	for _, row := range pageRows {
		included[rowString(row["span_id"])] = true
	}
	for _, row := range pageRows {
		parentId := rowString(row["parent_span_id"])
		for parentId != "" {
			parent, ok := byId[parentId]
			if !ok || included[parentId] {
				break
			}
			included[parentId] = true
			parentId = rowString(parent["parent_span_id"])
		}
	}
	rows := []map[string]any{}
	for _, row := range fullSpanTree {
		if included[rowString(row["span_id"])] {
			rows = append(rows, row)
		}
	}
	contextRows := len(rows) - len(pageRows)
	if contextRows < 0 {
		contextRows = 0
	}
	return rows, pageEnd, contextRows
}

// buildTraceTimelineSegments returns active/gap segments over the trace window.
func buildTraceTimelineSegments(spans []map[string]any, activityTsMs []float64) []map[string]any {
	if len(spans) == 0 {
		return []map[string]any{}
	}
	traceStartMs := math.Inf(1)
	traceEndMs := math.Inf(-1)
	for _, s := range spans {
		st, _ := coerceFloat(s["start_ms"])
		dur, _ := coerceFloat(s["duration_ms"])
		if dur < 0 {
			dur = 0
		}
		if st < traceStartMs {
			traceStartMs = st
		}
		if st+dur > traceEndMs {
			traceEndMs = st + dur
		}
	}
	traceTotalMs := traceEndMs - traceStartMs
	if traceTotalMs < 1.0 {
		traceTotalMs = 1.0
	}
	merged := mergeSpanIntervals(spans)
	activitySorted := append([]float64{}, activityTsMs...)
	sort.Float64s(activitySorted)
	toPct := func(v float64) float64 { return (v - traceStartMs) / traceTotalMs * 100.0 }
	hasGapActivity := func(startMs, endMs float64) bool {
		for _, ts := range activitySorted {
			if ts < startMs {
				continue
			}
			if ts > endMs {
				break
			}
			return true
		}
		return false
	}
	segments := []map[string]any{}
	cursor := traceStartMs
	for _, iv := range merged {
		startMs, endMs := iv[0], iv[1]
		if startMs > cursor {
			gapWidthPct := toPct(startMs) - toPct(cursor)
			if gapWidthPct > 0 {
				segments = append(segments, map[string]any{
					"kind":      "gap",
					"start_pct": roundTo3(toPct(cursor)),
					"width_pct": roundTo3(gapWidthPct),
					"potential": hasGapActivity(cursor, startMs),
				})
			}
		}
		activeWidthPct := toPct(endMs) - toPct(startMs)
		if activeWidthPct > 0 {
			segments = append(segments, map[string]any{
				"kind":      "active",
				"start_pct": roundTo3(toPct(startMs)),
				"width_pct": roundTo3(activeWidthPct),
				"potential": false,
			})
		}
		if endMs > cursor {
			cursor = endMs
		}
	}
	if cursor < traceEndMs {
		gapWidthPct := toPct(traceEndMs) - toPct(cursor)
		if gapWidthPct > 0 {
			segments = append(segments, map[string]any{
				"kind":      "gap",
				"start_pct": roundTo3(toPct(cursor)),
				"width_pct": roundTo3(gapWidthPct),
				"potential": hasGapActivity(cursor, traceEndMs),
			})
		}
	}
	return segments
}

// buildTraceWindowOverlaySegments returns window overlay segments aligned to the
// trace timeline axis.
func buildTraceWindowOverlaySegments(spans []map[string]any, windows []map[string]any) []map[string]any {
	if len(spans) == 0 || len(windows) == 0 {
		return []map[string]any{}
	}
	traceStartMs := math.Inf(1)
	traceEndMs := math.Inf(-1)
	for _, s := range spans {
		st, _ := coerceFloat(s["start_ms"])
		dur, _ := coerceFloat(s["duration_ms"])
		if dur < 0 {
			dur = 0
		}
		if st < traceStartMs {
			traceStartMs = st
		}
		if st+dur > traceEndMs {
			traceEndMs = st + dur
		}
	}
	traceTotalMs := traceEndMs - traceStartMs
	if traceTotalMs < 1.0 {
		traceTotalMs = 1.0
	}
	toPct := func(v float64) float64 { return (v - traceStartMs) / traceTotalMs * 100.0 }
	segments := []map[string]any{}
	for _, wnd := range windows {
		ws := tsStrToEpochMs(rowString(wnd["window_start"]))
		we := tsStrToEpochMs(rowString(wnd["window_end"]))
		if we <= 0 || ws <= 0 {
			continue
		}
		startMs := math.Max(ws, traceStartMs)
		endMs := math.Min(we, traceEndMs)
		if endMs <= startMs {
			continue
		}
		startPct := toPct(startMs)
		widthPct := toPct(endMs) - startPct
		if widthPct <= 0 {
			continue
		}
		copiedCount := coerceInt(wnd["copied_count"])
		expectedCount := coerceInt(wnd["expected_count"])
		signalType := rowString(wnd["signal_type"])
		signalRef := rowString(wnd["signal_ref"])
		title := signalType
		if title == "" {
			title = "window"
		}
		if signalRef != "" {
			title += fmt.Sprintf(" (%s)", signalRef)
		}
		title += fmt.Sprintf(" [%d/%d]", copiedCount, expectedCount)
		segments = append(segments, map[string]any{
			"start_pct":     roundTo3(startPct),
			"width_pct":     roundTo3(widthPct),
			"copy_complete": pyTruthy(wnd["copy_complete"]),
			"title":         title,
		})
	}
	sort.SliceStable(segments, func(i, j int) bool {
		a, _ := coerceFloat(segments[i]["start_pct"])
		b, _ := coerceFloat(segments[j]["start_pct"])
		return a < b
	})
	return segments
}

func viewTraces(w http.ResponseWriter, r *http.Request) {
	const chTsLayout = "2006-01-02 15:04:05.000000"
	db := getDb()
	errorIdSql := errorIdSqlExpr()
	query := r.URL.Query()
	selectedServices := []string{}
	for _, svc := range query["service"] {
		if t := strings.TrimSpace(svc); t != "" {
			selectedServices = append(selectedServices, t)
		}
	}
	service := ""
	if len(selectedServices) > 0 {
		service = selectedServices[0]
	}
	traceId := strings.TrimSpace(query.Get("trace_id"))
	fromTs, toTs, timeError := parseTimeWindowArgs(r)
	limit := parseLimit(r, 100)
	offset := parseOffset(r)
	traceSpanLimit := coercePositiveInt(query.Get("trace_span_limit"), traceDetailDefaultLimit, 1, traceDetailMaxLimit)
	traceSpanOffset := coercePositiveInt(query.Get("trace_span_offset"), 0, 0, traceDetailHardCap)
	sortBy, sortCol, sortDir := parseSort(r, map[string]string{
		"Timestamp":   "Timestamp",
		"SpanName":    "SpanName",
		"ServiceName": "ServiceName",
		"Duration":    "Duration",
	}, "Timestamp")
	orderDir := "DESC"
	if sortDir == "asc" {
		orderDir = "ASC"
	}
	orderClause := fmt.Sprintf("ORDER BY %s %s", sortCol, orderDir)

	conditions := []string{}
	params := []any{}
	q := strings.TrimSpace(query.Get("q"))
	qError := ""
	includePatterns := []string{}
	excludePatterns := []string{}
	if q != "" {
		ip, ep, regexError := prepareRe2FilterPatterns(db, q)
		includePatterns, excludePatterns = ip, ep
		if regexError != "" {
			qError = regexError
		}
	}
	if len(selectedServices) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selectedServices)), ",")
		conditions = append(conditions, fmt.Sprintf("ServiceName IN (%s)", placeholders))
		for _, v := range selectedServices {
			params = append(params, v)
		}
	}
	if traceId != "" {
		conditions = append(conditions, "TraceId=?")
		params = append(params, traceId)
	}
	appendTimeWindowFilter(&conditions, &params, "Timestamp", fromTs, toTs)
	if q != "" && qError == "" {
		appendRegexExpressionClauses(&conditions, &params, "SpanName", includePatterns, excludePatterns)
	}
	where := whereClause(conditions)

	total := 0
	rows := []Row{}
	if traceId != "" && timeError == "" {
		total = 0
		rows = []Row{}
	} else {
		if where == "" {
			total = activePartRows(db, "otel_traces")
		} else {
			if res, err := db.Execute(fmt.Sprintf("SELECT COUNT(*) FROM otel_traces %s", where), params...); err != nil {
				logger.Error("traces count query failed", "error", err)
			} else {
				total = firstScalarInt(res)
			}
		}
		if rowsRes, err := db.Execute(
			"SELECT Timestamp, TraceId, SpanId, ParentSpanId, "+
				"SpanName, ServiceName, Duration, StatusCode, SpanAttributes "+
				fmt.Sprintf("FROM otel_traces %s %s LIMIT ? OFFSET ?", where, orderClause),
			append(append([]any{}, params...), limit, offset)...,
		); err != nil {
			logger.Error("traces rows query failed", "error", err)
		} else {
			rows = rowsRes.Fetchall()
		}
	}

	attrGet := func(a map[string]any, primary, fallback string) any {
		if v, ok := a[primary]; ok {
			return v
		}
		if v, ok := a[fallback]; ok {
			return v
		}
		return ""
	}

	spans := []map[string]any{}
	for _, rrow := range rows {
		attrs := mapToDict(rrow["SpanAttributes"])
		durF, _ := coerceFloat(rrow["Duration"])
		spans = append(spans, map[string]any{
			"ts":             rowString(rrow["Timestamp"]),
			"trace_id":       rrow["TraceId"],
			"span_id":        rrow["SpanId"],
			"parent_span_id": rrow["ParentSpanId"],
			"name":           rrow["SpanName"],
			"service":        rrow["ServiceName"],
			"duration_ms":    roundTo2(durF / 1_000_000),
			"status":         rrow["StatusCode"],
			"http_method":    attrGet(attrs, "http.method", "http.request.method"),
			"http_url":       attrGet(attrs, "http.url", "url.full"),
			"http_status":    attrGet(attrs, "http.status_code", "http.response.status_code"),
		})
	}

	services := []string{}
	if svcRes, err := db.Execute(
		"SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName!='' ORDER BY ServiceName",
	); err != nil {
		logger.Error("traces services query failed", "error", err)
	} else {
		for _, row := range svcRes.Fetchall() {
			services = append(services, rowString(row["ServiceName"]))
		}
	}

	var traceDetail map[string]any
	if traceId != "" && timeError == "" {
		traceTotalSpans := 0
		if res, err := db.Execute("SELECT COUNT(*) FROM otel_traces WHERE TraceId=?", traceId); err == nil {
			traceTotalSpans = firstScalarInt(res)
		}
		detailFetchLimit := traceTotalSpans
		if detailFetchLimit > traceDetailHardCap {
			detailFetchLimit = traceDetailHardCap
		}
		var detailRows []Row
		if dres, err := db.Execute(
			"SELECT Timestamp, TraceId, SpanId, ParentSpanId, SpanName, ServiceName, "+
				"Duration, StatusCode, SpanAttributes "+
				"FROM otel_traces WHERE TraceId=? ORDER BY Timestamp ASC, SpanId ASC LIMIT ?",
			traceId, detailFetchLimit,
		); err == nil {
			detailRows = dres.Fetchall()
		}
		if len(detailRows) > 0 {
			allTraceSpans := []map[string]any{}
			for _, rrow := range detailRows {
				attrs := mapToDict(rrow["SpanAttributes"])
				tsStr := rowString(rrow["Timestamp"])
				durF, _ := coerceFloat(rrow["Duration"])
				allTraceSpans = append(allTraceSpans, map[string]any{
					"ts":             tsStr,
					"trace_id":       rowString(rrow["TraceId"]),
					"span_id":        rowString(rrow["SpanId"]),
					"parent_span_id": rowString(rrow["ParentSpanId"]),
					"name":           rowString(rrow["SpanName"]),
					"service":        rowString(rrow["ServiceName"]),
					"start_ms":       tsStrToEpochMs(tsStr),
					"duration_ms":    roundTo2(durF / 1_000_000),
					"status":         rowString(rrow["StatusCode"]),
					"http_method":    rowString(attrGet(attrs, "http.method", "http.request.method")),
					"http_url":       rowString(attrGet(attrs, "http.url", "url.full")),
					"http_status":    rowString(attrGet(attrs, "http.status_code", "http.response.status_code")),
					"namespace":      rowString(attrGet(attrs, "k8s.namespace.name", "namespace")),
					"pod":            rowString(attrGet(attrs, "k8s.pod.name", "pod")),
					"node":           rowString(attrGet(attrs, "k8s.node.name", "node")),
					"deployment":     rowString(attrGet(attrs, "k8s.deployment.name", "deployment")),
				})
			}

			traceStartMs := math.Inf(1)
			traceEndMs := math.Inf(-1)
			for _, s := range allTraceSpans {
				st := s["start_ms"].(float64)
				du := s["duration_ms"].(float64)
				if st < traceStartMs {
					traceStartMs = st
				}
				if st+du > traceEndMs {
					traceEndMs = st + du
				}
			}
			traceTotalMs := traceEndMs - traceStartMs
			if traceTotalMs < 1.0 {
				traceTotalMs = 1.0
			}
			traceActiveMs := computeActiveTimelineMs(allTraceSpans)
			traceCoveragePct := math.Min(100.0, math.Max(0.0, traceActiveMs/traceTotalMs*100.0))
			traceSpanSumMs := 0.0
			for _, s := range allTraceSpans {
				if d := s["duration_ms"].(float64); d > 0 {
					traceSpanSumMs += d
				}
			}
			for _, span := range allTraceSpans {
				st := span["start_ms"].(float64)
				du := span["duration_ms"].(float64)
				span["offset_pct"] = roundTo2((st - traceStartMs) / traceTotalMs * 100)
				span["width_pct"] = roundTo2(math.Max(0.5, du/traceTotalMs*100))
			}

			const traceErrorLimit = 50
			traceErrors := []map[string]any{}
			errorsTruncated := false
			traceActivityTsMs := []float64{}
			if eres, err := db.Execute(
				"SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes, ErrorId, "+
					"(ErrorId IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)) AS IsResolved "+
					"FROM ("+
					"SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes, "+
					fmt.Sprintf("%s AS ErrorId ", errorIdSql)+
					fmt.Sprintf("FROM (%s) WHERE TraceId=? LIMIT ?", errorSourcesSql)+
					")",
				traceId, traceErrorLimit+1,
			); err == nil {
				errRows := eres.Fetchall()
				if len(errRows) > traceErrorLimit {
					errorsTruncated = true
					errRows = errRows[:traceErrorLimit]
				}
				for _, row := range errRows {
					item := buildErrorItem(row)
					if eid := rowString(row["ErrorId"]); eid != "" {
						item["id"] = eid
					}
					item["resolved"] = pyTruthy(row["IsResolved"])
					traceErrors = append(traceErrors, item)
					if tsRaw := rowString(item["ts"]); tsRaw != "" {
						traceActivityTsMs = append(traceActivityTsMs, tsStrToEpochMs(tsRaw))
					}
				}
			} else {
				logger.Warn("view_traces: failed to fetch errors for trace", "trace", traceId, "error", err)
			}

			errorSpanIdSet := map[string]bool{}
			for _, e := range traceErrors {
				if sid := rowString(e["span_id"]); sid != "" {
					errorSpanIdSet[sid] = true
				}
			}
			errorSpanIds := sortedStringSet(errorSpanIdSet)

			logCounts := map[string]int{}
			if lres, err := db.Execute(
				"SELECT SpanId, count() AS cnt FROM otel_logs WHERE TraceId=? AND SpanId!='' GROUP BY SpanId",
				traceId,
			); err == nil {
				for _, rr := range lres.Fetchall() {
					logCounts[rowString(rr["SpanId"])] = coerceInt(rr["cnt"])
				}
				if ltres, err2 := db.Execute("SELECT Timestamp FROM otel_logs WHERE TraceId=? LIMIT 2000", traceId); err2 == nil {
					for _, rr := range ltres.Fetchall() {
						traceActivityTsMs = append(traceActivityTsMs, tsStrToEpochMs(rowString(rr["Timestamp"])))
					}
				}
			} else {
				logger.Warn("view_traces: failed to fetch log counts for trace", "trace", traceId, "error", err)
			}

			timelineSegments := buildTraceTimelineSegments(allTraceSpans, traceActivityTsMs)
			hasPotentialGap := false
			for _, seg := range timelineSegments {
				if rowString(seg["kind"]) == "gap" && pyTruthy(seg["potential"]) {
					hasPotentialGap = true
					break
				}
			}

			var traceAnomalyState any
			primarySvc := ""
			if len(allTraceSpans) > 0 {
				primarySvc = rowString(allTraceSpans[0]["service"])
			}
			if primarySvc != "" {
				if ares, err := db.Execute(
					"SELECT anomaly_state FROM v_derived_signals_anomaly "+
						"WHERE ServiceName=? AND SignalSource='traces' "+
						"AND time >= now() - INTERVAL 48 HOUR "+
						"ORDER BY time DESC LIMIT 1", primarySvc,
				); err == nil {
					if arow := ares.Fetchone(); arow != nil {
						traceAnomalyState = rowString(arow["anomaly_state"])
					}
				} else {
					logger.Warn("view_traces: failed to fetch anomaly state for trace", "trace", traceId, "error", err)
				}
			}

			collectField := func(field string) []string {
				set := map[string]bool{}
				for _, s := range allTraceSpans {
					if v := strings.TrimSpace(rowString(s[field])); v != "" {
						set[v] = true
					}
				}
				return sortedStringSet(set)
			}
			traceServices := collectField("service")
			traceStartTs := time.UnixMilli(int64(traceStartMs)).UTC().Format(chTsLayout)
			traceEndTs := time.UnixMilli(int64(traceEndMs)).UTC().Format(chTsLayout)
			traceWindows := listTraceOverlappingRawWindows(db, traceServices, traceStartTs, traceEndTs, 25)

			traceNamespaces := collectField("namespace")
			tracePods := collectField("pod")
			traceNodes := collectField("node")
			traceDeployments := collectField("deployment")
			const metricPadMs = 5 * 60 * 1000
			metricCtxStartTs := time.UnixMilli(int64(traceStartMs) - metricPadMs).UTC().Format(chTsLayout)
			metricCtxEndTs := time.UnixMilli(int64(traceEndMs) + metricPadMs).UTC().Format(chTsLayout)
			windowIds := []string{}
			for _, wnd := range traceWindows {
				if id := rowString(wnd["id"]); id != "" {
					windowIds = append(windowIds, id)
				}
			}
			traceMetricsContext := fetchTraceMetricContext(
				db, traceServices, metricCtxStartTs, metricCtxEndTs, windowIds, 12,
				traceNamespaces, tracePods, traceNodes, traceDeployments,
			)

			traceWindowSegments := buildTraceWindowOverlaySegments(allTraceSpans, traceWindows)

			fullSpanTree := buildSpanTree(allTraceSpans)
			cappedTotalSpans := len(fullSpanTree)
			if traceSpanOffset >= cappedTotalSpans && cappedTotalSpans > 0 {
				traceSpanOffset = ((cappedTotalSpans - 1) / traceSpanLimit) * traceSpanLimit
				if traceSpanOffset < 0 {
					traceSpanOffset = 0
				}
			}
			tracePageSpans, tracePageEnd, traceContextRows := sliceSpanTreeWithAncestors(fullSpanTree, traceSpanOffset, traceSpanLimit)
			detailPrevOffset := traceSpanOffset - traceSpanLimit
			if detailPrevOffset < 0 {
				detailPrevOffset = 0
			}
			detailNextOffset := traceSpanOffset + traceSpanLimit
			detailHardCapped := traceTotalSpans > traceDetailHardCap
			defaultCollapsed := cappedTotalSpans > traceDetailCollapseThreshold

			total = traceTotalSpans

			traceDetail = map[string]any{
				"span_tree":           tracePageSpans,
				"trace_start_ts":      rowString(allTraceSpans[0]["ts"]),
				"trace_end_ts":        rowString(allTraceSpans[len(allTraceSpans)-1]["ts"]),
				"trace_start_ms":      math.Round(traceStartMs),
				"trace_end_ms":        math.Round(traceEndMs),
				"errors":              traceErrors,
				"errors_truncated":    errorsTruncated,
				"error_span_ids":      errorSpanIds,
				"log_counts":          logCounts,
				"anomaly_state":       traceAnomalyState,
				"total_ms":            roundTo2(traceTotalMs),
				"active_ms":           roundTo2(traceActiveMs),
				"coverage_pct":        roundTo2(traceCoveragePct),
				"span_sum_ms":         roundTo2(traceSpanSumMs),
				"timeline_segments":   timelineSegments,
				"has_potential_gap":   hasPotentialGap,
				"raw_windows":         traceWindows,
				"raw_window_segments": traceWindowSegments,
				"metrics_context":     traceMetricsContext,
				"total_spans":         traceTotalSpans,
				"capped_total_spans":  cappedTotalSpans,
				"hard_cap":            traceDetailHardCap,
				"hard_capped":         detailHardCapped,
				"default_collapsed":   defaultCollapsed,
				"page_limit":          traceSpanLimit,
				"page_offset":         traceSpanOffset,
				"page_end":            tracePageEnd,
				"context_rows":        traceContextRows,
				"prev_offset":         detailPrevOffset,
				"next_offset":         detailNextOffset,
				"has_prev_page":       traceSpanOffset > 0,
				"has_next_page":       detailNextOffset < cappedTotalSpans,
			}
		}
	}

	traceWorkItemLinks := map[string]map[string]any{}
	if traceDetail != nil {
		traceErrorsLocal, _ := traceDetail["errors"].([]map[string]any)
		refSet := map[string]bool{}
		for _, e := range traceErrorsLocal {
			if id := rowString(e["id"]); id != "" {
				refSet[id] = true
			}
			if tid := rowString(e["trace_id"]); tid != "" {
				refSet[tid] = true
			}
		}
		if traceId != "" {
			refSet[traceId] = true
		}
		refIds := []string{}
		for k := range refSet {
			refIds = append(refIds, k)
		}
		traceWorkItemLinks = loadWorkItemLinksForRefIds(db, refIds)
	}

	errorMsgOut := qError
	if errorMsgOut == "" {
		errorMsgOut = timeError
	}

	renderTemplate(w, r, "traces.html", map[string]any{
		"spans":             spans,
		"total":             total,
		"limit":             limit,
		"offset":            offset,
		"service":           service,
		"selected_services": selectedServices,
		"trace_id":          traceId,
		"from_ts":           fromTs,
		"to_ts":             toTs,
		"error_msg":         errorMsgOut,
		"q":                 q,
		"services":          services,
		"sort_by":           sortBy,
		"sort_dir":          sortDir,
		"trace_detail":      traceDetail,
		"work_item_links":   traceWorkItemLinks,
	})
}
