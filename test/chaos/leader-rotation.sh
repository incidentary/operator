#!/usr/bin/env bash
#
# Leader-rotation chaos test.
#
# Plan: sub-task 1.18.
# SLO bar (from OPERATIONS.md):
#   Leader handoff data loss: <1 K8s event lost per leader rotation.
#
# Cannot run in the originating conversation (2 h clock time).
# The founder runs it on a real cluster. Output lands in
# ./chaos-results/<timestamp>/leader-rotation/.
#
# How it works:
#   1. kind cluster + operator install (replicaCount=2).
#   2. Mock backend that counts received batches.
#   3. Generate a known number of synthetic K8s events at a fixed rate
#      (default: 1 OOMKill every 30 s).
#   4. Every 10 minutes, kill the active leader pod.
#   5. Repeat for ROTATION_DURATION (default 2h = 12 rotations).
#   6. Compute event-loss percentage: events_emitted / events_received.

set -euo pipefail

ROTATION_DURATION="${ROTATION_DURATION:-2h}"
ROTATION_INTERVAL="${ROTATION_INTERVAL:-10m}"
EVENT_INTERVAL_S="${EVENT_INTERVAL_S:-30}"
KIND_NAME="${KIND_NAME:-incidentary-operator-chaos-lr}"
IMAGE="${IMAGE:-ghcr.io/incidentary/operator:chaos}"
REPO_PATH="${REPO_PATH:-$(git rev-parse --show-toplevel)}"

TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
RESULTS_DIR="${REPO_PATH}/chaos-results/${TIMESTAMP}/leader-rotation"
mkdir -p "${RESULTS_DIR}"

echo "==> Leader-rotation chaos run starting at ${TIMESTAMP}"
echo "    ROTATION_DURATION: ${ROTATION_DURATION}"
echo "    ROTATION_INTERVAL: ${ROTATION_INTERVAL}"
echo "    EVENT_INTERVAL_S:  ${EVENT_INTERVAL_S}"
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

cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata: {name: mock-backend, namespace: mock-backend}
spec:
  replicas: 1
  selector: {matchLabels: {app: mock-backend}}
  template:
    metadata: {labels: {app: mock-backend}}
    spec:
      containers:
        - name: backend
          image: docker.io/library/python:3.12-slim
          command: ["python3", "-c"]
          args:
            - |
              import http.server, json
              count = 0
              class H(http.server.BaseHTTPRequestHandler):
                  def do_POST(self):
                      global count; count += 1
                      self.send_response(200); self.end_headers()
                  def do_GET(self):
                      self.send_response(200); self.end_headers()
                      self.wfile.write(json.dumps({"batches": count}).encode())
              http.server.HTTPServer(('', 8080), H).serve_forever()
          ports: [{containerPort: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: mock-backend, namespace: mock-backend}
spec:
  ports: [{port: 8080, targetPort: 8080}]
  selector: {app: mock-backend}
EOF

kubectl -n mock-backend rollout status deploy/mock-backend --timeout=2m

docker build -t "${IMAGE}" "${REPO_PATH}"
kind load docker-image "${IMAGE}" --name "${KIND_NAME}"

kubectl create namespace incidentary-system --dry-run=client -o yaml | kubectl apply -f -

helm install incidentary-chaos-lr "${REPO_PATH}/charts/incidentary-operator" \
    --namespace incidentary-system \
    --set apiKey="sk_test_chaos_lr" \
    --set workspaceId="org_test_chaos_lr" \
    --set replicaCount=2 \
    --set image.repository="${IMAGE%:*}" \
    --set image.tag="${IMAGE##*:}" \
    --set image.pullPolicy=IfNotPresent \
    --set cluster.name="chaos-lr-${TIMESTAMP}" \
    --set config.ingestEndpoint="http://mock-backend.mock-backend.svc.cluster.local:8080" \
    --set config.topologyEndpoint="http://mock-backend.mock-backend.svc.cluster.local:8080" \
    --set config.servicesEndpoint="http://mock-backend.mock-backend.svc.cluster.local:8080" \
    --wait \
    --timeout 5m

# ----------------------------------------------------------------------------
# Synthetic events generator (background)
# ----------------------------------------------------------------------------

EVENTS_EMITTED=0
EVENTS_LOG="${RESULTS_DIR}/events-emitted.log"

(
    while :; do
        kubectl run "oom-${RANDOM}" \
            --image=busybox --restart=Never \
            --requests='memory=10Mi' --limits='memory=10Mi' \
            -- sh -c 'yes > /dev/null' 2>/dev/null || true
        echo "$(date +%s) emitted" >> "${EVENTS_LOG}"
        sleep "${EVENT_INTERVAL_S}"
    done
) &
EVENT_PID=$!

# ----------------------------------------------------------------------------
# Leader-rotation loop
# ----------------------------------------------------------------------------

case "${ROTATION_INTERVAL}" in
    *m) RI_S=$(( ${ROTATION_INTERVAL%m} * 60 )) ;;
    *)  RI_S="${ROTATION_INTERVAL%s}" ;;
