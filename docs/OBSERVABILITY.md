# Observability Guide

This document lists every metric, log field, and profiling surface the proxy
exposes and explains how to combine them when debugging incidents.

## 1. Logs

The proxy maintains **two independent logging systems** with different
purposes and output sinks.

### 1.1 slog JSON log (stdout) — real-time diagnostics

**Source**: `observabilityMiddleware` in `main.go`.\
**Output**: stdout → automatically collected by GKE into Cloud Logging.\
**Format**: single-line JSON via `log/slog.NewJSONHandler`.

A production pod emits exactly **two Info lines per data-plane request**:

1. `Received S3 Request` — fired by `handleS3Request` (entry point).
2. `HTTP request completed` — fired by `observabilityMiddleware` (exit).

**Fields in `HTTP request completed`**:

| Field | Type | Always present | Description |
|-------|------|:--------------:|-------------|
| `request_id` | string | yes | Chi-generated unique ID for the request |
| `source_ip` | string | yes | Client IP from `X-Forwarded-For`, `X-Real-Ip`, or `RemoteAddr` |
| `method` | string | yes | HTTP method (`GET`, `PUT`, `DELETE`, `POST`, `HEAD`) |
| `uri` | string | yes | Full request URI including query string |
| `status` | int | yes | HTTP response status code |
| `duration_ms` | int | yes | End-to-end request duration in milliseconds |
| `content_length` | int | yes | Request body `Content-Length` (-1 if unknown) |
| `handler` | string | yes | Handler label: `proxy`, `lifecycle`, `cors`, `logging`, `website`, `tagging`, `restore` |
| `access_key` | string | when present | Client HMAC Access Key ID (omitted for health probes / anonymous requests) |
| `guploader_upload_id` | string | when present | GCS `X-GUploader-UploadID` response header (omitted if GCS did not return one) |

**Example** (data-plane PUT):
```json
{"time":"2026-04-22T09:31:39.251Z","level":"INFO","msg":"HTTP request completed",
 "request_id":"s3proxy-d4dc7f474-k8sbm/pfIvDaoMdo-000033","source_ip":"10.60.0.15",
 "method":"PUT","uri":"/bucket/key?x-id=PutObject","status":200,"duration_ms":42,
 "content_length":1024,"handler":"proxy",
 "access_key":"GOOG1EIR44CN63...","guploader_upload_id":"ADPycdv..."}
```

**Example** (health probe — no `access_key` / `guploader_upload_id`):
```json
{"time":"2026-04-22T09:30:25.306Z","level":"INFO","msg":"HTTP request completed",
 "request_id":"s3proxy-d4dc7f474-k8sbm/pfIvDaoMdo-000001","source_ip":"10.60.0.1",
 "method":"GET","uri":"/readyz","status":200,"duration_ms":25,
 "content_length":0,"handler":"proxy"}
```

Additional slog lines only appear when:

- `DEBUG_LOGGING=true` — prints request headers, re-sign traces,
  `x-id` / `Accept-Encoding` stripping notices, and virtual-host rewrites.
- The request hits a control-plane handler — `Read control-plane request body`,
  `GCS API call succeeded`, `Successfully updated GCS bucket CORS`, etc.
- The proxy receives a 4xx/5xx response from GCS (WARN level, no body read).

**Production rule**: keep `DEBUG_LOGGING=false`. Expect ~2× the incoming
request count in log lines (~200 B per request). At 10 k QPS ≈ 2 MB/s log
volume, sized for a single-shard Cloud Logging sink.

### 1.2 reqlog CSV log (file) — offline analytics & audit

**Source**: `pkg/reqlog/reqlog.go`, registered as `reqlog.Middleware`.\
**Output**: local file `/var/log/s3proxy/req_YYYYMMDD.csv` (configurable via
`REQUEST_LOG_PATH`).\
**Format**: SOH (`\x01`) delimited CSV, one line per request.\
**Write mode**: asynchronous channel buffer (`REQUEST_LOG_CHAN_BUF`, default
4096), non-blocking on hot path.\
**Rotation**: daily by default, with size-based rotation
(`REQUEST_LOG_MAX_SIZE_MB`) and automatic cleanup
(`REQUEST_LOG_KEEP_DAYS`).

**Columns** (fixed order, 10 fields):

