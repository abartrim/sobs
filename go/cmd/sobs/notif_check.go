package main

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Notification rule evaluation + firing — a port of app.py's _check_notification_rule /
// _evaluate_notification_condition / _build_notification_payload / _dispatch_notification_channel.
// Parity-safe: the condition queries window on ClickHouse now(), which excludes the frozen-past
// fixture data, so no rule matches on the corpus (identical to the Python oracle) — the dispatch /
// log / LastFiredAt writes only run on a real match.

// notifRule is a notification rule loaded for evaluation/firing.
type notifRule struct {
	id, name, logicOperator, severity string
	enabled                           bool
	conditions                        []any // each a *jsonenc.Object
	channelIDs                        []string
	cooldownSecond                    int
}

// notifChannel is a channel loaded for dispatch.
type notifChannel struct {
	id, name, channelType, configJSON string
	enabled                           bool
}

// normalizeNotificationConditionObj mirrors app.py _normalize_notification_condition, returning a
// *jsonenc.Object whose keys are inserted in the SAME order as the Python dict literal (so the
// downstream JSON serialization / masked-conditions payload is byte-faithful). The crux for parity:
// Python coerces threshold via float(...), so the rendered summary shows "0.0", not the raw JSON
// "0". Returns nil for a non-object entry (Python returns None -> the entry is dropped).
func normalizeNotificationConditionObj(raw any) *jsonenc.Object {
	o, ok := raw.(*jsonenc.Object)
	if !ok {
		return nil
	}
	// gs mirrors str(raw.get(k) or "").strip(): falsy -> "", else str()-coerced then stripped.
	gs := func(k string) string {
		v, has := o.Get(k)
		return strings.TrimSpace(orDefaultVal(v, has, ""))
	}
	condTypeRaw, hasType := o.Get("type")
	condType := strings.ToLower(strings.TrimSpace(orDefaultVal(condTypeRaw, hasType, "signal")))
	threshold := condFloat(o, "threshold", 0.0) // float(raw.get("threshold") or 0)
	windowMinutes := condWindowMinutes(o)
	comparator := strings.ToLower(orDefault(gs("comparator"), "gt"))
	if !notificationComparators[comparator] {
		comparator = "gt"
	}
	if condType == "tag" {
		recordType := strings.ToLower(orDefault(gs("record_type"), "all"))
		if !tagRuleRecordTypes[recordType] {
			recordType = "all"
		}
		tagOp := strings.ToLower(orDefault(gs("tag_match_operator"), "eq"))
		if !notificationTagMatchOperators[tagOp] {
			tagOp = "eq"
		}
		return jsonenc.NewObject().
			Set("type", "tag").
			Set("record_type", recordType).
			Set("tag_key", gs("tag_key")).
			Set("tag_match_operator", tagOp).
			Set("tag_value", gs("tag_value")).
			Set("comparator", comparator).
			Set("threshold", threshold).
			Set("window_minutes", windowMinutes)
	}
	return jsonenc.NewObject().
		Set("type", "signal").
		Set("source", gs("source")).
		Set("signal", gs("signal")).
		Set("service", gs("service")).
		Set("comparator", comparator).
		Set("threshold", threshold).
		Set("window_minutes", windowMinutes)
}

