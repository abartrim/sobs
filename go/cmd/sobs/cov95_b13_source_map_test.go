package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// A minimal Source Map v3 document for "app.min.js" mapping generated line 0 col 0 to
// original line 0 col 0 in "app.js" with name "foo".
// Segment "AAAAA" (VLQ, 5 single-char values since each has continuation bit 0) decodes to
// [0,0,0,0,0] -> genCol0, srcIdx0, origLine0, origCol0, nameIdx0 ("foo").
const testSourceMapJSON = `{"version":3,"sources":["app.js"],"names":["foo"],"mappings":"AAAAA"}`

func writeMapFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// ---- maxInt --------------------------------------------------------------------------------

func TestMaxInt(t *testing.T) {
	if got := maxInt(3, 5); got != 5 {
		t.Errorf("maxInt(3,5) = %d, want 5", got)
	}
	if got := maxInt(5, 3); got != 5 {
		t.Errorf("maxInt(5,3) = %d, want 5", got)
	}
	if got := maxInt(-1, -1); got != -1 {
		t.Errorf("maxInt(-1,-1) = %d, want -1", got)
	}
}

// ---- decodeVLQSegment ------------------------------------------------------------------------
// (decodeVLQSegment's basic cases are already covered by TestDecodeVLQSegment in
// source_map_test.go; this adds the untested invalid-character and empty-input edge cases.)

func TestDecodeVLQSegment_EdgeCases(t *testing.T) {
	cases := []struct {
		name string
		seg  string
		want []int
	}{
		{"invalid char stops decode", "A!A", []int{0}},
		{"empty string", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodeVLQSegment(c.seg)
			if len(got) != len(c.want) {
				t.Fatalf("decodeVLQSegment(%q) = %v, want %v", c.seg, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("decodeVLQSegment(%q)[%d] = %d, want %d", c.seg, i, got[i], c.want[i])
				}
			}
		})
	}
}

// ---- parseMappings ---------------------------------------------------------------------------

func TestParseMappings(t *testing.T) {
	t.Run("single 4-value segment", func(t *testing.T) {
		lines := parseMappings("AAAA")
		if len(lines) != 1 || len(lines[0]) != 1 {
			t.Fatalf("lines = %#v", lines)
		}
		seg := lines[0][0]
		if seg.genCol != 0 || seg.srcIdx != 0 || seg.origLine != 0 || seg.origCol != 0 {
			t.Errorf("seg = %#v", seg)
		}
	})

	t.Run("multiple generated lines separated by ;", func(t *testing.T) {
		lines := parseMappings("AAAA;AACA")
		if len(lines) != 2 {
			t.Fatalf("want 2 lines, got %d: %#v", len(lines), lines)
		}
	})

	t.Run("empty segment string between commas skipped", func(t *testing.T) {
		lines := parseMappings("AAAA,,AACA")
		if len(lines) != 1 || len(lines[0]) != 2 {
			t.Fatalf("want 1 line with 2 segments, got %#v", lines)
		}
	})

	t.Run("1-value segment (no source mapping)", func(t *testing.T) {
		lines := parseMappings("A")
		if len(lines) != 1 || len(lines[0]) != 1 {
			t.Fatalf("lines = %#v", lines)
		}
		if lines[0][0].srcIdx != -1 {
			t.Errorf("srcIdx = %d, want -1 for a 1-value segment", lines[0][0].srcIdx)
		}
	})

	t.Run("5-value segment carries a name index", func(t *testing.T) {
		lines := parseMappings("AAAAC")
		if len(lines) != 1 || len(lines[0]) != 1 {
			t.Fatalf("lines = %#v", lines)
		}
		if lines[0][0].nameIdx != 1 { // 'C' VLQ decodes to 1
			t.Errorf("nameIdx = %d, want 1", lines[0][0].nameIdx)
		}
	})

	t.Run("garbage segment with no decodable values is skipped", func(t *testing.T) {
		lines := parseMappings("!!!")
		if len(lines) != 1 || len(lines[0]) != 0 {
			t.Fatalf("want 1 empty line, got %#v", lines)
		}
	})
}

