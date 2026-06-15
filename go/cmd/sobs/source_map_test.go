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
