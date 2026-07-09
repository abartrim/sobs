package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// chdb disk-encryption support — a Go port of scripts/render_clickhouse_config.py +
// scripts/docker-entrypoint.sh + app.py _validate_chdb_startup_configuration, so the slim Go image
// can offer the same SOBS_CHDB_ENCRYPTION_KEY runtime-encrypted-disk feature WITHOUT shipping a
// Python interpreter or a shell entrypoint. Everything is gated on SOBS_CHDB_ENCRYPTION_KEY (and
// the EXPECT_* assertions on their own env vars), all unset in parity, so it is a strict no-op
// there — the chdb store opens exactly as before.

var chdbKeyHexRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

// setupChdbEncryption mirrors scripts/docker-entrypoint.sh: when SOBS_CHDB_ENCRYPTION_KEY is set,
// render a ClickHouse config.xml, point the store at it via SOBS_CLICKHOUSE_CONFIG_FILE, and
// default the disk/policy startup assertions. Runs in main() before the store opens.
func setupChdbEncryption() error {
	if strings.TrimSpace(os.Getenv("SOBS_CHDB_ENCRYPTION_KEY")) == "" {
		return nil
	}
	out, err := renderClickhouseConfig()
	if err != nil {
		return err
	}
	if err := os.Setenv("SOBS_CLICKHOUSE_CONFIG_FILE", out); err != nil {
		return err
	}
	if os.Getenv("SOBS_CHDB_EXPECT_DISK") == "" {
		_ = os.Setenv("SOBS_CHDB_EXPECT_DISK", envTrim("SOBS_CHDB_ENCRYPTED_DISK_NAME", "encrypted_disk"))
	}
	if os.Getenv("SOBS_CHDB_EXPECT_STORAGE_POLICY") == "" {
		_ = os.Setenv("SOBS_CHDB_EXPECT_STORAGE_POLICY", envTrim("SOBS_CHDB_STORAGE_POLICY_NAME", "encrypted_only"))
	}
	return nil
}

// renderClickhouseConfig ports scripts/render_clickhouse_config.py: emit the encrypted-disk
// config.xml from the SOBS_CHDB_* env vars and return its absolute path.
func renderClickhouseConfig() (string, error) {
	keyHex := strings.TrimSpace(os.Getenv("SOBS_CHDB_ENCRYPTION_KEY"))
	if keyHex == "" {
		return "", nil
	}
	if !chdbKeyHexRe.MatchString(keyHex) {
		return "", fmt.Errorf("SOBS_CHDB_ENCRYPTION_KEY must be a hex string")
	}
	dataDir, err := mustAbs(envTrim("SOBS_DATA_DIR", "/data"), "SOBS_DATA_DIR")
	if err != nil {
		return "", err
	}
	baseDiskPath, err := mustAbs(envTrim("SOBS_CHDB_BASE_DISK_PATH", dataDir+"/chdb-disks/plain"), "SOBS_CHDB_BASE_DISK_PATH")
	if err != nil {
		return "", err
	}
	encDiskPath, err := mustAbs(envTrim("SOBS_CHDB_ENCRYPTED_DISK_PATH", dataDir+"/chdb-disks/encrypted"), "SOBS_CHDB_ENCRYPTED_DISK_PATH")
	if err != nil {
		return "", err
	}
	outPath, err := mustAbs(envTrim("SOBS_CHDB_CONFIG_RENDER_PATH", "/tmp/sobs-clickhouse-config.xml"), "SOBS_CHDB_CONFIG_RENDER_PATH")
	if err != nil {
		return "", err
	}
	diskName := envTrim("SOBS_CHDB_ENCRYPTED_DISK_NAME", "encrypted_disk")
	policyName := envTrim("SOBS_CHDB_STORAGE_POLICY_NAME", "encrypted_only")

	for _, d := range []string{baseDiskPath, encDiskPath, filepath.Dir(outPath)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	xml := fmt.Sprintf(`<clickhouse>
  <custom_local_disks_base_directory>%s</custom_local_disks_base_directory>
  <storage_configuration>
    <disks>
      <plain>
        <type>local</type>
        <path>%s/</path>
      </plain>
      <%s>
        <type>encrypted</type>
        <disk>plain</disk>
        <path>%s/</path>
        <algorithm>AES_128_CTR</algorithm>
        <key_hex>%s</key_hex>
      </%s>
    </disks>
    <policies>
      <%s>
        <volumes>
          <main>
            <disk>%s</disk>
          </main>
        </volumes>
      </%s>
    </policies>
  </storage_configuration>
</clickhouse>
`, dataDir, baseDiskPath, diskName, encDiskPath, keyHex, diskName, policyName, diskName, policyName)

	if err := os.WriteFile(outPath, []byte(xml), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", outPath, err)
	}
	return outPath, nil
}

// validateChdbStartup mirrors app.py _validate_chdb_startup_configuration: assert the expected
// encrypted disk + storage policy actually exist after chdb starts (a misapplied config-file fails
// loudly rather than silently writing to plain disk). No-op unless the expectations are set.
func (s *server) validateChdbStartup() error {
	expectDisk := strings.TrimSpace(os.Getenv("SOBS_CHDB_EXPECT_DISK"))
	expectPolicy := strings.TrimSpace(os.Getenv("SOBS_CHDB_EXPECT_STORAGE_POLICY"))
	if expectDisk == "" && expectPolicy == "" {
		return nil
	}
	if s.db == nil {
		return fmt.Errorf("chDB not open; cannot validate storage configuration")
	}
	diskNames := s.chdbNameSet("SELECT name FROM system.disks", "name")
	policyNames := s.chdbNameSet("SELECT DISTINCT policy_name FROM system.storage_policies", "policy_name")

	var missing []string
	if expectDisk != "" && !diskNames[expectDisk] {
		missing = append(missing, fmt.Sprintf("disk '%s'", expectDisk))
	}
	if expectPolicy != "" && !policyNames[expectPolicy] {
		missing = append(missing, fmt.Sprintf("storage policy '%s'", expectPolicy))
	}
	if len(missing) > 0 {
		return fmt.Errorf("chDB started but expected storage configuration was not applied; missing %s. "+
			"This usually means the config-file startup argument was ignored or invalid.", strings.Join(missing, ", "))
	}
	return nil
}

func (s *server) chdbNameSet(sql, col string) map[string]bool {
	out := map[string]bool{}
	res, err := s.db.Execute(sql)
	if err != nil {
		return out
	}
	for _, row := range rowMaps(res) {
		if v := cStr(row, col); v != "" {
			out[v] = true
		}
	}
	return out
}

func envTrim(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func mustAbs(p, varName string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s must be an absolute path, got: %s", varName, p)
	}
	return p, nil
}
