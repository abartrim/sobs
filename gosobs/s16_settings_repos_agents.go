package main

// s16_settings_repos_agents.go — port of app.py lines 26599-28767:
// health checks (/health, /health/db), AI settings pages (/settings/ai),
// enrichment settings (/settings/enrichment), GitHub repository management
// (/settings/repositories*), agent rules pages (/settings/agents*), the AI
// contextual helper API (/api/ai/helper*), the agent runs API
// (/api/agent/runs*), and the user-raised-issue trigger (/api/issues/raise).

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

func init() {
	// Health checks are intentionally unauthenticated.
	registerRoute("GET", "/health", health)
	registerRoute("GET", "/health/db", healthDb)

	registerRoute("GET", "/settings/ai", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(viewAiSettings)(w, r)
	})
	registerRoute("POST", "/settings/ai", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(saveAiSettings)(w, r)
	})

	registerRoute("GET", "/settings/enrichment", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(viewEnrichmentSettings)(w, r)
	})
	registerRoute("POST", "/settings/enrichment", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(saveEnrichmentSettings)(w, r)
	})

	registerRoute("GET", "/settings/repositories", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(viewSettingsRepositories)(w, r)
	})
	registerRoute("POST", "/settings/repositories", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(createSettingsRepository)(w, r)
	})
	registerRoute("POST", "/settings/repositories/github-token/validate", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(validateSettingsRepositoryGithubToken)(w, r)
	})
	registerRoute("POST", "/settings/repositories/{app_id}/realtime-mode", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(saveSettingsRepositoryRealtimeMode)(w, r)
	})
	registerRoute("POST", "/settings/repositories/{app_id}/ci-ingest-key/rotate", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(rotateSettingsRepositoryCiIngestKey)(w, r)
	})
	registerRoute("POST", "/settings/repositories/{app_id}/ci-ingest-key/revoke", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(revokeSettingsRepositoryCiIngestKey)(w, r)
	})
	registerRoute("POST", "/settings/repositories/{app_id}", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(updateSettingsRepository)(w, r)
	})
	registerRoute("POST", "/settings/repositories/{app_id}/releases", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(addSettingsRepositoryRelease)(w, r)
	})
	registerRoute("POST", "/settings/repositories/{app_id}/delete", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(deleteSettingsRepository)(w, r)
	})

	registerRoute("GET", "/settings/agents", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(viewAgentRules)(w, r)
	})
	registerRoute("POST", "/settings/agents", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(createAgentRule)(w, r)
	})
	registerRoute("POST", "/settings/agents/{rule_id}/delete", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(deleteAgentRule)(w, r)
	})

	registerRoute("GET", "/api/ai/helper/capabilities", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(aiHelperCapabilities)(w, r)
	})
	registerRoute("GET", "/api/ai/helper/actions/manifest", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(aiHelperActionManifest)(w, r)
	})
	registerRoute("GET", "/api/ai/helper/chats", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(aiHelperChats)(w, r)
	})
	registerRoute("GET", "/api/ai/helper/chats/{chat_id}", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(aiHelperChatDetail)(w, r)
	})
	registerRoute("POST", "/api/ai/helper/feedback", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(aiHelperFeedback)(w, r)
	})
	registerRoute("POST", "/api/ai/helper", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(aiHelper)(w, r)
	})
	registerRoute("POST", "/api/ai/helper/actions/execute", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(aiHelperExecuteAction)(w, r)
	})

	registerRoute("POST", "/api/issues/raise", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(raiseIssueFromUserObservation)(w, r)
	})
	registerRoute("GET", "/api/agent/runs", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(listAgentRuns)(w, r)
	})
	registerRoute("POST", "/api/agent/runs", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(triggerAgentRun)(w, r)
	})
	registerRoute("POST", "/api/agent/runs/{run_id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(dismissAgentRun)(w, r)
	})
}

// ---------------------------------------------------------------------------
// Session stand-in for the one-shot CI push API key plaintext map.
//
// PORT-NOTE: Quart's signed session dict has no Go equivalent here. The single
// session key used in this section (ci_push_api_key_plain_by_app, a one-time
// "shown once" plaintext key map) is mirrored via an unsigned JSON cookie, the
// same pattern used by the flash-cookie stand-in in s00_core.go.
// ---------------------------------------------------------------------------

const ciPushPlainCookieName = "sobs_ci_push_plain"

func ciPushPlainSessionRead(r *http.Request) map[string]string {
	out := map[string]string{}
	c, err := r.Cookie(ciPushPlainCookieName)
	if err != nil {
		return out
	}
	raw, err := base64.URLEncoding.DecodeString(c.Value)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	if out == nil {
		out = map[string]string{}
	}
	return out
}

func ciPushPlainSessionWrite(w http.ResponseWriter, value map[string]string) {
	raw, _ := json.Marshal(value)
	http.SetCookie(w, &http.Cookie{
		Name:     ciPushPlainCookieName,
		Value:    base64.URLEncoding.EncodeToString(raw),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func ciPushPlainSessionClear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     ciPushPlainCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ---------------------------------------------------------------------------
// Health check
// ---------------------------------------------------------------------------

func health(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{"status": "ok", "version": "1.0.0"})
}

func healthDb(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	dbErr := ensureDbSchema()
	if dbErr == nil {
		if _, err := getDb().Execute("SELECT 1"); err != nil {
			dbErr = err
		}
	}
	if dbErr != nil {
		logger.Error("DB readiness probe failed", "error", dbErr)
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{
			"status":            "degraded",
			"db":                "error",
			"error":             "database unavailable",
			"write_queue_depth": writeQueueDepth(),
			"version":           "1.0.0",
		})
		return
	}

	latencyMs := math.Round(float64(time.Since(started).Microseconds())/1000.0*100) / 100
	jsonResponse(w, http.StatusOK, map[string]any{
		"status":            "ok",
		"db":                "ok",
		"latency_ms":        latencyMs,
		"write_queue_depth": writeQueueDepth(),
		"version":           "1.0.0",
	})
}

// ---------------------------------------------------------------------------
// AI Settings  GET/POST /settings/ai
// ---------------------------------------------------------------------------

func viewAiSettings(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	settings := loadAllAiSettings(db)
	aiPricing, aiPricingSources := loadAiPricingWithSources(db)
	anomalyRules, _ := loadAnomalyRules(db)
	tagRules, _ := loadTagRules(db)
	tokenExpiryStatus := githubTokenExpiryStatus(strings.TrimSpace(settings["ai.github_token_expires_at"]))
	tokenValidationStatus := map[string]any{
		"status":            strings.TrimSpace(settings["ai.github_token_last_validation_status"]),
		"message":           strings.TrimSpace(settings["ai.github_token_last_validation_message"]),
		"last_validated_at": strings.TrimSpace(settings["ai.github_token_last_validated_at"]),
	}
	confirmedModels := loadConfirmedAiPricingModels(db)
	confirmedSorted := make([]string, 0, len(confirmedModels))
	for k := range confirmedModels {
		confirmedSorted = append(confirmedSorted, k)
	}
	sort.Strings(confirmedSorted)
	renderTemplate(w, r, "settings_ai.html", map[string]any{
		"settings":      settings,
		"anomaly_rules": anomalyRules,
		"tag_rules":     tagRules,
		"github_token_expires_date": githubTokenExpiryDateInputValue(
			strings.TrimSpace(settings["ai.github_token_expires_at"]),
		),
		"github_token_expiry_status":     tokenExpiryStatus,
		"github_token_validation_status": tokenValidationStatus,
		"default_ai_pricing":             defaultAiPricing,
		"saved_ai_pricing":               aiPricing,
		"ai_pricing_sources":             aiPricingSources,
		"confirmed_ai_pricing_models":    confirmedSorted,
	})
}

// aiSettingsSaveSkipKeys mirrors the in-loop skip set in save_ai_settings.
var aiSettingsSaveSkipKeys = map[string]bool{
	"ai.guard_thinking_level":                 true,
	"ai.guard_timeout_seconds":                true,
	"ai.github_token_expires_at":              true,
	"ai.github_token_last_validated_at":       true,
	"ai.github_token_last_validation_status":  true,
	"ai.github_token_last_validation_message": true,
	"ai.model_pricing":                        true, // handled separately with JSON validation below
	"ai.model_pricing_confirmed":              true, // handled separately with JSON validation below
}

func saveAiSettings(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	db := getDb()
	previousToken := strings.TrimSpace(loadAiSetting(db, "ai.github_token", ""))
	for _, key := range aiSettingKeys {
		if aiSettingsSaveSkipKeys[key] {
			// Guard thinking is intentionally not user-configured via the Settings UI.
			continue
		}
		// Strip key prefix for form field name: "ai.endpoint_url" → "endpoint_url"
		field := strings.TrimPrefix(key, "ai.")
		value := strings.TrimSpace(r.FormValue(field))
		saveAiSetting(db, key, value)
	}

	// Validate and save model pricing JSON
	rawPricing := strings.TrimSpace(r.FormValue("model_pricing"))
	clean := map[string]map[string]float64{}
	if rawPricing != "" {
		var parsed any
		if err := json.Unmarshal([]byte(rawPricing), &parsed); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "Invalid model_pricing JSON"})
			return
		}
		parsedMap, ok := parsed.(map[string]any)
		if !ok {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "Invalid model_pricing JSON"})
			return
		}
		// Re-serialize to normalize whitespace and strip invalid entries.
		for modelKey, prices := range parsedMap {
			normalizedKey := normalizeAiModelName(modelKey)
			entry := coerceAiPricingEntry(prices)
			if normalizedKey != "" && len(entry) > 0 {
				clean[normalizedKey] = entry
			}
		}
		// PORT-NOTE: Go map iteration is unordered; the normalized JSON re-emits
		// pricing keys in arbitrary order (Python preserved dict insertion order).
		saveAiSetting(db, "ai.model_pricing", llmJsonDumps(clean))
	} else {
		saveAiSetting(db, "ai.model_pricing", "")
	}

	rawConfirmedModels := strings.TrimSpace(r.FormValue("model_pricing_confirmed"))
	if rawConfirmedModels != "" {
		var parsedConfirmed any
		if err := json.Unmarshal([]byte(rawConfirmedModels), &parsedConfirmed); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "Invalid model_pricing_confirmed JSON"})
			return
		}
		confirmedList, ok := parsedConfirmed.([]any)
		if !ok {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "Invalid model_pricing_confirmed JSON"})
			return
		}
		confirmedModels := []string{}
		seenConfirmed := map[string]bool{}
		for _, modelKey := range confirmedList {
			normalizedKey := normalizeAiModelName(modelKey)
			if normalizedKey == "" || seenConfirmed[normalizedKey] {
				continue
			}
			if _, ok := clean[normalizedKey]; !ok {
				continue
			}
			seenConfirmed[normalizedKey] = true
			confirmedModels = append(confirmedModels, normalizedKey)
		}
		saveAiSetting(db, "ai.model_pricing_confirmed", llmJsonDumps(confirmedModels))
	} else {
		saveAiSetting(db, "ai.model_pricing_confirmed", "")
	}

	githubToken := strings.TrimSpace(r.FormValue("github_token"))
	githubTokenExpiry := normalizeGithubTokenExpiryInput(r.FormValue("github_token_expires_at"))
	if githubToken != "" {
		saveAiSetting(db, "ai.github_token_expires_at", githubTokenExpiry)
	} else {
		saveAiSetting(db, "ai.github_token_expires_at", "")
	}

	if githubToken != previousToken {
		saveAiSetting(db, "ai.github_token_last_validated_at", "")
		saveAiSetting(db, "ai.github_token_last_validation_status", "")
		saveAiSetting(db, "ai.github_token_last_validation_message", "")
	}

	flashMessage(w, r, "AI settings saved", "success")
	http.Redirect(w, r, "/settings/ai", http.StatusFound)
}

