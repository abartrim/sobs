package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// bodyMap decodes the JSON request body once into a map for field-presence checks. An empty
// or non-JSON body yields an empty map (matching Quart's `await request.get_json(silent=True)
// or {}` idiom).
func bodyMap(r *http.Request) map[string]any {
	m := map[string]any{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&m)
	}
	return m
}

// bstr returns m[key] as a trimmed string ("" when absent/non-string), mirroring
// `str(payload.get(key) or "").strip()`.
func bstr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// errorOnly writes {"error": msg} (NO ok field) at the given status — used by the handlers
// whose Python error path is `jsonify({"error": ...})`.
func errorOnly(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, jsonenc.NewObject().Set("error", msg))
}

// handleApiAiHelper is defined in ai_helper.go.

// handleApiAiHelperExecute is defined in ai_action_execute.go.

// handleApiAiHelperFeedback is defined in ai_emit.go.

// handleApiDashboardsSpecAiBuild is defined in ai_build.go.

// ---- Notifications / onboarding (field-required 400s) --------------------------------

// POST /api/notifications/subscribe — app.py subscribe_browser_push: register a browser push
// subscription as a browser_push notification channel (dedup by endpoint).
func (s *server) handleApiNotificationsSubscribe(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	name := strings.TrimSpace(orDefault(bstr(m, "name"), "Browser Push"))
	endpoint := strings.TrimSpace(bstr(m, "endpoint"))
	p256dh := strings.TrimSpace(bstr(m, "p256dh"))
	auth := strings.TrimSpace(bstr(m, "auth"))
	if endpoint == "" || p256dh == "" || auth == "" {
		s.errorJSON(w, http.StatusBadRequest, "endpoint, p256dh, and auth are required")
		return
	}
	// Dedup: an existing browser_push channel with the same endpoint short-circuits.
	res, err := s.db.Execute("SELECT Id, ConfigJson FROM sobs_notification_channels FINAL " +
		"WHERE IsDeleted = 0 AND ChannelType = 'browser_push'")
	if err != nil {
		s.dbError(w, err)
		return
	}
	for _, row := range rowMaps(res) {
		cfg := parseJSONObject(cStr(row, "ConfigJson"))
		if ep, _ := cfg.Get("endpoint"); toStr(ep) == endpoint {
			writeJSON(w, http.StatusOK, jsonenc.NewObject().
				Set("ok", true).Set("channel_id", cStr(row, "Id")).Set("existing", true))
			return
		}
	}
	channelID := newUUIDv4()
	cfg := jsonenc.NewObject().Set("endpoint", endpoint).Set("p256dh", p256dh).Set("auth", auth)
	row := map[string]any{
		"Id": channelID, "Name": name, "ChannelType": "browser_push",
		"ConfigJson": jsonenc.Encode(cfg, jsonenc.Options{SortKeys: false}),
		"Enabled":    1, "IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_notification_channels", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("channel_id", channelID).Set("existing", false))
}

// handleApiOnboardingCreateIssues is defined in onboarding_issues.go.

// bodyBool mirrors bool(body.get(key, default)).
func bodyBool(m map[string]any, key string, def bool) bool {
	if v, ok := m[key]; ok {
		return truthy(v)
	}
	return def
}

