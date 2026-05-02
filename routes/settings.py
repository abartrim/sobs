"""
routes/settings.py – Settings endpoints for AI, enrichment, repositories and agents.

Blueprint: settings_bp
Prefix: none (routes keep their original /settings/* paths)

Routes moved from app.py (original lines 26420–27051):
  GET  /settings/ai
  POST /settings/ai
  GET  /settings/enrichment
  POST /settings/enrichment
  GET  /settings/repositories
  POST /settings/repositories
  POST /settings/repositories/github-token/validate
  POST /settings/repositories/<app_id>/realtime-mode
  POST /settings/repositories/<app_id>/ci-ingest-key/rotate
  POST /settings/repositories/<app_id>/ci-ingest-key/revoke
  POST /settings/repositories/<app_id>
  POST /settings/repositories/<app_id>/releases
  POST /settings/repositories/<app_id>/delete
  GET  /settings/agents
  POST /settings/agents
  POST /settings/agents/<rule_id>/delete

Helpers and constants remain in app.py (shared with other settings/notification routes).
This blueprint imports them via a deferred module-level import that is safe because
app.py registers this blueprint at the very end of its module body.
"""

from __future__ import annotations

import json
import time
import uuid
from typing import Any

from quart import Blueprint, flash, redirect, render_template, request, session, url_for

# Deferred app imports – resolved when app.py executes its final registration step.
from app import (  # noqa: E402
    _AGENT_ACTIONS,
    _AGENT_TRIGGER_STATES,
    _AGENT_TRIGGER_TYPES,
    _AI_SETTING_KEYS,
    _CI_PUSH_API_KEY_DEFAULT_TTL_DAYS,
    _CI_PUSH_API_KEY_MAX_TTL_DAYS,
    _CVE_ENABLED_SETTING,
    _CVE_LAST_SCAN_SETTING,
    _DEFAULT_AI_PRICING,
    _GEO_ENABLED_SETTING,
    _GITHUB_BACKFILL_MAX_RELEASES_DEFAULT,
    _GITHUB_BACKFILL_MAX_RELEASES_MAX,
    _GITHUB_BACKFILL_MAX_RELEASES_MIN,
    _GITHUB_BACKFILL_MAX_RELEASES_SETTING,
    _GITHUB_TOKEN_EXPIRY_WARNING_DAYS,
    RowCompat,
    _app_slug,
    _ci_push_api_key_status,
    _coerce_ai_pricing_entry,
    _find_app_by_id,
    _get_app_setting,
    _github_backfill_max_releases,
    _github_token_expiry_date_input_value,
    _github_token_expiry_status,
    _insert_rows_json_each_row,
    _load_agent_rules,
    _load_agent_runs,
    _load_ai_pricing_with_sources,
    _load_ai_setting,
    _load_all_ai_settings,
    _load_anomaly_rules,
    _load_confirmed_ai_pricing_models,
    _load_repo_scoped_github_token,
    _load_tag_rules,
    _normalize_ai_model_name,
    _normalize_github_token_expiry_input,
    _normalize_ttl_days,
    _now_iso,
    _resolve_github_repo_fields,
    _revoke_ci_push_api_key,
    _rotate_ci_push_api_key,
    _save_ai_setting,
    _save_repo_scoped_github_token,
    _serialize_app_row,
    _set_app_setting,
    _set_ci_push_realtime_enabled,
    _soft_delete_latest_row,
    _validate_github_token,
    get_db,
    jsonify,
    require_basic_auth,
)

settings_bp = Blueprint("settings", __name__)


# ---------------------------------------------------------------------------
# AI Settings  GET/POST /settings/ai
# ---------------------------------------------------------------------------
@settings_bp.route("/settings/ai", methods=["GET"])
@require_basic_auth
async def view_ai_settings():
    db = get_db()
    settings = _load_all_ai_settings(db)
    ai_pricing, ai_pricing_sources = _load_ai_pricing_with_sources(db)
    anomaly_rules = _load_anomaly_rules(db)
    tag_rules = _load_tag_rules(db)
    token_expiry_status = _github_token_expiry_status(str(settings.get("ai.github_token_expires_at", "")).strip())
    token_validation_status = {
        "status": str(settings.get("ai.github_token_last_validation_status", "")).strip(),
        "message": str(settings.get("ai.github_token_last_validation_message", "")).strip(),
        "last_validated_at": str(settings.get("ai.github_token_last_validated_at", "")).strip(),
    }
    return await render_template(
        "settings_ai.html",
        settings=settings,
        anomaly_rules=anomaly_rules,
        tag_rules=tag_rules,
        github_token_expires_date=_github_token_expiry_date_input_value(
            str(settings.get("ai.github_token_expires_at", "")).strip()
        ),
        github_token_expiry_status=token_expiry_status,
        github_token_validation_status=token_validation_status,
        default_ai_pricing=_DEFAULT_AI_PRICING,
        saved_ai_pricing=ai_pricing,
        ai_pricing_sources=ai_pricing_sources,
        confirmed_ai_pricing_models=sorted(_load_confirmed_ai_pricing_models(db)),
    )


