/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package wireformat defines the Incidentary Wire Format v2 types used by
// the operator to construct IngestBatch payloads.
//
// The operator only produces SKELETON-mode batches for infrastructure and
// deployment-domain events. Detail blocks and application-domain fields
// (trace_id, span_id, parent_id, status_code, duration_ns) are intentionally
// absent — the server and wire-format validator reject operator batches that
// contain them.
//
// Canonical reference: /home/ahmed/dev/incidentary-wire-format/spec/wire-format-v2.md
package wireformat

import (
	"strings"
	"time"
)

// SpecVersion is the only value permitted in IngestBatch.SpecVersion for the
// operator. The server returns HTTP 422 for any other value.
const SpecVersion = "2"

// CaptureMode enum. Operator always sends CaptureModeSkeleton — it never
// captures rich operational context (no detail block).
type CaptureMode string

const (
	CaptureModeSkeleton CaptureMode = "SKELETON"
	CaptureModeFull     CaptureMode = "FULL"
)

// AgentType enum. Operator always sends AgentTypeK8sOperator.
type AgentType string

const (
	AgentTypeK8sOperator AgentType = "k8s_operator"
)

// Severity enum — exact string values from wire-format-v2.md section 7.2.
type Severity string

const (
	SeverityTrace   Severity = "trace"
	SeverityDebug   Severity = "debug"
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
	SeverityFatal   Severity = "fatal"
)

// Kind is the v2 event kind. The operator produces only infrastructure and
// deployment-domain kinds. Application, incident, and custom kinds are
// intentionally omitted from this package.
type Kind string

const (
	// Infrastructure domain — K8S_* prefix.
	KindK8sPodCrash      Kind = "K8S_POD_CRASH"
	KindK8sOOMKill       Kind = "K8S_OOM_KILL"
	KindK8sEviction      Kind = "K8S_EVICTION"
	KindK8sScheduleFail  Kind = "K8S_SCHEDULE_FAIL"
	KindK8sImagePullFail Kind = "K8S_IMAGE_PULL_FAIL"
	KindK8sNodePressure  Kind = "K8S_NODE_PRESSURE"
	KindK8sHPAScale      Kind = "K8S_HPA_SCALE"
	KindK8sPodStarted    Kind = "K8S_POD_STARTED"
	KindK8sPodTerminated Kind = "K8S_POD_TERMINATED"
	KindK8sDeployRollout Kind = "K8S_DEPLOY_ROLLOUT"

	// Deployment domain — DEPLOY_* prefix.
	KindDeployStarted    Kind = "DEPLOY_STARTED"
	KindDeploySucceeded  Kind = "DEPLOY_SUCCEEDED"
	KindDeployFailed     Kind = "DEPLOY_FAILED"
	KindDeployCancelled  Kind = "DEPLOY_CANCELLED"
	KindDeployRolledBack Kind = "DEPLOY_ROLLED_BACK"
)

// Domain categorizes a Kind for routing and validation. Used by the severity
// filter's DEPLOY_* always-pass rule. The domain is not a wire format field;
// it is an internal server-side classification. This function is the operator's
// local copy of the same logic so that the filter can reason about domains
// without a round-trip to the server.
type Domain string

const (
	DomainApplication    Domain = "application"
	DomainInfrastructure Domain = "infrastructure"
	DomainDeployment     Domain = "deployment"
	DomainIncident       Domain = "incident"
)

// Domain returns the Domain for the Kind. The operator only produces
// infrastructure and deployment kinds; any other kind (hypothetical or
// future) falls into DomainApplication as the last-resort default.
func (k Kind) Domain() Domain {
	s := string(k)
	switch {
	case strings.HasPrefix(s, "K8S_") || strings.HasPrefix(s, "CLOUD_"):
		return DomainInfrastructure
	case strings.HasPrefix(s, "DEPLOY_"):
		return DomainDeployment
	case strings.HasPrefix(s, "INCIDENT_"):
		return DomainIncident
	default:
		return DomainApplication
	}
}

