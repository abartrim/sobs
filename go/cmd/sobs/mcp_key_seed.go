package main

import (
	"encoding/hex"
	"log"
	"os"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// mcpAPIKeyEnvVar pre-seeds a usable MCP API key at startup from the environment, so local dev
// setups and agents can authenticate against X-MCP-API-Key immediately without a manual
// POST /api/mcp/keys round-trip through the UI.
const mcpAPIKeyEnvVar = "SOBS_MCP_API_KEY"

// seedMcpAPIKeyFromEnv registers SOBS_MCP_API_KEY (when set) in the mcp.api_keys keystore, using
// the same descriptor shape and scrypt hash (hashMcpKey) as a key minted via mcpAPIKeysCreate.
// No-op when the env var is unset. Idempotent across restarts: keyed by the raw key's hash, so
// re-seeding the same value never appends a duplicate descriptor.
func (s *server) seedMcpAPIKeyFromEnv() {
	rawKey := strings.TrimSpace(os.Getenv(mcpAPIKeyEnvVar))
	if rawKey == "" || s.db == nil {
		return
	}
	keyHash := hashMcpKey(rawKey)
	keys := s.loadMcpAPIKeys()
	for _, e := range keys {
		if o, ok := e.(*jsonenc.Object); ok && objGetStr(o, "key_hash") == keyHash {
			return
		}
	}
	if len(keys) >= mcpAPIKeyMax {
		log.Printf("mcp: %s is set but the key cap (%d) is already reached; skipping seed", mcpAPIKeyEnvVar, mcpAPIKeyMax)
		return
	}
	descriptor := jsonenc.NewObject().
		Set("id", hex.EncodeToString(randBytes(8))).
		Set("label", "Environment ("+mcpAPIKeyEnvVar+")").
		Set("key_hash", keyHash).
		Set("created_at", nowUTC().Format("2006-01-02T15:04:05Z")).
		Set("expires_at", nil)
	s.saveMcpAPIKeys(append(keys, any(descriptor)))
	log.Printf("mcp: seeded API key from %s", mcpAPIKeyEnvVar)
}
