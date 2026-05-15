from __future__ import annotations

import asyncio  # noqa: F401
import json
import os
import re
import time
import uuid
from datetime import datetime, timezone
from typing import Any, cast

from quart import Blueprint, Response, flash, redirect, render_template, request, url_for

import app as sobs_app
from app import (  # noqa: E402
    _NOTIFICATION_CHANNEL_TYPES,
    _NOTIFICATION_COMPARATORS,
    _NOTIFICATION_CONDITION_TYPES,
    _NOTIFICATION_LOGIC_OPERATORS,
    _NOTIFICATION_SEVERITIES,
    _NOTIFICATION_SIGNAL_SOURCES,
    _NOTIFICATION_TAG_MATCH_OPERATORS,
    _NOTIFICATION_TAG_RECORD_TYPES,
    _VAPID_PRIVATE_KEY_SETTING,
    ChDbConnection,
    RowCompat,
    _agent_rule_last_run_ts,
    _agent_rule_trigger_state_matches,
    _decrypt_notification_config,
    _del_app_setting,
    _dispatch_notification_channel,
    _encrypt_notification_config,
    _get_vapid_public_key,
    _insert_rows_json_each_row,
    _is_truthy_setting,
    _load_anomaly_rules,
    _load_notification_log,
    _mask_string_for_output,
    _maybe_await,
    _normalize_agent_trigger_state,
    _notification_channel_mask_output_enabled,
    _register_raw_window,
    _set_app_setting,
    _soft_delete_latest_row,
    get_db,
    jsonify,
    log,
    require_basic_auth,
)

notifications_bp: Blueprint = Blueprint("notifications", __name__)


def _load_notification_channels(*args: Any, **kwargs: Any):
    return sobs_app._load_notification_channels(*args, **kwargs)


def _load_notification_rules(*args: Any, **kwargs: Any):
    return sobs_app._load_notification_rules(*args, **kwargs)


def _check_notification_rule(*args: Any, **kwargs: Any):
    return sobs_app._check_notification_rule(*args, **kwargs)


def _collect_anomaly_agent_events(*args: Any, **kwargs: Any):
    return sobs_app._collect_anomaly_agent_events(*args, **kwargs)


def _collect_tag_rule_agent_events(*args: Any, **kwargs: Any):
    return sobs_app._collect_tag_rule_agent_events(*args, **kwargs)


def _generate_vapid_keys(*args: Any, **kwargs: Any):
    return sobs_app._generate_vapid_keys(*args, **kwargs)


def _load_agent_rules(*args: Any, **kwargs: Any):
    return sobs_app._load_agent_rules(*args, **kwargs)


def _load_all_ai_settings(*args: Any, **kwargs: Any):
    return sobs_app._load_all_ai_settings(*args, **kwargs)


def _run_agent_rule_instance(*args: Any, **kwargs: Any):
    return sobs_app._run_agent_rule_instance(*args, **kwargs)


@notifications_bp.route("/settings/notifications")
@require_basic_auth
async def view_notifications():
    """Notification channels and rules management page."""
    db = get_db()
    channels = _load_notification_channels(db)
    rules = _load_notification_rules(db)
    edit_rule_id = (request.args.get("edit_rule") or "").strip()
    edit_rule = next((rule for rule in rules if str(rule.get("id", "")) == edit_rule_id), None)
    notification_log = _load_notification_log(db, limit=50)
    vapid_public_key, vapid_key_source = _get_vapid_public_key(db)
    metric_rules = _load_anomaly_rules(db)
    return await render_template(
        "settings_notifications.html",
        channels=channels,
        rules=rules,
        notification_log=notification_log,
        channel_types=_NOTIFICATION_CHANNEL_TYPES,
        comparators=_NOTIFICATION_COMPARATORS,
        condition_types=_NOTIFICATION_CONDITION_TYPES,
        severities=_NOTIFICATION_SEVERITIES,
        logic_operators=_NOTIFICATION_LOGIC_OPERATORS,
        signal_sources=_NOTIFICATION_SIGNAL_SOURCES,
        tag_match_operators=_NOTIFICATION_TAG_MATCH_OPERATORS,
        tag_record_types=_NOTIFICATION_TAG_RECORD_TYPES,
        edit_rule=edit_rule,
        vapid_public_key=vapid_public_key,
        vapid_key_source=vapid_key_source,
        metric_rules=metric_rules,
    )