@settings_bp.route("/settings/ai", methods=["POST"])
@require_basic_auth
async def save_ai_settings():
    form = await request.form
    db = get_db()
    previous_token = _load_ai_setting(db, "ai.github_token", "").strip()
    for key in _AI_SETTING_KEYS:
        if key in {
            "ai.guard_thinking_level",
            "ai.guard_timeout_seconds",
            "ai.github_token_expires_at",
            "ai.github_token_last_validated_at",
            "ai.github_token_last_validation_status",
            "ai.github_token_last_validation_message",
            "ai.model_pricing",  # handled separately with JSON validation below
            "ai.model_pricing_confirmed",  # handled separately with JSON validation below
        }:
            # Guard thinking is intentionally not user-configured via the Settings UI.
            continue
        # Strip key prefix for form field name: "ai.endpoint_url" → "endpoint_url"
        field = key.removeprefix("ai.")
        value = (form.get(field) or "").strip()
        _save_ai_setting(db, key, value)

    # Validate and save model pricing JSON
    raw_pricing = (form.get("model_pricing") or "").strip()
    clean: dict[str, Any] = {}
    if raw_pricing:
        try:
            parsed = json.loads(raw_pricing)
            if not isinstance(parsed, dict):
                raise ValueError("pricing must be a JSON object")
            # Re-serialize to normalize whitespace and strip invalid entries
            for model_key, prices in parsed.items():
                normalized_key = _normalize_ai_model_name(model_key)
                entry = _coerce_ai_pricing_entry(prices)
                if normalized_key and entry:
                    clean[normalized_key] = entry
            _save_ai_setting(db, "ai.model_pricing", json.dumps(clean))
        except (json.JSONDecodeError, ValueError, TypeError):
            return jsonify({"error": "Invalid model_pricing JSON"}), 400
    else:
        _save_ai_setting(db, "ai.model_pricing", "")

    raw_confirmed_models = (form.get("model_pricing_confirmed") or "").strip()
    if raw_confirmed_models:
        try:
            parsed_confirmed = json.loads(raw_confirmed_models)
            if not isinstance(parsed_confirmed, list):
                raise ValueError("confirmed list must be a JSON array")
            confirmed_models: list[str] = []
            seen_confirmed: set[str] = set()
            for model_key in parsed_confirmed:
                normalized_key = _normalize_ai_model_name(model_key)
                if not normalized_key or normalized_key in seen_confirmed:
                    continue
                if normalized_key not in clean:
                    continue
                seen_confirmed.add(normalized_key)
                confirmed_models.append(normalized_key)
            _save_ai_setting(db, "ai.model_pricing_confirmed", json.dumps(confirmed_models))
        except (json.JSONDecodeError, ValueError, TypeError):
            return jsonify({"error": "Invalid model_pricing_confirmed JSON"}), 400
    else:
        _save_ai_setting(db, "ai.model_pricing_confirmed", "")

    github_token = (form.get("github_token") or "").strip()
    github_token_expiry = _normalize_github_token_expiry_input(form.get("github_token_expires_at") or "")
    _save_ai_setting(db, "ai.github_token_expires_at", github_token_expiry if github_token else "")

    if github_token != previous_token:
        _save_ai_setting(db, "ai.github_token_last_validated_at", "")
        _save_ai_setting(db, "ai.github_token_last_validation_status", "")
        _save_ai_setting(db, "ai.github_token_last_validation_message", "")

    await flash("AI settings saved", "success")
    return redirect(url_for("settings.view_ai_settings"))


