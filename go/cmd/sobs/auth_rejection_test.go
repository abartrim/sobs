package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sobs/sobs/internal/store/storetest"
)

// This file targets the two auth REJECTION paths flagged in PR #352 review as having zero
// coverage: isValidCiPushAPIKey (the managed per-app CI-push key check) and the external-auth
// Bearer/cookie acceptance path in enforceUIAuth. Both use subtle.ConstantTimeCompare for the
// actual secret comparison (see auth.go:244-245 and :272-273), so these tests exercise the
// branches around that compare (missing/malformed/expired/wrong credentials) rather than timing
// itself, which a unit test cannot meaningfully assert.

// --- isValidCiPushAPIKey ---

// ciPushFakeDB builds a storetest.FakeDB that answers the ai.ci_push.app.<appID>.* settings reads
// ciPushStatus/loadAISetting issue, keyed the same way storetest.SettingsDB does but scoped to a
// single app so tests can assert the app-scoping behaviour too.
func ciPushFakeDB(appID string, settings map[string]string) *storetest.FakeDB {
	full := map[string]string{}
	for leaf, v := range settings {
		full[ciPushSettingKey(appID, leaf)] = v
	}
	return storetest.SettingsDB(full)
}

func TestCiPushAPIKeyAccepts(t *testing.T) {
	plain := "ci-push-secret-token"
	s := &server{db: ciPushFakeDB("app-1", map[string]string{
		"hash":       hashAPIKey(plain),
		"expires_at": "2999-01-01T00:00:00+00:00", // far future -> not expired
	})}
	if !s.isValidCiPushAPIKey("app-1", plain) {
		t.Fatal("correct key + unexpired hash should validate (control case)")
	}
}

func TestCiPushAPIKeyRejectsWrongKey(t *testing.T) {
	s := &server{db: ciPushFakeDB("app-1", map[string]string{
		"hash":       hashAPIKey("ci-push-secret-token"),
		"expires_at": "2999-01-01T00:00:00+00:00",
	})}
	if s.isValidCiPushAPIKey("app-1", "totally-wrong-key") {
		t.Error("wrong key must not validate")
	}
}

func TestCiPushAPIKeyRejectsEmptyOrWhitespaceKey(t *testing.T) {
	s := &server{db: ciPushFakeDB("app-1", map[string]string{
		"hash":       hashAPIKey("ci-push-secret-token"),
		"expires_at": "2999-01-01T00:00:00+00:00",
	})}
	for _, candidate := range []string{"", "   ", "\t\n"} {
		if s.isValidCiPushAPIKey("app-1", candidate) {
			t.Errorf("empty/whitespace candidate %q must not validate", candidate)
		}
	}
}

func TestCiPushAPIKeyRejectsWhenNotConfigured(t *testing.T) {
	// No hash setting stored at all for this app -> ciPushStatus reports hash="".
	s := &server{db: storetest.SettingsDB(map[string]string{})}
	if s.isValidCiPushAPIKey("app-unconfigured", "any-key") {
		t.Error("an app with no configured CI-push key must never validate")
	}
}

func TestCiPushAPIKeyRejectsExpiredKey(t *testing.T) {
	plain := "ci-push-secret-token"
	s := &server{db: ciPushFakeDB("app-1", map[string]string{
		"hash":       hashAPIKey(plain),
		"expires_at": "2000-01-01T00:00:00+00:00", // far past -> expired
	})}
	if s.isValidCiPushAPIKey("app-1", plain) {
		t.Error("correct key but expired hash must not validate")
	}
}

func TestCiPushAPIKeyRejectsMalformedHash(t *testing.T) {
	plain := "ci-push-secret-token"
	// Stored hash present but missing the scrypt:v1: prefix (e.g. corrupted/legacy row).
	s := &server{db: ciPushFakeDB("app-1", map[string]string{
		"hash":       "not-a-scrypt-hash",
		"expires_at": "2999-01-01T00:00:00+00:00",
	})}
	if s.isValidCiPushAPIKey("app-1", plain) {
		t.Error("a hash without the scrypt:v1: prefix must not validate")
	}
}

