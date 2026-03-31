"""
Tests for SOBS – Simple Observe.
Run with:  pytest tests/
"""

import asyncio
import base64
import json
import os
import re
import secrets
import subprocess
import sys
import tempfile
import time

import pytest

# Point to a temp DB before importing the app
os.environ["SOBS_DATA_DIR"] = tempfile.mkdtemp()

import app as sobs_app  # noqa: E402
from app import app, compress, compress_json, decompress, decompress_json, init_db  # noqa: E402


@pytest.fixture(scope="session", autouse=True)
def setup_db():
    init_db()


@pytest.fixture
async def client():
    app.config["TESTING"] = True
    async with app.test_client() as c:
        yield c


# ---------------------------------------------------------------------------
# Compression helpers
# ---------------------------------------------------------------------------
class TestCompression:
    async def test_compress_decompress_roundtrip(self):
        text = "Hello, World! " * 100
        assert decompress(compress(text)) == text

    async def test_compress_json_roundtrip(self):
        obj = {"key": "value", "num": 42, "list": [1, 2, 3]}
        assert decompress_json(compress_json(obj)) == obj

    async def test_compressed_smaller_than_plain(self):
        text = "INFO This is a repeating log message. " * 50
        assert len(compress(text)) < len(text.encode())

    async def test_decompress_none_returns_empty(self):
        assert decompress(None) == ""

    async def test_decompress_json_none_returns_empty_dict(self):
        assert decompress_json(None) == {}


# ---------------------------------------------------------------------------
# Health check
# ---------------------------------------------------------------------------
class TestHealth:
    async def test_health_ok(self, client):
        r = await client.get("/health")
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["status"] == "ok"

    async def test_health_db_ok(self, client):
        r = await client.get("/health/db")
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["status"] == "ok"
        assert data["db"] == "ok"
        assert isinstance(data["write_queue_depth"], int)

    async def test_health_db_returns_503_when_db_fails(self, client, monkeypatch):
        class _BrokenDb:
            def execute(self, *_args, **_kwargs):
                raise RuntimeError("db timeout")

        monkeypatch.setattr(sobs_app, "ensure_db_schema", lambda: None)
        monkeypatch.setattr(sobs_app, "get_db", lambda: _BrokenDb())

        r = await client.get("/health/db")
        assert r.status_code == 503
        data = json.loads(await r.get_data())
        assert data["status"] == "degraded"
        assert data["db"] == "error"
        assert "db timeout" in data["error"]
        assert isinstance(data["write_queue_depth"], int)


class TestWriteQueue:
    async def test_ingest_returns_503_when_write_queue_full(self, client, monkeypatch):
        def _raise_queue_full(_op, wait=False):
            raise sobs_app.WriteQueueFullError("write queue is full")

        monkeypatch.setattr(sobs_app, "_queue_write", _raise_queue_full)
        r = await client.post("/v1/errors", json={"service": "q-full", "message": "drop me"})
        assert r.status_code == 503
        data = json.loads(await r.get_data())
        assert "write queue is full" in data["error"]

    async def test_ingest_returns_500_when_writer_op_fails(self, client, monkeypatch):
        def _raise_write_failure(*_args, **_kwargs):
            raise RuntimeError("write failed")

        monkeypatch.setattr(sobs_app, "_insert_rows_json_each_row", _raise_write_failure)
        r = await client.post("/v1/errors", json={"service": "q-fail", "message": "boom"})
        assert r.status_code == 500
        data = json.loads(await r.get_data())
        assert "write failed" in data["error"]

    async def test_non_testing_mode_uses_async_queue_and_persists(self, monkeypatch):
        # In non-testing mode ingest should enqueue writes and return immediately.
        monkeypatch.setitem(app.config, "TESTING", False)
        marker = f"queued-{time.time_ns()}"
        async with app.test_client() as c:
            r = await c.post("/v1/errors", json={"service": "q-async", "message": marker})
            assert r.status_code == 200

        deadline = time.time() + 2.0
        found = False
        while time.time() < deadline:
            cnt = (
                sobs_app.get_db()
                .execute("SELECT COUNT(*) FROM otel_logs WHERE EventName='exception' AND Body=?", (marker,))
                .fetchone()[0]
            )
            if cnt and int(cnt) > 0:
                found = True
                break
            await asyncio.sleep(0.02)
        assert found, "queued write was not persisted within timeout"

    async def test_health_db_remains_available_during_ingest_burst(self, monkeypatch):
        monkeypatch.setitem(app.config, "TESTING", False)
        marker = f"burst-{time.time_ns()}"

        async with app.test_client() as c:
            for i in range(60):
                r = await c.post(
                    "/v1/errors",
                    json={
                        "service": "q-burst",
                        "type": "BurstError",
                        "message": f"{marker}-{i}",
                    },
                )
                assert r.status_code == 200

                # Probe DB readiness repeatedly while writes are flowing.
                if i % 5 == 0:
                    hr = await c.get("/health/db")
                    assert hr.status_code == 200
                    hdata = json.loads(await hr.get_data())
                    assert hdata["db"] == "ok"
                    assert isinstance(hdata["write_queue_depth"], int)

            deadline = time.time() + 3.0
            while time.time() < deadline:
                cnt = (
                    sobs_app.get_db()
                    .execute("SELECT COUNT(*) FROM otel_logs WHERE EventName='exception' AND ServiceName='q-burst'")
                    .fetchone()[0]
                )
                if int(cnt) >= 60:
                    break
                await asyncio.sleep(0.02)

            final_health = await c.get("/health/db")
            assert final_health.status_code == 200
            final_data = json.loads(await final_health.get_data())
            assert final_data["db"] == "ok"

        assert int(cnt) >= 60


class TestStorageConfiguration:
    @staticmethod
    def _write_encrypted_config(base_dir: str, key_hex: str) -> str:
        plain_dir = os.path.join(base_dir, "plain")
        encrypted_dir = os.path.join(base_dir, "encrypted")
        os.makedirs(plain_dir, exist_ok=True)
        os.makedirs(encrypted_dir, exist_ok=True)

        config_path = os.path.join(base_dir, "config.xml")
        config_xml = f"""<clickhouse>
  <custom_local_disks_base_directory>{base_dir}</custom_local_disks_base_directory>
  <storage_configuration>
    <disks>
      <plain>
        <type>local</type>
        <path>{plain_dir}/</path>
      </plain>
      <encrypted_disk>
        <type>encrypted</type>
        <disk>plain</disk>
        <path>{encrypted_dir}/</path>
        <algorithm>AES_128_CTR</algorithm>
        <key_hex>{key_hex}</key_hex>
      </encrypted_disk>
    </disks>
    <policies>
      <encrypted_only>
        <volumes>
          <main>
            <disk>encrypted_disk</disk>
          </main>
        </volumes>
      </encrypted_only>
    </policies>
  </storage_configuration>
</clickhouse>
"""
        with open(config_path, "w", encoding="utf-8") as f:
            f.write(config_xml)
        return config_path

    @staticmethod
    def _run_probe_script(script: str, env_overrides: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
        env = os.environ.copy()
        if env_overrides:
            env.update(env_overrides)
        return subprocess.run(
            [sys.executable, "-c", script],
            capture_output=True,
            text=True,
            check=False,
            env=env,
        )

    async def test_chdb_startup_succeeds_without_external_config(self, monkeypatch):
        monkeypatch.delenv(sobs_app.CHDB_CONFIG_FILE_ENV, raising=False)
        monkeypatch.delenv(sobs_app.CHDB_EXPECT_DISK_ENV, raising=False)
        monkeypatch.delenv(sobs_app.CHDB_EXPECT_POLICY_ENV, raising=False)

        data_dir = tempfile.mkdtemp(prefix="sobs-chdb-plain-")
        script = """
import app as sobs_app

conn = sobs_app.ChDbConnection(sobs_app.DB_PATH)
try:
    row = conn.execute('SELECT 1 AS ok').fetchone()
    print(f"ok={row['ok']}")
finally:
    conn.close()
"""
        result = self._run_probe_script(script, {"SOBS_DATA_DIR": data_dir})
        assert result.returncode == 0, result.stderr
        assert "ok=1" in result.stdout
        assert "chDB connect target:" in result.stderr

    async def test_chdb_external_config_encrypted_disk_policy_active(self):
        base_dir = tempfile.mkdtemp(prefix="sobs-chdb-encrypted-")
        encryption_key = secrets.token_hex(16)
        config_path = self._write_encrypted_config(base_dir, encryption_key)

        script = """
import app as sobs_app

conn = sobs_app.ChDbConnection(sobs_app.DB_PATH)
try:
    conn.execute(
        "CREATE TABLE IF NOT EXISTS encryption_probe "
        "(id UInt64, payload String) "
        "ENGINE=MergeTree "
        "ORDER BY id "
        "SETTINGS storage_policy='encrypted_only'"
    )
    conn.execute("INSERT INTO encryption_probe VALUES (1, 'ok')")
    row = conn.execute("SELECT payload FROM encryption_probe WHERE id=1").fetchone()
    print(f"payload={row['payload']}")

    disk = conn.execute(
        "SELECT is_encrypted FROM system.disks WHERE name='encrypted_disk'"
    ).fetchone()
    print(f"encrypted_disk={disk['is_encrypted'] if disk else 'missing'}")

    policy = conn.execute(
        "SELECT disks FROM system.storage_policies WHERE policy_name='encrypted_only'"
    ).fetchone()
    print(f"policy_disks={policy['disks'] if policy else 'missing'}")

    table = conn.execute(
        "SELECT storage_policy FROM system.tables "
        "WHERE database='default' AND name='encryption_probe'"
    ).fetchone()
    print(f"table_policy={table['storage_policy'] if table else 'missing'}")
finally:
    conn.close()
"""

        result = self._run_probe_script(
            script,
            {
                "SOBS_DATA_DIR": base_dir,
                sobs_app.CHDB_CONFIG_FILE_ENV: config_path,
                sobs_app.CHDB_EXPECT_DISK_ENV: "encrypted_disk",
                sobs_app.CHDB_EXPECT_POLICY_ENV: "encrypted_only",
            },
        )
        assert result.returncode == 0, result.stderr
        assert "payload=ok" in result.stdout
        assert "encrypted_disk=1" in result.stdout
        assert "encrypted_disk" in result.stdout
        assert "table_policy=encrypted_only" in result.stdout
        assert "chDB connect target:" in result.stderr
        assert "config-file=" in result.stderr

    async def test_chdb_config_must_be_absolute_path(self, monkeypatch):
        monkeypatch.setenv(sobs_app.CHDB_CONFIG_FILE_ENV, "relative/config.xml")
        with pytest.raises(RuntimeError, match="must be an absolute path"):
            sobs_app._build_chdb_connect_target("/tmp/example.chdb")

    async def test_chdb_fails_clearly_when_expected_policy_is_missing(self):
        data_dir = tempfile.mkdtemp(prefix="sobs-chdb-ignored-")
        missing_cfg = os.path.join(tempfile.mkdtemp(prefix="sobs-missing-cfg-"), "missing.xml")

        script = """
import app as sobs_app

try:
    sobs_app.ChDbConnection(sobs_app.DB_PATH)
except RuntimeError as exc:
    print(str(exc))
    raise
"""

        result = self._run_probe_script(
            script,
            {
                "SOBS_DATA_DIR": data_dir,
                sobs_app.CHDB_CONFIG_FILE_ENV: missing_cfg,
                sobs_app.CHDB_EXPECT_DISK_ENV: "encrypted_disk",
                sobs_app.CHDB_EXPECT_POLICY_ENV: "encrypted_only",
            },
        )

        assert result.returncode != 0
        assert "expected storage configuration was not applied" in result.stderr or (
            "expected storage configuration was not applied" in result.stdout
        )


class TestDbBootstrap:
    async def test_first_dashboard_and_rum_request_bootstrap_schema(self, client):
        """Schema tables must exist and key routes must be functional after init_db()."""
        r = await client.get("/")
        assert r.status_code == 200

        rum = await client.post(
            "/v1/rum",
            json={
                "session_id": "first-session",
                "event_type": "pageview",
                "url": "/",
                "data": {"boot": True},
            },
        )
        assert rum.status_code == 200

        tables = {
            row[0]
            for row in sobs_app.get_db()
            .execute(
                "SELECT name FROM system.tables"
                " WHERE database='default' AND name IN ('otel_logs', 'hyperdx_sessions')"
            )
            .fetchall()
        }
        assert {"otel_logs", "hyperdx_sessions"}.issubset(tables)


# ---------------------------------------------------------------------------
# OTLP Logs ingest
# ---------------------------------------------------------------------------
class TestLogsIngest:
    def _otlp_payload(self, message="test log", level="INFO", service="test-svc"):
        ts_ns = str(int(time.time() * 1_000_000_000))
        return {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": ts_ns,
                                    "severityText": level,
                                    "body": {"stringValue": message},
                                    "attributes": [{"key": "env", "value": {"stringValue": "test"}}],
                                    "traceId": "aabbccdd11223344aabbccdd11223344",
                                    "spanId": "1122334455667788",
                                }
                            ]
                        }
                    ],
                }
            ]
        }

    async def test_ingest_single_log(self, client):
        r = await client.post("/v1/logs", json=self._otlp_payload())
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 1

    async def test_ingest_multiple_logs(self, client):
        payload = self._otlp_payload()
        # Add a second log record
        payload["resourceLogs"][0]["scopeLogs"][0]["logRecords"].append(
            {
                "timeUnixNano": str(int(time.time() * 1_000_000_000)),
                "severityText": "ERROR",
                "body": {"stringValue": "error log"},
            }
        )
        r = await client.post("/v1/logs", json=payload)
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 2

    async def test_ingest_empty_payload(self, client):
        r = await client.post("/v1/logs", json={})
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 0


# ---------------------------------------------------------------------------
# OTLP Traces ingest
# ---------------------------------------------------------------------------
class TestTracesIngest:
    def _span_payload(self, name="test-span", status_code=1):
        start_ns = int(time.time() * 1_000_000_000)
        return {
            "resourceSpans": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "trace-svc"}}]},
                    "scopeSpans": [
                        {
                            "spans": [
                                {
                                    "traceId": "deadbeefdeadbeefdeadbeefdeadbeef",
                                    "spanId": "cafebabe12345678",
                                    "parentSpanId": "",
                                    "name": name,
                                    "startTimeUnixNano": str(start_ns),
                                    "endTimeUnixNano": str(start_ns + 50_000_000),
                                    "status": {"code": status_code},
                                    "attributes": [
                                        {"key": "http.method", "value": {"stringValue": "GET"}},
                                        {"key": "http.status_code", "value": {"intValue": 200}},
                                    ],
                                }
                            ]
                        }
                    ],
                }
            ]
        }

    async def test_ingest_span(self, client):
        r = await client.post("/v1/traces", json=self._span_payload())
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 1

    async def test_error_span_creates_error(self, client):
        """An ERROR span should also create an entry in the errors table."""
        payload = self._span_payload(name="failing-op", status_code=2)
        payload["resourceSpans"][0]["scopeSpans"][0]["spans"][0]["attributes"].extend(
            [
                {"key": "exception.type", "value": {"stringValue": "ValueError"}},
                {"key": "exception.message", "value": {"stringValue": "bad input"}},
            ]
        )
        r = await client.post("/v1/traces", json=payload)
        assert r.status_code == 200

    async def test_ingest_empty_payload(self, client):
        r = await client.post("/v1/traces", json={})
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 0


# ---------------------------------------------------------------------------
# OTLP protobuf ingest
# ---------------------------------------------------------------------------
class TestOtlpProtobufIngest:
    """Verify that application/x-protobuf payloads are accepted and persisted."""

    PROTOBUF_CT = "application/x-protobuf"

    def _make_log_proto_bytes(self, message="proto log", level="INFO", service="proto-svc"):
        from opentelemetry.proto.collector.logs.v1.logs_service_pb2 import ExportLogsServiceRequest
        from opentelemetry.proto.common.v1.common_pb2 import AnyValue, KeyValue
        from opentelemetry.proto.logs.v1.logs_pb2 import LogRecord, ResourceLogs, ScopeLogs
        from opentelemetry.proto.resource.v1.resource_pb2 import Resource

        ts_ns = int(time.time() * 1_000_000_000)
        record = LogRecord(
            time_unix_nano=ts_ns,
            severity_text=level,
            body=AnyValue(string_value=message),
            attributes=[KeyValue(key="env", value=AnyValue(string_value="test"))],
            trace_id=bytes.fromhex("aabbccdd11223344aabbccdd11223344"),
            span_id=bytes.fromhex("1122334455667788"),
        )
        resource = Resource(attributes=[KeyValue(key="service.name", value=AnyValue(string_value=service))])
        msg = ExportLogsServiceRequest(
            resource_logs=[ResourceLogs(resource=resource, scope_logs=[ScopeLogs(log_records=[record])])]
        )
        return msg.SerializeToString()

    def _make_trace_proto_bytes(self, name="proto-span", status_code=1, service="proto-trace-svc"):
        from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
        from opentelemetry.proto.common.v1.common_pb2 import AnyValue, KeyValue
        from opentelemetry.proto.resource.v1.resource_pb2 import Resource
        from opentelemetry.proto.trace.v1.trace_pb2 import ResourceSpans, ScopeSpans, Span, Status

        start_ns = int(time.time() * 1_000_000_000)
        span = Span(
            trace_id=bytes.fromhex("deadbeefdeadbeefdeadbeefdeadbeef"),
            span_id=bytes.fromhex("cafebabe12345678"),
            name=name,
            start_time_unix_nano=start_ns,
            end_time_unix_nano=start_ns + 50_000_000,
            status=Status(code=status_code),
            attributes=[KeyValue(key="http.method", value=AnyValue(string_value="GET"))],
        )
        resource = Resource(attributes=[KeyValue(key="service.name", value=AnyValue(string_value=service))])
        msg = ExportTraceServiceRequest(
            resource_spans=[ResourceSpans(resource=resource, scope_spans=[ScopeSpans(spans=[span])])]
        )
        return msg.SerializeToString()

    async def test_protobuf_log_ingest_accepted(self, client):
        body = self._make_log_proto_bytes()
        r = await client.post("/v1/logs", data=body, headers={"Content-Type": self.PROTOBUF_CT})
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 1

    async def test_protobuf_log_persisted_in_db(self, client):
        body = self._make_log_proto_bytes(message="hello protobuf", service="proto-db-svc")
        r = await client.post("/v1/logs", data=body, headers={"Content-Type": self.PROTOBUF_CT})
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 1
        # Verify the row exists in the database
        row = (
            sobs_app.get_db()
            .execute(
                "SELECT ServiceName FROM otel_logs WHERE ServiceName=? ORDER BY Timestamp DESC LIMIT 1",
                ("proto-db-svc",),
            )
            .fetchone()
        )
        assert row is not None, "Log row not found in DB"
        assert row[0] == "proto-db-svc"

    async def test_protobuf_trace_ingest_accepted(self, client):
        body = self._make_trace_proto_bytes()
        r = await client.post("/v1/traces", data=body, headers={"Content-Type": self.PROTOBUF_CT})
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 1

    async def test_protobuf_trace_persisted_in_db(self, client):
        body = self._make_trace_proto_bytes(name="my-span", service="proto-trace-db-svc")
        r = await client.post("/v1/traces", data=body, headers={"Content-Type": self.PROTOBUF_CT})
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 1
        # Verify the row exists in the database
        row = (
            sobs_app.get_db()
            .execute(
                "SELECT SpanName, ServiceName FROM otel_traces " "WHERE ServiceName=? ORDER BY Timestamp DESC LIMIT 1",
                ("proto-trace-db-svc",),
            )
            .fetchone()
        )
        assert row is not None, "Span row not found in DB"
        assert row[0] == "my-span"
        assert row[1] == "proto-trace-db-svc"

    async def test_protobuf_error_span_creates_error(self, client):
        """An ERROR span sent via protobuf should also create an errors table entry."""
        from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
        from opentelemetry.proto.common.v1.common_pb2 import AnyValue, KeyValue
        from opentelemetry.proto.resource.v1.resource_pb2 import Resource
        from opentelemetry.proto.trace.v1.trace_pb2 import ResourceSpans, ScopeSpans, Span, Status

        start_ns = int(time.time() * 1_000_000_000)
        span = Span(
            trace_id=bytes.fromhex("deadbeefdeadbeefdeadbeefdeadbeef"),
            span_id=bytes.fromhex("cafebabe12345678"),
            name="failing-proto-op",
            start_time_unix_nano=start_ns,
            end_time_unix_nano=start_ns + 10_000_000,
            status=Status(code=2),  # STATUS_CODE_ERROR
            attributes=[
                KeyValue(key="exception.type", value=AnyValue(string_value="ValueError")),
                KeyValue(key="exception.message", value=AnyValue(string_value="proto bad input")),
            ],
        )
        resource = Resource(attributes=[KeyValue(key="service.name", value=AnyValue(string_value="proto-err-svc"))])
        msg = ExportTraceServiceRequest(
            resource_spans=[ResourceSpans(resource=resource, scope_spans=[ScopeSpans(spans=[span])])]
        )
        r = await client.post("/v1/traces", data=msg.SerializeToString(), headers={"Content-Type": self.PROTOBUF_CT})
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 1
        # Verify an errors row was created
        row = (
            sobs_app.get_db()
            .execute(
                "SELECT ServiceName, LogAttributes['exception.type'] "
                "FROM otel_logs "
                "WHERE ServiceName=? AND EventName='exception' "
                "ORDER BY Timestamp DESC LIMIT 1",
                ("proto-err-svc",),
            )
            .fetchone()
        )
        assert row is not None, "Error row not found in DB"
        assert row[1] == "ValueError"

    async def test_protobuf_invalid_body_returns_400(self, client):
        r = await client.post("/v1/logs", data=b"not valid protobuf", headers={"Content-Type": self.PROTOBUF_CT})
        assert r.status_code == 400
        assert "error" in json.loads(await r.get_data())

    async def test_protobuf_invalid_traces_body_returns_400(self, client):
        r = await client.post("/v1/traces", data=b"\xff\xfe garbage", headers={"Content-Type": self.PROTOBUF_CT})
        assert r.status_code == 400
        assert "error" in json.loads(await r.get_data())

    async def test_json_ingest_still_works_alongside_protobuf(self, client):
        """Regression: JSON ingest path must remain functional."""
        ts_ns = str(int(time.time() * 1_000_000_000))
        payload = {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "json-svc"}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {"timeUnixNano": ts_ns, "severityText": "INFO", "body": {"stringValue": "json ok"}}
                            ]
                        }
                    ],
                }
            ]
        }
        r = await client.post("/v1/logs", json=payload)
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 1


