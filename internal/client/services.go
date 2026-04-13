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
	"fmt"
	"io"
	"net/http"
)

// DefaultServicesEndpoint is the Incidentary API path for the services list.
const DefaultServicesEndpoint = "https://api.incidentary.com/api/v2/workspace/services"

// ServiceEntry is one row of the services list response.
type ServiceEntry struct {
	ServiceID    string `json:"service_id"`
	LastSeen     int64  `json:"last_seen"`
	Instrumented bool   `json:"instrumented"`
}

// ServicesResponse is the envelope for GET /api/v2/workspace/services.
type ServicesResponse struct {
	Services []ServiceEntry `json:"services"`
}

// ServicesClient exposes the Incidentary services list to the reconciler.
type ServicesClient interface {
	List(ctx context.Context) ([]ServiceEntry, error)
}

// HTTPServicesClient is the HTTP implementation.
type HTTPServicesClient struct {
	endpoint   string
	apiKey     string
	version    string
	httpClient *http.Client
}

// NewServicesClient constructs a services client. Options override the defaults.
func NewServicesClient(apiKey string, opts ...ServicesOption) *HTTPServicesClient {
	c := &HTTPServicesClient{
		endpoint:   DefaultServicesEndpoint,
		apiKey:     apiKey,
		version:    DefaultAgentVersion,
		httpClient: &http.Client{Timeout: DefaultTimeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// ServicesOption configures an HTTPServicesClient.
type ServicesOption func(*HTTPServicesClient)

// WithServicesEndpoint overrides the endpoint URL.
func WithServicesEndpoint(url string) ServicesOption {
	return func(c *HTTPServicesClient) {
		if url != "" {
			c.endpoint = url
		}
	}
}

// WithServicesHTTPClient overrides the *http.Client used.
func WithServicesHTTPClient(hc *http.Client) ServicesOption {
	return func(c *HTTPServicesClient) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// List fetches the registered services for the authenticated workspace.
func (c *HTTPServicesClient) List(ctx context.Context) ([]ServiceEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("client: build services request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set(HeaderAgentVersion, c.version)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("client: get services: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 256 {
			snippet = snippet[:256] + "..."
		}
		return nil, fmt.Errorf("services HTTP %d: %s", resp.StatusCode, snippet)
	}
	var parsed ServicesResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("client: parse services response: %w", err)
	}
	return parsed.Services, nil
}
