package main

import (
	"encoding/json"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// cov95_b15_fix_crypto_misc_test.go — batch 15 coverage for cmd/sobs/fix_crypto_misc.go:
//   pythonEquals1 (49)          60.0%
//   pyRepr (72)                 60.0%
//   isEmptyJSONContainer (94)   50.0%
//   compileMaskPattern (125)    75.0%

func TestPythonEquals1(t *testing.T) {
	cases := []struct {
		name    string
		v       any
		present bool
		want    bool
	}{
		{"absent", nil, false, false},
		{"bool true", true, true, true},
		{"bool false", false, true, false},
		// numEquals (the underlying numeric-equality helper) only recognizes json.Number values
		// (the parseJSONValue shape) — a native Go int/float64 is not what the JSON decoder ever
		// produces here, so pythonEquals1 correctly reports false for those, same as an
		// unrecognized type would.
		{"native int 1 (not json.Number) -> false", 1, true, false},
		{"native float64 1.0 (not json.Number) -> false", 1.0, true, false},
		{"json.Number 1", json.Number("1"), true, true},
		{"json.Number 1.0", json.Number("1.0"), true, true},
		{"json.Number 0", json.Number("0"), true, false},
		{"string not numeric", "1", true, false},
		{"present but nil value", nil, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pythonEquals1(c.v, c.present); got != c.want {
				t.Errorf("pythonEquals1(%#v, %v) = %v, want %v", c.v, c.present, got, c.want)
			}
		})
	}
}

func TestPyRepr(t *testing.T) {
	cases := []struct {
		name    string
		v       any
		present bool
		want    string
	}{
		{"absent -> None", nil, false, "None"},
		{"nil value present -> None", nil, true, "None"},
		{"plain string", "hello", true, "'hello'"},
		{"string with single quote escaped", "it's", true, `'it\'s'`},
		{"string with backslash escaped", `a\b`, true, `'a\\b'`},
		{"bool true", true, true, "True"},
		{"bool false", false, true, "False"},
		{"int", 42, true, "42"},
		{"float", 3.5, true, "3.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pyRepr(c.v, c.present); got != c.want {
				t.Errorf("pyRepr(%#v, %v) = %q, want %q", c.v, c.present, got, c.want)
			}
		})
	}
}

func TestIsEmptyJSONContainer(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"empty slice", []any{}, true},
		{"non-empty slice", []any{1}, false},
		{"empty object", jsonenc.NewObject(), true},
		{"non-empty object", jsonenc.NewObject().Set("a", 1), false},
		{"string is not a container", "", false},
		{"int is not a container", 0, false},
		{"nil is not a container", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isEmptyJSONContainer(c.v); got != c.want {
				t.Errorf("isEmptyJSONContainer(%#v) = %v, want %v", c.v, got, c.want)
			}
		})
	}
}

func TestCompileMaskPattern(t *testing.T) {
	re := compileMaskPattern(`\d+`)
	if re == nil {
		t.Fatal("expected a compiled pattern for a valid regex")
	}
	if !re.match("abc123") {
		t.Errorf("expected \\d+ to match abc123")
	}

	// An uncompilable pattern (unbalanced group) must yield nil, not panic.
	if bad := compileMaskPattern(`(unclosed`); bad != nil {
		t.Errorf("expected nil for an uncompilable pattern, got %v", bad)
	}
}
