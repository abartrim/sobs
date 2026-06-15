package main

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"strconv"

	"github.com/sobs/sobs/internal/jsonenc"
)

// severityNumber mirrors app.py _severity_number.
func severityNumber(level string) int {
	switch upper(level) {
	case "TRACE":
		return 1
	case "DEBUG":
		return 5
	case "WARN", "WARNING":
		return 13
	case "ERROR":
		return 17
	case "CRITICAL", "FATAL":
		return 21
	default: // INFO / METRIC / unknown
		return 9
	}
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

// fakeTimeNs mirrors the frozen time.time_ns() (determinism: int(FIXED_EPOCH * 1e9)).
func fakeTimeNs() string {
	return strconv.FormatInt(int64(nowUTC().UnixNano()), 10)
}

// emitAiHelperLogEvent mirrors app.py _emit_ai_helper_log_event: write one gen_ai telemetry event
// to otel_logs + otel_traces. Best-effort (Python wraps the write in try/except), so insert
// errors are swallowed. attrs values are already strings (the Map columns are String->String).
func (s *server) emitAiHelperLogEvent(eventName, chatID, turnID, page, model, guardModel, thinkingLevel, body, severity string, attrs map[string]string) {
	logAttrs := map[string]any{
		"gen_ai.system":                 "sobs",
		"gen_ai.operation.name":         "chat",
		"gen_ai.chat_id":                chatID,
		"gen_ai.turn_id":                turnID,
		"gen_ai.request.model":          model,
		"gen_ai.guard.model":            guardModel,
		"gen_ai.request.thinking_level": thinkingLevel,
		"sobs.ai.page":                  page,
		"sobs.ai.event":                 eventName,
	}
	for k, v := range attrs {
		logAttrs[k] = v
	}
	resourceAttrs := map[string]any{"service.name": aiHelperServiceName, "telemetry.sdk.name": "sobs"}
	now := nowISO()
	statusCode := "STATUS_CODE_OK"
	if upper(severity) == "ERROR" {
		statusCode = "STATUS_CODE_ERROR"
	}
	logRow := map[string]any{
		"Timestamp": now, "TraceId": chatID, "SpanId": turnID, "TraceFlags": 0,
		"SeverityText": severity, "SeverityNumber": severityNumber(severity),
		"ServiceName": aiHelperServiceName, "Body": body, "ResourceSchemaUrl": "",
		"ResourceAttributes": resourceAttrs, "ScopeSchemaUrl": "", "ScopeName": "sobs.gen_ai.helper",
		"ScopeVersion": "1", "ScopeAttributes": map[string]any{}, "LogAttributes": logAttrs,
		"EventName": eventName,
	}
	traceSpanID := turnID
	traceParent := ""
	if eventName != "turn.start" {
		sum := md5.Sum([]byte(turnID + "|" + eventName + "|" + fakeTimeNs()))
		traceSpanID = hex.EncodeToString(sum[:])[:16]
		traceParent = turnID
	}
	durationNs := 0
	if v, ok := attrs["gen_ai.response.latency_ms"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			durationNs = int(f * 1_000_000)
		}
	}
	traceRow := map[string]any{
		"Timestamp": now, "TraceId": chatID, "SpanId": traceSpanID, "ParentSpanId": traceParent,
		"TraceState": "", "SpanName": "ai." + eventName, "SpanKind": "INTERNAL",
		"ServiceName": aiHelperServiceName, "ResourceAttributes": resourceAttrs,
		"ScopeName": "sobs.gen_ai.helper", "ScopeVersion": "1", "SpanAttributes": logAttrs,
		"Duration": durationNs, "StatusCode": statusCode, "StatusMessage": body,
	}
	_, _ = s.insertRowsNormalized("otel_logs", []map[string]any{logRow})
	_, _ = s.insertRowsNormalized("otel_traces", []map[string]any{traceRow})
	// Side-effect mirroring app.py _emit_ai_helper_log_event: track the discovered log attr keys.
	// (Unlike the other inserters, the AI emitter does NOT apply tag rules.)
	s.rememberLogAttrKeys(extractLogAttrMaps([]map[string]any{logRow}))
}

// POST /api/ai/helper/feedback — app.py ai_helper_feedback: record a user feedback note as a
// turn.feedback telemetry event.
func (s *server) handleApiAiHelperFeedback(w http.ResponseWriter, r *http.Request) {
	m := bodyMap(r)
	chatID := bstr(m, "chat_id")
	turnID := bstr(m, "turn_id")
	note := bstr(m, "note")
	page := bstr(m, "page")
	if page == "" {
		page = "/logs"
	}
	if chatID == "" || turnID == "" || note == "" {
		s.writeMaskedJSON(w, http.StatusBadRequest,
			jsonenc.NewObject().Set("ok", false).Set("error", "chat_id, turn_id, and note are required"))
		return
	}
	s.emitAiHelperLogEvent("turn.feedback", chatID, turnID, page, "", "", "off", note, "INFO",
		map[string]string{"gen_ai.feedback.note": note, "gen_ai.feedback.kind": "user_note"})
	s.writeMaskedJSON(w, http.StatusOK, jsonenc.NewObject().Set("ok", true))
}
