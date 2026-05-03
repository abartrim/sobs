"""AI transparency, AI helper chat, and agent routes blueprint."""

from __future__ import annotations

import asyncio
import json
import logging
import re
import time
import uuid
from typing import Any, AsyncIterator, cast

from quart import Blueprint, Response, jsonify, render_template, request

from app import (  # noqa: E402
    _AI_HELPER_SERVICE_NAME,
    _AI_MEMORY_CONSOLIDATION_SCORE,
    _AI_SPAN_CONDITION,
    _AI_THINKING_LEVELS,
    _AI_TRACE_PROMPT_SQL,
    _AI_TRACE_RESPONSE_SQL,
    _action_meta_for_id,
    _action_meta_for_page,
    _agent_rule_last_run_ts,
    _build_ai_trace_turn_cards,
    _build_ai_turn_logs_url,
    _build_client_action,
    _chat_label_from_first_turn,
    _check_guard_model,
    _coerce_summary_value,
    _consolidate_memory_candidates,
    _decode_ai_action_token,
    _dedupe_system_input_messages,
    _derive_turn_summary,
    _emit_ai_helper_log_event,
    _error_id,
    _extract_assistant_meta,
    _extract_memory_candidates,
    _extract_messages_text,
    _get_ai_filter_metadata,
    _guard_telemetry_attrs,
    _helper_action_manifest_for_page,
    _helper_tools_for_page,
    _insert_rows_json_each_row,
    _issue_ai_action_token,
    _jsonify_with_optional_sql_output_mask,
    _load_agent_rule,
    _load_agent_runs,
    _load_ai_pricing_with_sources,
    _load_all_ai_settings,
    _load_chat_memories,
    _load_chat_tool_history,
    _load_recent_chat_turns,
    _load_recent_turn_summaries,
    _map_to_dict,
    _maybe_await,
    _model_supports_thinking,
    _model_supports_tools,
    _normalize_ai_sql_where,
    _normalize_genai_messages_for_display,
    _normalize_generic_ui_action_tool_call,
    _normalize_thinking_level,
    _parse_genai_messages_json,
    _parse_limit,
    _parse_offset,
    _parse_sort,
    _parse_time_window_args,
    _public_dashboard_query_error,
    _run_agent_rule_instance,
    _semantic_memory_matches,
    _sse_json_event,
    _stream_llm_endpoint,
    _suggest_chart_dashboard_pivot_tool,
    _time_window_conditions,
    _upsert_ai_memory,
    _where_clause,
    get_db,
    require_basic_auth,
)

ai_bp = Blueprint("ai", __name__)
log = logging.getLogger("sobs")


