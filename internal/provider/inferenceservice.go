// Package provider — InferenceService (serving.kserve.io/v1beta1) builder.
package provider

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
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
	MaxReplicas               *int32                            `json:"maxReplicas,omitempty"`
	Labels                    map[string]string                 `json:"labels,omitempty"`
	StorageContainerName      string                            `json:"storageContainerName,omitempty"`
	ServiceAccountName        string                            `json:"serviceAccountName,omitempty"`
	SchedulerName             string                            `json:"schedulerName,omitempty"`
	NodeSelector              map[string]string                 `json:"nodeSelector,omitempty"`
	Affinity                  *corev1.Affinity                  `json:"affinity,omitempty"`
	Tolerations               []corev1.Toleration               `json:"tolerations,omitempty"`
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	SecurityContext           *corev1.PodSecurityContext        `json:"securityContext,omitempty"`
	DeploymentStrategy        *appsv1.DeploymentStrategy        `json:"deploymentStrategy,omitempty"`
	ImagePullSecrets          []corev1.LocalObjectReference     `json:"imagePullSecrets,omitempty"`
	Volumes                   []corev1.Volume                   `json:"volumes,omitempty"`
	Containers                []corev1.Container                `json:"containers,omitempty"`
	Model                     *modelSpec                        `json:"model,omitempty"`
}

type modelSpec struct {
	ModelFormat     modelFormat                  `json:"modelFormat"`
	Runtime         string                       `json:"runtime,omitempty"`
	StorageURI      string                       `json:"storageUri,omitempty"`
	RuntimeVersion  string                       `json:"runtimeVersion,omitempty"`
	Resources       *corev1.ResourceRequirements `json:"resources,omitempty"`
	Env             []corev1.EnvVar              `json:"env,omitempty"`
	Args            []string                     `json:"args,omitempty"`
	SecurityContext *corev1.SecurityContext      `json:"securityContext,omitempty"`
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

	// KServe treats the container with this name as the model server: it gets
	// the storage-initializer mount and the Service routes to its first port.
	kserveContainerName = "kserve-container"
	// A container with this name in the predictor pod receives the traffic
	// instead (KServe's predictor/transformer collocation).
	transformerContainerName = "transformer-container"
	storageURIEnvVar         = "STORAGE_URI"
	sharedMemoryVolume       = "dshm"

	metricsPortAnnotation = "prometheus.kserve.io/port"
	metricsPathAnnotation = "prometheus.kserve.io/path"
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
	if params.UsesCustomContainer() {
		if err := validateCustomContainer(params); err != nil {
			return err
		}
	} else {
		if err := validateRuntimeModel(params); err != nil {
			return err
		}
	}
	// KServe's HPA raises a floor below 1 to 1 in Standard mode, so 0 would
	// silently keep one replica instead of scaling to zero.
	if params.MinReplicas != nil && *params.MinReplicas < 1 {
		return fmt.Errorf("%s.parameters.minReplicas must be at least 1: scale-to-zero is not supported in KServe Standard mode", common.ComponentPredictor)
	}
	if params.MinReplicas == nil && comp.Replicas != nil && *comp.Replicas < 1 {
		return fmt.Errorf("%s.replicas must be at least 1: scale-to-zero is not supported in KServe Standard mode", common.ComponentPredictor)
	}
	if params.GpuCount != nil && *params.GpuCount < 0 {
		return fmt.Errorf("%s.parameters.gpuCount must not be negative", common.ComponentPredictor)
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
	case "", predictor.ExternalAccessClusterIP, predictor.ExternalAccessLoadBalancer, predictor.ExternalAccessNodePort, predictor.ExternalAccessEnvoyAIGateway:
	default:
		return fmt.Errorf("topology.parameters.externalAccess must be ClusterIP, LoadBalancer, NodePort or EnvoyAIGateway")
	}
	if topo.UsesAIGateway() {
		if !common.AIGatewayEnabled() {
			return fmt.Errorf("topology.parameters.externalAccess EnvoyAIGateway requires aiGateway.enabled in the provider chart")
		}
		if _, ok := predictorGatewaySchema(params); !ok {
			return fmt.Errorf("topology.parameters.externalAccess EnvoyAIGateway needs a predictor that serves an API the gateway routes: "+
				"modelFormat %q with task %q, or an image with apiSchema (or openAICompatible)", huggingFaceModelFormat, textEmbeddingTask)
		}
		if err := validateAIGatewayAccess(c, c.Name(), "the predictor serves the model under the Instance name, so use another Instance name"); err != nil {
			return err
		}
	} else if params.BackendAPIKeySecret != "" {
		return fmt.Errorf("%s.parameters.backendAPIKeySecret requires topology.parameters.externalAccess EnvoyAIGateway", common.ComponentPredictor)
	}
	if topo.TokenLimitPerHour != nil {
		if !topo.UsesAIGateway() {
			return fmt.Errorf("topology.parameters.tokenLimitPerHour requires externalAccess EnvoyAIGateway")
		}
		if *topo.TokenLimitPerHour < 1 {
			return fmt.Errorf("topology.parameters.tokenLimitPerHour must be greater than zero")
		}
	}
	if err := validatePredictorMetadata(c, params); err != nil {
		return err
	}
	if params.Config != "" {
		return validatePredictorConfig(c)
	}
	return nil
}

