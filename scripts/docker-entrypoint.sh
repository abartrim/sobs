#!/bin/sh
set -eu

# Entrypoint for the Python oracle image. The published Go image (Dockerfile.go) needs no shell
# entrypoint: the Go binary renders the encrypted-disk ClickHouse config itself at startup
# (setupChdbEncryption / renderClickhouseConfig in go/cmd/sobs/chdb_encryption.go).

if [ -n "${SOBS_CHDB_ENCRYPTION_KEY:-}" ]; then
  python /app/scripts/render_clickhouse_config.py
  export SOBS_CLICKHOUSE_CONFIG_FILE="${SOBS_CHDB_CONFIG_RENDER_PATH:-/tmp/sobs-clickhouse-config.xml}"
  : "${SOBS_CHDB_EXPECT_DISK:=${SOBS_CHDB_ENCRYPTED_DISK_NAME:-encrypted_disk}}"
  : "${SOBS_CHDB_EXPECT_STORAGE_POLICY:=${SOBS_CHDB_STORAGE_POLICY_NAME:-encrypted_only}}"
  export SOBS_CHDB_EXPECT_DISK
  export SOBS_CHDB_EXPECT_STORAGE_POLICY
  echo "INFO generated chDB ClickHouse config at ${SOBS_CLICKHOUSE_CONFIG_FILE}" >&2
fi

exec "$@"
