# Autonomous Agent Context (AGENTS.md)

This file contains instructions and context for AI coding assistants working on the `s3proxy4gcs` repository.

## Project Vision

The goal is to serve as a transparent middleware for S3 protocols to translate unsupported features into GCS APIs seamlessly.

## Engineering Rules

1.  **Zero Tolerance for Syntax Errors**: Before committing or saving, ensure bracket matching and interface compliance is correct.
2.  **Centralized Configuration**: All environment variables and settings must be managed in `config/settings.go`. Use `.env` file for local development.
3.  **Documentation Sync**: Update `README.md` and `AGENTS.md` whenever the project footprint (ports, dependencies, paths) changes.
4.  **Reject Unsupported Filters**: Reject lifecycle rules using unsupported filters (Size, Tags) to prevent accidental over-deletion in GCS (Scope Broadening).
5.  **Full Scope Search**: Before implement translation, search official AWS S3 SDK for full parameters. Enforce strict type validation and test both valid and invalid fields.
6.  **Full Reverse Proxy**: The proxy handles all traffic by default using standard Go `httputil.NewSingleHostReverseProxy`. For data-plane operations (`GET`/`PUT` objects), ensure streaming behavior is preserved (do not read the entire body into memory). Tune `http.Transport` connection pools (`MaxIdleConns`, `MaxIdleConnsPerHost`) for high concurrency.
7.  **Context Propagation**: Always use the request's context (`r.Context()`) for outbound GCS API calls (e.g. `bucket.Update()`). If the client aborts, the outbound GCS call automatically cancels to save compute/cost.
8.  **Standard S3 Errors**: Use the `writeS3Error` helper to respond with standard AWS S3 XML error formats. Do not use plain text `http.Error` as SDK clients expect XML.
9.  **Structured JSON Logging**: When logging, use standard Go 1.21's `log/slog` module instead of standard `log.Printf`. Use semantic levels (`Info`, `Error`, `Debug`) and use keyword arguments (e.g., `slog.Info("msg", "key", val)`) to ensure parsed compatibility with Cloud Logging. **Hot-path rule**: anything that fires on every proxied request (Director rewrites, header stripping, re-sign traces, generation→version-id mapping) MUST be `Debug` and guarded by `config.Config.DebugLogging`. `Info` is reserved for per-request summary (entry + completion) and lifecycle events. Never log signed `Authorization` headers.
10. **Guaranteed QoS for K8s**: All K8s deployments must set `requests == limits` (Guaranteed QoS class) for both CPU and memory. Burstable QoS causes CFS throttling under node contention, resulting in throughput loss. Use Pod anti-affinity to separate proxy and client/benchmark Pods onto different nodes.
11. **Multi-Object Delete Support**: Bulk deletion via `DeleteObjects` (`POST /?delete`) is natively supported by GCS's XML API. The proxy automatically strips non-compliant client headers (e.g., `Accept-Encoding: identity`), re-signs the request using HMAC v4, and forwards the payload directly to GCS to process bulk deletes without requiring custom fan-out translation logic.
12. **Observability Hooks**: Any new hot-path cost (XML translation, extra re-sign, new SDK call) MUST be paired with a Prometheus series. Canonical metrics: `s3proxy_http_requests_total`, `s3proxy_http_request_duration_seconds`, `s3proxy_gcs_errors_total{status_class}`, `s3proxy_in_flight_requests`, `s3proxy_resign_duration_seconds`, `s3proxy_gcs_api_duration_seconds`. New endpoint labels MUST be low-cardinality constants (bucket / object names are never allowed as label values). SLO definitions live in `docs/SLO.md` and alerts in `k8s/monitoring/prometheus-rules.yaml` — keep them in sync.
13. **pprof Discipline**: `PPROF_ADDR` is empty by default. When enabled for diagnostics, bind to `127.0.0.1` only and unset it before closing the incident.

## Environment Layout

- `main.go`: Entry point for the Chi router setup.
- `config/settings.go`: Parameter load path.
- `pkg/translate/`: Location for XML translation logic (S3 XML ↔ GCS JSON).
- `e2e_tests/`: E2E acceptance tests (functional, stability, benchmark) against live proxy.
- `sdk_tests/`: Multi-SDK compatibility tests (Go V2, Go V1, Python, Java V1, Java V2, C++).
- `integration_tests/`: Local integration tests with DryRun proxy.
- `.github/workflows/`: CI/CD pipelines (e2e-tests, multi-sdk-tests, benchmark).
- `.env`: Secret bind template. Use `GCS_PREFIX` for test isolation.

---

## Workspace Status

The project is currently set up as a standalone Go module (`module s3proxy4gcs`).
For testing locally without breaking user paths, you can build locally with standard Go runtimes.
