package main

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ssePubSub is the /tail live-stream hub's interface, factored out of sseBroker so it can be
// swapped out in tests (see providers.go's newSSEBroker).
type ssePubSub interface {
	subscribe() chan string
	unsubscribe(chan string)
	broadcast(string)
}

// sseBroker is the in-process publish/subscribe hub for the /tail live stream — the Go analog
// of app.py's _sse_subscribers set + _sse_broadcast. Ingest handlers publish OTEL events; each
// /tail connection subscribes a buffered channel.
type sseBroker struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

// sseQueueMaxsize mirrors app.py _SSE_QUEUE_MAXSIZE = int(os.environ.get("SOBS_SSE_QUEUE_MAX", 200)).
var sseQueueMaxsize = envInt("SOBS_SSE_QUEUE_MAX", 200)

func (b *sseBroker) subscribe() chan string {
	ch := make(chan string, sseQueueMaxsize)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *sseBroker) unsubscribe(ch chan string) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// broadcast publishes a pre-serialized event JSON to every subscriber (non-blocking; a full
// queue drops the event, matching app.py's QueueFull handling).
func (b *sseBroker) broadcast(eventJSON string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- eventJSON:
		default:
		}
	}
}

// sseBroadcast builds the tail event payload (source/ts/service/...) and publishes it. A nil broker
// (servers constructed without the /tail hub, e.g. in unit tests) is a no-op.
func (s *server) sseBroadcast(fields *jsonenc.Object) {
	if s.sse == nil {
		return
	}
	s.sse.broadcast(string(jsonenc.Encode(fields, jsonenc.Options{SortKeys: false, EnsureASCII: false, ItemSep: ", ", KeySep: ": "})))
}

// GET /tail — app.py tail_stream: a Server-Sent Events stream of live logs/traces. Opens with
// `retry: 5000`, then streams `data:` frames as events arrive and `: keepalive` every 15s. The
// parity test reads only the deterministic opening frame (bounded stream read).
func (s *server) handleTail(w http.ResponseWriter, r *http.Request) {
	source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source")))
	if source == "" {
		source = "all"
	}
	serviceFilter := strings.TrimSpace(r.URL.Query().Get("service"))
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("retry: 5000\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
	ch := s.sse.subscribe()
	defer s.sse.unsubscribe(ch)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			if !sseEventMatches(ev, source, serviceFilter) {
				continue
			}
			if _, err := w.Write([]byte("data: " + ev + "\n\n")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-ticker.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// sseEventMatches mirrors the source/service filtering in app.py's _generate loop
// (app.py:24492-24495): `event.get("source") != source` / `event.get("service") != service_filter`,
// i.e. EXACT comparison against the event's dict values — NOT a substring search of the serialized
// frame. The old `strings.Contains(eventJSON, ...)` form could match the literal text appearing in
// any other field (e.g. a log body containing `"source": "logs"`) or fail to match when a value
// contained characters that altered the serialized form. We parse the event back to its structured
// form and compare the exact field values, reproducing Python's behavior.
func sseEventMatches(eventJSON, source, serviceFilter string) bool {
	if source == "all" && serviceFilter == "" {
		return true
	}
	ev := asObject(func() any { v, _ := parseJSONValue([]byte(eventJSON)); return v }())
	if source != "all" {
		// event.get("source") != source -> filtered out. A missing/non-string "source" key yields
		// "" here, which only equals an (impossible) empty source, so it is filtered out — matching
		// Python where event.get("source") would be None != source.
		if sseEventField(ev, "source") != source {
			return false
		}
	}
	if serviceFilter != "" {
		if sseEventField(ev, "service") != serviceFilter {
			return false
		}
	}
	return true
}

// sseEventField returns the event field as the string Python would compare against. The /tail
// event payloads always carry "source"/"service" as JSON strings, so a non-string (or absent)
// value compares unequal to any non-empty filter — the same outcome as Python's None != filter.
func sseEventField(ev *jsonenc.Object, key string) string {
	if v, ok := ev.Get(key); ok {
		if str, ok := v.(string); ok {
			return str
		}
	}
	return ""
}
