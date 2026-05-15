from __future__ import annotations

import json
import logging
import time
import uuid
from typing import Any

from quart import Blueprint, flash, redirect, render_template, request, url_for

import app as sobs_app
import telemetry as _telemetry
from app import (  # noqa: E402
    _AUTO_DASHBOARD_CREATE_MAX,
    _AUTO_RULE_CREATE_MAX,
    _SEASONAL_STRATEGIES,
    RowCompat,
    _annotate_rows_with_rules,
    _append_regex_expression_clauses,
    _append_time_window_filter,
    _build_auto_dashboard_chart_candidates,
    _build_raw_chart_spec,
    _build_seasonal_metric_rule_candidates,
    _coerce_positive_int,
    _default_auto_dashboard_name,
    _get_charts,
    _insert_rows_json_each_row,
    _list_derived_signal_dimensions,
    _load_anomaly_rules,
    _parse_limit,
    _parse_offset,
    _parse_sort,
    _parse_time_window_args,
    _prepare_re2_filter_patterns,
    _public_dashboard_query_error,
    _seed_dashboard_if_missing,
    _soft_delete_latest_row,
    _time_window_conditions,
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
metrics_bp: Blueprint = Blueprint("metrics", __name__)


def _build_auto_metric_rule_candidates(*args: Any, **kwargs: Any):
    return sobs_app._build_auto_metric_rule_candidates(*args, **kwargs)


def get_db():
    return sobs_app.get_db()


@metrics_bp.route("/metrics")
@require_basic_auth
@_telemetry.traced_view("sobs.dashboard.query", **{"dashboard.name": "metrics", "route": "/metrics"})
async def view_metrics():
    db = get_db()
    selected_services = [svc.strip() for svc in request.args.getlist("service") if svc.strip()]
    selected_signals = [sig.strip() for sig in request.args.getlist("signal") if sig.strip()]
    selected_sources = [src.strip() for src in request.args.getlist("source") if src.strip()]
    service = selected_services[0] if selected_services else ""
    signal = selected_signals[0] if selected_signals else ""
    source = selected_sources[0] if selected_sources else ""
    attr_fp = request.args.get("attr_fp", "").strip()
    q = request.args.get("q", "").strip()
    from_ts, to_ts, time_error = _parse_time_window_args()
    limit = _parse_limit(100)
    offset = _parse_offset()
    sort_by, sort_col, sort_dir = _parse_sort(
        {
            "last_time": "last_time",
            "service": "service",
            "source": "source",
            "signal": "signal",
            "last_value": "last_value",
            "last_anomaly_score": "last_anomaly_score",
            "last_anomaly_state": "last_anomaly_state",
            "last_sample_count": "last_sample_count",
            "point_count": "point_count",
        },
        "last_time",
    )
    order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"

    try:
        hours = max(1, min(168, int(request.args.get("hours") or 24)))
    except (TypeError, ValueError):
        hours = 24

    where_parts: list[str] = []
    params: list[str] = []
    if selected_services:
        placeholders = ",".join(["?"] * len(selected_services))
        where_parts.append(f"ServiceName IN ({placeholders})")
        params.extend(selected_services)
    if selected_signals:
        placeholders = ",".join(["?"] * len(selected_signals))
        where_parts.append(f"SignalName IN ({placeholders})")
        params.extend(selected_signals)
    if selected_sources:
        placeholders = ",".join(["?"] * len(selected_sources))
        where_parts.append(f"SignalSource IN ({placeholders})")
        params.extend(selected_sources)
    if attr_fp:
        where_parts.append("AttrFingerprint = ?")
        params.append(attr_fp)

    if not time_error:
        _append_time_window_filter(where_parts, params, "time", from_ts, to_ts)

    hour_clause = ""
    if not from_ts and not to_ts:
        hour_clause = "time >= now() - INTERVAL ? HOUR"

    rows: list[dict] = []
    total = 0
    error_msg = time_error
    include_patterns: list[str] = []
    exclude_patterns: list[str] = []
    if q and not error_msg:
        include_patterns, exclude_patterns, regex_error = _prepare_re2_filter_patterns(db, q)
        if regex_error:
            error_msg = regex_error
        else:
            _append_regex_expression_clauses(
                conditions=where_parts,
                params=params,
                column="SignalName",
                include_patterns=include_patterns,
                exclude_patterns=exclude_patterns,
            )

    if hour_clause:
        params.append(hours)

    where_clause = f" {_where_clause(where_parts)}" if where_parts else ""
    if hour_clause:
        where_clause = f"{where_clause} AND {hour_clause}" if where_clause else f" WHERE {hour_clause}"

    if not error_msg:
        try:
            grouped_sql = (
                "SELECT"
                "  ServiceName AS service,"
                "  SignalSource AS source,"
                "  SignalName AS signal,"
                "  AttrFingerprint AS attr_fp,"
                "  max(time) AS last_time,"
                "  argMax(value, time) AS last_value,"
                "  argMax(anomaly_score, time) AS last_anomaly_score,"
                "  argMax(anomaly_state, time) AS last_anomaly_state,"
                "  argMax(SampleCount, time) AS last_sample_count,"
                "  count() AS point_count"
                " FROM v_derived_signals_anomaly"
                f"{where_clause}"
                " GROUP BY ServiceName, SignalSource, SignalName, AttrFingerprint"
            )

            total = db.execute(f"SELECT COUNT(*) FROM ({grouped_sql})", params).fetchone()[0]
            fetched = db.execute(
                f"SELECT * FROM ({grouped_sql}) {order_clause} LIMIT ? OFFSET ?",
                params + [limit, offset],
            ).fetchall()
            for row in fetched:
                rows.append(
                    {
                        "service": str(row["service"]),
                        "source": str(row["source"]),
                        "signal": str(row["signal"]),
                        "attr_fp": str(row["attr_fp"]),
                        "last_time": str(row["last_time"]),
                        "last_value": row["last_value"],
                        "last_anomaly_score": row["last_anomaly_score"],
                        "last_anomaly_state": str(row["last_anomaly_state"]),
                        "last_sample_count": row["last_sample_count"],
                        "point_count": row["point_count"],
                        "rule_name": "",
                    }
                )
        except Exception as exc:
            log.exception("metrics index query failed")
            error_msg = _public_dashboard_query_error(exc)

    _annotate_rows_with_rules(
        rows,
        _load_anomaly_rules(db),
        source_key="source",
        signal_key="signal",
        service_key="service",
        attr_fp_key="attr_fp",
        value_key="last_value",
        sample_count_key="last_sample_count",
        time_key="last_time",
    )

    services, signals, sources = _list_derived_signal_dimensions(db)

    return await render_template(
        "metrics.html",
        rows=rows,
        total=total,
        limit=limit,
        offset=offset,
        service=service,
        selected_services=selected_services,
        signal=signal,
        selected_signals=selected_signals,
        source=source,
        selected_sources=selected_sources,
        attr_fp=attr_fp,
        q=q,
        from_ts=from_ts,
        to_ts=to_ts,
        hours=hours,
        error_msg=error_msg,
        services=services,
        signals=signals,
        sources=sources,
        sort_by=sort_by,
        sort_dir=sort_dir,
    )


# ---------------------------------------------------------------------------
# Web UI – Metrics Rules
# ---------------------------------------------------------------------------
@metrics_bp.route("/metrics/rules")
@require_basic_auth
async def view_metrics_rules():
    db = get_db()
    open_panel = (request.args.get("open_panel") or "").strip().lower()
    if open_panel not in {"auto-rules", "auto-dashboard"}:
        open_panel = ""
    services, signals, sources = _list_derived_signal_dimensions(db)
    rules = _load_anomaly_rules(db)
    return await render_template(
        "metrics_rules.html",
        rules=rules,
        services=services,
        signals=signals,
        sources=sources,
        auto_preview=[],
        auto_summary=None,
        auto_dashboard_preview=[],
        auto_dashboard_summary=None,
        auto_open_panel=open_panel,
    )


@metrics_bp.route("/metrics/rules", methods=["POST"])
@require_basic_auth
async def create_metrics_rule():
    form = await request.form
    name = (form.get("name") or "").strip()
    rule_type = (form.get("rule_type") or "threshold").strip().lower()
    source = (form.get("source") or "").strip()
    signal = (form.get("signal") or "").strip()
    service = (form.get("service") or "").strip()
    attr_fp = (form.get("attr_fp") or "").strip()
    comparator = (form.get("comparator") or "gt").strip().lower()
    secondary_source = (form.get("secondary_source") or "").strip()
    secondary_signal = (form.get("secondary_signal") or "").strip()
    secondary_comparator = (form.get("secondary_comparator") or "gt").strip().lower()

    if not name or not source or not signal:
        await flash("Rule name, source, and signal are required", "warning")
        return redirect(url_for("metrics.view_metrics_rules"))

    if rule_type not in {"threshold", "composite"}:
        await flash("Rule type must be 'threshold' or 'composite'", "warning")
        return redirect(url_for("metrics.view_metrics_rules"))

    if comparator not in {"gt", "lt"}:
        await flash("Comparator must be 'gt' or 'lt'", "warning")
        return redirect(url_for("metrics.view_metrics_rules"))
    if secondary_comparator not in {"gt", "lt"}:
        await flash("Secondary comparator must be 'gt' or 'lt'", "warning")
        return redirect(url_for("metrics.view_metrics_rules"))

    try:
        warning_threshold = float(form.get("warning_threshold") or "")
        critical_threshold = float(form.get("critical_threshold") or "")
        min_sample_count = max(1, int(form.get("min_sample_count") or 1))
        secondary_warning_threshold = float(form.get("secondary_warning_threshold") or 0)
        secondary_critical_threshold = float(form.get("secondary_critical_threshold") or 0)
    except (TypeError, ValueError):
        await flash("Thresholds must be numeric and sample count must be an integer", "warning")
        return redirect(url_for("metrics.view_metrics_rules"))

    if comparator == "gt" and critical_threshold < warning_threshold:
        await flash("For 'gt' rules, critical threshold must be >= warning threshold", "warning")
        return redirect(url_for("metrics.view_metrics_rules"))
    if comparator == "lt" and critical_threshold > warning_threshold:
        await flash("For 'lt' rules, critical threshold must be <= warning threshold", "warning")
        return redirect(url_for("metrics.view_metrics_rules"))
    if rule_type == "composite":
        if not secondary_source or not secondary_signal:
            await flash("Composite rules require a secondary source and signal", "warning")
            return redirect(url_for("metrics.view_metrics_rules"))
        if secondary_comparator == "gt" and secondary_critical_threshold < secondary_warning_threshold:
            await flash("For secondary 'gt' rules, critical threshold must be >= warning threshold", "warning")
            return redirect(url_for("metrics.view_metrics_rules"))
        if secondary_comparator == "lt" and secondary_critical_threshold > secondary_warning_threshold:
            await flash("For secondary 'lt' rules, critical threshold must be <= warning threshold", "warning")
            return redirect(url_for("metrics.view_metrics_rules"))
    else:
        secondary_source = ""
        secondary_signal = ""
        secondary_comparator = "gt"
        secondary_warning_threshold = 0.0
        secondary_critical_threshold = 0.0

    rule_id = str(uuid.uuid4())
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        get_db(),
        "sobs_anomaly_rules",
        [
            {
                "Id": rule_id,
                "Name": name,
                "RuleType": rule_type,
                "SignalSource": source,
                "SignalName": signal,
                "ServiceName": service,
                "AttrFingerprint": attr_fp,
                "Comparator": comparator,
                "WarningThreshold": warning_threshold,
                "CriticalThreshold": critical_threshold,
                "SecondarySignalSource": secondary_source,
                "SecondarySignalName": secondary_signal,
                "SecondaryComparator": secondary_comparator,
                "SecondaryWarningThreshold": secondary_warning_threshold,
                "SecondaryCriticalThreshold": secondary_critical_threshold,
                "MinSampleCount": min_sample_count,
                "IsDeleted": 0,
                "Version": version,
            }
        ],
    )
    await flash(f"Rule '{name}' created", "success")
    return redirect(url_for("metrics.view_metrics_rules"))


