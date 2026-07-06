package main

// Data Management settings + Setup Wizard + Onboarding wizard.
// Port of app.py lines ~31785-33930 (the final handler block before __main__).
// See CONVENTIONS.md. Symbols referenced from other sections (getAppSetting,
// setAppSetting, delAppSetting, getDb, insertRowsJsonEachRow, getDbStats,
// fmtBytes, encryptSecretValue/decryptSecretValue, the GitHub helpers, etc.)
// are owned elsewhere and used by the deterministic naming rule.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func init() {
	registerRoute("GET", "/settings/data-management", requireBasicAuth(viewDmSettings))
	registerRoute("POST", "/settings/data-management", requireBasicAuth(saveDmSettings))
	registerRoute("GET", "/api/data-management/backup/list", requireBasicAuth(apiDmBackupList))
	registerRoute("POST", "/api/data-management/backup/run", requireBasicAuth(apiDmBackupRun))
	registerRoute("POST", "/api/data-management/restore", requireBasicAuth(apiDmRestore))
	registerRoute("POST", "/api/data-management/prune", requireBasicAuth(apiDmPrune))
	registerRoute("GET", "/api/setup-wizard/steps", requireBasicAuth(apiSetupWizardSteps))
	registerRoute("POST", "/api/onboarding/create-repo", requireBasicAuth(apiOnboardingCreateRepo))
	registerRoute("POST", "/api/onboarding/import-repo", requireBasicAuth(apiOnboardingImportRepo))
	registerRoute("POST", "/api/onboarding/list-repos", requireBasicAuth(apiOnboardingListRepos))
	registerRoute("GET", "/api/onboarding/inspect-repo", requireBasicAuth(apiOnboardingInspectRepo))
	registerRoute("POST", "/api/onboarding/create-issues", requireBasicAuth(apiOnboardingCreateIssues))
}

// ---------------------------------------------------------------------------
// Data Management Settings  GET/POST /settings/data-management
// ---------------------------------------------------------------------------

var dmSettingKeys = []string{
	"data_management.backup_enabled",
	"data_management.s3_bucket",
	"data_management.s3_access_key_id",
	"data_management.s3_secret_access_key",
	"data_management.s3_region",
	"data_management.s3_path_prefix",
	"data_management.s3_encrypt_backup",
	"data_management.backup_encryption_password",
	"data_management.backup_schedule_full",
	"data_management.backup_schedule_incremental",
	"data_management.ttl_logs_days",
	"data_management.ttl_traces_days",
	"data_management.ttl_metrics_hours",
	"data_management.ttl_sessions_days",
	"data_management.ttl_backup_coupling_enabled",
}

var dmSensitiveSettingKeys = map[string]bool{
	"data_management.s3_secret_access_key":       true,
	"data_management.backup_encryption_password": true,
}

