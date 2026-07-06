package main

// Unit tests for read-only / security-relevant pure helpers:
//   - validateSql + buildQueryAllowedTables (TestNlqGuard, s17_vanna.go)
//   - buildS3BackupDest + parseDmPrunePeriod (s19_datamgmt_wizard.go)
//   - parseOssSafeguardReply (s04_llm_guard.go)

import (
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// SQL allowlist validation
// ---------------------------------------------------------------------------

func TestValidateSqlAllowsReadOnlyTables(t *testing.T) {
	allowed := []string{
		"SELECT Timestamp FROM otel_logs LIMIT 1",
		"SELECT TraceId FROM otel_traces LIMIT 1",
		"SELECT MetricName FROM otel_metrics_gauge LIMIT 1",
		"SELECT name FROM system.tables WHERE database='default'",
		"SELECT 1 FROM default.otel_logs LIMIT 1",
		"WITH t AS (SELECT 1 AS x) SELECT x FROM t",
	}
	for _, sql := range allowed {
		if err := validateSql(sql); err != nil {
			t.Errorf("validateSql(%q) = %v, want nil", sql, err)
		}
	}
}

func TestValidateSqlEmptyRaises(t *testing.T) {
	if err := validateSql("   "); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("got %v, want 'empty' error", err)
	}
}

func TestValidateSqlTruncateRaises(t *testing.T) {
	if err := validateSql("TRUNCATE TABLE t"); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("got %v, want 'read-only' error", err)
	}
}

func TestValidateSqlBlocksWriteAndDdl(t *testing.T) {
	for _, sql := range []string{"INSERT INTO otel_logs VALUES ()", "DROP TABLE otel_logs", "UPDATE otel_logs SET x=1"} {
		if err := validateSql(sql); err == nil {
			t.Errorf("validateSql(%q) = nil, want error", sql)
		}
	}
}

func TestValidateSqlBlocksSensitiveTables(t *testing.T) {
	blocked := []string{
		"SELECT Value FROM sobs_ai_settings",
		"SELECT * FROM sobs_notification_channels",
		"SELECT * FROM sobs_app_settings",
		"SELECT * FROM some_random_table",
		"SELECT * FROM other_db.otel_logs",
	}
	for _, sql := range blocked {
		err := validateSql(sql)
		if err == nil || !strings.Contains(err.Error(), "not permitted") {
			t.Errorf("validateSql(%q) = %v, want 'not permitted'", sql, err)
		}
	}
}

func TestBuildQueryAllowedTablesBuiltinWithoutEnv(t *testing.T) {
	os.Unsetenv("SOBS_QUERY_ALLOWED_TABLES")
	if !buildQueryAllowedTables()["otel_logs"] {
		t.Error("builtin allowlist must contain otel_logs")
	}
}

func TestBuildQueryAllowedTablesMergesEnv(t *testing.T) {
	t.Setenv("SOBS_QUERY_ALLOWED_TABLES", "my_custom_table, another_table")
	got := buildQueryAllowedTables()
	for _, name := range []string{"my_custom_table", "another_table", "otel_logs"} {
		if !got[name] {
			t.Errorf("allowlist missing %q", name)
		}
	}
}

