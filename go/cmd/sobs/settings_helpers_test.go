package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// appSettingBool / appSettingIntOrZero / kubernetesEnabled / queryPageEnabled gate several
// settings-driven feature flags. The corpus's fixture DB never sets these app settings, so only
// the "absent -> default" branch is corpus-reachable; the truthy/falsy/parse-error branches
// aren't. Oracle: app.py _is_truthy_setting / int(...) coercion / _kubernetes_enabled /
// _query_page_enabled.

func TestAppSettingBool(t *testing.T) {
	cases := []struct{ stored, want string }{
		{"1", "true"}, {"true", "true"}, {"YES", "true"}, {"on", "true"},
		{"0", "false"}, {"nah", "false"},
	}
	for _, tc := range cases {
		s := &server{db: storetest.SettingsDB(map[string]string{"dlp.enabled": tc.stored})}
		got := s.appSettingBool("dlp.enabled", false)
		want := tc.want == "true"
		if got != want {
			t.Fatalf("stored=%q: got %v, want %v", tc.stored, got, want)
		}
	}
	// Absent -> the caller's default, both ways.
	sAbsent := &server{db: storetest.SettingsDB(nil)}
	if got := sAbsent.appSettingBool("dlp.enabled", true); !got {
		t.Fatalf("absent default=true: got false")
	}
	if got := sAbsent.appSettingBool("dlp.enabled", false); got {
		t.Fatalf("absent default=false: got true")
	}
}

func TestAppSettingIntOrZero(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{"n": "42", "bad": "nope", "blank": "  "})}
	if got := s.appSettingIntOrZero("n"); got != 42 {
		t.Fatalf("got %d, want 42", got)
	}
	if got := s.appSettingIntOrZero("bad"); got != 0 {
		t.Fatalf("unparseable: got %d, want 0", got)
	}
	if got := s.appSettingIntOrZero("blank"); got != 0 {
		t.Fatalf("blank: got %d, want 0", got)
	}
	if got := s.appSettingIntOrZero("missing"); got != 0 {
		t.Fatalf("missing: got %d, want 0", got)
	}
}

func TestKubernetesEnabled(t *testing.T) {
	if (&server{db: storetest.SettingsDB(map[string]string{"kubernetes.enabled": "1"})}).kubernetesEnabled() != true {
		t.Fatal("want true for '1'")
	}
	if (&server{db: storetest.SettingsDB(map[string]string{"kubernetes.enabled": "true"})}).kubernetesEnabled() != false {
		t.Fatal("want false: only the literal '1' enables it, unlike appSettingBool's truthy set")
	}
	if (&server{db: storetest.SettingsDB(nil)}).kubernetesEnabled() != false {
		t.Fatal("want false when unset")
	}
}

func TestQueryPageEnabled(t *testing.T) {
	fake := &storetest.FakeDB{ExecuteFunc: func(_ string, params ...any) (*store.Result, error) {
		key, _ := params[0].(string)
		switch key {
		case "ai.endpoint_url":
			return storetest.Result([]string{"Value"}, []any{"https://llm.example"}), nil
		case "ai.model":
			return storetest.Result([]string{"Value"}, []any{"gpt-x"}), nil
		}
		return &store.Result{}, nil
	}}
	if !(&server{db: fake}).queryPageEnabled() {
		t.Fatal("want enabled when both endpoint and model are set")
	}

	fakeMissingModel := &storetest.FakeDB{ExecuteFunc: func(_ string, params ...any) (*store.Result, error) {
		if params[0] == "ai.endpoint_url" {
			return storetest.Result([]string{"Value"}, []any{"https://llm.example"}), nil
		}
		return &store.Result{}, nil
	}}
	if (&server{db: fakeMissingModel}).queryPageEnabled() {
		t.Fatal("want disabled when model is unset")
	}
}

func TestQueryIntClamp(t *testing.T) {
	mkReq := func(qs string) *http.Request { return httptest.NewRequest(http.MethodGet, "/x?"+qs, nil) }
	if got := queryIntClamp(mkReq(""), "n", 10, 1, 100); got != 10 {
		t.Fatalf("absent -> default: got %d", got)
	}
	if got := queryIntClamp(mkReq("n=abc"), "n", 10, 1, 100); got != 10 {
		t.Fatalf("unparseable -> default: got %d", got)
	}
	if got := queryIntClamp(mkReq("n=0"), "n", 10, 1, 100); got != 1 {
		t.Fatalf("below lo -> clamp to lo: got %d", got)
	}
	if got := queryIntClamp(mkReq("n=999"), "n", 10, 1, 100); got != 100 {
		t.Fatalf("above hi -> clamp to hi: got %d", got)
	}
	if got := queryIntClamp(mkReq("n=50"), "n", 10, 1, 100); got != 50 {
		t.Fatalf("in range -> passthrough: got %d", got)
	}
}

func TestTrimmedNonEmptyAndStringSet(t *testing.T) {
	got := trimmedNonEmpty([]string{" a ", "", "b", "   ", "a"})
	if strings.Join(got, ",") != "a,b,a" {
		t.Fatalf("trimmedNonEmpty: got %v", got)
	}
	set := toStringSet(got)
	if len(set) != 2 {
		t.Fatalf("toStringSet should dedupe: got %v", set)
	}
	sorted := sortedStringSet(set)
	if len(sorted) != 2 || sorted[0] != "a" || sorted[1] != "b" {
		t.Fatalf("sortedStringSet: got %v", sorted)
	}
}
