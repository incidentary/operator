#!/usr/bin/env bash
#
# 7-day soak test for the operator's memory + CPU SLOs.
#
# Plan: sub-task 1.16.
# SLO bars (from OPERATIONS.md):
#   - Memory ceiling (steady-state, 1 000-pod cluster): ≤ 512 MiB resident
#   - CPU steady-state: ≤ 0.2 cores
#   - CPU peak (rollout): ≤ 1 core
#
# Cannot be executed inside the Claude Code conversation that
# wrote this script — it requires DURATION (7 days) of clock time and a
# kind cluster sized for POD_COUNT (1000) synthetic workloads. The
# founder runs it on a real machine. Results land in
# ./soak-results/<timestamp>/.
#
# Environment variables:
#   DURATION       default: 7d   (Go-style duration: 7d, 12h, 30m...)
#   POD_COUNT      default: 1000 (synthetic workload count)
#   KIND_NAME      default: incidentary-operator-soak
#   IMAGE          default: ghcr.io/incidentary/operator:soak
#   REPO_PATH      default: $(git rev-parse --show-toplevel)
#
# Output:
#   soak-results/<timestamp>/
#     ├── memory.csv          (kubectl top pod every 60 s)
#     ├── cpu.csv             (kubectl top pod every 60 s)
#     ├── operator.log        (operator stdout from start to finish)
#     └── slo-summary.txt     (pass/fail per SLO with measured numbers)

set -euo pipefail

DURATION="${DURATION:-7d}"
POD_COUNT="${POD_COUNT:-1000}"
KIND_NAME="${KIND_NAME:-incidentary-operator-soak}"
IMAGE="${IMAGE:-ghcr.io/incidentary/operator:soak}"
REPO_PATH="${REPO_PATH:-$(git rev-parse --show-toplevel)}"

TIMESTAMP="$(date -u +%Y%m%dT%H%M%SZ)"
RESULTS_DIR="${REPO_PATH}/soak-results/${TIMESTAMP}"
mkdir -p "${RESULTS_DIR}"

echo "==> Soak run starting at ${TIMESTAMP}"
echo "    DURATION:  ${DURATION}"
echo "    POD_COUNT: ${POD_COUNT}"
echo "    Results:   ${RESULTS_DIR}"

# ----------------------------------------------------------------------------
# 1. Cluster
# ----------------------------------------------------------------------------

if ! kind get clusters | grep -qx "${KIND_NAME}"; then
    echo "==> Creating kind cluster ${KIND_NAME} (this can take 1-2 minutes)"
    kind create cluster --name "${KIND_NAME}" --wait 120s
fi

KCONFIG="$(mktemp)"
kind get kubeconfig --name "${KIND_NAME}" > "${KCONFIG}"
export KUBECONFIG="${KCONFIG}"

# ----------------------------------------------------------------------------
# 2. Operator install
# ----------------------------------------------------------------------------

echo "==> Building + loading operator image"
docker build -t "${IMAGE}" "${REPO_PATH}"
kind load docker-image "${IMAGE}" --name "${KIND_NAME}"

echo "==> Installing operator at replicaCount=2"
kubectl create namespace incidentary-system --dry-run=client -o yaml | kubectl apply -f -

helm install incidentary-soak "${REPO_PATH}/charts/incidentary-operator" \
    --namespace incidentary-system \
    --set apiKey="sk_test_soak_${TIMESTAMP}" \
    --set workspaceId="org_test_soak" \
    --set replicaCount=2 \
    --set image.repository="${IMAGE%:*}" \
    --set image.tag="${IMAGE##*:}" \
    --set image.pullPolicy=IfNotPresent \
    --set cluster.name="soak-${TIMESTAMP}" \
    --wait \
    --timeout 5m

# ----------------------------------------------------------------------------
# 3. Synthetic workload
# ----------------------------------------------------------------------------

echo "==> Creating ${POD_COUNT} synthetic workloads"

