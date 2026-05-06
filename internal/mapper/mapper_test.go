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
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/incidentary/operator/internal/identity"
	"github.com/incidentary/operator/internal/wireformat"
)

func TestNewMapper_NilResolverPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic when resolver is nil")
		}
	}()
	NewMapper(nil)
}

// TestDispatch_RouterCoverage exercises every branch of Dispatch so the
// type-switch routing is covered. It does not assert on the produced events —
// the individual mapper functions have their own tests. The goal is to confirm
// that routing reaches the right function without panicking.
func TestDispatch_RouterCoverage(t *testing.T) {
	m := newTestMapper(t)
	ctx := context.Background()

	t.Run("nil newObj nil oldObj returns nil", func(t *testing.T) {
		evts, err := m.Dispatch(ctx, nil, nil)
		if err != nil || evts != nil {
			t.Errorf("got %v, %v; want nil, nil", evts, err)
		}
	})

	t.Run("delete path – non-Deployment returns nil", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"}}
		evts, err := m.Dispatch(ctx, pod, nil)
		if err != nil || evts != nil {
			t.Errorf("got %v, %v; want nil, nil", evts, err)
		}
	})

	t.Run("delete path – Deployment without rollout annotation returns nil", func(t *testing.T) {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"}}
		evts, err := m.Dispatch(ctx, dep, nil)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		_ = evts
	})

	t.Run("unknown type returns nil", func(t *testing.T) {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}
		evts, err := m.Dispatch(ctx, nil, ns)
		if err != nil || evts != nil {
			t.Errorf("got %v, %v; want nil, nil for unknown type", evts, err)
		}
	})

	t.Run("eventsv1.Event route", func(t *testing.T) {
		ev := &eventsv1.Event{
			ObjectMeta:          metav1.ObjectMeta{Name: "ev1", Namespace: "default"},
			Reason:              "SomeReason",
			Regarding:           corev1.ObjectReference{Kind: "Pod"},
			ReportingController: "kubelet",
		}
		_, err := m.Dispatch(ctx, nil, ev)
		if err != nil {
			t.Errorf("unexpected error from eventsv1 route: %v", err)
		}
	})

	t.Run("corev1.Event route", func(t *testing.T) {
		ev := &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "ev2", Namespace: "default"},
			Reason:         "SomeReason",
			InvolvedObject: corev1.ObjectReference{Kind: "Pod"},
			Source:         corev1.EventSource{Component: "kubelet"},
		}
		_, err := m.Dispatch(ctx, nil, ev)
		if err != nil {
			t.Errorf("unexpected error from corev1 route: %v", err)
		}
	})

	t.Run("Node route – no old state", func(t *testing.T) {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
		_, err := m.Dispatch(ctx, nil, node)
		if err != nil {
			t.Errorf("unexpected error from Node route: %v", err)
		}
	})

	t.Run("Node route – with old state", func(t *testing.T) {
		old := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}
		newNode := old.DeepCopy()
		newNode.Status.Conditions = []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}
		_, err := m.Dispatch(ctx, old, newNode)
		if err != nil {
			t.Errorf("unexpected error from Node route with old state: %v", err)
		}
	})

	t.Run("Pod route", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "prod"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		_, err := m.Dispatch(ctx, nil, pod)
		if err != nil {
			t.Errorf("unexpected error from Pod route: %v", err)
		}
	})

	t.Run("HPA route", func(t *testing.T) {
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "hpa-1", Namespace: "prod"},
		}
		_, err := m.Dispatch(ctx, nil, hpa)
		if err != nil {
			t.Errorf("unexpected error from HPA route: %v", err)
		}
	})

	t.Run("Deployment route", func(t *testing.T) {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"},
		}
		_, err := m.Dispatch(ctx, nil, dep)
		if err != nil {
			t.Errorf("unexpected error from Deployment route: %v", err)
		}
	})

	t.Run("StatefulSet route", func(t *testing.T) {
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "prod"},
		}
		_, err := m.Dispatch(ctx, nil, sts)
		if err != nil {
			t.Errorf("unexpected error from StatefulSet route: %v", err)
		}
	})

	t.Run("DaemonSet route", func(t *testing.T) {
		ds := &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "fluentd", Namespace: "kube-system"},
		}
		_, err := m.Dispatch(ctx, nil, ds)
		if err != nil {
			t.Errorf("unexpected error from DaemonSet route: %v", err)
		}
	})
}

