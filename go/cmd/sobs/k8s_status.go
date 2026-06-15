package main

import (
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/sobs/sobs/internal/jsonenc"
)

// k8sAvgFloat mirrors float(stats.get(key) or 0) where chdb's avg() over an empty set yields nan in
// the Python driver. The Go store returns that empty avg as JSON null, so map null -> NaN to match.
func k8sAvgFloat(m map[string]any, key string) float64 {
	if v, ok := m[key]; !ok || v == nil {
		return math.NaN()
	}
	return cFloat(m, key)
}

// k8s table sort-column maps (mirrors sort_columns in _fetch_k8s_from_otel).
var k8sSortColumns = map[string]map[string]string{
	"nodes":       {"name": "name", "status": "status", "cpu": "cpu_usage", "mem": "mem_used"},
	"deployments": {"namespace": "namespace", "name": "name", "ready": "ready", "desired": "desired"},
	"pods":        {"namespace": "namespace", "name": "name", "phase": "phase", "cpu": "cpu_usage", "mem": "mem_used"},
}

var k8sDefaultSort = map[string]string{"nodes": "name", "deployments": "namespace", "pods": "namespace"}

type k8sTableOpts struct {
	sortKey, sortCol, sortDir string
	page, pageSize, offset    int
}

func k8sQInt(r *http.Request, name string, def, lo, hi int) int {
	v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get(name)))
	if err != nil {
		v = def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// handleApiKubernetesStatus mirrors api_kubernetes_status -> _fetch_k8s_from_otel. Enabled but with
// no k8s metrics, the otel queries return empty rows (and avg() over 0 rows yields NaN), so the
// status is the structured empty result with a "no data found" message.
func (s *server) handleApiKubernetesStatus(w http.ResponseWriter, r *http.Request) {
	if !s.kubernetesEnabled() {
		s.errorJSON(w, http.StatusNotFound, "Kubernetes health view is disabled.")
		return
	}
	opts := map[string]k8sTableOpts{}
	for _, table := range []string{"nodes", "deployments", "pods"} {
		def := k8sDefaultSort[table]
		reqSort := strings.TrimSpace(orDefault(r.URL.Query().Get(table+"_sort"), def))
		sortKey := def
		if _, ok := k8sSortColumns[table][reqSort]; ok {
			sortKey = reqSort
		}
		dir := "asc"
		if strings.ToLower(strings.TrimSpace(r.URL.Query().Get(table+"_dir"))) == "desc" {
			dir = "desc"
		}
		page := k8sQInt(r, table+"_page", 1, 1, 1_000_000)
		pageSize := k8sQInt(r, table+"_page_size", 25, 1, 200)
		opts[table] = k8sTableOpts{sortKey: sortKey, sortCol: k8sSortColumns[table][sortKey],
			sortDir: dir, page: page, pageSize: pageSize, offset: (page - 1) * pageSize}
	}

	nameFilter := strings.TrimSpace(r.URL.Query().Get("name"))
	summary := jsonenc.NewObject().
		Set("nodes_total", 0).Set("nodes_ready", 0).Set("nodes_cpu_avg", 0.0).Set("nodes_mem_used_avg", 0.0).
		Set("pods_total", 0).Set("pods_running", 0).Set("pods_failed", 0).
		Set("pods_cpu_total", 0.0).Set("pods_mem_used_total", 0.0).
		Set("deployments_total", 0).Set("deployments_unhealthy", 0).Set("namespaces_total", 0)
	meta := jsonenc.NewObject()
	for _, table := range []string{"nodes", "deployments", "pods"} {
		o := opts[table]
		meta.Set(table, jsonenc.NewObject().Set("total", 0).Set("sort_key", o.sortKey).
			Set("sort_col", o.sortCol).Set("sort_dir", o.sortDir).Set("page", o.page).
			Set("page_size", o.pageSize).Set("offset", o.offset))
	}
	nodes, deployments, pods, namespaces := []any{}, []any{}, []any{}, []any{}
	errors := []string{}

	format := s.detectK8sMetricFormat()
	source := format
	if format == "none" {
		source = "otel"
	}

	if format != "prometheus" {
		// --- Nodes (otel) ---
		nodeBase := "SELECT Attributes['k8s.node.name'] AS name, " +
			"maxIf(Value, MetricName='k8s.node.condition_ready') AS ready_signal, " +
			"maxIf(Value, MetricName='k8s.node.cpu.usage') AS cpu_usage, " +
			"maxIf(Value, MetricName='k8s.node.memory.usage') AS mem_used, " +
			"any(Attributes['k8s.kubelet.version']) AS version, max(TimeUnix) AS last_seen " +
			nameCond("k8s.node.name", nameFilter) + " GROUP BY name"
		if st, err := s.db.Execute("SELECT count() AS total, countIf(ready_signal > 0) AS ready, " +
			"avg(cpu_usage) AS cpu_avg, avg(mem_used) AS mem_avg FROM (" + nodeBase + ")"); err == nil && len(st.Rows) > 0 {
			m := rowMaps(st)[0]
			metaTable(meta, "nodes").Set("total", cInt(m, "total"))
			summary.Set("nodes_total", cInt(m, "total")).Set("nodes_ready", cInt(m, "ready")).
				Set("nodes_cpu_avg", k8sAvgFloat(m, "cpu_avg")).Set("nodes_mem_used_avg", k8sAvgFloat(m, "mem_avg"))
		} else if err != nil {
			errors = append(errors, "nodes: "+err.Error())
		}
		if rows, err := s.db.Execute("SELECT * FROM (" + nodeBase + ") ORDER BY " + opts["nodes"].sortCol + " " +
			strings.ToUpper(opts["nodes"].sortDir) + " LIMIT " + itoaInt(opts["nodes"].pageSize) + " OFFSET " + itoaInt(opts["nodes"].offset)); err == nil {
			for _, m := range rowMaps(rows) {
				status := "NotReady"
				if cFloat(m, "ready_signal") > 0 {
					status = "Ready"
				}
				nodes = append(nodes, jsonenc.NewObject().Set("name", cStr(m, "name")).Set("status", status).
					Set("version", cStr(m, "version")).Set("cpu_usage", cFloat(m, "cpu_usage")).
					Set("mem_used", cFloat(m, "mem_used")).Set("created", cStr(m, "last_seen")))
			}
		}

		// --- Pods (otel) ---
		podBase := "SELECT Attributes['k8s.namespace.name'] AS namespace, Attributes['k8s.pod.name'] AS name, " +
			"any(Attributes['k8s.pod.phase']) AS phase, maxIf(Value, MetricName='k8s.pod.status_ready') AS ready_signal, " +
			"maxIf(Value, MetricName='k8s.pod.cpu.usage') AS cpu_usage, maxIf(Value, MetricName='k8s.pod.memory.usage') AS mem_used, " +
			"maxIf(toInt64(Value), MetricName='k8s.container.restart_count') AS restarts, " +
			"any(Attributes['k8s.node.name']) AS node, max(TimeUnix) AS last_seen " +
			nameCond("k8s.pod.name", nameFilter) + " GROUP BY namespace, name"
		if st, err := s.db.Execute("SELECT count() AS total, countIf(phase='Running') AS running, " +
			"countIf(phase='Failed') AS failed, sum(cpu_usage) AS cpu_total, sum(mem_used) AS mem_total FROM (" + podBase + ")"); err == nil && len(st.Rows) > 0 {
			m := rowMaps(st)[0]
			metaTable(meta, "pods").Set("total", cInt(m, "total"))
			summary.Set("pods_total", cInt(m, "total")).Set("pods_running", cInt(m, "running")).
				Set("pods_failed", cInt(m, "failed")).Set("pods_cpu_total", cFloat(m, "cpu_total")).
				Set("pods_mem_used_total", cFloat(m, "mem_total"))
		} else if err != nil {
			errors = append(errors, "pods: "+err.Error())
		}
		if rows, err := s.db.Execute("SELECT * FROM (" + podBase + ") ORDER BY " + opts["pods"].sortCol + " " +
			strings.ToUpper(opts["pods"].sortDir) + " LIMIT " + itoaInt(opts["pods"].pageSize) + " OFFSET " + itoaInt(opts["pods"].offset)); err == nil {
			for _, m := range rowMaps(rows) {
				pods = append(pods, jsonenc.NewObject().Set("namespace", orDefault(cStr(m, "namespace"), "default")).
					Set("name", cStr(m, "name")).Set("phase", orDefault(cStr(m, "phase"), "Unknown")).
					Set("ready", cFloat(m, "ready_signal") > 0).Set("cpu_usage", cFloat(m, "cpu_usage")).
					Set("mem_used", cFloat(m, "mem_used")).Set("restarts", cInt(m, "restarts")).
					Set("node", cStr(m, "node")).Set("created", cStr(m, "last_seen")))
			}
		}

		// --- Deployments (otel) ---
		deployBase := "SELECT Attributes['k8s.namespace.name'] AS namespace, Attributes['k8s.deployment.name'] AS name, " +
			"maxIf(toInt64(Value), MetricName='k8s.deployment.desired') AS desired, " +
			"maxIf(toInt64(Value), MetricName='k8s.deployment.ready') AS ready, " +
			"maxIf(toInt64(Value), MetricName='k8s.deployment.available') AS available, " +
			"maxIf(toInt64(Value), MetricName='k8s.deployment.updated') AS updated, max(TimeUnix) AS last_seen " +
			nameCond("k8s.deployment.name", nameFilter) + " GROUP BY namespace, name"
		dTotal := s.countRows("SELECT count(*) FROM (" + deployBase + ")")
		metaTable(meta, "deployments").Set("total", dTotal)
		summary.Set("deployments_total", dTotal).
			Set("deployments_unhealthy", s.countRows("SELECT count(*) FROM ("+deployBase+") WHERE ready < desired"))
		if rows, err := s.db.Execute("SELECT * FROM (" + deployBase + ") ORDER BY " + opts["deployments"].sortCol + " " +
			strings.ToUpper(opts["deployments"].sortDir) + " LIMIT " + itoaInt(opts["deployments"].pageSize) + " OFFSET " + itoaInt(opts["deployments"].offset)); err == nil {
			for _, m := range rowMaps(rows) {
				deployments = append(deployments, jsonenc.NewObject().
					Set("namespace", orDefault(cStr(m, "namespace"), "default")).Set("name", cStr(m, "name")).
					Set("desired", cInt(m, "desired")).Set("ready", cInt(m, "ready")).
					Set("available", cInt(m, "available")).Set("updated", cInt(m, "updated")).Set("created", cStr(m, "last_seen")))
			}
		}

		// --- Namespaces (otel) ---
		if rows, err := s.db.Execute("SELECT Attributes['k8s.namespace.name'] AS name, max(TimeUnix) AS last_seen " +
			"FROM otel_metrics_gauge WHERE Attributes['k8s.namespace.name'] != '' GROUP BY name ORDER BY name"); err == nil {
			for _, m := range rowMaps(rows) {
				namespaces = append(namespaces, jsonenc.NewObject().Set("name", cStr(m, "name")).
					Set("status", "Active").Set("created", cStr(m, "last_seen")))
			}
		}
		summary.Set("namespaces_total", len(namespaces))
	}

	errStr := ""
	if len(errors) > 0 {
		errStr = strings.Join(errors, "; ")
	} else if len(pods) == 0 && len(deployments) == 0 && len(nodes) == 0 && len(namespaces) == 0 {
		errStr = "No Kubernetes data found yet. Deploy OTEL collectors (kubeletstats/k8s_cluster) or" +
			" configure an OTEL Prometheus receiver scraping kube-state-metrics and cAdvisor."
	}

	writeJSON(w, http.StatusOK, jsonenc.NewObject().
		Set("ok", true).Set("source", source).Set("error", errStr).
		Set("nodes", nodes).Set("deployments", deployments).Set("pods", pods).Set("namespaces", namespaces).
		Set("meta", meta).Set("summary", summary))
}

// detectK8sMetricFormat mirrors _detect_k8s_metric_format.
func (s *server) detectK8sMetricFormat() string {
	if s.countRows("SELECT count() FROM otel_metrics_gauge WHERE Attributes['k8s.node.name'] != '' LIMIT 1") > 0 {
		return "otel"
	}
	promFilter := "(MetricName LIKE 'kube_%' OR MetricName IN ('container_memory_working_set_bytes'))"
	for _, table := range []string{"otel_metrics_gauge", "otel_metrics_sum"} {
		if s.countRows("SELECT count() FROM "+table+" WHERE "+promFilter+" LIMIT 1") > 0 {
			return "prometheus"
		}
	}
	return "none"
}

func nameCond(attr, nameFilter string) string {
	cond := " FROM otel_metrics_gauge WHERE Attributes['" + attr + "'] != ''"
	if nameFilter != "" {
		cond += " AND positionCaseInsensitive(Attributes['" + attr + "'], " + sqlQuoteLiteral(nameFilter) + ") > 0"
	}
	return cond
}

// metaTable returns the nested per-table meta object.
func metaTable(meta *jsonenc.Object, key string) *jsonenc.Object {
	if v, ok := meta.Get(key); ok {
		if o, ok := v.(*jsonenc.Object); ok {
			return o
		}
	}
	return jsonenc.NewObject()
}
