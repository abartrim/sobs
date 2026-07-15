//go:build chdb

package goldenreplay

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCaptureGolden regenerates testdata/golden.tar.gz from a live run of the CURRENT
// binary, for every route TestGoldenCorpus would otherwise byte-diff against the frozen
// corpus. It exists for the one legitimate reason a byte-parity fixture is allowed to
// change: an intentional behavior change (bug fix, copy change, markup change) whose new
// output IS the new source of truth, not a regression the frozen corpus should catch.
//
// It is a plain sibling to TestGoldenCorpus, not a flag on it, and only runs when
// SOBS_GOLDEN_CAPTURE=1 is set — never in normal `go test`, dev, or CI runs, matching
// TestGoldenCorpus's CHDB_LIB_PATH-gated skip-by-default pattern.
//
// Run with:
//
//	CHDB_LIB_PATH=/path/to/libchdb.so SOBS_GOLDEN_CAPTURE=1 \
//	  go test -tags chdb -run TestCaptureGolden -v ./goldenreplay/...
//
// Only routes actually replayed (every non-excluded route in every profile — the same set
// TestGoldenCorpus iterates) get their <id>/{status,headers.txt,body.bin} archive entries
// overwritten; every other existing entry is carried through unchanged.
func TestCaptureGolden(t *testing.T) {
	if os.Getenv("SOBS_GOLDEN_CAPTURE") != "1" {
		t.Skip("SOBS_GOLDEN_CAPTURE=1 not set — this test only runs on explicit request, see its doc comment")
	}
	libPath := chdbLibPath(t)
	binary, seeder := buildBinaries(t)

	absTestdata, err := filepath.Abs(testdataDir)
	if err != nil {
		t.Fatal(err)
	}

	goldenPath := filepath.Join(absTestdata, "golden.tar.gz")
	goldenIndex, err := loadTarGzIndex(goldenPath)
	if err != nil {
		t.Fatalf("load golden.tar.gz: %v", err)
	}
	seedsIndex, err := loadTarGzIndex(filepath.Join(absTestdata, "fixtures", "seeds.tar.gz"))
	if err != nil {
		t.Fatalf("load fixtures/seeds.tar.gz: %v", err)
	}
	upstreamDir := filepath.Join(t.TempDir(), "upstream")
	if err := extractTarGz(filepath.Join(absTestdata, "fixtures", "upstream.tar.gz"), upstreamDir); err != nil {
		t.Fatalf("extract fixtures/upstream.tar.gz: %v", err)
	}
	baseFixture := filepath.Join(absTestdata, "fixtures", "base.tar.gz")

	routes, excluded, profileEnv, err := loadManifest(testdataDir)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}

	byProfile := map[string][]route{}
	for _, r := range routes {
		if excluded[r.ID] {
			continue
		}
		byProfile[r.Profile] = append(byProfile[r.Profile], r)
	}

	profiles := make([]string, 0, len(byProfile))
	for p := range byProfile {
		profiles = append(profiles, p)
	}

	captured := map[string][]byte{}
	var mu sync.Mutex
	var captureCount int
	sem := make(chan struct{}, 4) // same bounded concurrency as TestGoldenCorpus — chdb is memory-heavy
	var wg sync.WaitGroup

	portBase := 18900
	for i, profile := range profiles {
		i, profile := i, profile
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			t.Run(profile, func(t *testing.T) {
				n := captureProfile(t, profile, byProfile[profile], portBase+i, binary, seeder, libPath, baseFixture, upstreamDir, profileEnv[profile], seedsIndex, &mu, captured)
				mu.Lock()
				captureCount += n
				mu.Unlock()
			})
		}()
	}
	wg.Wait()

	t.Logf("CAPTURED %d routes into %d archive entries", captureCount, len(captured))

	// Merge freshly captured entries over the existing index — anything not captured
	// (there should be nothing outside `excluded`, which this deliberately leaves alone)
	// is preserved as-is.
	merged := make(map[string][]byte, len(goldenIndex)+len(captured))
	for k, v := range goldenIndex {
		merged[k] = v
	}
	for k, v := range captured {
		merged[k] = v
	}

	if err := writeTarGzIndex(goldenPath, merged); err != nil {
		t.Fatalf("write golden.tar.gz: %v", err)
	}
}