@ai_bp.route("/ai")
@require_basic_auth
async def view_ai():
    db = get_db()
    selected_services = [svc.strip() for svc in request.args.getlist("service") if svc.strip()]
    selected_models = [mdl.strip() for mdl in request.args.getlist("model") if mdl.strip()]
    selected_operations = [op.strip() for op in request.args.getlist("operation") if op.strip()]
    selected_span_names = [sn.strip() for sn in request.args.getlist("span_name") if sn.strip()]
    selected_row_types = [rt.strip().lower() for rt in request.args.getlist("row_type") if rt.strip()]
    selected_row_types = [rt for rt in selected_row_types if rt in ("llm", "system")]

    service = selected_services[0] if selected_services else ""
    model = selected_models[0] if selected_models else ""
    operation_filter = selected_operations[0] if selected_operations else ""
    span_name = selected_span_names[0] if selected_span_names else ""
    row_type = selected_row_types[0] if selected_row_types else ""
    sql_where = request.args.get("sql", "").strip()
    from_ts, to_ts, time_error = _parse_time_window_args()
    view_mode = request.args.get("view", "flat").strip().lower()
    if view_mode not in ("flat", "trace"):
        view_mode = "flat"
    limit = _parse_limit(50)
    offset = _parse_offset()
    sort_by, sort_col, sort_dir = _parse_sort(
        {"Timestamp": "Timestamp", "Duration": "Duration", "ServiceName": "ServiceName"},
        "Timestamp",
    )
    order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"

    conditions = []
    params = []
    error_msg = time_error
    base_ai_condition = _AI_SPAN_CONDITION
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    where = "WHERE " + base_ai_condition
    if sql_where and not error_msg:
        try:
            safe_sql = _normalize_ai_sql_where(sql_where)
            sql_conditions = [f"({safe_sql})", base_ai_condition]
            sql_conditions.extend(time_conditions)
            where = "WHERE " + " AND ".join(sql_conditions)
            params = list(time_params)
        except Exception as exc:
            error_msg = f"SQL error: {_public_dashboard_query_error(exc)}"
            where = "WHERE " + base_ai_condition
    elif not error_msg:
        if selected_services:
            placeholders = ",".join(["?"] * len(selected_services))
            conditions.append(f"ServiceName IN ({placeholders})")
            params.extend(selected_services)
        if selected_models:
            placeholders = ",".join(["?"] * len(selected_models))
            conditions.append(f"SpanAttributes['gen_ai.request.model'] IN ({placeholders})")
            params.extend(selected_models)
        if selected_operations:
            operation_conditions = []
            for selected_operation in selected_operations:
                if selected_operation.lower() == "chat":
                    operation_conditions.append(
                        "(SpanAttributes['gen_ai.operation.name']=? OR SpanAttributes['gen_ai.operation.name']='')"
                    )
                    params.append("chat")
                else:
                    operation_conditions.append("SpanAttributes['gen_ai.operation.name']=?")
                    params.append(selected_operation)
            if operation_conditions:
                conditions.append("(" + " OR ".join(operation_conditions) + ")")
        if selected_span_names:
            placeholders = ",".join(["?"] * len(selected_span_names))
            conditions.append(f"SpanName IN ({placeholders})")
            params.extend(selected_span_names)

        selected_row_type_set = set(selected_row_types)
        if selected_row_type_set == {"llm"}:
            conditions.append("SpanAttributes['gen_ai.request.model'] != ''")
        elif selected_row_type_set == {"system"}:
            conditions.append("SpanAttributes['gen_ai.request.model'] = ''")
        conditions.append(base_ai_condition)
        conditions.extend(time_conditions)
        params.extend(time_params)
        where = _where_clause(conditions)

    trace_ids: list[str] = []
    total = 0
    rows = []
    if not error_msg:
        try:
            if view_mode == "trace":
                trace_conditions = list(conditions)
                if sql_where:
                    trace_where = f"{where} AND TraceId != ''"
                else:
                    trace_conditions.append("TraceId != ''")
                    trace_where = "WHERE " + " AND ".join(trace_conditions)
                total = db.execute(f"SELECT uniq(TraceId) FROM otel_traces {trace_where}", params).fetchone()[0]
                trace_rows = db.execute(
                    f"SELECT TraceId, MAX(Timestamp) AS LastTs FROM otel_traces "
                    f"{trace_where} GROUP BY TraceId "
                    f"ORDER BY LastTs {'ASC' if sort_dir == 'asc' else 'DESC'} LIMIT ? OFFSET ?",
                    params + [limit, offset],
                ).fetchall()
                trace_ids = [str(r["TraceId"]) for r in trace_rows if str(r["TraceId"])]
                if trace_ids:
                    placeholders = ",".join(["?"] * len(trace_ids))
                    detail_where = f"{trace_where} AND TraceId IN ({placeholders})"
                    rows = db.execute(
                        f"SELECT Timestamp, ServiceName, TraceId, SpanName, Duration, SpanAttributes "
                        f"FROM otel_traces {detail_where} "
                        "ORDER BY Timestamp ASC",
                        params + trace_ids,
                    ).fetchall()
            else:
                total = db.execute(f"SELECT COUNT(*) FROM otel_traces {where}", params).fetchone()[0]
                rows = db.execute(
                    f"SELECT Timestamp, ServiceName, TraceId, SpanName, Duration, SpanAttributes "
                    f"FROM otel_traces {where} {order_clause} LIMIT ? OFFSET ?",
                    params + [limit, offset],
                ).fetchall()
        except Exception as exc:
            error_msg = f"SQL error: {_public_dashboard_query_error(exc)}"
            total = 0
            rows = []
            trace_ids = []

    def _safe_attr_int(attrs: dict[str, object], key: str) -> int:
        raw_value = attrs.get(key, "0")
        try:
            parsed = float(str(raw_value or 0))
        except (TypeError, ValueError):
            return 0
        if parsed != parsed or parsed in (float("inf"), float("-inf")):
            return 0
        return int(parsed)

    def _safe_duration_ms(duration_ns: object) -> float:
        try:
            parsed = float(str(duration_ns or 0))
        except (TypeError, ValueError):
            return 0.0
        if parsed != parsed or parsed in (float("inf"), float("-inf")):
            return 0.0
        return round(parsed / 1_000_000, 1)

    ai_items = []
    for r in rows:
        attrs = _map_to_dict(r["SpanAttributes"])
        ts = str(r["Timestamp"])
        # Coalesce provider: canonical gen_ai.provider.name with legacy gen_ai.system fallback
        provider = str(attrs.get("gen_ai.provider.name") or attrs.get("gen_ai.system", ""))
        req_model = str(attrs.get("gen_ai.request.model", ""))
        operation = str(attrs.get("gen_ai.operation.name", "chat"))
        # Coalesce prompt/response: OTel standard fields first, sobs legacy fields as fallback
        input_messages_raw = str(attrs.get("gen_ai.input.messages", ""))
        output_messages_raw = str(attrs.get("gen_ai.output.messages", ""))
        system_instructions_raw = str(attrs.get("gen_ai.system_instructions", ""))
        prompt = _extract_messages_text(input_messages_raw) or str(attrs.get("sobs.gen_ai.prompt", ""))
        response = _extract_messages_text(output_messages_raw) or str(attrs.get("sobs.gen_ai.response", ""))
        tokens_in = _safe_attr_int(attrs, "gen_ai.usage.input_tokens")
        tokens_out = _safe_attr_int(attrs, "gen_ai.usage.output_tokens")
        err_type = str(attrs.get("error.type", ""))
        msg = str(attrs.get("exception.message", ""))
        duration_ms = _safe_duration_ms(r["Duration"])
        tokens_per_sec = round(tokens_out / (duration_ms / 1000), 1) if duration_ms > 0 and tokens_out > 0 else 0
        # Additional OTel GenAI attributes
        finish_reason = str(attrs.get("gen_ai.response.finish_reason", ""))
        item_span_name = str(r["SpanName"] or "")
        temperature = str(attrs.get("gen_ai.request.temperature", ""))
        max_tokens = str(attrs.get("gen_ai.request.max_tokens", ""))
        thinking_tokens = _safe_attr_int(attrs, "gen_ai.usage.thinking_tokens")
        event_name = str(attrs.get("sobs.ai.event") or "")
        if not event_name and item_span_name.startswith("ai."):
            event_name = item_span_name[3:]
        # Build structured messages for conversation view
        input_messages = []
        output_messages = []
        input_messages = _normalize_genai_messages_for_display(_parse_genai_messages_json(input_messages_raw))
        output_messages = _normalize_genai_messages_for_display(_parse_genai_messages_json(output_messages_raw))
        input_messages, deduped_system_message_count = _dedupe_system_input_messages(
            input_messages,
            system_instructions_raw,
        )
        row_id = _error_id(ts, r["ServiceName"], provider, req_model + err_type + msg, r["TraceId"], "")
        ai_items.append(
            {
                "id": row_id,
                "ts": ts,
                "service": r["ServiceName"],
                "provider": provider,
                "model": req_model,
                "operation": operation,
                "span_name": item_span_name,
                "is_llm_call": bool(
                    req_model
                    and (
                        tokens_in > 0
                        or tokens_out > 0
                        or response
                        or input_messages
                        or output_messages
                        or bool(system_instructions_raw.strip())
                    )
                ),
                "prompt": prompt,
                "response": response,
                "input_messages": input_messages,
                "output_messages": output_messages,
                "input_messages_json": input_messages_raw,
                "output_messages_json": output_messages_raw,
                "system_instructions": system_instructions_raw,
                "system_message_deduped_count": deduped_system_message_count,
                "tokens_in": tokens_in,
                "tokens_out": tokens_out,
                "thinking_tokens": thinking_tokens,
                "duration_ms": duration_ms,
                "tokens_per_sec": tokens_per_sec,
                "trace_id": r["TraceId"],
                "chat_id": str(attrs.get("gen_ai.chat_id", "")),
                "turn_id": str(attrs.get("gen_ai.turn_id", "") or attrs.get("gen_ai.response.id", "")),
                "event_name": event_name,
                "input_question": str(attrs.get("gen_ai.input.question", "")),
                "turn_summary_request": str(attrs.get("gen_ai.turn.summary.request", "")),
                "turn_summary_action": str(attrs.get("gen_ai.turn.summary.action", "")),
                "turn_summary_result": str(attrs.get("gen_ai.turn.summary.result", "")),
                "guard_allowed": attrs.get("gen_ai.guard.allowed", ""),
                "guard_reason": str(attrs.get("gen_ai.guard.reason", "")),
                "tool_name": str(attrs.get("gen_ai.tool.name", "")),
                "tool_status": str(attrs.get("sobs.ai.action.status", "")),
                "tool_summary": str(attrs.get("sobs.ai.tool.summary", "")),
                "tool_action": str(attrs.get("sobs.ai.tool.action", "")),
                "tool_action_id": str(attrs.get("sobs.ai.action_id", "")),
                "error_type": err_type,
                "error_message": msg,
                "finish_reason": finish_reason,
                "temperature": temperature,
                "max_tokens": max_tokens,
            }
        )

    trace_groups = []
    if view_mode == "trace":
        by_trace: dict[str, dict] = {
            tid: {
                "id": _error_id("", "", "trace", tid, tid, ""),
                "trace_id": tid,
                "spans": [],
                "calls": 0,
                "tokens_in": 0,
                "tokens_out": 0,
                "errors": 0,
                "services": set(),
                "models": set(),
                "operations": set(),
                "first_ts": "",
                "last_ts": "",
            }
            for tid in trace_ids
        }
        for item in ai_items:
            tid = str(item.get("trace_id", ""))
            if not tid or tid not in by_trace:
                continue
            grp = by_trace[tid]
            grp["spans"].append(item)
            grp["calls"] += 1
            grp["tokens_in"] += int(item.get("tokens_in", 0) or 0)
            grp["tokens_out"] += int(item.get("tokens_out", 0) or 0)
            if item.get("error_type"):
                grp["errors"] += 1
            svc = str(item.get("service", ""))
            mdl = str(item.get("model", ""))
            op = str(item.get("operation", ""))
            if svc:
                grp["services"].add(svc)
            if mdl:
                grp["models"].add(mdl)
            if op:
                grp["operations"].add(op)
            ts = str(item.get("ts", ""))
            if ts:
                if not grp["first_ts"] or ts < grp["first_ts"]:
                    grp["first_ts"] = ts
                if not grp["last_ts"] or ts > grp["last_ts"]:
                    grp["last_ts"] = ts

        for tid in trace_ids:
            grp = by_trace[tid]
            if not grp["spans"]:
                continue
            grp["services"] = sorted(grp["services"])
            grp["models"] = sorted(grp["models"])
            grp["operations"] = sorted(grp["operations"])
            grp["turn_cards"] = _build_ai_trace_turn_cards(cast(list[dict[str, Any]], grp["spans"]))
            trace_groups.append(grp)

    services: list[str] = []
    models: list[str] = []
    operations: list[str] = []
    span_names: list[str] = []
    totals: dict[str, int] = {"ti": 0, "to_": 0, "cnt": 0, "errors": 0}
    metadata = _get_ai_filter_metadata(db, from_ts, to_ts)
    services = cast(list[str], metadata.get("services", []))
    models = cast(list[str], metadata.get("models", []))
    operations = cast(list[str], metadata.get("operations", []))
    span_names = cast(list[str], metadata.get("span_names", []))
    metadata_errors = cast(list[str], metadata.get("errors", []))

    try:
        totals_where = where if where else f"WHERE {_AI_SPAN_CONDITION}"
        totals_params = list(params) if where else []
        totals_row = db.execute(
            "SELECT "
            "SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.input_tokens'])) ti, "
            "SUM(toUInt64OrZero(SpanAttributes['gen_ai.usage.output_tokens'])) to_, "
            "COUNT(*) cnt, "
            "countIf(SpanAttributes['error.type'] != '') errors "
            "FROM otel_traces "
            f"{totals_where}",
            totals_params,
        ).fetchone()
        if totals_row:
            totals = {
                "ti": int(totals_row["ti"] or 0),
                "to_": int(totals_row["to_"] or 0),
                "cnt": int(totals_row["cnt"] or 0),
                "errors": int(totals_row["errors"] or 0),
            }
    except Exception as exc:
        metadata_errors.append(f"totals={_public_dashboard_query_error(exc)}")

    if metadata_errors:
        metadata_error_text = "Some AI metadata failed to load: " + "; ".join(metadata_errors[:3])
        error_msg = f"{error_msg}; {metadata_error_text}" if error_msg else metadata_error_text

    ai_pricing, ai_pricing_sources = _load_ai_pricing_with_sources(db)

    return await render_template(
        "ai.html",
        ai_items=ai_items,
        total=total,
        limit=limit,
        offset=offset,
        service=service,
        selected_services=selected_services,
        model=model,
        selected_models=selected_models,
        operation=operation_filter,
        selected_operations=selected_operations,
        span_name=span_name,
        selected_span_names=selected_span_names,
        row_type=row_type,
        selected_row_types=selected_row_types,
        sql_where=sql_where,
        view_mode=view_mode,
        services=services,
        models=models,
        operations=operations,
        span_names=span_names,
        trace_groups=trace_groups,
        total_tokens_in=totals["ti"],
        total_tokens_out=totals["to_"],
        total_calls=totals["cnt"],
        total_errors=totals["errors"],
        error_msg=error_msg,
        sort_by=sort_by,
        sort_dir=sort_dir,
        from_ts=from_ts,
        to_ts=to_ts,
        ai_pricing_json=ai_pricing,
        ai_pricing_sources_json=ai_pricing_sources,
    )


@ai_bp.route("/api/ai/span-attributes")
@require_basic_auth
async def get_ai_span_attributes():
    db = get_db()
    ts = request.args.get("ts", "").strip()
    service = request.args.get("service", "").strip()
    trace_id = request.args.get("trace_id", "").strip()
    span_name = request.args.get("span_name", "").strip()

    if not ts or not service:
        return jsonify({"ok": False, "error": "Missing required params: ts and service"}), 400

    conditions = [
        _AI_SPAN_CONDITION,
        "Timestamp=?",
        "ServiceName=?",
    ]
    params: list[Any] = [ts, service]
    if trace_id:
        conditions.append("TraceId=?")
        params.append(trace_id)
    if span_name:
        conditions.append("SpanName=?")
        params.append(span_name)

    try:
        row = db.execute(
            "SELECT SpanAttributes FROM otel_traces "
            f"WHERE {' AND '.join(conditions)} "
            "ORDER BY Timestamp DESC LIMIT 1",
            params,
        ).fetchone()
        if row is None:
            return jsonify({"ok": False, "error": "Span not found"}), 404
        attrs = _map_to_dict(row["SpanAttributes"])
        raw_attrs = json.dumps(attrs, ensure_ascii=False, indent=2)
        return _jsonify_with_optional_sql_output_mask({"ok": True, "raw_attrs": raw_attrs})
    except Exception as exc:
        log.warning("Error fetching AI span attributes: %s", exc)
        return jsonify({"ok": False, "error": "Failed to load span attributes"}), 500


