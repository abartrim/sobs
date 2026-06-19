package main

import (
	"regexp"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// mcpAPIKeyMax mirrors mcp.py _MCP_API_KEY_MAX (maximum number of concurrent keys).
const mcpAPIKeyMax = 20

// mcpAPIKeysSetting mirrors mcp.py _MCP_API_KEYS_SETTING.
const mcpAPIKeysSetting = "mcp.api_keys"

// loadMcpAPIKeys mirrors mcp.py _load_mcp_api_keys: read the mcp.api_keys app setting (a JSON
// array of descriptors; "[]" default), returning the list as order-preserving *jsonenc.Object
// items so the create/delete/save round-trip keeps insertion order. A non-list / invalid value
// yields an empty list (json.loads guarded by isinstance(list) + JSONDecodeError fallback).
func (s *server) loadMcpAPIKeys() []any {
	raw, _ := s.appSetting(mcpAPIKeysSetting)
	if raw == "" {
		raw = "[]"
	}
	v, err := parseJSONValue([]byte(raw))
	if err != nil {
		return []any{}
	}
	list, ok := v.([]any)
	if !ok {
		return []any{}
	}
	return list
}

// saveMcpAPIKeys mirrors mcp.py _save_mcp_api_keys: persist the descriptor list via
// _set_app_setting(mcp.api_keys, json.dumps(keys, ensure_ascii=False)). An empty list is stored
// as the literal "[]" (Python json.dumps([]) -> "[]"; _set_app_setting writes it verbatim).
func (s *server) saveMcpAPIKeys(keys []any) {
	saved := "[]"
	if len(keys) > 0 {
		saved = string(jsonenc.Encode(keys, jsonDumpsDefault))
	}
	_ = s.setAppSetting(mcpAPIKeysSetting, saved)
}

// pythonEquals1 reports whether v `== 1` under Python equality (the `template_version != 1`
// check in import_chart): the number 1 / 1.0, OR the bool True (Python's `True == 1`). Absent or
// any other value -> false. numEquals covers only the numeric forms, so this adds bool-true.
func pythonEquals1(v any, present bool) bool {
	if !present {
		return false
	}
	if b, ok := v.(bool); ok {
		return b // True == 1 (and False == 0 != 1)
	}
	return numEquals(v, present, 1)
}

// orDefaultVal mirrors `str(value or default)` applied to a parsed JSON value: when the key is
// absent or the value is falsy (None/""/0/false/empty list/empty object), use def; otherwise
// str()-coerce the value. The caller typically .strip()s the result.
func orDefaultVal(v any, present bool, def string) string {
	if !present || isFalsyAny(v) || isEmptyJSONContainer(v) {
		return def
	}
	return pyStr(v, true)
}

// pyRepr mirrors Python repr() for the JSON-decoded scalar types used in error messages:
// None (absent), 'str' (single-quoted), ints/floats verbatim, True/False. Containers fall back
// to a %v rendering (not exercised by the import-version error path, which carries a scalar).
func pyRepr(v any, present bool) string {
	if !present || v == nil {
		return "None"
	}
	switch x := v.(type) {
	case string:
		// CPython prefers single quotes; escape backslashes and single quotes.
		esc := strings.ReplaceAll(x, `\`, `\\`)
		esc = strings.ReplaceAll(esc, `'`, `\'`)
		return "'" + esc + "'"
	case bool:
		if x {
			return "True"
		}
		return "False"
	default:
		return pyStr(v, true)
	}
}

// isEmptyJSONContainer reports whether v is an empty JSON array/object (the parseJSONValue
// shapes), which Python treats as falsy in the `item or ""` idiom. Scalars are never containers.
func isEmptyJSONContainer(v any) bool {
	switch x := v.(type) {
	case []any:
		return len(x) == 0
	case *jsonenc.Object:
		return x.Len() == 0
	default:
		return false
	}
}

// --- output-masking RE2/Python-re reconciliation -----------------------------------------
//
// Python's `re` engine is Unicode-aware: `\d` matches every Unicode decimal digit and `\s`
// matches Unicode whitespace (plus ASCII \v and the \x1c-\x1f separators). Go's RE2 makes `\d`
// = [0-9] and `\s` = [\t\n\f\r ] (ASCII only, and notably WITHOUT \v / \x1c-\x1f). To keep the
// redaction engine faithful to app.py without changing ASCII bytes, we rewrite the COMPILED form
// of the sensitive patterns only (the literal pattern strings returned by /api/settings/masking/
// rules stay verbatim — that contract is the embedded masking_defaults.json). `\b` and lookahead
// have no RE2 equivalent; we leave those untouched (documented limitation).

// re2UnicodeWhitespaceClass is the RE2 character-class body that exactly reproduces Python `re`'s
// Unicode `\s`: ASCII [\t\n\v\f\r] + the \x1c-\x1f separators + space, plus NEL (\x85) and the
// Unicode \p{Z} separator categories. Verified char-for-char against CPython's re.match(r"\s").
const re2UnicodeWhitespaceClass = `\t\n\v\f\r\x1c\x1d\x1e\x1f \x{85}\p{Z}`

// pythonizeMaskRegex rewrites a Python `re` pattern into the closest RE2 equivalent that keeps
// ASCII behavior byte-identical while broadening \d and \s to match Python's Unicode semantics.
//
//   - `\d`  -> `\p{Nd}` (Unicode decimal digits; superset of ASCII [0-9], identical on ASCII).
//   - `\s`  -> a Unicode-whitespace class, but ONLY when it appears outside a `[...]` character
//     class (rewriting `\s` inside `[\s\S]` to a bracket class would nest brackets and break
//     RE2; `[\s\S]` already means "any char" so it needs no change).
//
// Escaped sequences (`\\`) and the class-context tracking ensure we never disturb `\\d`, `\\s`,
// or a `]`-terminated class. Anything we can't reconcile (Unicode `\b`, lookahead) is left as-is.
func pythonizeMaskRegex(pat string) string {
	var b []byte
	inClass := false
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		if c == '\\' && i+1 < len(pat) {
			next := pat[i+1]
			switch {
			case next == 'd':
				b = append(b, `\p{Nd}`...)
				i++
				continue
			case next == 's' && !inClass:
				b = append(b, '[')
				b = append(b, re2UnicodeWhitespaceClass...)
				b = append(b, ']')
				i++
				continue
			default:
				// Copy the escape pair verbatim (handles \\, \s-in-class, \b, \S, etc.).
				b = append(b, c, next)
				i++
				continue
			}
		}
		switch c {
		case '[':
			inClass = true
		case ']':
			inClass = false
		}
		b = append(b, c)
	}
	return string(b)
}

// compileMaskPattern compiles a sensitive pattern for the redaction engine, applying the
// Python-re reconciliation and the DOTALL flag (Python re.sub(..., flags=re.DOTALL)). Returns
// nil when the (reconciled) pattern is not RE2-compilable — e.g. a custom pattern using
// lookahead/possessive quantifiers that Python accepts but RE2 rejects (documented limitation).
func compileMaskPattern(pat string) *regexp.Regexp {
	re, err := regexp.Compile("(?s)" + pythonizeMaskRegex(pat))
	if err != nil {
		return nil
	}
	return re
}
