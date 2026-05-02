# Modularization Baseline Inventory

Generated: 2026-05-02  
Branch: `issue-284-simplicity-restart`  
Milestone: 2 (Completed)

---

## Progress Summary

| Milestone | Status | app.py delta |
|-----------|--------|-------------|
| 0 – Baseline inventory | ✅ Done | — |
| 1 – `routes/apps.py` Blueprint (9 routes) | ✅ Done | −212 lines |
| 2 – `routes/settings.py` Blueprint (16 routes) | ✅ Done | −632 lines |
| 3 – Wrapper reduction sweep | ✅ Done | −30 lines |
| 4 – Coverage consolidation | ⬜ Todo | — |

**app.py total: 33,957 → 33,083 lines (−874)**

---

## Current State

| Metric | Baseline | After M1+M2+M3 | Delta |
|--------|----------|----------------|-------|
| `app.py` total lines | 33,957 | 33,083 | −874 |
| `app.py` sync functions | 501 | 501 | 0 |
| `app.py` async route handlers | 245 | 217 | −28 |
| `app.py` registered routes | 188 | 160 | −28 |
| `routes/` blueprints | 0 | 2 | +2 |

---

## Route Files (current)

| File | Blueprint | Routes | Lines |
|------|-----------|--------|-------|
| `routes/apps.py` | `apps_bp` | 9 | ~220 |
| `routes/settings.py` | `settings_bp` | 16 | ~632 |

---

## Route Domain Summary

| Domain | Routes | Lines in app.py | Status |
|--------|--------|-----------------|--------|
| Ingest (`/v1/logs`, `/v1/traces`, `/v1/metrics`, `/v1/rum`, `/v1/ai`, `/v1/errors`) | 10 | ~502 | future |
| Apps/Releases (`/v1/apps`, `/v1/releases`) | 9 | ~0 | ✅ `routes/apps.py` |
| Settings AI/enrichment/repos/agents | 16 | ~0 | ✅ `routes/settings.py` |
| Settings masking/tags/notifications/k8s/dm (`/settings/*`) | 22 | ~1422 | future |
| Errors (`/errors`) | 2 | ~385 | future |
| Logs (`/logs`) | 1 | ~274 | future |
| Traces/Incident (`/traces`, `/incident`) | 2 | ~727 | future |
| Metrics (`/metrics`) | 7 | ~713 | future |
| RUM (`/rum`) | 1 | ~349 | future |
| Dashboards (`/dashboards`, `/api/dashboards`) | 22 | ~846 | future |
| Other (summary, tail, query, AI helper, etc.) | 93 | ~5646 | future |

---

## Apps / Release / Artifact Slice (Milestone 1) ✅

Routes moved to `routes/apps.py`:

| Function | Method | Path |
|----------|--------|------|
| `list_apps` | GET | `/v1/apps` |
| `create_app_registry_entry` | POST | `/v1/apps` |
| `get_app_registry_entry` | GET | `/v1/apps/<app_id>` |
| `update_app_registry_entry` | PATCH | `/v1/apps/<app_id>` |
| `list_app_releases` | GET | `/v1/apps/<app_id>/releases` |
| `create_app_release` | POST | `/v1/apps/<app_id>/releases` |
| `get_release` | GET | `/v1/releases/<release_id>` |
| `list_release_artifacts` | GET | `/v1/releases/<release_id>/artifacts` |
| `create_release_artifact_meta` | POST | `/v1/releases/<release_id>/artifacts/meta` |

Helpers remain in `app.py` (shared with settings and ingest routes).

---

## Settings Slice (Milestone 2) ✅

Routes moved to `routes/settings.py` (AI, enrichment, repositories, agents):

