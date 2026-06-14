package main

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// pyFloatRepr mirrors Python's repr/str of a float (integral floats keep ".0").
func pyFloatRepr(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eEnN") {
		s += ".0"
	}
	return s
}

// roundHalfEven mirrors Python round(x, n) (banker's rounding).
func roundHalfEven(f float64, places int) float64 {
	mult := math.Pow(10, float64(places))
	return math.RoundToEven(f*mult) / mult
}

// Anomaly rule-evaluation engine — a port of app.py's _rule_matches_series /
// _evaluate_threshold|composite|seasonal_rule / _annotate_rows_with_rules / _prepare_template_rows.
// Used by the derived_signal_overlay render path.

var anomalySeverityRank = map[string]int{"normal": 0, "warning": 1, "outlier": 2}

// fStr mirrors float(str(v)) — ok=false on failure.
func fStr(v any) (float64, bool) {
	s := pyStrAny(v)
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}

// iStr mirrors int(str(v)) — ok=false on failure (e.g. "5.0" -> int() raises).
func iStr(v any) (int, bool) {
	s := strings.TrimSpace(pyStrAny(v))
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// pyStrAny mirrors Python str(v) for the value types that flow through the engine.
func pyStrAny(v any) string { return toStr(v) }

func ruleMatchesSeries(rule map[string]any, source, signal, service, attrFp string) bool {
	if mapStr(rule, "source") != source {
		return false
	}
	if mapStr(rule, "signal") != signal {
		return false
	}
	if rs := mapStr(rule, "service"); rs != "" && rs != service {
		return false
	}
	if ra := mapStr(rule, "attr_fp"); ra != "" && ra != attrFp {
		return false
	}
	return true
}

// pyRound4 mirrors round(x, 4) (banker's rounding) then strips a trailing .0 like Python repr.
func pyRound4(f float64) string {
	r := roundHalfEven(f, 4)
	return pyFloatRepr(r)
}

// evaluateThresholdCondition mirrors _evaluate_threshold_condition.
func evaluateThresholdCondition(name, comparator string, warnTh, critTh, value, sampleCount, minSampleCount any) map[string]any {
	valueNum, ok1 := fStr(value)
	sampleNum, ok2 := iStr(sampleCount)
	if !ok1 || !ok2 {
		return nil
	}
	minSamples, _ := iStr(minSampleCount)
	if sampleNum < minSamples {
		return nil
	}
	warning, _ := fStr(warnTh)
	critical, _ := fStr(critTh)
	state := "normal"
	var triggered float64
	hasTrig := false
	if comparator == "gt" {
		if valueNum >= critical {
			state, triggered, hasTrig = "outlier", critical, true
		} else if valueNum >= warning {
			state, triggered, hasTrig = "warning", warning, true
		}
	} else if comparator == "lt" {
		if valueNum <= critical {
			state, triggered, hasTrig = "outlier", critical, true
		} else if valueNum <= warning {
			state, triggered, hasTrig = "warning", warning, true
		}
	}
	if state == "normal" || !hasTrig {
		return nil
	}
	operator := ">="
	if comparator != "gt" {
		operator = "<="
	}
	return map[string]any{
		"rule_state":  state,
		"rule_reason": name + ": value " + pyRound4(valueNum) + " " + operator + " " + formatPyNumber2(triggered),
	}
}

// formatPyNumber2 renders a threshold the way an f-string interpolates it (Python repr of the
// float, e.g. 450.0 -> "450.0").
func formatPyNumber2(f float64) string { return pyFloatRepr(f) }

func evaluateThresholdRule(rule map[string]any, value, sampleCount any) map[string]any {
	ev := evaluateThresholdCondition(mapStr(rule, "name"), orDefault(mapStr(rule, "comparator"), "gt"),
		rule["warning_threshold"], rule["critical_threshold"], value, sampleCount,
		ruleDefault(rule, "min_sample_count", 1))
	if ev == nil {
		return nil
	}
	out := map[string]any{"rule_id": mapStr(rule, "id"), "rule_name": mapStr(rule, "name")}
	for k, v := range ev {
		out[k] = v
	}
	return out
}

func ruleDefault(rule map[string]any, key string, def any) any {
	if v, ok := rule[key]; ok && v != nil {
		return v
	}
	return def
}

func combineRuleStates(states ...string) string {
	bestRank, best := -1, ""
	for _, st := range states {
		r := anomalySeverityRank[st]
		if r > bestRank || (r == bestRank && st > best) {
			bestRank, best = r, st
		}
	}
	return best
}

func buildSeriesRuleLookups(rows []map[string]any, sourceKey, signalKey, serviceKey, attrFpKey, timeKey string) (map[[4]string]map[string]any, map[[5]string]map[string]any) {
	latest := map[[4]string]map[string]any{}
	timed := map[[5]string]map[string]any{}
	for _, row := range rows {
		base := [4]string{mapStr(row, serviceKey), mapStr(row, attrFpKey), mapStr(row, sourceKey), mapStr(row, signalKey)}
		latest[base] = row
		if timeKey != "" {
			timed[[5]string{base[0], base[1], base[2], base[3], mapStr(row, timeKey)}] = row
		}
	}
	return latest, timed
}

// lookupSecondaryRuleRow mirrors _lookup_secondary_rule_row (chdb query into v_derived_signals_anomaly).
func (s *server) lookupSecondaryRuleRow(service, attrFp, secSource, secSignal, timeValue string) map[string]any {
	pull := func(extra string, params ...any) map[string]any {
		res, err := s.db.Execute("SELECT time, value, SampleCount FROM v_derived_signals_anomaly "+
			"WHERE ServiceName = ? AND SignalSource = ? AND SignalName = ? AND AttrFingerprint = ?"+extra, params...)
		if err != nil || len(res.Rows) == 0 {
			return nil
		}
		m := rowMaps(res)[0]
		return map[string]any{"time": cStr(m, "time"), "value": cStr(m, "value"), "sample_count": cInt(m, "SampleCount")}
	}
	if timeValue != "" {
		if r := pull(" AND time = ? ORDER BY time DESC LIMIT 1", service, secSource, secSignal, attrFp, timeValue); r != nil {
			return r
		}
	}
	return pull(" ORDER BY time DESC LIMIT 1", service, secSource, secSignal, attrFp)
}

func (s *server) evaluateCompositeRule(rule, row map[string]any, latest map[[4]string]map[string]any, timed map[[5]string]map[string]any,
	sourceKey, signalKey, serviceKey, attrFpKey, valueKey, sampleCountKey, timeKey string) map[string]any {
	primary := evaluateThresholdCondition(mapStr(rule, "name")+" primary", orDefault(mapStr(rule, "comparator"), "gt"),
		rule["warning_threshold"], rule["critical_threshold"], row[valueKey], row[sampleCountKey], ruleDefault(rule, "min_sample_count", 1))
	if primary == nil {
		return nil
	}
	secSource := mapStr(rule, "secondary_source")
	secSignal := mapStr(rule, "secondary_signal")
	if secSource == "" || secSignal == "" {
		return nil
	}
	service := mapStr(row, serviceKey)
	attrFp := mapStr(row, attrFpKey)
	timeValue := ""
	if timeKey != "" {
		timeValue = mapStr(row, timeKey)
	}
	var secondaryRow map[string]any
	if timeKey != "" {
		secondaryRow = timed[[5]string{service, attrFp, secSource, secSignal, timeValue}]
	}
	if secondaryRow == nil {
		secondaryRow = latest[[4]string{service, attrFp, secSource, secSignal}]
	}
	if secondaryRow == nil {
		secondaryRow = s.lookupSecondaryRuleRow(service, attrFp, secSource, secSignal, timeValue)
	}
	if secondaryRow == nil {
		return nil
	}
	secVal := secondaryRow[valueKey]
	if secVal == nil {
		secVal = secondaryRow["value"]
	}
	secSc := secondaryRow[sampleCountKey]
	if secSc == nil {
		secSc = secondaryRow["sample_count"]
	}
	secondary := evaluateThresholdCondition(mapStr(rule, "name")+" secondary", orDefault(mapStr(rule, "secondary_comparator"), "gt"),
		rule["secondary_warning_threshold"], rule["secondary_critical_threshold"], secVal, secSc, ruleDefault(rule, "min_sample_count", 1))
	if secondary == nil {
		return nil
	}
	combined := combineRuleStates(toStr(primary["rule_state"]), toStr(secondary["rule_state"]))
	return map[string]any{
		"rule_id": mapStr(rule, "id"), "rule_name": mapStr(rule, "name"), "rule_state": combined,
		"rule_reason": mapStr(rule, "name") + ": primary " + mapStr(row, signalKey) + "=" + toStr(row[valueKey]) +
			" and secondary " + secSignal + "=" + toStr(secondaryRow[valueKey]) + " triggered",
	}
}

// evaluateSeasonalRule mirrors _evaluate_seasonal_rule.
func evaluateSeasonalRule(rule map[string]any, value, sampleCount, timeValue any) map[string]any {
	bucketsJSON := strings.TrimSpace(mapStr(rule, "seasonal_buckets_json"))
	warningThreshold, _ := fStr(rule["warning_threshold"])
	criticalThreshold, _ := fStr(rule["critical_threshold"])
	isSeasonal := false
	if bucketsJSON != "" {
		if v, err := parseJSONValue([]byte(bucketsJSON)); err == nil {
			if bd, ok := v.(interface{ Get(string) (any, bool) }); ok {
				stratV, _ := bd.Get("strategy")
				strategy := orDefault(toStr(stratV), "hour_of_day")
				bucketsV, _ := bd.Get("buckets")
				if buckets, ok2 := bucketsV.(interface{ Get(string) (any, bool) }); ok2 && timeValue != nil {
					if t, ok3 := parseSeasonalTime(toStr(timeValue)); ok3 {
						var key string
						if strategy == "day_of_week" {
							key = strconv.Itoa(isoWeekday(t))
						} else {
							key = strconv.Itoa(t.Hour())
						}
						if bv, has := buckets.Get(key); has {
							if bobj, ok4 := bv.(interface{ Get(string) (any, bool) }); ok4 {
								if w, h := bobj.Get("warning"); h {
									if wf, ok := fStr(w); ok {
										warningThreshold = wf
									}
								}
								if cc, h := bobj.Get("critical"); h {
									if cf, ok := fStr(cc); ok {
										criticalThreshold = cf
									}
								}
								isSeasonal = true
							}
						}
					}
				}
			}
		}
	}
	ev := evaluateThresholdCondition(mapStr(rule, "name"), orDefault(mapStr(rule, "comparator"), "gt"),
		warningThreshold, criticalThreshold, value, sampleCount, ruleDefault(rule, "min_sample_count", 1))
	if ev == nil {
		return nil
	}
	out := map[string]any{"rule_id": mapStr(rule, "id"), "rule_name": mapStr(rule, "name"), "rule_seasonal": isSeasonal}
	for k, v := range ev {
		out[k] = v
	}
	return out
}

func parseSeasonalTime(raw string) (time.Time, bool) {
	s := strings.Replace(strings.TrimSpace(raw), " ", "T", 1)
	for _, layout := range []string{"2006-01-02T15:04:05.999999999-07:00", "2006-01-02T15:04:05-07:00",
		"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func isoWeekday(t time.Time) int {
	wd := int(t.Weekday()) // Sunday=0
	if wd == 0 {
		return 7
	}
	return wd
}

// annotateRowsWithRules mirrors _annotate_rows_with_rules (mutates rows in place).
func (s *server) annotateRowsWithRules(rows []map[string]any, rules []any,
	sourceKey, signalKey, serviceKey, attrFpKey, valueKey, sampleCountKey, timeKey string) {
	latest, timed := buildSeriesRuleLookups(rows, sourceKey, signalKey, serviceKey, attrFpKey, timeKey)
	typeRank := map[string]int{"seasonal": 3, "composite": 2, "threshold": 1}
	for _, row := range rows {
		row["rule_name"] = ""
		row["rule_state"] = "normal"
		row["rule_reason"] = ""
		row["rule_seasonal"] = false
		row["effective_state"] = orDefault(mapStr(row, "anomaly_state"), "normal")
		var bestMatch map[string]any
		bestRank := [3]any{-1, -1, ""}
		rSource, rSignal := mapStr(row, sourceKey), mapStr(row, signalKey)
		rService, rAttr := mapStr(row, serviceKey), mapStr(row, attrFpKey)
		for _, ruleAny := range rules {
			rule, ok := ruleAny.(map[string]any)
			if !ok || !ruleMatchesSeries(rule, rSource, rSignal, rService, rAttr) {
				continue
			}
			ruleType := orDefault(mapStr(rule, "rule_type"), "threshold")
			var ev map[string]any
			switch ruleType {
			case "composite":
				ev = s.evaluateCompositeRule(rule, row, latest, timed, sourceKey, signalKey, serviceKey, attrFpKey, valueKey, sampleCountKey, timeKey)
			case "seasonal":
				var tv any
				if timeKey != "" {
					tv = row[timeKey]
				}
				ev = evaluateSeasonalRule(rule, row[valueKey], row[sampleCountKey], tv)
			default:
				ev = evaluateThresholdRule(rule, row[valueKey], row[sampleCountKey])
			}
			if ev == nil {
				continue
			}
			severity := anomalySeverityRank[toStr(ev["rule_state"])]
			tr := typeRank[ruleType]
			rank := [3]any{severity, tr, toStr(ev["rule_name"])}
			if rankGreater(rank, bestRank) {
				bestMatch = ev
				bestRank = rank
			}
		}
		if bestMatch != nil {
			for k, v := range bestMatch {
				row[k] = v
			}
		}
		row["effective_state"] = combineRuleStates(orDefault(mapStr(row, "anomaly_state"), "normal"), orDefault(mapStr(row, "rule_state"), "normal"))
	}
}

// rankGreater compares (severity:int, typeRank:int, name:string) tuples.
func rankGreater(a, b [3]any) bool {
	ai, _ := a[0].(int)
	bi, _ := b[0].(int)
	if ai != bi {
		return ai > bi
	}
	aj, _ := a[1].(int)
	bj, _ := b[1].(int)
	if aj != bj {
		return aj > bj
	}
	return toStr(a[2]) > toStr(b[2])
}

var derivedRequiredColumns = []string{"time", "service", "source", "signal", "attr_fp", "value",
	"sample_count", "baseline_mean", "baseline_lower", "baseline_upper", "anomaly_state", "anomaly_score"}

// prepareTemplateRows mirrors _prepare_template_rows (derived_signal_overlay only).
func (s *server) prepareTemplateRows(columns []any, rows []map[string]any, roleIndices map[string]int) ([]any, []map[string]any, string) {
	if len(columns) < len(derivedRequiredColumns) {
		return columns, rows, ""
	}
	colForRole := func(role string, fallback int) string {
		idx := fallback
		if ri, ok := roleIndices[role]; ok {
			idx = ri
		}
		if idx >= 0 && idx < len(columns) {
			return toStr(columns[idx])
		}
		return toStr(columns[fallback])
	}
	roleCols := map[string]string{}
	for i, role := range derivedRequiredColumns {
		roleCols[role] = colForRole(role, i)
	}
	normalized := make([]map[string]any, len(rows))
	for i, raw := range rows {
		nr := map[string]any{}
		for _, role := range derivedRequiredColumns {
			nr[role] = raw[roleCols[role]]
		}
		normalized[i] = nr
	}
	s.annotateRowsWithRules(normalized, s.loadAnomalyRulesCtxAny(),
		"source", "signal", "service", "attr_fp", "value", "sample_count", "time")
	preparedCols := append(append([]any{}, strsToAnyList(derivedRequiredColumns)...),
		"rule_state", "rule_name", "rule_reason", "effective_state")
	prepared := make([]map[string]any, len(normalized))
	for i, row := range normalized {
		pr := map[string]any{}
		for _, c := range preparedCols {
			col := toStr(c)
			if v, ok := row[col]; ok {
				pr[col] = v
			} else {
				pr[col] = ""
			}
		}
		prepared[i] = pr
	}
	return preparedCols, prepared, ""
}

func strsToAnyList(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// loadAnomalyRulesCtxAny returns loadAnomalyRulesCtx as []any of map[string]any (the engine's shape).
func (s *server) loadAnomalyRulesCtxAny() []any {
	return s.loadAnomalyRulesCtx()
}
