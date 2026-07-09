package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Small pure utility helpers across the codebase, each mirroring a Python expression
// (str() coercions, sql-literal quoting, hex check, truthy settings, etc.). Corpus coverage
// reaches some only incidentally; these lock the behavior down directly and cheaply.

func TestStrOrEmpty(t *testing.T) {
	if strOrEmpty("x") != "x" || strOrEmpty(5) != "" || strOrEmpty(nil) != "" {
		t.Error("strOrEmpty")
	}
}

func TestTruncate80(t *testing.T) {
	if got := truncate80("short"); got != "short" {
		t.Errorf("short: %q", got)
	}
	if got := truncate80(strings.Repeat("a", 100)); len([]rune(got)) != 80 {
		t.Errorf("long: len %d, want 80", len([]rune(got)))
	}
}

func TestSQLQuoteLiteral(t *testing.T) {
	if got := sqlQuoteLiteral("x"); got != "'x'" {
		t.Errorf("plain: %q", got)
	}
	if got := sqlQuoteLiteral("a'b"); got != "'a''b'" {
		t.Errorf("quote doubling: %q", got)
	}
}

func TestFormatPyNumber(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{{5.0, "5"}, {0.0, "0"}, {-3.0, "-3"}, {2.5, "2.5"}}
	for _, c := range cases {
		if got := formatPyNumber(c.in); got != c.want {
			t.Errorf("formatPyNumber(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsHexStr(t *testing.T) {
	for _, s := range []string{"", "abc123", "ABCDEF", "0099ff"} {
		if !isHexStr(s) {
			t.Errorf("isHexStr(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"xyz", "12g", "0x10", "ab cd"} {
		if isHexStr(s) {
			t.Errorf("isHexStr(%q) = true, want false", s)
		}
	}
}

func TestMinIntIntAbs64(t *testing.T) {
	if minInt(2, 5) != 2 || minInt(5, 2) != 2 || minInt(3, 3) != 3 {
		t.Error("minInt")
	}
	if intAbs64(-5) != 5 || intAbs64(5) != 5 || intAbs64(0) != 0 {
		t.Error("intAbs64")
	}
}

func TestKvStringSingle(t *testing.T) {
	// Single key -> deterministic (map iteration order is irrelevant with one entry).
	if got := kvString(map[string]any{"k": "v"}); got != "k=v" {
		t.Errorf("kvString = %q, want k=v", got)
	}
}

func TestHas404(t *testing.T) {
	if !has404("github error 404 not found") || !has404("404") {
		t.Error("has404 should match a bare 404 token")
	}
	if has404("status 200 ok") || has404("error 4040") {
		t.Error("has404 should not match non-token 404")
	}
}

func TestAnyOrEmpty(t *testing.T) {
	if got := anyOrEmpty(nil); got == nil || len(got) != 0 {
		t.Errorf("nil -> %v, want empty non-nil", got)
	}
	if got := anyOrEmpty([]any{1, 2}); len(got) != 2 {
		t.Errorf("passthrough len %d, want 2", len(got))
	}
}

func TestMapsEqualInt(t *testing.T) {
	a := map[string]int{"x": 1, "y": 2}
	if !mapsEqualInt(a, map[string]int{"x": 1, "y": 2}) {
		t.Error("equal maps")
	}
	if mapsEqualInt(a, map[string]int{"x": 1, "y": 3}) {
		t.Error("differing value")
	}
	if mapsEqualInt(a, map[string]int{"x": 1}) {
		t.Error("differing length")
	}
}

func TestAnyToInt(t *testing.T) {
	cases := []struct {
		in   any
		want int
	}{{5, 5}, {int64(7), 7}, {3.9, 3}, {json.Number("4"), 4}}
	for _, c := range cases {
		if got := anyToInt(c.in); got != c.want {
			t.Errorf("anyToInt(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestIsTruthySetting(t *testing.T) {
	for _, s := range []string{"1", "true", "YES", " on "} {
		if !isTruthySetting(s) {
			t.Errorf("isTruthySetting(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"0", "false", "", "no"} {
		if isTruthySetting(s) {
			t.Errorf("isTruthySetting(%q) = true, want false", s)
		}
	}
}

func TestSplitSQLStatements(t *testing.T) {
	got := splitSQLStatements("a; b ;; c ;")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("splitSQLStatements = %v", got)
	}
}

func TestEmailUseTLS(t *testing.T) {
	if !emailUseTLS(jsonenc.NewObject()) { // default "1" -> true
		t.Error("default should be true")
	}
	if !emailUseTLS(jsonenc.NewObject().Set("use_tls", "yes")) {
		t.Error("yes -> true")
	}
	if emailUseTLS(jsonenc.NewObject().Set("use_tls", "0")) {
		t.Error("0 -> false")
	}
}

func TestScalarStr(t *testing.T) {
	if scalarStr("x") != "x" || scalarStr(nil) != "" || scalarStr(true) != "True" || scalarStr(false) != "False" {
		t.Error("scalarStr")
	}
}

func TestPyStr2OrDefault(t *testing.T) {
	if pyStr2OrDefault("", "d") != "d" || pyStr2OrDefault("x", "d") != "x" {
		t.Error("pyStr2OrDefault")
	}
}
