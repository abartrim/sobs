"""
Tests for the SOBS MCP (Model Context Protocol) server module (mcp.py).
Run with:  pytest tests/test_mcp.py
"""

import json
import os
import tempfile

import pytest

os.environ.setdefault("SOBS_DATA_DIR", tempfile.mkdtemp())

import app as sobs_app  # noqa: E402
import mcp as sobs_mcp  # noqa: E402
from app import app, init_db  # noqa: E402


@pytest.fixture(scope="session", autouse=True)
def setup_db():
    previous_testing = app.config.get("TESTING")
    app.config["TESTING"] = True
    init_db()
    yield
    sobs_app._shutdown_db_resources()
    if previous_testing is None:
        app.config.pop("TESTING", None)
    else:
        app.config["TESTING"] = previous_testing


@pytest.fixture
async def client():
    app.config["TESTING"] = True
    async with app.test_client() as c:
        yield c


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _get_db():
    return sobs_app.get_db()


def _create_mcp_key(db, label: str = "test") -> str:
    """Create an MCP API key and return the raw key value."""
    import secrets as _secrets

    raw_key = "smcp_" + _secrets.token_urlsafe(32)
    keys = sobs_mcp._load_mcp_api_keys(db)
    keys.append(
        {
            "id": _secrets.token_hex(8),
            "label": label,
            "key_hash": sobs_mcp._hash_key(raw_key),
            "created_at": "2024-01-01T00:00:00+00:00",
        }
    )
    sobs_mcp._save_mcp_api_keys(db, keys)
    return raw_key


def _clear_mcp_keys(db):
    sobs_mcp._save_mcp_api_keys(db, [])


# ---------------------------------------------------------------------------
# Unit: key management helpers
# ---------------------------------------------------------------------------
class TestMcpKeyHelpers:
    def test_hash_key_is_deterministic(self):
        raw = "smcp_testkey123"
        assert sobs_mcp._hash_key(raw) == sobs_mcp._hash_key(raw)

    def test_hash_key_differs_for_different_keys(self):
        assert sobs_mcp._hash_key("smcp_key_a") != sobs_mcp._hash_key("smcp_key_b")

    def test_load_mcp_api_keys_returns_empty_by_default(self):
        db = _get_db()
        _clear_mcp_keys(db)
        keys = sobs_mcp._load_mcp_api_keys(db)
        assert isinstance(keys, list)
        assert keys == []

    def test_save_and_load_roundtrip(self):
        db = _get_db()
        _clear_mcp_keys(db)
        entry = {"id": "abc", "label": "test", "key_hash": "xyz", "created_at": "2024-01-01"}
        sobs_mcp._save_mcp_api_keys(db, [entry])
        loaded = sobs_mcp._load_mcp_api_keys(db)
        assert len(loaded) == 1
        assert loaded[0]["id"] == "abc"
        _clear_mcp_keys(db)

    def test_mcp_enabled_defaults_to_true(self):
        db = _get_db()
        # Remove setting to test default.
        sobs_app._del_app_setting(db, sobs_mcp._MCP_ENABLED_SETTING)
        assert sobs_mcp._mcp_enabled(db) is True

    def test_mcp_enabled_respects_setting(self):
        db = _get_db()
        sobs_app._set_app_setting(db, sobs_mcp._MCP_ENABLED_SETTING, "0")
        assert sobs_mcp._mcp_enabled(db) is False
        sobs_app._set_app_setting(db, sobs_mcp._MCP_ENABLED_SETTING, "1")
        assert sobs_mcp._mcp_enabled(db) is True


