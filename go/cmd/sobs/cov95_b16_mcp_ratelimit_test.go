package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

// cov95_b16_mcp_ratelimit_test.go — batch 16 targeted coverage for cmd/sobs/mcp_ratelimit.go:
// mcpCheckRateLimit's window-eviction + limit-exceeded branches, and mcpClientIP's
// X-Forwarded-For / RemoteAddr / unknown branches. mcpCheckRateLimit mutates the package-level
// mcpRateLimitStore singleton, so tests use distinctive IP keys no other test/handler would share.

func TestMcpCheckRateLimitAllowsUnderLimitAndBlocksAtLimit(t *testing.T) {
	const ip = "cov95b16-ratelimit-ip-1"
	for i := 0; i < mcpRateLimitRequests; i++ {
		if !mcpCheckRateLimit(ip) {
			t.Fatalf("request %d should be allowed (under limit)", i)
		}
	}
	// The (mcpRateLimitRequests+1)th request in the same window must be rejected.
	if mcpCheckRateLimit(ip) {
		t.Fatal("request beyond the limit should be rejected")
	}
}

func TestMcpCheckRateLimitEvictsOldTimestamps(t *testing.T) {
	const ip = "cov95b16-ratelimit-ip-2"
	mcpRateLimitMu.Lock()
	// Seed the store with timestamps already outside the sliding window (must be evicted, freeing
	// capacity rather than counting toward the limit).
	old := time.Now().Add(-2 * mcpRateLimitWindowSec * time.Second)
	ts := make([]time.Time, mcpRateLimitRequests)
	for i := range ts {
		ts[i] = old
	}
	mcpRateLimitStore[ip] = ts
	mcpRateLimitMu.Unlock()

	if !mcpCheckRateLimit(ip) {
		t.Fatal("expired timestamps should be evicted, freeing capacity for a new request")
	}
}

func TestMcpClientIPVariants(t *testing.T) {
	t.Run("X-Forwarded-For first hop wins", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.1")
		r.RemoteAddr = "127.0.0.1:9999"
		if got := mcpClientIP(r); got != "203.0.113.5" {
			t.Errorf("got %q, want 203.0.113.5", got)
		}
	})

	t.Run("blank X-Forwarded-For falls back to RemoteAddr", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.Header.Set("X-Forwarded-For", "   ")
		r.RemoteAddr = "192.168.1.10:5555"
		if got := mcpClientIP(r); got != "192.168.1.10" {
			t.Errorf("got %q, want 192.168.1.10 (port stripped)", got)
		}
	})

	t.Run("no forwarded header, RemoteAddr without port", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.RemoteAddr = "192.168.1.20"
		// LastIndexByte finds no ':' -> i==-1, so ip is left untouched ("192.168.1.20"); only a
		// truly empty RemoteAddr should hit "unknown".
		if got := mcpClientIP(r); got != "192.168.1.20" {
			t.Errorf("got %q, want the untouched RemoteAddr string", got)
		}
	})

	t.Run("empty RemoteAddr and no forwarded header yields unknown", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/mcp", nil)
		r.RemoteAddr = ""
		if got := mcpClientIP(r); got != "unknown" {
			t.Errorf("got %q, want unknown", got)
		}
	})
}
