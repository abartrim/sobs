from __future__ import annotations

import logging
import re
from datetime import datetime, timezone
from typing import Any

from quart import Blueprint, render_template, request

import telemetry as _telemetry
from app import (  # noqa: E402
    _active_part_rows,
    _append_regex_expression_clauses,
    _append_time_window_filter,
    _compute_advanced_log_analysis,
    _compute_log_stats,
    _get_cached_log_attr_keys,
    _normalize_ch_timestamp,
    _parse_limit,
    _parse_offset,
    _parse_sort,
    _parse_time_window_args,
    _parse_trace_filter_values,
    _prepare_re2_filter_patterns,
    _public_dashboard_query_error,
    _record_id_for_log,
    _time_window_conditions,
    _validate_user_sql_where,
    _where_clause,
    get_db,
    jsonify,
    masked_jsonify,
    require_basic_auth,
)

log = logging.getLogger("sobs")
logs_bp: Blueprint = Blueprint("logs", __name__)


@logs_bp.route("/logs")
@require_basic_auth
@_telemetry.traced_view("sobs.dashboard.query", **{"dashboard.name": "logs", "route": "/logs"})
async def view_logs():
    db = get_db()
    q = request.args.get("q", "").strip()
    selected_levels = [level_val.strip().upper() for level_val in request.args.getlist("level") if level_val.strip()]
    selected_services = [svc.strip() for svc in request.args.getlist("service") if svc.strip()]
    trace_id = request.args.get("trace_id", "").strip()
    trace_ids, trace_id = _parse_trace_filter_values(trace_id, request.args.getlist("trace_ids"))
    trace_ids_csv = ",".join(trace_ids)
    trace_ids_count = len(trace_ids)
    selected_event_names = [evt.strip() for evt in request.args.getlist("event_name") if evt.strip()]
    event_name = ""  # Keep empty for backward compatibility; use selected_event_names for filtering
    from_ts, to_ts, time_error = _parse_time_window_args()
    sql_where = request.args.get("sql", "").strip()
    run_advanced_analysis = request.args.get("analyze", "").strip() == "1"
    limit = _parse_limit(200)
    offset = _parse_offset()
    sort_by, sort_col, sort_dir = _parse_sort(
        {"Timestamp": "Timestamp", "SeverityText": "SeverityText", "ServiceName": "ServiceName"},
        "Timestamp",
    )
    order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"

    rows = []
    log_rows = []
    total = 0
    error_msg = ""
    level_stats: dict = {}
    service_stats: dict = {}
    advanced_analysis = None
    stats_generated_at_iso = ""
    stats_generated_at_display = ""
    stats_generated_age_s = 0
    where = ""
    params: list = []
    include_patterns: list[str] = []
    exclude_patterns: list[str] = []

    if time_error:
        error_msg = time_error

    if q:
        include_patterns, exclude_patterns, regex_error = _prepare_re2_filter_patterns(db, q)
        if regex_error:
            error_msg = regex_error

    if error_msg:
        pass
    elif sql_where:
        # Allow raw WHERE clause (SQL search)
        try:
            _validate_user_sql_where(sql_where)
            safe_sql = sql_where.replace(";", "")
            safe_sql = re.sub(r"\blevel\b", "SeverityText", safe_sql, flags=re.IGNORECASE)
            safe_sql = re.sub(r"\bservice\b", "ServiceName", safe_sql, flags=re.IGNORECASE)
            safe_sql = re.sub(r"\btrace_id\b", "TraceId", safe_sql, flags=re.IGNORECASE)
            safe_sql = re.sub(r"\bspan_id\b", "SpanId", safe_sql, flags=re.IGNORECASE)
            safe_sql = re.sub(r"\bts\b", "Timestamp", safe_sql, flags=re.IGNORECASE)
            safe_sql = re.sub(r"\bbody\b", "Body", safe_sql, flags=re.IGNORECASE)

            # Translate has_tag('key', 'value') to a correlated subquery.
            # Supports SQL-escaped quotes inside key/value (e.g. O''Reilly).
            def _translate_has_tag(m: re.Match) -> str:
                tag_key = m.group(1).replace("''", "'").replace("'", "''")
                tag_val = m.group(2).replace("''", "'").replace("'", "''")
                return (
                    "MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)) IN ("
                    "SELECT RecordId FROM sobs_record_tags FINAL "
                    f"WHERE TagKey='{tag_key}' AND TagValue='{tag_val}' "
                    "AND IsDeleted=0 AND RecordType='log')"
                )

            safe_sql = re.sub(
                r"has_tag\s*\(\s*'((?:[^']|'')+)'\s*,\s*'((?:[^']|'')*)'\s*\)",
                _translate_has_tag,
                safe_sql,
                flags=re.IGNORECASE,
            )
            where = f"WHERE {safe_sql}"
            time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
            if time_conditions:
                where = f"{where} AND " + " AND ".join(time_conditions)
                params.extend(time_params)
        except Exception as exc:
            error_msg = f"SQL error: {_public_dashboard_query_error(exc)}"
    else:
        conditions = []
        params = []
        if selected_levels:
            placeholders = ",".join(["?"] * len(selected_levels))
            conditions.append(f"SeverityText IN ({placeholders})")
            params.extend(selected_levels)
        if selected_services:
            placeholders = ",".join(["?"] * len(selected_services))
            conditions.append(f"ServiceName IN ({placeholders})")
            params.extend(selected_services)
        if selected_event_names:
            placeholders = ",".join(["?"] * len(selected_event_names))
            conditions.append(f"EventName IN ({placeholders})")
            params.extend(selected_event_names)
        if trace_ids:
            placeholders = ",".join(["?"] * len(trace_ids))
            conditions.append(f"lower(TraceId) IN ({placeholders})")
            params.extend(trace_ids)
        elif trace_id:
            conditions.append("lower(TraceId)=?")
            params.append(trace_id.lower())
        _append_time_window_filter(conditions, params, "Timestamp", from_ts, to_ts)
        where = _where_clause(conditions)

    if not error_msg:
        try:
            query_where = where
            query_params = list(params)
            if q:
                regex_conditions: list[str] = []
                _append_regex_expression_clauses(
                    conditions=regex_conditions,
                    params=query_params,
                    column="Body",
                    include_patterns=include_patterns,
                    exclude_patterns=exclude_patterns,
                )
                if regex_conditions:
                    regex_sql = " AND ".join(regex_conditions)
                    query_where = f"{query_where} AND {regex_sql}" if query_where else f"WHERE {regex_sql}"

            select_base = (
                "SELECT Timestamp, SeverityText, ServiceName, Body, TraceId, SpanId " f"FROM otel_logs {query_where} "
            )

            if not query_where:
                total = _active_part_rows(db, "otel_logs")
            else:
                total = db.execute(f"SELECT COUNT(*) FROM otel_logs {query_where}", query_params).fetchone()[0]
            rows = db.execute(
                f"{select_base}{order_clause} LIMIT ? OFFSET ?",
                query_params + [limit, offset],
            ).fetchall()
            level_stats, service_stats = _compute_log_stats(db, query_where, query_params)
            if run_advanced_analysis:
                analysis_rows = db.execute(
                    f"SELECT SeverityText, ServiceName, Body, LogAttributes FROM otel_logs {query_where}",
                    query_params,
                ).fetchall()
                advanced_analysis = _compute_advanced_log_analysis(analysis_rows, level_stats, service_stats)

            generated_at = datetime.now(timezone.utc)
            snapshot_raw = db.execute(f"SELECT max(Timestamp) FROM otel_logs {query_where}", query_params).fetchone()[0]
            snapshot_at = generated_at
            if snapshot_raw is not None:
                if isinstance(snapshot_raw, datetime):
                    snapshot_at = snapshot_raw
                else:
                    parsed = datetime.fromisoformat(str(snapshot_raw).replace("Z", "+00:00"))
                    snapshot_at = parsed
                if snapshot_at.tzinfo is None:
                    snapshot_at = snapshot_at.replace(tzinfo=timezone.utc)
                else:
                    snapshot_at = snapshot_at.astimezone(timezone.utc)

            stats_generated_at_iso = snapshot_at.isoformat()
            stats_generated_at_display = snapshot_at.strftime("%Y-%m-%d %H:%M:%S UTC")
            stats_generated_age_s = max(0, int((generated_at - snapshot_at).total_seconds()))
        except Exception as exc:
            if sql_where:
                error_msg = f"SQL error: {_public_dashboard_query_error(exc)}"
            else:
                error_msg = f"Query error: {exc}"
            rows = []
            total = 0
            level_stats = {}
            service_stats = {}
            advanced_analysis = None

    # Compute record IDs for visible rows so tags can be batch-fetched
    row_record_ids = [
        _record_id_for_log(str(r["Timestamp"]), str(r["ServiceName"]), str(r["TraceId"]), str(r["SpanId"]))
        for r in rows
    ]
    # Batch-fetch tags for all visible rows in one query
    tags_by_record_id: dict[str, list[dict]] = {}
    tag_stats_count: dict[tuple[str, str], int] = {}
    if row_record_ids:
        try:
            placeholders = ",".join(["?"] * len(row_record_ids))
            tag_rows_raw = db.execute(
                f"SELECT RecordId, TagKey, TagValue, IsAuto "
                f"FROM sobs_record_tags FINAL "
                f"WHERE RecordType='log' AND RecordId IN ({placeholders}) AND IsDeleted=0 "
                f"ORDER BY RecordId, TagKey",
                row_record_ids,
            ).fetchall()
            for tr in tag_rows_raw:
                rid = str(tr["RecordId"])
                entry = {"key": str(tr["TagKey"]), "value": str(tr["TagValue"]), "is_auto": bool(tr["IsAuto"])}
                tags_by_record_id.setdefault(rid, []).append(entry)
                tag_key = str(tr["TagKey"])
                tag_value = str(tr["TagValue"])
                stats_key = (tag_key, tag_value)
                tag_stats_count[stats_key] = tag_stats_count.get(stats_key, 0) + 1
        except Exception:
            pass  # Tags are supplementary; ignore failures

    tag_stats = [
        {"key": k, "value": v, "count": cnt}
        for (k, v), cnt in sorted(tag_stats_count.items(), key=lambda item: (-item[1], item[0][0], item[0][1]))
    ]

    for r in rows:
        body = r["Body"]
        rid = _record_id_for_log(str(r["Timestamp"]), str(r["ServiceName"]), str(r["TraceId"]), str(r["SpanId"]))
        log_rows.append(
            {
                "ts": str(r["Timestamp"]),
                "level": r["SeverityText"],
                "service": r["ServiceName"],
                "body": body,
                "trace_id": r["TraceId"],
                "span_id": r["SpanId"],
                "record_id": rid,
                "tags": tags_by_record_id.get(rid, []),
            }
        )

    services = [
        row[0]
        for row in db.execute(
            "SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName!='' ORDER BY ServiceName"
        ).fetchall()
    ]
    levels = [
        row[0] for row in db.execute("SELECT DISTINCT SeverityText FROM otel_logs ORDER BY SeverityText").fetchall()
    ]
    event_names = [
        row[0]
        for row in db.execute(
            "SELECT DISTINCT EventName FROM otel_logs WHERE EventName!='' ORDER BY EventName"
        ).fetchall()
    ]

    return await render_template(
        "logs.html",
        logs=log_rows,
        total=total,
        limit=limit,
        offset=offset,
        q=q,
        level="",  # Keep empty for backward compatibility; use selected_levels for filtering
        selected_levels=selected_levels,
        service="",  # Keep empty for backward compatibility; use selected_services for filtering
        selected_services=selected_services,
        trace_id=trace_id,
        trace_ids_csv=trace_ids_csv,
        trace_ids_count=trace_ids_count,
        sql_where=sql_where,
        from_ts=from_ts,
        to_ts=to_ts,
        services=services,
        levels=levels,
        event_names=event_names,
        event_name=event_name,
        selected_event_names=selected_event_names,
        error_msg=error_msg,
        sort_by=sort_by,
        sort_dir=sort_dir,
        run_advanced_analysis=run_advanced_analysis,
        level_stats=level_stats,
        service_stats=service_stats,
        tag_stats=tag_stats,
        advanced_analysis=advanced_analysis,
        stats_generated_at_iso=stats_generated_at_iso,
        stats_generated_at_display=stats_generated_at_display,
        stats_generated_age_s=stats_generated_age_s,
    )


