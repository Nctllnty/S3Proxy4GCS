# Solutions for S3 to GCS Proxy

This document outlines the proposed solutions for resolving S3 features that are currently unsupported or have limited compatibility in Google Cloud Storage (GCS).

## Architecture Overview

The solution will be implemented as a **Middleware Proxy**. We are considering two primary deployment patterns:

1. **Centralized Proxy Service (e.g., Cloud Run)**
   - Deployed as a standalone microservice behind a load balancer. 
   - Handles all routing and heavy translation loads.

2. **GLB Service Extension (Callouts)**
   - Deployed directly at the Google Cloud Load Balancing (GLB) layer.
   - Intercepts requests using WebAssembly or external gRPC callouts.
   - **Pros**: Lower latency for pass-through traffic, tightly integrated with GLB routing.
   - **Cons**: More complex development model (Service Extensions API).

The proxy layer will handle:
1. **Request Interception**: Identifying subresources (like `?tagging`, `?lifecycle`) that require special handling.
2. **Body Translation**: Converting S3 XML schemas to GCS-compatible formats (XML or JSON).
3. **Authentication**: Re-signing requests before forwarding to GCS.



### Feature Feasibility by Proxy Type

Choosing between a GLB Extension and a Cloud Run proxy depends on whether the feature requires stateless header modification, payload translation, or stateful orchestration:

**🟢 Perfect for GLB Extensions (Header/Routing Modification)**
- **Versioning Interop**: Injecting the `x-amz-interop-list-objects-format: enabled` header upon request and mapping `x-goog-generation` to `x-amz-version-id` on the response.
- **RestoreObject (Synthetic Responses)**: Immediately returning `200 OK` without hitting GCS, since objects are "live". _Implemented in the Cloud Run proxy as of v1.5 with an optional HEAD existence probe so missing keys still return `404 NoSuchKey`._
- **Proxy Protection (ABAC)**: Inspecting the URL to proactively reject unsupported requests like `PUT ?policy` with `501 Not Implemented`.
- **Transparent Tag Translation**: Rewriting standard S3 `x-amz-tagging` upload headers into GCS custom metadata `x-goog-meta-s3tag-` instantly.

**🟡 Difficult for GLB Extensions (Requires Body Translation & Re-signing)**
- **DeleteObjects (Multi-Object Delete)**: Fully supported by passing the HMAC v4 re-signed payload directly to GCS's native XML API, requiring no heavy custom fan-out translation in the proxy.
- **XML Parsing (Lifecycle, CORS, Logging)**: Modifying the XML body invalidates the original AWS v4 signature. Generating a new GCP HMAC signature within a load balancer extension is heavily resource-intensive and risks hitting execution timeouts. Best routed to a dedicated Cloud Run instance.

**🔴 Unsuitable for GLB Extensions (Stateful/Orchestration)**
- **S3 `?tagging` API**: The read-modify-write cycle (GET metadata -> merge -> PUT) is too slow for load balancer inline validation.
- **Upload Part Copy**: Exceeds memory/streaming limits due to required buffering.

---

## Proposed Solutions per Feature

We have redefined the feature categories based on **latency impact, proxy resource consumption (CPU/Memory), and architectural complexity**. The "Hard" group requires deeper discussion with the customer to align on performance and cost trade-offs.

### Group A: Control-Plane Configuration (Easy / Low Impact)
These features are infrequent bucket management API calls. The proxy only needs to parse XML/JSON payloads and swap schemas. They **do not** impact the latency, memory, or bandwidth of heavy data-plane object transfers.

#### 1. Static Website Configuration
**Decision**: Stateless configuration mapping.
- **CORS**: **[Implemented ✅]** Translate `CORSConfiguration` XML to GCS CORS JSON bucket settings. Full PUT/GET/DELETE CRUD with bi-directional translation.
- **Logging**: **[Implemented ✅]** Map S3 bucket logging to GCS TargetBucket/Prefix using Go SDK. Full PUT/GET/DELETE CRUD.
- **Website**: **[Implemented ✅]** Map `IndexDocument`/`ErrorDocument` to GCS website fields. Full PUT/GET/DELETE CRUD.

