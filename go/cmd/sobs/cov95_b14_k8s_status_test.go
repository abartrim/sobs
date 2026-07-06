package main

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sobs/sobs/internal/jsonenc"
	"github.com/sobs/sobs/internal/store"
	"github.com/sobs/sobs/internal/store/storetest"
)

// Coverage batch 14: cmd/sobs/k8s_status.go — no dedicated test file existed yet. k8sQInt's
// clamp branches, metaTable's "absent key" / "wrong type" fallbacks, and
// handleApiKubernetesStatus's disabled/enabled(otel)/enabled(prometheus)/error paths were all
// untested.

func TestK8sQInt(t *testing.T) {
	t.Run("default_when_missing", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x", nil)
		if got := k8sQInt(r, "page", 1, 1, 100); got != 1 {
			t.Errorf("got %d, want default 1", got)
		}
	})
	t.Run("default_when_non_numeric", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?page=abc", nil)
		if got := k8sQInt(r, "page", 5, 1, 100); got != 5 {
			t.Errorf("got %d, want default 5", got)
		}
	})
	t.Run("clamped_below_lo", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?page=-3", nil)
		if got := k8sQInt(r, "page", 1, 1, 100); got != 1 {
			t.Errorf("got %d, want clamped to 1", got)
		}
	})
	t.Run("clamped_above_hi", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?page_size=99999", nil)
		if got := k8sQInt(r, "page_size", 25, 1, 200); got != 200 {
			t.Errorf("got %d, want clamped to 200", got)
		}
	})
	t.Run("valid_value_passed_through", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?page=7", nil)
		if got := k8sQInt(r, "page", 1, 1, 100); got != 7 {
			t.Errorf("got %d, want 7", got)
		}
	})
	t.Run("whitespace_trimmed", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/x?page=%20%207%20%20", nil)
		if got := k8sQInt(r, "page", 1, 1, 100); got != 7 {
			t.Errorf("got %d, want 7 after trim", got)
		}
	})
}

func TestK8sNormalizedValues(t *testing.T) {
	r := httptest.NewRequest("GET", "/x?node=%20a%20&node=&node=b", nil)
	got := k8sNormalizedValues(r, "node")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("got %v, want [a b] (blank/whitespace-only trimmed)", got)
	}
	if got := k8sNormalizedValues(r, "missing"); len(got) != 0 {
		t.Errorf("missing key should yield empty slice, got %v", got)
	}
}

func TestK8sAppendOrEquals(t *testing.T) {
	t.Run("empty_values_no_op", func(t *testing.T) {
		conditions := []string{"base"}
		var params []any
		k8sAppendOrEquals(&conditions, &params, "field", nil)
		if len(conditions) != 1 || len(params) != 0 {
			t.Errorf("expected no-op, got conditions=%v params=%v", conditions, params)
		}
	})
	t.Run("appends_or_clause", func(t *testing.T) {
		conditions := []string{}
		var params []any
		k8sAppendOrEquals(&conditions, &params, "field", []string{"a", "b"})
		if len(conditions) != 1 || !strings.Contains(conditions[0], "field = ? OR field = ?") {
			t.Errorf("got %v", conditions)
		}
		if len(params) != 2 || params[0] != "a" || params[1] != "b" {
			t.Errorf("got %v", params)
		}
	})
}

func TestK8sAvgFloat(t *testing.T) {
	t.Run("missing_key_yields_nan", func(t *testing.T) {
		got := k8sAvgFloat(map[string]any{}, "cpu_avg")
		if got == got { // NaN != NaN
			t.Errorf("expected NaN for missing key, got %v", got)
		}
	})
	t.Run("nil_value_yields_nan", func(t *testing.T) {
		got := k8sAvgFloat(map[string]any{"cpu_avg": nil}, "cpu_avg")
		if got == got {
			t.Errorf("expected NaN for nil value, got %v", got)
		}
	})
	t.Run("present_value_passed_through", func(t *testing.T) {
		got := k8sAvgFloat(map[string]any{"cpu_avg": float64(3.5)}, "cpu_avg")
		if got != 3.5 {
			t.Errorf("got %v, want 3.5", got)
		}
	})
}

