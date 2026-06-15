package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileOrEnv(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretPath, []byte("  from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// *_FILE wins when set and readable+non-empty (trimmed).
	t.Setenv("X_VAL", "from-env")
	t.Setenv("X_VAL_FILE", secretPath)
	if got := readFileOrEnv("X_VAL", "X_VAL_FILE"); got != "from-file" {
		t.Errorf("file precedence: got %q, want %q", got, "from-file")
	}

	// Empty/missing file path -> fall back to the direct env var.
	t.Setenv("X_VAL_FILE", "")
	if got := readFileOrEnv("X_VAL", "X_VAL_FILE"); got != "from-env" {
		t.Errorf("env fallback: got %q, want %q", got, "from-env")
	}

	// Unreadable file path -> fall back to env (no panic).
	t.Setenv("X_VAL_FILE", filepath.Join(dir, "does-not-exist"))
	if got := readFileOrEnv("X_VAL", "X_VAL_FILE"); got != "from-env" {
		t.Errorf("missing-file fallback: got %q, want %q", got, "from-env")
	}

	// No file-env name configured -> direct env, trimmed.
	t.Setenv("X_VAL", "  trimmed  ")
	if got := readFileOrEnv("X_VAL", ""); got != "trimmed" {
		t.Errorf("trim: got %q, want %q", got, "trimmed")
	}
}

func TestAIEnvOverrideMapsAligned(t *testing.T) {
	// Every override key must have a matching *_FILE entry (and vice versa), mirroring app.py.
	for k := range aiEnvOverrides {
		if _, ok := aiEnvFileOverrides[k]; !ok {
			t.Errorf("aiEnvFileOverrides missing key %q", k)
		}
	}
	for k := range aiEnvFileOverrides {
		if _, ok := aiEnvOverrides[k]; !ok {
			t.Errorf("aiEnvOverrides missing key %q", k)
		}
	}
}
