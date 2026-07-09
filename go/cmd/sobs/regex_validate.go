package main

import (
	"net/http"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Regex Validate API — POST /api/{logs,errors,traces,metrics,rum}/validate-regex.
//
// Port of app.py's *_validate_regex routes (api_logs_validate_regex etc.) plus their shared
// helpers: _parse_and_validate_regex_expression_for_api (app.py:23906), _regex_best_effort_sample
// (23913), _regex_scope_text / _regex_scope_time_conditions (23874-23903) and _truncate_sample
// (23867). The five routes share one handler that branches on the request path to select the
// per-source FROM clause, sample column and scope WHERE builder; everything else is identical.
//
// Used by the regex autocomplete / IntelliSense on each filter panel: it RE2-validates the
// `pattern` expression and, on success, probes recent rows for a best-effort sample match.

const (
	regexSampleMaxLen           = 200 // _REGEX_SAMPLE_MAX_LEN
	regexScopeMaxLen            = 200 // _REGEX_SCOPE_MAX_LEN
	regexValidateRecentHours    = 24  // _REGEX_VALIDATE_RECENT_HOURS
	regexValidateCandidateLimit = 2000
)

// regexValidateSource is the per-route configuration that differs across the five sources: the
// FROM subquery, the column whose value is returned as a sample, the ORDER BY / time-window
// column, and a builder that translates the scope payload into source-specific WHERE conditions.
type regexValidateSource struct {
	fromSQL      string
	sampleColumn string
	orderColumn  string
	timeColumn   string
	scopeWhere   func(scope map[string]any) (parts []string, params []any)
}

// regexValidateSourceForPath maps the request path to its source config. Mirrors the five
// near-identical app.py handlers; the path is the only thing that distinguishes them.
func regexValidateSourceForPath(path string) regexValidateSource {
	switch path {
	case "/api/errors/validate-regex":
		return regexValidateSource{
			fromSQL: "(" + errorSourcesSQL + ")", sampleColumn: "Body", orderColumn: "Timestamp", timeColumn: "Timestamp",
			scopeWhere: func(scope map[string]any) (parts []string, params []any) {
				if service := regexScopeText(scope, "service", regexScopeMaxLen); service != "" {
					parts = append(parts, "ServiceName = ?")
					params = append(params, service)
				}
				return parts, params
			},
		}
	case "/api/traces/validate-regex":
		return regexValidateSource{
			fromSQL: "otel_traces", sampleColumn: "SpanName", orderColumn: "Timestamp", timeColumn: "Timestamp",
			scopeWhere: func(scope map[string]any) (parts []string, params []any) {
				service := regexScopeText(scope, "service", regexScopeMaxLen)
				traceID := regexScopeText(scope, "trace_id", 64)
				if service != "" {
					parts = append(parts, "ServiceName = ?")
					params = append(params, service)
				}
				if traceID != "" {
					parts = append(parts, "TraceId = ?")
					params = append(params, traceID)
				}
				return parts, params
			},
		}
	case "/api/metrics/validate-regex":
		return regexValidateSource{
			fromSQL: "v_derived_signals_anomaly", sampleColumn: "SignalName", orderColumn: "time", timeColumn: "time",
			scopeWhere: func(scope map[string]any) (parts []string, params []any) {
				service := regexScopeText(scope, "service", regexScopeMaxLen)
				source := regexScopeText(scope, "source", regexScopeMaxLen)
				signal := regexScopeText(scope, "signal", regexScopeMaxLen)
				attrFP := regexScopeText(scope, "attr_fp", 64)
				if service != "" {
					parts = append(parts, "ServiceName = ?")
					params = append(params, service)
				}
				if source != "" {
					parts = append(parts, "SignalSource = ?")
					params = append(params, source)
				}
				if signal != "" {
					parts = append(parts, "SignalName = ?")
					params = append(params, signal)
				}
				if attrFP != "" {
					parts = append(parts, "AttrFingerprint = ?")
					params = append(params, attrFP)
				}
				return parts, params
			},
		}
	case "/api/rum/validate-regex":
		return regexValidateSource{
			fromSQL: "hyperdx_sessions", sampleColumn: "Body", orderColumn: "Timestamp", timeColumn: "Timestamp",
			scopeWhere: func(scope map[string]any) (parts []string, params []any) {
				eventType := regexScopeText(scope, "type", regexScopeMaxLen)
				errorSource := regexScopeText(scope, "error_source", regexScopeMaxLen)
				if eventType != "" {
					parts = append(parts, "EventName = ?")
					params = append(params, eventType)
				}
				if errorSource != "" {
					parts = append(parts, "LogAttributes['errorSource'] = ?")
					params = append(params, errorSource)
				}
				return parts, params
			},
		}
	default: // "/api/logs/validate-regex"
		return regexValidateSource{
			fromSQL: "otel_logs", sampleColumn: "Body", orderColumn: "Timestamp", timeColumn: "Timestamp",
			scopeWhere: func(scope map[string]any) (parts []string, params []any) {
				service := regexScopeText(scope, "service", regexScopeMaxLen)
				level := regexScopeText(scope, "level", regexScopeMaxLen)
				traceID := regexScopeText(scope, "trace_id", 64)
				if service != "" {
					parts = append(parts, "ServiceName = ?")
					params = append(params, service)
				}
				if level != "" {
					parts = append(parts, "SeverityText = ?")
					params = append(params, level)
				}
				if traceID != "" {
					parts = append(parts, "TraceId = ?")
					params = append(params, traceID)
				}
				return parts, params
			},
		}
	}
}

// handleValidateRegex serves all five validate-regex routes. Empty pattern -> {ok:true,sample:null}
// (jsonify, unmasked). On an invalid expression -> {ok:false,error,sample:null} (jsonify). On a
// valid expression it probes a sample and returns {ok:true,sample} (masked); any probe error
// degrades to {ok:true,sample:null} (masked), exactly as the Python try/except does.
func (s *server) handleValidateRegex(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	pattern := bstr(m, "pattern")
	scope, _ := m["scope"].(map[string]any)
	if scope == nil {
		scope = map[string]any{}
	}

	if pattern == "" {
		writeJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("sample", nil))
		return
	}

	includePatterns, excludePatterns, expressionError := s.parseAndValidateRegexExpressionForAPI(pattern)
	if expressionError != "" {
		writeJSON(w, http.StatusOK,
			jsonenc.NewObject().Set("ok", false).Set("error", expressionError).Set("sample", nil))
		return
	}

	src := regexValidateSourceForPath(r.URL.Path)
	whereParts, whereParams := src.scopeWhere(scope)
	timeParts, timeParams := regexScopeTimeConditions(scope, src.timeColumn)
	whereParts = append(whereParts, timeParts...)
	whereParams = append(whereParams, timeParams...)

	sample, err := s.regexBestEffortSample(
		src.fromSQL, src.sampleColumn, src.orderColumn, includePatterns, excludePatterns, whereParts, whereParams)
	if err != nil {
		s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("sample", nil))
		return
	}
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true).Set("sample", sample))
}

