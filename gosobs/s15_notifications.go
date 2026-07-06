// s15_notifications.go — port of app.py notifications/webhooks, enrichment
// settings keys + geo cache + CVE scanner tuning, app-settings key-value store,
// VAPID key resolution, and all notification routes.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Notifications / Webhooks — constants & helpers
// ---------------------------------------------------------------------------

var notificationChannelTypes = []string{"webhook", "slack", "email", "browser_push"}
var notificationComparators = []string{"gt", "lt", "gte", "lte", "eq"}
var notificationSeverities = []string{"warning", "critical"}
var notificationLogicOperators = []string{"any", "all"} // any=OR, all=AND
var notificationConditionTypes = []string{"signal", "tag"}
var notificationTagMatchOperators = []string{"eq", "contains", "regex"}
var notificationTagRecordTypes = []string{"all", "log", "trace", "error", "ai", "rum"}

// VAPID JWT expiry window (12 hours)
const vapidJwtExpirySeconds = 43200

// DB setting key for the VAPID private key
const vapidPrivateKeySetting = "vapid_private_key"

// Web Push AES-128-GCM record size per RFC 8291
const pushRecordSize = 4096

// notifContains reports whether s is present in list.
func notifContains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Enrichment – settings keys, geo-lookup cache, and CVE scanner
//
// Geolocation: geoip2fast (MIT license).  Data sourced from IANA/RIR delegated
// statistics files (public domain).  All lookups are performed locally against
// a bundled .dat.gz file — no external API calls for geolocation.
// Reference: https://github.com/rabuchaim/geoip2fast
//
// CVE data: OSV.dev (Apache 2.0, free, no API key required).
// Library versions are extracted from release metadata plus OTEL data.
// Reference: https://google.github.io/osv.dev/api/
// ---------------------------------------------------------------------------
const (
	geoEnabledSetting                  = "enrichment.geo_enabled"
	cveEnabledSetting                  = "enrichment.cve_enabled"
	cveLastScanSetting                 = "enrichment.cve_last_scan"
	githubBackfillMaxReleasesSetting   = "enrichment.github_backfill_max_releases"
	cveLastBackfillAttemptedSetting    = "enrichment.cve_last_scan_github_backfill_attempted"
	cveLastBackfillInsertedSetting     = "enrichment.cve_last_scan_github_backfill_inserted"
	cveLastBackfillCapSetting          = "enrichment.cve_last_scan_github_backfill_cap"
	githubRepoHealthLastSyncSetting    = "enrichment.github_repo_health_last_sync"
	githubRepoHealthLastSummarySetting = "enrichment.github_repo_health_last_summary"
)

// Simple bounded in-process geo cache: {ip: geo_dict}
//
// PORT-NOTE: Python uses an OrderedDict LRU; the Go port uses a plain map (the
// readers in s11 already note the lost recency ordering).
var (
	geoCache     = map[string]map[string]any{}
	geoCacheMax  = 2000
	geoCacheLock sync.Mutex

	// Lazy-loaded geoip2fast instance (any so getGeoDb in s11 can type-assert
	// the geoIp2FastLookup contract; nil means disabled).
	geoDb     any
	geoDbLock sync.Mutex
)

// CVE scanner tuning constants
const (
	cveScanInitialDelayS = 30    // seconds before the first scan after startup
	cveScanIntervalS     = 86400 // seconds between scans (24 hours)
	cveMaxVulnsPerPkg    = 10    // max OSV.dev results stored per package

	githubBackfillMaxReleasesDefault       = 300
	githubBackfillMaxReleasesMin           = 1
	githubBackfillMaxReleasesMax           = 2000
	githubRepoHealthMaxRepos               = 25
	githubRepoHealthMaxItemsPerRepo        = 100
	githubActionsSnapshotArtifactName      = "sobs-release-dependency-snapshots"
	githubActionsBackfillMaxRunsPerRelease = 20
	githubRepoHealthInitialDelayS          = 45
	githubRepoHealthIntervalS              = 3600
)

// cveDispositionValues mirrors the Python set of valid CVE dispositions.
var cveDispositionValues = map[string]bool{
	"open": true, "accepted": true, "false_positive": true, "fixed": true,
}

// Available signal sources for condition building (mirrors v_derived_signals_1m signals)
//
// PORT-NOTE: Python uses an ordered dict; Go map iteration order is random, so
// the template's signal-source ordering is not preserved.
var notificationSignalSources = map[string][]string{
	"logs":   {"log_volume", "error_volume", "error_ratio"},
	"traces": {"trace_volume", "trace_error_ratio", "latency_p95_ms"},
	"errors": {"exception_volume"},
}

var notificationSensitiveConfigKeys = map[string]bool{
	"smtp_password": true, "auth_token": true, "api_key": true,
	"webhook_url": true, "url": true, "auth": true,
}

// ---------------------------------------------------------------------------
// App-settings DB helpers  (simple key-value store backed by sobs_app_settings)
// ---------------------------------------------------------------------------

// getAppSetting returns a value from sobs_app_settings, or "" if the key is
// absent/empty (mirrors Python's None for the empty case).
func getAppSetting(db *ChDbConnection, key string) string {
	value := ""
	res, err := db.Execute(
		"SELECT Value FROM sobs_app_settings FINAL WHERE Key = ? LIMIT 1",
		key,
	)
	if err == nil {
		if row := res.Fetchone(); row != nil {
			value = strings.TrimSpace(rowString(row["Value"]))
		}
	}
	if key == vapidPrivateKeySetting {
		value = decryptSecretValue(value)
	}
	return value
}

var (
	appSettingsLastUpdatedAtMs int64
	appSettingsUpdatedAtLock   sync.Mutex
)

// nextAppSettingUpdatedAt returns a monotonic UTC timestamp string for
// sobs_app_settings writes.
//
// PORT-NOTE: a mutex guards the monotonic counter since Go handlers run
// concurrently (the Python original relied on the single-threaded event loop).
func nextAppSettingUpdatedAt() string {
	appSettingsUpdatedAtLock.Lock()
	defer appSettingsUpdatedAtLock.Unlock()
	nowMs := time.Now().UnixMilli()
	if nowMs <= appSettingsLastUpdatedAtMs {
		nowMs = appSettingsLastUpdatedAtMs + 1
	}
	appSettingsLastUpdatedAtMs = nowMs
	return time.UnixMilli(nowMs).UTC().Format("2006-01-02 15:04:05.000000")
}

// setAppSetting upserts a value in sobs_app_settings.
func setAppSetting(db *ChDbConnection, key, value string) {
	stored := value
	if key == vapidPrivateKeySetting {
		stored = encryptSecretValue(value)
	}
	updatedAt := nextAppSettingUpdatedAt()
	insertRowsJsonEachRow(
		db,
		"sobs_app_settings",
		[]Row{{"Key": key, "Value": stored, "UpdatedAt": updatedAt}},
	)
	if key == maskingOutputEnabledSetting {
		v := isTruthySetting(value, true)
		setMaskingSettingsCache(&v, nil, true)
	} else if key == maskingSqlOutputEnabledSetting {
		v := isTruthySetting(value, true)
		setMaskingSettingsCache(nil, &v, true)
	}
}

// delAppSetting clears a setting from sobs_app_settings by writing an empty
// value (tombstone).
func delAppSetting(db *ChDbConnection, key string) {
	updatedAt := nextAppSettingUpdatedAt()
	insertRowsJsonEachRow(
		db,
		"sobs_app_settings",
		[]Row{{"Key": key, "Value": "", "UpdatedAt": updatedAt}},
	)
	if key == maskingOutputEnabledSetting {
		v := true
		setMaskingSettingsCache(&v, nil, true)
	} else if key == maskingSqlOutputEnabledSetting {
		v := true
		setMaskingSettingsCache(nil, &v, true)
	}
}

func loadJsonStringListSetting(db *ChDbConnection, key string) []string {
	raw := getAppSetting(db, key)
	if raw == "" {
		return []string{}
	}
	var values []any
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		logger.Warn(fmt.Sprintf("Invalid JSON list in app setting %s", key))
		return []string{}
	}
	result := []string{}
	for _, item := range values {
		text := strings.TrimSpace(rowString(item))
		if text != "" {
			result = append(result, text)
		}
	}
	return result
}

func saveJsonStringListSetting(db *ChDbConnection, key string, values []string) {
	if len(values) == 0 {
		delAppSetting(db, key)
		return
	}
	encoded, _ := json.Marshal(values)
	setAppSetting(db, key, string(encoded))
}

func loadMaskingCustomKeys(db *ChDbConnection) []string {
	set := map[string]bool{}
	for _, value := range loadJsonStringListSetting(db, maskingCustomKeysSetting) {
		if key := maskingNormalizeSensitiveKey(value); key != "" {
			set[key] = true
		}
	}
	return sortedStringSet(set)
}

func saveMaskingCustomKeys(db *ChDbConnection, keys []string) {
	set := map[string]bool{}
	for _, value := range keys {
		if key := maskingNormalizeSensitiveKey(value); key != "" {
			set[key] = true
		}
	}
	saveJsonStringListSetting(db, maskingCustomKeysSetting, sortedStringSet(set))
}

func loadMaskingCustomPatterns(db *ChDbConnection) []string {
	patterns := []string{}
	for _, value := range loadJsonStringListSetting(db, maskingCustomPatternsSetting) {
		normalized, err := validateCustomMaskingPatternForStorage(value)
		if err != nil {
			logger.Warn("Ignoring invalid custom masking pattern from settings")
			continue
		}
		patterns = append(patterns, normalized)
	}
	return dedupePreserveOrder(patterns)
}

func saveMaskingCustomPatterns(db *ChDbConnection, patterns []string) {
	normalized := []string{}
	for _, value := range patterns {
		v, _ := validateCustomMaskingPatternForStorage(value)
		normalized = append(normalized, v)
	}
	saveJsonStringListSetting(db, maskingCustomPatternsSetting, dedupePreserveOrder(normalized))
}

func loadMaskingSettings(db *ChDbConnection) map[string]any {
	customKeys := loadMaskingCustomKeys(db)
	customPatterns := loadMaskingCustomPatterns(db)
	effectiveKeySet := copyKeySet(maskingDefaultSensitiveKeys)
	for _, k := range customKeys {
		effectiveKeySet[k] = true
	}
	effectivePatterns := append(append([]string{}, maskingDefaultSensitivePatterns...), customPatterns...)
	return map[string]any{
		"custom_keys":                customKeys,
		"custom_patterns":            customPatterns,
		"default_keys":               sortedStringSet(maskingDefaultSensitiveKeys),
		"default_patterns":           append([]string{}, maskingDefaultSensitivePatterns...),
		"effective_keys":             sortedStringSet(effectiveKeySet),
		"effective_patterns":         effectivePatterns,
		"output_masking_enabled":     isOutputMaskingEnabled(db),
		"sql_output_masking_enabled": isSqlOutputMaskingEnabled(db),
	}
}

func refreshMaskingRuntimeRules(db *ChDbConnection) {
	customKeys := loadMaskingCustomKeys(db)
	customPatterns := loadMaskingCustomPatterns(db)
	signature := [2][]string{customKeys, customPatterns}

	maskingRulesRefreshLock.Lock()
	defer maskingRulesRefreshLock.Unlock()
	if maskingLastRulesSignature != nil && strSliceEqual(maskingLastRulesSignature[0], customKeys) && strSliceEqual(maskingLastRulesSignature[1], customPatterns) {
		return
	}
	_, _ = maskingConfigureRuntimeRules(customKeys, customPatterns)
	sig := signature
	maskingLastRulesSignature = &sig
}

