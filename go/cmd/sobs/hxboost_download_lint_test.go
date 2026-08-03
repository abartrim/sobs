package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is a partial, honest automated check for the hx-boost contract documented in the
// comment above <body hx-boost="true" ...> in templates/base.html (added in PR #455, which had
// no automated enforcement for it — this closes that gap for ONE of its three categories).
//
// That contract: every same-origin <a href>/<form> is boosted by default, and a form/link MUST
// set hx-boost="false" if it falls into one of three categories:
//  1. Its POST handler can respond with something other than a redirect to a full mainContent
//     page on every path (e.g. a bare JSON/partial body on a validation-error branch).
//  2. It triggers a file download via a `Content-Disposition: attachment` response header (the
//     vendored htmx build does not special-case a `download` attribute).
//  3. It has its own JS 'submit'/'click' handler that needs to fully own the request.
//
// WHAT IS AUTOMATED HERE: category 2 only, via two cooperating tests below.
//   - TestKnownDownloadEndpointsCoverAllHandlers walks the Go AST of every non-test file in this
//     package (go/cmd/sobs) for `<expr>.Set("Content-Disposition", <literal containing
//     "attachment">)` call sites and fails if the enclosing function is not registered in
//     knownDownloadEndpoints below — so a NEW download handler cannot be added without also
//     wiring in its url_for endpoint name(s), keeping the map from silently going stale.
//   - TestHxBoostDownloadLinksNeedBoostFalse scans templates/*.html for <a>/<form> tags that
//     either carry a literal `download` attribute or resolve via url_for(...) to an endpoint in
//     knownDownloadEndpoints, and fails if such a tag is missing hx-boost="false".
//
// WHAT IS NOT COVERED (intentionally — do not read a green run of this test as "the hx-boost
// contract is fully enforced"):
//   - Categories 1 and 3 are not statically checkable at all. Category 1 would require modeling
//     every response path of every POST handler (does it always flashRedirect, or can some
//     branch write a bare body/non-2xx status?) — out of reach for a static scan. Category 3
//     would require reliably proving a given <a>/<form> element IS or ISN'T the target of some
//     'submit'/'click' listener anywhere in the page's <script> blocks (inline scripts, event
//     delegation, selector-based binding) — also not attempted here.
//   - hx-boost inherited from an ancestor element rather than set on the tag itself. Every
//     existing opt-out in this codebase sets hx-boost="false" directly on the <a>/<form> (see
//     templates/base.html's saveReportForm, templates/custom_dashboard_view.html's chart export
//     link), so this scan only looks at each tag's own attributes. A future container-level
//     opt-out (e.g. a <div hx-boost="false"> wrapping several download links) would silently
//     satisfy the real contract while still tripping this check as a false positive — and,
//     conversely, this check cannot see an ancestor opt-out being incorrectly relied upon.
//   - A download reachable only via JS (fetch()/window.location, with no <a href>/<form action>
//     in a template) is invisible to a template scan by construction.
//   - The `download` attribute half of category 2 is a heuristic, not the real signal: htmx does
//     not care whether the attribute is present. A future <a download> that DOESN'T hit a
//     Content-Disposition:attachment endpoint (e.g. a client-side blob download, like
//     templates/query.html's CSV export) doesn't strictly need hx-boost="false" for correctness,
//     but flagging it anyway costs nothing and matches this codebase's existing convention of
//     pairing `download` with hx-boost="false" wherever both are present.
//
// In short: a green run here rules out ONE concrete, previously-unenforced mistake (a
// known-or-marked download link silently losing its file to the htmx:beforeSwap fallback). It is
// not a substitute for the by-hand review the base.html comment still asks for on categories 1
// and 3.

// knownDownloadEndpoints maps a url_for endpoint name whose handler sets `Content-Disposition:
// attachment` to the Go function name that sets it. TestKnownDownloadEndpointsCoverAllHandlers
// keeps this honest: add an entry here whenever a new handler starts setting that header, or the
// build fails.
var knownDownloadEndpoints = map[string]string{
	"api_export_reports": "handleApiReportsExport",
	"export_ai_training": "handleApiAiExport",
	"export_chart":       "exportChart",
}

// contentDispositionHandlerFuncs returns the set of top-level function names declared in dir
// (excluding _test.go files) whose body contains a call setting the Content-Disposition header
// to a value containing "attachment" — i.e. a file-download response.
func contentDispositionHandlerFuncs(t *testing.T, dir string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	found := map[string]bool{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			setsDownloadDisposition := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Set" || len(call.Args) != 2 {
					return true
				}
				if literalStringValue(call.Args[0]) != "Content-Disposition" {
					return true
				}
				if strings.Contains(literalStringValue(call.Args[1]), "attachment") {
					setsDownloadDisposition = true
				}
				return true
			})
			if setsDownloadDisposition {
				found[fn.Name.Name] = true
			}
		}
	}
	return found
}

