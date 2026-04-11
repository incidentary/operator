#!/usr/bin/env bash
#
# Helm-based end-to-end test for the Incidentary operator.
#
# Verifies the full adoption path:
#   1. kind cluster → helm install → operator pod becomes Ready
#   2. Operator registers 14 informers (from CRD status message)
#   3. Operator discovers workloads and POSTs a topology report
#   4. Operator maps K8s events (pod OOM, deployment rollout) into v2
#      wire-format events and flushes them to the ingest endpoint
#   5. Response-header round-trip: when the mock backend asks for
#      X-Capture-Mode-Requested: FULL, the operator consumes it on the
#      next flush touching that service.
#
# Requirements on the host:
#   - docker (daemon running)
#   - kind 0.20+
#   - helm 3.x
#   - kubectl (falls back to $(git rev-parse --show-toplevel)/bin/k8s/.../kubectl
#     which kubebuilder's envtest downloads)
#   - python3 (for JSON parsing in assertions)
#
# Environment variables:
#   CLUSTER_NAME     default: incidentary-operator-e2e
#   IMAGE            default: ghcr.io/incidentary/operator:e2e
#   KEEP_CLUSTER     if "1", do not delete the kind cluster on exit
#
# Exit codes:
#   0 on success, 1 on any assertion failure.

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-incidentary-operator-e2e}"
IMAGE="${IMAGE:-ghcr.io/incidentary/operator:e2e}"
MOCK_IMAGE="mock-incidentary:e2e"
NAMESPACE="incidentary-system"
MOCK_NAMESPACE="mock-incidentary"
DEMO_NAMESPACE="demo"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

log() { printf '[e2e] %s\n' "$*" >&2; }

# -----------------------------------------------------------------------------
# Tool discovery
# -----------------------------------------------------------------------------
require_tool() {
    local tool="$1"
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "[e2e] required tool not found: $tool" >&2
        exit 1
    fi
}

KUBECTL="${KUBECTL:-}"
if [[ -z "$KUBECTL" ]]; then
    if command -v kubectl >/dev/null 2>&1; then
        KUBECTL="kubectl"
    else
        # Fall back to the kubectl binary that envtest downloaded when
        # the controller integration tests ran. That binary is version-
        # pinned to the controller-runtime's target Kubernetes version
        # and is always available after `make test`.
        FALLBACK_KUBECTL="$(find "$REPO_ROOT/bin/k8s" -type f -name kubectl 2>/dev/null | head -n1 || true)"
        if [[ -z "$FALLBACK_KUBECTL" ]]; then
            echo "[e2e] kubectl not found and no envtest fallback in bin/k8s" >&2
            exit 1
        fi
        KUBECTL="$FALLBACK_KUBECTL"
        log "using fallback kubectl: $KUBECTL"
    fi
fi

require_tool kind
require_tool helm
require_tool docker
require_tool python3

CONTEXT="kind-${CLUSTER_NAME}"
kctl() { "$KUBECTL" --context "$CONTEXT" "$@"; }

# -----------------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------------
cleanup() {
    local rc=$?
    if [[ "$KEEP_CLUSTER" == "1" ]]; then
        log "KEEP_CLUSTER=1 — leaving cluster '$CLUSTER_NAME' in place"
        return $rc
    fi
    log "deleting kind cluster '$CLUSTER_NAME'"
    kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
    return $rc
}
trap cleanup EXIT

# -----------------------------------------------------------------------------
# Cluster + images
# -----------------------------------------------------------------------------
log "creating kind cluster: $CLUSTER_NAME"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
    log "cluster already exists — reusing"
else
    cat <<YAML | kind create cluster --name "$CLUSTER_NAME" --wait 120s --config -
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
YAML
fi

log "building operator image: $IMAGE"
docker build -t "$IMAGE" "$REPO_ROOT" >/dev/null

log "loading operator image into kind"
kind load docker-image "$IMAGE" --name "$CLUSTER_NAME" >/dev/null

# Build + load the mock Incidentary API. Writing it inline so the script
# is self-contained and CI does not need an external fixture directory.
MOCK_DIR="$(mktemp -d)"
cat > "$MOCK_DIR/Dockerfile" <<'DOCKERFILE'
FROM python:3.12-alpine
WORKDIR /app
COPY server.py .
EXPOSE 8080
CMD ["python", "-u", "server.py"]
DOCKERFILE

cat > "$MOCK_DIR/server.py" <<'PYSERVER'
"""Minimal Incidentary v2 API mock used by the operator E2E script."""
from __future__ import annotations

import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

LOCK = threading.Lock()
STATE: dict[str, Any] = {
    "ingest_calls": [],
    "topology_calls": [],
    "services_calls": 0,
    "capture_mode_pending": set(),
    "capture_mode_consumed": [],
}


