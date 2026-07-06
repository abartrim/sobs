package main

import (
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b12_dbutil_test.go — coverage-gate batch 12 targeted coverage for cmd/sobs/dbutil.go's
// undertested pure helpers (parseJSONObjectOrdered, cStrDef, cStr, cFloat, parseReportFiltersNative)
// and the DB-backed dmSettingValue, whose sensitive-key decrypt branch was previously unreached.

// --- parseJSONObjectOrdered --------------------------------------------------------------------

func TestParseJSONObjectOrderedBlank(t *testing.T) {
	obj := parseJSONObjectOrdered("   ")
	if obj.Len() != 0 {
		t.Fatalf("blank input: want empty object, got %d keys", obj.Len())
	}
}

func TestParseJSONObjectOrderedInvalidJSON(t *testing.T) {
	obj := parseJSONObjectOrdered("{not json")
	if obj.Len() != 0 {
		t.Fatalf("invalid JSON: want empty object, got %d keys", obj.Len())
	}
}

func TestParseJSONObjectOrderedNonObjectTop(t *testing.T) {
	// A top-level JSON array parses fine but is not an object -> empty object fallback.
	obj := parseJSONObjectOrdered(`[1,2,3]`)
	if obj.Len() != 0 {
		t.Fatalf("array top-level: want empty object, got %d keys", obj.Len())
	}
}

func TestParseJSONObjectOrderedPreservesInsertionOrder(t *testing.T) {
	obj := parseJSONObjectOrdered(`{"z": 1, "a": 2, "m": 3}`)
	want := []string{"z", "a", "m"}
	got := obj.Keys()
	if len(got) != len(want) {
		t.Fatalf("Keys() = %v, want %v", got, want)
	}
	for i, k := range want {
		if got[i] != k {
			t.Fatalf("Keys()[%d] = %q, want %q (order not preserved): %v", i, got[i], k, got)
		}
	}
}

// --- cStrDef -----------------------------------------------------------------------------------

func TestCStrDefNilYieldsDefault(t *testing.T) {
	m := map[string]any{}
	if got := cStrDef(m, "missing", "fallback"); got != "fallback" {
		t.Errorf("cStrDef(missing key) = %q, want fallback", got)
	}
}

func TestCStrDefEmptyStringYieldsDefault(t *testing.T) {
	m := map[string]any{"k": ""}
	if got := cStrDef(m, "k", "fallback"); got != "fallback" {
		t.Errorf("cStrDef(empty string) = %q, want fallback", got)
	}
}

func TestCStrDefNonEmptyStringPassesThrough(t *testing.T) {
	m := map[string]any{"k": "value"}
	if got := cStrDef(m, "k", "fallback"); got != "value" {
		t.Errorf("cStrDef(non-empty string) = %q, want value", got)
	}
}

func TestCStrDefZeroFloatYieldsDefault(t *testing.T) {
	m := map[string]any{"k": float64(0)}
	if got := cStrDef(m, "k", "fallback"); got != "fallback" {
		t.Errorf("cStrDef(0.0) = %q, want fallback", got)
	}
}

func TestCStrDefNonZeroFloatFormatsViaCStr(t *testing.T) {
	m := map[string]any{"k": float64(42)}
	if got := cStrDef(m, "k", "fallback"); got != "42" {
		t.Errorf("cStrDef(42.0) = %q, want 42", got)
	}
}

func TestCStrDefOtherTypeFallsThroughToCStr(t *testing.T) {
	m := map[string]any{"k": true}
	if got := cStrDef(m, "k", "fallback"); got != "True" {
		t.Errorf("cStrDef(bool true) = %q, want True", got)
	}
}

// --- cStr (default/bool/float branches not covered elsewhere) ----------------------------------

func TestCStrBoolBranches(t *testing.T) {
	if got := cStr(map[string]any{"k": true}, "k"); got != "True" {
		t.Errorf("cStr(true) = %q, want True", got)
	}
	if got := cStr(map[string]any{"k": false}, "k"); got != "False" {
		t.Errorf("cStr(false) = %q, want False", got)
	}
}

func TestCStrNilBranch(t *testing.T) {
	if got := cStr(map[string]any{"k": nil}, "k"); got != "" {
		t.Errorf("cStr(nil) = %q, want empty string", got)
	}
}

func TestCStrDefaultBranch(t *testing.T) {
	type customType struct{ N int }
	got := cStr(map[string]any{"k": customType{N: 3}}, "k")
	if got != "{3}" {
		t.Errorf("cStr(customType) = %q, want {3}", got)
	}
}

// --- cFloat --------------------------------------------------------------------------------------

func TestCFloatFromFloat64(t *testing.T) {
	if got := cFloat(map[string]any{"k": 3.5}, "k"); got != 3.5 {
		t.Errorf("cFloat(float64) = %v, want 3.5", got)
	}
}

func TestCFloatFromString(t *testing.T) {
	if got := cFloat(map[string]any{"k": "2.25"}, "k"); got != 2.25 {
		t.Errorf("cFloat(string) = %v, want 2.25", got)
	}
}

func TestCFloatFromBadStringYieldsZero(t *testing.T) {
	if got := cFloat(map[string]any{"k": "not-a-number"}, "k"); got != 0 {
		t.Errorf("cFloat(bad string) = %v, want 0", got)
	}
}

func TestCFloatMissingKeyYieldsZero(t *testing.T) {
	if got := cFloat(map[string]any{}, "k"); got != 0 {
		t.Errorf("cFloat(missing) = %v, want 0", got)
	}
}

// --- parseReportFiltersNative --------------------------------------------------------------------

func TestParseReportFiltersNativeBlank(t *testing.T) {
	got := parseReportFiltersNative("  ")
	obj, ok := got.(interface{ Len() int })
	if !ok || obj.Len() != 0 {
		t.Fatalf("blank input: want empty object, got %#v", got)
	}
}

func TestParseReportFiltersNativeInvalidJSON(t *testing.T) {
	got := parseReportFiltersNative("not json")
	obj, ok := got.(interface{ Len() int })
	if !ok || obj.Len() != 0 {
		t.Fatalf("invalid JSON: want empty object, got %#v", got)
	}
}

func TestParseReportFiltersNativeNonObjectTop(t *testing.T) {
	// A valid JSON array decodes but is not an object -> falls back to empty object.
	got := parseReportFiltersNative(`["a","b"]`)
	obj, ok := got.(interface{ Len() int })
	if !ok || obj.Len() != 0 {
		t.Fatalf("array top-level: want empty object fallback, got %#v", got)
	}
}

func TestParseReportFiltersNativeValidObject(t *testing.T) {
	got := parseReportFiltersNative(`{"service": "web", "limit": 10}`)
	obj, ok := got.(interface{ Get(string) (any, bool) })
	if !ok {
		t.Fatalf("valid object: got %#v, want a *jsonenc.Object-like value", got)
	}
	v, present := obj.Get("service")
	if !present || v != "web" {
		t.Fatalf("Get(service) = %v, %v; want web, true", v, present)
	}
}

// --- dmSettingValue --------------------------------------------------------------------------

func TestDMSettingValueNonSensitivePassthrough(t *testing.T) {
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return storetest.Result([]string{"Value"}, []any{"plain-value"}), nil
	}}
	s := &server{db: fake, cfg: config{}}
	got := s.dmSettingValue("data_management.some_non_sensitive_key")
	if got != "plain-value" {
		t.Fatalf("dmSettingValue(non-sensitive) = %q, want plain-value", got)
	}
}