// strSliceEqual reports whether two string slices are element-wise equal.
func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// refreshMaskingRulesBeforeRequest ports the @app.before_request hook.
//
// PORT-NOTE: the Quart before_request registration is owned by the core
// request pipeline; here we expose the per-request refresh body. The "static"
// endpoint short-circuit is handled by the static file handler upstream.
func refreshMaskingRulesBeforeRequest() {
	func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Debug("Failed to refresh masking rules for request")
			}
		}()
		refreshMaskingRuntimeRules(getDb())
	}()
}

// ---------------------------------------------------------------------------
// VAPID key resolution  (env var takes precedence over DB)
// ---------------------------------------------------------------------------

// getVapidPrivateKeyB64 returns (private_key_b64url, source) where source is
// "env" or "db", or ("", "").
func getVapidPrivateKeyB64(db *ChDbConnection) (string, string) {
	envKey := strings.TrimSpace(os.Getenv("SOBS_VAPID_PRIVATE_KEY"))
	if envKey != "" {
		return envKey, "env"
	}
	resolvedDb := db
	if resolvedDb == nil {
		resolvedDb = getDb()
	}
	dbKey := getAppSetting(resolvedDb, vapidPrivateKeySetting)
	if dbKey != "" {
		return dbKey, "db"
	}
	return "", ""
}

// getVapidPublicKey returns (public_key_b64url, source) or ("", "").
func getVapidPublicKey(db *ChDbConnection) (string, string) {
	privateB64, source := getVapidPrivateKeyB64(db)
	if privateB64 == "" || source == "" {
		return "", ""
	}
	keyBytes, err := decodeBase64UrlPadded(privateB64)
	if err != nil {
		return "", ""
	}
	privateKey, err := loadVapidPrivateKey(keyBytes)
	if err != nil {
		return "", ""
	}
	pubBytes, err := ecdsaPublicUncompressed(privateKey)
	if err != nil {
		return "", ""
	}
	return base64.RawURLEncoding.EncodeToString(pubBytes), source
}

// ---------------------------------------------------------------------------
// Notification channel config encryption + loaders
// ---------------------------------------------------------------------------

// parseJsonObject parses a JSON object string into a map, returning an empty
// map on failure (mirrors json.loads(... or "{}")).
func parseJsonObject(raw string) map[string]any {
	out := map[string]any{}
	text := strings.TrimSpace(raw)
	if text == "" {
		return out
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func encryptNotificationConfig(config map[string]any) map[string]any {
	encrypted := map[string]any{}
	for key, value := range config {
		if s, ok := value.(string); ok && notificationSensitiveConfigKeys[key] {
			encrypted[key] = encryptSecretValue(s)
		} else {
			encrypted[key] = value
		}
	}
	return encrypted
}

func decryptNotificationConfig(config map[string]any) map[string]any {
	decrypted := map[string]any{}
	for key, value := range config {
		if s, ok := value.(string); ok && notificationSensitiveConfigKeys[key] {
			decrypted[key] = decryptSecretValue(s)
		} else {
			decrypted[key] = value
		}
	}
	return decrypted
}

// loadNotificationChannels returns all active notification channels.
func loadNotificationChannels(db *ChDbConnection) []map[string]any {
	res, err := db.Execute(
		"SELECT Id, Name, ChannelType, ConfigJson, Enabled " +
			"FROM sobs_notification_channels FINAL WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		return []map[string]any{}
	}
	out := []map[string]any{}
	for _, row := range res.Fetchall() {
		out = append(out, map[string]any{
			"id":           rowString(row["Id"]),
			"name":         rowString(row["Name"]),
			"channel_type": rowString(row["ChannelType"]),
			"config":       decryptNotificationConfig(parseJsonObject(rowString(row["ConfigJson"]))),
			"enabled":      coerceInt(row["Enabled"]) != 0,
		})
	}
	return out
}

func normalizeNotificationCondition(raw any) map[string]any {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}

	conditionType := strings.ToLower(strings.TrimSpace(rowStringOr(m["type"], "signal")))
	if conditionType == "tag" {
		recordType := strings.ToLower(strings.TrimSpace(rowStringOr(m["record_type"], "all")))
		if !notifContains(notificationTagRecordTypes, recordType) {
			recordType = "all"
		}
		tagMatchOperator := strings.ToLower(strings.TrimSpace(rowStringOr(m["tag_match_operator"], "eq")))
		if !notifContains(notificationTagMatchOperators, tagMatchOperator) {
			tagMatchOperator = "eq"
		}
		comparator := strings.ToLower(strings.TrimSpace(rowStringOr(m["comparator"], "gt")))
		if !notifContains(notificationComparators, comparator) {
			comparator = "gt"
		}
		threshold := parseFloatOrZero(m["threshold"])
		windowMinutes := clampWindowMinutes(m["window_minutes"])
		return map[string]any{
			"type":               "tag",
			"record_type":        recordType,
			"tag_key":            strings.TrimSpace(rowString(m["tag_key"])),
			"tag_match_operator": tagMatchOperator,
			"tag_value":          strings.TrimSpace(rowString(m["tag_value"])),
			"comparator":         comparator,
			"threshold":          threshold,
			"window_minutes":     windowMinutes,
		}
	}

	comparator := strings.ToLower(strings.TrimSpace(rowStringOr(m["comparator"], "gt")))
	if !notifContains(notificationComparators, comparator) {
		comparator = "gt"
	}
	threshold := parseFloatOrZero(m["threshold"])
	windowMinutes := clampWindowMinutes(m["window_minutes"])
	return map[string]any{
		"type":           "signal",
		"source":         strings.TrimSpace(rowString(m["source"])),
		"signal":         strings.TrimSpace(rowString(m["signal"])),
		"service":        strings.TrimSpace(rowString(m["service"])),
		"comparator":     comparator,
		"threshold":      threshold,
		"window_minutes": windowMinutes,
	}
}

// rowStringOr mirrors Python's `str(raw.get(k) or default)` — the default is
// used when the value is missing OR falsy (empty / nil).
func rowStringOr(value any, def string) string {
	if value == nil {
		return def
	}
	s := rowString(value)
	if s == "" {
		return def
	}
	return s
}

// parseFloatOrZero mirrors `float(raw.get(k) or 0)` with TypeError/ValueError
// → 0.0.
func parseFloatOrZero(value any) float64 {
	if value == nil {
		return 0.0
	}
	if f, ok := coerceFloat(value); ok {
		return f
	}
	switch v := value.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0.0
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0.0
		}
		return f
	default:
		return 0.0
	}
}

// clampWindowMinutes mirrors max(1, min(60, int(raw.get('window_minutes') or 5)))
// with parse-failure → 5.
func clampWindowMinutes(value any) int {
	wm := 5
	if value != nil {
		if iv, ok := coerceIntStrict(value); ok {
			wm = iv
		} else {
			wm = 5
		}
	}
	if wm < 1 {
		wm = 1
	}
	if wm > 60 {
		wm = 60
	}
	return wm
}

// coerceIntStrict mirrors Python int(x): truncates floats, parses int-ish
// strings, returns (0,false) on failure.
func coerceIntStrict(value any) (int, bool) {
	switch v := value.(type) {
	case nil:
		return 0, false
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return int(f), true
		}
		return 0, false
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, false
		}
		if iv, err := strconv.Atoi(s); err == nil {
			return iv, true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int(f), true
		}
		return 0, false
	default:
		return 0, false
	}
}

func parseNotificationConditionsJson(raw any) []map[string]any {
	text := strings.TrimSpace(rowString(raw))
	if text == "" {
		return []map[string]any{}
	}
	var parsed []any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return []map[string]any{}
	}
	normalized := []map[string]any{}
	for _, item := range parsed {
		if cond := normalizeNotificationCondition(item); cond != nil {
			normalized = append(normalized, cond)
		}
	}
	return normalized
}

// loadNotificationRules returns all active notification rules.
func loadNotificationRules(db *ChDbConnection) []map[string]any {
	res, err := db.Execute(
		"SELECT Id, Name, Enabled, LogicOperator, ConditionsJson, ChannelIds, " +
			"Severity, CooldownSeconds, LastFiredAt " +
			"FROM sobs_notification_rules FINAL WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		return []map[string]any{}
	}
	out := []map[string]any{}
	for _, row := range res.Fetchall() {
		logicOperator := rowString(row["LogicOperator"])
		if logicOperator == "" {
			logicOperator = "any"
		}
		severity := rowString(row["Severity"])
		if severity == "" {
			severity = "warning"
		}
		channelIds := []string{}
		for _, c := range strings.Split(rowString(row["ChannelIds"]), ",") {
			if c = strings.TrimSpace(c); c != "" {
				channelIds = append(channelIds, c)
			}
		}
		out = append(out, map[string]any{
			"id":               rowString(row["Id"]),
			"name":             rowString(row["Name"]),
			"enabled":          coerceInt(row["Enabled"]) != 0,
			"logic_operator":   logicOperator,
			"conditions":       parseNotificationConditionsJson(row["ConditionsJson"]),
			"channel_ids":      channelIds,
			"severity":         severity,
			"cooldown_seconds": coerceInt(row["CooldownSeconds"]),
			"last_fired_at":    rowString(row["LastFiredAt"]),
		})
	}
	return out
}

// loadNotificationLog returns recent notification delivery log entries.
func loadNotificationLog(db *ChDbConnection, limit int) []map[string]any {
	res, err := db.Execute(
		"SELECT Id, RuleId, RuleName, ChannelId, ChannelName, FiredAt, Status, ErrorMessage, Summary "+
			"FROM sobs_notification_log ORDER BY FiredAt DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return []map[string]any{}
	}
	out := []map[string]any{}
	for _, row := range res.Fetchall() {
		out = append(out, map[string]any{
			"id":            rowString(row["Id"]),
			"rule_id":       rowString(row["RuleId"]),
			"rule_name":     rowString(row["RuleName"]),
			"channel_id":    rowString(row["ChannelId"]),
			"channel_name":  rowString(row["ChannelName"]),
			"fired_at":      rowString(row["FiredAt"]),
			"status":        rowString(row["Status"]),
			"error_message": rowString(row["ErrorMessage"]),
			"summary":       rowString(row["Summary"]),
		})
	}
	return out
}

// maskChannelConfig returns config with sensitive fields masked for display in
// the UI.
func maskChannelConfig(channelType string, config map[string]any) map[string]any {
	masked := map[string]any{}
	for k, v := range config {
		masked[k] = v
	}
	sensitiveKeys := []string{"smtp_password", "auth_token", "api_key"}
	for _, key := range sensitiveKeys {
		if v, ok := masked[key]; ok {
			if s, isStr := v.(string); isStr && s != "" {
				masked[key] = "••••••••"
			} else if !isStr && v != nil && v != false {
				masked[key] = "••••••••"
			}
		}
	}
	return masked
}

func notificationChannelMaskOutputEnabled(channel map[string]any) bool {
	configAny, ok := channel["config"]
	if !ok {
		return true
	}
	config, ok := configAny.(map[string]any)
	if !ok {
		return true
	}
	raw, ok := config["mask_output_enabled"]
	if !ok || raw == nil || strings.TrimSpace(rowString(raw)) == "" {
		return true
	}
	return isTruthySetting(rowString(raw), true)
}

