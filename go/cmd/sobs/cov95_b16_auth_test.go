package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_auth_test.go — batch 16 targeted coverage for cmd/sobs/auth.go:
// resolveManagedCITargetAppID's non-single-segment / release-lookup-miss branches,
// checkExternalAuth's unconfigured/non-200 branches, and sameOriginRequest's
// referer-fallback / TLS-scheme / missing-host branches.

// ---- resolveManagedCITargetAppID ---------------------------------------------------------------

func TestResolveManagedCITargetAppIDReleaseBranches(t *testing.T) {
	t.Run("v1/apps multi-segment path is not a single app_id", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.resolveManagedCITargetAppID("/v1/apps/app-1/extra/segments"); got != "" {
			t.Errorf("want empty, got %q", got)
		}
	})

	t.Run("v1/apps trims whitespace-only segment to empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.resolveManagedCITargetAppID("/v1/apps/   "); got != "" {
			t.Errorf("want empty, got %q", got)
		}
	})

	t.Run("v1/apps/<id>/releases strips the suffix", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.resolveManagedCITargetAppID("/v1/apps/app-42/releases"); got != "app-42" {
			t.Errorf("got %q, want app-42", got)
		}
	})

	t.Run("v1/releases/<id> miss returns empty (no managed gating)", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}} // Execute returns empty result -> findReleaseByID miss
		if got := s.resolveManagedCITargetAppID("/v1/releases/rel-404"); got != "" {
			t.Errorf("want empty on release lookup miss, got %q", got)
		}
	})

	t.Run("v1/releases/<id>/artifacts/meta strips the longer suffix and resolves AppId", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			return storetest.Result([]string{"AppId"}, []any{"app-99"}), nil
		}}}
		if got := s.resolveManagedCITargetAppID("/v1/releases/rel-1/artifacts/meta"); got != "app-99" {
			t.Errorf("got %q, want app-99", got)
		}
	})

	t.Run("v1/releases/<id>/artifacts strips the shorter suffix", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
			return storetest.Result([]string{"AppId"}, []any{"app-7"}), nil
		}}}
		if got := s.resolveManagedCITargetAppID("/v1/releases/rel-2/artifacts"); got != "app-7" {
			t.Errorf("got %q, want app-7", got)
		}
	})

	t.Run("v1/releases multi-segment path is not a single release_id", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.resolveManagedCITargetAppID("/v1/releases/rel-1/artifacts/extra/segments"); got != "" {
			t.Errorf("want empty, got %q", got)
		}
	})

	t.Run("unrelated path resolves to empty", func(t *testing.T) {
		s := &server{db: &storetest.FakeDB{}}
		if got := s.resolveManagedCITargetAppID("/v1/logs"); got != "" {
			t.Errorf("want empty, got %q", got)
		}
	})
}

// ---- checkExternalAuth --------------------------------------------------------------------------

func TestCheckExternalAuthUnconfigured(t *testing.T) {
	s := &server{auth: authConfig{externalURL: ""}}
	if s.checkExternalAuth("Bearer x") {
		t.Error("want false when externalURL is unset")
	}
}

func TestCheckExternalAuthNon200(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	externalURL := "https://auth.example.com"
	endpoint := externalURL + "/internal/auth/validate"
	stem := upstreamFixtureKey(http.MethodPost, endpoint)
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), []byte(`{"status":401,"json":{"ok":false}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{auth: authConfig{externalURL: externalURL}}
	if s.checkExternalAuth("Bearer bad-token") {
		t.Error("want false on non-200 upstream response")
	}
}

func TestCheckExternalAuthSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	externalURL := "https://auth.example.com"
	endpoint := externalURL + "/internal/auth/validate"
	stem := upstreamFixtureKey(http.MethodPost, endpoint)
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), []byte(`{"status":200,"json":{"ok":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &server{auth: authConfig{externalURL: externalURL}}
	if !s.checkExternalAuth("Bearer good-token") {
		t.Error("want true on 200 upstream response")
	}
}

// ---- sameOriginRequest ---------------------------------------------------------------------------

func TestSameOriginRequestVariants(t *testing.T) {
	t.Run("no Origin and no Host means no expected host -> false", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/x", nil)
		r.Host = ""
		if sameOriginRequest(r) {
			t.Error("want false when expectedHost is empty")
		}
	})

	t.Run("Origin matches Host over plain HTTP", func(t *testing.T) {
		r := httptest.NewRequest("POST", "http://example.com/x", nil)
		r.Host = "example.com"
		r.Header.Set("Origin", "http://example.com")
		if !sameOriginRequest(r) {
			t.Error("want true when Origin matches http(host)")
		}
	})

	t.Run("Referer matches when Origin is absent or mismatched", func(t *testing.T) {
		r := httptest.NewRequest("POST", "http://example.com/x", nil)
		r.Host = "example.com"
		r.Header.Set("Referer", "http://example.com/some/page")
		if !sameOriginRequest(r) {
			t.Error("want true via referer fallback")
		}
	})

	t.Run("TLS request expects https scheme", func(t *testing.T) {
		r := httptest.NewRequest("POST", "https://example.com/x", nil)
		r.Host = "example.com"
		r.TLS = &tls.ConnectionState{}
		r.Header.Set("Origin", "https://example.com")
		if !sameOriginRequest(r) {
			t.Error("want true when Origin matches https(host) under TLS")
		}
	})

	t.Run("X-Forwarded-Host/Proto override the expected origin", func(t *testing.T) {
		r := httptest.NewRequest("POST", "http://internal.local/x", nil)
		r.Host = "internal.local"
		r.Header.Set("X-Forwarded-Host", "public.example.com, other.example.com")
		r.Header.Set("X-Forwarded-Proto", "https, http")
		r.Header.Set("Origin", "https://public.example.com")
		if !sameOriginRequest(r) {
			t.Error("want true when Origin matches the forwarded host/proto")
		}
	})

	t.Run("neither origin nor referer matches -> false", func(t *testing.T) {
		r := httptest.NewRequest("POST", "http://example.com/x", nil)
		r.Host = "example.com"
		r.Header.Set("Origin", "http://evil.example.com")
		if sameOriginRequest(r) {
			t.Error("want false on origin mismatch")
		}
	})
}
