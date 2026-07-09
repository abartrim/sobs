package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b15_handlers_crypto_test.go — batch 15 coverage for cmd/sobs/handlers_crypto.go:
//   handleApiNotificationsVapidKeygen (26)  53.8%
//   mcpAPIKeysCreate (57)                   80.0%

func TestHandleApiNotificationsVapidKeygen_Success(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	r := httptest.NewRequest(http.MethodPost, "/api/notifications/vapid-keygen", nil)
	rec := httptest.NewRecorder()
	s.handleApiNotificationsVapidKeygen(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"ok":true`, `"saved_to_db":true`, `"env_override":false`, `"public_key":`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}

	// The public key is a base64url-encoded uncompressed P-256 point: 0x04 || X(32) || Y(32) = 65
	// bytes, which base64url-encodes (no padding) to 87 characters. Extract it out of the JSON.
	const marker = `"public_key":"`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("public_key field not found in %s", body)
	}
	rest := body[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("unterminated public_key value in %s", body)
	}
	pk := rest[:j]
	decoded, err := base64.RawURLEncoding.DecodeString(pk)
	if err != nil {
		t.Fatalf("public_key not valid base64url: %v", err)
	}
	if len(decoded) != 65 || decoded[0] != 0x04 {
		t.Errorf("public_key decoded len=%d first byte=%#x, want len=65 first=0x04", len(decoded), decoded[0])
	}

	// The private key must have been persisted via setAppSetting -> InsertJSONEachRow.
	fdb := s.db.(*storetest.FakeDB)
	if len(fdb.Inserts) != 1 || fdb.Inserts[0].Table != "sobs_app_settings" {
		t.Fatalf("expected one sobs_app_settings insert, got %#v", fdb.Inserts)
	}
	row := fdb.Inserts[0].Rows[0]
	if row["Key"] != "vapid_private_key" {
		t.Errorf("insert Key = %v, want vapid_private_key", row["Key"])
	}
	if v, _ := row["Value"].(string); v == "" {
		t.Error("expected a non-empty persisted private key value")
	}
}

func TestHandleApiNotificationsVapidKeygen_SetAppSettingError(t *testing.T) {
	// InsertErr forces setAppSetting to fail, exercising the third error branch (after keygen +
	// PKCS8 marshal succeed).
	s := &server{db: &storetest.FakeDB{InsertErr: errB15Boom}}
	r := httptest.NewRequest(http.MethodPost, "/api/notifications/vapid-keygen", nil)
	rec := httptest.NewRecorder()
	s.handleApiNotificationsVapidKeygen(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "failed to generate VAPID keys") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// mcpAPIKeysCreate
// ---------------------------------------------------------------------------

// mcpKeysServer builds a server whose sobs_app_settings store starts with the given mcp.api_keys
// JSON array (raw stored form) and records subsequent writes.
func mcpKeysServer(initialKeysJSON string) (*server, *storetest.FakeDB) {
	fdb := &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_app_settings") && len(params) == 1 {
			if key, _ := params[0].(string); key == mcpAPIKeysSetting {
				if initialKeysJSON == "" {
					return &store.Result{}, nil
				}
				return storetest.Result([]string{"Value"}, []any{initialKeysJSON}), nil
			}
		}
		return &store.Result{}, nil
	}}
	return &server{db: fdb}, fdb
}

func TestMcpAPIKeysCreate_DefaultsAndPersists(t *testing.T) {
	s, fdb := mcpKeysServer("")
	r := httptest.NewRequest(http.MethodPost, "/api/mcp/keys", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	s.mcpAPIKeysCreate(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"label":"API Key"`) {
		t.Errorf("expected default label API Key, got %s", body)
	}
	if !strings.Contains(body, `"expires_at":null`) {
		t.Errorf("expected null expires_at when absent, got %s", body)
	}
	if !strings.Contains(body, `"key":"smcp_`) {
		t.Errorf("expected an smcp_-prefixed raw key, got %s", body)
	}
	// The descriptor must have been persisted (mcpAPIKeysCreate reads then writes back).
	found := false
	for _, ins := range fdb.Inserts {
		if ins.Table == "sobs_app_settings" && len(ins.Rows) == 1 && ins.Rows[0]["Key"] == mcpAPIKeysSetting {
			found = true
			if v, _ := ins.Rows[0]["Value"].(string); !strings.Contains(v, "API Key") {
				t.Errorf("persisted keystore missing label: %s", v)
			}
		}
	}
	if !found {
		t.Errorf("expected an mcp.api_keys insert, got %#v", fdb.Inserts)
	}
}

func TestMcpAPIKeysCreate_CustomLabelAndExpiresAt(t *testing.T) {
	s, _ := mcpKeysServer("")
	body := `{"label":"  My Custom Label  ","expires_at":"2027-01-01T00:00:00Z"}`
	r := httptest.NewRequest(http.MethodPost, "/api/mcp/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.mcpAPIKeysCreate(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"label":"My Custom Label"`) {
		t.Errorf("expected trimmed custom label, got %s", out)
	}
	if !strings.Contains(out, `"expires_at":"2027-01-01T00:00:00Z"`) {
		t.Errorf("expected pass-through expires_at, got %s", out)
	}
}

func TestMcpAPIKeysCreate_LabelTruncatedTo128Runes(t *testing.T) {
	s, _ := mcpKeysServer("")
	longLabel := strings.Repeat("é", 200) // multibyte, so rune-slicing (not byte-slicing) matters
	body := `{"label":"` + longLabel + `"}`
	r := httptest.NewRequest(http.MethodPost, "/api/mcp/keys", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.mcpAPIKeysCreate(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// The response is EnsureASCII-encoded, so each é becomes the 6-byte "é" escape.
	wantLabel := strings.Repeat("\\u00e9", 128)
	if !strings.Contains(rec.Body.String(), `"label":"`+wantLabel+`"`) {
		t.Errorf("expected label truncated to 128 runes (ASCII-escaped), got %s", rec.Body.String())
	}
}

func TestMcpAPIKeysCreate_MaxKeysReached(t *testing.T) {
	// Build a keystore already at the cap (20 entries).
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < mcpAPIKeyMax; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"k` + itoa(i) + `","label":"x","key_hash":"h","created_at":"t","expires_at":null}`)
	}
	b.WriteByte(']')

	s, fdb := mcpKeysServer(b.String())
	r := httptest.NewRequest(http.MethodPost, "/api/mcp/keys", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	s.mcpAPIKeysCreate(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Maximum of 20 keys reached.") {
		t.Errorf("body = %s", rec.Body.String())
	}
	// Must not have persisted anything (the cap check precedes any write).
	if len(fdb.Inserts) != 0 {
		t.Errorf("expected no inserts when capped, got %#v", fdb.Inserts)
	}
}
