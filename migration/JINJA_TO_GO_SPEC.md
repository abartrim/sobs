# Jinja2 → Go `text/template` Translation Spec

This is the hardest, most failure-prone part of the migration and the reason byte
parity is non-trivial. Read it fully before touching a template. Every rule here is
verifiable against the golden corpus; when a rule and a golden disagree, the **golden
wins** and you update this doc.

> **Decision: use `text/template`, not `html/template`.** Go's `html/template` does
> context-aware escaping (different inside `<script>`, `<style>`, URLs, attributes,
> HTML text) that *cannot* be made byte-identical to Jinja2. We use `text/template`
> (which escapes nothing) and **re-implement Jinja's escaping as explicit filters**,
> applied exactly where Jinja applies them. This trades "automatic safety" for
> "byte-exact reproduction" — which is the goal. Security review of the output is a
> non-issue because the output is provably identical to the already-shipped Python.

## 0. Strategy: do NOT hand-translate 75 templates

Hand-rewriting templates is how prior attempts drowned. Instead:

1. **Reuse the template *files* verbatim where the syntax overlaps**, via a
   **transpiler** (`go/internal/render/transpile`) that mechanically rewrites Jinja
   constructs into `text/template` constructs at build/load time. Jinja and Go
   templates are close enough that a focused transpiler (a few hundred lines) covers
   the constructs this codebase actually uses (catalogued in `AUDIT.md` §5). The
   transpiler is itself unit-tested against snippets with known expected Go-template
   output, but the **real** test is the golden corpus.
2. Where a construct is too divergent to transpile cleanly (complex `{% call %}`
   macros), special-case those specific macros as Go functions.

This keeps the templates as a single source of truth (still the Jinja files on disk),
shrinks the surface to "make the transpiler + filters correct," and lets the golden
corpus validate the whole set at once.

> If the transpiler proves too leaky for the 8 macros / `caller()` cases, fall back to
> a one-time mechanical conversion of just those partials into Go-native templates,
> committed under `go/templates/`, while the page templates stay transpiled. Decide
> based on what the golden diffs actually show — don't pre-optimize.

## 1. Construct mapping

| Jinja2 | Go `text/template` | Notes |
|---|---|---|
| `{{ x }}` | `{{ x \| e }}` | Jinja autoescapes by default. Pipe through the `e` (HTML-escape) filter unless the value is `\|safe`. See §2. |
| `{{ x \| safe }}` | `{{ x }}` | No escaping. |
| `{{ x \| tojson }}` | `{{ x \| tojson }}` | Port `tojson` exactly (§3). |
| `{% if a %}…{% elif b %}…{% else %}…{% endif %}` | `{{ if a }}…{{ else if b }}…{{ else }}…{{ end }}` | Jinja truthiness ≠ Go truthiness — see §4. |
| `{% for x in xs %}…{% endfor %}` | `{{ range $i, $x := xs }}…{{ end }}` | `loop.*` vars → see §5. |
| `{% set y = expr %}` | `{{ $y := expr }}` (or precompute in Go data) | Jinja `set` is scoped to the block; Go `$y` is template-scoped. Complex `set` expressions → compute in the handler and pass in. |
| `{% block name %}…{% endblock %}` | `{{ block "name" . }}…{{ end }}` + define overrides | Inheritance — §6. |
| `{% extends "base.html" %}` | (handled by loader/transpiler) | §6. |
| `{% include "_x.html" %}` | `{{ template "_x.html" . }}` | Ensure the included template is parsed into the same set. |
| `{% from "_m.html" import render_page_header %}` | call a registered Go template `{{ template "render_page_header" (dict ...) }}` or a func | Macros — §7. |
| `{% macro m(a, b=1) %}` | `{{ define "m" }}` + a `dict` arg, defaults applied in a wrapper func | §7. |
| `{% with x = y %}…{% endwith %}` | `{{ with ... }}` or precompute | 1 use only (base.html flash). |
| `{# comment #}` | `{{/* comment */}}` (stripped) | Comments don't reach output in either; drop them. |
| `{%- … -%}` whitespace trim | `{{- … -}}` | Map trim markers 1:1 — §8. |

## 2. Autoescaping (`e` filter)

Jinja's default autoescape (MarkupSafe `escape`) replaces, in this order:
```
&  → &amp;
<  → &lt;
>  → &gt;
"  → &#34;
'  → &#39;
```
Note: `"` → `&#34;` and `'` → `&#39;` (numeric, **not** `&quot;`/`&apos;`). Go's
`html.EscapeString` produces `&#34;` and `&#39;` too **but** also differs on some
edges — implement the `e` filter to match MarkupSafe *exactly* and unit-test it on:
`< > & " '`, already-escaped entities (Jinja double-escapes `&amp;`→`&amp;amp;` unless
the value is `Markup`), and non-ASCII (left as raw UTF-8 bytes, not escaped). The
transpiler inserts `| e` on every `{{ ... }}` that Jinja would have autoescaped (i.e.
all of them except `|safe`, `|tojson`-into-script, and Markup-returning globals/macros).

