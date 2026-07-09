package main

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// notifViewRedirect reproduces app.py's redirect target for create_notification_rule:
// url_for("view_notifications", edit_rule=edit_rule_id) when editing (the tag-regex error path),
// else url_for("view_notifications"). It mirrors render.go's url_for query encoding byte-for-byte
// (Werkzeug-style QueryEscape with the %3A->: relaxation) so the redirect Location matches Python.
func (s *server) notifViewRedirect(r *http.Request, editRuleID string) string {
	base := s.effectiveBasePath(r) + "/settings/notifications"
	if editRuleID == "" {
		return base
	}
	enc := strings.ReplaceAll(url.QueryEscape(editRuleID), "%3A", ":")
	return base + "?edit_rule=" + enc
}

// pyParseInt mirrors CPython's int(str) for base 10: it strips surrounding whitespace, accepts an
// optional sign and underscore digit-group separators, and on failure returns the empty value and
// the exact ValueError text "invalid literal for int() with base 10: '<original>'" (using repr-
// style quoting of the ORIGINAL, un-stripped argument). On success it returns (n, "").
func pyParseInt(raw string) (int, string) {
	// Python's int() strips ASCII whitespace (and a few Unicode spaces); the callers already
	// .strip() the value, but mirror the strip so the parse — not the error repr — matches.
	s := strings.Trim(raw, " \t\n\r\v\f")
	n, ok := pyIntBody(s)
	if !ok {
		// pyRepr (fix_crypto_misc.go) single-quotes + escapes, matching CPython repr() of the
		// ORIGINAL argument for the realistic (quote-free) TTL inputs.
		return 0, "invalid literal for int() with base 10: " + pyRepr(raw, true)
	}
	return n, ""
}

// pyIntBody parses the sign + underscore-grouped decimal digits CPython's int() accepts.
func pyIntBody(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	// Underscores are permitted only between digits (not leading/trailing/doubled).
	if strings.HasPrefix(s, "_") || strings.HasSuffix(s, "_") || strings.Contains(s, "__") {
		return 0, false
	}
	digits := strings.ReplaceAll(s, "_", "")
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// chartObjStr reads a string field from a getCharts() result entry verbatim (no strip / no
// falsy-coalesce, unlike objStrOr) — getCharts stores Go strings for id/title/chart_type/
// query/options_json, so the tombstone payload reproduces _get_charts' values exactly.
func chartObjStr(o *jsonenc.Object, key string) string {
	if v, ok := o.Get(key); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// chartObjInt reads an int field (getCharts stores position via cInt -> int) verbatim.
func chartObjInt(o *jsonenc.Object, key string) int {
	if v, ok := o.Get(key); ok {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
	}
	return 0
}
