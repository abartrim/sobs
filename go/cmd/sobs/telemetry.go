package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// telemetry is a faithful, dependency-light port of the Python `telemetry` package (config.py /
// spans.py / metrics.py). Like the Python module it is a strict NO-OP unless explicitly enabled
// (SOBS_TELEMETRY_ENABLED=true and OTEL_SDK_DISABLED!=true), so the default deployment — and the
// parity corpus, which never sets these — behaves exactly as before. Telemetry is emitted to a
// separate collector / stderr, never into an HTTP response, so it is invisible to parity.
//
// Where Python relies on the optional OpenTelemetry SDK (requirements-telemetry.txt), this port
// stays within the Go stdlib: the `console` exporter writes structured lines to stderr and the
// `otlp` exporter POSTs OTLP/HTTP-JSON to the configured endpoint. That keeps the minimal-deps
// policy (no OTel Go SDK) while still honoring every documented SOBS_TELEMETRY_* knob.
type telemetry struct {
	enabled       bool
	exporter      string // none | console | otlp
	serviceName   string
	environment   string
	otlpEndpoint  string
	consoleExport bool
	sampleRate    float64

	client *http.Client
	mu     sync.Mutex
	spans  []map[string]any // buffered OTLP/JSON spans
	points []map[string]any // buffered OTLP/JSON metric data points (gauge/sum)
}

func loadTelemetry() *telemetry {
	t := &telemetry{
		enabled:       telemetryEnabled(),
		exporter:      telemetryExporter(),
		serviceName:   envTrim("SOBS_TELEMETRY_SERVICE_NAME", "sobs"),
		environment:   envTrim("SOBS_TELEMETRY_ENVIRONMENT", "local"),
		otlpEndpoint:  strings.TrimSpace(os.Getenv("SOBS_TELEMETRY_OTLP_ENDPOINT")),
		consoleExport: envFlag("SOBS_TELEMETRY_CONSOLE_EXPORT", false),
		sampleRate:    telemetrySampleRate(),
		client:        &http.Client{Timeout: 5 * time.Second},
	}
	if t.enabled && t.exporter == "otlp" && t.otlpEndpoint != "" {
		go t.flushLoop()
	}
	if t.enabled {
		log.Printf("self-telemetry enabled: exporter=%s service=%s env=%s sample=%.2f",
			t.exporter, t.serviceName, t.environment, t.sampleRate)
	}
	return t
}

// telemetryEnabled mirrors config.telemetry_enabled: on only when SOBS_TELEMETRY_ENABLED=true and
// the standard OTEL_SDK_DISABLED override is not "true".
func telemetryEnabled() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("SOBS_TELEMETRY_ENABLED")), "true")
}

func telemetryExporter() string {
	raw := strings.ToLower(envTrim("SOBS_TELEMETRY_EXPORTER", "none"))
	switch raw {
	case "none", "console", "otlp":
		return raw
	default:
		log.Printf("SOBS_TELEMETRY_EXPORTER has unrecognised value %q; defaulting to 'none'", raw)
		return "none"
	}
}

func telemetrySampleRate() float64 {
	raw := envTrim("SOBS_TELEMETRY_SAMPLE_RATE", "1.0")
	rate, err := strconv.ParseFloat(raw, 64)
	if err != nil || rate < 0.0 || rate > 1.0 {
		return 1.0
	}
	return rate
}

func (t *telemetry) active() bool { return t != nil && t.enabled && t.exporter != "none" }

// span mirrors spans.span: returns an end func that records the span's duration when telemetry is
// active. A no-op (cheap) when disabled, so call sites can wrap hot paths unconditionally:
//
//	defer s.tel.span("sobs.ingest.request", map[string]any{"route": "/v1/logs", "event.type": "log"})()
func (t *telemetry) span(name string, attrs map[string]any) func() {
	if !t.active() {
		return func() {}
	}
	start := time.Now()
	return func() {
		durMs := float64(time.Since(start).Microseconds()) / 1000.0
		t.emitSpan(name, durMs, attrs)
	}
}

func (t *telemetry) recordIngestEvents(count int, eventType string) {
	if !t.active() {
		return
	}
	t.emitMetric("sobs.ingest.events.total", "sum", float64(count), map[string]any{"event.type": eventType})
}

func (t *telemetry) recordIngestBatchSize(size int, eventType string) {
	if !t.active() {
		return
	}
	t.emitMetric("sobs.ingest.batch.size", "gauge", float64(size), map[string]any{"event.type": eventType})
}

// ---- emit / export --------------------------------------------------------------------

