"""Kubernetes and data management settings routes blueprint."""

from __future__ import annotations

from typing import Any

from quart import Blueprint, jsonify, redirect, render_template, request, url_for

from app import (  # noqa: E402
    _acquire_dm_prune_lock,
    _apply_dm_ttl,
    _del_app_setting,
    _dm_backup_enabled,
    _dm_settings_from_form,
    _fetch_k8s_from_otel,
    _fmt_bytes,
    _get_app_setting_raw,
    _get_db_stats,
    _is_sensitive_dm_setting_key,
    _k8s_settings_from_form,
    _kubernetes_enabled,
    _list_dm_backups,
    _load_dm_settings,
    _load_k8s_settings,
    _parse_dm_prune_period,
    _run_dm_backup,
    _run_dm_prune,
    _run_dm_restore,
    _set_app_setting,
    _set_dm_setting,
    _validate_dm_backup_name,
    get_db,
    require_basic_auth,
)

k8s_dm_bp = Blueprint("k8s_dm", __name__)


@k8s_dm_bp.route("/settings/kubernetes", methods=["GET"])
@require_basic_auth
async def view_k8s_settings():
    """Kubernetes health view settings page."""
    db = get_db()
    settings = _load_k8s_settings(db)
    flash_msg = request.args.get("msg", "")
    flash_type = request.args.get("msg_type", "success")
    return await render_template(
        "settings_kubernetes.html",
        k8s_settings=settings,
        flash_msg=flash_msg,
        flash_type=flash_type,
    )


@k8s_dm_bp.route("/settings/kubernetes", methods=["POST"])
@require_basic_auth
async def save_k8s_settings():
    """Save Kubernetes health view settings."""
    form = await request.form
    new_settings = _k8s_settings_from_form(dict(form))
    db = get_db()
    for key, value in new_settings.items():
        if value:
            _set_app_setting(db, key, value)
        else:
            _del_app_setting(db, key)
    redirect_url = url_for("k8s_dm.view_k8s_settings") + "?msg=Settings+saved&msg_type=success"
    return redirect(redirect_url)


@k8s_dm_bp.route("/kubernetes")
@require_basic_auth
async def view_kubernetes():
    """Kubernetes health dashboard page."""
    if not _kubernetes_enabled():
        return (
            "Kubernetes health view is disabled. Enable it in Settings → Kubernetes.",
            404,
        )
    return await render_template("kubernetes.html")


@k8s_dm_bp.route("/api/kubernetes/status", methods=["GET"])
@require_basic_auth
async def api_kubernetes_status():
    """Return current Kubernetes health data from OTEL tables."""
    if not _kubernetes_enabled():
        return jsonify({"ok": False, "error": "Kubernetes health view is disabled."}), 404

    def _q_int(name: str, default: int, lo: int, hi: int) -> int:
        raw = request.args.get(name, str(default)).strip()
        try:
            parsed = int(raw)
        except Exception:
            parsed = default
        return max(lo, min(hi, parsed))

    query_opts: dict[str, Any] = {
        "namespace": request.args.get("namespace", "").strip(),
        "namespace_values": [v.strip() for v in request.args.getlist("namespace") if v.strip()],
        "node_values": [v.strip() for v in request.args.getlist("node") if v.strip()],
        "deployment_values": [v.strip() for v in request.args.getlist("deployment") if v.strip()],
        "pod_values": [v.strip() for v in request.args.getlist("pod") if v.strip()],
        "name": request.args.get("name", "").strip(),
        "nodes_sort": request.args.get("nodes_sort", "name").strip(),
        "nodes_dir": request.args.get("nodes_dir", "asc").strip().lower(),
        "nodes_page": _q_int("nodes_page", 1, 1, 1_000_000),
        "nodes_page_size": _q_int("nodes_page_size", 25, 1, 200),
        "deployments_sort": request.args.get("deployments_sort", "namespace").strip(),
        "deployments_dir": request.args.get("deployments_dir", "asc").strip().lower(),
        "deployments_page": _q_int("deployments_page", 1, 1, 1_000_000),
        "deployments_page_size": _q_int("deployments_page_size", 25, 1, 200),
        "pods_sort": request.args.get("pods_sort", "namespace").strip(),
        "pods_dir": request.args.get("pods_dir", "asc").strip().lower(),
        "pods_page": _q_int("pods_page", 1, 1, 1_000_000),
        "pods_page_size": _q_int("pods_page_size", 25, 1, 200),
    }

    db = get_db()
    data = _fetch_k8s_from_otel(db, query_opts)
    data["ok"] = True
    return jsonify(data)


