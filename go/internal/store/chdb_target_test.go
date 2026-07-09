package store

import "testing"

func TestChdbConnectTargetDefaults(t *testing.T) {
	t.Setenv("SOBS_CLICKHOUSE_CONFIG_FILE", "")
	t.Setenv("SOBS_CHDB_MAX_SERVER_MB", "")
	t.Setenv("SOBS_CHDB_MARK_CACHE_MB", "")
	t.Setenv("SOBS_CHDB_UNCOMPRESSED_CACHE_MB", "")
	got, err := chdbConnectTarget("/data/sobs.chdb")
	want := "/data/sobs.chdb?max_server_memory_usage=805306368&mark_cache_size=67108864" +
		"&uncompressed_cache_size=67108864&background_pool_size=2" +
		"&background_schedule_pool_size=16&background_io_pool_size=2"
	if err != nil || got != want {
		t.Errorf("defaults:\n got %q\nwant %q\nerr %v", got, want, err)
	}
}

func TestChdbConnectTargetMemOverride(t *testing.T) {
	t.Setenv("SOBS_CLICKHOUSE_CONFIG_FILE", "")
	t.Setenv("SOBS_CHDB_MAX_SERVER_MB", "0") // 0 == unlimited (integration CI disables the cap)
	got, _ := chdbConnectTarget("/data/sobs.chdb")
	if got != "/data/sobs.chdb?max_server_memory_usage=0&mark_cache_size=67108864"+
		"&uncompressed_cache_size=67108864&background_pool_size=2"+
		"&background_schedule_pool_size=16&background_io_pool_size=2" {
		t.Errorf("override not applied: %q", got)
	}
}

func TestChdbConnectTargetConfigFile(t *testing.T) {
	t.Setenv("SOBS_CLICKHOUSE_CONFIG_FILE", "/tmp/sobs-clickhouse-config.xml")
	got, err := chdbConnectTarget("/data/sobs.chdb")
	want := "/data/sobs.chdb?config-file=/tmp/sobs-clickhouse-config.xml"
	if err != nil || got != want {
		t.Errorf("got %q err %v, want %q", got, err, want)
	}
}

func TestChdbConnectTargetRelativeRejected(t *testing.T) {
	t.Setenv("SOBS_CLICKHOUSE_CONFIG_FILE", "relative/config.xml")
	if _, err := chdbConnectTarget("/data/sobs.chdb"); err == nil {
		t.Error("expected error for non-absolute config-file")
	}
}

func TestQuotePathSafeSlash(t *testing.T) {
	cases := map[string]string{
		"/tmp/sobs-clickhouse-config.xml": "/tmp/sobs-clickhouse-config.xml",
		"/a b/c.xml":                      "/a%20b/c.xml",
		"/a&b":                            "/a%26b",
	}
	for in, want := range cases {
		if got := quotePathSafeSlash(in); got != want {
			t.Errorf("quotePathSafeSlash(%q) = %q, want %q", in, got, want)
		}
	}
}