**`Markup`/`|safe` propagation:** values that are MarkupSafe `Markup` in Python (e.g.
macro outputs, `meta_html`, `actions_html`, `title_html`, the custom globals if they
return Markup) are *not* re-escaped. The transpiler must know which expressions are
safe. Maintain an allow-list in `transpile/safe_exprs.go` seeded from the `|safe`
sites and macro-return analysis; expand it when a golden diff shows over-escaping.

## 3. `tojson` (the #1 hazard — 142 uses, ~150 in `<script>`)

Jinja `tojson` = HTML-safe compact JSON. Reproduce precisely:
1. Serialize with Python-`json.dumps` semantics: **compact separators `(",", ":")`**,
   and the `ensure_ascii` setting Quart's policy uses — **verify from a golden**
   (capture a route with a unicode value and read the bytes). Implement in
   `go/internal/jsonenc` with an explicit option struct, **not** `encoding/json`
   defaults.
2. Then escape, on the resulting string:
   ```
   <  → <
   >  → >
   &  → &
   '  → '
   ```
   (This is what makes it safe to drop inside `<script>` and HTML.) Go's
   `encoding/json` already escapes `<`,`>`,`&` to `<…` **but not `'`**, and only
   when `SetEscapeHTML(true)`. Do the escaping yourself after a non-HTML-escaping
   encode so you control all four exactly.
3. Key order: preserve **insertion order** of the source dict (Python 3.7+). In Go,
   never feed a `map[string]any` to the encoder (Go randomizes/sorts map order) —
   pass ordered structures (a `[]KV`, an `*orderedmap`, or a struct whose field order
   matches). The handler is responsible for ordering the data it passes to the
   template.

Unit-test `tojson` on: nested objects, arrays, strings containing `</script>`,
quotes, unicode, `null`/`true`/`false`, integers vs floats (Python `1.0`→`1.0`,
`1`→`1`; Go must not turn an int into `1` where Python had `1.0` or vice-versa — match
the Python type at the source).

## 4. Truthiness (`if`)

Jinja and Go differ:
- Jinja false-y: `False`, `None`, `0`, `0.0`, `""`, `[]`, `{}`, `()`.
- Go `text/template` false-y: `false`, `0`, `""`, `nil`, and **empty arrays/slices/
  maps** (len 0). Numeric `0.0` and empty containers align; `None`→`nil` aligns.
- **Gotcha:** Jinja `{% if x %}` where `x` is the string `"false"` is *truthy*
  (non-empty string). Go agrees. But `{% if x is defined %}` / `{% if x is none %}` /
  `{% if x is mapping %}` have no Go equivalent — the transpiler must rewrite these:
  `is defined` → a `has` helper, `is none` → `eq x nil`, `is mapping` → an `isMap`
  func. Catalog every `is <test>` use (`grep -n " is "` the templates) and map each.
- Comparisons (`==`, `!=`, `<`, `in`) → Go `eq`/`ne`/`lt`/a `contains` func. Jinja
  `in` works on strings, lists, dicts — provide a polymorphic `in` func.

## 5. Loop variables

Jinja `loop.*` inside `{% for %}` must be synthesized in Go (no built-in):
| Jinja | Go |
|---|---|
| `loop.index` | `{{ add $i 1 }}` |
| `loop.index0` | `$i` |
| `loop.first` | `{{ eq $i 0 }}` |
| `loop.last` | `{{ eq $i (sub (len $xs) 1) }}` |
| `loop.length` | `{{ len $xs }}` |
| `loop.revindex` | `{{ sub (len $xs) $i }}` |
Used: `loop.index` (28×), `loop.first` (7×), `loop.last` (2×). The transpiler emits the
range with `$i, $x :=` and rewrites `loop.*` references. Provide `add`/`sub`/`len`
funcs.

## 6. Inheritance (`extends` / `block`)

Jinja child `{% extends "base.html" %}` + `{% block content %}…{% endblock %}` maps to
Go's `{{ block }}`/`define` *associated template* model:
- Parse `base.html` and each child into **one** `*template.Template` set per page.
- Base declares `{{ block "title" . }}default{{ end }}`, `{{ block "styles" . }}{{ end }}`,
  `{{ block "content" . }}{{ end }}`, `{{ block "scripts" . }}{{ end }}`.
- Each child `{{ define "content" }}…{{ end }}` (etc.) overrides. Render the **base**
  as the entrypoint with the child's defines associated.
- `{{ super() }}` (Jinja, ~10 sites in `styles`) has no native Go equivalent — the
  transpiler renames the base block body into a callable `{{ template "styles__base" . }}`
  and the child invokes it where `super()` appeared.

The loader (`go/internal/render/loader.go`) builds, for each page template, the set
`{base, the page, all imported partials/macros}` so names resolve.