# ---------------------------------------------------------------------------
# Enrichment Settings  GET/POST /settings/enrichment
# ---------------------------------------------------------------------------
@settings_bp.route("/settings/enrichment", methods=["GET"])
@require_basic_auth
async def view_enrichment_settings():
    db = get_db()
    geo_enabled = (_get_app_setting(db, _GEO_ENABLED_SETTING) or "true").lower() in ("1", "true", "yes")
    cve_enabled = (_get_app_setting(db, _CVE_ENABLED_SETTING) or "true").lower() in ("1", "true", "yes")
    cve_last_scan = _get_app_setting(db, _CVE_LAST_SCAN_SETTING) or ""
    github_backfill_max_releases = _github_backfill_max_releases(db)
    return await render_template(
        "settings_enrichment.html",
        geo_enabled=geo_enabled,
        cve_enabled=cve_enabled,
        cve_last_scan=cve_last_scan,
        github_backfill_max_releases=github_backfill_max_releases,
        github_backfill_min_releases=_GITHUB_BACKFILL_MAX_RELEASES_MIN,
        github_backfill_max_releases_limit=_GITHUB_BACKFILL_MAX_RELEASES_MAX,
    )


@settings_bp.route("/settings/enrichment", methods=["POST"])
@require_basic_auth
async def save_enrichment_settings():
    form = await request.form
    db = get_db()
    geo_enabled = "true" if form.get("geo_enabled") else "false"
    _set_app_setting(db, _GEO_ENABLED_SETTING, geo_enabled)
    cve_enabled = "true" if form.get("cve_enabled") else "false"
    _set_app_setting(db, _CVE_ENABLED_SETTING, cve_enabled)
    try:
        github_backfill_max_releases = int(
            (form.get("github_backfill_max_releases") or str(_GITHUB_BACKFILL_MAX_RELEASES_DEFAULT)).strip()
        )
    except (TypeError, ValueError):
        github_backfill_max_releases = _GITHUB_BACKFILL_MAX_RELEASES_DEFAULT
    github_backfill_max_releases = max(
        _GITHUB_BACKFILL_MAX_RELEASES_MIN,
        min(_GITHUB_BACKFILL_MAX_RELEASES_MAX, github_backfill_max_releases),
    )
    _set_app_setting(db, _GITHUB_BACKFILL_MAX_RELEASES_SETTING, str(github_backfill_max_releases))
    await flash("Enrichment settings saved", "success")
    return redirect(url_for("settings.view_enrichment_settings"))