| Function | Path |
|----------|------|
| `view_ai_settings` | GET `/settings/ai` |
| `save_ai_settings` | POST `/settings/ai` |
| `view_enrichment_settings` | GET `/settings/enrichment` |
| `save_enrichment_settings` | POST `/settings/enrichment` |
| `view_settings_repositories` | GET `/settings/repositories` |
| `create_settings_repository` | POST `/settings/repositories` |
| `validate_settings_repository_github_token` | POST `/settings/repositories/github-token/validate` |
| `save_settings_repository_realtime_mode` | POST `/settings/repositories/<app_id>/realtime-mode` |
| `rotate_settings_repository_ci_ingest_key` | POST `/settings/repositories/<app_id>/ci-ingest-key/rotate` |
| `revoke_settings_repository_ci_ingest_key` | POST `/settings/repositories/<app_id>/ci-ingest-key/revoke` |
| `update_settings_repository` | POST `/settings/repositories/<app_id>` |
| `add_settings_repository_release` | POST `/settings/repositories/<app_id>/releases` |
| `delete_settings_repository` | POST `/settings/repositories/<app_id>/delete` |
| `view_agent_rules` | GET `/settings/agents` |
| `create_agent_rule` | POST `/settings/agents` |
| `delete_agent_rule` | POST `/settings/agents/<rule_id>/delete` |

Production call sites rewired:
- 24 `url_for` calls in `app.py` routes updated to use `settings.` prefix
- 38 `url_for` calls in 17 templates updated to use `settings.` prefix
- `base.html` endpoint comparison updated for nav active state

---

## Wrappers Currently in `app.py`

### Helpers with exactly 1 caller (single-use, Milestone 3 candidates)

There are 248 such helpers at baseline. Key examples:

| Helper | Single Caller | Plausible Deletion Path |
|--------|--------------|------------------------|
| `_acquire_dm_prune_lock` | `api_dm_prune` | Inline into caller (Milestone 3) |
| `_apply_dm_ttl` | `save_dm_settings` | Inline or move with settings slice |
| `_build_auto_tag_rule_candidates` | `auto_tag_rules` | Move with tags slice |
| `_build_auto_metric_rule_candidates` | `auto_metrics_rules` | Move with metrics slice |
| `_build_seasonal_metric_rule_candidates` | `auto_metrics_rules` | Move with metrics slice |

### Wrappers kept for now (with named reasons)

| Wrapper | Named Callers | Reason to Keep |
|---------|--------------|----------------|
| `_find_app_by_id` | apps routes, settings repos routes | Shared – cannot remove until all callers are in one module |
| `_serialize_app_row` | apps routes, settings repos routes | Shared – ditto |
| `_app_slug` | apps, settings, onboarding routes | Shared – ditto |
| `require_api_key` | all authenticated routes | Core auth decorator – never remove |
| `get_db` | all routes | Core dependency – never remove |

---

## Shared Modules Worth Keeping

| Module | Purpose | Coverage |
|--------|---------|----------|
| `masking.py` | Input masking logic | high |
| `mcp.py` | MCP protocol blueprint | high |
| `telemetry/` | Optional OTEL self-telemetry | high |

---

## Quality Gate Baseline

- `pytest tests/` – requires chDB memory; run in Docker for full baseline
- `isort`, `black`, `flake8`, `mypy` – enforced per batch; routes/ passes clean
- `python3 scripts/check_dead_code.py` – Vulture 100% confidence; passes clean

---

## Target Feature Slices (Prioritised)

1. ~~**Milestone 1**: `routes/apps.py` – 9 routes, ~225 lines removed from `app.py`~~ ✅
2. ~~**Milestone 2**: `routes/settings.py` – 16 routes (AI/enrichment/repos/agents), ~632 lines removed from `app.py`~~ ✅
3. **Milestone 3**: Wrapper sweep – target wrappers with zero remaining callers after Milestones 1–2
4. **Milestone 4**: Coverage consolidation – direct tests for extracted logic


---

## Current State

| Metric | Value |
|--------|-------|
| `app.py` total lines | 33,957 |
| `app.py` AST statements | 14,494 |
| Top-level functions | 501 (486 private, 15 public) |
| Total routes | 188 |
| Existing extracted modules | `masking.py`, `mcp.py`, `telemetry/` |

---

## Route Files

