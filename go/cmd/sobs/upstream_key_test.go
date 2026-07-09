package main

import "testing"

// Cross-runtime parity for the upstream fixture keys: these MUST match
// migration/tools/determinism.py (upstream_fixture_key / upstream_fixture_key_body) byte-for-byte,
// or the Python oracle and the Go binary would read different canned files for the same request.
// The expected hex values are computed independently by the Python implementation.

func TestUpstreamFixtureKeyParity(t *testing.T) {
	// URL-only key — sha256("METHOD url")[:32]. Method is upper-cased on both sides.
	if got := upstreamFixtureKey("post", "https://sobs-ai.mock/chat/completions"); got != "66826e4ebb951e10b3e97d0cf3be4a1d" {
		t.Errorf("upstreamFixtureKey = %q, want 66826e4ebb951e10b3e97d0cf3be4a1d (Python parity)", got)
	}
}

func TestUpstreamFixtureKeyBodyParity(t *testing.T) {
	// Body-sensitive key — sha256("METHOD url\n" + body)[:32].
	body := []byte(`{"model": "m", "messages": [{"role": "user", "content": "hi"}], "max_tokens": 1024}`)
	if got := upstreamFixtureKeyBody("POST", "https://sobs-ai.mock/chat/completions", body); got != "cd42623034ef7d480f65b8869b3b70b4" {
		t.Errorf("upstreamFixtureKeyBody = %q, want cd42623034ef7d480f65b8869b3b70b4 (Python parity)", got)
	}
	// Distinct bodies -> distinct keys (the whole point of body-sensitivity).
	other := upstreamFixtureKeyBody("POST", "https://sobs-ai.mock/chat/completions", []byte(`{"model":"m2"}`))
	if other == "cd42623034ef7d480f65b8869b3b70b4" {
		t.Error("different bodies must produce different keys")
	}
}
