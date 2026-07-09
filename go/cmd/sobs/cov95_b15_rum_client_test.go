package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// cov95_b15_rum_client_test.go — batch 15 coverage for cmd/sobs/rum_client.go:
//   eventField (157)      80.0%
//   requestOrigin (169)   80.0%
//   rumJTI (179)          75.0%
//
// rum_client_test.go (an earlier batch) already covers verifyRumClientAuth/tokenEncode/Decode
// end-to-end; this file targets the smaller field-access helpers directly.

func TestEventField(t *testing.T) {
	// *jsonenc.Object form (decodeOrdered shape).
	obj := jsonenc.NewObject().Set("appName", "shop").Set("nested", jsonenc.NewObject())
	if got := eventField(obj, "appName"); got != "shop" {
		t.Errorf("eventField(*Object, appName) = %v, want shop", got)
	}
	if got := eventField(obj, "missing"); got != nil {
		t.Errorf("eventField(*Object, missing) = %v, want nil", got)
	}

	// map[string]any form.
	m := map[string]any{"clientAuthToken": "tok-123"}
	if got := eventField(m, "clientAuthToken"); got != "tok-123" {
		t.Errorf("eventField(map, clientAuthToken) = %v, want tok-123", got)
	}
	if got := eventField(m, "missing"); got != nil {
		t.Errorf("eventField(map, missing) = %v, want nil", got)
	}

	// Neither shape (e.g. a plain string or nil) -> nil.
	if got := eventField("not-an-event", "x"); got != nil {
		t.Errorf("eventField(string, x) = %v, want nil", got)
	}
	if got := eventField(nil, "x"); got != nil {
		t.Errorf("eventField(nil, x) = %v, want nil", got)
	}
}

func TestRequestOrigin(t *testing.T) {
	cases := []struct {
		name    string
		origin  string
		referer string
		want    string
	}{
		{"Origin header wins", "https://Shop.Example.com", "https://other.example.com/page", "https://shop.example.com"},
		{"falls back to Referer origin when Origin absent", "", "https://Ref.Example.com/some/path?x=1", "https://ref.example.com"},
		{"both absent -> empty", "", "", ""},
		{"invalid Referer (no scheme/host) -> empty", "", "not-a-url", ""},
		{"blank Origin header falls back to Referer", "", "http://r.example.com/", "http://r.example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/rum", nil)
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if c.referer != "" {
				r.Header.Set("Referer", c.referer)
			}
			if got := requestOrigin(r); got != c.want {
				t.Errorf("requestOrigin() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRumJTI(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		id := rumJTI()
		if len(id) != 32 { // 16 random bytes hex-encoded
			t.Fatalf("rumJTI() len = %d, want 32: %q", len(id), id)
		}
		for _, c := range id {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				t.Fatalf("rumJTI() = %q contains a non-hex character %q", id, c)
			}
		}
		if seen[id] {
			t.Fatalf("rumJTI() produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}