No `routes/` package exists yet. All 188 routes live in `app.py`.

---

## Route Domain Summary

| Domain | Routes | Lines in app.py | Target module |
|--------|--------|-----------------|---------------|
| Ingest (`/v1/logs`, `/v1/traces`, `/v1/metrics`, `/v1/rum`, `/v1/ai`, `/v1/errors`) | 10 | ~502 | `routes/ingest.py` (future) |
| Apps/Releases (`/v1/apps`, `/v1/releases`) | 9 | ~183 | `routes/apps.py` **← Milestone 1** |
| Settings (`/settings/*`) | 38 | ~1422 | `routes/settings.py` **← Milestone 2** |
| Errors (`/errors`) | 2 | ~385 | future |
| Logs (`/logs`) | 1 | ~274 | future |
| Traces/Incident (`/traces`, `/incident`) | 2 | ~727 | future |
| Metrics (`/metrics`) | 7 | ~713 | future |
| RUM (`/rum`) | 1 | ~349 | future |
| Dashboards (`/dashboards`, `/api/dashboards`) | 22 | ~846 | future |
| Other (summary, tail, query, AI helper, etc.) | 93 | ~5646 | future |

---

## Apps / Release / Artifact Slice (Milestone 1 Target)

Routes to move to `routes/apps.py`:

| Function | Method | Path | Lines |
|----------|--------|------|-------|
| `list_apps` | GET | `/v1/apps` | 10303–10310 |
| `create_app_registry_entry` | POST | `/v1/apps` | 10315–10347 |
| `get_app_registry_entry` | GET | `/v1/apps/<app_id>` | 10352–10357 |
| `update_app_registry_entry` | PATCH | `/v1/apps/<app_id>` | 10362–10399 |
| `list_app_releases` | GET | `/v1/apps/<app_id>/releases` | 10404–10416 |
| `create_app_release` | POST | `/v1/apps/<app_id>/releases` | 10421–10446 |
| `get_release` | GET | `/v1/releases/<release_id>` | 10451–10465 |
| `list_release_artifacts` | GET | `/v1/releases/<release_id>/artifacts` | 10470–10482 |
| `create_release_artifact_meta` | POST | `/v1/releases/<release_id>/artifacts/meta` | 10487–10517 |

Helpers used by this slice and their other callers:

| Helper | Defined at | Other callers |
|--------|-----------|---------------|
| `_app_slug` | 8853 | settings repos, onboarding (lines 26909, 33485, 33593) |
| `_find_app_by_id` | 8858 | settings repos (lines 26977, 26996, 27024, 27044, 27088, 27119) |
| `_find_release_by_id` | 8882 | ingest at line 7609 |
| `_serialize_app_row` | 8890 | settings repos (line 26840) |
| `_serialize_release_row` | 8905 | apps routes only |
| `_serialize_artifact_row` | 8918 | apps routes only |
| `_insert_rows_json_each_row` | 8759 | many callers across app.py |
| `_now_iso` | 7947 | many callers across app.py |
| `_safe_json_dumps` | 8813 | many callers across app.py |
| `_safe_json_loads` | 8838 | many callers across app.py |
| `_parse_bool` | 20384 | many callers across app.py |
| `get_db` | 2112 | all routes |
| `require_api_key` | 7615 | all API routes |
| `jsonify` | 93 | all routes |

**Conclusion:** All helpers remain in `app.py`; `routes/apps.py` imports them from `app` via deferred import (possible because blueprint is registered at the end of `app.py`).

**No duplicate logic to reduce in this slice.** The 9 routes are cohesive and use shared helpers already centralised. The measurable gain is: 9 fewer route registrations in `app.py`, ~225 fewer lines in `app.py`.

---

## Settings Slice (Milestone 2 Target)

Routes to move to `routes/settings.py` (AI, enrichment, repositories, agents):