@ai_bp.route("/api/ai/conversation")
@require_basic_auth
async def get_ai_conversation():
    """Return rendered conversation tab HTML for a single AI span."""
    db = get_db()
    ts = request.args.get("ts", "").strip()
    service = request.args.get("service", "").strip()
    trace_id = request.args.get("trace_id", "").strip()
    span_name = request.args.get("span_name", "").strip()
    from_ts = request.args.get("from_ts", "").strip()
    to_ts = request.args.get("to_ts", "").strip()

    if not ts or not service:
        return "<p class='text-danger small'>Missing required params: ts and service.</p>", 400

    conditions = [_AI_SPAN_CONDITION, "Timestamp=?", "ServiceName=?"]
    params: list[Any] = [ts, service]
    if trace_id:
        conditions.append("TraceId=?")
        params.append(trace_id)
    if span_name:
        conditions.append("SpanName=?")
        params.append(span_name)

    try:
        row = db.execute(
            "SELECT SpanAttributes FROM otel_traces "
            f"WHERE {' AND '.join(conditions)} "
            "ORDER BY Timestamp DESC LIMIT 1",
            params,
        ).fetchone()
        if row is None:
            return "<p class='text-danger small'>Span not found.</p>", 404
        attrs = _map_to_dict(row["SpanAttributes"])
        input_messages_raw = str(attrs.get("gen_ai.input.messages", ""))
        output_messages_raw = str(attrs.get("gen_ai.output.messages", ""))
        system_instructions_raw = str(attrs.get("gen_ai.system_instructions", ""))
        prompt = _extract_messages_text(input_messages_raw) or str(attrs.get("sobs.gen_ai.prompt", ""))
        response_text = _extract_messages_text(output_messages_raw) or str(attrs.get("sobs.gen_ai.response", ""))
        err_type = str(attrs.get("error.type", ""))
        err_msg = str(attrs.get("exception.message", ""))
        finish_reason = str(attrs.get("gen_ai.response.finish_reason", ""))
        operation = str(attrs.get("gen_ai.operation.name", "chat"))
        input_messages = _normalize_genai_messages_for_display(_parse_genai_messages_json(input_messages_raw))
        output_messages = _normalize_genai_messages_for_display(_parse_genai_messages_json(output_messages_raw))
        input_messages, deduped_count = _dedupe_system_input_messages(input_messages, system_instructions_raw)
        item: dict[str, Any] = {
            "service": service,
            "trace_id": trace_id,
            "error_type": err_type,
            "error_message": err_msg,
            "system_instructions": system_instructions_raw,
            "system_message_deduped_count": deduped_count,
            "input_messages": input_messages,
            "output_messages": output_messages,
            "prompt": prompt,
            "response": response_text,
            "operation": operation,
            "finish_reason": finish_reason,
        }
        html = await render_template(
            "_ai_conversation_partial.html",
            item=item,
            from_ts=from_ts,
            to_ts=to_ts,
        )
        return html, 200, {"Content-Type": "text/html; charset=utf-8"}
    except Exception as exc:
        log.warning("Error fetching AI conversation: %s", exc)
        return "<p class='text-danger small'>Error loading conversation.</p>", 500


@ai_bp.route("/api/ai/export")
@require_basic_auth
async def export_ai_training():
    """Export AI call data as JSONL for training dataset creation."""
    db = get_db()
    service = request.args.get("service", "").strip()
    model = request.args.get("model", "").strip()
    operation_filter = request.args.get("operation", "").strip()
    from_ts, to_ts, _time_error = _parse_time_window_args()
    fmt = request.args.get("format", "jsonl").strip().lower()
    try:
        max_rows = max(1, min(int(request.args.get("limit", 1000)), 5000))
    except (ValueError, TypeError):
        max_rows = 1000

    conditions = [
        _AI_SPAN_CONDITION,
    ]
    params: list = []
    if service:
        conditions.append("ServiceName=?")
        params.append(service)
    if model:
        conditions.append("SpanAttributes['gen_ai.request.model']=?")
        params.append(model)
    if operation_filter:
        if operation_filter.lower() == "chat":
            conditions.append(
                "(SpanAttributes['gen_ai.operation.name']=? OR SpanAttributes['gen_ai.operation.name']='')"
            )
            params.append("chat")
        else:
            conditions.append("SpanAttributes['gen_ai.operation.name']=?")
            params.append(operation_filter)
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    conditions.extend(time_conditions)
    params.extend(time_params)
    where = "WHERE " + " AND ".join(conditions)

    rows = db.execute(
        f"SELECT Timestamp, ServiceName, TraceId, Duration, SpanAttributes "
        f"FROM otel_traces {where} ORDER BY Timestamp DESC LIMIT ?",
        params + [max_rows],
    ).fetchall()

    records = []
    for r in rows:
        attrs = _map_to_dict(r["SpanAttributes"])
        provider = str(attrs.get("gen_ai.provider.name") or attrs.get("gen_ai.system", ""))
        req_model = str(attrs.get("gen_ai.request.model", ""))
        input_messages_raw = str(attrs.get("gen_ai.input.messages", ""))
        output_messages_raw = str(attrs.get("gen_ai.output.messages", ""))
        prompt = _extract_messages_text(input_messages_raw) or str(attrs.get("sobs.gen_ai.prompt", ""))
        response = _extract_messages_text(output_messages_raw) or str(attrs.get("sobs.gen_ai.response", ""))
        tokens_in = int(float(attrs.get("gen_ai.usage.input_tokens", "0") or 0))
        tokens_out = int(float(attrs.get("gen_ai.usage.output_tokens", "0") or 0))

        # Build messages array for training format
        messages: list = []
        try:
            if input_messages_raw:
                parsed = json.loads(input_messages_raw)
                if isinstance(parsed, list):
                    messages.extend(parsed)
        except (json.JSONDecodeError, TypeError):
            if prompt:
                messages.append({"role": "user", "content": prompt})
        try:
            if output_messages_raw:
                parsed = json.loads(output_messages_raw)
                if isinstance(parsed, list):
                    messages.extend(parsed)
        except (json.JSONDecodeError, TypeError):
            if response:
                messages.append({"role": "assistant", "content": response})

        record = {
            "messages": messages,
            "metadata": {
                "timestamp": str(r["Timestamp"]),
                "service": r["ServiceName"],
                "provider": provider,
                "model": req_model,
                "tokens_in": tokens_in,
                "tokens_out": tokens_out,
                "duration_ms": round(float(r["Duration"]) / 1_000_000, 1),
                "trace_id": r["TraceId"],
            },
        }
        records.append(record)

    if fmt == "json":
        body = json.dumps(records, ensure_ascii=False, indent=2)
        mime = "application/json"
        filename = "ai_training_data.json"
    else:
        lines = [json.dumps(rec, ensure_ascii=False) for rec in records]
        body = "\n".join(lines)
        mime = "application/x-ndjson"
        filename = "ai_training_data.jsonl"

    return Response(
        body,
        mimetype=mime,
        headers={"Content-Disposition": f'attachment; filename="{filename}"'},
    )


