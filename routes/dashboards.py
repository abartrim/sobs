"""Dashboard and chart spec routes blueprint."""

from __future__ import annotations

import asyncio
import json
import logging
import re
import time
import uuid
from typing import Any

from quart import Blueprint, flash, jsonify, redirect, render_template, request, url_for

import app as sobs_app
from app import (  # noqa: E402
    CHART_TEMPLATES,
    ChDbConnection,
    ChdbSqlRunner,
    _apply_chart_spec_visual_overrides,
    _build_fallback_custom_option_json,
    _coerce_positive_int,
    _compile_chart_spec,
    _default_chart_spec,
    _get_charts,
    _get_dashboard,
    _get_dashboards,
    _infer_column_types,
    _infer_custom_mapping_from_option,
    _insert_rows_json_each_row,
    _json_safe_rows,
    _jsonify_with_optional_sql_output_mask,
    _normalize_thinking_level,
    _parse_chart_form_submission,
    _public_dashboard_query_error,
    _render_chart_from_template,
    _sql_literal,
    _validate_chart_query,
    _vanna_execute_named_queries,
    _vanna_validate_and_execute_with_repair,
    require_basic_auth,
)

dashboards_bp: Blueprint = Blueprint("dashboards", __name__)
log = logging.getLogger("sobs")


def _load_all_ai_settings(*args: Any, **kwargs: Any):
    return sobs_app._load_all_ai_settings(*args, **kwargs)


def _vanna_generate_chart_spec(*args: Any, **kwargs: Any):
    return sobs_app._vanna_generate_chart_spec(*args, **kwargs)


def _vanna_generate_named_queries(*args: Any, **kwargs: Any):
    return sobs_app._vanna_generate_named_queries(*args, **kwargs)


def _vanna_generate_sql(*args: Any, **kwargs: Any):
    return sobs_app._vanna_generate_sql(*args, **kwargs)


def get_db():
    return sobs_app.get_db()


@dashboards_bp.route("/api/dashboards/list", methods=["GET"])
@require_basic_auth
async def api_dashboards_list():
    """Return all non-deleted dashboards for quick picker UIs."""
    db = get_db()
    dashboards = _get_dashboards(db)
    return jsonify({"ok": True, "dashboards": dashboards})


@dashboards_bp.route("/api/query/add-to-dashboard", methods=["POST"])
@require_basic_auth
async def api_query_add_to_dashboard():
    """Persist query-page SQL + chart JSON into a dashboard chart record."""
    payload = await request.get_json(silent=True) or {}

    dashboard_id = str(payload.get("dashboard_id") or "").strip()
    title = str(payload.get("title") or "").strip()
    sql = str(payload.get("sql") or "").strip()
    chart_spec_raw = payload.get("chart_spec")

    if not dashboard_id:
        return jsonify({"ok": False, "error": "dashboard_id is required"}), 400
    if not sql:
        return jsonify({"ok": False, "error": "sql is required"}), 400
    if not chart_spec_raw:
        return jsonify({"ok": False, "error": "chart_spec is required"}), 400

    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        return jsonify({"ok": False, "error": "Dashboard not found"}), 404

    if not title:
        title = "Query Chart"

    try:
        chart_option = json.loads(chart_spec_raw) if isinstance(chart_spec_raw, str) else chart_spec_raw
    except Exception as exc:
        return jsonify({"ok": False, "error": f"chart_spec must be valid JSON: {exc}"}), 400
    if not isinstance(chart_option, dict):
        return jsonify({"ok": False, "error": "chart_spec must be a JSON object"}), 400

    spec_raw = {
        "template_id": "custom_echarts",
        "sql": {"mode": "raw", "override_sql": sql},
        "visual": {
            "custom_option_json": json.dumps(chart_option, ensure_ascii=False),
            "custom_mapping_json": "{}",
        },
    }
    try:
        template_id, query, normalized_spec = _compile_chart_spec(spec_raw)
    except Exception as exc:
        return jsonify({"ok": False, "error": f"Chart spec error: {exc}"}), 400

    options_json = json.dumps({"chart_spec": normalized_spec}, ensure_ascii=False)
    existing = _get_charts(db, dashboard_id)
    position = max((c["position"] for c in existing), default=-1) + 1

    chart_id = str(uuid.uuid4())
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": chart_id,
                "DashboardId": dashboard_id,
                "Title": title,
                "ChartType": template_id,
                "Query": query,
                "OptionsJson": options_json,
                "Position": position,
                "IsDeleted": 0,
                "Version": version,
            }
        ],
    )

    return jsonify(
        {
            "ok": True,
            "chart_id": chart_id,
            "dashboard_id": dashboard_id,
            "dashboard_name": dashboard["name"],
            "dashboard_url": url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id),
        }
    )


