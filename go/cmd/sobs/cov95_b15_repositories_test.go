package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b15_repositories_test.go — batch 15 coverage for cmd/sobs/repositories.go:
//   buildRepositoriesApps (121)          89.7%
//   normalizeGithubTokenExpiry (19)      62.5%
//   githubTokenExpiryStatus (55)         90.0%
//   githubTokenExpiryDateInputValue (77) 66.7%

func TestNormalizeGithubTokenExpiry(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty -> empty", "", ""},
		{"whitespace only -> empty", "   ", ""},
		{"bare date -> end of day UTC", "2027-03-29", "2027-03-29T23:59:59+00:00"},
		{"bare date with surrounding whitespace", "  2027-03-29  ", "2027-03-29T23:59:59+00:00"},
		{"full ISO timestamp normalized", "2027-03-29T12:00:00Z", "2027-03-29T12:00:00+00:00"},
		// parseISODatetime UTC-normalizes the parsed time (t.UTC()), so a non-UTC offset input
		// comes back re-expressed at +00:00, not preserved verbatim.
		{"already-offset timestamp normalized to UTC", "2027-03-29T12:00:00+02:00", "2027-03-29T10:00:00+00:00"},
		{"garbage -> empty", "not-a-date", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizeGithubTokenExpiry(c.in); got != c.want {
				t.Errorf("normalizeGithubTokenExpiry(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestGithubTokenExpiryStatus(t *testing.T) {
	t.Run("unset -> unknown", func(t *testing.T) {
		got := githubTokenExpiryStatus("", 14)
		if got["state"] != "unknown" || got["days_remaining"] != nil {
			t.Errorf("got %#v", got)
		}
		if got["message"] != "Token expiry date not set" {
			t.Errorf("message = %v", got["message"])
		}
	})

	t.Run("expired", func(t *testing.T) {
		past := nowUTC().AddDate(0, 0, -5).Format("2006-01-02") + "T00:00:00Z"
		got := githubTokenExpiryStatus(past, 14)
		if got["state"] != "expired" {
			t.Errorf("state = %v, want expired", got["state"])
		}
		msg, _ := got["message"].(string)
		if !strings.HasPrefix(msg, "Token expired on") {
			t.Errorf("message = %q", msg)
		}
	})

	t.Run("warning within threshold", func(t *testing.T) {
		soon := nowUTC().AddDate(0, 0, 5).Format("2006-01-02") + "T23:59:59Z"
		got := githubTokenExpiryStatus(soon, 14)
		if got["state"] != "warning" {
			t.Errorf("state = %v, want warning", got["state"])
		}
	})

	t.Run("healthy beyond threshold", func(t *testing.T) {
		later := nowUTC().AddDate(0, 0, 60).Format("2006-01-02") + "T23:59:59Z"
		got := githubTokenExpiryStatus(later, 14)
		if got["state"] != "healthy" {
			t.Errorf("state = %v, want healthy", got["state"])
		}
		msg, _ := got["message"].(string)
		if !strings.Contains(msg, "healthy") {
			t.Errorf("message = %q", msg)
		}
	})
}

func TestGithubTokenExpiryDateInputValue(t *testing.T) {
	if got := githubTokenExpiryDateInputValue("2027-03-29T12:00:00Z"); got != "2027-03-29" {
		t.Errorf("got %q, want 2027-03-29", got)
	}
	if got := githubTokenExpiryDateInputValue("2027-03-29"); got != "2027-03-29" {
		t.Errorf("bare date got %q, want 2027-03-29", got)
	}
	if got := githubTokenExpiryDateInputValue("garbage"); got != "" {
		t.Errorf("unparseable got %q, want empty", got)
	}
	if got := githubTokenExpiryDateInputValue(""); got != "" {
		t.Errorf("empty got %q, want empty", got)
	}
}

func TestBuildRepositoriesApps(t *testing.T) {
	appCols := []string{"Id", "Name", "Slug", "RepoUrl", "Enabled"}
	relCols := []string{"AppId", "ReleaseVersion", "ReleasedAt"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "FROM sobs_apps"):
			return storetest.Result(appCols,
				[]any{"app-1", "Checkout", "checkout", "https://github.com/acme/checkout", 1.0},
				[]any{"app-2", "NoReleases", "no-releases", "", 0.0},
			), nil
		case strings.Contains(q, "FROM sobs_app_releases"):
			return storetest.Result(relCols,
				[]any{"app-1", "1.0.0", "2027-01-01 00:00:00"},
				[]any{"app-1", "1.0.1", "2027-01-02 00:00:00"},
				[]any{"app-1", "1.0.1", "2027-01-03 00:00:00"}, // duplicate version, must not double-count
			), nil
		case strings.Contains(q, "sobs_ai_settings"):
			return &store.Result{}, nil // ci push settings all absent
		}
		t.Fatalf("unexpected query: %s", q)
		return nil, nil
	}}}

	apps := s.buildRepositoriesApps(map[string]string{"app-1": "smcp_oneTimePlain"})
	if len(apps) != 2 {
		t.Fatalf("want 2 apps, got %d: %#v", len(apps), apps)
	}

	app1 := apps[0].(map[string]any)
	if app1["id"] != "app-1" || app1["name"] != "Checkout" {
		t.Fatalf("app1 = %#v", app1)
	}
	if app1["enabled"] != true {
		t.Errorf("enabled = %v, want true", app1["enabled"])
	}
	if app1["repo_owner"] != "acme" || app1["repo_name"] != "checkout" {
		t.Errorf("repo owner/name = %v/%v", app1["repo_owner"], app1["repo_name"])
	}
	if app1["release_count"] != 2 {
		t.Errorf("release_count = %v, want 2 (deduped)", app1["release_count"])
	}
	if app1["ci_push_plain"] != "smcp_oneTimePlain" {
		t.Errorf("ci_push_plain = %v, want the passed-through one-time plaintext", app1["ci_push_plain"])
	}

	app2 := apps[1].(map[string]any)
	if app2["enabled"] != false {
		t.Errorf("app2 enabled = %v, want false (explicit Enabled=0)", app2["enabled"])
	}
	if app2["ci_push_plain"] != "" {
		t.Errorf("app2 ci_push_plain = %v, want empty (no entry in the map)", app2["ci_push_plain"])
	}
	if app2["release_count"] != 0 {
		t.Errorf("app2 release_count = %v, want 0", app2["release_count"])
	}
}

