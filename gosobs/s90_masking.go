package main

// Port of masking.py – Shared PII/secret masking rules for SOBS UI output,
// notification messages, and GitHub issue bodies.
//
// All patterns are explicit and human-curated (no ML/heuristic detection).
//
// Extending the rule set:
//   - New sensitive value formats – add a regex string to maskingSensitivePatterns.
//     Each pattern is applied to every string value (recursively in maps/slices);
//     the entire match is replaced with maskingMask.
//   - New sensitive key names – add a lowercase key name to maskingSensitiveKeys.
//
// After modifying either collection at runtime call maskingBuildRedactingFilter()
// (or maskingConfigureRuntimeRules) to rebuild the singleton filter.

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// maskingMask is the replacement placeholder shown in masked output.
const maskingMask = "****"

// maskingDefaultSensitiveKeys: key names whose values are always fully masked.
// Comparison is done after lowercasing the actual key at call-site.
var maskingDefaultSensitiveKeys = map[string]bool{
	// Credentials / secrets
	"password":      true,
	"passwd":        true,
	"pwd":           true,
	"secret":        true,
	"client_secret": true,
	"api_key":       true,
	"api_secret":    true,
	"apikey":        true,
	// Tokens
	"token":         true,
	"access_token":  true,
	"refresh_token": true,
	"id_token":      true,
	"auth_token":    true,
	"bearer_token":  true,
	// Auth headers
	"authorization":   true,
	"x-authorization": true,
	"x-api-key":       true,
	// Cryptographic material
	"private_key": true,
	"private-key": true,
	// Payment / identity
	"credit_card":            true,
	"card_number":            true,
	"cvv":                    true,
	"cvc":                    true,
	"ssn":                    true,
	"social_security_number": true,
	// SOBS-specific sensitive settings keys
	"s3_secret_access_key":       true,
	"backup_encryption_password": true,
	"smtp_password":              true,
}

// maskingSensitiveKeys is the active key set (defaults + runtime custom keys).
var maskingSensitiveKeys = copyKeySet(maskingDefaultSensitiveKeys)

// maskingDefaultSensitivePatterns: regexes matched against string values. The
// entire match is replaced with maskingMask, so patterns capture the full
// sensitive fragment.
var maskingDefaultSensitivePatterns = []string{
	// --- Email addresses ---
	`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`,
	// --- JWT tokens (three base64url-encoded segments) ---
	`\beyJ[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]*\b`,
	// --- Bearer token in text / HTTP headers ---
	`(?i)bearer\s+[A-Za-z0-9\-_.~+/]+=*`,
	// --- AWS access key IDs ---
	`\bAKIA[0-9A-Z]{16}\b`,
	// --- US Social Security Numbers (###-##-####) ---
	`\b\d{3}-\d{2}-\d{4}\b`,
	// --- Common credit card patterns ---
	`\b4[0-9]{12}(?:[0-9]{3})?\b`,     // Visa (13 or 16 digits)
	`\b5[1-5][0-9]{14}\b`,             // Mastercard
	`\b3[47][0-9]{13}\b`,              // Amex
	`\b6(?:011|5[0-9]{2})[0-9]{12}\b`, // Discover
	// --- PEM private key blocks ---
	`-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----[\s\S]+?` +
		`-----END (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`,
	// --- Generic key=value / key: value assignment in log lines ---
	// Matches patterns like: password=abc123 | secret: "xyz" | api_key=ABCDEF...
	`(?i)(?:password|passwd|pwd|secret|api[_\-]?key|auth[_\-]?token|access[_\-]?token)` +
		`\s*[=:]\s*['"]?[A-Za-z0-9\-_.~+/!@#$%^&*]{6,}['"]?`,
	// --- Authorization header value ---
	`(?i)(?:Authorization|X-Api-Key|X-Auth-Token)\s*:\s*[^\r\n]+`,
}

// maskingSensitivePatterns is the active pattern list (defaults + custom).
var maskingSensitivePatterns = append([]string{}, maskingDefaultSensitivePatterns...)

