package main

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Batch 7: cmd/sobs/ai_action_execute.go — jnToInt, decodeAiActionToken, sanitizeActionValue,
// buildClientAction, handleApiAiHelperExecute. ai_action_normalize_test.go already covers
// actionMetaForPage/actionMetaForID and a decode/encode round trip; this file fills the
// remaining branches (jnToInt scalar kinds, expired/malformed tokens, sanitize depth/size caps,
// buildClientAction's empty-input guard, and the full HTTP handler incl. its error/ok paths).

// ---- jnToInt ----

func TestJnToInt(t *testing.T) {
	if got := jnToInt(mustJSONNumber(t, "42")); got != 42 {
		t.Errorf("json.Number: got %d, want 42", got)
	}
	if got := jnToInt(3.9); got != 3 {
		t.Errorf("float64: got %d, want 3 (truncated)", got)
	}
	if got := jnToInt("nope"); got != 0 {
		t.Errorf("unsupported type: got %d, want 0", got)
	}
	if got := jnToInt(nil); got != 0 {
		t.Errorf("nil: got %d, want 0", got)
	}
}

func mustJSONNumber(t *testing.T, raw string) any {
	t.Helper()
	v, err := parseJSONValue([]byte(`{"n":` + raw + `}`))
	if err != nil {
		t.Fatal(err)
	}
	obj := v.(*jsonenc.Object)
	n, _ := obj.Get("n")
	return n
}

// ---- decodeAiActionToken ----

func TestDecodeAiActionToken_Malformed(t *testing.T) {
	s := &server{}
	if got := s.decodeAiActionToken(""); got != nil {
		t.Errorf("empty token: got %v, want nil", got)
	}
	if got := s.decodeAiActionToken("no-dot-here"); got != nil {
		t.Errorf("no signature separator: got %v, want nil", got)
	}
	if got := s.decodeAiActionToken("body.badsig"); got != nil {
		t.Errorf("wrong signature: got %v, want nil", got)
	}
}

func TestDecodeAiActionToken_BadBase64(t *testing.T) {
	t.Setenv("SOBS_SECRET_KEY", "test-secret")
	s := &server{}
	body := "not!valid!base64!!"
	sig := sha256Hex(aiActionTokenSecret() + "." + body)
	if got := s.decodeAiActionToken(body + "." + sig); got != nil {
		t.Errorf("invalid base64 body: got %v, want nil", got)
	}
}

func TestDecodeAiActionToken_ExpiredAndValid(t *testing.T) {
	t.Setenv("SOBS_SECRET_KEY", "test-secret")
	s := &server{}
	past := jsonenc.NewObject().Set("action_id", "a").Set("exp", 1) // way in the past
	tok := encodeAiActionToken(past)
	if got := s.decodeAiActionToken(tok); got != nil {
		t.Errorf("expired token should decode to nil, got %v", got)
	}
	future := jsonenc.NewObject().Set("action_id", "a").Set("exp", 9999999999)
	tok2 := encodeAiActionToken(future)
	got := s.decodeAiActionToken(tok2)
	if got == nil {
		t.Fatal("valid non-expired token should decode")
	}
	if v := objStrOr(got, "action_id"); v != "a" {
		t.Errorf("action_id = %q, want a", v)
	}
}

func TestDecodeAiActionToken_NotAnObject(t *testing.T) {
	t.Setenv("SOBS_SECRET_KEY", "test-secret")
	s := &server{}
	// A validly-signed body whose decoded JSON is an array, not an object.
	bodyB64 := strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(`[1,2,3]`)), "=")
	sig := sha256Hex(aiActionTokenSecret() + "." + bodyB64)
	if got := s.decodeAiActionToken(bodyB64 + "." + sig); got != nil {
		t.Errorf("array body should not decode to an object, got %v", got)
	}
}

// ---- sanitizeActionValue ----

