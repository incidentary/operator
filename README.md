# incidentary-operator

Read-only Kubernetes operator that captures cluster-side incident
telemetry — OOMKilled containers, pod evictions, schedule failures,
deploy rollouts, HPA scale events — and ships them to the Incidentary
v2 ingest API or any compatible backend.

Apache 2.0. Go 1.25. HA via controller-runtime leader election.

Status: alpha. Wire format is stable; CRD shape may change before v1.0.

## What it does

- Watches 14 Kubernetes resource types (Pods, Events core + events.k8s.io/v1,
  Nodes, Services, Namespaces, Deployments, StatefulSets, DaemonSets,
  ReplicaSets, HPAs, Jobs, CronJobs, Ingresses) via a single read-only
  ClusterRole.
- Maps K8s events to Incidentary Wire Format v2 kinds (K8S_OOM_KILL,
  K8S_POD_CRASH, K8S_EVICTION, K8S_NODE_PRESSURE, K8S_HPA_SCALE,
  K8S_IMAGE_PULL_FAIL, K8S_SCHEDULE_FAIL, K8S_POD_STARTED,
  K8S_POD_TERMINATED, K8S_DEPLOY_ROLLOUT, and the five DEPLOY_* kinds).
- Auto-discovers workloads (Deployments, StatefulSets, DaemonSets) and
  POSTs a topology report to the Incidentary backend every 5 minutes
  (configurable). Populates ghost services with real K8s metadata.
- Resolves a per-event `service_id` from an
  `incidentary.com/service-id` annotation when present, falling back to the
  owning workload's name (per the D4 mapping rule in the plan).
- Runs in HA mode with 2 replicas and controller-runtime leader election;
  event processing only runs on the active leader.
- Reconciles discovered workloads against the Incidentary services list
  and classifies each as matched / ghost / mismatched / new. Mismatched
  workloads get a Levenshtein-based fuzzy-match suggestion.
- Never mutates Kubernetes resources. The ClusterRole is `get;list;watch`
  only; the single namespace-scoped Role covers leader-election Leases,
  audit events, and the operator's own CRD status subresource.

## Quick start

One-command install:

```bash
helm install incidentary oci://ghcr.io/incidentary/charts/operator \
  --namespace incidentary-system --create-namespace \
  --set apiKey=sk_live_... \
  --set workspaceId=ws_... \
  --set cluster.name=prod-us-east-1
```

The chart requires two values: `apiKey` and `workspaceId`. Everything
else has sensible defaults documented in
`charts/incidentary-operator/values.yaml`. Installing without
`workspaceId` while `apiKey` is set fails fast at `helm install` time —
the server rejects every batch with `WORKSPACE_MISMATCH (422)` so we
prevent the misconfigured operator from ever starting.

Installing a locally-built chart during development:

```bash
helm install incidentary ./charts/incidentary-operator \
  --namespace incidentary-system --create-namespace \
  --set apiKey=dev-key --set workspaceId=ws_dev \
  --set image.tag=dev --set image.pullPolicy=Never
```

## Recent changes

The chart is still on `0.1.0` (alpha). The following changes have landed
since the first preview cut and may affect anyone running an older
checkout. See [CHANGELOG.md](./CHANGELOG.md) for the full list.

**Breaking — required configuration:**

1. `workspaceId` is now required in both Helm values and the
   `IncidentaryConfig` CR (`spec.workspaceId`, `MinLength=1`). The chart
   fails fast at `helm install` time when `apiKey` is set without a
   matching `workspaceId`. CRs without `workspaceId` are rejected at
   `kubectl apply`.

**Additive (no action required):**

- API key rotation works without a pod restart. Edit the referenced
  Secret data; the controller re-reads it and atomically swaps clients.
- Default logging is production-mode JSON with stacktraces only at
  Error level. Pass `--zap-devel=true` for verbose dev-mode output.
- Metrics endpoint is exposed on `:8080` (HTTP) by default. Set
  `metricsBindAddress: "0"` in values.yaml to disable, or
  `metricsSecure: true` to enable mTLS.
- Default CPU limit (`500m`) prevents node-level resource pressure
  under event storms.
- `INCIDENTARY_MIN_SEVERITY` and `INCIDENTARY_EXCLUDE_NAMESPACES` are
  now wired through to the running container (they were documented in
  values.yaml but had no effect in earlier builds).

