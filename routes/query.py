"""Query, table explorer, and chart types routes blueprint."""

from __future__ import annotations

import asyncio
import hashlib
import json
import os
import time
import uuid
from typing import Any

from quart import Blueprint, jsonify, render_template, request

from app import (  # noqa: E402
    _QUERY_ALLOWED_TABLES,
    ChdbSqlRunner,
    _check_guard_model,
    _emit_ai_helper_log_event,
    _guard_telemetry_attrs,
    _infer_query_field_types,
    _json_safe_rows,
    _jsonify_with_optional_sql_output_mask,
    _load_all_ai_settings,
    _normalize_thinking_level,
    _query_page_enabled,
    _summarize_query_llm_stats,
    _vanna_execute_named_queries,
    _vanna_explain_sql,
    _vanna_generate_chart_spec,
    _vanna_generate_named_queries,
    _vanna_generate_sql,
    _vanna_refine_chart_spec,
    _vanna_run_query,
    _vanna_validate_and_execute_with_repair,
    get_db,
    require_basic_auth,
)

query_bp = Blueprint("query", __name__)


@query_bp.route("/query")
@require_basic_auth
async def view_query():
    if not _query_page_enabled():
        return (
            "Query page is unavailable until AI and guard settings are configured.",
            404,
        )
    return await render_template("query.html")


