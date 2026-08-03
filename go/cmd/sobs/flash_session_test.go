package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store/storetest"
)

// flash_session_test.go covers the flashRedirect() -> subsequent GET round trip: a POST handler
// flashes a message into the sobs_session cookie and 302s, and the destination page's GET handler
// (renderPage/renderPageReq/handleHelpPage, via consumeSessionFlashes in handlers_pages.go) must
// render that message and clear the cookie — mirroring Quart's session-backed flash()/
// get_flashed_messages(). Regression test for the bug where plain page views only ever built their
// render engine with nil pre-seeded flashes (newEngine == newEngineFlash(r, nil)) and so never
// looked at the request's actual session cookie.

// flashProbeTemplate is a self-contained template (no {% extends %}, so it doesn't depend on
// base.html) that exercises exactly what get_flashed_messages surfaces, in the with_categories
// form the real base.html uses (see templates/base.html's `{% for category, message in messages %}`).
const flashProbeTemplate = `flashes=[{% for c, m in get_flashed_messages(with_categories=true) %}{{ c }}:{{ m }};{% endfor %}]`

func newFlashProbeServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe.html"), []byte(flashProbeTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	return &server{cfg: config{TemplateDir: dir}, db: &storetest.FakeDB{}}
}

// cookieHeaderValue extracts the "name=value" pair from a Set-Cookie header string (dropping the
// trailing attributes), suitable for use as a request's Cookie header.
func cookieHeaderValue(setCookie string) string {
	return strings.SplitN(setCookie, ";", 2)[0]
}

func TestRenderPage_FlashRedirectRoundTrip(t *testing.T) {
	s := newFlashProbeServer(t)

	// A POST handler's flashRedirect() (handlers_forms.go) sets the flash into the session cookie
	// ahead of the 302 - this is the write side, already covered by existing tests
	// (TestSliceI_formRequire_MissingFlashesAndRedirects pins flashSessionCookie's exact bytes).
	redirRec := httptest.NewRecorder()
	flashRedirect(redirRec, "success", "Notification channel created", "/settings/notifications")
	setCookie := redirRec.Header().Get("Set-Cookie")
	if setCookie == "" {
		t.Fatal("flashRedirect did not set a Set-Cookie header")
	}

	// The browser follows the redirect, sending that cookie back on the destination page's GET -
	// this is the read side under test.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/settings/notifications", nil)
	r.Header.Set("Cookie", cookieHeaderValue(setCookie))
	s.renderPage(w, r, "probe.html", "view_notifications", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "flashes=[success:Notification channel created;]") {
		t.Errorf("flash message from the redirect was not rendered on the next page view: %s", body)
	}

	// Rendering must consume the flash: Quart's get_flashed_messages() pops "_flashes" from the
	// session, marking it modified, so the response clears the now-empty session cookie.
	respCookie := w.Header().Get("Set-Cookie")
	if !strings.HasPrefix(respCookie, sessionCookieName+"=;") || !strings.Contains(respCookie, "Max-Age=0") {
		t.Errorf("expected the session cookie to be cleared after consuming its flash, got Set-Cookie=%q", respCookie)
	}
	if w.Header().Get("Vary") != "Cookie" {
		t.Errorf("expected Vary: Cookie on a response that read the session cookie, got %q", w.Header().Get("Vary"))
	}

	// A subsequent view (the cookie has now been cleared client-side) must show no flash and must
	// not touch Set-Cookie at all - most requests carry no session cookie and should pass through
	// untouched, not have one spuriously cleared every time.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/settings/notifications", nil)
	s.renderPage(w2, r2, "probe.html", "view_notifications", nil)
	if body2 := w2.Body.String(); !strings.Contains(body2, "flashes=[]") {
		t.Errorf("expected no flash on a clean request, got: %s", body2)
	}
	if sc := w2.Header().Get("Set-Cookie"); sc != "" {
		t.Errorf("expected no Set-Cookie header when there was no pending flash, got %q", sc)
	}
}

// TestRenderPageReq_FlashRedirectRoundTrip pins the same behavior for renderPageReq (the
// request.args-aware sibling renderPage delegates to for filtered pages).
func TestRenderPageReq_FlashRedirectRoundTrip(t *testing.T) {
	s := newFlashProbeServer(t)

	redirRec := httptest.NewRecorder()
	flashRedirect(redirRec, "warning", "Repository entry not found", "/settings/repositories")
	setCookie := redirRec.Header().Get("Set-Cookie")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/settings/repositories", nil)
	r.Header.Set("Cookie", cookieHeaderValue(setCookie))
	s.renderPageReq(w, r, "probe.html", "view_settings_repositories", nil)

	if body := w.Body.String(); !strings.Contains(body, "flashes=[warning:Repository entry not found;]") {
		t.Errorf("flash message not rendered via renderPageReq: %s", body)
	}
	if sc := w.Header().Get("Set-Cookie"); !strings.HasPrefix(sc, sessionCookieName+"=;") {
		t.Errorf("expected renderPageReq to clear the consumed flash cookie, got Set-Cookie=%q", sc)
	}
}

