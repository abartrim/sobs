package main

import (
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Coverage batch 14: cmd/sobs/fix_forms.go's pure helpers — pyParseInt/pyIntBody (CPython int()
// parsing semantics: sign, underscore digit grouping, whitespace, error repr) and
// chartObjStr/chartObjInt (verbatim getCharts()-entry field readers), which were previously only
// exercised indirectly (and partially) via applyDMTTL's TTL-days branch.

func TestPyParseInt_Success(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"30", 30},
		{"0", 0},
		{"+7", 7},
		{"-7", -7},
		{"1_000", 1000},
		{"-1_000", -1000},
		{"  42  ", 42}, // surrounding whitespace stripped
	}
	for _, c := range cases {
		n, errMsg := pyParseInt(c.in)
		if errMsg != "" {
			t.Errorf("pyParseInt(%q) unexpected error %q", c.in, errMsg)
		}
		if n != c.want {
			t.Errorf("pyParseInt(%q) = %d, want %d", c.in, n, c.want)
		}
	}
}

func TestPyParseInt_Failure(t *testing.T) {
	cases := []string{
		"",
		"abc",
		"1.5",
		"1__000", // doubled underscore
		"_1",     // leading underscore
		"1_",     // trailing underscore
		"+",
		"-",
		"12a",
	}
	for _, in := range cases {
		n, errMsg := pyParseInt(in)
		if errMsg == "" {
			t.Errorf("pyParseInt(%q) expected an error, got n=%d", in, n)
		}
		if n != 0 {
			t.Errorf("pyParseInt(%q) failure should return 0, got %d", in, n)
		}
	}
}

// TestPyParseInt_ErrorMessageMatchesCPythonRepr proves the ValueError text quotes the ORIGINAL
// (un-stripped) argument via pyRepr, matching CPython's int("badval") message exactly.
func TestPyParseInt_ErrorMessageMatchesCPythonRepr(t *testing.T) {
	_, errMsg := pyParseInt("  bad-val  ")
	want := "invalid literal for int() with base 10: " + pyRepr("  bad-val  ", true)
	if errMsg != want {
		t.Errorf("errMsg = %q, want %q", errMsg, want)
	}
}

func TestPyIntBody_EdgeCases(t *testing.T) {
	if n, ok := pyIntBody(""); ok || n != 0 {
		t.Errorf("empty string should fail, got (%d,%v)", n, ok)
	}
	if n, ok := pyIntBody("+"); ok || n != 0 {
		t.Errorf("bare + should fail, got (%d,%v)", n, ok)
	}
	if n, ok := pyIntBody("-"); ok || n != 0 {
		t.Errorf("bare - should fail, got (%d,%v)", n, ok)
	}
	if n, ok := pyIntBody("12_3"); !ok || n != 123 {
		t.Errorf("12_3 should parse to 123, got (%d,%v)", n, ok)
	}
	if _, ok := pyIntBody("12__3"); ok {
		t.Error("doubled underscore should fail")
	}
	if _, ok := pyIntBody("_123"); ok {
		t.Error("leading underscore should fail")
	}
	if _, ok := pyIntBody("123_"); ok {
		t.Error("trailing underscore should fail")
	}
	if _, ok := pyIntBody("12x3"); ok {
		t.Error("non-digit should fail")
	}
}

func TestChartObjStr(t *testing.T) {
	o := jsonenc.NewObject().Set("title", "My Chart").Set("position", 3).Set("nullish", nil)
	if got := chartObjStr(o, "title"); got != "My Chart" {
		t.Errorf("title = %q", got)
	}
	// Present but non-string -> "" (no coercion, unlike objStrOr).
	if got := chartObjStr(o, "position"); got != "" {
		t.Errorf("non-string field should yield empty string, got %q", got)
	}
	// Absent key -> "".
	if got := chartObjStr(o, "missing"); got != "" {
		t.Errorf("missing key should yield empty string, got %q", got)
	}
	// Present but nil -> "" (type assertion to string fails).
	if got := chartObjStr(o, "nullish"); got != "" {
		t.Errorf("nil value should yield empty string, got %q", got)
	}
}

func TestChartObjInt(t *testing.T) {
	o := jsonenc.NewObject().
		Set("pos_int", 5).
		Set("pos_int64", int64(9)).
		Set("pos_float", float64(7)).
		Set("pos_str", "3").
		Set("pos_nil", nil)
	if got := chartObjInt(o, "pos_int"); got != 5 {
		t.Errorf("int variant = %d", got)
	}
	if got := chartObjInt(o, "pos_int64"); got != 9 {
		t.Errorf("int64 variant = %d", got)
	}
	if got := chartObjInt(o, "pos_float"); got != 7 {
		t.Errorf("float64 variant = %d", got)
	}
	// A string value isn't one of the switch's typed cases -> falls through to the 0 default.
	if got := chartObjInt(o, "pos_str"); got != 0 {
		t.Errorf("unsupported type should yield 0, got %d", got)
	}
	if got := chartObjInt(o, "pos_nil"); got != 0 {
		t.Errorf("nil should yield 0, got %d", got)
	}
	if got := chartObjInt(o, "missing"); got != 0 {
		t.Errorf("missing key should yield 0, got %d", got)
	}
}

func TestNotifViewRedirect(t *testing.T) {
	s := &server{cfg: config{BasePath: ""}}
	if got := s.notifViewRedirect(""); got != "/settings/notifications" {
		t.Errorf("no edit id = %q", got)
	}
	got := s.notifViewRedirect("rule:1")
	if got != "/settings/notifications?edit_rule=rule:1" {
		t.Errorf("edit id with colon = %q", got)
	}
}
