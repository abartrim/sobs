package main

import (
	"encoding/hex"
	"testing"
)

// cov95_b16_mcp_scrypt_test.go — batch 16 targeted coverage for cmd/sobs/mcp_scrypt.go's
// pbkdf2SHA256: the multi-block derivation path (dklen > one HMAC block, requiring numBlocks > 1)
// and the multi-iteration XOR-fold path, pinned against independently-computed
// hashlib.pbkdf2_hmac('sha256', ...) reference vectors (Python oracle).

func TestPbkdf2SHA256KnownVectors(t *testing.T) {
	cases := []struct {
		name     string
		pw, salt string
		iter     int
		keyLen   int
		wantHex  string
	}{
		// hashlib.pbkdf2_hmac('sha256', b'password', b'salt', 1, 32).hex()
		{"single iteration, one block", "password", "salt", 1, 32,
			"120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"},
		// hashlib.pbkdf2_hmac('sha256', b'password', b'salt', 2, 32).hex()
		{"two iterations (XOR-fold path)", "password", "salt", 2, 32,
			"ae4d0c95af6b46d32d0adff928f06dd02a303f8ef3c251dfd6e2d85a95474c43"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := hex.EncodeToString(pbkdf2SHA256([]byte(c.pw), []byte(c.salt), c.iter, c.keyLen))
			if got != c.wantHex {
				t.Errorf("got  %s\nwant %s", got, c.wantHex)
			}
		})
	}
}

func TestPbkdf2SHA256MultiBlockDerivation(t *testing.T) {
	// dklen=40 with a SHA256 HMAC (32-byte output) requires numBlocks=2, exercising the
	// multi-block concatenation loop. Reference: hashlib.pbkdf2_hmac('sha256',
	// b'passwordPASSWORDpassword', b'saltSALTsaltSALTsaltSALTsaltSALTsalt', 4096, 40).hex()
	got := hex.EncodeToString(pbkdf2SHA256(
		[]byte("passwordPASSWORDpassword"),
		[]byte("saltSALTsaltSALTsaltSALTsaltSALTsalt"),
		4096, 40))
	want := "348c89dbcbd32b2f32d814b8116e84cf2b17347ebc1800181c4e2a1fb8dd53e1c635518c7dac47e9"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestPbkdf2SHA256DeterministicAndDistinctForDifferentInputs(t *testing.T) {
	a := pbkdf2SHA256([]byte("pw1"), []byte("salt1"), 10, 16)
	b := pbkdf2SHA256([]byte("pw1"), []byte("salt1"), 10, 16)
	if hex.EncodeToString(a) != hex.EncodeToString(b) {
		t.Fatal("pbkdf2SHA256 must be deterministic for identical inputs")
	}
	c := pbkdf2SHA256([]byte("pw2"), []byte("salt1"), 10, 16)
	if hex.EncodeToString(a) == hex.EncodeToString(c) {
		t.Fatal("different passwords must yield different keys")
	}
}
