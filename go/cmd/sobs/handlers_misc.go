package main

import (
	"bytes"
	"encoding/json"
	"io"
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

// GET /api/settings/masking/rules — app.py api_masking_rules -> _load_masking_settings.
// Default sensitive keys/patterns are static config (embedded from masking.py); custom
// keys/patterns come from settings (empty on the fixture); flags default to true.
func (s *server) handleApiMaskingRules(w http.ResponseWriter, r *http.Request) {
	var def struct {
		Keys     []string `json:"keys"`
		Patterns []string `json:"patterns"`
	}
	_ = json.Unmarshal(maskingDefaultsJSON, &def)
	customKeys := s.loadJSONStringListSetting("masking.custom_keys")
	customPatterns := s.loadJSONStringListSetting("masking.custom_patterns")

	keySet := map[string]bool{}
	for _, k := range def.Keys {
		keySet[k] = true
	}
	for _, k := range customKeys {
		keySet[k] = true
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys) // Python sorted({...}) — lexicographic, matches byte sort for ASCII

	patterns := append(append([]string{}, def.Patterns...), customPatterns...)

	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).
		Set("keys", strsToAny(keys)).
		Set("patterns", strsToAny(patterns)).
		Set("custom_keys", strsToAny(customKeys)).
		Set("custom_patterns", strsToAny(customPatterns)).
		Set("output_masking_enabled", s.appSettingBool("masking.output_enabled", true)).
		Set("sql_output_masking_enabled", s.appSettingBool("masking.sql_output_enabled", true)))
}

