package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// upstreamFixtureKey mirrors determinism.upstream_fixture_key (migration/tools): the canned
// response filename stem = sha256("METHOD url")[:32]. Both sides key on the identical request
// URL string, so the same file backs the Python oracle and the Go server.
func upstreamFixtureKey(method, url string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(method) + " " + url))
	return hex.EncodeToString(sum[:])[:32]
}

// upstreamResponse is a canned upstream HTTP response: a status and a parsed JSON body
// (jsonenc.Object / []any / json.Number, via parseJSONValue), or nil body.
type upstreamResponse struct {
	Status int
	Body   any
}

// upstreamGet fetches an external URL (GitHub / OSV). Under parity it is served from
// SOBS_UPSTREAM_FIXTURES — canned files keyed by request URL, the same set the Python
// determinism httpx shim reads — so neither side touches the network and both build identical
// route responses. A missing fixture resolves to a 404 (a not-found upstream lookup).
func (s *server) upstreamGet(method, url string) (upstreamResponse, error) {
	dir := strings.TrimSpace(os.Getenv("SOBS_UPSTREAM_FIXTURES"))
	if dir == "" {
		return upstreamResponse{}, fmt.Errorf("no upstream backend configured")
	}
	raw, err := os.ReadFile(filepath.Join(dir, upstreamFixtureKey(method, url)+".json"))
	if err != nil {
		return upstreamResponse{Status: 404, Body: jsonenc.NewObject().
			Set("message", "Not Found (no upstream fixture)")}, nil
	}
	parsed, err := parseJSONValue(raw)
	if err != nil {
		return upstreamResponse{}, err
	}
	spec, ok := parsed.(*jsonenc.Object)
	if !ok {
		return upstreamResponse{}, fmt.Errorf("bad upstream fixture %q", url)
	}
	status := 200
	if sv, ok := spec.Get("status"); ok {
		if n, ok := sv.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				status = int(i)
			}
		}
	}
	var body any
	if b, ok := spec.Get("json"); ok {
		body = b
	}
	return upstreamResponse{Status: status, Body: body}, nil
}

// objStrOr mirrors `str(obj.get(key) or "").strip()` for a parsed-JSON object: a falsy value
// (missing/null/""/0/false) yields "".
func objStrOr(o *jsonenc.Object, key string) string {
	v, ok := o.Get(key)
	if !ok || !isTruthyVal(v, ok) {
		return ""
	}
	return strings.TrimSpace(pyStr(v, ok))
}

// objSub returns a nested object value, if present and an object.
func objSub(o *jsonenc.Object, key string) (*jsonenc.Object, bool) {
	v, ok := o.Get(key)
	if !ok {
		return nil, false
	}
	sub, ok := v.(*jsonenc.Object)
	return sub, ok
}

// objTruthy mirrors bool(obj.get(key, default-false)) for a parsed-JSON object.
func objTruthy(o *jsonenc.Object, key string) bool {
	v, ok := o.Get(key)
	return isTruthyVal(v, ok)
}