@logs_bp.route("/api/logs/field-hints", methods=["GET"])
@require_basic_auth
async def api_logs_field_hints():
    db = get_db()

    fields = [
        {"name": "level", "column": "SeverityText", "type": "string", "values": []},
        {"name": "service", "column": "ServiceName", "type": "string", "values": []},
        {"name": "body", "column": "Body", "type": "string", "values": []},
        {"name": "trace_id", "column": "TraceId", "type": "string", "values": []},
        {"name": "span_id", "column": "SpanId", "type": "string", "values": []},
        {"name": "ts", "column": "Timestamp", "type": "datetime", "values": []},
        {"name": "EventName", "column": "EventName", "type": "string", "values": []},
        {"name": "ScopeName", "column": "ScopeName", "type": "string", "values": []},
    ]

    attr_keys = _get_cached_log_attr_keys(db, record_type="log")

    # Active tag keys for logs (used in has_tag() suggestions)
    try:
        tag_key_rows = db.execute(
            "SELECT DISTINCT TagKey FROM sobs_record_tags FINAL "
            "WHERE RecordType='log' AND IsDeleted=0 ORDER BY TagKey LIMIT 100"
        ).fetchall()
        tag_keys = [str(r[0]) for r in tag_key_rows]
        # For each tag key, also fetch distinct values (cap at 20)
        tag_values: dict[str, list[str]] = {}
        for tk in tag_keys:
            val_rows = db.execute(
                "SELECT DISTINCT TagValue FROM sobs_record_tags FINAL "
                "WHERE RecordType='log' AND TagKey=? AND IsDeleted=0 ORDER BY TagValue LIMIT 20",
                [tk],
            ).fetchall()
            tag_values[tk] = [str(r[0]) for r in val_rows]
    except Exception:
        tag_keys = []
        tag_values = {}

    operators = ["=", "!=", "LIKE", "NOT LIKE", "ILIKE", "NOT ILIKE", "IN", "NOT IN", ">", "<", ">=", "<="]
    keywords = ["AND", "OR", "NOT", "IS NULL", "IS NOT NULL", "TRUE", "FALSE", "NULL"]
    functions = [
        {"name": "has_tag", "signature": "has_tag('key','value')", "kind": "tag"},
        {"name": "match", "signature": "match(body, 'regex')", "kind": "string"},
        {"name": "positionCaseInsensitive", "signature": "positionCaseInsensitive(body, 'needle')", "kind": "string"},
        {"name": "startsWith", "signature": "startsWith(service, 'api')", "kind": "string"},
        {"name": "endsWith", "signature": "endsWith(service, 'worker')", "kind": "string"},
        {"name": "lower", "signature": "lower(service)", "kind": "string"},
        {"name": "upper", "signature": "upper(level)", "kind": "string"},
        {"name": "toString", "signature": "toString(ts)", "kind": "cast"},
        {"name": "toDateTime", "signature": "toDateTime('2026-03-30 12:00:00')", "kind": "datetime"},
    ]
    snippets = [
        {"label": "level='ERROR'", "insert": "level='ERROR'", "kind": "predicate"},
        {"label": "service IN ('api','worker')", "insert": "service IN ('api','worker')", "kind": "predicate"},
        {"label": "has_tag('env','prod')", "insert": "has_tag('env','prod')", "kind": "predicate"},
        {"label": "match(body, 'timeout')", "insert": "match(body, 'timeout')", "kind": "predicate"},
        {
            "label": "ts >= toDateTime('2026-03-30 00:00:00')",
            "insert": "ts >= toDateTime('2026-03-30 00:00:00')",
            "kind": "predicate",
        },
    ]

    return jsonify(
        {
            "fields": fields,
            "attr_keys": attr_keys,
            "tag_keys": tag_keys,
            "tag_values": tag_values,
            "operators": operators,
            "keywords": keywords,
            "functions": functions,
            "snippets": snippets,
        }
    )