// ---------------------------------------------------------------------------
// Enrichment Settings  GET/POST /settings/enrichment
// ---------------------------------------------------------------------------

func viewEnrichmentSettings(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	geoEnabled := isEnrichmentToggleEnabled(getAppSetting(db, geoEnabledSetting))
	cveEnabled := isEnrichmentToggleEnabled(getAppSetting(db, cveEnabledSetting))
	cveLastScan := getAppSetting(db, cveLastScanSetting)
	renderTemplate(w, r, "settings_enrichment.html", map[string]any{
		"geo_enabled":                        geoEnabled,
		"cve_enabled":                        cveEnabled,
		"cve_last_scan":                      cveLastScan,
		"github_backfill_max_releases":       githubBackfillMaxReleases(db),
		"github_backfill_min_releases":       githubBackfillMaxReleasesMin,
		"github_backfill_max_releases_limit": githubBackfillMaxReleasesMax,
	})
}

// isEnrichmentToggleEnabled mirrors (_get_app_setting(...) or "true").lower() in
// ("1","true","yes").
func isEnrichmentToggleEnabled(raw string) bool {
	if raw == "" {
		raw = "true"
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func saveEnrichmentSettings(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	db := getDb()
	geoEnabled := "false"
	if r.FormValue("geo_enabled") != "" {
		geoEnabled = "true"
	}
	setAppSetting(db, geoEnabledSetting, geoEnabled)
	cveEnabled := "false"
	if r.FormValue("cve_enabled") != "" {
		cveEnabled = "true"
	}
	setAppSetting(db, cveEnabledSetting, cveEnabled)

	githubBackfillMaxReleasesVal := githubBackfillMaxReleasesDefault
	raw := strings.TrimSpace(r.FormValue("github_backfill_max_releases"))
	if raw == "" {
		raw = strconv.Itoa(githubBackfillMaxReleasesDefault)
	}
	if v, err := strconv.Atoi(raw); err == nil {
		githubBackfillMaxReleasesVal = v
	} else {
		githubBackfillMaxReleasesVal = githubBackfillMaxReleasesDefault
	}
	githubBackfillMaxReleasesVal = max(
		githubBackfillMaxReleasesMin,
		min(githubBackfillMaxReleasesMax, githubBackfillMaxReleasesVal),
	)
	setAppSetting(db, githubBackfillMaxReleasesSetting, strconv.Itoa(githubBackfillMaxReleasesVal))
	flashMessage(w, r, "Enrichment settings saved", "success")
	http.Redirect(w, r, "/settings/enrichment", http.StatusFound)
}

// ---------------------------------------------------------------------------
// GitHub Repository Management  GET/POST /settings/repositories
// ---------------------------------------------------------------------------

func viewSettingsRepositories(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	aiSettings := loadAllAiSettings(db)
	ciPushPlainByApp := ciPushPlainSessionRead(r)
	ciPushPlainSessionClear(w)
	githubTokenExpiresAt := strings.TrimSpace(aiSettings["ai.github_token_expires_at"])
	tokenExpiryStatus := githubTokenExpiryStatus(githubTokenExpiresAt)
	tokenValidationStatus := map[string]any{
		"status":            strings.TrimSpace(aiSettings["ai.github_token_last_validation_status"]),
		"message":           strings.TrimSpace(aiSettings["ai.github_token_last_validation_message"]),
		"last_validated_at": strings.TrimSpace(aiSettings["ai.github_token_last_validated_at"]),
	}

	appRowsRes, err := db.Execute("SELECT * FROM sobs_apps FINAL WHERE IsDeleted=0 ORDER BY Name ASC")
	var appRows []Row
	if err != nil {
		logger.Error("viewSettingsRepositories: apps query failed", "error", err)
	} else {
		appRows = appRowsRes.Fetchall()
	}
	releaseRowsRes, rerr := db.Execute(
		"SELECT AppId, ReleaseVersion, ReleasedAt " +
			"FROM sobs_app_releases FINAL " +
			"WHERE IsDeleted=0 " +
			"ORDER BY ReleasedAt DESC LIMIT 5000")
	var releaseRows []Row
	if rerr != nil {
		logger.Error("viewSettingsRepositories: releases query failed", "error", rerr)
	} else {
		releaseRows = releaseRowsRes.Fetchall()
	}

	releasesByApp := map[string][]string{}
	for _, row := range releaseRows {
		appId := rowString(row["AppId"])
		version := strings.TrimSpace(rowString(row["ReleaseVersion"]))
		if appId == "" || version == "" {
			continue
		}
		versions := releasesByApp[appId]
		found := false
		for _, v := range versions {
			if v == version {
				found = true
				break
			}
		}
		if !found {
			releasesByApp[appId] = append(versions, version)
		}
	}

	apps := []map[string]any{}
	for _, row := range appRows {
		app := serializeAppRow(row)
		appId := rowString(app["id"])
		appVersions := releasesByApp[appId]
		_, owner, repo := resolveGithubRepoFields(rowString(app["repoUrl"]), "", "")
		repoTokenConfigured := false
		if owner != "" && repo != "" {
			repoTokenConfigured = loadRepoScopedGithubToken(db, owner, repo) != ""
		}
		ciPushStatus := ciPushApiKeyStatus(db, appId)
		ciPushPlain := ciPushPlainByApp[appId]
		latestVersions := appVersions
		if len(latestVersions) > 5 {
			latestVersions = latestVersions[:5]
		}
		apps = append(apps, map[string]any{
			"id":                    appId,
			"name":                  rowString(app["name"]),
			"slug":                  rowString(app["slug"]),
			"repo_url":              rowString(app["repoUrl"]),
			"repo_owner":            owner,
			"repo_name":             repo,
			"enabled":               app["enabled"],
			"release_count":         len(appVersions),
			"latest_versions":       latestVersions,
			"repo_token_configured": repoTokenConfigured,
			"ci_push_status":        ciPushStatus,
			"ci_push_plain":         ciPushPlain,
		})
	}

	realtimeEnabledAny := false
	realtimeConfiguredAny := false
	for _, item := range apps {
		if status, ok := item["ci_push_status"].(map[string]any); ok {
			if parseBool(status["realtime_enabled"], false) {
				realtimeEnabledAny = true
			}
			if parseBool(status["configured"], false) {
				realtimeConfiguredAny = true
			}
		}
	}
	realtimeSeed := map[string]any{
		"enabled":           realtimeEnabledAny,
		"configured":        realtimeConfiguredAny,
		"expires_at":        "",
		"expiry_message":    "Per-repository CI ingest keys are managed from each repository row.",
		"api_key":           "",
		"api_key_show_once": false,
	}

	renderTemplate(w, r, "settings_repositories.html", map[string]any{
		"apps":                             apps,
		"github_token_configured":          strings.TrimSpace(aiSettings["ai.github_token"]) != "",
		"default_agent_repo":               strings.TrimSpace(aiSettings["ai.github_repo"]),
		"github_token_expires_date":        githubTokenExpiryDateInputValue(githubTokenExpiresAt),
		"github_token_expiry_status":       tokenExpiryStatus,
		"github_token_validation_status":   tokenValidationStatus,
		"github_token_expiry_warning_days": githubTokenExpiryWarningDays,
		"realtime_seed":                    realtimeSeed,
		"ci_push_default_ttl_days":         ciPushApiKeyDefaultTtlDays,
		"ci_push_max_ttl_days":             ciPushApiKeyMaxTtlDays,
	})
}

func createSettingsRepository(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	slugRaw := strings.TrimSpace(r.FormValue("slug"))
	repoUrlInput := strings.TrimSpace(r.FormValue("repo_url"))
	repoOwnerInput := strings.TrimSpace(r.FormValue("repo_owner"))
	repoNameInput := strings.TrimSpace(r.FormValue("repo_name"))
	repoUrl, owner, repo := resolveGithubRepoFields(repoUrlInput, repoOwnerInput, repoNameInput)
	defaultEnvironment := strings.TrimSpace(r.FormValue("default_environment"))
	githubToken := strings.TrimSpace(r.FormValue("github_token"))
	githubTokenExpiry := normalizeGithubTokenExpiryInput(r.FormValue("github_token_expires_at"))
	setGithubToken := r.FormValue("set_github_token") != ""
	setRepoToken := r.FormValue("set_repo_token") != ""
	setAgentRepo := r.FormValue("set_agent_repo") != ""

	if name == "" || repoUrl == "" {
		flashMessage(w, r, "App name and repository are required", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}

	slugSource := slugRaw
	if slugSource == "" {
		slugSource = name
	}
	slug := appSlug(slugSource)
	existingRes, err := db.Execute(
		"SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 LIMIT 1",
		slug)
	if err == nil && existingRes.Fetchone() != nil {
		flashMessage(w, r, "App slug already exists", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}

	version := time.Now().UnixMilli()
	row := Row{
		"Id":                 uuid4Hex(),
		"Name":               name,
		"Slug":               slug,
		"OwnerTeam":          "",
		"RepoUrl":            repoUrl,
		"DefaultEnvironment": defaultEnvironment,
		"Enabled":            1,
		"MetadataJson":       "{}",
		"IsDeleted":          0,
		"Version":            version,
		"CreatedAt":          nowIso(),
		"UpdatedAt":          nowIso(),
	}
	insertRowsJsonEachRow(db, "sobs_apps", []Row{row})

	if setGithubToken && githubToken != "" {
		saveAiSetting(db, "ai.github_token", githubToken)
		saveAiSetting(db, "ai.github_token_expires_at", githubTokenExpiry)
		saveAiSetting(db, "ai.github_token_last_validated_at", "")
		saveAiSetting(db, "ai.github_token_last_validation_status", "")
		saveAiSetting(db, "ai.github_token_last_validation_message", "")
	}

	if setRepoToken && githubToken != "" {
		if owner != "" && repo != "" {
			saveRepoScopedGithubToken(db, owner, repo, githubToken)
		}
	}

	if setAgentRepo {
		if owner != "" && repo != "" {
			saveAiSetting(db, "ai.github_repo", owner+"/"+repo)
		}
	}

	flashMessage(w, r, "Repository added", "success")
	http.Redirect(w, r, "/settings/repositories", http.StatusFound)
}

func validateSettingsRepositoryGithubToken(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	githubToken := strings.TrimSpace(loadAiSetting(db, "ai.github_token", ""))
	if githubToken == "" {
		flashMessage(w, r, "No GitHub token configured to validate", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}

	status, message := validateGithubToken(githubToken)
	saveAiSetting(db, "ai.github_token_last_validated_at", nowIso())
	saveAiSetting(db, "ai.github_token_last_validation_status", status)
	saveAiSetting(db, "ai.github_token_last_validation_message", message)

	category := "warning"
	if status == "valid" {
		category = "success"
	}
	flashMessage(w, r, "GitHub token validation: "+message, category)
	http.Redirect(w, r, "/settings/repositories", http.StatusFound)
}

func saveSettingsRepositoryRealtimeMode(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	appId := r.PathValue("app_id")
	current, _ := findAppById(db, appId)
	if current == nil {
		flashMessage(w, r, "Repository entry not found", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}
	_ = r.ParseForm()
	enabled := r.FormValue("realtime_enabled") != ""
	setCiPushRealtimeEnabled(db, appId, enabled)
	appName := strings.TrimSpace(rowString(current["Name"]))
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	flashMessage(w, r, fmt.Sprintf("Realtime CI support %s for %s", state, appName), "success")
	http.Redirect(w, r, "/settings/repositories", http.StatusFound)
}

func rotateSettingsRepositoryCiIngestKey(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	appId := r.PathValue("app_id")
	current, _ := findAppById(db, appId)
	if current == nil {
		flashMessage(w, r, "Repository entry not found", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}
	_ = r.ParseForm()
	ttlDays := normalizeTtlDays(r.FormValue("ttl_days"), ciPushApiKeyDefaultTtlDays)
	keyPlain, expiresAt := rotateCiPushApiKey(db, appId, ttlDays)
	if keyPlain == "" {
		flashMessage(w, r, "Failed to rotate CI ingest API key", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}
	setCiPushRealtimeEnabled(db, appId, true)
	plainByApp := ciPushPlainSessionRead(r)
	plainByApp[appId] = keyPlain
	ciPushPlainSessionWrite(w, plainByApp)
	expiresDate := expiresAt
	if len(expiresDate) > 10 {
		expiresDate = expiresDate[:10]
	}
	flashMessage(w, r, fmt.Sprintf(
		"CI ingest API key rotated for %s (expires %s). Copy the key now; it is shown once.",
		strings.TrimSpace(rowString(current["Name"])), expiresDate), "success")
	http.Redirect(w, r, "/settings/repositories", http.StatusFound)
}

func revokeSettingsRepositoryCiIngestKey(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	appId := r.PathValue("app_id")
	current, _ := findAppById(db, appId)
	if current == nil {
		flashMessage(w, r, "Repository entry not found", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}
	revokeCiPushApiKey(db, appId)
	flashMessage(w, r, fmt.Sprintf("CI ingest API key revoked for %s", strings.TrimSpace(rowString(current["Name"]))), "success")
	http.Redirect(w, r, "/settings/repositories", http.StatusFound)
}

func updateSettingsRepository(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	appId := r.PathValue("app_id")
	_ = r.ParseForm()
	repoUrlInput := strings.TrimSpace(r.FormValue("repo_url"))
	repoOwnerInput := strings.TrimSpace(r.FormValue("repo_owner"))
	repoNameInput := strings.TrimSpace(r.FormValue("repo_name"))
	repoToken := strings.TrimSpace(r.FormValue("repo_token"))
	setRepoToken := r.FormValue("set_repo_token") != ""

	current, _ := findAppById(db, appId)
	if current == nil {
		flashMessage(w, r, "Repository entry not found", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}

	repoUrl, owner, repo := resolveGithubRepoFields(repoUrlInput, repoOwnerInput, repoNameInput)
	if repoUrl == "" {
		flashMessage(w, r, "Repository is required", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}

	version := time.Now().UnixMilli()
	createdAt := rowString(current["CreatedAt"])
	if createdAt == "" {
		createdAt = nowIso()
	}
	metadataJson := rowString(current["MetadataJson"])
	if metadataJson == "" {
		metadataJson = "{}"
	}
	row := Row{
		"Id":                 appId,
		"Name":               rowString(current["Name"]),
		"Slug":               rowString(current["Slug"]),
		"OwnerTeam":          rowString(current["OwnerTeam"]),
		"RepoUrl":            repoUrl,
		"DefaultEnvironment": rowString(current["DefaultEnvironment"]),
		"Enabled":            coerceIntOrDefault(current["Enabled"], 1),
		"MetadataJson":       metadataJson,
		"IsDeleted":          0,
		"Version":            version,
		"CreatedAt":          createdAt,
		"UpdatedAt":          nowIso(),
	}
	insertRowsJsonEachRow(db, "sobs_apps", []Row{row})

	if setRepoToken && repoToken != "" {
		if owner != "" && repo != "" {
			saveRepoScopedGithubToken(db, owner, repo, repoToken)
		}
	}

	flashMessage(w, r, "Repository updated", "success")
	http.Redirect(w, r, "/settings/repositories", http.StatusFound)
}

func addSettingsRepositoryRelease(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	appId := r.PathValue("app_id")
	_ = r.ParseForm()
	releaseVersion := strings.TrimSpace(r.FormValue("version"))
	environment := strings.TrimSpace(r.FormValue("environment"))

	appRow, _ := findAppById(db, appId)
	if appRow == nil {
		flashMessage(w, r, "Repository entry not found", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}

	if releaseVersion == "" {
		flashMessage(w, r, "Release version is required", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}

	version := time.Now().UnixMilli()
	row := Row{
		"Id":             uuid4Hex(),
		"AppId":          appId,
		"ReleaseVersion": releaseVersion,
		"CommitSha":      "",
		"BuildId":        "",
		"Environment":    environment,
		"ReleasedAt":     nowIso(),
		"MetadataJson":   "{}",
		"IsDeleted":      0,
		"Version":        version,
	}
	insertRowsJsonEachRow(db, "sobs_app_releases", []Row{row})
	flashMessage(w, r, "Release added", "success")
	http.Redirect(w, r, "/settings/repositories", http.StatusFound)
}

func deleteSettingsRepository(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	appId := r.PathValue("app_id")
	current, _ := findAppById(db, appId)
	if current == nil {
		flashMessage(w, r, "Repository entry not found", "warning")
		http.Redirect(w, r, "/settings/repositories", http.StatusFound)
		return
	}

	version := time.Now().UnixMilli()
	nowIsoVal := nowIso()
	createdAt := rowString(current["CreatedAt"])
	if createdAt == "" {
		createdAt = nowIsoVal
	}
	metadataJson := rowString(current["MetadataJson"])
	if metadataJson == "" {
		metadataJson = "{}"
	}
	row := Row{
		"Id":                 appId,
		"Name":               rowString(current["Name"]),
		"Slug":               rowString(current["Slug"]),
		"OwnerTeam":          rowString(current["OwnerTeam"]),
		"RepoUrl":            rowString(current["RepoUrl"]),
		"DefaultEnvironment": rowString(current["DefaultEnvironment"]),
		"Enabled":            coerceIntOrDefault(current["Enabled"], 1),
		"MetadataJson":       metadataJson,
		"IsDeleted":          1,
		"Version":            version,
		"CreatedAt":          createdAt,
		"UpdatedAt":          nowIsoVal,
	}
	insertRowsJsonEachRow(db, "sobs_apps", []Row{row})

	releaseRowsRes, err := db.Execute(
		"SELECT * FROM sobs_app_releases FINAL WHERE AppId=? AND IsDeleted=0",
		appId)
	if err == nil {
		releaseRows := releaseRowsRes.Fetchall()
		if len(releaseRows) > 0 {
			releaseTombstones := make([]Row, 0, len(releaseRows))
			for _, releaseRow := range releaseRows {
				releaseTombstones = append(releaseTombstones, Row{
					"Id":             rowString(releaseRow["Id"]),
					"AppId":          rowString(releaseRow["AppId"]),
					"ReleaseVersion": rowString(releaseRow["ReleaseVersion"]),
					"CommitSha":      rowString(releaseRow["CommitSha"]),
					"BuildId":        rowString(releaseRow["BuildId"]),
					"Environment":    rowString(releaseRow["Environment"]),
					"ReleasedAt":     rowString(releaseRow["ReleasedAt"]),
					"MetadataJson":   rowString(releaseRow["MetadataJson"]),
					"IsDeleted":      1,
					"Version":        version,
				})
			}
			insertRowsJsonEachRow(db, "sobs_app_releases", releaseTombstones)
		}
	}

	flashMessage(w, r, fmt.Sprintf("Repository '%s' deleted", rowString(current["Name"])), "success")
	http.Redirect(w, r, "/settings/repositories", http.StatusFound)
}

// ---------------------------------------------------------------------------
// Agent Rules  GET/POST /settings/agents
// ---------------------------------------------------------------------------

func viewAgentRules(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	rules := loadAgentRules(db)
	runs := loadAgentRuns(db, 20)
	anomalyRules, _ := loadAnomalyRules(db)
	tagRules, _ := loadTagRules(db)
	renderTemplate(w, r, "settings_agents.html", map[string]any{
		"rules":          rules,
		"runs":           runs,
		"anomaly_rules":  anomalyRules,
		"tag_rules":      tagRules,
		"trigger_types":  agentTriggerTypes,
		"trigger_states": agentTriggerStates,
		"agent_actions":  agentActions,
	})
}

func createAgentRule(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	description := strings.TrimSpace(r.FormValue("description"))
	triggerTypeRaw := r.FormValue("trigger_type")
	if triggerTypeRaw == "" {
		triggerTypeRaw = "manual"
	}
	triggerType := strings.ToLower(strings.TrimSpace(triggerTypeRaw))
	triggerRefId := strings.TrimSpace(r.FormValue("trigger_ref_id"))
	triggerStateRaw := r.FormValue("trigger_state")
	if triggerStateRaw == "" {
		triggerStateRaw = "any"
	}
	triggerState := strings.ToLower(strings.TrimSpace(triggerStateRaw))
	actionsList := r.Form["actions"]
	rateLimit := 60
	if raw := r.FormValue("rate_limit_minutes"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			rateLimit = max(1, min(10080, v))
		} else {
			rateLimit = 60
		}
	}

	if name == "" {
		flashMessage(w, r, "Rule name is required", "warning")
		http.Redirect(w, r, "/settings/agents", http.StatusFound)
		return
	}
	if !slices.Contains(agentTriggerTypes, triggerType) {
		flashMessage(w, r, "Invalid trigger type: "+triggerType, "warning")
		http.Redirect(w, r, "/settings/agents", http.StatusFound)
		return
	}
	if !slices.Contains(agentTriggerStates, triggerState) {
		flashMessage(w, r, "Invalid trigger state: "+triggerState, "warning")
		http.Redirect(w, r, "/settings/agents", http.StatusFound)
		return
	}

	validActions := []string{}
	for _, a := range actionsList {
		if slices.Contains(agentActions, a) {
			validActions = append(validActions, a)
		}
	}
	if len(validActions) == 0 {
		validActions = []string{"analyze"}
	}

	ruleId := agentUuid4()
	insertRowsJsonEachRow(getDb(), "sobs_agent_rules", []Row{
		{
			"Id":               ruleId,
			"Name":             name,
			"Description":      description,
			"TriggerType":      triggerType,
			"TriggerRefId":     triggerRefId,
			"TriggerState":     triggerState,
			"Actions":          strings.Join(validActions, ","),
			"RateLimitMinutes": rateLimit,
			"IsEnabled":        1,
			"IsDeleted":        0,
			"Version":          time.Now().UnixMilli(),
		},
	})
	flashMessage(w, r, fmt.Sprintf("Agent rule '%s' created", name), "success")
	http.Redirect(w, r, "/settings/agents", http.StatusFound)
}

func deleteAgentRule(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	ruleId := r.PathValue("rule_id")

	deletedRow := func(row Row) map[string]any {
		return map[string]any{
			"Id":               ruleId,
			"Name":             rowString(row["Name"]),
			"Description":      "",
			"TriggerType":      "manual",
			"TriggerRefId":     "",
			"TriggerState":     "any",
			"Actions":          "analyze",
			"RateLimitMinutes": 60,
			"IsEnabled":        0,
		}
	}

	// PORT-NOTE: _soft_delete_latest_row is owned by another section; keyword-only
	// args (incl. default categories "warning"/"success") are passed positionally
	// in declaration order, with (w, r) prepended for flash/redirect handling.
	softDeleteLatestRow(
		w, r, db,
		"SELECT Id, Name FROM sobs_agent_rules FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1",
		[]any{ruleId},
		"sobs_agent_rules",
		deletedRow,
		"Agent rule not found",
		"Agent rule '{name}' deleted",
		"view_agent_rules",
		"warning",
		"success",
	)
}

// ---------------------------------------------------------------------------
// AI helper telemetry helpers
// ---------------------------------------------------------------------------

func sseJsonEvent(eventName string, payload any) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventName, llmJsonDumps(payload))
}

func guardTelemetryAttrs(allowed bool, guardReason string, guardStats map[string]any) map[string]any {
	get := func(key string) any {
		if v, ok := guardStats[key]; ok {
			return v
		}
		return 0
	}
	attrs := map[string]any{
		"gen_ai.guard.allowed":       allowed,
		"gen_ai.guard.reason":        guardReason,
		"gen_ai.usage.input_tokens":  get("prompt_tokens"),
		"gen_ai.usage.output_tokens": get("completion_tokens"),
		"gen_ai.response.latency_ms": get("elapsed_ms"),
	}
	systemPrompt := strings.TrimSpace(rowString(guardStats["system_instructions"]))
	if systemPrompt != "" {
		attrs["gen_ai.system_instructions"] = systemPrompt
	}

	if inputMessages, ok := guardStats["input_messages"]; ok && inputMessages != nil {
		if s, isStr := inputMessages.(string); isStr {
			attrs["gen_ai.input.messages"] = s
		} else {
			attrs["gen_ai.input.messages"] = llmJsonDumps(inputMessages)
		}
	}
	return attrs
}

func buildAiTurnLogsUrl(chatId, turnId string) string {
	where := "ServiceName = '" +
		aiHelperServiceName +
		"' AND LogAttributes['gen_ai.chat_id'] = '" +
		strings.ReplaceAll(chatId, "'", "''") +
		"' AND LogAttributes['gen_ai.turn_id'] = '" +
		strings.ReplaceAll(turnId, "'", "''") +
		"'"
	// PORT-NOTE: url_for('view_logs') resolves to the literal "/logs" route;
	// urllib.parse.quote(where, safe='') → QueryEscape with "+" rewritten to "%20".
	return "/logs?sql=" + strings.ReplaceAll(url.QueryEscape(where), "+", "%20")
}

// emitAiHelperLogEvent mirrors _emit_ai_helper_log_event (kw-only args become
// positional). severity defaults to "INFO" and attrs to nil at call sites.
func emitAiHelperLogEvent(
	eventName string,
	chatId string,
	turnId string,
	page string,
	model string,
	guardModel string,
	thinkingLevel string,
	body string,
	severity string,
	attrs map[string]any,
) {
	if severity == "" {
		severity = "INFO"
	}
	attrMap := map[string]any{
		"gen_ai.system":                 "sobs",
		"gen_ai.operation.name":         "chat",
		"gen_ai.chat_id":                chatId,
		"gen_ai.turn_id":                turnId,
		"gen_ai.request.model":          model,
		"gen_ai.guard.model":            guardModel,
		"gen_ai.request.thinking_level": thinkingLevel,
		"sobs.ai.page":                  page,
		"sobs.ai.event":                 eventName,
	}
	for key, value := range attrs {
		if value == nil {
			continue
		}
		attrMap[key] = pyScalarStr(value)
	}

	logAttrs := stringifyAttrs(attrMap)
	row := Row{
		"Timestamp":          nowIso(),
		"TraceId":            chatId,
		"SpanId":             turnId,
		"TraceFlags":         0,
		"SeverityText":       severity,
		"SeverityNumber":     severityNumber(severity),
		"ServiceName":        aiHelperServiceName,
		"Body":               body,
		"ResourceSchemaUrl":  "",
		"ResourceAttributes": map[string]any{"service.name": aiHelperServiceName, "telemetry.sdk.name": "sobs"},
		"ScopeSchemaUrl":     "",
		"ScopeName":          "sobs.gen_ai.helper",
		"ScopeVersion":       "1",
		"ScopeAttributes":    map[string]any{},
		"LogAttributes":      logAttrs,
		"EventName":          eventName,
	}

	traceSpanId := turnId
	if eventName != "turn.start" {
		digest := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s|%s|%d", turnId, eventName, time.Now().UnixNano()))))
		traceSpanId = digest[:16]
	}
	traceParentSpanId := turnId
	if eventName == "turn.start" {
		traceParentSpanId = ""
	}
	durationNs := int64(0)
	if attrs != nil {
		if v, ok := attrs["gen_ai.response.latency_ms"]; ok {
			if f, ok := coerceFloat(v); ok {
				durationNs = int64(f * 1_000_000)
				if durationNs < 0 {
					durationNs = 0
				}
			}
		}
	}
	statusCode := "STATUS_CODE_OK"
	if strings.ToUpper(severity) == "ERROR" {
		statusCode = "STATUS_CODE_ERROR"
	}
	traceRow := Row{
		"Timestamp":          nowIso(),
		"TraceId":            chatId,
		"SpanId":             traceSpanId,
		"ParentSpanId":       traceParentSpanId,
		"TraceState":         "",
		"SpanName":           "ai." + eventName,
		"SpanKind":           "INTERNAL",
		"ServiceName":        aiHelperServiceName,
		"ResourceAttributes": map[string]any{"service.name": aiHelperServiceName, "telemetry.sdk.name": "sobs"},
		"ScopeName":          "sobs.gen_ai.helper",
		"ScopeVersion":       "1",
		"SpanAttributes":     logAttrs,
		"Duration":           durationNs,
		"StatusCode":         statusCode,
		"StatusMessage":      body,
		"Events":             map[string]any{"Timestamp": []any{}, "Name": []any{}, "Attributes": []any{}},
		"Links":              map[string]any{"TraceId": []any{}, "SpanId": []any{}, "TraceState": []any{}, "Attributes": []any{}},
	}

	// PORT-NOTE: app.config["TESTING"] gated on SOBS_TESTING (see s02_db/s04).
	wait := envFlag("SOBS_TESTING", false)
	op := func(db *ChDbConnection) error {
		insertRowsJsonEachRow(db, "otel_logs", []Row{row})
		insertRowsJsonEachRow(db, "otel_traces", []Row{traceRow})
		rememberLogAttrKeys(db, extractLogAttrMaps([]Row{row}), "log")
		return nil
	}
	if err := queueWrite(op, wait); err != nil {
		logger.Error("Failed to emit AI helper telemetry event", "event", eventName, "error", err)
	}
}

// ---------------------------------------------------------------------------
// AI Contextual Helper API  /api/ai/helper*
// ---------------------------------------------------------------------------

func aiHelperCapabilities(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	settings := loadAllAiSettings(db)
	model := strings.TrimSpace(settings["ai.model"])
	thinkingLevel := normalizeThinkingLevel(settings["ai.thinking_level"])
	page := strings.TrimSpace(r.URL.Query().Get("page"))
	if page == "" {
		page = "/logs"
	}
	actionManifest := helperActionManifestForPage(page)
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":                     true,
		"model":                  model,
		"supports_tools":         modelSupportsTools(model),
		"supports_thinking":      modelSupportsThinking(model),
		"default_thinking_level": thinkingLevel,
		"thinking_levels":        aiThinkingLevels,
		"page":                   page,
		"action_manifest":        actionManifest,
	})
}

func aiHelperActionManifest(w http.ResponseWriter, r *http.Request) {
	page := strings.TrimSpace(r.URL.Query().Get("page"))
	if page == "" {
		page = "/logs"
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":      true,
		"page":    page,
		"actions": helperActionManifestForPage(page),
	})
}

func aiHelperChats(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	query := r.URL.Query()
	page := strings.TrimSpace(query.Get("page"))
	q := strings.ToLower(strings.TrimSpace(query.Get("q")))
	limit := 20
	if raw := query.Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			limit = max(5, min(v, 100))
		}
	}
	offset := 0
	if raw := query.Get("offset"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			offset = max(0, v)
		}
	}

	where := []string{"ServiceName=?", "EventName='turn.summary'", "LogAttributes['gen_ai.chat_id'] != ''"}
	params := []any{aiHelperServiceName}
	if page != "" {
		where = append(where, "LogAttributes['sobs.ai.page'] = ?")
		params = append(params, page)
	}
	whereSql := strings.Join(where, " AND ")
	res, err := db.Execute(
		"SELECT "+
			"  LogAttributes['gen_ai.chat_id'] AS chat_id, "+
			"  min(Timestamp) AS first_ts, "+
			"  max(Timestamp) AS last_ts, "+
			"  argMin(LogAttributes['gen_ai.input.question'], Timestamp) AS first_question, "+
			"  argMin(LogAttributes['gen_ai.turn.summary.request'], Timestamp) AS first_request, "+
			"  count() AS turn_count "+
			"FROM otel_logs WHERE "+whereSql+" "+
			"GROUP BY chat_id "+
			"ORDER BY last_ts DESC LIMIT 500",
		params...)
	var rows []Row
	if err != nil {
		logger.Error("aiHelperChats query failed", "error", err)
	} else {
		rows = res.Fetchall()
	}

	chats := []map[string]any{}
	for _, row := range rows {
		chatId := strings.TrimSpace(rowString(row["chat_id"]))
		if chatId == "" {
			continue
		}
		label := chatLabelFromFirstTurn(row["first_question"], row["first_request"])
		if q != "" && !strings.Contains(strings.ToLower(label), q) {
			continue
		}
		chats = append(chats, map[string]any{
			"chat_id":    chatId,
			"first_ts":   rowString(row["first_ts"]),
			"last_ts":    rowString(row["last_ts"]),
			"label":      label,
			"turn_count": coerceInt(row["turn_count"]),
		})
	}

	total := len(chats)
	start := offset
	if start > total {
		start = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	pageChats := chats[start:end]
	hasMore := offset+len(pageChats) < total
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":       true,
		"chats":    pageChats,
		"total":    total,
		"has_more": hasMore,
		"offset":   offset,
	})
}

func aiHelperChatDetail(w http.ResponseWriter, r *http.Request) {
	safeChatId := strings.TrimSpace(r.PathValue("chat_id"))
	if safeChatId == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "chat_id is required"})
		return
	}

	db := getDb()
	res, err := db.Execute(
		"SELECT "+
			"  Timestamp, "+
			"  LogAttributes['gen_ai.turn_id'] AS turn_id, "+
			"  LogAttributes['gen_ai.input.question'] AS input_question, "+
			"  LogAttributes['gen_ai.turn.summary.request'] AS request, "+
			"  LogAttributes['gen_ai.output.messages'] AS output_messages "+
			"FROM otel_logs "+
			"WHERE ServiceName=? AND EventName='turn.complete' AND LogAttributes['gen_ai.chat_id']=? "+
			"ORDER BY Timestamp ASC LIMIT 300",
		aiHelperServiceName, safeChatId)
	var rows []Row
	if err != nil {
		logger.Error("aiHelperChatDetail query failed", "error", err)
	} else {
		rows = res.Fetchall()
	}

	toolsByTurn := loadChatToolHistory(db, safeChatId)
	messages := []map[string]any{}
	for _, row := range rows {
		ts := rowString(row["Timestamp"])
		turnId := rowString(row["turn_id"])
		requestText := strings.TrimSpace(rowString(row["input_question"]))
		if requestText != "" {
			messages = append(messages, map[string]any{
				"kind":    "message",
				"role":    "user",
				"text":    requestText,
				"ts":      ts,
				"turn_id": turnId,
			})
		}

		assistantText := ""
		rawOutput := rowString(row["output_messages"])
		if rawOutput != "" {
			var parsed any
			if err := json.Unmarshal([]byte(rawOutput), &parsed); err == nil {
				if list, ok := parsed.([]any); ok {
					parts := []string{}
					for _, item := range list {
						if m, ok := item.(map[string]any); ok {
							content := strings.TrimSpace(rowString(m["content"]))
							if content != "" {
								parts = append(parts, content)
							}
						}
					}
					assistantText = strings.TrimSpace(strings.Join(parts, "\n\n"))
				}
			}
		}
		if assistantText != "" {
			assistantText, _ = extractAssistantMeta(assistantText)
		}
		if assistantText != "" {
			messages = append(messages, map[string]any{
				"kind":     "message",
				"role":     "assistant",
				"text":     assistantText,
				"ts":       ts,
				"turn_id":  turnId,
				"question": requestText,
			})
		}
		for _, toolItem := range toolsByTurn[turnId] {
			messages = append(messages, toolItem)
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "chat_id": safeChatId, "messages": messages})
}

