# SOBS — Go port

Faithful Go port of `../app.py` (Quart/Python → `net/http`/Go). One `main`
package; one `sNN_*.go` file per section of the original (see `CONVENTIONS.md`).

## Build

```sh
go build -o sobs .
```

Compiles clean with zero errors (Go 1.26+). `chdb-go` uses **purego** (runtime
`dlopen`), so `libchdb` is **not** needed to build — only to run.

## Run

The binary needs the embedded ClickHouse native library `libchdb.so`
(`.dylib` on macOS) on the loader path at startup — the same native dependency
the Python app gets from the `chdb` wheel. Without it the process panics at
`chdb-purego.init` with `dlopen(libchdb.so) ... no such file`.

Install libchdb (matching your platform), then:

```sh
# point the loader at the dir containing libchdb.so/.dylib
export DYLD_LIBRARY_PATH=/path/to/libchdb   # macOS
export LD_LIBRARY_PATH=/path/to/libchdb     # Linux

PORT=44317 SOBS_DATA_DIR=./data ./sobs
```

### Environment

Same `SOBS_*` / `PORT` / `HYPERCORN_BIND` / `GUNICORN_BIND` variables as the
Python app (default bind `0.0.0.0:44317`). Templates are read from
`../templates`, static assets from `../static` (resolved relative to the repo
root or the executable).

## Layout

| File | Origin (app.py lines) | Area |
|------|----------------------|------|
| s00_core.go | infra | route table, JSON/template/flash/http helpers |
| s01_setup.go | 1–710 | middleware, CORS, Fernet settings encryption, config |
| s02_db.go / s02_schema.go | 711–2324 | chDB connection, write queue, schema |
| s03_retention_ai_settings.go | 2325–3404 | retention, AI pricing/settings, CI keys |
| s04_llm_guard.go | 3405–5647 | LLM calls, guard/DLP, GitHub issues |
| s05_agents.go | 5648–7514 | agent rules/runs, write worker |
| s06_sse_auth_util.go | 7515–9650 | SSE, auth, OTLP convert, ingest helpers |
| s07_ingest.go | 9651–10742 | OTLP/RUM/AI/error ingest, registry APIs |
| s08_summary_logs.go | 10743–11513 | Summary + Logs UI |
| s09_signals_tags.go | 11514–13309 | derived signals, SQL-WHERE guard, tag rules |
| s10_metrics_errors_traces.go | 13310–15679 | Metrics/Errors/Traces UI |
| s11_span_incident_geo_cve.go | 15680–17302 | span API, incident, geo, CVE |
| s12_rum_traffic_workitems_ai.go | 17303–19227 | RUM, web-traffic, work-items, AI |
| s13_dashboards.go | 19228–22533 | custom eCharts dashboards |
| s14_reports_settings_tags_api.go | 22534–24505 | reports, settings, tags/regex APIs |
| s15_notifications.go | 24506–26598 | notifications, VAPID/Web Push, app-settings |
| s16_settings_repos_agents.go | 26599–28767 | health, AI/repo/agent settings, AI helper |
| s17_vanna.go | 28768–30183 | NL→SQL query service |
| s18_query_explorer_k8s.go | 30184–31784 | query page, table explorer, k8s |
| s19_datamgmt_wizard.go | 31785–33932 | data management, setup wizard, onboarding |
| main.go | 33933–33957 | startup, route mux, graceful shutdown |
| s90_masking.go / s91_mcp.go / s92_telemetry_stub.go | masking.py / mcp.py / telemetry | ported modules |

## Runtime status (verified)

With `libchdb` installed (`CHDB_LIB_PATH=/path/libchdb.so`), the binary boots,
bootstraps the chDB schema, seeds defaults, and serves. Verified live:

- `GET /health/db` → 200 `{"db":"ok","latency_ms":1.22,...}` (embedded chDB query)
- `GET /v1/apps` → 200 `[]`
- `GET /api/dashboards/list` → 200 returning the seeded dashboard
  (schema bootstrap → seed insert → JSONEachRow query → JSON serialize)
- `GET /api/chart-types` → 404 graceful error (catalog needs the `node`
  extract script, same as Python)

All 14+ HTML pages render (verified 200 with real content: summary, logs,
metrics, errors, traces, rum, web-traffic, settings/*, work-items, cve,
dashboards, ai).

### Template engine

Rendering uses **gonja v2** (natively Jinja2-compatible), not pongo2. The
shared `../templates` exercise the full Jinja2 surface, so `s00_core.go` adds a
thin compatibility layer on top of gonja:

- **block-set** `{% set x %}...{% endset %}` → rewritten to a macro + assign
  (gonja's `set` is expression-only)
- **global macro registration** — child templates that `{% extends %}` get the
  parent/macro-file macros loaded as globals (gonja doesn't run an extending
  child's top-level `{% macro %}`/`{% from import %}`)
- **`{% call %}` / `caller()`** control structure — implemented (incl. the
  `{% call(params) %}` form)
- **dict `.get(k, default)` / `.values()`** — gonja ships only `keys`/`items`
- registered globals/filters: `url_for`, `get_flashed_messages`, `config`,
  `signal_label`, `signal_description`, `source_label`, and the `mask` filter

Two small patches are vendored under `vendor/.../gonja/v2/` (re-apply if
`go mod vendor` is rerun): a **ternary expression node** so `(A if C else B)`
parses inside parens/lists/call-args, and an optional `count` arg on string
`.replace()`.

## Port notes

Behavioural divergences are tagged inline with `// PORT-NOTE`. Notable ones:

- **API-key fingerprints** (CI push keys, MCP keys): `x/crypto/blake2b` lacks
  personalization, so keyed BLAKE2b is used — fingerprints are **not**
  byte-compatible with a Python-written DB; existing keys must be regenerated.
- **Regex**: Go's RE2 has no catastrophic backtracking; the ReDoS guards are
  preserved for fidelity but are effectively no-ops.
- **Map iteration order**: a few spots that relied on Python dict insertion
  order are nondeterministic in Go (flagged where it affects output).
- **geoip**: geo enrichment helpers are stubbed where `geoip2fast` had no
  direct Go equivalent.
