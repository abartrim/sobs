from __future__ import annotations

import json
import os
import re
import threading
import time
import uuid
from typing import Any

from quart import Blueprint, current_app, request, send_from_directory, url_for

import app as sobs_app
import telemetry as _telemetry
from app import (  # noqa: E402
    RUM_ASSET_DIR,
    RUM_ASSET_MAX_BYTES,
    ChDbConnection,
    ExportLogsServiceRequest,
    ExportMetricsServiceRequest,
    ExportTraceServiceRequest,
    WriteQueueFullError,
    _apply_tag_rules,
    _asset_extension,
    _extract_log_attr_maps,
    _insert_error_events,
    _insert_log_events,
    _insert_metric_events,
    _insert_rows_json_each_row,
    _insert_span_events,
    _json_error,
    _load_tag_rules,
    _maybe_demangle_js_stack,
    _normalize_origin,
    _now_iso,
    _parse_otlp_request,
    _proto_logs_to_events,
    _proto_metrics_to_events,
    _proto_traces_to_events,
    _remap_rum_console_stacks,
    _remember_log_attr_keys,
    _request_origin,
    _rum_asset_meta_path,
    _rum_client_token_encode,
    _sanitize_rum_asset_name,
    _sanitize_rum_asset_type,
    _severity_number,
    _sse_broadcast,
    _stringify_attrs,
    _verify_rum_asset_signature,
    _verify_rum_client_auth,
    jsonify,
    log,
    require_api_key,
    require_basic_auth,
)

ingest_bp: Blueprint = Blueprint("ingest", __name__)


def _queue_write(*args: Any, **kwargs: Any):
    return sobs_app._queue_write(*args, **kwargs)


@ingest_bp.route("/v1/logs", methods=["OPTIONS"])
@ingest_bp.route("/v1/traces", methods=["OPTIONS"])
@ingest_bp.route("/v1/metrics", methods=["OPTIONS"])
@ingest_bp.route("/v1/rum/assets", methods=["OPTIONS"])
async def ingest_preflight():
    return "", 204


@ingest_bp.route("/v1/logs", methods=["POST"])
@require_api_key
async def ingest_logs():
    with _telemetry.span("sobs.ingest.request", route="/v1/logs", **{"event.type": "log"}):
        msg, err = await _parse_otlp_request(ExportLogsServiceRequest)
        if err:
            return err
        with _telemetry.span("sobs.ingest.parse", **{"event.type": "log", "parser": "otlp"}):
            events = _proto_logs_to_events(msg)
        wait = bool(current_app.config.get("TESTING", False))
        try:
            _queue_write(lambda db: _insert_log_events(db, events), wait=wait)
        except WriteQueueFullError:
            return _json_error("write queue is full", 503)
        except Exception:
            log.exception("log ingest write failed")
            return _json_error("log ingest write failed", 500)
        for event in events:
            await _sse_broadcast(
                {
                    "source": "logs",
                    "ts": event.ts,
                    "level": event.level,
                    "service": event.service,
                    "body": event.body,
                    "trace_id": event.trace_id,
                }
            )
        count = len(events)
        _telemetry.record_ingest_events(count, "log")
        _telemetry.record_ingest_batch_size(count, "log")
        return jsonify({"accepted": count}), 200


