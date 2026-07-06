package main

// Port of app.py lines 15680-17302:
//   - Raw span payload API (GET /api/traces/span/<span_id>)
//   - Incident view (GET /incident)
//   - Enrichment – geo-lookup helpers (geoip2fast)
//   - Enrichment – CVE scanner helpers (OSV.dev, GitHub dependency backfill)
//   - Background loops: CVE scanner, GitHub repo health
//
// Shared enrichment globals (_GEO_DB, _GEO_CACHE, _CVE_* / _GITHUB_* settings
// and tunables, app.py lines 24538-24574) belong to another section and are
// referenced here via the deterministic naming rule.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

func init() {
	registerRoute("GET", "/api/traces/span/{span_id}", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(apiRawSpan)(w, r)
	})
	registerRoute("GET", "/incident", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(viewIncident)(w, r)
	})
}

// ---------------------------------------------------------------------------
// Raw span payload API  GET /api/traces/span/<span_id>
// Returns the full raw record for a single span as JSON.  The payload is
// truncated to rawSpanMaxBytes so that very large attribute blobs do not
// overwhelm the browser.  The endpoint is used by the lazy-loaded accordion
// on the trace detail page and is intentionally additive – it does not change
// any existing UI behaviour.
// ---------------------------------------------------------------------------

const rawSpanMaxBytes = 32 * 1024 // 32 KB display cap

// rawSpanJsonDumps mirrors json.dumps(..., ensure_ascii=False, indent=2).
func rawSpanJsonDumps(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// apiRawSpan returns the raw record for a single span as a JSON object.
func apiRawSpan(w http.ResponseWriter, r *http.Request) {
	spanId := strings.TrimSpace(r.PathValue("span_id"))
	if spanId == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "span_id is required"})
		return
	}

	traceId := strings.TrimSpace(r.URL.Query().Get("trace_id"))

	db := getDb()
	baseSql := "SELECT Timestamp, TraceId, SpanId, ParentSpanId, TraceState, " +
		"SpanName, SpanKind, ServiceName, ResourceAttributes, " +
		"ScopeName, ScopeVersion, SpanAttributes, Duration, " +
		"StatusCode, StatusMessage " +
		"FROM otel_traces WHERE SpanId=?"
	params := []any{spanId}
	if traceId != "" {
		// Prefer a trace-qualified match when available so duplicate span IDs
		// across traces return the expected row.
		baseSql += " AND TraceId=?"
		params = append(params, traceId)
	}
	// Keep fallback deterministic even when multiple rows share a span_id.
	baseSql += " ORDER BY Timestamp DESC LIMIT 1"
	res, err := db.Execute(baseSql, params...)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	row := res.Fetchone()

	if row == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "span not found"})
		return
	}

	spanAttrs := mapToDict(row["SpanAttributes"])
	resourceAttrs := mapToDict(row["ResourceAttributes"])

	durationNs := coerceInt(row["Duration"])
	payload := map[string]any{
		"timestamp":           rowString(row["Timestamp"]),
		"trace_id":            rowString(row["TraceId"]),
		"span_id":             rowString(row["SpanId"]),
		"parent_span_id":      rowString(row["ParentSpanId"]),
		"trace_state":         rowString(row["TraceState"]),
		"name":                rowString(row["SpanName"]),
		"kind":                rowString(row["SpanKind"]),
		"service":             rowString(row["ServiceName"]),
		"scope_name":          rowString(row["ScopeName"]),
		"scope_version":       rowString(row["ScopeVersion"]),
		"duration_ns":         durationNs,
		"duration_ms":         math.Round(float64(durationNs)/1_000_000*1000) / 1000,
		"status_code":         rowString(row["StatusCode"]),
		"status_message":      rowString(row["StatusMessage"]),
		"attributes":          spanAttrs,
		"resource_attributes": resourceAttrs,
	}

	maskedPayload := maskValueForOutput(payload, nil)
	raw := rawSpanJsonDumps(maskedPayload)
	truncated := false
	if len(raw) > rawSpanMaxBytes {
		truncated = true
		// Truncate large attribute values to keep the overall payload small.
		const attrTruncate = 512
		// PORT-NOTE: Python slices by characters; truncate by runes here.
		truncMap := func(src map[string]any) map[string]any {
			out := map[string]any{}
			for k, v := range src {
				if s, ok := v.(string); ok {
					rs := []rune(s)
					if len(rs) > attrTruncate {
						out[k] = string(rs[:attrTruncate]) + "…"
						continue
					}
				}
				out[k] = v
			}
			return out
		}
		payload["attributes"] = truncMap(spanAttrs)
		payload["resource_attributes"] = truncMap(resourceAttrs)
		maskedPayload = maskValueForOutput(payload, nil)
		raw = rawSpanJsonDumps(maskedPayload)
	}

	maskedJsonResponse(w, http.StatusOK, map[string]any{"span": maskedPayload, "raw": raw, "truncated": truncated})
}

// ---------------------------------------------------------------------------
// Incident view  GET /incident
// Aggregates primary event details, related evidence, anomaly state, and work
// item links into a single read-only incident context page.  Either trace_id
// or error_id must be supplied.  The time window used to gather related
// evidence defaults to ±(window_minutes/2) around the primary event and can be
// overridden with explicit from_ts / to_ts parameters.
// ---------------------------------------------------------------------------

const (
	incidentMaxRelatedErrors     = 50
	incidentMaxRelatedSpans      = 20
	incidentMaxRelatedRumEvents  = 20
	incidentWindowDefaultMinutes = 30
	incidentWindowMaxMinutes     = 180
)