@logs_bp.route("/api/logs/validate-filter", methods=["POST"])
@require_basic_auth
async def api_logs_validate_filter():
    """Validate a SQL WHERE fragment used by /logs?sql=... and return actionable feedback."""
    payload = await request.get_json(silent=True)
    sql_where = str((payload or {}).get("sql", "") or "").strip()
    if not sql_where:
        return jsonify({"ok": True, "normalized": "", "issues": []})

    issues: list[dict[str, str]] = []

    # Lightweight structural checks for instant, helpful feedback.
    quote_open = False
    paren_depth = 0
    i = 0
    while i < len(sql_where):
        ch = sql_where[i]
        if ch == "'":
            if i + 1 < len(sql_where) and sql_where[i + 1] == "'":
                i += 2
                continue
            quote_open = not quote_open
        elif not quote_open:
            if ch == "(":
                paren_depth += 1
            elif ch == ")":
                paren_depth -= 1
                if paren_depth < 0:
                    issues.append({"level": "error", "message": "Unexpected ')' in filter."})
                    break
        i += 1

    if quote_open:
        issues.append({"level": "error", "message": "Unclosed single quote in filter."})
    if paren_depth > 0:
        issues.append({"level": "error", "message": "Unclosed '(' in filter."})
    if re.search(r"\b(AND|OR|NOT|IN|LIKE|ILIKE)\s*$", sql_where, re.IGNORECASE):
        issues.append({"level": "warning", "message": "Filter ends with an operator or keyword."})

    try:
        _validate_user_sql_where(sql_where)
        safe_sql = sql_where.replace(";", "")
        safe_sql = re.sub(r"\blevel\b", "SeverityText", safe_sql, flags=re.IGNORECASE)
        safe_sql = re.sub(r"\bservice\b", "ServiceName", safe_sql, flags=re.IGNORECASE)
        safe_sql = re.sub(r"\btrace_id\b", "TraceId", safe_sql, flags=re.IGNORECASE)
        safe_sql = re.sub(r"\bspan_id\b", "SpanId", safe_sql, flags=re.IGNORECASE)
        safe_sql = re.sub(r"\bts\b", "Timestamp", safe_sql, flags=re.IGNORECASE)
        safe_sql = re.sub(r"\bbody\b", "Body", safe_sql, flags=re.IGNORECASE)

        def _translate_has_tag(m: re.Match) -> str:
            tag_key = m.group(1).replace("''", "'").replace("'", "''")
            tag_val = m.group(2).replace("''", "'").replace("'", "''")
            return (
                "MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)) IN ("
                "SELECT RecordId FROM sobs_record_tags FINAL "
                f"WHERE TagKey='{tag_key}' AND TagValue='{tag_val}' "
                "AND IsDeleted=0 AND RecordType='log')"
            )

        safe_sql = re.sub(
            r"has_tag\s*\(\s*'((?:[^']|'')+)'\s*,\s*'((?:[^']|'')*)'\s*\)",
            _translate_has_tag,
            safe_sql,
            flags=re.IGNORECASE,
        )

        db = get_db()
        # Existence probe is much cheaper than aggregate count() for live typing validation.
        db.execute(f"SELECT 1 FROM otel_logs WHERE {safe_sql} LIMIT 1").fetchone()
    except Exception as exc:
        issues.append({"level": "error", "message": _public_dashboard_query_error(exc)})
        return jsonify({"ok": False, "normalized": "", "issues": issues}), 200

    return jsonify({"ok": True, "normalized": safe_sql, "issues": issues})


