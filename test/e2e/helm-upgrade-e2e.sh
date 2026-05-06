#!/usr/bin/env bash
#
# Helm upgrade test on `replicaCount: 2`: zero downtime.
#
# Plan: sub-task 1.19.
# Acceptance: "Helm upgrade test on replicaCount: 2: zero downtime; in CI."
#
# How this test verifies "zero downtime":
#   1. Install the chart at replicaCount=2; wait for both pods Ready.
#   2. Trigger a no-op helm upgrade (touches the pod template via a
#      benign label change so RollingUpdate strategy actually runs).
#   3. Poll the Deployment's `.status.readyReplicas` every 200 ms for the
#      duration of the upgrade.
#   4. Assert: at no observation point does readyReplicas drop below 1.
#   5. Assert: the upgrade completes successfully (`helm upgrade --wait`
#      exits 0).
#   6. Assert: post-upgrade, both replicas are Ready.
#
# This is the operational definition of "zero downtime" for an HA controller-
# manager pair: at any moment during the rolling upgrade, at least one
# replica is serving (= leader election can elect a leader; watchers stay
# warm).
#
# Requirements on the host:
#   - docker (daemon running)
#   - kind 0.20+
#   - helm 3.x
#   - kubectl
#
# Environment variables:
#   CLUSTER_NAME    default: incidentary-operator-upgrade-e2e
#   IMAGE           default: ghcr.io/incidentary/operator:e2e
#   KEEP_CLUSTER    if "1", do not delete the kind cluster on exit
#
# Exit codes:
#   0 on success, 1 on any assertion failure.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART_DIR="${REPO_ROOT}/charts/incidentary-operator"

CLUSTER_NAME="${CLUSTER_NAME:-incidentary-operator-upgrade-e2e}"
IMAGE="${IMAGE:-ghcr.io/incidentary/operator:e2e}"
NAMESPACE="incidentary-system"
RELEASE="incidentary-upgrade-test"

cleanup() {
    local rc=$?
    if [[ "${KEEP_CLUSTER:-0}" != "1" ]]; then
        echo "==> Deleting kind cluster ${CLUSTER_NAME}"
        kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
    else
        echo "==> KEEP_CLUSTER=1 — kind cluster ${CLUSTER_NAME} retained for inspection"
    fi
    exit "${rc}"
}
trap cleanup EXIT

# ----------------------------------------------------------------------------
# 0. Pre-flight
# ----------------------------------------------------------------------------

for cmd in docker kind helm kubectl; do
    if ! command -v "${cmd}" >/dev/null 2>&1; then
        echo "FAIL: required command not found: ${cmd}"
        exit 1
    fi
done

# ----------------------------------------------------------------------------
# 1. kind cluster
# ----------------------------------------------------------------------------

echo "==> Creating kind cluster ${CLUSTER_NAME}"
kind create cluster --name "${CLUSTER_NAME}" --wait 60s

KCONFIG="$(mktemp)"
kind get kubeconfig --name "${CLUSTER_NAME}" > "${KCONFIG}"
export KUBECONFIG="${KCONFIG}"

# ----------------------------------------------------------------------------
# 2. Build + load operator image
# ----------------------------------------------------------------------------

echo "==> Building operator image ${IMAGE}"
docker build -t "${IMAGE}" "${REPO_ROOT}"

echo "==> Loading image into kind cluster"
kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"

# ----------------------------------------------------------------------------
# 3. helm install at replicaCount=2
# ----------------------------------------------------------------------------

echo "==> Helm install at replicaCount=2"
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

helm install "${RELEASE}" "${CHART_DIR}" \
    --namespace "${NAMESPACE}" \
    --set apiKey="sk_test_phase1_19" \
    --set workspaceId="org_test_phase1_19" \
    --set replicaCount=2 \
    --set image.repository="${IMAGE%:*}" \
    --set image.tag="${IMAGE##*:}" \
    --set image.pullPolicy=IfNotPresent \
    --set cluster.name="phase1-19-upgrade-test" \
    --wait \
    --timeout 5m

DEPLOY=$(kubectl -n "${NAMESPACE}" get deploy -l app.kubernetes.io/name=incidentary-operator -o jsonpath='{.items[0].metadata.name}')

echo "==> Waiting for both replicas Ready"
kubectl -n "${NAMESPACE}" rollout status deploy/"${DEPLOY}" --timeout=2m

READY_BEFORE=$(kubectl -n "${NAMESPACE}" get deploy "${DEPLOY}" -o jsonpath='{.status.readyReplicas}')
if [[ "${READY_BEFORE}" != "2" ]]; then
    echo "FAIL: expected 2 ready replicas before upgrade; got ${READY_BEFORE}"
    exit 1
fi

# ----------------------------------------------------------------------------
# 4. Trigger upgrade + poll readyReplicas in background
# ----------------------------------------------------------------------------

echo "==> Triggering helm upgrade with a benign pod-template change"

POLL_LOG="$(mktemp)"

(
    while :; do
        ts=$(date +%s)
        ready=$(kubectl -n "${NAMESPACE}" get deploy "${DEPLOY}" \
            -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
        echo "${ts} ${ready:-0}" >> "${POLL_LOG}"
        sleep 0.2
    done
) &
POLL_PID=$!

helm upgrade "${RELEASE}" "${CHART_DIR}" \
    --namespace "${NAMESPACE}" \
    --reuse-values \
    --set podLabels.upgradeMarker="phase1-19-$(date +%s)" \
    --wait \
    --timeout 5m

UPGRADE_RC=$?

kill "${POLL_PID}" 2>/dev/null || true
wait "${POLL_PID}" 2>/dev/null || true

# ----------------------------------------------------------------------------
# 5. Assertions
# ----------------------------------------------------------------------------

if [[ "${UPGRADE_RC}" -ne 0 ]]; then
    echo "FAIL: helm upgrade exited with ${UPGRADE_RC}"
    exit 1
fi

# Find the minimum readyReplicas observed during the upgrade.
MIN_READY=$(awk '{ if ($2 < min || NR == 1) min = $2 } END { print min }' "${POLL_LOG}")

echo "==> Observed minimum readyReplicas during upgrade: ${MIN_READY}"

if [[ "${MIN_READY}" -lt 1 ]]; then
    echo "FAIL: readyReplicas dropped to ${MIN_READY} during upgrade — zero-downtime broken."
    echo "      Last 20 poll observations:"
    tail -20 "${POLL_LOG}" | sed 's/^/    /'
    exit 1
fi

READY_AFTER=$(kubectl -n "${NAMESPACE}" get deploy "${DEPLOY}" -o jsonpath='{.status.readyReplicas}')
if [[ "${READY_AFTER}" != "2" ]]; then
    echo "FAIL: expected 2 ready replicas after upgrade; got ${READY_AFTER}"
    exit 1
fi

echo "PASS: helm upgrade on replicaCount=2 maintained >=1 Ready replica throughout."
echo "      readyReplicas observations recorded to ${POLL_LOG}"
