//go:build chdb

// Package goldenreplay is the Go-native, Python-free successor to the migration parity
// harness (migration/tools/parity_check.py). It replays the frozen golden corpus under
// go/testdata/ against the real compiled sobs binary over real HTTP — the same
// subprocess-per-profile, real-transport approach the old harness used (chosen over an
// in-process server construction because newServer() calls log.Fatalf on a chdb-open
// failure, which would kill the whole test binary rather than one subtest; a fresh
// process per profile also sidesteps chdb-go's documented "global state" contention
// under repeated same-process boots — see server.go's newServer comment).
//
// Run with: CHDB_LIB_PATH=/path/to/libchdb.so go test -tags chdb ./goldenreplay/...
package goldenreplay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testdataDir = "../testdata"

// pinnedEnv mirrors migration/tools/parity_env.sh verbatim — the frozen environment both
// the Python oracle and the Go server were replayed under to produce the golden corpus.
// migration/ is deleted post-cutover, so this is the permanent record of that pin.
// scripts/e2e_server.py's PINNED_ENV is a separate hand-kept copy of this same map for the
// Playwright E2E suite — update both together (see that file's own comment).
var pinnedEnv = map[string]string{
	"SOBS_PARITY":                      "1",
	"SOBS_SECRET_KEY":                  "parity-fixed-secret-key",
	"SOBS_SESSION_COOKIE_NAME":         "sobs_session",
	"SOBS_SESSION_COOKIE_SAMESITE":     "Lax",
	"SOBS_BASE_PATH":                   "",
	"SOBS_ENABLE_FIRST_RUN_TOUR":       "0",
	"SOBS_SUMMARY_STATS_CACHE_TTL_SEC": "0",
	"SOBS_FAKE_EPOCH":                  "1704164645.0",
	"SOURCE_MAP_ENABLE":                "0",
	// chdb's embedded ClickHouse engine formats DateTime columns (e.g. hyperdx_sessions.Timestamp)
	// to strings using the PROCESS's system timezone — a chdb-internal behavior entirely outside
	// SOBS_FAKE_EPOCH's app-level clock override (the same class of gotcha as chdb's now() reading
	// real vDSO time). The frozen golden corpus was captured on a host with TZ=America/Phoenix
	// (fixed MST, no DST); replaying on a host/container with a different system TZ (e.g. a UTC
	// CI runner) silently reformats those same underlying values to different strings, byte-diffing
	// as a MISMATCH despite identical underlying data. Pinning TZ here — like SOBS_FAKE_EPOCH pins
	// the app clock — makes the corpus portable to any machine, not just the one that captured it.
	"TZ": "America/Phoenix",
}

var buildOnce sync.Once
var sobsBinary, seederBinary string
var buildErr error

