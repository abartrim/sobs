package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// autoDashboardCreateMax mirrors app.py _AUTO_DASHBOARD_CREATE_MAX.
const autoDashboardCreateMax = 24

// anyToInt coerces a value that getCharts stored for the "position" key (an int) into an int.
func anyToInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}

// appSettingIntOrZero mirrors app.py `int(_get_app_setting(db, key) or "0")` with the
// TypeError/ValueError → 0 fallback: missing/empty/unparseable settings yield 0.
func (s *server) appSettingIntOrZero(key string) int {
	v, ok := s.appSetting(key)
	if !ok || strings.TrimSpace(v) == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return n
}

// seedDashboardIfMissing mirrors app.py _seed_dashboard_if_missing: return the Id of the
// non-deleted dashboard named dashboardName, inserting one if it does not exist.
func (s *server) seedDashboardIfMissing(dashboardName, description string) (string, error) {
	res, err := s.db.Execute(
		"SELECT Id FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Name = ? LIMIT 1",
		dashboardName)
	if err != nil {
		return "", err
	}
	if rows := rowMaps(res); len(rows) > 0 {
		return cStr(rows[0], "Id"), nil
	}
	dashboardID := newUUIDv4()
	if _, err := s.insertRowsNormalized("sobs_dashboards", []map[string]any{{
		"Id":          dashboardID,
		"Name":        dashboardName,
		"Description": description,
		"IsDeleted":   0,
		"Version":     fixedVersionMillis(),
	}}); err != nil {
		return "", err
	}
	return dashboardID, nil
}

// defaultAutoDashboardName mirrors app.py _default_auto_dashboard_name.
func defaultAutoDashboardName(serviceFilter string) string {
	if serviceFilter != "" {
		return "Auto Metric Rules - " + serviceFilter
	}
	return "Auto Metric Rules Dashboard"
}

// buildAutoDashboardChartCandidates mirrors app.py _build_auto_dashboard_chart_candidates:
// one derived_signal_overlay chart candidate per anomaly rule (skipping rules without a
// source/signal, and rules whose service doesn't match serviceFilter), with the built SQL
// query, deduped titles, sorted by (service, source, signal, title).
func buildAutoDashboardChartCandidates(rules []any, serviceFilter string, hours int) []any {
	candidates := []any{}
	titleCounts := map[string]int{}
	for _, ri := range rules {
		rule, ok := ri.(map[string]any)
		if !ok {
			continue
		}
		source := strings.TrimSpace(mapStr(rule, "source"))
		signal := strings.TrimSpace(mapStr(rule, "signal"))
		if source == "" || signal == "" {
			continue
		}
		ruleService := strings.TrimSpace(mapStr(rule, "service"))
		if serviceFilter != "" && ruleService != "" && ruleService != serviceFilter {
			continue
		}
		attrFp := strings.TrimSpace(mapStr(rule, "attr_fp"))
		whereParts := []string{
			"SignalSource = " + sqlLiteral(source),
			"SignalName = " + sqlLiteral(signal),
			"time >= now() - INTERVAL " + itoa(hours) + " HOUR",
		}
		if ruleService != "" {
			whereParts = append(whereParts, "ServiceName = "+sqlLiteral(ruleService))
		}
		if attrFp != "" {
			whereParts = append(whereParts, "AttrFingerprint = "+sqlLiteral(attrFp))
		}
		sql := "SELECT time, " +
			"ServiceName AS service, " +
			"SignalSource AS source, " +
			"SignalName AS signal, " +
			"AttrFingerprint AS attr_fp, " +
			"value, " +
			"SampleCount AS sample_count, " +
			"baseline_mean, " +
			"baseline_lower, " +
			"baseline_upper, " +
			"anomaly_state, " +
			"anomaly_score " +
			"FROM v_derived_signals_anomaly " +
			"WHERE " + strings.Join(whereParts, " AND ") + " " +
			"ORDER BY time"

		baseTitle := strings.TrimSpace(mapStr(rule, "name"))
		if baseTitle == "" {
			baseTitle = source + "/" + signal
		}
		idx := titleCounts[baseTitle]
		titleCounts[baseTitle] = idx + 1
		title := baseTitle
		if idx != 0 {
			title = baseTitle + " (" + itoa(idx+1) + ")"
		}
		candidates = append(candidates, map[string]any{
			"title":      title,
			"rule_name":  mapStr(rule, "name"),
			"rule_type":  orDefault(mapStr(rule, "rule_type"), "threshold"),
			"source":     source,
			"signal":     signal,
			"service":    ruleService,
			"attr_fp":    attrFp,
			"chart_type": "derived_signal_overlay",
			"query":      sql,
		})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].(map[string]any), candidates[j].(map[string]any)
		for _, k := range []string{"service", "source", "signal", "title"} {
			if mapStr(a, k) != mapStr(b, k) {
				return mapStr(a, k) < mapStr(b, k)
			}
		}
		return false
	})
	return candidates
}

