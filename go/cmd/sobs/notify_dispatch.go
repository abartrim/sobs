package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/smtp"
	"net/url"
	"os"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Notification channel dispatch — faithful ports of app.py's _dispatch_slack_channel /
// _dispatch_email_channel / _dispatch_browser_push_channel. Real outbound HTTP/SMTP runs through
// s.upstreamRequest (mocked under parity, real http.Client at runtime) / net/smtp. Under parity
// only the webhook channel is exercised, so these branches don't affect the corpus.

const (
	vapidJWTExpirySeconds = 43200
	pushRecordSize        = 4096
)

func objStrDef(o *jsonenc.Object, key, def string) string {
	if v := strings.TrimSpace(objStrOr(o, key)); v != "" {
		return v
	}
	return def
}

// payloadSummary mirrors payload.get("summary", default): the value if the key is present
// (even empty), otherwise def.
func payloadSummary(payload *jsonenc.Object, def string) string {
	if v, ok := payload.Get("summary"); ok {
		return pyStrAny(v)
	}
	return def
}

// dispatchSlackChannel POSTs {"text": summary} to the Slack incoming-webhook URL.
func (s *server) dispatchSlackChannel(config *jsonenc.Object, payload *jsonenc.Object) string {
	webhookURL := objStrOr(config, "webhook_url")
	if webhookURL == "" {
		return "Slack webhook_url is not configured"
	}
	summary := payloadSummary(payload, "SOBS notification triggered")
	body := jsonenc.Encode(jsonenc.NewObject().Set("text", summary), dumpsDefault)
	resp, err := s.upstreamRequest("POST", webhookURL, body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err.Error()
	}
	if resp.Status >= 400 {
		return fmt.Sprintf("Slack webhook returned HTTP %d", resp.Status)
	}
	return "ok"
}

// emailUseTLS mirrors str(config.get("use_tls","1")).strip() in {"1","true","yes"} — STARTTLS is
// required (not opportunistic) when set; default-on only when the key is absent.
func emailUseTLS(config *jsonenc.Object) bool {
	raw := "1"
	if v, ok := config.Get("use_tls"); ok {
		raw = strings.TrimSpace(pyStrAny(v))
	}
	return raw == "1" || raw == "true" || raw == "yes"
}

// dispatchEmailChannel sends the notification via SMTP. Mirrors _dispatch_email_channel: the
// body is json.dumps(payload, indent=2) and STARTTLS is REQUIRED when use_tls is set (fails if
// the server doesn't offer it), matching Python's explicit server.starttls().
func (s *server) dispatchEmailChannel(config *jsonenc.Object, payload *jsonenc.Object) string {
	host := objStrDef(config, "smtp_host", "localhost")
	port := objStrDef(config, "smtp_port", "587")
	user := objStrOr(config, "smtp_user")
	password := objStrOr(config, "smtp_password")
	fromAddr := objStrDef(config, "from_addr", "sobs@localhost")
	toAddr := objStrOr(config, "to_addr")
	if toAddr == "" {
		return "Email to_addr is not configured"
	}
	subject := payloadSummary(payload, "SOBS Notification")
	if len(subject) > 200 {
		subject = subject[:200]
	}
	// json.dumps(payload, indent=2): MarshalIndent re-indents Object.MarshalJSON (ensure_ascii,
	// insertion order, no HTML escape).
	bodyText, _ := json.MarshalIndent(payload, "", "  ")
	msg := "Subject: " + subject + "\r\nFrom: " + fromAddr + "\r\nTo: " + toAddr +
		"\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + string(bodyText)
	return sendSMTPMessage(host+":"+port, host, emailUseTLS(config), user, password, fromAddr, toAddr, []byte(msg))
}