@ingest_bp.route("/v1/rum/assets", methods=["POST"])
@require_api_key
async def ingest_rum_asset():
    asset_type = _sanitize_rum_asset_type(request.args.get("type", "asset"))
    asset_name = _sanitize_rum_asset_name(request.args.get("name", "asset"))
    content_type = (request.headers.get("Content-Type") or "application/octet-stream").split(";", 1)[0].strip()
    body = await request.get_data(cache=False)

    if not body:
        return jsonify({"error": "asset body is required"}), 400
    if len(body) > max(1024, RUM_ASSET_MAX_BYTES):
        return jsonify({"error": "asset exceeds max allowed size"}), 413

    ok, err = _verify_rum_asset_signature(
        body=body,
        method=request.method,
        path=request.path,
        content_type=content_type,
        asset_type=asset_type,
        asset_name=asset_name,
    )
    if not ok:
        if "not configured" in err:
            return jsonify({"error": err}), 503
        return jsonify({"error": err}), 401

    asset_id = uuid.uuid4().hex
    ext = _asset_extension(asset_name, content_type)
    storage_name = f"{asset_id}.{ext}"
    asset_path = os.path.join(RUM_ASSET_DIR, storage_name)
    meta_path = _rum_asset_meta_path(asset_id)

    with open(asset_path, "wb") as handle:
        handle.write(body)

    metadata = {
        "id": asset_id,
        "type": asset_type,
        "original_name": asset_name,
        "storage_name": storage_name,
        "content_type": content_type,
        "size": len(body),
        "uploaded_at": _now_iso(),
    }
    with open(meta_path, "w", encoding="utf-8") as handle:
        json.dump(metadata, handle, ensure_ascii=False)

    return (
        jsonify(
            {
                "id": asset_id,
                "type": asset_type,
                "name": asset_name,
                "contentType": content_type,
                "size": len(body),
                "url": url_for("ingest.rum_asset_download", asset_id=asset_id),
            }
        ),
        201,
    )


@ingest_bp.route("/v1/rum/assets/<asset_id>", methods=["GET"])
@require_basic_auth
async def rum_asset_download(asset_id: str):
    if not re.fullmatch(r"[a-f0-9]{32}", asset_id):
        return jsonify({"error": "invalid asset id"}), 400
    meta_path = _rum_asset_meta_path(asset_id)
    if not os.path.exists(meta_path):
        return jsonify({"error": "not found"}), 404
    try:
        with open(meta_path, encoding="utf-8") as handle:
            metadata = json.load(handle)
    except Exception:
        return jsonify({"error": "asset metadata unavailable"}), 500

    storage_name = str(metadata.get("storage_name", ""))
    if not storage_name or "/" in storage_name or "\\" in storage_name:
        return jsonify({"error": "invalid asset metadata"}), 500

    file_path = os.path.join(RUM_ASSET_DIR, storage_name)
    if not os.path.exists(file_path):
        return jsonify({"error": "not found"}), 404

    return await send_from_directory(
        RUM_ASSET_DIR,
        storage_name,
        mimetype=str(metadata.get("content_type", "application/octet-stream")),
        as_attachment=False,
    )


@ingest_bp.route("/v1/rum/client-token", methods=["POST"])
@require_api_key
async def issue_rum_client_token():
    mode = (sobs_app.RUM_CLIENT_AUTH_MODE or "none").strip().lower()
    if mode in ("", "none", "off", "disabled"):
        return jsonify({"enabled": False, "token": "", "error": "RUM client auth is disabled"}), 200

    if mode not in ("origin", "origin-session"):
        return jsonify({"error": "Invalid SOBS_RUM_CLIENT_AUTH_MODE"}), 500

    if not sobs_app.RUM_CLIENT_SIGNING_KEY:
        return jsonify({"error": "RUM client signing key is not configured"}), 503

    payload = await request.get_json(force=True, silent=True) or {}
    app_name = str(payload.get("appName") or payload.get("app") or "").strip()
    requested_origin = str(payload.get("origin") or "").strip()
    origin = _normalize_origin(requested_origin) or _request_origin()
    if not origin:
        return jsonify({"error": "origin is required"}), 400

    ttl_raw = payload.get("ttlSec", sobs_app.RUM_CLIENT_TOKEN_TTL_SEC)
    try:
        ttl_sec = int(ttl_raw)
    except (TypeError, ValueError):
        ttl_sec = sobs_app.RUM_CLIENT_TOKEN_TTL_SEC
    ttl_sec = max(30, min(ttl_sec, 24 * 60 * 60))

    now = int(time.time())
    claims = {
        "iss": "sobs-rum",
        "app": app_name,
        "origin": origin,
        "iat": now,
        "exp": now + ttl_sec,
        "jti": uuid.uuid4().hex,
    }
    token = _rum_client_token_encode(claims)
    return jsonify({"enabled": True, "token": token, "expiresAt": claims["exp"], "origin": origin, "app": app_name})


