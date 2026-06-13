package main

import (
	"net/http"
	"strconv"
	"strings"
)

// HTML page handlers. Pages render Jinja templates via the render engine; feature-gated
// pages return a plain 404 string when disabled (the fixture state for query/k8s pages).

// textStatus writes a plain text/html string body at the given status (Quart's behavior
// for a `(str, code)` handler return).
func textStatus(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// renderPage renders an HTML template with baseContext merged with extra context vars and
// writes it with Quart's text/html content type.
func (s *server) renderPage(w http.ResponseWriter, templateName, endpoint string, extra map[string]any) {
	ctx := s.baseContext(endpoint)
	for k, v := range extra {
		ctx[k] = v
	}
	out, err := s.newEngine().Render(templateName, ctx)
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

// GET /query — app.py view_query: 404 string when the query page is disabled (fixture).
func (s *server) handleViewQuery(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.QueryPageEnabled {
		textStatus(w, http.StatusNotFound, "Query page is unavailable until AI and guard settings are configured.")
		return
	}
	s.renderPage(w, "query.html", "view_query", nil)
}

// GET /table-explorer — app.py view_table_explorer: 404 string when disabled (fixture).
func (s *server) handleViewTableExplorer(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.QueryPageEnabled {
		textStatus(w, http.StatusNotFound, "Table Explorer is unavailable until AI and guard settings are configured.")
		return
	}
	s.renderPage(w, "table_explorer.html", "view_table_explorer", nil)
}

// GET /dashboards — app.py list_dashboards: render custom_dashboards.html with the
// non-deleted dashboards (_get_dashboards).
func (s *server) handleListDashboards(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute(
		"SELECT Id, Name, Description FROM sobs_dashboards FINAL WHERE IsDeleted = 0 ORDER BY Name")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dashboards := []any{}
	for _, m := range rowMaps(res) {
		dashboards = append(dashboards, map[string]any{
			"id": cStr(m, "Id"), "name": cStr(m, "Name"), "description": cStr(m, "Description"),
		})
	}
	s.renderPage(w, "custom_dashboards.html", "list_dashboards", map[string]any{"dashboards": dashboards})
}

// GET /reports — app.py list_reports: render reports.html with all reports (_get_reports).
func (s *server) handleListReportsPage(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.Execute("SELECT Id, Name, Description, PageType, FiltersJson " +
		"FROM sobs_reports FINAL WHERE IsDeleted = 0 ORDER BY PageType, Name")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reports := []any{}
	for _, m := range rowMaps(res) {
		reports = append(reports, map[string]any{
			"id": cStr(m, "Id"), "name": cStr(m, "Name"), "description": cStr(m, "Description"),
			"page_type": cStr(m, "PageType"), "filters": parseReportFiltersNative(cStr(m, "FiltersJson")),
		})
	}
	s.renderPage(w, "reports.html", "list_reports", map[string]any{"reports": reports})
}

// GET /settings/kubernetes — app.py view_k8s_settings: render with k8s settings + flash.
func (s *server) handleViewK8sSettings(w http.ResponseWriter, r *http.Request) {
	val, _ := s.appSetting("kubernetes.enabled")
	msgType := r.URL.Query().Get("msg_type")
	if msgType == "" {
		msgType = "success"
	}
	s.renderPage(w, "settings_kubernetes.html", "view_k8s_settings", map[string]any{
		"k8s_settings": map[string]any{"kubernetes.enabled": val},
		"flash_msg":    r.URL.Query().Get("msg"),
		"flash_type":   msgType,
	})
}

// GET /settings/enrichment — app.py view_enrichment_settings: geo/cve flags + backfill.
func (s *server) handleViewEnrichmentSettings(w http.ResponseWriter, r *http.Request) {
	cveLastScan, _ := s.appSetting("enrichment.cve_last_scan")
	maxRel := 300
	if raw, ok := s.appSetting("enrichment.github_backfill_max_releases"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			if n < 1 {
				n = 1
			} else if n > 2000 {
				n = 2000
			}
			maxRel = n
		}
	}
	s.renderPage(w, "settings_enrichment.html", "view_enrichment_settings", map[string]any{
		"geo_enabled":                        s.appSettingBool("enrichment.geo_enabled", true),
		"cve_enabled":                        s.appSettingBool("enrichment.cve_enabled", true),
		"cve_last_scan":                      cveLastScan,
		"github_backfill_max_releases":       maxRel,
		"github_backfill_min_releases":       1,
		"github_backfill_max_releases_limit": 2000,
	})
}

// GET /kubernetes — app.py view_kubernetes: 404 string when k8s is disabled (fixture).
func (s *server) handleViewKubernetes(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.KubernetesEnabled {
		textStatus(w, http.StatusNotFound, "Kubernetes health view is disabled. Enable it in Settings → Kubernetes.")
		return
	}
	s.renderPage(w, "kubernetes.html", "view_kubernetes", nil)
}

// GET /dashboards/new — app.py new_dashboard_form: render custom_dashboards.html with an
// empty dashboards list and the new-form flag.
func (s *server) handleNewDashboardForm(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "custom_dashboards.html", "new_dashboard_form", map[string]any{
		"dashboards":    []any{},
		"show_new_form": true,
	})
}
