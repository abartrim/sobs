# app.py uncovered-line backlog

Oracle coverage **85%** · **2284** uncovered statements.

## By bucket

| bucket | functions | uncovered lines | meaning |
|---|---:|---:|---|
| route | 88 | 368 | needs a fixture/profile (corpus expansion; byte-verifiable) |
| helper | 372 | 1691 | usually covered when a calling route's fixture is added; else difftest |
| lifecycle | 21 | 212 | background/lifecycle — needs a function-level difftest (capture can't reach) |
| module | — | 13 | top-level/startup/defensive — mostly dead, classify+exclude |

## route — top by uncovered lines (88 functions)

| function | lines | uncovered | route |
|---|---|---:|---|
| `ai_helper` | 27607–28395 | 29 | `POST /api/ai/helper` |
| `view_incident` | 15785–16144 | 22 | `GET /incident` |
| `auto_tag_rules` | 23353–23460 | 16 | `POST /settings/tags/auto` |
| `view_traces` | 15312–15678 | 10 | `GET /traces` |
| `view_rum` | 17309–17657 | 9 | `GET /rum` |
| `api_query_ask` | 30203–30506 | 9 | `POST /api/query/ask` |
| `ingest_metrics` | 9896–9918 | 8 | `POST /v1/metrics` |
| `api_ai_field_hints` | 24229–24402 | 8 | `GET /api/ai/field-hints` |
| `api_onboarding_import_repo` | 33538–33598 | 8 | `POST /api/onboarding/import-repo` |
| `api_onboarding_list_repos` | 33603–33678 | 8 | `POST /api/onboarding/list-repos` |
| `view_logs` | 11239–11512 | 7 | `GET /logs` |
| `create_notification_rule` | 25918–26089 | 7 | `POST /settings/notifications/rules` |
| `check_notifications` | 26341–26447 | 7 | `POST /api/notifications/check` |
| `ai_helper_chats` | 27448–27502 | 7 | `GET /api/ai/helper/chats` |
| `api_onboarding_create_repo` | 33464–33533 | 7 | `POST /api/onboarding/create-repo` |
| `ingest_traces` | 9834–9888 | 6 | `POST /v1/traces` |
| `ingest_rum` | 10010–10149 | 6 | `POST /v1/rum` |
| `ingest_errors` | 10249–10295 | 6 | `POST /v1/errors` |
| `api_raw_span` | 15695–15764 | 6 | `GET /api/traces/span/<span_id>` |
| `create_tag_rule` | 23465–23584 | 6 | `POST /settings/tags` |
| `ai_helper_execute_action` | 28400–28468 | 6 | `POST /api/ai/helper/actions/execute` |
| `ingest_logs` | 9665–9694 | 5 | `POST /v1/logs` |
| `ingest_ai` | 10157–10241 | 5 | `POST /v1/ai` |
| `view_work_items` | 18361–18488 | 5 | `GET /work-items` |
| `api_logs_field_hints` | 23707–23779 | 5 | `GET /api/logs/field-hints` |
| `raise_issue_from_user_observation` | 28547–28654 | 5 | `POST /api/issues/raise` |
| `rum_asset_download` | 9761–9786 | 4 | `GET /v1/rum/assets/<asset_id>` |
| `auto_metrics_rules_dashboard` | 13848–13960 | 4 | `POST /metrics/rules/dashboard/auto` |
| `view_errors` | 14210–14583 | 4 | `GET /errors` |
| `api_cve_findings` | 18220–18278 | 4 | `GET /api/enrichment/cve/findings` |
| `api_query_add_to_dashboard` | 21331–21407 | 4 | `POST /api/query/add-to-dashboard` |
| `save_dm_settings` | 32215–32248 | 4 | `POST /settings/data-management` |
| `view_metrics` | 13430–13595 | 3 | `GET /metrics` |
| `view_metrics_anomaly` | 14010–14172 | 3 | `GET /metrics/anomaly` |
| `resolve_error` | 14588–14598 | 3 | `POST /errors/<string:error_id>/resolve` |
| `api_get_work_items` | 18493–18541 | 3 | `GET /api/work-items` |
| `get_ai_span_attributes` | 19003–19040 | 3 | `GET /api/ai/span-attributes` |
| `get_ai_conversation` | 19048–19115 | 3 | `GET /api/ai/conversation` |
| `execute_chart_query` | 21772–21791 | 3 | `POST /api/dashboards/query` |
| `compile_chart_spec_api` | 21892–21902 | 3 | `POST /api/dashboards/spec/compile` |

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

## helper — top by uncovered lines (372 functions)

| function | lines | uncovered | route |
|---|---|---:|---|
| `_choose_github_issue_outcome` | 5388–5646 | 25 |  |
| `_run_dm_backup` | 32134–32170 | 24 |  |
| `_shutdown_db_resources` | 7484–7510 | 21 |  |
| `_github_actions_dependency_rows` | 16479–16629 | 21 |  |
| `_generate` | 27807–28123 | 20 |  |
| `_stream_llm_endpoint` | 4773–4887 | 19 |  |
| `_fetch_release_deps_from_github` | 16703–16873 | 18 |  |
| `_build_user_issue_trigger_context` | 28476–28542 | 18 |  |
| `_fetch_k8s_from_otel` | 31071–31697 | 18 |  |
| `_backfill_github_work_item_links` | 6030–6189 | 17 |  |
| `_generate` | 24481–24498 | 17 |  |
| `_validate_custom_masking_pattern_for_storage` | 186–245 | 14 |  |
| `_coerce_reasoning_text` | 8304–8331 | 14 |  |
| `_collect_library_inventory` | 16876–17071 | 14 |  |
| `_build_s3_backup_dest` | 32086–32106 | 14 |  |
| `_embedding_from_json` | 3650–3666 | 13 |  |
| `_sanitize_chat_label_candidate` | 3718–3736 | 13 |  |
| `_build_agent_context_summary` | 6609–6703 | 13 |  |
| `_proto_any_value_to_python` | 9080–9097 | 13 |  |
| `_vanna_refine_chart_spec` | 30015–30086 | 13 |  |
| `_load_saved_ai_pricing` | 2776–2792 | 12 |  |
| `_load_confirmed_ai_pricing_models` | 2795–2810 | 12 |  |
| `_assign_issue_to_copilot` | 5329–5385 | 12 |  |
| `_genai_message_content_to_text` | 8242–8298 | 12 |  |
| `_extract_bindings` | 20525–20748 | 12 |  |
| `_validate_chdb_startup_configuration` | 1909–1931 | 11 |  |
| `_load_recent_turn_summaries` | 3928–3971 | 11 |  |
| `_to_utc_iso` | 5863–5883 | 11 |  |
| `_create_github_issue_record` | 6380–6435 | 11 |  |
| `_dedupe_system_input_messages` | 8450–8467 | 11 |  |
| `_inspect_repo_for_onboarding` | 32883–32959 | 11 |  |
| `_verify_rum_client_auth` | 7827–7878 | 10 |  |
| `_sourcemap_lookup_for_file` | 7970–8027 | 10 |  |
| `_safe_json_dumps` | 8813–8827 | 10 |  |
| `_evaluate_composite_rule` | 13151–13224 | 10 |  |
| `_get_ai_filter_metadata` | 18547–18626 | 10 |  |
| `_normalize_notification_condition` | 24626–24679 | 10 |  |
| `_validate_dm_s3_settings` | 31866–31877 | 10 |  |
| `_apply_dm_ttl` | 31938–31974 | 10 |  |
| `_run_dm_prune` | 32024–32083 | 10 |  |
