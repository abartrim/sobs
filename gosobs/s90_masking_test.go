package main

// Unit tests for the masking module (port of tests/test_app.py::TestMasking).
// Python tuple/set/frozenset/deepcopy cases are intentionally omitted: those
// exercise CPython-specific value semantics that have no Go analog.

import (
	"strings"
	"testing"
)

// restoreDefaultMaskingRules resets the package-global rule set after a test
// that mutated it via maskingConfigureRuntimeRules.
func restoreDefaultMaskingRules(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := maskingConfigureRuntimeRules(nil, nil); err != nil {
			t.Fatalf("restore default masking rules: %v", err)
		}
	})
}

func maskStr(t *testing.T, v any) string {
	t.Helper()
	out, ok := maskingMaskValue(v).(string)
	if !ok {
		t.Fatalf("expected string result, got %T", maskingMaskValue(v))
	}
	return out
}

func TestMaskingEmailInString(t *testing.T) {
	result := maskStr(t, "User john.doe@example.com logged in")
	if strings.Contains(result, "john.doe@example.com") {
		t.Errorf("email not masked: %q", result)
	}
	for _, want := range []string{"****", "User", "logged in"} {
		if !strings.Contains(result, want) {
			t.Errorf("missing %q in %q", want, result)
		}
	}
}

func TestMaskingBearerTokenInString(t *testing.T) {
	result := maskStr(t, "Authorization: Bearer supersecrettoken123")
	if strings.Contains(result, "supersecrettoken123") {
		t.Errorf("token not masked: %q", result)
	}
	if !strings.Contains(result, "****") {
		t.Errorf("missing mask in %q", result)
	}
}

func TestMaskingJWTInString(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJzdWIiOiIxMjM0NTY3ODkwIn0." +
		"SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	result := maskStr(t, "token="+jwt)
	if strings.Contains(result, jwt) {
		t.Errorf("jwt not masked: %q", result)
	}
	if !strings.Contains(result, "****") {
		t.Errorf("missing mask in %q", result)
	}
}

func TestMaskingSSNInString(t *testing.T) {
	result := maskStr(t, "SSN: 123-45-6789 found in event")
	if strings.Contains(result, "123-45-6789") {
		t.Errorf("ssn not masked: %q", result)
	}
	if !strings.Contains(result, "****") {
		t.Errorf("missing mask in %q", result)
	}
}

func TestMaskingCreditCardInString(t *testing.T) {
	result := maskStr(t, "card 4111111111111111 charged")
	if strings.Contains(result, "4111111111111111") {
		t.Errorf("card not masked: %q", result)
	}
	if !strings.Contains(result, "****") {
		t.Errorf("missing mask in %q", result)
	}
}

func TestMaskingAWSKeyInString(t *testing.T) {
	result := maskStr(t, "AKIAIOSFODNN7EXAMPLE was exposed")
	if strings.Contains(result, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("aws key not masked: %q", result)
	}
	if !strings.Contains(result, "****") {
		t.Errorf("missing mask in %q", result)
	}
}

func TestMaskingSensitiveKeyInDict(t *testing.T) {
	d := map[string]any{"username": "alice", "password": "supersecret", "service": "myapp"}
	result := maskingMaskValue(d).(map[string]any)
	if result["password"] != "****" {
		t.Errorf("password = %v, want ****", result["password"])
	}
	if result["username"] != "alice" {
		t.Errorf("username = %v, want alice", result["username"])
	}
	if result["service"] != "myapp" {
		t.Errorf("service = %v, want myapp", result["service"])
	}
}

func TestMaskingSensitiveKeyCaseInsensitive(t *testing.T) {
	d := map[string]any{"Password": "s3cr3t", "API_KEY": "abc123", "host": "localhost"}
	result := maskingMaskValue(d).(map[string]any)
	if result["Password"] != "****" {
		t.Errorf("Password = %v, want ****", result["Password"])
	}
	if result["API_KEY"] != "****" {
		t.Errorf("API_KEY = %v, want ****", result["API_KEY"])
	}
	if result["host"] != "localhost" {
		t.Errorf("host = %v, want localhost", result["host"])
	}
}

func TestMaskingNestedDict(t *testing.T) {
	d := map[string]any{"outer": map[string]any{"inner": map[string]any{"token": "secrettoken", "metric": "42"}}}
	result := maskingMaskValue(d).(map[string]any)
	inner := result["outer"].(map[string]any)["inner"].(map[string]any)
	if inner["token"] != "****" {
		t.Errorf("token = %v, want ****", inner["token"])
	}
	if inner["metric"] != "42" {
		t.Errorf("metric = %v, want 42", inner["metric"])
	}
}

func TestMaskingListOfDicts(t *testing.T) {
	data := []any{
		map[string]any{"api_key": "KEY123", "name": "svc-a"},
		map[string]any{"api_key": "KEY456", "name": "svc-b"},
	}
	result := maskingMaskValue(data).([]any)
	r0 := result[0].(map[string]any)
	r1 := result[1].(map[string]any)
	if r0["api_key"] != "****" || r1["api_key"] != "****" {
		t.Errorf("api_key not masked: %v, %v", r0["api_key"], r1["api_key"])
	}
	if r0["name"] != "svc-a" {
		t.Errorf("name = %v, want svc-a", r0["name"])
	}
}

func TestMaskingNonSensitiveStringPassThrough(t *testing.T) {
	text := "http_requests_total 1234 latency_p99 42ms error_rate 0.01"
	if got := maskStr(t, text); got != text {
		t.Errorf("pass-through changed: %q != %q", got, text)
	}
}

