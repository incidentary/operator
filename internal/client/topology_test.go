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
	"testing"
)

func testTopologyReport() *TopologyReport {
	return &TopologyReport{
		ClusterName: "test-cluster",
		ObservedAt:  1_000_000_000,
		Workloads: []TopologyWorkload{
			{
				ServiceID:         "web",
				Kind:              "Deployment",
				Namespace:         "prod",
				Replicas:          3,
				AvailableReplicas: 3,
			},
		},
	}
}

func TestTopologyReport_Success(t *testing.T) {
	want := TopologyResponse{Accepted: 1, CreatedGhostServices: 0, UpdatedServices: 1}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := NewTopologyClient("sk_test", WithTopologyEndpoint(srv.URL))
	got, err := c.Report(context.Background(), testTopologyReport())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Accepted != want.Accepted {
		t.Errorf("Accepted = %d, want %d", got.Accepted, want.Accepted)
	}
	if got.UpdatedServices != want.UpdatedServices {
		t.Errorf("UpdatedServices = %d, want %d", got.UpdatedServices, want.UpdatedServices)
	}
}

func TestTopologyReport_Accepted202(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(TopologyResponse{Accepted: 2})
	}))
	defer srv.Close()

	c := NewTopologyClient("sk", WithTopologyEndpoint(srv.URL))
	got, err := c.Report(context.Background(), testTopologyReport())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Accepted != 2 {
		t.Errorf("Accepted = %d, want 2", got.Accepted)
	}
}

func TestTopologyReport_NilReportIsError(t *testing.T) {
	c := NewTopologyClient("sk")
	_, err := c.Report(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil report")
	}
	if !strings.Contains(err.Error(), "nil topology report") {
		t.Errorf("error = %v, want mention of nil report", err)
	}
}

func TestTopologyReport_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	c := NewTopologyClient("bad-key", WithTopologyEndpoint(srv.URL))
	_, err := c.Report(context.Background(), testTopologyReport())
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, should mention HTTP 401", err)
	}
}

func TestTopologyReport_LongBodyTruncated(t *testing.T) {
	longBody := strings.Repeat("x", 512)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(longBody))
	}))
	defer srv.Close()

	c := NewTopologyClient("sk", WithTopologyEndpoint(srv.URL))
	_, err := c.Report(context.Background(), testTopologyReport())
	if err == nil {
		t.Fatal("expected error on 500")
	}
	// The snippet in the error message must be capped at 256+3 ("...").
	if len(err.Error()) > 512 {
		t.Errorf("error message unexpectedly long (%d chars), body should be truncated", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "...") {
		t.Errorf("truncated body should end with ellipsis, got: %v", err)
	}
}

func TestTopologyReport_RequestHeaders(t *testing.T) {
	var (
		gotAuth        string
		gotContentType string
		gotAccept      string
		gotVersion     string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotVersion = r.Header.Get(HeaderAgentVersion)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(TopologyResponse{})
	}))
	defer srv.Close()

	c := NewTopologyClient("tk_abc", WithTopologyEndpoint(srv.URL))
	if _, err := c.Report(context.Background(), testTopologyReport()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer tk_abc" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotContentType != contentTypeJSON {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotAccept != contentTypeJSON {
		t.Errorf("Accept = %q", gotAccept)
	}
	if gotVersion != DefaultAgentVersion {
		t.Errorf("AgentVersion = %q, want %q", gotVersion, DefaultAgentVersion)
	}
}

func TestTopologyReport_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(TopologyResponse{})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := NewTopologyClient("sk", WithTopologyEndpoint(srv.URL))
	_, err := c.Report(ctx, testTopologyReport())
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestTopologyReport_EmptyResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// no body
	}))
	defer srv.Close()

	c := NewTopologyClient("sk", WithTopologyEndpoint(srv.URL))
	got, err := c.Report(context.Background(), testTopologyReport())
	if err != nil {
		t.Fatalf("unexpected error on empty 200 body: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestNewTopologyClient_DefaultEndpoint(t *testing.T) {
	c := NewTopologyClient("sk")
	if c.endpoint != DefaultTopologyEndpoint {
		t.Errorf("endpoint = %q, want %q", c.endpoint, DefaultTopologyEndpoint)
	}
}

func TestTopologyReport_WithHTTPClientOption(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(TopologyResponse{Accepted: 5})
	}))
	defer srv.Close()

	custom := &http.Client{Transport: http.DefaultTransport}
	c := NewTopologyClient("sk",
		WithTopologyEndpoint(srv.URL),
		WithTopologyHTTPClient(custom),
	)
	got, err := c.Report(context.Background(), testTopologyReport())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Accepted != 5 {
		t.Errorf("Accepted = %d, want 5", got.Accepted)
	}
}
