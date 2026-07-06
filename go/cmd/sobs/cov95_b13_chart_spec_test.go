package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---- specFromBody ----------------------------------------------------------------------------

func TestSpecFromBody(t *testing.T) {
	t.Run("valid body with spec object", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/x", bytes.NewBufferString(`{"spec":{"template_id":"gauge_kpi"}}`))
		got := specFromBody(req)
		obj, ok := got.(*jsonenc.Object)
		if !ok {
			t.Fatalf("want *jsonenc.Object, got %T", got)
		}
		if v, _ := obj.Get("template_id"); v != "gauge_kpi" {
			t.Errorf("template_id = %v", v)
		}
	})

	t.Run("missing spec key yields nil", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/x", bytes.NewBufferString(`{}`))
		if got := specFromBody(req); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})

	t.Run("malformed JSON yields nil", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/x", bytes.NewBufferString(`{not json`))
		if got := specFromBody(req); got != nil {
			t.Errorf("want nil, got %v", got)
		}
	})
}

// ---- validateChartQuery -----------------------------------------------------------------------

func TestValidateChartQuery(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{"empty", "   ", "Query cannot be empty"},
		{"not select/with", "UPSERT foo", "Only SELECT queries are allowed"},
		{"select with deny keyword", "SELECT * FROM t; DROP TABLE t", "Query contains a disallowed keyword"},
		{"valid select", "SELECT 1", ""},
		{"valid with", "WITH x AS (SELECT 1) SELECT * FROM x", ""},
		{"lowercase select passes prefix check", "select 1", ""},
		{"delete keyword blocked", "SELECT * FROM t WHERE x IN (DELETE)", "Query contains a disallowed keyword"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := validateChartQuery(c.query); got != c.want {
				t.Errorf("validateChartQuery(%q) = %q, want %q", c.query, got, c.want)
			}
		})
	}
}

// ---- pyStr -------------------------------------------------------------------------------------

func TestPyStr(t *testing.T) {
	cases := []struct {
		name    string
		v       any
		present bool
		want    string
	}{
		{"absent", nil, false, "None"},
		{"nil present", nil, true, "None"},
		{"string", "hi", true, "hi"},
		{"json.Number", json.Number("42"), true, "42"},
		{"bool true", true, true, "True"},
		{"bool false", false, true, "False"},
		{"other type", 3.5, true, "3.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pyStr(c.v, c.present); got != c.want {
				t.Errorf("pyStr(%v,%v) = %q, want %q", c.v, c.present, got, c.want)
			}
		})
	}
}

// ---- pyStrOrStrip -------------------------------------------------------------------------------

func TestPyStrOrStrip(t *testing.T) {
	cases := []struct {
		name    string
		v       any
		present bool
		want    string
	}{
		{"absent", nil, false, ""},
		{"nil present", nil, true, ""},
		{"string trims", "  hi  ", true, "hi"},
		{"number zero", json.Number("0"), true, ""},
		{"number nonzero", json.Number(" 5 "), true, "5"},
		{"bool true", true, true, "True"},
		{"bool false", false, true, ""},
		{"unsupported type", 3.5, true, ""},
		{"unsupported type slice", []any{1, 2}, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pyStrOrStrip(c.v, c.present); got != c.want {
				t.Errorf("pyStrOrStrip(%v,%v) = %q, want %q", c.v, c.present, got, c.want)
			}
		})
	}
}

// ---- coercePositiveInt --------------------------------------------------------------------------

func TestCoercePositiveInt(t *testing.T) {
	cases := []struct {
		name            string
		v               any
		present         bool
		def, minV, maxV int
		want            int
	}{
		{"absent uses default", nil, false, 7, 1, 100, 7},
		{"parse failure uses default", "not-a-number", true, 7, 1, 100, 7},
		{"within range", json.Number("50"), true, 7, 1, 100, 50},
		{"below min clamps", json.Number("-5"), true, 7, 1, 100, 1},
		{"above max clamps", json.Number("1000"), true, 7, 1, 100, 100},
		{"whitespace trimmed", "  42  ", true, 7, 1, 100, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := coercePositiveInt(c.v, c.present, c.def, c.minV, c.maxV); got != c.want {
				t.Errorf("coercePositiveInt() = %d, want %d", got, c.want)
			}
		})
	}
}