// ---- parseSourceMap ---------------------------------------------------------------------------

func TestParseSourceMap(t *testing.T) {
	t.Run("valid map", func(t *testing.T) {
		sm := parseSourceMap([]byte(testSourceMapJSON))
		if sm == nil {
			t.Fatal("want non-nil parsedSourceMap")
		}
		if len(sm.sources) != 1 || sm.sources[0] != "app.js" {
			t.Errorf("sources = %#v", sm.sources)
		}
		if len(sm.names) != 1 || sm.names[0] != "foo" {
			t.Errorf("names = %#v", sm.names)
		}
	})

	t.Run("invalid JSON yields nil", func(t *testing.T) {
		if sm := parseSourceMap([]byte("not json")); sm != nil {
			t.Errorf("want nil, got %#v", sm)
		}
	})
}

// ---- parsedSourceMap.lookup ----------------------------------------------------------------------

func TestParsedSourceMapLookup(t *testing.T) {
	sm := parseSourceMap([]byte(testSourceMapJSON))
	if sm == nil {
		t.Fatal("setup: parseSourceMap failed")
	}

	t.Run("exact match", func(t *testing.T) {
		src, line, col, name, ok := sm.lookup(0, 0)
		if !ok || src != "app.js" || line != 0 || col != 0 || name != "foo" {
			t.Errorf("lookup = (%q,%d,%d,%q,%v)", src, line, col, name, ok)
		}
	})

	t.Run("out-of-range line fails", func(t *testing.T) {
		_, _, _, _, ok := sm.lookup(99, 0)
		if ok {
			t.Error("want ok=false for out-of-range line")
		}
	})

	t.Run("column before first token fails", func(t *testing.T) {
		// Build a map with a segment at genCol=5; querying col=0 should fail (bisect_right==0).
		lines := parseMappings("KAAA") // 'K' = 5 -> genCol 5
		psm := &parsedSourceMap{sources: []string{"a.js"}, names: nil, lines: lines}
		_, _, _, _, ok := psm.lookup(0, 0)
		if ok {
			t.Error("want ok=false when column precedes every token on the line")
		}
	})

	t.Run("column past every token picks the last one", func(t *testing.T) {
		psm := &parsedSourceMap{sources: []string{"a.js"}, names: nil, lines: parseMappings("AAAA")}
		src, _, _, _, ok := psm.lookup(0, 999)
		if !ok || src != "a.js" {
			t.Errorf("lookup = (%q,ok=%v), want success falling back to last token", src, ok)
		}
	})

	t.Run("1-value segment yields ok=true with empty src/name", func(t *testing.T) {
		psm := &parsedSourceMap{sources: []string{"a.js"}, names: nil, lines: parseMappings("A")}
		src, line, col, name, ok := psm.lookup(0, 0)
		if !ok || src != "" || line != 0 || col != 0 || name != "" {
			t.Errorf("lookup = (%q,%d,%d,%q,%v), want empty src/name but ok=true", src, line, col, name, ok)
		}
	})

	t.Run("srcIdx out of range yields empty src", func(t *testing.T) {
		// srcIdx=0 but sources is empty -> src stays "".
		psm := &parsedSourceMap{sources: nil, names: nil, lines: parseMappings("AAAA")}
		src, _, _, _, ok := psm.lookup(0, 0)
		if !ok || src != "" {
			t.Errorf("lookup = (%q, ok=%v), want empty src, ok=true", src, ok)
		}
	})
}

// ---- safeMapPath -----------------------------------------------------------------------------

