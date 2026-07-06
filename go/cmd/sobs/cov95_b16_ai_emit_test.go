package main

import "testing"

// cov95_b16_ai_emit_test.go — batch 16 targeted coverage for cmd/sobs/ai_emit.go's severityNumber
// (every switch branch, incl. the WARN/WARNING alias and CRITICAL/FATAL alias) and upper (mixed
// case, already-upper, digits/symbols passthrough, empty string).

func TestSeverityNumberAllBranches(t *testing.T) {
	cases := []struct {
		level string
		want  int
	}{
		{"TRACE", 1},
		{"trace", 1}, // lower-case input still matches via upper()
		{"DEBUG", 5},
		{"WARN", 13},
		{"WARNING", 13},
		{"warning", 13},
		{"ERROR", 17},
		{"error", 17},
		{"CRITICAL", 21},
		{"FATAL", 21},
		{"fatal", 21},
		{"INFO", 9},
		{"METRIC", 9},
		{"", 9},
		{"unknown-level", 9},
	}
	for _, c := range cases {
		t.Run(c.level, func(t *testing.T) {
			if got := severityNumber(c.level); got != c.want {
				t.Errorf("severityNumber(%q) = %d, want %d", c.level, got, c.want)
			}
		})
	}
}

func TestUpperVariants(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"already-upper", "ALREADY-UPPER"},
		{"MixedCase123", "MIXEDCASE123"},
		{"abc", "ABC"},
		{"123-!@#", "123-!@#"}, // non-letters pass through unchanged
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := upper(c.in); got != c.want {
				t.Errorf("upper(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