// parseAndValidateRegexExpressionForAPI mirrors _parse_and_validate_regex_expression_for_api:
// prepare the RE2 patterns and, on error, strip the leading "Regex error: " for the API payload.
// A non-empty returned string signals an invalid expression.
func (s *server) parseAndValidateRegexExpressionForAPI(expression string) (include, exclude []string, apiError string) {
	include, exclude, regexErr := s.prepareRe2FilterPatterns(expression)
	if regexErr != "" {
		return nil, nil, strings.Replace(regexErr, "Regex error: ", "", 1)
	}
	return include, exclude, ""
}

// regexBestEffortSample mirrors _regex_best_effort_sample: probe only recent candidate rows
// (ORDER BY ... DESC LIMIT candidate_limit) and return the first one whose sample_value matches
// the regex, truncated to a displayable length. Returns nil (Python None) when nothing matches.
func (s *server) regexBestEffortSample(fromSQL, sampleColumn, orderColumn string, includePatterns, excludePatterns, whereParts []string, whereParams []any) (any, error) {
	whereSQL := ""
	if len(whereParts) > 0 {
		whereSQL = "WHERE " + strings.Join(whereParts, " AND ")
	}
	var regexConditions []string
	var regexParams []any
	appendRegexExpressionClauses(&regexConditions, &regexParams, "sample_value", includePatterns, excludePatterns)
	regexWhereSQL := ""
	if len(regexConditions) > 0 {
		regexWhereSQL = "WHERE " + strings.Join(regexConditions, " AND ")
	}
	sql := "SELECT sample_value FROM (" +
		"SELECT " + sampleColumn + " AS sample_value FROM " + fromSQL + " " +
		whereSQL + " ORDER BY " + orderColumn + " DESC LIMIT ?" +
		") " + regexWhereSQL + " LIMIT 1"

	params := make([]any, 0, len(whereParams)+1+len(regexParams))
	params = append(params, whereParams...)
	params = append(params, regexValidateCandidateLimit)
	params = append(params, regexParams...)

	res, err := s.db.Execute(sql, params...)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return truncateRegexSample(nil), nil
	}
	return truncateRegexSample(res.Rows[0][0]), nil
}

// truncateRegexSample mirrors _truncate_sample: shorten an over-long sample to a "<197 chars>..."
// form (code-point based, like Python len()/slicing). nil (None) and "" pass through unchanged.
func truncateRegexSample(sample any) any {
	s, ok := sample.(string)
	if !ok {
		return nil
	}
	if r := []rune(s); len(r) > regexSampleMaxLen {
		return string(r[:regexSampleMaxLen-3]) + "..."
	}
	return s
}

// regexScopeText mirrors _regex_scope_text: a trimmed, length-bounded text value from the scope
// payload ("" when absent/blank). Scope text fields are strings by contract (like bstr).
func regexScopeText(scope map[string]any, key string, maxLen int) string {
	v, _ := scope[key].(string)
	raw := strings.TrimSpace(v)
	if raw == "" {
		return ""
	}
	if r := []rune(raw); len(r) > maxLen {
		return string(r[:maxLen])
	}
	return raw
}

// regexScopeTimeConditions mirrors _regex_scope_time_conditions: honor a requested from_ts/to_ts
// window when valid, otherwise fall back to a recent bounded window (now() - 24h).
func regexScopeTimeConditions(scope map[string]any, column string) (conds []string, params []any) {
	var fromTS, toTS string
	if fromRaw := regexScopeText(scope, "from_ts", 64); fromRaw != "" {
		fromTS = normalizeChTimestamp(fromRaw)
	}
	if toRaw := regexScopeText(scope, "to_ts", 64); toRaw != "" {
		toTS = normalizeChTimestamp(toRaw)
	}
	conds, params = timeWindowConditions(column, fromTS, toTS)
	if len(conds) == 0 {
		return []string{column + " >= now() - INTERVAL ? HOUR"}, []any{regexValidateRecentHours}
	}
	return conds, params
}
