// This file is the golden-corpus time-shift utility: it keeps the frozen fixture rows in
// testdata/fixtures/seeds.tar.gz looking "recent" to production handlers that scope
// queries off real wall-clock now() (regexValidateRecentHours, the auto/seasonal metric
// candidate windows, getSignalHealthByService, etc.), without which those rows silently
// age out of "recent" windows as real time elapses past the corpus's 2026-07-05 capture
// session.
//
// This is deliberately kept as its own utility, separate from the seeding/insert mechanics
// in main.go: the golden-corpus harness is expected to be retired over time as the Go
// server diverges from byte-parity with the frozen Python oracle, but that's a gradual,
// case-by-case migration to conventional Go tests — not a single cutover. Until every
// profile that depends on this shift is gone, this file is the one place that logic lives,
// so it can keep being extended (e.g. new shiftable fields for a new table) without
// spreading date-arithmetic across the seeder.
package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// referenceCaptureTime anchors the golden corpus to the LATEST wall-clock moment
// migration/tools/dump_profile_seeds.py captured any frozen fixture row in the capture
// session (the otel_logs/otel_traces/hyperdx_sessions/otel_metrics_*/otel_metrics_1m_agg
// Timestamp/TimestampTime/TimeUnix/TimeUnixMs/MinuteBucket values in
// testdata/fixtures/seeds.tar.gz that belong to this session cluster in a ~90min span
// ending 2026-07-05 11:30:00 UTC — see captureWindow below).
//
// This MUST be the max, not some midpoint of the session: anchoring to an earlier point
// and shifting by "time since anchor" pushes every row captured AFTER that anchor into
// the future relative to "now" (e.g. anchoring at the session's midpoint once pushed a
// row up to ~30min into the future, which silently broke a "last N hours" window's
// implicit assumption that data is never timestamped later than now() — see the
// summaryrich/tracewindows regression this comment replaced). Anchoring to the max
// guarantees shift(t) <= now() for every in-session row.
//
// Never re-run dump_profile_seeds.py; just bump this constant (to the new max) if the
// corpus is ever recaptured.
var referenceCaptureTime = time.Date(2026, 7, 5, 11, 30, 0, 0, time.UTC)

// captureWindow bounds which rows are considered part of the capture session (and thus
// eligible for the shift below) versus rows that pin a deliberately arbitrary, unrelated
// calendar date (many profiles — e.g. aiview, tracedetail, tracesrich, incidentmatch —
// seed rows dated 2020/2023/2024 purely so the golden HTML/JSON can byte-compare an
// exact rendered timestamp; those must NEVER move). The real capture span is under an
// hour (10:04-11:01), so a generous ±24h window around referenceCaptureTime cleanly
// separates "this session's data" from every pinned historical fixture without needing
// an exact per-profile allowlist.
const captureWindow = 24 * time.Hour

// dateTime64Layout/dateTimeLayout match ClickHouse's default JSONEachRow text rendering
// for DateTime64(9) and DateTime columns respectively.
const (
	dateTime64Layout = "2006-01-02 15:04:05.000000000"
	dateTimeLayout   = "2006-01-02 15:04:05"
)

// shiftableFields lists every top-level JSONEachRow field, across every seeded table, whose
// value is a capture-session wall-clock timestamp that must move in lockstep with
// referenceCaptureTime: Timestamp/TimestampTime (otel_logs, otel_traces, hyperdx_sessions —
// see cmd/sobs/schema.sql), TimeUnix/TimeUnixMs (otel_metrics_gauge/_sum/_histogram and
// their _pinned variants), and MinuteBucket (otel_metrics_1m_agg, handled separately by
// insertAggRows since that table has no JSONEachRow rows of its own). Routes like
// tracewindows/tracemetrics correlate a trace's Timestamp against nearby metric
// TimeUnix/MinuteBucket rows within a fixed window (see fetchTraceMetricContext in
// cmd/sobs/handlers_incident.go) — shifting one without the other desyncs that window and
// silently drops the correlation.
//
// Adding a new capture-session table with its own timestamp column? Add its field name and
// layout here (or to insertAggRows if it's non-JSONEachRow like otel_metrics_1m_agg) — the
// captureWindow check already keeps this safe for any pinned historical dates elsewhere.
var shiftableFields = map[string]string{
	"Timestamp":     dateTime64Layout,
	"TimestampTime": dateTimeLayout,
	"TimeUnix":      dateTime64Layout,
	"TimeUnixMs":    dateTimeLayout,
}