@ai_bp.route("/api/ai/field-hints", methods=["GET"])
@require_basic_auth
async def api_ai_field_hints():
    db = get_db()
    base_where = _AI_SPAN_CONDITION

    fields = [
        {"name": "service", "column": "ServiceName", "type": "string", "values": []},
        {"name": "model", "column": "SpanAttributes['gen_ai.request.model']", "type": "string", "values": []},
        {"name": "provider", "column": "SpanAttributes['gen_ai.provider.name']", "type": "string", "values": []},
        {"name": "operation", "column": "SpanAttributes['gen_ai.operation.name']", "type": "string", "values": []},
        {
            "name": "prompt",
            "column": _AI_TRACE_PROMPT_SQL,
            "type": "string",
            "values": [],
        },
        {
            "name": "response",
            "column": _AI_TRACE_RESPONSE_SQL,
            "type": "string",
            "values": [],
        },
        {"name": "span_name", "column": "SpanName", "type": "string", "values": []},
        {
            "name": "row_type",
            "column": "if(SpanAttributes['gen_ai.request.model'] != '', 'llm', 'system')",
            "type": "string",
            "values": [
                "llm",
                "system",
            ],
        },
        {"name": "trace_id", "column": "TraceId", "type": "string", "values": []},
        {"name": "span_id", "column": "SpanId", "type": "string", "values": []},
        {"name": "ts", "column": "Timestamp", "type": "datetime", "values": []},
        {"name": "status", "column": "StatusCode", "type": "string", "values": []},
        {"name": "error_type", "column": "SpanAttributes['error.type']", "type": "string", "values": []},
        {
            "name": "tokens_in",
            "column": "toUInt64OrZero(SpanAttributes['gen_ai.usage.input_tokens'])",
            "type": "number",
            "values": [],
        },
        {
            "name": "tokens_out",
            "column": "toUInt64OrZero(SpanAttributes['gen_ai.usage.output_tokens'])",
            "type": "number",
            "values": [],
        },
        {
            "name": "thinking_tokens",
            "column": "toUInt64OrZero(SpanAttributes['gen_ai.usage.thinking_tokens'])",
            "type": "number",
            "values": [],
        },
        {"name": "duration_ms", "column": "(Duration / 1000000.0)", "type": "number", "values": []},
    ]

    try:
        services = [
            str(r[0])
            for r in db.execute(
                f"SELECT DISTINCT ServiceName FROM otel_traces WHERE {base_where} "
                "AND ServiceName != '' ORDER BY ServiceName LIMIT 40"
            ).fetchall()
        ]
        models = [
            str(r[0])
            for r in db.execute(
                f"SELECT DISTINCT SpanAttributes['gen_ai.request.model'] FROM otel_traces WHERE {base_where} "
                "AND SpanAttributes['gen_ai.request.model'] != '' "
                "ORDER BY SpanAttributes['gen_ai.request.model'] LIMIT 40"
            ).fetchall()
        ]
        providers = [
            str(r[0])
            for r in db.execute(
                f"SELECT DISTINCT coalesce(SpanAttributes['gen_ai.provider.name'], SpanAttributes['gen_ai.system']) "
                f"FROM otel_traces WHERE {base_where} "
                "ORDER BY coalesce(SpanAttributes['gen_ai.provider.name'], SpanAttributes['gen_ai.system']) LIMIT 40"
            ).fetchall()
        ]
        operations = [
            str(r[0])
            for r in db.execute(
                f"SELECT DISTINCT SpanAttributes['gen_ai.operation.name'] FROM otel_traces WHERE {base_where} "
                "AND SpanAttributes['gen_ai.operation.name'] != '' "
                "ORDER BY SpanAttributes['gen_ai.operation.name'] LIMIT 40"
            ).fetchall()
        ]
        span_names = [
            str(r[0])
            for r in db.execute(
                f"SELECT DISTINCT SpanName FROM otel_traces WHERE {base_where} "
                "AND SpanName != '' ORDER BY SpanName LIMIT 60"
            ).fetchall()
        ]
        status_codes = [
            str(r[0])
            for r in db.execute(
                f"SELECT DISTINCT StatusCode FROM otel_traces WHERE {base_where} "
                "AND StatusCode != '' ORDER BY StatusCode LIMIT 20"
            ).fetchall()
        ]
        error_types = [
            str(r[0])
            for r in db.execute(
                f"SELECT DISTINCT SpanAttributes['error.type'] FROM otel_traces WHERE {base_where} "
                "AND SpanAttributes['error.type'] != '' ORDER BY SpanAttributes['error.type'] LIMIT 40"
            ).fetchall()
        ]
    except Exception:
        services = []
        models = []
        providers = []
        operations = []
        span_names = []
        status_codes = []
        error_types = []

    values_by_field = {
        "service": services,
        "model": models,
        "provider": providers,
        "operation": operations,
        "span_name": span_names,
        "status": status_codes,
        "error_type": error_types,
    }
    for fld in fields:
        if fld["name"] in values_by_field:
            fld["values"] = values_by_field[fld["name"]]

    operators = ["=", "!=", "LIKE", "NOT LIKE", "ILIKE", "NOT ILIKE", "IN", "NOT IN", ">", "<", ">=", "<="]
    keywords = ["AND", "OR", "NOT", "IS NULL", "IS NOT NULL", "TRUE", "FALSE", "NULL"]
    functions = [
        {"name": "match", "signature": "match(model, 'gpt')", "kind": "string"},
        {"name": "startsWith", "signature": "startsWith(span_name, 'ai.tool')", "kind": "string"},
        {"name": "endsWith", "signature": "endsWith(provider, 'cloud')", "kind": "string"},
        {"name": "lower", "signature": "lower(model)", "kind": "string"},
        {"name": "upper", "signature": "upper(operation)", "kind": "string"},
        {"name": "toDateTime", "signature": "toDateTime('2026-03-30 12:00:00')", "kind": "datetime"},
    ]
    snippets = [
        {"label": "row_type='llm'", "insert": "row_type='llm'", "kind": "predicate"},
        {"label": "row_type='system'", "insert": "row_type='system'", "kind": "predicate"},
        {"label": "span_name='ai.tool.executed'", "insert": "span_name='ai.tool.executed'", "kind": "predicate"},
        {
            "label": "prompt ILIKE '%graph%'",
            "insert": "prompt ILIKE '%graph%'",
            "kind": "predicate",
        },
        {
            "label": "response ILIKE '%chart%'",
            "insert": "response ILIKE '%chart%'",
            "kind": "predicate",
        },
        {"label": "tokens_out > 1000", "insert": "tokens_out > 1000", "kind": "predicate"},
        {"label": "error_type != ''", "insert": "error_type != ''", "kind": "predicate"},
        {
            "label": "ts >= toDateTime('2026-03-30 00:00:00')",
            "insert": "ts >= toDateTime('2026-03-30 00:00:00')",
            "kind": "predicate",
        },
    ]

    return jsonify(
        {
            "fields": fields,
            "operators": operators,
            "keywords": keywords,
            "functions": functions,
            "snippets": snippets,
        }
    )


@ai_bp.route("/api/ai/validate-filter", methods=["POST"])
@require_basic_auth
async def api_ai_validate_filter():
    """Validate a SQL WHERE fragment used by /ai?sql=... and return actionable feedback."""
    payload = await request.get_json(silent=True)
    sql_where = str((payload or {}).get("sql", "") or "").strip()
    if not sql_where:
        return jsonify({"ok": True, "normalized": "", "issues": []})

    issues: list[dict[str, str]] = []

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
        safe_sql = _normalize_ai_sql_where(sql_where)

        db = get_db()
        db.execute(
            "SELECT 1 FROM otel_traces " f"WHERE ({safe_sql}) " f"AND {_AI_SPAN_CONDITION} " "LIMIT 1"
        ).fetchone()
    except Exception as exc:
        issues.append({"level": "error", "message": _public_dashboard_query_error(exc)})
        return jsonify({"ok": False, "normalized": "", "issues": issues}), 200

    return jsonify({"ok": True, "normalized": safe_sql, "issues": issues})


@ai_bp.route("/api/ai/helper/capabilities", methods=["GET"])
@require_basic_auth
async def ai_helper_capabilities():
    db = get_db()
    settings = _load_all_ai_settings(db)
    model = settings.get("ai.model", "").strip()
    thinking_level = _normalize_thinking_level(settings.get("ai.thinking_level", "off"))
    page = str(request.args.get("page") or "").strip() or "/logs"
    action_manifest = _helper_action_manifest_for_page(page)
    return jsonify(
        {
            "ok": True,
            "model": model,
            "supports_tools": _model_supports_tools(model),
            "supports_thinking": _model_supports_thinking(model),
            "default_thinking_level": thinking_level,
            "thinking_levels": list(_AI_THINKING_LEVELS),
            "page": page,
            "action_manifest": action_manifest,
        }
    )


@ai_bp.route("/api/ai/helper/actions/manifest", methods=["GET"])
@require_basic_auth
async def ai_helper_action_manifest():
    page = str(request.args.get("page") or "").strip() or "/logs"
    return jsonify(
        {
            "ok": True,
            "page": page,
            "actions": _helper_action_manifest_for_page(page),
        }
    )


@ai_bp.route("/api/ai/helper/chats", methods=["GET"])
@require_basic_auth
async def ai_helper_chats():
    db = get_db()
    page = str(request.args.get("page") or "").strip()
    q = str(request.args.get("q") or "").strip().lower()
    try:
        limit = max(5, min(int(request.args.get("limit") or 20), 100))
    except (ValueError, TypeError):
        limit = 20
    try:
        offset = max(0, int(request.args.get("offset") or 0))
    except (ValueError, TypeError):
        offset = 0

    where = ["ServiceName=?", "EventName='turn.summary'", "LogAttributes['gen_ai.chat_id'] != ''"]
    params: list[Any] = [_AI_HELPER_SERVICE_NAME]
    if page:
        where.append("LogAttributes['sobs.ai.page'] = ?")
        params.append(page)
    where_sql = " AND ".join(where)
    rows = db.execute(
        "SELECT "
        "  LogAttributes['gen_ai.chat_id'] AS chat_id, "
        "  min(Timestamp) AS first_ts, "
        "  max(Timestamp) AS last_ts, "
        "  argMin(LogAttributes['gen_ai.input.question'], Timestamp) AS first_question, "
        "  argMin(LogAttributes['gen_ai.turn.summary.request'], Timestamp) AS first_request, "
        "  count() AS turn_count "
        f"FROM otel_logs WHERE {where_sql} "
        "GROUP BY chat_id "
        "ORDER BY last_ts DESC LIMIT 500",
        params,
    ).fetchall()

    chats: list[dict[str, Any]] = []
    for row in rows:
        chat_id = str(row["chat_id"] or "").strip()
        if not chat_id:
            continue
        label = _chat_label_from_first_turn(row["first_question"], row["first_request"])
        if q and q not in label.lower():
            continue
        chats.append(
            {
                "chat_id": chat_id,
                "first_ts": str(row["first_ts"] or ""),
                "last_ts": str(row["last_ts"] or ""),
                "label": label,
                "turn_count": int(row["turn_count"] or 0),
            }
        )

    total = len(chats)
    page_chats = chats[offset : offset + limit]
    has_more = offset + len(page_chats) < total
    return jsonify({"ok": True, "chats": page_chats, "total": total, "has_more": has_more, "offset": offset})


@ai_bp.route("/api/ai/helper/chats/<chat_id>", methods=["GET"])
@require_basic_auth
async def ai_helper_chat_detail(chat_id: str):
    safe_chat_id = str(chat_id or "").strip()
    if not safe_chat_id:
        return jsonify({"ok": False, "error": "chat_id is required"}), 400

    db = get_db()
    rows = db.execute(
        "SELECT "
        "  Timestamp, "
        "  LogAttributes['gen_ai.turn_id'] AS turn_id, "
        "  LogAttributes['gen_ai.input.question'] AS input_question, "
        "  LogAttributes['gen_ai.turn.summary.request'] AS request, "
        "  LogAttributes['gen_ai.output.messages'] AS output_messages "
        "FROM otel_logs "
        "WHERE ServiceName=? AND EventName='turn.complete' AND LogAttributes['gen_ai.chat_id']=? "
        "ORDER BY Timestamp ASC LIMIT 300",
        [_AI_HELPER_SERVICE_NAME, safe_chat_id],
    ).fetchall()

    tools_by_turn = _load_chat_tool_history(db, safe_chat_id)
    messages: list[dict[str, Any]] = []
    for row in rows:
        ts = str(row["Timestamp"] or "")
        turn_id = str(row["turn_id"] or "")
        request_text = str(row["input_question"] or "").strip()
        if request_text:
            messages.append(
                {
                    "kind": "message",
                    "role": "user",
                    "text": request_text,
                    "ts": ts,
                    "turn_id": turn_id,
                }
            )

        assistant_text = ""
        raw_output = str(row["output_messages"] or "")
        if raw_output:
            try:
                parsed = json.loads(raw_output)
                if isinstance(parsed, list):
                    parts: list[str] = []
                    for item in parsed:
                        if isinstance(item, dict):
                            content = str(item.get("content") or "").strip()
                            if content:
                                parts.append(content)
                    assistant_text = "\n\n".join(parts).strip()
            except (json.JSONDecodeError, TypeError):
                assistant_text = ""
        if assistant_text:
            assistant_text, _assistant_meta = _extract_assistant_meta(assistant_text)
        if assistant_text:
            messages.append(
                {
                    "kind": "message",
                    "role": "assistant",
                    "text": assistant_text,
                    "ts": ts,
                    "turn_id": turn_id,
                    "question": request_text,
                }
            )
        for tool_item in tools_by_turn.get(turn_id, []):
            messages.append(dict(tool_item))

    return jsonify({"ok": True, "chat_id": safe_chat_id, "messages": messages})


