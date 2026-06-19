# SOBS Python→Go Function-Parity Divergence Audit (PR #304 current branch)

**Date:** 2026-06-18 · **Branch:** `claude/dreamy-tesla-c5dbdd` (includes the integration chips H1/H2/H6/H7/H8/H10/H12/H20/C6).
**Oracle:** `app.py` (33,957 lines, frozen). **Port:** `go/cmd/sobs/*.go` + `go/internal/*`.

## Method & scope

The Go port passes the golden-corpus **byte-parity** test, but **the corpus is empty-data**: any
logic gated on populated rows, an enabled feature, configured auth/crypto, an error branch, or an
external call was *never byte-compared*. This audit is a function-for-function read of the oracle
against the port, partitioned across 18 domain auditors, **targeting exactly those blind spots**.
Every finding cites `file:line` on both sides plus the concrete behavioral difference. There are
**zero `not implemented` 501 stubs** remaining — all findings are silent divergences in live code.

The 5 most severe claims (both CRITICALs + the highest-impact HIGHs) were re-verified directly
against source by the orchestrator; all confirmed. Auditors also reported "verified-equivalent"
items (not listed here except as highlights), indicating the negatives are trustworthy too.

## Totals

| Severity | Count |
|----------|------:|
| CRITICAL | 2 |
| HIGH | ~64 |
| MEDIUM | ~57 |
| LOW | ~48 |

## Cross-cutting themes (read these first)

1. **Render-engine gaps corrupt *every* populated page.** The Jinja interpreter is missing true
   division `/` and `caller is defined` — both only fire on non-empty data, so the empty corpus
   never caught them. These two alone break Summary/AI/Traces number formatting and per-error
   action buttons site-wide. (See `render-engine`.)
2. **"Hardcoded-empty read panels."** A recurring pattern: a handler renders a page but leaves the
   populated-data computation as `[]any{}` / `0` / `""` (the empty-corpus value), never running the
   query. Concentrated in: Summary (4 panels), `/rum` (vitals + error-stats), web-traffic (all 7
   endpoints drop the time filter), `/metrics/anomaly` (whole page), `view_notifications`,
   `view_ai_settings`, `view_agent_rules`, MCP settings, k8s Prometheus path.
3. **OTLP/RUM ingest drops trace correlation + attributes.** Trace/Span IDs are hardcoded empty on
   ingest; RUM error rows shed ~9 attributes and the clientAuthToken scrub; encoding differs
   (`ensure_ascii`, int-vs-float stringification). Invisible to the empty corpus.
4. **AI/LLM happy-path-only port.** Guard model, multi-turn tool loop, per-turn telemetry, the
   internal gen_ai span write, and the named-query allowlist are simplified or dropped — masked by
   the URL-keyed LLM mock that ignores the request body.
5. **MCP key lifecycle is broken** (create never persists; salt hardcoded to the parity secret) —
   the whole authenticated MCP surface only ever ran its empty-keystore error branch.
6. **Soft-delete tombstones + JSON-at-rest encoding** diverge: several writes store raw columns or
   Go-sorted/compact/HTML-escaped JSON where Python re-serializes insertion-order/spaced/unescaped.

---

# CRITICAL

### [CRITICAL] MCP API-key create never persists or hashes the key
- **Python**: `mcp.py` `mcp_api_create_key` — mints `smcp_…`, **scrypt-hashes it, INSERTs the descriptor** (id/label/hash/created/expires) into the MCP keystore, enforces the 20-key cap, honors body `expires_at`.
- **Go**: `handlers_crypto.go:47-62` `mcpAPIKeysCreate` — mints id+key and returns them but performs **no DB write, no hash, no cap, no expires_at**. (Verified by direct read.)
- **Divergence**: every minted MCP key is unusable — authenticated `tools/call` can never succeed because the keystore is always empty; the 20-key cap and per-key expiry are absent.
- **Blind spot**: auth/config-gate (empty corpus has no MCP keys → only the `-32002` empty-store branch ran).

### [CRITICAL] AI-generated named-query SQL bypasses the table allowlist in ai-build
- **Python**: `app.py:22350-22358` `ai_build_chart_spec` → `_vanna_execute_named_queries(use_repair=True)` → `_vanna_validate_and_execute_with_repair` → `validate_sql` on **every** LLM named query before execution. (`_vanna_execute_named_queries` def `app.py:22156`.)
- **Go**: `ai_build.go:63` → `executeNamedQueries` (`query_exec.go:231-258`) runs `injectLimit + db.Execute` with **no `validateSQL`**; only the SELECT/WITH prefix gate (`ai_build.go:193-194`) applies. (Verified by direct read; the primary `main` SQL *is* validated at `ai_build.go:46`.)
- **Divergence**: a named query such as `SELECT * FROM system.users` or a non-allowlisted `default.*` table passes the prefix gate and executes in Go; Python rejects it. Re-opens the LLM-SQL data-exfil hole the allowlist exists to close. Fix belongs in the ai-build call path (the shared helper is legitimately unvalidated for the user-spec dry-run/render path, which matches Python).
- **Blind spot**: config-gate (AI configured) + populated-data + external-net.

---

# HIGH (by cluster)

## render-engine (`go/internal/render`, `go/internal/jsonenc`)
### [HIGH] Jinja true-division `/` is unimplemented → every `n / X` server-side expression evaluates to nil→0
- **Python**: Jinja2 `/`. `summary.html:4` & `ai.html:6` `fmt_num` (`n / 1000000`, `n / 1000`); `traces.html:113` `fmt_ms` (`ms / 3600000`, `/ 60000`, `/ 1000`).
- **Go**: `internal/render/eval.go:296-332` `evalMulDiv` handles `//`, `*`, `%` only; `/` falls through `evalFiltered`→`evalAtom("n / 1000")`→`ctx.lookup` → **nil**. (Verified by direct read.)
- **Divergence**: `fmt_num(1500000)` → Python `"1.5M"`, Go `"0.0M"`; `fmt_ms(75000)` → `"1.2m"` vs `"0.0m"`. Corrupts Summary count badges, AI metric badges, and every trace-detail waterfall duration label under real data.
- **Blind spot**: populated-data (empty corpus → counts <1000 take the `{% else %}{{ n }}` branch; spans/errors loops empty).

### [HIGH] `{% if caller is defined %}` is always False inside macros → caller-block content dropped
- **Python**: `_error_panels.html:313` in macro `render_error_accordion`, invoked `{% call(err) %}` from `errors.html:117`; Jinja sets `caller`, so per-error "Raise issue"/"View issue" buttons render.
- **Go**: `internal/render/engine.go:313-340` binds the caller body as a FUNC (`e.funcs["caller"]`), but `eval.go:160-189` `is defined` resolves `caller` via `ctx.lookup` (scope vars only, never `e.funcs`) → always nil → test false.
- **Divergence**: on a populated errors page every row loses its caller-supplied GitHub action button.
- **Blind spot**: populated-data (empty errors list never reaches the guard).

