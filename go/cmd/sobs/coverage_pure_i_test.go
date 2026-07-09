package main

// coverage_pure_i_test.go — oracle-anchored unit tests for SLICE I, the final cluster of
// store-free, corpus-0%, previously-untested helpers in the SOBS Python→Go migration.
//
// Target functions and their disposition:
//   TESTED (package main, in this file):
//     parseFloatDefault       (agent_flow.go:505)        app.py: float() parse-or-default idiom
//     mustAbs                 (chdb_encryption.go:163)    app.py:1880-1881 (os.path.isabs guard)
//     rawFlateReader          (otlp_decompress.go:48)     app.py:9596 (zlib.decompressobj(-MAX_WBITS))
//     notifViewRedirect       (fix_forms.go:15)           app.py:26002-26006 create_notification_rule
//                                                          (url_for("view_notifications", edit_rule=...))
//     remapRumConsoleStacks   (source_map.go:87)          app.py:8064-8076 _remap_rum_console_stacks
//     dbError                 (dbutil.go:359)             app.py:17941 jsonify({"ok":False,"error":str(exc)}),500
//     formRequire             (handlers_forms.go:244)     app.py: await flash(msg,cat); return redirect(loc)
//     ColumnIndex             (internal/store/params.go:84) — store.Result method, exercised here
//                                                          through the exported type (coverage attributes
//                                                          to internal/store, not cmd/sobs).
//
//   SKIPPED (needs a real chDB store / not unit-testable):
//     (none in this slice — every target is store-free and exercised directly)
//
// All tests are table-driven where there is more than one representative input. Each receiver is
// constructed inline as a minimal &server{cfg: config{...}} (none touch s.db) or the value type
// directly. No store is opened.

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
)

// ---------------------------------------------------------------------------
// parseFloatDefault — agent_flow.go:505
// Oracle: app.py's recurring `float(raw)`-or-default idiom (try float(...) except: default),
// with a leading/trailing whitespace strip and an empty-string short-circuit to the default.
// ---------------------------------------------------------------------------

func TestSliceI_parseFloatDefault(t *testing.T) {
	cases := []struct {
		raw  string
		def  float64
		want float64
		desc string
	}{
		{"", 1.5, 1.5, "empty -> default"},
		{"   ", 2.0, 2.0, "all-whitespace -> default (TrimSpace)"},
		{"3.14", 0, 3.14, "plain float"},
		{"  3.14  ", 0, 3.14, "surrounding whitespace trimmed"},
		{"42", 0, 42, "integer literal parses as float"},
		{"-7.5", 0, -7.5, "negative"},
		{"1e3", 0, 1000, "scientific notation (Python float() accepts)"},
		// PARITY (not a divergence): Go strconv.ParseFloat("inf") == Python float("inf") == +Inf,
		// so both bypass the default. Asserted via math.Inf(1) below in a dedicated check.
		{"notanumber", 9.9, 9.9, "unparseable -> default"},
		{"3.14x", 7, 7, "trailing garbage -> default"},
		{"0", 5, 0, "zero is a valid parse, not the default"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := parseFloatDefault(c.raw, c.def)
			if got != c.want {
				t.Fatalf("parseFloatDefault(%q, %v) = %v; want %v", c.raw, c.def, got, c.want)
			}
		})
	}

	// Inf/NaN parity: Go strconv.ParseFloat parses "inf"/"-inf"/"infinity"/"nan" exactly as
	// CPython's float() does, so the default is bypassed (verified against python3 float()).
	t.Run("inf parses to +Inf (matches Python float('inf'))", func(t *testing.T) {
		if got := parseFloatDefault("inf", 0); got != math.Inf(1) {
			t.Fatalf("parseFloatDefault(\"inf\") = %v; want +Inf", got)
		}
		if got := parseFloatDefault("-inf", 0); got != math.Inf(-1) {
			t.Fatalf("parseFloatDefault(\"-inf\") = %v; want -Inf", got)
		}
	})
	t.Run("nan parses to NaN (matches Python float('nan'))", func(t *testing.T) {
		if got := parseFloatDefault("nan", 0); !math.IsNaN(got) {
			t.Fatalf("parseFloatDefault(\"nan\") = %v; want NaN", got)
		}
	})
}

