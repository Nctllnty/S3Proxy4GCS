package main

import (
	"context"
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
	"sync"
	"syscall"
	"time"

	"s3proxy4gcs/config"
	"s3proxy4gcs/pkg/translate"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/api/option"
)

// maxControlPlaneBodySize is the maximum allowed request body size for
// control-plane PUT operations (Lifecycle, CORS, Logging, Website, Tagging).
// This matches the AWS S3 documented limit of 64 KB for bucket configuration
// XML payloads, preventing memory-exhaustion attacks via oversized bodies.
const maxControlPlaneBodySize = 64 * 1024 // 64 KB

var gcsClient *storage.Client
var gcsCtx context.Context
var reverseProxy *httputil.ReverseProxy
var gcsURL *url.URL

// Global signer instance (reused across all requests to reduce CPU overhead)
var signer = v4.NewSigner()

// BufferPool for ReverseProxy to reduce memory allocation and GC pressure
var proxyBufPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 32*1024) // 32KB buffer per goroutine
	},
}

type bufferPool struct{ *sync.Pool }

func (p *bufferPool) Get() []byte { return p.Pool.Get().([]byte) }
func (p *bufferPool) Put(b []byte) { p.Pool.Put(b) }

// Prometheus metrics
var (
	httpRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3proxy_http_requests_total",
			Help: "Total number of HTTP requests processed.",
		},
		[]string{"method", "handler", "status"},
	)
	httpRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "handler"},
	)
	gcsAPIDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "s3proxy_gcs_api_duration_seconds",
			Help:    "GCS API call duration in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"operation"},
	)
)

func init() {
	prometheus.MustRegister(httpRequestsTotal, httpRequestDuration, gcsAPIDuration)
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

	// Set BufferPool to reduce memory allocation and GC pressure
	reverseProxy.BufferPool = &bufferPool{&proxyBufPool}

	if config.Config.DryRun {
		reverseProxy.Transport = &dryRunTransport{}
		slog.Info("Reverse Proxy using DryRun Transport (no real hits)")
	} else {
		reverseProxy.Transport = &http.Transport{
			MaxIdleConns:          config.Config.MaxIdleConns,
			MaxIdleConnsPerHost:   config.Config.MaxIdleConnsPerHost,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableCompression:    true, // Preserve Accept-Encoding for S3 signatures
			ForceAttemptHTTP2:     true, // Enable HTTP/2 for multiplexing
		}
		slog.Info("Reverse Proxy using tuned Transport with timeouts",
			"MaxIdleConns", config.Config.MaxIdleConns,
			"MaxIdleConnsPerHost", config.Config.MaxIdleConnsPerHost)
	}

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
		// Always re-sign: the Director changes Host from proxy to GCS,
		// so the original SigV4 signature (signed for localhost) is invalid.
		shouldResign := true

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

		if shouldResign {
			if config.Config.ProxyAccessKey == "" || config.Config.ProxySecretKey == "" {
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

				// Strip User-Agent before re-signing to match aws4gcs known-good pattern
				req.Header.Del("User-Agent")

				if err := signer.SignHTTP(req.Context(), awsCreds, req, payloadHash, "s3", "us-east-1", time.Now()); err != nil {
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

		return nil
	}

	r := chi.NewRouter()

	// Base middlewares
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(observabilityMiddleware)

	// Concurrency limiter — prevents goroutine/connection exhaustion under burst load.
	// Requests exceeding the limit receive 503 Service Unavailable.
	// Configurable via MAX_CONCURRENT_REQUESTS (default 1000, 0 = disabled).
	if config.Config.MaxConcurrentRequests > 0 {
		r.Use(middleware.Throttle(config.Config.MaxConcurrentRequests))
		slog.Info("Concurrency throttle enabled", "max_concurrent_requests", config.Config.MaxConcurrentRequests)
	} else {
		slog.Warn("Concurrency throttle DISABLED (MAX_CONCURRENT_REQUESTS=0)")
	}

	// Operational endpoints (excluded from S3 routing)
	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if config.Config.DryRun {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ready","mode":"dry_run"}`))
			return
		}
		if gcsClient == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"not_ready","reason":"gcs_client_nil"}`))
			return
		}
		// Lightweight check: fetch bucket attrs to verify connectivity
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_, err := gcsClient.Bucket(config.Config.TargetBucket).Attrs(ctx)
		if err != nil {
			slog.Error("Readiness check failed", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"not_ready","reason":"gcs_connectivity_failed"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ready","mode":"live"}`))
	})

	r.Handle("/metrics", promhttp.Handler())

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

		// Timeout settings aligned with AWS SDK for Go v2 defaults:
		// - ReadHeaderTimeout: matches SDK's TLSHandshakeTimeout (10s), prevents Slowloris attacks
		// - IdleTimeout: matches SDK's IdleConnTimeout (90s), releases idle keep-alive connections
		// - ReadTimeout/WriteTimeout: intentionally unset (0) to support data-plane streaming
		//   of large objects (S3 max 50TB), consistent with SDK not setting Client.Timeout
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
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

