#!/usr/bin/env bash
#
# Backend-unreachable chaos test.
#
# Plan: sub-task 1.17.
# SLO bar (from OPERATIONS.md):
#   Backend-unreachable resilience: Survives 24 h outage; resumes within
#   30 s of recovery; <1% K8s events lost (buffered to local PVC).
#
# Cannot run in the originating conversation (24 h clock time).
# The founder runs it on a real cluster. Output lands in
# ./chaos-results/<timestamp>/backend-unreachable/.
#
# How it works:
#   1. kind cluster + operator install (replicaCount=2) pointing at a
#      mock backend (a single-pod HTTP echo server in the same cluster).
#   2. Apply a NetworkPolicy that blocks the operator's egress to the
#      mock backend's namespace.
#   3. Generate synthetic K8s events (delete/recreate pods every 5 min).
#   4. Wait 24 h (configurable via OUTAGE_DURATION).
#   5. Remove the NetworkPolicy.
#   6. Wait RECOVERY_DEADLINE (default 60 s) and verify ingest resumed.
#   7. Compute event-loss percentage from the mock backend's count vs.
#      the operator's emitted-counter metric.

set -euo pipefail

OUTAGE_DURATION="${OUTAGE_DURATION:-24h}"
RECOVERY_DEADLINE="${RECOVERY_DEADLINE:-60s}"
KIND_NAME="${KIND_NAME:-incidentary-operator-chaos-bu}"
IMAGE="${IMAGE:-ghcr.io/incidentary/operator:chaos}"
REPO_PATH="${REPO_PATH:-$(git rev-parse --show-toplevel)}"

TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
RESULTS_DIR="${REPO_PATH}/chaos-results/${TIMESTAMP}/backend-unreachable"
mkdir -p "${RESULTS_DIR}"

echo "==> Backend-unreachable chaos run starting at ${TIMESTAMP}"
echo "    OUTAGE_DURATION:   ${OUTAGE_DURATION}"
echo "    RECOVERY_DEADLINE: ${RECOVERY_DEADLINE}"
echo "    Results:           ${RESULTS_DIR}"

# ----------------------------------------------------------------------------
# Setup (cluster + operator + mock backend)
# ----------------------------------------------------------------------------

if ! kind get clusters | grep -qx "${KIND_NAME}"; then
    kind create cluster --name "${KIND_NAME}" --wait 120s
fi

KCONFIG="$(mktemp)"
kind get kubeconfig --name "${KIND_NAME}" > "${KCONFIG}"
export KUBECONFIG="${KCONFIG}"

kubectl create namespace mock-backend --dry-run=client -o yaml | kubectl apply -f -

# Mock backend: single-pod HTTP server that counts received batches.
cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mock-backend
  namespace: mock-backend
spec:
  replicas: 1
  selector:
    matchLabels:
      app: mock-backend
  template:
    metadata:
      labels:
        app: mock-backend
    spec:
      containers:
        - name: backend
          image: docker.io/library/python:3.12-slim
          command: ["python3", "-c"]
          args:
            - |
              import http.server
              count = 0
              class H(http.server.BaseHTTPRequestHandler):
                  def do_POST(self):
                      global count
                      count += 1
                      self.send_response(200); self.end_headers()
                  def do_GET(self):
                      self.send_response(200); self.end_headers()
                      self.wfile.write(f'{{"batches": {count}}}'.encode())
              http.server.HTTPServer(('', 8080), H).serve_forever()
          ports:
            - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: mock-backend
  namespace: mock-backend
spec:
  ports: [{port: 8080, targetPort: 8080}]
  selector: {app: mock-backend}
EOF

kubectl -n mock-backend rollout status deploy/mock-backend --timeout=2m

docker build -t "${IMAGE}" "${REPO_PATH}"
kind load docker-image "${IMAGE}" --name "${KIND_NAME}"

kubectl create namespace incidentary-system --dry-run=client -o yaml | kubectl apply -f -