@query_bp.route("/api/query/ask", methods=["POST"])
@require_basic_auth
async def api_query_ask():
    """Natural-language → SQL → DataFrame endpoint.

    Accepts JSON ``{question, execute, chart}`` and returns::

        {
          ok: bool,
                    trace_id: str,
                    turn_id: str,
          sql: str,
          columns: [...],
                    field_types: [{name, dtype, kind}, ...],
          rows: [[...], ...],
                    retry_count: int,
          chart_spec: str,   # ECharts option JSON, may be empty
          error: str
        }
    """
    payload = await request.get_json(force=True, silent=True) or {}
    question = str(payload.get("question") or "").strip()
    do_execute = bool(payload.get("execute", True))
    do_chart = bool(payload.get("chart", False))
    preferred_chart_type = str(payload.get("preferred_chart_type") or "").strip()
    chart_instruction = str(payload.get("chart_instruction") or "").strip()
    thinking_level = _normalize_thinking_level(str(payload.get("thinking_level") or "off"))

    if not question:
        return jsonify({"ok": False, "error": "question is required"}), 400

    db = get_db()
    settings = _load_all_ai_settings(db)
    if not _query_page_enabled(settings):
        return jsonify({"ok": False, "error": "Query page is unavailable."}), 404

    trace_id = hashlib.md5(f"query|{question}|{time.time_ns()}".encode("utf-8")).hexdigest()
    turn_id = trace_id[:16]
    model = settings.get("ai.model", "").strip()
    guard_model = settings.get("ai.guard_model", "").strip()

    _emit_ai_helper_log_event(
        event_name="query.turn.start",
        chat_id=trace_id,
        turn_id=turn_id,
        page="/query",
        model=model,
        guard_model=guard_model,
        thinking_level="off",
        body=question,
        attrs={"gen_ai.input.question": question},
    )

    allowed, guard_reason, guard_stats = await _check_guard_model(settings, question, "/query")
    _emit_ai_helper_log_event(
        event_name="query.guard.result",
        chat_id=trace_id,
        turn_id=turn_id,
        page="/query",
        model=model,
        guard_model=guard_model,
        thinking_level="off",
        body=f"Guard verdict: {guard_reason}",
        attrs=_guard_telemetry_attrs(allowed, guard_reason, guard_stats),
    )
    if not allowed:
        return (
            jsonify(
                {
                    "ok": False,
                    "error": f"Request blocked by safety guard: {guard_reason}",
                    "trace_id": trace_id,
                    "turn_id": turn_id,
                }
            ),
            403,
        )

    # Build schema context (run synchronously in a thread so we don't block the event loop)
    runner = ChdbSqlRunner(db)
    schema_context = await asyncio.to_thread(runner.get_schema_context)

    # Generate SQL
    sql, sql_err, sql_stats = await _vanna_generate_sql(
        question,
        schema_context,
        settings,
        preferred_chart_type=preferred_chart_type,
        chart_instruction=chart_instruction,
        thinking_level=thinking_level,
    )
    _emit_ai_helper_log_event(
        event_name="query.sql.generated",
        chat_id=trace_id,
        turn_id=turn_id,
        page="/query",
        model=model,
        guard_model=guard_model,
        thinking_level="off",
        body=sql if sql else sql_err,
        attrs={
            "gen_ai.operation.name": "query_sql",
            "gen_ai.usage.input_tokens": sql_stats.get("prompt_tokens", 0),
            "gen_ai.usage.output_tokens": sql_stats.get("completion_tokens", 0),
            "gen_ai.response.latency_ms": sql_stats.get("elapsed_ms", 0),
            "sobs.gen_ai.prompt": question,
            "sobs.gen_ai.response": sql,
        },
    )
    if sql_err:
        return (
            jsonify(
                {
                    "ok": False,
                    "error": sql_err,
                    "trace_id": trace_id,
                    "turn_id": turn_id,
                    "sql": "",
                    "columns": [],
                    "rows": [],
                    "llm_stats": _summarize_query_llm_stats(sql_generation=sql_stats),
                }
            ),
            503,
        )

    # Optionally execute
    columns: list[str] = []
    field_types: list[dict[str, str]] = []
    rows: list[list] = []
    datasets: list[dict[str, Any]] = []
    retry_count = 0
    exec_error = ""
    last_repair_stats: dict[str, Any] = {}
    named_stats: dict[str, Any] = {}
    chart_stats: dict[str, Any] = {}
    if do_execute:
        exec_started = time.monotonic()
        sql, main_df, exec_error, retry_count, last_repair_stats = await _vanna_validate_and_execute_with_repair(
            db=db,
            question=question,
            schema_context=schema_context,
            initial_sql=sql,
            settings=settings,
            thinking_level=thinking_level,
        )
        exec_elapsed_ms = int((time.monotonic() - exec_started) * 1000)
        row_count = int(len(main_df)) if main_df is not None else 0
        _emit_ai_helper_log_event(
            event_name="query.sql.executed",
            chat_id=trace_id,
            turn_id=turn_id,
            page="/query",
            model=model,
            guard_model=guard_model,
            thinking_level="off",
            body=sql,
            severity="INFO" if not exec_error else "ERROR",
            attrs={
                "gen_ai.operation.name": "query_sql_execute",
                "sobs.query.exec.attempt": max(1, retry_count + 1),
                "sobs.query.exec.status": "ok" if not exec_error else "error",
                "sobs.query.exec.row_count": row_count,
                "sobs.query.exec.error": exec_error,
                "gen_ai.response.latency_ms": exec_elapsed_ms,
                "sobs.gen_ai.prompt": question,
                "sobs.gen_ai.response": sql,
            },
        )

        if main_df is not None and not exec_error:
            if not main_df.empty:
                columns = list(main_df.columns)
                field_types = _infer_query_field_types(main_df)
                rows = _json_safe_rows(main_df.values.tolist())
            datasets.append(
                {
                    "name": "main",
                    "purpose": "primary dataset",
                    "sql": sql,
                    "columns": columns,
                    "field_types": field_types,
                    "rows": rows,
                    "error": "",
                }
            )

    # Optionally generate chart spec
    chart_spec = ""
    chart_error = ""
    if do_chart and not exec_error and columns:
        named_queries, _named_err, named_stats = await _vanna_generate_named_queries(
            question=question,
            schema_context=schema_context,
            base_sql=sql,
            settings=settings,
            preferred_chart_type=preferred_chart_type,
            chart_instruction=chart_instruction,
            thinking_level=thinking_level,
        )
        _emit_ai_helper_log_event(
            event_name="query.sql.named_generated",
            chat_id=trace_id,
            turn_id=turn_id,
            page="/query",
            model=model,
            guard_model=guard_model,
            thinking_level="off",
            body=json.dumps(named_queries, ensure_ascii=False),
            attrs={
                "gen_ai.operation.name": "query_sql_named",
                "gen_ai.usage.input_tokens": named_stats.get("prompt_tokens", 0),
                "gen_ai.usage.output_tokens": named_stats.get("completion_tokens", 0),
                "gen_ai.response.latency_ms": named_stats.get("elapsed_ms", 0),
            },
        )

        named_results = await _vanna_execute_named_queries(
            db=db,
            named_queries=named_queries,
            question=question,
            schema_context=schema_context,
            settings=settings,
            thinking_level=thinking_level,
            include_field_types=True,
            use_repair=False,
        )
        for ds in named_results:
            datasets.append(
                {
                    "name": str(ds.get("name") or "dataset"),
                    "purpose": str(ds.get("purpose") or ""),
                    "sql": str(ds.get("sql") or ""),
                    "columns": ds.get("columns") or [],
                    "field_types": ds.get("field_types") or [],
                    "rows": ds.get("rows") or [],
                    "error": str(ds.get("error") or ""),
                }
            )

        sample = [dict(zip(columns, r)) for r in rows[:20]]
        chart_spec, chart_error, chart_stats = await _vanna_generate_chart_spec(
            columns,
            sample,
            question,
            settings,
            preferred_chart_type=preferred_chart_type,
            chart_instruction=chart_instruction,
            named_datasets=datasets,
            thinking_level=thinking_level,
        )
        _emit_ai_helper_log_event(
            event_name="query.chart.generated",
            chat_id=trace_id,
            turn_id=turn_id,
            page="/query",
            model=model,
            guard_model=guard_model,
            thinking_level="off",
            body=chart_spec if chart_spec else chart_error,
            attrs={
                "gen_ai.operation.name": "query_chart",
                "gen_ai.usage.input_tokens": chart_stats.get("prompt_tokens", 0),
                "gen_ai.usage.output_tokens": chart_stats.get("completion_tokens", 0),
                "gen_ai.response.latency_ms": chart_stats.get("elapsed_ms", 0),
            },
        )

    _emit_ai_helper_log_event(
        event_name="query.turn.complete",
        chat_id=trace_id,
        turn_id=turn_id,
        page="/query",
        model=model,
        guard_model=guard_model,
        thinking_level="off",
        body="Query turn completed",
        attrs={
            "gen_ai.input.question": question,
            "sobs.gen_ai.prompt": question,
            "sobs.gen_ai.response": sql,
            "gen_ai.operation.name": "query",
        },
    )

    return _jsonify_with_optional_sql_output_mask(
        {
            "ok": True,
            "trace_id": trace_id,
            "turn_id": turn_id,
            "sql": sql,
            "columns": columns,
            "field_types": field_types,
            "rows": rows,
            "retry_count": retry_count,
            "datasets": datasets,
            "chart_spec": chart_spec,
            "error": exec_error or chart_error,
            "llm_stats": _summarize_query_llm_stats(
                sql_generation=sql_stats,
                sql_repair=last_repair_stats,
                named_query_generation=named_stats,
                chart_generation=chart_stats,
            ),
        }
    )