// captureShift returns how far to move every in-session row so the capture stays anchored
// to "now": time.Since(referenceCaptureTime), truncated to whole HOURS.
//
// Truncated to whole hours, not just whole seconds:
//
//   - Sub-second jitter: every captured Timestamp/TimeUnix in the capture session carries
//     all-zero sub-second digits (e.g. "...000000000"), and some golden fixtures (e.g.
//     summaryrich) mask only the human-meaningful part of a rendered timestamp, leaving
//     the literal fractional suffix in the golden text — any sub-second shift component
//     would inject nanosecond jitter into that unmasked suffix and break byte parity even
//     though the row's meaning is unchanged.
//   - Hour-of-day buckets: seasonal/periodicity analysis (metric_candidates.go,
//     chart_anomaly_engine.go) groups rows by toHour(Timestamp). The whole capture session
//     fits inside a single hour-plus-change window, so several profiles (e.g. seasonalauto:
//     155 rows, all hour 11) rely on their rows staying in the SAME hour bucket relative to
//     each other. A shift with a sub-hour remainder (e.g. "+1d7h35m") rotates rows by a
//     fractional hour, which can push some — but not all — rows across an hour boundary and
//     split one bucket into two (this exact regression appeared and was caught while
//     validating this fix: "Hour Of Day: 1 bucket(s)" in golden became "2 bucket(s)" in Go).
//     Truncating to whole hours applies the identical integer-hour rotation to every row, so
//     any two rows that started in the same hour stay in the same hour after shifting (just
//     relabeled to a different specific hour number, which no golden fixture asserts on
//     directly).
//
// Truncate (not Round) matters: it only ever rounds DOWN, so the shifted timestamp never
// overshoots into the future the way referenceCaptureTime's own choice (anchor to the
// session max, not a midpoint) already guards against. The discarded sub-hour remainder (at
// most ~1h) is negligible against the multi-hour "recent" windows (24h/48h) production
// routes actually check.
func captureShift() time.Duration {
	return time.Since(referenceCaptureTime).Truncate(time.Hour)
}

// shiftTimeString parses s per layout (UTC) and, if it falls within captureWindow of
// referenceCaptureTime, adds shift and reformats. Values outside the window are
// deliberately pinned historical dates (many profiles — e.g. aiview, tracedetail,
// tracesrich, incidentmatch — seed rows dated 2020/2023/2024 purely so the golden
// HTML/JSON can byte-compare an exact rendered timestamp) and must never move; those are
// returned unchanged with changed=false.
func shiftTimeString(s, layout string, shift time.Duration) (shifted string, changed bool, err error) {
	t, err := time.ParseInLocation(layout, s, time.UTC)
	if err != nil {
		return "", false, fmt.Errorf("parse %q: %w", s, err)
	}
	if d := t.Sub(referenceCaptureTime); d < -captureWindow || d > captureWindow {
		return s, false, nil // pinned historical date outside the capture session
	}
	return t.Add(shift).Format(layout), true, nil
}

// shiftRowTimestamps rewrites a JSONEachRow row's shiftableFields (Timestamp, TimestampTime,
// TimeUnix, TimeUnixMs), if present, via shiftTimeString. It decodes only into
// map[string]json.RawMessage — never map[string]any — so every other field (notably Map
// columns like ResourceAttributes/LogAttributes) round-trips as untouched original bytes;
// decoding into map[string]any would alphabetize their keys on re-marshal (Go's
// encoding/json always sorts map keys), corrupting Map-column byte parity against the
// golden capture (see insertRaw's original json.RawMessage-everywhere comment). Rows with
// none of these fields, or whose values are all outside captureWindow, are returned
// unmodified.
func shiftRowTimestamps(row json.RawMessage, shift time.Duration) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(row, &fields); err != nil {
		return nil, err
	}
	rowChanged := false
	for key, layout := range shiftableFields {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		shifted, changed, err := shiftTimeString(s, layout, shift)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		if !changed {
			continue
		}
		shiftedRaw, err := json.Marshal(shifted)
		if err != nil {
			return nil, err
		}
		fields[key] = shiftedRaw
		rowChanged = true
	}
	if !rowChanged {
		return row, nil
	}
	return json.Marshal(fields)
}
