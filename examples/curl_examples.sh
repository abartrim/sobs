#!/usr/bin/env bash
# SOBS curl examples – send telemetry data without any SDK.
# Replace http://localhost:44317 with your SOBS instance URL.

SOBS="${SOBS:-http://localhost:44317}"

# ---- 1. Send a log (OTLP/JSON format) ----
curl -s -X POST "$SOBS/v1/logs" \
  -H "Content-Type: application/json" \
  -d '{
    "resourceLogs": [{
      "resource": {
        "attributes": [{"key":"service.name","value":{"stringValue":"curl-demo"}}]
      },
      "scopeLogs": [{
        "logRecords": [{
          "timeUnixNano": "'"$(date +%s%N)"'",
          "severityText": "INFO",
          "body": {"stringValue": "Hello from curl!"},
          "attributes": [
            {"key":"env","value":{"stringValue":"dev"}}
          ]
        }]
      }]
    }]
  }'
echo ""

# ---- 2. Send a trace span ----
curl -s -X POST "$SOBS/v1/traces" \
  -H "Content-Type: application/json" \
  -d '{
    "resourceSpans": [{
      "resource": {
        "attributes": [{"key":"service.name","value":{"stringValue":"curl-demo"}}]
      },
      "scopeSpans": [{
        "spans": [{
          "traceId": "abcdef1234567890abcdef1234567890",
          "spanId": "1234567890abcdef",
          "name": "curl-span",
          "startTimeUnixNano": "'"$(( $(date +%s) * 1000000000 ))"'",
          "endTimeUnixNano":   "'"$(( $(date +%s) * 1000000000 + 50000000 ))"'",
          "status": {"code": 1}
        }]
      }]
    }]
  }'
echo ""

# ---- 3. Send an error directly ----
curl -s -X POST "$SOBS/v1/errors" \
  -H "Content-Type: application/json" \
  -d '{
    "service": "curl-demo",
    "type": "RuntimeError",
    "message": "Oops, something went wrong",
    "stack": "RuntimeError: Oops\n  at main (script.sh:42)"
  }'
echo ""

# ---- 4. Send RUM event ----
curl -s -X POST "$SOBS/v1/rum" \
  -H "Content-Type: application/json" \
  -d '[{
    "type": "pageview",
    "timestamp": "'"$(date -u +%FT%TZ)"'",
    "sessionId": "sess-abc123",
    "url": "https://example.com/home",
    "title": "Home Page"
  }]'
echo ""

# ---- 5. Send AI transparency event ----
curl -s -X POST "$SOBS/v1/ai" \
  -H "Content-Type: application/json" \
  -d '{
    "service": "curl-demo",
    "provider": "openai",
    "model": "gpt-4o-mini",
    "prompt": "What is the capital of France?",
    "response": "Paris.",
    "tokens_in": 10,
    "tokens_out": 2,
    "duration_ms": 250
  }'
echo ""

# ---- 6. Send a gauge metric (OTLP/JSON) ----
# Gauge: an instantaneous value that can go up or down (e.g. CPU %, memory).
curl -s -X POST "$SOBS/v1/metrics" \
  -H "Content-Type: application/json" \
  -d '{
    "resourceMetrics": [{
      "resource": {
        "attributes": [{"key":"service.name","value":{"stringValue":"curl-demo"}}]
      },
      "scopeMetrics": [{
        "metrics": [{
          "name": "system.cpu.utilization",
          "description": "CPU utilisation",
          "unit": "%",
          "gauge": {
            "dataPoints": [{
              "timeUnixNano": "'"$(date +%s%N)"'",
              "asDouble": 23.5,
              "attributes": [
                {"key":"core","value":{"stringValue":"0"}}
              ]
            }]
          }
        }]
      }]
    }]
  }'
echo ""

# ---- 7. Send a counter metric (OTLP/JSON) ----
# Sum (monotonic counter): a value that only increases (e.g. total request count).
# aggregationTemporality 2 = CUMULATIVE
curl -s -X POST "$SOBS/v1/metrics" \
  -H "Content-Type: application/json" \
  -d '{
    "resourceMetrics": [{
      "resource": {
        "attributes": [{"key":"service.name","value":{"stringValue":"curl-demo"}}]
      },
      "scopeMetrics": [{
        "metrics": [{
          "name": "http.server.requests",
          "description": "Total HTTP requests",
          "unit": "1",
          "sum": {
            "isMonotonic": true,
            "aggregationTemporality": 2,
            "dataPoints": [{
              "timeUnixNano": "'"$(date +%s%N)"'",
              "asDouble": 142,
              "attributes": [
                {"key":"http.route","value":{"stringValue":"/api/users"}},
                {"key":"http.status_code","value":{"stringValue":"200"}}
              ]
            }]
          }
        }]
      }]
    }]
  }'
echo ""

echo "All events sent. Open $SOBS in your browser."
