package provider

import (
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	"github.com/openeverest/provider-kserve/definition/components"
	"github.com/openeverest/provider-kserve/definition/topologies/predictor"
)

func TestValidatePredictorAIGateway(t *testing.T) {
	embeddings := components.ModelServerCustomSpec{ModelFormat: "huggingface", StorageURI: "hf://org/model", Task: "text_embedding"}
	gateway := predictor.PredictorTopologyParameters{ExternalAccess: predictor.ExternalAccessEnvoyAIGateway}
	cases := []struct {
		name    string
		enabled bool
		params  components.ModelServerCustomSpec
		topo    predictor.PredictorTopologyParameters
		routes  []client.Object
		wantErr string
	}{
		{name: "embedding model", enabled: true, params: embeddings, topo: gateway},
		{name: "chart gateway disabled", params: embeddings, topo: gateway, wantErr: "aiGateway.enabled"},
		{name: "not an embedding model", enabled: true, params: components.ModelServerCustomSpec{ModelFormat: "sklearn", StorageURI: "gs://bucket/iris"}, topo: gateway, wantErr: "OpenAI-compatible"},
		{name: "huggingface without the embedding task", enabled: true, params: components.ModelServerCustomSpec{ModelFormat: "huggingface", StorageURI: "hf://org/model"}, topo: gateway, wantErr: "OpenAI-compatible"},
		{name: "OpenAI-compatible image", enabled: true, params: components.ModelServerCustomSpec{Image: "vllm/vllm-openai", OpenAICompatible: true}, topo: gateway},
		{name: "image without openAICompatible", enabled: true, params: components.ModelServerCustomSpec{Image: "ghcr.io/example/server:1"}, topo: gateway, wantErr: "OpenAI-compatible"},
		{
			name: "model name served by another route", enabled: true, params: embeddings, topo: gateway,
			routes:  []client.Object{aiGatewayRouteObject("other", "iris-ai-gateway", "gw", "gw-ns", "iris")},
			wantErr: "other/iris-ai-gateway",
		},
		{
			name: "token limit with the gateway", enabled: true, params: embeddings,
			topo: predictor.PredictorTopologyParameters{ExternalAccess: predictor.ExternalAccessEnvoyAIGateway, TokenLimitPerHour: ptr.To(int32(5000))},
		},
		{name: "token limit without the gateway", params: embeddings, topo: predictor.PredictorTopologyParameters{TokenLimitPerHour: ptr.To(int32(5000))}, wantErr: "requires externalAccess EnvoyAIGateway"},
		{
			name: "zero token limit", enabled: true, params: embeddings,
			topo:    predictor.PredictorTopologyParameters{ExternalAccess: predictor.ExternalAccessEnvoyAIGateway, TokenLimitPerHour: ptr.To(int32(0))},
			wantErr: "greater than zero",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.enabled {
				t.Setenv("AI_GATEWAY_ENABLED", "true")
				t.Setenv("AI_GATEWAY_AUTH_ENABLED", "true")
				t.Setenv("AI_GATEWAY_AUTH_ALLOW_INSECURE_HTTP", "true")
				t.Setenv("AI_GATEWAY_NAME", "gw")
				t.Setenv("AI_GATEWAY_NAMESPACE", "gw-ns")
			}
			mapper := meta.NewDefaultRESTMapper(nil)
			mapper.Add(aiGatewayRouteGVK, meta.RESTScopeNamespace)
			cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithRESTMapper(mapper).WithObjects(tc.routes...).Build()
			c := predictorContext(t, cl, corev1alpha1.ComponentSpec{Parameters: rawJSON(t, tc.params)})
			c.Instance().Spec.Topology.Parameters = rawJSON(t, tc.topo)
			err := validatePredictor(c)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestBuildServiceBackends(t *testing.T) {
	backend, aiBackend := buildServiceBackends("minilm", "models", "minilm-predictor.models.svc.cluster.local", 80, "/openai/v1")

	if backend.GroupVersionKind() != envoyBackendGVK || backend.GetName() != "minilm-ai-gateway" || backend.GetNamespace() != "models" {
		t.Fatalf("backend = %s %s/%s", backend.GroupVersionKind(), backend.GetNamespace(), backend.GetName())
	}
	endpoints, _, _ := unstructured.NestedSlice(backend.Object, "spec", "endpoints")
	wantEndpoints := []any{map[string]any{"fqdn": map[string]any{"hostname": "minilm-predictor.models.svc.cluster.local", "port": int64(80)}}}
	if !reflect.DeepEqual(endpoints, wantEndpoints) {
		t.Fatalf("endpoints = %#v", endpoints)
	}

	if aiBackend.GroupVersionKind() != aiServiceBackendGVK || aiBackend.GetName() != "minilm-ai-gateway" {
		t.Fatalf("aiBackend = %s %s", aiBackend.GroupVersionKind(), aiBackend.GetName())
	}
	spec, _, _ := unstructured.NestedMap(aiBackend.Object, "spec")
	wantSpec := map[string]any{
		"schema":     map[string]any{"name": "OpenAI", "prefix": "/openai/v1"},
		"backendRef": map[string]any{"group": "gateway.envoyproxy.io", "kind": "Backend", "name": "minilm-ai-gateway"},
	}
	if !reflect.DeepEqual(spec, wantSpec) {
		t.Fatalf("aiBackend spec = %#v", spec)
	}
}

func TestPredictorAIGatewayServiceType(t *testing.T) {
	topo := predictor.PredictorTopologyParameters{ExternalAccess: predictor.ExternalAccessEnvoyAIGateway}
	if got := topo.ResolvedServiceType(); got != "ClusterIP" {
		t.Fatalf("ResolvedServiceType() = %q, want ClusterIP: the gateway needs no external Service", got)
	}
}