// POST /api/onboarding/create-repo — app.py api_onboarding_create_repo: register a repo/app row
// (sobs_apps) and optionally persist GitHub-token settings, returning the created app.
func (s *server) handleApiOnboardingCreateRepo(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	name := bstr(m, "name")
	repoURL, owner, repo := resolveGithubRepoFields(bstr(m, "repo_url"), bstr(m, "repo_owner"), bstr(m, "repo_name"))
	if name == "" || repoURL == "" {
		s.errorJSON(w, http.StatusBadRequest, "App name and repository are required")
		return
	}
	slugSrc := bstr(m, "slug")
	if slugSrc == "" {
		slugSrc = name
	}
	slug := appSlug(slugSrc, "app")
	if s.rowExists("SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 LIMIT 1", slug) {
		s.writeMaskedJSON(w, http.StatusConflict,
			jsonenc.NewObject().Set("ok", false).Set("error", "App slug already exists"))
		return
	}
	appID := newUUIDHex()
	now := nowISO()
	row := map[string]any{
		"Id": appID, "Name": name, "Slug": slug, "OwnerTeam": "", "RepoUrl": repoURL,
		"DefaultEnvironment": bstr(m, "default_environment"), "Enabled": 1, "MetadataJson": "{}",
		"IsDeleted": 0, "Version": fixedVersionMillis(), "CreatedAt": now, "UpdatedAt": now,
	}
	if _, err := s.insertRowsNormalized("sobs_apps", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	githubToken := bstr(m, "github_token")
	if bodyBool(m, "set_github_token", false) && githubToken != "" {
		s.saveAISetting("ai.github_token", githubToken)
		s.saveAISetting("ai.github_token_expires_at", normalizeGithubTokenExpiry(bstr(m, "github_token_expires_at")))
		s.saveAISetting("ai.github_token_last_validated_at", "")
		s.saveAISetting("ai.github_token_last_validation_status", "")
		s.saveAISetting("ai.github_token_last_validation_message", "")
	}
	if bodyBool(m, "set_repo_token", true) && githubToken != "" && owner != "" && repo != "" {
		s.saveAISetting(githubRepoTokenKey(owner, repo), githubToken)
	}
	if bodyBool(m, "set_agent_repo", true) && owner != "" && repo != "" {
		s.saveAISetting("ai.github_repo", owner+"/"+repo)
	}
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("app_id", appID).Set("name", name).Set("slug", slug).
		Set("repo_url", repoURL).Set("owner", owner).Set("repo", repo))
}

// POST /api/onboarding/import-repo — app.py api_onboarding_import_repo: fetch repo metadata from
// GitHub (GET /repos/{owner}/{repo}) for onboarding form auto-fill. The token (body override or
// ai.github_token) only sets request headers, which don't change the canned lookup.
func (s *server) handleApiOnboardingImportRepo(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	_, owner, repo := resolveGithubRepoFields(bstr(m, "repo_url"), bstr(m, "repo_owner"), bstr(m, "repo_name"))
	if owner == "" || repo == "" {
		s.errorJSON(w, http.StatusBadRequest, "Enter a valid GitHub owner and repository name")
		return
	}
	// Mirror api_onboarding_import_repo: token from the body override or ai.github_token; auth
	// headers when present, public headers otherwise (the canned lookup ignores them under parity).
	githubToken := strings.TrimSpace(bstr(m, "github_token"))
	if githubToken == "" {
		githubToken = strings.TrimSpace(s.loadAISetting("ai.github_token", ""))
	}
	resp, err := s.upstreamRequest("GET", "https://api.github.com/repos/"+owner+"/"+repo,
		nil, githubRequestHeaders(githubToken, false))
	if err != nil {
		s.writeMaskedJSON(w, http.StatusBadGateway,
			jsonenc.NewObject().Set("ok", false).Set("error", "GitHub lookup failed: "+err.Error()))
		return
	}
	if resp.Status != 200 {
		detail := ""
		if obj, ok := resp.Body.(*jsonenc.Object); ok {
			detail = objStrOr(obj, "message")
		}
		if detail == "" {
			detail = fmt.Sprintf("GitHub lookup failed (%d)", resp.Status)
		}
		s.writeMaskedJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", detail))
		return
	}
	obj, ok := resp.Body.(*jsonenc.Object)
	if !ok {
		s.writeMaskedJSON(w, http.StatusBadGateway,
			jsonenc.NewObject().Set("ok", false).Set("error", "Unexpected GitHub response payload"))
		return
	}
	fullName := objStrOr(obj, "full_name")
	if fullName == "" {
		fullName = owner + "/" + repo
	}
	importedRepoURL := objStrOr(obj, "html_url")
	if importedRepoURL == "" {
		importedRepoURL = "https://github.com/" + owner + "/" + repo
	}
	suggestedName := objStrOr(obj, "name")
	if suggestedName == "" {
		suggestedName = repo
	}
	visibility := objStrOr(obj, "visibility")
	if visibility == "" {
		visibility = "public"
	}
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("owner", owner).Set("repo", repo).
		Set("full_name", fullName).Set("repo_url", importedRepoURL).
		Set("name", suggestedName).Set("slug", appSlug(suggestedName, "app")).
		Set("default_branch", objStrOr(obj, "default_branch")).
		Set("visibility", visibility).Set("description", objStrOr(obj, "description")))
}

