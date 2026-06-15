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

// notificationSensitiveConfigKeys mirrors app.py _NOTIFICATION_SENSITIVE_CONFIG_KEYS: these
// channel-config values are Fernet-encrypted at rest and decrypted before dispatch.
var notificationSensitiveConfigKeys = map[string]bool{
	"smtp_password": true, "auth_token": true, "api_key": true,
	"webhook_url": true, "url": true, "auth": true,
}

// decryptNotificationConfig mirrors _decrypt_notification_config: decrypt the sensitive string
// values. decryptSecretValue is pass-through for non-`enc:v1:` (plaintext) values, so this is
// identity on the parity fixture's plaintext config.
func (s *server) decryptNotificationConfig(config *jsonenc.Object) *jsonenc.Object {
	out := jsonenc.NewObject()
	for _, k := range config.Keys() {
		v, _ := config.Get(k)
		if sv, ok := v.(string); ok && notificationSensitiveConfigKeys[k] {
			out.Set(k, s.decryptSecretValue(sv))
		} else {
			out.Set(k, v)
		}
	}
	return out
}

// dispatchNotificationChannel mirrors app.py _dispatch_notification_channel: decrypt the config
// then send the full payload to one channel, returning "ok" or an error message. Under parity
// only the webhook type is seeded; slack/email/browser_push are not in the corpus.
func (s *server) dispatchNotificationChannel(channelType, configJSON string, payload *jsonenc.Object) string {
	var config *jsonenc.Object
	if parsed, err := parseJSONValue([]byte(strOrBrace(configJSON))); err == nil {
		config, _ = parsed.(*jsonenc.Object)
	}
	if config == nil {
		config = jsonenc.NewObject()
	}
	config = s.decryptNotificationConfig(config)
	switch channelType {
	case "webhook":
		return s.dispatchWebhookChannel(config, payload)
	case "slack":
		return s.dispatchSlackChannel(config, payload)
	case "email":
		return s.dispatchEmailChannel(config, payload)
	case "browser_push":
		return s.dispatchBrowserPushChannel(config, payload)
	default:
		return "Unknown channel type: " + channelType
	}
}

// webhookDumpsOpts mirrors json.dumps(payload) defaults: ", "/": " separators, ensure_ascii=True,
// insertion order.
var webhookDumpsOpts = jsonenc.Options{SortKeys: false, EnsureASCII: true, ItemSep: ", ", KeySep: ": "}

// dispatchWebhookChannel mirrors app.py _dispatch_webhook_channel: POST (or configured method)
// to config["url"] with the full notification payload (or a body_template with {{summary}}
// substituted), merging config["headers"]; HTTP >= 400 (or a missing url) is an error.
func (s *server) dispatchWebhookChannel(config *jsonenc.Object, payload *jsonenc.Object) string {
	url := strings.TrimSpace(objStrOr(config, "url"))
	if url == "" {
		return "Webhook URL is not configured"
	}
	method := "POST"
	if mv := strings.TrimSpace(objStrOr(config, "method")); mv != "" {
		method = strings.ToUpper(mv)
	}
	headers := map[string]string{}
	if hv, ok := config.Get("headers"); ok {
		if hobj := asJSONObject(hv); hobj != nil {
			for _, k := range hobj.Keys() {
				vv, _ := hobj.Get(k)
				headers[k] = pyStrAny(vv)
			}
		}
	}
	if _, ok := headers["Content-Type"]; !ok {
		headers["Content-Type"] = "application/json"
	}
	var body []byte
	if bt := strings.TrimSpace(objStrOr(config, "body_template")); bt != "" {
		body = []byte(strings.ReplaceAll(bt, "{{summary}}", objStrOr(payload, "summary")))
	} else {
		body = jsonenc.Encode(payload, webhookDumpsOpts)
	}
	resp, err := s.upstreamRequest(method, url, body, headers)
	if err != nil {
		return err.Error()
	}
	if resp.Status >= 400 {
		return fmt.Sprintf("Webhook returned HTTP %d", resp.Status)
	}
	return "ok"
}

// asJSONObject coerces a config value (a *jsonenc.Object, or a JSON string) into an Object.
func asJSONObject(v any) *jsonenc.Object {
	switch x := v.(type) {
	case *jsonenc.Object:
		return x
	case string:
		if parsed, err := parseJSONValue([]byte(x)); err == nil {
			if o, ok := parsed.(*jsonenc.Object); ok {
				return o
			}
		}
	}
	return nil
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
