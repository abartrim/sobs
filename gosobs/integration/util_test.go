package integration

import (
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ysmood/gson"
)

func replaceAll(s, old, new string) string { return strings.ReplaceAll(s, old, new) }

// urlValues builds url.Values from alternating key/value pairs.
func urlValues(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func envInt(name string, def int) int {
	if raw := os.Getenv(name); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			return n
		}
	}
	return def
}

func mustStatusIn(t *testing.T, got int, allowed ...int) {
	t.Helper()
	for _, a := range allowed {
		if got == a {
			return
		}
	}
	t.Fatalf("status = %d, want one of %v", got, allowed)
}

func mustDisplay(t *testing.T, obj gson.JSON, key, want string) {
	t.Helper()
	if got := obj.Get(key).Str(); got != want {
		t.Errorf("%s display = %q, want %q", key, got, want)
	}
}

func mustNotContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("did not expect %q in visible text", needle)
	}
}

// selectOption sets a <select> value by value attribute and fires change
// (go-rod's MustSelect matches by visible text, which is brittle here).
func (p *page) selectOption(sel, value string) {
	p.MustEval(`(args) => {
        const el = document.querySelector(args.sel);
        if (!el) return;
        el.value = args.value;
        el.dispatchEvent(new Event('change', { bubbles: true }));
    }`, map[string]any{"sel": sel, "value": value})
}

// waitDCL waits for the load event (≈ wait_for_load_state domcontentloaded).
// Retries on go-rod's intermittent "-32000 Object reference chain is too long"
// CDP error; best-effort after a click-triggered navigation (the page is
// already loading, so a persistent wait error is not worth failing the test).
func (p *page) waitDCL() {
	for i := 0; i < 3; i++ {
		if err := p.WaitLoad(); err == nil {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}