// pyFloatStr mimics Python's str(float): integer-valued floats keep a trailing
// ".0" (e.g. 5.0 -> "5.0", 1.25 -> "1.25").
//
// PORT-NOTE: not a byte-for-byte match of CPython repr for extreme magnitudes,
// but matches for the threshold/value range used in notification summaries.
func pyFloatStr(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// buildNotificationPayload builds a notification payload from a triggered rule
// and its matched conditions.
func buildNotificationPayload(rule map[string]any, firedConditions []map[string]any, maskOutputEnabled bool) map[string]any {
	var conditionsPayload any
	if maskOutputEnabled {
		conditionsPayload = maskValueForOutput(firedConditions, nil)
	} else {
		conditionsPayload = firedConditions
	}

	comparatorLabels := map[string]string{"gt": ">", "lt": "<", "gte": "≥", "lte": "≤", "eq": "="}
	conditionSummaries := []string{}
	for _, cond := range firedConditions {
		comp, ok := comparatorLabels[rowStringOr(cond["comparator"], "gt")]
		if !ok {
			comp = ">"
		}
		thr, _ := coerceFloat(cond["threshold"])
		valueStr := "n/a"
		if v, present := cond["_value"]; present {
			vf, _ := coerceFloat(v)
			valueStr = pyFloatStr(vf)
		}
		if rowStringOr(cond["type"], "signal") == "tag" {
			recordType := rowString(cond["record_type"])
			recordTypeStr := ""
			if recordType != "" && recordType != "all" {
				recordTypeStr = fmt.Sprintf("[%s] ", recordType)
			}
			tagKey := rowString(cond["tag_key"])
			tagMatchOperator := rowStringOr(cond["tag_match_operator"], "eq")
			tagValue := rowString(cond["tag_value"])
			tagExpr := tagKey
			if tagValue != "" {
				tagExpr = fmt.Sprintf("%s %s %s", tagKey, tagMatchOperator, tagValue)
			}
			conditionSummaries = append(conditionSummaries, fmt.Sprintf(
				"tag %s%s %s %s (value=%s)",
				recordTypeStr, tagExpr, comp, pyFloatStr(thr), valueStr))
		} else {
			svc := rowString(cond["service"])
			serviceStr := ""
			if svc != "" {
				serviceStr = fmt.Sprintf(" [%s]", svc)
			}
			conditionSummaries = append(conditionSummaries, fmt.Sprintf(
				"%s/%s%s %s %s (value=%s)",
				rowString(cond["source"]), rowString(cond["signal"]), serviceStr, comp, pyFloatStr(thr), valueStr))
		}
	}
	summary := fmt.Sprintf("[SOBS] Rule '%s' triggered (%s): ",
		rowString(rule["name"]), strings.ToUpper(rowString(rule["severity"]))) +
		strings.Join(conditionSummaries, "; ")
	if maskOutputEnabled {
		summary = maskStringForOutput(summary, nil)
	}
	return map[string]any{
		"rule_name":  rule["name"],
		"severity":   rule["severity"],
		"conditions": conditionsPayload,
		"summary":    summary,
		"fired_at":   pyIsoFormat(time.Now().UTC()),
	}
}

// ---------------------------------------------------------------------------
// Channel dispatchers
// ---------------------------------------------------------------------------

// PORT-NOTE: Python uses a shared async httpx client (_get_async_http_client);
// the Go port uses a per-call *http.Client with the same per-request timeout.

// dispatchWebhookChannel dispatches a notification via generic HTTP webhook.
func dispatchWebhookChannel(config map[string]any, payload map[string]any) error {
	urlStr := strings.TrimSpace(rowString(config["url"]))
	if urlStr == "" {
		return fmt.Errorf("Webhook URL is not configured")
	}
	method := strings.ToUpper(strings.TrimSpace(rowStringOr(config["method"], "POST")))
	headersMap := map[string]any{}
	switch hv := config["headers"].(type) {
	case string:
		_ = json.Unmarshal([]byte(hv), &headersMap) // failure -> empty map
	case map[string]any:
		headersMap = hv
	}
	headers := map[string]string{}
	for k, v := range headersMap {
		headers[k] = rowString(v)
	}
	if _, ok := headers["Content-Type"]; !ok {
		headers["Content-Type"] = "application/json"
	}

	var content []byte
	bodyTemplate := strings.TrimSpace(rowString(config["body_template"]))
	if bodyTemplate != "" {
		body := strings.ReplaceAll(bodyTemplate, "{{summary}}", rowString(payload["summary"]))
		content = []byte(body)
	} else {
		content, _ = json.Marshal(payload)
	}

	req, err := http.NewRequest(method, urlStr, bytes.NewReader(content))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// dispatchSlackChannel dispatches a notification via Slack Incoming Webhook.
func dispatchSlackChannel(config map[string]any, payload map[string]any) error {
	webhookURL := strings.TrimSpace(rowString(config["webhook_url"]))
	if webhookURL == "" {
		return fmt.Errorf("Slack webhook_url is not configured")
	}
	text := rowStringOr(payload["summary"], "SOBS notification triggered")
	body, _ := json.Marshal(map[string]any{"text": text})
	req, err := http.NewRequest("POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Slack webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// dispatchEmailChannel dispatches a notification via SMTP email.
func dispatchEmailChannel(config map[string]any, payload map[string]any) error {
	smtpHost := strings.TrimSpace(rowStringOr(config["smtp_host"], "localhost"))
	smtpPort := 587
	if v, ok := coerceIntStrict(config["smtp_port"]); ok {
		smtpPort = v
	}
	smtpUser := strings.TrimSpace(rowString(config["smtp_user"]))
	smtpPassword := strings.TrimSpace(rowString(config["smtp_password"]))
	fromAddr := strings.TrimSpace(rowStringOr(config["from_addr"], "sobs@localhost"))
	toAddr := strings.TrimSpace(rowString(config["to_addr"]))
	useTls := false
	switch strings.TrimSpace(rowStringOr(config["use_tls"], "1")) {
	case "1", "true", "yes":
		useTls = true
	}
	if toAddr == "" {
		return fmt.Errorf("Email to_addr is not configured")
	}

	subject := rowStringOr(payload["summary"], "SOBS Notification")
	// PORT-NOTE: Python slices the subject by 200 characters; Go slices by bytes.
	if len(subject) > 200 {
		subject = subject[:200]
	}
	bodyJson, _ := json.MarshalIndent(payload, "", "  ")
	msg := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		fromAddr, toAddr, subject, string(bodyJson))

	addr := fmt.Sprintf("%s:%d", smtpHost, smtpPort)
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, smtpHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Close()
	if useTls {
		if err := client.StartTLS(&tls.Config{ServerName: smtpHost}); err != nil {
			return err
		}
	}
	if smtpUser != "" && smtpPassword != "" {
		auth := smtp.PlainAuth("", smtpUser, smtpPassword, smtpHost)
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(fromAddr); err != nil {
		return err
	}
	if err := client.Rcpt(toAddr); err != nil {
		return err
	}
	wc, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write([]byte(msg)); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// ---------------------------------------------------------------------------
// Web Push crypto helpers (RFC 8291 / RFC 8188, VAPID ES256)
// ---------------------------------------------------------------------------

// padBase64 mirrors Python _pad_base64: normalize the URL-safe alphabet to the
// standard alphabet and add '=' padding as needed.
func padBase64(s string) string {
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	if padding := 4 - len(s)%4; padding != 4 {
		s += strings.Repeat("=", padding)
	}
	return s
}

// decodeBase64UrlPadded decodes a base64 string that may use the URL-safe
// alphabet and/or omit padding.
func decodeBase64UrlPadded(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(padBase64(s))
}

// ecdsaPrivateKeyFromScalar builds a P-256 ECDSA private key from a raw 32-byte
// scalar (mirrors cryptography.derive_private_key).
func ecdsaPrivateKeyFromScalar(scalar []byte) *ecdsa.PrivateKey {
	curve := elliptic.P256()
	priv := &ecdsa.PrivateKey{D: new(big.Int).SetBytes(scalar)}
	priv.PublicKey.Curve = curve
	priv.PublicKey.X, priv.PublicKey.Y = curve.ScalarBaseMult(scalar)
	return priv
}

// loadVapidPrivateKey loads a P-256 ECDSA private key from DER (PKCS8 or SEC1)
// or, failing that, from a raw 32-byte scalar (mirrors the Python fallback to
// derive_private_key).
func loadVapidPrivateKey(keyBytes []byte) (*ecdsa.PrivateKey, error) {
	if k, err := x509.ParsePKCS8PrivateKey(keyBytes); err == nil {
		if ec, ok := k.(*ecdsa.PrivateKey); ok {
			return ec, nil
		}
	}
	if ec, err := x509.ParseECPrivateKey(keyBytes); err == nil {
		return ec, nil
	}
	if len(keyBytes) < 32 {
		return nil, fmt.Errorf("invalid VAPID private key length")
	}
	return ecdsaPrivateKeyFromScalar(keyBytes[:32]), nil
}

// ecdsaPublicUncompressed returns the X9.62 uncompressed-point encoding of an
// ECDSA P-256 public key (0x04 || X || Y, 65 bytes).
func ecdsaPublicUncompressed(priv *ecdsa.PrivateKey) ([]byte, error) {
	pub, err := priv.PublicKey.ECDH()
	if err != nil {
		return nil, err
	}
	return pub.Bytes(), nil
}

// buildVapidJwt builds a signed JWT (ES256) for VAPID authentication.
func buildVapidJwt(claims map[string]any, privateKey *ecdsa.PrivateKey) (string, error) {
	headerJson, _ := json.Marshal(map[string]any{"typ": "JWT", "alg": "ES256"})
	bodyJson, _ := json.Marshal(claims)
	header := base64.RawURLEncoding.EncodeToString(headerJson)
	body := base64.RawURLEncoding.EncodeToString(bodyJson)
	signingInput := header + "." + body
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, privateKey, digest[:])
	if err != nil {
		return "", err
	}
	// DER signature re-encoded as raw r||s (64 bytes).
	rawSig := make([]byte, 64)
	r.FillBytes(rawSig[:32])
	s.FillBytes(rawSig[32:])
	sigB64 := base64.RawURLEncoding.EncodeToString(rawSig)
	return signingInput + "." + sigB64, nil
}

// encryptPushPayload encrypts a Web Push payload using AES-128-GCM
// (RFC 8291 / RFC 8188). Returns (body, salt, serverPubBytes).
func encryptPushPayload(plaintext, subscriberPubKeyBytes, authBytes []byte) ([]byte, []byte, []byte, error) {
	curve := ecdh.P256()
	serverPriv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	serverPubBytes := serverPriv.PublicKey().Bytes()

	subPub, err := curve.NewPublicKey(subscriberPubKeyBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	sharedSecret, err := serverPriv.ECDH(subPub)
	if err != nil {
		return nil, nil, nil, err
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, nil, nil, err
	}

	hkdfExtract := func(saltBytes, ikm []byte) []byte {
		h := hmac.New(sha256.New, saltBytes)
		h.Write(ikm)
		return h.Sum(nil)
	}
	hkdfExpand := func(prk, info []byte, length int) []byte {
		var output, t []byte
		counter := byte(1)
		for len(output) < length {
			h := hmac.New(sha256.New, prk)
			h.Write(t)
			h.Write(info)
			h.Write([]byte{counter})
			t = h.Sum(nil)
			output = append(output, t...)
			counter++
		}
		return output[:length]
	}

	authInfo := append([]byte("WebPush: info\x00"), subscriberPubKeyBytes...)
	authInfo = append(authInfo, serverPubBytes...)
	prkCombine := hkdfExtract(authBytes, sharedSecret)
	ikm := hkdfExpand(prkCombine, authInfo, 32)

	prk := hkdfExtract(salt, ikm)
	cek := hkdfExpand(prk, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hkdfExpand(prk, []byte("Content-Encoding: nonce\x00"), 12)

	padded := append(append([]byte{}, plaintext...), 0x02) // 0x02 = last record delimiter
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, nil, nil, err
	}
	aesgcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, err
	}
	ciphertextRaw := aesgcm.Seal(nil, nonce, padded, nil)

	rs := []byte{
		byte(pushRecordSize >> 24), byte(pushRecordSize >> 16),
		byte(pushRecordSize >> 8), byte(pushRecordSize & 0xff),
	}
	header := append([]byte{}, salt...)
	header = append(header, rs...)
	header = append(header, byte(len(serverPubBytes)))
	header = append(header, serverPubBytes...)
	out := append(header, ciphertextRaw...)
	return out, salt, serverPubBytes, nil
}

// dispatchBrowserPushChannel dispatches a notification via Web Push (VAPID).
func dispatchBrowserPushChannel(config map[string]any, payload map[string]any) error {
	endpoint := strings.TrimSpace(rowString(config["endpoint"]))
	p256dh := strings.TrimSpace(rowString(config["p256dh"]))
	auth := strings.TrimSpace(rowString(config["auth"]))
	if endpoint == "" || p256dh == "" || auth == "" {
		return fmt.Errorf("browser_push channel is missing endpoint, p256dh, or auth")
	}

	vapidPrivateKeyB64, _ := getVapidPrivateKeyB64(nil)
	vapidSubject := strings.TrimSpace(os.Getenv("SOBS_VAPID_SUBJECT"))
	if vapidSubject == "" {
		vapidSubject = "mailto:sobs@localhost"
	}
	if vapidPrivateKeyB64 == "" {
		return fmt.Errorf("VAPID private key is not configured — generate one on the Notifications settings page")
	}

	p256dhBytes, err := decodeBase64UrlPadded(p256dh)
	if err != nil {
		return err
	}
	authBytes, err := decodeBase64UrlPadded(auth)
	if err != nil {
		return err
	}

	parsed, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	audience := fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
	nowTs := time.Now().Unix()
	jwtPayload := map[string]any{
		"aud": audience,
		"exp": nowTs + vapidJwtExpirySeconds,
		"sub": vapidSubject,
	}

	vapidKeyBytes, err := decodeBase64UrlPadded(vapidPrivateKeyB64)
	if err != nil {
		return err
	}
	vapidPrivateKey, err := loadVapidPrivateKey(vapidKeyBytes)
	if err != nil {
		return err
	}
	vapidPublicKeyBytes, err := ecdsaPublicUncompressed(vapidPrivateKey)
	if err != nil {
		return err
	}
	vapidPublicB64 := base64.RawURLEncoding.EncodeToString(vapidPublicKeyBytes)

	jwtToken, err := buildVapidJwt(jwtPayload, vapidPrivateKey)
	if err != nil {
		return err
	}
	messageBytes, _ := json.Marshal(map[string]any{
		"title": "SOBS Alert",
		"body":  rowString(payload["summary"]),
	})
	ciphertext, _, _, err := encryptPushPayload(messageBytes, p256dhBytes, authBytes)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(ciphertext))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("vapid t=%s,k=%s", jwtToken, vapidPublicB64))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 && resp.StatusCode != 202 {
		return fmt.Errorf("Push service returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// dispatchNotificationChannel dispatches a notification to one channel and
// returns "ok" or an error message.
func dispatchNotificationChannel(channel map[string]any, payload map[string]any) string {
	channelType := rowString(channel["channel_type"])
	config, _ := channel["config"].(map[string]any)
	if config == nil {
		config = map[string]any{}
	}
	var err error
	switch channelType {
	case "webhook":
		err = dispatchWebhookChannel(config, payload)
	case "slack":
		err = dispatchSlackChannel(config, payload)
	case "email":
		err = dispatchEmailChannel(config, payload)
	case "browser_push":
		err = dispatchBrowserPushChannel(config, payload)
	default:
		return fmt.Sprintf("Unknown channel type: %s", channelType)
	}
	if err != nil {
		return err.Error()
	}
	return "ok"
}

// ---------------------------------------------------------------------------
// Rule evaluation
// ---------------------------------------------------------------------------

// evalComparator applies a notification comparator to (value, threshold).
func evalComparator(comparator string, currentValue, threshold float64) bool {
	switch comparator {
	case "gt":
		return currentValue > threshold
	case "lt":
		return currentValue < threshold
	case "gte":
		return currentValue >= threshold
	case "lte":
		return currentValue <= threshold
	case "eq":
		return math.Abs(currentValue-threshold) < 1e-9
	default:
		return false
	}
}

// evaluateSignalCondition evaluates a signal condition against recent signal
// data. Returns (matched, currentValue).
func evaluateSignalCondition(db *ChDbConnection, cond map[string]any) (bool, float64) {
	source := strings.TrimSpace(rowString(cond["source"]))
	signal := strings.TrimSpace(rowString(cond["signal"]))
	service := strings.TrimSpace(rowString(cond["service"]))
	comparator := strings.TrimSpace(rowStringOr(cond["comparator"], "gt"))
	threshold := parseFloatOrZero(cond["threshold"])
	windowMinutes := clampWindowMinutes(cond["window_minutes"])

	if source == "" || signal == "" {
		return false, 0.0
	}

	serviceFilter := ""
	params := []any{windowMinutes, source, signal}
	if service != "" {
		serviceFilter = " AND ServiceName = ?"
		params = append(params, service)
	}
	params = append(params, 1) // SampleCount >= 1

	res, err := db.Execute(
		"SELECT avg(Value) AS v FROM v_derived_signals_1m "+
			"WHERE MinuteBucket >= now() - INTERVAL ? MINUTE "+
			"AND SignalSource = ? AND SignalName = ?"+
			serviceFilter+" "+
			"HAVING count() >= ?",
		params...,
	)
	if err != nil {
		return false, 0.0
	}
	row := res.Fetchone()
	if row == nil {
		return false, 0.0
	}
	currentValue := 0.0
	if v, ok := coerceFloat(row["v"]); ok {
		currentValue = v
	}
	return evalComparator(comparator, currentValue, threshold), currentValue
}

// evaluateTagCondition evaluates a tag condition against recent tag
// assignments. Returns (matched, currentValue).
func evaluateTagCondition(db *ChDbConnection, cond map[string]any) (bool, float64) {
	recordType := strings.ToLower(strings.TrimSpace(rowStringOr(cond["record_type"], "all")))
	tagKey := strings.TrimSpace(rowString(cond["tag_key"]))
	tagMatchOperator := strings.ToLower(strings.TrimSpace(rowStringOr(cond["tag_match_operator"], "eq")))
	tagValue := strings.TrimSpace(rowString(cond["tag_value"]))
	comparator := strings.TrimSpace(rowStringOr(cond["comparator"], "gt"))
	threshold := parseFloatOrZero(cond["threshold"])
	windowMinutes := clampWindowMinutes(cond["window_minutes"])

	if tagKey == "" {
		return false, 0.0
	}

	minVersion := int64((float64(time.Now().UnixNano())/1e9 - float64(windowMinutes*60)) * 1000)
	whereParts := []string{"IsDeleted = 0", "Version >= ?", "TagKey = ?"}
	params := []any{minVersion, tagKey}
	if recordType != "" && recordType != "all" {
		whereParts = append(whereParts, "RecordType = ?")
		params = append(params, recordType)
	}
	if tagValue != "" {
		switch tagMatchOperator {
		case "eq":
			whereParts = append(whereParts, "TagValue = ?")
			params = append(params, tagValue)
		case "contains":
			whereParts = append(whereParts, "positionCaseInsensitive(TagValue, ?) > 0")
			params = append(params, tagValue)
		case "regex":
			whereParts = append(whereParts, "match(TagValue, ?)")
			params = append(params, tagValue)
		}
	}
	res, err := db.Execute(
		"SELECT count() AS c FROM sobs_record_tags FINAL WHERE "+strings.Join(whereParts, " AND "),
		params...,
	)
	if err != nil {
		return false, 0.0
	}
	currentValue := 0.0
	if row := res.Fetchone(); row != nil {
		if v, ok := coerceFloat(row["c"]); ok {
			currentValue = v
		}
	}
	return evalComparator(comparator, currentValue, threshold), currentValue
}

// evaluateNotificationCondition dispatches to the signal or tag evaluator.
func evaluateNotificationCondition(db *ChDbConnection, cond map[string]any) (bool, float64) {
	conditionType := strings.ToLower(strings.TrimSpace(rowStringOr(cond["type"], "signal")))
	if conditionType == "tag" {
		return evaluateTagCondition(db, cond)
	}
	return evaluateSignalCondition(db, cond)
}

// checkNotificationRule evaluates one rule, dispatches if triggered, and
// returns a status map.
func checkNotificationRule(db *ChDbConnection, rule map[string]any, channelsById map[string]map[string]any) map[string]any {
	ruleId := rowString(rule["id"])
	enabled, _ := rule["enabled"].(bool)
	if !enabled {
		return map[string]any{"rule_id": ruleId, "fired": false, "reason": "disabled"}
	}

	// Cooldown check
	lastFiredTs := 0.0
	if res, err := db.Execute(
		"SELECT toUnixTimestamp64Milli(LastFiredAt) AS ts "+
			"FROM sobs_notification_rules FINAL WHERE Id = ? LIMIT 1",
		ruleId,
	); err == nil {
		if row := res.Fetchone(); row != nil {
			if v, ok := coerceFloat(row["ts"]); ok {
				lastFiredTs = v / 1000.0
			}
		}
	}
	cooldown := 300
	if v, ok := coerceIntStrict(rule["cooldown_seconds"]); ok {
		cooldown = v
	}
	nowTs := float64(time.Now().UnixNano()) / 1e9
	if nowTs-lastFiredTs < float64(cooldown) {
		return map[string]any{"rule_id": ruleId, "fired": false, "reason": "cooldown"}
	}

	// Evaluate conditions
	conditions, _ := rule["conditions"].([]map[string]any)
	logic := rowStringOr(rule["logic_operator"], "any")
	firedConditions := []map[string]any{}
	notFired := []map[string]any{}
	for _, cond := range conditions {
		matched, value := evaluateNotificationCondition(db, cond)
		annotated := map[string]any{}
		for k, v := range cond {
			annotated[k] = v
		}
		annotated["_value"] = math.Round(value*10000) / 10000
		if matched {
			firedConditions = append(firedConditions, annotated)
		} else {
			notFired = append(notFired, annotated)
		}
	}

	// Logic: 'any' = OR (at least one), 'all' = AND (all must match)
	shouldFire := false
	if logic == "all" {
		shouldFire = len(conditions) > 0 && len(notFired) == 0
	} else {
		shouldFire = len(firedConditions) > 0
	}
	if !shouldFire {
		return map[string]any{"rule_id": ruleId, "fired": false, "reason": "conditions not met"}
	}

	defaultPayload := buildNotificationPayload(rule, firedConditions, true)

	// Dispatch to each configured channel
	channelIds, _ := rule["channel_ids"].([]string)
	dispatchResults := []map[string]any{}
	for _, chId := range channelIds {
		channel := channelsById[chId]
		if channel == nil {
			dispatchResults = append(dispatchResults, map[string]any{
				"channel_id": chId,
				"status":     "error",
				"error":      "channel not found",
				"summary":    rowString(defaultPayload["summary"]),
			})
			continue
		}
		chEnabled, _ := channel["enabled"].(bool)
		if !chEnabled {
			dispatchResults = append(dispatchResults, map[string]any{
				"channel_id": chId,
				"status":     "skipped",
				"error":      "channel disabled",
				"summary":    rowString(defaultPayload["summary"]),
			})
			continue
		}
		maskOutputEnabled := notificationChannelMaskOutputEnabled(channel)
		payload := buildNotificationPayload(rule, firedConditions, maskOutputEnabled)
		status := dispatchNotificationChannel(channel, payload)
		statusStr := "error"
		errStr := status
		if status == "ok" {
			statusStr = "ok"
			errStr = ""
		}
		dispatchResults = append(dispatchResults, map[string]any{
			"channel_id":   chId,
			"channel_name": rowString(channel["name"]),
			"status":       statusStr,
			"error":        errStr,
			"summary":      rowString(payload["summary"]),
		})
	}

	// Write notification log entries
	for _, dr := range dispatchResults {
		insertRowsJsonEachRow(db, "sobs_notification_log", []Row{{
			"Id":           agentUuid4(),
			"RuleId":       ruleId,
			"RuleName":     rule["name"],
			"ChannelId":    rowString(dr["channel_id"]),
			"ChannelName":  rowString(dr["channel_name"]),
			"FiredAt":      time.Now().UTC().Format("2006-01-02 15:04:05.000"),
			"Status":       rowStringOr(dr["status"], "error"),
			"ErrorMessage": rowString(dr["error"]),
			"Summary":      rowString(dr["summary"]),
		}})
	}

	// Update LastFiredAt on rule
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	condBytes, _ := json.Marshal(rule["conditions"])
	insertRowsJsonEachRow(db, "sobs_notification_rules", []Row{{
		"Id":              ruleId,
		"Name":            rule["name"],
		"Enabled":         enabledInt,
		"LogicOperator":   rowStringOr(rule["logic_operator"], "any"),
		"ConditionsJson":  string(condBytes),
		"ChannelIds":      strings.Join(channelIds, ","),
		"Severity":        rowStringOr(rule["severity"], "warning"),
		"CooldownSeconds": cooldown,
		"LastFiredAt":     time.Now().UTC().Format("2006-01-02 15:04:05.000"),
		"IsDeleted":       0,
		"Version":         time.Now().UnixMilli(),
	}})

	// Register a raw preservation window around this signal
	func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Debug(fmt.Sprintf("failed to register raw window for notification rule %s", ruleId))
			}
		}()
		registerRawWindow(db, time.Now().UTC(), "notification", ruleId, "", "", "")
	}()

	return map[string]any{
		"rule_id":          ruleId,
		"rule_name":        rule["name"],
		"fired":            true,
		"summary":          rowString(defaultPayload["summary"]),
		"dispatch_results": dispatchResults,
	}
}

// ---------------------------------------------------------------------------
// Agent rule trigger collection
// ---------------------------------------------------------------------------

func normalizeAgentTriggerState(rawState string) string {
	state := strings.ToLower(strings.TrimSpace(rawState))
	if state == "outlier" {
		return "critical"
	}
	if state == "warning" || state == "critical" {
		return state
	}
	return "normal"
}

func agentRuleTriggerStateMatches(triggerState, eventState string) bool {
	requested := triggerState
	if requested == "" {
		requested = "any"
	}
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "any" {
		return eventState == "warning" || eventState == "critical"
	}
	return requested == eventState
}

func collectAnomalyAgentEvents(db *ChDbConnection) map[string]map[string]any {
	res, err := db.Execute(
		"SELECT ServiceName, SignalSource, SignalName, AttrFingerprint, " +
			"argMax(value, time) AS value, argMax(SampleCount, time) AS SampleCount, " +
			"argMax(time, time) AS latest_time " +
			"FROM v_derived_signals_anomaly " +
			"WHERE time >= now() - INTERVAL 24 HOUR " +
			"GROUP BY ServiceName, SignalSource, SignalName, AttrFingerprint")
	if err != nil {
		return map[string]map[string]any{}
	}
	rows := res.Fetchall()
	if len(rows) == 0 {
		return map[string]map[string]any{}
	}

	annotated := make([]Row, 0, len(rows))
	for _, r := range rows {
		copyRow := Row{}
		for k, v := range r {
			copyRow[k] = v
		}
		annotated = append(annotated, copyRow)
	}
	anomalyRules, _ := loadAnomalyRules(db)
	annotateRowsWithRules(annotated, anomalyRules,
		"SignalSource", "SignalName", "ServiceName", "AttrFingerprint",
		"value", "SampleCount", "latest_time")

	eventsByRule := map[string]map[string]any{}
	severityRank := map[string]int{"warning": 1, "critical": 2}
	for _, row := range annotated {
		ruleId := strings.TrimSpace(rowString(row["rule_id"]))
		if ruleId == "" {
			continue
		}
		state := normalizeAgentTriggerState(rowStringOr(row["effective_state"], "normal"))
		if _, ok := severityRank[state]; !ok {
			continue
		}
		event := map[string]any{
			"state":   state,
			"service": rowString(row["ServiceName"]),
			"source":  rowString(row["SignalSource"]),
			"signal":  rowString(row["SignalName"]),
			"value":   row["value"],
		}
		current := eventsByRule[ruleId]
		if current == nil || severityRank[state] > severityRank[rowStringOr(current["state"], "normal")] {
			eventsByRule[ruleId] = event
		}
	}
	return eventsByRule
}

func collectTagRuleAgentEvents(db *ChDbConnection, lookbackMinutes int) map[string]map[string]any {
	tagRules, _ := loadTagRules(db)
	if len(tagRules) == 0 {
		return map[string]map[string]any{}
	}
	type tagPair struct{ k, v string }
	lookup := map[tagPair]map[string]any{}
	for _, rule := range tagRules {
		lookup[tagPair{rowString(rule["tag_key"]), rowString(rule["tag_value"])}] = rule
	}
	minVersion := int64((float64(time.Now().UnixNano())/1e9 - float64(lookbackMinutes*60)) * 1000)
	res, err := db.Execute(
		"SELECT TagKey, TagValue, count() AS c FROM sobs_record_tags FINAL "+
			"WHERE IsDeleted = 0 AND IsAuto = 1 AND Version >= ? "+
			"GROUP BY TagKey, TagValue",
		minVersion,
	)
	if err != nil {
		return map[string]map[string]any{}
	}
	events := map[string]map[string]any{}
	for _, row := range res.Fetchall() {
		key := tagPair{rowString(row["TagKey"]), rowString(row["TagValue"])}
		rule, ok := lookup[key]
		if !ok {
			continue
		}
		ruleId := rowString(rule["id"])
		matches := 0
		if v, ok := coerceIntStrict(row["c"]); ok {
			matches = v
		}
		events[ruleId] = map[string]any{
			"state":     "warning",
			"tag_key":   key.k,
			"tag_value": key.v,
			"matches":   matches,
		}
	}
	return events
}

// runAgentRuleInstance records a pending agent run, executes the agent flow,
// and records the outcome.
func runAgentRuleInstance(db *ChDbConnection, rule map[string]any, settings map[string]string, triggerContext map[string]any) map[string]any {
	runId := agentUuid4()
	nowTs := normalizeChTimestamp(time.Now().UTC())
	tcBytes, _ := json.Marshal(triggerContext)
	insertRowsJsonEachRow(db, "sobs_agent_runs", []Row{{
		"Id":             runId,
		"RuleId":         rule["id"],
		"RuleName":       rule["name"],
		"TriggerContext": string(tcBytes),
		"Status":         "pending",
		"GuardDecision":  "",
		"DlpResult":      "",
		"Analysis":       "",
		"Suggestion":     "",
		"GithubIssueUrl": "",
		"ErrorMessage":   "",
		"CreatedAt":      nowTs,
		"CompletedAt":    nowTs,
		"IsDismissed":    0,
		"IsDeleted":      0,
		"Version":        time.Now().UnixMilli(),
	}})

	result, err := runAgentFlow(db, rule, settings, triggerContext, runId)
	if err != nil {
		logger.Error(fmt.Sprintf("agent flow error: %v", err))
		errorMsg := err.Error()
		insertRowsJsonEachRow(db, "sobs_agent_runs", []Row{{
			"Id":             runId,
			"RuleId":         rule["id"],
			"RuleName":       rule["name"],
			"TriggerContext": string(tcBytes),
			"Status":         "failed",
			"GuardDecision":  "",
			"DlpResult":      "",
			"Analysis":       "",
			"Suggestion":     "",
			"GithubIssueUrl": "",
			"ErrorMessage":   errorMsg,
			"CreatedAt":      nowTs,
			"CompletedAt":    normalizeChTimestamp(time.Now().UTC()),
			"IsDismissed":    0,
			"IsDeleted":      0,
			"Version":        time.Now().UnixMilli(),
		}})
		return map[string]any{"ok": false, "rule_id": rule["id"], "run_id": runId, "error": errorMsg}
	}
	return map[string]any{"ok": true, "rule_id": rule["id"], "run_id": runId, "result": result}
}

// generateVapidKeys generates a new VAPID key pair. Returns
// (private_key_b64url, public_key_b64url).
func generateVapidKeys() (string, string, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	privateBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return "", "", err
	}
	publicBytes, err := ecdsaPublicUncompressed(privateKey)
	if err != nil {
		return "", "", err
	}
	privateB64 := base64.RawURLEncoding.EncodeToString(privateBytes)
	publicB64 := base64.RawURLEncoding.EncodeToString(publicBytes)
	return privateB64, publicB64, nil
}

// ---------------------------------------------------------------------------
// Routes
// ---------------------------------------------------------------------------

// viewNotifications renders the notification channels and rules management page.
func viewNotifications(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	channels := loadNotificationChannels(db)
	rules := loadNotificationRules(db)
	editRuleId := strings.TrimSpace(r.URL.Query().Get("edit_rule"))
	var editRule map[string]any
	for _, rule := range rules {
		if rowString(rule["id"]) == editRuleId {
			editRule = rule
			break
		}
	}
	notificationLog := loadNotificationLog(db, 50)
	vapidPublicKey, vapidKeySource := getVapidPublicKey(db)
	metricRules, _ := loadAnomalyRules(db)
	renderTemplate(w, r, "settings_notifications.html", map[string]any{
		"channels":            channels,
		"rules":               rules,
		"notification_log":    notificationLog,
		"channel_types":       notificationChannelTypes,
		"comparators":         notificationComparators,
		"condition_types":     notificationConditionTypes,
		"severities":          notificationSeverities,
		"logic_operators":     notificationLogicOperators,
		"signal_sources":      notificationSignalSources,
		"tag_match_operators": notificationTagMatchOperators,
		"tag_record_types":    notificationTagRecordTypes,
		"edit_rule":           editRule,
		"vapid_public_key":    vapidPublicKey,
		"vapid_key_source":    vapidKeySource,
		"metric_rules":        metricRules,
	})
}

// createNotificationChannel creates a new notification channel.
func createNotificationChannel(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	channelType := strings.ToLower(strings.TrimSpace(r.FormValue("channel_type")))
	maskOutputValues := r.Form["mask_output_enabled"]
	maskOutputEnabled := false
	for _, value := range maskOutputValues {
		if isTruthySetting(value, false) {
			maskOutputEnabled = true
			break
		}
	}
	if len(maskOutputValues) == 0 {
		maskOutputEnabled = true
	}

	redirectToView := func() {
		http.Redirect(w, r, "/settings/notifications", http.StatusFound)
	}

	if name == "" {
		flashMessage(w, r, "Channel name is required", "warning")
		redirectToView()
		return
	}
	if !notifContains(notificationChannelTypes, channelType) {
		flashMessage(w, r, fmt.Sprintf("Invalid channel type: %s", channelType), "warning")
		redirectToView()
		return
	}

	// Build config dict from form fields for the selected channel type.
	config := map[string]any{}
	switch channelType {
	case "webhook":
		config["url"] = strings.TrimSpace(r.FormValue("webhook_url"))
		method := r.FormValue("webhook_method")
		if method == "" {
			method = "POST"
		}
		config["method"] = strings.ToUpper(strings.TrimSpace(method))
		headers := r.FormValue("webhook_headers")
		if headers == "" {
			headers = "{}"
		}
		config["headers"] = strings.TrimSpace(headers)
		config["body_template"] = strings.TrimSpace(r.FormValue("webhook_body_template"))
		if rowString(config["url"]) == "" {
			flashMessage(w, r, "Webhook URL is required", "warning")
			redirectToView()
			return
		}
	case "slack":
		config["webhook_url"] = strings.TrimSpace(r.FormValue("slack_webhook_url"))
		if rowString(config["webhook_url"]) == "" {
			flashMessage(w, r, "Slack webhook URL is required", "warning")
			redirectToView()
			return
		}
	case "email":
		host := r.FormValue("smtp_host")
		if host == "" {
			host = "localhost"
		}
		config["smtp_host"] = strings.TrimSpace(host)
		port := r.FormValue("smtp_port")
		if port == "" {
			port = "587"
		}
		config["smtp_port"] = strings.TrimSpace(port)
		config["smtp_user"] = strings.TrimSpace(r.FormValue("smtp_user"))
		config["smtp_password"] = strings.TrimSpace(r.FormValue("smtp_password"))
		fromAddr := r.FormValue("from_addr")
		if fromAddr == "" {
			fromAddr = "sobs@localhost"
		}
		config["from_addr"] = strings.TrimSpace(fromAddr)
		config["to_addr"] = strings.TrimSpace(r.FormValue("to_addr"))
		useTls := r.FormValue("use_tls")
		if useTls == "" {
			useTls = "1"
		}
		config["use_tls"] = strings.TrimSpace(useTls)
		if rowString(config["to_addr"]) == "" {
			flashMessage(w, r, "Email recipient (to_addr) is required", "warning")
			redirectToView()
			return
		}
	case "browser_push":
		config["endpoint"] = strings.TrimSpace(r.FormValue("push_endpoint"))
		config["p256dh"] = strings.TrimSpace(r.FormValue("push_p256dh"))
		config["auth"] = strings.TrimSpace(r.FormValue("push_auth"))
		if rowString(config["endpoint"]) == "" {
			flashMessage(w, r, "Push endpoint is required", "warning")
			redirectToView()
			return
		}
	}

	if maskOutputEnabled {
		config["mask_output_enabled"] = "1"
	} else {
		config["mask_output_enabled"] = "0"
	}

	channelId := agentUuid4()
	storedConfig := encryptNotificationConfig(config)
	configBytes, _ := json.Marshal(storedConfig)
	insertRowsJsonEachRow(getDb(), "sobs_notification_channels", []Row{{
		"Id":          channelId,
		"Name":        name,
		"ChannelType": channelType,
		"ConfigJson":  string(configBytes),
		"Enabled":     1,
		"IsDeleted":   0,
		"Version":     time.Now().UnixMilli(),
	}})
	flashMessage(w, r, fmt.Sprintf("Notification channel '%s' created", name), "success")
	redirectToView()
}

// deleteNotificationChannel soft-deletes a notification channel.
func deleteNotificationChannel(w http.ResponseWriter, r *http.Request) {
	channelId := r.PathValue("channel_id")
	db := getDb()
	deletedRow := func(row Row) Row {
		return Row{
			"Id":          channelId,
			"Name":        rowString(row["Name"]),
			"ChannelType": rowString(row["ChannelType"]),
			"ConfigJson":  rowString(row["ConfigJson"]),
			"Enabled":     coerceInt(row["Enabled"]),
		}
	}
	softDeleteLatestRow(
		w, r, db,
		"SELECT Id, Name, ChannelType, ConfigJson, Enabled "+
			"FROM sobs_notification_channels FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
		[]any{channelId},
		"sobs_notification_channels",
		deletedRow,
		"Notification channel not found",
		"Notification channel '{name}' deleted",
		"view_notifications",
		"warning",
		"success",
	)
}

// toggleNotificationChannel toggles the enabled/disabled state of a channel.
func toggleNotificationChannel(w http.ResponseWriter, r *http.Request) {
	channelId := r.PathValue("channel_id")
	db := getDb()
	res, err := db.Execute(
		"SELECT Id, Name, ChannelType, ConfigJson, Enabled "+
			"FROM sobs_notification_channels FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
		channelId,
	)
	var row Row
	if err == nil {
		row = res.Fetchone()
	}
	if row == nil {
		flashMessage(w, r, "Notification channel not found", "warning")
		http.Redirect(w, r, "/settings/notifications", http.StatusFound)
		return
	}
	newEnabled := 1
	if coerceInt(row["Enabled"]) != 0 {
		newEnabled = 0
	}
	insertRowsJsonEachRow(db, "sobs_notification_channels", []Row{{
		"Id":          channelId,
		"Name":        rowString(row["Name"]),
		"ChannelType": rowString(row["ChannelType"]),
		"ConfigJson":  rowString(row["ConfigJson"]),
		"Enabled":     newEnabled,
		"IsDeleted":   0,
		"Version":     time.Now().UnixMilli(),
	}})
	state := "disabled"
	if newEnabled != 0 {
		state = "enabled"
	}
	flashMessage(w, r, fmt.Sprintf("Notification channel '%s' %s", rowString(row["Name"]), state), "success")
	http.Redirect(w, r, "/settings/notifications", http.StatusFound)
}

// testNotificationChannel sends a test notification through the given channel.
func testNotificationChannel(w http.ResponseWriter, r *http.Request) {
	channelId := r.PathValue("channel_id")
	db := getDb()
	res, err := db.Execute(
		"SELECT Id, Name, ChannelType, ConfigJson, Enabled "+
			"FROM sobs_notification_channels FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
		channelId,
	)
	var row Row
	if err == nil {
		row = res.Fetchone()
	}
	if row == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "channel not found"})
		return
	}
	channel := map[string]any{
		"id":           rowString(row["Id"]),
		"name":         rowString(row["Name"]),
		"channel_type": rowString(row["ChannelType"]),
		"config":       decryptNotificationConfig(parseJsonObject(rowString(row["ConfigJson"]))),
		"enabled":      coerceInt(row["Enabled"]) != 0,
	}
	baseSummary := fmt.Sprintf("[SOBS] Test notification from channel '%s'", rowString(channel["name"]))
	summary := baseSummary
	if notificationChannelMaskOutputEnabled(channel) {
		summary = maskStringForOutput(baseSummary, nil)
	}
	testPayload := map[string]any{
		"rule_name":  "Test",
		"severity":   "info",
		"conditions": []any{},
		"summary":    summary,
		"fired_at":   pyIsoFormat(time.Now().UTC()),
	}
	result := dispatchNotificationChannel(channel, testPayload)
	if result == "ok" {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": result})
}

