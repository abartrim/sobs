# Web Traffic & Enrichment Design

## Overview

The Web Traffic & Enrichment subsystem adds a `/web-traffic` page with IP-to-geo visualisation,
a top-URLs and event-type breakdown, browser-context analytics, and automated CVE scanning via the
dedicated `/enrichment/cve` page. The merged library/dependency inventory is now collected in the
backend for scanning, while the standalone libraries API/panel remains a follow-up item. All data
sources are open/free (MIT or Apache 2.0) and the system operates without API keys or external
service registrations.

---

## Goals

- Visualise where web traffic originates (IP → country choropleth + bar chart)
- Inventory every library/SDK version observed across all services, with source provenance
- Automatically scan those libraries for known CVEs and surface findings with severity badges
- Allow analysts to triage (disposition) findings so acknowledged risk does not resurface as noise
- Remain fully configurable: each enrichment feature can be disabled independently

---

## Architecture

### Enrichment tiers — library detection

Library versions are collected from three sources, merged and deduplicated:

| Tier | Source | Precision | Notes |
|------|--------|-----------|-------|
| 1 | Release registry `MetadataJson["dependencies"]` | Lockfile-exact | Populated by CI (`--dependencies-json`) or auto-fetched from GitHub at release tag |
| 2 | OTEL `ResourceAttributes` (`telemetry.sdk.*`) | SDK only | Traces + logs; covers any service emitting OTEL |
| 3 | OTEL `ScopeName / ScopeVersion` | Instrumentation libs | Traces + logs; broader than Tier 2 but ecosystem detection is heuristic |

The merge is performed by `_collect_library_inventory(db)`, which replaces the original
`_extract_library_versions_from_otel`. Each entry carries a `source` field
(`release_registry`, `otel_sdk`, `otel_scope`) and, when from the registry, `app_name`,
`release_version`, and `environment`.

### GitHub dependency fetch (Tier 1 auto-pull) — multiple repos

SOBS supports any number of apps and repos without additional configuration.
Each app registered in `sobs_apps` has its own `RepoUrl`
(set via `register_release_artifacts.py --repo-url` or the SOBS Release Registry UI).
The enrichment scanner iterates every app where `RepoUrl LIKE 'https://github.com/%'`.

- **Authentication**: single global `ai.github_token` (set in AI Settings); requires
  `contents:read` scope. The same token is reused for all configured repos, so use a
  GitHub App installation token or a fine-grained PAT with per-repo access as needed.
- **API call**: `GET /repos/{owner}/{repo}/contents/{file}` with ref fallback
  (`refs/tags/{version}` → `refs/tags/v{version}` → `refs/heads/{version}` → `{version}`)
  using the existing `_get_async_http_client()` helper.
- **Lockfile priority** (first match wins per app): `requirements.txt` → PyPI,
  `package-lock.json` → npm, `go.sum` → Go, `Gemfile.lock` → RubyGems.
- **Storage**: parsed dependencies are stored as a `dependencies-lockfile` artifact
  row keyed on `(ReleaseId, ArtifactType='dependencies-lockfile', Name=<lockfile>)`.
  Re-scans are idempotent via ReplacingMergeTree.

**CI push (recommended):** Avoid the polling step entirely by running
`register_release_artifacts.py --requirements-file requirements.txt` (or `--dependencies-json`)
in your CI pipeline. GitHub token / repo access is not needed in this path.

**Requirement:** `ai.github_token` needs `contents:read` in addition to the `issues:write`
already used for the GitHub agent action.

### IP geolocation

- Library: **geoip2fast** (MIT license)
- Database: bundled IANA/RIR delegated statistics files (public domain)
- All lookups are local — no external network calls, no API key, no registration
- An in-process LRU cache (`OrderedDict`, max 2000 entries, `threading.Lock` guard) avoids
  redundant lookups across requests
- Private/loopback/link-local IPs are filtered before any lookup

### CVE scanning

- API: **OSV.dev** (Apache 2.0, free, no API key)
- Detection: `_collect_library_inventory(db)` (merged three-tier inventory)
- Schedule: background asyncio task (`_cve_scanner_loop`), 30 s initial delay then 24 h interval
- Manual trigger: `POST /api/enrichment/cve/scan` ("Scan now" button in UI)
- Findings stored in `sobs_cve_findings` (ReplacingMergeTree keyed on package+ecosystem+version+osv_id)
- Max 10 vulnerabilities stored per package per scan (`_CVE_MAX_VULNS_PER_PKG`)