// loadNotificationRulesForCheck mirrors _load_notification_rules, ORDER BY Name. ConditionsJson is
// run through normalizeNotificationConditionObj (= _parse_notification_conditions_json), so each
// condition carries the normalized shape + float threshold the evaluator and summary depend on.
func (s *server) loadNotificationRulesForCheck() []notifRule {
	res, err := s.db.Execute(
		"SELECT Id, Name, Enabled, LogicOperator, ConditionsJson, ChannelIds, Severity, CooldownSeconds " +
			"FROM sobs_notification_rules FINAL WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		return nil
	}
	out := []notifRule{}
	for _, m := range rowMaps(res) {
		conds := []any{}
		if parsed, err := parseJSONValue([]byte(cStr(m, "ConditionsJson"))); err == nil {
			if list, ok := parsed.([]any); ok {
				for _, it := range list {
					if c := normalizeNotificationConditionObj(it); c != nil {
						conds = append(conds, c)
					}
				}
			}
		}
		chIDs := []string{}
		for _, c := range strings.Split(cStr(m, "ChannelIds"), ",") {
			if c = strings.TrimSpace(c); c != "" {
				chIDs = append(chIDs, c)
			}
		}
		logic := cStr(m, "LogicOperator")
		if logic == "" {
			logic = "any"
		}
		sev := cStr(m, "Severity")
		if sev == "" {
			sev = "warning"
		}
		out = append(out, notifRule{
			id: cStr(m, "Id"), name: cStr(m, "Name"), enabled: cBool(m, "Enabled"),
			logicOperator: logic, conditions: conds, channelIDs: chIDs,
			severity: sev, cooldownSecond: cInt(m, "CooldownSeconds"),
		})
	}
	return out
}

// loadNotificationChannelsByID mirrors _load_notification_channels keyed by id.
func (s *server) loadNotificationChannelsByID() map[string]notifChannel {
	out := map[string]notifChannel{}
	res, err := s.db.Execute(
		"SELECT Id, Name, ChannelType, ConfigJson, Enabled FROM sobs_notification_channels FINAL " +
			"WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		id := cStr(m, "Id")
		out[id] = notifChannel{
			id: id, name: cStr(m, "Name"), channelType: cStr(m, "ChannelType"),
			configJSON: cStr(m, "ConfigJson"), enabled: cBool(m, "Enabled"),
		}
	}
	return out
}

// condFloat / condInt mirror Python float(cond.get(k, def)) / int(cond.get(k, def)).
func condFloat(o *jsonenc.Object, key string, def float64) float64 {
	if v, ok := o.Get(key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(pyStrAny(v)), 64); err == nil {
			return f
		}
	}
	return def
}

func condInt(o *jsonenc.Object, key string, def int) int {
	if v, ok := o.Get(key); ok {
		s := strings.TrimSpace(pyStrAny(v))
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int(f)
		}
	}
	return def
}

// compareThreshold mirrors the comp_map in _evaluate_*_condition.
func compareThreshold(comparator string, value, threshold float64) bool {
	switch comparator {
	case "gt":
		return value > threshold
	case "lt":
		return value < threshold
	case "gte":
		return value >= threshold
	case "lte":
		return value <= threshold
	case "eq":
		return math.Abs(value-threshold) < 1e-9
	}
	return false
}

// evaluateNotificationCondition mirrors _evaluate_notification_condition (signal vs tag).
func (s *server) evaluateNotificationCondition(cond *jsonenc.Object) (bool, float64) {
	condType := strings.ToLower(strings.TrimSpace(objStrOr(cond, "type")))
	if condType == "tag" {
		return s.evaluateTagCondition(cond)
	}
	return s.evaluateSignalCondition(cond)
}

// evaluateSignalCondition mirrors _evaluate_signal_condition (avg over v_derived_signals_1m).
func (s *server) evaluateSignalCondition(cond *jsonenc.Object) (bool, float64) {
	source := strings.TrimSpace(objStrOr(cond, "source"))
	signal := strings.TrimSpace(objStrOr(cond, "signal"))
	service := strings.TrimSpace(objStrOr(cond, "service"))
	comparator := strings.TrimSpace(objStrDef(cond, "comparator", "gt"))
	threshold := condFloat(cond, "threshold", 0)
	window := clampInt(condInt(cond, "window_minutes", 5), 1, 60)
	if source == "" || signal == "" {
		return false, 0
	}
	query := "SELECT avg(Value) AS v FROM v_derived_signals_1m " +
		"WHERE MinuteBucket >= now() - INTERVAL ? MINUTE AND SignalSource = ? AND SignalName = ?"
	params := []any{window, source, signal}
	if service != "" {
		query += " AND ServiceName = ?"
		params = append(params, service)
	}
	query += " HAVING count() >= ?"
	params = append(params, 1)
	res, err := s.db.Execute(query, params...)
	if err != nil || len(res.Rows) == 0 {
		return false, 0
	}
	value := cFloat(rowMaps(res)[0], "v")
	return compareThreshold(comparator, value, threshold), value
}

