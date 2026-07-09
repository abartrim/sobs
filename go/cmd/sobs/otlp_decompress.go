package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"strings"
)

// maxDecompressedBodyBytes caps the size of a decompressed OTLP request body at 32 MiB,
// mirroring app.py's _MAX_DECOMPRESSED_BODY_BYTES. It guards against a decompression
// (zip) bomb DoS where a tiny compressed payload expands to an unbounded amount of memory.
const maxDecompressedBodyBytes = 32 * 1024 * 1024

// errDecompressTooLarge is returned when a decompressed body exceeds maxDecompressedBodyBytes.
// It mirrors the ValueError app.py raises from _decompress_with_limit; the deflate path treats
// it as fatal (no raw-deflate fallback), exactly like Python's `except zlib.error` (which does
// NOT catch the ValueError).
var errDecompressTooLarge = errors.New("decompressed body exceeds limit")

// decompressWithLimit decompresses all of raw through the reader newReader builds and enforces
// maxDecompressedBodyBytes. It reads at most cap+1 bytes so a bomb never fully inflates: an output
// of exactly the cap is accepted but cap+1 (or more) is rejected, matching app.py's strict
// `total > _MAX_DECOMPRESSED_BODY_BYTES`.
func decompressWithLimit(raw []byte, newReader func(io.Reader) (io.Reader, error)) ([]byte, error) {
	r, err := newReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if rc, ok := r.(io.Closer); ok {
		defer func() { _ = rc.Close() }()
	}
	out, err := io.ReadAll(io.LimitReader(r, maxDecompressedBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxDecompressedBodyBytes {
		return nil, errDecompressTooLarge
	}
	return out, nil
}

func gzipReader(r io.Reader) (io.Reader, error)     { return gzip.NewReader(r) }
func zlibReader(r io.Reader) (io.Reader, error)     { return zlib.NewReader(r) }
func rawFlateReader(r io.Reader) (io.Reader, error) { return flate.NewReader(r), nil }

// splitContentEncodings mirrors app.py's
//
//	[e.strip().lower() for e in (content_encoding or "").split(",") if e.strip()]
//
// — comma-split, trimmed, lower-cased, with empty tokens dropped.
func splitContentEncodings(contentEncoding string) []string {
	var out []string
	for _, part := range strings.Split(contentEncoding, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// decompressRequestBody decodes raw according to its Content-Encoding header, a faithful port of
// app.py's _decompress_request_body. The OpenTelemetry Collector's otlphttp exporter enables gzip
// by default, so OTLP exports arrive with Content-Encoding: gzip and must be inflated before the
// JSON/protobuf body is parsed.
//
// Per RFC 9110, Content-Encoding may list multiple comma-separated encodings applied in order
// (e.g. "gzip, deflate"); they are undone in reverse (outermost first). Supported encodings are
// gzip and deflate (zlib-wrapped, with a raw-deflate fallback for senders that omit the zlib
// header). Unrecognised encodings pass through so a downstream parse error surfaces a meaningful
// message. A decompressed body exceeding maxDecompressedBodyBytes is rejected (errDecompressTooLarge).
func decompressRequestBody(raw []byte, contentEncoding string) ([]byte, error) {
	encodings := splitContentEncodings(contentEncoding)
	data := raw
	for i := len(encodings) - 1; i >= 0; i-- {
		switch encodings[i] {
		case "gzip":
			d, err := decompressWithLimit(data, gzipReader)
			if err != nil {
				return nil, err
			}
			data = d
		case "deflate":
			// Some senders use raw deflate (no zlib wrapper). Try zlib first, then fall back to
			// raw — but never fall back on an over-limit error (Python's `except zlib.error` does
			// not catch the size ValueError, so it propagates).
			d, err := decompressWithLimit(data, zlibReader)
			if err != nil && !errors.Is(err, errDecompressTooLarge) {
				d, err = decompressWithLimit(data, rawFlateReader)
			}
			if err != nil {
				return nil, err
			}
			data = d
		default:
			if len(data) > maxDecompressedBodyBytes {
				return nil, errDecompressTooLarge
			}
		}
	}
	return data, nil
}