class TestErrorsIngest:
    async def test_ingest_error(self, client):
        r = await client.post(
            "/v1/errors",
            json={
                "service": "test-svc",
                "type": "RuntimeError",
                "message": "something broke",
                "stack": "Traceback:\n  at main (app.py:10)",
            },
        )
        assert r.status_code == 200
        assert json.loads(await r.get_data())["ok"] is True

    async def test_ingest_error_minimal(self, client):
        r = await client.post("/v1/errors", json={})
        assert r.status_code == 200

    async def test_resolve_error(self, client):
        # Create an error first
        await client.post(
            "/v1/errors",
            json={
                "service": "resolve-svc",
                "type": "TestError",
                "message": "resolve me",
            },
        )
        # Resolve it (get the ID from the errors page)
        r = await client.get("/errors?service=resolve-svc&resolved=0")
        assert r.status_code == 200
        html = (await r.get_data()).decode("utf-8")
        m = re.search(r"/errors/([0-9a-f]{32})/resolve", html)
        assert m, "Could not find resolve URL in errors page"
        r2 = await client.post(f"/errors/{m.group(1)}/resolve")
        assert r2.status_code == 200

    async def test_errors_page_has_ai_help_button(self, client):
        """Each error on the errors page should include the AI Help clipboard button."""
        await client.post(
            "/v1/errors",
            json={
                "service": "ai-help-svc",
                "type": "AITestError",
                "message": "error for ai help test",
                "stack": "AITestError: error for ai help test\n  at test.py:1",
            },
        )
        r = await client.get("/errors?service=ai-help-svc")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "ai-help-btn" in body
        assert "AI Help" in body
        assert "bi-robot" in body  # bootstrap icon
        assert "data-err-type" in body  # data attributes for stable JS extraction
        assert "data-err-message" in body
        assert "data-err-service" in body


# ---------------------------------------------------------------------------
# RUM ingest
# ---------------------------------------------------------------------------
class TestRumIngest:
    async def test_ingest_pageview(self, client):
        r = await client.post(
            "/v1/rum",
            json=[
                {
                    "type": "pageview",
                    "timestamp": "2024-01-01T00:00:00Z",
                    "sessionId": "sess-001",
                    "url": "https://example.com/",
                    "title": "Home",
                }
            ],
        )
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 1

    async def test_ingest_web_vital(self, client):
        r = await client.post(
            "/v1/rum",
            json=[
                {
                    "type": "web-vital",
                    "name": "LCP",
                    "value": 1800,
                    "rating": "good",
                    "sessionId": "sess-001",
                    "url": "https://example.com/",
                }
            ],
        )
        assert r.status_code == 200

    async def test_ingest_js_error(self, client):
        r = await client.post(
            "/v1/rum",
            json=[
                {
                    "type": "error",
                    "sessionId": "sess-002",
                    "url": "https://example.com/app",
                    "message": "Cannot read properties of null",
                    "errorType": "TypeError",
                    "stack": "TypeError: Cannot read...\n  at main (app.js:5)",
                }
            ],
        )
        assert r.status_code == 200

    async def test_ingest_dict_payload(self, client):
        r = await client.post(
            "/v1/rum",
            json={
                "events": [
                    {
                        "type": "pageview",
                        "sessionId": "sess-003",
                        "url": "https://example.com/about",
                    }
                ]
            },
        )
        assert r.status_code == 200

    async def test_ingest_empty_list(self, client):
        r = await client.post("/v1/rum", json=[])
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 0


# ---------------------------------------------------------------------------
# AI transparency ingest
# ---------------------------------------------------------------------------
class TestAIIngest:
    async def test_ingest_ai_event(self, client):
        r = await client.post(
            "/v1/ai",
            json={
                "service": "my-app",
                "provider": "openai",
                "model": "gpt-4o-mini",
                "prompt": "What is 2+2?",
                "response": "4",
                "tokens_in": 8,
                "tokens_out": 1,
                "duration_ms": 320,
            },
        )
        assert r.status_code == 200
        assert json.loads(await r.get_data())["ok"] is True

    async def test_ingest_ai_minimal(self, client):
        r = await client.post("/v1/ai", json={})
        assert r.status_code == 200

    async def test_ingest_ai_with_otel_messages(self, client):
        """Ingest should accept gen_ai.input.messages / gen_ai.output.messages."""
        r = await client.post(
            "/v1/ai",
            json={
                "service": "my-app",
                "provider": "openai",
                "model": "gpt-4o",
                "input_messages": [{"role": "user", "content": "Hello"}],
                "output_messages": [{"role": "assistant", "content": "Hi there"}],
                "tokens_in": 5,
                "tokens_out": 3,
                "duration_ms": 150,
            },
        )
        assert r.status_code == 200
        assert json.loads(await r.get_data())["ok"] is True

    async def test_ingest_ai_operation_defaults_to_chat(self, client):
        """Missing operation field should default to 'chat'."""
        r = await client.post(
            "/v1/ai",
            json={"service": "svc", "provider": "anthropic", "model": "claude-3"},
        )
        assert r.status_code == 200

    async def test_ingest_ai_operation_canonicalised_to_lowercase(self, client):
        """operation value should be lower-cased and stripped before storage."""
        import app as app_module

        q = asyncio.Queue()
        app_module._sse_subscribers.add(q)
        try:
            r = await client.post(
                "/v1/ai",
                json={
                    "service": "svc",
                    "provider": "openai",
                    "model": "gpt-4o",
                    "operation": "  Chat  ",
                },
            )
            assert r.status_code == 200
            event = q.get_nowait()
            assert event["operation"] == "chat"
        finally:
            app_module._sse_subscribers.discard(q)

    async def test_ingest_ai_broadcasts_sse_event(self, client):
        """POST /v1/ai should broadcast a source=ai SSE event."""
        import app as app_module

        q = asyncio.Queue()
        app_module._sse_subscribers.add(q)
        try:
            r = await client.post(
                "/v1/ai",
                json={
                    "service": "ai-svc",
                    "provider": "openai",
                    "model": "gpt-4o",
                    "tokens_in": 10,
                    "tokens_out": 20,
                    "duration_ms": 200,
                },
            )
            assert r.status_code == 200
            event = q.get_nowait()
            assert event["source"] == "ai"
            assert event["service"] == "ai-svc"
            assert event["provider"] == "openai"
            assert event["model"] == "gpt-4o"
            assert "ts" in event
            assert "duration_ms" in event
        finally:
            app_module._sse_subscribers.discard(q)


# ---------------------------------------------------------------------------
# Web UI pages
# ---------------------------------------------------------------------------
class TestUIPages:
    async def test_dashboard(self, client):
        r = await client.get("/")
        assert r.status_code == 200
        assert b"SOBS" in await r.get_data()
        assert b"Dashboard" in await r.get_data()

    async def test_logs_page(self, client):
        r = await client.get("/logs")
        assert r.status_code == 200

    async def test_logs_grep_filter(self, client):
        # Insert a distinctive log
        await client.post(
            "/v1/logs",
            json={
                "resourceLogs": [
                    {
                        "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "grep-test"}}]},
                        "scopeLogs": [
                            {
                                "logRecords": [
                                    {
                                        "timeUnixNano": str(int(time.time() * 1_000_000_000)),
                                        "severityText": "INFO",
                                        "body": {"stringValue": "unique_grep_marker_xyz"},
                                    }
                                ]
                            }
                        ],
                    }
                ]
            },
        )
        r = await client.get("/logs?q=unique_grep_marker_xyz")
        assert r.status_code == 200
        assert b"unique_grep_marker_xyz" in await r.get_data()

    async def test_logs_sql_filter(self, client):
        r = await client.get("/logs?sql=level%3D%27INFO%27")
        assert r.status_code == 200

    async def test_logs_field_hints_api(self, client):
        r = await client.get("/api/logs/field-hints")
        assert r.status_code == 200
        data = await r.get_json()
        assert "fields" in data
        assert "tag_keys" in data
        assert "operators" in data
        assert "keywords" in data
        assert "functions" in data
        assert "snippets" in data
        field_names = [f["name"] for f in data["fields"]]
        assert "level" in field_names
        assert "service" in field_names
        assert "body" in field_names
        operator_names = [op.upper() for op in data["operators"]]
        assert "ILIKE" in operator_names
        function_names = [fn["name"] for fn in data["functions"]]
        assert "has_tag" in function_names

    async def test_logs_validate_filter_api(self, client):
        r_ok = await client.post("/api/logs/validate-filter", json={"sql": "level='INFO' AND service='svc-a'"})
        assert r_ok.status_code == 200
        ok_data = await r_ok.get_json()
        assert ok_data["ok"] is True
        assert "SeverityText='INFO'" in ok_data["normalized"]

        r_bad = await client.post("/api/logs/validate-filter", json={"sql": "level='INFO"})
        assert r_bad.status_code == 200
        bad_data = await r_bad.get_json()
        assert bad_data["ok"] is False
        assert bad_data["issues"]

    async def test_logs_attr_keys_catalog_and_hints(self, client):
        import time as _time

        from app import get_db

        ts_ns = int(_time.time() * 1_000_000_000)
        attr_key = f"http.route.test.{ts_ns}"
        await client.post(
            "/v1/logs",
            json={
                "resourceLogs": [
                    {
                        "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "attr-key-svc"}}]},
                        "scopeLogs": [
                            {
                                "logRecords": [
                                    {
                                        "timeUnixNano": str(ts_ns),
                                        "severityText": "INFO",
                                        "body": {"stringValue": "attr key capture test"},
                                        "attributes": [
                                            {"key": attr_key, "value": {"stringValue": "ok"}},
                                        ],
                                    }
                                ]
                            }
                        ],
                    }
                ]
            },
        )

        db = get_db()
        row = db.execute(
            "SELECT count() FROM sobs_log_attr_keys FINAL WHERE RecordType='log' AND AttrKey=? AND IsDeleted=0",
            [attr_key],
        ).fetchone()
        assert row is not None and int(row[0]) >= 1

        hints = await client.get("/api/logs/field-hints")
        assert hints.status_code == 200
        hints_data = await hints.get_json()
        assert attr_key in (hints_data.get("attr_keys") or [])

    async def test_logs_has_tag_filter(self, client):
        """has_tag() in sql WHERE should filter by record tags."""
        import time as _time

        ts_ns = int(_time.time() * 1_000_000_000)
        # Use a stable service name to avoid inflating the distinct-services count
        service_name = "has-tag-test-svc"
        tag_key = f"env-{ts_ns}"
        # Create a tag rule that matches this service
        await client.post(
            "/settings/tags",
            form={
                "name": f"env-rule-{ts_ns}",
                "record_types": ["log"],
                "match_field": "service_name",
                "match_operator": "eq",
                "match_value": service_name,
                "match_attr_key": "",
                "tag_key": tag_key,
                "tag_value": "prod",
            },
        )
        # Ingest a log so the tag rule fires
        await client.post(
            "/v1/logs",
            json={
                "resourceLogs": [
                    {
                        "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service_name}}]},
                        "scopeLogs": [
                            {
                                "logRecords": [
                                    {
                                        "timeUnixNano": str(ts_ns),
                                        "severityText": "INFO",
                                        "body": {"stringValue": f"has-tag-body-{ts_ns}"},
                                    }
                                ]
                            }
                        ],
                    }
                ]
            },
        )
        # Use has_tag() SQL filter – should not produce an error
        import urllib.parse

        sql = f"has_tag('{tag_key}','prod')"
        r = await client.get(f"/logs?sql={urllib.parse.quote(sql)}")
        assert r.status_code == 200
        # Should not show an SQL error
        data = await r.get_data()
        assert b"SQL error" not in data

    async def test_logs_has_tag_filter_escaped_quotes(self, client):
        """has_tag() should support SQL-escaped quotes in key/value arguments."""
        import time as _time
        import urllib.parse

        from app import get_db

        ts_ns = int(_time.time() * 1_000_000_000)
        service_name = f"has-tag-quote-svc-{ts_ns}"
        tag_key = "team'o"
        tag_value = "owner's"

        await client.post(
            "/v1/logs",
            json={
                "resourceLogs": [
                    {
                        "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service_name}}]},
                        "scopeLogs": [
                            {
                                "logRecords": [
                                    {
                                        "timeUnixNano": str(ts_ns),
                                        "severityText": "INFO",
                                        "body": {"stringValue": f"has-tag-quote-body-{ts_ns}"},
                                    }
                                ]
                            }
                        ],
                    }
                ]
            },
        )

        db = get_db()
        db.execute(
            "INSERT INTO sobs_record_tags (RecordType, RecordId, TagKey, TagValue, IsAuto, IsDeleted, Version) "
            "SELECT 'log', MD5(concat(ServiceName,'|',toString(Timestamp),'|',TraceId,'|',SpanId)), ?, ?, 0, 0, ? "
            "FROM otel_logs WHERE ServiceName=? ORDER BY Timestamp DESC LIMIT 1",
            [tag_key, tag_value, int(_time.time() * 1_000_000), service_name],
        )

        # Use SQL-escaped apostrophes in has_tag input to mirror UI-generated SQL.
        sql = "has_tag('team''o','owner''s')"
        r = await client.get(f"/logs?sql={urllib.parse.quote(sql)}")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"SQL error" not in data
        assert f"has-tag-quote-body-{ts_ns}".encode() in data

    async def test_logs_tag_stats_handles_colon_in_tag_parts(self, client):
        """Tag stats links should preserve key/value even when either contains ':'."""
        import time as _time

        from app import _record_id_for_log, get_db

        ts_ns = int(_time.time() * 1_000_000_000)
        service_name = f"has-tag-colon-svc-{ts_ns}"
        body = f"has-tag-colon-body-{ts_ns}"

        await client.post(
            "/v1/logs",
            json={
                "resourceLogs": [
                    {
                        "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service_name}}]},
                        "scopeLogs": [
                            {
                                "logRecords": [
                                    {
                                        "timeUnixNano": str(ts_ns),
                                        "severityText": "INFO",
                                        "body": {"stringValue": body},
                                    }
                                ]
                            }
                        ],
                    }
                ]
            },
        )

        db = get_db()
        row = db.execute(
            "SELECT Timestamp, TraceId, SpanId FROM otel_logs WHERE ServiceName=? ORDER BY Timestamp DESC LIMIT 1",
            [service_name],
        ).fetchone()
        assert row is not None
        rid = _record_id_for_log(str(row["Timestamp"]), service_name, str(row["TraceId"]), str(row["SpanId"]))

        db.execute(
            "INSERT INTO sobs_record_tags ("
            "RecordType, RecordId, TagKey, TagValue, IsAuto, IsDeleted, Version"
            ") VALUES (?, ?, ?, ?, ?, ?, ?)",
            ["log", rid, "k:a", "v:b", 0, 0, int(_time.time() * 1_000_000)],
        )

        r = await client.get(f"/logs?service={service_name}&stats=1")
        assert r.status_code == 200
        html = await r.get_data(as_text=True)
        assert "k:a=v:b" in html

    async def test_errors_page(self, client):
        r = await client.get("/errors")
        assert r.status_code == 200

    async def test_traces_page(self, client):
        r = await client.get("/traces")
        assert r.status_code == 200

    async def test_logs_time_window_filter(self, client):
        base_payload = {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "tw-logs"}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": str(1704067200 * 1_000_000_000),
                                    "severityText": "INFO",
                                    "body": {"stringValue": "too-early-log"},
                                },
                                {
                                    "timeUnixNano": str(1704067800 * 1_000_000_000),
                                    "severityText": "INFO",
                                    "body": {"stringValue": "in-window-log"},
                                },
                            ]
                        }
                    ],
                }
            ]
        }
        await client.post("/v1/logs", json=base_payload)

        r = await client.get("/logs?service=tw-logs&from_ts=2024-01-01T00:05:00Z&to_ts=2024-01-01T00:15:00Z")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "in-window-log" in body
        assert "too-early-log" not in body

    async def test_errors_time_window_filter(self, client):
        await client.post(
            "/v1/errors",
            json={
                "service": "tw-errors",
                "type": "RuntimeError",
                "message": "too-early-error",
                "timestamp": "2024-01-01T00:00:00Z",
            },
        )
        await client.post(
            "/v1/errors",
            json={
                "service": "tw-errors",
                "type": "RuntimeError",
                "message": "in-window-error",
                "timestamp": "2024-01-01T00:10:00Z",
            },
        )

        r = await client.get("/errors?service=tw-errors&from_ts=2024-01-01T00:05:00Z&to_ts=2024-01-01T00:15:00Z")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "in-window-error" in body
        assert "too-early-error" not in body

    async def test_traces_time_window_filter(self, client):
        trace_payload = {
            "resourceSpans": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "tw-traces"}}]},
                    "scopeSpans": [
                        {
                            "spans": [
                                {
                                    "traceId": "11111111111111111111111111111111",
                                    "spanId": "aaaaaaaaaaaaaaaa",
                                    "parentSpanId": "",
                                    "name": "too-early-span",
                                    "startTimeUnixNano": str(1704067200 * 1_000_000_000),
                                    "endTimeUnixNano": str(1704067205 * 1_000_000_000),
                                    "status": {"code": 1},
                                    "attributes": [],
                                },
                                {
                                    "traceId": "22222222222222222222222222222222",
                                    "spanId": "bbbbbbbbbbbbbbbb",
                                    "parentSpanId": "",
                                    "name": "in-window-span",
                                    "startTimeUnixNano": str(1704067800 * 1_000_000_000),
                                    "endTimeUnixNano": str(1704067805 * 1_000_000_000),
                                    "status": {"code": 1},
                                    "attributes": [],
                                },
                            ]
                        }
                    ],
                }
            ]
        }
        await client.post("/v1/traces", json=trace_payload)

        r = await client.get("/traces?service=tw-traces&from_ts=2024-01-01T00:05:00Z&to_ts=2024-01-01T00:15:00Z")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "in-window-span" in body
        assert "too-early-span" not in body

    async def test_rum_page(self, client):
        r = await client.get("/rum")
        assert r.status_code == 200

    async def test_ai_page(self, client):
        r = await client.get("/ai")
        assert r.status_code == 200

    async def test_first_run_tour_modal_present(self, client):
        r = await client.get("/")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"firstRunTourModal" in data
        assert b"Quick Tour" in data

    async def test_chart_editor_help_page(self, client):
        r = await client.get("/dashboards/help/chart-editor")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"Chart Editor Help" in data
        assert b"Custom ECharts" in data

    async def test_auto_metrics_rules_help_page(self, client):
        r = await client.get("/metrics/help/rules/auto")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"Auto Make Metric Rules Help" in data
        assert b"Preview First" in data

    async def test_rum_js_served(self, client):
        r = await client.get("/static/rum.js")
        assert r.status_code == 200
        assert b"SOBS" in await r.get_data()

    async def test_pagination(self, client):
        r = await client.get("/logs?limit=10&offset=0")
        assert r.status_code == 200

    async def test_logs_sort_by_level(self, client):
        r = await client.get("/logs?sort_by=SeverityText&sort_dir=asc")
        assert r.status_code == 200

    async def test_logs_sort_by_service_desc(self, client):
        r = await client.get("/logs?sort_by=ServiceName&sort_dir=desc")
        assert r.status_code == 200

    async def test_logs_stats_panel_visible(self, client):
        """Query statistics panel should appear when logs exist."""
        await client.post(
            "/v1/logs",
            json={
                "resourceLogs": [
                    {
                        "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "stats-svc"}}]},
                        "scopeLogs": [
                            {
                                "logRecords": [
                                    {
                                        "timeUnixNano": str(int(time.time() * 1_000_000_000)),
                                        "severityText": "ERROR",
                                        "body": {"stringValue": "stats panel test error"},
                                    }
                                ]
                            }
                        ],
                    }
                ]
            },
        )
        r = await client.get("/logs")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"Query Statistics" in data
        assert b"By Level" in data
        assert b"By Service" in data
        assert b'id="statsPanel" class="accordion-collapse collapse"' in data
        assert b"Snapshot:" in data

    async def test_logs_stats_panel_sql_mode(self, client):
        """Query statistics panel should appear when using SQL WHERE filter."""
        r = await client.get("/logs?sql=SeverityText%3D%27ERROR%27")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"Query Statistics" in data

    async def test_logs_stats_chips_are_clickable_filters(self, client):
        """Stats chips should be drill-down links for level and service filters."""
        now_ns = str(int(time.time() * 1_000_000_000))
        payload = {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "chip-svc"}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": now_ns,
                                    "severityText": "ERROR",
                                    "body": {"stringValue": "chip level test"},
                                }
                            ]
                        }
                    ],
                }
            ]
        }
        await client.post("/v1/logs", json=payload)

        r = await client.get("/logs?stats=1&service=chip-svc")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"level=ERROR" in data
        assert b"service=chip-svc" in data

    async def test_logs_stats_chip_click_exits_sql_mode(self, client):
        """Chip links should clear SQL mode to apply deterministic standard filters."""
        r = await client.get("/logs?sql=SeverityText%3D%27ERROR%27&stats=1")
        assert r.status_code == 200
        data = await r.get_data()
        # Chip links intentionally include sql='' to exit SQL mode.
        assert b"sql=&" in data

    async def test_logs_stats_are_query_scoped_not_page_scoped(self, client):
        """Stats should summarize full query result set, not just current page."""
        now_ns = str(int(time.time() * 1_000_000_000))
        payload = {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "query-scope"}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": now_ns,
                                    "severityText": "ERROR",
                                    "body": {"stringValue": "query scoped error A"},
                                },
                                {
                                    "timeUnixNano": str(int(now_ns) + 1),
                                    "severityText": "ERROR",
                                    "body": {"stringValue": "query scoped error B"},
                                },
                            ]
                        }
                    ],
                }
            ]
        }
        await client.post("/v1/logs", json=payload)

        r = await client.get("/logs?service=query-scope&limit=1&offset=0")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"2 total records" in data
        assert b"ERROR: 2" in data

    async def test_logs_stats_respect_grep_query_scope(self, client):
        """When grep is used, stats should reflect all grep-matching rows across pages."""
        now_ns = int(time.time() * 1_000_000_000)
        payload = {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "grep-scope"}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": str(now_ns),
                                    "severityText": "WARN",
                                    "body": {"stringValue": "cache timeout after 5000 ms"},
                                },
                                {
                                    "timeUnixNano": str(now_ns + 1),
                                    "severityText": "WARN",
                                    "body": {"stringValue": "cache timeout after 7000 ms"},
                                },
                                {
                                    "timeUnixNano": str(now_ns + 2),
                                    "severityText": "INFO",
                                    "body": {"stringValue": "health check passed"},
                                },
                            ]
                        }
                    ],
                }
            ]
        }
        await client.post("/v1/logs", json=payload)

        r = await client.get("/logs?service=grep-scope&q=cache%20timeout&limit=1&offset=0")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"2 total records" in data
        assert b"WARN: 2" in data

    async def test_logs_sql_and_grep_query_scope(self, client):
        """SQL WHERE + grep should both participate in query-scoped stats and totals."""
        now_ns = int(time.time() * 1_000_000_000)
        payload = {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "sqlgrep"}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": str(now_ns),
                                    "severityText": "ERROR",
                                    "body": {"stringValue": "queue backlog marker-xyz"},
                                },
                                {
                                    "timeUnixNano": str(now_ns + 1),
                                    "severityText": "ERROR",
                                    "body": {"stringValue": "queue backlog marker-xyz"},
                                },
                                {
                                    "timeUnixNano": str(now_ns + 2),
                                    "severityText": "ERROR",
                                    "body": {"stringValue": "queue backlog without marker"},
                                },
                            ]
                        }
                    ],
                }
            ]
        }
        await client.post("/v1/logs", json=payload)

        r = await client.get("/logs?sql=service%3D%27sqlgrep%27&q=marker-xyz&limit=1&offset=0")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"2 total records" in data
        assert b"ERROR: 2" in data

    async def test_logs_invalid_regex_query_returns_error_message(self, client):
        """Invalid regex should return an error message and not raise."""
        r = await client.get("/logs?q=([)")
        assert r.status_code == 200
        assert b"Regex error" in await r.get_data()

    async def test_logs_advanced_analysis_manual_trigger(self, client):
        """Advanced message analysis should render only after manual trigger."""
        now_ns = int(time.time() * 1_000_000_000)
        payload = {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "adv-scope"}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": str(now_ns),
                                    "severityText": "ERROR",
                                    "body": {"stringValue": "DatabaseTimeoutError while calling postgres"},
                                },
                                {
                                    "timeUnixNano": str(now_ns + 1),
                                    "severityText": "ERROR",
                                    "body": {"stringValue": "DatabaseTimeoutError while calling postgres"},
                                },
                                {
                                    "timeUnixNano": str(now_ns + 2),
                                    "severityText": "ERROR",
                                    "body": {"stringValue": "DatabaseTimeoutError while calling postgres"},
                                },
                            ]
                        }
                    ],
                }
            ]
        }
        await client.post("/v1/logs", json=payload)

        baseline = await client.get("/logs?service=adv-scope")
        assert baseline.status_code == 200
        assert b"Top Message Patterns" not in await baseline.get_data()

        analyzed = await client.get("/logs?service=adv-scope&analyze=1&stats=1&stats_updated=1")
        assert analyzed.status_code == 200
        analyzed_data = await analyzed.get_data()
        assert b"Top Message Patterns" in analyzed_data
        assert b"Error Families" in analyzed_data
        assert b"Top Keywords" in analyzed_data
        assert b"Optimization Hints" in analyzed_data
        assert b'id="statsPanel" class="accordion-collapse collapse show"' in analyzed_data
        assert b"stats-panel-updated" in analyzed_data

    async def test_logs_stats_snapshot_live_mode_hint(self, client):
        """When live mode is on, stats panel should show snapshot staleness guidance."""
        await client.post(
            "/v1/logs",
            json={
                "resourceLogs": [
                    {
                        "resource": {
                            "attributes": [{"key": "service.name", "value": {"stringValue": "live-hint-svc"}}]
                        },
                        "scopeLogs": [
                            {
                                "logRecords": [
                                    {
                                        "timeUnixNano": str(int(time.time() * 1_000_000_000)),
                                        "severityText": "INFO",
                                        "body": {"stringValue": "live hint sample"},
                                    }
                                ]
                            }
                        ],
                    }
                ]
            },
        )
        r = await client.get("/logs?live=1&stats=1")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"Snapshot:" in data
        assert b"Live stream active. Stats remain snapshot-based until refreshed." in data

    async def test_logs_advanced_analysis_uses_exception_type_for_families(self, client):
        """Error families should include structured exception.type values from log attributes."""
        await client.post(
            "/v1/errors",
            json={
                "service": "example-err-svc",
                "type": "RuntimeError",
                "message": "simulated error without family token",
                "stack": "Traceback line",
            },
        )

        analyzed = await client.get("/logs?service=example-err-svc&analyze=1&stats=1")
        assert analyzed.status_code == 200
        data = await analyzed.get_data()
        assert b"Error Families" in data
        assert b"RuntimeError" in data

    async def test_logs_invalid_sort_ignored(self, client):
        """An unknown sort_by value should fall back to Timestamp without error."""
        r = await client.get("/logs?sort_by=INVALID_COL&sort_dir=desc")
        assert r.status_code == 200

    async def test_errors_sort_by_service(self, client):
        r = await client.get("/errors?sort_by=ServiceName&sort_dir=asc")
        assert r.status_code == 200

    async def test_errors_page_size(self, client):
        r = await client.get("/errors?limit=25&offset=0")
        assert r.status_code == 200

    async def test_traces_sort_by_duration(self, client):
        r = await client.get("/traces?sort_by=Duration&sort_dir=desc")
        assert r.status_code == 200

    async def test_traces_sort_by_name(self, client):
        r = await client.get("/traces?sort_by=SpanName&sort_dir=asc")
        assert r.status_code == 200

    async def test_traces_page_size(self, client):
        r = await client.get("/traces?limit=50&offset=0")
        assert r.status_code == 200

    async def test_trace_detail_hierarchical_view(self, client):
        """When a trace_id filter is provided the response renders the hierarchical tree view."""
        from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
        from opentelemetry.proto.common.v1.common_pb2 import AnyValue, KeyValue
        from opentelemetry.proto.resource.v1.resource_pb2 import Resource
        from opentelemetry.proto.trace.v1.trace_pb2 import ResourceSpans, ScopeSpans, Span, Status

        trace_id_bytes = bytes.fromhex("aabbccddeeff00112233445566778800")
        parent_span_bytes = bytes.fromhex("1111111111111100")
        child_span_bytes = bytes.fromhex("2222222222222200")
        start_ns = 1704067200_000_000_000

        parent_span = Span(
            trace_id=trace_id_bytes,
            span_id=parent_span_bytes,
            name="root-span",
            start_time_unix_nano=start_ns,
            end_time_unix_nano=start_ns + 2_000_000_000,
            status=Status(code=1),
        )
        child_span = Span(
            trace_id=trace_id_bytes,
            span_id=child_span_bytes,
            parent_span_id=parent_span_bytes,
            name="child-span",
            start_time_unix_nano=start_ns + 500_000_000,
            end_time_unix_nano=start_ns + 1_500_000_000,
            status=Status(code=1),
        )
        resource = Resource(attributes=[KeyValue(key="service.name", value=AnyValue(string_value="detail-svc"))])
        msg = ExportTraceServiceRequest(
            resource_spans=[
                ResourceSpans(
                    resource=resource,
                    scope_spans=[ScopeSpans(spans=[parent_span, child_span])],
                )
            ]
        )
        r = await client.post(
            "/v1/traces", data=msg.SerializeToString(), headers={"Content-Type": "application/x-protobuf"}
        )
        assert r.status_code == 200

        trace_id_hex = trace_id_bytes.hex()
        r = await client.get(f"/traces?trace_id={trace_id_hex}")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        # Tree rows and span names rendered
        assert "trace-tree-row" in body
        assert "root-span" in body
        assert "child-span" in body
        # Anomaly/metrics tags present
        assert "normal" in body or "outlier" in body or "error" in body
        # JavaScript for tree toggle included
        assert "traceTree" in body

    async def test_trace_detail_with_error_span(self, client):
        """An ERROR span is highlighted with an error tag in the trace detail view."""
        from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
        from opentelemetry.proto.common.v1.common_pb2 import AnyValue, KeyValue
        from opentelemetry.proto.resource.v1.resource_pb2 import Resource
        from opentelemetry.proto.trace.v1.trace_pb2 import ResourceSpans, ScopeSpans, Span, Status

        trace_id_bytes = bytes.fromhex("ffee00112233445566778899aabbcc00")
        span_bytes = bytes.fromhex("aaaa111100000000")
        start_ns = 1704067200_000_000_000

        err_span = Span(
            trace_id=trace_id_bytes,
            span_id=span_bytes,
            name="failing-span",
            start_time_unix_nano=start_ns,
            end_time_unix_nano=start_ns + 1_000_000_000,
            status=Status(code=2, message="something went wrong"),
            attributes=[
                KeyValue(key="exception.type", value=AnyValue(string_value="ValueError")),
                KeyValue(key="exception.message", value=AnyValue(string_value="bad value")),
            ],
        )
        resource = Resource(attributes=[KeyValue(key="service.name", value=AnyValue(string_value="err-trace-svc"))])
        msg = ExportTraceServiceRequest(
            resource_spans=[ResourceSpans(resource=resource, scope_spans=[ScopeSpans(spans=[err_span])])]
        )
        r = await client.post(
            "/v1/traces", data=msg.SerializeToString(), headers={"Content-Type": "application/x-protobuf"}
        )
        assert r.status_code == 200

        trace_id_hex = trace_id_bytes.hex()
        r = await client.get(f"/traces?trace_id={trace_id_hex}")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "failing-span" in body
        assert "error" in body.lower()

    async def test_trace_detail_back_link(self, client):
        """The detail view includes a link back to the full traces list."""
        from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
        from opentelemetry.proto.common.v1.common_pb2 import AnyValue, KeyValue
        from opentelemetry.proto.resource.v1.resource_pb2 import Resource
        from opentelemetry.proto.trace.v1.trace_pb2 import ResourceSpans, ScopeSpans, Span, Status

        trace_id_bytes = bytes.fromhex("deadbeefdeadbeef0011223344550000")
        span_bytes = bytes.fromhex("cccc222200000000")
        start_ns = 1704067200_000_000_000

        span = Span(
            trace_id=trace_id_bytes,
            span_id=span_bytes,
            name="some-op",
            start_time_unix_nano=start_ns,
            end_time_unix_nano=start_ns + 1_000_000_000,
            status=Status(code=1),
        )
        resource = Resource(attributes=[KeyValue(key="service.name", value=AnyValue(string_value="back-link-svc"))])
        msg = ExportTraceServiceRequest(
            resource_spans=[ResourceSpans(resource=resource, scope_spans=[ScopeSpans(spans=[span])])]
        )
        r = await client.post(
            "/v1/traces", data=msg.SerializeToString(), headers={"Content-Type": "application/x-protobuf"}
        )
        assert r.status_code == 200

        trace_id_hex = trace_id_bytes.hex()
        r = await client.get(f"/traces?trace_id={trace_id_hex}")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "All Traces" in body

    async def test_rum_sort_by_type(self, client):
        r = await client.get("/rum?sort_by=EventName&sort_dir=asc")
        assert r.status_code == 200

    async def test_rum_page_size(self, client):
        r = await client.get("/rum?limit=25&offset=0")
        assert r.status_code == 200

    async def test_ai_sort_by_duration(self, client):
        r = await client.get("/ai?sort_by=Duration&sort_dir=desc")
        assert r.status_code == 200

    async def test_ai_page_size(self, client):
        r = await client.get("/ai?limit=25&offset=0")
        assert r.status_code == 200

    async def test_sql_error_handled(self, client):
        """Bad SQL should return an error message, not a 500."""
        r = await client.get("/logs?sql=INVALID+SQL+))))")
        assert r.status_code == 200
        assert b"SQL error" in await r.get_data()

    async def test_root_mode_uses_root_relative_links(self, client):
        """Default deployment should generate links/assets without a path prefix."""
        r = await client.get("/")
        assert r.status_code == 200
        assert b'href="/logs"' in await r.get_data()
        assert b'href="/errors"' in await r.get_data()
        assert b'src="/static/bootstrap.bundle.min.js"' in await r.get_data()


