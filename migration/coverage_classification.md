# Uncovered-line structural classification (DoD-3)

app.py statements: **14884** · covered **11538** (77.52%) · uncovered **3346**

Heuristic, conservative (unsure → COVERABLE). Deferred buckets are a *lower bound* on the structural ceiling; COVERABLE is an *upper bound* on remaining corpus work.

| reason | lines | % of uncovered | note |
|---|---:|---:|---|
| DEFENSIVE_EXCEPT | 386 | 11.5% | except-body pass/log/continue — needs fault injection |
| NOW_WINDOW | 1 | 0.0% | chdb now() wall-clock — needs now()-anchored seed or unverifiable |
| LIBRARY_ERR_TEXT | 44 | 1.3% | message embeds a Python library exception string (re/json/chdb) — differs Go-vs-Py (F2) |
| COVERABLE | 2915 | 87.1% | a byte-parity route can reach it (schedulable) |

**Structurally-deferred (lower bound): 431 lines (12.9% of uncovered).**
**Deterministically-reachable coverage ceiling (covered + COVERABLE): ~97.1%** of app.py statements — the realistic DoD-3 target for the corpus (the rest is classified as deferred above, not coverable by deterministic byte-parity).

> Note: most COVERABLE lines are seeded populated-handler success paths — these go byte-GREEN once captured with the CORRECT seeded-profile flow (seed_fixtures.py --only-profile X before capture). A residual few expose genuine Go port bugs (e.g. raw_attrs JSON key-order) that must be fixed first; the *lines* are reachable either way, so they count toward the ceiling.
