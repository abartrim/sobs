package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
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
		row := map[string]any{
			"Id": id, "Name": cStr(m, "Name"), "Description": cStr(m, "Description"),
			"PageType": cStr(m, "PageType"), "FiltersJson": cStr(m, "FiltersJson"),
			"IsDeleted": 1, "Version": fixedVersionMillis(),
		}
		if _, err := s.insertRowsNormalized("sobs_reports", []map[string]any{row}); err != nil {
			s.dbError(w, err)
			return
		}
		flashRedirect(w, "success", fmt.Sprintf("Report '%s' deleted", cStr(m, "Name")), "/reports")
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
				"RuleType":      orDefault(cStr(m, "RuleType"), "threshold"),
				"SignalSource":  cStr(m, "SignalSource"), "SignalName": cStr(m, "SignalName"),
				"ServiceName":   cStr(m, "ServiceName"), "AttrFingerprint": cStr(m, "AttrFingerprint"),
				"Comparator":    cStr(m, "Comparator"),
				"WarningThreshold":  cFloat(m, "WarningThreshold"), "CriticalThreshold": cFloat(m, "CriticalThreshold"),
				"SecondarySignalSource": cStr(m, "SecondarySignalSource"), "SecondarySignalName": cStr(m, "SecondarySignalName"),
				"SecondaryComparator":   orDefault(cStr(m, "SecondaryComparator"), "gt"),
				"SecondaryWarningThreshold": cFloat(m, "SecondaryWarningThreshold"),
				"SecondaryCriticalThreshold": cFloat(m, "SecondaryCriticalThreshold"),
				"MinSampleCount": cInt(m, "MinSampleCount"),
			}
		}, "warning", "Rule not found", "success", "Rule '{name}' deleted", "/metrics/rules")
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
	config := map[string]any{}
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
		config["url"], config["method"], config["headers"], config["body_template"] = ff("webhook_url"), method, headers, ff("webhook_body_template")
		if config["url"] == "" {
			flashRedirect(w, "warning", "Webhook URL is required", loc)
			return
		}
	case "slack":
		config["webhook_url"] = ff("slack_webhook_url")
		if config["webhook_url"] == "" {
			flashRedirect(w, "warning", "Slack webhook URL is required", loc)
			return
		}
	case "email":
		config["smtp_host"] = orDefault(ff("smtp_host"), "localhost")
		config["smtp_port"] = orDefault(ff("smtp_port"), "587")
		config["smtp_user"], config["smtp_password"] = ff("smtp_user"), ff("smtp_password")
		config["from_addr"] = orDefault(ff("from_addr"), "sobs@localhost")
		config["to_addr"] = ff("to_addr")
		config["use_tls"] = orDefault(ff("use_tls"), "1")
		if config["to_addr"] == "" {
			flashRedirect(w, "warning", "Email recipient (to_addr) is required", loc)
			return
		}
	case "browser_push":
		config["endpoint"], config["p256dh"], config["auth"] = ff("push_endpoint"), ff("push_p256dh"), ff("push_auth")
		if config["endpoint"] == "" {
			flashRedirect(w, "warning", "Push endpoint is required", loc)
			return
		}
	}
	config["mask_output_enabled"] = "0"
	if maskEnabled {
		config["mask_output_enabled"] = "1"
	}
	cfgJSON, _ := json.Marshal(config)
	row := map[string]any{
		"Id": newUUIDHex(), "Name": name, "ChannelType": channelType,
		"ConfigJson": string(cfgJSON), "Enabled": 1, "IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_notification_channels", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	flashRedirect(w, "success", fmt.Sprintf("Notification channel '%s' created", name), loc)
}

// POST /settings/notifications/rules — app.py create_notification_rule (create path; the
// edit_rule_id path requires an existing rule which the fixture lacks).
func (s *server) handleNotifRulesCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	loc := "/settings/notifications"
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
	conditions, ok := s.buildNotificationConditions(w, r, loc)
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
	condJSON, _ := json.Marshal(conditions)
	row := map[string]any{
		"Id": newUUIDHex(), "Name": name, "Enabled": 1, "LogicOperator": logicOp,
		"ConditionsJson": string(condJSON), "ChannelIds": strings.Join(chIDs, ","),
		"Severity": severity, "CooldownSeconds": cooldown, "LastFiredAt": "1970-01-01 00:00:00.000",
		"IsDeleted": 0, "Version": fixedVersionMillis(),
	}
	if _, err := s.insertRowsNormalized("sobs_notification_rules", []map[string]any{row}); err != nil {
		s.dbError(w, err)
		return
	}
	flashRedirect(w, "success", fmt.Sprintf("Notification rule '%s' created", name), loc)
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
// Returns (conditions, ok); ok=false means a validation flash was already written.
func (s *server) buildNotificationConditions(w http.ResponseWriter, r *http.Request, loc string) ([]any, bool) {
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
				if _, err := regexp.Compile(tagValue); err != nil {
					flashRedirect(w, "warning", "Invalid tag regex pattern: "+err.Error(), loc)
					return nil, false
				}
			}
			conditions = append(conditions, map[string]any{
				"type": "tag", "record_type": recordType, "tag_key": tagKey,
				"tag_match_operator": tagOp, "tag_value": tagValue,
				"comparator": comparator, "threshold": threshold, "window_minutes": window,
			})
			continue
		}
		source, signal, service := at(sources, i), at(signals, i), at(services, i)
		if source == "" || signal == "" {
			continue
		}
		conditions = append(conditions, map[string]any{
			"type": "signal", "source": source, "signal": signal, "service": service,
			"comparator": comparator, "threshold": threshold, "window_minutes": window,
		})
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