---

## Data Model

### `sobs_cve_findings`

Stores CVE scan results. `ReplacingMergeTree(ScannedAt)` — each rescan refreshes entries.

```sql
Package      String
Ecosystem    LowCardinality(String)
Version      String
ServiceName  LowCardinality(String)
OsvId        String
CveIds       String          -- comma-separated CVE-* IDs
Summary      String
Severity     LowCardinality(String)
Published    String
ScannedAt    DateTime64(3)
ORDER BY (Package, Ecosystem, Version, OsvId)
```

### `sobs_cve_dispositions`

Triage state for CVE findings. Separate from `sobs_cve_findings` so re-scans never overwrite
analyst decisions. `ReplacingMergeTree(Version_)`.

```sql
OsvId        String
Package      String
Ecosystem    LowCardinality(String)
Version      String
Disposition  LowCardinality(String)   -- open | accepted | false_positive | fixed
Note         String
CreatedAt    DateTime64(3)
UpdatedAt    DateTime64(3)
Version_     UInt64                   -- optimistic-concurrency version
ORDER BY (OsvId, Package, Ecosystem, Version)
```

Disposition semantics:

| Value | Meaning | Auto-expire? |
|-------|---------|-------------|
| `open` | Default — not yet reviewed | No |
| `accepted` | Known risk, accepted | No |
| `false_positive` | Not applicable to this usage | No |
| `fixed` | Remediated — suppress until version changes | Yes — expires when Package+Ecosystem appears at a new Version |

Default UI view hides `accepted`, `false_positive`, `fixed`. A "Show all" toggle reveals them.

---

## API Endpoints

