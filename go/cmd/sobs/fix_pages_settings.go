package main

import (
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// ---------------------------------------------------------------------------
// Notifications page loaders (app.py _load_notification_channels /
// _load_notification_rules / _load_notification_log / _get_vapid_public_key).
// On the empty parity fixture every SELECT returns nothing, so each loader
// yields an empty list and the rendered page is byte-identical to the prior
// hardcoded-empty stub.
// ---------------------------------------------------------------------------

// loadNotificationChannels mirrors app.py _load_notification_channels: active channels ordered by
// Name, with ConfigJson decoded into a native object (sensitive fields decrypted via
// decryptNotificationConfig — pass-through for the plaintext fixture).
func (s *server) loadNotificationChannels() []any {
	out := []any{}
	res, err := s.db.Execute(
		"SELECT Id, Name, ChannelType, ConfigJson, Enabled " +
			"FROM sobs_notification_channels FINAL WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		var cfg *jsonenc.Object
		if parsed, perr := parseJSONValue([]byte(strOrBrace(cStr(m, "ConfigJson")))); perr == nil {
			cfg, _ = parsed.(*jsonenc.Object)
		}
		if cfg == nil {
			cfg = jsonenc.NewObject()
		}
		out = append(out, map[string]any{
			"id":           cStr(m, "Id"),
			"name":         cStr(m, "Name"),
			"channel_type": cStr(m, "ChannelType"),
			"config":       s.decryptNotificationConfig(cfg),
			"enabled":      cInt(m, "Enabled") != 0,
		})
	}
	return out
}

// loadNotificationRules mirrors app.py _load_notification_rules.
func (s *server) loadNotificationRules() []any {
	out := []any{}
	res, err := s.db.Execute(
		"SELECT Id, Name, Enabled, LogicOperator, ConditionsJson, ChannelIds, " +
			"Severity, CooldownSeconds, LastFiredAt " +
			"FROM sobs_notification_rules FINAL WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		channelIDs := []any{}
		for _, c := range strings.Split(cStr(m, "ChannelIds"), ",") {
			if c = strings.TrimSpace(c); c != "" {
				channelIDs = append(channelIDs, c)
			}
		}
		out = append(out, map[string]any{
			"id":               cStr(m, "Id"),
			"name":             cStr(m, "Name"),
			"enabled":          cInt(m, "Enabled") != 0,
			"logic_operator":   orDefault(cStr(m, "LogicOperator"), "any"),
			"conditions":       parseNotificationConditionsJSON(cStr(m, "ConditionsJson")),
			"channel_ids":      channelIDs,
			"severity":         orDefault(cStr(m, "Severity"), "warning"),
			"cooldown_seconds": cInt(m, "CooldownSeconds"),
			"last_fired_at":    cStr(m, "LastFiredAt"),
		})
	}
	return out
}

// loadNotificationLog mirrors app.py _load_notification_log(limit).
func (s *server) loadNotificationLog(limit int) []any {
	out := []any{}
	res, err := s.db.Execute(
		"SELECT Id, RuleId, RuleName, ChannelId, ChannelName, FiredAt, Status, ErrorMessage, Summary "+
			"FROM sobs_notification_log ORDER BY FiredAt DESC LIMIT ?", limit)
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		out = append(out, map[string]any{
			"id":            cStr(m, "Id"),
			"rule_id":       cStr(m, "RuleId"),
			"rule_name":     cStr(m, "RuleName"),
			"channel_id":    cStr(m, "ChannelId"),
			"channel_name":  cStr(m, "ChannelName"),
			"fired_at":      cStr(m, "FiredAt"),
			"status":        cStr(m, "Status"),
			"error_message": cStr(m, "ErrorMessage"),
			"summary":       cStr(m, "Summary"),
		})
	}
	return out
}