// statusRecorder wraps http.ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// observabilityMiddleware replaces chi's default Logger middleware with structured
// JSON logging that includes request_id, method, path, status, duration, and body size.
// It also records Prometheus metrics for every request.
func observabilityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		duration := time.Since(start)
		reqID := middleware.GetReqID(r.Context())

		// Determine handler label for metrics
		handlerLabel := "proxy"
		q := r.URL.Query()
		for _, key := range []string{"lifecycle", "cors", "logging", "website", "tagging"} {
			if _, ok := q[key]; ok {
				handlerLabel = key
				break
			}
		}

		statusStr := strconv.Itoa(rec.status)
		httpRequestsTotal.WithLabelValues(r.Method, handlerLabel, statusStr).Inc()
		httpRequestDuration.WithLabelValues(r.Method, handlerLabel).Observe(duration.Seconds())

		slog.Info("HTTP request completed",
			"request_id", reqID,
			"method", r.Method,
			"uri", r.RequestURI,
			"status", rec.status,
			"duration_ms", duration.Milliseconds(),
			"content_length", r.ContentLength,
			"handler", handlerLabel,
		)
	})
}

// reqLogger returns a slog.Logger enriched with the request_id from context.
func reqLogger(ctx context.Context) *slog.Logger {
	reqID := middleware.GetReqID(ctx)
	if reqID == "" {
		return slog.Default()
	}
	return slog.Default().With("request_id", reqID)
}

// timeGCSCall executes a GCS SDK call with an optional per-call timeout,
// logs and records its duration. The fn receives a context that may have
// a deadline applied (controlled by GCS_CALL_TIMEOUT_SEC, default 30s).
func timeGCSCall(ctx context.Context, operation string, fn func(ctx context.Context) error) error {
	callCtx := ctx
	if config.Config.GCSCallTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, config.Config.GCSCallTimeout)
		defer cancel()
	}

	start := time.Now()
	err := fn(callCtx)
	duration := time.Since(start)
	gcsAPIDuration.WithLabelValues(operation).Observe(duration.Seconds())
	log := reqLogger(ctx)
	if err != nil {
		log.Error("GCS API call failed", "operation", operation, "duration_ms", duration.Milliseconds(), "error", err)
	} else {
		log.Info("GCS API call succeeded", "operation", operation, "duration_ms", duration.Milliseconds())
	}
	return err
}

func handleS3Request(w http.ResponseWriter, r *http.Request) {
	log := reqLogger(r.Context())
	log.Info("Received S3 Request", "method", r.Method, "uri", r.RequestURI)

	// Reject aws-chunked requests early.
	// Modern AWS SDKs (Go V2, Python boto3, Java V2) may default to Flexible Checksums,
	// which wraps the payload in aws-chunked Transfer-Encoding with checksum trailers.
	// GCS does not support aws-chunked framing, causing silent signature or body-parse failures.
	// Users must set AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED on their SDK client.
	if ce := r.Header.Get("Content-Encoding"); strings.Contains(ce, "aws-chunked") {
		log.Warn("Rejected aws-chunked request: GCS does not support Flexible Checksums trailers. "+
			"Client must set AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED",
			"content-encoding", ce,
			"method", r.Method,
			"uri", r.RequestURI,
			"user-agent", r.Header.Get("User-Agent"),
		)
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest",
			"This proxy does not support aws-chunked Transfer-Encoding (Flexible Checksums). "+
				"Please set the environment variable AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED "+
				"or configure your SDK client to disable automatic checksum trailers.")
		return
	}

	hasQueryParam := func(key string) bool {
		for k := range r.URL.Query() {
			if strings.EqualFold(k, key) {
				return true
			}
		}
		return false
	}

	// Check if this is a lifecycle request
	if hasQueryParam("lifecycle") {
		if r.Method == http.MethodPut {
			handlePutLifecycle(w, r)
			return
		} else if r.Method == http.MethodGet {
			handleGetLifecycle(w, r)
			return
		} else if r.Method == http.MethodDelete {
			handleDeleteLifecycle(w, r)
			return
		}
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
	if hasQueryParam("website") {
		if r.Method == http.MethodPut {
			handlePutWebsite(w, r)
			return
		} else if r.Method == http.MethodGet {
			handleGetWebsite(w, r)
			return
		} else if r.Method == http.MethodDelete {
			handleDeleteWebsite(w, r)
			return
		}
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

	// Return a synthetic response
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("Successfully proxied to GCS (DryRun - no real hits).")),
	}

	return resp, nil
}

