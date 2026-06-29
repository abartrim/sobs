#!/usr/bin/env python3
"""Emit Go *integration* coverage from a GOCOVERDIR — the SAME instrumented byte-parity replay.

When the parity replay is built with `SOBS_GOCOVER=1` (parity_check adds `-cover -coverpkg=./...`)
and `GOCOVERDIR` is set, every per-profile Go server flushes its counters into that dir on the
harness's SIGTERM (see `go/cmd/sobs/main.go`). This script merges those counters and writes, next to
the goldens, the artifacts CI publishes so the coverage grind never works off a stale local profile:

  migration/go_corpus_cover.txt   — merged textfmt profile (the corpus lens)
  migration/go_corpus_0pct.txt    — the actionable backlog: non-protobuf funcs at 0.0% (file histogram
                                    + full list), with the overall covdata percent on the header line

It is deliberately tolerant: if no counters were produced (e.g. a cover build wasn't used) it writes
empty-but-valid artifacts and exits 0, so it is NEVER a new way for the parity gate to fail. Runs from
the repo root inside the parity image (Go toolchain + source present); also usable locally on a
GOCOVERDIR pulled from CI.
"""

from __future__ import annotations

import os
import re
import subprocess
from collections import Counter
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
GO = REPO / "go"
GOCOVERDIR = os.environ.get("GOCOVERDIR") or str(REPO / "migration" / ".gocov")
OUT_PROFILE = REPO / "migration" / "go_corpus_cover.txt"
OUT_REPORT = REPO / "migration" / "go_corpus_0pct.txt"


def _go(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(["go", *args], cwd=str(GO), text=True, capture_output=True)


def main() -> int:
    counters = list(Path(GOCOVERDIR).glob("covcounters.*")) if Path(GOCOVERDIR).is_dir() else []
    if not counters:
        OUT_PROFILE.write_text("mode: set\n")
        OUT_REPORT.write_text("(no Go coverage counters in GOCOVERDIR — was SOBS_GOCOVER=1 set?)\n")
        print(f"WARN: no covcounters in {GOCOVERDIR}; wrote empty artifacts", flush=True)
        return 0

    txt = _go("tool", "covdata", "textfmt", f"-i={GOCOVERDIR}", "-o", str(OUT_PROFILE))
    if txt.returncode != 0 or not OUT_PROFILE.exists():
        OUT_PROFILE.write_text("mode: set\n")
        OUT_REPORT.write_text(f"(covdata textfmt failed: {txt.stderr.strip()[:300]})\n")
        print("WARN: covdata textfmt failed:", txt.stderr[:300], flush=True)
        return 0

    # Overall % comes from the `cover -func` total line (covdata percent prints per-package only).
    func = _go("tool", "cover", f"-func={OUT_PROFILE}")
    overall = ""
    zero: list[tuple[str, str]] = []
    for line in func.stdout.splitlines():
        parts = re.split(r"\t+", line.strip())
        if line.startswith("total:"):
            overall = parts[-1]
            continue
        if len(parts) < 3:
            continue
        if parts[-1] == "0.0%" and ".pb.go" not in parts[0]:
            short = re.sub(r".*/(cmd/sobs|internal/[^/]+)/", "", parts[0])
            zero.append((short.split(":")[0], parts[1]))

    hist = Counter(f for f, _ in zero)
    lines = [
        f"# Go corpus coverage {overall or '(n/a)'} (all-Go incl. generated) — {len(zero)} non-protobuf "
        f"funcs at 0.0% (corpus-unreachable; the unit-test grind backlog. Some are already UNIT-tested — "
        f"verify each with `grep -rl '\\bNAME\\b' go/**/*_test.go` before targeting).",
        "",
        "## by file",
    ]
    lines += [f"{n:3}  {f}" for f, n in hist.most_common()]
    lines += ["", "## full list (file  func)"]
    lines += [f"{f}  {fn}" for f, fn in sorted(zero)]
    OUT_REPORT.write_text("\n".join(lines) + "\n")
    print(f"Wrote {OUT_REPORT.name}: {len(zero)} corpus-0% non-protobuf funcs (overall {overall})", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
