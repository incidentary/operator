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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/incidentary/operator/internal/wireformat"
)

const contentTypeJSON = "application/json"

func testBatch() *wireformat.IngestBatch {
	return &wireformat.IngestBatch{
		SpecVersion: wireformat.SpecVersion,
		Resource:    wireformat.Resource{Attributes: map[string]string{"k8s.cluster.name": "test"}},
		Agent: wireformat.Agent{
			Type:        wireformat.AgentTypeK8sOperator,
			Version:     "test",
			WorkspaceID: "ws-1",
		},
		CaptureMode: wireformat.CaptureModeSkeleton,
		Events: []wireformat.Event{
			{ID: "id-1", Kind: wireformat.KindK8sOOMKill, Severity: wireformat.SeverityError, OccurredAt: 1, ServiceID: "web"},
		},
	}
}

func TestFlush_Success(t *testing.T) {
	var gotAuth, gotVersion, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get(HeaderAgentVersion)
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set(HeaderCaptureModeRequested, "FULL")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(IngestResponse{Accepted: 1, Dropped: 0})
	}))
	defer srv.Close()

	c := NewHTTPClient("sk_test_123",
		WithEndpoint(srv.URL),
		WithAgentVersion("1.2.3"),
	)
	res, err := c.Flush(context.Background(), testBatch())
	if err != nil {
		t.Fatalf("flush err: %v", err)
	}
	if res.Accepted != 1 {
		t.Errorf("accepted = %d, want 1", res.Accepted)
	}
	if res.CaptureModeRequested != "FULL" {
		t.Errorf("capture_mode_requested = %q, want FULL", res.CaptureModeRequested)
	}
	if gotAuth != "Bearer sk_test_123" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotVersion != "1.2.3" {
		t.Errorf("agent version header = %q", gotVersion)
	}
	if gotContentType != contentTypeJSON {
		t.Errorf("content-type = %q", gotContentType)
	}
}

func TestFlush_RetriesOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(IngestResponse{Accepted: 1})
	}))
	defer srv.Close()

	c := NewHTTPClient("sk",
		WithEndpoint(srv.URL),
		WithRetryBackoff(10*time.Millisecond, 40*time.Millisecond, 2*time.Second),
	)
	res, err := c.Flush(context.Background(), testBatch())
	if err != nil {
		t.Fatalf("flush err: %v", err)
	}
	if res.Accepted != 1 {
		t.Errorf("accepted = %d", res.Accepted)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestFlush_NoRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid batch"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient("sk",
		WithEndpoint(srv.URL),
		WithRetryBackoff(time.Millisecond, 5*time.Millisecond, 100*time.Millisecond),
	)
	_, err := c.Flush(context.Background(), testBatch())
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("error = %v, should mention HTTP 400", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retries on 4xx)", got)
	}
}

func TestFlush_Retries429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(IngestResponse{Accepted: 1})
	}))
	defer srv.Close()

	c := NewHTTPClient("sk",
		WithEndpoint(srv.URL),
		WithRetryBackoff(5*time.Millisecond, 20*time.Millisecond, 500*time.Millisecond),
	)
	_, err := c.Flush(context.Background(), testBatch())
	if err != nil {
		t.Fatalf("flush err: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

func TestFlush_NilBatchIsError(t *testing.T) {
	c := NewHTTPClient("sk")
	_, err := c.Flush(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error on nil batch")
	}
}

func TestFlush_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	c := NewHTTPClient("sk",
		WithEndpoint(srv.URL),
		WithRetryBackoff(10*time.Millisecond, 20*time.Millisecond, time.Second),
	)
	_, err := c.Flush(ctx, testBatch())
	if err == nil {
		t.Fatal("expected error from cancelled ctx")
	}
}

func TestWithHTTPClient_OverridesTransport(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(IngestResponse{Accepted: 1})
	}))
	defer srv.Close()

	custom := &http.Client{}
	c := NewHTTPClient("sk", WithEndpoint(srv.URL), WithHTTPClient(custom))
	if _, err := c.Flush(context.Background(), testBatch()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("expected custom http client to reach the server")
	}
}

func TestWithMaxRetries_LimitsAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := NewHTTPClient("sk",
		WithEndpoint(srv.URL),
		WithMaxRetries(1),
		WithRetryBackoff(5*time.Millisecond, 10*time.Millisecond, 5*time.Second),
	)
	if _, err := c.Flush(context.Background(), testBatch()); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls = %d, want 2 (maxRetries=1 → 2 total attempts)", got)
	}
}

// TestFlush_DeadlineExpiresBreaksRetryLoop covers the deadline-exceeded branch
// inside the retry loop that fires before sleeping when the window is exhausted.
func TestFlush_DeadlineExpiresBreaksRetryLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// Window of 1ns expires before the first sleep (50ms) fires.
	c := NewHTTPClient("sk",
		WithEndpoint(srv.URL),
		WithMaxRetries(5),
		WithRetryBackoff(50*time.Millisecond, 100*time.Millisecond, 1*time.Nanosecond),
	)
	if _, err := c.Flush(context.Background(), testBatch()); err == nil {
		t.Fatal("expected error when retry window expires")
	}
}

// TestFlush_ExhaustsAllRetries covers the post-loop return and the backoff cap.
func TestFlush_ExhaustsAllRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// initialBackoff=5ms, maxBackoff=6ms: after the first retry backoff doubles
	// to 10ms > 6ms, triggering the backoff-cap branch.
	c := NewHTTPClient("sk",
		WithEndpoint(srv.URL),
		WithMaxRetries(2),
		WithRetryBackoff(5*time.Millisecond, 6*time.Millisecond, 5*time.Second),
	)
	if _, err := c.Flush(context.Background(), testBatch()); err == nil {
		t.Fatal("expected error after exhausting all retries")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3 (maxRetries=2 → 3 total attempts)", got)
	}
}
