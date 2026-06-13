# THE GOAL

> **Make `migration/tools/parity_check.py` exit `0` with every route GREEN, by
> implementing the Go server in `../go/`, without ever modifying the golden corpus
> or the Python source.**

That single sentence is the entire success criterion. It is objective, falsifiable,
and machine-checkable. There is no "looks right." There is only green or red.

This file is written so that a single agent invocation —

> *"Complete the SOBS Go migration per `migration/GOAL.md`."*

— has everything it needs to run to completion autonomously.

---

## Rules of engagement (read these; they are why past attempts failed)

1. **The Python app and the golden corpus are READ-ONLY.** You may read them as the
   specification. You may never edit `app.py`, `mcp.py`, `masking.py`, `templates/`,
   `static/`, or anything under `migration/golden/`. If a golden looks "wrong", it is
   not wrong — it is the spec. Reproduce it exactly.
2. **Parity is byte-for-byte**, after the small, explicit normalization allow-list in
   [`tools/normalize.py`](tools/normalize.py) (only the `Date` header, the `Server`
   header, and — when a route genuinely sets one — the signed-session cookie value).
   Body bytes are **never** normalized. If you are tempted to add a normalization
   rule to make a body pass, stop: the Go output is wrong, not the harness.
3. **We are reproducing behavior, not improving it.** Bugs in the Python output are
   features to be reproduced. No refactors, no "while I'm here" fixes, no schema
   changes. Parity first. (Improvements get their own phase, *after* 100% green —
   see [`PHASES.md`](PHASES.md).)
4. **Minimal dependencies.** The Go module should depend on: the Go standard library,
   `github.com/chdb-io/chdb-go` (the one unavoidable native dep — see Phase 0), and
   `google.golang.org/protobuf` (OTLP ingestion). Reach for nothing else without
   recording the justification in `go/DEPENDENCIES.md`. No web framework, no template
   engine beyond stdlib `text/template`, no ORM.
5. **Work the ledger, smallest blast radius first.** Don't port breadth-first across
   all of `app.py`. Pick the next red route, make it green, commit, repeat. The
   `LEDGER.md` is the burndown.

---

## The autonomous loop

This is the algorithm to run. It is deliberately mechanical.

```
SETUP (once):
  1. Pass Phase 0 (chdb gate). If it fails, STOP and report — the migration is not
     viable until the same on-disk chdb directory round-trips between Python and Go.
  2. python migration/tools/extract_routes.py > migration/manifest/routes.generated.yaml
  3. Reconcile routes.generated.yaml into migration/manifest/routes.yaml
     (add request fixtures — body/query/headers/state — for any route that needs them).
  4. python migration/tools/seed_fixtures.py        # deterministic chdb dataset
  5. python migration/tools/capture_golden.py       # freeze Python → migration/golden/

LOOP (until green):
  6. python migration/tools/parity_check.py --update-ledger
       → writes migration/LEDGER.md, prints the first N red routes with diffs.
  7. Pick the highest-priority red route (priority order below).
  8. Implement / fix the corresponding Go handler + template in ../go/ until that
     route's golden matches byte-for-byte. Use the diff from step 6 as your guide:
       - HTML diffs → consult JINJA_TO_GO_SPEC.md (escaping, whitespace, filters).
       - JSON diffs → key order, separators, trailing newline, number formatting.
       - Header/status diffs → after_request hooks, content-type, ETag.
  9. Re-run parity_check.py for just that route:
       python migration/tools/parity_check.py --only <route_id>
 10. When green, commit: "go: parity for <route_id>".
 11. Go to 6.

DONE:
  parity_check.py exits 0. Every route GREEN. The migration is complete.
  Proceed to PHASES.md "Phase 5: cutover".
```

### Priority order for picking the next red route

Port in dependency order so each fix unblocks the most downstream work:

1. **Infrastructure first** (not a route, but a prerequisite): the chdb wrapper,
   the determinism shim equivalents, the `text/template` engine config, the
   `after_request` header middleware, the context processor. Until these match, no
   route can be green.
2. **`base.html` + the 8 macros + the 11 partials.** Every HTML page inherits from
   these. Get the shared chrome byte-perfect once and dozens of pages get close at
   once. (See `JINJA_TO_GO_SPEC.md` §"Inheritance & macros".)
3. **The `*_help.html` static-ish pages** (29 of them). Low data dependency, mostly
   prose — these validate the template engine itself cheaply.
4. **JSON API routes** (`/api/*`). Order doesn't matter much; they share a JSON
   encoder. Get the encoder byte-perfect against a few, the rest follow.
5. **OTLP ingest routes** (`/v1/logs|traces|metrics`). Protobuf in, JSON-row out.
6. **The data-heavy HTML pages** (`metrics.html`, `traces.html`, `rum.html`, etc.) —
   the `tojson`-in-`<script>` templates. These are the hardest; do them last when the
   engine, encoder, and chrome are all proven.

---

## What "complete" unlocks

When green:
- `go run ./go/cmd/sobs` serves the existing `data/sobs.chdb` and `static/` and
  `templates/` (Go reuses the same template *files*, compiled by the Go engine) and
  is indistinguishable from the Python app over the wire.
- The Python app can be deleted at cutover (hard cutover is approved — no clients).
- Only then do we open the door to fixing/improving anything (Phase 6).

## If you get stuck

A route that won't go green after real effort almost always means one of:
- A **filter or escaping rule** not yet captured in `JINJA_TO_GO_SPEC.md` → add a
  failing unit test to `go/internal/render` reproducing the exact Jinja byte, fix the
  Go filter, then re-run. Update the spec doc.
- A **non-determinism leak** — the golden itself varies between captures. Run
  `capture_golden.py --verify-stable <route_id>` (captures twice, diffs). If unstable,
  the determinism shim is missing a source of entropy; fix `tools/determinism.py`,
  never paper over it with normalization.
- A **chdb result-shape difference** (Go driver returns a column as string where
  Python returned int, etc.) → pin it in `go/internal/store` with a typed scan.

Never resolve a stuck route by editing the golden or relaxing the differ.
