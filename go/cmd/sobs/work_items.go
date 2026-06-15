package main

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// workItemUTCISOOffsetRe mirrors the offset-suffix probe in _serialize_github_work_item_row's
// nested _to_utc_iso: a value already carrying a tz designator must not get a second "+00:00".
var workItemUTCISOOffsetRe = regexp.MustCompile(`[zZ]|[+\-]\d\d:?\d\d$`)

// workItemUTCISOLayouts are the fromisoformat-compatible forms _to_utc_iso feeds to
// datetime.fromisoformat after normalization (space->T, trailing tz forced to +00:00).
var workItemUTCISOLayouts = []string{
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02T15:04:05-07:00",
}

// workItemToUTCISO ports the nested _to_utc_iso in app.py _serialize_github_work_item_row: a chdb
// DateTime64 value (e.g. "2026-03-29 12:00:00.000") is normalized to UTC ISO-8601 with
// millisecond precision and a "Z" suffix ("2026-03-29T12:00:00.000Z"). Empty input yields "" and
// an unparseable value is returned unchanged (the Python datetime.fromisoformat ValueError path).
func workItemToUTCISO(value any) string {
	raw := strings.TrimSpace(toStr(value))
	if raw == "" {
		return ""
	}
	// Python: normalized = raw.replace(" ", "T") (replaces every space).
	normalized := strings.ReplaceAll(raw, " ", "T")
	if strings.HasSuffix(normalized, "Z") {
		normalized = normalized[:len(normalized)-1] + "+00:00"
	}
	if !workItemUTCISOOffsetRe.MatchString(normalized) {
		normalized += "+00:00"
	}
	var dt time.Time
	parsed := false
	for _, layout := range workItemUTCISOLayouts {
		if t, err := time.Parse(layout, normalized); err == nil {
			dt = t
			parsed = true
			break
		}
	}
	if !parsed {
		// datetime.fromisoformat raised ValueError -> return the original raw string.
		return raw
	}
	// dt.isoformat(timespec="milliseconds").replace("+00:00", "Z").
	out := dt.UTC().Format("2006-01-02T15:04:05.000-07:00")
	return strings.Replace(out, "+00:00", "Z", 1)
}

// serializeGithubWorkItemRow ports app.py _serialize_github_work_item_row (~35 fields):
// CreatedAt/CompletedAt are normalized to UTC ISO, RelatedIssueUrls is JSON-parsed (defaulting to
// []), and the numeric/bool columns are coerced exactly as the Python int()/float()/bool(int())
// calls. Returns an ordered *jsonenc.Object so the same value serves both the JSON API (jsonify)
// and the work_items.html template (item.field access).
func serializeGithubWorkItemRow(m map[string]any) *jsonenc.Object {
	related := safeJSONLoadsList(cStr(m, "RelatedIssueUrls"))
	return jsonenc.NewObject().
		Set("id", cStr(m, "Id")).
		Set("created_at", workItemToUTCISO(m["CreatedAt"])).
		Set("completed_at", workItemToUTCISO(m["CompletedAt"])).
		Set("agent_rule_id", cStr(m, "AgentRuleId")).
		Set("agent_rule_name", cStr(m, "AgentRuleName")).
		Set("agent_action", cStr(m, "AgentAction")).
		Set("service", cStr(m, "ServiceName")).
		Set("anomaly_rule_id", cStr(m, "AnomalyRuleId")).
		Set("anomaly_state", cStr(m, "AnomalyState")).
		Set("signal_source", cStr(m, "SignalSource")).
		Set("signal_name", cStr(m, "SignalName")).
		Set("signal_value", cFloat(m, "SignalValue")).
		Set("github_repo", cStr(m, "GithubRepo")).
		Set("dedup_key", cStr(m, "DedupKey")).
		Set("dedup_decision", cStr(m, "DedupDecision")).
		Set("dedup_confidence", cFloat(m, "DedupConfidence")).
		Set("issue_number", cInt(m, "IssueNumber")).
		Set("issue_url", cStr(m, "IssueUrl")).
		Set("canonical_issue_number", cInt(m, "CanonicalIssueNumber")).
		Set("canonical_issue_url", cStr(m, "CanonicalIssueUrl")).
		Set("related_issue_urls", related).
		Set("occurrence_count", cIntDef(m, "OccurrenceCount", 1)).
		Set("issue_state", cStr(m, "IssueState")).
		Set("issue_title", cStr(m, "IssueTitle")).
		Set("analysis_summary", cStr(m, "AnalysisSummary")).
		Set("suggestion_summary", cStr(m, "SuggestionSummary")).
		Set("copilot_assignment_requested_at", cInt(m, "CopilotAssignmentRequestedAt")).
		Set("copilot_assignment_status", cStrDef(m, "CopilotAssignmentStatus", "not_requested")).
		Set("copilot_assignment_reason", cStr(m, "CopilotAssignmentReason")).
		Set("pr_linked", cBool(m, "PrLinked")).
		Set("pr_number", cInt(m, "PrNumber")).
		Set("pr_url", cStr(m, "PrUrl"))
}

