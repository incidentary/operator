/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package mapper

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/incidentary/operator/internal/identity"
	"github.com/incidentary/operator/internal/wireformat"
)

// newTestMapper constructs a Mapper with a fake cluster client containing
// the given objects.
func newTestMapper(t *testing.T, objs ...runtime.Object) *Mapper {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("appsv1 scheme: %v", err)
	}
	if err := autoscalingv2.AddToScheme(scheme); err != nil {
		t.Fatalf("autoscalingv2 scheme: %v", err)
	}
	if err := eventsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("eventsv1 scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	return NewMapper(identity.NewResolver(c))
}

// -----------------------------------------------------------------------------
// FromK8sEvent / FromCoreEvent — reason mapping table.
// -----------------------------------------------------------------------------

func TestFromK8sEvent_ReasonMappings(t *testing.T) {
	cases := []struct {
		name       string
		reason     string
		kind       string // involvedObject.kind
		wantKind   wireformat.Kind
		wantMapped bool
	}{
		{"oom on pod", "OOMKilling", "Pod", wireformat.KindK8sOOMKill, true},
		{"backoff on pod", "BackOff", "Pod", wireformat.KindK8sPodCrash, true},
		{"evicted on pod", "Evicted", "Pod", wireformat.KindK8sEviction, true},
		{"failed scheduling on pod", "FailedScheduling", "Pod", wireformat.KindK8sScheduleFail, true},
		{"image pull fail on pod", "ErrImagePull", "Pod", wireformat.KindK8sImagePullFail, true},
		{"image pull backoff on pod", "ImagePullBackOff", "Pod", wireformat.KindK8sImagePullFail, true},

		// Negative cases — SAME reason on WRONG involvedObject.kind must NOT map.
		// This is the critical correctness rule from the phase 3 plan.
		{"backoff on deployment rejected", "BackOff", "Deployment", "", false},
		{"evicted on node rejected", "Evicted", "Node", "", false},
		{"failed scheduling on replicaset rejected", "FailedScheduling", "ReplicaSet", "", false},
		{"unknown reason rejected", "ThisReasonDoesNotExist", "Pod", "", false},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payment-abc", Namespace: "prod"},
	}
	m := newTestMapper(t, pod)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := &eventsv1.Event{
				Reason: tc.reason,
				Regarding: corev1.ObjectReference{
					Kind: tc.kind, Namespace: "prod", Name: "payment-abc",
				},
				EventTime: metav1.MicroTime{Time: time.Now()},
			}
			out, ok, err := m.FromK8sEvent(context.Background(), ev)
			if err != nil {
				t.Fatalf("FromK8sEvent err: %v", err)
			}
			if ok != tc.wantMapped {
				t.Fatalf("mapped=%v, want %v", ok, tc.wantMapped)
			}
			if !ok {
				return
			}
			if out.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", out.Kind, tc.wantKind)
			}
			if out.ID == "" {
				t.Error("id should be populated")
			}
			if out.OccurredAt == 0 {
				t.Error("occurred_at should be populated")
			}
			if out.Attributes["k8s.reason"] != tc.reason {
				t.Errorf("k8s.reason attribute missing")
			}
			if out.ServiceID == "" {
				t.Error("service_id should be populated (fallback to pod name)")
			}
		})
	}
}

func TestFromK8sEvent_SeriesDeduplication(t *testing.T) {
	firstTime := time.Now().Add(-30 * time.Second)
	lastTime := time.Now()
	ev := &eventsv1.Event{
		Reason:    "OOMKilling",
		Regarding: corev1.ObjectReference{Kind: "Pod", Namespace: "prod", Name: "payment-abc"},
		EventTime: metav1.MicroTime{Time: firstTime},
		Series: &eventsv1.EventSeries{
			Count:            5,
			LastObservedTime: metav1.MicroTime{Time: lastTime},
		},
	}
	m := newTestMapper(t)
	out, ok, err := m.FromK8sEvent(context.Background(), ev)
	if err != nil || !ok {
		t.Fatalf("FromK8sEvent: ok=%v err=%v", ok, err)
	}
	if out.Series == nil {
		t.Fatal("series should be populated when count > 1")
	}
	if out.Series.Count != 5 {
		t.Errorf("series.count = %d, want 5", out.Series.Count)
	}
	// Blunder 3 fix: occurred_at equals series.first_at.
	if out.OccurredAt != out.Series.FirstAt {
		t.Errorf("occurred_at (%d) != series.first_at (%d)", out.OccurredAt, out.Series.FirstAt)
	}
	if out.Series.LastAt < out.Series.FirstAt {
		t.Errorf("series.last_at (%d) < first_at (%d)", out.Series.LastAt, out.Series.FirstAt)
	}
}

