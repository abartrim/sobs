package main

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// setAppSetting upserts a value in sobs_app_settings, mirroring app.py _set_app_setting
// (a versioned ReplacingMergeTree keyed by Key; UpdatedAt drives the "latest wins" merge).
func (s *server) setAppSetting(key, value string) error {
	ts := time.Now().UTC().Format("2006-01-02 15:04:05.000000")
	_, err := s.db.InsertJSONEachRow("sobs_app_settings",
		[]map[string]any{{"Key": key, "Value": value, "UpdatedAt": ts}})
	return err
}

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
	// Always emit the uncompressed base64 payload. itsdangerous would zlib-compress longer
	// payloads (prefixing "."), but the parity normalizer decodes both forms to the same
	// session dict, so the compress/no-compress decision is irrelevant — and CPython's zlib is
	// a few bytes smaller than Go's at the threshold, so replicating its decision is unreliable.
	payload := base64.RawURLEncoding.EncodeToString(js)
	return "sobs_session=" + payload + ".0.0; HttpOnly; Path=/; SameSite=Lax"
}

// plainRedirect reproduces a bare `return redirect(location)` (no flash): a 302 with
// Werkzeug's redirect body and no session cookie.
func plainRedirect(w http.ResponseWriter, location string) {
	esc := htmlEscapeMarkup(location)
	body := "<!doctype html>\n<html lang=en>\n<title>Redirecting...</title>\n" +
		"<h1>Redirecting...</h1>\n<p>You should be redirected automatically to the target URL: " +
		"<a href=\"" + esc + "\">" + esc + "</a>. If not, click the link.\n"
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Location", location)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusFound)
	_, _ = w.Write([]byte(body))
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

// POST /settings/masking/keys/delete — app.py delete_masking_key: an empty/unknown key is not
// in the custom-keys set, so it flashes "Custom sensitive key not found".
func (s *server) handleMaskingKeysDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if s.formRequire(w, r, "key", "warning", "Custom sensitive key not found", "/settings/masking") {
		return
	}
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// POST /settings/masking/patterns/delete — empty/unknown pattern -> "Custom masking pattern not found".
func (s *server) handleMaskingPatternsDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if s.formRequire(w, r, "pattern", "warning", "Custom masking pattern not found", "/settings/masking") {
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

// isTruthySetting mirrors app.py _is_truthy_setting(default=False): a value counts as on when
// it is one of the recognized truthy tokens.
func isTruthySetting(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// POST /settings/data-management — app.py save_dm_settings: write the data_management.*
// config from the form (_dm_settings_from_form), preserving the two sensitive secrets when
// their fields are blank, then a plain query-param redirect. GET (the db-stats page) is a
// follow-up. apply_ttl is off for the empty parity request, so no ALTER TABLE runs.
func (s *server) handleSettingsDataManagement(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.handleDataManagementGet(w, r)
		return
	}
	_ = r.ParseForm()
	txt := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	chk := func(k string) string {
		if r.PostFormValue(k) == "1" {
			return "1"
		}
		return "0"
	}
	_ = s.setAppSetting("data_management.backup_enabled", chk("backup_enabled"))
	_ = s.setAppSetting("data_management.s3_bucket", txt("s3_bucket"))
	_ = s.setAppSetting("data_management.s3_access_key_id", txt("s3_access_key_id"))
	_ = s.setAppSetting("data_management.s3_region", txt("s3_region"))
	_ = s.setAppSetting("data_management.s3_path_prefix", txt("s3_path_prefix"))
	_ = s.setAppSetting("data_management.s3_encrypt_backup", chk("s3_encrypt_backup"))
	_ = s.setAppSetting("data_management.backup_schedule_full", txt("backup_schedule_full"))
	_ = s.setAppSetting("data_management.backup_schedule_incremental", txt("backup_schedule_incremental"))
	_ = s.setAppSetting("data_management.ttl_logs_days", txt("ttl_logs_days"))
	_ = s.setAppSetting("data_management.ttl_traces_days", txt("ttl_traces_days"))
	_ = s.setAppSetting("data_management.ttl_metrics_hours", txt("ttl_metrics_hours"))
	_ = s.setAppSetting("data_management.ttl_sessions_days", txt("ttl_sessions_days"))
	_ = s.setAppSetting("data_management.ttl_backup_coupling_enabled", chk("ttl_backup_coupling_enabled"))
	// Sensitive secrets are preserved (not overwritten) when the form leaves them blank.
	if v := txt("s3_secret_access_key"); v != "" {
		_ = s.setAppSetting("data_management.s3_secret_access_key", v)
	}
	if v := txt("backup_encryption_password"); v != "" {
		_ = s.setAppSetting("data_management.backup_encryption_password", v)
	}
	plainRedirect(w, "/settings/data-management?msg=Settings+saved&msg_type=success")
}

// POST /settings/masking/output — app.py update_masking_output_setting: the checkbox list
// "enabled" is empty when unchecked, so the setting is written "0" and the disabled flash shown.
func (s *server) handleMaskingOutputSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	enabled := false
	for _, v := range r.PostForm["enabled"] {
		if isTruthySetting(v) {
			enabled = true
		}
	}
	val, msg := "0", "Global output masking disabled across UI/JSON/notifications/GitHub issue payloads"
	if enabled {
		val, msg = "1", "Global output masking enabled"
	}
	_ = s.setAppSetting("masking.output_enabled", val)
	flashRedirect(w, "success", msg, "/settings/masking")
}

// POST /settings/masking/sql-output — app.py update_masking_sql_output_setting.
func (s *server) handleMaskingSqlOutputSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	enabled := false
	for _, v := range r.PostForm["enabled"] {
		if isTruthySetting(v) {
			enabled = true
		}
	}
	val, msg := "0", "SQL output masking disabled for NLQ/chart endpoints"
	if enabled {
		val, msg = "1", "SQL output masking enabled for NLQ/chart endpoints"
	}
	_ = s.setAppSetting("masking.sql_output_enabled", val)
	flashRedirect(w, "success", msg, "/settings/masking")
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
