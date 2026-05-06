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
	"fmt"
	"runtime/debug"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/incidentary/operator/internal/batch"
	"github.com/incidentary/operator/internal/filter"
	"github.com/incidentary/operator/internal/mapper"
	opmetrics "github.com/incidentary/operator/internal/metrics"
)

// -----------------------------------------------------------------------------
// pipelineHandler — bridges informer events to the wire-format pipeline.
//
// The handler implements informers.Handler. For each cluster event it:
//  1. Asks the mapper to produce zero or more wire-format events.
//  2. Filters them through the configured severity policy (Tier 1 events
//     bypass the filter per Philosophy 1).
//  3. Enqueues survivors on the Batcher for shipping to ingest.
//
// emit() recovers from any panic in the mapper so a pathological cluster
// state can never crash the operator.
// -----------------------------------------------------------------------------

type pipelineHandler struct {
	mapper  mapper.Dispatcher
	filter  filter.Filter
	batcher *batch.Batcher
	log     logr.Logger
}

func (h *pipelineHandler) OnAdd(ctx context.Context, obj client.Object) {
	h.emit(ctx, nil, obj)
}

func (h *pipelineHandler) OnUpdate(ctx context.Context, oldObj, newObj client.Object) {
	h.emit(ctx, oldObj, newObj)
}

func (h *pipelineHandler) OnDelete(ctx context.Context, obj client.Object) {
	h.emit(ctx, obj, nil)
}

func (h *pipelineHandler) emit(ctx context.Context, oldObj, newObj client.Object) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error(fmt.Errorf("%v", r), "mapper dispatch panicked; dropping event",
				"stack", string(debug.Stack()))
		}
	}()
	events, err := h.mapper.Dispatch(ctx, oldObj, newObj)
	if err != nil {
		h.log.Error(err, "mapper dispatch failed")
		return
	}
	if len(events) == 0 {
		return
	}

	// Apply Philosophy 1 severity filter. Tier 1 events (crashes, OOMs,
	// failures, deploys) are pinned through regardless of the threshold.
	accepted := events[:0]
	for _, ev := range events {
		if h.filter.Accept(ev) {
			accepted = append(accepted, ev)
		} else {
			tier := "tier2"
			if filter.IsTier1(ev.Kind) {
				tier = "tier1"
			}
			opmetrics.EventsFilteredTotal.WithLabelValues(tier).Inc()
		}
	}
	if len(accepted) == 0 {
		return
	}
	h.batcher.Enqueue(accepted...)
}

// -----------------------------------------------------------------------------
// leaderMetricRunnable — sets the LeaderIsLeader gauge to 1 while this
// instance is the elected leader, and back to 0 on shutdown. Implements
// LeaderElectionRunnable so it only runs on the leader.
// -----------------------------------------------------------------------------

type leaderMetricRunnable struct{}

func (leaderMetricRunnable) Start(ctx context.Context) error {
	opmetrics.LeaderIsLeader.Set(1)
	<-ctx.Done()
	opmetrics.LeaderIsLeader.Set(0)
	return nil
}

func (leaderMetricRunnable) NeedLeaderElection() bool { return true }
