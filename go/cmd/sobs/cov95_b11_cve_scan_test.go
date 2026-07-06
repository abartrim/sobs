package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b11_cve_scan_test.go — batch 11 targeted coverage for cmd/sobs/cve_scan.go's undertested
// branches: inventoryScopeEcosystem's opentelemetry-scope-with-underscore rejection,
// githubRefCandidates' v-prefix dedup, decodeGithubContentsPayload's non-base64/missing-content
// branches, githubContentsLockfileRows' 404-skip / non-200-break / success paths,
// collectLibraryInventory's populated 3-tier merge, cveMetadataDependencies' malformed-JSON
// branches, and runCveOSVScan's fixture-backed OSV query loop (success, vuln cap, error skip).

// Note: inventoryScopeEcosystem is already covered by TestInventoryScopeEcosystem in
// cve_helpers_test.go (a prior batch); not re-tested here.

// ---- githubRefCandidates ---------------------------------------------------------------------

func TestGithubRefCandidates(t *testing.T) {
	t.Run("empty version yields nil", func(t *testing.T) {
		if got := githubRefCandidates(""); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("v-prefixed version has no duplicate v-tag candidate", func(t *testing.T) {
		got := githubRefCandidates("v1.2.3")
		want := []string{"refs/tags/v1.2.3", "refs/heads/v1.2.3", "v1.2.3"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("index %d: got %q want %q", i, got[i], want[i])
			}
		}
	})
	t.Run("bare version adds a v-prefixed tag candidate", func(t *testing.T) {
		got := githubRefCandidates("1.0.0")
		want := []string{"refs/tags/1.0.0", "refs/tags/v1.0.0", "refs/heads/1.0.0", "1.0.0"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

// ---- decodeGithubContentsPayload -------------------------------------------------------------

func TestDecodeGithubContentsPayload(t *testing.T) {
	t.Run("missing content -> nil", func(t *testing.T) {
		if got := decodeGithubContentsPayload(jsonenc.NewObject().Set("encoding", "base64")); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("non-base64 encoding -> nil", func(t *testing.T) {
		o := jsonenc.NewObject().Set("content", "aGVsbG8=").Set("encoding", "utf-8")
		if got := decodeGithubContentsPayload(o); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
	t.Run("base64 (case-insensitive) decodes leniently", func(t *testing.T) {
		o := jsonenc.NewObject().Set("content", "aGVs\nbG8=").Set("encoding", "BASE64")
		got := decodeGithubContentsPayload(o)
		if string(got) != "hello" {
			t.Errorf("got %q, want hello", got)
		}
	})
}

// ---- githubContentsLockfileRows ---------------------------------------------------------------

func TestGithubContentsLockfileRows(t *testing.T) {
	t.Run("every ref/candidate 404s -> no rows", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		s := &server{db: &storetest.FakeDB{}}
		got := s.githubContentsLockfileRows("tok", "acme", "widgets", "rel-1", "1.0.0")
		if got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})

	t.Run("non-200 non-404 breaks the candidate loop for that ref", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		// First ref candidate is "refs/tags/2.0.0"; requirements.txt request for it gets a 500,
		// which should `break` out of the candidate loop (not fall through to package-lock.json).
		url := "https://api.github.com/repos/acme/widgets/contents/requirements.txt?ref=refs%2Ftags%2F2.0.0"
		writeUpstreamFixture(t, dir, "GET", url, `{"status": 500, "json": {"message": "boom"}}`)
		s := &server{db: &storetest.FakeDB{}}
		got := s.githubContentsLockfileRows("tok", "acme", "widgets", "rel-2", "2.0.0")
		if got != nil {
			t.Errorf("want nil (break on 500), got %v", got)
		}
	})

	t.Run("success decodes requirements.txt into a dependencies-lockfile row", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		url := "https://api.github.com/repos/acme/widgets/contents/requirements.txt?ref=refs%2Ftags%2F3.0.0"
		content := "flask==2.0.1\n# a comment\nrequests==2.31.0\n"
		encoded := base64Std(content)
		writeUpstreamFixture(t, dir, "GET", url,
			`{"status": 200, "json": {"content": "`+encoded+`", "encoding": "base64"}}`)
		s := &server{db: &storetest.FakeDB{}}
		got := s.githubContentsLockfileRows("tok", "acme", "widgets", "rel-3", "3.0.0")
		if len(got) != 1 {
			t.Fatalf("want 1 row, got %d: %v", len(got), got)
		}
		row := got[0]
		if row["ArtifactType"] != "dependencies-lockfile" || row["Name"] != "requirements.txt" {
			t.Errorf("unexpected row: %v", row)
		}
		if row["StorageRef"] != "github://acme/widgets/requirements.txt?ref=refs%2Ftags%2F3.0.0" {
			t.Errorf("unexpected StorageRef: %v", row["StorageRef"])
		}
		meta, _ := row["MetadataJson"].(string)
		if !strings.Contains(meta, "flask") || !strings.Contains(meta, "requests") {
			t.Errorf("metadata missing expected deps: %s", meta)
		}
	})

	t.Run("first candidate has content but parses to zero deps, falls through to next candidate", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		reqURL := "https://api.github.com/repos/acme/w2/contents/requirements.txt?ref=v4.0.0"
		writeUpstreamFixture(t, dir, "GET", reqURL,
			`{"status": 200, "json": {"content": "`+base64Std("# just a comment, no deps\n")+`", "encoding": "base64"}}`)
		lockURL := "https://api.github.com/repos/acme/w2/contents/package-lock.json?ref=v4.0.0"
		lockContent := `{"dependencies":{"left-pad":{"version":"1.3.0"}}}`
		writeUpstreamFixture(t, dir, "GET", lockURL,
			`{"status": 200, "json": {"content": "`+base64Std(lockContent)+`", "encoding": "base64"}}`)
		s := &server{db: &storetest.FakeDB{}}
		got := s.githubContentsLockfileRows("tok", "acme", "w2", "rel-4", "v4.0.0")
		if len(got) != 1 || got[0]["Name"] != "package-lock.json" {
			t.Fatalf("want the package-lock.json row, got %v", got)
		}
	})
}

// ---- collectLibraryInventory -------------------------------------------------------------------

func TestCollectLibraryInventory_Populated(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_release_artifacts"):
			meta := `{"dependencies":[{"package":"flask","version":"2.0.1","ecosystem":"PyPI"}]}`
			return storetest.Result([]string{"ReleaseId", "Name", "MetadataJson"},
				[]any{"rel-1", "requirements.txt", meta}), nil
		case strings.Contains(q, "sobs_app_releases"):
			return storetest.Result([]string{"Id", "AppId", "ReleaseVersion", "Environment"},
				[]any{"rel-1", "app-1", "1.0.0", "prod"}), nil
		case strings.Contains(q, "sobs_apps"):
			return storetest.Result([]string{"Id", "Name", "Slug"}, []any{"app-1", "Widgets", "widgets"}), nil
		case strings.Contains(q, "otel_traces") && strings.Contains(q, "telemetry.sdk.version"):
			return storetest.Result([]string{"sdk_name", "sdk_version", "sdk_lang", "ServiceName"},
				[]any{"opentelemetry", "1.9.0", "python", "svc-a"}), nil
		case strings.Contains(q, "otel_logs") && strings.Contains(q, "telemetry.sdk.version"):
			return &store.Result{}, nil
		case strings.Contains(q, "otel_traces") && strings.Contains(q, "ScopeVersion"):
			return storetest.Result([]string{"ScopeName", "ScopeVersion", "ServiceName"},
				[]any{"opentelemetry-flask", "1.9.0", "svc-a"}), nil
		case strings.Contains(q, "otel_logs") && strings.Contains(q, "ScopeVersion"):
			return &store.Result{}, nil
		}
		return &store.Result{}, nil
	}}}
	got := s.collectLibraryInventory()
	// Tier 1 (release-registry flask), tier 2 (otel_sdk "opentelemetry" pkg), and tier 3
	// (otel_scope "opentelemetry-flask" scope) each contribute one distinct dedup key.
	if len(got) != 3 {
		t.Fatalf("want 3 libraries (tier1 flask + tier2 sdk + tier3 scope), got %d: %+v", len(got), got)
	}
	foundFlaskTier1 := false
	foundSDKTier2 := false
	foundScopeTier3 := false
	for _, lib := range got {
		if lib.pkg == "flask" && lib.source == "release_registry" && lib.appName == "Widgets" {
			foundFlaskTier1 = true
		}
		if lib.pkg == "opentelemetry" && lib.source == "otel_sdk" && lib.ecosystem == "PyPI" {
			foundSDKTier2 = true
		}
		if lib.pkg == "opentelemetry-flask" && lib.source == "otel_scope" && lib.ecosystem == "PyPI" {
			foundScopeTier3 = true
		}
	}
	if !foundFlaskTier1 {
		t.Errorf("missing tier-1 flask entry: %+v", got)
	}
	if !foundSDKTier2 {
		t.Errorf("missing tier-2 sdk entry: %+v", got)
	}
	if !foundScopeTier3 {
		t.Errorf("missing tier-3 scope entry: %+v", got)
	}
}

// ---- cveMetadataDependencies --------------------------------------------------------------------
//
// Note: the invalid-JSON / missing-key / not-an-array / non-object-entry branches are already
// covered by TestCveMetadataDependencies in cve_helpers_test.go (a prior batch). The one branch
// that leaves uncovered — parseJSONValue succeeding but yielding a non-*jsonenc.Object top-level
// value (e.g. a bare JSON array) — is added here.

func TestCveMetadataDependencies_TopLevelArray(t *testing.T) {
	if got := cveMetadataDependencies(`["a", "b"]`); len(got) != 0 {
		t.Errorf("top-level array (not an object) -> want empty, got %v", got)
	}
}

// ---- runCveOSVScan --------------------------------------------------------------------------

func TestRunCveOSVScan(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	osvURL := "https://api.osv.dev/v1/query"
	vulnsJSON := `{"vulns": [
		{"id":"OSV-1","aliases":["CVE-2024-0001","GHSA-xxx"],"summary":"first",
		 "severity":[{"type":"CVSS_V3","score":"7.5"}],"published":"2024-01-01T00:00:00Z"},
		{"id":"OSV-2","aliases":[],"summary":"second","database_specific":{"severity":"HIGH"}}
	]}`
	writeUpstreamFixture(t, dir, "POST", osvURL, `{"status": 200, "json": `+vulnsJSON+`}`)

	var insertedTable string
	var insertedRows []map[string]any
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}}
	// Capture inserts via the FakeDB.Inserts slice (insertRowsNormalized wraps InsertJSONEachRow).
	libs := []cveLib{
		{pkg: "flask", version: "2.0.1", ecosystem: "PyPI", service: "svc-a"},
		{pkg: "", ecosystem: "PyPI"},  // skipped: empty pkg
		{pkg: "nolib", ecosystem: ""}, // skipped: empty ecosystem
	}
	libCount, vulnCount := s.runCveOSVScan("2026-01-01 00:00:00.000000", libs)
	if libCount != len(libs) {
		t.Errorf("libCount = %d, want %d", libCount, len(libs))
	}
	if vulnCount != 2 {
		t.Errorf("vulnCount = %d, want 2", vulnCount)
	}
	fdb := s.db.(*storetest.FakeDB)
	for _, ins := range fdb.Inserts {
		if ins.Table == "sobs_cve_findings" {
			insertedTable = ins.Table
			insertedRows = ins.Rows
		}
	}
	if insertedTable != "sobs_cve_findings" || len(insertedRows) != 2 {
		t.Fatalf("want 2 findings inserted, got table=%q rows=%d", insertedTable, len(insertedRows))
	}
	if insertedRows[0]["CveIds"] != "CVE-2024-0001" || insertedRows[0]["Severity"] != "7.5" {
		t.Errorf("unexpected first finding: %+v", insertedRows[0])
	}
	if insertedRows[1]["Severity"] != "HIGH" {
		t.Errorf("unexpected second finding severity: %+v", insertedRows[1])
	}
}