@metrics_bp.route("/metrics/rules/auto", methods=["POST"])
@require_basic_auth
async def auto_metrics_rules():
    form = await request.form
    action = (form.get("action") or "preview").strip().lower()
    try:
        hours = max(1, min(168, int(form.get("hours") or 24)))
    except (TypeError, ValueError):
        hours = 24
    try:
        min_points = max(1, min(5000, int(form.get("min_points") or 30)))
    except (TypeError, ValueError):
        min_points = 30

    service_filter = (form.get("service_filter") or "").strip()
    include_attr_fp = (form.get("include_attr_fp") or "") in {"1", "true", "on", "yes"}
    mode = (form.get("mode") or "threshold").strip().lower()
    if mode not in {"threshold", "seasonal"}:
        mode = "threshold"
    seasonal_strategy = (form.get("seasonal_strategy") or "hour_of_day").strip().lower()
    if seasonal_strategy not in _SEASONAL_STRATEGIES:
        seasonal_strategy = "hour_of_day"

    db = get_db()
    services, signals, sources = _list_derived_signal_dimensions(db)
    existing_rules = _load_anomaly_rules(db)

    if mode == "seasonal":
        candidates, stats = _build_seasonal_metric_rule_candidates(
            db,
            hours=hours,
            min_points=min_points,
            service_filter=service_filter,
            include_attr_fp=include_attr_fp,
            strategy=seasonal_strategy,
        )
    else:
        candidates, stats = _build_auto_metric_rule_candidates(
            db,
            hours=hours,
            min_points=min_points,
            service_filter=service_filter,
            include_attr_fp=include_attr_fp,
        )

    summary = {
        "action": action,
        "hours": hours,
        "min_points": min_points,
        "service_filter": service_filter,
        "include_attr_fp": include_attr_fp,
        "mode": mode,
        "seasonal_strategy": seasonal_strategy,
        "examined": stats["examined"],
        "existing": stats["existing"],
        "invalid": stats["invalid"],
        "candidates": len(candidates),
        "create_cap": _AUTO_RULE_CREATE_MAX,
        "capped": len(candidates) > _AUTO_RULE_CREATE_MAX,
        "created": 0,
    }

    if action == "create":
        limited_candidates = candidates[:_AUTO_RULE_CREATE_MAX]
        now_version = int(time.time() * 1000)
        rows_to_insert: list[dict[str, object]] = []
        for idx, candidate in enumerate(limited_candidates):
            rows_to_insert.append(
                {
                    "Id": str(uuid.uuid4()),
                    "Name": str(candidate["name"]),
                    "RuleType": str(candidate.get("rule_type", "threshold")),
                    "SignalSource": str(candidate["source"]),
                    "SignalName": str(candidate["signal"]),
                    "ServiceName": str(candidate["service"]),
                    "AttrFingerprint": str(candidate["attr_fp"]),
                    "Comparator": str(candidate["comparator"]),
                    "WarningThreshold": float(candidate["warning_threshold"]),
                    "CriticalThreshold": float(candidate["critical_threshold"]),
                    "SecondarySignalSource": "",
                    "SecondarySignalName": "",
                    "SecondaryComparator": "gt",
                    "SecondaryWarningThreshold": 0.0,
                    "SecondaryCriticalThreshold": 0.0,
                    "MinSampleCount": int(candidate["min_sample_count"]),
                    "SeasonalBucketsJson": str(candidate.get("seasonal_buckets_json") or ""),
                    "IsDeleted": 0,
                    "Version": now_version + idx,
                }
            )

        if rows_to_insert:
            _insert_rows_json_each_row(db, "sobs_anomaly_rules", rows_to_insert)
        summary["created"] = len(rows_to_insert)
        skipped_by_cap = max(0, len(candidates) - len(limited_candidates))
        cap_suffix = f", skipped {skipped_by_cap} by max cap ({_AUTO_RULE_CREATE_MAX})." if skipped_by_cap else "."
        await flash(
            (
                f"Auto rule generation complete: created {summary['created']} rule(s), "
                f"skipped {summary['existing']} existing, {summary['invalid']} invalid"
                f"{cap_suffix}"
            ),
            "success",
        )
        return redirect(url_for("metrics.view_metrics_rules", open_panel="auto-rules"))

    await flash(
        (
            f"Auto-rule preview: {summary['candidates']} candidate(s), "
            f"{summary['existing']} existing skipped, {summary['invalid']} invalid."
        ),
        "info",
    )
    return await render_template(
        "metrics_rules.html",
        rules=existing_rules,
        services=services,
        signals=signals,
        sources=sources,
        auto_preview=candidates,
        auto_summary=summary,
        auto_dashboard_preview=[],
        auto_dashboard_summary=None,
        auto_open_panel="auto-rules",
    )


