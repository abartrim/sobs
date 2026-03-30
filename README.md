# SOBS – Simple Observe Stack

**SOBS** is a lightweight, single-user OpenTelemetry-compatible telemetry container focused on simplicity and transparency. It collects **Logs**, **Errors**, **Traces**, **RUM** (Real User Monitoring), and **AI call transparency** — all in one tiny container you can run as a standalone pod or sidecar.

![Dashboard](https://github.com/user-attachments/assets/fab68924-3526-49a9-9c03-d3f994bca3dd)

## Features

- 📦 **Tiny** – single Python service + embedded chDB, ~256 MB RAM target
- 🗜️ **Compressed storage** – MergeTree schema uses ZSTD with selective Delta/T64 codecs
- 🔭 **OpenTelemetry** – accepts OTLP (JSON and protobuf) for logs, traces, metrics
- 🌐 **RUM** – client-side JS snippet with Web Vitals (LCP, CLS, INP, TTFB, FCP)
- 🐛 **Error tracking** – with stack traces and one-click resolve
- 🤖 **AI transparency** – record LLM prompts, responses and token usage
- 🔍 **Search** – grep (regex) and SQL WHERE clause filtering on logs
- 📊 **Query statistics** – collapsible logs analytics panel with query-scoped level/service distributions
- 🧠 **Manual advanced log analysis** – on-demand message pattern clustering, keyword signals, and optimization hints
- 📡 **Live tail** – SSE endpoint (`/tail`) for real-time streaming of logs and traces
- ⚡ **Live logs mode** – optional in-page streaming on Logs with pause-on-scroll and queued event counter
- 📈 **Metrics & Signals** – top-level Metrics page with derived telemetry signals and anomaly status
- 🧩 **Auto rule generation** – preview/create metric anomaly rules from recent derived-signal history
- 🗂️ **Auto dashboard generation** – build a derived-signal dashboard directly from active metric rules
- ✨ **First-run visual tour** – one-time onboarding modal with flow overview and quick-tour reopen entry
- 🎨 **Bootstrap 5 dark UI** – served locally, no CDN required
- 🐳 **Docker ready** – Dockerfile + docker-compose + Kubernetes manifests

## Quick Start

```bash
# Docker
docker run -p 4317:4317 -v sobs_data:/data ghcr.io/abartrim/sobs:latest

# docker-compose
docker-compose up -d

# Python (dev)
pip install -r requirements.txt
python app.py
```

Note: `python app.py` runs Hypercorn with a Quart ASGI app in single-process mode.

Open `http://localhost:4317` in your browser.

On first open, SOBS shows a lightweight visual onboarding tour (ingest → analyze → act). You can reopen it any time from the left nav via **Quick Tour**.

Prebuilt image published by CI:

`ghcr.io/abartrim/sobs:latest`

## Runtime Modes

- Local and production process manager:
  - `python app.py` starts Hypercorn.
  - With embedded chDB, keep a single process by default.
  - Equivalent explicit command:

```bash
hypercorn --workers 1 --bind 0.0.0.0:${PORT:-4317} app:app
```

Why: embedded chDB is process-sensitive. Multiple process workers can trigger DB lock/stall behavior in embedded mode.

## Sending Data

### Python – OpenTelemetry SDK

```bash
pip install opentelemetry-sdk opentelemetry-exporter-otlp-proto-http
python examples/python/otel_example.py
```

### Flask auto-instrumentation

```bash
pip install opentelemetry-instrumentation-flask opentelemetry-exporter-otlp-proto-http
python examples/python/flask_example.py
```

### Node.js / Express

```bash
cd examples/nodejs && npm install && node example.js
```

### curl (no SDK)

```bash
bash examples/curl_examples.sh
```

### Client-side RUM

```html
<script src="http://YOUR_SOBS_HOST/static/rum.js"></script>
<script>
  SOBS.init({ endpoint: 'http://YOUR_SOBS_HOST/v1/rum', appName: 'my-app' });
</script>
```

## OTLP Endpoints

| Endpoint       | Method | Description                        |
|----------------|--------|------------------------------------|
| `/v1/logs`     | POST   | OTLP/JSON logs                     |
| `/v1/traces`   | POST   | OTLP/JSON traces                   |
| `/v1/metrics`  | POST   | OTLP/JSON metrics (typed metric tables + anomaly views) |
| `/v1/rum`      | POST   | RUM events (JSON array)            |
| `/v1/errors`   | POST   | Direct error submission            |
| `/v1/ai`       | POST   | AI/LLM call transparency           |
| `/health`      | GET    | Liveness check                     |
| `/health/db`   | GET    | DB readiness check (touches chDB) |

Ingest writes are queued and flushed by a single background DB writer thread.

- Default runtime behavior: ingest endpoints acknowledge once the write is queued.
- Test behavior (`app.config["TESTING"] = True`): writes wait for batch completion so tests assert committed state deterministically.
- If the queue is saturated, ingest returns `503` so clients can retry/backoff.

This model favors client latency under burst traffic. It does not guarantee synchronous commit-per-request in normal runtime.

## Metrics Rules Automation

SOBS includes two automation flows under **Metrics → Metrics Rules**:

- **Auto Make Metric Rules**: generates threshold rules from recent derived-signal history with a preview-first workflow and capped create.
- **Auto Generate Dashboard from Active Rules**: creates/updates a dashboard with one derived-signal overlay chart per matching active rule (preview-first, max chart cap, skip-existing by title).

Both auto panels include contextual help and retain their open/collapsed scope across preview/create interactions.

Fresh chDB databases are created with schema compression tuned using ZSTD plus selective Delta/T64 codecs. For encrypted local-disk testing in the container image, set `SOBS_CHDB_ENCRYPTION_KEY` and SOBS will render an internal ClickHouse config at startup and pass it to chDB automatically.

Use `/health/db` for readiness checks in orchestrated deployments when you need the probe to exercise DB availability as well as process liveness.

## Configuration

| Variable                    | Default        | Description                                      |
|-----------------------------|----------------|--------------------------------------------------|
| `SOBS_DATA_DIR`             | `./data`       | Directory for embedded chDB state                |
| `SOBS_API_KEY`              | _(empty)_      | Optional auth key for ingest endpoints           |
| `SOBS_BASIC_AUTH_USERNAME`  | _(empty)_      | Optional Basic Auth username for the Web UI      |
| `SOBS_BASIC_AUTH_PASSWORD`  | _(empty)_      | Optional Basic Auth password for the Web UI      |
| `SOBS_EXTERNAL_AUTH_URL`    | _(empty)_      | Optional external Bearer validator for the Web UI |
| `SOBS_BASE_PATH`            | _(empty)_      | Optional URL prefix (for example `/sobs`) for UI/API routing and generated links |
| `SOBS_SECRET_KEY`           | `sobs-dev-secret-key` | Secret key used by Quart session handling (set explicitly in production) |
| `PORT`                      | `4317`         | Listen port                                      |
| `SOBS_WRITE_QUEUE_MAX`      | `5000`         | Max buffered write operations before ingest returns `503` |
| `SOBS_WRITE_BATCH_MAX`      | `200`          | Max writes processed per DB batch |
| `SOBS_WRITE_BATCH_WAIT_MS`  | `20`           | Max milliseconds to wait for filling a write batch |
| `SOBS_CHDB_ENCRYPTION_KEY`  | _(empty)_      | Hex key for runtime-generated encrypted disk config in container startup |
| `SOBS_CHDB_BASE_DISK_PATH`  | `/data/chdb-disks/plain` | Base local disk path for runtime-generated storage configuration |
| `SOBS_CHDB_ENCRYPTED_DISK_PATH` | `/data/chdb-disks/encrypted` | Encrypted disk path for runtime-generated storage configuration |
| `SOBS_CHDB_ENCRYPTED_DISK_NAME` | `encrypted_disk` | Disk name emitted into runtime-generated ClickHouse config |
| `SOBS_CHDB_STORAGE_POLICY_NAME` | `encrypted_only` | Storage policy name emitted into runtime-generated ClickHouse config |
| `SOBS_CHDB_CONFIG_RENDER_PATH` | `/tmp/sobs-clickhouse-config.xml` | Absolute path where startup renders internal ClickHouse config |
| `SOBS_CLICKHOUSE_CONFIG_FILE` | _(empty)_    | Absolute mounted ClickHouse `config.xml` passed to embedded chDB as `config-file` startup arg |
| `SOBS_CHDB_EXPECT_DISK`     | _(empty)_       | Optional startup assertion: required disk name in `system.disks` |
| `SOBS_CHDB_EXPECT_STORAGE_POLICY` | _(empty)_ | Optional startup assertion: required policy name in `system.storage_policies` |
| `HYPERCORN_WORKERS`         | `1`            | Hypercorn worker process count (forced to 1 for embedded chDB safety) |
| `HYPERCORN_BIND`            | `0.0.0.0:$PORT` | Hypercorn bind address override |

When `SOBS_CHDB_ENCRYPTION_KEY` is set in the container image runtime:

- The entrypoint renders a ClickHouse `config.xml` inside the container.
- `SOBS_CLICKHOUSE_CONFIG_FILE` is exported to the rendered absolute path.
- Default startup assertions are set (`SOBS_CHDB_EXPECT_DISK` and `SOBS_CHDB_EXPECT_STORAGE_POLICY`) unless already provided.

This keeps encryption keys injected at runtime through environment/secret management, without baking secrets into the image.

Authentication details and setup examples are documented in [AUTHENTICATION.md](AUTHENTICATION.md).

The Web UI supports exactly one mode at a time:

- no auth
- basic auth (requires both `SOBS_BASIC_AUTH_USERNAME` and `SOBS_BASIC_AUTH_PASSWORD`)
- external bearer validation (`SOBS_EXTERNAL_AUTH_URL`)

Ingest API endpoints (`/v1/*`) use the separate `SOBS_API_KEY` mechanism.

For reverse proxies, SOBS also honors `X-Forwarded-Prefix` for URL generation and prefixed routing.

## Live Tail (SSE)

SOBS exposes a Server-Sent Events endpoint at `/tail` for real-time streaming of logs and traces as they arrive.

### Usage

```bash
# Stream all events (logs + traces)
curl -N http://localhost:4317/tail

# Stream logs only
curl -N "http://localhost:4317/tail?source=logs"

# Stream traces only
curl -N "http://localhost:4317/tail?source=traces"

# Filter by service
curl -N "http://localhost:4317/tail?service=myapp"

# Combine source and service filter
curl -N "http://localhost:4317/tail?source=logs&service=myapp"
```

### Query parameters

| Parameter | Default | Description |
|-----------|---------|-------------|
| `source`  | `all`   | Event source to stream: `logs`, `traces`, or `all` |
| `service` | _(empty)_ | Optional exact service name filter |

### Event format

Each SSE event is a JSON object on a single `data:` line:

**Log event:**
```json
{"source": "logs", "ts": "2024-01-15T10:30:00.000+00:00", "level": "INFO", "service": "my-service", "body": "Request processed", "trace_id": "abc123"}
```

**Trace event:**
```json
{"source": "traces", "ts": "2024-01-15T10:30:00.000+00:00", "trace_id": "abc123", "span_id": "def456", "name": "GET /api/users", "service": "my-service", "duration_ms": 12.5, "status": "OK"}
```

The stream sends a `retry: 5000` directive on connect and a `: keepalive` comment every 15 seconds to keep the connection alive through proxies.

### Authentication

`/tail` uses the same Web UI auth mode as all other UI routes. Supply credentials the same way you would for the Web UI:

```bash
# Basic auth
curl -N http://localhost:4317/tail \
  -H "Authorization: Basic $(printf 'admin:secret' | base64)"

# Bearer token (external auth)
curl -N http://localhost:4317/tail \
  -H "Authorization: Bearer eyJhbGciOi..."
```

### Browser / JavaScript

```javascript
const source = new EventSource('/tail?source=logs');
source.onmessage = (e) => {
  const event = JSON.parse(e.data);
  console.log(event.ts, event.level, event.service, event.body);
};
```

### Logs page Live mode

The Logs page includes a **Live mode** toggle (top-right) that consumes `/tail?source=logs` and appends new rows in real time.

- New rows are prepended at the top and briefly highlighted.
- If you scroll down, Live mode pauses rendering to avoid jumpy UX.
- While paused, a `N new` button appears; click it (or scroll back to top) to flush queued events.
- SQL WHERE mode disables Live mode to avoid mixed client/server filtering behavior.

### Logs query analytics

The Logs page includes a collapsible **Query Statistics** panel between filters and the table.

- Statistics are **query scoped** (computed across all rows matching the current query filters), not page scoped.
- Basic analytics include counts by severity level and top services.
- Advanced analytics are **manual**: click **Run advanced analysis** to compute message intelligence for the current query.

Advanced analysis outputs include:

- repeated message pattern fingerprints
- detected error families (for example, `TimeoutError`, `ConnectionRefusedError`)
- top message keywords
- actionable optimization hints based on severity mix, repetition, and timeout signals

## Kubernetes

Deploy as a standalone pod:

```bash
kubectl apply -f k8s/deployment.yaml
```

Or as a **sidecar** – see `k8s/sidecar.yaml` for instructions.

## Screenshots

| Logs (grep + SQL search) | AI Transparency |
|---|---|
| ![Logs](https://github.com/user-attachments/assets/f6eb544c-11c0-4836-a337-864a46e13e29) | ![AI](https://github.com/user-attachments/assets/caf5a401-d86b-4bea-b6db-28ef983879ba) |

## Running Tests

```bash
pip install -r requirements.txt -r requirements-integration.txt
pytest tests/
```

## Running Benchmarks

```bash
# Start SOBS first (for example: python app.py)
./scripts/benchmark.sh

# Or target a custom endpoint
./scripts/benchmark.sh http://127.0.0.1:44318
```

## Traffic Example (including realistic mode)

Use `scripts/load_example.py` directly when you want to drive specific event rates.

```bash
# High-throughput load mode (default)
python scripts/load_example.py --base http://127.0.0.1:4317 --total 420 --workers 28

# Realistic paced mode for UI demos
python scripts/load_example.py --base http://127.0.0.1:4317 --mode realistic --rps 3 --jitter-ms 250 --total 180 --workers 8
```

Parameters:

- `--mode load|realistic`
- `--rps` target requests/sec in realistic mode
- `--jitter-ms` random +/- milliseconds around the realistic interval

See [CONTRIBUTING.md](CONTRIBUTING.md) for local development setup and quality checks.

## Git Pre-Commit Hook (Python)

This repository includes a version-controlled Git pre-commit hook at `.githooks/pre-commit`.

It runs on staged Python files and performs:

- `isort`
- `black`
- `flake8`
- `mypy`

Enable it once per clone:

```bash
git config core.hooksPath .githooks
chmod +x .githooks/pre-commit
```

If formatting changes are applied, the hook re-stages those Python files before commit.

