# Differential fuzz findings

Produced by `migration/tools/fuzz_diff.py` (app.py test-client vs Go binary, byte-equal on the same
random input). These are divergences on branches the golden corpus never exercises.

**Current status (2026-06-20):** seeds 1–4 × 400 cases (validate_filter + rum_ingest) →
**0 real mismatches**, only the catalogued *accepted* class below. Run:

```
docker run --rm -v "$REPO:/repo" -w "$CW" -e CHDB_LIB_PATH=/repo/.libchdb/libchdb.so \
  sobs-parity:latest python migration/tools/fuzz_diff.py --surface all --cases 200 --seed 1
```

## FIXED

### F1 — chdb error wrapper defeated publicDashboardQueryError
`publicDashboardQueryError` (`go/cmd/sobs/query_exec.go`) strips the Go-only `chdb query: ` wrapper
before the `Code:/DB::Exception:` cleaning, matching Python's unwrapped exception. General fix for
every error-path caller.

### F2 — HARNESS BUG: Go fuzz DB had 0 tables (every probe → UNKNOWN_TABLE)
Not an app divergence. `fuzz_diff.py::_run_go` copied the fixture DB with `shutil.copytree(...)`
**without `symlinks=True`**. chdb's Atomic database engine maps the `default` database via a relative
symlink (`metadata/default -> ../store/<uuid>`); dereferencing it breaks the mapping so Go opened a
DB with **no tables**, and every validate-filter probe failed with `Unknown table expression
identifier 'otel_logs'/'otel_traces' (UNKNOWN_TABLE)` — masquerading as ~200 error/success
divergences. Fixed by adding `symlinks=True` (mirrors `parity_check.py`). This single fix resolved
the bulk of the apparent mismatches.

### F4 — publicDashboardQueryError truncated by bytes, not codepoints
`go/cmd/sobs/query_exec.go`: Python truncates the message with `len(message) > 280` /
`message[:277].rstrip()` (codepoint-based); Go used a byte length + byte slice, so a long chdb error
echoing multibyte input (🚀/𝕊/é) truncated at a different offset. Now uses rune length + rune slice +
`unicode.IsSpace` right-strip.

### F5 — RUM ingest error response leaked an extra `"ok":false`
`go/cmd/sobs/handlers_v1_ingest.go`: the write-failure path used `errorJSON` (`{"error":..,"ok":false}`)
but app.py `ingest_rum` uses `_json_error` → `{"error": msg}` with no `ok` field. Switched to a bare
`{"error": "rum ingest write failed"}`.

### F6 — RUM/ingest DateTime columns not normalized (scalar timestamp coercion divergence)
`go/cmd/sobs/handlers_v1_ingest.go` + `mutation_helpers.go`. app.py `_insert_rows_json_each_row`
runs `_normalize_ch_timestamp` on the dt-key columns (`Timestamp`, `TimeUnix`, …) before inserting,
so a raw user value like `false`/`1.5`/`-0.0`/`""` is coerced to a valid DateTime (or `now`), and
some values (e.g. negative epochs) are passed through to a chdb error consistently. Two bugs:
  1. The `/v1/rum`, `/v1/ai`, `/v1/errors` handlers inserted via `s.db.InsertJSONEachRow` directly,
     **bypassing** the normalizing wrapper. Routed through `insertRowsNormalized`.
  2. `normalizeCHTimestamp` used `toStr(v)` unconditionally, so falsy non-strings became
     `"False"`/`"0"` instead of Python's `str(value or "")` → `""` → `now`. Now gated on `rumTruthy`.
  Also: the RUM `ts` field is kept raw (`event.get("timestamp", now)` is NOT `str()`-wrapped in the
  oracle), so the normalizer — not a stringify — decides coercion.

## ACCEPTED (deliberately not matched)

### A1 — framework 500 page on a degenerate non-dict JSON body
A non-dict JSON body (`["x"]`, `9007199254740993`, `"a|b"`) makes the Python handler's
`(payload or {}).get(...)` raise an unhandled `AttributeError`, so a PRODUCTION Quart server returns
its generic `<!doctype html> … 500 Internal Server Error` page. This is an app.py bug + framework
artifact, not app logic; the Go port degrades gracefully (200 with an empty result). We deliberately
do **not** replicate a framework error page byte-for-byte — it would couple the port to Quart
internals (same precedent as library-error-text divergences). The fuzzer classifies and counts these
separately and they do **not** fail the run. (~6–10% of random cases per seed.)

> Harness note: `fuzz_diff.py` sets `PROPAGATE_EXCEPTIONS=False` on the oracle so it returns the
> production 500 page (boot sets `TESTING=True`, which would otherwise re-raise), and guards each
> case so one degenerate input can't abort the sweep.