func viewIncident(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	q := r.URL.Query()
	traceId := strings.TrimSpace(q.Get("trace_id"))
	errorId := strings.TrimSpace(q.Get("error_id"))
	rumSession := strings.TrimSpace(q.Get("rum_session"))
	rumTs := strings.TrimSpace(q.Get("rum_ts"))
	fromTs, toTs, timeError := parseTimeWindowArgs(r)

	windowMinutes := incidentWindowDefaultMinutes
	wmRaw := strings.TrimSpace(q.Get("window_minutes"))
	if wmRaw != "" {
		if wmInt, err := strconv.Atoi(wmRaw); err == nil {
			windowMinutes = max(1, min(incidentWindowMaxMinutes, wmInt))
		}
	}

	if traceId == "" && errorId == "" && rumSession == "" {
		renderTemplate(w, r, "incident.html", map[string]any{
			"trace_id":                 "",
			"error_id":                 "",
			"rum_session":              "",
			"rum_ts":                   "",
			"primary_error":            nil,
			"primary_trace":            nil,
			"primary_rum":              nil,
			"service":                  "",
			"from_ts":                  "",
			"to_ts":                    "",
			"window_minutes":           windowMinutes,
			"related_errors":           []map[string]any{},
			"related_errors_truncated": false,
			"existing_work_item":       nil,
			"related_log_count":        0,
			"related_span_count":       0,
			"related_rum_count":        0,
			"related_rum_sessions":     0,
			"related_rum_error_count":  0,
			"related_rum_events":       []map[string]any{},
			"raw_windows":              []map[string]any{},
			"metrics_context": map[string]any{
				"source_mode":      "none",
				"total_points":     0,
				"series":           []any{},
				"match_mode":       "none",
				"match_label":      "no match",
				"match_dimensions": []any{},
			},
			"anomaly_state":   nil,
			"work_item_links": map[string]map[string]any{},
			"time_error":      "",
			"error_msg":       "No incident reference provided. Specify trace_id, error_id, or rum_session.",
		})
		return
	}

	// ── Resolve primary error ───────────────────────────────────────────────
	var primaryError map[string]any
	if errorId != "" {
		scanLimit := 5000
		res, err := db.Execute(
			fmt.Sprintf("SELECT * FROM (%s) ORDER BY Timestamp DESC LIMIT ?", errorSourcesSql),
			scanLimit,
		)
		if err != nil {
			logger.Warn("view_incident: failed to look up error_id", "error_id", errorId, "error", err)
		} else {
			resolvedIds := getResolvedErrorIds(db)
			for _, row := range res.Fetchall() {
				candidate := buildErrorItem(row)
				if rowString(candidate["id"]) == errorId {
					candidate["resolved"] = resolvedIds[rowString(candidate["id"])]
					primaryError = candidate
					break
				}
			}
		}
	}

	// ── Resolve primary trace (root span summary) ───────────────────────────
	var primaryTrace map[string]any
	if traceId != "" {
		res, err := db.Execute(
			"SELECT Timestamp, TraceId, SpanId, ParentSpanId, SpanName, ServiceName, "+
				"Duration, StatusCode, SpanAttributes "+
				"FROM otel_traces WHERE TraceId=? ORDER BY Timestamp ASC",
			traceId,
		)
		if err != nil {
			logger.Warn("view_incident: failed to look up trace_id", "trace_id", traceId, "error", err)
		} else if spanRows := res.Fetchall(); len(spanRows) > 0 {
			serviceSet := map[string]bool{}
			for _, sr := range spanRows {
				if s := rowString(sr["ServiceName"]); s != "" {
					serviceSet[s] = true
				}
			}
			services := make([]string, 0, len(serviceSet))
			for s := range serviceSet {
				services = append(services, s)
			}
			sort.Strings(services)
			root := spanRows[0]
			startMs := tsStrToEpochMs(rowString(root["Timestamp"]))
			endMs := math.Inf(-1)
			for _, sr := range spanRows {
				durF, _ := coerceFloat(sr["Duration"])
				candidate := tsStrToEpochMs(rowString(sr["Timestamp"])) + roundTo2(durF/1_000_000)
				if candidate > endMs {
					endMs = candidate
				}
			}
			firstService := ""
			if len(services) > 0 {
				firstService = services[0]
			}
			primaryTrace = map[string]any{
				"trace_id":   traceId,
				"services":   services,
				"service":    firstService,
				"span_count": len(spanRows),
				"start_ts":   rowString(root["Timestamp"]),
				"start_ms":   int(math.Round(startMs)),
				"end_ms":     int(math.Round(endMs)),
				"total_ms":   roundTo2(endMs - startMs),
				"root_name":  rowString(root["SpanName"]),
				"status":     rowString(root["StatusCode"]),
			}
		}
	}

	// ── Resolve primary RUM event (session-scoped fallback) ─────────────────
	var primaryRum map[string]any
	if rumSession != "" {
		rumWhereParts := []string{rumSessionKeySql + "=?"}
		rumWhereParams := []any{rumSession}
		if rumTs != "" {
			rumWhereParts = append(rumWhereParts, "Timestamp <= parseDateTime64BestEffort(?, 9)")
			rumWhereParams = append(rumWhereParams, rumTs)
		}
		rumWhereSql := "WHERE " + strings.Join(rumWhereParts, " AND ")
		res, err := db.Execute(
			"SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId, ServiceName "+
				fmt.Sprintf("FROM hyperdx_sessions %s ", rumWhereSql)+
				"ORDER BY Timestamp DESC LIMIT 1",
			rumWhereParams...,
		)
		if err != nil {
			logger.Warn("view_incident: failed to look up rum_session", "rum_session", rumSession, "error", err)
		} else if rumRow := res.Fetchone(); rumRow != nil {
			primaryRum = buildRumEventItem(rumRow)
		}
	}

	// ── Determine primary service and event timestamp ───────────────────────
	service := ""
	eventTs := ""
	if primaryError != nil {
		service = rowString(primaryError["service"])
		eventTs = rowString(primaryError["ts"])
	} else if primaryTrace != nil {
		service = rowString(primaryTrace["service"])
		eventTs = rowString(primaryTrace["start_ts"])
	} else if primaryRum != nil {
		service = rowString(primaryRum["service"])
		eventTs = rowString(primaryRum["ts"])
	}

	// ── Expand time window around event if caller did not supply one ────────
	if eventTs != "" && !(fromTs != "" && toTs != "") && timeError == "" {
		isoCandidate := strings.TrimRight(strings.ReplaceAll(eventTs, " ", "T"), "Z") + "+00:00"
		if dt, err := parseIsoTimestamp(isoCandidate); err == nil {
			half := time.Duration(windowMinutes/2) * time.Minute
			fromTs = normalizeChTimestamp(dt.Add(-half))
			toTs = normalizeChTimestamp(dt.Add(half))
		}
	}

	// ── Gather related errors ───────────────────────────────────────────────
	relatedErrors := []map[string]any{}
	relatedErrorsTruncated := false
	{
		whereParts := []string{}
		whereParams := []any{}
		if traceId != "" {
			whereParts = append(whereParts, "TraceId=?")
			whereParams = append(whereParams, traceId)
		} else if service != "" {
			whereParts = append(whereParts, "ServiceName=?")
			whereParams = append(whereParams, service)
		}
		tc, tp := timeWindowConditions("Timestamp", fromTs, toTs)
		whereParts = append(whereParts, tc...)
		whereParams = append(whereParams, tp...)
		whereSql := ""
		if len(whereParts) > 0 {
			whereSql = "WHERE " + strings.Join(whereParts, " AND ")
		}
		res, err := db.Execute(
			fmt.Sprintf("SELECT * FROM (%s) %s ORDER BY Timestamp DESC LIMIT ?", errorSourcesSql, whereSql),
			append(whereParams, incidentMaxRelatedErrors+1)...,
		)
		if err != nil {
			logger.Warn("view_incident: failed to fetch related errors", "error", err)
		} else {
			errRows := res.Fetchall()
			resolvedIds := getResolvedErrorIds(db)
			primaryErrorId := ""
			if primaryError != nil {
				primaryErrorId = rowString(primaryError["id"])
			}
			limited := errRows
			if len(limited) > incidentMaxRelatedErrors {
				limited = limited[:incidentMaxRelatedErrors]
			}
			for _, row := range limited {
				item := buildErrorItem(row)
				item["resolved"] = resolvedIds[rowString(item["id"])]
				if rowString(item["id"]) != primaryErrorId {
					relatedErrors = append(relatedErrors, item)
				}
			}
			relatedErrorsTruncated = len(errRows) > incidentMaxRelatedErrors
		}
	}

	// ── Count related logs ──────────────────────────────────────────────────
	relatedLogCount := 0
	{
		logWhereParts := []string{}
		logWhereParams := []any{}
		if traceId != "" {
			logWhereParts = append(logWhereParts, "TraceId=?")
			logWhereParams = append(logWhereParams, traceId)
		} else if service != "" {
			logWhereParts = append(logWhereParts, "ServiceName=?")
			logWhereParams = append(logWhereParams, service)
		}
		tc, tp := timeWindowConditions("Timestamp", fromTs, toTs)
		logWhereParts = append(logWhereParts, tc...)
		logWhereParams = append(logWhereParams, tp...)
		logWhereSql := ""
		if len(logWhereParts) > 0 {
			logWhereSql = "WHERE " + strings.Join(logWhereParts, " AND ")
		}
		res, err := db.Execute(
			fmt.Sprintf("SELECT count() AS cnt FROM otel_logs %s", logWhereSql),
			logWhereParams...,
		)
		if err != nil {
			logger.Warn("view_incident: failed to count related logs", "error", err)
		} else if rowCnt := res.Fetchone(); rowCnt != nil {
			relatedLogCount = coerceInt(rowCnt["cnt"])
		}
	}

	// ── Count related spans ─────────────────────────────────────────────────
	relatedSpanCount := 0
	if service != "" {
		spanWhereParts := []string{"ServiceName=?"}
		spanWhereParams := []any{service}
		tc, tp := timeWindowConditions("Timestamp", fromTs, toTs)
		spanWhereParts = append(spanWhereParts, tc...)
		spanWhereParams = append(spanWhereParams, tp...)
		spanWhereSql := "WHERE " + strings.Join(spanWhereParts, " AND ")
		res, err := db.Execute(
			fmt.Sprintf("SELECT count() AS cnt FROM otel_traces %s", spanWhereSql),
			spanWhereParams...,
		)
		if err != nil {
			logger.Warn("view_incident: failed to count related spans", "error", err)
		} else if rowCnt := res.Fetchone(); rowCnt != nil {
			relatedSpanCount = coerceInt(rowCnt["cnt"])
		}
	}

	// ── RUM evidence summary ───────────────────────────────────────────────
	relatedRumCount := 0
	relatedRumSessions := 0
	relatedRumErrorCount := 0
	relatedRumEvents := []map[string]any{}
	{
		rumWhereParts := []string{}
		rumWhereParams := []any{}
		if traceId != "" {
			rumWhereParts = append(rumWhereParts, "TraceId=?")
			rumWhereParams = append(rumWhereParams, traceId)
		} else if service != "" {
			rumWhereParts = append(rumWhereParts, "(LogAttributes['service.name']=? OR LogAttributes['service']=?)")
			rumWhereParams = append(rumWhereParams, service, service)
		}
		tc, tp := timeWindowConditions("Timestamp", fromTs, toTs)
		rumWhereParts = append(rumWhereParts, tc...)
		rumWhereParams = append(rumWhereParams, tp...)
		rumWhereSql := ""
		if len(rumWhereParts) > 0 {
			rumWhereSql = "WHERE " + strings.Join(rumWhereParts, " AND ")
		}

		summaryRes, err := db.Execute(
			"SELECT "+
				"count() AS ev_count, "+
				fmt.Sprintf("uniq(%s) AS session_count, ", rumSessionKeySql)+
				"countIf(EventName IN ('error', 'unhandledrejection')) AS err_count "+
				fmt.Sprintf("FROM hyperdx_sessions %s", rumWhereSql),
			rumWhereParams...,
		)
		if err != nil {
			logger.Warn("view_incident: failed to fetch related RUM evidence", "error", err)
		} else {
			if rumSummaryRow := summaryRes.Fetchone(); rumSummaryRow != nil {
				relatedRumCount = coerceInt(rumSummaryRow["ev_count"])
				relatedRumSessions = coerceInt(rumSummaryRow["session_count"])
				relatedRumErrorCount = coerceInt(rumSummaryRow["err_count"])
			}

			rumRes, err := db.Execute(
				"SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId, ServiceName "+
					fmt.Sprintf("FROM hyperdx_sessions %s ", rumWhereSql)+
					"ORDER BY Timestamp DESC LIMIT ?",
				append(rumWhereParams, incidentMaxRelatedRumEvents)...,
			)
			if err != nil {
				logger.Warn("view_incident: failed to fetch related RUM evidence", "error", err)
			} else {
				for _, row := range rumRes.Fetchall() {
					relatedRumEvents = append(relatedRumEvents, buildRumEventItem(row))
				}
			}
		}
	}

	// ── Overlapping preserved raw windows + metric context ─────────────────
	rawWindows := []map[string]any{}
	metricsContext := map[string]any{
		"source_mode":      "none",
		"total_points":     0,
		"series":           []any{},
		"match_mode":       "none",
		"match_label":      "no match",
		"match_dimensions": []any{},
	}
	if fromTs != "" && toTs != "" {
		serviceNames := []string{}
		if service != "" {
			serviceNames = append(serviceNames, service)
		}
		rawWindows = listTraceOverlappingRawWindows(db, serviceNames, fromTs, toTs, 25)
		windowIds := []string{}
		for _, wnd := range rawWindows {
			if id := rowString(wnd["id"]); id != "" {
				windowIds = append(windowIds, id)
			}
		}
		// PORT-NOTE: Python passes keyword args; limit_metrics keeps its
		// default of 12 here.
		metricsContext = fetchTraceMetricContext(
			db,
			serviceNames,
			fromTs,
			toTs,
			windowIds,
			12,
			[]string{},
			[]string{},
			[]string{},
			[]string{},
		)
	}

	// ── Service anomaly state ───────────────────────────────────────────────
	var anomalyState any
	if service != "" {
		res, err := db.Execute(
			"SELECT anomaly_state FROM v_derived_signals_anomaly "+
				"WHERE ServiceName=? AND SignalSource='traces' "+
				"AND time >= now() - INTERVAL 48 HOUR "+
				"ORDER BY time DESC LIMIT 1",
			service,
		)
		if err != nil {
			logger.Warn("view_incident: failed to fetch anomaly state for service", "service", service, "error", err)
		} else if anomalyRow := res.Fetchone(); anomalyRow != nil {
			anomalyState = rowString(anomalyRow["anomaly_state"])
		}
	}

	// ── Work item links ─────────────────────────────────────────────────────
	refIds := []string{}
	if primaryError != nil {
		refIds = append(refIds, rowString(primaryError["id"]))
	} else if errorId != "" {
		refIds = append(refIds, errorId)
	}
	if traceId != "" {
		refIds = append(refIds, traceId)
	}
	if rumSession != "" {
		refIds = append(refIds, rumSession)
	}
	workItemLinks := loadWorkItemLinksForRefIds(db, refIds)

	// ── Resolve best existing work item for the raise-issue button ──────────
	var existingWorkItem map[string]any
	for _, ref := range refIds {
		wi := workItemLinks[ref]
		if wi != nil && pyTruthy(wi["issue_url"]) {
			existingWorkItem = wi
			break
		}
	}

	renderTemplate(w, r, "incident.html", map[string]any{
		"trace_id":                 traceId,
		"error_id":                 errorId,
		"rum_session":              rumSession,
		"rum_ts":                   rumTs,
		"primary_error":            primaryError,
		"primary_trace":            primaryTrace,
		"primary_rum":              primaryRum,
		"service":                  service,
		"from_ts":                  fromTs,
		"to_ts":                    toTs,
		"window_minutes":           windowMinutes,
		"related_errors":           relatedErrors,
		"related_errors_truncated": relatedErrorsTruncated,
		"related_log_count":        relatedLogCount,
		"related_span_count":       relatedSpanCount,
		"related_rum_count":        relatedRumCount,
		"related_rum_sessions":     relatedRumSessions,
		"related_rum_error_count":  relatedRumErrorCount,
		"related_rum_events":       relatedRumEvents,
		"raw_windows":              rawWindows,
		"metrics_context":          metricsContext,
		"anomaly_state":            anomalyState,
		"work_item_links":          workItemLinks,
		"existing_work_item":       existingWorkItem,
		"time_error":               timeError,
		"error_msg":                timeError,
	})
}

// ---------------------------------------------------------------------------
// Enrichment – geo-lookup helpers
// ---------------------------------------------------------------------------

// isPrivateIp returns true for private/loopback/link-local IPs that should not
// be geolocated.
// PORT-NOTE: netip.Addr.IsPrivate covers RFC 1918 + ULA, slightly narrower
// than Python ipaddress.is_private (which also includes e.g. 192.0.0.0/29,
// 198.18.0.0/15, reserved doc ranges).
func isPrivateIp(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return true
	}
	return addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified()
}

