package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// autoTagSlugRE mirrors re.sub(r"[^a-z0-9]+", "_", ...) used by _auto_tag_slug.
var autoTagSlugRE = regexp.MustCompile(`[^a-z0-9]+`)

// inferEnv*RE mirror the four _infer_env_from_service regexes (evaluated in order).
var (
	inferEnvProdRE  = regexp.MustCompile(`(^|[-_.])(prod|production)($|[-_.])`)
	inferEnvStageRE = regexp.MustCompile(`(^|[-_.])(stg|stage|staging)($|[-_.])`)
	inferEnvDevRE   = regexp.MustCompile(`(^|[-_.])(dev|development)($|[-_.])`)
	inferEnvTestRE  = regexp.MustCompile(`(^|[-_.])(qa|test|testing|uat)($|[-_.])`)
)

// autoTagSlug mirrors app.py _auto_tag_slug: lowercase, collapse non-alnum runs to "_",
// strip leading/trailing "_", fall back, truncate to max_len.
func autoTagSlug(value, fallback string) string {
	raw := strings.ToLower(strings.TrimSpace(value))
	slug := strings.Trim(autoTagSlugRE.ReplaceAllString(raw, "_"), "_")
	if slug == "" {
		slug = fallback
	}
	if len(slug) > 64 {
		slug = slug[:64]
	}
	return slug
}

// inferEnvFromService mirrors app.py _infer_env_from_service.
func inferEnvFromService(serviceName string) string {
	name := strings.ToLower(strings.TrimSpace(serviceName))
	if name == "" {
		return ""
	}
	switch {
	case inferEnvProdRE.MatchString(name):
		return "production"
	case inferEnvStageRE.MatchString(name):
		return "staging"
	case inferEnvDevRE.MatchString(name):
		return "development"
	case inferEnvTestRE.MatchString(name):
		return "test"
	}
	return ""
}

