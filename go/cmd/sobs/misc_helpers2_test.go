package main

import (
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Remaining pure helpers: finish-reason extraction, error-id hashing, attr truncation, severity
// pick, chart sample records, IN-clause repair (passthrough), tool-action summary, preview default.

func TestExtractFinishReason(t *testing.T) {
	ev := jsonenc.NewObject().Set("choices", []any{jsonenc.NewObject().Set("finish_reason", "stop")})
	if got := extractFinishReason(ev); got != "stop" {
		t.Errorf("got %q, want stop", got)
	}
	if got := extractFinishReason(jsonenc.NewObject()); got != "" {
		t.Errorf("no choices: got %q", got)
	}
}

func TestErrorIDFor(t *testing.T) {
	ts, svc, et, msg, tid, sid := "2026-01-01 00:00:00", "web", "ValueError", "boom", "trace1", "span1"
	got := errorIDFor(ts, svc, et, msg, tid, sid)
	// Independent recompute: md5 hex of the pipe-joined fields (verifies join order + field set).
	raw := strings.Join([]string{ts, svc, et, msg, tid, sid}, "|")
	sum := md5.Sum([]byte(raw))
	if want := hex.EncodeToString(sum[:]); got != want {
		t.Errorf("errorIDFor = %q, want %q", got, want)
	}
	if len(got) != 32 {
		t.Errorf("md5 hex should be 32 chars, got %d", len(got))
	}
}

func TestTruncateAttrObject(t *testing.T) {
	o := jsonenc.NewObject().Set("a", "short").Set("b", strings.Repeat("x", 10)).Set("n", 5)
	out := truncateAttrObject(o, 3)
	if v, _ := out.Get("a"); v != "sho…" {
		t.Errorf("a = %v, want sho…", v)
	}
	if v, _ := out.Get("b"); v != "xxx…" {
		t.Errorf("b = %v, want xxx…", v)
	}
	if v, _ := out.Get("n"); v != 5 { // non-string left untouched
		t.Errorf("n = %v, want 5", v)
	}
}

func TestPickHighestSeverityEvent(t *testing.T) {
	warn := jsonenc.NewObject().Set("state", "warning")
	crit := jsonenc.NewObject().Set("state", "critical")
	if got := pickHighestSeverityEvent([]*jsonenc.Object{warn, crit}); got != crit {
		t.Error("critical should win")
	}
	if got := pickHighestSeverityEvent([]*jsonenc.Object{warn}); got != warn {
		t.Error("single non-critical should be returned")
	}
	if got := pickHighestSeverityEvent(nil); got != nil {
		t.Error("empty -> nil")
	}
}

func TestChartSampleRecords(t *testing.T) {
	cols := []any{"a", "b"}
	rows := []any{[]any{1, 2}, []any{3, 4}}
	out := chartSampleRecords(cols, rows, 1) // limited to 1
	if len(out) != 1 {
		t.Fatalf("len %d, want 1 (limited)", len(out))
	}
	rec, ok := out[0].(*jsonenc.Object)
	if !ok {
		t.Fatalf("record not object: %T", out[0])
	}
	if v, _ := rec.Get("a"); v != 1 {
		t.Errorf("a = %v, want 1", v)
	}
	if v, _ := rec.Get("b"); v != 2 {
		t.Errorf("b = %v, want 2", v)
	}
}

func TestRepairTruncatedInClauseLiteralsPassthrough(t *testing.T) {
	// No trailing truncated IN(...) -> returned unchanged.
	in := "SELECT * FROM t WHERE x = 1"
	if got := repairTruncatedInClauseLiterals(in); got != in {
		t.Errorf("passthrough changed input: %q", got)
	}
}

func TestSummarizeAiToolAction(t *testing.T) {
	if got := summarizeAiToolAction(""); got != "" {
		t.Errorf("empty: %q", got)
	}
	if got := summarizeAiToolAction("just plain text"); got != "just plain text" {
		t.Errorf("non-JSON passthrough: %q", got)
	}
}

func TestAutoRulePreviewSummary(t *testing.T) {
	o := autoRulePreviewSummary()
	checks := map[string]any{
		"action": "preview", "hours": 24, "min_points": 30, "mode": "threshold",
		"seasonal_strategy": "hour_of_day", "create_cap": 200, "capped": false, "created": 0,
	}
	for k, want := range checks {
		if v, _ := o.Get(k); v != want {
			t.Errorf("%s = %v, want %v", k, v, want)
		}
	}
}