// cIntDef mirrors Python `int(row[key] or fallback)` — a falsy (0/""/nil) cell yields fallback.
// Used for OccurrenceCount, whose Python default is 1.
func cIntDef(m map[string]any, key string, def int) int {
	if n := cInt(m, key); n != 0 {
		return n
	}
	return def
}

// safeJSONLoadsList ports _safe_json_loads(value, []) for the RelatedIssueUrls column: parse the
// stored JSON; return [] when blank, on a parse error, or when the parsed value is not a list.
func safeJSONLoadsList(raw string) []any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []any{}
	}
	v, err := parseJSONValue([]byte(raw))
	if err != nil {
		return []any{}
	}
	if arr, ok := v.([]any); ok {
		return arr
	}
	return []any{}
}

// workItemsTimeWindow ports app.py _parse_time_window_args for the work-items page: it reads
// from_ts/to_ts/window_s, normalizes each timestamp to a chdb DateTime64 string, derives to_ts
// from window_s when only from_ts is given, and validates ordering. Returns (from_ts, to_ts,
// time_error); on a parse failure both timestamps are "" and a non-empty error is returned.
func workItemsTimeWindow(r *http.Request) (string, string, string) {
	const isoErr = "Invalid time value. Use ISO-8601, e.g. 2026-03-29T12:00:00Z"
	q := r.URL.Query()
	fromRaw := strings.TrimSpace(q.Get("from_ts"))
	toRaw := strings.TrimSpace(q.Get("to_ts"))
	windowRaw := strings.TrimSpace(q.Get("window_s"))

	fromTs := ""
	if fromRaw != "" {
		fromTs = normalizeCHTimestamp(fromRaw)
	}
	toTs := ""
	if toRaw != "" {
		toTs = normalizeCHTimestamp(toRaw)
	}

	const layout = "2006-01-02 15:04:05.999999"
	if fromTs != "" && toTs == "" && windowRaw != "" {
		windowS, ok := atoiStrict(windowRaw)
		if !ok {
			return "", "", isoErr
		}
		if windowS < 1 {
			windowS = 1
		}
		fromDt, err := time.Parse(layout, fromTs)
		if err != nil {
			return "", "", isoErr
		}
		toTs = normalizeCHTimestamp(fromDt.Add(time.Duration(windowS) * time.Second))
	}
	if fromTs != "" && toTs != "" {
		fromDt, err1 := time.Parse(layout, fromTs)
		toDt, err2 := time.Parse(layout, toTs)
		if err1 != nil || err2 != nil {
			return "", "", isoErr
		}
		if !toDt.After(fromDt) {
			return "", "", "Invalid time window: to_ts must be later than from_ts"
		}
	}
	return fromTs, toTs, ""
}

// atoiStrict parses a base-10 integer the way Python's int(window_s_raw) does for the supported
// (sign + digits) forms, reporting failure so the caller can emit the ISO-8601 error.
func atoiStrict(raw string) (int, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	start, neg := 0, false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		start = 1
	}
	if start >= len(s) {
		return 0, false
	}
	n := 0
	for i := start; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}
