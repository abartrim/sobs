package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// These exercise the app-registry seed dedup guards through the store.DB seam with an injected
// fake — the seed path itself is disabled under parity, so the byte-parity corpus never reaches
// them. Oracle: app.py _lookup_app_id_by_slug / _lookup_release_id return the row's Id when a
// non-deleted match exists, else "".

func TestLookupAppIDBySlug(t *testing.T) {
	// Found: a non-deleted app with the slug → its Id.
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if !strings.Contains(q, "sobs_apps") || !strings.Contains(q, "Slug=?") {
			t.Fatalf("unexpected query: %s", q)
		}
		if len(p) != 1 || p[0] != "web" {
			t.Fatalf("unexpected params: %v", p)
		}
		return storetest.Result([]string{"Id"}, []any{"app-123"}), nil
	}}}
	if got := s.lookupAppIDBySlug("web"); got != "app-123" {
		t.Fatalf("lookupAppIDBySlug found: got %q, want app-123", got)
	}

	// Not found: empty result → "".
	sEmpty := &server{db: &storetest.FakeDB{}}
	if got := sEmpty.lookupAppIDBySlug("nope"); got != "" {
		t.Fatalf("lookupAppIDBySlug not-found: got %q, want empty", got)
	}
}

func TestLookupReleaseID(t *testing.T) {
	// Found: match on all four keys → the release Id.
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if !strings.Contains(q, "sobs_app_releases") {
			t.Fatalf("unexpected query: %s", q)
		}
		want := []any{"app-1", "1.2.3", "abc123", "prod"}
		if len(p) != len(want) {
			t.Fatalf("param count: got %d, want %d", len(p), len(want))
		}
		for i := range want {
			if p[i] != want[i] {
				t.Fatalf("param %d: got %v, want %v", i, p[i], want[i])
			}
		}
		return storetest.Result([]string{"Id"}, []any{"rel-9"}), nil
	}}}
	if got := s.lookupReleaseID("app-1", "1.2.3", "abc123", "prod"); got != "rel-9" {
		t.Fatalf("lookupReleaseID found: got %q, want rel-9", got)
	}

	// Not found: empty result → "".
	sEmpty := &server{db: &storetest.FakeDB{}}
	if got := sEmpty.lookupReleaseID("x", "y", "z", "prod"); got != "" {
		t.Fatalf("lookupReleaseID not-found: got %q, want empty", got)
	}
}
