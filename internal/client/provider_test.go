/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package client_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"

	"github.com/incidentary/operator/internal/client"
	"github.com/incidentary/operator/internal/wireformat"
)

// -----------------------------------------------------------------------------
// Test doubles — minimal implementations of each client interface that record
// the credentials they were built with so we can verify hot-rotation.
// -----------------------------------------------------------------------------

type recordingIngest struct {
	id string // sentinel identifying this instance
}

func (r *recordingIngest) Flush(_ context.Context, _ *wireformat.IngestBatch) (client.FlushResult, error) {
	return client.FlushResult{}, nil
}

type recordingTopology struct {
	id string
}

func (r *recordingTopology) Report(_ context.Context, _ *client.TopologyReport) (*client.TopologyResponse, error) {
	return &client.TopologyResponse{}, nil
}

type recordingServices struct {
	id string
}

func (r *recordingServices) List(_ context.Context) ([]client.ServiceEntry, error) {
	return nil, nil
}

// -----------------------------------------------------------------------------
// Constructor and nil-guard contract.
// -----------------------------------------------------------------------------

func TestProvider_PanicsOnNilIngest(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil ingest client")
		}
	}()
	_ = client.NewProvider(nil, &recordingTopology{}, &recordingServices{})
}

func TestProvider_PanicsOnNilTopology(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil topology client")
		}
	}()
	_ = client.NewProvider(&recordingIngest{}, nil, &recordingServices{})
}

func TestProvider_PanicsOnNilServices(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil services client")
		}
	}()
	_ = client.NewProvider(&recordingIngest{}, &recordingTopology{}, nil)
}

// -----------------------------------------------------------------------------
// Initial accessors return whatever was supplied to NewProvider.
// -----------------------------------------------------------------------------

func TestProvider_AccessorsReturnInitialClients(t *testing.T) {
	ing := &recordingIngest{id: "init-ingest"}
	topo := &recordingTopology{id: "init-topo"}
	svc := &recordingServices{id: "init-svc"}

	p := client.NewProvider(ing, topo, svc)

	if got, ok := p.Ingest().(*recordingIngest); !ok || got.id != "init-ingest" {
		t.Errorf("Ingest() = %+v, want recordingIngest{init-ingest}", p.Ingest())
	}
	if got, ok := p.Topology().(*recordingTopology); !ok || got.id != "init-topo" {
		t.Errorf("Topology() = %+v, want recordingTopology{init-topo}", p.Topology())
	}
	if got, ok := p.Services().(*recordingServices); !ok || got.id != "init-svc" {
		t.Errorf("Services() = %+v, want recordingServices{init-svc}", p.Services())
	}
}

// -----------------------------------------------------------------------------
// Rotate atomically swaps all three clients.
// -----------------------------------------------------------------------------

func TestProvider_RotateWithAPIKeyInstallsHTTPClients(t *testing.T) {
	p := client.NewProvider(&recordingIngest{id: "init"},
		&recordingTopology{id: "init"}, &recordingServices{id: "init"})

	p.Rotate("sk_live_test", "ws_test",
		"https://example.com/ingest",
		"https://example.com/topology",
		"https://example.com/services",
		testr.New(t))

	if _, ok := p.Ingest().(*client.HTTPClient); !ok {
		t.Errorf("Ingest() type = %T, want *client.HTTPClient after Rotate with key", p.Ingest())
	}
	if _, ok := p.Topology().(*client.HTTPTopologyClient); !ok {
		t.Errorf("Topology() type = %T, want *client.HTTPTopologyClient after Rotate with key", p.Topology())
	}
	if _, ok := p.Services().(*client.HTTPServicesClient); !ok {
		t.Errorf("Services() type = %T, want *client.HTTPServicesClient after Rotate with key", p.Services())
	}
}

