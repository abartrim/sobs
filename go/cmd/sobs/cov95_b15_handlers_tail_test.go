package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// cov95_b15_handlers_tail_test.go — batch 15 coverage for cmd/sobs/handlers_tail.go:
//   sseEventMatches (116)  83.3%
//   handleTail (64)        66.7%

func TestSseEventMatches(t *testing.T) {
	cases := []struct {
		name          string
		eventJSON     string
		source        string
		serviceFilter string
		want          bool
	}{
		{"all/no-filter always matches", `{"source":"logs"}`, "all", "", true},
		{"matching source, no service filter", `{"source":"logs"}`, "logs", "", true},
		{"non-matching source", `{"source":"traces"}`, "logs", "", false},
		{"missing source key filtered out when source != all", `{"other":1}`, "logs", "", false},
		{"matching source and service", `{"source":"logs","service":"checkout"}`, "logs", "checkout", true},
		{"matching source, non-matching service", `{"source":"logs","service":"other"}`, "logs", "checkout", false},
		{"all source but service filter still applies", `{"source":"logs","service":"checkout"}`, "all", "checkout", true},
		{"all source, service filter mismatch", `{"source":"logs","service":"other"}`, "all", "checkout", false},
		{"non-string source field treated as absent", `{"source":123}`, "logs", "", false},
		{"invalid JSON -> parses to nil object, no match unless all+empty", `not json`, "logs", "", false},
		{"invalid JSON with all/no-filter still matches (short-circuit before parse matters)", `not json`, "all", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sseEventMatches(c.eventJSON, c.source, c.serviceFilter); got != c.want {
				t.Errorf("sseEventMatches(%q, %q, %q) = %v, want %v", c.eventJSON, c.source, c.serviceFilter, got, c.want)
			}
		})
	}
}

// TestHandleTail_OpeningFrame proves the handler writes the deterministic opening SSE frame and
// the expected headers before blocking on the event/keepalive/cancellation select loop.
func TestHandleTail_OpeningFrame(t *testing.T) {
	s := &server{sse: newSSEBroker()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/tail", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.handleTail(rec, r)
		close(done)
	}()

	// Give the handler a moment to write the opening frame and subscribe, then cancel the
	// request context so the handler returns (the <-r.Context().Done() branch).
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleTail did not return after context cancellation")
	}

	if rec.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering = %q", rec.Header().Get("X-Accel-Buffering"))
	}
	if !strings.Contains(rec.Body.String(), "retry: 5000\n\n") {
		t.Errorf("body missing opening retry frame: %q", rec.Body.String())
	}
}

// TestHandleTail_BroadcastEventReachesSubscriber drives a real broadcast through the live broker
// while /tail is connected, and asserts the matching event reaches the HTTP response body — the
// select's `case ev := <-ch` branch, including the sseEventMatches filter call.
func TestHandleTail_BroadcastEventReachesSubscriber(t *testing.T) {
	s := &server{sse: newSSEBroker()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/tail?source=logs&service=checkout", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.handleTail(rec, r)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)

	// A non-matching broadcast (wrong service) must be filtered out.
	s.sse.broadcast(`{"source":"logs","service":"other"}`)
	// A matching broadcast must appear in the stream.
	s.sse.broadcast(`{"source":"logs","service":"checkout","body":"hello"}`)
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleTail did not return after context cancellation")
	}

	body := rec.Body.String()
	if strings.Contains(body, `"service":"other"`) {
		t.Errorf("non-matching event leaked through: %s", body)
	}
	if !strings.Contains(body, `"service":"checkout","body":"hello"`) {
		t.Errorf("matching event missing from stream: %s", body)
	}
	if !strings.Contains(body, "data: ") {
		t.Errorf("expected an SSE 'data: ' frame, got: %s", body)
	}
}

// TestHandleTail_DefaultSourceIsAll proves an absent ?source query param defaults to "all" (every
// event passes the source filter, only the service filter — if any — applies).
func TestHandleTail_DefaultSourceIsAll(t *testing.T) {
	s := &server{sse: newSSEBroker()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/tail", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		s.handleTail(rec, r)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	s.sse.broadcast(`{"source":"traces","service":"anything"}`)
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	if !strings.Contains(rec.Body.String(), `"source":"traces"`) {
		t.Errorf("default source=all should pass every source through: %s", rec.Body.String())
	}
}