@dashboards_bp.route("/dashboards")
@require_basic_auth
async def list_dashboards():
    db = get_db()
    dashboards = _get_dashboards(db)
    return await render_template("custom_dashboards.html", dashboards=dashboards)


@dashboards_bp.route("/dashboards/new", methods=["GET"])
@require_basic_auth
async def new_dashboard_form():
    return await render_template("custom_dashboards.html", dashboards=[], show_new_form=True)


@dashboards_bp.route("/dashboards", methods=["POST"])
@require_basic_auth
async def create_dashboard():
    form = await request.form
    name = (form.get("name") or "").strip()
    description = (form.get("description") or "").strip()
    if not name:
        await flash("Dashboard name is required", "warning")
        return redirect(url_for("dashboards.list_dashboards"))
    dashboard_id = str(uuid.uuid4())
    version = int(time.time() * 1000)
    db = get_db()
    _insert_rows_json_each_row(
        db,
        "sobs_dashboards",
        [{"Id": dashboard_id, "Name": name, "Description": description, "IsDeleted": 0, "Version": version}],
    )
    return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))


@dashboards_bp.route("/dashboards/<dashboard_id>")
@require_basic_auth
async def view_custom_dashboard(dashboard_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("dashboards.list_dashboards"))
    charts = _get_charts(db, dashboard_id)
    # Convert chart_type to template metadata for frontend
    templates = [
        {
            "id": tid,
            "name": t["name"],
            "description": t["description"],
            "icon": t["icon"],
            "query_shape": t.get("query_shape", ""),
            "sample_sql": t.get("sample_sql", ""),
            "drilldown": t.get("drilldown"),
            "default_spec": _default_chart_spec(tid),
        }
        for tid, t in sorted(CHART_TEMPLATES.items())
    ]
    return await render_template(
        "custom_dashboard_view.html",
        dashboard=dashboard,
        charts=charts,
        templates=templates,
    )


@dashboards_bp.route("/dashboards/<dashboard_id>/delete", methods=["POST"])
@require_basic_auth
async def delete_dashboard(dashboard_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("dashboards.list_dashboards"))
    version = int(time.time() * 1000)
    # Soft-delete dashboard
    _insert_rows_json_each_row(
        db,
        "sobs_dashboards",
        [
            {
                "Id": dashboard_id,
                "Name": dashboard["name"],
                "Description": dashboard["description"],
                "IsDeleted": 1,
                "Version": version,
            }
        ],
    )
    # Soft-delete all charts in this dashboard
    charts = _get_charts(db, dashboard_id)
    if charts:
        tombstones = [
            {
                "Id": c["id"],
                "DashboardId": dashboard_id,
                "Title": c["title"],
                "ChartType": c["chart_type"],
                "Query": c["query"],
                "OptionsJson": c["options_json"],
                "Position": c["position"],
                "IsDeleted": 1,
                "Version": version,
            }
            for c in charts
        ]
        _insert_rows_json_each_row(db, "sobs_chart_configs", tombstones)
    await flash(f"Dashboard '{dashboard['name']}' deleted", "success")
    return redirect(url_for("dashboards.list_dashboards"))