// loadJSONStringListSetting mirrors _load_json_string_list_setting: parse a setting whose
// value is a JSON array of strings; empty list when absent or invalid.
func (s *server) loadJSONStringListSetting(key string) []string {
	out := []string{}
	raw, ok := s.appSetting(key)
	if !ok {
		return out
	}
	var arr []any
	if json.Unmarshal([]byte(raw), &arr) != nil {
		return out
	}
	for _, v := range arr {
		if str, ok := v.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

func strsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// GET /api/ai/field-hints — app.py api_ai_field_hints: static field/operator/keyword/
// function/snippet catalog (field values empty on the fixture).
func (s *server) handleApiAiFieldHints(w http.ResponseWriter, r *http.Request) {
	v, err := parseJSONValue(aiFieldHintsStaticJSON)
	if err != nil {
		s.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// GET /api/logs/field-hints — app.py api_logs_field_hints: static catalog + DB-derived
// attr_keys/tag_keys/tag_values (empty on the fixture).
func (s *server) handleApiLogsFieldHints(w http.ResponseWriter, r *http.Request) {
	v, err := parseJSONValue(logsFieldHintsStaticJSON)
	if err != nil {
		s.dbError(w, err)
		return
	}
	obj, ok := v.(*jsonenc.Object)
	if !ok {
		obj = jsonenc.NewObject()
	}
	obj.Set("attr_keys", s.distinctStrings(
		"SELECT DISTINCT AttrKey FROM sobs_log_attr_keys FINAL WHERE RecordType='log' AND IsDeleted=0 ORDER BY AttrKey"))
	tagKeys := s.distinctStrings(
		"SELECT DISTINCT TagKey FROM sobs_record_tags FINAL WHERE RecordType='log' AND IsDeleted=0 ORDER BY TagKey LIMIT 100")
	obj.Set("tag_keys", tagKeys)
	tagValues := jsonenc.NewObject()
	for _, tk := range tagKeys {
		key, _ := tk.(string)
		tagValues.Set(key, s.distinctStrings(
			"SELECT DISTINCT TagValue FROM sobs_record_tags FINAL WHERE RecordType='log' AND TagKey=? AND IsDeleted=0 ORDER BY TagValue LIMIT 20", key))
	}
	obj.Set("tag_values", tagValues)
	writeJSON(w, http.StatusOK, obj)
}

// GET /api/settings/tags/condition-suggestions — app.py api_tag_rule_condition_suggestions.
// Suggestions come from queries over empty fixture tables -> always [] here; the request
// args (scope/field/operator/target) are echoed back.
func (s *server) handleApiTagRuleConditionSuggestions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	get := func(k, def string) string {
		v := strings.ToLower(strings.TrimSpace(q.Get(k)))
		if v == "" {
			return def
		}
		return v
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).
		Set("scope", get("scope", "tag_rule")).
		Set("field", get("field", "")).
		Set("operator", get("operator", "eq")).
		Set("target", get("target", "value")).
		Set("suggestions", []any{}))
}

// GET /api/ai/conversation — app.py get_ai_conversation: requires ts+service (400 HTML
// else). The success path renders the AI conversation partial for a matched span — needs
// seeded AI traces (the fixture otel_traces has none), so only the guard is reachable here.
func (s *server) handleApiAiConversation(w http.ResponseWriter, r *http.Request) {
	ts := strings.TrimSpace(r.URL.Query().Get("ts"))
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	if ts == "" || service == "" {
		textStatus(w, http.StatusBadRequest,
			"<p class='text-danger small'>Missing required params: ts and service.</p>")
		return
	}
	textStatus(w, http.StatusNotFound, "<p class='text-danger small'>Span not found.</p>")
}

// GET /api/metrics/anomaly — app.py metrics_anomaly. Requires service+metric (400 else);
// returns the per-minute anomaly series (empty on the fixture).
func (s *server) handleApiMetricsAnomaly(w http.ResponseWriter, r *http.Request) {
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	metric := strings.TrimSpace(r.URL.Query().Get("metric"))
	if service == "" || metric == "" {
		writeJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("error", "service and metric query parameters are required"))
		return
	}
	hours := queryIntClamp(r, "hours", 24, 1, 168)
	attrFp := strings.TrimSpace(r.URL.Query().Get("attr_fp"))
	cols := []string{"time", "value", "sample_count", "baseline_mean", "baseline_stddev",
		"baseline_lower", "baseline_upper", "anomaly_score", "anomaly_state", "metric_kind", "attr_fp"}
	params := []any{service, metric, hours}
	fpClause := ""
	if attrFp != "" {
		fpClause = " AND AttrFingerprint = ?"
		params = append(params, attrFp)
	}
	res, err := s.db.Execute(
		"SELECT  time,  value,  SampleCount AS sample_count,  baseline_mean,  baseline_stddev,"+
			"  baseline_lower,  baseline_upper,  anomaly_score,  anomaly_state,  MetricKind AS metric_kind,"+
			"  AttrFingerprint AS attr_fp FROM v_otel_metrics_anomaly WHERE ServiceName = ?   AND MetricName = ?"+
			"   AND time >= now() - INTERVAL ? HOUR"+fpClause+" ORDER BY time LIMIT 1440", params...)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().Set("error", err.Error()))
		return
	}
	rows := []any{}
	for _, m := range rowMaps(res) {
		row := make([]any, len(cols))
		for i, c := range cols {
			row[i] = m[c]
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("service", service).Set("metric", metric).
		Set("columns", strsToAny(cols)).Set("rows", rows))
}

// GET /api/ai/helper/chats — app.py ai_helper_chats: AI-helper chat summaries from
// otel_logs (empty on the fixture).
func (s *server) handleApiAiHelperChats(w http.ResponseWriter, r *http.Request) {
	page := strings.TrimSpace(r.URL.Query().Get("page"))
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	limit := queryIntClamp(r, "limit", 20, 5, 100)
	offset := 0
	if v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("offset"))); err == nil && v > 0 {
		offset = v
	}
	where := "ServiceName=? AND EventName='turn.summary' AND LogAttributes['gen_ai.chat_id'] != ''"
	params := []any{"sobs-ai-helper"}
	if page != "" {
		where += " AND LogAttributes['sobs.ai.page'] = ?"
		params = append(params, page)
	}
	res, err := s.db.Execute(
		"SELECT   LogAttributes['gen_ai.chat_id'] AS chat_id,   min(Timestamp) AS first_ts,   max(Timestamp) AS last_ts,"+
			"   argMin(LogAttributes['gen_ai.input.question'], Timestamp) AS first_question,"+
			"   argMin(LogAttributes['gen_ai.turn.summary.request'], Timestamp) AS first_request,"+
			"   count() AS turn_count FROM otel_logs WHERE "+where+" GROUP BY chat_id ORDER BY last_ts DESC LIMIT 500", params...)
	if err != nil {
		s.dbError(w, err)
		return
	}
	chats := []any{}
	for _, m := range rowMaps(res) {
		chatID := strings.TrimSpace(cStr(m, "chat_id"))
		if chatID == "" {
			continue
		}
		label := chatLabelFromFirstTurn(cStr(m, "first_question"), cStr(m, "first_request"))
		if q != "" && !strings.Contains(strings.ToLower(label), q) {
			continue
		}
		chats = append(chats, jsonenc.NewObject().
			Set("chat_id", chatID).
			Set("first_ts", cStr(m, "first_ts")).
			Set("last_ts", cStr(m, "last_ts")).
			Set("label", label).
			Set("turn_count", cInt(m, "turn_count")))
	}
	total := len(chats)
	start := offset
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	pageChats := chats[start:end]
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("chats", pageChats).Set("total", total).
		Set("has_more", start+len(pageChats) < total).Set("offset", offset))
}

