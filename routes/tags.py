from __future__ import annotations

import json
import re
import time
import uuid
from typing import Any

from quart import Blueprint, flash, redirect, render_template, request, url_for

from app import (  # noqa: E402
    _AUTO_TAG_RULE_CREATE_MAX,
    _TAG_RULE_FIELDS,
    _TAG_RULE_OPERATORS,
    _TAG_RULE_RECORD_TYPES,
    RowCompat,
    _build_auto_tag_rule_candidates,
    _get_record_tags,
    _insert_rows_json_each_row,
    _list_tag_candidate_services,
    _load_tag_rules,
    _notification_condition_service_suggestions,
    _record_tag_key_suggestions,
    _record_tag_value_suggestions,
    _soft_delete_latest_row,
    _tag_rule_attribute_key_suggestions,
    _tag_rule_value_suggestions,
    get_db,
    jsonify,
    masked_jsonify,
    require_api_key,
    require_basic_auth,
)

tags_bp = Blueprint("tags", __name__)


@tags_bp.route("/settings/tags")
@require_basic_auth
async def view_tag_rules():
    db = get_db()
    open_panel = (request.args.get("open_panel") or "").strip().lower()
    if open_panel not in {"auto-tags"}:
        open_panel = ""
    rules = _load_tag_rules(db)
    edit_rule_id = (request.args.get("edit_rule") or "").strip()
    edit_rule = None
    if edit_rule_id:
        edit_rule = next((rule for rule in rules if rule.get("id") == edit_rule_id), None)
        if not edit_rule:
            await flash("Tag rule not found for editing", "warning")
    services = _list_tag_candidate_services(db)
    return await render_template(
        "settings_tags.html",
        rules=rules,
        edit_rule=edit_rule,
        record_types=_TAG_RULE_RECORD_TYPES,
        match_fields=_TAG_RULE_FIELDS,
        match_operators=_TAG_RULE_OPERATORS,
        services=services,
        auto_preview=[],
        auto_summary=None,
        auto_open_panel=open_panel,
    )


@tags_bp.route("/api/settings/tags/condition-suggestions", methods=["GET"])
@require_basic_auth
async def api_tag_rule_condition_suggestions():
    db = get_db()
    scope = (request.args.get("scope") or "tag_rule").strip().lower()
    field = (request.args.get("field") or "").strip().lower()
    operator = (request.args.get("operator") or "eq").strip().lower()
    query_text = (request.args.get("q") or "").strip()
    attr_key = (request.args.get("attr_key") or "").strip()
    source = (request.args.get("source") or "").strip().lower()
    signal = (request.args.get("signal") or "").strip()
    record_type = (request.args.get("record_type") or "all").strip().lower()
    tag_key = (request.args.get("tag_key") or "").strip()
    target = (request.args.get("target") or "value").strip().lower()
    try:
        limit = max(3, min(20, int(request.args.get("limit") or 8)))
    except (TypeError, ValueError):
        limit = 8

    if scope == "tag_rule":
        if target == "attr_key":
            suggestions = _tag_rule_attribute_key_suggestions(db, query_text, limit)
        else:
            suggestions = _tag_rule_value_suggestions(db, field, operator, query_text, attr_key, limit)
    else:
        if target == "service":
            suggestions = _notification_condition_service_suggestions(db, query_text, limit, source, signal)
        elif target == "tag_key":
            suggestions = _record_tag_key_suggestions(db, query_text, limit, record_type)
        elif target == "tag_value":
            suggestions = _record_tag_value_suggestions(db, tag_key, query_text, limit, record_type)
        else:
            suggestions = []

    return masked_jsonify(
        {
            "ok": True,
            "scope": scope,
            "field": field,
            "operator": operator,
            "target": target,
            "suggestions": suggestions,
        }
    )


