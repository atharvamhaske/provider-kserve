package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-kserve/definition/components"
	"github.com/openeverest/provider-kserve/internal/common"
)

func predictorContext(t *testing.T, cl client.Client, comp corev1alpha1.ComponentSpec) *controller.Context {
	t.Helper()
	inst := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "iris", Namespace: "ns"},
		Spec: corev1alpha1.InstanceSpec{
			Topology:   &corev1alpha1.TopologySpec{Type: common.TopologyPredictor},
			Components: map[string]corev1alpha1.ComponentSpec{common.ComponentPredictor: comp},
		},
	}
	return controller.NewContext(context.Background(), cl, inst, common.ProviderName)
}

func predictorPodLabels() map[string]any {
	return map[string]any{
		controller.ProviderLabel:  common.ProviderName,
		controller.InstanceLabel:  "iris",
		controller.ComponentLabel: common.ComponentPredictor,
	}
}

func TestBuildInferenceService(t *testing.T) {
	t.Run("maps parameters to KServe field paths", func(t *testing.T) {
		c := predictorContext(t, nil, corev1alpha1.ComponentSpec{
			Replicas: ptr.To(int32(2)),
			Resources: &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
			},
			Parameters: rawJSON(t, components.ModelServerCustomSpec{
				ModelFormat:    "sklearn",
				StorageURI:     "gs://bucket/iris",
				Runtime:          "kserve-sklearnserver",
				RuntimeVersion:   "1.5",
				MaxReplicas:      ptr.To(int32(4)),
				StorageContainer: "large-model-init",
			}),
		})

		isvc, err := buildInferenceService(c)
		if err != nil {
			t.Fatal(err)
		}
		if isvc.GroupVersionKind() != inferenceServiceGVK {
			t.Fatalf("gvk = %v, want %v", isvc.GroupVersionKind(), inferenceServiceGVK)
		}
		if got := isvc.GetAnnotations()[common.DeploymentModeAnnotation]; got != common.DeploymentModeStandard {
			t.Fatalf("deployment mode annotation = %q, want %q", got, common.DeploymentModeStandard)
		}

		want := map[string]any{
			"predictor": map[string]any{
				"minReplicas":          int64(2),
				"maxReplicas":          int64(4),
				"labels":               predictorPodLabels(),
				"storageContainerName": "large-model-init",
				"model": map[string]any{
					"modelFormat":    map[string]any{"name": "sklearn"},
					"runtime":        "kserve-sklearnserver",
					"storageUri":     "gs://bucket/iris",
					"runtimeVersion": "1.5",
					"resources":      map[string]any{"limits": map[string]any{"memory": "1Gi"}},
				},
			},
		}
		if !reflect.DeepEqual(isvc.Object["spec"], want) {
			t.Fatalf("spec = %#v\nwant %#v", isvc.Object["spec"], want)
		}
	})

	t.Run("omits unset optional fields", func(t *testing.T) {
		c := predictorContext(t, nil, corev1alpha1.ComponentSpec{
			Parameters: rawJSON(t, components.ModelServerCustomSpec{ModelFormat: "onnx"}),
		})

		isvc, err := buildInferenceService(c)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{
			"predictor": map[string]any{
				"labels": predictorPodLabels(),
				"model":  map[string]any{"modelFormat": map[string]any{"name": "onnx"}},
			},
		}
		if !reflect.DeepEqual(isvc.Object["spec"], want) {
			t.Fatalf("spec = %#v\nwant %#v", isvc.Object["spec"], want)
		}
	})
}

