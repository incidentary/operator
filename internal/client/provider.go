/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package client

import (
	"context"
	"sync"

	"github.com/go-logr/logr"

	"github.com/incidentary/operator/internal/wireformat"
)

// Provider holds the active outbound clients (ingest, topology, services)
// and allows atomic hot-swap when the API key changes. All accessors are
// safe for concurrent use by the batcher goroutine, the discovery loop, and
// the services-list reconciler.
//
// The pattern is read-copy-update:
//   - Reads (Ingest/Topology/Services) take an RLock and return the current
//     pointer. They never block each other, only Rotate.
//   - Rotate takes the write lock, builds three new clients, and atomically
//     swaps the pointers. In-flight requests using the previous clients
//     continue to completion with their old credentials, which is safe
//     because each request was authorized at the moment it started.
type Provider struct {
	mu          sync.RWMutex
	ingest      IngestClient
	topo        TopologyClient
	svc         ServicesClient
	workspaceID string
}

// NewProvider builds a Provider from initial clients. All three must be
// non-nil; the typical wiring at startup is to construct dropping stubs
// when no API key is yet available, then call Rotate() once the
// IncidentaryConfig CR has been reconciled.
func NewProvider(ingest IngestClient, topo TopologyClient, svc ServicesClient) *Provider {
	if ingest == nil {
		panic("client.NewProvider: ingest must not be nil")
	}
	if topo == nil {
		panic("client.NewProvider: topology must not be nil")
	}
	if svc == nil {
		panic("client.NewProvider: services must not be nil")
	}
	return &Provider{ingest: ingest, topo: topo, svc: svc}
}

// Rotate atomically replaces all three clients with fresh ones built from
// the supplied credentials. Pass an empty apiKey to install dropping/empty
// stubs (operator continues running but produces no telemetry).
//
// Endpoint URLs are passed through to the HTTP client constructors; an
// empty string falls back to the package defaults.
//
// workspaceID is stored on the Provider so callers (e.g., the batcher's
// agent producer) can read the latest value without needing a separate
// shared-state mechanism.
func (p *Provider) Rotate(apiKey, workspaceID string, ingestEP, topoEP, svcEP string, log logr.Logger) {
	var (
		newIngest IngestClient
		newTopo   TopologyClient
		newSvc    ServicesClient
	)
	if apiKey == "" {
		newIngest = &droppingIngestClient{log: log}
		newTopo = &droppingTopologyClient{log: log}
		newSvc = emptyServicesClient{}
	} else {
		newIngest = NewHTTPClient(apiKey, WithEndpoint(ingestEP))
		newTopo = NewTopologyClient(apiKey, WithTopologyEndpoint(topoEP))
		newSvc = NewServicesClient(apiKey, WithServicesEndpoint(svcEP))
	}

	p.mu.Lock()
	p.ingest, p.topo, p.svc = newIngest, newTopo, newSvc
	p.workspaceID = workspaceID
	p.mu.Unlock()
}

// WorkspaceID returns the active workspace ID. Updated by every Rotate call;
// callers that produce wire-format batches should call this on every flush
// to ensure the latest value is used.
func (p *Provider) WorkspaceID() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.workspaceID
}

// Ingest returns the active ingest client.
func (p *Provider) Ingest() IngestClient {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ingest
}

// Topology returns the active topology client.
func (p *Provider) Topology() TopologyClient {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.topo
}

// Services returns the active services-list client.
func (p *Provider) Services() ServicesClient {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.svc
}

// -----------------------------------------------------------------------------
// Dropping stubs — installed when no API key is configured. They satisfy
// each interface, never error, and report all submitted work as dropped so
// metrics correctly reflect the disabled-ingest state.
// -----------------------------------------------------------------------------

// droppingIngestClient discards every batch; used when the operator runs
// without an API key (read-only watch mode).
type droppingIngestClient struct {
	log logr.Logger
}

func (d *droppingIngestClient) Flush(_ context.Context, b *wireformat.IngestBatch) (FlushResult, error) {
	d.log.V(1).Info("ingest disabled; dropping batch", "events", len(b.Events))
	return FlushResult{
		IngestResponse: IngestResponse{Accepted: 0, Dropped: len(b.Events)},
	}, nil
}

// droppingTopologyClient discards every report; used when the operator runs
// without an API key.
type droppingTopologyClient struct {
	log logr.Logger
}

func (d *droppingTopologyClient) Report(_ context.Context, report *TopologyReport) (*TopologyResponse, error) {
	d.log.V(1).Info("topology disabled; dropping report", "workloads", len(report.Workloads))
	return &TopologyResponse{Accepted: 0}, nil
}

// emptyServicesClient returns an empty list and never errors; used when no
// API key is configured (the reconciler stays in dormant state).
type emptyServicesClient struct{}

func (emptyServicesClient) List(_ context.Context) ([]ServiceEntry, error) {
	return nil, nil
}
