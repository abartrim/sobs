package main

// Flask endpoint-name -> URL-rule map, extracted from the Python app's url_map
// (app.url_map.iter_rules()). The Go port registers routes by path only and does
// not track Flask endpoint names, so url_for() needs this table to resolve an
// endpoint name (e.g. "view_logs") to its real path (e.g. "/logs") exactly as
// Flask/Quart does. Without it, templates' url_for(...) links 404.

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var flaskRouteParamRe = regexp.MustCompile(`<([^>]+)>`)

// resolveUrlFor mirrors Flask url_for(endpoint, **values): it substitutes path
// parameters declared in the route pattern (e.g. <dashboard_id>, <path:filename>,
// <string:error_id>) from values, then appends any remaining values as a sorted
// query string. Unknown endpoints fall back to a dasherized path (best effort).
func resolveUrlFor(endpoint string, values map[string]string) string {
	pattern, ok := flaskRoutePatterns[endpoint]
	if !ok {
		pattern = "/" + strings.ReplaceAll(endpoint, "_", "-")
	}
	used := map[string]bool{}
	path := flaskRouteParamRe.ReplaceAllStringFunc(pattern, func(m string) string {
		inner := m[1 : len(m)-1] // strip < >
		converter, name := "", inner
		if i := strings.IndexByte(inner, ':'); i >= 0 {
			converter, name = inner[:i], inner[i+1:]
		}
		used[name] = true
		v := values[name]
		if converter == "path" {
			return v // keep slashes for path: converter
		}
		return url.PathEscape(v)
	})
	keys := make([]string, 0, len(values))
	for k := range values {
		if !used[k] {
			keys = append(keys, k)
		}
	}
	if len(keys) > 0 {
		sort.Strings(keys)
		q := url.Values{}
		for _, k := range keys {
			q.Set(k, values[k])
		}
		path += "?" + q.Encode()
	}
	return path
}

