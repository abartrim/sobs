package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Coverage batch 14: cmd/sobs/output_mask.go — no dedicated test file existed yet.
// activeSensitiveKeys/activeMaskPatterns/redactScalar/maskStringForOutput/maskValueForOutput all
// had partial coverage only through indirect handler-level exercises; here they're driven directly
// with a fully controlled FakeDB so every branch (masking disabled/enabled, custom keys, chDateTime,
// non-string scalar json.dumps path) is reached.

func TestActiveSensitiveKeys(t *testing.T) {
	t.Run("defaults_only_when_no_custom_keys_setting", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		got := s.activeSensitiveKeys()
		if !got["password"] || !got["api_key"] {
			t.Errorf("expected default sensitive keys present, got %v", got)
		}
	})

	t.Run("includes_custom_keys_from_settings", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{
			"masking.custom_keys": `["My_Custom_Field"]`,
		})}
		got := s.activeSensitiveKeys()
		if !got["my_custom_field"] {
			t.Errorf("expected normalized custom key present, got %v", got)
		}
	})
}

func TestActiveMaskPatterns(t *testing.T) {
	t.Run("defaults_present", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		got := s.activeMaskPatterns()
		if len(got) == 0 {
			t.Fatal("expected default patterns to compile")
		}
	})

	t.Run("custom_pattern_appended", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{
			"masking.custom_patterns": `["CUSTOM-[0-9]+"]`,
		})}
		got := s.activeMaskPatterns()
		found := false
		for _, re := range got {
			if re.replaceAll("CUSTOM-123", "****") == "****" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected the custom pattern active and matching")
		}
	})
}

func TestRedactScalar(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	patterns := s.activeMaskPatterns()

	if got := redactScalar(nil, patterns); got != nil {
		t.Errorf("nil should pass through, got %v", got)
	}
	if got := redactScalar(true, patterns); got != true {
		t.Errorf("bool should pass through, got %v", got)
	}
	if got := redactScalar(json.Number("5"), patterns); got != json.Number("5") {
		t.Errorf("json.Number should pass through, got %v", got)
	}
	if got := redactScalar(42, patterns); got != 42 {
		t.Errorf("int should pass through, got %v", got)
	}
	if got := redactScalar(int64(42), patterns); got != int64(42) {
		t.Errorf("int64 should pass through, got %v", got)
	}
	if got := redactScalar(3.14, patterns); got != 3.14 {
		t.Errorf("float64 should pass through, got %v", got)
	}
	// chDateTime -> always MASK (an "unhandled type" in Python's redact).
	if got := redactScalar(chDateTime{s: "2026-01-01 00:00:00"}, patterns); got != maskMASK {
		t.Errorf("chDateTime should be masked, got %v", got)
	}
	// A genuinely unrecognized type also falls to the default -> MASK branch.
	if got := redactScalar([]int{1, 2}, patterns); got != maskMASK {
		t.Errorf("unrecognized type should be masked, got %v", got)
	}
	// A string containing an email address gets pattern-redacted.
	if got := redactScalar("contact me at test@example.com", patterns); got == "contact me at test@example.com" {
		t.Errorf("expected email pattern to redact, got unchanged: %v", got)
	}
}

func TestMaskPayloadForOutput_DisabledIsNoOp(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{"masking.output_enabled": "0"})}
	payload := jsonenc.NewObject().Set("password", "secret123")
	got := s.maskPayloadForOutput(payload, true)
	obj, ok := got.(*jsonenc.Object)
	if !ok {
		t.Fatalf("expected passthrough object, got %T", got)
	}
	if v, _ := obj.Get("password"); v != "secret123" {
		t.Errorf("expected unmasked passthrough when disabled, got %v", v)
	}
}