// POST /api/onboarding/list-repos — app.py api_onboarding_list_repos: list an owner's repos via
// GitHub (users then orgs endpoint), shaped to {name, full_name, repo_url, private} and sorted.
func (s *server) handleApiOnboardingListRepos(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	owner := strings.Trim(bstr(m, "owner"), "/")
	if owner == "" {
		s.errorJSON(w, http.StatusBadRequest, "Owner or username is required")
		return
	}
	githubToken := bstr(m, "github_token")
	if githubToken == "" {
		githubToken = strings.TrimSpace(s.loadAISetting("ai.github_token", ""))
	}
	tokenUsed := githubToken != ""
	typeParam := "public"
	if tokenUsed {
		typeParam = "all"
	}
	endpoints := []string{
		"https://api.github.com/users/" + owner + "/repos?per_page=100&type=" + typeParam + "&sort=full_name",
		"https://api.github.com/orgs/" + owner + "/repos?per_page=100&type=" + typeParam + "&sort=full_name",
	}
	var payload any
	responseStatus := 0
	for _, url := range endpoints {
		resp, err := s.upstreamRequest("GET", url, nil, githubRequestHeaders(githubToken, false))
		if err != nil {
			s.writeMaskedJSON(w, http.StatusBadGateway,
				jsonenc.NewObject().Set("ok", false).Set("error", "GitHub lookup failed: "+err.Error()))
			return
		}
		responseStatus = resp.Status
		payload = resp.Body
		if responseStatus == 200 {
			break
		}
	}
	list, isList := payload.([]any)
	if responseStatus != 200 || !isList {
		detail := ""
		if obj, ok := payload.(*jsonenc.Object); ok {
			detail = objStrOr(obj, "message")
		}
		if detail == "" {
			detail = fmt.Sprintf("GitHub lookup failed (%d)", responseStatus)
		}
		s.writeMaskedJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", detail))
		return
	}
	repos := []any{}
	for _, itAny := range list {
		item, ok := itAny.(*jsonenc.Object)
		if !ok {
			continue
		}
		repoName := objStrOr(item, "name")
		if repoName == "" {
			continue
		}
		repoOwner := owner
		if ownerObj, ok := objSub(item, "owner"); ok {
			if login := objStrOr(ownerObj, "login"); login != "" {
				repoOwner = login
			}
		}
		fullName := objStrOr(item, "full_name")
		if fullName == "" {
			fullName = repoOwner + "/" + repoName
		}
		repoURL := objStrOr(item, "html_url")
		if repoURL == "" {
			repoURL = buildGithubRepoURL(repoOwner, repoName)
		}
		repos = append(repos, jsonenc.NewObject().
			Set("name", repoName).Set("full_name", fullName).
			Set("repo_url", repoURL).Set("private", objTruthy(item, "private")))
	}
	sort.SliceStable(repos, func(i, j int) bool {
		return strings.ToLower(objStrOr(repos[i].(*jsonenc.Object), "name")) <
			strings.ToLower(objStrOr(repos[j].(*jsonenc.Object), "name"))
	})
	visNote := ""
	if !tokenUsed {
		visNote = "Need PAT to see private repositories."
	}
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("owner", owner).Set("repos", repos).
		Set("token_used", tokenUsed).Set("visibility_note", visNote))
}

