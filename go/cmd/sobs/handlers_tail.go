package main

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sobs/sobs/internal/jsonenc"
)

// sseBroker is the in-process publish/subscribe hub for the /tail live stream — the Go analog
// of app.py's _sse_subscribers set + _sse_broadcast. Ingest handlers publish OTEL events; each
// /tail connection subscribes a buffered channel.
type sseBroker struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func newSSEBroker() *sseBroker { return &sseBroker{subs: map[chan string]struct{}{}} }

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

// sseBroadcast builds the tail event payload (source/ts/service/...) and publishes it.
func (s *server) sseBroadcast(fields *jsonenc.Object) {
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

// sseEventMatches mirrors the source/service filtering in app.py's _generate loop.
func sseEventMatches(eventJSON, source, serviceFilter string) bool {
	if source != "all" && !strings.Contains(eventJSON, `"source": "`+source+`"`) {
		return false
	}
	if serviceFilter != "" && !strings.Contains(eventJSON, `"service": "`+serviceFilter+`"`) {
		return false
	}
	return true
}
