package main

import (
	"encoding/json"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	collectorlogs "github.com/sobs/sobs/internal/otlp/genpb/collector/logs/v1"
	collectormetrics "github.com/sobs/sobs/internal/otlp/genpb/collector/metrics/v1"
	collectortrace "github.com/sobs/sobs/internal/otlp/genpb/collector/trace/v1"
)

// otlpProtoToMap deserializes an OTLP-protobuf ExportXServiceRequest body and re-renders it as the
// canonical OTLP-JSON shape (camelCase keys, int64 as strings, enums as numbers) so it can flow
// through the EXACT same row builders the JSON ingest path uses (ingestOTLPLogs / ingestOTLPTraces /
// ingestOTLPMetrics). This mirrors app.py, where the JSON path ParseDict's into the same proto class
// the protobuf path deserializes into, so both wire formats converge on one _proto_*_to_events path.
//
// recKey selects the request type (matching the keys v1IngestOTLP is dispatched with):
//
//	"logRecords" -> ExportLogsServiceRequest
//	"spans"      -> ExportTraceServiceRequest
//	"metrics"    -> ExportMetricsServiceRequest
//
// UseEnumNumbers is REQUIRED: the row builders read numeric enums (e.g. status.code 1/2 in
// ingestOTLPTraces, aggregationTemporality in ingestOTLPMetrics). protojson defaults to enum *names*,
// which would parse to 0 and silently downgrade every span status to UNSET.
func otlpProtoToMap(body []byte, recKey string) (map[string]any, error) {
	var msg proto.Message
	switch recKey {
	case "logRecords":
		msg = &collectorlogs.ExportLogsServiceRequest{}
	case "spans":
		msg = &collectortrace.ExportTraceServiceRequest{}
	case "metrics":
		msg = &collectormetrics.ExportMetricsServiceRequest{}
	default:
		// Unreachable: v1IngestOTLP only dispatches the three keys above.
		return map[string]any{}, nil
	}
	if err := proto.Unmarshal(body, msg); err != nil {
		return nil, err
	}
	jsonBytes, err := protojson.MarshalOptions{UseEnumNumbers: true}.Marshal(msg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return nil, err
	}
	return m, nil
}
