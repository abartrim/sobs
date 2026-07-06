package main

import (
	"strings"
	"testing"
)

// cov95_b15_fix_pages_obs_test.go — batch 15 coverage for cmd/sobs/fix_pages_obs.go.
//
// anyToInt (16), buildAutoDashboardChartCandidates (86), and getSignalHealthByService (167)
// already have solid existing coverage (small_helpers_test.go, agent_notif_pure_helpers_test.go,
// signal_health_test.go respectively). This file adds the specific branches those miss:
//   - anyToInt's string-coercion and unsupported-type (default 0) branches.
//   - buildAutoDashboardChartCandidates's "missing signal" skip and the attr_fp SQL-scoping
//     branch (existing tests cover "missing source" but not "missing signal", and never set
//     a non-empty attr_fp).

func TestAnyToInt_StringAndDefaultBranches(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want int
	}{
		{"string numeric", "42", 42},
		{"string with surrounding whitespace", "  8  ", 8},
		{"string non-numeric -> 0 (Atoi error swallowed)", "not-a-number", 0},
		{"string empty -> 0", "", 0},
		{"nil -> 0 (default branch)", nil, 0},
		{"bool -> 0 (default branch, unsupported type)", true, 0},
		{"slice -> 0 (default branch)", []any{1, 2}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := anyToInt(c.v); got != c.want {
				t.Errorf("anyToInt(%#v) = %d, want %d", c.v, got, c.want)
			}
		})
	}
}

func TestBuildAutoDashboardChartCandidates_MissingSignalAndAttrFp(t *testing.T) {
	rules := []any{
		// Missing signal (blank) -> skipped, same guard as the already-tested missing-source case
		// but exercising the OTHER half of `if source == "" || signal == ""`.
		map[string]any{"name": "no signal here", "source": "cpu", "signal": "", "service": "svc-a"},
		// A rule with attr_fp set exercises the "AttrFingerprint = " SQL-scoping branch that the
		// existing agent_notif_pure_helpers_test.go rules never set.
		map[string]any{"name": "fingerprinted", "source": "cpu", "signal": "usage", "service": "svc-a", "attr_fp": "fp-123"},
	}
	got := buildAutoDashboardChartCandidates(rules, "svc-a", 6)
	if len(got) != 1 {
		t.Fatalf("want 1 candidate (missing-signal rule skipped), got %d: %v", len(got), got)
	}
	m := got[0].(map[string]any)
	if m["attr_fp"] != "fp-123" {
		t.Errorf("attr_fp = %v, want fp-123", m["attr_fp"])
	}
	q := m["query"].(string)
	if !strings.Contains(q, "AttrFingerprint = ") {
		t.Errorf("query missing AttrFingerprint scoping clause: %s", q)
	}
}