// ---- Dashboards query/spec (error-only 400s) -----------------------------------------

// specSQLMode returns the `spec.sql.mode` string from a decoded body (or "" when absent).
func specSQLMode(m map[string]any) string {
	spec, _ := m["spec"].(map[string]any)
	sql, _ := spec["sql"].(map[string]any)
	mode, _ := sql["mode"].(string)
	return strings.TrimSpace(mode)
}

// POST /api/dashboards/render — app.py render_chart: execute a query and render it with a
// template to produce the eCharts option.
func (s *server) handleApiDashboardsRender(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	query := bstr(m, "query")
	templateID := strings.TrimSpace(orDefault(bstr(m, "template_id"), "time_series_percentiles"))
	if e := validateChartQuery(query); e != "" {
		errorOnly(w, http.StatusBadRequest, e)
		return
	}
	if _, ok := chartTemplateMeta[templateID]; !ok {
		errorOnly(w, http.StatusBadRequest, "Unknown template: "+templateID)
		return
	}
	res, err := s.db.Execute(injectLimit(query, 1000))
	if err != nil {
		errorOnly(w, http.StatusBadRequest, publicDashboardQueryError(err))
		return
	}
	columns, rows := serializeQueryDictRows(res)
	option, errMsg := s.renderChartFromTemplate(templateID, columns, rows, nil)
	if errMsg != "" {
		errorOnly(w, http.StatusBadRequest, errMsg)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("option", option))
}

// specModeGuard rejects a spec whose sql.mode is not 'builder' or 'raw' (the first gate in
// _compile_chart_spec). Returns true when it wrote the error.
func specModeGuard(w http.ResponseWriter, r *http.Request, extra func(o *jsonenc.Object)) bool {
	mode := specSQLMode(bodyMap(r))
	if mode != "builder" && mode != "raw" {
		o := jsonenc.NewObject().Set("error", "sql.mode must be 'builder' or 'raw'")
		if extra != nil {
			extra(o)
		}
		writeJSON(w, http.StatusBadRequest, o)
		return true
	}
	return false
}

// POST /api/dashboards/spec/compile — app.py compile_chart_spec_api: compile a chart spec to
// (template_id, query, normalized spec). A ValueError-style failure returns {"error": msg} 400.
func (s *server) handleApiDashboardsSpecCompile(w http.ResponseWriter, r *http.Request) {
	tid, query, spec, errMsg := s.compileChartSpec(specFromBody(r))
	if errMsg != "" {
		errorOnly(w, http.StatusBadRequest, errMsg)
		return
	}
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("template_id", tid).Set("query", query).Set("spec", spec))
}

// POST /api/dashboards/spec/render — app.py render_chart_spec_api: compile, execute, render to an
// eCharts option + apply the spec's visual overrides, masked through the output-masking wrapper.
func (s *server) handleApiDashboardsSpecRender(w http.ResponseWriter, r *http.Request) {
	tid, query, spec, errMsg := s.compileChartSpec(specFromBody(r))
	if errMsg != "" {
		errorOnly(w, http.StatusBadRequest, errMsg)
		return
	}
	res, err := s.db.Execute(injectLimit(query, 1000))
	if err != nil {
		errorOnly(w, http.StatusBadRequest, publicDashboardQueryError(err))
		return
	}
	columns, rows := serializeQueryDictRows(res)
	option, rErr := s.renderChartFromTemplate(tid, columns, rows, spec)
	if rErr != "" {
		errorOnly(w, http.StatusBadRequest, rErr)
		return
	}
	option = applyChartSpecVisualOverrides(tid, option, spec)
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("template_id", tid).Set("query", query).Set("spec", spec).Set("option", option))
}