func (t *telemetry) emitSpan(name string, durMs float64, attrs map[string]any) {
	if t.consoleExport || t.exporter == "console" {
		log.Printf("[telemetry] span %s dur=%.3fms %s", name, durMs, kvString(attrs))
	}
	if t.exporter == "otlp" && t.otlpEndpoint != "" {
		t.mu.Lock()
		t.spans = append(t.spans, map[string]any{
			"name":              name,
			"kind":              1, // SPAN_KIND_INTERNAL
			"durationMs":        durMs,
			"attributes":        otlpJSONAttrs(attrs),
			"endTimeUnixNano":   strconv.FormatInt(time.Now().UnixNano(), 10),
			"startTimeUnixNano": strconv.FormatInt(time.Now().Add(-time.Duration(durMs*float64(time.Millisecond))).UnixNano(), 10),
		})
		t.mu.Unlock()
	}
}

func (t *telemetry) emitMetric(name, kind string, value float64, attrs map[string]any) {
	if t.consoleExport || t.exporter == "console" {
		log.Printf("[telemetry] metric %s=%.3f %s", name, value, kvString(attrs))
	}
	if t.exporter == "otlp" && t.otlpEndpoint != "" {
		t.mu.Lock()
		t.points = append(t.points, map[string]any{
			"name":         name,
			"kind":         kind,
			"value":        value,
			"attributes":   otlpJSONAttrs(attrs),
			"timeUnixNano": strconv.FormatInt(time.Now().UnixNano(), 10),
		})
		t.mu.Unlock()
	}
}

// flushLoop periodically ships buffered spans/metrics to the OTLP/HTTP-JSON endpoint. Runs for the
// process lifetime, like the Python SDK's batch processors.
func (t *telemetry) flushLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		t.flush()
	}
}

func (t *telemetry) flush() {
	t.mu.Lock()
	spans, points := t.spans, t.points
	t.spans, t.points = nil, nil
	t.mu.Unlock()
	if len(spans) > 0 {
		t.post("/v1/traces", t.buildTracePayload(spans))
	}
	if len(points) > 0 {
		t.post("/v1/metrics", t.buildMetricPayload(points))
	}
}

func (t *telemetry) resourceAttrs() []map[string]any {
	return otlpJSONAttrs(map[string]any{
		"service.name":           t.serviceName,
		"deployment.environment": t.environment,
	})
}

func (t *telemetry) buildTracePayload(spans []map[string]any) map[string]any {
	return map[string]any{"resourceSpans": []map[string]any{{
		"resource":   map[string]any{"attributes": t.resourceAttrs()},
		"scopeSpans": []map[string]any{{"scope": map[string]any{"name": "sobs"}, "spans": spans}},
	}}}
}

func (t *telemetry) buildMetricPayload(points []map[string]any) map[string]any {
	metrics := make([]map[string]any, 0, len(points))
	for _, p := range points {
		dp := map[string]any{
			"asDouble":     p["value"],
			"timeUnixNano": p["timeUnixNano"],
			"attributes":   p["attributes"],
		}
		m := map[string]any{"name": p["name"]}
		if p["kind"] == "sum" {
			m["sum"] = map[string]any{"dataPoints": []map[string]any{dp}, "aggregationTemporality": 2, "isMonotonic": true}
		} else {
			m["gauge"] = map[string]any{"dataPoints": []map[string]any{dp}}
		}
		metrics = append(metrics, m)
	}
	return map[string]any{"resourceMetrics": []map[string]any{{
		"resource":     map[string]any{"attributes": t.resourceAttrs()},
		"scopeMetrics": []map[string]any{{"scope": map[string]any{"name": "sobs"}, "metrics": metrics}},
	}}}
}

func (t *telemetry) post(path string, payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	url := strings.TrimRight(t.otlpEndpoint, "/") + path
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// otlpJSONAttrs converts a string-keyed attribute map into the OTLP/JSON KeyValue list.
func otlpJSONAttrs(attrs map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(attrs))
	for k, v := range attrs {
		var val map[string]any
		switch x := v.(type) {
		case string:
			val = map[string]any{"stringValue": x}
		case bool:
			val = map[string]any{"boolValue": x}
		case int:
			val = map[string]any{"intValue": strconv.Itoa(x)}
		case int64:
			val = map[string]any{"intValue": strconv.FormatInt(x, 10)}
		case float64:
			val = map[string]any{"doubleValue": x}
		default:
			val = map[string]any{"stringValue": kvString(map[string]any{k: v})}
		}
		out = append(out, map[string]any{"key": k, "value": val})
	}
	return out
}

func kvString(attrs map[string]any) string {
	parts := make([]string, 0, len(attrs))
	for k, v := range attrs {
		parts = append(parts, k+"="+anyToStr(v))
	}
	return strings.Join(parts, " ")
}

func anyToStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}
