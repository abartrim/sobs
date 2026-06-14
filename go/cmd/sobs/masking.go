package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// defaultSensitiveKeys mirrors masking.DEFAULT_SENSITIVE_KEYS.
var defaultSensitiveKeys = map[string]bool{
	"password": true, "passwd": true, "pwd": true, "secret": true, "client_secret": true,
	"api_key": true, "api_secret": true, "apikey": true, "token": true, "access_token": true,
	"refresh_token": true, "id_token": true, "auth_token": true, "bearer_token": true,
	"authorization": true, "x-authorization": true, "x-api-key": true, "private_key": true,
	"private-key": true, "credit_card": true, "card_number": true, "cvv": true, "cvc": true,
	"ssn": true, "social_security_number": true, "s3_secret_access_key": true,
	"backup_encryption_password": true, "smtp_password": true,
}

// defaultSensitivePatterns mirrors masking.DEFAULT_SENSITIVE_PATTERNS (used for the
// effective-patterns "already active" membership check).
var defaultSensitivePatterns = []string{
	`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`,
	`\beyJ[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]*\b`,
	`(?i)bearer\s+[A-Za-z0-9\-_.~+/]+=*`,
	`\bAKIA[0-9A-Z]{16}\b`,
	`\b\d{3}-\d{2}-\d{4}\b`,
	`\b4[0-9]{12}(?:[0-9]{3})?\b`,
	`\b5[1-5][0-9]{14}\b`,
	`\b3[47][0-9]{13}\b`,
	`\b6(?:011|5[0-9]{2})[0-9]{12}\b`,
	`-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----[\s\S]+?-----END (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----`,
	`(?i)(?:password|passwd|pwd|secret|api[_\-]?key|auth[_\-]?token|access[_\-]?token)\s*[=:]\s*['"]?[A-Za-z0-9\-_.~+/!@#$%^&*]{6,}['"]?`,
	`(?i)(?:Authorization|X-Api-Key|X-Auth-Token)\s*:\s*[^\r\n]+`,
}

// normalizeSensitiveKey mirrors masking.normalize_sensitive_key.
func normalizeSensitiveKey(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// loadMaskingCustomKeys mirrors _load_masking_custom_keys (sorted unique normalized).
func (s *server) loadMaskingCustomKeys() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range s.loadJSONStringListSetting("masking.custom_keys") {
		k := normalizeSensitiveKey(v)
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (s *server) effectiveKeyActive(key string) bool {
	if defaultSensitiveKeys[key] {
		return true
	}
	return containsStr(s.loadMaskingCustomKeys(), key)
}

// saveMaskingCustomKeys mirrors _save_masking_custom_keys + _save_json_string_list_setting
// (empty -> tombstone via setAppSetting "").
func (s *server) saveMaskingCustomKeys(keys []string) {
	seen := map[string]bool{}
	norm := []string{}
	for _, v := range keys {
		k := normalizeSensitiveKey(v)
		if k != "" && !seen[k] {
			seen[k] = true
			norm = append(norm, k)
		}
	}
	sort.Strings(norm)
	if len(norm) == 0 {
		_ = s.setAppSetting("masking.custom_keys", "")
		return
	}
	b, _ := json.Marshal(norm)
	_ = s.setAppSetting("masking.custom_keys", string(b))
}

// loadMaskingCustomPatterns mirrors _load_masking_custom_patterns (validated, order-preserving dedupe).
func (s *server) loadMaskingCustomPatterns() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range s.loadJSONStringListSetting("masking.custom_patterns") {
		p, err := validateCustomMaskingPattern(v)
		if err != nil {
			continue
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func (s *server) effectivePatternActive(pattern string) bool {
	if containsStr(defaultSensitivePatterns, pattern) {
		return true
	}
	return containsStr(s.loadMaskingCustomPatterns(), pattern)
}

func (s *server) saveMaskingCustomPatterns(patterns []string) {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range patterns {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		_ = s.setAppSetting("masking.custom_patterns", "")
		return
	}
	b, _ := json.Marshal(out)
	_ = s.setAppSetting("masking.custom_patterns", string(b))
}

const maxCustomMaskingPatternLength = 512

var (
	redosNestedQuantifierRe = regexp.MustCompile(`\((?:[^()\\]|\\.)*[+*](?:[^()\\]|\\.)*\)\s*(?:[+*]|\{\d+,?\d*\})`)
	redosAmbiguousAlternRe  = regexp.MustCompile(`\((?:[^()\\]|\\.)*\|(?:[^()\\]|\\.)*\)\s*(?:[+*]|\{\d+,?\d*\})`)
	maskingInlineFlagsRe    = regexp.MustCompile(`^\(\?([a-zA-Z]+)\)`)
	maskingNamedGroupRe     = regexp.MustCompile(`\(\?P<[^>]+>`)
)

// validateCustomMaskingPattern mirrors app.py _validate_custom_masking_pattern_for_storage:
// returns the normalized (trimmed) pattern string, or an error whose message matches Python's.
func validateCustomMaskingPattern(raw string) (string, error) {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		return "", fmt.Errorf("Pattern is required")
	}
	if _, err := regexp.Compile(normalized); err != nil {
		return "", err
	}
	if len(normalized) > maxCustomMaskingPatternLength {
		return "", fmt.Errorf("Safety check failed: pattern is too long (max %d chars)", maxCustomMaskingPatternLength)
	}
	if strings.Contains(normalized, `\1`) || strings.Contains(normalized, `\2`) || strings.Contains(normalized, `\3`) {
		return "", fmt.Errorf("Safety check failed: backreferences are not allowed in custom masking patterns")
	}
	if redosNestedQuantifierRe.MatchString(normalized) {
		return "", fmt.Errorf("Safety check failed: pattern contains nested quantifiers and may cause catastrophic backtracking")
	}
	if redosAmbiguousAlternRe.MatchString(normalized) {
		return "", fmt.Errorf("Safety check failed: pattern contains quantified alternation and may cause catastrophic backtracking")
	}
	jsPattern := normalized
	if m := maskingInlineFlagsRe.FindString(jsPattern); m != "" {
		jsPattern = jsPattern[len(m):]
	}
	jsPattern = strings.ReplaceAll(jsPattern, `\A`, "^")
	jsPattern = strings.ReplaceAll(jsPattern, `\Z`, "$")
	jsPattern = maskingNamedGroupRe.ReplaceAllString(jsPattern, "(")
	if strings.Contains(jsPattern, "(?<=") || strings.Contains(jsPattern, "(?<!") {
		return "", fmt.Errorf("JavaScript compatibility check failed: lookbehind is not supported for screenshot DOM masking helper")
	}
	return normalized, nil
}
