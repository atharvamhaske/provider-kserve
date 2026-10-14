package predictor

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestResolvedServiceType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    PredictorTopologyParameters
		want corev1.ServiceType
	}{
		{name: "empty"},
		{name: "clusterip", p: PredictorTopologyParameters{ExternalAccess: ExternalAccessClusterIP}, want: corev1.ServiceTypeClusterIP},
		{name: "lb", p: PredictorTopologyParameters{ExternalAccess: ExternalAccessLoadBalancer}, want: corev1.ServiceTypeLoadBalancer},
		{name: "nodeport", p: PredictorTopologyParameters{ExternalAccess: ExternalAccessNodePort}, want: corev1.ServiceTypeNodePort},
		{name: "unknown", p: PredictorTopologyParameters{ExternalAccess: "Ingress"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.p.ResolvedServiceType(); got != tc.want {
				t.Fatalf("ResolvedServiceType()=%q want %q", got, tc.want)
			}
		})
	}
}

func TestMetricsEnabled(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{
		`{}`:                        true,
		`{"enableMetrics":null}`:    true,
		`{"enableMetrics":true}`:    true,
		`{"enableMetrics":false}`:   false,
		`{"enableMetrics":"true"}`:  true,
		`{"enableMetrics":"false"}`: false,
	} {
		var p PredictorTopologyParameters
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := p.MetricsEnabled(); got != want {
			t.Fatalf("%s: MetricsEnabled() = %t, want %t", raw, got, want)
		}
	}
}

func TestUnmarshalKeepsOtherFields(t *testing.T) {
	t.Parallel()
	var p PredictorTopologyParameters
	if err := json.Unmarshal([]byte(`{"externalAccess":"NodePort","enableMetrics":"false"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.ExternalAccess != ExternalAccessNodePort || p.MetricsEnabled() {
		t.Fatalf("params = %+v", p)
	}
	if err := json.Unmarshal([]byte(`{"enableMetrics":"maybe"}`), &p); err == nil {
		t.Fatal("want an error for a non-boolean string")
	}
}