// validatePredictorMetadata rejects user labels and annotations that are
// malformed or that would override keys the provider manages.
func validatePredictorMetadata(c *controller.Context, params components.ModelServerCustomSpec) error {
	reserved := providerLabelKeys(c)
	for _, k := range slices.Sorted(maps.Keys(params.Labels)) {
		if _, ok := reserved[k]; ok {
			return fmt.Errorf("%s.parameters.labels: %q is managed by the provider", common.ComponentPredictor, k)
		}
		errs := append(validation.IsQualifiedName(k), validation.IsValidLabelValue(params.Labels[k])...)
		if len(errs) > 0 {
			return fmt.Errorf("%s.parameters.labels: invalid label %q: %s", common.ComponentPredictor, k, strings.Join(errs, "; "))
		}
	}
	for _, k := range slices.Sorted(maps.Keys(params.Annotations)) {
		if k == common.DeploymentModeAnnotation {
			return fmt.Errorf("%s.parameters.annotations: %q is managed by the provider", common.ComponentPredictor, k)
		}
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return fmt.Errorf("%s.parameters.annotations: invalid key %q: %s", common.ComponentPredictor, k, strings.Join(errs, "; "))
		}
	}
	return nil
}

// validateRuntimeModel checks a predictor served by a KServe serving runtime.
func validateRuntimeModel(params components.ModelServerCustomSpec) error {
	if params.ModelFormat == "" {
		return fmt.Errorf("%s.parameters.modelFormat is required", common.ComponentPredictor)
	}
	if params.StorageURI == "" {
		return fmt.Errorf("%s.parameters.storageURI is required", common.ComponentPredictor)
	}
	if params.Task != "" && !strings.EqualFold(params.ModelFormat, huggingFaceModelFormat) {
		return fmt.Errorf("%s.parameters.task is only supported with modelFormat %q", common.ComponentPredictor, huggingFaceModelFormat)
	}
	imageOnly := map[string]bool{
		"command":          len(params.Command) > 0,
		"port":             params.Port != nil,
		"healthPath":       params.HealthPath != "",
		"openAICompatible":    params.OpenAICompatible,
		"apiSchema":           params.APISchema != "",
		"backendAPIKeySecret": params.BackendAPIKeySecret != "",
		"servedModelName":     params.ServedModelName != "",
		"imagePullSecret":     params.ImagePullSecret != "",
	}
	for _, field := range slices.Sorted(maps.Keys(imageOnly)) {
		if imageOnly[field] {
			return fmt.Errorf("%s.parameters.%s requires image", common.ComponentPredictor, field)
		}
	}
	return nil
}

