// Package provider — InferenceService (serving.kserve.io/v1beta1) builder.
package provider

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-kserve/definition/components"
	"github.com/openeverest/provider-kserve/definition/topologies/predictor"
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
	MinReplicas               *int32                            `json:"minReplicas,omitempty"`
	MaxReplicas               int32                             `json:"maxReplicas,omitempty"`
	Labels                    map[string]string                 `json:"labels,omitempty"`
	StorageContainerName      string                            `json:"storageContainerName,omitempty"`
	ServiceAccountName        string                            `json:"serviceAccountName,omitempty"`
	SchedulerName             string                            `json:"schedulerName,omitempty"`
	NodeSelector              map[string]string                 `json:"nodeSelector,omitempty"`
	Affinity                  *corev1.Affinity                  `json:"affinity,omitempty"`
	Tolerations               []corev1.Toleration               `json:"tolerations,omitempty"`
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	Model                     *modelSpec                        `json:"model,omitempty"`
}

type modelSpec struct {
	ModelFormat    modelFormat                  `json:"modelFormat"`
	Runtime        string                       `json:"runtime,omitempty"`
	StorageURI     string                       `json:"storageUri,omitempty"`
	RuntimeVersion string                       `json:"runtimeVersion,omitempty"`
	Resources      *corev1.ResourceRequirements `json:"resources,omitempty"`
	Env            []corev1.EnvVar              `json:"env,omitempty"`
	Args           []string                     `json:"args,omitempty"`
}

type modelFormat struct {
	Name string `json:"name"`
}

type inferenceServiceStatus struct {
	duckv1.Status `json:",inline"`
	Address       *duckv1.Addressable `json:"address,omitempty"`
}

