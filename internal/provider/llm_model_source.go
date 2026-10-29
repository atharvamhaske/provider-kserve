package provider

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	kservev1alpha2 "github.com/kserve/kserve/pkg/apis/serving/v1alpha2"

	"github.com/openeverest/provider-kserve/definition/components"
	"github.com/openeverest/provider-kserve/internal/common"
)

const (
	ociScheme       = "oci"
	ociNativeScheme = "oci+native"
)

// llmModelSchemes are the model URI schemes KServe's LLMInferenceService can
// load (pkg/controller/v1alpha2/llmisvc/workload_storage.go). Any other scheme
// fails there with "unsupported schema in model URI", visible only on the
// LLMInferenceService.
var llmModelSchemes = []string{"hf", "s3", "pvc", ociScheme, ociNativeScheme}

// validateLLMModelSource checks that KServe can make the model available to
// vLLM and that a private-registry pull secret is a valid name.
func validateLLMModelSource(params components.VllmCustomSpec) error {
	field := common.ComponentLlmEngine + ".parameters."
	scheme, _, _ := strings.Cut(params.ModelURI, "://")
	disabled := params.DisableStorageInitializer != nil && *params.DisableStorageInitializer
	switch {
	case disabled && (scheme == ociScheme || scheme == ociNativeScheme):
		// KServe skips model attachment entirely when the storage initializer is
		// disabled, so the image would never be mounted.
		return fmt.Errorf("%sdisableStorageInitializer must be false for %s:// models: KServe mounts the image in that step", field, scheme)
	case disabled:
		// The model is expected at /mnt/models already; the URI is informational.
	case !slices.Contains(llmModelSchemes, scheme):
		return fmt.Errorf("%smodelURI %q: scheme %q is not supported; use one of %s", field, params.ModelURI, scheme, strings.Join(schemeList(), ", "))
	}

	if params.ImagePullSecret != "" {
		if errs := validation.IsDNS1123Subdomain(params.ImagePullSecret); len(errs) > 0 {
			return fmt.Errorf("%simagePullSecret %q: %s", field, params.ImagePullSecret, strings.Join(errs, "; "))
		}
	}
	return nil
}

func schemeList() []string {
	out := make([]string, len(llmModelSchemes))
	for i, s := range llmModelSchemes {
		out[i] = s + "://"
	}
	return out
}

// applyImagePullSecret adds the pull secret to every vLLM pod template the
// provider builds. KServe attaches the model (modelcar or image volume) to the
// head, worker and prefill pods alike, so each needs to pull it. ImagePullSecrets
// merges by name, so secrets from presets or baseRefs are kept.
func applyImagePullSecret(spec *kservev1alpha2.LLMInferenceServiceSpec, name string) {
	if name == "" {
		return
	}
	if spec.Template == nil {
		spec.Template = &corev1.PodSpec{}
	}
	pods := []*corev1.PodSpec{spec.Template, spec.Worker}
	if spec.Prefill != nil {
		if spec.Prefill.Template == nil {
			spec.Prefill.Template = &corev1.PodSpec{}
		}
		pods = append(pods, spec.Prefill.Template, spec.Prefill.Worker)
	}
	for _, pod := range pods {
		if pod != nil {
			pod.ImagePullSecrets = append(pod.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
		}
	}
}