class TestBasePathSupport:
    async def test_prefixed_mode_routes_and_generates_prefixed_links(self, monkeypatch):
        """When SOBS base path is configured, both routing and generated links should honor it."""
        import app as app_module

        monkeypatch.setattr(app_module, "BASE_PATH", "/sobs")
        app.config["TESTING"] = True

        async with app.test_client() as c:
            dashboard = await c.get("/sobs/")
            assert dashboard.status_code == 200
            assert b'href="/sobs/logs"' in await dashboard.get_data()
            assert b'href="/sobs/errors"' in await dashboard.get_data()
            assert b'src="/sobs/static/bootstrap.bundle.min.js"' in await dashboard.get_data()

            logs_ingest = await c.post("/sobs/v1/logs", json={})
            assert logs_ingest.status_code == 200

            rum_script = await c.get("/sobs/static/rum.js")
            assert rum_script.status_code == 200

    async def test_forwarded_prefix_generates_prefixed_links(self, client):
        """X-Forwarded-Prefix should influence generated links even when backend paths are unprefixed."""
        r = await client.get("/", headers={"X-Forwarded-Prefix": "/sobs"})
        assert r.status_code == 200
        assert b'href="/sobs/logs"' in await r.get_data()
        assert b'href="/sobs/errors"' in await r.get_data()
        assert b'src="/sobs/static/bootstrap.bundle.min.js"' in await r.get_data()


# ---------------------------------------------------------------------------
# Basic Auth
# ---------------------------------------------------------------------------
class TestBasicAuth:
    """Tests for optional Basic Auth on Web UI routes."""

    _TEST_USER = "admin"
    _TEST_PASS = "secret"

    def _auth_header(self, username=None, password=None):
        u = username if username is not None else self._TEST_USER
        p = password if password is not None else self._TEST_PASS
        token = base64.b64encode(f"{u}:{p}".encode()).decode()
        return {"Authorization": f"Basic {token}"}

    @pytest.fixture
    async def authed_client(self, monkeypatch):
        """Client with Basic Auth enabled via env vars."""
        import app as app_module

        monkeypatch.setattr(app_module, "BASIC_AUTH_USERNAME", self._TEST_USER)
        monkeypatch.setattr(app_module, "BASIC_AUTH_PASSWORD", self._TEST_PASS)
        app.config["TESTING"] = True
        async with app.test_client() as c:
            yield c

    async def test_ui_requires_auth_when_configured(self, authed_client):
        """Web UI should return 401 when Basic Auth is configured and no credentials sent."""
        r = await authed_client.get("/")
        assert r.status_code == 401
        assert r.headers.get("WWW-Authenticate") == 'Basic realm="SOBS"'

    async def test_ui_accessible_with_correct_credentials(self, authed_client):
        """Web UI should be accessible with correct Basic Auth credentials."""
        r = await authed_client.get("/", headers=self._auth_header())
        assert r.status_code == 200

    async def test_ui_rejects_wrong_password(self, authed_client):
        """Web UI should return 401 when password is wrong."""
        r = await authed_client.get("/", headers=self._auth_header(password="wrong"))
        assert r.status_code == 401

    async def test_ui_rejects_wrong_username(self, authed_client):
        """Web UI should return 401 when username is wrong."""
        r = await authed_client.get("/", headers=self._auth_header(username="nobody"))
        assert r.status_code == 401

    async def test_ui_no_auth_without_config(self, client):
        """Web UI should be freely accessible when Basic Auth is not configured."""
        r = await client.get("/")
        assert r.status_code == 200

    async def test_all_ui_routes_protected(self, authed_client):
        """All Web UI routes should require auth when Basic Auth is configured."""
        ui_routes = ["/", "/logs", "/errors", "/traces", "/rum", "/ai"]
        for route in ui_routes:
            r = await authed_client.get(route)
            assert r.status_code == 401, f"Expected 401 for {route}, got {r.status_code}"

    async def test_api_endpoints_unaffected(self, authed_client):
        """Ingest API endpoints (/v1/*) should not be gated by Basic Auth."""
        r = await authed_client.post("/v1/logs", json={})
        assert r.status_code == 200

    async def test_health_endpoint_unaffected(self, authed_client):
        """/health should remain accessible regardless of Basic Auth config."""
        r = await authed_client.get("/health")
        assert r.status_code == 200

    async def test_health_db_endpoint_unaffected(self, authed_client):
        """/health/db should remain accessible regardless of Basic Auth config."""
        r = await authed_client.get("/health/db")
        assert r.status_code == 200

    async def test_partial_basic_auth_config_is_error(self, monkeypatch):
        """Supplying only one Basic Auth credential should be treated as misconfiguration."""
        import app as app_module

        monkeypatch.setattr(app_module, "BASIC_AUTH_USERNAME", self._TEST_USER)
        monkeypatch.setattr(app_module, "BASIC_AUTH_PASSWORD", "")
        monkeypatch.setattr(app_module, "EXTERNAL_AUTH_URL", "")
        app.config["TESTING"] = True
        async with app.test_client() as c:
            r = await c.get("/")
        assert r.status_code == 500
        assert await r.get_json() == {"error": "Server auth misconfiguration"}