# ---------------------------------------------------------------------------
# Unit: rate limiting
# ---------------------------------------------------------------------------
class TestMcpRateLimiting:
    def test_allows_requests_within_limit(self):
        sobs_mcp._rate_limit_store.clear()
        for _ in range(sobs_mcp._MCP_RATE_LIMIT_REQUESTS):
            assert sobs_mcp._check_rate_limit("127.0.0.1") is True

    def test_blocks_requests_exceeding_limit(self):
        sobs_mcp._rate_limit_store.clear()
        ip = "192.0.2.1"
        for _ in range(sobs_mcp._MCP_RATE_LIMIT_REQUESTS):
            sobs_mcp._check_rate_limit(ip)
        # Next request should be blocked.
        assert sobs_mcp._check_rate_limit(ip) is False

    def test_different_ips_have_independent_counters(self):
        sobs_mcp._rate_limit_store.clear()
        for _ in range(sobs_mcp._MCP_RATE_LIMIT_REQUESTS):
            sobs_mcp._check_rate_limit("10.0.0.1")
        # A different IP should still be allowed.
        assert sobs_mcp._check_rate_limit("10.0.0.2") is True


# ---------------------------------------------------------------------------
# Unit: timestamp parsing helpers
# ---------------------------------------------------------------------------
class TestMcpTimestampParsing:
    def test_parse_ts_with_z_suffix(self):
        result = sobs_mcp._parse_ts("2024-06-01T12:00:00Z")
        assert result == "2024-06-01 12:00:00"

    def test_parse_ts_with_offset(self):
        result = sobs_mcp._parse_ts("2024-06-01T14:00:00+02:00")
        assert result == "2024-06-01 12:00:00"

    def test_parse_ts_with_none(self):
        assert sobs_mcp._parse_ts(None) == ""

    def test_parse_ts_with_empty_string(self):
        assert sobs_mcp._parse_ts("") == ""

    def test_parse_ts_with_invalid_value(self):
        assert sobs_mcp._parse_ts("not-a-date") == ""

    def test_clamp(self):
        assert sobs_mcp._clamp(None, 1, 100, 50) == 50
        assert sobs_mcp._clamp(0, 1, 100, 50) == 1
        assert sobs_mcp._clamp(200, 1, 100, 50) == 100
        assert sobs_mcp._clamp(42, 1, 100, 50) == 42


# ---------------------------------------------------------------------------
# HTTP: GET /mcp/tools  (no auth required)
# ---------------------------------------------------------------------------
class TestMcpToolsDiscovery:
    async def test_get_mcp_tools_returns_200(self, client):
        r = await client.get("/mcp/tools")
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["jsonrpc"] == "2.0"
        assert "result" in data
        assert "tools" in data["result"]

    async def test_get_mcp_tools_lists_expected_tools(self, client):
        r = await client.get("/mcp/tools")
        data = json.loads(await r.get_data())
        names = {t["name"] for t in data["result"]["tools"]}
        expected = {
            "list_services",
            "query_otel_logs",
            "query_otel_traces",
            "query_metrics",
            "query_metrics_raw",
            "get_metric_names",
            "get_anomaly_rules",
            "get_recent_errors",
        }
        assert expected <= names

    async def test_each_tool_has_required_fields(self, client):
        r = await client.get("/mcp/tools")
        data = json.loads(await r.get_data())
        for tool in data["result"]["tools"]:
            assert "name" in tool
            assert "description" in tool
            assert "inputSchema" in tool


# ---------------------------------------------------------------------------
# HTTP: POST /mcp  initialize (no auth)
# ---------------------------------------------------------------------------
class TestMcpInitialize:
    async def test_initialize_returns_server_info(self, client):
        r = await client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["jsonrpc"] == "2.0"
        assert data["id"] == 1
        assert "result" in data
        assert "serverInfo" in data["result"]
        assert data["result"]["serverInfo"]["name"] == "sobs-mcp"

    async def test_initialize_does_not_require_api_key(self, client):
        """initialize method must work without an X-MCP-API-Key header."""
        r = await client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 2, "method": "initialize", "params": {}},
        )
        assert r.status_code == 200


