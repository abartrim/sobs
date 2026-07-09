package main

import (
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Pure agent-flow / dedupe helpers — corpus-unreachable (the agent flow needs a live LLM + GitHub
// upstream). Oracles: _parse_issue_ref_from_url, _extract_trigger_service_name,
// _parse_agent_analysis (app.py:6848), _extract_trigger_additional_context.

func TestParseIssueRefFromURL(t *testing.T) {
	owner, repo, n := parseIssueRefFromURL("https://github.com/acme/widget/issues/42")
	if owner != "acme" || repo != "widget" || n != 42 {
		t.Errorf("got (%q, %q, %d), want (acme, widget, 42)", owner, repo, n)
	}
	for _, bad := range []string{"", "not a url", "https://github.com/o/r/pull/5", "https://gitlab.com/o/r/issues/1"} {
		if o, r, num := parseIssueRefFromURL(bad); o != "" || r != "" || num != 0 {
			t.Errorf("parseIssueRefFromURL(%q) = (%q,%q,%d), want empties", bad, o, r, num)
		}
	}
}

func TestExtractTriggerServiceName(t *testing.T) {
	// direct service field wins
	if got := extractTriggerServiceName(jsonenc.NewObject().Set("service", "checkout")); got != "checkout" {
		t.Errorf("direct: got %q, want checkout", got)
	}
	// fall back to extra JSON (service_name / ServiceName keys)
	if got := extractTriggerServiceName(jsonenc.NewObject().Set("extra", `{"service_name":"web"}`)); got != "web" {
		t.Errorf("extra service_name: got %q, want web", got)
	}
	if got := extractTriggerServiceName(jsonenc.NewObject().Set("extra", `{"ServiceName":"api"}`)); got != "api" {
		t.Errorf("extra ServiceName: got %q, want api", got)
	}
	// nothing -> empty
	if got := extractTriggerServiceName(jsonenc.NewObject()); got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}
}

func TestParseAgentAnalysis(t *testing.T) {
	cases := []struct {
		name, reply, wantAnalysis, wantSuggestion string
	}{
		{"root cause + fix", "ROOT CAUSE: db slow\nSUGGESTED FIX: add index", "db slow", "add index"},
		{"analysis only", "just the analysis", "just the analysis", ""},
		{"noise line stripped", "NOISE_OR_IMPACT: IMPACT\nReal analysis here", "Real analysis here", ""},
		{"noise no newline -> empty", "NOISE_OR_IMPACT: NOISE", "", ""},
		{"combined", "NOISE_OR_IMPACT: IMPACT\nROOT CAUSE: x\nSUGGESTED FIX: y", "x", "y"},
	}
	for _, c := range cases {
		a, s := parseAgentAnalysis(c.reply)
		if a != c.wantAnalysis || s != c.wantSuggestion {
			t.Errorf("%s: parseAgentAnalysis(%q) = (%q, %q), want (%q, %q)",
				c.name, c.reply, a, s, c.wantAnalysis, c.wantSuggestion)
		}
	}
}

func TestExtractTriggerAdditionalContext(t *testing.T) {
	// extra as a nested object
	tctxObj := jsonenc.NewObject().Set("extra", jsonenc.NewObject().Set("additional_context", "ctx-a"))
	if got := extractTriggerAdditionalContext(tctxObj); got != "ctx-a" {
		t.Errorf("object extra: got %q, want ctx-a", got)
	}
	// extra as a JSON string
	tctxStr := jsonenc.NewObject().Set("extra", `{"additional_context":"ctx-b"}`)
	if got := extractTriggerAdditionalContext(tctxStr); got != "ctx-b" {
		t.Errorf("string extra: got %q, want ctx-b", got)
	}
	// missing extra
	if got := extractTriggerAdditionalContext(jsonenc.NewObject()); got != "" {
		t.Errorf("missing: got %q, want empty", got)
	}
}

func TestObjGetBool(t *testing.T) {
	if !objGetBool(jsonenc.NewObject().Set("b", true), "b") {
		t.Error("true bool: want true")
	}
	if objGetBool(jsonenc.NewObject().Set("b", false), "b") {
		t.Error("false bool: want false")
	}
	if objGetBool(jsonenc.NewObject().Set("b", "true"), "b") {
		t.Error("string value: want false (not a bool)")
	}
	if objGetBool(jsonenc.NewObject(), "b") {
		t.Error("missing: want false")
	}
}

func TestFirstNonZeroInt(t *testing.T) {
	if got := firstNonZeroInt(5, 9); got != 5 {
		t.Errorf("got %d, want 5", got)
	}
	if got := firstNonZeroInt(0, 9); got != 9 {
		t.Errorf("got %d, want 9", got)
	}
	if got := firstNonZeroInt(0, 0); got != 0 {
		t.Errorf("got %d, want 0", got)
	}
}

func TestAnyToStringList(t *testing.T) {
	if got := anyToStringList([]string{"x", "y"}); len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Errorf("[]string: got %v", got)
	}
	if got := anyToStringList([]any{"a", "b", "c"}); len(got) != 3 || got[2] != "c" {
		t.Errorf("[]any: got %v", got)
	}
	if got := anyToStringList("not a list"); got != nil {
		t.Errorf("non-list: got %v, want nil", got)
	}
	if got := anyToStringList(nil); got != nil {
		t.Errorf("nil: got %v, want nil", got)
	}
}
