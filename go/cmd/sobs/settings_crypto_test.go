package main

import (
	"encoding/base64"
	"testing"
)

// The official Fernet spec test vector (github.com/fernet/spec verify.json) — proves this
// implementation is byte-compatible with Python's cryptography.fernet, so tokens written by a
// migrating Python deployment decrypt here.
const (
	fernetVectorKey       = "cw_0x689RpI-jtRR7oE8h_eQsKImvJapLeSbXpwF4e4="
	fernetVectorToken     = "gAAAAAAdwJ6wAAECAwQFBgcICQoLDA0ODy021cpGVWKZ_eEwCGM4BLLF_5CV9dOPmrhuVUPgJobwOz7JcbmrR64jVmpU4IwqDA=="
	fernetVectorPlaintext = "hello"
	fernetVectorTS        = int64(499162800)
)

func TestFernetSpecVectorDecrypt(t *testing.T) {
	key, err := base64.URLEncoding.DecodeString(fernetVectorKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fernetDecrypt(key, fernetVectorToken)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != fernetVectorPlaintext {
		t.Errorf("decrypt = %q, want %q", got, fernetVectorPlaintext)
	}
}

func TestFernetSpecVectorEncrypt(t *testing.T) {
	key, _ := base64.URLEncoding.DecodeString(fernetVectorKey)
	iv := make([]byte, 16)
	for i := range iv {
		iv[i] = byte(i) // the spec vector IV is 0..15
	}
	got, err := fernetEncryptWithIV(key, fernetVectorPlaintext, fernetVectorTS, iv)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if got != fernetVectorToken {
		t.Errorf("encrypt =\n %q\nwant\n %q", got, fernetVectorToken)
	}
}

func TestFernetRoundTrip(t *testing.T) {
	key, _ := base64.URLEncoding.DecodeString(fernetVectorKey)
	for _, pt := range []string{"", "x", "a longer secret value with spaces", "🔐 unicode"} {
		token, err := fernetEncryptWithIV(key, pt, 1, nil) // random IV
		if err != nil {
			t.Fatalf("encrypt %q: %v", pt, err)
		}
		back, err := fernetDecrypt(key, token)
		if err != nil || back != pt {
			t.Errorf("round-trip %q -> %q err %v", pt, back, err)
		}
	}
	// A tampered token must fail HMAC.
	token, _ := fernetEncryptWithIV(key, "x", 1, nil)
	if _, err := fernetDecrypt(key, token[:len(token)-2]+"AA"); err == nil {
		t.Error("tampered token should fail")
	}
}

func TestSettingsEncryptionGating(t *testing.T) {
	// No secret configured -> strict no-op (the parity invariant).
	plain := newAuthServer(authConfig{})
	plain.cfg.EncryptionSecret = ""
	if got := plain.encryptSecretValue("topsecret"); got != "topsecret" {
		t.Errorf("no-secret encrypt should be no-op, got %q", got)
	}
	if got := plain.decryptSecretValue("topsecret"); got != "topsecret" {
		t.Errorf("no-secret decrypt of plaintext should pass through, got %q", got)
	}

	// Secret configured -> round-trips through the enc:v1: prefix.
	s := newAuthServer(authConfig{})
	s.cfg.EncryptionSecret = "my-deployment-secret"
	enc := s.encryptSecretValue("sk-abc123")
	if enc == "sk-abc123" || enc[:len(settingsEncPrefix)] != settingsEncPrefix {
		t.Fatalf("expected enc:v1: prefixed ciphertext, got %q", enc)
	}
	if got := s.decryptSecretValue(enc); got != "sk-abc123" {
		t.Errorf("decrypt = %q, want sk-abc123", got)
	}
	// Already-encrypted value is not double-encrypted.
	if again := s.encryptSecretValue(enc); again != enc {
		t.Error("double-encrypt should be a no-op")
	}
}

func TestIsSensitiveAISettingKey(t *testing.T) {
	for _, k := range []string{"ai.api_key", "ai.github_token", "ai.github_token.repo.owner/name", "AI.API_KEY"} {
		if !isSensitiveAISettingKey(k) {
			t.Errorf("%q should be sensitive", k)
		}
	}
	for _, k := range []string{"ai.model", "ai.endpoint_url", "ai.guard_model"} {
		if isSensitiveAISettingKey(k) {
			t.Errorf("%q should not be sensitive", k)
		}
	}
}
