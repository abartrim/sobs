package main

// Port of app.py lines 17303-19227:
//   - Web UI – RUM (GET /rum)
//   - Web UI – Web Traffic (GET /web-traffic) + /api/web-traffic/* aggregations
//   - Library inventory + GitHub repo health (/api/enrichment/libraries,
//     /api/enrichment/github/repo-health)
//   - CVE enrichment (GET /enrichment/cve, /api/enrichment/cve/*)
//   - Work Items (GET /work-items, GET /api/work-items)
//   - AI Transparency (_get_ai_filter_metadata, GET /ai)
//   - AI span attributes / conversation APIs, AI training data export

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func init() {
	registerRoute("GET", "/rum", requireBasicAuth(viewRum))
	registerRoute("GET", "/web-traffic", requireBasicAuth(viewWebTraffic))
	registerRoute("GET", "/api/web-traffic/geo", requireBasicAuth(apiWebTrafficGeo))
	registerRoute("GET", "/api/web-traffic/browsers", requireBasicAuth(apiWebTrafficBrowsers))
	registerRoute("GET", "/api/web-traffic/os", requireBasicAuth(apiWebTrafficOs))
	registerRoute("GET", "/api/web-traffic/timezones", requireBasicAuth(apiWebTrafficTimezones))
	registerRoute("GET", "/api/web-traffic/languages", requireBasicAuth(apiWebTrafficLanguages))
	registerRoute("GET", "/api/web-traffic/devices", requireBasicAuth(apiWebTrafficDevices))
	registerRoute("GET", "/api/enrichment/libraries", requireBasicAuth(apiEnrichmentLibraries))
	registerRoute("GET", "/api/enrichment/github/repo-health", requireBasicAuth(apiEnrichmentGithubRepoHealth))
	registerRoute("GET", "/enrichment/cve", requireBasicAuth(viewEnrichmentCve))
	registerRoute("GET", "/api/enrichment/cve/findings", requireBasicAuth(apiCveFindings))
	registerRoute("POST", "/api/enrichment/cve/findings/{osv_id}/disposition", requireBasicAuth(apiCveSetDisposition))
	registerRoute("POST", "/api/enrichment/cve/scan", requireBasicAuth(apiCveScan))
	registerRoute("GET", "/work-items", requireBasicAuth(viewWorkItems))
	registerRoute("GET", "/api/work-items", requireBasicAuth(apiGetWorkItems))
	registerRoute("GET", "/ai", requireBasicAuth(viewAi))
	registerRoute("GET", "/api/ai/span-attributes", requireBasicAuth(getAiSpanAttributes))
	registerRoute("GET", "/api/ai/conversation", requireBasicAuth(getAiConversation))
	registerRoute("GET", "/api/ai/export", requireBasicAuth(exportAiTraining))
}

// firstScalarInt extracts fetchone()[0] as int from a single-row result.
func firstScalarInt(res *ChDbResult) int {
	row := res.Fetchone()
	if row == nil || len(res.Cols) == 0 {
		return 0
	}
	return coerceInt(row[res.Cols[0]])
}

