package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// normalizeTestServer writes hermetic /logs and /ai templates (annotated like the real ones) and
// returns a server pointed at them, so the manifest-driven normalizer can be exercised end to end.
func normalizeTestServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	logsHTML := `<div>
  <button data-ai-action-id="logs.filter.apply_sql" data-ai-action-type="apply_sql_filter"
          data-ai-handler="applySqlFilter" data-ai-label="Apply logs SQL filter"
          data-ai-risk="low" data-ai-confirm="false" data-ai-action-role="sql-where-input">Apply</button>
  <button data-ai-action-id="logs.filter.form" data-ai-action-type="apply_form_filters"
          data-ai-handler="applyForm" data-ai-label="Apply filters" data-ai-confirm="true"
          data-ai-args='{"filter_fields":["service","status"]}'>Filter</button>
  <a data-ai-action-id="logs.export" data-ai-action-type="export">Export</a>
</div>`
	aiHTML := `<button data-ai-action-id="ai.modal.open" data-ai-action-type="open_modal"
          data-ai-handler="openModal" data-ai-label="Open AI modal" data-ai-confirm="false"
          data-ai-args='{"modal_id":"aiModal"}'>Open</button>`
	if err := os.WriteFile(filepath.Join(dir, "logs.html"), []byte(logsHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ai.html"), []byte(aiHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	return &server{cfg: config{TemplateDir: dir}}
}

func objStr(t *testing.T, o *jsonenc.Object, key string) string {
	t.Helper()
	return objStrOr(o, key)
}

func TestNormalizeUIAction_EmptyActionID(t *testing.T) {
	s := normalizeTestServer(t)
	if got := s.normalizeGenericUIActionToolCall(jsonenc.NewObject(), "/logs"); got != nil {
		t.Fatalf("empty action_id should return nil, got %v", got)
	}
	if got := s.normalizeGenericUIActionToolCall(nil, "/logs"); got != nil {
		t.Fatalf("nil args should return nil, got %v", got)
	}
}

func TestNormalizeUIAction_UnknownAction(t *testing.T) {
	s := normalizeTestServer(t)
	p := s.normalizeGenericUIActionToolCall(jsonenc.NewObject().Set("action_id", "logs.bogus"), "/logs")
	if p == nil {
		t.Fatal("unknown action should still return an (unsupported) proposal, got nil")
	}
	if !objTruthy(p, "unsupported") {
		t.Errorf("unsupported = false, want true")
	}
	if !objBoolDefaultTrue(p, "requires_confirmation") {
		t.Errorf("requires_confirmation = false, want true")
	}
	if got := objStr(t, p, "summary"); got != "Unsupported action: logs.bogus" {
		t.Errorf("summary = %q", got)
	}
	action, _ := objSub(p, "action")
	if got := objStr(t, action, "type"); got != "unsupported" {
		t.Errorf("action.type = %q, want unsupported", got)
	}
	if got := objStr(t, action, "target_page"); got != "/logs" {
		t.Errorf("action.target_page = %q, want /logs", got)
	}
}

func TestNormalizeUIAction_ApplySQLFromAltKey(t *testing.T) {
	s := normalizeTestServer(t)
	args := jsonenc.NewObject().
		Set("action_id", "logs.filter.apply_sql").
		Set("arguments", jsonenc.NewObject().Set("sql", "ServiceName = 'checkout'"))
	p := s.normalizeGenericUIActionToolCall(args, "/logs")
	if objTruthy(p, "unsupported") {
		t.Fatalf("apply_sql should be supported")
	}
	// Same page + meta confirm=false → requires_confirmation false.
	if objBoolDefaultTrue(p, "requires_confirmation") {
		t.Errorf("requires_confirmation = true, want false (same page, confirm=false)")
	}
	if got := objStr(t, p, "summary"); got != "Apply logs SQL filter" {
		t.Errorf("summary = %q (notes empty → label)", got)
	}
	action, _ := objSub(p, "action")
	if got := objStr(t, action, "type"); got != "apply_sql_filter" {
		t.Errorf("action.type = %q", got)
	}
	if got := objStr(t, action, "sql_where"); got != "ServiceName = 'checkout'" {
		t.Errorf("action.sql_where = %q, want extracted from sql alt-key", got)
	}
	if got := objStr(t, action, "sql"); got != "ServiceName = 'checkout'" {
		t.Errorf("action.sql = %q, want preserved original arg", got)
	}
	if got := objStr(t, action, "target_page"); got != "/logs" {
		t.Errorf("action.target_page = %q", got)
	}
}

func TestNormalizeUIAction_ApplySQLFromNotesRegex(t *testing.T) {
	s := normalizeTestServer(t)
	args := jsonenc.NewObject().
		Set("action_id", "logs.filter.apply_sql").
		Set("notes", "please filter With SQL ServiceName = 'api'")
	p := s.normalizeGenericUIActionToolCall(args, "/logs")
	action, _ := objSub(p, "action")
	if got := objStr(t, action, "sql_where"); got != "ServiceName = 'api'" {
		t.Errorf("action.sql_where = %q, want regex-extracted from notes", got)
	}
	// notes non-empty → summary is the notes text.
	if got := objStr(t, p, "summary"); got != "please filter With SQL ServiceName = 'api'" {
		t.Errorf("summary = %q, want the notes", got)
	}
}

func TestNormalizeUIAction_FormFilterAllowlist(t *testing.T) {
	s := normalizeTestServer(t)
	filters := jsonenc.NewObject().Set("service", "checkout").Set("evil", "x")
	args := jsonenc.NewObject().
		Set("action_id", "logs.filter.form").
		Set("arguments", jsonenc.NewObject().Set("filters", filters))
	p := s.normalizeGenericUIActionToolCall(args, "/logs")
	if objTruthy(p, "unsupported") {
		t.Fatalf("partial allowed filters should remain supported")
	}
	action, _ := objSub(p, "action")
	gotFilters, _ := objSub(action, "filters")
	if gotFilters == nil || gotFilters.Len() != 1 {
		t.Fatalf("filters = %v, want only the allowed one", gotFilters)
	}
	if _, ok := gotFilters.Get("evil"); ok {
		t.Errorf("disallowed filter 'evil' leaked through")
	}
	if got := objStr(t, gotFilters, "service"); got != "checkout" {
		t.Errorf("allowed filter service = %q", got)
	}
	// meta confirm=true → requires_confirmation true.
	if !objBoolDefaultTrue(p, "requires_confirmation") {
		t.Errorf("requires_confirmation = false, want true (confirm=true)")
	}
}

func TestNormalizeUIAction_FormFilterAllDropped(t *testing.T) {
	s := normalizeTestServer(t)
	args := jsonenc.NewObject().
		Set("action_id", "logs.filter.form").
		Set("arguments", jsonenc.NewObject().Set("filters", jsonenc.NewObject().Set("evil", "x")))
	p := s.normalizeGenericUIActionToolCall(args, "/logs")
	if !objTruthy(p, "unsupported") {
		t.Fatalf("all filters dropped → unsupported, got supported")
	}
	if objBoolDefaultTrue(p, "requires_confirmation") {
		t.Errorf("requires_confirmation = true, want false for dropped-filters case")
	}
	if got := objStr(t, p, "summary"); got != "Requested filters are not available on this page" {
		t.Errorf("summary = %q", got)
	}
}

func TestNormalizeUIAction_CrossPageConfirmationAndDefaults(t *testing.T) {
	s := normalizeTestServer(t)
	// Action declared on /ai, proposed from /logs → cross-page, confirmation forced even though
	// the action's own confirm=false.
	args := jsonenc.NewObject().Set("action_id", "ai.modal.open").Set("target_page", "/ai")
	p := s.normalizeGenericUIActionToolCall(args, "/logs")
	if objTruthy(p, "unsupported") {
		t.Fatalf("cross-page action should resolve from the target manifest")
	}
	if !objBoolDefaultTrue(p, "requires_confirmation") {
		t.Errorf("requires_confirmation = false, want true (cross-page)")
	}
	action, _ := objSub(p, "action")
	if got := objStr(t, action, "type"); got != "open_modal" {
		t.Errorf("action.type = %q", got)
	}
	// Template default argument modal_id is merged in.
	if got := objStr(t, action, "modal_id"); got != "aiModal" {
		t.Errorf("action.modal_id = %q, want merged template default", got)
	}
	if got := objStr(t, action, "target_page"); got != "/ai" {
		t.Errorf("action.target_page = %q", got)
	}
}

func TestAttachAiActionTokenRoundTrip(t *testing.T) {
	s := normalizeTestServer(t)
	args := jsonenc.NewObject().
		Set("action_id", "logs.filter.apply_sql").
		Set("arguments", jsonenc.NewObject().Set("sql_where", "ServiceName = 'checkout'"))
	p := s.normalizeGenericUIActionToolCall(args, "/logs")
	attachAiActionToken(p, "/logs", "chat-1", "turn-1")
	token := objStr(t, p, "action_token")
	if token == "" {
		t.Fatal("expected a signed action_token for a supported proposal")
	}
	decoded := s.decodeAiActionToken(token)
	if decoded == nil {
		t.Fatal("issued token failed to decode/verify")
	}
	if got := objStr(t, decoded, "action_id"); got != "logs.filter.apply_sql" {
		t.Errorf("decoded action_id = %q", got)
	}
	if got := objStr(t, decoded, "target_page"); got != "/logs" {
		t.Errorf("decoded target_page = %q", got)
	}
	if got := objStr(t, decoded, "chat_id"); got != "chat-1" {
		t.Errorf("decoded chat_id = %q", got)
	}
	if objBoolDefaultTrue(decoded, "requires_confirmation") {
		t.Errorf("decoded requires_confirmation = true, want false")
	}
	da, _ := objSub(decoded, "action")
	if got := objStr(t, da, "sql_where"); got != "ServiceName = 'checkout'" {
		t.Errorf("decoded action.sql_where = %q", got)
	}
}

func TestAttachAiActionTokenSkipsUnsupported(t *testing.T) {
	s := normalizeTestServer(t)
	p := s.normalizeGenericUIActionToolCall(jsonenc.NewObject().Set("action_id", "logs.bogus"), "/logs")
	attachAiActionToken(p, "/logs", "chat-1", "turn-1")
	if _, ok := p.Get("action_token"); ok {
		t.Errorf("unsupported proposal must not receive an action_token")
	}
}

// TestEncodeAiActionTokenByteParity proves the Go encoder is byte-identical to app.py
// _encode_ai_action_token: re-encoding the real parity-fixture token's payload (under the pinned
// SECRET_KEY) must reproduce the exact token string the Python oracle issued.
func TestEncodeAiActionTokenByteParity(t *testing.T) {
	t.Setenv("SOBS_SECRET_KEY", "parity-fixed-secret-key")
	const want = "eyJhY3Rpb24iOnsic3FsIjoiU2VydmljZU5hbWUgPSAnY2hlY2tvdXQnIiwidGFyZ2V0X3BhZ2UiOiIvbG9ncyJ9LCJhY3Rpb25faWQiOiJsb2dzLmZpbHRlci5hcHBseV9zcWwiLCJjaGF0X2lkIjoiY2hhdC1leGVjLTAwMDEiLCJleHAiOjk5OTk5OTk5OTksInJlcXVpcmVzX2NvbmZpcm1hdGlvbiI6ZmFsc2UsInRhcmdldF9wYWdlIjoiL2xvZ3MiLCJ0dXJuX2lkIjoidHVybi1leGVjLTAwMDEifQ.03a293253824c95208659efb5bade6e56fa99bc6d0f8855a78e9475053e08ee5"
	// Decode the body into an Object (key order is irrelevant — the encoder sorts), then re-encode.
	bodyB64 := want[:len(want)-65] // strip ".<64-hex-sig>"
	raw, err := base64.URLEncoding.DecodeString(bodyB64 + strings.Repeat("=", (4-len(bodyB64)%4)%4))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseJSONValue(raw)
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := parsed.(*jsonenc.Object)
	if !ok {
		t.Fatalf("payload not an object: %T", parsed)
	}
	if got := encodeAiActionToken(payload); got != want {
		t.Errorf("encodeAiActionToken mismatch:\n got=%s\nwant=%s", got, want)
	}
}

func TestActionMetaForPageAndID(t *testing.T) {
	s := normalizeTestServer(t)
	if m := s.actionMetaForPage("/logs", "logs.filter.apply_sql"); m == nil {
		t.Errorf("page-scoped lookup failed for a declared action")
	}
	if m := s.actionMetaForPage("/logs", "ai.modal.open"); m != nil {
		t.Errorf("page-scoped lookup should not find a foreign-page action")
	}
	// All-pages fallback finds the /ai action even though we don't know its page.
	if m := s.actionMetaForID("ai.modal.open"); m == nil {
		t.Errorf("all-pages fallback failed for ai.modal.open")
	} else if got := objStrOr(m, "action_type"); got != "open_modal" {
		t.Errorf("fallback meta action_type = %q", got)
	}
	if m := s.actionMetaForID("nope.nope"); m != nil {
		t.Errorf("fallback should be nil for an unknown action")
	}
	if m := s.actionMetaForID("   "); m != nil {
		t.Errorf("blank action_id → nil")
	}
}
