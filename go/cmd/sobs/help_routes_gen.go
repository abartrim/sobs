package main

// Code generated from migration/manifest/help_routes.json. DO NOT EDIT.
// The dynamically-registered *_help pages (app.py _register_help_route). All share
// base.html + the template engine; each renders its own *_help.html template.
var helpRoutes = []struct{ Path, Endpoint, Template string }{
	{"/ai/help", "ai_help", "ai_help.html"},
	{"/cve/help", "cve_help", "cve_help.html"},
	{"/dashboards/help/chart-editor", "chart_editor_help", "chart_editor_help.html"},
	{"/errors/help", "errors_help", "errors_help.html"},
	{"/incident/help", "incident_help", "incident_help.html"},
	{"/kubernetes/help", "kubernetes_help", "kubernetes_help.html"},
	{"/logs/help", "logs_help", "logs_help.html"},
	{"/metrics/help", "metrics_help", "metrics_help.html"},
	{"/metrics/help/anomaly", "metrics_anomaly_help", "metrics_anomaly_help.html"},
	{"/metrics/help/rules", "metrics_rules_help", "metrics_rules_help.html"},
	{"/metrics/help/rules/auto", "auto_metrics_rules_help", "auto_metrics_rules_help.html"},
	{"/query/help", "query_help", "query_help.html"},
	{"/reports/help", "reports_help", "reports_help.html"},
	{"/rum/help", "rum_help", "rum_help.html"},
	{"/settings/help", "settings_help", "settings_help.html"},
	{"/settings/help/agents", "settings_agents_help", "settings_agents_help.html"},
	{"/settings/help/ai", "settings_ai_help", "settings_ai_help.html"},
	{"/settings/help/data-management", "data_management_help", "data_management_help.html"},
	{"/settings/help/enrichment", "settings_enrichment_help", "settings_enrichment_help.html"},
	{"/settings/help/kubernetes", "settings_kubernetes_help", "kubernetes_help.html"},
	{"/settings/help/masking", "masking_help", "masking_help.html"},
	{"/settings/help/notifications", "settings_notifications_help", "settings_notifications_help.html"},
	{"/settings/help/repositories", "settings_repositories_help", "settings_repositories_help.html"},
	{"/settings/help/tags", "settings_tags_help", "settings_tags_help.html"},
	{"/setup/help/playbooks", "setup_playbooks_help", "setup_playbooks_help.html"},
	{"/summary/help", "summary_help", "summary_help.html"},
	{"/table-explorer/help", "table_explorer_help", "table_explorer_help.html"},
	{"/traces/help", "traces_help", "traces_help.html"},
	{"/web-traffic/help", "web_traffic_help", "web_traffic_help.html"},
	{"/work-items/help", "work_items_help", "work_items_help.html"},
}
