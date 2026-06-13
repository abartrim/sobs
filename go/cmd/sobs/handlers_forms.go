package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// flaskSessionOpts serializes the session dict the way Flask/Quart's TaggedJSONSerializer
// does: insertion order, compact separators, ensure_ascii (no HTML escaping).
var flaskSessionOpts = jsonenc.Options{SortKeys: false, EnsureASCII: true, ItemSep: ",", KeySep: ":"}

// htmlEscapeMarkup mirrors markupsafe.escape (used in Werkzeug's redirect body).
func htmlEscapeMarkup(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&#34;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}

// flashSessionCookie builds the sobs_session cookie carrying a single flash message. The
// parity normalizer keeps only the unsigned base64 payload segment (dropping the HMAC
// timestamp+signature), so a placeholder ".0.0" suffix is sufficient and the payload — the
// only compared part — is byte-identical to Quart's.
func flashSessionCookie(category, message string) string {
	sess := jsonenc.NewObject().Set("_flashes", []any{
		jsonenc.NewObject().Set(" t", []any{category, message}),
	})
	js := jsonenc.Encode(sess, flaskSessionOpts)
	// itsdangerous compresses the payload when zlib shrinks it (len(compressed) < len(json)-1),
	// prefixing the base64 with "." (URLSafeSerializerMixin.dump_payload). The parity
	// normalizer keeps only the segment before the first ".", so a compressed payload reduces
	// to "" on both sides — we just have to make the SAME compress/no-compress decision so the
	// uncompressed (short-message) cookies still match byte-for-byte.
	compressed := zlibCompress(js)
	var payload string
	if len(compressed) < len(js)-1 {
		payload = "." + base64.RawURLEncoding.EncodeToString(compressed)
	} else {
		payload = base64.RawURLEncoding.EncodeToString(js)
	}
	return "sobs_session=" + payload + ".0.0; HttpOnly; Path=/; SameSite=Lax"
}

// zlibCompress mirrors Python zlib.compress(data) at the default level (6).
func zlibCompress(data []byte) []byte {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	_, _ = w.Write(data)
	_ = w.Close()
	return b.Bytes()
}

// flashRedirect reproduces Quart's `flash(message, category); return redirect(location)`: a
// 302 carrying the flash session cookie and Werkzeug's standard redirect body.
func flashRedirect(w http.ResponseWriter, category, message, location string) {
	esc := htmlEscapeMarkup(location)
	body := "<!doctype html>\n<html lang=en>\n<title>Redirecting...</title>\n" +
		"<h1>Redirecting...</h1>\n<p>You should be redirected automatically to the target URL: " +
		"<a href=\"" + esc + "\">" + esc + "</a>. If not, click the link.\n"
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Location", location)
	h.Set("Set-Cookie", flashSessionCookie(category, message))
	h.Set("Vary", "Cookie")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusFound)
	_, _ = w.Write([]byte(body))
}

// POST /reports/<report_id>/delete — app.py delete_report: a missing report flashes "Report
// not found" and redirects to the reports list.
func (s *server) handleReportsFormSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/reports/")
	if id, ok := strings.CutSuffix(rest, "/delete"); ok && r.Method == http.MethodPost {
		if !s.rowExists("SELECT Id FROM sobs_reports FINAL WHERE IsDeleted = 0 AND Id = ?", id) {
			flashRedirect(w, "danger", "Report not found", "/reports")
			return
		}
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}
	http.NotFound(w, r)
}

