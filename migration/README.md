# SOBS Python → Go Migration Framework

This directory is a **self-contained migration harness**. Its purpose is narrow and
specific: let a human or an autonomous agent pursue **one goal** — *"make the Go
implementation produce byte-for-byte identical output to the Python implementation
for every route"* — without having to re-derive strategy, re-audit the codebase, or
make subjective "is this close enough?" judgements.

Prior migration attempts failed because they ported code *module by module* and
judged parity *by eye*. With a 34,000-line `app.py`, 75 Jinja templates, and
byte-level escaping differences between Jinja2 and Go's `html/template`, eyeballing
parity is hopeless and unfalsifiable. **This framework removes human judgement from
the parity question entirely.**

## The core idea

> **The Python app is the oracle.** We freeze it into a deterministic state, capture
> the exact bytes it emits for every route into a *golden corpus*, and then drive the
> Go port purely against a byte-diff of that corpus. A test, not a person, decides
> what "done" means. "Done" = every golden is green.

```
 ┌─────────────┐   freeze inputs    ┌──────────────┐   capture bytes   ┌─────────────┐
 │  Python app │ ─────────────────► │ deterministic│ ────────────────► │   golden    │
 │  (the spec) │  time/uuid/rand/db │   Python app │  status+hdrs+body │   corpus    │
 └─────────────┘                    └──────────────┘                   └──────┬──────┘
                                                                              │ byte-diff
 ┌─────────────┐   same inputs      ┌──────────────┐   capture bytes          │
 │   Go app    │ ─────────────────► │ deterministic│ ────────────────────────►│
 │ (the port)  │  same frozen state │    Go app    │                     red / GREEN
 └─────────────┘                    └──────────────┘
```

## Files

| File | What it is |
|------|------------|
| [`GOAL.md`](GOAL.md) | **Start here.** The single goal definition + the autonomous loop an agent runs to complete the migration. |
| [`AUDIT.md`](AUDIT.md) | The consolidated codebase audit. Every fact the port needs. Read once. |
| [`PARITY_STRATEGY.md`](PARITY_STRATEGY.md) | How the golden-corpus test engine works, the determinism contract, and what counts as parity. |
| [`JINJA_TO_GO_SPEC.md`](JINJA_TO_GO_SPEC.md) | The template-translation specification. Exact rules for reproducing Jinja2 output in Go `text/template`. **The hardest part of the migration lives here.** |
| [`PHASES.md`](PHASES.md) | Phased plan with hard gates. Phase 0 (the chdb gate) must pass before any porting. |
| [`LEDGER.md`](LEDGER.md) | Per-route parity status. Auto-maintained by the parity runner. The burndown chart. |
| [`tools/`](tools/) | The runnable harness: route extractor, determinism shim, fixture seeder, golden capturer, parity differ. |
| [`manifest/`](manifest/) | The route manifest (request specs) + fixtures that drive capture and replay. |
| [`../go/`](../go/) | The Go implementation skeleton + the Go-side parity test runner. |

## One-paragraph quickstart

```bash
# 0. Verify the chdb gate first (see PHASES.md Phase 0) — non-negotiable.
# 1. Generate / refresh the route manifest from the live Python source:
python migration/tools/extract_routes.py > migration/manifest/routes.generated.yaml
# 2. Seed the deterministic fixture database:
python migration/tools/seed_fixtures.py
# 3. Capture the golden corpus from the frozen Python app:
python migration/tools/capture_golden.py
# 4. Build the Go app, then check parity (this is the burndown loop):
python migration/tools/parity_check.py            # writes LEDGER.md, exits non-zero until 100% green
```

The migration is complete when `parity_check.py` exits `0` with every route GREEN.
