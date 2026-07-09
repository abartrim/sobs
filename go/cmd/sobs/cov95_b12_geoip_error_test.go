package main

import "testing"

// cov95_b12_geoip_error_test.go — coverage-gate batch 12 for cmd/sobs/geoip.go's
// (*geoBlobError).Error (0% coverage): a plain, pure error-message method that no existing test
// called directly (decodeGeoBlob's error paths return it, but nothing asserted its .Error() text).

func TestGeoBlobErrorMessage(t *testing.T) {
	err := &geoBlobError{}
	want := "geoip: bad embedded blob"
	if got := err.Error(); got != want {
		t.Fatalf("(*geoBlobError).Error() = %q, want %q", got, want)
	}
	// errBadGeoBlob is the package-level sentinel returned by decodeGeoBlob on a bad magic —
	// confirm it resolves through the standard `error` interface identically.
	if got := errBadGeoBlob.Error(); got != want {
		t.Fatalf("errBadGeoBlob.Error() = %q, want %q", got, want)
	}
}
