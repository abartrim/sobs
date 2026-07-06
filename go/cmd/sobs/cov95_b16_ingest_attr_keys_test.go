package main

import (
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b16_ingest_attr_keys_test.go — batch 16 targeted coverage for
// cmd/sobs/ingest_attr_keys.go's rememberAttrKeys: the empty-input no-op guard, the
// insert-error-swallowed branch, and a fresh-cache discover-and-persist path. rememberAttrKeys
// reads/writes the package-level logAttrKeyCache singleton, so this test uses a record type
// ("cov95b16-test-rt") no other test/handler ever uses, keeping it isolated from any state other
// tests may have already primed into the cache.

func TestRememberAttrKeysEmptyInputNoop(t *testing.T) {
	fdb := &storetest.FakeDB{}
	s := &server{db: fdb}
	s.rememberAttrKeys(nil, "cov95b16-test-rt-empty")
	if len(fdb.Inserts) != 0 {
		t.Fatalf("want no inserts for nil attrsMaps, got %v", fdb.Inserts)
	}
	s.rememberAttrKeys([]map[string]any{}, "cov95b16-test-rt-empty")
	if len(fdb.Inserts) != 0 {
		t.Fatalf("want no inserts for empty attrsMaps slice, got %v", fdb.Inserts)
	}
}

func TestRememberAttrKeysDiscoversAndPersistsNewKeys(t *testing.T) {
	const rt = "cov95b16-test-rt-discover"
	fdb := &storetest.FakeDB{ExecuteFunc: func(q string, _ ...any) (*store.Result, error) {
		// prime(): DISTINCT AttrKey query for every record type -> empty (fresh cache row set).
		return &store.Result{}, nil
	}}
	s := &server{db: fdb}
	s.rememberAttrKeys([]map[string]any{
		{"cov.b16.key.one": "v1", "  cov.b16.key.two  ": "v2"}, // whitespace-padded key trimmed
		{"cov.b16.key.one": "dup"},                             // duplicate within the same call
	}, rt)

	if len(fdb.Inserts) != 1 {
		t.Fatalf("want exactly one insert call, got %d: %+v", len(fdb.Inserts), fdb.Inserts)
	}
	ins := fdb.Inserts[0]
	if ins.Table != "sobs_log_attr_keys" {
		t.Fatalf("table = %q, want sobs_log_attr_keys", ins.Table)
	}
	gotKeys := map[string]bool{}
	for _, row := range ins.Rows {
		if row["RecordType"] != rt {
			t.Errorf("RecordType = %v, want %v", row["RecordType"], rt)
		}
		if row["IsDeleted"] != 0 {
			t.Errorf("IsDeleted = %v, want 0", row["IsDeleted"])
		}
		gotKeys[row["AttrKey"].(string)] = true
	}
	if !gotKeys["cov.b16.key.one"] || !gotKeys["cov.b16.key.two"] {
		t.Fatalf("want both trimmed keys discovered, got %v", gotKeys)
	}
	if len(gotKeys) != 2 {
		t.Fatalf("want exactly 2 distinct keys (dedup within call), got %d: %v", len(gotKeys), gotKeys)
	}

	// A second call with the SAME keys must not re-insert (already known from the in-memory cache
	// this same *server call populated).
	fdb.Inserts = nil
	s.rememberAttrKeys([]map[string]any{{"cov.b16.key.one": "v3"}}, rt)
	if len(fdb.Inserts) != 0 {
		t.Fatalf("want no re-insert of an already-known key, got %v", fdb.Inserts)
	}
}

func TestRememberAttrKeysInsertErrorSwallowed(t *testing.T) {
	const rt = "cov95b16-test-rt-insert-err"
	fdb := &storetest.FakeDB{
		ExecuteFunc: func(string, ...any) (*store.Result, error) { return &store.Result{}, nil },
		InsertErr:   assertErr("insert failed"),
	}
	s := &server{db: fdb}
	// Must not panic even though the insert fails; the discovered key is simply not persisted to
	// the in-memory cache (mirrors Python's best-effort insert wrapped in try/except).
	s.rememberAttrKeys([]map[string]any{{"cov.b16.key.err": "v"}}, rt)
	if len(fdb.Inserts) != 1 {
		t.Fatalf("want the insert to have been attempted, got %d calls", len(fdb.Inserts))
	}
}

func TestRememberAttrKeysBlankKeysSkipped(t *testing.T) {
	const rt = "cov95b16-test-rt-blank"
	fdb := &storetest.FakeDB{ExecuteFunc: func(string, ...any) (*store.Result, error) {
		return &store.Result{}, nil
	}}
	s := &server{db: fdb}
	s.rememberAttrKeys([]map[string]any{{"   ": "v", "": "v2"}}, rt)
	if len(fdb.Inserts) != 0 {
		t.Fatalf("want no inserts for all-blank keys, got %v", fdb.Inserts)
	}
}