// parseNotificationConditionsJSON mirrors app.py _parse_notification_conditions_json +
// _normalize_notification_condition: decode the JSON array, normalizing each entry into the signal
// or tag condition shape (drops non-dict entries). Non-list/parse-failure -> empty.
func parseNotificationConditionsJSON(raw string) []any {
	out := []any{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	v, err := parseJSONValue([]byte(raw))
	if err != nil {
		return out
	}
	list, ok := v.([]any)
	if !ok {
		return out
	}
	for _, it := range list {
		if c := normalizeNotificationCondition(it); c != nil {
			out = append(out, c)
		}
	}
	return out
}

// normalizeNotificationCondition mirrors app.py _normalize_notification_condition.
func normalizeNotificationCondition(raw any) map[string]any {
	o, ok := raw.(*jsonenc.Object)
	if !ok {
		return nil
	}
	// gs mirrors str(raw.get(k) or "").strip(): falsy -> "", else str()-coerced then stripped.
	gs := func(k string) string {
		v, has := o.Get(k)
		return strings.TrimSpace(orDefaultVal(v, has, ""))
	}
	// objType mirrors str(raw.get("type") or "signal").strip().lower().
	objTypeRaw, hasType := o.Get("type")
	condType := strings.ToLower(strings.TrimSpace(orDefaultVal(objTypeRaw, hasType, "signal")))
	// float(raw.get("threshold") or 0): the shared condFloat already yields 0.0 for absent/falsy/
	// non-numeric, equivalent to Python's `or 0` + caught float() error.
	threshold := condFloat(o, "threshold", 0.0)
	windowMinutes := condWindowMinutes(o)
	if condType == "tag" {
		recordType := strings.ToLower(orDefault(gs("record_type"), "all"))
		if !tagRuleRecordTypes[recordType] {
			recordType = "all"
		}
		tagOp := strings.ToLower(orDefault(gs("tag_match_operator"), "eq"))
		if !notificationTagMatchOperators[tagOp] {
			tagOp = "eq"
		}
		comparator := strings.ToLower(orDefault(gs("comparator"), "gt"))
		if !notificationComparators[comparator] {
			comparator = "gt"
		}
		return map[string]any{
			"type":               "tag",
			"record_type":        recordType,
			"tag_key":            gs("tag_key"),
			"tag_match_operator": tagOp,
			"tag_value":          gs("tag_value"),
			"comparator":         comparator,
			"threshold":          threshold,
			"window_minutes":     windowMinutes,
		}
	}
	comparator := strings.ToLower(orDefault(gs("comparator"), "gt"))
	if !notificationComparators[comparator] {
		comparator = "gt"
	}
	return map[string]any{
		"type":           "signal",
		"source":         gs("source"),
		"signal":         gs("signal"),
		"service":        gs("service"),
		"comparator":     comparator,
		"threshold":      threshold,
		"window_minutes": windowMinutes,
	}
}

// notificationTagMatchOperators mirrors app.py _NOTIFICATION_TAG_MATCH_OPERATORS.
var notificationTagMatchOperators = map[string]bool{"eq": true, "contains": true, "regex": true}

// condWindowMinutes mirrors `max(1, min(60, int(raw.get("window_minutes") or 5)))` with the
// (TypeError, ValueError) -> 5 fallback. int(float) truncates toward zero; a non-int-coercible
// value falls back to 5.
func condWindowMinutes(o *jsonenc.Object) int {
	v, has := o.Get("window_minutes")
	if !has || isFalsyAny(v) {
		return clampInt(5, 1, 60)
	}
	n, ok := pyIntTrunc(v)
	if !ok {
		n = 5
	}
	return clampInt(n, 1, 60)
}

// pyIntTrunc mirrors Python int(x) for the JSON scalar types: int/float (truncate toward zero) and
// numeric strings ("5" -> 5, but "5.5" / "abc" raise -> ok=false). bool -> 0/1.
func pyIntTrunc(v any) (int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return x, true
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i), true
		}
		// int("5.5") raises in Python; int(5.5)-the-float truncates. A JSON number that is not an
		// integer literal came from a float, so truncate it (matches int(float)).
		if f, err := x.Float64(); err == nil {
			return int(f), true
		}
		return 0, false
	case string:
		t := strings.TrimSpace(x)
		if n, err := strconv.Atoi(t); err == nil {
			return n, true
		}
		return 0, false
	}
	return 0, false
}

// getVapidPublicKey mirrors app.py _get_vapid_public_key: derive the uncompressed-point public key
// (base64url, no padding) from the resolved private key, returning (key, source) or ("", "").
// loadVapidPrivateKey already applies the env-over-DB precedence and decrypts the DB value.
func (s *server) getVapidPublicKey() (string, string) {
	priv, source, err := s.loadVapidPrivateKey()
	if err != nil || priv == nil || source == "" {
		return "", ""
	}
	pub := elliptic.Marshal(elliptic.P256(), priv.PublicKey.X, priv.PublicKey.Y) //nolint:staticcheck
	return base64.RawURLEncoding.EncodeToString(pub), source
}

