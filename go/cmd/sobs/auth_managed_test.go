package main

import (
	"encoding/hex"
	"testing"
)

// Python-derived reference values (frozen oracle, SOBS_SECRET_KEY="parity-fixed-secret-key"):
//
//	blake2b(secret, person="sobs-ci-hash-v1", digest_size=32).hex()           -> ciHashSaltHex
//	scrypt("ci-parity-token", salt=that, n=1024,r=8,p=1,dklen=32).hex()       -> ciParityTokenScryptHex
const (
	ciHashSaltHex          = "76c19dd45848ddbd83c3455b63ef1f9f6bce6fd421b198bb32ba3a5f348df9ca"
	ciParityTokenScryptHex = "cccd419c4d57cb86ce554255a82d2b45deb5df19f58f7635678a1e8a12443a57"
)

// blake2bPersonalSum must match Python's hashlib.blake2b for BOTH digest sizes the app uses:
//   - mcp.py _mcp_mac_key: default digest_size=64, then .digest()[:32]
//   - app.py _ci_push_hash_key: explicit digest_size=32
//
// The digest size is mixed into the parameter block, so these are genuinely different computations;
// pinning both guards the hand-rolled BLAKE2b against an independently-derived Python value.
func TestBlake2bMatchesPython(t *testing.T) {
	// mcp salt: blake2b(secret, person="sobs-mcp-v1\0\0\0\0\0").digest()[:32]
	mcp := hex.EncodeToString(blake2bPersonalSum([]byte("parity-fixed-secret-key"), []byte("sobs-mcp-v1"), 64)[:32])
	if mcp != mcpScryptSaltHex {
		t.Errorf("mcp salt (digest_size=64, trunc 32) mismatch:\n got=%s\nwant=%s", mcp, mcpScryptSaltHex)
	}
	// ci-push salt: blake2b(secret, person="sobs-ci-hash-v1", digest_size=32)
	ci := hex.EncodeToString(blake2bPersonalSum([]byte("parity-fixed-secret-key"), []byte("sobs-ci-hash-v1"), 32))
	if ci != ciHashSaltHex {
		t.Errorf("ci-push salt (digest_size=32) mismatch:\n got=%s\nwant=%s", ci, ciHashSaltHex)
	}
}

// hashAPIKey must be byte-exact with app.py _hash_api_key. A mismatch means a per-app CI key
// rotated under Python could never validate under Go, and vice-versa (finding C2).
func TestHashAPIKeyMatchesPython(t *testing.T) {
	t.Setenv("SOBS_SECRET_KEY", "parity-fixed-secret-key")
	got := hashAPIKey("ci-parity-token")
	want := "scrypt:v1:" + ciParityTokenScryptHex
	if got != want {
		t.Fatalf("hashAPIKey mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestHashAPIKeyEmptyAndDistinct(t *testing.T) {
	t.Setenv("SOBS_SECRET_KEY", "parity-fixed-secret-key")
	if hashAPIKey("") != "" || hashAPIKey("   ") != "" {
		t.Error("blank key should hash to empty string (mirrors _hash_api_key)")
	}
	if hashAPIKey("a") == hashAPIKey("b") {
		t.Error("distinct keys must produce distinct hashes")
	}
	if got := hashAPIKey(" trimmed "); got != hashAPIKey("trimmed") {
		t.Errorf("hashAPIKey must trim before hashing: %q", got)
	}
}

func TestResolveManagedCITargetAppID(t *testing.T) {
	s := newAuthServer(authConfig{}) // nil db: only the /v1/apps + non-matching branches are exercised
	cases := map[string]string{
		"/v1/apps/abc123":          "abc123",
		"/v1/apps/abc123/releases": "abc123",
		"/v1/apps":                 "", // collection route, no <app_id> kwarg
		"/v1/apps/":                "", // empty segment
		"/v1/apps/a/b":             "", // not a single segment -> Flask would not route here
		"/v1/logs":                 "",
		"/api/tags/log/123":        "",
		"/v1/rum/assets":           "",
	}
	for path, want := range cases {
		if got := s.resolveManagedCITargetAppID(path); got != want {
			t.Errorf("resolveManagedCITargetAppID(%q) = %q, want %q", path, got, want)
		}
	}
}
