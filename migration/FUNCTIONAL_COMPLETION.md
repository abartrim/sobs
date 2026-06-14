# SOBS Python → Go: Functional Completion Target

> **Status:** 76 → **43 `not implemented` stubs remaining**. `parity_check.py` = **GREEN 268 / RED 0 / MISSING_GOLDEN 0 / UNCOVERED 0 / EXCLUDED 0**. Branch `claude/jolly-wu-5fc6a3` / PR #304.
> **G1 done:** agent/tag/metrics rule delete, create_metrics_rule, notifications/subscribe. **G3 done:** chart-spec compile foundation (`chart_spec.go`/`chart_builder_sql.go`), spec/compile, import/add/remove chart, delete_dashboard, add-to-dashboard, export_chart, **dashboards/query, spec/dry-run, spec/validate** (query-exec foundation: `store.Result.Types` + `chQueryValue` faithful typed serializer in `query_exec.go`). **§4 done:** OTLP CORS (byte-verified), stale TODO. **Also fixed:** v1 405 Allow ordering, Jinja `>`/`<` comparisons, agent/tag/notif readers that hardcoded empty lists.
> **Remaining G3:** spec/render + dashboards/render need the ~600-line echarts BINDING pipeline (`_extract_bindings`/`_deep_substitute`/`_attach_drilldown_metadata`/`_prepare_template_rows`/`_render_custom_echarts`/`_apply_chart_spec_visual_overrides`) + the per-template `echarts_option_template` data (NOT embedded in Go). On the fixture every builder query is empty → render returns the fixed "No data" placeholder; do NOT fake-complete behind that — implement the binding so non-empty renders are real. `renderWouldError`+`chartTemplateMeta` (chart_render.go) already encode the render RAISE conditions (reused by validate).

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

## 2b. Mock-upstream harness (build once, unblocks G6 + G7 + the config-gated half of G2)

1. **`migration/tools/mock_upstream.py`** — a tiny deterministic HTTP server on a pinned port. Routes by path (+ optional request-hash) → canned JSON: `/chat/completions` (+ guard/dlp variants), GitHub repos/issues/contents/actions/rate_limit, OSV `/v1/query`. Fixed responses (request-byte differences between Python and Go don't matter; the response is what's compared).
2. **`determinism.py`** — install an httpx mock transport (or `respx`) redirecting `api.github.com`/`api.osv.dev` to the mock, so the frozen Python app reaches it without touching `app.py`.
3. **Go** — `loadAISetting`/`loadAllAISettings` honor `SOBS_AI_*` env overrides (mirrors `_AI_ENV_OVERRIDES`); the github/osv client reads `SOBS_GITHUB_API_BASE`/`SOBS_OSV_API_BASE` (default real, mock in parity).
4. **Dual-profile capture** — turning AI config on flips `query_enabled` **globally** (ripples into every page's `baseContext`), so the AI-on/mock env is a **separate capture profile** over ONLY the AI + query-gated routes (a second manifest tag/file + AI-on env + mock running). The default corpus stays AI-off so the 404-guard branches remain tested. `capture_routes`/`parity_check` start the mock and run the AI-on profile as a second pass.

This is a self-contained harness phase; build it before G6/G7 and the G2 enabled branches.

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
- [ ] `migration/tools/mock_upstream.py` exists and is deterministic; `determinism.py` redirects github/osv to it; Go honors `SOBS_AI_*` / `SOBS_GITHUB_API_BASE` / `SOBS_OSV_API_BASE`.
- [ ] `EXCLUSIONS.yaml` stays empty; every `mask` has a `mask_reason` limited to random/wall-clock/storage bytes.
- [ ] §4 non-stub gaps (OTLP CORS, source-map, mcp hash) closed or explicitly tracked.
- [ ] `go build ./...` clean; pre-commit (flake8/mypy/black) passes.
