package main

import "testing"

// cov95_b16_writequeue_test.go — batch 16 targeted coverage for cmd/sobs/writequeue.go's envInt:
// the unset-falls-back-to-default branch and the set-to-a-valid-int branch. The malformed-value
// branch calls log.Fatalf (process exit), which is not practically unit-testable without a
// subprocess harness this package doesn't otherwise use — skipped, same reasoning as
// parsePositiveIntEnv's fatal path (metrics_retention.go).

func TestEnvIntUnsetFallsBackToDefault(t *testing.T) {
	if got := envInt("SOBS_COV95_B16_ENVINT_UNSET_VAR", 123); got != 123 {
		t.Errorf("got %d, want default 123", got)
	}
}

func TestEnvIntUsesSetValue(t *testing.T) {
	t.Setenv("SOBS_COV95_B16_ENVINT_SET_VAR", "456")
	if got := envInt("SOBS_COV95_B16_ENVINT_SET_VAR", 1); got != 456 {
		t.Errorf("got %d, want 456", got)
	}
}

func TestEnvIntNegativeValueAllowed(t *testing.T) {
	// Unlike parsePositiveIntEnv, envInt has no positivity requirement — only malformed strings
	// are fatal. A negative value should parse through cleanly.
	t.Setenv("SOBS_COV95_B16_ENVINT_NEGATIVE_VAR", "-5")
	if got := envInt("SOBS_COV95_B16_ENVINT_NEGATIVE_VAR", 1); got != -5 {
		t.Errorf("got %d, want -5", got)
	}
}