// TestHandleHelpPage_FlashRedirectRoundTrip pins the same behavior for the dynamically-registered
// help routes (render.go's handleHelpPage), which render full pages extending base.html directly
// rather than through renderPage.
func TestHandleHelpPage_FlashRedirectRoundTrip(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{}}

	redirRec := httptest.NewRecorder()
	flashRedirect(redirRec, "success", "Masking pattern added", "/settings/masking/help")
	setCookie := redirRec.Header().Get("Set-Cookie")

	h := s.handleHelpPage("view_masking_help", "masking_help.html")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/settings/masking/help", nil)
	r.Header.Set("Cookie", cookieHeaderValue(setCookie))
	h(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "Masking pattern added") {
		t.Errorf("flash message not rendered by handleHelpPage: missing text in body (%d bytes)", len(body))
	}
	if sc := w.Header().Get("Set-Cookie"); !strings.HasPrefix(sc, sessionCookieName+"=;") {
		t.Errorf("expected handleHelpPage to clear the consumed flash cookie, got Set-Cookie=%q", sc)
	}
}

// TestSessionFlashedMessages covers the cookie-decoding helper directly: the tagged-tuple shape
// flashSessionCookie/flashRedirectWithCiKey emit, malformed cookies, and the no-cookie case.
func TestSessionFlashedMessages(t *testing.T) {
	t.Run("decodes a real flash cookie", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Cookie", cookieHeaderValue(flashSessionCookie("danger", "Dashboard not found")))
		flashes, had := sessionFlashedMessages(r)
		if !had {
			t.Fatal("expected hadFlashes=true for a cookie carrying _flashes")
		}
		if len(flashes) != 1 {
			t.Fatalf("expected 1 flash, got %d: %v", len(flashes), flashes)
		}
		pair, ok := flashes[0].([]any)
		if !ok || len(pair) != 2 || pair[0] != "danger" || pair[1] != "Dashboard not found" {
			t.Errorf("flash pair = %#v, want [danger Dashboard not found]", flashes[0])
		}
	})

	t.Run("no cookie at all", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		flashes, had := sessionFlashedMessages(r)
		if had || flashes != nil {
			t.Errorf("expected (nil, false) with no cookie, got (%v, %v)", flashes, had)
		}
	})

	t.Run("cookie present but not a session cookie this port recognizes", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Cookie", sessionCookieName+"=not-valid-base64!!!")
		flashes, had := sessionFlashedMessages(r)
		if had || flashes != nil {
			t.Errorf("expected (nil, false) for an undecodable cookie, got (%v, %v)", flashes, had)
		}
	})

	t.Run("cookie shared with the CI-push key decodes both independently", func(t *testing.T) {
		// flashRedirectWithCiKey (handlers_forms.go) stashes _flashes and ci_push_api_key_plain_by_app
		// in the same session cookie; both readers must decode their own field from it.
		rec := httptest.NewRecorder()
		flashRedirectWithCiKey(rec, "success", "CI key rotated", "/settings/repositories", "app1", "plain-key")
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Cookie", cookieHeaderValue(rec.Header().Get("Set-Cookie")))

		flashes, had := sessionFlashedMessages(r)
		if !had {
			t.Fatal("expected hadFlashes=true: flashRedirectWithCiKey always sets _flashes alongside the ci key")
		}
		if len(flashes) != 1 {
			t.Fatalf("expected 1 flash, got %d: %v", len(flashes), flashes)
		}
		if byApp := sessionCiPushPlainByApp(r); byApp["app1"] != "plain-key" {
			t.Errorf("sessionCiPushPlainByApp = %v, want app1 -> plain-key", byApp)
		}
	})
}