func TestFromK8sEvent_NoSeriesWhenCountOne(t *testing.T) {
	ev := &eventsv1.Event{
		Reason:    "OOMKilling",
		Regarding: corev1.ObjectReference{Kind: "Pod", Namespace: "prod", Name: "payment-abc"},
		EventTime: metav1.MicroTime{Time: time.Now()},
		Series:    &eventsv1.EventSeries{Count: 1},
	}
	m := newTestMapper(t)
	out, ok, err := m.FromK8sEvent(context.Background(), ev)
	if err != nil || !ok {
		t.Fatalf("FromK8sEvent: ok=%v err=%v", ok, err)
	}
	if out.Series != nil {
		t.Error("series should be nil when count == 1")
	}
}

func TestFromCoreEvent_NilReturnsNotOK(t *testing.T) {
	m := newTestMapper(t)
	_, ok, err := m.FromCoreEvent(context.Background(), nil)
	if err != nil || ok {
		t.Errorf("nil event: ok=%v err=%v; want ok=false, err=nil", ok, err)
	}
}

func TestFromCoreEvent_EmptyReasonReturnsNotOK(t *testing.T) {
	m := newTestMapper(t)
	ev := &corev1.Event{InvolvedObject: corev1.ObjectReference{Kind: "Pod"}}
	_, ok, err := m.FromCoreEvent(context.Background(), ev)
	if err != nil || ok {
		t.Errorf("empty reason: ok=%v err=%v; want ok=false, err=nil", ok, err)
	}
}

func TestFromCoreEvent_UnknownReasonReturnsNotOK(t *testing.T) {
	m := newTestMapper(t)
	ev := &corev1.Event{
		Reason:         "SomethingUnknown",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod"},
	}
	_, ok, err := m.FromCoreEvent(context.Background(), ev)
	if err != nil || ok {
		t.Errorf("unknown reason: ok=%v err=%v; want ok=false, err=nil", ok, err)
	}
}

func TestFromCoreEvent_WithSourceAttrs(t *testing.T) {
	// Source.Component and Source.Host must appear as attributes.
	ev := &corev1.Event{
		Reason: "BackOff",
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: "prod", Name: "web-1",
		},
		Source: corev1.EventSource{
			Component: "kubelet",
			Host:      "worker-1",
		},
		FirstTimestamp: metav1.Time{Time: time.Now()},
	}
	m := newTestMapper(t)
	out, ok, err := m.FromCoreEvent(context.Background(), ev)
	if err != nil || !ok {
		t.Fatalf("FromCoreEvent: ok=%v err=%v", ok, err)
	}
	if out.Attributes["k8s.source.component"] != "kubelet" {
		t.Errorf("k8s.source.component = %v, want kubelet", out.Attributes["k8s.source.component"])
	}
	if out.Attributes["k8s.node.name"] != "worker-1" {
		t.Errorf("k8s.node.name = %v, want worker-1", out.Attributes["k8s.node.name"])
	}
}

func TestFromCoreEvent_FallbackToEventTime(t *testing.T) {
	// When FirstTimestamp is zero, occurred_at should use EventTime.
	eventTime := metav1.MicroTime{Time: time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)}
	ev := &corev1.Event{
		Reason: "BackOff",
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: "prod", Name: "web-1",
		},
		// FirstTimestamp intentionally zero
		EventTime: eventTime,
	}
	m := newTestMapper(t)
	out, ok, err := m.FromCoreEvent(context.Background(), ev)
	if err != nil || !ok {
		t.Fatalf("FromCoreEvent: ok=%v err=%v", ok, err)
	}
	if out.OccurredAt != eventTime.UnixNano() {
		t.Errorf("OccurredAt = %d, want EventTime %d", out.OccurredAt, eventTime.UnixNano())
	}
}