# ---------------------------------------------------------------------------
# OTLP Ingest – Traces  POST /v1/traces
# ---------------------------------------------------------------------------
@ingest_bp.route("/v1/traces", methods=["POST"])
@require_api_key
async def ingest_traces():
    with _telemetry.span("sobs.ingest.request", route="/v1/traces", **{"event.type": "trace"}):
        msg, err = await _parse_otlp_request(ExportTraceServiceRequest)
        if err:
            return err
        with _telemetry.span("sobs.ingest.parse", **{"event.type": "trace", "parser": "otlp"}):
            span_events, error_events = _proto_traces_to_events(msg)
        wait = bool(current_app.config.get("TESTING", False))

        def _op(db: ChDbConnection) -> None:
            _insert_span_events(db, span_events)
            _insert_error_events(db, error_events)

        try:
            _queue_write(_op, wait=wait)
        except WriteQueueFullError:
            return _json_error("write queue is full", 503)
        except Exception:
            log.exception("trace ingest write failed")
            return _json_error("trace ingest write failed", 500)
        for event in span_events:
            await _sse_broadcast(
                {
                    "source": "traces",
                    "ts": event.ts,
                    "trace_id": event.trace_id,
                    "span_id": event.span_id,
                    "name": event.name,
                    "service": event.service,
                    "duration_ms": event.duration_ms,
                    "status": event.status,
                }
            )
            # Also broadcast as an AI event when the span carries GenAI attributes
            provider = event.attrs.get("gen_ai.provider.name") or event.attrs.get("gen_ai.system", "")
            operation_name = str(event.attrs.get("gen_ai.operation.name", ""))
            if provider or operation_name:
                await _sse_broadcast(
                    {
                        "source": "ai",
                        "ts": event.ts,
                        "trace_id": event.trace_id,
                        "span_id": event.span_id,
                        "service": event.service,
                        "provider": provider,
                        "model": str(event.attrs.get("gen_ai.request.model", "")),
                        "operation": str(event.attrs.get("gen_ai.operation.name", "")),
                        "duration_ms": event.duration_ms,
                        "status": event.status,
                    }
                )
        count = len(span_events)
        _telemetry.record_ingest_events(count, "trace")
        _telemetry.record_ingest_batch_size(count, "trace")
        return jsonify({"accepted": count}), 200


# ---------------------------------------------------------------------------
# OTLP Ingest – Metrics  POST /v1/metrics
# ---------------------------------------------------------------------------
@ingest_bp.route("/v1/metrics", methods=["POST"])
@require_api_key
async def ingest_metrics():
    with _telemetry.span("sobs.ingest.request", route="/v1/metrics", **{"event.type": "metric"}):
        msg, err = await _parse_otlp_request(ExportMetricsServiceRequest)
        if err:
            return err
        try:
            with _telemetry.span("sobs.ingest.parse", **{"event.type": "metric", "parser": "otlp"}):
                events = _proto_metrics_to_events(msg)
        except Exception:
            log.exception("failed to convert metrics protobuf to events")
            return _json_error("failed to convert metrics protobuf to events", 500)
        wait = bool(current_app.config.get("TESTING", False))
        try:
            _queue_write(lambda db: _insert_metric_events(db, events), wait=wait)
        except WriteQueueFullError:
            return _json_error("write queue is full", 503)
        except Exception:
            log.exception("metric ingest write failed")
            return _json_error("metric ingest write failed", 500)
        count = len(events)
        _telemetry.record_ingest_events(count, "metric")
        _telemetry.record_ingest_batch_size(count, "metric")
        return jsonify({"accepted": count}), 200