# ---------------------------------------------------------------------------
# Regex Validate API helpers
# ---------------------------------------------------------------------------
_REGEX_SAMPLE_MAX_LEN = 200
_REGEX_SCOPE_MAX_LEN = 200
_REGEX_VALIDATE_RECENT_HOURS = 24
_REGEX_VALIDATE_CANDIDATE_LIMIT = 2000


def _truncate_sample(sample: str | None) -> str | None:
    """Truncate a regex sample match to a displayable length."""
    if sample and len(sample) > _REGEX_SAMPLE_MAX_LEN:
        return f"{sample[:_REGEX_SAMPLE_MAX_LEN - 3]}..."
    return sample


def _regex_scope_text(scope: dict[str, Any], key: str, max_len: int = _REGEX_SCOPE_MAX_LEN) -> str:
    """Read a bounded text value from regex validation scope payload."""
    raw = str((scope or {}).get(key, "") or "").strip()
    if not raw:
        return ""
    return raw[:max_len]


def _regex_scope_time_conditions(scope: dict[str, Any], column: str) -> tuple[list[str], list[Any]]:
    """Use requested time window when valid; otherwise default to a recent bounded window."""
    from_ts = ""
    to_ts = ""

    from_raw = _regex_scope_text(scope, "from_ts", 64)
    to_raw = _regex_scope_text(scope, "to_ts", 64)
    if from_raw:
        try:
            from_ts = _normalize_ch_timestamp(from_raw)
        except Exception:
            from_ts = ""
    if to_raw:
        try:
            to_ts = _normalize_ch_timestamp(to_raw)
        except Exception:
            to_ts = ""

    conditions, params = _time_window_conditions(column, from_ts, to_ts)
    if not conditions:
        return [f"{column} >= now() - INTERVAL ? HOUR"], [_REGEX_VALIDATE_RECENT_HOURS]
    return conditions, params


