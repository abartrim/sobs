package main

// Unit tests for parseRegexFilterExpression (port of
// test_parse_regex_filter_expression_escaped_and plus edge cases).

import (
	"strings"
	"testing"
)

func eqSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestParseRegexFilterExpressionEscapedAnd(t *testing.T) {
	inc, exc, errMsg := parseRegexFilterExpression(`foo\&&bar&&baz&&!qux\&&z`)
	if errMsg != "" {
		t.Fatalf("unexpected error: %q", errMsg)
	}
	if !eqSlice(inc, []string{"foo&&bar", "baz"}) {
		t.Errorf("include = %v, want [foo&&bar baz]", inc)
	}
	if !eqSlice(exc, []string{"qux&&z"}) {
		t.Errorf("exclude = %v, want [qux&&z]", exc)
	}
}

func TestParseRegexFilterExpressionEmpty(t *testing.T) {
	inc, exc, errMsg := parseRegexFilterExpression("   ")
	if errMsg != "" || len(inc) != 0 || len(exc) != 0 {
		t.Errorf("got inc=%v exc=%v err=%q, want all empty", inc, exc, errMsg)
	}
}

func TestParseRegexFilterExpressionSimpleInclude(t *testing.T) {
	inc, exc, errMsg := parseRegexFilterExpression("timeout")
	if errMsg != "" {
		t.Fatalf("unexpected error: %q", errMsg)
	}
	if !eqSlice(inc, []string{"timeout"}) || len(exc) != 0 {
		t.Errorf("inc=%v exc=%v", inc, exc)
	}
}

func TestParseRegexFilterExpressionExclude(t *testing.T) {
	inc, exc, errMsg := parseRegexFilterExpression("!debug")
	if errMsg != "" {
		t.Fatalf("unexpected error: %q", errMsg)
	}
	if len(inc) != 0 || !eqSlice(exc, []string{"debug"}) {
		t.Errorf("inc=%v exc=%v", inc, exc)
	}
}

func TestParseRegexFilterExpressionBareNegationErrors(t *testing.T) {
	_, _, errMsg := parseRegexFilterExpression("!")
	if errMsg == "" {
		t.Error("expected error for bare '!'")
	}
}

func TestParseRegexFilterExpressionEmptyTermErrors(t *testing.T) {
	_, _, errMsg := parseRegexFilterExpression("foo&&&&bar")
	if errMsg == "" {
		t.Error("expected error for empty term around '&&'")
	}
}

func TestParseRegexFilterExpressionInvalidRegexErrors(t *testing.T) {
	_, _, errMsg := parseRegexFilterExpression("[invalid")
	if !strings.HasPrefix(errMsg, "Regex error:") {
		t.Errorf("got %q, want 'Regex error:' prefix", errMsg)
	}
}