## otlp ingest
### [HIGH] TraceId/SpanId/ParentSpanId never extracted — empty in every ingested row
- **Python**: `app.py:9212-9213`/`9236-9238` `record.trace_id.hex()` etc., into `otel_logs`/`otel_traces` rows (`9376-9377`,`9412-9414`) and error rows (`9453-9454`).
- **Go**: `otlp_ingest.go:199` (log), `:283` (trace), `:335` (error) hardcode `"TraceId":""`,`"SpanId":""`,`"ParentSpanId":""`; the parsed map carries them but builders never read them. (Verified.)
- **Divergence**: all spans/logs lose trace correlation → breaks trace↔log linking, trace-detail waterfall, `/api/traces/<id>`, error-id, and collapses every span auto-tag record-id to `md5("|")` (all collide). **Fix subtlety**: Python stores hex; protojson emits base64 → must base64-decode then lower-hex-encode.
- **Blind spot**: populated-data + external-net wire format.

### [HIGH] Unsupported metric types (exponential histogram, summary) silently dropped vs Python's fallback gauge
- **Python**: `app.py:9350-9365` — else-branch appends a placeholder gauge row (value 0, `now()` ts) that counts toward `accepted`.
- **Go**: `otlp_ingest.go:419-449` — gauge/sum/histogram if-chain with no trailing else → zero rows.
- **Divergence**: fewer rows + smaller `accepted`; OTel SDK runtime metrics commonly send exponential histograms.
- **Blind spot**: populated-data.

## v1 REST (RUM / registry / asset)
### [HIGH] RUM ingest error rows drop ~9 attributes + wrong ServiceName/Trace fields
- **Python**: `app.py:10076-10123` — `ServiceName="rum"`, real Trace/Span/Flags, conditional `exception.stacktrace`(demangled), `error.source`, `browser.page.title`, `browser.viewport`, `artifact.*`, `replay.*`.
- **Go**: `handlers_v1_ingest.go:399-413` — `ServiceName="browser"`, `TraceId=""`,`SpanId=""`,`TraceFlags=0`; only `exception.type/message`, `url.full`, `session.id`.
- **Divergence**: stored browser-exception rows differ in service, lose correlation, drop ~9 keys.
- **Blind spot**: populated-data (only error events).

### [HIGH] `_extract_trace_fields` not ported (no traceparent fallback / traceFlags)
- **Python**: `app.py:9927-9951` — trims/lowercases trace/span ids, parses traceFlags, falls back to parsing `traceparent` when ids absent.
- **Go**: `handlers_v1_ingest.go:389-391` — reads ids directly, `TraceFlags:0`, no fallback.
- **Divergence**: session rows lose TraceFlags; traceparent-only events get empty ids; affects RUM `traced_count`/linking.
- **Blind spot**: populated-data.

### [HIGH] `_handle_browser_context_delta` entirely missing
- **Python**: `app.py:9961-10005` — per-session browserContext cache, delta posting (contextUnchanged/contextHash), adds `browser.context.*` attrs.
- **Go**: `handlers_v1_ingest.go:383-386` — attrs = raw event + `client.ip` only; no cache, no `browser.context.*`.
- **Divergence**: session rows omit all `browser.context.*`; delta-posted events get no context.
- **Blind spot**: populated-data + stateful cache.

### [HIGH] `clientAuthToken` not stripped before Body/attrs → credential persisted into DB
- **Python**: `app.py:10034-10035` — `event.pop("clientAuthToken")` BEFORE `json.dumps(event)` Body and `_stringify_attrs`.
- **Go**: `handlers_v1_ingest.go:364-398` — never removes it; serialized Body + LogAttributes include the bearer token.
- **Divergence**: with RUM client auth enabled, Go writes the token into stored Body+LogAttributes (leak) and produces byte-different Body.
- **Blind spot**: config-gate (RUM client auth) + populated-data. **Security-relevant.**

### [HIGH] `/rum` vitals (summary/sparklines/hotspot) hardcoded empty
- **Python**: `app.py:17472-17544` — `v_derived_signals_anomaly`, `v_derived_signals_1m`, web-vital p75/poor-rate hotspot.
- **Go**: `handlers_rum.go:295` — all three return empty `jsonenc.NewObject()`.
- **Blind spot**: populated-data.

### [HIGH] `/rum` error_stats trend/by_type/top_messages/top_urls hardcoded empty
- **Python**: `app.py:17546-17633` — recent/prior/trend (30m vs prior 30m), total+by_type (24h), top_messages(8), top_urls(5).
- **Go**: `handlers_rum.go:273-277` — `total:0`, `by_type:{}`, `trend:"stable"`, `recent:0`, `prior:0`, empty lists; only sparkline computed.
- **Blind spot**: populated-data.

### [HIGH] `_build_rum_event_item`: has_artifact/has_replay hardcoded false + no trace-id injection
- **Python**: `app.py:8158-8194` — flags from parsed artifact/replay; injects `traceId`/`spanId` into data when present.
- **Go**: `handlers_rum.go:32-70` — `has_artifact:false`,`has_replay:false`; no id injection. Session-level OR-aggregation therefore always false.
- **Blind spot**: populated-data.

## ai helper / LLM
### [HIGH] Per-turn telemetry mostly not emitted (turn.start / guard.result / turn.blocked / tool.proposed / turn.error)
- **Python**: `app.py:27635-27648`,`27662-27686`,`28176-28244`,`28283-28293` — full event stream per turn.
- **Go**: `ai_helper.go:555-758` — emits only `turn.complete` + `turn.summary`.
- **Divergence**: otel rows missing per turn; `loadChatToolHistory` (`ai_chat.go:57`) reads `tool.proposed` Go never writes → chat-detail tool timeline structurally empty for Go-served turns.
- **Blind spot**: populated-data / telemetry side-effects.

### [HIGH] Multi-turn tool loop broken — loop messages never updated between rounds
- **Python**: `app.py:28131-28279` — appends assistant turn + tool-feedback system message each round, re-calls LLM; early-breaks on pending confirmation.
- **Go**: `ai_helper.go:625-659` — re-sends the identical `streamReq` every iteration; no message accumulation; no confirmation break.
- **Blind spot**: external-net (live endpoint returning tool_calls across rounds).

