package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/api/option"
	"s3proxy4gcs/config"
	"s3proxy4gcs/pkg/translate"
)

var (
	gcsClient    *storage.Client
	gcsCtx       context.Context
	reverseProxy *httputil.ReverseProxy
	gcsURL       *url.URL

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

func main() {
	// Initialize configuration
	config.LoadConfig()

	// Initialize Structured JSON Logger (slog)
	var level slog.Level = slog.LevelInfo
	if config.Config.DebugLogging {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	gcsCtx = context.Background()

	var err error
	if !config.Config.DryRun {
		var opts []option.ClientOption
		if config.Config.JSONKey != "" {
			opts = append(opts, option.WithCredentialsFile(config.Config.JSONKey))
			slog.Info("Using JSON key for GCS client", "path", config.Config.JSONKey)
		}
		gcsClient, err = storage.NewClient(gcsCtx, opts...)
		if err != nil {
			log.Fatalf("Failed to initialize GCS client: %v", err)
		}
		defer gcsClient.Close()
		log.Println("Initialized real GCS client.")
	} else {
		log.Println("Running in DRY_RUN mode (No real GCS hits).")
	}

	// Initialize Reverse Proxy for passthrough using centralized configuration
	gcsURL, err = url.Parse(config.Config.StorageBaseURL)
	if err != nil {
		log.Fatalf("Failed to parse GCS URL: %v", err)
	}

	reverseProxy = httputil.NewSingleHostReverseProxy(gcsURL)
	var baseTransport http.RoundTripper
	if config.Config.DryRun {
		baseTransport = &dryRunTransport{}
		slog.Info("Reverse Proxy using DryRun Transport (no real hits)")
	} else {
		baseTransport = &http.Transport{
			MaxIdleConns:          config.Config.MaxIdleConns,
			MaxIdleConnsPerHost:   config.Config.MaxIdleConnsPerHost,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableCompression:    true,
			ForceAttemptHTTP2:     true,
		}
		slog.Info("Reverse Proxy using tuned Transport with timeouts",
			"MaxIdleConns", config.Config.MaxIdleConns,
			"MaxIdleConnsPerHost", config.Config.MaxIdleConnsPerHost)
	}
	reverseProxy.Transport = &metricsTransport{base: baseTransport}

	reverseProxy.Director = func(req *http.Request) {
		req.URL.Host = gcsURL.Host
		req.URL.Scheme = gcsURL.Scheme
		req.Host = gcsURL.Host // Critical for TLS Handshake

		if config.Config.DebugLogging {
			headers := req.Header.Clone()
			headers.Del("Authorization")
			slog.Debug("Request Headers transmitted to GCS (Redacted)", "headers", headers)
		}

		if clStr := req.Header.Get("Content-Length"); clStr != "" {
			if cl, err := strconv.ParseInt(clStr, 10, 64); err == nil {
				req.ContentLength = cl
			}
		}

		// 1. Storage Class Translation & x-id Stripping (Hybrid Data-Plane)
		shouldResign := false

		sc := req.Header.Get("x-amz-storage-class")
		if sc != "" && sc != "STANDARD" {
			slog.Info("Detected non-standard S3 Storage Class", "storageClass", sc)
			switch sc {
			case "STANDARD_IA":
				req.Header.Set("x-amz-storage-class", "NEARLINE")
				shouldResign = true
			case "GLACIER_IR":
				req.Header.Set("x-amz-storage-class", "COLDLINE")
				shouldResign = true
			case "GLACIER", "DEEP_ARCHIVE":
				req.Header.Set("x-amz-storage-class", "ARCHIVE")
				shouldResign = true
			case "INTELLIGENT_TIERING":
				req.Header.Set("x-amz-storage-class", "AUTOCLASS")
				shouldResign = true
			default:
				req.Header.Set("x-amz-storage-class", "NEARLINE") // "The Others"
				shouldResign = true
			}
		}

		// Detect x-id query parameter (Go SDK v2 specific tracking)
		q := req.URL.Query()
		if q.Get("x-id") != "" {
			slog.Info("Detected x-id query parameter. Stripping and re-signing", "xId", q.Get("x-id"))
			q.Del("x-id")
			req.URL.RawQuery = q.Encode()
			shouldResign = true
		}

		// Detect Accept-Encoding: identity (causes issues with GCS S3 API)
		if req.Header.Get("Accept-Encoding") == "identity" {
			slog.Info("Detected Accept-Encoding: identity. Stripping and re-signing")
			req.Header.Del("Accept-Encoding")
			shouldResign = true
		}

		// 2. Versioning Interop (Egress - Must be set before re-signing if we re-sign!)
		if strings.Contains(req.URL.RawQuery, "versions") {
			req.Header.Set("x-amz-interop-list-objects-format", "enabled")
			slog.Info("Injected version interop header for ListObjectVersions")
		}

		if shouldResign {
			if config.Config.ProxyAccessKey == "" || config.Config.ProxySecretKey == "" {
				recordRejectedRequest("resign_skipped_missing_credentials")
				slog.Warn("Proxy HMAC credentials not set! Re-signing skipped. Signature will fail at GCS.")
			} else {
				payloadHash := req.Header.Get("X-Amz-Content-Sha256")
				if payloadHash == "" {
					payloadHash = "UNSIGNED-PAYLOAD"
				}

				awsCreds := aws.Credentials{
					AccessKeyID:     config.Config.ProxyAccessKey,
					SecretAccessKey: config.Config.ProxySecretKey,
				}

				signer := v4.NewSigner()

				// Strip User-Agent before re-signing to match aws4gcs known-good pattern
				req.Header.Del("User-Agent")

				if err := signer.SignHTTP(req.Context(), awsCreds, req, payloadHash, "s3", "us-east-1", time.Now()); err != nil {
					recordRejectedRequest("resign_failed")
					slog.Error("Failed to re-sign request", "error", err)
				} else {
					slog.Info("Successfully re-signed request for GCS")
				}
			}
		}
	}

	reverseProxy.ModifyResponse = func(resp *http.Response) error {
		if config.Config.DebugLogging {
			slog.Debug("Response Headers received from GCS", "headers", resp.Header)
		}

		// Read and log XML response if requested for version interop
		if strings.Contains(resp.Request.URL.RawQuery, "versions") {
			if bodyBytes, err := io.ReadAll(resp.Body); err == nil {
				slog.Debug("XML Response Body for ListObjectVersions", "xml", string(bodyBytes))
				resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
		}

		// 3. Versioning Interop (Ingress)
		if gen := resp.Header.Get("x-goog-generation"); gen != "" {
			resp.Header.Set("x-amz-version-id", gen)
			slog.Info("Mapped x-goog-generation to x-amz-version-id", "generation", gen)
		}
		return nil
	}

	r := chi.NewRouter()

	// Base middlewares
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(observabilityMiddleware)

	// API Handlers
	r.Handle("/metrics", promhttp.Handler())
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})
	r.Get("/readyz", handleReadyz)

	// Pass-through or intercept handlers
	r.Route("/", func(r chi.Router) {
		// Catch-all for S3 requests
		r.Get("/*", handleS3Request)
		r.Put("/*", handleS3Request)
		r.Post("/*", handleS3Request)
		r.Delete("/*", handleS3Request)
		r.Head("/*", handleS3Request)
	})

	srv := &http.Server{
		Addr:    ":" + config.Config.Port,
		Handler: r,
	}

	serverErrors := make(chan error, 1)

	go func() {
		slog.Info("Starting S3 to GCS proxy", "port", config.Config.Port)
		serverErrors <- srv.ListenAndServe()
	}()

	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErrors:
		slog.Error("Server error on startup", "error", err)
		return
	case sig := <-shutdownSignal:
		slog.Info("Shutdown signal received", "signal", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("Shutdown failed, forcing close", "error", err)
			srv.Close()
		} else {
			slog.Info("Server gracefully stopped")
		}
	}
}

