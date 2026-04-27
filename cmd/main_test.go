/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package main

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/incidentary/operator/internal/batch"
	ingestclient "github.com/incidentary/operator/internal/client"
	"github.com/incidentary/operator/internal/filter"
	"github.com/incidentary/operator/internal/identity"
	"github.com/incidentary/operator/internal/mapper"
	"github.com/incidentary/operator/internal/wireformat"
)

// ----------------------------------------------------------------------------
// getEnvOrDefault
// ----------------------------------------------------------------------------

func TestGetEnvOrDefault_ReturnsValueWhenSet(t *testing.T) {
	t.Setenv("TEST_KEY_GED", "hello")
	if got := getEnvOrDefault("TEST_KEY_GED", "fallback"); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestGetEnvOrDefault_ReturnsFallbackWhenUnset(t *testing.T) {
	t.Setenv("TEST_KEY_GED_UNSET", "")
	if got := getEnvOrDefault("TEST_KEY_GED_UNSET", "default"); got != "default" {
		t.Errorf("got %q, want %q", got, "default")
	}
}

func TestGetEnvOrDefault_ReturnsFallbackWhenMissing(t *testing.T) {
	// Ensure variable is not set.
	t.Setenv("TEST_KEY_DEFINITELY_MISSING_XYZ123", "")
	if got := getEnvOrDefault("TEST_KEY_DEFINITELY_MISSING_XYZ123", "fb"); got != "fb" {
		t.Errorf("got %q, want %q", got, "fb")
	}
}

// ----------------------------------------------------------------------------
// parseDurationEnv
// ----------------------------------------------------------------------------

func TestParseDurationEnv_ValidSeconds(t *testing.T) {
	t.Setenv("TEST_DUR_VALID", "120")
	got := parseDurationEnv("TEST_DUR_VALID", time.Minute)
	if got != 2*time.Minute {
		t.Errorf("got %v, want 2m", got)
	}
}

func TestParseDurationEnv_UnsetReturnsFallback(t *testing.T) {
	t.Setenv("TEST_DUR_UNSET", "")
	got := parseDurationEnv("TEST_DUR_UNSET", 5*time.Second)
	if got != 5*time.Second {
		t.Errorf("got %v, want 5s", got)
	}
}

func TestParseDurationEnv_InvalidStringReturnsFallback(t *testing.T) {
	t.Setenv("TEST_DUR_INVALID", "not-a-number")
	got := parseDurationEnv("TEST_DUR_INVALID", 10*time.Second)
	if got != 10*time.Second {
		t.Errorf("got %v, want 10s", got)
	}
}

func TestParseDurationEnv_ZeroReturnsFallback(t *testing.T) {
	t.Setenv("TEST_DUR_ZERO", "0")
	got := parseDurationEnv("TEST_DUR_ZERO", 30*time.Second)
	if got != 30*time.Second {
		t.Errorf("got %v, want 30s (zero should use fallback)", got)
	}
}

func TestParseDurationEnv_NegativeReturnsFallback(t *testing.T) {
	t.Setenv("TEST_DUR_NEG", "-5")
	got := parseDurationEnv("TEST_DUR_NEG", 30*time.Second)
	if got != 30*time.Second {
		t.Errorf("got %v, want 30s (negative should use fallback)", got)
	}
}

// ----------------------------------------------------------------------------
// pipelineHandler.emit
// ----------------------------------------------------------------------------

// newTestScheme builds a scheme with all types the fake client may encounter.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		autoscalingv2.AddToScheme,
		eventsv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return s
}

// recordingClient is an IngestClient that records every flushed batch.
type recordingClient struct {
	batches []*wireformat.IngestBatch
}

func (r *recordingClient) Flush(_ context.Context, b *wireformat.IngestBatch) (ingestclient.FlushResult, error) {
	r.batches = append(r.batches, b)
	return ingestclient.FlushResult{
		IngestResponse: ingestclient.IngestResponse{Accepted: len(b.Events)},
	}, nil
}

func newTestPipeline(t *testing.T) (*pipelineHandler, *batch.Batcher) {
	t.Helper()
	s := newTestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).Build()
	resolver := identity.NewResolver(fc)
	mpr := mapper.NewMapper(resolver)
	sev := filter.NewFromString("info")

	rec := &recordingClient{}
	resource := func() wireformat.Resource { return wireformat.Resource{Attributes: map[string]string{}} }
	agent := func() wireformat.Agent { return wireformat.Agent{Type: wireformat.AgentTypeK8sOperator} }
	b := batch.NewBatcher(rec, resource, agent, log.Log.WithName("test"))

	h := &pipelineHandler{
		mapper:  mpr,
		filter:  sev,
		batcher: b,
		log:     log.Log.WithName("test"),
	}
	return h, b
}

