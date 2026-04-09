# SOBS Examples

This directory contains examples for sending telemetry to SOBS using various
clients and for exercising major SOBS features end-to-end.

## Prerequisites

Start SOBS:

```bash
# Docker
docker run -p 44317:4317 ghcr.io/abartrim/sobs:latest

# or docker-compose (full env-var reference)
docker-compose up -d
```

---

## Sending telemetry

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

---

## Feature examples

### AI Transparency & Assistant

SOBS records every LLM call your application makes and exposes them on the
**AI** page (`/ai`) for cost, latency, and prompt tracking.  An in-product
assistant lets you ask natural-language questions about your live telemetry.

```bash
pip install requests
python python/ai_agent_example.py
```

The example demonstrates:
- Recording LLM calls via `POST /v1/ai` (provider, model, tokens, duration)
- Asking the SOBS assistant a question via `POST /api/ai/helper`
- Multi-turn conversations (pass `chat_id` for follow-up questions)
- Pointer to **Settings → Agents** for automated agent / work-item flows

**Environment variables** (set on the SOBS container):

| Variable | Description |
|---|---|
| `SOBS_AI_ENDPOINT_URL` | LLM provider base URL (e.g. `https://api.openai.com/v1`) |
| `SOBS_AI_MODEL` | Default model name (e.g. `gpt-4o-mini`) |
| `SOBS_AI_API_KEY` | API key for the LLM provider |
| `SOBS_AI_GUARD_ENDPOINT_URL` | Guard model endpoint (screens prompts before main LLM) |
| `SOBS_AI_GUARD_MODEL` | Guard model name |
| `SOBS_AI_DLP_ENDPOINT_URL` | DLP proxy endpoint (optional) |
| `SOBS_AI_THINKING_LEVEL` | Reasoning depth: `off` \| `low` \| `medium` \| `high` |

Configure interactively at `http://localhost:44317/settings/ai`.

---

### Notifications & Webhooks

SOBS can send notifications when metric rules are triggered.  Supported
channel types: **webhook**, **Slack**, **email**, **browser push** (VAPID).

```bash
pip install flask requests
python notifications/webhook_example.py
```

The example starts a local webhook receiver and prints the `curl` commands
needed to:
1. Create a webhook notification channel in SOBS.
2. Test the channel.
3. Trigger a manual notification check.

Webhook URL caveat:
- If SOBS runs in Docker, use `http://host.docker.internal:<WEBHOOK_PORT>/webhook`.
- If SOBS runs directly on your host (not in Docker), use `http://localhost:<WEBHOOK_PORT>/webhook`.

Configure notification channels and rules interactively at
`http://localhost:44317/settings`.

**Key environment variable:**

| Variable | Description |
|---|---|
| `SOBS_VAPID_PRIVATE_KEY` | Hex-encoded VAPID private key for browser push |
| `SOBS_VAPID_SUBJECT` | Subject claim for VAPID JWTs (e.g. `mailto:ops@example.com`) |

---

### Data Management, Backup & Retention

See [data-management/README.md](data-management/README.md) for:
- Configuring S3-compatible backup (bucket, region, credentials, encryption)
- Setting retention TTLs for logs, traces, metrics, and RUM sessions
- Running and restoring backups via the API
- MinIO local-dev example

**Key environment variables:**

| Variable | Default | Description |
|---|---|---|
| `SOBS_RAW_METRICS_TTL_HOURS` | `48` | Baseline raw metric data point retention |
| `SOBS_PINNED_METRICS_TTL_DAYS` | `14` | Pinned / retention-window metric data |

Configure S3 backup and other TTLs at `http://localhost:44317/settings/data-management`.

---

### RUM (Real User Monitoring)

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

**Key environment variables:**

| Variable | Description |
|---|---|
| `SOBS_RUM_CLIENT_AUTH_MODE` | `none` (default) or `origin` for signed tokens |
| `SOBS_RUM_CLIENT_SIGNING_KEY` | HMAC key used when `SOBS_RUM_CLIENT_AUTH_MODE=origin` |
| `SOBS_RUM_ASSET_SIGNING_KEY` | HMAC key for signed asset upload requests (replay / screenshots) |
| `SOBS_SOURCE_MAP_ENABLE` | Set to `1` to enable source-map upload endpoint |
| `SOBS_SOURCE_MAP_DIR` | Local directory for uploaded source maps |

#### RUM replay payload contract (rrweb-style)

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

#### Browser replay demo app

For a quick local test page that exercises the new RUM replay/artifact/error flows:

```bash
./scripts/start_ollama_ai_test.sh
```

Then open:

- `http://127.0.0.1:5005` (demo app)
- `http://127.0.0.1:44317/rum`
- `http://127.0.0.1:44317/errors`

Disable demo app auto-start with `START_EXAMPLE_APP=0`.

---

### Kubernetes Health View

SOBS can display live Kubernetes cluster health on the **Kubernetes** page
(`/kubernetes`).  The feature is **off by default**; enable it in
**Settings → Kubernetes**.

Two ingestion modes are supported:

| Mode | Description |
|---|---|
| `realtime` | SOBS polls the Kubernetes API directly |
| `ingested` | External process POSTs JSON snapshots to `POST /api/kubernetes/ingest` |

Cluster-level telemetry (logs, metrics, events) can also be forwarded to SOBS
via the OpenTelemetry Collector.  See [k8s/otel-k8s-daemonset.yaml](../k8s/otel-k8s-daemonset.yaml)
(node-level) and [k8s/otel-k8s-deployment.yaml](../k8s/otel-k8s-deployment.yaml)
(cluster-level).

---

### Natural Language → SQL Query (NL→SQL)

The **Query** page (`/query`) lets you ask questions about your SOBS data in
plain English.  It is available automatically once an AI model and endpoint
are configured in **Settings → AI** (no extra flag required).

```bash
# Open the query page
open http://localhost:44317/query
```

You can also call the API directly:

```bash
curl -s -X POST http://localhost:44317/api/query/ask \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $SOBS_API_KEY" \
  -d '{"question": "How many errors occurred in the last hour per service?"}'
```

Optional environment variable to extend the allowed table list:

| Variable | Description |
|---|---|
| `SOBS_QUERY_ALLOWED_TABLES` | Comma-separated extra table names to expose to the NL→SQL engine |
| `SOBS_QUERY_MAX_ROWS` | Maximum rows returned per query (default `1000`) |

---

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

For Web UI authentication options see the main [README.md](../README.md#authentication).