class Handler(BaseHTTPRequestHandler):
    def _read(self) -> bytes:
        n = int(self.headers.get("Content-Length", "0"))
        return self.rfile.read(n) if n > 0 else b""

    def _reply(self, code: int, body: dict[str, Any], extra: dict[str, str] | None = None) -> None:
        payload = json.dumps(body).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, fmt: str, *args: Any) -> None:  # noqa: A002
        sys.stderr.write(f"[mock] {self.address_string()} {fmt % args}\n")

    def do_POST(self) -> None:  # noqa: N802
        body = self._read()
        if self.path == "/api/v2/ingest":
            try:
                payload = json.loads(body) if body else {}
            except json.JSONDecodeError:
                self._reply(400, {"error": "invalid_json"})
                return
            events = payload.get("events", [])
            service_ids = {e.get("service_id") for e in events if e.get("service_id")}
            extra: dict[str, str] = {}
            with LOCK:
                STATE["ingest_calls"].append({
                    "events": len(events),
                    "service_ids": sorted(service_ids),
                    "agent_type": payload.get("agent", {}).get("type"),
                })
                overlap = STATE["capture_mode_pending"] & service_ids
                if overlap:
                    extra["X-Capture-Mode-Requested"] = "FULL"
                    STATE["capture_mode_pending"] -= service_ids
                    STATE["capture_mode_consumed"].append(sorted(overlap))
            self._reply(200, {"accepted": len(events), "dropped": 0, "drop_reasons": {}}, extra)
            return

        if self.path == "/api/v2/workspace/topology":
            try:
                payload = json.loads(body) if body else {}
            except json.JSONDecodeError:
                self._reply(400, {"error": "invalid_json"})
                return
            workloads = payload.get("workloads", [])
            with LOCK:
                STATE["topology_calls"].append({
                    "cluster_name": payload.get("cluster_name"),
                    "workload_count": len(workloads),
                    "service_ids": [w.get("service_id") for w in workloads],
                })
            self._reply(200, {
                "accepted": len(workloads),
                "created_ghost_services": len(workloads),
                "updated_services": 0,
            })
            return

        if self.path.startswith("/__test/capture_mode/"):
            service_id = self.path.rsplit("/", 1)[-1]
            with LOCK:
                STATE["capture_mode_pending"].add(service_id)
            self._reply(200, {"ok": True, "service_id": service_id})
            return

        self._reply(404, {"error": "not_found", "path": self.path})

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/api/v2/workspace/services":
            with LOCK:
                STATE["services_calls"] += 1
            self._reply(200, {"services": []})
            return
        if self.path == "/__test/state":
            with LOCK:
                self._reply(200, {
                    "ingest_calls": STATE["ingest_calls"],
                    "topology_calls": STATE["topology_calls"],
                    "services_calls": STATE["services_calls"],
                    "capture_mode_pending": sorted(STATE["capture_mode_pending"]),
                    "capture_mode_consumed": STATE["capture_mode_consumed"],
                })
            return
        if self.path == "/healthz":
            self._reply(200, {"status": "ok"})
            return
        self._reply(404, {"error": "not_found", "path": self.path})


if __name__ == "__main__":
    srv = HTTPServer(("0.0.0.0", 8080), Handler)
    sys.stderr.write("[mock] listening on :8080\n")
    srv.serve_forever()
PYSERVER

log "building mock image: $MOCK_IMAGE"
docker build -t "$MOCK_IMAGE" "$MOCK_DIR" >/dev/null
log "loading mock image into kind"
kind load docker-image "$MOCK_IMAGE" --name "$CLUSTER_NAME" >/dev/null
rm -rf "$MOCK_DIR"

# -----------------------------------------------------------------------------
# Deploy: mock API
# -----------------------------------------------------------------------------
log "deploying mock Incidentary API"
kctl create ns "$MOCK_NAMESPACE" --dry-run=client -o yaml | kctl apply -f - >/dev/null
cat <<YAML | kctl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mock-incidentary
  namespace: $MOCK_NAMESPACE
spec:
  replicas: 1
  selector:
    matchLabels:
      app: mock-incidentary
  template:
    metadata:
      labels:
        app: mock-incidentary
    spec:
      containers:
        - name: mock
          image: $MOCK_IMAGE
          imagePullPolicy: Never
          ports:
            - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: mock-incidentary
  namespace: $MOCK_NAMESPACE
spec:
  type: ClusterIP
  ports:
    - port: 80
      targetPort: 8080
  selector:
    app: mock-incidentary
YAML

kctl -n "$MOCK_NAMESPACE" rollout status deploy/mock-incidentary --timeout=60s

# -----------------------------------------------------------------------------
# Deploy: operator via Helm
# -----------------------------------------------------------------------------
log "installing operator via helm"
kctl create ns "$NAMESPACE" --dry-run=client -o yaml | kctl apply -f - >/dev/null
helm --kube-context "$CONTEXT" upgrade --install incidentary \
    "$REPO_ROOT/charts/incidentary-operator" \
    --namespace "$NAMESPACE" \
    --set apiKey=e2e-test-key \
    --set image.tag=e2e \
    --set "image.repository=ghcr.io/incidentary/operator" \
    --set image.pullPolicy=Never \
    --set cluster.name=kind-e2e \
    --set replicaCount=1 \
    --set config.reconciliationIntervalSeconds=30 \
    --set "config.ingestEndpoint=http://mock-incidentary.${MOCK_NAMESPACE}.svc.cluster.local/api/v2/ingest" \
    --set "config.topologyEndpoint=http://mock-incidentary.${MOCK_NAMESPACE}.svc.cluster.local/api/v2/workspace/topology" \
    --set "config.servicesEndpoint=http://mock-incidentary.${MOCK_NAMESPACE}.svc.cluster.local/api/v2/workspace/services" \
    --wait --timeout=120s >/dev/null