# ---------------------------------------------------------------------------
# GitHub Repository Management  GET/POST /settings/repositories
# ---------------------------------------------------------------------------
@settings_bp.route("/settings/repositories", methods=["GET"])
@require_basic_auth
async def view_settings_repositories():
    db = get_db()
    ai_settings = _load_all_ai_settings(db)
    ci_push_plain_by_app = session.pop("ci_push_api_key_plain_by_app", {})
    if not isinstance(ci_push_plain_by_app, dict):
        ci_push_plain_by_app = {}
    github_token_expires_at = str(ai_settings.get("ai.github_token_expires_at", "")).strip()
    github_token_expiry_status = _github_token_expiry_status(github_token_expires_at)
    github_token_validation_status = {
        "status": str(ai_settings.get("ai.github_token_last_validation_status", "")).strip(),
        "message": str(ai_settings.get("ai.github_token_last_validation_message", "")).strip(),
        "last_validated_at": str(ai_settings.get("ai.github_token_last_validated_at", "")).strip(),
    }
    app_rows = [
        dict(r) for r in db.execute("SELECT * FROM sobs_apps FINAL WHERE IsDeleted=0 ORDER BY Name ASC").fetchall()
    ]
    release_rows = db.execute(
        "SELECT AppId, ReleaseVersion, ReleasedAt "
        "FROM sobs_app_releases FINAL "
        "WHERE IsDeleted=0 "
        "ORDER BY ReleasedAt DESC LIMIT 5000"
    ).fetchall()

    releases_by_app: dict[str, list[str]] = {}
    for row in release_rows:
        app_id = str(row["AppId"])
        version = str(row["ReleaseVersion"] or "").strip()
        if not app_id or not version:
            continue
        versions = releases_by_app.setdefault(app_id, [])
        if version not in versions:
            versions.append(version)

    apps = []
    for row in app_rows:
        app = _serialize_app_row(row)
        app_versions = releases_by_app.get(app["id"], [])
        _, owner, repo = _resolve_github_repo_fields(app["repoUrl"])
        repo_token_configured = bool(_load_repo_scoped_github_token(db, owner, repo)) if owner and repo else False
        ci_push_status = _ci_push_api_key_status(db, app["id"])
        ci_push_plain = str(ci_push_plain_by_app.get(app["id"], "") or "")
        apps.append(
            {
                "id": app["id"],
                "name": app["name"],
                "slug": app["slug"],
                "repo_url": app["repoUrl"],
                "repo_owner": owner,
                "repo_name": repo,
                "enabled": app["enabled"],
                "release_count": len(app_versions),
                "latest_versions": app_versions[:5],
                "repo_token_configured": repo_token_configured,
                "ci_push_status": ci_push_status,
                "ci_push_plain": ci_push_plain,
            }
        )

    realtime_seed = {
        "enabled": any(bool((item.get("ci_push_status") or {}).get("realtime_enabled")) for item in apps),
        "configured": any(bool((item.get("ci_push_status") or {}).get("configured")) for item in apps),
        "expires_at": "",
        "expiry_message": "Per-repository CI ingest keys are managed from each repository row.",
        "api_key": "",
        "api_key_show_once": False,
    }

    return await render_template(
        "settings_repositories.html",
        apps=apps,
        github_token_configured=bool(str(ai_settings.get("ai.github_token", "")).strip()),
        default_agent_repo=str(ai_settings.get("ai.github_repo", "")).strip(),
        github_token_expires_date=_github_token_expiry_date_input_value(github_token_expires_at),
        github_token_expiry_status=github_token_expiry_status,
        github_token_validation_status=github_token_validation_status,
        github_token_expiry_warning_days=_GITHUB_TOKEN_EXPIRY_WARNING_DAYS,
        realtime_seed=realtime_seed,
        ci_push_default_ttl_days=_CI_PUSH_API_KEY_DEFAULT_TTL_DAYS,
        ci_push_max_ttl_days=_CI_PUSH_API_KEY_MAX_TTL_DAYS,
    )


@settings_bp.route("/settings/repositories", methods=["POST"])
@require_basic_auth
async def create_settings_repository():
    db = get_db()
    form = await request.form
    name = str(form.get("name", "")).strip()
    slug_raw = str(form.get("slug", "")).strip()
    repo_url_input = str(form.get("repo_url", "")).strip()
    repo_owner_input = str(form.get("repo_owner", "")).strip()
    repo_name_input = str(form.get("repo_name", "")).strip()
    repo_url, owner, repo = _resolve_github_repo_fields(repo_url_input, repo_owner_input, repo_name_input)
    default_environment = str(form.get("default_environment", "")).strip()
    github_token = str(form.get("github_token", "")).strip()
    github_token_expiry = _normalize_github_token_expiry_input(form.get("github_token_expires_at") or "")
    set_github_token = bool(form.get("set_github_token"))
    set_repo_token = bool(form.get("set_repo_token"))
    set_agent_repo = bool(form.get("set_agent_repo"))

    if not name or not repo_url:
        await flash("App name and repository are required", "warning")
        return redirect(url_for("settings.view_settings_repositories"))

    slug = _app_slug(slug_raw or name)
    existing = db.execute(
        "SELECT Id FROM sobs_apps FINAL WHERE Slug=? AND IsDeleted=0 LIMIT 1",
        [slug],
    ).fetchone()
    if existing:
        await flash("App slug already exists", "warning")
        return redirect(url_for("settings.view_settings_repositories"))

    version = int(time.time() * 1000)
    row = {
        "Id": uuid.uuid4().hex,
        "Name": name,
        "Slug": slug,
        "OwnerTeam": "",
        "RepoUrl": repo_url,
        "DefaultEnvironment": default_environment,
        "Enabled": 1,
        "MetadataJson": "{}",
        "IsDeleted": 0,
        "Version": version,
        "CreatedAt": _now_iso(),
        "UpdatedAt": _now_iso(),
    }
    _insert_rows_json_each_row(db, "sobs_apps", [row])

    if set_github_token and github_token:
        _save_ai_setting(db, "ai.github_token", github_token)
        _save_ai_setting(db, "ai.github_token_expires_at", github_token_expiry)
        _save_ai_setting(db, "ai.github_token_last_validated_at", "")
        _save_ai_setting(db, "ai.github_token_last_validation_status", "")
        _save_ai_setting(db, "ai.github_token_last_validation_message", "")

    if set_repo_token and github_token:
        if owner and repo:
            _save_repo_scoped_github_token(db, owner, repo, github_token)

    if set_agent_repo:
        if owner and repo:
            _save_ai_setting(db, "ai.github_repo", f"{owner}/{repo}")

    await flash("Repository added", "success")
    return redirect(url_for("settings.view_settings_repositories"))


