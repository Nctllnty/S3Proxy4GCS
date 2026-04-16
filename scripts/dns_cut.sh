#!/usr/bin/env bash
# DNS cut helper for the multi-sdk-dns-cutting-tests workflow.
#
# Switches the single A record (internal ALB) for ${DNS_NAME} to a CNAME ->
# ${CUTOVER_TARGET}. Cloud DNS does not allow in-place A<->CNAME change, so
# the cut is performed as a single atomic transaction:
#   - remove the current A record (whatever its rrdata / ttl is)
#   - add a CNAME to ${CUTOVER_TARGET}
# After execute, we poll the returned Cloud DNS change ID until status=done,
# then sleep a fixed 30s for client-side resolver cache settling.
#
# Rollback mode does the reverse: remove CNAME, add A -> ${BASELINE_IP}.
#
# Required env:
#   DNS_PROJECT       e.g. cbs-poctest
#   DNS_ZONE          Cloud DNS managed zone resource name (NOT the FQDN)
#   DNS_NAME          FQDN with trailing dot, e.g. s3proxy.lb.local.
#   MODE              "cutover" | "rollback" | "describe"
#   CUTOVER_TARGET    required for cutover (default storage.googleapis.com.)
#   BASELINE_IP       required for rollback (internal ALB IP)
#   TTL               default 60 (seconds)
#   POST_CHANGE_SLEEP default 30  (seconds; fixed client-cache settle)
#
# Stdout emits parseable lines the workflow captures:
#   DNS_EVENT=start mode=<mode> at=<utcISO> at_cst=<gmt+8 ISO> name=<fqdn>
#   DNS_EVENT=change_id id=<id>
#   DNS_EVENT=complete mode=<mode> at=<utcISO> at_cst=<gmt+8 ISO> elapsed_sec=<n>
set -euo pipefail

: "${DNS_PROJECT:?DNS_PROJECT required}"
: "${DNS_ZONE:?DNS_ZONE required}"
: "${DNS_NAME:?DNS_NAME required (must end with '.')}"
: "${MODE:?MODE must be cutover|rollback|describe}"

CUTOVER_TARGET="${CUTOVER_TARGET:-storage.googleapis.com.}"
TTL="${TTL:-60}"
POST_CHANGE_SLEEP="${POST_CHANGE_SLEEP:-30}"

log() { printf '[dns_cut] %s\n' "$*" >&2; }

ensure_dot() {
  case "$1" in
    *.) printf '%s' "$1" ;;
    *)  printf '%s.' "$1" ;;
  esac
}

utc_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
cst_now() { TZ=Asia/Shanghai date +%Y-%m-%dT%H:%M:%S%z; }

DNS_NAME="$(ensure_dot "$DNS_NAME")"
CUTOVER_TARGET="$(ensure_dot "$CUTOVER_TARGET")"

describe_current_json() {
  gcloud dns record-sets list \
    --project="$DNS_PROJECT" --zone="$DNS_ZONE" \
    --name="$DNS_NAME" --format="json"
}

if [[ "$MODE" == "describe" ]]; then
  describe_current_json
  exit 0
fi

# Parse current record sets into TSV: type<TAB>space-separated-rrdata<TAB>ttl
current_rrsets_tsv() {
  describe_current_json | python3 -c '
import json, sys
for r in json.load(sys.stdin):
    print(f"{r.get(\"type\",\"\")}\t{\" \".join(r.get(\"rrdatas\",[]))}\t{r.get(\"ttl\",\"\")}")
'
}

TX_FILE="$(mktemp --suffix=.yaml)"
trap 'rm -f "$TX_FILE"' EXIT

log "Current state of $DNS_NAME in zone $DNS_ZONE:"
current_rrsets_tsv >&2 || true

log "Starting transaction..."
gcloud dns record-sets transaction start \
  --project="$DNS_PROJECT" --zone="$DNS_ZONE" --transaction-file="$TX_FILE" >/dev/null

# Remove whatever is currently there (A and/or CNAME).
while IFS=$'\t' read -r rtype rrdata rttl; do
  [[ -z "$rtype" ]] && continue
  log "removing existing $rtype $DNS_NAME -> $rrdata (ttl=$rttl)"
  # shellcheck disable=SC2086
  gcloud dns record-sets transaction remove \
    --project="$DNS_PROJECT" --zone="$DNS_ZONE" --transaction-file="$TX_FILE" \
    --name="$DNS_NAME" --type="$rtype" --ttl="$rttl" \
    -- $rrdata >/dev/null
done < <(current_rrsets_tsv)

case "$MODE" in
  cutover)
    log "adding CNAME $DNS_NAME -> $CUTOVER_TARGET (ttl=$TTL)"
    gcloud dns record-sets transaction add \
      --project="$DNS_PROJECT" --zone="$DNS_ZONE" --transaction-file="$TX_FILE" \
      --name="$DNS_NAME" --type=CNAME --ttl="$TTL" \
      -- "$CUTOVER_TARGET" >/dev/null
    ;;
  rollback)
    : "${BASELINE_IP:?BASELINE_IP required for rollback}"
    log "adding A $DNS_NAME -> $BASELINE_IP (ttl=$TTL)"
    gcloud dns record-sets transaction add \
      --project="$DNS_PROJECT" --zone="$DNS_ZONE" --transaction-file="$TX_FILE" \
      --name="$DNS_NAME" --type=A --ttl="$TTL" \
      -- "$BASELINE_IP" >/dev/null
    ;;
  *)
    log "unknown MODE=$MODE"
    exit 2
    ;;
esac

START_UTC="$(utc_now)"
START_CST="$(cst_now)"
START_EPOCH="$(date +%s)"
echo "DNS_EVENT=start mode=$MODE at=$START_UTC at_cst=$START_CST name=$DNS_NAME"

log "Executing transaction..."
EXEC_OUT="$(gcloud dns record-sets transaction execute \
  --project="$DNS_PROJECT" --zone="$DNS_ZONE" --transaction-file="$TX_FILE" \
  --format="value(id)")"

CHANGE_ID="${EXEC_OUT:-}"
if [[ -n "$CHANGE_ID" ]]; then
  echo "DNS_EVENT=change_id id=$CHANGE_ID"
  log "Polling Cloud DNS change $CHANGE_ID until status=done..."
  # Poll every 2s, up to 2 minutes.
  for i in $(seq 1 60); do
    STATUS="$(gcloud dns record-sets changes describe "$CHANGE_ID" \
      --project="$DNS_PROJECT" --zone="$DNS_ZONE" --format='value(status)' 2>/dev/null || echo "")"
    log "  change $CHANGE_ID status=$STATUS (poll $i/60)"
    if [[ "$STATUS" == "done" ]]; then
      break
    fi
    sleep 2
  done
  if [[ "$STATUS" != "done" ]]; then
    echo "::warning::Cloud DNS change $CHANGE_ID did not reach status=done within poll window; proceeding anyway" >&2
  fi
else
  log "WARN: could not capture Cloud DNS change id from execute output; continuing without polling"
fi

log "Sleeping ${POST_CHANGE_SLEEP}s for client-side resolver cache settle..."
sleep "$POST_CHANGE_SLEEP"

END_UTC="$(utc_now)"
END_CST="$(cst_now)"
END_EPOCH="$(date +%s)"
ELAPSED=$(( END_EPOCH - START_EPOCH ))
echo "DNS_EVENT=complete mode=$MODE at=$END_UTC at_cst=$END_CST elapsed_sec=$ELAPSED"

log "Post-change state of $DNS_NAME:"
current_rrsets_tsv >&2 || true