var (
	dmS3EndpointRe   = regexp.MustCompile(`^[A-Za-z0-9:/._-]+$`)
	dmS3PrefixRe     = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)
	dmAwsRegionRe    = regexp.MustCompile(`^[a-z0-9-]*$`)
	dmAwsAccessKeyRe = regexp.MustCompile(`^[A-Za-z0-9]*$`)
	dmAwsSecretKeyRe = regexp.MustCompile(`^[A-Za-z0-9/+=]*$`)
	dmBackupNameRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,200}$`)
)

// dmPruneLock prevents concurrent manual prune operations.
var dmPruneLock sync.Mutex

var dmPrunePeriodUnits = map[string]string{
	"hours": "HOUR",
	"days":  "DAY",
}

// dmTtlTable: table_name, timestamp_expr, setting_key.
type dmTtlTable struct {
	table      string
	tsCol      string
	settingKey string
}

// Tables managed by data-management TTL settings and the timestamp column to
// use in the ALTER TABLE … MODIFY TTL expression.
var dmTtlTables = []dmTtlTable{
	{"otel_logs", "Timestamp", "data_management.ttl_logs_days"},
	{"otel_traces", "Timestamp", "data_management.ttl_traces_days"},
	{"hyperdx_sessions", "Timestamp", "data_management.ttl_sessions_days"},
}

// dmMetricTable: table_name, timestamp_column. Metric tables use millisecond
// timestamps, handled separately.
type dmMetricTable struct {
	table string
	tsCol string
}

var dmMetricTables = []dmMetricTable{
	{"otel_metrics_gauge", "TimeUnixMs"},
	{"otel_metrics_sum", "TimeUnixMs"},
	{"otel_metrics_histogram", "TimeUnixMs"},
}

func isSensitiveDmSettingKey(key string) bool {
	return dmSensitiveSettingKeys[key]
}

// sqlQuoteLiteral returns a safely quoted SQL string literal for ClickHouse.
func sqlQuoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// requireDmSafeValue mirrors _require_dm_safe_value: empty values pass.
func requireDmSafeValue(fieldName, value string, pattern *regexp.Regexp) error {
	if value != "" && !pattern.MatchString(value) {
		return fmt.Errorf("%s contains unsupported characters", fieldName)
	}
	return nil
}

func validateDmBackupName(backupName string) error {
	if !dmBackupNameRe.MatchString(backupName) {
		return fmt.Errorf("backup_name contains unsupported characters")
	}
	return nil
}

func validateDmS3Settings(settings map[string]string) error {
	bucket := strings.TrimRight(strings.TrimSpace(settings["data_management.s3_bucket"]), "/")
	prefix := strings.Trim(strings.TrimSpace(settings["data_management.s3_path_prefix"]), "/")
	region := strings.TrimSpace(settings["data_management.s3_region"])
	accessKey := strings.TrimSpace(settings["data_management.s3_access_key_id"])
	secretKey := strings.TrimSpace(settings["data_management.s3_secret_access_key"])

	if err := requireDmSafeValue("s3_bucket", bucket, dmS3EndpointRe); err != nil {
		return err
	}
	if err := requireDmSafeValue("s3_path_prefix", prefix, dmS3PrefixRe); err != nil {
		return err
	}
	if err := requireDmSafeValue("s3_region", region, dmAwsRegionRe); err != nil {
		return err
	}
	if err := requireDmSafeValue("s3_access_key_id", accessKey, dmAwsAccessKeyRe); err != nil {
		return err
	}
	if err := requireDmSafeValue("s3_secret_access_key", secretKey, dmAwsSecretKeyRe); err != nil {
		return err
	}
	return nil
}

// loadDmSettings loads all data-management settings from sobs_app_settings.
func loadDmSettings(db *ChDbConnection, includeSensitiveValues bool) map[string]string {
	result := map[string]string{}
	for _, k := range dmSettingKeys {
		result[k] = ""
	}
	for _, key := range dmSettingKeys {
		raw := getAppSettingRaw(db, key)
		if raw != "" {
			if isSensitiveDmSettingKey(key) {
				if includeSensitiveValues {
					result[key] = decryptSecretValue(raw)
				} else {
					result[key] = ""
				}
			} else {
				result[key] = raw
			}
		}
	}
	return result
}

// getAppSettingRaw returns the raw (possibly encrypted) stored value without
// decryption.
func getAppSettingRaw(db *ChDbConnection, key string) string {
	res, err := db.Execute(
		"SELECT Value FROM sobs_app_settings FINAL WHERE Key = ? LIMIT 1",
		key,
	)
	if err != nil || res == nil {
		return ""
	}
	row := res.Fetchone()
	if row == nil || len(res.Cols) == 0 {
		return ""
	}
	return strings.TrimSpace(rowString(row[res.Cols[0]]))
}

func setDmSetting(db *ChDbConnection, key, value string) {
	stored := value
	if isSensitiveDmSettingKey(key) {
		stored = encryptSecretValue(value)
	}
	_, _ = insertRowsJsonEachRow(db, "sobs_app_settings", []Row{
		{"Key": key, "Value": stored, "UpdatedAt": time.Now().UnixMilli()},
	})
}

// dmSettingsFromForm parses data-management settings from a submitted HTML form.
func dmSettingsFromForm(form func(string) string) map[string]string {
	flag := func(name string) string {
		if form(name) == "1" {
			return "1"
		}
		return "0"
	}
	return map[string]string{
		"data_management.backup_enabled":              flag("backup_enabled"),
		"data_management.s3_bucket":                   strings.TrimSpace(form("s3_bucket")),
		"data_management.s3_access_key_id":            strings.TrimSpace(form("s3_access_key_id")),
		"data_management.s3_secret_access_key":        strings.TrimSpace(form("s3_secret_access_key")),
		"data_management.s3_region":                   strings.TrimSpace(form("s3_region")),
		"data_management.s3_path_prefix":              strings.TrimSpace(form("s3_path_prefix")),
		"data_management.s3_encrypt_backup":           flag("s3_encrypt_backup"),
		"data_management.backup_encryption_password":  strings.TrimSpace(form("backup_encryption_password")),
		"data_management.backup_schedule_full":        strings.TrimSpace(form("backup_schedule_full")),
		"data_management.backup_schedule_incremental": strings.TrimSpace(form("backup_schedule_incremental")),
		"data_management.ttl_logs_days":               strings.TrimSpace(form("ttl_logs_days")),
		"data_management.ttl_traces_days":             strings.TrimSpace(form("ttl_traces_days")),
		"data_management.ttl_metrics_hours":           strings.TrimSpace(form("ttl_metrics_hours")),
		"data_management.ttl_sessions_days":           strings.TrimSpace(form("ttl_sessions_days")),
		"data_management.ttl_backup_coupling_enabled": flag("ttl_backup_coupling_enabled"),
	}
}

// dmBackupEnabled returns true if the backup feature is enabled in settings.
func dmBackupEnabled(db *ChDbConnection) bool {
	resolvedDb := db
	if resolvedDb == nil {
		resolvedDb = getDb()
	}
	v := getAppSetting(resolvedDb, "data_management.backup_enabled")
	if v == "" {
		v = "0"
	}
	return v == "1"
}

// applyDmTtl applies TTL settings to ClickHouse tables. Returns list of errors
// (empty = success).
func applyDmTtl(db *ChDbConnection, settings map[string]string) []string {
	errors := []string{}
	for _, t := range dmTtlTables {
		rawDays := strings.TrimSpace(settings[t.settingKey])
		if rawDays == "" {
			continue
		}
		days, convErr := strconv.Atoi(rawDays)
		if convErr != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", t.table, convErr))
			continue
		}
		if days <= 0 {
			errors = append(errors, fmt.Sprintf("%s: TTL days must be a positive integer", t.table))
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s MODIFY TTL %s + INTERVAL %d DAY", t.table, t.tsCol, days)
		if _, execErr := db.Execute(stmt); execErr != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", t.table, execErr))
		}
	}

	rawHours := strings.TrimSpace(settings["data_management.ttl_metrics_hours"])
	if rawHours != "" {
		hours, convErr := strconv.Atoi(rawHours)
		if convErr != nil {
			errors = append(errors, "metrics: TTL hours must be a positive integer")
		} else if hours <= 0 {
			errors = append(errors, "metrics: TTL hours must be a positive integer")
		} else {
			for _, t := range dmMetricTables {
				stmt := fmt.Sprintf(
					"ALTER TABLE %s MODIFY TTL toDateTime(intDiv(%s, 1000)) + INTERVAL %d HOUR",
					t.table, t.tsCol, hours,
				)
				if _, execErr := db.Execute(stmt); execErr != nil {
					errors = append(errors, fmt.Sprintf("%s: %v", t.table, execErr))
				}
			}
		}
	}

	return errors
}

// acquireDmPruneLock mirrors _acquire_dm_prune_lock: returns false when a prune
// is already in progress.
func acquireDmPruneLock() bool {
	return dmPruneLock.TryLock()
}

// parseDmPrunePeriod returns (value, unit, ok, error). ok=false means no period
// requested (Python None).
func parseDmPrunePeriod(payload map[string]any) (int, string, bool, error) {
	rawValue, hasValue := payload["prune_period_value"]
	rawUnit := strings.ToLower(strings.TrimSpace(rowString(payload["prune_period_unit"])))

	valueEmpty := !hasValue || rawValue == nil || rowString(rawValue) == ""
	if valueEmpty && rawUnit == "" {
		return 0, "", false, nil
	}
	if valueEmpty {
		return 0, "", false, fmt.Errorf("prune_period_value is required when prune_period_unit is provided")
	}
	if rawUnit == "" {
		return 0, "", false, fmt.Errorf("prune_period_unit is required when prune_period_value is provided")
	}
	if _, ok := dmPrunePeriodUnits[rawUnit]; !ok {
		return 0, "", false, fmt.Errorf("prune_period_unit must be 'hours' or 'days'")
	}

	periodValue, convErr := strconv.Atoi(strings.TrimSpace(rowString(rawValue)))
	if convErr != nil {
		return 0, "", false, fmt.Errorf("prune_period_value must be a positive integer")
	}
	if periodValue <= 0 {
		return 0, "", false, fmt.Errorf("prune_period_value must be a positive integer")
	}
	return periodValue, rawUnit, true, nil
}

func getDmColumnType(db *ChDbConnection, table, column string) (string, bool) {
	res, err := db.Execute(fmt.Sprintf("DESCRIBE TABLE %s", table))
	if err != nil || res == nil {
		return "", false
	}
	rows := res.Fetchall()
	if len(res.Cols) < 2 {
		return "", false
	}
	for _, row := range rows {
		if row != nil && rowString(row[res.Cols[0]]) == column {
			return strings.ToLower(strings.TrimSpace(rowString(row[res.Cols[1]]))), true
		}
	}
	return "", false
}

// runDmPrune forces TTL processing on all data-management tables. When
// prunePeriod is set (pruneOk), a one-time DELETE window is applied before
// OPTIMIZE TABLE … FINAL runs across managed tables.
func runDmPrune(db *ChDbConnection, pruneValue int, pruneUnit string, pruneOk bool) map[string]any {
	allTables := []string{}
	for _, t := range dmTtlTables {
		allTables = append(allTables, t.table)
	}
	for _, t := range dmMetricTables {
		allTables = append(allTables, t.table)
	}
	errors := []string{}
	if pruneOk {
		unitSql := dmPrunePeriodUnits[pruneUnit]
		for _, t := range dmTtlTables {
			stmt := fmt.Sprintf(
				"ALTER TABLE %s DELETE WHERE %s < now() - INTERVAL %d %s",
				t.table, t.tsCol, pruneValue, unitSql,
			)
			if _, err := db.Execute(stmt); err != nil {
				errors = append(errors, fmt.Sprintf("%s: %v", t.table, err))
			}
		}
		for _, t := range dmMetricTables {
			detectedColType, found := getDmColumnType(db, t.table, t.tsCol)
			useMsExpr := !found || !strings.Contains(detectedColType, "datetime")

			msExpr := fmt.Sprintf(
				"ALTER TABLE %s DELETE WHERE toDateTime(intDiv(%s, 1000)) < now() - INTERVAL %d %s",
				t.table, t.tsCol, pruneValue, unitSql,
			)
			plainExpr := fmt.Sprintf(
				"ALTER TABLE %s DELETE WHERE %s < now() - INTERVAL %d %s",
				t.table, t.tsCol, pruneValue, unitSql,
			)
			var primarySql, fallbackSql string
			if useMsExpr {
				primarySql, fallbackSql = msExpr, plainExpr
			} else {
				primarySql, fallbackSql = plainExpr, msExpr
			}

			if _, err := db.Execute(primarySql); err != nil {
				if _, fbErr := db.Execute(fallbackSql); fbErr != nil {
					errors = append(errors, fmt.Sprintf("%s: %v (fallback after: %v)", t.table, fbErr, err))
				}
			}
		}
	}

	for _, table := range allTables {
		if _, err := db.Execute(fmt.Sprintf("OPTIMIZE TABLE %s FINAL", table)); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", table, err))
		}
	}
	if len(errors) > 0 {
		return map[string]any{"ok": false, "message": "Prune completed with errors: " + strings.Join(errors, "; ")}
	}
	if pruneOk {
		return map[string]any{
			"ok": true,
			"message": fmt.Sprintf(
				"Prune completed successfully (%d tables processed, custom period: %d %s)",
				len(allTables), pruneValue, pruneUnit,
			),
		}
	}
	return map[string]any{
		"ok":      true,
		"message": fmt.Sprintf("Prune completed successfully (%d tables processed)", len(allTables)),
	}
}

// buildS3BackupDest builds a ClickHouse S3 backup destination string from
// settings. Returns an error on invalid name/settings.
func buildS3BackupDest(settings map[string]string, backupName string) (string, error) {
	if err := validateDmBackupName(backupName); err != nil {
		return "", err
	}
	if err := validateDmS3Settings(settings); err != nil {
		return "", err
	}

	bucket := strings.TrimRight(strings.TrimSpace(settings["data_management.s3_bucket"]), "/")
	prefix := strings.Trim(strings.TrimSpace(settings["data_management.s3_path_prefix"]), "/")
	region := strings.TrimSpace(settings["data_management.s3_region"])
	accessKey := strings.TrimSpace(settings["data_management.s3_access_key_id"])
	secretKey := strings.TrimSpace(settings["data_management.s3_secret_access_key"])

	var path string
	if prefix != "" {
		path = fmt.Sprintf("%s/%s/%s", bucket, prefix, backupName)
	} else {
		path = fmt.Sprintf("%s/%s", bucket, backupName)
	}
	// Ensure path starts with https:// if not already a full URL.
	var endpoint string
	if !strings.HasPrefix(path, "http") {
		if region != "" {
			endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com/%s", region, path)
		} else {
			endpoint = fmt.Sprintf("https://s3.amazonaws.com/%s", path)
		}
	} else {
		endpoint = path
	}

	if accessKey != "" && secretKey != "" {
		return fmt.Sprintf("S3(%s, %s, %s)", sqlQuoteLiteral(endpoint), sqlQuoteLiteral(accessKey), sqlQuoteLiteral(secretKey)), nil
	}
	return fmt.Sprintf("S3(%s)", sqlQuoteLiteral(endpoint)), nil
}

// listDmBackups lists available backups from ClickHouse system.backups table.
func listDmBackups(db *ChDbConnection, settings map[string]string) []map[string]string {
	res, err := db.Execute(
		"SELECT name, status, start_time, end_time, num_files, total_size, error " +
			"FROM system.backups ORDER BY start_time DESC LIMIT 100",
	)
	if err != nil || res == nil {
		return []map[string]string{}
	}
	rows := res.Fetchall()
	if len(res.Cols) < 7 {
		return []map[string]string{}
	}
	result := []map[string]string{}
	str := func(row Row, i int) string {
		v := row[res.Cols[i]]
		if v == nil || rowString(v) == "" {
			return ""
		}
		return rowString(v)
	}
	numOrZero := func(row Row, i int) string {
		s := str(row, i)
		if s == "" {
			return "0"
		}
		return s
	}
	for _, row := range rows {
		result = append(result, map[string]string{
			"name":       str(row, 0),
			"status":     str(row, 1),
			"start_time": str(row, 2),
			"end_time":   str(row, 3),
			"num_files":  numOrZero(row, 4),
			"total_size": numOrZero(row, 5),
			"error":      str(row, 6),
		})
	}
	return result
}

// runDmBackup runs a ClickHouse BACKUP ALL command to S3. Returns {ok, message}
// where ok is the string "true"/"false" (mirrors Python).
func runDmBackup(db *ChDbConnection, settings map[string]string, backupType string) map[string]string {
	if strings.TrimSpace(settings["data_management.s3_bucket"]) == "" {
		return map[string]string{"ok": "false", "message": "S3 bucket is not configured"}
	}

	ts := time.Now().UTC().Format("20060102T150405Z")
	backupName := fmt.Sprintf("sobs-%s-%s", backupType, ts)
	dest, err := buildS3BackupDest(settings, backupName)
	if err != nil {
		return map[string]string{"ok": "false", "message": err.Error()}
	}

	baseClause := ""
	if backupType == "incremental" {
		// Attempt to find the most recent completed backup to use as base.
		backups := listDmBackups(db, settings)
		var completed []map[string]string
		for _, b := range backups {
			if b["status"] == "BACKUP_COMPLETE" && strings.HasPrefix(b["name"], "sobs-") {
				completed = append(completed, b)
			}
		}
		if len(completed) > 0 {
			baseName := completed[0]["name"]
			baseDest, baseErr := buildS3BackupDest(settings, baseName)
			if baseErr != nil {
				return map[string]string{"ok": "false", "message": baseErr.Error()}
			}
			baseClause = fmt.Sprintf("BASE_BACKUP %s", baseDest)
		}
	}

	encryptClause := ""
	if settings["data_management.s3_encrypt_backup"] == "1" {
		encPassword := strings.TrimSpace(settings["data_management.backup_encryption_password"])
		if encPassword == "" {
			return map[string]string{"ok": "false", "message": "Backup encryption is enabled but no encryption password is configured"}
		}
		encryptClause = fmt.Sprintf(" SETTINGS compression_method='lz4', encryption_password=%s", sqlQuoteLiteral(encPassword))
	}

	baseSql := ""
	if baseClause != "" {
		baseSql = ", " + baseClause
	}
	sql := fmt.Sprintf("BACKUP ALL TO %s%s%s", dest, baseSql, encryptClause)
	if _, execErr := db.Execute(sql); execErr != nil {
		return map[string]string{"ok": "false", "message": execErr.Error()}
	}
	return map[string]string{"ok": "true", "message": fmt.Sprintf("Backup '%s' started successfully", backupName)}
}

// runDmRestore restores from a named backup. Returns {ok, message}.
func runDmRestore(db *ChDbConnection, settings map[string]string, backupName string) map[string]string {
	if backupName == "" {
		return map[string]string{"ok": "false", "message": "backup_name is required"}
	}
	if strings.TrimSpace(settings["data_management.s3_bucket"]) == "" {
		return map[string]string{"ok": "false", "message": "S3 bucket is not configured"}
	}

	dest, err := buildS3BackupDest(settings, backupName)
	if err != nil {
		return map[string]string{"ok": "false", "message": err.Error()}
	}
	sql := fmt.Sprintf("RESTORE ALL FROM %s", dest)
	if _, execErr := db.Execute(sql); execErr != nil {
		return map[string]string{"ok": "false", "message": execErr.Error()}
	}
	return map[string]string{"ok": "true", "message": fmt.Sprintf("Restore from '%s' started successfully", backupName)}
}

// ---------------------------------------------------------------------------
// Data Management route handlers
// ---------------------------------------------------------------------------

// viewDmSettings renders the data management settings page (TTL, backup, restore).
func viewDmSettings(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	settings := loadDmSettings(db, false)
	dmSecretPresent := map[string]any{
		"s3_secret_access_key":       getAppSettingRaw(db, "data_management.s3_secret_access_key") != "",
		"backup_encryption_password": getAppSettingRaw(db, "data_management.backup_encryption_password") != "",
	}
	flashMsg := r.URL.Query().Get("msg")
	flashType := r.URL.Query().Get("msg_type")
	if flashType == "" {
		flashType = "success"
	}
	dbStats := getDbStats(db)
	renderTemplate(w, r, "settings_data_management.html", map[string]any{
		"dm_settings":       settings,
		"dm_secret_present": dmSecretPresent,
		"flash_msg":         flashMsg,
		"flash_type":        flashType,
		"db_stats":          dbStats,
		"fmt_bytes":         fmtBytes,
	})
}

// saveDmSettings saves data management settings and optionally applies TTL.
func saveDmSettings(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := func(name string) string { return r.FormValue(name) }
	newSettings := dmSettingsFromForm(form)
	db := getDb()

	clearSensitiveKeys := map[string]bool{}
	if form("clear_s3_secret_access_key") == "1" {
		clearSensitiveKeys["data_management.s3_secret_access_key"] = true
	}
	if form("clear_backup_encryption_password") == "1" {
		clearSensitiveKeys["data_management.backup_encryption_password"] = true
	}

	// Iterate in stable key order for deterministic behaviour.
	for _, key := range dmSettingKeys {
		value := newSettings[key]
		if clearSensitiveKeys[key] {
			delAppSetting(db, key)
			continue
		}
		if isSensitiveDmSettingKey(key) && value == "" {
			// Preserve existing sensitive values when fields are left blank.
			continue
		}
		if value != "" {
			setDmSetting(db, key, value)
		} else {
			delAppSetting(db, key)
		}
	}

	// Apply TTL immediately if the form requested it.
	if form("apply_ttl") == "1" {
		errors := applyDmTtl(db, newSettings)
		if len(errors) > 0 {
			limit := errors
			if len(limit) > 3 {
				limit = limit[:3]
			}
			msg := "Settings saved but TTL errors: " + strings.Join(limit, "; ")
			redirectUrl := "/settings/data-management?msg=" + msg + "&msg_type=warning"
			http.Redirect(w, r, redirectUrl, http.StatusFound)
			return
		}
	}

	http.Redirect(w, r, "/settings/data-management?msg=Settings+saved&msg_type=success", http.StatusFound)
}

// apiDmBackupList returns the list of available backups from system.backups.
func apiDmBackupList(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	settings := loadDmSettings(db, true)
	backups := listDmBackups(db, settings)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "backups": backups})
}

// apiDmBackupRun triggers a ClickHouse BACKUP ALL to the configured S3 dest.
func apiDmBackupRun(w http.ResponseWriter, r *http.Request) {
	if !dmBackupEnabled(nil) {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "message": "Backup feature is disabled"})
		return
	}
	data, _ := readJsonBody(r)
	backupType := strings.ToLower(rowString(data["type"]))
	if backupType == "" {
		backupType = "full"
	}
	if backupType != "full" && backupType != "incremental" {
		backupType = "full"
	}
	db := getDb()
	settings := loadDmSettings(db, true)
	result := runDmBackup(db, settings, backupType)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": result["ok"] == "true", "message": result["message"]})
}

// apiDmRestore restores from a named backup on the configured S3 destination.
func apiDmRestore(w http.ResponseWriter, r *http.Request) {
	if !dmBackupEnabled(nil) {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "message": "Backup feature is disabled"})
		return
	}
	data, _ := readJsonBody(r)
	backupName := strings.TrimSpace(rowString(data["backup_name"]))
	if backupName != "" {
		if err := validateDmBackupName(backupName); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
			return
		}
	}
	db := getDb()
	settings := loadDmSettings(db, true)
	result := runDmRestore(db, settings, backupName)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": result["ok"] == "true", "message": result["message"]})
}

// apiDmPrune triggers an immediate prune of all TTL-managed tables via
// OPTIMIZE TABLE … FINAL.
func apiDmPrune(w http.ResponseWriter, r *http.Request) {
	rawBody, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	rawBodyTrimmed := strings.TrimSpace(string(rawBody))
	isJson := strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "application/json")

	// Mirror await request.get_json(silent=True): None on empty/invalid JSON.
	var decoded any
	parsedOk := false
	if rawBodyTrimmed != "" {
		dec := json.NewDecoder(bytes.NewReader(rawBody))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err == nil {
			parsedOk = true
		}
	}

	var payload map[string]any
	if !parsedOk {
		// payload is None
		if rawBodyTrimmed != "" && isJson {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "request body contains invalid JSON"})
			return
		}
		payload = map[string]any{}
	} else if obj, ok := decoded.(map[string]any); ok {
		payload = obj
	} else {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "request body must be a JSON object"})
		return
	}

	pruneValue, pruneUnit, pruneOk, err := parseDmPrunePeriod(payload)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}

	if !acquireDmPruneLock() {
		jsonResponse(w, http.StatusConflict, map[string]any{"ok": false, "message": "A prune operation is already in progress"})
		return
	}
	defer dmPruneLock.Unlock()
	db := getDb()
	result := runDmPrune(db, pruneValue, pruneUnit, pruneOk)
	jsonResponse(w, http.StatusOK, result)
}

// ---------------------------------------------------------------------------
// Setup Wizard API  (first-time instrumentation bootstrap)
// ---------------------------------------------------------------------------

// setupWizardVersion is the version stamp embedded in generated setup steps so
// consumers can detect staleness.
const setupWizardVersion = "1"

// Supported option values (used for validation).
var (
	wizardEnvs        = map[string]bool{"dev": true, "prod": true}
	wizardLanguages   = map[string]bool{"python": true, "node": true, "go": true, "java": true, "dotnet": true, "ruby": true, "php": true}
	wizardDeployments = map[string]bool{"docker": true, "kubernetes": true, "baremetal": true, "cloud": true}
)

// formatPySortedList renders a sorted []string the way Python renders
// `sorted(set)` inside an f-string: ['a', 'b', 'c'].
func formatPySortedList(items []string) string {
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	quoted := make([]string, len(sorted))
	for i, s := range sorted {
		quoted[i] = "'" + s + "'"
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// buildSetupWizardSteps returns a deterministic, ordered list of setup steps
// for the given context. Each step has id/title/description/commands/language.
func buildSetupWizardSteps(env, language, deployment string) map[string]any {
	prod := env == "prod"

	// 1. SDK install --------------------------------------------------------
	sdkSteps := []map[string]any{}
	switch language {
	case "python":
		pkgs := "opentelemetry-sdk opentelemetry-exporter-otlp opentelemetry-instrumentation"
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_install",
			"title":       "Install OpenTelemetry Python SDK",
			"description": "Add the core SDK and OTLP exporter to your project.",
			"commands":    []string{"pip install " + pkgs},
			"language":    "bash",
		})
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_init",
			"title":       "Initialise SDK in your application",
			"description": "Bootstrap tracing and metrics at startup.",
			"commands": []string{
				"from opentelemetry import trace",
				"from opentelemetry.sdk.trace import TracerProvider",
				"from opentelemetry.sdk.trace.export import BatchSpanProcessor",
				"from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter",
				"",
				"provider = TracerProvider()",
				"provider.add_span_processor(",
				`    BatchSpanProcessor(OTLPSpanExporter(endpoint="http://localhost:4317", insecure=True))`,
				")",
				"trace.set_tracer_provider(provider)",
			},
			"language": "python",
		})
	case "node":
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_install",
			"title":       "Install OpenTelemetry Node.js SDK",
			"description": "Add the SDK and OTLP exporter packages.",
			"commands": []string{
				"npm install @opentelemetry/sdk-node " +
					"@opentelemetry/auto-instrumentations-node " +
					"@opentelemetry/exporter-trace-otlp-grpc",
			},
			"language": "bash",
		})
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_init",
			"title":       "Initialise SDK (tracing.js)",
			"description": "Create tracing.js and require it before your app entry.",
			"commands": []string{
				"// tracing.js",
				"const { NodeSDK } = require('@opentelemetry/sdk-node');",
				"const { getNodeAutoInstrumentations } = require('@opentelemetry/auto-instrumentations-node');",
				"const { OTLPTraceExporter } = require('@opentelemetry/exporter-trace-otlp-grpc');",
				"",
				"const sdk = new NodeSDK({",
				"  traceExporter: new OTLPTraceExporter({ url: 'http://localhost:4317' }),",
				"  instrumentations: [getNodeAutoInstrumentations()],",
				"});",
				"sdk.start();",
			},
			"language": "javascript",
		})
	case "go":
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_install",
			"title":       "Add OpenTelemetry Go dependencies",
			"description": "Fetch the SDK and OTLP gRPC exporter modules.",
			"commands": []string{
				"go get go.opentelemetry.io/otel",
				"go get go.opentelemetry.io/otel/sdk/trace",
				"go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc",
			},
			"language": "bash",
		})
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_init",
			"title":       "Initialise tracer provider",
			"description": "Wire the OTLP exporter into your main function.",
			"commands": []string{
				"exp, _ := otlptracegrpc.New(ctx, otlptracegrpc.WithInsecure(), " +
					`otlptracegrpc.WithEndpoint("localhost:4317"))`,
				"tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))",
				"otel.SetTracerProvider(tp)",
				"defer tp.Shutdown(ctx)",
			},
			"language": "go",
		})
	case "java":
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_install",
			"title":       "Add OpenTelemetry Java dependencies (Maven)",
			"description": "Add the OTLP exporter and SDK to your pom.xml.",
			"commands": []string{
				"<dependency>",
				"  <groupId>io.opentelemetry</groupId>",
				"  <artifactId>opentelemetry-sdk</artifactId>",
				"  <version>1.36.0</version>",
				"</dependency>",
				"<dependency>",
				"  <groupId>io.opentelemetry</groupId>",
				"  <artifactId>opentelemetry-exporter-otlp</artifactId>",
				"  <version>1.36.0</version>",
				"</dependency>",
			},
			"language": "xml",
		})
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_init",
			"title":       "Alternatively use the Java agent (zero-code)",
			"description": "Attach the agent JAR to your JVM startup for automatic instrumentation.",
			"commands": []string{
				"# Download the agent",
				"curl -Lo opentelemetry-javaagent.jar " +
					"https://github.com/open-telemetry/opentelemetry-java-instrumentation/" +
					"releases/latest/download/opentelemetry-javaagent.jar",
				"",
				"# Run your app with the agent",
				"java -javaagent:opentelemetry-javaagent.jar " +
					"-Dotel.exporter.otlp.endpoint=http://localhost:4317 " +
					"-jar your-app.jar",
			},
			"language": "bash",
		})
	case "dotnet":
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_install",
			"title":       "Add OpenTelemetry .NET packages",
			"description": "Install the SDK and OTLP exporter via NuGet.",
			"commands": []string{
				"dotnet add package OpenTelemetry",
				"dotnet add package OpenTelemetry.Exporter.OpenTelemetryProtocol",
				"dotnet add package OpenTelemetry.Extensions.Hosting",
				"dotnet add package OpenTelemetry.Instrumentation.AspNetCore",
			},
			"language": "bash",
		})
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_init",
			"title":       "Register OpenTelemetry in Program.cs",
			"description": "Configure tracing with OTLP export in your startup code.",
			"commands": []string{
				"builder.Services.AddOpenTelemetry()",
				"  .WithTracing(b => b",
				"    .AddAspNetCoreInstrumentation()",
				`    .AddOtlpExporter(o => o.Endpoint = new Uri("http://localhost:4317")));`,
			},
			"language": "csharp",
		})
	case "ruby":
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_install",
			"title":       "Add OpenTelemetry Ruby gems",
			"description": "Add the SDK and OTLP exporter to your Gemfile.",
			"commands": []string{
				"gem 'opentelemetry-sdk'",
				"gem 'opentelemetry-exporter-otlp'",
				"gem 'opentelemetry-instrumentation-all'",
				"",
				"# then run:",
				"bundle install",
			},
			"language": "ruby",
		})
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_init",
			"title":       "Configure the SDK",
			"description": "Initialise OTEL before your app boots.",
			"commands": []string{
				"require 'opentelemetry/sdk'",
				"require 'opentelemetry/exporter/otlp'",
				"require 'opentelemetry/instrumentation/all'",
				"",
				"OpenTelemetry::SDK.configure do |c|",
				"  c.service_name = 'my-service'",
				"  c.use_all",
				"end",
			},
			"language": "ruby",
		})
	case "php":
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_install",
			"title":       "Install OpenTelemetry PHP SDK",
			"description": "Add the SDK and OTLP exporter via Composer.",
			"commands": []string{
				"composer require open-telemetry/sdk open-telemetry/exporter-otlp",
			},
			"language": "bash",
		})
		sdkSteps = append(sdkSteps, map[string]any{
			"id":          "sdk_init",
			"title":       "Bootstrap the SDK",
			"description": "Configure a tracer provider before handling requests.",
			"commands": []string{
				`use OpenTelemetry\SDK\Trace\TracerProviderFactory;`,
				`use OpenTelemetry\Contrib\Otlp\OtlpHttpTransportFactory;`,
				"",
				"$tracerProvider = (new TracerProviderFactory())->create();",
				`\OpenTelemetry\API\Globals::registerInitializer(fn() => $tracerProvider);`,
			},
			"language": "php",
		})
	}

	// 2. Collector config ---------------------------------------------------
	sobsOtlpEndpoint := "http://localhost:44317"
	if prod {
		sobsOtlpEndpoint = "http://sobs:44317"
	}

	var collectorSteps []map[string]any
	switch deployment {
	case "docker":
		collectorSteps = []map[string]any{
			{
				"id":          "collector_run",
				"title":       "Run the OpenTelemetry Collector (Docker)",
				"description": "Start the contrib collector with a minimal config wired to SOBS.",
				"commands": []string{
					"# otel-collector-config.yaml",
					"receivers:",
					"  otlp:",
					"    protocols:",
					"      grpc:",
					"        endpoint: 0.0.0.0:4317",
					"      http:",
					"        endpoint: 0.0.0.0:4318",
					"exporters:",
					"  otlphttp:",
					"    endpoint: " + sobsOtlpEndpoint,
					"service:",
					"  pipelines:",
					"    traces:",
					"      receivers: [otlp]",
					"      exporters: [otlphttp]",
					"    metrics:",
					"      receivers: [otlp]",
					"      exporters: [otlphttp]",
					"    logs:",
					"      receivers: [otlp]",
					"      exporters: [otlphttp]",
				},
				"language": "yaml",
			},
			{
				"id":          "collector_docker_run",
				"title":       "Start the collector container",
				"description": "Mount the config and expose OTLP ports.",
				"commands": []string{
					"docker run -d --name otel-collector \\",
					"  -p 4317:4317 -p 4318:4318 \\",
					"  -v $(pwd)/otel-collector-config.yaml:/etc/otelcol-contrib/config.yaml \\",
					"  otel/opentelemetry-collector-contrib:latest",
				},
				"language": "bash",
			},
		}
	case "kubernetes":
		collectorSteps = []map[string]any{
			{
				"id":          "collector_k8s",
				"title":       "Deploy the OpenTelemetry Collector on Kubernetes",
				"description": "Apply a ConfigMap and Deployment that routes to SOBS.",
				"commands": []string{
					"# otel-collector-k8s.yaml",
					"apiVersion: v1",
					"kind: ConfigMap",
					"metadata:",
					"  name: otel-collector-config",
					"data:",
					"  config.yaml: |",
					"    receivers:",
					"      otlp:",
					"        protocols:",
					"          grpc:",
					"            endpoint: 0.0.0.0:4317",
					"    exporters:",
					"      otlphttp:",
					"        endpoint: " + sobsOtlpEndpoint,
					"    service:",
					"      pipelines:",
					"        traces:",
					"          receivers: [otlp]",
					"          exporters: [otlphttp]",
					"        metrics:",
					"          receivers: [otlp]",
					"          exporters: [otlphttp]",
					"        logs:",
					"          receivers: [otlp]",
					"          exporters: [otlphttp]",
					"---",
					"apiVersion: apps/v1",
					"kind: Deployment",
					"metadata:",
					"  name: otel-collector",
					"spec:",
					"  replicas: 1",
					"  selector:",
					"    matchLabels:",
					"      app: otel-collector",
					"  template:",
					"    metadata:",
					"      labels:",
					"        app: otel-collector",
					"    spec:",
					"      containers:",
					"      - name: otel-collector",
					"        image: otel/opentelemetry-collector-contrib:latest",
					"        args: ['--config=/etc/otelcol-contrib/config.yaml']",
					"        volumeMounts:",
					"        - name: config",
					"          mountPath: /etc/otelcol-contrib",
					"      volumes:",
					"      - name: config",
					"        configMap:",
					"          name: otel-collector-config",
				},
				"language": "yaml",
			},
			{
				"id":          "collector_k8s_apply",
				"title":       "Apply the manifest",
				"description": "Deploy the collector to your cluster.",
				"commands":    []string{"kubectl apply -f otel-collector-k8s.yaml"},
				"language":    "bash",
			},
		}
	case "cloud":
		collectorSteps = []map[string]any{
			{
				"id":          "collector_cloud",
				"title":       "Configure a managed OTLP pipeline",
				"description": "Point your cloud provider's OTLP endpoint to forward to SOBS.",
				"commands": []string{
					"# For AWS Distro for OpenTelemetry (ADOT):",
					"# Set the exporter endpoint in your ADOT config to:",
					"#   endpoint: " + sobsOtlpEndpoint,
					"",
					"# For GCP OpenTelemetry Collector:",
					"# Override the exporter.endpoint in your otel-config.yaml to:",
					"#   endpoint: " + sobsOtlpEndpoint,
				},
				"language": "yaml",
			},
		}
	default: // baremetal
		collectorSteps = []map[string]any{
			{
				"id":          "collector_binary",
				"title":       "Run the OpenTelemetry Collector (binary)",
				"description": "Download and run the contrib collector directly.",
				"commands": []string{
					"# Download (Linux amd64):",
					"curl -LO https://github.com/open-telemetry/opentelemetry-collector-releases/" +
						"releases/latest/download/otelcol-contrib_linux_amd64.tar.gz",
					"tar xzf otelcol-contrib_linux_amd64.tar.gz",
					"",
					"# Write config.yaml (same format as Docker example above)",
					"",
					"# Start:",
					"./otelcol-contrib --config=config.yaml",
				},
				"language": "bash",
			},
		}
	}

	// 3. SOBS wiring --------------------------------------------------------
	sobsSteps := []map[string]any{
		{
			"id":          "sobs_verify",
			"title":       "Verify data arrives in SOBS",
			"description": "Check the Summary page for incoming telemetry.",
			"commands": []string{
				"# Open your browser and navigate to " + sobsOtlpEndpoint + "/",
				"# The Summary card should show span, log, and metric counts within ~30 s.",
			},
			"language": "bash",
		},
	}
	if prod {
		sobsSteps = append(sobsSteps, map[string]any{
			"id":          "sobs_anomaly",
			"title":       "Enable anomaly detection rules",
			"description": "Head to Settings → Anomaly Rules and add your first threshold rule.",
			"commands": []string{
				"# Navigate to: " + sobsOtlpEndpoint + "/settings/anomaly-rules",
				"# Click 'Add Rule' and choose a metric from your stack.",
			},
			"language": "bash",
		})
	}

	// 4. Checklist items (used by the UI progress panel) --------------------
	checklist := []map[string]any{
		{"id": "sdk", "label": "Install & initialise the SDK"},
		{"id": "collector", "label": "Run the OpenTelemetry Collector"},
		{"id": "verify", "label": "Verify data in SOBS"},
	}
	if prod {
		checklist = append(checklist, map[string]any{"id": "anomaly", "label": "Configure anomaly detection"})
	}

	steps := []map[string]any{}
	steps = append(steps, sdkSteps...)
	steps = append(steps, collectorSteps...)
	steps = append(steps, sobsSteps...)

	return map[string]any{
		"version":    setupWizardVersion,
		"env":        env,
		"language":   language,
		"deployment": deployment,
		"steps":      steps,
		"checklist":  checklist,
	}
}

// apiSetupWizardSteps returns tailored OTEL setup steps for the given context.
func apiSetupWizardSteps(w http.ResponseWriter, r *http.Request) {
	env := strings.ToLower(strings.TrimSpace(queryGetDefault(r, "env", "dev")))
	language := strings.ToLower(strings.TrimSpace(queryGetDefault(r, "language", "python")))
	deployment := strings.ToLower(strings.TrimSpace(queryGetDefault(r, "deployment", "docker")))

	if !wizardEnvs[env] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("Invalid env '%s'. Must be one of: %s", env, formatPySortedList(mapKeys(wizardEnvs))),
		})
		return
	}
	if !wizardLanguages[language] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("Invalid language '%s'. Must be one of: %s", language, formatPySortedList(mapKeys(wizardLanguages))),
		})
		return
	}
	if !wizardDeployments[deployment] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("Invalid deployment '%s'. Must be one of: %s", deployment, formatPySortedList(mapKeys(wizardDeployments))),
		})
		return
	}

	result := buildSetupWizardSteps(env, language, deployment)
	result["ok"] = true
	jsonResponse(w, http.StatusOK, result)
}

// queryGetDefault returns the query value for key, or def when absent.
// PORT-NOTE: mirrors request.args.get(key, default) — note an empty-string
// query value is returned as "" (not the default), matching Werkzeug.
func queryGetDefault(r *http.Request, key, def string) string {
	if !r.URL.Query().Has(key) {
		return def
	}
	return r.URL.Query().Get(key)
}

// ---------------------------------------------------------------------------
// Onboarding wizard
// ---------------------------------------------------------------------------

var sobsCiMetadataIndicators = []string{
	"sobs",
	"sobs-agent",
	"register_release",
	"release_artifacts",
	"sobs_release",
	"sobs/api/apps",
	"/api/releases",
}

var sobsCiOtelIndicators = []string{
	"opentelemetry",
	"otlp",
	"otel",
	"opentelemetry-sdk",
	"opentelemetry-api",
}

// githubQuotePath mirrors urllib.parse.quote(path, safe="/"): percent-escape
// each segment but preserve the "/" separators.
func githubQuotePath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}

// githubRequest performs a GitHub REST call and returns (status, body, err).
// PORT-NOTE: uses the shared httpClient (follows redirects), matching the
// pattern in s05_agents.go createGithubIssueRecord, rather than the Python
// follow_redirects=False AsyncClient.
func githubRequest(method, urlStr string, headers map[string]string, body []byte, timeoutSec int) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, urlStr, rdr)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// githubListDirectory returns a directory listing from the GitHub Contents API
// and an optional error message.
func githubListDirectory(githubToken, owner, repo, path string) ([]map[string]any, string) {
	encoded := githubQuotePath(path)
	status, body, err := githubRequest(
		"GET",
		fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", owner, repo, encoded),
		githubApiHeaders(githubToken, false, nil), nil, 12,
	)
	if err != nil {
		return []map[string]any{}, fmt.Sprintf("GitHub API request failed for %s: %v", path, err)
	}
	if status != 200 {
		return []map[string]any{}, fmt.Sprintf("GitHub API returned %d for %s", status, path)
	}
	if len(body) == 0 {
		return []map[string]any{}, ""
	}
	var data any
	if jerr := json.Unmarshal(body, &data); jerr != nil {
		return []map[string]any{}, fmt.Sprintf("GitHub API request failed for %s: %v", path, jerr)
	}
	list, ok := data.([]any)
	if !ok {
		return []map[string]any{}, ""
	}
	out := []map[string]any{}
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, ""
}

// githubFileText fetches a file's text content from the GitHub Contents API and
// an optional error message.
func githubFileText(githubToken, owner, repo, path string) (string, string) {
	encoded := githubQuotePath(path)
	status, body, err := githubRequest(
		"GET",
		fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", owner, repo, encoded),
		githubApiHeaders(githubToken, false, nil), nil, 12,
	)
	if err != nil {
		return "", fmt.Sprintf("GitHub API request failed for %s: %v", path, err)
	}
	if status != 200 {
		return "", fmt.Sprintf("GitHub API returned %d for %s", status, path)
	}
	if len(body) == 0 {
		return "", ""
	}
	var data any
	if jerr := json.Unmarshal(body, &data); jerr != nil {
		return "", fmt.Sprintf("GitHub API request failed for %s: %v", path, jerr)
	}
	m, ok := data.(map[string]any)
	if !ok {
		return "", fmt.Sprintf("Unexpected GitHub API response for %s", path)
	}
	raw := decodeGithubContentsPayload(m)
	if len(raw) == 0 {
		return "", ""
	}
	// PORT-NOTE: Python decodes utf-8 errors="replace"; Go replaces invalid
	// runs with U+FFFD via ToValidUTF8.
	return strings.ToValidUTF8(string(raw), "�"), ""
}

// inspectRepoForOnboarding inspects a GitHub repo and returns onboarding
// readiness signals.
func inspectRepoForOnboarding(githubToken, owner, repo string) map[string]any {
	if githubToken == "" || owner == "" || repo == "" {
		return map[string]any{
			"has_github_actions": false,
			"sobs_ci_found":      false,
			"sobs_otel_found":    false,
			"copilot_available":  false,
			"workflow_files":     []string{},
			"error":              "GitHub token or repository not configured",
		}
	}

	// 1. List .github/workflows/
	workflowEntries, workflowError := githubListDirectory(githubToken, owner, repo, ".github/workflows")
	if workflowError != "" && !strings.Contains(" "+workflowError+" ", " 404 ") {
		return map[string]any{
			"has_github_actions": false,
			"sobs_ci_found":      false,
			"sobs_otel_found":    false,
			"copilot_available":  false,
			"workflow_files":     []string{},
			"error":              workflowError,
		}
	}
	workflowFiles := []string{}
	for _, e := range workflowEntries {
		name := rowString(e["name"])
		if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
			workflowFiles = append(workflowFiles, name)
		}
	}
	hasGithubActions := len(workflowFiles) > 0

	// 2. Read workflow file contents and check for Sobs / OTEL indicators.
	sobsCiFound := false
	sobsOtelFound := false
	inspectError := ""
	capped := workflowFiles
	if len(capped) > 10 {
		capped = capped[:10] // cap at 10 to avoid excessive API calls
	}
	for _, filename := range capped {
		content, contentError := githubFileText(githubToken, owner, repo, ".github/workflows/"+filename)
		if contentError != "" && inspectError == "" {
			inspectError = contentError
			continue
		}
		lower := strings.ToLower(content)
		if !sobsCiFound && anyIndicator(lower, sobsCiMetadataIndicators) {
			sobsCiFound = true
		}
		if !sobsOtelFound && anyIndicator(lower, sobsCiOtelIndicators) {
			sobsOtelFound = true
		}
		if sobsCiFound && sobsOtelFound {
			break
		}
	}

	// Also check common manifest/config files for OTEL if not found in workflows.
	if !sobsOtelFound {
		for _, checkPath := range []string{"requirements.txt", "package.json", "go.mod", "pom.xml", "build.gradle"} {
			content, contentError := githubFileText(githubToken, owner, repo, checkPath)
			if contentError != "" && !strings.Contains(" "+contentError+" ", " 404 ") && inspectError == "" {
				inspectError = contentError
			}
			if content != "" && anyIndicator(strings.ToLower(content), sobsCiOtelIndicators) {
				sobsOtelFound = true
				break
			}
		}
	}

	// 3. Check Copilot availability.
	copilotAvailable := githubRepoSupportsCopilotAssignment(githubToken, owner+"/"+repo)

	return map[string]any{
		"has_github_actions": hasGithubActions,
		"sobs_ci_found":      sobsCiFound,
		"sobs_otel_found":    sobsOtelFound,
		"copilot_available":  copilotAvailable,
		"workflow_files":     workflowFiles,
		"error":              inspectError,
	}
}

func anyIndicator(haystack string, indicators []string) bool {
	for _, ind := range indicators {
		if strings.Contains(haystack, ind) {
			return true
		}
	}
	return false
}

// githubGetIssueDetail fetches a single GitHub issue payload; returns an empty
// map on error.
func githubGetIssueDetail(githubToken, githubRepo string, issueNumber int) map[string]any {
	if githubToken == "" || githubRepo == "" || issueNumber <= 0 {
		return map[string]any{}
	}
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return map[string]any{}
	}
	status, body, err := githubRequest(
		"GET",
		fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", owner, repo, issueNumber),
		githubApiHeaders(githubToken, false, nil), nil, 15,
	)
	if err != nil || status >= 400 {
		return map[string]any{}
	}
	if len(body) == 0 {
		return map[string]any{}
	}
	var data any
	if jerr := json.Unmarshal(body, &data); jerr != nil {
		return map[string]any{}
	}
	if m, ok := data.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// githubIssueIsNewState returns true when an issue is still untouched/new from
// an onboarding perspective.
func githubIssueIsNewState(issuePayload map[string]any) bool {
	if issuePayload == nil {
		return false
	}
	state := strings.ToLower(strings.TrimSpace(rowString(issuePayload["state"])))
	comments := coerceInt(issuePayload["comments"])
	createdAt := strings.TrimSpace(rowString(issuePayload["created_at"]))
	updatedAt := strings.TrimSpace(rowString(issuePayload["updated_at"]))
	return state == "open" && comments == 0 && createdAt != "" && createdAt == updatedAt
}

// updateGithubIssueRecord updates an existing GitHub issue and returns
// normalized metadata (or {"error": ...}).
func updateGithubIssueRecord(githubToken, githubRepo string, issueNumber int, title, bodyMd string, labels []string, maskOutputEnabled bool) map[string]any {
	if githubToken == "" || githubRepo == "" || issueNumber <= 0 {
		return map[string]any{}
	}
	owner, repo := parseGithubRepoOwnerName(githubRepo)
	if owner == "" || repo == "" {
		return map[string]any{}
	}

	issueTitle := title
	issueBody := bodyMd
	if maskOutputEnabled {
		issueTitle = maskStringForOutput(title, nil)
		issueBody = maskStringForOutput(bodyMd, nil)
	}
	issuePayload := map[string]any{"title": issueTitle, "body": issueBody}
	if labels != nil {
		issuePayload["labels"] = labels
	}
	payloadJson, merr := json.Marshal(issuePayload)
	if merr != nil {
		logger.Warn("GitHub issue update failed: " + merr.Error())
		return map[string]any{"error": "GitHub issue update failed: " + merr.Error()}
	}

	status, body, err := githubRequest(
		"PATCH",
		fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d", owner, repo, issueNumber),
		githubApiHeaders(githubToken, true, nil), payloadJson, 15,
	)
	if err != nil {
		logger.Warn(fmt.Sprintf("GitHub issue update failed: %v", err))
		return map[string]any{"error": fmt.Sprintf("GitHub issue update failed: %v", err)}
	}
	if status >= 400 {
		detail := ""
		if len(body) > 0 {
			errPayload := map[string]any{}
			if json.Unmarshal(body, &errPayload) == nil {
				detail = strings.TrimSpace(rowString(errPayload["message"]))
			}
		}
		if detail == "" {
			// PORT-NOTE: Python falls back to str(exc); Go reports the status line.
			detail = fmt.Sprintf("HTTP %d", status)
		}
		logger.Warn("GitHub issue update failed: " + detail)
		return map[string]any{"error": "GitHub issue update failed: " + detail}
	}
	result := map[string]any{}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &result)
	}
	issueNum := coerceInt(result["number"])
	if issueNum == 0 {
		issueNum = issueNumber
	}
	resultTitle := rowString(result["title"])
	if resultTitle == "" {
		resultTitle = title
	}
	resultState := rowString(result["state"])
	if resultState == "" {
		resultState = "open"
	}
	return map[string]any{
		"issue_url":    rowString(result["html_url"]),
		"issue_number": issueNum,
		"issue_title":  resultTitle,
		"issue_state":  resultState,
	}
}

// createOrUpdateOnboardingIssue creates an onboarding issue once; updates it
// only when it remains in untouched/new state.
func createOrUpdateOnboardingIssue(githubToken, githubRepo, title, bodyMd string, labels []string) map[string]any {
	openIssues := fetchOpenGithubIssues(githubToken, githubRepo)
	titleNorm := strings.TrimSpace(title)
	var existing map[string]any
	for _, item := range openIssues {
		if strings.TrimSpace(rowString(item["issue_title"])) == titleNorm {
			existing = item
			break
		}
	}

	if existing == nil {
		created := createGithubIssueRecord(githubToken, githubRepo, title, bodyMd, labels, false)
		if _, hasErr := created["error"]; hasErr {
			return created
		}
		created["status"] = "created"
		created["note"] = "Created a new onboarding issue."
		return created
	}

	issueNumber := coerceInt(existing["issue_number"])
	issueUrl := rowString(existing["issue_url"])
	detail := githubGetIssueDetail(githubToken, githubRepo, issueNumber)

	if len(detail) > 0 && githubIssueIsNewState(detail) {
		updated := updateGithubIssueRecord(githubToken, githubRepo, issueNumber, title, bodyMd, labels, false)
		if _, hasErr := updated["error"]; hasErr {
			return updated
		}
		updated["status"] = "updated"
		updated["note"] = "Updated the existing onboarding issue because it was still new."
		return updated
	}

	existingState := rowString(detail["state"])
	if existingState == "" {
		existingState = rowString(existing["issue_state"])
	}
	if existingState == "" {
		existingState = "open"
	}
	detailUrl := rowString(detail["html_url"])
	if detailUrl == "" {
		detailUrl = issueUrl
	}
	detailTitle := rowString(detail["title"])
	if detailTitle == "" {
		detailTitle = rowString(existing["issue_title"])
	}
	if detailTitle == "" {
		detailTitle = title
	}
	return map[string]any{
		"issue_url":    detailUrl,
		"issue_number": issueNumber,
		"issue_title":  detailTitle,
		"issue_state":  existingState,
		"status":       "reused",
		"note":         "Existing onboarding issue is not in new state; left unchanged.",
	}
}

// buildCiMetadataIssueBody builds the Markdown body for the Sobs CI metadata
// setup GitHub issue.
func buildCiMetadataIssueBody(owner, repo string, hasGithubActions bool) string {
	ciSection := "\n## CI Provider\n\nNo GitHub Actions workflows were detected. The steps below are provider-agnostic and can\nbe adapted for Jenkins, CircleCI, GitLab CI, Buildkite, or other CI systems.\n"
	if hasGithubActions {
		ciSection = "\n## CI Provider\n\nThis repository uses **GitHub Actions**. Use polling mode first, then optionally add\nrealtime push once security approval for outbound CI calls is in place.\n"
	}

	body := strings.Join([]string{
		"# Sobs CI Metadata Setup",
		"",
		"This issue defines how `%s/%s` should integrate with Sobs CI metadata.",
		"",
		"Sobs supports two modes:",
		"",
		"1. **Polling mode (default)**",
		"     - No CI workflow edits required.",
		"    - Sobs reads GitHub run/check state and uses conditional requests",
		"      (`ETag`/`If-None-Match`) to keep polling efficient.",
		"     - Best starting point when CI outbound calls require security approval.",
		"",
		"2. **Realtime push mode (optional)**",
		"     - CI posts release metadata directly to Sobs with a Sobs API key.",
		"     - Faster and deterministic release visibility.",
		"     - Optional GitHub webhook can be added for faster refresh triggers.",
		"",
		"> Keep polling mode available as fallback even if realtime push is enabled.",
		"",
		"%s",
		"",
		"---",
		"",
		"## Step 1 - Baseline repository setup in Sobs",
		"",
		"- Verify repository URL in **Settings -> Repositories**",
		"- Verify GitHub token is valid for read operations",
		"- Verify token expiry tracking is configured",
		"",
		"---",
		"",
		"## Step 2 - Polling mode (no CI changes)",
		"",
		"No workflow updates are required for this step.",
		"",
		"- Confirm Sobs can read workflow/check state for this repo",
		"- Confirm Sobs conditional polling is enabled and stable",
		"- Confirm CVE/release views continue to populate",
		"",
		"---",
		"",
		"## Step 3 - Register a release (optional realtime push mode)",
		"",
		"If CI outbound integration is approved, add these CI secrets:",
		"",
		"| Secret | Description |",
		"|--------|-------------|",
		"| `SOBS_URL` | Base URL of your Sobs instance (for example `https://sobs.internal`) |",
		"| `SOBS_INGEST_API_KEY` | Sobs ingest API key from Settings -> Repositories |",
		"| `SOBS_APP_ID` | Application ID from Settings -> Repositories |",
		"",
		"Use this push call in CI:",
		"",
		"```bash",
		"curl -sS -X POST \"${SOBS_URL}/v1/apps/${SOBS_APP_ID}/releases\" \\",
		"        -H \"X-API-Key: ${SOBS_INGEST_API_KEY}\" \\",
		"        -H \"Content-Type: application/json\" \\",
		"        -d '{",
		"                \"version\":    \"${VERSION}\",",
		"                \"commitSha\":  \"${COMMIT_SHA}\",",
		"                \"buildId\":    \"${BUILD_ID}\",",
		"                \"environment\": \"production\"",
		"        }'",
		"```",
		"",
		"Best practice requirements for release identity:",
		"",
		"- Use a release `version` that exactly matches deployed runtime identity (for example image tag or Git tag).",
		"- Keep `commitSha` and `buildId` immutable per published release.",
		"- Propagate the same release identifier into OTEL `service.version` so Sobs can",
		"    correlate CVEs to observed runtime activity.",
		"- For containerized workloads, include image digest/tag in release metadata where available.",
		"",
		"---",
		"",
		"## Step 4 - Upload dependency lockfile metadata",
		"",
		"Lockfile metadata improves release-scoped CVE enrichment. Best practice is to",
		"extract resolved dependency snapshots from the built container image for each",
		"target architecture (for example linux/amd64 and linux/arm64), then register",
		"each snapshot with provenance fields (size/checksum/storageRef/platform/architecture):",
		"",
		"For GitHub Actions, prefer a visible artifact directory/path for dependency",
		"snapshots (for example `sobs-release/pip-freeze-linux-amd64.txt`). Hidden",
		"directories such as `.sobs-release/` are excluded by `actions/upload-artifact`",
		"unless `include-hidden-files: true` is set explicitly.",
		"",
		"```bash",
		"curl -sS -X POST \"${SOBS_URL}/v1/releases/${RELEASE_ID}/artifacts/meta\" \\",
		"        -H \"X-API-Key: ${SOBS_INGEST_API_KEY}\" \\",
		"        -H \"Content-Type: application/json\" \\",
		"        -d '{",
		"                \"artifactType\": \"dependencies-lockfile\",",
		"                                \"name\": \"pip-freeze-linux-amd64\",",
		"                                \"contentType\": \"application/json\",",
		"                                \"size\": ${LOCKFILE_SIZE},",
		"                                \"storageRef\": \"ci://artifacts/pip-freeze-linux-amd64.txt\",",
		"                                \"checksumSha256\": \"${LOCKFILE_SHA256}\",",
		"                                \"platform\": \"linux\",",
		"                                \"architecture\": \"amd64\",",
		"                                \"metadata\": {",
		"                                    \"dependencies\": ${RESOLVED_DEPS_JSON}",
		"                                }",
		"        }'",
		"```",
		"",
		"Repeat per architecture (for example `pip-freeze-linux-arm64`) to ensure CVE",
		"tracking reflects what is actually shipped for each target platform.",
		"",
		"Dependency capture requirements:",
		"",
		"- Derive snapshots from the built/published container image, not from a host-only",
		"    resolver run.",
		"- Track per-arch snapshots independently for multi-arch releases.",
		"- Fail CI early if any expected dependency snapshot file is missing or empty",
		"    before artifact upload and metadata registration.",
		"- Verify the dependency snapshot artifact upload succeeds before release/artifact",
		"    registration continues.",
		"- Include provenance fields (`storageRef`, `checksumSha256`, `size`, `platform`,",
		"  `architecture`) on every dependency artifact.",
		"",
		"---",
		"",
		"## Step 5 - Upload JS source maps (web front-end only)",
		"",
		"Source maps let Sobs resolve minified stack traces to original source locations:",
		"",
		"```bash",
		"curl -sS -X POST \"${SOBS_URL}/v1/releases/${RELEASE_ID}/artifacts/meta\" \\",
		"    -H \"X-API-Key: ${SOBS_INGEST_API_KEY}\" \\",
		"    -H \"Content-Type: application/json\" \\",
		"    -d '{",
		"        \"artifactType\": \"js_sourcemap\",",
		"        \"name\": \"app.min.js.map\",",
		"        \"contentType\": \"application/json\",",
		"        \"size\": ${SOURCEMAP_SIZE},",
		"        \"checksumSha256\": \"${SOURCEMAP_SHA256}\",",
		"        \"storageRef\": \"ci://artifacts/app.min.js.map\"",
		"    }'",
		"```",
		"",
		"Source map capture requirements:",
		"",
		"- Register maps from the same build outputs that were deployed.",
		"- Include `size` and `checksumSha256` for provenance and troubleshooting.",
		"",
		"---",
		"",
		"## Step 6 - Optional webhook acceleration",
		"",
		"If repository admins approve webhook setup, add a GitHub webhook to Sobs for push/workflow events.",
		"",
		"- This is optional and should not block onboarding.",
		"- Admin/webhook-write permissions are usually required.",
		"- Keep polling mode enabled as fallback.",
		"",
		"---",
		"",
		"## Step 7 - Trigger a CVE scan (optional)",
		"",
		"```bash",
		"curl -sS -X POST \"${SOBS_URL}/api/enrichment/cve/scan\" \\",
		"        -H \"X-API-Key: ${SOBS_INGEST_API_KEY}\" \\",
		"        -H \"Content-Type: application/json\" \\",
		"        -d '{}'",
		"```",
		"",
		"---",
		"",
		"## Step 8 - OTEL-linked CVE impact triage",
		"",
		"Use CVE results together with OTEL/log evidence to separate:",
		"",
		"- **Confirmed impact candidates**: vulnerable package/version appears in release",
		"    metadata and related services show active OTEL/log usage for that runtime.",
		"- **Latent exposure**: vulnerable package/version exists in release metadata but no",
		"    current OTEL/log evidence of active usage.",
		"",
		"This lets teams prioritize \"must patch now\" findings while still tracking latent risk.",
		"",
		"Recommended correlation keys:",
		"",
		"- `service.name`",
		"- `service.version` (must match the registered release version)",
		"- `deployment.environment`",
		"- release metadata (`version`, `commitSha`, `buildId`, image tag/digest)",
		"",
		"---",
		"",
		"## Manual verification checklist",
		"",
		"- Confirm first pushed release appears in Sobs",
		"- Confirm lockfile artifact metadata is visible for each architecture",
		"- Confirm dependency snapshot artifacts upload successfully from non-hidden CI paths",
		"- Confirm dependency artifacts include provenance fields (size/checksum/storageRef/platform/architecture)",
		"- Confirm release version matches OTEL `service.version`",
		"- Confirm CVE findings reflect the container-derived dependency snapshots",
		"- Confirm CVE review distinguishes confirmed impact candidates vs latent exposure",
		"- Confirm polling-only fallback works if CI push or webhook path is blocked",
		"",
		"---",
		"",
		"*This issue was created automatically by the Sobs Onboarding Wizard for repository " +
			"`%s/%s`.*",
	}, "\n") + "\n"

	return fmt.Sprintf(body, owner, repo, ciSection, owner, repo)
}

