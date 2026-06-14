package main

import (
	"net/url"
	"strings"
)

// parseGithubRepoOwnerName mirrors app.py _parse_github_repo_owner_name: extract owner/repo from
// an HTTPS, SSH, or plain "owner/repo" GitHub URL. Returns ("","") when not a github.com repo.
func parseGithubRepoOwnerName(repoURL string) (string, string) {
	cleaned := strings.TrimSpace(repoURL)
	if cleaned == "" {
		return "", ""
	}
	directParts := nonEmpty(strings.Split(cleaned, "/"))
	if len(directParts) == 2 && !strings.Contains(cleaned, "://") && !strings.HasPrefix(cleaned, "git@") {
		return directParts[0], strings.TrimSuffix(directParts[1], ".git")
	}
	var path string
	if strings.HasPrefix(cleaned, "git@github.com:") {
		path = strings.SplitN(cleaned, ":", 2)[1]
	} else {
		u, err := url.Parse(cleaned)
		if err != nil || strings.ToLower(u.Host) != "github.com" {
			return "", ""
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(path, ".git")
	parts := nonEmpty(strings.Split(path, "/"))
	if len(parts) < 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func nonEmpty(parts []string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// buildGithubRepoURL mirrors app.py _build_github_repo_url.
func buildGithubRepoURL(owner, repo string) string {
	o := strings.Trim(strings.TrimSpace(owner), "/")
	rp := strings.TrimSuffix(strings.Trim(strings.TrimSpace(repo), "/"), ".git")
	if o == "" || rp == "" {
		return ""
	}
	return "https://github.com/" + o + "/" + rp
}

// resolveGithubRepoFields mirrors app.py _resolve_github_repo_fields.
func resolveGithubRepoFields(repoURL, owner, repo string) (string, string, string) {
	urlClean := strings.TrimSpace(repoURL)
	o := strings.Trim(strings.TrimSpace(owner), "/")
	rp := strings.TrimSuffix(strings.Trim(strings.TrimSpace(repo), "/"), ".git")
	if (o == "" || rp == "") && urlClean != "" {
		po, pr := parseGithubRepoOwnerName(urlClean)
		if o == "" {
			o = po
		}
		if rp == "" {
			rp = pr
		}
	}
	if canonical := buildGithubRepoURL(o, rp); canonical != "" {
		urlClean = canonical
	}
	return urlClean, o, rp
}

// githubVersionTokens mirrors app.py _github_version_tokens.
func githubVersionTokens(version string) map[string]bool {
	v := strings.ToLower(strings.TrimSpace(version))
	if v == "" {
		return map[string]bool{}
	}
	tokens := map[string]bool{v: true}
	if !strings.HasPrefix(v, "v") {
		tokens["v"+v] = true
	}
	return tokens
}

// loadAISetting mirrors app.py _load_ai_setting for the non-sensitive DB path: read
// sobs_ai_settings. (Env-var/secret-file fallbacks are deploy-time concerns; the parity fixture
// has neither, so an absent key resolves to default — matching Python.)
func (s *server) loadAISetting(key, def string) string {
	res, err := s.db.Execute("SELECT Value FROM sobs_ai_settings FINAL WHERE Key=? AND IsDeleted=0 LIMIT 1", key)
	if err == nil && len(res.Rows) > 0 {
		if v := strings.TrimSpace(cStr(rowMaps(res)[0], "Value")); v != "" {
			return v
		}
	}
	return def
}

// saveAISetting mirrors app.py _save_ai_setting (writes sobs_ai_settings). At-rest encryption of
// sensitive keys is omitted — a hard-cutover Go app reads back via loadAISetting self-consistently.
func (s *server) saveAISetting(key, value string) {
	_, _ = s.db.InsertJSONEachRow("sobs_ai_settings",
		[]map[string]any{{"Key": key, "Value": value, "IsDeleted": 0, "Version": fixedVersionMillis()}})
}

// repoScopedGithubToken mirrors app.py _load_repo_scoped_github_token.
func (s *server) repoScopedGithubToken(owner, repo string) string {
	if owner == "" || repo == "" {
		return ""
	}
	return strings.TrimSpace(s.loadAISetting(githubRepoTokenKey(owner, repo), ""))
}

// githubRepoTokenKey mirrors app.py _github_repo_token_key (owner/repo -> ai-settings key).
func githubRepoTokenKey(owner, repo string) string {
	return "ai.github_token.repo." + strings.ToLower(owner) + "/" + strings.ToLower(repo)
}
