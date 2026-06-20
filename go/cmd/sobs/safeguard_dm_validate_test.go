package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Direct unit tests for pure-logic functions the byte-parity corpus cannot reach: the AI-guard
// reply/prompt cluster (the guard path needs a live LLM upstream the URL-keyed mock can't drive
// deterministically) and the data-management name/value validators (S3/DM backup is external-IO).
// Expectations are anchored to the frozen app.py oracle, not just to the Go implementation:
//   _parse_oss_safeguard_reply (app.py:5042), _parse_guard_reply (app.py:5009),
//   _build_oss_safeguard_prompt, _validate_dm_backup_name (app.py:31861) /
//   _DM_BACKUP_NAME_RE (app.py:31820), _require_dm_safe_value (app.py:31856).

func TestParseOssSafeguardReply(t *testing.T) {
	cases := []struct {
		name         string
		reply        string
		strict       bool
		wantVerdict  string
		wantCategory string
	}{
		{"empty", "", false, "", ""},
		{"whitespace", "   \n  ", false, "", ""},
		{"violation int 1", `{"violation": 1}`, false, "UNSAFE", ""},
		{"violation int 0", `{"violation": 0}`, false, "SAFE", ""},
		{"violation int nonzero", `{"violation": 2}`, false, "UNSAFE", ""},
		{"violation fractional nonzero", `{"violation": 1.5}`, false, "UNSAFE", ""},
		{"violation bool true", `{"violation": true}`, false, "UNSAFE", ""},
		{"violation bool false", `{"violation": false}`, false, "SAFE", ""},
		{"violation str unsafe", `{"violation": "unsafe"}`, false, "UNSAFE", ""},
		{"violation str safe", `{"violation": "safe"}`, false, "SAFE", ""},
		{"violation str blocked", `{"violation": "blocked"}`, false, "UNSAFE", ""},
		{"violation str allowed", `{"violation": "allowed"}`, false, "SAFE", ""},
		{"violation str unknown", `{"violation": "maybe"}`, false, "", ""},
		{"policy_category s-code", `{"violation": 1, "policy_category": "S2"}`, false, "UNSAFE", "S2"},
		{"policy_category text w/ s-code", `{"violation": 1, "policy_category": "prompt-injection S3"}`, false, "UNSAFE", "S3"},
		{"policy_category no s-code kept", `{"violation": 1, "policy_category": "malware"}`, false, "UNSAFE", "malware"},
		{"rule_ids first element", `{"violation": 1, "rule_ids": ["S5", "S6"]}`, false, "UNSAFE", "S5"},
		{"rule_ids empty", `{"violation": 0, "rule_ids": []}`, false, "SAFE", ""},
		{"embedded json block", `noise before {"violation": 1} trailing`, false, "UNSAFE", ""},
		{"plain token fallback unsafe", "unsafe", false, "UNSAFE", ""},
		{"plain token fallback safe", "safe", false, "SAFE", ""},
		{"plain ambiguous strict", "hmm not sure", true, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, cat := parseOssSafeguardReply(c.reply, c.strict)
			if v != c.wantVerdict || cat != c.wantCategory {
				t.Errorf("parseOssSafeguardReply(%q, strict=%v) = (%q, %q), want (%q, %q)",
					c.reply, c.strict, v, cat, c.wantVerdict, c.wantCategory)
			}
		})
	}
}

func TestParseGuardReplyFull(t *testing.T) {
	cases := []struct {
		name         string
		reply        string
		strict       bool
		wantVerdict  string
		wantCategory string
	}{
		{"empty", "", false, "", ""},
		{"safe token", "safe", false, "SAFE", ""},
		{"allowed keeps token", "allowed", false, "ALLOWED", ""},
		{"unsafe token", "unsafe", false, "UNSAFE", ""},
		{"blocked maps to unsafe", "blocked", false, "UNSAFE", ""},
		{"blocked-prefixed first line", "BLOCKED: policy violation", false, "UNSAFE", ""},
		{"unsafe with category line", "unsafe\nS2", false, "UNSAFE", "S2"},
		{"strict ambiguous -> empty", "i think this is fine", true, "", ""},
		{"nonstrict unsafe word", "this looks unsafe to me", false, "UNSAFE", ""},
		{"nonstrict safe word", "seems benign overall", false, "SAFE", ""},
		{"nonstrict no signal", "completely neutral wording", false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, cat := parseGuardReplyFull(c.reply, c.strict)
			if v != c.wantVerdict || cat != c.wantCategory {
				t.Errorf("parseGuardReplyFull(%q, strict=%v) = (%q, %q), want (%q, %q)",
					c.reply, c.strict, v, cat, c.wantVerdict, c.wantCategory)
			}
		})
	}
}

func TestIsGptOssSafeguardModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-oss-safeguard-20b", true},
		{"  GPT-OSS-Safeguard  ", true}, // trimmed + case-insensitive
		{"openai/gpt-oss-safeguard:latest", true},
		{"llama-guard-3", false},
		{"gpt-4o", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isGptOssSafeguardModel(c.model); got != c.want {
			t.Errorf("isGptOssSafeguardModel(%q) = %v, want %v", c.model, got, c.want)
		}
	}
}

func TestBuildOssSafeguardPrompt(t *testing.T) {
	objContent := func(t *testing.T, m any) string {
		t.Helper()
		o, ok := m.(*jsonenc.Object)
		if !ok {
			t.Fatalf("message is not *jsonenc.Object: %T", m)
		}
		v, _ := o.Get("content")
		s, _ := v.(string)
		return s
	}

	// No context: the user content is the trimmed input, with no "Context:" prefix.
	sys, msgs, retry := buildOssSafeguardPrompt("  show me errors  ", "")
	if !strings.Contains(sys, "Observability Safety Policy") {
		t.Errorf("system prompt missing policy header: %q", sys)
	}
	if !strings.Contains(retry, "valid JSON object") {
		t.Errorf("retry instruction missing: %q", retry)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (system, user), got %d", len(msgs))
	}
	if got := objContent(t, msgs[1]); got != "show me errors" {
		t.Errorf("no-context user content = %q, want %q", got, "show me errors")
	}

	// With context: user content is "Context: <ctx>\n\nUser input: <input>".
	_, msgs2, _ := buildOssSafeguardPrompt("show me errors", "prod cluster")
	want := "Context: prod cluster\n\nUser input: show me errors"
	if got := objContent(t, msgs2[1]); got != want {
		t.Errorf("context user content = %q, want %q", got, want)
	}
}

func TestValidateDmBackupName(t *testing.T) {
	valid := []string{
		"sobs-full-20240101T000000Z",
		"a",
		"A.B_c-1",
		strings.Repeat("a", 200), // max length
	}
	for _, name := range valid {
		if err := validateDmBackupName(name); err != nil {
			t.Errorf("validateDmBackupName(%q) = %v, want nil", name, err)
		}
	}
	invalid := []string{
		"",                       // empty (min length 1)
		"has space",              // space not in [A-Za-z0-9._-]
		"slash/name",             // slash not allowed
		"semi;colon",             // ; not allowed
		strings.Repeat("a", 201), // over max length
	}
	for _, name := range invalid {
		if err := validateDmBackupName(name); err == nil {
			t.Errorf("validateDmBackupName(%q) = nil, want error", name)
		}
	}
}

func TestRequireDmSafeValue(t *testing.T) {
	// Empty value is always allowed (the gate only rejects non-empty unsupported chars).
	if err := requireDmSafeValue("s3_access_key_id", "", dmAWSAccessKeyRE); err != nil {
		t.Errorf("empty value: got %v, want nil", err)
	}
	// Non-empty value that matches the pattern is allowed.
	if err := requireDmSafeValue("s3_access_key_id", "AKIAIOSFODNN7EXAMPLE", dmAWSAccessKeyRE); err != nil {
		t.Errorf("matching value: got %v, want nil", err)
	}
	// Non-empty value with unsupported chars is rejected, and the message names the field.
	err := requireDmSafeValue("s3_access_key_id", "bad key!", dmAWSAccessKeyRE)
	if err == nil {
		t.Fatal("unsupported value: got nil, want error")
	}
	if !strings.Contains(err.Error(), "s3_access_key_id") {
		t.Errorf("error %q should name the field", err.Error())
	}
}