// buildGeoDict builds a normalised geo dict used throughout the geo-lookup
// subsystem.
func buildGeoDict(country, countryCode, city string, lat, lon float64) map[string]any {
	return map[string]any{"country": country, "country_code": countryCode, "city": city, "lat": lat, "lon": lon}
}

// geoIp2FastResult mirrors the result object returned by GeoIP2Fast.lookup().
type geoIp2FastResult struct {
	countryName string
	countryCode string
	isPrivate   bool
}

// geoIp2FastLookup is the lookup contract of the Python GeoIP2Fast class.
type geoIp2FastLookup interface {
	lookup(ip string) (*geoIp2FastResult, error)
}

// getGeoDb returns a singleton GeoIP2Fast instance (lazy-loaded, MIT licensed,
// local DB).
//
// PORT-NOTE: geoip2fast is a Python-only library and its bundled database is
// a Python pickle, so a pure-Go reader is not feasible. The loader is stubbed
// to always fail (mirroring the Python ImportError path): geo lookups are
// disabled and geoLookupBatch only classifies Private/Local addresses. The
// geoIp2FastLookup contract is kept so a real implementation can be plugged
// into the geoDb global later.
func getGeoDb() geoIp2FastLookup {
	if g, ok := geoDb.(geoIp2FastLookup); ok && g != nil {
		return g
	}
	geoDbLock.Lock()
	defer geoDbLock.Unlock()
	if g, ok := geoDb.(geoIp2FastLookup); ok && g != nil {
		return g
	}
	logger.Warn("geoip2fast not installed; geo lookups disabled. pip install geoip2fast")
	geoDb = nil
	return nil
}

// geoLookupBatch resolves a list of public IPs to geo info using a local
// geoip2fast database.
//
// All lookups are performed locally (no external network calls).
// geoip2fast is MIT licensed; its bundled data is sourced from IANA/RIR
// delegated statistics files (public domain).
func geoLookupBatch(ips []string, geoEnabled bool) map[string]map[string]any {
	if !geoEnabled || len(ips) == 0 {
		return map[string]map[string]any{}
	}

	geoDbInst := getGeoDb()
	results := map[string]map[string]any{}

	uncached := []string{}
	geoCacheLock.Lock()
	for _, ip := range ips {
		if isPrivateIp(ip) {
			results[ip] = buildGeoDict("Private/Local", "", "", 0, 0)
		} else if cached, ok := geoCache[ip]; ok {
			// PORT-NOTE: Python uses an OrderedDict LRU (move_to_end on hit);
			// the Go port uses a plain map, so recency ordering is lost.
			results[ip] = cached
		} else {
			uncached = append(uncached, ip)
		}
	}
	geoCacheLock.Unlock()

	if len(uncached) == 0 || geoDbInst == nil {
		return results
	}

	fresh := map[string]map[string]any{}
	for _, ip := range uncached {
		r, err := geoDbInst.lookup(ip)
		if err != nil {
			continue
		}
		if r != nil && !r.isPrivate {
			fresh[ip] = buildGeoDict(r.countryName, r.countryCode, "", 0, 0)
		} else {
			fresh[ip] = buildGeoDict("Private/Local", "", "", 0, 0)
		}
	}

	geoCacheLock.Lock()
	for len(geoCache) >= geoCacheMax {
		// PORT-NOTE: Python evicts the least-recently-used entry
		// (popitem(last=False)); the Go map evicts an arbitrary entry.
		for k := range geoCache {
			delete(geoCache, k)
			break
		}
	}
	for k, v := range fresh {
		geoCache[k] = v
	}
	geoCacheLock.Unlock()

	for k, v := range fresh {
		results[k] = v
	}
	return results
}

// ---------------------------------------------------------------------------
// Enrichment – CVE scanner helpers
// Extracts library/SDK versions from release metadata and OTEL telemetry,
// then queries OSV.dev (Apache 2.0) for known vulnerabilities.
// ---------------------------------------------------------------------------

// langToOsvEcosystem maps telemetry.sdk.language to an OSV.dev ecosystem name.
func langToOsvEcosystem(lang string) string {
	return map[string]string{
		"python":     "PyPI",
		"javascript": "npm",
		"nodejs":     "npm",
		"java":       "Maven",
		"go":         "Go",
		"ruby":       "RubyGems",
		"dotnet":     "NuGet",
		"rust":       "crates.io",
		"php":        "Packagist",
		"dart":       "Pub",
	}[strings.ToLower(lang)]
}

func inventoryScopeEcosystem(scopeName string) string {
	if strings.HasPrefix(scopeName, "io.opentelemetry") || strings.HasPrefix(scopeName, "com.") || strings.HasPrefix(scopeName, "org.") {
		return "Maven"
	}
	if strings.HasPrefix(scopeName, "@") {
		return "npm"
	}
	lastSegment := scopeName
	if idx := strings.LastIndex(scopeName, "/"); idx >= 0 {
		lastSegment = scopeName[idx+1:]
	}
	if strings.HasPrefix(scopeName, "opentelemetry-") && !strings.Contains(lastSegment, "_") {
		return "PyPI"
	}
	return ""
}

// parseGithubRepoOwnerName extracts owner/repo from a GitHub repo URL.
//
// Supports HTTPS, SSH, and plain owner/repo styles.
func parseGithubRepoOwnerName(repoUrl string) (string, string) {
	cleaned := strings.TrimSpace(repoUrl)
	if cleaned == "" {
		return "", ""
	}

	directParts := []string{}
	for _, p := range strings.Split(cleaned, "/") {
		if p != "" {
			directParts = append(directParts, p)
		}
	}
	if len(directParts) == 2 && !strings.Contains(cleaned, "://") && !strings.HasPrefix(cleaned, "git@") {
		return directParts[0], strings.TrimSuffix(directParts[1], ".git")
	}

	var repoPath string
	if strings.HasPrefix(cleaned, "git@github.com:") {
		repoPath = strings.SplitN(cleaned, ":", 2)[1]
	} else {
		parsed, err := url.Parse(cleaned)
		if err != nil || !strings.EqualFold(parsed.Host, "github.com") {
			return "", ""
		}
		repoPath = strings.TrimLeft(parsed.Path, "/")
	}

	repoPath = strings.TrimSuffix(repoPath, ".git")
	parts := []string{}
	for _, p := range strings.Split(repoPath, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func buildGithubRepoUrl(owner, repo string) string {
	ownerClean := strings.Trim(strings.TrimSpace(owner), "/")
	repoClean := strings.TrimSuffix(strings.Trim(strings.TrimSpace(repo), "/"), ".git")
	if ownerClean == "" || repoClean == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/%s", ownerClean, repoClean)
}

func resolveGithubRepoFields(repoUrl, owner, repo string) (string, string, string) {
	repoUrlClean := strings.TrimSpace(repoUrl)
	ownerClean := strings.Trim(strings.TrimSpace(owner), "/")
	repoClean := strings.TrimSuffix(strings.Trim(strings.TrimSpace(repo), "/"), ".git")

	if (ownerClean == "" || repoClean == "") && repoUrlClean != "" {
		parsedOwner, parsedRepo := parseGithubRepoOwnerName(repoUrlClean)
		if ownerClean == "" {
			ownerClean = parsedOwner
		}
		if repoClean == "" {
			repoClean = parsedRepo
		}
	}

	canonicalRepoUrl := buildGithubRepoUrl(ownerClean, repoClean)
	if canonicalRepoUrl != "" {
		repoUrlClean = canonicalRepoUrl
	}

	return repoUrlClean, ownerClean, repoClean
}

func parseRequirementsDependencies(content string) []map[string]string {
	deps := []map[string]string{}
	seen := map[[2]string]bool{}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.Index(line, " #"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		line = strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		if !strings.Contains(line, "==") {
			continue
		}
		nameVersion := strings.SplitN(line, "==", 2)
		pkg := strings.TrimSpace(nameVersion[0])
		ver := strings.TrimSpace(nameVersion[1])
		if pkg == "" || ver == "" {
			continue
		}
		key := [2]string{strings.ToLower(pkg), ver}
		if seen[key] {
			continue
		}
		seen[key] = true
		deps = append(deps, map[string]string{"package": pkg, "version": ver, "ecosystem": "PyPI"})
	}
	return deps
}

func parsePackageLockDependencies(content string) []map[string]string {
	deps := []map[string]string{}
	seen := map[[2]string]bool{}
	body, isDict := safeJsonLoads(content, map[string]any{}).(map[string]any)
	if !isDict {
		return deps
	}

	if packages, ok := body["packages"].(map[string]any); ok {
		for pkgPath, infoAny := range packages {
			info, isInfoDict := infoAny.(map[string]any)
			if !isInfoDict || pkgPath == "" || pkgPath == "." {
				continue
			}
			if !strings.HasPrefix(pkgPath, "node_modules/") {
				continue
			}
			name := pkgPath[strings.LastIndex(pkgPath, "node_modules/")+len("node_modules/"):]
			version := strings.TrimSpace(rowString(info["version"]))
			if name == "" || version == "" {
				continue
			}
			key := [2]string{strings.ToLower(name), version}
			if seen[key] {
				continue
			}
			seen[key] = true
			deps = append(deps, map[string]string{"package": name, "version": version, "ecosystem": "npm"})
		}
	}

	if len(deps) > 0 {
		return deps
	}

	legacy, isLegacyDict := body["dependencies"].(map[string]any)
	if !isLegacyDict {
		return deps
	}
	for name, infoAny := range legacy {
		info, isInfoDict := infoAny.(map[string]any)
		if !isInfoDict {
			continue
		}
		version := strings.TrimSpace(rowString(info["version"]))
		if name == "" || version == "" {
			continue
		}
		key := [2]string{strings.ToLower(name), version}
		if seen[key] {
			continue
		}
		seen[key] = true
		deps = append(deps, map[string]string{"package": name, "version": version, "ecosystem": "npm"})
	}
	return deps
}

func parseGoSumDependencies(content string) []map[string]string {
	deps := []map[string]string{}
	seen := map[[2]string]bool{}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		moduleName := strings.TrimSpace(parts[0])
		moduleVersion := strings.TrimSpace(parts[1])
		moduleVersion = strings.TrimSuffix(moduleVersion, "/go.mod")
		if moduleName == "" || moduleVersion == "" {
			continue
		}
		key := [2]string{strings.ToLower(moduleName), moduleVersion}
		if seen[key] {
			continue
		}
		seen[key] = true
		deps = append(deps, map[string]string{"package": moduleName, "version": moduleVersion, "ecosystem": "Go"})
	}
	return deps
}

var gemfileLockSpecRe = regexp.MustCompile(`^([A-Za-z0-9_\-\.]+)\s+\(([^)]+)\)`)

func parseGemfileLockDependencies(content string) []map[string]string {
	deps := []map[string]string{}
	seen := map[[2]string]bool{}
	inSpecs := false
	for _, raw := range strings.Split(content, "\n") {
		if strings.TrimSpace(raw) == "specs:" {
			inSpecs = true
			continue
		}
		if !inSpecs {
			continue
		}
		if raw != "" && !strings.HasPrefix(raw, " ") {
			break
		}
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		match := gemfileLockSpecRe.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		name := strings.TrimSpace(match[1])
		version := strings.TrimSpace(strings.SplitN(match[2], ",", 2)[0])
		if name == "" || version == "" {
			continue
		}
		key := [2]string{strings.ToLower(name), version}
		if seen[key] {
			continue
		}
		seen[key] = true
		deps = append(deps, map[string]string{"package": name, "version": version, "ecosystem": "RubyGems"})
	}
	return deps
}

func decodeGithubContentsPayload(payload map[string]any) []byte {
	content, isStr := payload["content"].(string)
	encoding := strings.ToLower(rowString(payload["encoding"]))
	if !isStr || encoding != "base64" {
		return nil
	}
	// PORT-NOTE: Python b64decode(validate=False) discards non-alphabet
	// characters; GitHub embeds newlines, so strip whitespace before decoding.
	compact := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', ' ':
			return -1
		}
		return r
	}, content)
	decoded, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(compact)
		if err != nil {
			return nil
		}
	}
	return decoded
}

