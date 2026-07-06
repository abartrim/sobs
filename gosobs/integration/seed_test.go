package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// seed fires `total` mixed telemetry requests using `workers` goroutines so the
// UI pages render populated views for screenshots / UIQA. Unlike the Python
// scripts/load_example.py it does not simulate realistic distributions — the
// screenshot and UIQA assertions check page structure, not seeded values — so a
// small mixed generator is enough.
func seed(total, workers int) {
	if workers < 1 {
		workers = 1
	}
	client := &http.Client{Timeout: 8 * time.Second}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			seedOne(client, i)
		}(i)
	}
	wg.Wait()
}

func seedOne(client *http.Client, i int) {
	switch i % 5 {
	case 0:
		post(client, "/v1/logs", otlpLogPayload(fmt.Sprintf("seed log %d", i), "load-demo", "INFO"))
	case 1:
		traceID := fmt.Sprintf("%032x", uint64(i)*2654435761)
		spanID := fmt.Sprintf("%016x", uint32(i)*40503)
		post(client, "/v1/traces", otlpTracePayload("load-demo", []any{
			span(fmt.Sprintf("GET /load/%d", i), traceID, spanID, "", []any{
				kv("http.method", "GET"),
				kv("http.url", fmt.Sprintf("/load/%d", i)),
			}),
		}))
	case 2:
		post(client, "/v1/errors", map[string]any{
			"service": "load-demo",
			"type":    "RuntimeError",
			"message": fmt.Sprintf("seed error %d", i),
			"stack":   "RuntimeError: seed\n  at load (seed.go)",
		})
	case 3:
		post(client, "/v1/rum", []any{map[string]any{
			"type":      "pageview",
			"timestamp": "2026-01-01T00:00:00Z",
			"sessionId": fmt.Sprintf("seed-sess-%d", i),
			"url":       fmt.Sprintf("https://example.test/page/%d", i),
			"title":     fmt.Sprintf("Page %d", i),
		}})
	case 4:
		post(client, "/v1/ai", map[string]any{
			"service":     "load-demo",
			"provider":    "openai",
			"model":       "gpt-4o-mini",
			"prompt":      fmt.Sprintf("seed prompt %d", i),
			"response":    "seed response",
			"tokens_in":   10,
			"tokens_out":  5,
			"duration_ms": 120,
		})
	}
}

func post(client *http.Client, path string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	resp, err := client.Post(baseURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	resp.Body.Close()
}
