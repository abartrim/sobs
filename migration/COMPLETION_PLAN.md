# SOBS Go Migration — Completion Plan (the path to a defensible "100%")

**Status:** active · **Branch:** `go-main` · **Oracle:** `app.py` (frozen, READ-ONLY)

## Why this exists

Five function-parity audits each found new divergences. That is structural, not bad luck:
**byte-parity GREEN only proves Go == Python on the inputs the golden corpus contains**, and an
audit is a human/LLM *reading* the un-exercised remainder — i.e. sampling. Sampling never converges.
The way out is to **measure the un-exercised surface and drive it mechanically**, so "are we done?"
becomes a number, not another audit.

## Branch & CI model

- **`go-main` is the migration's integration branch ("main" until cutover).** All work — code,
  tests, fixtures, tooling — lands here (directly for tooling, via feature→`go-main` PRs for code).
- **`main` is untouched** until the final cutover. We do **not** put migration Python tests on `main`,
  so `main`'s CI never runs for this effort and there is zero artifact contention with it.
- **CI on `go-main`** runs the same build/test/parity it would on main, but **artifacts are namespaced
  and nothing is published/deployed**:
  - Triggers: `push`/`pull_request` on `go-main` for the build / vet / unit-test / **Docker parity**
    / **coverage** jobs.
  - Image build: tag `:go-main` + `:<sha>` only — **never `:latest`**.
  - **Gated OFF for `go-main`** (kept `main`/tag-only): the ghcr publish push, `register-sobs-release`,
    `cleanup-ghcr`, and any `environment:`/deploy job. (Implement via `if: github.ref == 'refs/heads/main' || startsWith(github.ref,'refs/tags/v')`.)
- **Required checks to merge into `go-main`:** `go build ./...` + `go vet` + `gofmt` + full Go tests +
  **Docker parity GREEN (RED/MISSING/UNCOVERED = 0)** + **oracle coverage did not regress**.

### Branch & CI setup — work items / status

- [x] `go-main` branch created and pushed to origin (base `a39ec6c` + plan/coverage-gate commits).
      Pushing it does **not** trigger CI yet — `ci.yml` only matches `main`/`master`/`v*` tags.
- [x] Oracle coverage gate (`migration/tools/coverage_capture.py`) added; baseline measured = **58%**.
- [x] `ci.yml`: added `go-main` to `push`/`pull_request` triggers (build / vet / gofmt / unit-test /
      Docker parity all run on go-main). Commit `89fd409`.
- [x] Publish/release gating: the `docker`, `register-sobs-release`, `cleanup-ghcr` jobs were
      **already** gated to `main`/`master`/`v*` tags via their own `if:` — verified they do NOT run on
      go-main, so go-main never publishes.
- [x] Image `:latest` is `enable={{is_default_branch}}` only, and the publish job doesn't run on
      go-main anyway — no `:latest` from go-main.
- [x] Added the `oracle-coverage` CI job (go-main context only): runs `coverage_capture.py` +
      `coverage_gate.py`, fails if coverage drops below `migration/COVERAGE_FLOOR` (start 57.5%,
      ratchet up).
- [x] Branch protection on `go-main`: requires `Go Build & Test` + `Go Parity (byte-diff vs Python
      oracle)` + `Oracle Coverage` checks; `enforce_admins=false` so the integration branch still
      accepts direct pushes; force-push/deletion disabled.

## Definition of Done (measurable — this is what "100%" means)

1. **Stubs:** `grep 'not implemented", http.StatusNotImplemented' go/cmd/sobs` → empty. ✅ (already 0)
2. **Parity:** Docker `run_parity_ci.py` GREEN, RED/MISSING/UNCOVERED = 0. ✅ (396/0 currently)
3. **Oracle coverage:** app.py capture-coverage ≥ **99%**, and **every** uncovered line is individually
   classified as dead/unreachable, intentionally-deferred (with reason), or scheduled (has a tracking
   item). Measured by `coverage_capture.py` (see Harness §1).
4. **Differential fuzz:** ≥ N cases zero-diff (app.py vs Go binary) on each risk surface
   {OTLP ingest, DLP masking, query/regex validation, JSON encoding, settings combinations}.
5. **Mutation:** mutation testing on app.py leaves no *unexplained* surviving mutant (each survivor is
   a known-equivalent or an accepted gap).
6. **CI artifacts** namespaced; `main` never touched by migration CI.

## The confidence harness (build in this order; measure before building machinery)

1. **Oracle coverage gate — `migration/tools/coverage_capture.py`.** Runs the full capture phase
   (every profile, seeded) under `coverage.py --source=app`; emits `migration/coverage_app.json` +
   a report of uncovered app.py lines. **This is the map**: the uncovered set is the entire remaining
   risk surface. Run it first; its number tells us how much machinery is justified.
