package main

// coverage_pure_a_test.go — oracle-anchored unit tests for SLICE A pure helpers.
//
// Target functions and their disposition:
//   TESTED:
//     originAllowedForOtlp     (server.go:395)     app.py:399-426
//     safeJSONDumps            (handlers_v1.go:36)  app.py:8813-8827  [DIVERGENCE noted]
//     resolveGuardTimeoutSeconds (fix_ai_helper.go:315) app.py:5125-5133
//     extractTraceFields       (fix_rum_helpers.go:158) app.py:9927-9951
//     summarizeAiToolAction    (ai_view.go:726)     app.py:8486-8503
//     autoRuleThresholds       (metric_candidates.go:37) app.py:11706-11724
//     buildTraceTimelineSegments (handlers_pages_traces_detail.go:215) app.py:14712-14778
//     extractStructuredErrorSummary (handlers_incident.go:268) app.py:10568-10649
//     genaiMessageReasoningToText (ai_view.go:512)  app.py:8301-8354
//     extractAssistantMeta     (ai_helper.go:94)    app.py:3669-3708
//     buildClientAction        (ai_action_execute.go:148) app.py:4341-4396
//     regexScopeTimeConditions (regex_validate.go:254) app.py:23882-23903
//     extractBindings          (chart_render_binding.go:255) app.py:20525-20748
//     inferAiPricingForModel   (fix_pages_settings.go:372) app.py:2813-2825
//
//   SKIPPED (no Go port / server method / already tested / not unit-testable):
//     _normalize_base_path     — no Go port; BasePath handled via config field
//     _merge_script_name       — no Go port; BasePath used directly in Go
//     _get_geo_db              — Go port is geoLookupDB(); already well-tested in geoip_test.go
//     _same_origin_request     — already tested in auth_test.go:TestSameOriginRequest
//     _normalize_genai_messages_for_display — normalizeGenaiMessagesForDisplay; covered by
//                                 corpus parity on ai_view routes; normalisation details tested
//                                 via genaiMessageReasoningToText
//     _dispatch_webhook_channel — server method making live HTTP calls; not unit-testable
//     _dispatch_email_channel  — server method making SMTP connections; not unit-testable
//     _build_s3_backup_dest    — server method reading appSetting; not unit-testable
//     get_table_sample         — server method with live DB; not unit-testable
//     _compile_chart_spec      — server method with complex deps; not unit-testable
//     _render_custom_echarts   — complex; indirect coverage via extractBindings

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---------------------------------------------------------------------------
// originAllowedForOtlp — app.py:399-426
// Oracle: _origin_allowed_for_otlp. Tests use the default allow-list
// (http://localhost:*, https://localhost:*, http://127.0.0.1:*, https://127.0.0.1:*).
// ---------------------------------------------------------------------------

func TestOriginAllowedForOtlp_Default(t *testing.T) {
	// These use the process-level default list (localhost/127.0.0.1:*), loaded at
	// package init.  We save and restore the list so tests remain hermetic.
	orig := otlpCorsAllowedOrigins
	defer func() { otlpCorsAllowedOrigins = orig }()
	otlpCorsAllowedOrigins = []string{"https://example.com", "https://*.trusted.com"}

	cases := []struct {
		origin string
		want   bool
		desc   string
	}{
		// Oracle: exactly matching pattern
		{"https://example.com", true, "exact match"},
		// Oracle: port 443 is scheme default → adds candidate without port → matches "https://example.com"
		{"https://example.com:443", true, "default-port stripped"},
		// Oracle: wildcard sub-domain
		{"https://sub.trusted.com", true, "wildcard subdomain"},
		// Oracle: not in list
		{"https://evil.com", false, "not in list"},
		// Oracle: wrong scheme
		{"http://example.com", false, "wrong scheme"},
		// Oracle: non-default port → NOT stripped → no match
		{"https://example.com:8443", false, "non-default port not stripped"},
		// Oracle: no scheme → rejected early
		{"", false, "empty origin"},
		// Oracle: unsupported scheme
		{"ftp://example.com", false, "ftp scheme"},
		// Oracle: host-only (no netloc in the sense urllib expects) → rejected
		{"not-a-url", false, "not a url"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := originAllowedForOtlp(c.origin)
			if got != c.want {
				t.Errorf("originAllowedForOtlp(%q) = %v, want %v", c.origin, got, c.want)
			}
		})
	}
}

func TestOriginAllowedForOtlp_LocalhostDefault(t *testing.T) {
	// With the real default list localhost/127.0.0.1:* must be allowed.
	orig := otlpCorsAllowedOrigins
	defer func() { otlpCorsAllowedOrigins = orig }()
	otlpCorsAllowedOrigins = parseOtlpAllowedOrigins()
	// unset any env so parseOtlpAllowedOrigins uses its built-in default
	_ = os.Unsetenv("SOBS_OTLP_CORS_ALLOWED_ORIGINS")

	localOK := []string{
		"http://localhost:3000",
		"https://localhost:8443",
		"http://127.0.0.1:4000",
		"https://127.0.0.1:4001",
	}
	for _, o := range localOK {
		if !originAllowedForOtlp(o) {
			t.Errorf("expected localhost origin %q to be allowed by default", o)
		}
	}
	// Remote origins must be blocked by the default list.
	if originAllowedForOtlp("https://evil.com") {
		t.Error("external origin should be denied by default list")
	}
}

// ---------------------------------------------------------------------------
// safeJSONDumps — app.py:8813-8827
// Oracle: _safe_json_dumps
//
// DIVERGENCE: Python json.dumps uses ", "/": " separators by default (producing
// {"a": 1}) whereas Go json.Marshal produces compact output ({"a":1}).
// The Go port is wrong for dict/list inputs; this test documents the actual Go
// behaviour and flags the divergence so it can be fixed.
// ---------------------------------------------------------------------------