func TestFromCoreEvent_LegacyCount(t *testing.T) {
	ev := &corev1.Event{
		Reason: "BackOff",
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Namespace: "prod", Name: "payment-abc",
		},
		FirstTimestamp: metav1.Time{Time: time.Now().Add(-time.Minute)},
		LastTimestamp:  metav1.Time{Time: time.Now()},
		Count:          3,
	}
	m := newTestMapper(t)
	out, ok, err := m.FromCoreEvent(context.Background(), ev)
	if err != nil || !ok {
		t.Fatalf("FromCoreEvent: ok=%v err=%v", ok, err)
	}
	if out.Kind != wireformat.KindK8sPodCrash {
		t.Errorf("kind = %q", out.Kind)
	}
	if out.Series == nil || out.Series.Count != 3 {
		t.Errorf("series not populated for legacy count=3, got %+v", out.Series)
	}
}

// -----------------------------------------------------------------------------
// FromNodeConditionChange.
// -----------------------------------------------------------------------------

func TestFromNodeConditionChange_EmitsOnTransition(t *testing.T) {
	oldNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
			},
		},
	}
	newNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeMemoryPressure,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.Time{Time: time.Now()},
				},
			},
		},
	}
	m := newTestMapper(t)
	out, err := m.FromNodeConditionChange(context.Background(), oldNode, newNode)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	if out[0].Kind != wireformat.KindK8sNodePressure {
		t.Errorf("kind = %q", out[0].Kind)
	}
	if out[0].ServiceID != "node-1" {
		t.Errorf("service_id = %q, want node-1", out[0].ServiceID)
	}
}

func TestFromNodeConditionChange_NoTransition(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
			},
		},
	}
	m := newTestMapper(t)
	out, err := m.FromNodeConditionChange(context.Background(), node, node)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d events, want 0 on no transition", len(out))
	}
}

func TestFromNodeConditionChange_NilNewNodeReturnsNil(t *testing.T) {
	m := newTestMapper(t)
	out, err := m.FromNodeConditionChange(context.Background(), nil, nil)
	if err != nil || out != nil {
		t.Errorf("got %v, %v; want nil, nil", out, err)
	}
}

func TestFromNodeConditionChange_NodeNotReadyEmitsEvent(t *testing.T) {
	// NodeReady flipping to False must produce a K8S_NODE_PRESSURE event.
	oldNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	newNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeReady,
					Status:             corev1.ConditionFalse,
					Reason:             "KubeletNotReady",
					Message:            "container runtime not responding",
					LastTransitionTime: metav1.Time{Time: time.Now()},
				},
			},
		},
	}
	m := newTestMapper(t)
	out, err := m.FromNodeConditionChange(context.Background(), oldNode, newNode)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1 (NotReady)", len(out))
	}
	if out[0].Kind != wireformat.KindK8sNodePressure {
		t.Errorf("kind = %q, want K8S_NODE_PRESSURE", out[0].Kind)
	}
	if out[0].Attributes["k8s.reason"] != "NotReady" {
		t.Errorf("k8s.reason = %v, want NotReady", out[0].Attributes["k8s.reason"])
	}
	// cond.Reason and cond.Message should appear as attributes.
	if out[0].Attributes["k8s.condition.reason"] != "KubeletNotReady" {
		t.Errorf("k8s.condition.reason = %v, want KubeletNotReady", out[0].Attributes["k8s.condition.reason"])
	}
	if out[0].Attributes["k8s.message"] != "container runtime not responding" {
		t.Errorf("k8s.message = %v", out[0].Attributes["k8s.message"])
	}
}

func TestFromNodeConditionChange_FallsBackToHeartbeatTime(t *testing.T) {
	// When LastTransitionTime is zero, occurred_at should use LastHeartbeatTime.
	heartbeat := metav1.Time{Time: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)}
	newNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-4"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:              corev1.NodeMemoryPressure,
					Status:            corev1.ConditionTrue,
					LastHeartbeatTime: heartbeat,
					// LastTransitionTime intentionally zero
				},
			},
		},
	}
	m := newTestMapper(t)
	out, err := m.FromNodeConditionChange(context.Background(), nil, newNode)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	if out[0].OccurredAt != wireformat.TimeToUnixNano(heartbeat.Time) {
		t.Errorf("OccurredAt = %d, want heartbeat time %d", out[0].OccurredAt, wireformat.TimeToUnixNano(heartbeat.Time))
	}
}

func TestFromNodeConditionChange_AlreadyNotReadySkipped(t *testing.T) {
	// If node was already not-ready, no new event should be emitted.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-3"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			},
		},
	}
	m := newTestMapper(t)
	out, err := m.FromNodeConditionChange(context.Background(), node.DeepCopy(), node)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d events; already-not-ready node should not re-emit", len(out))
	}
}

