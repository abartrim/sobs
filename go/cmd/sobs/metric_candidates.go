package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// _AUTO_RULE_GT_HINTS / _AUTO_RULE_LT_HINTS from app.py.
var (
	autoRuleGTHints = []string{"error", "latency", "duration", "timeout", "p95", "p99", "failure", "fail", "retry"}
	autoRuleLTHints = []string{"availability", "success", "throughput", "rps", "qps"}
)

const seasonalMinBucketPoints = 3

// inferAutoRuleComparator mirrors app.py _infer_auto_rule_comparator.
func inferAutoRuleComparator(signalName string) string {
	name := strings.ToLower(signalName)
	for _, t := range autoRuleLTHints {
		if strings.Contains(name, t) {
			return "lt"
		}
	}
	for _, t := range autoRuleGTHints {
		if strings.Contains(name, t) {
			return "gt"
		}
	}
	return "gt"
}

// autoRuleThresholds mirrors app.py _auto_rule_thresholds.
func autoRuleThresholds(comparator string, q05, q20, q50, q80, q95 float64) (float64, float64) {
	if comparator == "lt" {
		warning := q20
		critical := q05
		if critical > warning {
			critical = math.Min(warning, q50)
		}
		if critical == warning {
			if warning != 0 {
				critical = warning * 0.9
			} else {
				critical = -0.1
			}
		}
		return warning, critical
	}
	warning := q80
	critical := q95
	if critical < warning {
		critical = math.Max(warning, q50)
	}
	if critical == warning {
		if warning != 0 {
			critical = warning * 1.1
		} else {
			critical = 0.1
		}
	}
	return warning, critical
}

// formatAutoRuleName mirrors app.py _format_auto_rule_name.
func formatAutoRuleName(source, signal, service, attrFp string) string {
	suffix := service
	if suffix == "" {
		suffix = "any"
	}
	if attrFp != "" {
		suffix = suffix + " / " + attrFp
	}
	return fmt.Sprintf("Auto %s/%s [%s]", source, signal, suffix)
}

// metricStatsSelect builds the per-series stats SELECT shared by both builders.
func metricStatsSelect(attrSelect string) string {
	return "SELECT ServiceName, SignalSource, SignalName, " +
		attrSelect + " AS AttrFingerprint, " +
		"count() AS point_count, " +
		"quantile(0.05)(toFloat64(value)) AS q05, " +
		"quantile(0.20)(toFloat64(value)) AS q20, " +
		"quantile(0.50)(toFloat64(value)) AS q50, " +
		"quantile(0.80)(toFloat64(value)) AS q80, " +
		"quantile(0.95)(toFloat64(value)) AS q95 "
}

// loadAnomalyExistingSeries builds the existing-rule scope set keyed on
// (source, signal, service, attr_fp, rule.rule_type) — mirroring app.py's existing_series set.
// A candidate is blocked only by a rule whose own rule_type matches the candidate's type.
func (s *server) loadAnomalyExistingSeries() map[string]bool {
	existing := map[string]bool{}
	for _, rv := range s.loadAnomalyRulesCtx() {
		rule, _ := rv.(map[string]any)
		if rule == nil {
			continue
		}
		rt := cStr(rule, "rule_type")
		if rt == "" {
			rt = "threshold"
		}
		existing[ruleKeyJoin(cStr(rule, "source"), cStr(rule, "signal"), cStr(rule, "service"), cStr(rule, "attr_fp"), rt)] = true
	}
	return existing
}

