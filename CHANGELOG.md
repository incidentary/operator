# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Until the first 1.0.0 release the public surface (CRD schema, Helm
values, wire format) may break between minor versions; breaking changes
are clearly flagged below.

## [Unreleased]

### Breaking

- **`spec.workspaceId` is now a required field on the `IncidentaryConfig`
  CRD** (`MinLength=1`). The operator embeds it as `agent.workspace_id`
  on every ingest batch; without it the server returns
  `WORKSPACE_MISMATCH (422)` and silently drops everything. Existing CRs
  without this field are rejected at `kubectl apply` time after the CRD
  upgrade — edit each CR before upgrading.
- **`workspaceId` is now a required Helm value** when `apiKey` is set.
  The chart fails fast at `helm install` / `helm template` time with
  `workspaceId is required when apiKey is set` rather than letting a
  misconfigured operator silently drop telemetry.

### Added

- **Hot-swappable API credentials.** `client.Provider` holds the active
  ingest, topology, and services clients behind an `RWMutex`. The
  `IncidentaryConfigReconciler` calls `Rotator.Rotate` on every
  reconcile (and via a `Watches(Secret, ...)` predicate, on every
  Secret data change), atomically swapping all three clients. In-flight
  HTTP requests run to completion with their old credentials; new
  requests pick up the rotated client. **API key rotation no longer
  requires a pod restart** — edit the referenced Secret and the
  operator picks up the new key within one reconcile cycle.
- **`buildInitialProvider` bootstrap helper** in `cmd/config.go` seeds
  the Provider with credentials read from environment variables, then
  immediately calls `Rotate` so the operator starts with the configured
  state.
- **Closure-based client resolution** through the pipeline. The
  `Batcher` accepts an `IngestClientProvider` (a `func() IngestClient`)
  resolved on every flush; `discovery.Loop` and `discovery.Reconciler`
  follow the same pattern with their respective client types. This is
  what makes hot-rotation work end-to-end.
- **`CHANGELOG.md`** (this file).
- **`charts/incidentary-operator/templates/validation.yaml`** — chart-side
  install-time validation guard that fails `helm install` / `helm
  template` when `apiKey` is set without `workspaceId`.
- **CPU limit** in the Helm chart (`limits.cpu: 500m`). Prevents an
  event-storm or initial cache sync from saturating node CPU.
- **`make test-integration` target** runs the unit suite plus
  build-tagged envtest integration tests under `-race`. Wired into the
  CI `test` job.
- **`internal/informers/integration_test.go`** — envtest harness that
  spins up a real control plane, starts the Manager, and asserts
  `OnAdd`/`OnUpdate`/`OnDelete` fire for a created/updated/deleted
  Deployment. Build tag: `integration`.
- **`internal/informers/cache_report_test.go`** — internal-package
  unit tests that exercise `reportCacheSizes` and
  `reportCacheSizesLoop` directly via fake stores, lifting the previously
  untested cache-sampling goroutine to 100% / 85.7% line coverage.
- **`internal/informers/unwrap.go`** — extracts the
  `DeletedFinalStateUnknown` tombstone-unwrap branch into a unit-testable
  helper.
- **`internal/client/provider.go`** — the `Provider` type plus the
  `droppingIngestClient`, `droppingTopologyClient`, and
  `emptyServicesClient` stubs (lifted from `cmd/main.go` where they
  were duplicated). 9 tests including a `-race`-validated 50 readers ×
  100 rotations concurrency stress test.
- **`Provider.WorkspaceID()`** — RWMutex-protected accessor for the
  active workspace ID. The Batcher's agent producer calls it on every
  flush so workspace-ID changes take effect immediately.
- **Configuration warning predicates**
  (`warnIfMisconfigured`, `warnIfClusterNameUnset`) in `cmd/config.go`
  surface common misconfigurations as structured startup logs.
- **README "Recent changes" section** documenting breaking and
  additive changes for users running off-tip.
- **E2E assertion** that every batch carries the configured
  `agent.workspace_id` (regression test for the `INCIDENTARY_WORKSPACE_ID`
  wiring).

### Changed

