package jsonenc

// coverage_utf16_test.go — oracle-anchored unit test for utf16Pair.
//
// TESTED:
//   utf16Pair(r rune) (rune, rune)  (jsonenc.go:311) — the UTF-16 surrogate-pair
//     split used when EnsureASCII=True emits an astral-plane rune as
//     \uXXXX\uXXXX (jsonenc.go:300-305). This mirrors CPython's json encoder,
//     which escapes any rune > U+FFFF as a high/low surrogate pair.
//
// Oracle values captured from CPython:
//   json.dumps(chr(0x1F600)) == "😀", etc. (ensure_ascii default True).
// The algorithm: c -= 0x10000; high = 0xD800 + (c >> 10); low = 0xDC00 + (c & 0x3FF).

import "testing"

func TestUtf16Pair_SurrogateSplit(t *testing.T) {
	cases := []struct {
		desc           string
		r              rune
		wantHi, wantLo rune
	}{
		// CPython: json.dumps("\U0001F600") -> "😀" (😀 grinning face)
		{"U+1F600 emoji", 0x1F600, 0xD83D, 0xDE00},
		// First astral code point: json.dumps("\U00010000") -> "𐀀"
		{"U+10000 lowest astral", 0x10000, 0xD800, 0xDC00},
		// Last valid code point: json.dumps("\U0010FFFF") -> "􏿿"
		{"U+10FFFF highest", 0x10FFFF, 0xDBFF, 0xDFFF},
		// Musical symbol G clef: json.dumps("\U0001D11E") -> "𝄞"
		{"U+1D11E G clef", 0x1D11E, 0xD834, 0xDD1E},
		// Pile of poo: json.dumps("\U0001F4A9") -> "💩"
		{"U+1F4A9 pile of poo", 0x1F4A9, 0xD83D, 0xDCA9},
		// CJK ext B: json.dumps("\U0002070E") -> "𠜎"
		{"U+2070E CJK ext-B", 0x2070E, 0xD841, 0xDF0E},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			hi, lo := utf16Pair(c.r)
			if hi != c.wantHi || lo != c.wantLo {
				t.Errorf("utf16Pair(%#x) = (%#x, %#x), want (%#x, %#x)",
					c.r, hi, lo, c.wantHi, c.wantLo)
			}
			// Both halves must land in the valid surrogate ranges.
			if hi < 0xD800 || hi > 0xDBFF {
				t.Errorf("high surrogate %#x out of [D800,DBFF]", hi)
			}
			if lo < 0xDC00 || lo > 0xDFFF {
				t.Errorf("low surrogate %#x out of [DC00,DFFF]", lo)
			}
		})
	}
}

// TestUtf16Pair_RoundTrip verifies the pair recombines back to the original rune
// via the standard UTF-16 surrogate-decoding formula — a cross-check independent
// of the hand-tabulated oracle values above.
func TestUtf16Pair_RoundTrip(t *testing.T) {
	for r := rune(0x10000); r <= 0x10FFFF; r += 0x111 {
		hi, lo := utf16Pair(r)
		got := 0x10000 + ((hi - 0xD800) << 10) + (lo - 0xDC00)
		if got != r {
			t.Fatalf("round-trip failed for %#x: pair (%#x,%#x) -> %#x", r, hi, lo, got)
		}
	}
}
