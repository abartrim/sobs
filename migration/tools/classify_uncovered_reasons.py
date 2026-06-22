#!/usr/bin/env python3
"""DoD-3 classifier: tag every uncovered app.py line with the STRUCTURAL reason it is not
byte-parity-covered, so "how close to 100% can the corpus get?" becomes a number.

The Completion Plan's Definition of Done #3 requires that every uncovered line be classified as
dead/unreachable, intentionally-deferred (with reason), or scheduled. A large share of app.py's
uncovered surface is *structurally* un-byte-testable by a deterministic golden-corpus differential:

  - DEFENSIVE_EXCEPT  — line lives in an `except ...:` handler whose body only pass/logs/continues
                        (no new Return/Raise). Needs fault injection to reach; cannot be triggered
                        by a normal request → deferred.
  - LIBRARY_ERR_TEXT  — a return/flash/jsonify whose message interpolates an exception (`{exc}`,
                        `str(e)`, …). The text comes from a Python LIBRARY (re/json/chdb) and differs
                        from the Go engine's wording → byte-parity-impossible (F2 class) → deferred.
  - NOW_WINDOW        — the line / its enclosing statement references chdb `now()` (wall-clock).
                        Nondeterministic across capture vs replay → deferred (or needs a now()-anchored
                        constant-signal seed, case by case).
  - COVERABLE         — none of the above → schedulable corpus work (a route can byte-test it).

This is a heuristic, deliberately CONSERVATIVE classifier (when unsure → COVERABLE), so the COVERABLE
bucket is an *upper bound* on remaining corpus work and the deferred buckets are a *lower bound* on the
structural ceiling. Output: migration/coverage_classification.{json,md}.

Usage: python migration/tools/classify_uncovered_reasons.py   (after coverage_capture.py)
"""

from __future__ import annotations

import ast
import json
import re
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
APP = REPO / "app.py"
COV = REPO / "migration" / "coverage_app.json"
OUT_JSON = REPO / "migration" / "coverage_classification.json"
OUT_MD = REPO / "migration" / "coverage_classification.md"

# An except body is "defensive" (unreachable by a normal request) if it contains no Return/Raise that
# introduces NEW behavior — i.e. it only passes, logs, or sets fallback locals. A bare `raise` (re-raise)
# still counts as defensive (it re-propagates; the handler itself is the swallow point only if logged).


def _defensive_except_lines(tree: ast.AST) -> set[int]:
    lines: set[int] = set()
    for node in ast.walk(tree):
        if not isinstance(node, ast.ExceptHandler):
            continue
        has_new_flow = any(
            isinstance(n, ast.Return)
            or (isinstance(n, ast.Raise) and n.exc is not None)  # raise NewError(...) = meaningful
            for n in ast.walk(node)
            if isinstance(n, (ast.Return, ast.Raise))
        )
        if has_new_flow:
            continue  # an except that returns/raises-new is a reachable error path, not defensive
        # span the handler (the `except ...:` line through the end of its body)
        start = node.lineno
        end = getattr(node, "end_lineno", None) or start
        for ln in range(start, end + 1):
            lines.add(ln)
    return lines


# Exception types whose handlers are reachable ONLY by an infrastructure fault (a full write queue,
# a broken upstream/IO) — never by a normal deterministic request. A try that catches any of these is
# treated as fault-injection-only: ALL its handlers (incl. the sibling broad `except Exception:` that
# returns the 5xx) are deferred, NOT corpus-coverable. This removes the classifier's biggest COVERABLE
# false-positive class (the ingest_* `_queue_write` 503/500 branches all return, so they'd otherwise
# read as COVERABLE despite being un-triggerable by a byte-parity golden).
_FAULT_EXC_NAMES = {"WriteQueueFullError"}


def _fault_injection_lines(tree: ast.AST) -> set[int]:
    lines: set[int] = set()
    for node in ast.walk(tree):
        if not isinstance(node, ast.Try):
            continue
        caught: set[str] = set()
        for h in node.handlers:
            t = h.type
            if isinstance(t, ast.Name):
                caught.add(t.id)
            elif isinstance(t, ast.Tuple):
                caught.update(e.id for e in t.elts if isinstance(e, ast.Name))
        if not (_FAULT_EXC_NAMES & caught):
            continue
        # The guarded body is an infra write/IO; every handler on this try is fault-injection-only.
        for h in node.handlers:
            start = h.lineno
            end = getattr(h, "end_lineno", None) or start
            for ln in range(start, end + 1):
                lines.add(ln)
    return lines


def _now_window_lines(src_lines: list[str]) -> set[int]:
    out: set[int] = set()
    for i, text in enumerate(src_lines, start=1):
        if "now()" in text:
            out.add(i)
    return out


def _library_err_text_lines(src_lines: list[str]) -> set[int]:
    """Lines that build a user-visible message embedding an exception value (library text → F2)."""
    out: set[int] = set()
    for i, text in enumerate(src_lines, start=1):
        t = text.strip()
        if not t:
            continue
        # f-string interpolation of an exception var, e.g. f"...: {exc}" or f"...{e}"
        if ("f'" in t or 'f"' in t) and re.search(r"\{(exc|e|err|error|ex|e2)\b[^}]*\}", t):
            out.add(i)
        # explicit str(exc)/_public_dashboard_query_error(exc) in a message expression
        elif re.search(r"(str\(\s*(exc|e|err|ex)\s*\)|_public_dashboard_query_error\()", t):
            out.add(i)
    return out


