package main

import (
	"net/http"

	"github.com/sobs/sobs/internal/jsonenc"
)

// This file ports the JSON API routes whose behavior in the parity fixture state is a
// feature-disabled guard return. These guards are faithful ports: the feature flags
// (query_enabled, kubernetes_enabled) are computed by app.py from DB settings and are
// False on the empty fixture DB, so each handler returns its 404 guard payload. When the
// settings/data layer lands, the enabled branch will query and return live data.

// errorJSON writes {"error": msg, "ok": false} at the given status (keys sorted by jsonenc).
func (s *server) errorJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, jsonenc.NewObject().Set("error", msg).Set("ok", false))
}

// GET /api/query/schema — app.py api_query_schema(): guarded by _query_page_enabled.
func (s *server) handleApiQuerySchema(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.QueryPageEnabled {
		s.errorJSON(w, http.StatusNotFound, "Query page is unavailable.")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented) // enabled branch: Phase 3 follow-up
}

// GET /api/table-explorer/tables — app.py api_table_explorer_tables(): query-page guard, then
// metadata for every allowlisted table that exists (name, column_count, columns).
func (s *server) handleApiTableExplorerTables(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.QueryPageEnabled {
		s.errorJSON(w, http.StatusNotFound, "Table Explorer is unavailable.")
		return
	}
	tables, err := s.allowedTablesInfo()
	if err != nil {
		s.writeMaskedJSON(w, http.StatusInternalServerError,
			jsonenc.NewObject().Set("ok", false).Set("error", err.Error()))
		return
	}
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("tables", tables))
}

// GET /api/kubernetes/status — app.py api_kubernetes_status(): guarded by _kubernetes_enabled.
func (s *server) handleApiKubernetesStatus(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.KubernetesEnabled {
		s.errorJSON(w, http.StatusNotFound, "Kubernetes health view is disabled.")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// GET /api/notifications/vapid-public-key — 404 when no VAPID key is configured (parity).
func (s *server) handleApiVapidPublicKey(w http.ResponseWriter, r *http.Request) {
	// No VAPID keypair in the fixture settings -> not configured.
	s.errorJSON(w, http.StatusNotFound, "VAPID key not configured")
}