// validateCustomContainer checks a predictor that runs a user image.
func validateCustomContainer(params components.ModelServerCustomSpec) error {
	runtimeOnly := map[string]string{
		"modelFormat":    params.ModelFormat,
		"runtime":        params.Runtime,
		"runtimeVersion": params.RuntimeVersion,
		"task":           params.Task,
	}
	for _, field := range slices.Sorted(maps.Keys(runtimeOnly)) {
		if runtimeOnly[field] != "" {
			return fmt.Errorf("%s.parameters.%s selects a KServe serving runtime and cannot be combined with image", common.ComponentPredictor, field)
		}
	}
	if port := params.ContainerPort(); port < 1 || port > 65535 {
		return fmt.Errorf("%s.parameters.port must be between 1 and 65535", common.ComponentPredictor)
	}
	if params.HealthPath != "" && !strings.HasPrefix(params.HealthPath, "/") {
		return fmt.Errorf("%s.parameters.healthPath must start with /", common.ComponentPredictor)
	}
	if params.ImagePullSecret != "" {
		if errs := validation.IsDNS1123Subdomain(params.ImagePullSecret); len(errs) > 0 {
			return fmt.Errorf("%s.parameters.imagePullSecret: %s", common.ComponentPredictor, strings.Join(errs, "; "))
		}
	}
	switch params.APISchema {
	case "", components.APISchemaOpenAI:
	case components.APISchemaTypeSafe:
		if params.OpenAICompatible {
			return fmt.Errorf("%s.parameters.openAICompatible conflicts with apiSchema %q", common.ComponentPredictor, params.APISchema)
		}
	default:
		return fmt.Errorf("%s.parameters.apiSchema must be %s or %s", common.ComponentPredictor, components.APISchemaOpenAI, components.APISchemaTypeSafe)
	}
	if params.BackendAPIKeySecret != "" {
		if errs := validation.IsDNS1123Subdomain(params.BackendAPIKeySecret); len(errs) > 0 {
			return fmt.Errorf("%s.parameters.backendAPIKeySecret: %s", common.ComponentPredictor, strings.Join(errs, "; "))
		}
	}
	if params.StorageContainer != "" && params.StorageURI == "" {
		return fmt.Errorf("%s.parameters.storageContainer requires storageURI", common.ComponentPredictor)
	}
	return nil
}

// providerLabelKeys are the labels the provider puts on the InferenceService
// and the predictor pods.
func providerLabelKeys(c *controller.Context) map[string]struct{} {
	keys := map[string]struct{}{
		controller.ProviderLabel:  {},
		controller.InstanceLabel:  {},
		controller.ComponentLabel: {},
	}
	for k := range c.ObjectMeta(c.Name()).Labels {
		keys[k] = struct{}{}
	}
	return keys
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

	resources := predictorResources(comp.Resources, params.GpuCount)
	var predictor predictorSpec
	switch {
	case params.UsesCustomContainer():
		predictor.Containers = []corev1.Container{customPredictorContainer(params, resources)}
		predictor.Volumes = []corev1.Volume{{
			Name:         sharedMemoryVolume,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}},
		}}
		if params.ImagePullSecret != "" {
			predictor.ImagePullSecrets = []corev1.LocalObjectReference{{Name: params.ImagePullSecret}}
		}
	case params.ModelFormat == "":
		return nil, fmt.Errorf("%s.parameters.modelFormat is required", common.ComponentPredictor)
	default:
		predictor.Model = &modelSpec{
			ModelFormat:     modelFormat{Name: params.ModelFormat},
			Runtime:         params.Runtime,
			StorageURI:      params.StorageURI,
			RuntimeVersion:  params.RuntimeVersion,
			Resources:       resources,
			Env:             params.Env,
			Args:            predictorArgs(params, resources),
			SecurityContext: params.ContainerSecurityContext,
		}
	}
	predictor.ServiceAccountName = params.ServiceAccountName
	predictor.SecurityContext = params.SecurityContext
	predictor.Labels = c.PodLabels(common.ComponentPredictor)
	if policy := comp.SchedulingPolicy; policy != nil {
		predictor.SchedulerName = policy.SchedulerName
		predictor.NodeSelector = policy.NodeSelector
		predictor.Affinity = policy.Affinity
		predictor.Tolerations = policy.Tolerations
		predictor.TopologySpreadConstraints = controller.TopologySpreadConstraints(policy, predictor.Labels)
	}
	if requestsGPU(resources) {
		predictor.Tolerations = withGPUTolerations(predictor.Tolerations, resources)
		predictor.DeploymentStrategy = gpuDeploymentStrategy()
	}

	switch {
	case params.MinReplicas != nil:
		predictor.MinReplicas = params.MinReplicas
	case comp.Replicas != nil:
		predictor.MinReplicas = comp.Replicas
	}
	predictor.MaxReplicas = params.MaxReplicas
	predictor.StorageContainerName = params.StorageContainer
	if usesHFTokenServiceAccount(params) {
		predictor.ServiceAccountName = modelPullerSAName(c.Name())
	}

	// KServe copies InferenceService labels and annotations to the predictor
	// pods; provider-managed keys are applied last so they always win.
	meta := c.ObjectMeta(c.Name())
	meta.Labels = mergeMaps(params.Labels, meta.Labels)
	annotations := params.Annotations
	if params.UsesCustomContainer() {
		// The PodMonitor reads the metrics endpoint from these; a serving
		// runtime would set them, a custom image has none.
		annotations = mergeMaps(map[string]string{
			metricsPortAnnotation: strconv.Itoa(int(params.ContainerPort())),
			metricsPathAnnotation: "/metrics",
		}, annotations)
	}
	meta.Annotations = mergeMaps(annotations, map[string]string{
		common.DeploymentModeAnnotation: common.DeploymentModeStandard,
	})

	spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&inferenceServiceSpec{Predictor: predictor})
	if err != nil {
		return nil, fmt.Errorf("building InferenceService spec: %w", err)
	}
	if params.Config != "" {
		spec, err = mergePredictorConfig(params.Config, spec)
		if err != nil {
			return nil, fmt.Errorf("%s.parameters.config: %w", common.ComponentPredictor, err)
		}
	}
	isvc := unstructuredObject(inferenceServiceGVK)
	isvc.SetName(meta.Name)
	isvc.SetNamespace(meta.Namespace)
	isvc.SetLabels(meta.Labels)
	isvc.SetAnnotations(meta.Annotations)
	isvc.Object["spec"] = spec
	return isvc, nil
}