// ---------------------------------------------------------------------------
// AI settings page loaders (live pricing merge + confirmed models + agent runs).
// ---------------------------------------------------------------------------

// loadConfirmedAiPricingModels mirrors app.py _load_confirmed_ai_pricing_models: the normalized set
// of confirmed model names from the ai.model_pricing_confirmed JSON array.
func (s *server) loadConfirmedAiPricingModels() map[string]bool {
	out := map[string]bool{}
	raw := strings.TrimSpace(s.loadAISetting("ai.model_pricing_confirmed", ""))
	if raw == "" {
		return out
	}
	v, err := parseJSONValue([]byte(raw))
	if err != nil {
		return out
	}
	list, ok := v.([]any)
	if !ok {
		return out
	}
	for _, it := range list {
		if k := normalizeAiModelName(it); k != "" {
			out[k] = true
		}
	}
	return out
}

// sortedConfirmedAiPricingModels returns sorted(_load_confirmed_ai_pricing_models(db)) as []any.
func (s *server) sortedConfirmedAiPricingModels() []any {
	set := s.loadConfirmedAiPricingModels()
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strsToAny(keys)
}

// normalizeAiModelName mirrors app.py _normalize_ai_model_name: str(model or "").strip().lower().
func normalizeAiModelName(model any) string {
	return strings.ToLower(strings.TrimSpace(pyStr(model, model != nil)))
}

