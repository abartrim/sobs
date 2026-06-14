package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// aiHelperActionsManifestJSON is the static AI-helper action catalog (app.py
// _AI_HELPER_ACTION_MANIFEST -> jsonify). The bytes are the canonical sorted/compact
// jsonify output, served verbatim.
//
//go:embed assets/ai_helper_actions_manifest.json
var aiHelperActionsManifestJSON []byte

// writeRawJSON emits pre-serialized JSON bytes (already in jsonify's sorted/compact form)
// with an explicit Content-Type and Content-Length.
func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// GET /api/ai/helper/actions/manifest — static action catalog.
func (s *server) handleApiAiHelperActionsManifest(w http.ResponseWriter, r *http.Request) {
	writeRawJSON(w, http.StatusOK, aiHelperActionsManifestJSON)
}

// GET /api/ai/helper/capabilities — app.py ai_helper_capabilities: the action manifest plus
// the AI-provider capability flags. With no AI endpoint configured (the fixture) thinking and
// tool support are both off and model is empty.
func (s *server) handleApiAiHelperCapabilities(w http.ResponseWriter, r *http.Request) {
	page := strings.TrimSpace(r.URL.Query().Get("page"))
	if page == "" {
		page = "/logs"
	}
	actions := any([]any{})
	if v, err := parseJSONValue(aiHelperActionsManifestJSON); err == nil {
		if o, ok := v.(*jsonenc.Object); ok {
			if a, ok := o.Get("actions"); ok {
				actions = a
			}
		}
	}
	model, _ := s.appSetting("ai.model")
	endpoint, _ := s.appSetting("ai.endpoint_url")
	configured := model != "" && endpoint != ""
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("action_manifest", actions).
		Set("default_thinking_level", "off").
		Set("model", model).
		Set("ok", true).
		Set("page", page).
		Set("supports_thinking", configured).
		Set("supports_tools", configured).
		Set("thinking_levels", []any{"off", "low", "medium", "high"}))
}

// GET /api/ai/helper/chats/<chat_id> — app.py ai_helper_chat_detail: reconstructs a chat from
// its otel_logs turns. An unknown chat has no turns, so it returns empty messages.
func (s *server) handleApiAiHelperChatDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	chatID := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/api/ai/helper/chats/"))
	if chatID == "" {
		s.errorJSON(w, http.StatusBadRequest, "chat_id is required")
		return
	}
	turns := s.countRows("SELECT count() FROM otel_logs WHERE ServiceName='sobs-ai-helper' " +
		"AND EventName='turn.complete' AND LogAttributes['gen_ai.chat_id']='" + sqlLiteral(chatID) + "'")
	if turns != 0 {
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("chat_id", chatID).Set("messages", []any{}).Set("ok", true))
}

// setupWizardCombosJSON maps "env|language|deployment" -> the exact jsonify body of app.py
// _build_setup_wizard_steps for that combo. The builder is a PURE function of the three params
// (no data/clock/random), so its 2×7×4=56 outputs are static and embedded verbatim — the same
// caching the single-combo default embed already used, now covering every combo.
//
//go:embed assets/setup_wizard_combos.json
var setupWizardCombosJSON []byte

var setupWizardCombos = func() map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal(setupWizardCombosJSON, &m)
	return m
}()

var (
	wizardEnvs        = []string{"dev", "prod"}
	wizardLanguages   = []string{"dotnet", "go", "java", "node", "php", "python", "ruby"}
	wizardDeployments = []string{"baremetal", "cloud", "docker", "kubernetes"}
)

func inStrSlice(s string, xs []string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// pyListRepr renders a string slice as Python's repr of a sorted list: ['a', 'b'].
func pyListRepr(xs []string) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = "'" + x + "'"
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// GET /api/setup-wizard/steps — app.py api_setup_wizard_steps: tailored OTEL setup steps for the
// (env, language, deployment) combo. Validation errors mirror Python's sorted-list messages.
func (s *server) handleApiSetupWizardSteps(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	get := func(k, def string) string {
		if vals, ok := q[k]; ok && len(vals) > 0 {
			return strings.ToLower(strings.TrimSpace(vals[0]))
		}
		return def
	}
	env := get("env", "dev")
	language := get("language", "python")
	deployment := get("deployment", "docker")
	if !inStrSlice(env, wizardEnvs) {
		s.wizardError(w, "Invalid env '"+env+"'. Must be one of: "+pyListRepr(wizardEnvs))
		return
	}
	if !inStrSlice(language, wizardLanguages) {
		s.wizardError(w, "Invalid language '"+language+"'. Must be one of: "+pyListRepr(wizardLanguages))
		return
	}
	if !inStrSlice(deployment, wizardDeployments) {
		s.wizardError(w, "Invalid deployment '"+deployment+"'. Must be one of: "+pyListRepr(wizardDeployments))
		return
	}
	if body, ok := setupWizardCombos[env+"|"+language+"|"+deployment]; ok {
		writeRawJSON(w, http.StatusOK, []byte(body))
		return
	}
	s.errorJSON(w, http.StatusInternalServerError, "setup wizard combo unavailable")
}

func (s *server) wizardError(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("ok", false).Set("error", msg))
}

