package main

import "testing"

func TestScryptMatchesPython(t *testing.T) {
	// hashMcpKey now derives the scrypt salt from SOBS_SECRET_KEY at runtime (mcp.py
	// _mcp_mac_key), so pin the parity secret the `want` value was generated under — same
	// convention as TestHashAPIKeyMatchesPython. Python reference:
	//   salt = blake2b("parity-fixed-secret-key", person="sobs-mcp-v1\0\0\0\0\0").digest()[:32]
	//   scrypt("mcp-parity-token", salt=salt, n=1024, r=8, p=1, dklen=32).hex()
	t.Setenv("SOBS_SECRET_KEY", "parity-fixed-secret-key")
	got := hashMcpKey("mcp-parity-token")
	want := "7331bd3bf079f63789d59ed01a5ebbd3bccf7f8472ec020f181153a9ea148884"
	if got != want {
		t.Fatalf("scrypt mismatch:\n got=%s\nwant=%s", got, want)
	}
}
