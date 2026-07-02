package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"testing"

	"github.com/sobs/sobs/internal/store/storetest"
)

// getVapidPublicKey derives the WebPush VAPID public key from the resolved private key (env
// takes precedence over the DB setting). The corpus never configures VAPID, so every branch here
// — env-sourced, DB-sourced, absent, and unparseable — is corpus-unreachable.
// Oracle: app.py _get_vapid_public_key / loadVapidPrivateKey's env-over-DB precedence.
func TestGetVapidPublicKey(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	b64 := base64.RawURLEncoding.EncodeToString(der)

	t.Run("env source wins over db", func(t *testing.T) {
		t.Setenv("SOBS_VAPID_PRIVATE_KEY", b64)
		s := &server{db: storetest.SettingsDB(map[string]string{"vapid_private_key": "should-be-ignored"})}
		pub, source := s.getVapidPublicKey()
		if source != "env" {
			t.Fatalf("source: got %q, want env", source)
		}
		raw, err := base64.RawURLEncoding.DecodeString(pub)
		if err != nil || len(raw) != 65 || raw[0] != 0x04 {
			t.Fatalf("public key should be a 65-byte uncompressed P256 point: len=%d err=%v", len(raw), err)
		}
	})

	t.Run("db source when env unset", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"vapid_private_key": b64})}
		pub, source := s.getVapidPublicKey()
		if source != "db" || pub == "" {
			t.Fatalf("got pub=%q source=%q, want a key from db", pub, source)
		}
	})

	t.Run("absent everywhere -> empty", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(nil)}
		pub, source := s.getVapidPublicKey()
		if pub != "" || source != "" {
			t.Fatalf("got pub=%q source=%q, want both empty", pub, source)
		}
	})

	t.Run("unparseable key -> empty", func(t *testing.T) {
		s := &server{db: storetest.SettingsDB(map[string]string{"vapid_private_key": "not-valid-base64url-key!!"})}
		pub, source := s.getVapidPublicKey()
		if pub != "" || source != "" {
			t.Fatalf("got pub=%q source=%q, want both empty on parse failure", pub, source)
		}
	})
}