var githubActionsSnapshotNameRe = regexp.MustCompile(`(?i)^pip-freeze-([a-z0-9_-]+)-([a-z0-9_-]+)\.txt$`)

// githubActionsSnapshotName mirrors _github_actions_snapshot_name; the bool
// reports whether the filename matched (Python returns None otherwise).
func githubActionsSnapshotName(filename string) (string, string, string, bool) {
	base := path.Base(strings.TrimSpace(filename))
	if base == "" || base == "." || base == "/" {
		return "", "", "", false
	}
	match := githubActionsSnapshotNameRe.FindStringSubmatch(base)
	if match == nil {
		return "", "", "", false
	}
	platform := strings.ToLower(match[1])
	architecture := strings.ToLower(match[2])
	return fmt.Sprintf("pip-freeze-%s-%s", platform, architecture), platform, architecture, true
}

// githubHttpGetJson issues a GET with the supplied headers/timeout and returns
// the status code and raw body (mirrors the httpx client.get call sites).
func githubHttpGetJson(client *http.Client, rawUrl string, params url.Values, headers map[string]string, timeout time.Duration) (int, []byte, error) {
	requestUrl := rawUrl
	if len(params) > 0 {
		requestUrl = rawUrl + "?" + params.Encode()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestUrl, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

// githubActionsDependencyRows returns dependency artifact rows from GH
// Actions snapshots for a release.
func githubActionsDependencyRows(
	client *http.Client,
	githubToken string,
	owner string,
	repo string,
	releaseId string,
	releaseVersion string,
	commitSha string,
) []Row {
	rows := []Row{}
	commit := strings.TrimSpace(commitSha)
	if commit == "" {
		// Without commit identity, we cannot safely bind a workflow run to this release.
		return rows
	}
	params := url.Values{}
	params.Set("status", "completed")
	params.Set("per_page", strconv.Itoa(githubActionsBackfillMaxRunsPerRelease))
	params.Set("head_sha", commit)

	status, body, err := githubHttpGetJson(
		client,
		fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs", owner, repo),
		params,
		githubApiHeaders(githubToken, false, nil),
		20*time.Second,
	)
	if err != nil || status != http.StatusOK {
		return rows
	}

	runsPayload := map[string]any{}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &runsPayload)
	}
	workflowRuns, isList := runsPayload["workflow_runs"].([]any)
	if !isList {
		return rows
	}

	for _, runAny := range workflowRuns {
		run, isDict := runAny.(map[string]any)
		if !isDict {
			continue
		}
		if strings.ToLower(rowString(run["conclusion"])) != "success" {
			continue
		}
		runId := strings.TrimSpace(rowString(run["id"]))
		if runId == "" {
			continue
		}

		artifactsStatus, artifactsBody, err := githubHttpGetJson(
			client,
			fmt.Sprintf("https://api.github.com/repos/%s/%s/actions/runs/%s/artifacts", owner, repo, runId),
			url.Values{"per_page": []string{"100"}},
			githubApiHeaders(githubToken, false, nil),
			20*time.Second,
		)
		if err != nil || artifactsStatus != http.StatusOK {
			continue
		}

		artifactsPayload := map[string]any{}
		if len(artifactsBody) > 0 {
			_ = json.Unmarshal(artifactsBody, &artifactsPayload)
		}
		artifacts, isArtifactsList := artifactsPayload["artifacts"].([]any)
		if !isArtifactsList {
			continue
		}

		var snapshotArtifact map[string]any
		for _, artifactAny := range artifacts {
			artifact, isArtifactDict := artifactAny.(map[string]any)
			if !isArtifactDict {
				continue
			}
			if rowString(artifact["name"]) != githubActionsSnapshotArtifactName {
				continue
			}
			if pyTruthy(artifact["expired"]) {
				continue
			}
			snapshotArtifact = artifact
			break
		}
		if snapshotArtifact == nil {
			continue
		}

		archiveUrl := strings.TrimSpace(rowString(snapshotArtifact["archive_download_url"]))
		artifactId := strings.TrimSpace(rowString(snapshotArtifact["id"]))
		if archiveUrl == "" {
			continue
		}

		archiveStatus, archiveBody, err := githubHttpGetJson(
			client,
			archiveUrl,
			nil,
			githubApiHeaders(githubToken, false, map[string]string{"Accept": "application/octet-stream"}),
			30*time.Second,
		)
		if err != nil || archiveStatus != http.StatusOK || len(archiveBody) == 0 {
			continue
		}

		zipReader, err := zip.NewReader(bytes.NewReader(archiveBody), int64(len(archiveBody)))
		if err != nil {
			continue
		}
		for _, info := range zipReader.File {
			if info.FileInfo().IsDir() {
				continue
			}
			depName, platform, architecture, ok := githubActionsSnapshotName(info.Name)
			if !ok {
				continue
			}
			fileReader, err := info.Open()
			if err != nil {
				continue
			}
			rawBytes, err := io.ReadAll(fileReader)
			_ = fileReader.Close()
			if err != nil {
				continue
			}
			deps := parseRequirementsDependencies(string(rawBytes))
			if len(deps) == 0 {
				continue
			}

			metadataJson, _ := json.Marshal(map[string]any{
				"source":          "github_actions_artifact",
				"repo":            fmt.Sprintf("%s/%s", owner, repo),
				"run_id":          runId,
				"run_head_sha":    rowString(run["head_sha"]),
				"release_version": releaseVersion,
				"artifact_name":   rowString(snapshotArtifact["name"]),
				"dependencies":    deps,
			})
			hexId := uuid4Hex()
			rows = append(rows, Row{
				"Id":           hexId[0:8] + "-" + hexId[8:12] + "-" + hexId[12:16] + "-" + hexId[16:20] + "-" + hexId[20:],
				"ReleaseId":    releaseId,
				"ArtifactType": "dependencies-lockfile",
				"Name":         depName,
				"ContentType":  "text/plain",
				"Size":         len(rawBytes),
				"StorageRef": fmt.Sprintf(
					"github-actions://%s/%s/runs/%s/artifacts/%s/%s",
					owner, repo, runId, artifactId, path.Base(info.Name),
				),
				"ChecksumSha256": fmt.Sprintf("%x", sha256.Sum256(rawBytes)),
				"Platform":       platform,
				"Architecture":   architecture,
				"MetadataJson":   string(metadataJson),
				"UploadedAt":     normalizeChTimestamp(time.Now().UTC()),
				"IsDeleted":      0,
				"Version":        time.Now().UnixMilli(),
			})
		}

		if len(rows) > 0 {
			return rows
		}
	}

	return rows
}

// githubRefCandidates returns Git refs to try for a release version, in
// priority order.
func githubRefCandidates(releaseVersion string) []string {
	version := strings.TrimSpace(releaseVersion)
	if version == "" {
		return nil
	}

	candidates := []string{fmt.Sprintf("refs/tags/%s", version)}
	if !strings.HasPrefix(version, "v") {
		candidates = append(candidates, fmt.Sprintf("refs/tags/v%s", version))
	}
	// Some teams publish snapshot builds from branches instead of tags.
	candidates = append(candidates, fmt.Sprintf("refs/heads/%s", version))
	// GitHub Contents API also accepts a raw branch/commit-ish ref string.
	candidates = append(candidates, version)

	deduped := []string{}
	seen := map[string]bool{}
	for _, ref := range candidates {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		deduped = append(deduped, ref)
	}
	return deduped
}

func githubBackfillMaxReleases(db *ChDbConnection) int {
	rawValue := strings.TrimSpace(getAppSetting(db, githubBackfillMaxReleasesSetting))
	if rawValue == "" {
		return githubBackfillMaxReleasesDefault
	}
	parsed, err := strconv.Atoi(rawValue)
	if err != nil {
		return githubBackfillMaxReleasesDefault
	}
	return max(githubBackfillMaxReleasesMin, min(githubBackfillMaxReleasesMax, parsed))
}

func githubVersionTokens(version string) map[string]bool {
	v := strings.ToLower(strings.TrimSpace(version))
	if v == "" {
		return map[string]bool{}
	}
	tokens := map[string]bool{v: true}
	if !strings.HasPrefix(v, "v") {
		tokens["v"+v] = true
	}
	return tokens
}

func textMentionsVersionTokens(text string, tokens map[string]bool) bool {
	if text == "" || len(tokens) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	for token := range tokens {
		// Use non-alnum boundaries to reduce unrelated partial matches.
		pattern := fmt.Sprintf(`(^|[^0-9a-z])%s([^0-9a-z]|$)`, regexp.QuoteMeta(token))
		if matched, err := regexp.MatchString(pattern, lower); err == nil && matched {
			return true
		}
	}
	return false
}

func githubItemIsSecurityRelated(item map[string]any) bool {
	securityKeywords := []string{"security", "vulnerability", "cve", "ghsa", "dependabot"}
	title := strings.ToLower(rowString(item["title"]))
	body := strings.ToLower(rowString(item["body"]))
	for _, k := range securityKeywords {
		if strings.Contains(title, k) || strings.Contains(body, k) {
			return true
		}
	}
	if labels, isList := item["labels"].([]any); isList {
		for _, labelAny := range labels {
			label, isDict := labelAny.(map[string]any)
			if !isDict {
				continue
			}
			name := strings.ToLower(rowString(label["name"]))
			for _, k := range securityKeywords {
				if strings.Contains(name, k) {
					return true
				}
			}
		}
	}
	return false
}

