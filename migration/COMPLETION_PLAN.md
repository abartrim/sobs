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
- [ ] `ci.yml`: add `go-main` to `push`/`pull_request` triggers for the build / vet / gofmt /
      unit-test / **Docker parity** / **coverage** jobs.
- [ ] `ci.yml`: gate the ghcr-publish push + `register-sobs-release` + `cleanup-ghcr` + any
      `environment:`/deploy job to **`main`/tags only**
      (`if: github.ref == 'refs/heads/main' || startsWith(github.ref,'refs/tags/v')`).
- [ ] `ci.yml`: image tags `:go-main` + `:<sha>` only — **never `:latest`** from `go-main`.
- [ ] Add a coverage CI job: run `coverage_capture.py`, fail if app.py coverage regresses below the
      committed floor (start 58%, ratchet up).
- [ ] Branch protection on `go-main`: require build/test + Docker parity GREEN + coverage-no-regress.

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

## Current status snapshot (2026-06-18)

- 0 `not implemented` stubs; Docker parity GREEN 396 / RED 0.
- 2026-06-18 audit (`GO_PARITY_DIVERGENCE_AUDIT.md`, ~171 findings, 2 CRITICAL) fully fixed.
- DLP masking + tag-rule regex now use `dlclark/regexp2` for Python-`re` parity.
- `go-main` created + pushed; `COMPLETION_PLAN.md` + `coverage_capture.py` committed.
- **Oracle coverage measured = 58%** (6,280 / 14,884 app.py stmts uncovered) — the remaining risk
  surface, recorded in `migration/coverage_app.json`.
- **Next:** (a) classify the 6,280 uncovered lines into buckets (corpus-expandable / needs-difftest /
  dead); (b) wire `go-main` CI per the checklist above (58% coverage floor); (c) stand up the agent
  roster to drive the backlog.
