package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b12_enrichment_loops_test.go — coverage-gate batch 12 targeted coverage for
// cmd/sobs/enrichment_loops.go's DB-backed, token-gated bodies. residual_workers_test.go already
// covers collectGithubRepoHealthSummary's all-zero (no apps / no token) path and
// syncGithubRepoHealthOnce's "no previous summary" write path; this file drives the previously
// unreached branches:
//   - collectGithubRepoHealthSummary's token-gated GitHub egress (via a real repo target + a
//     canned upstream fixture), including the issue/PR/security-item counting and repo sort;
//   - syncGithubRepoHealthOnce's dedup-against-stored-summary branch (both the "unchanged, skip
//     write" and the "changed, write" cases);
//   - objGetInt's int/int64/string(json.Number-like)/unparseable branches.
//
// cveScannerLoop and githubRepoHealthLoop themselves are NOT tested here: both are
// `time.Sleep(initialDelay); for { ...; time.Sleep(interval) }` background-worker DRIVERS,
// started only from startBackgroundWorkers (gated off under s.cfg.Parity — see
// background_tasks.go) and structured as genuine infinite loops with no exit condition. Calling
// them directly would hang the test (the shortest sleep here is cveScanInitialDelayS = 30s before
// the loop even starts, then real sleeps between iterations) with no way to make them return
// without modifying production code, which is out of scope for this batch. Their per-iteration
// bodies (runCveScan / syncGithubRepoHealthOnce) are ordinary one-shot methods and are exercised
// directly instead, both here and in residual_workers_test.go.

// writeGithubFixture drops a canned upstream JSON-array response for a GET request to reqURL,
// keyed the same way upstreamFixtureKey does (URL-only key — headers are not part of it).
func writeGithubFixture(t *testing.T, dir, reqURL, jsonArrayBody string) {
	t.Helper()
	stem := upstreamFixtureKey("GET", reqURL)
	spec := `{"status": 200, "json": ` + jsonArrayBody + `}`
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectGithubRepoHealthSummaryScansWithToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)

	reqURL := "https://api.github.com/repos/acme/widget/issues?state=open&per_page=" + strconv.Itoa(githubRepoHealthMaxItems)
	writeGithubFixture(t, dir, reqURL, `[
		{"title": "v1.2.0 crash on startup", "body": "affects v1.2.0", "pull_request": null,
		 "labels": [{"name": "bug"}]},
		{"title": "Security advisory for v1.2.0", "body": "CVE disclosure", "pull_request": null,
		 "labels": []},
		{"title": "Fix build for v1.2.0", "body": "", "pull_request": {"url": "x"}, "labels": []},
		{"title": "unrelated issue", "body": "no version mentioned here", "pull_request": null, "labels": []}
	]`)

	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_apps"):
			return storetest.Result(
				[]string{"Id", "Name", "Slug", "RepoUrl"},
				[]any{"app1", "Widget", "widget", "https://github.com/acme/widget"},
			), nil
		case strings.Contains(q, "sobs_app_releases"):
			return storetest.Result(
				[]string{"AppId", "ReleaseVersion"},
				[]any{"app1", "1.2.0"},
			), nil
		case strings.Contains(q, "sobs_ai_settings"):
			// loadAISetting("ai.github_token", ...) and repoScopedGithubToken both read here;
			// the repo-scoped key never matches so falls back to the default token below.
			if len(params) > 0 && params[0] == "ai.github_token" {
				return storetest.Result([]string{"Value"}, []any{"tok-default"}), nil
			}
			return &store.Result{}, nil
		default:
			return &store.Result{}, nil
		}
	}}
	s := &server{db: fake}

	summary := s.collectGithubRepoHealthSummary()
	okVal, _ := summary.Get("ok")
	if okVal != true {
		t.Fatalf("want ok:true, got %v", okVal)
	}
	if got := objGetInt(summary, "scanned_repos"); got != 1 {
		t.Fatalf("scanned_repos = %d, want 1", got)
	}
	if got := objGetInt(summary, "total_repos_considered"); got != 1 {
		t.Fatalf("total_repos_considered = %d, want 1", got)
	}
	// Two issues (crash + unrelated is excluded by version-token filtering -> only version-scoped
	// items count): "v1.2.0 crash" and "Security advisory for v1.2.0" are issues; "Fix build" is a PR.
	if got := objGetInt(summary, "open_issues"); got != 2 {
		t.Fatalf("open_issues = %d, want 2", got)
	}
	if got := objGetInt(summary, "open_prs"); got != 1 {
		t.Fatalf("open_prs = %d, want 1", got)
	}
	if got := objGetInt(summary, "security_items"); got != 1 {
		t.Fatalf("security_items = %d, want 1 (the CVE/security-titled issue)", got)
	}
	reposV, ok := summary.Get("repos")
	if !ok {
		t.Fatalf("summary missing repos key")
	}
	repos, ok := reposV.([]any)
	if !ok || len(repos) != 1 {
		t.Fatalf("repos = %#v, want a 1-element slice", reposV)
	}
}