| # | Column | Description |
|---|--------|-------------|
| 1 | `TimestampMs` | Request start time (Unix milliseconds) |
| 2 | `RequestID` | Chi-generated request ID |
| 3 | `SourceIP` | Client IP (same logic as slog) |
| 4 | `HTTPMethod` | `GET`, `PUT`, `DELETE`, `POST`, `HEAD` |
| 5 | `APIMethod` | Inferred S3 operation name (`PutObject`, `GetBucketCors`, `DeleteObjects`, …) |
| 6 | `Bucket` | Target bucket name |
| 7 | `AccessKey` | Client HMAC Access Key ID (empty for anonymous) |
| 8 | `GUploaderUploadID` | GCS `X-GUploader-UploadID` response header |
| 9 | `StatusCode` | HTTP response status code |
| 10 | `DurationMs` | End-to-end duration in milliseconds |

**Typical use cases**:
- Per-AccessKey traffic attribution and billing breakdown.
- Per-Bucket QPS / error-rate aggregation.
- Compliance audit log retention (configurable `REQUEST_LOG_KEEP_DAYS`).
- Cross-reference with GCS-side logs via `GUploaderUploadID`.

### 1.3 Comparison

| | slog JSON (stdout) | reqlog CSV (file) |
|---|---|---|
| **Sink** | stdout → Cloud Logging | Local file, daily rotation |
| **Write** | Synchronous | Async channel |
| **Content** | Request logs + business logs + WARN/ERROR | Request-level summary only (1 line per request) |
| **Unique fields** | `handler`, `content_length` | `api_method`, `bucket` |
| **Consumer** | SRE real-time monitoring, Cloud Logging queries | Data analytics, traffic attribution, audit |

### 1.4 Correlating with a client

Every request carries a `request_id` (propagated from the client via
`X-Request-Id` or generated by chi). The same ID appears in:

- Every slog JSON line for that request.
- The reqlog CSV record for that request.
- `Authorization` signed headers (via SigV4) — not logged.

## 2. Prometheus metrics (/metrics)

### Request counters
- `s3proxy_http_requests_total{method, endpoint, status_code}` — all
  requests processed, including `/health`, but `/metrics` itself is scraped
  outside the middleware chain.
- `s3proxy_http_request_duration_seconds_bucket{method, endpoint}` —
  latency histogram with default Prometheus buckets.
- `s3proxy_bytes_received_total{method, endpoint}` /
  `s3proxy_bytes_sent_total{method, endpoint}` — raw throughput counters.

### Error classification (v1.3+)
- `s3proxy_gcs_errors_total{endpoint, status_class}` — counts client-observed
  errors emitted by the WithMetrics middleware. `status_class` is one of
  `4xx`, `5xx`, `429`.
- `s3proxy_gcs_errors_total{operation, status_class}` — counts SDK-layer
  errors from `timeGCSCall`. `status_class` additionally includes
  `timeout`, `cancelled`, `network`.

> **Note**: the two label sets (`endpoint` vs `operation`) never collide
> because control-plane operations use canonical `PutBucketLifecycle`-style
> operation names, whereas data-plane endpoints use the lowercase
> `get_object`/`put_object` classifier.

### Runtime & capacity (v1.3+)
- `s3proxy_in_flight_requests{endpoint}` — Gauge tracked by the middleware
  (Inc on entry, Dec on return). Watch this versus `MAX_CONCURRENT_REQUESTS`
  to detect long-running streams saturating the throttle.
- `s3proxy_resign_duration_seconds` — Histogram of SigV4 re-sign time.
  Typical healthy values: P50 ~40 µs, P99 ~300 µs on c4d nodes.

### Feature gating (v1.6+)
- `s3proxy_feature_disabled_rejections_total{feature}` — counts requests
  rejected because the targeted operation is turned off via `ENABLE_*`
  environment variables. Expect this to be identically zero in a default
  deployment; a non-zero reading either means operators are intentionally
  blocking an S3 API surface, or a misconfiguration (invalid boolean
  string) caused a silent downgrade.
  - `feature` label values: `lifecycle`, `cors`, `logging`, `website`,
    `tagging`, `restore_object`, `copy_object`, `multipart_upload`,
    `delete_objects`, `health_endpoint`, `readyz_endpoint`,
    `metrics_endpoint`. All are low-cardinality string constants; no
    user-supplied data ever reaches this label.
  - Operator action on sustained non-zero: grep startup logs for the
    `Feature flags` summary to confirm intent; compare against the
    `ENABLE_*` env vars in the K8s manifest.

