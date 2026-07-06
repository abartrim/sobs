package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/store/storetest"
)

// cov95_b15_render_test.go — batch 15 coverage for cmd/sobs/render.go:
//   urlFor (104)          90.5%
//   handleHelpPage (197)  81.8%
//   newEngineFlash (42)   83.3%
//   argStr (92)           60.0%

func TestArgStr(t *testing.T) {
	cases := []struct {
		name string
		pos  []any
		i    int
		want string
	}{
		{"in-range string", []any{"hello"}, 0, "hello"},
		{"in-range non-string formats with %v", []any{42}, 0, "42"},
		{"second position", []any{"a", "b"}, 1, "b"},
		{"out of range -> empty", []any{"a"}, 5, ""},
		{"empty slice -> empty", []any{}, 0, ""},
		{"nil in slice formats as <nil>", []any{nil}, 0, "<nil>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := argStr(c.pos, c.i); got != c.want {
				t.Errorf("argStr(%v, %d) = %q, want %q", c.pos, c.i, got, c.want)
			}
		})
	}
}

// TestNewEngineFlash_Globals renders a small self-contained template (no {% extends %], so it
// does not depend on base.html) that exercises every global newEngineFlash installs:
// get_flashed_messages (both with_categories forms), source_label, signal_label, and
// signal_description — including their known-key and unknown-key-fallback branches.
func TestNewEngineFlash_Globals(t *testing.T) {
	dir := t.TempDir()
	tmpl := `msgs=[{% for m in get_flashed_messages() %}{{ m }}|{% endfor %}]` +
		`cats=[{% for c, m in get_flashed_messages(with_categories=true) %}{{ c }}:{{ m }};{% endfor %}]` +
		`known_source={{ source_label('logs') }} ` +
		`unknown_source={{ source_label('custom_thing') }} ` +
		`unknown_signal_label={{ signal_label('nosuch', 'sig_name') }} ` +
		`unknown_signal_desc=[{{ signal_description('nosuch', 'sig_name') }}]`
	if err := os.WriteFile(filepath.Join(dir, "probe.html"), []byte(tmpl), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &server{cfg: config{TemplateDir: dir}, db: &storetest.FakeDB{}}
	flashes := []any{[]any{"success", "Saved!"}, []any{"error", "Oops"}}
	eng := s.newEngineFlash(flashes)

	out, err := eng.Render("probe.html", map[string]any{})
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}

	if !strings.Contains(out, "msgs=[Saved!|Oops|]") {
		t.Errorf("get_flashed_messages(no categories) wrong: %s", out)
	}
	if !strings.Contains(out, "cats=[success:Saved!;error:Oops;]") {
		t.Errorf("get_flashed_messages(with_categories) wrong: %s", out)
	}
	if !strings.Contains(out, "known_source=Logs") {
		t.Errorf("source_label(logs) wrong: %s", out)
	}
	if !strings.Contains(out, "unknown_source=Custom Thing") {
		t.Errorf("source_label fallback wrong: %s", out)
	}
	if !strings.Contains(out, "unknown_signal_label=Sig Name") {
		t.Errorf("signal_label fallback wrong: %s", out)
	}
	if !strings.Contains(out, "unknown_signal_desc=[]") {
		t.Errorf("signal_description fallback should be empty: %s", out)
	}
}

func TestUrlFor_StaticAndEndpoints(t *testing.T) {
	s := &server{cfg: config{BasePath: ""}}

	// static endpoint: filename kwarg -> /static/<filename>.
	out, err := s.urlFor([]any{"static"}, map[string]any{"filename": "app.css"}, []string{"filename"})
	if err != nil || out != "/static/app.css" {
		t.Errorf("urlFor(static) = (%v, %v), want /static/app.css", out, err)
	}

	// A named endpoint with a path param substituted in, no extra query params.
	out, err = s.urlFor([]any{"ai_helper_chat_detail"}, map[string]any{"chat_id": "abc-123"}, []string{"chat_id"})
	if err != nil || out != "/api/ai/helper/chats/abc-123" {
		t.Errorf("urlFor(chat_detail) = (%v, %v), want /api/ai/helper/chats/abc-123", out, err)
	}

	// A named endpoint with a path param AND an extra kwarg that becomes a query string.
	out, err = s.urlFor([]any{"ai_helper_chat_detail"}, map[string]any{"chat_id": "abc", "verbose": "1"},
		[]string{"chat_id", "verbose"})
	if err != nil || out != "/api/ai/helper/chats/abc?verbose=1" {
		t.Errorf("urlFor(chat_detail+query) = (%v, %v), want with ?verbose=1", out, err)
	}

	// A kwarg whose value is nil must be OMITTED from the query string entirely.
	out, err = s.urlFor([]any{"ai_helper_chat_detail"}, map[string]any{"chat_id": "abc", "grouped": nil},
		[]string{"chat_id", "grouped"})
	if err != nil || out != "/api/ai/helper/chats/abc" {
		t.Errorf("urlFor(nil kwarg omitted) = (%v, %v), want no query string", out, err)
	}

	// Unknown endpoint -> error.
	if _, err := s.urlFor([]any{"totally_bogus_endpoint"}, nil, nil); err == nil {
		t.Error("expected an error for an unknown endpoint")
	}

	// No positional args at all -> error ("url_for requires an endpoint").
	if _, err := s.urlFor(nil, nil, nil); err == nil {
		t.Error("expected an error when no endpoint is given")
	}

	// BasePath is prefixed onto both static and rule paths.
	sPrefixed := &server{cfg: config{BasePath: "/sobs"}}
	out, err = sPrefixed.urlFor([]any{"static"}, map[string]any{"filename": "x.js"}, []string{"filename"})
	if err != nil || out != "/sobs/static/x.js" {
		t.Errorf("urlFor with BasePath = (%v, %v), want /sobs/static/x.js", out, err)
	}
}

func TestUrlFor_QueryEscaping(t *testing.T) {
	s := &server{}
	// Werkzeug-relaxed escaping: sub-delims like ' ( ) , : stay literal in query values, but
	// space becomes '+' and other reserved chars are percent-escaped.
	outAny, err := s.urlFor([]any{"ai_helper_chat_detail"},
		map[string]any{"chat_id": "abc", "sql": "has_tag('env','prod') a b"},
		[]string{"chat_id", "sql"})
	if err != nil {
		t.Fatalf("urlFor error: %v", err)
	}
	out, _ := outAny.(string)
	if !strings.Contains(out, "has_tag('env','prod')") {
		t.Errorf("expected sub-delims left literal, got %s", out)
	}
	if !strings.Contains(out, "a+b") {
		t.Errorf("expected space encoded as '+', got %s", out)
	}
}

func TestHandleHelpPage_RendersTemplate(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{}}
	h := s.handleHelpPage("view_masking_help", "masking_help.html")
	r := httptest.NewRequest(http.MethodGet, "/settings/masking/help", nil)
	rec := httptest.NewRecorder()
	h(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("expected a Content-Length header")
	}
	if !strings.Contains(rec.Body.String(), "Output Masking Help") {
		t.Errorf("body missing expected page content: %d bytes", rec.Body.Len())
	}
}

func TestHandleHelpPage_TemplateErrorIs500(t *testing.T) {
	s := &server{cfg: config{TemplateDir: "../../../templates"}, db: &storetest.FakeDB{}}
	// A template name that does not exist forces eng.Render to return an error.
	h := s.handleHelpPage("bogus_endpoint", "this_template_does_not_exist.html")
	r := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	h(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}