func handleS3Request(w http.ResponseWriter, r *http.Request) {
	slog.Info("Received S3 Request", "method", r.Method, "uri", r.RequestURI)

	hasQueryParam := func(key string) bool {
		for k := range r.URL.Query() {
			if strings.EqualFold(k, key) {
				return true
			}
		}
		return false
	}

	// Check if this is a lifecycle request
	if hasQueryParam("lifecycle") && r.Method == http.MethodPut {
		handlePutLifecycle(w, r)
		return
	}

	// Check if this is a CORS request
	if hasQueryParam("cors") {
		if r.Method == http.MethodPut {
			handlePutCORS(w, r)
			return
		} else if r.Method == http.MethodGet {
			handleGetCORS(w, r)
			return
		} else if r.Method == http.MethodDelete {
			handleDeleteCORS(w, r)
			return
		}
	}

	// Check if this is a Logging request
	if hasQueryParam("logging") {
		if r.Method == http.MethodPut {
			handlePutLogging(w, r)
			return
		} else if r.Method == http.MethodGet {
			handleGetLogging(w, r)
			return
		} else if r.Method == http.MethodDelete {
			handleDeleteLogging(w, r)
			return
		}
	}

	// Check if this is a Website request
	if hasQueryParam("website") && r.Method == http.MethodPut {
		handlePutWebsite(w, r)
		return
	}

	// Check if this is a Tagging request
	if hasQueryParam("tagging") {
		if r.Method == http.MethodPut {
			handlePutObjectTagging(w, r)
			return
		} else if r.Method == http.MethodGet {
			handleGetObjectTagging(w, r)
			return
		} else if r.Method == http.MethodDelete {
			handleDeleteObjectTagging(w, r)
			return
		}
	}

	// Default: Fallthrough to Reverse Proxy
	reverseProxy.ServeHTTP(w, r)
}

