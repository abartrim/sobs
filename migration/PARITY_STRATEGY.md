# Parity Strategy — the test engine

This document defines *how* parity is measured and *why* the method is sound. The
whole migration's success rests on this being airtight. If the harness is sloppy,
"green" means nothing and we fail again.

## 1. Definition of parity

For a given **request spec** `R` (method, path, query string, headers, body, and
any required pre-state), the Python app and the Go app are *at parity* iff, when both
run against the **same frozen inputs**:

```
normalize(go_response(R))  ==  normalize(python_response(R))     # byte-for-byte
```

where a "response" is the triple `(status_code, headers, body_bytes)` and `normalize`
is the **minimal, explicit** transform in [`tools/normalize.py`](tools/normalize.py).

- **Body bytes are compared raw — never normalized.** Not pretty-printed, not parsed,
  not re-serialized. Raw bytes.
- **Headers** are compared as a **case-insensitive, order-independent multiset** of
  `(name, value)` pairs, after blanking only: `Date` (wall-clock), `Server` (engine
  identity), and the signed value inside a `Set-Cookie: sobs_session=...` (HMAC over the
  same payload differs by signing impl; the *payload* is compared after unsign — see
  normalize.py). Header **presence and values** are otherwise compared exactly. Cross-name
  **order is not compared**: it is not semantically significant in HTTP and the two
  server stacks legitimately differ (Quart preserves insertion order; Go's `net/http`
  emits sorted). This is the one concession to HTTP-framing reality — it never touches
  the body.
- **Status code** compared exactly.

If you ever feel the need to widen `normalize`, that is a signal the Go port is wrong.
The allow-list is frozen by design and changing it requires editing GOAL.md's rules
and justifying it in this file's changelog.

## 2. The determinism contract

Byte-parity is impossible if either app's output depends on wall-clock, randomness, or
unstable data ordering. So before capture we **freeze every entropy source**. The same
freezing is applied to the Go app under test.

| Entropy source | Python freeze (`tools/determinism.py`) | Go freeze (`go/internal/clock`, `go/internal/idgen`) |
|---|---|---|
| Time | monkeypatch `time.time`, `time.time_ns`, `datetime.now`, `datetime.utcnow`, `datetime.today` → fixed `2024-01-02T03:04:05.000000Z` (epoch `1704164645`) | `clock.Now()` returns the same fixed instant when `SOBS_PARITY=1` |
| UUIDs | `uuid.uuid4` → deterministic sequence seeded `0` (`uuid5`-style counter) | `idgen.UUID4()` mirrors the exact same sequence |
| Tokens / `os.urandom` / `secrets` | deterministic PRNG (seeded ChaCha/`random.Random(0)`) | matching seeded stream |
| Env / config | `tools/parity_env.sh` pins every `SOBS_*` env var the output can depend on: `SOBS_SECRET_KEY`, `SOBS_SETTINGS_ENCRYPTION_SECRET`, `BUILD_VERSION`, feature flags, breakpoints | same env file sourced for the Go process |
| chdb data | `tools/seed_fixtures.py` builds a fixed dataset; all queries used in goldens must be deterministically ordered | Go opens the **same** seeded directory |
| Dict / JSON key order | Python 3.7+ insertion order is stable and *is the spec* | Go encoder must emit keys in the **same order** (see §4) |

**Critical discipline:** prefer *freezing an input* over *normalizing an output*. Every
normalization rule is a hole in the parity guarantee. We aim for **zero body
normalization and a 3-item header allow-list**, achieved by freezing inputs hard.

### Stability self-test
`capture_golden.py --verify-stable` captures the corpus **twice** and diffs the two
captures. Any route that differs between two captures of the *same* frozen Python app
has a determinism leak — fix the shim, do not proceed. A corpus that isn't stable
against itself can never validate the Go port.

## 3. Capture mechanism

`capture_golden.py`:
1. Imports `determinism.py` **before** importing `app` (monkeypatch must precede app
   import so module-level timestamps are frozen too).
2. Sources `parity_env.sh` values.
3. Points the app at the seeded fixture DB (`SOBS_DATA_DIR=migration/fixtures/data`).
4. Builds Quart's `app.test_client()`.
5. For each request spec in `manifest/routes.yaml`, issues the request and records:
   ```
   migration/golden/<route_id>/
     request.json     # the exact spec replayed (method, path, query, headers, body ref)
     status           # e.g. "200"
     headers.txt      # emitted headers, in emission order, one per line "Name: value"
     body.bin         # raw response bytes
     meta.json        # content-type, body sha256, byte length, capture timestamp
   ```
