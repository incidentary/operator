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

package wireformat_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/incidentary/operator/internal/wireformat"
)

func TestKindDomain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		kind wireformat.Kind
		want wireformat.Domain
	}{
		// Infrastructure — K8S_*
		{"K8S_POD_CRASH", wireformat.KindK8sPodCrash, wireformat.DomainInfrastructure},
		{"K8S_OOM_KILL", wireformat.KindK8sOOMKill, wireformat.DomainInfrastructure},
		{"K8S_EVICTION", wireformat.KindK8sEviction, wireformat.DomainInfrastructure},
		{"K8S_SCHEDULE_FAIL", wireformat.KindK8sScheduleFail, wireformat.DomainInfrastructure},
		{"K8S_IMAGE_PULL_FAIL", wireformat.KindK8sImagePullFail, wireformat.DomainInfrastructure},
		{"K8S_NODE_PRESSURE", wireformat.KindK8sNodePressure, wireformat.DomainInfrastructure},
		{"K8S_HPA_SCALE", wireformat.KindK8sHPAScale, wireformat.DomainInfrastructure},
		{"K8S_POD_STARTED", wireformat.KindK8sPodStarted, wireformat.DomainInfrastructure},
		{"K8S_POD_TERMINATED", wireformat.KindK8sPodTerminated, wireformat.DomainInfrastructure},
		{"K8S_DEPLOY_ROLLOUT", wireformat.KindK8sDeployRollout, wireformat.DomainInfrastructure},

		// Deployment — DEPLOY_*
		{"DEPLOY_STARTED", wireformat.KindDeployStarted, wireformat.DomainDeployment},
		{"DEPLOY_SUCCEEDED", wireformat.KindDeploySucceeded, wireformat.DomainDeployment},
		{"DEPLOY_FAILED", wireformat.KindDeployFailed, wireformat.DomainDeployment},
		{"DEPLOY_CANCELLED", wireformat.KindDeployCancelled, wireformat.DomainDeployment},
		{"DEPLOY_ROLLED_BACK", wireformat.KindDeployRolledBack, wireformat.DomainDeployment},

		// Cloud infrastructure
		{"CLOUD_INSTANCE_CHANGE", wireformat.Kind("CLOUD_INSTANCE_CHANGE"), wireformat.DomainInfrastructure},

		// Incident
		{"INCIDENT_TRIGGERED", wireformat.Kind("INCIDENT_TRIGGERED"), wireformat.DomainIncident},

		// Application fallback for unknown prefixes
		{"HTTP_SERVER", wireformat.Kind("HTTP_SERVER"), wireformat.DomainApplication},
		{"CUSTOM", wireformat.Kind("CUSTOM"), wireformat.DomainApplication},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.kind.Domain()
			if got != tc.want {
				t.Fatalf("Kind(%q).Domain() = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

func TestTimeToUnixNano(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   time.Time
		want int64
	}{
		{"zero time maps to 0", time.Time{}, 0},
		{"epoch", time.Unix(0, 0).UTC().Add(0), 0}, // epoch is zero unix-ns but NOT a zero time
		{"specific time", time.Unix(1733103000, 1).UTC(), 1733103000*int64(time.Second) + 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := wireformat.TimeToUnixNano(tc.in)
			if got != tc.want {
				t.Fatalf("TimeToUnixNano(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestIngestBatchJSON(t *testing.T) {
	t.Parallel()

	batch := wireformat.IngestBatch{
		SpecVersion: wireformat.SpecVersion,
		Resource: wireformat.Resource{
			Attributes: map[string]string{
				"k8s.cluster.name": "prod-ams3",
			},
		},
		Agent: wireformat.Agent{
			Type:        wireformat.AgentTypeK8sOperator,
			Version:     "0.1.0",
			WorkspaceID: "org_test",
		},
		CaptureMode: wireformat.CaptureModeSkeleton,
		FlushedAt:   1733103000000000000,
		Events: []wireformat.Event{
			{
				ID:         "550e8400-e29b-41d4-a716-446655440000",
				Kind:       wireformat.KindK8sPodCrash,
				Severity:   wireformat.SeverityError,
				OccurredAt: 1733103000000000000,
				ServiceID:  "payment-service",
				Attributes: map[string]any{
					"k8s.pod.name":  "payment-service-7d9f6b-xkp2m",
					"k8s.reason":    "BackOff",
					"k8s.exit_code": 137,
				},
			},
		},
	}

	raw, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// Decode into a generic map to assert exact spec key names without
	// coupling the test to Go struct ordering.
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	for _, key := range []string{"specversion", "resource", "agent", "capture_mode", "flushed_at", "events"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("batch JSON missing key %q: %s", key, raw)
		}
	}

	if decoded["specversion"] != "2" {
		t.Fatalf("specversion = %v, want \"2\"", decoded["specversion"])
	}
	if decoded["capture_mode"] != "SKELETON" {
		t.Fatalf("capture_mode = %v, want \"SKELETON\"", decoded["capture_mode"])
	}

	// Resource must flatten to a plain object — not wrapped in an "attributes" key.
	resource, ok := decoded["resource"].(map[string]any)
	if !ok {
		t.Fatalf("resource is not an object: %T", decoded["resource"])
	}
	if resource["k8s.cluster.name"] != "prod-ams3" {
		t.Fatalf("resource.k8s.cluster.name = %v, want \"prod-ams3\"", resource["k8s.cluster.name"])
	}
	if _, wrapped := resource["attributes"]; wrapped {
		t.Fatalf("resource must be a flat object, not wrapped in \"attributes\"")
	}

	agent, ok := decoded["agent"].(map[string]any)
	if !ok {
		t.Fatalf("agent is not an object: %T", decoded["agent"])
	}
	if agent["type"] != "k8s_operator" {
		t.Fatalf("agent.type = %v, want \"k8s_operator\"", agent["type"])
	}
	if agent["workspace_id"] != "org_test" {
		t.Fatalf("agent.workspace_id = %v, want \"org_test\"", agent["workspace_id"])
	}

	events, ok := decoded["events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("events is not an array of length 1: %T", decoded["events"])
	}
	ev, ok := events[0].(map[string]any)
	if !ok {
		t.Fatalf("event[0] is not an object: %T", events[0])
	}
	for _, key := range []string{"id", "kind", "severity", "occurred_at", "service_id", "attributes"} {
		if _, present := ev[key]; !present {
			t.Fatalf("event JSON missing key %q: %v", key, ev)
		}
	}
	// The operator never emits trace_id, span_id, parent_id, status_code,
	// duration_ns, detail — enforce their absence.
	for _, forbidden := range []string{"trace_id", "span_id", "parent_id", "status_code", "duration_ns", "detail"} {
		if _, present := ev[forbidden]; present {
			t.Fatalf("event JSON must not contain %q: %v", forbidden, ev)
		}
	}
	if ev["kind"] != "K8S_POD_CRASH" {
		t.Fatalf("event.kind = %v, want \"K8S_POD_CRASH\"", ev["kind"])
	}
	if ev["severity"] != "error" {
		t.Fatalf("event.severity = %v, want \"error\"", ev["severity"])
	}
}

func TestSeriesJSONShape(t *testing.T) {
	t.Parallel()

	ev := wireformat.Event{
		ID:         "id-1",
		Kind:       wireformat.KindK8sPodCrash,
		Severity:   wireformat.SeverityError,
		OccurredAt: 1733100600000000000,
		ServiceID:  "svc",
		Series: &wireformat.Series{
			Count:   47,
			FirstAt: 1733100600000000000,
			LastAt:  1733101920654321000,
		},
	}

	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	series, ok := decoded["series"].(map[string]any)
	if !ok {
		t.Fatalf("series is not an object: %T", decoded["series"])
	}
	// JSON numbers decode to float64.
	if series["count"].(float64) != 47 {
		t.Fatalf("series.count = %v, want 47", series["count"])
	}
	// occurred_at must equal first_at per blunder 3 fix.
	if series["first_at"].(float64) != decoded["occurred_at"].(float64) {
		t.Fatalf("series.first_at (%v) must equal occurred_at (%v)", series["first_at"], decoded["occurred_at"])
	}
}

func TestResourceRoundtrip(t *testing.T) {
	t.Parallel()

	r := wireformat.Resource{
		Attributes: map[string]string{
			"k8s.cluster.name":   "test-cluster",
			"k8s.namespace.name": "default",
		},
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var back wireformat.Resource
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if back.Attributes["k8s.cluster.name"] != "test-cluster" {
		t.Fatalf("round-trip lost k8s.cluster.name: %v", back.Attributes)
	}
	if back.Attributes["k8s.namespace.name"] != "default" {
		t.Fatalf("round-trip lost k8s.namespace.name: %v", back.Attributes)
	}
}

// TestAttributeNumericSerialization verifies that numeric attribute values
// (k8s.exit_code, k8s.restart_count, k8s.hpa.*_replicas per spec §13) are
// emitted as JSON numbers, not JSON strings. Compile-time RED if
// Event.Attributes is map[string]string.
func TestAttributeNumericSerialization(t *testing.T) {
	t.Parallel()

	ev := wireformat.Event{
		ID:         "id-num",
		Kind:       wireformat.KindK8sOOMKill,
		Severity:   wireformat.SeverityError,
		OccurredAt: 1733100600000000000,
		ServiceID:  "svc",
		Attributes: map[string]any{
			"k8s.exit_code":     137,
			"k8s.restart_count": 5,
			"k8s.pod.name":      "pod-abc",
		},
	}

	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	attrs, ok := decoded["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("attributes is not an object: %T", decoded["attributes"])
	}

	// exit_code must be a JSON number (float64 after decode), not a string.
	exitCode, ok := attrs["k8s.exit_code"].(float64)
	if !ok {
		t.Fatalf("k8s.exit_code type = %T (%v), want float64 (JSON number)", attrs["k8s.exit_code"], attrs["k8s.exit_code"])
	}
	if exitCode != 137 {
		t.Fatalf("k8s.exit_code = %v, want 137", exitCode)
	}

	restartCount, ok := attrs["k8s.restart_count"].(float64)
	if !ok {
		t.Fatalf("k8s.restart_count type = %T (%v), want float64 (JSON number)", attrs["k8s.restart_count"], attrs["k8s.restart_count"])
	}
	if restartCount != 5 {
		t.Fatalf("k8s.restart_count = %v, want 5", restartCount)
	}

	// String attribute must still work.
	podName, ok := attrs["k8s.pod.name"].(string)
	if !ok || podName != "pod-abc" {
		t.Fatalf("k8s.pod.name = %v (%T), want string \"pod-abc\"", attrs["k8s.pod.name"], attrs["k8s.pod.name"])
	}
}

func TestResourceMarshalNilAttributes(t *testing.T) {
	t.Parallel()

	var r wireformat.Resource
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(raw) != "{}" {
		t.Fatalf("nil Attributes should marshal to {}, got %s", raw)
	}
}

func TestResourceUnmarshalJSONNull(t *testing.T) {
	t.Parallel()
	// JSON null should unmarshal to an empty (not nil) Attributes map.
	var r wireformat.Resource
	if err := json.Unmarshal([]byte("null"), &r); err != nil {
		t.Fatalf("UnmarshalJSON(null): %v", err)
	}
	if r.Attributes == nil {
		t.Error("Attributes should be non-nil after null unmarshal")
	}
}

func TestResourceUnmarshalJSONInvalid(t *testing.T) {
	t.Parallel()
	// Invalid JSON must propagate the error.
	var r wireformat.Resource
	if err := json.Unmarshal([]byte("{bad json}"), &r); err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

// TestResourceUnmarshalJSONWrongType covers the internal error-return path in
// unmarshalFlatStringMap and Resource.UnmarshalJSON: the input is valid JSON
// (an array) so json.Unmarshal calls UnmarshalJSON, but the inner unmarshal
// into map[string]string fails because arrays cannot be decoded as maps.
func TestResourceUnmarshalJSONWrongType(t *testing.T) {
	t.Parallel()
	var r wireformat.Resource
	if err := json.Unmarshal([]byte(`[1, 2, 3]`), &r); err == nil {
		t.Error("expected error when unmarshaling a JSON array into Resource, got nil")
	}
}
