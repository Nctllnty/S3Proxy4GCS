package metrics

import (
	"net/http"
	"testing"
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
