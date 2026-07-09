package main

import "testing"

// Pure work-item helpers. Oracles: Python int() strict parsing (atoiStrict), cInt + default
// (cIntDef), _safe_json_loads(value, []) (safeJSONLoadsList), and _work_item_to_utc_iso —
// datetime.fromisoformat then .isoformat(timespec="milliseconds") with +00:00 -> Z.

func TestAtoiStrict(t *testing.T) {
	ok := []struct {
		in   string
		want int
	}{
		{"42", 42},
		{"+5", 5},
		{"-3", -3},
		{"  7  ", 7},
		{"0", 0},
	}
	for _, c := range ok {
		if n, valid := atoiStrict(c.in); !valid || n != c.want {
			t.Errorf("atoiStrict(%q) = (%d, %v), want (%d, true)", c.in, n, valid, c.want)
		}
	}
	bad := []string{"", "  ", "1.5", "abc", "12x", "-", "+", "1 2", "0x10"}
	for _, c := range bad {
		if n, valid := atoiStrict(c); valid {
			t.Errorf("atoiStrict(%q) = (%d, true), want (_, false)", c, n)
		}
	}
}

func TestCIntDef(t *testing.T) {
	if got := cIntDef(map[string]any{"k": float64(5)}, "k", 99); got != 5 {
		t.Errorf("nonzero float: got %d, want 5", got)
	}
	if got := cIntDef(map[string]any{"k": "7"}, "k", 99); got != 7 {
		t.Errorf("numeric string: got %d, want 7", got)
	}
	// cInt==0 (explicit zero, missing key, or unparseable) falls back to the default.
	if got := cIntDef(map[string]any{"k": float64(0)}, "k", 99); got != 99 {
		t.Errorf("zero value: got %d, want 99 (default)", got)
	}
	if got := cIntDef(map[string]any{}, "k", 99); got != 99 {
		t.Errorf("missing key: got %d, want 99 (default)", got)
	}
	if got := cIntDef(map[string]any{"k": "abc"}, "k", 99); got != 99 {
		t.Errorf("unparseable: got %d, want 99 (default)", got)
	}
}

func TestSafeJSONLoadsList(t *testing.T) {
	if got := safeJSONLoadsList(`[1, 2, 3]`); len(got) != 3 {
		t.Errorf("array: len = %d, want 3", len(got))
	}
	for _, raw := range []string{"", "   ", "{}", `{"a":1}`, "not json", "null", "42"} {
		if got := safeJSONLoadsList(raw); len(got) != 0 {
			t.Errorf("safeJSONLoadsList(%q) len = %d, want 0 (empty list)", raw, len(got))
		}
	}
}

func TestWorkItemToUTCISO(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"2026-03-29 12:00:00", "2026-03-29T12:00:00.000Z"},
		{"2026-03-29T12:00:00Z", "2026-03-29T12:00:00.000Z"},
		{"not a date", "not a date"}, // unparseable -> original raw
	}
	for _, c := range cases {
		if got := workItemToUTCISO(c.in); got != c.want {
			t.Errorf("workItemToUTCISO(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
