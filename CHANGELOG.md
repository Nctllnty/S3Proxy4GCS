# Changelog

All notable changes to the s3proxy4gcs project are documented in this file.
Dates are in ISO-8601. Versions follow semver; log/metric label changes are
listed under "Breaking (observability)" so downstream dashboards can adapt.

## v1.4.0 — Correctness & code cleanup (2026-04-18)

### Added
- `InvalidStorageClass` 400 response for any `x-amz-storage-class` value that
  is not one of the documented S3 classes. See the updated list in
  `README.md` / `translateS3StorageClass` in `main.go`.
- Startup fail-fast when `DRY_RUN=false` and
  `PROXY_AWS_ACCESS_KEY_ID` / `PROXY_AWS_SECRET_ACCESS_KEY` are missing.
  Previously the proxy would boot and silently forward unsigned requests,
  producing 100% 403 at GCS.
- `decodeControlPlaneXML` generic helper shared by 5 control-plane handlers
  (`PutLifecycle` / `PutCORS` / `PutLogging` / `PutWebsite` /
  `PutObjectTagging`). Enforces the 64 KB body cap uniformly and detects
  `http.MaxBytesError` via `errors.As` instead of string matching.
- New unit tests in `main_test.go`:
  - `TestWriteS3ErrorEscapesXML` — verifies XML escape safety.
  - `TestTranslateS3StorageClass` — guards against mapping regressions.
  - `TestHandleS3RequestRejectsUnknownStorageClass` — end-to-end check.
  - `TestDecodeControlPlaneXMLRejectsOversizedBody` / `Malformed` /
    `HappyPath` — exhaustive control-plane decoder coverage.

### Changed
- `writeS3Error` now produces XML via `encoding/xml.Encoder` instead of
  `fmt.Fprintf`. Messages with `<`, `&`, `"` etc. are properly escaped.
  Output shape is byte-compatible with the previous format for well-formed
  ASCII messages.
- Unknown S3 storage classes used to silently remap to `NEARLINE`. That
  behaviour is removed per AGENTS rule 4 ("Reject Unsupported").

### Breaking
- Clients that previously relied on undefined `x-amz-storage-class` values
  succeeding will now receive 400. Review client SDKs that may emit
  aliased class names.

## v1.3.0 — Observability enhancement (2026-04-18)

### Added
- Prometheus metrics:
  - `s3proxy_in_flight_requests{endpoint}` — gauge, panic-safe Inc/Dec.
  - `s3proxy_resign_duration_seconds` — histogram, SigV4 re-sign timing.
  - `s3proxy_gcs_errors_total{endpoint|operation, status_class}` —
    unified error counter. `WithMetrics` reports client-visible errors
    (label `endpoint`); `timeGCSCall` reports SDK-layer errors
    (label `operation`). `status_class` ∈
    {`4xx`, `5xx`, `429`, `timeout`, `cancelled`, `network`}.
- `PPROF_ADDR` env controls an optional `net/http/pprof` listener bound
  on a dedicated port (never exposed through the main chi router).
- `k8s/monitoring/prometheus-rules.yaml` — 5 baseline alerts matching
  `docs/SLO.md` thresholds.
- `docs/SLO.md` and `docs/OBSERVABILITY.md` are new canonical references
  for SLIs, SLOs, alert routing, and the full metric / label taxonomy.

### Changed
- `classifyEndpoint` now recognises `lifecycle`, `cors`, `logging`,
  `website`, `tagging`, and `delete_objects` as dedicated endpoints.
  Previously these rolled up into `other`.
- `WithMetrics` wraps observations in a `defer` with `recover()` so a
  panicking downstream handler no longer leaks `s3proxy_in_flight_requests`
  or drops the `HTTPRequestsTotal` increment.

### Breaking (observability)
- Grafana panels filtering on `endpoint="other"` will lose the
  control-plane traffic slice — update queries to use the new labels.
- `s3proxy_gcs_errors_total` is a new counter; alerts that previously
  approximated error rate via `HTTPRequestsTotal{status_code=~"5.."}`
  should be migrated (see `prometheus-rules.yaml`).

## v1.1.0 — Hot-path slimming (2026-04-18)

### Added
- `statusRecorder.Unwrap()` + `Flush()` in `main.go` and
  `pkg/reqlog/reqlog.go`. Restores the intended streaming behaviour of
  `readProxy.FlushInterval = -1`; previously `http.ResponseController`
  could not reach the real `http.Flusher` through the middleware wrapper.
- `handleS3Request` parses `r.URL.Query()` once per request (used to be
  5–7 times via `hasQueryParam`).

### Changed
- Director-hot-path logs downgraded to `Debug` and gated on
  `DEBUG_LOGGING`. Per-request `slog.Info` for storage-class translation,
  x-id stripping, Accept-Encoding stripping, and SigV4 re-signing is gone
  from the default log stream. `Authorization` headers are never logged.
- `modifyResponse` no longer buffers 4xx/5xx response bodies (they used
  to be read in full just to print 500 bytes in a warning), and the
  `?versions` XML dump is guarded on `DEBUG_LOGGING`.
- Chi's default plain-text `middleware.Logger` is removed in favour of
  the structured `observabilityMiddleware` (one JSON line per request).
- `k8s/deployment.yaml` ships with `DEBUG_LOGGING=false` as default.

### Breaking (observability)
- Per-request log count drops from ~6–7 to ~2 lines on data-plane requests
  and the `Authorization` field disappears entirely. Log-based
  dashboards that counted "lines per request" must be updated.

### Tooling
- `integration_tests/lifecycle_test.go` resolves `go` from `PATH` instead
  of hard-coding `/usr/local/go/bin/go`, letting the harness run on
  Homebrew-based macOS dev environments.