// buildOtelAuditIssueBody builds the Markdown body for the OTEL & RUM telemetry
// audit GitHub issue.
func buildOtelAuditIssueBody(owner, repo string) string {
	body := strings.Join([]string{
		"# OTEL & RUM Telemetry Audit",
		"",
		"This issue requests a comprehensive audit of the `%s/%s` repository to identify",
		"gaps in observability coverage and add best-practice OpenTelemetry (OTEL) instrumentation,",
		"Real User Monitoring (RUM), and AI telemetry.",
		"",
		"---",
		"",
		"## Audit Checklist",
		"",
		"### 1. Core OTEL SDK Setup",
		"",
		"- [ ] Install and configure the OTEL SDK for the primary language(s) used in this repository",
		"- [ ] Set up a `TracerProvider` with OTLP export pointing to Sobs (`<SOBS_URL>:4317`)",
		"- [ ] Set up a `LoggerProvider` (or bridge) so structured application logs flow through OTEL",
		"- [ ] Set up a `MeterProvider` for custom metrics (request counts, error rates, latency histograms)",
		"- [ ] Ensure `service.name`, `service.version`, and `deployment.environment` resource attributes",
		"      are set",
		"",
		"**Example (Python):**",
		"```python",
		"from opentelemetry import trace",
		"from opentelemetry.sdk.trace import TracerProvider",
		"from opentelemetry.sdk.trace.export import BatchSpanProcessor",
		"from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter",
		"",
		"provider = TracerProvider(",
		"    resource=Resource({\"service.name\": \"my-service\", \"service.version\": \"1.0.0\"})",
		")",
		"provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=\"http://sobs:4317\")))",
		"trace.set_tracer_provider(provider)",
		"```",
		"",
		"---",
		"",
		"### 2. Web Front-End — RUM Snippet (if applicable)",
		"",
		"If this repository contains a web front-end (HTML, React, Vue, Angular, etc.):",
		"",
		"- [ ] Add the Sobs RUM snippet to the `<head>` of every page (or the root layout component)",
		"- [ ] Configure RUM to capture **console logs**, **JavaScript stack traces**, **navigation",
		"      breadcrumbs**, **Web Vitals** (LCP, CLS, INP, TTFB, FCP), **screenshots** (on error),",
		"      and **session replays**",
		"- [ ] Set `service`, `environment`, and `release` attributes in the RUM config",
		"",
		"**Sobs RUM snippet:**",
		"```html",
		"<script>",
		"  window.SobsRumConfig = {",
		"    endpoint: '<SOBS_URL>/rum',",
		"    service:  'my-frontend',",
		"    env:      'production',",
		"    release:  '{{ APP_VERSION }}',",
		"    captureConsole: true,",
		"    captureErrors:  true,",
		"    captureReplays: true,",
		"    captureScreenshots: true",
		"  };",
		"</script>",
		"<script src=\"<SOBS_URL>/static/rum.min.js\"></script>",
		"```",
		"",
		"---",
		"",
		"### 3. AI / LLM Workloads (if applicable)",
		"",
		"If this repository makes LLM API calls (OpenAI, Anthropic, Azure OpenAI, etc.):",
		"",
		"- [ ] Use `opentelemetry-instrumentation-openai` (or equivalent) to auto-instrument LLM calls",
		"- [ ] Emit OTEL `gen_ai.*` semantic-convention attributes on every LLM span:",
		"      `gen_ai.system`, `gen_ai.request.model`, `gen_ai.usage.input_tokens`,",
		"      `gen_ai.usage.output_tokens`",
		"- [ ] Propagate trace context into LLM calls so the Sobs AI page can correlate prompts with",
		"      application traces",
		"- [ ] Record prompt templates and response hashes (not full content) as span attributes for",
		"      traceability",
		"- [ ] Ensure no PII / secrets are emitted in span attributes",
		"",
		"---",
		"",
		"### 4. Infrastructure & Web Logs (if applicable)",
		"",
		"For infrastructure services (proxies, gateways, databases, queues):",
		"",
		"- [ ] Add OTEL log bridge or structured JSON logging shipped via OTLP to Sobs",
		"- [ ] Include `http.method`, `http.route`, `http.status_code`, `net.peer.ip` attributes",
		"      for HTTP services",
		"- [ ] For databases: include `db.system`, `db.statement` (redacted), `db.name` span attributes",
		"- [ ] For message queues: include `messaging.system`, `messaging.destination` span attributes",
		"",
		"---",
		"",
		"### 5. Error & Exception Capture",
		"",
		"- [ ] Call `span.record_exception(exc)` and `span.set_status(StatusCode.ERROR)` in all",
		"      exception handlers",
		"- [ ] Ensure unhandled exceptions are captured and forwarded to the Sobs errors endpoint",
		"- [ ] Add a global uncaught-exception handler that emits a final error span before process exit",
		"",
		"---",
		"",
		"### 6. Telemetry Verification",
		"",
		"After implementing the above:",
		"",
		"- [ ] Verify traces appear on the Sobs **Traces** page",
		"- [ ] Verify logs appear on the Sobs **Logs** page",
		"- [ ] Verify metrics appear on the Sobs **Metrics** page",
		"- [ ] Verify RUM events appear on the Sobs **RUM** page (if web front-end added)",
		"- [ ] Verify AI calls appear on the Sobs **AI** page (if LLM workload added)",
		"- [ ] Run the CVE scan and verify findings appear on the Sobs **CVE** page",
		"",
		"---",
		"",
		"## What remains manual",
		"",
		"- Reviewing each checklist item and confirming it applies to this repository's technology stack",
		"- Testing that telemetry flows correctly end-to-end",
		"- Removing any accidentally captured PII or secrets from span attributes",
		"",
		"---",
		"",
		"*This issue was created automatically by the Sobs Onboarding Wizard for repository " +
			"`%s/%s`.*",
	}, "\n") + "\n"

	return fmt.Sprintf(body, owner, repo, owner, repo)
}

