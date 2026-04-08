#!/usr/bin/env python3
"""Shared helpers for repeatable AI prompt runner scripts in scripts/."""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any


@dataclass
class ApiCallResult:
    status: int
    body: dict[str, object]


def post_json(endpoint: str, path: str, payload: dict[str, Any], timeout: int) -> ApiCallResult:
    """POST JSON to an endpoint path and return parsed JSON body.

    Returns a synthetic error body on connection or parse errors so callers can
    keep iterating through prompt suites.
    """
    url = endpoint.rstrip("/") + path
    req = urllib.request.Request(
        url,
        data=json.dumps(payload).encode("utf-8"),
        headers={"Content-Type": "application/json"},
    )

    status = 200
    raw = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status = int(resp.status)
            raw = resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as exc:
        status = int(exc.code)
        raw = exc.read().decode("utf-8", "replace")
    except urllib.error.URLError as exc:
        return ApiCallResult(
            status=0,
            body={
                "ok": False,
                "error": f"connection error: {exc}",
            },
        )

    try:
        parsed = json.loads(raw)
    except Exception:
        parsed = {"ok": False, "error": f"non-json response: {raw[:300]}"}

    if not isinstance(parsed, dict):
        parsed = {"ok": False, "error": "response was not an object"}

    return ApiCallResult(status=status, body=parsed)


def summarize_chart_option(option_text: str) -> dict[str, object]:
    """Summarize an ECharts option JSON string for quality reporting."""
    if not option_text:
        return {"present": False, "empty_obj": False, "keys": [], "series_types": []}

    try:
        obj = json.loads(option_text)
    except Exception:
        return {"present": True, "empty_obj": False, "keys": ["<parse-error>"], "series_types": []}

    if not isinstance(obj, dict):
        return {"present": True, "empty_obj": False, "keys": ["<non-object>"], "series_types": []}

    series_types: list[str] = []
    series = obj.get("series")
    if isinstance(series, list):
        for item in series[:6]:
            if isinstance(item, dict):
                series_types.append(str(item.get("type") or ""))

    return {
        "present": True,
        "empty_obj": obj == {},
        "keys": sorted(list(obj.keys()))[:12],
        "series_types": series_types,
    }