func TestValidatePredictorStorageContainer(t *testing.T) {
	storageContainer := func(name string, mut func(map[string]any)) client.Object {
		obj := unstructuredObject(clusterStorageContainerGVK)
		obj.SetName(name)
		spec := map[string]any{
			"container": map[string]any{"name": "storage-initializer", "image": "kserve/storage-initializer"},
			"supportedUriFormats": []any{
				map[string]any{"prefix": "s3://"},
				map[string]any{"regex": `https://(.+?)\.blob\.core\.windows\.net/(.+)`},
			},
			"workloadType": "initContainer",
		}
		obj.Object["spec"] = spec
		if mut != nil {
			mut(obj.Object)
		}
		return obj
	}
	setWorkloadType := func(wt string) func(map[string]any) {
		return func(o map[string]any) { o["spec"].(map[string]any)["workloadType"] = wt }
	}

	cases := []struct {
		name             string
		storageContainer string
		storageURI       string
		objs             []client.Object
		wantErr          string
	}{
		{
			name:       "unset skips the lookup",
			storageURI: "gs://bucket/model",
		},
		{
			name:             "prefix match",
			storageContainer: "s3-init",
			storageURI:       "s3://bucket/model",
			objs:             []client.Object{storageContainer("s3-init", nil)},
		},
		{
			name:             "regex match",
			storageContainer: "s3-init",
			storageURI:       "https://acct.blob.core.windows.net/models/iris",
			objs:             []client.Object{storageContainer("s3-init", nil)},
		},
		{
			name:             "empty workloadType defaults to initContainer",
			storageContainer: "s3-init",
			storageURI:       "s3://bucket/model",
			objs:             []client.Object{storageContainer("s3-init", setWorkloadType(""))},
		},
		{
			name:             "invalid name",
			storageContainer: "S3_Init",
			storageURI:       "s3://bucket/model",
			wantErr:          `predictor.parameters.storageContainer: invalid name "S3_Init"`,
		},
		{
			name:             "not found",
			storageContainer: "missing",
			storageURI:       "s3://bucket/model",
			wantErr:          `predictor.parameters.storageContainer: ClusterStorageContainer "missing" not found`,
		},
		{
			name:             "disabled",
			storageContainer: "s3-init",
			storageURI:       "s3://bucket/model",
			objs: []client.Object{storageContainer("s3-init", func(o map[string]any) {
				o["disabled"] = true
			})},
			wantErr: `predictor.parameters.storageContainer: ClusterStorageContainer "s3-init" is disabled`,
		},
		{
			name:             "local model download job",
			storageContainer: "s3-init",
			storageURI:       "s3://bucket/model",
			objs:    []client.Object{storageContainer("s3-init", setWorkloadType("localModelDownloadJob"))},
			wantErr: `predictor.parameters.storageContainer: ClusterStorageContainer "s3-init" has workloadType "localModelDownloadJob", want "initContainer"`,
		},
		{
			name:             "unsupported URI",
			storageContainer: "s3-init",
			storageURI:       "gs://bucket/model",
			objs:             []client.Object{storageContainer("s3-init", nil)},
			wantErr:          `predictor.parameters.storageContainer: ClusterStorageContainer "s3-init" does not support storageURI "gs://bucket/model"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithObjects(tc.objs...).Build()
			c := predictorContext(t, cl, corev1alpha1.ComponentSpec{
				Parameters: rawJSON(t, components.ModelServerCustomSpec{
					ModelFormat:      "sklearn",
					StorageURI:       tc.storageURI,
					StorageContainer: tc.storageContainer,
				}),
			})

			err := validatePredictor(c)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.wantErr)):
				t.Fatalf("error = %v, want prefix %q", err, tc.wantErr)
			}
		})
	}
}

func TestStatusPredictor(t *testing.T) {
	isvcWithStatus := func(status map[string]any) *unstructured.Unstructured {
		obj := unstructuredObject(inferenceServiceGVK)
		obj.SetName("iris")
		obj.SetNamespace("ns")
		obj.Object["status"] = status
		return obj
	}
	comp := corev1alpha1.ComponentSpec{
		Parameters: rawJSON(t, components.ModelServerCustomSpec{ModelFormat: "sklearn"}),
	}

	cases := []struct {
		name      string
		objs      []client.Object
		wantPhase corev1alpha1.InstancePhase
		wantURI   string
	}{
		{
			name:      "missing InferenceService is provisioning",
			wantPhase: corev1alpha1.InstancePhaseProvisioning,
		},
		{
			name: "not ready is provisioning",
			objs: []client.Object{isvcWithStatus(map[string]any{
				"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "MinimumReplicasUnavailable"}},
			})},
			wantPhase: corev1alpha1.InstancePhaseProvisioning,
		},
		{
			name: "ready with URL publishes connection details",
			objs: []client.Object{isvcWithStatus(map[string]any{
				"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
				"url":        "https://iris.example.com",
			})},
			wantPhase: corev1alpha1.InstancePhaseReady,
			wantURI:   "https://iris.example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithObjects(tc.objs...).Build()
			got, err := (&Provider{}).statusPredictor(predictorContext(t, cl, comp))
			if err != nil {
				t.Fatal(err)
			}
			if got.Phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", got.Phase, tc.wantPhase)
			}
			if got.ConnectionDetails.URI != tc.wantURI {
				t.Fatalf("connection details = %+v, want URI %q", got.ConnectionDetails, tc.wantURI)
			}
		})
	}
}