// -----------------------------------------------------------------------------
// FromHPAScale.
// -----------------------------------------------------------------------------

func TestFromHPAScale_ReplicasChanged(t *testing.T) {
	oldHPA := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "web-hpa", Namespace: "prod"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Name: "web"},
			MaxReplicas:    20,
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			CurrentReplicas: 3,
			DesiredReplicas: 3,
		},
	}
	newHPA := oldHPA.DeepCopy()
	newHPA.Status.CurrentReplicas = 5
	newHPA.Status.DesiredReplicas = 5
	newHPA.Status.LastScaleTime = &metav1.Time{Time: time.Now()}

	m := newTestMapper(t)
	ev, ok, err := m.FromHPAScale(context.Background(), oldHPA, newHPA)
	if err != nil || !ok {
		t.Fatalf("FromHPAScale: ok=%v err=%v", ok, err)
	}
	if ev.Kind != wireformat.KindK8sHPAScale {
		t.Errorf("kind = %q", ev.Kind)
	}
	if ev.Attributes["k8s.reason"] != "ScaleUp" {
		t.Errorf("reason = %q, want ScaleUp", ev.Attributes["k8s.reason"])
	}
	if ev.ServiceID != "web" {
		t.Errorf("service_id = %q, want web", ev.ServiceID)
	}
}

func TestFromHPAScale_NoChange(t *testing.T) {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 3},
	}
	m := newTestMapper(t)
	_, ok, err := m.FromHPAScale(context.Background(), hpa, hpa)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Error("should not emit when current_replicas unchanged")
	}
}

func TestFromHPAScale_NilLastScaleTime(t *testing.T) {
	// An HPA whose LastScaleTime is nil must still produce occurred_at > 0.
	// §7.1: the server drops events with occurred_at ≤ 0 (INVALID_TIMESTAMP).
	// When LastScaleTime is unavailable, the mapper falls back to time.Now().
	old := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "app-hpa", Namespace: "prod"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Name: "app"},
			MaxReplicas:    10,
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{
			CurrentReplicas: 2,
			LastScaleTime:   nil, // explicitly nil
		},
	}
	newHPA := old.DeepCopy()
	newHPA.Status.CurrentReplicas = 4

	m := newTestMapper(t)
	ev, ok, err := m.FromHPAScale(context.Background(), old, newHPA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected event on replica change")
	}
	// occurred_at must be > 0 per §7.1 to avoid INVALID_TIMESTAMP drop.
	if ev.OccurredAt <= 0 {
		t.Errorf("OccurredAt = %d, want > 0 (§7.1)", ev.OccurredAt)
	}
}

// -----------------------------------------------------------------------------
// FromPodStatusChange.
// -----------------------------------------------------------------------------

func TestFromPodStatusChange_Running(t *testing.T) {
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payment-abc", Namespace: "prod"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	newPod := oldPod.DeepCopy()
	newPod.Status.Phase = corev1.PodRunning
	newPod.Status.StartTime = &metav1.Time{Time: time.Now()}

	m := newTestMapper(t, newPod)
	out, err := m.FromPodStatusChange(context.Background(), oldPod, newPod)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 || out[0].Kind != wireformat.KindK8sPodStarted {
		t.Fatalf("got %+v, want 1 K8S_POD_STARTED", out)
	}
}

func TestFromPodStatusChange_Failed(t *testing.T) {
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payment-abc", Namespace: "prod"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	newPod := oldPod.DeepCopy()
	newPod.Status.Phase = corev1.PodFailed
	newPod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					Reason:     "Error",
					ExitCode:   1,
					FinishedAt: metav1.Time{Time: time.Now()},
				},
			},
		},
	}

	m := newTestMapper(t, newPod)
	out, err := m.FromPodStatusChange(context.Background(), oldPod, newPod)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 || out[0].Kind != wireformat.KindK8sPodTerminated {
		t.Fatalf("got %+v, want 1 K8S_POD_TERMINATED", out)
	}
	if out[0].Attributes["k8s.exit_code"] != 1 {
		t.Errorf("exit_code attr = %v, want 1", out[0].Attributes["k8s.exit_code"])
	}
}

