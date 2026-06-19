# app.py uncovered-line backlog

Oracle coverage **64%** · **5377** uncovered statements.

## By bucket

| bucket | functions | uncovered lines | meaning |
|---|---:|---:|---|
| route | 123 | 967 | needs a fixture/profile (corpus expansion; byte-verifiable) |
| helper | 436 | 4141 | usually covered when a calling route's fixture is added; else difftest |
| lifecycle | 21 | 256 | background/lifecycle — needs a function-level difftest (capture can't reach) |
| module | — | 13 | top-level/startup/defensive — mostly dead, classify+exclude |

## route — top by uncovered lines (123 functions)

| function | lines | uncovered | route |
|---|---|---:|---|
| `ai_helper` | 27607–28395 | 58 | `POST /api/ai/helper` |
| `view_incident` | 15785–16144 | 39 | `GET /incident` |
| `auto_metrics_rules_dashboard` | 13848–13960 | 26 | `POST /metrics/rules/dashboard/auto` |
| `view_rum` | 17309–17657 | 26 | `GET /rum` |
| `api_onboarding_create_issues` | 33738–33930 | 25 | `POST /api/onboarding/create-issues` |
| `create_notification_rule` | 25918–26089 | 23 | `POST /settings/notifications/rules` |
| `view_traces` | 15312–15678 | 22 | `GET /traces` |
| `get_ai_conversation` | 19048–19115 | 21 | `GET /api/ai/conversation` |
| `issue_rum_client_token` | 9791–9826 | 20 | `POST /v1/rum/client-token` |
| `view_logs` | 11239–11512 | 20 | `GET /logs` |
| `view_work_items` | 18361–18488 | 20 | `GET /work-items` |
| `api_import_reports` | 22857–23017 | 19 | `POST /api/reports/import` |
| `view_enrichment_cve` | 18104–18215 | 18 | `GET /enrichment/cve` |
| `view_ai` | 18631–18998 | 18 | `GET /ai` |
| `api_query_run` | 30511–30765 | 17 | `POST /api/query/run` |
| `summary` | 10841–10971 | 16 | `GET /` |
| `auto_tag_rules` | 23353–23460 | 16 | `POST /settings/tags/auto` |
| `check_notifications` | 26341–26447 | 16 | `POST /api/notifications/check` |
| `clone_chart` | 21694–21733 | 15 | `POST /dashboards/<dashboard_id>/charts/<chart_id>/clone` |
| `create_notification_channel` | 25742–25813 | 15 | `POST /settings/notifications/channels` |
| `view_settings_repositories` | 26805–26884 | 15 | `GET /settings/repositories` |
| `ingest_rum_asset` | 9699–9756 | 14 | `POST /v1/rum/assets` |
| `ingest_ai` | 10157–10241 | 14 | `POST /v1/ai` |
| `api_enrichment_libraries` | 17882–17941 | 14 | `GET /api/enrichment/libraries` |
| `edit_chart` | 21651–21689 | 14 | `POST /dashboards/<dashboard_id>/charts/<chart_id>/edit` |
| `api_get_work_items` | 18493–18541 | 13 | `GET /api/work-items` |
| `api_logs_validate_filter` | 23784–23855 | 13 | `POST /api/logs/validate-filter` |
| `api_ai_validate_filter` | 24407–24454 | 13 | `POST /api/ai/validate-filter` |
| `rum_asset_download` | 9761–9786 | 12 | `GET /v1/rum/assets/<asset_id>` |
| `api_metrics_validate_regex` | 24115–24167 | 11 | `POST /api/metrics/validate-regex` |
| `create_settings_repository` | 26889–26951 | 11 | `POST /settings/repositories` |
| `api_query_ask` | 30203–30506 | 11 | `POST /api/query/ask` |
| `api_cve_findings` | 18220–18278 | 10 | `GET /api/enrichment/cve/findings` |
| `create_tag_rule` | 23465–23584 | 10 | `POST /settings/tags` |
| `api_onboarding_list_repos` | 33603–33678 | 10 | `POST /api/onboarding/list-repos` |
| `ingest_metrics` | 9896–9918 | 9 | `POST /v1/metrics` |
| `chart_spec_options_api` | 21816–21887 | 9 | `GET /api/dashboards/spec/options` |
| `render_chart_spec_api` | 21986–22031 | 9 | `POST /api/dashboards/spec/render` |
| `get_ai_span_attributes` | 19003–19040 | 8 | `GET /api/ai/span-attributes` |
| `export_ai_training` | 19123–19226 | 8 | `GET /api/ai/export` |

## lifecycle — top by uncovered lines (21 functions)

| function | lines | uncovered | route |
|---|---|---:|---|
| `_run_raw_window_copy_worker` | 2496–2649 | 67 |  |
| `_dispatch_browser_push_channel` | 24895–24960 | 39 |  |
| `_dispatch_email_channel` | 24858–24892 | 26 |  |
| `_shutdown_async_http_client` | 326–352 | 25 |  |
| `ensure_db_schema` | 2135–2152 | 11 |  |
| `_raw_window_copy_loop` | 2652–2667 | 9 |  |
| `_run_agent_flow` | 6772–6999 | 9 |  |
| `_cve_scanner_loop` | 17227–17241 | 9 |  |
| `_dispatch_notification_channel` | 25058–25075 | 9 |  |
| `_run_cve_scan` | 17122–17224 | 8 |  |
| `_dispatch_webhook_channel` | 24814–24839 | 8 |  |
| `_github_repo_health_loop` | 17244–17252 | 7 |  |
| `_dispatch_slack_channel` | 24842–24855 | 7 |  |
| `_run_agent_rule_instance` | 25426–25488 | 5 |  |
| `_write_worker_main` | 7427–7447 | 4 |  |
| `_startup_async_http_client` | 319–322 | 3 |  |
| `_startup_enrichment` | 17296–17301 | 3 |  |
| `_ensure_raw_metrics_retention` | 2360–2376 | 2 |  |
| `_ensure_notification_schema` | 7002–7011 | 2 |  |
| `_refresh_masking_rules_before_request` | 25660–25666 | 2 |  |
| `_apply_security_headers` | 459–485 | 1 |  |

## helper — top by uncovered lines (436 functions)

| function | lines | uncovered | route |
|---|---|---:|---|
| `_generate` | 27807–28123 | 94 |  |
| `_fetch_k8s_from_otel` | 31071–31697 | 92 |  |
| `_backfill_github_work_item_links` | 6030–6189 | 82 |  |
| `_github_actions_dependency_rows` | 16479–16629 | 71 |  |
| `_build_ai_trace_turn_cards` | 8506–8644 | 61 |  |
| `_normalize_generic_ui_action_tool_call` | 4399–4529 | 56 |  |
| `_seed_app_release_registry_from_env` | 8935–9063 | 54 |  |
| `_build_seasonal_metric_rule_candidates` | 11845–12019 | 52 |  |
| `_sourcemap_lookup_for_file` | 7970–8027 | 49 |  |
| `_genai_message_content_to_text` | 8242–8298 | 48 |  |
| `_call_llm_endpoint` | 4615–4770 | 46 |  |
| `_choose_github_issue_outcome` | 5388–5646 | 44 |  |
| `_parse_oss_safeguard_reply` | 5042–5092 | 43 |  |
| `_stream_llm_endpoint` | 4773–4887 | 41 |  |
| `_compute_advanced_log_analysis` | 11015–11107 | 41 |  |
| `_fetch_release_deps_from_github` | 16703–16873 | 41 |  |
| `_verify_rum_client_auth` | 7827–7878 | 40 |  |
| `_collect_github_repo_health_summary` | 17944–18083 | 39 |  |
| `_evaluate_seasonal_rule` | 13020–13089 | 38 |  |
| `_parse_package_lock_dependencies` | 16358–16399 | 38 |  |
| `_build_series_sql` | 19964–20181 | 38 |  |
| `_normalize_notification_condition` | 24626–24679 | 34 |  |
| `_vanna_validate_and_execute_with_repair` | 22071–22153 | 33 |  |
| `_evaluate_tag_condition` | 25127–25175 | 33 |  |
| `_check_notification_rule` | 25185–25332 | 33 |  |
| `decorated` | 7890–7939 | 32 |  |
| `_query_timeseries` | 15030–15089 | 32 |  |
| `_check_guard_model` | 5136–5233 | 31 |  |
| `_update_github_issue_record` | 32994–33047 | 31 |  |
| `_assign_issue_to_copilot` | 5329–5385 | 30 |  |
| `_coerce_reasoning_text` | 8304–8331 | 28 |  |
| `_geo_lookup_batch` | 16194–16241 | 28 |  |
| `_resolve_custom_binding_expr` | 21086–21119 | 28 |  |
| `_parse_gemfile_lock_dependencies` | 16426–16453 | 27 |  |
| `_encrypt_push_payload` | 24990–25055 | 27 |  |
| `_match_single_condition` | 12551–12588 | 26 |  |
| `_build_trace_window_overlay_segments` | 14787–14842 | 26 |  |
| `_collect_library_inventory` | 16876–17071 | 26 |  |
| `_load_chat_tool_history` | 4016–4075 | 25 |  |
| `_build_auto_tag_rule_candidates` | 12062–12308 | 25 |  |