func handlePutLifecycle(w http.ResponseWriter, r *http.Request) {
	log := reqLogger(r.Context())

	// 1. Read body (capped at 64 KB to match S3 control-plane limit)
	r.Body = http.MaxBytesReader(w, r.Body, maxControlPlaneBodySize)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			writeS3Error(w, http.StatusBadRequest, "MaxMessageLengthExceeded",
				fmt.Sprintf("Your request was too big. Max configuration size is %d bytes.", maxControlPlaneBodySize))
			return
		}
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "Failed to read request body.")
		return
	}
	log.Info("Read lifecycle request body", "body_size", len(body))

	// 2. Parse S3 XML
	var s3Cfg translate.LifecycleConfiguration
	if err := xml.Unmarshal(body, &s3Cfg); err != nil {
		log.Error("Failed to unmarshal S3 XML for Lifecycle", "error", err)
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		return
	}

	// 3. Translate S3 XML directly to GCS SDK Lifecycle struct
	storageLifecycle, err := translate.TranslateS3ToGCSLifecycle(&s3Cfg)
	if err != nil {
		log.Error("Failed to translate lifecycle to GCS SDK", "error", err)
		writeS3Error(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}

	// 4. If DryRun is true, return success without calling GCS
	if config.Config.DryRun {
		w.WriteHeader(http.StatusOK)
		return
	}

	// 5. Execute Bucket Update via GCS SDK
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		Lifecycle: storageLifecycle,
	}

	err = timeGCSCall(r.Context(), "PutBucketLifecycle", func(ctx context.Context) error {
		_, e := bucket.Update(ctx, uattrs)
		return e
	})
	if err != nil {
		log.Error("GCS API call failed for PutBucketLifecycle", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to update lifecycle configuration on GCS.")
		return
	}

	log.Info("Successfully updated GCS bucket lifecycle", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusOK)
}

func handleGetLifecycle(w http.ResponseWriter, r *http.Request) {
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	var attrs *storage.BucketAttrs
	err := timeGCSCall(r.Context(), "GetBucketLifecycle", func(ctx context.Context) error {
		var e error
		attrs, e = bucket.Attrs(ctx)
		return e
	})
	if err != nil {
		slog.Error("GCS API call failed for GetBucketLifecycle", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to retrieve lifecycle configuration from GCS.")
		return
	}

	s3Cfg := translate.TranslateGCSToS3Lifecycle(attrs.Lifecycle)
	if s3Cfg == nil || len(s3Cfg.Rules) == 0 {
		writeS3Error(w, http.StatusNotFound, "NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist.")
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml.NewEncoder(w).Encode(s3Cfg)
}

func handleDeleteLifecycle(w http.ResponseWriter, r *http.Request) {
	log := reqLogger(r.Context())
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		Lifecycle: &storage.Lifecycle{Rules: nil},
	}

	err := timeGCSCall(r.Context(), "DeleteBucketLifecycle", func(ctx context.Context) error {
		_, e := bucket.Update(ctx, uattrs)
		return e
	})
	if err != nil {
		slog.Error("GCS API call failed for DeleteBucketLifecycle", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to delete lifecycle configuration on GCS.")
		return
	}

	log.Info("Successfully deleted GCS bucket lifecycle", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusNoContent)
}

func handlePutCORS(w http.ResponseWriter, r *http.Request) {
	log := reqLogger(r.Context())

	// 1. Read body (capped at 64 KB to match S3 control-plane limit)
	r.Body = http.MaxBytesReader(w, r.Body, maxControlPlaneBodySize)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			writeS3Error(w, http.StatusBadRequest, "MaxMessageLengthExceeded",
				fmt.Sprintf("Your request was too big. Max configuration size is %d bytes.", maxControlPlaneBodySize))
			return
		}
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "Failed to read request body.")
		return
	}
	log.Info("Read CORS request body", "body_size", len(body))

	// 2. Parse S3 XML
	var s3Cfg translate.CORSConfiguration
	if err := xml.Unmarshal(body, &s3Cfg); err != nil {
		log.Error("Failed to unmarshal S3 XML for CORS", "error", err)
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		return
	}

	// 3. Translate to GCS CORS
	gcsCORS, droppedHeaders := translate.TranslateS3ToGCSCors(&s3Cfg)

	// Warn client about unsupported AllowedHeaders via response header
	if len(droppedHeaders) > 0 {
		w.Header().Set("X-S3Proxy-Warning",
			fmt.Sprintf("AllowedHeaders not supported by GCS and were ignored: %s", strings.Join(droppedHeaders, ", ")))
	}

	// 4. In DryRun mode, just print/return success
	if config.Config.DryRun {
		w.WriteHeader(http.StatusOK)
		
		return
	}

	// 5. Execute Bucket Update via GCS SDK
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		CORS: gcsCORS,
	}

	err = timeGCSCall(r.Context(), "PutBucketCors", func(ctx context.Context) error {
		_, e := bucket.Update(ctx, uattrs)
		return e
	})
	if err != nil {
		log.Error("GCS API call failed for PutBucketCors", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to update CORS configuration on GCS.")
		return
	}

	log.Info("Successfully updated GCS bucket CORS", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusOK)
}