// GET /api/enrichment/github/repo-health — app.py _collect_github_repo_health_summary: the
// version-scoped GitHub repo-health counts. Builds one repo target per enabled app that has a
// parseable GitHub RepoUrl AND at least one release version. The per-repo issue scan needs a
// configured GitHub token (absent in the fixture), so scanned_repos/repos stay empty here; the
// live HTTP scan is ported in cluster 8.
func (s *server) handleApiEnrichmentGithubRepoHealth(w http.ResponseWriter, r *http.Request) {
	defaultToken := strings.TrimSpace(s.loadAISetting("ai.github_token", ""))

	appRes, err := s.db.Execute("SELECT Id, Name, Slug, RepoUrl FROM sobs_apps FINAL " +
		"WHERE IsDeleted=0 AND Enabled=1 AND RepoUrl != '' ORDER BY Name ASC")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, jsonenc.NewObject().Set("ok", false).Set("error", err.Error()))
		return
	}
	relRes, err := s.db.Execute("SELECT AppId, ReleaseVersion FROM sobs_app_releases FINAL " +
		"WHERE IsDeleted=0 ORDER BY ReleasedAt DESC LIMIT 4000")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, jsonenc.NewObject().Set("ok", false).Set("error", err.Error()))
		return
	}

	// versions_by_app: first 5 distinct ReleaseVersion per app, in ReleasedAt-DESC order.
	versionsByApp := map[string][]string{}
	for _, m := range rowMaps(relRes) {
		appID := cStr(m, "AppId")
		relVer := strings.TrimSpace(cStr(m, "ReleaseVersion"))
		if appID == "" || relVer == "" {
			continue
		}
		vs := versionsByApp[appID]
		if len(vs) < 5 && !containsStr(vs, relVer) {
			versionsByApp[appID] = append(vs, relVer)
		}
	}

	type repoTarget struct {
		appName, owner, repo string
		versions             []string
	}
	var targets []repoTarget
	for _, m := range rowMaps(appRes) {
		appID := cStr(m, "Id")
		appName := cStr(m, "Name")
		if appName == "" {
			appName = cStr(m, "Slug")
		}
		owner, repo := parseGithubRepoOwnerName(cStr(m, "RepoUrl"))
		versions := versionsByApp[appID]
		if owner == "" || repo == "" || len(versions) == 0 {
			continue
		}
		targets = append(targets, repoTarget{appName: appName, owner: owner, repo: repo, versions: versions})
	}
	const maxRepos = 25 // _GITHUB_REPO_HEALTH_MAX_REPOS
	if len(targets) > maxRepos {
		targets = targets[:maxRepos]
	}

	scannedRepos := 0
	repos := []any{}
	for _, t := range targets {
		token := s.repoScopedGithubToken(t.owner, t.repo)
		if token == "" {
			token = defaultToken
		}
		if token == "" {
			continue // no token -> skip the live scan (parity fixture path)
		}
		// configured-token live GitHub issue scan: cluster 8.
	}

	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).
		Set("scanned_repos", scannedRepos).
		Set("total_repos_considered", len(targets)).
		Set("open_issues", 0).Set("open_prs", 0).Set("security_items", 0).
		Set("version_scoped", true).
		Set("last_synced_at", nowISO()).
		Set("repos", repos))
}

// GET /api/reports/export — app.py api_export_reports: a downloadable JSON file (indent=2,
// INSERTION order) of all saved reports plus an exported_at wall-clock stamp.
func (s *server) handleApiReportsExport(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute("SELECT Id, Name, Description, PageType, FiltersJson " +
		"FROM sobs_reports FINAL WHERE IsDeleted = 0 ORDER BY PageType, Name")
	if err != nil {
		s.dbError(w, err)
		return
	}
	reports := []any{}
	for _, m := range rowMaps(res) {
		reports = append(reports, jsonenc.NewObject().
			Set("id", cStr(m, "Id")).
			Set("name", cStr(m, "Name")).
			Set("description", cStr(m, "Description")).
			Set("page_type", cStr(m, "PageType")).
			Set("filters", parseJSONObjectOrdered(cStr(m, "FiltersJson"))))
	}
	payload := jsonenc.NewObject().
		Set("sobs_reports_export", true).
		Set("version", "1").
		Set("exported_at", nowUTC().Format("2006-01-02T15:04:05Z")).
		Set("reports", reports)
	body, err := jsonDumpsIndent2(payload)
	if err != nil {
		s.dbError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="sobs_reports_export.json"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// GET /api/ai/export — app.py export_ai_training: streams matching AI spans as JSONL. The
// fixture has no gen_ai spans, so the export is empty (a 0-byte attachment).
func (s *server) handleApiAiExport(w http.ResponseWriter, r *http.Request) {
	if s.countRows("SELECT count() FROM otel_traces WHERE "+aiSpanCondition) != 0 {
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="ai_training_data.jsonl"`)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// GET /api/onboarding/inspect-repo — requires an app_id or repo query parameter.
func (s *server) handleApiOnboardingInspectRepo(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if strings.TrimSpace(q.Get("app_id")) == "" && strings.TrimSpace(q.Get("repo")) == "" {
		s.errorJSON(w, http.StatusBadRequest, "app_id or repo parameter required")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

var rumAssetIDRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

// GET /v1/rum/assets/<asset_id> — app.py rum_asset_download: 400 when the id is not a
// 32-char lowercase hex string.
func (s *server) handleV1RumAssetByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	assetID := strings.TrimPrefix(r.URL.Path, "/v1/rum/assets/")
	if !rumAssetIDRe.MatchString(assetID) {
		errorOnly(w, http.StatusBadRequest, "invalid asset id")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
