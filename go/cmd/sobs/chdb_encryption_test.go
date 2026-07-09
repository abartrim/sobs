package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderClickhouseConfigDisabled(t *testing.T) {
	t.Setenv("SOBS_CHDB_ENCRYPTION_KEY", "")
	out, err := renderClickhouseConfig()
	if err != nil || out != "" {
		t.Errorf("disabled: out=%q err=%v, want empty/no-op", out, err)
	}
}

func TestRenderClickhouseConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_CHDB_ENCRYPTION_KEY", "00112233445566778899aabbccddeeff")
	t.Setenv("SOBS_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("SOBS_CHDB_CONFIG_RENDER_PATH", filepath.Join(dir, "cfg", "clickhouse-config.xml"))
	// disk name / policy name defaults

	out, err := renderClickhouseConfig()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read rendered config: %v", err)
	}
	xml := string(data)
	for _, want := range []string{
		"<key_hex>00112233445566778899aabbccddeeff</key_hex>",
		"<encrypted_disk>",
		"<algorithm>AES_128_CTR</algorithm>",
		"<encrypted_only>",
		"<type>encrypted</type>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("rendered config missing %q\n%s", want, xml)
		}
	}
}

func TestRenderClickhouseConfigBadHex(t *testing.T) {
	t.Setenv("SOBS_CHDB_ENCRYPTION_KEY", "not-hex!!")
	if _, err := renderClickhouseConfig(); err == nil {
		t.Error("expected error on non-hex key")
	}
}

func TestSetupChdbEncryptionDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOBS_CHDB_ENCRYPTION_KEY", "deadbeef")
	t.Setenv("SOBS_DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("SOBS_CHDB_CONFIG_RENDER_PATH", filepath.Join(dir, "cfg.xml"))
	t.Setenv("SOBS_CLICKHOUSE_CONFIG_FILE", "")
	t.Setenv("SOBS_CHDB_EXPECT_DISK", "")
	t.Setenv("SOBS_CHDB_EXPECT_STORAGE_POLICY", "")

	if err := setupChdbEncryption(); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if os.Getenv("SOBS_CLICKHOUSE_CONFIG_FILE") == "" {
		t.Error("SOBS_CLICKHOUSE_CONFIG_FILE not exported")
	}
	if got := os.Getenv("SOBS_CHDB_EXPECT_DISK"); got != "encrypted_disk" {
		t.Errorf("expect disk = %q, want encrypted_disk", got)
	}
	if got := os.Getenv("SOBS_CHDB_EXPECT_STORAGE_POLICY"); got != "encrypted_only" {
		t.Errorf("expect policy = %q, want encrypted_only", got)
	}
}