// ---------------------------------------------------------------------------
// Web UI – RUM
// ---------------------------------------------------------------------------
func viewRum(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	query := r.URL.Query()
	viewMode := strings.ToLower(strings.TrimSpace(query.Get("view")))
	if viewMode != "sessions" && viewMode != "events" {
		viewMode = "sessions"
	}
	eventType := strings.TrimSpace(query.Get("type"))
	errorSource := strings.TrimSpace(query.Get("error_source"))
	limit := parseLimit(r, 200)
	offset := parseOffset(r)
	var sortBy, sortCol, sortDir string
	if viewMode == "sessions" {
		sortBy, sortCol, sortDir = parseSort(r,
			map[string]string{
				"severity":  "severity_rank",
				"last_seen": "last_ts",
				"events":    "event_count",
				"errors":    "error_count",
			},
			"severity",
		)
	} else {
		sortBy, sortCol, sortDir = parseSort(r,
			map[string]string{"Timestamp": "Timestamp", "EventName": "EventName"},
			"Timestamp",
		)
	}
	orderDir := "DESC"
	if sortDir == "asc" {
		orderDir = "ASC"
	}
	orderClause := fmt.Sprintf("ORDER BY %s %s", sortCol, orderDir)
	fromTs, toTs, timeError := parseTimeWindowArgs(r)

	q := strings.TrimSpace(query.Get("q"))
	qError := ""
	includePatterns := []string{}
	excludePatterns := []string{}
	if q != "" {
		var regexError string
		includePatterns, excludePatterns, regexError = prepareRe2FilterPatterns(db, q)
		if regexError != "" {
			qError = regexError
		}
	}

	conditions := []string{}
	params := []any{}
	if eventType != "" {
		conditions = append(conditions, "EventName=?")
		params = append(params, eventType)
	}
	if errorSource != "" {
		conditions = append(conditions, "LogAttributes['errorSource']=?")
		params = append(params, errorSource)
	}
	appendTimeWindowFilter(&conditions, &params, "Timestamp", fromTs, toTs)
	if q != "" && qError == "" {
		appendRegexExpressionClauses(&conditions, &params, "Body", includePatterns, excludePatterns)
	}
	where := whereClause(conditions)
	total := 0
	events := []map[string]any{}
	sessionGroups := []map[string]any{}
	if viewMode == "sessions" {
		totalRes, err := db.Execute(
			"SELECT count() FROM ("+
				fmt.Sprintf("SELECT %s AS session_key ", rumSessionKeySql)+
				fmt.Sprintf("FROM hyperdx_sessions %s GROUP BY session_key)", where),
			params...,
		)
		if err != nil {
			logger.Error("view_rum sessions total query failed", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		total = firstScalarInt(totalRes)
		summaryRes, err := db.Execute(
			"SELECT "+
				fmt.Sprintf("  %s AS session_key,", rumSessionKeySql)+
				"  max(Timestamp) AS last_ts,"+
				"  count() AS event_count,"+
				"  countIf(EventName IN ('error', 'unhandledrejection')) AS error_count,"+
				"  countIf(EventName = 'web-vital' "+
				"AND JSONExtractString(Body, 'rating') = 'poor') AS poor_vital_count,"+
				"  countIf(EventName = 'web-vital' "+
				"AND JSONExtractString(Body, 'rating') = 'needs-improvement') AS warn_vital_count,"+
				"  countIf(TraceId != '') AS traced_count,"+
				"  multiIf("+
				"    countIf(EventName IN ('error', 'unhandledrejection')) > 0, 3,"+
				"    countIf(EventName = 'web-vital' "+
				"AND JSONExtractString(Body, 'rating') = 'poor') > 0, 2,"+
				"    countIf(EventName = 'web-vital' "+
				"AND JSONExtractString(Body, 'rating') = 'needs-improvement') > 0, 1,"+
				"    0"+
				"  ) AS severity_rank,"+
				"  argMax(if(LogAttributes['url'] != '', LogAttributes['url'], "+
				"LogAttributes['url.full']), Timestamp) AS last_url,"+
				"  argMax(EventName, Timestamp) AS last_event_type"+
				fmt.Sprintf(" FROM hyperdx_sessions %s", where)+
				" GROUP BY session_key "+
				fmt.Sprintf(" ORDER BY %s %s, last_ts DESC LIMIT ? OFFSET ?", sortCol, orderDir),
			append(append([]any{}, params...), limit, offset)...,
		)
		if err != nil {
			logger.Error("view_rum sessions summary query failed", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		summaryRows := summaryRes.Fetchall()

		if len(summaryRows) > 0 {
			sessionKeys := []string{}
			for _, row := range summaryRows {
				sessionKeys = append(sessionKeys, rowString(row["session_key"]))
			}
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(sessionKeys)), ",")
			detailConditions := append([]string{}, conditions...)
			detailConditions = append(detailConditions, fmt.Sprintf("%s IN (%s)", rumSessionKeySql, placeholders))
			detailWhere := "WHERE " + strings.Join(detailConditions, " AND ")
			detailParams := append([]any{}, params...)
			for _, key := range sessionKeys {
				detailParams = append(detailParams, key)
			}
			detailParams = append(detailParams, rumSessionDetailEventCap)
			detailRes, err := db.Execute(
				"SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId "+
					"FROM ("+
					"SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId, "+
					fmt.Sprintf("%s AS session_key, ", rumSessionKeySql)+
					fmt.Sprintf("row_number() OVER (PARTITION BY %s ORDER BY Timestamp DESC) AS row_rank ", rumSessionKeySql)+
					fmt.Sprintf("FROM hyperdx_sessions %s", detailWhere)+
					") "+
					"WHERE row_rank <= ? "+
					"ORDER BY session_key ASC, Timestamp DESC",
				detailParams...,
			)
			if err != nil {
				logger.Error("view_rum session detail query failed", "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			eventsBySession := map[string][]map[string]any{}
			for _, row := range detailRes.Fetchall() {
				item := buildRumEventItem(row)
				sessionKey := rowString(item["session_key"])
				eventsBySession[sessionKey] = append(eventsBySession[sessionKey], item)
			}

			for _, row := range summaryRows {
				sessionKey := rowString(row["session_key"])
				sessionEvents := eventsBySession[sessionKey]
				if sessionEvents == nil {
					sessionEvents = []map[string]any{}
				}
				sessionTraceId := ""
				for _, ev := range sessionEvents {
					if pyTruthy(ev["trace_id"]) {
						sessionTraceId = rowString(ev["trace_id"])
						break
					}
				}
				hasReplay := false
				hasArtifact := false
				for _, ev := range sessionEvents {
					if pyTruthy(ev["has_replay"]) {
						hasReplay = true
					}
					if pyTruthy(ev["has_artifact"]) {
						hasArtifact = true
					}
				}
				sessionGroups = append(sessionGroups, map[string]any{
					"session_key":      sessionKey,
					"session_id":       clipRunes(sessionKey, 8),
					"last_ts":          rowString(row["last_ts"]),
					"last_url":         rowString(row["last_url"]),
					"last_event_type":  rowString(row["last_event_type"]),
					"event_count":      coerceInt(row["event_count"]),
					"error_count":      coerceInt(row["error_count"]),
					"poor_vital_count": coerceInt(row["poor_vital_count"]),
					"warn_vital_count": coerceInt(row["warn_vital_count"]),
					"severity_rank":    coerceInt(row["severity_rank"]),
					"traced_count":     coerceInt(row["traced_count"]),
					"trace_id":         sessionTraceId,
					"has_replay":       hasReplay,
					"has_artifact":     hasArtifact,
					"events":           sessionEvents,
				})
			}
		}
	} else {
		if where == "" {
			total = activePartRows(db, "hyperdx_sessions")
		} else {
			totalRes, err := db.Execute(fmt.Sprintf("SELECT COUNT(*) FROM hyperdx_sessions %s", where), params...)
			if err != nil {
				logger.Error("view_rum events total query failed", "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			total = firstScalarInt(totalRes)
		}
		rowsRes, err := db.Execute(
			fmt.Sprintf("SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId FROM hyperdx_sessions %s ", where)+
				fmt.Sprintf("%s LIMIT ? OFFSET ?", orderClause),
			append(append([]any{}, params...), limit, offset)...,
		)
		if err != nil {
			logger.Error("view_rum events query failed", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		for _, row := range rowsRes.Fetchall() {
			events = append(events, buildRumEventItem(row))
		}
	}

	eventTypes := []string{}
	eventTypeRes, err := db.Execute("SELECT DISTINCT EventName FROM hyperdx_sessions ORDER BY EventName")
	if err != nil {
		logger.Error("view_rum event types query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	for _, row := range eventTypeRes.Fetchall() {
		eventTypes = append(eventTypes, rowString(row[eventTypeRes.Cols[0]]))
	}
	errorSources := []string{}
	errorSourceRes, err := db.Execute(
		"SELECT DISTINCT LogAttributes['errorSource'] FROM hyperdx_sessions " +
			"WHERE LogAttributes['errorSource']!='' ORDER BY LogAttributes['errorSource']",
	)
	if err != nil {
		logger.Error("view_rum error sources query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	for _, row := range errorSourceRes.Fetchall() {
		errorSources = append(errorSources, rowString(row[errorSourceRes.Cols[0]]))
	}

	// Web vitals — anomaly state + sparklines + hotspot via rule-backed derived signals
	vitalsSummary := map[string]map[string]any{}
	vitalsSparklines := map[string][]map[string]any{}
	vitalsHotspot := map[string][]map[string]any{}
	if vitalsErr := func() error {
		anomRes, err := db.Execute(
			"SELECT SignalName," +
				" argMax(value, time) AS latest_value," +
				" argMax(anomaly_state, time) AS latest_state," +
				" toUInt64(argMax(SampleCount, time)) AS latest_count" +
				" FROM v_derived_signals_anomaly" +
				" WHERE SignalSource = 'rum_vitals'" +
				"   AND time >= now() - INTERVAL 60 MINUTE" +
				" GROUP BY SignalName",
		)
		if err != nil {
			return err
		}
		for _, row := range anomRes.Fetchall() {
			nm := rowString(row["SignalName"])
			val, _ := coerceFloat(row["latest_value"])
			state := rowString(row["latest_state"])
			cnt := coerceInt(row["latest_count"])
			// PORT-NOTE: Python round() uses banker's rounding; math.Round
			// rounds half away from zero.
			p75 := math.Round(val)
			if nm == "CLS" {
				p75 = math.Round(val*1000) / 1000
			}
			vitalsSummary[nm] = map[string]any{
				"p75":           p75,
				"count":         cnt,
				"anomaly_state": state,
			}
		}
		sparkRes, err := db.Execute(
			"SELECT SignalName, MinuteBucket, Value, SampleCount" +
				" FROM v_derived_signals_1m" +
				" WHERE SignalSource = 'rum_vitals'" +
				"   AND MinuteBucket >= now() - INTERVAL 60 MINUTE" +
				" ORDER BY SignalName, MinuteBucket",
		)
		if err != nil {
			return err
		}
		for _, row := range sparkRes.Fetchall() {
			nm := rowString(row["SignalName"])
			val, _ := coerceFloat(row["Value"])
			v := math.Round(val*10) / 10
			if nm == "CLS" {
				v = math.Round(val*1000) / 1000
			}
			vitalsSparklines[nm] = append(vitalsSparklines[nm], map[string]any{
				"t": rowString(row["MinuteBucket"]),
				"v": v,
			})
		}
		hotspotRes, err := db.Execute(
			"SELECT" +
				"  JSONExtractString(Body, 'name') AS metric," +
				"  LogAttributes['url'] AS url," +
				"  count() AS total," +
				"  countIf(JSONExtractString(Body, 'rating') = 'poor') AS poor_count," +
				"  round(toFloat64(poor_count) / toFloat64(total), 3) AS poor_rate," +
				"  round(quantileExact(0.75)(JSONExtractFloat(Body, 'value')), 1) AS p75" +
				" FROM hyperdx_sessions" +
				" WHERE EventName = 'web-vital'" +
				"   AND Timestamp >= now() - INTERVAL 24 HOUR" +
				" GROUP BY metric, url" +
				" HAVING total >= 3" +
				" ORDER BY metric ASC, poor_rate DESC, total DESC" +
				" LIMIT 60",
		)
		if err != nil {
			return err
		}
		for _, row := range hotspotRes.Fetchall() {
			metric := rowString(row["metric"])
			if metric == "" {
				continue
			}
			poorRate, _ := coerceFloat(row["poor_rate"])
			p75, _ := coerceFloat(row["p75"])
			vitalsHotspot[metric] = append(vitalsHotspot[metric], map[string]any{
				"url":        rowString(row["url"]),
				"total":      coerceInt(row["total"]),
				"poor_count": coerceInt(row["poor_count"]),
				"poor_rate":  poorRate,
				"p75":        p75,
			})
		}
		for metric, entries := range vitalsHotspot {
			if len(entries) > 5 {
				vitalsHotspot[metric] = entries[:5]
			}
		}
		return nil
	}(); vitalsErr != nil {
		logger.Error("vitals derived-signal query failed", "error", vitalsErr)
	}
	// Error trend — sparkline + direction + top messages + top URLs (vs now())
	errorStats := map[string]any{
		"total":        0,
		"by_type":      map[string]int{},
		"trend":        "stable",
		"recent":       0,
		"prior":        0,
		"sparkline":    []map[string]any{},
		"top_messages": []map[string]any{},
		"top_urls":     []map[string]any{},
	}
	if errStatsErr := func() error {
		trendRes, err := db.Execute(
			"SELECT" +
				" countIf(Timestamp >= now() - INTERVAL 30 MINUTE) AS recent," +
				" countIf(" +
				"   Timestamp >= now() - INTERVAL 60 MINUTE" +
				"   AND Timestamp < now() - INTERVAL 30 MINUTE" +
				" ) AS prior" +
				" FROM hyperdx_sessions" +
				" WHERE EventName IN ('error', 'unhandledrejection')" +
				"   AND Timestamp >= now() - INTERVAL 60 MINUTE",
		)
		if err != nil {
			return err
		}
		if trendRow := trendRes.Fetchone(); trendRow != nil {
			recentCnt := coerceInt(trendRow["recent"])
			priorCnt := coerceInt(trendRow["prior"])
			errorStats["recent"] = recentCnt
			errorStats["prior"] = priorCnt
			errTrend := "stable"
			if priorCnt == 0 {
				if recentCnt != 0 {
					errTrend = "up"
				}
			} else if float64(recentCnt) > float64(priorCnt)*1.25 {
				errTrend = "up"
			} else if float64(recentCnt) < float64(priorCnt)*0.75 {
				errTrend = "down"
			}
			errorStats["trend"] = errTrend
		}
		typeRes, err := db.Execute(
			"SELECT EventName, count() AS cnt" +
				" FROM hyperdx_sessions" +
				" WHERE EventName IN ('error', 'unhandledrejection')" +
				"   AND Timestamp >= now() - INTERVAL 24 HOUR" +
				" GROUP BY EventName",
		)
		if err != nil {
			return err
		}
		total24h := 0
		byType := map[string]int{}
		for _, row := range typeRes.Fetchall() {
			cnt := coerceInt(row["cnt"])
			total24h += cnt
			byType[rowString(row["EventName"])] = cnt
		}
		errorStats["total"] = total24h
		errorStats["by_type"] = byType
		sparkRes, err := db.Execute(
			"SELECT mb, cnt" +
				" FROM (" +
				"   SELECT toStartOfMinute(Timestamp) AS mb, count() AS cnt" +
				"   FROM hyperdx_sessions" +
				"   WHERE EventName IN ('error', 'unhandledrejection')" +
				"     AND Timestamp >= now() - INTERVAL 180 MINUTE" +
				"   GROUP BY mb" +
				" )" +
				" ORDER BY mb" +
				" WITH FILL" +
				" FROM toStartOfMinute(now() - INTERVAL 180 MINUTE)" +
				" TO toStartOfMinute(now())" +
				" STEP toIntervalMinute(1)",
		)
		if err != nil {
			return err
		}
		sparkline := []map[string]any{}
		for _, row := range sparkRes.Fetchall() {
			sparkline = append(sparkline, map[string]any{"t": rowString(row["mb"]), "v": coerceInt(row["cnt"])})
		}
		errorStats["sparkline"] = sparkline
		msgRes, err := db.Execute(
			"SELECT JSONExtractString(Body, 'message') AS message, count() AS cnt" +
				" FROM hyperdx_sessions" +
				" WHERE EventName IN ('error', 'unhandledrejection')" +
				"   AND Timestamp >= now() - INTERVAL 24 HOUR" +
				"   AND JSONExtractString(Body, 'message') != ''" +
				" GROUP BY message ORDER BY cnt DESC LIMIT 8",
		)
		if err != nil {
			return err
		}
		topMessages := []map[string]any{}
		for _, row := range msgRes.Fetchall() {
			topMessages = append(topMessages, map[string]any{
				"message": rowString(row["message"]),
				"count":   coerceInt(row["cnt"]),
			})
		}
		errorStats["top_messages"] = topMessages
		urlRes, err := db.Execute(
			"SELECT LogAttributes['url'] AS url, count() AS cnt" +
				" FROM hyperdx_sessions" +
				" WHERE EventName IN ('error', 'unhandledrejection')" +
				"   AND Timestamp >= now() - INTERVAL 24 HOUR" +
				"   AND LogAttributes['url'] != ''" +
				" GROUP BY url ORDER BY cnt DESC LIMIT 5",
		)
		if err != nil {
			return err
		}
		topUrls := []map[string]any{}
		for _, row := range urlRes.Fetchall() {
			topUrls = append(topUrls, map[string]any{
				"url":   rowString(row["url"]),
				"count": coerceInt(row["cnt"]),
			})
		}
		errorStats["top_urls"] = topUrls
		return nil
	}(); errStatsErr != nil {
		logger.Error("error stats query failed", "error", errStatsErr)
	}

	errorMsg := qError
	if errorMsg == "" {
		errorMsg = timeError
	}
	renderTemplate(w, r, "rum.html", map[string]any{
		"events":            events,
		"session_groups":    sessionGroups,
		"total":             total,
		"limit":             limit,
		"offset":            offset,
		"view_mode":         viewMode,
		"event_type":        eventType,
		"event_types":       eventTypes,
		"error_source":      errorSource,
		"error_sources":     errorSources,
		"vitals_summary":    vitalsSummary,
		"vitals_sparklines": vitalsSparklines,
		"vitals_hotspot":    vitalsHotspot,
		"error_stats":       errorStats,
		"sort_by":           sortBy,
		"sort_dir":          sortDir,
		"from_ts":           fromTs,
		"to_ts":             toTs,
		"q":                 q,
		"error_msg":         errorMsg,
	})
}

// geoEnabledFromSetting mirrors the repeated inline expression:
// (_get_app_setting(db, _GEO_ENABLED_SETTING) or "true").lower() in ("1", "true", "yes")
func geoEnabledFromSetting(db *ChDbConnection) bool {
	raw := getAppSetting(db, geoEnabledSetting)
	if raw == "" {
		raw = "true"
	}
	lower := strings.ToLower(raw)
	return lower == "1" || lower == "true" || lower == "yes"
}

// ---------------------------------------------------------------------------
// Web UI – Web Traffic (IP geo-map, browser context analytics)
// ---------------------------------------------------------------------------

// viewWebTraffic renders web traffic analytics: IP→geo map, top URLs, and
// browser context breakdown.
func viewWebTraffic(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	fromTs, toTs, timeError := parseTimeWindowArgs(r)
	timeConditions, timeParams := timeWindowConditions("Timestamp", fromTs, toTs)
	where := whereClause(timeConditions)

	total := 0
	if where == "" {
		total = activePartRows(db, "hyperdx_sessions")
	} else {
		totalRes, err := db.Execute(fmt.Sprintf("SELECT COUNT(*) FROM hyperdx_sessions %s", where), timeParams...)
		if err != nil {
			logger.Error("view_web_traffic total query failed", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		total = firstScalarInt(totalRes)
	}

	topUrlsRes, err := db.Execute(
		fmt.Sprintf("SELECT LogAttributes['url'] AS url, COUNT(*) AS cnt "+
			"FROM hyperdx_sessions %s "+
			"GROUP BY url HAVING url != '' ORDER BY cnt DESC LIMIT 20", where),
		timeParams...,
	)
	if err != nil {
		logger.Error("view_web_traffic top urls query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// PORT-NOTE: Python builds (url, count) tuples; the Go port passes 2-element
	// slices so templates can index them positionally.
	topUrls := [][]any{}
	for _, row := range topUrlsRes.Fetchall() {
		topUrls = append(topUrls, []any{rowString(row["url"]), coerceInt(row["cnt"])})
	}

	eventTypeRes, err := db.Execute(
		fmt.Sprintf("SELECT EventName, COUNT(*) AS cnt FROM hyperdx_sessions %s "+
			"GROUP BY EventName ORDER BY cnt DESC LIMIT 20", where),
		timeParams...,
	)
	if err != nil {
		logger.Error("view_web_traffic event types query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	eventTypes := [][]any{}
	for _, row := range eventTypeRes.Fetchall() {
		eventTypes = append(eventTypes, []any{rowString(row["EventName"]), coerceInt(row["cnt"])})
	}

	geoEnabled := geoEnabledFromSetting(db)

	renderTemplate(w, r, "web_traffic.html", map[string]any{
		"total":       total,
		"top_urls":    topUrls,
		"event_types": eventTypes,
		"from_ts":     fromTs,
		"to_ts":       toTs,
		"error_msg":   timeError,
		"geo_enabled": geoEnabled,
	})
}

// ---------------------------------------------------------------------------
// API – Web Traffic geo aggregation  GET /api/web-traffic/geo
// ---------------------------------------------------------------------------

// apiWebTrafficGeo returns IP→country aggregation from RUM events using the
// local geoip2fast DB.
//
// All lookups are performed locally (no external network calls).
// geoip2fast is MIT licensed; bundled data is from IANA/RIR (public domain).
func apiWebTrafficGeo(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	fromTs, toTs, _ := parseTimeWindowArgs(r)
	timeConditions, timeParams := timeWindowConditions("Timestamp", fromTs, toTs)
	where := whereClause(timeConditions)

	res, err := db.Execute(
		fmt.Sprintf("SELECT LogAttributes['client.ip'] AS ip, COUNT(*) AS cnt "+
			"FROM hyperdx_sessions %s "+
			"GROUP BY ip HAVING ip != '' ORDER BY cnt DESC LIMIT 200", where),
		timeParams...,
	)
	if err != nil {
		logger.Error("api_web_traffic_geo query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	// PORT-NOTE: Python's ip_counts dict preserves insertion (cnt DESC) order;
	// the Go port keeps an ordered slice alongside the count map.
	ipOrder := []string{}
	ipCounts := map[string]int{}
	for _, row := range res.Fetchall() {
		ip := rowString(row["ip"])
		if _, seen := ipCounts[ip]; !seen {
			ipOrder = append(ipOrder, ip)
		}
		ipCounts[ip] = coerceInt(row["cnt"])
	}

	geoEnabled := geoEnabledFromSetting(db)
	geoData := geoLookupBatch(ipOrder, geoEnabled)

	countryOrder := []string{}
	countryTotals := map[string]int{}
	ipDetails := []map[string]any{}
	for _, ip := range ipOrder {
		cnt := ipCounts[ip]
		geo := geoData[ip]
		country := rowString(geo["country"])
		if country == "" {
			country = "Unknown"
		}
		countryCode := rowString(geo["country_code"])
		if _, seen := countryTotals[country]; !seen {
			countryOrder = append(countryOrder, country)
		}
		countryTotals[country] += cnt
		ipDetails = append(ipDetails, map[string]any{
			"ip":           ip,
			"count":        cnt,
			"country":      country,
			"country_code": countryCode,
		})
	}

	countryCounts := []map[string]any{}
	for _, name := range countryOrder {
		countryCounts = append(countryCounts, map[string]any{"name": name, "value": countryTotals[name]})
	}
	sort.SliceStable(countryCounts, func(i, j int) bool {
		return coerceInt(countryCounts[i]["value"]) > coerceInt(countryCounts[j]["value"])
	})
	if len(ipDetails) > 100 {
		ipDetails = ipDetails[:100]
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":             true,
		"country_counts": countryCounts,
		"ip_details":     ipDetails,
		"geo_enabled":    geoEnabled,
	})
}

// ---------------------------------------------------------------------------
// API – Web Traffic browser context aggregation (GET /api/web-traffic/browsers, etc.)
// ---------------------------------------------------------------------------

// apiWebTrafficBrowsers returns browser name/version aggregation from RUM events.
func apiWebTrafficBrowsers(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	fromTs, toTs, _ := parseTimeWindowArgs(r)
	timeConditions, timeParams := timeWindowConditions("Timestamp", fromTs, toTs)
	where := whereClause(timeConditions)

	res, err := db.Execute(
		fmt.Sprintf("SELECT LogAttributes['browser.context.browserName'] AS browser, "+
			"LogAttributes['browser.context.browserVersion'] AS version, COUNT(*) AS cnt "+
			"FROM hyperdx_sessions %s "+
			"GROUP BY browser, version ORDER BY cnt DESC LIMIT 50", where),
		timeParams...,
	)
	if err != nil {
		logger.Error("api_web_traffic_browsers query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	browsers := []map[string]any{}
	for _, row := range res.Fetchall() {
		name := strings.TrimSpace(fmt.Sprintf("%s %s", rowString(row["browser"]), rowString(row["version"])))
		if name == "" {
			name = "Unknown"
		}
		browsers = append(browsers, map[string]any{
			"name":  name,
			"value": coerceInt(row["cnt"]),
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "browsers": browsers})
}

// apiWebTrafficOs returns OS name/version aggregation from RUM events.
func apiWebTrafficOs(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	fromTs, toTs, _ := parseTimeWindowArgs(r)
	timeConditions, timeParams := timeWindowConditions("Timestamp", fromTs, toTs)
	where := whereClause(timeConditions)

	res, err := db.Execute(
		fmt.Sprintf("SELECT LogAttributes['browser.context.osName'] AS os, "+
			"LogAttributes['browser.context.osVersion'] AS version, COUNT(*) AS cnt "+
			"FROM hyperdx_sessions %s "+
			"GROUP BY os, version ORDER BY cnt DESC LIMIT 50", where),
		timeParams...,
	)
	if err != nil {
		logger.Error("api_web_traffic_os query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	operatingSystems := []map[string]any{}
	for _, row := range res.Fetchall() {
		name := strings.TrimSpace(fmt.Sprintf("%s %s", rowString(row["os"]), rowString(row["version"])))
		if name == "" {
			name = "Unknown"
		}
		operatingSystems = append(operatingSystems, map[string]any{
			"name":  name,
			"value": coerceInt(row["cnt"]),
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "operating_systems": operatingSystems})
}

// apiWebTrafficTimezones returns timezone aggregation from RUM events.
func apiWebTrafficTimezones(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	fromTs, toTs, _ := parseTimeWindowArgs(r)
	timeConditions, timeParams := timeWindowConditions("Timestamp", fromTs, toTs)
	where := whereClause(timeConditions)

	res, err := db.Execute(
		fmt.Sprintf("SELECT LogAttributes['browser.context.timezone'] AS tz, COUNT(*) AS cnt "+
			"FROM hyperdx_sessions %s "+
			"GROUP BY tz HAVING tz != '' ORDER BY cnt DESC LIMIT 50", where),
		timeParams...,
	)
	if err != nil {
		logger.Error("api_web_traffic_timezones query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	timezones := []map[string]any{}
	for _, row := range res.Fetchall() {
		timezones = append(timezones, map[string]any{"name": rowString(row["tz"]), "value": coerceInt(row["cnt"])})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "timezones": timezones})
}

// apiWebTrafficLanguages returns language aggregation from RUM events.
func apiWebTrafficLanguages(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	fromTs, toTs, _ := parseTimeWindowArgs(r)
	timeConditions, timeParams := timeWindowConditions("Timestamp", fromTs, toTs)
	where := whereClause(timeConditions)

	res, err := db.Execute(
		fmt.Sprintf("SELECT LogAttributes['browser.context.language'] AS lang, COUNT(*) AS cnt "+
			"FROM hyperdx_sessions %s "+
			"GROUP BY lang HAVING lang != '' ORDER BY cnt DESC LIMIT 50", where),
		timeParams...,
	)
	if err != nil {
		logger.Error("api_web_traffic_languages query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	languages := []map[string]any{}
	for _, row := range res.Fetchall() {
		languages = append(languages, map[string]any{"name": rowString(row["lang"]), "value": coerceInt(row["cnt"])})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "languages": languages})
}

// apiWebTrafficDevices returns device class aggregation from RUM events.
func apiWebTrafficDevices(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	fromTs, toTs, _ := parseTimeWindowArgs(r)
	timeConditions, timeParams := timeWindowConditions("Timestamp", fromTs, toTs)
	where := whereClause(timeConditions)

	res, err := db.Execute(
		fmt.Sprintf("SELECT LogAttributes['browser.context.deviceClass'] AS device, COUNT(*) AS cnt "+
			"FROM hyperdx_sessions %s "+
			"GROUP BY device HAVING device != '' ORDER BY cnt DESC", where),
		timeParams...,
	)
	if err != nil {
		logger.Error("api_web_traffic_devices query failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	devices := []map[string]any{}
	for _, row := range res.Fetchall() {
		devices = append(devices, map[string]any{"name": rowString(row["device"]), "value": coerceInt(row["cnt"])})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "devices": devices})
}

// apiEnrichmentLibraries returns merged library inventory with CVE counts and
// provenance.
func apiEnrichmentLibraries(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	libraries := []map[string]any{}
	if err := func() error {
		inventory := collectLibraryInventory(db)
		cveRes, err := db.Execute(
			"SELECT Package, Ecosystem, Version, countDistinct(OsvId) AS cve_count " +
				"FROM sobs_cve_findings FINAL " +
				"GROUP BY Package, Ecosystem, Version",
		)
		if err != nil {
			return err
		}
		cveCountByKey := map[string]int{}
		for _, row := range cveRes.Fetchall() {
			key := fmt.Sprintf("%s::%s::%s",
				rowString(row["Package"]), rowString(row["Ecosystem"]), rowString(row["Version"]))
			cveCountByKey[key] = coerceInt(row["cve_count"])
		}
		sourceOrder := map[string]int{"release_registry": 0, "otel_sdk": 1, "otel_scope": 2}

		for _, item := range inventory {
			pkg := item["package"]
			ecosystem := item["ecosystem"]
			version := item["version"]
			service := item["service"]
			if service == "" {
				service = item["app_name"]
			}
			source := item["source"]
			cveCount := cveCountByKey[fmt.Sprintf("%s::%s::%s", pkg, ecosystem, version)]
			status := "clean"
			if ecosystem == "" {
				status = "unknown_ecosystem"
			} else if cveCount > 0 {
				status = "vulnerable"
			}
			libraries = append(libraries, map[string]any{
				"package":         pkg,
				"ecosystem":       ecosystem,
				"version":         version,
				"service":         service,
				"source":          source,
				"app_name":        item["app_name"],
				"release_version": item["release_version"],
				"environment":     item["environment"],
				"cve_count":       cveCount,
				"status":          status,
			})
		}

		sort.SliceStable(libraries, func(i, j int) bool {
			a, b := libraries[i], libraries[j]
			ac, bc := coerceInt(a["cve_count"]), coerceInt(b["cve_count"])
			if ac != bc {
				return ac > bc
			}
			rank := func(m map[string]any) int {
				if v, ok := sourceOrder[rowString(m["source"])]; ok {
					return v
				}
				return 99
			}
			ar, br := rank(a), rank(b)
			if ar != br {
				return ar < br
			}
			ap, bp := strings.ToLower(rowString(a["package"])), strings.ToLower(rowString(b["package"]))
			if ap != bp {
				return ap < bp
			}
			av, bv := strings.ToLower(rowString(a["version"])), strings.ToLower(rowString(b["version"]))
			if av != bv {
				return av < bv
			}
			return strings.ToLower(rowString(a["service"])) < strings.ToLower(rowString(b["service"]))
		})
		return nil
	}(); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":         true,
		"libraries":  libraries,
		"scanned_at": getAppSetting(db, cveLastScanSetting),
	})
}

// collectGithubRepoHealthSummary returns version-scoped GitHub repo health
// counts for CVE workflow context.
func collectGithubRepoHealthSummary(db *ChDbConnection) map[string]any {
	defaultGithubToken := strings.TrimSpace(loadAiSetting(db, "ai.github_token", ""))

	appRes, err := db.Execute(
		"SELECT Id, Name, Slug, RepoUrl " +
			"FROM sobs_apps FINAL " +
			"WHERE IsDeleted=0 AND Enabled=1 AND RepoUrl != '' " +
			"ORDER BY Name ASC",
	)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	releaseRes, err := db.Execute(
		"SELECT AppId, ReleaseVersion " +
			"FROM sobs_app_releases FINAL " +
			"WHERE IsDeleted=0 " +
			"ORDER BY ReleasedAt DESC LIMIT 4000",
	)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	appRows := appRes.Fetchall()
	releaseRows := releaseRes.Fetchall()

	versionsByApp := map[string][]string{}
	for _, row := range releaseRows {
		appId := rowString(row["AppId"])
		relVer := strings.TrimSpace(rowString(row["ReleaseVersion"]))
		if appId == "" || relVer == "" {
			continue
		}
		versions := versionsByApp[appId]
		seen := false
		for _, v := range versions {
			if v == relVer {
				seen = true
				break
			}
		}
		if !seen && len(versions) < 5 {
			versionsByApp[appId] = append(versions, relVer)
		}
	}

	repoTargets := []map[string]any{}
	for _, row := range appRows {
		appId := rowString(row["Id"])
		appName := rowString(row["Name"])
		if appName == "" {
			appName = rowString(row["Slug"])
		}
		repoUrl := rowString(row["RepoUrl"])
		owner, repo := parseGithubRepoOwnerName(repoUrl)
		versions := versionsByApp[appId]
		if owner == "" || repo == "" || len(versions) == 0 {
			continue
		}
		repoTargets = append(repoTargets, map[string]any{
			"app_name": appName,
			"owner":    owner,
			"repo":     repo,
			"versions": versions,
		})
	}

	if len(repoTargets) > githubRepoHealthMaxRepos {
		repoTargets = repoTargets[:githubRepoHealthMaxRepos]
	}

	totalOpenIssues := 0
	totalOpenPrs := 0
	totalSecurityItems := 0
	scannedRepos := 0
	reposSummary := []map[string]any{}

	for _, target := range repoTargets {
		owner := rowString(target["owner"])
		repo := rowString(target["repo"])
		githubToken := loadRepoScopedGithubToken(db, owner, repo)
		if githubToken == "" {
			githubToken = defaultGithubToken
		}
		if githubToken == "" {
			continue
		}
		versions := []string{}
		if vs, ok := target["versions"].([]string); ok {
			for _, v := range vs {
				if strings.TrimSpace(v) != "" {
					versions = append(versions, v)
				}
			}
		}
		versionTokens := map[string]bool{}
		for _, version := range versions {
			for token := range githubVersionTokens(version) {
				versionTokens[token] = true
			}
		}
		if len(versionTokens) == 0 {
			continue
		}

		scannedRepos++
		items := []any{}
		fetchOk := func() bool {
			params := url.Values{}
			params.Set("state", "open")
			params.Set("per_page", strconv.Itoa(githubRepoHealthMaxItemsPerRepo))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet,
				fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?%s", owner, repo, params.Encode()), nil)
			if err != nil {
				return false
			}
			req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", githubToken))
			req.Header.Set("Accept", "application/vnd.github+json")
			req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
			resp, err := httpClient.Do(req)
			if err != nil {
				return false
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != 200 {
				return false
			}
			body, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				return false
			}
			var parsed any = []any{}
			if len(body) > 0 {
				if err := json.Unmarshal(body, &parsed); err != nil {
					return false
				}
			}
			list, ok := parsed.([]any)
			if !ok {
				return false
			}
			items = list
			return true
		}()
		if !fetchOk {
			continue
		}

		repoIssues := 0
		repoPrs := 0
		repoSecurity := 0

		for _, itemAny := range items {
			item, ok := itemAny.(map[string]any)
			if !ok {
				continue
			}
			text := fmt.Sprintf("%s\n%s", rowString(item["title"]), rowString(item["body"]))
			if !textMentionsVersionTokens(text, versionTokens) {
				continue
			}
			_, isPr := item["pull_request"].(map[string]any)
			if isPr {
				repoPrs++
			} else {
				repoIssues++
			}
			if githubItemIsSecurityRelated(item) {
				repoSecurity++
			}
		}

		totalOpenIssues += repoIssues
		totalOpenPrs += repoPrs
		totalSecurityItems += repoSecurity
		reposSummary = append(reposSummary, map[string]any{
			"repo":           fmt.Sprintf("%s/%s", owner, repo),
			"app_name":       rowString(target["app_name"]),
			"versions":       versions,
			"open_issues":    repoIssues,
			"open_prs":       repoPrs,
			"security_items": repoSecurity,
		})
	}

	sort.SliceStable(reposSummary, func(i, j int) bool {
		a, b := reposSummary[i], reposSummary[j]
		aScore := coerceInt(a["security_items"]) + coerceInt(a["open_issues"]) + coerceInt(a["open_prs"])
		bScore := coerceInt(b["security_items"]) + coerceInt(b["open_issues"]) + coerceInt(b["open_prs"])
		if aScore != bScore {
			return aScore > bScore
		}
		return strings.ToLower(rowString(a["repo"])) < strings.ToLower(rowString(b["repo"]))
	})

	return map[string]any{
		"ok":                     true,
		"scanned_repos":          scannedRepos,
		"total_repos_considered": len(repoTargets),
		"open_issues":            totalOpenIssues,
		"open_prs":               totalOpenPrs,
		"security_items":         totalSecurityItems,
		"version_scoped":         true,
		"last_synced_at":         nowIso(),
		"repos":                  reposSummary,
	}
}

// apiEnrichmentGithubRepoHealth returns version-scoped GitHub repo health
// counts for CVE workflow context.
func apiEnrichmentGithubRepoHealth(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	summary := collectGithubRepoHealthSummary(db)
	if ok, _ := summary["ok"].(bool); !ok {
		jsonResponse(w, http.StatusInternalServerError, summary)
		return
	}
	jsonResponse(w, http.StatusOK, summary)
}

// ---------------------------------------------------------------------------
// API – CVE enrichment endpoints
// Uses OSV.dev (Apache 2.0, free, no API key required)
// Reference: https://google.github.io/osv.dev/api/
// ---------------------------------------------------------------------------

// viewEnrichmentCve renders the dedicated CVE / vulnerability findings page.
func viewEnrichmentCve(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	query := r.URL.Query()
	cveEnabledRaw := getAppSetting(db, cveEnabledSetting)
	if cveEnabledRaw == "" {
		cveEnabledRaw = "true"
	}
	cveEnabledLower := strings.ToLower(cveEnabledRaw)
	cveEnabled := cveEnabledLower == "1" || cveEnabledLower == "true" || cveEnabledLower == "yes"
	cveLastScan := getAppSetting(db, cveLastScanSetting)
	githubBackfillMaxReleasesValue := githubBackfillMaxReleases(db)
	parseSettingInt := func(key string) int {
		raw := getAppSetting(db, key)
		if raw == "" {
			raw = "0"
		}
		value, err := strconv.Atoi(raw)
		if err != nil {
			return 0
		}
		return value
	}
	cveLastBackfillAttempted := parseSettingInt(cveLastBackfillAttemptedSetting)
	cveLastBackfillInserted := parseSettingInt(cveLastBackfillInsertedSetting)
	cveLastBackfillCap := parseSettingInt(cveLastBackfillCapSetting)

	selectedSeverities := []string{}
	for _, s := range query["severity"] {
		if v := strings.TrimSpace(s); v != "" {
			selectedSeverities = append(selectedSeverities, v)
		}
	}
	selectedEcosystems := []string{}
	for _, e := range query["ecosystem"] {
		if v := strings.TrimSpace(e); v != "" {
			selectedEcosystems = append(selectedEcosystems, v)
		}
	}
	severityFilter := ""
	if len(selectedSeverities) > 0 {
		severityFilter = selectedSeverities[0]
	}
	ecosystemFilter := ""
	if len(selectedEcosystems) > 0 {
		ecosystemFilter = selectedEcosystems[0]
	}
	packageFilter := strings.TrimSpace(query.Get("package"))
	showAllRaw := strings.ToLower(strings.TrimSpace(query.Get("show_all")))
	showAll := showAllRaw == "1" || showAllRaw == "true" || showAllRaw == "yes" || showAllRaw == "on"

	cveFindings := []map[string]any{}
	ecosystems := []string{}
	severities := []string{}
	if cveEnabled {
		if err := func() error {
			versionsByPackage := inventoryVersionsByPackage(db)
			dispositionRes, err := db.Execute(
				"SELECT OsvId, Package, Ecosystem, Version, Disposition, Note " + "FROM sobs_cve_dispositions FINAL",
			)
			if err != nil {
				return err
			}
			dispositionsByKey := map[string]map[string]string{}
			for _, dr := range dispositionRes.Fetchall() {
				disposition := rowString(dr["Disposition"])
				if disposition == "" {
					disposition = "open"
				}
				key := fmt.Sprintf("%s::%s::%s::%s",
					rowString(dr["OsvId"]), rowString(dr["Package"]),
					rowString(dr["Ecosystem"]), rowString(dr["Version"]))
				dispositionsByKey[key] = map[string]string{
					"disposition": disposition,
					"note":        rowString(dr["Note"]),
				}
			}
			res, err := db.Execute(
				"SELECT Package, Ecosystem, Version, ServiceName, OsvId, CveIds, Summary, Severity, Published " +
					"FROM sobs_cve_findings FINAL " +
					"ORDER BY Published DESC LIMIT 500",
			)
			if err != nil {
				return err
			}
			ecosystemSet := map[string]bool{}
			severitySet := map[string]bool{}
			for _, fr := range res.Fetchall() {
				findingKey := fmt.Sprintf("%s::%s::%s::%s",
					rowString(fr["OsvId"]), rowString(fr["Package"]),
					rowString(fr["Ecosystem"]), rowString(fr["Version"]))
				rawDisposition := "open"
				dispositionNote := ""
				if d, ok := dispositionsByKey[findingKey]; ok {
					if d["disposition"] != "" {
						rawDisposition = d["disposition"]
					}
					dispositionNote = d["note"]
				}
				disposition, dispositionExpired := effectiveCveDisposition(
					rawDisposition,
					rowString(fr["Package"]),
					rowString(fr["Ecosystem"]),
					rowString(fr["Version"]),
					versionsByPackage,
				)
				cveIds := []string{}
				for _, c := range strings.Split(rowString(fr["CveIds"]), ",") {
					if c != "" {
						cveIds = append(cveIds, c)
					}
				}
				cveFindings = append(cveFindings, map[string]any{
					"package":             rowString(fr["Package"]),
					"ecosystem":           rowString(fr["Ecosystem"]),
					"version":             rowString(fr["Version"]),
					"service":             rowString(fr["ServiceName"]),
					"osv_id":              rowString(fr["OsvId"]),
					"cve_ids":             cveIds,
					"summary":             rowString(fr["Summary"]),
					"severity":            rowString(fr["Severity"]),
					"published":           rowString(fr["Published"]),
					"disposition":         disposition,
					"raw_disposition":     rawDisposition,
					"disposition_expired": dispositionExpired,
					"disposition_note":    dispositionNote,
				})
				if eco := rowString(fr["Ecosystem"]); eco != "" {
					ecosystemSet[eco] = true
				}
				if sev := rowString(fr["Severity"]); sev != "" {
					severitySet[sev] = true
				}
			}
			ecosystems = sortedStringSet(ecosystemSet)
			severities = sortedStringSet(severitySet)
			if len(selectedSeverities) > 0 {
				selectedSeveritySet := map[string]bool{}
				for _, s := range selectedSeverities {
					selectedSeveritySet[s] = true
				}
				filtered := []map[string]any{}
				for _, f := range cveFindings {
					if selectedSeveritySet[rowString(f["severity"])] {
						filtered = append(filtered, f)
					}
				}
				cveFindings = filtered
			}
			if len(selectedEcosystems) > 0 {
				selectedEcosystemSet := map[string]bool{}
				for _, e := range selectedEcosystems {
					selectedEcosystemSet[e] = true
				}
				filtered := []map[string]any{}
				for _, f := range cveFindings {
					if selectedEcosystemSet[rowString(f["ecosystem"])] {
						filtered = append(filtered, f)
					}
				}
				cveFindings = filtered
			}
			if packageFilter != "" {
				pkgLower := strings.ToLower(packageFilter)
				filtered := []map[string]any{}
				for _, f := range cveFindings {
					if strings.Contains(strings.ToLower(rowString(f["package"])), pkgLower) {
						filtered = append(filtered, f)
					}
				}
				cveFindings = filtered
			}
			if !showAll {
				filtered := []map[string]any{}
				for _, f := range cveFindings {
					disposition := rowString(f["disposition"])
					if disposition == "" {
						disposition = "open"
					}
					if disposition != "accepted" && disposition != "false_positive" && disposition != "fixed" {
						filtered = append(filtered, f)
					}
				}
				cveFindings = filtered
			}
			return nil
		}(); err != nil {
			// Python: broad `except Exception: pass`.
			logger.Debug("view_enrichment_cve findings load failed", "error", err)
		}
	}

	renderTemplate(w, r, "cve.html", map[string]any{
		"cve_enabled":                  cveEnabled,
		"cve_last_scan":                cveLastScan,
		"github_backfill_max_releases": githubBackfillMaxReleasesValue,
		"cve_last_backfill_attempted":  cveLastBackfillAttempted,
		"cve_last_backfill_inserted":   cveLastBackfillInserted,
		"cve_last_backfill_cap":        cveLastBackfillCap,
		"cve_findings":                 cveFindings,
		"ecosystems":                   ecosystems,
		"severities":                   severities,
		"severity_filter":              severityFilter,
		"ecosystem_filter":             ecosystemFilter,
		"selected_severities":          selectedSeverities,
		"selected_ecosystems":          selectedEcosystems,
		"package_filter":               packageFilter,
		"show_all":                     showAll,
	})
}

// apiCveFindings returns the most recent CVE findings stored from the last
// background scan.
func apiCveFindings(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	cveEnabledRaw := getAppSetting(db, cveEnabledSetting)
	if cveEnabledRaw == "" {
		cveEnabledRaw = "true"
	}
	cveEnabledLower := strings.ToLower(cveEnabledRaw)
	cveEnabled := cveEnabledLower == "1" || cveEnabledLower == "true" || cveEnabledLower == "yes"
	if !cveEnabled {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "CVE enrichment is disabled"})
		return
	}
	findings := []map[string]any{}
	lastScan := ""
	if err := func() error {
		showAllRaw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("show_all")))
		showAll := showAllRaw == "1" || showAllRaw == "true" || showAllRaw == "yes" || showAllRaw == "on"
		versionsByPackage := inventoryVersionsByPackage(db)
		dispositionRes, err := db.Execute(
			"SELECT OsvId, Package, Ecosystem, Version, Disposition, Note " + "FROM sobs_cve_dispositions FINAL",
		)
		if err != nil {
			return err
		}
		dispositionsByKey := map[string]map[string]string{}
		for _, dr := range dispositionRes.Fetchall() {
			disposition := rowString(dr["Disposition"])
			if disposition == "" {
				disposition = "open"
			}
			key := fmt.Sprintf("%s::%s::%s::%s",
				rowString(dr["OsvId"]), rowString(dr["Package"]),
				rowString(dr["Ecosystem"]), rowString(dr["Version"]))
			dispositionsByKey[key] = map[string]string{
				"disposition": disposition,
				"note":        rowString(dr["Note"]),
			}
		}
		res, err := db.Execute(
			"SELECT Package, Ecosystem, Version, ServiceName, OsvId, CveIds, Summary, Severity, Published " +
				"FROM sobs_cve_findings FINAL " +
				"ORDER BY Published DESC LIMIT 100",
		)
		if err != nil {
			return err
		}
		for _, fr := range res.Fetchall() {
			findingKey := fmt.Sprintf("%s::%s::%s::%s",
				rowString(fr["OsvId"]), rowString(fr["Package"]),
				rowString(fr["Ecosystem"]), rowString(fr["Version"]))
			dispositionData := dispositionsByKey[findingKey]
			rawDisposition := "open"
			dispositionNote := ""
			if dispositionData != nil {
				if dispositionData["disposition"] != "" {
					rawDisposition = dispositionData["disposition"]
				}
				dispositionNote = dispositionData["note"]
			}
			disposition, dispositionExpired := effectiveCveDisposition(
				rawDisposition,
				rowString(fr["Package"]),
				rowString(fr["Ecosystem"]),
				rowString(fr["Version"]),
				versionsByPackage,
			)
			if !showAll && (disposition == "accepted" || disposition == "false_positive" || disposition == "fixed") {
				continue
			}
			cveIds := []string{}
			for _, c := range strings.Split(rowString(fr["CveIds"]), ",") {
				if c != "" {
					cveIds = append(cveIds, c)
				}
			}
			findings = append(findings, map[string]any{
				"package":             rowString(fr["Package"]),
				"ecosystem":           rowString(fr["Ecosystem"]),
				"version":             rowString(fr["Version"]),
				"service":             rowString(fr["ServiceName"]),
				"osv_id":              rowString(fr["OsvId"]),
				"cve_ids":             cveIds,
				"summary":             rowString(fr["Summary"]),
				"severity":            rowString(fr["Severity"]),
				"published":           rowString(fr["Published"]),
				"disposition":         disposition,
				"raw_disposition":     rawDisposition,
				"disposition_expired": dispositionExpired,
				"disposition_note":    dispositionNote,
			})
		}
		lastScan = getAppSetting(db, cveLastScanSetting)
		return nil
	}(); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "findings": findings, "last_scan": lastScan})
}

// apiCveSetDisposition sets the disposition and optional note for a CVE finding.
func apiCveSetDisposition(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	osvId := r.PathValue("osv_id")
	payload, _ := readJsonBody(r) // force=True, silent=True → {} on bad input
	pkg := strings.TrimSpace(rowString(payload["package"]))
	ecosystem := strings.TrimSpace(rowString(payload["ecosystem"]))
	version := strings.TrimSpace(rowString(payload["version"]))
	disposition := strings.ToLower(strings.TrimSpace(rowString(payload["disposition"])))
	note := strings.TrimSpace(rowString(payload["note"]))

	if strings.TrimSpace(osvId) == "" || pkg == "" || ecosystem == "" || version == "" {
		jsonResponse(w, http.StatusBadRequest,
			map[string]any{"ok": false, "error": "osv_id, package, ecosystem, and version are required"})
		return
	}
	if !cveDispositionValues[disposition] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"ok":      false,
			"error":   fmt.Sprintf("invalid disposition: %s", disposition),
			"allowed": sortedStringSet(cveDispositionValues),
		})
		return
	}

	existingRes, err := db.Execute(
		"SELECT CreatedAt, Version_ FROM sobs_cve_dispositions FINAL "+
			"WHERE OsvId=? AND Package=? AND Ecosystem=? AND Version=? LIMIT 1",
		osvId, pkg, ecosystem, version,
	)
	if err != nil {
		logger.Error("api_cve_set_disposition lookup failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	existing := existingRes.Fetchone()
	nowTs := nowIso()
	currentVersion := time.Now().UnixMilli()
	createdAt := nowTs
	versionValue := currentVersion
	if existing != nil {
		createdAt = rowString(existing["CreatedAt"])
		if candidate := int64(coerceInt(existing["Version_"])) + 1; candidate > versionValue {
			versionValue = candidate
		}
	}
	row := Row{
		"OsvId":       osvId,
		"Package":     pkg,
		"Ecosystem":   ecosystem,
		"Version":     version,
		"Disposition": disposition,
		"Note":        note,
		"CreatedAt":   createdAt,
		"UpdatedAt":   nowTs,
		"Version_":    versionValue,
	}
	if _, err := insertRowsJsonEachRow(db, "sobs_cve_dispositions", []Row{row}); err != nil {
		logger.Error("api_cve_set_disposition insert failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":          true,
		"osv_id":      osvId,
		"package":     pkg,
		"ecosystem":   ecosystem,
		"version":     version,
		"disposition": disposition,
		"note":        note,
		"updated_at":  nowTs,
	})
}

// apiCveScan triggers an immediate CVE scan (normally scheduled every 24 hours).
//
// Scans release metadata and OTEL telemetry for library versions, then queries
// OSV.dev (Apache 2.0) for known CVEs. Stores results in sobs_cve_findings.
func apiCveScan(w http.ResponseWriter, r *http.Request) {
	summary := runCveScan(nil)
	jsonResponse(w, http.StatusOK, summary)
}

// ---------------------------------------------------------------------------
// Web UI – Work Items (Auto-Created GitHub Issues)
// ---------------------------------------------------------------------------

// anyToStringList converts cached []any / []string values back to []string.
func anyToStringList(value any) []string {
	switch v := value.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, rowString(item))
		}
		return out
	}
	return nil
}

// viewWorkItems displays work items created by agent rules.
func viewWorkItems(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	query := r.URL.Query()

	// Filters
	serviceFilter := strings.TrimSpace(query.Get("service"))
	ruleFilter := strings.TrimSpace(query.Get("rule_name"))
	actionTypeFilter := strings.TrimSpace(query.Get("action_type"))
	statusFilter := strings.TrimSpace(query.Get("status"))
	fromTs, toTs, timeError := parseTimeWindowArgs(r)

	// Build query
	conditions := []string{"IsDeleted = 0"}
	params := []any{}

	if serviceFilter != "" {
		conditions = append(conditions, "ServiceName = ?")
		params = append(params, serviceFilter)
	}
	if ruleFilter != "" {
		conditions = append(conditions, "AgentRuleName = ?")
		params = append(params, ruleFilter)
	}
	if actionTypeFilter != "" {
		conditions = append(conditions, "AgentAction = ?")
		params = append(params, actionTypeFilter)
	}
	if statusFilter != "" {
		conditions = append(conditions, "IssueState = ?")
		params = append(params, statusFilter)
	}
	if fromTs != "" {
		conditions = append(conditions, "CreatedAt >= ?")
		params = append(params, fromTs)
	}
	if toTs != "" {
		conditions = append(conditions, "CreatedAt <= ?")
		params = append(params, toTs)
	}

	whereClauseStr := "WHERE " + strings.Join(conditions, " AND ")

	// Query work items
	items := []map[string]any{}
	totalItems := 0
	services := map[string]bool{}
	rules := map[string]bool{}
	limit := parseLimit(r, 100)
	offset := parseOffset(r)
	// PORT-NOTE: Python uses a tuple cache key; the Go port serializes the same
	// fields into a quoted string key.
	cacheKey := fmt.Sprintf("%q|%q|%q|%q|%q|%q|%d|%d",
		serviceFilter, ruleFilter, actionTypeFilter, statusFilter, fromTs, toTs, limit, offset)
	now := float64(time.Now().UnixNano()) / 1e9

	if err := func() error {
		settings := loadAllAiSettings(db)
		// Backfill may call multiple GitHub APIs; run it in the background so
		// page rendering is not blocked on network latency.
		go maybeBackfillGithubWorkItemLinks(db, settings)

		pageCacheHit := false
		workItemsCacheLock.Lock()
		if cachedPage, ok := workItemsPageCache[cacheKey]; ok && cachedPage != nil {
			expiresAt, _ := coerceFloat(cachedPage["expires_at"])
			if expiresAt > now {
				totalItems = coerceInt(cachedPage["total_items"])
				if cachedItems, ok := cachedPage["items"].([]map[string]any); ok {
					items = append([]map[string]any{}, cachedItems...)
				}
				pageCacheHit = true
			}
		}
		workItemsCacheLock.Unlock()

		if !pageCacheHit {
			countRes, err := db.Execute(
				fmt.Sprintf("SELECT count() AS c FROM sobs_github_work_items FINAL %s", whereClauseStr), params...)
			if err != nil {
				return err
			}
			if countRow := countRes.Fetchone(); countRow != nil {
				totalItems = coerceInt(countRow["c"])
			}

			rowsRes, err := db.Execute(
				fmt.Sprintf("SELECT * FROM sobs_github_work_items FINAL %s ", whereClauseStr)+
					fmt.Sprintf("ORDER BY CreatedAt DESC LIMIT %d OFFSET %d", limit, offset),
				params...,
			)
			if err != nil {
				return err
			}
			items = []map[string]any{}
			for _, row := range rowsRes.Fetchall() {
				items = append(items, serializeGithubWorkItemRow(row))
			}
			workItemsCacheLock.Lock()
			workItemsPageCache[cacheKey] = map[string]any{
				"total_items": totalItems,
				"items":       items,
				"expires_at":  now + float64(max(1, workItemsPageCacheTtlSec)),
			}
			workItemsCacheLock.Unlock()
		}

		filterCacheHit := false
		workItemsCacheLock.Lock()
		if expiresAt, _ := coerceFloat(workItemsFilterCache["expires_at"]); expiresAt > now {
			for _, svc := range anyToStringList(workItemsFilterCache["services"]) {
				services[svc] = true
			}
			for _, rule := range anyToStringList(workItemsFilterCache["rules"]) {
				rules[rule] = true
			}
			filterCacheHit = true
		}
		workItemsCacheLock.Unlock()

		if !filterCacheHit {
			allServicesRes, err := db.Execute(
				"SELECT DISTINCT ServiceName FROM sobs_github_work_items FINAL " +
					"WHERE IsDeleted=0 ORDER BY ServiceName",
			)
			if err != nil {
				return err
			}
			for _, row := range allServicesRes.Fetchall() {
				if pyTruthy(row["ServiceName"]) {
					services[rowString(row["ServiceName"])] = true
				}
			}

			allRulesRes, err := db.Execute(
				"SELECT DISTINCT AgentRuleName FROM sobs_github_work_items FINAL " +
					"WHERE IsDeleted=0 ORDER BY AgentRuleName",
			)
			if err != nil {
				return err
			}
			for _, row := range allRulesRes.Fetchall() {
				if pyTruthy(row["AgentRuleName"]) {
					rules[rowString(row["AgentRuleName"])] = true
				}
			}
			workItemsCacheLock.Lock()
			workItemsFilterCache["services"] = sortedStringSet(services)
			workItemsFilterCache["rules"] = sortedStringSet(rules)
			workItemsFilterCache["expires_at"] = now + float64(max(1, workItemsFilterCacheTtlSec))
			workItemsCacheLock.Unlock()
		}
		return nil
	}(); err != nil {
		logger.Warn(fmt.Sprintf("Error loading work items: %v", err))
	}

	renderTemplate(w, r, "work_items.html", map[string]any{
		"items":              items,
		"total_items":        totalItems,
		"services":           sortedStringSet(services),
		"rules":              sortedStringSet(rules),
		"service_filter":     serviceFilter,
		"rule_filter":        ruleFilter,
		"action_type_filter": actionTypeFilter,
		"status_filter":      statusFilter,
		"from_ts":            fromTs,
		"to_ts":              toTs,
		"time_error":         timeError,
	})
}

// apiGetWorkItems returns work items filtered by optional criteria.
func apiGetWorkItems(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	query := r.URL.Query()

	// Parse filters
	anomalyRuleId := strings.TrimSpace(query.Get("anomaly_rule_id"))
	serviceName := strings.TrimSpace(query.Get("service"))
	agentRuleId := strings.TrimSpace(query.Get("rule_id"))
	signalSource := strings.TrimSpace(query.Get("signal_source"))
	signalName := strings.TrimSpace(query.Get("signal_name"))
	limit := parseLimit(r, 100)

	conditions := []string{"IsDeleted = 0"}
	params := []any{}

	if anomalyRuleId != "" {
		conditions = append(conditions, "AnomalyRuleId = ?")
		params = append(params, anomalyRuleId)
	}
	if serviceName != "" {
		conditions = append(conditions, "ServiceName = ?")
		params = append(params, serviceName)
	}
	if agentRuleId != "" {
		conditions = append(conditions, "AgentRuleId = ?")
		params = append(params, agentRuleId)
	}
	if signalSource != "" {
		conditions = append(conditions, "SignalSource = ?")
		params = append(params, signalSource)
	}
	if signalName != "" {
		conditions = append(conditions, "SignalName = ?")
		params = append(params, signalName)
	}

	whereClauseStr := strings.Join(conditions, " AND ")

	settings := loadAllAiSettings(db)
	maybeBackfillGithubWorkItemLinks(db, settings)

	rowsRes, err := db.Execute(
		"SELECT * FROM sobs_github_work_items FINAL "+
			fmt.Sprintf("WHERE %s ", whereClauseStr)+
			"ORDER BY CreatedAt DESC "+
			fmt.Sprintf("LIMIT %d", limit),
		params...,
	)
	if err != nil {
		logger.Warn(fmt.Sprintf("Error fetching work items: %v", err))
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	items := []map[string]any{}
	for _, row := range rowsRes.Fetchall() {
		items = append(items, serializeGithubWorkItemRow(row))
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "items": items})
}

// ---------------------------------------------------------------------------
// Web UI – AI Transparency
// ---------------------------------------------------------------------------

// getAiFilterMetadata mirrors _get_ai_filter_metadata.
func getAiFilterMetadata(db *ChDbConnection, fromTs, toTs string) map[string]any {
	cacheKey := [2]string{fromTs, toTs}
	now := monotonicSeconds()
	aiFilterMetadataCacheLock.Lock()
	if cached, ok := aiFilterMetadataCache[cacheKey]; ok && cached != nil {
		expiresAt, _ := coerceFloat(cached["expires_at"])
		if now < expiresAt {
			result := map[string]any{
				"services":   append([]string{}, anyToStringList(cached["services"])...),
				"models":     append([]string{}, anyToStringList(cached["models"])...),
				"operations": append([]string{}, anyToStringList(cached["operations"])...),
				"span_names": append([]string{}, anyToStringList(cached["span_names"])...),
				"errors":     append([]string{}, anyToStringList(cached["errors"])...),
			}
			aiFilterMetadataCacheLock.Unlock()
			return result
		}
	}
	aiFilterMetadataCacheLock.Unlock()

	metadataErrors := []string{}
	services := []string{}
	models := []string{}
	operations := []string{}
	spanNames := []string{}

	metadataTimeConditions, metadataTimeParams := timeWindowConditions("Timestamp", fromTs, toTs)
	metadataBaseConditions := []string{aiSpanCondition}
	metadataBaseConditions = append(metadataBaseConditions, metadataTimeConditions...)
	metadataBaseWhere := strings.Join(metadataBaseConditions, " AND ")
	metadataSourceSql := "SELECT Timestamp, ServiceName, SpanName, " +
		"SpanAttributes['gen_ai.request.model'] AS RequestModel, " +
		"SpanAttributes['gen_ai.operation.name'] AS OperationName " +
		"FROM otel_traces " +
		fmt.Sprintf("WHERE %s ", metadataBaseWhere) +
		"ORDER BY Timestamp DESC LIMIT ?"
	metadataSourceParams := append(append([]any{}, metadataTimeParams...), aiFilterMetadataSampleRows)

	fetchDistinctAiMetadataValues := func(selectExpr, extraWhere string) ([]string, error) {
		whereSuffix := ""
		if extraWhere != "" {
			whereSuffix = fmt.Sprintf("WHERE %s", extraWhere)
		}
		res, err := db.Execute(
			fmt.Sprintf("SELECT DISTINCT %s AS v ", selectExpr)+
				fmt.Sprintf("FROM (%s) recent_ai %s", metadataSourceSql, whereSuffix),
			metadataSourceParams...,
		)
		if err != nil {
			return nil, err
		}
		valueSet := map[string]bool{}
		for _, row := range res.Fetchall() {
			v := rowString(row[res.Cols[0]])
			if strings.TrimSpace(v) != "" {
				valueSet[v] = true
			}
		}
		return sortedStringSet(valueSet), nil
	}

	var err error
	if services, err = fetchDistinctAiMetadataValues("ServiceName", "ServiceName != ''"); err != nil {
		services = []string{}
		metadataErrors = append(metadataErrors, fmt.Sprintf("services=%s", publicDashboardQueryError(err)))
	}
	if models, err = fetchDistinctAiMetadataValues("RequestModel", "RequestModel != ''"); err != nil {
		models = []string{}
		metadataErrors = append(metadataErrors, fmt.Sprintf("models=%s", publicDashboardQueryError(err)))
	}
	if operations, err = fetchDistinctAiMetadataValues("OperationName", "OperationName != ''"); err != nil {
		operations = []string{}
		metadataErrors = append(metadataErrors, fmt.Sprintf("operations=%s", publicDashboardQueryError(err)))
	}
	if spanNames, err = fetchDistinctAiMetadataValues("SpanName", "SpanName != ''"); err != nil {
		spanNames = []string{}
		metadataErrors = append(metadataErrors, fmt.Sprintf("span_names=%s", publicDashboardQueryError(err)))
	}

	result := map[string]any{
		"services":   services,
		"models":     models,
		"operations": operations,
		"span_names": spanNames,
		"errors":     metadataErrors,
	}
	aiFilterMetadataCacheLock.Lock()
	// Keep cache bounded to avoid unbounded growth for many time-window combinations.
	if len(aiFilterMetadataCache) > 16 {
		clear(aiFilterMetadataCache)
	}
	aiFilterMetadataCache[cacheKey] = map[string]any{
		"services":   services,
		"models":     models,
		"operations": operations,
		"span_names": spanNames,
		"errors":     metadataErrors,
		"expires_at": now + float64(max(1, aiFilterMetadataCacheTtlSec)),
	}
	aiFilterMetadataCacheLock.Unlock()
	return result
}

func viewAi(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	query := r.URL.Query()
	getList := func(key string) []string {
		out := []string{}
		for _, v := range query[key] {
			if t := strings.TrimSpace(v); t != "" {
				out = append(out, t)
			}
		}
		return out
	}
	selectedServices := getList("service")
	selectedModels := getList("model")
	selectedOperations := getList("operation")
	selectedSpanNames := getList("span_name")
	selectedRowTypes := []string{}
	for _, rt := range query["row_type"] {
		if t := strings.ToLower(strings.TrimSpace(rt)); t == "llm" || t == "system" {
			selectedRowTypes = append(selectedRowTypes, t)
		}
	}

	first := func(values []string) string {
		if len(values) > 0 {
			return values[0]
		}
		return ""
	}
	service := first(selectedServices)
	model := first(selectedModels)
	operationFilter := first(selectedOperations)
	spanName := first(selectedSpanNames)
	rowType := first(selectedRowTypes)
	sqlWhere := strings.TrimSpace(query.Get("sql"))
	fromTs, toTs, timeError := parseTimeWindowArgs(r)
	viewMode := strings.ToLower(strings.TrimSpace(query.Get("view")))
	if viewMode != "flat" && viewMode != "trace" {
		viewMode = "flat"
	}
	limit := parseLimit(r, 50)
	offset := parseOffset(r)
	sortBy, sortCol, sortDir := parseSort(r,
		map[string]string{"Timestamp": "Timestamp", "Duration": "Duration", "ServiceName": "ServiceName"},
		"Timestamp",
	)
	orderDir := "DESC"
	if sortDir == "asc" {
		orderDir = "ASC"
	}
	orderClause := fmt.Sprintf("ORDER BY %s %s", sortCol, orderDir)

	conditions := []string{}
	params := []any{}
	errorMsg := timeError
	baseAiCondition := aiSpanCondition
	timeConditions, timeParams := timeWindowConditions("Timestamp", fromTs, toTs)
	where := "WHERE " + baseAiCondition
	if sqlWhere != "" && errorMsg == "" {
		safeSql, err := normalizeAiSqlWhere(sqlWhere)
		if err != nil {
			errorMsg = fmt.Sprintf("SQL error: %s", publicDashboardQueryError(err))
			where = "WHERE " + baseAiCondition
		} else {
			sqlConditions := []string{fmt.Sprintf("(%s)", safeSql), baseAiCondition}
			sqlConditions = append(sqlConditions, timeConditions...)
			where = "WHERE " + strings.Join(sqlConditions, " AND ")
			params = append([]any{}, timeParams...)
		}
	} else if errorMsg == "" {
		if len(selectedServices) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selectedServices)), ",")
			conditions = append(conditions, fmt.Sprintf("ServiceName IN (%s)", placeholders))
			for _, v := range selectedServices {
				params = append(params, v)
			}
		}
		if len(selectedModels) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selectedModels)), ",")
			conditions = append(conditions, fmt.Sprintf("SpanAttributes['gen_ai.request.model'] IN (%s)", placeholders))
			for _, v := range selectedModels {
				params = append(params, v)
			}
		}
		if len(selectedOperations) > 0 {
			operationConditions := []string{}
			for _, selectedOperation := range selectedOperations {
				if strings.ToLower(selectedOperation) == "chat" {
					operationConditions = append(operationConditions,
						"(SpanAttributes['gen_ai.operation.name']=? OR SpanAttributes['gen_ai.operation.name']='')")
					params = append(params, "chat")
				} else {
					operationConditions = append(operationConditions, "SpanAttributes['gen_ai.operation.name']=?")
					params = append(params, selectedOperation)
				}
			}
			if len(operationConditions) > 0 {
				conditions = append(conditions, "("+strings.Join(operationConditions, " OR ")+")")
			}
		}
		if len(selectedSpanNames) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(selectedSpanNames)), ",")
			conditions = append(conditions, fmt.Sprintf("SpanName IN (%s)", placeholders))
			for _, v := range selectedSpanNames {
				params = append(params, v)
			}
		}

		selectedRowTypeSet := map[string]bool{}
		for _, rt := range selectedRowTypes {
			selectedRowTypeSet[rt] = true
		}
		if len(selectedRowTypeSet) == 1 && selectedRowTypeSet["llm"] {
			conditions = append(conditions, "SpanAttributes['gen_ai.request.model'] != ''")
		} else if len(selectedRowTypeSet) == 1 && selectedRowTypeSet["system"] {
			conditions = append(conditions, "SpanAttributes['gen_ai.request.model'] = ''")
		}
		conditions = append(conditions, baseAiCondition)
		conditions = append(conditions, timeConditions...)
		params = append(params, timeParams...)
		where = whereClause(conditions)
	}

	traceIds := []string{}
	total := 0
	rows := []Row{}
	if errorMsg == "" {
		if queryErr := func() error {
			if viewMode == "trace" {
				traceConditions := append([]string{}, conditions...)
				traceWhere := ""
				if sqlWhere != "" {
					traceWhere = fmt.Sprintf("%s AND TraceId != ''", where)
				} else {
					traceConditions = append(traceConditions, "TraceId != ''")
					traceWhere = "WHERE " + strings.Join(traceConditions, " AND ")
				}
				totalRes, err := db.Execute(fmt.Sprintf("SELECT uniq(TraceId) FROM otel_traces %s", traceWhere), params...)
				if err != nil {
					return err
				}
				total = firstScalarInt(totalRes)
				traceRes, err := db.Execute(
					"SELECT TraceId, MAX(Timestamp) AS LastTs FROM otel_traces "+
						fmt.Sprintf("%s GROUP BY TraceId ", traceWhere)+
						fmt.Sprintf("ORDER BY LastTs %s LIMIT ? OFFSET ?", orderDir),
					append(append([]any{}, params...), limit, offset)...,
				)
				if err != nil {
					return err
				}
				for _, tr := range traceRes.Fetchall() {
					if tid := rowString(tr["TraceId"]); tid != "" {
						traceIds = append(traceIds, tid)
					}
				}
				if len(traceIds) > 0 {
					placeholders := strings.TrimSuffix(strings.Repeat("?,", len(traceIds)), ",")
					detailWhere := fmt.Sprintf("%s AND TraceId IN (%s)", traceWhere, placeholders)
					detailParams := append([]any{}, params...)
					for _, tid := range traceIds {
						detailParams = append(detailParams, tid)
					}
					detailRes, err := db.Execute(
						"SELECT Timestamp, ServiceName, TraceId, SpanName, Duration, SpanAttributes "+
							fmt.Sprintf("FROM otel_traces %s ", detailWhere)+
							"ORDER BY Timestamp ASC",
						detailParams...,
					)
					if err != nil {
						return err
					}
					rows = detailRes.Fetchall()
				}
			} else {
				totalRes, err := db.Execute(fmt.Sprintf("SELECT COUNT(*) FROM otel_traces %s", where), params...)
				if err != nil {
					return err
				}
				total = firstScalarInt(totalRes)
				rowsRes, err := db.Execute(
					"SELECT Timestamp, ServiceName, TraceId, SpanName, Duration, SpanAttributes "+
						fmt.Sprintf("FROM otel_traces %s %s LIMIT ? OFFSET ?", where, orderClause),
					append(append([]any{}, params...), limit, offset)...,
				)
				if err != nil {
					return err
				}
				rows = rowsRes.Fetchall()
			}
			return nil
		}(); queryErr != nil {
			errorMsg = fmt.Sprintf("SQL error: %s", publicDashboardQueryError(queryErr))
			total = 0
			rows = []Row{}
			traceIds = []string{}
		}
	}

	safeAttrInt := func(attrs map[string]any, key string) int {
		rawValue, ok := attrs[key]
		if !ok {
			rawValue = "0"
		}
		text := rowString(rawValue)
		if text == "" {
			text = "0"
		}
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0
		}
		if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0
		}
		return int(parsed)
	}

	safeDurationMs := func(durationNs any) float64 {
		text := rowString(durationNs)
		if text == "" {
			text = "0"
		}
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0.0
		}
		if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0.0
		}
		return math.Round(parsed/1_000_000*10) / 10
	}

	aiItems := []map[string]any{}
	for _, rowItem := range rows {
		attrs := mapToDict(rowItem["SpanAttributes"])
		ts := rowString(rowItem["Timestamp"])
		provider := rowString(attrs["gen_ai.provider.name"])
		if provider == "" {
			provider = rowString(attrs["gen_ai.system"])
		}
		reqModel := rowString(attrs["gen_ai.request.model"])
		operation := rowString(attrs["gen_ai.operation.name"])
		if operation == "" {
			operation = "chat"
		}
		inputMessagesRaw := rowString(attrs["gen_ai.input.messages"])
		outputMessagesRaw := rowString(attrs["gen_ai.output.messages"])
		systemInstructionsRaw := rowString(attrs["gen_ai.system_instructions"])
		prompt := extractMessagesText(inputMessagesRaw)
		if prompt == "" {
			prompt = rowString(attrs["sobs.gen_ai.prompt"])
		}
		response := extractMessagesText(outputMessagesRaw)
		if response == "" {
			response = rowString(attrs["sobs.gen_ai.response"])
		}
		tokensIn := safeAttrInt(attrs, "gen_ai.usage.input_tokens")
		tokensOut := safeAttrInt(attrs, "gen_ai.usage.output_tokens")
		errType := rowString(attrs["error.type"])
		msg := rowString(attrs["exception.message"])
		durationMs := safeDurationMs(rowItem["Duration"])
		tokensPerSec := 0.0
		if durationMs > 0 && tokensOut > 0 {
			tokensPerSec = math.Round(float64(tokensOut)/(durationMs/1000)*10) / 10
		}
		finishReason := rowString(attrs["gen_ai.response.finish_reason"])
		itemSpanName := rowString(rowItem["SpanName"])
		temperature := rowString(attrs["gen_ai.request.temperature"])
		maxTokens := rowString(attrs["gen_ai.request.max_tokens"])
		thinkingTokens := safeAttrInt(attrs, "gen_ai.usage.thinking_tokens")
		eventName := rowString(attrs["sobs.ai.event"])
		if eventName == "" && strings.HasPrefix(itemSpanName, "ai.") {
			eventName = itemSpanName[3:]
		}
		turnID := rowString(attrs["gen_ai.turn_id"])
		if turnID == "" {
			turnID = rowString(attrs["gen_ai.response.id"])
		}
		parsedInput, _ := parseGenaiMessagesJson(inputMessagesRaw)
		parsedOutput, _ := parseGenaiMessagesJson(outputMessagesRaw)
		inputMessages := normalizeGenaiMessagesForDisplay(parsedInput)
		outputMessages := normalizeGenaiMessagesForDisplay(parsedOutput)
		inputMessages, dedupedSystemMessageCount := dedupeSystemInputMessages(inputMessages, systemInstructionsRaw)
		rowID := errorId(ts, rowString(rowItem["ServiceName"]), provider, reqModel+errType+msg, rowString(rowItem["TraceId"]), "")
		isLlmCall := reqModel != "" && (tokensIn > 0 || tokensOut > 0 || response != "" ||
			len(inputMessages) > 0 || len(outputMessages) > 0 || strings.TrimSpace(systemInstructionsRaw) != "")
		aiItems = append(aiItems, map[string]any{
			"id":                           rowID,
			"ts":                           ts,
			"service":                      rowItem["ServiceName"],
			"provider":                     provider,
			"model":                        reqModel,
			"operation":                    operation,
			"span_name":                    itemSpanName,
			"is_llm_call":                  isLlmCall,
			"prompt":                       prompt,
			"response":                     response,
			"input_messages":               inputMessages,
			"output_messages":              outputMessages,
			"input_messages_json":          inputMessagesRaw,
			"output_messages_json":         outputMessagesRaw,
			"system_instructions":          systemInstructionsRaw,
			"system_message_deduped_count": dedupedSystemMessageCount,
			"tokens_in":                    tokensIn,
			"tokens_out":                   tokensOut,
			"thinking_tokens":              thinkingTokens,
			"duration_ms":                  durationMs,
			"tokens_per_sec":               tokensPerSec,
			"trace_id":                     rowItem["TraceId"],
			"chat_id":                      rowString(attrs["gen_ai.chat_id"]),
			"turn_id":                      turnID,
			"event_name":                   eventName,
			"input_question":               rowString(attrs["gen_ai.input.question"]),
			"turn_summary_request":         rowString(attrs["gen_ai.turn.summary.request"]),
			"turn_summary_action":          rowString(attrs["gen_ai.turn.summary.action"]),
			"turn_summary_result":          rowString(attrs["gen_ai.turn.summary.result"]),
			"guard_allowed":                attrs["gen_ai.guard.allowed"],
			"guard_reason":                 rowString(attrs["gen_ai.guard.reason"]),
			"tool_name":                    rowString(attrs["gen_ai.tool.name"]),
			"tool_status":                  rowString(attrs["sobs.ai.action.status"]),
			"tool_summary":                 rowString(attrs["sobs.ai.tool.summary"]),
			"tool_action":                  rowString(attrs["sobs.ai.tool.action"]),
			"tool_action_id":               rowString(attrs["sobs.ai.action_id"]),
			"error_type":                   errType,
			"error_message":                msg,
			"finish_reason":                finishReason,
			"temperature":                  temperature,
			"max_tokens":                   maxTokens,
		})
	}

	traceGroups := []map[string]any{}
	if viewMode == "trace" {
		byTrace := map[string]map[string]any{}
		for _, tid := range traceIds {
			byTrace[tid] = map[string]any{
				"id":         errorId("", "", "trace", tid, tid, ""),
				"trace_id":   tid,
				"spans":      []map[string]any{},
				"calls":      0,
				"tokens_in":  0,
				"tokens_out": 0,
				"errors":     0,
				"services":   map[string]bool{},
				"models":     map[string]bool{},
				"operations": map[string]bool{},
				"first_ts":   "",
				"last_ts":    "",
			}
		}
		for _, item := range aiItems {
			tid := rowString(item["trace_id"])
			grp, ok := byTrace[tid]
			if tid == "" || !ok {
				continue
			}
			grp["spans"] = append(grp["spans"].([]map[string]any), item)
			grp["calls"] = grp["calls"].(int) + 1
			grp["tokens_in"] = grp["tokens_in"].(int) + coerceInt(item["tokens_in"])
			grp["tokens_out"] = grp["tokens_out"].(int) + coerceInt(item["tokens_out"])
			if rowString(item["error_type"]) != "" {
				grp["errors"] = grp["errors"].(int) + 1
			}
			if svc := rowString(item["service"]); svc != "" {
				grp["services"].(map[string]bool)[svc] = true
			}
			if mdl := rowString(item["model"]); mdl != "" {
				grp["models"].(map[string]bool)[mdl] = true
			}
			if op := rowString(item["operation"]); op != "" {
				grp["operations"].(map[string]bool)[op] = true
			}
			itemTs := rowString(item["ts"])
			if itemTs != "" {
				if grp["first_ts"].(string) == "" || itemTs < grp["first_ts"].(string) {
					grp["first_ts"] = itemTs
				}
				if grp["last_ts"].(string) == "" || itemTs > grp["last_ts"].(string) {
					grp["last_ts"] = itemTs
				}
			}
		}
		for _, tid := range traceIds {
			grp := byTrace[tid]
			spans := grp["spans"].([]map[string]any)
			if len(spans) == 0 {
				continue
			}
			grp["services"] = sortedStringSet(grp["services"].(map[string]bool))
			grp["models"] = sortedStringSet(grp["models"].(map[string]bool))
			grp["operations"] = sortedStringSet(grp["operations"].(map[string]bool))
			grp["turn_cards"] = buildAiTraceTurnCards(spans)
			traceGroups = append(traceGroups, grp)
		}
	}

	metadata := getAiFilterMetadata(db, fromTs, toTs)
	services := anyToStringList(metadata["services"])
	models := anyToStringList(metadata["models"])
	operations := anyToStringList(metadata["operations"])
	spanNames := anyToStringList(metadata["span_names"])
	metadataErrors := anyToStringList(metadata["errors"])

	totals := map[string]int{"ti": 0, "to_": 0, "cnt": 0, "errors": 0}
	totalsWhere := where
	if totalsWhere == "" {
		totalsWhere = "WHERE " + aiSpanCondition
	}
	totalsParams := []any{}
	if where != "" {
		totalsParams = append(totalsParams, params...)
	}
	totalsRes, totalsErr := db.Execute(
		"SELECT "+
			"SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.input_tokens'])) ti, "+
			"SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.output_tokens'])) to_, "+
			"COUNT(*) cnt, "+
			"countIf(SpanAttributes['error.type'] != '') errors "+
			"FROM otel_traces "+
			totalsWhere,
		totalsParams...,
	)
	if totalsErr != nil {
		metadataErrors = append(metadataErrors, fmt.Sprintf("totals=%s", publicDashboardQueryError(totalsErr)))
	} else if totalsRow := totalsRes.Fetchone(); totalsRow != nil {
		totals = map[string]int{
			"ti":     coerceInt(totalsRow["ti"]),
			"to_":    coerceInt(totalsRow["to_"]),
			"cnt":    coerceInt(totalsRow["cnt"]),
			"errors": coerceInt(totalsRow["errors"]),
		}
	}

	if len(metadataErrors) > 0 {
		shown := metadataErrors
		if len(shown) > 3 {
			shown = shown[:3]
		}
		metadataErrorText := "Some AI metadata failed to load: " + strings.Join(shown, "; ")
		if errorMsg != "" {
			errorMsg = fmt.Sprintf("%s; %s", errorMsg, metadataErrorText)
		} else {
			errorMsg = metadataErrorText
		}
	}

	aiPricing, aiPricingSources := loadAiPricingWithSources(db)

	renderTemplate(w, r, "ai.html", map[string]any{
		"ai_items":                aiItems,
		"total":                   total,
		"limit":                   limit,
		"offset":                  offset,
		"service":                 service,
		"selected_services":       selectedServices,
		"model":                   model,
		"selected_models":         selectedModels,
		"operation":               operationFilter,
		"selected_operations":     selectedOperations,
		"span_name":               spanName,
		"selected_span_names":     selectedSpanNames,
		"row_type":                rowType,
		"selected_row_types":      selectedRowTypes,
		"sql_where":               sqlWhere,
		"view_mode":               viewMode,
		"services":                services,
		"models":                  models,
		"operations":              operations,
		"span_names":              spanNames,
		"trace_groups":            traceGroups,
		"total_tokens_in":         totals["ti"],
		"total_tokens_out":        totals["to_"],
		"total_calls":             totals["cnt"],
		"total_errors":            totals["errors"],
		"error_msg":               errorMsg,
		"sort_by":                 sortBy,
		"sort_dir":                sortDir,
		"from_ts":                 fromTs,
		"to_ts":                   toTs,
		"ai_pricing_json":         aiPricing,
		"ai_pricing_sources_json": aiPricingSources,
	})
}

