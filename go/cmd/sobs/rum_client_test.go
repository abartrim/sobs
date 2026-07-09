package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func rumServer(mode, key string) *server {
	return &server{rumClient: rumClientConfig{mode: mode, signingKey: key, ttlSec: 900}}
}

func rumReq(origin, token string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/rum", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if token != "" {
		r.Header.Set("X-SOBS-RUM-Token", token)
	}
	return r
}

func TestRumClientDisabledPassthrough(t *testing.T) {
	s := rumServer("none", "")
	ok, status, _ := s.verifyRumClientAuth([]any{map[string]any{}}, rumReq("", ""))
	if !ok || status != 200 {
		t.Errorf("disabled mode should pass through (ok=%v status=%d)", ok, status)
	}
}

func TestRumClientTokenRoundTrip(t *testing.T) {
	s := rumServer("origin", "supersecret")
	now := nowUTC().Unix()
	token := s.rumClientTokenEncode(rumClaims{Iss: "sobs-rum", App: "shop", Origin: "https://shop.example.com", Iat: now, Exp: now + 900, Jti: "abc"})
	claims, errMsg := s.rumClientTokenDecode(token)
	if claims == nil || errMsg != "" {
		t.Fatalf("decode failed: %q", errMsg)
	}
	if claims["origin"] != "https://shop.example.com" || claims["app"] != "shop" {
		t.Errorf("claims = %v", claims)
	}
	// Tampered signature must fail. Flip the final hex digit to a guaranteed-different value —
	// the signature is lowercase hex, so a fixed "0" would be a no-op (and thus a false pass)
	// whenever the real last digit is already '0'.
	repl := byte('0')
	if token[len(token)-1] == '0' {
		repl = '1'
	}
	if _, e := s.rumClientTokenDecode(token[:len(token)-1] + string(repl)); e == "" {
		t.Error("tampered token should fail to decode")
	}
}

func TestRumClientVerifyMatrix(t *testing.T) {
	s := rumServer("origin", "supersecret")
	now := nowUTC().Unix()
	good := s.rumClientTokenEncode(rumClaims{Iss: "sobs-rum", App: "shop", Origin: "https://shop.example.com", Iat: now, Exp: now + 900, Jti: "j"})
	expired := s.rumClientTokenEncode(rumClaims{Iss: "sobs-rum", App: "shop", Origin: "https://shop.example.com", Iat: now - 1000, Exp: now - 1, Jti: "j"})

	cases := []struct {
		name   string
		events []any
		req    *http.Request
		ok     bool
		status int
	}{
		{"valid", []any{map[string]any{"appName": "shop"}}, rumReq("https://shop.example.com", good), true, 200},
		{"missing token", []any{map[string]any{}}, rumReq("https://shop.example.com", ""), false, 401},
		{"origin mismatch", []any{}, rumReq("https://evil.com", good), false, 401},
		{"expired", []any{}, rumReq("https://shop.example.com", expired), false, 401},
		{"app mismatch", []any{map[string]any{"appName": "other"}}, rumReq("https://shop.example.com", good), false, 401},
		{"missing origin header", []any{}, rumReq("", good), false, 401},
	}
	for _, c := range cases {
		ok, status, msg := s.verifyRumClientAuth(c.events, c.req)
		if ok != c.ok || status != c.status {
			t.Errorf("%s: ok=%v status=%d msg=%q (want ok=%v status=%d)", c.name, ok, status, msg, c.ok, c.status)
		}
	}

	// Token carried in an event body instead of the header.
	evToken := []any{map[string]any{"appName": "shop", "clientAuthToken": good}}
	if ok, status, _ := s.verifyRumClientAuth(evToken, rumReq("https://shop.example.com", "")); !ok || status != 200 {
		t.Errorf("event-carried token should pass (ok=%v status=%d)", ok, status)
	}
}

func TestRumClientVerifyConfigErrors(t *testing.T) {
	// origin mode but no signing key -> 503
	if ok, status, _ := rumServer("origin", "").verifyRumClientAuth([]any{}, rumReq("https://x", "")); ok || status != 503 {
		t.Errorf("no signing key should 503, got %d", status)
	}
	// invalid mode -> 500
	if ok, status, _ := rumServer("bogus", "k").verifyRumClientAuth([]any{}, rumReq("https://x", "")); ok || status != 500 {
		t.Errorf("invalid mode should 500, got %d", status)
	}
}