func aiHelperFeedback(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	chatId := strings.TrimSpace(rowString(payload["chat_id"]))
	turnId := strings.TrimSpace(rowString(payload["turn_id"]))
	note := strings.TrimSpace(rowString(payload["note"]))
	page := strings.TrimSpace(rowString(payload["page"]))
	if page == "" {
		page = "/logs"
	}
	if chatId == "" || turnId == "" || note == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "chat_id, turn_id, and note are required"})
		return
	}

	emitAiHelperLogEvent(
		"turn.feedback", chatId, turnId, page, "", "", "off", note, "INFO",
		map[string]any{
			"gen_ai.feedback.note": note,
			"gen_ai.feedback.kind": "user_note",
		},
	)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// aiHelper mirrors ai_helper: contextual AI helper. Accepts JSON
// {question, page, context} and returns an LLM answer (streamed or buffered).
func aiHelper(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	question := strings.TrimSpace(rowString(payload["question"]))
	page := strings.TrimSpace(rowString(payload["page"]))
	contextData, _ := payload["context"].(map[string]any)
	streamRequested := parseBool(payload["stream"], false) ||
		strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	chatId := strings.TrimSpace(rowString(payload["chat_id"]))
	if chatId == "" {
		chatId = agentUuid4()
	}
	turnId := strings.TrimSpace(rowString(payload["turn_id"]))
	if turnId == "" {
		turnId = agentUuid4()
	}

	if question == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "question is required"})
		return
	}

	db := getDb()
	settings := loadAllAiSettings(db)

	endpointUrl := strings.TrimSpace(settings["ai.endpoint_url"])
	model := strings.TrimSpace(settings["ai.model"])
	apiKey := strings.TrimSpace(settings["ai.api_key"])
	systemPromptOverride := strings.TrimSpace(settings["ai.system_prompt"])
	guardModel := strings.TrimSpace(settings["ai.guard_model"])

	defaultThinking := normalizeThinkingLevel(settings["ai.thinking_level"])
	requestedThinking := normalizeThinkingLevel(strings.TrimSpace(rowString(payload["thinking_level"])))
	thinkingLevel := defaultThinking
	if requestedThinking != "off" {
		thinkingLevel = requestedThinking
	}
	if !modelSupportsThinking(model) {
		thinkingLevel = "off"
	}

	emitAiHelperLogEvent(
		"turn.start", chatId, turnId, page, model, guardModel, thinkingLevel,
		"AI helper turn started", "",
		map[string]any{
			"gen_ai.request.stream": streamRequested,
			"gen_ai.input.messages": jsonDumpsNoEscape([]map[string]any{{"role": "user", "content": question}}),
		},
	)

	if endpointUrl == "" || model == "" {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "AI endpoint not configured. Visit Settings → AI Configuration.",
		})
		return
	}

	allowed, guardReason, guardStats := checkGuardModel(settings, question, page)
	emitAiHelperLogEvent(
		"guard.result", chatId, turnId, page, model, guardModel, thinkingLevel,
		fmt.Sprintf("Guard verdict: %s", guardReason), "",
		guardTelemetryAttrs(allowed, guardReason, guardStats),
	)
	if !allowed {
		errorMessage := fmt.Sprintf("Request blocked by safety guard: %s", guardReason)
		emitAiHelperLogEvent(
			"turn.blocked", chatId, turnId, page, model, guardModel, thinkingLevel,
			errorMessage, "WARN",
			map[string]any{"gen_ai.guard.reason": guardReason},
		)
		if streamRequested {
			flusher, ok := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			io.WriteString(w, sseJsonEvent("error", map[string]any{"error": errorMessage}))
			if ok {
				flusher.Flush()
			}
			return
		}
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": errorMessage})
		return
	}

	actionManifest := helperActionManifestForPage(page)
	actionManifestJson := jsonDumpsNoEscape(actionManifest)
	dashboardActionManifest := helperActionManifestForPage("/dashboards")
	dashboardActionManifestJson := jsonDumpsNoEscape(dashboardActionManifest)
	chatMemories := loadChatMemories(db, chatId)
	relevantMemories := semanticMemoryMatches(chatMemories, question, 5, aiMemorySemanticMinScore)
	recentChatTurns := loadRecentChatTurns(db, chatId, 8)
	recentHistory := loadRecentTurnSummaries(db, chatId, question, 4)

	memoryLines := []string{}
	for _, item := range relevantMemories {
		text := strings.TrimSpace(rowString(item["text"]))
		if text != "" {
			memoryLines = append(memoryLines, "- "+text)
		}
	}
	memoryBlock := strings.Join(memoryLines, "\n")

	historyLines := []string{}
	for _, item := range recentHistory {
		historyLines = append(historyLines, fmt.Sprintf("- request=%s; action=%s; result=%s",
			item["request"], item["action"], item["result"]))
	}
	historyBlock := strings.Join(historyLines, "\n")

	continuityLines := []string{}
	for _, item := range recentChatTurns {
		continuityLines = append(continuityLines, fmt.Sprintf("- request=%s; action=%s; result=%s",
			item["request"], item["action"], item["result"]))
	}
	continuityBlock := strings.Join(continuityLines, "\n")

	systemPrompt := systemPromptOverride
	if systemPrompt == "" {
		systemPrompt = "You are an expert observability assistant for SOBS (Simple Observe Stack). " +
			"You help operators understand and troubleshoot their application telemetry including " +
			"logs, traces, errors, metrics, RUM events, and AI transparency data. " +
			"Be concise and actionable. When suggesting SQL queries, use ClickHouse syntax. " +
			"If the request is ambiguous and multiple interpretations are plausible, ask one short " +
			"clarifying question before taking action. If intent is clear, act directly. " +
			"Try higher-quality solutions before simplistic ones, especially for grouping/ranking asks. " +
			"Only propose UI actions that exist in the action manifest for this page. " +
			"Do not claim any UI action was executed unless a tool is called and execution is " +
			"confirmed by the app. " +
			"When a UI action will be applied by the browser after your response, describe it as " +
			"proposed, queued, or ready to apply; do not say it already succeeded. " +
			"If the page action manifest does not expose the control needed for the request, explain " +
			"that limitation and do not call a UI action unless you can pivot using cross-page actions. " +
			"For chart or dashboard creation requests, prefer a cross-page pivot to /dashboards using " +
			"available dashboard actions. " +
			"If tools are available and the user asks to apply a logs SQL filter, call " +
			"propose_ui_action with action_id logs.filter.apply_sql. " +
			"If tools are available and the user asks to apply an AI page SQL filter, call " +
			"propose_ui_action with action_id ai.filter.apply_sql. " +
			"The otel_logs table has an EventName column for structured event types. " +
			"To filter by event name use: EventName = 'turn.feedback' " +
			"To access log attributes use: LogAttributes['gen_ai.feedback.note'] " +
			"Examples: EventName = 'turn.feedback' finds AI assistant feedback records; " +
			"EventName = 'turn.complete' finds completed AI turns; " +
			"EventName = 'turn.feedback' AND TraceId = '<chat_id>' scopes to one conversation. " +
			"All AI assistant telemetry lives in otel_logs under ServiceName = 'sobs-ai-helper'. " +
			"On the AI page the table is otel_traces. Supported aliases include: service, model, provider, " +
			"operation, prompt, response, span_name, row_type, trace_id, span_id, ts, status, " +
			"error_type, tokens_in, tokens_out, " +
			"thinking_tokens, duration_ms. " +
			"Do not use LogAttributes[...] on the AI page; use aliases or SpanAttributes[...] only. " +
			"AI page examples: row_type = 'system' AND span_name = 'ai.tool.executed'; " +
			"model = 'gpt-oss:120b-cloud' AND tokens_out > 1000; " +
			"prompt ILIKE '%graph%' OR response ILIKE '%chart%'; " +
			"provider = 'sobs' AND error_type != ''; " +
			"duration_ms > 1000 ORDER BY Timestamp DESC is not valid in WHERE, so only emit the filter expression. " +
			"For requests like 'longest traces' or 'highest total duration by trace', generate a " +
			"richer WHERE clause using an IN subquery with GROUP BY trace id and ORDER BY sum(Duration) DESC. " +
			"At the very end of every response, append a single compact metadata block in this exact format: " +
			"<assistant_meta>{\"turn_summary\":{\"request\":\"...\",\"action\":\"...\",\"result\":\"...\"}," +
			"\"memory_candidates\":[\"optional memory 1\",\"optional memory 2\"]}</assistant_meta>. " +
			"Keep memory_candidates empty when no durable memory is needed. " +
			"Do not include any additional text after </assistant_meta>. " +
			"Page action manifest: " +
			actionManifestJson +
			"\nCross-page dashboard actions (/dashboards): " +
			dashboardActionManifestJson
	}

	if memoryBlock != "" {
		systemPrompt += "\n\nRelevant persistent memories:\n" + memoryBlock
	}
	if continuityBlock != "" {
		systemPrompt += "\n\nCurrent chat continuity (recent turns):\n" + continuityBlock
	}
	if historyBlock != "" {
		systemPrompt += "\n\nSemantically relevant prior turn summaries:\n" + historyBlock
	}

	contextLines := []string{}
	if page != "" {
		contextLines = append(contextLines, "Current page: "+page)
	}
	for k, v := range contextData {
		if agentTruthy(v) {
			contextLines = append(contextLines, fmt.Sprintf("%s: %s", k, pyScalarStr(v)))
		}
	}
	contextStr := strings.Join(contextLines, "\n")
	userContent := question
	if contextStr != "" {
		userContent = fmt.Sprintf("%s\n\nQuestion: %s", contextStr, question)
	}

	messages := []map[string]any{
		{"role": "system", "content": systemPrompt},
		{"role": "user", "content": userContent},
	}
	var tools []map[string]any
	if modelSupportsTools(model) {
		tools = helperToolsForPage(page)
	}
	turnLogsUrl := buildAiTurnLogsUrl(chatId, turnId)

	if streamRequested {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		write := func(s string) {
			io.WriteString(w, s)
			if flusher != nil {
				flusher.Flush()
			}
		}

		answerParts := []string{}
		thinkingTokens := 0
		lastToolSummary := ""
		loopMessages := append([]map[string]any{}, messages...)
		maxToolRounds := 3
		write(sseJsonEvent("meta", map[string]any{
			"chat_id":           chatId,
			"turn_id":           turnId,
			"supports_thinking": modelSupportsThinking(model),
			"thinking_level":    thinkingLevel,
			"turn_logs_url":     turnLogsUrl,
		}))
		write(sseJsonEvent("guard", map[string]any{"guard_stats": guardStats}))

		modelStats := map[string]any{}
		// PORT-NOTE: Python wraps the whole generation in try/except; a stream
		// failure raises and is caught below. Client CancelledError is not
		// separately detectable here, so cancellation falls through as an error.
		genErr := func() error {
			for loopRound := 0; loopRound <= maxToolRounds; loopRound++ {
				roundTextParts := []string{}
				roundToolFeedback := []map[string]any{}
				err := streamLlmEndpoint(endpointUrl, model, apiKey, loopMessages, tools, thinkingLevel, 768, 60, func(event map[string]any) {
					eventType := rowString(event["type"])
					switch eventType {
					case "delta":
						chunk := rowString(event["text"])
						if chunk != "" {
							roundTextParts = append(roundTextParts, chunk)
							answerParts = append(answerParts, chunk)
							write(sseJsonEvent("token", map[string]any{"text": chunk}))
						}
					case "tool":
						toolCall, _ := event["tool_call"].(map[string]any)
						toolName := rowString(toolCall["name"])
						toolArgs, _ := toolCall["arguments"].(map[string]any)
						if toolArgs != nil {
							var normalizedTool map[string]any
							if toolName == "propose_ui_action" {
								normalizedTool = normalizeGenericUiActionToolCall(toolArgs, page)
							}
							if normalizedTool != nil {
								actionId := strings.TrimSpace(rowString(normalizedTool["action_id"]))
								unsupported := parseBool(normalizedTool["unsupported"], false)
								actionPayload, _ := normalizedTool["action"].(map[string]any)
								lastToolSummary = strings.TrimSpace(rowString(normalizedTool["summary"]))
								if actionId != "" && !unsupported && len(actionPayload) > 0 {
									targetPage := strings.TrimSpace(rowString(actionPayload["target_page"]))
									if targetPage == "" {
										targetPage = page
									}
									if targetPage == "" {
										targetPage = "/logs"
									}
									normalizedTool["action_token"] = issueAiActionToken(
										actionId, targetPage, actionPayload,
										parseBool(normalizedTool["requires_confirmation"], true),
										chatId, turnId,
									)
								}
								status := "proposed"
								if unsupported {
									status = "unsupported"
								}
								emitAiHelperLogEvent(
									"tool.proposed", chatId, turnId, page, model, guardModel, thinkingLevel,
									fmt.Sprintf("Tool proposed: %s", toolName), "",
									map[string]any{
										"gen_ai.tool.name":                     toolName,
										"sobs.ai.action_id":                    actionId,
										"sobs.ai.tool.summary":                 rowString(normalizedTool["summary"]),
										"sobs.ai.tool.action":                  jsonDumpsNoEscape(orEmptyMap(normalizedTool["action"])),
										"sobs.ai.action.requires_confirmation": parseBool(normalizedTool["requires_confirmation"], true),
										"sobs.ai.action.status":                status,
									},
								)
								roundToolFeedback = append(roundToolFeedback, map[string]any{
									"tool":                  toolName,
									"ok":                    !unsupported,
									"action_id":             actionId,
									"summary":               rowString(normalizedTool["summary"]),
									"action":                orEmptyMap(normalizedTool["action"]),
									"requires_confirmation": parseBool(normalizedTool["requires_confirmation"], true),
								})
								write(sseJsonEvent("tool", normalizedTool))
							}
						}
					case "done":
						if s, ok := event["stats"].(map[string]any); ok {
							modelStats = s
						} else {
							modelStats = map[string]any{}
						}
					}
				})
				if err != nil {
					return err
				}

				if len(roundToolFeedback) == 0 {
					fallbackTool := suggestChartDashboardPivotTool(question, page)
					if fallbackTool != nil {
						actionId := strings.TrimSpace(rowString(fallbackTool["action_id"]))
						unsupported := parseBool(fallbackTool["unsupported"], false)
						actionPayload, _ := fallbackTool["action"].(map[string]any)
						lastToolSummary = strings.TrimSpace(rowString(fallbackTool["summary"]))
						if actionId != "" && !unsupported && len(actionPayload) > 0 {
							targetPage := strings.TrimSpace(rowString(actionPayload["target_page"]))
							if targetPage == "" {
								targetPage = page
							}
							if targetPage == "" {
								targetPage = "/logs"
							}
							fallbackTool["action_token"] = issueAiActionToken(
								actionId, targetPage, actionPayload,
								parseBool(fallbackTool["requires_confirmation"], true),
								chatId, turnId,
							)
						}
						status := "proposed"
						if unsupported {
							status = "unsupported"
						}
						emitAiHelperLogEvent(
							"tool.proposed", chatId, turnId, page, model, guardModel, thinkingLevel,
							"Tool proposed: fallback.dashboard_chart_pivot", "",
							map[string]any{
								"gen_ai.tool.name":                     "fallback.dashboard_chart_pivot",
								"sobs.ai.action_id":                    actionId,
								"sobs.ai.tool.summary":                 rowString(fallbackTool["summary"]),
								"sobs.ai.tool.action":                  jsonDumpsNoEscape(orEmptyMap(fallbackTool["action"])),
								"sobs.ai.action.requires_confirmation": parseBool(fallbackTool["requires_confirmation"], true),
								"sobs.ai.action.status":                status,
							},
						)
						roundToolFeedback = append(roundToolFeedback, map[string]any{
							"tool":                  "propose_ui_action",
							"ok":                    !unsupported,
							"action_id":             actionId,
							"summary":               rowString(fallbackTool["summary"]),
							"action":                orEmptyMap(fallbackTool["action"]),
							"requires_confirmation": parseBool(fallbackTool["requires_confirmation"], true),
						})
						write(sseJsonEvent("tool", fallbackTool))
					}
				}

				hasPendingConfirmation := false
				for _, item := range roundToolFeedback {
					if parseBool(item["requires_confirmation"], true) {
						hasPendingConfirmation = true
						break
					}
				}
				// If awaiting user confirmation, stop loop to avoid re-proposing identical actions.
				if hasPendingConfirmation {
					break
				}

				// Continue loop only if tool calls were made this round and rounds remain.
				if len(roundToolFeedback) == 0 || loopRound >= maxToolRounds {
					break
				}

				assistantRoundText := strings.TrimSpace(strings.Join(roundTextParts, ""))
				if assistantRoundText != "" {
					loopMessages = append(loopMessages, map[string]any{"role": "assistant", "content": assistantRoundText})
				} else {
					loopMessages = append(loopMessages, map[string]any{
						"role":    "assistant",
						"content": "Requested tool calls for the current turn.",
					})
				}

				toolFeedbackText := jsonDumpsNoEscape(roundToolFeedback)
				loopMessages = append(loopMessages, map[string]any{
					"role": "system",
					"content": "Tool execution results for this turn (JSON). Use these results to continue reasoning " +
						"and produce the final answer when ready: " + toolFeedbackText,
				})
			}
			if f, _ := coerceFloat(modelStats["thinking_tokens"]); true {
				thinkingTokens = int(f)
			}
			finalAnswer, assistantMeta := extractAssistantMeta(strings.Join(answerParts, ""))
			metaSummary, _ := assistantMeta["turn_summary"].(map[string]any)
			summary := deriveTurnSummary(question, finalAnswer, lastToolSummary, metaSummary)

			memoryCandidates := extractMemoryCandidates(assistantMeta)
			savedMemoryIds := []string{}
			for _, candidate := range memoryCandidates {
				memoriesNow := loadChatMemories(db, chatId)
				related := semanticMemoryMatches(memoriesNow, candidate, 4, aiMemoryConsolidationScore)
				consolidation := consolidateMemoryCandidates(settings, candidate, related)
				action := strings.TrimSpace(rowString(consolidation["action"]))
				if action == "" {
					action = "keep_new"
				}
				if action == "ignore" {
					continue
				}
				mergedSource := consolidation["memory"]
				if rowString(mergedSource) == "" {
					mergedSource = candidate
				}
				mergedText := coerceSummaryValue(mergedSource, 280)
				if dropIds, ok := consolidation["drop_ids"].([]any); ok {
					for _, raw := range dropIds {
						upsertAiMemory(db, rowString(raw), chatId, "", turnId, true)
					}
				} else if dropIds, ok := consolidation["drop_ids"].([]string); ok {
					for _, mid := range dropIds {
						upsertAiMemory(db, mid, chatId, "", turnId, true)
					}
				}
				newId := agentUuid4()
				upsertAiMemory(db, newId, chatId, mergedText, turnId, false)
				savedMemoryIds = append(savedMemoryIds, newId)
			}

			emitAiHelperLogEvent(
				"turn.complete", chatId, turnId, page, model, guardModel, thinkingLevel,
				"AI helper turn completed", "",
				map[string]any{
					"gen_ai.response.id":           turnId,
					"gen_ai.input.question":        question,
					"gen_ai.usage.input_tokens":    orZero(modelStats["prompt_tokens"]),
					"gen_ai.usage.output_tokens":   orZero(modelStats["completion_tokens"]),
					"gen_ai.usage.thinking_tokens": thinkingTokens,
					"gen_ai.response.latency_ms":   orZero(modelStats["elapsed_ms"]),
					"gen_ai.output.messages":       jsonDumpsNoEscape([]map[string]any{{"role": "assistant", "content": finalAnswer}}),
					"gen_ai.turn.summary.request":  summary["request"],
					"gen_ai.turn.summary.action":   summary["action"],
					"gen_ai.turn.summary.result":   summary["result"],
					"gen_ai.memory.saved_ids":      jsonDumpsNoEscape(savedMemoryIds),
				},
			)
			emitAiHelperLogEvent(
				"turn.summary", chatId, turnId, page, model, guardModel, thinkingLevel,
				"AI helper turn summary", "",
				map[string]any{
					"gen_ai.turn.summary.request": summary["request"],
					"gen_ai.turn.summary.action":  summary["action"],
					"gen_ai.turn.summary.result":  summary["result"],
				},
			)
			write(sseJsonEvent("done", map[string]any{
				"ok":               true,
				"answer":           finalAnswer,
				"model":            model,
				"chat_id":          chatId,
				"turn_id":          turnId,
				"thinking_level":   thinkingLevel,
				"turn_logs_url":    turnLogsUrl,
				"guard_stats":      guardStats,
				"model_stats":      modelStats,
				"turn_summary":     summary,
				"saved_memory_ids": savedMemoryIds,
			}))
			return nil
		}()
		if genErr != nil {
			logger.Warn("LLM endpoint stream failed", "error", genErr)
			emitAiHelperLogEvent(
				"turn.error", chatId, turnId, page, model, guardModel, thinkingLevel,
				fmt.Sprintf("LLM stream error: %s", genErr), "ERROR", nil,
			)
			write(sseJsonEvent("error", map[string]any{"error": "LLM endpoint returned no response"}))
		}
		return
	}

	loopMessages := append([]map[string]any{}, messages...)
	answerParts := []string{}
	modelStats := map[string]any{}
	proposedTools := []map[string]any{}
	maxToolRounds := 3

	for loopRound := 0; loopRound <= maxToolRounds; loopRound++ {
		roundTextParts := []string{}
		roundToolFeedback := []map[string]any{}
		streamLlmEndpoint(endpointUrl, model, apiKey, loopMessages, tools, thinkingLevel, 768, 60, func(event map[string]any) {
			eventType := rowString(event["type"])
			switch eventType {
			case "delta":
				chunk := rowString(event["text"])
				if chunk != "" {
					roundTextParts = append(roundTextParts, chunk)
					answerParts = append(answerParts, chunk)
				}
			case "tool":
				toolCall, _ := event["tool_call"].(map[string]any)
				toolName := rowString(toolCall["name"])
				toolArgs, _ := toolCall["arguments"].(map[string]any)
				if toolArgs != nil {
					var normalizedTool map[string]any
					if toolName == "propose_ui_action" {
						normalizedTool = normalizeGenericUiActionToolCall(toolArgs, page)
					}
					if normalizedTool != nil {
						actionId := strings.TrimSpace(rowString(normalizedTool["action_id"]))
						unsupported := parseBool(normalizedTool["unsupported"], false)
						actionPayload, _ := normalizedTool["action"].(map[string]any)
						if actionId != "" && !unsupported && len(actionPayload) > 0 {
							targetPage := strings.TrimSpace(rowString(actionPayload["target_page"]))
							if targetPage == "" {
								targetPage = page
							}
							if targetPage == "" {
								targetPage = "/logs"
							}
							normalizedTool["action_token"] = issueAiActionToken(
								actionId, targetPage, actionPayload,
								parseBool(normalizedTool["requires_confirmation"], true),
								chatId, turnId,
							)
						}
						status := "proposed"
						if unsupported {
							status = "unsupported"
						}
						emitAiHelperLogEvent(
							"tool.proposed", chatId, turnId, page, model, guardModel, thinkingLevel,
							fmt.Sprintf("Tool proposed: %s", toolName), "",
							map[string]any{
								"gen_ai.tool.name":                     toolName,
								"sobs.ai.action_id":                    actionId,
								"sobs.ai.tool.summary":                 rowString(normalizedTool["summary"]),
								"sobs.ai.tool.action":                  jsonDumpsNoEscape(orEmptyMap(normalizedTool["action"])),
								"sobs.ai.action.requires_confirmation": parseBool(normalizedTool["requires_confirmation"], true),
								"sobs.ai.action.status":                status,
							},
						)
						proposedTools = append(proposedTools, normalizedTool)
						roundToolFeedback = append(roundToolFeedback, map[string]any{
							"tool":                  toolName,
							"ok":                    !unsupported,
							"action_id":             actionId,
							"summary":               rowString(normalizedTool["summary"]),
							"action":                orEmptyMap(normalizedTool["action"]),
							"requires_confirmation": parseBool(normalizedTool["requires_confirmation"], true),
						})
					}
				}
			case "done":
				if s, ok := event["stats"].(map[string]any); ok {
					modelStats = s
				} else {
					modelStats = map[string]any{}
				}
			}
		})

		if len(roundToolFeedback) == 0 {
			fallbackTool := suggestChartDashboardPivotTool(question, page)
			if fallbackTool != nil {
				actionId := strings.TrimSpace(rowString(fallbackTool["action_id"]))
				unsupported := parseBool(fallbackTool["unsupported"], false)
				actionPayload, _ := fallbackTool["action"].(map[string]any)
				if actionId != "" && !unsupported && len(actionPayload) > 0 {
					targetPage := strings.TrimSpace(rowString(actionPayload["target_page"]))
					if targetPage == "" {
						targetPage = page
					}
					if targetPage == "" {
						targetPage = "/logs"
					}
					fallbackTool["action_token"] = issueAiActionToken(
						actionId, targetPage, actionPayload,
						parseBool(fallbackTool["requires_confirmation"], true),
						chatId, turnId,
					)
				}
				status := "proposed"
				if unsupported {
					status = "unsupported"
				}
				emitAiHelperLogEvent(
					"tool.proposed", chatId, turnId, page, model, guardModel, thinkingLevel,
					"Tool proposed: fallback.dashboard_chart_pivot", "",
					map[string]any{
						"gen_ai.tool.name":                     "fallback.dashboard_chart_pivot",
						"sobs.ai.action_id":                    actionId,
						"sobs.ai.tool.summary":                 rowString(fallbackTool["summary"]),
						"sobs.ai.tool.action":                  jsonDumpsNoEscape(orEmptyMap(fallbackTool["action"])),
						"sobs.ai.action.requires_confirmation": parseBool(fallbackTool["requires_confirmation"], true),
						"sobs.ai.action.status":                status,
					},
				)
				proposedTools = append(proposedTools, fallbackTool)
				roundToolFeedback = append(roundToolFeedback, map[string]any{
					"tool":                  "propose_ui_action",
					"ok":                    !unsupported,
					"action_id":             actionId,
					"summary":               rowString(fallbackTool["summary"]),
					"action":                orEmptyMap(fallbackTool["action"]),
					"requires_confirmation": parseBool(fallbackTool["requires_confirmation"], true),
				})
			}
		}

		hasPendingConfirmation := false
		for _, item := range roundToolFeedback {
			if parseBool(item["requires_confirmation"], true) {
				hasPendingConfirmation = true
				break
			}
		}
		if hasPendingConfirmation {
			break
		}

		if len(roundToolFeedback) == 0 || loopRound >= maxToolRounds {
			break
		}

		assistantRoundText := strings.TrimSpace(strings.Join(roundTextParts, ""))
		if assistantRoundText != "" {
			loopMessages = append(loopMessages, map[string]any{"role": "assistant", "content": assistantRoundText})
		} else {
			loopMessages = append(loopMessages, map[string]any{"role": "assistant", "content": "Requested tool calls for the current turn."})
		}

		toolFeedbackText := jsonDumpsNoEscape(roundToolFeedback)
		loopMessages = append(loopMessages, map[string]any{
			"role": "system",
			"content": "Tool execution results for this turn (JSON). Use these results to continue reasoning " +
				"and produce the final answer when ready: " + toolFeedbackText,
		})
	}

	answer := strings.TrimSpace(strings.Join(answerParts, ""))
	if answer == "" {
		emitAiHelperLogEvent(
			"turn.error", chatId, turnId, page, model, guardModel, thinkingLevel,
			"LLM endpoint returned no response", "ERROR", nil,
		)
		jsonResponse(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "LLM endpoint returned no response"})
		return
	}

	finalAnswer, assistantMeta := extractAssistantMeta(answer)
	metaSummary, _ := assistantMeta["turn_summary"].(map[string]any)
	summary := deriveTurnSummary(question, finalAnswer, "", metaSummary)

	savedMemoryIds := []string{}
	memoryCandidates := extractMemoryCandidates(assistantMeta)
	for _, candidate := range memoryCandidates {
		memoriesNow := loadChatMemories(db, chatId)
		related := semanticMemoryMatches(memoriesNow, candidate, 4, aiMemoryConsolidationScore)
		consolidation := consolidateMemoryCandidates(settings, candidate, related)
		action := strings.TrimSpace(rowString(consolidation["action"]))
		if action == "" {
			action = "keep_new"
		}
		if action == "ignore" {
			continue
		}
		mergedSource := consolidation["memory"]
		if rowString(mergedSource) == "" {
			mergedSource = candidate
		}
		mergedText := coerceSummaryValue(mergedSource, 280)
		if dropIds, ok := consolidation["drop_ids"].([]any); ok {
			for _, raw := range dropIds {
				upsertAiMemory(db, rowString(raw), chatId, "", turnId, true)
			}
		} else if dropIds, ok := consolidation["drop_ids"].([]string); ok {
			for _, mid := range dropIds {
				upsertAiMemory(db, mid, chatId, "", turnId, true)
			}
		}
		newId := agentUuid4()
		upsertAiMemory(db, newId, chatId, mergedText, turnId, false)
		savedMemoryIds = append(savedMemoryIds, newId)
	}

	emitAiHelperLogEvent(
		"turn.complete", chatId, turnId, page, model, guardModel, thinkingLevel,
		"AI helper turn completed", "",
		map[string]any{
			"gen_ai.response.id":           turnId,
			"gen_ai.input.question":        question,
			"gen_ai.usage.input_tokens":    orZero(modelStats["prompt_tokens"]),
			"gen_ai.usage.output_tokens":   orZero(modelStats["completion_tokens"]),
			"gen_ai.usage.thinking_tokens": orZero(modelStats["thinking_tokens"]),
			"gen_ai.response.latency_ms":   orZero(modelStats["elapsed_ms"]),
			"gen_ai.output.messages":       jsonDumpsNoEscape([]map[string]any{{"role": "assistant", "content": finalAnswer}}),
			"gen_ai.turn.summary.request":  summary["request"],
			"gen_ai.turn.summary.action":   summary["action"],
			"gen_ai.turn.summary.result":   summary["result"],
			"gen_ai.memory.saved_ids":      jsonDumpsNoEscape(savedMemoryIds),
		},
	)
	emitAiHelperLogEvent(
		"turn.summary", chatId, turnId, page, model, guardModel, thinkingLevel,
		"AI helper turn summary", "",
		map[string]any{
			"gen_ai.turn.summary.request": summary["request"],
			"gen_ai.turn.summary.action":  summary["action"],
			"gen_ai.turn.summary.result":  summary["result"],
		},
	)

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":               true,
		"answer":           finalAnswer,
		"model":            model,
		"chat_id":          chatId,
		"turn_id":          turnId,
		"thinking_level":   thinkingLevel,
		"turn_logs_url":    turnLogsUrl,
		"guard_stats":      guardStats,
		"model_stats":      modelStats,
		"turn_summary":     summary,
		"saved_memory_ids": savedMemoryIds,
		"tool_proposals":   proposedTools,
	})
}