#### 2. Lifecycle Management — **[Implemented ✅]**
**Decision**: **Translation Layer**. The proxy intercepts `PUT/GET/DELETE /?lifecycle` and maps S3 actions (Expiration, Transition) to GCS Bucket Lifecycle configuration. Bi-directional translation (S3 XML ↔ GCS JSON) with full CRUD support. Rejects unsupported filters (Size, Tags) to prevent scope broadening.

### Group B: Data-Plane & Stateful Operations (Hard / High Impact)
These features intercept high-frequency data path operations or require heavy background processing. They introduce significant latency, require high proxy resources (memory/connections), or involve complex race conditions.

#### 1. Tagging (Object Tagging) — **[Implemented ✅]**
**Issue**: GCS lacks an exact `?tagging` equivalent and relies on object metadata. 
**Implementation**: The Proxy transparently translates `PUT ?tagging` requests directly into GCS Object Custom Metadata (`x-goog-meta-s3tag-`). It uses a read-modify-write cycle with **Optimistic Concurrency Control (IfMetagenerationMatch)** to prevent lost updates safely without heavy locking.

#### 2. Access Control (ACLs & Policies & Tag-Based ABAC) - **[Deferred]**
**Issue**: S3 uses XML ACLs, JSON Bucket Policies. GCS uses IAM.
**Proxy Impact**: Extreme Latency / Infeasible on Data Path for tag-based ABAC.
**Recommendation**: Shift to prefix-based security rather than object-level tags.

#### 3. DeleteObjects (Multi-Object Delete) - **[Implemented ✅]**
**Issue**: Previously believed GCS S3-compatible API did not support bulk `DeleteObjects`.
**Resolution**: GCS XML API natively supports `POST /?delete` for bulk deletion of up to 1000 objects per request. The proxy transparently passes through the request with SigV4 re-signing, stripping non-compliant headers before forwarding. No fan-out or special handling required.

#### 4. UploadPartCopy - **[Native ✅]**
**Issue**: Previously believed to require large memory buffers for proxy-side implementation.
**Resolution**: GCS S3-compatible API natively supports `UploadPartCopy` (`PUT` with `x-amz-copy-source` + `uploadId` + `partNumber`). The proxy transparently passes through with SigV4 re-signing. No buffering or special handling required.

#### 4. Inventory Data Manifests - **[Deferred]**
**Issue**: Automations expect specific S3 Inventory output formats.
**Proxy Impact**: Requires External Stateful ETL Worker.

#### 5. Flexible Checksums (aws-chunked unwrapping) - **[Deferred]**
**Issue**: Modern SDKs use `aws-chunked` framing for checksum trailers, unsupported by GCS.
**Proxy Impact**: Extreme Memory/Bandwidth Overhead. Unwrapping requires heavy stream parsing and may limit high-speed transparent data-plane throughput.
**Recommendation**: Use client-side `AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED` or use standard `Content-MD5` headers for integrity.

---

## SDK Compatibility & Client-Side Workarounds

The proxy has been validated against **6 AWS SDKs** with full data-plane and control-plane test coverage (60/60 PASS). Below documents both the known issues and the required client-side configurations for each SDK.

### Known Issues & Solutions

#### 1. Flexible Checksums / aws-chunked (Signature Mismatch)
**Issue**: Modern SDKs (Python, Java V2, Go V2, C++) default to "Flexible Checksums" using `aws-chunked` body framing with trailer checksums. GCS does not support this format, resulting in `SignatureDoesNotMatch` or corrupted stored data.
**Solution**: A combination of client-side and proxy-side fixes:
- **Client**: Set env var `AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED` (Java V2, C++)
- **Client**: Disable chunked encoding in SDK config (Java V1: `withChunkedEncodingDisabled(true)`, Java V2: `chunkedEncodingEnabled(false)`)
- **Proxy**: Automatically strips `Content-Encoding: aws-chunked`, `X-Amz-Decoded-Content-Length`, `X-Amz-Trailer` headers before re-signing