Current implementation note: the CVE findings, CVE scan, libraries inventory, and CVE disposition
workflow endpoints are live.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/web-traffic` | Web traffic page |
| `GET` | `/api/web-traffic/geo` | IP → country aggregation (geoip2fast, local) |
| `GET` | `/api/enrichment/libraries` | Merged library inventory with CVE counts and provenance |
| `GET` | `/api/enrichment/github/repo-health` | Version-scoped GitHub issues/PRs/security summary for registered release versions |
| `GET` | `/api/enrichment/cve/findings` | Stored CVE findings (with disposition joined) |
| `POST` | `/api/enrichment/cve/scan` | Trigger immediate CVE scan |
| `POST` | `/api/enrichment/cve/findings/<osv_id>/disposition` | Set disposition + optional note |
| `GET` | `/settings/enrichment` | Enrichment settings page |
| `POST` | `/settings/enrichment` | Save enrichment settings |

### `GET /api/enrichment/libraries` response shape

```json
{
  "ok": true,
  "libraries": [
    {
      "package": "flask",
      "ecosystem": "PyPI",
      "version": "3.0.0",
      "service": "checkout-api",
      "source": "release_registry",
      "app_name": "checkout-api",
      "release_version": "1.2.3",
      "environment": "prod",
      "cve_count": 0,
      "status": "clean"
    },
    {
      "package": "opentelemetry-sdk",
      "ecosystem": "PyPI",
      "version": "1.22.0",
      "service": "checkout-api",
      "source": "otel_sdk",
      "cve_count": 0,
      "status": "clean"
    },
    {
      "package": "some-scope",
      "ecosystem": "",
      "version": "1.0.0",
      "service": "frontend",
      "source": "otel_scope",
      "cve_count": 0,
      "status": "unknown_ecosystem"
    }
  ],
  "scanned_at": "2026-04-04T12:00:00"
}
```

### `POST /api/enrichment/cve/findings/<osv_id>/disposition` request body

```json
{
  "package": "some-old-lib",
  "ecosystem": "PyPI",
  "version": "2.1.0",
  "disposition": "accepted",
  "note": "Only used in internal tooling, no external exposure"
}
```

---

## Settings

All settings are stored in `sobs_settings` via `_get_app_setting` / `_set_app_setting`.

| Key | Default | Description |
|-----|---------|-------------|
| `enrichment.geo_enabled` | `true` | Enable IP geolocation lookups |
| `enrichment.cve_enabled` | `true` | Enable CVE background scanning |
| `enrichment.cve_last_scan` | `""` | ISO timestamp of last completed scan |
| `enrichment.github_backfill_max_releases` | `300` | Max releases checked per CVE scan for GitHub lockfile backfill |

`ai.github_token` (in AI settings) is shared with the repo dependency fetch feature.
It requires `contents:read` (for repo dep fetching) in addition to `Issues: read/write`
(for agent flow GitHub issue creation).

---

## `service.version` on ingest

`service.version` is a standard OTEL resource attribute. SOBS extracts it from
`ResourceAttributes` alongside `service.name` during trace and log ingest and stores it
(no enforcement — missing `service.version` produces a quality hint in the Libraries panel,
not a rejection). This enables correlating CVE findings to the exact deployed version of each
service.

---

## CI integration

### `register_release_artifacts.py` — new arguments

| Argument | Env var | Description |
|----------|---------|-------------|
| `--dependencies-json` | `SOBS_RELEASE_DEPENDENCIES_JSON` | JSON array of `{name, version, ecosystem}` objects |
| `--requirements-file` | `SOBS_RELEASE_REQUIREMENTS_FILE` | Path to `requirements.txt`; auto-parsed to PyPI deps |

Example CI usage:

```bash
# Python — generate from pip freeze
DEPS=$(pip freeze | python3 -c "
import sys, json
lines = [l.strip() for l in sys.stdin if '==' in l]
print(json.dumps([
  {'name': p.split('==')[0], 'version': p.split('==')[1], 'ecosystem': 'PyPI'}
  for p in lines
]))
")

python scripts/register_release_artifacts.py \
  --app-name checkout-api \
  --release-version "$VERSION" \
  --commit-sha "$GITHUB_SHA" \
  --dependencies-json "$DEPS"
```

```bash
# npm — generate from package-lock.json
DEPS=$(node -e "
const lock = require('./package-lock.json');
const deps = Object.entries(lock.packages || {})
  .filter(([k]) => k && k !== '')
  .map(([k, v]) => ({name: k.replace(/^node_modules\//, ''), version: v.version, ecosystem: 'npm'}));
console.log(JSON.stringify(deps));
")

python scripts/register_release_artifacts.py \
  --app-name browser-frontend \
  --release-version "$VERSION" \
  --dependencies-json "$DEPS"
```

---

## UI — Web Traffic page

### Sections

1. **Filters** — time range (from/to), consistent with other SOBS pages
2. **Visitor Locations** — ECharts world choropleth fed by `/api/web-traffic/geo` (async)
3. **Top Countries** — horizontal bar chart (top 15)
4. **Event Types** — donut/pie chart
5. **Top URLs** — table, top 20
6. **Detected Libraries** — async-loaded table (below), from `/api/enrichment/libraries`
7. **CVE / Vulnerability Findings** — from pre-loaded server-side data + disposition controls

### Detected Libraries panel columns

| Column | Notes |
|--------|-------|
| Package | `ecosystem/name@version` |
| Service | `service.name` (+ `service.version` when present) |
| Source | Badge: Release registry / OTEL SDK / OTEL Scope |
| Status | ✓ Clean / ⚠ N CVEs / — Unknown ecosystem / ⚠ No version tag |

### Disposition controls (per CVE finding row)

- Dropdown: Open / Accepted Risk / False Positive / Fixed
- Optional freetext note
- Inline `POST` to `/api/enrichment/cve/findings/<osv_id>/disposition`
- Default view suppresses non-`open` findings; "Show all" toggle reveals them

---

## Real IP capture via proxy headers

SOBS reads `client.ip` from incoming RUM events. For deployments behind a reverse proxy or
Kubernetes ingress, the real visitor IP is only available in forwarded headers. Ensure your
proxy passes these headers and that SOBS is configured to trust them.

### nginx (reverse proxy)

```nginx
location / {
    proxy_pass         http://sobs:44317;
    proxy_set_header   X-Forwarded-For  $proxy_add_x_forwarded_for;
    proxy_set_header   X-Real-IP        $remote_addr;
    proxy_set_header   Host             $host;
}
```

The RUM client automatically includes `client.ip` from `X-Real-IP` when the header is present.
If your SOBS instance sits behind multiple proxies, use the **first** untrusted IP in the
`X-Forwarded-For` chain:

```nginx
# Extract the leftmost (original client) address
map $http_x_forwarded_for $real_client_ip {
    ~^([^,]+) $1;
    default   $remote_addr;
}
server {
    ...
    proxy_set_header X-Real-IP $real_client_ip;
}
```

### Kubernetes Ingress (nginx-ingress-controller)

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: sobs
  annotations:
    nginx.ingress.kubernetes.io/use-forwarded-headers: "true"
    nginx.ingress.kubernetes.io/compute-full-forwarded-for: "true"
    nginx.ingress.kubernetes.io/forwarded-for-header: "X-Forwarded-For"
spec:
  rules:
    - host: sobs.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: sobs
                port:
                  number: 44317
```

### Traefik (Docker / K8s)

```yaml
# traefik.yml / static config
entryPoints:
  web:
    address: ":80"
    forwardedHeaders:
      trustedIPs:
        - "10.0.0.0/8"      # trust internal cluster CIDR
        - "172.16.0.0/12"
        - "192.168.0.0/16"
```

### Verifying

Navigate to **Web Traffic → Visitor Locations**. If IPs resolve to expected countries,
headers are configured correctly. You can also check the raw event body via the
SOBS **Query** page:

```sql
SELECT client_ip, geo_country, geo_city
FROM sobs_rum_events
WHERE client_ip != '' AND client_ip IS NOT NULL
ORDER BY ts DESC
LIMIT 20
```

---

## Implementation status

### Later stage backlog (not in current phase)

- **GitHub Repo Connect wizard (OAuth/GitHub App repo picker)** — deferred.
  - Goal: reduce manual form entry by letting users connect GitHub, browse accessible repos,
    and import selected repos into SOBS in one flow.
  - Scope (later): auth start/callback, repo list fetch, multi-select import, and persistence
    into `sobs_apps`/release tracking.
  - Out of scope for now: automatic PAT creation; manual token entry remains supported.

| Item | Status |
|------|--------|
| `/web-traffic` page, geo map, top URLs, event types chart | ✅ Done (PR #72) |
| `GET /api/web-traffic/geo` | ✅ Done |
| `GET /api/enrichment/cve/findings`, `POST /api/enrichment/cve/scan` | ✅ Done |
| `GET /settings/enrichment`, `POST /settings/enrichment` | ✅ Done |
| RUM `client.ip` extraction and storage | ✅ Done |
| Navigation link (Web Traffic between RUM and AI) | ✅ Done |
| Settings hub Enrichment card | ✅ Done |
| `sobs_cve_findings` schema | ✅ Done |
| CVE background scanner loop (24 h) | ✅ Done |
| All panels as accordions in web_traffic.html | ✅ Done |
| Browser-context aggregation APIs (`/api/web-traffic/browsers`, `/os`, `/timezones`, `/languages`, `/devices`) | ✅ Done |
| TZ selector + date range picker wired up | ✅ Done |
| World map bundled locally (no CDN dependency) | ✅ Done |
| `--dependencies-json` / `--requirements-file` in `register_release_artifacts.py` | ✅ Done |
| Proxy header documentation | ✅ Done |
| Geo cache bound fix (batch eviction) | 🔲 Planned |
| `otel_logs` ScopeVersion coverage in library extraction | ✅ Done |
| `service.version` extraction on ingest | 🔲 Planned |
| `_fetch_release_deps_from_github` (GitHub Contents API) | ✅ Done |
| Multiple repos support (per-app RepoUrl, shared token) | ✅ Done |
| Configurable GitHub backfill scan cap | ✅ Done |
| GitHub repo health panel (version-scoped issues/PRs/security) | ✅ Done |
| `_collect_library_inventory` (three-tier merge) | ✅ Done |
| `GET /api/enrichment/libraries` endpoint | ✅ Done |
| Detected Libraries panel in cve.html | ✅ Done |
| `sobs_cve_dispositions` schema | ✅ Done |
| `POST /api/enrichment/cve/findings/<osv_id>/disposition` | ✅ Done |
| Disposition join in findings API + page | ✅ Done |
| `fixed` disposition auto-expiry on version change | ✅ Done |
| Per-row disposition controls in cve.html | ✅ Done |
| CVE dedicated page (`/enrichment/cve`) | ✅ Done |
| Note in enrichment settings re: `contents:read` token scope | ✅ Done |
| GitHub Repo Connect wizard (OAuth/App repo picker) | 🔲 Later stage |
| Tests for all implemented items in this phase | ✅ Done |
