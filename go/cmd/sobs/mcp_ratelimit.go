package main

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// MCP rate limiting — a port of mcp.py's module-level sliding-window limiter. Each client IP
// is allowed _MCP_RATE_LIMIT_REQUESTS requests per _MCP_RATE_LIMIT_WINDOW_SEC sliding window;
// exceeding it returns HTTP 429. State is process-global (like Python's _rate_limit_store), so
// it persists across requests for the life of the server.
const (
	mcpRateLimitRequests  = 60 // requests allowed per window
	mcpRateLimitWindowSec = 60 // sliding window size in seconds
)

var (
	mcpRateLimitMu    sync.Mutex
	mcpRateLimitStore = map[string][]time.Time{}
)

// mcpCheckRateLimit reports whether a request from ip is allowed (true) or should be
// rate-limited (false). Mirrors mcp.py _check_rate_limit: drop timestamps outside the window,
// reject when the survivor count has reached the limit, otherwise record and allow.
//
// Parity note: under the determinism harness Python freezes time.monotonic()->0.0, so every
// captured request shares one window; the corpus stays GREEN only because it sends far fewer
// than the limit (6 POST /mcp requests total). A real clock here yields the same verdict for
// any sub-limit request set, so this is parity-safe.
func mcpCheckRateLimit(ip string) bool {
	now := time.Now()
	cutoff := now.Add(-mcpRateLimitWindowSec * time.Second)
	mcpRateLimitMu.Lock()
	defer mcpRateLimitMu.Unlock()
	ts := mcpRateLimitStore[ip]
	kept := ts[:0]
	for _, t := range ts {
		if !t.Before(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= mcpRateLimitRequests {
		mcpRateLimitStore[ip] = kept
		return false
	}
	kept = append(kept, now)
	mcpRateLimitStore[ip] = kept
	return true
}

// mcpClientIP mirrors mcp.py: the first X-Forwarded-For hop, else the (port-stripped) remote
// address, else "unknown".
func mcpClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0]); first != "" {
			return first
		}
	}
	ip := r.RemoteAddr
	if i := strings.LastIndexByte(ip, ':'); i >= 0 {
		ip = ip[:i]
	}
	if ip == "" {
		return "unknown"
	}
	return ip
}

// mcpEnabled reports whether the MCP server is enabled. Mirrors mcp.py _mcp_enabled:
// (setting or "1") == "1" — enabled when the setting is absent, empty, or exactly "1".
func (s *server) mcpEnabled() bool {
	v, _ := s.appSetting("mcp.enabled")
	if v == "" {
		v = "1"
	}
	return v == "1"
}