### GCS SDK path
- `s3proxy_gcs_api_duration_seconds_bucket{operation}` — per-operation
  latency for `timeGCSCall` invocations (control plane).

### HMAC credential mapping (v1.7+)
Emitted by the per-client AK→SK re-sign path (`pkg/credstore`,
`validateClientCredential` in `main.go`). See
[`hmac-credential-mapping-design.md`](hmac-credential-mapping-design.md)
for the full architecture.

- `s3proxy_hmac_credential_lookups_total{result}` — counts every AK
  lookup on the hot path. Operators use this to detect scanning/probing
  attacks, misconfigured clients, and silent fallback to the legacy
  single-key path. Labels:
  - `hit` — AK matched an entry in the store; request was re-signed
    with the client's own secret. Should dominate on a healthy cluster.
  - `miss` — AK not in the map; request was rejected with
    `403 InvalidAccessKeyId`. Expect a low steady-state baseline from
    old keys after rotation; a sustained spike is the canonical
    "someone is scanning us" signal.
  - `no_auth` — request carried no `Authorization` header and no
    `X-Amz-Credential` query param; rejected with `403 AccessDenied`.
    A small baseline from `/` probes and anonymous curl is normal.
  - `disabled` — the store is empty and the proxy fell through to the
    legacy single-key re-sign path. Should be zero once
    `HMAC_CREDENTIALS{,_FILE}` is configured; a non-zero reading on a
    production pod means the Secret volume is empty or failed to load.
- `s3proxy_hmac_credentials_loaded` — Gauge reporting the number of
  AK→SK entries currently held in memory. Alert on `== 0` to catch
  malformed Secret rollouts before they affect customers.
- `s3proxy_hmac_credentials_reload_total{result}` — counts hot-reload
  attempts triggered by `fsnotify`. `result` is `success` or `error`;
  a non-zero `error` count means the on-disk JSON failed strict
  validation (`pkg/credstore.parse`) and the in-memory snapshot was
  retained. Pair with the `HMAC credentials hot-reload failed` ERROR
  log line to find the offending payload.

### Endpoint label values

| Label | Triggered by |
|-------|--------------|
| `get_object` / `put_object` / `head_object` / `delete_object` | Standard data-plane HTTP verbs with bucket + key |
| `list_objects` | GET /<bucket>/ without key |
| `list` | GET / (service-level) |
| `delete_objects` | POST /?delete |
| `lifecycle` / `cors` / `logging` / `website` / `tagging` | Any HTTP verb on the corresponding subresource |
| `restore_object` | POST /<bucket>/<key>?restore — synthetic RestoreObject handler (v1.5+). Non-POST verbs on `?restore` are short-circuited with 501 and do **not** use this label. |
| `other` | Operational endpoints and unclassified requests |

## 3. pprof profiling (v1.3+)

- Set `PPROF_ADDR=127.0.0.1:6060` to expose `net/http/pprof` on a dedicated
  listener. The handler lives on `http.DefaultServeMux`, isolated from the
  S3 chi router so it cannot be reached via the public LoadBalancer.
- Typical commands:
  ```bash
  kubectl port-forward deploy/s3proxy 6060:6060
  go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30  # CPU
  go tool pprof http://127.0.0.1:6060/debug/pprof/heap                # Heap
  curl http://127.0.0.1:6060/debug/pprof/goroutine?debug=2            # Stacks
  ```
- **Leave `PPROF_ADDR` empty in production manifests**; flip it only when
  debugging an incident, and unset after.

## 4. Distributed tracing

Out of scope for v1.x. The reverse-proxy Director would be a natural hook,
but OpenTelemetry integration is deferred until customer observability
requirements are finalized.

## 5. Alert routing

Alerts defined in `k8s/monitoring/prometheus-rules.yaml` use labels
`severity=critical|warning` and `slo=<name>`. Route via Alertmanager:

```yaml
route:
  routes:
    - match: {slo: data-plane-availability, severity: critical}
      receiver: pager
    - match: {slo: control-plane-availability}
      receiver: slack-s3proxy
    - match: {slo: capacity}
      receiver: slack-s3proxy
```
