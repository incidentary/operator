# Security policy

This file documents the security posture of `incidentary-operator` and
the process for reporting vulnerabilities.

## Reporting a vulnerability

**Email:** `security@incidentary.com` (PGP key on request).

Please include:

- A description of the vulnerability and the conditions under which it
  triggers.
- The minimum operator version + Kubernetes version + Helm chart values
  you observed it on.
- Any proof-of-concept code, exploit script, or reproduction steps. We
  prefer minimal repros that don't require running a full production
  cluster.

We acknowledge every report within 3 business days. We do not run a
public bug bounty; valid reports are credited in the relevant release
note unless the reporter requests otherwise.

**Do not** open a public GitHub issue for a security report.

## Response policy

| Severity | Patch target | Notes |
|---|---|---|
| **CRITICAL** | within 7 days | Includes CVSS 9.0+ in either the operator container image or the operator's own code. |
| **HIGH** | within 14 days | Includes CVSS 7.0–8.9. The release workflow's Trivy gate enforces this for image CVEs (see [.github/workflows/release.yml](./.github/workflows/release.yml)). |
| **MEDIUM** | next minor release | CVSS 4.0–6.9. |
| **LOW** | next major release | CVSS < 4.0. |

The Trivy CVE gate in the release workflow blocks publication of any
new image tag whose Trivy scan reports HIGH or CRITICAL severity issues
for fixed packages (`ignore-unfixed: true` — we don't block on a CVE the
upstream cannot yet patch).

## Threat model

### What the operator can do

The operator runs in a Kubernetes cluster with the following authority:

- **Cluster-wide read** of: Pods, Events (core + events.k8s.io/v1),
  Nodes, Services, Namespaces, Deployments, StatefulSets, DaemonSets,
  ReplicaSets, HorizontalPodAutoscalers, Jobs, CronJobs, Ingresses.
- **Namespaced read+create+update** of: Leases (its own leader-election
  lease in the operator namespace).
- **CRD read+update** of: `IncidentaryConfig` and its status subresource.
- **Outbound HTTPS** to the configured Incidentary backend (default
  `api.incidentary.com:443`) carrying batches of K8s event metadata
  signed with a per-workspace bearer token.

The operator **cannot**:

- Mutate any Kubernetes resource other than its own CRD status and its
  own leader-election lease.
- Read Secret data other than the explicitly-referenced API-key Secret.
- Open inbound connections except `:8080/metrics` and `:8081/{healthz,readyz}`.
- Execute commands inside cluster pods, exec into containers, or attach
  to logs.

### What the operator must protect

- The API key (a workspace-bearer token) in the referenced Secret. The
  operator reads it once per reconcile, hot-swaps the in-memory clients,
  and never persists it outside Kubernetes.
- The integrity of outbound batches. The operator does not sign batches
  beyond the bearer token; transport security is plain TLS. mTLS is
  scheduled for v0.2.0 (see ROADMAP.md).

### Trust boundaries

| Boundary | Crossed by | Authentication |
|---|---|---|
| Cluster ↔ kube-apiserver | every watch, every reconcile | ServiceAccount token (mounted via projected volume) |
| Operator pod ↔ Incidentary backend | every batch flush, every topology report | Bearer token from the API-key Secret |
| Prometheus scraper ↔ operator metrics | scrape | Plain HTTP by default; mTLS-capable via `metricsSecure: true` |

### What the operator does NOT trust

- The IncidentaryConfig CR's `spec.workspaceId`: rejected at CRD
  validation if empty (`MinLength=1`); rejected at `helm install` if
  missing while `apiKey` is set.
- The API-key Secret's data: if the referenced Secret is deleted or its
  data is empty, the controller installs dropping stubs (the operator
  keeps watching but stops emitting). It does NOT crash, retry-storm,
  or fall back to a previous key.
- The Incidentary backend's response: 4xx responses other than 429 are
  treated as permanent (no retry); 429 / 5xx / network errors retry
  with exponential backoff to a 60-second window.

## RBAC posture

The full RBAC surface is in
[charts/incidentary-operator/templates/clusterrole.yaml](./charts/incidentary-operator/templates/clusterrole.yaml)
and [role.yaml](./charts/incidentary-operator/templates/role.yaml).

ClusterRole verbs: `get`, `list`, `watch` only. Never `create`, `update`,
`patch`, or `delete` on any cluster-scoped resource the operator does
not own.

Role (namespaced) verbs: `get`, `create`, `update`, `patch` on Leases
(leader election) and the IncidentaryConfig status subresource.

The README's [Configuration reference](./README.md#configuration-reference)
exposes the only knobs that affect runtime behavior; none of them change
the RBAC posture.

## Supply chain

- All container images are built reproducibly from the Dockerfile in
  this repo via Docker BuildKit.
- All images are pushed to `ghcr.io/incidentary/operator` and signed
  with cosign keyless via GitHub OIDC at release time.
- The Helm chart is signed with cosign at the same time as the image.
- Dependencies are tracked in `go.mod` / `go.sum` and audited by
  Dependabot weekly.

Verify a published image:

```bash
cosign verify ghcr.io/incidentary/operator:v0.1.1 \
  --certificate-identity-regexp 'https://github.com/incidentary/operator/.+' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

Verify a published chart:

```bash
cosign verify ghcr.io/incidentary/operator/charts/incidentary-operator:0.1.1 \
  --certificate-identity-regexp 'https://github.com/incidentary/operator/.+' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com'
```

## Network policy

The chart ships an optional NetworkPolicy template (off by default). When
enabled (`networkPolicy.enabled: true`), the operator pod is restricted
to:

- **Egress:** DNS (CoreDNS), Kubernetes API server, configurable
  Incidentary backend hostnames. Set `networkPolicy.egressTo` to narrow
  the allow-list to a specific CIDR.
- **Ingress:** none, unless `networkPolicy.allowPrometheusScrape: true`
  (then port 8080 is open to the configured Prometheus pod).

The NetworkPolicy is only enforced on clusters running a
NetworkPolicy-aware CNI (Calico, Cilium, Antrea, etc.). On other CNIs
the policy is silently ignored.

## Security contact

Email: `security@incidentary.com`.
