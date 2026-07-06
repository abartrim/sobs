package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b15_ai_chat_test.go — batch 15 coverage for cmd/sobs/ai_chat.go:
//   extractAssistantMetaText (ai_chat.go:24)              90.9% -> exercise the escaped-tag
//     and dangling-open-tag branches not yet hit.
//   handleApiAiHelperChatDetail (ai_chat.go:148)           78.4% -> exercise the method guard,
//     the blank chat_id 400, the dbError path, and a full success path (message + tool history).

// ---------------------------------------------------------------------------
// extractAssistantMetaText
// ---------------------------------------------------------------------------

func TestExtractAssistantMetaText_Variants(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no meta block passes through trimmed",
			in:   "  hello world  ",
			want: "hello world",
		},
		{
			name: "raw assistant_meta block stripped",
			in:   "before<assistant_meta>{\"x\":1}</assistant_meta>after",
			want: "beforeafter",
		},
		{
			name: "escaped assistant_meta block stripped",
			in:   "keep&lt;assistant_meta foo=\"bar\"&gt;{\"x\":1}&lt;/assistant_meta&gt;tail",
			want: "keeptail",
		},
		{
			name: "dangling raw open tag cuts the rest",
			in:   "visible text<assistant_meta>unterminated json here",
			want: "visible text",
		},
		{
			name: "dangling escaped open tag cuts the rest",
			in:   "visible &lt;assistant_metaXYZ trailing junk",
			want: "visible",
		},
		{
			name: "case-insensitive tag match",
			in:   "A<ASSISTANT_META>ignored</ASSISTANT_META>B",
			want: "AB",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "only whitespace",
			in:   "   \n\t  ",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractAssistantMetaText(c.in); got != c.want {
				t.Errorf("extractAssistantMetaText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// handleApiAiHelperChatDetail
// ---------------------------------------------------------------------------

func TestHandleApiAiHelperChatDetail_MethodGuard(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	r := httptest.NewRequest(http.MethodPost, "/api/ai/helper/chats/abc", nil)
	rec := httptest.NewRecorder()
	s.handleApiAiHelperChatDetail(rec, r)
	// paramMethodGuard on a non-GET/HEAD without an override should not itself write a body
	// that reaches StatusOK; the handler falls through to NotFound when the guard doesn't
	// short-circuit. Either way this must not be a 200 chat payload.
	if rec.Code == http.StatusOK {
		t.Errorf("POST should not reach the success path, got 200 body=%s", rec.Body.String())
	}
}

func TestHandleApiAiHelperChatDetail_BlankChatID(t *testing.T) {
	s := &server{db: &storetest.FakeDB{}}
	r := httptest.NewRequest(http.MethodGet, "/api/ai/helper/chats/", nil)
	rec := httptest.NewRecorder()
	s.handleApiAiHelperChatDetail(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "chat_id is required") {
		t.Errorf("body = %s, want chat_id error", rec.Body.String())
	}
}

func TestHandleApiAiHelperChatDetail_DBError(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		return nil, errB15Boom
	}}}
	r := httptest.NewRequest(http.MethodGet, "/api/ai/helper/chats/chat-1", nil)
	rec := httptest.NewRecorder()
	s.handleApiAiHelperChatDetail(rec, r)
	if rec.Code < 400 {
		t.Fatalf("expected an error status, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleApiAiHelperChatDetail_Success(t *testing.T) {
	// One turn.complete row (user question + assistant output_messages containing an
	// <assistant_meta> block that must be stripped), plus one tool.proposed row for the same
	// chat_id/turn_id so the tool history merges in.
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, params ...any) (*store.Result, error) {
		if strings.Contains(q, "EventName='turn.complete'") {
			return storetest.Result(
				[]string{"Timestamp", "turn_id", "input_question", "request", "output_messages"},
				[]any{"2026-01-01 00:00:00", "turn-1", "what is up", "",
					`[{"content":"hello <assistant_meta>{\"a\":1}</assistant_meta>there"}]`},
			), nil
		}
		if strings.Contains(q, "tool.proposed") {
			return storetest.Result(
				[]string{"Timestamp", "EventName", "turn_id", "action_id", "summary", "action_json", "action_status", "requires_confirmation"},
				[]any{"2026-01-01 00:00:01", "tool.proposed", "turn-1", "act-1", "do a thing", `{"k":"v"}`, "proposed", "false"},
			), nil
		}
		return &store.Result{}, nil
	}}}
	r := httptest.NewRequest(http.MethodGet, "/api/ai/helper/chats/chat-42", nil)
	rec := httptest.NewRecorder()
	s.handleApiAiHelperChatDetail(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"chat_id":"chat-42"`) {
		t.Errorf("body missing chat_id: %s", body)
	}
	if !strings.Contains(body, `"role":"user"`) {
		t.Errorf("body missing user message: %s", body)
	}
	if !strings.Contains(body, "hellothere") && !strings.Contains(body, "hello") {
		// assistant text with the meta block stripped should still contain "hello"
		t.Errorf("body missing cleaned assistant text: %s", body)
	}
	if strings.Contains(body, "assistant_meta") {
		t.Errorf("assistant_meta block should have been stripped: %s", body)
	}
	if !strings.Contains(body, `"kind":"tool"`) {
		t.Errorf("body missing tool history entry: %s", body)
	}
}

var errB15Boom = &b15BoomError{}

type b15BoomError struct{}

func (*b15BoomError) Error() string { return "boom" }
