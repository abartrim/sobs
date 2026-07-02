package store

import (
	"math"
	"strings"
	"testing"
)

// --- quoteLiteral ------------------------------------------------------------------

func TestQuoteLiteralNil(t *testing.T) {
	if got := quoteLiteral(nil); got != "NULL" {
		t.Errorf("quoteLiteral(nil) = %q, want NULL", got)
	}
}

func TestQuoteLiteralInt(t *testing.T) {
	cases := map[int]string{
		0:            "0",
		1:            "1",
		-1:           "-1",
		42:           "42",
		-42:          "-42",
		math.MaxInt8: "127",
	}
	for in, want := range cases {
		if got := quoteLiteral(in); got != want {
			t.Errorf("quoteLiteral(int %d) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteLiteralInt64(t *testing.T) {
	cases := map[int64]string{
		0:                    "0",
		9223372036854775807:  "9223372036854775807",
		-9223372036854775808: "-9223372036854775808",
	}
	for in, want := range cases {
		if got := quoteLiteral(in); got != want {
			t.Errorf("quoteLiteral(int64 %d) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteLiteralBool(t *testing.T) {
	if got := quoteLiteral(true); got != "1" {
		t.Errorf("quoteLiteral(true) = %q, want 1", got)
	}
	if got := quoteLiteral(false); got != "0" {
		t.Errorf("quoteLiteral(false) = %q, want 0", got)
	}
}

func TestQuoteLiteralStringBasic(t *testing.T) {
	cases := map[string]string{
		"":           "''",
		"hello":      "'hello'",
		"with space": "'with space'",
	}
	for in, want := range cases {
		if got := quoteLiteral(in); got != want {
			t.Errorf("quoteLiteral(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestQuoteLiteralStringInjectionShapes exercises the classic SQL-injection-shaped
// inputs: single quotes (literal terminators), backslashes (ClickHouse escape
// character), semicolons (statement separators), and combinations. Every single quote
// and backslash in the input must come out escaped, and the whole value must still be
// wrapped in a single pair of unescaped quotes so it parses as one string literal.
func TestQuoteLiteralStringInjectionShapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"single_quote", `O'Brien`, `'O\'Brien'`},
		{"classic_injection", `'; DROP TABLE users; --`, `'\'; DROP TABLE users; --'`},
		{"or_1_1", `' OR '1'='1`, `'\' OR \'1\'=\'1'`},
		{"backslash", `C:\path\to\file`, `'C:\\path\\to\\file'`},
		{"backslash_then_quote", `\'`, `'\\\''`},
		{"semicolon_only", `a; b`, `'a; b'`},
		{"double_quote_untouched", `say "hi"`, `'say "hi"'`},
		{"quote_and_backslash_mixed", `\a'b\c'd`, `'\\a\'b\\c\'d'`},
		{"multiple_consecutive_quotes", `''''`, `'\'\'\'\''`},
		{"null_byte", "a\x00b", "'a\x00b'"},
		{"newline", "a\nb", "'a\nb'"},
		{"unicode", "héllo wörld", "'héllo wörld'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := quoteLiteral(c.in)
			if got != c.want {
				t.Errorf("quoteLiteral(%q) = %q, want %q", c.in, got, c.want)
			}
			// The result must start and end with a single unescaped quote, and every
			// quote in between must be preceded by a backslash — i.e. it re-parses as
			// exactly one ClickHouse string literal, not something that terminates
			// early and appends attacker SQL.
			if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
				t.Fatalf("quoteLiteral(%q) = %q: does not look like a quoted literal", c.in, got)
			}
			inner := got[1 : len(got)-1]
			for i := 0; i < len(inner); i++ {
				if inner[i] == '\'' {
					t.Fatalf("quoteLiteral(%q) = %q: unescaped quote inside literal body at %d", c.in, got, i)
				}
				if inner[i] == '\\' {
					// Every backslash must be followed by another backslash or an
					// escaped quote — never dangling (which would escape the closing
					// quote and break out of the literal).
					if i+1 >= len(inner) {
						t.Fatalf("quoteLiteral(%q) = %q: dangling backslash before closing quote", c.in, got)
					}
					i++ // skip the escaped char
				}
			}
		})
	}
}

func TestQuoteLiteralFloatNormal(t *testing.T) {
	cases := map[float64]string{
		0:        "0",
		1:        "1",
		-1:       "-1",
		3.14:     "3.14",
		-3.14:    "-3.14",
		0.5:      "0.5",
		100000.0: "100000",
		1e300:    "1e+300",
		1e-300:   "1e-300",
	}
	for in, want := range cases {
		if got := quoteLiteral(in); got != want {
			t.Errorf("quoteLiteral(float64 %v) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteLiteralFloatNegativeZero(t *testing.T) {
	negZero := math.Copysign(0, -1)
	got := quoteLiteral(negZero)
	// strconv.FormatFloat renders negative zero as "-0"; this is valid ClickHouse
	// numeric syntax, so just confirm it doesn't fall through to the non-finite path.
	if got != "-0" {
		t.Errorf("quoteLiteral(-0.0) = %q, want -0", got)
	}
}

// TestQuoteLiteralFloatNonFinite is the regression test for the PR #352 review finding:
// NaN/+Inf/-Inf must render as valid, unambiguous ClickHouse literal syntax. We assert
// the exact CAST(...) form rather than the bare strconv spelling (NaN/+Inf/-Inf), since
// a bareword literal is resolved as a column/alias identifier first if one of that exact
// name is in scope (e.g. a WHERE clause referencing a column literally named "nan"),
// silently producing the wrong result with no error. CAST('...' AS Float64) is immune to
// that identifier-shadowing hazard and is valid ClickHouse syntax at any query position.
func TestQuoteLiteralFloatNonFinite(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want string
	}{
		{"nan", math.NaN(), "CAST('nan' AS Float64)"},
		{"pos_inf", math.Inf(1), "CAST('inf' AS Float64)"},
		{"neg_inf", math.Inf(-1), "CAST('-inf' AS Float64)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := quoteLiteral(c.in)
			if got != c.want {
				t.Errorf("quoteLiteral(%s) = %q, want %q", c.name, got, c.want)
			}
			// Must never be the bare, unquoted strconv spelling that a case-insensitive
			// bareword-identifier collision could misinterpret.
			if got == "NaN" || got == "+Inf" || got == "-Inf" || got == "Inf" {
				t.Errorf("quoteLiteral(%s) = %q: emitted bare strconv spelling, not the safe cast form", c.name, got)
			}
		})
	}
}

func TestQuoteLiteralFloatNonFiniteNotAcceptedAsBareIdentifierRisk(t *testing.T) {
	// Defensive check: whatever quoteLiteral emits for NaN must be wrapped such that it
	// cannot be interpreted as a bare SQL identifier/column reference by a naive
	// substring check. The CAST(...) form always contains a '(' immediately after a
	// keyword, which a plain identifier never does.
	got := quoteLiteral(math.NaN())
	if !strings.Contains(got, "(") || !strings.Contains(got, ")") {
		t.Errorf("quoteLiteral(NaN) = %q: expected a function-call/cast form, not a bareword", got)
	}
}

func TestQuoteLiteralDefaultFallback(t *testing.T) {
	// Uncovered concrete types fall through to the %v + quoted-string branch. Use a
	// simple struct with a String()-free Stringer-less type to hit the default case.
	type customID struct{ N int }
	got := quoteLiteral(customID{N: 7})
	want := "'{7}'"
	if got != want {
		t.Errorf("quoteLiteral(customID{7}) = %q, want %q", got, want)
	}
}

func TestQuoteLiteralDefaultFallbackEscapesQuotes(t *testing.T) {
	// A %v-formatted value containing a quote must still come out escaped — the default
	// branch reuses chEscape, not raw concatenation.
	type wrapped struct{ S string }
	// %v of a struct with a string field containing a quote: {it's}
	got := quoteLiteral(wrapped{S: "it's"})
	want := "'{it\\'s}'"
	if got != want {
		t.Errorf("quoteLiteral(wrapped{it's}) = %q, want %q", got, want)
	}
}

// --- chEscape ------------------------------------------------------------------------

func TestChEscape(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"plain":       "plain",
		"O'Brien":     `O\'Brien`,
		`a\b`:         `a\\b`,
		"quote'end":   `quote\'end`,
		`\'`:          `\\\'`,
		"''''":        `\'\'\'\'`,
		`back\\slash`: `back\\\\slash`,
	}
	for in, want := range cases {
		if got := chEscape(in); got != want {
			t.Errorf("chEscape(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- inlineParams ----------------------------------------------------------------------

func TestInlineParamsNoPlaceholders(t *testing.T) {
	q := "SELECT 1"
	got, err := inlineParams(q, nil)
	if err != nil || got != q {
		t.Errorf("inlineParams(no params) = %q, err %v; want %q, nil", got, err, q)
	}
}

func TestInlineParamsEmptyParamsSliceLeavesQueryUnchangedEvenWithPlaceholder(t *testing.T) {
	// len(params) == 0 short-circuits before scanning, even if the query text has a "?".
	q := "SELECT * FROM t WHERE x = ?"
	got, err := inlineParams(q, []any{})
	if err != nil || got != q {
		t.Errorf("inlineParams with empty params slice = %q, err %v; want unchanged query", got, err)
	}
}

func TestInlineParamsBasicSubstitution(t *testing.T) {
	q := "SELECT * FROM t WHERE id = ? AND name = ? AND active = ?"
	got, err := inlineParams(q, []any{42, "alice", true})
	want := "SELECT * FROM t WHERE id = 42 AND name = 'alice' AND active = 1"
	if err != nil || got != want {
		t.Errorf("inlineParams = %q, err %v; want %q", got, err, want)
	}
}

func TestInlineParamsNotEnoughParams(t *testing.T) {
	q := "SELECT * FROM t WHERE id = ? AND name = ?"
	_, err := inlineParams(q, []any{42})
	if err == nil {
		t.Fatal("expected error for insufficient params, got nil")
	}
}

func TestInlineParamsPlaceholderInsideSingleQuotedStringIsIgnored(t *testing.T) {
	// A literal "?" inside a string literal must NOT be treated as a placeholder, and
	// must not consume a param.
	q := "SELECT * FROM t WHERE label = 'what?' AND id = ?"
	got, err := inlineParams(q, []any{7})
	want := "SELECT * FROM t WHERE label = 'what?' AND id = 7"
	if err != nil || got != want {
		t.Errorf("inlineParams (quoted ?) = %q, err %v; want %q", got, err, want)
	}
}

func TestInlineParamsPlaceholderInsideDoubleQuotedStringIsIgnored(t *testing.T) {
	q := `SELECT * FROM t WHERE label = "what?" AND id = ?`
	got, err := inlineParams(q, []any{7})
	want := `SELECT * FROM t WHERE label = "what?" AND id = 7`
	if err != nil || got != want {
		t.Errorf("inlineParams (double-quoted ?) = %q, err %v; want %q", got, err, want)
	}
}

func TestInlineParamsQuoteCharactersInsideStringParamsAreEscapedNotBreakingOut(t *testing.T) {
	// The classic injection attempt: a string param containing a quote + SQL keywords
	// must not be able to terminate the literal early.
	q := "SELECT * FROM t WHERE name = ?"
	got, err := inlineParams(q, []any{`'; DROP TABLE t; --`})
	want := `SELECT * FROM t WHERE name = '\'; DROP TABLE t; --'`
	if err != nil || got != want {
		t.Errorf("inlineParams (injection attempt) = %q, err %v; want %q", got, err, want)
	}
}

func TestInlineParamsNullParam(t *testing.T) {
	q := "SELECT * FROM t WHERE deleted_at = ?"
	got, err := inlineParams(q, []any{nil})
	want := "SELECT * FROM t WHERE deleted_at = NULL"
	if err != nil || got != want {
		t.Errorf("inlineParams(nil) = %q, err %v; want %q", got, err, want)
	}
}

func TestInlineParamsNonFiniteFloatParam(t *testing.T) {
	q := "SELECT * FROM t WHERE v = ? AND w = ? AND z = ?"
	got, err := inlineParams(q, []any{math.NaN(), math.Inf(1), math.Inf(-1)})
	want := "SELECT * FROM t WHERE v = CAST('nan' AS Float64) AND w = CAST('inf' AS Float64)" +
		" AND z = CAST('-inf' AS Float64)"
	if err != nil || got != want {
		t.Errorf("inlineParams(nan/inf/-inf) = %q, err %v; want %q", got, err, want)
	}
}

func TestInlineParamsMultiplePlaceholdersOrderPreserved(t *testing.T) {
	q := "?-?-?-?"
	got, err := inlineParams(q, []any{1, 2, 3, 4})
	want := "1-2-3-4"
	if err != nil || got != want {
		t.Errorf("inlineParams order = %q, err %v; want %q", got, err, want)
	}
}

func TestInlineParamsExtraParamsIgnored(t *testing.T) {
	// More params than placeholders is not an error — only the ones consumed matter.
	q := "SELECT ?"
	got, err := inlineParams(q, []any{1, 2, 3})
	want := "SELECT 1"
	if err != nil || got != want {
		t.Errorf("inlineParams (extra params) = %q, err %v; want %q", got, err, want)
	}
}