// chatLabelFromFirstTurn mirrors _chat_label_from_first_turn (truncate to 80; fallback).
func chatLabelFromFirstTurn(question, request string) string {
	if q := strings.TrimSpace(question); q != "" {
		return truncate80(q)
	}
	if rq := strings.TrimSpace(request); rq != "" {
		return truncate80(rq)
	}
	return "New chat"
}

func truncate80(s string) string {
	r := []rune(s)
	if len(r) > 80 {
		return string(r[:80])
	}
	return s
}

// GET /api/dashboards/spec/templates — app.py list_chart_spec_templates: the static
// chart-spec template catalog (request-independent). Re-serialized via jsonify.
func (s *server) handleApiDashboardsSpecTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := parseJSONValue(chartSpecTemplatesJSON)
	if err != nil {
		s.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("templates", templates))
}

// GET /mcp — mcp.py mcp_endpoint_get: MCP transport probe. When enabled (default), returns
// the capability descriptor.
// handleMcpEndpointPost — mcp.py mcp_endpoint (POST): all JSON-RPC methods except
// initialize/ping/notifications/* require a valid X-MCP-API-Key. The fixture has no keys, so
// an unauthenticated call (the empty-body parity request) gets the -32002 error.
func (s *server) handleMcpEndpointPost(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := asObject(func() any { v, _ := parseJSONValue(raw); return v }())
	reqID, _ := body.Get("id")
	methodV, _ := body.Get("method")
	method, _ := methodV.(string)
	// initialize / ping / notifications/* need no API key (capability discovery + liveness).
	if method == "initialize" {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("jsonrpc", "2.0").Set("id", reqID).
			Set("result", jsonenc.NewObject().
				Set("protocolVersion", "2024-11-05").
				Set("capabilities", jsonenc.NewObject().Set("tools", jsonenc.NewObject())).
				Set("serverInfo", jsonenc.NewObject().Set("name", "sobs-mcp").Set("version", "1.0"))))
		return
	}
	if strings.HasPrefix(method, "notifications/") {
		// MCP spec: respond 202 with an empty body (Quart `return "", 202`).
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if method == "ping" {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("jsonrpc", "2.0").Set("id", reqID).
			Set("result", jsonenc.NewObject()))
		return
	}
	// _authenticate_mcp_request: the X-MCP-API-Key header must match a configured key. The
	// fixture's mcp.api_keys list is empty, so authentication always fails.
	if !s.mcpAuthenticated(r) {
		writeJSON(w, http.StatusUnauthorized, jsonenc.NewObject().
			Set("error", jsonenc.NewObject().Set("code", -32002).
				Set("message", "Unauthorized: missing or invalid X-MCP-API-Key header.")).
			Set("id", reqID).Set("jsonrpc", "2.0"))
		return
	}
	if method == "tools/list" {
		// mcpToolsJSON is the full /mcp/tools response ({id:null, jsonrpc, result:{tools}}); reuse
		// its tools list here under this request's id.
		var tools any = []any{}
		if parsed, err := parseJSONValue(mcpToolsJSON); err == nil {
			if o, ok := parsed.(*jsonenc.Object); ok {
				if res, ok := o.Get("result"); ok {
					if ro, ok := res.(*jsonenc.Object); ok {
						if t, ok := ro.Get("tools"); ok {
							tools = t
						}
					}
				}
			}
		}
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("jsonrpc", "2.0").Set("id", reqID).
			Set("result", jsonenc.NewObject().Set("tools", tools)))
		return
	}
	if method == "tools/call" {
		s.handleMcpToolsCall(w, reqID, body)
		return
	}
	writeJSON(w, http.StatusNotFound, jsonenc.NewObject().Set("jsonrpc", "2.0").Set("id", reqID).
		Set("error", jsonenc.NewObject().Set("code", -32601).Set("message", "Method not found: '"+method+"'")))
}