# ---------------------------------------------------------------------------
# External Auth
# ---------------------------------------------------------------------------
class TestExternalAuth:
    """Tests for optional external auth handler on Web UI routes."""

    _EXT_AUTH_URL = "http://auth-service"

    @pytest.fixture
    async def ext_auth_client(self, monkeypatch):
        """Client with external auth URL configured."""
        import app as app_module

        monkeypatch.setattr(app_module, "EXTERNAL_AUTH_URL", self._EXT_AUTH_URL)
        app.config["TESTING"] = True
        async with app.test_client() as c:
            yield c

    async def test_rejects_request_without_bearer_token(self, ext_auth_client):
        """Web UI should return 401 with Bearer challenge when external auth is configured and no token is sent."""
        r = await ext_auth_client.get("/")
        assert r.status_code == 401
        assert r.headers.get("WWW-Authenticate") == 'Bearer realm="SOBS"'

    async def test_allows_request_with_valid_bearer_token(self, ext_auth_client, monkeypatch):
        """Web UI should allow requests when external auth service approves the token."""
        import app as app_module

        monkeypatch.setattr(app_module, "_check_external_auth", lambda _auth: True)
        r = await ext_auth_client.get("/", headers={"Authorization": "Bearer valid-token"})
        assert r.status_code == 200

    async def test_rejects_request_with_invalid_bearer_token(self, ext_auth_client, monkeypatch):
        """Web UI should return 401 when external auth service rejects the token."""
        import app as app_module

        monkeypatch.setattr(app_module, "_check_external_auth", lambda _auth: False)
        r = await ext_auth_client.get("/", headers={"Authorization": "Bearer bad-token"})
        assert r.status_code == 401

    async def test_basic_and_external_together_is_error(self, monkeypatch):
        """Basic and external auth configured together should be treated as misconfiguration."""
        import app as app_module

        monkeypatch.setattr(app_module, "BASIC_AUTH_USERNAME", "admin")
        monkeypatch.setattr(app_module, "BASIC_AUTH_PASSWORD", "secret")
        monkeypatch.setattr(app_module, "EXTERNAL_AUTH_URL", self._EXT_AUTH_URL)
        app.config["TESTING"] = True
        async with app.test_client() as c:
            r = await c.get("/")
        assert r.status_code == 500
        assert await r.get_json() == {"error": "Server auth misconfiguration"}

    async def test_ingest_endpoints_unaffected_by_external_auth(self, ext_auth_client):
        """Ingest API endpoints (/v1/*) should not be gated by external auth."""
        r = await ext_auth_client.post("/v1/logs", json={})
        assert r.status_code == 200

    async def test_ui_no_auth_required_when_not_configured(self, client):
        """Web UI should be freely accessible when external auth is not configured."""
        r = await client.get("/")
        assert r.status_code == 200

    async def test_all_ui_routes_protected(self, ext_auth_client, monkeypatch):
        """All Web UI routes should require auth when external auth is configured."""
        import app as app_module

        monkeypatch.setattr(app_module, "_check_external_auth", lambda _auth: False)
        ui_routes = ["/", "/logs", "/errors", "/traces", "/rum", "/ai"]
        for route in ui_routes:
            r = await ext_auth_client.get(route, headers={"Authorization": "Bearer bad-token"})
            assert r.status_code == 401, f"Expected 401 for {route}, got {r.status_code}"

    async def test_check_external_auth_makes_correct_request(self, monkeypatch):
        """_check_external_auth should POST to /internal/auth/validate with the Authorization header."""
        import urllib.request

        import app as app_module

        monkeypatch.setattr(app_module, "EXTERNAL_AUTH_URL", self._EXT_AUTH_URL)

        captured = {}

        class _FakeResponse:
            status = 200

            def __enter__(self):
                return self

            def __exit__(self, *_):
                pass

        def fake_urlopen(req, timeout=None):
            captured["url"] = req.full_url
            captured["method"] = req.method
            captured["auth"] = req.get_header("Authorization")
            return _FakeResponse()

        monkeypatch.setattr(urllib.request, "urlopen", fake_urlopen)

        result = app_module._check_external_auth("Bearer my-token")

        assert result is True
        assert captured["url"] == self._EXT_AUTH_URL + "/internal/auth/validate"
        assert captured["method"] == "POST"
        assert captured["auth"] == "Bearer my-token"

    async def test_check_external_auth_returns_false_on_non_200(self, monkeypatch):
        """_check_external_auth should return False when the external service returns non-200."""
        import urllib.request

        import app as app_module

        monkeypatch.setattr(app_module, "EXTERNAL_AUTH_URL", self._EXT_AUTH_URL)

        class _FakeResponse:
            status = 401

            def __enter__(self):
                return self

            def __exit__(self, *_):
                pass

        monkeypatch.setattr(urllib.request, "urlopen", lambda req, timeout=None: _FakeResponse())

        assert app_module._check_external_auth("Bearer bad-token") is False

    async def test_check_external_auth_returns_false_on_network_error(self, monkeypatch):
        """_check_external_auth should return False when the external service is unreachable."""
        import urllib.request

        import app as app_module

        monkeypatch.setattr(app_module, "EXTERNAL_AUTH_URL", self._EXT_AUTH_URL)

        def _raise_network_error(req, timeout=None):
            raise OSError("unreachable")

        monkeypatch.setattr(urllib.request, "urlopen", _raise_network_error)

        assert app_module._check_external_auth("Bearer any-token") is False

    async def test_check_external_auth_returns_false_when_url_not_configured(self):
        """_check_external_auth should return False immediately when EXTERNAL_AUTH_URL is empty."""
        import app as app_module

        # EXTERNAL_AUTH_URL is empty in the default test environment
        assert app_module._check_external_auth("Bearer token") is False

    async def test_session_cookie_used_as_bearer_fallback_when_valid(self, ext_auth_client, monkeypatch):
        """When no Bearer header is present, a valid session cookie should be accepted via external auth."""
        import app as app_module

        monkeypatch.setattr(app_module, "_check_external_auth", lambda _auth: True)
        ext_auth_client.set_cookie("localhost", "session", "valid-session-token")
        r = await ext_auth_client.get("/")
        assert r.status_code == 200

    async def test_session_cookie_denied_when_validator_rejects(self, ext_auth_client, monkeypatch):
        """When session cookie is present but the external validator rejects it, return 401."""
        import app as app_module

        monkeypatch.setattr(app_module, "_check_external_auth", lambda _auth: False)
        ext_auth_client.set_cookie("localhost", "session", "invalid-session-token")
        r = await ext_auth_client.get("/")
        assert r.status_code == 401

    async def test_session_cookie_synthesizes_bearer_header(self, ext_auth_client, monkeypatch):
        """The session cookie value should be forwarded as a Bearer token to the external validator."""
        import app as app_module

        captured = {}

        def capturing_check(auth):
            captured["auth"] = auth
            return True

        monkeypatch.setattr(app_module, "_check_external_auth", capturing_check)
        ext_auth_client.set_cookie("localhost", "session", "my-session-value")
        r = await ext_auth_client.get("/")
        assert r.status_code == 200
        assert captured.get("auth") == "Bearer my-session-value"

    async def test_no_bearer_no_cookie_returns_401_with_bearer_challenge(self, ext_auth_client):
        """Requests with neither Authorization header nor session cookie should get 401 + Bearer challenge."""
        r = await ext_auth_client.get("/")
        assert r.status_code == 401
        assert r.headers.get("WWW-Authenticate") == 'Bearer realm="SOBS"'


# ---------------------------------------------------------------------------
# SSE tail endpoint
# ---------------------------------------------------------------------------
class TestSSETail:
    """Tests for the /tail SSE live-tail endpoint."""

    @staticmethod
    async def _get_streaming_response(client, path, headers=None):
        """Open a streaming HTTP connection, capture the initial response headers, then cancel.

        Because /tail is an infinite SSE stream, the standard ``await client.get()``
        would block forever.  This helper uses the underlying ``TestHTTPConnection``
        directly so we can inspect the response status/headers and cancel the task.
        """
        connection = client.request(path, headers=headers)
        await connection.__aenter__()
        try:
            await connection.send(b"")
            await connection.send_complete()
            # Allow Quart to dispatch the request and emit the response-start frame.
            await asyncio.sleep(0.05)
            return connection.status_code, connection.headers
        finally:
            if not connection._task.done():
                connection._task.cancel()
                try:
                    await connection._task
                except asyncio.CancelledError:
                    pass

    async def test_tail_returns_200_with_sse_content_type(self, client):
        """GET /tail should return 200 with text/event-stream content-type."""
        status, headers = await self._get_streaming_response(client, "/tail")
        assert status == 200
        assert headers is not None
        assert "text/event-stream" in headers.get("content-type", "")

    async def test_tail_response_has_no_cache_header(self, client):
        """GET /tail should include Cache-Control: no-cache."""
        status, headers = await self._get_streaming_response(client, "/tail")
        assert status == 200
        assert headers.get("cache-control") == "no-cache"

    async def test_tail_requires_auth_when_basic_auth_configured(self, monkeypatch):
        """GET /tail should return 401 when Basic Auth is configured and no credentials sent."""
        import app as app_module

        monkeypatch.setattr(app_module, "BASIC_AUTH_USERNAME", "admin")
        monkeypatch.setattr(app_module, "BASIC_AUTH_PASSWORD", "secret")
        app.config["TESTING"] = True
        async with app.test_client() as c:
            r = await c.get("/tail")
        assert r.status_code == 401
        assert r.headers.get("WWW-Authenticate") == 'Basic realm="SOBS"'

    async def test_tail_accessible_with_correct_basic_auth(self, monkeypatch):
        """GET /tail should be accessible with correct Basic Auth credentials."""
        import app as app_module

        monkeypatch.setattr(app_module, "BASIC_AUTH_USERNAME", "admin")
        monkeypatch.setattr(app_module, "BASIC_AUTH_PASSWORD", "secret")
        token = base64.b64encode(b"admin:secret").decode()
        app.config["TESTING"] = True
        async with app.test_client() as c:
            status, _ = await self._get_streaming_response(c, "/tail", headers={"Authorization": f"Basic {token}"})
        assert status == 200

    async def test_tail_requires_auth_when_external_auth_configured(self, monkeypatch):
        """GET /tail should return 401 when external auth is configured and no token sent."""
        import app as app_module

        monkeypatch.setattr(app_module, "EXTERNAL_AUTH_URL", "http://auth-service")
        app.config["TESTING"] = True
        async with app.test_client() as c:
            r = await c.get("/tail")
        assert r.status_code == 401
        assert r.headers.get("WWW-Authenticate") == 'Bearer realm="SOBS"'

    async def test_tail_accessible_with_valid_external_auth(self, monkeypatch):
        """GET /tail should be accessible when external auth approves the token."""
        import app as app_module

        monkeypatch.setattr(app_module, "EXTERNAL_AUTH_URL", "http://auth-service")
        monkeypatch.setattr(app_module, "_check_external_auth", lambda _auth: True)
        app.config["TESTING"] = True
        async with app.test_client() as c:
            status, _ = await self._get_streaming_response(c, "/tail", headers={"Authorization": "Bearer valid-token"})
        assert status == 200

    async def test_tail_accessible_without_auth_in_none_mode(self, client):
        """GET /tail should be accessible when no auth is configured."""
        status, _ = await self._get_streaming_response(client, "/tail")
        assert status == 200

    async def test_sse_broadcast_delivers_to_subscriber(self):
        """_sse_broadcast should put events on all registered subscriber queues."""
        import app as app_module

        q = asyncio.Queue()
        app_module._sse_subscribers.add(q)
        try:
            event = {"source": "logs", "ts": "2024-01-01T00:00:00Z", "level": "INFO", "service": "svc", "body": "hi"}
            await app_module._sse_broadcast(event)
            received = q.get_nowait()
            assert received == event
        finally:
            app_module._sse_subscribers.discard(q)

    async def test_sse_broadcast_delivers_to_multiple_subscribers(self):
        """_sse_broadcast should deliver to all registered queues simultaneously."""
        import app as app_module

        q1 = asyncio.Queue()
        q2 = asyncio.Queue()
        app_module._sse_subscribers.add(q1)
        app_module._sse_subscribers.add(q2)
        try:
            event = {"source": "logs", "ts": "t", "level": "INFO", "service": "s", "body": "b"}
            await app_module._sse_broadcast(event)
            assert q1.get_nowait() == event
            assert q2.get_nowait() == event
        finally:
            app_module._sse_subscribers.discard(q1)
            app_module._sse_subscribers.discard(q2)

    async def test_sse_broadcast_drops_on_full_queue(self):
        """_sse_broadcast should silently drop events when a subscriber queue is full."""
        import app as app_module

        q = asyncio.Queue(maxsize=1)
        q.put_nowait({"source": "logs"})  # fill it up
        app_module._sse_subscribers.add(q)
        try:
            # Should not raise even though the queue is full
            await app_module._sse_broadcast({"source": "logs", "ts": "x"})
            assert q.qsize() == 1  # still only the original event
        finally:
            app_module._sse_subscribers.discard(q)

    async def test_ingest_logs_broadcasts_to_sse_subscribers(self, client):
        """Posting to /v1/logs should result in log events delivered to SSE subscribers."""
        import app as app_module

        q = asyncio.Queue()
        app_module._sse_subscribers.add(q)
        try:
            r = await client.post(
                "/v1/logs",
                json={
                    "resourceLogs": [
                        {
                            "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "testsvc"}}]},
                            "scopeLogs": [
                                {
                                    "logRecords": [
                                        {
                                            "timeUnixNano": "1700000000000000000",
                                            "severityText": "INFO",
                                            "body": {"stringValue": "hello sse"},
                                        }
                                    ]
                                }
                            ],
                        }
                    ]
                },
            )
            assert r.status_code == 200
            event = q.get_nowait()
            assert event["source"] == "logs"
            assert event["service"] == "testsvc"
            assert event["body"] == "hello sse"
            assert event["level"] == "INFO"
            assert "ts" in event
            assert "trace_id" in event
        finally:
            app_module._sse_subscribers.discard(q)

    async def test_ingest_traces_broadcasts_to_sse_subscribers(self, client):
        """Posting to /v1/traces should result in span events delivered to SSE subscribers."""
        import app as app_module

        q = asyncio.Queue()
        app_module._sse_subscribers.add(q)
        try:
            r = await client.post(
                "/v1/traces",
                json={
                    "resourceSpans": [
                        {
                            "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "tracesvc"}}]},
                            "scopeSpans": [
                                {
                                    "spans": [
                                        {
                                            "traceId": "aabbccdd" * 4,
                                            "spanId": "11223344" * 2,
                                            "name": "GET /api",
                                            "startTimeUnixNano": "1700000000000000000",
                                            "endTimeUnixNano": "1700000001000000000",
                                            "status": {"code": 1},
                                        }
                                    ]
                                }
                            ],
                        }
                    ]
                },
            )
            assert r.status_code == 200
            event = q.get_nowait()
            assert event["source"] == "traces"
            assert event["service"] == "tracesvc"
            assert event["name"] == "GET /api"
            assert "ts" in event
            assert "duration_ms" in event
            assert "status" in event
        finally:
            app_module._sse_subscribers.discard(q)

    async def test_tail_source_and_service_filtering_logic(self):
        """Filtering by source and service should work as expected by the generator logic."""
        all_events = [
            {"source": "logs", "ts": "t1", "level": "INFO", "service": "myapp", "body": "a"},
            {"source": "traces", "ts": "t2", "name": "span", "service": "myapp"},
            {"source": "logs", "ts": "t3", "level": "INFO", "service": "other", "body": "b"},
        ]

        # Filter source=logs only
        logs_only = [e for e in all_events if e.get("source") == "logs"]
        assert len(logs_only) == 2

        # Filter source=traces only
        traces_only = [e for e in all_events if e.get("source") == "traces"]
        assert len(traces_only) == 1

        # Filter source=all, service=myapp
        myapp = [e for e in all_events if e.get("service") == "myapp"]
        assert len(myapp) == 2

        # Filter source=logs, service=myapp
        combined = [e for e in all_events if e.get("source") == "logs" and e.get("service") == "myapp"]
        assert len(combined) == 1
        assert combined[0]["body"] == "a"

    async def test_tail_source_ai_filter_logic(self):
        """source=ai filter should only pass events with source='ai'."""
        all_events = [
            {"source": "ai", "ts": "t1", "provider": "openai", "model": "gpt-4o", "service": "svc"},
            {"source": "traces", "ts": "t2", "name": "span", "service": "svc"},
            {"source": "logs", "ts": "t3", "level": "INFO", "service": "svc", "body": "msg"},
            {"source": "ai", "ts": "t4", "provider": "anthropic", "model": "claude-3", "service": "other"},
        ]

        # Filter source=ai only
        ai_only = [e for e in all_events if e.get("source") == "ai"]
        assert len(ai_only) == 2
        assert all(e["source"] == "ai" for e in ai_only)

        # Filter source=ai, service=svc
        ai_svc = [e for e in all_events if e.get("source") == "ai" and e.get("service") == "svc"]
        assert len(ai_svc) == 1
        assert ai_svc[0]["model"] == "gpt-4o"

    async def test_ingest_ai_span_via_traces_broadcasts_ai_sse_event(self, client):
        """Posting an AI span via /v1/traces should broadcast a source=ai SSE event."""
        import app as app_module

        q = asyncio.Queue()
        app_module._sse_subscribers.add(q)
        try:
            r = await client.post(
                "/v1/traces",
                json={
                    "resourceSpans": [
                        {
                            "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "llm-svc"}}]},
                            "scopeSpans": [
                                {
                                    "spans": [
                                        {
                                            "traceId": "aabbccdd" * 4,
                                            "spanId": "11223344" * 2,
                                            "name": "chat gpt-4o",
                                            "startTimeUnixNano": "1700000000000000000",
                                            "endTimeUnixNano": "1700000001000000000",
                                            "status": {"code": 1},
                                            "attributes": [
                                                {
                                                    "key": "gen_ai.provider.name",
                                                    "value": {"stringValue": "openai"},
                                                },
                                                {
                                                    "key": "gen_ai.request.model",
                                                    "value": {"stringValue": "gpt-4o"},
                                                },
                                            ],
                                        }
                                    ]
                                }
                            ],
                        }
                    ]
                },
            )
            assert r.status_code == 200
            # Drain the queue – we expect both a traces and an ai event
            events = []
            while not q.empty():
                events.append(q.get_nowait())
            sources = {e["source"] for e in events}
            assert "traces" in sources
            assert "ai" in sources
            ai_events = [e for e in events if e["source"] == "ai"]
            assert ai_events[0]["provider"] == "openai"
            assert ai_events[0]["model"] == "gpt-4o"
            assert ai_events[0]["service"] == "llm-svc"
        finally:
            app_module._sse_subscribers.discard(q)


