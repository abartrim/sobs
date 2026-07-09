package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store/storetest"
)

// persistGithubWorkItem records the agent's GitHub-issue decision as a sobs_github_work_items
// row; every derived field (issue number from the URL, github_repo from the canonical/fired URL,
// dedup default, occurrence floor, related-urls JSON) is corpus-unreachable because the corpus's
// analyze-only rule never creates a GitHub issue. Oracle: app.py _persist_github_work_item.
func TestPersistGithubWorkItem(t *testing.T) {
	fake := &storetest.FakeDB{}
	rule := &agentRule{id: "rule-1", name: "High CPU"}
	tctx := jsonenc.NewObject().Set("service", "svc-a")

	(&server{db: fake}).persistGithubWorkItem(
		"run-1", rule, tctx,
		"https://github.com/acme/widgets/issues/42", // githubIssueURL
		"root cause analysis", "suggested fix",
		"create_issue", "CPU spike", "open",
		"dedup-key-1", "", // dedupDecision blank -> defaults to "new_issue"
		0.75, "", 0, nil, // canonicalIssueURL blank, canonicalIssueNumber 0, relatedIssueURLs nil
		0, 0, "", "", // occurrenceCount 0 -> floors to 1; no copilot assignment
		false, 0, "", // no PR
	)
	if len(fake.Inserts) != 1 || fake.Inserts[0].Table != "sobs_github_work_items" {
		t.Fatalf("unexpected inserts: %v", fake.Inserts)
	}
	row := fake.Inserts[0].Rows[0]
	if row["IssueNumber"] != 42 {
		t.Fatalf("issue number should parse from the URL: %v", row["IssueNumber"])
	}
	if row["GithubRepo"] != "acme/widgets" {
		t.Fatalf("github repo should derive from the issue URL: %v", row["GithubRepo"])
	}
	if row["DedupDecision"] != "new_issue" {
		t.Fatalf("blank dedup decision should default: %v", row["DedupDecision"])
	}
	if row["OccurrenceCount"] != 1 {
		t.Fatalf("occurrence count should floor to 1: %v", row["OccurrenceCount"])
	}
	if row["CanonicalIssueNumber"] != 42 {
		t.Fatalf("blank canonical number should fall back to the parsed issue number: %v", row["CanonicalIssueNumber"])
	}
	if row["CanonicalIssueUrl"] != "https://github.com/acme/widgets/issues/42" {
		t.Fatalf("blank canonical url should fall back to the resolved issue url: %v", row["CanonicalIssueUrl"])
	}
	if row["RelatedIssueUrls"] != "[]" {
		t.Fatalf("nil related urls should encode as '[]': %v", row["RelatedIssueUrls"])
	}
	if row["ServiceName"] != "svc-a" {
		t.Fatalf("trigger fields should be threaded through: %v", row["ServiceName"])
	}

	// A canonical issue url (from a prior duplicate-of decision) wins the repo derivation and
	// pre-set canonical fields pass through unmodified.
	fake2 := &storetest.FakeDB{}
	(&server{db: fake2}).persistGithubWorkItem(
		"run-2", rule, jsonenc.NewObject(),
		"", "a", "s", "duplicate_of", "t", "open",
		"dedup-2", "duplicate_of", 0.9,
		"https://github.com/other/repo/issues/7", 7, []string{"https://x/1"},
		3, 100, "requested", "auto-assigned",
		true, 55, "https://github.com/other/repo/pull/55",
	)
	row2 := fake2.Inserts[0].Rows[0]
	if row2["GithubRepo"] != "other/repo" {
		t.Fatalf("canonical url should drive repo derivation when the issue url is blank: %v", row2["GithubRepo"])
	}
	if row2["IssueUrl"] != "https://github.com/other/repo/issues/7" {
		t.Fatalf("blank github issue url should fall back to canonical: %v", row2["IssueUrl"])
	}
	if row2["OccurrenceCount"] != 3 || row2["PrLinked"] != 1.0 || row2["PrNumber"] != 55 {
		t.Fatalf("unexpected row2: %v", row2)
	}
}

func TestEmitAgentIssueDecisionSummary_NoOp(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	emitAgentIssueDecisionSummary("run-1", &agentRule{id: "r1"}, jsonenc.NewObject(),
		map[string]any{}, "", false, false, "")
	if buf.Len() != 0 {
		t.Fatalf("wantsIssue=false should log nothing, got %q", buf.String())
	}
}

func TestEmitAgentIssueDecisionSummary_Logged(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	tctx := jsonenc.NewObject().Set("trigger_type", "manual").Set("trigger_ref_id", "rule-1")
	outcome := map[string]any{
		"dedup_decision": "new_issue", "dedup_confidence": 0.5,
		"copilot_assignment_status": "requested", "copilot_assignment_reason": "auto",
		"created_new_issue": true, "occurrence_count": 2,
	}
	emitAgentIssueDecisionSummary("run-1", &agentRule{id: "r1", name: "High CPU"}, tctx,
		outcome, "", true, true, "acme/widgets")

	out := buf.String()
	for _, want := range []string{"agent_issue_decision_summary", `"run_id": "run-1"`,
		`"github_repo": "acme/widgets"`, `"copilot_requested": true`, `"occurrence_count": 2`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q; got: %s", want, out)
		}
	}
}