@k8s_dm_bp.route("/settings/data-management", methods=["GET"])
@require_basic_auth
async def view_dm_settings():
    """Data management settings page (TTL, backup, restore)."""
    db = get_db()
    settings = _load_dm_settings(db, include_sensitive_values=False)
    dm_secret_present = {
        "s3_secret_access_key": bool(_get_app_setting_raw(db, "data_management.s3_secret_access_key")),
        "backup_encryption_password": bool(_get_app_setting_raw(db, "data_management.backup_encryption_password")),
    }
    flash_msg = request.args.get("msg", "")
    flash_type = request.args.get("msg_type", "success")
    db_stats = _get_db_stats(db)
    return await render_template(
        "settings_data_management.html",
        dm_settings=settings,
        dm_secret_present=dm_secret_present,
        flash_msg=flash_msg,
        flash_type=flash_type,
        db_stats=db_stats,
        fmt_bytes=_fmt_bytes,
    )


@k8s_dm_bp.route("/settings/data-management", methods=["POST"])
@require_basic_auth
async def save_dm_settings():
    """Save data management settings and optionally apply TTL to tables."""
    form = await request.form
    new_settings = _dm_settings_from_form(dict(form))
    db = get_db()

    clear_sensitive_keys: set[str] = set()
    if form.get("clear_s3_secret_access_key") == "1":
        clear_sensitive_keys.add("data_management.s3_secret_access_key")
    if form.get("clear_backup_encryption_password") == "1":
        clear_sensitive_keys.add("data_management.backup_encryption_password")

    for key, value in new_settings.items():
        if key in clear_sensitive_keys:
            _del_app_setting(db, key)
            continue
        if _is_sensitive_dm_setting_key(key) and not value:
            # Preserve existing sensitive values when form fields are intentionally left blank.
            continue
        if value:
            _set_dm_setting(db, key, value)
        else:
            _del_app_setting(db, key)

    # Apply TTL immediately if the form requested it
    if form.get("apply_ttl") == "1":
        errors = _apply_dm_ttl(db, new_settings)
        if errors:
            msg = "Settings saved but TTL errors: " + "; ".join(errors[:3])
            redirect_url = url_for("k8s_dm.view_dm_settings") + f"?msg={msg}&msg_type=warning"
            return redirect(redirect_url)

    redirect_url = url_for("k8s_dm.view_dm_settings") + "?msg=Settings+saved&msg_type=success"
    return redirect(redirect_url)


@k8s_dm_bp.route("/api/data-management/backup/list", methods=["GET"])
@require_basic_auth
async def api_dm_backup_list():
    """Return the list of available backups from system.backups."""
    db = get_db()
    settings = _load_dm_settings(db)
    backups = _list_dm_backups(db, settings)
    return jsonify({"ok": True, "backups": backups})


@k8s_dm_bp.route("/api/data-management/backup/run", methods=["POST"])
@require_basic_auth
async def api_dm_backup_run():
    """Trigger a ClickHouse BACKUP ALL to the configured S3 destination."""
    if not _dm_backup_enabled():
        return jsonify({"ok": False, "message": "Backup feature is disabled"}), 403
    data = await request.get_json(silent=True) or {}
    backup_type = str(data.get("type", "full")).lower()
    if backup_type not in ("full", "incremental"):
        backup_type = "full"
    db = get_db()
    settings = _load_dm_settings(db)
    result = _run_dm_backup(db, settings, backup_type)
    return jsonify({"ok": result["ok"] == "true", "message": result["message"]})


@k8s_dm_bp.route("/api/data-management/restore", methods=["POST"])
@require_basic_auth
async def api_dm_restore():
    """Restore from a named backup on the configured S3 destination."""
    if not _dm_backup_enabled():
        return jsonify({"ok": False, "message": "Backup feature is disabled"}), 403
    data = await request.get_json(silent=True) or {}
    backup_name = str(data.get("backup_name", "")).strip()
    if backup_name:
        try:
            _validate_dm_backup_name(backup_name)
        except ValueError as exc:
            return jsonify({"ok": False, "message": str(exc)}), 400
    db = get_db()
    settings = _load_dm_settings(db)
    result = _run_dm_restore(db, settings, backup_name)
    return jsonify({"ok": result["ok"] == "true", "message": result["message"]})


@k8s_dm_bp.route("/api/data-management/prune", methods=["POST"])
@require_basic_auth
async def api_dm_prune():
    """Trigger an immediate prune of all TTL-managed tables via OPTIMIZE TABLE … FINAL."""
    payload = await request.get_json(silent=True)
    raw_body = (await request.get_data()).strip()
    if payload is None:
        if raw_body and request.is_json:
            return jsonify({"ok": False, "message": "request body contains invalid JSON"}), 400
        payload = {}
    if not isinstance(payload, dict):
        return jsonify({"ok": False, "message": "request body must be a JSON object"}), 400
    try:
        prune_period = _parse_dm_prune_period(payload)
    except ValueError as exc:
        return jsonify({"ok": False, "message": str(exc)}), 400

    prune_lock = _acquire_dm_prune_lock()
    if prune_lock is None:
        return jsonify({"ok": False, "message": "A prune operation is already in progress"}), 409
    try:
        db = get_db()
        result = _run_dm_prune(db, prune_period=prune_period)
        return jsonify(result)
    finally:
        prune_lock.release()