// ---- oGet ----------------------------------------------------------------------------------------

func TestOGet(t *testing.T) {
	t.Run("nil object returns not-present", func(t *testing.T) {
		v, ok := oGet(nil, "x")
		if ok || v != nil {
			t.Errorf("oGet(nil,...) = (%v,%v), want (nil,false)", v, ok)
		}
	})
	t.Run("present key", func(t *testing.T) {
		o := jsonenc.NewObject().Set("k", "v")
		v, ok := oGet(o, "k")
		if !ok || v != "v" {
			t.Errorf("oGet = (%v,%v), want (v,true)", v, ok)
		}
	})
	t.Run("missing key", func(t *testing.T) {
		o := jsonenc.NewObject()
		v, ok := oGet(o, "missing")
		if ok || v != nil {
			t.Errorf("oGet = (%v,%v), want (nil,false)", v, ok)
		}
	})
}

// ---- normalizeChartSpec ----------------------------------------------------------------------

func TestNormalizeChartSpecUnknownTemplate(t *testing.T) {
	raw := jsonenc.NewObject().Set("template_id", "not_a_real_template")
	_, errMsg := normalizeChartSpec(raw)
	if errMsg != "Unknown template: not_a_real_template" {
		t.Errorf("errMsg = %q", errMsg)
	}
}

func TestNormalizeChartSpecDefaultTemplateWhenAbsent(t *testing.T) {
	// sql.mode is required regardless (an absent mode fails the builder/raw check per
	// TestNormalizeChartSpecAbsentModeFails), so a valid sql block is supplied here to isolate
	// the behavior under test: template_id defaults to derived_signal_overlay when omitted.
	raw := jsonenc.NewObject().
		Set("sql", jsonenc.NewObject().Set("mode", "raw").Set("override_sql", "SELECT 1"))
	spec, errMsg := normalizeChartSpec(raw)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	tid, _ := spec.Get("template_id")
	if tid != "derived_signal_overlay" {
		t.Errorf("template_id = %v, want derived_signal_overlay", tid)
	}
}

func TestNormalizeChartSpecBadSQLMode(t *testing.T) {
	raw := jsonenc.NewObject().Set("template_id", "gauge_kpi").
		Set("sql", jsonenc.NewObject().Set("mode", "nonsense"))
	_, errMsg := normalizeChartSpec(raw)
	if errMsg != "sql.mode must be 'builder' or 'raw'" {
		t.Errorf("errMsg = %q", errMsg)
	}
}

func TestNormalizeChartSpecAbsentModeFails(t *testing.T) {
	// app.py: sql_raw always a dict; absent "mode" -> str(None)="none" -> fails the builder/raw check.
	raw := jsonenc.NewObject().Set("template_id", "gauge_kpi").Set("sql", jsonenc.NewObject())
	_, errMsg := normalizeChartSpec(raw)
	if errMsg != "sql.mode must be 'builder' or 'raw'" {
		t.Errorf("errMsg = %q, want sql.mode error", errMsg)
	}
}

func TestNormalizeChartSpecMergesDataAndVisual(t *testing.T) {
	raw := jsonenc.NewObject().
		Set("template_id", "gauge_kpi").
		Set("sql", jsonenc.NewObject().Set("mode", "builder").Set("override_sql", "")).
		Set("data", jsonenc.NewObject().Set("service", "checkout").Set("window_hours", json.Number("12"))).
		Set("visual", jsonenc.NewObject().
			Set("legend_show", false).
			Set("role_map", jsonenc.NewObject().Set(" role1 ", "  mapped1  ").Set("empty_role", "").Set("", "x")))
	spec, errMsg := normalizeChartSpec(raw)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	data, _ := spec.Get("data")
	dataObj := data.(*jsonenc.Object)
	if v, _ := dataObj.Get("service"); v != "checkout" {
		t.Errorf("data.service = %v", v)
	}
	// unspecified defaults are preserved
	if v, _ := dataObj.Get("source_view"); v != "v_derived_signals_anomaly" {
		t.Errorf("data.source_view = %v", v)
	}

	visual, _ := spec.Get("visual")
	visualObj := visual.(*jsonenc.Object)
	if v, _ := visualObj.Get("legend_show"); v != false {
		t.Errorf("visual.legend_show = %v", v)
	}
	roleMapV, _ := visualObj.Get("role_map")
	roleMap := roleMapV.(*jsonenc.Object)
	if v, _ := roleMap.Get("role1"); v != "mapped1" {
		t.Errorf("role_map[role1] = %v, want trimmed mapped1", v)
	}
	if _, ok := roleMap.Get("empty_role"); ok {
		t.Errorf("empty_role should have been dropped (empty mapped value)")
	}
	if _, ok := roleMap.Get(""); ok {
		t.Errorf("empty role name should have been dropped")
	}
}

