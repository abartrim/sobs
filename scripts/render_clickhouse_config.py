#!/usr/bin/env python3
"""Render a ClickHouse config.xml for embedded chDB encrypted-disk startup.

Used by the Python oracle image's entrypoint. The published Go image ports this to
go/cmd/sobs/chdb_encryption.go (renderClickhouseConfig), so it needs no Python at runtime.
"""

from __future__ import annotations

import os
import re
from pathlib import Path


def _must_be_abs(path_value: str, var_name: str) -> str:
    if not os.path.isabs(path_value):
        raise RuntimeError(f"{var_name} must be an absolute path, got: {path_value}")
    return path_value


def _get_env(name: str, default: str = "") -> str:
    return os.environ.get(name, default).strip()


def main() -> None:
    key_hex = _get_env("SOBS_CHDB_ENCRYPTION_KEY")
    if not key_hex:
        return
    if not re.fullmatch(r"[0-9a-fA-F]+", key_hex):
        raise RuntimeError("SOBS_CHDB_ENCRYPTION_KEY must be a hex string")

    data_dir = _must_be_abs(_get_env("SOBS_DATA_DIR", "/data"), "SOBS_DATA_DIR")
    base_disk_path = _must_be_abs(
        _get_env("SOBS_CHDB_BASE_DISK_PATH", f"{data_dir}/chdb-disks/plain"),
        "SOBS_CHDB_BASE_DISK_PATH",
    )
    encrypted_disk_path = _must_be_abs(
        _get_env("SOBS_CHDB_ENCRYPTED_DISK_PATH", f"{data_dir}/chdb-disks/encrypted"),
        "SOBS_CHDB_ENCRYPTED_DISK_PATH",
    )
    output_path = _must_be_abs(
        _get_env("SOBS_CHDB_CONFIG_RENDER_PATH", "/tmp/sobs-clickhouse-config.xml"),
        "SOBS_CHDB_CONFIG_RENDER_PATH",
    )

    disk_name = _get_env("SOBS_CHDB_ENCRYPTED_DISK_NAME", "encrypted_disk")
    policy_name = _get_env("SOBS_CHDB_STORAGE_POLICY_NAME", "encrypted_only")

    Path(base_disk_path).mkdir(parents=True, exist_ok=True)
    Path(encrypted_disk_path).mkdir(parents=True, exist_ok=True)
    Path(output_path).parent.mkdir(parents=True, exist_ok=True)

    config_xml = f"""<clickhouse>
  <custom_local_disks_base_directory>{data_dir}</custom_local_disks_base_directory>
  <storage_configuration>
    <disks>
      <plain>
        <type>local</type>
        <path>{base_disk_path}/</path>
      </plain>
      <{disk_name}>
        <type>encrypted</type>
        <disk>plain</disk>
        <path>{encrypted_disk_path}/</path>
        <algorithm>AES_128_CTR</algorithm>
        <key_hex>{key_hex}</key_hex>
      </{disk_name}>
    </disks>
    <policies>
      <{policy_name}>
        <volumes>
          <main>
            <disk>{disk_name}</disk>
          </main>
        </volumes>
      </{policy_name}>
    </policies>
  </storage_configuration>
</clickhouse>
"""
    Path(output_path).write_text(config_xml, encoding="utf-8")


if __name__ == "__main__":
    main()