// buildAutoMetricRuleCandidates mirrors app.py _build_auto_metric_rule_candidates (threshold mode):
// per-series quantiles over the lookback window -> warning/critical thresholds.
func (s *server) buildAutoMetricRuleCandidates(hours, minPoints int, serviceFilter string, includeAttrFp bool) ([]any, map[string]int) {
	whereSQL, attrSelect, attrGroup := metricCandidateScope(hours, serviceFilter, includeAttrFp)
	sql := metricStatsSelect(attrSelect) +
		"FROM v_derived_signals_anomaly" + whereSQL +
		" GROUP BY ServiceName, SignalSource, SignalName" + attrGroup +
		fmt.Sprintf(" HAVING point_count >= %d", minPoints) +
		" ORDER BY point_count DESC"

	statsRows := s.queryRows(sql)
	existing := s.loadAnomalyExistingSeries()

	candidates := []any{}
	skippedExisting := 0
	skippedInvalid := 0
	for _, row := range statsRows {
		service := cStr(row, "ServiceName")
		source := cStr(row, "SignalSource")
		signal := cStr(row, "SignalName")
		attrFp := cStr(row, "AttrFingerprint")
		if existing[ruleKeyJoin(source, signal, service, attrFp, "threshold")] {
			skippedExisting++
			continue
		}
		comparator := inferAutoRuleComparator(signal)
		warning, critical := autoRuleThresholds(comparator,
			cFloat(row, "q05"), cFloat(row, "q20"), cFloat(row, "q50"), cFloat(row, "q80"), cFloat(row, "q95"))
		if comparator == "gt" && critical < warning {
			skippedInvalid++
			continue
		}
		if comparator == "lt" && critical > warning {
			skippedInvalid++
			continue
		}
		candidates = append(candidates, map[string]any{
			"name": formatAutoRuleName(source, signal, service, attrFp),
			"rule_type": "threshold", "source": source, "signal": signal,
			"service": service, "attr_fp": attrFp, "comparator": comparator,
			"warning_threshold": warning, "critical_threshold": critical,
			"min_sample_count": 3, "point_count": cInt(row, "point_count"),
		})
	}
	return candidates, map[string]int{"examined": len(statsRows), "existing": skippedExisting, "invalid": skippedInvalid}
}

