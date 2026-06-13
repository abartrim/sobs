// Package render is a byte-faithful Jinja2 interpreter for the subset of constructs the
// SOBS templates use. We interpret the Jinja templates directly (rather than transpiling
// to text/template) so we have exact control over whitespace and escaping — the only way
// to reach byte-for-byte parity with Quart's Jinja output. See migration/JINJA_TO_GO_SPEC.md.
package render

import "strings"

type tokKind int

const (
	tokText    tokKind = iota // literal text
	tokOutput                 // {{ ... }}
	tokTag                    // {% ... %}
	tokComment                // {# ... #}
)

type token struct {
	kind tokKind
	text string // inner text for output/tag/comment; raw text for tokText
	// trimLeft/trimRight record Jinja whitespace-control markers ({%- ... -%}).
	trimLeft  bool
	trimRight bool
}

// lex splits a template into tokens. It mirrors Jinja's default (no trim_blocks /
// lstrip_blocks — verified against the app's env) and records explicit {%- -%} markers.
func lex(src string) []token {
	var toks []token
	i := 0
	n := len(src)
	textStart := 0

	flushText := func(end int) {
		if end > textStart {
			toks = append(toks, token{kind: tokText, text: src[textStart:end]})
		}
	}

	for i < n {
		if src[i] == '{' && i+1 < n {
			switch src[i+1] {
			case '{', '%', '#':
				open := src[i+1]
				flushText(i)
				closeStr := map[byte]string{'{': "}}", '%': "%}", '#': "#}"}[open]
				// find matching close
				j := strings.Index(src[i+2:], closeStr)
				if j < 0 {
					// no close: treat rest as text
					textStart = i
					i = n
					continue
				}
				inner := src[i+2 : i+2+j]
				trimL := strings.HasPrefix(inner, "-")
				trimR := strings.HasSuffix(inner, "-")
				inner = strings.TrimSpace(strings.Trim(inner, "-"))
				kind := map[byte]tokKind{'{': tokOutput, '%': tokTag, '#': tokComment}[open]
				toks = append(toks, token{kind: kind, text: inner, trimLeft: trimL, trimRight: trimR})
				i = i + 2 + j + len(closeStr)
				textStart = i
				continue
			}
		}
		i++
	}
	flushText(n)
	return applyTrim(toks)
}

// applyTrim implements {%- and -%} whitespace control: strip whitespace from the adjacent
// text token on the marked side (Jinja trims all whitespace up to/including newlines).
func applyTrim(toks []token) []token {
	for idx := range toks {
		t := toks[idx]
		if t.kind == tokText {
			continue
		}
		if t.trimLeft && idx > 0 && toks[idx-1].kind == tokText {
			toks[idx-1].text = strings.TrimRight(toks[idx-1].text, " \t\r\n")
		}
		if t.trimRight && idx+1 < len(toks) && toks[idx+1].kind == tokText {
			toks[idx+1].text = strings.TrimLeft(toks[idx+1].text, " \t\r\n")
		}
	}
	return toks
}