@query_bp.route("/api/query/run", methods=["POST"])
@require_basic_auth
async def api_query_run():
    """Execute an existing SQL statement and optionally generate a chart."""
    payload = await request.get_json(force=True, silent=True) or {}
    sql = str(payload.get("sql") or "").strip()
    question = str(payload.get("question") or "").strip()
    do_chart = bool(payload.get("chart", False))
    preferred_chart_type = str(payload.get("preferred_chart_type") or "").strip()
    chart_instruction = str(payload.get("chart_instruction") or "").strip()
    thinking_level = _normalize_thinking_level(str(payload.get("thinking_level") or "off"))

    if not sql:
        return jsonify({"ok": False, "error": "sql is required"}), 400

    db = get_db()
    settings = _load_all_ai_settings(db)
    if not _query_page_enabled(settings):
        return jsonify({"ok": False, "error": "Query page is unavailable."}), 404

    trace_id = hashlib.md5(f"query-run|{sql}|{time.time_ns()}".encode("utf-8")).hexdigest()
    turn_id = trace_id[:16]
    model = settings.get("ai.model", "").strip()
    guard_model = settings.get("ai.guard_model", "").strip()

    _emit_ai_helper_log_event(
        event_name="query.turn.start",
        chat_id=trace_id,
        turn_id=turn_id,
        page="/query",
        model=model,
        guard_model=guard_model,
        thinking_level="off",
        body=question or sql,
        attrs={"gen_ai.input.question": question or "(manual SQL execution)"},
    )

    exec_started = time.monotonic()
    # Pre-flight EXPLAIN to surface any parse/planning errors before execution.
    explain_error = await asyncio.to_thread(_vanna_explain_sql, db, sql)
    if explain_error:
        exec_elapsed_ms = int((time.monotonic() - exec_started) * 1000)
        _emit_ai_helper_log_event(
            event_name="query.sql.explain_failed",
            chat_id=trace_id,
            turn_id=turn_id,
            page="/query",
            model=model,
            guard_model=guard_model,
            thinking_level="off",
            body=explain_error,
            severity="WARN",
            attrs={"gen_ai.operation.name": "query_sql_explain", "sobs.query.exec.error": explain_error},
        )
        return (
            jsonify(
                {
                    "ok": False,
                    "error": explain_error,
                    "trace_id": trace_id,
                    "turn_id": turn_id,
                    "sql": sql,
                    "columns": [],
                    "rows": [],
                    "llm_stats": _summarize_query_llm_stats(),
                }
            ),
            422,
        )
    try:
        df, exec_error = await asyncio.to_thread(_vanna_run_query, db, sql)
    except Exception as exc:
        df, exec_error = None, f"Query execution error: {exc}"
    exec_elapsed_ms = int((time.monotonic() - exec_started) * 1000)

    row_count = 0
    columns: list[str] = []
    field_types: list[dict[str, str]] = []
    rows: list[list] = []
    datasets: list[dict[str, Any]] = []
    if df is not None:
        row_count = int(len(df))
        if not df.empty:
            columns = list(df.columns)
            field_types = _infer_query_field_types(df)
            rows = _json_safe_rows(df.values.tolist())
        datasets.append(
            {
                "name": "main",
                "purpose": "primary dataset",
                "sql": sql,
                "columns": columns,
                "field_types": field_types,
                "rows": rows,
                "error": "",
            }
        )

    _emit_ai_helper_log_event(
        event_name="query.sql.executed",
        chat_id=trace_id,
        turn_id=turn_id,
        page="/query",
        model=model,
        guard_model=guard_model,
        thinking_level="off",
        body=sql,
        severity="INFO" if not exec_error else "ERROR",
        attrs={
            "gen_ai.operation.name": "query_sql_execute",
            "sobs.query.exec.attempt": 1,
            "sobs.query.exec.status": "ok" if not exec_error else "error",
            "sobs.query.exec.row_count": row_count,
            "sobs.query.exec.error": exec_error,
            "gen_ai.response.latency_ms": exec_elapsed_ms,
            "sobs.gen_ai.prompt": question,
            "sobs.gen_ai.response": sql,
        },
    )

    chart_spec = ""
    chart_error = ""
    named_stats: dict[str, Any] = {}
    chart_stats: dict[str, Any] = {}
    if do_chart and not exec_error and columns:
        guard_input = question or f"Generate chart for SQL: {sql[:500]}"
        allowed, guard_reason, guard_stats = await _check_guard_model(settings, guard_input, "/query")
        _emit_ai_helper_log_event(
            event_name="query.guard.result",
            chat_id=trace_id,
            turn_id=turn_id,
            page="/query",
            model=model,
            guard_model=guard_model,
            thinking_level="off",
            body=f"Guard verdict: {guard_reason}",
            attrs=_guard_telemetry_attrs(allowed, guard_reason, guard_stats),
        )
        if allowed:
            schema_context = await asyncio.to_thread(ChdbSqlRunner(db).get_schema_context)
            named_queries, _named_err, named_stats = await _vanna_generate_named_queries(
                question=question or sql,
                schema_context=schema_context,
                base_sql=sql,
                settings=settings,
                preferred_chart_type=preferred_chart_type,
                chart_instruction=chart_instruction,
                thinking_level=thinking_level,
            )
            _emit_ai_helper_log_event(
                event_name="query.sql.named_generated",
                chat_id=trace_id,
                turn_id=turn_id,
                page="/query",
                model=model,
                guard_model=guard_model,
                thinking_level="off",
                body=json.dumps(named_queries, ensure_ascii=False),
                attrs={
                    "gen_ai.operation.name": "query_sql_named",
                    "gen_ai.usage.input_tokens": named_stats.get("prompt_tokens", 0),
                    "gen_ai.usage.output_tokens": named_stats.get("completion_tokens", 0),
                    "gen_ai.response.latency_ms": named_stats.get("elapsed_ms", 0),
                },
            )

            named_results = await _vanna_execute_named_queries(
                db=db,
                named_queries=named_queries,
                question=question or sql,
                schema_context=schema_context,
                settings=settings,
                thinking_level=thinking_level,
                include_field_types=True,
                use_repair=False,
            )
            for ds in named_results:
                datasets.append(
                    {
                        "name": str(ds.get("name") or "dataset"),
                        "purpose": str(ds.get("purpose") or ""),
                        "sql": str(ds.get("sql") or ""),
                        "columns": ds.get("columns") or [],
                        "field_types": ds.get("field_types") or [],
                        "rows": ds.get("rows") or [],
                        "error": str(ds.get("error") or ""),
                    }
                )

            sample = [dict(zip(columns, r)) for r in rows[:20]]
            chart_spec, chart_error, chart_stats = await _vanna_generate_chart_spec(
                columns,
                sample,
                question,
                settings,
                preferred_chart_type=preferred_chart_type,
                chart_instruction=chart_instruction,
                named_datasets=datasets,
                thinking_level=thinking_level,
            )
            _emit_ai_helper_log_event(
                event_name="query.chart.generated",
                chat_id=trace_id,
                turn_id=turn_id,
                page="/query",
                model=model,
                guard_model=guard_model,
                thinking_level="off",
                body=chart_spec if chart_spec else chart_error,
                attrs={
                    "gen_ai.operation.name": "query_chart",
                    "gen_ai.usage.input_tokens": chart_stats.get("prompt_tokens", 0),
                    "gen_ai.usage.output_tokens": chart_stats.get("completion_tokens", 0),
                    "gen_ai.response.latency_ms": chart_stats.get("elapsed_ms", 0),
                },
            )
        else:
            chart_error = f"Chart generation blocked by safety guard: {guard_reason}"

    final_error = exec_error or chart_error
    _emit_ai_helper_log_event(
        event_name="query.turn.complete",
        chat_id=trace_id,
        turn_id=turn_id,
        page="/query",
        model=model,
        guard_model=guard_model,
        thinking_level="off",
        body="Query turn completed",
        severity="INFO" if not final_error else "ERROR",
        attrs={
            "gen_ai.input.question": question,
            "sobs.gen_ai.prompt": question,
            "sobs.gen_ai.response": sql,
            "gen_ai.operation.name": "query",
        },
    )

    return _jsonify_with_optional_sql_output_mask(
        {
            "ok": True,
            "trace_id": trace_id,
            "turn_id": turn_id,
            "sql": sql,
            "columns": columns,
            "field_types": field_types,
            "rows": rows,
            "retry_count": 0,
            "datasets": datasets,
            "chart_spec": chart_spec,
            "error": final_error,
            "llm_stats": _summarize_query_llm_stats(
                named_query_generation=named_stats,
                chart_generation=chart_stats,
            ),
        }
    )