func TestNormalizeChartSpecCustomEchartsDefault(t *testing.T) {
	raw := jsonenc.NewObject().Set("template_id", "custom_echarts").
		Set("sql", jsonenc.NewObject().Set("mode", "raw").Set("override_sql", "SELECT 1"))
	spec, errMsg := normalizeChartSpec(raw)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	visual, _ := spec.Get("visual")
	visualObj := visual.(*jsonenc.Object)
	if _, ok := visualObj.Get("custom_mapping_json"); !ok {
		t.Errorf("custom_echarts default visual missing custom_mapping_json")
	}
}

func TestNormalizeChartSpecNamedQueriesFilter(t *testing.T) {
	named := []any{
		jsonenc.NewObject().Set("name", " Valid_Name ").Set("sql", "SELECT 1;").Set("purpose", "p1"),
		jsonenc.NewObject().Set("name", "Bad Name!").Set("sql", "SELECT 2"), // invalid identifier -> dropped
		jsonenc.NewObject().Set("name", "empty_sql").Set("sql", "   "),      // empty sql -> dropped
		"not-an-object", // non-object item -> skipped
	}
	raw := jsonenc.NewObject().Set("template_id", "gauge_kpi").
		Set("sql", jsonenc.NewObject().Set("mode", "builder").Set("override_sql", "")).
		Set("named_queries", named)
	spec, errMsg := normalizeChartSpec(raw)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	nqV, _ := spec.Get("named_queries")
	nqList := nqV.([]any)
	if len(nqList) != 1 {
		t.Fatalf("want exactly 1 surviving named query, got %d: %+v", len(nqList), nqList)
	}
	nq := nqList[0].(*jsonenc.Object)
	if v, _ := nq.Get("name"); v != "valid_name" {
		t.Errorf("name = %v, want lowercased+trimmed valid_name", v)
	}
	if v, _ := nq.Get("sql"); v != "SELECT 1" {
		t.Errorf("sql = %v, want trailing ; stripped", v)
	}
}

func TestNormalizeChartSpecNamedQueriesNotAList(t *testing.T) {
	raw := jsonenc.NewObject().Set("template_id", "gauge_kpi").
		Set("sql", jsonenc.NewObject().Set("mode", "builder").Set("override_sql", "")).
		Set("named_queries", "not-a-list")
	spec, errMsg := normalizeChartSpec(raw)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	nqV, _ := spec.Get("named_queries")
	if nqList, ok := nqV.([]any); !ok || len(nqList) != 0 {
		t.Errorf("named_queries = %v, want empty list", nqV)
	}
}

// ---- compileChartSpec ------------------------------------------------------------------------

func TestCompileChartSpecPropagatesNormalizeError(t *testing.T) {
	s := &server{}
	_, _, _, errMsg := s.compileChartSpec(jsonenc.NewObject().Set("template_id", "bogus"))
	if errMsg == "" || errMsg != "Unknown template: bogus" {
		t.Errorf("errMsg = %q", errMsg)
	}
}

func TestCompileChartSpecCustomEchartsRequiresRawMode(t *testing.T) {
	s := &server{}
	raw := jsonenc.NewObject().Set("template_id", "custom_echarts").
		Set("sql", jsonenc.NewObject().Set("mode", "builder").Set("override_sql", ""))
	_, _, _, errMsg := s.compileChartSpec(raw)
	if errMsg != "custom_echarts requires sql.mode='raw'" {
		t.Errorf("errMsg = %q", errMsg)
	}
}