func TestPipelineHandler_EmitUnknownTypeProducesNoEvents(t *testing.T) {
	h, b := newTestPipeline(t)
	// A Namespace has no mapper — Dispatch returns nil, nil.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	h.emit(context.Background(), nil, ns)
	if b.BufferSize() != 0 {
		t.Errorf("expected 0 events in buffer for unknown type, got %d", b.BufferSize())
	}
}

func TestPipelineHandler_EmitOOMKillEnqueuesEvent(t *testing.T) {
	h, b := newTestPipeline(t)

	// Build a Pod that transitioned from Pending to Failed with OOMKilled.
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "prod"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	newPod := oldPod.DeepCopy()
	newPod.Status.Phase = corev1.PodFailed
	newPod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					Reason:   "OOMKilled",
					ExitCode: 137,
				},
			},
		},
	}

	h.emit(context.Background(), oldPod, newPod)

	// The OOMKill event should be in the batcher's buffer.
	if b.BufferSize() == 0 {
		t.Fatal("expected at least one event in buffer after OOMKill")
	}
}

func TestPipelineHandler_EmitFilteredEventNotEnqueued(t *testing.T) {
	// Use an error-only severity filter so Tier 2 Info events are dropped.
	// K8S_POD_STARTED is Tier 2 / Info — it must not reach the batcher.
	s := newTestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).Build()
	resolver := identity.NewResolver(fc)
	mpr := mapper.NewMapper(resolver)
	errorOnly := filter.NewFromString("error")

	rec := &recordingClient{}
	resource := func() wireformat.Resource { return wireformat.Resource{Attributes: map[string]string{}} }
	agent := func() wireformat.Agent { return wireformat.Agent{Type: wireformat.AgentTypeK8sOperator} }
	b := batch.NewBatcher(rec, resource, agent, log.Log.WithName("test"))

	h := &pipelineHandler{
		mapper:  mpr,
		filter:  errorOnly,
		batcher: b,
		log:     log.Log.WithName("test"),
	}

	// A pod transition Pending → Running emits K8S_POD_STARTED (Tier 2, Info).
	oldPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app-xyz", Namespace: "prod"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	newPod := oldPod.DeepCopy()
	newPod.Status.Phase = corev1.PodRunning

	h.emit(context.Background(), oldPod, newPod)

	// K8S_POD_STARTED (Info) must be dropped by the error-only filter.
	if b.BufferSize() != 0 {
		t.Errorf("expected 0 events in buffer (Tier 2 Info filtered), got %d", b.BufferSize())
	}
}

func TestPipelineHandler_EmitNilNewObjHandledGracefully(t *testing.T) {
	h, _ := newTestPipeline(t)
	// OnDelete path: newObj is nil. Dispatch handles this; it must not panic.
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"},
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{},
		},
	}
	// No panic expected even with a nil newObj.
	h.emit(context.Background(), dep, nil)
}

// ----------------------------------------------------------------------------
// panic recovery
// ----------------------------------------------------------------------------

// panicingMapper is a mapper.Dispatcher that always panics in Dispatch.
type panicingMapper struct{}

func (p *panicingMapper) Dispatch(_ context.Context, _, _ client.Object) ([]wireformat.Event, error) {
	panic("intentional panic from test")
}

func TestPipelineHandler_EmitRecoversPanic(t *testing.T) {
	// A panicking mapper must not kill the goroutine — emit() must recover and
	// log, never propagating the panic to the caller.
	rec := &recordingClient{}
	resource := func() wireformat.Resource { return wireformat.Resource{Attributes: map[string]string{}} }
	agent := func() wireformat.Agent { return wireformat.Agent{Type: wireformat.AgentTypeK8sOperator} }
	b := batch.NewBatcher(rec, resource, agent, log.Log.WithName("test"))

	h := &pipelineHandler{
		mapper:  &panicingMapper{},
		filter:  filter.NewFromString("info"),
		batcher: b,
		log:     log.Log.WithName("test"),
	}

	// Must not panic — emit() should recover and log.
	h.emit(context.Background(), nil, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "prod"},
	})
	// Buffer must be empty (panic was recovered, no events enqueued).
	if b.BufferSize() != 0 {
		t.Errorf("expected 0 events after panic recovery, got %d", b.BufferSize())
	}
}
