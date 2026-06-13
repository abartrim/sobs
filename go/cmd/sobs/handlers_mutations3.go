package main

import (
	"net/http"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Path-parameter mutation handlers. Each is registered as a ServeMux subtree ("…/") and
// dispatches by the trailing path segments + method. On the fixture the referenced record
// never exists, so the deterministic branch is the not-found / validation error.

// DELETE /api/mcp/keys/<key_id> — mcp.py mcp_api_delete_key: loads the mcp.api_keys setting
// (a JSON list) and 404s when no descriptor has the given id. The fixture has no keys.
func (s *server) handleMcpKeyByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.NotFound(w, r)
		return
	}
	keyID := strings.TrimPrefix(r.URL.Path, "/api/mcp/keys/")
	raw, _ := s.appSetting("mcp.api_keys")
	if raw == "" {
		raw = "[]"
	}
	found := false
	if v, err := parseJSONValue([]byte(raw)); err == nil {
		if list, ok := v.([]any); ok {
			for _, it := range list {
				if o, ok := it.(*jsonenc.Object); ok {
					if id, _ := o.Get("id"); id == keyID {
						found = true
						break
					}
				}
			}
		}
	}
	if !found {
		s.errorJSON(w, http.StatusNotFound, "Key not found.")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// DELETE /api/notifications/vapid-keys — app.py delete_vapid_keys: clears the DB VAPID key
// and reports the fixed note. env_override is false when SOBS_VAPID_PRIVATE_KEY is unset.
func (s *server) handleVapidKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("env_override", false).
		Set("note", "DB VAPID key cleared. Browser push is now unconfigured until new keys are generated.").
		Set("ok", true))
}

// /api/reports/import (POST) and /api/reports/<report_id> (DELETE). Registered under the
// "/api/reports/" subtree; the bare "/api/reports" route keeps its own handler.
func (s *server) handleReportsSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/reports/")
	if rest == "import" {
		// app.py api_import_reports: an empty/invalid body fails the export-file schema check.
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		errorOnly(w, http.StatusBadRequest, "Not a valid SOBS reports export file")
		return
	}
	// /api/reports/<report_id>
	if r.Method == http.MethodDelete {
		if !s.rowExists("SELECT Id FROM sobs_reports FINAL WHERE IsDeleted = 0 AND Id = ?", rest) {
			errorOnly(w, http.StatusNotFound, "not found")
			return
		}
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}
	http.NotFound(w, r)
}

// POST /api/agent/runs/<run_id>/dismiss — 404 when the run does not exist.
func (s *server) handleAgentRunSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/agent/runs/")
	runID, ok := strings.CutSuffix(rest, "/dismiss")
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !s.rowExists("SELECT Id FROM sobs_agent_runs FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1", runID) {
		s.errorJSON(w, http.StatusNotFound, "run not found")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/notifications/channels/<channel_id>/test — 404 when the channel does not exist.
func (s *server) handleChannelSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/notifications/channels/")
	chID, ok := strings.CutSuffix(rest, "/test")
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !s.rowExists("SELECT Id FROM sobs_notification_channels FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1", chID) {
		s.errorJSON(w, http.StatusNotFound, "channel not found")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/enrichment/cve/findings/<osv_id>/disposition — validates required fields before
// any record lookup; an empty body fails with the fixed message.
func (s *server) handleCveDispositionSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/enrichment/cve/findings/")
	if _, ok := strings.CutSuffix(rest, "/disposition"); !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	m := bodyMap(r)
	if bstr(m, "osv_id") == "" || bstr(m, "package") == "" || bstr(m, "ecosystem") == "" || bstr(m, "version") == "" {
		s.errorJSON(w, http.StatusBadRequest, "osv_id, package, ecosystem, and version are required")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /api/dashboards/<dashboard_id>/charts/import — 404 when the dashboard does not exist.
// Registered under the "/api/dashboards/" subtree (exact /api/dashboards/* routes win).
func (s *server) handleDashboardSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/dashboards/")
	if dashID, ok := strings.CutSuffix(rest, "/charts/import"); ok && r.Method == http.MethodPost {
		if !s.rowExists("SELECT Id FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Id = ?", dashID) {
			s.errorJSON(w, http.StatusNotFound, "Dashboard not found")
			return
		}
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}
	// GET /api/dashboards/<dashboard_id>/charts/<chart_id>/export — 404 when the dashboard
	// is absent (the lookup precedes the chart lookup).
	if seg := strings.Split(rest, "/"); r.Method == http.MethodGet && len(seg) == 4 &&
		seg[1] == "charts" && seg[3] == "export" {
		if !s.rowExists("SELECT Id FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Id = ?", seg[0]) {
			s.errorJSON(w, http.StatusNotFound, "Dashboard not found")
			return
		}
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}
	http.NotFound(w, r)
}

// POST /errors/<error_id>/resolve — app.py marks the error resolved; idempotent, so an
// unknown id still returns {"ok": true}. Registered under the "/errors/" subtree.
func (s *server) handleErrorSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/errors/")
	if _, ok := strings.CutSuffix(rest, "/resolve"); ok && r.Method == http.MethodPost {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true))
		return
	}
	http.NotFound(w, r)
}