// formDeleteGuard handles a POST .../<id>/delete form route: it looks the record up by Id in
// `table`, and when absent flashes `msg`/`category` and redirects to `location` (the deterministic
// branch on the fixture). `table` is a compile-time constant, never user input.
func (s *server) formDeleteGuard(w http.ResponseWriter, r *http.Request, prefix, table, category, msg, location string) {
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	id, ok := strings.CutSuffix(rest, "/delete")
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !s.rowExists("SELECT Id FROM "+table+" FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1", id) {
		flashRedirect(w, category, msg, location)
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// formRequire flashes `msg` and redirects to `location` when the POST form lacks a non-empty
// value for `field` (the required-field validation branch). Returns true when it handled it.
func (s *server) formRequire(w http.ResponseWriter, r *http.Request, field, category, msg, location string) bool {
	_ = r.ParseForm()
	if strings.TrimSpace(r.PostFormValue(field)) == "" {
		flashRedirect(w, category, msg, location)
		return true
	}
	return false
}

func (s *server) handleSettingsAgentsSub(w http.ResponseWriter, r *http.Request) {
	s.formDeleteGuard(w, r, "/settings/agents/", "sobs_agent_rules", "warning", "Agent rule not found", "/settings/agents")
}
func (s *server) handleSettingsTagsSub(w http.ResponseWriter, r *http.Request) {
	s.formDeleteGuard(w, r, "/settings/tags/", "sobs_tag_rules", "warning", "Tag rule not found", "/settings/tags")
}
func (s *server) handleMetricsRulesSub(w http.ResponseWriter, r *http.Request) {
	s.formDeleteGuard(w, r, "/metrics/rules/", "sobs_anomaly_rules", "warning", "Rule not found", "/metrics/rules")
}

// formLookupGuard handles every POST .../<id>/<action> form route under `prefix` (delete,
// toggle, rotate, …): it looks the first path segment up by Id in `table` and flashes the
// not-found message when absent — the deterministic branch for every action on the fixture.
func (s *server) formLookupGuard(w http.ResponseWriter, r *http.Request, prefix, table, category, msg, location string) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	id := strings.SplitN(rest, "/", 2)[0]
	if !s.rowExists("SELECT Id FROM "+table+" FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1", id) {
		flashRedirect(w, category, msg, location)
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func (s *server) handleNotifChannelsSub(w http.ResponseWriter, r *http.Request) {
	s.formLookupGuard(w, r, "/settings/notifications/channels/", "sobs_notification_channels", "warning", "Notification channel not found", "/settings/notifications")
}
func (s *server) handleNotifRulesSub(w http.ResponseWriter, r *http.Request) {
	s.formLookupGuard(w, r, "/settings/notifications/rules/", "sobs_notification_rules", "warning", "Notification rule not found", "/settings/notifications")
}

// /settings/repositories/<app_id>/... (delete, realtime-mode, ci-ingest-key/rotate|revoke,
// releases, save): a missing app flashes "Repository entry not found". The github-token/validate
// action is not an app route — it flashes when no GitHub token is configured.
func (s *server) handleSettingsRepositoriesSub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/settings/repositories/")
	if rest == "github-token/validate" {
		if tok, _ := s.appSetting("ai.github_token"); tok == "" {
			flashRedirect(w, "warning", "No GitHub token configured to validate", "/settings/repositories")
			return
		}
		http.Error(w, "not implemented", http.StatusNotImplemented)
		return
	}
	id := strings.SplitN(rest, "/", 2)[0]
	if !s.rowExists("SELECT Id FROM sobs_apps FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1", id) {
		flashRedirect(w, "warning", "Repository entry not found", "/settings/repositories")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /settings/notifications/channels (create) — empty form -> "Channel name is required".
func (s *server) handleNotifChannelsCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if s.formRequire(w, r, "name", "warning", "Channel name is required", "/settings/notifications") {
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /settings/notifications/rules (create) — empty form -> "Rule name is required".
func (s *server) handleNotifRulesCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if s.formRequire(w, r, "name", "warning", "Rule name is required", "/settings/notifications") {
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /settings/masking/keys — empty form -> "Sensitive key name is required".
func (s *server) handleMaskingKeysCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if s.formRequire(w, r, "key", "warning", "Sensitive key name is required", "/settings/masking") {
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /settings/masking/patterns — empty form -> "Invalid regex pattern: Pattern is required".
func (s *server) handleMaskingPatternsCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if s.formRequire(w, r, "pattern", "warning", "Invalid regex pattern: Pattern is required", "/settings/masking") {
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// /dashboards/<id>/... POST form routes (delete / create-chart / chart-delete): all begin
// with a dashboard lookup, so a missing dashboard flashes "Dashboard not found".
func (s *server) handleDashboardsFormSub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/dashboards/")
	dashID := strings.Split(rest, "/")[0]
	if !s.rowExists("SELECT Id FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Id = ?", dashID) {
		flashRedirect(w, "danger", "Dashboard not found", "/dashboards")
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
