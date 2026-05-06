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
	"errors"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/incidentary/operator/internal/identity"
	"github.com/incidentary/operator/internal/wireformat"
)

// deploy constructs a Deployment with the given revision, spec replicas, and
// conditions. Used as the base for every DEPLOY_* test case.
func deploy(revision string, replicas int32, conds ...appsv1.DeploymentCondition) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "prod",
			Annotations: map[string]string{
				"deployment.kubernetes.io/revision": revision,
			},
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Hour)},
		},
		Spec:   appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{Conditions: conds},
	}
}

func cond(typ appsv1.DeploymentConditionType, status corev1.ConditionStatus, reason string) appsv1.DeploymentCondition {
	return appsv1.DeploymentCondition{
		Type:               typ,
		Status:             status,
		Reason:             reason,
		LastTransitionTime: metav1.Time{Time: time.Now()},
		LastUpdateTime:     metav1.Time{Time: time.Now()},
	}
}

// -----------------------------------------------------------------------------
// DEPLOY_STARTED.
// -----------------------------------------------------------------------------

func TestFromDeploymentChange_DeployStarted_RevisionBump(t *testing.T) {
	old := deploy("3", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "NewReplicaSetAvailable"),
	)
	new := deploy("4", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "NewReplicaSetCreated"),
	)
	m := newTestMapper(t)
	out, err := m.FromDeploymentChange(context.Background(), old, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hasKind(out, wireformat.KindDeployStarted) {
		t.Fatalf("expected DEPLOY_STARTED, got %+v", kinds(out))
	}
}

// -----------------------------------------------------------------------------
// DEPLOY_SUCCEEDED.
// -----------------------------------------------------------------------------

func TestFromDeploymentChange_DeploySucceeded(t *testing.T) {
	old := deploy("4", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated"),
		cond(appsv1.DeploymentAvailable, corev1.ConditionFalse, ""),
	)
	new := deploy("4", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "NewReplicaSetAvailable"),
		cond(appsv1.DeploymentAvailable, corev1.ConditionTrue, ""),
	)
	new.Status.UpdatedReplicas = 3
	new.Status.ReadyReplicas = 3

	m := newTestMapper(t)
	out, err := m.FromDeploymentChange(context.Background(), old, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hasKind(out, wireformat.KindDeploySucceeded) {
		t.Fatalf("expected DEPLOY_SUCCEEDED, got %+v", kinds(out))
	}
}

func TestFromDeploymentChange_DeploySucceeded_NotOnOnAdd(t *testing.T) {
	// OnAdd of an already-stable Deployment should NOT emit DEPLOY_SUCCEEDED.
	new := deploy("4", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "NewReplicaSetAvailable"),
		cond(appsv1.DeploymentAvailable, corev1.ConditionTrue, ""),
	)
	new.Status.UpdatedReplicas = 3
	new.Status.ReadyReplicas = 3

	m := newTestMapper(t)
	out, err := m.FromDeploymentChange(context.Background(), nil, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if hasKind(out, wireformat.KindDeploySucceeded) {
		t.Fatalf("should not emit DEPLOY_SUCCEEDED on OnAdd, got %+v", kinds(out))
	}
}

// -----------------------------------------------------------------------------
// DEPLOY_FAILED.
// -----------------------------------------------------------------------------

func TestFromDeploymentChange_DeployFailed(t *testing.T) {
	old := deploy("5", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated"),
	)
	new := deploy("5", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionFalse, "ProgressDeadlineExceeded"),
	)

	m := newTestMapper(t)
	out, err := m.FromDeploymentChange(context.Background(), old, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hasKind(out, wireformat.KindDeployFailed) {
		t.Fatalf("expected DEPLOY_FAILED, got %+v", kinds(out))
	}
}

// -----------------------------------------------------------------------------
// DEPLOY_ROLLED_BACK.
// -----------------------------------------------------------------------------

func TestFromDeploymentChange_DeployRolledBack_RevisionDecrease(t *testing.T) {
	old := deploy("5", 3)
	new := deploy("3", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "NewReplicaSetCreated"),
	)

	m := newTestMapper(t)
	out, err := m.FromDeploymentChange(context.Background(), old, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hasKind(out, wireformat.KindDeployRolledBack) {
		t.Fatalf("expected DEPLOY_ROLLED_BACK, got %+v", kinds(out))
	}
	// Rollback should not also emit DEPLOY_STARTED in the same cycle.
	if hasKind(out, wireformat.KindDeployStarted) {
		t.Errorf("rollback cycle should not also emit DEPLOY_STARTED")
	}
}