// ---- Fixed-response success/guard ----------------------------------------------------

// handleApiIssuesRaise is defined in agent_flow.go.

// POST /api/mcp/enabled — app.py(mcp.py) mcp_api_set_enabled: enabled = bool(body.get(
// "enabled", True)); persists and echoes. (The persist is an unobservable side effect here.)
func (s *server) handleApiMcpEnabled(w http.ResponseWriter, r *http.Request) {
	enabled := true
	if v, ok := bodyMap(r)["enabled"]; ok {
		enabled = truthy(v)
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("enabled", enabled).Set("ok", true))
}

// POST /api/settings/masking/preview — mask the supplied value; an empty/absent value masks
// to the empty string.
func (s *server) handleApiSettingsMaskingPreview(w http.ResponseWriter, r *http.Request) {
	// Parse with the ordered decoder so a dict/list `value` becomes a *jsonenc.Object/[]any
	// the masker can traverse (bodyMap's plain decode would make nested dicts map[string]any,
	// which the masker treats as an opaque scalar).
	var value any
	raw, _ := io.ReadAll(r.Body)
	if parsed, err := parseJSONValue(raw); err == nil {
		if obj, ok := parsed.(*jsonenc.Object); ok {
			value, _ = obj.Get("value")
		}
	}
	var masked any
	switch value.(type) {
	case *jsonenc.Object, []any:
		masked = s.maskValueForOutput(value)
	default:
		masked = s.maskStringForOutput(value)
	}
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("masked", masked))
}

// handleApiDataManagementPrune is defined in dm_prune.go.

// handleApiNotificationsCheck is defined in notif_check.go.

// githubBackfillMaxReleases mirrors app.py _github_backfill_max_releases: the
// enrichment.github_backfill_max_releases setting clamped to [1, 2000], default 300.
func (s *server) githubBackfillMaxReleases() int {
	n := 300
	if v, _ := s.appSetting("enrichment.github_backfill_max_releases"); v != "" {
		if p, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			n = p
		}
	}
	if n < 1 {
		n = 1
	}
	if n > 2000 {
		n = 2000
	}
	return n
}

// cveInventoryCount counts library-inventory rows across the three _collect_library_inventory
// tiers (release lockfiles, telemetry.sdk.* attrs, scope name+version). 0 on the fixture.
func (s *server) cveInventoryCount() int {
	return s.countRows("SELECT count() FROM sobs_release_artifacts FINAL WHERE ArtifactType='dependencies-lockfile' AND IsDeleted=0") +
		s.countRows("SELECT count() FROM otel_traces WHERE ResourceAttributes['telemetry.sdk.version'] != ''") +
		s.countRows("SELECT count() FROM otel_logs WHERE ResourceAttributes['telemetry.sdk.version'] != ''") +
		s.countRows("SELECT count() FROM otel_traces WHERE ScopeName != '' AND ScopeVersion != ''") +
		s.countRows("SELECT count() FROM otel_logs WHERE ScopeName != '' AND ScopeVersion != ''")
}

// POST /api/enrichment/cve/scan — app.py _run_cve_scan. CVE enrichment is enabled by default;
// with no ai.github_token the GitHub backfill is a no-op (0/0/cap) and persists its bookkeeping
// settings, and an empty library inventory short-circuits to the zero summary. Manifest-last.
// The scan body lives in runCveScan so the periodic CVE-scanner loop (background_tasks.go, real
// runtime only) can reuse it byte-for-byte.
func (s *server) handleApiEnrichmentCveScan(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.runCveScan())
}

