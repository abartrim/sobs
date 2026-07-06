package main

// Core infrastructure helpers shared by all ported sections.
// See CONVENTIONS.md — this file owns: logger, route table, JSON responses,
// template rendering, request-body helpers, flash messages, time formatting,
// and the shared HTTP client.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nikolalohinski/gonja/v2"
	"github.com/nikolalohinski/gonja/v2/config"
	"github.com/nikolalohinski/gonja/v2/exec"
	"github.com/nikolalohinski/gonja/v2/loaders"
	"github.com/nikolalohinski/gonja/v2/nodes"
	"github.com/nikolalohinski/gonja/v2/parser"
	"github.com/nikolalohinski/gonja/v2/tokens"
)

// logger mirrors logging.getLogger("sobs").
var logger = slog.Default().With("logger", "sobs")

// ---------------------------------------------------------------------------
// Route table — sections register routes from init(); main builds the mux.
// ---------------------------------------------------------------------------

type routeEntry struct {
	Method  string
	Pattern string
	Handler http.HandlerFunc
}

var (
	registeredRoutes  []routeEntry
	registeredRouteMu sync.Mutex
	// endpointPaths maps a registered pattern to itself for url_for-style
	// lookups from templates. Flask endpoint names are not preserved in Go;
	// templates resolve url_for by literal path (see renderTemplate globals).
	endpointPaths = map[string]string{}
)

func registerRoute(method, pattern string, h http.HandlerFunc) {
	registeredRouteMu.Lock()
	defer registeredRouteMu.Unlock()
	registeredRoutes = append(registeredRoutes, routeEntry{Method: method, Pattern: pattern, Handler: h})
	endpointPaths[pattern] = pattern
}

// ---------------------------------------------------------------------------
// JSON responses
// ---------------------------------------------------------------------------

// coerceUndefinedForJson lives in s01_setup.go (port of _coerce_undefined_for_json).

