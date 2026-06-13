# Phases & Gates

The goal is one (make the corpus green), but it is reached through ordered phases with
**hard gates**. A gate that fails halts the migration — do not proceed past a red gate
by working around it. Past attempts died by skipping Phase 0 and discovering the data
layer was unviable after writing thousands of lines.

---

## Phase 0 — Viability gate: the chdb round-trip  ⛔ HARD GATE

**Nothing else starts until this passes.** The hard cutover requires the Go process to
read and write the *same* on-disk `data/sobs.chdb` directory that Python `chdb 4.1.9`
created. If the storage formats are incompatible, the entire approach is wrong and we
need a different strategy (e.g. a migration/export step) — better to learn it now.

Steps:
1. Pin versions. Python: `chdb==4.1.9` (already; needs `chdb-core>=26.5.0`). Go:
   `github.com/chdb-io/chdb-go` built from `main` against a **pinned** `chdb-core`
   whose ClickHouse kernel matches chDB 4.1.9 (do **not** use `lib.chdb.io | bash`
   unpinned or `chdb-core latest`). Record the exact pin in `go/CHDB_PIN.md`.
2. `python migration/tools/gate0_chdb.py` — creates a temp dir with Python chdb,
   writes a `ReplacingMergeTree` table with rows, `OPTIMIZE … FINAL`, closes it.
3. `go test ./go/internal/store -run TestGate0RoundTrip` — opens that **same** dir with
   chdb-go, reads the rows back (asserts equality), writes more, closes.
4. Re-open from Python; assert all rows present. Re-open from Go; assert again.

**Gate criterion:** rows written by either engine are readable, correct, and stable
when re-opened by the other, across `MergeTree`/`ReplacingMergeTree` with `FINAL`.
If it fails: STOP. Report the failure mode (forward-incompat? cgo/link issue? version
skew?) and escalate the version-pin decision before any porting.

Also in Phase 0 (cheap, parallel):
- Stand up the Go module skeleton (`go/`), confirm it builds and serves a hello route.
- Confirm `google.golang.org/protobuf` + generated OTEL stubs compile.
- Confirm the cgo build works in the target Docker base image (the deploy artifact must
  link libchdb/chdb-core — update `Dockerfile`/`k8s` in Phase 5, but prove it links now).

---

## Phase 1 — Harness bring-up

Stand up the parity engine end-to-end on a **trivial** subset (3–5 routes: one
`*_help.html`, one tiny JSON route, one static file) to prove the loop works before
scaling.

1. `extract_routes.py` produces a manifest; hand-author request specs for the subset.
2. `seed_fixtures.py` builds the fixture DB.
3. `capture_golden.py --verify-stable` → captures are stable against themselves.
4. Implement just enough Go (server, router, template engine, JSON encoder, header
   middleware) to serve the subset.
5. `parity_check.py` shows those routes GREEN.

**Gate:** the subset is green AND `--verify-stable` is clean. The machine works.

---

## Phase 2 — Shared chrome & the template engine

Make `base.html`, the 11 partials, the 8 macros, the filter set, and the context
processor byte-perfect. Validated via the 29 `*_help.html` pages (low data dependency).

**Gate:** all `*_help.html` routes GREEN. (This proves §6/§7/§9 of the Jinja spec.)

---

## Phase 3 — JSON API surface

Port the ~109 JSON routes. Get `go/internal/jsonenc` byte-perfect on a handful, then
the rest fall in groups. Includes OTLP ingest (`/v1/*`): protobuf parse → JSONEachRow
insert → JSON ack. Reuse the SQL verbatim against chdb-go.

**Gate:** all JSON + redirect + CORS-OPTIONS routes GREEN.

---

## Phase 4 — Data-heavy HTML pages

The `tojson`-in-`<script>` templates (`metrics`, `traces`, `rum`, `logs`, `errors`,
`work_items`, `kubernetes`, `ai`, `cve`, settings_*, dashboards, …). Hardest parity
work; done last when engine + encoder + chrome are proven. Each page = the handler
gathers data (chdb queries, ordered exactly), the template renders it.

**Gate:** every remaining route GREEN. `parity_check.py` exits 0. **Migration goal met.**

---

## Phase 5 — Cutover (hard)

Approved: no clients, hard cutover acceptable.

1. Live-server smoke: run the real Go server and the real Python server side-by-side
   against a copy of production-shaped data; `tools/live_smoke.py` replays the corpus
   over HTTP against both and diffs (catches anything the test clients masked:
   Hypercorn vs net/http header framing, chunking, compression).
2. Update `Dockerfile` (multi-stage: build Go + bundle libchdb/chdb-core, `templates/`,
   `static/`), `docker-compose.yml`, `k8s/` to run the Go binary.
3. Cut over. Keep the Python image tagged for one release as rollback.
4. Delete Python sources once the Go image is in prod and stable.

**Gate:** live smoke clean; Go image healthy in prod.

---

## Phase 6 — Post-parity (improvements now allowed)

Only after green. Now the things deferred for parity get done, each behind its own
tests (no longer golden-bound, since intentional divergence begins here):
- Port telemetry self-instrumentation (`telemetry/`) for ops parity.
- Re-implement Vanna NL→SQL natively (or drop) — without pandas.
- Background workers (CVE scan, notifications, agent runs) with integration tests.
- Resolve any `EXCLUSIONS.yaml` entries (streaming/SSE endpoints).
- Any actual bug fixes / features the team wanted but were forbidden during parity.

---

## Estimated shape (not a schedule)

| Phase | Relative effort | Risk |
|---|---|---|
| 0 chdb gate | small | **make-or-break** |
| 1 harness | small | low |
| 2 chrome/engine | medium | high (Jinja escaping) |
| 3 JSON | medium | medium (encoder bytes) |
| 4 data HTML | large | high (`tojson`/script) |
| 5 cutover | small | low |
| 6 improvements | open-ended | n/a |

The bulk of *novel* difficulty is Phases 2 and 4 — both are template/escaping parity,
both are front-loaded into the spec and the golden harness so they're falsifiable
rather than subjective.