func TestMaskingNonSensitiveDictPassThrough(t *testing.T) {
	d := map[string]any{"service": "myapp", "status": "ok", "duration_ms": 120}
	result := maskingMaskValue(d).(map[string]any)
	for k, v := range d {
		if result[k] != v {
			t.Errorf("field %q changed: %v != %v", k, result[k], v)
		}
	}
}

func TestMaskingNilReturnsNil(t *testing.T) {
	if maskingMaskValue(nil) != nil {
		t.Errorf("expected nil, got %v", maskingMaskValue(nil))
	}
}

func TestMaskingNilValueInsideDictPassesThrough(t *testing.T) {
	result := maskingMaskValue(map[string]any{"service": "svc", "detail": nil}).(map[string]any)
	if result["detail"] != nil {
		t.Errorf("detail = %v, want nil", result["detail"])
	}
	if result["service"] != "svc" {
		t.Errorf("service = %v, want svc", result["service"])
	}
}

func TestMaskingOriginalDictNotMutated(t *testing.T) {
	original := map[string]any{"password": "mysecret", "service": "svc"}
	_ = maskingMaskValue(original)
	if original["password"] != "mysecret" {
		t.Errorf("original mutated: password = %v", original["password"])
	}
}

func TestMaskStringCoercesDictToJSONWithKeysMasked(t *testing.T) {
	result := maskingMaskString(map[string]any{"password": "secret", "service": "test"})
	if strings.Contains(result, "secret") {
		t.Errorf("secret leaked: %q", result)
	}
	if !strings.Contains(result, "****") || !strings.Contains(result, "test") {
		t.Errorf("unexpected result: %q", result)
	}
}

func TestMaskStringOnNilReturnsEmpty(t *testing.T) {
	if got := maskingMaskString(nil); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestMaskStringGithubIssueBody(t *testing.T) {
	title := "Error in service billing@finance.example.com"
	body := "## Issue\n\nUser email: ops@company.example.com\npassword=hunter2\n"
	mt := maskingMaskString(title)
	mb := maskingMaskString(body)
	if strings.Contains(mt, "billing@finance.example.com") {
		t.Errorf("title email leaked: %q", mt)
	}
	if strings.Contains(mb, "ops@company.example.com") || strings.Contains(mb, "hunter2") {
		t.Errorf("body secret leaked: %q", mb)
	}
	if !strings.Contains(mt, "****") || !strings.Contains(mb, "****") {
		t.Errorf("missing mask: %q / %q", mt, mb)
	}
	if !strings.Contains(mt, "Error in service") || !strings.Contains(mb, "## Issue") {
		t.Errorf("structure lost: %q / %q", mt, mb)
	}
}

func TestMaskingValidatePatternErrorsOnEmpty(t *testing.T) {
	if _, err := maskingValidatePattern(""); err == nil || err.Error() != "Pattern is required" {
		t.Errorf("expected 'Pattern is required', got %v", err)
	}
}

func TestMaskingValidatePatternErrorsOnWhitespace(t *testing.T) {
	if _, err := maskingValidatePattern("   "); err == nil || err.Error() != "Pattern is required" {
		t.Errorf("expected 'Pattern is required', got %v", err)
	}
}

func TestMaskingValidatePatternRejectsInvalidRegex(t *testing.T) {
	if _, err := maskingValidatePattern("("); err == nil {
		t.Error("expected error for invalid regex, got nil")
	}
}

func TestMaskingConfigureRuntimeRulesCustomKeyAndPattern(t *testing.T) {
	restoreDefaultMaskingRules(t)
	if _, err := maskingConfigureRuntimeRules([]string{"my_secret_field"}, []string{`CUSTOM-\d{6}`}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	result := maskingMaskValue(map[string]any{"my_secret_field": "should_be_masked"}).(map[string]any)
	if result["my_secret_field"] != "****" {
		t.Errorf("custom key not masked: %v", result["my_secret_field"])
	}
	if got := maskStr(t, "token CUSTOM-123456 in log"); strings.Contains(got, "CUSTOM-123456") {
		t.Errorf("custom pattern not masked: %q", got)
	}
}

func TestMaskingConfigureRuntimeRulesDeduplicatesPatterns(t *testing.T) {
	restoreDefaultMaskingRules(t)
	if _, err := maskingConfigureRuntimeRules(nil, []string{`DUP-\d+`, `DUP-\d+`}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	count := 0
	for _, p := range maskingSensitivePatterns {
		if strings.Contains(p, "DUP") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 DUP pattern, got %d", count)
	}
}

func TestMaskingGetFilterRebuildsWhenNil(t *testing.T) {
	restoreDefaultMaskingRules(t)
	maskingFilterMu.Lock()
	maskingFilter = nil
	maskingFilterMu.Unlock()
	result := maskStr(t, "john@example.com")
	if strings.Contains(result, "john@example.com") || !strings.Contains(result, "****") {
		t.Errorf("filter not rebuilt: %q", result)
	}
}

func TestDedupePreserveOrder(t *testing.T) {
	got := dedupePreserveOrder([]string{"b", "a", "b", "c", "a"})
	want := []string{"b", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMaskingNormalizeSensitiveKey(t *testing.T) {
	cases := map[any]string{
		"  Password  ": "password",
		"API_KEY":      "api_key",
		nil:            "",
		123:            "123",
	}
	for in, want := range cases {
		if got := maskingNormalizeSensitiveKey(in); got != want {
			t.Errorf("normalize(%v) = %q, want %q", in, got, want)
		}
	}
}
