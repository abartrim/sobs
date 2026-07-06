// Integration tests for the SOBS Go server.
//
// Port of tests/test_integration.py. Each group mirrors a Python test class:
// curl/OTel/Flask/Node examples POST telemetry to a live server and assert it
// becomes visible; the Screenshots and UIQA groups drive a real browser via
// go-rod (the Playwright replacement).
//
// Run:
//
//	cd gosobs/integration && go test -v
//
// The harness builds ../  (the Go server), launches it with cwd = repo root so
// it finds templates/ and static/, then shares one headless browser across all
// tests. Each test gets a fresh page with console/dialog capture, mirroring the
// Python `page` fixture in conftest.py.
package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const serverHost = "127.0.0.1"

var (
	// baseURL is set in run() once a free port is chosen (a fixed port collides
	// with orphaned servers from interrupted runs, silently testing stale code).
	baseURL string
	browser *rod.Browser

	// screenshotsDir holds visual-regression PNGs (mirrors tests/screenshots).
	screenshotsDir string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		fmt.Println("resolve repo root:", err)
		return 1
	}
	serverPkg, err := filepath.Abs("..")
	if err != nil {
		fmt.Println("resolve server pkg:", err)
		return 1
	}
	screenshotsDir, _ = filepath.Abs("screenshots")
	_ = os.MkdirAll(screenshotsDir, 0o755)

	// Build the server binary.
	bin := filepath.Join(os.TempDir(), "sobs-itest-bin")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = serverPkg
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Println("build server:", err)
		return 1
	}

	// Launch the server with a throwaway data dir.
	dataDir, err := os.MkdirTemp("", "sobs-integration-")
	if err != nil {
		fmt.Println("temp data dir:", err)
		return 1
	}
	port, err := freePort()
	if err != nil {
		fmt.Println("pick free port:", err)
		return 1
	}
	baseURL = fmt.Sprintf("http://%s:%d", serverHost, port)

	logPath := filepath.Join(screenshotsDir, "integration-live-server.log")
	logFile, _ := os.Create(logPath)
	defer logFile.Close()

	srv := exec.Command(bin)
	srv.Dir = repoRoot
	srv.Stdout, srv.Stderr = logFile, logFile
	srv.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		"SOBS_DATA_DIR="+dataDir,
		"SOBS_ENABLE_FIRST_RUN_TOUR=0",
		"SOBS_AI_ENDPOINT_URL=http://localhost:9999/v1",
		"SOBS_AI_MODEL=docs-screenshot-model",
	)
	if err := srv.Start(); err != nil {
		fmt.Println("start server:", err)
		return 1
	}
	// Detect an early exit (e.g. failed bind) so we never silently test a
	// stranger process on the port instead of the binary we just built.
	serverDied := make(chan error, 1)
	go func() { serverDied <- srv.Wait() }()
	defer func() {
		_ = srv.Process.Signal(os.Interrupt)
		select {
		case <-serverDied:
		case <-time.After(5 * time.Second):
			_ = srv.Process.Kill()
		}
	}()

	if err := waitForHealth(10*time.Second, serverDied); err != nil {
		fmt.Println(err)
		tailFile(logPath, 120)
		return 1
	}

	// Shared headless browser. The launcher downloads a Chromium build on first
	// run if none is found locally.
	browser = launchBrowser()
	defer func() {
		browserMu.Lock()
		defer browserMu.Unlock()
		browser.MustClose()
	}()

	return m.Run()
}

// launchBrowser starts a fresh headless Chromium and connects to it.
func launchBrowser() *rod.Browser {
	controlURL := launcher.New().Headless(true).MustLaunch()
	return rod.New().ControlURL(controlURL).MustConnect()
}

var (
	browserMu        sync.Mutex
	pagesSinceLaunch int
)

// recycleBrowserIfNeeded restarts the shared browser every few pages. go-rod's
// shared session accumulates remote-object references over a long run, which
// surfaces intermittently as "-32000 Object reference chain is too long" or
// waits that never settle. A fresh browser process resets that state.
func recycleBrowserIfNeeded() {
	browserMu.Lock()
	defer browserMu.Unlock()
	pagesSinceLaunch++
	if pagesSinceLaunch <= 6 {
		return
	}
	pagesSinceLaunch = 0
	func() {
		defer func() { _ = recover() }()
		browser.MustClose()
	}()
	browser = launchBrowser()
}

func waitForHealth(timeout time.Duration, serverDied <-chan error) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		select {
		case err := <-serverDied:
			return fmt.Errorf("server process exited before becoming ready (likely port in use): %v", err)
		default:
		}
		resp, err := client.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("live SOBS server did not start within %s", timeout)
}

// freePort asks the OS for an unused TCP port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", serverHost+":0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func tailFile(path string, lines int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	fmt.Println("--- server log tail ---")
	fmt.Println(string(data))
}