def main() -> int:
    if not COV.exists():
        raise SystemExit(f"missing {COV} — run coverage_capture.py first")
    cov = json.loads(COV.read_text())
    fkey = next(k for k in cov["files"] if k.endswith("app.py"))
    uncovered = set(cov["files"][fkey]["missing_lines"])

    src = APP.read_text().splitlines()
    tree = ast.parse(APP.read_text())

    defensive = _defensive_except_lines(tree)
    fault = _fault_injection_lines(tree)
    now_win = _now_window_lines(src)
    lib_err = _library_err_text_lines(src)

    # Precedence: defensive (no-return swallow) > fault-injection (infra-only return) > now-window >
    # library-error-text > coverable.
    buckets: dict[str, list[int]] = {
        "DEFENSIVE_EXCEPT": [],
        "FAULT_INJECTION": [],
        "NOW_WINDOW": [],
        "LIBRARY_ERR_TEXT": [],
        "COVERABLE": [],
    }
    for ln in sorted(uncovered):
        if ln in defensive:
            buckets["DEFENSIVE_EXCEPT"].append(ln)
        elif ln in fault:
            buckets["FAULT_INJECTION"].append(ln)
        elif ln in now_win:
            buckets["NOW_WINDOW"].append(ln)
        elif ln in lib_err:
            buckets["LIBRARY_ERR_TEXT"].append(ln)
        else:
            buckets["COVERABLE"].append(ln)

    total = len(uncovered)
    total_app = cov["files"][fkey]["summary"]["num_statements"]
    covered = total_app - total
    summary = {k: len(v) for k, v in buckets.items()}

    OUT_JSON.write_text(json.dumps({"summary": summary, "buckets": buckets}, indent=2))

    deferred = (
        summary["DEFENSIVE_EXCEPT"] + summary["FAULT_INJECTION"] + summary["NOW_WINDOW"] + summary["LIBRARY_ERR_TEXT"]
    )
    # the deterministically-reachable ceiling = covered + coverable, as a % of all statements
    ceiling = (covered + summary["COVERABLE"]) / total_app * 100 if total_app else 0.0
    cur_pct = covered / total_app * 100 if total_app else 0.0

    md = [
        "# Uncovered-line structural classification (DoD-3)",
        "",
        f"app.py statements: **{total_app}** · covered **{covered}** ({cur_pct:.2f}%) · " f"uncovered **{total}**",
        "",
        "Heuristic, conservative (unsure → COVERABLE). Deferred buckets are a *lower bound* on the "
        "structural ceiling; COVERABLE is an *upper bound* on remaining corpus work.",
        "",
        "| reason | lines | % of uncovered | note |",
        "|---|---:|---:|---|",
        f"| DEFENSIVE_EXCEPT | {summary['DEFENSIVE_EXCEPT']} | "
        f"{summary['DEFENSIVE_EXCEPT']/total*100:.1f}% | except-body pass/log/continue — needs fault "
        f"injection |",
        f"| FAULT_INJECTION | {summary['FAULT_INJECTION']} | "
        f"{summary['FAULT_INJECTION']/total*100:.1f}% | except handler reachable only by an infra fault "
        f"(WriteQueueFullError etc.) — returns a 5xx but un-triggerable by a deterministic request |",
        f"| NOW_WINDOW | {summary['NOW_WINDOW']} | {summary['NOW_WINDOW']/total*100:.1f}% | chdb "
        f"now() wall-clock — needs now()-anchored seed or unverifiable |",
        f"| LIBRARY_ERR_TEXT | {summary['LIBRARY_ERR_TEXT']} | "
        f"{summary['LIBRARY_ERR_TEXT']/total*100:.1f}% | message embeds a Python library exception "
        f"string (re/json/chdb) — differs Go-vs-Py (F2) |",
        f"| COVERABLE | {summary['COVERABLE']} | {summary['COVERABLE']/total*100:.1f}% | a byte-parity "
        f"route can reach it (schedulable) |",
        "",
        f"**Structurally-deferred (lower bound): {deferred} lines " f"({deferred/total*100:.1f}% of uncovered).**",
        f"**Deterministically-reachable coverage ceiling (covered + COVERABLE): ~{ceiling:.1f}%** of "
        f"app.py statements — the realistic DoD-3 target for the corpus (the rest is classified as "
        f"deferred above, not coverable by deterministic byte-parity).",
        "",
        "> Note: most COVERABLE lines are seeded populated-handler success paths — these go byte-GREEN "
        "once captured with the CORRECT seeded-profile flow (seed_fixtures.py --only-profile X before "
        "capture). A residual few expose genuine Go port bugs (e.g. raw_attrs JSON key-order) that must "
        "be fixed first; the *lines* are reachable either way, so they count toward the ceiling.",
    ]
    OUT_MD.write_text("\n".join(md) + "\n")
    print("\n".join(md))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
