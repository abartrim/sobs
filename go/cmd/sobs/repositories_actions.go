package main

import (
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"os"
	"strconv"
	"strings"
)

const reposURL = "/settings/repositories"

const (
	ciPushDefaultTTLDays = 30
	ciPushMinTTLDays     = 1
	ciPushMaxTTLDays     = 365
	ciPushHashPrefix     = "scrypt:v1:" // matches app.py _CI_PUSH_HASH_PREFIX (keyed scrypt fingerprint)
)

// normalizeTTLDays mirrors _normalize_ttl_days.
func normalizeTTLDays(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		parsed = ciPushDefaultTTLDays
	}
	if parsed < ciPushMinTTLDays {
		return ciPushMinTTLDays
	}
	if parsed > ciPushMaxTTLDays {
		return ciPushMaxTTLDays
	}
	return parsed
}

// generateCiPushAPIKey mirrors _generate_ci_push_api_key (sobs_ci_ + url-safe random).
func generateCiPushAPIKey() string {
	return "sobs_ci_" + base64.RawURLEncoding.EncodeToString(randBytes(24))
}

// ciPushHashSalt mirrors app.py _ci_push_hash_key: a per-installation 32-byte salt derived from
// SOBS_SECRET_KEY via personalized BLAKE2b (person="sobs-ci-hash-v1"). The secret defaults to
// "sobs-dev-secret-key" only when the env var is ABSENT — matching os.environ.get's default, which
// honors an explicitly-empty value rather than substituting it (so LookupEnv, not aiActionTokenSecret
// which trims+defaults on empty).
func ciPushHashSalt() []byte {
	secret, ok := os.LookupEnv("SOBS_SECRET_KEY")
	if !ok {
		secret = "sobs-dev-secret-key"
	}
	return blake2bPersonalSum([]byte(secret), []byte("sobs-ci-hash-v1"), 32)
}

// hashAPIKey mirrors app.py _hash_api_key: a keyed, memory-hard fingerprint for CI push API keys —
// scrypt(key, salt=ciPushHashSalt, n=1024, r=8, p=1, dklen=32) as hex, prefixed "scrypt:v1:". It is
// byte-exact with Python (reusing mcp_scrypt.go's RFC 7914 scrypt core), so a per-app key rotated
// under either runtime validates under the other. The hash is stored in ai-settings and never
// returned in a response.
func hashAPIKey(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ""
	}
	digest := scryptKey([]byte(raw), ciPushHashSalt(), 1024, 8, 1, 32)
	return ciPushHashPrefix + hex.EncodeToString(digest)
}

// ciPushExpiryISOFromDays mirrors _ci_push_expiry_iso_from_days (now + ttl, clamped to 23:59:59).
func ciPushExpiryISOFromDays(ttlDays int) string {
	expires := nowUTC().AddDate(0, 0, ttlDays)
	expires = expires.Truncate(24 * 3600 * 1e9) // midnight
	expires = expires.Add((23*3600 + 59*60 + 59) * 1e9)
	return expires.Format("2006-01-02T15:04:05-07:00")
}

func (s *server) setCiPushRealtimeEnabled(appID string, enabled bool) {
	if strings.TrimSpace(appID) == "" {
		return
	}
	v := "false"
	if enabled {
		v = "true"
	}
	s.saveAISetting(ciPushSettingKey(appID, "realtime_enabled"), v)
}

// repoRealtimeMode mirrors save_settings_repository_realtime_mode.
func (s *server) repoRealtimeMode(w http.ResponseWriter, r *http.Request, appID string, current map[string]any) {
	enabled := r.PostFormValue("realtime_enabled") != ""
	s.setCiPushRealtimeEnabled(appID, enabled)
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	flashRedirect(w, "success", "Realtime CI support "+state+" for "+strings.TrimSpace(cStr(current, "Name")), reposURL)
}

// rotateCiPushKey mirrors _rotate_ci_push_api_key: generate a fresh CI-push key, store its hash +
// expiry + rotated-at, enable realtime, and return (plaintext key, expiry ISO).
func (s *server) rotateCiPushKey(appID string, ttlDays int) (string, string) {
	if strings.TrimSpace(appID) == "" {
		return "", ""
	}
	keyPlain := generateCiPushAPIKey()
	expiresAt := ciPushExpiryISOFromDays(normalizeTTLDaysInt(ttlDays))
	s.saveAISetting(ciPushSettingKey(appID, "hash"), hashAPIKey(keyPlain))
	s.saveAISetting(ciPushSettingKey(appID, "expires_at"), expiresAt)
	s.saveAISetting(ciPushSettingKey(appID, "rotated_at"), nowISO())
	return keyPlain, expiresAt
}