# ---------------------------------------------------------------------------
# RUM Ingest  POST /v1/rum
# ---------------------------------------------------------------------------
_TRACEPARENT_RE = re.compile(r"^[0-9a-fA-F]{2}-([0-9a-fA-F]{32})-([0-9a-fA-F]{16})-([0-9a-fA-F]{2})$")


def _extract_trace_fields(event: dict[str, Any]) -> tuple[str, str, int]:
    trace_id = str(event.get("traceId", "") or "").strip().lower()
    span_id = str(event.get("spanId", "") or "").strip().lower()
    trace_flags = 0

    raw_flags = event.get("traceFlags")
    if raw_flags is not None and str(raw_flags).strip() != "":
        try:
            trace_flags = int(str(raw_flags), 16) if isinstance(raw_flags, str) else int(raw_flags)
        except (TypeError, ValueError):
            trace_flags = 0

    if trace_id and span_id:
        return trace_id, span_id, trace_flags

    traceparent = str(event.get("traceparent", "") or "").strip()
    match = _TRACEPARENT_RE.match(traceparent)
    if not match:
        return trace_id, span_id, trace_flags

    parsed_trace_id = match.group(1).lower()
    parsed_span_id = match.group(2).lower()
    parsed_flags = int(match.group(3), 16)

    return parsed_trace_id or trace_id, parsed_span_id or span_id, parsed_flags


# RUM Browser Context Delta Posting Cache
# Session ID -> { contextHash: str, fullContext: dict }
_RUM_BROWSER_CONTEXT_CACHE: dict[str, dict[str, Any]] = {}
_RUM_BROWSER_CONTEXT_CACHE_LOCK = threading.Lock()
_RUM_BROWSER_CONTEXT_CACHE_MAX = 10000  # Keep recent 10k sessions


def _handle_browser_context_delta(event: dict[str, Any]) -> dict[str, str]:
    """
    Handle delta posting for browser context.

    If event has full browserContext, cache it.
    If event has contextUnchanged flag, retrieve from cache.
    Returns the full browser context attributes to add to LogAttributes.
    """
    session_id = str(event.get("sessionId", ""))
    browser_context = event.get("browserContext", {})
    context_hash = str(event.get("contextHash", ""))
    context_unchanged = bool(event.get("contextUnchanged", False))

    if not session_id or not context_hash:
        return {}

    with _RUM_BROWSER_CONTEXT_CACHE_LOCK:
        # If we have full context, cache it
        if browser_context and isinstance(browser_context, dict):
            _RUM_BROWSER_CONTEXT_CACHE[session_id] = {
                "contextHash": context_hash,
                "fullContext": browser_context,
            }
            # Trim cache if too large
            if len(_RUM_BROWSER_CONTEXT_CACHE) > _RUM_BROWSER_CONTEXT_CACHE_MAX:
                # Remove oldest (arbitrary first items)
                to_remove = len(_RUM_BROWSER_CONTEXT_CACHE) - _RUM_BROWSER_CONTEXT_CACHE_MAX
                for _ in range(to_remove):
                    _RUM_BROWSER_CONTEXT_CACHE.pop(next(iter(_RUM_BROWSER_CONTEXT_CACHE)), None)

        # Retrieve cached context if contextUnchanged or if we received just the hash
        if context_unchanged or (not browser_context and context_hash):
            cached = _RUM_BROWSER_CONTEXT_CACHE.get(session_id, {})
            if cached.get("contextHash") == context_hash:
                browser_context = cached.get("fullContext", {})

    # Convert browser context dict to LogAttributes string map
    # Prefix with "browser." to keep organized
    attrs: dict[str, str] = {}
    if isinstance(browser_context, dict):
        for key, value in browser_context.items():
            if value is not None and value != "":
                attrs[f"browser.context.{key}"] = str(value)

    return attrs


