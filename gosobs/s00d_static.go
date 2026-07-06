package main

// General static-file serving. The Python app serves everything under
// /static/<path:filename> from the repo's static/ directory (Quart's built-in
// static handler). The Go port previously registered only a handful of specific
// /static/rum.* routes, so shared assets (bootstrap.min.css, icons, JS, …)
// 404'd and the UI rendered unstyled. This adds the catch-all handler.

import (
	"net/http"
	"os"
	"path/filepath"
)

func init() {
	// More specific /static/rum.* routes (registered elsewhere) take precedence
	// over this trailing-wildcard pattern in Go 1.22's ServeMux.
	registerRoute("GET", "/static/{path...}", serveStaticFile)
}

// staticDir resolves the static asset directory robustly whether the binary is
// run from gosobs/, the repo root, or alongside the executable (mirrors
// templatesDir()).
func staticDir() string {
	for _, candidate := range []string{"static", "../static"} {
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			abs, _ := filepath.Abs(candidate)
			return abs
		}
	}
	if exe, err := os.Executable(); err == nil {
		c := filepath.Join(filepath.Dir(exe), "static")
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return "static"
}

// serveStaticFile serves files from the static directory. http.FileServer
// handles path cleaning / traversal protection and content types.
func serveStaticFile(w http.ResponseWriter, r *http.Request) {
	http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir()))).ServeHTTP(w, r)
}
