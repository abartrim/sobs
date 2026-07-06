package main

// No-op stubs for the optional telemetry self-instrumentation module.
// PORT-NOTE: telemetry/ in the Python tree wires optional OTEL
// self-instrumentation (configure_telemetry, span, ingest counters,
// traced_view). The Go port stubs these; wire OTEL-Go here if needed.

import "net/http"

func telemetryConfigureTelemetry() {}

// telemetrySpan mirrors `with _telemetry.span(name, **attrs):` — call the
// returned func to end the span.
func telemetrySpan(name string, attrs map[string]any) func() {
	_ = name
	_ = attrs
	return func() {}
}

func telemetryRecordIngestEvents(count int, kind string)    { _, _ = count, kind }
func telemetryRecordIngestBatchSize(count int, kind string) { _, _ = count, kind }

// telemetryTracedView mirrors the @_telemetry.traced_view decorator.
func telemetryTracedView(name string, attrs map[string]any) func(http.HandlerFunc) http.HandlerFunc {
	_ = name
	_ = attrs
	return func(h http.HandlerFunc) http.HandlerFunc { return h }
}
