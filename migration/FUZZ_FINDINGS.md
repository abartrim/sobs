# Differential fuzz findings

Produced by `migration/tools/fuzz_diff.py` (app.py test-client vs Go binary, byte-equal on the same
random input). These are divergences on branches the golden corpus never exercised.

## OPEN

### F1 — validate-filter error path returns raw chdb errors (Go) vs cleaned messages (Python)
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