## 7. Macros (`caller()` is the sharp edge)

- **Simple macros** (`render_page_header`, `render_regex_filter`,
  `render_error_panel_styles`, `render_tz_init_script`, `render_single_select`):
  transpile to `{{ define "macroname" }}` taking a single `dict` argument; a thin Go
  wrapper applies parameter **defaults** before invocation. Call sites
  `{{ render_page_header(title="X") }}` → `{{ template "render_page_header" (dict "title" "X") }}`.
- **`caller()` macros** (`render_filter_accordion`, `render_error_accordion`,
  `render_multi_select`): Go templates can't pass a body block as a callable. Two
  options, pick per-macro from golden diffs:
  1. Pre-render the caller body into a `Markup` string in the handler and pass it as a
     `body` parameter the macro inserts with `{{ .body }}` (no re-escape). Works when
     the body doesn't depend on macro-local vars.
  2. Port the macro to a Go function in `go/internal/render/macros.go` that takes the
     params + a `func() string` body and returns `Markup`. Register it as a template
     func.
- **`render_tz_init_script`** emits **raw JavaScript** (not HTML) — output is `|safe`;
  must be byte-identical including indentation. Port carefully; it's a parity trap.

## 8. Whitespace (sparse but lethal for byte parity)

- No `trim_blocks`/`lstrip_blocks` are set in the Python env, so Jinja's default is:
  a block tag (`{% … %}`) does **not** strip surrounding whitespace, and `{{ … }}`
  doesn't either. Go `text/template` default is the same (no stripping). So **most**
  whitespace maps 1:1 — good.
- The explicit trim markers (`{%- …`, `… -%}`, 30 total) map to Go `{{- …`, `… -}}`.
  But Go's trim trims **all** adjacent whitespace including newlines; Jinja's `-`
  trims whitespace up to and including one newline boundary the same way — **verify
  per-site against goldens** with `parity_check.py --bisect-body`; whitespace diffs are
  the most common first-failure and the bisect view points at the exact byte.
- Newline-at-EOF: Jinja preserves the template file's trailing newline; ensure the Go
  loader doesn't strip or add one. Capture the golden and match.

## 9. Filters to port (in `go/internal/render/filters.go`)

Each must match its Jinja semantics byte-exactly. Unit-test against captured fragments.

| Filter | Spec |
|---|---|
| `e` / autoescape | §2 (MarkupSafe escape). |
| `tojson` | §3. |
| `safe` | identity; marks Markup (no escape). |
| `length` | `len`. |
| `truncate(n, killwords=False, end='...', leeway=5)` | **Replicate Jinja's exact algorithm** including the `leeway` (default 5) and word-boundary logic — a naive `s[:n]+"..."` will diverge. Read Jinja source for `do_truncate`. 12 uses. |
| `title` | Jinja `title` capitalizes first letter of each word; **differs** from Go `strings.Title`/`cases.Title` on apostrophes/edge cases — match Jinja. |
| `lower` / `upper` | `strings.ToLower/ToUpper` (ASCII-safe; verify unicode). |
| `round(p=0, method='common')` | Python `round` is banker's-rounding-ish via Jinja's `do_round`; match the method arg. |
| `replace(old, new, count=None)` | `strings.Replace` with count `-1` when unset. |
| `format(...)` | printf-style `%`-formatting → Go `fmt.Sprintf` with translated verbs. Audit each call. |
| `join(sep, attr=None)` | `strings.Join`; the `attr` variant maps a field first. |
| `min` / `max` | over iterables; match tie/empty behavior. |
| `urlencode` | Jinja quotes via `urllib.parse.quote`/`urlencode` — match the safe-char set exactly (`urlencode` of a dict vs a string differ). |
| `default(d, boolean=False)` | `x if x is defined/not-none else d`; the `boolean` variant uses truthiness. |
| `mask` (**custom, compliance-critical**) | Port `_mask_value_for_output` from `masking.py` *exactly*. Unit-test against `masking.py`'s own behavior on the same inputs. 10 template uses + many JSON-path uses. |

## 10. Custom globals

`signal_label`, `signal_description`, `source_label` (`app.py:13415`) are functions
callable in templates. Port to Go funcs registered on the template FuncMap; their
output is inserted (escaped or `|safe` per call site — check whether Python returns
`Markup`). Plus the context-processor values (`AUDIT.md` §3) injected into every render.

## 11. Validation workflow for templates

For each template, the loop is:
1. Transpile (automatic).
2. Run its page route through `parity_check.py --only <route_id> --bisect-body`.
3. First differing byte → identify the rule (escaping? whitespace? filter? order?).
4. Fix the **filter/transpiler/data-ordering** (not the template, not the golden).
5. Add a regression unit test in `go/internal/render` for that exact byte pattern.
6. Green → next.

Because all 75 templates share one engine + one filter set + `base.html`, fixing a
rule once typically turns several reds green at the same time. That compounding is why
this approach converges instead of sprawling.
