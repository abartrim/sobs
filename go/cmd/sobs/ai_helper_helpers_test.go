package main

import (
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
)

// Pure ai_helper helpers — corpus-unreachable (LLM streaming/upstream). Oracles:
// urllib.parse.quote(s, safe='') (pyQuoteAll), _build_ai_turn_logs_url, _coerce_llm_content,
// OpenAI streaming delta/finish parsing, _coerce_summary_value.

func TestPyQuoteAll(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a b", "a%20b"},
		{"/", "%2F"},
		{"'", "%27"},
		{"=", "%3D"},
		{"ABCxyz019_.-~", "ABCxyz019_.-~"}, // unreserved unchanged
	}
	for _, c := range cases {
		if got := pyQuoteAll(c.in); got != c.want {
			t.Errorf("pyQuoteAll(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildAITurnLogsURL(t *testing.T) {
	got := buildAITurnLogsURL("chat'1", "turn1")
	if !strings.HasPrefix(got, "/logs?sql=") {
		t.Fatalf("missing prefix: %q", got)
	}
	enc := got[len("/logs?sql="):]
	// Everything after sql= is urllib.parse.quote(..., safe='') -> no raw spaces or quotes.
	if strings.ContainsAny(enc, " '") {
		t.Errorf("query not fully url-encoded: %q", enc)
	}
	// The chat id's single quote is SQL-escaped (doubled) before url-encoding -> "''" -> "%27%27".
	if !strings.Contains(enc, "%27%27") {
		t.Errorf("expected doubled-quote SQL escape (%%27%%27) in %q", enc)
	}
}

func TestCoerceLLMContent(t *testing.T) {
	if got := coerceLLMContent("plain"); got != "plain" {
		t.Errorf("string: got %q", got)
	}
	parts := []any{"a", jsonenc.NewObject().Set("text", "b"), "c"}
	if got := coerceLLMContent(parts); got != "abc" {
		t.Errorf("parts: got %q, want abc", got)
	}
	if got := coerceLLMContent([]any{}); got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}
}

func TestExtractStreamDelta(t *testing.T) {
	event := jsonenc.NewObject().Set("choices", []any{
		jsonenc.NewObject().Set("delta", jsonenc.NewObject().Set("content", "hello")),
	})
	if got := extractStreamDelta(event); got != "hello" {
		t.Errorf("delta content: got %q, want hello", got)
	}
	if got := extractStreamDelta(jsonenc.NewObject()); got != "" {
		t.Errorf("no choices: got %q, want empty", got)
	}
}

func TestExtractStreamFinishReason(t *testing.T) {
	event := jsonenc.NewObject().Set("choices", []any{
		jsonenc.NewObject().Set("finish_reason", "stop"),
	})
	if got := extractStreamFinishReason(event); got != "stop" {
		t.Errorf("finish_reason: got %q, want stop", got)
	}
	if got := extractStreamFinishReason(jsonenc.NewObject()); got != "" {
		t.Errorf("no choices: got %q, want empty", got)
	}
}

func TestCoerceSummaryValue(t *testing.T) {
	if got := coerceSummaryValue("  hello world  ", 5); got != "hello" {
		t.Errorf("trim+slice: got %q, want hello", got)
	}
	if got := coerceSummaryValue("hi", 10); got != "hi" {
		t.Errorf("under max: got %q, want hi", got)
	}
}

func TestObjGetArr(t *testing.T) {
	o := jsonenc.NewObject().Set("a", []any{1, 2}).Set("b", "notlist")
	if got := objGetArr(o, "a"); len(got) != 2 {
		t.Errorf("array: len %d, want 2", len(got))
	}
	if got := objGetArr(o, "b"); got != nil {
		t.Errorf("non-list: got %v, want nil", got)
	}
	if got := objGetArr(o, "missing"); got != nil {
		t.Errorf("missing: got %v, want nil", got)
	}
	if got := objGetArr(nil, "a"); got != nil {
		t.Errorf("nil obj: got %v, want nil", got)
	}
}