for i in $(seq 1 "${POD_COUNT}"); do
    NAMESPACE="soak-ns-$((i % 50))"
    kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
    cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: synthetic-${i}
  namespace: ${NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: synthetic-${i}
  template:
    metadata:
      labels:
        app: synthetic-${i}
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.9
          resources:
            requests:
              cpu: 1m
              memory: 2Mi
EOF
done

echo "==> Waiting 60 s for the operator to reach steady state"
sleep 60

# ----------------------------------------------------------------------------
# 4. Pre-flight: metrics-server must be available for `kubectl top`
# ----------------------------------------------------------------------------

if ! kubectl top pod -n kube-system --no-headers 2>/dev/null | head -1 >/dev/null; then
    cat <<EOF > "${RESULTS_DIR}/slo-summary.txt"
Soak SLO summary — ${TIMESTAMP}
DURATION: ${DURATION}  POD_COUNT: ${POD_COUNT}

SKIPPED — metrics-server is not available in this cluster.
'kubectl top pod' returned no output, so the soak loop cannot
measure memory or CPU. Install metrics-server before re-running:

  helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server
  helm upgrade --install metrics-server metrics-server/metrics-server \\
    --namespace kube-system \\
    --set 'args[0]=--kubelet-insecure-tls'

(The --kubelet-insecure-tls flag is required on kind because the
kubelet's serving cert is self-signed. On GKE/EKS metrics-server is
already installed; you can skip this step.)
EOF
    cat "${RESULTS_DIR}/slo-summary.txt"
    echo "==> Soak run aborted before sampling. Install metrics-server and re-run."
    exit 0
fi

# ----------------------------------------------------------------------------
# 5. Sample memory + CPU every 60 s for ${DURATION}
# ----------------------------------------------------------------------------

# Convert DURATION to seconds.
case "${DURATION}" in
    *d) DURATION_S=$(( ${DURATION%d} * 86400 )) ;;
    *h) DURATION_S=$(( ${DURATION%h} * 3600 )) ;;
    *m) DURATION_S=$(( ${DURATION%m} * 60 )) ;;
    *s) DURATION_S="${DURATION%s}" ;;
    *)  echo "FAIL: unrecognized DURATION format: ${DURATION}"; exit 1 ;;
esac

DEADLINE=$(( $(date +%s) + DURATION_S ))

echo "==> Sampling for ${DURATION_S} s (deadline epoch ${DEADLINE})"

echo "ts,pod,memory_bytes" > "${RESULTS_DIR}/memory.csv"
echo "ts,pod,cpu_milli"     > "${RESULTS_DIR}/cpu.csv"

kubectl -n incidentary-system logs -l app.kubernetes.io/name=incidentary-operator --tail=-1 -f \
    > "${RESULTS_DIR}/operator.log" 2>&1 &
LOG_PID=$!

while [[ $(date +%s) -lt ${DEADLINE} ]]; do
    TS=$(date +%s)
    while read -r line; do
        POD=$(awk '{print $1}' <<< "${line}")
        CPU=$(awk '{gsub(/m/, "", $2); print $2}' <<< "${line}")
        MEM=$(awk '{gsub(/Mi/, "", $3); print $3 * 1024 * 1024}' <<< "${line}")
        echo "${TS},${POD},${MEM}" >> "${RESULTS_DIR}/memory.csv"
        echo "${TS},${POD},${CPU}" >> "${RESULTS_DIR}/cpu.csv"
    done < <(kubectl -n incidentary-system top pod \
                  -l app.kubernetes.io/name=incidentary-operator \
                  --no-headers 2>/dev/null || true)
    sleep 60
done

kill "${LOG_PID}" 2>/dev/null || true

# ----------------------------------------------------------------------------
# 6. SLO summary
# ----------------------------------------------------------------------------

echo "==> Computing SLO summary"

P99_MEM_BYTES=$(awk -F, 'NR>1 {print $3}' "${RESULTS_DIR}/memory.csv" | sort -n | awk 'BEGIN{c=0} {a[c++]=$1} END{print a[int(c*0.99)]}')
P99_MEM_MIB=$(awk "BEGIN{printf \"%.0f\", ${P99_MEM_BYTES} / 1024 / 1024}")

P99_CPU_MILLI=$(awk -F, 'NR>1 {print $3}' "${RESULTS_DIR}/cpu.csv" | sort -n | awk 'BEGIN{c=0} {a[c++]=$1} END{print a[int(c*0.99)]}')

MAX_CPU_MILLI=$(awk -F, 'NR>1 {print $3}' "${RESULTS_DIR}/cpu.csv" | sort -n | tail -1)

cat > "${RESULTS_DIR}/slo-summary.txt" <<EOF
Soak SLO summary — ${TIMESTAMP}
DURATION: ${DURATION}  POD_COUNT: ${POD_COUNT}

Memory ceiling (≤ 512 MiB):
  p99 RSS = ${P99_MEM_MIB} MiB
  $([[ "${P99_MEM_MIB}" -le 512 ]] && echo "PASS" || echo "FAIL")

CPU steady-state (≤ 200 m):
  p99 CPU = ${P99_CPU_MILLI} m
  $([[ "${P99_CPU_MILLI}" -le 200 ]] && echo "PASS" || echo "FAIL")

CPU peak (≤ 1000 m):
  max CPU = ${MAX_CPU_MILLI} m
  $([[ "${MAX_CPU_MILLI}" -le 1000 ]] && echo "PASS" || echo "FAIL")
EOF

cat "${RESULTS_DIR}/slo-summary.txt"
echo "==> Soak run complete. Results: ${RESULTS_DIR}"