### [HIGH] Guard model check drastically simplified
- **Python**: `app.py:5136-5233` — gpt-oss-safeguard vs llama-guard prompt selection, guard-specific thinking/max_tokens/timeout, empty-content retry, category parsing, benign overrides, rich reasons.
- **Go**: `ai_helper.go:274-303` + `ai_llm.go:217-235` — llama-guard only; no retry, no category, no overrides, simplified reasons.
- **Divergence**: verdict + block-reason differ; benign prompts get blocked; oss-safeguard models use the wrong prompt.
- **Blind spot**: config-gate (guard endpoint/model) + external-net.

### [HIGH] Guard request omits guard-specific max_tokens / thinking / timeout resolution
- **Python**: `app.py:5164-5166` — `_resolve_guard_thinking_level/_max_tokens/_timeout_seconds`.
- **Go**: `ai_helper.go:284-290` — raw `ai.guard_thinking_level`, no max_tokens (defaults 1024), no timeout.
- **Blind spot**: config-gate + external-net.

### [HIGH] `propose_ui_action` feedback lacks ok/unsupported envelope; unsupported proposals dropped
- **Python**: `app.py:28160-28208` — unsupported normalized tools still emitted (`status unsupported`), added with `ok` flag, fed back to model.
- **Go**: `ai_helper.go:633-655` — `if normalized == nil { continue }`; unsupported silently dropped; no ok flag.
- **Blind spot**: populated-data / external-net.

### [HIGH] Internal gen_ai span broadcast to /tail but otel_traces CLIENT span row never written
- **Python**: `app.py:3528-3611` `_emit_internal_genai_span` — writes an `otel_traces` row + applies tag rules + broadcasts.
- **Go**: `ai_llm.go:48-60` `broadcastInternalGenAISpan` — only the /tail broadcast; no row write, no tag rules.
- **Divergence**: at runtime `/api/ai/conversation`, `/api/ai/span-attributes`, `/api/ai/export`, and the AI page have NO gen_ai spans for any self-generated LLM call — the producer never persists them.
- **Blind spot**: config-gate (live endpoint) + populated-data.

## ai build / NL→SQL
### [HIGH] ai-build drops `_infer_custom_mapping_from_option` on chart-spec success
- **Python**: `app.py:22387-22389` / `29900-29927`. **Go**: `ai_build.go:79-92` hardcodes `customMappingJSON="{}"`.
- **Divergence**: `spec.visual.custom_mapping_json` wrong whenever LLM option JSON has non-reserved placeholders + columns. **Blind spot**: populated-data + external-net.

### [HIGH] ai-build/ask drop EXPLAIN preflight + auto-repair + bounded LLM SQL-repair loop
- **Python**: `app.py:22071-22153` (EXPLAIN, CTE auto-repair, 3× `_vanna_repair_sql`, retry_count). **Go**: `ai_build.go:120-135` + `query_exec.go:509-518` one-shot, `retry_count` hardcoded 0.
- **Blind spot**: populated-data + external-net.

### [HIGH] `/api/query/ask` ignores execute/chart flags; do_chart pipeline missing
- **Python**: `app.py:30223-30224`,`30337-30467` (named queries + chart spec + 2 telemetry events). **Go**: `query_exec.go:461-540` never reads `execute`/`chart`, always `chart_spec:""`.
- **Blind spot**: populated-data + config-gate.

### [HIGH] NL→SQL prompt omits preferred_chart_type/chart_instruction/chart-catalog guidance
- **Python**: `app.py:29494-29521`. **Go**: `ai_llm.go:270` `generateSQLViaLLM(endpoint,question)` builds only question+allowlist; `generateNamedQueries` hardcodes empty type/instruction (`ai_build.go:140-145`).
- **Divergence**: parity-safe only because the mock ignores the body; wrong LLM request at runtime. **Blind spot**: external-net.

## charts
### [HIGH] derived_signal_overlay bindings swallow non-numeric values Python rejects
- **Python**: `app.py:20656-20681` `_extract_bindings` — `float(...)` raises ValueError → propagates → HTTP 400.
- **Go**: `chart_render_binding.go:516-523`,`559-620` — `numOf/fAt` best-effort; non-numeric → 0.
- **Divergence**: RAW-mode derived_signal_overlay with a non-numeric cell → Python 400, Go 200 with a chart of zeros. Affects spec/render + spec/validate. **Blind spot**: populated-data + error-path.

### [HIGH] custom_echarts point sort: DateTime cells sort by weekday string, not chronologically
- **Python**: `app.py:21172-21194` — datetime → rank 0 keyed chronologically.
- **Go**: `chart_render_binding.go:1111-1143` — a `chDateTime` struct falls to rank-3 default sorted by `toStr` = http_date `"Mon, 02 Jan 2006 …"`.
- **Divergence**: `SELECT toDateTime(...) AS time` series order differs (lexicographic-by-weekday). **Blind spot**: populated-data.

### [HIGH] custom_echarts numeric-STRING sort rank diverges (rank 1 vs 2)
- **Python**: `app.py:21177-21182` — string never float-parsed → rank (2, text) lexicographic (`"10"<"2"<"5"`).
- **Go**: `chart_render_binding.go:1118-1128` — string ParseFloat success → rank 1 numeric (`"2"<"5"<"10"`).
- **Blind spot**: populated-data.

### [HIGH] spec/render never executes named_queries → custom_echarts `{{rows:name}}` left literal
- **Python**: `app.py:22004-22024` — runs `_execute_chart_spec_named_queries(include_records=True)`, passes `named_datasets` into the renderer; `_render_custom_echarts` (`21245-21255`) resolves `{{rows:name}}`/`{{records:name}}`/`{{columns:name}}`.
- **Go**: `handlers_mutations2.go:376-396` → `renderChartFromTemplate` (`chart_render_binding.go:69`) has no named-datasets param; `renderCustomEcharts` (`:895`, comment `:893` "named_datasets is nil here") never builds those bindings.
- **Divergence**: custom_echarts specs referencing named datasets render with literal placeholders. **Blind spot**: configured-feature + populated-data.

## anomaly / candidates
### [HIGH] `/metrics/anomaly` page handler is a hardcoded empty stub
- **Python**: `app.py:14010-14172` `view_metrics_anomaly` — parameterized WHERE, queries `v_otel_metrics_anomaly`/`v_derived_signals_anomaly`, maps rows, runs `_annotate_rows_with_rules` (the full threshold/composite/seasonal engine), echoes all filters/point_state/point_score.
- **Go**: `handlers_pages.go:1996-2004` `handleViewMetricsAnomaly` — ignores every arg; always `rows:[]`, `total:0`, defaults; no query, no annotation.
- **Divergence**: empty + un-annotated table on populated data; the anomaly engine the rest of `chart_anomaly_engine.go` exists to serve is never invoked here. **Blind spot**: populated-data + request-param echo.

