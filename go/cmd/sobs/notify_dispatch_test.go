package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// RFC 5869 Appendix A.1 HKDF-SHA256 test vector.
func TestHKDFRFC5869(t *testing.T) {
	ikm, _ := hex.DecodeString("0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b")
	salt, _ := hex.DecodeString("000102030405060708090a0b0c")
	info, _ := hex.DecodeString("f0f1f2f3f4f5f6f7f8f9")
	wantPRK := "077709362c2e32df0ddc3f0dc47bba6390b6c73bb50f9c3122ec844ad7c2b3e5"
	wantOKM := "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865"

	if got := hex.EncodeToString(hkdfExtract(salt, ikm)); got != wantPRK {
		t.Errorf("HKDF-Extract = %s, want %s", got, wantPRK)
	}
	prk := hkdfExtract(salt, ikm)
	if got := hex.EncodeToString(hkdfExpand(prk, info, 42)); got != wantOKM {
		t.Errorf("HKDF-Expand = %s, want %s", got, wantOKM)
	}
}

func TestBuildVapidJWT(t *testing.T) {
	priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	token, err := buildVapidJWT(map[string]any{"aud": "https://push.example.com", "exp": int64(123), "sub": "mailto:x@y"}, priv)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT should have 3 parts, got %d", len(parts))
	}
	// Header decodes to alg ES256.
	hdrRaw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var hdr map[string]any
	_ = json.Unmarshal(hdrRaw, &hdr)
	if hdr["alg"] != "ES256" {
		t.Errorf("alg = %v, want ES256", hdr["alg"])
	}
	// ES256 signature verifies against the public key.
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if len(sig) != 64 {
		t.Fatalf("sig should be 64 bytes (r||s), got %d", len(sig))
	}
	r := new(big.Int).SetBytes(sig[:32])
	sv := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&priv.PublicKey, digest[:], r, sv) {
		t.Error("ES256 signature did not verify")
	}
}

// Full RFC 8291 round-trip: encrypt as SOBS, decrypt as the subscriber would. Proves a real push
// service can read the payload.
func TestEncryptPushPayloadRoundTrip(t *testing.T) {
	subPriv, _ := ecdh.P256().GenerateKey(rand.Reader)
	subPub := subPriv.PublicKey().Bytes()
	authSecret := make([]byte, 16)
	_, _ = rand.Read(authSecret)

	out, err := encryptPushPayload([]byte("hello push"), subPub, authSecret)
	if err != nil {
		t.Fatal(err)
	}
	// Parse the aes128gcm header.
	if len(out) < 16+4+1+65+17 {
		t.Fatalf("payload too short: %d", len(out))
	}
	salt := out[0:16]
	idlen := int(out[20])
	if idlen != 65 {
		t.Fatalf("server key length = %d, want 65", idlen)
	}
	serverPub := out[21 : 21+idlen]
	ct := out[21+idlen:]

	// Decrypt as the subscriber.
	srvPubKey, err := ecdh.P256().NewPublicKey(serverPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := subPriv.ECDH(srvPubKey)
	authInfo := append([]byte("WebPush: info\x00"), subPub...)
	authInfo = append(authInfo, serverPub...)
	ikm := hkdfExpand(hkdfExtract(authSecret, shared), authInfo, 32)
	prk := hkdfExtract(salt, ikm)
	cek := hkdfExpand(prk, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hkdfExpand(prk, []byte("Content-Encoding: nonce\x00"), 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("subscriber decrypt failed: %v", err)
	}
	if string(pt) != "hello push\x02" { // 0x02 = last-record delimiter
		t.Errorf("decrypted = %q, want %q", pt, "hello push\x02")
	}
}

func TestDispatchChannelConfigErrors(t *testing.T) {
	s := &server{}
	obj := func(kv map[string]any) *jsonenc.Object {
		o := jsonenc.NewObject()
		for k, v := range kv {
			o.Set(k, v)
		}
		return o
	}
	if got := s.dispatchSlackChannel(jsonenc.NewObject(), "x"); got != "Slack webhook_url is not configured" {
		t.Errorf("slack: %q", got)
	}
	if got := s.dispatchEmailChannel(jsonenc.NewObject(), "x"); got != "Email to_addr is not configured" {
		t.Errorf("email: %q", got)
	}
	if got := s.dispatchBrowserPushChannel(obj(map[string]any{"endpoint": "https://x"}), "x"); got != "browser_push channel is missing endpoint, p256dh, or auth" {
		t.Errorf("push: %q", got)
	}
	if got := s.dispatchNotificationChannel("carrier-pigeon", "{}", "x"); got != "Unknown channel type: carrier-pigeon" {
		t.Errorf("unknown: %q", got)
	}
}

func TestDecodeB64URLPadded(t *testing.T) {
	want := []byte{0xde, 0xad, 0xbe, 0xef}
	for _, enc := range []string{"3q2-7w==", "3q2-7w"} { // urlsafe, padded and unpadded
		got, err := decodeB64URLPadded(enc)
		if err != nil || string(got) != string(want) {
			t.Errorf("decode(%q) = %x err %v", enc, got, err)
		}
	}
}