// buildBinaries builds both the sobs server and the seeder helper ONCE for the whole test
// run. Both need CGO/the chdb build tag active, so they're built with `go build -tags chdb`
// even though this harness process itself only needs the tag on this file.
//
// When SOBS_GOCOVER is set, the sobs binary is additionally built with `-cover
// -covermode=atomic -coverpkg=./...`: every per-profile subprocess launch then flushes its
// coverage counters into GOCOVERDIR on exit (see cmd/sobs/main.go's GOCOVERDIR-gated
// SIGTERM/SIGINT hook). -covermode=atomic is required (not just -cover, which defaults to
// -covermode=set): runtime/coverage's explicit write API — needed here since this is a live
// server, not a one-shot binary that exits via testing.Main — only works against atomic
// counters. The build's working directory is pinned to the module root (.., one level up
// from this package) rather than left at this test binary's own default: -coverpkg=./...
// resolves relative to the invoking process's cwd, and building from goldenreplay/ silently
// matches zero packages ("no packages being built depend on matches for pattern ./...") — an
// empty coverpkg set, not a build error, so the binary would "successfully" build with no
// instrumentation at all. This is how the corpus's Go integration coverage is measured;
// normal (non-coverage) test runs are unaffected since the flag is opt-in.
func buildBinaries(t *testing.T) (sobs, seeder string) {
	buildOnce.Do(func() {
		dir := t.TempDir()
		sobsBinary = filepath.Join(dir, "sobs")
		seederBinary = filepath.Join(dir, "seeder")
		sobsArgs := []string{"build", "-o", sobsBinary}
		if os.Getenv("SOBS_GOCOVER") != "" {
			sobsArgs = append(sobsArgs, "-cover", "-covermode=atomic", "-coverpkg=./...")
		}
		sobsArgs = append(sobsArgs, "./cmd/sobs")
		buildCmd := exec.Command("go", sobsArgs...)
		buildCmd.Dir = ".."
		if out, err := buildCmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/sobs: %w\n%s", err, out)
			return
		}
		if out, err := exec.Command("go", "build", "-tags", "chdb", "-o", seederBinary, "./seeder").CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./seeder: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return sobsBinary, seederBinary
}

func chdbLibPath(t *testing.T) string {
	if v := os.Getenv("CHDB_LIB_PATH"); v != "" {
		return v
	}
	t.Skip("CHDB_LIB_PATH not set — see go/CHDB_PIN.md for the pinned native lib to download")
	return ""
}

func TestGoldenCorpus(t *testing.T) {
	libPath := chdbLibPath(t)
	binary, seeder := buildBinaries(t)

	absTestdata, err := filepath.Abs(testdataDir)
	if err != nil {
		t.Fatal(err)
	}

	// golden.tar.gz and fixtures/seeds.tar.gz are only ever read as bytes (never need to
	// exist as real files on disk), so they're decompressed once into in-memory indexes —
	// see archive.go's loadTarGzIndex. fixtures/base.tar.gz and fixtures/upstream.tar.gz DO
	// need to be real directories (chdb opens a directory; the server reads upstream mocks
	// off a directory path), so they stay as archive PATHS, extracted fresh where needed.
	goldenIndex, err := loadTarGzIndex(filepath.Join(absTestdata, "golden.tar.gz"))
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

	var green, red int
	var mu sync.Mutex
	sem := make(chan struct{}, 4) // bounded concurrency — chdb is memory-heavy under parallel opens
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
				g, r := runProfile(t, profile, byProfile[profile], portBase+i, binary, seeder, libPath, baseFixture, upstreamDir, profileEnv[profile], goldenIndex, seedsIndex)
				mu.Lock()
				green += g
				red += r
				mu.Unlock()
			})
		}()
	}
	wg.Wait()

	t.Logf("GOLDEN CORPUS: GREEN %d / RED %d / EXCLUDED %d — total %d", green, red, len(excluded), len(routes))
}

// setEnvVar OVERWRITES rather than appends: a duplicate key later in the list isn't reliably
// authoritative across platforms/libraries (e.g. glibc getenv() returns the FIRST match), so a pin
// like TZ must replace any inherited entry, not just be appended after it.
func setEnvVar(env []string, k, v string) []string {
	prefix := k + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = k + "=" + v
			return env
		}
	}
	return append(env, k+"="+v)
}