func handleGetCORS(w http.ResponseWriter, r *http.Request) {
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	var attrs *storage.BucketAttrs
	err := timeGCSCall(r.Context(), "GetBucketCors", func(ctx context.Context) error {
		var e error
		attrs, e = bucket.Attrs(ctx)
		return e
	})
	if err != nil {
		slog.Error("GCS API call failed for GetBucketCors", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to retrieve CORS configuration from GCS.")
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
	log := reqLogger(r.Context())
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		CORS: []storage.CORS{},
	}

	err := timeGCSCall(r.Context(), "DeleteBucketCors", func(ctx context.Context) error {
		_, e := bucket.Update(ctx, uattrs)
		return e
	})
	if err != nil {
		slog.Error("GCS API call failed for DeleteBucketCors", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to delete CORS configuration on GCS.")
		return
	}

	log.Info("Successfully deleted GCS bucket CORS", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusNoContent)
}

func handlePutLogging(w http.ResponseWriter, r *http.Request) {
	log := reqLogger(r.Context())

	// Read body (capped at 64 KB to match S3 control-plane limit)
	r.Body = http.MaxBytesReader(w, r.Body, maxControlPlaneBodySize)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			writeS3Error(w, http.StatusBadRequest, "MaxMessageLengthExceeded",
				fmt.Sprintf("Your request was too big. Max configuration size is %d bytes.", maxControlPlaneBodySize))
			return
		}
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "Failed to read request body.")
		return
	}
	log.Info("Read logging request body", "body_size", len(body))

	var s3Cfg translate.BucketLoggingStatus
	if err := xml.Unmarshal(body, &s3Cfg); err != nil {
		log.Error("Failed to unmarshal S3 XML for Logging", "error", err)
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		return
	}

	gcsLogging := translate.TranslateS3ToGCSLogging(s3Cfg)

	if config.Config.DryRun {
		w.WriteHeader(http.StatusOK)
		
		return
	}

	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		Logging: gcsLogging,
	}

	err = timeGCSCall(r.Context(), "PutBucketLogging", func(ctx context.Context) error {
		_, e := bucket.Update(ctx, uattrs)
		return e
	})
	if err != nil {
		log.Error("GCS API call failed for PutBucketLogging", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to update logging configuration on GCS.")
		return
	}

	log.Info("Successfully updated GCS bucket Logging", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusOK)
}