# ---------------------------------------------------------------------------
# GenAI OTel semantic convention compliance
# ---------------------------------------------------------------------------
class TestGenAICompliance:
    """Tests for OTel GenAI semantic convention compliance in queries and UI."""

    async def test_gen_ai_system_legacy_fallback_counts_as_ai_span(self, client):
        """Spans with gen_ai.system (legacy) should be counted as AI spans."""
        import app as app_module

        # Insert a span directly using legacy gen_ai.system attribute
        db = app_module.get_db()
        app_module._insert_rows_json_each_row(
            db,
            "otel_traces",
            [
                {
                    "Timestamp": "2024-01-01T00:00:00",
                    "TraceId": "legacy01" * 4,
                    "SpanId": "legspan1" * 2,
                    "ParentSpanId": "",
                    "TraceState": "",
                    "SpanName": "chat gpt-3.5-turbo",
                    "SpanKind": "CLIENT",
                    "ServiceName": "legacy-svc",
                    "ResourceAttributes": {},
                    "ScopeName": "test",
                    "ScopeVersion": "",
                    "SpanAttributes": {
                        "gen_ai.system": "openai",
                        "gen_ai.request.model": "gpt-3.5-turbo",
                        "gen_ai.usage.input_tokens": "10",
                        "gen_ai.usage.output_tokens": "5",
                    },
                    "Duration": 100000000,
                    "StatusCode": "STATUS_CODE_OK",
                    "StatusMessage": "",
                    "Events": {"Timestamp": [], "Name": [], "Attributes": []},
                    "Links": {"TraceId": [], "SpanId": [], "TraceState": [], "Attributes": []},
                }
            ],
        )

        r = await client.get("/ai")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        # The legacy span should appear in the AI view
        assert "legacy-svc" in body or "gpt-3.5-turbo" in body

    async def test_gen_ai_input_output_messages_displayed_in_ai_view(self, client):
        """gen_ai.input.messages / gen_ai.output.messages should be shown as prompt/response."""
        r = await client.post(
            "/v1/ai",
            json={
                "service": "msg-svc",
                "provider": "openai",
                "model": "gpt-4o",
                "input_messages": [{"role": "user", "content": "What is the capital of France?"}],
                "output_messages": [{"role": "assistant", "content": "Paris"}],
                "tokens_in": 10,
                "tokens_out": 2,
                "duration_ms": 300,
            },
        )
        assert r.status_code == 200

        r2 = await client.get("/ai")
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        assert "What is the capital of France?" in body
        assert "Paris" in body

    async def test_extract_messages_text_helper(self):
        """_extract_messages_text should handle standard OTel message format."""
        import app as app_module

        # Standard message array
        msgs = json.dumps([{"role": "user", "content": "Hello"}, {"role": "assistant", "content": "Hi"}])
        result = app_module._extract_messages_text(msgs)
        assert "Hello" in result
        assert "Hi" in result
        assert "[user]" in result
        assert "[assistant]" in result

        # Empty / non-JSON passthrough
        assert app_module._extract_messages_text("") == ""
        assert app_module._extract_messages_text("plain text") == "plain text"

        # Content array (vision API style)
        msgs2 = json.dumps([{"role": "user", "content": [{"type": "text", "text": "Describe this"}]}])
        result2 = app_module._extract_messages_text(msgs2)
        assert "Describe this" in result2

    async def test_ai_view_shows_error_type(self, client):
        """AI view should display error.type badge when present."""
        r = await client.post(
            "/v1/ai",
            json={
                "service": "err-svc",
                "provider": "openai",
                "model": "gpt-4o",
                "error_type": "RateLimitError",
                "tokens_in": 5,
                "tokens_out": 0,
                "duration_ms": 50,
            },
        )
        assert r.status_code == 200

        r2 = await client.get("/ai")
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        assert "RateLimitError" in body

    async def test_ai_view_operation_filter(self, client):
        """AI view should accept and apply an operation filter."""
        r = await client.post(
            "/v1/ai",
            json={
                "service": "embed-svc",
                "provider": "openai",
                "model": "text-embedding-3-small",
                "operation": "embeddings",
                "tokens_in": 20,
                "tokens_out": 0,
                "duration_ms": 80,
            },
        )
        assert r.status_code == 200

        r2 = await client.get("/ai?operation=embeddings")
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        assert "embed-svc" in body or "text-embedding-3-small" in body

    async def test_ai_view_exposes_operation_field(self, client):
        """AI view should expose operation field in the rendered page."""
        r = await client.post(
            "/v1/ai",
            json={
                "service": "op-svc",
                "provider": "anthropic",
                "model": "claude-3-5-sonnet",
                "operation": "chat",
                "input_messages": [{"role": "user", "content": "Hello operation test"}],
                "output_messages": [{"role": "assistant", "content": "Hi"}],
                "tokens_in": 10,
                "tokens_out": 5,
                "duration_ms": 200,
            },
        )
        assert r.status_code == 200

        r2 = await client.get("/ai")
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        # Operation data should be included in the rendered output
        assert "op-svc" in body
        assert "Operation:</strong> chat" in body

    async def test_ai_view_chat_operation_filter(self, client):
        """AI view chat filter should include chat calls and exclude non-chat calls."""
        r_chat = await client.post(
            "/v1/ai",
            json={
                "service": "chat-filter-svc",
                "provider": "openai",
                "model": "gpt-4o-mini",
                "operation": "chat",
                "prompt": "chat op",
                "response": "ok",
                "tokens_in": 6,
                "tokens_out": 2,
                "duration_ms": 70,
            },
        )
        assert r_chat.status_code == 200

        r_embed = await client.post(
            "/v1/ai",
            json={
                "service": "embed-filter-svc",
                "provider": "openai",
                "model": "text-embedding-3-small",
                "operation": "embeddings",
                "tokens_in": 20,
                "tokens_out": 0,
                "duration_ms": 80,
            },
        )
        assert r_embed.status_code == 200

        r2 = await client.get("/ai?operation=chat")
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        assert "<strong>Service:</strong> chat-filter-svc" in body
        assert "<strong>Service:</strong> embed-filter-svc" not in body

    async def test_ai_view_trace_group_mode_groups_calls(self, client):
        """AI view trace mode should group multiple calls sharing a trace id."""
        trace_id = "trace-group-abc123"
        r1 = await client.post(
            "/v1/ai",
            json={
                "trace_id": trace_id,
                "service": "trace-svc-a",
                "provider": "openai",
                "model": "gpt-4o-mini",
                "operation": "chat",
                "prompt": "turn 1",
                "response": "ok 1",
                "tokens_in": 10,
                "tokens_out": 4,
                "duration_ms": 120,
            },
        )
        assert r1.status_code == 200

        r2 = await client.post(
            "/v1/ai",
            json={
                "trace_id": trace_id,
                "service": "trace-svc-b",
                "provider": "openai",
                "model": "gpt-4o-mini",
                "operation": "embeddings",
                "tokens_in": 20,
                "tokens_out": 0,
                "duration_ms": 80,
            },
        )
        assert r2.status_code == 200

        r3 = await client.get("/ai?view=trace")
        assert r3.status_code == 200
        body = await r3.get_data(as_text=True)
        assert "Trace Groups" in body
        assert "trace-svc-a" in body
        assert "trace-svc-b" in body
        assert "2 calls" in body

    async def test_ai_view_trace_mode_operation_filter_keeps_trace_context(self, client):
        """Trace mode operation filter should still show other GenAI calls within matching traces."""
        trace_id = "trace-group-filter-xyz"
        r1 = await client.post(
            "/v1/ai",
            json={
                "trace_id": trace_id,
                "service": "trace-chat-svc",
                "provider": "openai",
                "model": "gpt-4o-mini",
                "operation": "chat",
                "prompt": "chat turn",
                "response": "ok",
                "tokens_in": 8,
                "tokens_out": 3,
                "duration_ms": 90,
            },
        )
        assert r1.status_code == 200

        r2 = await client.post(
            "/v1/ai",
            json={
                "trace_id": trace_id,
                "service": "trace-embed-svc",
                "provider": "openai",
                "model": "text-embedding-3-small",
                "operation": "embeddings",
                "tokens_in": 22,
                "tokens_out": 0,
                "duration_ms": 75,
            },
        )
        assert r2.status_code == 200

        r3 = await client.get("/ai?view=trace&operation=chat")
        assert r3.status_code == 200
        body = await r3.get_data(as_text=True)
        assert "trace-chat-svc" in body
        assert "trace-embed-svc" in body

    async def test_ai_view_trace_mode_has_full_detail_tabs(self, client):
        """Trace group mode should preserve full per-call detail tabs."""
        trace_id = "trace-detail-tabs-123"
        r = await client.post(
            "/v1/ai",
            json={
                "trace_id": trace_id,
                "service": "trace-detail-svc",
                "provider": "openai",
                "model": "gpt-4o",
                "operation": "chat",
                "input_messages": [{"role": "user", "content": "show details"}],
                "output_messages": [{"role": "assistant", "content": "ok"}],
                "tokens_in": 11,
                "tokens_out": 5,
                "duration_ms": 110,
            },
        )
        assert r.status_code == 200

        r2 = await client.get("/ai?view=trace")
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        assert "Conversation" in body
        assert "Metrics" in body
        assert "Raw JSON" in body

    async def test_ai_view_includes_raw_json_tab(self, client):
        """AI view should include Raw JSON tab with span attributes."""
        r = await client.post(
            "/v1/ai",
            json={
                "service": "json-svc",
                "provider": "openai",
                "model": "gpt-4o",
                "input_messages": [{"role": "user", "content": "JSON tab test"}],
                "output_messages": [{"role": "assistant", "content": "OK"}],
                "tokens_in": 8,
                "tokens_out": 2,
                "duration_ms": 100,
            },
        )
        assert r.status_code == 200

        r2 = await client.get("/ai")
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        # Raw JSON tab should be present
        assert "Raw JSON" in body
        assert "raw_attrs" in body or "Span Attributes" in body

    async def test_ai_view_includes_metrics_tab(self, client):
        """AI view should include Metrics tab with token and timing info."""
        r2 = await client.get("/ai")
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        assert "Metrics" in body
        assert "Tokens/sec" in body or "Duration" in body

    async def test_ai_view_includes_cost_estimation(self, client):
        """AI view should include cost estimation JavaScript."""
        r2 = await client.get("/ai")
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        assert "AI_PRICING" in body
        assert "aiCostEstimate" in body
        assert "Est. Cost" in body

    async def test_ai_export_jsonl(self, client):
        """GET /api/ai/export should return JSONL training data."""
        r = await client.post(
            "/v1/ai",
            json={
                "service": "export-svc",
                "provider": "openai",
                "model": "gpt-4o",
                "input_messages": [{"role": "user", "content": "Export test prompt"}],
                "output_messages": [{"role": "assistant", "content": "Export test response"}],
                "tokens_in": 12,
                "tokens_out": 6,
                "duration_ms": 150,
            },
        )
        assert r.status_code == 200

        r2 = await client.get("/api/ai/export")
        assert r2.status_code == 200
        data = await r2.get_data(as_text=True)
        # Each line should be valid JSON
        lines = [ln for ln in data.strip().split("\n") if ln.strip()]
        assert len(lines) > 0
        record = json.loads(lines[0])
        assert "messages" in record
        assert "metadata" in record
        assert "model" in record["metadata"]

    async def test_ai_export_json_format(self, client):
        """GET /api/ai/export?format=json should return JSON array."""
        r2 = await client.get("/api/ai/export?format=json")
        assert r2.status_code == 200
        assert r2.mimetype == "application/json"
        data = await r2.get_data(as_text=True)
        records = json.loads(data)
        assert isinstance(records, list)

    async def test_ai_export_negative_limit_is_clamped(self, client):
        """Export endpoint should clamp negative limit values to a safe minimum."""
        r = await client.post(
            "/v1/ai",
            json={
                "service": "clamp-svc",
                "provider": "openai",
                "model": "gpt-4o-mini",
                "prompt": "Clamp test",
                "response": "OK",
                "tokens_in": 3,
                "tokens_out": 1,
                "duration_ms": 30,
            },
        )
        assert r.status_code == 200

        r2 = await client.get("/api/ai/export?service=clamp-svc&limit=-50")
        assert r2.status_code == 200
        assert r2.mimetype == "application/x-ndjson"
        data = await r2.get_data(as_text=True)
        lines = [ln for ln in data.strip().split("\n") if ln.strip()]
        assert len(lines) == 1

    async def test_ai_export_filter_by_service(self, client):
        """Export endpoint should accept service filter."""
        r = await client.post(
            "/v1/ai",
            json={
                "service": "filtered-export-svc",
                "provider": "openai",
                "model": "gpt-4o-mini",
                "prompt": "Filtered export test",
                "response": "OK",
                "tokens_in": 5,
                "tokens_out": 1,
                "duration_ms": 50,
            },
        )
        assert r.status_code == 200

        r2 = await client.get("/api/ai/export?service=filtered-export-svc")
        assert r2.status_code == 200
        data = await r2.get_data(as_text=True)
        lines = [ln for ln in data.strip().split("\n") if ln.strip()]
        assert len(lines) > 0
        # All exported records should belong to this service
        for line in lines:
            record = json.loads(line)
            assert record["metadata"]["service"] == "filtered-export-svc"

    async def test_ai_view_includes_export_button(self, client):
        """AI page should include an Export JSONL button."""
        r = await client.get("/ai")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "Export JSONL" in body

    async def test_ai_view_total_errors_shown(self, client):
        """AI view should display total error count in summary cards."""
        r = await client.get("/ai")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "Errors" in body

    async def test_ai_view_shows_tokens_per_sec(self, client):
        """AI view should show tokens/sec for calls with duration > 0."""
        r = await client.post(
            "/v1/ai",
            json={
                "service": "speed-svc",
                "provider": "openai",
                "model": "gpt-4o",
                "tokens_in": 50,
                "tokens_out": 100,
                "duration_ms": 1000,
            },
        )
        assert r.status_code == 200

        r2 = await client.get("/ai")
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        assert "Tokens/sec" in body


