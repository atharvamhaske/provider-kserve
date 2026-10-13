// Package provider — InferenceService (serving.kserve.io/v1beta1) builder.
package provider

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-kserve/definition/components"
	"github.com/openeverest/provider-kserve/internal/common"
)

// The KServe v1beta1 Go package does not build against the k8s.io/api version
// OpenEverest requires, so the InferenceService fields the provider sets are
// mirrored here and applied as an unstructured object.
var inferenceServiceGVK = schema.GroupVersionKind{
	Group: "serving.kserve.io", Version: "v1beta1", Kind: "InferenceService",
}

type inferenceServiceSpec struct {
	Predictor predictorSpec `json:"predictor"`
}

type predictorSpec struct {
	MinReplicas          *int32            `json:"minReplicas,omitempty"`
	MaxReplicas          int32             `json:"maxReplicas,omitempty"`
	Labels               map[string]string `json:"labels,omitempty"`
	StorageContainerName string            `json:"storageContainerName,omitempty"`
	Model                *modelSpec        `json:"model,omitempty"`
}

type modelSpec struct {
	ModelFormat    modelFormat                  `json:"modelFormat"`
	Runtime        string                       `json:"runtime,omitempty"`
	StorageURI     string                       `json:"storageUri,omitempty"`
	RuntimeVersion string                       `json:"runtimeVersion,omitempty"`
	Resources      *corev1.ResourceRequirements `json:"resources,omitempty"`
}

type modelFormat struct {
	Name string `json:"name"`
}

type inferenceServiceStatus struct {
	duckv1.Status `json:",inline"`
	URL           *apis.URL `json:"url,omitempty"`
}

// KServe's v1alpha1 Go package pulls in the cloud storage SDKs, so the
// ClusterStorageContainer fields validation reads are mirrored here too.
var clusterStorageContainerGVK = schema.GroupVersionKind{
	Group: "serving.kserve.io", Version: "v1alpha1", Kind: "ClusterStorageContainer",
}

const storageContainerWorkloadInitContainer = "initContainer"

type clusterStorageContainer struct {
	Spec     storageContainerSpec `json:"spec"`
	Disabled *bool                `json:"disabled,omitempty"`
}

type storageContainerSpec struct {
	SupportedURIFormats []supportedURIFormat `json:"supportedUriFormats"`
	WorkloadType        string               `json:"workloadType,omitempty"`
}

type supportedURIFormat struct {
	Prefix string `json:"prefix,omitempty"`
	Regex  string `json:"regex,omitempty"`
}

// supports mirrors KServe's StorageContainerSpec.IsStorageUriSupported.
func (s storageContainerSpec) supports(uri string) (bool, error) {
	for _, f := range s.SupportedURIFormats {
		switch {
		case f.Prefix != "":
			if strings.HasPrefix(uri, f.Prefix) {
				return true, nil
			}
		case f.Regex != "":
			match, err := regexp.MatchString(f.Regex, uri)
			if err != nil {
				return false, err
			}
			if match {
				return true, nil
			}
		}
	}
	return false, nil
}

// validatePredictor checks the Instance spec for the predictor topology.
func validatePredictor(c *controller.Context) error {
	comp, ok := c.Instance().Spec.Components[common.ComponentPredictor]
	if !ok {
		return fmt.Errorf("the %q component is required for the %q topology", common.ComponentPredictor, common.TopologyPredictor)
	}

	var params components.ModelServerCustomSpec
	c.TryDecodeComponentParameters(comp, &params)
	if params.ModelFormat == "" {
		return fmt.Errorf("%s.parameters.modelFormat is required", common.ComponentPredictor)
	}
	if params.StorageURI == "" {
		return fmt.Errorf("%s.parameters.storageURI is required", common.ComponentPredictor)
	}
	if params.MinReplicas != nil && *params.MinReplicas < 0 {
		return fmt.Errorf("%s.parameters.minReplicas must not be negative", common.ComponentPredictor)
	}
	if params.StorageContainer != "" {
		if err := validateStorageContainer(c, params.StorageContainer, params.StorageURI); err != nil {
			return fmt.Errorf("%s.parameters.storageContainer: %w", common.ComponentPredictor, err)
		}
	}
	return nil
}

// validateStorageContainer mirrors the checks KServe's pod webhook runs on a
// named ClusterStorageContainer; failing them there leaves the Instance stuck
// provisioning with the error only in ReplicaSet events.
func validateStorageContainer(c *controller.Context, name, storageURI string) error {
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("invalid name %q: %s", name, strings.Join(errs, "; "))
	}

	obj := unstructuredObject(clusterStorageContainerGVK)
	if err := c.Client().Get(c.Context(), client.ObjectKey{Name: name}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("ClusterStorageContainer %q not found", name)
		}
		return fmt.Errorf("getting ClusterStorageContainer %q: %w", name, err)
	}
	var csc clusterStorageContainer
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &csc); err != nil {
		return fmt.Errorf("decoding ClusterStorageContainer %q: %w", name, err)
	}

	if csc.Disabled != nil && *csc.Disabled {
		return fmt.Errorf("ClusterStorageContainer %q is disabled", name)
	}
	// KServe defaults an empty workloadType to initContainer.
	if wt := csc.Spec.WorkloadType; wt != "" && wt != storageContainerWorkloadInitContainer {
		return fmt.Errorf("ClusterStorageContainer %q has workloadType %q, want %q", name, wt, storageContainerWorkloadInitContainer)
	}
	supported, err := csc.Spec.supports(storageURI)
	if err != nil {
		return fmt.Errorf("ClusterStorageContainer %q: %w", name, err)
	}
	if !supported {
		return fmt.Errorf("ClusterStorageContainer %q does not support storageURI %q", name, storageURI)
	}
	return nil
}