// TestConsumeSessionFlashes_PreservesOtherSessionData is a regression test for consumeSessionFlashes
// clearing the ENTIRE session cookie whenever "_flashes" was present, instead of popping only that
// key the way Quart's session.pop("_flashes") does. flashRedirectWithCiKey stashes a one-time
// ci_push_api_key_plain_by_app entry alongside the flash in the same cookie; if a plain page view
// (not the repositories page) consumes the flash first, the CI key must still survive in the
// re-sent cookie for the repositories page to read afterwards.
func TestConsumeSessionFlashes_PreservesOtherSessionData(t *testing.T) {
	s := newFlashProbeServer(t)

	rec := httptest.NewRecorder()
	flashRedirectWithCiKey(rec, "success", "CI key rotated", "/settings/repositories", "app1", "plain-key-xyz")
	setCookie := rec.Header().Get("Set-Cookie")

	// A different page view (not /settings/repositories) reads this cookie first and consumes the
	// flash via the generic renderPage path.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/settings/notifications", nil)
	r.Header.Set("Cookie", cookieHeaderValue(setCookie))
	s.renderPage(w, r, "probe.html", "view_notifications", nil)

	if body := w.Body.String(); !strings.Contains(body, "flashes=[success:CI key rotated;]") {
		t.Fatalf("expected the flash to render, got: %s", body)
	}
	respCookie := w.Header().Get("Set-Cookie")
	if respCookie == "" {
		t.Fatal("expected a re-sent Set-Cookie carrying the surviving ci_push_api_key_plain_by_app entry")
	}
	if strings.HasPrefix(respCookie, sessionCookieName+"=;") {
		t.Fatalf("consumeSessionFlashes cleared the WHOLE session cookie instead of just \"_flashes\" — the "+
			"one-time CI-push key was destroyed before the repositories page ever read it: %q", respCookie)
	}

	// The CI-push key must still be readable from the re-sent cookie on the next request...
	r2 := httptest.NewRequest(http.MethodGet, "/settings/repositories", nil)
	r2.Header.Set("Cookie", cookieHeaderValue(respCookie))
	if byApp := sessionCiPushPlainByApp(r2); byApp["app1"] != "plain-key-xyz" {
		t.Errorf("ci-push key lost after consuming the flash: sessionCiPushPlainByApp = %v, want app1 -> plain-key-xyz", byApp)
	}
	// ...and the flash must NOT still be pending (it was already consumed and popped).
	if flashes, had := sessionFlashedMessages(r2); had || len(flashes) != 0 {
		t.Errorf("expected the flash to have been fully consumed already, got had=%v flashes=%v", had, flashes)
	}
}

// TestRenderPageFlash_MergesPendingSessionFlash is a regression test for renderPageFlash silently
// discarding a real pending flash (set by an earlier flashRedirect()) whenever the destination GET
// happens to take a renderPageFlash branch (e.g. handleViewTagRules's "edit_rule not found" path)
// instead of renderPage. Quart's flash()+get_flashed_messages() would show BOTH the pre-existing
// and the newly-flashed message; renderPageFlash must do the same.
func TestRenderPageFlash_MergesPendingSessionFlash(t *testing.T) {
	s := newFlashProbeServer(t)

	redirRec := httptest.NewRecorder()
	flashRedirect(redirRec, "success", "Tag rule created", "/settings/tags")
	setCookie := redirRec.Header().Get("Set-Cookie")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/settings/tags?edit_rule=stale-id", nil)
	r.Header.Set("Cookie", cookieHeaderValue(setCookie))
	s.renderPageFlash(w, r, "probe.html", "view_tag_rules", "warning", "Tag rule not found for editing", nil)

	body := w.Body.String()
	if !strings.Contains(body, "success:Tag rule created;") {
		t.Errorf("the pending flash from the earlier flashRedirect was discarded, got: %s", body)
	}
	if !strings.Contains(body, "warning:Tag rule not found for editing;") {
		t.Errorf("the explicit renderPageFlash message is missing, got: %s", body)
	}
	if sc := w.Header().Get("Set-Cookie"); !strings.HasPrefix(sc, sessionCookieName+"=;") {
		t.Errorf("expected renderPageFlash to clear the now-empty session cookie, got Set-Cookie=%q", sc)
	}
}

// TestConsumeSessionFlashes_MergesVaryHeader confirms consumeSessionFlashes appends "Cookie" to any
// Vary value already present on the response instead of clobbering it via a raw Set().
func TestConsumeSessionFlashes_MergesVaryHeader(t *testing.T) {
	s := newFlashProbeServer(t)
	w := httptest.NewRecorder()
	w.Header().Set("Vary", "Origin")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Cookie", cookieHeaderValue(flashSessionCookie("info", "hi")))

	s.consumeSessionFlashes(w, r)

	if got := w.Header().Get("Vary"); got != "Origin, Cookie" {
		t.Errorf("Vary = %q, want \"Origin, Cookie\" (merged, not clobbered)", got)
	}
}