// ---------------------------------------------------------------------------
// Ordered driver: mirrors pytest's sequential class execution against one
// shared live server (examples post first, then screenshot/UIQA seeding).
// ---------------------------------------------------------------------------

func TestIntegration(t *testing.T) {
	t.Run("CurlExamples", testCurlExamples)
	t.Run("PythonOtelExample", testPythonOtelExample)
	t.Run("FlaskExample", testFlaskExample)
	t.Run("NodeJsExample", testNodeJsExample)
	t.Run("DataVisibleInUI", testDataVisibleInUI)
	t.Run("Screenshots", testScreenshots)
	t.Run("UIQA", testUIQA)
}

// ---------------------------------------------------------------------------
// Page wrapper — fresh browser page with console/dialog capture, mirroring the
// conftest.py `page` fixture. Unexpected console/page errors fail the test
// unless they match an allowed substring pattern.
// ---------------------------------------------------------------------------

type page struct {
	*rod.Page
	t            *testing.T
	allow        []string
	mu           sync.Mutex
	dialogAlerts []string
}

// consoleHookScript hooks console.error and uncaught errors into a page-global
// array. This mirrors conftest.py's console-error + pageerror capture without
// subscribing to the CDP Runtime domain (which conflicts with WaitLoad and
// triggers the flaky "Object reference chain is too long" error). It runs on
// every new document, before page scripts, so it catches load-time errors too.
const consoleHookScript = `
window.__sobsConsoleErrors = window.__sobsConsoleErrors || [];
(function () {
    var orig = console.error;
    console.error = function () {
        try { window.__sobsConsoleErrors.push(Array.from(arguments).map(String).join(' ')); } catch (_) {}
        return orig.apply(console, arguments);
    };
})();
window.addEventListener('error', function (e) {
    try { window.__sobsConsoleErrors.push('pageerror: ' + (e && e.message ? e.message : String(e))); } catch (_) {}
});
window.addEventListener('unhandledrejection', function (e) {
    try { window.__sobsConsoleErrors.push('pageerror: ' + String(e && e.reason)); } catch (_) {}
});
`

// newPage opens a page with a generous per-page timeout so a stuck wait fails
// the test instead of hanging forever. allow lists substrings of console/page
// errors that are expected (mirrors @allow_console_errors patterns).
func newPage(t *testing.T, allow ...string) *page {
	t.Helper()
	recycleBrowserIfNeeded()
	browserMu.Lock()
	b := browser
	browserMu.Unlock()
	rp := b.MustPage("")
	// Whole-page-lifetime deadline. Every passing test completes in <15s, so 45s
	// is ample headroom while still failing a genuinely stuck wait in bounded
	// time instead of blocking until the `go test` timeout.
	rp = rp.Timeout(45 * time.Second)
	p := &page{Page: rp, t: t, allow: allow}
	p.MustEvalOnNewDocument(consoleHookScript)

	// Dialog capture/dismiss uses the Page domain only (safe alongside WaitLoad).
	go p.EachEvent(func(e *proto.PageJavascriptDialogOpening) {
		p.mu.Lock()
		p.dialogAlerts = append(p.dialogAlerts, fmt.Sprintf("dialog(%s): %s", e.Type, e.Message))
		p.mu.Unlock()
		_ = proto.PageHandleJavaScriptDialog{Accept: false}.Call(p.Page)
	})()

	return p
}

// collectConsoleErrors reads the page-global console-error array (best-effort;
// the page may already be navigating/closed).
func (p *page) collectConsoleErrors() []string {
	var out []string
	defer func() { _ = recover() }()
	res := p.MustEval(`() => (window.__sobsConsoleErrors || [])`)
	for _, v := range res.Arr() {
		out = append(out, v.Str())
	}
	return out
}

func (p *page) isAllowed(entry string) bool {
	for _, pat := range p.allow {
		if pat != "" && contains(entry, pat) {
			return true
		}
	}
	return false
}

// close checks for unexpected console/page errors (like the conftest fixture)
// and closes the page. Call via defer.
func (p *page) close() {
	// Convert a residual go-rod panic into a failure for THIS test rather than
	// letting it abort the whole test binary (testing does not recover panics).
	if r := recover(); r != nil {
		p.t.Errorf("panic during test: %v", r)
	}
	var unexpected []string
	for _, e := range p.collectConsoleErrors() {
		if !p.isAllowed(e) {
			unexpected = append(unexpected, e)
		}
	}
	if len(unexpected) > 0 {
		p.t.Errorf("unexpected browser console/page errors:\n%v\nallowed patterns: %v", unexpected, p.allow)
	}
	// Best effort; the page may already be gone.
	func() {
		defer func() { _ = recover() }()
		p.MustClose()
	}()
}

func (p *page) dialogs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.dialogAlerts...)
}

// ctx is unused but kept so callers can pass timeouts if needed later.
var _ = context.Background
