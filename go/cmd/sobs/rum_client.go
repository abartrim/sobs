package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// RUM client-token auth — a faithful port of app.py's optional origin-bound RUM client auth
// (RUM_CLIENT_AUTH_MODE / _SIGNING_KEY / _TOKEN_TTL_SEC). Tokens are `b64url(claims).hmacHex`,
// HMAC-SHA256 signed with the signing key, bound to an app + origin with a TTL. Mode "none" (the
// default, and what the parity corpus uses) disables issuance and makes ingest verification a
// strict pass-through, so behaviour is unchanged there.

type rumClientConfig struct {
	mode       string // none | origin | origin-session
	signingKey string
	ttlSec     int
}

func loadRumClientConfig() rumClientConfig {
	return rumClientConfig{
		mode:       strings.ToLower(strings.TrimSpace(envOr("SOBS_RUM_CLIENT_AUTH_MODE", "none"))),
		signingKey: os.Getenv("SOBS_RUM_CLIENT_SIGNING_KEY"),
		ttlSec:     envInt("SOBS_RUM_CLIENT_TOKEN_TTL_SEC", 900),
	}
}

func (s *server) rumClientSign(payload string) string {
	mac := hmac.New(sha256.New, []byte(s.rumClient.signingKey))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// rumClaims mirrors the Python claim dict field order (iss, app, origin, iat, exp, jti) so the
// compact JSON — and thus the signature — is self-consistent.
type rumClaims struct {
	Iss    string `json:"iss"`
	App    string `json:"app"`
	Origin string `json:"origin"`
	Iat    int64  `json:"iat"`
	Exp    int64  `json:"exp"`
	Jti    string `json:"jti"`
}

func (s *server) rumClientTokenEncode(c rumClaims) string {
	// app.py: json.dumps(claims, separators=(",", ":"), ensure_ascii=False) — compact, insertion
	// order (iss, app, origin, iat, exp, jti), and NO HTML escaping of <>& (stdlib json.Marshal
	// escapes those to < etc, which would diverge the signed payload for such app/origin
	// values). Use jsonenc.Compact (ensure_ascii=False, no HTML escaping) over an ordered object.
	obj := jsonenc.NewObject().
		Set("iss", c.Iss).Set("app", c.App).Set("origin", c.Origin).
		Set("iat", c.Iat).Set("exp", c.Exp).Set("jti", c.Jti)
	raw := jsonenc.Encode(obj, jsonenc.Compact)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + s.rumClientSign(payload)
}

// rumClientTokenDecode mirrors _rum_client_token_decode: verify the HMAC (constant-time, lowercased
// signature) then JSON-decode the payload to a claims map.
func (s *server) rumClientTokenDecode(token string) (map[string]any, string) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 2 {
		return nil, "Invalid RUM client token format"
	}
	payloadB64, signature := parts[0], strings.ToLower(parts[1])
	expected := s.rumClientSign(payloadB64)
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return nil, "Invalid RUM client token signature"
	}
	raw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, "Invalid RUM client token payload"
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil || claims == nil {
		return nil, "Invalid RUM client token payload"
	}
	return claims, ""
}

// verifyRumClientAuth mirrors _verify_rum_client_auth: returns (ok, httpStatus, message).
func (s *server) verifyRumClientAuth(events []any, r *http.Request) (bool, int, string) {
	mode := s.rumClient.mode
	switch mode {
	case "", "none", "off", "disabled":
		return true, 200, ""
	case "origin", "origin-session":
	default:
		return false, 500, "Invalid SOBS_RUM_CLIENT_AUTH_MODE"
	}
	if s.rumClient.signingKey == "" {
		return false, 503, "RUM client signing key is not configured"
	}

	token := strings.TrimSpace(r.Header.Get("X-SOBS-RUM-Token"))
	if token == "" {
		for _, ev := range events {
			if t := strings.TrimSpace(toStr(eventField(ev, "clientAuthToken"))); t != "" {
				token = t
				break
			}
		}
	}
	if token == "" {
		return false, 401, "Missing RUM client auth token"
	}

	claims, errMsg := s.rumClientTokenDecode(token)
	if claims == nil {
		return false, 401, errMsg
	}

	now := nowUTC().Unix()
	exp := int64(otlpFloat(claims["exp"]))
	if exp <= now {
		return false, 401, "RUM client token expired"
	}

	boundOrigin := normalizeOrigin(toStr(claims["origin"]))
	reqOrigin := requestOrigin(r)
	if boundOrigin == "" {
		return false, 401, "RUM client token missing origin binding"
	}
	if reqOrigin == "" {
		return false, 401, "Missing Origin/Referer for RUM client auth"
	}
	if reqOrigin != boundOrigin {
		return false, 401, "RUM client token origin mismatch"
	}

	boundApp := strings.TrimSpace(toStr(claims["app"]))
	if boundApp != "" {
		for _, ev := range events {
			eventApp := strings.TrimSpace(toStr(eventField(ev, "appName")))
			if eventApp != "" && eventApp != boundApp {
				return false, 401, "RUM client token app mismatch"
			}
		}
	}
	return true, 200, ""
}

// eventField reads a top-level field from a parsed RUM event, which may be either an ordered
// *jsonenc.Object (decodeOrdered) or a plain map[string]any. Returns nil when absent / not an
// object.
func eventField(ev any, key string) any {
	switch e := ev.(type) {
	case *jsonenc.Object:
		v, _ := e.Get(key)
		return v
	case map[string]any:
		return e[key]
	}
	return nil
}

// requestOrigin mirrors app.py _request_origin: normalized Origin, else the Referer's origin.
func requestOrigin(r *http.Request) string {
	if o := normalizeOrigin(r.Header.Get("Origin")); o != "" {
		return o
	}
	if u, err := url.Parse(strings.TrimSpace(r.Header.Get("Referer"))); err == nil && u.Scheme != "" && u.Host != "" {
		return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
	}
	return ""
}

func rumJTI() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "0"
	}
	return hex.EncodeToString(b)
}