@ai_bp.route("/api/ai/helper/feedback", methods=["POST"])
@require_basic_auth
async def ai_helper_feedback():
    payload = await request.get_json(force=True, silent=True) or {}
    chat_id = str(payload.get("chat_id") or "").strip()
    turn_id = str(payload.get("turn_id") or "").strip()
    note = str(payload.get("note") or "").strip()
    page = str(payload.get("page") or "").strip() or "/logs"
    if not chat_id or not turn_id or not note:
        return jsonify({"ok": False, "error": "chat_id, turn_id, and note are required"}), 400

    _emit_ai_helper_log_event(
        event_name="turn.feedback",
        chat_id=chat_id,
        turn_id=turn_id,
        page=page,
        model="",
        guard_model="",
        thinking_level="off",
        body=note,
        attrs={
            "gen_ai.feedback.note": note,
            "gen_ai.feedback.kind": "user_note",
        },
    )
    return jsonify({"ok": True})


@ai_bp.route("/api/ai/helper", methods=["POST"])
@require_basic_auth
async def ai_helper():
    """Contextual AI helper. Accepts JSON {question, page, context} and returns LLM answer."""
    payload = await request.get_json(force=True, silent=True) or {}
    question = str(payload.get("question") or "").strip()
    page = str(payload.get("page") or "").strip()
    context_data = payload.get("context") or {}
    stream_requested = bool(payload.get("stream")) or "text/event-stream" in request.headers.get("Accept", "")
    chat_id = str(payload.get("chat_id") or "").strip() or str(uuid.uuid4())
    turn_id = str(payload.get("turn_id") or "").strip() or str(uuid.uuid4())

    if not question:
        return jsonify({"ok": False, "error": "question is required"}), 400

    db = get_db()
    settings = _load_all_ai_settings(db)

    endpoint_url = settings.get("ai.endpoint_url", "").strip()
    model = settings.get("ai.model", "").strip()
    api_key = settings.get("ai.api_key", "").strip()
    system_prompt_override = settings.get("ai.system_prompt", "").strip()
    guard_model = settings.get("ai.guard_model", "").strip()

    default_thinking = _normalize_thinking_level(settings.get("ai.thinking_level", "off"))
    requested_thinking = _normalize_thinking_level(str(payload.get("thinking_level") or "").strip())
    thinking_level = requested_thinking if requested_thinking != "off" else default_thinking
    if not _model_supports_thinking(model):
        thinking_level = "off"

    _emit_ai_helper_log_event(
        event_name="turn.start",
        chat_id=chat_id,
        turn_id=turn_id,
        page=page,
        model=model,
        guard_model=guard_model,
        thinking_level=thinking_level,
        body="AI helper turn started",
        attrs={
            "gen_ai.request.stream": stream_requested,
            "gen_ai.input.messages": json.dumps([{"role": "user", "content": question}], ensure_ascii=False),
        },
    )

    if not endpoint_url or not model:
        return (
            jsonify(
                {
                    "ok": False,
                    "error": "AI endpoint not configured. Visit Settings → AI Configuration.",
                }
            ),
            503,
        )

    allowed, guard_reason, guard_stats = await _maybe_await(_check_guard_model(settings, question, page))
    _emit_ai_helper_log_event(
        event_name="guard.result",
        chat_id=chat_id,
        turn_id=turn_id,
        page=page,
        model=model,
        guard_model=guard_model,
        thinking_level=thinking_level,
        body=f"Guard verdict: {guard_reason}",
        attrs=_guard_telemetry_attrs(allowed, guard_reason, guard_stats),
    )
    if not allowed:
        error_message = f"Request blocked by safety guard: {guard_reason}"
        _emit_ai_helper_log_event(
            event_name="turn.blocked",
            chat_id=chat_id,
            turn_id=turn_id,
            page=page,
            model=model,
            guard_model=guard_model,
            thinking_level=thinking_level,
            body=error_message,
            severity="WARN",
            attrs={"gen_ai.guard.reason": guard_reason},
        )
        if stream_requested:

            async def _guard_blocked():
                yield _sse_json_event("error", {"error": error_message})

            return Response(
                _guard_blocked(),
                mimetype="text/event-stream",
                headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
            )
        return jsonify({"ok": False, "error": error_message}), 400

    action_manifest = _helper_action_manifest_for_page(page)
    action_manifest_json = json.dumps(action_manifest, ensure_ascii=False)
    dashboard_action_manifest = _helper_action_manifest_for_page("/dashboards")
    dashboard_action_manifest_json = json.dumps(dashboard_action_manifest, ensure_ascii=False)
    chat_memories = _load_chat_memories(db, chat_id)
    relevant_memories = _semantic_memory_matches(chat_memories, question, max_results=5)
    recent_chat_turns = _load_recent_chat_turns(db, chat_id, limit=8)
    recent_history = _load_recent_turn_summaries(db, chat_id, question, limit=4)

    memory_lines: list[str] = []
    for item in relevant_memories:
        text = str(item.get("text") or "").strip()
        if text:
            memory_lines.append(f"- {text}")
    memory_block = "\n".join(memory_lines)

    history_lines: list[str] = []
    for item in recent_history:
        request_s = str(item.get("request") or "")
        action_s = str(item.get("action") or "")
        result_s = str(item.get("result") or "")
        history_lines.append(f"- request={request_s}; action={action_s}; result={result_s}")
    history_block = "\n".join(history_lines)

    continuity_lines: list[str] = []
    for item in recent_chat_turns:
        request_s = str(item.get("request") or "")
        action_s = str(item.get("action") or "")
        result_s = str(item.get("result") or "")
        continuity_lines.append(f"- request={request_s}; action={action_s}; result={result_s}")
    continuity_block = "\n".join(continuity_lines)

    system_prompt = system_prompt_override or (
        "You are an expert observability assistant for SOBS (Simple Observe Stack). "
        "You help operators understand and troubleshoot their application telemetry including "
        "logs, traces, errors, metrics, RUM events, and AI transparency data. "
        "Be concise and actionable. When suggesting SQL queries, use ClickHouse syntax. "
        "If the request is ambiguous and multiple interpretations are plausible, ask one short "
        "clarifying question before taking action. If intent is clear, act directly. "
        "Try higher-quality solutions before simplistic ones, especially for grouping/ranking asks. "
        "Only propose UI actions that exist in the action manifest for this page. "
        "Do not claim any UI action was executed unless a tool is called and execution is "
        "confirmed by the app. "
        "When a UI action will be applied by the browser after your response, describe it as "
        "proposed, queued, or ready to apply; do not say it already succeeded. "
        "If the page action manifest does not expose the control needed for the request, explain "
        "that limitation and do not call a UI action unless you can pivot using cross-page actions. "
        "For chart or dashboard creation requests, prefer a cross-page pivot to /dashboards using "
        "available dashboard actions. "
        "If tools are available and the user asks to apply a logs SQL filter, call "
        "propose_ui_action with action_id logs.filter.apply_sql. "
        "If tools are available and the user asks to apply an AI page SQL filter, call "
        "propose_ui_action with action_id ai.filter.apply_sql. "
        "The otel_logs table has an EventName column for structured event types. "
        "To filter by event name use: EventName = 'turn.feedback' "
        "To access log attributes use: LogAttributes['gen_ai.feedback.note'] "
        "Examples: EventName = 'turn.feedback' finds AI assistant feedback records; "
        "EventName = 'turn.complete' finds completed AI turns; "
        "EventName = 'turn.feedback' AND TraceId = '<chat_id>' scopes to one conversation. "
        "All AI assistant telemetry lives in otel_logs under ServiceName = 'sobs-ai-helper'. "
        "On the AI page the table is otel_traces. Supported aliases include: service, model, provider, "
        "operation, prompt, response, span_name, row_type, trace_id, span_id, ts, status, "
        "error_type, tokens_in, tokens_out, "
        "thinking_tokens, duration_ms. "
        "Do not use LogAttributes[...] on the AI page; use aliases or SpanAttributes[...] only. "
        "AI page examples: row_type = 'system' AND span_name = 'ai.tool.executed'; "
        "model = 'gpt-oss:120b-cloud' AND tokens_out > 1000; "
        "prompt ILIKE '%graph%' OR response ILIKE '%chart%'; "
        "provider = 'sobs' AND error_type != ''; "
        "duration_ms > 1000 ORDER BY Timestamp DESC is not valid in WHERE, so only emit the filter expression. "
        "For requests like 'longest traces' or 'highest total duration by trace', generate a "
        "richer WHERE clause using an IN subquery with GROUP BY trace id and ORDER BY sum(Duration) DESC. "
        "At the very end of every response, append a single compact metadata block in this exact format: "
        '<assistant_meta>{"turn_summary":{"request":"...","action":"...","result":"..."},'
        '"memory_candidates":["optional memory 1","optional memory 2"]}</assistant_meta>. '
        "Keep memory_candidates empty when no durable memory is needed. "
        "Do not include any additional text after </assistant_meta>. "
        "Page action manifest: "
        + action_manifest_json
        + "\nCross-page dashboard actions (/dashboards): "
        + dashboard_action_manifest_json
    )

    if memory_block:
        system_prompt += "\n\nRelevant persistent memories:\n" + memory_block
    if continuity_block:
        system_prompt += "\n\nCurrent chat continuity (recent turns):\n" + continuity_block
    if history_block:
        system_prompt += "\n\nSemantically relevant prior turn summaries:\n" + history_block

    context_lines: list[str] = [f"Current page: {page}" if page else ""]
    if isinstance(context_data, dict):
        for k, v in context_data.items():
            if v:
                context_lines.append(f"{k}: {v}")

    context_str = "\n".join(ln for ln in context_lines if ln)
    user_content = f"{context_str}\n\nQuestion: {question}" if context_str else question

    messages = [
        {"role": "system", "content": system_prompt},
        {"role": "user", "content": user_content},
    ]
    tools = _helper_tools_for_page(page) if _model_supports_tools(model) else []
    turn_logs_url = _build_ai_turn_logs_url(chat_id, turn_id)

    if stream_requested:

        async def _generate() -> AsyncIterator[str]:
            answer_parts: list[str] = []
            thinking_tokens = 0
            last_tool_summary = ""
            loop_messages: list[dict[str, Any]] = list(messages)
            max_tool_rounds = 3
            yield _sse_json_event(
                "meta",
                {
                    "chat_id": chat_id,
                    "turn_id": turn_id,
                    "supports_thinking": _model_supports_thinking(model),
                    "thinking_level": thinking_level,
                    "turn_logs_url": turn_logs_url,
                },
            )
            yield _sse_json_event("guard", {"guard_stats": guard_stats})
            try:
                model_stats: dict[str, Any] = {}
                for loop_round in range(max_tool_rounds + 1):
                    round_text_parts: list[str] = []
                    round_tool_feedback: list[dict[str, Any]] = []
                    async for event in _stream_llm_endpoint(
                        endpoint_url,
                        model,
                        api_key,
                        loop_messages,
                        tools=tools,
                        thinking_level=thinking_level,
                        max_tokens=768,
                    ):
                        event_type = str(event.get("type") or "")
                        if event_type == "delta":
                            chunk = str(event.get("text") or "")
                            if chunk:
                                round_text_parts.append(chunk)
                                answer_parts.append(chunk)
                                yield _sse_json_event("token", {"text": chunk})
                        elif event_type == "tool":
                            tool_call = event.get("tool_call") or {}
                            tool_name = str(tool_call.get("name") or "")
                            tool_args = tool_call.get("arguments") or {}
                            if isinstance(tool_args, dict):
                                normalized_tool: dict[str, Any] | None = None
                                if tool_name == "propose_ui_action":
                                    normalized_tool = _normalize_generic_ui_action_tool_call(tool_args, page)
                                if normalized_tool:
                                    action_id = str(normalized_tool.get("action_id") or "")
                                    unsupported = bool(normalized_tool.get("unsupported"))
                                    action_payload = cast(dict[str, Any], normalized_tool.get("action") or {})
                                    last_tool_summary = str(normalized_tool.get("summary") or "").strip()
                                    if action_id and not unsupported and action_payload:
                                        normalized_tool["action_token"] = _issue_ai_action_token(
                                            action_id=action_id,
                                            target_page=str(action_payload.get("target_page") or page or "/logs"),
                                            action=action_payload,
                                            requires_confirmation=bool(
                                                normalized_tool.get("requires_confirmation", True)
                                            ),
                                            chat_id=chat_id,
                                            turn_id=turn_id,
                                        )
                                    _emit_ai_helper_log_event(
                                        event_name="tool.proposed",
                                        chat_id=chat_id,
                                        turn_id=turn_id,
                                        page=page,
                                        model=model,
                                        guard_model=guard_model,
                                        thinking_level=thinking_level,
                                        body=f"Tool proposed: {tool_name}",
                                        attrs={
                                            "gen_ai.tool.name": tool_name,
                                            "sobs.ai.action_id": action_id,
                                            "sobs.ai.tool.summary": normalized_tool.get("summary", ""),
                                            "sobs.ai.tool.action": json.dumps(
                                                normalized_tool.get("action") or {}, ensure_ascii=False
                                            ),
                                            "sobs.ai.action.requires_confirmation": bool(
                                                normalized_tool.get("requires_confirmation", True)
                                            ),
                                            "sobs.ai.action.status": ("unsupported" if unsupported else "proposed"),
                                        },
                                    )
                                    round_tool_feedback.append(
                                        {
                                            "tool": tool_name,
                                            "ok": not unsupported,
                                            "action_id": action_id,
                                            "summary": str(normalized_tool.get("summary") or ""),
                                            "action": cast(dict[str, Any], normalized_tool.get("action") or {}),
                                            "requires_confirmation": bool(
                                                normalized_tool.get("requires_confirmation", True)
                                            ),
                                        }
                                    )
                                    yield _sse_json_event("tool", normalized_tool)
                        elif event_type == "done":
                            model_stats = cast(dict[str, Any], event.get("stats") or {})

                    if not round_tool_feedback:
                        fallback_tool = _suggest_chart_dashboard_pivot_tool(question, page)
                        if fallback_tool:
                            action_id = str(fallback_tool.get("action_id") or "")
                            unsupported = bool(fallback_tool.get("unsupported"))
                            action_payload = cast(dict[str, Any], fallback_tool.get("action") or {})
                            last_tool_summary = str(fallback_tool.get("summary") or "").strip()
                            if action_id and not unsupported and action_payload:
                                fallback_tool["action_token"] = _issue_ai_action_token(
                                    action_id=action_id,
                                    target_page=str(action_payload.get("target_page") or page or "/logs"),
                                    action=action_payload,
                                    requires_confirmation=bool(fallback_tool.get("requires_confirmation", True)),
                                    chat_id=chat_id,
                                    turn_id=turn_id,
                                )
                            _emit_ai_helper_log_event(
                                event_name="tool.proposed",
                                chat_id=chat_id,
                                turn_id=turn_id,
                                page=page,
                                model=model,
                                guard_model=guard_model,
                                thinking_level=thinking_level,
                                body="Tool proposed: fallback.dashboard_chart_pivot",
                                attrs={
                                    "gen_ai.tool.name": "fallback.dashboard_chart_pivot",
                                    "sobs.ai.action_id": action_id,
                                    "sobs.ai.tool.summary": fallback_tool.get("summary", ""),
                                    "sobs.ai.tool.action": json.dumps(
                                        fallback_tool.get("action") or {}, ensure_ascii=False
                                    ),
                                    "sobs.ai.action.requires_confirmation": bool(
                                        fallback_tool.get("requires_confirmation", True)
                                    ),
                                    "sobs.ai.action.status": ("unsupported" if unsupported else "proposed"),
                                },
                            )
                            round_tool_feedback.append(
                                {
                                    "tool": "propose_ui_action",
                                    "ok": not unsupported,
                                    "action_id": action_id,
                                    "summary": str(fallback_tool.get("summary") or ""),
                                    "action": cast(dict[str, Any], fallback_tool.get("action") or {}),
                                    "requires_confirmation": bool(fallback_tool.get("requires_confirmation", True)),
                                }
                            )
                            yield _sse_json_event("tool", fallback_tool)

                    has_pending_confirmation = any(
                        bool(item.get("requires_confirmation", True)) for item in round_tool_feedback
                    )
                    # If awaiting user confirmation, stop loop to avoid re-proposing identical actions.
                    if has_pending_confirmation:
                        break

                    # Continue loop only if tool calls were made this round and rounds remain.
                    if not round_tool_feedback or loop_round >= max_tool_rounds:
                        break

                    assistant_round_text = "".join(round_text_parts).strip()
                    if assistant_round_text:
                        loop_messages.append({"role": "assistant", "content": assistant_round_text})
                    else:
                        loop_messages.append(
                            {
                                "role": "assistant",
                                "content": "Requested tool calls for the current turn.",
                            }
                        )

                    tool_feedback_text = json.dumps(round_tool_feedback, ensure_ascii=False)
                    loop_messages.append(
                        {
                            "role": "system",
                            "content": (
                                "Tool execution results for this turn (JSON). Use these results to continue reasoning "
                                "and produce the final answer when ready: " + tool_feedback_text
                            ),
                        }
                    )

                thinking_tokens = int(model_stats.get("thinking_tokens") or 0)
                final_answer, assistant_meta = _extract_assistant_meta("".join(answer_parts))
                meta_summary = cast(dict[str, Any], assistant_meta.get("turn_summary") or {})
                summary = _derive_turn_summary(
                    question=question,
                    answer=final_answer,
                    tool_summary=last_tool_summary,
                    meta_summary=meta_summary,
                )

                memory_candidates = _extract_memory_candidates(assistant_meta)
                saved_memory_ids: list[str] = []
                for candidate in memory_candidates:
                    memories_now = _load_chat_memories(db, chat_id)
                    related = _semantic_memory_matches(
                        memories_now,
                        candidate,
                        max_results=4,
                        min_score=_AI_MEMORY_CONSOLIDATION_SCORE,
                    )
                    consolidation = await _consolidate_memory_candidates(
                        settings,
                        new_memory=candidate,
                        related=related,
                    )
                    action = str(consolidation.get("action") or "keep_new")
                    if action == "ignore":
                        continue
                    merged_text = _coerce_summary_value(consolidation.get("memory") or candidate, 280)
                    drop_ids = cast(list[str], consolidation.get("drop_ids") or [])
                    for memory_id in drop_ids:
                        _upsert_ai_memory(
                            db,
                            memory_id=memory_id,
                            chat_id=chat_id,
                            memory_text="",
                            source_turn_id=turn_id,
                            is_deleted=True,
                        )
                    new_id = str(uuid.uuid4())
                    _upsert_ai_memory(
                        db,
                        memory_id=new_id,
                        chat_id=chat_id,
                        memory_text=merged_text,
                        source_turn_id=turn_id,
                        is_deleted=False,
                    )
                    saved_memory_ids.append(new_id)

                _emit_ai_helper_log_event(
                    event_name="turn.complete",
                    chat_id=chat_id,
                    turn_id=turn_id,
                    page=page,
                    model=model,
                    guard_model=guard_model,
                    thinking_level=thinking_level,
                    body="AI helper turn completed",
                    attrs={
                        "gen_ai.response.id": turn_id,
                        "gen_ai.input.question": question,
                        "gen_ai.usage.input_tokens": model_stats.get("prompt_tokens", 0),
                        "gen_ai.usage.output_tokens": model_stats.get("completion_tokens", 0),
                        "gen_ai.usage.thinking_tokens": thinking_tokens,
                        "gen_ai.response.latency_ms": model_stats.get("elapsed_ms", 0),
                        "gen_ai.output.messages": json.dumps(
                            [{"role": "assistant", "content": final_answer}],
                            ensure_ascii=False,
                        ),
                        "gen_ai.turn.summary.request": summary.get("request", ""),
                        "gen_ai.turn.summary.action": summary.get("action", ""),
                        "gen_ai.turn.summary.result": summary.get("result", ""),
                        "gen_ai.memory.saved_ids": json.dumps(saved_memory_ids, ensure_ascii=False),
                    },
                )
                _emit_ai_helper_log_event(
                    event_name="turn.summary",
                    chat_id=chat_id,
                    turn_id=turn_id,
                    page=page,
                    model=model,
                    guard_model=guard_model,
                    thinking_level=thinking_level,
                    body="AI helper turn summary",
                    attrs={
                        "gen_ai.turn.summary.request": summary.get("request", ""),
                        "gen_ai.turn.summary.action": summary.get("action", ""),
                        "gen_ai.turn.summary.result": summary.get("result", ""),
                    },
                )
                yield _sse_json_event(
                    "done",
                    {
                        "ok": True,
                        "answer": final_answer,
                        "model": model,
                        "chat_id": chat_id,
                        "turn_id": turn_id,
                        "thinking_level": thinking_level,
                        "turn_logs_url": turn_logs_url,
                        "guard_stats": guard_stats,
                        "model_stats": model_stats,
                        "turn_summary": summary,
                        "saved_memory_ids": saved_memory_ids,
                    },
                )
            except asyncio.CancelledError:
                _emit_ai_helper_log_event(
                    event_name="turn.cancelled",
                    chat_id=chat_id,
                    turn_id=turn_id,
                    page=page,
                    model=model,
                    guard_model=guard_model,
                    thinking_level=thinking_level,
                    body="Client cancelled AI helper stream",
                    severity="WARN",
                )
                log.debug("AI helper stream cancelled by client")
            except Exception as exc:
                log.warning("LLM endpoint stream failed: %s", exc)
                _emit_ai_helper_log_event(
                    event_name="turn.error",
                    chat_id=chat_id,
                    turn_id=turn_id,
                    page=page,
                    model=model,
                    guard_model=guard_model,
                    thinking_level=thinking_level,
                    body=f"LLM stream error: {exc}",
                    severity="ERROR",
                )
                yield _sse_json_event("error", {"error": "LLM endpoint returned no response"})

        return Response(
            _generate(),
            mimetype="text/event-stream",
            headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
        )

    loop_messages: list[dict[str, Any]] = list(messages)
    answer_parts: list[str] = []
    model_stats: dict[str, Any] = {}
    proposed_tools: list[dict[str, Any]] = []
    max_tool_rounds = 3

    for loop_round in range(max_tool_rounds + 1):
        round_text_parts: list[str] = []
        round_tool_feedback: list[dict[str, Any]] = []
        async for event in _stream_llm_endpoint(
            endpoint_url,
            model,
            api_key,
            loop_messages,
            tools=tools,
            thinking_level=thinking_level,
            max_tokens=768,
        ):
            event_type = str(event.get("type") or "")
            if event_type == "delta":
                chunk = str(event.get("text") or "")
                if chunk:
                    round_text_parts.append(chunk)
                    answer_parts.append(chunk)
            elif event_type == "tool":
                tool_call = event.get("tool_call") or {}
                tool_name = str(tool_call.get("name") or "")
                tool_args = tool_call.get("arguments") or {}
                if isinstance(tool_args, dict):
                    normalized_tool: dict[str, Any] | None = None
                    if tool_name == "propose_ui_action":
                        normalized_tool = _normalize_generic_ui_action_tool_call(tool_args, page)
                    if normalized_tool:
                        action_id = str(normalized_tool.get("action_id") or "")
                        unsupported = bool(normalized_tool.get("unsupported"))
                        action_payload = cast(dict[str, Any], normalized_tool.get("action") or {})
                        if action_id and not unsupported and action_payload:
                            normalized_tool["action_token"] = _issue_ai_action_token(
                                action_id=action_id,
                                target_page=str(action_payload.get("target_page") or page or "/logs"),
                                action=action_payload,
                                requires_confirmation=bool(normalized_tool.get("requires_confirmation", True)),
                                chat_id=chat_id,
                                turn_id=turn_id,
                            )
                        _emit_ai_helper_log_event(
                            event_name="tool.proposed",
                            chat_id=chat_id,
                            turn_id=turn_id,
                            page=page,
                            model=model,
                            guard_model=guard_model,
                            thinking_level=thinking_level,
                            body=f"Tool proposed: {tool_name}",
                            attrs={
                                "gen_ai.tool.name": tool_name,
                                "sobs.ai.action_id": action_id,
                                "sobs.ai.tool.summary": normalized_tool.get("summary", ""),
                                "sobs.ai.tool.action": json.dumps(
                                    normalized_tool.get("action") or {}, ensure_ascii=False
                                ),
                                "sobs.ai.action.requires_confirmation": bool(
                                    normalized_tool.get("requires_confirmation", True)
                                ),
                                "sobs.ai.action.status": ("unsupported" if unsupported else "proposed"),
                            },
                        )
                        proposed_tools.append(normalized_tool)
                        round_tool_feedback.append(
                            {
                                "tool": tool_name,
                                "ok": not unsupported,
                                "action_id": action_id,
                                "summary": str(normalized_tool.get("summary") or ""),
                                "action": cast(dict[str, Any], normalized_tool.get("action") or {}),
                                "requires_confirmation": bool(normalized_tool.get("requires_confirmation", True)),
                            }
                        )
            elif event_type == "done":
                model_stats = cast(dict[str, Any], event.get("stats") or {})

        if not round_tool_feedback:
            fallback_tool = _suggest_chart_dashboard_pivot_tool(question, page)
            if fallback_tool:
                action_id = str(fallback_tool.get("action_id") or "")
                unsupported = bool(fallback_tool.get("unsupported"))
                action_payload = cast(dict[str, Any], fallback_tool.get("action") or {})
                if action_id and not unsupported and action_payload:
                    fallback_tool["action_token"] = _issue_ai_action_token(
                        action_id=action_id,
                        target_page=str(action_payload.get("target_page") or page or "/logs"),
                        action=action_payload,
                        requires_confirmation=bool(fallback_tool.get("requires_confirmation", True)),
                        chat_id=chat_id,
                        turn_id=turn_id,
                    )
                _emit_ai_helper_log_event(
                    event_name="tool.proposed",
                    chat_id=chat_id,
                    turn_id=turn_id,
                    page=page,
                    model=model,
                    guard_model=guard_model,
                    thinking_level=thinking_level,
                    body="Tool proposed: fallback.dashboard_chart_pivot",
                    attrs={
                        "gen_ai.tool.name": "fallback.dashboard_chart_pivot",
                        "sobs.ai.action_id": action_id,
                        "sobs.ai.tool.summary": fallback_tool.get("summary", ""),
                        "sobs.ai.tool.action": json.dumps(fallback_tool.get("action") or {}, ensure_ascii=False),
                        "sobs.ai.action.requires_confirmation": bool(fallback_tool.get("requires_confirmation", True)),
                        "sobs.ai.action.status": ("unsupported" if unsupported else "proposed"),
                    },
                )
                proposed_tools.append(fallback_tool)
                round_tool_feedback.append(
                    {
                        "tool": "propose_ui_action",
                        "ok": not unsupported,
                        "action_id": action_id,
                        "summary": str(fallback_tool.get("summary") or ""),
                        "action": cast(dict[str, Any], fallback_tool.get("action") or {}),
                        "requires_confirmation": bool(fallback_tool.get("requires_confirmation", True)),
                    }
                )

        has_pending_confirmation = any(bool(item.get("requires_confirmation", True)) for item in round_tool_feedback)
        if has_pending_confirmation:
            break

        if not round_tool_feedback or loop_round >= max_tool_rounds:
            break

        assistant_round_text = "".join(round_text_parts).strip()
        if assistant_round_text:
            loop_messages.append({"role": "assistant", "content": assistant_round_text})
        else:
            loop_messages.append({"role": "assistant", "content": "Requested tool calls for the current turn."})

        tool_feedback_text = json.dumps(round_tool_feedback, ensure_ascii=False)
        loop_messages.append(
            {
                "role": "system",
                "content": (
                    "Tool execution results for this turn (JSON). Use these results to continue reasoning "
                    "and produce the final answer when ready: " + tool_feedback_text
                ),
            }
        )

    answer = "".join(answer_parts).strip()
    if not answer:
        _emit_ai_helper_log_event(
            event_name="turn.error",
            chat_id=chat_id,
            turn_id=turn_id,
            page=page,
            model=model,
            guard_model=guard_model,
            thinking_level=thinking_level,
            body="LLM endpoint returned no response",
            severity="ERROR",
        )
        return jsonify({"ok": False, "error": "LLM endpoint returned no response"}), 502

    final_answer, assistant_meta = _extract_assistant_meta(answer)
    meta_summary = cast(dict[str, Any], assistant_meta.get("turn_summary") or {})
    summary = _derive_turn_summary(
        question=question,
        answer=final_answer,
        tool_summary="",
        meta_summary=meta_summary,
    )

    saved_memory_ids: list[str] = []
    memory_candidates = _extract_memory_candidates(assistant_meta)
    for candidate in memory_candidates:
        memories_now = _load_chat_memories(db, chat_id)
        related = _semantic_memory_matches(
            memories_now,
            candidate,
            max_results=4,
            min_score=_AI_MEMORY_CONSOLIDATION_SCORE,
        )
        consolidation = await _consolidate_memory_candidates(settings, new_memory=candidate, related=related)
        action = str(consolidation.get("action") or "keep_new")
        if action == "ignore":
            continue
        merged_text = _coerce_summary_value(consolidation.get("memory") or candidate, 280)
        drop_ids = cast(list[str], consolidation.get("drop_ids") or [])
        for memory_id in drop_ids:
            _upsert_ai_memory(
                db,
                memory_id=memory_id,
                chat_id=chat_id,
                memory_text="",
                source_turn_id=turn_id,
                is_deleted=True,
            )
        new_id = str(uuid.uuid4())
        _upsert_ai_memory(
            db,
            memory_id=new_id,
            chat_id=chat_id,
            memory_text=merged_text,
            source_turn_id=turn_id,
            is_deleted=False,
        )
        saved_memory_ids.append(new_id)

    _emit_ai_helper_log_event(
        event_name="turn.complete",
        chat_id=chat_id,
        turn_id=turn_id,
        page=page,
        model=model,
        guard_model=guard_model,
        thinking_level=thinking_level,
        body="AI helper turn completed",
        attrs={
            "gen_ai.response.id": turn_id,
            "gen_ai.input.question": question,
            "gen_ai.usage.input_tokens": model_stats.get("prompt_tokens", 0),
            "gen_ai.usage.output_tokens": model_stats.get("completion_tokens", 0),
            "gen_ai.usage.thinking_tokens": model_stats.get("thinking_tokens", 0),
            "gen_ai.response.latency_ms": model_stats.get("elapsed_ms", 0),
            "gen_ai.output.messages": json.dumps([{"role": "assistant", "content": final_answer}], ensure_ascii=False),
            "gen_ai.turn.summary.request": summary.get("request", ""),
            "gen_ai.turn.summary.action": summary.get("action", ""),
            "gen_ai.turn.summary.result": summary.get("result", ""),
            "gen_ai.memory.saved_ids": json.dumps(saved_memory_ids, ensure_ascii=False),
        },
    )
    _emit_ai_helper_log_event(
        event_name="turn.summary",
        chat_id=chat_id,
        turn_id=turn_id,
        page=page,
        model=model,
        guard_model=guard_model,
        thinking_level=thinking_level,
        body="AI helper turn summary",
        attrs={
            "gen_ai.turn.summary.request": summary.get("request", ""),
            "gen_ai.turn.summary.action": summary.get("action", ""),
            "gen_ai.turn.summary.result": summary.get("result", ""),
        },
    )

    return jsonify(
        {
            "ok": True,
            "answer": final_answer,
            "model": model,
            "chat_id": chat_id,
            "turn_id": turn_id,
            "thinking_level": thinking_level,
            "turn_logs_url": turn_logs_url,
            "guard_stats": guard_stats,
            "model_stats": model_stats,
            "turn_summary": summary,
            "saved_memory_ids": saved_memory_ids,
            "tool_proposals": proposed_tools,
        }
    )


