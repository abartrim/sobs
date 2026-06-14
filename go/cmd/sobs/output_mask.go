package main

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/sobs/sobs/internal/jsonenc"
)

// writeMaskedJSON mirrors _jsonify_with_optional_sql_output_mask: mask the payload (output
// masking on the scalars, SQL-field handling) then jsonify.
func (s *server) writeMaskedJSON(w http.ResponseWriter, status int, payload *jsonenc.Object) {
	writeJSON(w, status, s.maskPayloadForOutput(payload, s.isSQLOutputMaskingEnabled()))
}

// Output masking — a port of app.py _mask_payload_for_output_json + masking.py redact, used by
// the spec/* dashboard routes (_jsonify_with_optional_sql_output_mask).

const maskMASK = "****"

var sqlOutputMaskFields = map[string]bool{"sql": true, "query": true, "sample_sql": true, "override_sql": true}

func (s *server) isSQLOutputMaskingEnabled() bool {
	return s.appSettingBool("masking.sql_output_enabled", true)
}

// activeSensitiveKeys returns the effective masked-key set (defaults + custom, minus deactivations).
func (s *server) activeSensitiveKeys() map[string]bool {
	out := map[string]bool{}
	for k := range defaultSensitiveKeys {
		if s.effectiveKeyActive(k) {
			out[k] = true
		}
	}
	for _, k := range s.loadMaskingCustomKeys() {
		nk := normalizeSensitiveKey(k)
		if s.effectiveKeyActive(nk) {
			out[nk] = true
		}
	}
	return out
}

// activeMaskPatterns returns the effective SENSITIVE_PATTERNS compiled (DEFAULT first, then custom),
// each as a DOTALL regex (Python re.sub(..., flags=re.DOTALL)).
func (s *server) activeMaskPatterns() []*regexp.Regexp {
	out := []*regexp.Regexp{}
	add := func(p string) {
		if s.effectivePatternActive(p) {
			if re, err := regexp.Compile("(?s)" + p); err == nil {
				out = append(out, re)
			}
		}
	}
	for _, p := range defaultSensitivePatterns {
		add(p)
	}
	for _, p := range s.loadMaskingCustomPatterns() {
		add(p)
	}
	return out
}

// redactScalar mirrors masking.redact for scalar values: strings get every pattern applied;
// numbers/bools pass through; a chDateTime (an "unhandled type" in Python's redact) -> MASK.
func redactScalar(content any, patterns []*regexp.Regexp) any {
	switch x := content.(type) {
	case nil:
		return nil
	case string:
		out := x
		for _, re := range patterns {
			out = re.ReplaceAllString(out, maskMASK)
		}
		return out
	case bool, json.Number, int, int64, float64:
		return content
	case chDateTime:
		return maskMASK
	default:
		return maskMASK
	}
}

// maskPayloadForOutput mirrors _mask_payload_for_output_json: walk the response, mask sensitive
// keys / SQL fields / scalar values. A no-op when output masking is disabled.
func (s *server) maskPayloadForOutput(v any, maskSQLFields bool) any {
	if !s.appSettingBool("masking.output_enabled", true) {
		return v
	}
	return maskPayloadJSON(v, s.activeSensitiveKeys(), s.activeMaskPatterns(), maskSQLFields)
}

// maskValueForOutput mirrors app.py _mask_value_for_output: masking.mask_value when output
// masking is on (recursive key + pattern redaction, same-type), else the value unchanged.
func (s *server) maskValueForOutput(value any) any {
	if !s.appSettingBool("masking.output_enabled", true) {
		return value
	}
	return maskPayloadJSON(value, s.activeSensitiveKeys(), s.activeMaskPatterns(), true)
}

// maskStringForOutput mirrors app.py _mask_string_for_output → masking.mask_string: None→"",
// non-strings get recursive key masking + json.dumps then pattern redaction, strings get
// pattern redaction. Output masking off → str(value) (None→""). The handler only routes
// scalars here (dict/list go through maskValueForOutput), so the json.dumps branch sees
// numbers/bools only — no HTML-escaping divergence from Python's ensure_ascii=False dumps.
func (s *server) maskStringForOutput(value any) string {
	if !s.appSettingBool("masking.output_enabled", true) {
		if value == nil {
			return ""
		}
		return pyStrAny(value)
	}
	if value == nil {
		return ""
	}
	str, isStr := value.(string)
	if !isStr {
		mv := maskPayloadJSON(value, s.activeSensitiveKeys(), s.activeMaskPatterns(), true)
		if b, err := json.Marshal(mv); err == nil {
			str = string(b)
		} else {
			str = pyStrAny(mv)
		}
	}
	for _, re := range s.activeMaskPatterns() {
		str = re.ReplaceAllString(str, maskMASK)
	}
	return str
}

func maskPayloadJSON(v any, keys map[string]bool, patterns []*regexp.Regexp, maskSQL bool) any {
	switch x := v.(type) {
	case *jsonenc.Object:
		out := jsonenc.NewObject()
		for _, k := range x.Keys() {
			item, _ := x.Get(k)
			kn := normalizeSensitiveKey(k)
			if keys[kn] {
				out.Set(k, maskMASK)
				continue
			}
			if sqlOutputMaskFields[kn] && !maskSQL {
				if _, isStr := item.(string); isStr {
					out.Set(k, item)
					continue
				}
			}
			out.Set(k, maskPayloadJSON(item, keys, patterns, maskSQL))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, it := range x {
			out[i] = maskPayloadJSON(it, keys, patterns, maskSQL)
		}
		return out
	default:
		return redactScalar(v, patterns)
	}
}
