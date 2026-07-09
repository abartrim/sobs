package main

import (
	"math"
	"testing"
	"time"
)

// ---- isFalsy --------------------------------------------------------------------------------

func TestIsFalsyMH(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{"nil", nil, true},
		{"bool false", false, true},
		{"bool true", true, false},
		{"empty string", "", true},
		{"nonempty string", "x", false},
		{"zero float", 0.0, true},
		{"nonzero float", 1.5, false},
		{"empty map", map[string]any{}, true},
		{"nonempty map", map[string]any{"a": 1}, false},
		{"empty slice", []any{}, true},
		{"nonempty slice", []any{1}, false},
		{"unhandled type default false", 42, false}, // int (not float64) falls to default false
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isFalsy(c.v); got != c.want {
				t.Errorf("isFalsy(%v) = %v, want %v", c.v, got, c.want)
			}
		})
	}
}

// ---- clampInt --------------------------------------------------------------------------------

func TestClampInt(t *testing.T) {
	cases := []struct{ n, lo, hi, want int }{
		{5, 0, 10, 5},
		{-5, 0, 10, 0},
		{50, 0, 10, 10},
		{0, 0, 10, 0},
		{10, 0, 10, 10},
	}
	for _, c := range cases {
		if got := clampInt(c.n, c.lo, c.hi); got != c.want {
			t.Errorf("clampInt(%d,%d,%d) = %d, want %d", c.n, c.lo, c.hi, got, c.want)
		}
	}
}

// ---- pyDateTimeStr ---------------------------------------------------------------------------

func TestPyDateTimeStr(t *testing.T) {
	cases := []struct{ in, want string }{
		{"2024-01-02 03:00:00.000", "2024-01-02 03:00:00"},            // all-zero fraction dropped
		{"2024-01-02 03:00:00.120000", "2024-01-02 03:00:00.120000"},  // already 6-digit
		{"2024-01-02 03:00:00.12", "2024-01-02 03:00:00.120000"},      // padded to 6 digits
		{"2024-01-02 03:00:00.1234567", "2024-01-02 03:00:00.123456"}, // truncated to 6
		{"2024-01-02T03:00:00", "2024-01-02T03:00:00"},                // ISO form untouched (no match)
		{"", ""},
		{"not-a-timestamp", "not-a-timestamp"},
	}
	for _, c := range cases {
		if got := pyDateTimeStr(c.in); got != c.want {
			t.Errorf("pyDateTimeStr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- normalizeCHTimestamp ---------------------------------------------------------------------

func TestNormalizeCHTimestamp(t *testing.T) {
	t.Run("time.Time value formats directly", func(t *testing.T) {
		tv := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
		got := normalizeCHTimestamp(tv)
		want := "2024-03-04 05:06:07.000000"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("falsy value falls through to now()", func(t *testing.T) {
		for _, v := range []any{nil, false, 0.0, ""} {
			got := normalizeCHTimestamp(v)
			if got == "" {
				t.Errorf("normalizeCHTimestamp(%v) = empty, want now() formatted", v)
			}
		}
	})

	t.Run("ISO string with Z parses", func(t *testing.T) {
		got := normalizeCHTimestamp("2024-01-02T03:04:05.123456Z")
		want := "2024-01-02 03:04:05.123456"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("ISO string with offset parses", func(t *testing.T) {
		got := normalizeCHTimestamp("2024-01-02T03:04:05-07:00")
		want := "2024-01-02 10:04:05.000000"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("space-separated string parses", func(t *testing.T) {
		got := normalizeCHTimestamp("2024-01-02 03:04:05")
		want := "2024-01-02 03:04:05.000000"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("unparseable string falls to last resort T->space replace", func(t *testing.T) {
		got := normalizeCHTimestamp("garbageTvalue")
		if got != "garbage value" {
			t.Errorf("got %q, want %q", got, "garbage value")
		}
	})
}

// ---- bNum ------------------------------------------------------------------------------------

func TestBNum(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		key  string
		want float64
	}{
		{"missing key", map[string]any{}, "x", 0},
		{"float64 value", map[string]any{"x": 3.5}, "x", 3.5},
		{"string numeric", map[string]any{"x": "  42  "}, "x", 42},
		{"string non-numeric", map[string]any{"x": "abc"}, "x", 0},
		{"bool true", map[string]any{"x": true}, "x", 1},
		{"bool false", map[string]any{"x": false}, "x", 0},
		{"unsupported type", map[string]any{"x": []int{1}}, "x", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bNum(c.m, c.key); got != c.want {
				t.Errorf("bNum(%v,%q) = %v, want %v", c.m, c.key, got, c.want)
			}
		})
	}
}

// ---- toStr -----------------------------------------------------------------------------------

func TestToStrMH(t *testing.T) {
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"nil", nil, ""},
		{"string", "hi", "hi"},
		{"bool true", true, "True"},
		{"bool false", false, "False"},
		{"nan", math.NaN(), "nan"},
		{"pos inf", math.Inf(1), "inf"},
		{"neg inf", math.Inf(-1), "-inf"},
		{"float with trailing zero", 5.0, "5.0"},
		{"other type via Sprintf", 7, "7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toStr(c.v); got != c.want {
				t.Errorf("toStr(%v) = %q, want %q", c.v, got, c.want)
			}
		})
	}
}
