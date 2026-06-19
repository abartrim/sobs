# Uncovered-line structural classification (DoD-3)

app.py statements: **14884** · covered **9810** (65.91%) · uncovered **5074**

Heuristic, conservative (unsure → COVERABLE). Deferred buckets are a *lower bound* on the structural ceiling; COVERABLE is an *upper bound* on remaining corpus work.

| reason | lines | % of uncovered | note |
|---|---:|---:|---|
| DEFENSIVE_EXCEPT | 414 | 8.2% | except-body pass/log/continue — needs fault injection |
| NOW_WINDOW | 7 | 0.1% | chdb now() wall-clock — needs now()-anchored seed or unverifiable |
| LIBRARY_ERR_TEXT | 47 | 0.9% | message embeds a Python library exception string (re/json/chdb) — differs Go-vs-Py (F2) |
| COVERABLE | 4606 | 90.8% | a byte-parity route can reach it (schedulable) |

**Structurally-deferred (lower bound): 468 lines (9.2% of uncovered).**
**Deterministically-reachable coverage ceiling (covered + COVERABLE): ~96.9%** of app.py statements — the realistic DoD-3 target for the corpus (the rest is classified as deferred above, not coverable by deterministic byte-parity).

> Note: COVERABLE still includes seeded populated-handler success paths that currently surface Go port bugs (D1-D3) — those must be fixed before their routes go GREEN, but the *lines* are reachable, so they count toward the ceiling.
