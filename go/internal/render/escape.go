package render

import "html"

// escapeHTML reproduces MarkupSafe's escape() exactly (Jinja's autoescape): & -> &amp;,
// < -> &lt;, > -> &gt;, " -> &#34;, ' -> &#39;. Go's stdlib html.EscapeString escapes the
// same five characters to the same numeric entities (&#34;/&#39;, not &quot;/&apos;),
// verified byte-identical against MarkupSafe's output by fuzz test — using it directly
// means CodeQL's go/reflected-xss query recognizes this as a barrier, unlike a hand-rolled
// replacer of the same five characters.
func escapeHTML(s string) string { return html.EscapeString(s) }
