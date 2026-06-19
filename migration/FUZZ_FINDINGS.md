# Differential fuzz findings

Produced by `migration/tools/fuzz_diff.py` (app.py test-client vs Go binary, byte-equal on the same
random input). These are divergences on branches the golden corpus never exercised.

## FIXED

### F1 — chdb error wrapper defeated publicDashboardQueryError (validate-filter + every error path)
- **Fixed** in `go/cmd/sobs/query_exec.go`: `publicDashboardQueryError` now strips the Go-only
  `chdb query: ` wrapper (from `internal/store/chdb.go:152`) before the `Code:/DB::Exception:`
  cleaning, so it matches Python's unwrapped exception. General fix — corrects every error-path
  caller (all were latent because their error branches are uncovered). validate_filter fuzz went
  0 → 64/80 byte-identical (seed 1, 80 cases).

## OPEN

### F2 — validate-filter: Go's normalized WHERE yields a different chdb error than Python
- **Surface:** `POST /api/{logs,ai}/validate-filter` with column refs that fail to resolve.
- **Symptom:** py `"Unknown expression or function identifier..."` / `"There is no supertype..."` /
  `"Parameter for function..."` vs go `"Unknown table expression identifier..."`. The probe query
  Go builds (`normalizeLogsSQLWhere` / `normalizeAiSQLWhere` → `SELECT 1 FROM ... WHERE <norm>`)
  differs from Python's enough to trigger a different ClickHouse diagnostic, and the `normalized`
  field text also differs in some cases. Needs alignment of the normalize step + probe SQL.
- **Repro:** `python migration/tools/fuzz_diff.py --surface validate_filter --seed 1` (the
  `Unknown table expression` mismatches).

### F3 — non-dict JSON body: Python 500s, Go returns 200
- **Surface:** `POST /api/{logs,ai}/validate-filter` (and likely other `get_json` handlers) with a
  JSON **array** body, e.g. `["[a-z]+"]`.
- **Symptom:** py `(payload or {}).get("sql")` raises `AttributeError` on a list → 500 HTML error
  page; Go's `bodyMap` coerces to empty → 200 `{"issues":[],"normalized":""}`. Mirroring Python's
  500 byte-for-byte (its error page) is the faithful behavior. Low value (malformed array body).
- **Surface:** `POST /api/logs/validate-filter`, `POST /api/ai/validate-filter` (invalid `sql`).
- **Oracle:** `app.py:23852` → on exception, `_public_dashboard_query_error(exc)` (`app.py:20489`)
  strips the `Code: N. DB::Exception:` prefix and trailing stack/`while executing` noise →
  `"Syntax error: failed at position..."`, `"Unrecognized token..."`, `"Unknown expression..."`.
- **Go:** returns the raw error, e.g. `"chdb query: Code: 62. DB::Exception: Syntax error..."`.
  Go already HAS `publicDashboardQueryError` (`go/cmd/sobs/query_exec.go:23`) but the validate-filter
  handlers don't call it — AND Go wraps chdb errors with a `chdb query: ` prefix that the regex
  (`^Code:\s*\d+\.\s*DB::Exception:\s*`) wouldn't strip even if called.
- **Fix:** route the validate-filter exception through `publicDashboardQueryError`, and make the
  Go chdb error string match the shape the stripper expects (drop/relocate the `chdb query: ` prefix,
  or extend the stripper to tolerate it). Then add a `validateerr` profile so this branch is byte-tested.
- **Repro:** `python migration/tools/fuzz_diff.py --surface validate_filter --seed 1`