@ai_bp.route("/api/ai/helper/actions/execute", methods=["POST"])
@require_basic_auth
async def ai_helper_execute_action():
    payload = await request.get_json(force=True, silent=True) or {}
    token = str(payload.get("action_token") or "").strip()
    if not token:
        return jsonify({"ok": False, "error": "action_token is required"}), 400

    decoded = _decode_ai_action_token(token)
    if not decoded:
        return jsonify({"ok": False, "error": "Invalid or expired action token"}), 400

    action_id = str(decoded.get("action_id") or "").strip()
    target_page = str(decoded.get("target_page") or "").strip() or "/logs"
    action_payload = cast(dict[str, Any], decoded.get("action") or {})
    chat_id = str(decoded.get("chat_id") or "").strip()
    turn_id = str(decoded.get("turn_id") or "").strip()

    action_meta = _action_meta_for_page(target_page, action_id)
    if not action_meta:
        action_meta = _action_meta_for_id(action_id)
    if not action_meta:
        return jsonify({"ok": False, "error": "Action is not allowed for this page"}), 400
    if not bool(action_meta.get("implemented", False)):
        return jsonify({"ok": False, "error": "Action is not implemented"}), 400

    action_type = str(action_meta.get("action_type") or action_payload.get("type") or "").strip().lower()
    client_action = _build_client_action(action_type, action_payload)
    if not client_action:
        return jsonify({"ok": False, "error": "Action payload is invalid"}), 400

    requires_confirmation = bool(decoded.get("requires_confirmation", action_meta.get("requires_confirmation", True)))
    confirmed = bool(payload.get("confirm"))
    if requires_confirmation and not confirmed:
        return (
            jsonify(
                {
                    "ok": False,
                    "error": "Confirmation required",
                    "requires_confirmation": True,
                }
            ),
            409,
        )

    _emit_ai_helper_log_event(
        event_name="tool.executed",
        chat_id=chat_id,
        turn_id=turn_id,
        page=target_page,
        model="",
        guard_model="",
        thinking_level="off",
        body=f"Executed action: {action_id}",
        attrs={
            "gen_ai.tool.name": "propose_ui_action",
            "sobs.ai.action_id": action_id,
            "sobs.ai.tool.action": json.dumps(client_action, ensure_ascii=False),
            "sobs.ai.action.status": "executed",
        },
    )

    return jsonify(
        {
            "ok": True,
            "action_id": action_id,
            "client_action": client_action,
            "chat_id": chat_id,
            "turn_id": turn_id,
        }
    )