func TestFromPodStatusChange_OOMKilled(t *testing.T) {
	// A naked pod (no owner) that was OOM killed: container terminated with
	// reason containerReasonOOMKilled. Verify the mapper emits K8S_OOM_KILL
	// (Tier 1, always passes the severity filter) with the pod name as service ID.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "oom-victim", Namespace: "demo"},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     containerReasonOOMKilled,
							ExitCode:   137,
							FinishedAt: metav1.Time{Time: time.Now()},
						},
					},
				},
			},
		},
	}

	// Simulate an ADD event: old=nil, new=pod already in Failed phase.
	m := newTestMapper(t, pod)
	out, err := m.FromPodStatusChange(context.Background(), nil, pod)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 || out[0].Kind != wireformat.KindK8sOOMKill {
		t.Fatalf("got %+v, want 1 K8S_OOM_KILL", out)
	}
	if out[0].ServiceID != "oom-victim" {
		t.Errorf("ServiceID = %q, want %q", out[0].ServiceID, "oom-victim")
	}
	if out[0].Attributes["k8s.exit_code"] != 137 {
		t.Errorf("exit_code attr = %v, want 137", out[0].Attributes["k8s.exit_code"])
	}
	if out[0].Attributes["k8s.reason"] != containerReasonOOMKilled {
		t.Errorf("k8s.reason attr = %q, want %q", out[0].Attributes["k8s.reason"], containerReasonOOMKilled)
	}
}

// -----------------------------------------------------------------------------
// FromDeploymentRollout
// -----------------------------------------------------------------------------

func progressingCondition(status corev1.ConditionStatus, reason string) appsv1.DeploymentCondition {
	return appsv1.DeploymentCondition{
		Type:   appsv1.DeploymentProgressing,
		Status: status,
		Reason: reason,
	}
}

func deploymentWithProgressing(cond appsv1.DeploymentCondition) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"},
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{cond},
		},
	}
}

func TestFromDeploymentRollout_NilNewReturnsNil(t *testing.T) {
	m := newTestMapper(t)
	evts, err := m.FromDeploymentRollout(context.Background(), nil, nil)
	if err != nil || evts != nil {
		t.Errorf("got %v, %v; want nil, nil", evts, err)
	}
}

func TestFromDeploymentRollout_StatusUnchangedReturnsNil(t *testing.T) {
	m := newTestMapper(t)
	dep := deploymentWithProgressing(
		progressingCondition(corev1.ConditionTrue, "ReplicaSetUpdated"),
	)
	evts, err := m.FromDeploymentRollout(context.Background(), dep.DeepCopy(), dep)
	if err != nil || evts != nil {
		t.Errorf("got %v, %v; want nil, nil when status unchanged", evts, err)
	}
}

func TestFromDeploymentRollout_NilOldToProgressingEmitsEvent(t *testing.T) {
	m := newTestMapper(t)
	newDep := deploymentWithProgressing(
		progressingCondition(corev1.ConditionTrue, "ReplicaSetUpdated"),
	)
	evts, err := m.FromDeploymentRollout(context.Background(), nil, newDep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Kind != wireformat.KindK8sDeployRollout {
		t.Errorf("kind = %q, want %q", evts[0].Kind, wireformat.KindK8sDeployRollout)
	}
	if evts[0].Attributes["k8s.rollout.status"] != "progressing" {
		t.Errorf("rollout.status = %v, want progressing", evts[0].Attributes["k8s.rollout.status"])
	}
}

func TestFromDeploymentRollout_ProgressingToCompleteEmitsEvent(t *testing.T) {
	m := newTestMapper(t)
	oldDep := deploymentWithProgressing(
		progressingCondition(corev1.ConditionTrue, "ReplicaSetUpdated"),
	)
	newDep := deploymentWithProgressing(
		progressingCondition(corev1.ConditionTrue, "NewReplicaSetAvailable"),
	)
	evts, err := m.FromDeploymentRollout(context.Background(), oldDep, newDep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Attributes["k8s.rollout.status"] != "complete" {
		t.Errorf("rollout.status = %v, want complete", evts[0].Attributes["k8s.rollout.status"])
	}
}

func TestFromDeploymentRollout_WithRevisionAnnotationSetsAttr(t *testing.T) {
	m := newTestMapper(t)
	newDep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "prod",
			Annotations: map[string]string{
				"deployment.kubernetes.io/revision": "3",
			},
		},
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{
				progressingCondition(corev1.ConditionTrue, "ReplicaSetUpdated"),
			},
		},
	}
	evts, err := m.FromDeploymentRollout(context.Background(), nil, newDep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Attributes["k8s.rollout.revision"] != 3 {
		t.Errorf("k8s.rollout.revision = %v, want 3 (int)", evts[0].Attributes["k8s.rollout.revision"])
	}
}