#### 2. Content-MD5 Header Conflict
**Issue**: Go V1 and Java V1 SDKs compute `Content-MD5` and include it in the signed request. After proxy re-signing, this header causes GCS signature verification to fail.
**Solution**:
- **Proxy**: Automatically strips `Content-Md5` header before re-signing (no client-side config needed)

#### 3. Accept-Encoding Header (Go V1 403 Root Cause)
**Issue**: Go V1 SDK sends `Accept-Encoding` header which gets included in `SignedHeaders`. GCS modifies or strips this header during HTTP transport, causing the canonical request to not match the signed request, resulting in `403 SignatureDoesNotMatch`.
**Solution**:
- **Proxy**: Automatically strips `Accept-Encoding` header before re-signing

#### 4. Expect: 100-Continue (Go V1)
**Issue**: Go V1 SDK sends `Expect: 100-continue` header that interferes with GCS HMAC signature verification.
**Solution**:
- **Proxy**: Automatically strips `Expect` header before re-signing (no client-side config needed)

#### 5. Java V2 CopyObject (`411 Length Required`)
**Issue**: The default `UrlConnectionHttpClient` in the Java V2 SDK incorrectly omits the `Content-Length: 0` header on empty `PUT` requests, causing GCS to reject `CopyObject`.
**Solution**: Explicitly bind the alternative `ApacheHttpClient` in the Java V2 client configuration, which correctly transmits the `Content-Length` header.
**Status**: ⚠️ Known issue, not yet implemented in proxy. Client-side workaround required.

#### 6. RestoreObject
**Issue**: Throws `InvalidArgument` against GCS.
**Solution (Implemented, v1.5+)**: The proxy intercepts `POST /<bucket>/<key>?restore` and synthesises a `200 OK` response. GCS objects in every storage class (`STANDARD` / `NEARLINE` / `COLDLINE` / `ARCHIVE`) are directly readable without any thaw step, so the synthetic response is semantically equivalent to an "already restored" AWS S3 reply. Missing keys still return `404 NoSuchKey` so callers that immediately follow up with `GetObject` see a coherent story; set `RESTORE_SKIP_EXISTENCE_CHECK=true` to skip that HEAD probe in latency-sensitive workloads. Non-POST verbs on `?restore` are rejected with `501 NotImplemented`.
**Cost note**: GCS does not charge a restore/thaw fee, but every `GetObject` on a colder tier still incurs a per-read retrieval charge. Making `RestoreObject` a no-op does not hide that cost; reassess storage class placement for frequently-read data.

#### 7. Storage Classes
**Issue**: GCS rejects AWS-specific storage class values (e.g., `STANDARD_IA`, `GLACIER`).
**Solution (Validated)**: The proxy transparently translates AWS storage classes to GCS equivalents before forwarding:
- `STANDARD` / `REDUCED_REDUNDANCY` -> `STANDARD`
- `STANDARD_IA` / `ONEZONE_IA` -> `NEARLINE`
- `GLACIER_IR` (Instant Retrieval) -> `COLDLINE`
- `GLACIER` / `DEEP_ARCHIVE` -> `ARCHIVE`
- `INTELLIGENT_TIERING` -> `AUTOCLASS`
- Any other value -> `400 InvalidStorageClass` (v1.4+ change; previously silently remapped to `NEARLINE`, which violated AGENTS rule 4).

#### 8. Versioning (ListObjectVersions / HeadObject)
**Issue**: GCS uses `<Generation>` instead of `<VersionId>`.
**Resolution**: GCS S3-compatible API natively handles versioning interop (generation ↔ version-id mapping) without proxy intervention. The proxy transparently passes through versioning requests. No special header injection or response rewriting is needed.

---

### Proxy-Side Header Stripping (Automatic)

The proxy Director strips the following headers before SigV4 re-signing to ensure GCS HMAC verification passes. This is fully transparent to client applications:

