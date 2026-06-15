package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDecodeVLQSegment(t *testing.T) {
	cases := map[string][]int{
		"AAAA":  {0, 0, 0, 0},
		"C":     {1},
		"D":     {-1},
		"IAAAA": {4, 0, 0, 0, 0},
		"AACA":  {0, 0, 1, 0},
	}
	for in, want := range cases {
		if got := decodeVLQSegment(in); !reflect.DeepEqual(got, want) {
			t.Errorf("decodeVLQSegment(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseMappingsLookup(t *testing.T) {
	p := &parsedSourceMap{
		sources: []string{"src.js"},
		names:   []string{"myFunc"},
		lines:   parseMappings("IAAAA"), // one segment on generated line 0 at genCol 4
	}
	src, line, col, name, ok := p.lookup(0, 4)
	if !ok || src != "src.js" || line != 0 || col != 0 || name != "myFunc" {
		t.Errorf("lookup = (%q,%d,%d,%q,%v)", src, line, col, name, ok)
	}
	// A column before the first segment has no mapping.
	if _, _, _, _, ok := p.lookup(0, 0); ok {
		// genCol 4 > 0, so col 0 has no covering segment
		t.Error("col 0 should not map (first segment at genCol 4)")
	}
	// Out-of-range line.
	if _, _, _, _, ok := p.lookup(9, 0); ok {
		t.Error("out-of-range line should not map")
	}
	// A column PAST the only segment still resolves to it (bisect_right -> last token on the line).
	if src, _, _, _, ok := p.lookup(0, 99); !ok || src != "src.js" {
		t.Errorf("col past last segment should resolve to last token, got (%q,%v)", src, ok)
	}
}

// TestLookupSourcelessToken covers the `sourcemap` lib's behavior for a 1-value (source-less)
// segment: the decoder still builds a Token (src=empty-string default), and app.py treats it as a SUCCESSFUL
// lookup with src="" rather than a miss. The Go lookup must therefore return ok=true with an empty
// src/name so demangleStack falls back to the raw "{url}:{line}:{col}" target (not leave the frame
// unchanged).
func TestLookupSourcelessToken(t *testing.T) {
	// "IAAAA,QA" : two segments on generated line 0.
	//   IAAAA -> genCol 4, full mapping into src.js (origLine 0, origCol 0), name myFunc.
	//   QA    -> two VLQ values [8, 0]; genCol 4+8=12, no source (len < 4) -> source-less token.
	p := &parsedSourceMap{
		sources: []string{"src.js"},
		names:   []string{"myFunc"},
		lines:   parseMappings("IAAAA,QA"),
	}
	// A column landing on (or past) the source-less segment must SUCCEED with empty src/name.
	src, line, col, name, ok := p.lookup(0, 12)
	if !ok {
		t.Fatal("source-less token should be a successful lookup (Python returns src='')")
	}
	if src != "" || name != "" || line != 0 || col != 0 {
		t.Errorf("source-less lookup = (%q,%d,%d,%q); want empty src/name and zero line/col", src, line, col, name)
	}
	// The full segment still maps with its source when queried at its column.
	if src, _, _, name, ok := p.lookup(0, 4); !ok || src != "src.js" || name != "myFunc" {
		t.Errorf("full segment lookup = (%q,%q,%v); want src.js/myFunc/true", src, name, ok)
	}
}

// TestDemangleSourcelessFallback is the end-to-end consequence: a frame whose lookup lands on a
// source-less token is rewritten to "[mapped] {url}:{line}:{col}", matching app.py's
// `mapped_target = f"{url}:{line}:{col}"` branch when src is empty.
func TestDemangleSourcelessFallback(t *testing.T) {
	dir := t.TempDir()
	// Single source-less segment at genCol 0 on generated line 0 ("A" -> [0]).
	mapJSON := `{"version":3,"file":"app.min.js","sources":[],"names":[],"mappings":"A"}`
	if err := os.WriteFile(filepath.Join(dir, "app.min.js.map"), []byte(mapJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	sm := &sourceMapper{enable: true, dir: dir, cache: map[string]cachedSourceMap{}}

	stack := "    at https://cdn.example.com/app.min.js:1:5"
	got := sm.demangleStack(stack)
	want := "    at [mapped] https://cdn.example.com/app.min.js:1:5"
	if got != want {
		t.Errorf("source-less demangle =\n %q\nwant\n %q", got, want)
	}
}

func TestDemangleStackEndToEnd(t *testing.T) {
	dir := t.TempDir()
	mapJSON := `{"version":3,"file":"app.min.js","sources":["src.js"],"names":["myFunc"],"mappings":"IAAAA"}`
	if err := os.WriteFile(filepath.Join(dir, "app.min.js.map"), []byte(mapJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	sm := &sourceMapper{enable: true, dir: dir, cache: map[string]cachedSourceMap{}}

	stack := "    at https://cdn.example.com/app.min.js:1:5"
	got := sm.demangleStack(stack)
	want := "    at [mapped] myFunc (src.js:1:1)"
	if got != want {
		t.Errorf("demangle =\n %q\nwant\n %q", got, want)
	}

	// A non-frame line passes through untouched.
	if got := sm.demangleStack("Error: boom"); got != "Error: boom" {
		t.Errorf("non-frame line changed: %q", got)
	}
}

func TestDemangleDisabledIdentity(t *testing.T) {
	sm := &sourceMapper{enable: false}
	stack := "    at https://cdn.example.com/app.min.js:1:5"
	if got := sm.demangleStack(stack); got != stack {
		t.Errorf("disabled mapper should be identity, got %q", got)
	}
	// nil-safe (server.srcMap is always set, but defend the hot path).
	var nilSM *sourceMapper
	if got := nilSM.demangleStack(stack); got != stack {
		t.Errorf("nil mapper should be identity, got %q", got)
	}
}