// sendSMTPMessage performs the SMTP exchange, requiring STARTTLS when useTLS is set.
func sendSMTPMessage(addr, host string, useTLS bool, user, password, from, to string, msg []byte) string {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err.Error()
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		return err.Error()
	}
	if useTLS {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err.Error()
		}
	}
	if user != "" && password != "" {
		if err := c.Auth(smtp.PlainAuth("", user, password, host)); err != nil {
			return err.Error()
		}
	}
	if err := c.Mail(from); err != nil {
		return err.Error()
	}
	if err := c.Rcpt(to); err != nil {
		return err.Error()
	}
	w, err := c.Data()
	if err != nil {
		return err.Error()
	}
	if _, err := w.Write(msg); err != nil {
		return err.Error()
	}
	if err := w.Close(); err != nil {
		return err.Error()
	}
	_ = c.Quit()
	return "ok"
}

// dispatchBrowserPushChannel sends a Web Push (VAPID) notification (RFC 8291/8188).
func (s *server) dispatchBrowserPushChannel(config *jsonenc.Object, payload *jsonenc.Object) string {
	summary := payloadSummary(payload, "")
	endpoint := objStrOr(config, "endpoint")
	p256dh := objStrOr(config, "p256dh")
	auth := objStrOr(config, "auth")
	if endpoint == "" || p256dh == "" || auth == "" {
		return "browser_push channel is missing endpoint, p256dh, or auth"
	}
	priv, _, err := s.loadVapidPrivateKey()
	if err != nil {
		return err.Error()
	}
	if priv == nil {
		return "VAPID private key is not configured — generate one on the Notifications settings page"
	}
	vapidSubject := envTrim("SOBS_VAPID_SUBJECT", "mailto:sobs@localhost")

	p256dhBytes, err := decodeB64URLPadded(p256dh)
	if err != nil {
		return "invalid p256dh"
	}
	authBytes, err := decodeB64URLPadded(auth)
	if err != nil {
		return "invalid auth"
	}

	u, _ := url.Parse(endpoint)
	audience := ""
	if u != nil {
		audience = u.Scheme + "://" + u.Host
	}
	now := nowUTC().Unix()
	// Claims order matches Python's dict literal (aud, exp, sub) so the signed bytes encode identically.
	jwtToken, err := buildVapidJWT(jsonenc.NewObject().
		Set("aud", audience).
		Set("exp", now+vapidJWTExpirySeconds).
		Set("sub", vapidSubject), priv)
	if err != nil {
		return err.Error()
	}
	vapidPub := elliptic.Marshal(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y) //nolint:staticcheck
	vapidPubB64 := base64.RawURLEncoding.EncodeToString(vapidPub)

	message, _ := json.Marshal(map[string]any{"title": "SOBS Alert", "body": summary})
	ciphertext, err := encryptPushPayload(message, p256dhBytes, authBytes)
	if err != nil {
		return err.Error()
	}
	headers := map[string]string{
		"Authorization":    "vapid t=" + jwtToken + ",k=" + vapidPubB64,
		"Content-Type":     "application/octet-stream",
		"Content-Encoding": "aes128gcm",
		"TTL":              "86400",
	}
	resp, err := s.upstreamRequest("POST", endpoint, ciphertext, headers)
	if err != nil {
		return err.Error()
	}
	switch resp.Status {
	case 200, 201, 202:
		return "ok"
	default:
		return fmt.Sprintf("Push service returned HTTP %d", resp.Status)
	}
}

