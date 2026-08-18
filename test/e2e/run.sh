#!/usr/bin/env bash
#
# End-to-end test: a real kind cluster, a real SeaweedFS deployment, and real S3
# traffic through the operator-issued credentials.
#
# Unlike the envtest suite — which checks Kubernetes reconciliation against a
# fake storage backend — this exercises the half that only a running SeaweedFS
# can prove: that the generated flags actually start the servers, that the S3
# gateway picks up the IAM configuration the operator wrote to the filer, and
# that credentials minted into a Secret really authenticate.
#
# Usage: ./test/e2e/run.sh [--keep]
#   --keep   leave the kind cluster running afterwards for debugging

set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER:-seaweedfs-operator}"
IMG="${IMG:-ghcr.io/openeverest/seaweedfs-operator:e2e}"
NS=seaweedfs-e2e
OPERATOR_NS=seaweedfs-system
S3_ENDPOINT="http://127.0.0.1:30333"
KEEP=0

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

log()  { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
pass() { printf '\033[1;32m  PASS\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m  FAIL\033[0m %s\n' "$*" >&2; FAILURES=$((FAILURES + 1)); }
FAILURES=0

cleanup() {
  local status=$?
  if [[ $status -ne 0 || $FAILURES -ne 0 ]]; then
    log "Diagnostics"
    kubectl -n "$NS" get objectstoragecluster,objectstoragebucket,objectstorageuser -o wide || true
    kubectl -n "$NS" get pods,pvc,svc || true
    kubectl -n "$NS" describe objectstoragecluster e2e || true
    kubectl -n "$OPERATOR_NS" logs deploy/seaweedfs-operator-controller-manager --tail=200 || true
  fi
  if [[ $KEEP -eq 0 ]]; then
    log "Deleting kind cluster $CLUSTER_NAME"
    kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
  else
    log "Leaving kind cluster $CLUSTER_NAME running (--keep)"
  fi
}
trap cleanup EXIT

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "required tool not found: $1" >&2; exit 1; }
}
require kind
require kubectl
require docker

# ---------------------------------------------------------------------------

log "Creating kind cluster $CLUSTER_NAME"
if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  kind create cluster --name "$CLUSTER_NAME" --config test/e2e/kind-config.yaml
fi
kubectl config use-context "kind-${CLUSTER_NAME}"

log "Building and loading the operator image"
docker build -t "$IMG" .
kind load docker-image "$IMG" --name "$CLUSTER_NAME"

log "Installing CRDs and the operator"
kubectl apply -f config/crd
kubectl apply -f config/manager/namespace.yaml
kubectl apply -f config/rbac
sed "s|IMAGE_PLACEHOLDER|${IMG}|g" config/manager/manager.yaml | kubectl apply -f -
kubectl -n "$OPERATOR_NS" rollout status deploy/seaweedfs-operator-controller-manager --timeout=180s

log "Creating the ObjectStorageCluster"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f test/e2e/cluster.yaml

log "Waiting for the cluster to become Available"
# Every SeaweedFS pod has to pull its image and form a quorum, so the budget is
# generous; the loop reports progress so a stall is obvious rather than silent.
deadline=$((SECONDS + 600))
until kubectl -n "$NS" get objectstoragecluster e2e \
        -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null | grep -q True; do
  if (( SECONDS > deadline )); then
    fail "cluster did not become Available within 10 minutes"
    exit 1
  fi
  printf '.'
  sleep 5
done
echo
pass "cluster reports Available"

log "Waiting for the S3 endpoint to accept the operator's admin credentials"
deadline=$((SECONDS + 300))
until kubectl -n "$NS" get objectstoragecluster e2e \
        -o jsonpath='{.status.conditions[?(@.type=="S3Ready")].status}' 2>/dev/null | grep -q True; do
  if (( SECONDS > deadline )); then
    fail "S3Ready never became true"
    exit 1
  fi
  printf '.'
  sleep 5
done
echo
pass "S3 endpoint authenticated"

# --- status assertions -----------------------------------------------------

log "Checking reported status"

phase=$(kubectl -n "$NS" get objectstoragecluster e2e -o jsonpath='{.status.phase}')
[[ "$phase" == "Running" ]] && pass "phase is Running" || fail "phase is $phase, expected Running"

version=$(kubectl -n "$NS" get objectstoragecluster e2e -o jsonpath='{.status.currentVersion}')
[[ "$version" == "3.80" ]] && pass "currentVersion is $version" || fail "currentVersion is $version"

