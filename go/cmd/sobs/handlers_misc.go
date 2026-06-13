package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// aiSpanCondition mirrors app.py _AI_SPAN_CONDITION (app.py:11531).
const aiSpanCondition = "(SpanAttributes['gen_ai.provider.name'] != '' " +
	"OR SpanAttributes['gen_ai.system'] != '' " +
	"OR SpanAttributes['gen_ai.operation.name'] != '')"

// GET /api/ai/span-attributes — app.py get_ai_span_attributes (app.py:19003). Requires
// ts+service query params (400 if missing); looks up one AI span; 404 if none.
func (s *server) handleApiAiSpanAttributes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ts := strings.TrimSpace(q.Get("ts"))
	service := strings.TrimSpace(q.Get("service"))
	if ts == "" || service == "" {
		writeJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", "Missing required params: ts and service"))
		return
	}
	conds := aiSpanCondition + " AND Timestamp=? AND ServiceName=?"
	params := []any{ts, service}
	if v := strings.TrimSpace(q.Get("trace_id")); v != "" {
		conds += " AND TraceId=?"
		params = append(params, v)
	}
	if v := strings.TrimSpace(q.Get("span_name")); v != "" {
		conds += " AND SpanName=?"
		params = append(params, v)
	}
	res, err := s.db.Execute(
		"SELECT SpanAttributes FROM otel_traces WHERE "+conds+" ORDER BY Timestamp DESC LIMIT 1", params...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError,
			jsonenc.NewObject().Set("ok", false).Set("error", "Failed to load span attributes"))
		return
	}
	if len(res.Rows) == 0 {
		writeJSON(w, http.StatusNotFound,
			jsonenc.NewObject().Set("ok", false).Set("error", "Span not found"))
		return
	}
	// raw_attrs = json.dumps(_map_to_dict(SpanAttributes), ensure_ascii=False, indent=2).
	// Reached only with seeded AI-trace data (the fixture otel_traces is empty).
	attrs := rowMaps(res)[0]["SpanAttributes"]
	pretty, _ := jsonDumpsIndent2(mapToDict(attrs))
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("raw_attrs", pretty))
}

// GET /api/enrichment/cve/findings — app.py api_cve_findings (app.py:18220). Feature-flag
// guard (default enabled); returns the latest findings (empty on the fixture).
func (s *server) handleApiCveFindings(w http.ResponseWriter, r *http.Request) {
	if !s.appSettingBool("enrichment.cve_enabled", true) {
		s.errorJSON(w, http.StatusForbidden, "CVE enrichment is disabled")
		return
	}
	disp := map[string][2]string{} // key -> {disposition, note}
	if res, err := s.db.Execute(
		"SELECT OsvId, Package, Ecosystem, Version, Disposition, Note FROM sobs_cve_dispositions FINAL"); err == nil {
		for _, m := range rowMaps(res) {
			k := cStr(m, "OsvId") + "::" + cStr(m, "Package") + "::" + cStr(m, "Ecosystem") + "::" + cStr(m, "Version")
			d := cStr(m, "Disposition")
			if d == "" {
				d = "open"
			}
			disp[k] = [2]string{d, cStr(m, "Note")}
		}
	}
	res, err := s.db.Execute(
		"SELECT Package, Ecosystem, Version, ServiceName, OsvId, CveIds, Summary, Severity, Published " +
			"FROM sobs_cve_findings FINAL ORDER BY Published DESC LIMIT 100")
	if err != nil {
		s.dbError(w, err)
		return
	}
	findings := []any{}
	for _, m := range rowMaps(res) {
		key := cStr(m, "OsvId") + "::" + cStr(m, "Package") + "::" + cStr(m, "Ecosystem") + "::" + cStr(m, "Version")
		raw := "open"
		note := ""
		if d, ok := disp[key]; ok {
			raw, note = d[0], d[1]
		}
		findings = append(findings, jsonenc.NewObject().
			Set("package", cStr(m, "Package")).
			Set("ecosystem", cStr(m, "Ecosystem")).
			Set("version", cStr(m, "Version")).
			Set("service", cStr(m, "ServiceName")).
			Set("osv_id", cStr(m, "OsvId")).
			Set("cve_ids", toEncodable(m["CveIds"])).
			Set("summary", cStr(m, "Summary")).
			Set("severity", cStr(m, "Severity")).
			Set("published", cStr(m, "Published")).
			Set("disposition", raw).
			Set("raw_disposition", raw).
			Set("disposition_expired", false).
			Set("disposition_note", note))
	}
	lastScan, _ := s.appSetting("enrichment.cve_last_scan")
	writeJSON(w, http.StatusOK,
		jsonenc.NewObject().Set("ok", true).Set("findings", findings).Set("last_scan", lastScan))
}