func TestSafeMapPath(t *testing.T) {
	dir := t.TempDir()
	safeDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("relative path stays within safeDir", func(t *testing.T) {
		p, ok := safeMapPath(safeDir, "app.js.map")
		if !ok || p != filepath.Join(safeDir, "app.js.map") {
			t.Errorf("safeMapPath = (%q,%v)", p, ok)
		}
	})

	t.Run("path traversal is rejected", func(t *testing.T) {
		_, ok := safeMapPath(safeDir, "../../etc/passwd")
		if ok {
			t.Error("want traversal rejected")
		}
	})

	t.Run("resolves to exactly safeDir itself", func(t *testing.T) {
		p, ok := safeMapPath(safeDir, ".")
		if !ok || p != safeDir {
			t.Errorf("safeMapPath('.') = (%q,%v), want (%q,true)", p, ok, safeDir)
		}
	})
}

// ---- lookupForFile ---------------------------------------------------------------------------

func TestLookupForFile(t *testing.T) {
	t.Run("disabled sourceMapper fails fast", func(t *testing.T) {
		sm := &sourceMapper{enable: false}
		_, _, _, _, ok := sm.lookupForFile("https://example.com/app.js", 1, 1)
		if ok {
			t.Error("want ok=false when disabled")
		}
	})

	t.Run("dir does not exist fails", func(t *testing.T) {
		sm := &sourceMapper{enable: true, dir: "/no/such/dir/at/all"}
		_, _, _, _, ok := sm.lookupForFile("https://example.com/app.js", 1, 1)
		if ok {
			t.Error("want ok=false when dir missing")
		}
	})

	t.Run("successful lookup via basename .map file", func(t *testing.T) {
		dir := t.TempDir()
		writeMapFile(t, dir, "app.js.map", testSourceMapJSON)
		sm := &sourceMapper{enable: true, dir: dir, cache: map[string]cachedSourceMap{}}
		src, line, col, name, ok := sm.lookupForFile("https://cdn.example.com/static/app.js", 1, 1)
		if !ok || src != "app.js" || line != 1 || col != 1 || name != "foo" {
			t.Errorf("lookupForFile = (%q,%d,%d,%q,%v)", src, line, col, name, ok)
		}
	})

	t.Run("no matching map file fails", func(t *testing.T) {
		dir := t.TempDir()
		sm := &sourceMapper{enable: true, dir: dir, cache: map[string]cachedSourceMap{}}
		_, _, _, _, ok := sm.lookupForFile("https://cdn.example.com/static/missing.js", 1, 1)
		if ok {
			t.Error("want ok=false for missing map file")
		}
	})

	t.Run(".min.js suffix maps to .js.map", func(t *testing.T) {
		dir := t.TempDir()
		writeMapFile(t, dir, "app.js.map", testSourceMapJSON)
		sm := &sourceMapper{enable: true, dir: dir, cache: map[string]cachedSourceMap{}}
		src, _, _, _, ok := sm.lookupForFile("https://cdn.example.com/static/app.min.js", 1, 1)
		if !ok || src != "app.js" {
			t.Errorf("lookupForFile(.min.js) = (%q,%v)", src, ok)
		}
	})
}

// ---- load (mtime cache) ------------------------------------------------------------------------

func TestSourceMapperLoad(t *testing.T) {
	dir := t.TempDir()
	mapPath := filepath.Join(dir, "app.js.map")
	writeMapFile(t, dir, "app.js.map", testSourceMapJSON)
	sm := &sourceMapper{enable: true, dir: dir, cache: map[string]cachedSourceMap{}}

	first := sm.load(mapPath)
	if first == nil {
		t.Fatal("want parsed source map")
	}
	second := sm.load(mapPath)
	if second == nil {
		t.Fatal("want cached parsed source map")
	}

	t.Run("nonexistent file returns nil", func(t *testing.T) {
		if got := sm.load(filepath.Join(dir, "missing.js.map")); got != nil {
			t.Errorf("want nil, got %#v", got)
		}
	})

	t.Run("malformed map file returns nil without caching", func(t *testing.T) {
		badPath := filepath.Join(dir, "bad.js.map")
		writeMapFile(t, dir, "bad.js.map", "not json")
		if got := sm.load(badPath); got != nil {
			t.Errorf("want nil for malformed map, got %#v", got)
		}
	})
}

