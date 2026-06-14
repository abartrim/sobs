package main

import (
	_ "embed"
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

// setupWizardDefaultJSON is app.py _build_setup_wizard_steps("dev","python","docker") — the
// default-context OTEL setup steps (purely a transcription of the Python step templates; no
// data/clock/random). Served verbatim for the default context.
//
//go:embed assets/setup_wizard_default.json
var setupWizardDefaultJSON []byte

// GET /api/setup-wizard/steps — tailored OTEL setup steps. The no-parameter request resolves
// to the dev/python/docker default, which is static.
func (s *server) handleApiSetupWizardSteps(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	get := func(k, def string) string {
		if v := strings.ToLower(strings.TrimSpace(q.Get(k))); v != "" {
			return v
		}
		return def
	}
	if get("env", "dev") == "dev" && get("language", "python") == "python" && get("deployment", "docker") == "docker" {
		writeRawJSON(w, http.StatusOK, setupWizardDefaultJSON)
		return
	}
	// Other env/language/deployment combinations (and their validation) are a follow-up.
	http.Error(w, "not implemented", http.StatusNotImplemented)
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
			Set("filters", parseJSONObject(cStr(m, "FiltersJson"))))
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
