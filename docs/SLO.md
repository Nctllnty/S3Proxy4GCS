# S3Proxy4GCS SLOs

> Authoritative source for the service-level objectives the proxy commits to.
> Alert thresholds in `k8s/monitoring/prometheus-rules.yaml` MUST stay in
> sync with this document.

## SLI catalog

| SLI | Definition | Primary metric |
|-----|------------|----------------|
| Data-plane availability | Fraction of data-plane requests that did **not** return 5xx | `s3proxy_gcs_errors_total{endpoint=~"get_object|put_object|..."}` ÷ `s3proxy_http_requests_total` |
| Data-plane latency (P99) | Tail latency for object GET / PUT | `s3proxy_http_request_duration_seconds_bucket{endpoint=~"get_object|put_object"}` |
| Control-plane availability | Success rate of bucket/object configuration translations | `s3proxy_gcs_errors_total{operation=~"Put.*|Get.*|Delete.*"}` ÷ `s3proxy_gcs_api_duration_seconds_count` |
| Signing overhead | Portion of request time spent in SigV4 re-signing | `s3proxy_resign_duration_seconds_sum` |
| Throttle headroom | In-flight requests vs throttle ceiling | `s3proxy_in_flight_requests` |

## SLO targets

| SLO | Window | Target | Alert (warn / crit) |
|-----|--------|--------|---------------------|
| Data-plane availability | 30d rolling | 99.90% | `5xx rate > 0.5%` for 5m (critical) |
| Data-plane P99 latency | 30d rolling | < 1s for `get_object` / `put_object` | `P99 > 1s` for 10m (warn) |
| Control-plane availability | 30d rolling | 99.95% | Any `5xx` / `timeout` / `network` for 5m (warn) |
| Throttle saturation | instant | `< 80%` of `MAX_CONCURRENT_REQUESTS` | `> 80%` for 2m (warn) |
| Proxy uptime | n/a | `up == 1` | `up == 0` for 2m (critical) |

## Error budget accounting

- 0.10% monthly error budget on the data plane ≈ **43.8 min** of full outage
  equivalent per month. Every minute with ≥ 50% error rate consumes one unit.
- When 50% of the budget is burned in less than 30% of the window,
  **freeze non-essential deployments** (opt-in lifecycle / translation
  feature changes) and prioritize the runbook linked in the alert.

## Dashboards

- Grafana: `k8s/monitoring/s3proxy-dashboard.yaml` (ConfigMap, auto-discovered
  by the Grafana operator via `grafana_dashboard=1`). Data-plane rates,
  latency histograms, and error counters are on the same board.
- In-flight + signing overhead panels are added alongside in v1.3.
- Pod CPU / memory panels depend on `kube-state-metrics` +
  `container_cpu_usage_seconds_total` from `cadvisor`.

## Related runbooks

- `S3Proxy5xxRateHigh` — verify GCS regional status, proxy HMAC credentials,
  recent config changes.
- `S3ProxyP99Slow` — inspect `s3proxy_resign_duration_seconds_bucket` and
  `s3proxy_gcs_api_duration_seconds_bucket` to attribute tail latency.
- `S3ProxyControlPlaneGCSErrors` — usually indicates HMAC / IAM drift,
  confirm service account bindings.
- `S3ProxyThrottleSaturated` — scale replicas or revisit
  `MAX_CONCURRENT_REQUESTS`.
