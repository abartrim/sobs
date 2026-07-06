package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// cov95_b12_telemetry_test.go — coverage-gate batch 12 for cmd/sobs/telemetry.go's OTLP export
// path: post() (0% coverage) and flush() (0% coverage), plus buildMetricPayload's histogram/sum
// branches. flushLoop itself is a `for range ticker.C { t.flush() }` background loop analogous to
// the other infinite-loop workers in this batch — it is exercised indirectly here by calling
// flush() directly (its per-iteration body), never by letting the ticker-driven loop run, so no
// goroutine leak / hang risk.
//
// post() does a plain http.Client POST to t.otlpEndpoint; per the task brief we stand up an
// httptest.NewServer and point a telemetry{otlpEndpoint: srv.URL, client: http.DefaultClient} at
// it to cover both the success and error paths (bad URL, non-2xx, server closed).

func TestTelemetryPostSuccess(t *testing.T) {
	var (
		mu        sync.Mutex
		gotPath   string
		gotMethod string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tel := &telemetry{otlpEndpoint: srv.URL, client: http.DefaultClient}
	tel.post("/v1/traces", map[string]any{"hello": "world"})

	mu.Lock()
	defer mu.Unlock()
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/traces" {
		t.Errorf("path = %q, want /v1/traces", gotPath)
	}
	if gotBody["hello"] != "world" {
		t.Errorf("body = %v, want hello=world", gotBody)
	}
}

func TestTelemetryPostTrimsTrailingSlashOnEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tel := &telemetry{otlpEndpoint: srv.URL + "/", client: http.DefaultClient}
	tel.post("/v1/metrics", map[string]any{})
	if gotPath != "/v1/metrics" {
		t.Errorf("path = %q, want /v1/metrics (single slash, no double)", gotPath)
	}
}

func TestTelemetryPostNon2xxIsSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	tel := &telemetry{otlpEndpoint: srv.URL, client: http.DefaultClient}
	// post has no return value; a non-2xx response must not panic and the body must be closed
	// (verified implicitly by the server not hanging / by go test's leak-free exit).
	tel.post("/v1/traces", map[string]any{"x": 1})
}

func TestTelemetryPostServerClosedIsSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close() // closed before use: client.Do must fail, and post must not panic

	tel := &telemetry{otlpEndpoint: srv.URL, client: http.DefaultClient}
	tel.post("/v1/traces", map[string]any{"x": 1})
}

func TestTelemetryPostBadURLIsSwallowed(t *testing.T) {
	// A control character in the endpoint makes http.NewRequest itself fail (invalid URL),
	// exercising the `req, err := http.NewRequest(...)` error branch distinctly from a client.Do
	// network failure.
	tel := &telemetry{otlpEndpoint: "http://\x7f", client: http.DefaultClient}
	tel.post("/v1/traces", map[string]any{"x": 1})
}

func TestTelemetryPostMarshalErrorIsSwallowed(t *testing.T) {
	// A payload json.Marshal cannot encode (e.g. a channel) hits the earliest error branch —
	// post must return without ever building a request.
	tel := &telemetry{otlpEndpoint: "http://example.invalid", client: http.DefaultClient}
	tel.post("/v1/traces", map[string]any{"bad": make(chan int)})
}

func TestTelemetryFlushSendsBufferedSpansAndPoints(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tel := &telemetry{
		enabled: true, exporter: "otlp", otlpEndpoint: srv.URL, client: http.DefaultClient,
		serviceName: "svc", environment: "test",
	}
	tel.spans = []map[string]any{{"name": "span1"}}
	tel.points = []map[string]any{
		{"name": "m.sum", "kind": "sum", "value": 1.0, "timeUnixNano": "1", "attributes": []map[string]any{}},
		{"name": "m.hist", "kind": "histogram", "value": 2.5, "timeUnixNano": "2", "attributes": []map[string]any{}},
		{"name": "m.gauge", "kind": "gauge", "value": 3.0, "timeUnixNano": "3", "attributes": []map[string]any{}},
	}

	tel.flush()

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 {
		t.Fatalf("flush posted to %d paths, want 2 (traces+metrics): %v", len(paths), paths)
	}
	foundTraces, foundMetrics := false, false
	for _, p := range paths {
		if p == "/v1/traces" {
			foundTraces = true
		}
		if p == "/v1/metrics" {
			foundMetrics = true
		}
	}
	if !foundTraces || !foundMetrics {
		t.Fatalf("flush paths = %v, want both /v1/traces and /v1/metrics", paths)
	}
	// Buffers must be drained after flush.
	tel.mu.Lock()
	defer tel.mu.Unlock()
	if tel.spans != nil || tel.points != nil {
		t.Fatalf("flush did not drain buffers: spans=%v points=%v", tel.spans, tel.points)
	}
}

func TestTelemetryFlushNoopWhenEmpty(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tel := &telemetry{otlpEndpoint: srv.URL, client: http.DefaultClient}
	tel.flush() // no spans/points buffered: post must never be invoked
	if called {
		t.Fatalf("flush() with empty buffers still issued an HTTP request")
	}
}