func TestMetaTable(t *testing.T) {
	t.Run("absent_key_returns_empty_object", func(t *testing.T) {
		meta := jsonenc.NewObject()
		got := metaTable(meta, "nodes")
		if got == nil || got.Len() != 0 {
			t.Errorf("expected empty object for absent key, got %v", got)
		}
	})
	t.Run("wrong_type_returns_empty_object", func(t *testing.T) {
		meta := jsonenc.NewObject().Set("nodes", "not-an-object")
		got := metaTable(meta, "nodes")
		if got == nil || got.Len() != 0 {
			t.Errorf("expected empty object for wrong-typed value, got %v", got)
		}
	})
	t.Run("present_object_returned", func(t *testing.T) {
		inner := jsonenc.NewObject().Set("total", 5)
		meta := jsonenc.NewObject().Set("nodes", inner)
		got := metaTable(meta, "nodes")
		if v, _ := got.Get("total"); v != 5 {
			t.Errorf("expected the same inner object with total=5, got %v", got)
		}
	})
}

func TestHandleApiKubernetesStatus_Disabled(t *testing.T) {
	s := &server{db: storetest.SettingsDB(map[string]string{})}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/kubernetes/status", nil)
	s.handleApiKubernetesStatus(w, r)
	if w.Code != 404 {
		t.Fatalf("want 404 when k8s disabled, got %d", w.Code)
	}
}

func TestHandleApiKubernetesStatus_EnabledNoData(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		if strings.Contains(q, "sobs_app_settings") {
			return storetest.Result([]string{"Value"}, []any{"1"}), nil
		}
		// Every metric-format probe + stats/list query returns zero rows.
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/kubernetes/status", nil)
	s.handleApiKubernetesStatus(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "No Kubernetes data found yet") {
		t.Errorf("expected the no-data message, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"source":"otel"`) {
		t.Errorf(`expected source "otel" when format is "none", got %s`, w.Body.String())
	}
}

func TestHandleApiKubernetesStatus_OtelFormatWithFiltersAndSort(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_app_settings"):
			return storetest.Result([]string{"Value"}, []any{"1"}), nil
		case strings.Contains(q, "k8s.node.name") && strings.Contains(q, "count() AS c"):
			return storetest.Result([]string{"c"}, []any{float64(1)}), nil // detectK8sMetricFormat -> otel
		case strings.Contains(q, "count() AS total, countIf(ready_signal"):
			return storetest.Result([]string{"total", "ready", "cpu_avg", "mem_avg"},
				[]any{float64(1), float64(1), float64(10.0), float64(20.0)}), nil
		case strings.Contains(q, "FROM (SELECT Attributes['k8s.node.name']"):
			return storetest.Result(
				[]string{"name", "ready_signal", "status", "cpu_usage", "mem_used", "version", "last_seen"},
				[]any{"node-a", float64(1), "Ready", float64(5.0), float64(6.0), "v1.30", "2026-01-01 00:00:00"},
			), nil
		case strings.Contains(q, "count() AS total, countIf(phase"):
			return storetest.Result([]string{"total", "running", "failed", "cpu_total", "mem_total"},
				[]any{float64(2), float64(1), float64(0), float64(1.0), float64(2.0)}), nil
		case strings.Contains(q, "FROM (SELECT Attributes['k8s.namespace.name'] AS namespace, Attributes['k8s.pod.name']"):
			return storetest.Result(
				[]string{"namespace", "name", "phase", "ready_signal", "cpu_usage", "mem_used", "restarts", "node", "last_seen"},
				[]any{"ns1", "pod-a", "Running", float64(1), float64(1.0), float64(2.0), float64(0), "node-a", "2026-01-01 00:00:00"},
			), nil
		case strings.Contains(q, "count(*) AS c FROM (SELECT Attributes['k8s.namespace.name'] AS namespace, Attributes['k8s.deployment.name']"):
			return storetest.Result([]string{"c"}, []any{float64(1)}), nil
		case strings.Contains(q, "FROM (SELECT Attributes['k8s.namespace.name'] AS namespace, Attributes['k8s.deployment.name']"):
			return storetest.Result(
				[]string{"namespace", "name", "desired", "ready", "available", "updated", "last_seen"},
				[]any{"ns1", "deploy-a", float64(3), float64(3), float64(3), float64(3), "2026-01-01 00:00:00"},
			), nil
		case strings.Contains(q, "k8s.namespace.name'] AS name, max(TimeUnix)"):
			return storetest.Result([]string{"name", "last_seen"}, []any{"ns1", "2026-01-01 00:00:00"}), nil
		}
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/kubernetes/status?nodes_sort=version&nodes_dir=desc&namespace=ns1&node=node-a&name=node", nil)
	s.handleApiKubernetesStatus(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "node-a") {
		t.Errorf("expected node-a in response, got %s", body)
	}
	if !strings.Contains(body, "pod-a") {
		t.Errorf("expected pod-a in response, got %s", body)
	}
	if !strings.Contains(body, "deploy-a") {
		t.Errorf("expected deploy-a in response, got %s", body)
	}
	if !strings.Contains(body, `"source":"otel"`) {
		t.Errorf("expected source otel, got %s", body)
	}
}

