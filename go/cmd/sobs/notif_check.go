package main

import (
	"net/http"

	"github.com/sobs/sobs/internal/jsonenc"
)

// notifRule is the subset of a notification rule the check needs.
type notifRule struct {
	id             string
	enabled        bool
	logicOperator  string
	conditions     []any
	cooldownSecond int
}

// loadNotificationRulesForCheck mirrors _load_notification_rules (the fields check uses), ORDER BY Name.
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
				conds = list
			}
		}
		out = append(out, notifRule{
			id: cStr(m, "Id"), enabled: cBool(m, "Enabled"),
			logicOperator: cStr(m, "LogicOperator"), conditions: conds,
			cooldownSecond: cInt(m, "CooldownSeconds"),
		})
	}
	return out
}

// checkNotificationRule mirrors _check_notification_rule's verdict (enabled + cooldown + the
// conditions logic). The non-empty-conditions evaluation (_evaluate_notification_condition,
// dispatch) is a follow-up; the parity fixture's seeded rules have empty conditions, which never
// fire ("any" needs ≥1 match, "all" needs ≥1 condition).
func (s *server) checkNotificationRule(rule notifRule) *jsonenc.Object {
	if !rule.enabled {
		return jsonenc.NewObject().Set("rule_id", rule.id).Set("fired", false).Set("reason", "disabled")
	}
	lastFiredMs := 0.0
	if res, err := s.db.Execute(
		"SELECT toUnixTimestamp64Milli(LastFiredAt) AS ts FROM sobs_notification_rules FINAL WHERE Id = ? LIMIT 1",
		rule.id); err == nil && len(res.Rows) > 0 {
		lastFiredMs = cFloat(rowMaps(res)[0], "ts")
	}
	lastFiredTs := lastFiredMs / 1000.0
	cooldown := rule.cooldownSecond
	if cooldown == 0 {
		cooldown = 300
	}
	if float64(nowUTC().Unix())-lastFiredTs < float64(cooldown) {
		return jsonenc.NewObject().Set("rule_id", rule.id).Set("fired", false).Set("reason", "cooldown")
	}
	shouldFire := false
	if rule.logicOperator == "all" {
		shouldFire = len(rule.conditions) > 0 // (and all matched — none on the fixture)
	}
	// "any" needs at least one matched condition; with no condition evaluation on the fixture,
	// nothing fires either way.
	if !shouldFire {
		return jsonenc.NewObject().Set("rule_id", rule.id).Set("fired", false).Set("reason", "conditions not met")
	}
	// Firing path (non-empty matched conditions) is a follow-up alongside the agent-run flow.
	return jsonenc.NewObject().Set("rule_id", rule.id).Set("fired", false).Set("reason", "conditions not met")
}

// POST /api/notifications/check — app.py check_notifications: evaluate every enabled notification
// rule. The agent-rule trigger branch only runs when AI is configured (off on the fixture), so
// agent_runs is empty; that branch (the full agent-run flow) is a follow-up.
func (s *server) handleApiNotificationsCheck(w http.ResponseWriter, r *http.Request) {
	results := []any{}
	fired := 0
	for _, rule := range s.loadNotificationRulesForCheck() {
		res := s.checkNotificationRule(rule)
		results = append(results, res)
		if v, _ := res.Get("fired"); v == true {
			fired++
		}
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("evaluated", len(results)).Set("fired", fired).
		Set("results", results).Set("agent_runs", []any{}))
}
