package main

// coverage_pure_h_test.go — oracle-anchored unit tests for SLICE H: the OTLP
// self-telemetry payload/env helpers in telemetry.go.
//
// telemetry.go is a dependency-light port of the Python `telemetry/` package
// (config.py / spans.py / metrics.py / setup.py). It is corpus-0% because the
// parity replay never sets SOBS_TELEMETRY_ENABLED=true, so these funcs are never
// hit by the byte-diff — but they are pure/​buffer-only and directly unit-testable.
//
// Target functions and their disposition:
//   TESTED:
//     anyToStr          (telemetry.go:295)  value→string coercion; mirrors the scalar
//                                           str()/attribute formatting Python passes to
//                                           span.set_attribute (telemetry/spans.py:43-47,
//                                           tests/test_telemetry.py:186-194 multi-attr scalars).
//     buildTracePayload (telemetry.go:192)  OTLP/JSON trace export envelope; resource attrs
//                                           = service.name + deployment.environment
//                                           (telemetry/setup.py:117-122).
//     resourceAttrs     (telemetry.go:185)  the OTLP resource KeyValue block
//                                           (telemetry/setup.py:117-122 Resource.create).
//     loadTelemetry     (telemetry.go:40)   env-driven constructor; defaults mirror
//                                           telemetry/config.py (service=sobs, env=local,
//                                           exporter=none, sample=1.0, console_export=false)
//                                           and tests/test_telemetry.py:415-440.
//     emitSpan          (telemetry.go:127)  buffers an OTLP span (otlp exporter) and/or logs
//                                           it (console). Python: spans.span ends by calling
//                                           record_span_duration with the dur in ms
//                                           (telemetry/spans.py:65-71).
//     kvString          (telemetry.go:287)  EXTENDED: only the trivial "k=v" case was tested
//                                           in small_helpers_test.go; here we cover ordering-
//                                           independent multi-key + non-string values.
//
//   SKIPPED (reason):
//     telemetryEnabled / telemetryExporter / telemetrySampleRate — fully covered (100%) in
//                       telemetry_test.go (Test{TelemetryEnabled,TelemetryExporter,
//                       TelemetrySampleRate}); not re-tested here.
//     buildMetricPayload — histogram branch tested in telemetry_test.go; sum/gauge branches
//                       exercised here only incidentally via emitMetric is NOT needed — left to
//                       its own slice. (We avoid duplicate ownership.)
//     emitMetric        — covered via TestRecordIngestBatchSizeEmitsHistogram in telemetry_test.go.
//     otlpJSONAttrs     — covered in telemetry_test.go:TestOTLPJSONAttrs (extended default/scalar
//                       branches are asserted transitively through resourceAttrs/emitSpan here).
//     post / flush / flushLoop — live network IO (HTTP POST + tickers); not unit-testable.
//     span (the timing wrapper) — its disabled no-op path is tested in
//                       telemetry_test.go:TestTelemetryDisabledIsNoOp; its active path delegates
//                       to emitSpan, tested directly here.

import (
	"sort"
	"strconv"
	"testing"
)

// ---------------------------------------------------------------------------
// anyToStr — telemetry.go:295
// Mirrors the scalar formatting Python passes to span.set_attribute. Go uses
// strconv: ints decimal, float64 via 'g' shortest round-trip, bool "true"/"false".
// Non-scalar (the OTel-disallowed case) collapses to "" — Python never reaches
// here because attributes are documented as scalar-only (telemetry/spans.py:35-37).
// ---------------------------------------------------------------------------

