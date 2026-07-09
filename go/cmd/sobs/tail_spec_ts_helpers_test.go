package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// SSE-tail / spec-mode / timestamp-parse / cve-snapshot / query-stats pure helpers.

func TestSSEEventField(t *testing.T) {
	ev := jsonenc.NewObject().Set("source", "errors").Set("n", 5)
	if got := sseEventField(ev, "source"); got != "errors" {
		t.Errorf("string: %q", got)
	}
	if got := sseEventField(ev, "n"); got != "" {
		t.Errorf("non-string -> empty, got %q", got)
	}
	if got := sseEventField(ev, "missing"); got != "" {
		t.Errorf("missing -> empty, got %q", got)
	}
}

func TestSSEEventMatches(t *testing.T) {
	if !sseEventMatches("", "all", "") {
		t.Error(`source="all" + no filter should match anything`)
	}
	if sseEventMatches(`{"source":"logs"}`, "errors", "") {
		t.Error("source mismatch should NOT match")
	}
	if !sseEventMatches(`{"source":"errors"}`, "errors", "") {
		t.Error("source match should match")
	}
}

func TestSpecSQLMode(t *testing.T) {
	m := map[string]any{"spec": map[string]any{"sql": map[string]any{"mode": " raw "}}}
	if got := specSQLMode(m); got != "raw" {
		t.Errorf("nested: got %q, want raw", got)
	}
	if got := specSQLMode(map[string]any{}); got != "" {
		t.Errorf("missing: got %q", got)
	}
}

func TestSpecModeGuard(t *testing.T) {
	// valid mode -> not guarded (returns false, no write)
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"spec":{"sql":{"mode":"raw"}}}`))
	w := httptest.NewRecorder()
	if specModeGuard(w, req, nil) {
		t.Error("raw mode should pass (return false)")
	}
	// missing/invalid mode -> guarded 400
	req2 := httptest.NewRequest("POST", "/x", strings.NewReader(`{}`))
	w2 := httptest.NewRecorder()
	if !specModeGuard(w2, req2, nil) {
		t.Error("missing mode should be guarded (return true)")
	}
	if w2.Code != 400 {
		t.Errorf("want 400, got %d", w2.Code)
	}
}

func TestStringAttrTruthy(t *testing.T) {
	for _, v := range []any{"1", "true", "YES", "y", "On"} {
		if !stringAttrTruthy(v) {
			t.Errorf("stringAttrTruthy(%v) = false, want true", v)
		}
	}
	for _, v := range []any{"0", "false", "no", 5, ""} {
		if stringAttrTruthy(v) {
			t.Errorf("stringAttrTruthy(%v) = true, want false", v)
		}
	}
}

func TestParseISOFloorAndNormalize(t *testing.T) {
	tm, ok := parseISOFloor("2026-03-29 12:00:00")
	if !ok {
		t.Fatal("should parse")
	}
	if got := normalizeChTimestampTime(tm); got != "2026-03-29 12:00:00.000000" {
		t.Errorf("normalize: got %q", got)
	}
	if _, ok := parseISOFloor("not a timestamp"); ok {
		t.Error("garbage should not parse")
	}
	if tm2, ok := parseCHTimestamp("2026-03-29T12:00:00Z"); !ok || tm2.UTC().Hour() != 12 {
		t.Errorf("parseCHTimestamp Z form: ok=%v hour=%d", ok, tm2.UTC().Hour())
	}
}

func TestNormalizeChTimestampTime(t *testing.T) {
	tm := time.Date(2026, 3, 29, 12, 0, 0, 0, time.UTC)
	if got := normalizeChTimestampTime(tm); got != "2026-03-29 12:00:00.000000" {
		t.Errorf("got %q", got)
	}
}

func TestMustParseJSON(t *testing.T) {
	if v, ok := mustParseJSON([]byte(`[1,2,3]`)).([]any); !ok || len(v) != 3 {
		t.Error("valid array")
	}
	if _, ok := mustParseJSON([]byte(`not json`)).(*jsonenc.Object); !ok {
		t.Error("invalid -> empty object")
	}
}

func TestToJSONObject(t *testing.T) {
	o := toJSONObject(map[string]any{"a": 1, "b": map[string]any{"c": 2}})
	if v, _ := o.Get("a"); v != 1 {
		t.Errorf("a = %v", v)
	}
	sub, ok := o.Get("b")
	if !ok {
		t.Fatal("missing b")
	}
	if so, ok := sub.(*jsonenc.Object); !ok {
		t.Errorf("b should be nested object, got %T", sub)
	} else if cv, _ := so.Get("c"); cv != 2 {
		t.Errorf("b.c = %v", cv)
	}
}

func TestGetObjField(t *testing.T) {
	o := jsonenc.NewObject().Set("k", "v")
	if got := getObjField(o, "k"); got != "v" {
		t.Errorf("present: %v", got)
	}
	if got := getObjField(o, "missing"); got != nil {
		t.Errorf("missing -> nil, got %v", got)
	}
}

func TestGithubActionsSnapshotName(t *testing.T) {
	dep, plat, arch, ok := githubActionsSnapshotName("pip-freeze-linux-amd64.txt")
	if !ok || dep != "pip-freeze-linux-amd64" || plat != "linux" || arch != "amd64" {
		t.Errorf("got (%q,%q,%q,%v)", dep, plat, arch, ok)
	}
	// path basename is used
	if _, _, _, ok := githubActionsSnapshotName("artifacts/pip-freeze-darwin-arm64.txt"); !ok {
		t.Error("should match on basename")
	}
	if _, _, _, ok := githubActionsSnapshotName("requirements.txt"); ok {
		t.Error("non-matching name should fail")
	}
}

func TestZeroQueryLLMStats(t *testing.T) {
	o := zeroQueryLLMStats()
	for _, stage := range []string{"totals", "named_query_generation", "chart_generation"} {
		v, ok := o.Get(stage)
		if !ok {
			t.Errorf("missing stage %q", stage)
			continue
		}
		so := v.(*jsonenc.Object)
		if pt, _ := so.Get("prompt_tokens"); pt != 0 {
			t.Errorf("%s.prompt_tokens = %v, want 0", stage, pt)
		}
	}
}
