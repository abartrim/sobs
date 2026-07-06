// Command seeder is a short-lived helper invoked as its OWN subprocess (never in-process)
// by the goldenreplay test harness: open the chdb store at the given data dir, insert the
// given {table: rows} delta JSON, OPTIMIZE FINAL each touched table, and exit.
//
// It exists as a separate binary — not a function the test calls directly — because chdb-go's
// embedded engine has documented per-process global state that misbehaves under repeated
// Open/Close cycles within one long-lived process (see server.go's newServer comment); a
// fresh process per profile, exactly like the old Python seed_fixtures.py subprocess, avoids
// it entirely.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/store"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: seeder <data-dir> <delta.json>")
		os.Exit(2)
	}
	dataDir, deltaPath := os.Args[1], os.Args[2]

	b, err := os.ReadFile(deltaPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read delta:", err)
		os.Exit(1)
	}
	// Row values stay as json.RawMessage (never decoded into a Go map) end to end: chdb Map
	// columns (e.g. otel_traces.SpanAttributes) serialize to JSON in insertion order, and
	// dump_profile_seeds.py's Python-side json.dumps preserves that order faithfully — but
	// decoding into map[string]any and re-marshaling (as insertRaw used to) would silently
	// alphabetize the keys, since encoding/json always sorts Go map keys on Marshal. That
	// reordered a Map column's storage order relative to the golden capture, which routes
	// that read the map back in insertion order (mapKeys/mapValues) then surfaced as a
	// byte-level MISMATCH despite identical key/value content.
	var delta map[string][]json.RawMessage
	if err := json.Unmarshal(b, &delta); err != nil {
		fmt.Fprintln(os.Stderr, "parse delta:", err)
		os.Exit(1)
	}

	// Same intermittent embedded-server "recursive_mutex lock failed" / ASYNC_LOAD_WAIT_FAILED
	// boot error documented in cmd/sobs/server.go's newServer — chdb-go's embedded engine
	// occasionally fails to open when many short-lived chdb processes boot concurrently. Retry
	// in-process a few times before giving up; each attempt is a fresh Open() so the transient
	// contention almost always clears.
	var db store.DB
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		db, lastErr = store.Open(dataDir)
		if lastErr == nil {
			break
		}
		time.Sleep(time.Duration(300*(attempt+1)) * time.Millisecond)
	}
	if lastErr != nil {
		fmt.Fprintln(os.Stderr, "store.Open:", lastErr)
		os.Exit(1)
	}
	defer db.Close()

	// Truncated to whole HOURS, not just whole seconds:
	//
	//  - Sub-second jitter: every captured Timestamp/TimeUnix in the capture session
	//    carries all-zero sub-second digits (e.g. "...000000000"), and some golden
	//    fixtures (e.g. summaryrich) mask only the human-meaningful part of a rendered
	//    timestamp, leaving the literal fractional suffix in the golden text — any
	//    sub-second shift component would inject nanosecond jitter into that unmasked
	//    suffix and break byte parity even though the row's meaning is unchanged.
	//  - Hour-of-day buckets: seasonal/periodicity analysis (metric_candidates.go,
	//    chart_anomaly_engine.go) groups rows by toHour(Timestamp). The whole capture
	//    session fits inside a single hour-plus-change window, so several profiles (e.g.
	//    seasonalauto: 155 rows, all hour 11) rely on their rows staying in the SAME hour
	//    bucket relative to each other. A shift with a sub-hour remainder (e.g.
	//    "+1d7h35m") rotates rows by a fractional hour, which can push some — but not
	//    all — rows across an hour boundary and split one bucket into two (this exact
	//    regression appeared and was caught while validating this fix: "Hour Of Day: 1
	//    bucket(s)" in golden became "2 bucket(s)" in Go). Truncating to whole hours
	//    applies the identical integer-hour rotation to every row, so any two rows that
	//    started in the same hour stay in the same hour after shifting (just relabeled to
	//    a different specific hour number, which no golden fixture asserts on directly).
	//
	// Truncate (not Round) matters: it only ever rounds DOWN, so the shifted timestamp
	// never overshoots into the future the way referenceCaptureTime's own choice (anchor
	// to the session max, not a midpoint — see below) already guards against. The
	// discarded sub-hour remainder (at most ~1h) is negligible against the multi-hour
	// "recent" windows (24h/48h) every route below actually checks.
	shift := time.Since(referenceCaptureTime).Truncate(time.Hour)
	for table, rows := range delta {
		var insertErr error
		if table == aggTable {
			insertErr = insertAggRows(db, rows, shift)
		} else {
			insertErr = insertRaw(db, table, rows, shift)
		}
		if insertErr != nil {
			fmt.Fprintf(os.Stderr, "insert into %s: %v\n", table, insertErr)
			os.Exit(1)
		}
		if _, err := db.Execute("OPTIMIZE TABLE " + table + " FINAL"); err != nil {
			fmt.Fprintf(os.Stderr, "optimize %s: %v\n", table, err)
			os.Exit(1)
		}
	}
}

