package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
)

// Hand-rolled scrypt (RFC 7914) + PBKDF2-HMAC-SHA256, to match Python hashlib.scrypt without
// pulling in golang.org/x/crypto (minimal-deps constraint). Used only for MCP API-key fingerprints.

// pbkdf2SHA256 derives keyLen bytes (mirrors hashlib.pbkdf2_hmac / the PBKDF2 inside scrypt).
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	numBlocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, numBlocks*hLen)
	buf := make([]byte, 4)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(block))
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for n := 1; n < iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for x := range t {
				t[x] ^= u[x]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func salsa20_8(b *[16]uint32) {
	var x [16]uint32
	copy(x[:], b[:])
	rotl := func(a uint32, n uint) uint32 { return (a << n) | (a >> (32 - n)) }
	for i := 0; i < 8; i += 2 {
		x[4] ^= rotl(x[0]+x[12], 7)
		x[8] ^= rotl(x[4]+x[0], 9)
		x[12] ^= rotl(x[8]+x[4], 13)
		x[0] ^= rotl(x[12]+x[8], 18)
		x[9] ^= rotl(x[5]+x[1], 7)
		x[13] ^= rotl(x[9]+x[5], 9)
		x[1] ^= rotl(x[13]+x[9], 13)
		x[5] ^= rotl(x[1]+x[13], 18)
		x[14] ^= rotl(x[10]+x[6], 7)
		x[2] ^= rotl(x[14]+x[10], 9)
		x[6] ^= rotl(x[2]+x[14], 13)
		x[10] ^= rotl(x[6]+x[2], 18)
		x[3] ^= rotl(x[15]+x[11], 7)
		x[7] ^= rotl(x[3]+x[15], 9)
		x[11] ^= rotl(x[7]+x[3], 13)
		x[15] ^= rotl(x[11]+x[7], 18)
		x[1] ^= rotl(x[0]+x[3], 7)
		x[2] ^= rotl(x[1]+x[0], 9)
		x[3] ^= rotl(x[2]+x[1], 13)
		x[0] ^= rotl(x[3]+x[2], 18)
		x[6] ^= rotl(x[5]+x[4], 7)
		x[7] ^= rotl(x[6]+x[5], 9)
		x[4] ^= rotl(x[7]+x[6], 13)
		x[5] ^= rotl(x[4]+x[7], 18)
		x[11] ^= rotl(x[10]+x[9], 7)
		x[8] ^= rotl(x[11]+x[10], 9)
		x[9] ^= rotl(x[8]+x[11], 13)
		x[10] ^= rotl(x[9]+x[8], 18)
		x[12] ^= rotl(x[15]+x[14], 7)
		x[13] ^= rotl(x[12]+x[15], 9)
		x[14] ^= rotl(x[13]+x[12], 13)
		x[15] ^= rotl(x[14]+x[13], 18)
	}
	for i := range b {
		b[i] += x[i]
	}
}

// blockMix mirrors the scrypt BlockMix with 2r 64-byte blocks. in/out are 128*r bytes.
func blockMix(in, out []byte, r int) {
	var x [16]uint32
	for i := 0; i < 16; i++ {
		x[i] = binary.LittleEndian.Uint32(in[(2*r-1)*64+i*4:])
	}
	var t [16]uint32
	for i := 0; i < 2*r; i++ {
		for j := 0; j < 16; j++ {
			t[j] = x[j] ^ binary.LittleEndian.Uint32(in[i*64+j*4:])
		}
		x = t
		salsa20_8(&x)
		var dst int
		if i%2 == 0 {
			dst = (i / 2) * 64
		} else {
			dst = (r + (i-1)/2) * 64
		}
		for j := 0; j < 16; j++ {
			binary.LittleEndian.PutUint32(out[dst+j*4:], x[j])
		}
	}
}

// scryptROMix mirrors the scrypt ROMix on a 128*r byte block.
func scryptROMix(b []byte, n, r int) {
	blockLen := 128 * r
	x := make([]byte, blockLen)
	copy(x, b)
	v := make([]byte, n*blockLen)
	tmp := make([]byte, blockLen)
	for i := 0; i < n; i++ {
		copy(v[i*blockLen:], x)
		blockMix(x, tmp, r)
		x, tmp = tmp, x
	}
	for i := 0; i < n; i++ {
		j := int(binary.LittleEndian.Uint32(x[(2*r-1)*64:]) % uint32(n))
		for k := 0; k < blockLen; k++ {
			x[k] ^= v[j*blockLen+k]
		}
		blockMix(x, tmp, r)
		x, tmp = tmp, x
	}
	copy(b, x)
}

// scryptKey mirrors hashlib.scrypt(password, salt=salt, n=N, r=r, p=p, dklen=keyLen).
func scryptKey(password, salt []byte, N, r, p, keyLen int) []byte {
	b := pbkdf2SHA256(password, salt, 1, p*128*r)
	blockLen := 128 * r
	for i := 0; i < p; i++ {
		scryptROMix(b[i*blockLen:(i+1)*blockLen], N, r)
	}
	return pbkdf2SHA256(password, b, 1, keyLen)
}

// mcpScryptSaltHex is the parity-secret reference value: blake2b(SOBS_SECRET_KEY=
// "parity-fixed-secret-key", person="sobs-mcp-v1\0\0\0\0\0").digest()[:32]. It is NO LONGER used
// in the hash path (mcpMacKey derives the salt at runtime) — it is retained as the independently
// Python-derived expectation that TestBlake2bMatchesPython pins the hand-rolled BLAKE2b against,
// and as the value mcpMacKey() must reproduce when the parity secret is configured.
const mcpScryptSaltHex = "7d9c06e1f59311d11e03eb7852f305278559898943739e9c77d03217dee42835"

// mcpMacKey mirrors mcp.py _mcp_mac_key: a per-installation 32-byte salt derived from
// SOBS_SECRET_KEY at RUNTIME via personalized BLAKE2b — hashlib.blake2b(secret, person=
// b"sobs-mcp-v1\x00\x00\x00\x00\x00").digest()[:32]. Python's blake2b default digest_size is 64,
// so we compute the full 64-byte digest and truncate to 32 (blake2bPersonalSum zero-pads the
// 11-byte person tag to the 16-byte personal field, matching the explicit null padding). The
// secret defaults to "sobs-dev-secret-key" only when the env var is ABSENT (os.environ.get's
// default honors an explicitly-empty value), so LookupEnv — same convention as ciPushHashSalt.
// For the parity secret ("parity-fixed-secret-key") this reproduces the previously-hardcoded
// 7d9c06e1...2835 salt byte-for-byte, so parity is unaffected; a real secret is now honored.
func mcpMacKey() []byte {
	secret, ok := os.LookupEnv("SOBS_SECRET_KEY")
	if !ok {
		secret = "sobs-dev-secret-key"
	}
	return blake2bPersonalSum([]byte(secret), []byte("sobs-mcp-v1"), 64)[:32]
}

// hashMcpKey mirrors mcp.py _hash_key: scrypt(token, salt=_mcp_mac_key(), n=1024, r=8, p=1,
// dklen=32) hex.
func hashMcpKey(rawToken string) string {
	return hex.EncodeToString(scryptKey([]byte(rawToken), mcpMacKey(), 1024, 8, 1, 32))
}