// apiOnboardingCreateRepo creates a repository entry for the onboarding wizard
// and returns JSON details.
func apiOnboardingCreateRepo(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	body, _ := readJsonBody(r)

	name := strings.TrimSpace(rowString(body["name"]))
	slugRaw := strings.TrimSpace(rowString(body["slug"]))
	repoUrlInput := strings.TrimSpace(rowString(body["repo_url"]))
	repoOwnerInput := strings.TrimSpace(rowString(body["repo_owner"]))
	repoNameInput := strings.TrimSpace(rowString(body["repo_name"]))
	repoUrl, owner, repo := resolveGithubRepoFields(repoUrlInput, repoOwnerInput, repoNameInput)
	defaultEnvironment := strings.TrimSpace(rowString(body["default_environment"]))
	githubToken := strings.TrimSpace(rowString(body["github_token"]))
	githubTokenExpiry := normalizeGithubTokenExpiryInput(rowString(body["github_token_expires_at"]))
	setGithubToken := parseBool(body["set_github_token"], false)
	setRepoToken := parseBool(body["set_repo_token"], true)
	setAgentRepo := parseBool(body["set_agent_repo"], true)

	if name == "" || repoUrl == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "App name and repository are required"})
		return
	}

	slugInput := slugRaw
	if slugInput == "" {
		slugInput = name
	}
	slug := appSlug(slugInput)
	res, err := db.Execute(
		"SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 LIMIT 1",
		slug,
	)
	if err == nil && res.Fetchone() != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"ok": false, "error": "App slug already exists"})
		return
	}

	version := time.Now().UnixMilli()
	row := Row{
		"Id":                 uuid4Hex(),
		"Name":               name,
		"Slug":               slug,
		"OwnerTeam":          "",
		"RepoUrl":            repoUrl,
		"DefaultEnvironment": defaultEnvironment,
		"Enabled":            1,
		"MetadataJson":       "{}",
		"IsDeleted":          0,
		"Version":            version,
		"CreatedAt":          nowIso(),
		"UpdatedAt":          nowIso(),
	}
	_, _ = insertRowsJsonEachRow(db, "sobs_apps", []Row{row})

	if setGithubToken && githubToken != "" {
		saveAiSetting(db, "ai.github_token", githubToken)
		saveAiSetting(db, "ai.github_token_expires_at", githubTokenExpiry)
		saveAiSetting(db, "ai.github_token_last_validated_at", "")
		saveAiSetting(db, "ai.github_token_last_validation_status", "")
		saveAiSetting(db, "ai.github_token_last_validation_message", "")
	}

	if setRepoToken && githubToken != "" && owner != "" && repo != "" {
		saveRepoScopedGithubToken(db, owner, repo, githubToken)
	}

	if setAgentRepo && owner != "" && repo != "" {
		saveAiSetting(db, "ai.github_repo", fmt.Sprintf("%s/%s", owner, repo))
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":       true,
		"app_id":   rowString(row["Id"]),
		"name":     name,
		"slug":     slug,
		"repo_url": repoUrl,
		"owner":    owner,
		"repo":     repo,
	})
}