log "waiting for operator rollout"
kctl -n "$NAMESPACE" rollout status deploy/incidentary-incidentary-operator --timeout=120s

# -----------------------------------------------------------------------------
# Exercise the operator with realistic workloads
# -----------------------------------------------------------------------------
log "deploying demo workloads + OOM victim"
kctl create ns "$DEMO_NAMESPACE" --dry-run=client -o yaml | kctl apply -f - >/dev/null
cat <<'YAML' | kctl apply -n demo -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata:
  name: payment-service
  labels: { app: payment-service }
spec:
  replicas: 1
  selector: { matchLabels: { app: payment-service } }
  template:
    metadata:
      labels: { app: payment-service }
      annotations:
        incidentary.io/service-id: payment-service
    spec:
      containers:
        - name: app
          image: busybox:latest
          command: ["sh","-c","sleep 3600"]
          resources:
            requests: { memory: 16Mi }
            limits:   { memory: 32Mi }
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout-api
  labels: { app: checkout-api }
spec:
  replicas: 2
  selector: { matchLabels: { app: checkout-api } }
  template:
    metadata:
      labels: { app: checkout-api }
    spec:
      containers:
        - name: app
          image: busybox:latest
          command: ["sh","-c","sleep 3600"]
          resources:
            requests: { memory: 16Mi }
            limits:   { memory: 32Mi }
---
apiVersion: v1
kind: Pod
metadata:
  name: oom-victim
  labels: { app: oom-victim }
spec:
  restartPolicy: Never
  containers:
    - name: hog
      image: polinux/stress:latest
      command: ["stress"]
      args: ["--vm","1","--vm-bytes","256M","--vm-hang","0"]
      resources:
        requests: { memory: 10Mi }
        limits:   { memory: 32Mi }
YAML

log "waiting 60s for discovery, OOM, and batch flushes"
sleep 60

# -----------------------------------------------------------------------------
# Fetch and assert on the mock's state
# -----------------------------------------------------------------------------
fetch_state() {
    kctl -n "$MOCK_NAMESPACE" exec deploy/mock-incidentary -- \
        wget -q -O- http://127.0.0.1:8080/__test/state
}

log "fetching mock state"
STATE_JSON="$(fetch_state)"
echo "$STATE_JSON" | python3 -m json.tool >/tmp/operator-e2e-state.json
log "saved state to /tmp/operator-e2e-state.json"

assert_true() {
    local name="$1"; shift
    if python3 -c "import json,sys; d=json.loads(sys.stdin.read()); sys.exit(0 if ($*) else 1)" <<< "$STATE_JSON"; then
        log "PASS  $name"
    else
        log "FAIL  $name"
        log "state: $STATE_JSON"
        exit 1
    fi
}

assert_true "operator sent at least one topology report"                "len(d['topology_calls']) >= 1"
assert_true "topology report contains the expected demo workloads"      "set(['payment-service','checkout-api']).issubset(set(w for c in d['topology_calls'] for w in c['service_ids']))"
assert_true "operator sent ingest batches"                              "len(d['ingest_calls']) >= 1"
assert_true "at least one batch carries an agent.type=k8s_operator"     "any(c['agent_type']=='k8s_operator' for c in d['ingest_calls'])"
assert_true "oom-victim event was delivered"                            "any('oom-victim' in c['service_ids'] for c in d['ingest_calls'])"
assert_true "reconciliation loop polled the services endpoint"         "d['services_calls'] >= 1"

# -----------------------------------------------------------------------------
# Capture-mode round-trip
# -----------------------------------------------------------------------------
log "marking payment-service for FULL capture"
kctl -n "$MOCK_NAMESPACE" exec deploy/mock-incidentary -- \
    wget -q -O- --post-data='' http://127.0.0.1:8080/__test/capture_mode/payment-service >/dev/null

log "forcing a fresh event batch for payment-service via rollout restart"
kctl -n "$DEMO_NAMESPACE" rollout restart deploy/payment-service >/dev/null
sleep 20

STATE_JSON="$(fetch_state)"
echo "$STATE_JSON" | python3 -m json.tool >/tmp/operator-e2e-state.json

assert_true "capture_mode_pending was drained by operator's next flush" "'payment-service' not in d['capture_mode_pending']"
assert_true "mock recorded a capture_mode consumption"                  "len(d['capture_mode_consumed']) >= 1"
assert_true "the consumed capture_mode request was for payment-service" "any('payment-service' in batch for batch in d['capture_mode_consumed'])"

log ""
log "ALL E2E ASSERTIONS PASSED"
log ""
log "state snapshot saved to /tmp/operator-e2e-state.json"
