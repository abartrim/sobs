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
)

func main() {
	cfg := loadConfig()
	srv := newServer(cfg)

	addr := "127.0.0.1:" + cfg.Port
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

	SecretKey           string
	EncryptionSecret    string
	BuildVersion        string
	BasePath            string
	QueryPageEnabled    bool
	KubernetesEnabled   bool
	FirstRunTourEnabled bool
}

func loadConfig() config {
	return config{
		Parity:              os.Getenv("SOBS_PARITY") == "1",
		DataDir:             envOr("SOBS_DATA_DIR", "./data"),
		Port:                envOr("SOBS_PORT", "8799"),
		StaticDir:           envOr("SOBS_STATIC_DIR", "static"),
		TemplateDir:         envOr("SOBS_TEMPLATE_DIR", "templates"),
		SecretKey:           envOr("SOBS_SECRET_KEY", "sobs-dev-secret-key"),
		EncryptionSecret:    os.Getenv("SOBS_SETTINGS_ENCRYPTION_SECRET"),
		BuildVersion:        envOr("SOBS_BUILD_VERSION", "dev"),
		BasePath:            os.Getenv("SOBS_BASE_PATH"),
		QueryPageEnabled:    os.Getenv("SOBS_QUERY_PAGE_ENABLED") == "1",
		KubernetesEnabled:   os.Getenv("SOBS_KUBERNETES_ENABLED") == "1",
		FirstRunTourEnabled: os.Getenv("SOBS_ENABLE_FIRST_RUN_TOUR") == "1",
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