@dashboards_bp.route("/dashboards/<dashboard_id>/charts", methods=["POST"])
@require_basic_auth
async def add_chart(dashboard_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("dashboards.list_dashboards"))
    form = await request.form
    try:
        title, template_id, query, options_json = _parse_chart_form_submission(form)
    except ValueError as ve:
        await flash(str(ve), "warning")
        return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))
    existing = _get_charts(db, dashboard_id)
    position = max((c["position"] for c in existing), default=-1) + 1
    chart_id = str(uuid.uuid4())
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": chart_id,
                "DashboardId": dashboard_id,
                "Title": title,
                "ChartType": template_id,
                "Query": query,
                "OptionsJson": options_json,
                "Position": position,
                "IsDeleted": 0,
                "Version": version,
            }
        ],
    )
    return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))


@dashboards_bp.route("/dashboards/<dashboard_id>/charts/<chart_id>/edit", methods=["POST"])
@require_basic_auth
async def edit_chart(dashboard_id: str, chart_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("dashboards.list_dashboards"))

    charts = _get_charts(db, dashboard_id)
    chart = next((c for c in charts if c["id"] == chart_id), None)
    if not chart:
        await flash("Chart not found", "warning")
        return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))

    form = await request.form
    try:
        title, template_id, query, options_json = _parse_chart_form_submission(form)
    except ValueError as ve:
        await flash(str(ve), "warning")
        return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))

    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": chart_id,
                "DashboardId": dashboard_id,
                "Title": title,
                "ChartType": template_id,
                "Query": query,
                "OptionsJson": options_json,
                "Position": chart["position"],
                "IsDeleted": 0,
                "Version": version,
            }
        ],
    )
    return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))


@dashboards_bp.route("/dashboards/<dashboard_id>/charts/<chart_id>/clone", methods=["POST"])
@require_basic_auth
async def clone_chart(dashboard_id: str, chart_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("dashboards.list_dashboards"))

    charts = _get_charts(db, dashboard_id)
    source_chart = next((c for c in charts if c["id"] == chart_id), None)
    if not source_chart:
        await flash("Chart not found", "warning")
        return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))

    form = await request.form
    try:
        title, template_id, query, options_json = _parse_chart_form_submission(form)
    except ValueError as ve:
        await flash(str(ve), "warning")
        return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))

    position = max((c["position"] for c in charts), default=-1) + 1
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": str(uuid.uuid4()),
                "DashboardId": dashboard_id,
                "Title": title,
                "ChartType": template_id,
                "Query": query,
                "OptionsJson": options_json,
                "Position": position,
                "IsDeleted": 0,
                "Version": version,
            }
        ],
    )
    return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))


@dashboards_bp.route("/dashboards/<dashboard_id>/charts/<chart_id>/delete", methods=["POST"])
@require_basic_auth
async def remove_chart(dashboard_id: str, chart_id: str):
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        await flash("Dashboard not found", "danger")
        return redirect(url_for("dashboards.list_dashboards"))
    charts = _get_charts(db, dashboard_id)
    chart = next((c for c in charts if c["id"] == chart_id), None)
    if not chart:
        await flash("Chart not found", "warning")
        return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": chart_id,
                "DashboardId": dashboard_id,
                "Title": chart["title"],
                "ChartType": chart["chart_type"],
                "Query": chart["query"],
                "OptionsJson": chart["options_json"],
                "Position": chart["position"],
                "IsDeleted": 1,
                "Version": version,
            }
        ],
    )
    return redirect(url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id))


@dashboards_bp.route("/api/dashboards/query", methods=["POST"])
@require_basic_auth
async def execute_chart_query():
    """Execute a ClickHouse SELECT query and return raw results for eChart rendering."""
    body = await request.get_json(silent=True) or {}
    query = (body.get("query") or "").strip()
    err = _validate_chart_query(query)
    if err:
        return jsonify({"error": err}), 400
    # Inject a row limit to prevent runaway queries
    if not re.search(r"\bLIMIT\b", query, re.IGNORECASE):
        query = query.rstrip(";") + " LIMIT 1000"
    db = get_db()
    try:
        result = db.execute(query)
        rows = result.fetchall()
        columns = list(rows[0].keys()) if rows else []
        data = [[row[col] for col in columns] for row in rows]
        return jsonify({"columns": columns, "rows": data})
    except Exception as exc:
        log.exception("Chart query execution failed: %s", query)
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400


