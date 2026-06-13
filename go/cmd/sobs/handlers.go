package main

import (
	"net/http"
	"strconv"

	"github.com/sobs/sobs/internal/jsonenc"
)

// writeJSON emits a JSON body byte-identical to Quart's jsonify and sets the same
// Content-Type. Content-Length is set explicitly: Quart always emits it, but net/http
// drops it (switches to chunked) once a body exceeds its internal buffer (~2KB), which
// diverged on large JSON catalogs. The security headers are applied by headerCapture.
func writeJSON(w http.ResponseWriter, status int, obj any) {
	body := jsonenc.Encode(obj, jsonenc.QuartJSONify)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// handleHealth mirrors app.py: return jsonify({"status": "ok", "version": "1.0.0"}).
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	obj := jsonenc.NewObject().Set("status", "ok").Set("version", "1.0.0")
	writeJSON(w, http.StatusOK, obj)
}

// handleHealthDB mirrors app.py health_db(): SELECT 1; on success return the ok payload,
// else the 503 degraded payload. latency_ms is frozen to 0.0 (perf_counter frozen in
// parity), write_queue_depth is 0 (no async write queue yet).
func (s *server) handleHealthDB(w http.ResponseWriter, r *http.Request) {
	ok := s.db != nil
	if ok {
		if _, err := s.db.Execute("SELECT 1"); err != nil {
			ok = false
		}
	}
	if !ok {
		obj := jsonenc.NewObject().
			Set("status", "degraded").
			Set("db", "error").
			Set("error", "database unavailable").
			Set("write_queue_depth", 0).
			Set("version", "1.0.0")
		writeJSON(w, http.StatusServiceUnavailable, obj)
		return
	}
	obj := jsonenc.NewObject().
		Set("status", "ok").
		Set("db", "ok").
		Set("latency_ms", 0.0).
		Set("write_queue_depth", 0).
		Set("version", "1.0.0")
	writeJSON(w, http.StatusOK, obj)
}