### [HIGH] Dashboard-auto handler ignores all form params + hardcodes summary; candidates not service-filtered/stripped
- **Python**: `app.py:13848-13868` + `_build_auto_dashboard_chart_candidates` (`12311-12385`) — reads action/service_filter/hours/max_charts/dashboard_name; filters by service; strips fields; computes capped.
- **Go**: `handlers_pages.go:158-210` `handleMetricsRulesDashboardAuto` — reads NO form values; summary hardcoded; no service skip, no strip.
- **Divergence**: any parameterized POST diverges (form echoes, summary line, JS maxCharts, candidate count, capped). **Blind spot**: populated-data + form params.

### [HIGH] Dashboard-auto `action="create"` branch entirely missing
- **Python**: `app.py:13885-13940` — seeds dashboard, inserts one `sobs_chart_configs` row per non-dup candidate, flashes summary, redirects to the dashboard view.
- **Go**: `handlers_pages.go:158-210` — `action` never read; even `action=create` renders the preview page (200 HTML) instead of creating + 302.
- **Divergence**: create writes nothing and returns the wrong status. **Blind spot**: populated-data + create-action.

## query / SQL
### [HIGH] `/api/{logs,ai}/validate-filter` is a no-op stub — all validation dropped
- **Python**: `app.py:23782-23855` / `24405-24454` — structural checks (quote balance, paren depth, trailing-operator), write/DDL keyword rejection, token normalization + `has_tag()`/AI rewrite, real existence probe; returns `{ok,normalized,issues}`.
- **Go**: `handlers_mutations.go:253-257` `handleValidateFilter` — ignores the body, always `{"issues":[],"normalized":"","ok":true}`. (Helpers `validateUserSQLWhere`/`normalizeAiSQLWhere` exist in `ai_view.go:179-224` but are unwired.)
- **Divergence**: every non-empty filter reported ok with no normalization/issues. **Blind spot**: populated-data / error-path.

### [HIGH] `/api/traces/span/<id>` returns 404 even when the span exists
- **Python**: `app.py:15695-15764` — found row → full masked payload (`duration_ms`, attrs, `_RAW_SPAN_MAX_BYTES` truncation), 200.
- **Go**: `handlers_pathparam.go:107-136` — runs the correct lookup, then line 135 unconditionally 404s even when `len(res.Rows)>0` (serialization unimplemented).
- **Divergence**: found span → 200+payload (Py) vs 404 (Go). **Blind spot**: populated-data.

### [HIGH] `/api/query/run` skips the EXPLAIN pre-flight — planning errors get 200 instead of 422
- **Python**: `app.py:30546-30627` — `validate_sql` then `_vanna_explain_sql` (EXPLAIN); failure → emit `query.sql.explain_failed` + **422**.
- **Go**: `query_exec.go:386-440` — `validateSQL` then directly `db.Execute`, no EXPLAIN; a planning-error SQL → **200** `{ok:true,…,error:…}` + `query.sql.executed`(status=error); error bytes also differ (`publicDashboardQueryError` 280-trunc vs raw).
- **Blind spot**: populated-data / error-path.

## observability pages
### [HIGH] Summary `recent_errors` panel never queried (hardcoded `[]`)
- **Python**: `app.py:10846-10864` — last-5 unresolved errors from `ERROR_SOURCES_SQL` + `_build_error_item`.
- **Go**: `handlers_pages.go:2933,2951` — `recentErrors := []any{}`. **Blind spot**: populated-data.

### [HIGH] Summary `ai_summary` per-model token table never queried
- **Python**: `app.py:10918-10926` — per-model call/token aggregation from `otel_traces`.
- **Go**: `handlers_pages.go:2954` — `"ai_summary": []any{}`. **Blind spot**: populated-data (AI spans).

### [HIGH] Summary `signal_health` per-service health never computed
- **Python**: `app.py:10969` → `_get_signal_health_by_service` (`12890-12935`) — `v_derived_signals_anomaly` + `_annotate_rows_with_rules`, worst-state per service.
- **Go**: `handlers_pages.go:2955` — `"signal_health": []any{}` (no `getSignalHealthByService` exists). **Blind spot**: populated-data + config-gate.

### [HIGH] Summary `cve_overview` counts hardcoded zero
- **Python**: `app.py:10940-10960` — when `cve_enabled`, `SELECT Severity,COUNT(*) FROM sobs_cve_findings FINAL GROUP BY Severity`.
- **Go**: `handlers_pages.go:2956-2959` — `total/critical/high/medium/low` all 0; query never run, no `cve_enabled` gate. **Blind spot**: populated-data + config-gate.

### [HIGH] `view_web_traffic` page drops the time-window filter entirely
- **Python**: `app.py:17665-17703` — `_parse_time_window_args` + `_time_window_conditions` WHERE on all 3 queries; echoes from_ts/to_ts/error_msg.
- **Go**: `handlers_pages.go:2964-2985` — never parses time args; no WHERE; from_ts/to_ts/error_msg hardcoded `""`.
- **Divergence**: `?from_ts=…&to_ts=…` silently unfiltered (all-time); invalid window never surfaces error. **Blind spot**: populated-data + error-path.

### [HIGH] web-traffic API browsers/os/timezones/languages/devices all drop the time-window filter
- **Python**: `app.py:17768-17877` — each parses time args + injects WHERE into `FROM hyperdx_sessions {where}`.
- **Go**: `handlers_data.go:310-397` — `r` unused; no `{where}`, no time params. Whole family shares the missing branch. **Blind spot**: populated-data.

### [HIGH] `api_web_traffic_geo` drops the time-window filter
- **Python**: `app.py:17717-17727`. **Go**: `handlers_misc.go:119-122` — IP-count query has no WHERE; time args ignored. (Geo lookup + ordering are correct.) **Blind spot**: populated-data.

## domain pages (k8s)
### [HIGH] K8s status: Prometheus (kube-state-metrics) format entirely unimplemented
- **Python**: `app.py:31196-31665` `_fetch_k8s_from_otel` — when format is `prometheus`, dedicated queries against `kube_node_*`, `kube_pod_*`, `container_memory_*`, `kube_deployment_*`, `kube_namespace_*`.
- **Go**: `k8s_status.go:91-185` — `detectK8sMetricFormat` *can* return `"prometheus"` but the whole data block is guarded `if format != "prometheus"` (`:97`) → empty dashboard + "no data" while still reporting `source:"prometheus"`.
- **Blind spot**: config-gate + populated-data (empty corpus → `format=="none"`, otel branch runs).