esac
case "${ROTATION_DURATION}" in
    *h) RD_S=$(( ${ROTATION_DURATION%h} * 3600 )) ;;
    *m) RD_S=$(( ${ROTATION_DURATION%m} * 60 )) ;;
    *)  RD_S="${ROTATION_DURATION%s}" ;;
esac

DEADLINE=$(( $(date +%s) + RD_S ))
ROTATION_COUNT=0

while [[ $(date +%s) -lt ${DEADLINE} ]]; do
    sleep "${RI_S}"
    LEADER=$(kubectl -n incidentary-system get pods -l app.kubernetes.io/name=incidentary-operator -o json | \
        jq -r '.items[] | select(.metadata.annotations["control-plane.alpha.kubernetes.io/leader"] != null) | .metadata.name' | head -1)
    if [[ -z "${LEADER}" ]]; then
        # Fallback: pick the older pod by creationTimestamp (likely the leader).
        LEADER=$(kubectl -n incidentary-system get pods -l app.kubernetes.io/name=incidentary-operator \
            --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[0].metadata.name}')
    fi
    echo "==> Killing leader pod ${LEADER} (rotation ${ROTATION_COUNT})"
    kubectl -n incidentary-system delete pod "${LEADER}" --wait=false
    ROTATION_COUNT=$(( ROTATION_COUNT + 1 ))
done

kill "${EVENT_PID}" 2>/dev/null || true

# ----------------------------------------------------------------------------
# SLO summary
# ----------------------------------------------------------------------------

EVENTS_EMITTED=$(wc -l < "${EVENTS_LOG}")
BATCHES_RECEIVED=$(curl -s "http://$(kubectl -n mock-backend get svc mock-backend -o jsonpath='{.spec.clusterIP}'):8080/" | jq -r .batches)

# Each batch may carry multiple events; this is a lower bound on
# events-received. The SLO is per-rotation not per-event-percent.

cat > "${RESULTS_DIR}/slo-summary.txt" <<EOF
Leader-rotation chaos summary — ${TIMESTAMP}
ROTATION_DURATION: ${ROTATION_DURATION}
ROTATION_INTERVAL: ${ROTATION_INTERVAL}
ROTATIONS:         ${ROTATION_COUNT}

Synthetic events emitted: ${EVENTS_EMITTED}
Backend batches received: ${BATCHES_RECEIVED}

Leader-handoff data loss SLO: < 1 event lost per rotation
  events_emitted - events_received_estimate < ROTATION_COUNT?
  (Cannot compute exactly without event-level counters; manual review
   of operator.log + mock backend lines required.)
EOF

cat "${RESULTS_DIR}/slo-summary.txt"
echo "==> Leader-rotation run complete. Results: ${RESULTS_DIR}"
