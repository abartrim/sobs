package integration

import (
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// TestCurlExamples — simulate curl_examples.sh posting telemetry.
// ---------------------------------------------------------------------------

func testCurlExamples(t *testing.T) {
	t.Run("curl_log", func(t *testing.T) {
		status, body := postJSON(t, "/v1/logs", otlpLogPayload("Hello from curl!", "curl-demo", "INFO"))
		mustStatus(t, status, 200)
		mustAccepted(t, body, 1)
	})
	t.Run("curl_trace", func(t *testing.T) {
		status, body := postJSON(t, "/v1/traces", otlpTracePayload("curl-demo", []any{
			span("curl-span", "abcdef1234567890abcdef1234567890", "1234567890abcdef", "", nil),
		}))
		mustStatus(t, status, 200)
		mustAccepted(t, body, 1)
	})
	t.Run("curl_error", func(t *testing.T) {
		status, body := postJSON(t, "/v1/errors", map[string]any{
			"service": "curl-demo",
			"type":    "RuntimeError",
			"message": "Oops, something went wrong",
			"stack":   "RuntimeError: Oops\n  at main (script.sh:42)",
		})
		mustStatus(t, status, 200)
		mustOK(t, body)
	})
	t.Run("curl_rum", func(t *testing.T) {
		status, body := postJSON(t, "/v1/rum", []any{map[string]any{
			"type":      "pageview",
			"timestamp": "2024-01-01T00:00:00Z",
			"sessionId": "sess-abc123",
			"url":       "https://example.com/home",
			"title":     "Home Page",
		}})
		mustStatus(t, status, 200)
		mustAccepted(t, body, 1)
	})
	t.Run("curl_ai", func(t *testing.T) {
		status, body := postJSON(t, "/v1/ai", map[string]any{
			"service":     "curl-demo",
			"provider":    "openai",
			"model":       "gpt-4o-mini",
			"prompt":      "What is the capital of France?",
			"response":    "Paris.",
			"tokens_in":   10,
			"tokens_out":  2,
			"duration_ms": 250,
		})
		mustStatus(t, status, 200)
		mustOK(t, body)
	})
}

// ---------------------------------------------------------------------------
// TestPythonOtelExample — OTLP HTTP traces + logs.
// ---------------------------------------------------------------------------

func testPythonOtelExample(t *testing.T) {
	const service = "my-python-app"
	const traceID = "aabbccdd11223344aabbccdd11223344"

	t.Run("otel_traces", func(t *testing.T) {
		status, body := postJSON(t, "/v1/traces", otlpTracePayload(service, []any{
			span("handle_request", traceID, "cafebabe12345678", "", []any{
				kv("user.id", "user-123"),
				kv("http.method", "GET"),
				kv("http.url", "/api/users"),
			}),
			span("db_query", traceID, "deadbeef87654321", "cafebabe12345678", nil),
		}))
		mustStatus(t, status, 200)
		mustAccepted(t, body, 2)
	})

	t.Run("otel_logs", func(t *testing.T) {
		payload := map[string]any{
			"resourceLogs": []any{map[string]any{
				"resource": map[string]any{"attributes": []any{kv("service.name", service)}},
				"scopeLogs": []any{map[string]any{
					"logRecords": []any{
						map[string]any{
							"timeUnixNano": tsNs(),
							"severityText": "INFO",
							"body":         map[string]any{"stringValue": "Handling request for user user-123"},
							"traceId":      traceID,
							"spanId":       "cafebabe12345678",
						},
						map[string]any{
							"timeUnixNano": tsNs(),
							"severityText": "DEBUG",
							"body":         map[string]any{"stringValue": "Querying database"},
						},
						map[string]any{
							"timeUnixNano": tsNs(),
							"severityText": "INFO",
							"body":         map[string]any{"stringValue": "Request completed"},
						},
					},
				}},
			}},
		}
		status, body := postJSON(t, "/v1/logs", payload)
		mustStatus(t, status, 200)
		mustAccepted(t, body, 3)
	})
}

// ---------------------------------------------------------------------------
// TestFlaskExample — Flask example routes posting to SOBS.
// ---------------------------------------------------------------------------

func testFlaskExample(t *testing.T) {
	const service = "flask-demo"

	t.Run("flask_index_log_and_trace", func(t *testing.T) {
		status, _ := postJSON(t, "/v1/traces", otlpTracePayload(service, []any{
			span("GET /", "f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6", "a1b2c3d4e5f60001", "", []any{
				kv("http.method", "GET"),
				kv("http.route", "/"),
				intKV("http.status_code", 200),
			}),
		}))
		mustStatus(t, status, 200)
		status, _ = postJSON(t, "/v1/logs", otlpLogPayload("Root endpoint called", service, "INFO"))
		mustStatus(t, status, 200)
	})

	t.Run("flask_error_route", func(t *testing.T) {
		status, body := postJSON(t, "/v1/errors", map[string]any{
			"service": service,
			"type":    "ZeroDivisionError",
			"message": "division by zero",
			"stack":   "ZeroDivisionError('division by zero')",
		})
		mustStatus(t, status, 200)
		mustOK(t, body)
	})

	t.Run("flask_ai_demo_route", func(t *testing.T) {
		status, body := postJSON(t, "/v1/ai", map[string]any{
			"service":     service,
			"provider":    "openai",
			"model":       "gpt-4o-mini",
			"prompt":      "Summarise the user's request in one sentence.",
			"response":    "The user wants a summary of their request.",
			"tokens_in":   12,
			"tokens_out":  10,
			"duration_ms": 100,
		})
		mustStatus(t, status, 200)
		mustOK(t, body)
	})
}

// ---------------------------------------------------------------------------
// TestNodeJsExample — Express example routes posting to SOBS.
// ---------------------------------------------------------------------------

func testNodeJsExample(t *testing.T) {
	const service = "node-demo"

	t.Run("nodejs_root_trace", func(t *testing.T) {
		status, body := postJSON(t, "/v1/traces", otlpTracePayload(service, []any{
			span("GET /", "01020304050607080910111213141516", "1122334455667700", "", []any{
				kv("http.method", "GET"),
				kv("http.route", "/"),
				intKV("http.status_code", 200),
			}),
		}))
		mustStatus(t, status, 200)
		mustAccepted(t, body, 1)
	})

	t.Run("nodejs_error_route", func(t *testing.T) {
		status, body := postJSON(t, "/v1/errors", map[string]any{
			"service": service,
			"type":    "Error",
			"message": "Something went wrong",
			"stack":   "Error: Something went wrong\n    at /app/example.js:54:11",
		})
		mustStatus(t, status, 200)
		mustOK(t, body)
	})

	t.Run("nodejs_ai_demo_route", func(t *testing.T) {
		status, body := postJSON(t, "/v1/ai", map[string]any{
			"service":     service,
			"provider":    "openai",
			"model":       "gpt-4o-mini",
			"prompt":      `Translate "hello world" to Spanish.`,
			"response":    `"hola mundo"`,
			"tokens_in":   8,
			"tokens_out":  3,
			"duration_ms": 50,
		})
		mustStatus(t, status, 200)
		mustOK(t, body)
	})
}

// ---------------------------------------------------------------------------
// TestDataVisibleInUI — telemetry becomes visible in UI pages.
// ---------------------------------------------------------------------------

func testDataVisibleInUI(t *testing.T) {
	t.Run("dashboard_loads", func(t *testing.T) {
		status, text := getText(t, "/")
		mustStatus(t, status, 200)
		mustText(t, text, "Summary")
		mustText(t, text, "SOBS")
	})

	t.Run("logs_page_shows_curl_demo_data", func(t *testing.T) {
		marker := fmt.Sprintf("visibility-log-%d", nowMs())
		status, _ := postJSON(t, "/v1/logs", otlpLogPayload(marker, "visibility-seed", "INFO"))
		mustStatus(t, status, 200)
		waitForAnyText(t, "/logs?q="+marker, []string{marker}, 10*time.Second)
	})

	t.Run("logs_page_shows_otel_example_data", func(t *testing.T) {
		marker := fmt.Sprintf("visibility-otel-log-%d", nowMs())
		status, _ := postJSON(t, "/v1/logs", otlpLogPayload(marker, "visibility-seed", "INFO"))
		mustStatus(t, status, 200)
		waitForAnyText(t, "/logs?q="+marker, []string{marker}, 10*time.Second)
	})

	t.Run("traces_page_shows_example_data", func(t *testing.T) {
		traceID := fmt.Sprintf("%032x", nowMicros())
		if len(traceID) > 32 {
			traceID = traceID[len(traceID)-32:]
		}
		status, _ := postJSON(t, "/v1/traces", otlpTracePayload("visibility-seed", []any{
			span("visibility-trace-span", traceID, "1234567890abcdee", "", nil),
		}))
		mustStatus(t, status, 200)
		waitForAnyText(t, "/traces", []string{"visibility-seed"}, 10*time.Second)
	})

	t.Run("errors_page_shows_example_data", func(t *testing.T) {
		marker := fmt.Sprintf("visibility-error-%d", nowMs())
		status, _ := postJSON(t, "/v1/errors", map[string]any{
			"service": "visibility-seed",
			"type":    "Error",
			"message": marker,
			"stack":   "Error: visibility-seed",
		})
		mustStatus(t, status, 200)
		waitForAnyText(t, "/errors", []string{"visibility-seed", marker}, 10*time.Second)
	})

	t.Run("rum_page_shows_pageview", func(t *testing.T) {
		marker := fmt.Sprintf("https://example.com/visibility/%d", nowMs())
		status, _ := postJSON(t, "/v1/rum", map[string]any{
			"session_id": fmt.Sprintf("visibility-session-%d", nowMs()),
			"timestamp":  nowMs(),
			"event":      "pageview",
			"url":        marker,
			"path":       "/visibility",
			"title":      "Visibility",
			"user_agent": "visibility-seed",
			"service":    "visibility-seed",
		})
		mustStatus(t, status, 200)
		waitForAnyText(t, "/rum", []string{marker}, 10*time.Second)
	})

	t.Run("ai_page_shows_llm_events", func(t *testing.T) {
		model := fmt.Sprintf("visibility-model-%d", nowMs())
		status, _ := postJSON(t, "/v1/ai", map[string]any{
			"service":     "visibility-seed",
			"provider":    "openai",
			"model":       model,
			"prompt":      "seed",
			"response":    "seed",
			"tokens_in":   1,
			"tokens_out":  1,
			"duration_ms": 1,
		})
		mustStatus(t, status, 200)
		waitForAnyText(t, "/ai", []string{model}, 10*time.Second)
	})
}

// ---------------------------------------------------------------------------
// assertion + small helpers
// ---------------------------------------------------------------------------

func mustStatus(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func mustAccepted(t *testing.T, body map[string]any, want float64) {
	t.Helper()
	if got := accepted(body); got != want {
		t.Fatalf("accepted = %v, want %v (body=%v)", got, want, body)
	}
}

func mustOK(t *testing.T, body map[string]any) {
	t.Helper()
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got body=%v", body)
	}
}

func mustText(t *testing.T, haystack, needle string) {
	t.Helper()
	if !contains(haystack, needle) {
		t.Fatalf("expected %q in response", needle)
	}
}

func intKV(key string, intValue int) map[string]any {
	return map[string]any{"key": key, "value": map[string]any{"intValue": intValue}}
}

func nowMs() int64     { return time.Now().UnixMilli() }
func nowMicros() int64 { return time.Now().UnixMicro() }