func runProfile(t *testing.T, profile string, routes []route, port int, binary, seeder, libPath, baseFixture, upstreamDir string, envOverlay map[string]string, goldenIndex, seedsIndex map[string][]byte) (green, red int) {
	workdir := t.TempDir()
	dataDir := filepath.Join(workdir, "data")
	if err := extractTarGz(baseFixture, dataDir); err != nil {
		t.Fatalf("extract base fixture: %v", err)
	}

	// pinnedEnv (including TZ) must apply identically to BOTH the seeder and the server: chdb
	// interprets/formats DateTime columns using the process's system timezone, so if the seeder
	// (which INSERTs fixture timestamps) and the server (which later reads/formats them) ran under
	// different TZs, the round-trip wouldn't be an identity even though the underlying data matches.
	baseEnv := os.Environ()
	for k, v := range pinnedEnv {
		baseEnv = setEnvVar(baseEnv, k, v)
	}
	baseEnv = setEnvVar(baseEnv, "CHDB_LIB_PATH", libPath)

	if deltaJSON, ok := seedsIndex[profile+".json"]; ok {
		// The seeder subprocess takes a file path, so extract this profile's delta to a
		// temp file first — its own interface is otherwise untouched by the archive move.
		deltaPath := filepath.Join(workdir, "delta.json")
		if err := os.WriteFile(deltaPath, deltaJSON, 0o644); err != nil {
			t.Fatalf("write seed delta for profile %q: %v", profile, err)
		}
		// Run as its OWN subprocess (never in-process) — chdb-go's embedded engine has
		// documented per-process global state that misbehaves under repeated Open/Close
		// cycles within one long-lived process; a fresh process per profile avoids it.
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

	// baseEnv already carries pinnedEnv + CHDB_LIB_PATH (applied identically for the seeder above);
	// just layer the server-specific vars on top.
	env := append([]string{}, baseEnv...)
	set := func(k, v string) { env = setEnvVar(env, k, v) }
	set("SOBS_DATA_DIR", dataDir)
	set("SOBS_PORT", strconv.Itoa(port))
	if gcd := os.Getenv("GOCOVERDIR"); gcd != "" {
		// Every profile's subprocess writes into the SAME GOCOVERDIR — Go's coverage counter
		// files are uniquely named per process instance, so concurrent profile runs (see the
		// bounded-concurrency fan-out in TestGoldenCorpus) accumulate safely rather than
		// clobbering each other.
		set("GOCOVERDIR", gcd)
	}
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
	for _, rt := range routes {
		ok := t.Run(rt.ID, func(t *testing.T) {
			golden, has := readGolden(goldenIndex, rt.ID)
			if !has {
				t.Fatalf("no golden fixture for route %q", rt.ID)
			}
			got, err := replayRoute(client, port, rt)
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			gm, gt := applyMasks(golden, rt.Mask), applyMasks(got, rt.Mask)
			if !equalResponses(gm, gt) {
				t.Errorf("MISMATCH %s\n%s", rt.ID, diffSummary(gm, gt))
			}
		})
		if ok {
			green++
		} else {
			red++
		}
	}
	return green, red
}

func bootServer(binary string, env []string, port int) (*exec.Cmd, error) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		cmd := exec.Command(binary)
		cmd.Dir = repoRoot // templates/ and static/ resolve relative to cwd (SOBS_TEMPLATE_DIR/SOBS_STATIC_DIR default)
		cmd.Env = env
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
			if err == nil {
				resp.Body.Close()
				return cmd, nil
			}
			if cmd.ProcessState != nil {
				break // exited during startup -> retry
			}
			time.Sleep(100 * time.Millisecond)
		}
		stopServer(cmd)
		lastErr = fmt.Errorf("server did not become ready: %s", stderr.String())
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return nil, lastErr
}

func stopServer(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	// cmd.Wait (not cmd.Process.Wait) also drains the goroutines exec started to copy the
	// child's stderr into cmd.Stderr — skipping that would let bootServer's error path above
	// read a stderr buffer that hasn't finished filling yet.
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
	}
}

func replayRoute(client *http.Client, port int, rt route) (response, error) {
	req := rt.Request
	method := req.Method
	if method == "" {
		method = rt.Methods[0]
	}
	path := rt.Path
	if len(req.Query) > 0 {
		q := url.Values{}
		for k, v := range req.Query {
			q.Set(k, pyStr(v))
		}
		path += "?" + q.Encode()
	}
	var body []byte
	headers := map[string]string{}
	for k, v := range req.Headers {
		headers[k] = v
	}
	switch {
	case req.hasJSON():
		body = req.JSON
		headers["Content-Type"] = "application/json"
	case len(req.Form) > 0:
		form := url.Values{}
		for k, v := range req.Form {
			if arr, ok := v.([]any); ok {
				// Repeated form fields (e.g. multi-condition tag rules) arrive from the
				// manifest as a JSON array — must become one form value per element, not
				// pyStr's single stringified "[a b]" rendering of the whole slice.
				for _, e := range arr {
					form.Add(k, pyStr(e))
				}
				continue
			}
			form.Set(k, pyStr(v))
		}
		body = []byte(form.Encode())
		headers["Content-Type"] = "application/x-www-form-urlencoded"
	case req.BodyB64 != "":
		data, err := decodeB64(req.BodyB64)
		if err != nil {
			return response{}, err
		}
		body = data
	}

	// A manifest fixture can declare a Content-Length header that lies about the real body
	// size (e.g. reportsimport's "too_large" case, exercising a pre-body-read size
	// rejection) — net/http's client validates declared vs actual body length byte-for-byte
	// and refuses to send any such request ("http: ContentLength=N with Body length M"), so
	// fall back to writing the request over a raw socket, which — like the original golden
	// capture's client — just sends whatever header value the fixture specifies.
	if v, ok := headerLookup(headers, "Content-Length"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n != int64(len(body)) {
			return rawSocketRequest(port, method, path, headers, body)
		}
	}

	httpReq, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), bytes.NewReader(body))
	if err != nil {
		return response{}, err
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		resp, err := client.Do(httpReq.Clone(context.Background()))
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			continue
		}
		defer resp.Body.Close()
		var body []byte
		if rt.Stream {
			body = readFirstSSEFrame(resp.Body)
		} else {
			body, _ = io.ReadAll(resp.Body)
		}
		var hdrs [][2]string
		for k, vs := range resp.Header {
			for _, v := range vs {
				hdrs = append(hdrs, [2]string{k, v})
			}
		}
		return response{Status: resp.StatusCode, Headers: hdrs, Body: body}, nil
	}
	return response{}, lastErr
}

