# SOBS – Simple Observe Stack

## v0.1.0-beta.1 – First public beta

SOBS is actively used and under continued development. Beta APIs and UI behavior may evolve before stable v0.1.0. See [What's New](#whats-new-in-v010-beta1) below.

**SOBS** is a single-user OpenTelemetry-compatible telemetry container focused on simplicity and transparency. It collects **Logs**, **Errors**, **Traces**, **RUM** (Real User Monitoring), **Web Traffic analytics**, and **AI call transparency** — all in one container you can run as a standalone pod or sidecar. Taking an AI-first approach it implements an automation layer that can automatically raise GitHub issues and assign GitHub Copilot to create fix PRs and inform the user for a collaborative AI/DevOps experience.

### Who is this for?
- Development: a full-featured observability container for local development.
- Integration and test environments: includes infrastructure tracking metrics for a 360-degree view of issues and performance, while staying lightweight enough to keep environments truly separate.
- Multi-tenant setups: each tenant can keep its own data sovereign and isolated.

<br />

![Summary AI Assistant](static/help/summary_ai_assistant.png)

## Features

- **Single service** – Python + embedded chDB; 768 MB - 1 GB RAM when supporting millions of rows of data
- **Compressed storage** – MergeTree schema uses ZSTD with selective Delta/T64 codecs
- **OpenTelemetry** – accepts OTLP (JSON and protobuf) for logs, traces, metrics
- **RUM** – client-side JS snippet with Web Vitals (LCP, CLS, INP, TTFB, FCP)
- **Web Traffic** – geo map, browser/OS/timezone/language/device analytics from RUM data with date-range filters
- **CVE enrichment** – daily auto-scan of detected libraries against OSV.dev, with per-finding dispositions
- **Error tracking** – with stack traces, one-click resolve, and grouped/deduplicated view
- **AI transparency** – record LLM prompts, responses and token usage
- **Contextual AI Assistant** – bottom-right in-app assistant for page-aware help and guided UI actions
- **Search** – grep (regex, with autocomplete/IntelliSense) and SQL WHERE clause filtering on logs
- **ANSI color rendering** – colorized log output with on/off toggle
- **Tag-aware log SQL assistant** – `has_tag()` helper, SQL filter validation, and field hints/autocomplete on Logs
- **Query statistics** – collapsible logs analytics panel with query-scoped level/service distributions
- **Manual advanced log analysis** – on-demand message pattern clustering, keyword signals, and optimization hints
- **Saved reports** – persist and re-apply filter sets across Logs, Traces, Errors, Metrics, RUM, and AI pages
- **Multi-select filters** – all filter panels support multi-value service/level/status selection
- **GitHub work items** – track agent-created or agent-reused GitHub issues in a dedicated Work Items UI
- **Issue dedupe and noise control** – local-LLM-assisted reuse/link/create decisions before opening more GitHub work
- **Natural-language Query page** – NL→SQL over embedded chDB with read-only SQL guardrails and chart/dashboard actions
- **Table Explorer** – visual schema and sample-data browser for all allowed observability tables
- **Notifications & Webhooks** – Slack, webhook, email, and browser push channels with rule-based dispatch
- **Live tail** – SSE endpoint (`/tail`) for real-time streaming of logs and traces
- **Live logs mode** – optional in-page streaming on Logs with pause-on-scroll and queued event counter
- **Metrics & Signals** – top-level Metrics page with derived telemetry signals and anomaly status
- **Auto rule generation** – preview/create metric anomaly rules from recent derived-signal history
- **Auto dashboard generation** – build a derived-signal dashboard directly from active metric rules
- **Data Management** – TTL retention windows, S3 backup/restore, optional backup encryption
- **First-time setup wizard** – guided instrumentation bootstrap for env/language/deployment combinations
- **First-run visual tour** – one-time onboarding modal with flow overview and quick-tour reopen entry
- **Incident evidence view** – one-click aggregation of related errors, logs, spans, RUM, and anomaly state around a trace or error event
- **Free-text / regex filtering** – full-text and regex filter bar across Errors, Traces, Metrics, and RUM pages
- **Seasonality-aware anomaly rules** – metric anomaly rule generation with per-time-bucket seasonal thresholds
- **Human-friendly signal labels** – readable names and descriptions for all metric/anomaly signals
- **Report import / export / share** – export saved report presets to JSON, import on another instance, or share via URL
- **Bootstrap 5 theming** – served locally with light/dark/system theme toggle, no CDN required
- **Docker ready** – Dockerfile + docker-compose + Kubernetes manifests

## What's New in v0.1.0-beta.1

Released 2026-04-09.  Full details in
[`docs/RELEASE_NOTES_v0.1.0-beta.1_PUBLIC.md`](docs/RELEASE_NOTES_v0.1.0-beta.1_PUBLIC.md)
and the [operator notes](docs/RELEASE_NOTES_v0.1.0-beta.1_OPERATOR.md).

Highlights:

- **Security hardening** – hosted security controls and container runtime hardening (see [#176](https://github.com/abartrim/sobs/pull/176)).
- **Setup wizard** – first-time instrumentation bootstrap wizard (`GET /api/setup-wizard/steps`).
- **Incident evidence view** – `GET /incident` aggregates all evidence around a trace or error in one view.
- **Seasonality-aware anomaly rules** – per-bucket seasonal thresholds for metric anomaly auto-generation.
- **Regex / free-text filtering** – filter bars on Errors, Traces, Metrics, and RUM now support regex.
- **Signal labels** – human-readable names and descriptions for all metric/anomaly signals.
- **Report import / export / share** – `GET /api/reports/export`, `POST /api/reports/import`.
- **Database stats panel** – compressed/uncompressed storage and active query counts on Summary page.
- **Lazy raw span accordion** – trace detail loads raw span data on demand for improved performance.
- **AI trace link context** – trace links preserve time-window context across navigation.

## Quick Start

```bash
# Docker
docker run -p 44317:4317 -v sobs_data:/data ghcr.io/abartrim/sobs:latest

# docker-compose
docker-compose up -d

# Python (dev)
pip install -r requirements.txt
python app.py
```

Note: `python app.py` runs Hypercorn with a Quart ASGI app in single-process mode.

Open `http://localhost:44317` in your browser.

On first open, SOBS shows a lightweight visual onboarding tour (ingest → analyze → act). You can reopen it any time from the left nav via **Quick Tour**.

Prebuilt images published by CI to GHCR:

| Tag | When published |
|-----|----------------|
| `ghcr.io/abartrim/sobs:latest` | Every push to `main` |
| `ghcr.io/abartrim/sobs:vX.Y.Z` | On a `vX.Y.Z` GitHub Release tag |
| `ghcr.io/abartrim/sobs:<sha>` | Every push to `main` (short SHA) |

The version shown in the sidebar footer of the UI reflects `SOBS_BUILD_VERSION`, which is stamped into the image at build time from the release tag.

## System Requirements

| Resource | Minimum | Notes |
|----------|---------|-------|
| **RAM**  | 768 MB - 1 GB | Realistic working set when processing millions of rows of telemetry data; chDB uses memory for query fan-out and caching |
| **CPU**  | 1 vCPU  | Single-process Hypercorn + embedded chDB; more CPUs improve query throughput |
| **Disk** | 1 GB+   | Data directory for embedded chDB state; grows with ingested volume |

> **Note:** Earlier documentation stated a ~256 MB RAM target. In practice, with millions of rows of logs, traces, and metrics, the realistic working set is **768 MB - 1 GB**. Plan your deployment accordingly and use the chDB memory-optimization settings for constrained environments (see [PR #136](https://github.com/abartrim/sobs/pull/136) and the Kubernetes section below).



- Local and production process manager:
  - `python app.py` starts Hypercorn.
  - With embedded chDB, keep a single process by default.
  - Equivalent explicit command:

```bash
hypercorn --workers 1 --bind 0.0.0.0:${PORT:-44317} app:app
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

### Prometheus / OTEL metrics (OTel Collector bridge)

Forward existing Prometheus `/metrics` endpoints into SOBS using the OpenTelemetry
Collector as a bridge.  No changes are required to the instrumented application.

```bash
# Full local stack: SOBS + OTel Collector + demo app
docker compose -f examples/prometheus/docker-compose.yml up -d
```

Or push OTLP metrics directly from Python without a scrape endpoint:

```bash
pip install opentelemetry-sdk opentelemetry-exporter-otlp-proto-http prometheus_client
python examples/prometheus/python_metrics_example.py --mode push
```

See [`examples/prometheus/README.md`](examples/prometheus/README.md) for full configuration
details, security considerations, and limitations.

### Client-side RUM

```html
<!-- One-line auto-init (endpoint inferred from script host) -->
<script src="http://YOUR_SOBS_HOST/static/rum.js?app=my-app"></script>

<!-- Feature-complete init (auth token, SPA nav, trace propagation, replay/screenshot hooks) -->
<script
  src="http://YOUR_SOBS_HOST/static/rum.js"
  data-sobs-app="shop-web"
  data-sobs-endpoint="http://YOUR_SOBS_HOST/v1/rum"
  data-sobs-client-token-url="/internal/sobs/rum-client-token">
</script>

<script>
  SOBS.init({
    endpoint: 'http://YOUR_SOBS_HOST/v1/rum',
    appName: 'shop-web',
    clientAuthTokenUrl: '/internal/sobs/rum-client-token',
    trackSPA: true,

    // Optional trace defaults (or call setTraceParent/setTraceContext at runtime)
    traceparent: window.__TRACEPARENT__ || '',
    tracePropagationOrigins: ['https://api.example.com'],

    // Optional breadcrumb buffer sizing
    consoleBufferSize: 20,
    breadcrumbBufferSize: 40,

    // Optional replay + screenshot capture for enriched error events
    replay: {
      enabled: true,
      scriptUrl: 'https://cdn.jsdelivr.net/npm/rrweb@latest/dist/record/rrweb-record.min.js',
      maxEvents: 500,
      screenshot: {
        enabled: true,
        scriptUrl: 'https://cdn.jsdelivr.net/npm/html2canvas@1.4.1/dist/html2canvas.min.js',
        mimeType: 'image/jpeg',
        quality: 0.75,
        maxEdge: 1400
      },
      upload: async function (envelope) {
        // Your backend uploads replay/screenshot and returns metadata refs.
        const resp = await fetch('/api/replay/upload', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(envelope)
        });
        if (!resp.ok) throw new Error('Replay upload failed');
        // Expected shape:
        // { replay: { id, url, provider }, artifact: { type, id, url } }
        return resp.json();
      }
    }
  });

  // Add app-level breadcrumbs and custom events.
  SOBS.addBreadcrumb('checkout', 'User clicked Place Order', { step: 'payment' });
  SOBS.track('feature-flag', { flag: 'new-checkout', variant: 'B' });

  // Attach visual context to the NEXT captured error event.
  SOBS.setVisualContext({
    replay: { id: 'replay-123', url: 'https://example.com/replays/replay-123', provider: 'rrweb' },
    artifact: { type: 'screenshot', id: 'shot-123', url: 'https://example.com/artifacts/shot-123.png' },
    ttlMs: 15000,
    consumeOnce: true
  });

  // Manual capture path (in addition to auto window.onerror/unhandledrejection capture).
  try {
    throw new Error('Checkout confirmation failed');
  } catch (err) {
    SOBS.captureException(err, { errorSource: 'checkout-flow' });
  }

  // Runtime helpers:
  // SOBS.setTraceParent('00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01');
  // SOBS.setTraceContext('<trace-id-32-hex>', '<span-id-16-hex>');
  // SOBS.setReplayContext({ id: 'replay-123', url: 'https://.../replay-123', provider: 'rrweb' }, { ttlMs: 15000 });
  // SOBS.setArtifactContext({ type: 'screenshot', id: 'shot-123', url: 'https://.../shot-123.png' }, { ttlMs: 15000 });
  // SOBS.clearVisualContext();
  // SOBS.enableReplay({ enabled: true, scriptUrl: 'https://cdn.../rrweb-record.min.js' });
  // SOBS.disableReplay();
  // SOBS.setReplayUpload(async (envelope) => ({ replay: { id: 'r1', url: 'https://...', provider: 'rrweb' } }));
  // SOBS.setClientAuthToken('<short-lived-rum-token>');
</script>
```

React compatibility:

- Yes, it works with React apps. The RUM script hooks browser-level signals (`window.onerror`, `unhandledrejection`, fetch failures, performance observers), so framework internals do not block capture.
- For best results in React, still add an Error Boundary and call `SOBS.captureException(error, { errorSource: 'react-error-boundary' })` from `componentDidCatch`.
- In SPA routing (React Router), leave `trackSPA` enabled (default) so route changes are tracked via History API.

Replay and screenshot payload contract:

```json
{
  "replay": {
    "id": "replay-123",
    "url": "https://example.com/replays/replay-123",
    "provider": "rrweb"
  },
  "artifact": {
    "type": "screenshot",
    "id": "shot-123",
    "url": "https://example.com/artifacts/shot-123.png"
  }
}
```

For an integration sketch, see [examples/rum/rrweb_replay_example.js](examples/rum/rrweb_replay_example.js).

Signed asset upload endpoint:

- `POST /v1/rum/assets?type=<replay|screenshot|...>&name=<filename>`
- Body: raw bytes (`application/json`, `image/png`, etc.)
- Required headers:
  - `X-SOBS-Asset-Timestamp` (unix epoch seconds)
  - `X-SOBS-Asset-Signature` (`hex(hmac_sha256(signing_key, canonical_payload))`)

Canonical payload format:

```text
POST
/v1/rum/assets
<timestamp>
<sha256_body_hex>
<content_type_lowercase>
<asset_type_lowercase>
<asset_name>
```

Optional browser RUM auth endpoint:

- `POST /v1/rum/client-token`
- Body: `{ "appName": "my-app", "origin": "https://app.example.com", "ttlSec": 900 }`
- Requires `SOBS_API_KEY` when API key auth is enabled.
- Returns a short-lived token bound to app + origin, for use in browser RUM events.

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

## MCP Endpoints (Copilot / AI Agent Access)

SOBS exposes a [Model Context Protocol](https://modelcontextprotocol.io/) (MCP) server so that
GitHub Copilot Agent and VS Code Copilot can query observability data directly for diagnosis and
troubleshooting.

| Endpoint       | Method | Auth required | Description                        |
|----------------|--------|---------------|------------------------------------|
| `/mcp`         | POST   | Yes (MCP key) | JSON-RPC 2.0 MCP endpoint          |
| `/mcp/tools`   | GET    | No            | List available MCP tools (discovery) |

### Authentication

MCP endpoints use a separate key mechanism from the ingest API key.

1. Navigate to **Settings → MCP (Copilot Access)** in the SOBS Web UI.
2. Click **Generate Key** and copy the displayed key (`smcp_…`).
3. Pass the key in the `X-MCP-API-Key` request header.

Keys are stored as scrypt-derived fingerprints in `sobs_app_settings` (never stored in plain text).
The scrypt salt is derived from the installation's `SOBS_SECRET_KEY`, so fingerprints are unique per deployment.

### VS Code / GitHub Copilot configuration

Add to `.vscode/mcp.json` (or your Copilot agent config):

```json
{
  "servers": {
    "sobs": {
      "type": "http",
      "url": "http://localhost:44317/mcp",
      "headers": {
        "X-MCP-API-Key": "<your-mcp-api-key>"
      }
    }
  }
}
```

### Available MCP tools

| Tool name           | Description                                                   |
|---------------------|---------------------------------------------------------------|
| `list_services`     | List all service names that have sent telemetry               |
| `query_otel_logs`   | Query `otel_logs` (filter by service, severity, search text)  |
| `query_otel_traces` | Query `otel_traces` (filter by service, span name, trace ID)  |
| `query_metrics`     | Query pre-aggregated 1-minute metrics from `v_otel_metrics_1m`|
| `query_metrics_raw` | Query raw metric points from gauge / sum / histogram tables   |
| `get_metric_names`  | List all distinct metric names and the services that emit them |
| `get_anomaly_rules` | Return configured anomaly detection rules and thresholds      |
| `get_recent_errors` | Surface recent error-level log events and error-status spans  |

### Rate limiting

Each client IP is limited to **60 requests per 60 seconds**.  Exceeding this returns `HTTP 429`
with a JSON-RPC error body.

### MCP protocol notes

- Transport: HTTP POST (Streamable HTTP / JSON-RPC 2.0).
- The `initialize` method does **not** require an API key (used for capability negotiation).
- All other methods (`tools/list`, `tools/call`) require a valid `X-MCP-API-Key` header.
- The MCP server can be disabled from **Settings → MCP** without removing keys.

## Metrics Rules Automation

SOBS includes two automation flows under **Metrics → Metrics Rules**:

- **Auto Make Metric Rules**: generates threshold or seasonality-aware rules from recent derived-signal history with a preview-first workflow and capped create.
  - **Threshold mode** (default) – static warning/critical levels derived from recent signal history.
  - **Seasonal mode** – per-time-bucket thresholds that adapt to regular day/week patterns.
- **Auto Generate Dashboard from Active Rules**: creates/updates a dashboard with one derived-signal overlay chart per matching active rule (preview-first, max chart cap, skip-existing by title).

Both auto panels include contextual help and retain their open/collapsed scope across preview/create interactions.

Fresh chDB databases are created with schema compression tuned using ZSTD plus selective Delta/T64 codecs. For encrypted local-disk testing in the container image, set `SOBS_CHDB_ENCRYPTION_KEY` and SOBS will render an internal ClickHouse config at startup and pass it to chDB automatically.

Use `/health/db` for readiness checks in orchestrated deployments when you need the probe to exercise DB availability as well as process liveness.

## Saved Reports

SOBS supports saved report presets for page filters.

- Save the current filter state from Logs, Traces, Errors, Metrics, RUM, AI, or Web Traffic.
- Re-apply saved reports from page-level report pickers or from the dedicated **Reports** page.
- Delete reports from the **Reports** page when no longer needed.
- **Export** one or more reports to a portable JSON file.
- **Import** exported reports on another instance (conflict strategies: `rename`, `replace`, `skip`).
- **Share** a report via URL.

API endpoints:

- `GET /api/reports?page_type=<logs|traces|errors|metrics|rum|ai|web_traffic>`
- `POST /api/reports`
- `DELETE /api/reports/<report_id>`
- `GET /api/reports/export?ids=<comma-separated-uuids>` – export (omit `ids` to export all)
- `POST /api/reports/import` – body: `{"sobs_reports_export":true,"version":"1","reports":[...],"on_conflict":"rename|replace|skip"}`

UI routes:

- `GET /reports`

## Query Page (Natural Language → SQL)

SOBS includes a dedicated **Query** page that turns natural-language prompts into read-only SQL against embedded chDB.

- Query availability is automatic when AI endpoint and model settings are configured.
- SQL is restricted to read-only statements (`SELECT`, `EXPLAIN`, `SHOW`, `DESCRIBE`, `WITH`).
- Query execution is row-capped by `SOBS_QUERY_MAX_ROWS` (default `1000`).
- Query results can generate chart JSON and be added to an existing dashboard.

### Table/View Access Control

All SQL executed via the Query page is validated against an **allowlist of permitted tables and views**.
Only the following observability tables may be queried; attempts to access any other table or view
(including internal `sobs_*` configuration tables) are blocked and surfaced as a validation error.

| Allowed table / view       | Contents                        |
|----------------------------|---------------------------------|
| `otel_logs`                | OpenTelemetry log records       |
| `otel_traces`              | OpenTelemetry trace/span records|
| `hyperdx_sessions`         | Browser/RUM session records     |
| `otel_metrics_gauge`       | OTEL gauge metric data points   |
| `otel_metrics_sum`         | OTEL sum metric data points     |
| `otel_metrics_histogram`   | OTEL histogram metric data points |
| `v_otel_metrics_1m`        | 1-minute metric rollup view     |
| `v_otel_metrics_anomaly`   | Anomaly detection metric view   |
| `v_derived_signals_anomaly`| Derived anomaly signals view    |

The `system` database (e.g. `system.tables`, `system.columns`) is always permitted for schema
metadata introspection. Operators can extend the allowlist via the `SOBS_QUERY_ALLOWED_TABLES`
environment variable (comma-separated table names merged with the built-in set at startup).

Routes:

- `GET /query`
- `POST /api/query/ask`
- `POST /api/query/run`
- `POST /api/query/refine-chart`
- `POST /api/query/add-to-dashboard`
- `GET /api/query/schema`

## Notifications & Webhooks

SOBS provides rule-driven notifications under **Settings → Notifications & Webhooks**.

- Channel types: `webhook`, `slack`, `email`, `browser_push`.
- Browser push supports VAPID keys from env (`SOBS_VAPID_PRIVATE_KEY`) or DB-backed key generation in the settings UI.
- Notification checks are exposed as an API for external schedulers (cron, Kubernetes CronJob, etc.).

Operational trigger endpoint:

- `POST /api/notifications/check`

## Agent Work Items And GitHub Issue Control

SOBS can create GitHub work items from agent-rule execution, but issue creation and Copilot assignment are treated as separate decisions.

Current direction:
- Search existing local work items and open GitHub issues before creating a new issue.
- Use the configured local LLM to classify candidate matches as `same`, `related`, or `unrelated`.
- Reuse or link existing issues when confidence is high enough.
- Request Copilot work only when the chosen issue is actionable, not already being worked, and within configured assignment limits.

Copilot assignment uses GitHub's supported issue-assignment flow rather than a plain issue comment mention. In GitHub terms, SOBS assigns `copilot-swe-agent[bot]` with `agent_assignment` options when the repo is eligible for Copilot cloud agent.

Operational goal:
- limit noise first with dedupe and related-issue linking
- keep Copilot assignment conservative, typically one active assignment at a time during validation and early rollout

The Work Items page is intended to make these decisions visible so operators can see when SOBS created a new GitHub issue, reused an existing one, suppressed a duplicate, or skipped assignment because work was already in flight.

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
| `SOBS_SESSION_COOKIE_NAME`  | `sobs_session` | Session cookie name for SOBS UI sessions (prevents collisions with management services using `session`) |
| `SOBS_BEHIND_TLS`           | `0`            | Enable TLS-aware hardening (secure cookies + HSTS) when running behind HTTPS termination |
| `SOBS_SESSION_COOKIE_SAMESITE` | `Lax`       | Session cookie SameSite policy (`Lax`, `Strict`, `None`) |
| `SOBS_CSRF_ORIGIN_CHECK`    | `auto`         | Enforce same-origin checks on authenticated UI state-changing methods (defaults to enabled when `SOBS_BEHIND_TLS=1`) |
| `PORT`                      | `44317`        | Listen port                                      |
| `SOBS_WRITE_QUEUE_MAX`      | `5000`         | Max buffered write operations before ingest returns `503` |
| `SOBS_WRITE_BATCH_MAX`      | `200`          | Max writes processed per DB batch |
| `SOBS_WRITE_BATCH_WAIT_MS`  | `20`           | Max milliseconds to wait for filling a write batch |
| `SOBS_QUERY_MAX_ROWS`       | `1000`         | Hard cap for rows returned by Query page SQL execution |
| `SOBS_QUERY_ALLOWED_TABLES` | _(empty)_     | Comma-separated list of additional table/view names permitted on the Query page (merged with built-in allowlist) |
| `SOBS_SETTINGS_ENCRYPTION_KEY` | _(empty)_   | Optional app-settings encryption key (base64 URL-safe Fernet key) |
| `SOBS_SETTINGS_ENCRYPTION_KEY_FILE` | _(empty)_ | Optional absolute file path containing the app-settings encryption key |
| `SOBS_AI_ENDPOINT_URL`      | _(empty)_      | Optional fallback for AI endpoint URL when not configured in Settings -> AI |
| `SOBS_AI_ENDPOINT_URL_FILE` | _(empty)_      | Optional file path with AI endpoint URL override |
| `SOBS_AI_MODEL`             | _(empty)_      | Optional fallback for AI model when not configured in Settings -> AI |
| `SOBS_AI_MODEL_FILE`        | _(empty)_      | Optional file path with AI model override |
| `SOBS_AI_API_KEY`           | _(empty)_      | Optional fallback for AI API key when not configured in Settings -> AI |
| `SOBS_AI_API_KEY_FILE`      | _(empty)_      | Optional file path with AI API key override |
| `SOBS_AI_GUARD_ENDPOINT_URL` | _(empty)_     | Optional runtime override for guard endpoint URL |
| `SOBS_AI_GUARD_ENDPOINT_URL_FILE` | _(empty)_ | Optional file path with guard endpoint URL override |
| `SOBS_AI_GUARD_MODEL`       | _(empty)_      | Optional runtime override for guard model |
| `SOBS_AI_GUARD_MODEL_FILE`  | _(empty)_      | Optional file path with guard model override |
| `SOBS_AI_GUARD_THINKING_LEVEL` | _(empty)_   | Optional runtime override for guard thinking level (`off`,`low`,`medium`,`high`) |
| `SOBS_AI_GUARD_THINKING_LEVEL_FILE` | _(empty)_ | Optional file path with guard thinking level override |
| `SOBS_AI_DLP_ENDPOINT_URL`  | _(empty)_      | Optional runtime override for DLP endpoint URL |
| `SOBS_AI_DLP_ENDPOINT_URL_FILE` | _(empty)_  | Optional file path with DLP endpoint URL override |
| `SOBS_VAPID_PRIVATE_KEY`    | _(empty)_      | Optional browser-push private key override (takes precedence over DB-stored key) |
| `SOBS_VAPID_SUBJECT`        | `mailto:sobs@localhost` | Subject claim used when signing VAPID JWTs |
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

### Settings Secret Storage

SOBS can optionally encrypt sensitive values stored in app settings tables.

- If `SOBS_SETTINGS_ENCRYPTION_KEY` (or `SOBS_SETTINGS_ENCRYPTION_KEY_FILE`) is present, sensitive setting values are encrypted before persistence.
- If no settings encryption key is configured, values are stored in plaintext (backward-compatible behavior).
- Existing plaintext values remain readable; after saving a setting while encryption is enabled, the new value is persisted encrypted.

Sensitive values include AI and notification secrets such as API keys, tokens, webhook URLs, SMTP passwords, and the VAPID private key setting.

### AI Guard Behavior

AI helper guard checks are fail-closed.

- If guard endpoint/model settings are missing, requests are blocked.
- If the guard call fails or returns an invalid response, requests are blocked.
- The guard model must reply `safe` (allowed) or `unsafe` (blocked) on the first line.
- Guard thinking can be tuned independently from assistant thinking via environment overrides.
  For thinking-capable guard models, the default auto behavior uses low effort when no explicit guard thinking level is set.
- [Llama Guard 3](https://ollama.com/library/llama-guard3) is the recommended guard model.
  It uses the MLCommons hazard taxonomy and returns a two-line response when blocking:
  ```
  unsafe
  S2
  ```
  The category code (S1–S14) is surfaced in the blocked error message shown to the user,
  e.g. `blocked (S2: Non-Violent Crimes)`.

Configure guard settings in **Settings -> AI** before enabling AI helper flows.

The contextual AI helper keeps guard-first enforcement, then streams the base-model response back to the browser once the prompt is allowed. Guard calls are not streamed.

### AI Settings from Env and Secrets

AI, guard, and DLP settings can be managed in Settings -> AI and/or externally with environment variables or mounted secret files.

- Runtime precedence is: DB settings from **Settings -> AI**, then `*_FILE` values, then direct env values.
- This allows cluster-managed config in Docker/Kubernetes using ConfigMaps, Secrets, or mounted files.
- File-based variants (`*_FILE`) are useful for mounted secret volumes.

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
curl -N http://localhost:44317/tail

# Stream logs only
curl -N "http://localhost:44317/tail?source=logs"

# Stream traces only
curl -N "http://localhost:44317/tail?source=traces"

# Filter by service
curl -N "http://localhost:44317/tail?service=myapp"

# Combine source and service filter
curl -N "http://localhost:44317/tail?source=logs&service=myapp"
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


Optional browser RUM client auth (origin-bound tokens):

- `SOBS_RUM_CLIENT_AUTH_MODE=none|origin` (default `none`)
- `SOBS_RUM_CLIENT_SIGNING_KEY=<secret>` (required when mode is `origin`)
- `SOBS_RUM_CLIENT_TOKEN_TTL_SEC=900` (optional)

Recommended flow:

1. Your backend calls `POST /v1/rum/client-token` (with API key if enabled).
2. Your backend returns the token to your own web app.
3. Browser sends token via `data-sobs-client-token-url` or `SOBS.setClientAuthToken(...)`.
4. SOBS accepts RUM only when token origin matches request origin/referer.

Optional server-side JS stack source-map remapping:

- `SOBS_SOURCE_MAP_ENABLE=true`
- `SOBS_SOURCE_MAP_DIR=/path/to/source-maps`

When enabled, SOBS attempts to remap JavaScript stack frames (RUM + direct `/v1/errors`) to original source locations using `.map` files in `SOBS_SOURCE_MAP_DIR`.

App/release/artifact registry (Phase 1 scaffolding):

- `POST /v1/apps`
- `GET /v1/apps`
- `GET /v1/apps/{app_id}`
- `PATCH /v1/apps/{app_id}`
- `POST /v1/apps/{app_id}/releases`
- `GET /v1/apps/{app_id}/releases`
- `GET /v1/releases/{release_id}`
- `POST /v1/releases/{release_id}/artifacts/meta`
- `GET /v1/releases/{release_id}/artifacts`

Environment seed support for app/release/artifact registry:

- `SOBS_APP_REGISTRY_SEED_JSON` (inline JSON)
- `SOBS_APP_REGISTRY_SEED_JSON_FILE` (path to JSON file)

Seed JSON shape:

```json
{
  "apps": [
    {
      "name": "checkout-web",
      "slug": "checkout-web",
      "ownerTeam": "frontend",
      "releases": [
        {
          "version": "1.2.3",
          "commitSha": "abc123",
          "environment": "prod",
          "artifacts": [
            {
              "artifactType": "js_sourcemap",
              "name": "app.min.js.map",
              "storageRef": "s3://symbols/checkout/1.2.3/app.min.js.map"
            }
          ]
        }
      ]
    }
  ]
}
```

CI helper script for registry onboarding:

```bash
python scripts/register_release_artifacts.py \
  --base-url "${SOBS_BASE_URL:-http://127.0.0.1:44317}" \
  --api-key "$SOBS_API_KEY" \
  --app-name checkout-web \
  --app-slug checkout-web \
  --release-version "${RELEASE_VERSION}" \
  --commit-sha "${GITHUB_SHA}" \
  --environment prod \
  --artifacts-file ./build/sobs-artifacts.json
```

Equivalent bash entrypoint (same behavior, delegates to Python helper):

```bash
bash scripts/register_release_artifacts.sh \
  --base-url "${SOBS_BASE_URL:-http://127.0.0.1:44317}" \
  --api-key "$SOBS_API_KEY" \
  --app-name checkout-web \
  --release-version "${RELEASE_VERSION}" \
  --commit-sha "${GITHUB_SHA}" \
  --environment prod \
  --artifacts-file ./build/sobs-artifacts.json
```

The helper is idempotent-oriented:

- Reuses existing app by slug/name.
- Reuses existing release by version+commit+environment+build.
- Skips artifact metadata already present by `(artifactType, name, storageRef)`.

Environment-first usage is also supported (useful in CI):

- `SOBS_BASE_URL`
- `SOBS_API_KEY`
- `SOBS_APP_NAME`
- `SOBS_APP_SLUG`
- `SOBS_OWNER_TEAM`
- `SOBS_APP_REPO_URL`
- `SOBS_DEFAULT_ENVIRONMENT`
- `SOBS_RELEASE_VERSION`
- `SOBS_RELEASE_COMMIT_SHA`
- `SOBS_RELEASE_BUILD_ID`
- `SOBS_RELEASE_ENVIRONMENT`
- `SOBS_RELEASED_AT`
- `SOBS_RELEASE_METADATA_JSON`
- `SOBS_RELEASE_ARTIFACTS_JSON` or `SOBS_RELEASE_ARTIFACTS_JSON_FILE`

Artifacts JSON array example (`sobs-artifacts.json`):

```json
[
  {
    "artifactType": "js_sourcemap",
    "name": "app.min.js.map",
    "contentType": "application/json",
    "size": 3210,
    "storageRef": "s3://symbols/checkout/1.2.3/app.min.js.map",
    "checksumSha256": "..."
  }
]
```

GitHub Actions example step:

```yaml
- name: Register SOBS release artifacts
  env:
    SOBS_BASE_URL: ${{ secrets.SOBS_BASE_URL }}
    SOBS_API_KEY: ${{ secrets.SOBS_API_KEY }}
    SOBS_APP_NAME: checkout-web
    SOBS_APP_SLUG: checkout-web
    SOBS_OWNER_TEAM: frontend
    SOBS_APP_REPO_URL: ${{ github.server_url }}/${{ github.repository }}
    SOBS_DEFAULT_ENVIRONMENT: prod
    SOBS_RELEASE_VERSION: ${{ github.ref_name }}
    SOBS_RELEASE_COMMIT_SHA: ${{ github.sha }}
    SOBS_RELEASE_BUILD_ID: ${{ github.run_id }}
    SOBS_RELEASE_ENVIRONMENT: prod
  run: |
    cat > sobs-artifacts.json <<'JSON'
    [
      {
        "artifactType": "js_sourcemap",
        "name": "app.min.js.map",
        "contentType": "application/json",
        "size": 3210,
        "storageRef": "s3://symbols/checkout/${{ github.sha }}/app.min.js.map"
      }
    ]
    JSON

    bash scripts/register_release_artifacts.sh \
      --artifacts-file ./sobs-artifacts.json
```

```bash
# Basic auth
curl -N http://localhost:44317/tail \
  -H "Authorization: Basic $(printf 'admin:secret' | base64)"

# Bearer token (external auth)
curl -N http://localhost:44317/tail \
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

## Web Traffic & CVE Enrichment

### Web Traffic

The **Web Traffic** page (`/web-traffic`) visualises RUM event data as geo and distribution analytics:

- **World map** – visitor countries rendered with Apache ECharts.
- **Distribution charts** – browsers, operating systems, display timezones, languages, and device types.
- All charts support date-range filtering via the filter bar.
- Route: `GET /web-traffic`

Data requires at least one RUM event to be ingested.  Enable **IP geo-lookup** in **Settings → Enrichment** to
populate the country map.

### CVE Enrichment

The **CVE Findings** page (`/enrichment/cve`) surfaces known vulnerabilities detected in the software libraries observed from your OTEL attributes:

- **Automated daily scan** – library names are extracted from incoming OTEL attributes and checked against [OSV.dev](https://osv.dev/) (no API key required).
- **Manual trigger** – use the *Scan now* button to re-scan on demand.
- **Disposition tracking** – mark each finding as *false positive*, *accepted*, *won't fix*, or *resolved*.
- Enable scanning under **Settings → Enrichment → CVE Scanning**.

API endpoints:

- `GET /api/enrichment/cve/findings`
- `POST /api/enrichment/cve/scan`
- `POST /api/enrichment/cve/findings/<osv_id>/disposition`

## Errors – Grouped/Deduplicated View

The **Errors** page supports a best-effort deduplicated view.

- Click the **Group** button in the page header to switch to grouped mode (`?grouped=1`).
- Groups are formed by fingerprinting `(service, error_type, message)` across recent errors.
- Each group shows a count badge (×N), `first_seen`, and `last_seen` timestamps.
- Sort grouped results by **Count** (default), **Last Seen**, or **Service**.
- Click **Grouped** (active state) to return to the individual-error view.
- Each error card includes an **Incident View** button to jump directly to the incident evidence view.

## Incident Evidence View

The **Incident** page (`/incident`) aggregates all available evidence around a single error or trace into one view, reducing the time needed to understand the blast radius of an issue.

- Accepts `trace_id`, `error_id`, `from_ts`, `to_ts`, or `window_minutes` (default 30, max 180).
- Shows the primary event, related errors, log counts, span counts, RUM evidence, anomaly state, and linked work items.
- Reachable via the **Incident View** button on any error card or trace detail panel.

Route: `GET /incident`

## Table Explorer

The **Table Explorer** page (`/table-explorer`) provides a read-only visual schema and sample-data browser for all observability tables permitted by the Query page allow-list.

- **Search** – filter tables or columns by name in real time.
- **Schema** – expand any table to see column names and data types.
- **Sample data** – view up to 10 sample rows per table.
- Accessible from the **Query** page via the *Explore Tables* link.

Route: `GET /table-explorer`

## Kubernetes

Deploy as a standalone pod:

```bash
kubectl apply -f k8s/deployment.yaml
```

Or as a **sidecar** – see `k8s/sidecar.yaml` for instructions.

## Local AI Quickstart (Ollama Recommended)

For most users, the easiest manual AI setup is local Ollama.

1. Start Ollama (separate terminal):

```bash
ollama serve
```

2. Pull at least one model (example):

```bash
ollama pull llama3.1:8b
```

3. Start SOBS with Ollama-backed AI/guard settings:

```bash
./scripts/start_ollama_ai_test.sh
```

Optional model overrides:

```bash
SOBS_AI_MODEL=qwen2.5:7b-instruct \
SOBS_AI_GUARD_MODEL=qwen2.5:7b-instruct \
SOBS_AI_GUARD_THINKING_LEVEL=low \
./scripts/start_ollama_ai_test.sh -- .venv/bin/python app.py
```

The script validates Ollama availability at `OLLAMA_BASE_URL` (default `http://127.0.0.1:11434`), exports `SOBS_AI_*` env vars, and runs your command (default: `python app.py`).

By default it also starts a local browser demo app for RUM/replay testing at `http://127.0.0.1:5005`.
You can disable it with `START_EXAMPLE_APP=0`.
It also exports `SOBS_RUM_ASSET_SIGNING_KEY` so the demo app can upload replay/screenshot assets using signed auth.

This local Ollama path does not use Kubernetes and does not require `kubectl`.

## Spark Cluster AI Test Script (Advanced)

For local manual testing against cluster-hosted LLM/guard/DLP services, use:

```bash
./scripts/start_spark_ai_test.sh
```

By default the script uses:

- `kubectl --kubeconfig ~/.kube/microk8s.config`
- namespace `default`
- service resources `svc/vllm-llm`, `svc/vllm-embeddings`, `svc/dlp`
- DLP shared secret `infra-secrets` key `dlp-shared-secret`

You can override any of these via environment variables before running the script, for example:

```bash
KUBE_NAMESPACE=ml \
INFRA_NAMESPACE=infra \
LLM_RESOURCE=svc/my-vllm \
DLP_RESOURCE=svc/my-dlp \
DLP_SECRET_NAME=my-infra-secrets \
DLP_SECRET_KEY=dlp-token \
./scripts/start_spark_ai_test.sh -- .venv/bin/python app.py
```

The script starts local port-forwards for LLM, embeddings, and DLP, exports `SOBS_AI_*` endpoint/model/key env vars for SOBS, and then runs the command you pass (default: `python app.py`).

## Screenshots

| Summary | Summary + AI Assistant | AI Transparency |
|---|---|---|
| ![Summary](static/help/summary.png) | ![Summary AI Assistant](static/help/summary_ai_assistant.png) | ![AI Transparency](static/help/ai.png) |

| Custom Dashboard | Traces Drilldown | Query |
|---|---|---|
| ![Custom Dashboard](static/help/dashboard.png) | ![Traces Drilldown](static/help/traces_drilldown.png) | ![Query](static/help/query.png) |

| Logs | Traces |
|---|---|
| ![Logs](static/help/logs.png) | ![Traces](static/help/traces.png) |

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
python scripts/load_example.py --base http://127.0.0.1:44317 --total 420 --workers 28

# Realistic paced mode for UI demos
python scripts/load_example.py --base http://127.0.0.1:44317 --mode realistic --rps 3 --jitter-ms 250 --total 180 --workers 8
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

## Output Masking (PII / Secret Redaction)

SOBS redacts PII and secrets from web UI output, outbound notification
messages, and GitHub issue bodies using `masking.py` - a shared rule-based masking layer
backed by the [`loggingredactor`](https://pypi.org/project/loggingredactor/) library.

### What is masked

| Category | Examples |
|---|---|
| Email addresses | `user@company.com` |
| JWT tokens | `eyJhbGci…` |
| Bearer tokens | `Authorization: Bearer …` |
| AWS access key IDs | `AKIA…` |
| US Social Security Numbers | `123-45-6789` |
| Credit card numbers | Visa, Mastercard, Amex, Discover |
| PEM private keys | `-----BEGIN PRIVATE KEY-----…` |
| Dict/object keys | `password`, `api_key`, `token`, `secret`, … |

Masking is **display-only** – it does not modify the data stored in the database.

### Using the `mask` Jinja filter

Any template can use the `mask` filter to redact a value before rendering:

```html
{{ log.body|mask }}
{{ event.attributes|mask }}
```

### Extending the rule set

Edit `masking.py`:

* **New sensitive value patterns** – add a regex string to `SENSITIVE_PATTERNS`.
  The full regex match is replaced with `****`.

  ```python
  SENSITIVE_PATTERNS.append(r"\bMY_SECRET_PATTERN\b")
  masking.build_redacting_filter()   # rebuild singleton
  ```

* **New sensitive key names** – add a lowercase name to `SENSITIVE_KEYS`.
  Any dict key whose lowercased name is in this set will have its value masked,
  regardless of the value content.

  ```python
  # masking.py
  SENSITIVE_KEYS = frozenset({
      ...
      "my_new_sensitive_key",
  })
  ```

Masking is also applied automatically to:
* Outbound notification payloads (`summary` field sent to Slack/webhook/email)
* GitHub issue titles and bodies created by the agent flow

### Settings UI and APIs

SOBS now includes a dedicated masking settings page:

* `GET /settings/masking` for managing custom sensitive keys and regex patterns
* `GET /settings/help/masking` for implementation guidance and screenshot workflow notes
* `POST /api/settings/masking/preview` to preview redaction output for text/JSON values
* `GET /api/settings/masking/rules` to fetch effective key/pattern rules for browser helpers

### Global Output Masking Toggle

Global masking across UI/API output surfaces is controlled via:

* `POST /settings/masking/output`

When enabled (default), masking is applied to template `mask` filter output, masking preview
responses, masked JSON display payloads, notification summaries, and GitHub issue payload masking.
When disabled, masking is bypassed for those output surfaces.

### Per-Issue Masking Toggle (Raise Issue Modal)

On Errors/Traces raise-issue modals, the per-issue `mask_output` checkbox follows this contract:

* **Global masking ON**: checkbox is disabled, and issue payload masking is forced on.
* **Global masking OFF**: checkbox is enabled, and the posted `mask_output` value controls whether that specific issue payload is masked.

This keeps secure-by-default behavior when global masking is active while still allowing
explicit per-issue masking control during raw-debug workflows when global masking is off.

### SQL Output Masking Toggle

SQL text returned by NLQ/chart endpoints can be controlled independently via:

* `POST /settings/masking/sql-output`

When enabled, SOBS masks sensitive fragments in SQL/query fields (`sql`, `query`,
`sample_sql`, `override_sql`) for query/chart responses. When disabled, SQL text is
returned unmasked for debugging workflows.

The SQL toggle is evaluated only when global output masking is enabled.

### Replay and Artifact URLs

Replay/artifact metadata remains masked in visible labels and summaries, while action URLs
used by "View Replay", "View Artifact", and "Open Raw" remain functional so operators can
open attached resources directly from the UI.


