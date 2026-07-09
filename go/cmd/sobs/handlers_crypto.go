package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
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
		s.errorJSON(w, http.StatusInternalServerError, "failed to generate VAPID keys")
		return
	}
	pubBytes := elliptic.Marshal(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y) // 0x04||X||Y
	der, e := x509.MarshalPKCS8PrivateKey(priv)
	if e != nil {
		s.errorJSON(w, http.StatusInternalServerError, "failed to generate VAPID keys")
		return
	}
	if err := s.setAppSetting("vapid_private_key", base64.RawURLEncoding.EncodeToString(der)); err != nil {
		s.errorJSON(w, http.StatusInternalServerError, "failed to generate VAPID keys")
		return
	}
	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("env_override", false).
		Set("note", "New VAPID keys saved to the database. Keys are active immediately. "+
			"Existing browser subscriptions will need to re-subscribe.").
		Set("ok", true).
		Set("public_key", base64.RawURLEncoding.EncodeToString(pubBytes)).
		Set("saved_to_db", true))
}

// mcpAPIKeysCreate — POST /api/mcp/keys (mcp.py mcp_api_create_key): enforce the 20-key cap,
// mint "smcp_"+token_urlsafe(32) with a hex id, scrypt-hash the raw key, append the descriptor
// {id,label,key_hash,created_at,expires_at} to the mcp.api_keys keystore, and return
// id/key/label/created_at/expires_at. id+key are cryptographically random (masked in parity);
// created_at is the frozen clock; label defaults to "API Key"; expires_at honors the body (null
// when absent). The descriptor is persisted via _save_mcp_api_keys (json.dumps, ", " separators).
func (s *server) mcpAPIKeysCreate(w http.ResponseWriter, r *http.Request) {
	// _load_mcp_api_keys + the cap check precede body parsing in mcp.py.
	keys := s.loadMcpAPIKeys()
	if len(keys) >= mcpAPIKeyMax {
		writeJSON(w, http.StatusBadRequest, jsonenc.NewObject().
			Set("ok", false).
			Set("error", fmt.Sprintf("Maximum of %d keys reached.", mcpAPIKeyMax)))
		return
	}

	body := bodyMap(r)
	// label = str(body.get("label", "")).strip()[:128] or "API Key" — strip, then slice by CODE
	// POINTS (Python str slicing), then default. Rune-slice so a multibyte label isn't split mid-rune.
	label := bstr(body, "label")
	if r := []rune(label); len(r) > 128 {
		label = string(r[:128])
	}
	if label == "" {
		label = "API Key"
	}
	// expires_at = body.get("expires_at") — passed through verbatim (null when absent / JSON null).
	var expiresAt any = nil
	if v, ok := body["expires_at"]; ok {
		expiresAt = toEncodable(v)
	}
	// created_at = datetime.now(timezone.utc).isoformat().replace("+00:00", "Z").
	createdAt := nowUTC().Format("2006-01-02T15:04:05Z")

	rawKey := "smcp_" + base64.RawURLEncoding.EncodeToString(randBytes(32))
	keyID := hex.EncodeToString(randBytes(8))

	// Append the new descriptor (insertion order id,label,key_hash,created_at,expires_at) and
	// persist via _save_mcp_api_keys (json.dumps(keys, ensure_ascii=False)).
	descriptor := jsonenc.NewObject().
		Set("id", keyID).
		Set("label", label).
		Set("key_hash", hashMcpKey(rawKey)).
		Set("created_at", createdAt).
		Set("expires_at", expiresAt)
	keys = append(keys, any(descriptor))
	s.saveMcpAPIKeys(keys)

	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).
		Set("id", keyID).
		Set("key", rawKey).
		Set("label", label).
		Set("created_at", createdAt).
		Set("expires_at", expiresAt))
}
