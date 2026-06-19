package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// writeAppSetting upserts an already-encoded (stored-form) value into sobs_app_settings — the
// shared write path for _set_app_setting/_del_app_setting/_set_dm_setting. UpdatedAt drives the
// ReplacingMergeTree "latest wins" merge; it never appears in any response, so the wall-clock
// microsecond stamp is byte-invisible (Python's _set_dm_setting uses int ms — same instant).
func (s *server) writeAppSetting(key, stored string) error {
	ts := time.Now().UTC().Format("2006-01-02 15:04:05.000000")
	_, err := s.db.InsertJSONEachRow("sobs_app_settings",
		[]map[string]any{{"Key": key, "Value": stored, "UpdatedAt": ts}})
	return err
}

// setAppSetting mirrors app.py _set_app_setting: the WebPush VAPID private key is Fernet-encrypted
// at rest when SOBS_SETTINGS_ENCRYPTION_KEY is configured (a strict no-op otherwise); all other
// keys store verbatim.
func (s *server) setAppSetting(key, value string) error {
	stored := value
	if key == vapidPrivateKeySetting {
		stored = s.encryptSecretValue(value)
	}
	return s.writeAppSetting(key, stored)
}

// delAppSetting mirrors app.py _del_app_setting: tombstone a setting by writing an empty value
// (the latest-wins empty row shadows any prior value under FINAL).
func (s *server) delAppSetting(key string) error {
	return s.writeAppSetting(key, "")
}

// setDMSetting mirrors app.py _set_dm_setting: Fernet-encrypt the two data-management secrets
// (s3_secret_access_key, backup_encryption_password) at rest; store every other DM key verbatim.
func (s *server) setDMSetting(key, value string) error {
	stored := value
	if isSensitiveDMSettingKey(key) {
		stored = s.encryptSecretValue(value)
	}
	return s.writeAppSetting(key, stored)
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

// Session-cookie config (app.py SESSION_COOKIE_NAME / SESSION_COOKIE_SAMESITE /
// SESSION_COOKIE_SECURE), read once at startup. Defaults match Quart's (sobs_session, Lax, and
// Secure only behind TLS) so the parity corpus — which sets none of these — is byte-unchanged.
var (
	sessionCookieName      = envOr("SOBS_SESSION_COOKIE_NAME", "sobs_session")
	sessionCookieSameSite  = normalizeSameSite(envOr("SOBS_SESSION_COOKIE_SAMESITE", "Lax"))
	sessionCookieBehindTLS = envFlag("SOBS_BEHIND_TLS", false)
)

// normalizeSameSite mirrors app.py: lower-case, restrict to {lax,strict,none} (default lax),
// then capitalize to Lax/Strict/None.
func normalizeSameSite(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "lax", "strict", "none":
	default:
		v = "lax"
	}
	return strings.ToUpper(v[:1]) + v[1:]
}

// sessionCookieAttrs returns the trailing cookie attributes: HttpOnly, Path, the configured
// SameSite, and Secure when behind TLS. The default is "; HttpOnly; Path=/; SameSite=Lax".
func sessionCookieAttrs() string {
	a := "; HttpOnly; Path=/; SameSite=" + sessionCookieSameSite
	if sessionCookieBehindTLS {
		a += "; Secure"
	}
	return a
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
	return sessionCookieName + "=" + payload + ".0.0" + sessionCookieAttrs()
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

// flashRedirectWithCiKey is flashRedirect plus the one-time CI-push key stashed in the session
// (mirrors session["ci_push_api_key_plain_by_app"][app_id] = key_plain). The plaintext key makes
// the signed session cookie non-deterministic, so parity masks the whole Set-Cookie value.
func flashRedirectWithCiKey(w http.ResponseWriter, category, message, location, appID, keyPlain string) {
	esc := htmlEscapeMarkup(location)
	body := "<!doctype html>\n<html lang=en>\n<title>Redirecting...</title>\n" +
		"<h1>Redirecting...</h1>\n<p>You should be redirected automatically to the target URL: " +
		"<a href=\"" + esc + "\">" + esc + "</a>. If not, click the link.\n"
	sess := jsonenc.NewObject().
		Set("ci_push_api_key_plain_by_app", jsonenc.NewObject().Set(appID, keyPlain)).
		Set("_flashes", []any{jsonenc.NewObject().Set(" t", []any{category, message})})
	payload := base64.RawURLEncoding.EncodeToString(jsonenc.Encode(sess, flaskSessionOpts))
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Location", location)
	h.Set("Set-Cookie", sessionCookieName+"="+payload+".0.0"+sessionCookieAttrs())
	h.Set("Vary", "Cookie")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusFound)
	_, _ = w.Write([]byte(body))
}

// POST /reports/<report_id>/delete — app.py delete_report: soft-delete (re-insert IsDeleted=1)
// and flash; a missing report flashes "Report not found".
func (s *server) handleReportsFormSub(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/reports/")
	if id, ok := strings.CutSuffix(rest, "/delete"); ok && r.Method == http.MethodPost {
		res, err := s.db.Execute("SELECT Id, Name, Description, PageType, FiltersJson FROM sobs_reports FINAL WHERE IsDeleted = 0 AND Id = ?", id)
		if err != nil || len(res.Rows) == 0 {
			flashRedirect(w, "danger", "Report not found", "/reports")
			return
		}
		m := rowMaps(res)[0]
		// app.py delete_report: _get_report -> _parse_report_filters(FiltersJson) ({} for
		// empty/invalid/non-dict), then FiltersJson = json.dumps(report["filters"],
		// ensure_ascii=False) — re-parse + re-serialize, never the raw stored column.
		row := map[string]any{
			"Id": id, "Name": cStr(m, "Name"), "Description": cStr(m, "Description"),
			"PageType":    cStr(m, "PageType"),
			"FiltersJson": jsonDumpsNoEsc(parseReportFiltersNative(cStr(m, "FiltersJson"))),
			"IsDeleted":   1, "Version": fixedVersionMillis(),
		}
		if _, err := s.insertRowsNormalized("sobs_reports", []map[string]any{row}); err != nil {
			s.dbError(w, err)
			return
		}
		flashRedirect(w, "success", fmt.Sprintf("Report '%s' deleted", cStr(m, "Name")), "/reports")
		return
	}
	if paramMethodGuard(w, r) {
		return
	}
	http.NotFound(w, r)
}

