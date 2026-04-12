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

// Package batch buffers Incidentary wire-format events and periodically
// flushes them to the v2 ingest endpoint.
//
// The batcher is safe for concurrent enqueue from informer goroutines. It
// flushes when the buffer reaches MaxBatchSize OR when FlushInterval has
// elapsed since the last flush — whichever happens first. On graceful
// shutdown, all buffered events are flushed before Start returns.
package batch

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/incidentary/operator/internal/client"
	"github.com/incidentary/operator/internal/metrics"
	"github.com/incidentary/operator/internal/wireformat"
)

// Defaults tuned for low steady-state traffic with fast flush on spikes.
const (
	DefaultFlushInterval = 10 * time.Second
	DefaultMaxBatchSize  = 1000
	AbsoluteMaxBatchSize = 10000 // v2 spec ceiling
)

// ResourceProvider returns the Resource block that prefixes every batch.
// The provider is called on every flush so operator metadata (cluster name,
// namespace) can change over time — in practice these are immutable after
// startup, but keeping this as a closure keeps the batcher decoupled from
// environment lookups.
type ResourceProvider func() wireformat.Resource

// AgentProvider returns the Agent block that prefixes every batch.
type AgentProvider func() wireformat.Agent

// Batcher buffers wire format events and flushes them to the ingest client.
// A Batcher is a controller-runtime manager.Runnable: register it with
// mgr.Add, and Start will block until ctx is done.
type Batcher struct {
	client   client.IngestClient
	log      logr.Logger
	resource ResourceProvider
	agent    AgentProvider

	flushInterval time.Duration
	maxBatchSize  int

	mu     sync.Mutex
	buffer []wireformat.Event

	// captureModeReq holds the most recently observed value of the
	// X-Capture-Mode-Requested response header. Phase 7 will consume this.
	captureMu      sync.RWMutex
	captureModeReq string

	// flushCh kicks the flush goroutine when the buffer reaches max size
	// ahead of the ticker.
	flushCh chan struct{}
}

// Option configures a Batcher via the functional options pattern.
type Option func(*Batcher)

// WithFlushInterval overrides the default ticker period.
func WithFlushInterval(d time.Duration) Option {
	return func(b *Batcher) {
		if d > 0 {
			b.flushInterval = d
		}
	}
}

// WithMaxBatchSize overrides the eager flush threshold. Values above
// AbsoluteMaxBatchSize are clamped.
func WithMaxBatchSize(n int) Option {
	return func(b *Batcher) {
		if n <= 0 {
			return
		}
		if n > AbsoluteMaxBatchSize {
			n = AbsoluteMaxBatchSize
		}
		b.maxBatchSize = n
	}
}

