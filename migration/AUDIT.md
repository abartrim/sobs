# SOBS Codebase Audit (migration baseline)

Consolidated from a full read of `app.py` (33,957 lines), `mcp.py`, `masking.py`,
`telemetry/`, all 75 templates, `static/`, and the build pipeline. This is the
ground truth the Go port reproduces. Line numbers are against `app.py` at the audited
revision — re-verify with `extract_routes.py` if the source moves.

## 1. Stack

| Layer | Python | Go target |
|-------|--------|-----------|
| Web framework | Quart (async Flask) `>=0.20` | stdlib `net/http` |
| ASGI server | Hypercorn `>=0.18` | stdlib `net/http` server |
| Templates | Jinja2 (Quart default env) | stdlib `text/template` (NOT `html/template` — see §6) |
| Embedded DB | `chdb` `>=4.1.9` (embedded ClickHouse) | `github.com/chdb-io/chdb-go` (cgo + libchdb/chdb-core) |
| OTLP parsing | `opentelemetry-proto` `>=1.42` | `google.golang.org/protobuf` + generated OTEL `.pb.go` |
| Crypto | `cryptography` (Fernet, ECDSA P-256) | stdlib `crypto/*` (+ a Fernet helper, ~80 LOC) |
| HTTP client | `httpx` | stdlib `net/http` |
| DataFrames | `pandas` | **drop** — only used by optional Vanna NL→SQL path |
| GeoIP | `geoip2fast` | parse its bundled DB, or vendor the same data file |
| Sourcemaps | `sourcemap` | small pure-Go parser (~200 LOC); feature-flagged off by default |
| Log redaction | `loggingredactor` + `masking.py` | port `masking.py` (310 LOC) |

## 2. HTTP surface — 188 routes

All routes are registered with `@app.route(...)` (no blueprints). Breakdown by what
they return:

| Returns | Count | Parity concern |
|---------|------:|----------------|
| HTML via `render_template` | ~74 | Jinja→Go template bytes (the hard part) |
| JSON via `jsonify` / `json.dumps` | ~109 | Encoder byte-parity: key order, separators, trailing newline, escaping |
| Redirect | 5 | `Location` header + status |
| CORS `OPTIONS` preflight | 4 | header set/order |

HTTP methods used: GET (85), POST (95), DELETE (3), PATCH (1), OPTIONS (4). No HEAD/PUT.

### Distinct rendered templates: 25
5 are rendered with **string-literal** names: `summary.html`, `query.html`,
`table_explorer.html`, `custom_dashboards.html`, `reports.html`.
**64 routes compute the template name at runtime** (a `template` variable) — the
selection logic must be ported exactly. The remaining ~20 distinct templates are
chosen by that logic (settings_*, metrics_rules, work_items, cve, kubernetes,
incident, metrics_anomaly, custom_dashboard_view, _ai_conversation_partial, …).

## 3. Global request/response machinery (must port before any route)

| Hook | Location | Behavior |
|------|----------|----------|
| `@app.after_request _apply_security_headers` | `app.py:458` | `setdefault`s 5 security headers on **every** response (`X-Content-Type-Options: nosniff`, `X-Frame-Options: SAMEORIGIN`, `Referrer-Policy: strict-origin-when-cross-origin`, `Permissions-Policy: camera=(), microphone=(), geolocation=()`, `Content-Security-Policy: frame-ancestors 'self'; object-src 'none'; base-uri 'self'`), plus HSTS when the request is a secure context, plus OTLP CORS headers on `/v1/*` paths. **Order and exact strings matter for parity.** |
| `@app.before_request _refresh_masking_rules_before_request` | `app.py:25659` | Refreshes DLP masking rules from DB before each request; skipped for `endpoint == "static"`. |
| `@app.context_processor inject_feature_flags` | `app.py:3384` | Injects into every template render: `query_enabled`, `kubernetes_enabled`, `raise_issue_mask_toggle_effective`, `mobile_breakpoint_max` (`"575.98px"`), `sobs_version` (`BUILD_VERSION or "dev"`). |

No custom error handlers (default 404/500). Sessions: Quart secure-cookie, signed with
`SECRET_KEY` (default `"sobs-dev-secret-key"`), cookie `sobs_session`, `SameSite=Lax`,
`HttpOnly`, `Secure` behind TLS. Sessions are set rarely (CI/CD key flows) — most page
renders emit **no** `Set-Cookie`, so cookie parity is a narrow edge (see normalize.py).

## 4. Jinja environment

- **No `trim_blocks` / `lstrip_blocks`** are set → Quart/Jinja default whitespace
  behavior. Whitespace control in templates is sparse (17×`{%-`, 13×`-%}`, no `{{-`/`-}}`).
- Custom globals (`app.py:13415`): `signal_label`, `signal_description`, `source_label`
  (functions callable from templates).
- Custom filter (`app.py:13421`): `mask` → `_mask_value_for_output` (DLP; **compliance
  critical** — port exactly from `masking.py`).
- Default autoescape on (`.html`). `|safe` used 10× (controlled server-built HTML).

## 5. Template structure

- **Single-level inheritance.** `base.html` (the only base) defines 4 blocks:
  `title`, `styles`, `content`, `scripts`. 59 templates `{% extends "base.html" %}`.
- **11 partials** (`_`-prefixed) — included or imported, never extended.
- **8 macros** (in `_filter_macros.html`, `_error_panels.html`,
  `_multi_select_filter.html`, `_page_header_macros.html`). Several are complex:
  `render_filter_accordion` and `render_error_accordion` use `{% call %}`/`caller()`
  and 9–12 params with defaults. `render_page_header` is imported by ~45 templates.