// createNotificationRule creates or updates a notification rule.
func createNotificationRule(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	editRuleId := strings.TrimSpace(r.FormValue("edit_rule_id"))
	name := strings.TrimSpace(r.FormValue("name"))
	logicOperator := strings.ToLower(strings.TrimSpace(rowStringOr(r.FormValue("logic_operator"), "any")))
	severity := strings.ToLower(strings.TrimSpace(rowStringOr(r.FormValue("severity"), "warning")))
	cooldownSeconds := 300
	if raw := strings.TrimSpace(r.FormValue("cooldown_seconds")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			cooldownSeconds = v
		}
	}
	if cooldownSeconds < 0 {
		cooldownSeconds = 0
	}
	if cooldownSeconds > 86400 {
		cooldownSeconds = 86400
	}
	channelIdsRaw := r.Form["channel_ids"]

	// Parse conditions from repeated form fields.
	sources := r.Form["cond_source"]
	signals := r.Form["cond_signal"]
	services := r.Form["cond_service"]
	conditionTypes := r.Form["cond_type"]
	recordTypes := r.Form["cond_record_type"]
	tagKeys := r.Form["cond_tag_key"]
	tagMatchOperators := r.Form["cond_tag_match_operator"]
	tagValues := r.Form["cond_tag_value"]
	comparators := r.Form["cond_comparator"]
	thresholds := r.Form["cond_threshold"]
	windows := r.Form["cond_window_minutes"]

	redirectToView := func() {
		http.Redirect(w, r, "/settings/notifications", http.StatusFound)
	}

	if name == "" {
		flashMessage(w, r, "Rule name is required", "warning")
		redirectToView()
		return
	}
	if !notifContains(notificationLogicOperators, logicOperator) {
		flashMessage(w, r, fmt.Sprintf("Invalid logic operator: %s", logicOperator), "warning")
		redirectToView()
		return
	}
	if !notifContains(notificationSeverities, severity) {
		flashMessage(w, r, fmt.Sprintf("Invalid severity: %s", severity), "warning")
		redirectToView()
		return
	}

	conditions := []map[string]any{}
	rowCount := 0
	for _, s := range [][]string{conditionTypes, sources, signals, services, recordTypes, tagKeys, tagMatchOperators, tagValues, comparators, thresholds, windows} {
		if len(s) > rowCount {
			rowCount = len(s)
		}
	}
	atIdx := func(s []string, i int, def string) string {
		if i < len(s) {
			return s[i]
		}
		return def
	}
	for i := 0; i < rowCount; i++ {
		conditionType := strings.ToLower(strings.TrimSpace(atIdx(conditionTypes, i, "signal")))
		if !notifContains(notificationConditionTypes, conditionType) {
			flashMessage(w, r, fmt.Sprintf("Invalid notification condition type: %s", conditionType), "warning")
			redirectToView()
			return
		}
		comparator := strings.ToLower(strings.TrimSpace(atIdx(comparators, i, "gt")))
		threshold := 0.0
		if v, err := strconv.ParseFloat(strings.TrimSpace(atIdx(thresholds, i, "0")), 64); err == nil {
			threshold = v
		}
		windowMinutes := 5
		if v, err := strconv.Atoi(strings.TrimSpace(atIdx(windows, i, "5"))); err == nil {
			windowMinutes = v
		}
		if windowMinutes < 1 {
			windowMinutes = 1
		}
		if windowMinutes > 60 {
			windowMinutes = 60
		}
		if !notifContains(notificationComparators, comparator) {
			comparator = "gt"
		}
		if conditionType == "tag" {
			recordType := strings.ToLower(strings.TrimSpace(atIdx(recordTypes, i, "all")))
			tagKey := strings.TrimSpace(atIdx(tagKeys, i, ""))
			tagMatchOperator := strings.ToLower(strings.TrimSpace(atIdx(tagMatchOperators, i, "eq")))
			tagValue := strings.TrimSpace(atIdx(tagValues, i, ""))
			if tagKey == "" {
				continue
			}
			if !notifContains(notificationTagRecordTypes, recordType) {
				recordType = "all"
			}
			if !notifContains(notificationTagMatchOperators, tagMatchOperator) {
				tagMatchOperator = "eq"
			}
			if tagMatchOperator == "regex" {
				if _, err := regexp.Compile(tagValue); err != nil {
					flashMessage(w, r, fmt.Sprintf("Invalid tag regex pattern: %s", err), "warning")
					if editRuleId != "" {
						http.Redirect(w, r, "/settings/notifications?edit_rule="+url.QueryEscape(editRuleId), http.StatusFound)
					} else {
						http.Redirect(w, r, "/settings/notifications", http.StatusFound)
					}
					return
				}
			}
			conditions = append(conditions, map[string]any{
				"type":               "tag",
				"record_type":        recordType,
				"tag_key":            tagKey,
				"tag_match_operator": tagMatchOperator,
				"tag_value":          tagValue,
				"comparator":         comparator,
				"threshold":          threshold,
				"window_minutes":     windowMinutes,
			})
			continue
		}
		source := strings.TrimSpace(atIdx(sources, i, ""))
		signal := strings.TrimSpace(atIdx(signals, i, ""))
		service := strings.TrimSpace(atIdx(services, i, ""))
		if source == "" || signal == "" {
			continue
		}
		conditions = append(conditions, map[string]any{
			"type":           "signal",
			"source":         source,
			"signal":         signal,
			"service":        service,
			"comparator":     comparator,
			"threshold":      threshold,
			"window_minutes": windowMinutes,
		})
	}

	if len(conditions) == 0 {
		flashMessage(w, r, "At least one condition is required", "warning")
		redirectToView()
		return
	}

	// Validate channel IDs exist.
	db := getDb()
	validChannelIds := map[string]bool{}
	if res, err := db.Execute("SELECT Id FROM sobs_notification_channels FINAL WHERE IsDeleted = 0"); err == nil {
		for _, row := range res.Fetchall() {
			validChannelIds[rowString(row["Id"])] = true
		}
	}
	channelIds := []string{}
	for _, c := range channelIdsRaw {
		c = strings.TrimSpace(c)
		if validChannelIds[c] {
			channelIds = append(channelIds, c)
		}
	}

	enabled := 1
	lastFiredAt := "1970-01-01 00:00:00.000"
	ruleId := agentUuid4()
	if editRuleId != "" {
		res, err := db.Execute(
			"SELECT Id, Enabled, LastFiredAt FROM sobs_notification_rules FINAL "+
				"WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
			editRuleId,
		)
		var existing Row
		if err == nil {
			existing = res.Fetchone()
		}
		if existing == nil {
			flashMessage(w, r, "Notification rule not found for editing", "warning")
			redirectToView()
			return
		}
		ruleId = rowString(existing["Id"])
		enabled = coerceInt(existing["Enabled"])
		lastFiredAt = rowString(existing["LastFiredAt"])
	}

	condBytes, _ := json.Marshal(conditions)
	insertRowsJsonEachRow(db, "sobs_notification_rules", []Row{{
		"Id":              ruleId,
		"Name":            name,
		"Enabled":         enabled,
		"LogicOperator":   logicOperator,
		"ConditionsJson":  string(condBytes),
		"ChannelIds":      strings.Join(channelIds, ","),
		"Severity":        severity,
		"CooldownSeconds": cooldownSeconds,
		"LastFiredAt":     lastFiredAt,
		"IsDeleted":       0,
		"Version":         time.Now().UnixMilli(),
	}})
	action := "created"
	if editRuleId != "" {
		action = "updated"
	}
	flashMessage(w, r, fmt.Sprintf("Notification rule '%s' %s", name, action), "success")
	redirectToView()
}