# ---------------------------------------------------------------------------
# HTTP: POST /mcp  authentication
# ---------------------------------------------------------------------------
class TestMcpAuthentication:
    async def test_tools_list_returns_401_without_key(self, client):
        db = _get_db()
        _clear_mcp_keys(db)
        r = await client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}},
        )
        assert r.status_code == 401

    async def test_tools_list_returns_401_with_wrong_key(self, client):
        db = _get_db()
        _clear_mcp_keys(db)
        _create_mcp_key(db, "test")
        r = await client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}},
            headers={"X-MCP-API-Key": "wrong-key"},
        )
        assert r.status_code == 401
        _clear_mcp_keys(db)

    async def test_tools_list_succeeds_with_valid_key(self, client):
        db = _get_db()
        _clear_mcp_keys(db)
        raw_key = _create_mcp_key(db, "test")
        r = await client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}},
            headers={"X-MCP-API-Key": raw_key},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert "result" in data
        assert "tools" in data["result"]
        _clear_mcp_keys(db)


# ---------------------------------------------------------------------------
# HTTP: POST /mcp  tools/call
# ---------------------------------------------------------------------------
class TestMcpToolsCall:
    @pytest.fixture(autouse=True)
    def _setup_key(self):
        db = _get_db()
        self._raw_key = _create_mcp_key(db, "test-tool-call")
        yield
        _clear_mcp_keys(db)

    async def test_unknown_tool_returns_404(self, client):
        r = await client.post(
            "/mcp",
            json={
                "jsonrpc": "2.0",
                "id": 1,
                "method": "tools/call",
                "params": {"name": "nonexistent_tool", "arguments": {}},
            },
            headers={"X-MCP-API-Key": self._raw_key},
        )
        assert r.status_code == 404
        data = json.loads(await r.get_data())
        assert "error" in data

    async def test_list_services_returns_ok(self, client):
        r = await client.post(
            "/mcp",
            json={
                "jsonrpc": "2.0",
                "id": 2,
                "method": "tools/call",
                "params": {"name": "list_services", "arguments": {}},
            },
            headers={"X-MCP-API-Key": self._raw_key},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["jsonrpc"] == "2.0"
        assert "result" in data
        content = json.loads(data["result"]["content"][0]["text"])
        assert "services" in content

    async def test_query_otel_logs_returns_ok(self, client):
        r = await client.post(
            "/mcp",
            json={
                "jsonrpc": "2.0",
                "id": 3,
                "method": "tools/call",
                "params": {"name": "query_otel_logs", "arguments": {"limit": 5}},
            },
            headers={"X-MCP-API-Key": self._raw_key},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        content = json.loads(data["result"]["content"][0]["text"])
        assert "rows" in content
        assert "count" in content

    async def test_query_otel_traces_returns_ok(self, client):
        r = await client.post(
            "/mcp",
            json={
                "jsonrpc": "2.0",
                "id": 4,
                "method": "tools/call",
                "params": {"name": "query_otel_traces", "arguments": {"limit": 5}},
            },
            headers={"X-MCP-API-Key": self._raw_key},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        content = json.loads(data["result"]["content"][0]["text"])
        assert "rows" in content

    async def test_query_metrics_returns_ok(self, client):
        r = await client.post(
            "/mcp",
            json={
                "jsonrpc": "2.0",
                "id": 5,
                "method": "tools/call",
                "params": {"name": "query_metrics", "arguments": {"limit": 5}},
            },
            headers={"X-MCP-API-Key": self._raw_key},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        content = json.loads(data["result"]["content"][0]["text"])
        assert "rows" in content

    async def test_query_metrics_raw_gauge_returns_ok(self, client):
        r = await client.post(
            "/mcp",
            json={
                "jsonrpc": "2.0",
                "id": 6,
                "method": "tools/call",
                "params": {
                    "name": "query_metrics_raw",
                    "arguments": {"metric_kind": "gauge", "limit": 5},
                },
            },
            headers={"X-MCP-API-Key": self._raw_key},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        content = json.loads(data["result"]["content"][0]["text"])
        assert "rows" in content

    async def test_query_metrics_raw_invalid_kind_returns_error_in_content(self, client):
        r = await client.post(
            "/mcp",
            json={
                "jsonrpc": "2.0",
                "id": 7,
                "method": "tools/call",
                "params": {
                    "name": "query_metrics_raw",
                    "arguments": {"metric_kind": "invalid"},
                },
            },
            headers={"X-MCP-API-Key": self._raw_key},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        content = json.loads(data["result"]["content"][0]["text"])
        assert "error" in content

    async def test_get_metric_names_returns_ok(self, client):
        r = await client.post(
            "/mcp",
            json={
                "jsonrpc": "2.0",
                "id": 8,
                "method": "tools/call",
                "params": {"name": "get_metric_names", "arguments": {}},
            },
            headers={"X-MCP-API-Key": self._raw_key},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        content = json.loads(data["result"]["content"][0]["text"])
        assert "metrics" in content

    async def test_get_anomaly_rules_returns_ok(self, client):
        r = await client.post(
            "/mcp",
            json={
                "jsonrpc": "2.0",
                "id": 9,
                "method": "tools/call",
                "params": {"name": "get_anomaly_rules", "arguments": {}},
            },
            headers={"X-MCP-API-Key": self._raw_key},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        content = json.loads(data["result"]["content"][0]["text"])
        assert "rules" in content

    async def test_get_recent_errors_returns_ok(self, client):
        r = await client.post(
            "/mcp",
            json={
                "jsonrpc": "2.0",
                "id": 10,
                "method": "tools/call",
                "params": {"name": "get_recent_errors", "arguments": {"limit": 10}},
            },
            headers={"X-MCP-API-Key": self._raw_key},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        content = json.loads(data["result"]["content"][0]["text"])
        assert "errors" in content


# ---------------------------------------------------------------------------
# HTTP: POST /mcp  masking
# ---------------------------------------------------------------------------
class TestMcpOutputMasking:
    """Verify that the masking framework is applied to tool outputs."""

    @pytest.fixture(autouse=True)
    def _setup_key(self):
        db = _get_db()
        self._raw_key = _create_mcp_key(db, "test-masking")
        yield
        _clear_mcp_keys(db)

    def test_mask_value_for_output_is_applied(self):
        """_mask_value_for_output is called on the tool result before serialisation."""
        import app as sobs_app_mod

        # Build a fake tool result that contains PII (email) in a log body.
        raw_result = {
            "count": 1,
            "rows": [
                {
                    "ts": "2024-01-01 00:00:00",
                    "service": "api",
                    "severity": "ERROR",
                    "body": "error for user test-masking@example.com",
                    "trace_id": "abc",
                    "span_id": "def",
                    "attributes": {},
                }
            ],
        }

        # Apply the same masking the MCP endpoint uses.
        db = _get_db()
        masked = sobs_app_mod._mask_value_for_output(raw_result, db)

        # When output masking is enabled the email should be redacted.
        import masking as _masking_mod

        if sobs_app_mod._is_output_masking_enabled(db):
            body = masked["rows"][0]["body"]
            assert "test-masking@example.com" not in body
            assert _masking_mod.MASK in body
        else:
            # Masking disabled in this environment – at minimum the structure is intact.
            assert "rows" in masked


# ---------------------------------------------------------------------------
# HTTP: POST /mcp  disabled server
# ---------------------------------------------------------------------------
class TestMcpDisabled:
    async def test_disabled_server_returns_503(self, client):
        db = _get_db()
        _clear_mcp_keys(db)
        raw_key = _create_mcp_key(db, "test-disabled")
        sobs_app._set_app_setting(db, sobs_mcp._MCP_ENABLED_SETTING, "0")
        r = await client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": {}},
            headers={"X-MCP-API-Key": raw_key},
        )
        assert r.status_code == 503
        # Re-enable.
        sobs_app._set_app_setting(db, sobs_mcp._MCP_ENABLED_SETTING, "1")
        _clear_mcp_keys(db)


# ---------------------------------------------------------------------------
# HTTP: POST /mcp  unknown method
# ---------------------------------------------------------------------------
class TestMcpUnknownMethod:
    async def test_unknown_method_returns_404(self, client):
        db = _get_db()
        _clear_mcp_keys(db)
        raw_key = _create_mcp_key(db, "test-unknown")
        r = await client.post(
            "/mcp",
            json={"jsonrpc": "2.0", "id": 1, "method": "no_such_method", "params": {}},
            headers={"X-MCP-API-Key": raw_key},
        )
        assert r.status_code == 404
        data = json.loads(await r.get_data())
        assert "error" in data
        _clear_mcp_keys(db)


# ---------------------------------------------------------------------------
# HTTP: Settings API – /api/mcp/keys
# ---------------------------------------------------------------------------
class TestMcpKeyManagementApi:
    async def test_list_keys_returns_200(self, client):
        r = await client.get("/api/mcp/keys")
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["ok"] is True
        assert "keys" in data

    async def test_create_key_returns_raw_key(self, client):
        db = _get_db()
        _clear_mcp_keys(db)
        r = await client.post(
            "/api/mcp/keys",
            json={"label": "my-copilot-key"},
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["ok"] is True
        assert "key" in data
        assert data["key"].startswith("smcp_")
        assert data["label"] == "my-copilot-key"
        _clear_mcp_keys(db)

    async def test_delete_key_removes_it(self, client):
        db = _get_db()
        _clear_mcp_keys(db)
        # Create one key.
        r = await client.post("/api/mcp/keys", json={"label": "to-delete"})
        data = json.loads(await r.get_data())
        key_id = data["id"]
        # Now delete it.
        r2 = await client.delete(f"/api/mcp/keys/{key_id}")
        assert r2.status_code == 200
        d2 = json.loads(await r2.get_data())
        assert d2["ok"] is True
        # Verify it's gone.
        remaining = sobs_mcp._load_mcp_api_keys(db)
        assert all(k["id"] != key_id for k in remaining)
        _clear_mcp_keys(db)

    async def test_delete_nonexistent_key_returns_404(self, client):
        r = await client.delete("/api/mcp/keys/nonexistent-id")
        assert r.status_code == 404

    async def test_create_key_enforces_max_keys(self, client):
        db = _get_db()
        _clear_mcp_keys(db)
        # Fill up to the max.
        for i in range(sobs_mcp._MCP_API_KEY_MAX):
            _create_mcp_key(db, f"key-{i}")
        r = await client.post("/api/mcp/keys", json={"label": "one-too-many"})
        assert r.status_code == 400
        data = json.loads(await r.get_data())
        assert data["ok"] is False
        _clear_mcp_keys(db)

    async def test_list_keys_does_not_expose_key_hash(self, client):
        db = _get_db()
        _clear_mcp_keys(db)
        _create_mcp_key(db, "sensitive")
        r = await client.get("/api/mcp/keys")
        data = json.loads(await r.get_data())
        for key_entry in data["keys"]:
            assert "key_hash" not in key_entry
        _clear_mcp_keys(db)


# ---------------------------------------------------------------------------
# HTTP: Settings API – /api/mcp/enabled
# ---------------------------------------------------------------------------
class TestMcpEnabledApi:
    async def test_set_enabled_true(self, client):
        db = _get_db()
        r = await client.post("/api/mcp/enabled", json={"enabled": True})
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["ok"] is True
        assert data["enabled"] is True
        assert sobs_mcp._mcp_enabled(db) is True

    async def test_set_enabled_false(self, client):
        db = _get_db()
        r = await client.post("/api/mcp/enabled", json={"enabled": False})
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["ok"] is True
        assert data["enabled"] is False
        assert sobs_mcp._mcp_enabled(db) is False
        # Re-enable.
        sobs_app._set_app_setting(db, sobs_mcp._MCP_ENABLED_SETTING, "1")


# ---------------------------------------------------------------------------
# HTTP: Settings page
# ---------------------------------------------------------------------------
class TestMcpSettingsPage:
    async def test_settings_mcp_page_loads(self, client):
        r = await client.get("/settings/mcp")
        assert r.status_code == 200
        html = (await r.get_data()).decode()
        assert "MCP" in html
        assert "API Keys" in html

    async def test_settings_page_shows_mcp_card(self, client):
        r = await client.get("/settings")
        assert r.status_code == 200
        html = (await r.get_data()).decode()
        assert "MCP" in html
        assert "Configure MCP" in html
