package main

import (
	"net/http"

	"github.com/sobs/sobs/internal/jsonenc"
)

// writeJSON emits a JSON body byte-identical to Quart's jsonify and sets the same
// Content-Type. The security headers are applied by the headerCapture middleware.
func writeJSON(w http.ResponseWriter, status int, obj any) {
	body := jsonenc.Encode(obj, jsonenc.QuartJSONify)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleHealth mirrors app.py: return jsonify({"status": "ok", "version": "1.0.0"}).
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	obj := jsonenc.NewObject().Set("status", "ok").Set("version", "1.0.0")
	writeJSON(w, http.StatusOK, obj)
}
