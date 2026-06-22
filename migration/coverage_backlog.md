# app.py uncovered-line backlog

Oracle coverage **81%** · **2892** uncovered statements.

## By bucket

| bucket | functions | uncovered lines | meaning |
|---|---:|---:|---|
| route | 90 | 413 | needs a fixture/profile (corpus expansion; byte-verifiable) |
| helper | 381 | 2254 | usually covered when a calling route's fixture is added; else difftest |
| lifecycle | 21 | 212 | background/lifecycle — needs a function-level difftest (capture can't reach) |
| module | — | 13 | top-level/startup/defensive — mostly dead, classify+exclude |

## route — top by uncovered lines (90 functions)

| function | lines | uncovered | route |
|---|---|---:|---|
| `ai_helper` | 27607–28395 | 35 | `POST /api/ai/helper` |
| `view_incident` | 15785–16144 | 22 | `GET /incident` |
| `auto_tag_rules` | 23353–23460 | 16 | `POST /settings/tags/auto` |
| `api_query_run` | 30511–30765 | 16 | `POST /api/query/run` |
| `ingest_rum_asset` | 9699–9756 | 14 | `POST /v1/rum/assets` |
| `view_traces` | 15312–15678 | 10 | `GET /traces` |
| `view_rum` | 17309–17657 | 9 | `GET /rum` |
| `check_notifications` | 26341–26447 | 9 | `POST /api/notifications/check` |
| `api_query_ask` | 30203–30506 | 9 | `POST /api/query/ask` |
| `ingest_metrics` | 9896–9918 | 8 | `POST /v1/metrics` |
| `api_ai_field_hints` | 24229–24402 | 8 | `GET /api/ai/field-hints` |
| `api_onboarding_import_repo` | 33538–33598 | 8 | `POST /api/onboarding/import-repo` |
| `api_onboarding_list_repos` | 33603–33678 | 8 | `POST /api/onboarding/list-repos` |
| `view_logs` | 11239–11512 | 7 | `GET /logs` |
| `ai_build_chart_spec` | 22263–22431 | 7 | `POST /api/dashboards/spec/ai-build` |
| `create_notification_rule` | 25918–26089 | 7 | `POST /settings/notifications/rules` |
| `ai_helper_chats` | 27448–27502 | 7 | `GET /api/ai/helper/chats` |
| `api_onboarding_create_repo` | 33464–33533 | 7 | `POST /api/onboarding/create-repo` |
| `ingest_traces` | 9834–9888 | 6 | `POST /v1/traces` |
| `ingest_rum` | 10010–10149 | 6 | `POST /v1/rum` |
| `ingest_errors` | 10249–10295 | 6 | `POST /v1/errors` |
| `api_raw_span` | 15695–15764 | 6 | `GET /api/traces/span/<span_id>` |
| `create_tag_rule` | 23465–23584 | 6 | `POST /settings/tags` |
| `ai_helper_execute_action` | 28400–28468 | 6 | `POST /api/ai/helper/actions/execute` |
| `raise_issue_from_user_observation` | 28547–28654 | 6 | `POST /api/issues/raise` |
| `ingest_logs` | 9665–9694 | 5 | `POST /v1/logs` |
| `ingest_ai` | 10157–10241 | 5 | `POST /v1/ai` |
| `api_web_traffic_geo` | 17711–17760 | 5 | `GET /api/web-traffic/geo` |
| `view_work_items` | 18361–18488 | 5 | `GET /work-items` |
| `api_logs_field_hints` | 23707–23779 | 5 | `GET /api/logs/field-hints` |
| `rum_asset_download` | 9761–9786 | 4 | `GET /v1/rum/assets/<asset_id>` |
| `auto_metrics_rules_dashboard` | 13848–13960 | 4 | `POST /metrics/rules/dashboard/auto` |
| `view_errors` | 14210–14583 | 4 | `GET /errors` |
| `api_cve_findings` | 18220–18278 | 4 | `GET /api/enrichment/cve/findings` |
| `api_query_add_to_dashboard` | 21331–21407 | 4 | `POST /api/query/add-to-dashboard` |
| `save_dm_settings` | 32215–32248 | 4 | `POST /settings/data-management` |
| `api_onboarding_create_issues` | 33738–33930 | 4 | `POST /api/onboarding/create-issues` |
| `view_metrics` | 13430–13595 | 3 | `GET /metrics` |
| `view_metrics_anomaly` | 14010–14172 | 3 | `GET /metrics/anomaly` |
| `resolve_error` | 14588–14598 | 3 | `POST /errors/<string:error_id>/resolve` |

## lifecycle — top by uncovered lines (21 functions)

| function | lines | uncovered | route |
|---|---|---:|---|
| `_run_raw_window_copy_worker` | 2496–2649 | 67 |  |
| `_dispatch_email_channel` | 24858–24892 | 26 |  |
| `_shutdown_async_http_client` | 326–352 | 25 |  |
| `ensure_db_schema` | 2135–2152 | 11 |  |
| `_raw_window_copy_loop` | 2652–2667 | 9 |  |
| `_cve_scanner_loop` | 17227–17241 | 9 |  |
| `_dispatch_browser_push_channel` | 24895–24960 | 9 |  |
| `_run_cve_scan` | 17122–17224 | 8 |  |
| `_run_agent_flow` | 6772–6999 | 7 |  |
| `_github_repo_health_loop` | 17244–17252 | 7 |  |
| `_dispatch_webhook_channel` | 24814–24839 | 7 |  |
| `_write_worker_main` | 7427–7447 | 6 |  |
| `_run_agent_rule_instance` | 25426–25488 | 5 |  |
| `_startup_async_http_client` | 319–322 | 3 |  |
| `_startup_enrichment` | 17296–17301 | 3 |  |
| `_ensure_raw_metrics_retention` | 2360–2376 | 2 |  |
| `_ensure_notification_schema` | 7002–7011 | 2 |  |
| `_dispatch_slack_channel` | 24842–24855 | 2 |  |
| `_refresh_masking_rules_before_request` | 25660–25666 | 2 |  |
| `_apply_security_headers` | 459–485 | 1 |  |
| `_dispatch_notification_channel` | 25058–25075 | 1 |  |

## helper — top by uncovered lines (381 functions)

| function | lines | uncovered | route |
|---|---|---:|---|
| `_generate` | 27807–28123 | 53 |  |
| `_call_llm_endpoint` | 4615–4770 | 46 |  |
| `_github_actions_dependency_rows` | 16479–16629 | 43 |  |
| `_choose_github_issue_outcome` | 5388–5646 | 33 |  |
| `_normalize_generic_ui_action_tool_call` | 4399–4529 | 28 |  |
| `_geo_lookup_batch` | 16194–16241 | 28 |  |
| `_stream_llm_endpoint` | 4773–4887 | 25 |  |
| `_vanna_generate_named_queries` | 29555–29648 | 24 |  |
| `_run_dm_backup` | 32134–32170 | 24 |  |
| `_sanitize_value` | 4353–4382 | 22 |  |
| `_parse_oss_safeguard_reply` | 5042–5092 | 22 |  |
| `_fetch_release_deps_from_github` | 16703–16873 | 22 |  |
| `_vanna_generate_sql` | 29475–29552 | 22 |  |
| `_infer_custom_mapping_from_option` | 29900–29927 | 22 |  |
| `_check_guard_model` | 5136–5233 | 21 |  |
| `_shutdown_db_resources` | 7484–7510 | 21 |  |
| `_vanna_execute_named_queries` | 22156–22210 | 21 |  |
| `_create_github_issue_record` | 6380–6435 | 20 |  |
| `_build_ai_trace_turn_cards` | 8506–8644 | 20 |  |
| `_vanna_generate_chart_spec` | 29788–29889 | 20 |  |
| `_consolidate_memory_candidates` | 3838–3900 | 19 |  |
| `_sync_github_repo_health_once` | 17255–17292 | 18 |  |
| `_build_user_issue_trigger_context` | 28476–28542 | 18 |  |
| `_repair_truncated_in_clause_literals` | 29713–29743 | 18 |  |
| `_auto_repair_incomplete_cte_sql` | 29746–29785 | 18 |  |
| `_fetch_k8s_from_otel` | 31071–31697 | 18 |  |
| `_backfill_github_work_item_links` | 6030–6189 | 17 |  |
| `_verify_rum_asset_signature` | 7706–7745 | 17 |  |
| `_summarize_ai_tool_action` | 8486–8503 | 17 |  |
| `_handle_browser_context_delta` | 9961–10005 | 17 |  |
| `_generate` | 24481–24498 | 17 |  |
| `_vanna_refine_chart_spec` | 30015–30086 | 17 |  |
| `_update_github_issue_record` | 32994–33047 | 17 |  |
| `_embedding_from_json` | 3650–3666 | 16 |  |
| `_proto_metrics_to_events` | 9266–9367 | 16 |  |
| `_get_geo_db` | 16172–16191 | 16 |  |
| `_collect_anomaly_agent_events` | 25351–25395 | 16 |  |
| `_decrypt_secret_value` | 593–613 | 15 |  |
| `_resolve_template_role_indices` | 20332–20381 | 15 |  |
| `_format_drilldown_time` | 20751–20774 | 15 |  |
