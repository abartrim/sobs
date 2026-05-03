"""Miscellaneous routes blueprint (summary, incidents, work items, health, settings, etc.)."""

from __future__ import annotations

import asyncio
import json
import logging
import time
from datetime import datetime, timedelta
from typing import Any

from quart import Blueprint, Response, jsonify, render_template, request

from app import (  # noqa: E402
    _AI_SPAN_CONDITION,
    _CVE_ENABLED_SETTING,
    _CVE_LAST_SCAN_SETTING,
    _INCIDENT_MAX_RELATED_ERRORS,
    _INCIDENT_MAX_RELATED_RUM_EVENTS,
    _INCIDENT_WINDOW_DEFAULT_MINUTES,
    _INCIDENT_WINDOW_MAX_MINUTES,
    _QUERY_ALLOWED_TABLES,
    _RUM_SESSION_KEY_SQL,
    _SSE_QUEUE_MAXSIZE,
    ERROR_SOURCES_SQL,
    SUMMARY_STATS_CACHE_TTL_SEC,
    WORK_ITEMS_FILTER_CACHE_TTL_SEC,
    WORK_ITEMS_PAGE_CACHE_TTL_SEC,
    _active_part_rows,
    _build_error_item,
    _build_rum_event_item,
    _build_user_issue_trigger_context,
    _error_id_sql_expr,
    _fetch_trace_metric_context,
    _get_app_setting,
    _get_resolved_error_ids,
    _get_signal_health_by_service,
    _list_trace_overlapping_raw_windows,
    _load_agent_rules,
    _load_all_ai_settings,
    _load_anomaly_rules,
    _load_k8s_settings,
    _load_masking_settings,
    _load_notification_channels,
    _load_notification_rules,
    _load_tag_rules,
    _load_work_item_links_for_ref_ids,
    _maybe_await,
    _maybe_backfill_github_work_item_links,
    _normalize_ch_timestamp,
    _parse_bool,
    _parse_issue_ref_from_url,
    _parse_limit,
    _parse_offset,
    _parse_time_window_args,
    _public_dashboard_query_error,
    _resolve_agent_github_target,
    _run_agent_rule_instance,
    _serialize_github_work_item_row,
    _sse_subscribers,
    _summary_stats_cache,
    _summary_stats_cache_lock,
    _time_window_conditions,
    _ts_str_to_epoch_ms,
    _work_items_cache_lock,
    _work_items_filter_cache,
    _work_items_page_cache,
    _write_queue_depth,
    ensure_db_schema,
    get_db,
    masked_jsonify,
    require_basic_auth,
)
from routes.logs import (  # noqa: E402
    _parse_and_validate_regex_expression_for_api,
    _regex_best_effort_sample,
    _regex_scope_text,
    _regex_scope_time_conditions,
)

misc_bp = Blueprint("misc", __name__)
log = logging.getLogger("sobs")


