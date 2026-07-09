package main

import (
	"reflect"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// blockedRunBody rebuilds the api_query_run explain-failed payload exactly as handleApiQueryRun
// does (frozen time_ns so trace_id is deterministic) and encodes it like Quart's jsonify.
func blockedRunBody(sql string) string {
	explainErr := "SQL validation error: " + validateSQL(sql)
	traceID := md5Hex("query-run|" + sql + "|1704164645000000000") // FIXED_EPOCH * 1e9
	obj := jsonenc.NewObject().
		Set("ok", false).Set("error", explainErr).Set("trace_id", traceID).Set("turn_id", traceID[:16]).
		Set("sql", sql).Set("columns", []any{}).Set("rows", []any{}).
		Set("llm_stats", queryExplainFailedLLMStats())
	return string(jsonenc.Encode(obj, jsonenc.QuartJSONify))
}

// TestQueryRunBlockedResponseBytes pins the full 422 body byte-for-byte against the frozen Python
// oracle's jsonify output (sort_keys, ensure_ascii, compact separators, trailing newline) for both
// the no-suggestion ('users') and difflib-suggestion ('otel_logz') branches.
func TestQueryRunBlockedResponseBytes(t *testing.T) {
	wantUsers := `{"columns":[],"error":"SQL validation error: Access to table or view 'users' is not permitted. Only approved observability tables may be queried via the Query page. Allowed tables: hyperdx_sessions, otel_logs, otel_metrics_1m_agg, otel_metrics_gauge, otel_metrics_gauge_pinned, otel_metrics_histogram, otel_metrics_histogram_pinned, otel_metrics_sum, otel_metrics_sum_pinned, otel_traces, sobs_anomaly_rules, sobs_raw_windows, v_derived_signals_1m, v_derived_signals_anomaly, v_otel_metrics_1m, v_otel_metrics_anomaly, v_otel_metrics_dedup, v_otel_metrics_signal_context. If this is a valid custom table/view, add it via SOBS_QUERY_ALLOWED_TABLES.","llm_stats":{"totals":{"completion_tokens":0,"elapsed_ms":0,"prompt_tokens":0,"thinking_tokens":0}},"ok":false,"rows":[],"sql":"SELECT * FROM users","trace_id":"1653c2698fee41b847b323da16311d90","turn_id":"1653c2698fee41b8"}` + "\n"
	if got := blockedRunBody("SELECT * FROM users"); got != wantUsers {
		t.Errorf("users body mismatch:\n got  %s\n want %s", got, wantUsers)
	}

	wantSuggest := `{"columns":[],"error":"SQL validation error: Access to table or view 'otel_logz' is not permitted. Only approved observability tables may be queried via the Query page. Allowed tables: hyperdx_sessions, otel_logs, otel_metrics_1m_agg, otel_metrics_gauge, otel_metrics_gauge_pinned, otel_metrics_histogram, otel_metrics_histogram_pinned, otel_metrics_sum, otel_metrics_sum_pinned, otel_traces, sobs_anomaly_rules, sobs_raw_windows, v_derived_signals_1m, v_derived_signals_anomaly, v_otel_metrics_1m, v_otel_metrics_anomaly, v_otel_metrics_dedup, v_otel_metrics_signal_context. Closest allowed names: otel_logs, otel_traces, otel_metrics_histogram. If this is a valid custom table/view, add it via SOBS_QUERY_ALLOWED_TABLES.","llm_stats":{"totals":{"completion_tokens":0,"elapsed_ms":0,"prompt_tokens":0,"thinking_tokens":0}},"ok":false,"rows":[],"sql":"SELECT count() FROM otel_logz","trace_id":"b3f5f87039b879cdb73016ae095b1d3b","turn_id":"b3f5f87039b879cd"}` + "\n"
	if got := blockedRunBody("SELECT count() FROM otel_logz"); got != wantSuggest {
		t.Errorf("otel_logz body mismatch:\n got  %s\n want %s", got, wantSuggest)
	}
}

// allowedTablesList is the exact ", "-joined sorted allowlist that validateSQL embeds, recomputed
// here so the message assertions read independently of the (sorted) builtin slice.
const allowedTablesList = "hyperdx_sessions, otel_logs, otel_metrics_1m_agg, otel_metrics_gauge, " +
	"otel_metrics_gauge_pinned, otel_metrics_histogram, otel_metrics_histogram_pinned, " +
	"otel_metrics_sum, otel_metrics_sum_pinned, otel_traces, sobs_anomaly_rules, sobs_raw_windows, " +
	"v_derived_signals_1m, v_derived_signals_anomaly, v_otel_metrics_1m, v_otel_metrics_anomaly, " +
	"v_otel_metrics_dedup, v_otel_metrics_signal_context"

// TestSuggestAllowedTableNames pins the difflib.get_close_matches port against a table captured from
// CPython's difflib (n=5, cutoff=0.45) over the sorted builtin allowlist. Includes 5-result cases
// that exercise the (ratio, name) descending tie-break.
func TestSuggestAllowedTableNames(t *testing.T) {
	want := map[string][]string{
		"users":                   {},
		"secret":                  {},
		"otel_logz":               {"otel_logs", "otel_traces", "otel_metrics_histogram"},
		"otel_log":                {"otel_logs", "otel_traces", "otel_metrics_histogram", "otel_metrics_gauge"},
		"otel_metrics_gause":      {"otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_gauge_pinned", "otel_metrics_1m_agg", "v_otel_metrics_1m"},
		"sobs_anomaly_rule":       {"sobs_anomaly_rules", "v_otel_metrics_anomaly", "v_derived_signals_anomaly"},
		"passwords":               {},
		"information_schema":      {},
		"x":                       {},
		"traces":                  {"otel_traces"},
		"logs":                    {"otel_logs"},
		"metrics":                 {"otel_metrics_sum", "v_otel_metrics_1m", "otel_metrics_gauge", "otel_traces", "otel_metrics_1m_agg"},
		"otel":                    {"otel_logs", "otel_traces"},
		"v_otel_metrics":          {"v_otel_metrics_1m", "v_otel_metrics_dedup", "otel_metrics_sum", "v_otel_metrics_anomaly", "otel_metrics_gauge"},
		"sobs":                    {},
		"hyperdx_session":         {"hyperdx_sessions"},
		"otel_metrics_histograms": {"otel_metrics_histogram", "otel_metrics_histogram_pinned", "otel_metrics_sum", "otel_metrics_gauge", "v_otel_metrics_1m"},
		"v_derived":               {"v_derived_signals_1m", "v_derived_signals_anomaly"},
		"gauge":                   {},
		"otel_metrics_sum_pin":    {"otel_metrics_sum_pinned", "otel_metrics_sum", "otel_metrics_gauge_pinned", "otel_metrics_histogram_pinned", "otel_metrics_1m_agg"},
		"otel_traces":             {"otel_traces", "otel_metrics_sum", "v_otel_metrics_1m", "otel_metrics_gauge", "otel_metrics_1m_agg"},
		"otel_logs":               {"otel_logs", "otel_traces", "otel_metrics_sum", "v_otel_metrics_1m", "otel_metrics_histogram"},
	}
	for name, exp := range want {
		got := suggestAllowedTableNames(name, 5)
		if len(got) == 0 && len(exp) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, exp) {
			t.Errorf("suggest(%q):\n got  %v\n want %v", name, got, exp)
		}
	}
}

