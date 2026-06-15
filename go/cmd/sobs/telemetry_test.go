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

func TestBuildMetricPayloadHistogram(t *testing.T) {
	// ingest.batch.size must be emitted as an OTLP histogram (matching Python's
	// meter.create_histogram in telemetry/metrics.py), not a gauge or sum.
	tel := &telemetry{serviceName: "sobs", environment: "local"}
	point := map[string]any{
		"name":         "sobs.ingest.batch.size",
		"kind":         "histogram",
		"value":        7.0,
		"attributes":   otlpJSONAttrs(map[string]any{"event.type": "log"}),
		"timeUnixNano": "1",
	}
	payload := tel.buildMetricPayload([]map[string]any{point})
	rm := payload["resourceMetrics"].([]map[string]any)
	sm := rm[0]["scopeMetrics"].([]map[string]any)
	metrics := sm[0]["metrics"].([]map[string]any)
	if len(metrics) != 1 {
		t.Fatalf("want 1 metric, got %d", len(metrics))
	}
	m := metrics[0]
	if m["name"] != "sobs.ingest.batch.size" {
		t.Errorf("name = %v", m["name"])
	}
	if _, ok := m["gauge"]; ok {
		t.Error("must not be a gauge")
	}
	if _, ok := m["sum"]; ok {
		t.Error("must not be a sum")
	}
	h, ok := m["histogram"].(map[string]any)
	if !ok {
		t.Fatalf("expected histogram, got %v", m)
	}
	if h["aggregationTemporality"] != 2 {
		t.Errorf("aggregationTemporality = %v, want 2 (cumulative)", h["aggregationTemporality"])
	}
	dps := h["dataPoints"].([]map[string]any)
	if len(dps) != 1 {
		t.Fatalf("want 1 data point, got %d", len(dps))
	}
	dp := dps[0]
	if dp["count"] != "1" {
		t.Errorf("count = %v, want \"1\"", dp["count"])
	}
	if dp["sum"] != 7.0 || dp["min"] != 7.0 || dp["max"] != 7.0 {
		t.Errorf("sum/min/max = %v/%v/%v, want 7", dp["sum"], dp["min"], dp["max"])
	}
}

func TestRecordIngestBatchSizeEmitsHistogram(t *testing.T) {
	// End-to-end through emitMetric: an active otlp telemetry buffers a histogram-kind point.
	tel := &telemetry{enabled: true, exporter: "otlp", otlpEndpoint: "http://localhost:1"}
	tel.recordIngestBatchSize(3, "trace")
	if len(tel.points) != 1 {
		t.Fatalf("want 1 buffered point, got %d", len(tel.points))
	}
	if tel.points[0]["kind"] != "histogram" {
		t.Errorf("kind = %v, want histogram", tel.points[0]["kind"])
	}
	if tel.points[0]["name"] != "sobs.ingest.batch.size" {
		t.Errorf("name = %v", tel.points[0]["name"])
	}
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
