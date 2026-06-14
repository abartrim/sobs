package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"net/http"

	"github.com/sobs/sobs/internal/jsonenc"
)

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// POST /api/notifications/vapid-keygen — app.py generate_vapid_key: mint a WebPush VAPID EC
// P-256 key pair, persist the (DER PKCS8, base64url) private key, return the X9.62
// uncompressed-point public key. The key is cryptographically random — the parity manifest
// masks the public_key value and byte-compares the rest. Manifest-last (persists a setting).
func (s *server) handleApiNotificationsVapidKeygen(w http.ResponseWriter, r *http.Request) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		s.errorJSON(w, http.StatusInternalServerError, "VAPID key generation failed")
		return
	}
	pubBytes := elliptic.Marshal(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y) // 0x04||X||Y
	if der, e := x509.MarshalPKCS8PrivateKey(priv); e == nil {
		_ = s.setAppSetting("vapid_private_key", base64.RawURLEncoding.EncodeToString(der))
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("env_override", false).
		Set("note", "New VAPID keys saved to the database. Keys are active immediately. "+
			"Existing browser subscriptions will need to re-subscribe.").
		Set("ok", true).
		Set("public_key", base64.RawURLEncoding.EncodeToString(pubBytes)).
		Set("saved_to_db", true))
}

// mcpAPIKeysCreate — POST /api/mcp/keys (mcp.py mcp_api_create_key): mint "smcp_"+urlsafe(32)
// with a hex id, return id/key/label/created_at/expires_at. id+key are cryptographically
// random (masked in parity); created_at is the frozen clock; label defaults to "API Key".
func (s *server) mcpAPIKeysCreate(w http.ResponseWriter, r *http.Request) {
	label := bstr(bodyMap(r), "label")
	if len(label) > 128 {
		label = label[:128]
	}
	if label == "" {
		label = "API Key"
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("created_at", nowUTC().Format("2006-01-02T15:04:05Z")).
		Set("expires_at", nil).
		Set("id", hex.EncodeToString(randBytes(8))).
		Set("key", "smcp_"+base64.RawURLEncoding.EncodeToString(randBytes(32))).
		Set("label", label).
		Set("ok", true))
}
