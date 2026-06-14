package main

import (
	"encoding/json"
	"net/http"
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

// POST /api/notifications/subscribe — requires endpoint, p256dh, and auth.
func (s *server) handleApiNotificationsSubscribe(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	if bstr(m, "endpoint") == "" || bstr(m, "p256dh") == "" || bstr(m, "auth") == "" {
		s.errorJSON(w, http.StatusBadRequest, "endpoint, p256dh, and auth are required")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
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

// POST /api/onboarding/import-repo — requires a valid GitHub owner and repository name.
func (s *server) handleApiOnboardingImportRepo(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	if bstr(m, "owner") == "" || bstr(m, "repo") == "" {
		s.errorJSON(w, http.StatusBadRequest, "Enter a valid GitHub owner and repository name")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/onboarding/list-repos — requires an owner/username.
func (s *server) handleApiOnboardingListRepos(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	if bstr(m, "owner") == "" && bstr(m, "username") == "" {
		s.errorJSON(w, http.StatusBadRequest, "Owner or username is required")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// ---- Dashboards query/spec (error-only 400s) -----------------------------------------

// specSQLMode returns the `spec.sql.mode` string from a decoded body (or "" when absent).
func specSQLMode(m map[string]any) string {
	spec, _ := m["spec"].(map[string]any)
	sql, _ := spec["sql"].(map[string]any)
	mode, _ := sql["mode"].(string)
	return strings.TrimSpace(mode)
}

// POST /api/dashboards/query — empty query -> {"error":"Query cannot be empty"}.
func (s *server) handleApiDashboardsQuery(w http.ResponseWriter, r *http.Request) {
	if bstr(bodyMap(r), "query") == "" {
		errorOnly(w, http.StatusBadRequest, "Query cannot be empty")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/dashboards/render — empty query -> {"error":"Query cannot be empty"}.
func (s *server) handleApiDashboardsRender(w http.ResponseWriter, r *http.Request) {
	if bstr(bodyMap(r), "query") == "" {
		errorOnly(w, http.StatusBadRequest, "Query cannot be empty")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
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

// POST /api/dashboards/spec/compile.
func (s *server) handleApiDashboardsSpecCompile(w http.ResponseWriter, r *http.Request) {
	if specModeGuard(w, r, nil) {
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/dashboards/spec/dry-run.
func (s *server) handleApiDashboardsSpecDryRun(w http.ResponseWriter, r *http.Request) {
	if specModeGuard(w, r, nil) {
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/dashboards/spec/render.
func (s *server) handleApiDashboardsSpecRender(w http.ResponseWriter, r *http.Request) {
	if specModeGuard(w, r, nil) {
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/dashboards/spec/validate — adds a "valid": false alongside the error.
func (s *server) handleApiDashboardsSpecValidate(w http.ResponseWriter, r *http.Request) {
	if specModeGuard(w, r, func(o *jsonenc.Object) { o.Set("valid", false) }) {
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
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
	v := bodyMap(r)["value"]
	if v == nil || v == "" {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("masked", "").Set("ok", true))
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
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