func TestRunCveOSVScan_NonOKAndErrorSkipped(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// No fixture written -> upstreamFixture returns a 404 -> the loop `continue`s without inserting.
	s := &server{db: &storetest.FakeDB{}}
	libCount, vulnCount := s.runCveOSVScan("2026-01-01 00:00:00.000000", []cveLib{
		{pkg: "flask", version: "2.0.1", ecosystem: "PyPI"},
	})
	if libCount != 1 || vulnCount != 0 {
		t.Errorf("libCount=%d vulnCount=%d, want 1,0", libCount, vulnCount)
	}
}

// ---- truncate -------------------------------------------------------------------------------

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("want unchanged short string, got %q", got)
	}
	if got := truncate("hello world", 5); got != "hello" {
		t.Errorf("want truncated to 5 runes, got %q", got)
	}
	// Multibyte-safe: truncate must not split a rune in the middle.
	multibyte := "日本語のテスト"
	if got := truncate(multibyte, 3); got != "日本語" {
		t.Errorf("want rune-safe truncation, got %q", got)
	}
}

// ---- test helpers -----------------------------------------------------------------------------
//
// writeUpstreamFixture (shared SOBS_UPSTREAM_FIXTURES writer) is already defined in
// cov95_b4_handlers_mutations2_test.go; reused here rather than redeclared.

// base64Std encodes s the same way base64.b64decode(..., validate=False) expects on the wire
// (standard alphabet, padded) — matching decodeGithubContentsPayload's decoder.
func base64Std(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