// orEmptyMap mirrors Python `x or {}` for tool action payloads.
func orEmptyMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

// orZero mirrors Python `dict.get(key, 0)` where a missing key yields 0.
func orZero(v any) any {
	if v == nil {
		return 0
	}
	return v
}
func aiHelperExecuteAction(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	token := strings.TrimSpace(rowString(payload["action_token"]))
	if token == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "action_token is required"})
		return
	}

	decoded := decodeAiActionToken(token)
	if decoded == nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Invalid or expired action token"})
		return
	}

	actionId := strings.TrimSpace(rowString(decoded["action_id"]))
	targetPage := strings.TrimSpace(rowString(decoded["target_page"]))
	if targetPage == "" {
		targetPage = "/logs"
	}
	actionPayload := orEmptyMap(decoded["action"])
	chatId := strings.TrimSpace(rowString(decoded["chat_id"]))
	turnId := strings.TrimSpace(rowString(decoded["turn_id"]))

	actionMeta := actionMetaForPage(targetPage, actionId)
	if actionMeta == nil {
		actionMeta = actionMetaForId(actionId)
	}
	if actionMeta == nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Action is not allowed for this page"})
		return
	}
	if !parseBool(actionMeta["implemented"], false) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Action is not implemented"})
		return
	}

	actionType := rowString(actionMeta["action_type"])
	if actionType == "" {
		actionType = rowString(actionPayload["type"])
	}
	actionType = strings.ToLower(strings.TrimSpace(actionType))
	clientAction := buildClientAction(actionType, actionPayload)
	if len(clientAction) == 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Action payload is invalid"})
		return
	}

	var requiresConfirmation bool
	if v, ok := decoded["requires_confirmation"]; ok {
		requiresConfirmation = parseBool(v, true)
	} else {
		requiresConfirmation = parseBool(actionMeta["requires_confirmation"], true)
	}
	confirmed := parseBool(payload["confirm"], false)
	if requiresConfirmation && !confirmed {
		jsonResponse(w, http.StatusConflict, map[string]any{
			"ok":                    false,
			"error":                 "Confirmation required",
			"requires_confirmation": true,
		})
		return
	}

	emitAiHelperLogEvent(
		"tool.executed", chatId, turnId, targetPage, "", "", "off",
		fmt.Sprintf("Executed action: %s", actionId), "",
		map[string]any{
			"gen_ai.tool.name":      "propose_ui_action",
			"sobs.ai.action_id":     actionId,
			"sobs.ai.tool.action":   jsonDumpsNoEscape(clientAction),
			"sobs.ai.action.status": "executed",
		},
	)

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":            true,
		"action_id":     actionId,
		"client_action": clientAction,
		"chat_id":       chatId,
		"turn_id":       turnId,
	})
}

