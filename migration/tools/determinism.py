"""Freeze every entropy source so the Python app emits stable, reproducible bytes.

IMPORTANT — install AFTER importing the app, not before:

    import app                       # import heavy C extensions (pandas/numpy/pyarrow)
    import determinism               # with the REAL clock
    determinism.install()            # now freeze time/uuid/random for serving+capture

Why after, not before: freezing the clock and replacing datetime.datetime with a
subclass BEFORE importing pandas/numpy hangs their C-extension import (timing
calibration / datetime use at import). Output timestamps are produced at REQUEST time
(datetime.now() in handlers), so freezing post-import still yields byte-stable output.
install() sweeps sys.modules to rebind `datetime` in every module that did
`from datetime import datetime`, so app.py/mcp.py/masking.py all see the frozen class.

The Go app mirrors these exact values when SOBS_PARITY=1 (see go/internal/clock,
go/internal/idgen). The fixed instant and UUID/byte sequences are part of the parity
contract — change them and you must re-capture the whole golden corpus AND update the
Go mirror.

Pure stdlib. No third-party deps.
"""

from __future__ import annotations

import datetime as _dt
import os
import sys
import time as _time
import uuid as _uuid

# ---- The frozen constants (the contract) -----------------------------------------
FIXED_EPOCH = 1704164645.0  # 2024-01-02T03:04:05Z
FIXED_DT_UTC = _dt.datetime(2024, 1, 2, 3, 4, 5, 0, tzinfo=_dt.timezone.utc)
UUID_SEED = 0
BYTE_SEED = b"sobs-parity-seed"

_REAL_DATETIME = _dt.datetime  # captured before patching, for the sys.modules sweep
_installed = False


def install() -> None:
    """Freeze time/datetime/uuid/random. Call AFTER the app (and pandas) are imported."""
    global _installed
    if _installed:
        return
    _installed = True
    _freeze_time()
    _freeze_datetime()
    _freeze_uuid()
    _freeze_random_bytes()


# ---- time -------------------------------------------------------------------------
def _freeze_time() -> None:
    _time.time = lambda: FIXED_EPOCH  # type: ignore[assignment]
    _time.time_ns = lambda: int(FIXED_EPOCH * 1_000_000_000)  # type: ignore[assignment]
    # perf_counter/monotonic feed latency fields that render into output (e.g.
    # /health/db latency_ms). Freeze to a constant so any elapsed == 0.0.
    _time.perf_counter = lambda: 0.0  # type: ignore[assignment]
    _time.monotonic = lambda: 0.0  # type: ignore[assignment]


def _freeze_datetime() -> None:
    class _FrozenDateTime(_REAL_DATETIME):  # type: ignore[misc, valid-type]
        @classmethod
        def now(cls, tz=None):  # noqa: D401
            return FIXED_DT_UTC.astimezone(tz) if tz else FIXED_DT_UTC.replace(tzinfo=None)

        @classmethod
        def utcnow(cls):
            return FIXED_DT_UTC.replace(tzinfo=None)

        @classmethod
        def today(cls):
            return FIXED_DT_UTC.replace(tzinfo=None)

    _dt.datetime = _FrozenDateTime  # type: ignore[misc]
    # Sweep every already-imported module that bound the real datetime class by name
    # (`from datetime import datetime`) and rebind it to the frozen subclass.
    for mod in list(sys.modules.values()):
        try:
            if getattr(mod, "datetime", None) is _REAL_DATETIME:
                mod.datetime = _FrozenDateTime  # type: ignore[attr-defined]
        except Exception:
            continue


def patch_module(mod) -> None:
    """Back-compat no-op-ish: ensure a specific module sees the frozen datetime."""
    if getattr(mod, "datetime", None) is _REAL_DATETIME:
        mod.datetime = _dt.datetime  # type: ignore[attr-defined]


# ---- uuid -------------------------------------------------------------------------
def _freeze_uuid() -> None:
    counter = {"n": UUID_SEED}

    def _uuid4():
        n = counter["n"]
        counter["n"] += 1
        # deterministic 128-bit value from the counter, shaped like a v4 UUID
        b = bytearray(n.to_bytes(16, "big"))
        b[6] = (b[6] & 0x0F) | 0x40  # version 4
        b[8] = (b[8] & 0x3F) | 0x80  # variant
        return _uuid.UUID(bytes=bytes(b))

    _uuid.uuid4 = _uuid4  # type: ignore[assignment]


# ---- random bytes / tokens --------------------------------------------------------
def _freeze_random_bytes() -> None:
    import base64
    import hashlib
    import secrets as _secrets

    state = {"ctr": 0}

    def _stream(n: int) -> bytes:
        out = bytearray()
        while len(out) < n:
            block = hashlib.sha256(BYTE_SEED + state["ctr"].to_bytes(8, "big")).digest()
            state["ctr"] += 1
            out.extend(block)
        return bytes(out[:n])

    def _token_bytes(n: int | None = 32) -> bytes:
        return _stream(n if n is not None else 32)

    def _token_hex(n: int | None = 32) -> str:
        return _token_bytes(n).hex()

    def _token_urlsafe(n: int | None = 32) -> str:
        return base64.urlsafe_b64encode(_token_bytes(n)).rstrip(b"=").decode()

    os.urandom = _stream  # type: ignore[assignment]
    _secrets.token_bytes = _token_bytes  # type: ignore[assignment]
    _secrets.token_hex = _token_hex  # type: ignore[assignment]
    _secrets.token_urlsafe = _token_urlsafe  # type: ignore[assignment]


if __name__ == "__main__":
    install()
    print("frozen time:", _time.time(), _dt.datetime.now())
    print("frozen uuids:", _uuid.uuid4(), _uuid.uuid4())
    print("frozen urandom:", os.urandom(8).hex())