| Function | Path | Lines |
|----------|------|-------|
| `view_ai_settings` | GET `/settings/ai` | 26646–26672 |
| `save_ai_settings` | POST `/settings/ai` | 26677–26751 |
| `view_enrichment_settings` | GET `/settings/enrichment` | 26759–26773 |
| `save_enrichment_settings` | POST `/settings/enrichment` | 26778–26797 |
| `view_settings_repositories` | GET `/settings/repositories` | 26805–26884 |
| `create_settings_repository` | POST `/settings/repositories` | 26889–26951 |
| `validate_settings_repository_github_token` | POST `/settings/repositories/github-token/validate` | 26956–26970 |
| `save_settings_repository_realtime_mode` | POST `/settings/repositories/<app_id>/realtime-mode` | 26975–26989 |
| `rotate_settings_repository_ci_ingest_key` | POST `/settings/repositories/<app_id>/ci-ingest-key/rotate` | 26994–27017 |
| `revoke_settings_repository_ci_ingest_key` | POST `/settings/repositories/<app_id>/ci-ingest-key/revoke` | 27022–27030 |
| `update_settings_repository` | POST `/settings/repositories/<app_id>` | 27035–27077 |
| `add_settings_repository_release` | POST `/settings/repositories/<app_id>/releases` | 27082–27112 |
| `delete_settings_repository` | POST `/settings/repositories/<app_id>/delete` | 27117–27166 |
| `view_agent_rules` | GET `/settings/agents` | 27174–27189 |
| `create_agent_rule` | POST `/settings/agents` | 27194–27242 |
| `delete_agent_rule` | POST `/settings/agents/<rule_id>/delete` | 27247–27272 |

Retained in `app.py` for later milestones:
- `/settings/masking/*` routes
- `/settings/tags/*` routes
- `/settings/notifications/*` routes
- `/settings/kubernetes` routes
- `/settings/data-management` routes

---

## Wrappers Currently in `app.py`

### Helpers with exactly 1 caller (single-use, Milestone 3 candidates)

There are 248 such helpers. Key examples:

| Helper | Single Caller | Plausible Deletion Path |
|--------|--------------|------------------------|
| `_acquire_dm_prune_lock` | `api_dm_prune` | Inline into caller (Milestone 3) |
| `_apply_dm_ttl` | `save_dm_settings` | Inline or move with settings slice |
| `_build_auto_tag_rule_candidates` | `auto_tag_rules` | Move with tags slice |
| `_build_auto_metric_rule_candidates` | `auto_metrics_rules` | Move with metrics slice |
| `_build_seasonal_metric_rule_candidates` | `auto_metrics_rules` | Move with metrics slice |

### Wrappers kept for now (with named reasons)

| Wrapper | Named Callers | Reason to Keep |
|---------|--------------|----------------|
| `_find_app_by_id` | apps routes, settings repos routes | Shared – cannot remove until all callers are in one module |
| `_serialize_app_row` | apps routes, settings repos routes | Shared – ditto |
| `_app_slug` | apps, settings, onboarding routes | Shared – ditto |
| `require_api_key` | all authenticated routes | Core auth decorator – never remove |
| `get_db` | all routes | Core dependency – never remove |

---

## Shared Modules Worth Keeping

| Module | Purpose | Coverage |
|--------|---------|----------|
| `masking.py` | Input masking logic | high |
| `mcp.py` | MCP protocol blueprint | high |
| `telemetry/` | Optional OTEL self-telemetry | high |

---

## Quality Gate Baseline

- `pytest tests/` – requires chDB memory; run in Docker for full baseline
- `isort`, `black`, `flake8`, `mypy` – enforced per batch
- `python3 scripts/check_dead_code.py` – Vulture 100% confidence

---

## Target Feature Slices (Prioritised)

1. **Milestone 1**: `routes/apps.py` – 9 routes, ~225 lines removed from `app.py`
2. **Milestone 2**: `routes/settings.py` – 16 routes (AI/enrichment/repos/agents), ~600 lines removed from `app.py`
3. **Milestone 3**: Wrapper sweep – target wrappers with zero remaining callers after Milestones 1–2
4. **Milestone 4**: Coverage consolidation – direct tests for extracted logic
