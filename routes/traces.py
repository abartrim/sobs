from __future__ import annotations

import json
import logging
from datetime import datetime, timezone
from typing import Any, cast

from quart import Blueprint, render_template, request

import app as sobs_app
import telemetry as _telemetry
from app import (  # noqa: E402
    _TRACE_DETAIL_COLLAPSE_THRESHOLD,
    _TRACE_DETAIL_DEFAULT_LIMIT,
    _TRACE_DETAIL_HARD_CAP,
    _TRACE_DETAIL_MAX_LIMIT,
    ERROR_SOURCES_SQL,
    _active_part_rows,
    _append_regex_expression_clauses,
    _append_time_window_filter,
    _build_error_item,
    _build_span_tree,
    _build_trace_timeline_segments,
    _build_trace_window_overlay_segments,
    _coerce_positive_int,
    _compute_active_timeline_ms,
    _error_id_sql_expr,
    _load_work_item_links_for_ref_ids,
    _map_to_dict,
    _mask_value_for_output,
    _parse_limit,
    _parse_offset,
    _parse_sort,
    _parse_time_window_args,
    _prepare_re2_filter_patterns,
    _slice_span_tree_with_ancestors,
    _ts_str_to_epoch_ms,
    _where_clause,
    jsonify,
    masked_jsonify,
    require_basic_auth,
)
from routes.logs import (  # noqa: E402
    _parse_and_validate_regex_expression_for_api,
    _regex_best_effort_sample,
    _regex_scope_text,
    _regex_scope_time_conditions,
)

log = logging.getLogger("sobs")
traces_bp: Blueprint = Blueprint("traces", __name__)


def _fetch_trace_metric_context(*args: Any, **kwargs: Any):
    return sobs_app._fetch_trace_metric_context(*args, **kwargs)


def _list_trace_overlapping_raw_windows(*args: Any, **kwargs: Any):
    return sobs_app._list_trace_overlapping_raw_windows(*args, **kwargs)


def get_db():
    return sobs_app.get_db()