capacity=$(kubectl -n "$NS" get objectstoragecluster e2e -o jsonpath='{.status.provisionedCapacity}')
[[ "$capacity" == "4Gi" ]] && pass "provisionedCapacity is $capacity (2 x 2Gi)" \
  || fail "provisionedCapacity is $capacity, expected 4Gi"

# The topology comes from the live SeaweedFS master, not from the Kubernetes
# objects, so this is a real check that the volume servers registered.
servers=$(kubectl -n "$NS" get objectstoragecluster e2e -o jsonpath='{.status.topology.volumeServers}')
[[ "${servers:-0}" -ge 2 ]] && pass "master sees $servers volume servers" \
  || fail "master sees $servers volume servers, expected 2"

# --- bucket and user lifecycle ---------------------------------------------

log "Creating a bucket and a scoped S3 user"
kubectl apply -f test/e2e/workload.yaml

kubectl -n "$NS" wait --for=condition=Ready objectstoragebucket/e2e-bucket --timeout=120s
pass "bucket reports Ready"
kubectl -n "$NS" wait --for=condition=Ready objectstorageuser/e2e-user --timeout=120s
pass "user reports Ready"

ACCESS_KEY=$(kubectl -n "$NS" get secret e2e-user-s3-credentials -o jsonpath='{.data.accessKeyID}' | base64 -d)
SECRET_KEY=$(kubectl -n "$NS" get secret e2e-user-s3-credentials -o jsonpath='{.data.secretAccessKey}' | base64 -d)
[[ -n "$ACCESS_KEY" && -n "$SECRET_KEY" ]] && pass "credentials issued into a Secret" \
  || { fail "credentials Secret is incomplete"; exit 1; }

# The connection Secret must carry the bucket details and nothing sensitive.
if kubectl -n "$NS" get secret e2e-bucket-connection -o jsonpath='{.data}' | grep -q secretAccessKey; then
  fail "the bucket connection Secret leaked a secret access key"
else
  pass "bucket connection Secret carries no credentials"
fi

# --- real S3 traffic --------------------------------------------------------

log "Running real S3 operations with the issued credentials"

# Run the S3 client inside the cluster: it exercises the same in-cluster
# endpoint a real workload would use, and needs no host-side AWS CLI.
s3() {
  kubectl -n "$NS" run s3-client-$RANDOM \
    --rm -i --restart=Never --quiet \
    --image=amazon/aws-cli:2.17.0 \
    --env="AWS_ACCESS_KEY_ID=${ACCESS_KEY}" \
    --env="AWS_SECRET_ACCESS_KEY=${SECRET_KEY}" \
    --env="AWS_REGION=us-east-1" \
    --command -- /bin/sh -c "$1"
}

ENDPOINT="http://e2e-s3-client.${NS}.svc.cluster.local:8333"

if s3 "aws --endpoint-url ${ENDPOINT} s3 ls s3://e2e-bucket/" >/dev/null; then
  pass "issued credentials can list the granted bucket"
else
  fail "issued credentials could not list the bucket"
fi

if s3 "echo 'e2e payload' > /tmp/o.txt && aws --endpoint-url ${ENDPOINT} s3 cp /tmp/o.txt s3://e2e-bucket/o.txt" >/dev/null; then
  pass "object upload succeeded"
else
  fail "object upload failed"
fi

if s3 "aws --endpoint-url ${ENDPOINT} s3 cp s3://e2e-bucket/o.txt - | grep -q 'e2e payload'" >/dev/null; then
  pass "object download returned the same bytes"
else
  fail "object download did not match"
fi

# The grant is scoped to e2e-bucket only, so an unrelated bucket must be denied.
# This is the check that proves the IAM configuration is actually enforced
# rather than the gateway running wide open.
if s3 "aws --endpoint-url ${ENDPOINT} s3 mb s3://should-not-be-allowed" >/dev/null 2>&1; then
  fail "a scoped user was able to create an arbitrary bucket"
else
  pass "scoped user is denied outside its grant"
fi

# --- scaling ----------------------------------------------------------------

log "Scaling the volume tier from 2 to 3"
kubectl -n "$NS" patch objectstoragecluster e2e --type=merge -p '{"spec":{"volume":{"replicas":3}}}'
kubectl -n "$NS" rollout status statefulset/e2e-volume --timeout=300s

deadline=$((SECONDS + 180))
until [[ "$(kubectl -n "$NS" get objectstoragecluster e2e -o jsonpath='{.status.provisionedCapacity}')" == "6Gi" ]]; do
  if (( SECONDS > deadline )); then
    fail "provisionedCapacity did not reach 6Gi after scaling"
    break
  fi
  sleep 5
