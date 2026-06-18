package main

import (
	"encoding/base64"
	"testing"

	"google.golang.org/protobuf/proto"

	collectorlogs "github.com/sobs/sobs/internal/otlp/genpb/collector/logs/v1"
	collectormetrics "github.com/sobs/sobs/internal/otlp/genpb/collector/metrics/v1"
	collectortrace "github.com/sobs/sobs/internal/otlp/genpb/collector/trace/v1"
	commonpb "github.com/sobs/sobs/internal/otlp/genpb/common/v1"
	logspb "github.com/sobs/sobs/internal/otlp/genpb/logs/v1"
	metricspb "github.com/sobs/sobs/internal/otlp/genpb/metrics/v1"
	resourcepb "github.com/sobs/sobs/internal/otlp/genpb/resource/v1"
	tracepb "github.com/sobs/sobs/internal/otlp/genpb/trace/v1"
)

// kvStr builds an OTLP KeyValue holding a string AnyValue.
func kvStr(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

// The three fixtures mirror the existing OTLP-JSON parity entries (post__v1_{logs,traces,metrics}__ingest
// in migration/manifest/routes.yaml) so the protobuf and JSON wire formats encode the SAME logical
// batch and both must produce {"accepted":1}.
const fixtureTimeUnixNano = 1704164645000000000

func fixtureLogsReq() *collectorlogs.ExportLogsServiceRequest {
	return &collectorlogs.ExportLogsServiceRequest{
		ResourceLogs: []*logspb.ResourceLogs{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kvStr("service.name", "api")}},
			ScopeLogs: []*logspb.ScopeLogs{{
				LogRecords: []*logspb.LogRecord{{
					TimeUnixNano: fixtureTimeUnixNano,
					SeverityText: "ERROR",
					Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "boom"}},
					Attributes:   []*commonpb.KeyValue{kvStr("event.name", "request.failed")},
				}},
			}},
		}},
	}
}

func fixtureTracesReq() *collectortrace.ExportTraceServiceRequest {
	return &collectortrace.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kvStr("service.name", "api")}},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{
					Name:              "GET /",
					StartTimeUnixNano: fixtureTimeUnixNano,
					EndTimeUnixNano:   fixtureTimeUnixNano + 1000000000,
					Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK},
					Attributes:        []*commonpb.KeyValue{kvStr("http.method", "GET")},
				}},
			}},
		}},
	}
}

func fixtureMetricsReq() *collectormetrics.ExportMetricsServiceRequest {
	return &collectormetrics.ExportMetricsServiceRequest{
		ResourceMetrics: []*metricspb.ResourceMetrics{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{kvStr("service.name", "api")}},
			ScopeMetrics: []*metricspb.ScopeMetrics{{
				Metrics: []*metricspb.Metric{{
					Name: "cpu.usage",
					Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
						DataPoints: []*metricspb.NumberDataPoint{{
							TimeUnixNano: fixtureTimeUnixNano,
							Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: 0.5},
							Attributes:   []*commonpb.KeyValue{kvStr("core", "0")},
						}},
					}},
				}},
			}},
		}},
	}
}

// nthRecord drills resource[0]/scope[0]/rec[0] of an OTLP-JSON map and returns the record map.
func firstRecord(t *testing.T, m map[string]any, resKey, scopeKey, recKey string) map[string]any {
	t.Helper()
	res, _ := m[resKey].([]any)
	if len(res) != 1 {
		t.Fatalf("%s: want 1 resource, got %d", resKey, len(res))
	}
	ro, _ := res[0].(map[string]any)
	sc, _ := ro[scopeKey].([]any)
	if len(sc) != 1 {
		t.Fatalf("%s: want 1 scope, got %d", scopeKey, len(sc))
	}
	so, _ := sc[0].(map[string]any)
	recs, _ := so[recKey].([]any)
	if len(recs) != 1 {
		t.Fatalf("%s: want 1 record, got %d", recKey, len(recs))
	}
	rec, _ := recs[0].(map[string]any)
	return rec
}

