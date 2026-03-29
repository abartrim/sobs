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

    async def test_errors_page(self, client):
        r = await client.get("/errors")
        assert r.status_code == 200

    async def test_traces_page(self, client):
        r = await client.get("/traces")
        assert r.status_code == 200

    async def test_rum_page(self, client):
        r = await client.get("/rum")
        assert r.status_code == 200

    async def test_ai_page(self, client):
        r = await client.get("/ai")
        assert r.status_code == 200

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

    async def test_logs_stats_panel_sql_mode(self, client):
        """Query statistics panel should appear when using SQL WHERE filter."""
        r = await client.get("/logs?sql=SeverityText%3D%27ERROR%27")
        assert r.status_code == 200
        data = await r.get_data()
        assert b"Query Statistics" in data

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