// runCveScan is a port of app.py _run_cve_scan: backfill release-dependency lockfiles from GitHub
// (a no-op without ai.github_token, just persisting 0/0/cap bookkeeping), build the library
// inventory, query OSV.dev per library, persist findings + the scan timestamp, and return the
// summary object. Shared by the POST handler and the periodic CVE-scanner loop so both build the
// identical jsonify(...) body.
func (s *server) runCveScan() *jsonenc.Object {
	if v, ok := s.appSetting("enrichment.cve_enabled"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes":
		default:
			return jsonenc.NewObject().Set("ok", false).Set("reason", "disabled")
		}
	}
	attempted, inserted, maxRel := s.fetchReleaseDepsFromGithub()
	_ = s.setAppSetting("enrichment.cve_last_scan_github_backfill_attempted", strconv.Itoa(attempted))
	_ = s.setAppSetting("enrichment.cve_last_scan_github_backfill_inserted", strconv.Itoa(inserted))
	_ = s.setAppSetting("enrichment.cve_last_scan_github_backfill_cap", strconv.Itoa(maxRel))
	libraries := s.collectLibraryInventory()
	if len(libraries) == 0 {
		_ = s.setAppSetting("enrichment.cve_last_scan", nowISO())
		return jsonenc.NewObject().
			Set("github_backfill_attempted", attempted).
			Set("github_backfill_inserted", inserted).
			Set("github_backfill_max_releases", maxRel).
			Set("libraries_found", 0).
			Set("ok", true).
			Set("vulns_found", 0)
	}
	scanTS := nowISO()
	librariesFound, vulnsFound := s.runCveOSVScan(scanTS, libraries)
	return jsonenc.NewObject().
		Set("github_backfill_attempted", attempted).
		Set("github_backfill_inserted", inserted).
		Set("github_backfill_max_releases", maxRel).
		Set("libraries_found", librariesFound).
		Set("ok", true).
		Set("scanned_at", scanTS).
		Set("vulns_found", vulnsFound)
}

// POST /api/notifications/rules/auto-generate — app.py auto_generate_notification_rules in
// the default "preview" action (empty form): derive one candidate notification rule per active
// metric anomaly rule. On the fixture there are no existing notification rules (so nothing is
// skipped as already-covered) and no enabled channels (so channel_ids/names are empty).
func (s *server) handleApiNotificationsAutoGenerate(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	action := strings.ToLower(strings.TrimSpace(r.PostFormValue("action")))
	if action == "" {
		action = "preview"
	}
	metricRuleID := strings.TrimSpace(r.PostFormValue("metric_rule_id"))

	examined, skipped, candidates := s.notificationAutoCandidates(metricRuleID)

	if action == "create" {
		// Re-derive the covered set to guard against a race between preview and create.
		coveredNow := s.coveredSignalKeys()
		created := 0
		for _, c := range candidates {
			cand := c.(*jsonenc.Object)
			source, signal := objGetStr(cand, "source"), objGetStr(cand, "signal")
			key := source + "\x00" + signal
			if coveredNow[key] {
				skipped++
				continue
			}
			coveredNow[key] = true // prevent duplicates within this batch
			threshold, _ := cand.Get("threshold")
			conditions := []any{jsonenc.NewObject().
				Set("source", source).Set("signal", signal).
				Set("service", objGetStr(cand, "service")).
				Set("comparator", objGetStr(cand, "comparator")).
				Set("threshold", threshold).Set("window_minutes", 5)}
			chIDs := []string{}
			if v, ok := cand.Get("channel_ids"); ok {
				if list, ok := v.([]any); ok {
					for _, id := range list {
						chIDs = append(chIDs, id.(string))
					}
				}
			}
			_, _ = s.insertRowsNormalized("sobs_notification_rules", []map[string]any{{
				"Id": newUUIDv4(), "Name": objGetStr(cand, "name"), "Enabled": 1,
				"LogicOperator": "any", "ConditionsJson": string(jsonenc.Encode(conditions, dumpsDefault)),
				"ChannelIds": strings.Join(chIDs, ","), "Severity": objGetStr(cand, "severity"),
				"CooldownSeconds": 300, "LastFiredAt": "1970-01-01 00:00:00.000",
				"IsDeleted": 0, "Version": fixedVersionMillis(),
			}})
			created++
		}
		writeJSON(w, http.StatusOK, jsonenc.NewObject().
			Set("created", created).Set("examined", examined).Set("ok", true).Set("skipped", skipped))
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("candidates", candidates).Set("examined", examined).Set("ok", true).Set("skipped", skipped))
}

