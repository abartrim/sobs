package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCheckDLPEndpoint covers _check_dlp_endpoint parity: the verdict precedence
// (flagged/pii_detected/blocked, detail/reason fallback) and the fail-open behavior. The DLP POST
// is served from the same URL-keyed upstream fixtures the parity harness uses, so this exercises
// the real upstreamRequest -> upstreamFixture -> parse path rather than a stubbed transport.
func TestCheckDLPEndpoint(t *testing.T) {
	s := &server{}

	// Empty URL short-circuits to "skipped" without any HTTP call.
	if clean, detail := s.checkDLPEndpoint("", "anything", ""); !clean || detail != "skipped" {
		t.Fatalf(`empty url = (%v,%q), want (true,"skipped")`, clean, detail)
	}

	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)

	const base = "http://sobs-ai.mock/dlp/v1"
	writeFixture := func(t *testing.T, url, bodyJSON string) {
		t.Helper()
		stem := upstreamFixtureKey("POST", url)
		spec := `{"status": 200, "json": ` + bodyJSON + `}`
		if err := os.WriteFile(filepath.Join(dir, stem+".json"), []byte(spec), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name      string
		url       string
		body      string // nil-marked by empty => no fixture written (404 -> fail open)
		write     bool
		wantClean bool
		wantDet   string
	}{
		{"clean explicit false", base + "/a", `{"flagged": false}`, true, true, "clean"},
		{"clean empty body", base + "/b", `{}`, true, true, "clean"},
		{"flagged with detail", base + "/c", `{"flagged": true, "detail": "SSN detected"}`, true, false, "SSN detected"},
		{"pii_detected no detail", base + "/d", `{"pii_detected": true}`, true, false, "flagged"},
		{"blocked uses reason", base + "/e", `{"blocked": true, "reason": "policy violation"}`, true, false, "policy violation"},
		{"detail wins over reason", base + "/f", `{"flagged": true, "detail": "D", "reason": "R"}`, true, false, "D"},
		{"empty detail falls to reason", base + "/g", `{"flagged": true, "detail": "", "reason": "R"}`, true, false, "R"},
		{"clean with detail string", base + "/h", `{"flagged": false, "detail": "verified clean"}`, true, true, "verified clean"},
		{"missing fixture fails open", base + "/z", "", false, true, "dlp_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.write {
				writeFixture(t, tc.url, tc.body)
			}
			clean, detail := s.checkDLPEndpoint(tc.url, "issue text", "")
			if clean != tc.wantClean || detail != tc.wantDet {
				t.Errorf("checkDLPEndpoint(%q) = (%v,%q), want (%v,%q)", tc.url, clean, detail, tc.wantClean, tc.wantDet)
			}
		})
	}
}