// TestOTLPProtoToMap is the local proof that the protobuf wire format re-renders into the canonical
// OTLP-JSON shape the existing JSON-path row builders consume — the same convergence app.py gets by
// ParseDict-ing JSON into the proto class. The end-to-end insert+count is verified by the parity
// harness (post__v1_*__ingest_proto entries); this guards the conversion in isolation, with no chdb.
func TestOTLPProtoToMap(t *testing.T) {
	// Logs ----------------------------------------------------------------------------------------
	logBytes, err := proto.Marshal(fixtureLogsReq())
	if err != nil {
		t.Fatalf("marshal logs: %v", err)
	}
	logMap, err := otlpProtoToMap(logBytes, "logRecords")
	if err != nil {
		t.Fatalf("otlpProtoToMap logs: %v", err)
	}
	rec := firstRecord(t, logMap, "resourceLogs", "scopeLogs", "logRecords")
	if got := otlpInt(rec["timeUnixNano"]); got != fixtureTimeUnixNano {
		t.Errorf("log timeUnixNano = %d, want %d", got, int64(fixtureTimeUnixNano))
	}
	if got := toStr(rec["severityText"]); got != "ERROR" {
		t.Errorf("log severityText = %q, want ERROR", got)
	}
	if got, _ := otlpAnyValue(rec["body"]).(string); got != "boom" {
		t.Errorf("log body = %q, want boom", got)
	}
	if attrs := otlpKVList(asList(rec["attributes"])); toStr(attrs["event.name"]) != "request.failed" {
		t.Errorf("log event.name = %v, want request.failed", attrs["event.name"])
	}

	// Traces --------------------------------------------------------------------------------------
	traceBytes, err := proto.Marshal(fixtureTracesReq())
	if err != nil {
		t.Fatalf("marshal traces: %v", err)
	}
	traceMap, err := otlpProtoToMap(traceBytes, "spans")
	if err != nil {
		t.Fatalf("otlpProtoToMap traces: %v", err)
	}
	span := firstRecord(t, traceMap, "resourceSpans", "scopeSpans", "spans")
	if got := toStr(span["name"]); got != "GET /" {
		t.Errorf("span name = %q, want 'GET /'", got)
	}
	// CRITICAL: status.code must round-trip as a NUMBER (1), not the enum name "STATUS_CODE_OK".
	// This is the UseEnumNumbers guarantee — without it ingestOTLPTraces reads 0 and downgrades
	// every span to STATUS_CODE_UNSET, silently corrupting status on real exporter traffic.
	st, _ := span["status"].(map[string]any)
	if st == nil {
		t.Fatalf("span status missing")
	}
	if got := otlpInt(st["code"]); got != 1 {
		t.Errorf("span status.code = %v (%T), want numeric 1 — UseEnumNumbers regression", st["code"], st["code"])
	}

	// Metrics -------------------------------------------------------------------------------------
	metricBytes, err := proto.Marshal(fixtureMetricsReq())
	if err != nil {
		t.Fatalf("marshal metrics: %v", err)
	}
	metricMap, err := otlpProtoToMap(metricBytes, "metrics")
	if err != nil {
		t.Fatalf("otlpProtoToMap metrics: %v", err)
	}
	metric := firstRecord(t, metricMap, "resourceMetrics", "scopeMetrics", "metrics")
	if got := toStr(metric["name"]); got != "cpu.usage" {
		t.Errorf("metric name = %q, want cpu.usage", got)
	}
	gauge, _ := metric["gauge"].(map[string]any)
	if gauge == nil {
		t.Fatalf("metric gauge missing")
	}
	dps, _ := gauge["dataPoints"].([]any)
	if len(dps) != 1 {
		t.Fatalf("gauge dataPoints = %d, want 1", len(dps))
	}
	dp, _ := dps[0].(map[string]any)
	if got := otlpDPValue(dp); got != 0.5 {
		t.Errorf("gauge value = %v, want 0.5", got)
	}
	if got := otlpInt(dp["timeUnixNano"]); got != fixtureTimeUnixNano {
		t.Errorf("gauge timeUnixNano = %d, want %d", got, int64(fixtureTimeUnixNano))
	}
}

// TestGenerateOTLPProtoFixtures prints the base64-encoded protobuf bodies embedded as `body_b64`
// in the post__v1_*__ingest_proto manifest entries. Run `go test ./cmd/sobs -run
// TestGenerateOTLPProtoFixtures -v` to regenerate them. The bytes are a valid (deterministic)
// encoding of each request; both the Python oracle and Go decode them to the same logical batch.
func TestGenerateOTLPProtoFixtures(t *testing.T) {
	cases := []struct {
		name string
		msg  proto.Message
	}{
		{"logs", fixtureLogsReq()},
		{"traces", fixtureTracesReq()},
		{"metrics", fixtureMetricsReq()},
	}
	for _, c := range cases {
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(c.msg)
		if err != nil {
			t.Fatalf("marshal %s: %v", c.name, err)
		}
		t.Logf("OTLP_PROTO_FIXTURE %s body_b64=%s", c.name, base64.StdEncoding.EncodeToString(b))
	}
}