// buildSeasonalMetricRuleCandidates mirrors app.py _build_seasonal_metric_rule_candidates:
// per-series thresholds plus per-(hour-of-day|day-of-week)-bucket thresholds carried in
// seasonal_buckets_json. NOTE: bucket keys derive from wall-clock time, so this path is not
// byte-reproducible across capture/replay seeded at different real times — it is exercised by
// real traffic, never by the parity oracle.
func (s *server) buildSeasonalMetricRuleCandidates(hours, minPoints int, serviceFilter string, includeAttrFp bool, strategy string) ([]any, map[string]int) {
	if strategy != "hour_of_day" && strategy != "day_of_week" {
		strategy = "hour_of_day"
	}
	bucketExpr := "toHour(time)"
	if strategy == "day_of_week" {
		bucketExpr = "toDayOfWeek(time)"
	}
	whereSQL, attrSelect, attrGroup := metricCandidateScope(hours, serviceFilter, includeAttrFp)

	seriesRows := s.queryRows(metricStatsSelect(attrSelect) +
		"FROM v_derived_signals_anomaly" + whereSQL +
		" GROUP BY ServiceName, SignalSource, SignalName" + attrGroup +
		fmt.Sprintf(" HAVING point_count >= %d", minPoints) +
		" ORDER BY point_count DESC")

	eligibleSub := "SELECT ServiceName, SignalSource, SignalName, " + attrSelect + " AS AttrFingerprint " +
		"FROM v_derived_signals_anomaly" + whereSQL +
		" GROUP BY ServiceName, SignalSource, SignalName" + attrGroup +
		fmt.Sprintf(" HAVING count() >= %d", minPoints)

	bucketRows := s.queryRows("SELECT ServiceName, SignalSource, SignalName, " + attrSelect + " AS AttrFingerprint, " +
		bucketExpr + " AS bucket_key, " +
		"count() AS point_count, " +
		"quantile(0.05)(toFloat64(value)) AS q05, " +
		"quantile(0.20)(toFloat64(value)) AS q20, " +
		"quantile(0.50)(toFloat64(value)) AS q50, " +
		"quantile(0.80)(toFloat64(value)) AS q80, " +
		"quantile(0.95)(toFloat64(value)) AS q95 " +
		"FROM v_derived_signals_anomaly" + whereSQL +
		" AND (ServiceName, SignalSource, SignalName, " + attrSelect + ") IN (" + eligibleSub + ")" +
		" GROUP BY ServiceName, SignalSource, SignalName" + attrGroup + ", bucket_key" +
		fmt.Sprintf(" HAVING point_count >= %d", seasonalMinBucketPoints) +
		" ORDER BY ServiceName, SignalSource, SignalName" + attrGroup + ", bucket_key")

	// bucket_index: series key -> ordered {bucket_key -> {warning, critical}}.
	bucketIndex := map[string]*jsonenc.Object{}
	bucketCounts := map[string]int{}
	for _, br := range bucketRows {
		key := ruleKeyJoin(cStr(br, "SignalSource"), cStr(br, "SignalName"), cStr(br, "ServiceName"), cStr(br, "AttrFingerprint"))
		bk := strconv.Itoa(cInt(br, "bucket_key"))
		comparator := inferAutoRuleComparator(cStr(br, "SignalName"))
		w, c := autoRuleThresholds(comparator,
			cFloat(br, "q05"), cFloat(br, "q20"), cFloat(br, "q50"), cFloat(br, "q80"), cFloat(br, "q95"))
		obj := bucketIndex[key]
		if obj == nil {
			obj = jsonenc.NewObject()
			bucketIndex[key] = obj
		}
		obj.Set(bk, jsonenc.NewObject().Set("warning", w).Set("critical", c))
		bucketCounts[key]++
	}

	existing := s.loadAnomalyExistingSeries()
	candidates := []any{}
	skippedExisting := 0
	skippedInvalid := 0
	for _, row := range seriesRows {
		service := cStr(row, "ServiceName")
		source := cStr(row, "SignalSource")
		signal := cStr(row, "SignalName")
		attrFp := cStr(row, "AttrFingerprint")
		if existing[ruleKeyJoin(source, signal, service, attrFp, "seasonal")] {
			skippedExisting++
			continue
		}
		comparator := inferAutoRuleComparator(signal)
		warning, critical := autoRuleThresholds(comparator,
			cFloat(row, "q05"), cFloat(row, "q20"), cFloat(row, "q50"), cFloat(row, "q80"), cFloat(row, "q95"))
		if comparator == "gt" && critical < warning {
			skippedInvalid++
			continue
		}
		if comparator == "lt" && critical > warning {
			skippedInvalid++
			continue
		}
		key := ruleKeyJoin(source, signal, service, attrFp)
		buckets := bucketIndex[key]
		if buckets == nil {
			buckets = jsonenc.NewObject()
		}
		seasonalJSON := string(jsonenc.Encode(
			jsonenc.NewObject().Set("strategy", strategy).Set("buckets", buckets), jsonDumpsDefault))
		candidates = append(candidates, map[string]any{
			"name": formatAutoRuleName(source, signal, service, attrFp),
			"rule_type": "seasonal", "source": source, "signal": signal,
			"service": service, "attr_fp": attrFp, "comparator": comparator,
			"warning_threshold": warning, "critical_threshold": critical,
			"min_sample_count": 3, "point_count": cInt(row, "point_count"),
			"seasonal_buckets_json": seasonalJSON,
			"seasonal_bucket_count": bucketCounts[key],
			"seasonal_strategy":     strategy,
		})
	}
	return candidates, map[string]int{"examined": len(seriesRows), "existing": skippedExisting, "invalid": skippedInvalid}
}

// metricCandidateScope returns the shared WHERE clause + attr select/group fragments.
func metricCandidateScope(hours int, serviceFilter string, includeAttrFp bool) (whereSQL, attrSelect, attrGroup string) {
	whereParts := []string{fmt.Sprintf("time >= now() - INTERVAL %d HOUR", hours)}
	if serviceFilter != "" {
		whereParts = append(whereParts, "ServiceName = "+sqlLiteral(serviceFilter))
	}
	whereSQL = " WHERE " + strings.Join(whereParts, " AND ")
	attrSelect = "''"
	if includeAttrFp {
		attrSelect = "AttrFingerprint"
		attrGroup = ", AttrFingerprint"
	}
	return
}

func (s *server) queryRows(sql string) []map[string]any {
	res, err := s.db.Execute(sql)
	if err != nil {
		return nil
	}
	return rowMaps(res)
}
