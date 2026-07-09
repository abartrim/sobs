package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// mapBool / copilotAssignmentParams / extractTriggerMaskOutput / defaultAutoDashboardName /
// buildAutoDashboardChartCandidates are pure agent-flow/dashboard helpers whose non-default
// branches the corpus's single analyze-only rule and empty-fixture dashboard page never reach.
// Oracle: app.py fragments cited in each function's doc comment.

func TestMapBool(t *testing.T) {
	if mapBool(map[string]any{"x": true}, "x") != true {
		t.Fatal("want true")
	}
	if mapBool(map[string]any{"x": "true"}, "x") != false {
		t.Fatal("non-bool value should coerce to false, not parse the string")
	}
	if mapBool(map[string]any{}, "x") != false {
		t.Fatal("absent key should be false")
	}
}

func TestCopilotAssignmentParams(t *testing.T) {
	settings := map[string]string{
		"ai.github_copilot_base_branch":         " main ",
		"ai.github_copilot_custom_instructions": "Follow our style guide.",
	}
	branch, instructions := copilotAssignmentParams(settings, "")
	if branch != "main" {
		t.Fatalf("branch: got %q", branch)
	}
	if instructions != "Follow our style guide." {
		t.Fatalf("no suggestion: instructions should pass through unchanged, got %q", instructions)
	}

	_, withSuggestion := copilotAssignmentParams(settings, "add a nil check")
	want := "Follow our style guide.\n\nUse this suggested fix guidance when relevant:\nadd a nil check"
	if withSuggestion != want {
		t.Fatalf("got %q, want %q", withSuggestion, want)
	}

	_, noCustom := copilotAssignmentParams(map[string]string{}, "add a nil check")
	if !strings.HasPrefix(noCustom, "Use this suggested fix guidance when relevant:\n") {
		t.Fatalf("no custom instructions: got %q", noCustom)
	}
}

func TestExtractTriggerMaskOutput(t *testing.T) {
	if !extractTriggerMaskOutput(jsonenc.NewObject()) {
		t.Fatal("no extra key -> default true")
	}
	if !extractTriggerMaskOutput(jsonenc.NewObject().Set("extra", jsonenc.NewObject())) {
		t.Fatal("extra object without mask_output -> default true")
	}
	if extractTriggerMaskOutput(jsonenc.NewObject().Set("extra", jsonenc.NewObject().Set("mask_output", false))) {
		t.Fatal("extra object mask_output=false -> false")
	}
	if !extractTriggerMaskOutput(jsonenc.NewObject().Set("extra", "")) {
		t.Fatal("blank extra string -> default true")
	}
	if !extractTriggerMaskOutput(jsonenc.NewObject().Set("extra", "not json")) {
		t.Fatal("unparseable extra string -> default true")
	}
	if extractTriggerMaskOutput(jsonenc.NewObject().Set("extra", `{"mask_output": false}`)) {
		t.Fatal("extra JSON string mask_output=false -> false")
	}
	if !extractTriggerMaskOutput(jsonenc.NewObject().Set("extra", `{"other":1}`)) {
		t.Fatal("extra JSON string without mask_output -> default true")
	}
}

func TestDefaultAutoDashboardName(t *testing.T) {
	if got := defaultAutoDashboardName("svc-a"); got != "Auto Metric Rules - svc-a" {
		t.Fatalf("got %q", got)
	}
	if got := defaultAutoDashboardName(""); got != "Auto Metric Rules Dashboard" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildAutoDashboardChartCandidates(t *testing.T) {
	rules := []any{
		map[string]any{"name": "High CPU", "source": "cpu", "signal": "usage", "service": "svc-a"},
		map[string]any{"name": "", "source": "mem", "signal": "usage", "service": "svc-b", "attr_fp": "fp1", "rule_type": "seasonal"},
		map[string]any{"name": "No Source", "source": "", "signal": "usage", "service": "svc-a"}, // skipped: blank source
		map[string]any{"name": "Wrong Service", "source": "disk", "signal": "free", "service": "svc-c"},
		map[string]any{"name": "High CPU", "source": "cpu", "signal": "usage2", "service": "svc-a"}, // dup title
		"not-a-map", // skipped
	}
	got := buildAutoDashboardChartCandidates(rules, "svc-a", 6)
	if len(got) != 2 {
		t.Fatalf("want 2 svc-a candidates, got %d: %v", len(got), got)
	}

	// svc-b has no explicit filter match, but the rule has ruleService="svc-b" != filter "svc-a" -> should be skipped.
	for _, c := range got {
		m := c.(map[string]any)
		if m["service"] == "svc-b" || m["service"] == "svc-c" {
			t.Fatalf("wrong-service rule leaked through: %v", m)
		}
	}

	// Duplicate base title gets a "(2)" suffix; sorted by (service, source, signal, title) so the
	// first "High CPU" (signal usage) sorts before the second (signal usage2).
	titles := make([]string, len(got))
	for i, c := range got {
		titles[i] = c.(map[string]any)["title"].(string)
	}
	if titles[0] != "High CPU" || titles[1] != "High CPU (2)" {
		t.Fatalf("dedup/sort order wrong: %v", titles)
	}

	// The query embeds the anomaly window and service/attr_fp scoping when present.
	first := got[0].(map[string]any)
	if !strings.Contains(first["query"].(string), "INTERVAL 6 HOUR") {
		t.Fatalf("query missing hours window: %v", first["query"])
	}
	if !strings.Contains(first["query"].(string), "ServiceName = ") {
		t.Fatalf("query missing service scoping: %v", first["query"])
	}

	// No serviceFilter -> every rule with a source/signal passes (service scoping is per-row only).
	all := buildAutoDashboardChartCandidates(rules, "", 24)
	if len(all) != 4 {
		t.Fatalf("want 4 candidates with no filter, got %d: %v", len(all), all)
	}
}