// ---------------------------------------------------------------------------
// AI span attributes API  GET /api/ai/span-attributes
// ---------------------------------------------------------------------------
func getAiSpanAttributes(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	q := r.URL.Query()
	ts := strings.TrimSpace(q.Get("ts"))
	service := strings.TrimSpace(q.Get("service"))
	traceID := strings.TrimSpace(q.Get("trace_id"))
	spanName := strings.TrimSpace(q.Get("span_name"))

	if ts == "" || service == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Missing required params: ts and service"})
		return
	}

	conditions := []string{aiSpanCondition, "Timestamp=?", "ServiceName=?"}
	params := []any{ts, service}
	if traceID != "" {
		conditions = append(conditions, "TraceId=?")
		params = append(params, traceID)
	}
	if spanName != "" {
		conditions = append(conditions, "SpanName=?")
		params = append(params, spanName)
	}

	res, err := db.Execute(
		"SELECT SpanAttributes FROM otel_traces "+
			fmt.Sprintf("WHERE %s ", strings.Join(conditions, " AND "))+
			"ORDER BY Timestamp DESC LIMIT 1",
		params...,
	)
	if err != nil {
		logger.Warn("Error fetching AI span attributes", "error", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Failed to load span attributes"})
		return
	}
	row := res.Fetchone()
	if row == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "Span not found"})
		return
	}
	attrs := mapToDict(row[res.Cols[0]])
	rawAttrs, err := json.MarshalIndent(attrs, "", "  ")
	if err != nil {
		logger.Warn("Error fetching AI span attributes", "error", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Failed to load span attributes"})
		return
	}
	jsonifyWithOptionalSqlOutputMask(w, map[string]any{"ok": true, "raw_attrs": string(rawAttrs)})
}