// softDeleteLatestRow mirrors app.py _soft_delete_latest_row: select the live row, and when
// present re-insert a tombstone (the build-deleted-row payload + IsDeleted=1 + Version) and
// flash success; when absent flash the not-found message. Both branches redirect to location.
func (s *server) softDeleteLatestRow(w http.ResponseWriter, selectSQL string, params []any,
	table string, buildDeletedRow func(m map[string]any) map[string]any,
	notFoundCategory, notFoundMsg, successCategory, successMsgTmpl, location string) {
	res, err := s.db.Execute(selectSQL, params...)
	if err != nil {
		s.dbError(w, err)
		return
	}
	if len(res.Rows) == 0 {
		flashRedirect(w, notFoundCategory, notFoundMsg, location)
		return
	}
	m := rowMaps(res)[0]
	payload := buildDeletedRow(m)
	payload["IsDeleted"] = 1
	payload["Version"] = fixedVersionMillis()
	if _, err := s.insertRowsNormalized(table, []map[string]any{payload}); err != nil {
		s.dbError(w, err)
		return
	}
	flashRedirect(w, successCategory, strings.ReplaceAll(successMsgTmpl, "{name}", cStr(m, "Name")), location)
}

// deleteFormID extracts the <id> from a POST .../<id>/delete form route, writing 404 and
// returning ("", false) for any other shape so the caller can return early.
func deleteFormID(w http.ResponseWriter, r *http.Request, prefix string) (string, bool) {
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	id, ok := strings.CutSuffix(rest, "/delete")
	if !ok || r.Method != http.MethodPost {
		if paramMethodGuard(w, r) {
			return "", false
		}
		http.NotFound(w, r)
		return "", false
	}
	return id, true
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

// POST /settings/agents/<id>/delete — app.py delete_agent_rule.
func (s *server) handleSettingsAgentsSub(w http.ResponseWriter, r *http.Request) {
	id, ok := deleteFormID(w, r, "/settings/agents/")
	if !ok {
		return
	}
	s.softDeleteLatestRow(w,
		"SELECT Id, Name FROM sobs_agent_rules FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1", []any{id},
		"sobs_agent_rules", func(m map[string]any) map[string]any {
			return map[string]any{
				"Id": id, "Name": cStr(m, "Name"), "Description": "", "TriggerType": "manual",
				"TriggerRefId": "", "TriggerState": "any", "Actions": "analyze",
				"RateLimitMinutes": 60, "IsEnabled": 0,
			}
		}, "warning", "Agent rule not found", "success", "Agent rule '{name}' deleted", "/settings/agents")
}

// POST /settings/tags/<id>/delete — app.py delete_tag_rule.
func (s *server) handleSettingsTagsSub(w http.ResponseWriter, r *http.Request) {
	id, ok := deleteFormID(w, r, "/settings/tags/")
	if !ok {
		return
	}
	s.softDeleteLatestRow(w,
		"SELECT Id, Name FROM sobs_tag_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1", []any{id},
		"sobs_tag_rules", func(m map[string]any) map[string]any {
			return map[string]any{
				"Id": id, "Name": cStr(m, "Name"), "RecordTypes": "", "MatchField": "",
				"MatchOperator": "eq", "MatchValue": "", "MatchAttrKey": "", "TagKey": "",
				"TagValue": "", "ConditionsJson": "[]",
			}
		}, "warning", "Tag rule not found", "success", "Tag rule '{name}' deleted", "/settings/tags")
}

// POST /metrics/rules/<id>/delete — app.py delete_metrics_rule.
func (s *server) handleMetricsRulesSub(w http.ResponseWriter, r *http.Request) {
	id, ok := deleteFormID(w, r, "/metrics/rules/")
	if !ok {
		return
	}
	s.softDeleteLatestRow(w,
		"SELECT Id, Name, RuleType, SignalSource, SignalName, ServiceName, AttrFingerprint, Comparator, "+
			"WarningThreshold, CriticalThreshold, SecondarySignalSource, SecondarySignalName, "+
			"SecondaryComparator, SecondaryWarningThreshold, SecondaryCriticalThreshold, MinSampleCount "+
			"FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 AND Id = ?", []any{id},
		"sobs_anomaly_rules", func(m map[string]any) map[string]any {
			return map[string]any{
				"Id": cStr(m, "Id"), "Name": cStr(m, "Name"),
				"RuleType":     orDefault(cStr(m, "RuleType"), "threshold"),
				"SignalSource": cStr(m, "SignalSource"), "SignalName": cStr(m, "SignalName"),
				"ServiceName": cStr(m, "ServiceName"), "AttrFingerprint": cStr(m, "AttrFingerprint"),
				"Comparator":       cStr(m, "Comparator"),
				"WarningThreshold": cFloat(m, "WarningThreshold"), "CriticalThreshold": cFloat(m, "CriticalThreshold"),
				"SecondarySignalSource": cStr(m, "SecondarySignalSource"), "SecondarySignalName": cStr(m, "SecondarySignalName"),
				"SecondaryComparator":        orDefault(cStr(m, "SecondaryComparator"), "gt"),
				"SecondaryWarningThreshold":  cFloat(m, "SecondaryWarningThreshold"),
				"SecondaryCriticalThreshold": cFloat(m, "SecondaryCriticalThreshold"),
				"MinSampleCount":             cInt(m, "MinSampleCount"),
			}
		}, "warning", "Rule not found", "success", "Rule '{name}' deleted", "/metrics/rules")
}

// splitIDAction splits "<id>/<action>" from a trimmed sub-route path.
func splitIDAction(rest string) (id, action string) {
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

// handleNotifChannelsSub dispatches POST /settings/notifications/channels/<id>/{delete,toggle}
// (app.py delete_/toggle_notification_channel). A missing channel flashes "not found"; otherwise
// the action mutates and flashes. (The /api/.../test action is a separate route.)
func (s *server) handleNotifChannelsSub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		if paramMethodGuard(w, r) {
			return
		}
		http.NotFound(w, r)
		return
	}
	id, action := splitIDAction(strings.TrimPrefix(r.URL.Path, "/settings/notifications/channels/"))
	switch action {
	case "delete":
		s.softDeleteLatestRow(w,
			"SELECT Id, Name, ChannelType, ConfigJson, Enabled FROM sobs_notification_channels FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
			[]any{id}, "sobs_notification_channels",
			func(m map[string]any) map[string]any {
				return map[string]any{"Id": id, "Name": cStr(m, "Name"), "ChannelType": cStr(m, "ChannelType"),
					"ConfigJson": cStr(m, "ConfigJson"), "Enabled": cInt(m, "Enabled")}
			},
			"warning", "Notification channel not found", "success", "Notification channel '{name}' deleted",
			"/settings/notifications")
	case "toggle":
		s.toggleNotifChannel(w, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) toggleNotifChannel(w http.ResponseWriter, id string) {
	res, err := s.db.Execute(
		"SELECT Id, Name, ChannelType, ConfigJson, Enabled FROM sobs_notification_channels FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1", id)
	if err != nil {
		s.dbError(w, err)
		return
	}
	if len(res.Rows) == 0 {
		flashRedirect(w, "warning", "Notification channel not found", "/settings/notifications")
		return
	}
	m := rowMaps(res)[0]
	newEnabled := 1
	if cInt(m, "Enabled") != 0 {
		newEnabled = 0
	}
	row := map[string]any{"Id": id, "Name": cStr(m, "Name"), "ChannelType": cStr(m, "ChannelType"),
		"ConfigJson": cStr(m, "ConfigJson"), "Enabled": newEnabled, "IsDeleted": 0, "Version": fixedVersionMillis()}
	if _, err := s.insertRowsNormalized("sobs_notification_channels", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	state := "disabled"
	if newEnabled != 0 {
		state = "enabled"
	}
	flashRedirect(w, "success", "Notification channel '"+cStr(m, "Name")+"' "+state, "/settings/notifications")
}

// handleNotifRulesSub dispatches POST /settings/notifications/rules/<id>/{toggle,delete}.
func (s *server) handleNotifRulesSub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		if paramMethodGuard(w, r) {
			return
		}
		http.NotFound(w, r)
		return
	}
	id, action := splitIDAction(strings.TrimPrefix(r.URL.Path, "/settings/notifications/rules/"))
	switch action {
	case "delete":
		s.softDeleteLatestRow(w,
			"SELECT Id, Name, LogicOperator, ConditionsJson, ChannelIds, Severity, CooldownSeconds, Enabled FROM sobs_notification_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
			[]any{id}, "sobs_notification_rules",
			func(m map[string]any) map[string]any {
				return map[string]any{"Id": id, "Name": cStr(m, "Name"), "Enabled": cInt(m, "Enabled"),
					"LogicOperator": cStr(m, "LogicOperator"), "ConditionsJson": cStr(m, "ConditionsJson"),
					"ChannelIds": cStr(m, "ChannelIds"), "Severity": cStr(m, "Severity"),
					"CooldownSeconds": cInt(m, "CooldownSeconds"), "LastFiredAt": "1970-01-01 00:00:00.000"}
			},
			"warning", "Notification rule not found", "success", "Notification rule '{name}' deleted",
			"/settings/notifications")
	case "toggle":
		s.toggleNotifRule(w, id)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) toggleNotifRule(w http.ResponseWriter, id string) {
	res, err := s.db.Execute(
		"SELECT Id, Name, Enabled, LogicOperator, ConditionsJson, ChannelIds, Severity, CooldownSeconds FROM sobs_notification_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1", id)
	if err != nil {
		s.dbError(w, err)
		return
	}
	if len(res.Rows) == 0 {
		flashRedirect(w, "warning", "Notification rule not found", "/settings/notifications")
		return
	}
	m := rowMaps(res)[0]
	newEnabled := 1
	if cInt(m, "Enabled") != 0 {
		newEnabled = 0
	}
	row := map[string]any{"Id": id, "Name": cStr(m, "Name"), "Enabled": newEnabled,
		"LogicOperator": cStr(m, "LogicOperator"), "ConditionsJson": cStr(m, "ConditionsJson"),
		"ChannelIds": cStr(m, "ChannelIds"), "Severity": cStr(m, "Severity"),
		"CooldownSeconds": cInt(m, "CooldownSeconds"), "LastFiredAt": "1970-01-01 00:00:00.000",
		"IsDeleted": 0, "Version": fixedVersionMillis()}
	if _, err := s.insertRowsNormalized("sobs_notification_rules", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	state := "disabled"
	if newEnabled != 0 {
		state = "enabled"
	}
	flashRedirect(w, "success", "Notification rule '"+cStr(m, "Name")+"' "+state, "/settings/notifications")
}

// /settings/repositories/<app_id>/... (delete, realtime-mode, ci-ingest-key/rotate|revoke,
// releases, save): a missing app flashes "Repository entry not found". The github-token/validate
// action is not an app route — it flashes when no GitHub token is configured.
func (s *server) handleSettingsRepositoriesSub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// github-token/validate is a STATIC POST route; the rest are <app_id> param templates.
		if exactMethodGuard(w, r, r.URL.Path) || paramMethodGuard(w, r) {
			return
		}
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/settings/repositories/")
	const repos = "/settings/repositories"
	if rest == "github-token/validate" {
		s.repoGithubTokenValidate(w, r)
		return
	}
	// All other actions are app-scoped: "<app_id>[/<action>]".
	parts := strings.SplitN(rest, "/", 2)
	appID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	current, ok := s.findAppByID(appID)
	if !ok {
		flashRedirect(w, "warning", "Repository entry not found", repos)
		return
	}
	_ = r.ParseForm()
	switch action {
	case "realtime-mode":
		s.repoRealtimeMode(w, r, appID, current)
	case "ci-ingest-key/rotate":
		s.repoCiKeyRotate(w, r, appID, current)
	case "ci-ingest-key/revoke":
		s.repoCiKeyRevoke(w, appID, current)
	case "releases":
		s.repoAddRelease(w, r, appID, current)
	case "delete":
		s.repoDelete(w, appID, current)
	case "":
		s.repoUpdate(w, r, appID, current)
	default:
		flashRedirect(w, "warning", "Repository entry not found", repos)
	}
}

var notificationChannelTypes = map[string]bool{"webhook": true, "slack": true, "email": true, "browser_push": true}
var notificationLogicOperators = map[string]bool{"any": true, "all": true}
var notificationSeverities = map[string]bool{"warning": true, "critical": true}
var notificationComparators = map[string]bool{"gt": true, "lt": true, "gte": true, "lte": true, "eq": true}
var notificationConditionTypes = map[string]bool{"signal": true, "tag": true}

// POST /settings/notifications/channels — app.py create_notification_channel.
func (s *server) handleNotifChannelsCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	loc := "/settings/notifications"
	name := strings.TrimSpace(r.PostFormValue("name"))
	channelType := strings.ToLower(strings.TrimSpace(r.PostFormValue("channel_type")))
	maskVals := r.PostForm["mask_output_enabled"]
	maskEnabled := len(maskVals) == 0
	for _, v := range maskVals {
		if isTruthySetting(v) {
			maskEnabled = true
		}
	}
	if name == "" {
		flashRedirect(w, "warning", "Channel name is required", loc)
		return
	}
	if !notificationChannelTypes[channelType] {
		flashRedirect(w, "warning", "Invalid channel type: "+channelType, loc)
		return
	}
	ff := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	// config is an ordered Object so the stored JSON preserves Python's dict insertion order
	// (json.dumps(sort_keys=False)); a Go map would sort the keys.
	config := jsonenc.NewObject()
	switch channelType {
	case "webhook":
		method := strings.ToUpper(ff("webhook_method"))
		if method == "" {
			method = "POST"
		}
		headers := ff("webhook_headers")
		if headers == "" {
			headers = "{}"
		}
		config.Set("url", ff("webhook_url")).Set("method", method).Set("headers", headers).Set("body_template", ff("webhook_body_template"))
		if u, _ := config.Get("url"); u == "" {
			flashRedirect(w, "warning", "Webhook URL is required", loc)
			return
		}
	case "slack":
		config.Set("webhook_url", ff("slack_webhook_url"))
		if u, _ := config.Get("webhook_url"); u == "" {
			flashRedirect(w, "warning", "Slack webhook URL is required", loc)
			return
		}
	case "email":
		config.Set("smtp_host", orDefault(ff("smtp_host"), "localhost"))
		config.Set("smtp_port", orDefault(ff("smtp_port"), "587"))
		config.Set("smtp_user", ff("smtp_user")).Set("smtp_password", ff("smtp_password"))
		config.Set("from_addr", orDefault(ff("from_addr"), "sobs@localhost"))
		config.Set("to_addr", ff("to_addr"))
		config.Set("use_tls", orDefault(ff("use_tls"), "1"))
		if a, _ := config.Get("to_addr"); a == "" {
			flashRedirect(w, "warning", "Email recipient (to_addr) is required", loc)
			return
		}
	case "browser_push":
		config.Set("endpoint", ff("push_endpoint")).Set("p256dh", ff("push_p256dh")).Set("auth", ff("push_auth"))
		if e, _ := config.Get("endpoint"); e == "" {
			flashRedirect(w, "warning", "Push endpoint is required", loc)
			return
		}
	}
	config.Set("mask_output_enabled", "0")
	if maskEnabled {
		config.Set("mask_output_enabled", "1")
	}
	// Encrypt sensitive config values at rest (no-op when SOBS_SETTINGS_ENCRYPTION_KEY is unset, as
	// in the parity corpus, so the stored bytes stay plaintext), then json.dumps(ensure_ascii=False).
	storedConfig := s.encryptNotificationConfig(config)
	row := map[string]any{
		"Id": newUUIDHex(), "Name": name, "ChannelType": channelType,
		"ConfigJson": string(jsonenc.Encode(storedConfig, dumpsDefault)), "Enabled": 1, "IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_notification_channels", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	flashRedirect(w, "success", fmt.Sprintf("Notification channel '%s' created", name), loc)
}

// POST /settings/notifications/rules — app.py create_notification_rule: create OR update-in-place.
// When edit_rule_id is supplied it reuses the existing rule's Id and preserves Enabled/LastFiredAt
// (flashing "updated"); otherwise it mints a new rule (flashing "created").
func (s *server) handleNotifRulesCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	loc := "/settings/notifications"
	editRuleID := strings.TrimSpace(r.PostFormValue("edit_rule_id"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	logicOp := orDefault(strings.ToLower(strings.TrimSpace(r.PostFormValue("logic_operator"))), "any")
	severity := orDefault(strings.ToLower(strings.TrimSpace(r.PostFormValue("severity"))), "warning")
	cooldown := 300
	if v, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("cooldown_seconds"))); err == nil {
		cooldown = clampInt(v, 0, 86400)
	}
	if name == "" {
		flashRedirect(w, "warning", "Rule name is required", loc)
		return
	}
	if !notificationLogicOperators[logicOp] {
		flashRedirect(w, "warning", "Invalid logic operator: "+logicOp, loc)
		return
	}
	if !notificationSeverities[severity] {
		flashRedirect(w, "warning", "Invalid severity: "+severity, loc)
		return
	}
	conditions, ok := s.buildNotificationConditions(w, r, loc, editRuleID)
	if !ok {
		return // a validation flash was already written
	}
	if len(conditions) == 0 {
		flashRedirect(w, "warning", "At least one condition is required", loc)
		return
	}
	// Only channel ids that exist are kept; the fixture has none, so channel_ids -> "".
	valid := map[string]bool{}
	if res, err := s.db.Execute("SELECT Id FROM sobs_notification_channels FINAL WHERE IsDeleted = 0"); err == nil {
		for _, m := range rowMaps(res) {
			valid[cStr(m, "Id")] = true
		}
	}
	chIDs := []string{}
	for _, c := range r.PostForm["channel_ids"] {
		c = strings.TrimSpace(c)
		if valid[c] {
			chIDs = append(chIDs, c)
		}
	}
	// json.dumps(conditions, ensure_ascii=False): default (spaced) separators, insertion order.
	condJSON := jsonenc.Encode(conditions, dumpsDefault)
	// Defaults for a fresh rule; the edit_rule_id branch overrides id/enabled/last_fired_at from
	// the existing row (app.py:26053-26064).
	enabled := 1
	lastFiredAt := "1970-01-01 00:00:00.000"
	ruleID := newUUIDHex()
	if editRuleID != "" {
		res, err := s.db.Execute(
			"SELECT Id, Enabled, LastFiredAt FROM sobs_notification_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1", editRuleID)
		if err != nil {
			s.dbError(w, err)
			return
		}
		if len(res.Rows) == 0 {
			flashRedirect(w, "warning", "Notification rule not found for editing", loc)
			return
		}
		ex := rowMaps(res)[0]
		ruleID = cStr(ex, "Id")
		enabled = cInt(ex, "Enabled")
		lastFiredAt = cStr(ex, "LastFiredAt")
	}
	row := map[string]any{
		"Id": ruleID, "Name": name, "Enabled": enabled, "LogicOperator": logicOp,
		"ConditionsJson": string(condJSON), "ChannelIds": strings.Join(chIDs, ","),
		"Severity": severity, "CooldownSeconds": cooldown, "LastFiredAt": lastFiredAt,
		"IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_notification_rules", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	verb := "created"
	if editRuleID != "" {
		verb = "updated"
	}
	flashRedirect(w, "success", fmt.Sprintf("Notification rule '%s' %s", name, verb), loc)
}

// POST /settings/masking/keys — app.py add_masking_key.
func (s *server) handleMaskingKeysCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	key := normalizeSensitiveKey(r.PostFormValue("key"))
	if key == "" {
		flashRedirect(w, "warning", "Sensitive key name is required", "/settings/masking")
		return
	}
	if s.effectiveKeyActive(key) {
		flashRedirect(w, "info", fmt.Sprintf("Sensitive key '%s' is already active", key), "/settings/masking")
		return
	}
	s.saveMaskingCustomKeys(append(s.loadMaskingCustomKeys(), key))
	flashRedirect(w, "success", fmt.Sprintf("Sensitive key '%s' added", key), "/settings/masking")
}

// POST /settings/masking/keys/delete — app.py delete_masking_key.
func (s *server) handleMaskingKeysDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	key := normalizeSensitiveKey(r.PostFormValue("key"))
	custom := s.loadMaskingCustomKeys()
	if !containsStr(custom, key) {
		flashRedirect(w, "warning", "Custom sensitive key not found", "/settings/masking")
		return
	}
	kept := []string{}
	for _, k := range custom {
		if k != key {
			kept = append(kept, k)
		}
	}
	s.saveMaskingCustomKeys(kept)
	flashRedirect(w, "success", fmt.Sprintf("Sensitive key '%s' removed", key), "/settings/masking")
}

