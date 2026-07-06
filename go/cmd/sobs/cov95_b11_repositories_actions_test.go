package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b11_repositories_actions_test.go — batch 11 targeted coverage for
// cmd/sobs/repositories_actions.go: normalizeTTLDays' parse-failure/clamp branches,
// normalizeTTLDaysInt's clamp branches, repoAddRelease's validation/success branches, repoUpdate's
// validation/token-save/success branches, repoDelete's release-cascade branch,
// validateGithubToken's every status branch, and metadataJSONOr.

// ---- normalizeTTLDays / normalizeTTLDaysInt --------------------------------------------------

func TestNormalizeTTLDays(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", ciPushDefaultTTLDays}, // unparseable -> default
		{"not-a-number", ciPushDefaultTTLDays},
		{"0", ciPushMinTTLDays}, // below min -> clamp up
		{"-5", ciPushMinTTLDays},
		{"9999", ciPushMaxTTLDays}, // above max -> clamp down
		{"45", 45},                 // within range -> unchanged
		{"  10  ", 10},             // trimmed
	}
	for _, c := range cases {
		if got := normalizeTTLDays(c.in); got != c.want {
			t.Errorf("normalizeTTLDays(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestNormalizeTTLDaysInt(t *testing.T) {
	if got := normalizeTTLDaysInt(0); got != ciPushMinTTLDays {
		t.Errorf("got %d, want min", got)
	}
	if got := normalizeTTLDaysInt(-100); got != ciPushMinTTLDays {
		t.Errorf("got %d, want min", got)
	}
	if got := normalizeTTLDaysInt(999999); got != ciPushMaxTTLDays {
		t.Errorf("got %d, want max", got)
	}
	if got := normalizeTTLDaysInt(50); got != 50 {
		t.Errorf("got %d, want 50 (within range)", got)
	}
}

// ---- repoAddRelease -----------------------------------------------------------------------------

func TestRepoAddRelease(t *testing.T) {
	t.Run("missing version -> warning flash, no insert", func(t *testing.T) {
		fdb := &storetest.FakeDB{}
		s := &server{db: fdb}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/settings/repositories/app1/add-release",
			strings.NewReader(url.Values{"environment": {"prod"}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.repoAddRelease(w, r, "app1", map[string]any{})
		if w.Code != http.StatusFound {
			t.Fatalf("want redirect, got %d", w.Code)
		}
		if !strings.Contains(w.Header().Get("Set-Cookie"), "sobs_session=") {
			t.Fatalf("want a flash cookie, got %q", w.Header().Get("Set-Cookie"))
		}
		if len(fdb.Inserts) != 0 {
			t.Errorf("want no insert on validation failure, got %v", fdb.Inserts)
		}
	})

	t.Run("success inserts a release row and flashes success", func(t *testing.T) {
		fdb := &storetest.FakeDB{}
		s := &server{db: fdb}
		w := httptest.NewRecorder()
		form := url.Values{"version": {"1.2.3"}, "environment": {"prod"}}
		r := httptest.NewRequest(http.MethodPost, "/settings/repositories/app1/add-release",
			strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.repoAddRelease(w, r, "app1", map[string]any{})
		if w.Code != http.StatusFound {
			t.Fatalf("want redirect, got %d: %s", w.Code, w.Body.String())
		}
		if len(fdb.Inserts) != 1 || fdb.Inserts[0].Table != "sobs_app_releases" {
			t.Fatalf("want 1 release insert, got %v", fdb.Inserts)
		}
		row := fdb.Inserts[0].Rows[0]
		if row["AppId"] != "app1" || row["ReleaseVersion"] != "1.2.3" || row["Environment"] != "prod" {
			t.Errorf("unexpected row: %v", row)
		}
	})

	t.Run("db error -> dbError response, not a flash redirect", func(t *testing.T) {
		fdb := &storetest.FakeDB{InsertErr: assertErr("insert failed")}
		s := &server{db: fdb}
		w := httptest.NewRecorder()
		form := url.Values{"version": {"1.0.0"}}
		r := httptest.NewRequest(http.MethodPost, "/settings/repositories/app1/add-release",
			strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.repoAddRelease(w, r, "app1", map[string]any{})
		if w.Code == http.StatusFound {
			t.Fatalf("want a non-redirect error response, got %d", w.Code)
		}
	})
}

// ---- repoUpdate -----------------------------------------------------------------------------

func TestRepoUpdate(t *testing.T) {
	t.Run("empty repo -> warning flash, no insert", func(t *testing.T) {
		fdb := &storetest.FakeDB{}
		s := &server{db: fdb}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/settings/repositories/app1", strings.NewReader(""))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.repoUpdate(w, r, "app1", map[string]any{})
		if w.Code != http.StatusFound {
			t.Fatalf("want redirect, got %d", w.Code)
		}
		if len(fdb.Inserts) != 0 {
			t.Errorf("want no insert, got %v", fdb.Inserts)
		}
	})

	t.Run("success without token save (set_repo_token unset)", func(t *testing.T) {
		fdb := &storetest.FakeDB{}
		s := &server{db: fdb}
		w := httptest.NewRecorder()
		form := url.Values{"repo_url": {"https://github.com/acme/widgets"}}
		r := httptest.NewRequest(http.MethodPost, "/settings/repositories/app1", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		current := map[string]any{"Name": "Widgets", "Slug": "widgets", "CreatedAt": "2024-01-01T00:00:00Z"}
		s.repoUpdate(w, r, "app1", current)
		if w.Code != http.StatusFound {
			t.Fatalf("want redirect, got %d: %s", w.Code, w.Body.String())
		}
		var appsInsert *storetest.Insert
		var settingsInsert *storetest.Insert
		for i := range fdb.Inserts {
			switch fdb.Inserts[i].Table {
			case "sobs_apps":
				appsInsert = &fdb.Inserts[i]
			case "sobs_ai_settings":
				settingsInsert = &fdb.Inserts[i]
			}
		}
		if appsInsert == nil {
			t.Fatal("want an sobs_apps insert")
		}
		if settingsInsert != nil {
			t.Errorf("want no token saved (set_repo_token unset), got %v", settingsInsert)
		}
	})

	t.Run("success with repo token save when set_repo_token is present", func(t *testing.T) {
		fdb := &storetest.FakeDB{}
		s := &server{db: fdb}
		w := httptest.NewRecorder()
		form := url.Values{
			"repo_url":       {"https://github.com/acme/widgets2"},
			"repo_token":     {"ghp_abc123"},
			"set_repo_token": {"1"},
		}
		r := httptest.NewRequest(http.MethodPost, "/settings/repositories/app2", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.repoUpdate(w, r, "app2", map[string]any{"Name": "W2"})
		if w.Code != http.StatusFound {
			t.Fatalf("want redirect, got %d: %s", w.Code, w.Body.String())
		}
		found := false
		for _, ins := range fdb.Inserts {
			if ins.Table == "sobs_ai_settings" {
				for _, row := range ins.Rows {
					if row["Key"] == githubRepoTokenKey("acme", "widgets2") {
						found = true
					}
				}
			}
		}
		if !found {
			t.Errorf("want the repo-scoped token saved, got %v", fdb.Inserts)
		}
	})

	t.Run("db error on the apps insert -> dbError response", func(t *testing.T) {
		fdb := &storetest.FakeDB{InsertErr: assertErr("boom")}
		s := &server{db: fdb}
		w := httptest.NewRecorder()
		form := url.Values{"repo_url": {"https://github.com/acme/w3"}}
		r := httptest.NewRequest(http.MethodPost, "/settings/repositories/app3", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.repoUpdate(w, r, "app3", map[string]any{})
		if w.Code == http.StatusFound {
			t.Fatalf("want a non-redirect error response, got %d", w.Code)
		}
	})
}

// ---- validateGithubToken --------------------------------------------------------------------

func TestValidateGithubToken(t *testing.T) {
	t.Run("empty token -> missing", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		status, msg := s.validateGithubToken("  ")
		if status != "missing" || msg != "No token configured" {
			t.Errorf("got %q %q", status, msg)
		}
	})

	t.Run("200 -> valid", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		writeUpstreamFixture(t, dir, "GET", "https://api.github.com/rate_limit", `{"status": 200, "json": {}}`)
		s := &server{db: &storetest.FakeDB{}}
		status, msg := s.validateGithubToken("tok")
		if status != "valid" || msg != "Token is valid" {
			t.Errorf("got %q %q", status, msg)
		}
	})

	t.Run("401 -> invalid", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		writeUpstreamFixture(t, dir, "GET", "https://api.github.com/rate_limit", `{"status": 401, "json": {}}`)
		s := &server{db: &storetest.FakeDB{}}
		status, msg := s.validateGithubToken("tok")
		if status != "invalid" || msg != "Token rejected (401 Unauthorized)" {
			t.Errorf("got %q %q", status, msg)
		}
	})

	t.Run("403 -> error (forbidden/rate-limited)", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		writeUpstreamFixture(t, dir, "GET", "https://api.github.com/rate_limit", `{"status": 403, "json": {}}`)
		s := &server{db: &storetest.FakeDB{}}
		status, msg := s.validateGithubToken("tok")
		if status != "error" || msg != "GitHub returned 403 (forbidden or rate-limited)" {
			t.Errorf("got %q %q", status, msg)
		}
	})

	t.Run("other status -> generic error with status code", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
		writeUpstreamFixture(t, dir, "GET", "https://api.github.com/rate_limit", `{"status": 500, "json": {}}`)
		s := &server{db: &storetest.FakeDB{}}
		status, msg := s.validateGithubToken("tok")
		if status != "error" || msg != "GitHub returned HTTP 500" {
			t.Errorf("got %q %q", status, msg)
		}
	})
}

// ---- metadataJSONOr -------------------------------------------------------------------------

func TestMetadataJSONOr(t *testing.T) {
	if got := metadataJSONOr(map[string]any{"MetadataJson": ""}); got != "{}" {
		t.Errorf("empty -> want {}, got %q", got)
	}
	if got := metadataJSONOr(map[string]any{}); got != "{}" {
		t.Errorf("missing key -> want {}, got %q", got)
	}
	if got := metadataJSONOr(map[string]any{"MetadataJson": `{"a":1}`}); got != `{"a":1}` {
		t.Errorf("present -> want passthrough, got %q", got)
	}
}

// ---- repoDelete (release-cascade branch not covered by cov95_b3_handlers_forms_test.go) --------

func TestRepoDelete_CascadesReleases(t *testing.T) {
	fdb := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_app_releases") {
			return storetest.Result([]string{"Id", "AppId", "ReleaseVersion", "CommitSha", "BuildId",
				"Environment", "ReleasedAt", "MetadataJson"},
				[]any{"rel-1", "app1", "1.0.0", "sha1", "build1", "prod", "2024-01-01T00:00:00Z", "{}"}), nil
		}
		return &store.Result{}, nil
	}}
	s := &server{db: fdb}
	w := httptest.NewRecorder()
	s.repoDelete(w, "app1", map[string]any{"Name": "Widgets", "Slug": "widgets", "CreatedAt": "2024-01-01T00:00:00Z"})
	if w.Code != http.StatusFound {
		t.Fatalf("want redirect, got %d: %s", w.Code, w.Body.String())
	}
	var appsIns, releasesIns *storetest.Insert
	for i := range fdb.Inserts {
		switch fdb.Inserts[i].Table {
		case "sobs_apps":
			appsIns = &fdb.Inserts[i]
		case "sobs_app_releases":
			releasesIns = &fdb.Inserts[i]
		}
	}
	if appsIns == nil || appsIns.Rows[0]["IsDeleted"] != 1 {
		t.Fatalf("want a tombstoned app insert, got %v", appsIns)
	}
	if releasesIns == nil || len(releasesIns.Rows) != 1 || releasesIns.Rows[0]["IsDeleted"] != 1 {
		t.Fatalf("want a tombstoned release cascade, got %v", releasesIns)
	}
}

func TestRepoDelete_DbErrorOnAppsInsert(t *testing.T) {
	fdb := &storetest.FakeDB{InsertErr: assertErr("boom")}
	s := &server{db: fdb}
	w := httptest.NewRecorder()
	s.repoDelete(w, "app1", map[string]any{"Name": "W"})
	if w.Code == http.StatusFound {
		t.Fatalf("want a non-redirect error response, got %d", w.Code)
	}
}