// captureProfile mirrors runProfile's boot/seed/replay sequence exactly (see TestGoldenCorpus's
// runProfile in replay_test.go), but writes each route's live response into `captured` instead
// of comparing it against a frozen fixture.
func captureProfile(t *testing.T, profile string, routes []route, port int, binary, seeder, libPath, baseFixture, upstreamDir string, envOverlay map[string]string, seedsIndex map[string][]byte, mu *sync.Mutex, captured map[string][]byte) int {
	workdir := t.TempDir()
	dataDir := filepath.Join(workdir, "data")
	if err := extractTarGz(baseFixture, dataDir); err != nil {
		t.Fatalf("extract base fixture: %v", err)
	}

	baseEnv := os.Environ()
	for k, v := range pinnedEnv {
		baseEnv = setEnvVar(baseEnv, k, v)
	}
	baseEnv = setEnvVar(baseEnv, "CHDB_LIB_PATH", libPath)

	if deltaJSON, ok := seedsIndex[profile+".json"]; ok {
		deltaPath := filepath.Join(workdir, "delta.json")
		if err := os.WriteFile(deltaPath, deltaJSON, 0o644); err != nil {
			t.Fatalf("write seed delta for profile %q: %v", profile, err)
		}
		cmd := exec.Command(seeder, dataDir, deltaPath)
		cmd.Env = baseEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("seed profile %q: %v\n%s", profile, err, out)
		}
	}

	if filesJSON, ok := seedsIndex[profile+".files.json"]; ok {
		if err := applyExtraFiles(filesJSON, dataDir); err != nil {
			t.Fatalf("apply extra files for profile %q: %v", profile, err)
		}
	}

	env := append([]string{}, baseEnv...)
	set := func(k, v string) { env = setEnvVar(env, k, v) }
	set("SOBS_DATA_DIR", dataDir)
	set("SOBS_PORT", strconv.Itoa(port))
	for k, v := range envOverlay {
		if v == "$UPSTREAM_FIXTURES$" {
			v = upstreamDir
		}
		set(k, v)
	}

	proc, err := bootServer(binary, env, port)
	if err != nil {
		t.Fatalf("boot server for profile %q: %v", profile, err)
	}
	defer stopServer(proc)

	client := &http.Client{
		Timeout:       45 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}
	n := 0
	for _, rt := range routes {
		rt := rt
		t.Run(rt.ID, func(t *testing.T) {
			got, err := replayRoute(client, port, rt)
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			mu.Lock()
			captured[rt.ID+"/status"] = []byte(strconv.Itoa(got.Status))
			captured[rt.ID+"/headers.txt"] = []byte(formatHeaders(got.Headers))
			captured[rt.ID+"/body.bin"] = got.Body
			mu.Unlock()
		})
		n++
	}
	return n
}

// formatHeaders mirrors readGolden's parser in reverse: "key: value" lines joined by "\n",
// no trailing newline, one line per (possibly repeated) header value — see testdata.go.
func formatHeaders(headers [][2]string) string {
	lines := make([]string, len(headers))
	for i, kv := range headers {
		lines[i] = kv[0] + ": " + kv[1]
	}
	return strings.Join(lines, "\n")
}

// writeTarGzIndex serializes index as a gzip'd tar archive at path, one regular-file entry
// per map key (sorted for a reproducible byte layout across runs) — the write-side
// counterpart to loadTarGzIndex, which only ever needed to read.
func writeTarGzIndex(path string, index map[string][]byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)

	keys := make([]string, 0, len(index))
	for k := range index {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	writeErr := func() error {
		for _, k := range keys {
			b := index[k]
			hdr := &tar.Header{
				Name: k,
				Mode: 0o644,
				Size: int64(len(b)),
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("write header for %q: %w", k, err)
			}
			if _, err := tw.Write(b); err != nil {
				return fmt.Errorf("write body for %q: %w", k, err)
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return gz.Close()
	}()

	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(tmp)
		return writeErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, path)
}