// ---- demangleStack ---------------------------------------------------------------------------

func TestDemangleStack(t *testing.T) {
	t.Run("nil/disabled/empty short-circuits", func(t *testing.T) {
		var nilSM *sourceMapper
		if got := nilSM.demangleStack("some text"); got != "some text" {
			t.Errorf("nil receiver: got %q", got)
		}
		disabled := &sourceMapper{enable: false}
		if got := disabled.demangleStack("some text"); got != "some text" {
			t.Errorf("disabled: got %q", got)
		}
		enabled := &sourceMapper{enable: true}
		if got := enabled.demangleStack(""); got != "" {
			t.Errorf("empty text: got %q", got)
		}
	})

	t.Run("successful remap with mapped source", func(t *testing.T) {
		dir := t.TempDir()
		writeMapFile(t, dir, "app.js.map", testSourceMapJSON)
		sm := &sourceMapper{enable: true, dir: dir, cache: map[string]cachedSourceMap{}}
		in := "at foo (https://cdn.example.com/static/app.js:1:2)"
		out := sm.demangleStack(in)
		if out == in {
			t.Errorf("expected remap to change the line, got unchanged: %q", out)
		}
	})

	t.Run("no frame match leaves line untouched", func(t *testing.T) {
		sm := &sourceMapper{enable: true, dir: t.TempDir(), cache: map[string]cachedSourceMap{}}
		in := "plain text with no stack frame"
		if got := sm.demangleStack(in); got != in {
			t.Errorf("got %q, want unchanged %q", got, in)
		}
	})

	t.Run("multi-line text preserves newline joins", func(t *testing.T) {
		sm := &sourceMapper{enable: true, dir: t.TempDir(), cache: map[string]cachedSourceMap{}}
		in := "line one\r\nline two\nline three"
		got := sm.demangleStack(in)
		want := "line one\nline two\nline three"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// ---- remapRumConsoleStacksObj -------------------------------------------------------------------

func TestRemapRumConsoleStacksObj(t *testing.T) {
	t.Run("nil/disabled/nil-event short-circuits without panicking", func(t *testing.T) {
		var nilSM *sourceMapper
		nilSM.remapRumConsoleStacksObj(jsonenc.NewObject())
		disabled := &sourceMapper{enable: false}
		disabled.remapRumConsoleStacksObj(jsonenc.NewObject())
		enabled := &sourceMapper{enable: true}
		enabled.remapRumConsoleStacksObj(nil)
	})

	t.Run("missing breadcrumbs/console is a no-op", func(t *testing.T) {
		sm := &sourceMapper{enable: true, dir: t.TempDir(), cache: map[string]cachedSourceMap{}}
		event := jsonenc.NewObject()
		sm.remapRumConsoleStacksObj(event) // no breadcrumbs key at all
		event2 := jsonenc.NewObject().Set("breadcrumbs", jsonenc.NewObject())
		sm.remapRumConsoleStacksObj(event2) // no console key
	})

	t.Run("remaps stack entries in place", func(t *testing.T) {
		dir := t.TempDir()
		writeMapFile(t, dir, "app.js.map", testSourceMapJSON)
		sm := &sourceMapper{enable: true, dir: dir, cache: map[string]cachedSourceMap{}}
		entry := jsonenc.NewObject().Set("stack", "at foo (https://cdn.example.com/static/app.js:1:2)")
		console := []any{entry, "not-an-object"} // non-object entries must be skipped safely
		bc := jsonenc.NewObject().Set("console", console)
		event := jsonenc.NewObject().Set("breadcrumbs", bc)
		sm.remapRumConsoleStacksObj(event)
		stackV, _ := entry.Get("stack")
		stack, _ := stackV.(string)
		if stack == "" || stack == "at foo (https://cdn.example.com/static/app.js:1:2)" {
			t.Errorf("stack not remapped: %q", stack)
		}
	})
}
