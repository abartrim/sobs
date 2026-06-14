package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/render"
)

// mobileBreakpointMax mirrors app.py MOBILE_BREAKPOINT_MAX.
const mobileBreakpointMax = "575.98px"

// sourceLabels mirrors app.py _SOURCE_LABELS (signal source -> display name).
var sourceLabels = map[string]string{
	"logs": "Logs", "traces": "Traces", "errors": "Errors",
	"rum_vitals": "RUM Vitals", "metrics": "Metrics",
}

// signalLabels is the parsed _SIGNAL_LABELS ("source|signal" -> {label, description}).
var signalLabels = func() map[string]map[string]string {
	m := map[string]map[string]string{}
	_ = json.Unmarshal(signalLabelsJSON, &m)
	return m
}()

// pyTitle approximates Python str.title() for ASCII identifier fallbacks (capitalize the
// first letter of each space-separated word).
func pyTitle(s string) string {
	return strings.Title(strings.ToLower(s)) //nolint:staticcheck
}

// newEngine builds the template engine with the globals the templates call.
func (s *server) newEngine() *render.Engine { return s.newEngineFlash(nil) }

// newEngineFlash is newEngine with pre-seeded flash messages (each []any{category, message})
// that get_flashed_messages consumes — for handlers that flash() then render a page (rather
// than redirect), e.g. the auto-rule preview pages.
func (s *server) newEngineFlash(flashes []any) *render.Engine {
	e := render.New(s.cfg.TemplateDir)
	e.AddFunc("url_for", func(pos []any, kw map[string]any) (any, error) {
		return s.urlFor(pos, kw, e.KWOrder())
	})
	// fmt_bytes (app.py _fmt_bytes): human-readable byte count, passed to the DM-stats page.
	e.AddFunc("fmt_bytes", func(pos []any, kw map[string]any) (any, error) {
		return fmtBytes(pos), nil
	})
	e.AddFunc("get_flashed_messages", func(pos []any, kw map[string]any) (any, error) {
		if withCat, ok := kw["with_categories"]; ok && truthy(withCat) {
			return flashes, nil // [[category, message], ...]
		}
		msgs := make([]any, 0, len(flashes))
		for _, f := range flashes {
			if pair, ok := f.([]any); ok && len(pair) == 2 {
				msgs = append(msgs, pair[1])
			}
		}
		return msgs, nil
	})
	// Label globals (app.jinja_env.globals): source_label, signal_label, signal_description.
	e.AddFunc("source_label", func(pos []any, kw map[string]any) (any, error) {
		src := argStr(pos, 0)
		if v, ok := sourceLabels[src]; ok {
			return v, nil
		}
		return pyTitle(strings.ReplaceAll(src, "_", " ")), nil
	})
	e.AddFunc("signal_label", func(pos []any, kw map[string]any) (any, error) {
		src, sig := argStr(pos, 0), argStr(pos, 1)
		if entry, ok := signalLabels[src+"|"+sig]; ok {
			return entry["label"], nil
		}
		return pyTitle(strings.ReplaceAll(sig, "_", " ")), nil
	})
	e.AddFunc("signal_description", func(pos []any, kw map[string]any) (any, error) {
		src, sig := argStr(pos, 0), argStr(pos, 1)
		if entry, ok := signalLabels[src+"|"+sig]; ok {
			return entry["description"], nil
		}
		return "", nil
	})
	return e
}

// argStr returns positional arg i as a string, or "".
func argStr(pos []any, i int) string {
	if i < len(pos) {
		if s, ok := pos[i].(string); ok {
			return s
		}
		return fmt.Sprintf("%v", pos[i])
	}
	return ""
}

// urlFor mirrors Quart's url_for for the cases the templates use: a named endpoint
// (optionally with path params) and the special 'static' endpoint with a filename.
func (s *server) urlFor(pos []any, kw map[string]any, kwOrder []string) (any, error) {
	if len(pos) == 0 {
		return "", fmt.Errorf("url_for requires an endpoint")
	}
	endpoint, _ := pos[0].(string)
	if endpoint == "static" {
		fn, _ := kw["filename"].(string)
		return s.cfg.BasePath + "/static/" + fn, nil
	}
	rule, ok := endpointPaths[endpoint]
	if !ok {
		return "", fmt.Errorf("url_for: unknown endpoint %q", endpoint)
	}
	// Path-param kwargs are substituted into the rule; the rest become query params, in
	// the kwargs INSERTION order (Quart/Werkzeug preserves it — not sorted).
	var qs []string
	for _, k := range kwOrder {
		v := fmt.Sprintf("%v", kw[k])
		if pathHasParam(rule, k) {
			rule = replaceParam(rule, k, v)
		} else {
			qs = append(qs, url.QueryEscape(k)+"="+strings.ReplaceAll(url.QueryEscape(v), "%3A", ":"))
		}
	}
	out := s.cfg.BasePath + rule
	if len(qs) > 0 {
		out += "?" + strings.Join(qs, "&")
	}
	return out, nil
}

// pathHasParam reports whether a rule contains the named path param (<name> or <conv:name>).
func pathHasParam(rule, name string) bool {
	for _, t := range []string{"<" + name + ">", "<int:" + name + ">", "<path:" + name + ">", "<string:" + name + ">"} {
		if strings.Contains(rule, t) {
			return true
		}
	}
	return false
}

func replaceParam(rule, name, value string) string {
	// matches <name> or <converter:name>
	out := rule
	for _, token := range []string{"<" + name + ">", "<int:" + name + ">", "<path:" + name + ">", "<string:" + name + ">"} {
		out = strings.ReplaceAll(out, token, value)
	}
	return out
}

// baseContext returns the context processor values injected into every render
// (app.py inject_feature_flags) plus the per-request `request` and `config` globals the
// templates reference. `endpoint` is the Quart endpoint name for the active route.
func (s *server) baseContext(endpoint string) map[string]any {
	return map[string]any{
		"query_enabled":      s.cfg.QueryPageEnabled,
		"kubernetes_enabled": s.cfg.KubernetesEnabled,
		// not _is_output_masking_enabled() — masking.output_enabled defaults on, so False.
		"raise_issue_mask_toggle_effective": !s.appSettingBool("masking.output_enabled", true),
		"mobile_breakpoint_max":             mobileBreakpointMax,
		"sobs_version":                      s.cfg.BuildVersion,
		// request.args is empty for the param-less page captures; macros call
		// request.args.get(...) which falls back to defaults on an empty map.
		"request": map[string]any{
			"endpoint": endpoint, "args": map[string]any{}, "cookies": map[string]any{},
		},
		"config": map[string]any{
			"ENABLE_FIRST_RUN_TOUR": s.cfg.FirstRunTourEnabled,
		},
	}
}

// handleHelpPage renders a *_help.html template (the dynamically-registered help routes).
// endpoint is the Quart endpoint name (used by base.html nav-active logic).
func (s *server) handleHelpPage(endpoint, templateName string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		eng := s.newEngine()
		out, err := eng.Render(templateName, s.baseContext(endpoint))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body := []byte(out)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