const (
	// predictorServicePort is the port of KServe's in-cluster predictor Service
	// in Standard mode; it forwards to predictorContainerPort, where KServe
	// runtimes listen.
	predictorServicePort   = 80
	predictorContainerPort = 8080

	huggingFaceModelFormat = "huggingface"
	textEmbeddingTask      = "text_embedding"
)

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
	// KServe's webhook rejects other InferenceService names, which would leave
	// the Instance without a status.
	if errs := validation.IsDNS1035Label(c.Name()); len(errs) > 0 {
		return fmt.Errorf("instance name %q is not valid for a predictor: %s", c.Name(), strings.Join(errs, "; "))
	}

	var params components.ModelServerCustomSpec
	c.TryDecodeComponentParameters(comp, &params)
	params, err := resolveCatalogModel(params)
	if err != nil {
		return err
	}
	if params.ModelFormat == "" {
		return fmt.Errorf("%s.parameters.modelFormat is required", common.ComponentPredictor)
	}
	if params.StorageURI == "" {
		return fmt.Errorf("%s.parameters.storageURI is required", common.ComponentPredictor)
	}
	if params.MinReplicas != nil && *params.MinReplicas < 0 {
		return fmt.Errorf("%s.parameters.minReplicas must not be negative", common.ComponentPredictor)
	}
	if params.GpuCount != nil && *params.GpuCount < 0 {
		return fmt.Errorf("%s.parameters.gpuCount must not be negative", common.ComponentPredictor)
	}
	if params.Task != "" && !strings.EqualFold(params.ModelFormat, huggingFaceModelFormat) {
		return fmt.Errorf("%s.parameters.task is only supported with modelFormat %q", common.ComponentPredictor, huggingFaceModelFormat)
	}
	if params.StorageContainer != "" {
		if err := validateStorageContainer(c, params.StorageContainer, params.StorageURI); err != nil {
			return fmt.Errorf("%s.parameters.storageContainer: %w", common.ComponentPredictor, err)
		}
	}
	if comp.Service != nil {
		switch comp.Service.ServiceType {
		case "", corev1.ServiceTypeClusterIP, corev1.ServiceTypeLoadBalancer, corev1.ServiceTypeNodePort:
		default:
			return fmt.Errorf("%s.service.serviceType must be ClusterIP, LoadBalancer or NodePort", common.ComponentPredictor)
		}
	}

	var topo predictor.PredictorTopologyParameters
	c.TryDecodeTopologyParameters(&topo)
	switch topo.ExternalAccess {
	case "", predictor.ExternalAccessClusterIP, predictor.ExternalAccessLoadBalancer, predictor.ExternalAccessNodePort:
	default:
		return fmt.Errorf("topology.parameters.externalAccess must be ClusterIP, LoadBalancer or NodePort")
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
	params, err := resolveCatalogModel(params)
	if err != nil {
		return nil, err
	}

	if params.ModelFormat == "" {
		return nil, fmt.Errorf("%s.parameters.modelFormat is required", common.ComponentPredictor)
	}

	resources := predictorResources(comp.Resources, params.GpuCount)
	predictor := predictorSpec{Model: &modelSpec{
		ModelFormat:    modelFormat{Name: params.ModelFormat},
		Runtime:        params.Runtime,
		StorageURI:     params.StorageURI,
		RuntimeVersion: params.RuntimeVersion,
		Resources:      resources,
		Env:            params.Env,
		Args:           predictorArgs(params, resources),
	}}
	predictor.Labels = c.PodLabels(common.ComponentPredictor)
	if policy := comp.SchedulingPolicy; policy != nil {
		predictor.SchedulerName = policy.SchedulerName
		predictor.NodeSelector = policy.NodeSelector
		predictor.Affinity = policy.Affinity
		predictor.Tolerations = policy.Tolerations
		predictor.TopologySpreadConstraints = controller.TopologySpreadConstraints(policy, predictor.Labels)
	}

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
	if usesHFTokenServiceAccount(params.StorageURI) {
		predictor.ServiceAccountName = modelPullerSAName(c.Name())
	}

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

// predictorArgs adds the huggingface runtime flags the provider manages to the
// user's args: --task from the task parameter, and the transformers backend
// when no GPU is requested (the runtime otherwise picks vLLM, which fails to
// start without one). On a GPU the runtime uses vLLM, which ignores --task and
// serves decoder-based embedding models (e.g. Qwen3-Embedding) as text
// generators unless told to pool. Flags the user passed explicitly win.
func predictorArgs(params components.ModelServerCustomSpec, res *corev1.ResourceRequirements) []string {
	if !strings.EqualFold(params.ModelFormat, huggingFaceModelFormat) {
		return params.Args
	}
	args := slices.Clone(params.Args)
	embedding := false
	if params.Task != "" && !hasFlag(args, "--task") {
		args = append([]string{"--task=" + params.Task}, args...)
		embedding = params.Task == textEmbeddingTask
	}
	gpu := requestsGPU(res)
	if !gpu && !hasFlag(args, "--backend") {
		args = append(args, "--backend=huggingface")
	}
	if gpu && embedding && !hasFlag(args, "--runner") {
		args = append(args, "--runner=pooling")
	}
	return args
}

func hasFlag(args []string, flag string) bool {
	return slices.ContainsFunc(args, func(a string) bool {
		return a == flag || strings.HasPrefix(a, flag+"=")
	})
}

func requestsGPU(res *corev1.ResourceRequirements) bool {
	if res == nil {
		return false
	}
	for _, list := range []corev1.ResourceList{res.Limits, res.Requests} {
		for name, qty := range list {
			if strings.HasSuffix(string(name), "/gpu") && !qty.IsZero() {
				return true
			}
		}
	}
	return false
}

// usesHFTokenServiceAccount reports whether the predictor downloads from the
// Hugging Face Hub with the chart's token configured; KServe's
// storage-initializer reads the token from the pod's ServiceAccount.
func usesHFTokenServiceAccount(storageURI string) bool {
	return common.HFTokenSecretName() != "" && strings.HasPrefix(storageURI, "hf://")
}

// resolveCatalogModel fills modelFormat, storageURI and task from the catalog
// entry named by model. Setting modelFormat or storageURI opts out of the
// catalog: mixing a catalog URI with another format would serve the wrong
// runtime.
func resolveCatalogModel(params components.ModelServerCustomSpec) (components.ModelServerCustomSpec, error) {
	if params.Model == "" || params.ModelFormat != "" || params.StorageURI != "" {
		return params, nil
	}
	entry, ok := common.LookupPredictorModel(params.Model)
	if !ok {
		return params, fmt.Errorf("%s.parameters.model: %q is not in the provider's model catalog", common.ComponentPredictor, params.Model)
	}
	params.ModelFormat = entry.ModelFormat
	params.StorageURI = entry.StorageURI
	if params.Task == "" {
		params.Task = entry.Task
	}
	if params.GpuCount == nil && entry.GpuCount > 0 {
		params.GpuCount = &entry.GpuCount
	}
	return params, nil
}

// predictorResources adds the nvidia.com/gpu limit for gpuCount (the UI cannot
// write that resource key directly) and defaults each missing request to its
// limit. Without that KServe fills in the serving runtime's requests (2Gi
// memory for most), and a lower limit makes the Deployment invalid.
func predictorResources(res *corev1.ResourceRequirements, gpuCount *int32) *corev1.ResourceRequirements {
	gpu := gpuCount != nil && *gpuCount > 0
	if res == nil && !gpu {
		return nil
	}
	out := &corev1.ResourceRequirements{}
	if res != nil {
		out = res.DeepCopy()
	}
	if gpu {
		if out.Limits == nil {
			out.Limits = corev1.ResourceList{}
		}
		out.Limits[nvidiaGPUResource] = *resource.NewQuantity(int64(*gpuCount), resource.DecimalSI)
	}
	for name, limit := range out.Limits {
		if out.Requests == nil {
			out.Requests = corev1.ResourceList{}
		}
		if _, ok := out.Requests[name]; !ok {
			out.Requests[name] = limit
		}
	}
	return out
}

// syncPredictor creates or updates the InferenceService and the optional
// external Service in front of it.
func (p *Provider) syncPredictor(c *controller.Context) error {
	comp := c.Instance().Spec.Components[common.ComponentPredictor]
	var params components.ModelServerCustomSpec
	c.TryDecodeComponentParameters(comp, &params)
	params, err := resolveCatalogModel(params)
	if err != nil {
		return err
	}
	if usesHFTokenServiceAccount(params.StorageURI) {
		if err := ensureModelPullerServiceAccount(c); err != nil {
			return err
		}
	}

	isvc, err := buildInferenceService(c)
	if err != nil {
		return err
	}
	if err := common.Apply(c.Context(), c.Client(), c.Instance(), isvc); err != nil {
		return err
	}
	var topo predictor.PredictorTopologyParameters
	c.TryDecodeTopologyParameters(&topo)
	if err := syncPodMonitor(c, topo.MetricsEnabled(), buildPredictorPodMonitor(c.Name(), c.Instance().Namespace, c.PodLabels(common.ComponentPredictor), common.PodMonitorInterval())); err != nil {
		return err
	}
	return reconcileExternalService(c, comp.Service, predictorServiceType(c), c.PodLabels(common.ComponentPredictor), predictorServicePort, predictorContainerPort)
}

func predictorServiceType(c *controller.Context) corev1.ServiceType {
	var topo predictor.PredictorTopologyParameters
	c.TryDecodeTopologyParameters(&topo)
	return resolveServiceType(topo.ResolvedServiceType(), c.Instance().Spec.Components[common.ComponentPredictor].Service)
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
		if details := predictorConnectionDetails(c, status); details != nil {
			// KServe runtimes serve the model under the InferenceService name.
			details.AdditionalProperties = map[string]string{"model": c.Name()}
			return controller.ReadyWithConnectionDetails(*details), nil
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

// predictorConnectionDetails picks the address clients should use: the
// provider-owned external Service when one is requested (nil until it has an
// address), otherwise KServe's in-cluster predictor Service. status.url is
// ignored: KServe derives it from its ingress domain (example.com by default)
// whether or not any ingress serves it.
func predictorConnectionDetails(c *controller.Context, status inferenceServiceStatus) *controller.ConnectionDetails {
	if svcType := predictorServiceType(c); svcType != corev1.ServiceTypeClusterIP {
		return externalServiceConnectionDetails(c, svcType, predictorServicePort)
	}
	if status.Address != nil && status.Address.URL != nil {
		d := connectionDetails(status.Address.URL)
		return &d
	}
	host := fmt.Sprintf("%s-predictor.%s.svc.cluster.local", c.Name(), c.Namespace())
	d := kserveConnectionDetails(host, strconv.Itoa(predictorServicePort))
	return &d
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