// ---------------------------------------------------------------------------
// mustAbs — chdb_encryption.go:163
// Oracle: app.py:1880-1881 — `if not os.path.isabs(config_file): raise RuntimeError(...)`.
// Returns (p, nil) when absolute, else ("", error) whose message names the env var.
// ---------------------------------------------------------------------------

func TestSliceI_mustAbs(t *testing.T) {
	cases := []struct {
		p       string
		varName string
		wantOK  bool
		desc    string
	}{
		{"/etc/clickhouse/config.xml", "CHDB_CONFIG_FILE", true, "absolute path accepted"},
		{"/", "CHDB_DATA_PATH", true, "root is absolute"},
		{"relative/path.xml", "CHDB_CONFIG_FILE", false, "relative path rejected"},
		{"./config.xml", "CHDB_CONFIG_FILE", false, "dot-relative rejected"},
		{"config.xml", "CHDB_CONFIG_FILE", false, "bare filename rejected"},
		{"", "CHDB_CONFIG_FILE", false, "empty rejected (not absolute)"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got, err := mustAbs(c.p, c.varName)
			if c.wantOK {
				if err != nil {
					t.Fatalf("mustAbs(%q) unexpected error: %v", c.p, err)
				}
				if got != c.p {
					t.Fatalf("mustAbs(%q) = %q; want the input unchanged", c.p, got)
				}
				return
			}
			if err == nil {
				t.Fatalf("mustAbs(%q) expected error, got nil (returned %q)", c.p, got)
			}
			if got != "" {
				t.Fatalf("mustAbs(%q) error path should return empty string, got %q", c.p, got)
			}
			// The error mentions the var name and the offending path (matches app.py's message shape).
			if !strings.Contains(err.Error(), c.varName) || !strings.Contains(err.Error(), c.p) {
				t.Fatalf("mustAbs error %q must name var %q and path %q", err.Error(), c.varName, c.p)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// rawFlateReader — otlp_decompress.go:48
// Oracle: app.py:9596 — the deflate fallback uses zlib.decompressobj(-zlib.MAX_WBITS), i.e. RAW
// DEFLATE with no zlib header/checksum. rawFlateReader wraps compress/flate.NewReader, which reads
// the same raw-DEFLATE stream. The gzip/zlib distinction: a zlib-wrapped stream has a 2-byte zlib
// header (0x78 0x9c…) that raw DEFLATE does NOT expect, so decoding a zlib stream through the raw
// reader yields corrupt/garbage output (it does not transparently strip the header).
// ---------------------------------------------------------------------------

func rawDeflate(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate.NewWriter: %v", err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

func zlibWrap(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

func TestSliceI_rawFlateReader_RoundTrips(t *testing.T) {
	payloads := [][]byte{
		[]byte(""),
		[]byte("hello world"),
		[]byte(`{"resourceSpans":[{"scopeSpans":[]}]}`),
		bytes.Repeat([]byte("AB"), 5000), // larger, compressible
	}
	for i, p := range payloads {
		raw := rawDeflate(t, p)
		r, err := rawFlateReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("case %d: rawFlateReader returned err: %v", i, err)
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("case %d: ReadAll: %v", i, err)
		}
		if !bytes.Equal(got, p) {
			t.Fatalf("case %d: raw-deflate round-trip = %q; want %q", i, got, p)
		}
	}
}

func TestSliceI_rawFlateReader_RejectsZlibWrapped(t *testing.T) {
	// A zlib-wrapped stream is NOT raw DEFLATE: the raw reader treats the zlib 2-byte header
	// as compressed data, so it cannot recover the original payload. This is exactly the
	// gzip/zlib-vs-raw distinction the decompress fallback relies on (zlib first, raw second).
	payload := []byte("the quick brown fox")
	zw := zlibWrap(t, payload)
	r, err := rawFlateReader(bytes.NewReader(zw))
	if err != nil {
		t.Fatalf("rawFlateReader constructor should not error: %v", err)
	}
	got, err := io.ReadAll(r)
	// Either a decode error, or successfully-read-but-wrong-bytes — both prove it did NOT
	// transparently decode the zlib stream as if it were raw DEFLATE.
	if err == nil && bytes.Equal(got, payload) {
		t.Fatalf("rawFlateReader unexpectedly decoded a zlib-wrapped stream to the original payload")
	}
}

// ---------------------------------------------------------------------------
// ColumnIndex — internal/store/params.go:84 (method on *store.Result)
// Pure column-name lookup: linear scan over Columns, exact (case-sensitive) match, -1 if missing.
// Exercised here via the exported store.Result type. NOTE: with -coverpkg=./cmd/sobs/... the
// coverage credit lands on internal/store, not cmd/sobs — that is a coverpkg-scope artifact, the
// behavior is fully verified.
// ---------------------------------------------------------------------------

func TestSliceI_ColumnIndex(t *testing.T) {
	res := &store.Result{Columns: []string{"Id", "Name", "Count"}}
	cases := []struct {
		name string
		want int
		desc string
	}{
		{"Id", 0, "first column"},
		{"Name", 1, "middle column"},
		{"Count", 2, "last column"},
		{"id", -1, "case-sensitive: lowercase miss"},
		{"NAME", -1, "case-sensitive: uppercase miss"},
		{"Missing", -1, "absent column -> -1"},
		{"", -1, "empty name -> -1"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := res.ColumnIndex(c.name); got != c.want {
				t.Fatalf("ColumnIndex(%q) = %d; want %d", c.name, got, c.want)
			}
		})
	}

	// Empty Columns: every lookup is a miss.
	empty := &store.Result{}
	if got := empty.ColumnIndex("Id"); got != -1 {
		t.Fatalf("ColumnIndex on empty Columns = %d; want -1", got)
	}

	// Duplicate column names: returns the FIRST match (linear scan semantics).
	dup := &store.Result{Columns: []string{"X", "Y", "X"}}
	if got := dup.ColumnIndex("X"); got != 0 {
		t.Fatalf("ColumnIndex on duplicate names = %d; want 0 (first match)", got)
	}
}

// ---------------------------------------------------------------------------
// notifViewRedirect — fix_forms.go:15
// Oracle: app.py:26002-26006 create_notification_rule — on the tag-regex/edit error path it does
//   redirect(url_for("view_notifications", edit_rule=edit_rule_id) if edit_rule_id
//            else url_for("view_notifications"))
// The "view_notifications" endpoint maps to "/settings/notifications" (app.py:25708-25710).
// The Go port prepends cfg.BasePath and Werkzeug-encodes the edit_rule query value, relaxing
// %3A back to ':' (matching url_for's query encoding for the realistic rule-id inputs).
// ---------------------------------------------------------------------------

func TestSliceI_notifViewRedirect(t *testing.T) {
	cases := []struct {
		basePath   string
		editRuleID string
		want       string
		desc       string
	}{
		{"", "", "/settings/notifications", "no edit -> bare path"},
		{"", "abc-123", "/settings/notifications?edit_rule=abc-123", "edit -> ?edit_rule="},
		{"/sobs", "", "/sobs/settings/notifications", "BasePath prefix, no edit"},
		{"/sobs", "r1", "/sobs/settings/notifications?edit_rule=r1", "BasePath prefix + edit"},
		// A rule id with a colon: url.QueryEscape would emit %3A, then the %3A->: relaxation
		// restores ':' — so a ':' passes through literally (matches Werkzeug url_for).
		{"", "ns:rule", "/settings/notifications?edit_rule=ns:rule", "colon relaxed back to ':'"},
		// A space must be percent-encoded (QueryEscape -> '+'); not in the relax set.
		{"", "a b", "/settings/notifications?edit_rule=a+b", "space -> '+'"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			s := &server{cfg: config{BasePath: c.basePath}}
			got := s.notifViewRedirect(httptest.NewRequest(http.MethodGet, "/", nil), c.editRuleID)
			if got != c.want {
				t.Fatalf("notifViewRedirect(base=%q, id=%q) = %q; want %q",
					c.basePath, c.editRuleID, got, c.want)
			}
			// Sanity: the query value round-trips through url.QueryUnescape to the original id.
			if c.editRuleID != "" {
				q := got[strings.Index(got, "edit_rule=")+len("edit_rule="):]
				if dec, err := url.QueryUnescape(strings.ReplaceAll(q, ":", "%3A")); err == nil {
					if dec != c.editRuleID {
						t.Fatalf("decoded query %q != original id %q", dec, c.editRuleID)
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// remapRumConsoleStacks — source_map.go:87
// Oracle: app.py:8064-8076 _remap_rum_console_stacks — walk event["breadcrumbs"]["console"] (a
// list of dicts) and, for any entry with a non-empty "stack", replace it with the demangled stack.
// When SOURCE_MAP_ENABLE is False, _maybe_demangle_js_stack returns the text unchanged, so the
// stacks are untouched. Non-dict breadcrumbs / non-list console / non-dict entries are skipped.
// ---------------------------------------------------------------------------

func TestSliceI_remapRumConsoleStacks_NoopWhenDisabled(t *testing.T) {
	// enable=false (the corpus default): every stack is left exactly as-is.
	sm := &sourceMapper{enable: false}
	event := map[string]any{
		"breadcrumbs": map[string]any{
			"console": []any{
				map[string]any{"stack": "Error\n    at https://x/app.js:10:5"},
				map[string]any{"stack": "other"},
			},
		},
	}
	sm.remapRumConsoleStacks(event)
	console := event["breadcrumbs"].(map[string]any)["console"].([]any)
	if got := console[0].(map[string]any)["stack"]; got != "Error\n    at https://x/app.js:10:5" {
		t.Fatalf("disabled mapper changed stack[0]: %q", got)
	}
	if got := console[1].(map[string]any)["stack"]; got != "other" {
		t.Fatalf("disabled mapper changed stack[1]: %q", got)
	}
}

func TestSliceI_remapRumConsoleStacks_NilAndMalformed(t *testing.T) {
	// nil receiver: must be a safe no-op (it is the first guard).
	var nilSM *sourceMapper
	nilSM.remapRumConsoleStacks(map[string]any{"breadcrumbs": map[string]any{"console": []any{}}})

	// enabled but malformed shapes -> all skipped without panic, event unchanged.
	sm := &sourceMapper{enable: true, dir: ""}
	cases := []map[string]any{
		{},                                // no breadcrumbs
		{"breadcrumbs": "not-a-dict"},     // breadcrumbs wrong type
		{"breadcrumbs": map[string]any{}}, // no console
		{"breadcrumbs": map[string]any{"console": "x"}},                                // console wrong type
		{"breadcrumbs": map[string]any{"console": []any{"not-a-dict", 42}}},            // non-dict entries
		{"breadcrumbs": map[string]any{"console": []any{map[string]any{}}}},            // entry without stack
		{"breadcrumbs": map[string]any{"console": []any{map[string]any{"stack": ""}}}}, // empty stack
	}
	for i, ev := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d panicked: %v", i, r)
				}
			}()
			sm.remapRumConsoleStacks(ev)
		}()
	}

	// enabled with a real stack but NO source-map dir: lookupForFile fails -> demangleStack
	// returns the line unchanged, so the stack is structurally untouched.
	sm2 := &sourceMapper{enable: true, dir: ""}
	ev := map[string]any{"breadcrumbs": map[string]any{"console": []any{
		map[string]any{"stack": "    at https://x/app.js:10:5"},
	}}}
	sm2.remapRumConsoleStacks(ev)
	got := ev["breadcrumbs"].(map[string]any)["console"].([]any)[0].(map[string]any)["stack"]
	if got != "    at https://x/app.js:10:5" {
		t.Fatalf("no source-map dir should leave stack unchanged, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// dbError — dbutil.go:359
// Oracle: app.py:17941 (and 18278, 18353, …) — `return jsonify({"ok": False, "error": str(exc)}), 500`.
// Writes a 500 with the compact-jsonify body {"error":<msg>,"ok":false}\n (QuartJSONify sorts keys
// and appends a trailing newline; "error" sorts before "ok").
// ---------------------------------------------------------------------------

func TestSliceI_dbError(t *testing.T) {
	cases := []struct {
		errMsg string
		desc   string
	}{
		{"connection refused", "plain message"},
		{`bad "quote" and \backslash`, "message needing JSON escaping"},
		{"", "empty error message"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			s := &server{}
			rec := httptest.NewRecorder()
			s.dbError(rec, errorString(c.errMsg))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("dbError status = %d; want 500", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("dbError Content-Type = %q; want application/json", ct)
			}
			// Build the byte-exact expected body via the same encoder the server uses, so we
			// pin the {ok:false,error:str(exc)} shape with QuartJSONify (sorted keys + \n).
			want := string(jsonenc.Encode(
				jsonenc.NewObject().Set("ok", false).Set("error", c.errMsg),
				jsonenc.QuartJSONify))
			if rec.Body.String() != want {
				t.Fatalf("dbError body = %q; want %q", rec.Body.String(), want)
			}
			// Keys are sorted: "error" precedes "ok".
			if !strings.HasPrefix(rec.Body.String(), `{"error":`) {
				t.Fatalf("dbError body should start with sorted key {\"error\": ; got %q", rec.Body.String())
			}
		})
	}
}

// errorString is a trivial error wrapper so dbError sees err.Error() == the message.
type errorString string

func (e errorString) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// formRequire — handlers_forms.go:244
// Oracle: app.py's required-field branch — `if not form.get(field, "").strip(): await flash(msg,
// category); return redirect(location); ` (e.g. the "… is required" flash-redirects). formRequire
// returns true (handled) and writes a 302 flash-redirect when the POST form lacks a non-empty
// field; returns false (and writes nothing) when present.
// ---------------------------------------------------------------------------

func postForm(field, value string) *http.Request {
	form := url.Values{}
	if value != "" || field != "" {
		form.Set(field, value)
	}
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func TestSliceI_formRequire_MissingFlashesAndRedirects(t *testing.T) {
	cases := []struct {
		value string
		desc  string
	}{
		{"", "field absent"},
		{"   ", "field whitespace-only (TrimSpace empties it)"},
		{"\t\n", "field tabs/newlines only"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			s := &server{}
			rec := httptest.NewRecorder()
			r := postForm("name", c.value)
			handled := s.formRequire(rec, r, "name", "warning", "Name is required", "/settings/agents")
			if !handled {
				t.Fatalf("formRequire should return true (handled) when field is blank")
			}
			if rec.Code != http.StatusFound {
				t.Fatalf("formRequire status = %d; want 302", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != "/settings/agents" {
				t.Fatalf("formRequire Location = %q; want /settings/agents", loc)
			}
			// The flash cookie carries the (category, message) tuple deterministically.
			wantCookie := flashSessionCookie("warning", "Name is required")
			if sc := rec.Header().Get("Set-Cookie"); sc != wantCookie {
				t.Fatalf("formRequire Set-Cookie = %q; want %q", sc, wantCookie)
			}
		})
	}
}

func TestSliceI_formRequire_PresentReturnsFalse(t *testing.T) {
	s := &server{}
	rec := httptest.NewRecorder()
	r := postForm("name", "My Rule")
	handled := s.formRequire(rec, r, "name", "warning", "Name is required", "/settings/agents")
	if handled {
		t.Fatalf("formRequire should return false (not handled) when field is present")
	}
	if rec.Code != http.StatusOK { // httptest default; nothing written
		t.Fatalf("formRequire wrote a status (%d) when field was present; want no write (200 default)", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("formRequire wrote a body (%q) when field was present", rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Fatalf("formRequire set a Location header when field was present")
	}
}