// apiOnboardingImportRepo fetches repository metadata from GitHub for onboarding
// form auto-fill.
func apiOnboardingImportRepo(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	body, _ := readJsonBody(r)

	repoUrlInput := strings.TrimSpace(rowString(body["repo_url"]))
	repoOwnerInput := strings.TrimSpace(rowString(body["repo_owner"]))
	repoNameInput := strings.TrimSpace(rowString(body["repo_name"]))
	_, owner, repo := resolveGithubRepoFields(repoUrlInput, repoOwnerInput, repoNameInput)
	tokenOverride := strings.TrimSpace(rowString(body["github_token"]))

	if owner == "" || repo == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Enter a valid GitHub owner and repository name"})
		return
	}

	githubToken := tokenOverride
	if githubToken == "" {
		githubToken = strings.TrimSpace(loadAiSetting(db, "ai.github_token", ""))
	}
	var headers map[string]string
	if githubToken != "" {
		headers = githubApiHeaders(githubToken, false, nil)
	} else {
		headers = map[string]string{
			"Accept":               "application/vnd.github+json",
			"X-GitHub-Api-Version": "2022-11-28",
		}
	}

	status, respBody, err := githubRequest(
		"GET",
		fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo),
		headers, nil, 15,
	)
	if err != nil {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("GitHub lookup failed: %v", err)})
		return
	}
	var payloadAny any = map[string]any{}
	if len(respBody) > 0 {
		if jerr := json.Unmarshal(respBody, &payloadAny); jerr != nil {
			jsonResponse(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("GitHub lookup failed: %v", jerr)})
			return
		}
	}

	payload, isMap := payloadAny.(map[string]any)
	if status != 200 {
		detail := ""
		if isMap {
			detail = strings.TrimSpace(rowString(payload["message"]))
		}
		if detail == "" {
			detail = fmt.Sprintf("GitHub lookup failed (%d)", status)
		}
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": detail})
		return
	}

	if !isMap {
		jsonResponse(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "Unexpected GitHub response payload"})
		return
	}

	fullName := strings.TrimSpace(rowString(payload["full_name"]))
	if fullName == "" {
		fullName = fmt.Sprintf("%s/%s", owner, repo)
	}
	importedRepoUrl := strings.TrimSpace(rowString(payload["html_url"]))
	if importedRepoUrl == "" {
		importedRepoUrl = fmt.Sprintf("https://github.com/%s/%s", owner, repo)
	}
	suggestedName := strings.TrimSpace(rowString(payload["name"]))
	if suggestedName == "" {
		suggestedName = repo
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":             true,
		"owner":          owner,
		"repo":           repo,
		"full_name":      fullName,
		"repo_url":       importedRepoUrl,
		"name":           suggestedName,
		"slug":           appSlug(suggestedName),
		"default_branch": rowString(payload["default_branch"]),
		"visibility":     rowStringOr(payload["visibility"], "public"),
		"description":    rowString(payload["description"]),
	})
}