// NewBatcher constructs a Batcher. resource and agent producers are called
// at flush time to build the outgoing IngestBatch envelope.
func NewBatcher(c client.IngestClient, resource ResourceProvider, agent AgentProvider, log logr.Logger, opts ...Option) *Batcher {
	if c == nil {
		panic("batch.NewBatcher: client must not be nil")
	}
	if resource == nil {
		panic("batch.NewBatcher: resource provider must not be nil")
	}
	if agent == nil {
		panic("batch.NewBatcher: agent provider must not be nil")
	}
	b := &Batcher{
		client:        c,
		log:           log,
		resource:      resource,
		agent:         agent,
		flushInterval: DefaultFlushInterval,
		maxBatchSize:  DefaultMaxBatchSize,
		flushCh:       make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Compile-time assertion: *Batcher implements Runnable + LeaderElectionRunnable.
var _ interface {
	Start(context.Context) error
	NeedLeaderElection() bool
} = (*Batcher)(nil)

// NeedLeaderElection reports that the Batcher must run on the elected leader
// only — duplicate event uploads are unacceptable.
func (b *Batcher) NeedLeaderElection() bool { return true }

// Enqueue adds events to the buffer. Thread-safe; called from informer
// goroutines. If the buffer reaches MaxBatchSize after the add, a flush is
// signalled eagerly so the next tick does not wait out the full interval.
func (b *Batcher) Enqueue(events ...wireformat.Event) {
	if len(events) == 0 {
		return
	}
	b.mu.Lock()
	b.buffer = append(b.buffer, events...)
	size := len(b.buffer)
	b.mu.Unlock()

	if size >= b.maxBatchSize {
		select {
		case b.flushCh <- struct{}{}:
		default:
			// A flush is already pending; no need to double-signal.
		}
	}
}

// CaptureModeRequested returns the most recent value of the ingest response
// header X-Capture-Mode-Requested (empty when never set). Phase 7 wires this
// into the SDK coordination path.
func (b *Batcher) CaptureModeRequested() string {
	b.captureMu.RLock()
	defer b.captureMu.RUnlock()
	return b.captureModeReq
}

// Start implements manager.Runnable. It runs the flush loop until ctx is
// cancelled, then drains the buffer with one final flush.
func (b *Batcher) Start(ctx context.Context) error {
	ticker := time.NewTicker(b.flushInterval)
	defer ticker.Stop()

	b.log.Info("batcher started",
		"flushInterval", b.flushInterval,
		"maxBatchSize", b.maxBatchSize,
	)

	for {
		select {
		case <-ctx.Done():
			// Final drain using a fresh bounded context to avoid losing
			// in-flight events to the outer cancellation.
			drainCtx, cancel := context.WithTimeout(context.Background(), b.flushInterval)
			if err := b.flushNow(drainCtx); err != nil {
				b.log.Error(err, "final drain failed")
			}
			cancel()
			b.log.Info("batcher stopped")
			return nil
		case <-ticker.C:
			if err := b.flushNow(ctx); err != nil {
				b.log.Error(err, "scheduled flush failed")
			}
		case <-b.flushCh:
			if err := b.flushNow(ctx); err != nil {
				b.log.Error(err, "eager flush failed")
			}
		}
	}
}

// flushNow atomically removes the buffered events and sends them as a single
// batch. If the buffer is empty, nothing is sent.
func (b *Batcher) flushNow(ctx context.Context) error {
	b.mu.Lock()
	if len(b.buffer) == 0 {
		b.mu.Unlock()
		return nil
	}
	events := b.buffer
	b.buffer = make([]wireformat.Event, 0, b.maxBatchSize)
	b.mu.Unlock()

	batch := &wireformat.IngestBatch{
		SpecVersion: wireformat.SpecVersion,
		Resource:    b.resource(),
		Agent:       b.agent(),
		CaptureMode: wireformat.CaptureModeSkeleton,
		Events:      events,
	}

	metrics.FlushBatchSize.Observe(float64(len(events)))

	start := time.Now()
	res, err := b.client.Flush(ctx, batch)
	metrics.FlushLatencySeconds.Observe(time.Since(start).Seconds())

	if err != nil {
		return err
	}

	// Record per-kind accepted events.
	kindCounts := make(map[string]int, len(events))
	for i := range events {
		kindCounts[string(events[i].Kind)]++
	}
	for kind, count := range kindCounts {
		metrics.EventsSentTotal.WithLabelValues(kind).Add(float64(count))
	}

	// Record per-reason dropped events.
	for reason, count := range res.DropReasons {
		metrics.EventsDroppedTotal.WithLabelValues(reason).Add(float64(count))
	}

	if res.CaptureModeRequested != "" {
		b.captureMu.Lock()
		b.captureModeReq = res.CaptureModeRequested
		b.captureMu.Unlock()
	}
	if res.Dropped > 0 {
		b.log.Info("ingest dropped events",
			"dropped", res.Dropped,
			"accepted", res.Accepted,
			"dropReasons", res.DropReasons,
		)
	} else {
		b.log.V(1).Info("ingest flush ok",
			"accepted", res.Accepted,
			"batchSize", len(events),
		)
	}
	return nil
}

// BufferSize returns the current buffer size. Exposed for metrics and tests.
func (b *Batcher) BufferSize() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buffer)
}