func TestCollectGithubRepoHealthSummaryDBErrorOnAppsQuery(t *testing.T) {
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_apps") {
			return nil, errBoom
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	summary := s.collectGithubRepoHealthSummary()
	okVal, _ := summary.Get("ok")
	if okVal != false {
		t.Fatalf("want ok:false on apps query error, got %v", okVal)
	}
	if _, present := summary.Get("error"); !present {
		t.Fatalf("want an error field in the ok:false summary")
	}
}

func TestCollectGithubRepoHealthSummaryDBErrorOnReleasesQuery(t *testing.T) {
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_app_releases") {
			return nil, errBoom
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	summary := s.collectGithubRepoHealthSummary()
	okVal, _ := summary.Get("ok")
	if okVal != false {
		t.Fatalf("want ok:false on releases query error, got %v", okVal)
	}
}

// errBoom is a stand-in DB failure for the two error-path tests above.
type boomErr struct{}

func (boomErr) Error() string { return "boom" }

var errBoom = boomErr{}

func TestSyncGithubRepoHealthOnceDedupUnchangedSkipsWrite(t *testing.T) {
	// Previous stored summary has the SAME compact counts (all zero, matching the all-zero
	// no-token summary) -> the write is skipped entirely (mapsEqualInt short-circuit).
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_app_settings") && len(params) > 0 && params[0] == githubRepoHealthLastSummary {
			return storetest.Result([]string{"Value"}, []any{
				`{"scanned_repos":0,"total_repos_considered":0,"open_issues":0,"open_prs":0,"security_items":0,"last_synced_at":"2024-01-01T00:00:00.000Z"}`,
			}), nil
		}
		return &store.Result{}, nil // no apps -> all-zero summary
	}}
	s := &server{db: fake}
	summary := s.syncGithubRepoHealthOnce()
	okVal, _ := summary.Get("ok")
	if okVal != true {
		t.Fatalf("want ok:true, got %v", okVal)
	}
	if len(fake.Inserts) != 0 {
		t.Fatalf("dedup-unchanged: want 0 settings writes, got %d: %v", len(fake.Inserts), fake.Inserts)
	}
}

func TestSyncGithubRepoHealthOnceDedupChangedWrites(t *testing.T) {
	// Previous stored summary has DIFFERENT counts from the current (all-zero) summary -> the
	// mismatch triggers both the last-sync and compact-summary writes.
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_app_settings") && len(params) > 0 && params[0] == githubRepoHealthLastSummary {
			return storetest.Result([]string{"Value"}, []any{
				`{"scanned_repos":3,"total_repos_considered":5,"open_issues":2,"open_prs":1,"security_items":1,"last_synced_at":"2024-01-01T00:00:00.000Z"}`,
			}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	summary := s.syncGithubRepoHealthOnce()
	okVal, _ := summary.Get("ok")
	if okVal != true {
		t.Fatalf("want ok:true, got %v", okVal)
	}
	writes := map[string]int{}
	for _, in := range fake.Inserts {
		if in.Table == "sobs_app_settings" {
			writes["sobs_app_settings"]++
		}
	}
	if writes["sobs_app_settings"] != 2 {
		t.Fatalf("dedup-changed: want 2 settings writes (last-sync + summary), got %d", writes["sobs_app_settings"])
	}
}

func TestSyncGithubRepoHealthOnceDedupPreviousUnparsableFallsThroughToWrite(t *testing.T) {
	// A stored previous-summary value that isn't valid JSON leaves previousValues empty (all
	// zero-valued keys via objGetInt's not-found default), so it never equals the current
	// non-zero-shaped compact map in general — but with an all-zero current summary too it WOULD
	// match; use a non-empty garbage string that still fails to parse as an object to exercise the
	// `parsed.(*jsonenc.Object)` false branch distinctly (parses as a bare JSON string, not an
	// object).
	fake := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_app_settings") && len(params) > 0 && params[0] == githubRepoHealthLastSummary {
			return storetest.Result([]string{"Value"}, []any{`"just a string, not an object"`}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fake}
	summary := s.syncGithubRepoHealthOnce()
	okVal, _ := summary.Get("ok")
	if okVal != true {
		t.Fatalf("want ok:true, got %v", okVal)
	}
	// previousValues stays the zero map {} (len 0) which != compactValues (len 5, all zero
	// values) by mapsEqualInt's length check, so the write proceeds.
	writes := 0
	for _, in := range fake.Inserts {
		if in.Table == "sobs_app_settings" {
			writes++
		}
	}
	if writes != 2 {
		t.Fatalf("unparsable previous summary: want 2 settings writes, got %d", writes)
	}
}

// --- objGetInt ---------------------------------------------------------------------------------

func TestObjGetIntBranches(t *testing.T) {
	obj := jsonenc.NewObject().
		Set("as_int", 7).
		Set("as_int64", int64(9)).
		Set("as_str_ok", "42").
		Set("as_str_bad", "not-a-number").
		Set("as_str_pad", "  13  ").
		Set("unsupported", 3.14) // float64 falls to the default branch, then toStr/Atoi fails -> 0
	cases := map[string]int{
		"as_int":      7,
		"as_int64":    9,
		"as_str_ok":   42,
		"as_str_bad":  0,
		"as_str_pad":  13,
		"missing":     0,
		"unsupported": 0,
	}
	for key, want := range cases {
		if got := objGetInt(obj, key); got != want {
			t.Errorf("objGetInt(%q) = %d, want %d", key, got, want)
		}
	}
}