- **Default zap logger configuration**: `Development: false` (was
  `true`). The kubebuilder scaffold ships dev mode as the default,
  which attaches a full goroutine stacktrace to every Warn-level log
  line. Production now gets clean JSON output with stacktraces only at
  Error level. Users who want verbose dev-mode logs can opt in via
  `--zap-devel=true`.
- **Metrics endpoint enabled by default** on `:8080` (HTTP). Previously
  `metricsBindAddress: "0"` shipped disabled. Set to `"0"` explicitly
  to disable, or pair with `metricsSecure: true` for mTLS scraping.
- **`cmd/main.go` split into three files** within the same package:
  - `main.go` (334 lines) — flag parsing, manager wiring, runnable
    registration.
  - `config.go` (128 lines) — env-var parsers, warning predicates,
    `buildInitialProvider` helper.
  - `pipeline.go` (110 lines) — `pipelineHandler` (informers→batcher
    bridge) and `leaderMetricRunnable`.
- **`cmd/main.go` no longer redeclares dropping stubs**;
  `droppingIngestClient`, `droppingTopologyClient`, and
  `emptyServicesClient` are exported behaviour of `internal/client`.
- **`Batcher.client` field replaced with `clientFn IngestClientProvider`.**
  Batcher resolves the active client on every flush. Test fixtures
  use a small `ingestFn(c)` helper that wraps a concrete client in a
  closure.
- **`discovery.Loop` and `discovery.Reconciler`** accept
  `TopologyClientProvider` and `ServicesClientProvider` closures
  respectively (same pattern as the Batcher).
- **`IncidentaryConfigReconciler` watches Secrets** via
  `Watches(Secret, EnqueueRequestsFromMapFunc(findConfigsForSecret))`.
  Edits to the referenced Secret immediately enqueue a reconcile of
  every CR that references it.
- **Helm chart deployment template** wires three previously-missing env
  vars: `INCIDENTARY_WORKSPACE_ID`, `INCIDENTARY_MIN_SEVERITY`, and
  `INCIDENTARY_EXCLUDE_NAMESPACES` (the values were documented in
  `values.yaml` but had no effect at runtime).
- **README opens with operator capability** rather than the alpha
  caveat. The status line is demoted to second.

### Fixed

- **Wire format v2 §4.1 — `flushed_at` always populated.** The
  Batcher sets `IngestBatch.FlushedAt = time.Now().UnixNano()` before
  every flush. Batches with `flushed_at: 0` were being rejected as
  `INVALID_TIMESTAMP`.
- **Wire format v2 §7.1 — `occurred_at` fallback to `time.Now()`** in
  every mapper path that may receive an object with a zero timestamp.
  Affected paths: deployment delete events, status-only updates, and
  events whose source object's `metav1.Time` has not been populated.
- **Wire format v2 §17 — attribute value truncation** to spec-mandated
  length limits to prevent oversized payloads from being rejected.
- **Wire format v2 §5.1 — startup warning** when `K8S_CLUSTER_NAME` is
  unset or `"unknown"`. The field is required for `k8s_operator`
  agents; the placeholder is technically compliant but breaks
  cluster-level correlation in the backend.
- **`Event.Attributes` typed as `map[string]any`** so numeric
  attributes (`k8s.exit_code`, `k8s.restart_count`,
  `k8s.hpa.current_replicas`, etc.) serialize as JSON numbers, not
  JSON strings (which the server rejected).
- **Panic recovery in `pipelineHandler.emit`** with goroutine stack
  trace logging. A pathological cluster state can no longer crash the
  operator via a mapper panic.
- **`mapper.Dispatch` nil-resolver guard** panics fast on construction
  rather than nil-derefing on first dispatch.
- **`make lint` clean.** Resolved every `goconst` (test-only string
  literals lifted into constants) and `unparam` (always-the-same
  parameters removed) warning that golangci-lint v2.8 found.
- **Helm chart's `IncidentaryConfig` CR template** now passes
  `workspaceId` so the rendered CR is valid against the updated CRD
  schema.
- **`gofmt` alignment** in `internal/mapper/k8sEventReasonMappings`
  and the deployment-test fixtures.

### Security

- **HTTP/2 disabled by default** on the metrics and webhook servers
  (mitigates GHSA-qppj-fm5r-hxr3 / GHSA-4374-p667-p6c8 — HTTP/2 Stream
  Cancellation and Rapid Reset CVEs). Opt back in with
  `--enable-http2=true`.