type dryRunTransport struct{}

func (t *dryRunTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	slog.Info("[DRY_RUN] ReverseProxy intercepted", "method", req.Method, "url", req.URL.String())
	slog.Debug("[DRY_RUN] Header StorageClass", "class", req.Header.Get("x-amz-storage-class"))
	slog.Debug("[DRY_RUN] Header VersionFormat", "format", req.Header.Get("x-amz-interop-list-objects-format"))

	// Return a synthetic response
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("Successfully proxied to GCS (DryRun - no real hits).")),
	}

	// For Versioning Interop verification (Simulate GCS response header)
	if strings.Contains(req.URL.RawQuery, "versions") || req.Method == http.MethodHead {
		resp.Header.Set("x-goog-generation", "1234567890")
	}

	return resp, nil
}

func handlePutLifecycle(w http.ResponseWriter, r *http.Request) {
	// 1. Read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// 2. Parse S3 XML
	var s3Cfg translate.LifecycleConfiguration
	if err := xml.Unmarshal(body, &s3Cfg); err != nil {
		slog.Error("Failed to unmarshal S3 XML for Lifecycle", "error", err)
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		return
	}

	// 3. Translate to GCS JSON
	gcsJSON, err := translate.TranslateS3ToGCS(&s3Cfg)
	if err != nil {
		slog.Error("Failed to translate to GCS JSON for Lifecycle", "error", err)
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "Lifecycle translation failed.")
		return
	}

	// 4. If DryRun is true, we just return the translated JSON (for local laptop testing)
	if config.Config.DryRun {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(gcsJSON)
		return
	}

	// 5. Unmarshal translated GCS JSON back into storage.Lifecycle interface if we want to use BucketHandle.Update
	var storageLifecycle storage.Lifecycle
	if err := json.Unmarshal(gcsJSON, &storageLifecycle); err != nil {
		slog.Error("Failed to unmarshal translated JSON into storage.Lifecycle", "error", err)
		http.Error(w, "Internal translation error mapping to Go SDK", http.StatusInternalServerError)
		return
	}

	// 6. Execute Bucket Update via GCS SDK
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		Lifecycle: &storageLifecycle,
	}

	err = timeGCSCall(r.Context(), "PutBucketLifecycle", func(ctx context.Context) error {
		_, err := bucket.Update(ctx, uattrs)
		return err
	})
	if err != nil {
		slog.Error("Failed to update GCS bucket lifecycle", "bucket", config.Config.TargetBucket, "error", err)
		http.Error(w, fmt.Sprintf("GCS API error: %v", err), http.StatusBadGateway)
		return
	}

	slog.Info("Successfully updated GCS bucket lifecycle", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Successfully proxied and applied lifecycle to GCS."))
}

