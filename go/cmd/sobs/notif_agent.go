package main

import (
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Automatic agent-rule triggering from anomaly / tag events — a port of the agent branch of app.py
// check_notifications plus its helpers (_collect_anomaly_agent_events, _collect_tag_rule_agent_events,
// _normalize_agent_trigger_state, _agent_rule_trigger_state_matches). Reachable only when AI is
// configured; on the parity fixture (AI off) evaluateAgentRuleTriggers returns an empty list,
// identical to the oracle.

// normalizeAgentTriggerState mirrors _normalize_agent_trigger_state.
func normalizeAgentTriggerState(rawState string) string {
	state := strings.ToLower(strings.TrimSpace(rawState))
	if state == "outlier" {
		return "critical"
	}
	if state == "warning" || state == "critical" {
		return state
	}
	return "normal"
}

// agentRuleTriggerStateMatches mirrors _agent_rule_trigger_state_matches.
func agentRuleTriggerStateMatches(triggerState, eventState string) bool {
	requested := strings.ToLower(strings.TrimSpace(triggerState))
	if requested == "" {
		requested = "any"
	}
	if requested == "any" {
		return eventState == "warning" || eventState == "critical"
	}
	return requested == eventState
}

// orderedAgentEvents keeps events keyed by rule id while preserving first-insertion order, so that
// list(events.values()) (and the "first"/"max" selections that consume it) match the Python dict
// even though Go maps iterate in random order.
type orderedAgentEvents struct {
	order []string
	byID  map[string]*jsonenc.Object
}

func newOrderedAgentEvents() *orderedAgentEvents {
	return &orderedAgentEvents{byID: map[string]*jsonenc.Object{}}
}

// set inserts/updates an event. A new key is appended to the order; an existing key keeps its
// original position (Python dict-update semantics).
func (o *orderedAgentEvents) set(id string, ev *jsonenc.Object) {
	if _, ok := o.byID[id]; !ok {
		o.order = append(o.order, id)
	}
	o.byID[id] = ev
}

func (o *orderedAgentEvents) get(id string) *jsonenc.Object { return o.byID[id] }

func (o *orderedAgentEvents) values() []*jsonenc.Object {
	out := make([]*jsonenc.Object, 0, len(o.order))
	for _, k := range o.order {
		out = append(out, o.byID[k])
	}
	return out
}

// pickHighestSeverityEvent mirrors max(events, key=lambda e: 2 if state == "critical" else 1).
// Python's max returns the FIRST element achieving the maximum, so ties resolve to insertion order;
// the strict `>` comparison below preserves that.
func pickHighestSeverityEvent(events []*jsonenc.Object) *jsonenc.Object {
	var best *jsonenc.Object
	bestKey := -1
	for _, ev := range events {
		key := 1
		if objStrOr(ev, "state") == "critical" {
			key = 2
		}
		if key > bestKey {
			bestKey = key
			best = ev
		}
	}
	return best
}

// collectAnomalyAgentEvents mirrors _collect_anomaly_agent_events: scan the last-24h anomaly view,
// annotate the rows with the configured anomaly rules, then keep the highest-severity firing event
// per rule id.
func (s *server) collectAnomalyAgentEvents() *orderedAgentEvents {
	events := newOrderedAgentEvents()
	res, err := s.db.Execute(
		"SELECT ServiceName, SignalSource, SignalName, AttrFingerprint, " +
			"argMax(value, time) AS value, argMax(SampleCount, time) AS SampleCount, " +
			"argMax(time, time) AS latest_time " +
			"FROM v_derived_signals_anomaly " +
			"WHERE time >= now() - INTERVAL 24 HOUR " +
			"GROUP BY ServiceName, SignalSource, SignalName, AttrFingerprint")
	if err != nil || len(res.Rows) == 0 {
		return events
	}
	annotated := rowMaps(res)
	s.annotateRowsWithRules(annotated, s.loadAnomalyRulesCtxAny(),
		"SignalSource", "SignalName", "ServiceName", "AttrFingerprint",
		"value", "SampleCount", "latest_time")
	severityRank := map[string]int{"warning": 1, "critical": 2}
	for _, row := range annotated {
		ruleID := strings.TrimSpace(cStr(row, "rule_id"))
		if ruleID == "" {
			continue
		}
		state := normalizeAgentTriggerState(cStrDef(row, "effective_state", "normal"))
		rank, ok := severityRank[state]
		if !ok {
			continue
		}
		ev := jsonenc.NewObject().
			Set("state", state).
			Set("service", cStr(row, "ServiceName")).
			Set("source", cStr(row, "SignalSource")).
			Set("signal", cStr(row, "SignalName")).
			Set("value", row["value"])
		if cur := events.get(ruleID); cur == nil || rank > severityRank[objStrOr(cur, "state")] {
			events.set(ruleID, ev)
		}
	}
	return events
}

// collectTagRuleAgentEvents mirrors _collect_tag_rule_agent_events: count recently auto-applied tags
// and emit a warning event for each tag rule whose (tag_key, tag_value) matched.
func (s *server) collectTagRuleAgentEvents(lookbackMinutes int) *orderedAgentEvents {
	events := newOrderedAgentEvents()
	tagRules := s.loadTagRulesCtx()
	if len(tagRules) == 0 {
		return events
	}
	type tagKV struct{ key, value string }
	lookup := map[tagKV]map[string]any{}
	for _, ruleAny := range tagRules {
		rule, ok := ruleAny.(map[string]any)
		if !ok {
			continue
		}
		lookup[tagKV{mapStr(rule, "tag_key"), mapStr(rule, "tag_value")}] = rule
	}
	minVersion := nowUTC().Add(-time.Duration(lookbackMinutes) * time.Minute).UnixMilli()
	res, err := s.db.Execute(
		"SELECT TagKey, TagValue, count() AS c FROM sobs_record_tags FINAL "+
			"WHERE IsDeleted = 0 AND IsAuto = 1 AND Version >= ? "+
			"GROUP BY TagKey, TagValue", minVersion)
	if err != nil {
		return events
	}
	for _, row := range rowMaps(res) {
		key := tagKV{cStr(row, "TagKey"), cStr(row, "TagValue")}
		rule, ok := lookup[key]
		if !ok {
			continue
		}
		events.set(mapStr(rule, "id"), jsonenc.NewObject().
			Set("state", "warning").
			Set("tag_key", key.key).
			Set("tag_value", key.value).
			Set("matches", cInt(row, "c")))
	}
	return events
}

// agentRuleFromCtx rebuilds the typed agentRule from a loadAgentRulesCtx() map.
func agentRuleFromCtx(m map[string]any) *agentRule {
	actions := []string{}
	if list, ok := m["actions"].([]any); ok {
		for _, a := range list {
			if t := strings.TrimSpace(toStr(a)); t != "" {
				actions = append(actions, t)
			}
		}
	}
	isEnabled, _ := m["is_enabled"].(bool)
	return &agentRule{
		id:               mapStr(m, "id"),
		name:             mapStr(m, "name"),
		description:      mapStr(m, "description"),
		triggerType:      mapStr(m, "trigger_type"),
		triggerRefID:     mapStr(m, "trigger_ref_id"),
		triggerState:     mapStr(m, "trigger_state"),
		actions:          actions,
		rateLimitMinutes: mapInt(m, "rate_limit_minutes"),
		isEnabled:        isEnabled,
	}
}

// evaluateAgentRuleTriggers mirrors the automatic agent-rule trigger branch of app.py
// check_notifications. When AI is configured it collects anomaly/tag events, matches them against
// each enabled agent rule (severity/state gate + per-rule rate limit), registers a raw-preservation
// window, and runs the agent flow. Returns the agent_runs list (empty when AI is unconfigured).
func (s *server) evaluateAgentRuleTriggers() []any {
	agentResults := []any{}
	settings := map[string]string{
		"ai.endpoint_url":   s.loadAISetting("ai.endpoint_url", ""),
		"ai.model":          s.loadAISetting("ai.model", ""),
		"ai.api_key":        s.loadAISetting("ai.api_key", ""),
		"ai.system_prompt":  s.loadAISetting("ai.system_prompt", ""),
		"ai.thinking_level": s.loadAISetting("ai.thinking_level", ""),
		"ai.github_repo":    s.loadAISetting("ai.github_repo", ""),
		"ai.github_token":   s.loadAISetting("ai.github_token", ""),
	}
	if settings["ai.endpoint_url"] == "" || settings["ai.model"] == "" {
		return agentResults
	}

	anomalyEvents := s.collectAnomalyAgentEvents()
	tagEvents := s.collectTagRuleAgentEvents(5)
	allAnomaly := anomalyEvents.values()
	allTag := tagEvents.values()

	for _, ruleAny := range s.loadAgentRulesCtx() {
		m, ok := ruleAny.(map[string]any)
		if !ok {
			continue
		}
		rule := agentRuleFromCtx(m)
		if !rule.isEnabled {
			continue
		}
		triggerType := strings.ToLower(strings.TrimSpace(rule.triggerType))
		triggerRefID := strings.TrimSpace(rule.triggerRefID)
		triggerState := strings.ToLower(strings.TrimSpace(rule.triggerState))

		var event *jsonenc.Object
		switch triggerType {
		case "anomaly_rule":
			if triggerRefID != "" {
				event = anomalyEvents.get(triggerRefID)
			} else if len(allAnomaly) > 0 {
				event = pickHighestSeverityEvent(allAnomaly)
			}
		case "tag_rule":
			if triggerRefID != "" {
				event = tagEvents.get(triggerRefID)
			} else if len(allTag) > 0 {
				event = allTag[0]
			}
		default:
			continue
		}
		if event == nil {
			continue
		}

		eventState := normalizeAgentTriggerState(objStrDef(event, "state", "normal"))
		if !agentRuleTriggerStateMatches(triggerState, eventState) {
			continue
		}

		rateLimitMinutes := rule.rateLimitMinutes
		if rateLimitMinutes == 0 {
			rateLimitMinutes = 60
		}
		lastRunTs := s.agentRuleLastRunTs(rule.id)
		elapsedMinutes := (float64(nowUTC().Unix()) - lastRunTs) / 60.0
		if elapsedMinutes < float64(rateLimitMinutes) && lastRunTs > 0 {
			agentResults = append(agentResults, jsonenc.NewObject().
				Set("rule_id", rule.id).
				Set("status", "skipped_rate_limited").
				Set("elapsed_minutes", roundHalfEven(elapsedMinutes, 2)))
			continue
		}

		tctx := jsonenc.NewObject().
			Set("rule_name", rule.name).
			Set("trigger_state", eventState).
			Set("trigger_type", triggerType).
			Set("trigger_ref_id", triggerRefID).
			Set("extra", string(jsonenc.Encode(event, dumpsDefault)))

		// Register a raw preservation window when an anomaly or tag event triggers an agent.
		s.registerRawWindow(nowUTC(), triggerType, triggerRefID, objStrOr(event, "service"), "", "")

		outcome := s.runAgentRuleInstance(rule, settings, tctx)
		res := jsonenc.NewObject().Set("ok", outcome.ok).Set("rule_id", rule.id).Set("run_id", outcome.runID)
		if outcome.ok {
			res.Set("result", outcome.result)
		} else {
			res.Set("error", outcome.err)
		}
		agentResults = append(agentResults, res)
	}
	return agentResults
}