@dashboards_bp.route("/api/dashboards/spec/templates", methods=["GET"])
@require_basic_auth
async def list_chart_spec_templates():
    templates = [
        {
            "id": tid,
            "name": t["name"],
            "description": t["description"],
            "query_shape": t.get("query_shape", ""),
            "sample_sql": t.get("sample_sql", ""),
            "default_spec": _default_chart_spec(tid),
            "min_columns": t.get("min_columns", 0),
            "max_columns": t.get("max_columns"),
            "column_roles": t.get("column_roles", {}),
        }
        for tid, t in sorted(CHART_TEMPLATES.items())
    ]
    return jsonify({"templates": templates})


@dashboards_bp.route("/api/dashboards/spec/options", methods=["GET"])
@require_basic_auth
async def chart_spec_options_api():
    source_view = str(request.args.get("source_view") or "v_derived_signals_anomaly").strip()
    signal_source = str(request.args.get("signal_source") or "").strip()
    limit = _coerce_positive_int(request.args.get("limit"), 100, 1, 500)

    supported_sources = {
        "v_derived_signals_anomaly",
        "v_otel_metrics_anomaly",
        "otel_metrics_gauge",
        "otel_metrics_sum",
        "otel_metrics_histogram",
        "otel_logs",
        "otel_traces",
        "sobs_error_resolutions",
    }
    if source_view not in supported_sources:
        return jsonify({"error": "Unsupported source for options"}), 400

    db = get_db()

    def _distinct_values(query: str) -> list[str]:
        rows = db.execute(query).fetchall()
        values: list[str] = []
        for row in rows:
            val = str(row["v"] or "").strip()
            if val:
                values.append(val)
        return values

    services: list[str] = []
    signals: list[str] = []
    metrics: list[str] = []

    if source_view == "v_derived_signals_anomaly":
        services = _distinct_values(
            "SELECT DISTINCT ServiceName AS v "
            "FROM v_derived_signals_anomaly "
            "WHERE time >= now() - INTERVAL 24 HOUR "
            "ORDER BY v "
            f"LIMIT {limit}"
        )
        signals = _distinct_values(
            "SELECT DISTINCT SignalName AS v "
            "FROM v_derived_signals_anomaly "
            f"{'WHERE' if signal_source else 'WHERE'} time >= now() - INTERVAL 24 HOUR"
            + (f" AND SignalSource = {_sql_literal(signal_source)}" if signal_source else "")
            + " ORDER BY v "
            f"LIMIT {limit}"
        )
    elif source_view in {"otel_logs", "otel_traces"}:
        services = _distinct_values(
            "SELECT DISTINCT ServiceName AS v " f"FROM {source_view} " "ORDER BY v " f"LIMIT {limit}"
        )
        signals = ["log_volume"] if source_view == "otel_logs" else ["trace_volume"]
    elif source_view == "sobs_error_resolutions":
        signals = ["resolved_error_volume"]
    elif source_view in {"v_otel_metrics_anomaly", "otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram"}:
        services = _distinct_values(
            "SELECT DISTINCT ServiceName AS v " f"FROM {source_view} " "ORDER BY v " f"LIMIT {limit}"
        )
        metrics = _distinct_values(
            "SELECT DISTINCT MetricName AS v " f"FROM {source_view} " "ORDER BY v " f"LIMIT {limit}"
        )

    return jsonify(
        {
            "source_view": source_view,
            "services": services,
            "signals": signals,
            "metrics": metrics,
        }
    )


@dashboards_bp.route("/api/dashboards/spec/compile", methods=["POST"])
@require_basic_auth
async def compile_chart_spec_api():
    body = await request.get_json(silent=True) or {}
    spec = body.get("spec") if isinstance(body, dict) else {}
    try:
        template_id, query, normalized_spec = _compile_chart_spec(spec)
    except ValueError as ve:
        return jsonify({"error": str(ve)}), 400
    except Exception as exc:
        log.exception("Chart spec compile failed")
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400
    return _jsonify_with_optional_sql_output_mask({"template_id": template_id, "query": query, "spec": normalized_spec})


