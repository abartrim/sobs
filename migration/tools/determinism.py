"""Freeze every entropy source so the Python app emits stable, reproducible bytes.

MUST be imported *before* `app` is imported (so module-level timestamps freeze too):

    import migration.tools.determinism as determinism
    determinism.install()
    import app   # now frozen

The Go app mirrors these exact values when SOBS_PARITY=1 (see go/internal/clock,
go/internal/idgen). The fixed instant and UUID/byte sequences are part of the parity
contract — change them and you must re-capture the whole golden corpus AND update the
Go mirror.

Pure stdlib. No third-party deps.
"""

from __future__ import annotations

import datetime as _dt
import os
import time as _time
import uuid as _uuid

# ---- The frozen constants (the contract) -----------------------------------------
FIXED_EPOCH = 1704164645.0  # 2024-01-02T03:04:05Z
FIXED_DT_UTC = _dt.datetime(2024, 1, 2, 3, 4, 5, 0, tzinfo=_dt.timezone.utc)
UUID_SEED = 0
BYTE_SEED = b"sobs-parity-seed"

_installed = False


def install() -> None:
    global _installed
    if _installed:
        return
    _installed = True
    _freeze_time()
    _freeze_uuid()
    _freeze_random_bytes()


# ---- time -------------------------------------------------------------------------
def _freeze_time() -> None:
    _time.time = lambda: FIXED_EPOCH  # type: ignore[assignment]
    _time.time_ns = lambda: int(FIXED_EPOCH * 1_000_000_000)  # type: ignore[assignment]

    class _FrozenDateTime(_dt.datetime):
        @classmethod
        def now(cls, tz=None):  # noqa: D401
            return FIXED_DT_UTC.astimezone(tz) if tz else FIXED_DT_UTC.replace(tzinfo=None)

        @classmethod
        def utcnow(cls):
            return FIXED_DT_UTC.replace(tzinfo=None)

        @classmethod
        def today(cls):
            return FIXED_DT_UTC.replace(tzinfo=None)

    # Patch the datetime class app code references. NOTE: app.py does
    # `from datetime import datetime` — patch there post-import too via patch_module().
    _dt.datetime = _FrozenDateTime  # type: ignore[misc]


def patch_module(mod) -> None:
    """Re-bind frozen names inside a module that did `from datetime import datetime`."""
    if hasattr(mod, "datetime"):
        mod.datetime = _dt.datetime
    if hasattr(mod, "time") and isinstance(getattr(mod, "time"), type(_time)):
        pass  # module imported the `time` module object; already patched in place


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