func handlePutCORS(w http.ResponseWriter, r *http.Request) {
	// 1. Read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// 2. Parse S3 XML
	var s3Cfg translate.CORSConfiguration
	if err := xml.Unmarshal(body, &s3Cfg); err != nil {
		slog.Error("Failed to unmarshal S3 XML for CORS", "error", err)
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		return
	}

	// 3. Translate to GCS CORS
	gcsCORS := translate.TranslateS3ToGCSCors(&s3Cfg)

	// 4. In DryRun mode, just print/return success
	if config.Config.DryRun {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Successfully proxied CORS to GCS (DryRun - no real hits)."))
		return
	}

	// 5. Execute Bucket Update via GCS SDK
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		CORS: gcsCORS,
	}

	err = timeGCSCall(r.Context(), "PutBucketCORS", func(ctx context.Context) error {
		_, err := bucket.Update(ctx, uattrs)
		return err
	})
	if err != nil {
		slog.Error("Failed to update GCS bucket CORS", "bucket", config.Config.TargetBucket, "error", err)
		http.Error(w, fmt.Sprintf("GCS API error: %v", err), http.StatusBadGateway)
		return
	}

	slog.Info("Successfully updated GCS bucket CORS", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Successfully proxied and applied CORS to GCS."))
}

func handleGetCORS(w http.ResponseWriter, r *http.Request) {
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	var attrs *storage.BucketAttrs
	err := timeGCSCall(r.Context(), "GetBucketCORS", func(ctx context.Context) error {
		var err error
		attrs, err = bucket.Attrs(ctx)
		return err
	})
	if err != nil {
		slog.Error("Failed to fetch GCS bucket attributes for CORS", "bucket", config.Config.TargetBucket, "error", err)
		http.Error(w, fmt.Sprintf("GCS API error: %v", err), http.StatusBadGateway)
		return
	}

	s3Cfg := translate.TranslateGCSToS3Cors(attrs.CORS)
	if s3Cfg == nil {
		s3Cfg = &translate.CORSConfiguration{} // Return empty but valid XML
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml.NewEncoder(w).Encode(s3Cfg)
}

func handleDeleteCORS(w http.ResponseWriter, r *http.Request) {
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		CORS: []storage.CORS{},
	}

	err := timeGCSCall(r.Context(), "DeleteBucketCORS", func(ctx context.Context) error {
		_, err := bucket.Update(ctx, uattrs)
		return err
	})
	if err != nil {
		slog.Error("Failed to reset GCS bucket CORS", "bucket", config.Config.TargetBucket, "error", err)
		http.Error(w, fmt.Sprintf("GCS API error deleting CORS: %v", err), http.StatusBadGateway)
		return
	}

	slog.Info("Successfully deleted GCS bucket CORS", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusNoContent)
}

func handlePutLogging(w http.ResponseWriter, r *http.Request) {
	// 1. Read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// 2. Parse S3 XML
	var s3Cfg translate.BucketLoggingStatus
	if err := xml.Unmarshal(body, &s3Cfg); err != nil {
		slog.Error("Failed to unmarshal S3 XML for Logging", "error", err)
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		return
	}

	// 3. Translate to GCS Logging
	gcsLogging := translate.TranslateS3ToGCSLogging(s3Cfg)

	// 4. In DryRun mode, just print/return success
	if config.Config.DryRun {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Successfully proxied Logging to GCS (DryRun - no real hits)."))
		return
	}

	// 5. Execute Bucket Update via GCS SDK
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		Logging: gcsLogging,
	}

	err = timeGCSCall(r.Context(), "PutBucketLogging", func(ctx context.Context) error {
		_, err := bucket.Update(ctx, uattrs)
		return err
	})
	if err != nil {
		slog.Error("Failed to update GCS bucket Logging", "bucket", config.Config.TargetBucket, "error", err)
		http.Error(w, fmt.Sprintf("GCS API error: %v", err), http.StatusBadGateway)
		return
	}

	slog.Info("Successfully updated GCS bucket Logging", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Successfully proxied and applied Logging to GCS."))
}