@ingest_bp.route("/v1/rum", methods=["POST"])
@require_api_key
async def ingest_rum():
    payload = await request.get_json(force=True, silent=True)
    if payload is None:
        payload = {}
    if isinstance(payload, list):
        events = payload
    else:
        events = payload.get("events", [payload])
    # Extract client IP from proxy-forwarded or direct headers
    client_ip = (
        (request.headers.get("X-Forwarded-For", "") or "").split(",")[0].strip()
        or (request.headers.get("X-Real-IP", "") or "").strip()
        or (request.remote_addr or "")
    )

    ok, status_code, auth_err = _verify_rum_client_auth(events)
    if not ok:
        return jsonify({"error": auth_err}), status_code

    session_rows = []
    error_rows = []
    for event in events:
        if not isinstance(event, dict):
            continue
        event = dict(event)
        event.pop("clientAuthToken", None)
        if event.get("stack"):
            event["stack"] = _maybe_demangle_js_stack(str(event.get("stack", "")))
        _remap_rum_console_stacks(event)
        ts = event.get("timestamp", _now_iso())
        session_id = event.get("sessionId", "")
        event_type = event.get("type", "unknown")
        url = event.get("url", "")
        trace_id, span_id, trace_flags = _extract_trace_fields(event)
        attrs = _stringify_attrs(event)

        # Handle browser context delta posting (compress redundant context)
        browser_context_attrs = _handle_browser_context_delta(event)
        attrs.update(browser_context_attrs)

        if client_ip:
            attrs["client.ip"] = client_ip
        session_rows.append(
            {
                "Timestamp": ts,
                "TraceId": trace_id,
                "SpanId": span_id,
                "TraceFlags": trace_flags,
                "SeverityText": "ERROR" if event_type in ("error", "unhandledrejection") else "INFO",
                "SeverityNumber": _severity_number(
                    "ERROR" if event_type in ("error", "unhandledrejection") else "INFO"
                ),
                "ServiceName": str(event.get("service", "browser")),
                "Body": json.dumps(event, ensure_ascii=False),
                "ResourceSchemaUrl": "",
                "ResourceAttributes": {},
                "ScopeSchemaUrl": "",
                "ScopeName": "browser-rum",
                "ScopeVersion": "",
                "ScopeAttributes": {},
                "LogAttributes": attrs,
                "EventName": event_type,
            }
        )

        # Also index browser exceptions into otel_logs for unified error views.
        if event_type in ("error", "unhandledrejection"):
            err_attrs = {
                "exception.type": str(event.get("errorType", "JSError")),
                "exception.message": str(event.get("message", "")),
                "url.full": url,
                "session.id": session_id,
            }
            if event.get("stack"):
                err_attrs["exception.stacktrace"] = str(event.get("stack"))
            if event.get("errorSource"):
                err_attrs["error.source"] = str(event.get("errorSource"))
            page = event.get("page") if isinstance(event.get("page"), dict) else {}
            if page.get("title"):
                err_attrs["browser.page.title"] = str(page.get("title"))
            if page.get("viewport"):
                err_attrs["browser.viewport"] = str(page.get("viewport"))
            artifact = event.get("artifact") if isinstance(event.get("artifact"), dict) else {}
            if artifact.get("type"):
                err_attrs["artifact.type"] = str(artifact.get("type"))
            if artifact.get("id"):
                err_attrs["artifact.id"] = str(artifact.get("id"))
            if artifact.get("url"):
                err_attrs["artifact.url"] = str(artifact.get("url"))
            replay = event.get("replay") if isinstance(event.get("replay"), dict) else {}
            if replay.get("id"):
                err_attrs["replay.id"] = str(replay.get("id"))
            if replay.get("url"):
                err_attrs["replay.url"] = str(replay.get("url"))
            error_rows.append(
                {
                    "Timestamp": ts,
                    "TraceId": trace_id,
                    "SpanId": span_id,
                    "TraceFlags": trace_flags,
                    "SeverityText": "ERROR",
                    "SeverityNumber": _severity_number("ERROR"),
                    "ServiceName": "rum",
                    "Body": str(event.get("message", "")),
                    "ResourceSchemaUrl": "",
                    "ResourceAttributes": {},
                    "ScopeSchemaUrl": "",
                    "ScopeName": "browser-rum",
                    "ScopeVersion": "",
                    "ScopeAttributes": {},
                    "LogAttributes": err_attrs,
                    "EventName": "exception",
                }
            )
    wait = bool(current_app.config.get("TESTING", False))

    def _op(db: ChDbConnection) -> None:
        _insert_rows_json_each_row(db, "hyperdx_sessions", session_rows)
        _insert_rows_json_each_row(db, "otel_logs", error_rows)
        _remember_log_attr_keys(db, _extract_log_attr_maps(error_rows), record_type="log")
        try:
            rules = _load_tag_rules(db)
            if rules:
                _apply_tag_rules(db, "rum", session_rows, rules)
                if error_rows:
                    _apply_tag_rules(db, "error", error_rows, rules)
        except Exception:
            log.exception("auto-tag application failed for rum")

    try:
        _queue_write(_op, wait=wait)
    except WriteQueueFullError:
        return _json_error("write queue is full", 503)
    except Exception:
        log.exception("rum ingest write failed")
        return _json_error("rum ingest write failed", 500)
    count = len(session_rows)
    _telemetry.record_ingest_events(count, "rum")
    _telemetry.record_ingest_batch_size(count, "rum")
    return jsonify({"accepted": count}), 200


