package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var githubExpiryDateOnlyRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ciPushSettingKey mirrors app.py _ci_push_setting_key.
func ciPushSettingKey(appID, leaf string) string {
	return "ai.ci_push.app." + strings.ToLower(strings.TrimSpace(appID)) + "." + leaf
}

// normalizeGithubTokenExpiry mirrors app.py _normalize_github_token_expiry_input: a bare date
// becomes end-of-day UTC; a full ISO timestamp is normalized; anything else -> "".
func normalizeGithubTokenExpiry(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ""
	}
	if githubExpiryDateOnlyRe.MatchString(raw) {
		return raw + "T23:59:59+00:00"
	}
	if t, ok := parseISODatetime(raw); ok {
		return t.Format("2006-01-02T15:04:05-07:00")
	}
	return ""
}

// parseISODatetime mirrors app.py _parse_iso_datetime (UTC-normalized, or ok=false).
func parseISODatetime(value string) (time.Time, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return time.Time{}, false
	}
	if strings.HasSuffix(raw, "Z") {
		raw = raw[:len(raw)-1] + "+00:00"
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999-07:00", "2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05",
		"2006-01-02 15:04:05-07:00", "2006-01-02 15:04:05", "2006-01-02",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// githubTokenExpiryStatus mirrors app.py _github_token_expiry_status.
func githubTokenExpiryStatus(expiresAt string, warningDays int) map[string]any {
	parsed, ok := parseISODatetime(expiresAt)
	if !ok {
		return map[string]any{"state": "unknown", "expires_at": "", "days_remaining": nil, "message": "Token expiry date not set"}
	}
	secondsRemaining := int(parsed.Sub(nowUTC()).Seconds())
	daysRemaining := floorDiv(secondsRemaining, 86400)
	iso := parsed.Format("2006-01-02T15:04:05-07:00")
	switch {
	case secondsRemaining < 0:
		return map[string]any{"state": "expired", "expires_at": iso, "days_remaining": daysRemaining,
			"message": fmt.Sprintf("Token expired on %s", parsed.Format("2006-01-02"))}
	case daysRemaining <= warningDays:
		return map[string]any{"state": "warning", "expires_at": iso, "days_remaining": daysRemaining,
			"message": fmt.Sprintf("Token expires in %d day(s)", daysRemaining)}
	default:
		return map[string]any{"state": "healthy", "expires_at": iso, "days_remaining": daysRemaining,
			"message": fmt.Sprintf("Token healthy (%d day(s) remaining)", daysRemaining)}
	}
}

// githubTokenExpiryDateInputValue mirrors app.py _github_token_expiry_date_input_value.
func githubTokenExpiryDateInputValue(value string) string {
	if parsed, ok := parseISODatetime(value); ok {
		return parsed.Format("2006-01-02")
	}
	return ""
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// ciPushStatus mirrors app.py _ci_push_api_key_status.
func (s *server) ciPushStatus(appID string) map[string]any {
	id := strings.TrimSpace(appID)
	if id == "" {
		return map[string]any{
			"app_id": "", "configured": false, "expires_at": "", "rotated_at": "", "hash": "",
			"realtime_enabled": false,
			"expiry":           map[string]any{"state": "missing", "expires_at": "", "days_remaining": nil, "message": "CI push API key not configured"},
		}
	}
	keyHash := strings.TrimSpace(s.loadAISetting(ciPushSettingKey(id, "hash"), ""))
	expiresAt := strings.TrimSpace(s.loadAISetting(ciPushSettingKey(id, "expires_at"), ""))
	rotatedAt := strings.TrimSpace(s.loadAISetting(ciPushSettingKey(id, "rotated_at"), ""))
	realtime := strings.ToLower(strings.TrimSpace(s.loadAISetting(ciPushSettingKey(id, "realtime_enabled"), "false")))
	realtimeEnabled := realtime == "1" || realtime == "true" || realtime == "yes"

	expiry := githubTokenExpiryStatus(expiresAt, 14)
	if keyHash == "" {
		expiry = map[string]any{"state": "missing", "expires_at": "", "days_remaining": nil, "message": "CI push API key not configured"}
	}
	return map[string]any{
		"app_id": id, "configured": keyHash != "", "expires_at": expiresAt, "rotated_at": rotatedAt,
		"hash": keyHash, "realtime_enabled": realtimeEnabled, "expiry": expiry,
	}
}

// buildRepositoriesApps mirrors the per-app list in app.py view_settings_repositories.
func (s *server) buildRepositoriesApps() []any {
	appRes, err := s.db.Execute("SELECT * FROM sobs_apps FINAL WHERE IsDeleted=0 ORDER BY Name ASC")
	if err != nil {
		return []any{}
	}
	releasesByApp := map[string][]string{}
	if relRes, err := s.db.Execute("SELECT AppId, ReleaseVersion, ReleasedAt FROM sobs_app_releases FINAL " +
		"WHERE IsDeleted=0 ORDER BY ReleasedAt DESC LIMIT 5000"); err == nil {
		for _, m := range rowMaps(relRes) {
			appID := cStr(m, "AppId")
			ver := strings.TrimSpace(cStr(m, "ReleaseVersion"))
			if appID == "" || ver == "" {
				continue
			}
			if !containsStr(releasesByApp[appID], ver) {
				releasesByApp[appID] = append(releasesByApp[appID], ver)
			}
		}
	}

	apps := []any{}
	for _, row := range rowMaps(appRes) {
		id := cStr(row, "Id")
		enabled := true
		if _, ok := row["Enabled"]; ok {
			enabled = cBool(row, "Enabled")
		}
		owner, repo := parseGithubRepoOwnerName(cStr(row, "RepoUrl"))
		repoTokenConfigured := owner != "" && repo != "" && s.repoScopedGithubToken(owner, repo) != ""
		versions := releasesByApp[id]
		latest := versions
		if len(latest) > 5 {
			latest = latest[:5]
		}
		latestAny := make([]any, len(latest))
		for i, v := range latest {
			latestAny[i] = v
		}
		apps = append(apps, map[string]any{
			"id": id, "name": cStr(row, "Name"), "slug": cStr(row, "Slug"),
			"repo_url": cStr(row, "RepoUrl"), "repo_owner": owner, "repo_name": repo,
			"enabled": enabled, "release_count": len(versions), "latest_versions": latestAny,
			"repo_token_configured": repoTokenConfigured,
			"ci_push_status":        s.ciPushStatus(id), "ci_push_plain": "",
		})
	}
	return apps
}
