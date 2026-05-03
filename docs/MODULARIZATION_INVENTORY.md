# Modularization Inventory

**Last updated:** 2026-05-03  
**Branch:** `issue-284-simplicity-restart`  
**Overall status:** Route extraction complete — all routes in blueprints

---

## Progress Summary

| Batch | Blueprints Created | app.py delta | Status |
|-------|--------------------|-------------|--------|
| Batch A (M1+M2) | `routes/apps.py`, `routes/settings.py` | −874 lines | ✅ Done |
| Batch B | `routes/ingest.py`, `routes/notifications.py`, `routes/tags.py`, `routes/masking.py` | −1 200 lines | ✅ Done |
| Batch C | `routes/errors.py`, `routes/logs.py`, `routes/traces.py` | −1 700 lines | ✅ Done |
| Batch D | `routes/metrics.py`, `routes/rum.py` | −2 100 lines | ✅ Done |
| Batch E | `routes/dashboards.py`, `routes/reports.py`, `routes/query.py` | −2 094 lines | ✅ Done |
| Batch F | `routes/ai.py`, `routes/onboarding.py`, `routes/k8s_dm.py` | −2 697 lines | ✅ Done |
| Batch G | `routes/misc.py` | −1 027 lines | ✅ Done |
| Stabilisation | Fix 75+ endpoint namespace regressions in templates + Python files | — | ✅ Done |

**app.py total: 33,957 → 21,640 lines (−12,317 lines, −36%)**

---

## Current State

| Metric | Baseline | Current | Delta |
|--------|----------|---------|-------|
| `app.py` total lines | 33,957 | 21,640 | −12,317 |
| Route handlers in `app.py` | 188 | 0 | −188 |
| `routes/` blueprints | 0 | 18 | +18 |
| Routes in blueprints | 0 | 191 | +191 |
| Help-page routes (app-direct, registry loop) | 0 | 30 | +30 |
| Total registered routes | 188 | 221 | +33 (new features added) |

All quality gates pass:
- `isort` ✅
- `black` ✅
- `flake8` ✅
- `mypy` (20 source files, no issues) ✅
- `python3 scripts/check_dead_code.py` ✅

---

## Route Files (current)

| File | Blueprint | Routes | Lines | Domain |
|------|-----------|--------|-------|--------|
| `routes/apps.py` | `apps_bp` | 9 | 325 | `/v1/apps`, `/v1/releases` |
| `routes/settings.py` | `settings_bp` | 16 | 728 | `/settings/ai`, `/settings/enrichment`, `/settings/repositories`, `/settings/agents` |
| `routes/ingest.py` | `ingest_bp` | 13 | 704 | `/v1/logs`, `/v1/traces`, `/v1/metrics`, `/v1/rum`, `/v1/ai`, `/v1/errors`, etc. |
| `routes/notifications.py` | `notifications_bp` | 15 | 951 | `/settings/notifications`, `/api/notifications/*` |
| `routes/tags.py` | `tags_bp` | 8 | 460 | `/settings/tags`, `/api/tags/*` |
| `routes/masking.py` | `masking_bp` | 9 | 194 | `/settings/masking`, `/api/masking/*` |
| `routes/errors.py` | `errors_bp` | 2 | 433 | `/errors`, `/api/errors/*` |
| `routes/logs.py` | `logs_bp` | 4 | 618 | `/logs`, `/api/logs/*` |
| `routes/traces.py` | `traces_bp` | 3 | 564 | `/traces`, `/incident`, `/api/traces/*` |
| `routes/metrics.py` | `metrics_bp` | 8 | 858 | `/metrics`, `/metrics/anomaly`, `/metrics/rules`, `/api/metrics/*` |
| `routes/rum.py` | `rum_bp` | 15 | 1,160 | `/rum`, `/web-traffic`, `/cve`, `/api/rum/*`, `/api/web-traffic/*` |
| `routes/dashboards.py` | `dashboards_bp` | 22 | 1,031 | `/dashboards`, `/api/dashboards/*`, `/api/chart-spec/*` |
| `routes/reports.py` | `reports_bp` | 7 | 402 | `/reports`, `/api/reports/*` |
| `routes/query.py` | `query_bp` | 9 | 863 | `/query`, `/table-explorer`, `/api/query/*`, `/api/table-explorer/*` |
| `routes/ai.py` | `ai_bp` | 16 | 2,079 | `/ai`, `/api/ai/*`, `/settings/agents` trigger/dismiss |
| `routes/onboarding.py` | `onboarding_bp` | 6 | 599 | `/onboarding`, `/setup-wizard`, `/api/onboarding/*`, RUM asset serving |
| `routes/k8s_dm.py` | `k8s_dm_bp` | 10 | 257 | `/kubernetes`, `/api/kubernetes/*`, `/settings/data-management`, `/api/dm/*` |
| `routes/misc.py` | `misc_bp` | 11 | 1,128 | `/`, `/summary`, `/incident`, `/work-items`, `/health`, `/tail`, `/settings`, `/api/work-items/*` |
| `mcp.py` | `mcp_bp` | 8 | (existing) | MCP protocol (pre-existing extraction) |

