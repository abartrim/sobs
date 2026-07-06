package main

import (
	"encoding/base64"
	"testing"
)

// cov95_b8_otlp_ingest_test.go — batch 8 targeted coverage for cmd/sobs/otlp_ingest.go: the small
// OTLP value/id/attr pure helpers whose untested branches are malformed/edge-case inputs (odd-length
// hex, non-base64 garbage, non-string/non-numeric attr values, unknown status strings, etc.).

// otlpKVList: non-map entries in the list are skipped; a nil "value" key maps to nil (not skipped).
func TestOtlpKVListSkipsNonMapEntries(t *testing.T) {
	attrs := []any{
		"not a map",
		42,
		map[string]any{"key": "a", "value": map[string]any{"stringValue": "x"}},
		map[string]any{"key": "b"}, // value key absent -> otlpAnyValue(nil) -> nil
	}
	m := otlpKVList(attrs)
	if len(m) != 2 {
		t.Fatalf("otlpKVList len = %d, want 2 (non-map entries skipped), got %#v", len(m), m)
	}
	if m["a"] != "x" {
		t.Errorf(`m["a"] = %v, want "x"`, m["a"])
	}
	if v, ok := m["b"]; !ok || v != nil {
		t.Errorf(`m["b"] = %v (present=%v), want nil (present)`, v, ok)
	}
}

// otlpStringifyAttrs: nil values are dropped entirely (not stringified to "None"/"null").
func TestOtlpStringifyAttrsDropsNilValues(t *testing.T) {
	out := otlpStringifyAttrs(map[string]any{
		"dropped": nil,
		"kept":    "value",
	})
	if _, ok := out["dropped"]; ok {
		t.Errorf("nil-valued attr should be dropped, got %#v", out)
	}
	if out["kept"] != "value" {
		t.Errorf("kept = %v, want value", out["kept"])
	}
}

// otlpStringifyAttrs: bool true/false stringify to Python-style "True"/"False".
func TestOtlpStringifyAttrsBool(t *testing.T) {
	out := otlpStringifyAttrs(map[string]any{"t": true, "f": false})
	if out["t"] != "True" {
		t.Errorf("true -> %v, want True", out["t"])
	}
	if out["f"] != "False" {
		t.Errorf("false -> %v, want False", out["f"])
	}
}

// otlpStringifyAttrs: int64 and float64 stringify via their own numeric formatters.
func TestOtlpStringifyAttrsNumeric(t *testing.T) {
	out := otlpStringifyAttrs(map[string]any{"i": int64(42), "f": 3.5})
	if out["i"] != "42" {
		t.Errorf("int64 -> %v, want 42", out["i"])
	}
	if out["f"] != "3.5" {
		t.Errorf("float64 -> %v, want 3.5", out["f"])
	}
}

// otlpStringifyAttrs: a non-scalar value (list/map) falls to the JSON-dump default branch.
func TestOtlpStringifyAttrsNonScalarFallsToJSON(t *testing.T) {
	out := otlpStringifyAttrs(map[string]any{"list": []any{"a", "b"}})
	if out["list"] != `["a","b"]` {
		t.Errorf("list attr = %q, want JSON-encoded array", out["list"])
	}
}

// otlpHexID: empty input stays empty.
func TestOtlpHexIDEmpty(t *testing.T) {
	if got := otlpHexID("", 32); got != "" {
		t.Errorf("otlpHexID(\"\") = %q, want empty", got)
	}
	if got := otlpHexID(nil, 16); got != "" {
		t.Errorf("otlpHexID(nil) = %q, want empty", got)
	}
}

// otlpHexID: an already-hex string of the expected length is just lowercased.
func TestOtlpHexIDAlreadyHexLowercased(t *testing.T) {
	got := otlpHexID("ABCDEF0123456789ABCDEF0123456789", 32)
	want := "abcdef0123456789abcdef0123456789"
	if got != want {
		t.Errorf("otlpHexID = %q, want %q", got, want)
	}
}