### [HIGH] K8s status: namespace/node/pod/deployment multi-value filters dropped
- **Python**: `app.py:31759-31764` + `31106-31113` — reads `namespace`/`*_values`, filters each table via OR-equals.
- **Go**: `k8s_status.go:52-95` — reads only `*_sort/_dir/_page/_page_size` + `name`; never reads namespace/node/deployment/pod, no OR-equals clauses.
- **Divergence**: filtered requests return unfiltered rows + wrong summary/total. **Blind spot**: populated-data + query-param.

## settings pages
### [HIGH] `view_notifications`: channels/rules/log/VAPID/edit_rule all hardcoded empty
- **Python**: `app.py:25710-25737` — `_load_notification_channels` (decrypts ConfigJson), `_load_notification_rules`, `_load_notification_log(50)`, `_get_vapid_public_key`, `?edit_rule`.
- **Go**: `handlers_pages.go:797-819` — `channels:[]`,`rules:[]`,`notification_log:[]`,`vapid_public_key:nil`,`edit_rule:nil`; only `metric_rules` real.
- **Blind spot**: populated-data + config-gate + crypto.

### [HIGH] `view_ai_settings` GET: all ai.* settings forced "", tag_rules/pricing/token hardcoded
- **Python**: `app.py:26646-26672` — `_load_all_ai_settings` (decrypts sensitive keys + env/file overrides), `_load_ai_pricing_with_sources`, `_load_tag_rules`, token status, `_load_confirmed_ai_pricing_models`.
- **Go**: `handlers_pages.go:3205-3227` — every `settings[k]=""`, `tag_rules:[]`, `confirmed_ai_pricing_models:[]`, hardcoded token maps, pricing read from embedded fixtures.
- **Blind spot**: populated-data + config-gate + crypto.

### [HIGH] `view_settings`: `ai_configured` misses env-only AI config
- **Python**: `app.py:23078-23103` — `_load_all_ai_settings` applies `_AI_ENV_OVERRIDES` (DB→file-env→env).
- **Go**: `handlers_pages.go:476-497` — `aiURL!="" && aiModel!=""` reading `appSetting` directly, no env fallback.
- **Divergence**: env-supplied AI config → Python `ai_configured=True`, Go `False`. **Blind spot**: config-gate.

### [HIGH] `view_agent_rules`: runs hardcoded empty
- **Python**: `app.py:27174-27189` — `_load_agent_runs(db, limit=20)`.
- **Go**: `handlers_pages.go:2989-3003` — `"runs": []any{}`. **Blind spot**: populated-data.

## forms / mutations
### [HIGH] `createTagRule` writes ConditionsJson with wrong encoding
- **Python**: `app.py:23577` — `json.dumps(conditions, ensure_ascii=False)`: spaced separators, insertion-order keys, no `<>&` escaping.
- **Go**: `handlers_pages.go:3082-3084` — `json.Marshal` over a Go map: compact, alphabetically-sorted keys, HTML-escapes `<>&`.
- **Divergence**: divergent at-rest bytes on every populated tag-rule create. **Blind spot**: populated-data.

### [HIGH] `delete_report` tombstone writes raw FiltersJson
- **Python**: `app.py:22699-22720` → `_get_report`/`_parse_report_filters` → re-serialized (`{}` for invalid). **Go**: `handlers_forms.go:174-184` writes `cStr(m,"FiltersJson")` verbatim. **Blind spot**: populated-data.

### [HIGH] `deleteDashboard` / `removeChart` tombstones write raw OptionsJson
- **Python**: `app.py:21549`,`21736` source from `_get_charts` (rebuilt `{"chart_spec":…}` wrapper). **Go**: `handlers_forms.go:1042-1071`,`1114-1137` own SELECT, write raw column (bypassing their own `getCharts`). **Blind spot**: populated-data.

### [HIGH] `subscribe_browser_push` stores ConfigJson as base64 of separator-less invalid JSON + skips secret encryption
- **Python**: `app.py:26497-26535` — `_encrypt_notification_config({…,auth})` (Fernet when key set) → `ConfigJson: json.dumps(…)` (well-formed).
- **Go**: `handlers_mutations2.go:78-84` — `jsonenc.Encode(cfg, Options{SortKeys:false})` with zero-value `ItemSep/KeySep=""` → invalid `{"endpoint""…""p256dh""…"}`; the `[]byte` is then stdlib-marshaled into JSONEachRow → **base64 of malformed JSON**; no `encryptNotificationConfig` (plaintext `auth`).
- **Divergence**: dedup never matches, push dispatch loses keys, secret at rest. **Blind spot**: populated-data write + crypto.

### [HIGH] `/api/reports/import` is a hardcoded 400 stub
- **Python**: `app.py:22855-23017` — size guard, multipart/JSON parse, on_conflict rename/replace/skip, validation, real inserts, `{imported,skipped,replaced,errors}`.
- **Go**: `handlers_mutations3.go:78-92` — always `400 "Not a valid SOBS reports export file"`. **Blind spot**: populated-data.

### [HIGH] DM backup drops encryption SETTINGS + incremental BASE_BACKUP
- **Python**: `app.py:32134-32170` `_run_dm_backup` — appends `SETTINGS …encryption_password=…` when `s3_encrypt_backup=="1"`; appends `BASE_BACKUP <dest>` for incremental.
- **Go**: `handlers_mutations.go:184-199` `runDmBackup` — `BACKUP ALL TO <dest>` only; no encryption, no BASE_BACKUP.
- **Divergence**: encrypted backups written UNENCRYPTED; incremental always full. **Blind spot**: config-gate + external-net (S3).

## notifications / agent flow
### [HIGH] Agent flow persists work item via the WRONG function (onboarding, not agent)
- **Python**: `app.py:6937-6965` `_run_agent_flow` → `_persist_github_work_item` (`6438-6537`) keyed by run_id with real AgentRunId/RuleId/Name, AgentAction=`github_issue[_copilot]`, trigger-derived signal fields, real dedup/PR fields, `AnalysisSummary=analysis[:500]`, then `_invalidate_work_items_cache()`.
- **Go**: `agent_flow.go:218-229` → `persistOnboardingWorkItem` (`onboarding_issues.go:305-331`) — random Id, empty AgentRunId/RuleId, `AgentRuleName="Onboarding Wizard"`, `AgentAction="onboarding_agent"`, `ServiceName=rule.name`, empty signal/dedup, `AnalysisSummary="Sobs onboarding wizard issue."`, no PR, no cache invalidation.
- **Divergence**: agent-created work items get near-totally wrong metadata; breaks work-items rows, cross-linking, occurrence counting, and future-run dedup. **Blind spot**: populated-data + config-gate.

