# SOBS Python → Go: Functional Completion Target

> **Status as of this audit:** 76 → **54 `not implemented` stubs remaining**. `parity_check.py` = **GREEN 250 / RED 0 / MISSING_GOLDEN 0 / UNCOVERED 0 / EXCLUDED 0**. Branch `claude/jolly-wu-5fc6a3` / PR #304.

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

## 2. What "verified" can and cannot mean

Three tiers, because not every success path is deterministically capturable offline:

- **Tier A — deterministic, byte-verifiable** (the bar for "done"): DB create/update/serialize/delete, spec compile/validate/render, ingest inserts, candidate generation, config-gated branches. These MUST be implemented AND parity-GREEN on the success path.
- **Tier B — external network (GitHub / OSV / web-push endpoints)**: Python itself cannot be frozen into a deterministic golden for a live `api.github.com` call. These MUST be implemented faithfully (port the HTTP logic) and **documented as network-dependent / not-offline-capturable** — verified by code review + a mocked/integration check, not the golden corpus.
- **Tier C — real LLM calls (`ai.endpoint_url`/`ai.model`)**: `ai/helper`, `ai/helper/execute`, `dashboards/spec/ai-build`. Same as Tier B — implement faithfully, document as model-dependent. The deterministic *guard/validation* branches are already GREEN.

A Tier-B/C route is "done" when its real logic is ported (not a 501) even though its golden is the error/guard branch; this must be **explicitly noted**, never silently left as a stub.

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

### G6. AI / LLM routes — *Tier C* (≈6 stub-lines)
Port faithfully; success path is model-dependent (document, don't byte-capture).
- `handlers_mutations2.go` ai/helper (46), ai/helper/execute (55), ai/helper/feedback (65), dashboards/spec/ai-build (74), the 503-when-unconfigured branch (217), `:238` adjacent.
- **NOT AI** — `handlers_mutations2.go:86` notifications/subscribe is a **Tier A** deterministic insert (registers a browser_push channel, dedup-by-endpoint, `@require_basic_auth` only) — do it in the G1/G3 batch, not here. (Confirmed: `spec/compile|validate|render|dry-run` are also `@require_basic_auth`-only deterministic transforms, NOT query/AI-gated.)

### G7. External network (GitHub / OSV / onboarding) — *Tier B* (≈8 stub-lines)
Port the HTTP logic; not offline-capturable.
- `handlers_mutations2.go` onboarding create-issues (96), create-repo (106), import-repo (116), list-repos (126); live GitHub backfill (309); real OSV scan (317).
- `handlers_get2.go:247` onboarding/inspect-repo.
- (github-repo-health's *counting* is already done; only the token-gated live issue scan remains — same Tier B.)

### G8. MCP authenticated paths — *Tier A (needs mcp keys)* (≈3 stub-lines)
- `handlers_misc.go:502,514` mcp JSON-RPC authenticated call + key auth (fixture mcp.api_keys empty → -32002 tested). Needs a configured mcp key to exercise; `handlers_misc.go:533` notes the per-key hash comparison is itself unfinished.
- `handlers_mutations3.go:43` mcp key descriptor lookup by id.

---

## 4. Non-stub unfinished work (no 501, but incomplete)

- **OTLP CORS** (`server.go:281`, `_ = strings.HasPrefix` placeholder): `app.py _path_needs_otlp_cors` adds CORS headers to `/v1/*`; Go does not. Parity-invisible (normalize drops/ the tested v1 routes don't trigger it) but **real OTLP browser clients need it**. Implement.
- **JS source-map demangling** (`handlers_v1_ingest.go:189`): RUM stack-trace demangling deferred even within the ingest path.
- **MCP per-key hash comparison** (`handlers_misc.go:533`): key validation returns `len(list)>0` instead of a real per-key hash check.
- **Stale comment** (`server.go:224` "TODO Phase 1+: register real handlers"): handlers ARE registered now — delete the comment.

---

## 5. Execution order (recommended)

1. **G1 action handlers** (Tier A, no external deps) — biggest stub-count win; establish seeded-record + create-then-act + ordering patterns.
2. **G3 dashboards spec** + **G2 config-gated** (Tier A, deterministic) — pure transforms / flip-a-flag.
3. **G5 ingest** (Tier A, manifest-last).
4. **G4 candidate generation** (Tier A, hard — statistical ports).
5. **G7 external + G6 AI + G8 mcp** (Tier B/C — implement faithfully, document the non-offline-capturable ones; do NOT leave as 501).
6. Clean up §4 non-stub items; delete stale TODOs; final `grep` must show **zero** stubs.

Per-stub loop, gotchas, harness capabilities, and chdb-determinism traps: see the `go-migration-route-recipe` memory. Workflow per route: read `app.py` handler → port to Go → add success-path manifest entry (`json:`/`form:` body or seeded/created id) → `seed_fixtures.py` → `capture_routes.py --only <ids>` → **re-seed** → `parity_check.py` (kill zombies first) → commit on GREEN.

---

## 6. Acceptance checklist

- [ ] `grep -rc 'not implemented", http.StatusNotImplemented' go/cmd/sobs | grep -v ':0'` → **empty** (0 stubs).
- [ ] `parity_check.py` exits 0: GREEN = full surface, RED/MISSING/UNCOVERED/EXCLUDED all 0.
- [ ] Every Tier-A route has a manifest entry that drives its **success** path (not just the error branch).
- [ ] Every Tier-B/C route's real logic is ported (not a 501) and its network/model dependency is documented (here + in code).
- [ ] `EXCLUSIONS.yaml` stays empty; every `mask` has a `mask_reason` limited to random/wall-clock/storage bytes.
- [ ] §4 non-stub gaps (OTLP CORS, source-map, mcp hash) closed or explicitly tracked.
- [ ] `go build ./...` clean; pre-commit (flake8/mypy/black) passes.