// TestSuggestQualifiedRefUsesTableComponent mirrors _suggest_allowed_table_names splitting on "."
// and matching only the final identifier.
func TestSuggestQualifiedRefUsesTableComponent(t *testing.T) {
	got := suggestAllowedTableNames("myschema.OTEL_LOGZ", 5)
	want := []string{"otel_logs", "otel_traces", "otel_metrics_histogram"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestValidateSQLReadOnly(t *testing.T) {
	cases := []struct {
		name, sql, want string
	}{
		{"empty", "   ", "SQL statement is empty."},
		// Prefix check runs BEFORE the unsafe-pattern check, so a write statement is rejected on
		// its leading keyword (matches app.py validate_sql order).
		{"non_read_only", "DELETE FROM otel_logs", "Only read-only SQL is allowed (SELECT, EXPLAIN, SHOW, DESCRIBE, WITH). Got: 'DELETE'."},
		{"bad_prefix", "PRAGMA table_info(x)", "Only read-only SQL is allowed (SELECT, EXPLAIN, SHOW, DESCRIBE, WITH). Got: 'PRAGMA'."},
		// A safe-prefixed statement that smuggles a write/DDL keyword IS caught by the pattern check.
		{"unsafe_keyword_in_select", "SELECT 1; DROP TABLE otel_logs", "SQL statement contains a disallowed write or DDL keyword (INSERT, UPDATE, DELETE, DROP, CREATE, TRUNCATE, …)."},
	}
	for _, c := range cases {
		if got := validateSQL(c.sql); got != c.want {
			t.Errorf("%s: validateSQL(%q)\n got  %q\n want %q", c.name, c.sql, got, c.want)
		}
	}
}

func TestValidateSQLDisallowedTableNoSuggestion(t *testing.T) {
	want := "Access to table or view 'users' is not permitted. " +
		"Only approved observability tables may be queried via the Query page. " +
		"Allowed tables: " + allowedTablesList + "." +
		" If this is a valid custom table/view, add it via SOBS_QUERY_ALLOWED_TABLES."
	if got := validateSQL("SELECT * FROM users"); got != want {
		t.Errorf("\n got  %q\n want %q", got, want)
	}
}

func TestValidateSQLDisallowedTableWithSuggestion(t *testing.T) {
	want := "Access to table or view 'otel_logz' is not permitted. " +
		"Only approved observability tables may be queried via the Query page. " +
		"Allowed tables: " + allowedTablesList + "." +
		" Closest allowed names: otel_logs, otel_traces, otel_metrics_histogram." +
		" If this is a valid custom table/view, add it via SOBS_QUERY_ALLOWED_TABLES."
	if got := validateSQL("SELECT count() FROM otel_logz"); got != want {
		t.Errorf("\n got  %q\n want %q", got, want)
	}
}

// TestValidateSQLAllowed covers the permitted branches: allowlisted tables, the always-allowed
// `system` database, CTE aliases, and ARRAY JOIN targets are not treated as disallowed tables.
func TestValidateSQLAllowed(t *testing.T) {
	ok := []string{
		"SELECT * FROM otel_logs",
		"SELECT 1 AS one",
		"select count() from otel_traces join otel_logs on 1=1",
		"SELECT name FROM system.tables",
		"WITH t AS (SELECT 1) SELECT * FROM t",
		"WITH RECURSIVE r AS (SELECT 1) SELECT * FROM r",
		"SELECT k FROM otel_logs ARRAY JOIN LogAttributes AS k",
		"SHOW TABLES",
		"DESCRIBE otel_logs",
		"EXPLAIN SELECT * FROM otel_logs",
	}
	for _, sql := range ok {
		if got := validateSQL(sql); got != "" {
			t.Errorf("validateSQL(%q) should pass, got %q", sql, got)
		}
	}
}

// TestValidateSQLNonDefaultDatabaseBlocked: a database-qualified ref outside default/system is
// blocked (returns the original-case ref).
func TestValidateSQLNonDefaultDatabaseBlocked(t *testing.T) {
	got := validateSQL("SELECT * FROM otherdb.otel_logs")
	if got == "" {
		t.Fatalf("expected block for otherdb.otel_logs")
	}
	if want := "Access to table or view 'otherdb.otel_logs' is not permitted. "; got[:len(want)] != want {
		t.Errorf("prefix:\n got  %q\n want %q", got[:len(want)], want)
	}
}
