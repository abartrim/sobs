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
		writeMethodGuard(w, r, g.allow)
	})
}

// writeMethodGuard emits Werkzeug's two automatic responses for a method the matched route does
// not serve: OPTIONS -> empty 200 + Allow (provide_automatic_options); any other -> 405 + sorted
// Allow + the default error page (MethodNotAllowed). Shared by the exact-route guard and the
// parameterized sub-router guard so both stay byte-identical.
func writeMethodGuard(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	if r.Method == http.MethodOptions {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(methodNotAllowed405Body)))
	w.WriteHeader(http.StatusMethodNotAllowed)
	_, _ = w.Write([]byte(methodNotAllowed405Body))
}

// exactMethodGuard applies the exact-route guard for a sub-router that internally serves a STATIC
// (non-parameterized) Werkzeug route — one that, in Python, Werkzeug prefers over a colliding
// parameterized rule. The sub-router calls it for a wrong method on that static sub-path so the
// 405/OPTIONS Allow lists the STATIC route's methods, not the param template's. Returns false
// (caller keeps its 404) when path is absent from the exact table or the method is allowed.
func exactMethodGuard(w http.ResponseWriter, r *http.Request, path string) bool {
	g, ok := routeMethods[path]
	if !ok || g.methods[r.Method] {
		return false
	}
	writeMethodGuard(w, r, g.allow)
	return true
}

// guardKey maps a Go ServeMux exact pattern to its Werkzeug path. Only the special root form
// "/{$}" (exact "/" match) differs from the literal route path.
func guardKey(pattern string) string {
	if pattern == "/{$}" {
		return "/"
	}
	return pattern
}

// paramRoute is one compiled parameterized template: its segments (with a per-segment "literal"
// flag) plus the precomputed allowed-method set and sorted Allow header.
type paramRoute struct {
	segs    []paramSeg
	allow   string
	methods map[string]bool
}

type paramSeg struct {
	literal bool   // true => the concrete segment must equal lit exactly
	lit     string // meaningful only when literal
}

// paramRoutes is rawParamRoutes (generated from the Werkzeug URL map) compiled into segment
// matchers once at startup. None of the templates use a <path:...> converter (those are excluded
// by the generator), so every template has fixed arity and matching is a segment-count + literal
// comparison. Werkzeug never picks a parameterized rule over a colliding static one, and the Go
// ServeMux mirrors that by routing the static paths to their own more-specific handlers before
// they ever reach a sub-router; so the only concrete paths that land here match at most one
// template (verified: no two templates of equal arity overlap).
var paramRoutes = buildParamRoutes()

func buildParamRoutes() []paramRoute {
	out := make([]paramRoute, 0, len(rawParamRoutes))
	for tmpl, allow := range rawParamRoutes {
		segs := make([]paramSeg, 0, 8)
		for _, p := range strings.Split(strings.Trim(tmpl, "/"), "/") {
			if strings.HasPrefix(p, "<") && strings.HasSuffix(p, ">") {
				segs = append(segs, paramSeg{literal: false})
			} else {
				segs = append(segs, paramSeg{literal: true, lit: p})
			}
		}
		methods := make(map[string]bool)
		for _, name := range strings.Split(allow, ", ") {
			methods[name] = true
		}
		out = append(out, paramRoute{segs: segs, allow: allow, methods: methods})
	}
	return out
}

// paramRouteFor matches a concrete request path against the compiled parameterized templates,
// returning the matched route. A concrete segment matches a literal template segment only on an
// exact string equality; a placeholder segment matches any single non-empty segment. ok is false
// when no template matches (the caller then keeps its own 404 behavior).
func paramRouteFor(path string) (paramRoute, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for _, pr := range paramRoutes {
		if len(pr.segs) != len(parts) {
			continue
		}
		match := true
		for i, seg := range pr.segs {
			if parts[i] == "" || (seg.literal && parts[i] != seg.lit) {
				match = false
				break
			}
		}
		if match {
			return pr, true
		}
	}
	return paramRoute{}, false
}

// paramMethodGuard reproduces, for the parameterized routes a Go prefix sub-router serves, the
// two automatic responses Werkzeug attaches to every rule. A sub-router calls it at the point
// where it would otherwise 404 a request. When the concrete path matches a param template it
// returns true after writing:
//   - OPTIONS              -> empty 200 + sorted Allow (provide_automatic_options), even though
//     OPTIONS is in the allowed set — Werkzeug answers it itself rather than running the view; and
//   - any disallowed method -> 405 + sorted Allow + the default error page (MethodNotAllowed).
//
// It returns false (leaving the sub-router's own routing intact) when the path matches no template
// or when the method is a non-OPTIONS method the template DOES serve (the sub-router handles it).
//
// Parity-safe by construction: the golden corpus only ever sends a template's declared, non-OPTIONS
// method to a matching concrete path and never sends OPTIONS/HEAD, so this guard is a no-op on
// every captured request (verified against the corpus param cases).
func paramMethodGuard(w http.ResponseWriter, r *http.Request) bool {
	pr, ok := paramRouteFor(r.URL.Path)
	if !ok {
		return false
	}
	if r.Method == http.MethodOptions {
		writeMethodGuard(w, r, pr.allow) // 200 + Allow
		return true
	}
	if pr.methods[r.Method] {
		return false // a real method the sub-router serves
	}
	writeMethodGuard(w, r, pr.allow) // 405 + Allow
	return true
}
