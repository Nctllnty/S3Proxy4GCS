package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"s3proxy4gcs/config"
)

var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_http_requests_total",
			Help: "Total number of HTTP requests handled by the proxy.",
		},
		[]string{"method", "route", "status"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_http_request_duration_seconds",
			Help:    "End-to-end HTTP request duration in seconds.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"method", "route"},
	)
	httpInFlightRequests = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "s3proxy_http_in_flight_requests",
			Help: "Current number of in-flight HTTP requests by route.",
		},
		[]string{"route"},
	)
	gcsSDKRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_gcs_sdk_requests_total",
			Help: "Total number of GCS SDK calls made by control-plane handlers.",
		},
		[]string{"operation", "result"},
	)
	gcsSDKRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_gcs_sdk_request_duration_seconds",
			Help:    "Duration of GCS SDK calls made by control-plane handlers.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"operation", "result"},
	)
	upstreamRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_upstream_requests_total",
			Help: "Total number of upstream requests sent to GCS.",
		},
		[]string{"method", "status_class"},
	)
	upstreamRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_upstream_request_duration_seconds",
			Help:    "Duration of upstream requests sent to GCS.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60},
		},
		[]string{"method"},
	)
	requestsRejectedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_requests_rejected_total",
			Help: "Total number of rejected or degraded requests by reason.",
		},
		[]string{"reason"},
	)
	readinessChecksTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_readiness_checks_total",
			Help: "Total number of readiness checks by result.",
		},
		[]string{"result"},
	)
)

func init() {
	prometheus.MustRegister(
		httpRequestsTotal,
		httpRequestDuration,
		httpInFlightRequests,
		gcsSDKRequestsTotal,
		gcsSDKRequestDuration,
		upstreamRequestsTotal,
		upstreamRequestDuration,
		requestsRejectedTotal,
		readinessChecksTotal,
	)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

type metricsTransport struct {
	base http.RoundTripper
}

func (t *metricsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	upstreamRequestDuration.WithLabelValues(req.Method).Observe(time.Since(start).Seconds())
	if err != nil {
		upstreamRequestsTotal.WithLabelValues(req.Method, "error").Inc()
		return nil, err
	}
	upstreamRequestsTotal.WithLabelValues(req.Method, statusClass(resp.StatusCode)).Inc()
	return resp, nil
}

func routeLabelForRequest(r *http.Request) string {
	switch r.URL.Path {
	case "/health":
		return "health"
	case "/readyz":
		return "readyz"
	case "/metrics":
		return "metrics"
	}

	for _, key := range []string{"lifecycle", "cors", "logging", "website", "tagging"} {
		for actualKey := range r.URL.Query() {
			if strings.EqualFold(actualKey, key) {
				return key
			}
		}
	}

	return "s3"
}

func statusClass(statusCode int) string {
	if statusCode < 100 {
		return "unknown"
	}
	return strconv.Itoa(statusCode/100) + "xx"
}

func observabilityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := routeLabelForRequest(r)
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		httpInFlightRequests.WithLabelValues(route).Inc()
		defer httpInFlightRequests.WithLabelValues(route).Dec()

		next.ServeHTTP(rec, r)

		httpRequestsTotal.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		httpRequestDuration.WithLabelValues(r.Method, route).Observe(time.Since(start).Seconds())
	})
}

func timeGCSCall(ctx context.Context, operation string, fn func(context.Context) error) error {
	start := time.Now()
	err := fn(ctx)
	result := "success"
	if err != nil {
		result = "error"
	}

	gcsSDKRequestsTotal.WithLabelValues(operation, result).Inc()
	gcsSDKRequestDuration.WithLabelValues(operation, result).Observe(time.Since(start).Seconds())
	return err
}

func recordRejectedRequest(reason string) {
	requestsRejectedTotal.WithLabelValues(reason).Inc()
}

func metricsHandler() http.Handler {
	return promhttp.Handler()
}

func handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if config.Config.DryRun {
		readinessChecksTotal.WithLabelValues("success").Inc()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ready","mode":"dry_run"}`))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if _, err := gcsClient.Bucket(config.Config.TargetBucket).Attrs(ctx); err != nil {
		readinessChecksTotal.WithLabelValues("error").Inc()
		slog.Error("Readiness check failed", "bucket", config.Config.TargetBucket, "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"status":"not_ready","error":%q}`, err.Error())
		return
	}

	readinessChecksTotal.WithLabelValues("success").Inc()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ready"}`))
}