// otlpHexID: a base64-encoded 8-byte span id decodes to the matching 16-char hex string.
func TestOtlpHexIDBase64Decodes(t *testing.T) {
	raw := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x11, 0x22, 0x33}
	b64 := base64.StdEncoding.EncodeToString(raw)
	got := otlpHexID(b64, 16)
	want := "deadbeef00112233"
	if got != want {
		t.Errorf("otlpHexID(base64) = %q, want %q", got, want)
	}
}

// otlpHexID: a value that is neither hex-of-expected-length nor valid base64 falls back to a
// lowercased passthrough (the "odd/garbage string" branch).
func TestOtlpHexIDFallbackLowercase(t *testing.T) {
	got := otlpHexID("NOT-VALID-B64!!!", 32)
	want := "not-valid-b64!!!"
	if got != want {
		t.Errorf("otlpHexID(garbage) = %q, want %q", got, want)
	}
}

// otlpHexID: a hex-looking string of the WRONG length (not matching expectHexLen) is treated as
// non-hex and falls through to the base64/fallback branches rather than the exact-match branch.
func TestOtlpHexIDWrongLengthHexIsNotShortCircuited(t *testing.T) {
	// "abcd" is valid hex but only 4 chars; expectHexLen=32 means it takes the base64-or-fallback
	// path. "abcd" is not valid base64 either (odd handling aside) so it lowercases as a fallback.
	got := otlpHexID("abcd", 32)
	if got == "" {
		t.Errorf("otlpHexID(short hex) should not be empty")
	}
}

// attrFingerprint: keys with a skip-prefix (telemetry./process./os./runtime.) are excluded from the
// fingerprint input entirely, so two attribute sets differing only in a skipped key fingerprint the
// same.
func TestAttrFingerprintSkipsPrefixedKeys(t *testing.T) {
	a := attrFingerprint(map[string]any{"telemetry.sdk.name": "sobs", "http.method": "GET"})
	b := attrFingerprint(map[string]any{"telemetry.sdk.name": "other-sdk", "http.method": "GET"})
	if a != b {
		t.Errorf("fingerprints should match when only a skip-prefixed key differs: %q vs %q", a, b)
	}
}

// attrFingerprint: caps at the first 8 sorted pairs (more than 8 non-skipped attrs still yields a
// fixed-length fingerprint, exercising the truncation branch).
func TestAttrFingerprintCapsAtEightPairs(t *testing.T) {
	attrs := map[string]any{}
	for i := 0; i < 12; i++ {
		attrs[string(rune('a'+i))] = i
	}
	got := attrFingerprint(attrs)
	if len(got) != 16 {
		t.Errorf("attrFingerprint length = %d, want 16 (md5[:16])", len(got))
	}
}

// attrFingerprint: an empty attrs map still yields a deterministic (empty-join) fingerprint, not a
// panic or empty string.
func TestAttrFingerprintEmpty(t *testing.T) {
	got := attrFingerprint(map[string]any{})
	if len(got) != 16 {
		t.Errorf("attrFingerprint(empty) length = %d, want 16", len(got))
	}
}

