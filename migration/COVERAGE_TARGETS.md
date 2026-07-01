# Coverable route targets (honest, COVERABLE-filtered)

Generated from coverage_app.json (oracle 84.65%) + coverage_classification.json.
Raw per-route uncovered counts in coverage_backlog.md include DEFENSIVE_EXCEPT / LIBRARY_ERR_TEXT
/ FAULT_INJECTION lines that are structurally deferred. This list keeps ONLY lines the classifier
marked COVERABLE — the schedulable corpus work.

**78 route fns · 276 coverable lines.**

| coverable | route | function | coverable app.py lines |
|---:|---|---|---|
| 44 | `POST /api/ai/helper` | `ai_helper` | 27674,27675,27687,27689,27690,27692,27697,27717,27718,27719,27720,27725… |
| 17 | `GET /tail` | `tail_stream` | 24478,24479,24481,24482,24483,24484,24485,24486,24487,24488,24492,24493… |
| 12 | `POST /settings/tags/auto` | `auto_tag_rules` | 23398,23399,23400,23401,23402,23428,23429,23430,23431,23432,23433,23441 |
| 9 | `POST /api/query/ask` | `api_query_ask` | 30311,30392,30401,30418,30428,30429,30441,30442,30452 |
| 7 | `POST /api/onboarding/list-repos` | `api_onboarding_list_repos` | 33638,33647,33648,33649,33650,33655,33658 |
| 7 | `POST /api/onboarding/import-repo` | `api_onboarding_import_repo` | 33556,33569,33573,33574,33575,33576,33579 |
| 7 | `POST /api/onboarding/create-repo` | `api_onboarding_create_repo` | 33491,33511,33512,33513,33514,33515,33518 |
| 7 | `GET /api/ai/helper/chats` | `ai_helper_chats` | 27483,27484,27485,27486,27487,27488,27489 |
| 6 | `POST /settings/notifications/rules` | `create_notification_rule` | 25992,25998,25999,26000,26002,26025 |
| 6 | `GET /api/traces/span/<span_id>` | `api_raw_span` | 15750,15752,15753,15757,15761,15762 |
| 6 | `POST /api/ai/helper/actions/execute` | `ai_helper_execute_action` | 28408,28418,28420,28422,28427,28432 |
| 5 | `GET /rum` | `view_rum` | 17451,17531,17578,17579,17581 |
| 5 | `GET /ai` | `view_ai` | 18766,18767,18769,18889,18916 |
| 5 | `POST /api/issues/raise` | `raise_issue_from_user_observation` | 28594,28612,28613,28614,28629 |
| 5 | `GET /api/metrics/anomaly` | `metrics_anomaly` | 22620,22621,22622,22626,22627 |
| 5 | `POST /settings/tags` | `create_tag_rule` | 23534,23535,23536,23538,23561 |
| 4 | `GET /logs` | `view_logs` | 11343,11344,11390,11397 |
| 4 | `GET /errors` | `view_errors` | 14410,14445,14500,14525 |
| 4 | `POST /settings/data-management` | `save_dm_settings` | 32223,32225,32229,32230 |
| 4 | `GET /v1/rum/assets/<asset_id>` | `rum_asset_download` | 9770,9771,9775,9779 |
| 4 | `POST /metrics/rules/dashboard/auto` | `auto_metrics_rules_dashboard` | 13887,13888,13905,13906 |
| 3 | `GET /work-items` | `view_work_items` | 18426,18427,18428 |
| 3 | `GET /settings/tags` | `view_tag_rules` | 23287,23288,23289 |
| 3 | `POST /api/agent/runs` | `trigger_agent_run` | 28687,28702,28722 |
| 3 | `POST /errors/<string:error_id>/resolve` | `resolve_error` | 14595,14596,14597 |
| 3 | `POST /v1/metrics` | `ingest_metrics` | 9904,9905,9906 |
| 3 | `GET /health/db` | `health_db` | 26614,26615,26616 |
| 3 | `GET /api/ai/span-attributes` | `get_ai_span_attributes` | 19038,19039,19040 |
| 3 | `GET /api/ai/conversation` | `get_ai_conversation` | 19113,19114,19115 |
| 3 | `POST /api/notifications/vapid-keygen` | `generate_vapid_key` | 26569,26570,26571 |
| 3 | `POST /api/onboarding/create-issues` | `api_onboarding_create_issues` | 33848,33889,33899 |
| 3 | `GET /api/enrichment/cve/findings` | `api_cve_findings` | 18225,18257,18277 |
| 3 | `GET /api/chart-types` | `api_chart_types` | 30997,31011,31012 |
| 3 | `POST /api/dashboards/spec/ai-build` | `ai_build_chart_spec` | 22283,22299,22311 |
| 2 | `GET /settings/repositories` | `view_settings_repositories` | 26810,26833 |
| 2 | `POST /settings/repositories/<app_id>` | `update_settings_repository` | 27073,27074 |
| 2 | `POST /settings/ai` | `save_ai_settings` | 26730,26732 |
| 2 | `POST /settings/repositories/<app_id>/ci-ingest-key/rotate` | `rotate_settings_repository_ci_ingest_key` | 27004,27005 |
| 2 | `POST /api/dashboards/spec/render` | `render_chart_spec_api` | 22015,22017 |
| 2 | `POST /v1/rum/assets` | `ingest_rum_asset` | 9706,9708 |
| 2 | `POST /api/dashboards/<dashboard_id>/charts/import` | `import_chart` | 22490,22498 |
| 2 | `POST /api/dashboards/query` | `execute_chart_query` | 21789,21790 |
| 2 | `POST /api/dashboards/spec/dry-run` | `dry_run_chart_spec_api` | 21925,21926 |
| 2 | `POST /api/dashboards/spec/compile` | `compile_chart_spec_api` | 21899,21900 |
| 2 | `POST /api/notifications/check` | `check_notifications` | 26384,26385 |
| 2 | `POST /metrics/rules/auto` | `auto_metrics_rules` | 13737,13740 |
| 2 | `POST /api/traces/validate-regex` | `api_traces_validate_regex` | 24105,24106 |
| 2 | `POST /api/rum/validate-regex` | `api_rum_validate_regex` | 24219,24220 |
| 2 | `POST /api/query/add-to-dashboard` | `api_query_add_to_dashboard` | 21357,21372 |
| 2 | `POST /api/metrics/validate-regex` | `api_metrics_validate_regex` | 24166,24167 |
| 2 | `POST /api/logs/validate-regex` | `api_logs_validate_regex` | 24003,24004 |
| 2 | `GET /api/logs/field-hints` | `api_logs_field_hints` | 23733,23738 |
| 2 | `GET /api/work-items` | `api_get_work_items` | 18539,18540 |
| 2 | `POST /api/errors/validate-regex` | `api_errors_validate_regex` | 24052,24053 |
| 1 | `GET /kubernetes` | `view_kubernetes` | 31741 |
| 1 | `GET /incident` | `view_incident` | 16092 |
| 1 | `POST /api/notifications/subscribe` | `subscribe_browser_push` | 26516 |
| 1 | `POST /settings/kubernetes` | `save_k8s_settings` | 31727 |
| 1 | `GET /static/rum.js.map` | `rum_js_map` | 23048 |
| 1 | `POST /v1/traces` | `ingest_traces` | 9871 |
| 1 | `POST /v1/rum` | `ingest_rum` | 10013 |
| 1 | `POST /v1/errors` | `ingest_errors` | 10256 |
| 1 | `GET /api/notifications/vapid-public-key` | `get_vapid_public_key` | 26457 |
| 1 | `POST /v1/releases/<release_id>/artifacts/meta` | `create_release_artifact_meta` | 10497 |
| 1 | `POST /api/notifications/rules/auto-generate` | `auto_generate_notification_rules` | 26280 |
| 1 | `GET /api/settings/tags/condition-suggestions` | `api_tag_rule_condition_suggestions` | 23337 |
| 1 | `GET /api/table-explorer/tables` | `api_table_explorer_tables` | 30942 |
| 1 | `GET /api/table-explorer/table/<name>` | `api_table_explorer_table` | 30984 |
| 1 | `POST /api/query/run` | `api_query_run` | 30726 |
| 1 | `GET /api/onboarding/inspect-repo` | `api_onboarding_inspect_repo` | 33718 |
| 1 | `POST /api/reports/import` | `api_import_reports` | 22896 |
| 1 | `GET /api/enrichment/libraries` | `api_enrichment_libraries` | 17940 |
| 1 | `GET /api/enrichment/github/repo-health` | `api_enrichment_github_repo_health` | 18093 |
| 1 | `POST /api/data-management/prune` | `api_dm_prune` | 32315 |
| 1 | `POST /api/data-management/backup/run` | `api_dm_backup_run` | 32270 |
| 1 | `DELETE /api/tags/<record_type>/<record_id>/<tag_key>` | `api_delete_tag` | 23677 |
| 1 | `POST /api/enrichment/cve/scan` | `api_cve_scan` | 18352 |
| 1 | `GET /api/ai/helper/chats/<chat_id>` | `ai_helper_chat_detail` | 27510 |
