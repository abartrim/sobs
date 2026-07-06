package render

import "testing"

// cov95_b16_lex_test.go — batch 16 targeted coverage for internal/render/lex.go's lex: the
// unclosed-tag-treated-as-text branch (no matching "}}"/"%}"/"#}"), the comment-token branch, and
// the {%- -%} whitespace-control trim branches (both left and right, and when the adjacent token
// is not a text token so no trim is applied).

func TestLexUnclosedTagTreatedAsText(t *testing.T) {
	// No matching "}}" is found: the text preceding the opening delimiter is flushed as its own
	// token, then textStart resets to the delimiter itself and the rest of the input (starting at
	// "{{") is flushed as a second, trailing text token — i.e. the unclosed tag reads as literal
	// text rather than being parsed as an output token.
	toks := lex("before {{ unterminated")
	if len(toks) != 2 {
		t.Fatalf("want 2 text tokens, got %d: %+v", len(toks), toks)
	}
	if toks[0].kind != tokText || toks[0].text != "before " {
		t.Errorf("first token = %+v", toks[0])
	}
	if toks[1].kind != tokText || toks[1].text != "{{ unterminated" {
		t.Errorf("second token = %+v", toks[1])
	}
}

func TestLexCommentToken(t *testing.T) {
	toks := lex("a{# a comment #}b")
	if len(toks) != 3 {
		t.Fatalf("want 3 tokens, got %d: %+v", len(toks), toks)
	}
	if toks[1].kind != tokComment || toks[1].text != "a comment" {
		t.Errorf("comment token = %+v", toks[1])
	}
	if toks[0].text != "a" || toks[2].text != "b" {
		t.Errorf("surrounding text = %q / %q", toks[0].text, toks[2].text)
	}
}

func TestLexOutputAndTagTokens(t *testing.T) {
	toks := lex("{{ x }}{% if y %}")
	if len(toks) != 2 {
		t.Fatalf("want 2 tokens, got %d: %+v", len(toks), toks)
	}
	if toks[0].kind != tokOutput || toks[0].text != "x" {
		t.Errorf("output token = %+v", toks[0])
	}
	if toks[1].kind != tokTag || toks[1].text != "if y" {
		t.Errorf("tag token = %+v", toks[1])
	}
}

func TestLexWhitespaceControlTrimsAdjacentText(t *testing.T) {
	// {%- trims trailing whitespace from the PRECEDING text token; -%} trims leading whitespace
	// from the FOLLOWING text token.
	toks := lex("a   \n{%- if x -%}\n   b")
	if len(toks) != 3 {
		t.Fatalf("want 3 tokens, got %d: %+v", len(toks), toks)
	}
	if toks[0].text != "a" {
		t.Errorf("preceding text not trimmed: %q", toks[0].text)
	}
	if toks[2].text != "b" {
		t.Errorf("following text not trimmed: %q", toks[2].text)
	}
	if !toks[1].trimLeft || !toks[1].trimRight {
		t.Errorf("trim markers not recorded: %+v", toks[1])
	}
}

func TestLexWhitespaceControlNoAdjacentTextIsSafe(t *testing.T) {
	// Two trim-marked tags back-to-back: neither has a text-token neighbor on the trimmed side, so
	// applyTrim's guard (idx>0/idx+1<len && kind==tokText) must skip without panicking.
	toks := lex("{%- if x -%}{%- endif -%}")
	if len(toks) != 2 {
		t.Fatalf("want 2 tokens, got %d: %+v", len(toks), toks)
	}
	for _, tok := range toks {
		if tok.kind != tokTag {
			t.Errorf("unexpected token kind: %+v", tok)
		}
	}
}

func TestLexEmptyTemplate(t *testing.T) {
	if toks := lex(""); len(toks) != 0 {
		t.Errorf("want no tokens for empty input, got %+v", toks)
	}
}

func TestLexPlainTextOnly(t *testing.T) {
	toks := lex("just some plain text, no tags")
	if len(toks) != 1 || toks[0].kind != tokText {
		t.Fatalf("got %+v", toks)
	}
}