// toggleNotificationRule toggles the enabled/disabled state of a rule.
func toggleNotificationRule(w http.ResponseWriter, r *http.Request) {
	ruleId := r.PathValue("rule_id")
	db := getDb()
	res, err := db.Execute(
		"SELECT Id, Name, Enabled, LogicOperator, ConditionsJson, ChannelIds, "+
			"Severity, CooldownSeconds "+
			"FROM sobs_notification_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
		ruleId,
	)
	var row Row
	if err == nil {
		row = res.Fetchone()
	}
	if row == nil {
		flashMessage(w, r, "Notification rule not found", "warning")
		http.Redirect(w, r, "/settings/notifications", http.StatusFound)
		return
	}
	newEnabled := 1
	if coerceInt(row["Enabled"]) != 0 {
		newEnabled = 0
	}
	insertRowsJsonEachRow(db, "sobs_notification_rules", []Row{{
		"Id":              ruleId,
		"Name":            rowString(row["Name"]),
		"Enabled":         newEnabled,
		"LogicOperator":   rowString(row["LogicOperator"]),
		"ConditionsJson":  rowString(row["ConditionsJson"]),
		"ChannelIds":      rowString(row["ChannelIds"]),
		"Severity":        rowString(row["Severity"]),
		"CooldownSeconds": coerceInt(row["CooldownSeconds"]),
		"LastFiredAt":     "1970-01-01 00:00:00.000",
		"IsDeleted":       0,
		"Version":         time.Now().UnixMilli(),
	}})
	state := "disabled"
	if newEnabled != 0 {
		state = "enabled"
	}
	flashMessage(w, r, fmt.Sprintf("Notification rule '%s' %s", rowString(row["Name"]), state), "success")
	http.Redirect(w, r, "/settings/notifications", http.StatusFound)
}

