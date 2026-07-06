package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// cov95_b16_route_guard_test.go — batch 16 targeted coverage for cmd/sobs/route_guard.go's
// route: the subtree (trailing "/") passthrough branch, the unknown-pattern passthrough branch,
// the auto-OPTIONS-answered branch (a route that does NOT declare OPTIONS explicitly), the
// explicit-OPTIONS-route passthrough-to-handler branch, and the allowed-method dispatch branch.

func newRouteGuardServer() *server {
	s := &server{mux: http.NewServeMux()}
	return s
}

func TestRouteSubtreePassesThroughUnguarded(t *testing.T) {
	s := newRouteGuardServer()
	called := false
	s.route("/static/", func(w http.ResponseWriter, r *http.Request) { called = true })
	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/static/anything", nil) // any method reaches h directly
	s.mux.ServeHTTP(w, r)
	if !called {
		t.Fatal("subtree pattern should register unguarded and dispatch directly to h")
	}
}

func TestRouteUnknownPatternPassesThroughUnguarded(t *testing.T) {
	s := newRouteGuardServer()
	called := false
	// "/healthz" is not in rawRouteAllow (per the package comment: a non-Python route).
	s.route("/healthz", func(w http.ResponseWriter, r *http.Request) { called = true })
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/healthz", nil)
	s.mux.ServeHTTP(w, r)
	if !called {
		t.Fatal("a pattern absent from routeMethods should register unguarded")
	}
}

func TestRouteAutoOptionsAnsweredWithoutInvokingHandler(t *testing.T) {
	s := newRouteGuardServer()
	called := false
	// "/health" = "GET, HEAD, OPTIONS" with no explicit OPTIONS declaration -> auto-answered.
	s.route("/health", func(w http.ResponseWriter, r *http.Request) { called = true })
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodOptions, "/health", nil)
	s.mux.ServeHTTP(w, r)
	if called {
		t.Fatal("auto-OPTIONS route must not invoke the real handler")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD, OPTIONS" {
		t.Errorf("Allow header = %q", got)
	}
	if w.Body.Len() != 0 {
		t.Errorf("want empty body, got %q", w.Body.String())
	}
}

func TestRouteExplicitOptionsRouteDispatchesToHandler(t *testing.T) {
	s := newRouteGuardServer()
	called := false
	// "/v1/logs" is in explicitOptionsRoutes -> OPTIONS must reach the real handler.
	s.route("/v1/logs", func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodOptions, "/v1/logs", nil)
	s.mux.ServeHTTP(w, r)
	if !called {
		t.Fatal("an explicit-OPTIONS route must dispatch OPTIONS to the real handler")
	}
	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204 from the real handler, got %d", w.Code)
	}
}

func TestRouteAllowedMethodDispatchesToHandler(t *testing.T) {
	s := newRouteGuardServer()
	called := false
	s.route("/health", func(w http.ResponseWriter, r *http.Request) { called = true })
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	s.mux.ServeHTTP(w, r)
	if !called {
		t.Fatal("an allowed method must dispatch to h")
	}
}

func TestRouteDisallowedMethodWrites405(t *testing.T) {
	s := newRouteGuardServer()
	called := false
	s.route("/health", func(w http.ResponseWriter, r *http.Request) { called = true })
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/health", nil) // POST not in "GET, HEAD, OPTIONS"
	s.mux.ServeHTTP(w, r)
	if called {
		t.Fatal("a disallowed method must not invoke h")
	}
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "GET, HEAD, OPTIONS" {
		t.Errorf("Allow header = %q", got)
	}
}
