package main

// Unit tests for assorted pure helpers:
//   - compression (TestCompression)
//   - OTLP CORS origin matching + Vary header (TestOtlpCors)
//   - seasonal anomaly rule evaluation (TestMetricsAnomalyDetection)

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Compression
// ---------------------------------------------------------------------------

func TestCompressDecompressRoundtrip(t *testing.T) {
	text := strings.Repeat("Hello, World! ", 100)
	got, err := decompress(compress(text))
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if got != text {
		t.Errorf("roundtrip mismatch")
	}
}

func TestCompressJsonRoundtrip(t *testing.T) {
	obj := map[string]any{"key": "value", "num": float64(42), "list": []any{float64(1), float64(2), float64(3)}}
	got, err := decompressJson(compressJson(obj))
	if err != nil {
		t.Fatalf("decompressJson: %v", err)
	}
	if !reflect.DeepEqual(got, obj) {
		t.Errorf("got %#v, want %#v", got, obj)
	}
}

func TestCompressedSmallerThanPlain(t *testing.T) {
	text := strings.Repeat("INFO This is a repeating log message. ", 50)
	if len(compress(text)) >= len(text) {
		t.Errorf("compressed (%d) not smaller than plain (%d)", len(compress(text)), len(text))
	}
}

func TestDecompressNoneReturnsEmpty(t *testing.T) {
	got, err := decompress(nil)
	if err != nil || got != "" {
		t.Errorf("got %q, err %v; want empty", got, err)
	}
}

func TestDecompressJsonNoneReturnsEmptyDict(t *testing.T) {
	got, err := decompressJson(nil)
	if err != nil {
		t.Fatalf("err %v", err)
	}
	if !reflect.DeepEqual(got, map[string]any{}) {
		t.Errorf("got %#v, want empty map", got)
	}
}

// ---------------------------------------------------------------------------
// OTLP CORS origin matching
// ---------------------------------------------------------------------------

// withCorsOrigins temporarily replaces the allow-list global and restores it.
func withCorsOrigins(t *testing.T, origins ...string) {
	t.Helper()
	prev := otlpCorsAllowedOrigins
	otlpCorsAllowedOrigins = origins
	t.Cleanup(func() { otlpCorsAllowedOrigins = prev })
}

func TestOriginAllowedWildcardPort(t *testing.T) {
	withCorsOrigins(t, "http://localhost:*")
	if !originAllowedForOtlp("http://localhost:3000") {
		t.Error("localhost:3000 should be allowed by wildcard port")
	}
}

func TestOriginAllowedRejectsDisallowed(t *testing.T) {
	withCorsOrigins(t, "http://localhost:*")
	if originAllowedForOtlp("https://evil.example.com") {
		t.Error("evil.example.com should be rejected")
	}
}

func TestOriginPatternWithoutPortRejectsNonDefaultPort(t *testing.T) {
	withCorsOrigins(t, "https://example.com")
	if originAllowedForOtlp("https://example.com:8443") {
		t.Error("non-default port 8443 must not match port-less pattern")
	}
}

func TestOriginPatternWithoutPortMatchesNoPort(t *testing.T) {
	withCorsOrigins(t, "https://example.com")
	if !originAllowedForOtlp("https://example.com") {
		t.Error("port-less origin should match port-less pattern")
	}
}

func TestOriginPatternWithoutPortMatchesDefaultPort(t *testing.T) {
	withCorsOrigins(t, "https://example.com")
	if !originAllowedForOtlp("https://example.com:443") {
		t.Error("default port 443 should match port-less pattern")
	}
}

func TestOriginRejectsInvalidOrigin(t *testing.T) {
	withCorsOrigins(t, "http://localhost:*")
	if originAllowedForOtlp("not-an-origin") {
		t.Error("malformed origin should be rejected")
	}
	if originAllowedForOtlp("") {
		t.Error("empty origin should be rejected")
	}
}

func TestOriginRejectsInvalidPort(t *testing.T) {
	withCorsOrigins(t, "https://example.com")
	if originAllowedForOtlp("https://example.com:abc") {
		t.Error("non-numeric port should be rejected")
	}
}

func TestAppendVaryCaseInsensitiveDedup(t *testing.T) {
	h := http.Header{}
	appendVaryHeader(h, "Origin")
	appendVaryHeader(h, "origin") // different case must not duplicate
	tokens := []string{}
	for tk := range strings.SplitSeq(h.Get("Vary"), ",") {
		if s := strings.TrimSpace(tk); s != "" {
			tokens = append(tokens, s)
		}
	}
	if len(tokens) != 1 {
		t.Errorf("Vary = %q, want exactly 1 token", h.Get("Vary"))
	}
}

func TestAppendVaryAppendsDistinct(t *testing.T) {
	h := http.Header{}
	h.Set("Vary", "Accept-Encoding")
	appendVaryHeader(h, "Origin")
	if v := h.Get("Vary"); v != "Accept-Encoding, Origin" {
		t.Errorf("Vary = %q, want 'Accept-Encoding, Origin'", v)
	}
}

