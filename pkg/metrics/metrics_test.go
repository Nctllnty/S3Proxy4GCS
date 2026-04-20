package metrics

import (
	"net/http"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// TestClassifyEndpoint guards the low-cardinality endpoint labels emitted
// by the observability middleware. Keep the matrix small but cover every
// branch that the hot path exercises so accidental regressions in routing
// show up here before Grafana dashboards break.
func TestClassifyEndpoint(t *testing.T) {
	cases := []struct {
		name   string
		method string
		rawURI string
		want   string
	}{
		{"health", http.MethodGet, "/health", "other"},
		{"lifecycle_put", http.MethodPut, "/bucket/?lifecycle", "lifecycle"},
		{"cors_get", http.MethodGet, "/bucket/?cors", "cors"},
		{"delete_objects", http.MethodPost, "/bucket/?delete", "delete_objects"},
		{"restore_post", http.MethodPost, "/bucket/key?restore", "restore_object"},
		{"restore_with_value", http.MethodPost, "/bucket/key?restore=&other=1", "restore_object"},
		// Non-POST ?restore is rejected upstream with 501 but we still want
		// a deterministic label for the transient request in metrics.
		{"restore_get_is_not_restore_object", http.MethodGet, "/bucket/key?restore", "get_object"},
		{"put_object", http.MethodPut, "/bucket/key", "put_object"},
		{"get_object", http.MethodGet, "/bucket/key", "get_object"},
		{"list_objects", http.MethodGet, "/bucket/", "list_objects"},
		{"list_service", http.MethodGet, "/", "list"},
		{"head_object", http.MethodHead, "/bucket/key", "head_object"},
		{"delete_object", http.MethodDelete, "/bucket/key", "delete_object"},
		{"unknown_method", http.MethodOptions, "/bucket/key", "other"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := classifyEndpoint(tc.method, tc.rawURI)
			if got != tc.want {
				t.Fatalf("classifyEndpoint(%q, %q) = %q, want %q",
					tc.method, tc.rawURI, got, tc.want)
			}
		})
	}
}

// TestFeatureDisabledRejectionsCounter checks that the counter added in
// v1.6 is registered, accepts the expected label, and increments as a
// normal Prometheus counter. This is a tiny smoke test; the feature-gate
// branches in main.go own the behavioural coverage.
func TestFeatureDisabledRejectionsCounter(t *testing.T) {
	// Each distinct label is a separate series. Using a test-only label
	// keeps this assertion independent of other tests running in the same
	// process that may touch the production labels.
	const feature = "unit_test_sentinel"

	before := readCounter(t, feature)
	FeatureDisabledRejections.WithLabelValues(feature).Inc()
	FeatureDisabledRejections.WithLabelValues(feature).Inc()
	after := readCounter(t, feature)

	if got := after - before; got != 2 {
		t.Errorf("counter delta = %v, want 2", got)
	}
}

func readCounter(t *testing.T, feature string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := FeatureDisabledRejections.WithLabelValues(feature).Write(m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	if m.Counter == nil || m.Counter.Value == nil {
		return 0
	}
	return *m.Counter.Value
}
