package provider

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	knativekmeta "knative.dev/pkg/kmeta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-kserve/internal/common"
)

const podMonitorSuffix = "-metrics"

var podMonitorGVK = schema.GroupVersionKind{
	Group:   "monitoring.coreos.com",
	Version: "v1",
	Kind:    "PodMonitor",
}

// buildPodMonitor builds a Prometheus Operator PodMonitor that scrapes the pods
// matched by selector on targetPort/path.
//
// This serves the per-Instance workload layers: vLLM (llm) and predictor
// (InferenceService) pods — both are created per Instance by this reconcile,
// so a new layer is just a different selector/endpoint here, not new
// plumbing. Singleton infrastructure does NOT belong here: the Envoy AI
// Gateway is installed once by the chart (not per Instance) and exposes its
// gen_ai.* metrics on a differently-named port (aigw-admin), so its PodMonitor
// lives in the chart template that installs the gateway
// (charts/provider-kserve/templates/ai-gateway-podmonitor.yaml).
//
// targetPort (rather than a named port) is used so the PodMonitor does not
// depend on KServe naming the vLLM container port, which is not guaranteed
// stable across KServe versions.
func buildPodMonitor(
	instanceName, namespace string,
	selector map[string]string,
	targetPort int,
	path, interval string,
) *unstructured.Unstructured {
	return newPodMonitor(instanceName, namespace, selector, map[string]any{
		"targetPort": int64(targetPort),
		"path":       path,
		"interval":   interval,
	})
}

// buildPredictorPodMonitor scrapes the endpoint the serving runtime declares in
// its prometheus.kserve.io/port and /path annotations, which KServe copies onto
// the predictor pods (e.g. Triton 8002, TorchServe 8082, most others 8080).
// KServe declares no container ports on these pods, so a targetPort endpoint
// would match no targets.
func buildPredictorPodMonitor(instanceName, namespace string, selector map[string]string, interval string) *unstructured.Unstructured {
	return newPodMonitor(instanceName, namespace, selector, map[string]any{
		"path":     "/metrics",
		"interval": interval,
		"relabelings": []any{
			map[string]any{
				"sourceLabels": []any{"__meta_kubernetes_pod_container_name"},
				"regex":        "kserve-container",
				"action":       "keep",
			},
			map[string]any{
				"sourceLabels": []any{"__address__"},
				"regex":        `([^:]+)(?::\d+)?`,
				"replacement":  fmt.Sprintf("$1:%d", predictorContainerPort),
				"targetLabel":  "__address__",
			},
			map[string]any{
				"sourceLabels": []any{"__address__", "__meta_kubernetes_pod_annotation_prometheus_kserve_io_port"},
				"regex":        `([^:]+):\d+;(\d+)`,
				"replacement":  "$1:$2",
				"targetLabel":  "__address__",
			},
			map[string]any{
				"sourceLabels": []any{"__meta_kubernetes_pod_annotation_prometheus_kserve_io_path"},
				"regex":        "(.+)",
				"targetLabel":  "__metrics_path__",
			},
		},
	})
}

func newPodMonitor(instanceName, namespace string, selector map[string]string, endpoint map[string]any) *unstructured.Unstructured {
	labels := make(map[string]any, len(selector))
	for k, v := range selector {
		labels[k] = v
	}

	pm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": podMonitorGVK.GroupVersion().String(),
		"kind":       podMonitorGVK.Kind,
		"metadata": map[string]any{
			"name":      knativekmeta.ChildName(instanceName, podMonitorSuffix),
			"namespace": namespace,
			// Lets consumers such as plugin-metrics find an Instance's monitors.
			"labels": map[string]any{controller.InstanceLabel: instanceName},
		},
		"spec": map[string]any{
			// No namespaceSelector: the default restricts to the PodMonitor's
			// own namespace, which is where the Instance's workload pods live.
			"selector":            map[string]any{"matchLabels": labels},
			"podMetricsEndpoints": []any{endpoint},
		},
	}}
	pm.SetGroupVersionKind(podMonitorGVK)
	return pm
}

// syncPodMonitor applies pm, or removes it when the chart-level or
// instance-level flag is off.
//
// A target cluster may not have the Prometheus Operator (monitoring.coreos.com)
// CRDs installed. applyPodMonitor treats that as a no-op rather than an error,
// so enabling metrics never breaks a bare cluster; install the CRDs later and
// the next reconcile will emit the PodMonitor.
func syncPodMonitor(c *controller.Context, enabled bool, pm *unstructured.Unstructured) error {
	if !common.PodMonitorEnabled() || !enabled {
		return deletePodMonitor(c)
	}
	return applyPodMonitor(c, pm)
}

func applyPodMonitor(c *controller.Context, pm *unstructured.Unstructured) error {
	if err := common.Apply(c.Context(), c.Client(), c.Instance(), pm); err != nil {
		if meta.IsNoMatchError(err) {
			log.FromContext(c.Context()).V(1).Info(
				"PodMonitor CRD not installed; skipping metrics scrape config",
				"instance", c.Name(),
			)
			return nil
		}
		return err
	}
	return nil
}

func deletePodMonitor(c *controller.Context) error {
	pm := &unstructured.Unstructured{}
	pm.SetGroupVersionKind(podMonitorGVK)
	pm.SetName(knativekmeta.ChildName(c.Name(), podMonitorSuffix))
	pm.SetNamespace(c.Instance().Namespace)
	if err := c.Delete(pm); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return client.IgnoreNotFound(err)
	}
	return nil
}