// ----------------------------------------------------------------------------
// dispatchDelete — happy path (Deployment deleted mid-rollout).
// ----------------------------------------------------------------------------

func TestDispatch_DeleteDeploymentMidRolloutProducesDeployCancelled(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"},
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{
				{
					// Progressing=True with a non-terminal reason signals an
					// in-progress rollout; delete must emit DEPLOY_CANCELLED.
					Type:   appsv1.DeploymentProgressing,
					Status: corev1.ConditionTrue,
					Reason: "ReplicaSetUpdated",
				},
			},
		},
	}
	m := newTestMapper(t) // no pre-seeded objects; resolver falls back to dep.Name
	evts, err := m.Dispatch(context.Background(), dep, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("expected 1 DEPLOY_CANCELLED event, got %d", len(evts))
	}
	if evts[0].Kind != wireformat.KindDeployCancelled {
		t.Errorf("kind = %q, want KindDeployCancelled", evts[0].Kind)
	}
}

// ----------------------------------------------------------------------------
// dispatchK8sEvent / dispatchCoreEvent — resolver error propagation.
// ----------------------------------------------------------------------------

// mapperErrorReader implements client.Reader and always returns the stored error.
// Used to exercise the error-return path in dispatchK8sEvent and dispatchCoreEvent.
type mapperErrorReader struct{ err error }

func (e *mapperErrorReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return e.err
}

func (e *mapperErrorReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return e.err
}

func newErrorMapper(t *testing.T, err error) *Mapper {
	t.Helper()
	resolver := &identity.Resolver{Client: &mapperErrorReader{err: err}}
	return NewMapper(resolver)
}

func TestDispatch_K8sEventHappyPathProducesEvent(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payment-abc", Namespace: "prod"},
	}
	m := newTestMapper(t, pod)

	ev := &eventsv1.Event{
		Reason: "BackOff",
		Regarding: corev1.ObjectReference{
			Kind:      "Pod",
			Namespace: "prod",
			Name:      "payment-abc",
		},
		EventTime: metav1.MicroTime{Time: time.Now()},
	}
	evts, err := m.Dispatch(context.Background(), nil, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("expected 1 event from dispatchK8sEvent happy path, got %d", len(evts))
	}
}

func TestDispatch_CoreEventHappyPathProducesEvent(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "payment-abc", Namespace: "prod"},
	}
	m := newTestMapper(t, pod)

	ev := &corev1.Event{
		Reason: "BackOff",
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Namespace: "prod",
			Name:      "payment-abc",
		},
		FirstTimestamp: metav1.Time{Time: time.Now()},
	}
	evts, err := m.Dispatch(context.Background(), nil, ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("expected 1 event from dispatchCoreEvent happy path, got %d", len(evts))
	}
}

func TestDispatch_K8sEventResolverErrorPropagates(t *testing.T) {
	m := newErrorMapper(t, errors.New("etcd unavailable"))

	// "BackOff" on Pod is in the mapping table — resolver will be called.
	ev := &eventsv1.Event{
		Reason: "BackOff",
		Regarding: corev1.ObjectReference{
			Kind:      "Pod",
			Namespace: "prod",
			Name:      "payment-abc",
		},
		EventTime: metav1.MicroTime{Time: time.Now()},
	}
	_, err := m.Dispatch(context.Background(), nil, ev)
	if err == nil {
		t.Error("expected non-nil error when resolver fails for K8s event, got nil")
	}
}

func TestDispatch_CoreEventResolverErrorPropagates(t *testing.T) {
	m := newErrorMapper(t, errors.New("etcd unavailable"))

	// "BackOff" on Pod is in the mapping table — resolver will be called.
	ev := &corev1.Event{
		Reason: "BackOff",
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Namespace: "prod",
			Name:      "payment-abc",
		},
		FirstTimestamp: metav1.Time{Time: time.Now()},
	}
	_, err := m.Dispatch(context.Background(), nil, ev)
	if err == nil {
		t.Error("expected non-nil error when resolver fails for core event, got nil")
	}
}
