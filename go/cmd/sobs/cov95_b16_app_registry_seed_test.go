package main

import (
	"testing"

	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_app_registry_seed_test.go — batch 16 targeted coverage for
// cmd/sobs/app_registry_seed.go: boolToIntSeed's both branches, and seedAppRegistry's early-return
// guards (empty env, nil db) that a full end-to-end seed test doesn't otherwise exercise.

func TestBoolToIntSeed(t *testing.T) {
	if got := boolToIntSeed(true); got != 1 {
		t.Errorf("boolToIntSeed(true) = %d, want 1", got)
	}
	if got := boolToIntSeed(false); got != 0 {
		t.Errorf("boolToIntSeed(false) = %d, want 0", got)
	}
}

func TestSeedAppRegistryNoEnvIsNoop(t *testing.T) {
	t.Setenv("SOBS_APP_REGISTRY_SEED_JSON", "")
	t.Setenv("SOBS_APP_REGISTRY_SEED_JSON_FILE", "")
	fdb := &storetest.FakeDB{}
	s := &server{db: fdb}
	s.seedAppRegistry() // must return before touching the db
	if len(fdb.Inserts) != 0 {
		t.Errorf("want no inserts when env unset, got %v", fdb.Inserts)
	}
}

func TestSeedAppRegistryNilDBIsNoop(t *testing.T) {
	t.Setenv("SOBS_APP_REGISTRY_SEED_JSON", `{"apps":[{"name":"x"}]}`)
	s := &server{db: nil}
	// Must not panic despite a non-empty env var: db==nil short-circuits before any db access.
	s.seedAppRegistry()
}

func TestSeedAppRegistryMalformedJSONLogsAndReturns(t *testing.T) {
	t.Setenv("SOBS_APP_REGISTRY_SEED_JSON", "{not valid json")
	fdb := &storetest.FakeDB{}
	s := &server{db: fdb}
	s.seedAppRegistry()
	if len(fdb.Inserts) != 0 {
		t.Errorf("want no inserts on malformed JSON, got %v", fdb.Inserts)
	}
}

func TestSeedAppRegistryUnexpectedShapeIsIgnored(t *testing.T) {
	// A JSON scalar (neither an object with "apps" nor a bare array) hits the default case.
	t.Setenv("SOBS_APP_REGISTRY_SEED_JSON", `"just a string"`)
	fdb := &storetest.FakeDB{}
	s := &server{db: fdb}
	s.seedAppRegistry()
	if len(fdb.Inserts) != 0 {
		t.Errorf("want no inserts for scalar JSON, got %v", fdb.Inserts)
	}
}
