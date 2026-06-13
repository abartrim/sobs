#!/usr/bin/env python3
"""Phase 0 gate (Python side): write a chdb directory that the Go test re-opens.

Pairs with go/internal/store/gate0_roundtrip_test.go. Proves the on-disk chdb storage
format round-trips between Python chdb 4.1.9 and Go chdb-go (pinned chdb-core). If this
pair fails, the hard-cutover strategy of sharing one directory is not viable — STOP and
revisit before porting anything (see PHASES.md Phase 0).

Sequence:
  1. python migration/tools/gate0_chdb.py write   # Python creates + populates the dir
  2. go test ./go/internal/store -run TestGate0RoundTrip   # Go reads, asserts, writes
  3. python migration/tools/gate0_chdb.py verify  # Python re-opens, asserts Go's writes

The directory is migration/fixtures/_gate0.chdb (gitignored).
"""

from __future__ import annotations

import sys
from pathlib import Path

GATE_DIR = Path(__file__).resolve().parents[2] / "migration" / "fixtures" / "_gate0.chdb"

DDL = """
CREATE TABLE IF NOT EXISTS gate0 (
    Id UInt64,
    Name String,
    Version UInt64,
    IsDeleted UInt8
) ENGINE = ReplacingMergeTree(Version) ORDER BY Id;
"""

PY_ROWS = [(1, "alpha"), (2, "beta"), (3, "gamma")]


def _connect():
    import chdb.session as session  # chdb 4.x session API

    return session.Session(str(GATE_DIR))


def write() -> int:
    GATE_DIR.parent.mkdir(parents=True, exist_ok=True)
    sess = _connect()
    sess.query(DDL)
    values = ",".join(f"({i},'{n}',1,0)" for i, n in PY_ROWS)
    sess.query(f"INSERT INTO gate0 (Id,Name,Version,IsDeleted) VALUES {values}")
    sess.query("OPTIMIZE TABLE gate0 FINAL")
    out = sess.query("SELECT Id,Name FROM gate0 FINAL ORDER BY Id", "CSV")
    print("python wrote+read:\n" + str(out))
    sess.close()
    return 0


def verify() -> int:
    sess = _connect()
    out = sess.query("SELECT Id,Name FROM gate0 FINAL ORDER BY Id", "CSV")
    text = str(out)
    print("python re-open after Go writes:\n" + text)
    # Expect Python's 3 rows + the 2 rows the Go test inserts (Id 4,5).
    ok = all(f"{i}," in text for i in (1, 2, 3, 4, 5))
    print("GATE0 PYTHON VERIFY:", "PASS" if ok else "FAIL")
    sess.close()
    return 0 if ok else 1


if __name__ == "__main__":
    cmd = sys.argv[1] if len(sys.argv) > 1 else "write"
    raise SystemExit(write() if cmd == "write" else verify())