func handleGetLogging(w http.ResponseWriter, r *http.Request) {
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	var attrs *storage.BucketAttrs
	err := timeGCSCall(r.Context(), "GetBucketLogging", func(ctx context.Context) error {
		var e error
		attrs, e = bucket.Attrs(ctx)
		return e
	})
	if err != nil {
		slog.Error("GCS API call failed for GetBucketLogging", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to retrieve logging configuration from GCS.")
		return
	}

	s3Cfg := translate.TranslateGCSToS3Logging(attrs.Logging)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml.NewEncoder(w).Encode(s3Cfg)
}

func handleDeleteLogging(w http.ResponseWriter, r *http.Request) {
	log := reqLogger(r.Context())
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		Logging: &storage.BucketLogging{},
	}

	err := timeGCSCall(r.Context(), "DeleteBucketLogging", func(ctx context.Context) error {
		_, e := bucket.Update(ctx, uattrs)
		return e
	})
	if err != nil {
		slog.Error("GCS API call failed for DeleteBucketLogging", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to delete logging configuration on GCS.")
		return
	}

	log.Info("Successfully deleted GCS bucket Logging", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusNoContent)
}

func handlePutWebsite(w http.ResponseWriter, r *http.Request) {
	log := reqLogger(r.Context())

	// Read body (capped at 64 KB to match S3 control-plane limit)
	r.Body = http.MaxBytesReader(w, r.Body, maxControlPlaneBodySize)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			writeS3Error(w, http.StatusBadRequest, "MaxMessageLengthExceeded",
				fmt.Sprintf("Your request was too big. Max configuration size is %d bytes.", maxControlPlaneBodySize))
			return
		}
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "Failed to read request body.")
		return
	}
	log.Info("Read website request body", "body_size", len(body))

	var s3Cfg translate.WebsiteConfiguration
	if err := xml.Unmarshal(body, &s3Cfg); err != nil {
		log.Error("Failed to unmarshal S3 XML for Website", "error", err)
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		return
	}

	gcsWebsite := translate.TranslateS3ToGCSWebsite(s3Cfg)

	if config.Config.DryRun {
		w.WriteHeader(http.StatusOK)
		
		return
	}

	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		Website: gcsWebsite,
	}

	err = timeGCSCall(r.Context(), "PutBucketWebsite", func(ctx context.Context) error {
		_, e := bucket.Update(ctx, uattrs)
		return e
	})
	if err != nil {
		log.Error("GCS API call failed for PutBucketWebsite", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to update website configuration on GCS.")
		return
	}

	log.Info("Successfully updated GCS bucket Website", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusOK)
}

func handleGetWebsite(w http.ResponseWriter, r *http.Request) {
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	var attrs *storage.BucketAttrs
	err := timeGCSCall(r.Context(), "GetBucketWebsite", func(ctx context.Context) error {
		var e error
		attrs, e = bucket.Attrs(ctx)
		return e
	})
	if err != nil {
		slog.Error("GCS API call failed for GetBucketWebsite", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to retrieve website configuration from GCS.")
		return
	}

	s3Cfg := translate.TranslateGCSToS3Website(attrs.Website)
	if s3Cfg == nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchWebsiteConfiguration", "The specified bucket does not have a website configuration.")
		return
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml.NewEncoder(w).Encode(s3Cfg)
}

func handleDeleteWebsite(w http.ResponseWriter, r *http.Request) {
	log := reqLogger(r.Context())
	bucket := gcsClient.Bucket(config.Config.TargetBucket)
	uattrs := storage.BucketAttrsToUpdate{
		Website: &storage.BucketWebsite{},
	}

	err := timeGCSCall(r.Context(), "DeleteBucketWebsite", func(ctx context.Context) error {
		_, e := bucket.Update(ctx, uattrs)
		return e
	})
	if err != nil {
		log.Error("GCS API call failed for DeleteBucketWebsite", "error", err)
		writeS3Error(w, http.StatusBadGateway, "InternalError", "Failed to delete website configuration on GCS.")
		return
	}

	log.Info("Successfully deleted GCS bucket Website", "bucket", config.Config.TargetBucket)
	w.WriteHeader(http.StatusNoContent)
}

func handlePutObjectTagging(w http.ResponseWriter, r *http.Request) {
	log := reqLogger(r.Context())

	// Read body (capped at 64 KB to match S3 control-plane limit)
	r.Body = http.MaxBytesReader(w, r.Body, maxControlPlaneBodySize)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if err.Error() == "http: request body too large" {
			writeS3Error(w, http.StatusBadRequest, "MaxMessageLengthExceeded",
				fmt.Sprintf("Your request was too big. Max configuration size is %d bytes.", maxControlPlaneBodySize))
			return
		}
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "Failed to read request body.")
		return
	}
	log.Info("Read tagging request body", "body_size", len(body))

	var s3Cfg translate.Tagging
	if err := xml.Unmarshal(body, &s3Cfg); err != nil {
		log.Error("Failed to unmarshal S3 XML for Tagging", "error", err)
		writeS3Error(w, http.StatusBadRequest, "MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.")
		return
	}

	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) < 2 || pathParts[0] == "" || pathParts[1] == "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "Bucket and Object name required.")
		return
	}
	targetBucket := pathParts[0]
	targetObject := strings.Join(pathParts[1:], "/")

	log.Info("Applying Tagging to GCS Object", "bucket", targetBucket, "object", targetObject)

	if config.Config.DryRun {
		log.Info("[DRY_RUN] Would apply Tagging to GCS Object", "bucket", targetBucket, "object", targetObject)
		w.WriteHeader(http.StatusOK)
		
		return
	}

	obj := gcsClient.Bucket(targetBucket).Object(targetObject)
	var attrs *storage.ObjectAttrs
	err = timeGCSCall(r.Context(), "GetObjectAttrs_Tagging", func(ctx context.Context) error {
		var e error
		attrs, e = obj.Attrs(ctx)
		return e
	})
	if err != nil {
		log.Error("GCS API call failed for GetObjectAttrs_Tagging", "error", err)
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}

	updateMetadata := translate.TranslateS3ToGCSTagging(s3Cfg, attrs.Metadata)
	uattrs := storage.ObjectAttrsToUpdate{
		Metadata: updateMetadata,
	}

	err = timeGCSCall(r.Context(), "PutObjectTagging", func(ctx context.Context) error {
		_, e := obj.If(storage.Conditions{
			MetagenerationMatch: attrs.Metageneration,
		}).Update(ctx, uattrs)
		return e
	})
	if err != nil {
		log.Error("GCS API call failed for PutObjectTagging", "error", err)
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "Failed to update object tagging.")
		return
	}

	log.Info("Successfully updated GCS Object Tagging", "bucket", targetBucket, "object", targetObject)
	w.WriteHeader(http.StatusOK)
}