// IngestBatch is the top-level v2 payload envelope. Matches the JSON shape
// described in wire-format-v2.md section 4.
type IngestBatch struct {
	SpecVersion string      `json:"specversion"`
	Resource    Resource    `json:"resource"`
	Agent       Agent       `json:"agent"`
	CaptureMode CaptureMode `json:"capture_mode"`
	FlushedAt   int64       `json:"flushed_at"`
	Events      []Event     `json:"events"`
}

// Resource identifies where the batch originated. For the operator, this is
// the operator instance — not the services being observed. Per-event
// ServiceID identifies the observed service.
//
// Keys follow OpenTelemetry semantic conventions (dot-notation). The operator
// populates at minimum k8s.cluster.name (required for k8s_operator agents,
// per section 5.1).
type Resource struct {
	Attributes map[string]string `json:"attributes"`
}

// MarshalJSON flattens Resource to a plain JSON object so the wire
// representation matches the spec — `"resource": { "k8s.cluster.name": "..." }`
// rather than `"resource": { "attributes": { ... } }`.
func (r Resource) MarshalJSON() ([]byte, error) {
	return marshalFlatStringMap(r.Attributes)
}

// UnmarshalJSON hydrates Resource from the flat JSON object representation.
func (r *Resource) UnmarshalJSON(data []byte) error {
	attrs, err := unmarshalFlatStringMap(data)
	if err != nil {
		return err
	}
	r.Attributes = attrs
	return nil
}

// Agent identifies the sending agent. Operator always sets Type to
// AgentTypeK8sOperator.
type Agent struct {
	Type        AgentType       `json:"type"`
	Version     string          `json:"version"`
	WorkspaceID string          `json:"workspace_id"`
	Telemetry   *AgentTelemetry `json:"telemetry,omitempty"`
}

// AgentTelemetry reports optional agent self-metrics. All fields are optional.
type AgentTelemetry struct {
	QueueDepth     int64 `json:"queue_depth,omitempty"`
	DroppedCECount int64 `json:"dropped_ce_count,omitempty"`
	FlushLatencyMs int64 `json:"flush_latency_ms,omitempty"`
	RingBufferSize int64 `json:"ring_buffer_size,omitempty"`
}

// Event is a single causal event. The operator only produces infrastructure
// and deployment-domain events, so TraceID, SpanID, ParentID, StatusCode,
// DurationNs, and Detail are all absent from this struct.
//
// Wire-format mapping: wire-format-v2.md section 7.
type Event struct {
	// ID is a UUID string (RFC 9562) uniquely identifying this event.
	// Used for deduplication on retry. The operator emits UUIDv7 via
	// the ids.NewID() helper for time-ordered sort-key locality; the
	// wire format accepts v1/v4/v7 transparently.
	ID string `json:"id"`

	// Kind is the structural category of the event.
	Kind Kind `json:"kind"`

	// Severity is the operator-assigned severity level.
	Severity Severity `json:"severity"`

	// OccurredAt is the event time in Unix nanoseconds. For series events,
	// this must equal Series.FirstAt (per spec blunder 3 fix).
	OccurredAt int64 `json:"occurred_at"`

	// ServiceID is the service this event is about — required for
	// k8s_operator agents on infrastructure-domain kinds.
	ServiceID string `json:"service_id"`

	// Attributes is the per-kind structured metadata map. OTel semantic
	// convention keys; string values only (per wire-format-v2.md section 13.1).
	Attributes map[string]string `json:"attributes,omitempty"`

	// Series holds deduplication metadata for K8s events with count > 1.
	// Nil when the event occurred once.
	Series *Series `json:"series,omitempty"`
}

// Series holds deduplication metadata for K8s events with count > 1.
// See wire-format-v2.md section 11.
//
// Per spec:
//   - Count must be >= 2 when Series is present.
//   - FirstAt must equal the parent Event.OccurredAt.
//   - LastAt must be >= FirstAt.
type Series struct {
	Count   int64 `json:"count"`
	FirstAt int64 `json:"first_at"`
	LastAt  int64 `json:"last_at"`
}

// TimeToUnixNano converts a Go time.Time to the wire format's int64
// Unix-nanoseconds representation. The zero time maps to 0.
func TimeToUnixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}