| Header | Triggered By | Reason |
|---|---|---|
| `User-Agent` | All SDKs | AWS-format User-Agent interferes with GCS canonical request |
| `Content-Md5` | Go V1, Java V1 | SDK-computed MD5 invalidated after re-signing |
| `Expect` | Go V1 | `100-continue` causes GCS signature mismatch |
| `Accept-Encoding` | Go V2 | Go V2 gzip middleware sends `identity`, GCS modifies in transport → canonical request mismatch |
| `Amz-Sdk-Invocation-Id` | Java V1/V2 | AWS internal tracking, GCS rejects unknown signed headers |
| `Amz-Sdk-Request` | Java V1/V2 | AWS retry metadata, not recognized by GCS |
| `X-Amz-Decoded-Content-Length` | Java V1/V2 | aws-chunked artifact, meaningless after unwrap |
| `X-Amz-Trailer` | Java V2 | Flexible Checksums trailer declaration |
| `Content-Encoding` | Java V1/V2 | Conditionally stripped when value contains `aws-chunked` |

---

### Per-SDK Required Configuration

#### Go V2 (aws-sdk-go-v2 v1.75+) — Zero Configuration
```go
o.BaseEndpoint = aws.String(proxyEndpoint)
```
No special flags or env vars needed. With `PROXY_BASE_DOMAIN` enabled, no path-style config required.

#### Go V1 (aws-sdk-go v1.50+) — Zero Configuration
```go
Endpoint: aws.String(proxyEndpoint),
```
No special flags or env vars needed. The proxy automatically strips `Content-MD5`, `Expect`, and `Accept-Encoding` headers before re-signing. With `PROXY_BASE_DOMAIN` enabled, no path-style config required.

#### Python / boto3 (1.42+) — Zero Configuration
```python
endpoint_url=proxy_endpoint
```
No special flags or env vars needed. With `PROXY_BASE_DOMAIN` enabled, no path-style config required.

#### Java V1 (1.12+) — 1 Required Setting
```java
.withChunkedEncodingDisabled(true)
```

#### Java V2 (2.20+) — 1 Code Setting + 2 Env Vars
```java
.serviceConfiguration(S3Configuration.builder()
    .chunkedEncodingEnabled(false)
    .build())
```
**Required env vars:** `AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED`, `AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED`

#### C++ (1.11+) — 1 Recommended Setting + 1 Env Var
```cpp
Aws::Client::AWSAuthV4Signer::PayloadSigningPolicy::Never  // Recommended
/* useVirtualAddressing */ true
```
**Required env var:** `AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED`

> **Note**: With `PROXY_BASE_DOMAIN` set on the server, all SDKs can use default virtual-hosted addressing. Path-style is only needed when `PROXY_BASE_DOMAIN` is not configured.

---

### Explicit Client Transport Routing Strategy

**Issue**: Customers prefer scoped routing over global environment variables, or want to avoid setting system-wide `HTTP_PROXY` which might affect other services.
**Solution**: Use a custom `http.Transport` with `DialContext` when initializing the S3 SDK client.
- **How it works**: The S3 SDK signs requests for `storage.googleapis.com` (preserving signature integrity). The underlying `DialContext` overrides the connection to route to the local proxy (`localhost:8081`).
- **Scope**: Isolated purely to the S3 Client instance. No side effects for other services or standard HTTP requests using `http.DefaultClient`.

---

## Technical Considerations

### Authentication & Re-signing
When the proxy modifies a request body (e.g., translating Lifecycle XML), the original AWS V4 signature becomes invalid because the payload hash changes.
- **Requirement**: The proxy must possess a GCS Service Account HMAC key to **re-sign** requests before forwarding them to GCS.
- **Workflow**: 
  1. Validate incoming S3 signature.
  2. Modify request (translate body).
  3. Generate new signature with proxy's HMAC keys.
  4. Forward to GCS.

### HTTP vs HTTPS Tradeoffs in Internal Networks

When deploying the proxy in a private VPC, you can choose between unencrypted HTTP or HTTPS for both inbound (Client to Proxy) and outbound (Proxy to GCS) traffic.

#### ⚖️ Summary Comparison:
| Type | Performance | Security | Network Integrity |
| :--- | :--- | :--- | :--- |
| **HTTP (Unencrypted)** | 🚀 **Highest** (No TLS handshake overhead) | ❌ **Low** (Sniffable if VPC is breached) | ⚠️ **TCP Checksum only** (Weak) |
| **HTTPS (HTTP/2 with TLS 1.3)** | ⚡ **Fast** (Multiplexed requests via single connection) | ✅ **High** (Encrypted in transit) | ✅ **AEAD MAC Protection** (Prevents bit-flips during transit) |

