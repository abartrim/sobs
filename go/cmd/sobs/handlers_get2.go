package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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

// GET /api/ai/helper/capabilities — app.py ai_helper_capabilities: the page-specific action
// manifest (_helper_action_manifest_for_page) plus the AI-provider capability flags derived from
// the configured model name (_model_supports_tools / _model_supports_thinking) and the stored
// ai.thinking_level setting (normalized). With no model configured (the fixture) both support
// flags are False, default_thinking_level is "off" and the per-page manifest is parsed from the
// page's template — unchanged on the empty-corpus path the golden captures.
func (s *server) handleApiAiHelperCapabilities(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(s.loadAISetting("ai.model", ""))
	thinkingLevel := normalizeThinkingLevel(s.loadAISetting("ai.thinking_level", "off"))
	page := strings.TrimSpace(r.URL.Query().Get("page"))
	if page == "" {
		page = "/logs"
	}
	manifest := s.helperActionManifestForPage(page)
	actionManifest := make([]any, 0, len(manifest))
	for _, a := range manifest {
		actionManifest = append(actionManifest, a)
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).
		Set("model", model).
		Set("supports_tools", modelSupportsTools(model)).
		Set("supports_thinking", modelSupportsThinking(model)).
		Set("default_thinking_level", thinkingLevel).
		Set("thinking_levels", []any{"off", "low", "medium", "high"}).
		Set("page", page).
		Set("action_manifest", actionManifest))
}

// handleApiAiHelperChatDetail is defined in ai_chat.go (full turn serialization).

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