- **Distroless nonroot container image** with `runAsNonRoot: true`,
  `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`,
  `seccompProfile: RuntimeDefault`, and all capabilities dropped.
- **Read-only ClusterRole** across all 14 watched resource types
  (informers use list+watch only; no `create`/`update`/`delete` verbs
  except for leader-election leases and the operator's own CRD status).
- **API key stored in a Kubernetes Secret**, referenced by the CR
  via `apiKeySecretRef.{name,key}`. The Helm chart can either create
  the Secret from the `apiKey` value or reference an externally-managed
  Secret via `existingSecretName` / `existingSecretKey`.

### Test coverage

- **Net package coverage (unit + integration):**
  - `internal/batch` — 100.0%
  - `internal/client` — 90.8%
  - `internal/controller` — 72.6%
  - `internal/discovery` — 83.8%
  - `internal/filter` — 100.0%
  - `internal/identity` — 100.0%
  - `internal/ids` — 100.0%
  - `internal/informers` — **79.6%** (was 15.4%)
  - `internal/mapper` — 89.6%
  - `internal/metrics` — 100.0%
  - `internal/wireformat` — 96.2%
  - `cmd` — 25.1%
- **340 tests** pass under `-race`, plus 2 envtest integration tests
  in `internal/informers` (build tag `integration`) and 10 E2E
  assertions in `test/e2e/helm-e2e.sh` (kind cluster + Helm install +
  mock Incidentary backend).

### Internal architecture (no user-visible change)

- **14 read-only informers** registered via
  `internal/informers.WatchSet` (Pods, core Events, events.k8s.io/v1
  Events, Nodes, Services, Namespaces, Deployments, StatefulSets,
  DaemonSets, ReplicaSets, HPAs, Jobs, CronJobs, Ingresses).
- **Identity resolver** walks owner references to derive a stable
  `service_id`: explicit `incidentary.com/service-id` annotation,
  falling back to the workload name, falling back to walking
  `Pod → ReplicaSet → Deployment` (or `→ StatefulSet`/`→ DaemonSet`).
- **Mapper produces wire-format v2 events** for every supported K8s
  state transition. Domain dispatch covered: `K8S_OOM_KILL`,
  `K8S_POD_CRASH`, `K8S_EVICTION`, `K8S_NODE_PRESSURE`, `K8S_HPA_SCALE`,
  `K8S_IMAGE_PULL_FAIL`, `K8S_SCHEDULE_FAIL`, `K8S_POD_STARTED`,
  `K8S_POD_TERMINATED`, `K8S_DEPLOY_ROLLOUT`, and the five `DEPLOY_*`
  kinds.
- **Severity filter** (`internal/filter`) drops Tier-2 events below the
  configured threshold; Tier-1 events (crashes, OOMs, failures, deploys)
  always pass per Philosophy 1.
- **Topology reporter** (`internal/discovery.Loop`) scans the cluster
  every 5 minutes (configurable via
  `reconciliationIntervalSeconds`) and POSTs a `TopologyReport` to
  `/api/v2/workspace/topology` with workload name, kind, namespace,
  image, replicas, and conditions.
- **Services reconciler** (`internal/discovery.Reconciler`) polls
  `/api/v2/workspace/services` and classifies each watched workload
  as `matched` / `ghost` / `mismatched` / `new` (with Levenshtein-based
  fuzzy match for typo detection). Counts surface in the
  `IncidentaryConfig` CR `.status` block.
- **HA leader election** via controller-runtime
  (`LeaderElectionID = "140b099f.incidentary.com"`). Event processing,
  topology reporting, and services reconciliation only run on the
  elected leader; the operator ships with `replicaCount: 2` and a PDB
  for hot-standby failover.
- **`agent.id` generation** uses UUIDv4
  (reverted from UUIDv7 in commit `0db1af1` for compatibility with
  the v2 ingest validator).
- **13 Prometheus metrics** registered with the controller-runtime
  registry: `events_sent_total`, `events_dropped_total`,
  `events_filtered_total`, `flush_latency_seconds`,
  `flush_batch_size`, `informer_cache_size`, `leader_is_leader`, plus
  the discovery / reconciler gauges.

<!-- Compare links will be added once the first version tag is cut. -->

