/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package batch

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"

	"github.com/incidentary/operator/internal/client"
	"github.com/incidentary/operator/internal/wireformat"
)

// fakeClient records every Flush call for inspection.
type fakeClient struct {
	mu       sync.Mutex
	batches  [][]wireformat.Event
	calls    atomic.Int32
	response client.FlushResult
	err      error
}

func (f *fakeClient) Flush(_ context.Context, batch *wireformat.IngestBatch) (client.FlushResult, error) {
	f.calls.Add(1)
	if f.err != nil {
		return client.FlushResult{}, f.err
	}
	f.mu.Lock()
	copyEvents := make([]wireformat.Event, len(batch.Events))
	copy(copyEvents, batch.Events)
	f.batches = append(f.batches, copyEvents)
	f.mu.Unlock()
	return f.response, nil
}

func (f *fakeClient) FlushCount() int32 {
	return f.calls.Load()
}

func (f *fakeClient) BatchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

func testResource() wireformat.Resource {
	return wireformat.Resource{Attributes: map[string]string{"k8s.cluster.name": "test"}}
}

func testAgent() wireformat.Agent {
	return wireformat.Agent{
		Type:        wireformat.AgentTypeK8sOperator,
		Version:     "test",
		WorkspaceID: "ws-1",
	}
}

func event(id string) wireformat.Event {
	return wireformat.Event{
		ID:         id,
		Kind:       wireformat.KindK8sOOMKill,
		Severity:   wireformat.SeverityError,
		OccurredAt: 1,
		ServiceID:  "web",
	}
}

// startBatcher runs b.Start on a goroutine and returns a stop closure the
// caller must defer. The stop closure cancels the context and waits for the
// Start goroutine to return, which prevents racy log-after-test-complete
// panics from deferred drain paths.
func startBatcher(t *testing.T, b *Batcher) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = b.Start(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("batcher Start did not return within 2s of cancel")
		}
	}
}

func TestBatcher_FlushesOnMaxBatchSize(t *testing.T) {
	c := &fakeClient{}
	log := testr.New(t)
	b := NewBatcher(c, testResource, testAgent, log,
		WithFlushInterval(time.Hour), // never via ticker
		WithMaxBatchSize(3),
	)
	defer startBatcher(t, b)()

	b.Enqueue(event("1"), event("2"), event("3"))

	// Wait until the flush goroutine picks up the signal.
	waitFor(t, func() bool { return c.FlushCount() >= 1 })

	if c.BatchCount() != 1 {
		t.Fatalf("batches = %d, want 1", c.BatchCount())
	}
	if got := c.batches[0]; len(got) != 3 {
		t.Errorf("events flushed = %d, want 3", len(got))
	}
	if b.BufferSize() != 0 {
		t.Errorf("buffer not drained: size = %d", b.BufferSize())
	}
}

func TestBatcher_FlushesOnInterval(t *testing.T) {
	c := &fakeClient{}
	log := testr.New(t)
	b := NewBatcher(c, testResource, testAgent, log,
		WithFlushInterval(50*time.Millisecond),
		WithMaxBatchSize(10000),
	)
	defer startBatcher(t, b)()

	b.Enqueue(event("1"))
	waitFor(t, func() bool { return c.FlushCount() >= 1 })

	if b.BufferSize() != 0 {
		t.Errorf("buffer not drained after ticker flush")
	}
}

func TestBatcher_EmptyBufferNoFlush(t *testing.T) {
	c := &fakeClient{}
	log := testr.New(t)
	b := NewBatcher(c, testResource, testAgent, log,
		WithFlushInterval(30*time.Millisecond),
	)
	defer startBatcher(t, b)()

	time.Sleep(100 * time.Millisecond)
	if c.FlushCount() != 0 {
		t.Errorf("empty batcher should not call Flush, got %d calls", c.FlushCount())
	}
}

func TestBatcher_ShutdownDrains(t *testing.T) {
	c := &fakeClient{}
	log := testr.New(t)
	b := NewBatcher(c, testResource, testAgent, log,
		WithFlushInterval(time.Hour), // only shutdown drain can fire
		WithMaxBatchSize(10000),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = b.Start(ctx)
		close(done)
	}()

	b.Enqueue(event("1"), event("2"))
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}

	if c.FlushCount() != 1 {
		t.Errorf("shutdown flush count = %d, want 1", c.FlushCount())
	}
}

