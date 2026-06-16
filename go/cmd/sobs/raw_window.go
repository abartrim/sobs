package main

import (
	"strings"
	"time"
)

// rawMetricsWindowMinutes mirrors app.py _RAW_METRICS_WINDOW_MINUTES.
const rawMetricsWindowMinutes = 5

// registerRawWindow mirrors app.py _register_raw_window: register a raw preservation window around a
// signal and return its (deterministic, dedup-keyed) window Id. The window is keyed by a SHA-256 of
// the minute-bucketed signal coordinates so repeated triggers within the same minute collapse onto a
// single row. Best-effort insert like the Python _insert_rows_json_each_row call.
func (s *server) registerRawWindow(signalTs time.Time, signalType, signalRef, serviceName, namespace, nodeName string) string {
	windowStart := signalTs.Add(-time.Duration(rawMetricsWindowMinutes) * time.Minute)
	windowEnd := signalTs.Add(time.Duration(rawMetricsWindowMinutes) * time.Minute)

	dedupKey := strings.Join([]string{
		signalTs.Format("2006-01-02T15:04"),
		truncRunes(signalType, 64),
		truncRunes(signalRef, 128),
		truncRunes(serviceName, 64),
		truncRunes(namespace, 64),
		truncRunes(nodeName, 64),
	}, "|")
	windowID := sha256Hex(dedupKey)[:32]

	// Python formats with "%Y-%m-%d %H:%M:%S.%f"[:-3] (microseconds truncated to milliseconds).
	const tsFmt = "2006-01-02 15:04:05.000"
	_, _ = s.insertRowsNormalized("sobs_raw_windows", []map[string]any{{
		"Id":          windowID,
		"SignalTs":    signalTs.Format(tsFmt),
		"WindowStart": windowStart.Format(tsFmt),
		"WindowEnd":   windowEnd.Format(tsFmt),
		"SignalType":  truncRunes(signalType, 64),
		"SignalRef":   truncRunes(signalRef, 256),
		"ServiceName": truncRunes(serviceName, 128),
		"Namespace":   truncRunes(namespace, 128),
		"NodeName":    truncRunes(nodeName, 128),
		"Version":     fixedVersionMillis(),
	}})
	return windowID
}