// ---------------------------------------------------------------------------
// Agent Runs API  GET /api/agent/runs
//                 POST /api/agent/runs          (trigger manual run)
//                 POST /api/agent/runs/<id>/dismiss
// ---------------------------------------------------------------------------

// pySliceStr mirrors Python s[:n] (code-point slice).
func pySliceStr(s string, n int) string {
	if n < 0 {
		n = 0
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func buildUserIssueTriggerContext(sourcePage string, payload map[string]any) map[string]any {
	source := strings.ToLower(strings.TrimSpace(sourcePage))
	switch source {
	case "errors", "traces", "incident":
	default:
		source = "errors"
	}

	service := strings.TrimSpace(rowString(payload["service"]))
	traceId := strings.TrimSpace(rowString(payload["trace_id"]))
	spanId := strings.TrimSpace(rowString(payload["span_id"]))
	errorId := strings.TrimSpace(rowString(payload["error_id"]))
	errType := strings.TrimSpace(rowString(payload["err_type"]))
	spanName := strings.TrimSpace(rowString(payload["span_name"]))
	status := strings.TrimSpace(rowString(payload["status"]))
	message := strings.TrimSpace(rowString(payload["message"]))
	stack := strings.TrimSpace(rowString(payload["stack"]))

	var signalSource, signalName, anomalyState, triggerRefId string
	var signalValue float64

	if source == "errors" {
		signalSource = "errors"
		signalName = errType
		if signalName == "" {
			signalName = "exception"
		}
		anomalyState = "critical"
		signalValue = 1.0
		triggerRefId = errorId
	} else if source == "traces" {
		signalSource = "traces"
		signalName = spanName
		if signalName == "" {
			signalName = "trace_span"
		}
		anomalyState = "warning"
		if strings.Contains(strings.ToUpper(status), "ERROR") {
			anomalyState = "critical"
		}
		signalValue = 0.0
		if raw := payload["duration_ms"]; agentTruthy(raw) {
			if f, ok := coerceFloat(raw); ok {
				signalValue = f
			} else {
				signalValue = 0.0
			}
		}
		triggerRefId = traceId
		if triggerRefId == "" {
			triggerRefId = spanId
		}
	} else {
		signalSource = "incident"
		signalName = errType
		if signalName == "" {
			signalName = spanName
		}
		if signalName == "" {
			signalName = "incident_packet"
		}
		anomalyState = "warning"
		if errorId != "" || strings.Contains(strings.ToUpper(status), "ERROR") {
			anomalyState = "critical"
		}
		signalValue = 1.0
		if raw := payload["duration_ms"]; agentTruthy(raw) {
			if f, ok := coerceFloat(raw); ok {
				signalValue = f
			} else {
				signalValue = 1.0
			}
		}
		triggerRefId = errorId
		if triggerRefId == "" {
			triggerRefId = traceId
		}
		if triggerRefId == "" {
			triggerRefId = spanId
		}
	}

	extra := map[string]any{
		"initiated_by":       "user",
		"source_page":        source,
		"source":             signalSource,
		"signal":             signalName,
		"state":              anomalyState,
		"value":              signalValue,
		"service":            service,
		"trace_id":           traceId,
		"span_id":            spanId,
		"error_id":           errorId,
		"err_type":           errType,
		"message":            pySliceStr(message, 1200),
		"stack":              pySliceStr(stack, 3000),
		"url":                strings.TrimSpace(rowString(payload["url"])),
		"timestamp":          strings.TrimSpace(rowString(payload["timestamp"])),
		"additional_context": pySliceStr(strings.TrimSpace(rowString(payload["additional_context"])), 2000),
	}

	return map[string]any{
		"rule_name":      fmt.Sprintf("User Raised Issue (%s)", source),
		"trigger_state":  anomalyState,
		"trigger_type":   "manual",
		"trigger_ref_id": triggerRefId,
		"service":        service,
		"extra":          extra,
	}
}
func raiseIssueFromUserObservation(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	sourcePage := strings.TrimSpace(rowString(payload["source_page"]))
	if sourcePage == "" {
		sourcePage = "errors"
	}
	sourcePage = strings.ToLower(sourcePage)
	assignCopilot := parseBool(payload["assign_copilot"], false)
	maskOutput := parseBool(payload["mask_output"], true)

	db := getDb()
	settings := loadAllAiSettings(db)
	if settings["ai.endpoint_url"] == "" || settings["ai.model"] == "" {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "AI endpoint not configured. Visit Settings -> AI Configuration.",
		})
		return
	}

	triggerContext := buildUserIssueTriggerContext(sourcePage, payload)
	if extra, ok := triggerContext["extra"].(map[string]any); ok {
		extra["mask_output"] = maskOutput
	}
	githubRepo, githubToken := resolveAgentGithubTarget(db, settings, triggerContext)
	if githubRepo == "" || githubToken == "" {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "GitHub repo/token not configured for issue creation. Visit Settings -> AI Configuration.",
		})
		return
	}

	actions := []any{"analyze", "github_issue", "dlp_check"}
	if assignCopilot {
		actions = append(actions, "github_issue_copilot")
	}
	rule := map[string]any{
		"id":                 fmt.Sprintf("user-observation-%s", sourcePage),
		"name":               fmt.Sprintf("User Raised Issue (%s)", sourcePage),
		"actions":            actions,
		"rate_limit_minutes": 0,
	}

	outcome := runAgentRuleInstance(db, rule, settings, triggerContext)
	if !parseBool(outcome["ok"], false) {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{
			"ok":     false,
			"error":  rowStringOr(outcome["error"], "agent flow failed"),
			"run_id": rowString(outcome["run_id"]),
		})
		return
	}

	result := orEmptyMap(outcome["result"])
	issueUrl := rowString(result["github_issue_url"])
	dedupDecision := rowString(result["dedup_decision"])
	issueError := strings.TrimSpace(rowString(result["issue_error"]))
	if issueUrl != "" {
		owner, repo, issueNumber := parseIssueRefFromUrl(issueUrl)
		if owner == "" || repo == "" || issueNumber <= 0 {
			if issueError == "" {
				issueError = "Agent returned an invalid issue URL"
			}
			dedupDecision = "create_failed"
			issueUrl = ""
		}
	}
	if issueUrl == "" && dedupDecision == "create_failed" {
		errMsg := issueError
		if errMsg == "" {
			errMsg = "GitHub issue creation failed. Check repository settings and token scopes."
		}
		jsonResponse(w, http.StatusBadGateway, map[string]any{
			"ok":          false,
			"error":       errMsg,
			"run_id":      rowString(outcome["run_id"]),
			"source":      "user",
			"source_page": sourcePage,
		})
		return
	}
	if issueUrl == "" && dedupDecision == "suppressed_rate_limit" {
		jsonResponse(w, http.StatusTooManyRequests, map[string]any{
			"ok":          false,
			"error":       "GitHub issue creation suppressed by hourly limit. Try again later.",
			"run_id":      rowString(outcome["run_id"]),
			"source":      "user",
			"source_page": sourcePage,
		})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":                        true,
		"run_id":                    rowString(outcome["run_id"]),
		"source":                    "user",
		"source_page":               sourcePage,
		"issue_url":                 issueUrl,
		"dedup_decision":            dedupDecision,
		"copilot_assignment_status": rowString(result["copilot_assignment_status"]),
		"copilot_assignment_reason": rowString(result["copilot_assignment_reason"]),
		"status":                    rowString(result["status"]),
	})
}
func listAgentRuns(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			limit = max(1, min(200, v))
		}
	}
	runs := loadAgentRuns(db, limit)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "runs": runs})
}

