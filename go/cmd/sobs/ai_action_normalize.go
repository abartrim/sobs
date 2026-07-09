package main

import (
	"encoding/base64"
	"regexp"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// aiActionTokenTTLSeconds mirrors app.py _AI_ACTION_TOKEN_TTL_SECONDS.
const aiActionTokenTTLSeconds = 300

// aiActionTokenOpts mirrors json.dumps(payload, separators=(",", ":"), sort_keys=True,
// ensure_ascii=False) — the canonical signed-token body.
var aiActionTokenOpts = jsonenc.Options{SortKeys: true, EnsureASCII: false, ItemSep: ",", KeySep: ":"}

// aiHelperJSONDumpOpts mirrors json.dumps(obj, ensure_ascii=False): default separators
// (", ", ": "), insertion order, raw UTF-8. Used for the sobs.ai.tool.action log attr.
var aiHelperJSONDumpOpts = jsonenc.Options{SortKeys: false, EnsureASCII: false, ItemSep: ", ", KeySep: ": "}

// aiNoteSQLRE mirrors app.py's `\bwith\s+sql\s+(.+)$` (re.IGNORECASE) note-extraction pattern.
var aiNoteSQLRE = regexp.MustCompile(`(?i)\bwith\s+sql\s+(.+)$`)

// encodeAiActionToken mirrors app.py _encode_ai_action_token: base64url(body).rstrip("=") +
// "." + sha256(secret "." body_b64).
func encodeAiActionToken(payload *jsonenc.Object) string {
	body := jsonenc.Encode(payload, aiActionTokenOpts)
	bodyB64 := strings.TrimRight(base64.URLEncoding.EncodeToString(body), "=")
	sig := sha256Hex(aiActionTokenSecret() + "." + bodyB64)
	return bodyB64 + "." + sig
}

// issueAiActionToken mirrors app.py _issue_ai_action_token.
func issueAiActionToken(actionID, targetPage string, action *jsonenc.Object, requiresConfirmation bool, chatID, turnID string) string {
	now := int(nowUTC().Unix())
	payload := jsonenc.NewObject().
		Set("v", 1).
		Set("iat", now).
		Set("exp", now+aiActionTokenTTLSeconds).
		Set("action_id", actionID).
		Set("target_page", targetPage).
		Set("action", action).
		Set("requires_confirmation", requiresConfirmation).
		Set("chat_id", chatID).
		Set("turn_id", turnID)
	return encodeAiActionToken(payload)
}

// objBoolDefaultTrue mirrors bool(obj.get(key, True)): absent → true, else the value's truthiness.
func objBoolDefaultTrue(o *jsonenc.Object, key string) bool {
	v, ok := o.Get(key)
	if !ok {
		return true
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return truthy(v)
}

// manifestByID indexes a page action manifest by action_id (app.py's
// {item.get("action_id"): item for item in ...}).
func manifestByID(manifest []*jsonenc.Object) map[string]*jsonenc.Object {
	out := make(map[string]*jsonenc.Object, len(manifest))
	for _, a := range manifest {
		out[objStrOr(a, "action_id")] = a
	}
	return out
}

// unsupportedActionProposal mirrors the "unsupported" return shape of
// _normalize_generic_ui_action_tool_call.
func unsupportedActionProposal(actionID, summary string, requiresConfirmation bool, targetPage string) *jsonenc.Object {
	return jsonenc.NewObject().
		Set("tool", "propose_ui_action").
		Set("action_id", actionID).
		Set("summary", summary).
		Set("requires_confirmation", requiresConfirmation).
		Set("unsupported", true).
		Set("action", jsonenc.NewObject().
			Set("type", "unsupported").
			Set("action_id", actionID).
			Set("target_page", targetPage))
}

// normalizeGenericUIActionToolCall mirrors app.py _normalize_generic_ui_action_tool_call: validate
// the action_id against the current-page (then target-page) manifest, compute cross-page
// confirmation, apply the apply_form_filters allowlist / apply_sql_filter extraction, merge
// template-default arguments, and run _build_client_action sanitization. Returns nil only when no
// action_id is supplied (every other path returns a proposal, possibly an unsupported one).
func (s *server) normalizeGenericUIActionToolCall(args *jsonenc.Object, currentPage string) *jsonenc.Object {
	if args == nil {
		args = jsonenc.NewObject()
	}
	actionID := strings.TrimSpace(objStrOr(args, "action_id"))
	if actionID == "" {
		return nil
	}

	templateManifest := manifestByID(s.helperActionManifestForPage(currentPage))
	templateArgsPre, _ := objSub(orEmptyObj(templateManifest[actionID]), "arguments")

	explicitTarget := strings.TrimSpace(objStrOr(args, "target_page"))
	defaultTarget := ""
	if templateArgsPre != nil {
		defaultTarget = strings.TrimSpace(objStrOr(templateArgsPre, "target_page"))
	}
	targetPage := firstNonEmpty(firstNonEmpty(explicitTarget, defaultTarget), strings.TrimSpace(currentPage))
	if targetPage == "" {
		targetPage = currentPage
	}

	actionArguments, _ := objSub(args, "arguments")
	if actionArguments == nil {
		actionArguments = jsonenc.NewObject()
	}
	notes := strings.TrimSpace(objStrOr(args, "notes"))

	// Resolve the action meta from the current-page manifest first so cross-page navigation
	// actions declared on the current page (e.g. summary.nav.ai → /ai) remain valid, then fall
	// back to the target-page manifest.
	actionMeta := templateManifest[actionID]
	if actionMeta == nil {
		actionMeta = manifestByID(s.helperActionManifestForPage(targetPage))[actionID]
	}

	if actionMeta == nil {
		return unsupportedActionProposal(actionID, firstNonEmpty(notes, "Unsupported action: "+actionID), true, targetPage)
	}

	actionType := strings.ToLower(strings.TrimSpace(objStrOr(actionMeta, "action_type")))
	requiresConfirmation := targetPage != currentPage || objBoolDefaultTrue(actionMeta, "requires_confirmation")
	templateArgs, _ := objSub(actionMeta, "arguments")
	if templateArgs == nil {
		templateArgs = jsonenc.NewObject()
	}

	if actionType == "apply_form_filters" {
		requestedFilters, _ := objSub(actionArguments, "filters")
		allowed := map[string]bool{}
		if fv, ok := templateArgs.Get("filter_fields"); ok {
			if list, ok := fv.([]any); ok {
				for _, it := range list {
					if name := strings.TrimSpace(anyToStr(it)); name != "" {
						allowed[name] = true
					}
				}
			}
		}
		if len(allowed) > 0 && requestedFilters != nil && requestedFilters.Len() > 0 {
			filtered := jsonenc.NewObject()
			for _, k := range requestedFilters.Keys() {
				if allowed[strings.TrimSpace(k)] {
					v, _ := requestedFilters.Get(k)
					filtered.Set(k, v)
				}
			}
			if filtered.Len() == 0 {
				return unsupportedActionProposal(actionID, firstNonEmpty(notes, "Requested filters are not available on this page"), false, targetPage)
			}
			actionArguments = cloneObject(actionArguments)
			actionArguments.Set("filters", filtered)
		}
	}

	if actionType == "apply_sql_filter" {
		sqlWhere := strings.TrimSpace(objStrOr(actionArguments, "sql_where"))
		if sqlWhere == "" {
			for _, altKey := range []string{"sql", "where", "filter", "expression", "query"} {
				cv, ok := actionArguments.Get(altKey)
				if !ok {
					continue
				}
				switch c := cv.(type) {
				case string:
					if strings.TrimSpace(c) != "" {
						sqlWhere = strings.TrimSpace(c)
					}
				case *jsonenc.Object:
					nested := strings.TrimSpace(objStrOr(c, "sql_where"))
					if nested == "" {
						nested = strings.TrimSpace(objStrOr(c, "sql"))
					}
					if nested == "" {
						nested = strings.TrimSpace(objStrOr(c, "where"))
					}
					if nested != "" {
						sqlWhere = nested
					}
				}
				if sqlWhere != "" {
					break
				}
			}
		}
		if sqlWhere == "" && notes != "" {
			if m := aiNoteSQLRE.FindStringSubmatch(notes); m != nil {
				sqlWhere = strings.TrimSpace(m[1])
			}
		}
		if sqlWhere != "" {
			actionArguments = cloneObject(actionArguments)
			actionArguments.Set("sql_where", sqlWhere)
		}
	}

	// Build the action payload: target_page first, the (filtered) arguments, then any
	// template-defined default arguments not already present.
	actionPayload := jsonenc.NewObject().Set("target_page", targetPage)
	for _, k := range actionArguments.Keys() {
		v, _ := actionArguments.Get(k)
		actionPayload.Set(k, v)
	}
	for _, k := range templateArgs.Keys() {
		if _, ok := actionPayload.Get(k); !ok {
			v, _ := templateArgs.Get(k)
			actionPayload.Set(k, v)
		}
	}

	clientAction := buildClientAction(actionType, actionPayload)
	if clientAction == nil {
		return unsupportedActionProposal(actionID, firstNonEmpty(notes, "Invalid arguments for action: "+actionID), true, targetPage)
	}

	summary := firstNonEmpty(firstNonEmpty(notes, objStrOr(actionMeta, "label")), actionID)
	return jsonenc.NewObject().
		Set("tool", "propose_ui_action").
		Set("action_id", actionID).
		Set("summary", summary).
		Set("requires_confirmation", requiresConfirmation).
		Set("unsupported", !objTruthy(actionMeta, "implemented")).
		Set("action", clientAction)
}

// orEmptyObj returns o, or a fresh empty object when o is nil — mirrors `(template_action or {})`.
func orEmptyObj(o *jsonenc.Object) *jsonenc.Object {
	if o == nil {
		return jsonenc.NewObject()
	}
	return o
}

// attachAiActionToken mints and attaches a signed action_token to a proposal, mirroring the
// app.py ai_helper tool loop: only for supported actions that carry a non-empty action payload.
func attachAiActionToken(proposal *jsonenc.Object, page, chatID, turnID string) {
	if proposal == nil {
		return
	}
	actionID := strings.TrimSpace(objStrOr(proposal, "action_id"))
	if actionID == "" || objTruthy(proposal, "unsupported") {
		return
	}
	actionPayload, ok := objSub(proposal, "action")
	if !ok || actionPayload == nil || actionPayload.Len() == 0 {
		return
	}
	tp := firstNonEmpty(firstNonEmpty(objStrOr(actionPayload, "target_page"), page), "/logs")
	proposal.Set("action_token", issueAiActionToken(actionID, tp, actionPayload, objBoolDefaultTrue(proposal, "requires_confirmation"), chatID, turnID))
}