done
[[ "$(kubectl -n "$NS" get objectstoragecluster e2e -o jsonpath='{.status.provisionedCapacity}')" == "6Gi" ]] \
  && pass "capacity grew to 6Gi after adding a replica"

# --- failure recovery -------------------------------------------------------

log "Deleting a volume server pod to check recovery"
kubectl -n "$NS" delete pod e2e-volume-0 --wait=false
sleep 5
kubectl -n "$NS" wait --for=condition=Ready pod/e2e-volume-0 --timeout=300s
pass "deleted volume server pod came back"

# The object must still be readable: it lives on a PVC that survived the pod.
if s3 "aws --endpoint-url ${ENDPOINT} s3 cp s3://e2e-bucket/o.txt - | grep -q 'e2e payload'" >/dev/null; then
  pass "object survived the pod restart"
else
  fail "object was lost across a pod restart"
fi

log "Deleting an operator-owned Service to check drift correction"
kubectl -n "$NS" delete service e2e-s3-client
deadline=$((SECONDS + 180))
until kubectl -n "$NS" get service e2e-s3-client >/dev/null 2>&1; do
  if (( SECONDS > deadline )); then
    fail "the operator did not recreate a deleted Service"
    break
  fi
  sleep 5
done
kubectl -n "$NS" get service e2e-s3-client >/dev/null 2>&1 && pass "deleted Service was recreated"

# --- upgrade ----------------------------------------------------------------

log "Rolling the cluster to a new SeaweedFS version"
kubectl -n "$NS" patch objectstoragecluster e2e --type=merge -p '{"spec":{"version":"3.79"}}'

# The master tier must move first and the volume tier must lag behind it.
sleep 10
master_img=$(kubectl -n "$NS" get statefulset e2e-master -o jsonpath='{.spec.template.spec.containers[0].image}')
[[ "$master_img" == *":3.79" ]] && pass "master tier started the upgrade first" \
  || fail "master image is $master_img"

deadline=$((SECONDS + 600))
until [[ "$(kubectl -n "$NS" get objectstoragecluster e2e -o jsonpath='{.status.currentVersion}')" == "3.79" ]]; do
  if (( SECONDS > deadline )); then
    fail "upgrade did not complete within 10 minutes"
    break
  fi
  printf '.'
  sleep 10
done
echo
[[ "$(kubectl -n "$NS" get objectstoragecluster e2e -o jsonpath='{.status.currentVersion}')" == "3.79" ]] \
  && pass "every tier reached 3.79"

if s3 "aws --endpoint-url ${ENDPOINT} s3 cp s3://e2e-bucket/o.txt - | grep -q 'e2e payload'" >/dev/null; then
  pass "object survived the version upgrade"
else
  fail "object was lost across the upgrade"
fi

# --- deletion ---------------------------------------------------------------

log "Deleting the bucket with deletionPolicy: Delete"
kubectl -n "$NS" delete objectstoragebucket e2e-bucket --timeout=120s
pass "bucket object deleted and finalizer released"

log "Checking that the cluster refuses to delete while a user still references it"
kubectl -n "$NS" delete objectstoragecluster e2e --wait=false
sleep 15
if kubectl -n "$NS" get objectstoragecluster e2e >/dev/null 2>&1; then
  pass "cluster deletion is blocked by the remaining ObjectStorageUser"
else
  fail "cluster was deleted while a dependent object still existed"
fi

kubectl -n "$NS" delete objectstorageuser e2e-user --timeout=120s
deadline=$((SECONDS + 240))
until ! kubectl -n "$NS" get objectstoragecluster e2e >/dev/null 2>&1; do
  if (( SECONDS > deadline )); then
    fail "cluster finalizer was never released"
    break
  fi
  sleep 5
done
kubectl -n "$NS" get objectstoragecluster e2e >/dev/null 2>&1 || pass "cluster deleted cleanly"

# PVCs are retained by design so a mistaken delete cannot destroy the data.
remaining=$(kubectl -n "$NS" get pvc -o name 2>/dev/null | wc -l | tr -d ' ')
[[ "$remaining" -gt 0 ]] && pass "PVCs retained after cluster deletion ($remaining remaining)" \
  || fail "PVCs were deleted with the cluster; they should be retained"

# ---------------------------------------------------------------------------

log "Summary"
if [[ $FAILURES -eq 0 ]]; then
  printf '\033[1;32mAll end-to-end checks passed.\033[0m\n'
else
  printf '\033[1;31m%d end-to-end check(s) failed.\033[0m\n' "$FAILURES"
  exit 1
fi
