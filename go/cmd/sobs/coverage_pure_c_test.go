package main

// Oracle-anchored unit tests for pure helper functions — Slice C.
// Expected outputs are derived from the frozen Python oracle (app.py).
// No Docker, no DB — native `go test` only.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---------------------------------------------------------------------------
// validateCustomMaskingPattern (masking.go)
// ---------------------------------------------------------------------------

func TestValidateCustomMaskingPattern(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
		wantOut string // only checked when wantErr==false
	}{
		{
			name:    "empty_pattern",
			input:   "",
			wantErr: true,
		},
		{
			name:    "whitespace_only",
			input:   "   ",
			wantErr: true,
		},
		{
			name:    "valid_simple",
			input:   `\d{4}-\d{4}`,
			wantErr: false,
			wantOut: `\d{4}-\d{4}`,
		},
		{
			name:    "backreference_rejected",
			input:   `(foo)\1`,
			wantErr: true, // Go RE2 fails to compile \1; Python gives explicit backreference error
		},
		{
			name:    "lookbehind_rejected",
			input:   `(?<=foo)bar`,
			wantErr: true, // JavaScript-compat check rejects lookbehind
		},
		{
			name:    "bad_regex_rejected",
			input:   `[unclosed`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateCustomMaskingPattern(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantOut {
				t.Fatalf("got %q, want %q", got, tc.wantOut)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// readEnvOrFile (settings_crypto.go)
// ---------------------------------------------------------------------------

func TestReadEnvOrFile(t *testing.T) {
	t.Run("direct_env_wins", func(t *testing.T) {
		t.Setenv("TEST_SOBS_DIRECT", "hello")
		got := readEnvOrFile("TEST_SOBS_DIRECT", "TEST_SOBS_DIRECT_FILE")
		if got != "hello" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("file_env_fallback", func(t *testing.T) {
		dir := t.TempDir()
		fp := filepath.Join(dir, "secret.txt")
		if err := os.WriteFile(fp, []byte("  file-secret  \n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TEST_SOBS_ONLY_FILE", "")
		t.Setenv("TEST_SOBS_ONLY_FILE_FILE", fp)
		got := readEnvOrFile("TEST_SOBS_ONLY_FILE", "TEST_SOBS_ONLY_FILE_FILE")
		if got != "file-secret" {
			t.Fatalf("got %q, want %q", got, "file-secret")
		}
	})

	t.Run("both_unset_returns_empty", func(t *testing.T) {
		t.Setenv("TEST_SOBS_ABSENT", "")
		t.Setenv("TEST_SOBS_ABSENT_FILE", "")
		got := readEnvOrFile("TEST_SOBS_ABSENT", "TEST_SOBS_ABSENT_FILE")
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("direct_env_whitespace_ignored", func(t *testing.T) {
		t.Setenv("TEST_SOBS_WS", "   ")
		t.Setenv("TEST_SOBS_WS_FILE", "")
		got := readEnvOrFile("TEST_SOBS_WS", "TEST_SOBS_WS_FILE")
		if got != "" {
			t.Fatalf("whitespace-only env should not win; got %q", got)
		}
	})

	t.Run("no_file_env_name_returns_empty", func(t *testing.T) {
		t.Setenv("TEST_SOBS_NF", "")
		got := readEnvOrFile("TEST_SOBS_NF", "")
		if got != "" {
			t.Fatalf("got %q", got)
		}
	})
}

// ---------------------------------------------------------------------------
// parseRegexFilterExpression (query_filters.go)
// ---------------------------------------------------------------------------

func TestParseRegexFilterExpression(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantInclude []string
		wantExclude []string
		wantErr     bool
	}{
		{
			name:        "empty",
			input:       "",
			wantInclude: nil,
			wantExclude: nil,
		},
		{
			name:        "whitespace_only",
			input:       "   ",
			wantInclude: nil,
			wantExclude: nil,
		},
		{
			name:        "single_include",
			input:       "error",
			wantInclude: []string{"error"},
			wantExclude: nil,
		},
		{
			name:        "include_and_exclude",
			input:       "error && !debug",
			wantInclude: []string{"error"},
			wantExclude: []string{"debug"},
		},
		{
			name:        "negation_only",
			input:       "!health",
			wantInclude: nil,
			wantExclude: []string{"health"},
		},
		{
			name:    "invalid_regex",
			input:   "[unclosed",
			wantErr: true,
		},
		{
			name:    "empty_token_after_bang",
			input:   "!",
			wantErr: true,
		},
		{
			name:        "multiple_includes",
			input:       "foo && bar",
			wantInclude: []string{"foo", "bar"},
			wantExclude: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inc, exc, errMsg := parseRegexFilterExpression(tc.input)
			if tc.wantErr {
				if errMsg == "" {
					t.Fatalf("expected error, got include=%v exclude=%v", inc, exc)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("unexpected error: %q", errMsg)
			}
			if !strSliceEqual(inc, tc.wantInclude) {
				t.Fatalf("include: got %v, want %v", inc, tc.wantInclude)
			}
			if !strSliceEqual(exc, tc.wantExclude) {
				t.Fatalf("exclude: got %v, want %v", exc, tc.wantExclude)
			}
		})
	}
}

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

// ---------------------------------------------------------------------------
// mapToDict (handlers_misc.go)
// ---------------------------------------------------------------------------

func TestMapToDict(t *testing.T) {
	t.Run("nil_returns_empty", func(t *testing.T) {
		r := mapToDict(nil)
		m, ok := r.(map[string]any)
		if !ok || len(m) != 0 {
			t.Fatalf("got %T %v", r, r)
		}
	})

	t.Run("map_pass_through", func(t *testing.T) {
		input := map[string]any{"a": 1.0}
		r := mapToDict(input)
		rm, ok := r.(map[string]any)
		if !ok || len(rm) != 1 {
			t.Fatalf("expected same map, got %v", r)
		}
		if rm["a"] != 1.0 {
			t.Fatalf("expected a=1.0, got %v", rm["a"])
		}
	})

	t.Run("valid_json_string", func(t *testing.T) {
		r := mapToDict(`{"x": 42}`)
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("expected map, got %T", r)
		}
		if fmt.Sprintf("%v", m["x"]) != "42" {
			t.Fatalf("x = %v", m["x"])
		}
	})

	t.Run("invalid_json_string_returns_empty", func(t *testing.T) {
		r := mapToDict("not json at all")
		m, ok := r.(map[string]any)
		if !ok || len(m) != 0 {
			t.Fatalf("got %T %v", r, r)
		}
	})

	t.Run("python_dict_literal_returns_empty", func(t *testing.T) {
		// Go does not do ast.literal_eval — Python dicts stay as empty dict.
		r := mapToDict("{'py': 'dict'}")
		m, ok := r.(map[string]any)
		if !ok || len(m) != 0 {
			t.Fatalf("Python dict literal should return empty map, got %T %v", r, r)
		}
	})

	t.Run("json_array_is_valid_json_not_map", func(t *testing.T) {
		// Python: parsed if isinstance(parsed, dict) else {} — array → {}
		r := mapToDict(`[1,2]`)
		m, ok := r.(map[string]any)
		if !ok || len(m) != 0 {
			t.Fatalf("array JSON string should return empty map (mirrors Python), got %T %v", r, r)
		}
	})

	t.Run("int_returns_empty", func(t *testing.T) {
		r := mapToDict(42)
		m, ok := r.(map[string]any)
		if !ok || len(m) != 0 {
			t.Fatalf("got %T %v", r, r)
		}
	})
}

// ---------------------------------------------------------------------------
// otlpAnyValue + otlpKVList (otlp_ingest.go)
// ---------------------------------------------------------------------------

func TestOtlpAnyValue(t *testing.T) {
	t.Run("string_value", func(t *testing.T) {
		v := otlpAnyValue(map[string]any{"stringValue": "hello"})
		if v != "hello" {
			t.Fatalf("got %v", v)
		}
	})

	t.Run("int_value_string_form", func(t *testing.T) {
		v := otlpAnyValue(map[string]any{"intValue": "42"})
		if v != int64(42) {
			t.Fatalf("got %T(%v)", v, v)
		}
	})

	t.Run("int_value_float_form", func(t *testing.T) {
		v := otlpAnyValue(map[string]any{"intValue": float64(100)})
		if v != int64(100) {
			t.Fatalf("got %T(%v)", v, v)
		}
	})

	t.Run("double_value", func(t *testing.T) {
		v := otlpAnyValue(map[string]any{"doubleValue": float64(3.14)})
		if math.Abs(v.(float64)-3.14) > 1e-9 {
			t.Fatalf("got %v", v)
		}
	})

	t.Run("bool_value_true", func(t *testing.T) {
		v := otlpAnyValue(map[string]any{"boolValue": true})
		if v != true {
			t.Fatalf("got %v", v)
		}
	})

	t.Run("bool_value_false", func(t *testing.T) {
		v := otlpAnyValue(map[string]any{"boolValue": false})
		if v != false {
			t.Fatalf("got %v", v)
		}
	})

	t.Run("array_value", func(t *testing.T) {
		v := otlpAnyValue(map[string]any{
			"arrayValue": map[string]any{
				"values": []any{
					map[string]any{"stringValue": "a"},
					map[string]any{"intValue": "1"},
				},
			},
		})
		arr, ok := v.([]any)
		if !ok || len(arr) != 2 {
			t.Fatalf("got %T %v", v, v)
		}
		if arr[0] != "a" {
			t.Fatalf("arr[0] = %v", arr[0])
		}
		if arr[1] != int64(1) {
			t.Fatalf("arr[1] = %T(%v)", arr[1], arr[1])
		}
	})

	t.Run("kvlist_value", func(t *testing.T) {
		v := otlpAnyValue(map[string]any{
			"kvlistValue": map[string]any{
				"values": []any{
					map[string]any{"key": "k", "value": map[string]any{"stringValue": "v"}},
				},
			},
		})
		m, ok := v.(map[string]any)
		if !ok || m["k"] != "v" {
			t.Fatalf("got %T %v", v, v)
		}
	})

	t.Run("unknown_type_returns_nil", func(t *testing.T) {
		v := otlpAnyValue(map[string]any{})
		if v != nil {
			t.Fatalf("got %v", v)
		}
	})

	t.Run("non_map_returns_nil", func(t *testing.T) {
		v := otlpAnyValue("not a map")
		if v != nil {
			t.Fatalf("got %v", v)
		}
	})
}

func TestOtlpKVList(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		m := otlpKVList(nil)
		if len(m) != 0 {
			t.Fatalf("got %v", m)
		}
	})

	t.Run("string_attr", func(t *testing.T) {
		attrs := []any{
			map[string]any{"key": "http.method", "value": map[string]any{"stringValue": "GET"}},
		}
		m := otlpKVList(attrs)
		if m["http.method"] != "GET" {
			t.Fatalf("got %v", m)
		}
	})

	t.Run("int_attr", func(t *testing.T) {
		attrs := []any{
			map[string]any{"key": "http.status_code", "value": map[string]any{"intValue": "200"}},
		}
		m := otlpKVList(attrs)
		if m["http.status_code"] != int64(200) {
			t.Fatalf("got %T(%v)", m["http.status_code"], m["http.status_code"])
		}
	})

	t.Run("mixed_attrs", func(t *testing.T) {
		attrs := []any{
			map[string]any{"key": "svc", "value": map[string]any{"stringValue": "web"}},
			map[string]any{"key": "code", "value": map[string]any{"intValue": "404"}},
		}
		m := otlpKVList(attrs)
		if m["svc"] != "web" || m["code"] != int64(404) {
			t.Fatalf("got %v", m)
		}
	})
}

// ---------------------------------------------------------------------------
// formatDrilldownTime (chart_render_binding.go)
// ---------------------------------------------------------------------------

func TestFormatDrilldownTime(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  string
	}{
		{"empty_string", "", ""},
		{"nil", nil, ""},
		{"utc_iso_z", "2024-01-15T10:30:45Z", "2024-01-15T10:30:45Z"},
		{"offset_plus", "2024-01-15T10:30:45+05:30", "2024-01-15T05:00:45Z"},
		{"offset_minus", "2024-01-15T10:30:45-08:00", "2024-01-15T18:30:45Z"},
		{"space_separated", "2024-01-15 10:30:45", "2024-01-15T10:30:45Z"},
		{"space_with_offset", "2024-01-15 10:30:45+00:00", "2024-01-15T10:30:45Z"},
		{"rfc1123_form", "Mon, 15 Jan 2024 10:30:45 GMT", "2024-01-15T10:30:45Z"},
		{"unparseable_passthrough", "not-a-date", "not-a-date"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatDrilldownTime(tc.input)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// semanticMemoryMatches (ai_helper_context.go)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// resolveCustomBindingExpr (chart_render_binding.go)
// ---------------------------------------------------------------------------

func TestResolveCustomBindingExpr(t *testing.T) {
	cols := []any{"ts", "value"}
	records := []map[string]any{
		{"ts": "2024-01-01", "value": 10.0},
		{"ts": "2024-01-02", "value": 20.0},
	}
	rows2d := []any{
		[]any{"2024-01-01", 10.0},
		[]any{"2024-01-02", 20.0},
	}

	t.Run("string_columns", func(t *testing.T) {
		r, e := resolveCustomBindingExpr("columns", cols, records, rows2d)
		if e != "" {
			t.Fatalf("err: %s", e)
		}
		if fmt.Sprintf("%v", r) != fmt.Sprintf("%v", cols) {
			t.Fatalf("got %v", r)
		}
	})

	t.Run("string_rows", func(t *testing.T) {
		r, e := resolveCustomBindingExpr("rows", cols, records, rows2d)
		if e != "" {
			t.Fatalf("err: %s", e)
		}
		if fmt.Sprintf("%v", r) != fmt.Sprintf("%v", rows2d) {
			t.Fatalf("got %v", r)
		}
	})

	t.Run("string_records", func(t *testing.T) {
		r, e := resolveCustomBindingExpr("records", cols, records, rows2d)
		if e != "" {
			t.Fatalf("err: %s", e)
		}
		arr, ok := r.([]any)
		if !ok || len(arr) != 2 {
			t.Fatalf("got %T %v", r, r)
		}
	})

	t.Run("string_column_name", func(t *testing.T) {
		r, e := resolveCustomBindingExpr("value", cols, records, rows2d)
		if e != "" {
			t.Fatalf("err: %s", e)
		}
		arr, ok := r.([]any)
		if !ok || len(arr) != 2 {
			t.Fatalf("got %T %v", r, r)
		}
		if arr[0] != 10.0 || arr[1] != 20.0 {
			t.Fatalf("got %v", arr)
		}
	})

	t.Run("empty_string_returns_nil", func(t *testing.T) {
		r, e := resolveCustomBindingExpr("", cols, records, rows2d)
		if e != "" {
			t.Fatalf("err: %s", e)
		}
		if r != nil {
			t.Fatalf("got %v", r)
		}
	})

	t.Run("object_from_columns", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("from", "columns")
		r, e := resolveCustomBindingExpr(obj, cols, records, rows2d)
		if e != "" {
			t.Fatalf("err: %s", e)
		}
		if fmt.Sprintf("%v", r) != fmt.Sprintf("%v", cols) {
			t.Fatalf("got %v", r)
		}
	})

	t.Run("object_from_literal", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("from", "literal").Set("value", "my-literal")
		r, e := resolveCustomBindingExpr(obj, cols, records, rows2d)
		if e != "" {
			t.Fatalf("err: %s", e)
		}
		if r != "my-literal" {
			t.Fatalf("got %v", r)
		}
	})

	t.Run("object_from_column_by_name", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("from", "column").Set("name", "ts")
		r, e := resolveCustomBindingExpr(obj, cols, records, rows2d)
		if e != "" {
			t.Fatalf("err: %s", e)
		}
		arr, ok := r.([]any)
		if !ok || len(arr) != 2 || arr[0] != "2024-01-01" || arr[1] != "2024-01-02" {
			t.Fatalf("got %v", r)
		}
	})

	t.Run("object_from_column_missing_name_errors", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("from", "column")
		_, e := resolveCustomBindingExpr(obj, cols, records, rows2d)
		if e == "" {
			t.Fatal("expected error for missing column name")
		}
	})

	t.Run("object_unsupported_mode_errors", func(t *testing.T) {
		obj := jsonenc.NewObject().Set("from", "badmode")
		_, e := resolveCustomBindingExpr(obj, cols, records, rows2d)
		if e == "" {
			t.Fatal("expected error for unsupported mode")
		}
	})

	t.Run("non_string_non_object_errors", func(t *testing.T) {
		_, e := resolveCustomBindingExpr(42, cols, records, rows2d)
		if e == "" {
			t.Fatal("expected error for invalid expr type")
		}
	})
}

// ---------------------------------------------------------------------------
// normalizeChartSpec (chart_spec.go)
// ---------------------------------------------------------------------------

func TestNormalizeChartSpec(t *testing.T) {
	t.Run("unknown_template_errors", func(t *testing.T) {
		raw := jsonenc.NewObject().Set("template_id", "nonexistent")
		_, errMsg := normalizeChartSpec(raw)
		if errMsg == "" {
			t.Fatal("expected error for unknown template")
		}
		if errMsg != "Unknown template: nonexistent" {
			t.Fatalf("got %q", errMsg)
		}
	})

	t.Run("missing_sql_mode_errors", func(t *testing.T) {
		// template_id present, sql present but no mode → str(None) = "none" → invalid
		raw := jsonenc.NewObject().
			Set("template_id", "time_series_percentiles").
			Set("sql", jsonenc.NewObject())
		_, errMsg := normalizeChartSpec(raw)
		if errMsg == "" {
			t.Fatal("expected error for missing sql.mode")
		}
		if errMsg != "sql.mode must be 'builder' or 'raw'" {
			t.Fatalf("got %q", errMsg)
		}
	})

	t.Run("absent_template_defaults_to_derived_signal_overlay", func(t *testing.T) {
		// No template_id → defaults to "derived_signal_overlay"
		raw := jsonenc.NewObject().
			Set("sql", jsonenc.NewObject().Set("mode", "builder"))
		spec, errMsg := normalizeChartSpec(raw)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		tidV, _ := spec.Get("template_id")
		if tidV != "derived_signal_overlay" {
			t.Fatalf("default template_id = %v", tidV)
		}
	})

	t.Run("valid_builder_spec", func(t *testing.T) {
		raw := jsonenc.NewObject().
			Set("template_id", "time_series_percentiles").
			Set("sql", jsonenc.NewObject().Set("mode", "builder"))
		spec, errMsg := normalizeChartSpec(raw)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		tidV, _ := spec.Get("template_id")
		if tidV != "time_series_percentiles" {
			t.Fatalf("template_id = %v", tidV)
		}
		sqlV, _ := spec.Get("sql")
		sqlObj, ok := sqlV.(*jsonenc.Object)
		if !ok {
			t.Fatalf("sql not an object: %T", sqlV)
		}
		modeV, _ := sqlObj.Get("mode")
		if modeV != "builder" {
			t.Fatalf("sql.mode = %v", modeV)
		}
	})

	t.Run("valid_raw_spec", func(t *testing.T) {
		raw := jsonenc.NewObject().
			Set("template_id", "heatmap").
			Set("sql", jsonenc.NewObject().Set("mode", "raw").Set("override_sql", "SELECT 1"))
		spec, errMsg := normalizeChartSpec(raw)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		sqlV, _ := spec.Get("sql")
		sqlObj := sqlV.(*jsonenc.Object)
		ov, _ := sqlObj.Get("override_sql")
		if ov != "SELECT 1" {
			t.Fatalf("override_sql = %v", ov)
		}
	})

	t.Run("named_queries_filtered", func(t *testing.T) {
		// Invalid name (has space) → filtered out; valid one stays.
		raw := jsonenc.NewObject().
			Set("template_id", "gauge_kpi").
			Set("sql", jsonenc.NewObject().Set("mode", "raw")).
			Set("named_queries", []any{
				jsonenc.NewObject().Set("name", "valid_query").Set("sql", "SELECT 1").Set("purpose", "test"),
				jsonenc.NewObject().Set("name", "bad name").Set("sql", "SELECT 2"),
				jsonenc.NewObject().Set("name", "").Set("sql", "SELECT 3"),
				jsonenc.NewObject().Set("name", "no_sql"),
			})
		spec, errMsg := normalizeChartSpec(raw)
		if errMsg != "" {
			t.Fatalf("unexpected error: %s", errMsg)
		}
		nqV, _ := spec.Get("named_queries")
		nqList, ok := nqV.([]any)
		if !ok {
			t.Fatalf("named_queries not list: %T", nqV)
		}
		if len(nqList) != 1 {
			t.Fatalf("expected 1 valid named_query, got %d", len(nqList))
		}
	})

	t.Run("nil_input_defaults", func(t *testing.T) {
		// nil input → default template "derived_signal_overlay", no sql → error
		_, errMsg := normalizeChartSpec(nil)
		if errMsg == "" {
			t.Fatal("expected error when sql.mode absent")
		}
	})
}

// ---------------------------------------------------------------------------
// extractStructuredErrorSummary / summaryFromParsed (handlers_incident.go)
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// buildAiTraceTurnCards (ai_view.go)
// ---------------------------------------------------------------------------

func TestBuildAiTraceTurnCards(t *testing.T) {
	t.Run("empty_spans_returns_empty", func(t *testing.T) {
		out := buildAiTraceTurnCards(nil)
		if len(out) != 0 {
			t.Fatalf("expected empty, got %v", out)
		}
	})

	t.Run("span_without_turn_id_skipped", func(t *testing.T) {
		spans := []*aiItem{
			{turnID: "", eventName: "turn.complete", response: "hello"},
		}
		out := buildAiTraceTurnCards(spans)
		if len(out) != 0 {
			t.Fatalf("expected empty (no turnID), got %d", len(out))
		}
	})

	t.Run("single_complete_turn", func(t *testing.T) {
		spans := []*aiItem{
			{
				turnID:        "turn-1",
				chatID:        "chat-1",
				model:         "gpt-4",
				provider:      "openai",
				traceID:       "trace-abc",
				eventName:     "turn.complete",
				ts:            "2024-01-01T10:00:00Z",
				tokensIn:      100,
				tokensOut:     50,
				inputQuestion: "What is 2+2?",
				response:      "4",
			},
		}
		out := buildAiTraceTurnCards(spans)
		if len(out) != 1 {
			t.Fatalf("expected 1 turn card, got %d", len(out))
		}
		card, ok := out[0].(*jsonenc.Object)
		if !ok {
			t.Fatalf("expected *jsonenc.Object, got %T", out[0])
		}
		assertObjStr(t, card, "turn_id", "turn-1")
		assertObjStr(t, card, "chat_id", "chat-1")
		assertObjStr(t, card, "model", "gpt-4")
		assertObjStr(t, card, "provider", "openai")
		assertObjStr(t, card, "status", "completed")
		assertObjStr(t, card, "user_message", "What is 2+2?")
		assertObjStr(t, card, "assistant_message", "4")
		assertObjStr(t, card, "trace_id", "trace-abc")
		assertObjInt(t, card, "tokens_in", 100)
		assertObjInt(t, card, "tokens_out", 50)
		assertObjInt(t, card, "index", 1)
	})

	t.Run("blocked_turn", func(t *testing.T) {
		// guard.result sets guardAllowed; turn.blocked overwrites guardReason.
		spans := []*aiItem{
			{
				turnID:    "turn-2",
				chatID:    "chat-2",
				eventName: "guard.result",
				ts:        "2024-01-01T09:00:00Z",
			},
			{
				turnID:      "turn-2",
				chatID:      "chat-2",
				eventName:   "turn.blocked",
				ts:          "2024-01-01T09:00:01Z",
				guardReason: "policy violation",
			},
		}
		out := buildAiTraceTurnCards(spans)
		if len(out) != 1 {
			t.Fatalf("expected 1 turn, got %d", len(out))
		}
		card := out[0].(*jsonenc.Object)
		assertObjStr(t, card, "status", "blocked")
		assertObjStr(t, card, "guard_reason", "policy violation")
	})

	t.Run("tool_events_aggregated", func(t *testing.T) {
		spans := []*aiItem{
			{
				turnID:      "turn-3",
				eventName:   "tool.proposed",
				ts:          "2024-01-01T08:00:00Z",
				toolName:    "apply_sql_filter",
				toolStatus:  "proposed",
				toolSummary: "Filter by service",
			},
			{
				turnID:      "turn-3",
				eventName:   "tool.executed",
				ts:          "2024-01-01T08:00:01Z",
				toolName:    "apply_sql_filter",
				toolStatus:  "executed",
				toolSummary: "Filter applied",
			},
			{
				turnID:    "turn-3",
				eventName: "turn.complete",
				ts:        "2024-01-01T08:00:02Z",
			},
		}
		out := buildAiTraceTurnCards(spans)
		if len(out) != 1 {
			t.Fatalf("expected 1 turn, got %d", len(out))
		}
		card := out[0].(*jsonenc.Object)
		assertObjStr(t, card, "status", "completed")
		toolCountV, _ := card.Get("tool_count")
		if toolCountV != 2 {
			t.Fatalf("tool_count = %v", toolCountV)
		}
	})

	t.Run("two_turns_sorted_by_started_at", func(t *testing.T) {
		spans := []*aiItem{
			{
				turnID:    "turn-b",
				eventName: "turn.complete",
				ts:        "2024-01-01T11:00:00Z",
				response:  "second",
			},
			{
				turnID:    "turn-a",
				eventName: "turn.complete",
				ts:        "2024-01-01T10:00:00Z",
				response:  "first",
			},
		}
		out := buildAiTraceTurnCards(spans)
		if len(out) != 2 {
			t.Fatalf("expected 2 turns, got %d", len(out))
		}
		// turn-a starts earlier → index 1
		c0 := out[0].(*jsonenc.Object)
		c1 := out[1].(*jsonenc.Object)
		assertObjStr(t, c0, "turn_id", "turn-a")
		assertObjInt(t, c0, "index", 1)
		assertObjStr(t, c1, "turn_id", "turn-b")
		assertObjInt(t, c1, "index", 2)
	})

	t.Run("duration_accumulated_rounded", func(t *testing.T) {
		spans := []*aiItem{
			{turnID: "turn-4", eventName: "turn.complete", ts: "2024-01-01T12:00:00Z", durationMS: 100.3},
			{turnID: "turn-4", eventName: "turn.complete", ts: "2024-01-01T12:00:01Z", durationMS: 200.2},
		}
		out := buildAiTraceTurnCards(spans)
		card := out[0].(*jsonenc.Object)
		dv, _ := card.Get("duration_ms")
		if dv != float64(300.5) {
			t.Fatalf("duration_ms = %v (type %T)", dv, dv)
		}
	})
}

// helpers for card field assertions
func assertObjStr(t *testing.T, obj *jsonenc.Object, key, want string) {
	t.Helper()
	v, _ := obj.Get(key)
	if v != want {
		t.Errorf("card[%q] = %v (%T), want %q", key, v, v, want)
	}
}

func assertObjInt(t *testing.T, obj *jsonenc.Object, key string, want int) {
	t.Helper()
	v, _ := obj.Get(key)
	if v != want {
		t.Errorf("card[%q] = %v (%T), want %d", key, v, v, want)
	}
}