func normalizeTTLDaysInt(ttlDays int) int {
	if ttlDays < ciPushMinTTLDays {
		return ciPushMinTTLDays
	}
	if ttlDays > ciPushMaxTTLDays {
		return ciPushMaxTTLDays
	}
	return ttlDays
}

// repoCiKeyRotate mirrors rotate_settings_repository_ci_ingest_key (the plaintext key is stashed in
// the session for one-time display; the session cookie is therefore non-deterministic and masked).
func (s *server) repoCiKeyRotate(w http.ResponseWriter, r *http.Request, appID string, current map[string]any) {
	ttlDays := normalizeTTLDays(r.PostFormValue("ttl_days"))
	keyPlain, expiresAt := s.rotateCiPushKey(appID, ttlDays)
	s.setCiPushRealtimeEnabled(appID, true)
	expiresDate := expiresAt
	if len(expiresDate) >= 10 {
		expiresDate = expiresDate[:10]
	}
	msg := "CI ingest API key rotated for " + strings.TrimSpace(cStr(current, "Name")) +
		" (expires " + expiresDate + "). Copy the key now; it is shown once."
	flashRedirectWithCiKey(w, "success", msg, reposURL, appID, keyPlain)
}

// repoCiKeyRevoke mirrors revoke_settings_repository_ci_ingest_key.
func (s *server) repoCiKeyRevoke(w http.ResponseWriter, appID string, current map[string]any) {
	s.saveAISetting(ciPushSettingKey(appID, "hash"), "")
	s.saveAISetting(ciPushSettingKey(appID, "expires_at"), "")
	s.saveAISetting(ciPushSettingKey(appID, "rotated_at"), nowISO())
	flashRedirect(w, "success", "CI ingest API key revoked for "+strings.TrimSpace(cStr(current, "Name")), reposURL)
}

// repoAddRelease mirrors add_settings_repository_release.
func (s *server) repoAddRelease(w http.ResponseWriter, r *http.Request, appID string, _ map[string]any) {
	releaseVersion := strings.TrimSpace(r.PostFormValue("version"))
	environment := strings.TrimSpace(r.PostFormValue("environment"))
	if releaseVersion == "" {
		flashRedirect(w, "warning", "Release version is required", reposURL)
		return
	}
	_, _ = s.insertRowsNormalized("sobs_app_releases", []map[string]any{{
		"Id": newUUIDHex(), "AppId": appID, "ReleaseVersion": releaseVersion,
		"CommitSha": "", "BuildId": "", "Environment": environment, "ReleasedAt": nowISO(),
		"MetadataJson": "{}", "IsDeleted": 0, "Version": fixedVersionMillis(),
	}})
	flashRedirect(w, "success", "Release added", reposURL)
}

// repoUpdate mirrors update_settings_repository (re-insert the app row with the resolved repo URL).
func (s *server) repoUpdate(w http.ResponseWriter, r *http.Request, appID string, current map[string]any) {
	repoURL, owner, repo := resolveGithubRepoFields(
		strings.TrimSpace(r.PostFormValue("repo_url")),
		strings.TrimSpace(r.PostFormValue("repo_owner")),
		strings.TrimSpace(r.PostFormValue("repo_name")))
	repoToken := strings.TrimSpace(r.PostFormValue("repo_token"))
	setRepoToken := r.PostFormValue("set_repo_token") != ""
	if repoURL == "" {
		flashRedirect(w, "warning", "Repository is required", reposURL)
		return
	}
	createdAt := cStr(current, "CreatedAt")
	if createdAt == "" {
		createdAt = nowISO()
	}
	_, _ = s.insertRowsNormalized("sobs_apps", []map[string]any{{
		"Id": appID, "Name": cStr(current, "Name"), "Slug": cStr(current, "Slug"),
		"OwnerTeam": cStr(current, "OwnerTeam"), "RepoUrl": repoURL,
		"DefaultEnvironment": cStr(current, "DefaultEnvironment"), "Enabled": cInt(current, "Enabled"),
		"MetadataJson": metadataJSONOr(current), "IsDeleted": 0, "Version": fixedVersionMillis(),
		"CreatedAt": createdAt, "UpdatedAt": nowISO(),
	}})
	if setRepoToken && repoToken != "" && owner != "" && repo != "" {
		s.saveAISetting(githubRepoTokenKey(owner, repo), repoToken)
	}
	flashRedirect(w, "success", "Repository updated", reposURL)
}