// notificationAutoCandidates mirrors _get_notification_auto_candidates: derive candidate
// notification rules from active anomaly (metric) rules, skipping any (source, signal) pair already
// covered by an existing notification rule's conditions, with all enabled channels pre-selected.
func (s *server) notificationAutoCandidates(metricRuleID string) (examined, skipped int, candidates []any) {
	query := "SELECT Id, Name, SignalSource, SignalName, ServiceName, Comparator, " +
		"WarningThreshold, CriticalThreshold FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0"
	var qArgs []any
	if metricRuleID != "" {
		query += " AND Id = ? LIMIT 1"
		qArgs = append(qArgs, metricRuleID)
	} else {
		query += " ORDER BY Name"
	}
	res, err := s.db.Execute(query, qArgs...)
	if err != nil {
		return 0, 0, []any{}
	}
	metricRows := rowMaps(res)
	covered := s.coveredSignalKeys()

	// All currently enabled channels are the default selection for every candidate.
	channelIDs, channelNames := []any{}, []any{}
	if chRes, err := s.db.Execute(
		"SELECT Id, Name FROM sobs_notification_channels FINAL WHERE IsDeleted = 0 AND Enabled = 1"); err == nil {
		for _, c := range rowMaps(chRes) {
			channelIDs = append(channelIDs, cStr(c, "Id"))
			channelNames = append(channelNames, cStr(c, "Name"))
		}
	}

	candidates = []any{}
	for _, m := range metricRows {
		source, signal := cStr(m, "SignalSource"), cStr(m, "SignalName")
		if covered[source+"\x00"+signal] {
			skipped++
			continue
		}
		crit, warn := cFloat(m, "CriticalThreshold"), cFloat(m, "WarningThreshold")
		threshold, severity := 0.0, "warning"
		if crit > 0 {
			threshold, severity = crit, "critical"
		} else if warn > 0 {
			threshold, severity = warn, "warning"
		}
		candidates = append(candidates, jsonenc.NewObject().
			Set("metric_rule_id", cStr(m, "Id")).
			Set("name", "Auto: "+cStr(m, "Name")).
			Set("source", source).
			Set("signal", signal).
			Set("service", cStr(m, "ServiceName")).
			Set("comparator", cStr(m, "Comparator")).
			Set("threshold", threshold).
			Set("severity", severity).
			Set("channel_ids", append([]any{}, channelIDs...)).
			Set("channel_names", append([]any{}, channelNames...)))
	}
	return len(metricRows), skipped, candidates
}

// coveredSignalKeys is the set of "source\x00signal" pairs already covered by an existing
// notification rule's conditions (mirrors the `covered` set in _get_notification_auto_candidates).
func (s *server) coveredSignalKeys() map[string]bool {
	covered := map[string]bool{}
	for _, rule := range s.loadNotificationRulesForCheck() {
		for _, c := range rule.conditions {
			if o, ok := c.(*jsonenc.Object); ok {
				covered[objGetStr(o, "source")+"\x00"+objGetStr(o, "signal")] = true
			}
		}
	}
	return covered
}

// objGetStr returns a string field of an ordered object (empty if absent or non-string).
func objGetStr(o *jsonenc.Object, key string) string {
	if v, ok := o.Get(key); ok {
		if str, ok := v.(string); ok {
			return str
		}
	}
	return ""
}

// truthy mirrors Python bool() for the JSON scalar types that reach a request body.
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case nil:
		return false
	default:
		return true
	}
}
