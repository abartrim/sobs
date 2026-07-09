package main

import "testing"

// cov95_b16_metrics_retention_test.go — batch 16 targeted coverage for
// cmd/sobs/metrics_retention.go's parsePositiveIntEnv: the env-var-set branch (every existing
// call in metrics_retention_test.go leaves the vars unset, only exercising the default-fallback
// path). The log.Fatalf branch on an invalid/non-positive value is not covered here: it calls
// os.Exit via log.Fatalf, which would kill the test binary — genuinely impractical to unit test
// without a subprocess harness this package doesn't otherwise use, and it's a fail-fast startup
// guard (not a request-serving code path).

func TestParsePositiveIntEnvUsesSetValue(t *testing.T) {
	t.Setenv("SOBS_COV95_B16_POSITIVE_INT", "42")
	if got := parsePositiveIntEnv("SOBS_COV95_B16_POSITIVE_INT", "7", "widgets"); got != 42 {
		t.Errorf("got %d, want 42 (the env-provided value)", got)
	}
}

func TestParsePositiveIntEnvFallsBackWhenUnset(t *testing.T) {
	// A variable name never set in this process exercises the LookupEnv-miss -> default branch.
	if got := parsePositiveIntEnv("SOBS_COV95_B16_TRULY_UNSET_VAR", "9", "widgets"); got != 9 {
		t.Errorf("got %d, want the default 9", got)
	}
}