// POST /settings/masking/patterns — app.py add_masking_pattern.
func (s *server) handleMaskingPatternsCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	pattern, err := validateCustomMaskingPattern(r.PostFormValue("pattern"))
	if err != nil {
		flashRedirect(w, "warning", "Invalid regex pattern: "+err.Error(), "/settings/masking")
		return
	}
	if s.effectivePatternActive(pattern) {
		flashRedirect(w, "info", "That regex pattern is already active", "/settings/masking")
		return
	}
	s.saveMaskingCustomPatterns(append(s.loadMaskingCustomPatterns(), pattern))
	flashRedirect(w, "success", "Custom masking pattern added", "/settings/masking")
}

// POST /settings/masking/patterns/delete — app.py delete_masking_pattern.
func (s *server) handleMaskingPatternsDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	pattern, err := validateCustomMaskingPattern(r.PostFormValue("pattern"))
	if err != nil {
		flashRedirect(w, "warning", "Custom masking pattern not found", "/settings/masking")
		return
	}
	custom := s.loadMaskingCustomPatterns()
	if !containsStr(custom, pattern) {
		flashRedirect(w, "warning", "Custom masking pattern not found", "/settings/masking")
		return
	}
	kept := []string{}
	for _, p := range custom {
		if p != pattern {
			kept = append(kept, p)
		}
	}
	s.saveMaskingCustomPatterns(kept)
	flashRedirect(w, "success", "Custom masking pattern removed", "/settings/masking")
}