# ---------------------------------------------------------------------------
# Custom Dashboards (eChart)
# ---------------------------------------------------------------------------
class TestCustomDashboards:
    async def test_list_dashboards_empty(self, client):
        r = await client.get("/dashboards")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "Dashboards" in body

    async def test_create_and_view_dashboard(self, client):
        r = await client.post(
            "/dashboards",
            form={"name": "Test Dashboard", "description": "A test"},
            follow_redirects=False,
        )
        assert r.status_code in (302, 303)
        location = r.headers.get("Location", "")
        assert "/dashboards/" in location

        r2 = await client.get(location, follow_redirects=False)
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        assert "Test Dashboard" in body
        assert "Add Chart" in body

    async def test_create_dashboard_requires_name(self, client):
        r = await client.post(
            "/dashboards",
            form={"name": "", "description": ""},
            follow_redirects=False,
        )
        assert r.status_code in (302, 303)
        assert r.headers.get("Location", "").endswith("/dashboards")

    async def test_add_chart_to_dashboard(self, client):
        # Create a dashboard first
        r = await client.post(
            "/dashboards",
            form={"name": "Chart Dashboard", "description": ""},
            follow_redirects=False,
        )
        location = r.headers.get("Location", "")
        dashboard_id = location.rstrip("/").split("/")[-1]

        # Add a chart
        r2 = await client.post(
            f"/dashboards/{dashboard_id}/charts",
            form={
                "title": "Latency Bands",
                "chart_spec_json": json.dumps(
                    {
                        "template_id": "time_series_percentiles",
                        "sql": {
                            "mode": "raw",
                            "override_sql": (
                                "SELECT toDateTime('2024-01-01 00:00:00') AS time, " "1 AS value, 2 AS p95, 3 AS p99"
                            ),
                        },
                    }
                ),
            },
            follow_redirects=False,
        )
        assert r2.status_code in (302, 303)

        # Verify chart appears on dashboard page
        r3 = await client.get(f"/dashboards/{dashboard_id}")
        assert r3.status_code == 200
        body = await r3.get_data(as_text=True)
        assert "Latency Bands" in body
        assert "Open Source View" in body

    async def test_dashboard_view_includes_template_guidance(self, client):
        r = await client.post(
            "/dashboards",
            form={"name": "Template Guidance", "description": ""},
            follow_redirects=False,
        )
        location = r.headers.get("Location", "")

        r2 = await client.get(location)
        assert r2.status_code == 200
        body = await r2.get_data(as_text=True)
        assert "SQL Builder" in body
        assert "Visual Builder" in body
        assert "Compiled SQL" in body
        assert "Columns: time, value, p95, p99" in body

    async def test_remove_chart_from_dashboard(self, client):
        # Create dashboard and add chart
        r = await client.post(
            "/dashboards",
            form={"name": "Remove Chart Test", "description": ""},
            follow_redirects=False,
        )
        location = r.headers.get("Location", "")
        dashboard_id = location.rstrip("/").split("/")[-1]
        await client.post(
            f"/dashboards/{dashboard_id}/charts",
            form={
                "title": "Temp Chart",
                "chart_spec_json": json.dumps(
                    {
                        "template_id": "gauge_kpi",
                        "sql": {"mode": "raw", "override_sql": "SELECT 1 AS value"},
                    }
                ),
            },
            follow_redirects=False,
        )

        # Get chart list to find chart id
        # import app as sobs_app  # noqa: PLC0415
        from app import _get_charts, get_db  # noqa: PLC0415

        charts = _get_charts(get_db(), dashboard_id)
        assert len(charts) >= 1
        chart_id = charts[0]["id"]

        r2 = await client.post(
            f"/dashboards/{dashboard_id}/charts/{chart_id}/delete",
            follow_redirects=False,
        )
        assert r2.status_code in (302, 303)

    async def test_edit_chart_on_dashboard(self, client):
        r = await client.post(
            "/dashboards",
            form={"name": "Edit Chart Test", "description": ""},
            follow_redirects=False,
        )
        dashboard_id = r.headers.get("Location", "").rstrip("/").split("/")[-1]

        await client.post(
            f"/dashboards/{dashboard_id}/charts",
            form={
                "title": "Original Chart",
                "chart_spec_json": json.dumps(
                    {
                        "template_id": "gauge_kpi",
                        "sql": {"mode": "raw", "override_sql": "SELECT 1 AS value"},
                    }
                ),
            },
            follow_redirects=False,
        )

        from app import _get_charts, get_db  # noqa: PLC0415

        charts = _get_charts(get_db(), dashboard_id)
        assert charts
        chart_id = charts[0]["id"]

        r2 = await client.post(
            f"/dashboards/{dashboard_id}/charts/{chart_id}/edit",
            form={
                "title": "Updated Chart",
                "chart_spec_json": json.dumps(
                    {
                        "template_id": "time_series_percentiles",
                        "sql": {
                            "mode": "raw",
                            "override_sql": (
                                "SELECT toDateTime('2024-01-01 00:00:00') AS time, " "1 AS value, 2 AS p95, 3 AS p99"
                            ),
                        },
                    }
                ),
            },
            follow_redirects=False,
        )
        assert r2.status_code in (302, 303)

        charts_after = _get_charts(get_db(), dashboard_id)
        edited = next(c for c in charts_after if c["id"] == chart_id)
        assert edited["title"] == "Updated Chart"
        assert edited["chart_type"] == "time_series_percentiles"

    async def test_clone_chart_on_dashboard(self, client):
        r = await client.post(
            "/dashboards",
            form={"name": "Clone Chart Test", "description": ""},
            follow_redirects=False,
        )
        dashboard_id = r.headers.get("Location", "").rstrip("/").split("/")[-1]

        await client.post(
            f"/dashboards/{dashboard_id}/charts",
            form={
                "title": "Source Chart",
                "chart_spec_json": json.dumps(
                    {
                        "template_id": "gauge_kpi",
                        "sql": {"mode": "raw", "override_sql": "SELECT 1 AS value"},
                    }
                ),
            },
            follow_redirects=False,
        )

        from app import _get_charts, get_db  # noqa: PLC0415

        charts = _get_charts(get_db(), dashboard_id)
        assert len(charts) == 1
        source_chart_id = charts[0]["id"]

        r2 = await client.post(
            f"/dashboards/{dashboard_id}/charts/{source_chart_id}/clone",
            form={
                "title": "Source Chart (copy)",
                "chart_spec_json": json.dumps(
                    {
                        "template_id": "gauge_kpi",
                        "sql": {"mode": "raw", "override_sql": "SELECT 1 AS value"},
                    }
                ),
            },
            follow_redirects=False,
        )
        assert r2.status_code in (302, 303)

        charts_after = _get_charts(get_db(), dashboard_id)
        assert len(charts_after) == 2
        titles = [c["title"] for c in charts_after]
        assert "Source Chart" in titles
        assert "Source Chart (copy)" in titles

    async def test_delete_dashboard(self, client):
        r = await client.post(
            "/dashboards",
            form={"name": "Delete Me", "description": ""},
            follow_redirects=False,
        )
        location = r.headers.get("Location", "")
        dashboard_id = location.rstrip("/").split("/")[-1]

        r2 = await client.post(
            f"/dashboards/{dashboard_id}/delete",
            follow_redirects=False,
        )
        assert r2.status_code in (302, 303)

        # Dashboard should no longer appear
        r3 = await client.get(f"/dashboards/{dashboard_id}", follow_redirects=False)
        assert r3.status_code in (302, 303)

    async def test_chart_query_api_select(self, client):
        r = await client.post(
            "/api/dashboards/query",
            json={"query": "SELECT 1 AS num"},
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert "columns" in data
        assert "rows" in data
        assert data["columns"] == ["num"]
        assert data["rows"][0][0] == 1

    async def test_chart_query_api_rejects_non_select(self, client):
        r = await client.post(
            "/api/dashboards/query",
            json={"query": "INSERT INTO otel_logs FORMAT JSONEachRow {}"},
        )
        assert r.status_code == 400
        data = await r.get_json()
        assert "error" in data

    async def test_chart_query_api_rejects_empty(self, client):
        r = await client.post(
            "/api/dashboards/query",
            json={"query": ""},
        )
        assert r.status_code == 400

    async def test_chart_query_api_auto_limits(self, client):
        r = await client.post(
            "/api/dashboards/query",
            json={"query": "SELECT number FROM system.numbers"},
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert len(data["rows"]) <= 1000

    async def test_chart_query_api_returns_sanitized_db_error(self, client):
        r = await client.post(
            "/api/dashboards/query",
            json={"query": "SELECT 'x' >= 500 AS bad"},
        )
        assert r.status_code == 400
        data = await r.get_json()
        assert "error" in data
        assert "Traceback" not in data["error"]
        assert "/Users/" not in data["error"]
        assert "Check casts and column types" in data["error"]

    async def test_chart_render_api_returns_sanitized_db_error(self, client):
        r = await client.post(
            "/api/dashboards/render",
            json={"query": "SELECT 'x' >= 500 AS value", "template_id": "gauge_kpi"},
        )
        assert r.status_code == 400
        data = await r.get_json()
        assert "error" in data
        assert "Traceback" not in data["error"]
        assert "/Users/" not in data["error"]
        assert "Check casts and column types" in data["error"]

    async def test_chart_spec_compile_builder_endpoint(self, client):
        r = await client.post(
            "/api/dashboards/spec/compile",
            json={
                "spec": {
                    "template_id": "derived_signal_overlay",
                    "sql": {"mode": "builder"},
                    "data": {
                        "source_view": "v_derived_signals_anomaly",
                        "signal_source": "traces",
                        "signal_name": "trace_volume",
                        "window_hours": 6,
                        "limit": 100,
                    },
                }
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert data["template_id"] == "derived_signal_overlay"
        assert "FROM v_derived_signals_anomaly" in data["query"]

    async def test_chart_spec_compile_builder_supports_base_table_source(self, client):
        r = await client.post(
            "/api/dashboards/spec/compile",
            json={
                "spec": {
                    "template_id": "derived_signal_overlay",
                    "sql": {"mode": "builder"},
                    "data": {
                        "source_view": "otel_logs",
                        "service": "checkout",
                        "signal_name": "log_volume",
                        "window_hours": 6,
                        "limit": 100,
                    },
                }
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert "FROM otel_logs" in data["query"]
        assert "toStartOfMinute(TimestampTime)" in data["query"]

    async def test_chart_spec_compile_builder_supports_metric_base_table_source(self, client):
        r = await client.post(
            "/api/dashboards/spec/compile",
            json={
                "spec": {
                    "template_id": "anomaly_overlay",
                    "sql": {"mode": "builder"},
                    "data": {
                        "source_view": "otel_metrics_sum",
                        "service": "checkout",
                        "metric_name": "cpu.usage",
                        "window_hours": 6,
                        "limit": 100,
                    },
                }
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert "FROM otel_metrics_sum" in data["query"]
        assert "avg(toFloat64(Value)) AS value" in data["query"]

    async def test_chart_spec_compile_builder_supports_all_templates(self, client):
        templates = [
            "time_series_percentiles",
            "heatmap",
            "box_plot",
            "dual_axis_anomaly",
            "anomaly_overlay",
            "derived_signal_overlay",
            "gauge_kpi",
        ]

        for template_id in templates:
            source_view = "v_otel_metrics_anomaly"
            if template_id == "derived_signal_overlay":
                source_view = "v_derived_signals_anomaly"

            r = await client.post(
                "/api/dashboards/spec/compile",
                json={
                    "spec": {
                        "template_id": template_id,
                        "sql": {"mode": "builder"},
                        "data": {
                            "source_view": source_view,
                            "window_hours": 6,
                            "limit": 100,
                        },
                    }
                },
            )
            assert r.status_code == 200

    async def test_chart_spec_compile_rejects_builder_mode_for_custom_echarts(self, client):
        r = await client.post(
            "/api/dashboards/spec/compile",
            json={
                "spec": {
                    "template_id": "custom_echarts",
                    "sql": {"mode": "builder"},
                    "data": {
                        "source_view": "v_derived_signals_anomaly",
                        "window_hours": 6,
                        "limit": 100,
                    },
                }
            },
        )
        assert r.status_code == 400
        data = await r.get_json()
        assert "requires sql.mode='raw'" in data["error"]

    async def test_chart_spec_options_endpoint_returns_distinct_lists(self, client):
        r = await client.get("/api/dashboards/spec/options?source_view=v_derived_signals_anomaly&signal_source=traces")
        assert r.status_code == 200
        data = await r.get_json()
        assert data["source_view"] == "v_derived_signals_anomaly"
        assert isinstance(data["services"], list)
        assert isinstance(data["signals"], list)
        assert isinstance(data["metrics"], list)

    async def test_chart_spec_validate_endpoint(self, client):
        r = await client.post(
            "/api/dashboards/spec/validate",
            json={
                "spec": {
                    "template_id": "anomaly_overlay",
                    "sql": {
                        "mode": "raw",
                        "override_sql": (
                            "SELECT toDateTime('2024-01-01 00:00:00') AS time, "
                            "1 AS value, 1 AS baseline_mean, 0 AS baseline_lower, "
                            "2 AS baseline_upper, 'normal' AS anomaly_state"
                        ),
                    },
                }
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert data["valid"] is True
        assert data["row_count"] == 1

    async def test_chart_spec_validate_endpoint_with_role_map(self, client):
        r = await client.post(
            "/api/dashboards/spec/validate",
            json={
                "spec": {
                    "template_id": "anomaly_overlay",
                    "sql": {
                        "mode": "raw",
                        "override_sql": (
                            "SELECT toDateTime('2024-01-01 00:00:00') AS t, "
                            "1 AS v, 1 AS bm, 0 AS bl, 2 AS bu, 'normal' AS st"
                        ),
                    },
                    "visual": {
                        "role_map": {
                            "time": "t",
                            "value": "v",
                            "baseline_mean": "bm",
                            "baseline_lower": "bl",
                            "baseline_upper": "bu",
                            "anomaly_state": "st",
                        }
                    },
                }
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert data["valid"] is True

    async def test_chart_spec_validate_endpoint_rejects_unknown_role_column(self, client):
        r = await client.post(
            "/api/dashboards/spec/validate",
            json={
                "spec": {
                    "template_id": "anomaly_overlay",
                    "sql": {
                        "mode": "raw",
                        "override_sql": (
                            "SELECT toDateTime('2024-01-01 00:00:00') AS time, "
                            "1 AS value, 1 AS baseline_mean, 0 AS baseline_lower, "
                            "2 AS baseline_upper, 'normal' AS anomaly_state"
                        ),
                    },
                    "visual": {"role_map": {"value": "missing_col"}},
                }
            },
        )
        assert r.status_code == 400
        data = await r.get_json()
        assert data["valid"] is False
        assert "unknown column" in data["error"].lower()

    async def test_chart_spec_dry_run_includes_column_types(self, client):
        r = await client.post(
            "/api/dashboards/spec/dry-run",
            json={
                "spec": {
                    "template_id": "anomaly_overlay",
                    "sql": {
                        "mode": "raw",
                        "override_sql": (
                            "SELECT toDateTime('2024-01-01 00:00:00') AS time, "
                            "1 AS value, 1 AS baseline_mean, 0 AS baseline_lower, "
                            "2 AS baseline_upper, 'normal' AS anomaly_state"
                        ),
                    },
                }
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert data["columns"][0] == "time"
        assert len(data["column_types"]) == len(data["columns"])

    async def test_chart_spec_render_endpoint(self, client):
        r = await client.post(
            "/api/dashboards/spec/render",
            json={
                "spec": {
                    "template_id": "anomaly_overlay",
                    "sql": {
                        "mode": "raw",
                        "override_sql": (
                            "SELECT toDateTime('2024-01-01 00:00:00') AS time, "
                            "1 AS value, 1 AS baseline_mean, 0 AS baseline_lower, "
                            "2 AS baseline_upper, 'normal' AS anomaly_state"
                        ),
                    },
                    "visual": {
                        "zoom_inside": True,
                        "zoom_slider": True,
                        "zoom_start_pct": 10,
                        "zoom_end_pct": 90,
                        "legend_show": False,
                        "smooth_line": False,
                    },
                }
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert "option" in data
        assert data["template_id"] == "anomaly_overlay"
        assert data["option"]["legend"]["show"] is False
        assert len(data["option"]["dataZoom"]) == 2

    async def test_chart_spec_render_endpoint_custom_echarts(self, client):
        r = await client.post(
            "/api/dashboards/spec/render",
            json={
                "spec": {
                    "template_id": "custom_echarts",
                    "sql": {
                        "mode": "raw",
                        "override_sql": (
                            "SELECT toDateTime('2024-01-01 00:00:00') AS ts, "
                            "10 AS v UNION ALL "
                            "SELECT toDateTime('2024-01-01 00:01:00') AS ts, 12 AS v "
                            "ORDER BY ts"
                        ),
                    },
                    "visual": {
                        "custom_mapping_json": '{"points": {"from": "rows"}}',
                        "custom_option_json": (
                            '{"xAxis": {"type": "time"}, '
                            '"yAxis": {"type": "value"}, '
                            '"series": [{"type": "line", "data": "{{points}}"}]}'
                        ),
                    },
                }
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert data["template_id"] == "custom_echarts"
        assert data["option"]["series"][0]["data"][0][1] == 10

    async def test_chart_spec_validate_rejects_invalid_custom_json(self, client):
        r = await client.post(
            "/api/dashboards/spec/validate",
            json={
                "spec": {
                    "template_id": "custom_echarts",
                    "sql": {
                        "mode": "raw",
                        "override_sql": "SELECT 1 AS value",
                    },
                    "visual": {
                        "custom_mapping_json": "{bad json",
                        "custom_option_json": "{}",
                    },
                }
            },
        )
        assert r.status_code == 400
        data = await r.get_json()
        assert data["valid"] is False
        assert "custom_mapping_json" in data["error"]

    async def test_chart_spec_render_custom_echarts_emits_custom_drilldown(self, client):
        r = await client.post(
            "/api/dashboards/spec/render",
            json={
                "spec": {
                    "template_id": "custom_echarts",
                    "sql": {
                        "mode": "raw",
                        "override_sql": (
                            "SELECT 'checkout' AS service, toDateTime('2024-01-01 00:00:00') AS ts, 42 AS value"
                        ),
                    },
                    "visual": {
                        "custom_mapping_json": (
                            '{"points":{"from":"rows"},"_drilldown":{"target":"logs",'
                            '"label":"Open logs","extra":{"service":"{{service}}","from_ts":"{{ts}}"}}}'
                        ),
                        "custom_option_json": (
                            '{"xAxis":{"type":"time"},"yAxis":{"type":"value"},'
                            '"series":[{"type":"line","data":"{{points}}"}]}'
                        ),
                    },
                }
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        dd = data["option"]["_customDrilldown"]
        assert dd["target"] == "logs"
        assert dd["label"] == "Open logs"
        assert dd["extra"]["service"] == "checkout"
        assert "2024-01-01" in dd["extra"]["from_ts"]

    async def test_chart_render_api_attaches_time_series_drilldown_metadata(self, client):
        r = await client.post(
            "/api/dashboards/render",
            json={
                "template_id": "time_series_percentiles",
                "query": ("SELECT toDateTime('2024-01-01 00:00:00') AS time, " "1 AS value, 2 AS p95, 3 AS p99"),
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        first_point = data["option"]["series"][0]["data"][0]
        assert first_point["drilldown"]["from_ts"] == "2024-01-01T00:00:00Z"
        assert first_point["drilldown"]["window_s"] == 60

    async def test_chart_render_api_attaches_heatmap_drilldown_metadata(self, client):
        r = await client.post(
            "/api/dashboards/render",
            json={
                "template_id": "heatmap",
                "query": (
                    "SELECT 'checkout' AS x_category, " "toDateTime('2024-01-01 00:05:00') AS y_category, 42 AS value"
                ),
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        first_cell = data["option"]["series"][0]["data"][0]
        assert first_cell["drilldown"]["service"] == "checkout"
        assert first_cell["drilldown"]["from_ts"] == "2024-01-01T00:05:00Z"
        assert first_cell["drilldown"]["window_s"] == 300

    async def test_derived_signal_overlay_uses_line_with_visual_map(self, client):
        r = await client.post(
            "/api/dashboards/render",
            json={
                "template_id": "derived_signal_overlay",
                "query": (
                    "SELECT toDateTime('2024-01-01 00:00:00') AS time, "
                    "'svc-a' AS service, 'traces' AS source, 'trace_volume' AS signal, '' AS attr_fp, "
                    "10.0 AS value, 5 AS sample_count, 8.0 AS baseline_mean, 5.0 AS baseline_lower, "
                    "11.0 AS baseline_upper, 'normal' AS anomaly_state, 0.2 AS anomaly_score"
                ),
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert data["option"]["series"][3]["type"] == "line"
        assert data["option"]["series"][4]["name"] == "Warnings"
        assert data["option"]["series"][5]["name"] == "Outliers"
        assert data["option"]["visualMap"]["dimension"] == 2
        assert isinstance(data["option"]["dataZoom"], list)
        assert data["option"]["dataZoom"][0]["start"] == 0
        assert data["option"]["series"][3]["markArea"]["label"]["show"] is False
        assert "now" in data["option"]["title"]["subtext"]
        assert data["option"]["yAxis"]["name"] == "Delta %"

    async def test_derived_signal_overlay_clamps_ratio_axis(self, client):
        r = await client.post(
            "/api/dashboards/render",
            json={
                "template_id": "derived_signal_overlay",
                "query": (
                    "SELECT toDateTime('2024-01-01 00:00:00') AS time, "
                    "'svc-a' AS service, 'traces' AS source, 'trace_error_ratio' AS signal, '' AS attr_fp, "
                    "0.33 AS value, 5 AS sample_count, 0.2 AS baseline_mean, 0.1 AS baseline_lower, "
                    "0.4 AS baseline_upper, 'warning' AS anomaly_state, 1.2 AS anomaly_score"
                ),
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert data["option"]["series"][3]["type"] == "line"
        assert data["option"]["yAxis"]["min"] == 0
        assert data["option"]["yAxis"]["max"] == 1
        assert isinstance(data["option"]["series"][3]["markArea"]["data"], list)

    async def test_add_chart_rejects_non_select_query(self, client):
        r = await client.post(
            "/dashboards",
            form={"name": "Security Test", "description": ""},
            follow_redirects=False,
        )
        location = r.headers.get("Location", "")
        dashboard_id = location.rstrip("/").split("/")[-1]

        r2 = await client.post(
            f"/dashboards/{dashboard_id}/charts",
            form={
                "title": "Bad Query",
                "chart_spec_json": json.dumps(
                    {
                        "template_id": "gauge_kpi",
                        "sql": {"mode": "raw", "override_sql": "DROP TABLE otel_logs"},
                    }
                ),
            },
            follow_redirects=False,
        )
        # Should redirect back (flash warning) not crash
        assert r2.status_code in (302, 303)

        # Confirm no chart was added
        from app import _get_charts, get_db  # noqa: PLC0415

        charts = _get_charts(get_db(), dashboard_id)
        assert not any(c["title"] == "Bad Query" for c in charts)

    async def test_dashboards_page_in_nav(self, client):
        r = await client.get("/dashboards")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "bar-chart-line" in body


# ---------------------------------------------------------------------------
# OTEL Metrics Anomaly Detection Tests
# ---------------------------------------------------------------------------
class TestMetricsAnomalyDetection:
    """Tests for the SQL-first anomaly-detection layer on OTEL metrics."""

    # ── helpers ──────────────────────────────────────────────────────────────

    def _make_gauge_payload(self, service: str, metric: str, value: float, ts_ns: int | None = None) -> dict:
        ts = ts_ns or int(time.time() * 1_000_000_000)
        return {
            "resourceMetrics": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service}}]},
                    "scopeMetrics": [
                        {
                            "metrics": [
                                {
                                    "name": metric,
                                    "description": "test gauge",
                                    "unit": "ms",
                                    "gauge": {
                                        "dataPoints": [
                                            {
                                                "timeUnixNano": str(ts),
                                                "asDouble": value,
                                            }
                                        ]
                                    },
                                }
                            ]
                        }
                    ],
                }
            ]
        }

    def _make_sum_payload(self, service: str, metric: str, value: float, ts_ns: int | None = None) -> dict:
        ts = ts_ns or int(time.time() * 1_000_000_000)
        return {
            "resourceMetrics": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service}}]},
                    "scopeMetrics": [
                        {
                            "metrics": [
                                {
                                    "name": metric,
                                    "description": "test counter",
                                    "unit": "1",
                                    "sum": {
                                        "isMonotonic": True,
                                        "aggregationTemporality": 2,
                                        "dataPoints": [
                                            {
                                                "timeUnixNano": str(ts),
                                                "asDouble": value,
                                            }
                                        ],
                                    },
                                }
                            ]
                        }
                    ],
                }
            ]
        }

    def _make_histogram_payload(
        self, service: str, metric: str, count: int, hsum: float, ts_ns: int | None = None
    ) -> dict:
        ts = ts_ns or int(time.time() * 1_000_000_000)
        return {
            "resourceMetrics": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service}}]},
                    "scopeMetrics": [
                        {
                            "metrics": [
                                {
                                    "name": metric,
                                    "description": "test histogram",
                                    "unit": "ms",
                                    "histogram": {
                                        "aggregationTemporality": 2,
                                        "dataPoints": [
                                            {
                                                "timeUnixNano": str(ts),
                                                "count": str(count),
                                                "sum": hsum,
                                                "bucketCounts": ["1", str(count - 1)],
                                                "explicitBounds": [50.0],
                                            }
                                        ],
                                    },
                                }
                            ]
                        }
                    ],
                }
            ]
        }

    # ── schema / table existence ─────────────────────────────────────────────

    async def test_otel_metric_tables_exist(self, client):
        """The three typed metric tables must be created by init_db."""
        db = sobs_app.get_db()
        for table in ("otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram"):
            row = db.execute("SELECT 1 FROM system.tables WHERE database='default' AND name=?", (table,)).fetchone()
            assert row is not None, f"Table {table!r} not found in schema"

    async def test_anomaly_views_exist(self, client):
        """The two anomaly views must be created by init_db."""
        db = sobs_app.get_db()
        for view in ("v_otel_metrics_1m", "v_otel_metrics_anomaly"):
            row = db.execute("SELECT 1 FROM system.tables WHERE database='default' AND name=?", (view,)).fetchone()
            assert row is not None, f"View {view!r} not found in schema"

    async def test_derived_signal_views_exist(self, client):
        """Derived signal views should exist for Option C anomaly workflows."""
        db = sobs_app.get_db()
        for view in ("v_derived_signals_1m", "v_derived_signals_anomaly"):
            row = db.execute("SELECT 1 FROM system.tables WHERE database='default' AND name=?", (view,)).fetchone()
            assert row is not None, f"View {view!r} not found in schema"

    async def test_metrics_index_page_renders(self, client):
        """Top-level metrics page should be accessible without chart drilldown."""
        r = await client.get("/metrics")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "Metrics & Signals" in body
        assert "sort_by=last_time" in body
        assert "No derived signals for the current filters." in body or "/ page" in body

    async def test_metrics_rules_page_renders(self, client):
        r = await client.get("/metrics/rules")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "Metrics Rules" in body
        assert "Rule Type" in body

    async def test_metrics_sort_by_signal(self, client):
        r = await client.get("/metrics?sort_by=signal&sort_dir=asc")
        assert r.status_code == 200

    async def test_metrics_limit_offset(self, client):
        r = await client.get("/metrics?limit=25&offset=0")
        assert r.status_code == 200

    async def test_rule_creation_surfaces_on_metrics_index(self, client):
        marker = f"rule-svc-{time.time_ns()}"
        r = await client.post(
            "/v1/errors",
            json={"service": marker, "type": "RuntimeError", "message": "rule eval seed", "stack": "x"},
        )
        assert r.status_code == 200

        r = await client.post(
            "/metrics/rules",
            form={
                "name": "Exception volume high",
                "source": "errors",
                "signal": "exception_volume",
                "service": marker,
                "attr_fp": "",
                "comparator": "gt",
                "warning_threshold": "0.5",
                "critical_threshold": "1.0",
                "min_sample_count": "1",
            },
        )
        assert r.status_code in (302, 303)

        r = await client.get(f"/metrics?service={marker}&source=errors&signal=exception_volume")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "Exception volume high" in body

    async def test_composite_rule_creation_renders_in_rules_page(self, client):
        marker = f"trace-svc-{time.time_ns()}"
        r = await client.post(
            "/metrics/rules",
            form={
                "name": "Trace distress composite",
                "rule_type": "composite",
                "source": "traces",
                "signal": "latency_p95_ms",
                "service": marker,
                "attr_fp": "",
                "comparator": "gt",
                "warning_threshold": "250",
                "critical_threshold": "500",
                "secondary_source": "traces",
                "secondary_signal": "trace_error_ratio",
                "secondary_comparator": "gt",
                "secondary_warning_threshold": "0.05",
                "secondary_critical_threshold": "0.1",
                "min_sample_count": "1",
            },
        )
        assert r.status_code in (302, 303)

        r = await client.get("/metrics/rules")
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "Trace distress composite" in body
        assert "trace_error_ratio" in body

    async def test_auto_metrics_rules_preview_shows_candidates(self, client):
        marker = f"auto-preview-svc-{time.time_ns()}"
        for _ in range(8):
            r = await client.post(
                "/v1/errors",
                json={"service": marker, "type": "RuntimeError", "message": "auto preview seed", "stack": "x"},
            )
            assert r.status_code == 200

        r = await client.post(
            "/metrics/rules/auto",
            form={
                "action": "preview",
                "hours": "24",
                "min_points": "1",
                "service_filter": marker,
            },
        )
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "Preview Candidates" in body
        assert marker in body

    async def test_auto_metrics_rules_create_is_idempotent(self, client):
        marker = f"auto-create-svc-{time.time_ns()}"
        for _ in range(8):
            r = await client.post(
                "/v1/errors",
                json={"service": marker, "type": "RuntimeError", "message": "auto create seed", "stack": "x"},
            )
            assert r.status_code == 200

        r = await client.post(
            "/metrics/rules/auto",
            form={
                "action": "create",
                "hours": "24",
                "min_points": "1",
                "service_filter": marker,
            },
        )
        assert r.status_code in (302, 303)

        count1 = (
            sobs_app.get_db()
            .execute(
                "SELECT count() AS c FROM sobs_anomaly_rules FINAL " "WHERE IsDeleted = 0 AND ServiceName = ?",
                (marker,),
            )
            .fetchone()["c"]
        )
        assert int(count1) >= 1

        r = await client.post(
            "/metrics/rules/auto",
            form={
                "action": "create",
                "hours": "24",
                "min_points": "1",
                "service_filter": marker,
            },
        )
        assert r.status_code in (302, 303)

        count2 = (
            sobs_app.get_db()
            .execute(
                "SELECT count() AS c FROM sobs_anomaly_rules FINAL " "WHERE IsDeleted = 0 AND ServiceName = ?",
                (marker,),
            )
            .fetchone()["c"]
        )
        assert int(count2) == int(count1)

    async def test_auto_metrics_rules_create_honors_max_cap(self, client, monkeypatch):
        marker = f"auto-cap-svc-{time.time_ns()}"

        def _fake_candidates(*args, **kwargs):
            rows = []
            for i in range(250):
                rows.append(
                    {
                        "name": f"Auto cap {i}",
                        "rule_type": "threshold",
                        "source": "errors",
                        "signal": f"sig_{i}",
                        "service": marker,
                        "attr_fp": "",
                        "comparator": "gt",
                        "warning_threshold": 1.0,
                        "critical_threshold": 2.0,
                        "min_sample_count": 3,
                        "point_count": 100,
                    }
                )
            return rows, {"examined": 250, "existing": 0, "invalid": 0}

        monkeypatch.setattr(sobs_app, "_build_auto_metric_rule_candidates", _fake_candidates)

        r = await client.post(
            "/metrics/rules/auto",
            form={
                "action": "create",
                "hours": "24",
                "min_points": "1",
                "service_filter": marker,
            },
        )
        assert r.status_code in (302, 303)

        created = (
            sobs_app.get_db()
            .execute(
                "SELECT count() AS c FROM sobs_anomaly_rules FINAL " "WHERE IsDeleted = 0 AND ServiceName = ?",
                (marker,),
            )
            .fetchone()["c"]
        )
        assert int(created) == 200

    async def test_auto_dashboard_preview_shows_rule_candidates(self, client):
        marker = f"auto-dash-preview-svc-{time.time_ns()}"
        r = await client.post(
            "/metrics/rules",
            form={
                "name": "Auto dashboard preview rule",
                "source": "errors",
                "signal": "exception_volume",
                "service": marker,
                "attr_fp": "",
                "comparator": "gt",
                "warning_threshold": "0.5",
                "critical_threshold": "1.0",
                "min_sample_count": "1",
            },
        )
        assert r.status_code in (302, 303)

        r = await client.post(
            "/metrics/rules/dashboard/auto",
            form={
                "action": "preview",
                "hours": "24",
                "max_charts": "12",
                "service_filter": marker,
                "dashboard_name": f"Auto Preview Dashboard {marker}",
            },
        )
        assert r.status_code == 200
        body = await r.get_data(as_text=True)
        assert "Dashboard Preview Candidates" in body
        assert "Auto dashboard preview rule" in body

    async def test_auto_dashboard_create_is_idempotent(self, client):
        marker = f"auto-dash-create-svc-{time.time_ns()}"
        dashboard_name = f"Auto Dashboard {marker}"
        r = await client.post(
            "/metrics/rules",
            form={
                "name": "Auto dashboard create rule",
                "source": "errors",
                "signal": "exception_volume",
                "service": marker,
                "attr_fp": "",
                "comparator": "gt",
                "warning_threshold": "0.5",
                "critical_threshold": "1.0",
                "min_sample_count": "1",
            },
        )
        assert r.status_code in (302, 303)

        r = await client.post(
            "/metrics/rules/dashboard/auto",
            form={
                "action": "create",
                "hours": "24",
                "max_charts": "12",
                "service_filter": marker,
                "dashboard_name": dashboard_name,
            },
        )
        assert r.status_code in (302, 303)

        db = sobs_app.get_db()
        dashboard_id = db.execute(
            "SELECT Id FROM sobs_dashboards FINAL WHERE IsDeleted = 0 AND Name = ? LIMIT 1",
            (dashboard_name,),
        ).fetchone()["Id"]
        count1 = db.execute(
            "SELECT count() AS c FROM sobs_chart_configs FINAL WHERE IsDeleted = 0 AND DashboardId = ?",
            (str(dashboard_id),),
        ).fetchone()["c"]
        assert int(count1) >= 1

        r = await client.post(
            "/metrics/rules/dashboard/auto",
            form={
                "action": "create",
                "hours": "24",
                "max_charts": "12",
                "service_filter": marker,
                "dashboard_name": dashboard_name,
            },
        )
        assert r.status_code in (302, 303)

        count2 = db.execute(
            "SELECT count() AS c FROM sobs_chart_configs FINAL WHERE IsDeleted = 0 AND DashboardId = ?",
            (str(dashboard_id),),
        ).fetchone()["c"]
        assert int(count2) == int(count1)

    async def test_derived_signal_overlay_template_injects_rule_metadata(self, client):
        r = await client.post(
            "/metrics/rules",
            form={
                "name": "Overlay latency high",
                "rule_type": "threshold",
                "source": "traces",
                "signal": "latency_p95_ms",
                "service": "overlay-svc",
                "attr_fp": "",
                "comparator": "gt",
                "warning_threshold": "100",
                "critical_threshold": "150",
                "min_sample_count": "1",
            },
        )
        assert r.status_code in (302, 303)

        r = await client.post(
            "/api/dashboards/render",
            json={
                "template_id": "derived_signal_overlay",
                "query": (
                    "SELECT toDateTime('2024-01-01 00:00:00') AS time, "
                    "'overlay-svc' AS service, "
                    "'traces' AS source, "
                    "'latency_p95_ms' AS signal, "
                    "'' AS attr_fp, "
                    "175.0 AS value, "
                    "3 AS sample_count, "
                    "120.0 AS baseline_mean, "
                    "90.0 AS baseline_lower, "
                    "140.0 AS baseline_upper, "
                    "'warning' AS anomaly_state, "
                    "2.2 AS anomaly_score"
                ),
            },
        )
        assert r.status_code == 200
        data = await r.get_json()
        value_series = next(s for s in data["option"]["series"] if s.get("name") == "Value")
        first_point = value_series["data"][0]
        assert first_point["drilldown"]["service"] == "overlay-svc"
        assert first_point["drilldown"]["signal"] == "latency_p95_ms"
        assert first_point["drilldown"]["_rule_name"] == "Overlay latency high"
        assert first_point["drilldown"]["_effective_state"] == "outlier"

    # ── gauge ingest ─────────────────────────────────────────────────────────

    async def test_gauge_metric_ingest_accepted(self, client):
        payload = self._make_gauge_payload("svc-gauge", "cpu.usage", 42.5)
        r = await client.post("/v1/metrics", json=payload)
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["accepted"] == 1

    async def test_gauge_metric_persisted_in_db(self, client):
        ts_ns = int(time.time() * 1_000_000_000)
        payload = self._make_gauge_payload("svc-gauge-db", "memory.usage", 77.0, ts_ns)
        r = await client.post("/v1/metrics", json=payload)
        assert r.status_code == 200
        row = (
            sobs_app.get_db()
            .execute(
                "SELECT Value, ServiceName FROM otel_metrics_gauge WHERE ServiceName=? ORDER BY TimeUnix DESC LIMIT 1",
                ("svc-gauge-db",),
            )
            .fetchone()
        )
        assert row is not None, "Gauge row not found"
        assert abs(float(row["Value"]) - 77.0) < 1e-6

    # ── sum ingest ───────────────────────────────────────────────────────────

    async def test_sum_metric_ingest_accepted(self, client):
        payload = self._make_sum_payload("svc-sum", "requests.total", 1000.0)
        r = await client.post("/v1/metrics", json=payload)
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 1

    async def test_sum_metric_persisted_in_db(self, client):
        ts_ns = int(time.time() * 1_000_000_000)
        payload = self._make_sum_payload("svc-sum-db", "http.requests", 500.0, ts_ns)
        r = await client.post("/v1/metrics", json=payload)
        assert r.status_code == 200
        row = (
            sobs_app.get_db()
            .execute(
                "SELECT Value, IsMonotonic FROM otel_metrics_sum WHERE ServiceName=? ORDER BY TimeUnix DESC LIMIT 1",
                ("svc-sum-db",),
            )
            .fetchone()
        )
        assert row is not None, "Sum row not found"
        assert abs(float(row["Value"]) - 500.0) < 1e-6
        assert int(row["IsMonotonic"]) == 1

    # ── histogram ingest ─────────────────────────────────────────────────────

    async def test_histogram_metric_ingest_accepted(self, client):
        payload = self._make_histogram_payload("svc-hist", "request.duration", 100, 5000.0)
        r = await client.post("/v1/metrics", json=payload)
        assert r.status_code == 200
        assert json.loads(await r.get_data())["accepted"] == 1

    async def test_histogram_metric_persisted_in_db(self, client):
        ts_ns = int(time.time() * 1_000_000_000)
        payload = self._make_histogram_payload("svc-hist-db", "latency", 200, 10000.0, ts_ns)
        r = await client.post("/v1/metrics", json=payload)
        assert r.status_code == 200
        row = (
            sobs_app.get_db()
            .execute(
                "SELECT Count, Sum FROM otel_metrics_histogram WHERE ServiceName=? ORDER BY TimeUnix DESC LIMIT 1",
                ("svc-hist-db",),
            )
            .fetchone()
        )
        assert row is not None, "Histogram row not found"
        assert int(row["Count"]) == 200
        assert abs(float(row["Sum"]) - 10000.0) < 1e-6

    # ── attr fingerprint ─────────────────────────────────────────────────────

    async def test_attr_fingerprint_is_stable(self, client):
        """Same attribute dict should always produce the same fingerprint."""
        from app import _attr_fingerprint  # noqa: PLC0415

        attrs = {"env": "prod", "region": "us-east-1"}
        fp1 = _attr_fingerprint(attrs)
        fp2 = _attr_fingerprint(attrs)
        assert fp1 == fp2
        assert len(fp1) == 16

    async def test_attr_fingerprint_excludes_runtime_attrs(self, client):
        """Runtime/telemetry prefixes should not affect fingerprint."""
        from app import _attr_fingerprint  # noqa: PLC0415

        attrs_with = {"env": "prod", "telemetry.sdk.version": "1.0", "process.pid": "42"}
        attrs_without = {"env": "prod"}
        # telemetry.* and process.* are excluded so fingerprints should match
        assert _attr_fingerprint(attrs_with) == _attr_fingerprint(attrs_without)

    # ── normalised view ───────────────────────────────────────────────────────

    async def test_v_otel_metrics_1m_returns_gauge_data(self, client):
        """After ingesting a gauge, v_otel_metrics_1m should include the row."""
        ts_ns = int(time.time() * 1_000_000_000)
        payload = self._make_gauge_payload("svc-view-test", "view.metric", 99.9, ts_ns)
        r = await client.post("/v1/metrics", json=payload)
        assert r.status_code == 200

        rows = (
            sobs_app.get_db()
            .execute(
                "SELECT MetricKind, Value FROM v_otel_metrics_1m"
                " WHERE ServiceName='svc-view-test' AND MetricName='view.metric'"
                " ORDER BY MinuteBucket DESC LIMIT 1"
            )
            .fetchall()
        )
        assert rows, "v_otel_metrics_1m returned no rows for ingested gauge"
        assert str(rows[0]["MetricKind"]) == "gauge"

    # ── anomaly API endpoint ──────────────────────────────────────────────────

    async def test_anomaly_api_requires_service_and_metric(self, client):
        r = await client.get("/api/metrics/anomaly")
        assert r.status_code == 400
        data = await r.get_json()
        assert "error" in data

    async def test_anomaly_api_missing_metric_returns_400(self, client):
        r = await client.get("/api/metrics/anomaly?service=svc")
        assert r.status_code == 400

    async def test_anomaly_api_returns_expected_structure(self, client):
        """After ingesting gauge points the anomaly API must return expected columns."""
        # Insert several gauge data points so the view has data
        ts_base = int(time.time() * 1_000_000_000)
        for i in range(5):
            ts_ns = ts_base - i * 60 * 1_000_000_000
            p = self._make_gauge_payload("svc-anomaly-api", "api.metric", float(10 + i), ts_ns)
            await client.post("/v1/metrics", json=p)

        r = await client.get("/api/metrics/anomaly?service=svc-anomaly-api&metric=api.metric&hours=1")
        assert r.status_code == 200
        data = await r.get_json()
        assert data["service"] == "svc-anomaly-api"
        assert data["metric"] == "api.metric"
        assert "columns" in data
        assert "rows" in data
        expected_cols = {"time", "value", "anomaly_score", "anomaly_state", "baseline_mean"}
        assert expected_cols.issubset(set(data["columns"]))

    async def test_anomaly_api_spike_flagged_as_warning_or_outlier(self, client):
        """A synthetic 10-sigma spike should be flagged as warning or outlier."""
        ts_base = int(time.time() * 1_000_000_000)
        # 59 normal points near 10.0 …
        for i in range(59):
            ts_ns = ts_base - (60 - i) * 60 * 1_000_000_000
            p = self._make_gauge_payload("svc-spike-test", "spike.metric", 10.0, ts_ns)
            await client.post("/v1/metrics", json=p)
        # … followed by one large spike
        spike_ts = ts_base - 1 * 60 * 1_000_000_000
        p = self._make_gauge_payload("svc-spike-test", "spike.metric", 1000.0, spike_ts)
        await client.post("/v1/metrics", json=p)

        r = await client.get("/api/metrics/anomaly?service=svc-spike-test&metric=spike.metric&hours=2")
        assert r.status_code == 200
        data = await r.get_json()
        col_idx = {c: i for i, c in enumerate(data["columns"])}
        states = [row[col_idx["anomaly_state"]] for row in data["rows"]]
        assert any(
            s in ("warning", "outlier") for s in states
        ), f"No anomalous point detected for 10-sigma spike; states={states!r}"

    async def test_anomaly_api_steady_series_not_over_flagged(self, client):
        """A perfectly steady series should produce only 'normal' anomaly states."""
        ts_base = int(time.time() * 1_000_000_000)
        for i in range(30):
            ts_ns = ts_base - i * 60 * 1_000_000_000
            p = self._make_gauge_payload("svc-steady", "steady.metric", 42.0, ts_ns)
            await client.post("/v1/metrics", json=p)

        r = await client.get("/api/metrics/anomaly?service=svc-steady&metric=steady.metric&hours=1")
        assert r.status_code == 200
        data = await r.get_json()
        col_idx = {c: i for i, c in enumerate(data["columns"])}
        if data["rows"]:
            states = [row[col_idx["anomaly_state"]] for row in data["rows"]]
            assert all(s == "normal" for s in states), f"Steady series was over-flagged; states={states!r}"

    # ── chart templates ───────────────────────────────────────────────────────

    async def test_dual_axis_anomaly_template_present(self, client):
        """The dual_axis_anomaly template must reference v_otel_metrics_anomaly."""
        from app import CHART_TEMPLATES  # noqa: PLC0415

        t = CHART_TEMPLATES.get("dual_axis_anomaly")
        assert t is not None
        assert "v_otel_metrics_anomaly" in t["sample_sql"]

    async def test_anomaly_overlay_template_present(self, client):
        """The anomaly_overlay template must exist with the correct column count."""
        from app import CHART_TEMPLATES  # noqa: PLC0415

        t = CHART_TEMPLATES.get("anomaly_overlay")
        assert t is not None
        assert t["min_columns"] == 6
        assert "anomaly_state" in t["column_roles"]

    async def test_anomaly_overlay_render_with_synthetic_data(self, client):
        """anomaly_overlay template must render without errors for synthetic data."""
        query = (
            "SELECT"
            "  now() AS time,"
            "  10.0 AS value,"
            "  10.0 AS baseline_mean,"
            "   8.0 AS baseline_lower,"
            "  12.0 AS baseline_upper,"
            " 'normal' AS anomaly_state"
        )
        r = await client.post(
            "/api/dashboards/render",
            json={"query": query, "template_id": "anomaly_overlay"},
        )
        assert r.status_code == 200
        data = await r.get_json()
        assert "error" not in data
        assert "option" in data

    async def test_anomaly_overlay_spike_coloring(self, client):
        """An outlier row should produce red colour binding in the rendered option."""
        query = (
            "SELECT"
            "  now() AS time,"
            "  100.0 AS value,"
            "  10.0 AS baseline_mean,"
            "   8.0 AS baseline_lower,"
            "  12.0 AS baseline_upper,"
            " 'outlier' AS anomaly_state"
        )
        r = await client.post(
            "/api/dashboards/render",
            json={"query": query, "template_id": "anomaly_overlay"},
        )
        assert r.status_code == 200
        data = await r.get_json()
        # The rendered option must contain the outlier colour (#dc3545) somewhere
        option_str = json.dumps(data.get("option", {}))
        assert "#dc3545" in option_str, "Outlier colour not found in rendered chart option"

    # ── hours boundary ────────────────────────────────────────────────────────

    async def test_anomaly_api_hours_clamped(self, client):
        """hours parameter must be clamped to the 1–168 range."""
        r = await client.get("/api/metrics/anomaly?service=x&metric=y&hours=9999")
        # Should not raise; may return empty data or valid JSON
        assert r.status_code in (200, 400)
        data = await r.get_json()
        # If 200, it's valid JSON with the expected structure
        if r.status_code == 200:
            assert "rows" in data


# ---------------------------------------------------------------------------
# Tag Rules & Record Tags
# ---------------------------------------------------------------------------
class TestTagRules:
    """Tests for auto-tagging, tag rule CRUD, and the record tag API."""

    # ── Settings pages ────────────────────────────────────────────────────────

    async def test_settings_page_loads(self, client):
        r = await client.get("/settings")
        assert r.status_code == 200
        text = (await r.get_data()).decode()
        assert "Settings" in text
        assert "Tag Rules" in text
        assert "Anomaly Rules" in text

    async def test_settings_tags_page_loads(self, client):
        r = await client.get("/settings/tags")
        assert r.status_code == 200
        text = (await r.get_data()).decode()
        assert "Tag Rules" in text
        assert "Create Tag Rule" in text

    async def test_auto_tag_rules_preview(self, client):
        ts_ns = int(time.time() * 1_000_000_000)
        payload = {
            "resourceLogs": [
                {
                    "resource": {
                        "attributes": [{"key": "service.name", "value": {"stringValue": f"auto-preview-{ts_ns}"}}]
                    },
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": str(ts_ns),
                                    "severityText": "ERROR",
                                    "severityNumber": 17,
                                    "body": {"stringValue": "preview seed"},
                                    "traceId": "",
                                    "spanId": "",
                                    "attributes": [],
                                }
                            ]
                        }
                    ],
                }
            ]
        }
        r_seed = await client.post("/v1/logs", json=payload)
        assert r_seed.status_code == 200

        r = await client.post(
            "/settings/tags/auto",
            form={
                "action": "preview",
                "hours": "24",
                "min_count": "1",
                "service_filter": f"auto-preview-{ts_ns}",
                "auto_record_types": ["log"],
            },
        )
        assert r.status_code == 200
        text = (await r.get_data()).decode()
        assert "Preview Candidates" in text

    async def test_auto_tag_rules_create(self, client):
        ts_ns = int(time.time() * 1_000_000_000)
        service_name = f"auto-create-{ts_ns}"
        payload = {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service_name}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": str(ts_ns),
                                    "severityText": "INFO",
                                    "severityNumber": 9,
                                    "body": {"stringValue": "create seed"},
                                    "traceId": "",
                                    "spanId": "",
                                    "attributes": [],
                                }
                            ]
                        }
                    ],
                }
            ]
        }
        r_seed = await client.post("/v1/logs", json=payload)
        assert r_seed.status_code == 200

        r = await client.post(
            "/settings/tags/auto",
            form={
                "action": "create",
                "hours": "24",
                "min_count": "1",
                "service_filter": service_name,
                "auto_record_types": ["log"],
            },
        )
        assert r.status_code in (200, 302)

        from app import get_db

        db = get_db()
        created = db.execute(
            "SELECT count() FROM sobs_tag_rules FINAL "
            "WHERE MatchField='service_name' AND MatchValue=? AND TagKey='service' AND IsDeleted=0",
            [service_name],
        ).fetchone()[0]
        assert created >= 1

    # ── Tag rule CRUD ─────────────────────────────────────────────────────────

    async def test_create_tag_rule_and_list(self, client):
        r = await client.post(
            "/settings/tags",
            form={
                "name": "tag-test-rule",
                "record_types": ["log"],
                "match_field": "severity",
                "match_operator": "eq",
                "match_value": "ERROR",
                "match_attr_key": "",
                "tag_key": "priority",
                "tag_value": "high",
            },
        )
        # Should redirect back to the tag rules page
        assert r.status_code in (200, 302)
        r2 = await client.get("/settings/tags")
        text = (await r2.get_data()).decode()
        assert "tag-test-rule" in text
        assert "priority" in text
        assert "high" in text

    async def test_create_tag_rule_missing_fields_rejected(self, client):
        r = await client.post(
            "/settings/tags",
            form={
                "name": "",  # missing
                "match_field": "severity",
                "match_operator": "eq",
                "match_value": "ERROR",
                "tag_key": "k",
                "tag_value": "v",
            },
        )
        # Should redirect with a warning flash (302 → 200 with flash)
        assert r.status_code in (200, 302)

    async def test_delete_tag_rule(self, client):
        # Create a rule to delete
        await client.post(
            "/settings/tags",
            form={
                "name": "to-be-deleted",
                "record_types": ["all"],
                "match_field": "service_name",
                "match_operator": "eq",
                "match_value": "test-svc",
                "match_attr_key": "",
                "tag_key": "delete-me",
                "tag_value": "yes",
            },
        )
        # Look up the rule ID
        from app import get_db

        db = get_db()
        row = db.execute(
            "SELECT Id FROM sobs_tag_rules FINAL WHERE Name='to-be-deleted' AND IsDeleted=0 LIMIT 1"
        ).fetchone()
        assert row is not None, "Rule was not created"
        rule_id = str(row["Id"])

        r = await client.post(f"/settings/tags/{rule_id}/delete")
        assert r.status_code in (200, 302)

        # Verify it's soft-deleted
        row2 = db.execute(
            "SELECT Id FROM sobs_tag_rules FINAL WHERE Id=? AND IsDeleted=0 LIMIT 1",
            [rule_id],
        ).fetchone()
        assert row2 is None, "Rule was not deleted"

    # ── Auto-tagging at ingest ────────────────────────────────────────────────

    async def test_auto_tag_applied_on_log_ingest(self, client):
        """A tag rule matching ERROR severity should tag ingested error logs."""
        # Create a tag rule
        await client.post(
            "/settings/tags",
            form={
                "name": "auto-tag-errors",
                "record_types": ["log"],
                "match_field": "severity",
                "match_operator": "eq",
                "match_value": "ERROR",
                "match_attr_key": "",
                "tag_key": "auto-env",
                "tag_value": "test",
            },
        )

        # Ingest an ERROR log
        ts_ns = int(time.time() * 1_000_000_000)
        payload = {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "tag-svc"}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": str(ts_ns),
                                    "severityText": "ERROR",
                                    "severityNumber": 17,
                                    "body": {"stringValue": "auto-tag test log"},
                                    "traceId": "",
                                    "spanId": "",
                                    "attributes": [],
                                }
                            ]
                        }
                    ],
                }
            ]
        }
        r = await client.post("/v1/logs", json=payload)
        assert r.status_code == 200

        # Check that a tag was created in sobs_record_tags
        from app import get_db

        db = get_db()
        count = db.execute(
            "SELECT count() FROM sobs_record_tags FINAL "
            "WHERE TagKey='auto-env' AND TagValue='test' AND IsAuto=1 AND IsDeleted=0"
        ).fetchone()[0]
        assert count >= 1, "Auto-tag not written to sobs_record_tags"

    async def test_auto_tag_not_applied_for_non_matching_rule(self, client):
        """A tag rule for WARN should NOT tag DEBUG logs."""
        # Ensure there's a rule only for WARN
        await client.post(
            "/settings/tags",
            form={
                "name": "warn-only-rule",
                "record_types": ["log"],
                "match_field": "severity",
                "match_operator": "eq",
                "match_value": "WARN",
                "match_attr_key": "",
                "tag_key": "warn-tagged",
                "tag_value": "yes",
            },
        )

        # Ingest a DEBUG log with a unique service name so we can find it
        ts_ns = int(time.time() * 1_000_000_000)
        unique_service = f"no-warn-svc-{ts_ns}"
        payload = {
            "resourceLogs": [
                {
                    "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": unique_service}}]},
                    "scopeLogs": [
                        {
                            "logRecords": [
                                {
                                    "timeUnixNano": str(ts_ns),
                                    "severityText": "DEBUG",
                                    "severityNumber": 5,
                                    "body": {"stringValue": "debug log no warn"},
                                    "traceId": "",
                                    "spanId": "",
                                    "attributes": [],
                                }
                            ]
                        }
                    ],
                }
            ]
        }
        r = await client.post("/v1/logs", json=payload)
        assert r.status_code == 200

        from app import _record_id_for_log, get_db

        db = get_db()
        # The log row's record ID
        row = db.execute(
            "SELECT Timestamp, TraceId, SpanId FROM otel_logs WHERE ServiceName=? LIMIT 1",
            [unique_service],
        ).fetchone()
        if row:
            rid = _record_id_for_log(str(row["Timestamp"]), unique_service, str(row["TraceId"]), str(row["SpanId"]))
            tag_row = db.execute(
                "SELECT count() FROM sobs_record_tags FINAL "
                "WHERE RecordId=? AND TagKey='warn-tagged' AND IsDeleted=0",
                [rid],
            ).fetchone()
            assert tag_row[0] == 0, "WARN rule incorrectly tagged a DEBUG log"

    async def test_auto_tag_applied_on_trace_ingest(self, client):
        tag_key = f"trace-auto-{time.time_ns()}"
        await client.post(
            "/settings/tags",
            form={
                "name": f"trace-rule-{time.time_ns()}",
                "record_types": ["trace"],
                "match_field": "span_name",
                "match_operator": "contains",
                "match_value": "checkout",
                "match_attr_key": "",
                "tag_key": tag_key,
                "tag_value": "yes",
            },
        )

        trace_payload = {
            "resourceSpans": [
                {
                    "resource": {
                        "attributes": [
                            {"key": "service.name", "value": {"stringValue": "trace-tag-svc"}},
                        ]
                    },
                    "scopeSpans": [
                        {
                            "spans": [
                                {
                                    "traceId": "0123456789abcdef0123456789abcdef",
                                    "spanId": "0123456789abcdef",
                                    "name": "checkout request",
                                    "kind": 2,
                                    "startTimeUnixNano": str(time.time_ns()),
                                    "endTimeUnixNano": str(time.time_ns() + 1_000_000),
                                    "attributes": [],
                                    "status": {"code": 1},
                                }
                            ]
                        }
                    ],
                }
            ]
        }
        r = await client.post("/v1/traces", json=trace_payload)
        assert r.status_code == 200

        from app import get_db

        db = get_db()
        count = db.execute(
            "SELECT count() FROM sobs_record_tags FINAL "
            "WHERE RecordType='trace' AND TagKey=? AND TagValue='yes' AND IsAuto=1 AND IsDeleted=0",
            [tag_key],
        ).fetchone()[0]
        assert count >= 1

    async def test_auto_tag_applied_on_direct_error_ingest(self, client):
        tag_key = f"error-auto-{time.time_ns()}"
        await client.post(
            "/settings/tags",
            form={
                "name": f"error-rule-{time.time_ns()}",
                "record_types": ["error"],
                "match_field": "severity",
                "match_operator": "eq",
                "match_value": "ERROR",
                "match_attr_key": "",
                "tag_key": tag_key,
                "tag_value": "yes",
            },
        )

        r = await client.post(
            "/v1/errors",
            json={"service": "err-tag-svc", "type": "ValueError", "message": "boom"},
        )
        assert r.status_code == 200

        from app import get_db

        db = get_db()
        count = db.execute(
            "SELECT count() FROM sobs_record_tags FINAL "
            "WHERE RecordType='error' AND TagKey=? AND TagValue='yes' AND IsAuto=1 AND IsDeleted=0",
            [tag_key],
        ).fetchone()[0]
        assert count >= 1

    async def test_auto_tag_applied_on_ai_ingest(self, client):
        tag_key = f"ai-auto-{time.time_ns()}"
        await client.post(
            "/settings/tags",
            form={
                "name": f"ai-rule-{time.time_ns()}",
                "record_types": ["ai"],
                "match_field": "span_name",
                "match_operator": "contains",
                "match_value": "chat",
                "match_attr_key": "",
                "tag_key": tag_key,
                "tag_value": "yes",
            },
        )

        r = await client.post(
            "/v1/ai",
            json={
                "service": "ai-tag-svc",
                "provider": "openai",
                "model": "gpt-4o-mini",
                "operation": "chat",
                "trace_id": "fedcba9876543210fedcba9876543210",
                "span_id": "89abcdef01234567",
            },
        )
        assert r.status_code == 200

        from app import get_db

        db = get_db()
        count = db.execute(
            "SELECT count() FROM sobs_record_tags FINAL "
            "WHERE RecordType='ai' AND TagKey=? AND TagValue='yes' AND IsAuto=1 AND IsDeleted=0",
            [tag_key],
        ).fetchone()[0]
        assert count >= 1

    async def test_auto_tag_applied_on_rum_ingest(self, client):
        tag_key = f"rum-auto-{time.time_ns()}"
        await client.post(
            "/settings/tags",
            form={
                "name": f"rum-rule-{time.time_ns()}",
                "record_types": ["rum"],
                "match_field": "event_type",
                "match_operator": "eq",
                "match_value": "pageview",
                "match_attr_key": "",
                "tag_key": tag_key,
                "tag_value": "yes",
            },
        )

        r = await client.post(
            "/v1/rum",
            json=[
                {
                    "type": "pageview",
                    "timestamp": "2026-01-01T00:00:00Z",
                    "sessionId": "tag-rum-001",
                    "url": "https://example.com/",
                    "service": "browser",
                }
            ],
        )
        assert r.status_code == 200

        from app import get_db

        db = get_db()
        count = db.execute(
            "SELECT count() FROM sobs_record_tags FINAL "
            "WHERE RecordType='rum' AND TagKey=? AND TagValue='yes' AND IsAuto=1 AND IsDeleted=0",
            [tag_key],
        ).fetchone()[0]
        assert count >= 1

    # ── Record tag API ────────────────────────────────────────────────────────

    async def test_api_add_and_get_tag(self, client):
        record_id = "deadbeef01234567deadbeef01234567"
        # Add a manual tag
        r = await client.post(
            f"/api/tags/log/{record_id}",
            json={"key": "env", "value": "staging"},
        )
        assert r.status_code == 201

        # Retrieve tags
        r2 = await client.get(f"/api/tags/log/{record_id}")
        assert r2.status_code == 200
        data = await r2.get_json()
        tags = data["tags"]
        assert any(t["key"] == "env" and t["value"] == "staging" for t in tags)

    async def test_api_add_tag_missing_key_returns_400(self, client):
        r = await client.post(
            "/api/tags/log/someid",
            json={"value": "no-key"},
        )
        assert r.status_code == 400

    async def test_api_delete_tag(self, client):
        record_id = "cafebabe00000000cafebabe00000000"
        await client.post(
            f"/api/tags/trace/{record_id}",
            json={"key": "temp-tag", "value": "remove-me"},
        )
        r = await client.delete(f"/api/tags/trace/{record_id}/temp-tag")
        assert r.status_code == 200

        r2 = await client.get(f"/api/tags/trace/{record_id}")
        data = await r2.get_json()
        assert not any(t["key"] == "temp-tag" for t in data["tags"])

    async def test_api_delete_nonexistent_tag_returns_404(self, client):
        r = await client.delete("/api/tags/log/nonexistentid/no-such-key")
        assert r.status_code == 404

    async def test_api_delete_tag_removes_all_values_for_key(self, client):
        record_id = "facefeedfacefeedfacefeedfacefeed"
        await client.post(f"/api/tags/log/{record_id}", json={"key": "env", "value": "staging"})
        await client.post(f"/api/tags/log/{record_id}", json={"key": "env", "value": "prod"})

        r = await client.delete(f"/api/tags/log/{record_id}/env")
        assert r.status_code == 200

        r2 = await client.get(f"/api/tags/log/{record_id}")
        assert r2.status_code == 200
        data = await r2.get_json()
        assert not any(t["key"] == "env" for t in data["tags"])

    # ── Helper functions ──────────────────────────────────────────────────────

    def test_record_id_for_log_stable(self):
        from app import _record_id_for_log

        rid1 = _record_id_for_log("2026-01-01T00:00:00", "svc", "traceid", "spanid")
        rid2 = _record_id_for_log("2026-01-01T00:00:00", "svc", "traceid", "spanid")
        assert rid1 == rid2
        assert len(rid1) == 32

    def test_record_id_for_span_stable(self):
        from app import _record_id_for_span

        rid1 = _record_id_for_span("traceid", "spanid")
        rid2 = _record_id_for_span("traceid", "spanid")
        assert rid1 == rid2
        assert len(rid1) == 32

    def test_record_id_for_log_differs_by_fields(self):
        from app import _record_id_for_log

        rid1 = _record_id_for_log("2026-01-01T00:00:00", "svc-a", "t1", "s1")
        rid2 = _record_id_for_log("2026-01-01T00:00:00", "svc-b", "t1", "s1")
        assert rid1 != rid2

    def test_match_tag_rule_eq_severity(self):
        from app import _match_tag_rule

        rule = {
            "record_types": ["log"],
            "match_field": "severity",
            "match_operator": "eq",
            "match_value": "ERROR",
            "match_attr_key": "",
            "tag_key": "k",
            "tag_value": "v",
        }
        assert _match_tag_rule(rule, "log", "svc", "ERROR", "body", {}) is True
        assert _match_tag_rule(rule, "log", "svc", "WARN", "body", {}) is False

    def test_match_tag_rule_contains_body(self):
        from app import _match_tag_rule

        rule = {
            "record_types": ["all"],
            "match_field": "body",
            "match_operator": "contains",
            "match_value": "timeout",
            "match_attr_key": "",
            "tag_key": "k",
            "tag_value": "v",
        }
        assert _match_tag_rule(rule, "log", "svc", "ERROR", "connection timeout error", {}) is True
        assert _match_tag_rule(rule, "log", "svc", "ERROR", "success", {}) is False

    def test_match_tag_rule_regex(self):
        from app import _match_tag_rule

        rule = {
            "record_types": ["all"],
            "match_field": "service_name",
            "match_operator": "regex",
            "match_value": r"^prod-",
            "match_attr_key": "",
            "tag_key": "k",
            "tag_value": "v",
        }
        assert _match_tag_rule(rule, "log", "prod-api", "", "", {}) is True
        assert _match_tag_rule(rule, "log", "staging-api", "", "", {}) is False

    def test_match_tag_rule_attribute(self):
        from app import _match_tag_rule

        rule = {
            "record_types": ["trace"],
            "match_field": "attribute",
            "match_operator": "eq",
            "match_value": "500",
            "match_attr_key": "http.status_code",
            "tag_key": "k",
            "tag_value": "v",
        }
        attrs = {"http.status_code": "500"}
        assert _match_tag_rule(rule, "trace", "svc", "", "", attrs) is True
        attrs2 = {"http.status_code": "200"}
        assert _match_tag_rule(rule, "trace", "svc", "", "", attrs2) is False

    def test_match_tag_rule_wrong_record_type(self):
        from app import _match_tag_rule

        rule = {
            "record_types": ["trace"],
            "match_field": "severity",
            "match_operator": "eq",
            "match_value": "ERROR",
            "match_attr_key": "",
            "tag_key": "k",
            "tag_value": "v",
        }
        # Rule only applies to traces, not logs
        assert _match_tag_rule(rule, "log", "svc", "ERROR", "", {}) is False
        assert _match_tag_rule(rule, "trace", "svc", "ERROR", "", {}) is True

    def test_match_tag_rule_invalid_regex_returns_false(self):
        from app import _match_tag_rule

        rule = {
            "record_types": ["all"],
            "match_field": "body",
            "match_operator": "regex",
            "match_value": "[invalid",
            "match_attr_key": "",
            "tag_key": "k",
            "tag_value": "v",
        }
        # Invalid regex must not raise, just return False
        assert _match_tag_rule(rule, "log", "svc", "ERROR", "any body", {}) is False


