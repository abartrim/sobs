package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Pure helpers in the AI build/chat path — corpus-unreachable (the LLM upstream can't be driven
// deterministically). Oracles: _tool_status_label (app.py:4005), _AI_ASSISTANT_META_RE
// (app.py:3455), the ```-fence strip, CPython json "Expecting value" error text, and
// _build_fallback_custom_option_json.

func TestStripCodeFences(t *testing.T) {
	cases := []struct{ in, want string }{
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n[1,2]\n```", "[1,2]"},
		{"```python\nx\n```", "x"},
		{"  no fences here  ", "no fences here"}, // trimmed, unchanged otherwise
	}
	for _, c := range cases {
		if got := stripCodeFences(c.in); got != c.want {
			t.Errorf("stripCodeFences(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPyExpectingValueError(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "Expecting value: line 1 column 1 (char 0)"},
		{"abc", "Expecting value: line 1 column 1 (char 0)"},
		{"  x", "Expecting value: line 1 column 3 (char 2)"},
		{"\n y", "Expecting value: line 2 column 2 (char 2)"},
	}
	for _, c := range cases {
		if got := pyExpectingValueError(c.in); got != c.want {
			t.Errorf("pyExpectingValueError(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestChartSpecParseError(t *testing.T) {
	if got := chartSpecParseError(""); got != "empty chart spec" {
		t.Errorf("empty: got %q", got)
	}
	if got := chartSpecParseError("   "); got != "empty chart spec" {
		t.Errorf("whitespace: got %q", got)
	}
	// Non-empty, non-JSON delegates to the CPython "Expecting value" wording.
	if got := chartSpecParseError("garbage"); got != "Expecting value: line 1 column 1 (char 0)" {
		t.Errorf("garbage: got %q", got)
	}
}

func TestBuildFallbackCustomOptionJSON(t *testing.T) {
	out := buildFallbackCustomOptionJSON()
	var parsed any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	for _, want := range []string{`"backgroundColor"`, "transparent", `"{{points}}"`, `"Value"`, `"line"`} {
		if !strings.Contains(out, want) {
			t.Errorf("fallback option JSON missing %s:\n%s", want, out)
		}
	}
}

func TestObjGetOr(t *testing.T) {
	o := jsonenc.NewObject().Set("k", "v")
	if got := objGetOr(o, "k", "def"); got != "v" {
		t.Errorf("present key: got %v, want v", got)
	}
	if got := objGetOr(o, "missing", "def"); got != "def" {
		t.Errorf("missing key: got %v, want def", got)
	}
}

func TestExtractAssistantMetaText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"no meta", "just the answer", "just the answer"},
		{"full block removed", "Answer.<assistant_meta>secret</assistant_meta> More", "Answer. More"},
		{"dangling open cut", "Real answer<assistant_meta foo=1 bar", "Real answer"},
	}
	for _, c := range cases {
		if got := extractAssistantMetaText(c.in); got != c.want {
			t.Errorf("%s: extractAssistantMetaText(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestToolStatusLabel(t *testing.T) {
	cases := []struct {
		status  string
		confirm bool
		want    string
	}{
		{"executed", false, "Executed"},
		{"EXECUTED", true, "Executed"}, // case/whitespace normalized
		{"unsupported", true, "Not available in this page action manifest"},
		{"proposed", true, "Awaiting confirmation"},
		{"proposed", false, "Queued"},
		{"", false, "Queued"},
	}
	for _, c := range cases {
		if got := toolStatusLabel(c.status, c.confirm); got != c.want {
			t.Errorf("toolStatusLabel(%q, %v) = %q, want %q", c.status, c.confirm, got, c.want)
		}
	}
}

func TestTruthyStr(t *testing.T) {
	for _, s := range []string{"1", "true", "TRUE", "yes", "on", "  On  "} {
		if !truthyStr(s) {
			t.Errorf("truthyStr(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"0", "false", "no", "off", "", "maybe"} {
		if truthyStr(s) {
			t.Errorf("truthyStr(%q) = true, want false", s)
		}
	}
}