func TestProvider_RotateWithEmptyKeyInstallsDroppingStubs(t *testing.T) {
	// Start with real-ish clients (recording stubs are fine — the test only
	// checks the type after Rotate, not before).
	p := client.NewProvider(&recordingIngest{}, &recordingTopology{}, &recordingServices{})

	p.Rotate("", "", "", "", "", testr.New(t))

	// Empty key must install dropping/empty stubs, not real HTTP clients.
	if _, ok := p.Ingest().(*client.HTTPClient); ok {
		t.Error("Ingest() should not be *HTTPClient when key is empty")
	}
	if _, ok := p.Topology().(*client.HTTPTopologyClient); ok {
		t.Error("Topology() should not be *HTTPTopologyClient when key is empty")
	}
	if _, ok := p.Services().(*client.HTTPServicesClient); ok {
		t.Error("Services() should not be *HTTPServicesClient when key is empty")
	}

	// Calling Flush on the dropping stub must not error and must report all
	// events as dropped (not accepted).
	res, err := p.Ingest().Flush(context.Background(), &wireformat.IngestBatch{
		Events: []wireformat.Event{{ID: "e1"}, {ID: "e2"}},
	})
	if err != nil {
		t.Errorf("dropping ingest should not error, got %v", err)
	}
	if res.Accepted != 0 {
		t.Errorf("dropping ingest should accept 0, got %d", res.Accepted)
	}
	if res.Dropped != 2 {
		t.Errorf("dropping ingest should drop all 2, got %d", res.Dropped)
	}
}

func TestProvider_RotateIsRepeatable(t *testing.T) {
	// Calling Rotate multiple times with different keys must always end in
	// the most recent state, with no stale references leaked.
	p := client.NewProvider(&recordingIngest{}, &recordingTopology{}, &recordingServices{})

	p.Rotate("k1", "ws1", "ep1", "tp1", "sv1", testr.New(t))
	first := p.Ingest()

	p.Rotate("k2", "ws2", "ep2", "tp2", "sv2", testr.New(t))
	second := p.Ingest()

	// Different rotations must yield distinct underlying clients.
	if first == second {
		t.Error("Rotate should replace clients, but Ingest() returned the same pointer twice")
	}

	// Both must be real HTTP clients (non-empty keys).
	if _, ok := second.(*client.HTTPClient); !ok {
		t.Errorf("after second Rotate, Ingest() type = %T, want *HTTPClient", second)
	}
}

func TestProvider_RotateBackToEmptyKeyReinstallsDroppers(t *testing.T) {
	p := client.NewProvider(&recordingIngest{}, &recordingTopology{}, &recordingServices{})

	// Start with a real key…
	p.Rotate("k1", "ws1", "ep1", "tp1", "sv1", testr.New(t))
	if _, ok := p.Ingest().(*client.HTTPClient); !ok {
		t.Fatalf("setup failed: Ingest() should be HTTPClient")
	}

	// …then rotate to empty (e.g., the Secret was deleted).
	p.Rotate("", "", "", "", "", testr.New(t))
	if _, ok := p.Ingest().(*client.HTTPClient); ok {
		t.Error("Ingest() should fall back to dropping stub when key is rotated to empty")
	}
}

// -----------------------------------------------------------------------------
// Concurrency contract: many goroutines reading while one rotates must never
// race. Run this with `go test -race` to catch the data-race scenario.
// -----------------------------------------------------------------------------

func TestProvider_ConcurrentReadsDuringRotate(t *testing.T) {
	p := client.NewProvider(&recordingIngest{id: "init"},
		&recordingTopology{id: "init"}, &recordingServices{id: "init"})

	// 50 reader goroutines, 1 rotator goroutine, 100 rotations.
	const readers = 50
	const rotations = 100

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var observedNonNil atomic.Int64

	for range readers {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					if p.Ingest() != nil {
						observedNonNil.Add(1)
					}
					if p.Topology() != nil {
						observedNonNil.Add(1)
					}
					if p.Services() != nil {
						observedNonNil.Add(1)
					}
				}
			}
		})
	}

	log := logr.Discard()
	for i := range rotations {
		// Alternate between empty (dropping stubs) and a real key, exercising
		// both branches inside Rotate while readers are active.
		if i%2 == 0 {
			p.Rotate("", "", "", "", "", log)
		} else {
			p.Rotate("k", "ws", "ep", "tp", "sv", log)
		}
		// Yield occasionally so the readers actually get scheduled.
		if i%10 == 0 {
			time.Sleep(time.Microsecond)
		}
	}

	close(stop)
	wg.Wait()

	// Sanity: readers must have observed many non-nil clients. The exact
	// count varies; we only care that the loop did real work.
	if observedNonNil.Load() < int64(readers) {
		t.Errorf("readers observed only %d non-nil clients; expected at least %d",
			observedNonNil.Load(), readers)
	}
}
