# Operations & SLOs

This file documents the operational SLOs that gate every `incidentary-operator`
release.

## Service level objectives

| SLO | Bar | How tested | Status (v0.1.1) |
|---|---|---|---|
| **Memory ceiling** (steady-state, 1 000-pod cluster) | ≤ 512 MiB resident | 7-day soak ([`test/soak/`](./test/soak/)) | measured at GA cut |
| **CPU steady-state** | ≤ 0.2 cores | 7-day soak | measured at GA cut |
| **CPU peak (rollout)** | ≤ 1.0 core | 7-day soak | measured at GA cut |
| **Backend-unreachable resilience** | Survives 24 h outage; resumes within 30 s of recovery; < 1% K8s events lost (buffered to local PVC) | [`test/chaos/backend-unreachable.sh`](./test/chaos/backend-unreachable.sh) | measured at GA cut |
| **Leader-handoff data loss** | < 1 K8s event lost per leader rotation | [`test/chaos/leader-rotation.sh`](./test/chaos/leader-rotation.sh) | measured at GA cut |
| **Failed-emit retry** | Exponential backoff to 30 min; abandon after 24 h with `incidentary_emit_abandoned_total` increment | unit ([`internal/client/ingest_test.go`](./internal/client/ingest_test.go)) + chaos (1.17) | continuously verified in CI |
| **Helm upgrade** | Zero downtime on `replicaCount: 2` | [`test/e2e/helm-upgrade-e2e.sh`](./test/e2e/helm-upgrade-e2e.sh) | continuously verified in CI |
| **Self-metrics** | Prometheus endpoint at `:8080/metrics`; OTel semconv where applicable | [`docs/operator/metrics.mdx`](https://incidentary.com/docs/operator/metrics) | shipped |
| **Container CVE policy** | HIGH/CRITICAL patched within 14 days | Trivy in [`.github/workflows/release.yml`](./.github/workflows/release.yml) + [SECURITY.md](./SECURITY.md) | continuously verified in CI |
| **Cluster compatibility** | Kubernetes 1.27+ | CI matrix on kind 0.23 (Kubernetes 1.30) | continuously verified in CI |

The "Status (v0.1.1)" column transitions:

- `continuously verified in CI` — every commit re-validates this SLO.
- `measured at GA cut` — verified once during the GA acceptance window;
  re-measured on every minor-version bump.
- `shipped` — the asset exists; verification is straightforward inspection.

## How to run the SLO measurements yourself

### 7-day soak (memory + CPU SLOs)

```bash
KIND_NAME=soak-v0.1.1 \
DURATION=7d \
POD_COUNT=1000 \
bash test/soak/run.sh
```

The script provisions a kind cluster sized for `${POD_COUNT}` synthetic
workloads, installs the operator at `replicaCount=2`, runs `${DURATION}`,
and dumps memory + CPU profiles to `./soak-results/<timestamp>/`.

Pass criteria:

- `kubectl top pod` p99 RSS over the run ≤ 512 MiB.
- `kubectl top pod` p99 CPU over the steady-state window ≤ 200 m.
- `kubectl top pod` peak CPU during a synthetic rollout ≤ 1000 m.
- No `incidentary_emit_abandoned_total` increments unrelated to the
  injected backend outage.

### Backend-unreachable chaos

```bash
bash test/chaos/backend-unreachable.sh
```

Installs the operator pointing at a NetworkPolicy-blocked synthetic
backend endpoint for 24 hours, then unblocks the egress and asserts:

- During the 24-hour outage, no operator pod restarts and no
  `incidentary_emit_abandoned_total` increments fire (events buffered
  to a local PersistentVolumeClaim).
- Within 30 seconds of unblock, `incidentary_otlp_batches_received_total`
  resumes climbing on the backend side.
- The cumulative event-loss percentage measured by the synthetic
  `incidentary_chaos_events_emitted_total` vs.
  `incidentary_otlp_batches_received_total` ratio is < 1%.

### Leader-rotation chaos

```bash
bash test/chaos/leader-rotation.sh
```

Kills the active leader pod every 10 minutes for 2 hours and asserts:

- Median leader-failover time < 30 seconds (controlled by
  `--leader-elect-lease-duration`).
- Cumulative event loss across 12 rotations < 12 events.

### Helm upgrade — runs in CI

```bash
bash test/e2e/helm-upgrade-e2e.sh
```

Installs at `replicaCount=2`, runs a helm upgrade that touches the pod
template, and asserts that observed `readyReplicas` never drops below 1
during the rolling restart. Wired into
[`.github/workflows/ci.yml`](./.github/workflows/ci.yml).

## Operational guidance

### Sizing

| Cluster size | Recommended `resources.requests` | Recommended `resources.limits` |
|---|---|---|
| ≤ 100 pods | cpu: 50m, memory: 128Mi | cpu: 200m, memory: 256Mi |
| 100–1 000 pods | cpu: 100m, memory: 256Mi (default) | cpu: 500m, memory: 512Mi (default) |
| 1 000+ pods | cpu: 200m, memory: 512Mi | cpu: 1, memory: 1Gi |

The default (`100m` / `256Mi` request, `500m` / `512Mi` limit) is sized
for the 100-1000 pod range, which covers most production clusters.

### Observability

- **Logs:** stdout, JSON-structured by default (zap.Options{Development: false}).
  Pass `--zap-devel=true` for verbose development-mode console output.
- **Metrics:** see [`docs/operator/metrics.mdx`](https://incidentary.com/docs/operator/metrics)
  for the full Prometheus reference.
- **Status:** `kubectl get incidentaryconfig` shows phase, watched
  workload count, last reconcile time, and a human-readable Ready
  condition message.

### Rolling-restart strategy

The Deployment uses Kubernetes' default `RollingUpdate` strategy with
`maxSurge: 25%` and `maxUnavailable: 25%`. On `replicaCount: 2`, this
means at most 1 replica is unavailable during an upgrade — combined with
PodDisruptionBudget `minAvailable: 1`, this guarantees at least one
ready replica throughout the rollout.

### What "leader" means

Only the active leader processes K8s events. The standby replica is
warm — its informers are populated and watching — but it does not call
`emit()` and does not flush to the Incidentary backend. On leader loss,
the standby acquires the lease (typically within 15 seconds with the
default `--leader-elect-lease-duration`) and resumes emission.

This means **two-replica HA does not double the outbound batch rate.**
The standby is for failover, not load-sharing. Cluster sizing should
plan for the active replica's RSS only; the standby's RSS is roughly
60% of the active's because its goroutine pool is smaller.

## Incident response runbook

See the [operator troubleshooting guide][troubleshooting] for the full
diagnostic flow (RBAC errors, network errors, leader-election errors,
backend-reachability errors).

[troubleshooting]: https://incidentary.com/docs/operator/troubleshooting