## Configuration reference

| Helm value | CRD field | Env var | Default |
|------------|-----------|---------|---------|
| `apiKey` | `apiKeySecretRef` | `INCIDENTARY_API_KEY` | — (required) |
| `workspaceId` | `workspaceId` | `INCIDENTARY_WORKSPACE_ID` | — (required) |
| `cluster.name` | — | `K8S_CLUSTER_NAME` | `unknown` |
| `config.reconciliationIntervalSeconds` | `reconciliationIntervalSeconds` | `INCIDENTARY_RECONCILIATION_INTERVAL_SECONDS` | `300` |
| `config.eventFilters.minSeverity` | `eventFilters.minSeverity` | `INCIDENTARY_MIN_SEVERITY` | `warning` |
| `config.excludeNamespaces` | `excludeNamespaces` | `INCIDENTARY_EXCLUDE_NAMESPACES` | `kube-system,kube-public,kube-node-lease` |
| `metricsBindAddress` | — | — | `:8080` |
| `metricsSecure` | — | — | `false` |
| `config.ingestEndpoint` | `ingestEndpoint` | `INCIDENTARY_INGEST_ENDPOINT` | `https://api.incidentary.com/api/v2/ingest` |
| `config.topologyEndpoint` | `topologyEndpoint` | `INCIDENTARY_TOPOLOGY_ENDPOINT` | `https://api.incidentary.com/api/v2/workspace/topology` |
| `config.servicesEndpoint` | — | `INCIDENTARY_SERVICES_ENDPOINT` | `https://api.incidentary.com/api/v2/workspace/services` |
| `config.excludeNamespaces` | `excludeNamespaces` | — | `kube-system, kube-public, kube-node-lease` |
| `replicaCount` | — | — | `2` |

## Per-workload annotations

| Annotation | Effect |
|------------|--------|
| `incidentary.com/service-id: <value>` | Overrides the derived `service_id` for all events owned by this workload. Takes precedence over the workload's `metadata.name`. |

## Health, metrics, and status

- Liveness: `:8081/healthz`
- Readiness: `:8081/readyz`
- Prometheus metrics: `:8080/metrics`
- CRD status surfaces `Phase` (`Running` / `Degraded`),
  `LastReconciliation`, `WatchedWorkloads`, `MatchedServices`,
  `UnmatchedWorkloads`, and a `Ready` condition with a human-readable
  message (e.g. `informers watching 14 resource types`).

## Development

Prerequisites:

- Go 1.25+
- Docker with Buildx
- kubectl (or use the envtest kubectl downloaded by `make test` into
  `bin/k8s/<version>-linux-amd64/kubectl`)
- kind 0.20+ (for E2E)
- helm 3.x (for E2E)

Common commands:

```bash
make build        # compile the manager binary
make test         # run unit + envtest integration tests
make lint         # run golangci-lint (v2.8.0, pinned)
make manifests    # regenerate CRD YAML from kubebuilder markers
make generate     # regenerate DeepCopy methods
make run          # run the manager against the current kubeconfig
make docker-build # build local container image
```

### Running the Helm-based E2E

`test/e2e/helm-e2e.sh` provisions a kind cluster, builds the operator
image, loads a stubbed Incidentary API, installs the Helm chart,
exercises the operator with realistic workloads (payment-service,
checkout-api, a deliberate OOM-killed pod), and asserts that:

1. The operator sent a topology report containing every demo workload.
2. The operator sent ingest batches with `agent.type=k8s_operator`.
3. The K8S_OOM_KILL event for the OOM victim reached the mock.
4. The reconciliation loop polled the services endpoint at least once.
5. A server-side `X-Capture-Mode-Requested: FULL` response header is
   round-tripped: the mock marks `payment-service` as pending, the
   next operator flush for that service consumes the marker, and the
   mock's `capture_mode_consumed` log records the transition.

Run it:

```bash
bash test/e2e/helm-e2e.sh           # fresh cluster + cleanup
KEEP_CLUSTER=1 bash test/e2e/helm-e2e.sh  # leave cluster for debugging
```

The script runs against an isolated cluster named
`incidentary-operator-e2e` (override with `CLUSTER_NAME=...`) and cleans
up automatically. It is wired into the `e2e` job of the GitHub Actions
CI workflow (`.github/workflows/ci.yml`).

## License

Apache 2.0.