# ---------------------------------------------------------------------------
# AI Transparency  POST /v1/ai
# ---------------------------------------------------------------------------
@ingest_bp.route("/v1/ai", methods=["POST"])
@require_api_key
async def ingest_ai():
    payload = await request.get_json(force=True, silent=True) or {}
    ts = payload.get("timestamp", _now_iso())
    model = str(payload.get("model", ""))
    # Canonicalize operation: default to "chat", normalise case/whitespace
    operation = (str(payload.get("operation", "")) or "chat").lower().strip()
    duration_ms = float(payload.get("duration_ms", 0) or 0)
    provider = str(payload.get("provider", ""))
    service = str(payload.get("service", ""))
    span_name = f"{operation} {model}".strip()
    span_attrs: dict = {
        "gen_ai.operation.name": operation,
        "gen_ai.provider.name": provider,
        "gen_ai.request.model": model,
        "gen_ai.usage.input_tokens": int(payload.get("tokens_in", 0) or 0),
        "gen_ai.usage.output_tokens": int(payload.get("tokens_out", 0) or 0),
    }
    # Standard OTel GenAI content attributes (primary)
    if payload.get("input_messages") is not None:
        raw = payload["input_messages"]
        span_attrs["gen_ai.input.messages"] = raw if isinstance(raw, str) else json.dumps(raw, ensure_ascii=False)
    if payload.get("output_messages") is not None:
        raw = payload["output_messages"]
        span_attrs["gen_ai.output.messages"] = raw if isinstance(raw, str) else json.dumps(raw, ensure_ascii=False)
    if payload.get("system_instructions") is not None:
        raw = payload["system_instructions"]
        span_attrs["gen_ai.system_instructions"] = raw if isinstance(raw, str) else json.dumps(raw, ensure_ascii=False)
    # Legacy sobs fields (kept for backward-compat / UI fallback)
    if payload.get("prompt"):
        span_attrs["sobs.gen_ai.prompt"] = str(payload["prompt"])
    if payload.get("response"):
        span_attrs["sobs.gen_ai.response"] = str(payload["response"])
    if payload.get("error_type"):
        span_attrs["error.type"] = str(payload["error_type"])
    row = {
        "Timestamp": ts,
        "TraceId": str(payload.get("trace_id", "")),
        "SpanId": str(payload.get("span_id", "")),
        "ParentSpanId": "",
        "TraceState": "",
        "SpanName": span_name,
        "SpanKind": "CLIENT",
        "ServiceName": service,
        "ResourceAttributes": {},
        "ScopeName": "sobs-ai",
        "ScopeVersion": "",
        "SpanAttributes": _stringify_attrs(span_attrs),
        "Duration": max(0, int(duration_ms * 1_000_000)),
        "StatusCode": "STATUS_CODE_OK",
        "StatusMessage": "",
        "Events": {"Timestamp": [], "Name": [], "Attributes": []},
        "Links": {"TraceId": [], "SpanId": [], "TraceState": [], "Attributes": []},
    }
    wait = bool(current_app.config.get("TESTING", False))

    def _op(db: ChDbConnection) -> None:
        _insert_rows_json_each_row(db, "otel_traces", [row])
        try:
            rules = _load_tag_rules(db)
            if rules:
                _apply_tag_rules(db, "ai", [row], rules)
        except Exception:
            log.exception("auto-tag application failed for ai")

    try:
        _queue_write(_op, wait=wait)
    except WriteQueueFullError:
        return _json_error("write queue is full", 503)
    except Exception:
        log.exception("ai ingest write failed")
        return _json_error("ai ingest write failed", 500)
    await _sse_broadcast(
        {
            "source": "ai",
            "ts": ts,
            "service": service,
            "provider": provider,
            "model": model,
            "operation": operation,
            "duration_ms": round(duration_ms, 1),
            "tokens_in": span_attrs["gen_ai.usage.input_tokens"],
            "tokens_out": span_attrs["gen_ai.usage.output_tokens"],
        }
    )
    return jsonify({"ok": True}), 200