// deleteNotificationRule soft-deletes a notification rule.
func deleteNotificationRule(w http.ResponseWriter, r *http.Request) {
	ruleId := r.PathValue("rule_id")
	db := getDb()
	deletedRow := func(row Row) Row {
		return Row{
			"Id":              ruleId,
			"Name":            rowString(row["Name"]),
			"Enabled":         coerceInt(row["Enabled"]),
			"LogicOperator":   rowString(row["LogicOperator"]),
			"ConditionsJson":  rowString(row["ConditionsJson"]),
			"ChannelIds":      rowString(row["ChannelIds"]),
			"Severity":        rowString(row["Severity"]),
			"CooldownSeconds": coerceInt(row["CooldownSeconds"]),
			"LastFiredAt":     "1970-01-01 00:00:00.000",
		}
	}
	softDeleteLatestRow(
		w, r, db,
		"SELECT Id, Name, LogicOperator, ConditionsJson, ChannelIds, Severity, CooldownSeconds, Enabled "+
			"FROM sobs_notification_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
		[]any{ruleId},
		"sobs_notification_rules",
		deletedRow,
		"Notification rule not found",
		"Notification rule '{name}' deleted",
		"view_notifications",
		"warning",
		"success",
	)
}

// getNotificationAutoCandidates returns auto-generate candidates derived from
// active metric rules, skipping (source, signal) pairs already covered by an
// existing notification rule. metricRuleId == "" processes all rules.
func getNotificationAutoCandidates(db *ChDbConnection, metricRuleId string) map[string]any {
	var rows []Row
	if metricRuleId != "" {
		if res, err := db.Execute(
			"SELECT Id, Name, SignalSource, SignalName, ServiceName, Comparator, "+
				"WarningThreshold, CriticalThreshold "+
				"FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 AND Id = ? LIMIT 1",
			metricRuleId,
		); err == nil {
			rows = res.Fetchall()
		}
	} else {
		if res, err := db.Execute(
			"SELECT Id, Name, SignalSource, SignalName, ServiceName, Comparator, " +
				"WarningThreshold, CriticalThreshold " +
				"FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 ORDER BY Name",
		); err == nil {
			rows = res.Fetchall()
		}
	}
	metricRules := []map[string]any{}
	for _, mrow := range rows {
		warn, _ := coerceFloat(mrow["WarningThreshold"])
		crit, _ := coerceFloat(mrow["CriticalThreshold"])
		metricRules = append(metricRules, map[string]any{
			"id":                 rowString(mrow["Id"]),
			"name":               rowString(mrow["Name"]),
			"source":             rowString(mrow["SignalSource"]),
			"signal":             rowString(mrow["SignalName"]),
			"service":            rowString(mrow["ServiceName"]),
			"comparator":         rowString(mrow["Comparator"]),
			"warning_threshold":  warn,
			"critical_threshold": crit,
		})
	}

	type srcSig struct{ source, signal string }
	covered := map[srcSig]bool{}
	for _, nr := range loadNotificationRules(db) {
		conds, _ := nr["conditions"].([]map[string]any)
		for _, cond := range conds {
			covered[srcSig{rowString(cond["source"]), rowString(cond["signal"])}] = true
		}
	}

	allChannelIds := []string{}
	channelNames := map[string]string{}
	if res, err := db.Execute(
		"SELECT Id, Name FROM sobs_notification_channels FINAL WHERE IsDeleted = 0 AND Enabled = 1",
	); err == nil {
		for _, crow := range res.Fetchall() {
			id := rowString(crow["Id"])
			allChannelIds = append(allChannelIds, id)
			channelNames[id] = rowString(crow["Name"])
		}
	}

	candidates := []map[string]any{}
	skipped := 0
	for _, mr := range metricRules {
		key := srcSig{rowString(mr["source"]), rowString(mr["signal"])}
		if covered[key] {
			skipped++
			continue
		}
		crit, _ := coerceFloat(mr["critical_threshold"])
		warn, _ := coerceFloat(mr["warning_threshold"])
		threshold := 0.0
		severity := "warning"
		if crit > 0 {
			threshold = crit
			severity = "critical"
		} else if warn > 0 {
			threshold = warn
			severity = "warning"
		}
		channelNamesList := []string{}
		for _, cid := range allChannelIds {
			if n, ok := channelNames[cid]; ok {
				channelNamesList = append(channelNamesList, n)
			} else {
				channelNamesList = append(channelNamesList, cid)
			}
		}
		candidates = append(candidates, map[string]any{
			"metric_rule_id": rowString(mr["id"]),
			"name":           fmt.Sprintf("Auto: %s", rowString(mr["name"])),
			"source":         rowString(mr["source"]),
			"signal":         rowString(mr["signal"]),
			"service":        rowString(mr["service"]),
			"comparator":     rowString(mr["comparator"]),
			"threshold":      threshold,
			"severity":       severity,
			"channel_ids":    allChannelIds,
			"channel_names":  channelNamesList,
		})
	}
	return map[string]any{
		"examined":   len(metricRules),
		"skipped":    skipped,
		"candidates": candidates,
	}
}