@notifications_bp.route("/settings/notifications/channels", methods=["POST"])
@require_basic_auth
async def create_notification_channel():
    """Create a new notification channel."""
    form = await request.form
    name = (form.get("name") or "").strip()
    channel_type = (form.get("channel_type") or "").strip().lower()
    mask_output_values = form.getlist("mask_output_enabled")
    mask_output_enabled = any(_is_truthy_setting(value, default=False) for value in mask_output_values)
    if not mask_output_values:
        mask_output_enabled = True

    if not name:
        await flash("Channel name is required", "warning")
        return redirect(url_for("notifications.view_notifications"))
    if channel_type not in _NOTIFICATION_CHANNEL_TYPES:
        await flash(f"Invalid channel type: {channel_type}", "warning")
        return redirect(url_for("notifications.view_notifications"))

    # Build config dict from form fields for the selected channel type
    config: dict[str, str] = {}
    if channel_type == "webhook":
        config["url"] = (form.get("webhook_url") or "").strip()
        config["method"] = (form.get("webhook_method") or "POST").strip().upper()
        config["headers"] = (form.get("webhook_headers") or "{}").strip()
        config["body_template"] = (form.get("webhook_body_template") or "").strip()
        if not config["url"]:
            await flash("Webhook URL is required", "warning")
            return redirect(url_for("notifications.view_notifications"))
    elif channel_type == "slack":
        config["webhook_url"] = (form.get("slack_webhook_url") or "").strip()
        if not config["webhook_url"]:
            await flash("Slack webhook URL is required", "warning")
            return redirect(url_for("notifications.view_notifications"))
    elif channel_type == "email":
        config["smtp_host"] = (form.get("smtp_host") or "localhost").strip()
        config["smtp_port"] = (form.get("smtp_port") or "587").strip()
        config["smtp_user"] = (form.get("smtp_user") or "").strip()
        config["smtp_password"] = (form.get("smtp_password") or "").strip()
        config["from_addr"] = (form.get("from_addr") or "sobs@localhost").strip()
        config["to_addr"] = (form.get("to_addr") or "").strip()
        config["use_tls"] = (form.get("use_tls") or "1").strip()
        if not config["to_addr"]:
            await flash("Email recipient (to_addr) is required", "warning")
            return redirect(url_for("notifications.view_notifications"))
    elif channel_type == "browser_push":
        config["endpoint"] = (form.get("push_endpoint") or "").strip()
        config["p256dh"] = (form.get("push_p256dh") or "").strip()
        config["auth"] = (form.get("push_auth") or "").strip()
        if not config["endpoint"]:
            await flash("Push endpoint is required", "warning")
            return redirect(url_for("notifications.view_notifications"))

    config["mask_output_enabled"] = "1" if mask_output_enabled else "0"

    channel_id = str(uuid.uuid4())
    stored_config = _encrypt_notification_config(config)
    _insert_rows_json_each_row(
        get_db(),
        "sobs_notification_channels",
        [
            {
                "Id": channel_id,
                "Name": name,
                "ChannelType": channel_type,
                "ConfigJson": json.dumps(stored_config, ensure_ascii=False),
                "Enabled": 1,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    await flash(f"Notification channel '{name}' created", "success")
    return redirect(url_for("notifications.view_notifications"))


@notifications_bp.route("/settings/notifications/channels/<channel_id>/delete", methods=["POST"])
@require_basic_auth
async def delete_notification_channel(channel_id: str):
    """Soft-delete a notification channel."""
    db = get_db()

    def _deleted_row(row: RowCompat) -> dict[str, Any]:
        return {
            "Id": channel_id,
            "Name": str(row["Name"]),
            "ChannelType": str(row["ChannelType"]),
            "ConfigJson": str(row["ConfigJson"]),
            "Enabled": int(row["Enabled"]),
        }

    return await _soft_delete_latest_row(
        db,
        select_sql=(
            "SELECT Id, Name, ChannelType, ConfigJson, Enabled "
            "FROM sobs_notification_channels FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1"
        ),
        select_params=[channel_id],
        table_name="sobs_notification_channels",
        build_deleted_row=_deleted_row,
        not_found_message="Notification channel not found",
        success_message="Notification channel '{name}' deleted",
        redirect_endpoint="notifications.view_notifications",
    )


@notifications_bp.route("/settings/notifications/channels/<channel_id>/toggle", methods=["POST"])
@require_basic_auth
async def toggle_notification_channel(channel_id: str):
    """Toggle enabled/disabled state of a notification channel."""
    db = get_db()
    row = db.execute(
        "SELECT Id, Name, ChannelType, ConfigJson, Enabled "
        "FROM sobs_notification_channels FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
        [channel_id],
    ).fetchone()
    if not row:
        await flash("Notification channel not found", "warning")
        return redirect(url_for("notifications.view_notifications"))
    new_enabled = 0 if int(row["Enabled"]) else 1
    _insert_rows_json_each_row(
        db,
        "sobs_notification_channels",
        [
            {
                "Id": channel_id,
                "Name": str(row["Name"]),
                "ChannelType": str(row["ChannelType"]),
                "ConfigJson": str(row["ConfigJson"]),
                "Enabled": new_enabled,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    state = "enabled" if new_enabled else "disabled"
    await flash(f"Notification channel '{row['Name']}' {state}", "success")
    return redirect(url_for("notifications.view_notifications"))


@notifications_bp.route("/api/notifications/channels/<channel_id>/test", methods=["POST"])
@require_basic_auth
async def test_notification_channel(channel_id: str):
    """Send a test notification through the given channel."""
    db = get_db()
    row = db.execute(
        "SELECT Id, Name, ChannelType, ConfigJson, Enabled "
        "FROM sobs_notification_channels FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
        [channel_id],
    ).fetchone()
    if not row:
        return jsonify({"ok": False, "error": "channel not found"}), 404
    channel = {
        "id": str(row["Id"]),
        "name": str(row["Name"]),
        "channel_type": str(row["ChannelType"]),
        "config": _decrypt_notification_config(json.loads(str(row["ConfigJson"]) or "{}")),
        "enabled": bool(int(row["Enabled"])),
    }
    test_payload = {
        "rule_name": "Test",
        "severity": "info",
        "conditions": [],
        "summary": (
            _mask_string_for_output(f"[SOBS] Test notification from channel '{channel['name']}'")
            if _notification_channel_mask_output_enabled(channel)
            else f"[SOBS] Test notification from channel '{channel['name']}'"
        ),
        "fired_at": datetime.now(timezone.utc).isoformat(),
    }
    result = await _dispatch_notification_channel(channel, test_payload)
    if result == "ok":
        return jsonify({"ok": True})
    return jsonify({"ok": False, "error": result}), 500


@notifications_bp.route("/settings/notifications/rules", methods=["POST"])
@require_basic_auth
async def create_notification_rule():
    """Create or update a notification rule."""
    form = await request.form
    edit_rule_id = (form.get("edit_rule_id") or "").strip()
    name = (form.get("name") or "").strip()
    logic_operator = (form.get("logic_operator") or "any").strip().lower()
    severity = (form.get("severity") or "warning").strip().lower()
    try:
        cooldown_seconds = max(0, min(86400, int(form.get("cooldown_seconds") or 300)))
    except (TypeError, ValueError):
        cooldown_seconds = 300
    channel_ids_raw = form.getlist("channel_ids")

    # Parse conditions from repeated form fields
    sources = form.getlist("cond_source")
    signals = form.getlist("cond_signal")
    services = form.getlist("cond_service")
    condition_types = form.getlist("cond_type")
    record_types = form.getlist("cond_record_type")
    tag_keys = form.getlist("cond_tag_key")
    tag_match_operators = form.getlist("cond_tag_match_operator")
    tag_values = form.getlist("cond_tag_value")
    comparators = form.getlist("cond_comparator")
    thresholds = form.getlist("cond_threshold")
    windows = form.getlist("cond_window_minutes")

    if not name:
        await flash("Rule name is required", "warning")
        return redirect(url_for("notifications.view_notifications"))
    if logic_operator not in _NOTIFICATION_LOGIC_OPERATORS:
        await flash(f"Invalid logic operator: {logic_operator}", "warning")
        return redirect(url_for("notifications.view_notifications"))
    if severity not in _NOTIFICATION_SEVERITIES:
        await flash(f"Invalid severity: {severity}", "warning")
        return redirect(url_for("notifications.view_notifications"))

    conditions = []
    row_count = max(
        len(condition_types),
        len(sources),
        len(signals),
        len(services),
        len(record_types),
        len(tag_keys),
        len(tag_match_operators),
        len(tag_values),
        len(comparators),
        len(thresholds),
        len(windows),
    )
    for i in range(row_count):
        condition_type = (condition_types[i] if i < len(condition_types) else "signal").strip().lower()
        if condition_type not in _NOTIFICATION_CONDITION_TYPES:
            await flash(f"Invalid notification condition type: {condition_type}", "warning")
            return redirect(url_for("notifications.view_notifications"))

        comparator = (comparators[i] if i < len(comparators) else "gt").strip().lower()
        try:
            threshold = float(thresholds[i] if i < len(thresholds) else 0)
        except (TypeError, ValueError):
            threshold = 0.0
        try:
            window_minutes = max(1, min(60, int(windows[i] if i < len(windows) else 5)))
        except (TypeError, ValueError):
            window_minutes = 5

        if comparator not in _NOTIFICATION_COMPARATORS:
            comparator = "gt"
        if condition_type == "tag":
            record_type = (record_types[i] if i < len(record_types) else "all").strip().lower()
            tag_key = (tag_keys[i] if i < len(tag_keys) else "").strip()
            tag_match_operator = (tag_match_operators[i] if i < len(tag_match_operators) else "eq").strip().lower()
            tag_value = (tag_values[i] if i < len(tag_values) else "").strip()
            if not tag_key:
                continue
            if record_type not in _NOTIFICATION_TAG_RECORD_TYPES:
                record_type = "all"
            if tag_match_operator not in _NOTIFICATION_TAG_MATCH_OPERATORS:
                tag_match_operator = "eq"
            if tag_match_operator == "regex":
                try:
                    re.compile(tag_value)
                except re.error as exc:
                    await flash(f"Invalid tag regex pattern: {exc}", "warning")
                    return redirect(
                        url_for("notifications.view_notifications", edit_rule=edit_rule_id)
                        if edit_rule_id
                        else url_for("notifications.view_notifications")
                    )
            conditions.append(
                {
                    "type": "tag",
                    "record_type": record_type,
                    "tag_key": tag_key,
                    "tag_match_operator": tag_match_operator,
                    "tag_value": tag_value,
                    "comparator": comparator,
                    "threshold": threshold,
                    "window_minutes": window_minutes,
                }
            )
            continue

        source = (sources[i] if i < len(sources) else "").strip()
        signal = (signals[i] if i < len(signals) else "").strip()
        service = (services[i] if i < len(services) else "").strip()
        if not source or not signal:
            continue
        conditions.append(
            {
                "type": "signal",
                "source": source,
                "signal": signal,
                "service": service,
                "comparator": comparator,
                "threshold": threshold,
                "window_minutes": window_minutes,
            }
        )

    if not conditions:
        await flash("At least one condition is required", "warning")
        return redirect(url_for("notifications.view_notifications"))

    # Validate channel IDs exist
    db = get_db()
    valid_channel_ids = {
        str(r["Id"])
        for r in db.execute("SELECT Id FROM sobs_notification_channels FINAL WHERE IsDeleted = 0").fetchall()
    }
    channel_ids = [c.strip() for c in channel_ids_raw if c.strip() in valid_channel_ids]

    enabled = 1
    last_fired_at = "1970-01-01 00:00:00.000"
    rule_id = str(uuid.uuid4())
    if edit_rule_id:
        existing_row = db.execute(
            "SELECT Id, Enabled, LastFiredAt FROM sobs_notification_rules FINAL "
            "WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
            [edit_rule_id],
        ).fetchone()
        if not existing_row:
            await flash("Notification rule not found for editing", "warning")
            return redirect(url_for("notifications.view_notifications"))
        rule_id = str(existing_row["Id"])
        enabled = int(existing_row["Enabled"])
        last_fired_at = str(existing_row["LastFiredAt"])

    _insert_rows_json_each_row(
        db,
        "sobs_notification_rules",
        [
            {
                "Id": rule_id,
                "Name": name,
                "Enabled": enabled,
                "LogicOperator": logic_operator,
                "ConditionsJson": json.dumps(conditions, ensure_ascii=False),
                "ChannelIds": ",".join(channel_ids),
                "Severity": severity,
                "CooldownSeconds": cooldown_seconds,
                "LastFiredAt": last_fired_at,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    await flash(
        f"Notification rule '{name}' {'updated' if edit_rule_id else 'created'}",
        "success",
    )
    return redirect(url_for("notifications.view_notifications"))


@notifications_bp.route("/settings/notifications/rules/<rule_id>/toggle", methods=["POST"])
@require_basic_auth
async def toggle_notification_rule(rule_id: str):
    """Toggle enabled/disabled state of a notification rule."""
    db = get_db()
    row = db.execute(
        "SELECT Id, Name, Enabled, LogicOperator, ConditionsJson, ChannelIds, "
        "Severity, CooldownSeconds "
        "FROM sobs_notification_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1",
        [rule_id],
    ).fetchone()
    if not row:
        await flash("Notification rule not found", "warning")
        return redirect(url_for("notifications.view_notifications"))
    new_enabled = 0 if int(row["Enabled"]) else 1
    _insert_rows_json_each_row(
        db,
        "sobs_notification_rules",
        [
            {
                "Id": rule_id,
                "Name": str(row["Name"]),
                "Enabled": new_enabled,
                "LogicOperator": str(row["LogicOperator"]),
                "ConditionsJson": str(row["ConditionsJson"]),
                "ChannelIds": str(row["ChannelIds"]),
                "Severity": str(row["Severity"]),
                "CooldownSeconds": int(row["CooldownSeconds"]),
                "LastFiredAt": "1970-01-01 00:00:00.000",
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    state = "enabled" if new_enabled else "disabled"
    await flash(f"Notification rule '{row['Name']}' {state}", "success")
    return redirect(url_for("notifications.view_notifications"))


@notifications_bp.route("/settings/notifications/rules/<rule_id>/delete", methods=["POST"])
@require_basic_auth
async def delete_notification_rule(rule_id: str):
    """Soft-delete a notification rule."""
    db = get_db()

    def _deleted_row(row: RowCompat) -> dict[str, Any]:
        return {
            "Id": rule_id,
            "Name": str(row["Name"]),
            "Enabled": int(row["Enabled"]),
            "LogicOperator": str(row["LogicOperator"]),
            "ConditionsJson": str(row["ConditionsJson"]),
            "ChannelIds": str(row["ChannelIds"]),
            "Severity": str(row["Severity"]),
            "CooldownSeconds": int(row["CooldownSeconds"]),
            "LastFiredAt": "1970-01-01 00:00:00.000",
        }

    return await _soft_delete_latest_row(
        db,
        select_sql=(
            "SELECT Id, Name, LogicOperator, ConditionsJson, ChannelIds, Severity, CooldownSeconds, Enabled "
            "FROM sobs_notification_rules FINAL WHERE Id = ? AND IsDeleted = 0 LIMIT 1"
        ),
        select_params=[rule_id],
        table_name="sobs_notification_rules",
        build_deleted_row=_deleted_row,
        not_found_message="Notification rule not found",
        success_message="Notification rule '{name}' deleted",
        redirect_endpoint="notifications.view_notifications",
    )


def _get_notification_auto_candidates(
    db: ChDbConnection,
    metric_rule_id: str | None = None,
) -> dict:
    """Return auto-generate candidates from active metric rules.

    Skips any metric rule whose (source, signal) pair is already covered by an
    existing notification rule condition.  Returns all enabled channel IDs
    pre-selected as the default target for each candidate.
    """
    if metric_rule_id:
        rows = db.execute(
            "SELECT Id, Name, SignalSource, SignalName, ServiceName, Comparator, "
            "WarningThreshold, CriticalThreshold "
            "FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 AND Id = ? LIMIT 1",
            [metric_rule_id],
        ).fetchall()
    else:
        rows = db.execute(
            "SELECT Id, Name, SignalSource, SignalName, ServiceName, Comparator, "
            "WarningThreshold, CriticalThreshold "
            "FROM sobs_anomaly_rules FINAL WHERE IsDeleted = 0 ORDER BY Name",
        ).fetchall()
    metric_rules = [
        {
            "id": str(r["Id"]),
            "name": str(r["Name"]),
            "source": str(r["SignalSource"]),
            "signal": str(r["SignalName"]),
            "service": str(r["ServiceName"]),
            "comparator": str(r["Comparator"]),
            "warning_threshold": float(r["WarningThreshold"]),
            "critical_threshold": float(r["CriticalThreshold"]),
        }
        for r in rows
    ]

    # Build set of already-covered (source, signal) keys from existing rules
    existing_rules = _load_notification_rules(db)
    covered: set[tuple[str, str]] = set()
    for nr in existing_rules:
        for cond in nr.get("conditions", []):
            covered.add((cond.get("source", ""), cond.get("signal", "")))

    # All currently enabled channels are the default selection
    channel_rows = db.execute(
        "SELECT Id, Name FROM sobs_notification_channels FINAL WHERE IsDeleted = 0 AND Enabled = 1"
    ).fetchall()
    all_channel_ids = [str(r["Id"]) for r in channel_rows]
    channel_names = {str(r["Id"]): str(r["Name"]) for r in channel_rows}

    candidates = []
    skipped = 0
    for mr in metric_rules:
        key = (mr["source"], mr["signal"])
        if key in covered:
            skipped += 1
            continue
        # Prefer critical threshold; fall back to warning
        crit = cast(float, mr["critical_threshold"])
        warn = cast(float, mr["warning_threshold"])
        if crit > 0:
            threshold = crit
            severity = "critical"
        elif warn > 0:
            threshold = warn
            severity = "warning"
        else:
            threshold = 0.0
            severity = "warning"
        candidates.append(
            {
                "metric_rule_id": mr["id"],
                "name": f"Auto: {mr['name']}",
                "source": mr["source"],
                "signal": mr["signal"],
                "service": mr["service"],
                "comparator": mr["comparator"],
                "threshold": threshold,
                "severity": severity,
                "channel_ids": all_channel_ids,
                "channel_names": [channel_names.get(cid, cid) for cid in all_channel_ids],
            }
        )
    return {
        "examined": len(metric_rules),
        "skipped": skipped,
        "candidates": candidates,
    }


@notifications_bp.route("/api/notifications/rules/auto-generate", methods=["POST"])
@require_basic_auth
async def auto_generate_notification_rules():
    """Preview or create notification rules auto-generated from active metric rules.

    POST params:
      action          - "preview" (default) or "create"
      metric_rule_id  - optional; if given, process only that one metric rule
    """
    form = await request.form
    action = (form.get("action") or "preview").strip().lower()
    metric_rule_id = (form.get("metric_rule_id") or "").strip() or None

    db = get_db()
    result = _get_notification_auto_candidates(db, metric_rule_id)
    candidates = result["candidates"]

    if action == "create":
        # Re-derive the covered set to guard against race conditions between
        # preview and create calls.
        existing_rules_now = _load_notification_rules(db)
        covered_now: set[tuple[str, str]] = set()
        for nr in existing_rules_now:
            for cond in nr.get("conditions", []):
                covered_now.add((cond.get("source", ""), cond.get("signal", "")))

        created = 0
        for cand in candidates:
            key = (cand["source"], cand["signal"])
            if key in covered_now:
                result["skipped"] = result.get("skipped", 0) + 1
                continue
            covered_now.add(key)  # prevent duplicates within this batch
            conditions = [
                {
                    "source": cand["source"],
                    "signal": cand["signal"],
                    "service": cand["service"],
                    "comparator": cand["comparator"],
                    "threshold": cand["threshold"],
                    "window_minutes": 5,
                }
            ]
            _insert_rows_json_each_row(
                db,
                "sobs_notification_rules",
                [
                    {
                        "Id": str(uuid.uuid4()),
                        "Name": cand["name"],
                        "Enabled": 1,
                        "LogicOperator": "any",
                        "ConditionsJson": json.dumps(conditions, ensure_ascii=False),
                        "ChannelIds": ",".join(cand["channel_ids"]),
                        "Severity": cand["severity"],
                        "CooldownSeconds": 300,
                        "LastFiredAt": "1970-01-01 00:00:00.000",
                        "IsDeleted": 0,
                        "Version": int(time.time() * 1000),
                    }
                ],
            )
            created += 1
        return jsonify(
            {
                "ok": True,
                "created": created,
                "skipped": result.get("skipped", 0),
                "examined": result["examined"],
            }
        )

    # action == "preview"
    return jsonify(
        {
            "ok": True,
            "examined": result["examined"],
            "skipped": result["skipped"],
            "candidates": candidates,
        }
    )


@notifications_bp.route("/api/notifications/check", methods=["POST"])
@require_basic_auth
async def check_notifications():
    """Evaluate all enabled notification rules and fire any that match.

    Designed to be called periodically (e.g., via cron or external scheduler).
    Returns a JSON summary of rule evaluations.
    """
    db = get_db()
    rules = _load_notification_rules(db)
    channels = _load_notification_channels(db)
    channels_by_id = {c["id"]: c for c in channels}

    results = []
    for rule in rules:
        try:
            result = await _check_notification_rule(db, rule, channels_by_id)
            results.append(result)
        except Exception:
            log.exception("Error evaluating notification rule %s", rule.get("id"))
            results.append({"rule_id": rule.get("id"), "fired": False, "error": "rule evaluation failed"})

    fired = [r for r in results if r.get("fired")]

    # Also evaluate automatic agent rule triggers from anomaly/tag events.
    agent_results: list[dict[str, object]] = []
    settings = _load_all_ai_settings(db)
    if settings.get("ai.endpoint_url") and settings.get("ai.model"):
        anomaly_events = _collect_anomaly_agent_events(db)
        tag_events = _collect_tag_rule_agent_events(db)
        all_anomaly_events = list(anomaly_events.values())
        all_tag_events = list(tag_events.values())

        for agent_rule in _load_agent_rules(db):
            if not agent_rule.get("is_enabled"):
                continue

            trigger_type = str(agent_rule.get("trigger_type", "")).strip().lower()
            trigger_ref_id = str(agent_rule.get("trigger_ref_id", "")).strip()
            trigger_state = str(agent_rule.get("trigger_state", "any")).strip().lower()

            event: dict[str, object] | None = None
            if trigger_type == "anomaly_rule":
                if trigger_ref_id:
                    event = anomaly_events.get(trigger_ref_id)
                elif all_anomaly_events:
                    event = max(
                        all_anomaly_events,
                        key=lambda e: 2 if str(e.get("state")) == "critical" else 1,
                    )
            elif trigger_type == "tag_rule":
                if trigger_ref_id:
                    event = tag_events.get(trigger_ref_id)
                elif all_tag_events:
                    event = all_tag_events[0]
            else:
                continue

            if not event:
                continue

            event_state = _normalize_agent_trigger_state(str(event.get("state", "normal")))
            if not _agent_rule_trigger_state_matches(trigger_state, event_state):
                continue

            rate_limit_minutes = int(agent_rule.get("rate_limit_minutes", 60) or 60)
            last_run_ts = _agent_rule_last_run_ts(db, str(agent_rule["id"]))
            elapsed_minutes = (time.time() - last_run_ts) / 60.0
            if elapsed_minutes < rate_limit_minutes and last_run_ts > 0:
                agent_results.append(
                    {
                        "rule_id": agent_rule["id"],
                        "status": "skipped_rate_limited",
                        "elapsed_minutes": round(elapsed_minutes, 2),
                    }
                )
                continue

            trigger_context = {
                "rule_name": agent_rule["name"],
                "trigger_state": event_state,
                "trigger_type": trigger_type,
                "trigger_ref_id": trigger_ref_id,
                "extra": json.dumps(event, ensure_ascii=False),
            }
            # Register a raw preservation window when an anomaly or tag event triggers an agent
            try:
                _register_raw_window(
                    db,
                    signal_ts=datetime.now(timezone.utc),
                    signal_type=trigger_type,
                    signal_ref=trigger_ref_id,
                    service_name=str(event.get("service") or ""),
                )
            except Exception:
                log.debug("failed to register raw window for agent trigger %s", trigger_ref_id, exc_info=True)
            agent_results.append(
                await _maybe_await(_run_agent_rule_instance(db, agent_rule, settings, trigger_context))
            )

    return jsonify(
        {
            "ok": True,
            "evaluated": len(results),
            "fired": len(fired),
            "results": results,
            "agent_runs": agent_results,
        }
    )


@notifications_bp.route("/api/notifications/vapid-public-key", methods=["GET"])
@require_basic_auth
async def get_vapid_public_key():
    """Return the VAPID public key for browser push subscription setup."""
    pub_key, _source = _get_vapid_public_key()
    if not pub_key:
        return jsonify({"ok": False, "error": "VAPID key not configured"}), 404
    return jsonify({"ok": True, "public_key": pub_key})


@notifications_bp.route("/service-worker.js", methods=["GET"])
async def service_worker_js():
    """Serve a minimal service worker needed for browser push notifications."""
    sw_source = """
self.addEventListener('push', function (event) {
    var data = {};
    try {
        data = event.data ? event.data.json() : {};
    } catch (_err) {
        data = { title: 'SOBS Alert', body: event.data ? event.data.text() : 'Notification received' };
    }

    var title = (data && data.title) || 'SOBS Alert';
    var options = {
        body: (data && data.body) || 'Notification received',
    };

    event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener('notificationclick', function (event) {
    event.notification.close();
    event.waitUntil(clients.openWindow(self.registration.scope));
});
""".lstrip()
    return Response(
        sw_source,
        mimetype="application/javascript",
        headers={
            "Cache-Control": "no-cache",
            "Service-Worker-Allowed": "/",
        },
    )


@notifications_bp.route("/api/notifications/subscribe", methods=["POST"])
@require_basic_auth
async def subscribe_browser_push():
    """Register a browser push subscription as a notification channel.

    Expects JSON body: {"name": "...", "endpoint": "...", "p256dh": "...", "auth": "..."}
    """
    data = await request.get_json(silent=True) or {}
    name = str(data.get("name") or "Browser Push").strip()
    endpoint = str(data.get("endpoint") or "").strip()
    p256dh = str(data.get("p256dh") or "").strip()
    auth = str(data.get("auth") or "").strip()

    if not endpoint or not p256dh or not auth:
        return jsonify({"ok": False, "error": "endpoint, p256dh, and auth are required"}), 400

    db = get_db()
    # Dedup: check if this endpoint is already registered
    existing_channels = _load_notification_channels(db)
    for ch in existing_channels:
        if ch.get("channel_type") == "browser_push" and ch.get("config", {}).get("endpoint") == endpoint:
            return jsonify({"ok": True, "channel_id": ch["id"], "existing": True})

    channel_id = str(uuid.uuid4())
    stored_config = _encrypt_notification_config({"endpoint": endpoint, "p256dh": p256dh, "auth": auth})
    _insert_rows_json_each_row(
        db,
        "sobs_notification_channels",
        [
            {
                "Id": channel_id,
                "Name": name,
                "ChannelType": "browser_push",
                "ConfigJson": json.dumps(stored_config),
                "Enabled": 1,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    return jsonify({"ok": True, "channel_id": channel_id, "existing": False})


@notifications_bp.route("/api/notifications/vapid-keygen", methods=["POST"])
@require_basic_auth
async def generate_vapid_key():
    """Generate a new VAPID key pair and save the private key to the DB.

    The env var SOBS_VAPID_PRIVATE_KEY takes precedence at dispatch time if set,
    but this endpoint always persists the new private key in sobs_app_settings so
    that self-hosted deployments work without env var management.
    """
    try:
        private_b64, public_b64 = _generate_vapid_keys()
        db = get_db()
        _set_app_setting(db, _VAPID_PRIVATE_KEY_SETTING, private_b64)
        env_override = bool(os.environ.get("SOBS_VAPID_PRIVATE_KEY", "").strip())
        return jsonify(
            {
                "ok": True,
                "public_key": public_b64,
                "saved_to_db": True,
                "env_override": env_override,
                "note": (
                    "New VAPID keys saved to the database. "
                    + (
                        "WARNING: SOBS_VAPID_PRIVATE_KEY env var is set and takes precedence \u2014 "
                        "remove it or update it to use the new DB key."
                        if env_override
                        else "Keys are active immediately. Existing browser subscriptions will need to re-subscribe."
                    )
                ),
            }
        )
    except Exception:
        log.exception("VAPID key generation failed")
        return jsonify({"ok": False, "error": "failed to generate VAPID keys"}), 500


@notifications_bp.route("/api/notifications/vapid-keys", methods=["DELETE"])
@require_basic_auth
async def delete_vapid_keys():
    """Remove the DB-stored VAPID private key.

    Does not affect SOBS_VAPID_PRIVATE_KEY if set as an env var.
    """
    db = get_db()
    _del_app_setting(db, _VAPID_PRIVATE_KEY_SETTING)
    env_override = bool(os.environ.get("SOBS_VAPID_PRIVATE_KEY", "").strip())
    return jsonify(
        {
            "ok": True,
            "env_override": env_override,
            "note": (
                "DB VAPID key cleared. "
                + (
                    "The SOBS_VAPID_PRIVATE_KEY env var is still set and will continue to be used."
                    if env_override
                    else "Browser push is now unconfigured until new keys are generated."
                )
            ),
        }
    )