// loadVapidPrivateKey resolves the VAPID EC private key: SOBS_VAPID_PRIVATE_KEY env wins, else the
// vapid_private_key app setting (how the keygen stores it: PKCS8 DER, base64url).
func (s *server) loadVapidPrivateKey() (*ecdsa.PrivateKey, string, error) {
	b64 := strings.TrimSpace(os.Getenv("SOBS_VAPID_PRIVATE_KEY"))
	source := "env"
	if b64 == "" {
		if v, ok := s.appSetting("vapid_private_key"); ok {
			b64, source = strings.TrimSpace(v), "db"
		}
	}
	if b64 == "" {
		return nil, "", nil
	}
	der, err := decodeB64URLPadded(b64)
	if err != nil {
		return nil, "", err
	}
	if k, e := x509.ParsePKCS8PrivateKey(der); e == nil {
		if ec, ok := k.(*ecdsa.PrivateKey); ok {
			return ec, source, nil
		}
	}
	if ec, e := x509.ParseECPrivateKey(der); e == nil {
		return ec, source, nil
	}
	if len(der) >= 32 { // raw scalar fallback (app.py derive_private_key path)
		curve := elliptic.P256()
		x, y := curve.ScalarBaseMult(der[:32])
		return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: new(big.Int).SetBytes(der[:32])}, source, nil
	}
	return nil, "", fmt.Errorf("could not parse VAPID private key")
}

// buildVapidJWT mirrors app.py _build_vapid_jwt: an ES256 JWT with raw r||s (64-byte) signature.
// The header/claims are encoded with json.dumps semantics — insertion order, ", "/": " separators,
// no HTML escaping (jsonenc dumpsDefault) — so the signed bytes match Python's encoding exactly
// (header literal {"typ": "JWT", "alg": "ES256"}; claims in aud/exp/sub order).
func buildVapidJWT(claims *jsonenc.Object, priv *ecdsa.PrivateKey) (string, error) {
	header := jsonenc.Encode(jsonenc.NewObject().Set("typ", "JWT").Set("alg", "ES256"), dumpsDefault)
	body := jsonenc.Encode(claims, dumpsDefault)
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(signingInput))
	r, sig, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return "", err
	}
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	sig.FillBytes(raw[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(raw), nil
}

// encryptPushPayload mirrors app.py _encrypt_push_payload (RFC 8291 aes128gcm content-encoding).
func encryptPushPayload(plaintext, subscriberPubKey, authSecret []byte) ([]byte, error) {
	serverPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serverPub := serverPriv.PublicKey().Bytes() // X9.62 uncompressed, 65 bytes
	subPub, err := ecdh.P256().NewPublicKey(subscriberPubKey)
	if err != nil {
		return nil, err
	}
	shared, err := serverPriv.ECDH(subPub)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}

	authInfo := append([]byte("WebPush: info\x00"), subscriberPubKey...)
	authInfo = append(authInfo, serverPub...)
	ikm := hkdfExpand(hkdfExtract(authSecret, shared), authInfo, 32)
	prk := hkdfExtract(salt, ikm)
	cek := hkdfExpand(prk, []byte("Content-Encoding: aes128gcm\x00"), 16)
	nonce := hkdfExpand(prk, []byte("Content-Encoding: nonce\x00"), 12)

	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, append(plaintext, 0x02), nil) // 0x02 = last-record delimiter

	header := make([]byte, 0, 16+4+1+len(serverPub))
	header = append(header, salt...)
	var rs [4]byte
	binary.BigEndian.PutUint32(rs[:], pushRecordSize)
	header = append(header, rs[:]...)
	header = append(header, byte(len(serverPub)))
	header = append(header, serverPub...)
	return append(header, ct...), nil
}

func hkdfExtract(salt, ikm []byte) []byte {
	h := hmac.New(sha256.New, salt)
	h.Write(ikm)
	return h.Sum(nil)
}

func hkdfExpand(prk, info []byte, length int) []byte {
	var out, t []byte
	for counter := byte(1); len(out) < length; counter++ {
		h := hmac.New(sha256.New, prk)
		h.Write(t)
		h.Write(info)
		h.Write([]byte{counter})
		t = h.Sum(nil)
		out = append(out, t...)
	}
	return out[:length]
}

// decodeB64URLPadded mirrors app.py _pad_base64 + urlsafe decode (tolerates padding / no padding).
func decodeB64URLPadded(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(s), "="))
}
