package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"testing"
)

// ---- pyStrOr ---------------------------------------------------------------------------------

func TestPyStrOrCVE(t *testing.T) {
	cases := []struct {
		name    string
		v       any
		present bool
		want    string
	}{
		{"absent", nil, false, ""},
		{"nil present", nil, true, ""},
		{"falsy empty string", "", true, ""},
		{"falsy zero number", json.Number("0"), true, ""},
		{"truthy string", "hello", true, "hello"},
		{"truthy number", json.Number("42"), true, "42"},
		{"truthy bool", true, true, "True"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pyStrOr(c.v, c.present); got != c.want {
				t.Errorf("pyStrOr(%v,%v) = %q, want %q", c.v, c.present, got, c.want)
			}
		})
	}
}

// ---- decodeBase64Lenient ----------------------------------------------------------------------

func TestDecodeBase64Lenient(t *testing.T) {
	t.Run("clean base64 decodes", func(t *testing.T) {
		got := decodeBase64Lenient("aGVsbG8=")
		if string(got) != "hello" {
			t.Errorf("got %q, want hello", got)
		}
	})

	t.Run("whitespace and newlines stripped (GitHub-style wrapping)", func(t *testing.T) {
		got := decodeBase64Lenient("aGVs\r\nbG8=\n")
		if string(got) != "hello" {
			t.Errorf("got %q, want hello", got)
		}
	})

	t.Run("invalid base64 after cleaning returns nil", func(t *testing.T) {
		// Non-alphabet chars (!, *) are stripped first, so a fixture must leave an invalid
		// remainder to actually exercise the error path: "!!!a!!!" cleans to "a" (length 1,
		// not a valid base64 length), unlike "not!!!valid***" which cleans to "notvalid" (8
		// clean base64-alphabet letters -> decodes successfully, so it doesn't test this case).
		got := decodeBase64Lenient("!!!a!!!")
		if got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("empty string decodes to empty", func(t *testing.T) {
		got := decodeBase64Lenient("")
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}

// ---- githubActionsSnapshotName ------------------------------------------------------------------
// (the basic valid/basename/non-matching cases already have a dedicated test,
// TestGithubActionsSnapshotName, in tail_spec_ts_helpers_test.go — this adds the untested
// case-insensitivity and degenerate-basename edge cases.)

func TestGithubActionsSnapshotName_EdgeCases(t *testing.T) {
	t.Run("case-insensitive match", func(t *testing.T) {
		dep, platform, arch, ok := githubActionsSnapshotName("PIP-FREEZE-Linux-ARM64.TXT")
		if !ok || dep != "pip-freeze-linux-arm64" || platform != "linux" || arch != "arm64" {
			t.Errorf("got (%q,%q,%q,%v)", dep, platform, arch, ok)
		}
	})

	t.Run("with directory prefix uses basename", func(t *testing.T) {
		dep, _, _, ok := githubActionsSnapshotName("some/dir/pip-freeze-darwin-arm64.txt")
		if !ok || dep != "pip-freeze-darwin-arm64" {
			t.Errorf("got (%q,%v)", dep, ok)
		}
	})

	t.Run("non-matching filename", func(t *testing.T) {
		_, _, _, ok := githubActionsSnapshotName("readme.txt")
		if ok {
			t.Error("want ok=false for non-matching name")
		}
	})

	t.Run("empty/degenerate basenames", func(t *testing.T) {
		for _, name := range []string{"", ".", "/"} {
			_, _, _, ok := githubActionsSnapshotName(name)
			if ok {
				t.Errorf("githubActionsSnapshotName(%q) ok=true, want false", name)
			}
		}
	})
}

// ---- parseGithubActionsSnapshotZip --------------------------------------------------------------

// buildZip constructs an in-memory zip archive from the given (name, content) entries.
// dirNames get a trailing "/" name and no content, mirroring a directory zip entry.
func buildZip(t *testing.T, files map[string]string, dirNames ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, d := range dirNames {
		if _, err := zw.Create(d + "/"); err != nil {
			t.Fatalf("create dir entry: %v", err)
		}
	}
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

func TestParseGithubActionsSnapshotZip(t *testing.T) {
	t.Run("empty zip yields no rows", func(t *testing.T) {
		archive := buildZip(t, map[string]string{})
		rows := parseGithubActionsSnapshotZip(archive, "acme", "repo", "run1", "art1", "rel1", "1.0.0", "sha1", "snap")
		if len(rows) != 0 {
			t.Errorf("want 0 rows, got %+v", rows)
		}
	})

	t.Run("malformed zip archive yields nil", func(t *testing.T) {
		rows := parseGithubActionsSnapshotZip([]byte("not a zip file"), "acme", "repo", "run1", "art1", "rel1", "1.0.0", "sha1", "snap")
		if rows != nil {
			t.Errorf("want nil, got %+v", rows)
		}
	})

	t.Run("directory entries are skipped", func(t *testing.T) {
		archive := buildZip(t, map[string]string{}, "somedir")
		rows := parseGithubActionsSnapshotZip(archive, "acme", "repo", "run1", "art1", "rel1", "1.0.0", "sha1", "snap")
		if len(rows) != 0 {
			t.Errorf("want 0 rows for directory-only zip, got %+v", rows)
		}
	})

	t.Run("non-matching filenames are skipped", func(t *testing.T) {
		archive := buildZip(t, map[string]string{"README.md": "hello", "notes.txt": "world"})
		rows := parseGithubActionsSnapshotZip(archive, "acme", "repo", "run1", "art1", "rel1", "1.0.0", "sha1", "snap")
		if len(rows) != 0 {
			t.Errorf("want 0 rows for non-matching filenames, got %+v", rows)
		}
	})

	t.Run("matching pip-freeze file with no parseable deps is skipped", func(t *testing.T) {
		archive := buildZip(t, map[string]string{"pip-freeze-linux-x86_64.txt": "# just a comment\n"})
		rows := parseGithubActionsSnapshotZip(archive, "acme", "repo", "run1", "art1", "rel1", "1.0.0", "sha1", "snap")
		if len(rows) != 0 {
			t.Errorf("want 0 rows when no deps parse, got %+v", rows)
		}
	})

	t.Run("matching pip-freeze file with real deps emits a row", func(t *testing.T) {
		archive := buildZip(t, map[string]string{"pip-freeze-linux-x86_64.txt": "requests==2.31.0\nurllib3==2.0.7\n"})
		rows := parseGithubActionsSnapshotZip(archive, "acme", "repo", "run42", "art99", "rel7", "1.2.3", "deadbeef", "snapname")
		if len(rows) != 1 {
			t.Fatalf("want 1 row, got %d: %+v", len(rows), rows)
		}
		row := rows[0]
		if row["ArtifactType"] != "dependencies-lockfile" {
			t.Errorf("ArtifactType = %v", row["ArtifactType"])
		}
		if row["Name"] != "pip-freeze-linux-x86_64" {
			t.Errorf("Name = %v", row["Name"])
		}
		if row["Platform"] != "linux" || row["Architecture"] != "x86_64" {
			t.Errorf("Platform/Architecture = %v/%v", row["Platform"], row["Architecture"])
		}
		storageRef, _ := row["StorageRef"].(string)
		wantPrefix := "github-actions://acme/repo/runs/run42/artifacts/art99/"
		if len(storageRef) < len(wantPrefix) || storageRef[:len(wantPrefix)] != wantPrefix {
			t.Errorf("StorageRef = %q, want prefix %q", storageRef, wantPrefix)
		}
		meta, _ := row["MetadataJson"].(string)
		if meta == "" {
			t.Error("MetadataJson should be non-empty")
		}
	})

	t.Run("multiple matching files each emit a row", func(t *testing.T) {
		archive := buildZip(t, map[string]string{
			"pip-freeze-linux-x86_64.txt": "requests==2.31.0\n",
			"pip-freeze-darwin-arm64.txt": "requests==2.31.0\n",
		})
		rows := parseGithubActionsSnapshotZip(archive, "acme", "repo", "run1", "art1", "rel1", "1.0.0", "sha1", "snap")
		if len(rows) != 2 {
			t.Fatalf("want 2 rows, got %d: %+v", len(rows), rows)
		}
	})
}
