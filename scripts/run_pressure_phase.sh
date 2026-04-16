#!/usr/bin/env bash
# Run one phase (A or C) of the DNS-cutting pressure test.
#
# Launches two sequential K8s Jobs — first PUT, then GET — using the Go V2
# pressure image. Each Job runs for ${DURATION} with ${CONCURRENCY} workers.
# Pod logs are captured to results/phase-${PHASE}/go-v2-<op>.log and the
# JSON block between PRESSURE_JSON_START/END markers is extracted to
# results/phase-${PHASE}/go-v2-<op>.json.
#
# Env (set by the workflow):
#   PHASE          "A" | "C"
#   IMAGE          fully-qualified pressure image ref
#   RUN_PREFIX     e.g. "dns-cut-${github.run_id}"
#   DURATION       Go duration, e.g. "2m"
#   CONCURRENCY    e.g. "2"
#   PAYLOAD_BYTES  e.g. "1024"
#
# Assumes kubectl is already authed against the target cluster.
set -euo pipefail

: "${PHASE:?PHASE required}"
: "${IMAGE:?IMAGE required}"
: "${RUN_PREFIX:?RUN_PREFIX required}"
: "${DURATION:?DURATION required}"
: "${CONCURRENCY:?CONCURRENCY required}"
: "${PAYLOAD_BYTES:?PAYLOAD_BYTES required}"

NAMESPACE="${K8S_NAMESPACE:-s3proxy-e2e}"
TEMPLATE="${TEMPLATE:-k8s/pressure-gov2-job.yaml}"
OUT_DIR="results/phase-${PHASE}"
mkdir -p "$OUT_DIR"

PHASE_LOWER="$(echo "$PHASE" | tr '[:upper:]' '[:lower:]')"

render_manifest() {
  local op="$1" job_name="$2" out_path="$3"
  # All substitutions via sed; the template carries no multi-line injection.
  sed \
    -e "s|__JOB_NAME__|${job_name}|g" \
    -e "s|__IMAGE__|${IMAGE//|/\\|}|g" \
    -e "s|__OP__|${op}|g" \
    -e "s|__PHASE__|${PHASE}|g" \
    -e "s|__DURATION__|${DURATION}|g" \
    -e "s|__CONCURRENCY__|${CONCURRENCY}|g" \
    -e "s|__PAYLOAD_BYTES__|${PAYLOAD_BYTES}|g" \
    -e "s|__TEST_PREFIX__|${RUN_PREFIX}-phase${PHASE_LOWER}/|g" \
    "$TEMPLATE" > "$out_path"
}

wait_for_job() {
  local job_name="$1" timeout="$2"
  # Accept either Complete (classic) or SuccessCriteriaMet (JobSuccessPolicy
  # on K8s 1.35+). `kubectl wait` only knows Complete/Failed, so fall back
  # to polling status.conditions[].type.
  kubectl -n "$NAMESPACE" wait --for=condition=complete "job/${job_name}" --timeout="${timeout}s" 2>/dev/null \
    || kubectl -n "$NAMESPACE" wait --for=condition=failed "job/${job_name}" --timeout=1s 2>/dev/null \
    || true
  kubectl -n "$NAMESPACE" get job "$job_name" \
    -o jsonpath='{range .status.conditions[*]}{.type}{"\n"}{end}' \
    | grep -E '^(Complete|SuccessCriteriaMet|Failed)$' | head -1 || true
}

extract_json() {
  local job_name="$1" out_json="$2" op="$3"
  local tmp_log
  tmp_log="$(mktemp)"
  kubectl -n "$NAMESPACE" logs "job/${job_name}" > "$tmp_log" 2>&1 || true
  python3 - "$tmp_log" "$out_json" "$op" "$PHASE" <<'PY'
import json, pathlib, sys
log_path, out_path, op, phase = sys.argv[1:]
text = pathlib.Path(log_path).read_text(errors="replace")
s = text.find("PRESSURE_JSON_START")
e = text.find("PRESSURE_JSON_END")
if s >= 0 and e > s:
    body = text[s + len("PRESSURE_JSON_START"):e].strip()
    try:
        obj = json.loads(body)
        pathlib.Path(out_path).write_text(json.dumps(obj, indent=2))
        print(f"OK: {out_path}")
        sys.exit(0)
    except Exception as ex:
        print(f"WARN: json parse failed: {ex}", file=sys.stderr)
pathlib.Path(out_path).write_text(json.dumps({
    "sdk": "go-v2", "op": op.upper(), "phase": phase,
    "duration_sec": 0, "total_requests": 0, "success": 0, "error": 0,
    "error_rate": 0.0, "throughput_rps": 0.0,
    "latency_ms": {"p50":0,"p95":0,"p99":0,"max":0,"avg":0},
    "concurrency": 0, "payload_bytes": 0,
    "started_at": "", "ended_at": "", "endpoint": "",
    "_note": "no JSON block found in pod logs; see .log artifact"
}, indent=2))
print(f"WARN: wrote stub to {out_path}", file=sys.stderr)
PY
  rm -f "$tmp_log"
}

run_one() {
  local op="$1"
  local op_lower
  op_lower="$(echo "$op" | tr '[:upper:]' '[:lower:]')"

  local job_name="pressure-gov2-${op_lower}-${PHASE_LOWER}-${GITHUB_RUN_ID:-local}"
  job_name="${job_name:0:52}"

  echo "=================================================================="
  echo "[Phase ${PHASE}] op=${op} job=${job_name} image=${IMAGE}"
  echo "=================================================================="

  kubectl -n "$NAMESPACE" delete job "$job_name" --ignore-not-found

  local manifest
  manifest="$(mktemp --suffix=.yaml)"
  render_manifest "$op_lower" "$job_name" "$manifest"
  cat "$manifest"
  kubectl apply -f "$manifest"
  rm -f "$manifest"

  # Allow duration + 5 min runway.
  local dur_sec
  dur_sec="$(python3 -c "import re,sys;s='${DURATION}';m=re.fullmatch(r'(\\d+)([smh])',s);u={'s':1,'m':60,'h':3600};print(int(m.group(1))*u[m.group(2)] if m else 300)")"
  local deadline=$(( dur_sec + 300 ))
  echo "Waiting up to ${deadline}s for job/${job_name}..."
  local status
  status="$(wait_for_job "$job_name" "$deadline")"
  echo "Job status: ${status:-<unknown>}"

  local log_out="${OUT_DIR}/go-v2-${op_lower}.log"
  local json_out="${OUT_DIR}/go-v2-${op_lower}.json"
  kubectl -n "$NAMESPACE" logs "job/${job_name}" > "$log_out" 2>&1 || true
  extract_json "$job_name" "$json_out" "$op_lower"

  kubectl -n "$NAMESPACE" delete job "$job_name" --ignore-not-found || true

  if [[ "$status" != "Complete" && "$status" != "SuccessCriteriaMet" ]]; then
    echo "::warning::Pressure job ${job_name} did not finish cleanly (status=${status}); continuing to report stage"
  fi
}

# Sequential PUT -> GET per phase; keeps the proxy under a predictable
# single-op load profile at any moment.
run_one PUT
run_one GET

echo
echo "=== Phase ${PHASE} artifacts ==="
ls -la "$OUT_DIR"
