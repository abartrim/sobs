package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_cve_dispositions_test.go — batch 16 targeted coverage for
// cmd/sobs/cve_dispositions.go's inventoryVersionsByPackage: the populated-inventory path
// (grouping distinct versions per ecosystem::package key) and the skip-on-blank-field branch,
// driven through collectLibraryInventory's tier-2 (otel_sdk) query since that's the simplest
// FakeDB shape to populate.

func TestInventoryVersionsByPackage(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_release_artifacts"):
			return &store.Result{}, nil
		case strings.Contains(q, "telemetry.sdk.name") && strings.Contains(q, "otel_traces"):
			// Two distinct versions of the same npm package, plus one row missing a package name
			// (must be skipped after trimming).
			return storetest.Result([]string{"sdk_name", "sdk_version", "sdk_lang", "ServiceName"},
				[]any{"react", "18.0.0", "javascript", "svc-a"},
				[]any{"react", "18.2.0", "javascript", "svc-b"},
				[]any{"  ", "1.0.0", "javascript", "svc-c"}, // blank pkg (after trim) -> skipped
			), nil
		case strings.Contains(q, "ScopeName"):
			return &store.Result{}, nil
		default:
			return &store.Result{}, nil
		}
	}}}

	got := s.inventoryVersionsByPackage()
	key := "npm::react"
	versions, ok := got[key]
	if !ok {
		t.Fatalf("want key %q present, got keys %v", key, got)
	}
	if len(versions) != 2 {
		t.Fatalf("want 2 distinct versions, got %d: %v", len(versions), versions)
	}
	if _, ok := versions["18.0.0"]; !ok {
		t.Error("want 18.0.0 present")
	}
	if _, ok := versions["18.2.0"]; !ok {
		t.Error("want 18.2.0 present")
	}
	// The blank-package row must not have created its own key.
	for k := range got {
		if !strings.Contains(k, "react") {
			t.Errorf("unexpected extra key from blank-field row: %q", k)
		}
	}
}

func TestInventoryVersionsByPackageEmpty(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	got := s.inventoryVersionsByPackage()
	if len(got) != 0 {
		t.Errorf("want empty map for empty inventory, got %v", got)
	}
}