// ---------------------------------------------------------------------------
// AI conversation tab  GET /api/ai/conversation
// ---------------------------------------------------------------------------
func getAiConversation(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	q := r.URL.Query()
	ts := strings.TrimSpace(q.Get("ts"))
	service := strings.TrimSpace(q.Get("service"))
	traceID := strings.TrimSpace(q.Get("trace_id"))
	spanName := strings.TrimSpace(q.Get("span_name"))
	fromTs := strings.TrimSpace(q.Get("from_ts"))
	toTs := strings.TrimSpace(q.Get("to_ts"))

	writeHtml := func(status int, body string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}

	if ts == "" || service == "" {
		writeHtml(http.StatusBadRequest, "<p class='text-danger small'>Missing required params: ts and service.</p>")
		return
	}

	conditions := []string{aiSpanCondition, "Timestamp=?", "ServiceName=?"}
	params := []any{ts, service}
	if traceID != "" {
		conditions = append(conditions, "TraceId=?")
		params = append(params, traceID)
	}
	if spanName != "" {
		conditions = append(conditions, "SpanName=?")
		params = append(params, spanName)
	}

	res, err := db.Execute(
		"SELECT SpanAttributes FROM otel_traces "+
			fmt.Sprintf("WHERE %s ", strings.Join(conditions, " AND "))+
			"ORDER BY Timestamp DESC LIMIT 1",
		params...,
	)
	if err != nil {
		logger.Warn("Error fetching AI conversation", "error", err)
		writeHtml(http.StatusInternalServerError, "<p class='text-danger small'>Error loading conversation.</p>")
		return
	}
	row := res.Fetchone()
	if row == nil {
		writeHtml(http.StatusNotFound, "<p class='text-danger small'>Span not found.</p>")
		return
	}
	attrs := mapToDict(row[res.Cols[0]])
	inputMessagesRaw := rowString(attrs["gen_ai.input.messages"])
	outputMessagesRaw := rowString(attrs["gen_ai.output.messages"])
	systemInstructionsRaw := rowString(attrs["gen_ai.system_instructions"])
	prompt := extractMessagesText(inputMessagesRaw)
	if prompt == "" {
		prompt = rowString(attrs["sobs.gen_ai.prompt"])
	}
	responseText := extractMessagesText(outputMessagesRaw)
	if responseText == "" {
		responseText = rowString(attrs["sobs.gen_ai.response"])
	}
	errType := rowString(attrs["error.type"])
	errMsg := rowString(attrs["exception.message"])
	finishReason := rowString(attrs["gen_ai.response.finish_reason"])
	operation := rowString(attrs["gen_ai.operation.name"])
	if operation == "" {
		operation = "chat"
	}
	parsedInput, _ := parseGenaiMessagesJson(inputMessagesRaw)
	parsedOutput, _ := parseGenaiMessagesJson(outputMessagesRaw)
	inputMessages := normalizeGenaiMessagesForDisplay(parsedInput)
	outputMessages := normalizeGenaiMessagesForDisplay(parsedOutput)
	inputMessages, dedupedCount := dedupeSystemInputMessages(inputMessages, systemInstructionsRaw)
	item := map[string]any{
		"service":                      service,
		"trace_id":                     traceID,
		"error_type":                   errType,
		"error_message":                errMsg,
		"system_instructions":          systemInstructionsRaw,
		"system_message_deduped_count": dedupedCount,
		"input_messages":               inputMessages,
		"output_messages":              outputMessages,
		"prompt":                       prompt,
		"response":                     responseText,
		"operation":                    operation,
		"finish_reason":                finishReason,
	}
	renderTemplate(w, r, "_ai_conversation_partial.html", map[string]any{
		"item":    item,
		"from_ts": fromTs,
		"to_ts":   toTs,
	})
}