var notificationTagMatchOps = map[string]bool{"eq": true, "contains": true, "regex": true}
var notificationTagRecordTypes = map[string]bool{"all": true, "log": true, "trace": true, "error": true, "ai": true, "rum": true}

// buildNotificationConditions mirrors the per-row condition loop in create_notification_rule.
// Returns (conditions, ok); ok=false means a validation flash was already written. editRuleID is
// the form's edit_rule_id: only the tag-regex error redirect is edit-aware (app.py:26002-26006);
// every other validation flash redirects to the plain view_notifications location.
func (s *server) buildNotificationConditions(w http.ResponseWriter, r *http.Request, loc, editRuleID string) ([]any, bool) {
	get := func(k string) []string { return r.PostForm[k] }
	at := func(xs []string, i int) string {
		if i < len(xs) {
			return strings.TrimSpace(xs[i])
		}
		return ""
	}
	condTypes, sources, signals, services := get("cond_type"), get("cond_source"), get("cond_signal"), get("cond_service")
	recordTypes, tagKeys, tagOps, tagValues := get("cond_record_type"), get("cond_tag_key"), get("cond_tag_match_operator"), get("cond_tag_value")
	comparators, thresholds, windows := get("cond_comparator"), get("cond_threshold"), get("cond_window_minutes")
	rowCount := 0
	for _, xs := range [][]string{condTypes, sources, signals, services, recordTypes, tagKeys, tagOps, tagValues, comparators, thresholds, windows} {
		if len(xs) > rowCount {
			rowCount = len(xs)
		}
	}
	conditions := []any{}
	for i := 0; i < rowCount; i++ {
		condType := strings.ToLower(orDefault(at(condTypes, i), "signal"))
		if !notificationConditionTypes[condType] {
			flashRedirect(w, "warning", "Invalid notification condition type: "+condType, loc)
			return nil, false
		}
		comparator := strings.ToLower(orDefault(at(comparators, i), "gt"))
		threshold := 0.0
		if f, err := strconv.ParseFloat(orDefault(at(thresholds, i), "0"), 64); err == nil {
			threshold = f
		}
		window := 5
		if v, err := strconv.Atoi(orDefault(at(windows, i), "5")); err == nil {
			window = clampInt(v, 1, 60)
		}
		if !notificationComparators[comparator] {
			comparator = "gt"
		}
		if condType == "tag" {
			recordType := strings.ToLower(orDefault(at(recordTypes, i), "all"))
			tagKey := at(tagKeys, i)
			tagOp := strings.ToLower(orDefault(at(tagOps, i), "eq"))
			tagValue := at(tagValues, i)
			if tagKey == "" {
				continue
			}
			if !notificationTagRecordTypes[recordType] {
				recordType = "all"
			}
			if !notificationTagMatchOps[tagOp] {
				tagOp = "eq"
			}
			if tagOp == "regex" {
				if _, err := compileUserRegex(tagValue, false); err != nil {
					// app.py:26002-26006: this error redirects to view_notifications carrying
					// edit_rule=<id> when editing, else the plain location.
					flashRedirect(w, "warning", "Invalid tag regex pattern: "+err.Error(), s.notifViewRedirect(editRuleID))
					return nil, false
				}
			}
			// Ordered Object so the stored ConditionsJson preserves Python's dict insertion
			// order (json.dumps(sort_keys=False)); threshold is a float64 so it renders "0.0"
			// (Python float), not "0" (a Go map + encoding/json would do both wrong).
			conditions = append(conditions, jsonenc.NewObject().
				Set("type", "tag").Set("record_type", recordType).Set("tag_key", tagKey).
				Set("tag_match_operator", tagOp).Set("tag_value", tagValue).
				Set("comparator", comparator).Set("threshold", threshold).Set("window_minutes", window))
			continue
		}
		source, signal, service := at(sources, i), at(signals, i), at(services, i)
		if source == "" || signal == "" {
			continue
		}
		conditions = append(conditions, jsonenc.NewObject().
			Set("type", "signal").Set("source", source).Set("signal", signal).Set("service", service).
			Set("comparator", comparator).Set("threshold", threshold).Set("window_minutes", window))
	}
	return conditions, true
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

// POST /settings/data-management — app.py save_dm_settings: write the data_management.* config
// from the form (_dm_settings_from_form), in that exact key order. Per key: a clear_* flag
// tombstones the secret; a sensitive key left blank is preserved (skipped); a non-empty value is
// written (sensitive ones Fernet-encrypted via _set_dm_setting); an empty value tombstones
// (_del_app_setting). When apply_ttl=1 is submitted, each configured retention is pushed to
// ClickHouse via ALTER TABLE … MODIFY TTL; on any error it redirects with the "Settings saved but
// TTL errors:" warning (otherwise a plain query-param redirect). GET is the db-stats page.
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
	// _dm_settings_from_form — order matters (Python iterates the dict in insertion order).
	newSettings := []struct{ key, val string }{
		{"data_management.backup_enabled", chk("backup_enabled")},
		{"data_management.s3_bucket", txt("s3_bucket")},
		{"data_management.s3_access_key_id", txt("s3_access_key_id")},
		{"data_management.s3_secret_access_key", txt("s3_secret_access_key")},
		{"data_management.s3_region", txt("s3_region")},
		{"data_management.s3_path_prefix", txt("s3_path_prefix")},
		{"data_management.s3_encrypt_backup", chk("s3_encrypt_backup")},
		{"data_management.backup_encryption_password", txt("backup_encryption_password")},
		{"data_management.backup_schedule_full", txt("backup_schedule_full")},
		{"data_management.backup_schedule_incremental", txt("backup_schedule_incremental")},
		{"data_management.ttl_logs_days", txt("ttl_logs_days")},
		{"data_management.ttl_traces_days", txt("ttl_traces_days")},
		{"data_management.ttl_metrics_hours", txt("ttl_metrics_hours")},
		{"data_management.ttl_sessions_days", txt("ttl_sessions_days")},
		{"data_management.ttl_backup_coupling_enabled", chk("ttl_backup_coupling_enabled")},
	}
	clearSensitive := map[string]bool{}
	if r.PostFormValue("clear_s3_secret_access_key") == "1" {
		clearSensitive["data_management.s3_secret_access_key"] = true
	}
	if r.PostFormValue("clear_backup_encryption_password") == "1" {
		clearSensitive["data_management.backup_encryption_password"] = true
	}
	for _, kv := range newSettings {
		switch {
		case clearSensitive[kv.key]:
			_ = s.delAppSetting(kv.key)
		case isSensitiveDMSettingKey(kv.key) && kv.val == "":
			// Preserve existing sensitive values when the form field is intentionally left blank.
		case kv.val != "":
			_ = s.setDMSetting(kv.key, kv.val)
		default:
			_ = s.delAppSetting(kv.key)
		}
	}
	// Apply TTL immediately if the form requested it (app.py save_dm_settings: the apply_ttl=1
	// branch runs _apply_dm_ttl on new_settings; on any error it redirects with a warning).
	if r.PostFormValue("apply_ttl") == "1" {
		ttlSettings := map[string]string{
			"data_management.ttl_logs_days":     txt("ttl_logs_days"),
			"data_management.ttl_traces_days":   txt("ttl_traces_days"),
			"data_management.ttl_sessions_days": txt("ttl_sessions_days"),
			"data_management.ttl_metrics_hours": txt("ttl_metrics_hours"),
		}
		if errs := s.applyDMTTL(ttlSettings); len(errs) > 0 {
			if len(errs) > 3 {
				errs = errs[:3]
			}
			msg := "Settings saved but TTL errors: " + strings.Join(errs, "; ")
			plainRedirect(w, "/settings/data-management?msg="+msg+"&msg_type=warning")
			return
		}
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
	// This subtree carries several /dashboards/<id>/... param templates (GET view, POST
	// delete/charts/clone/edit/charts-<cid>-delete); guard wrong methods on any of them first.
	if paramMethodGuard(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/dashboards/")
	parts := strings.Split(rest, "/")
	dashID := parts[0]
	res, err := s.db.Execute("SELECT Id, Name, Description FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Id = ?", dashID)
	if err != nil {
		s.dbError(w, err)
		return
	}
	if len(res.Rows) == 0 {
		flashRedirect(w, "danger", "Dashboard not found", "/dashboards")
		return
	}
	dash := rowMaps(res)[0]
	switch {
	case r.Method == http.MethodGet && rest == dashID:
		s.viewCustomDashboard(w, dash)
	case rest == dashID+"/delete":
		s.deleteDashboard(w, dashID, cStr(dash, "Name"), cStr(dash, "Description"))
	case rest == dashID+"/charts":
		s.addChart(w, r, dashID)
	case len(parts) == 4 && parts[1] == "charts" && parts[3] == "delete":
		s.removeChart(w, parts[2], dashID)
	case len(parts) == 4 && parts[1] == "charts" && parts[3] == "edit":
		s.editChart(w, r, dashID, parts[2])
	case len(parts) == 4 && parts[1] == "charts" && parts[3] == "clone":
		s.cloneChart(w, r, dashID, parts[2])
	default:
		http.NotFound(w, r)
	}
}

// viewCustomDashboard mirrors app.py view_custom_dashboard: render custom_dashboard_view.html
// with the dashboard, its charts (each carrying a rebuilt chart_spec), and the static chart-
// template catalog (CHART_TEMPLATES sorted by id + _default_chart_spec, embedded as an asset).
func (s *server) viewCustomDashboard(w http.ResponseWriter, dash map[string]any) {
	dashID := cStr(dash, "Id")
	dashboard := map[string]any{
		"id":          dashID,
		"name":        cStr(dash, "Name"),
		"description": cStr(dash, "Description"),
	}
	charts, err := s.getCharts(dashID)
	if err != nil {
		s.dbError(w, err)
		return
	}
	templates, err := parseJSONValue(dashboardViewTemplatesJSON)
	if err != nil {
		s.dbError(w, err)
		return
	}
	s.renderPage(w, "custom_dashboard_view.html", "view_custom_dashboard", map[string]any{
		"dashboard": dashboard,
		"charts":    charts,
		"templates": templates,
	})
}

// getCharts mirrors app.py _get_charts: load a dashboard's non-deleted charts ordered by
// Position,Id; rebuild each chart_spec via buildRawChartSpec and re-wrap OptionsJson as
// json.dumps({"chart_spec": ...}, ensure_ascii=False).
func (s *server) getCharts(dashID string) ([]any, error) {
	res, err := s.db.Execute("SELECT Id, Title, ChartType, Query, OptionsJson, Position "+
		"FROM sobs_chart_configs FINAL WHERE IsDeleted = 0 AND DashboardId = ? "+
		"ORDER BY Position, Id", dashID)
	if err != nil {
		return nil, err
	}
	charts := []any{}
	for _, c := range rowMaps(res) {
		chartType := cStr(c, "ChartType")
		query := cStr(c, "Query")
		chartSpec := buildRawChartSpec(chartType, query, cStr(c, "OptionsJson"))
		optionsJSON := string(jsonenc.Encode(jsonenc.NewObject().Set("chart_spec", chartSpec), jsonDumpsDefault))
		charts = append(charts, jsonenc.NewObject().
			Set("id", cStr(c, "Id")).
			Set("title", cStr(c, "Title")).
			Set("chart_type", chartType).
			Set("query", query).
			Set("options_json", optionsJSON).
			Set("position", cInt(c, "Position")).
			Set("chart_spec", chartSpec))
	}
	return charts, nil
}

// deleteDashboard mirrors app.py delete_dashboard: soft-delete the dashboard and all its charts,
// then flash success and redirect to the dashboards list.
func (s *server) deleteDashboard(w http.ResponseWriter, dashID, name, description string) {
	version := fixedVersionMillis()
	if _, err := s.insertRowsNormalized("sobs_dashboards", []map[string]any{{
		"Id": dashID, "Name": name, "Description": description, "IsDeleted": 1, "Version": version,
	}}); err != nil {
		s.dbError(w, err)
		return
	}
	// app.py delete_dashboard sources its chart tombstones from _get_charts (rebuilt chart_spec
	// + re-wrapped {"chart_spec": ...} options_json, ORDER BY Position, Id), not the raw column.
	charts, err := s.getCharts(dashID)
	if err != nil {
		s.dbError(w, err)
		return
	}
	tombstones := []map[string]any{}
	for _, c := range charts {
		co := c.(*jsonenc.Object)
		tombstones = append(tombstones, map[string]any{
			"Id": chartObjStr(co, "id"), "DashboardId": dashID, "Title": chartObjStr(co, "title"),
			"ChartType": chartObjStr(co, "chart_type"), "Query": chartObjStr(co, "query"),
			"OptionsJson": chartObjStr(co, "options_json"), "Position": chartObjInt(co, "position"),
			"IsDeleted": 1, "Version": version,
		})
	}
	if len(tombstones) > 0 {
		if _, err := s.insertRowsNormalized("sobs_chart_configs", tombstones); err != nil {
			s.dbError(w, err)
			return
		}
	}
	flashRedirect(w, "success", "Dashboard '"+name+"' deleted", "/dashboards")
}

// addChart mirrors app.py add_chart + _parse_chart_form_submission: validate title/chart_spec_json,
// compile, insert at the next position, then plain-redirect to the dashboard.
func (s *server) addChart(w http.ResponseWriter, r *http.Request, dashID string) {
	_ = r.ParseForm()
	loc := "/dashboards/" + dashID
	title := strings.TrimSpace(r.PostFormValue("title"))
	chartSpecJSON := strings.TrimSpace(r.PostFormValue("chart_spec_json"))
	if title == "" {
		flashRedirect(w, "warning", "Chart title is required", loc)
		return
	}
	if chartSpecJSON == "" {
		flashRedirect(w, "warning", "Chart spec is required", loc)
		return
	}
	specRaw, perr := parseJSONValue([]byte(chartSpecJSON))
	if perr != nil {
		flashRedirect(w, "warning", "Chart spec error: "+perr.Error(), loc)
		return
	}
	templateID, query, normSpec, errMsg := s.compileChartSpec(specRaw)
	if errMsg != "" {
		flashRedirect(w, "warning", "Chart spec error: "+errMsg, loc)
		return
	}
	optionsJSON := string(jsonenc.Encode(jsonenc.NewObject().Set("chart_spec", normSpec), jsonDumpsDefault))
	row := map[string]any{
		"Id": newUUIDv4(), "DashboardId": dashID, "Title": title, "ChartType": templateID,
		"Query": query, "OptionsJson": optionsJSON, "Position": s.nextChartPosition(dashID),
		"IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_chart_configs", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	plainRedirect(w, loc)
}

// removeChart mirrors app.py remove_chart: tombstone the chart (404-flash when absent), then
// plain-redirect to the dashboard.
func (s *server) removeChart(w http.ResponseWriter, chartID, dashID string) {
	loc := "/dashboards/" + dashID
	// app.py remove_chart: charts = _get_charts(...); chart = next(c for c if c["id"]==chart_id),
	// then tombstone the rebuilt chart values (options_json re-wrapped), not the raw column.
	charts, err := s.getCharts(dashID)
	if err != nil {
		s.dbError(w, err)
		return
	}
	var c *jsonenc.Object
	for _, ch := range charts {
		co := ch.(*jsonenc.Object)
		if chartObjStr(co, "id") == chartID {
			c = co
			break
		}
	}
	if c == nil {
		flashRedirect(w, "warning", "Chart not found", loc)
		return
	}
	row := map[string]any{
		"Id": chartID, "DashboardId": dashID, "Title": chartObjStr(c, "title"),
		"ChartType": chartObjStr(c, "chart_type"), "Query": chartObjStr(c, "query"),
		"OptionsJson": chartObjStr(c, "options_json"), "Position": chartObjInt(c, "position"),
		"IsDeleted": 1, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_chart_configs", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	plainRedirect(w, loc)
}

// parseChartFormSubmission mirrors app.py _parse_chart_form_submission: validate title +
// chart_spec_json, parse+compile the spec, and return (title, templateID, query, optionsJSON).
// On any failure it returns a non-empty errMsg matching the Python ValueError text.
func (s *server) parseChartFormSubmission(r *http.Request) (title, templateID, query, optionsJSON, errMsg string) {
	title = strings.TrimSpace(r.PostFormValue("title"))
	chartSpecJSON := strings.TrimSpace(r.PostFormValue("chart_spec_json"))
	if title == "" {
		return "", "", "", "", "Chart title is required"
	}
	if chartSpecJSON == "" {
		return "", "", "", "", "Chart spec is required"
	}
	specRaw, perr := parseJSONValue([]byte(chartSpecJSON))
	if perr != nil {
		return "", "", "", "", "Chart spec error: " + perr.Error()
	}
	tid, q, normSpec, cErr := s.compileChartSpec(specRaw)
	if cErr != "" {
		return "", "", "", "", "Chart spec error: " + cErr
	}
	optionsJSON = string(jsonenc.Encode(jsonenc.NewObject().Set("chart_spec", normSpec), jsonDumpsDefault))
	return title, tid, q, optionsJSON, ""
}

// editChart mirrors app.py edit_chart: look up the chart within the dashboard (404-flash when
// absent), parse the form (warning-flash on ValueError), then re-INSERT the chart row at its
// EXISTING position/id and plain-redirect to the dashboard. The version is a wall-clock millis
// value that is only stored, never emitted — so the redirect body is byte-stable.
func (s *server) editChart(w http.ResponseWriter, r *http.Request, dashID, chartID string) {
	loc := "/dashboards/" + dashID
	charts, err := s.getCharts(dashID)
	if err != nil {
		s.dbError(w, err)
		return
	}
	var chart *jsonenc.Object
	for _, ch := range charts {
		co := ch.(*jsonenc.Object)
		if chartObjStr(co, "id") == chartID {
			chart = co
			break
		}
	}
	if chart == nil {
		flashRedirect(w, "warning", "Chart not found", loc)
		return
	}
	_ = r.ParseForm()
	title, templateID, query, optionsJSON, errMsg := s.parseChartFormSubmission(r)
	if errMsg != "" {
		flashRedirect(w, "warning", errMsg, loc)
		return
	}
	row := map[string]any{
		"Id": chartID, "DashboardId": dashID, "Title": title, "ChartType": templateID,
		"Query": query, "OptionsJson": optionsJSON, "Position": chartObjInt(chart, "position"),
		"IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_chart_configs", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	plainRedirect(w, loc)
}

// cloneChart mirrors app.py clone_chart: look up the source chart within the dashboard
// (404-flash when absent), parse the form (warning-flash on ValueError), then INSERT a NEW
// chart (fresh uuid id, next position) and plain-redirect to the dashboard. The new uuid is
// only stored, never emitted (the redirect targets the existing dashboard_id) — so the redirect
// body is byte-stable.
func (s *server) cloneChart(w http.ResponseWriter, r *http.Request, dashID, chartID string) {
	loc := "/dashboards/" + dashID
	charts, err := s.getCharts(dashID)
	if err != nil {
		s.dbError(w, err)
		return
	}
	var source *jsonenc.Object
	for _, ch := range charts {
		co := ch.(*jsonenc.Object)
		if chartObjStr(co, "id") == chartID {
			source = co
			break
		}
	}
	if source == nil {
		flashRedirect(w, "warning", "Chart not found", loc)
		return
	}
	_ = r.ParseForm()
	title, templateID, query, optionsJSON, errMsg := s.parseChartFormSubmission(r)
	if errMsg != "" {
		flashRedirect(w, "warning", errMsg, loc)
		return
	}
	// app.py: position = max(c["position"] for c in charts, default=-1) + 1.
	position := -1
	for _, ch := range charts {
		co := ch.(*jsonenc.Object)
		if p := chartObjInt(co, "position"); p > position {
			position = p
		}
	}
	position++
	row := map[string]any{
		"Id": newUUIDv4(), "DashboardId": dashID, "Title": title, "ChartType": templateID,
		"Query": query, "OptionsJson": optionsJSON, "Position": position,
		"IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_chart_configs", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	plainRedirect(w, loc)
}
