# Changelog

All notable changes to the s3proxy4gcs project are documented in this file.
Dates are in ISO-8601. Versions follow semver; log/metric label changes are
listed under "Breaking (observability)" so downstream dashboards can adapt.

## v1.6.0 — Three-plane feature flags (2026-04-20)

### Added
- 13 `ENABLE_*` environment variables (all default `true`) controlling
  every externally-visible operation of the proxy. Disabling a control- or
  data-plane flag returns `501 NotImplemented` at dispatch entry — before
  any body parsing, XML decode, GCS call, or reverse-proxy forwarding —
  so the proxy can be locked down to a subset of capabilities without
  relying on network policy or IAM.
  - **Control plane (6)**: `ENABLE_LIFECYCLE`, `ENABLE_CORS`, `ENABLE_LOGGING`,
    `ENABLE_WEBSITE`, `ENABLE_TAGGING`, `ENABLE_RESTORE_OBJECT`. Each flag
    gates PUT / GET / DELETE on its subresource as one unit.
  - **Data plane, composite only (3)**: `ENABLE_COPY_OBJECT` (PUT with
    `x-amz-copy-source`, covers both `CopyObject` and `UploadPartCopy`),
    `ENABLE_MULTIPART_UPLOAD` (any of `?uploads`, `?uploadId`,
    `?partNumber`), `ENABLE_DELETE_OBJECTS` (POST `?delete`). Basic
    object CRUD (Get/Put/Head/Delete/List/ListBuckets) is intentionally
    always-on — turning it off would make the proxy useless and is better
    enforced at the network or IAM layer.
  - **Ops plane (3)**: `ENABLE_HEALTH_ENDPOINT`, `ENABLE_READYZ_ENDPOINT`,
    `ENABLE_METRICS_ENDPOINT`. When off, the endpoint is replaced with a
    404 stub (not left unregistered) so requests do not fall through to
    the S3 catch-all and get classified as bucket reads.
- `s3proxy_feature_disabled_rejections_total{feature}` Prometheus counter
  — one low-cardinality label per feature name, incremented on every
  rejection.
- Startup `Feature flags` info log summarises the state of all 12 flags;
  extra WARN lines for any disabled ops endpoint so operators are not
  surprised by K8s probe / Prometheus scrape failures.
- Unit coverage in `main_test.go`: 6 control-plane gates, 5 data-plane
  composite request permutations, dispatch-priority sanity check (copy
  beats multipart when both match), 3 ops-endpoint 404 stubs, and an
  "allow path is side-effect-free" guard on `ensureFeatureEnabled`. New
  metric smoke test in `pkg/metrics/metrics_test.go`.
- `config.FeatureFlags` struct and `getEnvBool` helper in
  `config/settings.go`. `getEnvBool` rejects invalid boolean strings with
  a startup WARN instead of silently falling back, so typos cannot
  accidentally disable critical features.

### Changed
- `.env.example` extended with a dedicated "Feature Flags" section
  documenting each variable, its default, and the operator-facing impact
  (especially the risk of turning off `/health`, `/readyz`, `/metrics`).
- `AGENTS.md` rule 14: any new operation MUST be wired through
  `ensureFeatureEnabled` / `featureDisabled404` with a corresponding
  `FeatureFlags` field — there are no ungated code paths going forward.

### Notes
- Flags default to `true`, so zero-config upgrades from v1.5 retain every
  behaviour. A `config` unit test injects all-true defaults via `TestMain`
  so the pre-existing suite never trips over zero-value flags.
- `501 NotImplemented` was chosen over `403 AccessDenied` because AWS
  SDKs interpret 403 as an authentication problem and may enter retry /
  credential-refresh loops, whereas 501 is terminal and unambiguous.

## v1.5.0 — S3 compatibility shim: RestoreObject (2026-04-20)

### Added
- Synthetic handler for `POST /<bucket>/<key>?restore` (AWS S3
  `RestoreObject`). GCS objects in every storage class are always directly
  readable, so the proxy returns 200 OK with an empty body instead of
  forwarding to GCS (which would reply 400 InvalidArgument). Legacy clients
  that still call `RestoreObject` in a migration path keep working without
  any code change.
- `RESTORE_SKIP_EXISTENCE_CHECK` environment flag (`Settings.RestoreSkipExistenceCheck`,
  default `false`). When `true`, the handler short-circuits the GCS HEAD
  probe and returns 200 immediately. Useful for latency-sensitive
  workloads that are willing to surface missing keys only on the next GET.
- `restore_object` endpoint label in the Prometheus metrics
  (`classifyEndpoint` in `pkg/metrics/metrics.go`). Separate from `proxy`
  so operators can track how many callers still depend on the shim.
- Non-POST verbs on `?restore` are rejected with `501 NotImplemented`
  rather than silently forwarded, matching AGENTS rule 4
  ("Reject Unsupported").
- New unit tests in `main_test.go`:
  - `TestHandleRestoreObject_HappyPath` — DryRun 200 path.
  - `TestHandleRestoreObject_RejectsNonPOST` — GET/PUT/DELETE → 501.
  - `TestHandleRestoreObject_BodySizeCap` — 64 KB cap enforced with
    `MaxMessageLengthExceeded`.
  - `TestHandleRestoreObject_RequiresBucketAndKey` — refuses
    `/bucket/?restore` at the root.
  - `TestHandleRestoreObject_SkipExistenceCheck` — opt-out bypasses the
    GCS probe.
- New package-level test `pkg/metrics/metrics_test.go` with
  `TestClassifyEndpoint` covering the full low-cardinality label matrix
  including the new `restore_object` case.
- Integration test `integration_tests/restore_object_test.go` with three
  sub-tests: AWS SDK Go V2 happy path on a real GCS object, signed raw
  HTTP assertion of the `NoSuchKey` path, and GET ?restore → 501 rejection.

### Changed
- `observabilityMiddleware` now attaches a `handler=restore` label on
  POST `?restore` requests so the structured access log mirrors the
  Prometheus endpoint label.

### Notes
- Existence probe performs one extra Class B GCS operation
  (`storage.Object.Attrs`) per RestoreObject call. This is negligible for
  a call that is rare by nature; operators concerned about cost can set
  `RESTORE_SKIP_EXISTENCE_CHECK=true`.
- **Cost reminder**: GCS does not charge a restore/thaw fee, but every
  `GetObject` on a `NEARLINE` / `COLDLINE` / `ARCHIVE` tier object
  incurs a retrieval charge. Making `RestoreObject` a no-op does not
  exempt callers from that charge; review whether "cold" data is being
  read frequently enough to warrant an `Autoclass` or `STANDARD` tier.

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
