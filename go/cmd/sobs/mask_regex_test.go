package main

import (
	"regexp"
	"testing"
)

// TestUserRegexReplaceAll pins the regexp2 replace semantics the masking engine relies on:
// replace-all (Replace(..., -1, -1)) and literal replacement ($ in the repl stays literal).
func TestUserRegexReplaceAll(t *testing.T) {
	re, err := compileUserRegex(`\d+`, true)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if got := re.replaceAll("abc123def456", maskMASK); got != "abc****def****" {
		t.Fatalf("replaceAll all-matches: got %q", got)
	}
	// A `$` in the replacement must be literal (not a regexp2/.NET group substitution).
	reX, _ := compileUserRegex(`x`, false)
	if got := reX.replaceAll("axb", "$1"); got != "a$1b" {
		t.Fatalf("literal $ replacement: got %q", got)
	}
}

// TestUserRegexASCIIParity asserts regexp2 masks ASCII identically to stdlib RE2 for the kind of
// patterns the corpus exercises — this is why swapping the engine keeps the byte-parity corpus green.
func TestUserRegexASCIIParity(t *testing.T) {
	for _, pat := range []string{`\d{3}-\d{2}-\d{4}`, `[\w.+-]+@[\w-]+\.[\w.-]+`, `\bsecret\b`} {
		re2 := regexp.MustCompile("(?s)" + pat)
		u, err := compileUserRegex(pat, true)
		if err != nil {
			t.Fatalf("regexp2 compile %q: %v", pat, err)
		}
		for _, in := range []string{"ssn 123-45-6789 end", "mail ops@example.com x", "a secret here", "nothing"} {
			want := re2.ReplaceAllString(in, maskMASK)
			if got := u.replaceAll(in, maskMASK); got != want {
				t.Fatalf("ASCII parity pat=%q in=%q: RE2=%q regexp2=%q", pat, in, want, got)
			}
		}
	}
}

// TestUserRegexClosesEngineGaps documents the capabilities regexp2 adds over RE2: Unicode \d and
// lookahead — the two things the audit flagged that stdlib regexp cannot do.
func TestUserRegexClosesEngineGaps(t *testing.T) {
	// Unicode \d matches fullwidth digits (Python `re` does; RE2 does not).
	u, _ := compileUserRegex(`\d+`, false)
	if got := u.replaceAll("ID：１２３４", maskMASK); got != "ID：****" {
		t.Fatalf("unicode \\d: got %q", got)
	}
	// Lookahead compiles (RE2 rejects it); confirms tag-rule / custom masking patterns with
	// lookahead are now accepted rather than silently dropped.
	if _, err := compileUserRegex(`foo(?=bar)`, false); err != nil {
		t.Fatalf("lookahead should compile under regexp2: %v", err)
	}
	if _, err := regexp.Compile(`foo(?=bar)`); err == nil {
		t.Fatalf("sanity: RE2 was expected to reject lookahead")
	}
}
