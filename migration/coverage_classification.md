# Uncovered-line structural classification (DoD-3)

app.py statements: **14884** · covered **12600** (84.65%) · uncovered **2284**

Heuristic, conservative (unsure → COVERABLE). Deferred buckets are a *lower bound* on the structural ceiling; COVERABLE is an *upper bound* on remaining corpus work.

| reason | lines | % of uncovered | note |
|---|---:|---:|---|
| DEFENSIVE_EXCEPT | 371 | 16.2% | except-body pass/log/continue — needs fault injection |
| FAULT_INJECTION | 30 | 1.3% | except handler reachable only by an infra fault (WriteQueueFullError etc.) — returns a 5xx but un-triggerable by a deterministic request |
| NOW_WINDOW | 1 | 0.0% | chdb now() wall-clock — needs now()-anchored seed or unverifiable |
| LIBRARY_ERR_TEXT | 39 | 1.7% | message embeds a Python library exception string (re/json/chdb) — differs Go-vs-Py (F2) |
| COVERABLE | 1843 | 80.7% | a byte-parity route can reach it (schedulable) |

**Structurally-deferred (lower bound): 441 lines (19.3% of uncovered).**
**Deterministically-reachable coverage ceiling (covered + COVERABLE): ~97.0%** of app.py statements — the realistic DoD-3 target for the corpus (the rest is classified as deferred above, not coverable by deterministic byte-parity).

> Note: most COVERABLE lines are seeded populated-handler success paths — these go byte-GREEN once captured with the CORRECT seeded-profile flow (seed_fixtures.py --only-profile X before capture). A residual few expose genuine Go port bugs (e.g. raw_attrs JSON key-order) that must be fixed first; the *lines* are reachable either way, so they count toward the ceiling.
