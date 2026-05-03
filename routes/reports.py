"""Reports routes blueprint."""

from __future__ import annotations

import json
import time
import uuid
from datetime import datetime, timezone
from typing import Any

from quart import Blueprint, Response, flash, jsonify, redirect, render_template, request, url_for

from app import (  # noqa: E402
    ChDbConnection,
    _insert_rows_json_each_row,
    get_db,
    require_basic_auth,
)

reports_bp = Blueprint("reports", __name__)

_REPORT_PAGE_TYPES = {"logs", "traces", "errors", "metrics", "rum", "ai", "work_items", "web_traffic"}


def _parse_report_filters(raw_filters_json: Any) -> dict[str, Any]:
    if not raw_filters_json:
        return {}
    try:
        parsed = json.loads(str(raw_filters_json))
    except (TypeError, ValueError):
        return {}
    return parsed if isinstance(parsed, dict) else {}


def _get_reports(db: ChDbConnection, page_type: str | None = None) -> list[dict]:
    if page_type:
        rows = db.execute(
            "SELECT Id, Name, Description, PageType, FiltersJson "
            "FROM sobs_reports FINAL WHERE IsDeleted = 0 AND PageType = ? ORDER BY Name",
            [page_type],
        ).fetchall()
    else:
        rows = db.execute(
            "SELECT Id, Name, Description, PageType, FiltersJson "
            "FROM sobs_reports FINAL WHERE IsDeleted = 0 ORDER BY PageType, Name"
        ).fetchall()
    return [
        {
            "id": str(r["Id"]),
            "name": str(r["Name"]),
            "description": str(r["Description"]),
            "page_type": str(r["PageType"]),
            "filters": _parse_report_filters(r["FiltersJson"]),
        }
        for r in rows
    ]


def _get_report(db: ChDbConnection, report_id: str) -> dict | None:
    row = db.execute(
        "SELECT Id, Name, Description, PageType, FiltersJson " "FROM sobs_reports FINAL WHERE IsDeleted = 0 AND Id = ?",
        [report_id],
    ).fetchone()
    if not row:
        return None
    return {
        "id": str(row["Id"]),
        "name": str(row["Name"]),
        "description": str(row["Description"]),
        "page_type": str(row["PageType"]),
        "filters": _parse_report_filters(row["FiltersJson"]),
    }


@reports_bp.route("/reports")
@require_basic_auth
async def list_reports():
    db = get_db()
    reports = _get_reports(db)
    return await render_template("reports.html", reports=reports)


@reports_bp.route("/reports/<report_id>/delete", methods=["POST"])
@require_basic_auth
async def delete_report(report_id: str):
    db = get_db()
    report = _get_report(db, report_id)
    if not report:
        await flash("Report not found", "danger")
        return redirect(url_for("reports.list_reports"))
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_reports",
        [
            {
                "Id": report_id,
                "Name": report["name"],
                "Description": report["description"],
                "PageType": report["page_type"],
                "FiltersJson": json.dumps(report["filters"], ensure_ascii=False),
                "IsDeleted": 1,
                "Version": version,
            }
        ],
    )
    await flash(f"Report '{report['name']}' deleted", "success")
    return redirect(url_for("reports.list_reports"))


@reports_bp.route("/api/reports", methods=["GET"])
@require_basic_auth
async def api_list_reports():
    page_type = request.args.get("page_type", "").strip()
    db = get_db()
    reports = _get_reports(db, page_type if page_type else None)
    return jsonify(reports)


@reports_bp.route("/api/reports", methods=["POST"])
@require_basic_auth
async def api_create_report():
    body = await request.get_json(silent=True) or {}
    name = (body.get("name") or "").strip()
    description = (body.get("description") or "").strip()
    page_type = (body.get("page_type") or "").strip()
    filters = body.get("filters") or {}

    if not name:
        return jsonify({"error": "name is required"}), 400
    if page_type not in _REPORT_PAGE_TYPES:
        return jsonify({"error": f"page_type must be one of: {', '.join(sorted(_REPORT_PAGE_TYPES))}"}), 400
    if not isinstance(filters, dict):
        return jsonify({"error": "filters must be an object"}), 400

    report_id = str(uuid.uuid4())
    version = int(time.time() * 1000)
    db = get_db()
    _insert_rows_json_each_row(
        db,
        "sobs_reports",
        [
            {
                "Id": report_id,
                "Name": name,
                "Description": description,
                "PageType": page_type,
                "FiltersJson": json.dumps(filters, ensure_ascii=False),
                "IsDeleted": 0,
                "Version": version,
            }
        ],
    )
    result = {"id": report_id, "name": name, "description": description, "page_type": page_type, "filters": filters}
    return jsonify(result), 201