# ---------------------------------------------------------------------------
# Notifications & Webhooks
# ---------------------------------------------------------------------------
class TestNotifications:
    async def test_notifications_page_loads(self, client):
        r = await client.get("/settings/notifications")
        assert r.status_code == 200
        text = (await r.get_data()).decode()
        assert "Notifications" in text
        assert "Notification Channels" in text
        assert "Notification Rules" in text

    async def test_notifications_page_registers_service_worker(self, client):
        r = await client.get("/settings/notifications")
        assert r.status_code == 200
        text = (await r.get_data()).decode()
        assert "/service-worker.js" in text
        assert "navigator.serviceWorker.register" in text

    async def test_service_worker_js_route_serves_push_handlers(self, client):
        r = await client.get("/service-worker.js")
        assert r.status_code == 200
        assert r.content_type.startswith("application/javascript")
        body = (await r.get_data()).decode()
        assert "addEventListener('push'" in body
        assert "showNotification" in body

    async def test_create_webhook_channel(self, client):
        r = await client.post(
            "/settings/notifications/channels",
            form={
                "name": "Test Webhook",
                "channel_type": "webhook",
                "webhook_url": "https://example.com/hook",
                "webhook_method": "POST",
                "webhook_headers": "{}",
                "webhook_body_template": "",
            },
        )
        assert r.status_code in (200, 302)
        # Check it appears in the notifications page
        r2 = await client.get("/settings/notifications")
        text = (await r2.get_data()).decode()
        assert "Test Webhook" in text

    async def test_create_slack_channel(self, client):
        r = await client.post(
            "/settings/notifications/channels",
            form={
                "name": "Slack Ops",
                "channel_type": "slack",
                "slack_webhook_url": "https://hooks.slack.com/services/TEST",
            },
        )
        assert r.status_code in (200, 302)
        r2 = await client.get("/settings/notifications")
        text = (await r2.get_data()).decode()
        assert "Slack Ops" in text

    async def test_create_channel_missing_name_rejected(self, client):
        r = await client.post(
            "/settings/notifications/channels",
            form={
                "name": "",
                "channel_type": "webhook",
                "webhook_url": "https://example.com/hook",
            },
        )
        assert r.status_code in (200, 302)
        # Should not have added a channel
        r2 = await client.get("/settings/notifications")
        text = (await r2.get_data()).decode()
        # No empty-named channel should exist
        assert "channel not found" not in text.lower()

    async def test_create_channel_invalid_type_rejected(self, client):
        r = await client.post(
            "/settings/notifications/channels",
            form={
                "name": "Bad Type",
                "channel_type": "invalid_type",
            },
        )
        assert r.status_code in (200, 302)

    async def test_toggle_and_delete_channel(self, client):
        # Create a channel first
        await client.post(
            "/settings/notifications/channels",
            form={
                "name": "Toggle Test Channel",
                "channel_type": "slack",
                "slack_webhook_url": "https://hooks.slack.com/services/TOGGLE",
            },
        )
        # Find the channel ID
        channels = sobs_app._load_notification_channels(sobs_app.get_db())
        ch = next((c for c in channels if c["name"] == "Toggle Test Channel"), None)
        assert ch is not None
        ch_id = ch["id"]

        # Toggle
        r = await client.post(f"/settings/notifications/channels/{ch_id}/toggle")
        assert r.status_code in (200, 302)

        # Delete
        r = await client.post(f"/settings/notifications/channels/{ch_id}/delete")
        assert r.status_code in (200, 302)
        channels = sobs_app._load_notification_channels(sobs_app.get_db())
        assert all(c["id"] != ch_id for c in channels)

    async def test_create_notification_rule(self, client):
        # Create a channel first
        await client.post(
            "/settings/notifications/channels",
            form={
                "name": "Rule Test Channel",
                "channel_type": "slack",
                "slack_webhook_url": "https://hooks.slack.com/services/RULE_TEST",
            },
        )
        channels = sobs_app._load_notification_channels(sobs_app.get_db())
        ch = next((c for c in channels if c["name"] == "Rule Test Channel"), None)
        assert ch is not None

        r = await client.post(
            "/settings/notifications/rules",
            form={
                "name": "High Error Rate",
                "logic_operator": "any",
                "severity": "warning",
                "cooldown_seconds": "60",
                "channel_ids": ch["id"],
                "cond_source": "logs",
                "cond_signal": "error_volume",
                "cond_service": "",
                "cond_comparator": "gt",
                "cond_threshold": "10",
                "cond_window_minutes": "5",
            },
        )
        assert r.status_code in (200, 302)

        rules = sobs_app._load_notification_rules(sobs_app.get_db())
        rule = next((r for r in rules if r["name"] == "High Error Rate"), None)
        assert rule is not None
        assert rule["severity"] == "warning"
        assert rule["logic_operator"] == "any"
        assert len(rule["conditions"]) == 1
        assert rule["conditions"][0]["signal"] == "error_volume"

    async def test_create_rule_missing_name_rejected(self, client):
        r = await client.post(
            "/settings/notifications/rules",
            form={
                "name": "",
                "logic_operator": "any",
                "severity": "warning",
                "cooldown_seconds": "60",
                "cond_source": "logs",
                "cond_signal": "error_volume",
                "cond_comparator": "gt",
                "cond_threshold": "10",
                "cond_window_minutes": "5",
            },
        )
        assert r.status_code in (200, 302)

    async def test_create_rule_no_conditions_rejected(self, client):
        r = await client.post(
            "/settings/notifications/rules",
            form={
                "name": "No Conditions Rule",
                "logic_operator": "any",
                "severity": "warning",
                "cooldown_seconds": "60",
            },
        )
        assert r.status_code in (200, 302)
        rules = sobs_app._load_notification_rules(sobs_app.get_db())
        assert all(r["name"] != "No Conditions Rule" for r in rules)

    async def test_toggle_and_delete_rule(self, client):
        # Create a channel and rule first
        await client.post(
            "/settings/notifications/channels",
            form={
                "name": "Toggle Rule Channel",
                "channel_type": "slack",
                "slack_webhook_url": "https://hooks.slack.com/services/TOGGLE_RULE",
            },
        )
        channels = sobs_app._load_notification_channels(sobs_app.get_db())
        ch = next((c for c in channels if c["name"] == "Toggle Rule Channel"), None)
        assert ch is not None

        await client.post(
            "/settings/notifications/rules",
            form={
                "name": "Toggle Test Rule",
                "logic_operator": "all",
                "severity": "critical",
                "cooldown_seconds": "120",
                "channel_ids": ch["id"],
                "cond_source": "traces",
                "cond_signal": "trace_error_ratio",
                "cond_service": "",
                "cond_comparator": "gt",
                "cond_threshold": "0.5",
                "cond_window_minutes": "10",
            },
        )
        rules = sobs_app._load_notification_rules(sobs_app.get_db())
        rule = next((r for r in rules if r["name"] == "Toggle Test Rule"), None)
        assert rule is not None
        rule_id = rule["id"]

        # Toggle
        r = await client.post(f"/settings/notifications/rules/{rule_id}/toggle")
        assert r.status_code in (200, 302)

        # Delete
        r = await client.post(f"/settings/notifications/rules/{rule_id}/delete")
        assert r.status_code in (200, 302)
        rules = sobs_app._load_notification_rules(sobs_app.get_db())
        assert all(r["id"] != rule_id for r in rules)

    async def test_check_notifications_api_returns_json(self, client):
        r = await client.post("/api/notifications/check")
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["ok"] is True
        assert "evaluated" in data
        assert "fired" in data
        assert isinstance(data["results"], list)

    async def test_subscribe_browser_push_requires_fields(self, client):
        r = await client.post(
            "/api/notifications/subscribe",
            json={"name": "My Browser"},
        )
        assert r.status_code == 400
        data = json.loads(await r.get_data())
        assert data["ok"] is False

    async def test_subscribe_browser_push_valid(self, client):
        r = await client.post(
            "/api/notifications/subscribe",
            json={
                "name": "Test Browser",
                "endpoint": "https://push.example.com/test-endpoint",
                "p256dh": "BPUT=",
                "auth": "AUTH=",
            },
        )
        assert r.status_code == 200
        data = json.loads(await r.get_data())
        assert data["ok"] is True
        assert "channel_id" in data

    async def test_subscribe_browser_push_deduplication(self, client):
        endpoint = "https://push.example.com/dedup-endpoint"
        # First subscription
        r1 = await client.post(
            "/api/notifications/subscribe",
            json={
                "name": "Browser Dedup",
                "endpoint": endpoint,
                "p256dh": "BPUT=",
                "auth": "AUTH=",
            },
        )
        data1 = json.loads(await r1.get_data())
        assert data1["ok"] is True
        assert data1["existing"] is False

        # Second subscription with same endpoint
        r2 = await client.post(
            "/api/notifications/subscribe",
            json={
                "name": "Browser Dedup Again",
                "endpoint": endpoint,
                "p256dh": "BPUT=",
                "auth": "AUTH=",
            },
        )
        data2 = json.loads(await r2.get_data())
        assert data2["ok"] is True
        assert data2["existing"] is True
        assert data2["channel_id"] == data1["channel_id"]

    async def test_vapid_public_key_not_configured(self, client):
        """Without SOBS_VAPID_PRIVATE_KEY set, endpoint returns 404."""
        r = await client.get("/api/notifications/vapid-public-key")
        assert r.status_code == 404
        data = json.loads(await r.get_data())
        assert data["ok"] is False

    async def test_settings_page_shows_notification_counts(self, client):
        r = await client.get("/settings")
        assert r.status_code == 200
        text = (await r.get_data()).decode()
        assert "Notifications" in text
        assert "channel" in text.lower()

    def test_build_notification_payload(self):
        from app import _build_notification_payload

        rule = {"name": "Test Rule", "severity": "warning"}
        conditions = [
            {
                "source": "logs",
                "signal": "error_volume",
                "service": "api",
                "comparator": "gt",
                "threshold": 10,
                "_value": 15.0,
            }
        ]
        payload = _build_notification_payload(rule, conditions)
        assert payload["rule_name"] == "Test Rule"
        assert payload["severity"] == "warning"
        assert "Test Rule" in payload["summary"]
        assert "error_volume" in payload["summary"]

    def test_mask_channel_config_hides_password(self):
        from app import _mask_channel_config

        config = {
            "smtp_host": "mail.example.com",
            "smtp_password": "supersecret",
            "to_addr": "admin@example.com",
        }
        masked = _mask_channel_config("email", config)
        assert masked["smtp_password"] == "••••••••"
        assert masked["smtp_host"] == "mail.example.com"
        assert masked["to_addr"] == "admin@example.com"

    def test_mask_channel_config_empty_password_unchanged(self):
        from app import _mask_channel_config

        config = {"smtp_host": "mail.example.com", "smtp_password": ""}
        masked = _mask_channel_config("email", config)
        assert masked["smtp_password"] == ""

    def test_notification_tables_exist(self, client):
        tables = {
            row[0]
            for row in sobs_app.get_db()
            .execute(
                "SELECT name FROM system.tables WHERE database='default' "
                "AND name IN ('sobs_notification_channels', 'sobs_notification_rules', 'sobs_notification_log')"
            )
            .fetchall()
        }
        assert "sobs_notification_channels" in tables
        assert "sobs_notification_rules" in tables
        assert "sobs_notification_log" in tables