// ---------------------------------------------------------------------------
// AI training data export  GET /api/ai/export
// ---------------------------------------------------------------------------
func exportAiTraining(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	q := r.URL.Query()
	service := strings.TrimSpace(q.Get("service"))
	model := strings.TrimSpace(q.Get("model"))
	operationFilter := strings.TrimSpace(q.Get("operation"))
	fromTs, toTs, _ := parseTimeWindowArgs(r)
	fmtParam := strings.ToLower(strings.TrimSpace(q.Get("format")))
	if fmtParam == "" {
		fmtParam = "jsonl"
	}
	maxRows := 1000
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			maxRows = parsed
		}
	}
	if maxRows < 1 {
		maxRows = 1
	}
	if maxRows > 5000 {
		maxRows = 5000
	}

	conditions := []string{aiSpanCondition}
	params := []any{}
	if service != "" {
		conditions = append(conditions, "ServiceName=?")
		params = append(params, service)
	}
	if model != "" {
		conditions = append(conditions, "SpanAttributes['gen_ai.request.model']=?")
		params = append(params, model)
	}
	if operationFilter != "" {
		if strings.ToLower(operationFilter) == "chat" {
			conditions = append(conditions, "(SpanAttributes['gen_ai.operation.name']=? OR SpanAttributes['gen_ai.operation.name']='')")
			params = append(params, "chat")
		} else {
			conditions = append(conditions, "SpanAttributes['gen_ai.operation.name']=?")
			params = append(params, operationFilter)
		}
	}
	timeConditions, timeParams := timeWindowConditions("Timestamp", fromTs, toTs)
	conditions = append(conditions, timeConditions...)
	params = append(params, timeParams...)
	where := "WHERE " + strings.Join(conditions, " AND ")

	res, err := db.Execute(
		"SELECT Timestamp, ServiceName, TraceId, Duration, SpanAttributes "+
			fmt.Sprintf("FROM otel_traces %s ORDER BY Timestamp DESC LIMIT ?", where),
		append(append([]any{}, params...), maxRows)...,
	)
	if err != nil {
		logger.Warn("Error exporting AI training data", "error", err)
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "Failed to export AI training data"})
		return
	}

	records := []map[string]any{}
	for _, row := range res.Fetchall() {
		attrs := mapToDict(row["SpanAttributes"])
		provider := rowString(attrs["gen_ai.provider.name"])
		if provider == "" {
			provider = rowString(attrs["gen_ai.system"])
		}
		reqModel := rowString(attrs["gen_ai.request.model"])
		inputMessagesRaw := rowString(attrs["gen_ai.input.messages"])
		outputMessagesRaw := rowString(attrs["gen_ai.output.messages"])
		prompt := extractMessagesText(inputMessagesRaw)
		if prompt == "" {
			prompt = rowString(attrs["sobs.gen_ai.prompt"])
		}
		response := extractMessagesText(outputMessagesRaw)
		if response == "" {
			response = rowString(attrs["sobs.gen_ai.response"])
		}
		tokensIn := coerceInt(attrs["gen_ai.usage.input_tokens"])
		tokensOut := coerceInt(attrs["gen_ai.usage.output_tokens"])

		messages := []any{}
		if inputMessagesRaw != "" {
			var parsed any
			if jsonErr := json.Unmarshal([]byte(inputMessagesRaw), &parsed); jsonErr == nil {
				if arr, ok := parsed.([]any); ok {
					messages = append(messages, arr...)
				}
			} else if prompt != "" {
				messages = append(messages, map[string]any{"role": "user", "content": prompt})
			}
		}
		if outputMessagesRaw != "" {
			var parsed any
			if jsonErr := json.Unmarshal([]byte(outputMessagesRaw), &parsed); jsonErr == nil {
				if arr, ok := parsed.([]any); ok {
					messages = append(messages, arr...)
				}
			} else if response != "" {
				messages = append(messages, map[string]any{"role": "assistant", "content": response})
			}
		}

		durationMs := 0.0
		if d, ok := coerceFloat(row["Duration"]); ok {
			durationMs = math.Round(d/1_000_000*10) / 10
		}
		records = append(records, map[string]any{
			"messages": messages,
			"metadata": map[string]any{
				"timestamp":   rowString(row["Timestamp"]),
				"service":     row["ServiceName"],
				"provider":    provider,
				"model":       reqModel,
				"tokens_in":   tokensIn,
				"tokens_out":  tokensOut,
				"duration_ms": durationMs,
				"trace_id":    row["TraceId"],
			},
		})
	}

	var body string
	var mime, filename string
	if fmtParam == "json" {
		encoded, _ := json.MarshalIndent(records, "", "  ")
		body = string(encoded)
		mime = "application/json"
		filename = "ai_training_data.json"
	} else {
		lines := make([]string, 0, len(records))
		for _, rec := range records {
			encoded, _ := json.Marshal(rec)
			lines = append(lines, string(encoded))
		}
		body = strings.Join(lines, "\n")
		mime = "application/x-ndjson"
		filename = "ai_training_data.jsonl"
	}

	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}
