package main

import (
	"encoding/binary"
	"math/bits"
)

// Hand-rolled BLAKE2b (RFC 7693), unkeyed, with personalization — to match Python's
// hashlib.blake2b(data, person=..., digest_size=...) without pulling in golang.org/x/crypto
// (minimal-deps constraint, same rationale as mcp_scrypt.go's hand-rolled scrypt). Used to derive
// the per-installation CI-push hash salt from SOBS_SECRET_KEY at RUNTIME, so a salt computed by
// the Python oracle (_ci_push_hash_key) and by the Go server agree for the same secret — the
// precomputed-constant trick in mcp_scrypt.go only works because the parity secret is fixed; the
// CI-push salt must track whatever secret an operator actually configures.

var blake2bIV = [8]uint64{
	0x6a09e667f3bcc908, 0xbb67ae8584caa73b,
	0x3c6ef372fe94f82b, 0xa54ff53a5f1d36f1,
	0x510e527fade682d1, 0x9b05688c2b3e6c1f,
	0x1f83d9abfb41bd6b, 0x5be0cd19137e2179,
}

var blake2bSigma = [12][16]byte{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	{14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
	{11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4},
	{7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8},
	{9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13},
	{2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9},
	{12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11},
	{13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10},
	{6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5},
	{10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0},
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	{14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
}

func blake2bG(v *[16]uint64, a, b, c, d int, x, y uint64) {
	v[a] = v[a] + v[b] + x
	v[d] = bits.RotateLeft64(v[d]^v[a], -32)
	v[c] = v[c] + v[d]
	v[b] = bits.RotateLeft64(v[b]^v[c], -24)
	v[a] = v[a] + v[b] + y
	v[d] = bits.RotateLeft64(v[d]^v[a], -16)
	v[c] = v[c] + v[d]
	v[b] = bits.RotateLeft64(v[b]^v[c], -63)
}

// blake2bCompress is the RFC 7693 compression F on a 128-byte block. t is the (low 64 bits of the)
// byte counter; our inputs are far below 2^64 bytes so the high word is always zero.
func blake2bCompress(h *[8]uint64, block []byte, t uint64, last bool) {
	var m [16]uint64
	for i := 0; i < 16; i++ {
		m[i] = binary.LittleEndian.Uint64(block[i*8:])
	}
	var v [16]uint64
	copy(v[:8], h[:])
	copy(v[8:], blake2bIV[:])
	v[12] ^= t
	if last {
		v[14] ^= 0xffffffffffffffff
	}
	for r := 0; r < 12; r++ {
		s := &blake2bSigma[r]
		blake2bG(&v, 0, 4, 8, 12, m[s[0]], m[s[1]])
		blake2bG(&v, 1, 5, 9, 13, m[s[2]], m[s[3]])
		blake2bG(&v, 2, 6, 10, 14, m[s[4]], m[s[5]])
		blake2bG(&v, 3, 7, 11, 15, m[s[6]], m[s[7]])
		blake2bG(&v, 0, 5, 10, 15, m[s[8]], m[s[9]])
		blake2bG(&v, 1, 6, 11, 12, m[s[10]], m[s[11]])
		blake2bG(&v, 2, 7, 8, 13, m[s[12]], m[s[13]])
		blake2bG(&v, 3, 4, 9, 14, m[s[14]], m[s[15]])
	}
	for i := 0; i < 8; i++ {
		h[i] ^= v[i] ^ v[i+8]
	}
}

// blake2bPersonalSum mirrors hashlib.blake2b(data, person=person, digest_size=size).digest() for
// the unkeyed, single-personalization case (no key, no salt). person is right-zero-padded (or
// truncated) to the 16-byte personal field; size must be in 1..64.
func blake2bPersonalSum(data, person []byte, size int) []byte {
	var p [16]byte
	copy(p[:], person)

	var h [8]uint64
	copy(h[:], blake2bIV[:])
	// Parameter block word 0: digest_length | key_length<<8 | fanout<<16 | depth<<24.
	h[0] ^= uint64(size) | (1 << 16) | (1 << 24)
	// Words 6 & 7 carry the 16-byte personalization (salt occupies words 4 & 5, left zero).
	h[6] ^= binary.LittleEndian.Uint64(p[0:8])
	h[7] ^= binary.LittleEndian.Uint64(p[8:16])

	var counter uint64
	full := 0 // complete 128-byte blocks BEFORE the final (last=true) block
	if len(data) > 0 {
		full = (len(data) - 1) / 128
	}
	for i := 0; i < full; i++ {
		counter += 128
		blake2bCompress(&h, data[i*128:(i+1)*128], counter, false)
	}
	var fb [128]byte
	rem := data[full*128:]
	copy(fb[:], rem)
	counter += uint64(len(rem))
	blake2bCompress(&h, fb[:], counter, true)

	out := make([]byte, 64)
	for i := 0; i < 8; i++ {
		binary.LittleEndian.PutUint64(out[i*8:], h[i])
	}
	return out[:size]
}
