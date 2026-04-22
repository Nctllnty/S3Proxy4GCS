#!/usr/bin/env bash
# Provision a per-client GCS HMAC key and register it in the S3Proxy
# credential mapping Secret (`s3proxy-hmac-credentials`, key
# `credentials.json`).
#
# This script is the intended operator-facing entry point for client
# onboarding on S3Proxy v1.7+. It does two things:
#   1. Creates a GCS HMAC key pair bound to a caller-supplied IAM service
#      account (each tenant should have their own SA with least-privilege
#      bucket/object bindings).
#   2. Merges the new AK/SK into the in-cluster Secret so the proxy's
#      fsnotify hot-reload watcher picks it up within ~1s — no pod
#      restart required.
#
# Usage:
#   scripts/create-client-hmac.sh <client-name> [service-account-email]
#
# Environment:
#   GCP_PROJECT_ID    GCP project that owns the service account (required)
#   K8S_NAMESPACE     Namespace hosting the S3Proxy Deployment
#                     (default: s3proxy-e2e)
#   SECRET_NAME       Name of the credential-mapping Secret
#                     (default: s3proxy-hmac-credentials)
#   DRY_RUN           If "true", print the merged JSON but do not touch
#                     GCS or the cluster.
#
# Output:
#   Prints the new access key id + secret to stdout in the shape the
#   tenant's client expects (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY).
#   Keep that output — GCS does not let you retrieve the secret again.

set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "Usage: $0 <client-name> [service-account-email]" >&2
  exit 2
fi

CLIENT_NAME="$1"
SA_EMAIL="${2:-}"
GCP_PROJECT_ID="${GCP_PROJECT_ID:-}"
K8S_NAMESPACE="${K8S_NAMESPACE:-s3proxy-e2e}"
SECRET_NAME="${SECRET_NAME:-s3proxy-hmac-credentials}"
DRY_RUN="${DRY_RUN:-false}"

if [[ -z "${GCP_PROJECT_ID}" ]]; then
  echo "ERROR: GCP_PROJECT_ID is required" >&2
  exit 2
fi

if [[ -z "${SA_EMAIL}" ]]; then
  # Default service account naming convention: <client>-s3proxy@<project>.iam
  SA_EMAIL="${CLIENT_NAME}-s3proxy@${GCP_PROJECT_ID}.iam.gserviceaccount.com"
  echo "No service account given, using convention: ${SA_EMAIL}" >&2
fi

for bin in gcloud kubectl jq; do
  if ! command -v "${bin}" >/dev/null 2>&1; then
    echo "ERROR: required tool '${bin}' is not installed" >&2
    exit 2
  fi
done

echo ">>> Creating GCS HMAC key for service account ${SA_EMAIL}"
if [[ "${DRY_RUN}" == "true" ]]; then
  NEW_AK="GOOG1EDRYRUN$(date +%s)"
  NEW_SK="dry-run-secret-placeholder"
  echo "  (dry-run) would call: gcloud storage hmac create"
else
  CREATE_JSON="$(gcloud storage hmac create "${SA_EMAIL}" \
    --project "${GCP_PROJECT_ID}" --format=json)"
  NEW_AK="$(echo "${CREATE_JSON}" | jq -r '.metadata.accessId')"
  NEW_SK="$(echo "${CREATE_JSON}" | jq -r '.secret')"
  if [[ -z "${NEW_AK}" || "${NEW_AK}" == "null" ]]; then
    echo "ERROR: failed to parse AK from gcloud response" >&2
    echo "${CREATE_JSON}" >&2
    exit 1
  fi
fi

echo ">>> Fetching existing credential map (${K8S_NAMESPACE}/${SECRET_NAME})"
EXISTING="{}"
if kubectl -n "${K8S_NAMESPACE}" get secret "${SECRET_NAME}" \
    >/dev/null 2>&1; then
  EXISTING="$(kubectl -n "${K8S_NAMESPACE}" get secret "${SECRET_NAME}" \
    -o jsonpath='{.data.credentials\.json}' | base64 --decode || echo '{}')"
  if [[ -z "${EXISTING}" ]]; then
    EXISTING="{}"
  fi
fi

# Merge the new AK/SK into the map. Fail loudly if the AK already exists
# so operators never silently overwrite an active tenant's credential.
if echo "${EXISTING}" | jq -e --arg ak "${NEW_AK}" 'has($ak)' >/dev/null; then
  echo "ERROR: AK ${NEW_AK} already present in secret; refusing to overwrite." >&2
  exit 1
fi
MERGED="$(echo "${EXISTING}" | jq --arg ak "${NEW_AK}" --arg sk "${NEW_SK}" \
  '. + {($ak): $sk}')"

echo ">>> Merged credential map preview ($(echo "${MERGED}" | jq 'keys | length') entries):"
echo "${MERGED}" | jq 'with_entries(.value = "***redacted***")'

if [[ "${DRY_RUN}" == "true" ]]; then
  echo ">>> DRY_RUN=true — skipping kubectl apply"
  exit 0
fi

echo ">>> Applying updated Secret"
kubectl -n "${K8S_NAMESPACE}" create secret generic "${SECRET_NAME}" \
  --from-literal=credentials.json="${MERGED}" \
  --dry-run=client -o yaml | kubectl apply -f -

cat <<EOF

------------------------------------------------------------------------
Client credentials for ${CLIENT_NAME} (STORE SECURELY — cannot be re-read):

AWS_ACCESS_KEY_ID=${NEW_AK}
AWS_SECRET_ACCESS_KEY=${NEW_SK}

Endpoint: the S3Proxy load balancer for ${K8S_NAMESPACE}.
------------------------------------------------------------------------

The proxy's fsnotify watcher will pick up the new credential in <1s.
Verify with:
  kubectl -n ${K8S_NAMESPACE} logs deploy/s3proxy | grep 'HMAC credentials hot-reloaded'
EOF