6. Writes `migration/golden/INDEX.json` (route_id → sha256 of the normalized triple).

The **test client**, not a live server, is used for Python capture — it removes
Hypercorn's `Date`/`Server`/connection noise and is reproducible. (The Go side uses
`net/http/httptest` symmetrically.) A separate Phase-5 live-server smoke compares the
two *actual* servers end-to-end to catch anything the test clients hide.

## 4. JSON byte-parity (the `jsonify` / `tojson` problem)

The 109 JSON routes and 150 in-template `tojson` calls only pass if the Go encoder
reproduces Python's bytes exactly. Do **not** assume — the harness pins it, but here is
what to reproduce (verify against goldens, adjust the Go encoder until the diff is
empty):

- **`jsonify` (response bodies):** Quart's default JSON provider. Capture a known
  route's golden and read off, then match in Go: key ordering (Quart's provider
  historically **sorts keys**, but the wrapped `jsonify` at `app.py:93` may pass
  dict-ordered payloads — *verify per-route from the golden*), item/key separators,
  `ensure_ascii` behavior (non-ASCII → `\uXXXX` or raw UTF-8), and the **trailing
  newline** Flask/Quart appends. Encode these as explicit options on a custom Go
  encoder in `go/internal/jsonenc`, not `encoding/json` defaults (Go sorts map keys
  and HTML-escapes `<>&` by default — both likely wrong here).
- **`json.dumps` call sites** use varied options (`separators=(",", ":")`,
  `sort_keys=True`, `ensure_ascii=False`, `indent=2`) — see `AUDIT.md` references and
  `app.py` lines 4289, 7549, 7805, 8786, 10565. Each Go call site must mirror the
  exact options of its Python origin. `extract_routes.py --json-call-sites` lists them.
- **`tojson` (in templates):** Jinja's HTML-safe JSON: compact `json.dumps` then
  escape `<`→`<`, `>`→`>`, `&`→`&`, `'`→`'`. Port as the Go
  template filter `tojson` in `go/internal/render`. (Spec'd in `JINJA_TO_GO_SPEC.md`.)

Go's default `encoding/json` is **wrong** for all three (sorts map keys; HTML-escapes;
no trailing newline). Use ordered structures (slices of KV, or `json.RawMessage`
assembly, or a small ordered-map encoder) so key order is explicit and controlled.

## 5. Replay & diff mechanism

`parity_check.py`:
1. Builds the Go binary (`go build ./go/cmd/sobs`) — fails loud if it doesn't compile.
2. Boots it with `SOBS_PARITY=1`, `parity_env.sh`, and `SOBS_DATA_DIR` pointed at the
   **same** seeded fixtures (a fresh copy per run so writes don't poison reruns).
3. Replays each `manifest/routes.yaml` spec against the Go server.
4. `normalize`s both sides and diffs `(status, headers, body)`.
5. Writes `LEDGER.md` (per-route GREEN/RED + first-diff summary) and a machine-readable
   `migration/golden/RESULTS.json`.
6. Exits `0` only if every route is GREEN.

Flags: `--only <route_id>` (single route, with full byte diff printed),
`--update-ledger`, `--max-diffs N`, `--bisect-body` (prints the first differing byte
offset + a 120-byte window on each side — invaluable for whitespace/escaping bugs).

## 6. Coverage accounting (no silent gaps)

A migration that passes 150/188 routes and quietly ignores 38 is a failure dressed as
success. The harness enforces coverage:
- `extract_routes.py` is the source of truth for the route set. `parity_check.py`
  asserts every extracted route has either (a) a golden + a GREEN result, or (b) an
  explicit entry in `manifest/EXCLUSIONS.yaml` with a written reason and an owner.
- The only legitimate exclusions are routes that are genuinely impossible to capture
  deterministically *and* are out of scope per `AUDIT.md` §10 (e.g. a streaming SSE
  endpoint whose framing is timing-dependent). Each exclusion needs a Phase-6 follow-up.
- `LEDGER.md` shows `GREEN / RED / EXCLUDED / total` at the top. "Complete" means
  `RED == 0` **and** `EXCLUDED` is only the documented set.

## 7. Why this succeeds where eyeballing failed

- **Falsifiable.** Every route has a single bit of truth. No debate.
- **Incremental.** One red route at a time; each commit is provably non-regressing
  (re-run the full corpus in CI).
- **Self-checking.** The stability self-test catches harness bugs before they hide
  port bugs.
- **Exhaustive.** Coverage accounting makes "we forgot about X" impossible.
- **Reproduces, doesn't reinterpret.** The Go author never decides what the output
  *should* be — only makes Go match what Python *did*.