func TestSanitizeActionValue_ScalarsAndDepthCap(t *testing.T) {
	if got := sanitizeActionValue(nil, 0); got != nil {
		t.Errorf("nil passthrough: got %v", got)
	}
	if got := sanitizeActionValue(true, 0); got != true {
		t.Errorf("bool passthrough: got %v", got)
	}
	// depth > 3 => nil, regardless of the value's own kind.
	if got := sanitizeActionValue("x", 4); got != nil {
		t.Errorf("depth>3 should return nil, got %v", got)
	}
	// Unsupported/default type (e.g. a struct) -> nil.
	type weird struct{ X int }
	if got := sanitizeActionValue(weird{X: 1}, 0); got != nil {
		t.Errorf("unsupported kind should return nil, got %v", got)
	}
}

func TestSanitizeActionValue_StringTrimAndTruncate(t *testing.T) {
	if got := sanitizeActionValue("  hi  ", 0); got != "hi" {
		t.Errorf("string trim: got %q", got)
	}
	long := strings.Repeat("a", 5000)
	got := sanitizeActionValue(long, 0)
	gs, ok := got.(string)
	if !ok || len(gs) != 4096 {
		t.Fatalf("long string should truncate to 4096 chars, got len=%d", len(gs))
	}
}

func TestSanitizeActionValue_ObjectKeyCapAndBlankKeySkip(t *testing.T) {
	obj := jsonenc.NewObject()
	obj.Set("", "should be skipped")
	for i := 0; i < 60; i++ {
		obj.Set("k"+strconv.Itoa(i), i)
	}
	got := sanitizeActionValue(obj, 0)
	outObj, ok := got.(*jsonenc.Object)
	if !ok {
		t.Fatalf("expected *jsonenc.Object, got %T", got)
	}
	if outObj.Len() > 50 {
		t.Errorf("object should cap at 50 keys, got %d", outObj.Len())
	}
	if _, has := outObj.Get(""); has {
		t.Error("blank key should have been skipped")
	}
}

func TestSanitizeActionValue_ArrayItemCap(t *testing.T) {
	arr := make([]any, 150)
	for i := range arr {
		arr[i] = i
	}
	got := sanitizeActionValue(arr, 0)
	outArr, ok := got.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T", got)
	}
	if len(outArr) != 100 {
		t.Errorf("array should cap at 100 items, got %d", len(outArr))
	}
}

// ---- buildClientAction ----

func TestBuildClientAction_EmptyGuards(t *testing.T) {
	if got := buildClientAction("", jsonenc.NewObject()); got != nil {
		t.Errorf("empty action type should return nil, got %v", got)
	}
	if got := buildClientAction("filter", nil); got != nil {
		t.Errorf("nil payload should return nil, got %v", got)
	}
}

func TestBuildClientAction_SanitizesAndSetsType(t *testing.T) {
	payload := jsonenc.NewObject().Set("sql_where", "  x = 1  ").Set(" bad key ", "val")
	got := buildClientAction("apply_sql_filter", payload)
	if got == nil {
		t.Fatal("expected a client action")
	}
	if v := objStrOr(got, "type"); v != "apply_sql_filter" {
		t.Errorf("type = %q", v)
	}
	if v := objStrOr(got, "sql_where"); v != "x = 1" {
		t.Errorf("sql_where = %q, want trimmed", v)
	}
	if v := objStrOr(got, "bad key"); v != "val" {
		t.Errorf("trimmed key should carry the value through, got %q", v)
	}
}

// ---- handleApiAiHelperExecute ----

