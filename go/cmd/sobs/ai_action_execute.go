package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// aiActionTokenSecret mirrors app.py _ai_action_token_secret (SECRET_KEY).
func aiActionTokenSecret() string {
	if v := strings.TrimSpace(os.Getenv("SOBS_SECRET_KEY")); v != "" {
		return v
	}
	return "sobs-dev-secret-key"
}

// decodeAiActionToken mirrors app.py _decode_ai_action_token: verify the sha256 signature,
// base64url-decode the body, and reject if expired.
func (s *server) decodeAiActionToken(token string) *jsonenc.Object {
	token = strings.TrimSpace(token)
	idx := strings.LastIndex(token, ".")
	if idx < 0 {
		return nil
	}
	bodyB64, sig := token[:idx], token[idx+1:]
	expected := sha256Hex(aiActionTokenSecret() + "." + bodyB64)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return nil
	}
	padded := bodyB64 + strings.Repeat("=", (4-len(bodyB64)%4)%4)
	raw, err := base64.URLEncoding.DecodeString(padded)
	if err != nil {
		return nil
	}
	parsed, err := parseJSONValue(raw)
	if err != nil {
		return nil
	}
	obj, ok := parsed.(*jsonenc.Object)
	if !ok {
		return nil
	}
	if ev, _ := obj.Get("exp"); jnToInt(ev) <= int(nowUTC().Unix()) {
		return nil
	}
	return obj
}

func jnToInt(v any) int {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
	case float64:
		return int(x)
	}
	return 0
}

// actionMetaForID mirrors app.py _action_meta_for_id: the action descriptor from the manifest
// catalog (the embedded /logs action set on the fixture).
func actionMetaForID(actionID string) *jsonenc.Object {
	parsed, err := parseJSONValue(aiHelperActionsManifestJSON)
	if err != nil {
		return nil
	}
	obj, ok := parsed.(*jsonenc.Object)
	if !ok {
		return nil
	}
	av, _ := obj.Get("actions")
	actions, _ := av.([]any)
	for _, a := range actions {
		if ao, ok := a.(*jsonenc.Object); ok && objStrOr(ao, "action_id") == actionID {
			return ao
		}
	}
	return nil
}

// sanitizeActionValue mirrors app.py _build_client_action._sanitize_value.
func sanitizeActionValue(v any, depth int) any {
	if depth > 3 {
		return nil
	}
	switch x := v.(type) {
	case nil, bool, json.Number, float64, int:
		return v
	case string:
		s := strings.TrimSpace(x)
		if len(s) > 4096 {
			return s[:4096]
		}
		return s
	case *jsonenc.Object:
		out := jsonenc.NewObject()
		for _, k := range x.Keys() {
			if out.Len() >= 50 {
				break
			}
			ck := strings.TrimSpace(k)
			if ck == "" {
				continue
			}
			vv, _ := x.Get(k)
			out.Set(ck, sanitizeActionValue(vv, depth+1))
		}
		return out
	case []any:
		out := []any{}
		for _, it := range x {
			if len(out) >= 100 {
				break
			}
			out = append(out, sanitizeActionValue(it, depth+1))
		}
		return out
	default:
		return nil
	}
}

// buildClientAction mirrors app.py _build_client_action: {"type": action_type, **sanitized}.
func buildClientAction(actionType string, payload *jsonenc.Object) *jsonenc.Object {
	if actionType == "" || payload == nil {
		return nil
	}
	out := jsonenc.NewObject().Set("type", actionType)
	for _, k := range payload.Keys() {
		ck := strings.TrimSpace(k)
		if ck == "" {
			continue
		}
		v, _ := payload.Get(k)
		out.Set(ck, sanitizeActionValue(v, 1))
	}
	return out
}

// POST /api/ai/helper/execute — app.py ai_helper_execute_action: decode a signed action token and
// return the sanitized client action to apply.
func (s *server) handleApiAiHelperExecute(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	token := bstr(m, "action_token")
	if token == "" {
		s.writeMaskedJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", "action_token is required"))
		return
	}
	decoded := s.decodeAiActionToken(token)
	if decoded == nil {
		s.writeMaskedJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", "Invalid or expired action token"))
		return
	}
	actionID := objStrOr(decoded, "action_id")
	targetPage := objStrOr(decoded, "target_page")
	if targetPage == "" {
		targetPage = "/logs"
	}
	actionPayload, _ := objSub(decoded, "action")
	chatID := objStrOr(decoded, "chat_id")
	turnID := objStrOr(decoded, "turn_id")
	meta := actionMetaForID(actionID)
	if meta == nil {
		s.writeMaskedJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", "Action is not allowed for this page"))
		return
	}
	if !objTruthy(meta, "implemented") {
		s.writeMaskedJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", "Action is not implemented"))
		return
	}
	actionType := objStrOr(meta, "action_type")
	if actionType == "" && actionPayload != nil {
		actionType = objStrOr(actionPayload, "type")
	}
	clientAction := buildClientAction(strings.ToLower(actionType), actionPayload)
	if clientAction == nil {
		s.writeMaskedJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", "Action payload is invalid"))
		return
	}
	requiresConfirmation := objTruthy(meta, "requires_confirmation")
	if _, has := decoded.Get("requires_confirmation"); has {
		requiresConfirmation = objTruthy(decoded, "requires_confirmation")
	}
	if requiresConfirmation && !truthy(m["confirm"]) {
		s.writeMaskedJSON(w, http.StatusConflict, jsonenc.NewObject().
			Set("ok", false).Set("error", "Confirmation required").Set("requires_confirmation", true))
		return
	}
	s.emitAiHelperLogEvent("tool.executed", chatID, turnID, targetPage, "", "", "off",
		"Executed action: "+actionID, "INFO", map[string]string{
			"gen_ai.tool.name": "propose_ui_action", "sobs.ai.action_id": actionID,
			"sobs.ai.action.status": "executed",
		})
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("action_id", actionID).Set("client_action", clientAction).
		Set("chat_id", chatID).Set("turn_id", turnID))
}