@traces_bp.route("/traces")
@require_basic_auth
@_telemetry.traced_view("sobs.dashboard.query", **{"dashboard.name": "traces", "route": "/traces"})
async def view_traces():
    db = get_db()
    error_id_sql = _error_id_sql_expr()
    selected_services = [svc.strip() for svc in request.args.getlist("service") if svc.strip()]
    service = selected_services[0] if selected_services else ""
    trace_id = request.args.get("trace_id", "").strip()
    from_ts, to_ts, time_error = _parse_time_window_args()
    limit = _parse_limit(100)
    offset = _parse_offset()
    trace_span_limit = _coerce_positive_int(
        request.args.get("trace_span_limit"),
        _TRACE_DETAIL_DEFAULT_LIMIT,
        1,
        _TRACE_DETAIL_MAX_LIMIT,
    )
    trace_span_offset = _coerce_positive_int(request.args.get("trace_span_offset"), 0, 0, _TRACE_DETAIL_HARD_CAP)
    sort_by, sort_col, sort_dir = _parse_sort(
        {
            "Timestamp": "Timestamp",
            "SpanName": "SpanName",
            "ServiceName": "ServiceName",
            "Duration": "Duration",
        },
        "Timestamp",
    )
    order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"

    conditions = []
    params = []
    q = request.args.get("q", "").strip()
    q_error = ""
    include_patterns: list[str] = []
    exclude_patterns: list[str] = []
    if q:
        include_patterns, exclude_patterns, regex_error = _prepare_re2_filter_patterns(db, q)
        if regex_error:
            q_error = regex_error
    if selected_services:
        placeholders = ",".join(["?"] * len(selected_services))
        conditions.append(f"ServiceName IN ({placeholders})")
        params.extend(selected_services)
    if trace_id:
        conditions.append("TraceId=?")
        params.append(trace_id)
    _append_time_window_filter(conditions, params, "Timestamp", from_ts, to_ts)
    if q and not q_error:
        _append_regex_expression_clauses(
            conditions=conditions,
            params=params,
            column="SpanName",
            include_patterns=include_patterns,
            exclude_patterns=exclude_patterns,
        )
    where = _where_clause(conditions)
    if trace_id and not time_error:
        total = 0
        rows = []
    else:
        if not where:
            total = _active_part_rows(db, "otel_traces")
        else:
            total = db.execute(f"SELECT COUNT(*) FROM otel_traces {where}", params).fetchone()[0]
        rows = db.execute(
            (
                "SELECT Timestamp, TraceId, SpanId, ParentSpanId, "
                "SpanName, ServiceName, Duration, StatusCode, SpanAttributes "
                f"FROM otel_traces {where} {order_clause} LIMIT ? OFFSET ?"
            ),
            params + [limit, offset],
        ).fetchall()

    spans = []
    for r in rows:
        attrs = _map_to_dict(r["SpanAttributes"])
        spans.append(
            {
                "ts": str(r["Timestamp"]),
                "trace_id": r["TraceId"],
                "span_id": r["SpanId"],
                "parent_span_id": r["ParentSpanId"],
                "name": r["SpanName"],
                "service": r["ServiceName"],
                "duration_ms": round(float(r["Duration"]) / 1_000_000, 2),
                "status": r["StatusCode"],
                "http_method": attrs.get("http.method", attrs.get("http.request.method", "")),
                "http_url": attrs.get("http.url", attrs.get("url.full", "")),
                "http_status": attrs.get("http.status_code", attrs.get("http.response.status_code", "")),
            }
        )

    services = [
        row[0]
        for row in db.execute(
            "SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName!='' ORDER BY ServiceName"
        ).fetchall()
    ]

    # When a specific trace is selected build an enriched detail view.
    trace_detail: dict | None = None
    if trace_id and not time_error:
        trace_total_spans = int(
            db.execute("SELECT COUNT(*) FROM otel_traces WHERE TraceId=?", [trace_id]).fetchone()[0] or 0
        )
        detail_fetch_limit = min(trace_total_spans, _TRACE_DETAIL_HARD_CAP)
        detail_rows = db.execute(
            "SELECT Timestamp, TraceId, SpanId, ParentSpanId, SpanName, ServiceName, "
            "Duration, StatusCode, SpanAttributes "
            "FROM otel_traces WHERE TraceId=? ORDER BY Timestamp ASC, SpanId ASC LIMIT ?",
            [trace_id, detail_fetch_limit],
        ).fetchall()
        if detail_rows:
            all_trace_spans = []
            for r in detail_rows:
                attrs = _map_to_dict(r["SpanAttributes"])
                ts_str = str(r["Timestamp"])
                start_ms = _ts_str_to_epoch_ms(ts_str)
                dur_ms = round(float(r["Duration"]) / 1_000_000, 2)
                all_trace_spans.append(
                    {
                        "ts": ts_str,
                        "trace_id": str(r["TraceId"]),
                        "span_id": str(r["SpanId"]),
                        "parent_span_id": str(r["ParentSpanId"]),
                        "name": str(r["SpanName"]),
                        "service": str(r["ServiceName"]),
                        "start_ms": start_ms,
                        "duration_ms": dur_ms,
                        "status": str(r["StatusCode"]),
                        "http_method": str(attrs.get("http.method", attrs.get("http.request.method", ""))),
                        "http_url": str(attrs.get("http.url", attrs.get("url.full", ""))),
                        "http_status": str(attrs.get("http.status_code", attrs.get("http.response.status_code", ""))),
                        "namespace": str(attrs.get("k8s.namespace.name", attrs.get("namespace", ""))),
                        "pod": str(attrs.get("k8s.pod.name", attrs.get("pod", ""))),
                        "node": str(attrs.get("k8s.node.name", attrs.get("node", ""))),
                        "deployment": str(attrs.get("k8s.deployment.name", attrs.get("deployment", ""))),
                    }
                )

            # Compute relative timeline positions.
            trace_start_ms = min(s["start_ms"] for s in all_trace_spans)
            trace_end_ms = max(s["start_ms"] + s["duration_ms"] for s in all_trace_spans)
            trace_total_ms = max(trace_end_ms - trace_start_ms, 1.0)
            trace_active_ms = _compute_active_timeline_ms(all_trace_spans)
            trace_coverage_pct = min(100.0, max(0.0, (trace_active_ms / trace_total_ms) * 100.0))
            trace_span_sum_ms = sum(max(0.0, float(s.get("duration_ms", 0.0) or 0.0)) for s in all_trace_spans)
            for span in all_trace_spans:
                span["offset_pct"] = round((span["start_ms"] - trace_start_ms) / trace_total_ms * 100, 2)
                # 0.5 minimum keeps very short spans visible in the timeline bar
                span["width_pct"] = round(max(0.5, span["duration_ms"] / trace_total_ms * 100), 2)

            # Fetch related errors for this trace (capped at 50; flag truncation for the UI).
            _TRACE_ERROR_LIMIT = 50
            trace_errors: list[dict] = []
            errors_truncated = False
            trace_activity_ts_ms: list[float] = []
            try:
                err_rows = db.execute(
                    "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes, ErrorId, "
                    "(ErrorId IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)) AS IsResolved "
                    "FROM ("
                    "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes, "
                    f"{error_id_sql} AS ErrorId "
                    f"FROM ({ERROR_SOURCES_SQL}) WHERE TraceId=? LIMIT ?"
                    ")",
                    [trace_id, _TRACE_ERROR_LIMIT + 1],
                ).fetchall()
                if len(err_rows) > _TRACE_ERROR_LIMIT:
                    errors_truncated = True
                    err_rows = err_rows[:_TRACE_ERROR_LIMIT]
                for row in err_rows:
                    item = _build_error_item(dict(row))
                    item["id"] = str(row["ErrorId"] or item["id"])
                    item["resolved"] = bool(row["IsResolved"])
                    trace_errors.append(item)
                    ts_raw = str(item.get("ts") or "")
                    if ts_raw:
                        trace_activity_ts_ms.append(_ts_str_to_epoch_ms(ts_raw))
            except Exception as exc:
                log.warning("view_traces: failed to fetch errors for trace %s: %s", trace_id, exc)

            error_span_ids = {e["span_id"] for e in trace_errors if e.get("span_id")}

            # Fetch log counts per span for this trace.
            log_counts: dict[str, int] = {}
            try:
                log_rows = db.execute(
                    "SELECT SpanId, count() AS cnt FROM otel_logs " "WHERE TraceId=? AND SpanId!='' GROUP BY SpanId",
                    [trace_id],
                ).fetchall()
                for r in log_rows:
                    log_counts[str(r["SpanId"])] = int(r["cnt"])

                log_ts_rows = db.execute(
                    "SELECT Timestamp FROM otel_logs WHERE TraceId=? LIMIT 2000",
                    [trace_id],
                ).fetchall()
                for r in log_ts_rows:
                    trace_activity_ts_ms.append(_ts_str_to_epoch_ms(str(r["Timestamp"])))
            except Exception as exc:
                log.warning("view_traces: failed to fetch log counts for trace %s: %s", trace_id, exc)

            timeline_segments = _build_trace_timeline_segments(all_trace_spans, trace_activity_ts_ms)
            has_potential_gap = any(
                seg.get("kind") == "gap" and bool(seg.get("potential")) for seg in timeline_segments
            )

            # Fetch anomaly state for the primary service.
            trace_anomaly_state: str | None = None
            try:
                svc = all_trace_spans[0]["service"] if all_trace_spans else ""
                if svc:
                    anomaly_row = db.execute(
                        "SELECT anomaly_state FROM v_derived_signals_anomaly "
                        "WHERE ServiceName=? AND SignalSource='traces' "
                        "AND time >= now() - INTERVAL 48 HOUR "
                        "ORDER BY time DESC LIMIT 1",
                        [svc],
                    ).fetchone()
                    if anomaly_row:
                        trace_anomaly_state = str(anomaly_row["anomaly_state"])
            except Exception as exc:
                log.warning("view_traces: failed to fetch anomaly state for trace %s: %s", trace_id, exc)

            trace_windows: list[dict[str, object]] = []
            try:
                trace_services = sorted(
                    {str(s.get("service") or "").strip() for s in all_trace_spans if s.get("service")}
                )
                trace_start_ts = datetime.fromtimestamp(trace_start_ms / 1000.0, tz=timezone.utc).isoformat()
                trace_end_ts = datetime.fromtimestamp(trace_end_ms / 1000.0, tz=timezone.utc).isoformat()
                trace_windows = _list_trace_overlapping_raw_windows(
                    db,
                    service_names=trace_services,
                    start_ts=trace_start_ts,
                    end_ts=trace_end_ts,
                )
            except Exception as exc:
                log.warning("view_traces: failed to fetch raw windows for trace %s: %s", trace_id, exc)

            trace_metrics_context: dict[str, object] = {
                "source_mode": "none",
                "total_points": 0,
                "series": [],
                "match_mode": "none",
                "match_label": "no match",
                "match_dimensions": [],
            }
            try:
                trace_services = sorted(
                    {str(s.get("service") or "").strip() for s in all_trace_spans if s.get("service")}
                )
                trace_namespaces = sorted(
                    {str(s.get("namespace") or "").strip() for s in all_trace_spans if s.get("namespace")}
                )
                trace_pods = sorted({str(s.get("pod") or "").strip() for s in all_trace_spans if s.get("pod")})
                trace_nodes = sorted({str(s.get("node") or "").strip() for s in all_trace_spans if s.get("node")})
                trace_deployments = sorted(
                    {str(s.get("deployment") or "").strip() for s in all_trace_spans if s.get("deployment")}
                )
                # Expand the metric window by ±5 minutes around the trace so short
                # traces (e.g. 35ms) still capture surrounding metric data points.
                _METRIC_PAD_MS = 5 * 60 * 1000
                metric_ctx_start_ts = datetime.fromtimestamp(
                    (trace_start_ms - _METRIC_PAD_MS) / 1000.0, tz=timezone.utc
                ).isoformat()
                metric_ctx_end_ts = datetime.fromtimestamp(
                    (trace_end_ms + _METRIC_PAD_MS) / 1000.0, tz=timezone.utc
                ).isoformat()
                trace_metrics_context = _fetch_trace_metric_context(
                    db,
                    service_names=trace_services,
                    start_ts=metric_ctx_start_ts,
                    end_ts=metric_ctx_end_ts,
                    window_ids=[str(w.get("id") or "") for w in trace_windows if str(w.get("id") or "")],
                    namespace_values=trace_namespaces,
                    pod_values=trace_pods,
                    node_values=trace_nodes,
                    deployment_values=trace_deployments,
                )
            except Exception as exc:
                log.warning("view_traces: failed to fetch metrics context for trace %s: %s", trace_id, exc)

            trace_window_segments = _build_trace_window_overlay_segments(all_trace_spans, trace_windows)

            full_span_tree = _build_span_tree(all_trace_spans)
            capped_total_spans = len(full_span_tree)
            if trace_span_offset >= capped_total_spans and capped_total_spans > 0:
                trace_span_offset = max(0, ((capped_total_spans - 1) // trace_span_limit) * trace_span_limit)
            trace_page_spans, trace_page_end, trace_context_rows = _slice_span_tree_with_ancestors(
                full_span_tree,
                trace_span_offset,
                trace_span_limit,
            )
            detail_prev_offset = max(0, trace_span_offset - trace_span_limit)
            detail_next_offset = trace_span_offset + trace_span_limit
            detail_hard_capped = trace_total_spans > _TRACE_DETAIL_HARD_CAP
            default_collapsed = capped_total_spans > _TRACE_DETAIL_COLLAPSE_THRESHOLD

            total = trace_total_spans

            trace_detail = {
                "span_tree": trace_page_spans,
                "trace_start_ts": str(all_trace_spans[0]["ts"]),
                "trace_end_ts": str(all_trace_spans[-1]["ts"]),
                "trace_start_ms": round(trace_start_ms),
                "trace_end_ms": round(trace_end_ms),
                "errors": trace_errors,
                "errors_truncated": errors_truncated,
                "error_span_ids": error_span_ids,
                "log_counts": log_counts,
                "anomaly_state": trace_anomaly_state,
                "total_ms": round(trace_total_ms, 2),
                "active_ms": round(trace_active_ms, 2),
                "coverage_pct": round(trace_coverage_pct, 2),
                "span_sum_ms": round(trace_span_sum_ms, 2),
                "timeline_segments": timeline_segments,
                "has_potential_gap": has_potential_gap,
                "raw_windows": trace_windows,
                "raw_window_segments": trace_window_segments,
                "metrics_context": trace_metrics_context,
                "total_spans": trace_total_spans,
                "capped_total_spans": capped_total_spans,
                "hard_cap": _TRACE_DETAIL_HARD_CAP,
                "hard_capped": detail_hard_capped,
                "default_collapsed": default_collapsed,
                "page_limit": trace_span_limit,
                "page_offset": trace_span_offset,
                "page_end": trace_page_end,
                "context_rows": trace_context_rows,
                "prev_offset": detail_prev_offset,
                "next_offset": detail_next_offset,
                "has_prev_page": trace_span_offset > 0,
                "has_next_page": detail_next_offset < capped_total_spans,
            }

    # Build work-item pre-check map for trace-detail errors so "Raise issue" shows as
    # "View issue →" when an issue already exists for this error or trace.
    trace_work_item_links: dict[str, dict] = {}
    if trace_detail:
        trace_errors_local = trace_detail.get("errors") or []
        ref_ids = list(
            {e["id"] for e in trace_errors_local if e.get("id")}
            | {e["trace_id"] for e in trace_errors_local if e.get("trace_id")}
        )
        if trace_id:
            ref_ids.append(trace_id)
        trace_work_item_links = _load_work_item_links_for_ref_ids(db, ref_ids)

    return await render_template(
        "traces.html",
        spans=spans,
        total=total,
        limit=limit,
        offset=offset,
        service=service,
        selected_services=selected_services,
        trace_id=trace_id,
        from_ts=from_ts,
        to_ts=to_ts,
        error_msg=q_error or time_error,
        q=q,
        services=services,
        sort_by=sort_by,
        sort_dir=sort_dir,
        trace_detail=trace_detail,
        work_item_links=trace_work_item_links,
    )


# ---------------------------------------------------------------------------
# Raw span payload API  GET /api/traces/span/<span_id>
# Returns the full raw record for a single span as JSON.  The payload is
# truncated to _RAW_SPAN_MAX_BYTES so that very large attribute blobs do not
# overwhelm the browser.  The endpoint is used by the lazy-loaded accordion
# on the trace detail page and is intentionally additive – it does not change
# any existing UI behaviour.
# ---------------------------------------------------------------------------

_RAW_SPAN_MAX_BYTES = 32 * 1024  # 32 KB display cap


@traces_bp.route("/api/traces/span/<span_id>", methods=["GET"])
@require_basic_auth
async def api_raw_span(span_id: str):
    """Return the raw record for a single span as a JSON object."""
    span_id = span_id.strip()
    if not span_id:
        return jsonify({"error": "span_id is required"}), 400

    trace_id = (request.args.get("trace_id") or "").strip()

    db = get_db()
    base_sql = (
        "SELECT Timestamp, TraceId, SpanId, ParentSpanId, TraceState, "
        "SpanName, SpanKind, ServiceName, ResourceAttributes, "
        "ScopeName, ScopeVersion, SpanAttributes, Duration, "
        "StatusCode, StatusMessage "
        "FROM otel_traces WHERE SpanId=?"
    )
    params: list[str] = [span_id]
    if trace_id:
        # Prefer a trace-qualified match when available so duplicate span IDs
        # across traces return the expected row.
        base_sql += " AND TraceId=?"
        params.append(trace_id)
    # Keep fallback deterministic even when multiple rows share a span_id.
    base_sql += " ORDER BY Timestamp DESC LIMIT 1"
    row = db.execute(base_sql, params).fetchone()

    if row is None:
        return jsonify({"error": "span not found"}), 404

    span_attrs = dict(_map_to_dict(row["SpanAttributes"]))
    resource_attrs = dict(_map_to_dict(row["ResourceAttributes"]))

    payload: dict[str, object] = {
        "timestamp": str(row["Timestamp"]),
        "trace_id": str(row["TraceId"]),
        "span_id": str(row["SpanId"]),
        "parent_span_id": str(row["ParentSpanId"]),
        "trace_state": str(row["TraceState"]),
        "name": str(row["SpanName"]),
        "kind": str(row["SpanKind"]),
        "service": str(row["ServiceName"]),
        "scope_name": str(row["ScopeName"]),
        "scope_version": str(row["ScopeVersion"]),
        "duration_ns": int(row["Duration"]),
        "duration_ms": round(int(row["Duration"]) / 1_000_000, 3),
        "status_code": str(row["StatusCode"]),
        "status_message": str(row["StatusMessage"]),
        "attributes": span_attrs,
        "resource_attributes": resource_attrs,
    }

    masked_payload = cast(dict[str, object], _mask_value_for_output(payload))
    raw = json.dumps(masked_payload, ensure_ascii=False, indent=2)
    truncated = False
    if len(raw.encode()) > _RAW_SPAN_MAX_BYTES:
        truncated = True
        # Truncate large attribute values to keep the overall payload small.
        _ATTR_TRUNCATE = 512
        payload["attributes"] = {
            k: (v[:_ATTR_TRUNCATE] + "…" if isinstance(v, str) and len(v) > _ATTR_TRUNCATE else v)
            for k, v in span_attrs.items()
        }
        payload["resource_attributes"] = {
            k: (v[:_ATTR_TRUNCATE] + "…" if isinstance(v, str) and len(v) > _ATTR_TRUNCATE else v)
            for k, v in resource_attrs.items()
        }
        masked_payload = cast(dict[str, object], _mask_value_for_output(payload))
        raw = json.dumps(masked_payload, ensure_ascii=False, indent=2)

    return masked_jsonify({"span": masked_payload, "raw": raw, "truncated": truncated})


@traces_bp.route("/api/traces/validate-regex", methods=["POST"])
@require_basic_auth
async def api_traces_validate_regex():
    """Validate a regex pattern used by /traces?q=... and return a sample match."""
    payload = await request.get_json(silent=True)
    pattern = str((payload or {}).get("pattern", "") or "").strip()
    scope = (payload or {}).get("scope")
    if not isinstance(scope, dict):
        scope = {}
    if not pattern:
        return jsonify({"ok": True, "sample": None})

    db = get_db()
    include_patterns, _exclude_patterns, expression_error = _parse_and_validate_regex_expression_for_api(db, pattern)
    if expression_error:
        return jsonify({"ok": False, "error": expression_error, "sample": None})

    try:
        where_parts: list[str] = []
        where_params: list[Any] = []

        service = _regex_scope_text(scope, "service")
        trace_id = _regex_scope_text(scope, "trace_id", 64)
        if service:
            where_parts.append("ServiceName = ?")
            where_params.append(service)
        if trace_id:
            where_parts.append("TraceId = ?")
            where_params.append(trace_id)

        time_parts, time_params = _regex_scope_time_conditions(scope, "Timestamp")
        where_parts.extend(time_parts)
        where_params.extend(time_params)

        sample = _regex_best_effort_sample(
            db,
            from_sql="otel_traces",
            sample_column="SpanName",
            order_column="Timestamp",
            include_patterns=include_patterns,
            exclude_patterns=_exclude_patterns,
            where_parts=where_parts,
            where_params=where_params,
        )
        return masked_jsonify({"ok": True, "sample": sample})
    except Exception:
        return masked_jsonify({"ok": True, "sample": None})


# ---------------------------------------------------------------------------
# Metrics Regex Validate API  POST /api/metrics/validate-regex