func TestBuildRepositoriesApps_QueryError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return nil, errB15Boom
	}}}
	if got := s.buildRepositoriesApps(nil); len(got) != 0 {
		t.Errorf("expected empty slice on query error, got %#v", got)
	}
}

func TestBuildRepositoriesApps_MoreThanFiveReleasesTruncatesLatest(t *testing.T) {
	appCols := []string{"Id", "Name", "Slug", "RepoUrl"}
	relCols := []string{"AppId", "ReleaseVersion", "ReleasedAt"}
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "FROM sobs_apps"):
			return storetest.Result(appCols, []any{"app-1", "App", "app", ""}), nil
		case strings.Contains(q, "FROM sobs_app_releases"):
			rows := make([][]any, 0, 7)
			for i := 1; i <= 7; i++ {
				rows = append(rows, []any{"app-1", "1.0." + itoa(i), "2027-01-0" + itoa(i%9+1) + " 00:00:00"})
			}
			return &store.Result{Columns: relCols, Rows: rows}, nil
		}
		return &store.Result{}, nil
	}}}
	apps := s.buildRepositoriesApps(nil)
	app1 := apps[0].(map[string]any)
	if app1["release_count"] != 7 {
		t.Errorf("release_count = %v, want 7 (total distinct)", app1["release_count"])
	}
	latest, _ := app1["latest_versions"].([]any)
	if len(latest) != 5 {
		t.Errorf("latest_versions len = %d, want 5 (capped)", len(latest))
	}
}