**App-direct routes (30):** help pages registered via `_HELP_ROUTE_REGISTRY` loop in `app.py`.

---

## What Remains in `app.py`

`app.py` now contains:
1. **Application setup** – Flask/Quart app creation, config loading, middleware, CORS, Jinja2 globals
2. **Database layer** – `get_db()`, chDB schema management, migration helpers
3. **Background tasks** – log-event loop, GitHub repo health, metrics anomaly scheduling, etc.
4. **Core helper library** – shared utilities used by 2+ blueprints (auth, serialization, query builders, tag matching, etc.)
5. **Help route registry** – the `_HELP_ROUTE_REGISTRY` loop that registers 30 static template-render routes
6. **Blueprint registration** – deferred imports and `app.register_blueprint()` calls at the very end

There are **no route handler functions** left in `app.py`.

---

## Wrappers Kept (with Named Callers)

| Helper | Named Callers | Reason to Keep in `app.py` |
|--------|--------------|----------------------------|
| `get_db` | all 18 blueprints | Core DB dependency – never move |
| `require_api_key` | ingest, apps, all API routes | Core auth – never move |
| `_find_app_by_id` | apps, settings, onboarding | Shared across 3+ blueprints |
| `_serialize_app_row` | apps, settings | Shared across 2 blueprints |
| `_app_slug` | apps, settings, onboarding | Shared across 3+ blueprints |
| `_load_tag_rules` | ingest, tags, misc | Shared across 3+ blueprints |
| `masked_jsonify` | most blueprints | Shared output wrapper |
| `_safe_json_loads`/`_safe_json_dumps` | all blueprints | Universal utility |

---

## Endpoint Namespace Status

All `url_for()` calls and `request.endpoint` comparisons across:
- 19 template files (`.html`) ✅
- `app.py` ✅
- All `routes/*.py` files ✅

…now use fully-qualified blueprint-namespaced endpoint names (e.g., `url_for('misc.view_incident')` not `url_for('view_incident')`).

Audit script: run `python3 -c "import app as a; ..."` with the inline audit in the PR description to verify.

---

## Remaining Work

Route extraction is complete. Remaining lower-priority items for follow-on PRs:

| Item | Effort | Priority |
|------|--------|----------|
| Move single-use helper functions into their respective blueprint files | Medium | Low |
| Add blueprint-level unit tests (routes test isolation) | Medium | Medium |
| Coverage consolidation (`pytest --cov` delta) | Low | Medium |
| Evaluate splitting `routes/ai.py` (2,079 lines) into `routes/ai_views.py` + `routes/ai_api.py` | Low | Low |