def _parse_and_validate_regex_expression_for_api(db: Any, expression: str) -> tuple[list[str], list[str], str | None]:
    include_patterns, exclude_patterns, regex_error = _prepare_re2_filter_patterns(db, expression)
    if regex_error:
        return [], [], regex_error.replace("Regex error: ", "", 1)
    return include_patterns, exclude_patterns, None


def _regex_best_effort_sample(
    db: Any,
    *,
    from_sql: str,
    sample_column: str,
    order_column: str,
    include_patterns: list[str],
    exclude_patterns: list[str],
    where_parts: list[str],
    where_params: list[Any],
) -> str | None:
    """Return a bounded sample match by probing only recent candidate rows."""
    where_sql = ("WHERE " + " AND ".join(where_parts)) if where_parts else ""
    regex_conditions: list[str] = []
    regex_params: list[Any] = []
    _append_regex_expression_clauses(
        conditions=regex_conditions,
        params=regex_params,
        column="sample_value",
        include_patterns=include_patterns,
        exclude_patterns=exclude_patterns,
    )
    regex_where_sql = ("WHERE " + " AND ".join(regex_conditions)) if regex_conditions else ""
    sql = (
        "SELECT sample_value FROM ("
        f"SELECT {sample_column} AS sample_value FROM {from_sql} "
        f"{where_sql} ORDER BY {order_column} DESC LIMIT ?"
        ") "
        f"{regex_where_sql} LIMIT 1"
    )
    params = [*where_params, _REGEX_VALIDATE_CANDIDATE_LIMIT, *regex_params]
    row = db.execute(sql, params).fetchone()
    return _truncate_sample(row[0] if row else None)