func TestFromDeploymentRollout_ProgressingToFailedEmitsEvent(t *testing.T) {
	m := newTestMapper(t)
	oldDep := deploymentWithProgressing(
		progressingCondition(corev1.ConditionTrue, "ReplicaSetUpdated"),
	)
	newDep := deploymentWithProgressing(
		progressingCondition(corev1.ConditionFalse, "ProgressDeadlineExceeded"),
	)
	evts, err := m.FromDeploymentRollout(context.Background(), oldDep, newDep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if evts[0].Attributes["k8s.rollout.status"] != "failed" {
		t.Errorf("rollout.status = %v, want failed", evts[0].Attributes["k8s.rollout.status"])
	}
}

// -----------------------------------------------------------------------------
// §7.1 occurred_at > 0 fallback: time.Now() when all timestamps are zero.
// -----------------------------------------------------------------------------

func TestFromK8sEvent_BothTimestampsZeroFallsBackToNow(t *testing.T) {
	// Neither EventTime nor DeprecatedFirstTimestamp is set; the mapper must
	// fall back to time.Now() to satisfy §7.1 (occurred_at > 0).
	ev := &eventsv1.Event{
		Reason:    "OOMKilling",
		Regarding: corev1.ObjectReference{Kind: "Pod", Namespace: "prod", Name: "p"},
		// EventTime and DeprecatedFirstTimestamp intentionally zero
	}
	m := newTestMapper(t)
	out, ok, err := m.FromK8sEvent(context.Background(), ev)
	if err != nil || !ok {
		t.Fatalf("FromK8sEvent: ok=%v err=%v", ok, err)
	}
	if out.OccurredAt <= 0 {
		t.Errorf("OccurredAt = %d, want > 0 (§7.1)", out.OccurredAt)
	}
}

func TestFromCoreEvent_BothTimestampsZeroFallsBackToNow(t *testing.T) {
	// Neither FirstTimestamp nor EventTime is set; the mapper must fall back to
	// time.Now() to satisfy §7.1 (occurred_at > 0).
	ev := &corev1.Event{
		Reason:         "BackOff",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "prod", Name: "p"},
		// FirstTimestamp and EventTime intentionally zero
	}
	m := newTestMapper(t)
	out, ok, err := m.FromCoreEvent(context.Background(), ev)
	if err != nil || !ok {
		t.Fatalf("FromCoreEvent: ok=%v err=%v", ok, err)
	}
	if out.OccurredAt <= 0 {
		t.Errorf("OccurredAt = %d, want > 0 (§7.1)", out.OccurredAt)
	}
}

func TestFromPodStatusChange_AllTimestampsZeroFallsBackToNow(t *testing.T) {
	// A pod transition where StartTime, ContainerStatuses, and CreationTimestamp
	// are all zero: buildPodLifecycleEvent must fall back to time.Now().
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "ghost-pod", Namespace: "prod"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			// StartTime intentionally nil
		},
	}
	m := newTestMapper(t, pod)
	out, err := m.FromPodStatusChange(context.Background(), nil, pod)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	if out[0].OccurredAt <= 0 {
		t.Errorf("OccurredAt = %d, want > 0 (§7.1)", out[0].OccurredAt)
	}
}

// ----------------------------------------------------------------------------
// truncate — §17 attribute string limit.
// ----------------------------------------------------------------------------

func TestTruncate_ShortStringUnchanged(t *testing.T) {
	s := "hello world"
	if got := truncate(s, 2048); got != s {
		t.Errorf("truncate(%q, 2048) = %q; want unchanged", s, got)
	}
}

func TestTruncate_ExactMaxUnchanged(t *testing.T) {
	s := strings.Repeat("a", 2048)
	if got := truncate(s, 2048); got != s {
		t.Error("string at exactly max should be returned unchanged")
	}
}

func TestTruncate_ExceedsMaxClipsWithEllipsis(t *testing.T) {
	s := strings.Repeat("a", 2049)
	got := truncate(s, 2048)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated string should end with ellipsis (U+2026), got %q suffix", got[len(got)-4:])
	}
	// Prefix must be exactly max-1 bytes of the original.
	wantPrefix := s[:2047]
	if !strings.HasPrefix(got, wantPrefix) {
		t.Error("truncated prefix mismatch")
	}
}
