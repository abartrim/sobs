from __future__ import annotations

import logging
import time
from typing import Any

from quart import Blueprint, render_template, request

from app import (  # noqa: E402
    _CVE_DISPOSITION_VALUES,
    _CVE_ENABLED_SETTING,
    _CVE_LAST_BACKFILL_ATTEMPTED_SETTING,
    _CVE_LAST_BACKFILL_CAP_SETTING,
    _CVE_LAST_BACKFILL_INSERTED_SETTING,
    _CVE_LAST_SCAN_SETTING,
    _GEO_ENABLED_SETTING,
    _GITHUB_REPO_HEALTH_MAX_ITEMS_PER_REPO,
    _GITHUB_REPO_HEALTH_MAX_REPOS,
    _RUM_SESSION_KEY_SQL,
    RUM_SESSION_DETAIL_EVENT_CAP,
    ChDbConnection,
    _active_part_rows,
    _append_regex_expression_clauses,
    _append_time_window_filter,
    _build_rum_event_item,
    _collect_library_inventory,
    _effective_cve_disposition,
    _geo_lookup_batch,
    _get_app_setting,
    _get_async_http_client,
    _github_backfill_max_releases,
    _github_item_is_security_related,
    _github_version_tokens,
    _insert_rows_json_each_row,
    _inventory_versions_by_package,
    _load_ai_setting,
    _load_repo_scoped_github_token,
    _now_iso,
    _parse_github_repo_owner_name,
    _parse_limit,
    _parse_offset,
    _parse_sort,
    _parse_time_window_args,
    _prepare_re2_filter_patterns,
    _run_cve_scan,
    _text_mentions_version_tokens,
    _time_window_conditions,
    _where_clause,
    get_db,
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
rum_bp = Blueprint("rum", __name__)


@rum_bp.route("/rum")
@require_basic_auth
async def view_rum():
    db = get_db()
    view_mode = request.args.get("view", "sessions").strip().lower()
    if view_mode not in ("sessions", "events"):
        view_mode = "sessions"
    event_type = request.args.get("type", "").strip()
    error_source = request.args.get("error_source", "").strip()
    limit = _parse_limit(200)
    offset = _parse_offset()
    if view_mode == "sessions":
        sort_by, sort_col, sort_dir = _parse_sort(
            {
                "severity": "severity_rank",
                "last_seen": "last_ts",
                "events": "event_count",
                "errors": "error_count",
            },
            "severity",
        )
    else:
        sort_by, sort_col, sort_dir = _parse_sort(
            {"Timestamp": "Timestamp", "EventName": "EventName"},
            "Timestamp",
        )
    order_clause = f"ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}"
    from_ts, to_ts, time_error = _parse_time_window_args()

    q = request.args.get("q", "").strip()
    q_error = ""
    include_patterns: list[str] = []
    exclude_patterns: list[str] = []
    if q:
        include_patterns, exclude_patterns, regex_error = _prepare_re2_filter_patterns(db, q)
        if regex_error:
            q_error = regex_error

    conditions = []
    params = []
    if event_type:
        conditions.append("EventName=?")
        params.append(event_type)
    if error_source:
        conditions.append("LogAttributes['errorSource']=?")
        params.append(error_source)
    _append_time_window_filter(conditions, params, "Timestamp", from_ts, to_ts)
    if q and not q_error:
        _append_regex_expression_clauses(
            conditions=conditions,
            params=params,
            column="Body",
            include_patterns=include_patterns,
            exclude_patterns=exclude_patterns,
        )
    where = _where_clause(conditions)
    total = 0
    events: list[dict[str, Any]] = []
    session_groups: list[dict[str, Any]] = []
    if view_mode == "sessions":
        total = db.execute(
            "SELECT count() FROM ("
            f"SELECT {_RUM_SESSION_KEY_SQL} AS session_key "
            f"FROM hyperdx_sessions {where} GROUP BY session_key)",
            params,
        ).fetchone()[0]
        summary_rows = db.execute(
            "SELECT "
            f"  {_RUM_SESSION_KEY_SQL} AS session_key,"
            "  max(Timestamp) AS last_ts,"
            "  count() AS event_count,"
            "  countIf(EventName IN ('error', 'unhandledrejection')) AS error_count,"
            "  countIf(EventName = 'web-vital' "
            "AND JSONExtractString(Body, 'rating') = 'poor') AS poor_vital_count,"
            "  countIf(EventName = 'web-vital' "
            "AND JSONExtractString(Body, 'rating') = 'needs-improvement') AS warn_vital_count,"
            "  countIf(TraceId != '') AS traced_count,"
            "  multiIf("
            "    countIf(EventName IN ('error', 'unhandledrejection')) > 0, 3,"
            "    countIf(EventName = 'web-vital' "
            "AND JSONExtractString(Body, 'rating') = 'poor') > 0, 2,"
            "    countIf(EventName = 'web-vital' "
            "AND JSONExtractString(Body, 'rating') = 'needs-improvement') > 0, 1,"
            "    0"
            "  ) AS severity_rank,"
            "  argMax(if(LogAttributes['url'] != '', LogAttributes['url'], "
            "LogAttributes['url.full']), Timestamp) AS last_url,"
            "  argMax(EventName, Timestamp) AS last_event_type"
            f" FROM hyperdx_sessions {where}"
            " GROUP BY session_key "
            f" ORDER BY {sort_col} {'ASC' if sort_dir == 'asc' else 'DESC'}, last_ts DESC LIMIT ? OFFSET ?",
            params + [limit, offset],
        ).fetchall()

        if summary_rows:
            session_keys = [str(row["session_key"]) for row in summary_rows]
            placeholders = ",".join(["?"] * len(session_keys))
            detail_conditions = list(conditions)
            detail_conditions.append(f"{_RUM_SESSION_KEY_SQL} IN ({placeholders})")
            detail_where = "WHERE " + " AND ".join(detail_conditions)
            detail_rows = db.execute(
                "SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId "
                "FROM ("
                "SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId, "
                f"{_RUM_SESSION_KEY_SQL} AS session_key, "
                f"row_number() OVER (PARTITION BY {_RUM_SESSION_KEY_SQL} ORDER BY Timestamp DESC) AS row_rank "
                f"FROM hyperdx_sessions {detail_where}"
                ") "
                "WHERE row_rank <= ? "
                "ORDER BY session_key ASC, Timestamp DESC",
                params + session_keys + [RUM_SESSION_DETAIL_EVENT_CAP],
            ).fetchall()
            events_by_session: dict[str, list[dict[str, Any]]] = {}
            for row in detail_rows:
                item = _build_rum_event_item(row)
                events_by_session.setdefault(str(item["session_key"]), []).append(item)

            for row in summary_rows:
                session_key = str(row["session_key"])
                session_events = events_by_session.get(session_key, [])
                session_trace_id = next(
                    (str(ev.get("trace_id", "")) for ev in session_events if ev.get("trace_id")), ""
                )
                session_groups.append(
                    {
                        "session_key": session_key,
                        "session_id": session_key[:8],
                        "last_ts": str(row["last_ts"]),
                        "last_url": str(row["last_url"] or ""),
                        "last_event_type": str(row["last_event_type"] or ""),
                        "event_count": int(row["event_count"]),
                        "error_count": int(row["error_count"]),
                        "poor_vital_count": int(row["poor_vital_count"]),
                        "warn_vital_count": int(row["warn_vital_count"]),
                        "severity_rank": int(row["severity_rank"]),
                        "traced_count": int(row["traced_count"]),
                        "trace_id": session_trace_id,
                        "has_replay": any(bool(ev.get("has_replay")) for ev in session_events),
                        "has_artifact": any(bool(ev.get("has_artifact")) for ev in session_events),
                        "events": session_events,
                    }
                )
    else:
        if not where:
            total = _active_part_rows(db, "hyperdx_sessions")
        else:
            total = db.execute(f"SELECT COUNT(*) FROM hyperdx_sessions {where}", params).fetchone()[0]
        rows = db.execute(
            f"SELECT Timestamp, EventName, Body, LogAttributes, TraceId, SpanId FROM hyperdx_sessions {where} "
            f"{order_clause} LIMIT ? OFFSET ?",
            params + [limit, offset],
        ).fetchall()
        events = [_build_rum_event_item(row) for row in rows]

    event_types = [
        row[0] for row in db.execute("SELECT DISTINCT EventName FROM hyperdx_sessions ORDER BY EventName").fetchall()
    ]
    error_sources = [
        row[0]
        for row in db.execute(
            "SELECT DISTINCT LogAttributes['errorSource'] FROM hyperdx_sessions "
            "WHERE LogAttributes['errorSource']!='' ORDER BY LogAttributes['errorSource']"
        ).fetchall()
    ]

    # Web vitals — anomaly state + sparklines + hotspot via rule-backed derived signals
    vitals_summary: dict[str, dict[str, object]] = {}
    vitals_sparklines: dict[str, list[dict[str, object]]] = {}
    vitals_hotspot: dict[str, list[dict[str, object]]] = {}
    try:
        anom_rows = db.execute(
            "SELECT SignalName,"
            " argMax(value, time) AS latest_value,"
            " argMax(anomaly_state, time) AS latest_state,"
            " toUInt64(argMax(SampleCount, time)) AS latest_count"
            " FROM v_derived_signals_anomaly"
            " WHERE SignalSource = 'rum_vitals'"
            "   AND time >= now() - INTERVAL 60 MINUTE"
            " GROUP BY SignalName"
        ).fetchall()
        for row in anom_rows:
            nm = str(row["SignalName"])
            val = float(row["latest_value"])
            state = str(row["latest_state"])
            cnt = int(row["latest_count"])
            vitals_summary[nm] = {
                "p75": round(val, 3) if nm == "CLS" else round(val, 0),
                "count": cnt,
                "anomaly_state": state,
            }
        spark_rows = db.execute(
            "SELECT SignalName, MinuteBucket, Value, SampleCount"
            " FROM v_derived_signals_1m"
            " WHERE SignalSource = 'rum_vitals'"
            "   AND MinuteBucket >= now() - INTERVAL 60 MINUTE"
            " ORDER BY SignalName, MinuteBucket"
        ).fetchall()
        for row in spark_rows:
            nm = str(row["SignalName"])
            vitals_sparklines.setdefault(nm, []).append(
                {
                    "t": str(row["MinuteBucket"]),
                    "v": round(float(row["Value"]), 3) if nm == "CLS" else round(float(row["Value"]), 1),
                }
            )
        hotspot_rows = db.execute(
            "SELECT"
            "  JSONExtractString(Body, 'name') AS metric,"
            "  LogAttributes['url'] AS url,"
            "  count() AS total,"
            "  countIf(JSONExtractString(Body, 'rating') = 'poor') AS poor_count,"
            "  round(toFloat64(poor_count) / toFloat64(total), 3) AS poor_rate,"
            "  round(quantileExact(0.75)(JSONExtractFloat(Body, 'value')), 1) AS p75"
            " FROM hyperdx_sessions"
            " WHERE EventName = 'web-vital'"
            "   AND Timestamp >= now() - INTERVAL 24 HOUR"
            " GROUP BY metric, url"
            " HAVING total >= 3"
            " ORDER BY metric ASC, poor_rate DESC, total DESC"
            " LIMIT 60"
        ).fetchall()
        for row in hotspot_rows:
            metric = str(row["metric"])
            if not metric:
                continue
            vitals_hotspot.setdefault(metric, []).append(
                {
                    "url": str(row["url"]),
                    "total": int(row["total"]),
                    "poor_count": int(row["poor_count"]),
                    "poor_rate": float(row["poor_rate"]),
                    "p75": float(row["p75"]),
                }
            )
        for metric in vitals_hotspot:
            vitals_hotspot[metric] = vitals_hotspot[metric][:5]
    except Exception:
        log.exception("vitals derived-signal query failed")

    # Error trend — sparkline + direction + top messages + top URLs (vs now())
    error_stats: dict[str, Any] = {
        "total": 0,
        "by_type": {},
        "trend": "stable",
        "recent": 0,
        "prior": 0,
        "sparkline": [],
        "top_messages": [],
        "top_urls": [],
    }
    try:
        trend_row = db.execute(
            "SELECT"
            " countIf(Timestamp >= now() - INTERVAL 30 MINUTE) AS recent,"
            " countIf("
            "   Timestamp >= now() - INTERVAL 60 MINUTE"
            "   AND Timestamp < now() - INTERVAL 30 MINUTE"
            " ) AS prior"
            " FROM hyperdx_sessions"
            " WHERE EventName IN ('error', 'unhandledrejection')"
            "   AND Timestamp >= now() - INTERVAL 60 MINUTE"
        ).fetchone()
        if trend_row:
            recent_cnt = int(trend_row["recent"])
            prior_cnt = int(trend_row["prior"])
            error_stats["recent"] = recent_cnt
            error_stats["prior"] = prior_cnt
            if prior_cnt == 0:
                err_trend = "stable" if recent_cnt == 0 else "up"
            elif recent_cnt > prior_cnt * 1.25:
                err_trend = "up"
            elif recent_cnt < prior_cnt * 0.75:
                err_trend = "down"
            else:
                err_trend = "stable"
            error_stats["trend"] = err_trend
        type_rows = db.execute(
            "SELECT EventName, count() AS cnt"
            " FROM hyperdx_sessions"
            " WHERE EventName IN ('error', 'unhandledrejection')"
            "   AND Timestamp >= now() - INTERVAL 24 HOUR"
            " GROUP BY EventName"
        ).fetchall()
        total_24h = 0
        by_type: dict[str, int] = {}
        for row in type_rows:
            cnt = int(row["cnt"])
            total_24h += cnt
            by_type[str(row["EventName"])] = cnt
        error_stats["total"] = total_24h
        error_stats["by_type"] = by_type
        spark_rows = db.execute(
            "SELECT mb, cnt"
            " FROM ("
            "   SELECT toStartOfMinute(Timestamp) AS mb, count() AS cnt"
            "   FROM hyperdx_sessions"
            "   WHERE EventName IN ('error', 'unhandledrejection')"
            "     AND Timestamp >= now() - INTERVAL 180 MINUTE"
            "   GROUP BY mb"
            " )"
            " ORDER BY mb"
            " WITH FILL"
            " FROM toStartOfMinute(now() - INTERVAL 180 MINUTE)"
            " TO toStartOfMinute(now())"
            " STEP toIntervalMinute(1)"
        ).fetchall()
        error_stats["sparkline"] = [{"t": str(row["mb"]), "v": int(row["cnt"])} for row in spark_rows]
        msg_rows = db.execute(
            "SELECT JSONExtractString(Body, 'message') AS message, count() AS cnt"
            " FROM hyperdx_sessions"
            " WHERE EventName IN ('error', 'unhandledrejection')"
            "   AND Timestamp >= now() - INTERVAL 24 HOUR"
            "   AND JSONExtractString(Body, 'message') != ''"
            " GROUP BY message ORDER BY cnt DESC LIMIT 8"
        ).fetchall()
        error_stats["top_messages"] = [{"message": str(row["message"]), "count": int(row["cnt"])} for row in msg_rows]
        url_rows = db.execute(
            "SELECT LogAttributes['url'] AS url, count() AS cnt"
            " FROM hyperdx_sessions"
            " WHERE EventName IN ('error', 'unhandledrejection')"
            "   AND Timestamp >= now() - INTERVAL 24 HOUR"
            "   AND LogAttributes['url'] != ''"
            " GROUP BY url ORDER BY cnt DESC LIMIT 5"
        ).fetchall()
        error_stats["top_urls"] = [{"url": str(row["url"]), "count": int(row["cnt"])} for row in url_rows]
    except Exception:
        log.exception("error stats query failed")

    return await render_template(
        "rum.html",
        events=events,
        session_groups=session_groups,
        total=total,
        limit=limit,
        offset=offset,
        view_mode=view_mode,
        event_type=event_type,
        event_types=event_types,
        error_source=error_source,
        error_sources=error_sources,
        vitals_summary=vitals_summary,
        vitals_sparklines=vitals_sparklines,
        vitals_hotspot=vitals_hotspot,
        error_stats=error_stats,
        sort_by=sort_by,
        sort_dir=sort_dir,
        from_ts=from_ts,
        to_ts=to_ts,
        q=q,
        error_msg=q_error or time_error,
    )


# ---------------------------------------------------------------------------
# Web UI – Web Traffic (IP geo-map, browser context analytics)
# ---------------------------------------------------------------------------
@rum_bp.route("/web-traffic")
@require_basic_auth
async def view_web_traffic():
    """Web traffic analytics: IP→geo map, top URLs, and browser context breakdown."""
    db = get_db()
    from_ts, to_ts, time_error = _parse_time_window_args()
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    where = ("WHERE " + " AND ".join(time_conditions)) if time_conditions else ""

    if not where:
        total = _active_part_rows(db, "hyperdx_sessions")
    else:
        total = db.execute(f"SELECT COUNT(*) FROM hyperdx_sessions {where}", time_params).fetchone()[0]

    top_urls_rows = db.execute(
        f"SELECT LogAttributes['url'] AS url, COUNT(*) AS cnt "
        f"FROM hyperdx_sessions {where} "
        f"GROUP BY url HAVING url != '' ORDER BY cnt DESC LIMIT 20",
        time_params,
    ).fetchall()
    top_urls = [(str(r[0]), int(r[1])) for r in top_urls_rows]

    event_type_rows = db.execute(
        f"SELECT EventName, COUNT(*) AS cnt FROM hyperdx_sessions {where} "
        f"GROUP BY EventName ORDER BY cnt DESC LIMIT 20",
        time_params,
    ).fetchall()
    event_types = [(str(r[0]), int(r[1])) for r in event_type_rows]

    geo_enabled = (_get_app_setting(db, _GEO_ENABLED_SETTING) or "true").lower() in ("1", "true", "yes")

    return await render_template(
        "web_traffic.html",
        total=total,
        top_urls=top_urls,
        event_types=event_types,
        from_ts=from_ts,
        to_ts=to_ts,
        error_msg=time_error,
        geo_enabled=geo_enabled,
    )


# ---------------------------------------------------------------------------
# API – Web Traffic geo aggregation  GET /api/web-traffic/geo
# ---------------------------------------------------------------------------
@rum_bp.route("/api/web-traffic/geo", methods=["GET"])
@require_basic_auth
async def api_web_traffic_geo():
    """Return IP→country aggregation from RUM events using local geoip2fast DB.

    All lookups are performed locally (no external network calls).
    geoip2fast is MIT licensed; bundled data is from IANA/RIR (public domain).
    """
    db = get_db()
    from_ts, to_ts, _ = _parse_time_window_args()
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    where = ("WHERE " + " AND ".join(time_conditions)) if time_conditions else ""

    rows = db.execute(
        f"SELECT LogAttributes['client.ip'] AS ip, COUNT(*) AS cnt "
        f"FROM hyperdx_sessions {where} "
        f"GROUP BY ip HAVING ip != '' ORDER BY cnt DESC LIMIT 200",
        time_params,
    ).fetchall()
    ip_counts: dict[str, int] = {str(r[0]): int(r[1]) for r in rows}

    geo_enabled = (_get_app_setting(db, _GEO_ENABLED_SETTING) or "true").lower() in ("1", "true", "yes")
    geo_data = _geo_lookup_batch(list(ip_counts.keys()), geo_enabled=geo_enabled)

    country_totals: dict[str, int] = {}
    ip_details: list[dict] = []
    for ip, cnt in ip_counts.items():
        geo = geo_data.get(ip, {})
        country = geo.get("country") or "Unknown"
        country_code = geo.get("country_code", "")
        country_totals[country] = country_totals.get(country, 0) + cnt
        ip_details.append(
            {
                "ip": ip,
                "count": cnt,
                "country": country,
                "country_code": country_code,
            }
        )

    country_counts = sorted(
        [{"name": k, "value": v} for k, v in country_totals.items()],
        key=lambda x: -x["value"],
    )
    return jsonify(
        {
            "ok": True,
            "country_counts": country_counts,
            "ip_details": ip_details[:100],
            "geo_enabled": geo_enabled,
        }
    )


# ---------------------------------------------------------------------------
# API – Web Traffic browser context aggregation (GET /api/web-traffic/browsers, etc.)
# ---------------------------------------------------------------------------
@rum_bp.route("/api/web-traffic/browsers", methods=["GET"])
@require_basic_auth
async def api_web_traffic_browsers():
    """Return browser name/version aggregation from RUM events."""
    db = get_db()
    from_ts, to_ts, _ = _parse_time_window_args()
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    where = ("WHERE " + " AND ".join(time_conditions)) if time_conditions else ""

    rows = db.execute(
        f"SELECT LogAttributes['browser.context.browserName'] AS browser, "
        f"LogAttributes['browser.context.browserVersion'] AS version, COUNT(*) AS cnt "
        f"FROM hyperdx_sessions {where} "
        f"GROUP BY browser, version ORDER BY cnt DESC LIMIT 50",
        time_params,
    ).fetchall()

    browsers = [
        {
            "name": f"{str(r[0])} {str(r[1])}".strip() or "Unknown",
            "value": int(r[2]),
        }
        for r in rows
    ]
    return jsonify({"ok": True, "browsers": browsers})


@rum_bp.route("/api/web-traffic/os", methods=["GET"])
@require_basic_auth
async def api_web_traffic_os():
    """Return OS name/version aggregation from RUM events."""
    db = get_db()
    from_ts, to_ts, _ = _parse_time_window_args()
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    where = ("WHERE " + " AND ".join(time_conditions)) if time_conditions else ""

    rows = db.execute(
        f"SELECT LogAttributes['browser.context.osName'] AS os, "
        f"LogAttributes['browser.context.osVersion'] AS version, COUNT(*) AS cnt "
        f"FROM hyperdx_sessions {where} "
        f"GROUP BY os, version ORDER BY cnt DESC LIMIT 50",
        time_params,
    ).fetchall()

    operating_systems = [
        {
            "name": f"{str(r[0])} {str(r[1])}".strip() or "Unknown",
            "value": int(r[2]),
        }
        for r in rows
    ]
    return jsonify({"ok": True, "operating_systems": operating_systems})


@rum_bp.route("/api/web-traffic/timezones", methods=["GET"])
@require_basic_auth
async def api_web_traffic_timezones():
    """Return timezone aggregation from RUM events."""
    db = get_db()
    from_ts, to_ts, _ = _parse_time_window_args()
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    where = ("WHERE " + " AND ".join(time_conditions)) if time_conditions else ""

    rows = db.execute(
        f"SELECT LogAttributes['browser.context.timezone'] AS tz, COUNT(*) AS cnt "
        f"FROM hyperdx_sessions {where} "
        f"GROUP BY tz HAVING tz != '' ORDER BY cnt DESC LIMIT 50",
        time_params,
    ).fetchall()

    timezones = [{"name": str(r[0]), "value": int(r[1])} for r in rows]
    return jsonify({"ok": True, "timezones": timezones})


@rum_bp.route("/api/web-traffic/languages", methods=["GET"])
@require_basic_auth
async def api_web_traffic_languages():
    """Return language aggregation from RUM events."""
    db = get_db()
    from_ts, to_ts, _ = _parse_time_window_args()
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    where = ("WHERE " + " AND ".join(time_conditions)) if time_conditions else ""

    rows = db.execute(
        f"SELECT LogAttributes['browser.context.language'] AS lang, COUNT(*) AS cnt "
        f"FROM hyperdx_sessions {where} "
        f"GROUP BY lang HAVING lang != '' ORDER BY cnt DESC LIMIT 50",
        time_params,
    ).fetchall()

    languages = [{"name": str(r[0]), "value": int(r[1])} for r in rows]
    return jsonify({"ok": True, "languages": languages})


@rum_bp.route("/api/web-traffic/devices", methods=["GET"])
@require_basic_auth
async def api_web_traffic_devices():
    """Return device class aggregation from RUM events."""
    db = get_db()
    from_ts, to_ts, _ = _parse_time_window_args()
    time_conditions, time_params = _time_window_conditions("Timestamp", from_ts, to_ts)
    where = ("WHERE " + " AND ".join(time_conditions)) if time_conditions else ""

    rows = db.execute(
        f"SELECT LogAttributes['browser.context.deviceClass'] AS device, COUNT(*) AS cnt "
        f"FROM hyperdx_sessions {where} "
        f"GROUP BY device HAVING device != '' ORDER BY cnt DESC",
        time_params,
    ).fetchall()

    devices = [{"name": str(r[0]), "value": int(r[1])} for r in rows]
    return jsonify({"ok": True, "devices": devices})


@rum_bp.route("/api/enrichment/libraries", methods=["GET"])
@require_basic_auth
async def api_enrichment_libraries():
    """Return merged library inventory with CVE counts and provenance."""
    db = get_db()
    try:
        inventory = _collect_library_inventory(db)
        cve_rows = db.execute(
            "SELECT Package, Ecosystem, Version, countDistinct(OsvId) AS cve_count "
            "FROM sobs_cve_findings FINAL "
            "GROUP BY Package, Ecosystem, Version"
        ).fetchall()
        cve_count_by_key = {f"{str(r[0])}::{str(r[1])}::{str(r[2])}": int(r[3]) for r in cve_rows}
        source_order = {"release_registry": 0, "otel_sdk": 1, "otel_scope": 2}

        libraries: list[dict[str, Any]] = []
        for item in inventory:
            package = str(item.get("package") or "")
            ecosystem = str(item.get("ecosystem") or "")
            version = str(item.get("version") or "")
            service = str(item.get("service") or item.get("app_name") or "")
            source = str(item.get("source") or "")
            cve_count = cve_count_by_key.get(f"{package}::{ecosystem}::{version}", 0)
            if not ecosystem:
                status = "unknown_ecosystem"
            elif cve_count > 0:
                status = "vulnerable"
            else:
                status = "clean"
            libraries.append(
                {
                    "package": package,
                    "ecosystem": ecosystem,
                    "version": version,
                    "service": service,
                    "source": source,
                    "app_name": str(item.get("app_name") or ""),
                    "release_version": str(item.get("release_version") or ""),
                    "environment": str(item.get("environment") or ""),
                    "cve_count": cve_count,
                    "status": status,
                }
            )

        libraries.sort(
            key=lambda x: (
                -int(x.get("cve_count", 0)),
                source_order.get(str(x.get("source") or ""), 99),
                str(x.get("package") or "").lower(),
                str(x.get("version") or "").lower(),
                str(x.get("service") or "").lower(),
            )
        )
        return jsonify(
            {
                "ok": True,
                "libraries": libraries,
                "scanned_at": _get_app_setting(db, _CVE_LAST_SCAN_SETTING) or "",
            }
        )
    except Exception as exc:
        return jsonify({"ok": False, "error": str(exc)}), 500


async def _collect_github_repo_health_summary(db: "ChDbConnection") -> dict[str, Any]:
    """Return version-scoped GitHub repo health counts for CVE workflow context."""
    default_github_token = _load_ai_setting(db, "ai.github_token", "").strip()

    try:
        app_rows = db.execute(
            "SELECT Id, Name, Slug, RepoUrl "
            "FROM sobs_apps FINAL "
            "WHERE IsDeleted=0 AND Enabled=1 AND RepoUrl != '' "
            "ORDER BY Name ASC"
        ).fetchall()
        release_rows = db.execute(
            "SELECT AppId, ReleaseVersion "
            "FROM sobs_app_releases FINAL "
            "WHERE IsDeleted=0 "
            "ORDER BY ReleasedAt DESC LIMIT 4000"
        ).fetchall()
    except Exception as exc:
        return {"ok": False, "error": str(exc)}

    versions_by_app: dict[str, list[str]] = {}
    for row in release_rows:
        app_id = str(row[0] or "")
        rel_ver = str(row[1] or "").strip()
        if not app_id or not rel_ver:
            continue
        versions = versions_by_app.setdefault(app_id, [])
        if rel_ver not in versions and len(versions) < 5:
            versions.append(rel_ver)

    repo_targets: list[dict[str, Any]] = []
    for row in app_rows:
        app_id = str(row[0] or "")
        app_name = str(row[1] or row[2] or "")
        repo_url = str(row[3] or "")
        owner, repo = _parse_github_repo_owner_name(repo_url)
        versions = versions_by_app.get(app_id, [])
        if not owner or not repo or not versions:
            continue
        repo_targets.append(
            {
                "app_name": app_name,
                "owner": owner,
                "repo": repo,
                "versions": versions,
            }
        )

    repo_targets = repo_targets[:_GITHUB_REPO_HEALTH_MAX_REPOS]
    client = await _get_async_http_client()

    total_open_issues = 0
    total_open_prs = 0
    total_security_items = 0
    scanned_repos = 0
    repos_summary: list[dict[str, Any]] = []

    for target in repo_targets:
        owner = str(target["owner"])
        repo = str(target["repo"])
        github_token = _load_repo_scoped_github_token(db, owner, repo) or default_github_token
        if not github_token:
            continue
        versions = [str(v) for v in target.get("versions", []) if str(v).strip()]
        version_tokens: set[str] = set()
        for version in versions:
            version_tokens.update(_github_version_tokens(version))
        if not version_tokens:
            continue

        scanned_repos += 1
        try:
            resp = await client.get(
                f"https://api.github.com/repos/{owner}/{repo}/issues",
                params={"state": "open", "per_page": str(_GITHUB_REPO_HEALTH_MAX_ITEMS_PER_REPO)},
                headers={
                    "Authorization": f"Bearer {github_token}",
                    "Accept": "application/vnd.github+json",
                    "X-GitHub-Api-Version": "2022-11-28",
                },
                timeout=15,
            )
            if resp.status_code != 200:
                continue
            items = resp.json() if resp.content else []
            if not isinstance(items, list):
                continue
        except Exception:
            continue

        repo_issues = 0
        repo_prs = 0
        repo_security = 0

        for item in items:
            if not isinstance(item, dict):
                continue
            text = f"{str(item.get('title') or '')}\n{str(item.get('body') or '')}"
            if not _text_mentions_version_tokens(text, version_tokens):
                continue
            is_pr = isinstance(item.get("pull_request"), dict)
            if is_pr:
                repo_prs += 1
            else:
                repo_issues += 1
            if _github_item_is_security_related(item):
                repo_security += 1

        total_open_issues += repo_issues
        total_open_prs += repo_prs
        total_security_items += repo_security
        repos_summary.append(
            {
                "repo": f"{owner}/{repo}",
                "app_name": str(target.get("app_name") or ""),
                "versions": versions,
                "open_issues": repo_issues,
                "open_prs": repo_prs,
                "security_items": repo_security,
            }
        )

    repos_summary.sort(
        key=lambda r: (
            -(int(r.get("security_items", 0)) + int(r.get("open_issues", 0)) + int(r.get("open_prs", 0))),
            str(r.get("repo") or "").lower(),
        )
    )

    return {
        "ok": True,
        "scanned_repos": scanned_repos,
        "total_repos_considered": len(repo_targets),
        "open_issues": total_open_issues,
        "open_prs": total_open_prs,
        "security_items": total_security_items,
        "version_scoped": True,
        "last_synced_at": _now_iso(),
        "repos": repos_summary,
    }


@rum_bp.route("/api/enrichment/github/repo-health", methods=["GET"])
@require_basic_auth
async def api_enrichment_github_repo_health():
    """Return version-scoped GitHub repo health counts for CVE workflow context."""
    db = get_db()
    summary = await _collect_github_repo_health_summary(db)
    if not bool(summary.get("ok")):
        return jsonify(summary), 500
    return jsonify(summary)


# ---------------------------------------------------------------------------
# API – CVE enrichment endpoints
# Uses OSV.dev (Apache 2.0, free, no API key required)
# Reference: https://google.github.io/osv.dev/api/
# ---------------------------------------------------------------------------
@rum_bp.route("/enrichment/cve")
@require_basic_auth
async def view_enrichment_cve():
    """Dedicated CVE / vulnerability findings page."""
    db = get_db()
    cve_enabled = (_get_app_setting(db, _CVE_ENABLED_SETTING) or "true").lower() in ("1", "true", "yes")
    cve_last_scan = _get_app_setting(db, _CVE_LAST_SCAN_SETTING) or ""
    github_backfill_max_releases = _github_backfill_max_releases(db)
    try:
        cve_last_backfill_attempted = int(_get_app_setting(db, _CVE_LAST_BACKFILL_ATTEMPTED_SETTING) or "0")
    except (TypeError, ValueError):
        cve_last_backfill_attempted = 0
    try:
        cve_last_backfill_inserted = int(_get_app_setting(db, _CVE_LAST_BACKFILL_INSERTED_SETTING) or "0")
    except (TypeError, ValueError):
        cve_last_backfill_inserted = 0
    try:
        cve_last_backfill_cap = int(_get_app_setting(db, _CVE_LAST_BACKFILL_CAP_SETTING) or "0")
    except (TypeError, ValueError):
        cve_last_backfill_cap = 0

    selected_severities = [s.strip() for s in request.args.getlist("severity") if s.strip()]
    selected_ecosystems = [e.strip() for e in request.args.getlist("ecosystem") if e.strip()]
    severity_filter = selected_severities[0] if selected_severities else ""
    ecosystem_filter = selected_ecosystems[0] if selected_ecosystems else ""
    package_filter = request.args.get("package", "").strip()
    show_all = request.args.get("show_all", "").strip().lower() in ("1", "true", "yes", "on")

    cve_findings: list[dict] = []
    ecosystems: list[str] = []
    severities: list[str] = []
    if cve_enabled:
        try:
            versions_by_package = _inventory_versions_by_package(db)
            disposition_rows = db.execute(
                "SELECT OsvId, Package, Ecosystem, Version, Disposition, Note " "FROM sobs_cve_dispositions FINAL"
            ).fetchall()
            dispositions_by_key = {
                f"{str(r[0])}::{str(r[1])}::{str(r[2])}::{str(r[3])}": {
                    "disposition": str(r[4] or "open"),
                    "note": str(r[5] or ""),
                }
                for r in disposition_rows
            }
            rows = db.execute(
                "SELECT Package, Ecosystem, Version, ServiceName, OsvId, CveIds, Summary, Severity, Published "
                "FROM sobs_cve_findings FINAL "
                "ORDER BY Published DESC LIMIT 500"
            ).fetchall()
            for r in rows:
                finding_key = f"{str(r[4])}::{str(r[0])}::{str(r[1])}::{str(r[2])}"
                raw_disposition = dispositions_by_key.get(finding_key, {}).get("disposition", "open")
                disposition, disposition_expired = _effective_cve_disposition(
                    str(raw_disposition or "open"),
                    str(r[0]),
                    str(r[1]),
                    str(r[2]),
                    versions_by_package,
                )
                cve_findings.append(
                    {
                        "package": str(r[0]),
                        "ecosystem": str(r[1]),
                        "version": str(r[2]),
                        "service": str(r[3]),
                        "osv_id": str(r[4]),
                        "cve_ids": [c for c in str(r[5]).split(",") if c],
                        "summary": str(r[6]),
                        "severity": str(r[7]),
                        "published": str(r[8]),
                        "disposition": disposition,
                        "raw_disposition": str(raw_disposition or "open"),
                        "disposition_expired": disposition_expired,
                        "disposition_note": dispositions_by_key.get(finding_key, {}).get("note", ""),
                    }
                )
            ecosystems = sorted({f["ecosystem"] for f in cve_findings if f["ecosystem"]})
            severities = sorted({f["severity"] for f in cve_findings if f["severity"]})
            if selected_severities:
                selected_severity_set = set(selected_severities)
                cve_findings = [f for f in cve_findings if f["severity"] in selected_severity_set]
            if selected_ecosystems:
                selected_ecosystem_set = set(selected_ecosystems)
                cve_findings = [f for f in cve_findings if f["ecosystem"] in selected_ecosystem_set]
            if package_filter:
                pkg_lower = package_filter.lower()
                cve_findings = [f for f in cve_findings if pkg_lower in f["package"].lower()]
            if not show_all:
                cve_findings = [
                    f
                    for f in cve_findings
                    if f.get("disposition", "open") not in ("accepted", "false_positive", "fixed")
                ]
        except Exception:
            pass

    return await render_template(
        "cve.html",
        cve_enabled=cve_enabled,
        cve_last_scan=cve_last_scan,
        github_backfill_max_releases=github_backfill_max_releases,
        cve_last_backfill_attempted=cve_last_backfill_attempted,
        cve_last_backfill_inserted=cve_last_backfill_inserted,
        cve_last_backfill_cap=cve_last_backfill_cap,
        cve_findings=cve_findings,
        ecosystems=ecosystems,
        severities=severities,
        severity_filter=severity_filter,
        ecosystem_filter=ecosystem_filter,
        selected_severities=selected_severities,
        selected_ecosystems=selected_ecosystems,
        package_filter=package_filter,
        show_all=show_all,
    )


@rum_bp.route("/api/enrichment/cve/findings", methods=["GET"])
@require_basic_auth
async def api_cve_findings():
    """Return the most recent CVE findings stored from the last background scan."""
    db = get_db()
    cve_enabled = (_get_app_setting(db, _CVE_ENABLED_SETTING) or "true").lower() in ("1", "true", "yes")
    if not cve_enabled:
        return jsonify({"ok": False, "error": "CVE enrichment is disabled"}), 403
    try:
        show_all = request.args.get("show_all", "").strip().lower() in ("1", "true", "yes", "on")
        versions_by_package = _inventory_versions_by_package(db)
        disposition_rows = db.execute(
            "SELECT OsvId, Package, Ecosystem, Version, Disposition, Note " "FROM sobs_cve_dispositions FINAL"
        ).fetchall()
        dispositions_by_key = {
            f"{str(r[0])}::{str(r[1])}::{str(r[2])}::{str(r[3])}": {
                "disposition": str(r[4] or "open"),
                "note": str(r[5] or ""),
            }
            for r in disposition_rows
        }
        rows = db.execute(
            "SELECT Package, Ecosystem, Version, ServiceName, OsvId, CveIds, Summary, Severity, Published "
            "FROM sobs_cve_findings FINAL "
            "ORDER BY Published DESC LIMIT 100"
        ).fetchall()
        findings = []
        for r in rows:
            finding_key = f"{str(r[4])}::{str(r[0])}::{str(r[1])}::{str(r[2])}"
            disposition_data = dispositions_by_key.get(finding_key, {})
            raw_disposition = str(disposition_data.get("disposition", "open") or "open")
            disposition, disposition_expired = _effective_cve_disposition(
                raw_disposition,
                str(r[0]),
                str(r[1]),
                str(r[2]),
                versions_by_package,
            )
            if (not show_all) and disposition in ("accepted", "false_positive", "fixed"):
                continue
            findings.append(
                {
                    "package": str(r[0]),
                    "ecosystem": str(r[1]),
                    "version": str(r[2]),
                    "service": str(r[3]),
                    "osv_id": str(r[4]),
                    "cve_ids": [c for c in str(r[5]).split(",") if c],
                    "summary": str(r[6]),
                    "severity": str(r[7]),
                    "published": str(r[8]),
                    "disposition": disposition,
                    "raw_disposition": raw_disposition,
                    "disposition_expired": disposition_expired,
                    "disposition_note": str(disposition_data.get("note", "") or ""),
                }
            )
        last_scan = _get_app_setting(db, _CVE_LAST_SCAN_SETTING) or ""
        return jsonify({"ok": True, "findings": findings, "last_scan": last_scan})
    except Exception as exc:
        return jsonify({"ok": False, "error": str(exc)}), 500


@rum_bp.route("/api/enrichment/cve/findings/<osv_id>/disposition", methods=["POST"])
@require_basic_auth
async def api_cve_set_disposition(osv_id: str):
    """Set disposition and optional note for a CVE finding."""
    db = get_db()
    payload = await request.get_json(force=True, silent=True) or {}
    package = str(payload.get("package", "")).strip()
    ecosystem = str(payload.get("ecosystem", "")).strip()
    version = str(payload.get("version", "")).strip()
    disposition = str(payload.get("disposition", "")).strip().lower()
    note = str(payload.get("note", "")).strip()

    if not osv_id.strip() or not package or not ecosystem or not version:
        return jsonify({"ok": False, "error": "osv_id, package, ecosystem, and version are required"}), 400
    if disposition not in _CVE_DISPOSITION_VALUES:
        return (
            jsonify(
                {
                    "ok": False,
                    "error": f"invalid disposition: {disposition}",
                    "allowed": sorted(_CVE_DISPOSITION_VALUES),
                }
            ),
            400,
        )

    existing = db.execute(
        "SELECT CreatedAt, Version_ FROM sobs_cve_dispositions FINAL "
        "WHERE OsvId=? AND Package=? AND Ecosystem=? AND Version=? LIMIT 1",
        [osv_id, package, ecosystem, version],
    ).fetchone()
    now_ts = _now_iso()
    current_version = int(time.time() * 1000)
    row = {
        "OsvId": osv_id,
        "Package": package,
        "Ecosystem": ecosystem,
        "Version": version,
        "Disposition": disposition,
        "Note": note,
        "CreatedAt": str(existing["CreatedAt"]) if existing else now_ts,
        "UpdatedAt": now_ts,
        "Version_": max(current_version, int(existing["Version_"]) + 1 if existing else current_version),
    }
    _insert_rows_json_each_row(db, "sobs_cve_dispositions", [row])
    return jsonify(
        {
            "ok": True,
            "osv_id": osv_id,
            "package": package,
            "ecosystem": ecosystem,
            "version": version,
            "disposition": disposition,
            "note": note,
            "updated_at": row["UpdatedAt"],
        }
    )


@rum_bp.route("/api/enrichment/cve/scan", methods=["POST"])
@require_basic_auth
async def api_cve_scan():
    """Trigger an immediate CVE scan (normally scheduled every 24 hours).

    Scans release metadata and OTEL telemetry for library versions,
    then queries OSV.dev (Apache 2.0) for known CVEs.  Stores results in
    sobs_cve_findings.
    """
    try:
        summary = await _run_cve_scan()
        return jsonify(summary)
    except Exception as exc:
        return jsonify({"ok": False, "error": str(exc)}), 500


@rum_bp.route("/api/rum/validate-regex", methods=["POST"])
@require_basic_auth
async def api_rum_validate_regex():
    """Validate a regex pattern used by /rum?q=... and return a sample match."""
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

        event_type = _regex_scope_text(scope, "type")
        error_source = _regex_scope_text(scope, "error_source")
        if event_type:
            where_parts.append("EventName = ?")
            where_params.append(event_type)
        if error_source:
            where_parts.append("LogAttributes['errorSource'] = ?")
            where_params.append(error_source)

        time_parts, time_params = _regex_scope_time_conditions(scope, "Timestamp")
        where_parts.extend(time_parts)
        where_params.extend(time_params)

        sample = _regex_best_effort_sample(
            db,
            from_sql="hyperdx_sessions",
            sample_column="Body",
            order_column="Timestamp",
            include_patterns=include_patterns,
            exclude_patterns=_exclude_patterns,
            where_parts=where_parts,
            where_params=where_params,
        )
        return masked_jsonify({"ok": True, "sample": sample})
    except Exception:
        return masked_jsonify({"ok": True, "sample": None})
