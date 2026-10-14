// Package predictor contains parameter types for the predictor topology.
//
// Add fields to PredictorTopologyParameters and reference it via
// parametersSchema in topology.yaml when this topology needs parameters.
//
// +k8s:openapi-gen=true
package predictor

import (
	"encoding/json"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
)

// External access modes for the predictor topology.
const (
	ExternalAccessClusterIP    = "ClusterIP"
	ExternalAccessLoadBalancer = "LoadBalancer"
	ExternalAccessNodePort     = "NodePort"
)

// PredictorTopologyParameters defines the topology-level parameters for the
// predictor topology. Per-model configuration lives on the predictor component;
// these are cross-cutting deployment options that shape the generated
// InferenceService.
type PredictorTopologyParameters struct {
	// ExternalAccess is the client access path: ClusterIP, LoadBalancer, or
	// NodePort. When set it takes precedence over predictor.service.serviceType.
	ExternalAccess string `json:"externalAccess,omitempty"`

	// EnableMetrics emits a Prometheus Operator PodMonitor for the predictor
	// pods. Unset means enabled, as for the llm topology.
	EnableMetrics *bool `json:"enableMetrics,omitempty"`
}

// MetricsEnabled reports whether this instance should emit a PodMonitor.
func (t PredictorTopologyParameters) MetricsEnabled() bool {
	return t.EnableMetrics == nil || *t.EnableMetrics
}

// UnmarshalJSON also accepts "true"/"false" strings for enableMetrics: the UI
// renders it as a select, as in the llm topology, because the form keeps the
// value when switching topology and the llm field stores a string.
func (t *PredictorTopologyParameters) UnmarshalJSON(data []byte) error {
	type plain PredictorTopologyParameters
	aux := struct {
		*plain
		EnableMetrics json.RawMessage `json:"enableMetrics,omitempty"`
	}{plain: (*plain)(t)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	t.EnableMetrics = nil
	if len(aux.EnableMetrics) == 0 || string(aux.EnableMetrics) == "null" {
		return nil
	}
	var b bool
	if err := json.Unmarshal(aux.EnableMetrics, &b); err != nil {
		var s string
		if err := json.Unmarshal(aux.EnableMetrics, &s); err != nil {
			return fmt.Errorf("enableMetrics: %w", err)
		}
		if b, err = strconv.ParseBool(s); err != nil {
			return fmt.Errorf("enableMetrics: %w", err)
		}
	}
	t.EnableMetrics = &b
	return nil
}

// ResolvedServiceType returns the Kubernetes Service type implied by
// ExternalAccess. Empty means "defer to predictor.service.serviceType".
func (t PredictorTopologyParameters) ResolvedServiceType() corev1.ServiceType {
	switch t.ExternalAccess {
	case ExternalAccessLoadBalancer:
		return corev1.ServiceTypeLoadBalancer
	case ExternalAccessNodePort:
		return corev1.ServiceTypeNodePort
	case ExternalAccessClusterIP:
		return corev1.ServiceTypeClusterIP
	default:
		return ""
	}
}
