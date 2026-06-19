package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sort"
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

// actionMetaForPage mirrors app.py _action_meta_for_page: the descriptor for action_id from the
// given page's action manifest, or nil when the page does not declare it.
func (s *server) actionMetaForPage(page, actionID string) *jsonenc.Object {
	for _, a := range s.helperActionManifestForPage(page) {
		if objStrOr(a, "action_id") == actionID {
			return a
		}
	}
	return nil
}

// actionMetaForID mirrors app.py _action_meta_for_id: the first matching descriptor scanning every
// declared page template in sorted page order (the all-pages fallback).
func (s *server) actionMetaForID(actionID string) *jsonenc.Object {
	wanted := strings.TrimSpace(actionID)
	if wanted == "" {
		return nil
	}
	pages := make([]string, 0, len(aiActionPageTemplates))
	for p := range aiActionPageTemplates {
		pages = append(pages, p)
	}
	sort.Strings(pages)
	for _, p := range pages {
		if meta := s.actionMetaForPage(p, wanted); meta != nil {
			return meta
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
		// app.py _build_client_action calls _sanitize_value(value) with the default depth=0;
		// the guard is `depth > max_depth` (max_depth=3), so depths 0..3 are kept. Starting at
		// depth 0 (not 1) keeps one extra nesting level, matching Python exactly.
		out.Set(ck, sanitizeActionValue(v, 0))
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
	// Mirror app.py ai_helper_execute_action: page-scoped descriptor first, then the all-pages
	// fallback (so cross-page proposals still resolve).
	meta := s.actionMetaForPage(targetPage, actionID)
	if meta == nil {
		meta = s.actionMetaForID(actionID)
	}
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
			"sobs.ai.tool.action":   string(jsonenc.Encode(clientAction, aiHelperJSONDumpOpts)),
			"sobs.ai.action.status": "executed",
		})
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("action_id", actionID).Set("client_action", clientAction).
		Set("chat_id", chatID).Set("turn_id", turnID))
}