// apiOnboardingListRepos lists repositories for an owner/user to support
// onboarding autocomplete.
func apiOnboardingListRepos(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	body, _ := readJsonBody(r)

	owner := strings.Trim(strings.TrimSpace(rowString(body["owner"])), "/")
	tokenOverride := strings.TrimSpace(rowString(body["github_token"]))
	if owner == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Owner or username is required"})
		return
	}

	githubToken := tokenOverride
	if githubToken == "" {
		githubToken = strings.TrimSpace(loadAiSetting(db, "ai.github_token", ""))
	}
	tokenUsed := githubToken != ""
	var headers map[string]string
	if githubToken != "" {
		headers = githubApiHeaders(githubToken, false, nil)
	} else {
		headers = map[string]string{
			"Accept":               "application/vnd.github+json",
			"X-GitHub-Api-Version": "2022-11-28",
		}
	}

	endpoints := []string{}
	if tokenUsed {
		endpoints = append(endpoints, fmt.Sprintf("https://api.github.com/users/%s/repos?per_page=100&type=all&sort=full_name", owner))
		endpoints = append(endpoints, fmt.Sprintf("https://api.github.com/orgs/%s/repos?per_page=100&type=all&sort=full_name", owner))
	} else {
		endpoints = append(endpoints, fmt.Sprintf("https://api.github.com/users/%s/repos?per_page=100&type=public&sort=full_name", owner))
		endpoints = append(endpoints, fmt.Sprintf("https://api.github.com/orgs/%s/repos?per_page=100&type=public&sort=full_name", owner))
	}

	var payloadAny any
	responseStatus := 0
	for _, urlStr := range endpoints {
		status, respBody, err := githubRequest("GET", urlStr, headers, nil, 15)
		if err != nil {
			jsonResponse(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("GitHub lookup failed: %v", err)})
			return
		}
		responseStatus = status
		payloadAny = nil
		if len(respBody) > 0 {
			_ = json.Unmarshal(respBody, &payloadAny)
		}
		if responseStatus == 200 {
			break
		}
	}

	payloadList, isList := payloadAny.([]any)
	if responseStatus != 200 || !isList {
		detail := ""
		if m, ok := payloadAny.(map[string]any); ok {
			detail = strings.TrimSpace(rowString(m["message"]))
		}
		if detail == "" {
			detail = fmt.Sprintf("GitHub lookup failed (%d)", responseStatus)
		}
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": detail})
		return
	}

	repos := []map[string]any{}
	for _, itemAny := range payloadList {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		repoName := strings.TrimSpace(rowString(item["name"]))
		if repoName == "" {
			continue
		}
		repoOwner := owner
		if ownerMap, ok := item["owner"].(map[string]any); ok {
			if login := strings.TrimSpace(rowString(ownerMap["login"])); login != "" {
				repoOwner = login
			}
		}
		fullName := strings.TrimSpace(rowString(item["full_name"]))
		if fullName == "" {
			fullName = fmt.Sprintf("%s/%s", repoOwner, repoName)
		}
		repoUrl := strings.TrimSpace(rowString(item["html_url"]))
		if repoUrl == "" {
			repoUrl = buildGithubRepoUrl(repoOwner, repoName)
		}
		repos = append(repos, map[string]any{
			"name":      repoName,
			"full_name": fullName,
			"repo_url":  repoUrl,
			"private":   parseBool(item["private"], false),
		})
	}

	sort.SliceStable(repos, func(i, j int) bool {
		return strings.ToLower(rowString(repos[i]["name"])) < strings.ToLower(rowString(repos[j]["name"]))
	})

	visibilityNote := ""
	if !tokenUsed {
		visibilityNote = "Need PAT to see private repositories."
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":              true,
		"owner":           owner,
		"repos":           repos,
		"token_used":      tokenUsed,
		"visibility_note": visibilityNote,
	})
}

