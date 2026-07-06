// Command sobs is the Go reimplementation of the SOBS server. During the migration its
// sole measure of correctness is byte-for-byte parity with the Python app, enforced by
// migration/tools/parity_check.py against the golden corpus.
//
// This file is the skeleton: a stdlib-only HTTP server with the parity-mode plumbing
// (fixed clock, security-header middleware, static serving, healthz) and a router that
// handlers register into. Handlers + the template engine + the chdb store are built out
// phase by phase per migration/PHASES.md. Keep dependencies to the standard library plus
// the two justified in ../DEPENDENCIES.md.
package main

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/coverage"
	"strings"
	"syscall"
	"time"
)

func main() {
	// `sobs healthcheck` is a self-contained probe for container HEALTHCHECK directives (the slim
	// runtime image ships no curl/python): GET /health on the loopback and exit 0/non-0.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	// chdb disk-encryption entrypoint (no-op unless SOBS_CHDB_ENCRYPTION_KEY is set): render the
	// ClickHouse config and point the store at it, replacing the Python image's shell entrypoint.
	if err := setupChdbEncryption(); err != nil {
		log.Fatalf("chdb encryption setup: %v", err)
	}

	cfg := loadConfig()
	srv := newServer(cfg)

	addr := resolveBindAddr(cfg.Port)
	// Coverage flush hook (NO-OP unless GOCOVERDIR is set, i.e. a `go build -cover` measurement run):
	// the parity harness stops the server with SIGINT/SIGTERM, and this is a long-running server
	// (not a one-shot `go test` binary), so use runtime/coverage's explicit API — designed exactly
	// for "long-running and/or server programs that do not terminate via os.Exit" — rather than
	// os.Exit's implicit exit-hook flush. Once the counters are durably written to gcd, terminate via
	// a raw SIGKILL to self rather than os.Exit: chdb's cgo threads can be mid-call when the signal
	// arrives, and routing process teardown through the Go runtime's os.Exit->libc exit() path races
	// that native state and aborts (SIGABRT) on darwin/arm64. SIGKILL is handled entirely by the
	// kernel, so it can't collide with in-flight cgo/native cleanup — safe here because nothing after
	// this point needs orderly Go-level shutdown. Production and normal parity runs never set
	// GOCOVERDIR, so behavior there is unchanged.
	if gcd := os.Getenv("GOCOVERDIR"); gcd != "" {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			<-sigCh
			if err := coverage.WriteMetaDir(gcd); err != nil {
				log.Printf("coverage: WriteMetaDir: %v", err)
			}
			if err := coverage.WriteCountersDir(gcd); err != nil {
				log.Printf("coverage: WriteCountersDir: %v", err)
			}
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
		}()
	}
	log.Printf("sobs (go) listening on %s  parity=%v dataDir=%s", addr, cfg.Parity, cfg.DataDir)
	if err := http.ListenAndServe(addr, srv); err != nil {
		log.Fatal(err)
	}
}

// ---- config ----------------------------------------------------------------------

type config struct {
	Parity      bool
	DataDir     string
	Port        string
	StaticDir   string
	TemplateDir string

	SecretKey        string
	EncryptionSecret string
	BuildVersion     string
	BasePath         string
	// query_enabled / kubernetes_enabled are NOT cached here: app.py derives them per request
	// from DB settings (ai.endpoint_url+ai.model / kubernetes.enabled), so see
	// server.queryPageEnabled() / kubernetesEnabled() — enabling them via the Settings UI must
	// take effect without a restart.
	FirstRunTourEnabled bool
}

func loadConfig() config {
	return config{
		Parity:           os.Getenv("SOBS_PARITY") == "1",
		DataDir:          envOr("SOBS_DATA_DIR", "./data"),
		Port:             envOr("SOBS_PORT", envOr("PORT", "44317")),
		StaticDir:        envOr("SOBS_STATIC_DIR", "static"),
		TemplateDir:      envOr("SOBS_TEMPLATE_DIR", "templates"),
		SecretKey:        envOr("SOBS_SECRET_KEY", "sobs-dev-secret-key"),
		EncryptionSecret: readEnvOrFile("SOBS_SETTINGS_ENCRYPTION_KEY", "SOBS_SETTINGS_ENCRYPTION_KEY_FILE"),
		BuildVersion:     envOr("SOBS_BUILD_VERSION", "dev"),
		BasePath:         os.Getenv("SOBS_BASE_PATH"),
		// app.py: _env_flag("SOBS_ENABLE_FIRST_RUN_TOUR", True) — default ON, {1,true,yes,on}.
		FirstRunTourEnabled: envFlag("SOBS_ENABLE_FIRST_RUN_TOUR", true),
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// resolveBindAddr mirrors app.py's __main__ bind resolution: HYPERCORN_BIND / GUNICORN_BIND
// override the full host:port; otherwise SOBS_HOST (default 0.0.0.0, matching app.py's
// f"0.0.0.0:{port}" default — bind all interfaces) plus the resolved port.
func resolveBindAddr(port string) string {
	if b := strings.TrimSpace(envOr("HYPERCORN_BIND", os.Getenv("GUNICORN_BIND"))); b != "" {
		return b
	}
	return envOr("SOBS_HOST", "0.0.0.0") + ":" + port
}

// runHealthcheck probes the local /health endpoint; returns 0 if it answers 200, else 1.
func runHealthcheck() int {
	client := http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + envOr("SOBS_PORT", envOr("PORT", "44317")) + "/health")
	if err != nil {
		log.Printf("healthcheck: %v", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return 0
	}
	log.Printf("healthcheck: status %d", resp.StatusCode)
	return 1
}
