package store

import "testing"

func TestChdbConnectTargetPlain(t *testing.T) {
	t.Setenv("SOBS_CLICKHOUSE_CONFIG_FILE", "")
	got, err := chdbConnectTarget("/data/sobs.chdb")
	if err != nil || got != "/data/sobs.chdb" {
		t.Errorf("unset: got %q err %v, want plain path", got, err)
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