func execServerWithManifest(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()
	html := `<button data-ai-action-id="logs.filter.apply_sql" data-ai-action-type="apply_sql_filter"
          data-ai-handler="applySqlFilter" data-ai-label="Apply SQL" data-ai-confirm="false">Apply</button>
<button data-ai-action-id="logs.filter.confirm" data-ai-action-type="apply_form_filters"
          data-ai-handler="applyForm" data-ai-label="Apply filters" data-ai-confirm="true">Apply</button>`
	if err := os.WriteFile(filepath.Join(dir, "logs.html"), []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	return &server{cfg: config{TemplateDir: dir}, db: &storetest.FakeDB{}}
}

func TestHandleApiAiHelperExecute_MissingToken(t *testing.T) {
	s := execServerWithManifest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/ai/helper/execute", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.handleApiAiHelperExecute(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "action_token is required") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestHandleApiAiHelperExecute_InvalidToken(t *testing.T) {
	s := execServerWithManifest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/ai/helper/execute",
		strings.NewReader(`{"action_token":"garbage.sig"}`))
	w := httptest.NewRecorder()
	s.handleApiAiHelperExecute(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Invalid or expired action token") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestHandleApiAiHelperExecute_ActionNotAllowed(t *testing.T) {
	t.Setenv("SOBS_SECRET_KEY", "test-secret")
	s := execServerWithManifest(t)
	payload := jsonenc.NewObject().
		Set("action_id", "nonexistent.action").
		Set("target_page", "/logs").
		Set("exp", 9999999999)
	token := encodeAiActionToken(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/ai/helper/execute",
		strings.NewReader(`{"action_token":"`+token+`"}`))
	w := httptest.NewRecorder()
	s.handleApiAiHelperExecute(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Action is not allowed for this page") {
		t.Errorf("body = %s", w.Body.String())
	}
}

func TestHandleApiAiHelperExecute_SuccessNoConfirmation(t *testing.T) {
	t.Setenv("SOBS_SECRET_KEY", "test-secret")
	s := execServerWithManifest(t)
	payload := jsonenc.NewObject().
		Set("action_id", "logs.filter.apply_sql").
		Set("target_page", "/logs").
		Set("action", jsonenc.NewObject().Set("sql_where", "ServiceName = 'x'")).
		Set("chat_id", "chat-1").
		Set("turn_id", "turn-1").
		Set("exp", 9999999999)
	token := encodeAiActionToken(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/ai/helper/execute",
		strings.NewReader(`{"action_token":"`+token+`"}`))
	w := httptest.NewRecorder()
	s.handleApiAiHelperExecute(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("body = %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "logs.filter.apply_sql") {
		t.Errorf("body should echo action_id: %s", w.Body.String())
	}
}

func TestHandleApiAiHelperExecute_RequiresConfirmation(t *testing.T) {
	t.Setenv("SOBS_SECRET_KEY", "test-secret")
	s := execServerWithManifest(t)
	payload := jsonenc.NewObject().
		Set("action_id", "logs.filter.confirm").
		Set("target_page", "/logs").
		Set("action", jsonenc.NewObject().Set("type", "apply_form_filters")).
		Set("exp", 9999999999)
	token := encodeAiActionToken(payload)

	// Without confirm=true -> 409 Conflict, requires_confirmation true.
	req := httptest.NewRequest(http.MethodPost, "/api/ai/helper/execute",
		strings.NewReader(`{"action_token":"`+token+`"}`))
	w := httptest.NewRecorder()
	s.handleApiAiHelperExecute(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"requires_confirmation":true`) {
		t.Errorf("body = %s", w.Body.String())
	}

	// With confirm=true -> 200 OK.
	req2 := httptest.NewRequest(http.MethodPost, "/api/ai/helper/execute",
		strings.NewReader(`{"action_token":"`+token+`","confirm":true}`))
	w2 := httptest.NewRecorder()
	s.handleApiAiHelperExecute(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("confirmed: want 200, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestHandleApiAiHelperExecute_InvalidActionPayload(t *testing.T) {
	t.Setenv("SOBS_SECRET_KEY", "test-secret")
	s := execServerWithManifest(t)
	// No action_type on the meta AND no action payload with a "type" -> actionType stays "" ->
	// buildClientAction's actionType=="" guard fires -> "Action payload is invalid".
	payload := jsonenc.NewObject().
		Set("action_id", "logs.filter.apply_sql").
		Set("target_page", "/logs").
		Set("exp", 9999999999)
	// action key entirely absent (nil payload) so buildClientAction's payload==nil guard also
	// exercises via the meta's own action_type ("apply_sql_filter") still being non-empty... to hit
	// the *payload-nil* branch specifically, assert the actual behavior instead of guessing shape.
	token := encodeAiActionToken(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/ai/helper/execute",
		strings.NewReader(`{"action_token":"`+token+`"}`))
	w := httptest.NewRecorder()
	s.handleApiAiHelperExecute(w, req)
	// meta.action_type is "apply_sql_filter" (non-empty) but action payload is nil ->
	// buildClientAction(actionType, nil) -> nil -> "Action payload is invalid".
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Action payload is invalid") {
		t.Errorf("body = %s", w.Body.String())
	}
}