// aggTable is populated as a materialized-view side effect of inserting into the raw
// otel_metrics_gauge/_sum/_histogram tables — and those raw tables carry a TTL (real
// wall-clock, not the app's frozen clock) applied at store-open time, so fixture rows
// timestamped in the past get evicted by the very "OPTIMIZE ... FINAL" the fixture
// extractor runs right after seeding them. The aggregated rows the MV already produced
// on the way through have no TTL and survive — see dump_profile_seeds.py's AGG_QUERY
// (extracted via avgMerge/sumMerge into plain scalars, since AggregateFunction columns
// aren't JSON-plain). insertAggRows reconstructs the state below.
const aggTable = "otel_metrics_1m_agg"

// insertAggRows reconstructs otel_metrics_1m_agg's AggregateFunction(avg/sum) state columns
// from the plain (Value, SampleCount) scalars dump_profile_seeds.py captured, by building each
// as a state over a SINGLE synthetic input (avgState(Value), sumState(SampleCount)). avgMerge of
// one avgState(V) always yields exactly V, and sumMerge of one sumState(C) always yields exactly
// C, regardless of the state's internal weight/count — so this exactly reproduces what
// v_otel_metrics_1m (avgMerge/sumMerge GROUP BY the same key this was captured at) displays. It
// would only under-weight a bucket relative to others if some OTHER query re-aggregated multiple
// MinuteBuckets/AttrFingerprints together, which no route in the corpus does.
func insertAggRows(db store.DB, rawRows []json.RawMessage, shift time.Duration) error {
	for _, raw := range rawRows {
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			return fmt.Errorf("decode agg row: %w", err)
		}
		// MinuteBucket faces the same capture-session-vs-pinned-date and now()-scoped-query
		// staleness issue as insertRaw's Timestamp/TimeUnix fields (see shiftTimeString) —
		// this table has no Map columns, so a plain map[string]any round-trip is safe here.
		if s, ok := row["MinuteBucket"].(string); ok {
			shifted, changed, err := shiftTimeString(s, dateTimeLayout, shift)
			if err != nil {
				return fmt.Errorf("shift agg MinuteBucket: %w", err)
			}
			if changed {
				row["MinuteBucket"] = shifted
			}
		}
		_, err := db.Execute(
			// CAST(? AS Float64/UInt64) matters: a bare numeric literal that happens to be
			// whole (e.g. "1") is typed UInt8 by ClickHouse, so avgState()/sumState() over it
			// would build an AggregateFunction(_, UInt8) state — incompatible with the
			// column's declared AggregateFunction(_, Float64/UInt64) type (CANNOT_CONVERT_TYPE
			// on insert).
			"INSERT INTO otel_metrics_1m_agg (ServiceName, MetricName, AttrFingerprint, MetricKind, MinuteBucket, Value, SampleCount) "+
				"SELECT ?, ?, ?, ?, ?, avgState(CAST(? AS Float64)), sumState(CAST(? AS UInt64))",
			row["ServiceName"], row["MetricName"], row["AttrFingerprint"], row["MetricKind"], row["MinuteBucket"],
			row["Value"], row["SampleCount"],
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// insertRaw mirrors migration/tools/seed_fixtures.py's _insert helper: "what
// _insert_rows_json_each_row does, minus the _WRITABLE_TABLES guard". Fixture seeding writes
// tables (e.g. sobs_error_resolutions) that the app itself only ever writes via a single
// parameterized INSERT, not the bulk-ingest JSONEachRow path — so store.DB's InsertJSONEachRow
// (which deliberately enforces that app-level allowlist) is the wrong call here. Executing the
// raw statement directly bypasses it, exactly as the Python fixture seeder does.
func insertRaw(db store.DB, table string, rows []json.RawMessage, shift time.Duration) error {
	if len(rows) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" FORMAT JSONEachRow\n")
	for _, row := range rows {
		shifted, err := shiftRowTimestamps(row, shift)
		if err != nil {
			return fmt.Errorf("shift timestamps in %s row: %w", table, err)
		}
		b.Write(shifted)
		b.WriteByte('\n')
	}
	_, err := db.Execute(b.String())
	return err
}

// referenceCaptureTime anchors the golden corpus to the LATEST wall-clock moment
// migration/tools/dump_profile_seeds.py captured any frozen fixture row in the capture
// session (the otel_logs/otel_traces/hyperdx_sessions/otel_metrics_*/otel_metrics_1m_agg
// Timestamp/TimestampTime/TimeUnix/TimeUnixMs/MinuteBucket values in
// testdata/fixtures/seeds.tar.gz that belong to this session cluster in a ~90min span
// ending 2026-07-05 11:30:00 UTC — see captureWindow below). Production handlers scope
// queries off real wall-clock now() (e.g. regexValidateRecentHours in
// cmd/sobs/regex_validate.go, the auto/seasonal metric candidate windows in
// metric_candidates.go), so as real time elapses past capture, those frozen rows
// silently age out of those windows and golden routes that depend on "recent" data
// start returning empty/zero results. shiftRowTimestamps adds
// time.Since(referenceCaptureTime) to a row's Timestamp/TimestampTime at seed time,
// keeping the captured set anchored to "now" while preserving every row's relative
// offset from every other row.
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
// eligible for the shift above) versus rows that pin a deliberately arbitrary, unrelated
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
var shiftableFields = map[string]string{
	"Timestamp":     dateTime64Layout,
	"TimestampTime": dateTimeLayout,
	"TimeUnix":      dateTime64Layout,
	"TimeUnixMs":    dateTimeLayout,
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
