package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
)

type fakeIncidentDB struct{}

func (fakeIncidentDB) Execute(query string, params ...any) (*store.Result, error) {
	return &store.Result{}, nil
}
func (fakeIncidentDB) InsertJSONEachRow(table string, rows []map[string]any) (int, error) {
	return 0, nil
}
func (fakeIncidentDB) Close() error { return nil }

// renderIncidentHTML renders incident.html directly (bypassing the HTTP handler, which needs
// a real store for its DB-backed branches) with traceID/service substituted into the minimal
// context the "## Links" script block reads.
func renderIncidentHTML(t *testing.T, traceID, service string) string {
	t.Helper()
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: fakeIncidentDB{}}
	ctx := map[string]any{
		"request":                           map[string]any{"endpoint": "view_incident", "args": map[string]any{}, "cookies": map[string]any{}},
		"config":                            map[string]any{"ENABLE_FIRST_RUN_TOUR": false},
		"sobs_version":                      "dev",
		"mobile_breakpoint_max":             "575.98px",
		"query_enabled":                     false,
		"kubernetes_enabled":                false,
		"raise_issue_mask_toggle_effective": false,
		"trace_id":                          traceID,
		"error_id":                          "",
		"rum_session":                       "",
		"rum_ts":                            "",
		"primary_error":                     nil,
		"primary_trace":                     nil,
		"primary_rum":                       nil,
		"service":                           service,
		"from_ts":                           "",
		"to_ts":                             "",
		"window_minutes":                    60,
		"related_errors":                    []any{},
		"related_log_count":                 0,
		"related_span_count":                0,
		"related_rum_count":                 0,
		"related_rum_sessions":              0,
		"related_rum_error_count":           0,
		"related_rum_events":                []any{},
		"raw_windows":                       []any{},
		"metrics_context": map[string]any{
			"source_mode": "none", "total_points": 0, "series": []any{},
			"match_mode": "none", "match_label": "no match", "match_dimensions": []any{},
		},
		"anomaly_state":   nil,
		"work_item_links": map[string]any{},
		"time_error":      "",
		"error_msg":       "",
	}
	out, err := s.newEngine().Render("incident.html", ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	return out
}

// A trace_id/service containing a single quote must never reach the "## Links" script block's
// url_for(...) | safe calls unescaped: url_for's query encoding intentionally leaves a literal
// "'" in place (matching Werkzeug's byte-for-byte encoding, safe in an href/URL context), but
// these values are embedded inside single-quoted JS string literals — an unescaped quote breaks
// out of the string and the remainder executes as script. GET /incident?trace_id=x');alert(1);//
// reproduced this end to end before the fix (incident.html's url_for(...) | safe calls now
// re-encode the quote via | replace("'", "%27") first).
func TestIncidentLinksScriptEscapesQuoteInTraceID(t *testing.T) {
	malicious := "x');alert(document.cookie);//"
	out := renderIncidentHTML(t, malicious, "")

	needle := "lines.push('- [Trace Detail]("
	idx := strings.Index(out, needle)
	if idx < 0 {
		t.Fatalf("marker not found in rendered output")
	}
	end := idx + 300
	if end > len(out) {
		end = len(out)
	}
	snippet := out[idx:end]

	if strings.Contains(snippet, "x');alert(document.cookie)") {
		t.Fatalf("JS breakout: malicious trace_id's literal quote broke out of the JS string literal:\n%s", snippet)
	}
	if !strings.Contains(snippet, "x%27") {
		t.Fatalf("expected the payload's quote to be percent-encoded (x%%27), got:\n%s", snippet)
	}
}

// A normal trace_id (no special characters) must render unchanged — the %27 substitution is a
// no-op for legitimate values, so this only regresses if the fix ever over-encodes.
func TestIncidentLinksScriptNormalTraceIDUnchanged(t *testing.T) {
	out := renderIncidentHTML(t, "abc123def456", "")

	needle := "lines.push('- [Trace Detail]("
	idx := strings.Index(out, needle)
	if idx < 0 {
		t.Fatalf("marker not found in rendered output")
	}
	end := idx + 200
	if end > len(out) {
		end = len(out)
	}
	snippet := out[idx:end]

	if !strings.Contains(snippet, "trace_id=abc123def456") {
		t.Fatalf("expected the ordinary trace_id to render unchanged, got:\n%s", snippet)
	}
	if strings.Contains(snippet, "%27") {
		t.Fatalf("did not expect any %%27 substitution for a trace_id with no quote, got:\n%s", snippet)
	}
}
