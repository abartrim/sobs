package main

import (
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// agentMockContent is the canned analyze/dedupe reply the parity AI mock returns for the agent
// profiles (a root-cause text with no JSON), so the LLM dedupe classifier must fall back.
const agentMockContent = "NOISE_OR_IMPACT: IMPACT\n" +
	"ROOT CAUSE: Connection pool exhausted under sustained load, causing request queueing.\n" +
	"SUGGESTED FIX: Raise the pool ceiling and add bounded retry with exponential backoff."

func TestNormalizeIssueMatchText(t *testing.T) {
	cases := map[string]string{
		"acme/reuse-demo": "acme reuse demo",
		"TimeoutError":    "timeouterror",
		"  Errors!! ":     "errors",
		"":                "",
		"a/b-c_d.e":       "a b c d e",
	}
	for in, want := range cases {
		if got := normalizeIssueMatchText(in); got != want {
			t.Errorf("normalizeIssueMatchText(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIssueReuseDedupKey pins the exact dedup key the issuereuse parity profile depends on: the
// key recomputed from the raise-issue request MUST equal the seeded work item's DedupKey, or the
// local fallback would not classify the incident "same" and the reuse branch would never run.
func TestIssueReuseDedupKey(t *testing.T) {
	tctx := buildUserIssueTriggerContext("errors", map[string]any{
		"err_type": "TimeoutError",
		"error_id": "err-001",
	})
	tf := extractAgentTriggerFields(tctx)
	if tf.serviceName != "" || tf.signalSource != "errors" || tf.signalName != "TimeoutError" || tf.anomalyState != "critical" {
		t.Fatalf("unexpected trigger fields: %+v", tf)
	}
	got := buildGithubWorkItemDedupKey("acme/reuse-demo", tf)
	want := "acme reuse demo||errors|timeouterror|critical"
	if got != want {
		t.Fatalf("dedup key = %q, want %q (must match seed_issues_reuse W1.DedupKey)", got, want)
	}
}

func TestBuildUserIssueTriggerContextSources(t *testing.T) {
	// traces: ERROR status -> critical; span_name drives the signal name.
	tctx := buildUserIssueTriggerContext("traces", map[string]any{
		"span_name": "GET /checkout", "status": "STATUS_CODE_ERROR", "trace_id": "t-1",
	})
	tf := extractAgentTriggerFields(tctx)
	if tf.signalSource != "traces" || tf.signalName != "GET /checkout" || tf.anomalyState != "critical" {
		t.Errorf("traces fields wrong: %+v", tf)
	}
	if objGetStr(tctx, "trigger_ref_id") != "t-1" {
		t.Errorf("traces trigger_ref_id = %q", objGetStr(tctx, "trigger_ref_id"))
	}
	// incident with no error -> warning; default signal name.
	tctx = buildUserIssueTriggerContext("incident", map[string]any{})
	tf = extractAgentTriggerFields(tctx)
	if tf.signalSource != "incident" || tf.signalName != "incident_packet" || tf.anomalyState != "warning" {
		t.Errorf("incident fields wrong: %+v", tf)
	}
	// unknown source -> coerced to errors.
	tctx = buildUserIssueTriggerContext("weird", map[string]any{})
	if objGetStr(tctx, "rule_name") != "User Raised Issue (errors)" {
		t.Errorf("unknown source not coerced to errors: %q", objGetStr(tctx, "rule_name"))
	}
}

func mkCandidate(id, dedupKey, service, signal string) dedupeCandidate {
	return dedupeCandidate{candidateID: id, issueURL: id, dedupKey: dedupKey, serviceName: service, signalName: signal}
}

func TestFallbackIssueDedupeDecision(t *testing.T) {
	proposed := newJSONObjFrom(map[string]any{
		"dedup_key":    "acme reuse demo||errors|timeouterror|critical",
		"service_name": "checkout", "signal_name": "TimeoutError",
	})
	cands := []dedupeCandidate{
		mkCandidate("u/keymatch", "acme reuse demo||errors|timeouterror|critical", "checkout", "TimeoutError"),
	}
	// exact dedup-key match -> "same"@0.92
	d := fallbackIssueDedupeDecision(proposed, cands)
	if d["classification"] != "same" || d["candidate_id"] != "u/keymatch" || d["confidence"].(float64) != 0.92 {
		t.Fatalf("key-match decision wrong: %+v", d)
	}
	// no key match but same service+signal family -> "related"@0.73
	cands2 := []dedupeCandidate{mkCandidate("u/family", "different|key", "checkout", "TimeoutError")}
	d = fallbackIssueDedupeDecision(proposed, cands2)
	if d["classification"] != "related" || d["candidate_id"] != "u/family" || d["confidence"].(float64) != 0.73 {
		t.Fatalf("family decision wrong: %+v", d)
	}
	// nothing matches -> "unrelated"@0
	cands3 := []dedupeCandidate{mkCandidate("u/none", "x|y", "billing", "OtherError")}
	d = fallbackIssueDedupeDecision(proposed, cands3)
	if d["classification"] != "unrelated" || d["candidate_id"] != "" || d["confidence"].(float64) != 0.0 {
		t.Fatalf("unrelated decision wrong: %+v", d)
	}
	// empty candidate list -> unrelated
	if d := fallbackIssueDedupeDecision(proposed, nil); d["classification"] != "unrelated" {
		t.Fatalf("empty candidates should be unrelated: %+v", d)
	}
}

func TestExtractFirstJSONObject(t *testing.T) {
	// The agent mock content has no JSON object -> empty -> dedupe falls back.
	if o := extractFirstJSONObject(agentMockContent); o.Len() != 0 {
		t.Errorf("agent mock content should yield no JSON object, got %d keys", o.Len())
	}
	// plain JSON object.
	o := extractFirstJSONObject(`{"classification": "same", "candidate_id": "u/1"}`)
	if objGetStr(o, "classification") != "same" || objGetStr(o, "candidate_id") != "u/1" {
		t.Errorf("plain JSON parse wrong: %v", o)
	}
	// fenced JSON.
	o = extractFirstJSONObject("```json\n{\"classification\": \"related\"}\n```")
	if objGetStr(o, "classification") != "related" {
		t.Errorf("fenced JSON parse wrong: %v", o)
	}
	// embedded object inside prose.
	o = extractFirstJSONObject("Here you go: {\"classification\": \"unrelated\"} thanks")
	if objGetStr(o, "classification") != "unrelated" {
		t.Errorf("embedded JSON parse wrong: %v", o)
	}
	// empty.
	if o := extractFirstJSONObject("   "); o.Len() != 0 {
		t.Errorf("blank should yield empty object")
	}
}

func TestParseBoundedIntSetting(t *testing.T) {
	s := map[string]string{"a": "7", "b": "abc", "c": "  3 ", "d": "999"}
	cases := []struct {
		key           string
		def, min, max int
		want          int
	}{
		{"a", 5, 1, 20, 7},       // parsed
		{"missing", 5, 1, 20, 5}, // default
		{"b", 5, 1, 20, 5},       // unparseable -> default
		{"c", 5, 1, 20, 3},       // trimmed
		{"d", 5, 1, 10, 10},      // clamped to max
		{"a", 5, 9, 20, 9},       // clamped to min
	}
	for _, c := range cases {
		if got := parseBoundedIntSetting(s, c.key, c.def, c.min, c.max); got != c.want {
			t.Errorf("parseBoundedIntSetting(%q,def=%d,[%d,%d]) = %d, want %d", c.key, c.def, c.min, c.max, got, c.want)
		}
	}
}

func TestBuildAgentIssueTitle(t *testing.T) {
	rule := &agentRule{name: "My Rule"}
	tf := triggerFields{serviceName: "checkout", signalSource: "errors", signalName: "TimeoutError", anomalyState: "critical"}
	if got := buildAgentIssueTitle(rule, tf); got != "[SOBS Agent] checkout — errors/TimeoutError critical anomaly" {
		t.Errorf("title with signal wrong: %q", got)
	}
	// no service -> rule name; no signal -> state form; empty state -> "detected".
	tf2 := triggerFields{}
	if got := buildAgentIssueTitle(rule, tf2); got != "[SOBS Agent] My Rule — detected state detected" {
		t.Errorf("title fallback wrong: %q", got)
	}
}

func TestNewIssueState(t *testing.T) {
	if newIssueState(map[string]any{"issue_state": "closed"}) != "closed" {
		t.Error("explicit state")
	}
	if newIssueState(map[string]any{"issue_url": "x"}) != "open" {
		t.Error("non-empty created -> open")
	}
	if newIssueState(map[string]any{}) != "" {
		t.Error("empty created -> empty")
	}
}

// newJSONObjFrom builds a jsonenc.Object from a map (test helper; key order is unimportant here).
func newJSONObjFrom(m map[string]any) *jsonenc.Object {
	o := jsonenc.NewObject()
	for k, v := range m {
		o.Set(k, v)
	}
	return o
}
