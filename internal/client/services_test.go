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

func TestServicesList_Success(t *testing.T) {
	want := []ServiceEntry{
		{ServiceID: "web", LastSeen: 1000, Instrumented: true},
		{ServiceID: "db", LastSeen: 2000, Instrumented: false},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ServicesResponse{Services: want})
	}))
	defer srv.Close()

	c := NewServicesClient("sk_test", WithServicesEndpoint(srv.URL))
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	if got[0].ServiceID != "web" {
		t.Errorf("first ServiceID = %q, want %q", got[0].ServiceID, "web")
	}
	if got[1].Instrumented != false {
		t.Error("second entry should not be instrumented")
	}
}

func TestServicesList_EmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ServicesResponse{Services: []ServiceEntry{}})
	}))
	defer srv.Close()

	c := NewServicesClient("sk", WithServicesEndpoint(srv.URL))
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(got))
	}
}

func TestServicesList_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()

	c := NewServicesClient("bad-key", WithServicesEndpoint(srv.URL))
	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("expected error on 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, should mention HTTP 403", err)
	}
}

func TestServicesList_LongBodyTruncated(t *testing.T) {
	longBody := strings.Repeat("e", 512)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(longBody))
	}))
	defer srv.Close()

	c := NewServicesClient("sk", WithServicesEndpoint(srv.URL))
	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if len(err.Error()) > 512 {
		t.Errorf("error unexpectedly long (%d chars), body should be truncated", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "...") {
		t.Errorf("truncated error should end with ellipsis: %v", err)
	}
}

func TestServicesList_RequestHeaders(t *testing.T) {
	var gotAuth, gotAccept, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotVersion = r.Header.Get(HeaderAgentVersion)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ServicesResponse{})
	}))
	defer srv.Close()

	c := NewServicesClient("bearer_xyz", WithServicesEndpoint(srv.URL))
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer bearer_xyz" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccept != contentTypeJSON {
		t.Errorf("Accept = %q", gotAccept)
	}
	if gotVersion != DefaultAgentVersion {
		t.Errorf("AgentVersion = %q, want %q", gotVersion, DefaultAgentVersion)
	}
}

func TestServicesList_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ServicesResponse{})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := NewServicesClient("sk", WithServicesEndpoint(srv.URL))
	_, err := c.List(ctx)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestServicesList_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	c := NewServicesClient("sk", WithServicesEndpoint(srv.URL))
	_, err := c.List(context.Background())
	if err == nil {
		t.Fatal("expected error on invalid JSON response")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error = %v, should mention parsing", err)
	}
}

func TestNewServicesClient_DefaultEndpoint(t *testing.T) {
	c := NewServicesClient("sk")
	if c.endpoint != DefaultServicesEndpoint {
		t.Errorf("endpoint = %q, want %q", c.endpoint, DefaultServicesEndpoint)
	}
}

func TestServicesList_WithHTTPClientOption(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ServicesResponse{Services: []ServiceEntry{{ServiceID: "svc-1"}}})
	}))
	defer srv.Close()

	custom := &http.Client{Transport: http.DefaultTransport}
	c := NewServicesClient("sk",
		WithServicesEndpoint(srv.URL),
		WithServicesHTTPClient(custom),
	)
	got, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ServiceID != "svc-1" {
		t.Errorf("unexpected result: %+v", got)
	}
}