# ---------------------------------------------------------------------------
# Error ingest  POST /v1/errors  (direct error submission)
# ---------------------------------------------------------------------------
@ingest_bp.route("/v1/errors", methods=["POST"])
@require_api_key
async def ingest_errors():
    payload = await request.get_json(force=True, silent=True) or {}
    ts = payload.get("timestamp", _now_iso())
    attrs = _stringify_attrs(payload.get("attributes", {}))
    attrs["exception.type"] = str(payload.get("type", "Error"))
    attrs["exception.message"] = str(payload.get("message", ""))
    if payload.get("stack"):
        attrs["exception.stacktrace"] = _maybe_demangle_js_stack(str(payload.get("stack")))
    row = {
        "Timestamp": ts,
        "TraceId": str(payload.get("trace_id", "")),
        "SpanId": str(payload.get("span_id", "")),
        "TraceFlags": 0,
        "SeverityText": "ERROR",
        "SeverityNumber": _severity_number("ERROR"),
        "ServiceName": str(payload.get("service", "")),
        "Body": str(payload.get("message", "")),
        "ResourceSchemaUrl": "",
        "ResourceAttributes": {},
        "ScopeSchemaUrl": "",
        "ScopeName": "",
        "ScopeVersion": "",
        "ScopeAttributes": {},
        "LogAttributes": attrs,
        "EventName": "exception",
    }
    wait = bool(current_app.config.get("TESTING", False))

    def _op(db: ChDbConnection) -> None:
        _insert_rows_json_each_row(db, "otel_logs", [row])
        _remember_log_attr_keys(db, _extract_log_attr_maps([row]), record_type="log")
        try:
            rules = _load_tag_rules(db)
            if rules:
                _apply_tag_rules(db, "error", [row], rules)
        except Exception:
            log.exception("auto-tag application failed for direct errors")

    try:
        _queue_write(_op, wait=wait)
    except WriteQueueFullError:
        return _json_error("write queue is full", 503)
    except Exception:
        log.exception("error ingest write failed")
        return _json_error("error ingest write failed", 500)
    _telemetry.record_ingest_events(1, "error")
    return jsonify({"ok": True}), 200