func TestBatcher_CaptureModeRequestedPropagated(t *testing.T) {
	c := &fakeClient{
		response: client.FlushResult{
			IngestResponse:       client.IngestResponse{Accepted: 1},
			CaptureModeRequested: "FULL",
		},
	}
	log := testr.New(t)
	b := NewBatcher(c, testResource, testAgent, log,
		WithFlushInterval(20*time.Millisecond),
	)
	defer startBatcher(t, b)()

	b.Enqueue(event("1"))
	waitFor(t, func() bool { return b.CaptureModeRequested() == "FULL" })
}

func TestBatcher_NeedLeaderElection(t *testing.T) {
	c := &fakeClient{}
	b := NewBatcher(c, testResource, testAgent, testr.New(t))
	if !b.NeedLeaderElection() {
		t.Error("NeedLeaderElection should be true")
	}
}

func TestBatcher_NilClientPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil client")
		}
	}()
	_ = NewBatcher(nil, testResource, testAgent, testr.New(t))
}

func TestBatcher_FlushedAtIsPositive(t *testing.T) {
	// §4.1 requires flushed_at > 0 — a zero value is a spec violation that
	// causes the server to reject the batch with INVALID_TIMESTAMP.
	var captured *wireformat.IngestBatch
	c := &batchCapturingClient{}
	log := testr.New(t)
	b := NewBatcher(c, testResource, testAgent, log,
		WithFlushInterval(20*time.Millisecond),
	)
	defer startBatcher(t, b)()

	b.Enqueue(event("flushed-at-1"))
	waitFor(t, func() bool {
		captured = c.last()
		return captured != nil
	})
	if captured.FlushedAt <= 0 {
		t.Errorf("flushed_at = %d, want > 0 (wire format v2 §4.1)", captured.FlushedAt)
	}
}

// batchCapturingClient records the last full IngestBatch flushed.
type batchCapturingClient struct {
	mu     sync.Mutex
	_batch *wireformat.IngestBatch
}

func (c *batchCapturingClient) Flush(_ context.Context, b *wireformat.IngestBatch) (client.FlushResult, error) {
	c.mu.Lock()
	c._batch = b
	c.mu.Unlock()
	return client.FlushResult{IngestResponse: client.IngestResponse{Accepted: len(b.Events)}}, nil
}

func (c *batchCapturingClient) last() *wireformat.IngestBatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c._batch
}

func TestBatcher_WithMaxBatchSizeZeroIgnored(t *testing.T) {
	// n <= 0 must leave the default max batch size unchanged.
	c := &fakeClient{}
	b := NewBatcher(c, testResource, testAgent, testr.New(t), WithMaxBatchSize(0))
	if b.maxBatchSize != DefaultMaxBatchSize {
		t.Errorf("maxBatchSize = %d after WithMaxBatchSize(0), want default %d",
			b.maxBatchSize, DefaultMaxBatchSize)
	}
}

func TestBatcher_WithMaxBatchSizeClampsToAbsoluteMax(t *testing.T) {
	// n > AbsoluteMaxBatchSize must be clamped.
	c := &fakeClient{}
	b := NewBatcher(c, testResource, testAgent, testr.New(t),
		WithMaxBatchSize(AbsoluteMaxBatchSize+1),
	)
	if b.maxBatchSize != AbsoluteMaxBatchSize {
		t.Errorf("maxBatchSize = %d, want AbsoluteMaxBatchSize %d",
			b.maxBatchSize, AbsoluteMaxBatchSize)
	}
}

func TestBatcher_WithFlushIntervalZeroIgnored(t *testing.T) {
	// d <= 0 must leave the default flush interval unchanged.
	c := &fakeClient{}
	b := NewBatcher(c, testResource, testAgent, testr.New(t), WithFlushInterval(0))
	if b.flushInterval != DefaultFlushInterval {
		t.Errorf("flushInterval = %v after WithFlushInterval(0), want default %v",
			b.flushInterval, DefaultFlushInterval)
	}
}

// waitFor polls the predicate until it returns true or the timeout fires.
func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("waitFor: predicate never became true")
}