@tags_bp.route("/settings/tags/auto", methods=["POST"])
@require_basic_auth
async def auto_tag_rules():
    form = await request.form
    action = (form.get("action") or "preview").strip().lower()
    try:
        hours = max(1, min(168, int(form.get("hours") or 24)))
    except (TypeError, ValueError):
        hours = 24
    try:
        min_count = max(1, min(5000, int(form.get("min_count") or 30)))
    except (TypeError, ValueError):
        min_count = 30

    service_filter = (form.get("service_filter") or "").strip()
    selected_record_types = [rt.strip().lower() for rt in form.getlist("auto_record_types") if rt and rt.strip()]
    if not selected_record_types:
        selected_record_types = ["log", "trace", "error", "ai", "rum"]

    db = get_db()
    rules = _load_tag_rules(db)
    services = _list_tag_candidate_services(db)

    candidates, stats = _build_auto_tag_rule_candidates(
        db,
        hours=hours,
        min_count=min_count,
        service_filter=service_filter,
        record_types=selected_record_types,
    )

    summary = {
        "action": action,
        "hours": hours,
        "min_count": min_count,
        "service_filter": service_filter,
        "record_types": selected_record_types,
        "examined": stats["examined"],
        "existing": stats["existing"],
        "invalid": stats["invalid"],
        "candidates": len(candidates),
        "create_cap": _AUTO_TAG_RULE_CREATE_MAX,
        "capped": len(candidates) > _AUTO_TAG_RULE_CREATE_MAX,
        "created": 0,
    }

    if action == "create":
        limited_candidates = candidates[:_AUTO_TAG_RULE_CREATE_MAX]
        version = int(time.time() * 1000)
        rows_to_insert: list[dict[str, object]] = []
        for idx, candidate in enumerate(limited_candidates):
            rows_to_insert.append(
                {
                    "Id": str(uuid.uuid4()),
                    "Name": str(candidate["name"]),
                    "RecordTypes": ",".join([str(rt) for rt in candidate["record_types"]]),
                    "MatchField": str(candidate["match_field"]),
                    "MatchOperator": str(candidate["match_operator"]),
                    "MatchValue": str(candidate["match_value"]),
                    "MatchAttrKey": str(candidate["match_attr_key"]),
                    "TagKey": str(candidate["tag_key"]),
                    "TagValue": str(candidate["tag_value"]),
                    "ConditionsJson": json.dumps(
                        [
                            {
                                "match_field": str(candidate["match_field"]),
                                "match_operator": str(candidate["match_operator"]),
                                "match_value": str(candidate["match_value"]),
                                "match_attr_key": str(candidate["match_attr_key"]),
                            }
                        ],
                        ensure_ascii=False,
                    ),
                    "IsDeleted": 0,
                    "Version": version + idx,
                }
            )
        if rows_to_insert:
            _insert_rows_json_each_row(db, "sobs_tag_rules", rows_to_insert)
        summary["created"] = len(rows_to_insert)
        skipped_by_cap = max(0, len(candidates) - len(limited_candidates))
        cap_suffix = f", skipped {skipped_by_cap} by max cap ({_AUTO_TAG_RULE_CREATE_MAX})." if skipped_by_cap else "."
        await flash(
            (
                f"Auto tag rule generation complete: created {summary['created']} rule(s), "
                f"skipped {summary['existing']} existing, {summary['invalid']} invalid"
                f"{cap_suffix}"
            ),
            "success",
        )
        return redirect(url_for("tags.view_tag_rules", open_panel="auto-tags"))

    await flash(
        (
            f"Auto-tag preview: {summary['candidates']} candidate(s), "
            f"{summary['existing']} existing skipped, {summary['invalid']} invalid."
        ),
        "info",
    )
    return await render_template(
        "settings_tags.html",
        rules=rules,
        record_types=_TAG_RULE_RECORD_TYPES,
        match_fields=_TAG_RULE_FIELDS,
        match_operators=_TAG_RULE_OPERATORS,
        services=services,
        auto_preview=candidates,
        auto_summary=summary,
        auto_open_panel="auto-tags",
    )


