"""masking.py – Shared PII/secret masking rules for SOBS UI output,
notification messages, and GitHub issue bodies.

All patterns are explicit and human-curated (no ML/heuristic detection).

Extending the rule set
----------------------
* **New sensitive value formats** – add a regex string to ``SENSITIVE_PATTERNS``.
  Each pattern is applied via ``re.sub(pattern, MASK, text)`` to every string
  value encountered (recursively in dicts/lists).  The entire match is replaced
  with ``MASK``, so keep patterns tight to avoid destroying unrelated context.

* **New sensitive key names** – add a lowercase key name to ``SENSITIVE_KEYS``.
  Any dict key whose *lowercased* name is in this set will have its value
  replaced with ``MASK``, regardless of the value itself.

After modifying either collection at runtime call ``build_redacting_filter()``
to rebuild the singleton filter instance and pick up the changes.
"""

from __future__ import annotations

import copy
import json
import re
from collections.abc import Mapping
from typing import Any

from loggingredactor import RedactingFilter

# ---------------------------------------------------------------------------
# Replacement placeholder shown in masked output
# ---------------------------------------------------------------------------
MASK: str = "****"

# ---------------------------------------------------------------------------
# Key names whose values should always be fully masked.
# Comparison is done after lowercasing the actual key at call-site.
# ---------------------------------------------------------------------------
DEFAULT_SENSITIVE_KEYS: frozenset[str] = frozenset(
    {
        # Credentials / secrets
        "password",
        "passwd",
        "pwd",
        "secret",
        "client_secret",
        "api_key",
        "api_secret",
        "apikey",
        # Tokens
        "token",
        "access_token",
        "refresh_token",
        "id_token",
        "auth_token",
        "bearer_token",
        # Auth headers
        "authorization",
        "x-authorization",
        "x-api-key",
        # Cryptographic material
        "private_key",
        "private-key",
        # Payment / identity
        "credit_card",
        "card_number",
        "cvv",
        "cvc",
        "ssn",
        "social_security_number",
        # SOBS-specific sensitive settings keys
        "s3_secret_access_key",
        "backup_encryption_password",
        "smtp_password",
    }
)

SENSITIVE_KEYS: frozenset[str] = DEFAULT_SENSITIVE_KEYS

# ---------------------------------------------------------------------------
# Regex patterns matched against string values.
# The *entire* match is replaced with MASK, so patterns should capture the
# full sensitive fragment (not just a capture group inside it).
# ---------------------------------------------------------------------------
DEFAULT_SENSITIVE_PATTERNS: list[str] = [
    # --- Email addresses ---
    r"\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b",
    # --- JWT tokens (three base64url-encoded segments) ---
    r"\beyJ[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]*\b",
    # --- Bearer token in text / HTTP headers ---
    r"(?i)bearer\s+[A-Za-z0-9\-_.~+/]+=*",
    # --- AWS access key IDs ---
    r"\bAKIA[0-9A-Z]{16}\b",
    # --- US Social Security Numbers (###-##-####) ---
    r"\b\d{3}-\d{2}-\d{4}\b",
    # --- Common credit card patterns ---
    r"\b4[0-9]{12}(?:[0-9]{3})?\b",  # Visa (13 or 16 digits)
    r"\b5[1-5][0-9]{14}\b",  # Mastercard
    r"\b3[47][0-9]{13}\b",  # Amex
    r"\b6(?:011|5[0-9]{2})[0-9]{12}\b",  # Discover
    # --- PEM private key blocks ---
    (
        r"-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----[\s\S]+?"
        r"-----END (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----"
    ),
    # --- Generic key=value / key: value assignment in log lines ---
    # Matches patterns like: password=abc123 | secret: "xyz" | api_key=ABCDEF...
    r"(?i)(?:password|passwd|pwd|secret|api[_\-]?key|auth[_\-]?token|access[_\-]?token)"
    r"\s*[=:]\s*['\"]?[A-Za-z0-9\-_.~+/!@#$%^&*]{6,}['\"]?",
    # --- Authorization header value ---
    r"(?i)(?:Authorization|X-Api-Key|X-Auth-Token)\s*:\s*[^\r\n]+",
]

SENSITIVE_PATTERNS: list[str] = list(DEFAULT_SENSITIVE_PATTERNS)

# ---------------------------------------------------------------------------
# Custom filter with case-insensitive key matching
# ---------------------------------------------------------------------------


