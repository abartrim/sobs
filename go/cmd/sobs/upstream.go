package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
// (jsonenc.Object / []any / json.Number, via parseJSONValue), or nil body. RawContent holds the
// fixture's raw "content" string (used for non-JSON bodies such as the SSE streams the AI helper's
// /chat/completions mock returns), mirroring the determinism shim's `content` branch.
type upstreamResponse struct {
	Status     int
	Body       any
	RawContent string
}

var upstreamHTTPClient = &http.Client{Timeout: 30 * time.Second}

// upstreamRequest issues an external request (GitHub / OSV / webhook / LLM / push). When
// SOBS_UPSTREAM_FIXTURES is set (the parity harness) it is served from canned files keyed by
// request URL — the same set the Python determinism httpx shim reads — so neither side touches the
// network and both build identical route responses (the request body is NOT part of the key,
// mirroring the shim). When the fixtures dir is UNSET (real runtime) it falls back to a real
// http.Client, sending the body + headers. Parity is unaffected: the harness always sets the
// fixtures dir, so the mock path is taken there exactly as before.
func (s *server) upstreamRequest(method, url string, body []byte, headers map[string]string) (upstreamResponse, error) {
	if dir := strings.TrimSpace(os.Getenv("SOBS_UPSTREAM_FIXTURES")); dir != "" {
		return s.upstreamFixture(dir, method, url)
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(strings.ToUpper(method), url, rdr)
	if err != nil {
		return upstreamResponse{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := upstreamHTTPClient.Do(req)
	if err != nil {
		return upstreamResponse{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := upstreamResponse{Status: resp.StatusCode, RawContent: string(raw)}
	if parsed, perr := parseJSONValue(raw); perr == nil {
		out.Body = parsed
	}
	return out, nil
}

// upstreamGet is the no-body wrapper kept for the many existing call sites.
func (s *server) upstreamGet(method, url string) (upstreamResponse, error) {
	return s.upstreamRequest(method, url, nil, nil)
}

// upstreamFixture serves the canned parity response keyed by METHOD+url. A missing fixture
// resolves to a 404.
func (s *server) upstreamFixture(dir, method, url string) (upstreamResponse, error) {
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
	rawContent := ""
	if c, ok := spec.Get("content"); ok {
		rawContent, _ = c.(string)
	}
	return upstreamResponse{Status: status, Body: body, RawContent: rawContent}, nil
}

// dispatchNotificationChannel mirrors app.py _dispatch_notification_channel: send to one
// channel, returning "ok" or an error message. Config decryption is identity on the parity
// fixture's plaintext config. Under parity only the webhook type is seeded; slack/email/
// browser_push are not in the corpus, so those branches are never exercised there.
func (s *server) dispatchNotificationChannel(channelType, configJSON, summary string) string {
	var config *jsonenc.Object
	if parsed, err := parseJSONValue([]byte(strOrBrace(configJSON))); err == nil {
		config, _ = parsed.(*jsonenc.Object)
	}
	if config == nil {
		config = jsonenc.NewObject()
	}
	switch channelType {
	case "webhook":
		return s.dispatchWebhookChannel(config, summary)
	case "slack":
		return s.dispatchSlackChannel(config, summary)
	case "email":
		return s.dispatchEmailChannel(config, summary)
	case "browser_push":
		return s.dispatchBrowserPushChannel(config, summary)
	default:
		return "Unknown channel type: " + channelType
	}
}

// dispatchWebhookChannel mirrors app.py _dispatch_webhook_channel: POST (or configured method)
// to config["url"] with the notification payload; HTTP >= 400 (or a missing url) is an error.
func (s *server) dispatchWebhookChannel(config *jsonenc.Object, summary string) string {
	url := objStrOr(config, "url")
	if url == "" {
		return "Webhook URL is not configured"
	}
	method := "POST"
	if mv := objStrOr(config, "method"); mv != "" {
		method = strings.ToUpper(mv)
	}
	body, _ := json.Marshal(map[string]any{"summary": summary})
	resp, err := s.upstreamRequest(method, url, body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err.Error()
	}
	if resp.Status >= 400 {
		return fmt.Sprintf("Webhook returned HTTP %d", resp.Status)
	}
	return "ok"
}

// strOrBrace returns "{}" for an empty config string (mirrors `str(ConfigJson) or "{}"`).
func strOrBrace(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
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
