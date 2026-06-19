# Populated-render divergences found by corpus expansion

These Go bugs were latent because the original golden corpus captured every page with **empty**
data — so the populated rendering branches (counts, filters, querystrings, status badges) were
never byte-compared. Seeding `view_logs` (`logsview` profile) and the `view_traces` list path
(reusing the `tracedetail` seed) exposed five genuine divergences. Each fix only makes Go match the
Python/Quart/Werkzeug/Jinja2 oracle — it can never regress empty-corpus parity.

| # | Symptom (golden vs Go) | Root cause | Fix |
|---|---|---|---|
| R1 | `level_stats`/`service_stats` rendered `ERROR: 3.0` not `ERROR: 3` | `COUNT()` arrives as float64; Python keeps it an int | `cInt(m,"cnt")` in `computeLogStats` (`handlers_pages_logs_errors.go`) |
| R2 | `url_for` querystring over-encoded: `sql=has_tag%28%27env%27%2C%27prod%27%29` vs `has_tag('env','prod')` | Go `url.QueryEscape` only leaves `A-Za-z0-9-_.~` literal; Werkzeug 3.1.8 `_urlencode` (quote_plus) also leaves `! $ ' ( ) * , / : ; ? @` literal | `werkzeugQueryEscape` relaxes those 12 codes (`render.go`) |
| R3 | filter pages dropped the raw `from_ts`/`to_ts` input value (`value=""` on a time error) | `request.args` was hardcoded to an empty map in `baseContext` | `renderPageReq` populates `request.args` from `r.URL.Query()` (`handlers_pages.go`); `view_logs` switched to it |
| R4 | HTTP status badge wrong: `404` got `bg-success` not `bg-warning` (`span.http_status\|string >= '400'`) | `compareOrd` only compared numbers; any non-numeric operand returned `false` | lexicographic string fallback in `compareOrd` (`eval.go`) |
| R5 | `view_traces` list path 500'd: `unsupported filter "string"` | Jinja `\|string` (soft_str) filter not implemented | added `case "string": return pyStr(val)` (`eval.go`) |
| R6 | `url_for(... grouped=_g ...)` rendered `grouped=%3Cnil%3E` vs Python omitting it (where `{% set _g = '1' if grouped_mode else none %}`) | Go `urlFor` rendered `fmt.Sprintf("%v", nil)` = `<nil>`; Werkzeug omits None-valued query params (empty string is kept as `k=`) | skip `kw[k] == nil` in `urlFor` (`render.go`) — found by the `errorsview` batch |
| R7 | incident window `from_ts`/`to_ts` rendered `2023-06-01 11:45:00 +0000 UTC` vs Python `2023-06-01 11:45:00.000000` | `normalizeCHTimestamp(v any)` was called with a `time.Time` (`dt.Add(-half)`); `toStr(time.Time)` yields Go's `String()` which no layout parses → fell through to the raw value. Python `_normalize_ch_timestamp` handles `datetime` directly | add a `time.Time` branch → `t.UTC().Format("2006-01-02 15:04:05.000000")` (`mutation_helpers.go`) — found by the `view_incident` batch |
| R8 | `view_ai` rows rendered empty model/tokens/provider (`data-model=""`, tokens 0) for spans whose attrs were present in the DB | `attrMap(cStr(r, "SpanAttributes"))` — `cStr` `fmt.Sprintf`s the chdb map into an unparseable `"map[...]"` string → `mapToDict` returns `{}`. Working paths pass the RAW value | `attrMap(r["SpanAttributes"])` (`handlers_pages.go`) — found by the `aiview` batch |
| R9 | `view_ai` `ai_pricing_json` omitted observed models (e.g. `claude-opus` inferred entry) | view_ai rendered a STATIC embedded `savedAiPricingJSON` blob; Python `_load_ai_pricing_with_sources` dynamically merges defaults + observed-model inference + saved/confirmed | call `s.loadAiPricingWithSources()` (already implemented for save_ai_settings) in view_ai (`handlers_pages.go`) — found by the `aiview` batch |
| R10 | `view_metrics` + `view_metrics_anomaly` rendered `Sample` `5.0` / `point_count` as floats vs Python ints | `last_sample_count`/`point_count`/`sample_count` are UInts (`argMax(SampleCount,…)`/`count()`) passed raw (float64 from chdb); Python renders the int. `value`/`baseline_*`/`anomaly_score` are genuine Float64 → kept raw | `cInt(m, …)` for the count columns in both handlers (`handlers_pages.go`) — found by the `metricsauto` batches (same class as R1) |
| R11 | `save_ai_settings` returned 302 (success) on invalid `model_pricing`/`model_pricing_confirmed` JSON; Python returns 400 `{"error": ...}` | the Go POST handler was a simplified port — it wrote the basic ai.* keys but omitted the pricing/confirmed JSON validation+normalize and the github_token expiry/validation-reset logic | ported the validation (parse → 400 on bad JSON, normalize entries, save) + github_token branch (`handlers_pages.go`) — found by the `aisettings` batch |

**Werkzeug query-encoding reference** (probed from the frozen oracle, WZ 3.1.8): input
`a b!"#$%&'()*+,-./:;<=>?@[\]^_`+"`"+`{|}~` → `a+b!%22%23$%25%26'()*%2B,-./:;%3C%3D%3E?@%5B%5C%5D%5E_%60%7B%7C%7D~`.
Literal: `! $ ' ( ) * , - . / : ; ? @ _ ~` + alnum; space→`+`; escaped: `" # % & + < = > [ \ ] ^ ` + "`` ` `` + `{ | }`.

**Blast radius:** R2/R3/R4/R5 are shared render-layer fixes (every page that builds a `url_for`
querystring with sub-delims, reads `request.args`, does an ordinal string compare, or uses
`|string`). The full Docker parity suite was re-run after the fixes to confirm no regression on the
existing corpus.