func TestHandleApiKubernetesStatus_PrometheusFormatWithMultiValueFilters(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_app_settings"):
			return storetest.Result([]string{"Value"}, []any{"1"}), nil
		case strings.Contains(q, "k8s.node.name") && strings.Contains(q, "count() AS c"):
			return &store.Result{}, nil // otel format absent
		case strings.Contains(q, "kube_%") && strings.Contains(q, "count() AS c"):
			return storetest.Result([]string{"c"}, []any{float64(1)}), nil // prometheus format present
		case strings.Contains(q, "count() AS total, countIf(ready_signal"):
			return storetest.Result([]string{"total", "ready", "cpu_avg", "mem_avg"},
				[]any{float64(1), float64(0), float64(0.0), float64(0.0)}), nil
		case strings.Contains(q, "count() AS total, countIf(phase"):
			return storetest.Result([]string{"total", "running", "failed", "cpu_total", "mem_total"},
				[]any{float64(0), float64(0), float64(0), float64(0.0), float64(0.0)}), nil
		case strings.Contains(q, "count(*) AS c FROM"):
			return storetest.Result([]string{"c"}, []any{float64(0)}), nil
		}
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/kubernetes/status?namespace=ns1&namespace=ns2&deployment=d1&pod=p1&nodes_page=2&nodes_page_size=10", nil)
	s.handleApiKubernetesStatus(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"source":"prometheus"`) {
		t.Errorf("expected source prometheus, got %s", w.Body.String())
	}
}

func TestHandleApiKubernetesStatus_ListQueryErrorsAggregated(t *testing.T) {
	s := &server{db: &storetest.FakeDB{ExecuteFunc: func(q string, p ...any) (*store.Result, error) {
		switch {
		case strings.Contains(q, "sobs_app_settings"):
			return storetest.Result([]string{"Value"}, []any{"1"}), nil
		case strings.Contains(q, "k8s.node.name") && strings.Contains(q, "count() AS c"):
			return storetest.Result([]string{"c"}, []any{float64(1)}), nil
		case strings.Contains(q, "count() AS total, countIf(ready_signal"):
			return nil, errors.New("nodes stats boom")
		case strings.Contains(q, "count() AS total, countIf(phase"):
			return nil, errors.New("pods stats boom")
		case strings.Contains(q, "count(*) AS c FROM"):
			return nil, errors.New("deployments count boom")
		case strings.Contains(q, "k8s.namespace.name'] AS name, max(TimeUnix)"):
			return nil, errors.New("namespaces boom")
		}
		return &store.Result{}, nil
	}}}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/kubernetes/status", nil)
	s.handleApiKubernetesStatus(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200 (errors are aggregated into the body, not a 5xx), got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "nodes:") || !strings.Contains(body, "pods:") {
		t.Errorf("expected aggregated per-section errors, got %s", body)
	}
}