// ---------------------------------------------------------------------------
// Seasonal anomaly rules
// ---------------------------------------------------------------------------

func TestBuildSeasonalBucketExpr(t *testing.T) {
	if got := buildSeasonalBucketExpr("hour_of_day"); got != "toHour(time)" {
		t.Errorf("hour_of_day = %q", got)
	}
	if got := buildSeasonalBucketExpr("day_of_week"); got != "toDayOfWeek(time)" {
		t.Errorf("day_of_week = %q", got)
	}
	if got := buildSeasonalBucketExpr("unknown"); got != "toHour(time)" {
		t.Errorf("unknown should default to toHour, got %q", got)
	}
}

func hourBuckets() string {
	// buckets {"0":{...}..."23":{...}} with warning=h*10+5, critical=h*10+9
	parts := make([]string, 24)
	for h := range 24 {
		parts[h] = fmt.Sprintf(`"%d":{"warning":%d,"critical":%d}`, h, h*10+5, h*10+9)
	}
	return `{"strategy":"hour_of_day","buckets":{` + strings.Join(parts, ",") + `}}`
}

func seasonalRule(bucketsJSON string) map[string]any {
	return map[string]any{
		"id":                    "test-id",
		"name":                  "test-seasonal",
		"comparator":            "gt",
		"warning_threshold":     999.0,
		"critical_threshold":    9999.0,
		"min_sample_count":      1,
		"seasonal_buckets_json": bucketsJSON,
	}
}

func TestEvaluateSeasonalRuleUsesBucketThreshold(t *testing.T) {
	rule := seasonalRule(hourBuckets())
	// hour 2: warning=25, critical=29
	r := evaluateSeasonalRule(rule, 27.0, 1, "2024-01-01 02:30:00")
	if r == nil || r["rule_state"] != "warning" || r["rule_seasonal"] != true {
		t.Errorf("27 @ h2: %#v, want warning/seasonal", r)
	}
	r = evaluateSeasonalRule(rule, 30.0, 1, "2024-01-01 02:30:00")
	if r == nil || r["rule_state"] != "outlier" || r["rule_seasonal"] != true {
		t.Errorf("30 @ h2: %#v, want outlier/seasonal", r)
	}
}

func TestEvaluateSeasonalRuleFallsBackToGlobal(t *testing.T) {
	rule := seasonalRule(`{"strategy":"hour_of_day","buckets":{}}`)
	rule["warning_threshold"] = 10.0
	rule["critical_threshold"] = 20.0
	r := evaluateSeasonalRule(rule, 15.0, 1, "2024-01-01 06:00:00")
	if r == nil || r["rule_state"] != "warning" || r["rule_seasonal"] != false {
		t.Errorf("%#v, want warning + non-seasonal", r)
	}
}

func TestEvaluateSeasonalRuleNoJsonFallsBack(t *testing.T) {
	rule := seasonalRule("")
	rule["warning_threshold"] = 5.0
	rule["critical_threshold"] = 10.0
	r := evaluateSeasonalRule(rule, 7.0, 1, "2024-01-01 10:00:00")
	if r == nil || r["rule_state"] != "warning" || r["rule_seasonal"] != false {
		t.Errorf("%#v, want warning + non-seasonal", r)
	}
}

func TestEvaluateSeasonalRuleRespectsMinSampleCount(t *testing.T) {
	rule := seasonalRule(`{"strategy":"hour_of_day","buckets":{"5":{"warning":1.0,"critical":2.0}}}`)
	rule["warning_threshold"] = 1.0
	rule["critical_threshold"] = 2.0
	rule["min_sample_count"] = 10
	if r := evaluateSeasonalRule(rule, 5.0, 3, "2024-01-01 05:00:00"); r != nil {
		t.Errorf("sample 3 < min 10 should return nil, got %#v", r)
	}
}

func TestEvaluateSeasonalRuleDayOfWeek(t *testing.T) {
	// 2024-01-01 is Monday (isoweekday 1)
	rule := seasonalRule(`{"strategy":"day_of_week","buckets":{"1":{"warning":50.0,"critical":100.0}}}`)
	r := evaluateSeasonalRule(rule, 75.0, 1, "2024-01-01 12:00:00")
	if r == nil || r["rule_state"] != "warning" || r["rule_seasonal"] != true {
		t.Errorf("%#v, want warning/seasonal", r)
	}
}

func TestEvaluateSeasonalRuleNormalizesOffsetToUtc(t *testing.T) {
	// 02:30 at -05:00 == 07:30 UTC → bucket key 7
	rule := seasonalRule(`{"strategy":"hour_of_day","buckets":{"7":{"warning":10.0,"critical":20.0}}}`)
	r := evaluateSeasonalRule(rule, 15.0, 1, "2024-01-01T02:30:00-05:00")
	if r == nil || r["rule_state"] != "warning" || r["rule_seasonal"] != true {
		t.Errorf("%#v, want warning/seasonal (UTC hour 7)", r)
	}
}
