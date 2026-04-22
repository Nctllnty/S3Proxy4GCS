// Package metrics provides Prometheus metrics and an HTTP middleware for the
// s3proxy4gcs service.
//
// All metrics exported here are registered against the default Prometheus
// registry on package import, so callers only need to wire up the middleware
// via WithMetrics and expose promhttp.Handler() at /metrics.
package metrics

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Histogram buckets for request/response body sizes (bytes):
// 1KB, 10KB, 100KB, 1MB, 10MB, 100MB, 1GB.
var byteSizeBuckets = []float64{
	1 << 10,          // 1 KB
	10 << 10,         // 10 KB
	100 << 10,        // 100 KB
	1 << 20,          // 1 MB
	10 << 20,         // 10 MB
	100 << 20,        // 100 MB
	1 << 30,          // 1 GB
}

// All metrics are auto-registered against the default prometheus registry.
var (
	BytesReceivedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_bytes_received_total",
			Help: "Total number of request body bytes received by the s3proxy.",
		},
		[]string{"method", "endpoint"},
	)

	BytesSentTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_bytes_sent_total",
			Help: "Total number of response body bytes sent by the s3proxy.",
		},
		[]string{"method", "endpoint"},
	)

	RequestSizeBytes = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_request_size_bytes",
			Help:    "Distribution of request body sizes in bytes.",
			Buckets: byteSizeBuckets,
		},
		[]string{"method", "endpoint"},
	)

	ResponseSizeBytes = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_response_size_bytes",
			Help:    "Distribution of response body sizes in bytes.",
			Buckets: byteSizeBuckets,
		},
		[]string{"method", "endpoint"},
	)

	HTTPRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_http_requests_total",
			Help: "Total number of HTTP requests processed, labeled by method, endpoint and status code.",
		},
		[]string{"method", "endpoint", "status_code"},
	)

	HTTPRequestDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "endpoint"},
	)

	// GCSAPIDurationSeconds preserves the existing GCS SDK timing metric so
	// the move to this package does not drop observability we already had.
	GCSAPIDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_gcs_api_duration_seconds",
			Help:    "GCS SDK call duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"operation"},
	)

	// GCSErrorsTotal counts GCS failures seen by the proxy, separately for
	// data-plane (reverse proxy responses) and control-plane (SDK calls).
	// status_class buckets: "4xx", "5xx", "429" (throttling), "network"
	// (transport/timeout), "other" (e.g. context cancelled).
	GCSErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_gcs_errors_total",
			Help: "Total number of GCS responses/SDK calls classified as errors, by operation and status class.",
		},
		[]string{"operation", "status_class"},
	)

	// InFlightRequests tracks the number of requests currently being
	// served by the proxy, broken down by endpoint. Useful for detecting
	// long-running PUTs that saturate the concurrency throttle.
	InFlightRequests = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3proxy_in_flight_requests",
			Help: "Requests currently in flight in the proxy, by endpoint.",
		},
		[]string{"endpoint"},
	)

	// ResignDurationSeconds measures the SigV4 re-signing cost per request.
	// Quantifies how much CPU is spent in the Director hot path relative to
	// the full request duration.
	ResignDurationSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name: "s3proxy_resign_duration_seconds",
			Help: "Time spent inside SigV4 re-signing for each proxied request.",
			// Very short durations: 10us, 50us, 100us, 500us, 1ms, 5ms, 10ms, 50ms.
			Buckets: []float64{1e-5, 5e-5, 1e-4, 5e-4, 1e-3, 5e-3, 1e-2, 5e-2},
		},
	)

	// FeatureDisabledRejections counts requests refused because the targeted
	// plane operation is turned off via `config.Features` (ENABLE_* env vars).
	// Label cardinality is bounded by the fixed set of feature names defined
	// in config.FeatureFlags — never bucket / object names.
	FeatureDisabledRejections = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_feature_disabled_rejections_total",
			Help: "Total number of requests rejected because the targeted feature is disabled via configuration.",
		},
		[]string{"feature"},
	)

	// HMACCredentialLookups counts per-client AK lookups on the re-sign hot
	// path. Labels are `result`:
	//   - "hit": AK matched an entry in the store (request re-signed with
	//            the client's own secret)
	//   - "miss": AK not found (request was rejected with InvalidAccessKeyId)
	//   - "no_auth": Authorization header missing / malformed (request was
	//            rejected with AccessDenied)
	//   - "disabled": store empty and legacy single-key fallback active
	//            (mapping mode off; proxy is using the legacy path)
	// Used in docs/SLO.md to monitor credential-mapping health.
	HMACCredentialLookups = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_hmac_credential_lookups_total",
			Help: "Total number of HMAC credential lookups performed by the Director, by result.",
		},
		[]string{"result"},
	)

	// HMACCredentialsLoaded reports the number of AK→SK entries currently
	// held by the credential store. Useful for wiring an alert if the map
	// suddenly drops to zero (likely a malformed Secret rollout).
	HMACCredentialsLoaded = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "s3proxy_hmac_credentials_loaded",
			Help: "Current number of AK→SK entries loaded in the HMAC credential store.",
		},
	)

	// HMACCredentialsReloadTotal counts hot-reload attempts triggered by
	// fsnotify. `result` is "success" or "error"; a non-zero error count
	// usually means the K8s Secret was published with malformed JSON.
	HMACCredentialsReloadTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_hmac_credentials_reload_total",
			Help: "Total number of HMAC credential hot-reload attempts, by result.",
		},
		[]string{"result"},
	)
)