// buildInferenceService translates an Instance into an InferenceService.
func buildInferenceService(c *controller.Context) (*unstructured.Unstructured, error) {
	comp := c.Instance().Spec.Components[common.ComponentPredictor]

	var params components.ModelServerCustomSpec
	c.TryDecodeComponentParameters(comp, &params)

	if params.ModelFormat == "" {
		return nil, fmt.Errorf("%s.parameters.modelFormat is required", common.ComponentPredictor)
	}

	predictor := predictorSpec{Model: &modelSpec{
		ModelFormat:    modelFormat{Name: params.ModelFormat},
		Runtime:        params.Runtime,
		StorageURI:     params.StorageURI,
		RuntimeVersion: params.RuntimeVersion,
		Resources:      comp.Resources,
	}}
	predictor.Labels = c.PodLabels(common.ComponentPredictor)

	switch {
	case params.MinReplicas != nil:
		predictor.MinReplicas = params.MinReplicas
	case comp.Replicas != nil:
		predictor.MinReplicas = comp.Replicas
	}
	if params.MaxReplicas != nil {
		predictor.MaxReplicas = *params.MaxReplicas
	}
	predictor.StorageContainerName = params.StorageContainer

	meta := c.ObjectMeta(c.Name())
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	meta.Annotations[common.DeploymentModeAnnotation] = common.DeploymentModeStandard

	spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&inferenceServiceSpec{Predictor: predictor})
	if err != nil {
		return nil, fmt.Errorf("building InferenceService spec: %w", err)
	}
	isvc := unstructuredObject(inferenceServiceGVK)
	isvc.SetName(meta.Name)
	isvc.SetNamespace(meta.Namespace)
	isvc.SetLabels(meta.Labels)
	isvc.SetAnnotations(meta.Annotations)
	isvc.Object["spec"] = spec
	return isvc, nil
}

// syncPredictor creates or updates the InferenceService.
func (p *Provider) syncPredictor(c *controller.Context) error {
	isvc, err := buildInferenceService(c)
	if err != nil {
		return err
	}
	return common.Apply(c.Context(), c.Client(), c.Instance(), isvc)
}

// statusPredictor translates the InferenceService status into a provider Status.
func (p *Provider) statusPredictor(c *controller.Context) (controller.Status, error) {
	isvc := unstructuredObject(inferenceServiceGVK)
	if err := c.Get(isvc, c.Name()); err != nil {
		return controller.Provisioning("Waiting for InferenceService"), nil
	}

	var status inferenceServiceStatus
	if raw, ok := isvc.Object["status"].(map[string]any); ok {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &status); err != nil {
			return controller.Status{}, fmt.Errorf("decoding InferenceService status: %w", err)
		}
	}

	ready := status.GetCondition(apis.ConditionReady)
	if ready != nil && ready.IsTrue() {
		// A Ready service without external ingress exposes only the in-cluster
		// Service and KServe leaves Status.URL empty, so surface Ready with
		// connection details only when a URL is actually published.
		if status.URL != nil {
			return controller.ReadyWithConnectionDetails(connectionDetails(status.URL)), nil
		}
		return controller.Ready(), nil
	}

	// A not-ready InferenceService is still progressing, not failed. KServe
	// drives Ready through False during normal startup (e.g.
	// MinimumReplicasUnavailable while the storage-initializer downloads the
	// model), so surface it as Provisioning and let the condition message
	// explain the current state rather than flipping the Instance to Failed.
	return controller.Provisioning(conditionMessage(ready, "InferenceService is being created")), nil
}

// connectionDetails builds ConnectionDetails from a KServe status URL.
func connectionDetails(u *apis.URL) controller.ConnectionDetails {
	host := u.Host
	port := "80"
	if u.Scheme == "https" {
		port = "443"
	}
	if h, p, ok := strings.Cut(u.Host, ":"); ok {
		host = h
		port = p
	}
	return controller.ConnectionDetails{
		Type:     "kserve",
		Provider: common.ProviderName,
		Host:     host,
		Port:     port,
		URI:      u.String(),
	}
}

// conditionMessage returns a human-readable message for a condition.
func conditionMessage(cond *apis.Condition, fallback string) string {
	if cond == nil {
		return fallback
	}
	if cond.Message != "" {
		return cond.Message
	}
	if cond.Reason != "" {
		return cond.Reason
	}
	return fallback
}