@metrics_bp.route("/metrics/rules/dashboard/auto", methods=["POST"])
@require_basic_auth
async def auto_metrics_rules_dashboard():
    form = await request.form
    action = (form.get("action") or "preview").strip().lower()
    service_filter = (form.get("service_filter") or "").strip()
    hours = _coerce_positive_int(form.get("hours"), default_value=24, min_value=1, max_value=168)
    max_charts = _coerce_positive_int(
        form.get("max_charts"),
        default_value=12,
        min_value=1,
        max_value=_AUTO_DASHBOARD_CREATE_MAX,
    )
    dashboard_name = (form.get("dashboard_name") or "").strip() or _default_auto_dashboard_name(service_filter)

    db = get_db()
    services, signals, sources = _list_derived_signal_dimensions(db)
    rules = _load_anomaly_rules(db)
    candidates = _build_auto_dashboard_chart_candidates(
        rules,
        service_filter=service_filter,
        hours=hours,
    )
    capped_candidates = candidates[:max_charts]

    summary = {
        "action": action,
        "hours": hours,
        "service_filter": service_filter,
        "max_charts": max_charts,
        "create_cap": _AUTO_DASHBOARD_CREATE_MAX,
        "dashboard_name": dashboard_name,
        "rules_total": len(rules),
        "candidates": len(candidates),
        "capped": len(candidates) > max_charts,
        "created": 0,
        "existing": 0,
    }

    if action == "create":
        if not capped_candidates:
            await flash("No matching rules found for dashboard generation", "warning")
            return redirect(url_for("metrics.view_metrics_rules", open_panel="auto-dashboard"))

        dashboard_description = (
            "Auto-generated from active metric rules. "
            f"window={hours}h, scope={'all services' if not service_filter else service_filter}."
        )
        dashboard_id = _seed_dashboard_if_missing(db, dashboard_name, dashboard_description)

        existing_charts = _get_charts(db, dashboard_id)
        existing_titles = {str(chart["title"]) for chart in existing_charts}
        next_position = max((int(chart["position"]) for chart in existing_charts), default=-1) + 1
        next_version = int(time.time() * 1000)
        rows_to_insert: list[dict[str, object]] = []

        for idx, candidate in enumerate(capped_candidates):
            title = str(candidate["title"])
            if title in existing_titles:
                summary["existing"] += 1
                continue
            query = str(candidate["query"])
            chart_type = str(candidate["chart_type"])
            rows_to_insert.append(
                {
                    "Id": str(uuid.uuid4()),
                    "DashboardId": dashboard_id,
                    "Title": title,
                    "ChartType": chart_type,
                    "Query": query,
                    "OptionsJson": json.dumps(
                        {"chart_spec": _build_raw_chart_spec(chart_type, query)},
                        ensure_ascii=False,
                    ),
                    "Position": next_position + idx,
                    "IsDeleted": 0,
                    "Version": next_version + idx,
                }
            )
            existing_titles.add(title)

        if rows_to_insert:
            _insert_rows_json_each_row(db, "sobs_chart_configs", rows_to_insert)
        summary["created"] = len(rows_to_insert)

        skipped_by_max = max(0, len(candidates) - len(capped_candidates))
        cap_note = f", skipped {skipped_by_max} by selected max ({max_charts})" if skipped_by_max else ""
        await flash(
            (
                f"Auto dashboard ready: created {summary['created']} chart(s), "
                f"skipped {summary['existing']} existing{cap_note}."
            ),
            "success",
        )
        return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))

    await flash(
        (
            f"Auto-dashboard preview: {summary['candidates']} candidate chart(s) from "
            f"{summary['rules_total']} rule(s)."
        ),
        "info",
    )
    return await render_template(
        "metrics_rules.html",
        rules=rules,
        services=services,
        signals=signals,
        sources=sources,
        auto_preview=[],
        auto_summary=None,
        auto_dashboard_preview=candidates,
        auto_dashboard_summary=summary,
        auto_open_panel="auto-dashboard",
    )