@ai_bp.route("/api/agent/runs", methods=["GET"])
@require_basic_auth
async def list_agent_runs():
    db = get_db()
    try:
        limit = max(1, min(200, int(request.args.get("limit", 50))))
    except (TypeError, ValueError):
        limit = 50
    runs = _load_agent_runs(db, limit=limit)
    return jsonify({"ok": True, "runs": runs})


@ai_bp.route("/api/agent/runs", methods=["POST"])
@require_basic_auth
async def trigger_agent_run():
    """Manually trigger an agent flow for a given rule_id."""
    payload = await request.get_json(force=True, silent=True) or {}
    rule_id = str(payload.get("rule_id") or "").strip()
    extra_context = str(payload.get("extra_context") or "").strip()

    if not rule_id:
        return jsonify({"ok": False, "error": "rule_id is required"}), 400

    db = get_db()
    rule = _load_agent_rule(db, rule_id)
    if not rule:
        return jsonify({"ok": False, "error": "agent rule not found"}), 404

    settings = _load_all_ai_settings(db)
    if not settings.get("ai.endpoint_url") or not settings.get("ai.model"):
        return (
            jsonify(
                {
                    "ok": False,
                    "error": "AI endpoint not configured. Visit Settings → AI Configuration.",
                }
            ),
            503,
        )

    # Rate limit check
    rate_limit_minutes = rule.get("rate_limit_minutes", 60)
    last_run_ts = _agent_rule_last_run_ts(db, rule_id)
    elapsed_minutes = (time.time() - last_run_ts) / 60.0
    if elapsed_minutes < rate_limit_minutes and last_run_ts > 0:
        return (
            jsonify(
                {
                    "ok": False,
                    "error": f"Rate limit: this rule ran {elapsed_minutes:.0f}m ago "
                    f"(limit: every {rate_limit_minutes}m)",
                }
            ),
            429,
        )

    trigger_context = {
        "rule_name": rule["name"],
        "trigger_state": "manual",
        "trigger_type": "manual",
        "trigger_ref_id": "",
        "extra": extra_context,
    }
    outcome = await _maybe_await(_run_agent_rule_instance(db, rule, settings, trigger_context))
    if not outcome.get("ok"):
        return (
            jsonify({"ok": False, "error": outcome.get("error", "agent flow failed"), "run_id": outcome["run_id"]}),
            500,
        )

    return jsonify({"ok": True, "run_id": outcome["run_id"], "result": outcome["result"]})


