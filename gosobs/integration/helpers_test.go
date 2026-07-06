package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/ysmood/gson"
)

// ---------------------------------------------------------------------------
// HTTP clients
// ---------------------------------------------------------------------------

// httpClient follows redirects (default behaviour).
var httpClient = &http.Client{Timeout: 15 * time.Second}

// noRedirectClient captures 3xx Location headers (mirrors requests'
// allow_redirects=False).
var noRedirectClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// postJSON posts a JSON body and returns the status code and decoded body.
func postJSON(t *testing.T, path string, payload any) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	resp, err := httpClient.Post(baseURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

// postForm posts form-encoded data without following redirects.
func postForm(t *testing.T, path string, form url.Values) *http.Response {
	t.Helper()
	resp, err := noRedirectClient.PostForm(baseURL+path, form)
	if err != nil {
		t.Fatalf("POST form %s: %v", path, err)
	}
	return resp
}

func getText(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := httpClient.Get(baseURL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// accepted extracts the numeric "accepted" field.
func accepted(body map[string]any) float64 {
	if v, ok := body["accepted"].(float64); ok {
		return v
	}
	return -1
}

// waitForAnyText polls a page until any expected token appears (eventual
// ingestion). Fails the test on timeout.
func waitForAnyText(t *testing.T, path string, expected []string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		status, text := getText(t, path)
		if status != 200 {
			t.Fatalf("GET %s returned %d", path, status)
		}
		last = text
		for _, tok := range expected {
			if strings.Contains(last, tok) {
				return last
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for any of %v on %s (last len=%d)", expected, path, len(last))
	return last
}

// ---------------------------------------------------------------------------
// OTLP payload builders (mirror the Python helpers)
// ---------------------------------------------------------------------------

func tsNs() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

func otlpLogPayload(message, service, level string) map[string]any {
	if level == "" {
		level = "INFO"
	}
	return map[string]any{
		"resourceLogs": []any{map[string]any{
			"resource": map[string]any{
				"attributes": []any{kv("service.name", service)},
			},
			"scopeLogs": []any{map[string]any{
				"logRecords": []any{map[string]any{
					"timeUnixNano": tsNs(),
					"severityText": level,
					"body":         map[string]any{"stringValue": message},
				}},
			}},
		}},
	}
}

func otlpTracePayload(service string, spans []any) map[string]any {
	return map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource": map[string]any{
				"attributes": []any{kv("service.name", service)},
			},
			"scopeSpans": []any{map[string]any{"spans": spans}},
		}},
	}
}

func kv(key, stringValue string) map[string]any {
	return map[string]any{"key": key, "value": map[string]any{"stringValue": stringValue}}
}

// span builds an OTLP span. parentSpanId may be "", statusCode defaults to 1
// (use spanOpts for attributes).
func span(name, traceID, spanID, parentSpanID string, attrs []any) map[string]any {
	start := time.Now().UnixNano()
	s := map[string]any{
		"traceId":           traceID,
		"spanId":            spanID,
		"parentSpanId":      parentSpanID,
		"name":              name,
		"startTimeUnixNano": fmt.Sprintf("%d", start),
		"endTimeUnixNano":   fmt.Sprintf("%d", start+50_000_000),
		"status":            map[string]any{"code": 1},
	}
	if attrs != nil {
		s["attributes"] = attrs
	}
	return s
}

// ---------------------------------------------------------------------------
// Page action wrappers (Playwright-shaped convenience over go-rod)
// ---------------------------------------------------------------------------

// navLoad navigates and waits for the load event, retrying on go-rod's
// intermittent "-32000 Object reference chain is too long" CDP error (a known
// flaky failure of WaitLoad's internal eval). Re-navigating resets the JS
// execution context and clears the transient state.
func (p *page) navLoad(url string) {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if err := p.Navigate(url); err != nil {
			lastErr = err
			time.Sleep(150 * time.Millisecond)
			continue
		}
		if err := p.WaitLoad(); err != nil {
			lastErr = err
			time.Sleep(150 * time.Millisecond)
			continue
		}
		return
	}
	p.t.Fatalf("navLoad %s failed after retries: %v", url, lastErr)
}

// requestIdle waits for network to settle. Best-effort: the same flaky CDP
// error can surface here and is non-fatal (the page is already loaded).
func (p *page) requestIdle() {
	defer func() { _ = recover() }()
	p.MustWaitRequestIdle()()
}

// gotoIdle navigates and waits for load + network idle (≈ Playwright networkidle).
func (p *page) gotoIdle(url string) {
	p.navLoad(url)
	p.requestIdle()
}

func (p *page) evalJSON(js string, args ...any) gson.JSON { return p.MustEval(js, args...) }
func (p *page) evalBool(js string, args ...any) bool      { return p.MustEval(js, args...).Bool() }
func (p *page) evalStr(js string, args ...any) string     { return p.MustEval(js, args...).Str() }
func (p *page) evalInt(js string, args ...any) int        { return p.MustEval(js, args...).Int() }

func (p *page) waitFn(js string, args ...any) { p.MustWait(js, args...) }

func (p *page) has(sel string) bool { return p.MustHas(sel) }
func (p *page) count(sel string) int {
	return len(p.MustElements(sel))
}

// first returns the first matching element, or nil if none.
func (p *page) first(sel string) *rod.Element {
	els := p.MustElements(sel)
	if len(els) == 0 {
		return nil
	}
	return els.First()
}

func (p *page) clickSel(sel string) { p.MustElement(sel).MustClick() }

// waitSelectorGone waits until the selector no longer matches (state=hidden).
func (p *page) waitSelectorGone(sel string) {
	p.MustWait(`(s) => !document.querySelector(s)`, sel)
}

// setViewport mirrors page.set_viewport_size.
func (p *page) setViewport(w, h int) { p.MustSetViewport(w, h, 1, false) }

// short returns the page bound to a shorter deadline, for waits that should
// fail fast (e.g. a modal that may never appear) rather than blocking on the
// 90s page timeout.
func (p *page) short(d time.Duration) *rod.Page { return p.Page.Timeout(d) }

// once guards run-once seeding within a test process.
type once struct {
	sync.Once
}
