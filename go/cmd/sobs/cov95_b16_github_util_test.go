package main

import (
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_github_util_test.go — batch 16 targeted coverage for cmd/sobs/github_util.go:
// parseGithubRepoOwnerName's HTTPS/SSH/bare-form/non-github-host branches, githubVersionTokens'
// empty-input and v-prefix-dedup branches, and repoScopedGithubToken's empty-owner/repo guard.

func TestParseGithubRepoOwnerNameVariants(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantOwner string
		wantRepo  string
	}{
		{"empty string", "", "", ""},
		{"bare owner/repo", "acme/widgets", "acme", "widgets"},
		{"bare owner/repo with .git suffix", "acme/widgets.git", "acme", "widgets"},
		{"https url", "https://github.com/acme/widgets", "acme", "widgets"},
		{"https url with .git suffix", "https://github.com/acme/widgets.git", "acme", "widgets"},
		{"ssh url", "git@github.com:acme/widgets.git", "acme", "widgets"},
		{"ssh url no .git", "git@github.com:acme/widgets", "acme", "widgets"},
		{"non-github host", "https://gitlab.com/acme/widgets", "", ""},
		{"malformed url", "://bad-url", "", ""},
		{"too few path segments", "https://github.com/acme", "", ""},
		{"trailing slash path", "https://github.com/acme/widgets/", "acme", "widgets"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner, repo := parseGithubRepoOwnerName(c.in)
			if owner != c.wantOwner || repo != c.wantRepo {
				t.Errorf("parseGithubRepoOwnerName(%q) = (%q, %q), want (%q, %q)", c.in, owner, repo, c.wantOwner, c.wantRepo)
			}
		})
	}
}

func TestGithubVersionTokensVariants(t *testing.T) {
	t.Run("empty version yields empty set", func(t *testing.T) {
		got := githubVersionTokens("")
		if len(got) != 0 {
			t.Errorf("want empty map, got %v", got)
		}
	})
	t.Run("whitespace-only version yields empty set", func(t *testing.T) {
		got := githubVersionTokens("   ")
		if len(got) != 0 {
			t.Errorf("want empty map, got %v", got)
		}
	})
	t.Run("bare version adds v-prefixed token", func(t *testing.T) {
		got := githubVersionTokens("1.2.3")
		if !got["1.2.3"] || !got["v1.2.3"] || len(got) != 2 {
			t.Errorf("got %v", got)
		}
	})
	t.Run("already v-prefixed version has no duplicate", func(t *testing.T) {
		got := githubVersionTokens("V1.2.3")
		if !got["v1.2.3"] || len(got) != 1 {
			t.Errorf("got %v, want single lowercased v1.2.3 entry", got)
		}
	})
}

func TestRepoScopedGithubTokenEmptyGuard(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		t.Fatal("db should not be queried when owner/repo is empty")
		return nil, nil
	}}}
	if got := s.repoScopedGithubToken("", "widgets"); got != "" {
		t.Errorf("empty owner: got %q, want empty", got)
	}
	if got := s.repoScopedGithubToken("acme", ""); got != "" {
		t.Errorf("empty repo: got %q, want empty", got)
	}
}

func TestRepoScopedGithubTokenLookup(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if len(params) == 1 && params[0] == githubRepoTokenKey("Acme", "Widgets") {
			return storetest.Result([]string{"Value"}, []any{"  scoped-tok  "}), nil
		}
		return &store.Result{}, nil
	}}}
	if got := s.repoScopedGithubToken("Acme", "Widgets"); got != "scoped-tok" {
		t.Errorf("got %q, want trimmed scoped-tok", got)
	}
}
