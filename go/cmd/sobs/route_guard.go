package main

import (
	"net/http"
	"strconv"
	"strings"
)

// routeMethodSet is the expanded allowed-method membership for one exact route plus the
// precomputed, alphabetically-sorted Allow header Werkzeug emits on a 405 / OPTIONS.
type routeMethodSet struct {
	allow   string
	methods map[string]bool
}

// routeMethods is rawRouteAllow (generated from the Werkzeug URL map) expanded into membership
// sets once at startup.
var routeMethods = buildRouteMethods()

func buildRouteMethods() map[string]routeMethodSet {
	m := make(map[string]routeMethodSet, len(rawRouteAllow))
	for path, allow := range rawRouteAllow {
		set := make(map[string]bool)
		for _, name := range strings.Split(allow, ", ") {
			set[name] = true
		}
		m[path] = routeMethodSet{allow: allow, methods: set}
	}
	return m
}

// route registers h for pattern, wrapping it in a Werkzeug-faithful method guard when pattern
// is an exact route with a known allowed-method set. The guard reproduces Werkzeug's two
// automatic behaviors the bare ServeMux lacks: a wrong method yields 405 + sorted Allow + the
// default error page, and OPTIONS yields an empty 200 + Allow. Declared methods (incl. the
// auto HEAD where GET is allowed) pass straight through to h, so guarded routes keep their
// existing behavior for every method the route actually serves.
//
// Subtree patterns (trailing "/") and routes absent from the table register unguarded: the
// former are prefix sub-routers that dispatch methods per sub-path themselves; the latter are
// non-Python routes (e.g. /healthz) or parameterized routes handled by those sub-routers.
//
// Parity-safe by construction: the golden corpus never sends a method outside a route's
// declared set to any exact path (verified) and never sends OPTIONS/HEAD, so the guard is a
// no-op on every captured request.
func (s *server) route(pattern string, h http.HandlerFunc) {
	if strings.HasSuffix(pattern, "/") {
		s.mux.HandleFunc(pattern, h)
		return
	}
	g, ok := routeMethods[guardKey(pattern)]
	if !ok {
		s.mux.HandleFunc(pattern, h)
		return
	}
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if g.methods[r.Method] {
			h(w, r)
			return
		}
		if r.Method == http.MethodOptions {
			// Werkzeug provide_automatic_options: 200 with Allow and an empty body.
			w.Header().Set("Allow", g.allow)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		// Werkzeug MethodNotAllowed: 405 + sorted Allow + the default error page.
		w.Header().Set("Allow", g.allow)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(methodNotAllowed405Body)))
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(methodNotAllowed405Body))
	})
}

// guardKey maps a Go ServeMux exact pattern to its Werkzeug path. Only the special root form
// "/{$}" (exact "/" match) differs from the literal route path.
func guardKey(pattern string) string {
	if pattern == "/{$}" {
		return "/"
	}
	return pattern
}