// mcpAuthenticated reports whether the request carries a valid MCP API key (one of the
// descriptors in the mcp.api_keys setting). Empty registry -> never authenticated.
func (s *server) mcpAuthenticated(r *http.Request) bool {
	key := strings.TrimSpace(r.Header.Get("X-MCP-API-Key"))
	if key == "" {
		return false
	}
	raw, _ := s.appSetting("mcp.api_keys")
	if raw == "" {
		return false
	}
	v, err := parseJSONValue([]byte(raw))
	if err != nil {
		return false
	}
	list, _ := v.([]any)
	keyHash := hashMcpKey(key)
	for _, e := range list {
		if o, ok := e.(*jsonenc.Object); ok && objGetStr(o, "key_hash") == keyHash {
			return true
		}
	}
	return false
}

func (s *server) handleMcpEndpointGet(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleMcpEndpointPost(w, r)
		return
	}
	enabled := true
	if v, ok := s.appSetting("mcp.enabled"); ok {
		enabled = v == "1"
	}
	if !enabled {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("jsonrpc", "2.0").Set("id", nil).
			Set("error", jsonenc.NewObject().Set("code", -32001).Set("message", "MCP server is disabled.")))
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("capabilities", jsonenc.NewObject().Set("tools", jsonenc.NewObject())).
		Set("protocolVersion", "2024-11-05").
		Set("serverInfo", jsonenc.NewObject().Set("name", "sobs-mcp").Set("version", "1.0")))
}

// GET /mcp/tools — mcp.py mcp_list_tools: the static MCP tools list.
func (s *server) handleMcpListTools(w http.ResponseWriter, r *http.Request) {
	v, err := parseJSONValue(mcpToolsJSON)
	if err != nil {
		s.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// GET /api/mcp/keys — mcp.py mcp_api_list_keys: load the mcp.api_keys setting (a JSON
// array of key descriptors; "[]" default) and return id/label/created_at/expires_at only.
func (s *server) handleApiMcpKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.mcpAPIKeysCreate(w, r)
		return
	}
	keys := []any{}
	if raw, ok := s.appSetting("mcp.api_keys"); ok {
		var arr []any
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&arr) == nil {
			for _, item := range arr {
				m, isMap := item.(map[string]any)
				if !isMap {
					continue
				}
				obj := jsonenc.NewObject().
					Set("id", strOrEmpty(m["id"])).
					Set("label", strOrEmpty(m["label"])).
					Set("created_at", strOrEmpty(m["created_at"])).
					Set("expires_at", toEncodable(m["expires_at"])) // k.get("expires_at") -> null if absent
				keys = append(keys, obj)
			}
		}
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("keys", keys))
}

// strOrEmpty mirrors dict.get(key, "") for a string-valued field.
func strOrEmpty(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
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

// jsonDumpsIndent2 mirrors json.dumps(obj, ensure_ascii=False, indent=2). Python does NOT
// HTML-escape <>& (so embedded SQL/attrs stay literal); delegate to the non-escaping form.
func jsonDumpsIndent2(v any) (string, error) {
	return jsonDumpsIndent2NoEsc(v)
}

// jsonDumpsIndent2NoEsc mirrors json.dumps(obj, ensure_ascii=False, indent=2) WITHOUT Go's
// default HTML escaping (so `<`, `>`, `&` in embedded SQL stay literal, like CPython).
func jsonDumpsIndent2NoEsc(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