func TestCompileChartSpecRawModeValidatesQuery(t *testing.T) {
	// validateChartQuery checks the SELECT/WITH prefix before scanning for disallowed
	// keywords, so a non-SELECT statement like "DROP TABLE foo" fails the prefix check
	// ("Only SELECT queries are allowed") rather than ever reaching the keyword scan. To
	// exercise the keyword-scan branch the query must start with SELECT/WITH but still
	// contain a denied keyword elsewhere.
	s := &server{}
	raw := jsonenc.NewObject().Set("template_id", "gauge_kpi").
		Set("sql", jsonenc.NewObject().Set("mode", "raw").Set("override_sql", "SELECT 1; DROP TABLE foo"))
	_, _, _, errMsg := s.compileChartSpec(raw)
	if errMsg != "Query contains a disallowed keyword" {
		t.Errorf("errMsg = %q", errMsg)
	}
}

func TestCompileChartSpecRawModeSuccess(t *testing.T) {
	s := &server{}
	raw := jsonenc.NewObject().Set("template_id", "gauge_kpi").
		Set("sql", jsonenc.NewObject().Set("mode", "raw").Set("override_sql", "  SELECT 1  "))
	templateID, query, spec, errMsg := s.compileChartSpec(raw)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if templateID != "gauge_kpi" || query != "SELECT 1" || spec == nil {
		t.Errorf("templateID=%q query=%q spec=%v", templateID, query, spec)
	}
}

func TestCompileChartSpecNamedQueryValidationFailure(t *testing.T) {
	s := &server{}
	named := []any{jsonenc.NewObject().Set("name", "bad_one").Set("sql", "SELECT 1; DROP TABLE x")}
	raw := jsonenc.NewObject().Set("template_id", "gauge_kpi").
		Set("sql", jsonenc.NewObject().Set("mode", "raw").Set("override_sql", "SELECT 1")).
		Set("named_queries", named)
	_, _, _, errMsg := s.compileChartSpec(raw)
	want := "Named query 'bad_one': Query contains a disallowed keyword"
	if errMsg != want {
		t.Errorf("errMsg = %q, want %q", errMsg, want)
	}
}

func TestCompileChartSpecBuilderModeNonEchartsTemplate(t *testing.T) {
	// Builder-mode compilation for a template other than custom_echarts routes through
	// compileBuilderSQL rather than the raw-override path exercised above.
	s := &server{}
	raw := jsonenc.NewObject().Set("template_id", "time_series_percentiles").
		Set("sql", jsonenc.NewObject().Set("mode", "builder").Set("override_sql", ""))
	templateID, query, _, errMsg := s.compileChartSpec(raw)
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if templateID != "time_series_percentiles" || query == "" {
		t.Errorf("templateID=%q query=%q", templateID, query)
	}
}

// ---- isTruthyVal ------------------------------------------------------------------------------

func TestIsTruthyVal(t *testing.T) {
	cases := []struct {
		name    string
		v       any
		present bool
		want    bool
	}{
		{"absent", nil, false, false},
		{"nil present", nil, true, false},
		{"empty string", "", true, false},
		{"nonempty string", "x", true, true},
		{"bool false", false, true, false},
		{"bool true", true, true, true},
		{"number zero", json.Number("0"), true, false},
		{"number nonzero", json.Number("3"), true, true},
		{"number invalid text", json.Number("abc"), true, true}, // Float64 err -> falls through to true (not zero)
		{"empty object", jsonenc.NewObject(), true, false},
		{"nonempty object", jsonenc.NewObject().Set("a", 1), true, true},
		{"empty slice", []any{}, true, false},
		{"nonempty slice", []any{1}, true, true},
		{"other type default true", 3.14, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isTruthyVal(c.v, c.present); got != c.want {
				t.Errorf("isTruthyVal(%v,%v) = %v, want %v", c.v, c.present, got, c.want)
			}
		})
	}
}

// ---- numEquals ---------------------------------------------------------------------------------

func TestNumEquals(t *testing.T) {
	cases := []struct {
		name    string
		v       any
		present bool
		n       float64
		want    bool
	}{
		{"absent", nil, false, 1, false},
		{"matching number", json.Number("1"), true, 1, true},
		{"matching float-string", json.Number("1.0"), true, 1, true},
		{"non-matching number", json.Number("2"), true, 1, false},
		{"non-number type", "1", true, 1, false},
		{"invalid number text", json.Number("nope"), true, 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := numEquals(c.v, c.present, c.n); got != c.want {
				t.Errorf("numEquals(%v,%v,%v) = %v, want %v", c.v, c.present, c.n, got, c.want)
			}
		})
	}
}