// evaluateTagCondition mirrors _evaluate_tag_condition (count over sobs_record_tags).
func (s *server) evaluateTagCondition(cond *jsonenc.Object) (bool, float64) {
	recordType := strings.ToLower(strings.TrimSpace(objStrOr(cond, "record_type")))
	tagKey := strings.TrimSpace(objStrOr(cond, "tag_key"))
	tagOp := strings.ToLower(strings.TrimSpace(objStrDef(cond, "tag_match_operator", "eq")))
	tagValue := strings.TrimSpace(objStrOr(cond, "tag_value"))
	comparator := strings.TrimSpace(objStrDef(cond, "comparator", "gt"))
	threshold := condFloat(cond, "threshold", 0)
	window := clampInt(condInt(cond, "window_minutes", 5), 1, 60)
	if tagKey == "" {
		return false, 0
	}
	minVersion := nowUTC().Add(-time.Duration(window) * time.Minute).UnixMilli()
	where := []string{"IsDeleted = 0", "Version >= ?", "TagKey = ?"}
	params := []any{minVersion, tagKey}
	if recordType != "" && recordType != "all" {
		where = append(where, "RecordType = ?")
		params = append(params, recordType)
	}
	if tagValue != "" {
		switch tagOp {
		case "eq":
			where = append(where, "TagValue = ?")
			params = append(params, tagValue)
		case "contains":
			where = append(where, "positionCaseInsensitive(TagValue, ?) > 0")
			params = append(params, tagValue)
		case "regex":
			where = append(where, "match(TagValue, ?)")
			params = append(params, tagValue)
		}
	}
	res, err := s.db.Execute("SELECT count() AS c FROM sobs_record_tags FINAL WHERE "+strings.Join(where, " AND "), params...)
	if err != nil || len(res.Rows) == 0 {
		return false, 0
	}
	value := cFloat(rowMaps(res)[0], "c")
	return compareThreshold(comparator, value, threshold), value
}