// ClassifyGCSError maps an HTTP status code to a low-cardinality status_class
// label used by GCSErrorsTotal. Returns "" when the code is not an error
// (i.e. 2xx/3xx), so the caller can skip incrementing.
func ClassifyGCSError(statusCode int) string {
	switch {
	case statusCode == 429:
		return "429"
	case statusCode >= 500 && statusCode < 600:
		return "5xx"
	case statusCode >= 400 && statusCode < 500:
		return "4xx"
	default:
		return ""
	}
}

// controlPlaneQueryKeys lists S3 subresource query parameters that route to
// the in-proxy XML translators. Detected as separate endpoints so operators
// can tell control-plane CPU cost apart from data-plane streaming.
var controlPlaneQueryKeys = []string{"lifecycle", "cors", "logging", "website", "tagging"}

// classifyEndpoint returns a simplified S3 operation label based on the HTTP
// method and request URL. The mapping keeps cardinality low and avoids leaking
// bucket/object names into label values.
//
// Prior to v1.3 all control-plane paths collapsed into "other"; they are now
// reported individually (`lifecycle` / `cors` / `logging` / `website` /
// `tagging`) so their latency and error rate can be tracked separately.
func classifyEndpoint(method, rawPath string) string {
	// Split path and query once; we need both.
	path := rawPath
	query := ""
	if i := strings.IndexByte(rawPath, '?'); i >= 0 {
		path = rawPath[:i]
		query = rawPath[i+1:]
	}
	trimmed := strings.Trim(path, "/")

	// Reserved operational endpoints never hit this middleware, but guard anyway.
	switch trimmed {
	case "health", "readyz", "metrics":
		return "other"
	}

	// Control-plane subresources take precedence: a PUT /?lifecycle request
	// is NOT put_object — it's an XML-translated bucket update.
	if query != "" {
		for _, key := range controlPlaneQueryKeys {
			if hasQueryKey(query, key) {
				return key
			}
		}
		// Multi-object delete: POST /?delete
		if hasQueryKey(query, "delete") && method == http.MethodPost {
			return "delete_objects"
		}
		// RestoreObject: POST /<bucket>/<key>?restore — handled in-proxy
		// as a synthetic 200/202 since GCS objects are always "live". Kept
		// as its own label so operators can quickly spot callers that still
		// depend on the compatibility shim.
		if hasQueryKey(query, "restore") && method == http.MethodPost {
			return "restore_object"
		}
	}

	// Determine whether the path has a bucket and/or object key.
	// path style: /<bucket>/<key...>
	// virtual-hosted style: / (bucket in Host header) — treat as service-level,
	// which callers typically use for ListBuckets; we classify as "list".
	var hasBucket, hasKey bool
	if trimmed != "" {
		hasBucket = true
		if strings.Contains(trimmed, "/") {
			hasKey = true
		}
	}

	switch method {
	case http.MethodPut:
		if hasBucket && hasKey {
			return "put_object"
		}
		return "other"
	case http.MethodGet:
		if hasKey {
			return "get_object"
		}
		if hasBucket {
			return "list_objects"
		}
		return "list"
	case http.MethodDelete:
		return "delete_object"
	case http.MethodHead:
		return "head_object"
	default:
		return "other"
	}
}

// hasQueryKey does a zero-allocation scan over a raw URL query string and
// reports whether `key` appears as a parameter name (the segment preceding
// `=` or the whole segment if no `=`). Avoids url.ParseQuery's allocations
// on a hot path that runs once per request.
func hasQueryKey(rawQuery, key string) bool {
	for len(rawQuery) > 0 {
		seg := rawQuery
		if i := strings.IndexByte(seg, '&'); i >= 0 {
			seg = rawQuery[:i]
			rawQuery = rawQuery[i+1:]
		} else {
			rawQuery = ""
		}
		name := seg
		if i := strings.IndexByte(seg, '='); i >= 0 {
			name = seg[:i]
		}
		if name == key {
			return true
		}
	}
	return false
}