var flaskRoutePatterns = map[string]string{
	"add_chart":                                 "/dashboards/<dashboard_id>/charts",
	"add_masking_key":                           "/settings/masking/keys",
	"add_masking_pattern":                       "/settings/masking/patterns",
	"add_settings_repository_release":           "/settings/repositories/<app_id>/releases",
	"ai_build_chart_spec":                       "/api/dashboards/spec/ai-build",
	"ai_help":                                   "/ai/help",
	"ai_helper":                                 "/api/ai/helper",
	"ai_helper_action_manifest":                 "/api/ai/helper/actions/manifest",
	"ai_helper_capabilities":                    "/api/ai/helper/capabilities",
	"ai_helper_chat_detail":                     "/api/ai/helper/chats/<chat_id>",
	"ai_helper_chats":                           "/api/ai/helper/chats",
	"ai_helper_execute_action":                  "/api/ai/helper/actions/execute",
	"ai_helper_feedback":                        "/api/ai/helper/feedback",
	"api_add_tag":                               "/api/tags/<record_type>/<record_id>",
	"api_ai_field_hints":                        "/api/ai/field-hints",
	"api_ai_validate_filter":                    "/api/ai/validate-filter",
	"api_chart_types":                           "/api/chart-types",
	"api_create_report":                         "/api/reports",
	"api_cve_findings":                          "/api/enrichment/cve/findings",
	"api_cve_scan":                              "/api/enrichment/cve/scan",
	"api_cve_set_disposition":                   "/api/enrichment/cve/findings/<osv_id>/disposition",
	"api_dashboards_list":                       "/api/dashboards/list",
	"api_delete_report":                         "/api/reports/<report_id>",
	"api_delete_tag":                            "/api/tags/<record_type>/<record_id>/<tag_key>",
	"api_dm_backup_list":                        "/api/data-management/backup/list",
	"api_dm_backup_run":                         "/api/data-management/backup/run",
	"api_dm_prune":                              "/api/data-management/prune",
	"api_dm_restore":                            "/api/data-management/restore",
	"api_enrichment_github_repo_health":         "/api/enrichment/github/repo-health",
	"api_enrichment_libraries":                  "/api/enrichment/libraries",
	"api_errors_validate_regex":                 "/api/errors/validate-regex",
	"api_export_reports":                        "/api/reports/export",
	"api_get_tags":                              "/api/tags/<record_type>/<record_id>",
	"api_get_work_items":                        "/api/work-items",
	"api_import_reports":                        "/api/reports/import",
	"api_kubernetes_status":                     "/api/kubernetes/status",
	"api_list_reports":                          "/api/reports",
	"api_logs_field_hints":                      "/api/logs/field-hints",
	"api_logs_validate_filter":                  "/api/logs/validate-filter",
	"api_logs_validate_regex":                   "/api/logs/validate-regex",
	"api_masking_preview":                       "/api/settings/masking/preview",
	"api_masking_rules":                         "/api/settings/masking/rules",
	"api_metrics_validate_regex":                "/api/metrics/validate-regex",
	"api_onboarding_create_issues":              "/api/onboarding/create-issues",
	"api_onboarding_create_repo":                "/api/onboarding/create-repo",
	"api_onboarding_import_repo":                "/api/onboarding/import-repo",
	"api_onboarding_inspect_repo":               "/api/onboarding/inspect-repo",
	"api_onboarding_list_repos":                 "/api/onboarding/list-repos",
	"api_query_add_to_dashboard":                "/api/query/add-to-dashboard",
	"api_query_ask":                             "/api/query/ask",
	"api_query_refine_chart":                    "/api/query/refine-chart",
	"api_query_run":                             "/api/query/run",
	"api_query_schema":                          "/api/query/schema",
	"api_raw_span":                              "/api/traces/span/<span_id>",
	"api_rum_validate_regex":                    "/api/rum/validate-regex",
	"api_setup_wizard_steps":                    "/api/setup-wizard/steps",
	"api_table_explorer_table":                  "/api/table-explorer/table/<name>",
	"api_table_explorer_tables":                 "/api/table-explorer/tables",
	"api_tag_rule_condition_suggestions":        "/api/settings/tags/condition-suggestions",
	"api_traces_validate_regex":                 "/api/traces/validate-regex",
	"api_web_traffic_browsers":                  "/api/web-traffic/browsers",
	"api_web_traffic_devices":                   "/api/web-traffic/devices",
	"api_web_traffic_geo":                       "/api/web-traffic/geo",
	"api_web_traffic_languages":                 "/api/web-traffic/languages",
	"api_web_traffic_os":                        "/api/web-traffic/os",
	"api_web_traffic_timezones":                 "/api/web-traffic/timezones",
	"auto_generate_notification_rules":          "/api/notifications/rules/auto-generate",
	"auto_metrics_rules":                        "/metrics/rules/auto",
	"auto_metrics_rules_dashboard":              "/metrics/rules/dashboard/auto",
	"auto_metrics_rules_help":                   "/metrics/help/rules/auto",
	"auto_tag_rules":                            "/settings/tags/auto",
	"chart_editor_help":                         "/dashboards/help/chart-editor",
	"chart_spec_options_api":                    "/api/dashboards/spec/options",
	"check_notifications":                       "/api/notifications/check",
	"clone_chart":                               "/dashboards/<dashboard_id>/charts/<chart_id>/clone",
	"compile_chart_spec_api":                    "/api/dashboards/spec/compile",
	"create_agent_rule":                         "/settings/agents",
	"create_app_registry_entry":                 "/v1/apps",
	"create_app_release":                        "/v1/apps/<app_id>/releases",
	"create_dashboard":                          "/dashboards",
	"create_metrics_rule":                       "/metrics/rules",
	"create_notification_channel":               "/settings/notifications/channels",
	"create_notification_rule":                  "/settings/notifications/rules",
	"create_release_artifact_meta":              "/v1/releases/<release_id>/artifacts/meta",
	"create_settings_repository":                "/settings/repositories",
	"create_tag_rule":                           "/settings/tags",
	"cve_help":                                  "/cve/help",
	"data_management_help":                      "/settings/help/data-management",
	"delete_agent_rule":                         "/settings/agents/<rule_id>/delete",
	"delete_dashboard":                          "/dashboards/<dashboard_id>/delete",
	"delete_masking_key":                        "/settings/masking/keys/delete",
	"delete_masking_pattern":                    "/settings/masking/patterns/delete",
	"delete_metrics_rule":                       "/metrics/rules/<rule_id>/delete",
	"delete_notification_channel":               "/settings/notifications/channels/<channel_id>/delete",
	"delete_notification_rule":                  "/settings/notifications/rules/<rule_id>/delete",
	"delete_report":                             "/reports/<report_id>/delete",
	"delete_settings_repository":                "/settings/repositories/<app_id>/delete",
	"delete_tag_rule":                           "/settings/tags/<rule_id>/delete",
	"delete_vapid_keys":                         "/api/notifications/vapid-keys",
	"dismiss_agent_run":                         "/api/agent/runs/<run_id>/dismiss",
	"dry_run_chart_spec_api":                    "/api/dashboards/spec/dry-run",
	"edit_chart":                                "/dashboards/<dashboard_id>/charts/<chart_id>/edit",
	"errors_help":                               "/errors/help",
	"execute_chart_query":                       "/api/dashboards/query",
	"export_ai_training":                        "/api/ai/export",
	"export_chart":                              "/api/dashboards/<dashboard_id>/charts/<chart_id>/export",
	"generate_vapid_key":                        "/api/notifications/vapid-keygen",
	"get_ai_conversation":                       "/api/ai/conversation",
	"get_ai_span_attributes":                    "/api/ai/span-attributes",
	"get_app_registry_entry":                    "/v1/apps/<app_id>",
	"get_release":                               "/v1/releases/<release_id>",
	"get_vapid_public_key":                      "/api/notifications/vapid-public-key",
	"health":                                    "/health",
	"health_db":                                 "/health/db",
	"import_chart":                              "/api/dashboards/<dashboard_id>/charts/import",
	"incident_help":                             "/incident/help",
	"ingest_ai":                                 "/v1/ai",
	"ingest_errors":                             "/v1/errors",
	"ingest_logs":                               "/v1/logs",
	"ingest_metrics":                            "/v1/metrics",
	"ingest_preflight":                          "/v1/rum/assets",
	"ingest_rum":                                "/v1/rum",
	"ingest_rum_asset":                          "/v1/rum/assets",
	"ingest_traces":                             "/v1/traces",
	"issue_rum_client_token":                    "/v1/rum/client-token",
	"kubernetes_help":                           "/kubernetes/help",
	"list_agent_runs":                           "/api/agent/runs",
	"list_app_releases":                         "/v1/apps/<app_id>/releases",
	"list_apps":                                 "/v1/apps",
	"list_chart_spec_templates":                 "/api/dashboards/spec/templates",
	"list_dashboards":                           "/dashboards",
	"list_release_artifacts":                    "/v1/releases/<release_id>/artifacts",
	"list_reports":                              "/reports",
	"logs_help":                                 "/logs/help",
	"masking_help":                              "/settings/help/masking",
	"mcp.mcp_api_create_key":                    "/api/mcp/keys",
	"mcp.mcp_api_delete_key":                    "/api/mcp/keys/<key_id>",
	"mcp.mcp_api_list_keys":                     "/api/mcp/keys",
	"mcp.mcp_api_set_enabled":                   "/api/mcp/enabled",
	"mcp.mcp_endpoint":                          "/mcp",
	"mcp.mcp_endpoint_get":                      "/mcp",
	"mcp.mcp_list_tools":                        "/mcp/tools",
	"mcp.mcp_settings_page":                     "/settings/mcp",
	"metrics_anomaly":                           "/api/metrics/anomaly",
	"metrics_anomaly_help":                      "/metrics/help/anomaly",
	"metrics_help":                              "/metrics/help",
	"metrics_rules_help":                        "/metrics/help/rules",
	"new_dashboard_form":                        "/dashboards/new",
	"query_help":                                "/query/help",
	"raise_issue_from_user_observation":         "/api/issues/raise",
	"remove_chart":                              "/dashboards/<dashboard_id>/charts/<chart_id>/delete",
	"render_chart":                              "/api/dashboards/render",
	"render_chart_spec_api":                     "/api/dashboards/spec/render",
	"reports_help":                              "/reports/help",
	"resolve_error":                             "/errors/<string:error_id>/resolve",
	"revoke_settings_repository_ci_ingest_key":  "/settings/repositories/<app_id>/ci-ingest-key/revoke",
	"rotate_settings_repository_ci_ingest_key":  "/settings/repositories/<app_id>/ci-ingest-key/rotate",
	"rum_asset_download":                        "/v1/rum/assets/<asset_id>",
	"rum_d_ts":                                  "/static/rum.d.ts",
	"rum_help":                                  "/rum/help",
	"rum_js":                                    "/static/rum.js",
	"rum_js_map":                                "/static/rum.js.map",
	"rum_min_js":                                "/static/rum.min.js",
	"rum_min_js_map":                            "/static/rum.min.js.map",
	"save_ai_settings":                          "/settings/ai",
	"save_dm_settings":                          "/settings/data-management",
	"save_enrichment_settings":                  "/settings/enrichment",
	"save_k8s_settings":                         "/settings/kubernetes",
	"save_settings_repository_realtime_mode":    "/settings/repositories/<app_id>/realtime-mode",
	"service_worker_js":                         "/service-worker.js",
	"settings_agents_help":                      "/settings/help/agents",
	"settings_ai_help":                          "/settings/help/ai",
	"settings_enrichment_help":                  "/settings/help/enrichment",
	"settings_help":                             "/settings/help",
	"settings_kubernetes_help":                  "/settings/help/kubernetes",
	"settings_notifications_help":               "/settings/help/notifications",
	"settings_repositories_help":                "/settings/help/repositories",
	"settings_tags_help":                        "/settings/help/tags",
	"setup_playbooks_help":                      "/setup/help/playbooks",
	"static":                                    "/static/<path:filename>",
	"subscribe_browser_push":                    "/api/notifications/subscribe",
	"summary":                                   "/",
	"summary_help":                              "/summary/help",
	"table_explorer_help":                       "/table-explorer/help",
	"tail_stream":                               "/tail",
	"test_notification_channel":                 "/api/notifications/channels/<channel_id>/test",
	"toggle_notification_channel":               "/settings/notifications/channels/<channel_id>/toggle",
	"toggle_notification_rule":                  "/settings/notifications/rules/<rule_id>/toggle",
	"traces_help":                               "/traces/help",
	"trigger_agent_run":                         "/api/agent/runs",
	"update_app_registry_entry":                 "/v1/apps/<app_id>",
	"update_masking_output_setting":             "/settings/masking/output",
	"update_masking_sql_output_setting":         "/settings/masking/sql-output",
	"update_settings_repository":                "/settings/repositories/<app_id>",
	"validate_chart_spec_api":                   "/api/dashboards/spec/validate",
	"validate_settings_repository_github_token": "/settings/repositories/github-token/validate",
	"view_agent_rules":                          "/settings/agents",
	"view_ai":                                   "/ai",
	"view_ai_settings":                          "/settings/ai",
	"view_custom_dashboard":                     "/dashboards/<dashboard_id>",
	"view_dm_settings":                          "/settings/data-management",
	"view_enrichment_cve":                       "/enrichment/cve",
	"view_enrichment_settings":                  "/settings/enrichment",
	"view_errors":                               "/errors",
	"view_incident":                             "/incident",
	"view_k8s_settings":                         "/settings/kubernetes",
	"view_kubernetes":                           "/kubernetes",
	"view_logs":                                 "/logs",
	"view_masking_settings":                     "/settings/masking",
	"view_metrics":                              "/metrics",
	"view_metrics_anomaly":                      "/metrics/anomaly",
	"view_metrics_rules":                        "/metrics/rules",
	"view_notifications":                        "/settings/notifications",
	"view_query":                                "/query",
	"view_rum":                                  "/rum",
	"view_settings":                             "/settings",
	"view_settings_repositories":                "/settings/repositories",
	"view_table_explorer":                       "/table-explorer",
	"view_tag_rules":                            "/settings/tags",
	"view_traces":                               "/traces",
	"view_web_traffic":                          "/web-traffic",
	"view_work_items":                           "/work-items",
	"web_traffic_help":                          "/web-traffic/help",
	"work_items_help":                           "/work-items/help",
}