@metrics_bp.route("/metrics/rules/<rule_id>/delete", methods=["POST"])
@require_basic_auth
async def delete_metrics_rule(rule_id: str):
    db = get_db()

    def _deleted_row(row: RowCompat) -> dict[str, Any]:
        return {
            "Id": str(row["Id"]),
            "Name": str(row["Name"]),
            "RuleType": str(row["RuleType"] or "threshold"),
            "SignalSource": str(row["SignalSource"]),
            "SignalName": str(row["SignalName"]),
            "ServiceName": str(row["ServiceName"]),
            "AttrFingerprint": str(row["AttrFingerprint"]),
            "Comparator": str(row["Comparator"]),
            "WarningThreshold": float(row["WarningThreshold"]),
            "CriticalThreshold": float(row["CriticalThreshold"]),
            "SecondarySignalSource": str(row["SecondarySignalSource"]),
            "SecondarySignalName": str(row["SecondarySignalName"]),
            "SecondaryComparator": str(row["SecondaryComparator"] or "gt"),
            "SecondaryWarningThreshold": float(row["SecondaryWarningThreshold"]),
            "SecondaryCriticalThreshold": float(row["SecondaryCriticalThreshold"]),
            "MinSampleCount": int(row["MinSampleCount"]),
        }

    return await _soft_delete_latest_row(
        db,
        select_sql=(
            "SELECT Id, Name, RuleType, SignalSource, SignalName, ServiceName, AttrFingerprint, Comparator, "
            "WarningThreshold, CriticalThreshold, SecondarySignalSource, SecondarySignalName, "
            "SecondaryComparator, SecondaryWarningThreshold, SecondaryCriticalThreshold, MinSampleCount "
            "FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 AND Id = ?"
        ),
        select_params=[rule_id],
        table_name="sobs_anomaly_rules",
        build_deleted_row=_deleted_row,
        not_found_message="Rule not found",
        success_message="Rule '{name}' deleted",
        redirect_endpoint="metrics.view_metrics_rules",
    )


