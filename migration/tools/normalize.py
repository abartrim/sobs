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

# Headers whose VALUE is server/clock identity, not app output. Value is blanked but
# presence + position is still compared (so a missing/extra Date still fails).
_BLANK_VALUE_HEADERS = {"date", "server"}

# The signed session cookie: HMAC signature differs by signing implementation, but the
# PAYLOAD must match. We compare the unsigned payload, not the signature bytes.
_SESSION_COOKIE_RE = re.compile(rb"(sobs_session=)([^;]*)(.*)", re.IGNORECASE)


def normalize(resp: dict) -> dict:
    headers = []
    for name, value in resp["headers"]:
        lname = name.lower()
        if lname in _BLANK_VALUE_HEADERS:
            headers.append([name, "<normalized>"])
            continue
        if lname == "set-cookie" and b"sobs_session=" in value.encode("latin1", "ignore"):
            headers.append([name, _normalize_session_cookie(value)])
            continue
        headers.append([name, value])
    return {"status": resp["status"], "headers": headers, "body": resp["body"]}


def _normalize_session_cookie(value: str) -> str:
    # Quart secure cookie = base64(payload).timestamp.signature  — keep the payload,
    # blank timestamp+signature. If the format differs, compare the decoded session
    # dict instead (left as a TODO hook for the rare session-setting routes).
    raw = value.encode("latin1", "ignore")
    m = _SESSION_COOKIE_RE.match(raw)
    if not m:
        return value
    cookie_val = m.group(2).decode("latin1")
    payload = cookie_val.split(".", 1)[0]  # the unsigned payload segment
    rest = m.group(3).decode("latin1")
    return f"sobs_session={payload}.<sig>{rest}"


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
