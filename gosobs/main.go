package main

// Port of the app.py __main__ / hypercorn startup block plus the Quart
// before_serving / after_serving lifecycle. Embedded chDB requires a single
// process (no multi-worker fork), matching the Python "forcing worker count
// to 1" behaviour.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// buildMux constructs the ServeMux from the global route table populated by
// each section's init() via registerRoute. Go 1.22+ method-prefixed patterns
// are used; Flask "<id>"/"<path:p>" converters were normalized to "{id}" /
// "{p...}" at registration time (see CONVENTIONS.md).
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	seen := map[string]bool{}
	for _, rt := range registeredRoutes {
		method := strings.ToUpper(strings.TrimSpace(rt.Method))
		if method == "" {
			method = http.MethodGet
		}
		pattern := rt.Pattern
		// PORT-NOTE: Go's ServeMux treats a bare "/" as a subtree match (it would
		// catch every otherwise-unmatched path). Quart's `@app.route("/")` matches
		// only "/", so anchor the root with the exact-match marker "{$}" — unknown
		// paths then 404 instead of silently serving the index.
		if pattern == "/" {
			pattern = "/{$}"
		}
		key := method + " " + pattern
		if seen[key] {
			logger.Warn("duplicate route registration ignored", "route", key)
			continue
		}
		seen[key] = true
		mux.HandleFunc(key, rt.Handler)
		// PORT-NOTE: Quart auto-adds HEAD for GET routes. Go's ServeMux has no
		// equivalent and an explicit "HEAD /" alias conflicts with more-specific
		// GET patterns under method-precedence rules, so HEAD aliasing is omitted.
	}
	return mux
}

// startBackgroundWorkers mirrors the @app.before_serving hooks: start the
// write-queue worker and the enrichment/retention/repo-health loops.
func startBackgroundWorkers() {
	ensureWriteWorker()
	startupEnrichment() // go cveScannerLoop / rawWindowCopyLoop / githubRepoHealthLoop
}

// resolveBind mirrors the Python bind resolution: HYPERCORN_BIND or
// GUNICORN_BIND override; otherwise 0.0.0.0:$PORT (PORT default 44317).
func resolveBind() string {
	port := 44317
	if raw := strings.TrimSpace(os.Getenv("PORT")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			port = n
		}
	}
	if bind := strings.TrimSpace(os.Getenv("HYPERCORN_BIND")); bind != "" {
		return bind
	}
	if bind := strings.TrimSpace(os.Getenv("GUNICORN_BIND")); bind != "" {
		return bind
	}
	return "0.0.0.0:" + strconv.Itoa(port)
}

func warnMultiWorker() {
	raw := strings.TrimSpace(os.Getenv("HYPERCORN_WORKERS"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("GUNICORN_WORKERS"))
	}
	if raw == "" {
		return
	}
	if n, err := strconv.Atoi(raw); err == nil && n != 1 {
		logger.Warn("Embedded chDB requires single-process mode; forcing worker count to 1")
	}
}

func main() {
	// Startup ordering mirrors module import + before_serving in app.py.
	ensureDataDirs()

	if err := initDb(); err != nil {
		logger.Error("database init failed", "error", err)
		os.Exit(1)
	}
	if err := ensureDbSchema(); err != nil {
		logger.Error("schema bootstrap failed", "error", err)
		os.Exit(1)
	}

	startupHooks()           // async HTTP client warmup analogue + telemetry + AI annotations check
	startBackgroundWorkers() // write worker + enrichment loops

	// Graceful shutdown safety net (Python's after_serving + atexit).
	defer shutdownHooks()

	warnMultiWorker()
	bind := resolveBind()

	handler := basePathMiddleware(applySecurityHeaders(buildMux()))
	server := &http.Server{
		Addr:    bind,
		Handler: handler,
		BaseContext: func(net.Listener) context.Context {
			return context.Background()
		},
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("SOBS listening", "bind", bind)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server error", "error", err)
			stop <- syscall.SIGTERM
		}
	}()

	<-stop
	logger.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