// GET /api/web-traffic/geo — app.py api_web_traffic_geo (app.py:17711). Aggregates RUM IPs
// (none carry client.ip on the fixture) into country counts via local geoip.
func (s *server) handleApiWebTrafficGeo(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute(
		"SELECT LogAttributes['client.ip'] AS ip, COUNT(*) AS cnt " +
			"FROM hyperdx_sessions GROUP BY ip HAVING ip != '' ORDER BY cnt DESC LIMIT 200")
	if err != nil {
		s.dbError(w, err)
		return
	}
	geoEnabled := s.appSettingBool("enrichment.geo_enabled", true)
	type ipc struct {
		ip  string
		cnt int
	}
	var ips []ipc
	countryTotals := map[string]int{}
	for _, m := range rowMaps(res) {
		ip := cStr(m, "ip")
		cnt := cInt(m, "cnt")
		ips = append(ips, ipc{ip, cnt})
		// geoip2fast lookup not ported (no client.ip rows on the fixture) -> "Unknown".
		countryTotals["Unknown"] += cnt
	}
	ipDetails := []any{}
	for i, x := range ips {
		if i >= 100 {
			break
		}
		ipDetails = append(ipDetails, jsonenc.NewObject().
			Set("ip", x.ip).Set("count", x.cnt).Set("country", "Unknown").Set("country_code", ""))
	}
	type cc struct {
		name string
		val  int
	}
	var counts []cc
	for k, v := range countryTotals {
		counts = append(counts, cc{k, v})
	}
	sort.SliceStable(counts, func(i, j int) bool { return counts[i].val > counts[j].val })
	countryCounts := []any{}
	for _, c := range counts {
		countryCounts = append(countryCounts, jsonenc.NewObject().Set("name", c.name).Set("value", c.val))
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).
		Set("country_counts", countryCounts).
		Set("ip_details", ipDetails).
		Set("geo_enabled", geoEnabled))
}

// GET /api/dashboards/spec/options — app.py chart_spec_options_api (app.py:21816). Distinct
// services/signals/metrics for a source view (default v_derived_signals_anomaly).
func (s *server) handleApiDashboardsSpecOptions(w http.ResponseWriter, r *http.Request) {
	sourceView := strings.TrimSpace(r.URL.Query().Get("source_view"))
	if sourceView == "" {
		sourceView = "v_derived_signals_anomaly"
	}
	signalSource := strings.TrimSpace(r.URL.Query().Get("signal_source"))
	limit := queryIntClamp(r, "limit", 100, 1, 500)
	supported := map[string]bool{
		"v_derived_signals_anomaly": true, "v_otel_metrics_anomaly": true,
		"otel_metrics_gauge": true, "otel_metrics_sum": true, "otel_metrics_histogram": true,
		"otel_logs": true, "otel_traces": true, "sobs_error_resolutions": true,
	}
	if !supported[sourceView] {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("error", "Unsupported source for options"))
		return
	}
	distinct := func(query string) []any {
		out := []any{}
		res, err := s.db.Execute(query)
		if err != nil {
			return out
		}
		for _, m := range rowMaps(res) {
			if v := strings.TrimSpace(cStr(m, "v")); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	services, signals, metrics := []any{}, []any{}, []any{}
	lim := " ORDER BY v LIMIT " + strconv.Itoa(limit)
	switch {
	case sourceView == "v_derived_signals_anomaly":
		services = distinct("SELECT DISTINCT ServiceName AS v FROM v_derived_signals_anomaly WHERE time >= now() - INTERVAL 24 HOUR" + lim)
		sig := "SELECT DISTINCT SignalName AS v FROM v_derived_signals_anomaly WHERE time >= now() - INTERVAL 24 HOUR"
		if signalSource != "" {
			sig += " AND SignalSource = " + sqlLiteral(signalSource)
		}
		signals = distinct(sig + lim)
	case sourceView == "otel_logs" || sourceView == "otel_traces":
		services = distinct("SELECT DISTINCT ServiceName AS v FROM " + sourceView + lim)
		if sourceView == "otel_logs" {
			signals = []any{"log_volume"}
		} else {
			signals = []any{"trace_volume"}
		}
	case sourceView == "sobs_error_resolutions":
		signals = []any{"resolved_error_volume"}
	default: // v_otel_metrics_anomaly, otel_metrics_{gauge,sum,histogram}
		services = distinct("SELECT DISTINCT ServiceName AS v FROM " + sourceView + lim)
		metrics = distinct("SELECT DISTINCT MetricName AS v FROM " + sourceView + lim)
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("source_view", sourceView).
		Set("services", services).
		Set("signals", signals).
		Set("metrics", metrics))
}

// --- small helpers ------------------------------------------------------------------

// sqlLiteral mirrors _sql_literal: single-quote and double interior quotes.
func sqlLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// mapToDict mirrors _map_to_dict: a chdb Map column arrives as a JSON object (map) or a
// JSON string; return a normalized map for serialization.
func mapToDict(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return x
	case string:
		var parsed any
		if json.Unmarshal([]byte(x), &parsed) == nil {
			return parsed
		}
		return map[string]any{}
	default:
		return map[string]any{}
	}
}

// jsonDumpsIndent2 mirrors json.dumps(obj, ensure_ascii=False, indent=2).
func jsonDumpsIndent2(v any) (string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
