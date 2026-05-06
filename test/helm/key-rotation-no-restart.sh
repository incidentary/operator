#!/usr/bin/env bash
#
# a — verify Helm chart does not bounce pods on API-key Secret rotation.
#
# Plan: sub-task 1.4a
# (D5 revised): Runtime API-key rotation must work without pod restart on
# `replicaCount: 2`.
#
# The runtime path is exercised by Go unit tests in:
#   - internal/controller/incidentaryconfig_controller_unit_test.go
#       (Rotator called with new key on Secret change)
#   - internal/client/provider_test.go
#       (Provider hot-swaps clients atomically; concurrent reads safe)
#
# This shell test pins the *Helm-template* invariant: the rendered
# Deployment must NOT contain any pod-template annotation that would
# trigger a rolling restart when the API-key Secret changes (e.g.
# `checksum/secret: <sha>`). If such an annotation exists, every Secret
# rotation would restart both replicas — defeating the in-process
# hot-swap and turning every key rotation into a brief outage.
#
# How to run:
#   bash test/helm/key-rotation-no-restart.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART_DIR="${REPO_ROOT}/charts/incidentary-operator"

echo "==> Rendering Helm template at replicaCount=2 with apiKey + workspaceId set"

# Render with replicaCount=2 (the GA-supported HA configuration) and a
# real apiKey/workspaceId so the validation guards don't short-circuit
# the render. The values are irrelevant for this test — only the
# rendered Deployment shape matters.
RENDERED="$(helm template incidentary-test \
    "${CHART_DIR}" \
    --namespace incidentary-system \
    --set apiKey="sk_test_phase1_helm_template_only" \
    --set workspaceId="org_test_helm" \
    --set replicaCount=2 \
    --set cluster.name="phase1-helm-template-test")"

echo "==> Asserting rendered Deployment has no Secret-checksum annotation"

# Extract the Deployment kind and look for any annotation key that ends
# with /secret or contains a sha256/checksum-of-secret hash. The shape
# we want to BAN looks like:
#
#   spec:
#     template:
#       metadata:
#         annotations:
#           checksum/secret: 8f4a...
#
# Helm's pattern-of-the-trade is `{{ include (print $.Template.BasePath "/secret.yaml") . | sha256sum }}`
# applied as an annotation. We grep for any such pattern.

if echo "${RENDERED}" | grep -E 'checksum/secret|checksum/apikey|secret-checksum' > /dev/null; then
    echo "FAIL: rendered Deployment contains a Secret-checksum annotation."
    echo "      This would force a rolling restart on every API-key rotation,"
    echo "      defeating the runtime Secret-on-reconcile hot-swap path."
    echo
    echo "Offending lines:"
    echo "${RENDERED}" | grep -nE 'checksum/secret|checksum/apikey|secret-checksum' | sed 's/^/    /'
    exit 1
fi

echo "==> Asserting rendered Deployment has replicaCount=2"

# Verify replicaCount actually flowed through. If replicaCount drops to
# 1, leader handoff testing in becomes meaningless and the
# zero-downtime contract collapses to "best-effort restart."
#
# Slice out the Deployment block (between `kind: Deployment` and the
# next `---` document separator) and require `replicas: 2` to appear
# in its `spec:` section.

DEPLOY_BLOCK="$(echo "${RENDERED}" | awk '/^kind: Deployment$/{flag=1} flag; flag && /^---$/{flag=0; exit}')"

if [[ -z "${DEPLOY_BLOCK}" ]]; then
    echo "FAIL: no Deployment kind found in rendered chart."
    exit 1
fi

if ! echo "${DEPLOY_BLOCK}" | grep -E '^[[:space:]]+replicas:[[:space:]]+2$' > /dev/null; then
    echo "FAIL: rendered Deployment does not set replicas: 2 when --set replicaCount=2."
    echo
    echo "Deployment block:"
    echo "${DEPLOY_BLOCK}" | sed 's/^/    /'
    exit 1
fi

echo "==> Asserting Secret manifest exists (the operator owns the Secret it watches)"

if ! echo "${RENDERED}" | grep -E '^kind: Secret$' > /dev/null; then
    echo "FAIL: rendered chart does not include a Secret kind."
    echo "      The runtime rotation path watches this Secret; without it,"
    echo "      the controller's Secret-on-reconcile loop has nothing to watch."
    exit 1
fi

echo "PASS: zero-downtime key rotation Helm-template invariants hold."