@query_bp.route("/api/query/refine-chart", methods=["POST"])
@require_basic_auth
async def api_query_refine_chart():
    """Refine an existing chart spec based on user instruction."""
    settings = _load_all_ai_settings(get_db())
    if not _query_page_enabled(settings):
        return jsonify({"ok": False, "error": "Query page is unavailable."}), 404

    payload = await request.get_json() or {}
    current_spec = payload.get("chart_spec", "")
    columns = payload.get("columns", [])
    rows = payload.get("rows", [])
    user_instruction = payload.get("instruction", "").strip()
    thinking_level = _normalize_thinking_level(str(payload.get("thinking_level") or "off"))

    if not current_spec:
        return jsonify({"ok": False, "error": "No chart spec provided."}), 400
    if not user_instruction:
        return jsonify({"ok": False, "error": "No instruction provided."}), 400

    # Use current row data as sample if available, otherwise empty list
    sample_rows = rows[:20] if rows else []

    trace_id = str(uuid.uuid4())
    turn_id = str(uuid.uuid4())
    model = settings.get("ai.model", "").strip()

    # Emit trace start event
    _emit_ai_helper_log_event(
        event_name="query.turn.start",
        chat_id=trace_id,
        turn_id=turn_id,
        page="/query",
        model=model,
        guard_model="",
        thinking_level="off",
        body=f"Chart refinement requested: {user_instruction}",
        attrs={
            "gen_ai.operation.name": "refine_chart",
            "sobs.gen_ai.instruction": user_instruction,
        },
    )

    chart_spec, chart_error, chart_stats = await _vanna_refine_chart_spec(
        current_spec, columns, sample_rows, user_instruction, settings, thinking_level=thinking_level
    )

    # Emit chart refinement event with LLM call details
    _emit_ai_helper_log_event(
        event_name="query.chart.refined",
        chat_id=trace_id,
        turn_id=turn_id,
        page="/query",
        model=model,
        guard_model="",
        thinking_level="off",
        body=chart_spec if chart_spec else chart_error,
        severity="ERROR" if chart_error else "INFO",
        attrs={
            "gen_ai.operation.name": "refine_chart",
            "gen_ai.usage.input_tokens": chart_stats.get("prompt_tokens", 0),
            "gen_ai.usage.output_tokens": chart_stats.get("completion_tokens", 0),
            "gen_ai.response.latency_ms": chart_stats.get("elapsed_ms", 0),
            "sobs.gen_ai.instruction": user_instruction,
        },
    )

    # Emit turn complete event
    _emit_ai_helper_log_event(
        event_name="query.turn.complete",
        chat_id=trace_id,
        turn_id=turn_id,
        page="/query",
        model=model,
        guard_model="",
        thinking_level="off",
        body="Chart refinement completed",
        severity="ERROR" if chart_error else "INFO",
        attrs={
            "gen_ai.operation.name": "refine_chart",
        },
    )

    if chart_error:
        return (
            jsonify(
                {
                    "ok": False,
                    "error": chart_error,
                    "trace_id": trace_id,
                }
            ),
            500,
        )

    return jsonify(
        {
            "ok": True,
            "trace_id": trace_id,
            "chart_spec": chart_spec,
        }
    )