// getSignalHealthByService mirrors app.py _get_signal_health_by_service: worst effective_state
// per service for derived signals in the last `hours` hours.
func (s *server) getSignalHealthByService(hours int) []any {
	res, err := s.db.Execute(
		"SELECT ServiceName, SignalSource, SignalName, AttrFingerprint, "+
			"argMax(value, time) AS value, argMax(SampleCount, time) AS SampleCount "+
			"FROM v_derived_signals_anomaly "+
			"WHERE time >= now() - INTERVAL ? HOUR "+
			"GROUP BY ServiceName, SignalSource, SignalName, AttrFingerprint",
		hours)
	if err != nil {
		return []any{}
	}
	dicts := rowMaps(res)
	if len(dicts) == 0 {
		return []any{}
	}
	rules := s.loadAnomalyRulesCtxAny()
	s.annotateRowsWithRules(dicts, rules,
		"SignalSource", "SignalName", "ServiceName", "AttrFingerprint",
		"value", "SampleCount", "")

	serviceWorst := map[string]int{}
	serviceCount := map[string]int{}
	seenService := map[string]bool{}
	order := []string{}
	for _, row := range dicts {
		svc := cStr(row, "ServiceName")
		rank := anomalySeverityRank[orDefault(mapStr(row, "effective_state"), "normal")]
		// Track "seen" independently of serviceWorst: when every signal for a
		// service ranks "normal" (0), `rank > serviceWorst[svc]` (0 > 0) is
		// always false, so serviceWorst[svc] never actually gets inserted —
		// which previously made the seen-check below false on every row for
		// that service and duplicated it into order once per signal.
		if !seenService[svc] {
			seenService[svc] = true
			order = append(order, svc)
		}
		if rank > serviceWorst[svc] {
			serviceWorst[svc] = rank
		}
		serviceCount[svc]++
	}
	rankToState := map[int]string{}
	for k, v := range anomalySeverityRank {
		rankToState[v] = k
	}
	out := make([]any, 0, len(order))
	for _, svc := range order {
		state := rankToState[serviceWorst[svc]]
		if state == "" {
			state = "normal"
		}
		out = append(out, map[string]any{
			"service":      svc,
			"worst_state":  state,
			"signal_count": serviceCount[svc],
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].(map[string]any), out[j].(map[string]any)
		ra := anomalySeverityRank[mapStr(a, "worst_state")]
		rb := anomalySeverityRank[mapStr(b, "worst_state")]
		if ra != rb {
			return ra > rb // -severity_rank ascending == severity_rank descending
		}
		return mapStr(a, "service") < mapStr(b, "service")
	})
	return out
}

// rowsToAny converts a []map[string]any to a []any so it can be passed as a template var
// the way Python passes a list of dicts.
func rowsToAny(rows []map[string]any) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return out
}

// chartSpecOptionsJSON builds the OptionsJson value for an auto-dashboard chart row,
// mirroring app.py json.dumps({"chart_spec": _build_raw_chart_spec(chart_type, query)},
// ensure_ascii=False). Returns a STRING (never []byte) for the JSONEachRow column.
func chartSpecOptionsJSON(chartType, query string) string {
	spec := buildRawChartSpec(chartType, query, "")
	obj := jsonenc.NewObject().Set("chart_spec", spec)
	return string(jsonenc.Encode(obj, jsonDumpsDefault))
}