2. **Corpus expansion.** For each reachable-but-uncovered cluster, add a seeded/feature-on profile +
   fixtures until coverage hits the DoD threshold. Each profile is a new byte-for-byte differential
   test that also pins Go. (This is the systematic version of the five audits.)
3. **Differential fuzzing.** Generate randomized inputs, run app.py (test client) and the Go binary on
   the *same* input, assert byte-equal. Targets the classes humans miss (encoding, regex engine,
   numeric/Unicode edges) — exactly the bugs the audits kept finding by hand.
4. **Mutation testing.** Perturb app.py behavior; a surviving mutant under the corpus = a proven hole
   in the test corpus. Verifies the verifier.

Cheap structural checks (run continuously, catch the recurring "stub-shaped" regressions):
- **Grep signatures** for hardcoded-empty handlers (`[]any{}` / `"...": 0` / `return nil` where the
  Python computes) and deferral comments ("follow-up", "needs the LLM mock", "wired separately").
- **Constant/table diff:** extract Python module-level allowlists / sensitive-key sets / status maps /
  thresholds and diff against the Go equivalents.
- **Function/route inventory matrix:** every Python `def`/route → Go counterpart; unmapped = suspect.

## Multi-agent process (how we do the work — guardrails from hard experience)

Orchestration is **hierarchical**, not a peer mesh. Roles (define in `.claude/agents/*.md`):
- **Coordinator** — reads the coverage map / fuzz failures, slices them into file-disjoint tasks,
  fans out coders, integrates at a barrier, re-runs the gate. (This is the only agent that "merges".)
- **Coder** — ports ONE task. **MUST run in an isolated git worktree** (`isolation: "worktree"`) —
  a shared tree caused the "files edited under me" chaos. File-disjoint ownership.
- **Parity-tester** — runs the deterministic harness (build/vet/test + Docker parity + coverage) on a
  change and reports GREEN/RED + diffs. Does **not** decide correctness by reading — it runs the gate.
- **Code-reviewer** — adversarial diff review against the Python oracle.

**Invariant: deterministic assurance lives in scripts/CI, never in agents.** Agents do the *labor*
(port, review); the harness (coverage + parity + fuzz + mutation) provides the *proof*. An agent may
*call* the parity gate; it is never trusted in place of it. The parity gate is the non-negotiable
barrier for every integration.

## Current status snapshot (2026-06-19)

**Harness COMPLETE and operational** (all of §"confidence harness" + §"structural checks" + the
agent roster + CI are built and committed on `go-main`):
- Coverage gate (`coverage_capture.py`) + ratchet (`coverage_gate.py` + `COVERAGE_FLOOR`) — baseline
  **57.81%** (8,604/14,884 covered; 6,280 uncovered).
- Backlog classifier (`classify_coverage.py` → `coverage_backlog.{json,md}`): route 123 fns/1,710
  lines · helper 448/4,301 · lifecycle 21/256 · module 13. The sized work-list for corpus expansion.
- Structural checks (`structural_checks.py`): route matrix 188/188 mapped · 0 allowlist gaps · 3
  benign stub markers. Wired as a fast CI gate.
- Differential fuzzer (`fuzz_diff.py`): found **F1** (validate-filter error path leaked raw chdb
  errors) → fixed in `query_exec.go`; validate_filter fuzz 0→64/80. F2/F3 logged (`FUZZ_FINDINGS.md`).
- Mutation harness (`mutation_test.py`): validated (summary/get__root 2/5 killed). High-value once
  coverage is high.
- CI on `go-main` (build/test/parity/structural/coverage) + branch protection; publish gated to main.
- **Full loop proven end-to-end:** fuzz → fix (F1) → `validateerr` profile byte-tests the error
  branch GREEN → permanent regression test + coverage gain. Docker parity GREEN 396/0.
