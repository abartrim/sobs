package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// staticMaxAge is Quart's framework default send_file_max_age_default (12h). The default
// static handler emits "Cache-Control: public, max-age=43200".
const staticMaxAge = 43200

// contentTypeByExt mirrors the content types Quart/Werkzeug return for the extensions in
// static/. Set explicitly (not Go's mime table) so the bytes match exactly.
var contentTypeByExt = map[string]string{
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".map":   "application/json",
	".html":  "text/html; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/vnd.microsoft.icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
	".ts":    "video/mp2t",
}

// handleStatic serves files from static/ byte-for-byte, matching Quart's default static
// endpoint for the deterministic headers (Content-Type, Content-Length, Cache-Control).
// The mtime/clock-derived caching headers (Last-Modified, ETag, Expires) are intentionally
// NOT emitted: they are not reproducible across hosts and the parity harness drops them
// for filesystem-static responses (normalize.py _STATIC_CACHE_HEADERS). The asset body is
// the rendered artifact and is byte-identical.
func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/static/")
	// Reject path traversal; serve only within the static dir.
	clean := filepath.Clean("/" + rel)
	full := filepath.Join(s.cfg.StaticDir, strings.TrimPrefix(clean, "/"))

	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if ct, ok := contentTypeByExt[strings.ToLower(filepath.Ext(full))]; ok {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(staticMaxAge))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