### [HIGH] Auto agent-trigger path drops DLP + max-issue/assignment settings
- **Python**: `app.py:26365` `check_notifications` → `_load_all_ai_settings(db)` (full `_AI_SETTING_KEYS`).
- **Go**: `notif_agent.go:203-211` — fixed 7-key map omitting `ai.dlp_endpoint_url` + all `ai.agent_max_*`.
- **Divergence**: auto-triggered rule with a `dlp_check` action + configured DLP endpoint → Go sees `""` → **skips DLP screening** (`agent_flow.go:195`), letting PII-laden issue text reach GitHub; caps revert to defaults. **Blind spot**: config-gate (DLP) + populated-data.

### [HIGH] Manual `trigger_agent_run` / `raise_issue` drop system_prompt, thinking_level, dlp & max settings
- **Python**: `app.py:28685`,`28554` — `_load_all_ai_settings(db)`. **Go**: `agent_flow.go:376-383`,`503-510` — fixed 6-key maps omitting `ai.system_prompt`,`ai.thinking_level`,`ai.agent_max_*`,`ai.github_copilot_*`.
- **Divergence**: built-in default prompt used despite a configured custom prompt; empty thinking level to the analyze LLM. **Blind spot**: config-gate + populated-data.

### [HIGH] `mask_output_enabled` never threaded → Go always masks issue body
- **Python**: `app.py:6794-6801`,`6933`,`6398-6399` — derives `mask_output_enabled` (default True) from trigger context, threads to `_create_github_issue_record` which masks ONLY when enabled.
- **Go**: `runAgentFlow` never reads `mask_output`; `chooseGithubIssueOutcome` (`agent_dedupe.go:262`) has no mask param; `createGithubIssueRecord` (`onboarding_issues.go:181-182`) unconditionally masks. (`handleApiIssuesRaise` sets `extra.mask_output` at `agent_flow.go:517-521` but it's dead.)
- **Divergence**: with `mask_output=false`, Python sends unmasked title/body to GitHub, Go sends masked. **Blind spot**: config-gate + populated-data.

### [HIGH] `assignIssueToCopilot` stores requested-at in SECONDS, breaking the hourly rate limiter
- **Python**: `app.py:5357` — `int(time.time()*1000)` (ms); `_count_copilot_assignments_last_hour` compares `>= now_ms - 3600*1000`.
- **Go**: `onboarding_issues.go:300` — `int(nowUTC().Unix())` (seconds); `countCopilotAssignmentsLastHour` (`agent_dedupe.go:510`) uses `UnixMilli()-3600000`.
- **Divergence**: a seconds value (~1.7e9) is always below the ms cutoff (~1.7e12) → a just-requested assignment never counts → per-hour limiter effectively disabled. **Blind spot**: config-gate + populated-data.

### [HIGH] `assignIssueToCopilot` skips copilot-support probe + body fields + response inspection
- **Python**: `app.py:5337-5378` — issue_number guard; `_github_repo_supports_copilot_assignment` probe → blocked if unsupported; threads `base_branch` + `custom_instructions[:4000]`; inspects response assignees; HTTP error → `("failed", detail, requested_at)`.
- **Go**: `onboarding_issues.go:283-301` — no guard, no probe, no body fields, no inspection; always `("requested","",…)` on 2xx.
- **Divergence**: Go attempts assignment where Python returns "blocked"; reason strings differ; POST body lacks base_branch/custom_instructions. **Blind spot**: external-net + config-gate.

### [HIGH] Notification fire does NOT register a raw-preservation window
- **Python**: `app.py:25316-25324` — on fire calls `_register_raw_window(signal_type="notification", signal_ref=rule.id)`.
- **Go**: `notif_check.go:368-402` — writes log rows + bumps LastFiredAt but never calls `registerRawWindow` (only the agent path does, `notif_agent.go:282`).
- **Divergence**: a fired rule leaves no `sobs_raw_windows` row → raw metrics around the alert aren't pinned. **Blind spot**: populated-data.

## repos / onboarding / cve
### [HIGH] `_assign_issue_to_copilot` port omits copilot-support gate + wrong return values
*(Same root code as the notif-agent copilot findings, surfaced from the onboarding create-issues path.)*
- **Python**: `app.py:5329-5385`. **Go**: `onboarding_issues.go:283-301` — never returns `"blocked"`; success `("requested","",seconds)`; failure `("failed","assignment request failed",0)`.
- **Divergence**: wrong `copilot_assignment_status`/`reason` + `requested_at` off by 1000× in the create-issues response and persisted row. **Blind spot**: external-net + populated-data + config-gate.

### [HIGH] `_github_actions_dependency_rows` stubbed (whole GH-Actions dep-snapshot path missing)
- **Python**: `app.py:16479-16629` — `/actions/runs?head_sha=` → artifacts → download snapshot zip → parse `pip-freeze-*` → emit `dependencies-lockfile` rows; returning rows makes the caller skip the contents fallback.
- **Go**: `cve_scan.go:147-152` `githubActionsDependencyRows` always returns nil → caller (`cve_scan.go:124-129`) always falls through to the contents API.
- **Divergence**: different/absent inserted artifacts → wrong `github_backfill_inserted` + different CVE inventory. **Blind spot**: external-net + populated-data + config-gate.

## MCP
### [HIGH] MCP `limit` argument ignored when sent as a JSON number (the schema-conformant form)
- **Python**: `mcp.py:553` (+604/657/714/836) — `_clamp(args.get("limit"),1,500,100)` → `int(value)` honors a JSON integer (schema says `"type":"integer"`).
- **Go**: `mcp_tools.go:137/159/185/211/274` — `mcpClamp(objGetStr(args,"limit"),…)`; `objGetStr` (`handlers_mutations2.go:648-655`) returns `""` for a non-string. MCP args parse with UseNumber → `limit` is a `json.Number` → `""` → default.
- **Divergence**: `arguments:{"limit":250}` → Python up to 250 rows, Go caps at default; honored only if the client non-conformantly sends a string. **Blind spot**: auth/config-gate (MCP needs a key).

### [HIGH] MCP default time window is 24h in Go vs 1h in Python
- **Python**: `mcp.py:458` `_DEFAULT_WINDOW_HOURS = 1`. **Go**: `mcp_tools.go:15` `mcpDefaultWindowHours = 24`.
- **Divergence**: with no `from_ts`, every windowed MCP tool (logs/traces/metrics/metrics_raw/recent_errors) returns a different default row set + count. **Blind spot**: auth/config-gate + populated-data.

## auth
### [HIGH] OPTIONS preflight on OTLP/RUM ingest returns 4xx, breaking browser CORS
- **Python**: `app.py:9655-9660` (+10008/10155/10247/9789) — `ingest_preflight` returns 204; Werkzeug auto-OPTIONS returns 200 on the others.
- **Go**: `handlers_static.go:149-174`, `handlers_v1_ingest.go:145/262/327/456` — `OPTIONS /v1/logs|traces|metrics|rum/assets` → 405; `OPTIONS /v1/rum|errors|ai|rum/client-token` → 404. CORS headers are applied but the non-2xx status fails the preflight.
- **Blind spot**: error-path (corpus never sends OPTIONS).

---

# MEDIUM (by cluster)

**otlp** — timestampless metric points → Go stamps 1970 (`otlp_ingest.go:413`) vs Python `now()` (`app.py:9287/9307/9330`); AnyValue `bytesValue` dropped (`otlp_ingest.go:30-71` vs `app.py:9091-9092`); malformed/non-object JSON → Go 200 `{accepted:0}` (`handlers_v1_ingest.go:69-74`) vs Python 400 (`app.py:9641-9648`).

**v1rest** — otel_logs error write is fire-and-forget, swallows failures (`handlers_v1_ingest.go:435-448` vs `app.py:10126-10145`, Py 500); `ingest_ai` Duration not clamped ≥0 (`:190` vs `app.py:10204`); non-ASCII escaped in non-scalar attrs (`ensure_ascii` mismatch, `:254/:320` vs `app.py:8215`); integral-float stringifies `"1"` vs `"1.0"` (`:251-252` vs `app.py:8212-8213`, port-wide); int fields reject string-typed numbers (`:126-131` vs `app.py:10171-10172`); `record_ingest_batch_size` not called for RUM (`:449` vs `app.py:10147-10148`); app-registry seed slug fallback `"app"` vs raw name (`app_registry_seed.go:51` vs `app.py:8970`).

**aihelper** — capabilities uses static manifest + hardcoded thinking + gates supports_tools/thinking on "configured" not model name (`handlers_get2.go:40-65` vs `app.py:27410-27430`); `/api/ai/export` ignores time window (`handlers_get2.go:199-236` vs `app.py:19129/19155-19157`); `/api/ai/span-attributes` success not masked (`handlers_misc.go:57` vs `app.py:19037` — borderline CRITICAL if AI prompts contain secrets, needs masking rules + seeded spans); `/api/ai/conversation` success path unimplemented, always 404 (`handlers_misc.go:410-419` vs `app.py:19046-19115`); export JSON-parse fallback uses raw text not extracted message text (`handlers_get2.go:248-261` vs `app.py:19173-19195`).

**aibuild** — ai-build/ask drop `_QUERY_MAX_ROWS` cap (`ai_build.go:133`+`query_exec.go:516` vs `app.py:30176-30177`); `_build_client_action` sanitizer off-by-one max depth, Go stricter/no security loss (`ai_action_execute.go:159` vs `app.py:4353-4391`).

**charts** — spec/validate uses raise-only pre-check `renderWouldError` instead of the real render → returns `valid:true` 200 where Python returns `valid:false` 400 for data/custom-mapping render failures (`chart_render.go:142-162/37-57` vs `app.py:21968`).

**anomaly** — dashboard-auto preview candidates omit `chart_type`/`query` + don't strip fields (`handlers_pages.go:183-186` vs `app.py:12363-12375`, also the cause of the missing create branch); seasonal eval sets `is_seasonal=true` for empty/malformed bucket dicts where Python keeps False (`chart_anomaly_engine.go:262-275` vs `app.py:13063-13069`).

**query** — `inferQueryFieldTypes` reports Map/Array columns kind `"string"` not `"json"` + no whole-column pandas dtype (`query_exec.go:304-334` vs `app.py:30092-30123`); `/api/query/run` ignores `chart:true` do_chart branch (`query_exec.go:355` vs `app.py:30629-30765`); `getSchemaContext` omits the observed-OTEL-attr-keys block under data (`query_introspect.go:246-267` vs `app.py:29158-29221`); validate-regex/filter first-pass uses Go RE2 not Python `re` → over-rejects backref/lookahead patterns + divergent error bytes (`query_filters.go:304-338` vs `app.py:11157-11207`).

**pages-domain** — k8s sort-column maps are a reduced/divergent subset (`k8s_status.go:22-26` vs `app.py:31120-31144`); k8s OTel pod/deployment `created/node/available` sort keys ignored (`:24-25` vs `app.py:31135-31143`); CVE page `cve_last_backfill_attempted/inserted/cap` hardcoded 0 (`handlers_pages.go:951` vs `app.py:18110-18121`).

**pages-settings** — `view_tag_rules` drops `?edit_rule` lookup+flash (`handlers_pages.go:3161-3178` vs `app.py:23278-23302`); `view_settings_repositories` never surfaces the show-once `ci_push_plain` from session (`repositories.go:157-163`+`handlers_pages.go:3311-3314` vs `app.py:26805-26861`); AI pricing table from embedded fixture vs live DB merge (`handlers_pages.go:3211-3226` vs `app.py:2849-2869`).

**forms** — `createSettingsRepository` skips the 3 `ai.github_token_last_validation_*` resets (`handlers_pages.go:3265-3269` vs `app.py:26935-26940`); stores raw `github_token_expires_at` skipping `_normalize_github_token_expiry_input` (`:3268` vs `app.py:26900`); `validateGithubToken` flash uses full `err.Error()` vs `exc.__class__.__name__` (`repositories_actions.go:245` vs `app.py:3351`); `applyDMTTL` non-numeric ttl-days error text diverges (`handlers_datamgmt.go:39-42` vs `app.py:31945-31953`).

**mutations** — DM backup-enabled gate accepts `true/yes/on` vs Python exactly `"1"` (`handlers_mutations.go:149`/`dbutil.go:279-290` vs `app.py:31932-31935`); `/api/query/refine-chart` drops body `thinking_level` + rejects non-string `chart_spec` (`handlers_mutations.go:25-69` vs `app.py:30770-30813`); `buildS3BackupDest` omits `_validate_dm_backup_name`/`_validate_dm_s3_settings` (`handlers_mutations.go:219-242` vs `app.py:32086-32089`); `api_export_reports` ignores the `?ids=` subset (`handlers_get2.go:160-191` vs `app.py:22820-22824`).

**repos-cve** — CVE backfill `StorageRef` not URL-escaped (`cve_scan.go:189` vs `app.py:16830`); `api_web_traffic_geo` ignores time window (`handlers_misc.go:120-122` vs `app.py:17717-17727` — dup of obs-pages finding); onboarding create/update never reaches the "updated" branch, always "reused" (`onboarding_issues.go:147-166` vs `app.py:33050-33105`); onboarding OTEL work-item persisted with `AgentAction="onboarding_otel"` vs Python `onboarding_observability` (`onboarding_issues.go:106-107` vs `app.py:33914-33928`); onboarding issue title/body masked in Go but `mask_output_enabled=False` in Python (`onboarding_issues.go:181-182` vs `app.py:33063-33090`); github-token expiry ISO drops microseconds (`repositories.go:27-29/62-71` vs `app.py:3144-3147`).

**notif-agent** — no per-rule exception isolation in the check loop; a panic 500s the whole `/api/notifications/check` vs Python's per-rule error entry + 200 (`notif_check.go:412-418` vs `app.py:26353-26359`).

**masking** — `/api/settings/masking/rules` does NOT normalize/validate/dedupe/sort custom keys+patterns (`handlers_misc.go:247-279` vs `app.py:23258-23270`→`_load_masking_settings`); `/settings/masking` page mirrors the same un-normalized rendering + counts invalid/dup patterns (`handlers_pages.go:3393-3419` vs `app.py:23106-23121`); custom-pattern validation engine mismatch — Go RE2 rejects Python-`re`-valid patterns (lookahead/possessive) → silently dropped on load → under-redaction (`masking.go:139-170` vs `app.py:186-245`); `loadJSONStringListSetting` skips trim + numeric coercion (`handlers_misc.go:283-299` vs `app.py:25566-25584`).

**crypto** — MCP auth key compare is not constant-time (compounded by the create CRITICAL).

**auth** — exact-route guard never emits Werkzeug auto-OPTIONS (OPTIONS leaks into handlers, `route_guard.go:46-63`); `rawRouteAllow` wrongly lists GET/HEAD for OTLP ingest paths (`route_allow_gen.go:166-169`); HSTS omitted when `SOBS_BEHIND_TLS=1` without https header (`server.go:485-487` vs `app.py:361-367`); session cookie emitted UNSIGNED (`handlers_forms.go:102-167`, no itsdangerous HMAC — forgeable, not interoperable with Python; no in-Go bypass today).

---

# LOW (by cluster, compact)

- **otlp**: metrics 500-wrapper unreachable; histogram mean computed-and-discarded both sides (equivalent).
- **v1rest**: client-token ttlSec ignores string values; RUM client-token encode HTML-escapes `<>&`; `_normalize_origin` netloc-vs-Host userinfo; `/static/rum.js.map` 404 body differs; `rum_asset_download` thinner headers (no caching/range); `demangleStack` `Split("\n")` vs `splitlines()` CRLF.
- **aihelper**: `coerceSummaryValue` truncates by bytes not runes; guard_stats shape differs on guard-unavailable.
- **aibuild**: refine-chart sample_rows double-slice (same result); refine-chart drops `_repair_chart_spec_json_with_llm` fallback.
- **charts**: custom_echarts asset carries `drilldown:{}` (never read); dead `specModeGuard`/`specSQLMode` helpers.
- **anomaly**: metrics-retention env validation parity-gated (Go) vs import-time (Python) — TTL ALTER itself faithful.
- **query**: `injectLimit` semicolon+whitespace edge vs `rstrip(";")`; `_check_table_refs` blocked-ref bytes differ for non-ASCII identifiers (still blocked, no bypass).
- **pages-obs**: errors services-list TTL cache not ported (perf only); summary-stats TTL cache not ported (perf only); parseLimit/coerce empty-arg clamping (no observable diff).
- **pages-domain**: k8s namespace status always "Active" on otel path (subsumed by Prometheus HIGH); AI page totals dead-branch (verified equivalent).
- **pages-settings**: masking custom-key/pattern parse path differs from canonical loader (likely diff under custom rules); `handleMcpSettingsPage` `mcp_keys` hardcoded empty.
- **forms**: tag/agent rule Ids use hex vs dashed uuid4; `edit_rule_id` update-in-place path not ported (always creates duplicate); notif tag-regex error redirect drops `edit_rule`; `repoCiKeyRotate` empty-key guard unreachable.
- **mutations**: `api_create_report` FiltersJson sorted+compact vs insertion-order+spaced (not observable); `import_chart` `sobs_chart_template_version:true` accepted by Py, rejected by Go; setup-wizard non-default combos served from static embed (unverified data).
- **repos-cve**: `_validate_github_token` network-error message differs; base64 decode tolerance differs (`validate=False`).
- **notif-agent**: `_emit_agent_issue_decision_summary` log line not ported; VAPID JWT header/claims JSON byte-shape differs (both valid ES256); guard model simplified (cross-ref aihelper).
- **crypto**: truncated chdb error message.
- **masking**: `appSettingBool` no TrimSpace (`" 1 "`→falsy); preview re-masks the response wrapper (idempotent); `maskPayloadJSON` only handles `*jsonenc.Object`/`[]any` (latent — all live callers safe).
- **auth**: one-time rotated CI key never read back (`repositories.go:162`); `sobs_version` not `.strip()`-ed; `_refresh_masking_rules_before_request` replaced by per-call DB reads (equivalent for responses).
- **mcp-engine**: `|length`/`|count` byte-length vs code-point for strings (ASCII-safe in practice); telemetry enabled-gated, never in HTTP responses; `is defined`/`default` can't distinguish defined-None from undefined (no observed template hits).
- **render**: see `|length` above (in mcp-engine).

---

# Notable confirmed-faithful subsystems (bounding the audit)

These were read closely and verified equivalent — **not** problem areas:
per-app CI-push key crypto (scrypt n=1024/r=8/p=1 + blake2b person + constant-time compare + runtime salt),
Fernet settings encryption (cross-decrypt verified against a real Python token), VAPID/DM at-rest
encryption (prior plaintext finding resolved), security-header strings+order + full OTLP CORS allow-list,
SQL allowlist core (`validate_sql`/`_check_table_refs`/18-table list + env merge + difflib),
webpush AES-128-GCM RFC-8291/8188 + HKDF, the dedup/reuse classifier, geoip (private-IP union + embedded
DB byte-verified), all 4 lockfile parsers + library inventory merge, OSV query construction,
the incident-view correlation engine + helpers, the AI-page trace-turn aggregation, work-items backfill,
CVE findings/disposition logic, the chart builder SQL (8 sources × 7 templates) + spec
normalize/compile/visual-overrides, the metric/tag candidate-gen math + sort + dedup, notification
auto-generate, the dm-prune family, jsonenc float/NaN/Inf/unicode/`<>&'` escaping + key sorting, and
most render-engine operators (`*` float-aware, `~`, `in` substring-vs-membership, `truncate`, `round`
half-even, `selectattr`, loop vars). The AI memory-consolidation WRITE path (`saved_memory_ids` +
`sobs_ai_memories` INSERT) is also correctly ported.
