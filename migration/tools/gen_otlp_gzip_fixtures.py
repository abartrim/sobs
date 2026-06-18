#!/usr/bin/env python3
"""Generate the deterministic compressed request bodies for the OTLP Content-Encoding
parity fixtures (manifest ids post__v1_*__ingest_gzip / _deflate / gzip_invalid).

The OpenTelemetry Collector's otlphttp exporter enables gzip by default, so real OTLP
exports arrive with ``Content-Encoding: gzip`` and the server must inflate the body before
parsing it (app.py ``_decompress_request_body``; the Go port is ``decompressRequestBody`` in
go/cmd/sobs/otlp_decompress.go). These fixtures exercise that path end-to-end under parity.

This emits the ``body_b64`` blobs to paste into migration/manifest/routes.yaml. The bytes are
deterministic (gzip mtime=0, fixed zlib level) so the committed manifest is reproducible. As a
self-check it round-trips every blob through a VERBATIM copy of app.py's decompression helpers
(which depend only on stdlib ``zlib``), proving app.py will accept exactly these bytes.

Usage:  python migration/tools/gen_otlp_gzip_fixtures.py
Pure stdlib. Run from repo root.
"""

from __future__ import annotations

import base64
import gzip
import json
import zlib

# --- The OTLP-JSON payloads (identical to the uncompressed post__v1_*__ingest manifest
# entries, so the compressed variants yield the same {"accepted": N} response). ------------
LOGS_BODY = {
    "resourceLogs": [
        {
            "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "api"}}]},
            "scopeLogs": [
                {
                    "logRecords": [
                        {
                            "timeUnixNano": "1704164645000000000",
                            "severityText": "ERROR",
                            "body": {"stringValue": "boom"},
                            "attributes": [{"key": "event.name", "value": {"stringValue": "request.failed"}}],
                        }
                    ]
                }
            ],
        }
    ]
}

METRICS_BODY = {
    "resourceMetrics": [
        {
            "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "api"}}]},
            "scopeMetrics": [
                {
                    "metrics": [
                        {
                            "name": "cpu.usage",
                            "gauge": {
                                "dataPoints": [
                                    {
                                        "timeUnixNano": "1704164645000000000",
                                        "asDouble": 0.5,
                                        "attributes": [{"key": "core", "value": {"stringValue": "0"}}],
                                    }
                                ]
                            },
                        }
                    ]
                }
            ],
        }
    ]
}

# A body that CLAIMS Content-Encoding: gzip but is plain JSON (no gzip magic). app.py's
# decompressor raises zlib.error -> the JSON path returns 400 {"error":"failed to read request
# body"}; the Go port must match. Tiny, mutation-free (rejected before any insert).
INVALID_GZIP_BODY = b'{"resourceLogs":[]}'


# --- VERBATIM copy of app.py:9532-9599 (for cross-validation only; do NOT edit app.py). -----
_MAX_DECOMPRESSED_BODY_BYTES = 32 * 1024 * 1024


def _decompress_with_limit(raw: bytes, *, wbits: int) -> bytes:
    decompressor = zlib.decompressobj(wbits)
    output_parts: list[bytes] = []
    total = 0
    chunk_size = 64 * 1024
    for start in range(0, len(raw), chunk_size):
        remaining = _MAX_DECOMPRESSED_BODY_BYTES - total
        piece = decompressor.decompress(raw[start : start + chunk_size], remaining + 1)
        total += len(piece)
        if total > _MAX_DECOMPRESSED_BODY_BYTES:
            raise ValueError(f"decompressed body exceeds {_MAX_DECOMPRESSED_BODY_BYTES} bytes")
        if piece:
            output_parts.append(piece)
    remaining = _MAX_DECOMPRESSED_BODY_BYTES - total
    tail = decompressor.flush(remaining + 1)
    total += len(tail)
    if total > _MAX_DECOMPRESSED_BODY_BYTES:
        raise ValueError(f"decompressed body exceeds {_MAX_DECOMPRESSED_BODY_BYTES} bytes")
    if tail:
        output_parts.append(tail)
    return b"".join(output_parts)


def _decompress_request_body(raw: bytes, content_encoding: str) -> bytes:
    encodings = [e.strip().lower() for e in (content_encoding or "").split(",") if e.strip()]
    data = raw
    for enc in reversed(encodings):
        if enc == "gzip":
            data = _decompress_with_limit(data, wbits=16 + zlib.MAX_WBITS)
        elif enc == "deflate":
            try:
                data = _decompress_with_limit(data, wbits=zlib.MAX_WBITS)
            except zlib.error:
                data = _decompress_with_limit(data, wbits=-zlib.MAX_WBITS)
        elif len(data) > _MAX_DECOMPRESSED_BODY_BYTES:
            raise ValueError(f"decompressed body exceeds {_MAX_DECOMPRESSED_BODY_BYTES} bytes")
    return data


def _gzip(data: bytes) -> bytes:
    # mtime=0 → no wall-clock in the gzip header, so the output bytes are reproducible.
    return gzip.compress(data, compresslevel=9, mtime=0)


def _deflate(data: bytes) -> bytes:
    # zlib-wrapped deflate (the common "Content-Encoding: deflate"). Fixed level → reproducible.
    return zlib.compress(data, 9)


def _b64(data: bytes) -> str:
    return base64.b64encode(data).decode("ascii")


def main() -> int:
    logs_json = json.dumps(LOGS_BODY).encode()
    metrics_json = json.dumps(METRICS_BODY).encode()

    gz_logs = _gzip(logs_json)
    df_metrics = _deflate(metrics_json)

    # Cross-validate against app.py's verbatim decompressor: these exact bytes must round-trip.
    assert _decompress_request_body(gz_logs, "gzip") == logs_json, "gzip logs round-trip failed"
    assert _decompress_request_body(df_metrics, "deflate") == metrics_json, "deflate metrics round-trip failed"
    try:
        _decompress_request_body(INVALID_GZIP_BODY, "gzip")
    except zlib.error:
        pass
    else:
        raise AssertionError("invalid-gzip body unexpectedly decompressed")

    print("# Paste these body_b64 values into migration/manifest/routes.yaml.")
    print(f"#   gzip logs    -> {len(gz_logs)} compressed bytes  (decodes to {len(logs_json)} bytes, accepted: 1)")
    print(
        f"#   deflate metrics -> {len(df_metrics)} compressed bytes (decodes to {len(metrics_json)} bytes, accepted: 1)"
    )
    print(f"#   invalid gzip -> {len(INVALID_GZIP_BODY)} bytes (rejected, 400)\n")
    print(f"post__v1_logs__ingest_gzip   body_b64: {_b64(gz_logs)}")
    print(f"post__v1_metrics__ingest_deflate body_b64: {_b64(df_metrics)}")
    print(f"post__v1_logs__gzip_invalid  body_b64: {_b64(INVALID_GZIP_BODY)}")
    print("\nAll bodies round-trip through app.py's verbatim decompressor. ✓")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
