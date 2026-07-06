package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_masking_test.go — batch 16 targeted coverage for cmd/sobs/masking.go:
// validateCustomMaskingPattern's remaining safety-check branches (max-length, nested-quantifier
// ReDoS, ambiguous-alternation ReDoS, negative-lookbehind rejection, inline-flags stripped before
// the JS-compat check), and loadMaskingCustomPatterns' invalid-pattern-filtered / dedup branches.

func TestValidateCustomMaskingPatternRemainingBranches(t *testing.T) {
	t.Run("over max length rejected", func(t *testing.T) {
		long := strings.Repeat("a", maxCustomMaskingPatternLength+1)
		_, err := validateCustomMaskingPattern(long)
		if err == nil || !strings.Contains(err.Error(), "too long") {
			t.Fatalf("want a too-long error, got %v", err)
		}
	})

	t.Run("exactly max length is accepted", func(t *testing.T) {
		exact := strings.Repeat("a", maxCustomMaskingPatternLength)
		got, err := validateCustomMaskingPattern(exact)
		if err != nil {
			t.Fatalf("unexpected error at exact max length: %v", err)
		}
		if got != exact {
			t.Errorf("got %q, want unchanged pattern", got)
		}
	})

	t.Run("nested quantifier rejected (ReDoS)", func(t *testing.T) {
		_, err := validateCustomMaskingPattern(`(a+)+`)
		if err == nil || !strings.Contains(err.Error(), "nested quantifiers") {
			t.Fatalf("want nested-quantifier error, got %v", err)
		}
	})

	t.Run("quantified alternation rejected (ReDoS)", func(t *testing.T) {
		_, err := validateCustomMaskingPattern(`(a|b)+`)
		if err == nil || !strings.Contains(err.Error(), "quantified alternation") {
			t.Fatalf("want quantified-alternation error, got %v", err)
		}
	})

	t.Run("negative lookbehind rejected", func(t *testing.T) {
		_, err := validateCustomMaskingPattern(`(?<!foo)bar`)
		if err == nil || !strings.Contains(err.Error(), "lookbehind") {
			t.Fatalf("want lookbehind error, got %v", err)
		}
	})

	t.Run("inline flags stripped before JS-compat check succeeds", func(t *testing.T) {
		// (?i) is a valid inline-flags prefix; stripping it must not itself trip the lookbehind
		// check, and the pattern (with no lookbehind) should validate cleanly.
		got, err := validateCustomMaskingPattern(`(?i)hello\A\Zworld`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != `(?i)hello\A\Zworld` {
			t.Errorf("got %q, want the original (unmodified) pattern returned", got)
		}
	})

	t.Run(".NET-style named group does not itself trip lookbehind check", func(t *testing.T) {
		// regexp2 (.NET-flavored) accepts (?<name>...) but NOT Python's (?P<name>...); the
		// maskingNamedGroupRe substitution in validateCustomMaskingPattern targets the latter
		// syntax specifically, so this .NET-style pattern exercises the "no named-group rewrite
		// needed" path through the same JS-compat check.
		got, err := validateCustomMaskingPattern(`(?<year>\d{4})-(?<month>\d{2})`)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != `(?<year>\d{4})-(?<month>\d{2})` {
			t.Errorf("got %q", got)
		}
	})
}

func TestLoadMaskingCustomPatternsFiltersInvalidAndDedups(t *testing.T) {
	// masking.custom_patterns holds a JSON array; loadJSONStringListSetting parses it, then
	// validateCustomMaskingPattern filters out anything that fails validation, and duplicates
	// (including duplicates surviving validation) collapse via the seen-set.
	value := `["\\d{3}-\\d{4}", "\\d{3}-\\d{4}", "(a+)+", "   "]`
	s := &server{db: storetest.SettingsDB(map[string]string{"masking.custom_patterns": value})}
	got := s.loadMaskingCustomPatterns()
	if len(got) != 1 || got[0] != `\d{3}-\d{4}` {
		t.Fatalf("got %v, want exactly one deduped valid pattern", got)
	}
}

func TestLoadMaskingCustomPatternsEmptySetting(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	got := s.loadMaskingCustomPatterns()
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}
