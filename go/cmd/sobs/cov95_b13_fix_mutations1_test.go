package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store/storetest"
)

// ---- bstrOr ------------------------------------------------------------------------------------

func TestBstrOr(t *testing.T) {
	cases := []struct {
		name string
		m    map[string]any
		key  string
		def  string
		want string
	}{
		{"missing key uses default", map[string]any{}, "x", "fallback", "fallback"},
		{"nil value uses default", map[string]any{"x": nil}, "x", "fallback", "fallback"},
		{"empty string uses default", map[string]any{"x": ""}, "x", "fallback", "fallback"},
		{"non-string value uses default", map[string]any{"x": 5}, "x", "fallback", "fallback"},
		{"present string is trimmed and used", map[string]any{"x": "  hi  "}, "x", "fallback", "hi"},
		{"default itself is trimmed", map[string]any{}, "x", "  fallback  ", "fallback"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bstrOr(c.m, c.key, c.def); got != c.want {
				t.Errorf("bstrOr(%v,%q,%q) = %q, want %q", c.m, c.key, c.def, got, c.want)
			}
		})
	}
}

// ---- refineChartSpecString ----------------------------------------------------------------------

func TestRefineChartSpecString(t *testing.T) {
	t.Run("nil is falsy", func(t *testing.T) {
		s, truthy := refineChartSpecString(nil)
		if s != "" || truthy {
			t.Errorf("got (%q,%v), want (\"\",false)", s, truthy)
		}
	})

	t.Run("empty string is falsy", func(t *testing.T) {
		s, truthy := refineChartSpecString("")
		if s != "" || truthy {
			t.Errorf("got (%q,%v), want (\"\",false)", s, truthy)
		}
	})

	t.Run("nonempty string passes through verbatim and truthy", func(t *testing.T) {
		s, truthy := refineChartSpecString(`{"a":1}`)
		if s != `{"a":1}` || !truthy {
			t.Errorf("got (%q,%v)", s, truthy)
		}
	})

	t.Run("empty map is falsy", func(t *testing.T) {
		s, truthy := refineChartSpecString(map[string]any{})
		if s != "" || truthy {
			t.Errorf("got (%q,%v)", s, truthy)
		}
	})

	t.Run("nonempty map serializes to JSON and is truthy", func(t *testing.T) {
		s, truthy := refineChartSpecString(map[string]any{"template_id": "gauge_kpi"})
		if !truthy || !strings.Contains(s, "gauge_kpi") {
			t.Errorf("got (%q,%v)", s, truthy)
		}
	})

	t.Run("empty slice is falsy", func(t *testing.T) {
		s, truthy := refineChartSpecString([]any{})
		if s != "" || truthy {
			t.Errorf("got (%q,%v)", s, truthy)
		}
	})

	t.Run("nonempty slice serializes and is truthy", func(t *testing.T) {
		s, truthy := refineChartSpecString([]any{1, 2, 3})
		if !truthy || s == "" {
			t.Errorf("got (%q,%v)", s, truthy)
		}
	})

	t.Run("bool true is truthy with empty string", func(t *testing.T) {
		s, truthy := refineChartSpecString(true)
		if s != "" || !truthy {
			t.Errorf("got (%q,%v), want (\"\",true)", s, truthy)
		}
	})

	t.Run("bool false is falsy", func(t *testing.T) {
		s, truthy := refineChartSpecString(false)
		if s != "" || truthy {
			t.Errorf("got (%q,%v), want (\"\",false)", s, truthy)
		}
	})

	t.Run("zero float64 is falsy", func(t *testing.T) {
		s, truthy := refineChartSpecString(0.0)
		if s != "" || truthy {
			t.Errorf("got (%q,%v)", s, truthy)
		}
	})

	t.Run("nonzero float64 is truthy with empty string", func(t *testing.T) {
		s, truthy := refineChartSpecString(3.5)
		if s != "" || !truthy {
			t.Errorf("got (%q,%v)", s, truthy)
		}
	})

	t.Run("default branch (unhandled type) is truthy", func(t *testing.T) {
		s, truthy := refineChartSpecString(42) // int, not float64/string/map/slice/bool
		if s != "" || !truthy {
			t.Errorf("got (%q,%v), want (\"\",true)", s, truthy)
		}
	})
}

// (toJSONObject already has a dedicated test in tail_spec_ts_helpers_test.go.)

// ---- vannaRefineChartSpecTL ----------------------------------------------------------------------

func TestVannaRefineChartSpecTL_MissingEndpointOrModel(t *testing.T) {
	s := &server{}
	_, errMsg := s.vannaRefineChartSpecTL("", "", "{}", "make it a bar chart", nil, nil, "off")
	if errMsg != "AI endpoint not configured." {
		t.Errorf("errMsg = %q", errMsg)
	}
	_, errMsg2 := s.vannaRefineChartSpecTL("http://x", "", "{}", "instr", nil, nil, "off")
	if errMsg2 != "AI endpoint not configured." {
		t.Errorf("errMsg2 = %q", errMsg2)
	}
}

func TestVannaRefineChartSpecTL_InvalidCurrentSpecJSON(t *testing.T) {
	s := &server{}
	_, errMsg := s.vannaRefineChartSpecTL("http://x", "model", "not json", "instr", nil, nil, "off")
	if !strings.HasPrefix(errMsg, "Current chart spec is invalid JSON:") {
		t.Errorf("errMsg = %q", errMsg)
	}
}

// ---- dmBackupEnabled / listDmBackups / validateDmBackupName / requireDmSafeValue -----------------

func TestDmBackupEnabled(t *testing.T) {
	t.Run("exactly 1 is enabled", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"data_management.backup_enabled": "1"})}
		if !s.dmBackupEnabled() {
			t.Error("want enabled for '1'")
		}
	})
	t.Run("truthy-but-not-1 values are NOT enabled (strict check)", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"data_management.backup_enabled": "true"})}
		if s.dmBackupEnabled() {
			t.Error("want disabled for 'true' (only exact '1' counts)")
		}
	})
	t.Run("unset is disabled", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{})}
		if s.dmBackupEnabled() {
			t.Error("want disabled when unset")
		}
	})
}

// (validateDmBackupName and requireDmSafeValue already have dedicated tests in
// safeguard_dm_validate_test.go.)
