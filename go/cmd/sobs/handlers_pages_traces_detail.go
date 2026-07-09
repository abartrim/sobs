package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Trace-detail waterfall port of app.py view_traces' populated trace_id branch and its six
// helpers (_build_span_tree, _slice_span_tree_with_ancestors, _compute_active_timeline_ms,
// _merge_span_intervals, _build_trace_timeline_segments, _build_trace_window_overlay_segments).
// The empty fixture never has a trace, so handleViewTraces still renders the nil-stub there;
// this builds the same trace_detail dict the template expects once a trace has spans.

const (
	traceDetailHardCap           = 5000 // _TRACE_DETAIL_HARD_CAP
	traceDetailDefaultLimit      = 200  // _TRACE_DETAIL_DEFAULT_LIMIT
	traceDetailMaxLimit          = 1000 // _TRACE_DETAIL_MAX_LIMIT
	traceDetailCollapseThreshold = 300  // _TRACE_DETAIL_COLLAPSE_THRESHOLD
	traceDetailErrorLimit        = 50   // _TRACE_ERROR_LIMIT
)

// spanDict narrows an []any element to its underlying map (spans are always maps here).
func spanDict(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// buildSpanTree mirrors app.py _build_span_tree: spans ordered depth-first with `depth` and
// `has_children`. Children/roots are sorted by `ts` (stable, preserving the SQL tie order).
func buildSpanTree(spans []any) []any {
	byID := make(map[string]bool, len(spans))
	for _, sv := range spans {
		byID[toStr(spanDict(sv)["span_id"])] = true
	}
	children := map[string][]any{}
	roots := []any{}
	for _, sv := range spans {
		pid := toStr(spanDict(sv)["parent_span_id"])
		if pid != "" && byID[pid] {
			children[pid] = append(children[pid], sv)
		} else {
			roots = append(roots, sv)
		}
	}
	for pid := range children {
		lst := children[pid]
		sort.SliceStable(lst, func(i, j int) bool {
			return toStr(spanDict(lst[i])["ts"]) < toStr(spanDict(lst[j])["ts"])
		})
		children[pid] = lst
	}
	sort.SliceStable(roots, func(i, j int) bool {
		return toStr(spanDict(roots[i])["ts"]) < toStr(spanDict(roots[j])["ts"])
	})
	type frame struct {
		span  any
		depth int
	}
	stack := make([]frame, 0, len(spans))
	for i := len(roots) - 1; i >= 0; i-- {
		stack = append(stack, frame{roots[i], 0})
	}
	result := []any{}
	for len(stack) > 0 {
		fr := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		s := spanDict(fr.span)
		spanID := toStr(s["span_id"])
		_, hasChildren := children[spanID]
		row := make(map[string]any, len(s)+2)
		for k, v := range s {
			row[k] = v
		}
		row["depth"] = fr.depth
		row["has_children"] = hasChildren
		result = append(result, row)
		kids := children[spanID]
		for i := len(kids) - 1; i >= 0; i-- {
			stack = append(stack, frame{kids[i], fr.depth + 1})
		}
	}
	return result
}

// sliceSpanTreeWithAncestors mirrors app.py _slice_span_tree_with_ancestors: a paged DFS slice
// plus the ancestors required for context. Returns (rows, pageEnd, contextRows).
func sliceSpanTreeWithAncestors(fullSpanTree []any, offset, limit int) ([]any, int, int) {
	if len(fullSpanTree) == 0 {
		return []any{}, 0, 0
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
		return []any{}, pageEnd, 0
	}
	byID := make(map[string]any, total)
	for _, rv := range fullSpanTree {
		byID[toStr(spanDict(rv)["span_id"])] = rv
	}
	included := map[string]bool{}
	for _, rv := range pageRows {
		included[toStr(spanDict(rv)["span_id"])] = true
	}
	for _, rv := range pageRows {
		parentID := toStr(spanDict(rv)["parent_span_id"])
		for parentID != "" {
			pv, ok := byID[parentID]
			if !ok || included[parentID] {
				break
			}
			included[parentID] = true
			parentID = toStr(spanDict(pv)["parent_span_id"])
		}
	}
	rows := []any{}
	for _, rv := range fullSpanTree {
		if included[toStr(spanDict(rv)["span_id"])] {
			rows = append(rows, rv)
		}
	}
	contextRows := len(rows) - len(pageRows)
	if contextRows < 0 {
		contextRows = 0
	}
	return rows, pageEnd, contextRows
}

// mergeSpanIntervals mirrors app.py _merge_span_intervals: span start/end intervals merged,
// sorted by start time. Each interval is [start_ms, end_ms].
func mergeSpanIntervals(spans []any) [][2]float64 {
	if len(spans) == 0 {
		return nil
	}
	intervals := make([][2]float64, 0, len(spans))
	for _, sv := range spans {
		s := spanDict(sv)
		startMs := cFloat(s, "start_ms")
		durMs := cFloat(s, "duration_ms")
		if durMs < 0 {
			durMs = 0
		}
		intervals = append(intervals, [2]float64{startMs, startMs + durMs})
	}
	sort.SliceStable(intervals, func(i, j int) bool {
		return intervals[i][0] < intervals[j][0]
	})
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

// computeActiveTimelineMs mirrors app.py _compute_active_timeline_ms.
func computeActiveTimelineMs(spans []any) float64 {
	total := 0.0
	for _, iv := range mergeSpanIntervals(spans) {
		if d := iv[1] - iv[0]; d > 0 {
			total += d
		}
	}
	return total
}

// traceSpanBounds returns (start_ms, end_ms, total_ms) over span intervals, mirroring the
// min/max + max(...,1.0) idiom shared by the timeline/window-overlay builders.
func traceSpanBounds(spans []any) (float64, float64, float64) {
	startMs := math.Inf(1)
	endMs := math.Inf(-1)
	for _, sv := range spans {
		s := spanDict(sv)
		st := cFloat(s, "start_ms")
		du := cFloat(s, "duration_ms")
		if du < 0 {
			du = 0
		}
		if st < startMs {
			startMs = st
		}
		if st+du > endMs {
			endMs = st + du
		}
	}
	totalMs := endMs - startMs
	if totalMs < 1.0 {
		totalMs = 1.0
	}
	return startMs, endMs, totalMs
}

// buildTraceTimelineSegments mirrors app.py _build_trace_timeline_segments: active/gap segments
// over the trace window, flagging gaps that overlap recorded activity timestamps.
func buildTraceTimelineSegments(spans []any, activityTsMs []float64) []any {
	if len(spans) == 0 {
		return []any{}
	}
	traceStartMs, traceEndMs, traceTotalMs := traceSpanBounds(spans)
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

	segments := []any{}
	cursor := traceStartMs
	for _, iv := range merged {
		startMs, endMs := iv[0], iv[1]
		if startMs > cursor {
			gapWidthPct := toPct(startMs) - toPct(cursor)
			if gapWidthPct > 0 {
				segments = append(segments, map[string]any{
					"kind":      "gap",
					"start_pct": roundHalfEven(toPct(cursor), 3),
					"width_pct": roundHalfEven(gapWidthPct, 3),
					"potential": hasGapActivity(cursor, startMs),
				})
			}
		}
		activeWidthPct := toPct(endMs) - toPct(startMs)
		if activeWidthPct > 0 {
			segments = append(segments, map[string]any{
				"kind":      "active",
				"start_pct": roundHalfEven(toPct(startMs), 3),
				"width_pct": roundHalfEven(activeWidthPct, 3),
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
				"start_pct": roundHalfEven(toPct(cursor), 3),
				"width_pct": roundHalfEven(gapWidthPct, 3),
				"potential": hasGapActivity(cursor, traceEndMs),
			})
		}
	}
	return segments
}

// buildTraceWindowOverlaySegments mirrors app.py _build_trace_window_overlay_segments: preserved
// raw-window overlay segments aligned to the trace timeline axis, sorted by start_pct.
func buildTraceWindowOverlaySegments(spans []any, windows []any) []any {
	if len(spans) == 0 || len(windows) == 0 {
		return []any{}
	}
	traceStartMs, traceEndMs, traceTotalMs := traceSpanBounds(spans)
	toPct := func(v float64) float64 { return (v - traceStartMs) / traceTotalMs * 100.0 }

	segments := []any{}
	for _, wv := range windows {
		w := spanDict(wv)
		ws := tsStrToEpochMs(toStr(w["window_start"]))
		we := tsStrToEpochMs(toStr(w["window_end"]))
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
		copiedCount := intStrOrZero(w["copied_count"])
		expectedCount := intStrOrZero(w["expected_count"])
		copyComplete, _ := w["copy_complete"].(bool)
		signalType := toStr(w["signal_type"])
		signalRef := toStr(w["signal_ref"])
		title := signalType
		if title == "" {
			title = "window"
		}
		if signalRef != "" {
			title += " (" + signalRef + ")"
		}
		title += " [" + strconv.Itoa(copiedCount) + "/" + strconv.Itoa(expectedCount) + "]"
		segments = append(segments, map[string]any{
			"start_pct":     roundHalfEven(startPct, 3),
			"width_pct":     roundHalfEven(widthPct, 3),
			"copy_complete": copyComplete,
			"title":         title,
		})
	}
	sort.SliceStable(segments, func(i, j int) bool {
		a, _ := spanDict(segments[i])["start_pct"].(float64)
		b, _ := spanDict(segments[j])["start_pct"].(float64)
		return a < b
	})
	return segments
}

// intStrOrZero mirrors Python int(str(value or 0)) for the window copy counters.
func intStrOrZero(v any) int {
	if v == nil {
		return 0
	}
	if n, ok := iStr(v); ok {
		return n
	}
	return 0
}

// sortedTraceDims mirrors sorted({str(s.get(field) or "").strip() for s in spans if s.get(field)}).
func sortedTraceDims(spans []any, field string) []string {
	set := map[string]bool{}
	for _, sv := range spans {
		raw := toStr(spanDict(sv)[field])
		if raw == "" { // Python skips falsy s.get(field)
			continue
		}
		set[strings.TrimSpace(raw)] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// epochMsToISOUTC mirrors datetime.fromtimestamp(ms/1000.0, tz=timezone.utc).isoformat().
func epochMsToISOUTC(ms float64) string {
	micros := int64(math.Round(ms * 1000.0))
	t := time.UnixMicro(micros).UTC()
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05+00:00")
	}
	return t.Format("2006-01-02T15:04:05.000000+00:00")
}

// buildTraceDetail mirrors the enriched trace_detail block in app.py view_traces (the
// `if trace_id and not time_error:` branch). Returns (trace_detail, total, work_item_links);
// trace_detail is nil when the trace has no spans (total 0, empty work_item_links), matching
// the Python None branch.
func (s *server) buildTraceDetail(traceID string, traceSpanLimit, traceSpanOffset int) (any, int, map[string]any) {
	traceTotalSpans := 0
	if res, err := s.db.Execute("SELECT COUNT(*) AS c FROM otel_traces WHERE TraceId=?", traceID); err == nil && len(res.Rows) > 0 {
		traceTotalSpans = cInt(rowMaps(res)[0], "c")
	}
	detailFetchLimit := traceTotalSpans
	if detailFetchLimit > traceDetailHardCap {
		detailFetchLimit = traceDetailHardCap
	}
	detailRes, err := s.db.Execute(
		"SELECT Timestamp, TraceId, SpanId, ParentSpanId, SpanName, ServiceName, "+
			"Duration, StatusCode, SpanAttributes "+
			"FROM otel_traces WHERE TraceId=? ORDER BY Timestamp ASC, SpanId ASC LIMIT ?",
		traceID, detailFetchLimit)
	if err != nil {
		return nil, 0, map[string]any{}
	}
	detailRows := rowMaps(detailRes)
	if len(detailRows) == 0 {
		return nil, 0, map[string]any{}
	}

	allTraceSpans := make([]any, 0, len(detailRows))
	for _, m := range detailRows {
		attrs := mapToDict(m["SpanAttributes"])
		tsStr := cStr(m, "Timestamp")
		allTraceSpans = append(allTraceSpans, map[string]any{
			"ts":             tsStr,
			"trace_id":       toStr(m["TraceId"]),
			"span_id":        toStr(m["SpanId"]),
			"parent_span_id": toStr(m["ParentSpanId"]),
			"name":           toStr(m["SpanName"]),
			"service":        toStr(m["ServiceName"]),
			"start_ms":       tsStrToEpochMs(tsStr),
			"duration_ms":    roundHalfEven(cFloat(m, "Duration")/1_000_000, 2),
			"status":         toStr(m["StatusCode"]),
			"http_method":    attrGet(attrs, "http.method", "http.request.method"),
			"http_url":       attrGet(attrs, "http.url", "url.full"),
			"http_status":    attrGet(attrs, "http.status_code", "http.response.status_code"),
			"namespace":      attrGet(attrs, "k8s.namespace.name", "namespace"),
			"pod":            attrGet(attrs, "k8s.pod.name", "pod"),
			"node":           attrGet(attrs, "k8s.node.name", "node"),
			"deployment":     attrGet(attrs, "k8s.deployment.name", "deployment"),
		})
	}

	// Relative timeline positions.
	traceStartMs, traceEndMs, traceTotalMs := traceSpanBounds(allTraceSpans)
	traceActiveMs := computeActiveTimelineMs(allTraceSpans)
	traceCoveragePct := traceActiveMs / traceTotalMs * 100.0
	if traceCoveragePct > 100.0 {
		traceCoveragePct = 100.0
	}
	if traceCoveragePct < 0.0 {
		traceCoveragePct = 0.0
	}
	traceSpanSumMs := 0.0
	for _, sv := range allTraceSpans {
		if d := cFloat(spanDict(sv), "duration_ms"); d > 0 {
			traceSpanSumMs += d
		}
	}
	for _, sv := range allTraceSpans {
		s2 := spanDict(sv)
		st := cFloat(s2, "start_ms")
		du := cFloat(s2, "duration_ms")
		s2["offset_pct"] = roundHalfEven((st-traceStartMs)/traceTotalMs*100, 2)
		w := du / traceTotalMs * 100
		if w < 0.5 {
			w = 0.5
		}
		s2["width_pct"] = roundHalfEven(w, 2)
	}

	// Related errors for this trace (capped at 50; flag truncation).
	traceErrors := []any{}
	errorsTruncated := false
	traceActivityTsMs := []float64{}
	errSQL := "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes, ErrorId, " +
		"(ErrorId IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)) AS IsResolved " +
		"FROM (" +
		"SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes, " +
		errorIDExpr + " AS ErrorId " +
		"FROM (" + errorSourcesSQL + ") WHERE TraceId=? LIMIT ?" +
		")"
	if errRes, eerr := s.db.Execute(errSQL, traceID, traceDetailErrorLimit+1); eerr == nil {
		erows := rowMaps(errRes)
		if len(erows) > traceDetailErrorLimit {
			errorsTruncated = true
			erows = erows[:traceDetailErrorLimit]
		}
		for _, row := range erows {
			item := s.buildErrorItem(row)
			if eid := toStr(row["ErrorId"]); eid != "" {
				item["id"] = eid
			}
			item["resolved"] = cBool(row, "IsResolved")
			traceErrors = append(traceErrors, item)
			if tsRaw := toStr(item["ts"]); tsRaw != "" {
				traceActivityTsMs = append(traceActivityTsMs, tsStrToEpochMs(tsRaw))
			}
		}
	}

	// error_span_ids: {e["span_id"] for e in trace_errors if e.get("span_id")} (dedup, membership-only).
	errorSpanIDs := []any{}
	seenSpan := map[string]bool{}
	for _, ev := range traceErrors {
		sid := toStr(spanDict(ev)["span_id"])
		if sid != "" && !seenSpan[sid] {
			seenSpan[sid] = true
			errorSpanIDs = append(errorSpanIDs, sid)
		}
	}

	// Log counts per span + activity timestamps.
	logCounts := map[string]any{}
	if lcRes, lcErr := s.db.Execute(
		"SELECT SpanId, count() AS cnt FROM otel_logs WHERE TraceId=? AND SpanId!='' GROUP BY SpanId",
		traceID); lcErr == nil {
		for _, r := range rowMaps(lcRes) {
			logCounts[cStr(r, "SpanId")] = cInt(r, "cnt")
		}
	}
	if ltRes, ltErr := s.db.Execute(
		"SELECT Timestamp FROM otel_logs WHERE TraceId=? LIMIT 2000", traceID); ltErr == nil {
		for _, r := range rowMaps(ltRes) {
			traceActivityTsMs = append(traceActivityTsMs, tsStrToEpochMs(cStr(r, "Timestamp")))
		}
	}

	timelineSegments := buildTraceTimelineSegments(allTraceSpans, traceActivityTsMs)
	hasPotentialGap := false
	for _, segv := range timelineSegments {
		seg := spanDict(segv)
		if toStr(seg["kind"]) == "gap" {
			if b, _ := seg["potential"].(bool); b {
				hasPotentialGap = true
				break
			}
		}
	}

	// Anomaly state for the primary service.
	var traceAnomalyState any = nil
	svc := ""
	if len(allTraceSpans) > 0 {
		svc = toStr(spanDict(allTraceSpans[0])["service"])
	}
	if svc != "" {
		if aRes, aErr := s.db.Execute(
			"SELECT anomaly_state FROM v_derived_signals_anomaly "+
				"WHERE ServiceName=? AND SignalSource='traces' "+
				"AND time >= now() - INTERVAL 48 HOUR "+
				"ORDER BY time DESC LIMIT 1", svc); aErr == nil {
			arows := rowMaps(aRes)
			if len(arows) > 0 {
				traceAnomalyState = cStr(arows[0], "anomaly_state")
			}
		}
	}

	// Overlapping preserved raw windows.
	traceServices := sortedTraceDims(allTraceSpans, "service")
	traceStartTS := epochMsToISOUTC(traceStartMs)
	traceEndTS := epochMsToISOUTC(traceEndMs)
	traceWindows := s.listTraceOverlappingRawWindows(traceServices, traceStartTS, traceEndTS, 25)

	// Metric retention context (±5 min around the trace).
	const metricPadMs = 5 * 60 * 1000.0
	metricCtxStartTS := epochMsToISOUTC(traceStartMs - metricPadMs)
	metricCtxEndTS := epochMsToISOUTC(traceEndMs + metricPadMs)
	traceNamespaces := sortedTraceDims(allTraceSpans, "namespace")
	tracePods := sortedTraceDims(allTraceSpans, "pod")
	traceNodes := sortedTraceDims(allTraceSpans, "node")
	traceDeployments := sortedTraceDims(allTraceSpans, "deployment")
	windowIDs := []string{}
	for _, wv := range traceWindows {
		if id := toStr(spanDict(wv)["id"]); id != "" {
			windowIDs = append(windowIDs, id)
		}
	}
	traceMetricsContext := s.fetchTraceMetricContext(
		traceServices, metricCtxStartTS, metricCtxEndTS, windowIDs, 12,
		traceNamespaces, tracePods, traceNodes, traceDeployments)

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

	traceDetail := map[string]any{
		"span_tree":           tracePageSpans,
		"trace_start_ts":      toStr(spanDict(allTraceSpans[0])["ts"]),
		"trace_end_ts":        toStr(spanDict(allTraceSpans[len(allTraceSpans)-1])["ts"]),
		"trace_start_ms":      int(roundHalfEven(traceStartMs, 0)),
		"trace_end_ms":        int(roundHalfEven(traceEndMs, 0)),
		"errors":              traceErrors,
		"errors_truncated":    errorsTruncated,
		"error_span_ids":      errorSpanIDs,
		"log_counts":          logCounts,
		"anomaly_state":       traceAnomalyState,
		"total_ms":            roundHalfEven(traceTotalMs, 2),
		"active_ms":           roundHalfEven(traceActiveMs, 2),
		"coverage_pct":        roundHalfEven(traceCoveragePct, 2),
		"span_sum_ms":         roundHalfEven(traceSpanSumMs, 2),
		"timeline_segments":   timelineSegments,
		"has_potential_gap":   hasPotentialGap,
		"raw_windows":         traceWindows,
		"raw_window_segments": traceWindowSegments,
		"metrics_context":     traceMetricsContext,
		"total_spans":         traceTotalSpans,
		"capped_total_spans":  cappedTotalSpans,
		"hard_cap":            traceDetailHardCap,
		"hard_capped":         traceTotalSpans > traceDetailHardCap,
		"default_collapsed":   cappedTotalSpans > traceDetailCollapseThreshold,
		"page_limit":          traceSpanLimit,
		"page_offset":         traceSpanOffset,
		"page_end":            tracePageEnd,
		"context_rows":        traceContextRows,
		"prev_offset":         detailPrevOffset,
		"next_offset":         detailNextOffset,
		"has_prev_page":       traceSpanOffset > 0,
		"has_next_page":       detailNextOffset < cappedTotalSpans,
	}

	// Work-item pre-check map for trace-detail errors (ref_ids = error ids + error trace_ids +
	// this trace_id). Empty when no work item references these.
	refSet := map[string]bool{}
	refIDs := []string{}
	addRef := func(v string) {
		if v != "" && !refSet[v] {
			refSet[v] = true
			refIDs = append(refIDs, v)
		}
	}
	for _, ev := range traceErrors {
		e := spanDict(ev)
		addRef(toStr(e["id"]))
		addRef(toStr(e["trace_id"]))
	}
	if traceID != "" {
		refIDs = append(refIDs, traceID)
	}
	workItemLinks := s.loadWorkItemLinksForRefIDs(refIDs)

	return traceDetail, traceTotalSpans, workItemLinks
}
