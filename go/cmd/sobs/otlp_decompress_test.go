package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"testing"
)

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func zlibBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("zlib write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zlib close: %v", err)
	}
	return buf.Bytes()
}

func rawDeflateBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatalf("flate writer: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

func TestDecompressRequestBody(t *testing.T) {
	payload := []byte(`{"resourceLogs":[{"scopeLogs":[{"logRecords":[{"body":{"stringValue":"boom"}}]}]}]}`)

	cases := []struct {
		name     string
		raw      []byte
		encoding string
		want     []byte
	}{
		{"no encoding passes through", payload, "", payload},
		{"gzip", gzipBytes(t, payload), "gzip", payload},
		{"gzip case+whitespace insensitive", gzipBytes(t, payload), "  GZIP  ", payload},
		{"deflate zlib-wrapped", zlibBytes(t, payload), "deflate", payload},
		{"deflate raw fallback", rawDeflateBytes(t, payload), "deflate", payload},
		{"unknown encoding passes through", payload, "br", payload},
		{"empty tokens dropped", gzipBytes(t, payload), ", ,gzip,", payload},
		// "gzip, deflate" = gzip applied first, then deflate → body is deflate(gzip(payload));
		// decoded in reverse (deflate then gzip). Mirrors app.py's reversed() loop.
		{"multi-encoding reversed order", zlibBytes(t, gzipBytes(t, payload)), "gzip, deflate", payload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decompressRequestBody(tc.raw, tc.encoding)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestDecompressRequestBodyCorruptGzip(t *testing.T) {
	// A non-gzip body labelled gzip must error (app.py: zlib.error propagates → 400), not pass through.
	if _, err := decompressRequestBody([]byte("not gzip at all"), "gzip"); err == nil {
		t.Fatal("expected error for corrupt gzip body, got nil")
	}
}

func TestDecompressRequestBodyCapBoundary(t *testing.T) {
	// app.py rejects strictly when decompressed size > _MAX_DECOMPRESSED_BODY_BYTES, so exactly the
	// cap is accepted and cap+1 is rejected.
	atCap := bytes.Repeat([]byte{0}, maxDecompressedBodyBytes)
	got, err := decompressRequestBody(gzipBytes(t, atCap), "gzip")
	if err != nil {
		t.Fatalf("exactly-cap body should decompress, got error: %v", err)
	}
	if len(got) != maxDecompressedBodyBytes {
		t.Fatalf("got %d bytes want %d", len(got), maxDecompressedBodyBytes)
	}

	overCap := bytes.Repeat([]byte{0}, maxDecompressedBodyBytes+1)
	if _, err := decompressRequestBody(gzipBytes(t, overCap), "gzip"); !errors.Is(err, errDecompressTooLarge) {
		t.Fatalf("over-cap body should be rejected with errDecompressTooLarge, got: %v", err)
	}
}

// TestDecompressRequestBodyManifestBlobs pins the exact Python-generated body_b64 blobs that the
// OTLP Content-Encoding parity fixtures (migration/manifest/routes.yaml) post, proving the Go
// decompressor inter-operates with the bytes app.py's oracle accepts. Regenerate the blobs with
// migration/tools/gen_otlp_gzip_fixtures.py if the payloads change.
func TestDecompressRequestBodyManifestBlobs(t *testing.T) {
	const (
		gzipLogsB64       = "H4sIAAAAAAAC/4VPwQ6CMAz9FdKzIZAgJt69GU0W9WI8jFHJIqy4DQIh/LuboPGgsb30ta99fQNoNNRogVsqDKyD8/DuODQAt1bLrLE4D2/YuwIM6lYKDBWvEBYBtLxspgXj+Ko4zRh4LWEcL6PjGEH1h0xJBUNBOp+xlRUelex2XJHfjFdREqdJmiyjV3glgy1qafsDdtbTNoztmR9klPffPsiIKvD6P6y4e8r+N6Lx3qCx4ZXLEvOnpykfunzzk0MBAAA="
		deflateMetricsB64 = "eNp9j80KwjAQhF9F9iwlhbaCZ6+KF71ID9u4hGCblPwUpeTdTdoqCmIOIbM784UZwZDV3nDakzOSW9iuLuN7GNUI6OKm8Y6W5Y0e8QGWzCA5ZQo7gvUKBmz9HLDRr8R50YC9hBDqED2W6/77p+5TTKiY4L3PvEUxcQV6MXOv6PCopXKL3cmOTkreD6h0iuUbVuRVURUle50EQLvTvmkTg2VlGvwuxLX5X4RNNer5qsMT3CdmXg=="
		invalidGzipB64    = "eyJyZXNvdXJjZUxvZ3MiOltdfQ=="
	)
	mustB64 := func(s string) []byte {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("bad base64: %v", err)
		}
		return b
	}

	got, err := decompressRequestBody(mustB64(gzipLogsB64), "gzip")
	if err != nil {
		t.Fatalf("gzip logs blob: %v", err)
	}
	if !bytes.Contains(got, []byte(`"resourceLogs"`)) || !bytes.Contains(got, []byte(`"request.failed"`)) {
		t.Fatalf("gzip logs blob decoded to unexpected JSON: %q", got)
	}

	got, err = decompressRequestBody(mustB64(deflateMetricsB64), "deflate")
	if err != nil {
		t.Fatalf("deflate metrics blob: %v", err)
	}
	if !bytes.Contains(got, []byte(`"resourceMetrics"`)) || !bytes.Contains(got, []byte(`"cpu.usage"`)) {
		t.Fatalf("deflate metrics blob decoded to unexpected JSON: %q", got)
	}

	if _, err := decompressRequestBody(mustB64(invalidGzipB64), "gzip"); err == nil {
		t.Fatal("invalid-gzip blob should be rejected, got nil error")
	}
}

func TestDecompressRequestBodyDeflateOverCapNoFallback(t *testing.T) {
	// A valid zlib-wrapped deflate body that exceeds the cap must propagate the size error, NOT
	// retry as raw deflate — mirroring app.py's `except zlib.error` (which does not catch the
	// ValueError raised for over-limit bodies).
	overCap := bytes.Repeat([]byte{0}, maxDecompressedBodyBytes+1)
	if _, err := decompressRequestBody(zlibBytes(t, overCap), "deflate"); !errors.Is(err, errDecompressTooLarge) {
		t.Fatalf("over-cap deflate body should be rejected with errDecompressTooLarge, got: %v", err)
	}
}
