package main

import (
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store/storetest"
)

// seedMcpAPIKeyFromEnv only runs when SOBS_MCP_API_KEY is set, which the byte-parity corpus never
// does — so it is corpus-unreachable. Drive it directly.

func TestSeedMcpAPIKeyFromEnv_UnsetEnv_NoOp(t *testing.T) {
	fake := storetest.SettingsDB(map[string]string{})
	(&server{db: fake}).seedMcpAPIKeyFromEnv()
	if len(fake.Inserts) != 0 {
		t.Fatalf("unset env: want no inserts, got %v", fake.Inserts)
	}
}

func TestSeedMcpAPIKeyFromEnv_SeedsNewKey(t *testing.T) {
	t.Setenv("SOBS_MCP_API_KEY", "test-raw-key")
	fake := storetest.SettingsDB(map[string]string{})
	s := &server{db: fake}
	s.seedMcpAPIKeyFromEnv()

	if len(fake.Inserts) != 1 {
		t.Fatalf("want 1 insert, got %d: %v", len(fake.Inserts), fake.Inserts)
	}
	saved := fake.Inserts[0].Rows[0]["Value"].(string)
	v, err := parseJSONValue([]byte(saved))
	if err != nil {
		t.Fatalf("saved value not valid JSON: %v", err)
	}
	list, ok := v.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("want a 1-element list, got %v", v)
	}
	o, ok := list[0].(*jsonenc.Object)
	if !ok {
		t.Fatalf("want a *jsonenc.Object descriptor, got %T", list[0])
	}
	if got, want := objGetStr(o, "key_hash"), hashMcpKey("test-raw-key"); got != want {
		t.Fatalf("key_hash = %q, want %q", got, want)
	}
	if objGetStr(o, "id") == "" {
		t.Fatalf("want a non-empty id")
	}
}

func TestSeedMcpAPIKeyFromEnv_AlreadySeeded_NoOp(t *testing.T) {
	t.Setenv("SOBS_MCP_API_KEY", "test-raw-key")
	existing := jsonenc.NewObject().
		Set("id", "abc123").
		Set("label", "Environment (SOBS_MCP_API_KEY)").
		Set("key_hash", hashMcpKey("test-raw-key")).
		Set("created_at", "2026-01-01T00:00:00Z").
		Set("expires_at", nil)
	raw := string(jsonenc.Encode([]any{existing}, jsonDumpsDefault))
	fake := storetest.SettingsDB(map[string]string{"mcp.api_keys": raw})
	(&server{db: fake}).seedMcpAPIKeyFromEnv()

	if len(fake.Inserts) != 0 {
		t.Fatalf("already-seeded key: want no inserts, got %v", fake.Inserts)
	}
}

func TestSeedMcpAPIKeyFromEnv_AtCap_NoOp(t *testing.T) {
	t.Setenv("SOBS_MCP_API_KEY", "test-raw-key")
	var keys []any
	for i := 0; i < mcpAPIKeyMax; i++ {
		keys = append(keys, any(jsonenc.NewObject().
			Set("id", "id").
			Set("label", "l").
			Set("key_hash", "not-the-seed-hash").
			Set("created_at", "2026-01-01T00:00:00Z").
			Set("expires_at", nil)))
	}
	raw := string(jsonenc.Encode(keys, jsonDumpsDefault))
	fake := storetest.SettingsDB(map[string]string{"mcp.api_keys": raw})
	(&server{db: fake}).seedMcpAPIKeyFromEnv()

	if len(fake.Inserts) != 0 {
		t.Fatalf("at cap: want no inserts, got %v", fake.Inserts)
	}
}

func TestSeedMcpAPIKeyFromEnv_NilDB_NoOp(t *testing.T) {
	t.Setenv("SOBS_MCP_API_KEY", "test-raw-key")
	(&server{}).seedMcpAPIKeyFromEnv() // must not panic on a nil s.db
}