// fetchReleaseDepsFromGithub backfills dependencies-lockfile artifacts from
// GitHub tags when missing.
func fetchReleaseDepsFromGithub(db *ChDbConnection) map[string]int {
	githubToken := strings.TrimSpace(loadAiSetting(db, "ai.github_token", ""))
	maxReleases := githubBackfillMaxReleases(db)
	if githubToken == "" {
		return map[string]int{"attempted": 0, "inserted": 0, "max_releases": maxReleases}
	}

	existingReleaseIds := map[string]bool{}
	existingRes, err := db.Execute(
		"SELECT DISTINCT ReleaseId FROM sobs_release_artifacts FINAL " +
			"WHERE ArtifactType='dependencies-lockfile' AND IsDeleted=0",
	)
	if err != nil {
		logger.Debug("github deps fetch: failed reading existing dependency artifacts", "error", err)
	} else {
		for _, row := range existingRes.Fetchall() {
			existingReleaseIds[rowString(row["ReleaseId"])] = true
		}
	}

	releaseRes, err := db.Execute(
		"SELECT Id, AppId, ReleaseVersion, CommitSha " +
			"FROM sobs_app_releases FINAL " +
			"WHERE IsDeleted=0 " +
			fmt.Sprintf("ORDER BY ReleasedAt DESC LIMIT %d", maxReleases),
	)
	var appRes *ChDbResult
	if err == nil {
		appRes, err = db.Execute("SELECT Id, RepoUrl, Enabled " + "FROM sobs_apps FINAL " + "WHERE IsDeleted=0")
	}
	if err != nil {
		logger.Debug("github deps fetch: failed loading releases", "error", err)
		return map[string]int{"attempted": 0, "inserted": 0, "max_releases": maxReleases}
	}

	appsById := map[string]map[string]any{}
	for _, r := range appRes.Fetchall() {
		appsById[rowString(r["Id"])] = map[string]any{
			"repo_url": strings.TrimSpace(rowString(r["RepoUrl"])),
			"enabled":  coerceInt(r["Enabled"]),
		}
	}

	type lockfileCandidate struct {
		path        string
		contentType string
		parserKind  string
	}
	candidates := []lockfileCandidate{
		{"requirements.txt", "text/plain", "requirements"},
		{"package-lock.json", "application/json", "package_lock"},
		{"go.sum", "text/plain", "go_sum"},
		{"Gemfile.lock", "text/plain", "gemfile_lock"},
	}
	parserByKind := map[string]func(string) []map[string]string{
		"requirements": parseRequirementsDependencies,
		"package_lock": parsePackageLockDependencies,
		"go_sum":       parseGoSumDependencies,
		"gemfile_lock": parseGemfileLockDependencies,
	}

	// PORT-NOTE: _get_async_http_client() → shared core httpClient.
	client := httpClient
	insertedRows := []Row{}
	attempted := 0
	inserted := 0

	for _, row := range releaseRes.Fetchall() {
		releaseId := rowString(row["Id"])
		appId := rowString(row["AppId"])
		releaseVersion := strings.TrimSpace(rowString(row["ReleaseVersion"]))
		commitSha := strings.TrimSpace(rowString(row["CommitSha"]))
		appInfo := appsById[appId]
		repoUrl := ""
		appEnabled := 0
		if appInfo != nil {
			repoUrl = strings.TrimSpace(rowString(appInfo["repo_url"]))
			appEnabled = coerceInt(appInfo["enabled"])
		}
		if releaseId == "" || releaseVersion == "" || existingReleaseIds[releaseId] {
			continue
		}
		if appEnabled == 0 || repoUrl == "" {
			continue
		}

		owner, repo := parseGithubRepoOwnerName(repoUrl)
		if owner == "" || repo == "" {
			continue
		}

		attempted++

		actionsRows := githubActionsDependencyRows(
			client,
			githubToken,
			owner,
			repo,
			releaseId,
			releaseVersion,
			commitSha,
		)
		if len(actionsRows) > 0 {
			insertedRows = append(insertedRows, actionsRows...)
			existingReleaseIds[releaseId] = true
			inserted += len(actionsRows)
			continue
		}

		foundForRelease := false
		for _, ref := range githubRefCandidates(releaseVersion) {
			for _, candidate := range candidates {
				// PORT-NOTE: urllib.parse.quote(path, safe="/") is identity for
				// these fixed lockfile names.
				contentsUrl := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", owner, repo, candidate.path)
				status, respBody, err := githubHttpGetJson(
					client,
					contentsUrl,
					url.Values{"ref": []string{ref}},
					map[string]string{
						"Authorization":        "Bearer " + githubToken,
						"Accept":               "application/vnd.github+json",
						"X-GitHub-Api-Version": "2022-11-28",
					},
					12*time.Second,
				)
				if err != nil {
					continue
				}
				if status == http.StatusNotFound {
					continue
				}
				if status != http.StatusOK {
					break
				}

				body := map[string]any{}
				if len(respBody) > 0 {
					if unmarshalErr := json.Unmarshal(respBody, &body); unmarshalErr != nil {
						continue
					}
				}

				rawBytes := decodeGithubContentsPayload(body)
				if len(rawBytes) == 0 {
					continue
				}

				parser := parserByKind[candidate.parserKind]
				deps := parser(string(rawBytes))
				if len(deps) == 0 {
					continue
				}

				checksum := fmt.Sprintf("%x", sha256.Sum256(rawBytes))
				// PORT-NOTE: urllib.parse.quote(ref, safe='') → QueryEscape with
				// "+" rewritten to "%20" (refs rarely contain spaces).
				storageRef := fmt.Sprintf(
					"github://%s/%s/%s?ref=%s",
					owner, repo, candidate.path,
					strings.ReplaceAll(url.QueryEscape(ref), "+", "%20"),
				)
				hexId := uuid4Hex()
				artifactId := hexId[0:8] + "-" + hexId[8:12] + "-" + hexId[12:16] + "-" + hexId[16:20] + "-" + hexId[20:]
				metadataJson, _ := json.Marshal(map[string]any{
					"source":       "github_contents_api",
					"repo":         fmt.Sprintf("%s/%s", owner, repo),
					"ref":          ref,
					"path":         candidate.path,
					"dependencies": deps,
				})
				insertedRows = append(insertedRows, Row{
					"Id":             artifactId,
					"ReleaseId":      releaseId,
					"ArtifactType":   "dependencies-lockfile",
					"Name":           candidate.path,
					"ContentType":    candidate.contentType,
					"Size":           len(rawBytes),
					"StorageRef":     storageRef,
					"ChecksumSha256": checksum,
					"Platform":       "",
					"Architecture":   "",
					"MetadataJson":   string(metadataJson),
					"UploadedAt":     normalizeChTimestamp(time.Now().UTC()),
					"IsDeleted":      0,
					"Version":        time.Now().UnixMilli(),
				})
				existingReleaseIds[releaseId] = true
				inserted++
				foundForRelease = true
				break
			}
			if foundForRelease {
				break
			}
		}
	}

	if len(insertedRows) > 0 {
		if _, err := insertRowsJsonEachRow(db, "sobs_release_artifacts", insertedRows); err != nil {
			logger.Warn("github deps fetch: failed storing dependency artifacts", "error", err)
			inserted = 0
		}
	}

	return map[string]int{"attempted": attempted, "inserted": inserted, "max_releases": maxReleases}
}

// collectLibraryInventory collects deduplicated library inventory from release
// metadata and OTEL telemetry.
//
// Source priority:
// 1. release_registry dependencies-lockfile artifacts registered by CI
// 2. telemetry.sdk.* attributes from traces/logs
// 3. ScopeName / ScopeVersion from traces/logs
func collectLibraryInventory(db *ChDbConnection) []map[string]string {
	inventory := map[string]map[string]string{}
	// PORT-NOTE: Python dict preserves insertion order; iteration order of the
	// returned slice is made deterministic via key tracking below.
	inventoryKeys := []string{}
	sourcePriority := map[string]int{"release_registry": 0, "otel_sdk": 1, "otel_scope": 2}

	serviceLabel := func(item map[string]string) string {
		if item["service"] != "" {
			return item["service"]
		}
		return item["app_name"]
	}

	keyOf := func(item map[string]string) string {
		return strings.Join(
			[]string{item["ecosystem"], item["package"], item["version"], serviceLabel(item)},
			"::",
		)
	}

	priorityOf := func(source string) int {
		if p, ok := sourcePriority[source]; ok {
			return p
		}
		return 99
	}

	add := func(item map[string]string) {
		pkg := strings.TrimSpace(item["package"])
		version := strings.TrimSpace(item["version"])
		if pkg == "" || version == "" {
			return
		}
		normalized := map[string]string{
			"package":         pkg,
			"version":         version,
			"ecosystem":       strings.TrimSpace(item["ecosystem"]),
			"service":         strings.TrimSpace(item["service"]),
			"source":          strings.TrimSpace(item["source"]),
			"app_name":        strings.TrimSpace(item["app_name"]),
			"release_version": strings.TrimSpace(item["release_version"]),
			"environment":     strings.TrimSpace(item["environment"]),
		}
		itemKey := keyOf(normalized)
		current := inventory[itemKey]
		if current == nil {
			inventory[itemKey] = normalized
			inventoryKeys = append(inventoryKeys, itemKey)
			return
		}
		if priorityOf(normalized["source"]) < priorityOf(current["source"]) {
			inventory[itemKey] = normalized
		}
	}

	// Tier 1: dependencies-lockfile artifacts registered via CI/release metadata.
	func() {
		artifactRes, err := db.Execute(
			"SELECT ReleaseId, Name, MetadataJson " +
				"FROM sobs_release_artifacts FINAL " +
				"WHERE ArtifactType='dependencies-lockfile' AND IsDeleted=0 " +
				"ORDER BY UploadedAt DESC LIMIT 500",
		)
		if err != nil {
			logger.Debug("release registry dependency inventory query failed", "error", err)
			return
		}
		releaseRes, err := db.Execute(
			"SELECT Id, AppId, ReleaseVersion, Environment " + "FROM sobs_app_releases FINAL WHERE IsDeleted=0",
		)
		if err != nil {
			logger.Debug("release registry dependency inventory query failed", "error", err)
			return
		}
		appRes, err := db.Execute("SELECT Id, Name, Slug FROM sobs_apps FINAL WHERE IsDeleted=0")
		if err != nil {
			logger.Debug("release registry dependency inventory query failed", "error", err)
			return
		}
		releasesById := map[string]map[string]string{}
		for _, row := range releaseRes.Fetchall() {
			releasesById[rowString(row["Id"])] = map[string]string{
				"app_id":          rowString(row["AppId"]),
				"release_version": rowString(row["ReleaseVersion"]),
				"environment":     rowString(row["Environment"]),
			}
		}
		appsById := map[string]map[string]string{}
		for _, row := range appRes.Fetchall() {
			appsById[rowString(row["Id"])] = map[string]string{
				"name": rowString(row["Name"]),
				"slug": rowString(row["Slug"]),
			}
		}
		for _, row := range artifactRes.Fetchall() {
			releaseInfo := releasesById[rowString(row["ReleaseId"])]
			if releaseInfo == nil {
				releaseInfo = map[string]string{}
			}
			appInfo := appsById[releaseInfo["app_id"]]
			if appInfo == nil {
				appInfo = map[string]string{}
			}
			metadata, isDict := safeJsonLoads(row["MetadataJson"], map[string]any{}).(map[string]any)
			if !isDict {
				continue
			}
			dependencies, isList := metadata["dependencies"].([]any)
			if !isList {
				continue
			}
			for _, depAny := range dependencies {
				dep, isDepDict := depAny.(map[string]any)
				if !isDepDict {
					continue
				}
				appName := appInfo["name"]
				if appName == "" {
					appName = appInfo["slug"]
				}
				pkg := rowString(dep["package"])
				if _, present := dep["package"]; !present {
					pkg = rowString(dep["name"])
				}
				add(map[string]string{
					"package":         pkg,
					"version":         rowString(dep["version"]),
					"ecosystem":       rowString(dep["ecosystem"]),
					"service":         appName,
					"source":          "release_registry",
					"app_name":        appName,
					"release_version": releaseInfo["release_version"],
					"environment":     releaseInfo["environment"],
				})
			}
		}
	}()

	// Tier 2: telemetry.sdk.* from traces.
	sdkInventoryQuery := func(table, failMsg string) {
		res, err := db.Execute(
			"SELECT " +
				"  ResourceAttributes['telemetry.sdk.name'] AS sdk_name, " +
				"  ResourceAttributes['telemetry.sdk.version'] AS sdk_version, " +
				"  ResourceAttributes['telemetry.sdk.language'] AS sdk_lang, " +
				"  ServiceName " +
				fmt.Sprintf("FROM %s ", table) +
				"WHERE ResourceAttributes['telemetry.sdk.version'] != '' " +
				"GROUP BY sdk_name, sdk_version, sdk_lang, ServiceName " +
				"LIMIT 200",
		)
		if err != nil {
			logger.Debug(failMsg, "error", err)
			return
		}
		for _, row := range res.Fetchall() {
			add(map[string]string{
				"package":   rowString(row["sdk_name"]),
				"version":   rowString(row["sdk_version"]),
				"ecosystem": langToOsvEcosystem(rowString(row["sdk_lang"])),
				"service":   rowString(row["ServiceName"]),
				"source":    "otel_sdk",
			})
		}
	}
	sdkInventoryQuery("otel_traces", "otel trace sdk inventory query failed")
	// Tier 2: telemetry.sdk.* from logs.
	sdkInventoryQuery("otel_logs", "otel log sdk inventory query failed")

	// Tier 3: instrumentation library versions via ScopeName / ScopeVersion.
	scopeInventoryQuery := func(table, failMsg string) {
		res, err := db.Execute(
			"SELECT ScopeName, ScopeVersion, ServiceName " +
				fmt.Sprintf("FROM %s ", table) +
				"WHERE ScopeVersion != '' AND ScopeName != '' " +
				"GROUP BY ScopeName, ScopeVersion, ServiceName " +
				"LIMIT 300",
		)
		if err != nil {
			logger.Debug(failMsg, "error", err)
			return
		}
		for _, row := range res.Fetchall() {
			scopeName := rowString(row["ScopeName"])
			add(map[string]string{
				"package":   scopeName,
				"version":   rowString(row["ScopeVersion"]),
				"ecosystem": inventoryScopeEcosystem(scopeName),
				"service":   rowString(row["ServiceName"]),
				"source":    "otel_scope",
			})
		}
	}
	scopeInventoryQuery("otel_traces", "otel trace scope inventory query failed")
	scopeInventoryQuery("otel_logs", "otel log scope inventory query failed")

	out := make([]map[string]string, 0, len(inventoryKeys))
	for _, k := range inventoryKeys {
		out = append(out, inventory[k])
	}
	return out
}

