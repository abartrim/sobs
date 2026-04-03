# SOBS Examples

This directory contains examples for sending telemetry to SOBS using various clients.

## Prerequisites

Start SOBS:

```bash
# Docker
docker run -p 44317:4317 ghcr.io/abartrim/sobs:latest

# or docker-compose
docker-compose up -d
```

## Examples

### Python – OpenTelemetry SDK

```bash
pip install opentelemetry-sdk opentelemetry-exporter-otlp-proto-http
python python/otel_example.py
```

### Python – Flask with auto-instrumentation

```bash
pip install flask opentelemetry-sdk opentelemetry-instrumentation-flask \
    opentelemetry-exporter-otlp-proto-http requests
python python/flask_example.py
```

### Node.js – Express with OpenTelemetry SDK

```bash
cd nodejs
npm install
node example.js
```

### Prometheus & OTEL metrics (OTel Collector bridge)

Forward existing Prometheus `/metrics` endpoints into SOBS via the OpenTelemetry Collector:

```bash
# Full local stack: SOBS + OTel Collector + demo app
docker compose -f prometheus/docker-compose.yml up -d
```

Or push metrics directly from Python without a scrape endpoint:

```bash
pip install opentelemetry-sdk opentelemetry-exporter-otlp-proto-http prometheus_client
python prometheus/python_metrics_example.py --mode push
```

See [prometheus/README.md](prometheus/README.md) for full details, configuration reference,
and security considerations.

### curl – No SDK required

```bash
bash curl_examples.sh
```

### Load example (repo script)

From the repository root:

```bash
# Default high-throughput mode
python scripts/load_example.py --base http://localhost:44317 --total 420 --workers 28

# Realistic paced mode (useful for observing Logs Live mode)
python scripts/load_example.py --base http://localhost:44317 --mode realistic --rps 3 --jitter-ms 250 --total 180 --workers 8
```

## RUM (Client-side)

Embed in your HTML:

```html
<script src="http://localhost:44317/static/rum.js?app=my-app"></script>

<!-- Optional origin-bound token bootstrap -->
<script
  src="http://localhost:44317/static/rum.js"
  data-sobs-app="my-app"
  data-sobs-endpoint="http://localhost:44317/v1/rum"
  data-sobs-client-token-url="/internal/sobs/rum-client-token">
</script>
```

### RUM replay payload contract (rrweb-style)

SOBS expects replay metadata on error events under `replay` and optional screenshot metadata under `artifact`.
The metadata should reference an uploaded replay/session in your own storage system.

Replay contract:

```json
{
  "replay": {
    "id": "replay-123",
    "url": "https://example.com/replays/replay-123",
    "provider": "rrweb"
  }
}
```

Artifact contract:

```json
{
  "artifact": {
    "type": "screenshot",
    "id": "shot-123",
    "url": "https://example.com/artifacts/shot-123.png"
  }
}
```

Use either `SOBS.setVisualContext(...)` directly, or the dedicated helpers:

- `SOBS.setReplayContext(replay, { ttlMs, consumeOnce })`
- `SOBS.setArtifactContext(artifact, { ttlMs, consumeOnce })`

Signed upload endpoint contract for replay/screenshot bytes:

- `POST /v1/rum/assets?type=<replay|screenshot|...>&name=<filename>`
- Body: raw bytes
- Required headers:
  - `X-SOBS-Asset-Timestamp`
  - `X-SOBS-Asset-Signature`

Signature payload:

```text
POST
/v1/rum/assets
<timestamp>
<sha256_body_hex>
<content_type_lowercase>
<asset_type_lowercase>
<asset_name>
```

Optional browser client auth:

- Configure `SOBS_RUM_CLIENT_AUTH_MODE=origin` and `SOBS_RUM_CLIENT_SIGNING_KEY` on SOBS.
- Mint token from your backend via `POST /v1/rum/client-token`.
- Feed token to browser using `data-sobs-client-token-url` or `SOBS.setClientAuthToken(token)`.

React notes:

- SOBS RUM works with React because collection is browser-level and independent of framework runtime.
- For component render failures, pair with a React Error Boundary and call `SOBS.captureException(...)` explicitly.

See [rum/rrweb_replay_example.js](rum/rrweb_replay_example.js) for an end-to-end browser integration pattern.

### Browser replay demo app

For a quick local test page that exercises the new RUM replay/artifact/error flows:

```bash
./scripts/start_ollama_ai_test.sh
```

Then open:

- `http://127.0.0.1:5005` (demo app)
- `http://127.0.0.1:44317/rum`
- `http://127.0.0.1:44317/errors`

Disable demo app auto-start with `START_EXAMPLE_APP=0`.

## OTLP Endpoint Reference

| Endpoint         | Method | Description                        |
|------------------|--------|------------------------------------|
| `/v1/logs`       | POST   | OTLP logs (JSON or protobuf)       |
| `/v1/traces`     | POST   | OTLP traces (JSON or protobuf)     |
| `/v1/metrics`    | POST   | OTLP metrics (JSON or protobuf, typed metric tables + anomaly views) |
| `/v1/rum`        | POST   | RUM events (JSON array)            |
| `/v1/errors`     | POST   | Direct error submission            |
| `/v1/ai`         | POST   | AI/LLM call transparency           |
| `/health`        | GET    | Health check                       |

## Authentication

Set `SOBS_API_KEY` environment variable to enable API key auth.
Send key in `X-API-Key` header or `?api_key=` query parameter.
