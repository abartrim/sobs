package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
)

// Raw-metrics retention — a 1:1 port of app.py's `_ensure_raw_metrics_retention`
// (app.py ~2326-2376) plus the two TTL env vars and their positive-int validation.
//
// Python parses these at module-import time via `_parse_positive_int_env`, which raises
// ValueError on a non-int or <=0 value — crashing the process before it serves. Go mirrors
// that fail-fast with log.Fatalf in parsePositiveIntEnv (called from applyRawMetricsRetention,
// which runs during startup before any request is served).
//
// PARITY SAFETY: the golden corpus captures Python with these vars UNSET (profiles.py /
// seed_fixtures.py set neither SOBS_RAW_METRICS_TTL_HOURS nor SOBS_PINNED_METRICS_TTL_DAYS),
// so both keep their 48h / 14d defaults. More importantly, Go's schema-init runs against an
// ALREADY-SEEDED fixture whose metric rows are timestamped 2024 — far older than now()-48h.
// An `ALTER ... MODIFY TTL` that materializes could DROP those fixture rows and turn metric
// routes RED. The corpus is captured under SOBS_PARITY=1, so applyRawMetricsRetention is GATED
// on NOT being in parity mode: under parity Go issues NO ALTERs (fixture untouched); in real
// runtime it issues them exactly as Python does. The TTL never appears in any response body or
// header (it only governs background part-drops), so emitting it in real runtime is byte-parity
// -equivalent to Python while skipping it under parity protects the frozen fixture rows.

// Defaults match app.py's `_parse_positive_int_env(..., "48", "hours")` / `(..., "14", "days")`.
const (
	rawMetricsBaselineTTLDefaultHours = "48"
	rawMetricsPinnedTTLDefaultDays    = "14"
)

var rawMetricTables = [...]string{
	"otel_metrics_gauge",
	"otel_metrics_sum",
	"otel_metrics_histogram",
}

var pinnedMetricTables = [...]string{
	"otel_metrics_gauge_pinned",
	"otel_metrics_sum_pinned",
	"otel_metrics_histogram_pinned",
}

// parsePositiveIntEnv ports app.py `_parse_positive_int_env(name, default, unit)`: read the env
// var (falling back to default), require a base-10 integer > 0, and otherwise fail the same way
// Python does — Python raises ValueError at import time, which never serves a request, so Go
// terminates the process before it boots (log.Fatalf). The unit only shapes the error message.
func parsePositiveIntEnv(name, def, unit string) int {
	raw := def
	if v, ok := os.LookupEnv(name); ok {
		raw = v
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		log.Fatalf("%s must be a positive integer (%s)", name, unit)
	}
	return value
}

// applyRawMetricsRetention ports app.py `_ensure_raw_metrics_retention(db)`: issue
// `ALTER TABLE <t> MODIFY TTL TimeUnixMs + INTERVAL <hours> HOUR` for the raw metric tables and
// `... INTERVAL <days> DAY` for the *_pinned tables, each tolerant of failure (Python wraps every
// statement in try/except that only debug-logs). Called from the same DB-init point Python calls
// it (post-schema setup). Under parity this is a no-op — see the package-level PARITY SAFETY note.
func (s *server) applyRawMetricsRetention() {
	if s.db == nil || s.cfg.Parity {
		return
	}
	baselineHours := parsePositiveIntEnv("SOBS_RAW_METRICS_TTL_HOURS", rawMetricsBaselineTTLDefaultHours, "hours")
	pinnedDays := parsePositiveIntEnv("SOBS_PINNED_METRICS_TTL_DAYS", rawMetricsPinnedTTLDefaultDays, "days")

	statements := make([]string, 0, len(rawMetricTables)+len(pinnedMetricTables))
	for _, t := range rawMetricTables {
		statements = append(statements,
			fmt.Sprintf("ALTER TABLE %s MODIFY TTL TimeUnixMs + INTERVAL %d HOUR", t, baselineHours))
	}
	for _, t := range pinnedMetricTables {
		statements = append(statements,
			fmt.Sprintf("ALTER TABLE %s MODIFY TTL TimeUnixMs + INTERVAL %d DAY", t, pinnedDays))
	}
	for _, stmt := range statements {
		if _, err := s.db.Execute(stmt); err != nil {
			log.Printf("raw metrics retention alter skipped: %s (%v)", stmt, err)
		}
	}
}