// extractLibraryVersionsFromOtel is a backward-compatible wrapper for existing
// OTEL/library inventory callers.
func extractLibraryVersionsFromOtel(db *ChDbConnection) []map[string]string {
	inventory := collectLibraryInventory(db)
	out := make([]map[string]string, 0, len(inventory))
	for _, item := range inventory {
		service := item["service"]
		if service == "" {
			service = item["app_name"]
		}
		out = append(out, map[string]string{
			"package":   item["package"],
			"version":   item["version"],
			"ecosystem": item["ecosystem"],
			"service":   service,
		})
	}
	return out
}

// inventoryVersionsByPackage maps ecosystem/package to currently observed
// versions in merged inventory.
func inventoryVersionsByPackage(db *ChDbConnection) map[string]map[string]bool {
	versionsByPackage := map[string]map[string]bool{}
	for _, item := range collectLibraryInventory(db) {
		pkg := strings.TrimSpace(item["package"])
		ecosystem := strings.TrimSpace(item["ecosystem"])
		version := strings.TrimSpace(item["version"])
		if pkg == "" || ecosystem == "" || version == "" {
			continue
		}
		key := fmt.Sprintf("%s::%s", ecosystem, pkg)
		if versionsByPackage[key] == nil {
			versionsByPackage[key] = map[string]bool{}
		}
		versionsByPackage[key][version] = true
	}
	return versionsByPackage
}

// effectiveCveDisposition returns the effective disposition and whether it was
// auto-expired.
//
// A `fixed` disposition expires once a different version for the same
// package+ecosystem appears in the merged inventory.
func effectiveCveDisposition(
	rawDisposition string,
	pkg string,
	ecosystem string,
	version string,
	versionsByPackage map[string]map[string]bool,
) (string, bool) {
	disposition := rawDisposition
	if disposition == "" {
		disposition = "open"
	}
	if disposition != "fixed" {
		return disposition, false
	}
	currentVersions := versionsByPackage[fmt.Sprintf("%s::%s", ecosystem, pkg)]
	for v := range currentVersions {
		if v != version {
			return "open", true
		}
	}
	return disposition, false
}

