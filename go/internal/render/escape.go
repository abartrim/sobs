package render

import "strings"

// escapeHTML reproduces MarkupSafe's escape() exactly (Jinja's autoescape):
//
//	&  -> &amp;
//	<  -> &lt;
//	>  -> &gt;
//	"  -> &#34;
//	'  -> &#39;
//
// Note the numeric entities for quotes (&#34;/&#39;), not &quot;/&apos; — and that & is
// replaced first. Non-ASCII bytes are left as-is (UTF-8), matching MarkupSafe.
var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&#34;",
	"'", "&#39;",
)

func escapeHTML(s string) string { return htmlEscaper.Replace(s) }