class _SobsRedactingFilter(RedactingFilter):
    """Extends :class:`RedactingFilter` with case-insensitive dict-key matching.

    The upstream library compares key names verbatim; this subclass converts
    each key to lowercase before checking membership in ``_mask_keys``.
    """

    def redact(self, content: Any, key: Any = None) -> Any:  # type: ignore[override]
        return self._redact_value(content, key=key, visited=set())

    def _redact_value(self, content: Any, *, key: Any = None, visited: set[int]) -> Any:
        if key is not None and str(key).lower() in self._mask_keys:
            return self._mask

        if content is None:
            return None
        if isinstance(content, str):
            masked_text = content
            for pattern in self._mask_patterns:
                masked_text = re.sub(pattern, self._mask, masked_text, flags=re.DOTALL)
            return masked_text
        if isinstance(content, (bool, int, float)):
            return content

        if isinstance(content, Mapping):
            object_id = id(content)
            if object_id in visited:
                return self._mask
            visited.add(object_id)
            try:
                return {
                    item_key: self._redact_value(item_value, key=item_key, visited=visited)
                    for item_key, item_value in content.items()
                }
            finally:
                visited.remove(object_id)

        if isinstance(content, list):
            object_id = id(content)
            if object_id in visited:
                return self._mask
            visited.add(object_id)
            try:
                return [self._redact_value(item, visited=visited) for item in content]
            finally:
                visited.remove(object_id)

        if isinstance(content, tuple):
            object_id = id(content)
            if object_id in visited:
                return self._mask
            visited.add(object_id)
            try:
                return tuple(self._redact_value(item, visited=visited) for item in content)
            finally:
                visited.remove(object_id)

        if isinstance(content, (set, frozenset)):
            object_id = id(content)
            if object_id in visited:
                return self._mask
            visited.add(object_id)
            try:
                return type(content)(self._redact_value(item, visited=visited) for item in content)
            finally:
                visited.remove(object_id)

        try:
            content_copy = copy.deepcopy(content)
        except Exception:
            return self._mask

        if content_copy is content:
            return self._mask
        return self._redact_value(content_copy, visited=visited)


# ---------------------------------------------------------------------------
# Internal singleton filter – rebuilt by build_redacting_filter()
# ---------------------------------------------------------------------------
_filter: _SobsRedactingFilter | None = None


def normalize_sensitive_key(value: Any) -> str:
    """Return a normalized lowercase key name for key-based masking."""
    return str(value or "").strip().lower()


def validate_pattern(pattern: Any) -> str:
    """Validate and normalize a custom regex pattern."""
    normalized = str(pattern or "").strip()
    if not normalized:
        raise ValueError("Pattern is required")
    re.compile(normalized, re.DOTALL)
    return normalized


def _dedupe_preserve_order(values: list[str]) -> list[str]:
    seen: set[str] = set()
    result: list[str] = []
    for value in values:
        if value in seen:
            continue
        seen.add(value)
        result.append(value)
    return result


def configure_runtime_rules(
    custom_keys: list[str] | tuple[str, ...] | None = None,
    custom_patterns: list[str] | tuple[str, ...] | None = None,
) -> _SobsRedactingFilter:
    """Merge persisted custom rules with defaults and rebuild the filter."""
    global SENSITIVE_KEYS, SENSITIVE_PATTERNS

    normalized_keys = sorted({key for key in (normalize_sensitive_key(item) for item in (custom_keys or [])) if key})
    normalized_patterns = _dedupe_preserve_order([validate_pattern(item) for item in (custom_patterns or [])])

    SENSITIVE_KEYS = frozenset({*DEFAULT_SENSITIVE_KEYS, *normalized_keys})
    SENSITIVE_PATTERNS = [*DEFAULT_SENSITIVE_PATTERNS, *normalized_patterns]
    return build_redacting_filter()


def build_redacting_filter() -> _SobsRedactingFilter:
    """(Re)build and return the shared :class:`_SobsRedactingFilter` instance.

    Call this function after modifying :data:`SENSITIVE_KEYS` or
    :data:`SENSITIVE_PATTERNS` at runtime to ensure the changes take effect.
    """
    global _filter
    _filter = _SobsRedactingFilter(
        mask_patterns=SENSITIVE_PATTERNS,
        mask=MASK,
        mask_keys=SENSITIVE_KEYS,
    )
    return _filter


def _get_filter() -> _SobsRedactingFilter:
    if _filter is None:
        build_redacting_filter()
    return _filter  # type: ignore[return-value]


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------


def mask_value(value: Any) -> Any:
    """Mask sensitive data in *value* and return the same type.

    Suitable for use as a Jinja2 template filter (``{{ value|mask }}``) or
    for pre-processing any observable data before rendering.

    * Strings have all :data:`SENSITIVE_PATTERNS` applied.
    * Dicts/lists are traversed recursively; keys in :data:`SENSITIVE_KEYS`
      have their values replaced with ``"****"``.
    * ``None`` is returned as-is; numeric/bool values pass through unchanged.

    This function is **non-mutating**: original containers are not modified.
    """
    if value is None:
        return value
    return _get_filter().redact(value)


def mask_string(value: Any) -> str:
    """Mask sensitive data and coerce the result to :class:`str`.

    Use this variant when a plain string is required—e.g. for notification
    summary messages or GitHub issue title/body text before sending.
    """
    if value is None:
        return ""
    # For non-string types apply recursive key-level masking first, then
    # serialise to a string and apply pattern-level masking in one final pass.
    if not isinstance(value, str):
        value = mask_value(value)  # recursive key masking on dicts/lists
        try:
            value = json.dumps(value, ensure_ascii=False, default=str)
        except Exception:
            value = str(value)
    result = _get_filter().redact(value)
    return str(result) if result is not None else ""


# Initialise the singleton at module import time.
configure_runtime_rules()
