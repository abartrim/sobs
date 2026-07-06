package jsonenc

import (
	"math"
	"testing"
)

// cov95_b15_jsonenc_test.go — batch 15 coverage for internal/jsonenc/jsonenc.go:
//   encodeFloat (202)   60.0%
//   encodeString (277)  63.2%
//
// jsonenc_test.go already pins PyFloatRepr's finite-float formatting exhaustively; this file
// targets encodeFloat's own non-finite branches (Inf/-Inf/NaN) and encodeString's escape-table
// branches (control chars, quote/backslash, EnsureASCII on/off, non-BMP surrogate pairs), plus
// Encode's dispatch for every supported Go value type (coverage_marshaljson_test.go exercises
// *Object nesting via MarshalJSON already; this fills in the direct-type dispatch and the
// TrailingNL/Indent options which those don't touch).

func TestEncode_FloatNonFiniteValues(t *testing.T) {
	cases := []struct {
		name string
		v    float64
		want string
	}{
		{"positive infinity", math.Inf(1), "Infinity"},
		{"negative infinity", math.Inf(-1), "-Infinity"},
		{"NaN", math.NaN(), "NaN"},
		{"finite value still delegates to PyFloatRepr", 1.5, "1.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(Encode(c.v, Compact))
			if got != c.want {
				t.Errorf("Encode(%v) = %q, want %q", c.v, got, c.want)
			}
		})
	}
}

func TestEncode_StringEscaping(t *testing.T) {
	// Non-ASCII / non-BMP inputs and their EnsureASCII=true expected output are built with
	// explicit \u / \U Go source escapes (never a literal glyph in this file) so the exact code
	// points are unambiguous.
	const cafeArrowInput = "café→"                // "café→"
	const cafeArrowLiteralWant = "\"café→\""      // the same text, quoted, EnsureASCII=false
	const cafeInput = "café"                      // "café"
	const cafeEscapedWant = "\"caf\\u00e9\""      // "café" ASCII-escaped
	const emojiInput = "\U0001F600"               // grinning-face emoji (astral plane)
	const emojiLiteralWant = "\"\U0001F600\""     // the same emoji, quoted, EnsureASCII=false
	const emojiEscapedWant = "\"\\ud83d\\ude00\"" // UTF-16 surrogate pair for U+1F600

	cases := []struct {
		name string
		in   string
		opts Options
		want string
	}{
		{"quote and backslash escaped", `a"b\c`, Compact, `"a\"b\\c"`},
		{"newline/cr/tab/backspace/formfeed", "a\nb\rc\td\be\ff", Compact, `"a\nb\rc\td\be\ff"`},
		{"other control char (0x01) uses \\u escape", "a\x01b", Compact, "\"a\\u0001b\""},
		{"DEL (0x7f) is NOT escaped (only <0x20 is)", "a\x7fb", Compact, "\"a\x7fb\""},
		{"ASCII passes through unescaped regardless of EnsureASCII", "hello", Compact, `"hello"`},
		{
			"non-ASCII left literal when EnsureASCII=false",
			cafeArrowInput,
			Options{EnsureASCII: false, ItemSep: ",", KeySep: ":"},
			cafeArrowLiteralWant,
		},
		{
			"non-ASCII escaped to \\uXXXX when EnsureASCII=true",
			cafeInput,
			Options{EnsureASCII: true, ItemSep: ",", KeySep: ":"},
			cafeEscapedWant,
		},
		{
			"astral-plane rune becomes a UTF-16 surrogate pair under EnsureASCII",
			emojiInput,
			Options{EnsureASCII: true, ItemSep: ",", KeySep: ":"},
			emojiEscapedWant,
		},
		{
			"astral-plane rune left as literal UTF-8 when EnsureASCII=false",
			emojiInput,
			Options{EnsureASCII: false, ItemSep: ",", KeySep: ":"},
			emojiLiteralWant,
		},
		{"empty string", "", Compact, `""`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(Encode(c.in, c.opts))
			if got != c.want {
				t.Errorf("Encode(%q, %+v) = %q, want %q", c.in, c.opts, got, c.want)
			}
		})
	}
}

func TestEncode_ValueDispatchAndOptions(t *testing.T) {
	// Every directly-supported scalar/collection type through the top-level value() dispatch.
	if got := string(Encode(nil, Compact)); got != "null" {
		t.Errorf("Encode(nil) = %q, want null", got)
	}
	if got := string(Encode(true, Compact)); got != "true" {
		t.Errorf("Encode(true) = %q, want true", got)
	}
	if got := string(Encode(false, Compact)); got != "false" {
		t.Errorf("Encode(false) = %q, want false", got)
	}
	if got := string(Encode(42, Compact)); got != "42" {
		t.Errorf("Encode(int 42) = %q, want 42", got)
	}
	if got := string(Encode(int64(-7), Compact)); got != "-7" {
		t.Errorf("Encode(int64 -7) = %q, want -7", got)
	}
	if got := string(Encode([]any{1, "a", nil}, Compact)); got != `[1,"a",null]` {
		t.Errorf("Encode(slice) = %q", got)
	}
	if got := string(Encode([]any{}, Compact)); got != "[]" {
		t.Errorf("Encode(empty slice) = %q, want []", got)
	}
	// Unsupported type (e.g. a struct) falls back to a %v string rendering rather than panicking.
	type unsupported struct{ X int }
	if got := string(Encode(unsupported{X: 5}, Compact)); got != `"{5}"` {
		t.Errorf("Encode(unsupported struct) = %q, want the %%v string fallback", got)
	}

	// TrailingNL appends exactly one '\n' after the value.
	if got := string(Encode(1, Options{ItemSep: ",", KeySep: ":", TrailingNL: true})); got != "1\n" {
		t.Errorf("Encode with TrailingNL = %q, want trailing newline", got)
	}

	// Indent mode: nested object/array get real newlines+indentation, empty containers stay inline.
	obj := NewObject().Set("a", 1).Set("b", []any{1, 2})
	got := string(Encode(obj, Options{ItemSep: ",", KeySep: ": ", Indent: "  "}))
	want := "{\n  \"a\": 1,\n  \"b\": [\n    1,\n    2\n  ]\n}"
	if got != want {
		t.Errorf("Encode with Indent =\n%q\nwant\n%q", got, want)
	}
	if got := string(Encode(NewObject(), Options{Indent: "  ", ItemSep: ","})); got != "{}" {
		t.Errorf("Encode empty object with Indent = %q, want inline {}", got)
	}
	if got := string(Encode([]any{}, Options{Indent: "  ", ItemSep: ","})); got != "[]" {
		t.Errorf("Encode empty array with Indent = %q, want inline []", got)
	}
}

func TestEncode_ObjectSortKeys(t *testing.T) {
	obj := NewObject().Set("zeta", 1).Set("alpha", 2)
	got := string(Encode(obj, Options{SortKeys: true, ItemSep: ",", KeySep: ":"}))
	if got != `{"alpha":2,"zeta":1}` {
		t.Errorf("Encode with SortKeys = %q, want alpha before zeta", got)
	}
	// Without SortKeys, insertion order is preserved.
	gotUnsorted := string(Encode(obj, Options{SortKeys: false, ItemSep: ",", KeySep: ":"}))
	if gotUnsorted != `{"zeta":1,"alpha":2}` {
		t.Errorf("Encode without SortKeys = %q, want insertion order", gotUnsorted)
	}
}