func handleGetLogging(w http.ResponseWriter, r *http.Request) {
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	var attrs *storage.BucketAttrs
	err := timeGCSCall(r.Context(), "GetBucketLogging", func(ctx context.Context) error {
		var err error
		attrs, err = bucket.Attrs(ctx)
		return err
	})
	if err != nil {
		slog.Error("Failed to fetch GCS bucket attributes for Logging", "bucket", config.Config.TargetBucket, "error", err)
		http.Error(w, fmt.Sprintf("GCS API error: %v", err), http.StatusBadGateway)
		return
	}

	s3Cfg := translate.TranslateGCSToS3Logging(attrs.Logging)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml.NewEncoder(w).Encode(s3Cfg)
}

func handleDeleteLogging(w http.ResponseWriter, r *http.Request) {
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		Logging: &storage.BucketLogging{},
	}

	err := timeGCSCall(r.Context(), "DeleteBucketLogging", func(ctx context.Context) error {
		_, err := bucket.Update(ctx, uattrs)
		return err
	})
	if err != nil {
		slog.Error("Failed to reset GCS bucket Logging", "bucket", config.Config.TargetBucket, "error", err)
		http.Error(w, fmt.Sprintf("GCS API error deleting Logging: %v", err), http.StatusBadGateway)
		return
	}

	slog.Info("Successfully deleted GCS bucket Logging", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusNoContent)
}

func handlePutWebsite(w http.ResponseWriter, r *http.Request) {
	// 1. Read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// 2. Parse S3 XML
	var s3Cfg translate.WebsiteConfiguration
	if err := xml.Unmarshal(body, &s3Cfg); err != nil {
		slog.Error("Failed to unmarshal S3 XML for Website", "error", err)
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		return
	}

	// 3. Translate to GCS Website
	gcsWebsite := translate.TranslateS3ToGCSWebsite(s3Cfg)

	// 4. In DryRun mode, just print/return success
	if config.Config.DryRun {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Successfully proxied Website to GCS (DryRun - no real hits)."))
		return
	}

	// 5. Execute Bucket Update via GCS SDK
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		Website: gcsWebsite,
	}

	err = timeGCSCall(r.Context(), "PutBucketWebsite", func(ctx context.Context) error {
		_, err := bucket.Update(ctx, uattrs)
		return err
	})
	if err != nil {
		slog.Error("Failed to update GCS bucket Website", "bucket", config.Config.TargetBucket, "error", err)
		http.Error(w, fmt.Sprintf("GCS API error: %v", err), http.StatusBadGateway)
		return
	}

	slog.Info("Successfully updated GCS bucket Website", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Successfully proxied and applied Website to GCS."))
}

func handlePutObjectTagging(w http.ResponseWriter, r *http.Request) {
	// 1. Read body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	// 2. Parse S3 XML
	var s3Cfg translate.Tagging
	if err := xml.Unmarshal(body, &s3Cfg); err != nil {
		slog.Error("Failed to unmarshal S3 XML for Tagging", "error", err)
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		return
	}

	// Determine target bucket and object from URL path (e.g., /test-bucket/object-path)
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) < 2 || pathParts[0] == "" || pathParts[1] == "" {
		recordRejectedRequest("invalid_object_path")
		http.Error(w, "Bucket and Object name required", http.StatusBadRequest)
		return
	}
	targetBucket := pathParts[0]
	targetObject := strings.Join(pathParts[1:], "/")

	slog.Info("Applying Tagging to GCS Object", "bucket", targetBucket, "object", targetObject)

	if config.Config.DryRun {
		slog.Info("[DRY_RUN] Would apply Tagging to GCS Object", "bucket", targetBucket, "object", targetObject)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Successfully proxied Tagging to GCS (DryRun - no real hits)."))
		return
	}

	// 3. Fetch existing object metadata for read-modify-write
	obj := gcsClient.Bucket(targetBucket).Object(targetObject)
	var attrs *storage.ObjectAttrs
	err = timeGCSCall(r.Context(), "GetObjectTaggingAttrs", func(ctx context.Context) error {
		var err error
		attrs, err = obj.Attrs(ctx)
		return err
	})
	if err != nil {
		slog.Error("Failed to fetch object attributes for read-modify-write", "error", err)
		http.Error(w, fmt.Sprintf("GCS API error fetching attributes: %v", err), http.StatusNotFound) // Use NotFound for safety or Internal
		return
	}

	// 4. Translate via gcs_tagging.go
	updateMetadata := translate.TranslateS3ToGCSTagging(s3Cfg, attrs.Metadata)

	uattrs := storage.ObjectAttrsToUpdate{
		Metadata: updateMetadata,
	}

	// 5. Update Object using OCC via IfMetagenerationMatch
	err = timeGCSCall(r.Context(), "PutObjectTagging", func(ctx context.Context) error {
		_, err := obj.If(storage.Conditions{
			MetagenerationMatch: attrs.Metageneration,
		}).Update(ctx, uattrs)
		return err
	})

	if err != nil {
		slog.Error("GCS API error applying Tagging", "error", err)
		http.Error(w, fmt.Sprintf("GCS API error: %v (OCC conflict if 412)", err), http.StatusInternalServerError)
		return
	}

	slog.Info("Successfully updated GCS Object Tagging", "bucket", targetBucket, "object", targetObject)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Successfully proxied and applied Tagging to GCS."))
}

