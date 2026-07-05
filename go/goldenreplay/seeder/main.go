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

	for table, rows := range delta {
		var insertErr error
		if table == aggTable {
			insertErr = insertAggRows(db, rows)
		} else {
			insertErr = insertRaw(db, table, rows)
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
func insertAggRows(db store.DB, rawRows []json.RawMessage) error {
	for _, raw := range rawRows {
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			return fmt.Errorf("decode agg row: %w", err)
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
func insertRaw(db store.DB, table string, rows []json.RawMessage) error {
	if len(rows) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" FORMAT JSONEachRow\n")
	for _, row := range rows {
		b.Write(row)
		b.WriteByte('\n')
	}
	_, err := db.Execute(b.String())
	return err
}
