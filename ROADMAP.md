# Roadmap

This file tracks the post-GA roadmap for `incidentary-operator`.
`v0.1.1` is the GA cut; `v0.1.0` was the pre-GA preview tag and remains
in git history.

The operator does not promise dates. It promises ordering: each release
cuts only after the previous one's measured SLOs hold and the next
feature's tests are green.

## v0.1.1 — GA (current)

Tag: `v0.1.1`. Helm chart `oci://ghcr.io/incidentary/operator/charts/incidentary-operator`.

> **Why `v0.1.1` and not `v0.1.0`?** A pre-GA preview was published as
> `v0.1.0` on 2026-04-28 ("first tagged preview"). The GA quality bar
> (the SLOs in [OPERATIONS.md](./OPERATIONS.md), the runtime API-key
> rotation contract, the cosign supply-chain story) was finalised
> after that preview. Rather than rewrite public git history, the
> first tag that meets the GA bar is `v0.1.1`. From this point forward
> the chart and image versions move in lockstep with the operator's
> semver.

What ships:

- Read-only watches on 14 K8s resource types.
- Mapping to Incidentary Wire Format v2 (`agent.type = k8s_operator`,
  `agent.surface = "operator"`).
- Topology auto-discovery + ghost-service classification.
- HA via 2 replicas + leader election; only the active leader processes
  events.
- Helm chart with `apiKey`-required validation, NetworkPolicy template
  (off by default), ServiceMonitor template (off by default).
- Runtime API-key rotation (no pod restart).
- Published operational SLOs (see [OPERATIONS.md](./OPERATIONS.md)).
- CVE response policy (see [SECURITY.md](./SECURITY.md)).

## v0.2.0 — mTLS for backend ingest (target: ~1 quarter post-GA)

What ships:

- Mutual-TLS between the operator and the Incidentary backend, anchored
  on a customer-supplied CA bundle.
- Optional client certificate via `tls.client.cert` / `tls.client.key`
  in the IncidentaryConfig CR (referenced as Secret keys, rotated in the
  same way as the API key).
- Helm chart values: `tls.enabled`, `tls.caBundleSecretName`,
  `tls.clientCertSecretName`.
- Backwards-compatible: TLS is opt-in; default behavior unchanged from
  v0.1.1.

Why: enterprise customers running zero-trust networking want client-cert
authentication on every outbound request, not just bearer tokens.
v0.1.1's bearer-token authentication remains the default.

## v0.3.0 — incident-domain mapper integration (target: ~2 quarters post-GA)

What ships:

- Receivers for incoming PagerDuty / OpsGenie webhooks at the operator
  layer (currently the backend handles these directly; the operator
  receiver gives K8s-native customers a single ingress point).
- Mapping to `INCIDENT_TRIGGERED` / `INCIDENT_ACKNOWLEDGED` /
  `INCIDENT_RESOLVED` / `INCIDENT_ESCALATED` / `INCIDENT_ANNOTATED` CEs.
- Optional: enrich incident events with K8s context (the affected
  service's recent K8S_OOM_KILL / K8S_DEPLOY_ROLLOUT events) at the
  operator before forwarding.

Why: closes the loop on the "incident layer for any OTel stack" framing
— the operator becomes the single ingress for both K8s ground-truth
events and incident-management webhooks.

## Considered, not committed

The following are not on the roadmap until a real customer-pull signal:

- OTel auto-instrumentation co-deployment patterns (alongside
  `open-telemetry/opentelemetry-operator`).
- Multi-cluster topology aggregation (single operator instance watching
  multiple clusters via service-account-impersonation tokens).
- Built-in eBPF profile collection (depends on OTel Profiles signal
  graduating from alpha).

## Cadence and version policy

- **Patch releases (`v0.1.x`)**: bug fixes, dependency CVE patches.
  Patched within the SECURITY.md response window for HIGH/CRITICAL CVEs.
- **Minor releases (`v0.2.0`, `v0.3.0`, ...)**: additive features. No
  breaking CRD or Helm-values changes.
- **Major releases (`v1.0.0`)**: reserved for the post-stability
  graduation that promotes the CRD to v1 and removes deprecated
  values.yaml fields (none yet).

The operator follows semver. Anything that would break an existing
`helm install` or `kubectl apply` is a major-version bump, not a minor.
