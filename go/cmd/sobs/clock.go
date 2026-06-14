package main

import (
	"os"
	"strconv"
	"time"
)

// nowUTC returns the current UTC time, or a parity-injected fixed clock when SOBS_FAKE_EPOCH
// (unix seconds, may be fractional) is set. The determinism harness freezes Python's clock to
// FIXED_EPOCH during capture; injecting the same epoch into the Go server makes wall-clock-
// derived response fields (export timestamps, etc.) byte-reproducible. Unset in production.
func nowUTC() time.Time {
	if v := os.Getenv("SOBS_FAKE_EPOCH"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			sec := int64(f)
			return time.Unix(sec, int64((f-float64(sec))*1e9)).UTC()
		}
	}
	return time.Now().UTC()
}
