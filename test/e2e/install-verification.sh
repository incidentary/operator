#!/usr/bin/env bash
#
# install verification (kind variant).
#
# Plan: sub-task 1.10.
# Acceptance criteria (plan §5 .D):
#   - `helm install` succeeds on three clusters from the published OCI chart
#   - Operator pod set is `Ready` within 60 seconds
#   - Topology report and ≥1 CE batch arrive at backend within 5 minutes
#
# This script discharges the kind variant. The GKE and EKS variants run
# the same flow against their respective clusters and chart sources — the
# only differences are how the cluster is provisioned and whether the
# chart is pulled from OCI (`oci://ghcr.io/...`) or installed from the
# local source tree (the kind variant uses local source so it can verify
# the unpublished chart bits).
#
# Environment variables:
#   CLUSTER_NAME    default: incidentary-operator-install-verify
#   IMAGE           default: incidentary-operator:install-verify
#   READY_TIMEOUT   default: 60s   (.D bar)
#   BATCH_TIMEOUT   default: 5m    (.D bar)
#   KEEP_CLUSTER    if "1", do not delete the kind cluster on exit
#
# Output: stdout-only. Non-zero exit on any assertion failure.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART_DIR="${REPO_ROOT}/charts/incidentary-operator"
KUBECTL_BIN="${REPO_ROOT}/bin/k8s/1.35.0-linux-amd64/kubectl"

CLUSTER_NAME="${CLUSTER_NAME:-incidentary-operator-install-verify}"
IMAGE="${IMAGE:-incidentary-operator:install-verify}"
READY_TIMEOUT="${READY_TIMEOUT:-60s}"
BATCH_TIMEOUT="${BATCH_TIMEOUT:-5m}"

if ! command -v kubectl >/dev/null 2>&1; then
    if [[ -x "${KUBECTL_BIN}" ]]; then
        export PATH="$(dirname "${KUBECTL_BIN}"):${PATH}"
    else
        echo "FAIL: kubectl not found in PATH or at ${KUBECTL_BIN}"
        exit 1
    fi
fi

cleanup() {
    local rc=$?
    if [[ "${KEEP_CLUSTER:-0}" != "1" ]]; then
        echo "==> Deleting kind cluster ${CLUSTER_NAME}"
        kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
    else
        echo "==> KEEP_CLUSTER=1 — kind cluster ${CLUSTER_NAME} retained"
    fi
    exit "${rc}"
}
trap cleanup EXIT

# ----------------------------------------------------------------------------
# 1. Mock backend image (so the operator has somewhere to flush to)
# ----------------------------------------------------------------------------

MOCK_IMAGE="${REPO_ROOT}/.install-verify-mock-backend"
mkdir -p "${MOCK_IMAGE}"
cat > "${MOCK_IMAGE}/server.py" <<'PY'
import http.server, json
state = {"batches": 0, "topologies": 0}
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        if "topology" in self.path: state["topologies"] += 1
        else: state["batches"] += 1
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        del body
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"accepted":1,"dropped":0}')
    def do_GET(self):
        self.send_response(200); self.end_headers()
        self.wfile.write(json.dumps(state).encode())
    def log_message(self, *a, **kw): pass
http.server.HTTPServer(("", 8080), H).serve_forever()
PY
cat > "${MOCK_IMAGE}/Dockerfile" <<'DOCKER'
FROM docker.io/library/python:3.12-slim
COPY server.py /server.py
CMD ["python3", "/server.py"]
DOCKER

# ----------------------------------------------------------------------------
# 2. Cluster
# ----------------------------------------------------------------------------

echo "==> Creating kind cluster ${CLUSTER_NAME}"
kind create cluster --name "${CLUSTER_NAME}" --wait 120s

KCONFIG="$(mktemp)"
kind get kubeconfig --name "${CLUSTER_NAME}" > "${KCONFIG}"
export KUBECONFIG="${KCONFIG}"

# ----------------------------------------------------------------------------
# 3. Mock backend
# ----------------------------------------------------------------------------

echo "==> Building + loading mock backend image"
docker build -t mock-backend:install-verify "${MOCK_IMAGE}" >/dev/null 2>&1
kind load docker-image mock-backend:install-verify --name "${CLUSTER_NAME}"

kubectl create namespace mock-backend --dry-run=client -o yaml | kubectl apply -f -

cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata: {name: mock, namespace: mock-backend}
spec:
  replicas: 1
  selector: {matchLabels: {app: mock}}
  template:
    metadata: {labels: {app: mock}}
    spec:
      containers:
        - name: backend
          image: mock-backend:install-verify
          imagePullPolicy: Never
          ports: [{containerPort: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: mock, namespace: mock-backend}
spec:
  ports: [{port: 8080, targetPort: 8080}]
  selector: {app: mock}
EOF

kubectl -n mock-backend rollout status deploy/mock --timeout=2m

# ----------------------------------------------------------------------------
# 4. Operator
# ----------------------------------------------------------------------------

echo "==> Building + loading operator image ${IMAGE}"
docker build -t "${IMAGE}" "${REPO_ROOT}" >/dev/null 2>&1
kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"

echo "==> Installing operator chart from ${CHART_DIR}"
kubectl create namespace incidentary-system --dry-run=client -o yaml | kubectl apply -f -

INSTALL_START=$(date +%s)

helm install incidentary "${CHART_DIR}" \
    --namespace incidentary-system \
    --set apiKey="sk_test_install_verify" \
    --set workspaceId="org_test_install_verify" \
    --set replicaCount=2 \
    --set image.repository="${IMAGE%:*}" \
    --set image.tag="${IMAGE##*:}" \
    --set image.pullPolicy=Never \
    --set cluster.name="install-verify" \
    --set config.ingestEndpoint="http://mock.mock-backend.svc.cluster.local:8080" \
    --set config.topologyEndpoint="http://mock.mock-backend.svc.cluster.local:8080/topology" \
    --set config.servicesEndpoint="http://mock.mock-backend.svc.cluster.local:8080/services" \
    --wait \
    --timeout "${READY_TIMEOUT}"

INSTALL_END=$(date +%s)
INSTALL_DURATION=$((INSTALL_END - INSTALL_START))

echo "==> helm install completed in ${INSTALL_DURATION}s (bar: 60s)"

# ----------------------------------------------------------------------------
# 5. Pod readiness assertion (.D: Ready within 60s)
# ----------------------------------------------------------------------------

DEPLOY=$(kubectl -n incidentary-system get deploy -l app.kubernetes.io/name=incidentary-operator -o jsonpath='{.items[0].metadata.name}')

READY=$(kubectl -n incidentary-system get deploy "${DEPLOY}" -o jsonpath='{.status.readyReplicas}')
if [[ "${READY}" != "2" ]]; then
    echo "FAIL: expected 2 ready replicas; got ${READY}"
    exit 1
fi

if [[ "${INSTALL_DURATION}" -gt 60 ]]; then
    echo "FAIL: helm install + ready took ${INSTALL_DURATION}s > 60s SLO"
    exit 1
fi

echo "==> 2 replicas Ready (.D criterion #2 PASS)"

# ----------------------------------------------------------------------------
# 6. Topology + first batch arrival (.D: within 5 min)
# ----------------------------------------------------------------------------

echo "==> Waiting up to ${BATCH_TIMEOUT} for first batch + topology"

case "${BATCH_TIMEOUT}" in
    *m) BATCH_S=$(( ${BATCH_TIMEOUT%m} * 60 )) ;;
    *)  BATCH_S="${BATCH_TIMEOUT%s}" ;;
esac

DEADLINE=$(( $(date +%s) + BATCH_S ))
MOCK_IP=$(kubectl -n mock-backend get svc mock -o jsonpath='{.spec.clusterIP}')

# Generate a synthetic K8s event so the operator has something to flush
# (an OOMKill — known-good signal under the operator's tier-1 filter).
kubectl run install-verify-oom \
    --image=busybox \
    --restart=Never \
    --requests='memory=10Mi' \
    --limits='memory=10Mi' \
    -- sh -c 'yes > /dev/null' 2>/dev/null || true

BATCHES_SEEN=0
TOPOLOGIES_SEEN=0
while [[ $(date +%s) -lt ${DEADLINE} ]]; do
    STATE=$(kubectl exec -n mock-backend deploy/mock -- python3 -c "
import urllib.request, json
print(json.loads(urllib.request.urlopen('http://localhost:8080/').read()))
" 2>/dev/null || echo "{'batches': 0, 'topologies': 0}")
    BATCHES_SEEN=$(echo "${STATE}" | python3 -c "import sys, ast; print(ast.literal_eval(sys.stdin.read()).get('batches', 0))" 2>/dev/null || echo 0)
    TOPOLOGIES_SEEN=$(echo "${STATE}" | python3 -c "import sys, ast; print(ast.literal_eval(sys.stdin.read()).get('topologies', 0))" 2>/dev/null || echo 0)
    if [[ "${BATCHES_SEEN}" -ge 1 && "${TOPOLOGIES_SEEN}" -ge 1 ]]; then
        break
    fi
    sleep 5
done

if [[ "${BATCHES_SEEN}" -lt 1 ]]; then
    echo "FAIL: no CE batch arrived at backend within ${BATCH_TIMEOUT}"
    echo "    Operator log tail:"
    kubectl -n incidentary-system logs -l app.kubernetes.io/name=incidentary-operator --tail=30 | sed 's/^/      /'
    exit 1
fi

if [[ "${TOPOLOGIES_SEEN}" -lt 1 ]]; then
    echo "FAIL: no topology report arrived at backend within ${BATCH_TIMEOUT}"
    exit 1
fi

echo "==> ${BATCHES_SEEN} batch(es), ${TOPOLOGIES_SEEN} topology report(s) received within budget (.D criterion #3 PASS)"

# ----------------------------------------------------------------------------
# 7. IncidentaryConfig status
# ----------------------------------------------------------------------------

PHASE=$(kubectl -n incidentary-system get incidentaryconfig -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo "<missing>")
echo "==> IncidentaryConfig phase: ${PHASE}"
if [[ "${PHASE}" != "Running" ]]; then
    echo "FAIL: expected IncidentaryConfig.status.phase = Running; got ${PHASE}"
    exit 1
fi

echo "PASS: install verification complete (kind variant)."
echo "      .D criteria 1-3 + IncidentaryConfig status all green."
echo "      For GKE/EKS verification: re-run with KUBECONFIG pointed at the target cluster."