@reports_bp.route("/api/reports/<report_id>", methods=["DELETE"])
@require_basic_auth
async def api_delete_report(report_id: str):
    db = get_db()
    report = _get_report(db, report_id)
    if not report:
        return jsonify({"error": "not found"}), 404
    version = int(time.time() * 1000)
    _insert_rows_json_each_row(
        db,
        "sobs_reports",
        [
            {
                "Id": report_id,
                "Name": report["name"],
                "Description": report["description"],
                "PageType": report["page_type"],
                "FiltersJson": json.dumps(report["filters"], ensure_ascii=False),
                "IsDeleted": 1,
                "Version": version,
            }
        ],
    )
    return jsonify({"deleted": True})


_REPORTS_EXPORT_VERSION = "1"


_REPORTS_IMPORT_MAX = 500


_REPORTS_IMPORT_MAX_BYTES = 5 * 1024 * 1024


@reports_bp.route("/api/reports/export", methods=["GET"])
@require_basic_auth
async def api_export_reports():
    """Export one or more saved reports as a portable JSON payload.

    Query parameters:
      ids – comma-separated list of report UUIDs.  Omit to export all reports.

    Returns a ``Content-Disposition: attachment`` JSON file so the browser
    triggers a download automatically.
    """
    db = get_db()
    raw_ids = request.args.get("ids", "").strip()
    if raw_ids:
        wanted = {s.strip() for s in raw_ids.split(",") if s.strip()}
        all_reports = _get_reports(db)
        reports = [r for r in all_reports if r["id"] in wanted]
    else:
        reports = _get_reports(db)

    payload = {
        "sobs_reports_export": True,
        "version": _REPORTS_EXPORT_VERSION,
        "exported_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "reports": [
            {
                "id": r["id"],
                "name": r["name"],
                "description": r["description"],
                "page_type": r["page_type"],
                "filters": r["filters"],
            }
            for r in reports
        ],
    }
    json_bytes = json.dumps(payload, indent=2, ensure_ascii=False).encode("utf-8")
    filename = "sobs_reports_export.json"
    return Response(
        json_bytes,
        status=200,
        headers={
            "Content-Type": "application/json; charset=utf-8",
            "Content-Disposition": f'attachment; filename="{filename}"',
        },
    )


