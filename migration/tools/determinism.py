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
    _install_upstream_fixtures()


# ---- upstream HTTP fixtures (GitHub / OSV) -----------------------------------------
def upstream_fixture_key(method: str, url: str) -> str:
    """Deterministic filename stem for a canned upstream response. MUST match the Go side
    (go/cmd/sobs/upstream.go upstreamFixtureKey): sha256 of "METHOD url", first 32 hex."""
    import hashlib

    return hashlib.sha256(f"{method.upper()} {url}".encode("utf-8")).hexdigest()[:32]


def _install_upstream_fixtures() -> None:
    """Serve api.github.com / api.osv.dev from canned files instead of the network, so the
    external-integration routes are byte-reproducible. Both the Python oracle (this httpx
    MockTransport) and the Go server (upstream.go) read the SAME files keyed by request URL,
    so neither side touches the network and both build identical route responses.

    Activated only when SOBS_UPSTREAM_FIXTURES points at the canned-response directory.
    A request with no matching fixture returns 404 (matching a not-found GitHub/OSV lookup).
    """
    fixtures_dir = os.environ.get("SOBS_UPSTREAM_FIXTURES", "").strip()
    if not fixtures_dir:
        return
    import json as _json
    from pathlib import Path as _Path

    import httpx  # app.py already imports httpx, so it is importable here

    base = _Path(fixtures_dir)
    # api.github.com / api.osv.dev are the real upstreams; hooks.example.com is the parity test
    # webhook sink; sobs-ai.mock is the LLM /chat/completions endpoint (each AI route's profile
    # points at a distinct path so the URL-keyed canned response is per-route). All canned.
    intercept_hosts = {"api.github.com", "api.osv.dev", "hooks.example.com", "sobs-ai.mock"}

    def _handler(request: "httpx.Request") -> "httpx.Response":
        host = request.url.host
        if host not in intercept_hosts:
            return httpx.Response(599, json={"error": f"unmocked upstream host {host}"})
        stem = upstream_fixture_key(request.method, str(request.url))
        path = base / f"{stem}.json"
        if not path.exists():
            return httpx.Response(404, json={"message": "Not Found (no upstream fixture)"})
        spec = _json.loads(path.read_text())
        status = int(spec.get("status", 200))
        if "json" in spec:
            return httpx.Response(status, json=spec["json"])
        return httpx.Response(status, content=str(spec.get("content", "")).encode("utf-8"))

    _orig_init = httpx.AsyncClient.__init__

    def _patched_init(self, *args, **kwargs):  # type: ignore[no-untyped-def]
        kwargs["transport"] = httpx.MockTransport(_handler)
        _orig_init(self, *args, **kwargs)

    httpx.AsyncClient.__init__ = _patched_init  # type: ignore[assignment]


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
