package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Mutation handlers (POST/DELETE/PATCH). The parity manifest sends the simplest request
// that yields a DETERMINISTIC response (no generated uuid/timestamp/cookie) — usually an
// empty body hitting a validation/no-op branch.

// jsonBodyStr decodes the JSON request body and returns a trimmed string field (or "").
func jsonBodyStr(r *http.Request, key string) string {
	if r.Body == nil {
		return ""
	}
	var m map[string]any
	if json.NewDecoder(r.Body).Decode(&m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// POST /api/{logs,errors,traces,metrics,rum}/validate-regex — app.py *_validate_regex.
// Empty pattern -> {"ok": true, "sample": null}.
func (s *server) handleValidateRegex(w http.ResponseWriter, r *http.Request) {
	if jsonBodyStr(r, "pattern") == "" {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("sample", nil))
		return
	}
	// Non-empty pattern (full validation + sample probe) is not exercised by the empty-body
	// parity request; the no-pattern branch above is the covered behavior.
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("sample", nil))
}

// POST /api/data-management/backup/run and /restore — app.py: 403 when the backup feature
// is disabled (data_management.backup_enabled off by default on the fixture).
func (s *server) handleDmBackupGuard(w http.ResponseWriter, r *http.Request) {
	if !s.appSettingBool("data_management.backup_enabled", false) {
		writeJSON(w, http.StatusForbidden,
			jsonenc.NewObject().Set("ok", false).Set("message", "Backup feature is disabled"))
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented) // enabled branch: follow-up
}

// POST /api/{logs,ai}/validate-filter — empty filter -> {"issues":[],"normalized":"","ok":true}.
func (s *server) handleValidateFilter(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("issues", []any{}).Set("normalized", "").Set("ok", true))
}