@reports_bp.route("/api/reports/import", methods=["POST"])
@require_basic_auth
async def api_import_reports():
    """Import reports from a previously exported JSON payload.

    Accepts either ``application/json`` body or a ``multipart/form-data``
    upload with a ``file`` field containing the JSON file.

    Body / file must be the JSON object produced by ``/api/reports/export``.

    Optional query / body parameter ``on_conflict`` controls duplicate
    handling when a report with the same name already exists for the same
    page_type:
      - ``rename``  (default) – append " (imported)" / " (2)" etc. to the name.
      - ``replace`` – delete the existing report and insert the new one.
      - ``skip``    – leave the existing report unchanged and skip the new one.

    Returns a JSON summary: ``{imported, skipped, replaced, errors}``.
    """

    def _payload_too_large_response():
        return jsonify({"error": f"Import payload too large (max {_REPORTS_IMPORT_MAX_BYTES} bytes)"}), 413

    if (request.content_length or 0) > _REPORTS_IMPORT_MAX_BYTES:
        return _payload_too_large_response()

    # ── Parse body ────────────────────────────────────────────────────────────
    content_type = request.content_type or ""
    on_conflict = (request.args.get("on_conflict") or "").strip().lower()

    if "multipart/form-data" in content_type or "application/x-www-form-urlencoded" in content_type:
        files = await request.files
        forms = await request.form
        if not on_conflict:
            on_conflict = forms.get("on_conflict", "rename").strip().lower()
        uploaded = files.get("file")
        if not uploaded:
            return jsonify({"error": "No file uploaded"}), 400
        try:
            raw_bytes = uploaded.read()
            if len(raw_bytes) > _REPORTS_IMPORT_MAX_BYTES:
                return _payload_too_large_response()
            body = json.loads(raw_bytes)
        except (ValueError, TypeError):
            return jsonify({"error": "Invalid JSON file"}), 400
    else:
        body = await request.get_json(silent=True)
        if body is None:
            return jsonify({"error": "Invalid or missing JSON body"}), 400
        if not on_conflict:
            on_conflict = (body.get("on_conflict") or "rename").strip().lower()

    # ── Validate envelope ────────────────────────────────────────────────────
    if not isinstance(body, dict) or not body.get("sobs_reports_export"):
        return jsonify({"error": "Not a valid SOBS reports export file"}), 400
    if str(body.get("version", "")) != _REPORTS_EXPORT_VERSION:
        return jsonify({"error": f"Unsupported export version: {body.get('version')!r}"}), 400
    if on_conflict not in ("rename", "replace", "skip"):
        return jsonify({"error": "on_conflict must be one of: rename, replace, skip"}), 400

    incoming = body.get("reports")
    if not isinstance(incoming, list):
        return jsonify({"error": "'reports' must be a list"}), 400
    if len(incoming) > _REPORTS_IMPORT_MAX:
        return jsonify({"error": f"Too many reports (max {_REPORTS_IMPORT_MAX})"}), 400

    # ── Build index of existing reports by (page_type, lower(name)) ──────────
    db = get_db()
    existing = _get_reports(db)
    existing_index: dict[tuple[str, str], dict] = {(r["page_type"], r["name"].lower()): r for r in existing}

    n_imported = 0
    n_skipped = 0
    n_replaced = 0
    n_errors = 0
    version_base = int(time.time() * 1000)

    for idx, item in enumerate(incoming):
        if not isinstance(item, dict):
            n_errors += 1
            continue
        name = (item.get("name") or "").strip()
        description = (item.get("description") or "").strip()
        page_type = (item.get("page_type") or "").strip()
        filters = item.get("filters") or {}

        if not name:
            n_errors += 1
            continue
        if page_type not in _REPORT_PAGE_TYPES:
            n_errors += 1
            continue
        if not isinstance(filters, dict):
            n_errors += 1
            continue

        conflict_key = (page_type, name.lower())
        conflict = existing_index.get(conflict_key)

        if conflict:
            if on_conflict == "skip":
                n_skipped += 1
                continue
            elif on_conflict == "replace":
                # Soft-delete the existing report
                _insert_rows_json_each_row(
                    db,
                    "sobs_reports",
                    [
                        {
                            "Id": conflict["id"],
                            "Name": conflict["name"],
                            "Description": conflict["description"],
                            "PageType": conflict["page_type"],
                            "FiltersJson": json.dumps(conflict["filters"], ensure_ascii=False),
                            "IsDeleted": 1,
                            "Version": version_base + idx * 2,
                        }
                    ],
                )
                n_replaced += 1
                # Remove from index so we don't double-delete
                del existing_index[conflict_key]
            else:
                # rename – find a unique name
                candidate = f"{name} (imported)"
                suffix = 2
                while (page_type, candidate.lower()) in existing_index:
                    candidate = f"{name} (imported {suffix})"
                    suffix += 1
                name = candidate

        new_id = str(uuid.uuid4())
        _insert_rows_json_each_row(
            db,
            "sobs_reports",
            [
                {
                    "Id": new_id,
                    "Name": name,
                    "Description": description,
                    "PageType": page_type,
                    "FiltersJson": json.dumps(filters, ensure_ascii=False),
                    "IsDeleted": 0,
                    "Version": version_base + idx * 2 + 1,
                }
            ],
        )
        # Track freshly-imported name to avoid collisions within the same batch
        existing_index[(page_type, name.lower())] = {"id": new_id, "name": name, "page_type": page_type}
        if conflict and on_conflict == "replace":
            # Replacement inserts are counted as replaced, not imported.
            continue
        n_imported += 1

    return jsonify(
        {
            "imported": n_imported,
            "skipped": n_skipped,
            "replaced": n_replaced,
            "errors": n_errors,
        }
    )
