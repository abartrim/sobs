# SOBS Python → Go: Functional Completion Target

> **Status:** 76 → **20 `not implemented` stubs remaining**. `parity_check.py` = **GREEN 318 / RED 0 / MISSING_GOLDEN 0 / UNCOVERED 0 / EXCLUDED 0**. Added since GREEN 317: api_query_run no-chart path (queryrun profile) — proves the AI-cluster pattern (deterministic md5 `trace_id` from frozen `time_ns`, pandas-dtype `field_types`, zero `llm_stats`, 3 `emitAiHelperLogEvent` calls, all byte-exact). parity_check now retries the per-profile chdb seed (many-profile runs contend on the embedded server). **Remaining 20, all large/multi-part:** LLM routes still needing the `/chat/completions` mock + orchestration (ai_helper guard+main+memory, ai-build/query_ask/query_refine vanna pipelines, issues_raise); ai_helper_execute (HMAC-token construct via `SECRET_KEY=parity-fixed-secret-key` + action-manifest meta + sanitizer); trigger_agent_run; create_issues (200+ lines); cve_scan github+OSV; notifications check/auto (rule-eval); candidate-gen; OTLP ingest (3 schemas); repositories-sub actions; k8s status; dm backup; authenticated mcp (scrypt). Added since GREEN 314: ai_helper_chat_detail (aichat gen_ai seed — full turn serialization), export_ai_training (gen_ai span seed — spaced JSONL), `_emit_ai_helper_log_event` + ai_helper_feedback (shared AI telemetry primitive). parity_check now has settle delays so the 9-profile multi-boot run is chdb-stable. **Remaining 21, all large:** G6 AI/LLM that still need the AI mock + orchestration — ai_helper, ai_helper_execute, dashboards/spec/ai-build, query_ask, query_run, query_refine, issues_raise, trigger_agent_run (the LLM call is `POST {endpoint}/chat/completions`; add that host to `determinism.intercept_hosts` + a canned body; `emitAiHelperLogEvent` is now ready for their telemetry); create_issues (200+ lines); cve_scan github+OSV; notifications check + auto-generate (rule-eval); candidate-gen tag/metrics auto; v1 OTLP ingest; repositories-sub found-app actions; k8s status; dm backup; authenticated mcp (scrypt). Added since GREEN 311: inspect_repo (githubtoken profile = github mock + seeded ai.github_token), create_repo (createrepo isolation profile), unreachable v1-ingest default→404, mcp_api_delete_key (mcpkey seed profile). **All remaining 24 are large per-route ports** (no more quick wins): G6 AI/LLM (ai_helper, ai_helper_execute, ai_helper_feedback, ai-build, query_ask/run/refine, issues_raise, trigger_agent_run, chat_detail) — need the AI endpoint host in `intercept_hosts` + `_emit_ai_helper_log_event` + each route's (vanna) orchestration; create_issues (200+ lines: issue search/create + copilot + CI-key rotation); cve_scan github-backfill + OSV; notifications check + auto-generate (rule-eval engine); candidate-gen tag/metrics auto; v1 OTLP ingest; repositories-sub found-app actions + github-token validate; k8s status; dm backup; authenticated mcp (scrypt); ai/export (seed gen_ai spans). Branch `claude/jolly-wu-5fc6a3` / PR #304. **G3 COMPLETE** (full dashboards/spec cluster, render binding + anomaly engine + output masking). **§2b harness BUILT** (file-backed variant — see §2b). **Cleared since GREEN 296:** query-page introspection (schema/tables/table-detail, ai profile), masking-preview value branch, rum_asset_download 404, onboarding list_repos + import_repo (github mock), dismiss_agent_run (agentrun seed round-trip), notification channel/rule toggle+delete + test (notif seed — the previously-reverted bundle, now decoupled).
> **G8 mcp** initialize/ping/notifications (unauthenticated); cve-disposition upsert; OTLP CORS; v1 `Allow` sort; Jinja `>`/`<`; agent/tag/notif real readers.
> **Remaining 28 (all need the built infra + a per-route port):** G6 AI-call routes (~9: ai/helper{,/execute,/feedback}, spec/ai-build, query/ask, query/run, query/refine, issues/raise, chat-detail — add the AI endpoint host to the shim's `intercept_hosts`, port `_emit_ai_helper_log_event` + the vanna orchestration); G7 external (inspect_repo, create_repo, create_issues, cve_scan github/osv — github mock + a `githubtoken` profile seeding `ai.github_token`); G5 OTLP ingest; G4 candidate-gen (tag/metrics auto in-window); notifications check/auto-generate (rule-eval engine); G8 authenticated mcp (scrypt); k8s status; dm backup; rum asset upload (signing key); ai/export (seed gen_ai spans).

---

## 1. The Goal (definition of done)

Produce a **genuinely complete, functional Go replacement** for the Python (Quart) SOBS app. Every route must actually **perform its real behavior** — create / update / serialize / ingest / scan / compile / generate / render — for real inputs.

**Byte-for-byte parity against the frozen Python oracle is the *verification means*, not the goal.** It is the test that removes human judgment from "is this a faithful port?". A route that is GREEN only because the test exercises an empty-input error branch while its real logic sits behind `http.Error(w,"not implemented",501)` is **NOT done**.

**DONE =**
1. **Zero `not implemented` stubs** in `go/cmd/sobs/` (`grep -rc 'not implemented", http.StatusNotImplemented' go/cmd/sobs | grep -v ':0'` → empty), **and**
2. `parity_check.py` GREEN with each route's **success path** genuinely exercised (valid bodies / existing-or-created records / non-empty batches / enabled config), **and**
3. No masked region hides real logic — masks neutralize ONLY inherently-random or wall-clock or storage-internal bytes (uuid, CSPRNG keys, `system.parts` sizes), documented in `mask_reason`.

**Constraints:** hard cutover OK; minimal Go deps (stdlib + chdb-go + protobuf); **`app.py` / `mcp.py` and the golden corpus are READ-ONLY**; migration tooling + Go code are editable.

---

## 2. What "verified" means — everything is byte-verifiable via a mock upstream

The original worry was that LLM and external-network success paths can't be frozen into a deterministic golden. **A deterministic mock upstream removes that worry:** point BOTH the Python oracle (capture) and the Go port (replay) at the same canned responder, and the upstream *response* is pinned — so the only thing under test is each side's request-building + response-handling, which is exactly the logic to verify. This collapses the former Tiers B/C into the deterministic, byte-verifiable Tier A.

**The bar for "done" is byte-GREEN on the success path for EVERY route** (no "implement-but-trust-me" routes), achieved as follows:

- **Pure-deterministic** (DB CRUD/serialize, spec compile/validate/render, ingest, candidate generation, config-gated branches): seeded/created records or valid bodies + (for config-gated) the AI-on profile. Done = parity GREEN on the success path.
- **LLM routes** (`ai/helper`, `ai/helper/execute`, `ai/helper/feedback`, `dashboards/spec/ai-build`, guard/dlp): the call is `POST {ai.endpoint_url}/chat/completions` (OpenAI-compatible; reads `choices[0].message.content`). Set `SOBS_AI_ENDPOINT_URL`/`SOBS_AI_MODEL` (and guard/dlp) to the **mock upstream** — both Python (`_load_all_ai_settings` honors `_AI_ENV_OVERRIDES`) and Go (make `loadAISetting`/`loadAllAISettings` honor the same env). The mock returns a fixed OpenAI-shaped body; both sides post-process it identically → GREEN.
- **External network** (GitHub `api.github.com/*`, OSV `api.osv.dev/v1/query`): URLs are hardcoded in read-only `app.py`. Python side — add an **httpx mock transport in `determinism.py`** (editable) that maps those hosts → canned JSON. Go side — the Go HTTP client (ours) reads `SOBS_GITHUB_API_BASE`/`SOBS_OSV_API_BASE`, set to the mock. Same canned responses both sides → GREEN.

Only genuinely irreproducible bytes (uuid, CSPRNG key material, wall-clock, `system.parts` storage sizes) stay masked — never real logic, never a whole route.

## 2b. Mock-upstream + dual-profile + per-profile-seed harness — **BUILT** (file-backed variant)

Implemented equivalently to the original design but **file-backed (no running server)**, which is simpler and needs no port coordination. Three reusable mechanisms (full details in the `go-migration-route-recipe` memory):

1. **File-backed mock upstream** — both sides read the SAME canned files keyed by `sha256("METHOD url")[:32]`. Python: `determinism._install_upstream_fixtures()` patches `httpx.AsyncClient.__init__` to inject an `httpx.MockTransport` intercepting `intercept_hosts` (api.github.com, api.osv.dev, hooks.example.com) → `<key>.json`. Go: `upstream.go upstreamGet(method,url)` reads the same files. Activated by `SOBS_UPSTREAM_FIXTURES`. Covers GET and POST. **To extend to G6 LLM routes: add the AI endpoint host to `intercept_hosts` + author `/chat/completions` canned bodies.**
2. **Dual-profile capture/replay** — `migration/tools/profiles.py` `PROFILES` (env overlays) + a `profile:` manifest tag; `capture_routes --profile <p>` and `parity_check` boot a separate Go server per profile. The `ai` profile flips `query_enabled` via env (`SOBS_AI_ENDPOINT_URL`/`SOBS_AI_MODEL`/`SOBS_QUERY_PAGE_ENABLED`) — pure overlay, no mock needed for the gate-only introspection routes. The `github` profile sets `SOBS_UPSTREAM_FIXTURES`.
3. **Per-profile DB seed** — `seed_fixtures.py --only-profile <p>` inserts a profile's rows into ITS fixture only (never base), so found/mutate branches run without rippling base readers. `parity_check._seed_profile` applies it to `_run`. Used by `agentrun` (dismiss round-trip) and `notif` (channel/rule toggle/delete/test). This is how the notification bundle was decoupled.

Go `loadAISetting` already honors `SOBS_AI_*` (`aiEnvOverrides`). The original `SOBS_GITHUB_API_BASE`/`SOBS_OSV_API_BASE` knobs are unnecessary — the file-backed lookup keys on the (unchanged, hardcoded) request URL, so Go and Python hit the same key without any base rewrite.

---

## 3. Remaining work — the 54 stubs, grouped

### G1. Action handlers needing existing records — *Tier A* (≈12 stub-lines)
Shared 501 lines, each gating several routes; the count drops only when **all** paths through the handler are done. Test via **create-then-act** or a seeded record; mind manifest ordering.
- `handlers_forms.go` formDeleteGuard (agent/tag/**metrics** rule delete), formLookupGuard (channel/rule toggle+delete), repositories-sub (app delete / realtime-mode / ci-key rotate+revoke / releases / github-token validate), dashboards-sub (delete / add-chart / chart-delete).
- `handlers_data.go:130` trigger_agent_run (needs existing rule_id).
- `handlers_mutations3.go` agent-run dismiss (107), channel test (122), dashboards-subtree chart lookups (150,161), mcp-key descriptor by id (43).
- `handlers_pages.go:382` (flash-without-insert branch).
- **Ordering caveat:** metrics-rule-delete must be manifest-AFTER the auto-routes that read `sobs_anomaly_rules`; channel/rule actions need a seeded or created channel/rule first.

### G2. Config-gated branches — *Tier A* (≈8 stub-lines)
The fixture leaves `query_enabled` (= `ai.endpoint_url` + `ai.model`) and `kubernetes_enabled` false, so the guard (404) is tested; the enabled branch is stubbed. Flip the setting in a manifest sub-fixture (or a dedicated capture env) and capture the enabled path.
- `handlers_mutations.go` query/run (58), refine-chart (67), add-to-dashboard (76), query-builder enabled (49,87).
- `handlers_json.go` table-explorer enabled (26), table-explorer/tables (35), kubernetes/status enabled (44).
- `handlers_pathparam.go:138` table-explorer/table/<id> enabled.
- `handlers_get2.go:108` setup-wizard non-default env/lang/deployment (pure static step templates — deterministic, just needs the other combos ported).

### G3. Dashboards spec compile/validate/render + query/render — *Tier A* (≈6 stub-lines)
`_compile_chart_spec` and friends are **deterministic transforms** (spec → echarts options); only `ai-build` is Tier C.
- `handlers_mutations2.go` spec/compile (177), spec/dry-run (185), spec/render (193), spec/validate (201), dashboards/query (145), dashboards/render (154). Implement the transforms; valid-spec success paths are byte-testable.

### G4. Candidate generation / auto-render — *Tier A, hard* (≈6 stub-lines)
Deterministic but require faithful ports of the statistical/threshold algorithms, then a 180–240 KB page render.
- `handlers_pages.go:39,140` metrics/rules/auto + dashboard/auto candidate generation.
- `handlers_mutations2.go:238,257,344` notifications/rules/auto-generate (candidates from metric rules), auto per-rule evaluation, auto-generate-with-rules.
- (settings/tags/auto candidate generation analogous.)

### G5. Telemetry ingest (non-empty) — *Tier A* (≈4 stub-lines)
Build the full otel row(s) from the body and `InsertJSONEachRow`; manifest-LAST (they mutate); rebuild fixture after capture.
- `handlers_v1_ingest.go:44` v1 ingest+insert (note `:189` JS source-map demangling is a deferred sub-feature even once ingest lands).
- `handlers_get2.go:79` chat messages with turns (needs seeded otel_logs turns), `:231` gen_ai export with spans (needs seeded gen_ai spans).
- `handlers_static.go:155` rum asset upload (needs the asset-upload signing key configured).

### G6. AI / LLM routes — *byte-verifiable via mock upstream §2b* (≈6 stub-lines)
Point `SOBS_AI_*` at the mock `/chat/completions`; capture under the AI-on profile → GREEN.
- `handlers_mutations2.go` ai/helper (46), ai/helper/execute (55), ai/helper/feedback (65), dashboards/spec/ai-build (74), the 503-when-unconfigured branch (217), `:238` adjacent.
- **NOT AI** — `handlers_mutations2.go:86` notifications/subscribe is a **Tier A** deterministic insert (registers a browser_push channel, dedup-by-endpoint, `@require_basic_auth` only) — do it in the G1/G3 batch, not here. (Confirmed: `spec/compile|validate|render|dry-run` are also `@require_basic_auth`-only deterministic transforms, NOT query/AI-gated.)

### G7. External network (GitHub / OSV / onboarding) — *byte-verifiable via mock upstream §2b* (≈8 stub-lines)
Python reaches the mock via the `determinism.py` httpx transport; Go via `SOBS_GITHUB_API_BASE`/`SOBS_OSV_API_BASE` → same canned JSON → GREEN.
- `handlers_mutations2.go` onboarding create-issues (96), create-repo (106), import-repo (116), list-repos (126); live GitHub backfill (309); real OSV scan (317).
- `handlers_get2.go:247` onboarding/inspect-repo.
- (github-repo-health's *counting* is already done; only the token-gated live issue scan remains — same Tier B.)

### G8. MCP authenticated paths — *Tier A (needs mcp keys)* (≈3 stub-lines)
- `handlers_misc.go:502,514` mcp JSON-RPC authenticated call + key auth (fixture mcp.api_keys empty → -32002 tested). Needs a configured mcp key to exercise; `handlers_misc.go:533` notes the per-key hash comparison is itself unfinished.
- `handlers_mutations3.go:43` mcp key descriptor lookup by id.

---

## 4. Non-stub unfinished work (no 501, but incomplete)

- ~~**OTLP CORS**~~ ✅ DONE — `applyOtlpCors` in server.go ports `_path_needs_otlp_cors`/`_origin_allowed_for_otlp`/`_otlp_cors_allow_methods`/`_append_vary_header`; byte-verified via `post__v1_ai_cors` (a `/v1/ai` POST with a localhost `Origin`).
- **JS source-map demangling** (`handlers_v1_ingest.go:189`): RUM stack-trace demangling deferred even within the ingest path.
- **MCP per-key hash comparison** (`handlers_misc.go:533`): key validation returns `len(list)>0` instead of a real per-key hash check.
- ~~**Stale comment**~~ ✅ DONE — the obsolete `server.go` "register real handlers" TODO is deleted.

---

## 5. Execution order (recommended)

1. **G1 action handlers** (deterministic, no external deps) — biggest stub-count win; establish seeded-record + create-then-act + ordering patterns.
2. **G3 dashboards spec** (deterministic transforms) + **G5 ingest** (manifest-last).
3. **§2b mock-upstream harness** (build once) — then **G2 config-gated** + **G6 AI** + **G7 external** all become byte-verifiable under the AI-on / mock profile.
4. **G8 mcp** (seed mcp keys) and **G4 candidate generation** (hard — statistical ports).
5. Clean up §4 non-stub items; delete stale TODOs; final `grep` must show **zero** stubs.

Per-stub loop, gotchas, harness capabilities, and chdb-determinism traps: see the `go-migration-route-recipe` memory. Workflow per route: read `app.py` handler → port to Go → add success-path manifest entry (`json:`/`form:` body or seeded/created id) → `seed_fixtures.py` → `capture_routes.py --only <ids>` → **re-seed** → `parity_check.py` (kill zombies first) → commit on GREEN.

---

## 6. Acceptance checklist

- [ ] `grep -rc 'not implemented", http.StatusNotImplemented' go/cmd/sobs | grep -v ':0'` → **empty** (0 stubs).
- [ ] `parity_check.py` exits 0: GREEN = full surface, RED/MISSING/UNCOVERED/EXCLUDED all 0.
- [ ] Every route has a manifest entry that drives its **success** path (not just the error branch) — including AI/external routes via the §2b mock upstream + AI-on profile.
- [x] Deterministic mock upstream both sides reach (file-backed, §2b): `determinism._install_upstream_fixtures` (httpx MockTransport) + Go `upstream.go`, keyed on `sha256("METHOD url")`; Go honors `SOBS_AI_*`. (The `mock_upstream.py` server + `SOBS_GITHUB_API_BASE` form was superseded by the file-backed variant — same guarantee, no server. The AI endpoint host still needs adding to `intercept_hosts` for G6.)
- [ ] `EXCLUSIONS.yaml` stays empty; every `mask` has a `mask_reason` limited to random/wall-clock/storage bytes.
- [ ] §4 non-stub gaps (OTLP CORS, source-map, mcp hash) closed or explicitly tracked.
- [ ] `go build ./...` clean; pre-commit (flake8/mypy/black) passes.
