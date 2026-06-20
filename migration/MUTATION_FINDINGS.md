# Mutation-testing findings

Produced by `migration/tools/mutation_test.py` — "verify the verifier". It perturbs a COVERED
constant in app.py (bool flip / int+1 / str+"X"), re-captures the named golden route(s) from the
mutated oracle, and checks whether the golden bytes change:
- **KILLED**  — a golden changed → the corpus observes this line (good).
- **SURVIVED** — goldens identical → the line ran during full-corpus coverage but its effect never
  reaches *the compared route's* captured response.

A survivor bounds what byte-parity can catch: parity could not detect a Go divergence on that exact
line either. So survivors are either real corpus blind spots or methodology artifacts (below).

## Result (2026-06-20, representative sample)

The corpus **kills behavioral mutations on exercised lines** — e.g. in `summary()`:
`'SELECT count() AS cnt FROM ('` → killed, and the `if … or "true"` boolean literal → killed. This
confirms the verifier works: changing oracle behavior changes the goldens.

## Methodology caveat (why per-function scores read low)

`mutation_test.py` mutates **every** covered constant in a function span, but `--only` compares a
**single** route+profile. A multi-branch handler (e.g. `summary()` has base / `summaryrich` /
`cveview` / cve-overview branches gated by settings + seeded data) is only partially exercised by
any one route, so constants in the *other* branches survive **trivially** — not because the corpus
is blind, but because that route doesn't run them. Meaningful scoring requires pairing a function
with the route+profile that maximally exercises it (or `--lines` narrowed to the route's hot path).
A full campaign (every function × its exercising routes) is future work; the mechanism + harness are
validated here.

## Harness hardening (this session)

A mutant that *breaks* the oracle (capture subprocess crashes) is now counted **KILLED** (a crash is
a detectable change) instead of aborting the whole sweep via `check=True`. So one pathological
mutant no longer kills the run.

## Open gap (worthwhile to close)

### G1 — summary cve-overview *populated* severity tally is not byte-asserted
`summary()` lines ~10945–10958 tally `sobs_cve_findings` by severity into
`cve_overview[critical|high|medium|low|total]`. Mutating the `'LOW'`/`'low'`/`'MEDIUM'`/… branch
constants **survives** because no golden renders the *populated* counts:
- `get__root` (base): renders `0 total` — the for-loop severity branches never run (no findings).
- `get__root__cveview`: renders `CVE scanning is disabled.` — also does not surface populated counts.

(Observed by byte-diffing the two goldens; neither contains a non-zero `critical/high/medium/low`
tally, so the populated path is unobserved regardless of the exact gating.)

So the per-severity tallying branches — and the Go port of them — are not byte-verified. **To close:**
add a profile that BOTH seeds `sobs_cve_findings` (1+/severity) AND leaves cve enabled, capture
`get__root` under it, and confirm parity GREEN — the populated `critical:N high:N …` counts then
byte-assert the branches and kill the mutants. (A discrete M-series corpus-expansion item.)