# ---------------------------------------------------------------------------
# Web UI – Metrics Anomaly Details
# ---------------------------------------------------------------------------
@metrics_bp.route("/metrics/anomaly")
@require_basic_auth
async def view_metrics_anomaly():
    db = get_db()
    service = request.args.get("service", "").strip()
    metric = request.args.get("metric", "").strip()
    signal = request.args.get("signal", "").strip()
    source = request.args.get("source", "").strip()
    attr_fp = request.args.get("attr_fp", "").strip()
    from_ts, to_ts, time_error = _parse_time_window_args()

    # Optional metadata passed from chart click for point-level context.
    point_state = request.args.get("_anomaly_state", "").strip()
    point_score = request.args.get("_anomaly_score", "").strip()

    try:
        hours = max(1, min(168, int(request.args.get("hours") or 24)))
    except (TypeError, ValueError):
        hours = 24

    where_parts: list[str] = []
    params: list[str] = []
    if service:
        where_parts.append("ServiceName = ?")
        params.append(service)
    if metric:
        where_parts.append("MetricName = ?")
        params.append(metric)
    if signal:
        where_parts.append("SignalName = ?")
        params.append(signal)
    if source:
        where_parts.append("SignalSource = ?")
        params.append(source)
    if attr_fp:
        where_parts.append("AttrFingerprint = ?")
        params.append(attr_fp)

    if not time_error:
        time_conditions, time_params = _time_window_conditions("time", from_ts, to_ts)
        where_parts.extend(time_conditions)
        params.extend(time_params)

    # Fallback to hour-based window only when explicit time window is not provided.
    hour_clause = ""
    if not from_ts and not to_ts:
        hour_clause = "time >= now() - INTERVAL ? HOUR"
        params.append(hours)

    where_clause = ""
    if where_parts:
        where_clause = " WHERE " + " AND ".join(where_parts)
    if hour_clause:
        where_clause = f"{where_clause} AND {hour_clause}" if where_clause else f" WHERE {hour_clause}"

    rows: list[dict] = []
    error_msg = time_error
    related_target = source if source in {"logs", "traces", "errors"} else ""
    active_rules = _load_anomaly_rules(db)
    use_otel_metrics_view = bool(metric) and not signal and not source
    if not error_msg:
        try:
            # Keep existing metric drilldown behavior and support derived signals.
            result = db.execute(
                (
                    (
                        "SELECT"
                        "  time,"
                        "  ServiceName,"
                        "  MetricName AS Name,"
                        "  MetricKind AS Kind,"
                        "  AttrFingerprint,"
                        "  value,"
                        "  SampleCount,"
                        "  baseline_mean,"
                        "  baseline_stddev,"
                        "  baseline_lower,"
                        "  baseline_upper,"
                        "  anomaly_score,"
                        "  anomaly_state"
                        " FROM v_otel_metrics_anomaly"
                    )
                    if use_otel_metrics_view
                    else (
                        "SELECT"
                        "  time,"
                        "  ServiceName,"
                        "  SignalName AS Name,"
                        "  SignalSource AS Kind,"
                        "  AttrFingerprint,"
                        "  value,"
                        "  SampleCount,"
                        "  baseline_mean,"
                        "  baseline_stddev,"
                        "  baseline_lower,"
                        "  baseline_upper,"
                        "  anomaly_score,"
                        "  anomaly_state"
                        " FROM v_derived_signals_anomaly"
                    )
                    + f"{where_clause}"
                    + " ORDER BY time DESC"
                    + " LIMIT 500"
                ),
                params,
            )
            fetched = result.fetchall()
            for row in fetched:
                rows.append(
                    {
                        "time": str(row["time"]),
                        "service": str(row["ServiceName"]),
                        "metric": str(row["Name"]),
                        "metric_kind": str(row["Kind"]),
                        "related_target": ("" if use_otel_metrics_view else str(row["Kind"])),
                        "attr_fp": str(row["AttrFingerprint"]),
                        "value": row["value"],
                        "sample_count": row["SampleCount"],
                        "baseline_mean": row["baseline_mean"],
                        "baseline_stddev": row["baseline_stddev"],
                        "baseline_lower": row["baseline_lower"],
                        "baseline_upper": row["baseline_upper"],
                        "anomaly_score": row["anomaly_score"],
                        "anomaly_state": str(row["anomaly_state"]),
                    }
                )
        except Exception as exc:
            log.exception("metrics anomaly detail query failed")
            error_msg = _public_dashboard_query_error(exc)

    if not use_otel_metrics_view:
        _annotate_rows_with_rules(
            rows,
            active_rules,
            source_key="related_target",
            signal_key="metric",
            service_key="service",
            attr_fp_key="attr_fp",
            value_key="value",
            sample_count_key="sample_count",
            time_key="time",
        )

    services, signals, sources = _list_derived_signal_dimensions(db)

    return await render_template(
        "metrics_anomaly.html",
        rows=rows,
        total=len(rows),
        service=service,
        metric=metric,
        signal=signal,
        source=source,
        attr_fp=attr_fp,
        from_ts=from_ts,
        to_ts=to_ts,
        hours=hours,
        error_msg=error_msg,
        point_state=point_state,
        point_score=point_score,
        related_target=related_target,
        services=services,
        signals=signals,
        sources=sources,
    )


