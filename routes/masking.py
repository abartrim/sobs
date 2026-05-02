from __future__ import annotations

import logging
import re

from quart import Blueprint, flash, redirect, render_template, request, url_for

import masking as _masking
from app import (  # noqa: E402
    _MASKING_OUTPUT_ENABLED_SETTING,
    _MASKING_SQL_OUTPUT_ENABLED_SETTING,
    _is_truthy_setting,
    _load_masking_settings,
    _mask_string_for_output,
    _mask_value_for_output,
    _refresh_masking_runtime_rules,
    _save_masking_custom_keys,
    _save_masking_custom_patterns,
    _set_app_setting,
    _validate_custom_masking_pattern_for_storage,
    get_db,
    jsonify,
    require_basic_auth,
)

log = logging.getLogger("sobs")
masking_bp = Blueprint("masking", __name__)


@masking_bp.route("/settings/masking", methods=["GET"])
@require_basic_auth
async def view_masking_settings():
    db = get_db()
    settings = _load_masking_settings(db)
    return await render_template(
        "settings_masking.html",
        custom_keys=settings["custom_keys"],
        custom_patterns=settings["custom_patterns"],
        default_keys=settings["default_keys"],
        default_patterns=settings["default_patterns"],
        effective_key_count=len(settings["effective_keys"]),
        effective_pattern_count=len(settings["effective_patterns"]),
        output_masking_enabled=settings["output_masking_enabled"],
        sql_output_masking_enabled=settings["sql_output_masking_enabled"],
    )


@masking_bp.route("/settings/masking/keys", methods=["POST"])
@require_basic_auth
async def add_masking_key():
    db = get_db()
    key = _masking.normalize_sensitive_key((await request.form).get("key"))
    settings = _load_masking_settings(db)
    if not key:
        await flash("Sensitive key name is required", "warning")
        return redirect(url_for("masking.view_masking_settings"))
    if key in settings["effective_keys"]:
        await flash(f"Sensitive key '{key}' is already active", "info")
        return redirect(url_for("masking.view_masking_settings"))

    custom_keys = [*settings["custom_keys"], key]
    _save_masking_custom_keys(db, custom_keys)
    _refresh_masking_runtime_rules(db)
    await flash(f"Sensitive key '{key}' added", "success")
    return redirect(url_for("masking.view_masking_settings"))


@masking_bp.route("/settings/masking/keys/delete", methods=["POST"])
@require_basic_auth
async def delete_masking_key():
    db = get_db()
    key = _masking.normalize_sensitive_key((await request.form).get("key"))
    settings = _load_masking_settings(db)
    if key not in settings["custom_keys"]:
        await flash("Custom sensitive key not found", "warning")
        return redirect(url_for("masking.view_masking_settings"))

    custom_keys = [item for item in settings["custom_keys"] if item != key]
    _save_masking_custom_keys(db, custom_keys)
    _refresh_masking_runtime_rules(db)
    await flash(f"Sensitive key '{key}' removed", "success")
    return redirect(url_for("masking.view_masking_settings"))


@masking_bp.route("/settings/masking/patterns", methods=["POST"])
@require_basic_auth
async def add_masking_pattern():
    db = get_db()
    raw_pattern = (await request.form).get("pattern")
    settings = _load_masking_settings(db)
    try:
        pattern = _validate_custom_masking_pattern_for_storage(raw_pattern)
    except (ValueError, re.error) as exc:
        await flash(f"Invalid regex pattern: {exc}", "warning")
        return redirect(url_for("masking.view_masking_settings"))

    if pattern in settings["effective_patterns"]:
        await flash("That regex pattern is already active", "info")
        return redirect(url_for("masking.view_masking_settings"))

    custom_patterns = [*settings["custom_patterns"], pattern]
    _save_masking_custom_patterns(db, custom_patterns)
    _refresh_masking_runtime_rules(db)
    await flash("Custom masking pattern added", "success")
    return redirect(url_for("masking.view_masking_settings"))


@masking_bp.route("/settings/masking/patterns/delete", methods=["POST"])
@require_basic_auth
async def delete_masking_pattern():
    db = get_db()
    raw_pattern = (await request.form).get("pattern")
    settings = _load_masking_settings(db)
    try:
        pattern = _validate_custom_masking_pattern_for_storage(raw_pattern)
    except (ValueError, re.error):
        await flash("Custom masking pattern not found", "warning")
        return redirect(url_for("masking.view_masking_settings"))

    if pattern not in settings["custom_patterns"]:
        await flash("Custom masking pattern not found", "warning")
        return redirect(url_for("masking.view_masking_settings"))

    custom_patterns = [item for item in settings["custom_patterns"] if item != pattern]
    _save_masking_custom_patterns(db, custom_patterns)
    _refresh_masking_runtime_rules(db)
    await flash("Custom masking pattern removed", "success")
    return redirect(url_for("masking.view_masking_settings"))


@masking_bp.route("/settings/masking/output", methods=["POST"])
@require_basic_auth
async def update_masking_output_setting():
    db = get_db()
    form = await request.form
    enabled_values = form.getlist("enabled")
    enabled = any(_is_truthy_setting(value, default=False) for value in enabled_values)
    _set_app_setting(db, _MASKING_OUTPUT_ENABLED_SETTING, "1" if enabled else "0")
    await flash(
        (
            "Global output masking enabled"
            if enabled
            else "Global output masking disabled across UI/JSON/notifications/GitHub issue payloads"
        ),
        "success",
    )
    return redirect(url_for("masking.view_masking_settings"))


@masking_bp.route("/settings/masking/sql-output", methods=["POST"])
@require_basic_auth
async def update_masking_sql_output_setting():
    db = get_db()
    form = await request.form
    # Browser submissions can send both hidden and checkbox values for the same
    # field name. Treat the toggle as enabled if any submitted value is truthy.
    enabled_values = form.getlist("enabled")
    enabled = any(_is_truthy_setting(value, default=False) for value in enabled_values)
    _set_app_setting(db, _MASKING_SQL_OUTPUT_ENABLED_SETTING, "1" if enabled else "0")
    await flash(
        (
            "SQL output masking enabled for NLQ/chart endpoints"
            if enabled
            else "SQL output masking disabled for NLQ/chart endpoints"
        ),
        "success",
    )
    return redirect(url_for("masking.view_masking_settings"))


@masking_bp.route("/api/settings/masking/preview", methods=["POST"])
@require_basic_auth
async def api_masking_preview():
    payload = await request.get_json(silent=True)
    value = (payload or {}).get("value")
    masked = _mask_value_for_output(value) if isinstance(value, (dict, list)) else _mask_string_for_output(value)
    return jsonify({"ok": True, "masked": masked})


@masking_bp.route("/api/settings/masking/rules", methods=["GET"])
@require_basic_auth
async def api_masking_rules():
    settings = _load_masking_settings(get_db())
    return jsonify(
        {
            "ok": True,
            "keys": settings["effective_keys"],
            "patterns": settings["effective_patterns"],
            "custom_keys": settings["custom_keys"],
            "custom_patterns": settings["custom_patterns"],
            "output_masking_enabled": settings["output_masking_enabled"],
            "sql_output_masking_enabled": settings["sql_output_masking_enabled"],
        }
    )
