# ADR: Simplicity-First Modularization Reset

## Status

Accepted

## Date

2026-05-02

## Context

The current modularization and coverage campaign improved direct test coverage and extracted substantial logic from `app.py`, but the execution pattern drifted away from the intended outcome.

Observed problems from PR #281:

- Coverage improved, but the codebase also grew in surface area and indirection.
- Helper-first extraction often kept old wrappers and new helpers alive at the same time.
- Progress was difficult to observe because coverage increased without a matching, reliable reduction in `app.py` and route complexity.
- The work became hard to reason about because many small shared modules were introduced before feature ownership became simpler.
- The implementation process did not consistently force the following sequence: remove duplication, move a coherent feature slice, rewire production call sites, delete obsolete wrappers, then measure the outcome.

The result was technically valid but operationally noisy. A task intended to take hours expanded to roughly a week of incremental extraction work with limited simplicity gains.

## Decision

Adopt a simplicity-first modularization approach with the following rules:

1. Feature slices come before helper proliferation.
2. New shared code is allowed only when it replaces duplicated logic used by at least two real call sites, or when a single feature slice clearly needs a pure helper boundary for direct tests.
3. No extraction batch is complete unless production call sites are rewired in the same batch.
4. No compatibility wrapper remains without a named, verified caller.
5. `app.py` size and wrapper count are tracked as first-class success metrics alongside coverage.
6. Every batch must make the system simpler in at least one observable way:
   - fewer lines in `app.py`, or
   - fewer wrappers in `app.py`, or
   - fewer duplicate code paths, or
   - a complete feature group moved to one route/module boundary.
7. Simplicity outranks extraction volume. Smaller numbers of well-chosen modules are preferred over many narrow helper files.

## Consequences

Positive:

- Progress becomes visible and measurable after each batch.
- `app.py` reduction becomes an explicit deliverable rather than a side effect.
- LLM-driven work gets clearer boundaries and fewer opportunities for scope drift.
- Feature ownership becomes easier to understand because each batch targets a complete vertical slice.
- Coverage growth remains useful because it follows simpler architecture instead of compensating for growing indirection.

Negative:

- Some previously extracted helpers may remain in place until a full feature slice can absorb and simplify them.
- Some work that appears “extractable” will now be deferred if it does not reduce complexity in the same batch.
- Batches may be fewer and larger than micro-extractions because each batch must carry deletion and rewiring work too.

## Mandatory Rules For Future Work

### Batch shape

Each batch must follow this order:

1. Identify a cohesive feature slice.
2. Identify duplication inside that slice.
3. Extract only the shared logic that slice actually needs.
4. Move or simplify the feature endpoints/module in the same batch.
5. Rewire all production call sites in the same batch.
6. Remove obsolete wrappers and duplicate paths in the same batch when safe.
7. Add or update direct tests and route tests.
8. Measure the resulting complexity and coverage deltas.

### Explicit non-goals

The following are not allowed as standalone outcomes:

- extracting helpers without simplifying feature ownership,
- adding wrappers without a near-term deletion path,
- increasing test count without reducing duplicated logic or route/app complexity,
- introducing new micro-modules purely because code is movable.

### Allowed reasons to keep a wrapper temporarily

A compatibility wrapper may remain only if at least one of these is true:

- another production route still imports it,
- tests intentionally patch the app-level symbol and the route cannot yet be rewired safely,
- the wrapper injects app-scoped runtime dependencies that have not yet been moved to a stable boundary.

If a wrapper is kept, the batch must record:

- the wrapper symbol,
- the remaining callers,
- the next batch expected to remove it.

## Quality Gates

Every modularization batch must pass:

- `isort`
- `black`
- `flake8`
- `mypy`
- `python3 scripts/check_dead_code.py`
- `pytest tests/`

Required `mypy` rules for the campaign:

- `warn_unused_ignores = true`
- `warn_redundant_casts = true`

Dead-code auditing:

- Use `python3 scripts/check_dead_code.py` as a conservative Vulture pass at `100%` confidence.
- Do not use low-confidence dead-code findings as automatic deletion signals.
- Use them as prompts for targeted wrapper and caller audits.

## Success Metrics

Each batch must report:

- overall coverage delta,
- target file coverage delta,
- `app.py` line or statement delta,
- wrapper count delta in `app.py`,
- number of feature endpoints fully rewired,
- number of obsolete wrappers deleted,
- number of new shared modules created, which should usually be `0` or `1`.

## Superseded Approach

The previous helper-first extraction pattern is superseded for future work. Existing extracted modules remain valid where they already provide real reuse and high-confidence tests, but they should not define the workflow going forward.