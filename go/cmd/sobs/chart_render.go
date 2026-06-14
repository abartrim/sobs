package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// chartTemplateColMeta is the column-count + role metadata from CHART_TEMPLATES, used by the
// render-validation path (min/max columns, role indices).
type chartTemplateColMeta struct {
	min, max int // max == -1 means unbounded (custom_echarts)
	roles    map[string]int
}

var chartTemplateMeta = map[string]chartTemplateColMeta{
	"time_series_percentiles": {4, 4, map[string]int{"time": 0, "value": 1, "p95": 2, "p99": 3}},
	"heatmap":                 {3, 3, map[string]int{"x_category": 0, "y_category": 1, "value": 2}},
	"box_plot":                {6, 6, map[string]int{"dimension": 0, "min": 1, "q1": 2, "median": 3, "q3": 4, "max": 5}},
	"dual_axis_anomaly":       {3, 3, map[string]int{"time": 0, "metric": 1, "anomaly_score": 2}},
	"anomaly_overlay":         {6, 6, map[string]int{"time": 0, "value": 1, "baseline_mean": 2, "baseline_lower": 3, "baseline_upper": 4, "anomaly_state": 5}},
	"derived_signal_overlay": {12, 16, map[string]int{"time": 0, "service": 1, "source": 2, "signal": 3, "attr_fp": 4,
		"value": 5, "sample_count": 6, "baseline_mean": 7, "baseline_lower": 8, "baseline_upper": 9,
		"anomaly_state": 10, "anomaly_score": 11, "rule_state": 12, "rule_name": 13, "rule_reason": 14, "effective_state": 15}},
	"gauge_kpi":      {1, 1, map[string]int{"value": 0}},
	"custom_echarts": {0, -1, map[string]int{}},
}

// renderWouldError mirrors the raise conditions of app.py _render_chart_from_template WITHOUT
// producing the echarts option (used by spec/validate, whose response is only valid/columns/
// row_count). Returns "" when the render would succeed. NOTE: the full binding/substitution
// pipeline (the OPTION output for spec/render + dashboards/render) is not yet ported; those
// routes remain. For empty results (every builder query on this fixture) render returns the
// fixed "No data" placeholder and never raises.
func renderWouldError(templateID string, columns []any, rows []any, spec *jsonenc.Object) string {
	meta, ok := chartTemplateMeta[templateID]
	if !ok {
		return "Unknown template: " + templateID
	}
	if templateID == "custom_echarts" {
		return customEchartsWouldError(spec)
	}
	if len(rows) == 0 {
		return "" // placeholder, no raise
	}
	if len(columns) < meta.min {
		return "Template " + templateID + " requires at least " + strconv.Itoa(meta.min) +
			" columns, got " + strconv.Itoa(len(columns))
	}
	if meta.max > 0 && len(columns) > meta.max {
		return "Template " + templateID + " accepts maximum " + strconv.Itoa(meta.max) +
			" columns, got " + strconv.Itoa(len(columns))
	}
	return resolveRoleMapError(templateID, meta, columns, spec)
}

// resolveRoleMapError mirrors the raise conditions of _resolve_template_role_indices for an
// explicit spec.visual.role_map (unknown role / unknown column).
func resolveRoleMapError(templateID string, meta chartTemplateColMeta, columns []any, spec *jsonenc.Object) string {
	if spec == nil {
		return ""
	}
	visualV, _ := spec.Get("visual")
	visual, ok := visualV.(*jsonenc.Object)
	if !ok {
		return ""
	}
	rmV, _ := visual.Get("role_map")
	roleMap, ok := rmV.(*jsonenc.Object)
	if !ok {
		return ""
	}
	colIndex := map[string]int{}
	lowerIndex := map[string]int{}
	for i, c := range columns {
		name, _ := c.(string)
		if _, seen := colIndex[name]; !seen {
			colIndex[name] = i
		}
		lc := strings.ToLower(name)
		if _, seen := lowerIndex[lc]; !seen {
			lowerIndex[lc] = i
		}
	}
	for _, role := range roleMap.Keys() {
		cv, _ := roleMap.Get(role)
		roleName := strings.TrimSpace(role)
		colName := strings.TrimSpace(pyStr(cv, true))
		if roleName == "" || colName == "" {
			continue
		}
		if _, known := meta.roles[roleName]; !known {
			return "Unknown role '" + roleName + "' for template " + templateID
		}
		if _, ok := colIndex[colName]; ok {
			continue
		}
		if _, ok := lowerIndex[strings.ToLower(colName)]; ok {
			continue
		}
		return "Role '" + roleName + "' maps to unknown column '" + colName + "'"
	}
	return ""
}

// customEchartsWouldError mirrors the JSON-parse validation in _render_custom_echarts (the only
// pre-data raise path): custom_option_json / custom_mapping_json must be valid JSON.
func customEchartsWouldError(spec *jsonenc.Object) string {
	if spec == nil {
		return ""
	}
	visualV, _ := spec.Get("visual")
	visual, ok := visualV.(*jsonenc.Object)
	if !ok {
		return ""
	}
	for field, label := range map[string]string{
		"custom_option_json": "custom_option_json", "custom_mapping_json": "custom_mapping_json",
	} {
		v, has := visual.Get(field)
		if !has || v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			continue // already a dict/list -> valid
		}
		if strings.TrimSpace(s) == "" {
			continue
		}
		if _, err := parseJSONValue([]byte(s)); err != nil {
			return label + " must be valid JSON"
		}
	}
	return ""
}

// handleApiDashboardsSpecValidate — app.py validate_chart_spec_api: compile, execute (LIMIT 200),
// render (raise-check only), and report {valid, template_id, query, spec, columns, row_count}.
func (s *server) handleApiDashboardsSpecValidate(w http.ResponseWriter, r *http.Request) {
	tid, query, spec, errMsg := s.compileChartSpec(specFromBody(r))
	if errMsg != "" {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("valid", false).Set("error", errMsg))
		return
	}
	res, err := s.db.Execute(injectLimit(query, 200))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().
			Set("valid", false).Set("error", publicDashboardQueryError(err)))
		return
	}
	columns, rows := serializeQueryResult(res)
	if e := renderWouldError(tid, columns, rows, spec); e != "" {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("valid", false).Set("error", e))
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("valid", true).Set("template_id", tid).Set("query", query).Set("spec", spec).
		Set("columns", columns).Set("row_count", len(rows)))
}
