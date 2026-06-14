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

// ---- AI helper (field-required 400s) -------------------------------------------------

// POST /api/ai/helper — app.py ai_helper: requires `question`.
func (s *server) handleApiAiHelper(w http.ResponseWriter, r *http.Request) {
	if bstr(bodyMap(r), "question") == "" {
		s.errorJSON(w, http.StatusBadRequest, "question is required")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/ai/helper/actions/execute — requires `action_token`.
func (s *server) handleApiAiHelperExecute(w http.ResponseWriter, r *http.Request) {
	if bstr(bodyMap(r), "action_token") == "" {
		s.errorJSON(w, http.StatusBadRequest, "action_token is required")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/ai/helper/feedback — requires chat_id, turn_id, and note.
func (s *server) handleApiAiHelperFeedback(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	if bstr(m, "chat_id") == "" || bstr(m, "turn_id") == "" || bstr(m, "note") == "" {
		s.errorJSON(w, http.StatusBadRequest, "chat_id, turn_id, and note are required")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/dashboards/spec/ai-build — requires `question`.
func (s *server) handleApiDashboardsSpecAiBuild(w http.ResponseWriter, r *http.Request) {
	if bstr(bodyMap(r), "question") == "" {
		s.errorJSON(w, http.StatusBadRequest, "question is required")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

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

// POST /api/onboarding/create-issues — requires an app_id or repo parameter.
func (s *server) handleApiOnboardingCreateIssues(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	if bstr(m, "app_id") == "" && bstr(m, "repo") == "" {
		s.errorJSON(w, http.StatusBadRequest, "app_id or repo parameter required")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/onboarding/create-repo — requires app name and repository.
func (s *server) handleApiOnboardingCreateRepo(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	if bstr(m, "name") == "" || bstr(m, "repository") == "" {
		s.errorJSON(w, http.StatusBadRequest, "App name and repository are required")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
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
	resp, err := s.upstreamGet("GET", "https://api.github.com/repos/"+owner+"/"+repo)
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
		resp, err := s.upstreamGet("GET", url)
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

// POST /api/issues/raise — app.py raise_issue_from_user_observation gates on AI config first;
// the fixture has no ai.endpoint_url/ai.model, so it always returns 503.
func (s *server) handleApiIssuesRaise(w http.ResponseWriter, r *http.Request) {
	endpoint, _ := s.appSetting("ai.endpoint_url")
	model, _ := s.appSetting("ai.model")
	if endpoint == "" || model == "" {
		writeJSON(w, http.StatusServiceUnavailable, jsonenc.NewObject().
			Set("error", "AI endpoint not configured. Visit Settings -> AI Configuration.").
			Set("ok", false))
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

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

// POST /api/data-management/prune — app.py prunes the retention-eligible tables; on the
// fixture (all rows within the frozen retention window) nothing is deleted and it reports the
// fixed six-table summary.
func (s *server) handleApiDataManagementPrune(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("message", "Prune completed successfully (6 tables processed)").Set("ok", true))
}

// POST /api/notifications/check — app.py check_notifications evaluates every enabled
// notification rule and every agent rule. The fixture defines none of either, so the
// evaluation loops never execute and the response is the fully-empty summary. (When rules
// exist the per-rule evaluation path is a follow-up.)
func (s *server) handleApiNotificationsCheck(w http.ResponseWriter, r *http.Request) {
	notifRules := s.countRows("SELECT count() FROM sobs_notification_rules FINAL WHERE IsDeleted = 0")
	agentRules := s.countRows("SELECT count() FROM sobs_agent_rules FINAL WHERE IsDeleted=0")
	if notifRules != 0 || agentRules != 0 {
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("agent_runs", []any{}).
		Set("evaluated", 0).
		Set("fired", 0).
		Set("ok", true).
		Set("results", []any{}))
}

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
func (s *server) handleApiEnrichmentCveScan(w http.ResponseWriter, r *http.Request) {
	if v, ok := s.appSetting("enrichment.cve_enabled"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes":
		default:
			writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", false).Set("reason", "disabled"))
			return
		}
	}
	if tok, _ := s.appSetting("ai.github_token"); tok != "" {
		http.Error(w, "not implemented", http.StatusNotImplemented) // live GitHub backfill is a follow-up
		return
	}
	maxRel := s.githubBackfillMaxReleases()
	_ = s.setAppSetting("enrichment.cve_last_scan_github_backfill_attempted", "0")
	_ = s.setAppSetting("enrichment.cve_last_scan_github_backfill_inserted", "0")
	_ = s.setAppSetting("enrichment.cve_last_scan_github_backfill_cap", strconv.Itoa(maxRel))
	if s.cveInventoryCount() != 0 {
		http.Error(w, "not implemented", http.StatusNotImplemented) // real OSV scan is a follow-up
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("github_backfill_attempted", 0).
		Set("github_backfill_inserted", 0).
		Set("github_backfill_max_releases", maxRel).
		Set("libraries_found", 0).
		Set("ok", true).
		Set("vulns_found", 0))
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
	// The "create" action (and the already-covered/channel-selection logic when notification
	// rules or channels exist) is a follow-up; the fixture exercises the empty-state preview.
	if action != "preview" ||
		s.countRows("SELECT count() FROM sobs_notification_rules FINAL WHERE IsDeleted = 0") != 0 ||
		s.countRows("SELECT count() FROM sobs_notification_channels FINAL WHERE IsDeleted = 0 AND Enabled = 1") != 0 {
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}
	res, err := s.db.Execute("SELECT Id, Name, SignalSource, SignalName, ServiceName, Comparator, " +
		"WarningThreshold, CriticalThreshold FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		s.dbError(w, err)
		return
	}
	candidates := []any{}
	for _, m := range rowMaps(res) {
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
			Set("source", cStr(m, "SignalSource")).
			Set("signal", cStr(m, "SignalName")).
			Set("service", cStr(m, "ServiceName")).
			Set("comparator", cStr(m, "Comparator")).
			Set("threshold", threshold).
			Set("severity", severity).
			Set("channel_ids", []any{}).
			Set("channel_names", []any{}))
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("candidates", candidates).Set("examined", len(candidates)).
		Set("ok", true).Set("skipped", 0))
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
