package main

import "testing"

func TestTelemetryEnabled(t *testing.T) {
	t.Setenv("SOBS_TELEMETRY_ENABLED", "")
	t.Setenv("OTEL_SDK_DISABLED", "")
	if telemetryEnabled() {
		t.Error("disabled by default")
	}
	t.Setenv("SOBS_TELEMETRY_ENABLED", "true")
	if !telemetryEnabled() {
		t.Error("should enable on true")
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if telemetryEnabled() {
		t.Error("OTEL_SDK_DISABLED=true must override")
	}
}

func TestTelemetryExporter(t *testing.T) {
	t.Setenv("SOBS_TELEMETRY_EXPORTER", "")
	if telemetryExporter() != "none" {
		t.Error("default none")
	}
	for _, v := range []string{"console", "otlp", "none"} {
		t.Setenv("SOBS_TELEMETRY_EXPORTER", v)
		if telemetryExporter() != v {
			t.Errorf("exporter %q", v)
		}
	}
	t.Setenv("SOBS_TELEMETRY_EXPORTER", "bogus")
	if telemetryExporter() != "none" {
		t.Error("unknown -> none")
	}
}

func TestTelemetrySampleRate(t *testing.T) {
	cases := map[string]float64{"": 1.0, "0.5": 0.5, "0": 0.0, "1": 1.0, "2.0": 1.0, "-1": 1.0, "x": 1.0}
	for in, want := range cases {
		t.Setenv("SOBS_TELEMETRY_SAMPLE_RATE", in)
		if got := telemetrySampleRate(); got != want {
			t.Errorf("sampleRate(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTelemetryDisabledIsNoOp(t *testing.T) {
	// The parity-critical invariant: a disabled telemetry never touches output and never panics.
	tel := &telemetry{enabled: false, exporter: "none"}
	if tel.active() {
		t.Error("should be inactive")
	}
	end := tel.span("sobs.ingest.request", map[string]any{"route": "/v1/logs"})
	end() // must not panic
	tel.recordIngestEvents(5, "log")
	tel.recordIngestBatchSize(5, "log")
	// nil-safe too (defensive: server.tel always set, but span/record are called widely)
	var nilTel *telemetry
	if nilTel.active() {
		t.Error("nil telemetry inactive")
	}
	nilTel.span("x", nil)() // must not panic
}

func TestOTLPJSONAttrs(t *testing.T) {
	attrs := otlpJSONAttrs(map[string]any{"k": "v"})
	if len(attrs) != 1 || attrs[0]["key"] != "k" {
		t.Fatalf("unexpected attrs: %v", attrs)
	}
	val, _ := attrs[0]["value"].(map[string]any)
	if val["stringValue"] != "v" {
		t.Errorf("stringValue not set: %v", val)
	}
}
