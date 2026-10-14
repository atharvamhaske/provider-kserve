package provider

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/json"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-kserve/internal/common"
)

func mergePredictorConfig(raw string, generated map[string]any) (map[string]any, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(raw)))
	doc, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("reading YAML: %w", err)
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config must contain exactly one YAML document")
	}
	data, err := yaml.YAMLToJSONStrict(doc)
	if err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("config must be a spec object: %w", err)
	}
	if config == nil {
		return nil, fmt.Errorf("config must be a spec object")
	}
	for _, key := range []string{"apiVersion", "kind", "metadata", "spec", "status"} {
		if _, ok := config[key]; ok {
			return nil, fmt.Errorf("provide only the InferenceService spec body, not %q", key)
		}
	}
	mergeSpecMaps(config, generated)
	return config, nil
}

// mergeSpecMaps overlays generated fields in place; lists and scalars are atomic.
func mergeSpecMaps(config, generated map[string]any) {
	for key, value := range generated {
		base, baseOK := config[key].(map[string]any)
		override, overrideOK := value.(map[string]any)
		if baseOK && overrideOK {
			mergeSpecMaps(base, override)
		} else {
			config[key] = value
		}
	}
}

// validatePredictorConfig runs the same apply as Sync without persisting it.
// The installed CRD and admission webhooks validate fields absent from our Go types.
func validatePredictorConfig(c *controller.Context) error {
	isvc, err := buildInferenceService(c)
	if err != nil {
		return err
	}
	// API validation also runs before the Instance has been persisted.
	if c.Instance().UID != "" {
		if err := controllerutil.SetControllerReference(c.Instance(), isvc, c.Client().Scheme()); err != nil {
			return fmt.Errorf("setting InferenceService owner: %w", err)
		}
	}
	//nolint:staticcheck // KServe's unstructured resources lack apply configurations.
	if err := c.Client().Patch(c.Context(), isvc, client.Apply,
		client.FieldOwner(common.FieldOwner), client.ForceOwnership,
		client.DryRunAll, client.FieldValidation(metav1.FieldValidationStrict)); err != nil {
		return fmt.Errorf("%s.parameters.config: validating merged InferenceService: %w", common.ComponentPredictor, err)
	}
	return nil
}
