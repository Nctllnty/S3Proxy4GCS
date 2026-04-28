# Autonomous Agent Context (AGENTS.md)

This file contains instructions and context for AI coding assistants working on the `s3proxy4gcs` repository.

## Project Vision

The goal is to serve as a transparent middleware for S3 protocols to translate unsupported features into GCS APIs seamlessly.

## Engineering Rules

1.  **Zero Tolerance for Syntax Errors**: Before committing or saving, ensure bracket matching and interface compliance is correct.
2.  **Centralized Configuration**: All environment variables and settings must be managed in `config/settings.go`. Use `.env` file for local development.
3.  **Documentation Sync**: Update `README.md` and `AGENTS.md` whenever the project footprint (ports, dependencies, paths) changes.
4.  **Reject Unsupported Filters**: Reject lifecycle rules using unsupported filters (Size, Tags) to prevent accidental over-deletion in GCS (Scope Broadening). **Compatibility shim exception**: an S3 operation MAY be synthesised at the proxy layer (e.g. `RestoreObject` → synthetic `200 OK`) only when the underlying GCS semantics make the call a no-op (objects in every storage class are always live). Document each shim in `README.md` § Compatibility Shims and `solutions.md`.
5.  **Full Scope Search**: Before implement translation, search official AWS S3 SDK for full parameters. Enforce strict type validation and test both valid and invalid fields.
6.  **Full Reverse Proxy**: The proxy handles all traffic by default using standard Go `httputil.NewSingleHostReverseProxy`. For data-plane operations (`GET`/`PUT` objects), ensure streaming behavior is preserved (do not read the entire body into memory). Tune `http.Transport` connection pools (`MaxIdleConns`, `MaxIdleConnsPerHost`) for high concurrency.
7.  **Context Propagation**: Always use the request's context (`r.Context()`) for outbound GCS API calls (e.g. `bucket.Update()`). If the client aborts, the outbound GCS call automatically cancels to save compute/cost.
8.  **Standard S3 Errors**: Use the `writeS3Error` helper to respond with standard AWS S3 XML error formats. Do not use plain text `http.Error` as SDK clients expect XML.
9.  **Structured JSON Logging**: When logging, use standard Go 1.21's `log/slog` module instead of standard `log.Printf`. Use semantic levels (`Info`, `Error`, `Debug`) and use keyword arguments (e.g., `slog.Info("msg", "key", val)`) to ensure parsed compatibility with Cloud Logging. **Hot-path rule**: anything that fires on every proxied request (Director rewrites, header stripping, re-sign traces, generation→version-id mapping) MUST be `Debug` and guarded by `config.Config.DebugLogging`. `Info` is reserved for per-request summary (entry + completion) and lifecycle events. Never log signed `Authorization` headers.
10. **Guaranteed QoS for K8s**: All K8s deployments must set `requests == limits` (Guaranteed QoS class) for both CPU and memory. Burstable QoS causes CFS throttling under node contention, resulting in throughput loss. Use Pod anti-affinity to separate proxy and client/benchmark Pods onto different nodes.
11. **Multi-Object Delete Support**: Bulk deletion via `DeleteObjects` (`POST /?delete`) is natively supported by GCS's XML API. The proxy automatically strips non-compliant client headers (e.g., `Accept-Encoding: identity`), re-signs the request using HMAC v4, and forwards the payload directly to GCS to process bulk deletes without requiring custom fan-out translation logic.
12. **Observability Hooks**: Any new hot-path cost (XML translation, extra re-sign, new SDK call) MUST be paired with a Prometheus series. Canonical metrics: `s3proxy_http_requests_total`, `s3proxy_http_request_duration_seconds`, `s3proxy_gcs_errors_total{status_class}`, `s3proxy_in_flight_requests`, `s3proxy_resign_duration_seconds`, `s3proxy_gcs_api_duration_seconds`. New endpoint labels MUST be low-cardinality constants (bucket / object names are never allowed as label values). SLO definitions live in `docs/SLO.md` and alerts in `k8s/monitoring/prometheus-rules.yaml` — keep them in sync.
13. **pprof Discipline**: `PPROF_ADDR` is empty by default. When enabled for diagnostics, bind to `127.0.0.1` only and unset it before closing the incident.
14. **Feature Gating (v1.6+)**: Every externally-visible operation MUST be toggleable via a boolean field on `config.FeatureFlags` and guarded at dispatch entry with `ensureFeatureEnabled` (for control-plane / data-plane handlers) or `featureDisabled404` (for operational HTTP endpoints). Defaults MUST be `true` so zero-config upgrades preserve existing behaviour. Disabled operations return `501 NotImplemented` (AWS XML error) for S3 surfaces and `404` for ops endpoints, and increment `s3proxy_feature_disabled_rejections_total{feature=...}` — never rely on 403, which SDKs retry. Basic object CRUD (single-object Get/Put/Head/Delete/List/ListBuckets) has no flag by design; use network policy / IAM to restrict those.
15. **Per-Client HMAC Credential Mapping (v1.7+)**: The proxy MUST re-sign each inbound request with the originating client's own GCS HMAC AK/SK instead of a single proxy-wide key. The AK→SK map is owned by `pkg/credstore` behind an `atomic.Value` (lock-free reads) and populated from `HMAC_CREDENTIALS_FILE` (K8s Secret volume, hot-reloaded via fsnotify) or `HMAC_CREDENTIALS` (inline JSON for dev). `validateClientCredential` in `main.go` extracts the AK from the SigV4 `Authorization` header (or `X-Amz-Credential` for presigned URLs), rejects unknown AKs with `403 InvalidAccessKeyId` and missing credentials with `403 AccessDenied`, then stores the resolved `aws.Credentials` on the request context so the Director can re-sign without a second lookup. The pre-v1.7 `PROXY_AWS_ACCESS_KEY_ID` / `PROXY_AWS_SECRET_ACCESS_KEY` env vars still work (auto-synthesised to a 1-entry map) but emit a migration WARN at startup. Any new credential path MUST ship with: (a) `s3proxy_hmac_credential_lookups_total{result}` labels (`hit|miss|no_auth|disabled`), (b) unit tests for the happy/miss/no-auth/presigned paths, and (c) operator docs at `docs/hmac-credential-mapping-design.md`. Rotation runbook: `scripts/create-client-hmac.sh`.
16. **Multi-Tenant Bucket Parsing**: `TARGET_BUCKET` is **optional**. When empty, the proxy runs in multi-tenant mode — all control-plane handlers (lifecycle / CORS / logging / website / tagging / restore) parse the bucket from the request URL path via `parseBucketFromPath`. Startup warmup probe and `/readyz` active bucket check are skipped (`/readyz` degrades to `client_only`). Setting `TARGET_BUCKET` only acts as a warmup / probe hint; it does not restrict which buckets clients may address.
17. **Per-Suite Test Bucket**: Each test suite owns a dedicated, pre-provisioned GCS bucket hard-coded in its framework file (no `TEST_BUCKET` env). See the `Testing Bucket Inventory` table in `README.md` for the full mapping. To retarget a suite, edit the single constant (`e2eTestBucket`, `integrationBucket`, `sdkTestBucket`, …) in the corresponding framework source.