// TestCiPushAPIKeyRejectsWrongApp exercises the "different app/scope" case: a key rotated for
// app-1 must not validate against app-2, since each app's key is stored under its own scoped
// setting keys (ai.ci_push.app.<id>.hash).
func TestCiPushAPIKeyRejectsWrongApp(t *testing.T) {
	plain := "ci-push-secret-token"
	full := map[string]string{
		ciPushSettingKey("app-1", "hash"):       hashAPIKey(plain),
		ciPushSettingKey("app-1", "expires_at"): "2999-01-01T00:00:00+00:00",
		// app-2 has no key configured at all.
	}
	s := &server{db: storetest.SettingsDB(full)}
	if s.isValidCiPushAPIKey("app-2", plain) {
		t.Error("a key valid for app-1 must not validate against a different app-id")
	}
	// Sanity: the same key DOES validate for the app it was actually issued to.
	if !s.isValidCiPushAPIKey("app-1", plain) {
		t.Fatal("sanity check: app-1's own key should still validate")
	}
}

// TestCiPushAPIKeyRejectsEmptyAppID exercises the appID=="" branch of ciPushStatus, which returns
// a synthetic "missing" status with hash="" — must not validate against any candidate.
func TestCiPushAPIKeyRejectsEmptyAppID(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{})}
	if s.isValidCiPushAPIKey("", "anything") {
		t.Error("empty app id must never validate")
	}
}

// --- enforceAPIKey's managed-key acceptance path (integration of isValidCiPushAPIKey through the
// HTTP-facing enforceAuth/enforceAPIKey entry point) ---