func TestFromDeploymentChange_DeployRolledBack_ChangeCause(t *testing.T) {
	old := deploy("4", 3)
	old.Annotations["kubernetes.io/change-cause"] = "kubectl apply"
	new := deploy("5", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "NewReplicaSetCreated"),
	)
	new.Annotations["kubernetes.io/change-cause"] = "kubectl rollback to revision 3"

	m := newTestMapper(t)
	out, err := m.FromDeploymentChange(context.Background(), old, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hasKind(out, wireformat.KindDeployRolledBack) {
		t.Fatalf("expected DEPLOY_ROLLED_BACK, got %+v", kinds(out))
	}
}

// -----------------------------------------------------------------------------
// DEPLOY_CANCELLED.
// -----------------------------------------------------------------------------

func TestFromDeploymentChange_DeployCancelled_ScaleToZero(t *testing.T) {
	old := deploy("5", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated"),
	)
	new := deploy("5", 0,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated"),
	)

	m := newTestMapper(t)
	out, err := m.FromDeploymentChange(context.Background(), old, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hasKind(out, wireformat.KindDeployCancelled) {
		t.Fatalf("expected DEPLOY_CANCELLED, got %+v", kinds(out))
	}
}

func TestFromDeploymentDelete_MidRollout(t *testing.T) {
	d := deploy("5", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated"),
	)

	m := newTestMapper(t)
	ev, ok, err := m.FromDeploymentDelete(context.Background(), d)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ok {
		t.Fatal("expected event when deleted mid-rollout")
	}
	if ev.Kind != wireformat.KindDeployCancelled {
		t.Errorf("kind = %q, want DEPLOY_CANCELLED", ev.Kind)
	}
}

func TestFromDeploymentDelete_StableDoesNotFireCancelled(t *testing.T) {
	// A stable Deployment (Progressing=True, Reason=NewReplicaSetAvailable) that
	// is intentionally deleted (e.g. kubectl delete deployment) must NOT emit
	// DEPLOY_CANCELLED. The deployment completed its last rollout successfully;
	// the deletion is routine cleanup, not a mid-rollout cancellation.
	d := deploy("5", 3,
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "NewReplicaSetAvailable"),
	)
	m := newTestMapper(t)
	_, ok, err := m.FromDeploymentDelete(context.Background(), d)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok {
		t.Error("stable deployment deletion should NOT emit DEPLOY_CANCELLED (false positive)")
	}
}

// -----------------------------------------------------------------------------
// StatefulSet and DaemonSet rollouts.
// -----------------------------------------------------------------------------

func TestFromStatefulSetChange_Started(t *testing.T) {
	old := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptrInt32(3)},
		Status: appsv1.StatefulSetStatus{
			UpdateRevision:  "db-abc",
			CurrentRevision: "db-abc",
		},
	}
	new := old.DeepCopy()
	new.Status.UpdateRevision = "db-def"

	m := newTestMapper(t)
	out, err := m.FromStatefulSetChange(context.Background(), old, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hasKind(out, wireformat.KindDeployStarted) {
		t.Fatalf("expected DEPLOY_STARTED, got %+v", kinds(out))
	}
}

func TestFromStatefulSetChange_Succeeded(t *testing.T) {
	old := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "prod"},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptrInt32(3)},
		Status: appsv1.StatefulSetStatus{
			UpdateRevision:  "db-def",
			CurrentRevision: "db-abc",
			ReadyReplicas:   2,
		},
	}
	new := old.DeepCopy()
	new.Status.CurrentRevision = "db-def"
	new.Status.ReadyReplicas = 3

	m := newTestMapper(t)
	out, err := m.FromStatefulSetChange(context.Background(), old, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hasKind(out, wireformat.KindDeploySucceeded) {
		t.Fatalf("expected DEPLOY_SUCCEEDED, got %+v", kinds(out))
	}
}