@dashboards_bp.route("/api/dashboards/spec/dry-run", methods=["POST"])
@require_basic_auth
async def dry_run_chart_spec_api():
    body = await request.get_json(silent=True) or {}
    spec = body.get("spec") if isinstance(body, dict) else {}
    try:
        template_id, query, normalized_spec = _compile_chart_spec(spec)
    except ValueError as ve:
        return jsonify({"error": str(ve)}), 400

    run_query = query
    if not re.search(r"\bLIMIT\b", run_query, re.IGNORECASE):
        run_query = run_query.rstrip(";") + " LIMIT 20"
    db = get_db()
    try:
        result = db.execute(run_query)
        rows = result.fetchall()
        columns = list(rows[0].keys()) if rows else []
        data = [[row[col] for col in columns] for row in rows]
        column_types = _infer_column_types(columns, data)
    except Exception as exc:
        log.exception("Chart spec dry-run failed")
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400

    named_query_results = _execute_chart_spec_named_queries(
        db,
        normalized_spec.get("named_queries"),
        default_limit=5,
        include_records=False,
    )

    return _jsonify_with_optional_sql_output_mask(
        {
            "template_id": template_id,
            "query": query,
            "spec": normalized_spec,
            "columns": columns,
            "column_types": column_types,
            "rows": data,
            "named_query_results": named_query_results,
        }
    )


@dashboards_bp.route("/api/dashboards/spec/validate", methods=["POST"])
@require_basic_auth
async def validate_chart_spec_api():
    body = await request.get_json(silent=True) or {}
    spec = body.get("spec") if isinstance(body, dict) else {}
    try:
        template_id, query, normalized_spec = _compile_chart_spec(spec)
    except ValueError as ve:
        return jsonify({"valid": False, "error": str(ve)}), 400

    db = get_db()
    try:
        run_query = query
        if not re.search(r"\bLIMIT\b", run_query, re.IGNORECASE):
            run_query = run_query.rstrip(";") + " LIMIT 200"
        result = db.execute(run_query)
        raw_rows = result.fetchall()
        columns = list(raw_rows[0].keys()) if raw_rows else []
        data = [dict(row) for row in raw_rows]
        _render_chart_from_template(template_id, columns, data, normalized_spec)
    except Exception as exc:
        return jsonify({"valid": False, "error": _public_dashboard_query_error(exc)}), 400

    return _jsonify_with_optional_sql_output_mask(
        {
            "valid": True,
            "template_id": template_id,
            "query": query,
            "spec": normalized_spec,
            "columns": columns,
            "row_count": len(data),
        }
    )


@dashboards_bp.route("/api/dashboards/spec/render", methods=["POST"])
@require_basic_auth
async def render_chart_spec_api():
    body = await request.get_json(silent=True) or {}
    spec = body.get("spec") if isinstance(body, dict) else {}
    try:
        template_id, query, normalized_spec = _compile_chart_spec(spec)
    except ValueError as ve:
        return jsonify({"error": str(ve)}), 400

    db = get_db()
    try:
        run_query = query
        if not re.search(r"\bLIMIT\b", run_query, re.IGNORECASE):
            run_query = run_query.rstrip(";") + " LIMIT 1000"
        result = db.execute(run_query)
        raw_rows = result.fetchall()
        columns = list(raw_rows[0].keys()) if raw_rows else []
        data = [dict(row) for row in raw_rows]

        # Execute named queries and collect datasets
        named_datasets: dict[str, dict[str, object]] = {}
        named_query_results = _execute_chart_spec_named_queries(
            db,
            normalized_spec.get("named_queries"),
            default_limit=1000,
            include_records=True,
        )
        for nq in named_query_results:
            nq_name = str(nq.get("name") or "").strip()
            if not nq_name:
                continue
            if str(nq.get("error") or ""):
                log.warning("Named query '%s' failed during render: %s", nq_name, nq.get("error"))
            named_datasets[nq_name] = {
                "columns": nq.get("columns") or [],
                "records": nq.get("records") or [],
                "rows": nq.get("rows") or [],
            }

        option = _render_chart_from_template(template_id, columns, data, normalized_spec, named_datasets=named_datasets)
        option = _apply_chart_spec_visual_overrides(template_id, option, normalized_spec)
    except Exception as exc:
        log.exception("Chart spec render failed")
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400
    return _jsonify_with_optional_sql_output_mask(
        {"template_id": template_id, "query": query, "spec": normalized_spec, "option": option}
    )


