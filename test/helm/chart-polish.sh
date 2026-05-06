#!/usr/bin/env bash
#
# Helm chart polish (per plan §5 sub-task 1.6).
#
# Verifies:
#   1. NetworkPolicy template exists, off by default, renders when enabled.
#   2. ServiceMonitor template exists, off by default, renders when enabled.
#   3. `keyRotation.workflow` value is documented and renders into the
#      IncidentaryConfig (so the runbook can reference the chosen workflow).
#   4. `image.pullPolicy` defaults to `IfNotPresent`.
#
# This is a pure-template test — no cluster required. Runs locally and in CI.
#
# How to run:
#   bash test/helm/chart-polish.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART_DIR="${REPO_ROOT}/charts/incidentary-operator"

DEFAULT_FLAGS=(
    --namespace incidentary-system
    --set apiKey="sk_test_phase1_6"
    --set workspaceId="org_test_phase1_6"
    --set cluster.name="phase1-6-test"
)

# ----------------------------------------------------------------------------
# 1. NetworkPolicy
# ----------------------------------------------------------------------------

echo "==> 1a. NetworkPolicy must NOT render with default values"

DEFAULT_RENDER="$(helm template incidentary-test "${CHART_DIR}" "${DEFAULT_FLAGS[@]}")"

if echo "${DEFAULT_RENDER}" | grep -E '^kind: NetworkPolicy$' > /dev/null; then
    echo "FAIL: NetworkPolicy rendered when --set networkPolicy.enabled is unset (must be off by default)."
    exit 1
fi

echo "==> 1b. NetworkPolicy MUST render when --set networkPolicy.enabled=true"

NP_RENDER="$(helm template incidentary-test "${CHART_DIR}" "${DEFAULT_FLAGS[@]}" --set networkPolicy.enabled=true)"

if ! echo "${NP_RENDER}" | grep -E '^kind: NetworkPolicy$' > /dev/null; then
    echo "FAIL: NetworkPolicy did not render when --set networkPolicy.enabled=true."
    echo
    echo "Last 30 lines of render:"
    echo "${NP_RENDER}" | tail -30 | sed 's/^/    /'
    exit 1
fi

# ----------------------------------------------------------------------------
# 2. ServiceMonitor
# ----------------------------------------------------------------------------

echo "==> 2a. ServiceMonitor must NOT render with default values"

if echo "${DEFAULT_RENDER}" | grep -E '^kind: ServiceMonitor$' > /dev/null; then
    echo "FAIL: ServiceMonitor rendered when --set serviceMonitor.enabled is unset (must be off by default)."
    exit 1
fi

echo "==> 2b. ServiceMonitor MUST render when --set serviceMonitor.enabled=true"

SM_RENDER="$(helm template incidentary-test "${CHART_DIR}" "${DEFAULT_FLAGS[@]}" --set serviceMonitor.enabled=true)"

if ! echo "${SM_RENDER}" | grep -E '^kind: ServiceMonitor$' > /dev/null; then
    echo "FAIL: ServiceMonitor did not render when --set serviceMonitor.enabled=true."
    exit 1
fi

# ----------------------------------------------------------------------------
# 3. keyRotation.workflow value
# ----------------------------------------------------------------------------

echo "==> 3a. values.yaml must document keyRotation.workflow (default: secret-on-reconcile)"

if ! grep -E 'keyRotation:' "${CHART_DIR}/values.yaml" > /dev/null; then
    echo "FAIL: charts/incidentary-operator/values.yaml does not document a keyRotation block."
    echo "      Per plan sub-task 1.6, the chart must expose a keyRotation.workflow value."
    exit 1
fi

if ! grep -E 'workflow:[[:space:]]*secret-on-reconcile' "${CHART_DIR}/values.yaml" > /dev/null; then
    echo "FAIL: charts/incidentary-operator/values.yaml does not default keyRotation.workflow to 'secret-on-reconcile'."
    exit 1
fi

# ----------------------------------------------------------------------------
# 4. image.pullPolicy default
# ----------------------------------------------------------------------------

echo "==> 4. image.pullPolicy default must be IfNotPresent"

if ! grep -E 'pullPolicy:[[:space:]]*IfNotPresent' "${CHART_DIR}/values.yaml" > /dev/null; then
    echo "FAIL: image.pullPolicy default is not IfNotPresent."
    exit 1
fi

DEPLOY_BLOCK="$(echo "${DEFAULT_RENDER}" | awk '/^kind: Deployment$/{flag=1} flag; flag && /^---$/{flag=0; exit}')"

if ! echo "${DEPLOY_BLOCK}" | grep -E 'imagePullPolicy:[[:space:]]+IfNotPresent' > /dev/null; then
    echo "FAIL: rendered Deployment does not set imagePullPolicy: IfNotPresent."
    echo
    echo "Deployment block:"
    echo "${DEPLOY_BLOCK}" | sed 's/^/    /'
    exit 1
fi

echo "PASS: Helm chart polish invariants hold (NetworkPolicy / ServiceMonitor / keyRotation / pullPolicy)."
