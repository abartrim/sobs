package main

import "testing"

// cov95_b16_otlp_proto_test.go — batch 16 targeted coverage for cmd/sobs/otlp_proto.go's
// otlpProtoToMap: the corrupt-bytes unmarshal-error branch (for each of the three recKey types)
// and the default/unreachable recKey branch. TestOTLPProtoToMap (otlp_proto_test.go) already
// covers all three successful decode paths.

func TestOtlpProtoToMapUnmarshalError(t *testing.T) {
	garbage := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0x00, 0x01, 0x02}
	for _, recKey := range []string{"logRecords", "spans", "metrics"} {
		t.Run(recKey, func(t *testing.T) {
			if _, err := otlpProtoToMap(garbage, recKey); err == nil {
				t.Fatalf("expected an unmarshal error for corrupt %q bytes, got nil", recKey)
			}
		})
	}
}

func TestOtlpProtoToMapUnknownRecKey(t *testing.T) {
	// v1IngestOTLP only ever dispatches logRecords/spans/metrics; any other key hits the
	// unreachable default branch, returning an empty map with no error.
	got, err := otlpProtoToMap([]byte{}, "somethingElse")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want empty map for unknown recKey, got %v", got)
	}
}

func TestOtlpProtoToMapEmptyBodyDecodesSuccessfully(t *testing.T) {
	// An empty (but valid) protobuf body for a known recKey decodes to a zero-value message,
	// re-rendering to a map with no resource lists set.
	got, err := otlpProtoToMap([]byte{}, "logRecords")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("want non-nil map for an empty-but-valid protobuf body")
	}
}