@dashboards_bp.route("/api/dashboards/render", methods=["POST"])
@require_basic_auth
async def render_chart():
    """Execute a query and render with a template to produce eCharts option."""
    body = await request.get_json(silent=True) or {}
    query = (body.get("query") or "").strip()
    template_id = (body.get("template_id") or "time_series_percentiles").strip()

    err = _validate_chart_query(query)
    if err:
        return jsonify({"error": err}), 400

    if template_id not in CHART_TEMPLATES:
        return jsonify({"error": f"Unknown template: {template_id}"}), 400

    # Inject a row limit to prevent runaway queries
    if not re.search(r"\bLIMIT\b", query, re.IGNORECASE):
        query = query.rstrip(";") + " LIMIT 1000"

    db = get_db()
    try:
        result = db.execute(query)
        raw_rows = result.fetchall()
        columns = list(raw_rows[0].keys()) if raw_rows else []
        data = [dict(row) for row in raw_rows]

        # Render using template
        option = _render_chart_from_template(template_id, columns, data)
        return jsonify({"option": option})
    except ValueError as ve:
        # Template column mismatch
        return jsonify({"error": str(ve)}), 400
    except Exception as exc:
        log.exception("Chart render failed: template=%s query=%s", template_id, query)
        return jsonify({"error": _public_dashboard_query_error(exc)}), 400


def _execute_chart_spec_named_queries(
    db: "ChDbConnection",
    named_queries: object,
    *,
    default_limit: int,
    include_records: bool,
) -> list[dict[str, object]]:
    """Execute spec named queries with uniform output shape for dry-run/render."""
    results: list[dict[str, object]] = []
    if not isinstance(named_queries, list):
        return results
    for nq in named_queries:
        if not isinstance(nq, dict):
            continue
        nq_name = str(nq.get("name") or "").strip()
        nq_sql = str(nq.get("sql") or "").strip()
        if not nq_name or not nq_sql:
            continue
        nq_run = nq_sql if re.search(r"\bLIMIT\b", nq_sql, re.IGNORECASE) else f"{nq_sql} LIMIT {default_limit}"
        try:
            nq_result = db.execute(nq_run)
            nq_rows = nq_result.fetchall()
            nq_columns = list(nq_rows[0].keys()) if nq_rows else []
            nq_data = [[row[col] for col in nq_columns] for row in nq_rows]
            item: dict[str, object] = {
                "name": nq_name,
                "purpose": str(nq.get("purpose") or ""),
                "columns": nq_columns,
                "rows": nq_data,
                "error": "",
            }
            if include_records:
                item["records"] = [dict(row) for row in nq_rows]
            results.append(item)
        except Exception as exc:
            item = {
                "name": nq_name,
                "purpose": str(nq.get("purpose") or ""),
                "columns": [],
                "rows": [],
                "error": _public_dashboard_query_error(exc),
            }
            if include_records:
                item["records"] = []
            results.append(item)
    return results


