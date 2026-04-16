# Go S3 to GCS Proxy

This project acts as a middleware proxy between AWS S3-compatible client SDKs and Google Cloud Storage (GCS). It translates unsupported or edge-case S3 features into GCS-compatible operations transparently.

## Getting Started

### Prerequisites
- **Go 1.21+** (Download from [golang.org](https://golang.org/))

### Configuration
The proxy configuration depends on `.env` file or direct environment variables.

Copy the `.env` template:
```bash
cp .env.example .env
```

Available Configuration Options:
-   `PORT` (Default: `8080`): The port the proxy listens on.
-   `GCP_PROJECT_ID`: The target Google Cloud Project ID.
-   `TARGET_BUCKET`: The target GCS bucket name.
-   `STORAGE_BASE_URL` (Default: `https://storage.googleapis.com`): The GCS endpoint URL.
-   `GCS_PREFIX`: Subfolder prefix for testing or namespacing.
-   `DRY_RUN` (Default: `true`): Disables real GCS API hits (safe for laptop testing). Set to `false` for live integration.
-   `JSON_KEY`: Path to the Google Cloud Service Account JSON key (required for real GCS API calls like Website/CORS).
-   `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`: Proxy's HMAC credentials for re-signing requests to GCS.
-   `MAX_IDLE_CONNS` (Default: `1000`): Maximum idle connections for the reverse proxy.
-   `MAX_IDLE_CONNS_PER_HOST` (Default: `1000`): Maximum idle connections per host for the reverse pool.

## Using with standard AWS S3 SDK (Zero Code Change via HTTP_PROXY)

You can route all traffic from your S3 client application to the proxy transparently by setting standard proxy environment variables. This allows you to use standard S3 endpoints in your code without modifying the initialization logic.

### 1. Set Environment Variables
Set the `HTTP_PROXY` or `HTTPS_PROXY` (depending on whether your proxy uses TLS) to point to your local proxy instance.

```bash
export HTTP_PROXY=http://localhost:8081
export HTTPS_PROXY=http://localhost:8081
```

### 2. Configure Client to use PathStyle
While you don't need to change the endpoint or transport, ensure your SDK is configured to use **Path-Style addressing** (required for GCS S3 compatibility).

---

## Using with standard AWS S3 SDK (Explicit Client Transport)

Instead of setting a system-wide environment variable, you can configure your S3 client's `HTTPClient` transport to route traffic to the proxy while keeping standard endpoints in signatures. This allows you guards against side effects for non-S3 traffic.

### Go SDK v2 Example

```go
import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func createS3Client() (*s3.Client, error) {
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "storage.googleapis.com") {
				return dialer.DialContext(ctx, network, "localhost:8081")
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}

	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		return nil, err
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
		o.HTTPClient = &http.Client{Transport: transport}
		o.BaseEndpoint = aws.String("http://storage.googleapis.com")
	}), nil
}
```

## Features

- **Multi-Object Delete**: Fully supports bulk `DeleteObjects` using standard S3 XML payloads routed directly to GCS's native XML API via HMAC re-signing.
- **Lifecycle Intercept**: Translates S3 XML Lifecycle Configuration to GCS JSON.
- **Real GCS Forwarding**: Submits translated JSON to GCS via official GCS Go SDK.
- **Structured JSON Logging**: Native `log/slog` for modern cloud observability (Parsable JSON lines). Toggle `DEBUG_LOGGING=true` for verbose output.
- **Prometheus Metrics**: Exposes `/metrics` with HTTP, GCS SDK, upstream proxy, readiness, and rejection metrics for operational visibility.
- **Reliable Timeouts**: Set timeouts on `http.Transport` to prevent hanging connections.
- **Graceful Shutdown**: Listens for `SIGTERM`/`SIGINT` and waits up to 10s for draining requests.
- **Prefix Isolation**: Use `GCS_PREFIX` for test isolation.
- **DryRun Toggle**: Use `DRY_RUN=true` to disable real GCS API hits (safe for local laptop testing).

---

## Observability

The proxy exposes the following operational endpoints on the same port as the main service:

- `GET /health`: basic liveness check
- `GET /readyz`: readiness check backed by a GCS bucket metadata call unless `DRY_RUN=true`
- `GET /metrics`: Prometheus scrape endpoint

### Key Metrics

- `s3proxy_http_requests_total{method,route,status}`: total inbound requests split by fixed route labels such as `s3`, `health`, `readyz`, `metrics`, `lifecycle`, `cors`, `logging`, `website`, and `tagging`
- `s3proxy_http_request_duration_seconds{method,route}`: end-to-end request latency seen by clients
- `s3proxy_http_in_flight_requests{route}`: current in-flight requests per route
- `s3proxy_gcs_sdk_requests_total{operation,result}`: success/error counts for control-plane GCS SDK calls
- `s3proxy_gcs_sdk_request_duration_seconds{operation,result}`: latency of control-plane GCS SDK calls
- `s3proxy_upstream_requests_total{method,status_class}`: upstream GCS proxy traffic split by HTTP method and status class
- `s3proxy_upstream_request_duration_seconds{method}`: upstream GCS proxy latency
- `s3proxy_requests_rejected_total{reason}`: degraded or rejected requests such as invalid object paths or re-sign failures
- `s3proxy_readiness_checks_total{result}`: readiness probe success/error counts

### Label Guardrails

- Route and reason labels are fixed low-cardinality enums to avoid cardinality blow-ups.
- Raw paths, bucket names, and object keys are intentionally not exported as metric labels.
- Upstream status is grouped as `2xx`, `4xx`, `5xx`, or `error` rather than per-object labels.

### Scrape Example

```bash
curl http://localhost:8080/metrics
```

---

## Technical Features

### Lifecycle Translation

The proxy intercepts `PUT /?lifecycle` and maps it directly to Google Cloud Storage. Standard actions like `Expiration` (Delete) and `Transition` (SetStorageClass) are translated into GCS JSON schemas.

- To verify the translation locally, you can use the unit tests in `pkg/translate`.
- To see it in action, run the proxy and hit the endpoint with a standard S3 XML payload.

---

## 🔬 Integration Tests (Isolated Module)

To run automated integration tests using the **AWS S3 Go SDK** without polluting the main project module, we use an isolated sub-module:

```bash
cd integration_tests
/usr/local/go/bin/go mod tidy
/usr/local/go/bin/go test -v ./...
```

The test will automatically spawn the local proxy, run tests using the real AWS SDK client, and report results!

---

## Development & Usage

Initialize dependencies:
```bash
go mod tidy
```

Run the server locally:
```bash
go run .
```

---

## 📂 File Structure & Features

### Root
- **[main.go](file:///Users/deckardy/gitlab/s3proxy4gcs/main.go)**: Router entry point. Intercepts custom XML operations and falls through to a high-performance Reverse Proxy for all standard object traffic. Uses tuned connection pooling for speed. **Enforces standard S3 XML error responses and propagates request contexts for automatic cost cancellation.**
- **[config/settings.go](file:///Users/deckardy/gitlab/s3proxy4gcs/config/settings.go)**: Centralized environment configuration (Port, Bucket, DryRun, Connection Limits).

### Package `pkg/translate`
Handles bi-directional translation between AWS S3 XML schemas and Google Cloud Storage schemas:
- **`s3_*.go`**: Defines the incoming AWS S3 XML Structs (Parsing).
- **`gcs_*.go`**: Translates the parsed S3 structs into GCS SDK types or JSON payloads.

#### Feature Files:
- **[lifecycle](file:///Users/deckardy/gitlab/s3proxy4gcs/pkg/translate/gcs_lifecycle.go)**: Maps Lifecycle settings with rule rejections for unsupported filters.
- **[cors](file:///Users/deckardy/gitlab/s3proxy4gcs/pkg/translate/gcs_cors.go)**: Maps S3 XML CORS permissions to GCS Go SDK types.
- **[logging](file:///Users/deckardy/gitlab/s3proxy4gcs/pkg/translate/gcs_logging.go)**: Parses and holds bucket logging specifications.
- **[website](file:///Users/deckardy/gitlab/s3proxy4gcs/pkg/translate/gcs_website.go)**: Maps main page suffixes and 404 error documents.
- **[tagging](file:///Users/deckardy/gitlab/s3proxy4gcs/pkg/translate/gcs_tagging.go)**: Translates tags into GCS custom metadata using Optimistic Concurrency Control (OCC) to prevent overwrite losses.