func TestBuildQueryAllowedTablesRejectsUnsafeNames(t *testing.T) {
	t.Setenv("SOBS_QUERY_ALLOWED_TABLES", "valid_table, bad name, another.bad, ;evil")
	got := buildQueryAllowedTables()
	if !got["valid_table"] {
		t.Error("valid_table should be allowed")
	}
	for _, bad := range []string{"bad name", "another.bad", ";evil"} {
		if got[bad] {
			t.Errorf("unsafe name %q must not be allowed", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// S3 backup destination
// ---------------------------------------------------------------------------

func TestBuildS3BackupDestWithRegion(t *testing.T) {
	settings := map[string]string{
		"data_management.s3_bucket":            "my-sobs-backups",
		"data_management.s3_region":            "eu-west-1",
		"data_management.s3_path_prefix":       "",
		"data_management.s3_access_key_id":     "",
		"data_management.s3_secret_access_key": "",
	}
	dest, err := buildS3BackupDest(settings, "sobs-full-20240101")
	if err != nil {
		t.Fatalf("err %v", err)
	}
	for _, want := range []string{"my-sobs-backups", "eu-west-1", "sobs-full-20240101"} {
		if !strings.Contains(dest, want) {
			t.Errorf("dest %q missing %q", dest, want)
		}
	}
}

func TestBuildS3BackupDestWithCredentials(t *testing.T) {
	settings := map[string]string{
		"data_management.s3_bucket":            "my-bucket",
		"data_management.s3_region":            "us-east-1",
		"data_management.s3_path_prefix":       "backups",
		"data_management.s3_access_key_id":     "AKIATEST",
		"data_management.s3_secret_access_key": "secret123",
	}
	dest, err := buildS3BackupDest(settings, "test-backup")
	if err != nil {
		t.Fatalf("err %v", err)
	}
	for _, want := range []string{"AKIATEST", "secret123", "test-backup"} {
		if !strings.Contains(dest, want) {
			t.Errorf("dest %q missing %q", dest, want)
		}
	}
}

func TestBuildS3BackupDestRejectsUnsafeBucket(t *testing.T) {
	settings := map[string]string{
		"data_management.s3_bucket":            "my-bucket'bad",
		"data_management.s3_region":            "us-east-1",
		"data_management.s3_path_prefix":       "",
		"data_management.s3_access_key_id":     "",
		"data_management.s3_secret_access_key": "",
	}
	_, err := buildS3BackupDest(settings, "test-backup")
	if err == nil || !strings.Contains(err.Error(), "s3_bucket") {
		t.Errorf("got %v, want 's3_bucket' rejection", err)
	}
}

// ---------------------------------------------------------------------------
// Data-management prune period parsing
// ---------------------------------------------------------------------------

func TestParseDmPrunePeriodUnitWithoutValue(t *testing.T) {
	_, _, _, err := parseDmPrunePeriod(map[string]any{"prune_period_unit": "days"})
	if err == nil || !strings.Contains(err.Error(), "prune_period_value is required") {
		t.Errorf("got %v", err)
	}
}

func TestParseDmPrunePeriodValueWithoutUnit(t *testing.T) {
	_, _, _, err := parseDmPrunePeriod(map[string]any{"prune_period_value": 7})
	if err == nil || !strings.Contains(err.Error(), "prune_period_unit is required") {
		t.Errorf("got %v", err)
	}
}

func TestParseDmPrunePeriodInvalidUnit(t *testing.T) {
	_, _, _, err := parseDmPrunePeriod(map[string]any{"prune_period_value": 7, "prune_period_unit": "weeks"})
	if err == nil || !strings.Contains(err.Error(), "prune_period_unit must be 'hours' or 'days'") {
		t.Errorf("got %v", err)
	}
}

func TestParseDmPrunePeriodNonIntegerValue(t *testing.T) {
	_, _, _, err := parseDmPrunePeriod(map[string]any{"prune_period_value": "not_a_number", "prune_period_unit": "days"})
	if err == nil || !strings.Contains(err.Error(), "prune_period_value must be a positive integer") {
		t.Errorf("got %v", err)
	}
}

func TestParseDmPrunePeriodValid(t *testing.T) {
	value, unit, ok, err := parseDmPrunePeriod(map[string]any{"prune_period_value": 7, "prune_period_unit": "days"})
	if err != nil || !ok || value != 7 || unit != "days" {
		t.Errorf("got (%d, %q, %v, %v), want (7, days, true, nil)", value, unit, ok, err)
	}
}

func TestParseDmPrunePeriodEmptyIsNoop(t *testing.T) {
	_, _, ok, err := parseDmPrunePeriod(map[string]any{})
	if err != nil || ok {
		t.Errorf("empty payload should be no-op, got ok=%v err=%v", ok, err)
	}
}

// ---------------------------------------------------------------------------
// OSS safeguard reply parsing
// ---------------------------------------------------------------------------

func TestParseOssSafeguardReplyJson(t *testing.T) {
	verdict, category := parseOssSafeguardReply(
		`{"violation": 1, "policy_category": "H2.f", "rule_ids": ["H2.f"], "confidence": "high"}`, true)
	if verdict != "UNSAFE" || category != "H2.f" {
		t.Errorf("got (%q, %q), want (UNSAFE, H2.f)", verdict, category)
	}

	verdict, category = parseOssSafeguardReply(
		`{"violation": 0, "policy_category": null, "rule_ids": [], "confidence": "low"}`, true)
	if verdict != "SAFE" || category != "" {
		t.Errorf("got (%q, %q), want (SAFE, '')", verdict, category)
	}
}

func TestParseOssSafeguardReplyStrictInvalid(t *testing.T) {
	verdict, category := parseOssSafeguardReply("not json", true)
	if verdict != "" || category != "" {
		t.Errorf("got (%q, %q), want ('', '')", verdict, category)
	}
}