@dashboards_bp.route("/api/dashboards/spec/ai-build", methods=["POST"])
@require_basic_auth
async def ai_build_chart_spec():
    """Generate a dashboard chart spec from a natural-language description using AI.

    Accepts JSON ``{question, preferred_chart_type, chart_instruction, thinking_level}``
    and returns ``{ok, spec, sql, named_queries, columns}``.
    """
    payload = await request.get_json(silent=True) or {}
    question = str(payload.get("question") or "").strip()
    preferred_chart_type = str(payload.get("preferred_chart_type") or "").strip()
    chart_instruction = str(payload.get("chart_instruction") or "").strip()
    thinking_level = _normalize_thinking_level(str(payload.get("thinking_level") or "off"))

    if not question:
        return jsonify({"ok": False, "error": "question is required"}), 400

    db = get_db()
    settings = _load_all_ai_settings(db)
    endpoint_url = settings.get("ai.endpoint_url", "").strip()
    model = settings.get("ai.model", "").strip()
    if not endpoint_url or not model:
        return jsonify({"ok": False, "error": "AI endpoint not configured. Visit Settings → AI Configuration."}), 503

    # Build schema context in a background thread
    runner = ChdbSqlRunner(db)
    schema_context = await asyncio.to_thread(runner.get_schema_context)

    # Generate primary SQL
    sql, sql_err, _sql_stats = await _vanna_generate_sql(
        question,
        schema_context,
        settings,
        preferred_chart_type=preferred_chart_type,
        chart_instruction=chart_instruction,
        thinking_level=thinking_level,
    )
    if sql_err:
        return jsonify({"ok": False, "error": f"SQL generation failed: {sql_err}"}), 503

    # Validate/execute primary SQL and auto-repair if needed.
    sql, primary_df, primary_error, sql_retry_count, _ = await _vanna_validate_and_execute_with_repair(
        db=db,
        question=question,
        schema_context=schema_context,
        initial_sql=sql,
        settings=settings,
        thinking_level=thinking_level,
    )
    if primary_error or primary_df is None:
        return (
            jsonify(
                {
                    "ok": False,
                    "error": primary_error or "Generated SQL could not be executed.",
                    "sql": sql,
                }
            ),
            422,
        )

    # Primary query data for chart generation context.
    columns: list[str] = []
    rows: list[list] = []
    datasets: list[dict[str, Any]] = []
    columns = list(primary_df.columns)
    rows = _json_safe_rows(primary_df.values.tolist()) if not primary_df.empty else []
    datasets.append(
        {
            "name": "main",
            "purpose": "primary dataset",
            "sql": sql,
            "columns": columns,
            "rows": rows,
        }
    )

    # Optionally generate named queries for complex multi-dataset charts
    named_query_results: list[dict[str, Any]] = []
    if columns:
        named_queries_raw, _, _ = await _vanna_generate_named_queries(
            question=question,
            schema_context=schema_context,
            base_sql=sql,
            settings=settings,
            preferred_chart_type=preferred_chart_type,
            chart_instruction=chart_instruction,
            thinking_level=thinking_level,
        )
        named_query_results = await _vanna_execute_named_queries(
            db=db,
            named_queries=named_queries_raw,
            question=question,
            schema_context=schema_context,
            settings=settings,
            thinking_level=thinking_level,
            use_repair=True,
        )
        for nq in named_query_results:
            if not str(nq.get("error") or ""):
                datasets.append(
                    {
                        "name": str(nq.get("name") or ""),
                        "purpose": str(nq.get("purpose") or ""),
                        "sql": str(nq.get("sql") or ""),
                        "columns": nq.get("columns") or [],
                        "rows": nq.get("rows") or [],
                    }
                )

    # Generate eCharts option JSON via LLM
    chart_spec_json = ""
    chart_error = ""
    custom_mapping_json = "{}"
    if columns:
        sample = [dict(zip(columns, r)) for r in rows[:20]]
        chart_spec_json, chart_error, _ = await _vanna_generate_chart_spec(
            columns,
            sample,
            question,
            settings,
            preferred_chart_type=preferred_chart_type,
            chart_instruction=chart_instruction,
            named_datasets=datasets,
            thinking_level=thinking_level,
        )
        if chart_spec_json:
            inferred_mapping = _infer_custom_mapping_from_option(chart_spec_json, columns)
            custom_mapping_json = json.dumps(inferred_mapping, ensure_ascii=False) if inferred_mapping else "{}"
        else:
            # Ensure the UI always gets a usable option JSON even when chart generation fails.
            chart_spec_json = _build_fallback_custom_option_json()
            custom_mapping_json = json.dumps({"points": {"from": "rows"}}, ensure_ascii=False)
            chart_error = (
                f"{chart_error} Using fallback chart option template."
                if chart_error
                else "Chart generation failed; using fallback chart option template."
            )

    named_queries: list[dict[str, str]] = [
        {
            "name": str(nq.get("name") or ""),
            "sql": str(nq.get("sql") or ""),
            "purpose": str(nq.get("purpose") or ""),
        }
        for nq in named_query_results
        if not str(nq.get("error") or "") and nq.get("name") and nq.get("sql")
    ]

    spec: dict[str, object] = {
        "template_id": "custom_echarts",
        "sql": {"mode": "raw", "override_sql": sql},
        "named_queries": named_queries,
        "visual": {
            "custom_option_json": chart_spec_json or "{}",
            "custom_mapping_json": custom_mapping_json,
        },
    }

    return jsonify(
        {
            "ok": True,
            "spec": spec,
            "sql": sql,
            "retry_count": sql_retry_count,
            "columns": columns,
            "named_queries": named_queries,
            "named_query_results": named_query_results,
            "chart_error": chart_error,
        }
    )


