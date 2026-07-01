package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// resolveAgentGithubTarget resolves (repo, token) for agent-created GitHub issues: an app-scoped
// repo takes priority over the global default, and a repo-scoped token takes priority over the
// global default token. The corpus's parity rule never maps a service to an app and never sets a
// global repo, so every branch below is corpus-unreachable. Oracle: app.py _resolve_agent_github_target.
func TestResolveAgentGithubTarget(t *testing.T) {
	settingsQueryFor := func(appRepoURL string, scopedToken map[string]string) *storetest.FakeDB {
		return &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			switch {
			case strings.Contains(q, "sobs_apps"):
				if appRepoURL == "" {
					return &store.Result{}, nil
				}
				return storetest.Result([]string{"RepoUrl"}, []any{appRepoURL}), nil
			case strings.Contains(q, "sobs_ai_settings"):
				if len(params) == 1 {
					if key, ok := params[0].(string); ok {
						if v, present := scopedToken[key]; present {
							return storetest.Result([]string{"Value"}, []any{v}), nil
						}
					}
				}
				return &store.Result{}, nil
			}
			return &store.Result{}, nil
		}}
	}
	svcTctx := jsonenc.NewObject().Set("service", "svc-a")

	t.Run("service resolves app repo, no scoped token -> default token", func(t *testing.T) {
		s := &server{db: settingsQueryFor("https://github.com/acme/widgets", nil)}
		repo, token := s.resolveAgentGithubTarget(map[string]string{"ai.github_token": "deftok"}, svcTctx)
		if repo != "acme/widgets" || token != "deftok" {
			t.Fatalf("got repo=%q token=%q", repo, token)
		}
	})

	t.Run("service resolves app repo with a repo-scoped token", func(t *testing.T) {
		s := &server{db: settingsQueryFor("https://github.com/beta/repo2",
			map[string]string{"ai.github_token.repo.beta/repo2": "scoped-tok"})}
		repo, token := s.resolveAgentGithubTarget(map[string]string{"ai.github_token": "deftok"}, svcTctx)
		if repo != "beta/repo2" || token != "scoped-tok" {
			t.Fatalf("got repo=%q token=%q", repo, token)
		}
	})

	t.Run("no app match, default repo is a bare owner/repo string", func(t *testing.T) {
		s := &server{db: settingsQueryFor("", nil)}
		repo, token := s.resolveAgentGithubTarget(
			map[string]string{"ai.github_repo": "owner3/repo3", "ai.github_token": "deftok"}, svcTctx)
		if repo != "owner3/repo3" || token != "deftok" {
			t.Fatalf("got repo=%q token=%q", repo, token)
		}
	})

	t.Run("default repo unparseable but slash-splits to >=2 segments", func(t *testing.T) {
		s := &server{db: settingsQueryFor("", nil)}
		repo, token := s.resolveAgentGithubTarget(
			map[string]string{"ai.github_repo": "/org4/team/repo4/", "ai.github_token": "deftok"},
			jsonenc.NewObject())
		if repo != "team/repo4" || token != "deftok" {
			t.Fatalf("got repo=%q token=%q", repo, token)
		}
	})

	t.Run("default repo entirely unparseable -> raw passthrough", func(t *testing.T) {
		s := &server{db: settingsQueryFor("", nil)}
		repo, token := s.resolveAgentGithubTarget(
			map[string]string{"ai.github_repo": "not-a-valid-repo-string", "ai.github_token": "deftok"},
			jsonenc.NewObject())
		if repo != "not-a-valid-repo-string" || token != "deftok" {
			t.Fatalf("got repo=%q token=%q", repo, token)
		}
	})

	t.Run("no service, no default repo -> empty repo, default token", func(t *testing.T) {
		s := &server{db: settingsQueryFor("", nil)}
		repo, token := s.resolveAgentGithubTarget(map[string]string{"ai.github_token": "deftok"}, jsonenc.NewObject())
		if repo != "" || token != "deftok" {
			t.Fatalf("got repo=%q token=%q", repo, token)
		}
	})

	t.Run("app repo found but fails to parse into owner/repo falls through to default", func(t *testing.T) {
		s := &server{db: settingsQueryFor("not-a-url-at-all", nil)}
		repo, token := s.resolveAgentGithubTarget(
			map[string]string{"ai.github_repo": "owner5/repo5", "ai.github_token": "deftok"}, svcTctx)
		if repo != "owner5/repo5" || token != "deftok" {
			t.Fatalf("got repo=%q token=%q", repo, token)
		}
	})
}
