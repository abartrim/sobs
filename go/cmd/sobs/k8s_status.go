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

// k8s table sort-column maps (mirrors sort_columns in _fetch_k8s_from_otel, app.py:31120-31144).
var k8sSortColumns = map[string]map[string]string{
	"nodes": {
		"name":    "name",
		"status":  "status",
		"version": "version",
		"created": "last_seen",
	},
	"deployments": {
		"namespace": "namespace",
		"name":      "name",
		"desired":   "desired",
		"ready":     "ready",
		"available": "available",
		"created":   "last_seen",
	},
	"pods": {
		"namespace": "namespace",
		"name":      "name",
		"phase":     "phase",
		"ready":     "ready_signal",
		"restarts":  "restarts",
		"node":      "node",
		"created":   "last_seen",
	},
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

// k8sNormalizedValues mirrors [v.strip() for v in request.args.getlist(name) if v.strip()].
func k8sNormalizedValues(r *http.Request, name string) []string {
	out := []string{}
	for _, v := range r.URL.Query()[name] {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// k8sAppendOrEquals mirrors _append_or_equals (app.py:31099-31104): appends a
// "(field = ? OR field = ? …)" clause and extends params with the values.
func k8sAppendOrEquals(conditions *[]string, params *[]any, fieldSQL string, values []string) {
	if len(values) == 0 {
		return
	}
	parts := make([]string, len(values))
	for i := range values {
		parts[i] = fieldSQL + " = ?"
		*params = append(*params, values[i])
	}
	*conditions = append(*conditions, "("+strings.Join(parts, " OR ")+")")
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

	// Multi-value filters (app.py:31106-31113). request.args.getlist("namespace") / "node" / …
	nameFilter := strings.TrimSpace(r.URL.Query().Get("name"))
	namespaceFilter := strings.TrimSpace(r.URL.Query().Get("namespace"))
	namespaceValues := k8sNormalizedValues(r, "namespace")
	if len(namespaceValues) == 0 && namespaceFilter != "" {
		namespaceValues = []string{namespaceFilter}
	}
	nodeValues := k8sNormalizedValues(r, "node")
	deploymentValues := k8sNormalizedValues(r, "deployment")
	podValues := k8sNormalizedValues(r, "pod")

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
	prometheus := format == "prometheus"

	// --- Nodes ---
	func() {
		var nodeConditions []string
		var nodeParams []any
		var nodeAttr string
		if prometheus {
			nodeConditions = []string{
				"Attributes['node'] != ''",
				"MetricName IN ('kube_node_status_condition', 'kube_node_status_allocatable', 'kube_node_info')",
			}
			nodeAttr = "node"
			k8sAppendOrEquals(&nodeConditions, &nodeParams, "Attributes['node']", nodeValues)
		} else {
			nodeConditions = []string{"Attributes['k8s.node.name'] != ''"}
			nodeAttr = "k8s.node.name"
			k8sAppendOrEquals(&nodeConditions, &nodeParams, "Attributes['k8s.node.name']", nodeValues)
		}
		if nameFilter != "" {
			nodeConditions = append(nodeConditions, "positionCaseInsensitive(Attributes['"+nodeAttr+"'], ?) > 0")
			nodeParams = append(nodeParams, nameFilter)
		}

		var nodeBase string
		if prometheus {
			nodeBase = "SELECT Attributes['node'] AS name, " +
				"maxIf(Value, MetricName = 'kube_node_status_condition' AND Attributes['condition'] = 'Ready' AND Attributes['status'] = 'true') AS ready_signal, " +
				"if(maxIf(Value, MetricName = 'kube_node_status_condition' AND Attributes['condition'] = 'Ready' AND Attributes['status'] = 'true') > 0, 'Ready', 'NotReady') AS status, " +
				"0.0 AS cpu_usage, " +
				"maxIf(Value, MetricName = 'kube_node_status_allocatable' AND Attributes['resource'] = 'memory') AS mem_used, " +
				"anyIf(Attributes['kubelet_version'], MetricName = 'kube_node_info') AS version, max(TimeUnix) AS last_seen " +
				"FROM otel_metrics_gauge WHERE " + strings.Join(nodeConditions, " AND ") + " GROUP BY name"
		} else {
			nodeBase = "SELECT Attributes['k8s.node.name'] AS name, " +
				"maxIf(Value, MetricName = 'k8s.node.condition_ready') AS ready_signal, " +
				"if(maxIf(Value, MetricName = 'k8s.node.condition_ready') > 0, 'Ready', 'NotReady') AS status, " +
				"maxIf(Value, MetricName = 'k8s.node.cpu.usage') AS cpu_usage, " +
				"maxIf(Value, MetricName = 'k8s.node.memory.usage') AS mem_used, " +
				"any(Attributes['k8s.kubelet.version']) AS version, max(TimeUnix) AS last_seen " +
				"FROM otel_metrics_gauge WHERE " + strings.Join(nodeConditions, " AND ") + " GROUP BY name"
		}

		statSQL := "SELECT count() AS total, countIf(ready_signal > 0) AS ready, " +
			"avg(cpu_usage) AS cpu_avg, avg(mem_used) AS mem_avg FROM (" + nodeBase + ")"
		if st, err := s.db.Execute(statSQL, nodeParams...); err == nil && len(st.Rows) > 0 {
			m := rowMaps(st)[0]
			metaTable(meta, "nodes").Set("total", cInt(m, "total"))
			summary.Set("nodes_total", cInt(m, "total")).Set("nodes_ready", cInt(m, "ready")).
				Set("nodes_cpu_avg", k8sAvgFloat(m, "cpu_avg")).Set("nodes_mem_used_avg", k8sAvgFloat(m, "mem_avg"))
		} else if err != nil {
			errors = append(errors, "nodes: "+err.Error())
			return
		}
		listSQL := "SELECT * FROM (" + nodeBase + ") ORDER BY " + opts["nodes"].sortCol + " " +
			strings.ToUpper(opts["nodes"].sortDir) + " LIMIT ? OFFSET ?"
		listParams := append(append([]any{}, nodeParams...), opts["nodes"].pageSize, opts["nodes"].offset)
		if rows, err := s.db.Execute(listSQL, listParams...); err == nil {
			for _, m := range rowMaps(rows) {
				status := "NotReady"
				if cFloat(m, "ready_signal") > 0 {
					status = "Ready"
				}
				nodes = append(nodes, jsonenc.NewObject().Set("name", cStr(m, "name")).Set("status", status).
					Set("version", cStr(m, "version")).Set("cpu_usage", cFloat(m, "cpu_usage")).
					Set("mem_used", cFloat(m, "mem_used")).Set("created", cStr(m, "last_seen")))
			}
		} else {
			errors = append(errors, "nodes: "+err.Error())
		}
	}()

	// --- Pods ---
	func() {
		var podConditions []string
		var podParams []any
		var podBase string
		if prometheus {
			podConditions = []string{
				"Attributes['pod'] != ''",
				"MetricName IN ('kube_pod_status_phase', 'kube_pod_status_ready', 'container_memory_working_set_bytes', 'kube_pod_container_status_restarts_total', 'kube_pod_info')",
			}
			k8sAppendOrEquals(&podConditions, &podParams, "Attributes['namespace']", namespaceValues)
			k8sAppendOrEquals(&podConditions, &podParams, "Attributes['pod']", podValues)
			if nameFilter != "" {
				podConditions = append(podConditions, "positionCaseInsensitive(Attributes['pod'], ?) > 0")
				podParams = append(podParams, nameFilter)
			}
			gaugeBase := "SELECT Attributes['namespace'] AS namespace, Attributes['pod'] AS name, " +
				"anyIf(Attributes['phase'], MetricName = 'kube_pod_status_phase' AND Value > 0) AS phase, " +
				"maxIf(Value, MetricName = 'kube_pod_status_ready' AND Attributes['condition'] = 'true') AS ready_signal, " +
				"0.0 AS cpu_usage, " +
				"sumIf(Value, MetricName = 'container_memory_working_set_bytes' AND Attributes['container'] != 'POD') AS mem_used, " +
				"toInt64(maxIf(Value, MetricName = 'kube_pod_container_status_restarts_total')) AS restarts, " +
				"anyIf(Attributes['node'], MetricName = 'kube_pod_info') AS node, max(TimeUnix) AS last_seen " +
				"FROM otel_metrics_gauge WHERE " + strings.Join(podConditions, " AND ") + " GROUP BY namespace, name"

			// Restart counter may live in otel_metrics_sum for some exporters.
			sumConditions := []string{
				"Attributes['pod'] != ''",
				"MetricName = 'kube_pod_container_status_restarts_total'",
			}
			var sumParams []any
			k8sAppendOrEquals(&sumConditions, &sumParams, "Attributes['namespace']", namespaceValues)
			k8sAppendOrEquals(&sumConditions, &sumParams, "Attributes['pod']", podValues)
			if nameFilter != "" {
				sumConditions = append(sumConditions, "positionCaseInsensitive(Attributes['pod'], ?) > 0")
				sumParams = append(sumParams, nameFilter)
			}
			sumBase := "SELECT Attributes['namespace'] AS namespace, Attributes['pod'] AS name, " +
				"'' AS phase, 0.0 AS ready_signal, 0.0 AS cpu_usage, 0.0 AS mem_used, " +
				"toInt64(max(Value)) AS restarts, '' AS node, max(TimeUnix) AS last_seen " +
				"FROM otel_metrics_sum WHERE " + strings.Join(sumConditions, " AND ") + " GROUP BY namespace, name"

			podBase = "SELECT namespace, name, anyIf(phase, phase != '') AS phase, " +
				"max(ready_signal) AS ready_signal, max(cpu_usage) AS cpu_usage, max(mem_used) AS mem_used, " +
				"max(restarts) AS restarts, anyIf(node, node != '') AS node, max(last_seen) AS last_seen " +
				"FROM (" + gaugeBase + " UNION ALL " + sumBase + ") GROUP BY namespace, name"
			podParams = append(podParams, sumParams...)
		} else {
			podConditions = []string{"Attributes['k8s.pod.name'] != ''"}
			k8sAppendOrEquals(&podConditions, &podParams, "Attributes['k8s.namespace.name']", namespaceValues)
			k8sAppendOrEquals(&podConditions, &podParams, "Attributes['k8s.pod.name']", podValues)
			if nameFilter != "" {
				podConditions = append(podConditions, "positionCaseInsensitive(Attributes['k8s.pod.name'], ?) > 0")
				podParams = append(podParams, nameFilter)
			}
			podBase = "SELECT Attributes['k8s.namespace.name'] AS namespace, Attributes['k8s.pod.name'] AS name, " +
				"any(Attributes['k8s.pod.phase']) AS phase, maxIf(Value, MetricName = 'k8s.pod.status_ready') AS ready_signal, " +
				"maxIf(Value, MetricName = 'k8s.pod.cpu.usage') AS cpu_usage, maxIf(Value, MetricName = 'k8s.pod.memory.usage') AS mem_used, " +
				"maxIf(toInt64(Value), MetricName = 'k8s.container.restart_count') AS restarts, " +
				"any(Attributes['k8s.node.name']) AS node, max(TimeUnix) AS last_seen " +
				"FROM otel_metrics_gauge WHERE " + strings.Join(podConditions, " AND ") + " GROUP BY namespace, name"
		}

		statSQL := "SELECT count() AS total, countIf(phase = 'Running') AS running, " +
			"countIf(phase = 'Failed') AS failed, sum(cpu_usage) AS cpu_total, sum(mem_used) AS mem_total FROM (" + podBase + ")"
		if st, err := s.db.Execute(statSQL, podParams...); err == nil && len(st.Rows) > 0 {
			m := rowMaps(st)[0]
			metaTable(meta, "pods").Set("total", cInt(m, "total"))
			summary.Set("pods_total", cInt(m, "total")).Set("pods_running", cInt(m, "running")).
				Set("pods_failed", cInt(m, "failed")).Set("pods_cpu_total", cFloat(m, "cpu_total")).
				Set("pods_mem_used_total", cFloat(m, "mem_total"))
		} else if err != nil {
			errors = append(errors, "pods: "+err.Error())
			return
		}
		listSQL := "SELECT * FROM (" + podBase + ") ORDER BY " + opts["pods"].sortCol + " " +
			strings.ToUpper(opts["pods"].sortDir) + " LIMIT ? OFFSET ?"
		listParams := append(append([]any{}, podParams...), opts["pods"].pageSize, opts["pods"].offset)
		if rows, err := s.db.Execute(listSQL, listParams...); err == nil {
			for _, m := range rowMaps(rows) {
				pods = append(pods, jsonenc.NewObject().Set("namespace", orDefault(cStr(m, "namespace"), "default")).
					Set("name", cStr(m, "name")).Set("phase", orDefault(cStr(m, "phase"), "Unknown")).
					Set("ready", cFloat(m, "ready_signal") > 0).Set("cpu_usage", cFloat(m, "cpu_usage")).
					Set("mem_used", cFloat(m, "mem_used")).Set("restarts", cInt(m, "restarts")).
					Set("node", cStr(m, "node")).Set("created", cStr(m, "last_seen")))
			}
		} else {
			errors = append(errors, "pods: "+err.Error())
		}
	}()

	// --- Deployments ---
	func() {
		var deployConditions []string
		var deployParams []any
		var deployBase string
		if prometheus {
			deployConditions = []string{
				"Attributes['deployment'] != ''",
				"MetricName IN ('kube_deployment_spec_replicas', 'kube_deployment_status_replicas_ready', 'kube_deployment_status_replicas_available', 'kube_deployment_status_replicas_updated', 'kube_deployment_status_replicas')",
			}
			k8sAppendOrEquals(&deployConditions, &deployParams, "Attributes['namespace']", namespaceValues)
			k8sAppendOrEquals(&deployConditions, &deployParams, "Attributes['deployment']", deploymentValues)
			if nameFilter != "" {
				deployConditions = append(deployConditions, "positionCaseInsensitive(Attributes['deployment'], ?) > 0")
				deployParams = append(deployParams, nameFilter)
			}
			deployBase = "SELECT Attributes['namespace'] AS namespace, Attributes['deployment'] AS name, " +
				"toInt64(maxIf(Value, MetricName = 'kube_deployment_spec_replicas')) AS desired, " +
				"toInt64(maxIf(Value, MetricName = 'kube_deployment_status_replicas_ready')) AS ready, " +
				"toInt64(maxIf(Value, MetricName = 'kube_deployment_status_replicas_available')) AS available, " +
				"toInt64(maxIf(Value, MetricName = 'kube_deployment_status_replicas_updated')) AS updated, max(TimeUnix) AS last_seen " +
				"FROM otel_metrics_gauge WHERE " + strings.Join(deployConditions, " AND ") + " GROUP BY namespace, name"
		} else {
			deployConditions = []string{"Attributes['k8s.deployment.name'] != ''"}
			k8sAppendOrEquals(&deployConditions, &deployParams, "Attributes['k8s.namespace.name']", namespaceValues)
			k8sAppendOrEquals(&deployConditions, &deployParams, "Attributes['k8s.deployment.name']", deploymentValues)
			if nameFilter != "" {
				deployConditions = append(deployConditions, "positionCaseInsensitive(Attributes['k8s.deployment.name'], ?) > 0")
				deployParams = append(deployParams, nameFilter)
			}
			deployBase = "SELECT Attributes['k8s.namespace.name'] AS namespace, Attributes['k8s.deployment.name'] AS name, " +
				"maxIf(toInt64(Value), MetricName = 'k8s.deployment.desired') AS desired, " +
				"maxIf(toInt64(Value), MetricName = 'k8s.deployment.ready') AS ready, " +
				"maxIf(toInt64(Value), MetricName = 'k8s.deployment.available') AS available, " +
				"maxIf(toInt64(Value), MetricName = 'k8s.deployment.updated') AS updated, max(TimeUnix) AS last_seen " +
				"FROM otel_metrics_gauge WHERE " + strings.Join(deployConditions, " AND ") + " GROUP BY namespace, name"
		}

		dTotal := s.countRowsParams("SELECT count(*) AS c FROM ("+deployBase+")", deployParams...)
		metaTable(meta, "deployments").Set("total", dTotal)
		summary.Set("deployments_total", dTotal).
			Set("deployments_unhealthy", s.countRowsParams("SELECT count(*) AS c FROM ("+deployBase+") WHERE ready < desired", deployParams...))
		listSQL := "SELECT * FROM (" + deployBase + ") ORDER BY " + opts["deployments"].sortCol + " " +
			strings.ToUpper(opts["deployments"].sortDir) + " LIMIT ? OFFSET ?"
		listParams := append(append([]any{}, deployParams...), opts["deployments"].pageSize, opts["deployments"].offset)
		if rows, err := s.db.Execute(listSQL, listParams...); err == nil {
			for _, m := range rowMaps(rows) {
				deployments = append(deployments, jsonenc.NewObject().
					Set("namespace", orDefault(cStr(m, "namespace"), "default")).Set("name", cStr(m, "name")).
					Set("desired", cInt(m, "desired")).Set("ready", cInt(m, "ready")).
					Set("available", cInt(m, "available")).Set("updated", cInt(m, "updated")).Set("created", cStr(m, "last_seen")))
			}
		} else {
			errors = append(errors, "deployments: "+err.Error())
		}
	}()

	// --- Namespaces ---
	func() {
		var nsSQL string
		if prometheus {
			nsSQL = "SELECT Attributes['namespace'] AS name, anyIf(Attributes['phase'], Value > 0) AS status, " +
				"max(TimeUnix) AS last_seen FROM otel_metrics_gauge " +
				"WHERE Attributes['namespace'] != '' AND MetricName = 'kube_namespace_status_phase' " +
				"GROUP BY name ORDER BY name"
		} else {
			nsSQL = "SELECT Attributes['k8s.namespace.name'] AS name, max(TimeUnix) AS last_seen " +
				"FROM otel_metrics_gauge WHERE Attributes['k8s.namespace.name'] != '' GROUP BY name ORDER BY name"
		}
		if rows, err := s.db.Execute(nsSQL); err == nil {
			for _, m := range rowMaps(rows) {
				status := "Active"
				if prometheus {
					status = orDefault(cStr(m, "status"), "Unknown")
				}
				namespaces = append(namespaces, jsonenc.NewObject().Set("name", cStr(m, "name")).
					Set("status", status).Set("created", cStr(m, "last_seen")))
			}
			summary.Set("namespaces_total", len(namespaces))
		} else {
			errors = append(errors, "namespaces: "+err.Error())
		}
	}()

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

// detectK8sMetricFormat mirrors _detect_k8s_metric_format. Counts are aliased AS c so the
// countRows helper (which reads the "c" column) sees them; a bare count() column is named
// "count()" and would always read as 0.
func (s *server) detectK8sMetricFormat() string {
	if s.countRows("SELECT count() AS c FROM otel_metrics_gauge WHERE Attributes['k8s.node.name'] != '' LIMIT 1") > 0 {
		return "otel"
	}
	promFilter := "(MetricName LIKE 'kube_%' OR MetricName IN ('container_memory_working_set_bytes'))"
	for _, table := range []string{"otel_metrics_gauge", "otel_metrics_sum"} {
		if s.countRows("SELECT count() AS c FROM "+table+" WHERE "+promFilter+" LIMIT 1") > 0 {
			return "prometheus"
		}
	}
	return "none"
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