@metrics_bp.route("/api/metrics/validate-regex", methods=["POST"])
@require_basic_auth
async def api_metrics_validate_regex():
    """Validate a regex pattern used by /metrics?q=... and return a sample match."""
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
        source = _regex_scope_text(scope, "source")
        signal = _regex_scope_text(scope, "signal")
        attr_fp = _regex_scope_text(scope, "attr_fp", 64)
        if service:
            where_parts.append("ServiceName = ?")
            where_params.append(service)
        if source:
            where_parts.append("SignalSource = ?")
            where_params.append(source)
        if signal:
            where_parts.append("SignalName = ?")
            where_params.append(signal)
        if attr_fp:
            where_parts.append("AttrFingerprint = ?")
            where_params.append(attr_fp)

        time_parts, time_params = _regex_scope_time_conditions(scope, "time")
        where_parts.extend(time_parts)
        where_params.extend(time_params)

        sample = _regex_best_effort_sample(
            db,
            from_sql="v_derived_signals_anomaly",
            sample_column="SignalName",
            order_column="time",
            include_patterns=include_patterns,
            exclude_patterns=_exclude_patterns,
            where_parts=where_parts,
            where_params=where_params,
        )
        return masked_jsonify({"ok": True, "sample": sample})
    except Exception:
        return masked_jsonify({"ok": True, "sample": None})
