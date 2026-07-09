package main

import (
	"time"

	"testing"

	"github.com/dlclark/regexp2"
)

// cov95_b16_mask_regex_test.go — batch 16 targeted coverage for cmd/sobs/mask_regex.go's match
// and replaceAll: the timeout/engine-error branch, which compileUserRegex's real callers never hit
// under benign patterns (userRegexMatchTimeout is a full second). Constructing the userRegex struct
// directly (same package) lets the test set a short MatchTimeout instead of waiting a full second
// for a genuinely pathological pattern.

func newTimeoutProneUserRegex(t *testing.T, pattern string, timeout time.Duration) *userRegex {
	t.Helper()
	re, err := regexp2.Compile(pattern, regexp2.None)
	if err != nil {
		t.Fatalf("compile %q: %v", pattern, err)
	}
	re.MatchTimeout = timeout
	return &userRegex{re: re}
}

func TestUserRegexMatchTimeoutTreatedAsNoMatch(t *testing.T) {
	// Classic catastrophic-backtracking pattern against a string with no trailing match, forced to
	// time out almost immediately via a 1ns timeout.
	u := newTimeoutProneUserRegex(t, `(a+)+$`, 1*time.Nanosecond)
	long := ""
	for i := 0; i < 40; i++ {
		long += "a"
	}
	long += "!"
	if u.match(long) {
		t.Fatal("a timed-out match must report false (Python re.error -> False at call sites)")
	}
}

func TestUserRegexReplaceAllTimeoutReturnsInputUnchanged(t *testing.T) {
	u := newTimeoutProneUserRegex(t, `(a+)+$`, 1*time.Nanosecond)
	long := ""
	for i := 0; i < 40; i++ {
		long += "a"
	}
	long += "!"
	if got := u.replaceAll(long, "MASKED"); got != long {
		t.Fatalf("a timed-out replaceAll must return the input unchanged, got %q", got)
	}
}

func TestEscapeRegexReplacementLiteralDollar(t *testing.T) {
	if got := escapeRegexReplacement("price: $5"); got != "price: $$5" {
		t.Fatalf("got %q, want literal-$ escaped form", got)
	}
	if got := escapeRegexReplacement("no dollars here"); got != "no dollars here" {
		t.Fatalf("got %q, want unchanged", got)
	}
}
