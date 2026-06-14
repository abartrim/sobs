package main

import (
	_ "embed"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Echarts render pipeline — a port of app.py _render_chart_from_template + _extract_bindings +
// _deep_substitute + _attach_drilldown_metadata. The per-template echarts_option_template +
// drilldown data is embedded (a pure-static asset extracted from CHART_TEMPLATES).

//go:embed assets/chart_echarts_templates.json
var chartEchartsTemplatesJSON []byte

type echartsTemplate struct {
	option    any // *jsonenc.Object tree (option template, before substitution)
	drilldown *jsonenc.Object
}

var echartsTemplates = func() map[string]echartsTemplate {
	out := map[string]echartsTemplate{}
	v, err := parseJSONValue(chartEchartsTemplatesJSON)
	if err != nil {
		return out
	}
	root, ok := v.(*jsonenc.Object)
	if !ok {
		return out
	}
	for _, tid := range root.Keys() {
		tv, _ := root.Get(tid)
		to, ok := tv.(*jsonenc.Object)
		if !ok {
			continue
		}
		opt, _ := to.Get("echarts_option_template")
		dd, _ := to.Get("drilldown")
		ddObj, _ := dd.(*jsonenc.Object)
		out[tid] = echartsTemplate{option: opt, drilldown: ddObj}
	}
	return out
}()

// noDataPlaceholder mirrors the empty-rows return of _render_chart_from_template.
func noDataPlaceholder() *jsonenc.Object {
	return jsonenc.NewObject().
		Set("backgroundColor", "transparent").
		Set("textStyle", jsonenc.NewObject().Set("color", "#adb5bd")).
		Set("title", jsonenc.NewObject().
			Set("text", "No data for selected query/time window").
			Set("left", "center").Set("top", "middle").
			Set("textStyle", jsonenc.NewObject().Set("color", "#6c757d").
				Set("fontSize", 13).Set("fontWeight", 500))).
		Set("series", []any{}).
		Set("xAxis", jsonenc.NewObject().Set("show", false)).
		Set("yAxis", jsonenc.NewObject().Set("show", false))
}

// renderChartFromTemplate mirrors app.py _render_chart_from_template. rows are dict rows
// (column name -> serialized value). Returns (option, errMsg).
func (s *server) renderChartFromTemplate(templateID string, columns []any, rows []map[string]any, spec *jsonenc.Object) (any, string) {
	tmpl, ok := echartsTemplates[templateID]
	meta, metaOK := chartTemplateMeta[templateID]
	if !ok || !metaOK {
		return nil, "Unknown template: " + templateID
	}
	// custom_echarts (the custom-mapping subsystem) and derived_signal_overlay (the anomaly
	// rule-evaluation engine) render paths are follow-up phases; signal not-implemented so the
	// caller keeps a 501 rather than emitting a wrong-but-plausible body.
	if templateID == "custom_echarts" || templateID == "derived_signal_overlay" {
		return nil, renderNotImplemented
	}
	if len(rows) == 0 {
		return noDataPlaceholder(), ""
	}
	if len(columns) < meta.min {
		return nil, "Template " + templateID + " requires at least " + itoa(meta.min) + " columns, got " + itoa(len(columns))
	}
	if meta.max > 0 && len(columns) > meta.max {
		return nil, "Template " + templateID + " accepts maximum " + itoa(meta.max) + " columns, got " + itoa(len(columns))
	}
	roleIndices, e := resolveTemplateRoleIndices(templateID, meta, columns, spec)
	if e != "" {
		return nil, e
	}
	bindings := extractBindings(templateID, columns, rows, roleIndices)
	option := deepSubstitute(deepCopyJSON(tmpl.option), bindings)
	if obj, ok := option.(*jsonenc.Object); ok {
		attachDrilldownMetadata(templateID, tmpl.drilldown, bindings, obj)
		if _, has := obj.Get("backgroundColor"); !has {
			obj.Set("backgroundColor", "transparent")
		}
		if _, has := obj.Get("textStyle"); !has {
			obj.Set("textStyle", jsonenc.NewObject().Set("color", "#adb5bd"))
		}
	}
	return option, ""
}

// resolveTemplateRoleIndices mirrors _resolve_template_role_indices: template roles, overridden
// by spec.visual.role_map (mapping role -> column name/lowercased).
func resolveTemplateRoleIndices(templateID string, meta chartTemplateColMeta, columns []any, spec *jsonenc.Object) (map[string]int, string) {
	roleIndices := map[string]int{}
	for role, idx := range meta.roles {
		roleIndices[role] = idx
	}
	if spec == nil {
		return roleIndices, ""
	}
	visualV, _ := spec.Get("visual")
	visual, ok := visualV.(*jsonenc.Object)
	if !ok {
		return roleIndices, ""
	}
	rmV, _ := visual.Get("role_map")
	roleMap, ok := rmV.(*jsonenc.Object)
	if !ok {
		return roleIndices, ""
	}
	colIndex := map[string]int{}
	lowerIndex := map[string]int{}
	for i, c := range columns {
		name := toStr(c)
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
		colName := strings.TrimSpace(toStr(cv))
		if roleName == "" || colName == "" {
			continue
		}
		if _, known := meta.roles[roleName]; !known {
			return nil, "Unknown role '" + roleName + "' for template " + templateID
		}
		if idx, ok := colIndex[colName]; ok {
			roleIndices[roleName] = idx
			continue
		}
		if idx, ok := lowerIndex[strings.ToLower(colName)]; ok {
			roleIndices[roleName] = idx
			continue
		}
		return nil, "Role '" + roleName + "' maps to unknown column '" + colName + "'"
	}
	return roleIndices, ""
}

// deepCopyJSON clones a parsed-JSON value tree (so substitution does not mutate the embed).
func deepCopyJSON(v any) any {
	switch x := v.(type) {
	case *jsonenc.Object:
		out := jsonenc.NewObject()
		for _, k := range x.Keys() {
			cv, _ := x.Get(k)
			out.Set(k, deepCopyJSON(cv))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = deepCopyJSON(item)
		}
		return out
	default:
		return v
	}
}

// deepSubstitute mirrors _deep_substitute: replace a string that contains any {{key}} placeholder
// (for a key in bindings) with the binding value (whole value, not interpolation); nil -> keep.
func deepSubstitute(obj any, bindings map[string]any) any {
	switch x := obj.(type) {
	case *jsonenc.Object:
		out := jsonenc.NewObject()
		for _, k := range x.Keys() {
			v, _ := x.Get(k)
			out.Set(k, deepSubstitute(v, bindings))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = deepSubstitute(item, bindings)
		}
		return out
	case string:
		for key, value := range bindings {
			if strings.Contains(x, "{{"+key+"}}") {
				if value != nil {
					return value
				}
				return x
			}
		}
		return x
	default:
		return obj
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// numOf coerces a serialized cell value (json.Number/float64/int/string) to float64.
func numOf(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// extractBindings mirrors app.py _extract_bindings (the template-agnostic + heatmap/box/gauge/
// anomaly-symbol parts). The derived_signal_overlay-specific block is added in a later phase.
func extractBindings(templateID string, columns []any, rows []map[string]any, roleIndices map[string]int) map[string]any {
	bindings := map[string]any{}
	for role, colIdx := range roleIndices {
		if colIdx < 0 || colIdx >= len(columns) {
			continue
		}
		colName := toStr(columns[colIdx])
		vals := make([]any, len(rows))
		for i, row := range rows {
			vals[i] = row[colName]
		}
		bindings[role] = vals
	}

	// Heatmap: unique X/Y + matrix + value range.
	if xv, okx := bindings["x_category"].([]any); okx {
		if yv, oky := bindings["y_category"].([]any); oky {
			if vv, okv := bindings["value"].([]any); okv {
				xUnique := sortedUniqueAny(xv)
				yUnique := sortedUniqueAny(yv)
				bindings["x_unique_values"] = xUnique
				bindings["y_unique_values"] = yUnique
				heatmap := []any{}
				for i, xVal := range xUnique {
					for j, yVal := range yUnique {
						for k := 0; k < len(xv) && k < len(yv) && k < len(vv); k++ {
							if anyEqual(xv[k], xVal) && anyEqual(yv[k], yVal) {
								heatmap = append(heatmap, []any{i, j, vv[k]})
								break
							}
						}
					}
				}
				bindings["heatmap_data"] = heatmap
				vmin, vmax, any := 0.0, 1.0, false
				for _, v := range vv {
					if f, ok := numOf(v); ok {
						if !any {
							vmin, vmax, any = f, f, true
						} else {
							if f < vmin {
								vmin = f
							}
							if f > vmax {
								vmax = f
							}
						}
					}
				}
				if any {
					bindings["value_min"] = vmin
					bindings["value_max"] = vmax
				} else {
					bindings["value_min"] = 0
					bindings["value_max"] = 1
				}
			}
		}
	}

	// Box plot: [min, q1, median, q3, max] per dimension.
	if _, hasMin := bindings["min"]; hasMin {
		if _, hasMax := bindings["max"]; hasMax {
			mn, _ := bindings["min"].([]any)
			q1, _ := bindings["q1"].([]any)
			md, _ := bindings["median"].([]any)
			q3, _ := bindings["q3"].([]any)
			mx, _ := bindings["max"].([]any)
			n := minInt5(len(mn), len(q1), len(md), len(q3), len(mx))
			box := []any{}
			for i := 0; i < n; i++ {
				box = append(box, []any{mn[i], q1[i], md[i], q3[i], mx[i]})
			}
			bindings["boxplot_data"] = box
			if dim, ok := bindings["dimension"]; ok {
				bindings["dimension_values"] = dim
			} else {
				bindings["dimension_values"] = []any{}
			}
		}
	}

	// Gauge: first value.
	if vlist, ok := bindings["value"].([]any); ok && len(vlist) > 0 {
		bindings["value_first"] = vlist[0]
	}

	// Anomaly overlays: per-point symbol size + color.
	stateBinding := bindings["effective_state"]
	if stateBinding == nil {
		stateBinding = bindings["anomaly_state"]
	}
	if states, ok := stateBinding.([]any); ok {
		colors := map[string]string{"outlier": "#dc3545", "warning": "#ffc107", "normal": "#0d6efd"}
		sizes := map[string]int{"outlier": 10, "warning": 7, "normal": 4}
		pc := make([]any, len(states))
		ss := make([]any, len(states))
		for i, st := range states {
			key := toStr(st)
			if c, ok := colors[key]; ok {
				pc[i] = c
			} else {
				pc[i] = "#0d6efd"
			}
			if sz, ok := sizes[key]; ok {
				ss[i] = sz
			} else {
				ss[i] = 4
			}
		}
		bindings["anomaly_point_color"] = pc
		bindings["anomaly_symbol_size"] = ss
	}

	return bindings
}

func minInt5(a, b, c, d, e int) int {
	m := a
	for _, x := range []int{b, c, d, e} {
		if x < m {
			m = x
		}
	}
	return m
}

// sortedUniqueAny mirrors Python sorted(set(values)) for the heatmap categories (numbers sort
// before strings; within a type, natural order).
func sortedUniqueAny(vals []any) []any {
	seen := map[string]bool{}
	uniq := []any{}
	for _, v := range vals {
		k := anyKey(v)
		if !seen[k] {
			seen[k] = true
			uniq = append(uniq, v)
		}
	}
	sort.SliceStable(uniq, func(i, j int) bool {
		fi, oki := numOf(uniq[i])
		fj, okj := numOf(uniq[j])
		if oki && okj {
			return fi < fj
		}
		if oki != okj {
			return oki // numbers before strings
		}
		return toStr(uniq[i]) < toStr(uniq[j])
	})
	return uniq
}

func anyKey(v any) string { return toStr(v) }
func anyEqual(a, b any) bool {
	if fa, oka := numOf(a); oka {
		if fb, okb := numOf(b); okb {
			return fa == fb
		}
	}
	return toStr(a) == toStr(b)
}

// attachDrilldownMetadata mirrors app.py _attach_drilldown_metadata for the time-based + heatmap
// templates (derived_signal_overlay anomaly-metadata variant is added with that template's phase).
func attachDrilldownMetadata(templateID string, drilldown *jsonenc.Object, bindings map[string]any, option *jsonenc.Object) {
	if drilldown == nil {
		return
	}
	seriesV, _ := option.Get("series")
	series, ok := seriesV.([]any)
	if !ok {
		return
	}
	bucketSeconds, _ := drilldown.Get("bucket_seconds")

	timeTemplates := map[string]bool{"time_series_percentiles": true, "dual_axis_anomaly": true, "anomaly_overlay": true, "derived_signal_overlay": true}
	if timeTemplates[templateID] {
		timeValues, _ := bindings["time"].([]any)
		if timeValues == nil {
			return
		}
		isAnomaly := templateID == "anomaly_overlay" || templateID == "derived_signal_overlay"
		anomalyStates, _ := bindings["anomaly_state"].([]any)
		anomalyScores, _ := bindings["anomaly_score"].([]any)
		for _, se := range series {
			seo, ok := se.(*jsonenc.Object)
			if !ok {
				continue
			}
			dataV, _ := seo.Get("data")
			data, ok := dataV.([]any)
			if !ok || len(data) != len(timeValues) {
				continue
			}
			nameV, _ := seo.Get("name")
			isValueSeries := isAnomaly && toStr(nameV) == "Value"
			newData := make([]any, len(data))
			for idx, value := range data {
				dd := jsonenc.NewObject().
					Set("from_ts", formatDrilldownTime(timeValues[idx])).
					Set("window_s", bucketSeconds)
				if isValueSeries {
					st := "normal"
					if idx < len(anomalyStates) {
						st = toStr(anomalyStates[idx])
					}
					var sc any = 0
					if idx < len(anomalyScores) {
						sc = anomalyScores[idx]
					}
					dd.Set("_anomaly_state", st).Set("_anomaly_score", sc)
					if templateID == "derived_signal_overlay" {
						attachDerivedDrilldownFields(dd, bindings, idx)
					}
				}
				newData[idx] = jsonenc.NewObject().Set("value", value).Set("drilldown", dd)
			}
			seo.Set("data", newData)
		}
		return
	}

	if templateID == "heatmap" && len(series) > 0 {
		xUnique, _ := bindings["x_unique_values"].([]any)
		yUnique, _ := bindings["y_unique_values"].([]any)
		fs, ok := series[0].(*jsonenc.Object)
		if !ok || xUnique == nil || yUnique == nil {
			return
		}
		dataV, _ := fs.Get("data")
		data, ok := dataV.([]any)
		if !ok {
			return
		}
		newData := make([]any, len(data))
		for i, item := range data {
			pt, ok := item.([]any)
			if !ok || len(pt) < 3 {
				newData[i] = item
				continue
			}
			xIdx, _ := numOf(pt[0])
			yIdx, _ := numOf(pt[1])
			fromVal := any("")
			if int(yIdx) >= 0 && int(yIdx) < len(yUnique) {
				fromVal = yUnique[int(yIdx)]
			}
			svc := any("")
			if int(xIdx) >= 0 && int(xIdx) < len(xUnique) {
				svc = xUnique[int(xIdx)]
			}
			newData[i] = jsonenc.NewObject().Set("value", item).Set("drilldown",
				jsonenc.NewObject().Set("from_ts", formatDrilldownTime(fromVal)).
					Set("window_s", bucketSeconds).Set("service", toStr(svc)))
		}
		fs.Set("data", newData)
	}
}

// renderNotImplemented is the sentinel errMsg meaning "this template's render path isn't ported
// yet" — the caller (render handlers) translates it into a 501 (NOT a fake 400/200).
const renderNotImplemented = "\x00render-not-implemented\x00"

// ---- phase placeholders (custom_echarts + derived_signal_overlay added in later phases) ----

func attachDerivedDrilldownFields(dd *jsonenc.Object, bindings map[string]any, idx int) {}

var drilldownTimeLayouts = []string{
	"2006-01-02T15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05-07:00", "2006-01-02 15:04:05-07:00",
	"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05", "2006-01-02 15:04:05",
	time.RFC1123, // Flask http_date form for DateTime cells: "Mon, 02 Jan 2006 15:04:05 GMT"
}

// formatDrilldownTime mirrors app.py _format_drilldown_time: parse an ISO/chdb timestamp and
// emit a canonical UTC "YYYY-MM-DDTHH:MM:SSZ"; unparseable input is returned as-is.
func formatDrilldownTime(value any) string {
	raw := strings.TrimSpace(toStr(value))
	if raw == "" {
		return ""
	}
	s := strings.Replace(raw, "Z", "+00:00", 1)
	for _, layout := range drilldownTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05Z")
		}
	}
	// Fallback: normalized chdb timestamp.
	norm := normalizeCHTimestamp(raw)
	for _, layout := range drilldownTimeLayouts {
		if t, err := time.Parse(layout, norm); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05Z")
		}
	}
	return raw
}