# ---------------------------------------------------------------------------
# Logs Regex Validate API  POST /api/logs/validate-regex
# Used by the regex autocomplete / IntelliSense on the Logs filter panel.
# ---------------------------------------------------------------------------
@logs_bp.route("/api/logs/validate-regex", methods=["POST"])
@require_basic_auth
async def api_logs_validate_regex():
    """Validate a regex pattern used by /logs?q=... and return a sample match."""
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

    # Attempt a cheap LIMIT 1 probe to surface a real sample match.
    try:
        where_parts: list[str] = []
        where_params: list[Any] = []

        service = _regex_scope_text(scope, "service")
        level = _regex_scope_text(scope, "level")
        trace_id = _regex_scope_text(scope, "trace_id", 64)

        if service:
            where_parts.append("ServiceName = ?")
            where_params.append(service)
        if level:
            where_parts.append("SeverityText = ?")
            where_params.append(level)
        if trace_id:
            where_parts.append("TraceId = ?")
            where_params.append(trace_id)

        time_parts, time_params = _regex_scope_time_conditions(scope, "Timestamp")
        where_parts.extend(time_parts)
        where_params.extend(time_params)

        sample = _regex_best_effort_sample(
            db,
            from_sql="otel_logs",
            sample_column="Body",
            order_column="Timestamp",
            include_patterns=include_patterns,
            exclude_patterns=_exclude_patterns,
            where_parts=where_parts,
            where_params=where_params,
        )
        return masked_jsonify({"ok": True, "sample": sample})
    except Exception:
        return masked_jsonify({"ok": True, "sample": None})