func headerLookup(headers map[string]string, name string) (string, bool) {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// rawSocketRequest writes an HTTP/1.1 request directly over a TCP socket instead of going
// through net/http's client, so a header value (e.g. a spoofed Content-Length) can be sent
// verbatim regardless of the real body's length — see replayRoute's caller comment.
func rawSocketRequest(port int, method, path string, headers map[string]string, body []byte) (response, error) {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		return response{}, err
	}
	defer conn.Close()

	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", method, path)
	fmt.Fprintf(&b, "Host: 127.0.0.1:%d\r\n", port)
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("Connection: close\r\n\r\n")
	b.Write(body)
	if _, err := conn.Write(b.Bytes()); err != nil {
		return response{}, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return response{}, err
	}

	dummyReq, err := http.NewRequest(method, path, nil)
	if err != nil {
		return response{}, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), dummyReq)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{}, err
	}
	var hdrs [][2]string
	for k, vs := range resp.Header {
		for _, v := range vs {
			hdrs = append(hdrs, [2]string{k, v})
		}
	}
	return response{Status: resp.StatusCode, Headers: hdrs, Body: respBody}, nil
}

func readFirstSSEFrame(r io.Reader) []byte {
	buf := make([]byte, 0, 4096)
	one := make([]byte, 1)
	for !bytes.Contains(buf, []byte("\n\n")) && len(buf) < 4096 {
		n, err := r.Read(one)
		if n == 0 || err != nil {
			break
		}
		buf = append(buf, one[0])
	}
	return buf
}

func decodeB64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// pyStr mirrors Python's str() for the JSON scalar types that appear in query/form values —
// in particular a whole-number float64 (JSON has no separate int type) must render as "2",
// not "2".
func pyStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		if t {
			return "True"
		}
		return "False"
	default:
		return fmt.Sprintf("%v", t)
	}
}

func diffSummary(golden, got response) string {
	var b strings.Builder
	if golden.Status != got.Status {
		fmt.Fprintf(&b, "  status: golden=%d go=%d\n", golden.Status, got.Status)
	}
	if len(golden.Body) != len(got.Body) {
		fmt.Fprintf(&b, "  body len: golden=%d go=%d\n", len(golden.Body), len(got.Body))
	}
	n := len(golden.Body)
	if len(got.Body) < n {
		n = len(got.Body)
	}
	for i := 0; i < n; i++ {
		if golden.Body[i] != got.Body[i] {
			lo := i - 40
			if lo < 0 {
				lo = 0
			}
			hi := i + 40
			gLen, gotLen := len(golden.Body), len(got.Body)
			if hi > gLen {
				hi = gLen
			}
			gotHi := i + 40
			if gotHi > gotLen {
				gotHi = gotLen
			}
			fmt.Fprintf(&b, "  first body diff at byte %d:\n    golden: %q\n    go:     %q\n", i, golden.Body[lo:hi], got.Body[lo:gotHi])
			break
		}
	}
	ng, ngot := normalize(golden), normalize(got)
	gm, gotm := headerMultiset(ng.Headers), headerMultiset(ngot.Headers)
	if !equalMultiset(gm, gotm) {
		fmt.Fprintf(&b, "  golden headers: %q\n  go headers:     %q\n", gm, gotm)
	}
	return b.String()
}

// jsonRawEqual is a tiny helper kept for potential future use decoding request.json bodies
// without re-marshaling (avoids reordering keys the golden route might depend on).
var _ = json.RawMessage{}