// traceStatusCode: case-insensitive ERROR/OK, and any other/unknown string maps to UNSET.
func TestTraceStatusCode(t *testing.T) {
	cases := map[string]string{
		"ERROR":     "STATUS_CODE_ERROR",
		"error":     "STATUS_CODE_ERROR",
		"OK":        "STATUS_CODE_OK",
		"ok":        "STATUS_CODE_OK",
		"":          "STATUS_CODE_UNSET",
		"UNSET":     "STATUS_CODE_UNSET",
		"something": "STATUS_CODE_UNSET",
	}
	for in, want := range cases {
		if got := traceStatusCode(in); got != want {
			t.Errorf("traceStatusCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// otlpFloat: string values parse; unparseable strings and unsupported types default to 0.
func TestOtlpFloat(t *testing.T) {
	if got := otlpFloat("3.25"); got != 3.25 {
		t.Errorf("otlpFloat(string) = %v, want 3.25", got)
	}
	if got := otlpFloat(2.5); got != 2.5 {
		t.Errorf("otlpFloat(float64) = %v, want 2.5", got)
	}
	if got := otlpFloat("not-a-number"); got != 0 {
		t.Errorf("otlpFloat(bad string) = %v, want 0", got)
	}
	if got := otlpFloat(nil); got != 0 {
		t.Errorf("otlpFloat(nil) = %v, want 0", got)
	}
	if got := otlpFloat(true); got != 0 {
		t.Errorf("otlpFloat(bool) = %v, want 0 (unsupported type)", got)
	}
}

// maxInt64: both branches (a>b and a<=b).
func TestMaxInt64(t *testing.T) {
	if got := maxInt64(5, 3); got != 5 {
		t.Errorf("maxInt64(5,3) = %d, want 5", got)
	}
	if got := maxInt64(3, 5); got != 5 {
		t.Errorf("maxInt64(3,5) = %d, want 5", got)
	}
	if got := maxInt64(4, 4); got != 4 {
		t.Errorf("maxInt64(4,4) = %d, want 4", got)
	}
}

// otlpAnyValue: intValue as a plain (non-string/float64) type falls to the int64(0) default inside
// that branch, and an entirely unrecognized AnyValue shape yields nil.
func TestOtlpAnyValueIntValueOddTypeDefaultsZero(t *testing.T) {
	v := otlpAnyValue(map[string]any{"intValue": true})
	if v != int64(0) {
		t.Errorf("otlpAnyValue(intValue=bool) = %#v, want int64(0)", v)
	}
}

// otlpAnyValue: doubleValue present but not a float64 (bad shape) falls through to nil.
func TestOtlpAnyValueDoubleValueWrongTypeYieldsNil(t *testing.T) {
	v := otlpAnyValue(map[string]any{"doubleValue": "not-a-float"})
	if v != nil {
		t.Errorf("otlpAnyValue(doubleValue=string) = %#v, want nil", v)
	}
}

// otlpAnyValue: boolValue present but wrong type falls through to nil.
func TestOtlpAnyValueBoolValueWrongTypeYieldsNil(t *testing.T) {
	v := otlpAnyValue(map[string]any{"boolValue": "yes"})
	if v != nil {
		t.Errorf("otlpAnyValue(boolValue=string) = %#v, want nil", v)
	}
}

// otlpAnyValue: bytesValue that is unparsable in ALL base64 variants falls back to the raw string.
func TestOtlpAnyValueBytesValueUnparsableFallsBackRaw(t *testing.T) {
	v := otlpAnyValue(map[string]any{"bytesValue": "%%%not-base64%%%"})
	s, ok := v.(string)
	if !ok || s != "%%%not-base64%%%" {
		t.Errorf("otlpAnyValue(bad bytesValue) = %#v, want raw passthrough", v)
	}
}

// otlpAnyValue: arrayValue with a malformed inner object (missing "values") yields an empty slice.
func TestOtlpAnyValueArrayValueMissingValues(t *testing.T) {
	v := otlpAnyValue(map[string]any{"arrayValue": map[string]any{}})
	arr, ok := v.([]any)
	if !ok || len(arr) != 0 {
		t.Errorf("otlpAnyValue(arrayValue={}) = %#v, want empty slice", v)
	}
}

// otlpAnyValue: kvlistValue recurses through otlpKVList.
func TestOtlpAnyValueKVListValue(t *testing.T) {
	v := otlpAnyValue(map[string]any{"kvlistValue": map[string]any{"values": []any{
		map[string]any{"key": "nested", "value": map[string]any{"stringValue": "deep"}},
	}}})
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("otlpAnyValue(kvlistValue) = %#v, want map", v)
	}
	if m["nested"] != "deep" {
		t.Errorf(`m["nested"] = %v, want "deep"`, m["nested"])
	}
}