@misc_bp.route("/")
@require_basic_auth
async def summary():
    db = get_db()
    error_id_sql = _error_id_sql_expr()
    unresolved_condition = f"{error_id_sql} NOT IN (SELECT ErrorId FROM sobs_error_resolutions GROUP BY ErrorId)"

    recent_errors = []
    for row in db.execute(
        "SELECT Timestamp, ServiceName, TraceId, SpanId, Body, LogAttributes "
        f"FROM ({ERROR_SOURCES_SQL}) "
        "WHERE Timestamp >= now() - INTERVAL 48 HOUR "
        f"AND {unresolved_condition} "
        "ORDER BY Timestamp DESC "
        "LIMIT 5"
    ).fetchall():
        item = _build_error_item(dict(row))
        recent_errors.append(
            {
                "id": item["id"],
                "ts": item["ts"],
                "service": item["service"],
                "err_type": item["err_type"],
                "message": item["message"],
            }
        )

    _now = time.monotonic()
    with _summary_stats_cache_lock:
        _cached_stats: dict[str, Any] = (
            _summary_stats_cache["data"] if _summary_stats_cache["expires_at"] > _now else {}
        )
    if not _cached_stats:
        errors_total = db.execute(f"SELECT count() AS cnt FROM ({ERROR_SOURCES_SQL})").fetchone()
        unresolved_total_row = db.execute(
            f"SELECT count() AS cnt FROM ({ERROR_SOURCES_SQL}) WHERE {unresolved_condition}"
        ).fetchone()

        _cached_stats = {
            "logs": _active_part_rows(db, "otel_logs"),
            "spans": _active_part_rows(db, "otel_traces"),
            "rum": _active_part_rows(db, "hyperdx_sessions"),
            "ai": db.execute("SELECT COUNT(*) FROM otel_traces " f"WHERE {_AI_SPAN_CONDITION}").fetchone()[0],
            "errors_total": int(errors_total["cnt"]) if errors_total else 0,
            "errors": int(unresolved_total_row["cnt"]) if unresolved_total_row else 0,
            "services": [
                r[0]
                for r in db.execute(
                    "SELECT DISTINCT ServiceName FROM otel_logs WHERE ServiceName!='' "
                    "UNION DISTINCT SELECT DISTINCT ServiceName FROM otel_traces WHERE ServiceName!='' "
                    "UNION DISTINCT SELECT DISTINCT ServiceName FROM hyperdx_sessions WHERE ServiceName!=''"
                ).fetchall()
            ],
        }
        with _summary_stats_cache_lock:
            _summary_stats_cache["expires_at"] = _now + SUMMARY_STATS_CACHE_TTL_SEC
            _summary_stats_cache["data"] = _cached_stats
    stats = {}

    # Recent logs (last 10)
    recent_logs = []
    for r in db.execute(
        "SELECT Timestamp, SeverityText, ServiceName, Body FROM otel_logs ORDER BY Timestamp DESC LIMIT 10"
    ).fetchall():
        recent_logs.append(
            {
                "ts": str(r["Timestamp"]),
                "level": r["SeverityText"],
                "service": r["ServiceName"],
                "body": r["Body"],
            }
        )
    # RUM summary – page views last 24h
    rum_summary = db.execute(
        "SELECT EventName, COUNT(*) as cnt FROM hyperdx_sessions GROUP BY EventName ORDER BY cnt DESC"
    ).fetchall()
    # AI summary
    ai_summary = db.execute(
        "SELECT SpanAttributes['gen_ai.request.model'] AS model, "
        "COUNT(*) cnt, "
        "SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.input_tokens'])) ti, "
        "SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.output_tokens'])) to_ "
        "FROM otel_traces "
        f"WHERE {_AI_SPAN_CONDITION} "
        "GROUP BY model"
    ).fetchall()

    # CVE summary for Summary page security panel.
    cve_enabled = (_get_app_setting(db, _CVE_ENABLED_SETTING) or "true").lower() in ("1", "true", "yes")
    cve_last_scan = _get_app_setting(db, _CVE_LAST_SCAN_SETTING) or ""
    cve_overview = {
        "enabled": cve_enabled,
        "last_scan": cve_last_scan,
        "total": 0,
        "critical": 0,
        "high": 0,
        "medium": 0,
        "low": 0,
    }
    if cve_enabled:
        try:
            cve_rows = db.execute(
                "SELECT Severity, COUNT(*) AS cnt FROM sobs_cve_findings FINAL GROUP BY Severity"
            ).fetchall()
            total = 0
            for row in cve_rows:
                sev = str(row["Severity"] or "").upper()
                cnt = int(row["cnt"])
                total += cnt
                if sev == "CRITICAL":
                    cve_overview["critical"] += cnt
                elif sev == "HIGH":
                    cve_overview["high"] += cnt
                elif sev == "MEDIUM":
                    cve_overview["medium"] += cnt
                elif sev == "LOW":
                    cve_overview["low"] += cnt
            cve_overview["total"] = total
        except Exception:
            log.exception("summary cve overview query failed")

    return await render_template(
        "summary.html",
        stats=stats,
        recent_errors=recent_errors,
        recent_logs=recent_logs,
        rum_summary=rum_summary,
        ai_summary=ai_summary,
        signal_health=_get_signal_health_by_service(db),
        cve_overview=cve_overview,
    )