func TestFromDaemonSetChange_Started(t *testing.T) {
	old := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "log-agent", Namespace: "monitoring"},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     1,
			UpdatedNumberScheduled: 5,
			DesiredNumberScheduled: 5,
		},
	}
	new := old.DeepCopy()
	new.Status.ObservedGeneration = 2
	new.Status.UpdatedNumberScheduled = 1
	new.Status.DesiredNumberScheduled = 5

	m := newTestMapper(t)
	out, err := m.FromDaemonSetChange(context.Background(), old, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hasKind(out, wireformat.KindDeployStarted) {
		t.Fatalf("expected DEPLOY_STARTED, got %+v", kinds(out))
	}
}

func TestFromDaemonSetChange_Succeeded(t *testing.T) {
	old := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "log-agent", Namespace: "monitoring"},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     2,
			UpdatedNumberScheduled: 3,
			DesiredNumberScheduled: 5,
		},
	}
	new := old.DeepCopy()
	new.Status.UpdatedNumberScheduled = 5

	m := newTestMapper(t)
	out, err := m.FromDaemonSetChange(context.Background(), old, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hasKind(out, wireformat.KindDeploySucceeded) {
		t.Fatalf("expected DEPLOY_SUCCEEDED, got %+v", kinds(out))
	}
}

// -----------------------------------------------------------------------------
// specReplicas edge cases.
// -----------------------------------------------------------------------------

func TestSpecReplicas_NilDefaultsToOne(t *testing.T) {
	if got := specReplicas(nil); got != 1 {
		t.Errorf("specReplicas(nil) = %d, want 1 (K8s default)", got)
	}
}

func TestSpecReplicas_ExplicitValue(t *testing.T) {
	n := int32(5)
	if got := specReplicas(&n); got != 5 {
		t.Errorf("specReplicas(&5) = %d, want 5", got)
	}
}

// -----------------------------------------------------------------------------
// FromDeploymentChange with nil Spec.Replicas (K8s default of 1).
// -----------------------------------------------------------------------------

func TestFromDeploymentChange_NilReplicasSucceeded(t *testing.T) {
	// A Deployment with no explicit Spec.Replicas (nil → K8s default 1) that
	// just completed its rollout must still emit DEPLOY_SUCCEEDED.
	old := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "web",
			Namespace:         "prod",
			Annotations:       map[string]string{"deployment.kubernetes.io/revision": "2"},
			CreationTimestamp: metav1.Now(),
		},
		Spec: appsv1.DeploymentSpec{Replicas: nil}, // ← nil, defaults to 1
		Status: appsv1.DeploymentStatus{
			UpdatedReplicas: 0,
			ReadyReplicas:   0,
			Conditions: []appsv1.DeploymentCondition{
				cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated"),
				cond(appsv1.DeploymentAvailable, corev1.ConditionFalse, ""),
			},
		},
	}
	new := old.DeepCopy()
	new.Status.UpdatedReplicas = 1
	new.Status.ReadyReplicas = 1
	new.Status.Conditions = []appsv1.DeploymentCondition{
		cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, reasonNewReplicaSetAvailable),
		cond(appsv1.DeploymentAvailable, corev1.ConditionTrue, ""),
	}

	m := newTestMapper(t)
	out, err := m.FromDeploymentChange(context.Background(), old, new)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hasKind(out, wireformat.KindDeploySucceeded) {
		t.Errorf("expected DEPLOY_SUCCEEDED for nil-replicas deployment, got %+v", kinds(out))
	}
}

// -----------------------------------------------------------------------------
// Dispatch — unknown type and delete path.
// -----------------------------------------------------------------------------

func TestDispatch_UnknownTypeReturnsNil(t *testing.T) {
	// Service is in WatchSet but has no mapper. Dispatch must return (nil, nil).
	m := newTestMapper(t)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: "prod"}}
	out, err := m.Dispatch(context.Background(), nil, svc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected no events for unknown type, got %d", len(out))
	}
}

func TestDispatch_DeleteNonDeploymentReturnsNil(t *testing.T) {
	// Deleting a Pod (or any non-Deployment) must return (nil, nil).
	m := newTestMapper(t)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "prod"}}
	out, err := m.Dispatch(context.Background(), pod, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected no events for pod delete, got %d", len(out))
	}
}

// -----------------------------------------------------------------------------
// StatefulSet — OccurredAt timestamp documents current behavior.
// -----------------------------------------------------------------------------