// customPredictorContainer is the model server container for a user image.
func customPredictorContainer(params components.ModelServerCustomSpec, resources *corev1.ResourceRequirements) corev1.Container {
	port := params.ContainerPort()
	container := corev1.Container{
		Name:            kserveContainerName,
		Image:           params.Image,
		Command:         params.Command,
		Args:            params.Args,
		Env:             params.Env,
		Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: port, Protocol: corev1.ProtocolTCP}},
		SecurityContext: params.ContainerSecurityContext,
		// PyTorch, vLLM and Triton need more than the 64Mi /dev/shm default.
		VolumeMounts: []corev1.VolumeMount{{Name: sharedMemoryVolume, MountPath: "/dev/shm"}},
	}
	if params.StorageURI != "" {
		container.Env = append(slices.Clone(params.Env), corev1.EnvVar{Name: storageURIEnvVar, Value: params.StorageURI})
	}
	if resources != nil {
		container.Resources = *resources
	}
	if params.HealthPath != "" {
		container.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: params.HealthPath,
				Port: intstr.FromInt32(port),
			}},
			PeriodSeconds: 10,
		}
	}
	return container
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

// withGPUTolerations tolerates the NoSchedule taint named after each GPU
// resource the predictor requests (e.g. nvidia.com/gpu, set by the NVIDIA GPU
// Operator and many managed GPU node pools), unless a toleration already
// covers it.
func withGPUTolerations(tolerations []corev1.Toleration, res *corev1.ResourceRequirements) []corev1.Toleration {
	out := slices.Clone(tolerations)
	for _, name := range slices.Sorted(maps.Keys(res.Limits)) {
		if !strings.HasSuffix(string(name), "/gpu") {
			continue
		}
		key := string(name)
		covered := slices.ContainsFunc(out, func(t corev1.Toleration) bool {
			keyMatch := t.Key == key || (t.Key == "" && t.Operator == corev1.TolerationOpExists)
			return keyMatch && (t.Effect == "" || t.Effect == corev1.TaintEffectNoSchedule)
		})
		if !covered {
			out = append(out, corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule})
		}
	}
	return out
}

