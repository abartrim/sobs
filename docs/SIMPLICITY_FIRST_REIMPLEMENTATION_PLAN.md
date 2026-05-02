# Simplicity-First Reimplementation Plan

## Goal

Rework the modularization and coverage campaign so that each batch produces both:

- a measurable simplicity gain, and
- a measurable testability gain.

The target outcome is not only `95%+` coverage in selected extracted surfaces. The target outcome is a system that is easier to understand, easier to change, and visibly smaller in `app.py` and duplicated route logic after every completed batch.

## Core Principles

1. Simplicity is the primary design constraint.
2. Feature slices are the unit of work, not arbitrary helper groups.
3. Shared code must remove duplication, not just move code.
4. Coverage is evidence of a better design, not the design goal by itself.
5. No batch is complete until obsolete call paths are removed or explicitly documented.

## Work Sequence

Every batch must follow this exact order:

1. Select one cohesive feature slice.
2. Identify repeated logic inside that slice.
3. Extract only the minimum shared logic needed for that slice.
4. Rewire the feature slice to use the extracted logic.
5. Delete obsolete wrappers and duplicate call paths where safe.
6. Add direct tests for extracted logic.
7. Add or update route tests for the feature slice.
8. Run the full quality gate.
9. Record the measured deltas.

## Tooling And Rules

### Formatting

- `isort`
- `black`

### Linting

- `flake8`

### Type checking

- `mypy`

Required active `mypy` checks for this campaign:

- `warn_unused_ignores = true`
- `warn_redundant_casts = true`

Optional future rule, only after a cleanup pass:

- `warn_unreachable = true`

### Dead-code audit

- `python3 scripts/check_dead_code.py`

Rules:

- Use Vulture findings only as a prompt for targeted audits.
- Do not delete dynamic or dependency-injection wrappers from Vulture output alone.

### Test gate

- `pytest tests/`

## Hard Constraints For LLM Execution

These constraints are mandatory for any Copilot or LLM-driven implementation issue:

1. Work one milestone at a time.
2. Work one feature slice at a time within a milestone.
3. Do not create a new shared module unless the issue explicitly allows it.
4. Prefer extending an existing shared module over creating a new one.
5. Do not keep both old and new production code paths unless the issue explicitly names the remaining callers.
6. Do not leave compatibility wrappers behind without documenting the exact symbols and callers.
7. Do not count “helper extracted” as success unless the route or app layer became simpler in the same batch.
8. Do not start the next slice until the current slice has passed the full quality gate.

## Milestones

### Milestone 0: Baseline Inventory

Outcome:

- produce a current-state inventory of:
  - route files,
  - app-level wrappers,
  - shared modules worth keeping,
  - wrappers that still have callers,
  - wrappers with a plausible same-batch deletion path.

Measurable success:

- inventory document created,
- wrapper count captured,
- `app.py` statement count captured,
- target feature slices prioritized.

### Milestone 1: `routes/apps.py` Simplification

Scope:

- app registry endpoints,
- release endpoints,
- artifact metadata endpoints.

Required outputs:

- duplicate logic identified and reduced,
- remaining `app.py` wrappers used by this slice audited,
- same-batch deletion attempted for any wrappers that no longer have callers,
- direct tests added where shared logic is real,
- route tests updated or expanded.

Measurable success:

- `routes/apps.py` coverage increases,
- wrapper count for this slice decreases,
- `app.py` statement count decreases or stays flat while duplicated logic decreases,
- no new micro-modules created unless explicitly justified.

### Milestone 2: `routes/settings.py` Final Simplification Pass

Scope:

- remaining route-owned orchestration,
- any still-duplicated settings logic,
- remaining wrappers that exist only for settings flows.

Required outputs:

- reduce residual route complexity,
- remove settings-only wrappers that are no longer needed,
- keep direct shared tests and route tests balanced.

Measurable success:

- `routes/settings.py` remains at or above current coverage,
- wrapper count drops,
- no net increase in helper surface for the settings domain.

### Milestone 3: `app.py` Wrapper Reduction Sweep

Scope:

- only after Milestones 1 and 2 complete,
- target wrappers with verified zero or single removable callers.

Required outputs:

- delete obsolete wrappers,
- rewire tests that patch app-level symbols when safe,
- keep app-scoped dependency injection only where genuinely needed.

Measurable success:

- `app.py` statement count decreases materially,
- wrapper inventory decreases materially,
- dead-code helper remains clean at `100%` confidence.

### Milestone 4: Coverage Consolidation

Scope:

- only after simplification milestones have stabilized,
- target remaining low-coverage business paths with direct tests.

Required outputs:

- fill meaningful branch gaps,
- avoid new indirection,
- preserve simpler architecture from prior milestones.

Measurable success:

- overall coverage increases,
- target modules sustain `95%+` where appropriate,
- no regression in simplicity metrics.

## Per-Batch Template

Every batch report must include:

1. Slice worked on.
2. Duplicate logic removed.
3. Shared module added or extended.
4. Production call sites rewired.
5. Wrappers deleted.
6. Wrappers intentionally kept, with named callers.
7. `app.py` statement delta.
8. Coverage delta.
9. Validation results.

## Definition Of Done

A batch is done only if all of the following are true:

- feature slice changed is still understandable as one unit,
- direct tests cover extracted logic,
- route tests cover the slice behavior,
- dead-code audit passes,
- lint/type/test gates pass,
- measured deltas are recorded,
- the batch made the codebase simpler in at least one observable way.

## Anti-Patterns To Reject

Reject a batch if it does any of the following:

- creates new helpers without reducing duplicate code,
- extracts code but leaves old call paths in place without named reasons,
- adds wrappers to preserve convenience rather than necessity,
- grows `app.py` and shared surface at the same time without deleting anything,
- improves coverage while leaving feature ownership less clear.

## Immediate Next Step

Start with Milestone 0 and produce the current-state inventory before any further modularization work.