@misc_bp.route("/incident")
@require_basic_auth
async def view_incident():
    db = get_db()
    trace_id = request.args.get("trace_id", "").strip()
    error_id = request.args.get("error_id", "").strip()
    rum_session = request.args.get("rum_session", "").strip()
    rum_ts = request.args.get("rum_ts", "").strip()
    from_ts, to_ts, time_error = _parse_time_window_args()

    try:
        _wm_raw = request.args.get("window_minutes", "").strip()
        _wm_int = int(_wm_raw) if _wm_raw else _INCIDENT_WINDOW_DEFAULT_MINUTES
        window_minutes = max(1, min(_INCIDENT_WINDOW_MAX_MINUTES, _wm_int))
    except (TypeError, ValueError):
        window_minutes = _INCIDENT_WINDOW_DEFAULT_MINUTES

    if not trace_id and not error_id and not rum_session:
        return await render_template(
            "incident.html",
            trace_id="",
            error_id="",
            rum_session="",
            rum_ts="",
            primary_error=None,
            primary_trace=None,
            primary_rum=None,
            service="",
            from_ts="",
            to_ts="",
            window_minutes=window_minutes,
            related_errors=[],
            related_log_count=0,
            related_span_count=0,
            related_rum_count=0,
            related_rum_sessions=0,
            related_rum_error_count=0,
            related_rum_events=[],
            raw_windows=[],
            metrics_context={
                "source_mode": "none",
                "total_points": 0,
                "series": [],
                "match_mode": "none",
                "match_label": "no match",
                "match_dimensions": [],
            },
            anomaly_state=None,
            work_item_links={},
            time_error="",
            error_msg="No incident reference provided. Specify trace_id, error_id, or rum_session.",
        )

    # ── Resolve primary error ───────────────────────────────────────────────
    primary_error: dict | None = None
    if error_id:
        try:
            scan_limit = 5000
            err_rows = db.execute(
                f"SELECT * FROM ({ERROR_SOURCES_SQL}) ORDER BY Timestamp DESC LIMIT ?",
                [scan_limit],
            ).fetchall()
            resolved_ids = _get_resolved_error_ids(db)
            for row in err_rows:
                candidate = _build_error_item(dict(row))
                if candidate["id"] == error_id:
                    candidate["resolved"] = candidate["id"] in resolved_ids
                    primary_error = candidate
                    break
        except Exception as exc:
            log.warning("view_incident: failed to look up error_id %s: %s", error_id, exc)

    # ── Resolve primary trace (root span summary) ───────────────────────────
    primary_trace: dict | None = None
    if trace_id:
        try:
            span_rows = db.execute(
                "SELECT Timestamp, TraceId, SpanId, ParentSpanId, SpanName, ServiceName, "
                "Duration, StatusCode, SpanAttributes "
                "FROM otel_traces WHERE TraceId=? ORDER BY Timestamp ASC",
                [trace_id],
            ).fetchall()
            if span_rows:
                services = sorted({str(r["ServiceName"]) for r in span_rows if r["ServiceName"]})
                root = span_rows[0]
                start_ms = _ts_str_to_epoch_ms(str(root["Timestamp"]))
                end_ms = max(
                    _ts_str_to_epoch_ms(str(r["Timestamp"])) + round(float(r["Duration"]) / 1_000_000, 2)
                    for r in span_rows
                )
                primary_trace = {
                    "trace_id": trace_id,
                    "services": services,
                    "service": services[0] if services else "",
                    "span_count": len(span_rows),
                    "start_ts": str(root["Timestamp"]),
                    "start_ms": round(start_ms),
                    "end_ms": round(end_ms),
                    "total_ms": round(end_ms - start_ms, 2),
                    "root_name": str(root["SpanName"]),
                    "status": str(root["StatusCode"]),
                }
        except Exception as exc:
            log.warning("view_incident: failed to look up trace_id %s: %s", trace_id, exc)

    # ── Resolve primary RUM event (session-scoped fallback) ─────────────────
    primary_rum: dict | None = None
    if rum_session:
        try:
            rum_where_parts = [f"{_RUM_SESSION_KEY_SQL}=?"]
            rum_where_params: list[str] = [rum_session]
            if rum_ts:
                rum_where_parts.append("Timestamp <= parseDateTime64BestEffort(?, 9)")
                rum_where_params.append(rum_ts)
            rum_where_sql = "WHERE " + " AND ".join(rum_where_parts)
            rum_row = db.execute(
                "SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId, ServiceName "
                f"FROM hyperdx_sessions {rum_where_sql} "
                "ORDER BY Timestamp DESC LIMIT 1",
                rum_where_params,
            ).fetchone()
            if rum_row:
                primary_rum = _build_rum_event_item(rum_row)
        except Exception as exc:
            log.warning("view_incident: failed to look up rum_session %s: %s", rum_session, exc)

    # ── Determine primary service and event timestamp ───────────────────────
    service = ""
    event_ts = ""
    if primary_error:
        service = primary_error.get("service", "")
        event_ts = primary_error.get("ts", "")
    elif primary_trace:
        service = primary_trace.get("service", "")
        event_ts = primary_trace.get("start_ts", "")
    elif primary_rum:
        service = str(primary_rum.get("service", "") or "")
        event_ts = str(primary_rum.get("ts", "") or "")

    # ── Expand time window around event if caller did not supply one ────────
    if event_ts and not (from_ts and to_ts) and not time_error:
        try:
            dt = datetime.fromisoformat(event_ts.replace(" ", "T").rstrip("Z") + "+00:00")
            half = timedelta(minutes=window_minutes // 2)
            from_ts = _normalize_ch_timestamp(dt - half)
            to_ts = _normalize_ch_timestamp(dt + half)
        except (TypeError, ValueError):
            pass

    # ── Gather related errors ───────────────────────────────────────────────
    related_errors: list[dict] = []
    try:
        where_parts: list[str] = []
        where_params: list[str] = []
        if trace_id:
            where_parts.append("TraceId=?")
            where_params.append(trace_id)
        elif service:
            where_parts.append("ServiceName=?")
            where_params.append(service)
        tc, tp = _time_window_conditions("Timestamp", from_ts, to_ts)
        where_parts.extend(tc)
        where_params.extend(tp)
        where_sql = ("WHERE " + " AND ".join(where_parts)) if where_parts else ""
        err_rows = db.execute(
            f"SELECT * FROM ({ERROR_SOURCES_SQL}) {where_sql} " f"ORDER BY Timestamp DESC LIMIT ?",
            where_params + [_INCIDENT_MAX_RELATED_ERRORS + 1],
        ).fetchall()
        resolved_ids = _get_resolved_error_ids(db)
        primary_error_id = primary_error["id"] if primary_error else ""
        for row in err_rows[:_INCIDENT_MAX_RELATED_ERRORS]:
            item = _build_error_item(dict(row))
            item["resolved"] = item["id"] in resolved_ids
            if item["id"] != primary_error_id:
                related_errors.append(item)
        related_errors_truncated = len(err_rows) > _INCIDENT_MAX_RELATED_ERRORS
    except Exception as exc:
        log.warning("view_incident: failed to fetch related errors: %s", exc)
        related_errors_truncated = False

    # ── Count related logs ──────────────────────────────────────────────────
    related_log_count = 0
    try:
        log_where_parts: list[str] = []
        log_where_params: list[str] = []
        if trace_id:
            log_where_parts.append("TraceId=?")
            log_where_params.append(trace_id)
        elif service:
            log_where_parts.append("ServiceName=?")
            log_where_params.append(service)
        tc, tp = _time_window_conditions("Timestamp", from_ts, to_ts)
        log_where_parts.extend(tc)
        log_where_params.extend(tp)
        log_where_sql = ("WHERE " + " AND ".join(log_where_parts)) if log_where_parts else ""
        row_cnt = db.execute(
            f"SELECT count() AS cnt FROM otel_logs {log_where_sql}",
            log_where_params,
        ).fetchone()
        related_log_count = int(row_cnt["cnt"]) if row_cnt else 0
    except Exception as exc:
        log.warning("view_incident: failed to count related logs: %s", exc)

    # ── Count related spans ─────────────────────────────────────────────────
    related_span_count = 0
    try:
        if service:
            span_where_parts: list[str] = ["ServiceName=?"]
            span_where_params: list[str] = [service]
            tc, tp = _time_window_conditions("Timestamp", from_ts, to_ts)
            span_where_parts.extend(tc)
            span_where_params.extend(tp)
            span_where_sql = "WHERE " + " AND ".join(span_where_parts)
            row_cnt = db.execute(
                f"SELECT count() AS cnt FROM otel_traces {span_where_sql}",
                span_where_params,
            ).fetchone()
            related_span_count = int(row_cnt["cnt"]) if row_cnt else 0
    except Exception as exc:
        log.warning("view_incident: failed to count related spans: %s", exc)

    # ── RUM evidence summary ───────────────────────────────────────────────
    related_rum_count = 0
    related_rum_sessions = 0
    related_rum_error_count = 0
    related_rum_events: list[dict[str, Any]] = []
    try:
        rum_where_parts: list[str] = []
        rum_where_params: list[str] = []
        if trace_id:
            rum_where_parts.append("TraceId=?")
            rum_where_params.append(trace_id)
        elif service:
            rum_where_parts.append("(LogAttributes['service.name']=? OR LogAttributes['service']=?)")
            rum_where_params.extend([service, service])
        tc, tp = _time_window_conditions("Timestamp", from_ts, to_ts)
        rum_where_parts.extend(tc)
        rum_where_params.extend(tp)
        rum_where_sql = ("WHERE " + " AND ".join(rum_where_parts)) if rum_where_parts else ""

        rum_summary_row = db.execute(
            "SELECT "
            "count() AS ev_count, "
            f"uniq({_RUM_SESSION_KEY_SQL}) AS session_count, "
            "countIf(EventName IN ('error', 'unhandledrejection')) AS err_count "
            f"FROM hyperdx_sessions {rum_where_sql}",
            rum_where_params,
        ).fetchone()
        if rum_summary_row:
            related_rum_count = int(rum_summary_row["ev_count"])
            related_rum_sessions = int(rum_summary_row["session_count"])
            related_rum_error_count = int(rum_summary_row["err_count"])

        rum_rows = db.execute(
            "SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId, ServiceName "
            f"FROM hyperdx_sessions {rum_where_sql} "
            "ORDER BY Timestamp DESC LIMIT ?",
            rum_where_params + [_INCIDENT_MAX_RELATED_RUM_EVENTS],
        ).fetchall()
        related_rum_events = [_build_rum_event_item(row) for row in rum_rows]
    except Exception as exc:
        log.warning("view_incident: failed to fetch related RUM evidence: %s", exc)

    # ── Overlapping preserved raw windows + metric context ─────────────────
    raw_windows: list[dict[str, object]] = []
    metrics_context: dict[str, object] = {
        "source_mode": "none",
        "total_points": 0,
        "series": [],
        "match_mode": "none",
        "match_label": "no match",
        "match_dimensions": [],
    }
    try:
        if from_ts and to_ts:
            service_names = [service] if service else []
            raw_windows = _list_trace_overlapping_raw_windows(
                db,
                service_names=service_names,
                start_ts=from_ts,
                end_ts=to_ts,
                limit=25,
            )
            metrics_context = _fetch_trace_metric_context(
                db,
                service_names=service_names,
                start_ts=from_ts,
                end_ts=to_ts,
                window_ids=[str(w.get("id") or "") for w in raw_windows if str(w.get("id") or "")],
                namespace_values=[],
                pod_values=[],
                node_values=[],
                deployment_values=[],
            )
    except Exception as exc:
        log.warning("view_incident: failed to fetch window/metrics context: %s", exc)

    # ── Service anomaly state ───────────────────────────────────────────────
    anomaly_state: str | None = None
    try:
        if service:
            anomaly_row = db.execute(
                "SELECT anomaly_state FROM v_derived_signals_anomaly "
                "WHERE ServiceName=? AND SignalSource='traces' "
                "AND time >= now() - INTERVAL 48 HOUR "
                "ORDER BY time DESC LIMIT 1",
                [service],
            ).fetchone()
            if anomaly_row:
                anomaly_state = str(anomaly_row["anomaly_state"])
    except Exception as exc:
        log.warning("view_incident: failed to fetch anomaly state for service %s: %s", service, exc)

    # ── Work item links ─────────────────────────────────────────────────────
    ref_ids: list[str] = []
    if primary_error:
        ref_ids.append(primary_error["id"])
    elif error_id:
        ref_ids.append(error_id)
    if trace_id:
        ref_ids.append(trace_id)
    if rum_session:
        ref_ids.append(rum_session)
    work_item_links = _load_work_item_links_for_ref_ids(db, ref_ids)

    # ── Resolve best existing work item for the raise-issue button ──────────
    existing_work_item: dict | None = None
    for ref in ref_ids:
        wi = work_item_links.get(ref)
        if wi and wi.get("issue_url"):
            existing_work_item = wi
            break

    return await render_template(
        "incident.html",
        trace_id=trace_id,
        error_id=error_id,
        rum_session=rum_session,
        rum_ts=rum_ts,
        primary_error=primary_error,
        primary_trace=primary_trace,
        primary_rum=primary_rum,
        service=service,
        from_ts=from_ts,
        to_ts=to_ts,
        window_minutes=window_minutes,
        related_errors=related_errors,
        related_errors_truncated=related_errors_truncated,
        related_log_count=related_log_count,
        related_span_count=related_span_count,
        related_rum_count=related_rum_count,
        related_rum_sessions=related_rum_sessions,
        related_rum_error_count=related_rum_error_count,
        related_rum_events=related_rum_events,
        raw_windows=raw_windows,
        metrics_context=metrics_context,
        anomaly_state=anomaly_state,
        work_item_links=work_item_links,
        existing_work_item=existing_work_item,
        time_error=time_error,
        error_msg=time_error or "",
    )


@misc_bp.route("/work-items")
@require_basic_auth
async def view_work_items():
    """Display work items created by agent rules."""
    db = get_db()

    # Filters
    service_filter = request.args.get("service", "").strip()
    rule_filter = request.args.get("rule_name", "").strip()
    action_type_filter = request.args.get("action_type", "").strip()
    status_filter = request.args.get("status", "").strip()
    from_ts, to_ts, time_error = _parse_time_window_args()

    # Build query
    conditions = ["IsDeleted = 0"]
    params = []

    if service_filter:
        conditions.append("ServiceName = ?")
        params.append(service_filter)
    if rule_filter:
        conditions.append("AgentRuleName = ?")
        params.append(rule_filter)
    if action_type_filter:
        conditions.append("AgentAction = ?")
        params.append(action_type_filter)
    if status_filter:
        conditions.append("IssueState = ?")
        params.append(status_filter)
    if from_ts:
        conditions.append("CreatedAt >= ?")
        params.append(from_ts)
    if to_ts:
        conditions.append("CreatedAt <= ?")
        params.append(to_ts)

    where_clause = "WHERE " + " AND ".join(conditions) if conditions else "WHERE 1=1"

    # Query work items
    items = []
    total_items = 0
    services = set()
    rules = set()
    limit = _parse_limit(100)
    offset = _parse_offset()
    cache_key = (
        service_filter,
        rule_filter,
        action_type_filter,
        status_filter,
        str(from_ts or ""),
        str(to_ts or ""),
        int(limit),
        int(offset),
    )
    now = time.time()

    try:
        settings = _load_all_ai_settings(db)
        # Backfill may call multiple GitHub APIs; run it in the background so
        # page rendering is not blocked on network latency.
        asyncio.create_task(_maybe_backfill_github_work_item_links(db, settings))

        page_cache_hit = False
        with _work_items_cache_lock:
            cached_page = _work_items_page_cache.get(cache_key)
            if cached_page and float(cached_page.get("expires_at", 0.0)) > now:
                total_items = int(cached_page.get("total_items", 0))
                items = list(cached_page.get("items", []))
                page_cache_hit = True

        if not page_cache_hit:
            count_row = db.execute(
                f"SELECT count() AS c FROM sobs_github_work_items FINAL {where_clause}", params
            ).fetchone()
            total_items = int(count_row["c"]) if count_row else 0

            rows = db.execute(
                f"SELECT * FROM sobs_github_work_items FINAL {where_clause} "
                f"ORDER BY CreatedAt DESC LIMIT {limit} OFFSET {offset}",
                params,
            ).fetchall()
            items = [_serialize_github_work_item_row(r) for r in rows]
            with _work_items_cache_lock:
                _work_items_page_cache[cache_key] = {
                    "total_items": total_items,
                    "items": items,
                    "expires_at": now + max(1, WORK_ITEMS_PAGE_CACHE_TTL_SEC),
                }

        filter_cache_hit = False
        with _work_items_cache_lock:
            if float(_work_items_filter_cache.get("expires_at", 0.0)) > now:
                services = set(_work_items_filter_cache.get("services", []))
                rules = set(_work_items_filter_cache.get("rules", []))
                filter_cache_hit = True

        if not filter_cache_hit:
            all_services = db.execute(
                "SELECT DISTINCT ServiceName FROM sobs_github_work_items FINAL "
                "WHERE IsDeleted=0 ORDER BY ServiceName"
            ).fetchall()
            services = {str(r["ServiceName"]) for r in all_services if r["ServiceName"]}

            all_rules = db.execute(
                "SELECT DISTINCT AgentRuleName FROM sobs_github_work_items FINAL "
                "WHERE IsDeleted=0 ORDER BY AgentRuleName"
            ).fetchall()
            rules = {str(r["AgentRuleName"]) for r in all_rules if r["AgentRuleName"]}
            with _work_items_cache_lock:
                _work_items_filter_cache["services"] = sorted(services)
                _work_items_filter_cache["rules"] = sorted(rules)
                _work_items_filter_cache["expires_at"] = now + max(1, WORK_ITEMS_FILTER_CACHE_TTL_SEC)
    except Exception as exc:
        log.warning("Error loading work items: %s", exc)

    return await render_template(
        "work_items.html",
        items=items,
        total_items=total_items,
        services=sorted(services),
        rules=sorted(rules),
        service_filter=service_filter,
        rule_filter=rule_filter,
        action_type_filter=action_type_filter,
        status_filter=status_filter,
        from_ts=from_ts,
        to_ts=to_ts,
        time_error=time_error,
    )


@misc_bp.route("/api/work-items", methods=["GET"])
@require_basic_auth
async def api_get_work_items():
    """Get work items filtered by optional criteria."""
    db = get_db()

    # Parse filters
    anomaly_rule_id = request.args.get("anomaly_rule_id", "").strip()
    service_name = request.args.get("service", "").strip()
    agent_rule_id = request.args.get("rule_id", "").strip()
    signal_source = request.args.get("signal_source", "").strip()
    signal_name = request.args.get("signal_name", "").strip()
    limit = _parse_limit(100)

    conditions = ["IsDeleted = 0"]
    params = []

    if anomaly_rule_id:
        conditions.append("AnomalyRuleId = ?")
        params.append(anomaly_rule_id)
    if service_name:
        conditions.append("ServiceName = ?")
        params.append(service_name)
    if agent_rule_id:
        conditions.append("AgentRuleId = ?")
        params.append(agent_rule_id)
    if signal_source:
        conditions.append("SignalSource = ?")
        params.append(signal_source)
    if signal_name:
        conditions.append("SignalName = ?")
        params.append(signal_name)

    where_clause = " AND ".join(conditions)

    try:
        settings = _load_all_ai_settings(db)
        await _maybe_backfill_github_work_item_links(db, settings)

        rows = db.execute(
            f"SELECT * FROM sobs_github_work_items FINAL "
            f"WHERE {where_clause} "
            f"ORDER BY CreatedAt DESC "
            f"LIMIT {limit}",
            params,
        ).fetchall()
        items = [_serialize_github_work_item_row(r) for r in rows]
        return jsonify({"ok": True, "items": items})
    except Exception as exc:
        log.warning("Error fetching work items: %s", exc)
        return jsonify({"ok": False, "error": str(exc)}), 500


@misc_bp.route("/api/metrics/anomaly", methods=["GET"])
@require_basic_auth
async def metrics_anomaly():
    """Return per-minute anomaly detection data for a specific metric series.

    Query parameters:
    - ``service``: ServiceName (required)
    - ``metric``: MetricName (required)
    - ``hours``: look-back window in hours, 1–168 (default: 24)
    - ``attr_fp``: optional AttrFingerprint to select a single series

    Response JSON::

        {
          "service": "...",
          "metric": "...",
          "columns": ["time", "value", "sample_count", "baseline_mean",
                      "baseline_stddev", "baseline_lower", "baseline_upper",
                      "anomaly_score", "anomaly_state", "metric_kind", "attr_fp"],
          "rows": [[...], ...]
        }
    """
    service = (request.args.get("service") or "").strip()
    metric = (request.args.get("metric") or "").strip()
    if not service or not metric:
        return jsonify({"error": "service and metric query parameters are required"}), 400

    try:
        hours = max(1, min(168, int(request.args.get("hours") or 24)))
    except (TypeError, ValueError):
        hours = 24

    attr_fp = (request.args.get("attr_fp") or "").strip()

    db = get_db()
    try:
        fp_clause = " AND AttrFingerprint = ?" if attr_fp else ""
        params: list = [service, metric, hours]
        if attr_fp:
            params.append(attr_fp)
        result = db.execute(
            "SELECT"
            "  time,"
            "  value,"
            "  SampleCount AS sample_count,"
            "  baseline_mean,"
            "  baseline_stddev,"
            "  baseline_lower,"
            "  baseline_upper,"
            "  anomaly_score,"
            "  anomaly_state,"
            "  MetricKind AS metric_kind,"
            "  AttrFingerprint AS attr_fp"
            " FROM v_otel_metrics_anomaly"
            " WHERE ServiceName = ?"
            "   AND MetricName = ?"
            f"   AND time >= now() - INTERVAL ? HOUR"
            f"{fp_clause}"
            " ORDER BY time"
            " LIMIT 1440",
            params,
        )
        rows = result.fetchall()
        columns = (
            list(rows[0].keys())
            if rows
            else [
                "time",
                "value",
                "sample_count",
                "baseline_mean",
                "baseline_stddev",
                "baseline_lower",
                "baseline_upper",
                "anomaly_score",
                "anomaly_state",
                "metric_kind",
                "attr_fp",
            ]
        )

        def _safe(v: Any) -> Any:
            if isinstance(v, float) and (v != v):  # IEEE 754: NaN is the only value not equal to itself
                return None
            return v

        data = [[_safe(row[col]) for col in columns] for row in rows]
        return jsonify({"service": service, "metric": metric, "columns": columns, "rows": data})
    except Exception as exc:
        log.exception("metrics_anomaly query failed: service=%s metric=%s", service, metric)
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400


@misc_bp.route("/settings")
@require_basic_auth
async def view_settings():
    """Settings/config hub page linking to tag rules, metrics rules, and other config."""
    db = get_db()
    tag_rules = _load_tag_rules(db)
    anomaly_rules = _load_anomaly_rules(db)
    agent_rules = _load_agent_rules(db)
    ai_settings = _load_all_ai_settings(db)
    notification_channels = _load_notification_channels(db)
    notification_rules = _load_notification_rules(db)
    k8s_settings = _load_k8s_settings(db)
    masking_settings = _load_masking_settings(db)
    backup_enabled = (_get_app_setting(db, "data_management.backup_enabled") or "0") == "1"
    return await render_template(
        "settings.html",
        tag_rule_count=len(tag_rules),
        anomaly_rule_count=len(anomaly_rules),
        agent_rule_count=len(agent_rules),
        ai_configured=bool(ai_settings.get("ai.endpoint_url") and ai_settings.get("ai.model")),
        notification_channel_count=len(notification_channels),
        notification_rule_count=len(notification_rules),
        masking_custom_key_count=len(masking_settings["custom_keys"]),
        masking_custom_pattern_count=len(masking_settings["custom_patterns"]),
        kubernetes_view_enabled=k8s_settings.get("kubernetes.enabled") == "1",
        backup_enabled=backup_enabled,
        query_allowed_tables=sorted(_QUERY_ALLOWED_TABLES),
    )


@misc_bp.route("/api/errors/validate-regex", methods=["POST"])
@require_basic_auth
async def api_errors_validate_regex():
    """Validate a regex pattern used by /errors?q=... and return a sample match."""
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
        if service:
            where_parts.append("ServiceName = ?")
            where_params.append(service)

        time_parts, time_params = _regex_scope_time_conditions(scope, "Timestamp")
        where_parts.extend(time_parts)
        where_params.extend(time_params)

        sample = _regex_best_effort_sample(
            db,
            from_sql=f"({ERROR_SOURCES_SQL})",
            sample_column="Body",
            order_column="Timestamp",
            include_patterns=include_patterns,
            where_parts=where_parts,
            where_params=where_params,
        )
        return masked_jsonify({"ok": True, "sample": sample})
    except Exception:
        return masked_jsonify({"ok": True, "sample": None})


@misc_bp.route("/tail")
@require_basic_auth
async def tail_stream():
    """Live-tail logs and traces as a Server-Sent Events stream.

    Query parameters:
    - ``source``: ``logs``, ``traces``, or ``all`` (default: ``all``)
    - ``service``: optional service name filter (exact match)

    SSE event format::

        data: {"source": "logs", "ts": "...", "level": "INFO", "service": "...", "body": "..."}

    Example usage::

        curl -N http://localhost:44317/tail
        curl -N "http://localhost:44317/tail?source=logs&service=myapp"
    """
    source = request.args.get("source", "all").strip().lower()
    service_filter = request.args.get("service", "").strip()

    async def _generate():
        q: asyncio.Queue = asyncio.Queue(maxsize=_SSE_QUEUE_MAXSIZE)
        _sse_subscribers.add(q)
        try:
            yield "retry: 5000\n\n"
            while True:
                try:
                    event = await asyncio.wait_for(q.get(), timeout=15.0)
                except asyncio.TimeoutError:
                    yield ": keepalive\n\n"
                    continue
                if source != "all" and event.get("source") != source:
                    continue
                if service_filter and event.get("service") != service_filter:
                    continue
                yield f"data: {json.dumps(event, ensure_ascii=False)}\n\n"
        finally:
            _sse_subscribers.discard(q)

    return Response(
        _generate(),
        mimetype="text/event-stream",
        headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
    )


@misc_bp.route("/health")
async def health():
    return jsonify({"status": "ok", "version": "1.0.0"})


@misc_bp.route("/health/db")
async def health_db():
    started = time.perf_counter()
    try:
        ensure_db_schema()
        get_db().execute("SELECT 1").fetchone()
    except Exception:
        log.exception("DB readiness probe failed")
        return (
            jsonify(
                {
                    "status": "degraded",
                    "db": "error",
                    "error": "database unavailable",
                    "write_queue_depth": _write_queue_depth(),
                    "version": "1.0.0",
                }
            ),
            503,
        )

    latency_ms = round((time.perf_counter() - started) * 1000, 2)
    return jsonify(
        {
            "status": "ok",
            "db": "ok",
            "latency_ms": latency_ms,
            "write_queue_depth": _write_queue_depth(),
            "version": "1.0.0",
        }
    )


@misc_bp.route("/api/issues/raise", methods=["POST"])
@require_basic_auth
async def raise_issue_from_user_observation():
    payload = await request.get_json(force=True, silent=True) or {}
    source_page = str(payload.get("source_page") or "errors").strip().lower()
    assign_copilot = _parse_bool(payload.get("assign_copilot"), False)
    mask_output = _parse_bool(payload.get("mask_output"), True)

    db = get_db()
    settings = _load_all_ai_settings(db)
    if not settings.get("ai.endpoint_url") or not settings.get("ai.model"):
        return (
            jsonify(
                {
                    "ok": False,
                    "error": "AI endpoint not configured. Visit Settings -> AI Configuration.",
                }
            ),
            503,
        )

    trigger_context = _build_user_issue_trigger_context(source_page, payload)
    trigger_extra = trigger_context.get("extra")
    if isinstance(trigger_extra, dict):
        trigger_extra["mask_output"] = mask_output
    github_repo, github_token = _resolve_agent_github_target(db, settings, trigger_context)
    if not github_repo or not github_token:
        return (
            jsonify(
                {
                    "ok": False,
                    "error": "GitHub repo/token not configured for issue creation. Visit Settings -> AI Configuration.",
                }
            ),
            503,
        )

    actions = ["analyze", "github_issue", "dlp_check"]
    if assign_copilot:
        actions.append("github_issue_copilot")
    rule = {
        "id": f"user-observation-{source_page}",
        "name": f"User Raised Issue ({source_page})",
        "actions": actions,
        "rate_limit_minutes": 0,
    }

    outcome = await _maybe_await(_run_agent_rule_instance(db, rule, settings, trigger_context))
    if not outcome.get("ok"):
        return (
            jsonify(
                {
                    "ok": False,
                    "error": outcome.get("error", "agent flow failed"),
                    "run_id": outcome.get("run_id", ""),
                }
            ),
            500,
        )

    result = outcome.get("result") if isinstance(outcome.get("result"), dict) else {}
    issue_url = str(result.get("github_issue_url") or "")
    dedup_decision = str(result.get("dedup_decision") or "")
    issue_error = str(result.get("issue_error") or "").strip()
    if issue_url:
        owner, repo, issue_number = _parse_issue_ref_from_url(issue_url)
        if not owner or not repo or issue_number <= 0:
            issue_error = issue_error or "Agent returned an invalid issue URL"
            dedup_decision = "create_failed"
            issue_url = ""
    if not issue_url and dedup_decision == "create_failed":
        return (
            jsonify(
                {
                    "ok": False,
                    "error": issue_error or "GitHub issue creation failed. Check repository settings and token scopes.",
                    "run_id": outcome.get("run_id", ""),
                    "source": "user",
                    "source_page": source_page,
                }
            ),
            502,
        )
    if not issue_url and dedup_decision == "suppressed_rate_limit":
        return (
            jsonify(
                {
                    "ok": False,
                    "error": "GitHub issue creation suppressed by hourly limit. Try again later.",
                    "run_id": outcome.get("run_id", ""),
                    "source": "user",
                    "source_page": source_page,
                }
            ),
            429,
        )

    return jsonify(
        {
            "ok": True,
            "run_id": outcome.get("run_id", ""),
            "source": "user",
            "source_page": source_page,
            "issue_url": issue_url,
            "dedup_decision": dedup_decision,
            "copilot_assignment_status": str(result.get("copilot_assignment_status") or ""),
            "copilot_assignment_reason": str(result.get("copilot_assignment_reason") or ""),
            "status": str(result.get("status") or ""),
        }
    )