// apiOnboardingInspectRepo inspects a configured repository for Sobs onboarding
// readiness.
//
// Query parameters:
//
//	app_id   UUID of the app in sobs_apps (preferred)
//	repo     owner/repo or full GitHub URL (fallback if app_id not provided)
func apiOnboardingInspectRepo(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	appId := strings.TrimSpace(r.URL.Query().Get("app_id"))
	repoParam := strings.TrimSpace(r.URL.Query().Get("repo"))

	repoUrl := ""
	if appId != "" {
		res, err := db.Execute(
			"SELECT RepoUrl FROM sobs_apps FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1",
			appId,
		)
		var row Row
		if err == nil {
			row = res.Fetchone()
		}
		if row == nil {
			jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "App not found"})
			return
		}
		repoUrl = rowString(row[res.Cols[0]])
	} else if repoParam != "" {
		repoUrl = repoParam
	} else {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "app_id or repo parameter required"})
		return
	}

	owner, repo := parseGithubRepoOwnerName(repoUrl)
	if owner == "" || repo == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("Could not parse owner/repo from '%s'", repoUrl)})
		return
	}

	// Resolve token: repo-scoped first, then global fallback.
	githubToken := loadRepoScopedGithubToken(db, owner, repo)
	if githubToken == "" {
		githubToken = strings.TrimSpace(loadAiSetting(db, "ai.github_token", ""))
	}
	if githubToken == "" {
		jsonResponse(w, http.StatusOK, map[string]any{
			"ok":                 true,
			"owner":              owner,
			"repo":               repo,
			"has_github_actions": false,
			"sobs_ci_found":      false,
			"sobs_otel_found":    false,
			"copilot_available":  false,
			"workflow_files":     []string{},
			"error":              "No GitHub token configured for this repository",
		})
		return
	}

	result := inspectRepoForOnboarding(githubToken, owner, repo)
	response := map[string]any{"ok": true, "owner": owner, "repo": repo}
	for k, v := range result {
		response[k] = v
	}
	jsonResponse(w, http.StatusOK, response)
}