@query_bp.route("/api/query/schema", methods=["GET"])
@require_basic_auth
async def api_query_schema():
    """Return the schema context string used for LLM prompts."""
    settings = _load_all_ai_settings(get_db())
    if not _query_page_enabled(settings):
        return jsonify({"ok": False, "error": "Query page is unavailable."}), 404
    db = get_db()
    runner = ChdbSqlRunner(db)
    schema = await asyncio.to_thread(runner.get_schema_context)
    return jsonify({"ok": True, "schema": schema})


@query_bp.route("/table-explorer")
@require_basic_auth
async def view_table_explorer():
    """Render the visual database table explorer page."""
    if not _query_page_enabled():
        return (
            "Table Explorer is unavailable until AI and guard settings are configured.",
            404,
        )
    return await render_template("table_explorer.html")


@query_bp.route("/api/table-explorer/tables", methods=["GET"])
@require_basic_auth
async def api_table_explorer_tables():
    """Return metadata for all allowed tables (name, column count, columns).

    Response shape::

        {
            "ok": true,
            "tables": [
                {
                    "name": "otel_logs",
                    "column_count": 12,
                    "columns": [
                        {
                            "name": "Timestamp",
                            "type": "DateTime64(9)",
                            "is_nullable": false,
                            "is_primary_key": false,
                            "is_sorting_key": true,
                            "is_partition_key": false,
                            "default_kind": "",
                            "comment": ""
                        },
                        ...
                    ]
                },
                ...
            ]
        }
    """
    if not _query_page_enabled():
        return jsonify({"ok": False, "error": "Table Explorer is unavailable."}), 404
    db = get_db()
    runner = ChdbSqlRunner(db)
    try:
        tables = await asyncio.to_thread(runner.get_allowed_tables_info)
        return jsonify({"ok": True, "tables": tables})
    except Exception as exc:
        return jsonify({"ok": False, "error": str(exc)}), 500