- **Filters used (ranked):** `tojson` (142×), `length` (26×), `truncate` (12×),
  `safe` (10×), `mask` (10× custom), `title` (8×), `min`/`max` (6× each), `lower` (6×),
  `round`/`replace`/`format` (4× each), `join` (3×), `urlencode` (1×).
- **`tojson` inside `<script>` (26 templates, ~150 call sites)** is the single biggest
  parity hazard. Server data is serialized into the page for the JS to consume
  (`const scope = {{ x | tojson }};`). Go `html/template` mangles this; the spec in
  `JINJA_TO_GO_SPEC.md` mandates `text/template` + a hand-ported `tojson`.

## 6. Why `text/template`, not `html/template`

Go's `html/template` applies **context-aware** auto-escaping that is fundamentally
different from Jinja2's. It will rewrite bytes inside `<script>`, `<style>`, URL
attributes, and HTML text in ways that *cannot* be made byte-identical to Jinja2.
Jinja2 does plain HTML-entity autoescaping in HTML text and **nothing context-aware**
in `<script>`; `tojson` does its own `<`,`>`,`&`,`'` → `\uXXXX` escaping. The only way
to get byte-parity is to use `text/template` (no auto-escaping) and **port Jinja's
escaping rules as explicit filters** (`e`/autoescape, `tojson`, `safe`, `mask`, …),
applied exactly where Jinja applies them. See `JINJA_TO_GO_SPEC.md` for the full rule
set. This is the crux of the whole migration.

## 7. Data layer

- **Single persistent `chdb` connection** (`ChDbConnection`, `app.py:1876`) guarded by
  `threading.Lock()`. One global instance for process lifetime (`get_db()`,
  `app.py:2112`). Schema applied on first open.
- Query API: `db.execute(sql, params)` → `ChDbResult` (columns + materialized rows,
  dict/index access). `?` placeholders for params. ~150+ call sites. `executescript`
  for DDL. Heavy ClickHouse-specific SQL: `FINAL`, `ReplacingMergeTree`,
  `JSONExtract`, `arrayMap/arrayFilter`, `toUnixTimestamp64Milli`, `uniq`,
  window functions. **SQL is reused verbatim in Go** — chdb-go runs the same engine.
- Writes: async **write queue + batch worker** (`_queue_write`, `app.py:7462`) and
  bulk `INSERT … FORMAT JSONEachRow` (`_insert_rows_json_each_row`, `app.py:8759`).
  `_WRITABLE_TABLES` allow-list (`app.py:8760`).
- On-disk state: `SOBS_DATA_DIR` (default `./data`) → `sobs.chdb/` (the database) +
  `rum_assets/` (uploaded sourcemaps/artifacts). Settings live in `sobs_app_settings`
  (ReplacingMergeTree), secrets Fernet-encrypted with prefix `✦ENCRYPTED✦`.
- Encryption: Fernet key = `base64url(sha256(SOBS_SETTINGS_ENCRYPTION_SECRET))`.
  VAPID push: ECDSA P-256 keypair in settings. Both reproducible with Go stdlib.
- OTLP: protobuf (and JSON) ingest at `/v1/logs|traces|metrics`; gzip/deflate decode;
  proto `AnyValue`/kvlist → nested dict → JSONEachRow insert.

## 8. Static assets & build

- `static/` is **all committed**; nothing is generated at request time. The Go server
  serves these byte-for-byte. Most served via Quart's default `/static/<file>`; RUM
  JS files have **explicit routes with content-hash `ETag` headers and `X-SourceMap`/
  `SourceMap` headers** (`/static/rum.js`, `rum.js.map`, `rum.min.js`, `rum.min.js.map`,
  `rum.d.ts`) — port these explicitly.
- RUM build: `static/rum.js` (hand-written source) → `rum.min.js` + maps via **terser**
  (`scripts/build-rum.js`), type-checked by `tsc`. Build output is committed. **The Go
  migration does not need to run terser** — treat the committed artifacts as frozen
  inputs and serve them as-is. (Keep `package.json`/terser around as the source of
  truth for when `rum.js` changes; out of scope for parity.)
- `echarts-chart-types.json` and `echarts_chart_types.py` are generated by
  `scripts/extract-echarts-types.js` (Node) and committed. Frozen inputs; serve as-is.

## 9. Determinism hazards (must be frozen for stable goldens — see PARITY_STRATEGY.md)

| Source | Count | Freeze strategy |
|--------|------:|-----------------|
| `datetime.now/utcnow`, `time.time` | ~103 sites | monkeypatch to fixed epoch in capture; inject a `Clock` in Go |
| `uuid.uuid4` | ~46 sites | deterministic counter-seeded UUIDs |
| `secrets.token_*`, `os.urandom` | ~4 sites | deterministic byte stream |
| dict iteration / JSON key order | ~55 sites | Python 3.7+ preserves insertion order; Go must emit the **same** order — see encoder spec |
| chdb query result order | many | fixtures use `ORDER BY`-stable data; never rely on engine default order |

## 10. Out-of-scope for *parity* (defer to post-green phases)

- Telemetry/OpenTelemetry self-instrumentation spans (`telemetry/`) — not visible in
  HTTP output; port for ops parity in Phase 6, not gated by goldens.
- Vanna NL→SQL (pandas) — optional feature; can be stubbed/feature-flagged off during
  parity and ported later without pandas.
- Background workers (CVE scan, notifications, agent runs) — not request/response
  output; covered by their own integration goldens in Phase 6, not the HTTP corpus.
