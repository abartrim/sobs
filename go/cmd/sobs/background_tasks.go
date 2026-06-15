package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

// Background periodic workers — ports of app.py's @app.before_serving asyncio tasks. They run only
// in real runtime: app.py launches them at before_serving, but the parity capture harness uses
// app.test_client() without driving the lifespan, so they never run during capture. Gating the Go
// goroutines on !Parity keeps the parity replay free of their background chdb queries, byte-
// equivalent to the captured oracle. (The raw-window worker is additionally a no-op on the fixture,
// which seeds no sobs_raw_windows.)

// rawMetricTables / pinnedMetricTables are declared in metrics_retention.go (shared with the TTL
// retention worker).
const (
	rawWindowCopyIntervalS = 60
	rawWindowCopyMaxPerRun = 10
)

// startBackgroundWorkers launches the periodic workers app.py starts at before_serving. Skipped
// under parity (see file header). Call once, after the store is open.
func (s *server) startBackgroundWorkers() {
	if s.cfg.Parity || s.db == nil {
		return
	}
	go s.rawWindowCopyLoop()
	go s.cveScannerLoop()       // _cve_scanner_loop (enrichment_loops.go)
	go s.githubRepoHealthLoop() // _github_repo_health_loop (enrichment_loops.go)
}

// rawWindowCopyLoop is a port of app.py _raw_window_copy_loop: run the window-copy worker, then
// sleep 60s, forever.
func (s *server) rawWindowCopyLoop() {
	for {
		func() {
			defer func() { _ = recover() }()
			_, ok, errc := s.runRawWindowCopyWorker()
			if ok > 0 || errc > 0 {
				log.Printf("raw window copy: ok=%d errors=%d", ok, errc)
			}
		}()
		time.Sleep(rawWindowCopyIntervalS * time.Second)
	}
}

// runRawWindowCopyWorker is a port of app.py _run_raw_window_copy_worker: copy raw metric rows that
// fall within registered windows (sobs_raw_windows) into the pinned tables, recording completion in
// sobs_raw_window_copy_state. Idempotent — re-running is safe. Returns (windowsAttempted, copiesOK,
// copiesError), mirroring Python's stats dict.
func (s *server) runRawWindowCopyWorker() (windowsAttempted, copiesOK, copiesError int) {
	if s.db == nil {
		return 0, 0, 0
	}
	res, err := s.db.Execute(fmt.Sprintf(
		"SELECT Id, WindowStart, WindowEnd, ServiceName, Namespace, NodeName "+
			"FROM sobs_raw_windows FINAL ORDER BY WindowStart DESC LIMIT %d", rawWindowCopyMaxPerRun*20))
	if err != nil {
		return 0, 0, 0
	}
	windows := rowMaps(res)
	if len(windows) == 0 {
		return 0, 0, 0
	}

	copiesAttempted := 0
	for _, wrow := range windows {
		if copiesAttempted >= rawWindowCopyMaxPerRun {
			break
		}
		windowID := cStr(wrow, "Id")
		windowStart := cStr(wrow, "WindowStart")
		windowEnd := cStr(wrow, "WindowEnd")
		serviceName := cStr(wrow, "ServiceName")
		namespace := cStr(wrow, "Namespace")
		nodeName := cStr(wrow, "NodeName")

		for ti, rawTable := range rawMetricTables {
			if copiesAttempted >= rawWindowCopyMaxPerRun {
				break
			}
			pinnedTable := pinnedMetricTables[ti]

			chk, err := s.db.Execute(
				"SELECT 1 FROM sobs_raw_window_copy_state FINAL WHERE WindowId=? AND SourceTable=? LIMIT 1",
				windowID, rawTable)
			if err != nil {
				continue
			}
			if len(rowMaps(chk)) > 0 {
				continue // already copied
			}

			windowsAttempted++

			where := []string{
				"TimeUnix >= parseDateTime64BestEffort(?, 9)",
				"TimeUnix <= parseDateTime64BestEffort(?, 9)",
			}
			params := []any{windowStart, windowEnd}
			if serviceName != "" {
				where = append(where, "ServiceName = ?")
				params = append(params, serviceName)
			}
			if namespace != "" {
				where = append(where, "Attributes['k8s.namespace.name'] = ?")
				params = append(params, namespace)
			}
			if nodeName != "" {
				where = append(where, "Attributes['k8s.node.name'] = ?")
				params = append(params, nodeName)
			}
			whereSQL := strings.Join(where, " AND ")

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

			countRes, err := s.db.Execute("SELECT count() AS cnt FROM "+rawTable+" WHERE "+whereSQL, params...)
			if err != nil {
				copiesAttempted++
				copiesError++
				continue
			}
			matched := 0
			if rm := rowMaps(countRes); len(rm) > 0 {
				matched = cInt(rm[0], "cnt")
			}
			if matched <= 0 {
				continue
			}

			// Only copy rows not already present in the pinned table (params appear twice: outer
			// WHERE + the NOT IN subquery's WHERE — Python's `params * 2`).
			doubled := append(append([]any{}, params...), params...)
			notIn := " AND (ServiceName, MetricName, AttrFingerprint, TimeUnix) NOT IN (" +
				"SELECT ServiceName, MetricName, AttrFingerprint, TimeUnix FROM " + pinnedTable + " WHERE " + whereSQL + ")"

			missRes, err := s.db.Execute("SELECT count() AS cnt FROM "+rawTable+" WHERE "+whereSQL+notIn, doubled...)
			if err != nil {
				copiesAttempted++
				copiesError++
				continue
			}
			missing := 0
			if rm := rowMaps(missRes); len(rm) > 0 {
				missing = cInt(rm[0], "cnt")
			}

			version := time.Now().UnixMilli()
			markCopied := func() {
				_, _ = s.insertRowsNormalized("sobs_raw_window_copy_state", []map[string]any{
					{"WindowId": windowID, "SourceTable": rawTable, "Version": version},
				})
			}
			if missing <= 0 {
				markCopied()
				copiesAttempted++
				copiesOK++
				continue
			}

			if _, err := s.db.Execute("INSERT INTO "+pinnedTable+" ("+selectCols+") "+
				"SELECT "+selectCols+" FROM "+rawTable+" WHERE "+whereSQL+notIn, doubled...); err != nil {
				copiesAttempted++
				copiesError++
				continue
			}
			markCopied()
			copiesAttempted++
			copiesOK++
		}
	}
	return windowsAttempted, copiesOK, copiesError
}