- **Coverage drive started — BOTH expansion modes demonstrated GREEN:**
  - *Error-path mode:* `validateerr` (2) + `formerr` (15) = 17 routes byte-testing
    validation/error branches across validate-filter, metrics-rule, tag-rule, notification
    rule/channel, repos, onboarding, add-to-dashboard, cve-disposition, subscribe.
  - *Seeded populated-data mode:* `api_raw_span` found-branch (949B real span detail) via the
    existing `tracedetail` seed.
  - *Big-view populated mode:* `logsview` profile (seeded otel_logs + record tags) byte-tests the
    whole `view_logs` populated render (rows/snapshot/tags, every filter, raw-SQL incl. `has_tag()`,
    regex `q`); plus the `view_traces` LIST path (service/regex filters) reusing `tracedetail`.
  - Plus `view_rum` filter/mode branches (4 base routes: `?view=`, `?type=`, `?q=`,
    `?error_source=`) reusing the base hyperdx_sessions seed; its vitals/error-trend
    derived-signal lines (need the `v_derived_signals_*` views) are deferred.
  - Plus `view_errors` (8 routes, `errorsview` profile: error events in 3 groups + a resolution) —
    the non-grouped narrow+hydrate path, the grouped aggregate path, and resolved=0/1/all + filters.
  - Plus `view_incident` trace path (2 routes, `?trace_id=<tracedetail trace>`): primary-trace
    resolution + related errors/logs/spans/rum/windows/metrics/anomaly gathering. error_id /
    rum_session match paths deferred (need computed ids).
  - Plus `view_ai` (9 routes, `aiview` profile: otel_traces AI spans): ai_items build, flat + trace
    view modes, totals, and the service/model/operation/span_name/row_type/sql filters.
  - Plus the POST cluster (zero new Go bugs — POST-JSON/form handlers have little latent surface):
    `api_import_reports` (8 routes, `reportsimport` profile — envelope/item validation +
    skip/replace/rename/insert), and `create_metrics_rule` + `create_tag_rule` (12 routes,
    `rulecreate` profile — validation branches + success inserts, isolated, flash+redirect).
  - Plus the **derived-signals views** (previously deferred as "hard"): `view_metrics` +
    `view_metrics_anomaly` (11 routes, `metricsauto` seed). Cracked via (a) pairing filters with
    `signal=log_volume` → one signal group (no `ORDER BY` tie) and (b) masking the wall-clock bucket
    time with `[0-9-]+[ +][0-9:]+` (the `[ +]` catches the url-encoded `from_ts=r.time`); the drift
    only shows in the full suite (its phases are minutes apart). R10 found here too.
  - Coverage **57.81% → 62.86%** (covered 9,356 / 14,884; uncovered 5,528), floor ratcheted to
    **62.8**. Full Docker parity GREEN **485/0**.
- **Ten render/handler fixes (R1–R10) found by these batches** — all latent in the empty corpus,
  all now matching the Python/Werkzeug/Jinja2 oracle (see `migration/POPULATED_RENDER_FINDINGS.md`):
  float-rendered counts, `url_for` over-encoding, `url_for` None-param omission, hardcoded-empty
  `request.args`, ordinal string `>=`, missing `|string` filter, `normalizeCHTimestamp(time.Time)`,
  `view_ai` SpanAttributes read (cStr'd map), `view_ai` static-vs-dynamic pricing, and float-rendered
  UInt counts in the metrics views (R10). Shared primitives, so per-batch new-bug count fell
  5 → 0 → 1 → 1 → 2 → 0 → 0 → 0 → 1 → 1.
- **Populated-render fixes (found by the logsview/traces batch, all latent in the empty corpus):**
  five genuine Go render-layer bugs fixed — float-rendered COUNT()s, over-aggressive `url_for`
  query encoding (matched to Werkzeug 3.1.8's exact safe set via an oracle probe), `request.args`
  hardcoded empty, ordinal string `>=` always false, and a missing Jinja `|string` filter. These
  are shared primitives, so they de-risk every later populated page. See
  `migration/POPULATED_RENDER_FINDINGS.md`.
- **Empirical finding:** error-path routes plateau coverage fast (their branches are 1–2 lines, often
  already executed) — they keep *verification* value but the headline % needs the **seeded
  populated-data handlers** (view_incident/view_ai/view_errors/…), which dominate the remaining gap
  and are the real (multi-session) work.

**Remaining DoD item — the coverage drive (ongoing operational work):** raise oracle coverage
57.81% → ≥99% by working the backlog (corpus expansion for the 123 route fns + their helpers;
function-level difftests for the 21 lifecycle fns; classify the 13 module/dead lines). This is a
sequential, Docker-capture-bound grind (~1 profile per route-cluster) — the machinery makes it
systematic and the ratchet guarantees monotonic progress. Drive via the agent roster, parity-gated.

- **Next:** continue the `route` bucket of `coverage_backlog.md` top-down — `api_import_reports`
  (73), `view_metrics`/`view_metrics_anomaly`, the AI POST handlers, … — one seeded/feature-on
  profile per cluster, parity-GREEN, ratchet the floor. DONE: `view_logs`, `view_traces` list,
  `view_rum` (filters; vitals derived-signal lines deferred), `view_errors`, `view_incident` (trace
  path; error_id/rum_session match deferred), `view_ai`. The nine render/handler primitives fixed so
  far make these go much smoother. Drain `FUZZ_FINDINGS.md` (F2/F3).
