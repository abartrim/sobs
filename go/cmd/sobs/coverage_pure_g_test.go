package main

// coverage_pure_g_test.go — oracle-anchored unit tests for SLICE G pure helpers
// (encoding / data / misc that the byte-parity corpus does not reach).
//
// Target functions and their disposition:
//   TESTED:
//     anySlice                    (handlers_misc.go:895)   — chdb Array cell normalizer (no
//                                   single app.py line; mirrors the "FORMAT JSON yields []any,
//                                   else json.loads of an array string" coercion used throughout
//                                   app.py array-column handling)
//     jsonDumpsIndent2            (handlers_misc.go:912)   app.py:10565,15747,15762
//                                   (json.dumps(obj, ensure_ascii=False, indent=2))
//     runHealthcheck              (main.go:112)            — failure path only (no server up →
//                                   connection refused → returns 1); the success path needs a
//                                   live /health server (see SKIPPED note for the 200 branch)
//     parseGithubActionsSnapshotZip (fix_cve_helpers.go:190) app.py:16577-16624
//                                   (_github_actions_dependency_rows zip loop)
//
//   SKIPPED (already covered / live IO / not unit-testable):
//     jsonDumpsNoEscBytes  — already covered by remaining_pure_helpers_test.go
//     exactMethodGuard     — already covered by remaining_pure_helpers_test.go
//     sendSMTPMessage      — live SMTP: smtp.Dial(addr) is the first statement, no pure
//                            message-building portion to isolate. Mirrors how
//                            coverage_pure_a_test.go skips _dispatch_email_channel
//                            ("server method making SMTP connections; not unit-testable").
//                            (The success path is exercised only by an integration test with a
//                            real/mock SMTP server.)
//     runHealthcheck 200-branch — needs a live HTTP server answering /health with 200; only the
//                            connection-refused failure path is determinable in a unit test.

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---------------------------------------------------------------------------
// anySlice — handlers_misc.go:895
// Oracle: normalize a chdb cell that should be an Array into []any. FORMAT JSON
// yields a []any directly; a JSON-array string (defensive fallback) is parsed;
// anything else → nil.
// ---------------------------------------------------------------------------

