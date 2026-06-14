package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// specFromBody reads the request body and returns body["spec"] as an ordered value (or nil).
// Used by the /api/dashboards/spec/* routes, which need int-preserving (json.Number) parsing.
func specFromBody(r *http.Request) any {
	raw, _ := io.ReadAll(r.Body)
	v, err := parseJSONValue(raw)
	if err != nil {
		return nil
	}
	spec, _ := asObject(v).Get("spec")
	return spec
}

// Chart-spec compilation — a faithful port of app.py's _compile_chart_spec chain
// (_normalize_chart_spec, _default_chart_spec, _compile_builder_sql, _validate_chart_query).
// Pure deterministic transform: a chart spec (builder or raw) -> (template_id, SQL, normalized
// spec). API responses sort keys recursively, so only the values/structure must match, not the
// jsonenc.Object insertion order.

var chartTemplateIDs = map[string]bool{
	"time_series_percentiles": true, "heatmap": true, "box_plot": true,
	"dual_axis_anomaly": true, "anomaly_overlay": true, "derived_signal_overlay": true,
	"gauge_kpi": true, "custom_echarts": true,
}

var queryDenyRe = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|DROP|ALTER|CREATE|TRUNCATE|REPLACE|RENAME|ATTACH|DETACH|GRANT|REVOKE)\b`)

// validateChartQuery mirrors _validate_chart_query: returns an error string ("" when valid).
func validateChartQuery(query string) string {
	stripped := strings.TrimSpace(query)
	if stripped == "" {
		return "Query cannot be empty"
	}
	upper := strings.ToUpper(stripped)
	if !strings.HasPrefix(upper, "SELECT") && !strings.HasPrefix(upper, "WITH") {
		return "Only SELECT queries are allowed"
	}
	if queryDenyRe.MatchString(stripped) {
		return "Query contains a disallowed keyword"
	}
	return ""
}

// pyStr mirrors Python str(x): an absent/None value renders as "None".
func pyStr(v any, present bool) string {
	if !present || v == nil {
		return "None"
	}
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// pyStrOrStrip mirrors str(x or "").strip() for the builder-mode string fields.
func pyStrOrStrip(v any, present bool) string {
	if !present || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		if x.String() == "0" {
			return ""
		}
		return strings.TrimSpace(x.String())
	case bool:
		if !x {
			return ""
		}
		return "True"
	default:
		return ""
	}
}

// coercePositiveInt mirrors _coerce_positive_int(raw, default, min, max): int(str(raw)) clamped,
// falling back to default on parse failure.
func coercePositiveInt(v any, present bool, def, minV, maxV int) int {
	s := pyStr(v, present)
	parsed, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	if parsed < minV {
		return minV
	}
	if parsed > maxV {
		return maxV
	}
	return parsed
}

func asObject(v any) *jsonenc.Object {
	if o, ok := v.(*jsonenc.Object); ok {
		return o
	}
	return jsonenc.NewObject()
}

func oGet(o *jsonenc.Object, key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	return o.Get(key)
}

// defaultChartSpec mirrors _default_chart_spec(template_id).
func defaultChartSpec(templateID string) *jsonenc.Object {
	data := jsonenc.NewObject().
		Set("source_view", "v_derived_signals_anomaly").Set("service", "").
		Set("signal_source", "traces").Set("signal_name", "trace_volume").
		Set("metric_name", "").Set("attr_fp", "").Set("window_hours", 6).Set("limit", 1000)
	if templateID == "custom_echarts" {
		customMapping := jsonenc.Encode(
			jsonenc.NewObject().Set("points", jsonenc.NewObject().Set("from", "rows")),
			jsonDumpsDefault)
		optionObj := jsonenc.NewObject().
			Set("tooltip", jsonenc.NewObject().Set("trigger", "axis")).
			Set("xAxis", jsonenc.NewObject().Set("type", "time")).
			Set("yAxis", jsonenc.NewObject().Set("type", "value")).
			Set("series", []any{jsonenc.NewObject().
				Set("name", "Value").Set("type", "line").Set("data", "{{points}}").
				Set("showSymbol", false).Set("smooth", true)})
		customOption := jsonenc.Encode(optionObj, jsonDumpsDefault)
		return jsonenc.NewObject().
			Set("template_id", templateID).
			Set("sql", jsonenc.NewObject().Set("mode", "raw").
				Set("override_sql", "SELECT toDateTime('2024-01-01 00:00:00') AS time, 1 AS value")).
			Set("data", data).
			Set("visual", jsonenc.NewObject().
				Set("zoom_inside", true).Set("zoom_slider", false).
				Set("zoom_start_pct", 0).Set("zoom_end_pct", 100).
				Set("legend_show", true).Set("smooth_line", true).Set("value_color", "").
				Set("role_map", jsonenc.NewObject()).
				Set("custom_mapping_json", string(customMapping)).
				Set("custom_option_json", string(customOption)))
	}
	return jsonenc.NewObject().
		Set("template_id", templateID).
		Set("sql", jsonenc.NewObject().Set("mode", "builder").Set("override_sql", "")).
		Set("data", data).
		Set("visual", jsonenc.NewObject().
			Set("zoom_inside", true).Set("zoom_slider", false).
			Set("zoom_start_pct", 0).Set("zoom_end_pct", 100).
			Set("legend_show", true).Set("smooth_line", true).Set("value_color", "").
			Set("role_map", jsonenc.NewObject()))
}

// jsonDumpsDefault mirrors json.dumps(..., ensure_ascii=False) — default separators, no sort.
var jsonDumpsDefault = jsonenc.Options{SortKeys: false, EnsureASCII: false, ItemSep: ", ", KeySep: ": "}

var namedQueryNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// normalizeChartSpec mirrors _normalize_chart_spec: validate template, then rebuild sql/data/
// visual/named_queries from the defaults merged with the raw input. Returns a ValueError-style
// message for an unknown template / bad sql.mode.
func normalizeChartSpec(rawAny any) (*jsonenc.Object, string) {
	raw := asObject(rawAny)
	tidV, tidOK := oGet(raw, "template_id")
	templateID := strings.TrimSpace(firstNonEmpty(pyStrOrStrip(tidV, tidOK), "derived_signal_overlay"))
	if !chartTemplateIDs[templateID] {
		return nil, "Unknown template: " + templateID
	}
	normalized := defaultChartSpec(templateID)
	normalized.Set("template_id", templateID)

	sqlV, _ := oGet(raw, "sql")
	sqlRaw := asObject(sqlV)
	// app.py: str(sql_raw.get("mode")).strip().lower() — sql_raw is always a dict, so an absent
	// mode is str(None)="none" (it does NOT default to "builder"), failing the builder/raw check.
	modeV, modeOK := oGet(sqlRaw, "mode")
	sqlMode := strings.ToLower(strings.TrimSpace(pyStr(modeV, modeOK)))
	if sqlMode != "builder" && sqlMode != "raw" {
		return nil, "sql.mode must be 'builder' or 'raw'"
	}
	overrideV, overrideOK := oGet(sqlRaw, "override_sql")
	normalized.Set("sql", jsonenc.NewObject().Set("mode", sqlMode).Set("override_sql", pyStr(overrideV, overrideOK)))

	// data: default data updated with raw data.
	dataV, _ := oGet(raw, "data")
	dataRaw := asObject(dataV)
	mergedData := cloneObject(asObject(mustGet(normalized, "data")))
	for _, k := range dataRaw.Keys() {
		v, _ := dataRaw.Get(k)
		mergedData.Set(k, v)
	}
	normalized.Set("data", mergedData)

	// visual: default visual updated with raw visual, then role_map normalized.
	visualV, _ := oGet(raw, "visual")
	visualRaw := asObject(visualV)
	mergedVisual := cloneObject(asObject(mustGet(normalized, "visual")))
	for _, k := range visualRaw.Keys() {
		v, _ := visualRaw.Get(k)
		mergedVisual.Set(k, v)
	}
	roleMap := jsonenc.NewObject()
	if rmV, ok := mergedVisual.Get("role_map"); ok {
		if rmObj, ok := rmV.(*jsonenc.Object); ok {
			for _, role := range rmObj.Keys() {
				cv, _ := rmObj.Get(role)
				roleName := strings.TrimSpace(role)
				mapped := strings.TrimSpace(pyStr(cv, true))
				if roleName != "" && mapped != "" {
					roleMap.Set(roleName, mapped)
				}
			}
		}
	}
	mergedVisual.Set("role_map", roleMap)
	normalized.Set("visual", mergedVisual)

	// named_queries: filter+normalize.
	named := []any{}
	if nqV, ok := oGet(raw, "named_queries"); ok {
		if list, ok := nqV.([]any); ok {
			for _, item := range list {
				itemObj, ok := item.(*jsonenc.Object)
				if !ok {
					continue
				}
				nameV, nameOK := itemObj.Get("name")
				name := strings.ToLower(strings.TrimSpace(pyStrOrStrip(nameV, nameOK)))
				sqlTextV, sqlTextOK := itemObj.Get("sql")
				sqlText := strings.TrimRight(strings.TrimSpace(pyStrOrStrip(sqlTextV, sqlTextOK)), ";")
				purposeV, purposeOK := itemObj.Get("purpose")
				purpose := strings.TrimSpace(pyStrOrStrip(purposeV, purposeOK))
				if name == "" || !namedQueryNameRe.MatchString(name) {
					continue
				}
				if sqlText == "" {
					continue
				}
				named = append(named, jsonenc.NewObject().
					Set("name", name).Set("sql", sqlText).Set("purpose", purpose))
			}
		}
	}
	normalized.Set("named_queries", named)
	return normalized, ""
}

// compileChartSpec mirrors _compile_chart_spec: returns (template_id, query, normalizedSpec) or
// a ValueError-style message.
func (s *server) compileChartSpec(rawAny any) (string, string, *jsonenc.Object, string) {
	spec, errMsg := normalizeChartSpec(rawAny)
	if errMsg != "" {
		return "", "", nil, errMsg
	}
	templateID := strings.TrimSpace(firstNonEmpty(pyStrOrStrip(mustGet(spec, "template_id"), true), "time_series_percentiles"))
	sqlBlock := asObject(mustGet(spec, "sql"))
	modeV, _ := sqlBlock.Get("mode")
	sqlMode := strings.ToLower(strings.TrimSpace(pyStrOrStrip(modeV, true)))

	var query string
	if sqlMode == "raw" {
		overrideV, _ := sqlBlock.Get("override_sql")
		query = strings.TrimSpace(pyStr(overrideV, true))
	} else {
		if templateID == "custom_echarts" {
			return "", "", nil, "custom_echarts requires sql.mode='raw'"
		}
		q, err := compileBuilderSQL(templateID, asObject(mustGet(spec, "data")))
		if err != "" {
			return "", "", nil, err
		}
		query = q
	}
	if e := validateChartQuery(query); e != "" {
		return "", "", nil, e
	}
	// Validate named queries (read-only check).
	if nqV, ok := spec.Get("named_queries"); ok {
		if list, ok := nqV.([]any); ok {
			for _, nq := range list {
				nqObj, ok := nq.(*jsonenc.Object)
				if !ok {
					continue
				}
				sv, sok := nqObj.Get("sql")
				nqSQL := strings.TrimSpace(pyStrOrStrip(sv, sok))
				nv, nok := nqObj.Get("name")
				nqName := strings.TrimSpace(pyStrOrStrip(nv, nok))
				if nqSQL != "" {
					if e := validateChartQuery(nqSQL); e != "" {
						return "", "", nil, "Named query '" + nqName + "': " + e
					}
				}
			}
		}
	}
	return templateID, query, spec, ""
}

// --- helpers ---

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func mustGet(o *jsonenc.Object, key string) any {
	v, _ := o.Get(key)
	return v
}

// isTruthyVal mirrors Python truthiness (for `if not x` / `if x`): nil/""/0/false/empty
// object/empty list are falsy.
func isTruthyVal(v any, present bool) bool {
	if !present || v == nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return x != ""
	case bool:
		return x
	case json.Number:
		f, err := x.Float64()
		return !(err == nil && f == 0)
	case *jsonenc.Object:
		return x.Len() > 0
	case []any:
		return len(x) > 0
	default:
		return true
	}
}

// numEquals reports whether v is numerically equal to n (Python `x == 1`, where 1.0 == 1).
func numEquals(v any, present bool, n float64) bool {
	if !present {
		return false
	}
	if num, ok := v.(json.Number); ok {
		f, err := num.Float64()
		return err == nil && f == n
	}
	return false
}

func cloneObject(o *jsonenc.Object) *jsonenc.Object {
	out := jsonenc.NewObject()
	for _, k := range o.Keys() {
		v, _ := o.Get(k)
		out.Set(k, v)
	}
	return out
}
