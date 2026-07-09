package main

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// cov95_b16_blake2b_test.go — batch 16 targeted coverage for cmd/sobs/blake2b.go's
// blake2bPersonalSum: the multi-block (>128 bytes) input path and a person tag longer than 16
// bytes (truncated, mirroring the fixed-size personal field). TestBlake2bMatchesPython
// (auth_managed_test.go) already covers the single-block, exact-16-byte-person case.

func TestBlake2bPersonalSumMultiBlock(t *testing.T) {
	// A 300-byte input spans multiple 128-byte compression blocks (full=2, remainder=44 bytes).
	data := make([]byte, 300)
	for i := range data {
		data[i] = byte(i % 251)
	}
	out := blake2bPersonalSum(data, []byte("multi-block-test"), 32)
	if len(out) != 32 {
		t.Fatalf("want 32-byte digest, got %d bytes", len(out))
	}
	// Determinism: hashing the same input twice must yield the identical digest.
	out2 := blake2bPersonalSum(data, []byte("multi-block-test"), 32)
	if hex.EncodeToString(out) != hex.EncodeToString(out2) {
		t.Fatalf("hash not deterministic: %x vs %x", out, out2)
	}
	// Sanity: it must not just be a stdlib sha256 in disguise (different algorithm entirely).
	sha := sha256.Sum256(data)
	if hex.EncodeToString(out) == hex.EncodeToString(sha[:]) {
		t.Fatalf("blake2b output unexpectedly matched sha256")
	}
}

func TestBlake2bPersonalSumEmptyInput(t *testing.T) {
	// len(data)==0 exercises the "full=0" branch (no complete blocks before the final block).
	out := blake2bPersonalSum([]byte{}, []byte("p"), 16)
	if len(out) != 16 {
		t.Fatalf("want 16-byte digest, got %d bytes", len(out))
	}
}

func TestBlake2bPersonalSumLongPersonTruncated(t *testing.T) {
	// A person tag longer than 16 bytes must be silently truncated to the fixed personal field,
	// matching copy(p[:], person) semantics (Go's copy caps at len(dst)).
	longPerson := []byte("this-is-way-more-than-sixteen-bytes-long")
	truncated := longPerson[:16]
	out1 := blake2bPersonalSum([]byte("hello"), longPerson, 32)
	out2 := blake2bPersonalSum([]byte("hello"), truncated, 32)
	if hex.EncodeToString(out1) != hex.EncodeToString(out2) {
		t.Fatalf("expected truncated person to match explicit 16-byte person: %x vs %x", out1, out2)
	}
}

func TestBlake2bPersonalSumExactBlockBoundary(t *testing.T) {
	// Exactly 128 bytes: full = (128-1)/128 = 0, so the entire input is the final block.
	data := make([]byte, 128)
	for i := range data {
		data[i] = byte(i)
	}
	out := blake2bPersonalSum(data, []byte("boundary"), 64)
	if len(out) != 64 {
		t.Fatalf("want 64-byte digest, got %d", len(out))
	}
	// 256 bytes: full = (256-1)/128 = 1 complete block, then a full 128-byte final block.
	data2 := make([]byte, 256)
	for i := range data2 {
		data2[i] = byte(i)
	}
	out2 := blake2bPersonalSum(data2, []byte("boundary"), 64)
	if len(out2) != 64 || hex.EncodeToString(out) == hex.EncodeToString(out2) {
		t.Fatalf("distinct inputs must give distinct digests")
	}
}