## Environment Layout

- `main.go`: Entry point for the Chi router setup.
- `config/settings.go`: Parameter load path.
- `pkg/translate/`: Location for XML translation logic (S3 XML ↔ GCS JSON).
- `e2e_tests/`: E2E acceptance tests (functional, stability, benchmark) against live proxy. Bucket hard-coded to `s3proxy-e2e-test`.
- `sdk_tests/`: Multi-SDK compatibility tests (Go V2, Go V1, Python, Java V1, Java V2, C++). Each SDK hard-codes its own `s3proxy-sdk-<lang>` bucket.
- `integration_tests/`: Local integration tests (spawn proxy subprocess, hit real GCS). Bucket hard-coded to `s3proxy-integration`; logging-target bucket `s3proxy-integration-log-target`.
- `.github/workflows/`: CI/CD pipelines (e2e-tests, multi-sdk-tests, benchmark). No `TEST_BUCKET` / `TARGET_BUCKET` Secret is injected anymore — bucket names are compiled into each suite.
- `.env`: Secret bind template. Use `GCS_PREFIX` for test isolation; `TARGET_BUCKET` is optional (empty ⇒ multi-tenant mode).

---

## Workspace Status

The project is currently set up as a standalone Go module (`module s3proxy4gcs`).
For testing locally without breaking user paths, you can build locally with standard Go runtimes.