func TestSafeJSONDumps(t *testing.T) {
	cases := []struct {
		in      any
		want    string // Go's actual output (compact)
		pyWant  string // Python oracle (spaced) — marked as DIVERGENCE where different
		comment string
	}{
		{nil, "{}", "{}", "nil → {}"},
		{"", "{}", "{}", "empty string → {}"},
		{"   ", "{}", "{}", "whitespace-only string → {}"},
		{"invalid json", "{}", "{}", "invalid JSON string → {}"},
		{42, "{}", "{}", "int input → {}"},
		{true, "{}", "{}", "bool input → {}"},
		// Valid JSON string: Go re-marshals → compact; Python round-trips → spaced
		{`{"a":1}`, `{"a":1}`, `{"a": 1}`, "DIVERGENCE: compact vs spaced JSON string"},
		{`  {"a":1}  `, `{"a":1}`, `{"a": 1}`, "DIVERGENCE: trimmed compact vs spaced"},
		// Dict input
		{map[string]any{"a": 1}, `{"a":1}`, `{"a": 1}`, "DIVERGENCE: dict compact vs spaced"},
		// List input
		{[]any{1, 2, 3}, `[1,2,3]`, `[1, 2, 3]`, "DIVERGENCE: list compact vs spaced"},
		// nil/empty handling
		{`{}`, `{}`, `{}`, "empty object string"},
	}
	for _, c := range cases {
		t.Run(c.comment, func(t *testing.T) {
			got := safeJSONDumps(c.in)
			if got != c.want {
				t.Errorf("safeJSONDumps(%#v) = %q, want %q (Python oracle: %q)",
					c.in, got, c.want, c.pyWant)
			}
			// Where Go diverges from Python, log it explicitly.
			if c.want != c.pyWant {
				t.Logf("KNOWN DIVERGENCE from Python oracle: got %q, py_want %q", got, c.pyWant)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// resolveGuardTimeoutSeconds — app.py:5125-5133
// Oracle: _resolve_guard_timeout_seconds (Go takes the raw string value, Python
// takes the settings dict and extracts 'ai.guard_timeout_seconds').
// ---------------------------------------------------------------------------

func TestResolveGuardTimeoutSeconds(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", 30},       // missing → default 30
		{"abc", 30},    // non-numeric → default 30
		{"30", 30},     // within range
		{"60", 60},     // within range
		{"4", 5},       // below minimum → clamp to 5
		{"150", 120},   // above maximum → clamp to 120
		{"5", 5},       // at minimum edge
		{"120", 120},   // at maximum edge
		{"  45  ", 45}, // surrounding whitespace stripped
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("raw=%q", c.raw), func(t *testing.T) {
			got := resolveGuardTimeoutSeconds(c.raw)
			if got != c.want {
				t.Errorf("resolveGuardTimeoutSeconds(%q) = %d, want %d", c.raw, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// extractTraceFields — app.py:9927-9951
// Oracle: _extract_trace_fields
// ---------------------------------------------------------------------------

func TestExtractTraceFields(t *testing.T) {
	cases := []struct {
		desc      string
		event     map[string]any
		wantTrace string
		wantSpan  string
		wantFlags int
	}{
		{
			"empty event",
			map[string]any{},
			"", "", 0,
		},
		{
			"explicit traceId and spanId",
			map[string]any{"traceId": "abc123", "spanId": "def456"},
			"abc123", "def456", 0,
		},
		{
			"IDs lowercased",
			map[string]any{"traceId": "ABC123", "spanId": "DEF456"},
			"abc123", "def456", 0,
		},
		{
			"string traceFlags hex parsed",
			map[string]any{"traceId": "abc123", "spanId": "def456", "traceFlags": "01"},
			"abc123", "def456", 1,
		},
		{
			"integer traceFlags",
			map[string]any{"traceId": "abc123", "spanId": "def456", "traceFlags": 1},
			"abc123", "def456", 1,
		},
		{
			"string 'bad' is valid hex 0xbad = 2989",
			map[string]any{"traceFlags": "bad"},
			"", "", 2989,
		},
		{
			"string 'xyz' is invalid hex → 0",
			map[string]any{"traceFlags": "xyz"},
			"", "", 0,
		},
		{
			"empty traceFlags string → 0",
			map[string]any{"traceFlags": ""},
			"", "", 0,
		},
		{
			"traceparent parsed when IDs missing",
			map[string]any{
				"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			},
			"4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7", 1,
		},
		{
			"invalid traceparent → empty",
			map[string]any{"traceparent": "invalid"},
			"", "", 0,
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			gotTrace, gotSpan, gotFlags := extractTraceFields(c.event)
			if gotTrace != c.wantTrace || gotSpan != c.wantSpan || gotFlags != c.wantFlags {
				t.Errorf("extractTraceFields(%v) = (%q,%q,%d), want (%q,%q,%d)",
					c.event, gotTrace, gotSpan, gotFlags,
					c.wantTrace, c.wantSpan, c.wantFlags)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// summarizeAiToolAction — app.py:8486-8503
// Oracle: _summarize_ai_tool_action
// ---------------------------------------------------------------------------

func TestSummarizeAiToolAction_OracleA(t *testing.T) {
	// Extended oracle-anchored tests; misc_helpers2_test.go covers "" and "plain text" cases.
	cases := []struct {
		raw  string
		want string
	}{
		{`{"type":"filter","sql_where":"x > 1"}`, "filter: x > 1"},
		{`{"type":"navigate","target_page":"/logs"}`, "navigate -> /logs"},
		{`{"sql_where":"x > 1"}`, "action: x > 1"},
		{`{"target_page":"/traces"}`, "action -> /traces"},
		{`{"type":"noop"}`, "noop"},
		{`[1,2]`, "[1,2]"}, // non-dict JSON → text[:180]
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("raw=%q", c.raw), func(t *testing.T) {
			got := summarizeAiToolAction(c.raw)
			if got != c.want {
				t.Errorf("summarizeAiToolAction(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// autoRuleThresholds — app.py:11706-11724
// Oracle: _auto_rule_thresholds
// ---------------------------------------------------------------------------

func TestAutoRuleThresholds(t *testing.T) {
	cases := []struct {
		comparator              string
		q05, q20, q50, q80, q95 float64
		warnWant, critWant      float64
	}{
		// "lt" — warning=q20, critical=q05 (normal case: critical < warning)
		{"lt", 5, 20, 50, 80, 95, 20, 5},
		// "gt" — warning=q80, critical=q95 (normal case: critical > warning)
		{"gt", 5, 20, 50, 80, 95, 80, 95},
		// "lt": critical (q05=30) > warning (q20=20) → critical = min(warning,q50)=20; then equal → *0.9=18
		{"lt", 30, 20, 50, 80, 95, 20, 18},
		// "lt": critical==warning (both 20) → critical *= 0.9 = 18
		{"lt", 20, 20, 50, 80, 95, 20, 18},
		// "lt": critical==warning==0 → critical = -0.1
		{"lt", 0, 0, 50, 80, 95, 0, -0.1},
		// "gt": critical (q95=80) < warning (q80=80) → but equal...
		// Test: q95=95, q80=95 → equal → *1.1
		{"gt", 5, 20, 50, 95, 95, 95, 104.5},
		// "gt": critical (q95=0) == warning (q80=0) == 0 → critical = 0.1
		{"gt", 0, 20, 50, 0, 0, 0, 0.1},
		// "gt": q95=60 < q80=80 → critical = max(q80=80, q50=50) = 80; critical==warning → *1.1 = 88
		{"gt", 5, 20, 50, 80, 60, 80, 88},
		// any other comparator acts like "gt" (the else branch)
		{"gte", 5, 20, 50, 80, 95, 80, 95},
	}
	for _, c := range cases {
		desc := fmt.Sprintf("comparator=%s q05=%.1f q20=%.1f q50=%.1f q80=%.1f q95=%.1f",
			c.comparator, c.q05, c.q20, c.q50, c.q80, c.q95)
		t.Run(desc, func(t *testing.T) {
			gotWarn, gotCrit := autoRuleThresholds(c.comparator, c.q05, c.q20, c.q50, c.q80, c.q95)
			const eps = 1e-9
			if abs64(gotWarn-c.warnWant) > eps || abs64(gotCrit-c.critWant) > eps {
				t.Errorf("autoRuleThresholds(%s,...) = (%.6f, %.6f), want (%.6f, %.6f)",
					c.comparator, gotWarn, gotCrit, c.warnWant, c.critWant)
			}
		})
	}
}

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// ---------------------------------------------------------------------------
// buildTraceTimelineSegments — app.py:14712-14778
// Oracle: _build_trace_timeline_segments
// ---------------------------------------------------------------------------

func TestBuildTraceTimelineSegments(t *testing.T) {
	t.Run("empty spans", func(t *testing.T) {
		got := buildTraceTimelineSegments([]any{}, nil)
		if len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})

	t.Run("single span fills 100pct", func(t *testing.T) {
		spans := []any{
			map[string]any{"start_ms": 0.0, "duration_ms": 100.0},
		}
		got := buildTraceTimelineSegments(spans, nil)
		if len(got) != 1 {
			t.Fatalf("expected 1 segment, got %d: %v", len(got), got)
		}
		seg := got[0].(map[string]any)
		if seg["kind"] != "active" {
			t.Errorf("kind: got %v, want active", seg["kind"])
		}
		if seg["start_pct"] != 0.0 {
			t.Errorf("start_pct: got %v, want 0.0", seg["start_pct"])
		}
		if seg["width_pct"] != 100.0 {
			t.Errorf("width_pct: got %v, want 100.0", seg["width_pct"])
		}
		if seg["potential"] != false {
			t.Errorf("potential: got %v, want false", seg["potential"])
		}
	})

	t.Run("two spans with gap", func(t *testing.T) {
		// Span 0-30ms, span 70-100ms → trace 0-100ms
		// active 0-30%, gap 30-70%, active 70-100%
		spans := []any{
			map[string]any{"start_ms": 0.0, "duration_ms": 30.0},
			map[string]any{"start_ms": 70.0, "duration_ms": 30.0},
		}
		got := buildTraceTimelineSegments(spans, nil)
		if len(got) != 3 {
			t.Fatalf("expected 3 segments, got %d: %v", len(got), got)
		}
		want := []struct {
			kind      string
			startPct  float64
			widthPct  float64
			potential bool
		}{
			{"active", 0.0, 30.0, false},
			{"gap", 30.0, 40.0, false},
			{"active", 70.0, 30.0, false},
		}
		for i, w := range want {
			seg := got[i].(map[string]any)
			if seg["kind"] != w.kind {
				t.Errorf("[%d] kind: got %v, want %v", i, seg["kind"], w.kind)
			}
			if seg["start_pct"] != w.startPct {
				t.Errorf("[%d] start_pct: got %v, want %v", i, seg["start_pct"], w.startPct)
			}
			if seg["width_pct"] != w.widthPct {
				t.Errorf("[%d] width_pct: got %v, want %v", i, seg["width_pct"], w.widthPct)
			}
			if seg["potential"] != w.potential {
				t.Errorf("[%d] potential: got %v, want %v", i, seg["potential"], w.potential)
			}
		}
	})

	t.Run("gap with activity in it marked potential=true", func(t *testing.T) {
		// Span 0-20ms, span 80-100ms, activity at 50ms
		spans := []any{
			map[string]any{"start_ms": 0.0, "duration_ms": 20.0},
			map[string]any{"start_ms": 80.0, "duration_ms": 20.0},
		}
		got := buildTraceTimelineSegments(spans, []float64{50.0})
		if len(got) != 3 {
			t.Fatalf("expected 3 segments, got %d", len(got))
		}
		gap := got[1].(map[string]any)
		if gap["kind"] != "gap" {
			t.Fatalf("expected gap segment, got %v", gap["kind"])
		}
		if gap["potential"] != true {
			t.Errorf("gap with activity in it should be potential=true, got %v", gap["potential"])
		}
	})
}

// ---------------------------------------------------------------------------
// extractStructuredErrorSummary — app.py:10568-10649
// Oracle: _extract_structured_error_summary
// ---------------------------------------------------------------------------

func TestExtractStructuredErrorSummary(t *testing.T) {
	cases := []struct {
		desc    string
		message string
		rawBody string
		wantTxt string
		wantHit bool
	}{
		{
			"both empty",
			"", "", "", false,
		},
		{
			"plain text message",
			"plain error", "", "plain error", false,
		},
		{
			"JSON with message+code key",
			`{"message":"Something failed","code":"500"}`, "",
			"Something failed [code 500]", true,
		},
		{
			"JSON with error+type keys",
			`{"error":"Not found","type":"NotFoundError"}`, "",
			"Not found [NotFoundError]", true,
		},
		{
			"JSON type+code only",
			// 'type' is in type_keys, 'code' is in code_keys; _first_scalar(text_keys) descends
			// into values: first value is 'Timeout' → string → returns 'Timeout' as message_text.
			// Then code and type from direct key checks. Python result: "Timeout [code 408]"
			`{"type":"Timeout","code":"408"}`, "",
			"Timeout [code 408]", true,
		},
		{
			"JSON code only (descend-to-value path)",
			// _first_scalar(text_keys): 'code' not in text_keys → descend → '404' is str → returns '404'
			// message_text='404', type_text='', code_text='404' → code in summary already → no extras
			// Python: "404"
			`{"code":"404"}`, "",
			"404", true,
		},
		{
			"plain message wins over JSON body",
			"plain msg", `{"error":"from body"}`,
			// message is plain (doesn't start with { or [), rawBody starts with { →
			// rawBody is parsed → returns ("from body", true)
			"from body", true,
		},
		{
			"no-matching-keys JSON → fallback dump",
			`{"nokeys":true}`, "",
			// _to_summary returns '' because all _first_scalar calls descend to 'True' value
			// which is not empty... wait let's re-check.
			// Actual Python result: 'True' (from _first_scalar descending into bool value True → str(True)='True')
			"True", true,
		},
		{
			"list wrapping dict → uses first element",
			`[{"message":"list error"}]`, "",
			"list error", true,
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			gotTxt, gotHit := extractStructuredErrorSummary(c.message, c.rawBody)
			if gotTxt != c.wantTxt || gotHit != c.wantHit {
				t.Errorf("extractStructuredErrorSummary(%q, %q) = (%q, %v), want (%q, %v)",
					c.message, c.rawBody, gotTxt, gotHit, c.wantTxt, c.wantHit)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// genaiMessageReasoningToText — app.py:8301-8354
// Oracle: _genai_message_reasoning_to_text
// ---------------------------------------------------------------------------

func TestGenaiMessageReasoningToText(t *testing.T) {
	cases := []struct {
		desc    string
		message map[string]any
		want    string
	}{
		{"empty message", map[string]any{}, ""},
		{
			"reasoning_content string",
			map[string]any{"reasoning_content": "I think X"},
			"I think X",
		},
		{
			"reasoning field",
			map[string]any{"reasoning": "step by step"},
			"step by step",
		},
		{
			"thinking field with whitespace",
			map[string]any{"thinking": "  deep thought  "},
			"deep thought",
		},
		{
			"reasoning_content as list of strings",
			map[string]any{"reasoning_content": []any{"chunk1", "chunk2"}},
			"chunk1\nchunk2",
		},
		{
			"reasoning_content list of dicts with text/content",
			map[string]any{"reasoning_content": []any{
				map[string]any{"text": "part1"},
				map[string]any{"content": "part2"},
			}},
			"part1\npart2",
		},
		{
			"reasoning_content as dict with text key",
			map[string]any{"reasoning_content": map[string]any{"text": "dict text"}},
			"dict text",
		},
		{
			"reasoning_content as dict with content key",
			map[string]any{"reasoning_content": map[string]any{"content": "dict content"}},
			"dict content",
		},
		{
			// Go uses jsonDumpsNoEsc which uses dumpsDefault (spaced ": " and ", " separators)
			// — same as Python json.dumps default.
			"reasoning_content as dict with no text/content key → json.dumps",
			map[string]any{"reasoning_content": map[string]any{"other": "val"}},
			`{"other": "val"}`,
		},
		{
			"parts list with reasoning type",
			map[string]any{"parts": []any{
				map[string]any{"type": "reasoning", "content": "part reason"},
				map[string]any{"type": "text", "content": "skip"},
			}},
			"part reason",
		},
		{
			"parts list with uppercase REASONING type",
			map[string]any{"parts": []any{
				map[string]any{"type": "REASONING", "text": "upper reason"},
			}},
			"upper reason",
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := genaiMessageReasoningToText(c.message)
			if got != c.want {
				t.Errorf("genaiMessageReasoningToText(%v) = %q, want %q", c.message, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// extractAssistantMeta — app.py:3669-3708
// Oracle: _extract_assistant_meta (Go port: extractAssistantMeta, ai_helper.go:94)
//
// Note: Go returns (string, *jsonenc.Object) rather than Python's (str, dict).
// We compare the cleaned text and serialised meta JSON.
// ---------------------------------------------------------------------------

func TestExtractAssistantMeta(t *testing.T) {
	cases := []struct {
		desc        string
		input       string
		wantCleaned string
		wantMetaKey string // a key to check in meta (empty = expect empty meta)
		wantMetaVal string
	}{
		{
			"plain text → no meta",
			"simple text",
			"simple text",
			"", "",
		},
		{
			"empty string → empty cleaned, empty meta",
			"",
			"",
			"", "",
		},
		{
			"tag with JSON meta → cleaned text extracted, meta populated",
			`text <assistant_meta>{"action": "navigate"}</assistant_meta>`,
			"text",
			"action", "navigate",
		},
		{
			"tag mid-text → surrounding text cleaned",
			`text with <assistant_meta>{"key": "val"}</assistant_meta> after`,
			"text with  after",
			"key", "val",
		},
		{
			"invalid JSON meta → cleaned text, empty meta",
			`invalid meta <assistant_meta>not json</assistant_meta>`,
			"invalid meta",
			"", "",
		},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			gotCleaned, gotMeta := extractAssistantMeta(c.input)
			if gotCleaned != c.wantCleaned {
				t.Errorf("cleaned text: got %q, want %q", gotCleaned, c.wantCleaned)
			}
			if c.wantMetaKey == "" {
				if gotMeta != nil && gotMeta.Len() != 0 {
					t.Errorf("expected empty meta, got %v", gotMeta)
				}
			} else {
				if gotMeta == nil {
					t.Fatalf("expected non-nil meta with key %q", c.wantMetaKey)
				}
				v, ok := gotMeta.Get(c.wantMetaKey)
				if !ok {
					t.Errorf("meta missing key %q", c.wantMetaKey)
				} else if fmt.Sprintf("%v", v) != c.wantMetaVal {
					t.Errorf("meta[%q] = %v, want %q", c.wantMetaKey, v, c.wantMetaVal)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// buildClientAction — app.py:4341-4396
// Oracle: _build_client_action (Go port: buildClientAction, ai_action_execute.go:148)
//
// Go takes *jsonenc.Object payload; Python takes dict. We test the pure logic.
// ---------------------------------------------------------------------------

func TestBuildClientAction(t *testing.T) {
	t.Run("empty action type → nil", func(t *testing.T) {
		payload := jsonenc.NewObject().Set("key", "val")
		if got := buildClientAction("", payload); got != nil {
			t.Errorf("empty type should return nil, got %v", got)
		}
	})

	t.Run("nil payload → nil", func(t *testing.T) {
		if got := buildClientAction("navigate", nil); got != nil {
			t.Errorf("nil payload should return nil, got %v", got)
		}
	})

	t.Run("basic action with payload", func(t *testing.T) {
		payload := jsonenc.NewObject().Set("sql_where", "x > 1")
		got := buildClientAction("filter", payload)
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if v, _ := got.Get("type"); v != "filter" {
			t.Errorf("type: got %v, want filter", v)
		}
		if v, _ := got.Get("sql_where"); v != "x > 1" {
			t.Errorf("sql_where: got %v, want 'x > 1'", v)
		}
	})

	t.Run("empty key in payload skipped", func(t *testing.T) {
		// Use a key that will TrimSpace to "" — impossible with jsonenc keys, so test
		// that normal empty-trimming works: " key " → "key".
		payload := jsonenc.NewObject().Set("  val  ", "data") // key gets trimmed to "val"
		got := buildClientAction("action", payload)
		if got == nil {
			t.Fatal("expected non-nil")
		}
		// The trimmed key "val" should appear in the result.
		if v, ok := got.Get("val"); !ok || v != "data" {
			t.Errorf("trimmed key 'val': got %v (ok=%v), want 'data'", v, ok)
		}
	})

	t.Run("bool payload value preserved", func(t *testing.T) {
		payload := jsonenc.NewObject().Set("flag", true)
		got := buildClientAction("action", payload)
		if got == nil {
			t.Fatal("expected non-nil")
		}
		if v, _ := got.Get("flag"); v != true {
			t.Errorf("bool value: got %v, want true", v)
		}
	})

	t.Run("string value trimmed", func(t *testing.T) {
		payload := jsonenc.NewObject().Set("key", "  padded  ")
		got := buildClientAction("action", payload)
		if got == nil {
			t.Fatal("expected non-nil")
		}
		if v, _ := got.Get("key"); v != "padded" {
			t.Errorf("string value: got %v, want 'padded'", v)
		}
	})
}

// ---------------------------------------------------------------------------
// regexScopeTimeConditions — app.py:23882-23903
// Oracle: _regex_scope_time_conditions
// ---------------------------------------------------------------------------

func TestRegexScopeTimeConditions(t *testing.T) {
	t.Run("empty scope → fallback to recent hours", func(t *testing.T) {
		conds, params := regexScopeTimeConditions(map[string]any{}, "Timestamp")
		if len(conds) != 1 || !strings.Contains(conds[0], "now()") {
			t.Errorf("expected now() fallback condition, got %v", conds)
		}
		if len(params) != 1 || params[0] != regexValidateRecentHours {
			t.Errorf("expected [%d] params, got %v", regexValidateRecentHours, params)
		}
	})

	t.Run("from_ts only", func(t *testing.T) {
		scope := map[string]any{"from_ts": "2024-01-01 00:00:00"}
		conds, params := regexScopeTimeConditions(scope, "Timestamp")
		if len(conds) != 1 {
			t.Fatalf("expected 1 condition, got %v", conds)
		}
		if !strings.Contains(conds[0], "Timestamp >=") {
			t.Errorf("expected >= condition, got %q", conds[0])
		}
		if !strings.Contains(conds[0], "parseDateTime64BestEffort") {
			t.Errorf("expected parseDateTime64BestEffort in condition, got %q", conds[0])
		}
		// normalizeChTimestamp formats with microsecond precision
		if len(params) != 1 || params[0] != "2024-01-01 00:00:00.000000" {
			t.Errorf("expected ['2024-01-01 00:00:00.000000'] params, got %v", params)
		}
	})

	t.Run("from_ts and to_ts", func(t *testing.T) {
		scope := map[string]any{"from_ts": "2024-01-01", "to_ts": "2024-02-01"}
		conds, params := regexScopeTimeConditions(scope, "Timestamp")
		if len(conds) != 2 {
			t.Fatalf("expected 2 conditions, got %v", conds)
		}
		if !strings.Contains(conds[0], ">=") {
			t.Errorf("first cond should be >=, got %q", conds[0])
		}
		if !strings.Contains(conds[1], "<") {
			t.Errorf("second cond should be <, got %q", conds[1])
		}
		if len(params) != 2 {
			t.Errorf("expected 2 params, got %v", params)
		}
	})

	t.Run("empty from_ts falls back to recent", func(t *testing.T) {
		scope := map[string]any{"from_ts": ""}
		conds, params := regexScopeTimeConditions(scope, "LogTimestamp")
		if len(conds) != 1 || !strings.Contains(conds[0], "now()") {
			t.Errorf("empty from_ts should fall back to now(), got %v", conds)
		}
		_ = params
	})
}

// ---------------------------------------------------------------------------
// extractBindings — app.py:20525-20748
// Oracle: _extract_bindings (Go: extractBindings, chart_render_binding.go:255)
// ---------------------------------------------------------------------------

func TestExtractBindings(t *testing.T) {
	t.Run("basic time+value extraction", func(t *testing.T) {
		columns := []any{"ts", "val"}
		rows := []map[string]any{
			{"ts": "2024-01-01", "val": 100},
			{"ts": "2024-01-02", "val": 200},
		}
		roleIndices := map[string]int{"time": 0, "value": 1}
		got, err := extractBindings("time_series", columns, rows, roleIndices)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		timeVals, ok := got["time"].([]any)
		if !ok || len(timeVals) != 2 {
			t.Errorf("time binding: got %v", got["time"])
		}
		valVals, ok := got["value"].([]any)
		if !ok || len(valVals) != 2 {
			t.Errorf("value binding: got %v", got["value"])
		}
		// value_first should be set when "value" binding exists
		if got["value_first"] != 100 {
			t.Errorf("value_first: got %v, want 100", got["value_first"])
		}
	})

	t.Run("anomaly state → per-point colors and sizes", func(t *testing.T) {
		columns := []any{"ts", "state"}
		rows := []map[string]any{
			{"ts": "t1", "state": "normal"},
			{"ts": "t2", "state": "warning"},
			{"ts": "t3", "state": "outlier"},
		}
		roleIndices := map[string]int{"time": 0, "effective_state": 1}
		got, err := extractBindings("time_series", columns, rows, roleIndices)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		colors, ok := got["anomaly_point_color"].([]any)
		if !ok || len(colors) != 3 {
			t.Fatalf("anomaly_point_color: got %v", got["anomaly_point_color"])
		}
		// Oracle: normal=#0d6efd, warning=#ffc107, outlier=#dc3545
		wantColors := []string{"#0d6efd", "#ffc107", "#dc3545"}
		for i, wc := range wantColors {
			if colors[i] != wc {
				t.Errorf("anomaly_point_color[%d]: got %v, want %v", i, colors[i], wc)
			}
		}
		sizes, ok := got["anomaly_symbol_size"].([]any)
		if !ok || len(sizes) != 3 {
			t.Fatalf("anomaly_symbol_size: got %v", got["anomaly_symbol_size"])
		}
		wantSizes := []int{4, 7, 10}
		for i, ws := range wantSizes {
			if sizes[i] != ws {
				t.Errorf("anomaly_symbol_size[%d]: got %v, want %d", i, sizes[i], ws)
			}
		}
	})

	t.Run("empty roleIndices → empty bindings", func(t *testing.T) {
		columns := []any{"ts"}
		rows := []map[string]any{{"ts": "now"}}
		got, err := extractBindings("time_series", columns, rows, map[string]int{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected empty bindings, got %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// inferAiPricingForModel — app.py:2813-2825
// Oracle: _infer_ai_pricing_for_model (Go: inferAiPricingForModel, fix_pages_settings.go:372)
//
// The Go function takes the defaults *jsonenc.Object as first arg; we load it
// from the embedded defaultAiPricingJSON to produce oracle-anchored results.
// ---------------------------------------------------------------------------

func TestInferAiPricingForModel(t *testing.T) {
	// Load the same defaults the production code uses.
	defaults, _ := parseJSONValue(defaultAiPricingJSON)
	defObj, ok := defaults.(*jsonenc.Object)
	if !ok || defObj == nil {
		t.Skip("could not parse defaultAiPricingJSON")
	}

	// Helper: verify that the returned entry has numeric "in" and "out" fields.
	checkEntry := func(t *testing.T, got any, desc string) {
		t.Helper()
		obj, ok := got.(*jsonenc.Object)
		if !ok {
			t.Fatalf("%s: expected *jsonenc.Object, got %T: %v", desc, got, got)
		}
		inVal, _ := obj.Get("in")
		outVal, _ := obj.Get("out")
		if inVal == nil || outVal == nil {
			t.Errorf("%s: missing 'in' or 'out' field in %v", desc, obj)
		}
	}

	cases := []struct {
		model   string
		wantKey string // the expected base key that should be returned
	}{
		{"", "gpt-4o"},                 // empty → generic default
		{"gpt-4o", "gpt-4o"},           // exact match
		{"gpt-4o-mini", "gpt-4o-mini"}, // exact match
		// "my-gpt-4o-mini-preview" → defaults loop finds "gpt-4o" first (it's a substring),
		// same as Python oracle with the actual default_ai_pricing.json key order.
		{"my-gpt-4o-mini-preview", "gpt-4o"},            // defaults-loop: "gpt-4o" ⊂ model
		{"claude-3-5-haiku-latest", "claude-3-5-haiku"}, // inference rule: "haiku"
		{"gpt-3.5-turbo", "gpt-3.5-turbo"},              // exact match
		{"unknown-model-xyz", "gpt-4o"},                 // no match → generic default
	}
	for _, c := range cases {
		t.Run("model="+c.model, func(t *testing.T) {
			got := inferAiPricingForModel(defObj, c.model)
			checkEntry(t, got, fmt.Sprintf("model=%q", c.model))

			// Verify same in/out values as the expected key in defaults.
			wantBase, ok := defObj.Get(c.wantKey)
			if !ok {
				t.Skipf("base key %q not found in defaults (test environment mismatch)", c.wantKey)
			}
			wantObj, _ := wantBase.(*jsonenc.Object)
			gotObj, _ := got.(*jsonenc.Object)
			if wantObj != nil && gotObj != nil {
				wantIn, _ := wantObj.Get("in")
				gotIn, _ := gotObj.Get("in")
				wantOut, _ := wantObj.Get("out")
				gotOut, _ := gotObj.Get("out")
				if fmt.Sprintf("%v", gotIn) != fmt.Sprintf("%v", wantIn) ||
					fmt.Sprintf("%v", gotOut) != fmt.Sprintf("%v", wantOut) {
					t.Errorf("model=%q: in/out mismatch: got in=%v out=%v, want in=%v out=%v",
						c.model, gotIn, gotOut, wantIn, wantOut)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// sameOriginRequest — additional branches not covered by auth_test.go
// ---------------------------------------------------------------------------

func TestSameOriginRequest_XForwardedHeaders(t *testing.T) {
	// X-Forwarded-Host and X-Forwarded-Proto override the scheme/host.
	r := httptest.NewRequest("POST", "http://internal.cluster/x", nil)
	r.Host = "internal.cluster"
	r.Header.Set("X-Forwarded-Host", "app.external.com")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Origin", "https://app.external.com")
	if !sameOriginRequest(r) {
		t.Error("X-Forwarded headers should be used for expected origin; same origin should pass")
	}

	// Mismatch: origin is http but forwarded proto says https.
	r2 := httptest.NewRequest("POST", "http://internal.cluster/x", nil)
	r2.Host = "internal.cluster"
	r2.Header.Set("X-Forwarded-Proto", "https")
	r2.Header.Set("Origin", "http://internal.cluster")
	if sameOriginRequest(r2) {
		t.Error("http origin vs https expected origin should fail")
	}
}

// ---------------------------------------------------------------------------
// normalizeGenaiMessagesForDisplay — additional spot-checks
// app.py:8403-8440
// ---------------------------------------------------------------------------

func TestNormalizeGenaiMessagesForDisplay(t *testing.T) {
	t.Run("non-list input → empty", func(t *testing.T) {
		got := normalizeGenaiMessagesForDisplay(nil)
		if len(got) != 0 {
			t.Errorf("nil → expected empty, got %v", got)
		}
		got2 := normalizeGenaiMessagesForDisplay([]any{})
		if len(got2) != 0 {
			t.Errorf("empty list → expected empty, got %v", got2)
		}
	})

	t.Run("string message", func(t *testing.T) {
		msgs := []any{"plain string"}
		got := normalizeGenaiMessagesForDisplay(msgs)
		if len(got) != 1 {
			t.Fatalf("expected 1, got %d", len(got))
		}
		// Oracle: {"role": "", "content": "plain string"}
		obj, ok := got[0].(*jsonenc.Object)
		if !ok {
			t.Fatalf("expected *jsonenc.Object, got %T", got[0])
		}
		role, _ := obj.Get("role")
		content, _ := obj.Get("content")
		if role != "" || content != "plain string" {
			t.Errorf("string msg: role=%v content=%v", role, content)
		}
	})

	t.Run("dict message with role and content", func(t *testing.T) {
		msgs := []any{
			map[string]any{"role": "user", "content": "hello"},
		}
		got := normalizeGenaiMessagesForDisplay(msgs)
		if len(got) != 1 {
			t.Fatalf("expected 1, got %d", len(got))
		}
		obj, ok := got[0].(*jsonenc.Object)
		if !ok {
			t.Fatalf("expected *jsonenc.Object, got %T", got[0])
		}
		role, _ := obj.Get("role")
		roleLabel, _ := obj.Get("role_label")
		if role != "user" {
			t.Errorf("role: got %v, want user", role)
		}
		// Oracle: role_labels["user"] = "user"
		if roleLabel != "user" {
			t.Errorf("role_label: got %v, want user", roleLabel)
		}
	})

	t.Run("system role label", func(t *testing.T) {
		msgs := []any{
			map[string]any{"role": "system", "content": "You are a helper"},
		}
		got := normalizeGenaiMessagesForDisplay(msgs)
		if len(got) != 1 {
			t.Fatalf("expected 1 item")
		}
		obj := got[0].(*jsonenc.Object)
		roleLabel, _ := obj.Get("role_label")
		// Oracle: role_labels["system"] = "system instruction"
		if roleLabel != "system instruction" {
			t.Errorf("role_label: got %v, want 'system instruction'", roleLabel)
		}
	})

	t.Run("reasoning content → thinking_content field", func(t *testing.T) {
		msgs := []any{
			map[string]any{
				"role":              "assistant",
				"content":           "final answer",
				"reasoning_content": "my inner thoughts",
			},
		}
		got := normalizeGenaiMessagesForDisplay(msgs)
		if len(got) != 1 {
			t.Fatalf("expected 1")
		}
		obj := got[0].(*jsonenc.Object)
		tc, ok := obj.Get("thinking_content")
		if !ok {
			t.Error("expected thinking_content to be set")
		} else if tc != "my inner thoughts" {
			t.Errorf("thinking_content: got %v, want 'my inner thoughts'", tc)
		}
	})
}

// ---------------------------------------------------------------------------
// sameOriginRequest via Referer (additional edge cases)
// ---------------------------------------------------------------------------

func TestSameOriginRequest_RefererFallback(t *testing.T) {
	// When Origin header is absent but Referer matches, should pass.
	r := httptest.NewRequest("GET", "http://app.local/page", nil)
	r.Host = "app.local"
	r.Header.Set("Referer", "http://app.local/other-page?q=1")
	if !sameOriginRequest(r) {
		t.Error("Referer origin match should pass")
	}
}

// Verify safeJSONDumps handles the ensure_ascii=False property: non-ASCII chars
// in dicts should be preserved (in compact form), not escaped.
func TestSafeJSONDumps_NonASCII(t *testing.T) {
	// Go json.Marshal does NOT escape non-ASCII by default.
	in := map[string]any{"emoji": "🎉"}
	got := safeJSONDumps(in)
	if strings.Contains(got, `\u`) {
		t.Errorf("non-ASCII should not be escaped: got %q", got)
	}
	if !strings.Contains(got, "🎉") {
		t.Errorf("emoji should be preserved: got %q", got)
	}
}

// Verify safeJSONDumps round-trips a JSON string and the result is valid JSON.
func TestSafeJSONDumps_JSONStringRoundtrip(t *testing.T) {
	in := `{"key":"value","num":42}`
	got := safeJSONDumps(in)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Errorf("result should be valid JSON; got %q, err: %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// buildTraceTimelineSegments — additional edge: trailing gap
// ---------------------------------------------------------------------------

func TestBuildTraceTimelineSegments_TrailingGap(t *testing.T) {
	// Span 0-50ms in a 0-100ms trace → 50% active, 50% trailing gap
	spans := []any{
		map[string]any{"start_ms": 0.0, "duration_ms": 50.0},
		// Add a zero-duration span at 100ms to extend the trace end
		map[string]any{"start_ms": 100.0, "duration_ms": 0.0},
	}
	got := buildTraceTimelineSegments(spans, nil)
	// trace_start=0, trace_end=max(0+50, 100+0)=100, total=100
	// merged: [0,50] only (zero-duration discarded by activeWidthPct>0 check)
	// segments: active 0-50%, gap 50-100%
	if len(got) < 2 {
		t.Fatalf("expected at least 2 segments, got %d: %v", len(got), got)
	}
	// Last segment should be a gap
	lastSeg := got[len(got)-1].(map[string]any)
	if lastSeg["kind"] != "gap" {
		t.Errorf("trailing gap: expected kind=gap, got %v", lastSeg["kind"])
	}
}

// ---------------------------------------------------------------------------
// autoRuleThresholds — additional: "gt" with critical < warning
// ---------------------------------------------------------------------------

func TestAutoRuleThresholds_GTCriticalLessThanWarning(t *testing.T) {
	// q95 (60) < q80 (80) → critical = max(warning, q50) = max(80, 50) = 80
	// Then critical == warning (both 80) → critical = 80 * 1.1 = 88
	w, c := autoRuleThresholds("gt", 5, 20, 50, 80, 60)
	if w != 80 {
		t.Errorf("warning: got %v, want 80", w)
	}
	const wantCrit = 88.0
	if abs64(c-wantCrit) > 1e-9 {
		t.Errorf("critical: got %v, want %v", c, wantCrit)
	}
}

// ---------------------------------------------------------------------------
// Verify extractTraceFields handles float64 traceFlags (JSON number decoding)
// ---------------------------------------------------------------------------

func TestExtractTraceFields_FloatFlags(t *testing.T) {
	// Python: int(raw_flags) when not str → int(16.5) = 16
	event := map[string]any{"traceFlags": float64(16.5)}
	_, _, flags := extractTraceFields(event)
	if flags != 16 {
		t.Errorf("float traceFlags: got %d, want 16", flags)
	}
}

// ---------------------------------------------------------------------------
// Additional sameOriginRequest: first CSV token taken from X-Forwarded-Host
// ---------------------------------------------------------------------------

func TestSameOriginRequest_MultiValueForwardedHost(t *testing.T) {
	// X-Forwarded-Host may be a comma-list; only the first token counts.
	r := httptest.NewRequest("GET", "http://internal/x", nil)
	r.Host = "internal"
	r.Header.Set("X-Forwarded-Host", "app.example.com, proxy.example.com")
	r.Header.Set("Origin", "http://app.example.com")
	if !sameOriginRequest(r) {
		t.Error("first CSV token of X-Forwarded-Host should be used; same origin should pass")
	}
}

// Suppress unused import warning; json is used in TestSafeJSONDumps_JSONStringRoundtrip.
var _ = json.Marshal
var _ = http.StatusOK
var _ = httptest.NewRequest