// gpuDeploymentStrategy replaces pods one at a time without surging: a surge
// pod would wait for a GPU still held by the pod it replaces.
func gpuDeploymentStrategy() *appsv1.DeploymentStrategy {
	return &appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxSurge:       ptr.To(intstr.FromInt32(0)),
			MaxUnavailable: ptr.To(intstr.FromInt32(1)),
		},
	}
}

// usesHFTokenServiceAccount reports whether the predictor downloads from the
// Hugging Face Hub with the chart's token configured; KServe's
// storage-initializer reads the token from the pod's ServiceAccount. A
// user-set serviceAccountName wins, so that account must carry the token.
func usesHFTokenServiceAccount(params components.ModelServerCustomSpec) bool {
	return params.ServiceAccountName == "" && common.HFTokenSecretName() != "" && strings.HasPrefix(params.StorageURI, "hf://")
}

// resolveCatalogModel fills modelFormat, storageURI and task from the catalog
// entry named by model. Setting modelFormat or storageURI opts out of the
// catalog: mixing a catalog URI with another format would serve the wrong
// runtime. A custom container image ignores the catalog.
func resolveCatalogModel(params components.ModelServerCustomSpec) (components.ModelServerCustomSpec, error) {
	if params.Model == "" || params.ModelFormat != "" || params.StorageURI != "" || params.UsesCustomContainer() {
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

// mergeMaps returns a new map with base's entries overridden by override's.
func mergeMaps(base, override map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(override))
	maps.Copy(out, base)
	maps.Copy(out, override)
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
	if usesHFTokenServiceAccount(params) {
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
	if err := syncPredictorAIGateway(c, topo, params); err != nil {
		return err
	}
	return reconcileExternalService(c, comp.Service, predictorServiceType(c), c.PodLabels(common.ComponentPredictor), predictorServicePort, predictorTargetPort(isvc))
}

// predictorTargetPort mirrors how KServe picks the predictor Service's target:
// the first port of the transformer-container if the pod has one, else of the
// first container, else the runtimes' 8080.
func predictorTargetPort(isvc *unstructured.Unstructured) int32 {
	containers, _, _ := unstructured.NestedSlice(isvc.Object, "spec", "predictor", "containers")
	if len(containers) == 0 {
		return predictorContainerPort
	}
	container, _ := containers[0].(map[string]any)
	for _, raw := range containers {
		if c, _ := raw.(map[string]any); c != nil && c["name"] == transformerContainerName {
			container = c
			break
		}
	}
	ports, _, _ := unstructured.NestedSlice(container, "ports")
	if len(ports) == 0 {
		return predictorContainerPort
	}
	port, _ := ports[0].(map[string]any)
	switch p := port["containerPort"].(type) {
	case int64:
		return int32(p)
	case float64:
		return int32(p)
	}
	return predictorContainerPort
}

func predictorServiceType(c *controller.Context) corev1.ServiceType {
	var topo predictor.PredictorTopologyParameters
	c.TryDecodeTopologyParameters(&topo)
	return resolveServiceType(topo.ResolvedServiceType(), c.Instance().Spec.Components[common.ComponentPredictor].Service)
}

// kserveOpenAIPrefix is where KServe's Hugging Face runtime serves the OpenAI
// API; OpenAI-compatible servers (vLLM, TEI, llama.cpp, NIM) use /v1.
const (
	kserveOpenAIPrefix = "/openai/v1"
	openAIPrefix       = "/v1"
)

// predictorGatewaySchema returns the API the predictor serves to the Envoy AI
// Gateway, or false when it serves none the gateway can route. Of the
// runtimes, only Hugging Face embeddings qualify: the AI Gateway route exposes
// no other runtime endpoint.
func predictorGatewaySchema(params components.ModelServerCustomSpec) (aiGatewaySchema, bool) {
	switch {
	case params.UsesCustomContainer():
		switch params.ResolvedAPISchema() {
		case components.APISchemaOpenAI:
			return aiGatewaySchema{name: components.APISchemaOpenAI, prefix: openAIPrefix}, true
		case components.APISchemaTypeSafe:
			return aiGatewaySchema{name: components.APISchemaTypeSafe}, true
		}
		return aiGatewaySchema{}, false
	case strings.EqualFold(params.ModelFormat, huggingFaceModelFormat) && params.Task == textEmbeddingTask:
		return aiGatewaySchema{name: components.APISchemaOpenAI, prefix: kserveOpenAIPrefix}, true
	}
	return aiGatewaySchema{}, false
}

// syncPredictorAIGateway registers the predictor on the shared AI Gateway, or
// removes its gateway objects when another access path is selected.
func syncPredictorAIGateway(c *controller.Context, topo predictor.PredictorTopologyParameters, params components.ModelServerCustomSpec) error {
	if !topo.UsesAIGateway() {
		if !common.AIGatewayEnabled() {
			return nil
		}
		if err := cleanupAIGateway(c); err != nil {
			return err
		}
		if err := deleteAIGatewayObject(c, backendSecurityPolicyGVK, aiGatewayRouteSuffix); err != nil {
			return err
		}
		if err := deleteAIGatewayObject(c, aiServiceBackendGVK, aiGatewayRouteSuffix); err != nil {
			return err
		}
		return deleteAIGatewayObject(c, envoyBackendGVK, aiGatewayRouteSuffix)
	}
	apiSchema, _ := predictorGatewaySchema(params)
	host := fmt.Sprintf("%s-predictor.%s.svc.cluster.local", c.Name(), c.Namespace())
	backend, aiBackend := buildServiceBackends(c.Name(), c.Namespace(), host, predictorServicePort, apiSchema)
	backends := []*unstructured.Unstructured{backend, aiBackend}
	if params.BackendAPIKeySecret != "" {
		backends = append(backends, buildBackendAPIKeyPolicy(c.Name(), c.Namespace(), params.BackendAPIKeySecret))
	} else if err := deleteAIGatewayObject(c, backendSecurityPolicyGVK, aiGatewayRouteSuffix); err != nil {
		return err
	}
	return syncAIGateway(c, c.Name(), topo.TokenLimitPerHour, predictorAIBackendRef(aiBackend.GetName(), c.Name(), params), backends...)
}

// predictorAIBackendRef routes to the AIServiceBackend. Clients send the
// Instance name as the model; servers that check it get servedModelName.
func predictorAIBackendRef(backendName, instanceName string, params components.ModelServerCustomSpec) map[string]any {
	ref := map[string]any{"name": backendName}
	if params.ServedModelName != "" && params.ServedModelName != instanceName {
		ref["modelNameOverride"] = params.ServedModelName
	}
	return ref
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
		var topo predictor.PredictorTopologyParameters
		c.TryDecodeTopologyParameters(&topo)
		if topo.UsesAIGateway() {
			details, waiting, err := aiGatewayConnectionDetails(c, c.Name(), predictorClientBasePath(c))
			if err != nil {
				return controller.Status{}, err
			}
			if waiting != "" {
				return controller.Provisioning(waiting), nil
			}
			return controller.ReadyWithConnectionDetails(details), nil
		}
		if details := predictorConnectionDetails(c, status); details != nil {
			if model := predictorServedModelName(c); model != "" {
				details.AdditionalProperties = map[string]string{"model": model}
			}
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

// predictorClientBasePath is the gateway path clients of the predictor use as
// their base URL: /typesafe for a TypeSafe image, /v1 otherwise.
func predictorClientBasePath(c *controller.Context) string {
	var params components.ModelServerCustomSpec
	c.TryDecodeComponentParameters(c.Instance().Spec.Components[common.ComponentPredictor], &params)
	if apiSchema, ok := predictorGatewaySchema(params); ok {
		return apiSchema.clientBasePath()
	}
	return openAIBasePath
}

// predictorServedModelName is the model name clients send directly to the
// predictor: KServe runtimes use the InferenceService name, a custom image
// whatever servedModelName declares (unknown when unset).
func predictorServedModelName(c *controller.Context) string {
	var params components.ModelServerCustomSpec
	c.TryDecodeComponentParameters(c.Instance().Spec.Components[common.ComponentPredictor], &params)
	if params.UsesCustomContainer() {
		return params.ServedModelName
	}
	return c.Name()
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
