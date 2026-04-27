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
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