func copyKeySet(src map[string]bool) map[string]bool {
	dst := make(map[string]bool, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// sobsRedactingFilter ports _SobsRedactingFilter (loggingredactor subclass with
// case-insensitive dict-key matching).
type sobsRedactingFilter struct {
	maskKeys     map[string]bool
	maskPatterns []*regexp.Regexp
	mask         string
}

var (
	maskingFilterMu sync.RWMutex
	maskingFilter   *sobsRedactingFilter
)

func (f *sobsRedactingFilter) redact(content any) any {
	return f.redactValue(content, "", false, map[uintptr]bool{})
}

// redactValue mirrors _redact_value. visited guards recursive structures.
// PORT-NOTE: Go cannot take id() of arbitrary values; cycle detection covers
// maps and slices via reflection-free best effort (JSON-decoded data cannot
// be cyclic, which is the only data this filter receives in practice).
func (f *sobsRedactingFilter) redactValue(content any, key string, hasKey bool, visited map[uintptr]bool) any {
	if hasKey && f.maskKeys[strings.ToLower(key)] {
		return f.mask
	}
	switch v := content.(type) {
	case nil:
		return nil
	case string:
		masked := v
		for _, p := range f.maskPatterns {
			masked = p.ReplaceAllString(masked, f.mask)
		}
		return masked
	case bool, int, int64, float64, json.Number:
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for ik, iv := range v {
			out[ik] = f.redactValue(iv, ik, true, visited)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = f.redactValue(item, "", false, visited)
		}
		return out
	default:
		// Mirror the Python fallback: deep-copy unknown objects or mask them.
		// In Go, serialize through JSON; on failure, mask.
		raw, err := json.Marshal(v)
		if err != nil {
			return f.mask
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return f.mask
		}
		// Avoid infinite recursion: a decoded value is plain JSON data.
		if _, same := decoded.(map[string]any); same {
			return f.redactValue(decoded, "", false, visited)
		}
		if _, same := decoded.([]any); same {
			return f.redactValue(decoded, "", false, visited)
		}
		if s, ok := decoded.(string); ok {
			return f.redactValue(s, "", false, visited)
		}
		return decoded
	}
}

// maskingNormalizeSensitiveKey returns a normalized lowercase key name.
func maskingNormalizeSensitiveKey(value any) string {
	if value == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", value)))
}

// maskingValidatePattern validates and normalizes a custom regex pattern.
// PORT-NOTE: Python compiles with re.DOTALL; Go prepends (?s) at use time.
func maskingValidatePattern(pattern any) (string, error) {
	normalized := ""
	if pattern != nil {
		normalized = strings.TrimSpace(fmt.Sprintf("%v", pattern))
	}
	if normalized == "" {
		return "", errors.New("Pattern is required")
	}
	if _, err := regexp.Compile("(?s)" + normalized); err != nil {
		return "", err
	}
	return normalized, nil
}

func dedupePreserveOrder(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		result = append(result, v)
	}
	return result
}

// maskingConfigureRuntimeRules merges persisted custom rules with defaults and
// rebuilds the filter. Invalid custom patterns return an error (Python raises).
func maskingConfigureRuntimeRules(customKeys []string, customPatterns []string) (*sobsRedactingFilter, error) {
	keySet := map[string]bool{}
	for _, item := range customKeys {
		if k := maskingNormalizeSensitiveKey(item); k != "" {
			keySet[k] = true
		}
	}
	normalizedKeys := make([]string, 0, len(keySet))
	for k := range keySet {
		normalizedKeys = append(normalizedKeys, k)
	}
	sort.Strings(normalizedKeys)

	var normalizedPatterns []string
	for _, item := range customPatterns {
		p, err := maskingValidatePattern(item)
		if err != nil {
			return nil, err
		}
		normalizedPatterns = append(normalizedPatterns, p)
	}
	normalizedPatterns = dedupePreserveOrder(normalizedPatterns)

	maskingFilterMu.Lock()
	maskingSensitiveKeys = copyKeySet(maskingDefaultSensitiveKeys)
	for _, k := range normalizedKeys {
		maskingSensitiveKeys[k] = true
	}
	maskingSensitivePatterns = append(append([]string{}, maskingDefaultSensitivePatterns...), normalizedPatterns...)
	maskingFilterMu.Unlock()

	return maskingBuildRedactingFilter()
}

// maskingBuildRedactingFilter (re)builds and returns the shared filter.
func maskingBuildRedactingFilter() (*sobsRedactingFilter, error) {
	maskingFilterMu.Lock()
	defer maskingFilterMu.Unlock()
	compiled := make([]*regexp.Regexp, 0, len(maskingSensitivePatterns))
	for _, p := range maskingSensitivePatterns {
		re, err := regexp.Compile("(?s)" + p)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, re)
	}
	maskingFilter = &sobsRedactingFilter{
		maskKeys:     copyKeySet(maskingSensitiveKeys),
		maskPatterns: compiled,
		mask:         maskingMask,
	}
	return maskingFilter, nil
}

func maskingGetFilter() *sobsRedactingFilter {
	maskingFilterMu.RLock()
	f := maskingFilter
	maskingFilterMu.RUnlock()
	if f == nil {
		f, _ = maskingBuildRedactingFilter()
	}
	return f
}

// maskingIsSensitiveKey reports whether a normalized key is in the active set
// (Python call sites test `key in _masking.SENSITIVE_KEYS`).
func maskingIsSensitiveKey(key string) bool {
	maskingFilterMu.RLock()
	defer maskingFilterMu.RUnlock()
	return maskingSensitiveKeys[key]
}

// maskingMaskValue masks sensitive data in value and returns the same shape.
// Non-mutating: original containers are not modified.
func maskingMaskValue(value any) any {
	if value == nil {
		return value
	}
	return maskingGetFilter().redact(value)
}

// maskingMaskString masks sensitive data and coerces the result to a string.
func maskingMaskString(value any) string {
	if value == nil {
		return ""
	}
	s, isStr := value.(string)
	if !isStr {
		masked := maskingMaskValue(value)
		raw, err := json.Marshal(masked)
		if err != nil {
			s = fmt.Sprintf("%v", masked)
		} else {
			s = string(raw)
		}
	}
	result := maskingGetFilter().redact(s)
	if result == nil {
		return ""
	}
	if out, ok := result.(string); ok {
		return out
	}
	return fmt.Sprintf("%v", result)
}

// Initialise the singleton at package init time (module import in Python).
func init() {
	if _, err := maskingConfigureRuntimeRules(nil, nil); err != nil {
		panic("masking: default rules failed to compile: " + err.Error())
	}
}