// loadObservedAiModels mirrors app.py _load_observed_ai_models: DISTINCT non-empty request models
// from otel_traces AI spans (normalized, order-preserving dedupe). Empty on the fixture.
func (s *server) loadObservedAiModels(limit int) []string {
	safeLimit := clampInt(limit, 1, 500)
	out := []string{}
	res, err := s.db.Execute(
		"SELECT DISTINCT SpanAttributes['gen_ai.request.model'] AS model FROM otel_traces " +
			"WHERE " + aiSpanCondition + " AND SpanAttributes['gen_ai.request.model'] != '' " +
			"ORDER BY model LIMIT " + itoa(safeLimit))
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, m := range rowMaps(res) {
		k := normalizeAiModelName(cStr(m, "model"))
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// loadSavedAiPricing mirrors app.py _load_saved_ai_pricing: the user-saved ai.model_pricing entries
// (normalized keys, coerced {in,out} float pairs). Empty on the fixture.
func (s *server) loadSavedAiPricing() *jsonenc.Object {
	out := jsonenc.NewObject()
	raw := strings.TrimSpace(s.loadAISetting("ai.model_pricing", ""))
	if raw == "" {
		return out
	}
	v, err := parseJSONValue([]byte(raw))
	if err != nil {
		return out
	}
	o, ok := v.(*jsonenc.Object)
	if !ok {
		return out
	}
	for _, k := range o.Keys() {
		val, _ := o.Get(k)
		entry := coerceAiPricingEntry(val)
		nk := normalizeAiModelName(k)
		if nk != "" && entry != nil {
			out.Set(nk, entry)
		}
	}
	return out
}

// coerceAiPricingEntry mirrors app.py _coerce_ai_pricing_entry: a dict with float-coercible "in"
// and "out" -> {"in": float, "out": float}; otherwise nil.
func coerceAiPricingEntry(prices any) *jsonenc.Object {
	o, ok := prices.(*jsonenc.Object)
	if !ok {
		return nil
	}
	in, hasIn := o.Get("in")
	outv, hasOut := o.Get("out")
	if !hasIn || !hasOut {
		return nil
	}
	inF, err1 := pyFloatStrict(in)
	outF, err2 := pyFloatStrict(outv)
	if err1 != nil || err2 != nil {
		return nil
	}
	return jsonenc.NewObject().Set("in", inF).Set("out", outF)
}

// inferAiPricingForModel mirrors app.py _infer_ai_pricing_for_model against the embedded
// _DEFAULT_AI_PRICING table. Returns a copied {in,out} entry.
func inferAiPricingForModel(defaults *jsonenc.Object, model string) any {
	normalized := normalizeAiModelName(model)
	genericKey := "gpt-4o"
	copyEntry := func(key string) any {
		if v, ok := defaults.Get(key); ok {
			return copyAiPricingEntry(v)
		}
		return copyAiPricingEntry(mustGet(defaults, genericKey))
	}
	if normalized == "" {
		return copyEntry(genericKey)
	}
	if _, ok := defaults.Get(normalized); ok {
		return copyEntry(normalized)
	}
	for _, knownKey := range defaults.Keys() {
		if strings.Contains(knownKey, normalized) || strings.Contains(normalized, knownKey) {
			return copyEntry(knownKey)
		}
	}
	for _, rule := range aiPricingInferenceRules {
		for _, needle := range rule.needles {
			if strings.Contains(normalized, needle) {
				return copyEntry(rule.baseKey)
			}
		}
	}
	return copyEntry(genericKey)
}

// copyAiPricingEntry mirrors app.py _copy_ai_pricing_entry: {"in": float, "out": float}.
func copyAiPricingEntry(prices any) any {
	o, ok := prices.(*jsonenc.Object)
	if !ok {
		return jsonenc.NewObject()
	}
	in, _ := o.Get("in")
	outv, _ := o.Get("out")
	return jsonenc.NewObject().Set("in", in).Set("out", outv)
}

// aiPricingInferenceRules mirrors app.py _AI_PRICING_INFERENCE_RULES (ordered).
var aiPricingInferenceRules = []struct {
	needles []string
	baseKey string
}{
	{[]string{"4o-mini"}, "gpt-4o-mini"},
	{[]string{"4o"}, "gpt-4o"},
	{[]string{"3.5"}, "gpt-3.5-turbo"},
	{[]string{"turbo"}, "gpt-4-turbo"},
	{[]string{"o3-mini"}, "o3-mini"},
	{[]string{"o1-mini"}, "o1-mini"},
	{[]string{"o1"}, "o1"},
	{[]string{"haiku"}, "claude-3-5-haiku"},
	{[]string{"sonnet"}, "claude-3-5-sonnet"},
	{[]string{"opus"}, "claude-3-opus"},
	{[]string{"claude"}, "claude-3-5-sonnet"},
	{[]string{"2.0-flash", "2-flash"}, "gemini-2.0-flash"},
	{[]string{"1.5-flash", "flash-lite", "flash"}, "gemini-1.5-flash"},
	{[]string{"1.5-pro", "pro"}, "gemini-1.5-pro"},
	{[]string{"gemini"}, "gemini-1.5-flash"},
	{[]string{"70b"}, "llama-3.1-70b"},
	{[]string{"8b"}, "llama-3.1-8b"},
	{[]string{"llama"}, "llama-3.1-8b"},
	{[]string{"large"}, "mistral-large"},
	{[]string{"small"}, "mistral-small"},
	{[]string{"mistral"}, "mistral-small"},
}

// loadAiPricingWithSources mirrors app.py _load_ai_pricing_with_sources: start from
// _DEFAULT_AI_PRICING (all "default"), add observed-model inferences, then apply DB-saved overrides
// (promoting inferred->confirmed/custom). Returns (merged, sources) as insertion-ordered objects so
// the template's `| tojson` rendering preserves the default key order. On the empty fixture (no
// observed models, no saved pricing) the result is byte-identical to the embedded default/sources
// fixtures the prior stub returned.
func (s *server) loadAiPricingWithSources() (*jsonenc.Object, *jsonenc.Object) {
	defaults, _ := parseJSONValue(defaultAiPricingJSON)
	defObj, _ := defaults.(*jsonenc.Object)
	if defObj == nil {
		defObj = jsonenc.NewObject()
	}
	merged := jsonenc.NewObject()
	sources := jsonenc.NewObject()
	for _, k := range defObj.Keys() {
		merged.Set(k, copyAiPricingEntry(mustGet(defObj, k)))
		sources.Set(k, "default")
	}

	for _, modelKey := range s.loadObservedAiModels(200) {
		if _, ok := merged.Get(modelKey); !ok {
			merged.Set(modelKey, inferAiPricingForModel(defObj, modelKey))
			sources.Set(modelKey, "inferred")
		}
	}

	confirmed := s.loadConfirmedAiPricingModels()
	saved := s.loadSavedAiPricing()
	for _, modelKey := range saved.Keys() {
		prices, _ := saved.Get(modelKey)
		merged.Set(modelKey, prices)
		cur, has := sources.Get(modelKey)
		if has && cur == "inferred" {
			if confirmed[modelKey] {
				sources.Set(modelKey, "confirmed")
			}
		} else if !has {
			sources.Set(modelKey, "custom")
		}
	}
	return merged, sources
}

// loadAgentRunsCtx mirrors app.py _load_agent_runs(db, limit): recent agent runs ordered by
// CreatedAt DESC. Empty on the fixture.
func (s *server) loadAgentRunsCtx(limit int) []any {
	out := []any{}
	res, err := s.db.Execute(
		"SELECT Id, RuleId, RuleName, TriggerContext, Status, GuardDecision, DlpResult, " +
			"Analysis, Suggestion, GithubIssueUrl, ErrorMessage, CreatedAt, CompletedAt, IsDismissed " +
			"FROM sobs_agent_runs FINAL WHERE IsDeleted=0 ORDER BY CreatedAt DESC LIMIT " + itoa(limit))
	if err != nil {
		return out
	}
	for _, m := range rowMaps(res) {
		out = append(out, map[string]any{
			"id":               cStr(m, "Id"),
			"rule_id":          cStr(m, "RuleId"),
			"rule_name":        cStr(m, "RuleName"),
			"trigger_context":  cStr(m, "TriggerContext"),
			"status":           cStr(m, "Status"),
			"guard_decision":   cStr(m, "GuardDecision"),
			"dlp_result":       cStr(m, "DlpResult"),
			"analysis":         cStr(m, "Analysis"),
			"suggestion":       cStr(m, "Suggestion"),
			"github_issue_url": cStr(m, "GithubIssueUrl"),
			"error_message":    cStr(m, "ErrorMessage"),
			"created_at":       cStr(m, "CreatedAt"),
			"completed_at":     cStr(m, "CompletedAt"),
			"is_dismissed":     cInt(m, "IsDismissed") != 0,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// MCP settings page (mcp.py mcp_settings_page safe_keys shaping).
// ---------------------------------------------------------------------------

// mcpSafeKeys mirrors mcp.py mcp_settings_page: shape the stored key descriptors as
// {id, label, created_at, expires_at} (no key_hash). id/label/created_at default to ""; expires_at
// defaults to None (nil) when absent. Empty on the fixture.
func (s *server) mcpSafeKeys() []any {
	out := []any{}
	for _, k := range s.loadMcpAPIKeys() {
		o, ok := k.(*jsonenc.Object)
		if !ok {
			continue
		}
		gs := func(key string) string {
			if v, has := o.Get(key); has {
				return pyStr(v, true)
			}
			return ""
		}
		var expires any
		if v, has := o.Get("expires_at"); has {
			expires = v
		}
		out = append(out, jsonenc.NewObject().
			Set("id", gs("id")).
			Set("label", gs("label")).
			Set("created_at", gs("created_at")).
			Set("expires_at", expires))
	}
	return out
}

// ---------------------------------------------------------------------------
// Session-cookie decoding shared by the CI-push one-time key (repositories page)
// and the flashRedirect() flash-message round trip (render.go / handlers_pages.go).
// ---------------------------------------------------------------------------

// decodeSessionCookie reads, signature-verifies (verifySessionCookiePayload, handlers_forms.go),
// and JSON-decodes the sobs_session cookie this port emits (see flashSessionCookie/
// flashRedirectWithCiKey/sessionCookieHeaderFor). A missing cookie, a bad/missing signature (e.g.
// a forged or hand-edited cookie value — without sessionSecretKey it cannot be reproduced), or a
// parse failure all yield (nil, false).
func decodeSessionCookie(r *http.Request) (*jsonenc.Object, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c == nil || c.Value == "" {
		return nil, false
	}
	payload, ok := verifySessionCookiePayload(c.Value)
	if !ok {
		return nil, false
	}
	js, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, false
	}
	v, err := parseJSONValue(js)
	if err != nil {
		return nil, false
	}
	o, ok := v.(*jsonenc.Object)
	if !ok {
		return nil, false
	}
	return o, true
}

// ciPushPlainByAppFromSession extracts the per-app one-time CI-push plaintext keys (mirrors
// session.pop("ci_push_api_key_plain_by_app")) from an already-decoded session object, so a
// caller that also needs sessionFlashedMessages/flashesFromSession from the same cookie (e.g.
// handleViewSettingsRepositories) can decode the cookie once and derive both from it.
func ciPushPlainByAppFromSession(o *jsonenc.Object) map[string]string {
	out := map[string]string{}
	if o == nil {
		return out
	}
	mv, has := o.Get("ci_push_api_key_plain_by_app")
	if !has {
		return out
	}
	mo, ok := mv.(*jsonenc.Object)
	if !ok {
		return out
	}
	for _, appID := range mo.Keys() {
		val, _ := mo.Get(appID)
		if sv, ok := val.(string); ok {
			out[appID] = sv
		}
	}
	return out
}

// sessionCiPushPlainByApp reads the per-app one-time CI-push plaintext keys stashed in the
// sobs_session cookie by flashRedirectWithCiKey (mirrors session.pop(...)). Any parse failure
// (incl. the empty-corpus no-cookie case) yields an empty map, so this only surfaces a key right
// after a rotation POST.
func sessionCiPushPlainByApp(r *http.Request) map[string]string {
	o, ok := decodeSessionCookie(r)
	if !ok {
		return map[string]string{}
	}
	return ciPushPlainByAppFromSession(o)
}

// flashesFromSession extracts any pending flash messages (app.py's session["_flashes"], set by
// flashRedirect/flashRedirectWithCiKey) from an already-decoded session object. Each entry is
// decoded from Flask/Quart's TaggedJSONSerializer tuple tag ({" t": [category, message]}) into the
// []any{category, message} pair get_flashed_messages (render.go's newEngineFlash) expects.
//
// The returned hadFlashes bool reports whether the object carried a "_flashes" key at all — Quart's
// get_flashed_messages() pops that key unconditionally (session.pop), which is what marks the
// session modified and makes Quart re-send an updated Set-Cookie in the response, even if every
// entry in the list failed to decode. Callers should only touch the response cookie when this is
// true, so requests with no session cookie — the common case — pass through untouched.
func flashesFromSession(o *jsonenc.Object) (flashes []any, hadFlashes bool) {
	if o == nil {
		return nil, false
	}
	fv, has := o.Get("_flashes")
	if !has {
		return nil, false
	}
	arr, ok := fv.([]any)
	if !ok {
		return nil, true
	}
	out := make([]any, 0, len(arr))
	for _, item := range arr {
		tagged, ok := item.(*jsonenc.Object)
		if !ok {
			continue
		}
		pair, has := tagged.Get(" t")
		if !has {
			continue
		}
		pairArr, ok := pair.([]any)
		if !ok || len(pairArr) != 2 {
			continue
		}
		out = append(out, pairArr)
	}
	return out, true
}

// sessionFlashedMessages reads any pending flash messages out of the incoming sobs_session cookie
// (see flashesFromSession for the decode).
func sessionFlashedMessages(r *http.Request) (flashes []any, hadFlashes bool) {
	o, ok := decodeSessionCookie(r)
	if !ok {
		return nil, false
	}
	return flashesFromSession(o)
}

// sessionObjectWithoutKey returns a copy of o with the given key removed (insertion order of the
// remaining keys preserved), for re-serializing a session after popping one entry — mirrors
// Quart/Flask's session.pop(key), which only drops that key and leaves the rest of the session
// dict (and so the re-sent cookie) intact.
func sessionObjectWithoutKey(o *jsonenc.Object, key string) *jsonenc.Object {
	out := jsonenc.NewObject()
	if o == nil {
		return out
	}
	for _, k := range o.Keys() {
		if k == key {
			continue
		}
		if v, has := o.Get(k); has {
			out.Set(k, v)
		}
	}
	return out
}

// sessionCookieHeaderFor builds the Set-Cookie header value for the session that remains after
// popping a key: an empty (or nil) remainder clears the cookie entirely — mirroring Quart, which
// deletes the cookie once the session dict becomes empty — otherwise it re-encodes the remaining
// session data the same way flashSessionCookie does (handlers_forms.go), so any other session
// entry (e.g. flashRedirectWithCiKey's one-time ci_push_api_key_plain_by_app) survives until it is
// independently popped by its own reader.
func sessionCookieHeaderFor(remaining *jsonenc.Object) string {
	if remaining == nil || remaining.Len() == 0 {
		return sessionCookieName + "=; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0" + sessionCookieAttrs()
	}
	payload := base64.RawURLEncoding.EncodeToString(jsonenc.Encode(remaining, flaskSessionOpts))
	return sessionCookieName + "=" + signSessionPayload(payload) + sessionCookieAttrs()
}