#### Recommendations:
1. **Outbound (Proxy to GCS)**: Use **HTTPS with HTTP/2** (Default: `https://storage.googleapis.com`). This ensures that data leaving your proxy environment is encrypted and protected against bit-flips by TLS’s native integrity checks (AEAD), even if application-level checksums (`aws-chunked`) are disabled for speed.
2. **Inbound (Client to Proxy)**: In a trusted internal VPC, **unencrypted HTTP** is often preferred to save TLS handshake latency for every client connect.

---

---

## Implementation Status Summary

### Control Plane — XML Bi-directional Translation

Bucket/Object configuration APIs intercepted by the proxy, performing S3 XML ↔ GCS SDK struct translation via `pkg/translate`.

| Feature | Methods | Status | Scope |
|---|---|---|---|
| **Lifecycle** | PUT / GET / DELETE | ✅ Implemented | S3 XML ↔ GCS JSON, unsupported filter rejection (Size/Tags) |
| **CORS** | PUT / GET / DELETE | ✅ Implemented | S3 XML ↔ GCS CORS struct translation |
| **Logging** | PUT / GET / DELETE | ✅ Implemented | S3 XML ↔ GCS BucketLogging |
| **Website** | PUT / GET / DELETE | ✅ Implemented | IndexDocument / ErrorDocument mapping |
| **Tagging** | PUT / GET / DELETE | ✅ Implemented | GCS object metadata with OCC (IfMetagenerationMatch) |
| **ACLs & Policies** | — | ⏸ Deferred | IAM model mismatch, recommend prefix-based security |

### Data Plane — Reverse Proxy & Header Rewriting

All standard S3 object operations streamed through `httputil.ReverseProxy` with automatic header/query transformation.

| Feature | Status | Scope |
|---|---|---|
| **Reverse Proxy (Streaming)** | ✅ Implemented | High-performance streaming with tuned connection pooling |
| **Storage Class Translation** | ✅ Implemented | STANDARD_IA→NEARLINE, GLACIER→ARCHIVE, INTELLIGENT_TIERING→AUTOCLASS, etc. |
| **Versioning Interop** | ✅ Native (GCS) | GCS S3-compatible API handles generation ↔ version-id natively, transparent pass-through |
| **SigV4 Re-signing** | ✅ Implemented | Automatic re-sign on header/query modification |
| **x-id Stripping** | ✅ Implemented | Remove AWS SDK v2 tracking query parameter |
| **DeleteObjects (Bulk)** | ✅ Implemented | GCS XML API natively supports POST /?delete (up to 1000 objects), transparent pass-through |
| **Flexible Checksums** | ⏸ Deferred | Client-side `WHEN_REQUIRED` workaround available |
| **Inventory Manifests** | ⏸ Deferred | Requires external stateful ETL worker |

### Operations Plane — Observability & Reliability

Health probes, metrics, logging infrastructure, and operational safeguards.

| Feature | Status | Scope |
|---|---|---|
| **Health Check** (`/health`) | ✅ Implemented | Lightweight liveness probe |
| **Readiness Probe** (`/readyz`) | ✅ Implemented | GCS connectivity test (Bucket.Attrs) |
| **Prometheus Metrics** (`/metrics`) | ✅ Implemented | http_requests_total, request_duration, gcs_api_duration |
| **Structured JSON Logging** | ✅ Implemented | Go 1.21 `slog` with semantic levels + request_id tracing |
| **Graceful Shutdown** | ✅ Implemented | SIGTERM/SIGINT with 10s drain |
| **DryRun Mode** | ✅ Implemented | Local dev without real GCS hits |

### Three-plane Feature Gating (v1.6+)