@settings_bp.route("/settings/repositories/github-token/validate", methods=["POST"])
@require_basic_auth
async def validate_settings_repository_github_token():
    db = get_db()
    github_token = _load_ai_setting(db, "ai.github_token", "").strip()
    if not github_token:
        await flash("No GitHub token configured to validate", "warning")
        return redirect(url_for("settings.view_settings_repositories"))

    status, message = await _validate_github_token(github_token)
    _save_ai_setting(db, "ai.github_token_last_validated_at", _now_iso())
    _save_ai_setting(db, "ai.github_token_last_validation_status", status)
    _save_ai_setting(db, "ai.github_token_last_validation_message", message)

    category = "success" if status == "valid" else "warning"
    await flash(f"GitHub token validation: {message}", category)
    return redirect(url_for("settings.view_settings_repositories"))


@settings_bp.route("/settings/repositories/<app_id>/realtime-mode", methods=["POST"])
@require_basic_auth
async def save_settings_repository_realtime_mode(app_id: str):
    db = get_db()
    current = _find_app_by_id(db, app_id)
    if not current:
        await flash("Repository entry not found", "warning")
        return redirect(url_for("settings.view_settings_repositories"))
    form = await request.form
    enabled = bool(form.get("realtime_enabled"))
    _set_ci_push_realtime_enabled(db, app_id, enabled)
    app_name = str(current.get("Name", "repository")).strip()
    await flash(
        f"Realtime CI support {'enabled' if enabled else 'disabled'} for {app_name}",
        "success",
    )
    return redirect(url_for("settings.view_settings_repositories"))


@settings_bp.route("/settings/repositories/<app_id>/ci-ingest-key/rotate", methods=["POST"])
@require_basic_auth
async def rotate_settings_repository_ci_ingest_key(app_id: str):
    db = get_db()
    current = _find_app_by_id(db, app_id)
    if not current:
        await flash("Repository entry not found", "warning")
        return redirect(url_for("settings.view_settings_repositories"))
    form = await request.form
    ttl_days = _normalize_ttl_days(form.get("ttl_days"), _CI_PUSH_API_KEY_DEFAULT_TTL_DAYS)
    key_plain, expires_at = _rotate_ci_push_api_key(db, app_id, ttl_days)
    if not key_plain:
        await flash("Failed to rotate CI ingest API key", "warning")
        return redirect(url_for("settings.view_settings_repositories"))
    _set_ci_push_realtime_enabled(db, app_id, True)
    plain_by_app = session.get("ci_push_api_key_plain_by_app")
    if not isinstance(plain_by_app, dict):
        plain_by_app = {}
    plain_by_app[app_id] = key_plain
    session["ci_push_api_key_plain_by_app"] = plain_by_app
    await flash(
        f"CI ingest API key rotated for {str(current.get('Name', 'repository')).strip()} (expires {expires_at[:10]}). "
        "Copy the key now; it is shown once.",
        "success",
    )
    return redirect(url_for("settings.view_settings_repositories"))


@settings_bp.route("/settings/repositories/<app_id>/ci-ingest-key/revoke", methods=["POST"])
@require_basic_auth
async def revoke_settings_repository_ci_ingest_key(app_id: str):
    db = get_db()
    current = _find_app_by_id(db, app_id)
    if not current:
        await flash("Repository entry not found", "warning")
        return redirect(url_for("settings.view_settings_repositories"))
    _revoke_ci_push_api_key(db, app_id)
    await flash(f"CI ingest API key revoked for {str(current.get('Name', 'repository')).strip()}", "success")
    return redirect(url_for("settings.view_settings_repositories"))


