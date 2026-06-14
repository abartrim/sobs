package main

import "testing"

func TestScryptMatchesPython(t *testing.T) {
	got := hashMcpKey("mcp-parity-token")
	want := "7331bd3bf079f63789d59ed01a5ebbd3bccf7f8472ec020f181153a9ea148884"
	if got != want {
		t.Fatalf("scrypt mismatch:\n got=%s\nwant=%s", got, want)
	}
}