// TestSessionCookieSigning covers signSessionPayload/verifySessionCookiePayload directly: this is
// the fix for the widened attack surface flagged in review — decodeSessionCookie now verifies an
// HMAC signature (keyed by sessionSecretKey) before trusting ANY session cookie content, so a
// client without the server's secret key cannot forge a flash message or a ci_push_api_key entry.
func TestSessionCookieSigning(t *testing.T) {
	t.Run("valid round trip", func(t *testing.T) {
		signed := signSessionPayload("cGF5bG9hZA")
		payload, ok := verifySessionCookiePayload(signed)
		if !ok || payload != "cGF5bG9hZA" {
			t.Fatalf("verifySessionCookiePayload(%q) = (%q, %v), want (cGF5bG9hZA, true)", signed, payload, ok)
		}
	})

	t.Run("tampered payload is rejected", func(t *testing.T) {
		signed := signSessionPayload("cGF5bG9hZA")
		i := strings.LastIndexByte(signed, '.')
		forged := "ZXZpbC1wYXlsb2Fk" + signed[i:] // attacker's own payload + the ORIGINAL signature
		if _, ok := verifySessionCookiePayload(forged); ok {
			t.Error("a payload swapped in under someone else's signature must not verify")
		}
	})

	t.Run("tampered signature is rejected", func(t *testing.T) {
		signed := signSessionPayload("cGF5bG9hZA")
		if _, ok := verifySessionCookiePayload(signed + "AAAA"); ok {
			t.Error("an altered signature must not verify")
		}
	})

	t.Run("no signature segment at all is rejected", func(t *testing.T) {
		if _, ok := verifySessionCookiePayload("cGF5bG9hZA"); ok {
			t.Error("a bare payload with no \".signature\" suffix must not verify")
		}
	})

	t.Run("a hand-forged flash cookie (no valid signature) is never trusted", func(t *testing.T) {
		// Build the exact JSON payload flashSessionCookie would, but WITHOUT going through
		// signSessionPayload — simulating an attacker who knows the wire format but not the secret.
		forgedPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"_flashes":[{" t":["danger","you have been hacked"]}]}`))
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Cookie", sessionCookieName+"="+forgedPayload+".0.0") // the OLD placeholder shape
		if flashes, had := sessionFlashedMessages(r); had || len(flashes) != 0 {
			t.Errorf("forged cookie without a valid signature was trusted: had=%v flashes=%v", had, flashes)
		}
	})
}

// TestHandleViewSettingsRepositories_CiKeyAndFlashTogether exercises the real handler end to end:
// a rotation's flashRedirectWithCiKey cookie must still deliver BOTH the one-time CI-push plaintext
// key AND the flash message on the very next repositories-page GET, now that the handler decodes
// the session cookie once (decodeSessionCookie) and derives ciPushPlainByAppFromSession/
// consumeSessionFlashesFrom from that single decode instead of two independent ones.
func TestHandleViewSettingsRepositories_CiKeyAndFlashTogether(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{}}

	rec := httptest.NewRecorder()
	flashRedirectWithCiKey(rec, "success", "CI push key rotated", "/settings/repositories", "app1", "plain-secret-key")
	setCookie := rec.Header().Get("Set-Cookie")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/settings/repositories", nil)
	r.Header.Set("Cookie", cookieHeaderValue(setCookie))
	s.handleViewSettingsRepositories(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "CI push key rotated") {
		t.Errorf("flash message missing from repositories page render: %d bytes", len(body))
	}
	// handleViewSettingsRepositories only READS ciPushPlainByAppFromSession (mirroring app.py's
	// session.pop, but this port never actually pops it — a pre-existing gap, not introduced here),
	// so that entry is still in the session after "_flashes" is popped: the cookie must be
	// RE-SENT with it intact, not fully cleared.
	sc := w.Header().Get("Set-Cookie")
	if strings.HasPrefix(sc, sessionCookieName+"=;") {
		t.Errorf("expected the ci_push_api_key_plain_by_app entry to survive popping \"_flashes\", "+
			"but the whole cookie was cleared: %q", sc)
	}
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Cookie", cookieHeaderValue(sc))
	if byApp := sessionCiPushPlainByApp(r2); byApp["app1"] != "plain-secret-key" {
		t.Errorf("ci-push key lost from the re-sent cookie: sessionCiPushPlainByApp = %v, want app1 -> plain-secret-key", byApp)
	}
	if flashes, had := sessionFlashedMessages(r2); had || len(flashes) != 0 {
		t.Errorf("expected the flash to already be consumed in the re-sent cookie, got had=%v flashes=%v", had, flashes)
	}
}