func TestSliceH_anyToStr(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string", "hello", "hello"},
		{"empty string", "", ""},
		{"unicode string", "café—日本語", "café—日本語"},
		{"non-ascii emoji", "🚀ok", "🚀ok"},
		{"int zero", 0, "0"},
		{"int positive", 42, "42"},
		{"int negative", -7, "-7"},
		{"int large", 9007199254740993, "9007199254740993"},
		{"int64", int64(9223372036854775807), "9223372036854775807"},
		{"int64 negative", int64(-9223372036854775808), "-9223372036854775808"},
		{"float whole", 3.0, "3"},
		{"float fraction", 12.5, "12.5"},
		{"float negative", -0.25, "-0.25"},
		{"float small", 0.001, "0.001"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		// Non-scalar values are not valid OTel attributes; Go returns "" (the
		// default branch). Python never passes these (scalar-only contract).
		{"nil -> empty", nil, ""},
		{"slice -> empty", []int{1, 2}, ""},
		{"map -> empty", map[string]int{"a": 1}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := anyToStr(c.in); got != c.want {
				t.Errorf("anyToStr(%#v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// kvString — telemetry.go:287  (EXTENDED beyond small_helpers_test.go's "k=v")
// Joins "key=anyToStr(value)" pairs with a space. Iteration over a Go map is
// unordered, so multi-key output is asserted order-independently.
// ---------------------------------------------------------------------------

func TestSliceH_kvString(t *testing.T) {
	t.Run("empty map", func(t *testing.T) {
		if got := kvString(map[string]any{}); got != "" {
			t.Errorf("kvString(empty) = %q, want \"\"", got)
		}
	})
	t.Run("nil map", func(t *testing.T) {
		if got := kvString(nil); got != "" {
			t.Errorf("kvString(nil) = %q, want \"\"", got)
		}
	})
	t.Run("single scalar mix", func(t *testing.T) {
		if got := kvString(map[string]any{"event.count": 3}); got != "event.count=3" {
			t.Errorf("kvString = %q, want event.count=3", got)
		}
	})
	t.Run("multi key order-independent", func(t *testing.T) {
		got := kvString(map[string]any{
			"event.type": "log",
			"count":      5,
			"rate":       0.5,
			"ok":         true,
		})
		// Split into "k=v" tokens and compare as a sorted set so map ordering
		// doesn't make the assertion flaky.
		tokens := splitSpaceTokens(got)
		sort.Strings(tokens)
		want := []string{"count=5", "event.type=log", "ok=true", "rate=0.5"}
		if len(tokens) != len(want) {
			t.Fatalf("kvString tokens = %v, want %v", tokens, want)
		}
		for i := range want {
			if tokens[i] != want[i] {
				t.Errorf("token[%d] = %q, want %q (full=%q)", i, tokens[i], want[i], got)
			}
		}
	})
}

func splitSpaceTokens(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// ---------------------------------------------------------------------------
// resourceAttrs — telemetry.go:185
// Oracle: telemetry/setup.py:117-122 Resource.create({"service.name": ...,
// "deployment.environment": ...}). Emitted as the OTLP/JSON KeyValue list.
// ---------------------------------------------------------------------------

func TestSliceH_resourceAttrs(t *testing.T) {
	tel := &telemetry{serviceName: "my-svc", environment: "prod"}
	attrs := tel.resourceAttrs()
	if len(attrs) != 2 {
		t.Fatalf("want 2 resource attrs (service.name + deployment.environment), got %d: %v", len(attrs), attrs)
	}
	got := map[string]string{}
	for _, kv := range attrs {
		key, _ := kv["key"].(string)
		val, _ := kv["value"].(map[string]any)
		sv, _ := val["stringValue"].(string)
		got[key] = sv
	}
	if got["service.name"] != "my-svc" {
		t.Errorf("service.name = %q, want my-svc", got["service.name"])
	}
	if got["deployment.environment"] != "prod" {
		t.Errorf("deployment.environment = %q, want prod", got["deployment.environment"])
	}
}

// ---------------------------------------------------------------------------
// buildTracePayload — telemetry.go:192
// The OTLP/HTTP-JSON trace export envelope: resourceSpans[0].resource.attributes
// = resourceAttrs(); scopeSpans[0].scope.name = "sobs"; .spans = the passed spans.
// ---------------------------------------------------------------------------

func TestSliceH_buildTracePayload(t *testing.T) {
	tel := &telemetry{serviceName: "sobs", environment: "local"}
	spans := []map[string]any{
		{"name": "sobs.ingest.request", "kind": 1, "durationMs": 12.5},
		{"name": "sobs.dashboard.query", "kind": 1, "durationMs": 3.0},
	}
	payload := tel.buildTracePayload(spans)

	rsAny, ok := payload["resourceSpans"]
	if !ok {
		t.Fatalf("missing resourceSpans: %v", payload)
	}
	rs := rsAny.([]map[string]any)
	if len(rs) != 1 {
		t.Fatalf("want 1 resourceSpans, got %d", len(rs))
	}
	// resource.attributes carries service.name + deployment.environment.
	resource := rs[0]["resource"].(map[string]any)
	rAttrs := resource["attributes"].([]map[string]any)
	if len(rAttrs) != 2 {
		t.Errorf("resource attributes len = %d, want 2", len(rAttrs))
	}
	scopeSpans := rs[0]["scopeSpans"].([]map[string]any)
	if len(scopeSpans) != 1 {
		t.Fatalf("want 1 scopeSpans, got %d", len(scopeSpans))
	}
	scope := scopeSpans[0]["scope"].(map[string]any)
	if scope["name"] != "sobs" {
		t.Errorf("scope.name = %v, want sobs", scope["name"])
	}
	gotSpans := scopeSpans[0]["spans"].([]map[string]any)
	if len(gotSpans) != 2 {
		t.Fatalf("want 2 spans, got %d", len(gotSpans))
	}
	if gotSpans[0]["name"] != "sobs.ingest.request" || gotSpans[1]["name"] != "sobs.dashboard.query" {
		t.Errorf("span names not passed through in order: %v", gotSpans)
	}

	// Empty span list still yields a well-formed envelope (the flush guard prevents
	// posting empty, but the builder itself must not choke on an empty slice).
	emptyPayload := tel.buildTracePayload([]map[string]any{})
	ers := emptyPayload["resourceSpans"].([]map[string]any)
	espans := ers[0]["scopeSpans"].([]map[string]any)[0]["spans"].([]map[string]any)
	if len(espans) != 0 {
		t.Errorf("empty payload spans len = %d, want 0", len(espans))
	}
}

// ---------------------------------------------------------------------------
// emitSpan — telemetry.go:127
// For the otlp exporter with a configured endpoint, buffers an OTLP span on
// t.spans with name/kind/durationMs/attributes. For console (or consoleExport)
// it logs only. Python's span() terminates by recording the duration in ms
// (telemetry/spans.py:65-71); the Go port records it onto the buffer.
// ---------------------------------------------------------------------------

func TestSliceH_emitSpan(t *testing.T) {
	t.Run("otlp buffers span", func(t *testing.T) {
		tel := &telemetry{enabled: true, exporter: "otlp", otlpEndpoint: "http://localhost:1"}
		tel.emitSpan("sobs.ingest.request", 12.5, map[string]any{"event.type": "log", "event.count": 3})
		if len(tel.spans) != 1 {
			t.Fatalf("want 1 buffered span, got %d", len(tel.spans))
		}
		s := tel.spans[0]
		if s["name"] != "sobs.ingest.request" {
			t.Errorf("name = %v", s["name"])
		}
		if s["kind"] != 1 {
			t.Errorf("kind = %v, want 1 (SPAN_KIND_INTERNAL)", s["kind"])
		}
		if s["durationMs"] != 12.5 {
			t.Errorf("durationMs = %v, want 12.5", s["durationMs"])
		}
		attrs, ok := s["attributes"].([]map[string]any)
		if !ok || len(attrs) != 2 {
			t.Fatalf("attributes = %v, want 2 KeyValues", s["attributes"])
		}
		// start/end timestamps must be present and parse as int64 nanos, with
		// start <= end (start is end minus the duration).
		startStr, _ := s["startTimeUnixNano"].(string)
		endStr, _ := s["endTimeUnixNano"].(string)
		start, errS := strconv.ParseInt(startStr, 10, 64)
		end, errE := strconv.ParseInt(endStr, 10, 64)
		if errS != nil || errE != nil {
			t.Fatalf("timestamps not int64 nanos: start=%q end=%q", startStr, endStr)
		}
		if start > end {
			t.Errorf("startTimeUnixNano (%d) must be <= endTimeUnixNano (%d)", start, end)
		}
	})

	t.Run("nil attrs buffers empty attribute list", func(t *testing.T) {
		tel := &telemetry{enabled: true, exporter: "otlp", otlpEndpoint: "http://localhost:1"}
		tel.emitSpan("sobs.test", 1.0, nil)
		if len(tel.spans) != 1 {
			t.Fatalf("want 1 buffered span, got %d", len(tel.spans))
		}
		attrs, _ := tel.spans[0]["attributes"].([]map[string]any)
		if len(attrs) != 0 {
			t.Errorf("nil attrs should produce empty KeyValue list, got %v", attrs)
		}
	})

	t.Run("console exporter does not buffer", func(t *testing.T) {
		// console-only: logs to stderr but the otlp buffer stays empty.
		tel := &telemetry{enabled: true, exporter: "console"}
		tel.emitSpan("sobs.test", 2.0, map[string]any{"route": "/v1/logs"})
		if len(tel.spans) != 0 {
			t.Errorf("console exporter must not buffer spans, got %d", len(tel.spans))
		}
	})

	t.Run("otlp without endpoint does not buffer", func(t *testing.T) {
		// emitSpan only buffers when an endpoint is configured (flush would have
		// nowhere to send otherwise).
		tel := &telemetry{enabled: true, exporter: "otlp", otlpEndpoint: ""}
		tel.emitSpan("sobs.test", 2.0, map[string]any{"route": "/v1/logs"})
		if len(tel.spans) != 0 {
			t.Errorf("otlp without endpoint must not buffer, got %d", len(tel.spans))
		}
	})
}

// ---------------------------------------------------------------------------
// loadTelemetry — telemetry.go:40
// Env-driven constructor. Defaults mirror telemetry/config.py:
//   service=sobs, env=local, exporter=none, sample=1.0, console_export=false,
//   enabled=false (SOBS_TELEMETRY_ENABLED unset).
// We never set exporter=otlp WITH an endpoint while enabled, to avoid spawning
// the background flushLoop goroutine (live IO) — the field-population logic is
// the point, and that branch (post/flush/flushLoop) is the documented SKIP.
// ---------------------------------------------------------------------------

func TestSliceH_loadTelemetry(t *testing.T) {
	// Each subtest sets the full env surface explicitly via t.Setenv (auto-restored).
	clearTelemetryEnv := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{
			"SOBS_TELEMETRY_ENABLED", "OTEL_SDK_DISABLED", "SOBS_TELEMETRY_EXPORTER",
			"SOBS_TELEMETRY_SERVICE_NAME", "SOBS_TELEMETRY_ENVIRONMENT",
			"SOBS_TELEMETRY_OTLP_ENDPOINT", "SOBS_TELEMETRY_CONSOLE_EXPORT",
			"SOBS_TELEMETRY_SAMPLE_RATE",
		} {
			t.Setenv(k, "")
		}
	}

	t.Run("defaults: disabled no-op", func(t *testing.T) {
		clearTelemetryEnv(t)
		tel := loadTelemetry()
		if tel.enabled {
			t.Error("default must be disabled")
		}
		if tel.active() {
			t.Error("default must be inactive (no-op)")
		}
		if tel.exporter != "none" {
			t.Errorf("exporter = %q, want none", tel.exporter)
		}
		if tel.serviceName != "sobs" {
			t.Errorf("serviceName = %q, want sobs", tel.serviceName)
		}
		if tel.environment != "local" {
			t.Errorf("environment = %q, want local", tel.environment)
		}
		if tel.sampleRate != 1.0 {
			t.Errorf("sampleRate = %v, want 1.0", tel.sampleRate)
		}
		if tel.consoleExport {
			t.Error("consoleExport must default false")
		}
		if tel.otlpEndpoint != "" {
			t.Errorf("otlpEndpoint = %q, want empty", tel.otlpEndpoint)
		}
		if tel.client == nil {
			t.Error("client must be initialised")
		}
	})

	t.Run("enabled console exporter with custom service/env", func(t *testing.T) {
		clearTelemetryEnv(t)
		t.Setenv("SOBS_TELEMETRY_ENABLED", "true")
		t.Setenv("SOBS_TELEMETRY_EXPORTER", "console")
		t.Setenv("SOBS_TELEMETRY_SERVICE_NAME", "edge-collector")
		t.Setenv("SOBS_TELEMETRY_ENVIRONMENT", "staging")
		t.Setenv("SOBS_TELEMETRY_SAMPLE_RATE", "0.25")
		t.Setenv("SOBS_TELEMETRY_CONSOLE_EXPORT", "true")
		tel := loadTelemetry()
		if !tel.enabled {
			t.Error("should be enabled")
		}
		if !tel.active() {
			t.Error("console exporter when enabled is active")
		}
		if tel.exporter != "console" {
			t.Errorf("exporter = %q, want console", tel.exporter)
		}
		if tel.serviceName != "edge-collector" {
			t.Errorf("serviceName = %q, want edge-collector", tel.serviceName)
		}
		if tel.environment != "staging" {
			t.Errorf("environment = %q, want staging", tel.environment)
		}
		if tel.sampleRate != 0.25 {
			t.Errorf("sampleRate = %v, want 0.25", tel.sampleRate)
		}
		if !tel.consoleExport {
			t.Error("consoleExport should be true")
		}
	})

	t.Run("OTEL_SDK_DISABLED overrides enabled", func(t *testing.T) {
		clearTelemetryEnv(t)
		t.Setenv("SOBS_TELEMETRY_ENABLED", "true")
		t.Setenv("OTEL_SDK_DISABLED", "true")
		t.Setenv("SOBS_TELEMETRY_EXPORTER", "console")
		tel := loadTelemetry()
		if tel.enabled {
			t.Error("OTEL_SDK_DISABLED=true must force disabled")
		}
		if tel.active() {
			t.Error("must be inactive when OTEL_SDK_DISABLED overrides")
		}
	})

	t.Run("enabled otlp but no endpoint: no flushLoop, inactive-by-endpoint buffering", func(t *testing.T) {
		// otlp + enabled but endpoint empty: loadTelemetry must NOT start the
		// background flushLoop (the guard requires a non-empty endpoint). active()
		// is still true (exporter != none), but emitSpan won't buffer (no endpoint).
		clearTelemetryEnv(t)
		t.Setenv("SOBS_TELEMETRY_ENABLED", "true")
		t.Setenv("SOBS_TELEMETRY_EXPORTER", "otlp")
		// no SOBS_TELEMETRY_OTLP_ENDPOINT
		tel := loadTelemetry()
		if !tel.enabled {
			t.Error("should be enabled")
		}
		if tel.exporter != "otlp" {
			t.Errorf("exporter = %q, want otlp", tel.exporter)
		}
		if tel.otlpEndpoint != "" {
			t.Errorf("otlpEndpoint = %q, want empty", tel.otlpEndpoint)
		}
		// active() is exporter-based, true here; but buffering requires an endpoint.
		tel.emitSpan("sobs.test", 1.0, nil)
		if len(tel.spans) != 0 {
			t.Errorf("otlp without endpoint must not buffer, got %d", len(tel.spans))
		}
	})
}
