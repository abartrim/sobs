package main

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/sobs/sobs/internal/jsonenc"
)

// otlpRecordCount counts the data points in an OTLP-JSON ingest body (resource[].scope[].rec[]).
// ok is false when the body is not JSON (e.g. an OTLP-protobuf upload), which the empty-body
// parity request never is. An empty/recordless body yields (0, true).
func otlpRecordCount(body []byte, resKey, scopeKey, recKey string) (int, bool) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return 0, false
	}
	n := 0
	res, _ := m[resKey].([]any)
	for _, r := range res {
		ro, _ := r.(map[string]any)
		scopes, _ := ro[scopeKey].([]any)
		for _, sc := range scopes {
			so, _ := sc.(map[string]any)
			recs, _ := so[recKey].([]any)
			n += len(recs)
		}
	}
	return n, true
}

// v1IngestOTLP handles a POST /v1/{logs,metrics,traces}: a recordless batch is accepted with a
// zero count and no insert (the deterministic empty-body path). A non-empty batch — the real
// ingest+insert — is a follow-up.
func (s *server) v1IngestOTLP(w http.ResponseWriter, r *http.Request, resKey, scopeKey, recKey string) {
	body, _ := io.ReadAll(r.Body)
	if n, ok := otlpRecordCount(body, resKey, scopeKey, recKey); ok && n == 0 {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("accepted", 0))
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /v1/rum/client-token — app.py issue_rum_client_token: RUM client auth is disabled on
// the fixture (RUM_CLIENT_AUTH_MODE unset), so it reports the disabled state.
func (s *server) handleV1RumClientToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("enabled", false).Set("error", "RUM client auth is disabled").Set("token", ""))
}