@ai_bp.route("/api/agent/runs/<run_id>/dismiss", methods=["POST"])
@require_basic_auth
async def dismiss_agent_run(run_id: str):
    db = get_db()
    row = db.execute(
        "SELECT Id, RuleId, RuleName, TriggerContext, Status, GuardDecision, DlpResult, "
        "Analysis, Suggestion, GithubIssueUrl, ErrorMessage, CreatedAt, CompletedAt "
        "FROM sobs_agent_runs FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1",
        [run_id],
    ).fetchone()
    if not row:
        return jsonify({"ok": False, "error": "run not found"}), 404
    _insert_rows_json_each_row(
        db,
        "sobs_agent_runs",
        [
            {
                "Id": run_id,
                "RuleId": str(row["RuleId"]),
                "RuleName": str(row["RuleName"]),
                "TriggerContext": str(row["TriggerContext"]),
                "Status": str(row["Status"]),
                "GuardDecision": str(row["GuardDecision"]),
                "DlpResult": str(row["DlpResult"]),
                "Analysis": str(row["Analysis"]),
                "Suggestion": str(row["Suggestion"]),
                "GithubIssueUrl": str(row["GithubIssueUrl"]),
                "ErrorMessage": str(row["ErrorMessage"]),
                "CreatedAt": str(row["CreatedAt"]),
                "CompletedAt": str(row["CompletedAt"]),
                "IsDismissed": 1,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    return jsonify({"ok": True})