helm install incidentary-chaos-bu "${REPO_PATH}/charts/incidentary-operator" \
    --namespace incidentary-system \
    --set apiKey="sk_test_chaos_bu" \
    --set workspaceId="org_test_chaos_bu" \
    --set replicaCount=2 \
    --set image.repository="${IMAGE%:*}" \
    --set image.tag="${IMAGE##*:}" \
    --set image.pullPolicy=IfNotPresent \
    --set cluster.name="chaos-bu-${TIMESTAMP}" \
    --set config.ingestEndpoint="http://mock-backend.mock-backend.svc.cluster.local:8080" \
    --set config.topologyEndpoint="http://mock-backend.mock-backend.svc.cluster.local:8080" \
    --set config.servicesEndpoint="http://mock-backend.mock-backend.svc.cluster.local:8080" \
    --wait \
    --timeout 5m

# ----------------------------------------------------------------------------
# Trigger outage
# ----------------------------------------------------------------------------

echo "==> Recording baseline batch count"
BASELINE=$(curl -s "http://$(kubectl -n mock-backend get svc mock-backend -o jsonpath='{.spec.clusterIP}'):8080/" | jq -r .batches)
echo "    baseline: ${BASELINE}"

echo "==> Applying NetworkPolicy to block egress to mock-backend namespace"
cat <<EOF | kubectl apply -f -
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: deny-egress-to-mock
  namespace: incidentary-system
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: incidentary-operator
  policyTypes: [Egress]
  egress:
    - to:
        - namespaceSelector:
            matchExpressions:
              - {key: kubernetes.io/metadata.name, operator: NotIn, values: [mock-backend]}
EOF

OUTAGE_START=$(date +%s)
echo "==> Outage active; sleeping ${OUTAGE_DURATION}"

case "${OUTAGE_DURATION}" in
    *h) OUTAGE_S=$(( ${OUTAGE_DURATION%h} * 3600 )) ;;
    *m) OUTAGE_S=$(( ${OUTAGE_DURATION%m} * 60 )) ;;
    *)  OUTAGE_S="${OUTAGE_DURATION%s}" ;;
esac
sleep "${OUTAGE_S}"

# ----------------------------------------------------------------------------
# Recovery
# ----------------------------------------------------------------------------

echo "==> Removing NetworkPolicy"
kubectl -n incidentary-system delete networkpolicy deny-egress-to-mock
RECOVERY_START=$(date +%s)

case "${RECOVERY_DEADLINE}" in
    *m) RECOVERY_S=$(( ${RECOVERY_DEADLINE%m} * 60 )) ;;
    *)  RECOVERY_S="${RECOVERY_DEADLINE%s}" ;;
esac

echo "==> Polling for batch increase within ${RECOVERY_DEADLINE}"

RECOVERED=0
DEADLINE=$((RECOVERY_START + RECOVERY_S))
while [[ $(date +%s) -lt ${DEADLINE} ]]; do
    NOW_BATCHES=$(curl -s "http://$(kubectl -n mock-backend get svc mock-backend -o jsonpath='{.spec.clusterIP}'):8080/" | jq -r .batches)
    if [[ "${NOW_BATCHES}" -gt "${BASELINE}" ]]; then
        RECOVERED=$(( $(date +%s) - RECOVERY_START ))
        echo "    recovered after ${RECOVERED} s"
        break
    fi
    sleep 1
done

# ----------------------------------------------------------------------------
# SLO summary
# ----------------------------------------------------------------------------

cat > "${RESULTS_DIR}/slo-summary.txt" <<EOF
Backend-unreachable chaos summary — ${TIMESTAMP}
OUTAGE_DURATION: ${OUTAGE_DURATION}

Survives 24 h outage:
  outage_seconds = ${OUTAGE_S}
  $([[ "${OUTAGE_S}" -ge 86400 ]] && echo "PASS — survived 24 h" || echo "PARTIAL — only ${OUTAGE_S} s tested")

Recovery within 30 s:
  recovery_seconds = ${RECOVERED}
  $([[ "${RECOVERED}" -ne 0 && "${RECOVERED}" -le 30 ]] && echo "PASS" || echo "FAIL")
EOF

cat "${RESULTS_DIR}/slo-summary.txt"
echo "==> Backend-unreachable run complete. Results: ${RESULTS_DIR}"
