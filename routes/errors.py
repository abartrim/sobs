from __future__ import annotations

import logging
import time
from typing import Any

from quart import Blueprint, render_template, request

import app as sobs_app
import telemetry as _telemetry
from app import (  # noqa: E402
    ERROR_SOURCES_SQL,
    ERRORS_SERVICES_CACHE_TTL_SEC,
    ChDbConnection,
    _append_regex_expression_clauses,
    _append_time_window_filter,
    _build_error_item,
    _error_id,
    _error_id_sql_expr,
    _errors_cache_lock,
    _errors_services_cache,
    _extract_structured_error_summary,
    _get_resolved_error_ids,
    _hex,
    _json_error,
    _load_work_item_links_for_ref_ids,
    _parse_limit,
    _parse_offset,
    _parse_sort,
    _parse_time_window_args,
    _prepare_re2_filter_patterns,
    _where_clause,
    get_db,
    jsonify,
    require_basic_auth,
)

log = logging.getLogger("sobs")
errors_bp: Blueprint = Blueprint("errors", __name__)


def _queue_write(*args: Any, **kwargs: Any):
    return sobs_app._queue_write(*args, **kwargs)


@errors_bp.route("/errors")
@require_basic_auth
@_telemetry.traced_view("sobs.dashboard.query", **{"dashboard.name": "errors", "route": "/errors"})
async def view_errors():
    db = get_db()
    error_id_sql = _error_id_sql_expr()
    grouped_trace_chunk_size = 200
    hydrate_key_chunk_size = 200

    def _build_error_stub_from_narrow(row: dict, resolved_flag: bool) -> dict:
        ts = str(row.get("Timestamp", ""))
        service_name = str(row.get("ServiceName", ""))
        trace_id = str(row.get("TraceId", ""))
        span_id = str(row.get("SpanId", ""))
        err_type = str(row.get("ErrorType", "") or "Error")
        message = str(row.get("ErrorMessage", ""))
        raw_body = str(row.get("Body", ""))
        message_summary, summary_from_json = _extract_structured_error_summary(message, raw_body)
        item_id = str(row.get("ErrorId", "")) or _error_id(ts, service_name, err_type, message, trace_id, span_id)
        return {
            "id": item_id,
            "ts": ts,
            "service": service_name,
            "err_type": err_type,
            "message": message,
            "message_summary": message_summary,
            "summary_from_json": summary_from_json,
            "message_is_json": False,
            "message_pretty_json": "",
            "raw_body": raw_body,
            "raw_body_is_json": False,
            "raw_body_pretty_json": "",
            "stack": "",
            "stack_is_json": False,
            "stack_pretty_json": "",
            "trace_id": trace_id,
            "span_id": span_id,
            "url": "",
            "error_source": "",
            "page_title": "",
            "viewport": "",
            "artifact_type": "",
            "artifact_id": "",
            "artifact_url": "",
            "replay_id": "",
            "replay_url": "",
            "resolved": resolved_flag,
        }

    selected_services = [svc.strip() for svc in request.args.getlist("service") if svc.strip()]
    service = selected_services[0] if selected_services else ""
    group_by = request.args.get("group_by", "").strip().lower()
    grouped_mode = request.args.get("grouped", "").strip() == "1" or group_by in {
        "group",
        "message",
        "fingerprint",
        "signature",
    }
    from_ts, to_ts, time_error = _parse_time_window_args()
    resolved = request.args.get("resolved", "0").strip()
    limit = _parse_limit(100)
    offset = _parse_offset()
    if grouped_mode:
        sort_by, sort_col, sort_dir = _parse_sort(
            {
                "count": "Count",
                "last_seen": "LastSeen",
                "ServiceName": "RepServiceName",
                # Legacy alias retained for backwards compatibility.
                "Timestamp": "LastSeen",
            },
            "count",
        )
    else:
        sort_by, sort_col, sort_dir = _parse_sort(
            {"Timestamp": "Timestamp", "ServiceName": "ServiceName"},
            "Timestamp",
        )
    q = request.args.get("q", "").strip()
    include_patterns: list[str] = []
    exclude_patterns: list[str] = []
    error_msg = time_error or ""
    if q and not error_msg:
        include_patterns, exclude_patterns, regex_error = _prepare_re2_filter_patterns(db, q)
        if regex_error:
            error_msg = regex_error
    resolved_ids: set[str] = set()
    if resolved not in ("0", "1"):
        resolved_ids = _get_resolved_error_ids(db)
    where_parts = []
    where_params = []
    if selected_services:
        placeholders = ",".join(["?"] * len(selected_services))
        where_parts.append(f"ServiceName IN ({placeholders})")
        where_params.extend(selected_services)
    _append_time_window_filter(where_parts, where_params, "Timestamp", from_ts, to_ts)
    if q and not error_msg:
        _append_regex_expression_clauses(
            conditions=where_parts,
            params=where_params,
            column="Body",
            include_patterns=include_patterns,
            exclude_patterns=exclude_patterns,
        )
    where_sql = _where_clause(where_parts)

    if grouped_mode:
        # Best-effort deduplication: probe recent raw events, then aggregate in SQL.
        probe_limit = max(2000, min(10000, limit * 100))
        grouped_where_sql = where_sql
        if resolved == "1":
            resolved_condition = f"{error_id_sql} IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)"
            grouped_where_sql = (
                f"{grouped_where_sql} AND {resolved_condition}" if grouped_where_sql else f"WHERE {resolved_condition}"
            )
        elif resolved == "0":
            resolved_condition = f"{error_id_sql} NOT IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)"
            grouped_where_sql = (
                f"{grouped_where_sql} AND {resolved_condition}" if grouped_where_sql else f"WHERE {resolved_condition}"
            )

        grouped_probe_sql = (
            "SELECT "
            "Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes, "
            "substring(replaceRegexpAll(lower(ServiceName), '\\s+', ' '), 1, 220) AS GroupService, "
            "substring("
            "replaceRegexpAll("
            "lower(if(LogAttributes['exception.type'] != '', LogAttributes['exception.type'], 'Error')), "
            "'\\s+', ' '"
            "), 1, 220"
            ") AS GroupType, "
            "substring("
            "replaceRegexpAll("
            "lower(if(LogAttributes['exception.message'] != '', LogAttributes['exception.message'], Body)), "
            "'\\s+', ' '"
            "), 1, 220"
            ") AS GroupMessage "
            f"FROM ({ERROR_SOURCES_SQL}) {grouped_where_sql} "
            "ORDER BY Timestamp DESC LIMIT ?"
        )
        grouped_aggregate_sql = (
            "SELECT "
            "GroupService, GroupType, GroupMessage, "
            "count() AS Count, "
            "min(Timestamp) AS FirstSeen, "
            "max(Timestamp) AS LastSeen, "
            "argMax(Timestamp, Timestamp) AS RepTimestamp, "
            "argMax(ServiceName, Timestamp) AS RepServiceName, "
            "argMax(TraceId, Timestamp) AS RepTraceId, "
            "argMax(SpanId, Timestamp) AS RepSpanId, "
            "argMax(Body, Timestamp) AS RepBody, "
            "argMax(LogAttributes, Timestamp) AS RepLogAttributes, "
            "groupUniqArray(64)(TraceId) AS TraceIds "
            f"FROM ({grouped_probe_sql}) "
            "GROUP BY GroupService, GroupType, GroupMessage"
        )

        total = db.execute(
            f"SELECT COUNT(*) FROM ({grouped_aggregate_sql})",
            where_params + [probe_limit],
        ).fetchone()[0]
        sort_direction = "ASC" if sort_dir == "asc" else "DESC"
        page_sql = f"{grouped_aggregate_sql} ORDER BY {sort_col} {sort_direction} LIMIT ? OFFSET ?"
        group_rows = db.execute(
            page_sql,
            where_params + [probe_limit, limit, offset],
        ).fetchall()
        errors = []
        visible_group_tuples: list[tuple[str, str, str]] = []
        for row in group_rows:
            group_tuple = (
                str(row["GroupService"] or ""),
                str(row["GroupType"] or ""),
                str(row["GroupMessage"] or ""),
            )
            item = _build_error_item(
                {
                    "Timestamp": row["RepTimestamp"],
                    "ServiceName": row["RepServiceName"],
                    "TraceId": row["RepTraceId"],
                    "SpanId": row["RepSpanId"],
                    "Body": row["RepBody"],
                    "LogAttributes": row["RepLogAttributes"],
                }
            )
            if resolved == "1":
                item["resolved"] = True
            elif resolved == "0":
                item["resolved"] = False
            else:
                item["resolved"] = item["id"] in resolved_ids
            item["count"] = int(row["Count"] or 0)
            item["first_seen"] = str(row["FirstSeen"] or item["ts"])
            item["last_seen"] = str(row["LastSeen"] or item["ts"])
            item["group_tuple"] = group_tuple
            visible_group_tuples.append(group_tuple)
            errors.append(item)

        if errors:
            unique_group_tuples: list[tuple[str, str, str]] = []
            seen_group_tuples: set[tuple[str, str, str]] = set()
            for group_tuple in visible_group_tuples:
                if group_tuple in seen_group_tuples:
                    continue
                seen_group_tuples.add(group_tuple)
                unique_group_tuples.append(group_tuple)

            trace_group_params: list[Any] = [*where_params, probe_limit]
            trace_ids_by_group: dict[tuple[str, str, str], list[str]] = {}
            for chunk_start in range(0, len(unique_group_tuples), grouped_trace_chunk_size):
                group_chunk = unique_group_tuples[chunk_start : chunk_start + grouped_trace_chunk_size]
                chunk_params = list(trace_group_params)
                trace_group_placeholders = ", ".join(["(?, ?, ?)"] * len(group_chunk))
                for group_service, group_type, group_message in group_chunk:
                    chunk_params.extend([group_service, group_type, group_message])

                grouped_trace_sql = (
                    "SELECT GroupService, GroupType, GroupMessage, "
                    "arrayStringConcat(groupUniqArray(64)(TraceId), ',') AS TraceIdsCsv "
                    f"FROM ({grouped_probe_sql}) "
                    f"WHERE (GroupService, GroupType, GroupMessage) IN ({trace_group_placeholders}) "
                    "GROUP BY GroupService, GroupType, GroupMessage"
                )
                for row in db.execute(grouped_trace_sql, chunk_params).fetchall():
                    group_tuple = (
                        str(row["GroupService"] or ""),
                        str(row["GroupType"] or ""),
                        str(row["GroupMessage"] or ""),
                    )
                    trace_ids_by_group[group_tuple] = [
                        _hex(value).strip() for value in str(row["TraceIdsCsv"] or "").split(",") if _hex(value).strip()
                    ]

            for item in errors:
                group_tuple = item.pop("group_tuple", ("", "", ""))
                trace_values = list(trace_ids_by_group.get(group_tuple, []))
                primary_trace = str(item.get("trace_id") or "").strip()
                if primary_trace and primary_trace not in trace_values:
                    trace_values.insert(0, primary_trace)
                if trace_values:
                    item["trace_ids"] = trace_values
                    item["trace_ids_csv"] = ",".join(trace_values)
    else:
        order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"
        source_sql = (
            "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes "
            f"FROM ({ERROR_SOURCES_SQL}) {where_sql} "
            f"{order_clause} LIMIT ? OFFSET ?"
        )
        use_resolved_sql_path = resolved in ("0", "1")
        if use_resolved_sql_path:
            error_id_expr = error_id_sql
            poc_where_sql = where_sql
            poc_where_params: list[Any] = list(where_params)
            if resolved == "1":
                resolved_condition = f"{error_id_expr} IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)"
                poc_where_sql = (
                    f"{poc_where_sql} AND {resolved_condition}" if poc_where_sql else f"WHERE {resolved_condition}"
                )
            elif resolved == "0":
                resolved_condition = (
                    f"{error_id_expr} NOT IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)"
                )
                poc_where_sql = (
                    f"{poc_where_sql} AND {resolved_condition}" if poc_where_sql else f"WHERE {resolved_condition}"
                )
            narrow_source_sql = (
                "SELECT "
                "Timestamp, ServiceName, TraceId, SpanId, "
                f"{error_id_expr} AS ErrorId "
                f"FROM ({ERROR_SOURCES_SQL}) {poc_where_sql} "
                f"{order_clause} LIMIT ? OFFSET ?"
            )

            page_rows: list[dict] = []
            count_sql = f"SELECT COUNT(*) FROM ({ERROR_SOURCES_SQL}) {poc_where_sql}"
            total = db.execute(
                count_sql,
                poc_where_params,
            ).fetchone()[0]
            page_rows = [dict(r) for r in db.execute(narrow_source_sql, poc_where_params + [limit, offset]).fetchall()]
            details_by_id: dict[str, dict] = {}
            if page_rows:
                detail_key_tuples: list[tuple[Any, Any, Any, Any]] = []
                seen_detail_keys: set[tuple[Any, Any, Any, Any]] = set()
                for row in page_rows:
                    detail_key = (
                        row.get("Timestamp"),
                        row.get("ServiceName"),
                        row.get("TraceId"),
                        row.get("SpanId"),
                    )
                    if detail_key in seen_detail_keys:
                        continue
                    seen_detail_keys.add(detail_key)
                    detail_key_tuples.append(detail_key)
                for chunk_start in range(0, len(detail_key_tuples), hydrate_key_chunk_size):
                    detail_chunk = detail_key_tuples[chunk_start : chunk_start + hydrate_key_chunk_size]
                    detail_params: list[Any] = []
                    tuple_placeholders = ", ".join(["(?, ?, ?, ?)"] * len(detail_chunk))
                    for ts_val, service_val, trace_val, span_val in detail_chunk:
                        detail_params.extend([ts_val, service_val, trace_val, span_val])
                    detail_sql = (
                        "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes "
                        f"FROM ({ERROR_SOURCES_SQL}) "
                        f"WHERE (Timestamp, ServiceName, TraceId, SpanId) IN ({tuple_placeholders})"
                    )
                    for drow in db.execute(detail_sql, detail_params).fetchall():
                        detail_item = _build_error_item(dict(drow))
                        details_by_id[detail_item["id"]] = detail_item
            errors = []
            for row in page_rows:
                row_id = str(row.get("ErrorId", ""))
                if resolved == "1":
                    resolved_flag = True
                elif resolved == "0":
                    resolved_flag = False
                else:
                    resolved_flag = row_id in resolved_ids
                item = _build_error_stub_from_narrow(row, resolved_flag)
                detail_item = details_by_id.get(item["id"])
                if detail_item:
                    detail_item["resolved"] = resolved_flag
                    item = detail_item
                errors.append(item)
        else:
            total = db.execute(
                f"SELECT COUNT(*) FROM ({ERROR_SOURCES_SQL}) {where_sql}",
                where_params,
            ).fetchone()[0]
            rows = db.execute(source_sql, where_params + [limit, offset]).fetchall()
            errors = []
            for row in rows:
                item = _build_error_item(dict(row))
                item["resolved"] = item["id"] in resolved_ids
                errors.append(item)

    now = time.time()
    services: list[str] = []
    with _errors_cache_lock:
        if float(_errors_services_cache.get("expires_at", 0.0)) > now:
            services = list(_errors_services_cache.get("services", []))

    if not services:
        services = [
            row[0]
            for row in db.execute(
                "SELECT DISTINCT ServiceName FROM ("
                + ERROR_SOURCES_SQL
                + ") WHERE ServiceName!='' ORDER BY ServiceName"
            ).fetchall()
        ]
        with _errors_cache_lock:
            _errors_services_cache["services"] = list(services)
            _errors_services_cache["expires_at"] = now + max(1, ERRORS_SERVICES_CACHE_TTL_SEC)

    work_item_links = _load_work_item_links_for_ref_ids(db, [e["id"] for e in errors])

    return await render_template(
        "errors.html",
        errors=errors,
        total=total,
        limit=limit,
        offset=offset,
        service=service,
        selected_services=selected_services,
        from_ts=from_ts,
        to_ts=to_ts,
        error_msg=error_msg,
        q=q,
        resolved=resolved,
        services=services,
        sort_by=sort_by,
        sort_dir=sort_dir,
        grouped_mode=grouped_mode,
        work_item_links=work_item_links,
    )


@errors_bp.route("/errors/<string:error_id>/resolve", methods=["POST"])
@require_basic_auth
async def resolve_error(error_id: str):
    try:

        def _op(db: ChDbConnection) -> None:
            db.execute("INSERT INTO sobs_error_resolutions(ErrorId) VALUES(?)", (error_id,))

        _queue_write(_op, wait=True)
    except Exception:
        log.exception("resolve error write failed")
        return _json_error("resolve error write failed", 500)
    return jsonify({"ok": True})