func TestDMSettingValueSensitiveKeyNoEncryptionSecretIsNoop(t *testing.T) {
	// isSensitiveDMSettingKey routes through decryptSecretValue; with no configured
	// EncryptionSecret and a plaintext (unprefixed) stored value, decryptSecretValue is a
	// pass-through (matches the parity invariant: no key configured = plaintext).
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return storetest.Result([]string{"Value"}, []any{"plain-secret"}), nil
	}}
	s := &server{db: fake, cfg: config{}}
	got := s.dmSettingValue("data_management.s3_secret_access_key")
	if got != "plain-secret" {
		t.Fatalf("dmSettingValue(sensitive, no key, plaintext) = %q, want plain-secret (passthrough)", got)
	}
}

func TestDMSettingValueSensitiveKeyEncryptedNoSecretYieldsEmpty(t *testing.T) {
	// A value prefixed enc:v1: with no configured EncryptionSecret cannot be decrypted ->
	// decryptSecretValue's `secret == ""` branch returns "".
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return storetest.Result([]string{"Value"}, []any{"enc:v1:deadbeef"}), nil
	}}
	s := &server{db: fake, cfg: config{}}
	got := s.dmSettingValue("data_management.backup_encryption_password")
	if got != "" {
		t.Fatalf("dmSettingValue(sensitive, encrypted, no key) = %q, want empty", got)
	}
}

func TestDMSettingValueSensitiveKeyRoundTripsWithEncryptionSecret(t *testing.T) {
	s := &server{cfg: config{EncryptionSecret: "test-secret"}}
	encrypted := s.encryptSecretValue("super-secret-password")
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return storetest.Result([]string{"Value"}, []any{encrypted}), nil
	}}
	s.db = fake
	got := s.dmSettingValue("data_management.backup_encryption_password")
	if got != "super-secret-password" {
		t.Fatalf("dmSettingValue round-trip = %q, want super-secret-password", got)
	}
}

func TestDMSettingValueMissingKeyYieldsEmpty(t *testing.T) {
	fake := &storetest.FakeDB{} // zero-value FakeDB: every query returns an empty result
	s := &server{db: fake}
	if got := s.dmSettingValue("data_management.s3_secret_access_key"); got != "" {
		t.Fatalf("dmSettingValue(missing) = %q, want empty", got)
	}
}