// triggerAgentRun manually triggers an agent flow for a given rule_id.
func triggerAgentRun(w http.ResponseWriter, r *http.Request) {
	payload, _ := readJsonBody(r)
	ruleId := strings.TrimSpace(rowString(payload["rule_id"]))
	extraContext := strings.TrimSpace(rowString(payload["extra_context"]))

	if ruleId == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "rule_id is required"})
		return
	}

	db := getDb()
	rule := loadAgentRule(db, ruleId)
	if rule == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent rule not found"})
		return
	}

	settings := loadAllAiSettings(db)
	if settings["ai.endpoint_url"] == "" || settings["ai.model"] == "" {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "AI endpoint not configured. Visit Settings → AI Configuration.",
		})
		return
	}

	// Rate limit check
	rateLimitMinutes := 60.0
	if v, ok := coerceFloat(rule["rate_limit_minutes"]); ok {
		rateLimitMinutes = v
	}
	lastRunTs := agentRuleLastRunTs(db, ruleId)
	elapsedMinutes := (float64(time.Now().UnixNano())/1e9 - lastRunTs) / 60.0
	if elapsedMinutes < rateLimitMinutes && lastRunTs > 0 {
		jsonResponse(w, http.StatusTooManyRequests, map[string]any{
			"ok": false,
			"error": fmt.Sprintf("Rate limit: this rule ran %.0fm ago (limit: every %vm)",
				elapsedMinutes, rateLimitMinutes),
		})
		return
	}

	triggerContext := map[string]any{
		"rule_name":      rule["name"],
		"trigger_state":  "manual",
		"trigger_type":   "manual",
		"trigger_ref_id": "",
		"extra":          extraContext,
	}
	outcome := runAgentRuleInstance(db, rule, settings, triggerContext)
	if !parseBool(outcome["ok"], false) {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{
			"ok":     false,
			"error":  rowStringOr(outcome["error"], "agent flow failed"),
			"run_id": outcome["run_id"],
		})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "run_id": outcome["run_id"], "result": outcome["result"]})
}