@dashboards_bp.route("/api/dashboards/<dashboard_id>/charts/<chart_id>/export", methods=["GET"])
@require_basic_auth
async def export_chart(dashboard_id: str, chart_id: str):
    """Export a chart configuration as a downloadable JSON template."""
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        return jsonify({"ok": False, "error": "Dashboard not found"}), 404

    charts = _get_charts(db, dashboard_id)
    chart = next((c for c in charts if c["id"] == chart_id), None)
    if not chart:
        return jsonify({"ok": False, "error": "Chart not found"}), 404

    template_payload: dict[str, object] = {
        "sobs_chart_template_version": 1,
        "title": chart["title"],
        "chart_spec": chart["chart_spec"],
    }

    safe_title = re.sub(r"[^a-zA-Z0-9_-]", "_", chart["title"])[:64] or "chart"
    filename = f"sobs_chart_{safe_title}.json"
    from quart import Response as QuartResponse

    return QuartResponse(
        json.dumps(template_payload, ensure_ascii=False, indent=2),
        mimetype="application/json",
        headers={"Content-Disposition": f'attachment; filename="{filename}"'},
    )


@dashboards_bp.route("/api/dashboards/<dashboard_id>/charts/import", methods=["POST"])
@require_basic_auth
async def import_chart(dashboard_id: str):
    """Import a chart from a JSON template and add it to the dashboard."""
    db = get_db()
    dashboard = _get_dashboard(db, dashboard_id)
    if not dashboard:
        return jsonify({"ok": False, "error": "Dashboard not found"}), 404

    payload = await request.get_json(silent=True) or {}

    template_version = payload.get("sobs_chart_template_version")
    if template_version != 1:
        return (
            jsonify(
                {
                    "ok": False,
                    "error": "Invalid or unsupported chart template format (expected sobs_chart_template_version: 1)",
                }
            ),
            400,
        )

    title = str(payload.get("title") or "").strip()
    if not title:
        title = "Imported Chart"

    chart_spec_raw = payload.get("chart_spec")
    if not chart_spec_raw:
        return jsonify({"ok": False, "error": "chart_spec is required in template"}), 400

    try:
        template_id, query, normalized_spec = _compile_chart_spec(chart_spec_raw)
    except Exception as exc:
        return jsonify({"ok": False, "error": f"Chart spec error: {exc}"}), 400

    options_json = json.dumps({"chart_spec": normalized_spec}, ensure_ascii=False)
    existing = _get_charts(db, dashboard_id)
    position = max((c["position"] for c in existing), default=-1) + 1

    chart_id_new = str(uuid.uuid4())
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_chart_configs",
        [
            {
                "Id": chart_id_new,
                "DashboardId": dashboard_id,
                "Title": title,
                "ChartType": template_id,
                "Query": query,
                "OptionsJson": options_json,
                "Position": position,
                "IsDeleted": 0,
                "Version": version,
            }
        ],
    )

    return jsonify(
        {
            "ok": True,
            "chart_id": chart_id_new,
            "dashboard_id": dashboard_id,
            "dashboard_url": url_for("dashboards.view_custom_dashboard", dashboard_id=dashboard_id),
        }
    )