// literalStringValue flattens a string expression — including `+`-concatenations of multiple
// literals, e.g. `"attachment; filename=\""+filename+"\""` — into the joined text of its literal
// parts. Non-literal sub-expressions (identifiers, calls) contribute nothing, which is fine here:
// every existing Content-Disposition value in this codebase has "attachment" in a literal part.
func literalStringValue(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			if v, err := strconv.Unquote(e.Value); err == nil {
				return v
			}
		}
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return literalStringValue(e.X) + literalStringValue(e.Y)
		}
	}
	return ""
}

// TestKnownDownloadEndpointsCoverAllHandlers fails if either side of knownDownloadEndpoints has
// gone stale: a handler that sets Content-Disposition:attachment but isn't registered (the map is
// missing an entry — the actual "did someone forget to wire this in" signal), or a registered
// endpoint name that no longer resolves via url_for (a rename/removal left a dangling entry).
func TestKnownDownloadEndpointsCoverAllHandlers(t *testing.T) {
	found := contentDispositionHandlerFuncs(t, ".")

	registeredFuncs := map[string]bool{}
	for endpoint, fn := range knownDownloadEndpoints {
		if _, ok := endpointPaths[endpoint]; !ok {
			t.Errorf("knownDownloadEndpoints[%q] = %q: no such url_for endpoint in endpointPaths "+
				"(renamed or removed? update or delete this entry)", endpoint, fn)
		}
		registeredFuncs[fn] = true
	}

	for fn := range found {
		if !registeredFuncs[fn] {
			t.Errorf("func %s sets Content-Disposition to an \"attachment\" value but is not "+
				"registered in knownDownloadEndpoints (go/cmd/sobs/hxboost_download_lint_test.go) "+
				"— add its url_for endpoint name(s) there so TestHxBoostDownloadLinksNeedBoostFalse "+
				"can check templates for a missing hx-boost=\"false\"", fn)
		}
	}
}

var (
	tagRe          = regexp.MustCompile(`<(?:a|form)\b[^>]*>`)
	downloadAttrRe = regexp.MustCompile(`\bdownload\b`)
	hxBoostFalseRe = regexp.MustCompile(`hx-boost\s*=\s*"false"`)
	urlForTargetRe = regexp.MustCompile(`(?:href|action)\s*=\s*"[^"]*\{\{\s*url_for\(\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]`)
)

// TestHxBoostDownloadLinksNeedBoostFalse scans every templates/*.html file for <a>/<form> tags
// that look like file-download triggers (see the package doc comment above for exactly what
// "look like" means and doesn't cover) and fails if such a tag is missing hx-boost="false".
func TestHxBoostDownloadLinksNeedBoostFalse(t *testing.T) {
	templatesDir := filepath.Join("..", "..", "..", "templates")
	files, err := filepath.Glob(filepath.Join(templatesDir, "*.html"))
	if err != nil {
		t.Fatalf("globbing %s: %v", templatesDir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no *.html files found under %s — templatesDir is likely wrong", templatesDir)
	}

	var violations []string
	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		rel, _ := filepath.Rel(templatesDir, path)
		for _, tag := range tagRe.FindAllString(string(content), -1) {
			if hxBoostFalseRe.MatchString(tag) {
				continue
			}

			var reason string
			if downloadAttrRe.MatchString(tag) {
				reason = "has a `download` attribute"
			} else if m := urlForTargetRe.FindStringSubmatch(tag); m != nil {
				if _, ok := knownDownloadEndpoints[m[1]]; ok {
					reason = fmt.Sprintf("targets known-download endpoint %q", m[1])
				}
			}
			if reason == "" {
				continue
			}

			snippet := tag
			if len(snippet) > 200 {
				snippet = snippet[:200] + "..."
			}
			violations = append(violations, fmt.Sprintf("%s: %s but is missing hx-boost=\"false\":\n    %s", rel, reason, snippet))
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("%d template tag(s) look like file downloads but do not opt out of htmx boost "+
			"(see the hx-boost contract comment above <body> in templates/base.html, category 2: "+
			"a boosted download link gets XHR-fetched and swap-checked before ever falling back to "+
			"a real navigation, silently discarding the attachment response). Add hx-boost=\"false\" "+
			"to each:\n\n%s", len(violations), strings.Join(violations, "\n\n"))
	}
}