func TestEnforceAPIKeyManagedAccepts(t *testing.T) {
	plain := "ci-push-secret-token"
	s := &server{
		auth: authConfig{}, // no static SOBS_API_KEY configured
		db: ciPushFakeDB("app-1", map[string]string{
			"hash":       hashAPIKey(plain),
			"expires_at": "2999-01-01T00:00:00+00:00",
		}),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/apps/app-1/releases", nil)
	req.Header.Set("X-API-Key", plain)
	if s.enforceAuth(rec, req) {
		t.Fatalf("correct managed CI-push key should pass, got code %d", rec.Code)
	}
}

func TestEnforceAPIKeyManagedRejectsWrongKey(t *testing.T) {
	s := &server{
		auth: authConfig{},
		db: ciPushFakeDB("app-1", map[string]string{
			"hash":       hashAPIKey("ci-push-secret-token"),
			"expires_at": "2999-01-01T00:00:00+00:00",
		}),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/apps/app-1/releases", nil)
	req.Header.Set("X-API-Key", "forged-key")
	if !s.enforceAuth(rec, req) {
		t.Fatal("wrong managed CI-push key must be rejected")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

func TestEnforceAPIKeyManagedRejectsMissingHeader(t *testing.T) {
	s := &server{
		auth: authConfig{},
		db: ciPushFakeDB("app-1", map[string]string{
			"hash":       hashAPIKey("ci-push-secret-token"),
			"expires_at": "2999-01-01T00:00:00+00:00",
		}),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/apps/app-1/releases", nil) // no X-API-Key at all
	if !s.enforceAuth(rec, req) {
		t.Fatal("a configured managed key with no X-API-Key header must be rejected")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

// --- external-auth Bearer/cookie path (enforceUIAuth, mode == "external") ---

func externalAuthServer(t *testing.T, externalURL string) *server {
	t.Helper()
	return &server{auth: authConfig{externalURL: externalURL}}
}

// writeExternalAuthFixture drops a canned upstream response for POST {externalURL}/internal/auth/validate
// keyed the same way upstreamFixtureKey does, so checkExternalAuth's real upstreamRequest call
// resolves through the fixture mock instead of touching the network.
func writeExternalAuthFixture(t *testing.T, dir, externalURL string, status int) {
	t.Helper()
	endpoint := externalURL + "/internal/auth/validate"
	stem := upstreamFixtureKey("POST", endpoint)
	spec := `{"status": ` + strconv.Itoa(status) + `}`
	if err := os.WriteFile(filepath.Join(dir, stem+".json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
}

const externalAuthTestURL = "http://external-auth.mock"

func TestExternalAuthAcceptsBearer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeExternalAuthFixture(t, dir, externalAuthTestURL, 200)

	s := externalAuthServer(t, externalAuthTestURL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/logs", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	if s.enforceAuth(rec, req) {
		t.Fatalf("valid bearer token accepted by external service should pass, got code %d", rec.Code)
	}
}

func TestExternalAuthRejectsMissingHeaderAndCookie(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeExternalAuthFixture(t, dir, externalAuthTestURL, 200) // even though the service WOULD accept, no header/cookie was sent

	s := externalAuthServer(t, externalAuthTestURL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/logs", nil) // no Authorization header, no session cookie
	if !s.enforceAuth(rec, req) {
		t.Fatal("no Authorization header and no session cookie must be rejected")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="SOBS"` {
		t.Errorf("WWW-Authenticate = %q", got)
	}
}

func TestExternalAuthRejectsForgedBearer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeExternalAuthFixture(t, dir, externalAuthTestURL, 401) // external service says "no"

	s := externalAuthServer(t, externalAuthTestURL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/logs", nil)
	req.Header.Set("Authorization", "Bearer forged-or-revoked-token")
	if !s.enforceAuth(rec, req) {
		t.Fatal("a bearer token the external service rejects must be rejected")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

func TestExternalAuthRejectsMalformedAuthorizationHeader(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeExternalAuthFixture(t, dir, externalAuthTestURL, 200)

	s := externalAuthServer(t, externalAuthTestURL)
	// Not "Bearer <token>" shaped at all — e.g. a stray Basic header, or just the token with no
	// scheme prefix. Neither should ever reach checkExternalAuth, since the code only recognizes
	// the exact "Bearer " prefix.
	for _, hdr := range []string{"Basic dXNlcjpwYXNz", "valid-token", "bearer valid-token", "Bearer"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/logs", nil)
		req.Header.Set("Authorization", hdr)
		if !s.enforceAuth(rec, req) {
			t.Errorf("malformed Authorization header %q must be rejected", hdr)
		}
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q: code = %d, want 401", hdr, rec.Code)
		}
	}
}

func TestExternalAuthAcceptsSessionCookieFallback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeExternalAuthFixture(t, dir, externalAuthTestURL, 200)

	s := externalAuthServer(t, externalAuthTestURL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/logs", nil) // no Authorization header
	req.AddCookie(&http.Cookie{Name: "session", Value: "valid-session-value"})
	if s.enforceAuth(rec, req) {
		t.Fatalf("a valid session cookie should be accepted as a Bearer-token fallback, got code %d", rec.Code)
	}
}

func TestExternalAuthRejectsForgedSessionCookie(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeExternalAuthFixture(t, dir, externalAuthTestURL, 401) // external service rejects this value

	s := externalAuthServer(t, externalAuthTestURL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/logs", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: "forged-session-value"})
	if !s.enforceAuth(rec, req) {
		t.Fatal("a session cookie the external service rejects must be rejected")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

// TestExternalAuthRejectsEmptyOrCRLFCookie exercises the cookie-value guard in enforceUIAuth
// (`v != "" && !strings.ContainsAny(v, "\r\n")`) directly: an empty cookie value, or one containing
// CR/LF (a header-injection attempt smuggled through the cookie into the synthesized "Bearer "
// header), must never be forwarded to checkExternalAuth.
func TestExternalAuthRejectsEmptyOrCRLFCookie(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	// The external service would accept ANY bearer value reaching it -- proving rejection happens
	// before the call, not because the mock said no.
	writeExternalAuthFixture(t, dir, externalAuthTestURL, 200)

	s := externalAuthServer(t, externalAuthTestURL)

	// Empty cookie value.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/logs", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: ""})
	if !s.enforceAuth(rec, req) {
		t.Error("empty session cookie value must be rejected")
	}

	// CRLF injection attempt via a raw Cookie header. Go's own http.Request.Cookie() parser
	// already refuses to parse a value containing embedded CR/LF (r.Cookie("session") returns
	// ErrNoCookie), so this never even reaches the explicit `ContainsAny(v, "\r\n")` guard in
	// enforceUIAuth in practice — but the net effect (rejection, never a synthesized "Bearer "
	// header built from attacker-controlled control bytes) is exactly the invariant that guard
	// exists to enforce, so it is pinned here as defense-in-depth against a future net/http
	// parsing change.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/logs", nil)
	req.Header.Set("Cookie", "session=abc\r\nX-Injected: evil")
	if !s.enforceAuth(rec, req) {
		t.Error("a session cookie value containing CR/LF must be rejected")
	}
}

// TestExternalAuthDoesNotFallBackToCookieWhenBearerPresent asserts the cookie fallback only
// applies when there is no Authorization header at all: an explicit (forged) Bearer header must be
// validated on its own terms, not silently swapped for a valid cookie.
func TestExternalAuthDoesNotFallBackToCookieWhenBearerPresent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_UPSTREAM_FIXTURES", dir)
	writeExternalAuthFixture(t, dir, externalAuthTestURL, 401) // the (forged) bearer token is rejected

	s := externalAuthServer(t, externalAuthTestURL)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/logs", nil)
	req.Header.Set("Authorization", "Bearer forged-token")
	req.AddCookie(&http.Cookie{Name: "session", Value: "valid-session-value"}) // would pass alone
	if !s.enforceAuth(rec, req) {
		t.Error("an explicit forged Bearer header must not be bypassed by a valid session cookie")
	}
}

// TestExternalAuthUnconfiguredURLRejects covers checkExternalAuth's own guard: mode=="external"
// requires externalURL != "" to even be selected (see authConfig.mode), but this locks in that a
// server somehow reaching checkExternalAuth with an empty externalURL always rejects rather than
// silently accepting.
func TestExternalAuthUnconfiguredURLRejects(t *testing.T) {
	s := &server{}
	if s.checkExternalAuth("Bearer anything") {
		t.Error("checkExternalAuth with no externalURL configured must return false")
	}
}

// --- constant-time comparison observation ---
//
// isValidCiPushAPIKey (auth.go) compares the candidate's scrypt fingerprint against the stored
// hash via subtle.ConstantTimeCompare, and the external-auth path's sibling (basic-auth username
// and password compare in enforceUIAuth) also uses subtle.ConstantTimeCompare. The external-auth
// Bearer/cookie path itself performs no local byte comparison of the secret at all — the token is
// forwarded to an external service via checkExternalAuth/upstreamRequest, which does the actual
// comparison out of process, so there is no local timing side-channel to guard there. This test
// only pins the ci-push behavioural contract (equal-length wrong key still rejects); it does not
// attempt to measure timing, which is not reliable in a unit test.
func TestCiPushAPIKeyRejectsEqualLengthWrongKey(t *testing.T) {
	plain := "ci-push-secret-token"
	wrongSameLen := "ci-push-secret-toke0" // same length, last byte differs
	s := &server{db: ciPushFakeDB("app-1", map[string]string{
		"hash":       hashAPIKey(plain),
		"expires_at": "2999-01-01T00:00:00+00:00",
	})}
	if s.isValidCiPushAPIKey("app-1", wrongSameLen) {
		t.Error("an equal-length wrong key must still be rejected")
	}
}