@settings_bp.route("/settings/repositories/<app_id>", methods=["POST"])
@require_basic_auth
async def update_settings_repository(app_id: str):
    db = get_db()
    form = await request.form
    repo_url_input = str(form.get("repo_url", "")).strip()
    repo_owner_input = str(form.get("repo_owner", "")).strip()
    repo_name_input = str(form.get("repo_name", "")).strip()
    repo_token = str(form.get("repo_token", "")).strip()
    set_repo_token = bool(form.get("set_repo_token"))

    current = _find_app_by_id(db, app_id)
    if not current:
        await flash("Repository entry not found", "warning")
        return redirect(url_for("settings.view_settings_repositories"))

    repo_url, owner, repo = _resolve_github_repo_fields(repo_url_input, repo_owner_input, repo_name_input)

    if not repo_url:
        await flash("Repository is required", "warning")
        return redirect(url_for("settings.view_settings_repositories"))

    version = int(time.time() * 1000)
    row = {
        "Id": app_id,
        "Name": str(current.get("Name", "")),
        "Slug": str(current.get("Slug", "")),
        "OwnerTeam": str(current.get("OwnerTeam", "")),
        "RepoUrl": repo_url,
        "DefaultEnvironment": str(current.get("DefaultEnvironment", "")),
        "Enabled": int(current.get("Enabled", 1) or 0),
        "MetadataJson": str(current.get("MetadataJson", "{}") or "{}"),
        "IsDeleted": 0,
        "Version": version,
        "CreatedAt": str(current.get("CreatedAt", "")) or _now_iso(),
        "UpdatedAt": _now_iso(),
    }
    _insert_rows_json_each_row(db, "sobs_apps", [row])

    if set_repo_token and repo_token:
        if owner and repo:
            _save_repo_scoped_github_token(db, owner, repo, repo_token)

    await flash("Repository updated", "success")
    return redirect(url_for("settings.view_settings_repositories"))


@settings_bp.route("/settings/repositories/<app_id>/releases", methods=["POST"])
@require_basic_auth
async def add_settings_repository_release(app_id: str):
    db = get_db()
    form = await request.form
    release_version = str(form.get("version", "")).strip()
    environment = str(form.get("environment", "")).strip()

    app_row = _find_app_by_id(db, app_id)
    if not app_row:
        await flash("Repository entry not found", "warning")
        return redirect(url_for("settings.view_settings_repositories"))

    if not release_version:
        await flash("Release version is required", "warning")
        return redirect(url_for("settings.view_settings_repositories"))

    version = int(time.time() * 1000)
    row = {
        "Id": uuid.uuid4().hex,
        "AppId": app_id,
        "ReleaseVersion": release_version,
        "CommitSha": "",
        "BuildId": "",
        "Environment": environment,
        "ReleasedAt": _now_iso(),
        "MetadataJson": "{}",
        "IsDeleted": 0,
        "Version": version,
    }
    _insert_rows_json_each_row(db, "sobs_app_releases", [row])
    await flash("Release added", "success")
    return redirect(url_for("settings.view_settings_repositories"))


@settings_bp.route("/settings/repositories/<app_id>/delete", methods=["POST"])
@require_basic_auth
async def delete_settings_repository(app_id: str):
    db = get_db()
    current = _find_app_by_id(db, app_id)
    if not current:
        await flash("Repository entry not found", "warning")
        return redirect(url_for("settings.view_settings_repositories"))

    version = int(time.time() * 1000)
    now_iso = _now_iso()
    row = {
        "Id": app_id,
        "Name": str(current.get("Name", "")),
        "Slug": str(current.get("Slug", "")),
        "OwnerTeam": str(current.get("OwnerTeam", "")),
        "RepoUrl": str(current.get("RepoUrl", "")),
        "DefaultEnvironment": str(current.get("DefaultEnvironment", "")),
        "Enabled": int(current.get("Enabled", 1) or 0),
        "MetadataJson": str(current.get("MetadataJson", "{}") or "{}"),
        "IsDeleted": 1,
        "Version": version,
        "CreatedAt": str(current.get("CreatedAt", "")) or now_iso,
        "UpdatedAt": now_iso,
    }
    _insert_rows_json_each_row(db, "sobs_apps", [row])

    release_rows = db.execute(
        "SELECT * FROM sobs_app_releases FINAL WHERE AppId=? AND IsDeleted=0",
        [app_id],
    ).fetchall()
    if release_rows:
        release_tombstones: list[dict[str, Any]] = []
        for release_row in release_rows:
            release_tombstones.append(
                {
                    "Id": str(release_row["Id"]),
                    "AppId": str(release_row["AppId"]),
                    "ReleaseVersion": str(release_row["ReleaseVersion"]),
                    "CommitSha": str(release_row["CommitSha"]),
                    "BuildId": str(release_row["BuildId"]),
                    "Environment": str(release_row["Environment"]),
                    "ReleasedAt": str(release_row["ReleasedAt"]),
                    "MetadataJson": str(release_row["MetadataJson"]),
                    "IsDeleted": 1,
                    "Version": version,
                }
            )
        _insert_rows_json_each_row(db, "sobs_app_releases", release_tombstones)

    await flash(f"Repository '{str(current.get('Name', ''))}' deleted", "success")
    return redirect(url_for("settings.view_settings_repositories"))


