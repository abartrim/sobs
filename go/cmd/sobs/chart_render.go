package main

import (
	"net/http"

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

// handleApiDashboardsSpecValidate — app.py validate_chart_spec_api: compile, execute (LIMIT 200),
// actually render via _render_chart_from_template (catching ANY exception -> {valid:false,error}
// 400), and report {valid, template_id, query, spec, columns, row_count}. The full render pipeline
// is now ported, so this runs it for real (matching Python) instead of the prior column-count/
// role/JSON-parse-only pre-check. NOTE: validate does NOT pass named_datasets or apply visual
// overrides (Python passes neither here).
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
	columns, dictRows := serializeQueryDictRows(res)
	if _, rErr := s.renderChartFromTemplate(tid, columns, dictRows, spec); rErr != "" {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("valid", false).Set("error", rErr))
		return
	}
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("valid", true).Set("template_id", tid).Set("query", query).Set("spec", spec).
		Set("columns", columns).Set("row_count", len(dictRows)))
}