// GET /api/enrichment/github/repo-health — app.py api_enrichment_github_repo_health: returns the
// version-scoped GitHub repo-health summary, 500 on the (ok:false) error branch. The summary build
// (and live per-repo issue scan) lives in collectGithubRepoHealthSummary (enrichment_loops.go) so
// the periodic repo-health loop can reuse it. Without a configured GitHub token (the base fixture)
// the per-repo scan is skipped, so scanned_repos/repos stay empty and the zero summary is returned
// byte-for-byte as before.
func (s *server) handleApiEnrichmentGithubRepoHealth(w http.ResponseWriter, r *http.Request) {
	summary := s.collectGithubRepoHealthSummary()
	if ok, _ := summary.Get("ok"); ok != true {
		writeJSON(w, http.StatusInternalServerError, summary)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// GET /api/reports/export — app.py api_export_reports: a downloadable JSON file (indent=2,
// INSERTION order) of all saved reports plus an exported_at wall-clock stamp. With an optional
// ?ids= comma-separated list, only matching report UUIDs are exported (filtered in-process from
// the full _get_reports list, preserving its PageType,Name order — matching app.py).
func (s *server) handleApiReportsExport(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute("SELECT Id, Name, Description, PageType, FiltersJson " +
		"FROM sobs_reports FINAL WHERE IsDeleted = 0 ORDER BY PageType, Name")
	if err != nil {
		s.dbError(w, err)
		return
	}
	// ids filter: {s.strip() for s in raw_ids.split(",") if s.strip()}; empty => all.
	var wanted map[string]bool
	if rawIDs := strings.TrimSpace(r.URL.Query().Get("ids")); rawIDs != "" {
		wanted = map[string]bool{}
		for _, part := range strings.Split(rawIDs, ",") {
			if id := strings.TrimSpace(part); id != "" {
				wanted[id] = true
			}
		}
	}
	reports := []any{}
	for _, m := range rowMaps(res) {
		id := cStr(m, "Id")
		if wanted != nil && !wanted[id] {
			continue
		}
		reports = append(reports, jsonenc.NewObject().
			Set("id", id).
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

// dumpsDefault mirrors json.dumps(ensure_ascii=False) with default (spaced) separators and
// insertion order — the encoding export_ai_training uses for each JSONL record / the json body.
var dumpsDefault = jsonenc.Options{ItemSep: ", ", KeySep: ": "}

// GET /api/ai/export — app.py export_ai_training: matching gen_ai spans as JSONL (or JSON) for
// training-set creation. Empty result -> empty body (the fixture's no-span case).
func (s *server) handleApiAiExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	conds := []string{aiSpanCondition}
	params := []any{}
	if v := strings.TrimSpace(q.Get("service")); v != "" {
		conds = append(conds, "ServiceName=?")
		params = append(params, v)
	}
	if v := strings.TrimSpace(q.Get("model")); v != "" {
		conds = append(conds, "SpanAttributes['gen_ai.request.model']=?")
		params = append(params, v)
	}
	if v := strings.TrimSpace(q.Get("operation")); v != "" {
		if strings.EqualFold(v, "chat") {
			conds = append(conds, "(SpanAttributes['gen_ai.operation.name']=? OR SpanAttributes['gen_ai.operation.name']='')")
		} else {
			conds = append(conds, "SpanAttributes['gen_ai.operation.name']=?")
		}
		params = append(params, v)
	}
	// _parse_time_window_args() + _time_window_conditions("Timestamp", ...) (app.py:19129,19155).
	fromTS, toTS, _ := parseTimeWindowArgs(r)
	timeConds, timeParams := timeWindowConditions("Timestamp", fromTS, toTS)
	conds = append(conds, timeConds...)
	params = append(params, timeParams...)
	maxRows := 1000
	if n, err := strconv.Atoi(strings.TrimSpace(q.Get("limit"))); err == nil {
		maxRows = n
	}
	if maxRows < 1 {
		maxRows = 1
	} else if maxRows > 5000 {
		maxRows = 5000
	}
	sql := "SELECT Timestamp, ServiceName, TraceId, Duration, " +
		"SpanAttributes['gen_ai.provider.name'] AS provider_name, SpanAttributes['gen_ai.system'] AS system, " +
		"SpanAttributes['gen_ai.request.model'] AS req_model, " +
		"SpanAttributes['gen_ai.input.messages'] AS input_messages, " +
		"SpanAttributes['gen_ai.output.messages'] AS output_messages, " +
		"SpanAttributes['sobs.gen_ai.prompt'] AS sobs_prompt, " +
		"SpanAttributes['sobs.gen_ai.response'] AS sobs_response, " +
		"SpanAttributes['gen_ai.usage.input_tokens'] AS tokens_in, " +
		"SpanAttributes['gen_ai.usage.output_tokens'] AS tokens_out " +
		"FROM otel_traces WHERE " + strings.Join(conds, " AND ") + " ORDER BY Timestamp DESC LIMIT ?"
	res, err := s.db.Execute(sql, append(params, maxRows)...)
	if err != nil {
		s.dbError(w, err)
		return
	}
	records := []any{}
	for _, m := range rowMaps(res) {
		provider := cStr(m, "provider_name")
		if provider == "" {
			provider = cStr(m, "system")
		}
		// prompt/response fallbacks: _extract_messages_text(raw) or attrs[sobs.gen_ai.prompt/response].
		inputRaw := cStr(m, "input_messages")
		outputRaw := cStr(m, "output_messages")
		prompt := extractMessagesText(inputRaw)
		if prompt == "" {
			prompt = cStr(m, "sobs_prompt")
		}
		response := extractMessagesText(outputRaw)
		if response == "" {
			response = cStr(m, "sobs_response")
		}
		messages := []any{}
		// On a genuine JSON-decode failure (not a successful non-list parse) fall back to the
		// extracted prompt/response text, mirroring app.py's try/except (json.JSONDecodeError).
		appendMsgs := func(raw, fallbackRole, fallbackText string) {
			if raw == "" {
				return
			}
			if parsed, perr := parseJSONValue([]byte(raw)); perr == nil {
				if list, ok := parsed.([]any); ok {
					messages = append(messages, list...)
				}
			} else if fallbackText != "" {
				messages = append(messages, jsonenc.NewObject().Set("role", fallbackRole).Set("content", fallbackText))
			}
		}
		appendMsgs(inputRaw, "user", prompt)
		appendMsgs(outputRaw, "assistant", response)
		records = append(records, jsonenc.NewObject().
			Set("messages", messages).
			Set("metadata", jsonenc.NewObject().
				Set("timestamp", cStr(m, "Timestamp")).
				Set("service", cStr(m, "ServiceName")).
				Set("provider", provider).
				Set("model", cStr(m, "req_model")).
				Set("tokens_in", int(cFloat(m, "tokens_in"))).
				Set("tokens_out", int(cFloat(m, "tokens_out"))).
				Set("duration_ms", roundHalfEven(cFloat(m, "Duration")/1_000_000, 1)).
				Set("trace_id", cStr(m, "TraceId"))))
	}
	var body, mime, filename string
	if strings.EqualFold(strings.TrimSpace(q.Get("format")), "json") {
		body, _ = jsonDumpsIndent2NoEsc(records) // json.dumps(records, ensure_ascii=False, indent=2)
		mime, filename = "application/json", "ai_training_data.json"
	} else {
		lines := make([]string, len(records))
		for i, rec := range records {
			lines[i] = string(jsonenc.Encode(rec, dumpsDefault))
		}
		body = strings.Join(lines, "\n")
		mime, filename = "application/x-ndjson", "ai_training_data.jsonl"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// GET /api/onboarding/inspect-repo — app.py api_onboarding_inspect_repo: resolve owner/repo from
// app_id (DB) or the repo param, then (with a token) inspect the GitHub repo for onboarding
// readiness. No configured token → the deterministic "no token" payload.
func (s *server) handleApiOnboardingInspectRepo(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	appID := strings.TrimSpace(q.Get("app_id"))
	repoParam := strings.TrimSpace(q.Get("repo"))
	repoURL := ""
	switch {
	case appID != "":
		res, err := s.db.Execute("SELECT RepoUrl FROM sobs_apps FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1", appID)
		if err != nil {
			s.dbError(w, err)
			return
		}
		if len(res.Rows) == 0 {
			s.writeMaskedJSON(w, http.StatusNotFound, jsonenc.NewObject().Set("ok", false).Set("error", "App not found"))
			return
		}
		repoURL = cStr(rowMaps(res)[0], "RepoUrl")
	case repoParam != "":
		repoURL = repoParam
	default:
		s.errorJSON(w, http.StatusBadRequest, "app_id or repo parameter required")
		return
	}
	owner, repo := parseGithubRepoOwnerName(repoURL)
	if owner == "" || repo == "" {
		s.writeMaskedJSON(w, http.StatusBadRequest, jsonenc.NewObject().
			Set("ok", false).Set("error", "Could not parse owner/repo from '"+repoURL+"'"))
		return
	}
	githubToken := s.repoScopedGithubToken(owner, repo)
	if githubToken == "" {
		githubToken = strings.TrimSpace(s.loadAISetting("ai.github_token", ""))
	}
	if githubToken == "" {
		s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
			Set("ok", true).Set("owner", owner).Set("repo", repo).
			Set("has_github_actions", false).Set("sobs_ci_found", false).
			Set("sobs_otel_found", false).Set("copilot_available", false).
			Set("workflow_files", []any{}).
			Set("error", "No GitHub token configured for this repository"))
		return
	}
	result := s.inspectRepoForOnboarding(githubToken, owner, repo)
	out := jsonenc.NewObject().Set("ok", true).Set("owner", owner).Set("repo", repo)
	for _, k := range result.Keys() {
		v, _ := result.Get(k)
		out.Set(k, v)
	}
	s.writeMaskedJSON(w, http.StatusOK, out)
}

var rumAssetIDRe = regexp.MustCompile(`^[a-f0-9]{32}$`)

// GET /v1/rum/assets/<asset_id> — app.py rum_asset_download: 400 when the id is not a
// 32-char lowercase hex string.
func (s *server) handleV1RumAssetByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		if paramMethodGuard(w, r) {
			return
		}
		http.NotFound(w, r)
		return
	}
	assetID := strings.TrimPrefix(r.URL.Path, "/v1/rum/assets/")
	if !rumAssetIDRe.MatchString(assetID) {
		errorOnly(w, http.StatusBadRequest, "invalid asset id")
		return
	}
	// app.py rum_asset_download: read {id}.meta.json under DATA_DIR/rum_assets; 404 when absent
	// (the fixture seeds no assets, so every valid id resolves here), else serve the stored file.
	dir := filepath.Join(s.cfg.DataDir, "rum_assets")
	metaRaw, err := os.ReadFile(filepath.Join(dir, assetID+".meta.json"))
	if err != nil {
		errorOnly(w, http.StatusNotFound, "not found")
		return
	}
	var meta map[string]any
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		errorOnly(w, http.StatusInternalServerError, "asset metadata unavailable")
		return
	}
	storageName, _ := meta["storage_name"].(string)
	if storageName == "" || strings.ContainsAny(storageName, `/\`) {
		errorOnly(w, http.StatusInternalServerError, "invalid asset metadata")
		return
	}
	filePath := filepath.Join(dir, storageName)
	data, err := os.ReadFile(filePath)
	if err != nil {
		errorOnly(w, http.StatusNotFound, "not found")
		return
	}
	contentType, _ := meta["content_type"].(string)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	// app.py serves via Quart send_from_directory(mimetype=…, as_attachment=False). Quart's
	// send_file emits Content-Type (passed through Werkzeug get_content_type, which appends
	// "; charset=utf-8" for text mimetypes), Content-Length, Cache-Control:
	// "public, max-age=<send_file_max_age_default>" (12h), plus Last-Modified / Expires / a
	// filesystem ETag. It does NOT set Accept-Ranges or honor Range requests in this config.
	// http.ServeContent (the prior port) diverged on every one of these: it emits Accept-Ranges,
	// omits Cache-Control, and does not charset the Content-Type. Mirror Quart's header set
	// directly (the same approach serveRumFile uses for the static rum.* downloads). The
	// non-reproducible Last-Modified / Expires / mtime-format ETag are intentionally NOT emitted —
	// normalize.py drops them from the golden anyway, so omitting them keeps the compared header
	// multiset exact. See POPULATED_RENDER_FINDINGS.md R15.
	w.Header().Set("Content-Type", werkzeugContentType(contentType))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(staticMaxAge))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// werkzeugContentType mirrors werkzeug.utils.get_content_type(mimetype, "utf-8"): a charset is
// appended for text mimetypes (text/*, the known-textual application types, or any +xml type).
var werkzeugCharsetMimetypes = map[string]bool{
	"application/ecmascript":                 true,
	"application/javascript":                 true,
	"application/sql":                        true,
	"application/xml":                        true,
	"application/xml-dtd":                    true,
	"application/xml-external-parsed-entity": true,
}

func werkzeugContentType(mimetype string) string {
	if strings.HasPrefix(mimetype, "text/") || werkzeugCharsetMimetypes[mimetype] || strings.HasSuffix(mimetype, "+xml") {
		return mimetype + "; charset=utf-8"
	}
	return mimetype
}
