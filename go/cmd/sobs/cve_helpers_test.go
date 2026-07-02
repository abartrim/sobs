package main

import (
	"errors"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// The CVE-scan pure helpers (ecosystem mapping, OSV response field extraction, disposition
// auto-expiry) never run on the corpus's empty-inventory fixture, which has nothing to scan or
// disposition. Oracle: app.py fragments in the CVE enrichment endpoints (cve_scan.go doc
// comments cite the mirrored helper names).

func TestLangToOSVEcosystem(t *testing.T) {
	cases := map[string]string{
		"Python": "PyPI", "javascript": "npm", "nodejs": "npm", "JAVA": "Maven",
		"go": "Go", "ruby": "RubyGems", "dotnet": "NuGet", "rust": "crates.io",
		"php": "Packagist", "dart": "Pub", "cobol": "",
	}
	for lang, want := range cases {
		if got := langToOSVEcosystem(lang); got != want {
			t.Errorf("langToOSVEcosystem(%q) = %q, want %q", lang, got, want)
		}
	}
}

func TestInventoryScopeEcosystem(t *testing.T) {
	cases := map[string]string{
		"io.opentelemetry.instrumentation":    "Maven",
		"com.example.lib":                     "Maven",
		"org.apache.commons":                  "Maven",
		"@opentelemetry/api":                  "npm",
		"opentelemetry-instrumentation-flask": "PyPI",
		"opentelemetry-util_http":             "", // underscore in the final segment blocks PyPI
		"unrelated-scope":                     "",
	}
	for scope, want := range cases {
		if got := inventoryScopeEcosystem(scope); got != want {
			t.Errorf("inventoryScopeEcosystem(%q) = %q, want %q", scope, got, want)
		}
	}
}

func TestCveServiceLabelAndSourcePriority(t *testing.T) {
	if got := cveServiceLabel(cveLib{service: "checkout", appName: "Checkout App"}); got != "checkout" {
		t.Fatalf("service wins over appName: got %q", got)
	}
	if got := cveServiceLabel(cveLib{appName: "Checkout App"}); got != "Checkout App" {
		t.Fatalf("blank service falls back to appName: got %q", got)
	}
	if got := cveSourcePriorityOr("release_registry"); got != 0 {
		t.Fatalf("known source: got %d", got)
	}
	if got := cveSourcePriorityOr("unknown_source"); got != 99 {
		t.Fatalf("unknown source: got %d, want 99", got)
	}
}

func TestCveMetadataDependencies(t *testing.T) {
	deps := cveMetadataDependencies(`{"dependencies":[{"name":"lodash","version":"4.0.0"},"not-an-object"]}`)
	if len(deps) != 1 {
		t.Fatalf("want 1 dependency (non-object dropped), got %d: %v", len(deps), deps)
	}
	if v, _ := deps[0].Get("name"); v != "lodash" {
		t.Fatalf("unexpected dependency: %v", deps[0])
	}
	if got := cveMetadataDependencies("{not json"); len(got) != 0 {
		t.Fatalf("invalid json: want empty, got %v", got)
	}
	if got := cveMetadataDependencies(`{"other":1}`); len(got) != 0 {
		t.Fatalf("no dependencies key: want empty, got %v", got)
	}
	if got := cveMetadataDependencies(`{"dependencies":"not-a-list"}`); len(got) != 0 {
		t.Fatalf("dependencies not a list: want empty, got %v", got)
	}
}

func TestCveAliasIDs(t *testing.T) {
	v := jsonenc.NewObject().Set("aliases", []any{"CVE-2024-1234", "GHSA-xxxx-yyyy", "CVE-2023-5678"})
	got := cveAliasIDs(v)
	if len(got) != 2 || got[0] != "CVE-2024-1234" || got[1] != "CVE-2023-5678" {
		t.Fatalf("unexpected aliases: %v", got)
	}
	if got := cveAliasIDs(jsonenc.NewObject()); len(got) != 0 {
		t.Fatalf("no aliases key: want empty, got %v", got)
	}
}

func TestOsvSeverity(t *testing.T) {
	withScore := jsonenc.NewObject().Set("severity", []any{jsonenc.NewObject().Set("score", "CVSS:7.5")})
	if got := osvSeverity(withScore); got != "CVSS:7.5" {
		t.Fatalf("score branch: got %q", got)
	}
	withType := jsonenc.NewObject().Set("severity", []any{jsonenc.NewObject().Set("type", "CVSS_V3")})
	if got := osvSeverity(withType); got != "CVSS_V3" {
		t.Fatalf("type fallback: got %q", got)
	}
	withDBSpecific := jsonenc.NewObject().Set("database_specific", jsonenc.NewObject().Set("severity", "HIGH"))
	if got := osvSeverity(withDBSpecific); got != "HIGH" {
		t.Fatalf("database_specific fallback: got %q", got)
	}
	if got := osvSeverity(jsonenc.NewObject()); got != "" {
		t.Fatalf("nothing present: got %q, want empty", got)
	}
}

func TestEffectiveCveDisposition(t *testing.T) {
	versions := map[string]map[string]struct{}{
		"npm::lodash": {"4.0.0": {}}, // only the disposition's own version currently observed
		"npm::react":  {"18.0.0": {}, "17.0.0": {}},
	}
	if disp, expired := effectiveCveDisposition("", "lodash", "npm", "4.0.0", versions); disp != "open" || expired {
		t.Fatalf("blank disposition defaults to open: got %q expired=%v", disp, expired)
	}
	if disp, expired := effectiveCveDisposition("ignored", "lodash", "npm", "4.0.0", versions); disp != "ignored" || expired {
		t.Fatalf("non-fixed disposition passes through: got %q expired=%v", disp, expired)
	}
	if disp, expired := effectiveCveDisposition("fixed", "lodash", "npm", "4.0.0", versions); disp != "fixed" || expired {
		t.Fatalf("fixed + only its own version observed: got %q expired=%v", disp, expired)
	}
	if disp, expired := effectiveCveDisposition("fixed", "react", "npm", "18.0.0", versions); disp != "open" || !expired {
		t.Fatalf("fixed + a different version now observed -> expires: got %q expired=%v", disp, expired)
	}
}

func TestCveSplitIds(t *testing.T) {
	got := cveSplitIds("CVE-2024-1,,CVE-2024-2, ")
	if len(got) != 3 || got[2] != " " { // only empty segments are dropped, not whitespace-only
		t.Fatalf("got %v", got)
	}
	if got := cveSplitIds(""); len(got) != 0 {
		t.Fatalf("blank input: want empty, got %v", got)
	}
}

// loadCveDispositions shapes the sobs_cve_dispositions rows into a keyed lookup map; the
// corpus's empty fixture never has a disposition row.
func TestLoadCveDispositions(t *testing.T) {
	cols := []string{"OsvId", "Package", "Ecosystem", "Version", "Disposition", "Note"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(_ string, _ ...any) (*store.Result, error) {
		return storetest.Result(cols,
			[]any{"CVE-1", "lodash", "npm", "4.0.0", "", "no note"}, // blank disposition -> "open"
			[]any{"CVE-2", "react", "npm", "18.0.0", "ignored", "false positive"},
		), nil
	}}}
	got := s.loadCveDispositions()
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %v", len(got), got)
	}
	e := got["CVE-1::lodash::npm::4.0.0"]
	if e.disposition != "open" || e.note != "no note" {
		t.Fatalf("blank-disposition entry wrong: %+v", e)
	}
	e2 := got["CVE-2::react::npm::18.0.0"]
	if e2.disposition != "ignored" {
		t.Fatalf("unexpected entry: %+v", e2)
	}

	sErr := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return nil, errors.New("boom")
	}}}
	if got := sErr.loadCveDispositions(); len(got) != 0 {
		t.Fatalf("query error: want empty, got %v", got)
	}
}