Every control-plane resource family, every high-cost data-plane composite
operation (`CopyObject`, Multipart, bulk `DeleteObjects`) and every
operational endpoint (`/health`, `/readyz`, `/metrics`) can be individually
turned off via an `ENABLE_*` env var. Flags default to `true`, so upgrades
preserve behaviour. Rejections return `501 NotImplemented` (for S3
surfaces) or `404` (for ops endpoints) and are counted by
`s3proxy_feature_disabled_rejections_total{feature=...}`. Basic
object CRUD has no flag by design — lock it down at the network / IAM
layer if required. See `README.md § Feature Flags` for the full matrix.

### Quality Assurance & CI/CD

| Layer | Status | Scope |
|---|---|---|
| **Unit Tests** | ✅ Implemented | 17 tests across all `pkg/translate` modules |
| **Integration Tests** | ✅ Implemented | Isolated Go module, auto-spawns local proxy in DryRun |
| **E2E Acceptance Tests** | ✅ Implemented | Functional + Stability + Benchmark suites against live proxy |
| **Multi-SDK Tests** | ✅ Validated | 6 SDKs (Go V2, Go V1, Python, Java V1, Java V2, C++) — 60+ tests, all PASS |
| **CI/CD (GitHub Actions)** | ✅ Implemented | E2E workflow (3 parallel jobs) + Multi-SDK workflow (6 SDK jobs) |

---

## Open Questions & Next Steps

1. ~~**Target SDKs Compatibility**: The target SDKs are **Go, Java, Python, and C++**.~~ **✅ Resolved**: All 6 SDKs (Go V2, Go V1, Python, Java V1, Java V2, C++) validated with 60+ tests passing. See [Per-SDK Required Configuration](#per-sdk-required-configuration) above.
2. **Consistency Requirements**: For features like Tagging via Metadata, is the eventual consistency of GCS metadata acceptable for the customer's application logic?
3. ~~**Performance Hardening**: Consider implementing request body size limits (`MaxBytesReader`), server-level timeouts, and concurrency limiting middleware.~~ **✅ Resolved**: Implemented read/write split Transport (separate GET/HEAD vs PUT/POST/DELETE proxies with optimized buffer sizes), global SigV4 signer reuse, streaming MD5 for DeleteObjects, parameterized connection pool timeouts (IdleConnTimeout, ResponseHeaderTimeout), and concurrency limiter middleware. **K8s deployment must use Guaranteed QoS (requests==limits) + Pod anti-affinity** — Burstable QoS causes 30~60% throughput loss due to CFS throttling on overcommitted nodes.
4. ~~**E2E Validation**: Run the E2E acceptance test suite against a live GKE deployment to validate all translations end-to-end.~~ **✅ Resolved**: E2E tests and Multi-SDK CI pipeline validated on GKE.

---

## Architecture decisions (Cloud Service & Language)

### 1. Cloud Service: Cloud Run vs GKE
To serve high concurrent requests robustly, we evaluate:

| Feature | Cloud Run | GKE (Google Kubernetes Engine) |
| :--- | :--- | :--- |
| **Ops Overhead** | Low (Serverless) | High (Requires cluster management) |
| **Scaling** | Instant, request-based | Metric-based (HPA), can pre-warm |
| **Latency Tails** | Potential cold starts (mitigated by `min-instances`) | Zero cold starts (warm pools) |
| **Connection Pooling** | Harder to tune kernel-level limits | Ultimate control over TCP/IP tuning |
| **Cost at Scale** | Linear with requests | Dense utilization is cheaper |

**Decision**: **GKE Standard** is the production deployment target. GKE Standard provides full control over node machine types, TCP connection pooling, zero cold starts, and Guaranteed QoS (requests == limits) which is critical for proxy workloads.

### 2. Coding Language
For a high-performance, low-latency proxy:

- **Go (Golang)**: **Recommended**. 
  - Standard for cloud infrastructure (Kubernetes, Docker, Envoy are Go/C++).
  - Excellent concurrency primitives (Goroutines) for handling multi-delete fan-outs.
  - Low memory footprint and fast startup.
  - Native GCP and AWS SDK support is top-tier.
- **Rust**: Highest performance, but slower development velocity and steeper learning curve.
- **Java/C#**: Heavy runtimes, GC pauses (bad for sub-100ms proxy latency tails), and higher memory usage.

**Decision**: **Go** is the sweet spot for performance, maintainability, and ecosystem support.