// autoGenerateNotificationRules previews or creates notification rules
// auto-generated from active metric rules.
func autoGenerateNotificationRules(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	action := strings.ToLower(strings.TrimSpace(rowStringOr(r.FormValue("action"), "preview")))
	metricRuleId := strings.TrimSpace(r.FormValue("metric_rule_id"))

	db := getDb()
	result := getNotificationAutoCandidates(db, metricRuleId)
	candidates, _ := result["candidates"].([]map[string]any)

	if action == "create" {
		// Re-derive the covered set to guard against races between preview/create.
		type srcSig struct{ source, signal string }
		coveredNow := map[srcSig]bool{}
		for _, nr := range loadNotificationRules(db) {
			conds, _ := nr["conditions"].([]map[string]any)
			for _, cond := range conds {
				coveredNow[srcSig{rowString(cond["source"]), rowString(cond["signal"])}] = true
			}
		}
		created := 0
		skipped := coerceInt(result["skipped"])
		for _, cand := range candidates {
			key := srcSig{rowString(cand["source"]), rowString(cand["signal"])}
			if coveredNow[key] {
				skipped++
				continue
			}
			coveredNow[key] = true // prevent duplicates within this batch
			threshold, _ := coerceFloat(cand["threshold"])
			conditions := []map[string]any{{
				"source":         rowString(cand["source"]),
				"signal":         rowString(cand["signal"]),
				"service":        rowString(cand["service"]),
				"comparator":     rowString(cand["comparator"]),
				"threshold":      threshold,
				"window_minutes": 5,
			}}
			condBytes, _ := json.Marshal(conditions)
			channelIds, _ := cand["channel_ids"].([]string)
			insertRowsJsonEachRow(db, "sobs_notification_rules", []Row{{
				"Id":              agentUuid4(),
				"Name":            rowString(cand["name"]),
				"Enabled":         1,
				"LogicOperator":   "any",
				"ConditionsJson":  string(condBytes),
				"ChannelIds":      strings.Join(channelIds, ","),
				"Severity":        rowString(cand["severity"]),
				"CooldownSeconds": 300,
				"LastFiredAt":     "1970-01-01 00:00:00.000",
				"IsDeleted":       0,
				"Version":         time.Now().UnixMilli(),
			}})
			created++
		}
		jsonResponse(w, http.StatusOK, map[string]any{
			"ok":       true,
			"created":  created,
			"skipped":  skipped,
			"examined": result["examined"],
		})
		return
	}

	// action == "preview"
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":         true,
		"examined":   result["examined"],
		"skipped":    result["skipped"],
		"candidates": candidates,
	})
}