// notificationChannelMaskOutputEnabled mirrors _notification_channel_mask_output_enabled.
func notificationChannelMaskOutputEnabled(configJSON string) bool {
	parsed, err := parseJSONValue([]byte(strOrBrace(configJSON)))
	if err != nil {
		return true
	}
	config, ok := parsed.(*jsonenc.Object)
	if !ok {
		return true
	}
	v, ok := config.Get("mask_output_enabled")
	if !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(pyStrAny(v))) {
	case "":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// condThreshold / condValue render the threshold / matched value into a summary string, mirroring
// the f-strings in _build_notification_payload.
func condThreshold(cond *jsonenc.Object) string {
	if v, ok := cond.Get("threshold"); ok {
		return pyStrAny(v)
	}
	return "0"
}

func condValue(cond *jsonenc.Object) string {
	if v, ok := cond.Get("_value"); ok {
		return pyStrAny(v)
	}
	return "n/a"
}

// buildNotificationPayload mirrors _build_notification_payload.
func (s *server) buildNotificationPayload(rule notifRule, firedConditions []any, maskEnabled bool) *jsonenc.Object {
	var conditionsPayload any = firedConditions
	if maskEnabled {
		conditionsPayload = s.maskValueForOutput(firedConditions)
	}
	comparatorLabels := map[string]string{"gt": ">", "lt": "<", "gte": "≥", "lte": "≤", "eq": "="}
	summaries := []string{}
	for _, c := range firedConditions {
		cond, _ := c.(*jsonenc.Object)
		if cond == nil {
			continue
		}
		comp := comparatorLabels[strings.TrimSpace(objStrDef(cond, "comparator", "gt"))]
		if comp == "" {
			comp = ">"
		}
		condType := objStrOr(cond, "type")
		if condType == "tag" {
			recordType := objStrOr(cond, "record_type")
			if recordType == "" {
				recordType = "all"
			}
			recordTypeStr := ""
			if recordType != "all" {
				recordTypeStr = "[" + recordType + "] "
			}
			tagKey := objStrOr(cond, "tag_key")
			tagValue := objStrOr(cond, "tag_value")
			tagExpr := tagKey
			if tagValue != "" {
				tagExpr = tagKey + " " + objStrDef(cond, "tag_match_operator", "eq") + " " + tagValue
			}
			summaries = append(summaries, "tag "+recordTypeStr+tagExpr+" "+comp+" "+condThreshold(cond)+" (value="+condValue(cond)+")")
		} else {
			serviceStr := ""
			if svc := objStrOr(cond, "service"); svc != "" {
				serviceStr = " [" + svc + "]"
			}
			summaries = append(summaries, objStrOr(cond, "source")+"/"+objStrOr(cond, "signal")+serviceStr+" "+comp+" "+condThreshold(cond)+" (value="+condValue(cond)+")")
		}
	}
	summary := "[SOBS] Rule '" + rule.name + "' triggered (" + strings.ToUpper(rule.severity) + "): " + strings.Join(summaries, "; ")
	if maskEnabled {
		summary = s.maskStringForOutput(summary)
	}
	return jsonenc.NewObject().
		Set("rule_name", rule.name).
		Set("severity", rule.severity).
		Set("conditions", conditionsPayload).
		Set("summary", summary).
		Set("fired_at", nowUTC().Format("2006-01-02T15:04:05.000000-07:00"))
}

// checkNotificationRule mirrors _check_notification_rule: enabled + cooldown + condition logic,
// then (on fire) dispatch to each channel, write the log, and bump LastFiredAt.
func (s *server) checkNotificationRule(rule notifRule, channelsByID map[string]notifChannel) *jsonenc.Object {
	if !rule.enabled {
		return jsonenc.NewObject().Set("rule_id", rule.id).Set("fired", false).Set("reason", "disabled")
	}

	lastFiredTs := 0.0
	if res, err := s.db.Execute(
		"SELECT toUnixTimestamp64Milli(LastFiredAt) AS ts FROM sobs_notification_rules FINAL WHERE Id = ? LIMIT 1",
		rule.id); err == nil && len(res.Rows) > 0 {
		lastFiredTs = cFloat(rowMaps(res)[0], "ts") / 1000.0
	}
	if float64(nowUTC().Unix())-lastFiredTs < float64(rule.cooldownSecond) {
		return jsonenc.NewObject().Set("rule_id", rule.id).Set("fired", false).Set("reason", "cooldown")
	}

	firedConditions := []any{}
	notFired := 0
	for _, c := range rule.conditions {
		cond, _ := c.(*jsonenc.Object)
		if cond == nil {
			notFired++
			continue
		}
		matched, value := s.evaluateNotificationCondition(cond)
		annotated := cloneObject(cond).Set("_value", roundHalfEven(value, 4))
		if matched {
			firedConditions = append(firedConditions, annotated)
		} else {
			notFired++
		}
	}
	shouldFire := len(firedConditions) > 0
	if rule.logicOperator == "all" {
		shouldFire = len(rule.conditions) > 0 && notFired == 0
	}
	if !shouldFire {
		return jsonenc.NewObject().Set("rule_id", rule.id).Set("fired", false).Set("reason", "conditions not met")
	}

	defaultPayload := s.buildNotificationPayload(rule, firedConditions, true)

	type dispatchResult struct{ channelID, channelName, status, errMsg, summary string }
	results := []dispatchResult{}
	for _, chID := range rule.channelIDs {
		ch, ok := channelsByID[chID]
		if !ok {
			results = append(results, dispatchResult{channelID: chID, status: "error", errMsg: "channel not found", summary: objStrOr(defaultPayload, "summary")})
			continue
		}
		if !ch.enabled {
			results = append(results, dispatchResult{channelID: chID, status: "skipped", errMsg: "channel disabled", summary: objStrOr(defaultPayload, "summary")})
			continue
		}
		maskEnabled := notificationChannelMaskOutputEnabled(ch.configJSON)
		payload := s.buildNotificationPayload(rule, firedConditions, maskEnabled)
		status := s.dispatchNotificationChannel(ch.channelType, ch.configJSON, payload)
		dr := dispatchResult{channelID: chID, channelName: ch.name, status: "ok", summary: objStrOr(payload, "summary")}
		if status != "ok" {
			dr.status, dr.errMsg = "error", status
		}
		results = append(results, dr)
	}

	// Notification log entries.
	for _, dr := range results {
		_, _ = s.insertRowsNormalized("sobs_notification_log", []map[string]any{{
			"Id": newUUIDv4(), "RuleId": rule.id, "RuleName": rule.name,
			"ChannelId": dr.channelID, "ChannelName": dr.channelName,
			"FiredAt": nowUTC().Format("2006-01-02 15:04:05.000"),
			"Status":  dr.status, "ErrorMessage": dr.errMsg, "Summary": dr.summary,
		}})
	}

	// Bump LastFiredAt.
	_, _ = s.insertRowsNormalized("sobs_notification_rules", []map[string]any{{
		"Id": rule.id, "Name": rule.name, "Enabled": boolToInt(rule.enabled),
		"LogicOperator":  rule.logicOperator,
		"ConditionsJson": string(jsonenc.Encode(rule.conditions, webhookDumpsOpts)),
		"ChannelIds":     strings.Join(rule.channelIDs, ","),
		"Severity":       rule.severity, "CooldownSeconds": rule.cooldownSecond,
		"LastFiredAt": nowUTC().Format("2006-01-02 15:04:05.000"),
		"IsDeleted":   0, "Version": fixedVersionMillis(),
	}})

	// Register a raw preservation window around this signal (app.py 25315-25324). Best-effort, like
	// the Python try/except — a failure must not abort the fired result.
	func() {
		defer func() { _ = recover() }()
		s.registerRawWindow(nowUTC(), "notification", rule.id, "", "", "")
	}()

	dispatchResults := make([]any, 0, len(results))
	for _, dr := range results {
		o := jsonenc.NewObject().Set("channel_id", dr.channelID)
		if dr.channelName != "" {
			o.Set("channel_name", dr.channelName)
		}
		o.Set("status", dr.status).Set("error", dr.errMsg).Set("summary", dr.summary)
		dispatchResults = append(dispatchResults, o)
	}
	return jsonenc.NewObject().
		Set("rule_id", rule.id).Set("rule_name", rule.name).Set("fired", true).
		Set("summary", objStrOr(defaultPayload, "summary")).
		Set("dispatch_results", dispatchResults)
}

// POST /api/notifications/check — app.py check_notifications: evaluate every notification rule and
// fire any that match, then evaluate automatic agent-rule triggers from anomaly/tag events. The
// agent branch (evaluateAgentRuleTriggers) only does work when AI is configured — off on the fixture
// — so agent_runs is empty there, identical to the oracle.
func (s *server) handleApiNotificationsCheck(w http.ResponseWriter, r *http.Request) {
	channelsByID := s.loadNotificationChannelsByID()
	results := []any{}
	fired := 0
	for _, rule := range s.loadNotificationRulesForCheck() {
		// Per-rule isolation (app.py 26352-26359): a failing rule yields a {fired:false,
		// error:"rule evaluation failed"} result and the loop continues with the next rule.
		res := func(rule notifRule) (out *jsonenc.Object) {
			defer func() {
				if r := recover(); r != nil {
					out = jsonenc.NewObject().Set("rule_id", rule.id).Set("fired", false).
						Set("error", "rule evaluation failed")
				}
			}()
			return s.checkNotificationRule(rule, channelsByID)
		}(rule)
		results = append(results, res)
		if v, _ := res.Get("fired"); v == true {
			fired++
		}
	}
	agentRuns := s.evaluateAgentRuleTriggers()
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("evaluated", len(results)).Set("fired", fired).
		Set("results", results).Set("agent_runs", agentRuns))
}