func dismissAgentRun(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	runId := r.PathValue("run_id")
	res, err := db.Execute(
		"SELECT Id, RuleId, RuleName, TriggerContext, Status, GuardDecision, DlpResult, "+
			"Analysis, Suggestion, GithubIssueUrl, ErrorMessage, CreatedAt, CompletedAt "+
			"FROM sobs_agent_runs FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1",
		runId,
	)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	row := res.Fetchone()
	if row == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "run not found"})
		return
	}
	insertRowsJsonEachRow(db, "sobs_agent_runs", []Row{
		{
			"Id":             runId,
			"RuleId":         rowString(row["RuleId"]),
			"RuleName":       rowString(row["RuleName"]),
			"TriggerContext": rowString(row["TriggerContext"]),
			"Status":         rowString(row["Status"]),
			"GuardDecision":  rowString(row["GuardDecision"]),
			"DlpResult":      rowString(row["DlpResult"]),
			"Analysis":       rowString(row["Analysis"]),
			"Suggestion":     rowString(row["Suggestion"]),
			"GithubIssueUrl": rowString(row["GithubIssueUrl"]),
			"ErrorMessage":   rowString(row["ErrorMessage"]),
			"CreatedAt":      rowString(row["CreatedAt"]),
			"CompletedAt":    rowString(row["CompletedAt"]),
			"IsDismissed":    1,
			"IsDeleted":      0,
			"Version":        time.Now().UnixMilli(),
		},
	})
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