func TestMaskPayloadForOutput_EnabledMasksSensitiveKey(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}} // masking.output_enabled defaults to true (absent -> def)
	payload := jsonenc.NewObject().Set("password", "secret123").Set("note", "hello")
	got := s.maskPayloadForOutput(payload, true).(*jsonenc.Object)
	if v, _ := got.Get("password"); v != maskMASK {
		t.Errorf("password = %v, want masked", v)
	}
	if v, _ := got.Get("note"); v != "hello" {
		t.Errorf("note = %v, want unchanged", v)
	}
}

func TestMaskPayloadForOutput_SQLFieldHandling(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	payload := jsonenc.NewObject().Set("sql", "SELECT password FROM users")
	// maskSQLFields=false and the value is a string -> passed through unmasked (but still
	// subject to recursion for non-string / non-SQL-field values elsewhere).
	got := s.maskPayloadForOutput(payload, false).(*jsonenc.Object)
	if v, _ := got.Get("sql"); v != "SELECT password FROM users" {
		t.Errorf("sql field with maskSQLFields=false should pass through unmasked, got %v", v)
	}
	// maskSQLFields=true -> the sql field recurses into the normal scalar-redaction path (a plain
	// string with no sensitive-key or pattern match here stays unchanged content-wise, but it is
	// no longer given the special "leave alone" treatment).
	got2 := s.maskPayloadForOutput(payload, true).(*jsonenc.Object)
	if _, ok := got2.Get("sql"); !ok {
		t.Errorf("expected sql key still present when maskSQLFields=true")
	}
}

func TestMaskPayloadForOutput_ArrayRecursion(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	arr := []any{jsonenc.NewObject().Set("password", "x"), "plain"}
	got := s.maskPayloadForOutput(arr, true).([]any)
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	inner, _ := got[0].(*jsonenc.Object)
	if v, _ := inner.Get("password"); v != maskMASK {
		t.Errorf("expected nested object's password masked, got %v", v)
	}
}

func TestMaskValueForOutput(t *testing.T) {
	t.Run("disabled_passthrough", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"masking.output_enabled": "0"})}
		if got := s.maskValueForOutput("some value"); got != "some value" {
			t.Errorf("got %v", got)
		}
	})
	t.Run("enabled_masks", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		payload := jsonenc.NewObject().Set("token", "abc123")
		got := s.maskValueForOutput(payload).(*jsonenc.Object)
		if v, _ := got.Get("token"); v != maskMASK {
			t.Errorf("got %v", v)
		}
	})
}

func TestMaskStringForOutput(t *testing.T) {
	t.Run("disabled_nil_yields_empty", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"masking.output_enabled": "0"})}
		if got := s.maskStringForOutput(nil); got != "" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("disabled_non_nil_stringified", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"masking.output_enabled": "0"})}
		if got := s.maskStringForOutput(42); got != "42" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("enabled_nil_yields_empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.maskStringForOutput(nil); got != "" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("enabled_string_gets_pattern_redaction", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		got := s.maskStringForOutput("email me at a@b.com")
		if strings.Contains(got, "a@b.com") {
			t.Errorf("expected email redacted, got %q", got)
		}
	})
	t.Run("enabled_non_string_is_json_dumped_then_redacted", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		got := s.maskStringForOutput(42)
		if got != "42" {
			t.Errorf("got %q, want the json.dumps'd scalar", got)
		}
	})
	t.Run("enabled_bool_json_dumped", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		got := s.maskStringForOutput(true)
		if got != "true" {
			t.Errorf("got %q, want true (json.dumps bool)", got)
		}
	})
}

func TestWriteMaskedJSON(t *testing.T) {
	// isSQLOutputMaskingEnabled defaults to true when the setting is absent.
	s := &server{db: &storetest.FakeDB{}}
	if !s.isSQLOutputMaskingEnabled() {
		t.Error("expected SQL output masking enabled by default")
	}
	s2 := &server{db: storetest.SettingsDB(map[string]string{"masking.sql_output_enabled": "0"})}
	if s2.isSQLOutputMaskingEnabled() {
		t.Error("expected SQL output masking disabled when explicitly set to 0")
	}
}