// buildAutoTagRuleCandidates mirrors app.py _build_auto_tag_rule_candidates: scan recent
// telemetry per selected record type, propose tag-rule candidates, skipping ones that
// duplicate existing rules or have empty values. Returns the sorted candidates plus
// {examined, existing, invalid} counters.
func (s *server) buildAutoTagRuleCandidates(hours, minCount int, serviceFilter string, recordTypes []string) ([]any, map[string]int) {
	allowed := map[string]bool{"log": true, "trace": true, "error": true, "ai": true, "rum": true}
	selected := map[string]bool{}
	for _, rt := range recordTypes {
		if allowed[rt] {
			selected[rt] = true
		}
	}
	if len(selected) == 0 {
		selected = map[string]bool{"log": true, "trace": true, "error": true, "ai": true, "rum": true}
	}

	// existing_keys: 7-tuple per existing rule (record_types joined+sorted, then the match/tag fields).
	existingKeys := map[string]bool{}
	for _, rv := range s.loadTagRulesCtx() {
		rule, _ := rv.(map[string]any)
		if rule == nil {
			continue
		}
		var rts []string
		if lst, ok := rule["record_types"].([]any); ok {
			for _, t := range lst {
				if str := strings.TrimSpace(fmt.Sprintf("%v", t)); str != "" {
					rts = append(rts, str)
				}
			}
		}
		sort.Strings(rts)
		existingKeys[ruleKeyJoin(strings.Join(rts, ","),
			cStr(rule, "match_field"), cStr(rule, "match_operator"), cStr(rule, "match_value"),
			cStr(rule, "match_attr_key"), cStr(rule, "tag_key"), cStr(rule, "tag_value"))] = true
	}

	candidates := []any{}
	examined := 0
	skippedExisting := 0
	skippedInvalid := 0

	appendCandidate := func(recordType, name, matchField, matchOperator, matchValue, tagKey, tagValue string, pointCount int, matchAttrKey string) {
		if strings.TrimSpace(matchValue) == "" || strings.TrimSpace(tagKey) == "" || strings.TrimSpace(tagValue) == "" {
			skippedInvalid++
			return
		}
		key := ruleKeyJoin(recordType, matchField, matchOperator, matchValue, matchAttrKey, tagKey, tagValue)
		if existingKeys[key] {
			skippedExisting++
			return
		}
		candidates = append(candidates, map[string]any{
			"name": name, "record_types": []any{recordType},
			"match_field": matchField, "match_operator": matchOperator, "match_value": matchValue,
			"match_attr_key": matchAttrKey, "tag_key": tagKey, "tag_value": tagValue,
			"point_count": pointCount,
		})
	}

	whereService := ""
	if serviceFilter != "" {
		whereService = " AND ServiceName = " + sqlLiteral(serviceFilter)
	}
	rows := func(sql string) []map[string]any {
		res, err := s.db.Execute(sql)
		if err != nil {
			return nil
		}
		return rowMaps(res)
	}

	if selected["log"] {
		r := rows(fmt.Sprintf(
			"SELECT ServiceName, count() AS c FROM otel_logs "+
				"WHERE Timestamp >= now() - INTERVAL %d HOUR AND ServiceName != ''%s "+
				"GROUP BY ServiceName HAVING c >= %d ORDER BY c DESC", hours, whereService, minCount))
		examined += len(r)
		for _, row := range r {
			service := cStr(row, "ServiceName")
			count := cInt(row, "c")
			if env := inferEnvFromService(service); env != "" {
				appendCandidate("log", "log env="+env, "service_name", "contains", service, "env", env, count, "")
				continue
			}
			appendCandidate("log", "log service="+service, "service_name", "eq", service, "service", service, count, "")
		}
	}

	if selected["trace"] {
		r := rows(fmt.Sprintf(
			"SELECT ServiceName, count() AS c FROM otel_traces "+
				"WHERE Timestamp >= now() - INTERVAL %d HOUR AND ScopeName != 'sobs-ai' AND ServiceName != ''%s "+
				"GROUP BY ServiceName HAVING c >= %d ORDER BY c DESC", hours, whereService, minCount))
		examined += len(r)
		for _, row := range r {
			service := cStr(row, "ServiceName")
			count := cInt(row, "c")
			if env := inferEnvFromService(service); env != "" {
				appendCandidate("trace", "trace env="+env, "service_name", "contains", service, "env", env, count, "")
				continue
			}
			appendCandidate("trace", "trace service="+service, "service_name", "eq", service, "service", service, count, "")
		}
	}

	if selected["error"] {
		r := rows(fmt.Sprintf(
			"SELECT coalesce(LogAttributes['exception.type'], '') AS ExceptionType, count() AS c "+
				"FROM otel_logs "+
				"WHERE Timestamp >= now() - INTERVAL %d HOUR "+
				"AND (EventName = 'exception' OR SeverityNumber >= 17 OR SeverityText IN ('ERROR','CRITICAL','FATAL'))%s "+
				"GROUP BY ExceptionType HAVING c >= %d ORDER BY c DESC", hours, whereService, minCount))
		examined += len(r)
		for _, row := range r {
			exceptionType := strings.TrimSpace(cStr(row, "ExceptionType"))
			if exceptionType == "" {
				skippedInvalid++
				continue
			}
			count := cInt(row, "c")
			appendCandidate("error", "error type="+autoTagSlug(exceptionType, "error"), "attribute", "eq",
				exceptionType, "error_type", autoTagSlug(exceptionType, "error"), count, "exception.type")
		}
	}

	if selected["ai"] {
		r := rows(fmt.Sprintf(
			"SELECT coalesce(SpanAttributes['gen_ai.provider.name'], '') AS Provider, count() AS c "+
				"FROM otel_traces "+
				"WHERE Timestamp >= now() - INTERVAL %d HOUR AND ScopeName = 'sobs-ai'%s "+
				"GROUP BY Provider HAVING c >= %d ORDER BY c DESC", hours, whereService, minCount))
		examined += len(r)
		for _, row := range r {
			provider := strings.TrimSpace(cStr(row, "Provider"))
			if provider == "" {
				skippedInvalid++
				continue
			}
			count := cInt(row, "c")
			appendCandidate("ai", "ai provider="+autoTagSlug(provider, "provider"), "attribute", "eq",
				provider, "ai_provider", autoTagSlug(provider, "provider"), count, "gen_ai.provider.name")
		}
	}

	if selected["rum"] {
		r := rows(fmt.Sprintf(
			"SELECT EventName, count() AS c FROM hyperdx_sessions "+
				"WHERE Timestamp >= now() - INTERVAL %d HOUR AND EventName != ''%s "+
				"GROUP BY EventName HAVING c >= %d ORDER BY c DESC", hours, whereService, minCount))
		examined += len(r)
		for _, row := range r {
			eventName := cStr(row, "EventName")
			count := cInt(row, "c")
			appendCandidate("rum", "rum event="+autoTagSlug(eventName, "event"), "event_type", "eq",
				eventName, "rum_event", autoTagSlug(eventName, "event"), count, "")
		}
	}

	// Python: candidates.sort(key=lambda c: (point_count, name), reverse=True) — descending on
	// both, stable.
	sort.SliceStable(candidates, func(i, j int) bool {
		ci := candidates[i].(map[string]any)
		cj := candidates[j].(map[string]any)
		pi, pj := ci["point_count"].(int), cj["point_count"].(int)
		if pi != pj {
			return pi > pj
		}
		return ci["name"].(string) > cj["name"].(string)
	})

	return candidates, map[string]int{"examined": examined, "existing": skippedExisting, "invalid": skippedInvalid}
}

func ruleKeyJoin(parts ...string) string { return strings.Join(parts, "\x00") }

// listTagCandidateServices mirrors app.py _list_tag_candidate_services: distinct non-empty
// service names across logs/traces/sessions, ordered.
func (s *server) listTagCandidateServices() []any {
	return s.distinctStrings(
		"SELECT DISTINCT ServiceName FROM (" +
			"  SELECT ServiceName FROM otel_logs " +
			"  UNION DISTINCT SELECT ServiceName FROM otel_traces " +
			"  UNION DISTINCT SELECT ServiceName FROM hyperdx_sessions" +
			") WHERE ServiceName != '' ORDER BY ServiceName")
}