func handleGetObjectTagging(w http.ResponseWriter, r *http.Request) {
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) < 2 || pathParts[0] == "" || pathParts[1] == "" {
		recordRejectedRequest("invalid_object_path")
		http.Error(w, "Bucket and Object name required", http.StatusBadRequest)
		return
	}
	targetBucket := pathParts[0]
	targetObject := strings.Join(pathParts[1:], "/")

	obj := gcsClient.Bucket(targetBucket).Object(targetObject)
	var attrs *storage.ObjectAttrs
	err := timeGCSCall(r.Context(), "GetObjectTagging", func(ctx context.Context) error {
		var err error
		attrs, err = obj.Attrs(ctx)
		return err
	})
	if err != nil {
		slog.Error("Failed to fetch GCS Object attributes for GetTagging", "bucket", targetBucket, "object", targetObject, "error", err)
		http.Error(w, fmt.Sprintf("GCS API error fetching attributes: %v", err), http.StatusNotFound)
		return
	}

	s3Cfg := translate.TranslateGCSToS3Tagging(attrs.Metadata)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml.NewEncoder(w).Encode(s3Cfg)
}

func handleDeleteObjectTagging(w http.ResponseWriter, r *http.Request) {
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) < 2 || pathParts[0] == "" || pathParts[1] == "" {
		recordRejectedRequest("invalid_object_path")
		http.Error(w, "Bucket and Object name required", http.StatusBadRequest)
		return
	}
	targetBucket := pathParts[0]
	targetObject := strings.Join(pathParts[1:], "/")

	obj := gcsClient.Bucket(targetBucket).Object(targetObject)
	var attrs *storage.ObjectAttrs
	err := timeGCSCall(r.Context(), "GetDeleteObjectTaggingAttrs", func(ctx context.Context) error {
		var err error
		attrs, err = obj.Attrs(ctx)
		return err
	})
	if err != nil {
		slog.Error("Failed to fetch GCS Object attributes for DeleteTagging", "bucket", targetBucket, "object", targetObject, "error", err)
		http.Error(w, fmt.Sprintf("GCS API error: %v", err), http.StatusNotFound)
		return
	}

	updateMetadata := make(map[string]string)
	for k := range attrs.Metadata {
		if strings.HasPrefix(strings.ToLower(k), strings.ToLower(translate.S3TagPrefix)) {
			updateMetadata[k] = "" // Set to empty to delete
		}
	}

	uattrs := storage.ObjectAttrsToUpdate{
		Metadata: updateMetadata,
	}

	err = timeGCSCall(r.Context(), "DeleteObjectTagging", func(ctx context.Context) error {
		_, err := obj.If(storage.Conditions{
			MetagenerationMatch: attrs.Metageneration,
		}).Update(ctx, uattrs)
		return err
	})

	if err != nil {
		slog.Error("GCS API error deleting Object Tagging", "bucket", targetBucket, "object", targetObject, "error", err)
		http.Error(w, fmt.Sprintf("GCS API error: %v", err), http.StatusInternalServerError)
		return
	}

	slog.Info("Successfully deleted GCS Object Tagging", "bucket", targetBucket, "object", targetObject)
	w.WriteHeader(http.StatusNoContent)
}

func writeS3Error(w http.ResponseWriter, statusCode int, code string, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(statusCode)
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>%s</Code><Message>%s</Message></Error>\n", code, message)
}