// countingReadCloser wraps an io.ReadCloser and counts bytes read from it.
// It preserves streaming semantics: it never buffers the body.
type countingReadCloser struct {
	rc    io.ReadCloser
	count int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	if n > 0 {
		c.count += int64(n)
	}
	return n, err
}

func (c *countingReadCloser) Close() error { return c.rc.Close() }

// countingResponseWriter wraps http.ResponseWriter to capture status code and
// count bytes written to the response body. It implements http.Flusher and
// http.Hijacker when the underlying writer does.
type countingResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *countingResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		// Implicit 200 OK — record it so the status_code label is accurate.
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	n, err := w.ResponseWriter.Write(b)
	if n > 0 {
		w.bytes += int64(n)
	}
	return n, err
}

// Flush implements http.Flusher for upstream writers that stream responses.
func (w *countingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter so Go 1.20+ helpers and
// middlewares (e.g. chi's middleware.Recoverer) can access features via
// http.ResponseController.
func (w *countingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// WithMetrics wraps the given handler and records:
//   - bytes_received / request_size from the request body
//   - bytes_sent / response_size from the response body
//   - http_requests_total (method, endpoint, status_code)
//   - http_request_duration_seconds (method, endpoint)
//
// It is safe to use alongside any other middleware. The middleware swaps
// r.Body for a counting reader and wraps the ResponseWriter, so it does NOT
// buffer bodies and preserves streaming.
func WithMetrics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		endpoint := classifyEndpoint(r.Method, r.URL.RequestURI())

		var reqCounter *countingReadCloser
		if r.Body != nil && r.Body != http.NoBody {
			reqCounter = &countingReadCloser{rc: r.Body}
			r.Body = reqCounter
		}

		rec := &countingResponseWriter{ResponseWriter: w, status: http.StatusOK}

		inFlight := InFlightRequests.WithLabelValues(endpoint)
		inFlight.Inc()

		start := time.Now()

		// All observations are deferred so they execute even if the
		// downstream handler panics (chi's Recoverer lives further up the
		// stack and turns the panic into a 500). Without this, a crash
		// would leave `s3proxy_in_flight_requests` leaked and the request
		// count uncounted.
		defer func() {
			duration := time.Since(start)
			inFlight.Dec()

			var reqBytes int64
			if reqCounter != nil {
				reqBytes = reqCounter.count
			}
			respBytes := rec.bytes

			BytesReceivedTotal.WithLabelValues(r.Method, endpoint).Add(float64(reqBytes))
			BytesSentTotal.WithLabelValues(r.Method, endpoint).Add(float64(respBytes))
			RequestSizeBytes.WithLabelValues(r.Method, endpoint).Observe(float64(reqBytes))
			ResponseSizeBytes.WithLabelValues(r.Method, endpoint).Observe(float64(respBytes))

			// If the panic reached Recoverer before any WriteHeader, rec.status
			// still holds its default (200). Coerce to 500 in that case so the
			// status_code label reflects what the client actually receives.
			status := rec.status
			if pv := recover(); pv != nil {
				if !rec.wroteHeader {
					status = http.StatusInternalServerError
				}
				defer panic(pv) // propagate to chi's Recoverer
			}

			statusStr := strconv.Itoa(status)
			HTTPRequestsTotal.WithLabelValues(r.Method, endpoint, statusStr).Inc()
			HTTPRequestDurationSeconds.WithLabelValues(r.Method, endpoint).Observe(duration.Seconds())

			// Error-class counter: records every non-2xx/3xx response the proxy
			// returned to clients. The label is the classified endpoint (so you
			// can tell `get_object 5xx` from `lifecycle 5xx`) rather than the
			// raw GCS SDK operation name (that variant is handled in
			// timeGCSCall).
			if class := ClassifyGCSError(status); class != "" {
				GCSErrorsTotal.WithLabelValues(endpoint, class).Inc()
			}
		}()

		next.ServeHTTP(rec, r)
	})
}

// StatusCode returns the final status recorded by a counting response writer
// wrapped by WithMetrics. Returns 0 if w is not a wrapped writer.
//
// Intended for downstream middleware (e.g. structured loggers) that want to
// read the status without duplicating the ResponseWriter wrapping.
func StatusCode(w http.ResponseWriter) int {
	if crw, ok := w.(*countingResponseWriter); ok {
		return crw.status
	}
	return 0
}