func jsonResponse(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(coerceUndefinedForJson(payload, 0, 12))
	if err != nil {
		logger.Warn("jsonResponse marshal failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error": "internal serialization error"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// maskedJsonResponse mirrors masked_jsonify: apply input-masking rules to the
// payload when enabled, then serialize.
func maskedJsonResponse(w http.ResponseWriter, status int, payload any) {
	jsonResponse(w, status, maskJsonPayload(payload))
}

// jsonError mirrors _json_error.
func jsonError(w http.ResponseWriter, message string, statusCode int) {
	jsonResponse(w, statusCode, map[string]any{"error": message})
}

// readJsonBody decodes a JSON request body into a generic map. Mirrors
// `await request.get_json(silent=True)` returning ({}, error) on bad input.
func readJsonBody(r *http.Request) (map[string]any, error) {
	if r.Body == nil {
		return map[string]any{}, errors.New("empty body")
	}
	defer func() { _ = r.Body.Close() }()
	var out map[string]any
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return map[string]any{}, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Template rendering (gonja v2 over the existing Jinja templates)
// ---------------------------------------------------------------------------
//
// gonja v2 implements Jinja2 natively, so the templates are loaded and rendered
// without any source rewriting: {% call %}/caller(), `is mapping`/`is string`
// tests, dict literals, macro kwargs, etc. all work as-is.

func templatesDir() string {
	// The Go binary may run from gosobs/ or the repo root; resolve robustly.
	for _, candidate := range []string{"templates", "../templates"} {
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			abs, _ := filepath.Abs(candidate)
			return abs
		}
	}
	if exe, err := os.Executable(); err == nil {
		c := filepath.Join(filepath.Dir(exe), "templates")
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return "templates"
}

var (
	templateInitOnce sync.Once
	templateCfg      *config.Config
	templateLoader   loaders.Loader
	templateCache    = map[string]*exec.Template{}
	templateCacheMu  sync.Mutex
)

// initTemplateEngine builds the shared gonja config + loader, registers the
// custom `mask` filter, and installs the request-independent global helpers.
// Run once; safe to call from any render path.
func initTemplateEngine() {
	templateInitOnce.Do(func() {
		templateCfg = config.New()
		templateCfg.AutoEscape = true
		ldr, err := loaders.NewFileSystemLoader(templatesDir())
		if err != nil {
			logger.Error("template loader init failed", "error", err)
			ldr, _ = loaders.NewFileSystemLoader(".")
		}
		templateLoader = &blockSetLoader{inner: ldr}
		// Custom `mask` filter (redacts PII/secrets); gonja ships the rest.
		_ = gonja.DefaultEnvironment.Filters.Register("mask", filterMask)
		// Request-independent template globals (mirrors app.jinja_env.globals).
		gonja.DefaultEnvironment.Context.Set("signal_label", wrap2(signalLabel))
		gonja.DefaultEnvironment.Context.Set("signal_description", wrap2(signalDescription))
		gonja.DefaultEnvironment.Context.Set("source_label", wrap1(sourceLabel))
		// url_for is request-independent (resolves endpoint names to static paths),
		// so register it as a shared global. This makes it resolvable from
		// globally-registered component macros (e.g. render_error_accordion), which
		// execute against the shared env context and cannot see per-render data.
		gonja.DefaultEnvironment.Context.Set("url_for", urlForGlobal)
		// Jinja's {% call %}/caller() block (gonja has no such control structure).
		_ = gonja.DefaultEnvironment.ControlStructures.Register("call", callParser)
		installDictMethods()
		registerTemplateMacros()
	})
}

// installDictMethods replaces gonja's dict method set (which ships only
// keys/items) with one that also provides Python's get/values, so templates can
// use `d.get('k'[, default])`, `d.values()` on Go map[string]any values.
func installDictMethods() {
	gonja.DefaultEnvironment.Methods.Dict = exec.NewMethodSet[map[string]any](map[string]exec.Method[map[string]any]{
		"get": func(self map[string]any, selfValue *exec.Value, args *exec.VarArgs) (any, error) {
			if len(args.Args) == 0 {
				return nil, exec.ErrInvalidCall(fmt.Errorf("dict.get requires a key"))
			}
			if v, ok := self[args.Args[0].String()]; ok {
				return v, nil
			}
			if len(args.Args) > 1 {
				return args.Args[1].Interface(), nil
			}
			return nil, nil
		},
		"keys": func(self map[string]any, selfValue *exec.Value, args *exec.VarArgs) (any, error) {
			return sortedMapKeys(self), nil
		},
		"values": func(self map[string]any, selfValue *exec.Value, args *exec.VarArgs) (any, error) {
			keys := sortedMapKeys(self)
			vals := make([]any, 0, len(keys))
			for _, k := range keys {
				vals = append(vals, self[k])
			}
			return vals, nil
		},
		"items": func(self map[string]any, selfValue *exec.Value, args *exec.VarArgs) (any, error) {
			keys := sortedMapKeys(self)
			items := make([]any, 0, len(keys))
			for _, k := range keys {
				items = append(items, []any{k, self[k]})
			}
			return items, nil
		},
	})
}

func sortedMapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// registerTemplateMacros loads every template and installs its {% macro %}s as
// template globals.
//
// PORT-NOTE: gonja executes only the top parent of an extending template, so a
// child's top-level `{% from "_x.html" import m %}` and its own top-level
// `{% macro m() %}` never run, leaving `m` undefined. Registering all macros
// globally makes them callable from any template (matching how Flask/Jinja
// effectively treat these shared/component macros).
func registerTemplateMacros() {
	entries, err := os.ReadDir(templatesDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".html") {
			continue
		}
		tpl, err := exec.NewTemplate(name, templateCfg, templateLoader, gonja.DefaultEnvironment)
		if err != nil {
			logger.Debug("skip macros from template", "template", name, "error", err)
			continue
		}
		macros := tpl.Macros()
		if len(macros) == 0 {
			continue
		}
		renderer := exec.NewRenderer(gonja.DefaultEnvironment, io.Discard, templateCfg, templateLoader, tpl)
		for mname, mnode := range macros {
			fn, ferr := exec.MacroNodeToFunc(mnode, renderer)
			if ferr != nil {
				continue
			}
			gonja.DefaultEnvironment.Context.Set(mname, fn)
		}
	}
}

// ---------------------------------------------------------------------------
// {% call macro(args) %}BODY{% endcall %} — Jinja's call/caller block. gonja
// has no `call` control structure, so we provide one. The body is exposed to
// the invoked macro as caller(); installing caller on the shared environment
// context lets globally-registered macros resolve it.
// ---------------------------------------------------------------------------

type callControlStructure struct {
	location     *tokens.Token
	call         nodes.Expression
	body         *nodes.Wrapper
	callerParams []string
}

func (c *callControlStructure) Position() *tokens.Token { return c.location }

func (c *callControlStructure) String() string {
	t := c.Position()
	return fmt.Sprintf("CallControlStructure(Line=%d Col=%d)", t.Line, t.Col)
}

func (c *callControlStructure) Execute(r *exec.Renderer, tag *nodes.ControlStructureBlock) error {
	caller := exec.Macro(func(params *exec.VarArgs) *exec.Value {
		var out strings.Builder
		sub := r.Inherit()
		sub.Output = &out
		// Bind {% call(a, b) %} caller parameters from the caller() invocation.
		for i, name := range c.callerParams {
			if i < len(params.Args) {
				sub.Environment.Context.Set(name, params.Args[i].Interface())
			} else {
				sub.Environment.Context.Set(name, nil)
			}
		}
		if err := sub.ExecuteWrapper(c.body); err != nil {
			return exec.AsValue(err)
		}
		return exec.AsSafeValue(out.String())
	})
	// PORT-NOTE: caller is installed on the shared env context so global macros
	// resolve it. Save/restore makes nested {% call %} correct within a render;
	// concurrent renders across requests could still race (acceptable here).
	ctx := gonja.DefaultEnvironment.Context
	prev, had := ctx.Get("caller")
	ctx.Set("caller", caller)
	value := r.Eval(c.call)
	if had {
		ctx.Set("caller", prev)
	} else {
		ctx.Set("caller", nil)
	}
	if value.IsError() {
		return fmt.Errorf("unable to execute call: %s", value.String())
	}
	_, err := io.WriteString(r.Output, value.String())
	return err
}

func callParser(p *parser.Parser, args *parser.Parser) (nodes.ControlStructure, error) {
	cs := &callControlStructure{location: p.Current()}
	// Optional caller parameter list: {% call(a, b) macro(...) %}.
	if args.Match(tokens.LeftParenthesis) != nil {
		for args.Match(tokens.RightParenthesis) == nil {
			name := args.Match(tokens.Name)
			if name == nil {
				return nil, args.Error("Expected caller parameter name.", args.Current())
			}
			cs.callerParams = append(cs.callerParams, name.Val)
			if args.Match(tokens.RightParenthesis) != nil {
				break
			}
			if args.Match(tokens.Comma) == nil {
				return nil, args.Error("Expected ',' or ')'.", args.Current())
			}
		}
	}
	expr, err := args.ParseExpression()
	if err != nil {
		return nil, err
	}
	cs.call = expr
	if !args.End() {
		return nil, args.Error("Malformed call: expected a single macro invocation.", args.Current())
	}
	wrapper, _, err := p.WrapUntil("endcall")
	if err != nil {
		return nil, err
	}
	cs.body = wrapper
	return cs, nil
}

// blockSetRe matches Jinja's block-assignment `{% set NAME %}...{% endset %}`.
// gonja v2's `set` control structure only supports `{% set NAME = expr %}`, so
// we rewrite the block form into a macro whose rendered output is assigned.
var blockSetRe = regexp.MustCompile(`(?s)\{%-?\s*set\s+([A-Za-z_]\w*)\s*-?%\}(.*?)\{%-?\s*endset\s*-?%\}`)

// rewriteBlockSet converts `{% set NAME %}BODY{% endset %}` into
// `{% macro __bs_NAME() %}BODY{% endmacro %}{% set NAME = __bs_NAME() %}`.
func rewriteBlockSet(src string) string {
	return blockSetRe.ReplaceAllStringFunc(src, func(match string) string {
		m := blockSetRe.FindStringSubmatch(match)
		name, body := m[1], m[2]
		return "{% macro __bs_" + name + "() %}" + body + "{% endmacro %}" +
			"{% set " + name + " = __bs_" + name + "() %}"
	})
}

// blockSetLoader wraps a gonja loader and rewrites block-set assignments (which
// gonja cannot parse) before compilation. All other Jinja constructs pass
// through to gonja unchanged.
type blockSetLoader struct{ inner loaders.Loader }

func (l *blockSetLoader) Read(path string) (io.Reader, error) {
	rdr, err := l.inner.Read(path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(rdr)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(rewriteBlockSet(string(data))), nil
}

func (l *blockSetLoader) Resolve(path string) (string, error) { return l.inner.Resolve(path) }

func (l *blockSetLoader) Inherit(from string) (loaders.Loader, error) {
	sub, err := l.inner.Inherit(from)
	if err != nil {
		return nil, err
	}
	return &blockSetLoader{inner: sub}, nil
}

// getCachedTemplate compiles (or returns the cached) template by name.
func getCachedTemplate(name string) (*exec.Template, error) {
	initTemplateEngine()
	templateCacheMu.Lock()
	defer templateCacheMu.Unlock()
	if tpl, ok := templateCache[name]; ok {
		return tpl, nil
	}
	tpl, err := exec.NewTemplate(name, templateCfg, templateLoader, gonja.DefaultEnvironment)
	if err != nil {
		return nil, err
	}
	templateCache[name] = tpl
	return tpl, nil
}

// filterMask mirrors the Python `mask` Jinja filter (_mask_value_for_output)
// with db=None.
func filterMask(e *exec.Evaluator, in *exec.Value, params *exec.VarArgs) *exec.Value {
	return exec.AsValue(maskValueForOutput(in.Interface(), nil))
}

// wrap2 adapts a two-string-argument Go function into a gonja global callable.
func wrap2(fn func(string, string) string) func(*exec.VarArgs) *exec.Value {
	return func(va *exec.VarArgs) *exec.Value {
		a, b := "", ""
		if len(va.Args) > 0 {
			a = va.Args[0].String()
		}
		if len(va.Args) > 1 {
			b = va.Args[1].String()
		}
		return exec.AsValue(fn(a, b))
	}
}

// wrap1 adapts a one-string-argument Go function into a gonja global callable.
func wrap1(fn func(string) string) func(*exec.VarArgs) *exec.Value {
	return func(va *exec.VarArgs) *exec.Value {
		a := ""
		if len(va.Args) > 0 {
			a = va.Args[0].String()
		}
		return exec.AsValue(fn(a))
	}
}

// urlForGlobal provides the url_for shim for templates (supports kwargs natively
// via VarArgs). It resolves Flask endpoint NAMES to their real registered paths
// via flaskRoutePatterns (see s00c_routes.go), substituting path parameters and
// appending the remaining keyword arguments as a query string — matching Flask's
// url_for(endpoint, **values).
func urlForGlobal(va *exec.VarArgs) *exec.Value {
	endpoint := ""
	if len(va.Args) > 0 {
		endpoint = va.Args[0].String()
	}
	kv := map[string]string{}
	for k, v := range va.KwArgs {
		kv[k] = v.String()
	}
	return exec.AsValue(resolveUrlFor(endpoint, kv))
}

// renderTemplate mirrors quart.render_template + writes the response.
// Globals injected on every render mirror Quart context processors
// (injectFeatureFlags from s03) plus url_for and get_flashed_messages.
func renderTemplate(w http.ResponseWriter, r *http.Request, name string, ctx map[string]any) {
	data := map[string]any{}
	for k, v := range injectFeatureFlags() {
		data[k] = v
	}
	data["url_for"] = urlForGlobal
	// get_flashed_messages depends on this request/response, so it is built per
	// render rather than installed on the shared env.
	data["get_flashed_messages"] = func(va *exec.VarArgs) *exec.Value {
		withCategories := va.GetKeywordArgument("with_categories", false).Bool()
		msgs := popFlashedMessages(w, r)
		if withCategories {
			pairs := make([]any, 0, len(msgs))
			for _, m := range msgs {
				pairs = append(pairs, []any{m["category"], m["message"]})
			}
			return exec.AsValue(pairs)
		}
		out := make([]any, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, m["message"])
		}
		return exec.AsValue(out)
	}
	cookies := map[string]string{}
	for _, c := range r.Cookies() {
		cookies[c.Name] = c.Value
	}
	data["request"] = map[string]any{
		"path":    r.URL.Path,
		"args":    r.URL.Query(),
		"cookies": cookies,
	}
	// PORT-NOTE: Flask's `config` object. The Go port exposes it as an empty map
	// so `config.get('KEY', default)` / `config['KEY']|default(...)` fall through.
	data["config"] = map[string]any{}
	for k, v := range ctx {
		data[k] = v
	}
	tpl, err := getCachedTemplate(name)
	if err != nil {
		es := err.Error()
		if len(es) > 500 {
			es = es[len(es)-500:]
		}
		logger.Error("template load failed", "template", name, "error_tail", es)
		http.Error(w, "template error: "+name, http.StatusInternalServerError)
		return
	}
	// PORT-NOTE: render fully to a buffer before writing the response. This makes
	// a real render error a clean 500 (instead of partial streamed output), and a
	// client disconnect during the final write is a harmless ignored Write error
	// rather than a logged template-execution failure (e.g. "broken pipe"). The
	// recover guards against panics inside the template engine so one bad value
	// yields a clean 500 instead of a raw stack trace.
	out, err := func() (b []byte, e error) {
		defer func() {
			if rec := recover(); rec != nil {
				e = fmt.Errorf("template panic: %v", rec)
			}
		}()
		return tpl.ExecuteToBytes(exec.NewContext(data))
	}()
	if err != nil {
		es := err.Error()
		if len(es) > 500 {
			es = es[len(es)-500:]
		}
		logger.Error("template render failed", "template", name, "error_tail", es)
		http.Error(w, "template error: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(out)
}

// ---------------------------------------------------------------------------
// Flash messages (cookie-based stand-in for Quart's session flash)
// ---------------------------------------------------------------------------

const flashCookieName = "sobs_flash"

type flashEntry struct {
	Message  string `json:"message"`
	Category string `json:"category"`
}

// PORT-NOTE: Quart stores flashes in the signed session cookie. The Go port
// uses an unsigned JSON cookie; flashes are display-only hints, not trusted data.
func flashMessage(w http.ResponseWriter, r *http.Request, message, category string) {
	var entries []flashEntry
	if c, err := r.Cookie(flashCookieName); err == nil {
		if raw, err := base64.URLEncoding.DecodeString(c.Value); err == nil {
			_ = json.Unmarshal(raw, &entries)
		}
	}
	entries = append(entries, flashEntry{Message: message, Category: category})
	raw, _ := json.Marshal(entries)
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Value:    base64.URLEncoding.EncodeToString(raw),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func popFlashedMessages(w http.ResponseWriter, r *http.Request) []map[string]string {
	c, err := r.Cookie(flashCookieName)
	if err != nil {
		return nil
	}
	var entries []flashEntry
	if raw, err := base64.URLEncoding.DecodeString(c.Value); err == nil {
		_ = json.Unmarshal(raw, &entries)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	out := make([]map[string]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]string{"message": e.Message, "category": e.Category})
	}
	return out
}

// ---------------------------------------------------------------------------
// Time formatting
// ---------------------------------------------------------------------------

// pyIsoFormat mirrors datetime.isoformat() for an aware UTC datetime:
// microsecond precision when non-zero, "+00:00" offset.
func pyIsoFormat(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05") + "+00:00"
	}
	return t.Format("2006-01-02T15:04:05.000000") + "+00:00"
}

// ---------------------------------------------------------------------------
// Shared HTTP client (httpx equivalent)
// ---------------------------------------------------------------------------

var httpClient = &http.Client{Timeout: 30 * time.Second}