@query_bp.route("/api/table-explorer/table/<name>", methods=["GET"])
@require_basic_auth
async def api_table_explorer_table(name: str):
    """Return detailed info for a single allowed table: columns, DDL, and sample rows.

    Response shape::

        {
            "ok": true,
            "table": "otel_logs",
            "columns": [...],
            "ddl": "CREATE TABLE otel_logs ...",
            "sample": {
                "columns": ["Timestamp", "ServiceName", ...],
                "rows": [["2024-01-01 00:00:00", "my-svc", ...], ...]
            }
        }
    """
    if not _query_page_enabled():
        return jsonify({"ok": False, "error": "Table Explorer is unavailable."}), 404

    # Validate table is in the allowlist
    if name not in _QUERY_ALLOWED_TABLES:
        return jsonify({"ok": False, "error": f"Table '{name}' is not accessible."}), 403

    db = get_db()
    runner = ChdbSqlRunner(db)
    try:
        detail = await asyncio.to_thread(runner.get_table_detail, name)
        return jsonify(
            {
                "ok": True,
                "table": name,
                "columns": detail["columns"],
                "ddl": detail["ddl"],
                "sample": detail["sample"],
            }
        )
    except Exception as exc:
        return jsonify({"ok": False, "error": str(exc)}), 500


@query_bp.route("/api/chart-types", methods=["GET"])
@require_basic_auth
async def api_chart_types():
    """Return the catalog of available ECharts chart types with configurations."""
    try:
        import json as json_module

        chart_types_path = os.path.join(
            os.path.dirname(os.path.dirname(__file__)), "static", "echarts-chart-types.json"
        )
        if not os.path.exists(chart_types_path):
            return (
                jsonify(
                    {
                        "ok": False,
                        "error": "Chart types catalog not found. Run: node scripts/extract-echarts-types.js",
                    }
                ),
                404,
            )

        with open(chart_types_path, "r") as f:
            catalog = json_module.load(f)

        return jsonify({"ok": True, "data": catalog})
    except Exception as e:
        return (
            jsonify({"ok": False, "error": f"Failed to load chart types: {str(e)}"}),
            500,
        )