// runCveScan scans release metadata and OTEL telemetry for library versions
// and checks OSV.dev for CVEs.
//
// Stores results in sobs_cve_findings.  Returns a summary dict.
// Returns early if CVE enrichment is disabled.
// PORT-NOTE: Python db=None default → pass nil to use getDb().
func runCveScan(db *ChDbConnection) map[string]any {
	resolvedDb := db
	if resolvedDb == nil {
		resolvedDb = getDb()
	}
	cveEnabledRaw := getAppSetting(resolvedDb, cveEnabledSetting)
	if cveEnabledRaw == "" {
		cveEnabledRaw = "true"
	}
	cveEnabledLower := strings.ToLower(cveEnabledRaw)
	cveEnabled := cveEnabledLower == "1" || cveEnabledLower == "true" || cveEnabledLower == "yes"
	if !cveEnabled {
		return map[string]any{"ok": false, "reason": "disabled"}
	}

	githubBackfill := fetchReleaseDepsFromGithub(resolvedDb)
	setAppSetting(resolvedDb, cveLastBackfillAttemptedSetting, strconv.Itoa(githubBackfill["attempted"]))
	setAppSetting(resolvedDb, cveLastBackfillInsertedSetting, strconv.Itoa(githubBackfill["inserted"]))
	setAppSetting(resolvedDb, cveLastBackfillCapSetting, strconv.Itoa(githubBackfill["max_releases"]))

	libraries := collectLibraryInventory(resolvedDb)
	if len(libraries) == 0 {
		setAppSetting(resolvedDb, cveLastScanSetting, nowIso())
		return map[string]any{
			"ok":                           true,
			"libraries_found":              0,
			"vulns_found":                  0,
			"github_backfill_attempted":    githubBackfill["attempted"],
			"github_backfill_inserted":     githubBackfill["inserted"],
			"github_backfill_max_releases": githubBackfill["max_releases"],
		}
	}

	// PORT-NOTE: _get_async_http_client() → shared core httpClient.
	client := httpClient
	scanTs := nowIso()
	allFindings := []Row{}
	newCount := 0

	for _, lib := range libraries {
		pkg := lib["package"]
		eco := lib["ecosystem"]
		ver := lib["version"]
		if pkg == "" || eco == "" {
			continue
		}
		func() {
			queryBody := map[string]any{
				"package": map[string]any{"name": pkg, "ecosystem": eco},
				"version": ver,
			}
			bodyBytes, err := json.Marshal(queryBody)
			if err != nil {
				logger.Debug("CVE scan failed", "ecosystem", eco, "package", pkg, "version", ver, "error", err)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(
				ctx, http.MethodPost, "https://api.osv.dev/v1/query", bytes.NewReader(bodyBytes))
			if err != nil {
				logger.Debug("CVE scan failed", "ecosystem", eco, "package", pkg, "version", ver, "error", err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				logger.Debug("CVE scan failed", "ecosystem", eco, "package", pkg, "version", ver, "error", err)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return
			}
			respBody, err := io.ReadAll(resp.Body)
			if err != nil {
				logger.Debug("CVE scan failed", "ecosystem", eco, "package", pkg, "version", ver, "error", err)
				return
			}
			data := map[string]any{}
			if err := json.Unmarshal(respBody, &data); err != nil {
				logger.Debug("CVE scan failed", "ecosystem", eco, "package", pkg, "version", ver, "error", err)
				return
			}
			vulns, _ := data["vulns"].([]any)
			if len(vulns) > cveMaxVulnsPerPkg {
				vulns = vulns[:cveMaxVulnsPerPkg]
			}
			for _, vAny := range vulns {
				v, isDict := vAny.(map[string]any)
				if !isDict {
					continue
				}
				cveIds := []string{}
				if aliases, ok := v["aliases"].([]any); ok {
					for _, aliasAny := range aliases {
						if alias, isStr := aliasAny.(string); isStr && strings.HasPrefix(alias, "CVE-") {
							cveIds = append(cveIds, alias)
						}
					}
				}
				severity := ""
				if sevList, ok := v["severity"].([]any); ok && len(sevList) > 0 {
					if sev0, isSevDict := sevList[0].(map[string]any); isSevDict {
						severity = rowString(sev0["score"])
						if severity == "" {
							severity = rowString(sev0["type"])
						}
					}
				}
				dbSpecific, _ := v["database_specific"].(map[string]any)
				if severity == "" && dbSpecific != nil && pyTruthy(dbSpecific["severity"]) {
					severity = rowString(dbSpecific["severity"])
				}
				summary := []rune(rowString(v["summary"]))
				if len(summary) > 500 {
					summary = summary[:500]
				}
				published := rowString(v["published"])
				if len(published) > 10 {
					published = published[:10]
				}
				allFindings = append(allFindings, Row{
					"Package":     pkg,
					"Ecosystem":   eco,
					"Version":     ver,
					"ServiceName": lib["service"],
					"OsvId":       rowString(v["id"]),
					"CveIds":      strings.Join(cveIds, ","),
					"Summary":     string(summary),
					"Severity":    severity,
					"Published":   published,
					"ScannedAt":   scanTs,
				})
				newCount++
			}
		}()
	}

	if len(allFindings) > 0 {
		if _, err := insertRowsJsonEachRow(resolvedDb, "sobs_cve_findings", allFindings); err != nil {
			logger.Warn("Failed to store CVE findings", "error", err)
		}
	}

	setAppSetting(resolvedDb, cveLastScanSetting, scanTs)
	return map[string]any{
		"ok":                           true,
		"libraries_found":              len(libraries),
		"vulns_found":                  newCount,
		"scanned_at":                   scanTs,
		"github_backfill_attempted":    githubBackfill["attempted"],
		"github_backfill_inserted":     githubBackfill["inserted"],
		"github_backfill_max_releases": githubBackfill["max_releases"],
	}
}

// cveScannerLoop is a background task: scan for CVEs in collected library
// inventory every 24 hours.
func cveScannerLoop() {
	time.Sleep(time.Duration(cveScanInitialDelayS) * time.Second)
	for {
		summary := runCveScan(nil)
		if pyTruthy(summary["ok"]) && coerceInt(summary["vulns_found"]) > 0 {
			logger.Info(
				"CVE scan complete",
				"libraries", summary["libraries_found"],
				"vulnerabilities", summary["vulns_found"],
			)
		}
		time.Sleep(time.Duration(cveScanIntervalS) * time.Second)
	}
}

// githubRepoHealthLoop is a background task: periodically sync GitHub repo
// health for configured repos.
func githubRepoHealthLoop() {
	time.Sleep(time.Duration(githubRepoHealthInitialDelayS) * time.Second)
	for {
		syncGithubRepoHealthOnce(nil)
		time.Sleep(time.Duration(githubRepoHealthIntervalS) * time.Second)
	}
}

// syncGithubRepoHealthOnce runs a single GitHub repo-health sync and persists
// summary settings.
// PORT-NOTE: Python db=None default → pass nil to use getDb().
func syncGithubRepoHealthOnce(db *ChDbConnection) map[string]any {
	resolvedDb := db
	if resolvedDb == nil {
		resolvedDb = getDb()
	}
	summary := collectGithubRepoHealthSummary(resolvedDb)
	if !pyTruthy(summary["ok"]) {
		return summary
	}

	compactValues := map[string]int{
		"scanned_repos":          coerceInt(summary["scanned_repos"]),
		"total_repos_considered": coerceInt(summary["total_repos_considered"]),
		"open_issues":            coerceInt(summary["open_issues"]),
		"open_prs":               coerceInt(summary["open_prs"]),
		"security_items":         coerceInt(summary["security_items"]),
	}

	previousRaw := getAppSetting(resolvedDb, githubRepoHealthLastSummarySetting)
	if previousRaw != "" {
		previousValues := map[string]int{}
		if previous, isDict := safeJsonLoads(previousRaw, map[string]any{}).(map[string]any); isDict {
			previousValues = map[string]int{
				"scanned_repos":          coerceInt(previous["scanned_repos"]),
				"total_repos_considered": coerceInt(previous["total_repos_considered"]),
				"open_issues":            coerceInt(previous["open_issues"]),
				"open_prs":               coerceInt(previous["open_prs"]),
				"security_items":         coerceInt(previous["security_items"]),
			}
		}
		same := len(previousValues) == len(compactValues)
		if same {
			for k, v := range compactValues {
				if previousValues[k] != v {
					same = false
					break
				}
			}
		}
		if same {
			return summary
		}
	}

	setAppSetting(resolvedDb, githubRepoHealthLastSyncSetting, rowString(summary["last_synced_at"]))
	compact := map[string]any{
		"scanned_repos":          compactValues["scanned_repos"],
		"total_repos_considered": compactValues["total_repos_considered"],
		"open_issues":            compactValues["open_issues"],
		"open_prs":               compactValues["open_prs"],
		"security_items":         compactValues["security_items"],
		"last_synced_at":         rowString(summary["last_synced_at"]),
	}
	compactJson, _ := json.Marshal(compact)
	setAppSetting(resolvedDb, githubRepoHealthLastSummarySetting, string(compactJson))
	return summary
}

// startupEnrichment mirrors the @app.before_serving _startup_enrichment hook:
// starts the background CVE scanner and raw metrics window copy worker.
// PORT-NOTE: call from main() after setup (alongside startupHooks()).
// PORT-NOTE: asyncio task handles (_CVE_SCAN_TASK, _RAW_WINDOW_COPY_TASK,
// _GITHUB_REPO_HEALTH_TASK) have no Go equivalent; goroutines run for the
// process lifetime.
func startupEnrichment() {
	go cveScannerLoop()
	go rawWindowCopyLoop()
	go githubRepoHealthLoop()
}

// ---------------------------------------------------------------------------
// Trace metric-context helpers (port of app.py 14606, 14906-15306)
// ---------------------------------------------------------------------------

// tsStrToEpochMs parses a DateTime64 timestamp string to epoch milliseconds.
func tsStrToEpochMs(ts string) float64 {
	ts = strings.TrimSpace(ts)
	if i := strings.Index(ts, "."); i >= 0 {
		base := ts[:i]
		frac := ts[i+1:]
		if len(frac) > 6 {
			frac = frac[:6]
		}
		for len(frac) < 6 {
			frac += "0"
		}
		ts = base + "." + frac
	}
	t, err := parseIsoTimestamp(strings.ReplaceAll(ts, " ", "T"))
	if err != nil {
		logger.Warn("tsStrToEpochMs: could not parse", "ts", ts, "error", err)
		return 0.0
	}
	return float64(t.UnixNano()) / 1e6
}

type metricGroupDef struct {
	key      string
	label    string
	icon     string
	patterns []string
}

var metricGroupDefs = []metricGroupDef{
	{"resource", "Resource Pressure", "bi-cpu", []string{"cpu", "memory", "mem_usage", "node.cpu", "node.memory", "system.cpu", "system.memory"}},
	{"io", "I/O & Storage", "bi-hdd", []string{"blkio", "fs_read", "fs_write", "disk", "network", "bandwidth"}},
	{"k8s", "Kubernetes State", "bi-layers", []string{"kube_pod", "kube_node", "kube_deploy", "pod_phase", "pod_status", "replica", "feature_enabled", "tasks_state"}},
	{"infra", "Infrastructure", "bi-server", []string{"apiserver", "etcd", "scheduler", "controller_manager"}},
}

// groupMetricSeries partitions metric series into labelled display groups.
func groupMetricSeries(series []map[string]any) []map[string]any {
	buckets := map[string][]map[string]any{}
	for _, d := range metricGroupDefs {
		buckets[d.key] = []map[string]any{}
	}
	other := []map[string]any{}
	for _, s := range series {
		m := strings.ToLower(rowString(s["metric"]))
		placed := false
		for _, d := range metricGroupDefs {
			for _, p := range d.patterns {
				if strings.Contains(m, p) {
					buckets[d.key] = append(buckets[d.key], s)
					placed = true
					break
				}
			}
			if placed {
				break
			}
		}
		if !placed {
			other = append(other, s)
		}
	}
	result := []map[string]any{}
	for _, d := range metricGroupDefs {
		if len(buckets[d.key]) > 0 {
			result = append(result, map[string]any{"label": d.label, "icon": d.icon, "key": d.key, "metrics": buckets[d.key]})
		}
	}
	if len(other) > 0 {
		result = append(result, map[string]any{"label": "Other", "icon": "bi-graph-up", "key": "other", "metrics": other})
	}
	return result
}

// computeHealthChips derives at-a-glance health indicator chips.
func computeHealthChips(series []map[string]any) []map[string]any {
	chips := []map[string]any{}
	for _, s := range series {
		m := strings.ToLower(rowString(s["metric"]))
		avg, _ := coerceFloat(s["avg"])
		maxV, _ := coerceFloat(s["max"])
		switch {
		case strings.Contains(m, "cpu") && (strings.Contains(m, "utiliz") || strings.Contains(m, "usage")):
			level := "ok"
			if avg > 80 {
				level = "crit"
			} else if avg > 60 {
				level = "warn"
			}
			chips = append(chips, map[string]any{"label": "CPU", "value": fmt.Sprintf("%.1f%%", avg), "level": level, "icon": "bi-cpu"})
		case strings.Contains(m, "memory_failures") || strings.Contains(m, "mem_failures"):
			level := "ok"
			if maxV > 1000 {
				level = "crit"
			} else if maxV > 0 {
				level = "warn"
			}
			chips = append(chips, map[string]any{"label": "Mem Faults", "value": strconv.Itoa(int(maxV)), "level": level, "icon": "bi-exclamation-triangle"})
		case strings.Contains(m, "memory") && strings.Contains(m, "usage") && !strings.Contains(m, "failures"):
			gb := avg / (1024 * 1024 * 1024)
			valStr := fmt.Sprintf("%.0fMB", avg/1048576)
			if gb >= 0.1 {
				valStr = fmt.Sprintf("%.1fGB", gb)
			}
			chips = append(chips, map[string]any{"label": "Memory", "value": valStr, "level": "ok", "icon": "bi-memory"})
		case strings.Contains(m, "pod_status_phase") || strings.Contains(m, "pod_phase"):
			level := "crit"
			if avg >= 0.9 {
				level = "ok"
			} else if avg >= 0.5 {
				level = "warn"
			}
			chips = append(chips, map[string]any{"label": "Pod Phase", "value": fmt.Sprintf("%.2f", avg), "level": level, "icon": "bi-layers"})
		case strings.Contains(m, "tasks_state"):
			level := "ok"
			if maxV > 0 {
				level = "crit"
			}
			chips = append(chips, map[string]any{"label": "Container Tasks", "value": strconv.Itoa(int(maxV)), "level": level, "icon": "bi-box"})
		}
		if len(chips) >= 6 {
			break
		}
	}
	return chips
}

// fetchTraceMetricContext fetches metric context using ranked matching and a
// raw/pinned fallback. Port of _fetch_trace_metric_context.
func fetchTraceMetricContext(
	db *ChDbConnection,
	serviceNames []string,
	startTs, endTs string,
	windowIds []string,
	limitMetrics int,
	namespaceValues, podValues, nodeValues, deploymentValues []string,
) map[string]any {
	uniq := func(values []string) []string {
		out := []string{}
		seen := map[string]bool{}
		for _, raw := range values {
			v := strings.TrimSpace(raw)
			if v != "" && !seen[v] {
				out = append(out, v)
				seen[v] = true
			}
		}
		return out
	}
	serviceFamilies := func(values []string) []string {
		families := []string{}
		seen := map[string]bool{}
		for _, svc := range values {
			candidate := svc
			if strings.Contains(svc, "-") {
				candidate = svc[:strings.LastIndex(svc, "-")]
			}
			candidate = strings.TrimSpace(candidate)
			if candidate != "" && !seen[candidate] {
				families = append(families, candidate)
				seen[candidate] = true
			}
		}
		return families
	}
	attrClause := func(primaryKey, legacyKey string, values []string) (string, []any) {
		if len(values) == 0 {
			return "", nil
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(values)), ",")
		paramsLocal := make([]any, 0, len(values)*2)
		for _, v := range values {
			paramsLocal = append(paramsLocal, v)
		}
		clause := fmt.Sprintf("Attributes['%s'] IN (%s)", primaryKey, placeholders)
		if legacyKey != "" && legacyKey != primaryKey {
			clause = fmt.Sprintf("(%s OR Attributes['%s'] IN (%s))", clause, legacyKey, placeholders)
			for _, v := range values {
				paramsLocal = append(paramsLocal, v)
			}
		}
		return clause, paramsLocal
	}

	startMsNorm := int64(tsStrToEpochMs(startTs))
	endMsNorm := int64(tsStrToEpochMs(endTs))
	queryStartTs := startTs
	queryEndTs := endTs
	if endMsNorm > startMsNorm && startMsNorm > 0 {
		queryStartTs = time.UnixMilli(startMsNorm).UTC().Format("2006-01-02 15:04:05.000000")
		queryEndTs = time.UnixMilli(endMsNorm).UTC().Format("2006-01-02 15:04:05.000000")
	}

	queryTimeseries := func(extraClauses []string, extraParams []any, topMetricNames []string, timeParseMode string, numBuckets int) map[string]any {
		if len(topMetricNames) == 0 {
			return map[string]any{"ticks_ms": []any{}, "by_metric": map[string]any{}}
		}
		startMsInt := int64(tsStrToEpochMs(startTs))
		endMsInt := int64(tsStrToEpochMs(endTs))
		if endMsInt <= startMsInt {
			return map[string]any{"ticks_ms": []any{}, "by_metric": map[string]any{}}
		}
		durationMs := endMsInt - startMsInt
		bucketMs := durationMs / int64(numBuckets)
		if bucketMs < 1 {
			bucketMs = 1
		}
		ticksMs := make([]any, 0, numBuckets)
		for i := 0; i < numBuckets; i++ {
			ticksMs = append(ticksMs, int64(float64(startMsInt)+(float64(i)+0.5)*float64(bucketMs)))
		}
		metricPhs := strings.TrimSuffix(strings.Repeat("?,", len(topMetricNames)), ",")
		parseModes := []string{"utc", "default"}
		if timeParseMode == "default" {
			parseModes = []string{"default", "utc"}
		}
		var tsRows []Row
		for _, mode := range parseModes {
			var startClause, endClause string
			if mode == "utc" {
				startClause = "TimeUnix >= parseDateTime64BestEffort(?, 9, 'UTC')"
				endClause = "TimeUnix <= parseDateTime64BestEffort(?, 9, 'UTC')"
			} else {
				startClause = "TimeUnix >= parseDateTime64BestEffort(?, 9)"
				endClause = "TimeUnix <= parseDateTime64BestEffort(?, 9)"
			}
			tsWhereParts := append([]string{startClause, endClause, fmt.Sprintf("MetricName IN (%s)", metricPhs)}, extraClauses...)
			tsWhereSql := strings.Join(tsWhereParts, " AND ")
			tsParams := []any{queryStartTs, queryEndTs}
			for _, mn := range topMetricNames {
				tsParams = append(tsParams, mn)
			}
			tsParams = append(tsParams, extraParams...)
			tsDedup := fmt.Sprintf(
				"SELECT MetricName, TimeUnix, argMin(Value, SourceRank) AS Value "+
					"FROM v_otel_metrics_dedup WHERE %s "+
					"GROUP BY MetricName, TimeUnix, AttrFingerprint", tsWhereSql)
			res, err := db.Execute(
				fmt.Sprintf("SELECT MetricName, "+
					"intDiv(toUnixTimestamp64Milli(TimeUnix) - %d, %d) AS BucketIdx, "+
					"round(avg(Value), 6) AS AvgVal "+
					"FROM (%s) AS src "+
					"WHERE BucketIdx >= 0 AND BucketIdx < %d "+
					"GROUP BY MetricName, BucketIdx "+
					"ORDER BY MetricName, BucketIdx", startMsInt, bucketMs, tsDedup, numBuckets),
				tsParams...,
			)
			if err != nil {
				continue
			}
			tsRows = res.Fetchall()
			if len(tsRows) > 0 {
				break
			}
		}
		byMetric := map[string]any{}
		for _, mn := range topMetricNames {
			// ponytail: typed nil (*float64), not untyped any-nil. Empty buckets are
			// chart gaps (tojson → null); but gonja eagerly stringifies this whole
			// dict on any `.get(...)` call, and an untyped-nil slice element resolves
			// to a zero reflect.Value that panics in Value.String(). A typed nil
			// pointer stays a valid reflect.Value and still marshals to JSON null.
			buckets := make([]any, numBuckets)
			for i := range buckets {
				buckets[i] = (*float64)(nil)
			}
			byMetric[mn] = buckets
		}
		for _, r := range tsRows {
			mname := rowString(r["MetricName"])
			idx := coerceInt(r["BucketIdx"])
			if arr, ok := byMetric[mname].([]any); ok && idx >= 0 && idx < numBuckets {
				v, _ := coerceFloat(r["AvgVal"])
				arr[idx] = v
			}
		}
		return map[string]any{"ticks_ms": ticksMs, "by_metric": byMetric}
	}

	query := func(extraClauses []string, extraParams []any) map[string]any {
		for _, timeParseMode := range []string{"utc", "default"} {
			var startClause, endClause string
			if timeParseMode == "utc" {
				startClause = "TimeUnix >= parseDateTime64BestEffort(?, 9, 'UTC')"
				endClause = "TimeUnix <= parseDateTime64BestEffort(?, 9, 'UTC')"
			} else {
				startClause = "TimeUnix >= parseDateTime64BestEffort(?, 9)"
				endClause = "TimeUnix <= parseDateTime64BestEffort(?, 9)"
			}
			whereParts := append([]string{startClause, endClause}, extraClauses...)
			params := []any{queryStartTs, queryEndTs}
			params = append(params, extraParams...)
			whereSql := strings.Join(whereParts, " AND ")
			dedupSubquerySql := fmt.Sprintf(
				"SELECT ServiceName, MetricName, AttrFingerprint, TimeUnix, "+
					"argMin(Value, SourceRank) AS Value, min(SourceRank) AS DedupRank "+
					"FROM v_otel_metrics_dedup WHERE %s "+
					"GROUP BY ServiceName, MetricName, AttrFingerprint, TimeUnix", whereSql)
			statsRes, err := db.Execute(
				fmt.Sprintf("SELECT count() AS c, min(DedupRank) AS min_rank, max(DedupRank) AS max_rank "+
					"FROM (%s) AS dedup", dedupSubquerySql), params...)
			if err != nil {
				continue
			}
			statsRow := statsRes.Fetchone()
			totalPoints := 0
			minRank := 1
			maxRank := 1
			if statsRow != nil {
				totalPoints = coerceInt(statsRow["c"])
				minRank = coerceInt(statsRow["min_rank"])
				maxRank = coerceInt(statsRow["max_rank"])
			}
			if totalPoints <= 0 {
				continue
			}
			sourceMode := "mixed"
			if minRank == 0 && maxRank == 0 {
				sourceMode = "raw"
			} else if minRank == 1 && maxRank == 1 {
				sourceMode = "pinned"
			}
			lim := limitMetrics
			if lim < 1 {
				lim = 1
			}
			if lim > 50 {
				lim = 50
			}
			rowsParams := append(append([]any{}, params...), lim)
			rowsRes, err := db.Execute(
				fmt.Sprintf("SELECT ServiceName, MetricName, count() AS points, "+
					"round(avg(Value), 4) AS avg_value, "+
					"round(min(Value), 4) AS min_value, "+
					"round(max(Value), 4) AS max_value "+
					"FROM (%s) AS dedup "+
					"GROUP BY ServiceName, MetricName "+
					"ORDER BY points DESC, MetricName ASC "+
					"LIMIT ?", dedupSubquerySql), rowsParams...)
			if err != nil {
				continue
			}
			series := []map[string]any{}
			for _, r := range rowsRes.Fetchall() {
				avgV, _ := coerceFloat(r["avg_value"])
				minV, _ := coerceFloat(r["min_value"])
				maxV, _ := coerceFloat(r["max_value"])
				series = append(series, map[string]any{
					"service": rowString(r["ServiceName"]),
					"metric":  rowString(r["MetricName"]),
					"points":  coerceInt(r["points"]),
					"avg":     avgV,
					"min":     minV,
					"max":     maxV,
				})
			}
			return map[string]any{
				"source_mode":     sourceMode,
				"total_points":    totalPoints,
				"series":          series,
				"time_parse_mode": timeParseMode,
			}
		}
		return map[string]any{"source_mode": "none", "total_points": 0, "series": []map[string]any{}, "time_parse_mode": "none"}
	}

	_ = windowIds // kept for API compatibility; raw SQL path intentionally ignores this
	traceServices := uniq(serviceNames)
	traceNamespaces := uniq(namespaceValues)
	tracePods := uniq(podValues)
	traceNodes := uniq(nodeValues)
	traceDeployments := uniq(deploymentValues)
	families := serviceFamilies(traceServices)

	type metricAttempt struct {
		mode, label string
		clauses     []string
		params      []any
		dimensions  []string
	}
	attempts := []metricAttempt{}

	nsClause, nsParams := attrClause("k8s.namespace.name", "namespace", traceNamespaces)
	podClause, podParams := attrClause("k8s.pod.name", "pod", tracePods)
	nodeClause, nodeParams := attrClause("k8s.node.name", "node", traceNodes)
	deployClause, deployParams := attrClause("k8s.deployment.name", "deployment", traceDeployments)

	if nsClause != "" && podClause != "" {
		attempts = append(attempts, metricAttempt{"pod_exact", "pod + namespace", []string{nsClause, podClause}, append(append([]any{}, nsParams...), podParams...), []string{"namespace", "pod"}})
	}
	if nsClause != "" && nodeClause != "" {
		attempts = append(attempts, metricAttempt{"node_namespace", "node + namespace", []string{nsClause, nodeClause}, append(append([]any{}, nsParams...), nodeParams...), []string{"namespace", "node"}})
	}
	if nsClause != "" && deployClause != "" {
		attempts = append(attempts, metricAttempt{"deployment_namespace", "deployment + namespace", []string{nsClause, deployClause}, append(append([]any{}, nsParams...), deployParams...), []string{"namespace", "deployment"}})
	}
	if len(traceServices) > 0 {
		svcPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(traceServices)), ",")
		svcParams := make([]any, 0, len(traceServices))
		for _, s := range traceServices {
			svcParams = append(svcParams, s)
		}
		attempts = append(attempts, metricAttempt{"service_exact", "service exact", []string{fmt.Sprintf("ServiceName IN (%s)", svcPlaceholders)}, svcParams, []string{"service"}})
	}
	if len(families) > 0 {
		famPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(families)), ",")
		clause := fmt.Sprintf("(ServiceName IN (%s) OR Attributes['service.name'] IN (%s) OR Attributes['service'] IN (%s))", famPlaceholders, famPlaceholders, famPlaceholders)
		famParams := make([]any, 0, len(families)*3)
		for i := 0; i < 3; i++ {
			for _, f := range families {
				famParams = append(famParams, f)
			}
		}
		attempts = append(attempts, metricAttempt{"service_family", "service family", []string{clause}, famParams, []string{"service_family"}})
	}
	attempts = append(attempts, metricAttempt{"time_window_only", "time window only", []string{}, []any{}, []string{"time_window"}})

	for _, att := range attempts {
		ctx := query(att.clauses, att.params)
		if coerceInt(ctx["total_points"]) <= 0 {
			continue
		}
		ctx["match_mode"] = att.mode
		ctx["match_label"] = att.label
		dims := make([]any, 0, len(att.dimensions))
		for _, d := range att.dimensions {
			dims = append(dims, d)
		}
		ctx["match_dimensions"] = dims
		rawSeries, _ := ctx["series"].([]map[string]any)
		topNames := []string{}
		for i, s := range rawSeries {
			if i >= 6 {
				break
			}
			topNames = append(topNames, rowString(s["metric"]))
		}
		cpuMetric := ""
		for _, s := range rawSeries {
			mn := rowString(s["metric"])
			if strings.Contains(strings.ToLower(mn), "cpu") {
				cpuMetric = mn
				break
			}
		}
		finalTopNames := topNames
		if cpuMetric != "" {
			inTop := false
			for _, n := range topNames {
				if n == cpuMetric {
					inTop = true
					break
				}
			}
			if !inTop {
				finalTopNames = append([]string{cpuMetric}, topNames...)
			} else {
				filtered := []string{cpuMetric}
				for _, n := range topNames {
					if n != cpuMetric {
						filtered = append(filtered, n)
					}
				}
				finalTopNames = filtered
			}
		}
		timeParseMode := rowString(ctx["time_parse_mode"])
		if timeParseMode == "" {
			timeParseMode = "utc"
		}
		ctx["timeseries"] = queryTimeseries(att.clauses, att.params, finalTopNames, timeParseMode, 24)
		ctx["metric_groups"] = groupMetricSeries(rawSeries)
		healthChips := computeHealthChips(rawSeries)
		ctx["health_chips"] = healthChips
		var headerChip any
		for _, c := range healthChips {
			if strings.Contains(rowString(c["label"]), "CPU") {
				headerChip = c
				break
			}
		}
		ctx["header_chip"] = headerChip
		return ctx
	}

	return map[string]any{
		"source_mode":      "none",
		"total_points":     0,
		"series":           []map[string]any{},
		"match_mode":       "none",
		"match_label":      "no match",
		"match_dimensions": []any{},
	}
}