func TestSliceG_anySlice(t *testing.T) {
	cases := []struct {
		desc string
		in   any
		want []any
	}{
		{"already []any", []any{"a", float64(1), true}, []any{"a", float64(1), true}},
		{"empty []any", []any{}, []any{}},
		{"json array string", `["x", 2, null]`, []any{"x", float64(2), nil}},
		{"json empty array string", `[]`, []any{}},
		{"json object string → not an array → nil", `{"a":1}`, nil},
		{"json scalar string → nil", `42`, nil},
		{"plain (non-json) string → nil", "hello", nil},
		{"empty string → nil", "", nil},
		{"nil → nil", nil, nil},
		{"int → nil", 7, nil},
		{"map → nil", map[string]any{"a": 1}, nil},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := anySlice(c.in)
			if c.want == nil {
				if got != nil {
					t.Fatalf("anySlice(%#v) = %#v, want nil", c.in, got)
				}
				return
			}
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(c.want)
			if string(gb) != string(wb) {
				t.Errorf("anySlice(%#v) = %s, want %s", c.in, gb, wb)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// jsonDumpsIndent2 — handlers_misc.go:912
// Oracle: json.dumps(obj, ensure_ascii=False, indent=2) — two-space indent,
// "," item separator (no trailing space), ": " key separator, NO HTML-escaping
// of <>&, non-ASCII left literal, insertion-order keys.
// Oracle reference outputs captured from CPython (app.py:10565,15747,15762).
// ---------------------------------------------------------------------------

func TestSliceG_jsonDumpsIndent2(t *testing.T) {
	cases := []struct {
		desc string
		in   any
		want string
	}{
		// CPython: json.dumps({"a":1,"b":[1,2],"c":"x<y>&z"}, ensure_ascii=False, indent=2)
		{
			"nested object/array + raw <>&",
			jsonenc.NewObject().
				Set("a", json.Number("1")).
				Set("b", []any{json.Number("1"), json.Number("2")}).
				Set("c", "x<y>&z"),
			"{\n  \"a\": 1,\n  \"b\": [\n    1,\n    2\n  ],\n  \"c\": \"x<y>&z\"\n}",
		},
		// CPython: json.dumps({"k":"héllo→"}, ensure_ascii=False, indent=2)
		// ensure_ascii=False keeps non-ASCII literal.
		{
			"non-ascii left literal",
			jsonenc.NewObject().Set("k", "héllo→"),
			"{\n  \"k\": \"héllo→\"\n}",
		},
		// Empty object: CPython json.dumps({}, indent=2) → "{}"
		{"empty object", jsonenc.NewObject(), "{}"},
		// Empty array: CPython json.dumps([], indent=2) → "[]"
		{"empty array", []any{}, "[]"},
		// Scalar string at top level.
		{"top-level string", "hi", `"hi"`},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got, err := jsonDumpsIndent2(c.in)
			if err != nil {
				t.Fatalf("jsonDumpsIndent2(%#v) error: %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("jsonDumpsIndent2 mismatch\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// runHealthcheck — main.go:112
// Oracle: probe local /health; 200 → 0 else 1. We can only deterministically
// exercise the FAILURE path (no server up → connection refused → 1) and the
// non-200 path (a stub server returning 500 → 1). The genuine 200 path needs a
// live SOBS server, so it stays an integration concern.
// ---------------------------------------------------------------------------

func TestSliceG_runHealthcheck_ConnRefused(t *testing.T) {
	// Point SOBS_PORT at a port with (almost certainly) nothing listening so the
	// GET fails fast with connection-refused → returns 1. We pick a high port and
	// restore the env afterwards.
	orig, had := os.LookupEnv("SOBS_PORT")
	origP, hadP := os.LookupEnv("PORT")
	defer func() {
		if had {
			os.Setenv("SOBS_PORT", orig)
		} else {
			os.Unsetenv("SOBS_PORT")
		}
		if hadP {
			os.Setenv("PORT", origP)
		} else {
			os.Unsetenv("PORT")
		}
	}()
	os.Unsetenv("PORT")
	os.Setenv("SOBS_PORT", "1") // privileged/unused → dial fails immediately

	if rc := runHealthcheck(); rc != 1 {
		t.Errorf("runHealthcheck() with no server = %d, want 1", rc)
	}
}

func TestSliceG_runHealthcheck_Non200(t *testing.T) {
	// A stub server that answers /health with 500 must yield exit code 1.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	// srv.Listener.Addr() is 127.0.0.1:<port>; runHealthcheck dials 127.0.0.1:<SOBS_PORT>.
	port := srv.Listener.Addr().(interface{ String() string }).String()
	// Extract the port number after the last ':'.
	colon := -1
	for i := len(port) - 1; i >= 0; i-- {
		if port[i] == ':' {
			colon = i
			break
		}
	}
	if colon < 0 {
		t.Fatalf("unexpected listener addr %q", port)
	}
	portNum := port[colon+1:]

	orig, had := os.LookupEnv("SOBS_PORT")
	origP, hadP := os.LookupEnv("PORT")
	defer func() {
		if had {
			os.Setenv("SOBS_PORT", orig)
		} else {
			os.Unsetenv("SOBS_PORT")
		}
		if hadP {
			os.Setenv("PORT", origP)
		} else {
			os.Unsetenv("PORT")
		}
	}()
	os.Unsetenv("PORT")
	os.Setenv("SOBS_PORT", portNum)

	if rc := runHealthcheck(); rc != 1 {
		t.Errorf("runHealthcheck() against 500 server = %d, want 1", rc)
	}
}

func TestSliceG_runHealthcheck_200(t *testing.T) {
	// The success path: a stub server answering 200 on /health → exit code 0.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	colon := -1
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			colon = i
			break
		}
	}
	portNum := addr[colon+1:]

	orig, had := os.LookupEnv("SOBS_PORT")
	origP, hadP := os.LookupEnv("PORT")
	defer func() {
		if had {
			os.Setenv("SOBS_PORT", orig)
		} else {
			os.Unsetenv("SOBS_PORT")
		}
		if hadP {
			os.Setenv("PORT", origP)
		} else {
			os.Unsetenv("PORT")
		}
	}()
	os.Unsetenv("PORT")
	os.Setenv("SOBS_PORT", portNum)

	if rc := runHealthcheck(); rc != 0 {
		t.Errorf("runHealthcheck() against 200 /health server = %d, want 0", rc)
	}
}

// ---------------------------------------------------------------------------
// parseGithubActionsSnapshotZip — fix_cve_helpers.go:190
// Oracle: app.py:16577-16624 — for each non-dir zip entry whose basename matches
// pip-freeze-<platform>-<arch>.txt, parse requirements into PyPI deps and emit a
// dependencies-lockfile artifact row. Entries that don't match, or whose parsed
// deps are empty, are skipped. A corrupt archive yields no rows.
// We assert the STABLE row fields (Id/UploadedAt/Version are non-deterministic).
// ---------------------------------------------------------------------------

func makeZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestSliceG_parseGithubActionsSnapshotZip_Match(t *testing.T) {
	// One matching entry with two pinned deps; one non-matching entry (ignored).
	reqs := "# comment\nflask==3.0.0\nrequests==2.31.0  # inline note\nnotpinned\n"
	archive := makeZip(t, map[string]string{
		"pip-freeze-linux-amd64.txt": reqs,
		"README.md":                  "ignore me",
	})

	rows := parseGithubActionsSnapshotZip(
		archive, "octo", "repo", "run123", "art456", "rel789", "1.2.3", "deadbeef", "snapshot-artifact")

	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	r := rows[0]

	// Stable scalar fields anchored to app.py:16591-16620.
	stable := map[string]any{
		"ReleaseId":      "rel789",
		"ArtifactType":   "dependencies-lockfile",
		"Name":           "pip-freeze-linux-amd64",
		"ContentType":    "text/plain",
		"Size":           len(reqs),
		"Platform":       "linux",
		"Architecture":   "amd64",
		"IsDeleted":      0,
		"StorageRef":     "github-actions://octo/repo/runs/run123/artifacts/art456/pip-freeze-linux-amd64.txt",
		"ChecksumSha256": sha256Sum([]byte(reqs)),
	}
	for k, want := range stable {
		if got := r[k]; got != want {
			t.Errorf("row[%q] = %#v, want %#v", k, got, want)
		}
	}

	// MetadataJson mirrors json.dumps({...}, separators=(",", ":")) with the deps
	// in file order (app.py:16606-16617). Build the same object and compare bytes.
	wantMeta := jsonenc.NewObject().
		Set("source", "github_actions_artifact").
		Set("repo", "octo/repo").
		Set("run_id", "run123").
		Set("run_head_sha", "deadbeef").
		Set("release_version", "1.2.3").
		Set("artifact_name", "snapshot-artifact").
		Set("dependencies", []any{
			depObj("flask", "3.0.0", "PyPI"),
			depObj("requests", "2.31.0", "PyPI"),
		})
	wantMetaJSON := string(jsonenc.Encode(wantMeta, cveCompactDumpsOpts))
	if got := r["MetadataJson"]; got != wantMetaJSON {
		t.Errorf("MetadataJson mismatch\n got: %v\nwant: %v", got, wantMetaJSON)
	}

	// Non-deterministic fields must still be present + typed.
	if id, _ := r["Id"].(string); id == "" {
		t.Errorf("Id should be a non-empty uuid string, got %#v", r["Id"])
	}
	if _, ok := r["UploadedAt"].(string); !ok {
		t.Errorf("UploadedAt should be a string timestamp, got %#v", r["UploadedAt"])
	}
}

func TestSliceG_parseGithubActionsSnapshotZip_NoMatchOrEmpty(t *testing.T) {
	// Corrupt archive → zip.NewReader fails → nil (app.py broad except → []).
	if rows := parseGithubActionsSnapshotZip([]byte("not a zip"), "o", "r", "i", "a", "rel", "v", "sha", "n"); len(rows) != 0 {
		t.Errorf("corrupt archive: expected 0 rows, got %d", len(rows))
	}

	// A matching name but no pinned (==) deps → parseRequirementsDeps empty → skipped.
	archive := makeZip(t, map[string]string{
		"pip-freeze-darwin-arm64.txt": "# only comments\nunpinnedpkg\n",
		"nested/dir/notmatching.txt":  "flask==1.0.0\n",
	})
	rows := parseGithubActionsSnapshotZip(archive, "o", "r", "i", "a", "rel", "v", "sha", "n")
	if len(rows) != 0 {
		t.Errorf("no-deps + non-matching name: expected 0 rows, got %d", len(rows))
	}

	// Empty zip (no files) → no rows.
	empty := makeZip(t, map[string]string{})
	if rows := parseGithubActionsSnapshotZip(empty, "o", "r", "i", "a", "rel", "v", "sha", "n"); len(rows) != 0 {
		t.Errorf("empty zip: expected 0 rows, got %d", len(rows))
	}
}
