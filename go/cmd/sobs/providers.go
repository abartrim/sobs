package main

import (
	"net/http"

	"github.com/sobs/sobs/internal/store"
)

// Provider seams
// --------------
// These package-level function values are the single points at which the server's swappable
// backends are constructed. They default to the embedded, self-contained implementations; an
// alternate build can reassign them (e.g. in an init()) to inject a different store.DB without
// modifying newServer or any handler. Keeping this as a var — not a direct call — is what makes
// the store.DB interface (see internal/store/store.go) genuinely swappable, and it is the same
// seam the storetest fake uses in unit tests.

// openStore opens the process's store. The default opens the embedded chdb session at
// cfg.DataDir (mirroring app.py's ChDbConnection). newServer calls this through its open-retry
// loop, so any implementation may return a transient error to request a retry.
var openStore = func(cfg config) (store.DB, error) {
	return store.Open(cfg.DataDir)
}

// authGate runs the request auth gate before the router. It returns true when it has fully written
// a blocking response (401/403/500), in which case ServeHTTP short-circuits; otherwise the request
// proceeds. The default applies the built-in api-key / basic / external-URL enforcement
// (s.enforceAuth). An alternate build can reassign it to a provider that authenticates differently
// (e.g. validating a bearer token) and attaches identity/roles to the request context.
var authGate = func(s *server, w http.ResponseWriter, r *http.Request) bool {
	return s.enforceAuth(w, r)
}

// newWriteQueue constructs the process's background DB-writer (app.py's _ensure_write_worker),
// i.e. the queue every ingest route (logs/traces/metrics/rum/errors/ai) enqueues onto via
// enqueueWrite. The default is the embedded in-process channel+goroutine batcher (writequeue.go).
// An alternate build can reassign this (e.g. in an init()) to a different writeQueuer backend —
// for instance one that publishes to an external broker — without touching newServer or
// enqueueWrite.
var newWriteQueue = func(cfg config) writeQueuer {
	return newDefaultWriteQueue()
}

// newSSEBroker constructs the process's /tail live-stream pub/sub hub (see handlers_tail.go).
// A package-level var, like the other seams above, so it can be swapped out in tests.
var newSSEBroker = func() ssePubSub {
	return &sseBroker{subs: map[chan string]struct{}{}}
}
