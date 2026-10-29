package provider

import (
	"reflect"
	"regexp"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestBuildPodMonitor(t *testing.T) {
	t.Parallel()

	pm := buildPodMonitor("chat", "models", workloadPodSelector("chat"), vllmServingPort, "/metrics", "30s")
	ep := assertPodMonitor(t, pm, "chat", workloadPodSelector("chat"))
	if ep["targetPort"] != int64(vllmServingPort) {
		t.Fatalf("targetPort = %v, want %d", ep["targetPort"], vllmServingPort)
	}
}

func TestBuildPredictorPodMonitor(t *testing.T) {
	t.Parallel()

	selector := map[string]string{"core.openeverest.io/instance": "iris", "core.openeverest.io/component": "predictor"}
	pm := buildPredictorPodMonitor("iris", "models", selector, "30s")
	ep := assertPodMonitor(t, pm, "iris", selector)
	if _, ok := ep["targetPort"]; ok {
		t.Fatal("predictor pods declare no ports; a targetPort would match no targets")
	}
	relabelings, ok := ep["relabelings"].([]any)
	if !ok || len(relabelings) != 4 {
		t.Fatalf("relabelings = %#v", ep["relabelings"])
	}

	// Apply the address rewrites the way Prometheus does (anchored regex).
	address := func(podIP, annotationPort string) string {
		addr := podIP
		for _, r := range relabelings[1:3] {
			r := r.(map[string]any)
			src := addr
			if len(r["sourceLabels"].([]any)) == 2 {
				src += ";" + annotationPort
			}
			re := regexp.MustCompile("^(?:" + r["regex"].(string) + ")$")
			if re.MatchString(src) {
				addr = re.ReplaceAllString(src, r["replacement"].(string))
			}
		}
		return addr
	}
	for _, tc := range []struct{ port, want string }{
		{"", "10.0.0.5:8080"},
		{"8002", "10.0.0.5:8002"},
	} {
		if got := address("10.0.0.5", tc.port); got != tc.want {
			t.Fatalf("annotation port %q: address = %q, want %q", tc.port, got, tc.want)
		}
	}
}

func assertPodMonitor(t *testing.T, pm *unstructured.Unstructured, name string, selector map[string]string) map[string]any {
	t.Helper()

	if pm.GetName() != name+"-metrics" {
		t.Fatalf("name = %q, want %s-metrics", pm.GetName(), name)
	}
	if pm.GetNamespace() != "models" {
		t.Fatalf("namespace = %q, want models", pm.GetNamespace())
	}
	if pm.GroupVersionKind() != podMonitorGVK {
		t.Fatalf("GVK = %v, want %v", pm.GroupVersionKind(), podMonitorGVK)
	}
	if got := pm.GetLabels()["core.openeverest.io/instance"]; got != name {
		t.Fatalf("instance label = %q, want %q", got, name)
	}

	labels, found, err := unstructured.NestedStringMap(pm.Object, "spec", "selector", "matchLabels")
	if err != nil || !found {
		t.Fatalf("matchLabels found %t, err %v", found, err)
	}
	if !reflect.DeepEqual(labels, selector) {
		t.Fatalf("selector = %v, want %v", labels, selector)
	}

	eps, found, err := unstructured.NestedSlice(pm.Object, "spec", "podMetricsEndpoints")
	if err != nil || !found || len(eps) != 1 {
		t.Fatalf("podMetricsEndpoints = %#v, found %t, err %v", eps, found, err)
	}
	ep := eps[0].(map[string]any)
	if ep["path"] != "/metrics" {
		t.Fatalf("path = %v, want /metrics", ep["path"])
	}
	if ep["interval"] != "30s" {
		t.Fatalf("interval = %v, want 30s", ep["interval"])
	}
	return ep
}