# ---------------------------------------------------------------------------
# Agent Rules  GET/POST /settings/agents
# ---------------------------------------------------------------------------
@settings_bp.route("/settings/agents", methods=["GET"])
@require_basic_auth
async def view_agent_rules():
    db = get_db()
    rules = _load_agent_rules(db)
    runs = _load_agent_runs(db, limit=20)
    anomaly_rules = _load_anomaly_rules(db)
    tag_rules = _load_tag_rules(db)
    return await render_template(
        "settings_agents.html",
        rules=rules,
        runs=runs,
        anomaly_rules=anomaly_rules,
        tag_rules=tag_rules,
        trigger_types=_AGENT_TRIGGER_TYPES,
        trigger_states=_AGENT_TRIGGER_STATES,
        agent_actions=_AGENT_ACTIONS,
    )


@settings_bp.route("/settings/agents", methods=["POST"])
@require_basic_auth
async def create_agent_rule():
    form = await request.form
    name = (form.get("name") or "").strip()
    description = (form.get("description") or "").strip()
    trigger_type = (form.get("trigger_type") or "manual").strip().lower()
    trigger_ref_id = (form.get("trigger_ref_id") or "").strip()
    trigger_state = (form.get("trigger_state") or "any").strip().lower()
    actions_list = form.getlist("actions")
    try:
        rate_limit = max(1, min(10080, int(form.get("rate_limit_minutes") or 60)))
    except (TypeError, ValueError):
        rate_limit = 60

    if not name:
        await flash("Rule name is required", "warning")
        return redirect(url_for("settings.view_agent_rules"))
    if trigger_type not in _AGENT_TRIGGER_TYPES:
        await flash(f"Invalid trigger type: {trigger_type}", "warning")
        return redirect(url_for("settings.view_agent_rules"))
    if trigger_state not in _AGENT_TRIGGER_STATES:
        await flash(f"Invalid trigger state: {trigger_state}", "warning")
        return redirect(url_for("settings.view_agent_rules"))

    valid_actions = [a for a in actions_list if a in _AGENT_ACTIONS]
    if not valid_actions:
        valid_actions = ["analyze"]

    rule_id = str(uuid.uuid4())
    _insert_rows_json_each_row(
        get_db(),
        "sobs_agent_rules",
        [
            {
                "Id": rule_id,
                "Name": name,
                "Description": description,
                "TriggerType": trigger_type,
                "TriggerRefId": trigger_ref_id,
                "TriggerState": trigger_state,
                "Actions": ",".join(valid_actions),
                "RateLimitMinutes": rate_limit,
                "IsEnabled": 1,
                "IsDeleted": 0,
                "Version": int(time.time() * 1000),
            }
        ],
    )
    await flash(f"Agent rule '{name}' created", "success")
    return redirect(url_for("settings.view_agent_rules"))


@settings_bp.route("/settings/agents/<rule_id>/delete", methods=["POST"])
@require_basic_auth
async def delete_agent_rule(rule_id: str):
    db = get_db()

    def _deleted_row(row: RowCompat) -> dict[str, Any]:
        return {
            "Id": rule_id,
            "Name": str(row["Name"]),
            "Description": "",
            "TriggerType": "manual",
            "TriggerRefId": "",
            "TriggerState": "any",
            "Actions": "analyze",
            "RateLimitMinutes": 60,
            "IsEnabled": 0,
        }

    return await _soft_delete_latest_row(
        db,
        select_sql="SELECT Id, Name FROM sobs_agent_rules FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1",
        select_params=[rule_id],
        table_name="sobs_agent_rules",
        build_deleted_row=_deleted_row,
        not_found_message="Agent rule not found",
        success_message="Agent rule '{name}' deleted",
        redirect_endpoint="settings.view_agent_rules",
    )