// apiOnboardingCreateIssues creates onboarding GitHub issues (CI metadata
// and/or OTEL audit).
//
// JSON body:
//
//	app_id                   UUID of the app in sobs_apps
//	repo                     owner/repo fallback if app_id not provided
//	create_ci                bool — create CI metadata setup issue
//	create_otel              bool — create OTEL & RUM audit issue
//	assign_copilot           bool — attempt to assign both issues to Copilot
//	has_github_actions       bool — passed from inspection result (affects issue body)
//	enable_realtime_support  bool — include manual realtime CI setup guidance and key state
func apiOnboardingCreateIssues(w http.ResponseWriter, r *http.Request) {
	db := getDb()
	body, _ := readJsonBody(r)

	appId := strings.TrimSpace(rowString(body["app_id"]))
	repoParam := strings.TrimSpace(rowString(body["repo"]))
	createCi := parseBool(body["create_ci"], true)
	createOtel := parseBool(body["create_otel"], true)
	assignCopilot := parseBool(body["assign_copilot"], false)
	hasGithubActions := parseBool(body["has_github_actions"], true)
	enableRealtimeSupport := parseBool(body["enable_realtime_support"], false)

	if !createCi && !createOtel && !enableRealtimeSupport {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Select at least one issue type or enable realtime support"})
		return
	}

	repoUrl := ""
	if appId != "" {
		res, err := db.Execute(
			"SELECT RepoUrl FROM sobs_apps FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1",
			appId,
		)
		var row Row
		if err == nil {
			row = res.Fetchone()
		}
		if row == nil {
			jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "App not found"})
			return
		}
		repoUrl = rowString(row[res.Cols[0]])
	} else if repoParam != "" {
		repoUrl = repoParam
	} else {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "app_id or repo parameter required"})
		return
	}

	owner, repo := parseGithubRepoOwnerName(repoUrl)
	if owner == "" || repo == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": fmt.Sprintf("Could not parse owner/repo from '%s'", repoUrl)})
		return
	}

	githubToken := loadRepoScopedGithubToken(db, owner, repo)
	if githubToken == "" {
		githubToken = strings.TrimSpace(loadAiSetting(db, "ai.github_token", ""))
	}
	if githubToken == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "No GitHub token configured for this repository"})
		return
	}

	githubRepo := fmt.Sprintf("%s/%s", owner, repo)
	results := map[string]any{"ok": true, "ci_issue": nil, "otel_issue": nil, "realtime": nil}

	if enableRealtimeSupport {
		realtimeAppId := strings.TrimSpace(appId)
		if realtimeAppId == "" && repoUrl != "" {
			realtimeAppId, _ = findAppIdByRepoUrl(db, repoUrl)
		}

		if realtimeAppId == "" {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "Realtime support requires a saved repository app."})
			return
		}

		keyPlain := ""
		keyStatus := ciPushApiKeyStatus(db, realtimeAppId)
		expiryState := ""
		if exp, ok := keyStatus["expiry"].(map[string]any); ok {
			expiryState = rowString(exp["state"])
		}
		if !parseBool(keyStatus["configured"], false) || expiryState == "expired" {
			keyPlain, _ = rotateCiPushApiKey(db, realtimeAppId, ciPushApiKeyDefaultTtlDays)
			keyStatus = ciPushApiKeyStatus(db, realtimeAppId)
		}
		setCiPushRealtimeEnabled(db, realtimeAppId, true)
		appIdForExample := realtimeAppId
		if appIdForExample == "" {
			appIdForExample = "<APP_ID>"
		}
		expiryStateOut := "unknown"
		expiryMessage := ""
		if exp, ok := keyStatus["expiry"].(map[string]any); ok {
			if s := rowString(exp["state"]); s != "" {
				expiryStateOut = s
			}
			expiryMessage = rowString(exp["message"])
		}
		results["realtime"] = map[string]any{
			"app_id":            realtimeAppId,
			"enabled":           true,
			"configured":        parseBool(keyStatus["configured"], false),
			"expires_at":        rowString(keyStatus["expires_at"]),
			"expiry_state":      expiryStateOut,
			"expiry_message":    expiryMessage,
			"api_key":           keyPlain,
			"api_key_show_once": keyPlain != "",
			"instructions": map[string]any{
				"required_secrets": []string{"SOBS_URL", "SOBS_INGEST_API_KEY", "SOBS_APP_ID"},
				"curl_example": fmt.Sprintf(
					"curl -sS -X POST '$SOBS_URL/v1/apps/%s/releases' "+
						"-H 'X-API-Key: $SOBS_INGEST_API_KEY' "+
						"-H 'Content-Type: application/json' "+
						"-d '{\"version\":\"$VERSION\",\"commitSha\":\"$COMMIT_SHA\",\"buildId\":\"$BUILD_ID\"}'",
					appIdForExample,
				),
				"webhook_note": "Optional: add a GitHub webhook for push/workflow events to reduce polling latency.",
			},
		}
	}

	if createCi {
		ciBody := buildCiMetadataIssueBody(owner, repo, hasGithubActions)
		ciResult := createOrUpdateOnboardingIssue(
			githubToken,
			githubRepo,
			fmt.Sprintf("[Sobs] Set up CI metadata scripts for %s", repo),
			ciBody,
			[]string{"sobs-onboarding", "ci-metadata"},
		)
		if _, hasErr := ciResult["error"]; hasErr {
			results["ci_issue"] = map[string]any{"error": ciResult["error"]}
		} else {
			issueUrl := rowString(ciResult["issue_url"])
			issueNumber := coerceInt(ciResult["issue_number"])
			issueStatus := rowString(ciResult["status"])
			issueNote := rowString(ciResult["note"])
			copilotAssignmentStatus := "not_requested"
			copilotAssignmentReason := ""
			var copilotAssignmentRequestedAt int64 = 0
			if assignCopilot && issueNumber != 0 {
				copilotAssignmentStatus, copilotAssignmentReason, copilotAssignmentRequestedAt =
					assignIssueToCopilot(githubToken, githubRepo, issueNumber, "", "")
			}
			results["ci_issue"] = map[string]any{
				"url":                             issueUrl,
				"number":                          issueNumber,
				"status":                          issueStatus,
				"note":                            issueNote,
				"copilot_status":                  copilotAssignmentStatus,
				"copilot_assignment_status":       copilotAssignmentStatus,
				"copilot_assignment_reason":       copilotAssignmentReason,
				"copilot_assignment_requested_at": copilotAssignmentRequestedAt,
			}
			if issueStatus == "created" || issueStatus == "updated" {
				issueTitle := rowString(ciResult["issue_title"])
				if issueTitle == "" {
					issueTitle = fmt.Sprintf("[Sobs] Set up CI metadata scripts for %s", repo)
				}
				issueState := rowString(ciResult["issue_state"])
				if issueState == "" {
					issueState = "open"
				}
				persistOnboardingWorkItem(
					db, githubRepo, issueUrl, issueNumber, issueTitle, issueState,
					issueStatus, issueNote, copilotAssignmentStatus, copilotAssignmentReason,
					int(copilotAssignmentRequestedAt), "ci",
				)
			}
		}
	}

	if createOtel {
		otelBody := buildOtelAuditIssueBody(owner, repo)
		otelResult := createOrUpdateOnboardingIssue(
			githubToken,
			githubRepo,
			fmt.Sprintf("[Sobs] OTEL & RUM telemetry audit for %s", repo),
			otelBody,
			[]string{"sobs-onboarding", "observability"},
		)
		if _, hasErr := otelResult["error"]; hasErr {
			results["otel_issue"] = map[string]any{"error": otelResult["error"]}
		} else {
			issueUrl := rowString(otelResult["issue_url"])
			issueNumber := coerceInt(otelResult["issue_number"])
			issueStatus := rowString(otelResult["status"])
			issueNote := rowString(otelResult["note"])
			copilotAssignmentStatus := "not_requested"
			copilotAssignmentReason := ""
			var copilotAssignmentRequestedAt int64 = 0
			if assignCopilot && issueNumber != 0 {
				copilotAssignmentStatus, copilotAssignmentReason, copilotAssignmentRequestedAt =
					assignIssueToCopilot(githubToken, githubRepo, issueNumber, "", "")
			}
			results["otel_issue"] = map[string]any{
				"url":                             issueUrl,
				"number":                          issueNumber,
				"status":                          issueStatus,
				"note":                            issueNote,
				"copilot_status":                  copilotAssignmentStatus,
				"copilot_assignment_status":       copilotAssignmentStatus,
				"copilot_assignment_reason":       copilotAssignmentReason,
				"copilot_assignment_requested_at": copilotAssignmentRequestedAt,
			}
			if issueStatus == "created" || issueStatus == "updated" {
				issueTitle := rowString(otelResult["issue_title"])
				if issueTitle == "" {
					issueTitle = fmt.Sprintf("[Sobs] OTEL & RUM telemetry audit for %s", repo)
				}
				issueState := rowString(otelResult["issue_state"])
				if issueState == "" {
					issueState = "open"
				}
				persistOnboardingWorkItem(
					db, githubRepo, issueUrl, issueNumber, issueTitle, issueState,
					issueStatus, issueNote, copilotAssignmentStatus, copilotAssignmentReason,
					int(copilotAssignmentRequestedAt), "observability",
				)
			}
		}
	}

	jsonResponse(w, http.StatusOK, results)
}