func TestFromStatefulSetChange_OccurredAtIsCreationTimestamp(t *testing.T) {
	// StatefulSet DEPLOY_STARTED events use CreationTimestamp for OccurredAt
	// because the StatefulSet controller does not expose condition transition
	// times with the precision that Deployment conditions do.
	// This test documents the current behavior so any future change to use
	// a more precise timestamp is a deliberate, tested decision.
	creation := metav1.Time{Time: time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)}
	old := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "db",
			Namespace:         "prod",
			CreationTimestamp: creation,
		},
		Spec: appsv1.StatefulSetSpec{Replicas: ptrInt32(3)},
		Status: appsv1.StatefulSetStatus{
			UpdateRevision:  "db-v1",
			CurrentRevision: "db-v1",
		},
	}
	n := old.DeepCopy()
	n.Status.UpdateRevision = "db-v2"

	m := newTestMapper(t)
	out, err := m.FromStatefulSetChange(context.Background(), old, n)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("expected DEPLOY_STARTED event")
	}
	wantAt := wireformat.TimeToUnixNano(creation.Time)
	if out[0].OccurredAt != wantAt {
		t.Errorf("OccurredAt = %d, want CreationTimestamp (%d)", out[0].OccurredAt, wantAt)
	}
}

// -----------------------------------------------------------------------------
// revisionAttr / parseRevision
// -----------------------------------------------------------------------------

func TestRevisionAttr_NumericStringReturnsInt(t *testing.T) {
	got := revisionAttr("42")
	if got != 42 {
		t.Errorf("revisionAttr(\"42\") = %v (%T), want 42 (int)", got, got)
	}
}

func TestRevisionAttr_NonNumericStringReturnsSameString(t *testing.T) {
	// Non-parseable revision strings (e.g. git SHAs) must pass through as-is
	// so no revision information is lost.
	got := revisionAttr("abc-123")
	if got != "abc-123" {
		t.Errorf("revisionAttr(\"abc-123\") = %v (%T), want \"abc-123\" (string)", got, got)
	}
}

func TestParseRevision_ValidInt(t *testing.T) {
	n, err := parseRevision("7")
	if err != nil || n != 7 {
		t.Errorf("parseRevision(\"7\") = %d, %v; want 7, nil", n, err)
	}
}

func TestParseRevision_InvalidReturnsError(t *testing.T) {
	_, err := parseRevision("not-a-number")
	if err == nil {
		t.Error("expected error for non-numeric revision string")
	}
}

// -----------------------------------------------------------------------------
// FromDeploymentDelete — additional branches.
// -----------------------------------------------------------------------------

func TestFromDeploymentDelete_NilIsNoop(t *testing.T) {
	m := newTestMapper(t)
	_, ok, err := m.FromDeploymentDelete(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for nil deployment")
	}
}

func TestFromDeploymentDelete_ResolverErrorPropagates(t *testing.T) {
	m := newErrorMapper(t, errors.New("etcd unavailable"))
	d := deploy("5", 3, cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated"))
	_, _, err := m.FromDeploymentDelete(context.Background(), d)
	if err == nil {
		t.Error("expected error when resolver fails, got nil")
	}
}

// TestFromDeploymentDelete_ServiceIDFromAnnotation covers the
// resolveWorkloadServiceID branch where res.ServiceID != "" (annotation wins).
func TestFromDeploymentDelete_ServiceIDFromAnnotation(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "prod",
			Annotations: map[string]string{
				identity.ServiceIDAnnotation:        "my-service",
				"deployment.kubernetes.io/revision": "5",
			},
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-time.Hour)},
		},
		Spec: appsv1.DeploymentSpec{Replicas: ptrInt32(3)},
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{
				cond(appsv1.DeploymentProgressing, corev1.ConditionTrue, "ReplicaSetUpdated"),
			},
		},
	}
	m := newTestMapper(t, dep)

	ev, ok, err := m.FromDeploymentDelete(context.Background(), dep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected DEPLOY_CANCELLED event")
	}
	if ev.ServiceID != "my-service" {
		t.Errorf("ServiceID = %q; want %q", ev.ServiceID, "my-service")
	}
}

// -----------------------------------------------------------------------------
// Helpers.
// -----------------------------------------------------------------------------

func hasKind(events []wireformat.Event, k wireformat.Kind) bool {
	for _, e := range events {
		if e.Kind == k {
			return true
		}
	}
	return false
}

func kinds(events []wireformat.Event) []wireformat.Kind {
	out := make([]wireformat.Kind, 0, len(events))
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

func ptrInt32(n int32) *int32 { return &n }
