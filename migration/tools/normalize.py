"""The minimal, explicit normalization allow-list for parity comparison.

PHILOSOPHY: every rule here is a hole in the byte-for-byte guarantee. Keep it tiny.
Bodies are NEVER normalized. Only three header concerns are normalized, and only
because they reflect the HTTP server identity / wall clock, not application output.

If a route only passes because you added a rule here, the Go port is wrong — revert
the rule and fix the port. Changing this file requires updating PARITY_STRATEGY.md §1.

A "response" is the dict: {"status": int, "headers": [[name, value], ...], "body": bytes}

Header comparison is ORDER-INDEPENDENT (a case-insensitive sorted multiset of
(name, value) pairs). Rationale: cross-name header order is not semantically significant
in HTTP, and the two server stacks legitimately differ — Quart/Hypercorn preserves
insertion order, Go's net/http emits sorted. Header *presence and values* are still
compared exactly (minus the value allow-list below). The BODY — the rendered asset the
migration is about — is compared raw, byte-for-byte, never reordered or normalized.
"""

from __future__ import annotations

import re

# Transport / server-identity headers: dropped entirely from BOTH sides before compare.
# These are artifacts of the HTTP transport (real net/http server vs Quart test client),
# never application output. Dropping (not blanking) avoids false presence-asymmetry
# failures: the test client emits no Date/Connection; a live net/http server does.
# The BODY is never touched, so rendered-asset parity is unaffected.
_DROP_HEADERS = {"date", "server", "connection", "transfer-encoding", "keep-alive"}

# Wall-clock / filesystem-mtime caching metadata. last-modified and expires derive from
# the file mtime and the wall clock; they are NEVER reproducible across a frozen-clock
# Python capture and a live Go server (git does not preserve mtimes either), so they are
# ALWAYS dropped — they are caching metadata, not the rendered asset (the JS/CSS/JSON
# bytes, which ARE compared byte-for-byte). The etag is handled separately: Werkzeug's
# filesystem etag `{mtime}-{size}-{adler32}` is mtime-derived and dropped, but app-set
# content-hash ETags (e.g. on /static/rum.js, sha256 of the bytes) ARE deterministic and
# are KEPT and compared. Documented in PARITY_STRATEGY.md §1.
_ALWAYS_DROP_CACHE_HEADERS = {"last-modified", "expires"}

# The signed session cookie: HMAC signature differs by signing implementation, but the
# PAYLOAD must match. We compare the unsigned payload, not the signature bytes.
_SESSION_COOKIE_RE = re.compile(rb"(sobs_session=)([^;]*)(.*)", re.IGNORECASE)


# Werkzeug filesystem-static etag: "{mtime_float}-{size}-{adler32}". When a response
# carries one of these, it's a filesystem-static file and its mtime/clock caching headers
# are dropped (see _STATIC_CACHE_HEADERS).
_FS_ETAG_RE = re.compile(r'^"?\d+\.\d+-\d+-\d+"?$')


def normalize(resp: dict) -> dict:
    # A bodyless status (1xx/204/304) must not carry a message body (RFC 7230 §3.3.2), so any
    # Content-Length on it is a transport artifact, not application output: Quart/Hypercorn emits
    # "Content-Length: 0", Go's net/http omits it (it refuses to write Content-Length for these
    # statuses). Drop it on BOTH sides for these statuses ONLY; every body-bearing status still
    # compares Content-Length exactly. Same category as the _DROP_HEADERS transport artifacts.
    status = resp["status"]
    bodyless = status in (204, 304) or (100 <= status < 200)
    headers = []
    for name, value in resp["headers"]:
        lname = name.lower()
        if lname in _DROP_HEADERS:
            continue
        if lname == "content-length" and bodyless:
            continue
        if lname in _ALWAYS_DROP_CACHE_HEADERS:
            continue
        # Drop ONLY the mtime-format Werkzeug etag; keep content-hash etags for comparison.
        if lname == "etag" and _FS_ETAG_RE.match(value.strip()):
            continue
        if lname == "set-cookie" and b"sobs_session=" in value.encode("latin1", "ignore"):
            headers.append([name, _normalize_session_cookie(value)])
            continue
        # The 405 `Allow` header lists methods in a set-iteration order that Werkzeug does NOT
        # fix (it flips between "POST, OPTIONS" and "OPTIONS, POST" across captures). The method
        # SET is order-irrelevant, so sort it on both sides before comparing.
        if lname == "allow":
            methods = sorted(p.strip() for p in value.split(",") if p.strip())
            headers.append([name, ", ".join(methods)])
            continue
        headers.append([name, value])
    return {"status": resp["status"], "headers": headers, "body": resp["body"]}


def _normalize_session_cookie(value: str) -> str:
    # Quart secure cookie = payload.timestamp.signature, where payload is base64(json) or,
    # when itsdangerous decided zlib shrinks it, ".".base64(zlib(json)). We DECODE the payload
    # to the session dict and emit a canonical sorted JSON, then blank timestamp+signature.
    # Comparing the decoded dict (not the raw base64) makes the result independent of the
    # compress/no-compress decision — Go's stdlib zlib is a few bytes larger than CPython's, so
    # the two disagree at the threshold; the underlying flash payload is identical either way.
    raw = value.encode("latin1", "ignore")
    m = _SESSION_COOKIE_RE.match(raw)
    if not m:
        return value
    cookie_val = m.group(2).decode("latin1")
    rest = m.group(3).decode("latin1")
    segments = cookie_val.split(".")
    # The last two segments are the timestamp and signature; the rest is the payload (which is
    # itself ".".join-able for the compressed ".{b64}" form).
    payload = ".".join(segments[:-2]) if len(segments) >= 3 else segments[0]
    canonical = _decode_session_payload(payload)
    if canonical is None:
        # Unknown format: fall back to the raw payload segment (signature blanked).
        return f"sobs_session={payload}.<sig>{rest}"
    return f"sobs_session={canonical}.<sig>{rest}"


def _decode_session_payload(payload: str) -> str | None:
    import base64
    import json
    import zlib

    try:
        compressed = payload.startswith(".")
        b64 = payload[1:] if compressed else payload
        data = base64.urlsafe_b64decode(b64 + "=" * (-len(b64) % 4))
        if compressed:
            data = zlib.decompress(data)
        session = json.loads(data)
        return json.dumps(session, sort_keys=True, separators=(",", ":"))
    except Exception:
        return None


def _header_multiset(headers: list) -> list:
    # case-insensitive name, exact value, order-independent
    return sorted((name.lower(), value) for name, value in headers)


def equal(a: dict, b: dict) -> bool:
    na, nb = normalize(a), normalize(b)
    return (
        na["status"] == nb["status"]
        and _header_multiset(na["headers"]) == _header_multiset(nb["headers"])
        and na["body"] == nb["body"]
    )


def first_body_diff(a: bytes, b: bytes, window: int = 120):
    """Return (offset, a_window, b_window) of the first differing byte, or None."""
    n = min(len(a), len(b))
    for i in range(n):
        if a[i] != b[i]:
            lo = max(0, i - window // 2)
            return i, a[lo : i + window // 2], b[lo : i + window // 2]
    if len(a) != len(b):
        i = n
        return i, a[max(0, i - window) :], b[max(0, i - window) :]
    return None