@tags_bp.route("/settings/tags", methods=["POST"])
@require_basic_auth
async def create_tag_rule():
    form = await request.form
    edit_rule_id = (form.get("edit_rule_id") or "").strip()
    redirect_endpoint = (
        url_for("tags.view_tag_rules", edit_rule=edit_rule_id) if edit_rule_id else url_for("tags.view_tag_rules")
    )
    name = (form.get("name") or "").strip()
    record_types_list = form.getlist("record_types")
    tag_key = (form.get("tag_key") or "").strip()
    tag_value = (form.get("tag_value") or "").strip()

    # --- Composite conditions ---------------------------------------------------
    # The form may submit multiple conditions via parallel lists:
    #   condition_field[]  condition_operator[]  condition_value[]  condition_attr_key[]
    # When at least two conditions are present the rule is "composite".
    # When exactly one condition is provided it is stored both as ConditionsJson
    # (for forward-compat reads) AND in the legacy MatchField/MatchOperator/MatchValue
    # columns (for backward compat with existing query paths).
    cond_fields = form.getlist("condition_field")
    cond_operators = form.getlist("condition_operator")
    cond_values = form.getlist("condition_value")
    cond_attr_keys = form.getlist("condition_attr_key")

    # Zip together, padding shorter lists with empty strings
    n = max(len(cond_fields), len(cond_operators), len(cond_values), len(cond_attr_keys))

    def _get(lst: list, i: int) -> str:
        return lst[i].strip() if i < len(lst) else ""

    conditions: list[dict] = []
    for i in range(n):
        f = _get(cond_fields, i).lower()
        op = _get(cond_operators, i).lower() or "eq"
        val = _get(cond_values, i)
        attr = _get(cond_attr_keys, i)
        if f:
            conditions.append({"match_field": f, "match_operator": op, "match_value": val, "match_attr_key": attr})

    # Fall back to single-condition fields if no composite conditions supplied
    if not conditions:
        match_field = (form.get("match_field") or "").strip().lower()
        match_operator = (form.get("match_operator") or "eq").strip().lower()
        match_value = (form.get("match_value") or "").strip()
        match_attr_key = (form.get("match_attr_key") or "").strip()
        if match_field:
            conditions = [
                {
                    "match_field": match_field,
                    "match_operator": match_operator,
                    "match_value": match_value,
                    "match_attr_key": match_attr_key,
                }
            ]

    if not name or not conditions or not tag_key or not tag_value:
        await flash("Name, at least one match condition, tag key, and tag value are required", "warning")
        return redirect(redirect_endpoint)

    valid_fields = set(_TAG_RULE_FIELDS)
    valid_ops = set(_TAG_RULE_OPERATORS)
    for cond in conditions:
        if cond["match_field"] not in valid_fields:
            await flash(f"Invalid match field: {cond['match_field']}", "warning")
            return redirect(redirect_endpoint)
        if cond["match_operator"] not in valid_ops:
            await flash(f"Invalid match operator: {cond['match_operator']}", "warning")
            return redirect(redirect_endpoint)
        if cond["match_field"] == "attribute" and not cond["match_attr_key"]:
            await flash("Attribute key is required when match field is 'attribute'", "warning")
            return redirect(redirect_endpoint)
        if cond["match_operator"] == "regex":
            try:
                re.compile(cond["match_value"])
            except re.error as exc:
                await flash(f"Invalid regex pattern: {exc}", "warning")
                return redirect(redirect_endpoint)

    # Normalise record types
    valid_types = set(_TAG_RULE_RECORD_TYPES)
    chosen = [t.strip() for t in record_types_list if t.strip() in valid_types]
    record_types_str = ",".join(chosen) if chosen else "all"

    # For the legacy single-condition columns use the first condition.
    primary = conditions[0]

    rule_id = str(uuid.uuid4())
    if edit_rule_id:
        existing_row = (
            get_db()
            .execute(
                "SELECT Id FROM sobs_tag_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
                [edit_rule_id],
            )
            .fetchone()
        )
        if not existing_row:
            await flash("Tag rule not found for editing", "warning")
            return redirect(url_for("tags.view_tag_rules"))
        rule_id = str(existing_row["Id"])

    _insert_rows_json_each_row(
        get_db(),
        "sobs_tag_rules",
        [
            {
                "Id": rule_id,
                "Name": name,
                "RecordTypes": record_types_str,
                "MatchField": primary["match_field"],
                "MatchOperator": primary["match_operator"],
                "MatchValue": primary["match_value"],
                "MatchAttrKey": primary["match_attr_key"],
                "TagKey": tag_key,
                "TagValue": tag_value,
                "ConditionsJson": json.dumps(conditions, ensure_ascii=False),
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    await flash(f"Tag rule '{name}' {'updated' if edit_rule_id else 'created'}", "success")
    return redirect(url_for("tags.view_tag_rules"))


@tags_bp.route("/settings/tags/<rule_id>/delete", methods=["POST"])
@require_basic_auth
async def delete_tag_rule(rule_id: str):
    db = get_db()

    def _deleted_row(row: RowCompat) -> dict[str, Any]:
        return {
            "Id": rule_id,
            "Name": str(row["Name"]),
            "RecordTypes": "",
            "MatchField": "",
            "MatchOperator": "eq",
            "MatchValue": "",
            "MatchAttrKey": "",
            "TagKey": "",
            "TagValue": "",
            "ConditionsJson": "[]",
        }

    return await _soft_delete_latest_row(
        db,
        select_sql="SELECT Id, Name FROM sobs_tag_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
        select_params=[rule_id],
        table_name="sobs_tag_rules",
        build_deleted_row=_deleted_row,
        not_found_message="Tag rule not found",
        success_message="Tag rule '{name}' deleted",
        redirect_endpoint="view_tag_rules",
    )


# ---------------------------------------------------------------------------
# Record Tags API  GET/POST /api/tags/<record_type>/<record_id>
#                  DELETE /api/tags/<record_type>/<record_id>/<tag_key>
# ---------------------------------------------------------------------------
@tags_bp.route("/api/tags/<record_type>/<record_id>", methods=["GET"])
@require_api_key
async def api_get_tags(record_type: str, record_id: str):
    db = get_db()
    tags = _get_record_tags(db, record_type, record_id)
    return jsonify({"tags": tags})


@tags_bp.route("/api/tags/<record_type>/<record_id>", methods=["POST"])
@require_api_key
async def api_add_tag(record_type: str, record_id: str):
    payload = await request.get_json(force=True, silent=True) or {}
    tag_key = str(payload.get("key", "")).strip()
    tag_value = str(payload.get("value", "")).strip()
    if not tag_key:
        return jsonify({"error": "key is required"}), 400
    if len(tag_key) > 128 or len(tag_value) > 512:
        return jsonify({"error": "tag key or value too long"}), 400
    _insert_rows_json_each_row(
        get_db(),
        "sobs_record_tags",
        [
            {
                "RecordType": record_type,
                "RecordId": record_id,
                "TagKey": tag_key,
                "TagValue": tag_value,
                "IsAuto": 0,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    return jsonify({"ok": True}), 201


@tags_bp.route("/api/tags/<record_type>/<record_id>/<tag_key>", methods=["DELETE"])
@require_api_key
async def api_delete_tag(record_type: str, record_id: str, tag_key: str):
    db = get_db()
    rows = db.execute(
        "SELECT TagKey, TagValue, IsAuto FROM sobs_record_tags FINAL "
        "WHERE RecordType = ? AND RecordId = ? AND TagKey = ? AND IsDeleted = 0",
        [record_type, record_id, tag_key],
    ).fetchall()
    if not rows:
        return jsonify({"error": "tag not found"}), 404
    tombstones = []
    version = int(time.time() * 1000)
    seen_values: set[tuple[str, int]] = set()
    for row in rows:
        tag_value = str(row["TagValue"])
        is_auto = int(row["IsAuto"])
        dedupe_key = (tag_value, is_auto)
        if dedupe_key in seen_values:
            continue
        seen_values.add(dedupe_key)
        tombstones.append(
            {
                "RecordType": record_type,
                "RecordId": record_id,
                "TagKey": tag_key,
                "TagValue": tag_value,
                "IsAuto": is_auto,
                "IsDeleted": 1,
                "Version": version,
            }
        )
        version += 1
    _insert_rows_json_each_row(
        db,
        "sobs_record_tags",
        tombstones,
    )
    return jsonify({"ok": True}), 200