// checkNotifications evaluates all enabled notification rules (and automatic
// agent rule triggers) and fires any that match.
func checkNotifications(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	rules := loadNotificationRules(db)
	channels := loadNotificationChannels(db)
	channelsById := map[string]map[string]any{}
	for _, c := range channels {
		channelsById[rowString(c["id"])] = c
	}

	results := []map[string]any{}
	for _, rule := range rules {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error(fmt.Sprintf("Error evaluating notification rule %s", rowString(rule["id"])))
					results = append(results, map[string]any{"rule_id": rule["id"], "fired": false, "error": "rule evaluation failed"})
				}
			}()
			results = append(results, checkNotificationRule(db, rule, channelsById))
		}()
	}

	fired := 0
	for _, res := range results {
		if b, _ := res["fired"].(bool); b {
			fired++
		}
	}

	// Also evaluate automatic agent rule triggers from anomaly/tag events.
	agentResults := []any{}
	settings := loadAllAiSettings(db)
	if settings["ai.endpoint_url"] != "" && settings["ai.model"] != "" {
		anomalyEvents := collectAnomalyAgentEvents(db)
		tagEvents := collectTagRuleAgentEvents(db, 5)
		allAnomalyEvents := []map[string]any{}
		for _, e := range anomalyEvents {
			allAnomalyEvents = append(allAnomalyEvents, e)
		}
		allTagEvents := []map[string]any{}
		for _, e := range tagEvents {
			allTagEvents = append(allTagEvents, e)
		}

		for _, agentRule := range loadAgentRules(db) {
			isEnabled, _ := agentRule["is_enabled"].(bool)
			if !isEnabled {
				continue
			}
			triggerType := strings.ToLower(strings.TrimSpace(rowString(agentRule["trigger_type"])))
			triggerRefId := strings.TrimSpace(rowString(agentRule["trigger_ref_id"]))
			triggerState := strings.ToLower(strings.TrimSpace(rowStringOr(agentRule["trigger_state"], "any")))

			var event map[string]any
			if triggerType == "anomaly_rule" {
				if triggerRefId != "" {
					event = anomalyEvents[triggerRefId]
				} else if len(allAnomalyEvents) > 0 {
					// Pick the most severe (critical > warning); first wins on ties.
					event = allAnomalyEvents[0]
					bestRank := 1
					if rowString(event["state"]) == "critical" {
						bestRank = 2
					}
					for _, e := range allAnomalyEvents[1:] {
						rank := 1
						if rowString(e["state"]) == "critical" {
							rank = 2
						}
						if rank > bestRank {
							bestRank = rank
							event = e
						}
					}
				}
			} else if triggerType == "tag_rule" {
				if triggerRefId != "" {
					event = tagEvents[triggerRefId]
				} else if len(allTagEvents) > 0 {
					event = allTagEvents[0]
				}
			} else {
				continue
			}

			if event == nil {
				continue
			}

			eventState := normalizeAgentTriggerState(rowStringOr(event["state"], "normal"))
			if !agentRuleTriggerStateMatches(triggerState, eventState) {
				continue
			}

			rateLimitMinutes := 60
			if v, ok := coerceIntStrict(agentRule["rate_limit_minutes"]); ok && v != 0 {
				rateLimitMinutes = v
			}
			lastRunTs := agentRuleLastRunTs(db, rowString(agentRule["id"]))
			elapsedMinutes := (float64(time.Now().UnixNano())/1e9 - lastRunTs) / 60.0
			if elapsedMinutes < float64(rateLimitMinutes) && lastRunTs > 0 {
				agentResults = append(agentResults, map[string]any{
					"rule_id":         agentRule["id"],
					"status":          "skipped_rate_limited",
					"elapsed_minutes": math.Round(elapsedMinutes*100) / 100,
				})
				continue
			}

			extraBytes, _ := json.Marshal(event)
			triggerContext := map[string]any{
				"rule_name":      agentRule["name"],
				"trigger_state":  eventState,
				"trigger_type":   triggerType,
				"trigger_ref_id": triggerRefId,
				"extra":          string(extraBytes),
			}
			// Register a raw preservation window when an event triggers an agent.
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						logger.Debug(fmt.Sprintf("failed to register raw window for agent trigger %s", triggerRefId))
					}
				}()
				registerRawWindow(db, time.Now().UTC(), triggerType, triggerRefId, rowString(event["service"]), "", "")
			}()
			agentResults = append(agentResults, runAgentRuleInstance(db, agentRule, settings, triggerContext))
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":         true,
		"evaluated":  len(results),
		"fired":      fired,
		"results":    results,
		"agent_runs": agentResults,
	})
}

// getVapidPublicKeyRoute returns the VAPID public key for browser push setup.
//
// PORT-NOTE: named with a "Route" suffix to avoid colliding with the
// getVapidPublicKey helper.
func getVapidPublicKeyRoute(w http.ResponseWriter, r *http.Request) {
	pubKey, _ := getVapidPublicKey(nil)
	if pubKey == "" {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "VAPID key not configured"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "public_key": pubKey})
}

// serviceWorkerJs serves a minimal service worker for browser push.
func serviceWorkerJs(w http.ResponseWriter, r *http.Request) {
	swSource := `self.addEventListener('push', function (event) {
    var data = {};
    try {
        data = event.data ? event.data.json() : {};
    } catch (_err) {
        data = { title: 'SOBS Alert', body: event.data ? event.data.text() : 'Notification received' };
    }

    var title = (data && data.title) || 'SOBS Alert';
    var options = {
        body: (data && data.body) || 'Notification received',
    };

    event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener('notificationclick', function (event) {
    event.notification.close();
    event.waitUntil(clients.openWindow(self.registration.scope));
});
`
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Service-Worker-Allowed", "/")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(swSource))
}

// subscribeBrowserPush registers a browser push subscription as a channel.
func subscribeBrowserPush(w http.ResponseWriter, r *http.Request) {
	data, _ := readJsonBody(r)
	name := strings.TrimSpace(rowStringOr(data["name"], "Browser Push"))
	endpoint := strings.TrimSpace(rowString(data["endpoint"]))
	p256dh := strings.TrimSpace(rowString(data["p256dh"]))
	auth := strings.TrimSpace(rowString(data["auth"]))

	if endpoint == "" || p256dh == "" || auth == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "endpoint, p256dh, and auth are required"})
		return
	}

	db := getDb()
	// Dedup: check if this endpoint is already registered.
	for _, ch := range loadNotificationChannels(db) {
		if rowString(ch["channel_type"]) == "browser_push" {
			if cfg, ok := ch["config"].(map[string]any); ok && rowString(cfg["endpoint"]) == endpoint {
				jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "channel_id": ch["id"], "existing": true})
				return
			}
		}
	}

	channelId := agentUuid4()
	storedConfig := encryptNotificationConfig(map[string]any{"endpoint": endpoint, "p256dh": p256dh, "auth": auth})
	configBytes, _ := json.Marshal(storedConfig)
	insertRowsJsonEachRow(db, "sobs_notification_channels", []Row{{
		"Id":          channelId,
		"Name":        name,
		"ChannelType": "browser_push",
		"ConfigJson":  string(configBytes),
		"Enabled":     1,
		"IsDeleted":   0,
		"Version":     time.Now().UnixMilli(),
	}})
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "channel_id": channelId, "existing": false})
}

// generateVapidKey generates a new VAPID key pair and persists the private key.
func generateVapidKey(w http.ResponseWriter, r *http.Request) {
	privateB64, publicB64, err := generateVapidKeys()
	if err != nil {
		logger.Error(fmt.Sprintf("VAPID key generation failed: %v", err))
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "failed to generate VAPID keys"})
		return
	}
	db := getDb()
	setAppSetting(db, vapidPrivateKeySetting, privateB64)
	envOverride := strings.TrimSpace(os.Getenv("SOBS_VAPID_PRIVATE_KEY")) != ""
	note := "New VAPID keys saved to the database. "
	if envOverride {
		note += "WARNING: SOBS_VAPID_PRIVATE_KEY env var is set and takes precedence — remove it or update it to use the new DB key."
	} else {
		note += "Keys are active immediately. Existing browser subscriptions will need to re-subscribe."
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":           true,
		"public_key":   publicB64,
		"saved_to_db":  true,
		"env_override": envOverride,
		"note":         note,
	})
}

// deleteVapidKeys removes the DB-stored VAPID private key.
func deleteVapidKeys(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	delAppSetting(db, vapidPrivateKeySetting)
	envOverride := strings.TrimSpace(os.Getenv("SOBS_VAPID_PRIVATE_KEY")) != ""
	note := "DB VAPID key cleared. "
	if envOverride {
		note += "The SOBS_VAPID_PRIVATE_KEY env var is still set and will continue to be used."
	} else {
		note += "Browser push is now unconfigured until new keys are generated."
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":           true,
		"env_override": envOverride,
		"note":         note,
	})
}

func init() {
	registerRoute("GET", "/settings/notifications", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(viewNotifications)(w, r)
	})
	registerRoute("POST", "/settings/notifications/channels", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(createNotificationChannel)(w, r)
	})
	registerRoute("POST", "/settings/notifications/channels/{channel_id}/delete", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(deleteNotificationChannel)(w, r)
	})
	registerRoute("POST", "/settings/notifications/channels/{channel_id}/toggle", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(toggleNotificationChannel)(w, r)
	})
	registerRoute("POST", "/api/notifications/channels/{channel_id}/test", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(testNotificationChannel)(w, r)
	})
	registerRoute("POST", "/settings/notifications/rules", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(createNotificationRule)(w, r)
	})
	registerRoute("POST", "/settings/notifications/rules/{rule_id}/toggle", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(toggleNotificationRule)(w, r)
	})
	registerRoute("POST", "/settings/notifications/rules/{rule_id}/delete", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(deleteNotificationRule)(w, r)
	})
	registerRoute("POST", "/api/notifications/rules/auto-generate", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(autoGenerateNotificationRules)(w, r)
	})
	registerRoute("POST", "/api/notifications/check", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(checkNotifications)(w, r)
	})
	registerRoute("GET", "/api/notifications/vapid-public-key", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(getVapidPublicKeyRoute)(w, r)
	})
	registerRoute("GET", "/service-worker.js", serviceWorkerJs)
	registerRoute("POST", "/api/notifications/subscribe", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(subscribeBrowserPush)(w, r)
	})
	registerRoute("POST", "/api/notifications/vapid-keygen", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(generateVapidKey)(w, r)
	})
	registerRoute("DELETE", "/api/notifications/vapid-keys", func(w http.ResponseWriter, r *http.Request) {
		requireBasicAuth(deleteVapidKeys)(w, r)
	})
}