func handleGetObjectTagging(w http.ResponseWriter, r *http.Request) {
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) < 2 || pathParts[0] == "" || pathParts[1] == "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "Bucket and Object name required.")
		return
	}
	targetBucket := pathParts[0]
	targetObject := strings.Join(pathParts[1:], "/")

	if config.Config.DryRun {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Tagging><TagSet></TagSet></Tagging>")
		return
	}

	obj := gcsClient.Bucket(targetBucket).Object(targetObject)
	var attrs *storage.ObjectAttrs
	err := timeGCSCall(r.Context(), "GetObjectAttrs_GetTagging", func(ctx context.Context) error {
		var e error
		attrs, e = obj.Attrs(ctx)
		return e
	})
	if err != nil {
		slog.Error("GCS API call failed for GetObjectAttrs_GetTagging", "error", err)
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}

	s3Cfg := translate.TranslateGCSToS3Tagging(attrs.Metadata)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	xml.NewEncoder(w).Encode(s3Cfg)
}

func handleDeleteObjectTagging(w http.ResponseWriter, r *http.Request) {
	log := reqLogger(r.Context())

	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(pathParts) < 2 || pathParts[0] == "" || pathParts[1] == "" {
		writeS3Error(w, http.StatusBadRequest, "InvalidArgument", "Bucket and Object name required.")
		return
	}
	targetBucket := pathParts[0]
	targetObject := strings.Join(pathParts[1:], "/")

	if config.Config.DryRun {
		log.Info("[DRY_RUN] Would delete Tagging from GCS Object", "bucket", targetBucket, "object", targetObject)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	obj := gcsClient.Bucket(targetBucket).Object(targetObject)
	var attrs *storage.ObjectAttrs
	err := timeGCSCall(r.Context(), "GetObjectAttrs_DeleteTagging", func(ctx context.Context) error {
		var e error
		attrs, e = obj.Attrs(ctx)
		return e
	})
	if err != nil {
		log.Error("GCS API call failed for GetObjectAttrs_DeleteTagging", "error", err)
		writeS3Error(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
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
		_, e := obj.If(storage.Conditions{
			MetagenerationMatch: attrs.Metageneration,
		}).Update(ctx, uattrs)
		return e
	})
	if err != nil {
		log.Error("GCS API call failed for DeleteObjectTagging", "error", err)
		writeS3Error(w, http.StatusInternalServerError, "InternalError", "Failed to delete object tagging.")
		return
	}

	log.Info("Successfully deleted GCS Object Tagging", "bucket", targetBucket, "object", targetObject)
	w.WriteHeader(http.StatusNoContent)
}

func writeS3Error(w http.ResponseWriter, statusCode int, code string, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(statusCode)
	fmt.Fprintf(w, "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>%s</Code><Message>%s</Message></Error>\n", code, message)
}