// repoDelete mirrors delete_settings_repository (tombstone the app + cascade its releases).
func (s *server) repoDelete(w http.ResponseWriter, appID string, current map[string]any) {
	createdAt := cStr(current, "CreatedAt")
	if createdAt == "" {
		createdAt = nowISO()
	}
	ver := fixedVersionMillis()
	_, _ = s.insertRowsNormalized("sobs_apps", []map[string]any{{
		"Id": appID, "Name": cStr(current, "Name"), "Slug": cStr(current, "Slug"),
		"OwnerTeam": cStr(current, "OwnerTeam"), "RepoUrl": cStr(current, "RepoUrl"),
		"DefaultEnvironment": cStr(current, "DefaultEnvironment"), "Enabled": cInt(current, "Enabled"),
		"MetadataJson": metadataJSONOr(current), "IsDeleted": 1, "Version": ver,
		"CreatedAt": createdAt, "UpdatedAt": nowISO(),
	}})
	if res, err := s.db.Execute("SELECT * FROM sobs_app_releases FINAL WHERE AppId=? AND IsDeleted=0", appID); err == nil {
		tombstones := []map[string]any{}
		for _, rel := range rowMaps(res) {
			tombstones = append(tombstones, map[string]any{
				"Id": cStr(rel, "Id"), "AppId": cStr(rel, "AppId"), "ReleaseVersion": cStr(rel, "ReleaseVersion"),
				"CommitSha": cStr(rel, "CommitSha"), "BuildId": cStr(rel, "BuildId"),
				"Environment": cStr(rel, "Environment"), "ReleasedAt": cStr(rel, "ReleasedAt"),
				"MetadataJson": cStr(rel, "MetadataJson"), "IsDeleted": 1, "Version": ver,
			})
		}
		if len(tombstones) > 0 {
			_, _ = s.insertRowsNormalized("sobs_app_releases", tombstones)
		}
	}
	flashRedirect(w, "success", "Repository '"+cStr(current, "Name")+"' deleted", reposURL)
}

// repoGithubTokenValidate mirrors validate_settings_repository_github_token.
func (s *server) repoGithubTokenValidate(w http.ResponseWriter, _ *http.Request) {
	token := strings.TrimSpace(s.loadAISetting("ai.github_token", ""))
	if token == "" {
		flashRedirect(w, "warning", "No GitHub token configured to validate", reposURL)
		return
	}
	status, message := s.validateGithubToken(token)
	s.saveAISetting("ai.github_token_last_validated_at", nowISO())
	s.saveAISetting("ai.github_token_last_validation_status", status)
	s.saveAISetting("ai.github_token_last_validation_message", message)
	category := "warning"
	if status == "valid" {
		category = "success"
	}
	flashRedirect(w, category, "GitHub token validation: "+message, reposURL)
}

// validateGithubToken mirrors _validate_github_token (GET api.github.com/rate_limit).
func (s *server) validateGithubToken(token string) (string, string) {
	if strings.TrimSpace(token) == "" {
		return "missing", "No token configured"
	}
	resp, err := s.upstreamRequest("GET", "https://api.github.com/rate_limit", nil, githubAPIHeaders(token, false, nil))
	if err != nil {
		return "error", "Validation request failed: " + err.Error()
	}
	switch resp.Status {
	case 200:
		return "valid", "Token is valid"
	case 401:
		return "invalid", "Token rejected (401 Unauthorized)"
	case 403:
		return "error", "GitHub returned 403 (forbidden or rate-limited)"
	default:
		return "error", "GitHub returned HTTP " + strconv.Itoa(resp.Status)
	}
}

// metadataJSONOr mirrors str(current.get("MetadataJson","{}") or "{}").
func metadataJSONOr(current map[string]any) string {
	if v := cStr(current, "MetadataJson"); v != "" {
		return v
	}
	return "{}"
}
