package main

import (
	_ "embed"
	"encoding/json"
	"math"
	"regexp"
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

// renderChartFromTemplate mirrors app.py _render_chart_from_template (named_datasets=None). rows
// are dict rows (column name -> serialized value). Returns (option, errMsg).
func (s *server) renderChartFromTemplate(templateID string, columns []any, rows []map[string]any, spec *jsonenc.Object) (any, string) {
	return s.renderChartFromTemplateWithNamed(templateID, columns, rows, spec, nil)
}

// renderChartFromTemplateWithNamed mirrors app.py _render_chart_from_template WITH named_datasets.
// namedDatasets maps a named-query name -> a {columns, records, rows} object (matching Python's
// named_datasets[name] = {"columns": [...], "records": [...], "rows": [...]}). Only custom_echarts
// consumes them (resolving {{rows:name}}/{{records:name}}/{{columns:name}} bindings); other
// templates ignore them, exactly as Python does.
func (s *server) renderChartFromTemplateWithNamed(templateID string, columns []any, rows []map[string]any, spec *jsonenc.Object, namedDatasets map[string]*jsonenc.Object) (any, string) {
	tmpl, ok := echartsTemplates[templateID]
	meta, metaOK := chartTemplateMeta[templateID]
	if !ok || !metaOK {
		return nil, "Unknown template: " + templateID
	}
	if templateID == "custom_echarts" {
		return renderCustomEcharts(tmpl, columns, rows, spec, namedDatasets)
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
	if templateID == "derived_signal_overlay" {
		var perr string
		columns, rows, perr = s.prepareTemplateRows(columns, rows, roleIndices)
		if perr != "" {
			return nil, perr
		}
	}
	bindings, bErr := extractBindings(templateID, columns, rows, roleIndices)
	if bErr != nil {
		// app.py: float() inside _extract_bindings raises, the route catches it and reports
		// _public_dashboard_query_error(exc). Mirror that public-error shaping here.
		return nil, publicDashboardQueryError(bErr)
	}
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
func extractBindings(templateID string, columns []any, rows []map[string]any, roleIndices map[string]int) (map[string]any, error) {
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

	if templateID == "derived_signal_overlay" {
		if err := extractDerivedSignalBindings(templateID, bindings); err != nil {
			return bindings, err
		}
	}
	return bindings, nil
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

// numList coerces a binding list to floats (best-effort).
func numListAt(bindings map[string]any, key string) []any {
	if v, ok := bindings[key].([]any); ok {
		return v
	}
	return nil
}

// extractDerivedSignalBindings mirrors the derived_signal_overlay block of app.py _extract_bindings.
// It returns an error when a value/baseline cell is non-numeric — Python uses float() at these
// sites, which raises (propagating to the route's try/except -> 400) rather than coercing to 0.
func extractDerivedSignalBindings(templateID string, bindings map[string]any) error {
	bindings["value_axis_min"] = "dataMin"
	bindings["value_axis_max"] = "dataMax"
	bindings["zoom_start_pct"] = 0
	bindings["signal_summary"] = ""
	bindings["y_axis_name"] = "Value"

	signalName := ""
	if sb, ok := bindings["signal"].([]any); ok && len(sb) > 0 {
		signalName = strings.ToLower(toStr(sb[0]))
	}
	if strings.Contains(signalName, "ratio") {
		bindings["value_axis_min"] = 0
		bindings["value_axis_max"] = 1
	} else {
		for _, tok := range []string{"volume", "count", "latency", "duration", "p95", "p99"} {
			if strings.Contains(signalName, tok) {
				bindings["value_axis_min"] = 0
				break
			}
		}
	}

	timeValues := numListAt(bindings, "time")
	valueValues := numListAt(bindings, "value")
	baselineMean := numListAt(bindings, "baseline_mean")
	baselineLower := numListAt(bindings, "baseline_lower")
	baselineUpper := numListAt(bindings, "baseline_upper")
	effStatesAny := bindings["effective_state"]
	if effStatesAny == nil {
		effStatesAny = bindings["anomaly_state"]
	}
	effStates, _ := effStatesAny.([]any)
	if timeValues == nil || valueValues == nil || baselineMean == nil || baselineLower == nil || baselineUpper == nil {
		return nil
	}

	stateRank := map[string]int{"normal": 0, "warning": 1, "outlier": 2}
	rankSeries := make([]int, 0)
	if effStates != nil {
		for _, s := range effStates {
			rankSeries = append(rankSeries, stateRank[toStr(s)])
		}
	}
	if len(rankSeries) == 0 {
		rankSeries = make([]int, len(valueValues))
	}

	useDelta := !strings.Contains(signalName, "ratio")
	var plotValues, plotBaseline, plotLower, plotUpper []float64
	if useDelta {
		bindings["y_axis_name"] = "Delta %"
		n := min4(len(valueValues), len(baselineMean), len(baselineLower), len(baselineUpper))
		for idx := 0; idx < n; idx++ {
			// Python order: base, val, low, up (each float() can raise).
			base, err := pyFloatStrict(baselineMean[idx])
			if err != nil {
				return err
			}
			val, err := pyFloatStrict(valueValues[idx])
			if err != nil {
				return err
			}
			low, err := pyFloatStrict(baselineLower[idx])
			if err != nil {
				return err
			}
			up, err := pyFloatStrict(baselineUpper[idx])
			if err != nil {
				return err
			}
			if math.Abs(base) < 1e-9 {
				plotValues = append(plotValues, 0)
				plotBaseline = append(plotBaseline, 0)
				plotLower = append(plotLower, 0)
				plotUpper = append(plotUpper, 0)
			} else {
				denom := math.Abs(base)
				plotValues = append(plotValues, ((val-base)/denom)*100.0)
				plotBaseline = append(plotBaseline, 0)
				plotLower = append(plotLower, ((low-base)/denom)*100.0)
				plotUpper = append(plotUpper, ((up-base)/denom)*100.0)
			}
		}
		if len(plotValues) > 0 {
			minBound := minFloats(append(append([]float64{}, plotLower...), plotValues...))
			maxBound := maxFloats(append(append([]float64{}, plotUpper...), plotValues...))
			span := math.Max(5.0, (maxBound-minBound)*0.15)
			bindings["value_axis_min"] = roundHalfEven(minBound-span, 2)
			bindings["value_axis_max"] = roundHalfEven(maxBound+span, 2)
		}
	} else {
		for _, v := range valueValues {
			f, err := pyFloatStrict(v)
			if err != nil {
				return err
			}
			plotValues = append(plotValues, f)
		}
		for _, v := range baselineMean {
			f, err := pyFloatStrict(v)
			if err != nil {
				return err
			}
			plotBaseline = append(plotBaseline, f)
		}
		for _, v := range baselineLower {
			f, err := pyFloatStrict(v)
			if err != nil {
				return err
			}
			plotLower = append(plotLower, math.Max(0, f))
		}
		for _, v := range baselineUpper {
			f, err := pyFloatStrict(v)
			if err != nil {
				return err
			}
			plotUpper = append(plotUpper, f)
		}
	}

	valuePoints := []any{}
	for idx := 0; idx < min2(len(timeValues), len(plotValues)); idx++ {
		rk := 0
		if idx < len(rankSeries) {
			rk = rankSeries[idx]
		}
		valuePoints = append(valuePoints, []any{timeValues[idx], plotValues[idx], rk})
	}
	baselineMeanPoints := []any{}
	for idx := 0; idx < min2(len(timeValues), len(plotBaseline)); idx++ {
		baselineMeanPoints = append(baselineMeanPoints, []any{timeValues[idx], plotBaseline[idx]})
	}
	baselineLowerPoints := []any{}
	for idx := 0; idx < min2(len(timeValues), len(plotLower)); idx++ {
		baselineLowerPoints = append(baselineLowerPoints, []any{timeValues[idx], plotLower[idx]})
	}
	baselineUpperPoints := []any{}
	for idx := 0; idx < min3(len(timeValues), len(plotUpper), len(plotLower)); idx++ {
		baselineUpperPoints = append(baselineUpperPoints, []any{timeValues[idx], math.Max(0, plotUpper[idx]-plotLower[idx])})
	}

	markAreas := []any{}
	if effStates != nil && len(timeValues) > 0 {
		i := 0
		for i < min2(len(effStates), len(timeValues)) {
			state := toStr(effStates[i])
			if state == "normal" {
				i++
				continue
			}
			startIdx := i
			for i+1 < len(effStates) && toStr(effStates[i+1]) == state {
				i++
			}
			endIdx := i
			shade := "rgba(220, 53, 69, 0.15)"
			if state == "warning" {
				shade = "rgba(255, 193, 7, 0.15)"
			}
			markAreas = append(markAreas, []any{
				jsonenc.NewObject().Set("name", pyTitle(state)).
					Set("itemStyle", jsonenc.NewObject().Set("color", shade)).
					Set("xAxis", timeValues[startIdx]),
				jsonenc.NewObject().Set("xAxis", timeValues[endIdx]),
			})
			i++
		}
	}

	warningPoints := []any{}
	outlierPoints := []any{}
	for _, p := range valuePoints {
		pl := p.([]any)
		if len(pl) >= 3 {
			if rk, _ := pl[2].(int); rk == 1 {
				warningPoints = append(warningPoints, []any{pl[0], pl[1]})
			} else if rk == 2 {
				outlierPoints = append(outlierPoints, []any{pl[0], pl[1]})
			}
		}
	}

	latestValue := 0.0
	if len(valueValues) > 0 {
		var err error
		latestValue, err = pyFloatStrict(valueValues[len(valueValues)-1])
		if err != nil {
			return err
		}
	}
	latestBaseline := 0.0
	if len(baselineMean) > 0 {
		var err error
		latestBaseline, err = pyFloatStrict(baselineMean[len(baselineMean)-1])
		if err != nil {
			return err
		}
	}
	deltaPct := 0.0
	if math.Abs(latestBaseline) > 1e-9 {
		deltaPct = ((latestValue - latestBaseline) / math.Abs(latestBaseline)) * 100.0
	}
	bindings["signal_summary"] = "now " + sprintf1f(latestValue) + " | baseline " + sprintf1f(latestBaseline) +
		" | Δ " + sprintfPlus0f(deltaPct) + "% | warn " + itoa(len(warningPoints)) + " | outlier " + itoa(len(outlierPoints))

	bindings["value_points"] = valuePoints
	bindings["baseline_mean_points"] = baselineMeanPoints
	bindings["baseline_lower_points"] = baselineLowerPoints
	bindings["baseline_upper_points"] = baselineUpperPoints
	bindings["anomaly_mark_areas"] = markAreas
	bindings["warning_points"] = warningPoints
	bindings["outlier_points"] = outlierPoints
	return nil
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func min3(a, b, c int) int    { return min2(min2(a, b), c) }
func min4(a, b, c, d int) int { return min2(min2(a, b), min2(c, d)) }
func minFloats(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs {
		if x < m {
			m = x
		}
	}
	return m
}
func maxFloats(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}
func sprintf1f(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }
func sprintfPlus0f(f float64) string { // Python {:+.0f}
	s := strconv.FormatFloat(f, 'f', 0, 64)
	if f >= 0 && !strings.HasPrefix(s, "+") {
		return "+" + s
	}
	return s
}

// attachDerivedDrilldownFields mirrors the derived-signal extra metadata block in
// _attach_drilldown_metadata (only injected on the "Value" series).
func attachDerivedDrilldownFields(dd *jsonenc.Object, bindings map[string]any, idx int) {
	get := func(key, def string) string {
		if l, ok := bindings[key].([]any); ok && idx < len(l) {
			return toStr(l[idx])
		}
		return def
	}
	dd.Set("_rule_state", get("rule_state", "normal"))
	dd.Set("_rule_name", get("rule_name", ""))
	dd.Set("_rule_reason", get("rule_reason", ""))
	dd.Set("_effective_state", get("effective_state", "normal"))
	dd.Set("service", get("service", ""))
	dd.Set("source", get("source", ""))
	dd.Set("signal", get("signal", ""))
	dd.Set("attr_fp", get("attr_fp", ""))
}

// parseBool3 mirrors app.py _parse_bool.
func parseBool3(v any, present bool, def bool) bool {
	if !present || v == nil {
		return def
	}
	if b, ok := v.(bool); ok {
		return b
	}
	raw := strings.ToLower(strings.TrimSpace(toStr(v)))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// applyChartSpecVisualOverrides mirrors app.py _apply_chart_spec_visual_overrides.
func applyChartSpecVisualOverrides(templateID string, option any, spec *jsonenc.Object) any {
	opt, ok := option.(*jsonenc.Object)
	if !ok || templateID == "custom_echarts" {
		return option
	}
	var visual *jsonenc.Object
	if spec != nil {
		if vv, _ := spec.Get("visual"); vv != nil {
			visual, _ = vv.(*jsonenc.Object)
		}
	}
	if visual == nil {
		return option
	}
	gv := func(k string) (any, bool) { return visual.Get(k) }

	lsV, lsOK := gv("legend_show")
	legendShow := parseBool3(lsV, lsOK, true)
	if lv, _ := opt.Get("legend"); lv != nil {
		if lo, ok := lv.(*jsonenc.Object); ok {
			lo.Set("show", legendShow)
		}
	}

	ziV, ziOK := gv("zoom_inside")
	zoomInside := parseBool3(ziV, ziOK, true)
	zsV, zsOK := gv("zoom_slider")
	zoomSlider := parseBool3(zsV, zsOK, false)
	var dataZoom []any
	if dz, _ := opt.Get("dataZoom"); dz != nil {
		dataZoom, _ = dz.([]any)
	}
	zStartV, _ := gv("zoom_start_pct")
	zEndV, _ := gv("zoom_end_pct")
	zoomStart := coercePositiveInt(zStartV, zStartV != nil, 0, 0, 100)
	zoomEnd := coercePositiveInt(zEndV, zEndV != nil, 100, 0, 100)
	endVal := zoomStart
	if zoomEnd > endVal {
		endVal = zoomEnd
	}
	next := []any{}
	if zoomInside {
		next = append(next, jsonenc.NewObject().Set("type", "inside").Set("xAxisIndex", 0).
			Set("filterMode", "none").Set("start", zoomStart).Set("end", endVal))
	}
	if zoomSlider {
		next = append(next, jsonenc.NewObject().Set("type", "slider").Set("xAxisIndex", 0).
			Set("start", zoomStart).Set("end", endVal).Set("height", 16).Set("bottom", 30).
			Set("borderColor", "#495057").Set("fillerColor", "rgba(13, 110, 253, 0.20)").
			Set("handleStyle", jsonenc.NewObject().Set("color", "#0d6efd")))
	}
	if len(next) > 0 {
		opt.Set("dataZoom", next)
	} else {
		opt.Set("dataZoom", anyOrEmpty(dataZoom))
	}

	smV, smOK := gv("smooth_line")
	smoothLine := parseBool3(smV, smOK, true)
	vcV, _ := gv("value_color")
	valueColor := strings.TrimSpace(toStr(vcV))
	if sv, _ := opt.Get("series"); sv != nil {
		if series, ok := sv.([]any); ok {
			for _, se := range series {
				so, ok := se.(*jsonenc.Object)
				if !ok {
					continue
				}
				nameV, _ := so.Get("name")
				if toStr(nameV) != "Value" {
					continue
				}
				if tv, has := so.Get("type"); has && toStr(tv) == "line" {
					so.Set("smooth", smoothLine)
				}
				if valueColor != "" {
					ls := cloneOrNew(so, "lineStyle")
					is := cloneOrNew(so, "itemStyle")
					ls.Set("color", valueColor)
					is.Set("color", valueColor)
					so.Set("lineStyle", ls)
					so.Set("itemStyle", is)
				}
			}
		}
	}
	return opt
}

func anyOrEmpty(xs []any) []any {
	if xs == nil {
		return []any{}
	}
	return xs
}

func cloneOrNew(o *jsonenc.Object, key string) *jsonenc.Object {
	out := jsonenc.NewObject()
	if v, _ := o.Get(key); v != nil {
		if existing, ok := v.(*jsonenc.Object); ok {
			for _, k := range existing.Keys() {
				ev, _ := existing.Get(k)
				out.Set(k, ev)
			}
		}
	}
	return out
}

// ---- custom_echarts render (phase 2) ----

// renderCustomEcharts mirrors app.py _render_custom_echarts. namedDatasets (may be nil) exposes
// each named-query result as {{rows:name}}/{{records:name}}/{{columns:name}} bindings.
func renderCustomEcharts(tmpl echartsTemplate, columns []any, rows []map[string]any, spec *jsonenc.Object, namedDatasets map[string]*jsonenc.Object) (any, string) {
	var visual *jsonenc.Object
	if spec != nil {
		if vv, _ := spec.Get("visual"); vv != nil {
			visual, _ = vv.(*jsonenc.Object)
		}
	}
	mappingRaw, mok := parseCustomJSONConfig(visual, "custom_mapping_json")
	mapping, isObj := mappingRaw.(*jsonenc.Object)
	if !mok {
		return nil, "visual.custom_mapping_json must be valid JSON"
	}
	if !isObj {
		return nil, "visual.custom_mapping_json must be a JSON object"
	}
	var optionTemplate any
	optRaw, present := getVisual(visual, "custom_option_json")
	if !present || optRaw == nil || (isStr(optRaw) && strings.TrimSpace(toStr(optRaw)) == "") {
		optionTemplate = deepCopyJSON(tmpl.option)
	} else {
		v, ok := parseCustomJSONConfig(visual, "custom_option_json")
		if !ok {
			return nil, "visual.custom_option_json must be valid JSON"
		}
		optionTemplate = v
	}
	if _, ok := optionTemplate.(*jsonenc.Object); !ok {
		return nil, "visual.custom_option_json must be a JSON object"
	}

	records := make([]map[string]any, len(rows))
	for i, row := range rows {
		rec := map[string]any{}
		for _, c := range columns {
			col := toStr(c)
			rec[col] = row[col]
		}
		records[i] = rec
	}
	rows2d := make([]any, len(records))
	for i, rec := range records {
		r := make([]any, len(columns))
		for j, c := range columns {
			r[j] = rec[toStr(c)]
		}
		rows2d[i] = r
	}
	colsAny := append([]any(nil), columns...)
	bindings := map[string]any{"columns": colsAny, "records": recordsToAny(records), "rows": rows2d}
	for _, key := range mapping.Keys() {
		expr, _ := mapping.Get(key)
		bk := strings.TrimSpace(key)
		if bk == "" || strings.HasPrefix(bk, "_") {
			continue
		}
		val, e := resolveCustomBindingExpr(expr, colsAny, records, rows2d)
		if e != "" {
			return nil, e
		}
		bindings[bk] = val
	}

	// Expose named dataset results as {{rows:name}}, {{records:name}}, {{columns:name}} — mirrors
	// app.py _render_custom_echarts (named_datasets block). Python uses `ds.get(k) or []`, so an
	// absent/empty value yields [].
	for _, dsName := range sortedKeys(namedDatasets) {
		dsData := namedDatasets[dsName]
		if dsData == nil {
			continue
		}
		bindings["rows:"+dsName] = namedDatasetField(dsData, "rows")
		bindings["records:"+dsName] = namedDatasetField(dsData, "records")
		bindings["columns:"+dsName] = namedDatasetField(dsData, "columns")
	}

	option := deepSubstitute(optionTemplate, bindings)
	oo, ok := option.(*jsonenc.Object)
	if !ok {
		return nil, "Custom ECharts option must resolve to a JSON object"
	}
	if _, has := oo.Get("backgroundColor"); !has {
		oo.Set("backgroundColor", "transparent")
	}
	if _, has := oo.Get("textStyle"); !has {
		oo.Set("textStyle", jsonenc.NewObject().Set("color", "#adb5bd"))
	}
	normalizeCustomSeriesPointOrder(oo)
	if dd := buildCustomDrilldown(mapping, records); dd != nil {
		oo.Set("_customDrilldown", dd)
	}
	return oo, ""
}

// sortedKeys returns the map keys in sorted order (deterministic iteration; the bindings are
// keyed-substituted so order does not affect output bytes, but determinism is preferred).
func sortedKeys(m map[string]*jsonenc.Object) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// namedDatasetField mirrors Python `ds_data.get(key) or []`: returns the stored list if it is a
// non-empty []any, else an empty []any.
func namedDatasetField(ds *jsonenc.Object, key string) []any {
	v, _ := ds.Get(key)
	if list, ok := v.([]any); ok && len(list) > 0 {
		return list
	}
	return []any{}
}

func recordsToAny(records []map[string]any) []any {
	out := make([]any, len(records))
	for i, rec := range records {
		o := jsonenc.NewObject()
		// jsonenc.Object preserves insertion order; records iterate columns order is lost in a
		// map, but the records binding is only consumed via {{records:...}}/column expressions —
		// matched by key, not order. Build sorted for determinism.
		keys := make([]string, 0, len(rec))
		for k := range rec {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			o.Set(k, rec[k])
		}
		out[i] = o
	}
	return out
}

func getVisual(visual *jsonenc.Object, key string) (any, bool) {
	if visual == nil {
		return nil, false
	}
	return visual.Get(key)
}
func isStr(v any) bool { _, ok := v.(string); return ok }

// parseCustomJSONConfig mirrors _parse_custom_json_config: dict/list pass through; nil -> {};
// a string is json-parsed (ok=false on failure).
func parseCustomJSONConfig(visual *jsonenc.Object, key string) (any, bool) {
	raw, _ := getVisual(visual, key)
	switch x := raw.(type) {
	case *jsonenc.Object:
		return x, true
	case []any:
		return x, true
	case nil:
		return jsonenc.NewObject(), true
	}
	text := strings.TrimSpace(toStr(raw))
	if text == "" {
		return jsonenc.NewObject(), true
	}
	v, err := parseJSONValue([]byte(text))
	if err != nil {
		return nil, false
	}
	return v, true
}

// resolveCustomBindingExpr mirrors _resolve_custom_binding_expr.
func resolveCustomBindingExpr(expr any, columns []any, records []map[string]any, rows2d []any) (any, string) {
	if s, ok := expr.(string); ok {
		key := strings.TrimSpace(s)
		switch key {
		case "":
			return nil, ""
		case "columns":
			return columns, ""
		case "rows":
			return rows2d, ""
		case "records":
			return recordsToAny(records), ""
		}
		out := make([]any, len(records))
		for i, rec := range records {
			out[i] = rec[key]
		}
		return out, ""
	}
	eo, ok := expr.(*jsonenc.Object)
	if !ok {
		return nil, "custom_mapping_json values must be strings or objects"
	}
	modeV, _ := eo.Get("from")
	mode := strings.ToLower(strings.TrimSpace(firstNonEmpty(pyStrOrStrip(modeV, modeV != nil), "column")))
	switch mode {
	case "columns":
		return columns, ""
	case "rows":
		return rows2d, ""
	case "records":
		return recordsToAny(records), ""
	case "literal":
		v, _ := eo.Get("value")
		return v, ""
	case "column":
		nameV, _ := eo.Get("name")
		name := strings.TrimSpace(toStr(nameV))
		if name == "" {
			return nil, "custom_mapping_json column mapping requires a non-empty 'name'"
		}
		out := make([]any, len(records))
		for i, rec := range records {
			out[i] = rec[name]
		}
		return out, ""
	}
	return nil, "Unsupported custom mapping mode: " + mode
}

// normalizeCustomSeriesPointOrder mirrors _normalize_custom_series_point_order: sort tuple-like
// series points by their first element.
func normalizeCustomSeriesPointOrder(option *jsonenc.Object) {
	seriesV, _ := option.Get("series")
	series, ok := seriesV.([]any)
	if !ok {
		return
	}
	for _, e := range series {
		eo, ok := e.(*jsonenc.Object)
		if !ok {
			continue
		}
		dataV, _ := eo.Get("data")
		data, ok := dataV.([]any)
		if !ok || len(data) < 2 {
			continue
		}
		allPoints := true
		for _, p := range data {
			if pl, ok := p.([]any); !ok || len(pl) < 2 {
				allPoints = false
				break
			}
		}
		if !allPoints {
			continue
		}
		sort.SliceStable(data, func(i, j int) bool {
			return customSortKeyLess(data[i].([]any)[0], data[j].([]any)[0])
		})
	}
}

// customSortRank mirrors app.py _to_sort_key (in _normalize_custom_series_point_order):
//   - datetime  -> (0, value)             [chronological]
//   - int/float -> (1, float(value))
//   - str       -> (0, datetime) if ISO-parseable, else (2, text)   [NEVER float-parsed]
//   - other     -> (3, str(value))
//
// A chDateTime cell is a chdb Date/DateTime value (Python's chdb returns a datetime here), so it
// ranks 0 and sorts by its underlying time — NOT by its http_date string under the rank-3 default.
func customSortRank(v any) (int, float64, string) {
	if d, ok := v.(chDateTime); ok {
		for _, layout := range drilldownTimeLayouts {
			if t, err := time.Parse(layout, d.s); err == nil {
				return 0, float64(t.UTC().UnixNano()), ""
			}
		}
		// Unparseable timestamp string: fall back to lexicographic (rank 3 / str(value)).
		return 3, 0, d.s
	}
	// Python isinstance(value, (int, float)) is True for bool too. A genuine number (json.Number/
	// float64/int/bool) ranks 1; a numeric STRING is NOT float-parsed by Python and must not be
	// here either.
	switch v.(type) {
	case json.Number, float64, int, int64, bool:
		if f, ok := numOf(v); ok {
			return 1, f, ""
		}
		if b, ok := v.(bool); ok {
			if b {
				return 1, 1, ""
			}
			return 1, 0, ""
		}
	}
	if s, ok := v.(string); ok {
		txt := strings.TrimSpace(s)
		for _, layout := range drilldownTimeLayouts {
			if t, err := time.Parse(layout, strings.Replace(txt, "Z", "+00:00", 1)); err == nil {
				return 0, float64(t.UTC().UnixNano()), ""
			}
		}
		// NOTE: Python does NOT float-parse a string here — numeric strings sort
		// lexicographically as rank-2 text ("10" < "2" < "5").
		return 2, 0, txt
	}
	return 3, 0, toStr(v)
}

func customSortKeyLess(a, b any) bool {
	ra, fa, sa := customSortRank(a)
	rb, fb, sb := customSortRank(b)
	if ra != rb {
		return ra < rb
	}
	if ra == 2 || ra == 3 {
		return sa < sb
	}
	return fa < fb
}

// buildCustomDrilldown mirrors _build_custom_drilldown.
func buildCustomDrilldown(mapping *jsonenc.Object, records []map[string]any) *jsonenc.Object {
	ddV, _ := mapping.Get("_drilldown")
	dd, ok := ddV.(*jsonenc.Object)
	if !ok {
		return nil
	}
	targetV, _ := dd.Get("target")
	target := strings.TrimSpace(toStr(targetV))
	if target != "logs" && target != "metrics" && target != "traces" && target != "errors" {
		return nil
	}
	labelV, _ := dd.Get("label")
	label := strings.TrimSpace(toStr(labelV))
	if label == "" {
		label = "Open Source View"
	}
	var firstRecord map[string]any
	if len(records) > 0 {
		firstRecord = records[0]
	}
	out := jsonenc.NewObject().Set("target", target).Set("label", label)
	for _, k := range []string{"bucket_seconds", "time_axis", "service_axis"} {
		if v, ok := dd.Get(k); ok {
			out.Set(k, v)
		}
	}
	extraV, _ := dd.Get("extra")
	if extra, ok := extraV.(*jsonenc.Object); ok && extra.Len() > 0 {
		eo := jsonenc.NewObject()
		for _, k := range extra.Keys() {
			key := strings.TrimSpace(k)
			if key == "" {
				continue
			}
			v, _ := extra.Get(k)
			if s, ok := v.(string); ok {
				eo.Set(key, resolveTemplateStringCustom(s, firstRecord))
			} else {
				eo.Set(key, v)
			}
		}
		if eo.Len() > 0 {
			out.Set("extra", eo)
		}
	}
	return out
}

var customTplVarRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

// resolveTemplateStringCustom mirrors _resolve_template_string: replace {{ key }} with
// str(record[key]) ("" when None/absent).
func resolveTemplateStringCustom(value string, record map[string]any) string {
	return customTplVarRe.ReplaceAllStringFunc(value, func(m string) string {
		sub := customTplVarRe.FindStringSubmatch(m)
		if len(sub) < 2 {
			return ""
		}
		v, ok := record[strings.TrimSpace(sub[1])]
		if !ok || v == nil {
			return ""
		}
		return toStr(v)
	})
}

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
